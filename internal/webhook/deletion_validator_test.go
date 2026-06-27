// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
