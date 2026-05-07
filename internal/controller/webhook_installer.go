// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sync"

	registrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
)

const webhookName = "dependency-controller"

// WebhookInstaller creates and updates a single ValidatingWebhookConfiguration
// per provider workspace so that kcp's admission plugin dispatches DELETE
// requests to the dependency-controller's webhook server.
//
// Multiple DependencyRules may reference different resources from the same
// provider (e.g., VPCs and FirewallRules both from the network provider).
// The installer merges all protected group/version/resource tuples into the
// Rules list of one webhook entry per provider workspace.
//
// When a DependencyRule is deleted, its contributions are removed. If no rules
// remain for a workspace the webhook is deleted entirely.
type WebhookInstaller struct {
	// Manager is the multicluster manager for the dep-ctrl APIExport. Used to
	// obtain per-workspace clients via the virtual workspace, which scopes
	// access to workspaces that have bound the APIExport and accepted the
	// validatingwebhookconfigurations permissionClaim.
	Manager    mcmanager.Manager
	WebhookURL string
	CABundle   []byte

	mu sync.Mutex
	// ruleTargets tracks which workspace/resource tuples each DependencyRule
	// contributes, keyed by rule name.
	ruleTargets map[string][]ruleTarget
}

type ruleTarget struct {
	Workspace string
	Key       webhookRuleKey
}

type webhookRuleKey struct {
	Group    string
	Version  string
	Resource string
}

func NewWebhookInstaller(mgr mcmanager.Manager, webhookURL string, caBundle []byte) *WebhookInstaller {
	return &WebhookInstaller{
		Manager:     mgr,
		WebhookURL:  webhookURL,
		CABundle:    caBundle,
		ruleTargets: make(map[string][]ruleTarget),
	}
}

// EnsureWebhooks installs or updates ValidatingWebhookConfigurations for all
// dependency targets in the given rule. The clusterNames map translates
// workspace paths (from APIExportRef.Path) to logical cluster names used
// to connect via the virtual workspace.
func (w *WebhookInstaller) EnsureWebhooks(ctx context.Context, ruleKey string, rule *v1alpha1.DependencyRule, clusterNames map[string]string) error {
	// Group targets by logical cluster name so we do one update per workspace.
	byCluster := make(map[string][]v1alpha1.DependencyTarget)
	for _, dep := range rule.Spec.Dependencies {
		clusterName := clusterNames[dep.APIExportRef.Path]
		byCluster[clusterName] = append(byCluster[clusterName], dep)
	}

	for clusterName, deps := range byCluster {
		if err := w.ensureWebhookForWorkspace(ctx, ruleKey, clusterName, deps); err != nil {
			return err
		}
	}

	return nil
}

// RemoveWebhooks removes all webhook rules contributed by the given DependencyRule.
// If a workspace has no remaining rules, the webhook is deleted entirely.
func (w *WebhookInstaller) RemoveWebhooks(ctx context.Context, ruleName string) error {
	w.mu.Lock()
	targets, exists := w.ruleTargets[ruleName]
	if !exists {
		w.mu.Unlock()
		return nil
	}
	delete(w.ruleTargets, ruleName)

	// Collect affected workspaces.
	affectedWorkspaces := make(map[string]struct{})
	for _, t := range targets {
		affectedWorkspaces[t.Workspace] = struct{}{}
	}
	w.mu.Unlock()

	for wsPath := range affectedWorkspaces {
		if err := w.reconcileWorkspaceWebhook(ctx, wsPath); err != nil {
			return err
		}
	}

	return nil
}

func (w *WebhookInstaller) ensureWebhookForWorkspace(ctx context.Context, ruleName, wsPath string, deps []v1alpha1.DependencyTarget) error {
	w.mu.Lock()

	// Check if all targets are already tracked for this rule.
	existing := w.ruleTargets[ruleName]
	existingSet := make(map[ruleTarget]struct{}, len(existing))
	for _, t := range existing {
		existingSet[t] = struct{}{}
	}

	var newTargets []ruleTarget
	for _, dep := range deps {
		t := ruleTarget{
			Workspace: wsPath,
			Key:       webhookRuleKey{Group: dep.Group, Version: dep.Version, Resource: dep.Resource},
		}
		if _, ok := existingSet[t]; !ok {
			newTargets = append(newTargets, t)
		}
	}
	if len(newTargets) == 0 {
		w.mu.Unlock()
		return nil
	}

	// Record the new targets.
	w.ruleTargets[ruleName] = append(w.ruleTargets[ruleName], newTargets...)
	w.mu.Unlock()

	return w.reconcileWorkspaceWebhook(ctx, wsPath)
}

// reconcileWorkspaceWebhook computes the desired webhook rules for a workspace
// from all tracked DependencyRules and creates, updates, or deletes the webhook.
func (w *WebhookInstaller) reconcileWorkspaceWebhook(ctx context.Context, wsPath string) error {
	logger := log.FromContext(ctx).WithValues("workspace", wsPath)

	// Compute desired rules for this workspace across all DependencyRules.
	desired := w.desiredRulesForWorkspace(wsPath)

	cl, err := w.Manager.GetCluster(ctx, multicluster.ClusterName(wsPath))
	if err != nil {
		return fmt.Errorf("getting cluster %s from manager: %w", wsPath, err)
	}
	c := cl.GetClient()

	whCfg := &registrationv1.ValidatingWebhookConfiguration{}
	err = c.Get(ctx, types.NamespacedName{Name: webhookName}, whCfg)
	webhookExists := err == nil

	if apierrors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("getting webhook in %s: %w", wsPath, err)
	}

	if len(desired) == 0 {
		// No rules left — delete the webhook if it exists.
		if webhookExists {
			logger.Info("deleting webhook, no rules remaining")
			if err := c.Delete(ctx, whCfg); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting webhook in %s: %w", wsPath, err)
			}
		}

		return nil
	}

	rules := w.buildRuleList(desired)

	if !webhookExists {
		// Create new webhook.
		whCfg = w.buildWebhookConfig(rules)
		logger.Info("installing webhook", "rules", len(rules))
		if err := c.Create(ctx, whCfg); err != nil {
			return fmt.Errorf("creating webhook in %s: %w", wsPath, err)
		}

		return nil
	}

	// Update existing webhook with the full desired rule set.
	if len(whCfg.Webhooks) > 0 {
		whCfg.Webhooks[0].Rules = rules
	}
	logger.Info("updating webhook", "rules", len(rules))
	if err := c.Update(ctx, whCfg); err != nil {
		return fmt.Errorf("updating webhook in %s: %w", wsPath, err)
	}

	return nil
}

// desiredRulesForWorkspace returns the deduplicated set of resource keys that
// should be protected in the given workspace, computed from all tracked rules.
func (w *WebhookInstaller) desiredRulesForWorkspace(wsPath string) map[webhookRuleKey]struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()

	desired := make(map[webhookRuleKey]struct{})
	for _, targets := range w.ruleTargets {
		for _, t := range targets {
			if t.Workspace == wsPath {
				desired[t.Key] = struct{}{}
			}
		}
	}

	return desired
}

func (w *WebhookInstaller) buildRuleList(desired map[webhookRuleKey]struct{}) []registrationv1.RuleWithOperations {
	rules := make([]registrationv1.RuleWithOperations, 0, len(desired))
	for key := range desired {
		rules = append(rules, registrationv1.RuleWithOperations{
			Operations: []registrationv1.OperationType{registrationv1.Delete},
			Rule: registrationv1.Rule{
				APIGroups:   []string{key.Group},
				APIVersions: []string{key.Version},
				Resources:   []string{key.Resource},
			},
		})
	}

	return rules
}

// buildWebhookConfig creates a new ValidatingWebhookConfiguration with the given rules.
func (w *WebhookInstaller) buildWebhookConfig(rules []registrationv1.RuleWithOperations) *registrationv1.ValidatingWebhookConfiguration {
	failPolicy := registrationv1.Fail
	sideEffects := registrationv1.SideEffectClassNone
	webhookURL := w.WebhookURL

	return &registrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: webhookName,
		},
		Webhooks: []registrationv1.ValidatingWebhook{{
			Name:                    fmt.Sprintf("%s.dependencies.opendefense.cloud", webhookName),
			AdmissionReviewVersions: []string{"v1"},
			ClientConfig: registrationv1.WebhookClientConfig{
				URL:      &webhookURL,
				CABundle: w.CABundle,
			},
			FailurePolicy: &failPolicy,
			SideEffects:   &sideEffects,
			Rules:         rules,
		}},
	}
}
