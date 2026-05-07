// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"fmt"

	"github.com/kcp-dev/logicalcluster/v3"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
	"go.opendefense.cloud/dependency-controller/internal/kcp"
)

// RuleCacheManager reconciles DependencyRule objects and populates the
// RuleRegistry with rule metadata. The DeletionValidator uses the registry
// to look up which rules protect a given resource type, then queries
// dependent resources directly per admission request.
type RuleCacheManager struct {
	// DepCtrlManager is the multicluster manager for the dep-ctrl's APIExport.
	DepCtrlManager mcmanager.Manager

	// Scheme is the runtime scheme used when creating clients.
	Scheme *runtime.Scheme

	// APIExportName is the name of the dep-ctrl APIExport (and its
	// APIExportEndpointSlice), used to resolve the virtual workspace URL
	// during initial registry population.
	APIExportName string

	// Registry holds the rule metadata queried by the DeletionValidator.
	Registry *RuleRegistry
}

// PopulateRegistry lists all existing DependencyRules from every shard's
// APIExport virtual workspace and registers their metadata in the registry.
// This must be called after the manager has started (e.g., from a
// manager.Runnable) so that the APIExportEndpointSlice is available.
func (m *RuleCacheManager) PopulateRegistry(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("rule-cache-manager")

	vwClients, err := m.virtualWorkspaceClients(ctx)
	if err != nil {
		return fmt.Errorf("creating virtual workspace clients: %w", err)
	}

	var totalRules int
	for _, vwClient := range vwClients {
		var ruleList v1alpha1.DependencyRuleList
		if err := vwClient.List(ctx, &ruleList); err != nil {
			return fmt.Errorf("listing initial DependencyRules: %w", err)
		}

		totalRules += len(ruleList.Items)

		for i := range ruleList.Items {
			rule := &ruleList.Items[i]
			clusterName := logicalcluster.From(rule)
			key := ruleStateKey(clusterName.String(), rule.Name)
			m.registerRule(key, rule)
		}
	}

	logger.Info("rule registry populated", "ruleCount", totalRules, "shards", len(vwClients))

	return nil
}

// virtualWorkspaceClients reads the APIExportEndpointSlice from the dep-ctrl
// workspace to discover the virtual workspace URLs (one per kcp shard), then
// returns a client per shard pointing at {vwURL}/clusters/* so it can list
// resources across all bound workspaces on that shard.
func (m *RuleCacheManager) virtualWorkspaceClients(ctx context.Context) ([]client.Client, error) {
	// Use a direct (non-cached) client to read the APIExportEndpointSlice
	// from the dep-ctrl workspace.
	localCfg := m.DepCtrlManager.GetLocalManager().GetConfig()
	directClient, err := client.New(localCfg, client.Options{Scheme: m.Scheme})
	if err != nil {
		return nil, fmt.Errorf("creating direct client: %w", err)
	}

	ess, err := kcp.FindEndpointSlice(ctx, directClient, m.APIExportName)
	if err != nil {
		return nil, fmt.Errorf("resolving APIExportEndpointSlice for %s: %w", m.APIExportName, err)
	}

	if len(ess.Status.APIExportEndpoints) == 0 {
		return nil, fmt.Errorf("APIExportEndpointSlice %s has no endpoints", ess.Name)
	}

	// Create a client for each shard's virtual workspace URL.
	var clients []client.Client
	for _, ep := range ess.Status.APIExportEndpoints {
		vwCfg := rest.CopyConfig(localCfg)
		vwCfg.Host = ep.URL + "/clusters/*"

		vwClient, err := client.New(vwCfg, client.Options{Scheme: m.Scheme})
		if err != nil {
			return nil, fmt.Errorf("creating VW client for %s: %w", ep.URL, err)
		}

		clients = append(clients, vwClient)
	}

	return clients, nil
}

// Reconcile handles DependencyRule events. On creation/update it registers
// the rule metadata. On deletion it removes it from the registry.
func (m *RuleCacheManager) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("rule", req.Name, "cluster", req.ClusterName)

	cl, err := m.DepCtrlManager.GetCluster(ctx, req.ClusterName)
	if err != nil {
		logger.Error(err, "failed to get cluster")
		return ctrl.Result{}, err
	}

	clusterName := string(req.ClusterName)
	key := ruleStateKey(clusterName, req.Name)

	var rule v1alpha1.DependencyRule
	if err := cl.GetClient().Get(ctx, client.ObjectKey{Name: req.Name}, &rule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("DependencyRule deleted, removing from registry")
			m.Registry.Unregister(key)

			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	m.registerRule(key, &rule)

	return ctrl.Result{}, nil
}

// registerRule extracts metadata from a DependencyRule and stores it in the
// registry.
func (m *RuleCacheManager) registerRule(key string, rule *v1alpha1.DependencyRule) {
	dep := rule.Spec.Dependent
	gvk := schema.GroupVersionKind{
		Group:   dep.Group,
		Version: dep.Version,
		Kind:    dep.Kind,
	}
	gvr := schema.GroupVersionResource{
		Group:    dep.Group,
		Version:  dep.Version,
		Resource: dep.Resource,
	}

	var indexFields []IndexedField
	for _, depTarget := range rule.Spec.Dependencies {
		indexFields = append(indexFields, IndexedField{
			FieldPath: depTarget.FieldRef.Path,
			TargetGVR: schema.GroupVersionResource{
				Group:    depTarget.Group,
				Version:  depTarget.Version,
				Resource: depTarget.Resource,
			},
		})
	}

	m.Registry.Register(key, &RuleState{
		Rule:         rule.Spec,
		DependentGVK: gvk,
		DependentGVR: gvr,
		IndexFields:  indexFields,
	})
}

// ruleStateKey returns a qualified key combining the cluster and rule name.
func ruleStateKey(clusterName, ruleName string) string {
	return clusterName + "/" + ruleName
}
