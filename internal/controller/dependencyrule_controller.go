package controller

import (
	"context"
	"fmt"
	"sync"

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
)

// DependencyRuleReconciler watches DependencyRule objects across provider workspaces
// via the dep-ctrl's own APIExport, and dynamically sets up multicluster watchers
// on dependent resource types via the referenced APIExports.
type DependencyRuleReconciler struct {
	// DepCtrlManager is the multicluster manager for the dep-ctrl's APIExport.
	DepCtrlManager mcmanager.Manager

	// BaseConfig is the root kcp REST config (no workspace path suffix).
	BaseConfig *rest.Config

	// Scheme is the runtime scheme used when creating dynamic managers.
	Scheme *runtime.Scheme

	// WebhookInstaller installs ValidatingWebhookConfigurations in provider
	// workspaces. Nil if webhook installation is not configured.
	WebhookInstaller *WebhookInstaller

	mu             sync.Mutex
	exportManagers map[string]*exportManagerState // keyed by "path/name"
}

// exportManagerState tracks a dynamically created mcmanager for a specific APIExport.
type exportManagerState struct {
	manager        mcmanager.Manager
	cancel         context.CancelFunc
	activeWatchers map[string]struct{} // keyed by rule name
}

func NewDependencyRuleReconciler(depCtrlMgr mcmanager.Manager, baseCfg *rest.Config, scheme *runtime.Scheme) *DependencyRuleReconciler {
	return &DependencyRuleReconciler{
		DepCtrlManager: depCtrlMgr,
		BaseConfig:     baseCfg,
		Scheme:         scheme,
		exportManagers: make(map[string]*exportManagerState),
	}
}

// Reconcile is a multicluster reconciler for DependencyRule objects discovered
// via the dep-ctrl's APIExport virtual workspace.
func (r *DependencyRuleReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("rule", req.Name, "cluster", req.ClusterName)

	cl, err := r.DepCtrlManager.GetCluster(ctx, req.ClusterName)
	if err != nil {
		logger.Error(err, "failed to get cluster")
		return ctrl.Result{}, err
	}

	var rule v1alpha1.DependencyRule
	if err := cl.GetClient().Get(ctx, client.ObjectKey{Name: req.Name}, &rule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("DependencyRule deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if err := r.ensureWatcher(ctx, &rule); err != nil {
		logger.Error(err, "failed to ensure watcher")
		return ctrl.Result{}, err
	}

	if r.WebhookInstaller != nil {
		if err := r.WebhookInstaller.EnsureWebhooks(ctx, &rule); err != nil {
			logger.Error(err, "failed to ensure webhooks")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// ensureWatcher starts a multicluster dependent resource watcher for the given rule
// if one isn't already running. The watcher uses a dynamic mcmanager created for
// the APIExport referenced in the rule.
func (r *DependencyRuleReconciler) ensureWatcher(ctx context.Context, rule *v1alpha1.DependencyRule) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	ref := rule.Spec.Dependent.APIExportRef
	key := ref.Path + "/" + ref.Name

	state, exists := r.exportManagers[key]
	if !exists {
		var err error
		state, err = r.createExportManager(ctx, ref)
		if err != nil {
			return fmt.Errorf("creating manager for APIExport %s: %w", key, err)
		}
		r.exportManagers[key] = state
	}

	if _, watched := state.activeWatchers[rule.Name]; watched {
		return nil
	}

	dep := rule.Spec.Dependent
	gvr := schema.GroupVersionResource{
		Group:    dep.Group,
		Version:  dep.Version,
		Resource: dep.Resource,
	}
	gvk := schema.GroupVersionKind{
		Group:   dep.Group,
		Version: dep.Version,
		Kind:    dep.Kind,
	}

	reconciler := &DependentReconciler{
		DepCtrlManager:   r.DepCtrlManager,
		DependentManager: state.manager,
		RuleName:         rule.Name,
		Dependent:        gvr,
		DependentKind:    gvk,
		Dependencies:     rule.Spec.Dependencies,
	}

	watchObj := &unstructured.Unstructured{}
	watchObj.SetGroupVersionKind(gvk)

	if err := mcbuilder.ControllerManagedBy(state.manager).
		Named(fmt.Sprintf("dependent-%s", rule.Name)).
		For(watchObj).
		Complete(mcreconcile.Func(reconciler.Reconcile)); err != nil {
		return fmt.Errorf("registering dependent controller for rule %s: %w", rule.Name, err)
	}

	state.activeWatchers[rule.Name] = struct{}{}
	return nil
}

// createExportManager creates a new apiexport provider and mcmanager for the
// given APIExport reference.
func (r *DependencyRuleReconciler) createExportManager(ctx context.Context, ref v1alpha1.APIExportReference) (*exportManagerState, error) {
	logger := log.FromContext(ctx).WithValues("apiExport", ref.Name, "path", ref.Path)

	// Construct REST config pointing to the workspace where the APIExport lives.
	cfg := rest.CopyConfig(r.BaseConfig)
	cfg.Host += logicalcluster.NewPath(ref.Path).RequestPath()

	provider, err := apiexport.New(cfg, ref.Name, apiexport.Options{
		Scheme: r.Scheme,
	})
	if err != nil {
		return nil, fmt.Errorf("creating apiexport provider: %w", err)
	}

	mgr, err := mcmanager.New(cfg, provider, manager.Options{
		Scheme: r.Scheme,
	})
	if err != nil {
		return nil, fmt.Errorf("creating multicluster manager: %w", err)
	}

	mgrCtx, cancel := context.WithCancel(ctx)

	go func() {
		logger.Info("starting dynamic manager for APIExport")
		if err := mgr.Start(mgrCtx); err != nil {
			logger.Error(err, "dynamic manager failed")
		}
	}()

	return &exportManagerState{
		manager:        mgr,
		cancel:         cancel,
		activeWatchers: make(map[string]struct{}),
	}, nil
}
