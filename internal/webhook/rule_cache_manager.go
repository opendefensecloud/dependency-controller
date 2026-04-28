// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"fmt"

	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/kcp-dev/multicluster-provider/apiexport"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mccontroller "sigs.k8s.io/multicluster-runtime/pkg/controller"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
	"go.opendefense.cloud/dependency-controller/internal/fieldpath"
	"go.opendefense.cloud/dependency-controller/internal/kcp"
)

// RuleCacheManager reconciles DependencyRule objects and manages a dedicated
// indexed cache per rule. Each rule's dependent resource type (e.g., VirtualMachine)
// is watched through a multicluster manager connected to the provider's APIExport
// virtual workspace. Field indices on the dependent resources allow the
// DeletionValidator to efficiently query "which dependents reference resource X?".
type RuleCacheManager struct {
	// DepCtrlManager is the multicluster manager for the dep-ctrl's APIExport.
	DepCtrlManager mcmanager.Manager

	// BaseConfig is the root kcp REST config (no workspace path suffix).
	BaseConfig *rest.Config

	// Scheme is the runtime scheme used when creating per-rule managers.
	Scheme *runtime.Scheme

	// APIExportName is the name of the dep-ctrl APIExport (and its
	// APIExportEndpointSlice), used to resolve the virtual workspace URL
	// during initial registry population.
	APIExportName string

	// Registry holds the per-rule cache state queried by the DeletionValidator.
	Registry *RuleRegistry
}

// PopulateRegistry lists all existing DependencyRules from every shard's
// APIExport virtual workspace and ensures an indexed cache exists for each one.
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
			if err := m.ensureCache(ctx, key, clusterName.String(), rule); err != nil {
				return fmt.Errorf("populating rule %s/%s: %w", clusterName, rule.Name, err)
			}
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

// Reconcile handles DependencyRule events. On creation/update it ensures an
// indexed cache exists for the rule. On deletion it tears down the cache.
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
			logger.Info("DependencyRule deleted, tearing down cache")
			m.Registry.Unregister(key)

			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	if err := m.ensureCache(ctx, key, clusterName, &rule); err != nil {
		logger.Error(err, "failed to ensure cache for rule")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// ensureCache creates a multicluster manager for the rule's dependent resource
// type, registers field indices for each dependency target, and stores the
// resulting cache state in the registry. If a cache already exists for the
// given key this is a no-op.
func (m *RuleCacheManager) ensureCache(ctx context.Context, key string, clusterName string, rule *v1alpha1.DependencyRule) error {
	if m.Registry.Exists(key) {
		return nil
	}

	dep := rule.Spec.Dependent

	mgr, mgrCancel, err := m.startProviderManager(ctx, clusterName, dep.APIExportName)
	if err != nil {
		return fmt.Errorf("creating manager for rule %s: %w", rule.Name, err)
	}

	gvk := schema.GroupVersionKind{
		Group:   dep.Group,
		Version: dep.Version,
		Kind:    dep.Kind,
	}

	// Build indexed fields for each dependency target.
	var indexFields []IndexedField
	watchObj := &unstructured.Unstructured{}
	watchObj.SetGroupVersionKind(gvk)

	for _, depTarget := range rule.Spec.Dependencies {
		fieldPath := depTarget.FieldRef.Path
		targetGVR := schema.GroupVersionResource{
			Group:    depTarget.Group,
			Version:  depTarget.Version,
			Resource: depTarget.Resource,
		}

		// Register an index on the dependent resource's field path.
		if err := mgr.GetFieldIndexer().IndexField(ctx, watchObj, fieldPath, func(obj client.Object) []string {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return nil
			}
			val := fieldpath.Resolve(u.Object, fieldPath)
			if val == "" {
				return nil
			}

			return []string{val}
		}); err != nil {
			mgrCancel()
			return fmt.Errorf("indexing field %s for rule %s: %w", fieldPath, rule.Name, err)
		}

		indexFields = append(indexFields, IndexedField{
			FieldPath: fieldPath,
			TargetGVR: targetGVR,
		})
	}

	// Register a controller to activate the informer and track discovered clusters.
	registry := m.Registry
	ruleKey := key
	if err := mcbuilder.ControllerManagedBy(mgr).
		Named(fmt.Sprintf("dep-index-%s", key)).
		WithOptions(mccontroller.Options{SkipNameValidation: new(true)}).
		For(watchObj).
		Complete(mcreconcile.Func(func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
			registry.TrackCluster(ruleKey, string(req.ClusterName))
			if state := registry.Get(ruleKey); state != nil && !state.IsReady() {
				registry.MarkReady(ruleKey)
			}

			return ctrl.Result{}, nil
		})); err != nil {
		mgrCancel()
		return fmt.Errorf("registering index controller for rule %s: %w", rule.Name, err)
	}

	state := &RuleState{
		Manager:      mgr,
		Cancel:       mgrCancel,
		Rule:         rule.Spec,
		DependentGVK: gvk,
		IndexFields:  indexFields,
	}

	// Register atomically; if a concurrent reconcile raced us, cancel the
	// duplicate manager we just created.
	if old := m.Registry.Register(key, state); old != nil {
		old.Cancel()
	}

	return nil
}

// startProviderManager creates a new multicluster manager backed by the given
// APIExport's virtual workspace. The manager is started in a background
// goroutine and the returned cancel function tears it down.
func (m *RuleCacheManager) startProviderManager(ctx context.Context, clusterName string, apiExportName string) (mcmanager.Manager, context.CancelFunc, error) {
	logger := log.FromContext(ctx).WithValues("apiExport", apiExportName, "cluster", clusterName)

	cfg := rest.CopyConfig(m.BaseConfig)
	cfg.Host += logicalcluster.NewPath(clusterName).RequestPath()

	// Resolve the APIExportEndpointSlice name — it may differ from the APIExport name.
	wsClient, err := client.New(cfg, client.Options{Scheme: m.Scheme})
	if err != nil {
		return nil, nil, fmt.Errorf("creating client for endpoint slice discovery: %w", err)
	}

	ess, err := kcp.FindEndpointSlice(ctx, wsClient, apiExportName)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving APIExportEndpointSlice for %s: %w", apiExportName, err)
	}

	provider, err := apiexport.New(cfg, ess.Name, apiexport.Options{
		Scheme: m.Scheme,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("creating apiexport provider: %w", err)
	}

	mgr, err := mcmanager.New(cfg, provider, manager.Options{
		Scheme:                 m.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		return nil, nil, fmt.Errorf("creating multicluster manager: %w", err)
	}

	mgrCtx, cancel := context.WithCancel(ctx)

	go func() {
		logger.Info("starting provider manager for APIExport")
		if err := mgr.Start(mgrCtx); err != nil {
			logger.Error(err, "provider manager failed")
		}
	}()

	return mgr, cancel, nil
}

// ruleStateKey returns a qualified key combining the cluster and rule name.
func ruleStateKey(clusterName, ruleName string) string {
	return clusterName + "/" + ruleName
}
