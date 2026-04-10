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

	mu        sync.Mutex
	ruleState map[string]*ruleManagerState // keyed by rule name
}

// ruleManagerState tracks the dynamic manager, reconciler, and cancel function
// for a single DependencyRule. Each rule gets its own manager so that cancelling
// it cleanly stops the watch/informer and controller.
type ruleManagerState struct {
	manager    mcmanager.Manager
	reconciler *DependentReconciler
	cancel     context.CancelFunc
}

func NewDependencyRuleReconciler(depCtrlMgr mcmanager.Manager, baseCfg *rest.Config, scheme *runtime.Scheme) *DependencyRuleReconciler {
	return &DependencyRuleReconciler{
		DepCtrlManager: depCtrlMgr,
		BaseConfig:     baseCfg,
		Scheme:         scheme,
		ruleState:      make(map[string]*ruleManagerState),
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

	ruleStateKey := ruleStateKey(req.ClusterName, req.Name)

	var rule v1alpha1.DependencyRule
	if err := cl.GetClient().Get(ctx, client.ObjectKey{Name: req.Name}, &rule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("DependencyRule deleted")
			return ctrl.Result{}, r.handleDeletion(ctx, ruleStateKey, req.Name)
		}
		return ctrl.Result{}, err
	}

	if err := r.ensureWatcher(ctx, ruleStateKey, req.ClusterName, &rule); err != nil {
		logger.Error(err, "failed to ensure watcher")
		return ctrl.Result{}, err
	}

	if r.WebhookInstaller != nil {
		if err := r.WebhookInstaller.EnsureWebhooks(ctx, ruleStateKey, &rule); err != nil {
			logger.Error(err, "failed to ensure webhooks")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// ensureWatcher starts a multicluster dependent resource watcher for the given rule
// if one isn't already running. Each rule gets its own mcmanager so that it can
// be stopped independently when the rule is deleted.
func (r *DependencyRuleReconciler) ensureWatcher(ctx context.Context, key, clusterName string, rule *v1alpha1.DependencyRule) error {
	r.mu.Lock()
	if state, exists := r.ruleState[key]; exists {
		// Update dependencies in case the rule spec changed.
		state.reconciler.Dependencies = rule.Spec.Dependencies
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	// Create the manager and controller outside the lock to avoid blocking
	// other rule reconciliations during potentially slow operations.
	ref := rule.Spec.Dependent.APIExportRef
	dep := rule.Spec.Dependent

	mgr, mgrCancel, err := r.createExportManager(ctx, ref)
	if err != nil {
		return fmt.Errorf("creating manager for rule %s: %w", rule.Name, err)
	}

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
		DependentManager: mgr,
		RuleName:         rule.Name,
		RuleCluster:      clusterName,
		Dependent:        gvr,
		DependentKind:    gvk,
		Dependencies:     rule.Spec.Dependencies,
	}

	watchObj := &unstructured.Unstructured{}
	watchObj.SetGroupVersionKind(gvk)

	if err := mcbuilder.ControllerManagedBy(mgr).
		Named(fmt.Sprintf("dependent-%s", key)).
		For(watchObj).
		Complete(mcreconcile.Func(reconciler.Reconcile)); err != nil {
		mgrCancel()
		return fmt.Errorf("registering dependent controller for rule %s: %w", rule.Name, err)
	}

	// Re-lock to insert. Check again in case a concurrent reconcile raced us.
	r.mu.Lock()
	if _, exists := r.ruleState[key]; exists {
		r.mu.Unlock()
		// Another goroutine won the race — discard the manager we just created.
		mgrCancel()
		return nil
	}
	r.ruleState[key] = &ruleManagerState{
		manager:    mgr,
		reconciler: reconciler,
		cancel:     mgrCancel,
	}
	r.mu.Unlock()
	return nil
}

// ruleStateKey returns a qualified key combining the cluster and rule name.
func ruleStateKey(clusterName, ruleName string) string {
	return clusterName + "/" + ruleName
}

// handleDeletion cleans up all resources associated with a deleted DependencyRule:
// Dependencies are deleted across all known clusters, the dynamic manager is stopped
// (which tears down the watch and controller), and webhook rules are removed.
func (r *DependencyRuleReconciler) handleDeletion(ctx context.Context, key, ruleName string) error {
	logger := log.FromContext(ctx).WithValues("rule", ruleName)

	if r.WebhookInstaller != nil {
		if err := r.WebhookInstaller.RemoveWebhooks(ctx, key); err != nil {
			return fmt.Errorf("removing webhooks for rule %s: %w", ruleName, err)
		}
	}

	r.mu.Lock()
	state, exists := r.ruleState[key]
	if !exists {
		r.mu.Unlock()
		return nil
	}
	delete(r.ruleState, key)
	r.mu.Unlock()

	// Actively clean up all Dependencies created by this rule across all
	// clusters the reconciler has seen.
	if err := state.reconciler.CleanupAll(ctx); err != nil {
		logger.Error(err, "failed to clean up all dependencies")
		// Continue to stop the manager even if cleanup partially fails.
	}

	// Cancel the manager context — this stops the controller, the informer
	// watch, and all associated goroutines.
	logger.Info("stopping dynamic manager for deleted rule")
	state.cancel()

	return nil
}

// createExportManager creates a new apiexport provider and mcmanager for the
// given APIExport reference. Returns the manager and a cancel function.
func (r *DependencyRuleReconciler) createExportManager(ctx context.Context, ref v1alpha1.APIExportReference) (mcmanager.Manager, context.CancelFunc, error) {
	logger := log.FromContext(ctx).WithValues("apiExport", ref.Name, "path", ref.Path)

	// Construct REST config pointing to the workspace where the APIExport lives.
	cfg := rest.CopyConfig(r.BaseConfig)
	cfg.Host += logicalcluster.NewPath(ref.Path).RequestPath()

	provider, err := apiexport.New(cfg, ref.Name, apiexport.Options{
		Scheme: r.Scheme,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("creating apiexport provider: %w", err)
	}

	mgr, err := mcmanager.New(cfg, provider, manager.Options{
		Scheme: r.Scheme,
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
