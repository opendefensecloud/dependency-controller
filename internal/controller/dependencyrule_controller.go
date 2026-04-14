package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
)

// DependencyRuleReconciler watches DependencyRule objects across provider workspaces
// via the dep-ctrl's own APIExport. It handles webhook installation and RBAC
// management. Per-rule indexed caches are managed by the webhook server's
// DependencyRuleWatcher instead.
type DependencyRuleReconciler struct {
	// DepCtrlManager is the multicluster manager for the dep-ctrl's APIExport.
	DepCtrlManager mcmanager.Manager

	// WebhookInstaller installs ValidatingWebhookConfigurations in provider
	// workspaces. Nil if webhook installation is not configured.
	WebhookInstaller *WebhookInstaller

	// RBACManager manages the ClusterRole in system:master. Nil if not configured.
	RBACManager *RBACManager
}

func NewDependencyRuleReconciler(depCtrlMgr mcmanager.Manager) *DependencyRuleReconciler {
	return &DependencyRuleReconciler{
		DepCtrlManager: depCtrlMgr,
	}
}

// Reconcile handles DependencyRule events: installs/removes webhooks and
// reconciles RBAC in system:master.
func (r *DependencyRuleReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("rule", req.Name, "cluster", req.ClusterName)

	cl, err := r.DepCtrlManager.GetCluster(ctx, req.ClusterName)
	if err != nil {
		logger.Error(err, "failed to get cluster")
		return ctrl.Result{}, err
	}

	ruleKey := req.ClusterName + "/" + req.Name

	var rule v1alpha1.DependencyRule
	if err := cl.GetClient().Get(ctx, client.ObjectKey{Name: req.Name}, &rule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("DependencyRule deleted")
			return ctrl.Result{}, r.handleDeletion(ctx, ruleKey, req.Name)
		}
		return ctrl.Result{}, err
	}

	if r.WebhookInstaller != nil {
		if err := r.WebhookInstaller.EnsureWebhooks(ctx, ruleKey, &rule); err != nil {
			logger.Error(err, "failed to ensure webhooks")
			return ctrl.Result{}, err
		}
	}

	if r.RBACManager != nil {
		dep := rule.Spec.Dependent
		gvr := schema.GroupVersionResource{
			Group:    dep.Group,
			Version:  dep.Version,
			Resource: dep.Resource,
		}
		r.RBACManager.TrackRule(ruleKey, gvr)
		if err := r.RBACManager.Reconcile(ctx); err != nil {
			logger.Error(err, "failed to reconcile RBAC")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// handleDeletion cleans up webhooks and RBAC for a deleted DependencyRule.
func (r *DependencyRuleReconciler) handleDeletion(ctx context.Context, key, ruleName string) error {
	logger := log.FromContext(ctx).WithValues("rule", ruleName)

	if r.WebhookInstaller != nil {
		if err := r.WebhookInstaller.RemoveWebhooks(ctx, key); err != nil {
			return fmt.Errorf("removing webhooks for rule %s: %w", ruleName, err)
		}
	}

	if r.RBACManager != nil {
		r.RBACManager.RemoveRule(key)
		if err := r.RBACManager.Reconcile(ctx); err != nil {
			logger.Error(err, "failed to reconcile RBAC after rule deletion")
			// Non-fatal: the role may have stale entries but won't break anything.
		}
	}

	return nil
}
