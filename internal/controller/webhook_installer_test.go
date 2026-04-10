package controller

import "testing"

func TestDesiredRulesForWorkspace_Empty(t *testing.T) {
	w := &WebhookInstaller{
		ruleTargets: make(map[string][]ruleTarget),
	}
	desired := w.desiredRulesForWorkspace("root:network")
	if len(desired) != 0 {
		t.Errorf("expected empty, got %d rules", len(desired))
	}
}

func TestDesiredRulesForWorkspace_Deduplication(t *testing.T) {
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

	desired := w.desiredRulesForWorkspace("root:network")
	if len(desired) != 2 {
		t.Errorf("expected 2 deduplicated rules, got %d", len(desired))
	}
}

func TestDesiredRulesForWorkspace_FiltersWorkspace(t *testing.T) {
	w := &WebhookInstaller{
		ruleTargets: map[string][]ruleTarget{
			"rule-a": {
				{Workspace: "root:network", Key: webhookRuleKey{Group: "net.io", Version: "v1", Resource: "vpcs"}},
				{Workspace: "root:storage", Key: webhookRuleKey{Group: "store.io", Version: "v1", Resource: "volumes"}},
			},
		},
	}

	network := w.desiredRulesForWorkspace("root:network")
	if len(network) != 1 {
		t.Errorf("expected 1 rule for root:network, got %d", len(network))
	}

	storage := w.desiredRulesForWorkspace("root:storage")
	if len(storage) != 1 {
		t.Errorf("expected 1 rule for root:storage, got %d", len(storage))
	}

	missing := w.desiredRulesForWorkspace("root:compute")
	if len(missing) != 0 {
		t.Errorf("expected 0 rules for root:compute, got %d", len(missing))
	}
}
