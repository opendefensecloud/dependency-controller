// Copyright 2026 Open Defense and dependency-controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sync"

	"github.com/kcp-dev/logicalcluster/v3"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	rbacClusterRoleName        = "dependency-controller-webhook"
	rbacClusterRoleBindingName = "dependency-controller-webhook"
)

// ExportRef identifies an APIExport by workspace path and name.
type ExportRef struct {
	WorkspacePath string
	ExportName    string
}

// RBACManager maintains ClusterRoles and ClusterRoleBindings in each provider
// workspace that grants the webhook server read access to APIExport content.
//
// In kcp, virtual workspace access is authorized by the apiexports/content
// subresource in the workspace where the APIExport is defined. The manager
// creates a ClusterRole granting get/list/watch on apiexports/content (scoped
// to specific APIExport names) and binds it to the webhook service account.
//
// Each DependencyRule's dependent resource references an APIExport in a provider
// workspace. As rules are added/removed, the manager reconciles RBAC in the
// affected provider workspaces.
type RBACManager struct {
	// BaseConfig is the root kcp REST config (no workspace path suffix).
	// Used to create per-workspace clients for RBAC management.
	BaseConfig *rest.Config

	// ServiceAccountName is the name of the webhook's service account.
	ServiceAccountName string

	// ServiceAccountNamespace is the namespace of the webhook's service account.
	ServiceAccountNamespace string

	mu          sync.RWMutex
	ruleExports map[string]ExportRef // rule key -> APIExport reference
}

// TrackRule records the APIExport reference for a given rule key.
func (m *RBACManager) TrackRule(key string, ref ExportRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ruleExports == nil {
		m.ruleExports = make(map[string]ExportRef)
	}
	m.ruleExports[key] = ref
}

// RemoveRule removes the tracked APIExport reference for a given rule key
// and returns the workspace path that was affected (empty if key was not tracked).
func (m *RBACManager) RemoveRule(key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref, ok := m.ruleExports[key]
	if !ok {
		return ""
	}
	delete(m.ruleExports, key)
	return ref.WorkspacePath
}

// Reconcile computes the desired RBAC state per workspace from tracked rules
// and creates, updates, or removes ClusterRoles and ClusterRoleBindings in
// each affected provider workspace. The extraWorkspaces parameter lists
// workspace paths that should also be reconciled (e.g., workspaces that just
// lost their last rule via RemoveRule).
func (m *RBACManager) Reconcile(ctx context.Context, extraWorkspaces ...string) error {
	desired := m.desiredStateByWorkspace()

	// Reconcile each workspace that currently has rules.
	for wsPath, exportNames := range desired {
		if err := m.reconcileWorkspace(ctx, wsPath, exportNames); err != nil {
			return fmt.Errorf("reconciling RBAC in %s: %w", wsPath, err)
		}
	}

	// Reconcile extra workspaces (typically those that just lost all rules).
	for _, wsPath := range extraWorkspaces {
		if _, ok := desired[wsPath]; ok {
			continue // already reconciled above
		}
		if err := m.reconcileWorkspace(ctx, wsPath, nil); err != nil {
			return fmt.Errorf("cleaning up RBAC in %s: %w", wsPath, err)
		}
	}

	return nil
}

// desiredStateByWorkspace groups tracked rules by workspace path and returns
// the set of APIExport names per workspace.
func (m *RBACManager) desiredStateByWorkspace() map[string]map[string]struct{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	byWorkspace := make(map[string]map[string]struct{})
	for _, ref := range m.ruleExports {
		if byWorkspace[ref.WorkspacePath] == nil {
			byWorkspace[ref.WorkspacePath] = make(map[string]struct{})
		}
		byWorkspace[ref.WorkspacePath][ref.ExportName] = struct{}{}
	}

	return byWorkspace
}

// buildPolicyRules constructs RBAC policy rules granting apiexports/content
// access for the given APIExport names.
func (m *RBACManager) buildPolicyRules(exportNames map[string]struct{}) []rbacv1.PolicyRule {
	if len(exportNames) == 0 {
		return nil
	}

	names := make([]string, 0, len(exportNames))
	for name := range exportNames {
		names = append(names, name)
	}

	return []rbacv1.PolicyRule{
		{
			APIGroups:     []string{"apis.kcp.io"},
			Resources:     []string{"apiexports/content"},
			ResourceNames: names,
			Verbs:         []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{"apis.kcp.io"},
			Resources: []string{"apiexportendpointslices"},
			Verbs:     []string{"get", "list", "watch"},
		},
	}
}

// reconcileWorkspace creates or updates the ClusterRole and ClusterRoleBinding
// in the given workspace.
func (m *RBACManager) reconcileWorkspace(ctx context.Context, wsPath string, exportNames map[string]struct{}) error {
	logger := log.FromContext(ctx).WithName("rbac-manager").WithValues("workspace", wsPath)

	client, err := m.clientForWorkspace(wsPath)
	if err != nil {
		return fmt.Errorf("creating client for %s: %w", wsPath, err)
	}

	desiredRules := m.buildPolicyRules(exportNames)

	if len(desiredRules) == 0 {
		return m.removeWorkspaceRBAC(ctx, wsPath)
	}

	// Reconcile ClusterRole.
	crClient := client.RbacV1().ClusterRoles()
	existing, err := crClient.Get(ctx, rbacClusterRoleName, metav1.GetOptions{})

	switch {
	case apierrors.IsNotFound(err):
		cr := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: rbacClusterRoleName,
			},
			Rules: desiredRules,
		}
		logger.Info("creating ClusterRole", "rules", len(desiredRules))
		if _, err := crClient.Create(ctx, cr, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating ClusterRole: %w", err)
		}
	case err != nil:
		return fmt.Errorf("getting ClusterRole: %w", err)
	default:
		existing.Rules = desiredRules
		logger.Info("updating ClusterRole", "rules", len(desiredRules))
		if _, err := crClient.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating ClusterRole: %w", err)
		}
	}

	// Ensure the ClusterRoleBinding exists.
	if err := m.ensureClusterRoleBinding(ctx, client, wsPath); err != nil {
		logger.Info("ClusterRoleBinding not found or cannot be created; ensure it was pre-created during deployment", "error", err)
	}

	return nil
}

// removeWorkspaceRBAC removes the ClusterRole and ClusterRoleBinding from
// a workspace. Non-fatal if they don't exist.
func (m *RBACManager) removeWorkspaceRBAC(ctx context.Context, wsPath string) error {
	client, err := m.clientForWorkspace(wsPath)
	if err != nil {
		return fmt.Errorf("creating client for %s: %w", wsPath, err)
	}

	crClient := client.RbacV1().ClusterRoles()
	if err := crClient.Delete(ctx, rbacClusterRoleName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting ClusterRole in %s: %w", wsPath, err)
	}

	crbClient := client.RbacV1().ClusterRoleBindings()
	if err := crbClient.Delete(ctx, rbacClusterRoleBindingName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting ClusterRoleBinding in %s: %w", wsPath, err)
	}

	return nil
}

// ensureClusterRoleBinding creates the ClusterRoleBinding if it doesn't exist.
func (m *RBACManager) ensureClusterRoleBinding(ctx context.Context, client kubernetes.Interface, wsPath string) error {
	crbClient := client.RbacV1().ClusterRoleBindings()

	_, err := crbClient.Get(ctx, rbacClusterRoleBindingName, metav1.GetOptions{})
	if err == nil {
		return nil // already exists
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting ClusterRoleBinding: %w", err)
	}

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: rbacClusterRoleBindingName,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     rbacClusterRoleName,
		},
		Subjects: []rbacv1.Subject{{
			APIGroup: rbacv1.GroupName,
			Kind:     rbacv1.UserKind,
			Name:     fmt.Sprintf("system:serviceaccount:%s:%s", m.ServiceAccountNamespace, m.ServiceAccountName),
		}},
	}

	logger := log.FromContext(ctx).WithName("rbac-manager")
	logger.Info("creating ClusterRoleBinding", "workspace", wsPath)
	if _, err := crbClient.Create(ctx, crb, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating ClusterRoleBinding: %w", err)
	}

	return nil
}

// clientForWorkspace creates a Kubernetes clientset targeting the given workspace.
func (m *RBACManager) clientForWorkspace(wsPath string) (kubernetes.Interface, error) {
	cfg := rest.CopyConfig(m.BaseConfig)
	cfg.Host += logicalcluster.NewPath(wsPath).RequestPath()

	return kubernetes.NewForConfig(cfg)
}
