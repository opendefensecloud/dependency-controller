// Copyright 2026 Open Defense and dependency-controller contributors
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"fmt"

	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/kcp-dev/multicluster-provider/apiexport"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
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
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
	"go.opendefense.cloud/dependency-controller/internal/fieldpath"
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

// PopulateRegistry lists all existing DependencyRules from the APIExport virtual
// workspace and ensures an indexed cache exists for each one. This must be called
// after the manager has started (e.g., from a manager.Runnable) so that the
// APIExportEndpointSlice is available.
func (m *RuleCacheManager) PopulateRegistry(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("rule-cache-manager")

	vwClient, err := m.virtualWorkspaceClient(ctx)
	if err != nil {
		return fmt.Errorf("creating virtual workspace client: %w", err)
	}

	var ruleList v1alpha1.DependencyRuleList
	if err := vwClient.List(ctx, &ruleList); err != nil {
		return fmt.Errorf("listing initial DependencyRules: %w", err)
	}

	logger.Info("populating rule registry", "ruleCount", len(ruleList.Items))

	for i := range ruleList.Items {
		rule := &ruleList.Items[i]
		clusterName := logicalcluster.From(rule)
		key := ruleStateKey(clusterName.String(), rule.Name)
		if err := m.ensureCache(ctx, key, rule); err != nil {
			return fmt.Errorf("populating rule %s/%s: %w", clusterName, rule.Name, err)
		}
	}

	logger.Info("rule registry populated")

	return nil
}

// virtualWorkspaceClient reads the APIExportEndpointSlice from the dep-ctrl
// workspace to discover the virtual workspace URL, then returns a client
// pointing at {vwURL}/clusters/* so it can list resources across all bound
// workspaces.
func (m *RuleCacheManager) virtualWorkspaceClient(ctx context.Context) (client.Client, error) {
	// Use a direct (non-cached) client to read the APIExportEndpointSlice
	// from the dep-ctrl workspace.
	localCfg := m.DepCtrlManager.GetLocalManager().GetConfig()
	directClient, err := client.New(localCfg, client.Options{Scheme: m.Scheme})
	if err != nil {
		return nil, fmt.Errorf("creating direct client: %w", err)
	}

	var ess apisv1alpha1.APIExportEndpointSlice
	if err := directClient.Get(ctx, client.ObjectKey{Name: m.APIExportName}, &ess); err != nil {
		return nil, fmt.Errorf("getting APIExportEndpointSlice %s: %w", m.APIExportName, err)
	}

	if len(ess.Status.APIExportEndpoints) == 0 {
		return nil, fmt.Errorf("APIExportEndpointSlice %s has no endpoints", m.APIExportName)
	}

	vwURL := ess.Status.APIExportEndpoints[0].URL

	// Create a client for the wildcard cluster path to list across all
	// logical clusters visible through the virtual workspace.
	vwCfg := rest.CopyConfig(localCfg)
	vwCfg.Host = vwURL + "/clusters/*"

	vwClient, err := client.New(vwCfg, client.Options{Scheme: m.Scheme})
	if err != nil {
		return nil, fmt.Errorf("creating VW client: %w", err)
	}

	return vwClient, nil
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

	key := ruleStateKey(req.ClusterName, req.Name)

	var rule v1alpha1.DependencyRule
	if err := cl.GetClient().Get(ctx, client.ObjectKey{Name: req.Name}, &rule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("DependencyRule deleted, tearing down cache")
			m.Registry.Unregister(key)

			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	if err := m.ensureCache(ctx, key, &rule); err != nil {
		logger.Error(err, "failed to ensure cache for rule")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// ensureCache creates a multicluster manager for the rule's dependent resource
// type, registers field indices for each dependency target, and stores the
// resulting cache state in the registry. If a cache already exists for the
// given key this is a no-op.
func (m *RuleCacheManager) ensureCache(ctx context.Context, key string, rule *v1alpha1.DependencyRule) error {
	if m.Registry.Exists(key) {
		return nil
	}

	ref := rule.Spec.Dependent.APIExportRef
	dep := rule.Spec.Dependent

	mgr, mgrCancel, err := m.startProviderManager(ctx, ref)
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
		For(watchObj).
		Complete(mcreconcile.Func(func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
			registry.TrackCluster(ruleKey, req.ClusterName)
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
func (m *RuleCacheManager) startProviderManager(ctx context.Context, ref v1alpha1.APIExportReference) (mcmanager.Manager, context.CancelFunc, error) {
	logger := log.FromContext(ctx).WithValues("apiExport", ref.Name, "path", ref.Path)

	cfg := rest.CopyConfig(m.BaseConfig)
	cfg.Host += logicalcluster.NewPath(ref.Path).RequestPath()

	provider, err := apiexport.New(cfg, ref.Name, apiexport.Options{
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
