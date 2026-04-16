// Copyright 2026 Open Defense and dependency-controller contributors
// SPDX-License-Identifier: Apache-2.0

package fieldpath

import "testing"

func TestResolve(t *testing.T) {
	obj := map[string]any{
		"spec": map[string]any{
			"vpcRef": map[string]any{
				"name": "my-vpc",
			},
			"count":  int64(3),
			"nested": "not-a-map",
		},
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "simple nested path", path: ".spec.vpcRef.name", want: "my-vpc"},
		{name: "without leading dot", path: "spec.vpcRef.name", want: "my-vpc"},
		{name: "missing field", path: ".spec.missing.name", want: ""},
		{name: "non-string leaf", path: ".spec.count", want: ""},
		{name: "intermediate not a map", path: ".spec.nested.deep", want: ""},
		{name: "top-level missing", path: ".nothing", want: ""},
		{name: "empty path", path: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(obj, tt.path)
			if got != tt.want {
				t.Errorf("Resolve(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestResolve_NilObj(t *testing.T) {
	got := Resolve(nil, ".spec.name")
	if got != "" {
		t.Errorf("Resolve on nil obj = %q, want empty", got)
	}
}
