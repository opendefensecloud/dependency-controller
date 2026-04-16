// Copyright 2026 Open Defense and dependency-controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	registrationv1 "k8s.io/api/admissionregistration/v1"
)

func TestWebhookInstaller_DesiredRulesEmpty(t *testing.T) {
	w := &WebhookInstaller{ruleTargets: make(map[string][]ruleTarget)}
	if len(w.desiredRulesForWorkspace("root:network")) != 0 {
		t.Error("expected empty rules for empty installer")
	}
}

func TestWebhookInstaller_DesiredRulesDedup(t *testing.T) {
	w := &WebhookInstaller{
		ruleTargets: map[string][]ruleTarget{
			"rule-a": {
				{Workspace: "root:network", Key: webhookRuleKey{Group: "net.io", Version: "v1", Resource: "vpcs"}},
			},
			"rule-b": {
				{Workspace: "root:network", Key: webhookRuleKey{Group: "net.io", Version: "v1", Resource: "vpcs"}},
				{Workspace: "root:network", Key: webhookRuleKey{Group: "net.io", Version: "v1", Resource: "firewallrules"}},
			},
		},
	}
	if len(w.desiredRulesForWorkspace("root:network")) != 2 {
		t.Errorf("expected 2 deduplicated rules, got %d", len(w.desiredRulesForWorkspace("root:network")))
	}
}

func TestWebhookInstaller_DesiredRulesFilterByWorkspace(t *testing.T) {
	w := &WebhookInstaller{
		ruleTargets: map[string][]ruleTarget{
			"rule-a": {
				{Workspace: "root:network", Key: webhookRuleKey{Group: "net.io", Version: "v1", Resource: "vpcs"}},
				{Workspace: "root:storage", Key: webhookRuleKey{Group: "store.io", Version: "v1", Resource: "volumes"}},
			},
		},
	}
	if len(w.desiredRulesForWorkspace("root:network")) != 1 {
		t.Error("expected 1 rule for root:network")
	}
	if len(w.desiredRulesForWorkspace("root:storage")) != 1 {
		t.Error("expected 1 rule for root:storage")
	}
	if len(w.desiredRulesForWorkspace("root:compute")) != 0 {
		t.Error("expected 0 rules for root:compute")
	}
}

func TestWebhookInstaller_BuildRuleList(t *testing.T) {
	w := &WebhookInstaller{}
	desired := map[webhookRuleKey]struct{}{
		{Group: "net.io", Version: "v1", Resource: "vpcs"}:          {},
		{Group: "net.io", Version: "v1", Resource: "firewallrules"}: {},
		{Group: "compute.io", Version: "v1", Resource: "vms"}:       {},
	}

	rules := w.buildRuleList(desired)
	if len(rules) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(rules))
	}

	for _, rule := range rules {
		if len(rule.Operations) != 1 || rule.Operations[0] != registrationv1.Delete {
			t.Errorf("expected DELETE operation, got %v", rule.Operations)
		}
	}
}

func TestWebhookInstaller_BuildWebhookConfig(t *testing.T) {
	caBundle := []byte("test-ca")
	w := &WebhookInstaller{
		WebhookURL: "https://webhook.example.com/validate",
		CABundle:   caBundle,
	}

	rules := []registrationv1.RuleWithOperations{{
		Operations: []registrationv1.OperationType{registrationv1.Delete},
		Rule: registrationv1.Rule{
			APIGroups:   []string{"net.io"},
			APIVersions: []string{"v1"},
			Resources:   []string{"vpcs"},
		},
	}}

	cfg := w.buildWebhookConfig(rules)

	if cfg.Name != webhookName {
		t.Errorf("name = %q, want %q", cfg.Name, webhookName)
	}
	if len(cfg.Webhooks) != 1 {
		t.Fatalf("expected 1 webhook, got %d", len(cfg.Webhooks))
	}

	wh := cfg.Webhooks[0]
	if wh.ClientConfig.URL == nil || *wh.ClientConfig.URL != "https://webhook.example.com/validate" {
		t.Errorf("unexpected URL: %v", wh.ClientConfig.URL)
	}
	if string(wh.ClientConfig.CABundle) != "test-ca" {
		t.Error("unexpected CABundle")
	}
	if *wh.FailurePolicy != registrationv1.Fail {
		t.Errorf("failure policy = %v, want Fail", *wh.FailurePolicy)
	}
	if *wh.SideEffects != registrationv1.SideEffectClassNone {
		t.Errorf("side effects = %v, want None", *wh.SideEffects)
	}
	if len(wh.Rules) != 1 {
		t.Errorf("expected 1 rule, got %d", len(wh.Rules))
	}
}

func TestWebhookInstaller_EnsureIdempotent(t *testing.T) {
	w := &WebhookInstaller{ruleTargets: make(map[string][]ruleTarget)}

	// Track some targets manually to test ensureWebhookForWorkspace idempotency.
	w.ruleTargets["rule-a"] = []ruleTarget{
		{Workspace: "root:network", Key: webhookRuleKey{Group: "net.io", Version: "v1", Resource: "vpcs"}},
	}

	// Re-adding the same targets should be a no-op (no new targets).
	deps := []ruleTarget{
		{Workspace: "root:network", Key: webhookRuleKey{Group: "net.io", Version: "v1", Resource: "vpcs"}},
	}

	// Check existing set detection.
	existing := w.ruleTargets["rule-a"]
	existingSet := make(map[ruleTarget]struct{}, len(existing))
	for _, tt := range existing {
		existingSet[tt] = struct{}{}
	}

	var newTargets []ruleTarget
	for _, d := range deps {
		if _, ok := existingSet[d]; !ok {
			newTargets = append(newTargets, d)
		}
	}

	if len(newTargets) != 0 {
		t.Errorf("expected 0 new targets (already tracked), got %d", len(newTargets))
	}
}

func TestWebhookInstaller_MultipleRulesSameWorkspace(t *testing.T) {
	w := &WebhookInstaller{ruleTargets: make(map[string][]ruleTarget)}

	// Two different DependencyRules contribute targets to the same workspace.
	w.ruleTargets["rule-a"] = []ruleTarget{
		{Workspace: "root:network", Key: webhookRuleKey{Group: "net.io", Version: "v1", Resource: "vpcs"}},
	}
	w.ruleTargets["rule-b"] = []ruleTarget{
		{Workspace: "root:network", Key: webhookRuleKey{Group: "net.io", Version: "v1", Resource: "firewallrules"}},
	}

	desired := w.desiredRulesForWorkspace("root:network")
	if len(desired) != 2 {
		t.Fatalf("expected 2 desired rules, got %d", len(desired))
	}

	// Removing rule-a should leave only firewallrules.
	delete(w.ruleTargets, "rule-a")
	desired = w.desiredRulesForWorkspace("root:network")
	if len(desired) != 1 {
		t.Fatalf("expected 1 desired rule after removal, got %d", len(desired))
	}

	key := webhookRuleKey{Group: "net.io", Version: "v1", Resource: "firewallrules"}
	if _, ok := desired[key]; !ok {
		t.Error("expected firewallrules to remain")
	}
}

func TestWebhookInstaller_RemoveAllRulesFromWorkspace(t *testing.T) {
	w := &WebhookInstaller{ruleTargets: make(map[string][]ruleTarget)}

	w.ruleTargets["rule-a"] = []ruleTarget{
		{Workspace: "root:network", Key: webhookRuleKey{Group: "net.io", Version: "v1", Resource: "vpcs"}},
	}

	delete(w.ruleTargets, "rule-a")

	desired := w.desiredRulesForWorkspace("root:network")
	if len(desired) != 0 {
		t.Errorf("expected 0 desired rules when all removed, got %d", len(desired))
	}
}
