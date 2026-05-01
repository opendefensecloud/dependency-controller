// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/kcp-dev/logicalcluster/v3"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
)

// DependencyRuleReconciler watches DependencyRule objects across provider workspaces
// via the dep-ctrl's own APIExport. It handles webhook installation in
// dependency provider workspaces. Per-rule indexed caches are managed by the
// webhook server's RuleCacheManager instead.
type DependencyRuleReconciler struct {
	// DepCtrlManager is the multicluster manager for the dep-ctrl's APIExport.
	DepCtrlManager mcmanager.Manager

	// WebhookInstaller installs ValidatingWebhookConfigurations in provider
	// workspaces. Nil if webhook installation is not configured.
	WebhookInstaller *WebhookInstaller

	// BaseConfig is the kcp front-proxy REST config (no workspace path suffix).
	// Used for webhook installation (routed to the correct shard per workspace)
	// and to resolve workspace paths to logical cluster names.
	BaseConfig *rest.Config

	// mu protects lazy initialization of wsResolver.
	mu         sync.Mutex
	wsResolver *workspaceResolver
}

func NewDependencyRuleReconciler(depCtrlMgr mcmanager.Manager) *DependencyRuleReconciler {
	return &DependencyRuleReconciler{
		DepCtrlManager: depCtrlMgr,
	}
}

// ensureInitialized lazily creates the workspace resolver used to map
// workspace paths to logical cluster names for webhook installation.
func (r *DependencyRuleReconciler) ensureInitialized() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.wsResolver != nil {
		return nil
	}

	localMgr := r.DepCtrlManager.GetLocalManager()

	// Create workspace resolver using the base kcp config (front-proxy URL).
	rootCfg := rest.CopyConfig(r.BaseConfig)
	rootCfg.Host += logicalcluster.NewPath("root").RequestPath()
	r.wsResolver = &workspaceResolver{rootCfg: rootCfg, scheme: localMgr.GetScheme()}

	return nil
}

// Reconcile handles DependencyRule events: installs/removes webhooks in
// dependency provider workspaces.
func (r *DependencyRuleReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("rule", req.Name, "cluster", req.ClusterName)

	if err := r.ensureInitialized(); err != nil {
		logger.Error(err, "failed to initialize workspace resolver")
		return ctrl.Result{}, err
	}

	cl, err := r.DepCtrlManager.GetCluster(ctx, req.ClusterName)
	if err != nil {
		logger.Error(err, "failed to get cluster")
		return ctrl.Result{}, err
	}

	ruleKey := string(req.ClusterName) + "/" + req.Name

	var rule v1alpha1.DependencyRule
	if err := cl.GetClient().Get(ctx, client.ObjectKey{Name: req.Name}, &rule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("DependencyRule deleted")
			return ctrl.Result{}, r.handleDeletion(ctx, ruleKey, req.Name)
		}

		return ctrl.Result{}, err
	}

	// Resolve workspace paths to logical cluster names for VW access.
	wsPaths := r.collectWorkspacePaths(&rule)
	if err := r.wsResolver.ensureResolved(ctx, wsPaths); err != nil {
		logger.Error(err, "failed to resolve workspace paths")
		return ctrl.Result{}, err
	}

	if r.WebhookInstaller != nil {
		if err := r.ensureWebhooks(ctx, ruleKey, &rule); err != nil {
			logger.Error(err, "failed to ensure webhooks")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// ensureWebhooks installs webhooks in dependency provider workspaces via the virtual workspace.
func (r *DependencyRuleReconciler) ensureWebhooks(ctx context.Context, ruleKey string, rule *v1alpha1.DependencyRule) error {
	// Build a mapping from workspace path to logical cluster name.
	clusterNames := make(map[string]string, len(rule.Spec.Dependencies))
	for _, dep := range rule.Spec.Dependencies {
		wsPath := dep.APIExportRef.Path
		if _, ok := clusterNames[wsPath]; ok {
			continue
		}

		clusterName, err := r.wsResolver.resolve(wsPath)
		if err != nil {
			return fmt.Errorf("resolving %s: %w", wsPath, err)
		}

		clusterNames[wsPath] = clusterName
	}

	return r.WebhookInstaller.EnsureWebhooks(ctx, ruleKey, rule, clusterNames)
}

// collectWorkspacePaths extracts dependency workspace paths referenced in a DependencyRule.
func (r *DependencyRuleReconciler) collectWorkspacePaths(rule *v1alpha1.DependencyRule) []string {
	seen := make(map[string]struct{})
	var paths []string

	for _, dep := range rule.Spec.Dependencies {
		if _, ok := seen[dep.APIExportRef.Path]; !ok {
			seen[dep.APIExportRef.Path] = struct{}{}
			paths = append(paths, dep.APIExportRef.Path)
		}
	}

	return paths
}

// handleDeletion cleans up webhooks for a deleted DependencyRule.
func (r *DependencyRuleReconciler) handleDeletion(ctx context.Context, key, ruleName string) error {
	if r.WebhookInstaller != nil {
		if err := r.WebhookInstaller.RemoveWebhooks(ctx, key); err != nil {
			return fmt.Errorf("removing webhooks for rule %s: %w", ruleName, err)
		}
	}

	return nil
}

// workspaceResolver maps kcp workspace paths (e.g., "root:compute-provider")
// to logical cluster names by reading Workspace objects from the root workspace.
type workspaceResolver struct {
	rootCfg *rest.Config
	scheme  *runtime.Scheme

	mu    sync.Mutex
	cache map[string]string // workspace path -> logical cluster name
}

// ensureResolved ensures all the given workspace paths are resolved to logical
// cluster names, fetching any missing mappings from the root workspace.
func (w *workspaceResolver) ensureResolved(ctx context.Context, paths []string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.cache == nil {
		w.cache = make(map[string]string)
	}

	var missing []string
	for _, path := range paths {
		if _, ok := w.cache[path]; !ok {
			missing = append(missing, path)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	c, err := client.New(w.rootCfg, client.Options{Scheme: w.scheme})
	if err != nil {
		return fmt.Errorf("creating root workspace client: %w", err)
	}

	for _, path := range missing {
		// Extract the workspace name from path like "root:compute-provider".
		parts := strings.Split(path, ":")
		wsName := parts[len(parts)-1]

		var ws tenancyv1alpha1.Workspace
		if err := c.Get(ctx, client.ObjectKey{Name: wsName}, &ws); err != nil {
			return fmt.Errorf("getting workspace %s: %w", wsName, err)
		}

		if ws.Spec.Cluster == "" {
			return fmt.Errorf("workspace %s has no logical cluster name", wsName)
		}

		w.cache[path] = ws.Spec.Cluster
		log.FromContext(ctx).Info("resolved workspace path", "path", path, "cluster", ws.Spec.Cluster)
	}

	return nil
}

// resolve returns the logical cluster name for a workspace path.
func (w *workspaceResolver) resolve(path string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	name, ok := w.cache[path]
	if !ok {
		return "", fmt.Errorf("workspace path %s not resolved", path)
	}

	return name, nil
}
