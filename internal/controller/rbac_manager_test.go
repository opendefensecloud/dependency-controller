// Copyright 2026 Open Defense and dependency-controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sort"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestRBACManager() *RBACManager {
	return &RBACManager{
		Client:                  fake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
		ServiceAccountName:      "dep-ctrl",
		ServiceAccountNamespace: "dep-ctrl-system",
	}
}

func TestRBACManager_TrackAndRemove(t *testing.T) {
	mgr := newTestRBACManager()
	vpcGVR := schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "vpcs"}

	mgr.TrackRule("c1/rule1", vpcGVR)
	rules := mgr.buildPolicyRules()
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].APIGroups[0] != "net.io" {
		t.Errorf("group = %q, want %q", rules[0].APIGroups[0], "net.io")
	}

	mgr.RemoveRule("c1/rule1")
	if len(mgr.buildPolicyRules()) != 0 {
		t.Error("expected 0 rules after remove")
	}
}

func TestRBACManager_DeduplicatesSameGroup(t *testing.T) {
	mgr := newTestRBACManager()

	mgr.TrackRule("c1/rule1", schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "vpcs"})
	mgr.TrackRule("c1/rule2", schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "vpcs"})
	mgr.TrackRule("c1/rule3", schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "subnets"})

	rules := mgr.buildPolicyRules()
	if len(rules) != 1 {
		t.Fatalf("expected 1 policy rule (grouped by API group), got %d", len(rules))
	}

	resources := rules[0].Resources
	sort.Strings(resources)
	if len(resources) != 2 || resources[0] != "subnets" || resources[1] != "vpcs" {
		t.Errorf("resources = %v, want [subnets vpcs]", resources)
	}
}

func TestRBACManager_GroupsByAPIGroup(t *testing.T) {
	mgr := newTestRBACManager()

	mgr.TrackRule("c1/rule1", schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "vpcs"})
	mgr.TrackRule("c1/rule2", schema.GroupVersionResource{Group: "compute.io", Version: "v1", Resource: "vms"})

	rules := mgr.buildPolicyRules()
	if len(rules) != 2 {
		t.Errorf("expected 2 policy rules (one per group), got %d", len(rules))
	}
}

func TestRBACManager_OverwritesSameKey(t *testing.T) {
	mgr := newTestRBACManager()

	mgr.TrackRule("c1/rule1", schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "vpcs"})
	mgr.TrackRule("c1/rule1", schema.GroupVersionResource{Group: "compute.io", Version: "v1", Resource: "vms"})

	rules := mgr.buildPolicyRules()
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule after overwrite, got %d", len(rules))
	}
	if rules[0].APIGroups[0] != "compute.io" {
		t.Errorf("group = %q, want %q", rules[0].APIGroups[0], "compute.io")
	}
}

func TestRBACManager_RemoveNonexistent(t *testing.T) {
	mgr := newTestRBACManager()
	mgr.RemoveRule("nonexistent") // should not panic
}

func TestRBACManager_BuildPolicyRulesEmpty(t *testing.T) {
	mgr := &RBACManager{}
	if len(mgr.buildPolicyRules()) != 0 {
		t.Error("expected 0 rules for empty manager")
	}
}

func TestRBACManager_ReconcileCreatesRoleAndBinding(t *testing.T) {
	ctx := context.Background()
	mgr := newTestRBACManager()

	mgr.TrackRule("c1/rule1", schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "vpcs"})
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var cr rbacv1.ClusterRole
	if err := mgr.Client.Get(ctx, types.NamespacedName{Name: rbacClusterRoleName}, &cr); err != nil {
		t.Fatalf("getting ClusterRole: %v", err)
	}
	if len(cr.Rules) != 1 {
		t.Fatalf("expected 1 policy rule, got %d", len(cr.Rules))
	}
	if cr.Rules[0].Verbs[0] != "get" {
		t.Errorf("verbs = %v, want [get list watch]", cr.Rules[0].Verbs)
	}

	var crb rbacv1.ClusterRoleBinding
	if err := mgr.Client.Get(ctx, types.NamespacedName{Name: rbacClusterRoleBindingName}, &crb); err != nil {
		t.Fatalf("getting ClusterRoleBinding: %v", err)
	}
	if crb.RoleRef.Name != rbacClusterRoleName {
		t.Errorf("roleRef name = %q, want %q", crb.RoleRef.Name, rbacClusterRoleName)
	}
	if len(crb.Subjects) != 1 || crb.Subjects[0].Name != "dep-ctrl" {
		t.Errorf("subjects = %v, want [{dep-ctrl dep-ctrl-system}]", crb.Subjects)
	}
}

func TestRBACManager_ReconcileUpdatesRole(t *testing.T) {
	ctx := context.Background()
	mgr := newTestRBACManager()

	mgr.TrackRule("c1/rule1", schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "vpcs"})
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	mgr.TrackRule("c1/rule2", schema.GroupVersionResource{Group: "compute.io", Version: "v1", Resource: "vms"})
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	var cr rbacv1.ClusterRole
	if err := mgr.Client.Get(ctx, types.NamespacedName{Name: rbacClusterRoleName}, &cr); err != nil {
		t.Fatalf("getting ClusterRole: %v", err)
	}
	if len(cr.Rules) != 2 {
		t.Errorf("expected 2 policy rules after update, got %d", len(cr.Rules))
	}
}

func TestRBACManager_ReconcileRemovesRules(t *testing.T) {
	ctx := context.Background()
	mgr := newTestRBACManager()

	mgr.TrackRule("c1/rule1", schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "vpcs"})
	mgr.TrackRule("c1/rule2", schema.GroupVersionResource{Group: "compute.io", Version: "v1", Resource: "vms"})
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	mgr.RemoveRule("c1/rule1")
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	var cr rbacv1.ClusterRole
	if err := mgr.Client.Get(ctx, types.NamespacedName{Name: rbacClusterRoleName}, &cr); err != nil {
		t.Fatalf("getting ClusterRole: %v", err)
	}
	if len(cr.Rules) != 1 {
		t.Errorf("expected 1 policy rule after removal, got %d", len(cr.Rules))
	}
	if cr.Rules[0].APIGroups[0] != "compute.io" {
		t.Errorf("remaining group = %q, want %q", cr.Rules[0].APIGroups[0], "compute.io")
	}
}

func TestRBACManager_ReconcileEmptyRules(t *testing.T) {
	ctx := context.Background()
	mgr := newTestRBACManager()

	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var cr rbacv1.ClusterRole
	if err := mgr.Client.Get(ctx, types.NamespacedName{Name: rbacClusterRoleName}, &cr); err != nil {
		t.Fatalf("getting ClusterRole: %v", err)
	}
	if len(cr.Rules) != 0 {
		t.Errorf("expected 0 rules, got %d", len(cr.Rules))
	}
}

func TestRBACManager_ReconcileIdempotentBinding(t *testing.T) {
	ctx := context.Background()
	mgr := newTestRBACManager()

	mgr.TrackRule("c1/rule1", schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "vpcs"})
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	// Second reconcile should not error on existing binding.
	mgr.TrackRule("c1/rule2", schema.GroupVersionResource{Group: "compute.io", Version: "v1", Resource: "vms"})
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
}
