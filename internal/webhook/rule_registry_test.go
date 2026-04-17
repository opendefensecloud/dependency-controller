// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"fmt"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
)

func TestRuleRegistry_RegisterAndFind(t *testing.T) {
	r := NewRuleRegistry()

	vpcGVR := schema.GroupVersionResource{Group: "network.test.io", Version: "v1", Resource: "vpcs"}
	subnetGVR := schema.GroupVersionResource{Group: "network.test.io", Version: "v1", Resource: "subnets"}

	state := &RuleState{
		Cancel: func() {},
		Rule: v1alpha1.DependencyRuleSpec{
			Dependent: v1alpha1.DependentRef{
				Group:    "compute.test.io",
				Version:  "v1",
				Kind:     "VirtualMachine",
				Resource: "virtualmachines",
			},
		},
		DependentGVK: schema.GroupVersionKind{Group: "compute.test.io", Version: "v1", Kind: "VirtualMachine"},
		IndexFields: []IndexedField{
			{FieldPath: ".spec.vpcRef.name", TargetGVR: vpcGVR},
			{FieldPath: ".spec.subnetRef.name", TargetGVR: subnetGVR},
		},
	}

	r.Register("cluster1/vm-deps", state)

	// Find by VPC GVR.
	entries := r.FindByTargetGVR(vpcGVR)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry for vpcs, got %d", len(entries))
	}
	if entries[0].MatchedField.FieldPath != ".spec.vpcRef.name" {
		t.Errorf("field path = %q, want %q", entries[0].MatchedField.FieldPath, ".spec.vpcRef.name")
	}

	// Find by Subnet GVR.
	entries = r.FindByTargetGVR(subnetGVR)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry for subnets, got %d", len(entries))
	}

	// Find by unknown GVR.
	entries = r.FindByTargetGVR(schema.GroupVersionResource{Group: "other", Version: "v1", Resource: "things"})
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for unknown GVR, got %d", len(entries))
	}
}

func TestRuleRegistry_Unregister(t *testing.T) {
	r := NewRuleRegistry()

	vpcGVR := schema.GroupVersionResource{Group: "network.test.io", Version: "v1", Resource: "vpcs"}
	cancelled := false

	state := &RuleState{
		Cancel: func() { cancelled = true },
		IndexFields: []IndexedField{
			{FieldPath: ".spec.vpcRef.name", TargetGVR: vpcGVR},
		},
	}

	r.Register("cluster1/vm-deps", state)
	r.Unregister("cluster1/vm-deps")

	if !cancelled {
		t.Error("expected cancel to be called")
	}

	entries := r.FindByTargetGVR(vpcGVR)
	if len(entries) != 0 {
		t.Errorf("expected 0 entries after unregister, got %d", len(entries))
	}

	if r.Exists("cluster1/vm-deps") {
		t.Error("expected rule to not exist after unregister")
	}
}

func TestRuleRegistry_MultipleRulesSameTarget(t *testing.T) {
	r := NewRuleRegistry()

	vpcGVR := schema.GroupVersionResource{Group: "network.test.io", Version: "v1", Resource: "vpcs"}

	state1 := &RuleState{
		Cancel:      func() {},
		IndexFields: []IndexedField{{FieldPath: ".spec.vpcRef.name", TargetGVR: vpcGVR}},
	}
	state2 := &RuleState{
		Cancel:      func() {},
		IndexFields: []IndexedField{{FieldPath: ".spec.networkRef.vpc", TargetGVR: vpcGVR}},
	}

	r.Register("cluster1/vm-deps", state1)
	r.Register("cluster1/db-deps", state2)

	entries := r.FindByTargetGVR(vpcGVR)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Unregister one, the other should remain.
	r.Unregister("cluster1/vm-deps")
	entries = r.FindByTargetGVR(vpcGVR)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after partial unregister, got %d", len(entries))
	}
	if entries[0].Key != "cluster1/db-deps" {
		t.Errorf("remaining key = %q, want %q", entries[0].Key, "cluster1/db-deps")
	}
}

func TestRuleRegistry_TrackCluster(t *testing.T) {
	r := NewRuleRegistry()

	state := &RuleState{
		Cancel: func() {},
	}
	r.Register("cluster1/rule", state)

	r.TrackCluster("cluster1/rule", "consumer-ws-1")
	r.TrackCluster("cluster1/rule", "consumer-ws-2")
	r.TrackCluster("cluster1/rule", "consumer-ws-1") // duplicate

	state.mu.Lock()
	count := len(state.knownClusters)
	state.mu.Unlock()

	if count != 2 {
		t.Errorf("expected 2 known clusters, got %d", count)
	}
}

func TestRuleRegistry_AllDependentGVRs(t *testing.T) {
	r := NewRuleRegistry()

	r.Register("c1/rule1", &RuleState{
		Cancel: func() {},
		Rule: v1alpha1.DependencyRuleSpec{
			Dependent: v1alpha1.DependentRef{Group: "compute.io", Version: "v1", Resource: "vms"},
		},
	})
	r.Register("c1/rule2", &RuleState{
		Cancel: func() {},
		Rule: v1alpha1.DependencyRuleSpec{
			Dependent: v1alpha1.DependentRef{Group: "compute.io", Version: "v1", Resource: "vms"},
		},
	})
	r.Register("c1/rule3", &RuleState{
		Cancel: func() {},
		Rule: v1alpha1.DependencyRuleSpec{
			Dependent: v1alpha1.DependentRef{Group: "db.io", Version: "v1", Resource: "databases"},
		},
	})

	gvrs := r.AllDependentGVRs()
	if len(gvrs) != 2 {
		t.Errorf("expected 2 deduplicated GVRs, got %d", len(gvrs))
	}
}

func TestRuleRegistry_MarkReady(t *testing.T) {
	r := NewRuleRegistry()

	state := &RuleState{Cancel: func() {}}
	r.Register("c1/rule", state)

	if state.IsReady() {
		t.Error("expected Ready=false initially")
	}

	r.MarkReady("c1/rule")

	if !state.IsReady() {
		t.Error("expected Ready=true after MarkReady")
	}
}

func TestRuleRegistry_UnregisterNonexistent(t *testing.T) {
	r := NewRuleRegistry()
	// Should not panic.
	r.Unregister("nonexistent")
}

func TestRuleRegistry_AllTargetGVRs(t *testing.T) {
	r := NewRuleRegistry()

	vpcGVR := schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "vpcs"}
	subnetGVR := schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "subnets"}

	r.Register("c1/rule1", &RuleState{
		Cancel:      func() {},
		IndexFields: []IndexedField{{FieldPath: ".spec.vpcRef.name", TargetGVR: vpcGVR}},
	})
	r.Register("c1/rule2", &RuleState{
		Cancel: func() {},
		IndexFields: []IndexedField{
			{FieldPath: ".spec.vpcRef.name", TargetGVR: vpcGVR},
			{FieldPath: ".spec.subnetRef.name", TargetGVR: subnetGVR},
		},
	})

	gvrs := r.AllTargetGVRs()
	if len(gvrs) != 2 {
		t.Errorf("expected 2 target GVRs, got %d", len(gvrs))
	}
}

func TestRuleRegistry_GetNil(t *testing.T) {
	r := NewRuleRegistry()
	if r.Get("nonexistent") != nil {
		t.Error("expected nil for nonexistent key")
	}
}

func TestRuleRegistry_MarkReadyNonexistent(t *testing.T) {
	r := NewRuleRegistry()
	// Should not panic.
	r.MarkReady("nonexistent")
}

func TestRuleRegistry_TrackClusterNonexistent(t *testing.T) {
	r := NewRuleRegistry()
	// Should not panic.
	r.TrackCluster("nonexistent", "cluster1")
}

func TestRuleRegistry_ConcurrentAccess(t *testing.T) {
	r := NewRuleRegistry()
	vpcGVR := schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "vpcs"}

	const goroutines = 20
	const opsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("cluster%d/rule%d", id%5, id)

			for j := range opsPerGoroutine {
				switch j % 7 {
				case 0:
					r.Register(key, &RuleState{
						Cancel:      func() {},
						IndexFields: []IndexedField{{FieldPath: ".spec.ref", TargetGVR: vpcGVR}},
						Rule: v1alpha1.DependencyRuleSpec{
							Dependent: v1alpha1.DependentRef{Group: "compute.io", Version: "v1", Resource: "vms"},
						},
					})
				case 1:
					r.FindByTargetGVR(vpcGVR)
				case 2:
					r.Exists(key)
				case 3:
					r.Get(key)
				case 4:
					r.TrackCluster(key, fmt.Sprintf("consumer-%d", j))
				case 5:
					r.MarkReady(key)
				case 6:
					r.AllTargetGVRs()
				}
			}
		}(i)
	}

	wg.Wait()

	// Final cleanup: unregister all concurrently.
	wg.Add(goroutines)
	for i := range goroutines {
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("cluster%d/rule%d", id%5, id)
			r.Unregister(key)
		}(i)
	}

	wg.Wait()
}

func TestRuleRegistry_RegisterOverwrite(t *testing.T) {
	r := NewRuleRegistry()

	vpcGVR := schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "vpcs"}
	subnetGVR := schema.GroupVersionResource{Group: "net.io", Version: "v1", Resource: "subnets"}

	oldState := &RuleState{
		Cancel:      func() {},
		IndexFields: []IndexedField{{FieldPath: ".spec.vpcRef.name", TargetGVR: vpcGVR}},
	}
	old := r.Register("c1/rule", oldState)
	if old != nil {
		t.Error("expected nil old state on first register")
	}

	// Overwrite with new state targeting a different GVR.
	old = r.Register("c1/rule", &RuleState{
		Cancel:      func() {},
		IndexFields: []IndexedField{{FieldPath: ".spec.subnetRef.name", TargetGVR: subnetGVR}},
	})
	if old != oldState {
		t.Error("expected old state to be returned on overwrite")
	}

	// Old target should no longer be in the index.
	entries := r.FindByTargetGVR(vpcGVR)
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for old GVR after overwrite, got %d", len(entries))
	}

	// New target should be in the index.
	entries = r.FindByTargetGVR(subnetGVR)
	if len(entries) != 1 {
		t.Errorf("expected 1 entry for new GVR after overwrite, got %d", len(entries))
	}
}

func TestRuleRegistry_RegisterFirstReturnsNil(t *testing.T) {
	r := NewRuleRegistry()
	old := r.Register("c1/rule", &RuleState{Cancel: func() {}})
	if old != nil {
		t.Error("expected nil when registering a new key")
	}
}
