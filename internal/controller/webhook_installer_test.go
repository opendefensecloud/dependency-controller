// Copyright 2026 Open Defense and dependency-controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("WebhookInstaller", func() {
	Describe("desiredRulesForWorkspace", func() {
		It("returns empty for an installer with no rules", func() {
			w := &WebhookInstaller{
				ruleTargets: make(map[string][]ruleTarget),
			}
			Expect(w.desiredRulesForWorkspace("root:network")).To(BeEmpty())
		})

		It("deduplicates rules across multiple DependencyRules", func() {
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
			Expect(w.desiredRulesForWorkspace("root:network")).To(HaveLen(2))
		})

		It("filters rules by workspace", func() {
			w := &WebhookInstaller{
				ruleTargets: map[string][]ruleTarget{
					"rule-a": {
						{Workspace: "root:network", Key: webhookRuleKey{Group: "net.io", Version: "v1", Resource: "vpcs"}},
						{Workspace: "root:storage", Key: webhookRuleKey{Group: "store.io", Version: "v1", Resource: "volumes"}},
					},
				},
			}
			Expect(w.desiredRulesForWorkspace("root:network")).To(HaveLen(1))
			Expect(w.desiredRulesForWorkspace("root:storage")).To(HaveLen(1))
			Expect(w.desiredRulesForWorkspace("root:compute")).To(BeEmpty())
		})
	})
})
