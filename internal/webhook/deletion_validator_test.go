package webhook

import (
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func makeRequest(annotations map[string]string) admission.Request {
	obj := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "VPC",
		"metadata": map[string]interface{}{
			"name":        "my-vpc",
			"annotations": annotations,
		},
	}
	raw, _ := json.Marshal(obj)
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
	obj := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "VPC",
		"metadata": map[string]interface{}{
			"name": "fallback-vpc",
		},
	}
	raw, _ := json.Marshal(obj)
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
