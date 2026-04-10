package controller

import (
	"strings"
	"testing"
)

func TestDependencyName_Basic(t *testing.T) {
	name := dependencyName("cluster1", "rule1", "my-vm", "vpcs", "my-vpc")
	want := "cluster1.rule1--my-vm--vpcs.my-vpc"
	if name != want {
		t.Errorf("dependencyName() = %q, want %q", name, want)
	}
}

func TestDependencyName_SlashReplacement(t *testing.T) {
	name := dependencyName("cluster1", "rule/with/slash", "vm", "vpcs", "vpc")
	if strings.Contains(name, "/") {
		t.Errorf("dependencyName() contains slash: %q", name)
	}
}

func TestDependencyName_Truncation(t *testing.T) {
	long := strings.Repeat("a", 200)
	name := dependencyName(long, long, long, "vpcs", "vpc")

	if len(name) > 253 {
		t.Errorf("dependencyName() length = %d, want <= 253", len(name))
	}

	// Verify the hash suffix is present (16 hex chars after a dash).
	parts := strings.Split(name, "-")
	suffix := parts[len(parts)-1]
	if len(suffix) != 16 {
		t.Errorf("expected 16-char hash suffix, got %q (len %d)", suffix, len(suffix))
	}
}

func TestDependencyName_TruncationDeterministic(t *testing.T) {
	long := strings.Repeat("x", 200)
	name1 := dependencyName(long, "rule1", "vm1", "vpcs", "vpc1")
	name2 := dependencyName(long, "rule1", "vm1", "vpcs", "vpc1")
	if name1 != name2 {
		t.Errorf("dependencyName() not deterministic: %q != %q", name1, name2)
	}
}

func TestDependencyName_TruncationUniqueness(t *testing.T) {
	long := strings.Repeat("x", 200)
	name1 := dependencyName(long, "rule1", "vm1", "vpcs", "vpc1")
	name2 := dependencyName(long, "rule1", "vm1", "vpcs", "vpc2")
	if name1 == name2 {
		t.Errorf("dependencyName() collision: both = %q", name1)
	}
}

func TestDependencyName_NoTruncationUnder253(t *testing.T) {
	name := dependencyName("c", "r", "vm", "vpcs", "vpc")
	want := "c.r--vm--vpcs.vpc"
	if name != want {
		t.Errorf("dependencyName() = %q, want %q", name, want)
	}
	if len(name) > 253 {
		t.Errorf("unexpected truncation for short name")
	}
}

func TestRuleLabels(t *testing.T) {
	r := &DependentReconciler{
		RuleName:    "my-rule",
		RuleCluster: "abc123",
	}

	labels1 := r.ruleLabels()
	if labels1[LabelRule] != "my-rule" {
		t.Errorf("LabelRule = %q, want %q", labels1[LabelRule], "my-rule")
	}
	if labels1[LabelRuleCluster] != "abc123" {
		t.Errorf("LabelRuleCluster = %q, want %q", labels1[LabelRuleCluster], "abc123")
	}

	// Verify mutation safety — mutating one map shouldn't affect a new call.
	labels1["extra"] = "value"
	labels2 := r.ruleLabels()
	if _, ok := labels2["extra"]; ok {
		t.Error("ruleLabels() returned shared map, mutation leaked")
	}
}
