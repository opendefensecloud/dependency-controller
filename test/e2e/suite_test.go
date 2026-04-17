// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Tool paths resolved from env vars with PATH fallback.
var (
	kindBin    string
	kubectlBin string
	helmBin    string
	dockerBin  string
)

const (
	kindClusterName    = "dep-ctrl-e2e"
	kcpNamespace       = "kcp-system"
	depNamespace       = "dependency-system"
	kcpNodePort        = "31500"
	kcpServerLocalPort = "31501" // NodePort to kcp server (not front-proxy)
	certManagerVer     = "v1.17.2"
	imageName          = "dependency-controller:integration-test"
	helmTimeout        = "300s"
)

// Workspace names under root.
const (
	wsDepCtrl         = "dep-ctrl"
	wsNetworkProvider = "network-provider"
	wsComputeProvider = "compute-provider"
	wsConsumer1       = "consumer1"
	wsConsumer2       = "consumer2"
)

var (
	rootDir     string
	fixturesDir string
	tmpDir      string

	// Host kubeconfig for kcp via front-proxy NodePort.
	kcpHostKubeconfig string

	// Per-component kubeconfigs for in-cluster pods.
	controllerKubeconfigPath string
	webhookKubeconfigPath    string
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Suite")
}

func lookupTool(envVar, fallback string) string {
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	p, err := exec.LookPath(fallback)
	if err != nil {
		return fallback // let it fail later with a clear error
	}

	return p
}

func init() {
	kindBin = lookupTool("KIND", "kind")
	kubectlBin = lookupTool("KUBECTL", "kubectl")
	helmBin = lookupTool("HELM", "helm")
	dockerBin = lookupTool("DOCKER", "docker")
}

// run executes a command and returns combined output. Fails the test on non-zero exit.
func run(name string, args ...string) string {
	GinkgoHelper()
	cmd := exec.CommandContext(context.Background(), name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		Fail(fmt.Sprintf("command failed: %s %s\n%s\n%v", name, strings.Join(args, " "), buf.String(), err))
	}

	return buf.String()
}

// runNoFail executes a command and returns output + error without failing.
func runNoFail(name string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	return buf.String(), err
}

// kindctl runs kubectl against the kind cluster.
func kindctl(args ...string) string {
	GinkgoHelper()
	return run(kubectlBin, append([]string{"--context", "kind-" + kindClusterName}, args...)...)
}

// kindctlNoFail runs kubectl against the kind cluster without failing.
func kindctlNoFail(args ...string) (string, error) {
	return runNoFail(kubectlBin, append([]string{"--context", "kind-" + kindClusterName}, args...)...)
}

// kcpctl runs kubectl against kcp at a given workspace path.
func kcpctl(wsPath string, args ...string) {
	GinkgoHelper()
	run(kubectlBin, append([]string{
		"--kubeconfig", kcpHostKubeconfig,
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root:%s", kcpNodePort, wsPath),
	}, args...)...)
}

// kcpctlNoFail runs kubectl against kcp without failing.
func kcpctlNoFail(wsPath string, args ...string) (string, error) {
	return runNoFail(kubectlBin, append([]string{
		"--kubeconfig", kcpHostKubeconfig,
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root:%s", kcpNodePort, wsPath),
	}, args...)...)
}

// kcpctlRootNoFail runs kubectl against the kcp root workspace without failing.
func kcpctlRootNoFail(args ...string) (string, error) {
	return runNoFail(kubectlBin, append([]string{
		"--kubeconfig", kcpHostKubeconfig,
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root", kcpNodePort),
	}, args...)...)
}

// subjectAccessReview is the JSON structure for a SubjectAccessReview response.
type subjectAccessReview struct {
	Status struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
	} `json:"status"`
}

// webhookSACanAccessExport checks if the webhook SA has apiexports/content
// access for the given APIExport in the specified workspace, using a
// SubjectAccessReview evaluated by kcp.
func webhookSACanAccessExport(wsPath, exportName string) (bool, error) {
	sarJSON := fmt.Sprintf(`{
  "apiVersion": "authorization.k8s.io/v1",
  "kind": "SubjectAccessReview",
  "spec": {
    "user": "system:serviceaccount:%s:dependency-webhook",
    "groups": ["system:serviceaccounts", "system:serviceaccounts:%s"],
    "resourceAttributes": {
      "group": "apis.kcp.io",
      "resource": "apiexports",
      "subresource": "content",
      "name": %q,
      "verb": "get"
    }
  }
}`, depNamespace, depNamespace, exportName)

	cmd := exec.CommandContext(context.Background(), kubectlBin,
		"--kubeconfig", kcpHostKubeconfig,
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root:%s", kcpNodePort, wsPath),
		"create", "-f", "-", "-o", "json",
	)
	cmd.Stdin = strings.NewReader(sarJSON)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("SAR failed in %s: %s %v", wsPath, buf.String(), err)
	}

	var sar subjectAccessReview
	if err := json.Unmarshal(buf.Bytes(), &sar); err != nil {
		return false, fmt.Errorf("parsing SAR response: %w", err)
	}

	return sar.Status.Allowed, nil
}

// webhookSACanAccessResource checks if the webhook SA can access a specific
// resource type in a workspace, using a SubjectAccessReview.
func webhookSACanAccessResource(wsPath, group, resource, verb string) (bool, error) {
	sarJSON := fmt.Sprintf(`{
  "apiVersion": "authorization.k8s.io/v1",
  "kind": "SubjectAccessReview",
  "spec": {
    "user": "system:serviceaccount:%s:dependency-webhook",
    "groups": ["system:serviceaccounts", "system:serviceaccounts:%s"],
    "resourceAttributes": {
      "group": %q,
      "resource": %q,
      "verb": %q
    }
  }
}`, depNamespace, depNamespace, group, resource, verb)

	cmd := exec.CommandContext(context.Background(), kubectlBin,
		"--kubeconfig", kcpHostKubeconfig,
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root:%s", kcpNodePort, wsPath),
		"create", "-f", "-", "-o", "json",
	)
	cmd.Stdin = strings.NewReader(sarJSON)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("SAR failed in %s: %s %v", wsPath, buf.String(), err)
	}

	var sar subjectAccessReview
	if err := json.Unmarshal(buf.Bytes(), &sar); err != nil {
		return false, fmt.Errorf("parsing SAR response: %w", err)
	}

	return sar.Status.Allowed, nil
}

// applyFixtureToWS applies a YAML fixture to a kcp workspace with placeholder substitution.
func applyFixtureToWS(wsPath, file string, substitutions map[string]string) {
	GinkgoHelper()
	raw, err := os.ReadFile(file)
	Expect(err).NotTo(HaveOccurred())

	content := string(raw)
	for k, v := range substitutions {
		content = strings.ReplaceAll(content, "${"+k+"}", v)
	}

	cmd := exec.CommandContext(context.Background(), kubectlBin,
		"--kubeconfig", kcpHostKubeconfig,
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root:%s", kcpNodePort, wsPath),
		"apply", "-f", "-",
	)
	cmd.Stdin = strings.NewReader(content)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	Expect(cmd.Run()).To(Succeed(), "applying %s to %s: %s", file, wsPath, buf.String())
}

// waitFor retries a check function until it succeeds or the timeout is reached.
func waitFor(timeout time.Duration, desc string, check func() error) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		if err := check(); err == nil {
			return
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			Fail(fmt.Sprintf("timed out waiting for: %s (last error: %v)", desc, lastErr))
		case <-ticker.C:
		}
	}
}

// secretField extracts a base64-decoded field from a k8s secret in the kcp namespace.
func secretField(name, jsonpath string) []byte {
	GinkgoHelper()
	out := kindctl("-n", kcpNamespace, "get", "secret", name, "-o", fmt.Sprintf("jsonpath=%s", jsonpath))
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	Expect(err).NotTo(HaveOccurred())

	return decoded
}

var _ = SynchronizedBeforeSuite(func() {
	var err error
	rootDir, err = filepath.Abs("../..")
	Expect(err).NotTo(HaveOccurred())
	fixturesDir = filepath.Join(rootDir, "test", "fixtures")

	tmpDir, err = os.MkdirTemp("", "dep-ctrl-e2e-*")
	Expect(err).NotTo(HaveOccurred())
	kcpHostKubeconfig = filepath.Join(tmpDir, "kcp-host.kubeconfig")

	By("creating kind cluster")
	createKindCluster()

	By("installing cert-manager")
	installCertManager()

	By("deploying kcp via helm")
	deployKCP()

	By("building admin kubeconfigs")
	buildAdminKubeconfigs()

	By("exposing kcp server via NodePort")
	kindctl("apply", "-f", filepath.Join(fixturesDir, "kcp-server-nodeport.yaml"))

	By("building component kubeconfigs")
	buildComponentKubeconfigs()

	By("building and loading image")
	buildAndLoadImage()

	By("setting up kcp workspaces")
	setupKCPWorkspaces()

	By("bootstrapping RBAC")
	bootstrapRBAC()

	By("deploying helm charts")
	deployCharts()
}, func() {})

var _ = SynchronizedAfterSuite(func() {}, func() {
	if os.Getenv("E2E_SKIP_CLEANUP") != "" {
		return
	}
	out, err := runNoFail(kindBin, "delete", "cluster", "--name", kindClusterName)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "kind delete: %s %v\n", out, err)
	}
	if tmpDir != "" {
		_ = os.RemoveAll(tmpDir)
	}
})

func createKindCluster() {
	// Reuse if it already exists.
	out, _ := runNoFail(kindBin, "get", "clusters")
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == kindClusterName {
			// Ensure kubeconfig context exists (may be lost after kind delete/recreate).
			run(kindBin, "export", "kubeconfig", "--name", kindClusterName)
			return
		}
	}

	run(kindBin, "create", "cluster",
		"--name", kindClusterName,
		"--config", filepath.Join(fixturesDir, "kind-config.yaml"),
		"--wait", "60s",
	)
}

func installCertManager() {
	kindctl("apply", "-f",
		fmt.Sprintf("https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml", certManagerVer))

	waitFor(2*time.Minute, "cert-manager ready", func() error {
		_, err := kindctlNoFail("-n", "cert-manager", "wait", "deployment", "cert-manager-webhook",
			"--for=condition=Available", "--timeout=1s")

		return err
	})

	waitFor(time.Minute, "self-signed ClusterIssuer created", func() error {
		_, err := kindctlNoFail("apply", "-f", filepath.Join(fixturesDir, "cert-manager-selfsigned-issuer.yaml"))
		return err
	})
}

func deployKCP() {
	_, _ = runNoFail(helmBin, "repo", "add", "kcp", "https://kcp-dev.github.io/helm-charts")
	run(helmBin, "repo", "update", "kcp")

	run(helmBin, "upgrade", "--install", "kcp", "kcp/kcp",
		"--namespace", kcpNamespace,
		"--create-namespace",
		"--values", filepath.Join(fixturesDir, "integration-values-kcp.yaml"),
		"--wait", "--timeout", helmTimeout,
	)
}

func buildAdminKubeconfigs() {
	kindctl("apply", "-f", filepath.Join(fixturesDir, "kcp-admin-cert.yaml"))

	waitFor(time.Minute, "front-proxy admin cert issued", func() error {
		_, err := kindctlNoFail("-n", kcpNamespace, "get", "secret", "kcp-admin-front-proxy-cert",
			"-o", "jsonpath={.data.tls\\.crt}")

		return err
	})

	// Extract front-proxy client certs for host kubeconfig.
	fpClientCrt := secretField("kcp-admin-front-proxy-cert", "{.data.tls\\.crt}")
	fpClientKey := secretField("kcp-admin-front-proxy-cert", "{.data.tls\\.key}")

	fpCrtFile := filepath.Join(tmpDir, "fp-client.crt")
	fpKeyFile := filepath.Join(tmpDir, "fp-client.key")
	Expect(os.WriteFile(fpCrtFile, fpClientCrt, 0o600)).To(Succeed())
	Expect(os.WriteFile(fpKeyFile, fpClientKey, 0o600)).To(Succeed())

	run(kubectlBin, "--kubeconfig", kcpHostKubeconfig, "config", "set-cluster", "kcp",
		"--server=https://localhost:"+kcpNodePort,
		"--insecure-skip-tls-verify=true")
	run(kubectlBin, "--kubeconfig", kcpHostKubeconfig, "config", "set-credentials", "kcp-admin",
		"--client-certificate="+fpCrtFile,
		"--client-key="+fpKeyFile,
		"--embed-certs=true")
	run(kubectlBin, "--kubeconfig", kcpHostKubeconfig, "config", "set-context", "kcp",
		"--cluster=kcp", "--user=kcp-admin")
	run(kubectlBin, "--kubeconfig", kcpHostKubeconfig, "config", "use-context", "kcp")

	waitFor(30*time.Second, "kcp API reachable", func() error {
		_, err := runNoFail(kubectlBin, "--kubeconfig", kcpHostKubeconfig, "get", "--raw", "/readyz")
		return err
	})
}

// buildComponentKubeconfigs creates per-component client certificates and
// kubeconfigs for the controller and webhook. Each component gets a
// least-privilege identity instead of shared admin credentials.
func buildComponentKubeconfigs() {
	// Apply cert requests for controller and webhook identities.
	kindctl("apply", "-f", filepath.Join(fixturesDir, "kcp-controller-cert.yaml"))
	kindctl("apply", "-f", filepath.Join(fixturesDir, "kcp-webhook-sa-cert.yaml"))

	waitFor(time.Minute, "controller cert issued", func() error {
		_, err := kindctlNoFail("-n", kcpNamespace, "get", "secret", "kcp-controller-cert",
			"-o", "jsonpath={.data.tls\\.crt}")

		return err
	})
	waitFor(time.Minute, "webhook SA cert issued", func() error {
		_, err := kindctlNoFail("-n", kcpNamespace, "get", "secret", "kcp-webhook-sa-cert",
			"-o", "jsonpath={.data.tls\\.crt}")

		return err
	})

	kcpServerCA := secretField("kcp-ca", "{.data.tls\\.crt}")
	caFile := filepath.Join(tmpDir, "kcp-server-ca.crt")
	Expect(os.WriteFile(caFile, kcpServerCA, 0o600)).To(Succeed())

	kcpInternalServer := fmt.Sprintf("https://kcp.%s.svc.cluster.local:6443/clusters/root:%s", kcpNamespace, wsDepCtrl)

	// Build controller kubeconfig.
	ctrlCrt := secretField("kcp-controller-cert", "{.data.tls\\.crt}")
	ctrlKey := secretField("kcp-controller-cert", "{.data.tls\\.key}")
	ctrlCrtFile := filepath.Join(tmpDir, "ctrl-client.crt")
	ctrlKeyFile := filepath.Join(tmpDir, "ctrl-client.key")
	Expect(os.WriteFile(ctrlCrtFile, ctrlCrt, 0o600)).To(Succeed())
	Expect(os.WriteFile(ctrlKeyFile, ctrlKey, 0o600)).To(Succeed())

	controllerKubeconfigPath = filepath.Join(tmpDir, "kcp-controller.kubeconfig")
	run(kubectlBin, "--kubeconfig", controllerKubeconfigPath, "config", "set-cluster", "kcp",
		"--server="+kcpInternalServer,
		"--certificate-authority="+caFile,
		"--embed-certs=true")
	run(kubectlBin, "--kubeconfig", controllerKubeconfigPath, "config", "set-credentials", "controller",
		"--client-certificate="+ctrlCrtFile,
		"--client-key="+ctrlKeyFile,
		"--embed-certs=true")
	run(kubectlBin, "--kubeconfig", controllerKubeconfigPath, "config", "set-context", "kcp",
		"--cluster=kcp", "--user=controller")
	run(kubectlBin, "--kubeconfig", controllerKubeconfigPath, "config", "use-context", "kcp")

	// Build webhook kubeconfig.
	whCrt := secretField("kcp-webhook-sa-cert", "{.data.tls\\.crt}")
	whKey := secretField("kcp-webhook-sa-cert", "{.data.tls\\.key}")
	whCrtFile := filepath.Join(tmpDir, "webhook-client.crt")
	whKeyFile := filepath.Join(tmpDir, "webhook-client.key")
	Expect(os.WriteFile(whCrtFile, whCrt, 0o600)).To(Succeed())
	Expect(os.WriteFile(whKeyFile, whKey, 0o600)).To(Succeed())

	webhookKubeconfigPath = filepath.Join(tmpDir, "kcp-webhook.kubeconfig")
	run(kubectlBin, "--kubeconfig", webhookKubeconfigPath, "config", "set-cluster", "kcp",
		"--server="+kcpInternalServer,
		"--certificate-authority="+caFile,
		"--embed-certs=true")
	run(kubectlBin, "--kubeconfig", webhookKubeconfigPath, "config", "set-credentials", "webhook",
		"--client-certificate="+whCrtFile,
		"--client-key="+whKeyFile,
		"--embed-certs=true")
	run(kubectlBin, "--kubeconfig", webhookKubeconfigPath, "config", "set-context", "kcp",
		"--cluster=kcp", "--user=webhook")
	run(kubectlBin, "--kubeconfig", webhookKubeconfigPath, "config", "use-context", "kcp")
}

// bootstrapRBAC uses the bootstrap (system:masters) cert to create
// RBAC for the controller and webhook identities.
// - Root workspace: workspace access, webhook/RBAC management, APIExport content
// - Dep-ctrl workspace: APIExportEndpointSlice read for VW URL discovery
func bootstrapRBAC() {
	kindctl("apply", "-f", filepath.Join(fixturesDir, "kcp-bootstrap-cert.yaml"))

	waitFor(time.Minute, "bootstrap cert issued", func() error {
		_, err := kindctlNoFail("-n", kcpNamespace, "get", "secret", "kcp-bootstrap-cert",
			"-o", "jsonpath={.data.tls\\.crt}")

		return err
	})

	bsCrt := secretField("kcp-bootstrap-cert", "{.data.tls\\.crt}")
	bsKey := secretField("kcp-bootstrap-cert", "{.data.tls\\.key}")
	kcpServerCA := secretField("kcp-ca", "{.data.tls\\.crt}")

	bsCrtFile := filepath.Join(tmpDir, "bootstrap-client.crt")
	bsKeyFile := filepath.Join(tmpDir, "bootstrap-client.key")
	caFile := filepath.Join(tmpDir, "kcp-server-ca.crt")
	Expect(os.WriteFile(bsCrtFile, bsCrt, 0o600)).To(Succeed())
	Expect(os.WriteFile(bsKeyFile, bsKey, 0o600)).To(Succeed())
	Expect(os.WriteFile(caFile, kcpServerCA, 0o600)).To(Succeed())

	// Build a temporary bootstrap kubeconfig targeting root via kcp server NodePort.
	bsKubeconfig := filepath.Join(tmpDir, "kcp-bootstrap.kubeconfig")
	run(kubectlBin, "--kubeconfig", bsKubeconfig, "config", "set-cluster", "kcp",
		fmt.Sprintf("--server=https://localhost:%s/clusters/root", kcpServerLocalPort),
		"--certificate-authority="+caFile,
		"--embed-certs=true")
	run(kubectlBin, "--kubeconfig", bsKubeconfig, "config", "set-credentials", "bootstrap",
		"--client-certificate="+bsCrtFile,
		"--client-key="+bsKeyFile,
		"--embed-certs=true")
	run(kubectlBin, "--kubeconfig", bsKubeconfig, "config", "set-context", "kcp",
		"--cluster=kcp", "--user=bootstrap")
	run(kubectlBin, "--kubeconfig", bsKubeconfig, "config", "use-context", "kcp")

	// Apply RBAC in the root workspace.
	run(kubectlBin, "--kubeconfig", bsKubeconfig, "apply", "-f",
		filepath.Join(fixturesDir, "root-rbac-bootstrap.yaml"))

	// Apply RBAC in the dep-ctrl workspace (apiexportendpointslices read).
	run(kubectlBin, "--kubeconfig", bsKubeconfig,
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root:%s", kcpServerLocalPort, wsDepCtrl),
		"apply", "-f", filepath.Join(fixturesDir, "depctrl-rbac-bootstrap.yaml"))

	// Apply shard-wide RBAC in system:admin (apiexports/content + apiexportendpointslices for webhook SA).
	// The Bootstrap Policy Authorizer evaluates system:admin RBAC for every request on the shard.
	run(kubectlBin, "--kubeconfig", bsKubeconfig,
		"--server", fmt.Sprintf("https://localhost:%s/clusters/system:admin", kcpServerLocalPort),
		"apply", "-f", filepath.Join(fixturesDir, "shard-admin-rbac-bootstrap.yaml"))
}

func buildAndLoadImage() {
	run(dockerBin, "build", "-t", imageName, rootDir)
	run(kindBin, "load", "docker-image", imageName, "--name", kindClusterName)
}

func setupKCPWorkspaces() {
	// Create all workspaces.
	for _, ws := range []string{wsDepCtrl, wsNetworkProvider, wsComputeProvider, wsConsumer1, wsConsumer2} {
		cmd := exec.CommandContext(context.Background(), kubectlBin,
			"--kubeconfig", kcpHostKubeconfig,
			"--server", fmt.Sprintf("https://localhost:%s/clusters/root", kcpNodePort),
			"apply", "-f", "-",
		)
		cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: tenancy.kcp.io/v1alpha1
kind: Workspace
metadata:
  name: %s`, ws))
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		cmd.Run() //nolint:errcheck // ignore AlreadyExists
	}

	// Wait for workspaces to be ready.
	for _, ws := range []string{wsDepCtrl, wsNetworkProvider, wsComputeProvider, wsConsumer1, wsConsumer2} {
		waitFor(time.Minute, fmt.Sprintf("workspace %s ready", ws), func() error {
			out, err := kcpctlRootNoFail("get", "workspace", ws, "-o", "jsonpath={.status.phase}")
			if err != nil {
				return err
			}
			if strings.TrimSpace(out) != "Ready" {
				return fmt.Errorf("workspace %s phase: %s", ws, out)
			}

			return nil
		})
	}

	// Apply schemas and exports.
	kcpctl(wsDepCtrl, "apply", "-f", filepath.Join(rootDir, "config/kcp/apiresourceschema-dependencyrules.dependencies.opendefense.cloud.yaml"))
	kcpctl(wsDepCtrl, "apply", "-f", filepath.Join(rootDir, "config/kcp/apiexport-dependencies.opendefense.cloud.yaml"))

	kcpctl(wsNetworkProvider, "apply", "-f", filepath.Join(fixturesDir, "apiresourceschema-vpcs.yaml"))
	kcpctl(wsNetworkProvider, "apply", "-f", filepath.Join(fixturesDir, "apiexport-network.test.io.yaml"))
	kcpctl(wsComputeProvider, "apply", "-f", filepath.Join(fixturesDir, "apiresourceschema-virtualmachines.yaml"))
	kcpctl(wsComputeProvider, "apply", "-f", filepath.Join(fixturesDir, "apiexport-compute.test.io.yaml"))

	// Bindings.
	depCtrlPath := "root:" + wsDepCtrl
	networkPath := "root:" + wsNetworkProvider
	computePath := "root:" + wsComputeProvider

	subs := map[string]string{
		"DEP_CTRL_PATH": depCtrlPath,
	}
	applyFixtureToWS(wsComputeProvider, filepath.Join(fixturesDir, "apibinding-dependencies.opendefense.cloud.yaml"), subs)
	applyFixtureToWS(wsNetworkProvider, filepath.Join(fixturesDir, "apibinding-dependencies.opendefense.cloud.yaml"), subs)

	consumerSubs := map[string]string{
		"NETWORK_PROVIDER_PATH": networkPath,
		"COMPUTE_PROVIDER_PATH": computePath,
	}
	for _, ws := range []string{wsConsumer1, wsConsumer2} {
		applyFixtureToWS(ws, filepath.Join(fixturesDir, "apibinding-network.test.io.yaml"), consumerSubs)
		applyFixtureToWS(ws, filepath.Join(fixturesDir, "apibinding-compute.test.io.yaml"), consumerSubs)
	}

	// Wait for bindings.
	bindings := []struct {
		ws, name string
	}{
		{wsComputeProvider, "dependencies.opendefense.cloud"},
		{wsNetworkProvider, "dependencies.opendefense.cloud"},
		{wsConsumer1, "network.test.io"}, {wsConsumer1, "compute.test.io"},
		{wsConsumer2, "network.test.io"}, {wsConsumer2, "compute.test.io"},
	}
	for _, b := range bindings {
		waitFor(time.Minute, fmt.Sprintf("binding %s in %s bound", b.name, b.ws), func() error {
			out, err := kcpctlNoFail(b.ws, "get", "apibinding", b.name, "-o", "jsonpath={.status.phase}")
			if err != nil {
				return err
			}
			if strings.TrimSpace(out) != "Bound" {
				return fmt.Errorf("phase: %s", out)
			}

			return nil
		})
	}
}

// createKubeconfigSecret creates or updates a Secret in the dep-ctrl namespace
// containing the given kubeconfig file.
func createKubeconfigSecret(secretName, kubeconfigPath string) {
	GinkgoHelper()
	cmd := exec.CommandContext(context.Background(), kubectlBin, "--context", "kind-"+kindClusterName,
		"-n", depNamespace, "create", "secret", "generic", secretName,
		"--from-file=kubeconfig="+kubeconfigPath,
		"--dry-run=client", "-o", "yaml")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	Expect(cmd.Run()).To(Succeed(), buf.String())
	applyCmd := exec.CommandContext(context.Background(), kubectlBin, "--context", "kind-"+kindClusterName, "apply", "-f", "-")
	applyCmd.Stdin = bytes.NewReader(buf.Bytes())
	var applyBuf bytes.Buffer
	applyCmd.Stdout = &applyBuf
	applyCmd.Stderr = &applyBuf
	Expect(applyCmd.Run()).To(Succeed(), applyBuf.String())
}

func deployCharts() {
	kindctlNoFail("create", "namespace", depNamespace) //nolint:errcheck

	createKubeconfigSecret("kcp-controller-kubeconfig", controllerKubeconfigPath)
	createKubeconfigSecret("kcp-webhook-kubeconfig", webhookKubeconfigPath)

	run(helmBin, "upgrade", "--install", "dep-ctrl",
		filepath.Join(rootDir, "charts/dependency-controller"),
		"--namespace", depNamespace,
		"--values", filepath.Join(fixturesDir, "integration-values.yaml"),
		"--wait", "--timeout", "120s",
	)
}
