// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// crdPath is relative to this package's directory (where `go test` runs).
const crdPath = "../../config/crds/dependencies.opendefense.cloud_dependencyrules.yaml"

// TestCRDFieldValidation asserts that every constrained field on DependencyRule
// has the expected pattern / minLength / maxLength, and that the published
// pattern accepts the inputs we expect to be valid and rejects the ones we
// expect to be invalid. Loading the regex from the generated CRD (rather than
// from the Go source) means dropped or weakened markers are caught here, not
// silently at runtime.
func TestCRDFieldValidation(t *testing.T) {
	crd := loadCRD(t)

	// Each case targets a single field by its dot-path under
	// spec.versions[0].schema.openAPIV3Schema. "items" steps into an array's
	// element schema (used for spec.dependencies[]).
	cases := []struct {
		path      string
		pattern   string // must match exactly — change here if you intentionally tighten/loosen
		minLength int64
		maxLength int64
		valid     []string
		invalid   []string
	}{
		{
			path:      "spec.dependent.apiExportName",
			pattern:   `^[a-z]([-a-z0-9]*[a-z0-9])?(\.[a-z]([-a-z0-9]*[a-z0-9])?)*$`,
			minLength: 1,
			maxLength: 253,
			valid:     []string{"a", "compute.test.io", "dependencies.opendefense.cloud", "x-y", "x.y.z"},
			invalid:   []string{"", "Compute.test.io", "compute..test.io", "-foo", "foo-", "foo_bar", ".foo", "foo."},
		},
		{
			path:      "spec.dependent.group",
			pattern:   `^[a-z]([-a-z0-9]*[a-z0-9])?(\.[a-z]([-a-z0-9]*[a-z0-9])?)*$`,
			minLength: 1,
			maxLength: 253,
			valid:     []string{"compute.test.io", "network.example.com"},
			invalid:   []string{"", "Network.example.com", "network..example.com"},
		},
		{
			path:      "spec.dependent.version",
			pattern:   `^v[1-9][0-9]*([a-z]+[1-9][0-9]*)?$`,
			minLength: 1,
			maxLength: 63,
			valid:     []string{"v1", "v2", "v10", "v1alpha1", "v2beta3", "v123beta456"},
			invalid:   []string{"", "1", "V1", "v0", "v01", "v1.0", "v1alpha", "v1ALPHA1", "v1alpha0", "alpha1"},
		},
		{
			path:      "spec.dependent.kind",
			pattern:   `^[A-Z][A-Za-z0-9]*$`,
			minLength: 1,
			maxLength: 63,
			valid:     []string{"A", "VirtualMachine", "VPC", "Foo123"},
			invalid:   []string{"", "virtualMachine", "1Foo", "Foo-Bar", "Foo.Bar", "Foo_Bar"},
		},
		{
			path:      "spec.dependencies.items.apiExportRef.path",
			pattern:   `^[a-z][a-z0-9-]*(:[a-z][a-z0-9-]*)*$`,
			minLength: 1,
			maxLength: 253,
			valid:     []string{"root", "root:provider", "root:network-provider", "root:org-1:team-2"},
			invalid:   []string{"", "Root", "root:", ":root", "root::team", "root:.foo", "root:_foo", "root:Foo"},
		},
		{
			path:      "spec.dependencies.items.apiExportRef.name",
			pattern:   `^[a-z]([-a-z0-9]*[a-z0-9])?(\.[a-z]([-a-z0-9]*[a-z0-9])?)*$`,
			minLength: 1,
			maxLength: 253,
			valid:     []string{"a", "network.test.io", "compute.test.io"},
			invalid:   []string{"", "Network.test.io", "network..test.io"},
		},
		{
			path:      "spec.dependencies.items.group",
			pattern:   `^[a-z]([-a-z0-9]*[a-z0-9])?(\.[a-z]([-a-z0-9]*[a-z0-9])?)*$`,
			minLength: 1,
			maxLength: 253,
			valid:     []string{"network.test.io"},
			invalid:   []string{"", "Network.test.io"},
		},
		{
			path:      "spec.dependencies.items.version",
			pattern:   `^v[1-9][0-9]*([a-z]+[1-9][0-9]*)?$`,
			minLength: 1,
			maxLength: 63,
			valid:     []string{"v1", "v1alpha1"},
			invalid:   []string{"", "v0", "V1", "v1alpha"},
		},
		{
			path:      "spec.dependencies.items.resource",
			pattern:   `^[a-z][a-z0-9]*$`,
			minLength: 1,
			maxLength: 63,
			valid:     []string{"vpcs", "subnets"},
			invalid:   []string{"", "VPCs", "vpc-s"},
		},
		{
			path:      "spec.dependencies.items.fieldRef.path",
			pattern:   `^\.?[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)+$`,
			minLength: 2,
			maxLength: 253,
			valid: []string{
				".spec.vpcRef.name",
				".spec.subnetRef.name",
				"spec.foo.bar",
				".metadata.name",
				".a.b",
				"_under.score",
			},
			invalid: []string{
				"",
				".",
				".spec",             // single segment after optional dot
				".spec.",            // trailing dot
				"..foo.bar",         // empty first segment
				".spec..foo",        // empty middle segment
				".1foo.bar",         // segment starts with digit
				".spec[0].name",     // array indexing — fieldpath.Resolve doesn't support it
				".spec.*.name",      // wildcard — not supported
				".spec.foo-bar.baz", // hyphen — not a Go-ish identifier
				".spec.foo bar.baz", // whitespace
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			schema := schemaAt(t, crd, tc.path)

			if schema.MinLength == nil {
				t.Fatalf("no minLength on %s", tc.path)
			}
			if *schema.MinLength != tc.minLength {
				t.Errorf("minLength on %s = %d, want %d", tc.path, *schema.MinLength, tc.minLength)
			}
			if schema.MaxLength == nil {
				t.Fatalf("no maxLength on %s", tc.path)
			}
			if *schema.MaxLength != tc.maxLength {
				t.Errorf("maxLength on %s = %d, want %d", tc.path, *schema.MaxLength, tc.maxLength)
			}
			if schema.Pattern != tc.pattern {
				t.Errorf("pattern on %s =\n  %s\nwant:\n  %s", tc.path, schema.Pattern, tc.pattern)
			}

			re := regexp.MustCompile(schema.Pattern)

			for _, v := range tc.valid {
				if int64(len(v)) < *schema.MinLength {
					t.Errorf("test bug: valid input %q is shorter than minLength on %s", v, tc.path)
				}
				if int64(len(v)) > *schema.MaxLength {
					t.Errorf("test bug: valid input %q is longer than maxLength on %s", v, tc.path)
				}
				if !re.MatchString(v) {
					t.Errorf("valid input %q rejected by pattern %s on %s", v, schema.Pattern, tc.path)
				}
			}

			for _, v := range tc.invalid {
				tooShort := int64(len(v)) < *schema.MinLength
				tooLong := int64(len(v)) > *schema.MaxLength
				patternMismatch := !re.MatchString(v)
				if !(tooShort || tooLong || patternMismatch) {
					t.Errorf("invalid input %q passed all checks on %s (pattern=%s minLength=%d maxLength=%d)",
						v, tc.path, schema.Pattern, *schema.MinLength, *schema.MaxLength)
				}
			}
		})
	}
}

func loadCRD(t *testing.T) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("reading CRD at %s — run `make manifests` if missing: %v", crdPath, err)
	}

	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(raw, crd); err != nil {
		t.Fatalf("unmarshaling CRD: %v", err)
	}

	return crd
}

// schemaAt navigates spec.versions[0].schema.openAPIV3Schema by dot-path.
// "items" steps into an array's element schema.
func schemaAt(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, path string) *apiextensionsv1.JSONSchemaProps {
	t.Helper()
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("CRD has no versions")
	}
	if crd.Spec.Versions[0].Schema == nil || crd.Spec.Versions[0].Schema.OpenAPIV3Schema == nil {
		t.Fatal("CRD version 0 has no openAPIV3Schema")
	}

	parts := strings.Split(path, ".")
	cur := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	for i, part := range parts {
		prefix := strings.Join(parts[:i+1], ".")
		if part == "items" {
			if cur.Items == nil || cur.Items.Schema == nil {
				t.Fatalf("no items schema at %s", prefix)
			}
			cur = cur.Items.Schema

			continue
		}
		next, ok := cur.Properties[part]
		if !ok {
			t.Fatalf("no property %q at %s", part, prefix)
		}
		cur = &next
	}

	return cur
}
