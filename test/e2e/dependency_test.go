// Copyright 2026 Open Defense and dependency-controller contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Dependency Controller E2E", Ordered, func() {
	var subs map[string]string

	BeforeAll(func() {
		subs = map[string]string{
			"COMPUTE_PROVIDER_PATH": "root:" + wsComputeProvider,
			"NETWORK_PROVIDER_PATH": "root:" + wsNetworkProvider,
		}
	})

	It("should create DependencyRule and install webhook", func() {
		By("creating a VPC in consumer1")
		applyFixtureToWS(wsConsumer1, filepath.Join(fixturesDir, "vpc-my-vpc.yaml"), nil)

		By("creating a DependencyRule in the compute-provider workspace")
		applyFixtureToWS(wsComputeProvider, filepath.Join(fixturesDir, "dependencyrule-vm-dependencies.yaml"), subs)

		By("creating a VM referencing the VPC in consumer1")
		applyFixtureToWS(wsConsumer1, filepath.Join(fixturesDir, "vm-my-vm.yaml"), nil)

		By("waiting for the ValidatingWebhookConfiguration in network-provider")
		waitFor(time.Minute, "webhook installed in network-provider", func() error {
			_, err := kcpctlNoFail(wsNetworkProvider, "get", "validatingwebhookconfiguration", "dependency-controller")
			return err
		})
	})

	It("should block VPC deletion while a VM references it", func() {
		waitFor(time.Minute, "webhook blocks VPC deletion", func() error {
			out, err := kcpctlNoFail(wsConsumer1, "delete", "vpc", "my-vpc", "--namespace", "default")
			if err == nil {
				// Deletion succeeded — recreate and retry.
				applyFixtureToWS(wsConsumer1, filepath.Join(fixturesDir, "vpc-my-vpc.yaml"), nil)
				return fmt.Errorf("deletion was not blocked, recreated VPC")
			}
			if strings.Contains(out, "still referenced by") {
				return nil
			}

			return fmt.Errorf("unexpected output: %s", out)
		})
	})

	It("should not affect consumer2 where there are no VMs", func() {
		applyFixtureToWS(wsConsumer2, filepath.Join(fixturesDir, "vpc-isolated-vpc.yaml"), nil)

		waitFor(30*time.Second, "VPC deletion succeeds in consumer2", func() error {
			_, err := kcpctlNoFail(wsConsumer2, "delete", "vpc", "isolated-vpc", "--namespace", "default")
			return err
		})
	})

	It("should allow VPC deletion after the dependent VM is deleted", func() {
		kcpctl(wsConsumer1, "delete", "virtualmachine", "my-vm", "--namespace", "default")

		waitFor(time.Minute, "VPC deletion allowed after VM removal", func() error {
			_, err := kcpctlNoFail(wsConsumer1, "delete", "vpc", "my-vpc", "--namespace", "default")
			return err
		})
	})

	It("should remove webhook after DependencyRule is deleted", func() {
		By("creating VPC and VM for cleanup test")
		applyFixtureToWS(wsConsumer1, filepath.Join(fixturesDir, "vpc-cleanup-vpc.yaml"), nil)
		applyFixtureToWS(wsConsumer1, filepath.Join(fixturesDir, "vm-cleanup-vm.yaml"), nil)

		By("waiting for webhook to block cleanup-vpc deletion")
		waitFor(time.Minute, "webhook blocks cleanup-vpc deletion", func() error {
			out, err := kcpctlNoFail(wsConsumer1, "delete", "vpc", "cleanup-vpc", "--namespace", "default")
			if err == nil {
				applyFixtureToWS(wsConsumer1, filepath.Join(fixturesDir, "vpc-cleanup-vpc.yaml"), nil)
				return fmt.Errorf("deletion was not blocked, recreated VPC")
			}
			if strings.Contains(out, "still referenced by") {
				return nil
			}

			return fmt.Errorf("unexpected output: %s", out)
		})

		By("deleting the DependencyRule")
		kcpctl(wsComputeProvider, "delete", "dependencyrule", "vm-dependencies")

		By("waiting for webhook removal from network-provider")
		waitFor(time.Minute, "webhook removed from network-provider", func() error {
			_, err := kcpctlNoFail(wsNetworkProvider, "get", "validatingwebhookconfiguration", "dependency-controller")
			if err != nil {
				return nil // not found = removed
			}

			return fmt.Errorf("webhook still exists")
		})

		By("verifying VPC deletion now succeeds")
		Expect(func() error {
			_, err := kcpctlNoFail(wsConsumer1, "delete", "vpc", "cleanup-vpc", "--namespace", "default")
			return err
		}()).To(Succeed())

		// Clean up remaining resources.
		kcpctlNoFail(wsConsumer1, "delete", "virtualmachine", "cleanup-vm", "--namespace", "default") //nolint:errcheck
	})
})
