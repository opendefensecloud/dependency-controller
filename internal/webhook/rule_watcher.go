package webhook

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/kcp-dev/multicluster-provider/apiexport"

	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"

	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
	"go.opendefense.cloud/dependency-controller/internal/fieldpath"
)

// DependencyRuleWatcher watches DependencyRule objects via the dep-ctrl APIExport
// and manages per-rule multicluster managers with indexed caches. This is the
// webhook server's own reconciler — it does not depend on the controller.
type DependencyRuleWatcher struct {
	// DepCtrlManager is the multicluster manager for the dep-ctrl's APIExport.
	DepCtrlManager mcmanager.Manager

	// BaseConfig is the root kcp REST config (no workspace path suffix).
	BaseConfig *rest.Config

	// Scheme is the runtime scheme used when creating dynamic managers.
	Scheme *runtime.Scheme

	// APIExportName is the name of the dep-ctrl APIExport (and its
	// APIExportEndpointSlice), used to resolve the virtual workspace URL
	// during initial registry population.
	APIExportName string

	// Registry holds the rule state queried by the DeletionValidator.
	Registry *RuleRegistry
}

// PopulateRegistry performs an initial population of the rule registry by
// listing all existing DependencyRules from the APIExport virtual workspace.
// It resolves the VW URL from the APIExportEndpointSlice, creates a client
// for the wildcard cluster path, and processes every rule found.
//
// This must be called after the manager has started (e.g., from a
// manager.Runnable) so that the APIExportEndpointSlice is available.
func (w *DependencyRuleWatcher) PopulateRegistry(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("rule-watcher")

	vwClient, err := w.virtualWorkspaceClient(ctx)
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
		if err := w.ensureWatcher(ctx, key, rule); err != nil {
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
func (w *DependencyRuleWatcher) virtualWorkspaceClient(ctx context.Context) (client.Client, error) {
	// Use a direct (non-cached) client to read the APIExportEndpointSlice
	// from the dep-ctrl workspace.
	localCfg := w.DepCtrlManager.GetLocalManager().GetConfig()
	directClient, err := client.New(localCfg, client.Options{Scheme: w.Scheme})
	if err != nil {
		return nil, fmt.Errorf("creating direct client: %w", err)
	}

	var ess apisv1alpha1.APIExportEndpointSlice
	if err := directClient.Get(ctx, client.ObjectKey{Name: w.APIExportName}, &ess); err != nil {
		return nil, fmt.Errorf("getting APIExportEndpointSlice %s: %w", w.APIExportName, err)
	}

	if len(ess.Status.APIExportEndpoints) == 0 {
		return nil, fmt.Errorf("APIExportEndpointSlice %s has no endpoints", w.APIExportName)
	}

	vwURL := ess.Status.APIExportEndpoints[0].URL

	// Create a client for the wildcard cluster path to list across all
	// logical clusters visible through the virtual workspace.
	vwCfg := rest.CopyConfig(localCfg)
	vwCfg.Host = vwURL + "/clusters/*"

	vwClient, err := client.New(vwCfg, client.Options{Scheme: w.Scheme})
	if err != nil {
		return nil, fmt.Errorf("creating VW client: %w", err)
	}

	return vwClient, nil
}

// Reconcile handles DependencyRule events. On creation/update it ensures a
// per-rule indexed cache exists. On deletion it tears down the cache.
func (w *DependencyRuleWatcher) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("rule", req.Name, "cluster", req.ClusterName)

	cl, err := w.DepCtrlManager.GetCluster(ctx, req.ClusterName)
	if err != nil {
		logger.Error(err, "failed to get cluster")
		return ctrl.Result{}, err
	}

	key := ruleStateKey(req.ClusterName, req.Name)

	var rule v1alpha1.DependencyRule
	if err := cl.GetClient().Get(ctx, client.ObjectKey{Name: req.Name}, &rule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("DependencyRule deleted, tearing down cache")
			w.Registry.Unregister(key)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if err := w.ensureWatcher(ctx, key, &rule); err != nil {
		logger.Error(err, "failed to ensure watcher")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// ensureWatcher starts a multicluster manager for the given rule's dependent
// resource type, registers field indices for each dependency target, and stores
// the state in the registry. If the rule already exists, it is a no-op.
func (w *DependencyRuleWatcher) ensureWatcher(ctx context.Context, key string, rule *v1alpha1.DependencyRule) error {
	if w.Registry.Exists(key) {
		return nil
	}

	ref := rule.Spec.Dependent.APIExportRef
	dep := rule.Spec.Dependent

	mgr, mgrCancel, err := w.createExportManager(ctx, ref)
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

	// Register a no-op controller to trigger the informer and track clusters.
	registry := w.Registry
	ruleKey := key
	if err := mcbuilder.ControllerManagedBy(mgr).
		Named(fmt.Sprintf("dep-index-%s", key)).
		For(watchObj).
		Complete(mcreconcile.Func(func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
			registry.TrackCluster(ruleKey, req.ClusterName)
			if !registry.Get(ruleKey).Ready {
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

	// Check again in case a concurrent reconcile raced us.
	if w.Registry.Exists(key) {
		mgrCancel()
		return nil
	}

	w.Registry.Register(key, state)
	return nil
}

// createExportManager creates a new apiexport provider and mcmanager for the
// given APIExport reference. Returns the manager and a cancel function.
func (w *DependencyRuleWatcher) createExportManager(ctx context.Context, ref v1alpha1.APIExportReference) (mcmanager.Manager, context.CancelFunc, error) {
	logger := log.FromContext(ctx).WithValues("apiExport", ref.Name, "path", ref.Path)

	cfg := rest.CopyConfig(w.BaseConfig)
	cfg.Host += logicalcluster.NewPath(ref.Path).RequestPath()

	provider, err := apiexport.New(cfg, ref.Name, apiexport.Options{
		Scheme: w.Scheme,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("creating apiexport provider: %w", err)
	}

	mgr, err := mcmanager.New(cfg, provider, manager.Options{
		Scheme: w.Scheme,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("creating multicluster manager: %w", err)
	}

	mgrCtx, cancel := context.WithCancel(ctx)

	go func() {
		logger.Info("starting dynamic manager for APIExport")
		if err := mgr.Start(mgrCtx); err != nil {
			logger.Error(err, "dynamic manager failed")
		}
	}()

	return mgr, cancel, nil
}

// ruleStateKey returns a qualified key combining the cluster and rule name.
func ruleStateKey(clusterName, ruleName string) string {
	return clusterName + "/" + ruleName
}
