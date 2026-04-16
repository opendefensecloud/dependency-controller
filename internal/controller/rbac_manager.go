// Copyright 2026 Open Defense and dependency-controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sync"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	rbacClusterRoleName        = "dependency-controller"
	rbacClusterRoleBindingName = "dependency-controller"
)

// RBACManager maintains a ClusterRole and ClusterRoleBinding in the system:master
// workspace that grants the dependency-controller read access to all dependent
// resource types declared by active DependencyRules.
type RBACManager struct {
	// Client targets the system:master workspace.
	Client client.Client

	// ServiceAccountName is the name of the controller's service account.
	ServiceAccountName string

	// ServiceAccountNamespace is the namespace of the controller's service account.
	ServiceAccountNamespace string

	mu       sync.RWMutex
	ruleGVRs map[string]schema.GroupVersionResource // rule key -> dependent GVR
}

// TrackRule records the dependent GVR for a given rule key.
func (m *RBACManager) TrackRule(key string, gvr schema.GroupVersionResource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ruleGVRs == nil {
		m.ruleGVRs = make(map[string]schema.GroupVersionResource)
	}
	m.ruleGVRs[key] = gvr
}

// RemoveRule removes the tracked GVR for a given rule key.
func (m *RBACManager) RemoveRule(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.ruleGVRs, key)
}

// Reconcile builds the desired ClusterRole from the tracked rules
// and creates or updates it in the system:master workspace.
func (m *RBACManager) Reconcile(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("rbac-manager")

	desiredRules := m.buildPolicyRules()

	// Reconcile ClusterRole.
	existing := &rbacv1.ClusterRole{}
	err := m.Client.Get(ctx, types.NamespacedName{Name: rbacClusterRoleName}, existing)

	switch {
	case apierrors.IsNotFound(err):
		cr := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: rbacClusterRoleName,
			},
			Rules: desiredRules,
		}
		logger.Info("creating ClusterRole in system:master", "rules", len(desiredRules))
		if err := m.Client.Create(ctx, cr); err != nil {
			return fmt.Errorf("creating ClusterRole: %w", err)
		}
	case err != nil:
		return fmt.Errorf("getting ClusterRole: %w", err)
	default:
		existing.Rules = desiredRules
		logger.Info("updating ClusterRole in system:master", "rules", len(desiredRules))
		if err := m.Client.Update(ctx, existing); err != nil {
			return fmt.Errorf("updating ClusterRole: %w", err)
		}
	}

	// Reconcile ClusterRoleBinding (idempotent).
	if err := m.ensureClusterRoleBinding(ctx); err != nil {
		return err
	}

	return nil
}

// buildPolicyRules constructs RBAC policy rules from all tracked dependency
// rules. Each unique API group/resource pair gets a rule with get/list/watch verbs.
func (m *RBACManager) buildPolicyRules() []rbacv1.PolicyRule {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Group resources by API group for compact rules.
	byGroup := make(map[string]map[string]struct{})
	for _, gvr := range m.ruleGVRs {
		if byGroup[gvr.Group] == nil {
			byGroup[gvr.Group] = make(map[string]struct{})
		}
		byGroup[gvr.Group][gvr.Resource] = struct{}{}
	}

	rules := make([]rbacv1.PolicyRule, 0, len(byGroup))
	for group, resources := range byGroup {
		resourceList := make([]string, 0, len(resources))
		for r := range resources {
			resourceList = append(resourceList, r)
		}
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{group},
			Resources: resourceList,
			Verbs:     []string{"get", "list", "watch"},
		})
	}

	return rules
}

// ensureClusterRoleBinding creates the ClusterRoleBinding if it doesn't exist.
func (m *RBACManager) ensureClusterRoleBinding(ctx context.Context) error {
	existing := &rbacv1.ClusterRoleBinding{}
	err := m.Client.Get(ctx, types.NamespacedName{Name: rbacClusterRoleBindingName}, existing)
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
			Kind:      rbacv1.ServiceAccountKind,
			Name:      m.ServiceAccountName,
			Namespace: m.ServiceAccountNamespace,
		}},
	}

	logger := log.FromContext(ctx).WithName("rbac-manager")
	logger.Info("creating ClusterRoleBinding in system:master")
	if err := m.Client.Create(ctx, crb); err != nil {
		return fmt.Errorf("creating ClusterRoleBinding: %w", err)
	}

	return nil
}
