// Copyright 2026 Open Defense and dependency-controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"sort"
	"testing"
)

func TestRBACManager_TrackAndRemove(t *testing.T) {
	mgr := &RBACManager{}
	ref := ExportRef{WorkspacePath: "root:compute-provider", ExportName: "compute.test.io"}

	mgr.TrackRule("c1/rule1", ref)

	state := mgr.desiredStateByWorkspace()
	if len(state) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(state))
	}
	exports := state["root:compute-provider"]
	if _, ok := exports["compute.test.io"]; !ok {
		t.Error("expected compute.test.io in workspace exports")
	}

	mgr.RemoveRule("c1/rule1")
	state = mgr.desiredStateByWorkspace()
	if len(state) != 0 {
		t.Error("expected 0 workspaces after remove")
	}
}

func TestRBACManager_DeduplicatesSameExport(t *testing.T) {
	mgr := &RBACManager{}

	// Two rules referencing the same APIExport.
	mgr.TrackRule("c1/rule1", ExportRef{WorkspacePath: "root:compute", ExportName: "compute.io"})
	mgr.TrackRule("c1/rule2", ExportRef{WorkspacePath: "root:compute", ExportName: "compute.io"})

	state := mgr.desiredStateByWorkspace()
	if len(state) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(state))
	}
	if len(state["root:compute"]) != 1 {
		t.Errorf("expected 1 export, got %d", len(state["root:compute"]))
	}
}

func TestRBACManager_GroupsByWorkspace(t *testing.T) {
	mgr := &RBACManager{}

	mgr.TrackRule("c1/rule1", ExportRef{WorkspacePath: "root:compute", ExportName: "compute.io"})
	mgr.TrackRule("c1/rule2", ExportRef{WorkspacePath: "root:network", ExportName: "network.io"})

	state := mgr.desiredStateByWorkspace()
	if len(state) != 2 {
		t.Errorf("expected 2 workspaces, got %d", len(state))
	}
}

func TestRBACManager_MultipleExportsInSameWorkspace(t *testing.T) {
	mgr := &RBACManager{}

	mgr.TrackRule("c1/rule1", ExportRef{WorkspacePath: "root:provider", ExportName: "compute.io"})
	mgr.TrackRule("c1/rule2", ExportRef{WorkspacePath: "root:provider", ExportName: "network.io"})

	state := mgr.desiredStateByWorkspace()
	if len(state) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(state))
	}
	exports := state["root:provider"]
	if len(exports) != 2 {
		t.Errorf("expected 2 exports, got %d", len(exports))
	}
}

func TestRBACManager_OverwritesSameKey(t *testing.T) {
	mgr := &RBACManager{}

	mgr.TrackRule("c1/rule1", ExportRef{WorkspacePath: "root:compute", ExportName: "compute.io"})
	mgr.TrackRule("c1/rule1", ExportRef{WorkspacePath: "root:network", ExportName: "network.io"})

	state := mgr.desiredStateByWorkspace()
	if len(state) != 1 {
		t.Fatalf("expected 1 workspace after overwrite, got %d", len(state))
	}
	if _, ok := state["root:network"]; !ok {
		t.Error("expected root:network workspace after overwrite")
	}
}

func TestRBACManager_RemoveNonexistent(t *testing.T) {
	mgr := &RBACManager{}
	mgr.RemoveRule("nonexistent") // should not panic
}

func TestRBACManager_BuildPolicyRulesEmpty(t *testing.T) {
	mgr := &RBACManager{}
	rules := mgr.buildPolicyRules(nil)
	if len(rules) != 0 {
		t.Errorf("expected 0 rules for empty exports, got %d", len(rules))
	}
}

func TestRBACManager_BuildPolicyRules(t *testing.T) {
	mgr := &RBACManager{}
	exports := map[string]struct{}{
		"compute.io": {},
		"network.io": {},
	}
	rules := mgr.buildPolicyRules(exports)

	// Expect 2 rules: apiexports/content + apiexportendpointslices.
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}

	// First rule: apiexports/content with resource names.
	contentRule := rules[0]
	if contentRule.APIGroups[0] != "apis.kcp.io" {
		t.Errorf("group = %q, want apis.kcp.io", contentRule.APIGroups[0])
	}
	if contentRule.Resources[0] != "apiexports/content" {
		t.Errorf("resource = %q, want apiexports/content", contentRule.Resources[0])
	}
	sort.Strings(contentRule.ResourceNames)
	if len(contentRule.ResourceNames) != 2 || contentRule.ResourceNames[0] != "compute.io" || contentRule.ResourceNames[1] != "network.io" {
		t.Errorf("resourceNames = %v, want [compute.io network.io]", contentRule.ResourceNames)
	}

	// Verify read-only verbs.
	allowed := map[string]bool{"get": true, "list": true, "watch": true}
	for _, verb := range contentRule.Verbs {
		if !allowed[verb] {
			t.Errorf("unexpected verb %q; only get/list/watch allowed", verb)
		}
	}

	// Second rule: apiexportendpointslices.
	essRule := rules[1]
	if essRule.Resources[0] != "apiexportendpointslices" {
		t.Errorf("resource = %q, want apiexportendpointslices", essRule.Resources[0])
	}
}

func TestRBACManager_BuildPolicyRulesSingleExport(t *testing.T) {
	mgr := &RBACManager{}
	exports := map[string]struct{}{"compute.io": {}}
	rules := mgr.buildPolicyRules(exports)

	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
	if len(rules[0].ResourceNames) != 1 || rules[0].ResourceNames[0] != "compute.io" {
		t.Errorf("resourceNames = %v, want [compute.io]", rules[0].ResourceNames)
	}
}
