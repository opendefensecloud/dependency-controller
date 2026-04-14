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

	// Registry holds the rule state queried by the DeletionValidator.
	Registry *RuleRegistry
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
