// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func makeRequest(annotations map[string]string) admission.Request {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "VPC",
		"metadata": map[string]any{
			"name":        "my-vpc",
			"annotations": annotations,
		},
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			OldObject: runtime.RawExtension{Raw: raw},
		},
	}
}

func TestObjectFromRequest_Valid(t *testing.T) {
	req := makeRequest(map[string]string{"foo": "bar"})
	obj, err := objectFromRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if obj.GetName() != "my-vpc" {
		t.Errorf("name = %q, want %q", obj.GetName(), "my-vpc")
	}
}

func TestObjectFromRequest_EmptyRaw(t *testing.T) {
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{},
	}
	_, err := objectFromRequest(req)
	if err == nil {
		t.Error("expected error for empty raw, got nil")
	}
}

func TestObjectFromRequest_FallsBackToObject(t *testing.T) {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "VPC",
		"metadata": map[string]any{
			"name": "fallback-vpc",
		},
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Object: runtime.RawExtension{Raw: raw},
		},
	}
	result, err := objectFromRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.GetName() != "fallback-vpc" {
		t.Errorf("name = %q, want %q", result.GetName(), "fallback-vpc")
	}
}

func TestObjectFromRequest_InvalidJSON(t *testing.T) {
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			OldObject: runtime.RawExtension{Raw: []byte("{invalid")},
		},
	}
	_, err := objectFromRequest(req)
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestSkipProtection_Present(t *testing.T) {
	req := makeRequest(map[string]string{
		AnnotationSkipProtection: "true",
	})
	obj, _ := objectFromRequest(req)
	if obj.GetAnnotations()[AnnotationSkipProtection] != "true" {
		t.Error("expected skip-protection annotation to be true")
	}
}

func TestSkipProtection_Absent(t *testing.T) {
	req := makeRequest(map[string]string{})
	obj, _ := objectFromRequest(req)
	if obj.GetAnnotations()[AnnotationSkipProtection] == "true" {
		t.Error("expected skip-protection annotation to be absent")
	}
}

func TestSkipProtection_WrongValue(t *testing.T) {
	req := makeRequest(map[string]string{
		AnnotationSkipProtection: "false",
	})
	obj, _ := objectFromRequest(req)
	if obj.GetAnnotations()[AnnotationSkipProtection] == "true" {
		t.Error("expected skip-protection to not match with value 'false'")
	}
}

func vpcPeering(namespace, name, targetVPC string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "network.test.io/v1",
		"kind":       "VPCPeering",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       map[string]any{"targetVpcRef": map[string]any{"name": targetVPC}},
	}}
}

func TestListDependents_NamespaceQualification(t *testing.T) {
	vpcGVR := schema.GroupVersionResource{Group: "network.test.io", Version: "v1", Resource: "vpcs"}
	vpcPeeringGVR := schema.GroupVersionResource{Group: "network.test.io", Version: "v1", Resource: "vpcpeerings"}

	entry := RuleEntry{
		State: &RuleState{
			DependentGVR: vpcPeeringGVR,
			DependentGVK: schema.GroupVersionKind{Group: "network.test.io", Version: "v1", Kind: "VPCPeering"},
		},
		MatchedField: IndexedField{FieldPath: ".spec.targetVpcRef.name", TargetGVR: vpcGVR},
	}

	newClient := func(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
		return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(),
			map[schema.GroupVersionResource]string{vpcPeeringGVR: "VPCPeeringList"},
			objs...,
		)
	}

	v := &DeletionValidator{}

	t.Run("namespaced target renders clean Kind/name", func(t *testing.T) {
		// Namespaced target: dependents are listed within the target's namespace
		// only, so names are unique and the namespace segment is redundant.
		dyn := newClient(vpcPeering("team-a", "peer1", "my-vpc"))

		got, err := v.listDependents(context.Background(), dyn, entry, "my-vpc", "team-a")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"VPCPeering/peer1"}
		assertBlockers(t, got, want)
	})

	t.Run("cluster-scoped target keeps same-named dependents distinct", func(t *testing.T) {
		// Cluster-scoped target (namespace ""): dependents are listed across all
		// namespaces, so two same-named objects must not be conflated.
		dyn := newClient(
			vpcPeering("team-a", "peer1", "global-vpc"),
			vpcPeering("team-b", "peer1", "global-vpc"),
		)

		got, err := v.listDependents(context.Background(), dyn, entry, "global-vpc", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"VPCPeering/team-a/peer1", "VPCPeering/team-b/peer1"}
		assertBlockers(t, got, want)
	})
}

func assertBlockers(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blockers = %v, want %v", got, want)
	}
}

func TestBlockerID(t *testing.T) {
	tests := []struct {
		name      string
		kind      string
		namespace string
		objName   string
		want      string
	}{
		{
			name:    "cluster-scoped dependent has no namespace segment",
			kind:    "VPCPeering",
			objName: "peer1",
			want:    "VPCPeering/peer1",
		},
		{
			name:      "namespaced dependent includes its namespace",
			kind:      "Order",
			namespace: "team-a",
			objName:   "order1",
			want:      "Order/team-a/order1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := blockerID(tt.kind, tt.namespace, tt.objName); got != tt.want {
				t.Errorf("blockerID(%q, %q, %q) = %q, want %q", tt.kind, tt.namespace, tt.objName, got, tt.want)
			}
		})
	}

	// Same name in different namespaces must not collapse to one identity,
	// which would hide a genuine blocker for cluster-scoped deletions.
	if a, b := blockerID("Order", "team-a", "order1"), blockerID("Order", "team-b", "order1"); a == b {
		t.Errorf("distinct dependents collided: both rendered %q", a)
	}
}

func TestDedupeBlockers(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "nil",
			in:   nil,
			want: nil,
		},
		{
			name: "no duplicates preserves order",
			in:   []string{"VPCPeering/peer1", "VPCPeering/peer2"},
			want: []string{"VPCPeering/peer1", "VPCPeering/peer2"},
		},
		{
			// A single dependent referencing the target via multiple fields
			// must be listed once.
			name: "same dependent collapsed to one",
			in:   []string{"VPCPeering/peer1", "VPCPeering/peer1", "VPCPeering/peer1"},
			want: []string{"VPCPeering/peer1"},
		},
		{
			name: "keeps distinct dependents, first-seen order",
			in:   []string{"VPCPeering/peer1", "VPCPeering/peer2", "VPCPeering/peer1"},
			want: []string{"VPCPeering/peer1", "VPCPeering/peer2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedupeBlockers(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("dedupeBlockers(%v) = %v, want %v", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func FuzzObjectFromRequest(f *testing.F) {
	for _, seed := range [][]byte{
		nil,
		[]byte(``),
		[]byte(`{}`),
		[]byte(`null`),
		[]byte(`{"metadata":{"name":"foo"}}`),
		[]byte(`{"metadata":{"annotations":{"dependencies.opendefense.cloud/skip-protection":"true"}}}`),
		[]byte(`{"metadata":{"annotations":{"kcp.io/cluster":"root:org:workspace"}}}`),
		[]byte(`{"metadata":null}`),
		[]byte(`{"metadata":{"annotations":null}}`),
		[]byte(`[]`),
		[]byte(`"string-not-object"`),
		[]byte("{\"metadata\":{\"name\":\"\xff\xfe\"}}"),
		{0x00, 0x01, 0x02},
	} {
		f.Add(seed, true)
		f.Add(seed, false)
	}

	f.Fuzz(func(_ *testing.T, raw []byte, useOld bool) {
		req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{}}
		if useOld {
			req.OldObject = runtime.RawExtension{Raw: raw}
		} else {
			req.Object = runtime.RawExtension{Raw: raw}
		}

		obj, err := objectFromRequest(req)
		if err != nil {
			return
		}
		_ = obj.GetAnnotations()[AnnotationSkipProtection]
		_ = obj.GetName()
		_ = obj.GetNamespace()
	})
}
