package controller

import (
	"context"
	"fmt"
	"sync"

	registrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kcp-dev/logicalcluster/v3"

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
type WebhookInstaller struct {
	BaseConfig *rest.Config
	WebhookURL string
	CABundle   []byte

	mu sync.Mutex
	// rules tracks which group/version/resource tuples have been added to
	// the webhook in each workspace, keyed by workspace path.
	rules map[string]map[ruleKey]struct{}
}

type ruleKey struct {
	Group    string
	Version  string
	Resource string
}

func NewWebhookInstaller(baseCfg *rest.Config, webhookURL string, caBundle []byte) *WebhookInstaller {
	return &WebhookInstaller{
		BaseConfig: baseCfg,
		WebhookURL: webhookURL,
		CABundle:   caBundle,
		rules:      make(map[string]map[ruleKey]struct{}),
	}
}

// EnsureWebhooks installs or updates ValidatingWebhookConfigurations for all
// dependency targets in the given rule.
func (w *WebhookInstaller) EnsureWebhooks(ctx context.Context, rule *v1alpha1.DependencyRule) error {
	// Group targets by provider workspace so we do one update per workspace.
	byWorkspace := make(map[string][]v1alpha1.DependencyTarget)
	for _, dep := range rule.Spec.Dependencies {
		wsPath := dep.APIExportRef.Path
		byWorkspace[wsPath] = append(byWorkspace[wsPath], dep)
	}

	for wsPath, deps := range byWorkspace {
		if err := w.ensureWebhookForWorkspace(ctx, wsPath, deps); err != nil {
			return err
		}
	}
	return nil
}

func (w *WebhookInstaller) ensureWebhookForWorkspace(ctx context.Context, wsPath string, deps []v1alpha1.DependencyTarget) error {
	w.mu.Lock()
	existing := w.rules[wsPath]
	if existing == nil {
		existing = make(map[ruleKey]struct{})
		w.rules[wsPath] = existing
	}

	// Check if all targets are already covered.
	var newKeys []ruleKey
	for _, dep := range deps {
		key := ruleKey{Group: dep.Group, Version: dep.Version, Resource: dep.Resource}
		if _, ok := existing[key]; !ok {
			newKeys = append(newKeys, key)
		}
	}
	if len(newKeys) == 0 {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()

	logger := log.FromContext(ctx).WithValues("workspace", wsPath)

	cfg := rest.CopyConfig(w.BaseConfig)
	cfg.Host += logicalcluster.NewPath(wsPath).RequestPath()

	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("creating client for %s: %w", wsPath, err)
	}

	// Fetch the existing webhook configuration if any.
	whCfg := &registrationv1.ValidatingWebhookConfiguration{}
	err = c.Get(ctx, types.NamespacedName{Name: webhookName}, whCfg)
	if apierrors.IsNotFound(err) {
		// Create a new webhook with all desired rules.
		whCfg = w.buildWebhookConfig(wsPath, deps)
		logger.Info("installing webhook", "rules", len(whCfg.Webhooks[0].Rules))
		if err := c.Create(ctx, whCfg); err != nil {
			return fmt.Errorf("creating webhook in %s: %w", wsPath, err)
		}
	} else if err != nil {
		return fmt.Errorf("getting webhook in %s: %w", wsPath, err)
	} else {
		// Merge new rules into the existing webhook.
		if w.mergeRules(whCfg, deps) {
			logger.Info("updating webhook with new rules", "rules", len(whCfg.Webhooks[0].Rules))
			if err := c.Update(ctx, whCfg); err != nil {
				return fmt.Errorf("updating webhook in %s: %w", wsPath, err)
			}
		}
	}

	// Mark all targets as installed.
	w.mu.Lock()
	for _, dep := range deps {
		w.rules[wsPath][ruleKey{Group: dep.Group, Version: dep.Version, Resource: dep.Resource}] = struct{}{}
	}
	w.mu.Unlock()
	return nil
}

// buildWebhookConfig creates a new ValidatingWebhookConfiguration with rules
// for all the given dependency targets.
func (w *WebhookInstaller) buildWebhookConfig(wsPath string, deps []v1alpha1.DependencyTarget) *registrationv1.ValidatingWebhookConfiguration {
	failPolicy := registrationv1.Fail
	sideEffects := registrationv1.SideEffectClassNone
	webhookURL := w.WebhookURL

	var rules []registrationv1.RuleWithOperations
	for _, dep := range deps {
		rules = append(rules, registrationv1.RuleWithOperations{
			Operations: []registrationv1.OperationType{registrationv1.Delete},
			Rule: registrationv1.Rule{
				APIGroups:   []string{dep.Group},
				APIVersions: []string{dep.Version},
				Resources:   []string{dep.Resource},
			},
		})
	}

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

// mergeRules appends rules for any dependency targets not already present in
// the webhook configuration. Returns true if any rules were added.
func (w *WebhookInstaller) mergeRules(whCfg *registrationv1.ValidatingWebhookConfiguration, deps []v1alpha1.DependencyTarget) bool {
	if len(whCfg.Webhooks) == 0 {
		return false
	}

	existing := make(map[ruleKey]struct{})
	for _, rule := range whCfg.Webhooks[0].Rules {
		for _, group := range rule.APIGroups {
			for _, version := range rule.APIVersions {
				for _, resource := range rule.Resources {
					existing[ruleKey{Group: group, Version: version, Resource: resource}] = struct{}{}
				}
			}
		}
	}

	changed := false
	for _, dep := range deps {
		key := ruleKey{Group: dep.Group, Version: dep.Version, Resource: dep.Resource}
		if _, ok := existing[key]; ok {
			continue
		}
		whCfg.Webhooks[0].Rules = append(whCfg.Webhooks[0].Rules, registrationv1.RuleWithOperations{
			Operations: []registrationv1.OperationType{registrationv1.Delete},
			Rule: registrationv1.Rule{
				APIGroups:   []string{dep.Group},
				APIVersions: []string{dep.Version},
				Resources:   []string{dep.Resource},
			},
		})
		changed = true
	}
	return changed
}
