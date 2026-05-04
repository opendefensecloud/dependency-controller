// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	kindClusterName = "dep-ctrl-e2e"
	kcpNamespace    = "kcp-system"
	depNamespace    = "dependency-system"
	certManagerVer  = "v1.17.2"
	imageName       = "dependency-controller:integration-test"
	helmTimeout     = "300s"

	// NodePort for the front-proxy service exposed via kind.
	frontProxyNodePort = "31443"
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

	// In-cluster front-proxy base URL (extracted from kcp-operator kubeconfig).
	inClusterFPURL string
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
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root:%s", frontProxyNodePort, wsPath),
	}, args...)...)
}

// kcpctlNoFail runs kubectl against kcp without failing.
func kcpctlNoFail(wsPath string, args ...string) (string, error) {
	return runNoFail(kubectlBin, append([]string{
		"--kubeconfig", kcpHostKubeconfig,
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root:%s", frontProxyNodePort, wsPath),
	}, args...)...)
}

// kcpctlRootNoFail runs kubectl against the kcp root workspace without failing.
func kcpctlRootNoFail(args ...string) (string, error) {
	return runNoFail(kubectlBin, append([]string{
		"--kubeconfig", kcpHostKubeconfig,
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root", frontProxyNodePort),
	}, args...)...)
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
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root:%s", frontProxyNodePort, wsPath),
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

// kindctlSecret extracts the kubeconfig from a k8s secret in the kcp-system namespace.
func kindctlSecret(name string) string {
	GinkgoHelper()
	return kindctl("-n", kcpNamespace, "get", "secret", name, "-o", "jsonpath={.data.kubeconfig}")
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

	By("deploying kcp via kcp-operator")
	deployKCPOperator()

	By("deploying etcd instances")
	deployEtcd()

	By("creating kcp RootShard, Shard, and FrontProxy")
	createKCPResources()

	By("generating admin kubeconfig")
	buildAdminKubeconfig()

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

func deployKCPOperator() {
	_, _ = runNoFail(helmBin, "repo", "add", "kcp", "https://kcp-dev.github.io/helm-charts")
	run(helmBin, "repo", "update", "kcp")

	run(helmBin, "upgrade", "--install", "kcp-operator", "kcp/kcp-operator",
		"--namespace", kcpNamespace,
		"--create-namespace",
		"--wait", "--timeout", helmTimeout,
	)
}

func deployEtcd() {
	// Deploy two etcd instances: one for the root shard, one for the secondary shard.
	for _, name := range []string{"etcd-root", "etcd-shard"} {
		applyEtcd(name)
	}

	// Wait for etcd pods to be ready.
	for _, name := range []string{"etcd-root", "etcd-shard"} {
		waitFor(2*time.Minute, fmt.Sprintf("%s ready", name), func() error {
			_, err := kindctlNoFail("-n", kcpNamespace, "wait", "statefulset", name,
				"--for=jsonpath={.status.readyReplicas}=1", "--timeout=1s")

			return err
		})
	}
}

// applyEtcd creates a minimal single-node etcd instance in the kcp namespace.
func applyEtcd(name string) {
	GinkgoHelper()
	manifest := fmt.Sprintf(`---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/name: etcd
    app.kubernetes.io/instance: %[1]s
  ports:
    - name: client
      port: 2379
      targetPort: client
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s-headless
  namespace: %[2]s
  annotations:
    service.alpha.kubernetes.io/tolerate-unready-endpoints: "true"
spec:
  type: ClusterIP
  clusterIP: None
  publishNotReadyAddresses: true
  selector:
    app.kubernetes.io/name: etcd
    app.kubernetes.io/instance: %[1]s
  ports:
    - name: client
      port: 2379
      targetPort: client
    - name: peer
      port: 2380
      targetPort: peer
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: etcd
      app.kubernetes.io/instance: %[1]s
  serviceName: %[1]s-headless
  template:
    metadata:
      labels:
        app.kubernetes.io/name: etcd
        app.kubernetes.io/instance: %[1]s
    spec:
      automountServiceAccountToken: false
      containers:
        - name: etcd
          image: quay.io/coreos/etcd:v3.5.21
          imagePullPolicy: IfNotPresent
          command: ["/usr/local/bin/etcd"]
          args:
            - --name=$(HOSTNAME)
            - --data-dir=/data
            - --listen-peer-urls=http://0.0.0.0:2380
            - --listen-client-urls=http://0.0.0.0:2379
            - --advertise-client-urls=http://$(HOSTNAME).%[1]s-headless.%[2]s.svc.cluster.local:2379
            - --initial-cluster-state=new
            - --initial-cluster-token=$(HOSTNAME)
            - --initial-cluster=$(HOSTNAME)=http://$(HOSTNAME).%[1]s-headless.%[2]s.svc.cluster.local:2380
            - --initial-advertise-peer-urls=http://$(HOSTNAME).%[1]s-headless.%[2]s.svc.cluster.local:2380
            - --listen-metrics-urls=http://0.0.0.0:8080
          env:
            - name: HOSTNAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
          ports:
            - name: client
              containerPort: 2379
            - name: peer
              containerPort: 2380
            - name: metrics
              containerPort: 8080
          livenessProbe:
            httpGet:
              path: /livez
              port: metrics
            initialDelaySeconds: 15
            periodSeconds: 10
          readinessProbe:
            httpGet:
              path: /readyz
              port: metrics
            initialDelaySeconds: 10
            periodSeconds: 5
            failureThreshold: 30
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              memory: 256Mi
          volumeMounts:
            - name: data
              mountPath: /data
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: [ReadWriteOnce]
        resources:
          requests:
            storage: 1Gi
`, name, kcpNamespace)

	cmd := exec.CommandContext(context.Background(), kubectlBin,
		"--context", "kind-"+kindClusterName, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	Expect(cmd.Run()).To(Succeed(), "applying etcd %s: %s", name, buf.String())
}

func createKCPResources() {
	// The front-proxy hostname used for in-cluster access and via NodePort.
	fpHostname := fmt.Sprintf("kcp-front-proxy.%s.svc.cluster.local", kcpNamespace)

	// Create a cert-manager Issuer in the kcp namespace for kcp-operator PKI.
	applyToKind(fmt.Sprintf(`apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: selfsigned
  namespace: %s
spec:
  selfSigned: {}`, kcpNamespace))

	// Create RootShard. certificateTemplates adds localhost to the server cert
	// so we can port-forward to the shard for direct access during bootstrap.
	applyToKind(fmt.Sprintf(`apiVersion: operator.kcp.io/v1alpha1
kind: RootShard
metadata:
  name: root
  namespace: %[1]s
spec:
  external:
    hostname: %[2]s
    port: 6443
  certificates:
    issuerRef:
      group: cert-manager.io
      kind: Issuer
      name: selfsigned
  certificateTemplates:
    server:
      spec:
        dnsNames:
          - localhost
        ipAddresses:
          - "127.0.0.1"
  cache:
    embedded:
      enabled: true
  etcd:
    endpoints:
      - http://etcd-root.%[1]s.svc.cluster.local:2379
  auth:
    serviceAccount:
      enabled: true
  deploymentTemplate:
    spec:
      template:
        spec:
          hostAliases:
            - ip: "10.96.200.200"
              hostnames:
                - "%[2]s"`, kcpNamespace, fpHostname))

	// Create FrontProxy with a fixed ClusterIP and NodePort for host access.
	applyToKind(fmt.Sprintf(`apiVersion: operator.kcp.io/v1alpha1
kind: FrontProxy
metadata:
  name: kcp
  namespace: %[1]s
spec:
  rootShard:
    ref:
      name: root
  auth:
    serviceAccount:
      enabled: true
  serviceTemplate:
    spec:
      type: NodePort
      clusterIP: "10.96.200.200"
  certificateTemplates:
    server:
      spec:
        dnsNames:
          - localhost
          - "%[2]s"
        ipAddresses:
          - "127.0.0.1"`, kcpNamespace, fpHostname))

	// Wait for the RootShard to be running.
	waitFor(3*time.Minute, "root shard running", func() error {
		out, err := kindctlNoFail("-n", kcpNamespace, "get", "rootshard", "root",
			"-o", "jsonpath={.status.phase}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "Running" {
			return fmt.Errorf("root shard phase: %s", out)
		}

		return nil
	})

	// Wait for the FrontProxy to be running.
	waitFor(2*time.Minute, "front-proxy running", func() error {
		out, err := kindctlNoFail("-n", kcpNamespace, "get", "frontproxy", "kcp",
			"-o", "jsonpath={.status.phase}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "Running" {
			return fmt.Errorf("front-proxy phase: %s", out)
		}

		return nil
	})

	// Patch the front-proxy Service to use a fixed NodePort.
	kindctl("-n", kcpNamespace, "patch", "service", "kcp-front-proxy", "--type=json",
		fmt.Sprintf(`-p=[{"op":"replace","path":"/spec/ports/0/nodePort","value":%s}]`, frontProxyNodePort))

	// Create secondary Shard with localhost in server cert for port-forward access.
	applyToKind(fmt.Sprintf(`apiVersion: operator.kcp.io/v1alpha1
kind: Shard
metadata:
  name: shard1
  namespace: %[1]s
spec:
  rootShard:
    ref:
      name: root
  etcd:
    endpoints:
      - http://etcd-shard.%[1]s.svc.cluster.local:2379
  auth:
    serviceAccount:
      enabled: true
  certificateTemplates:
    server:
      spec:
        dnsNames:
          - localhost
        ipAddresses:
          - "127.0.0.1"
  deploymentTemplate:
    spec:
      template:
        spec:
          hostAliases:
            - ip: "10.96.200.200"
              hostnames:
                - "%[2]s"`, kcpNamespace, fpHostname))

	// Wait for the secondary shard to be running.
	waitFor(3*time.Minute, "shard1 running", func() error {
		out, err := kindctlNoFail("-n", kcpNamespace, "get", "shard", "shard1",
			"-o", "jsonpath={.status.phase}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "Running" {
			return fmt.Errorf("shard1 phase: %s", out)
		}

		return nil
	})
}

func buildAdminKubeconfig() {
	// Create a Kubeconfig CR for admin access via front-proxy.
	applyToKind(fmt.Sprintf(`apiVersion: operator.kcp.io/v1alpha1
kind: Kubeconfig
metadata:
  name: e2e-admin
  namespace: %s
spec:
  username: kcp-admin
  groups:
    - "system:kcp:admin"
  validity: 8766h
  secretRef:
    name: e2e-admin-kubeconfig
  target:
    frontProxyRef:
      name: kcp`, kcpNamespace))

	// Wait for the kubeconfig secret to be created.
	waitFor(2*time.Minute, "admin kubeconfig secret created", func() error {
		_, err := kindctlNoFail("-n", kcpNamespace, "get", "secret", "e2e-admin-kubeconfig",
			"-o", "jsonpath={.data.kubeconfig}")

		return err
	})

	// Extract the kubeconfig and rewrite the server URL to use localhost NodePort.
	kcRaw := kindctlSecret("e2e-admin-kubeconfig")
	kcBytes, err := decodeBase64(kcRaw)
	Expect(err).NotTo(HaveOccurred())

	// Extract the actual server URL from the kubeconfig rather than hardcoding the port.
	adminServerURL := extractServerFromKubeconfig(kcBytes)
	rewritten := strings.ReplaceAll(string(kcBytes),
		adminServerURL,
		fmt.Sprintf("https://localhost:%s", frontProxyNodePort))

	Expect(os.WriteFile(kcpHostKubeconfig, []byte(rewritten), 0o600)).To(Succeed())

	waitFor(30*time.Second, "kcp API reachable via front-proxy", func() error {
		_, err := runNoFail(kubectlBin, "--kubeconfig", kcpHostKubeconfig,
			"--server", fmt.Sprintf("https://localhost:%s/clusters/root", frontProxyNodePort),
			"get", "--raw", "/readyz")

		return err
	})
}

// buildComponentKubeconfigs creates Kubeconfig CRs for the controller and webhook
// identities, then extracts the generated kubeconfigs pointing at the in-cluster
// front-proxy for use by deployed pods.
func buildComponentKubeconfigs() {
	depCtrlPath := "root:" + wsDepCtrl

	// Controller and webhook kubeconfigs target the root shard (not the front-proxy)
	// so their client certificates are signed by root-client-ca. This CA is trusted by
	// both the front-proxy (via kcp-merged-client-ca) and all shards directly. This is
	// required because the multicluster-provider connects to APIExport virtual workspace
	// URLs that point directly at shards, not through the front-proxy.
	// The server URL is rewritten below to point at the front-proxy.
	applyToKind(fmt.Sprintf(`apiVersion: operator.kcp.io/v1alpha1
kind: Kubeconfig
metadata:
  name: e2e-controller
  namespace: %[1]s
spec:
  username: "system:serviceaccount:%[2]s:dependency-controller"
  groups:
    - "system:authenticated"
    - "system:serviceaccounts"
    - "system:serviceaccounts:%[2]s"
  validity: 8766h
  secretRef:
    name: e2e-controller-kubeconfig
  target:
    rootShardRef:
      name: root`, kcpNamespace, depNamespace))

	applyToKind(fmt.Sprintf(`apiVersion: operator.kcp.io/v1alpha1
kind: Kubeconfig
metadata:
  name: e2e-webhook
  namespace: %[1]s
spec:
  username: "system:serviceaccount:%[2]s:dependency-webhook"
  groups:
    - "system:authenticated"
    - "system:serviceaccounts"
    - "system:serviceaccounts:%[2]s"
  validity: 8766h
  secretRef:
    name: e2e-webhook-kubeconfig
  target:
    rootShardRef:
      name: root`, kcpNamespace, depNamespace))

	// Wait for both kubeconfig secrets.
	for _, name := range []string{"e2e-controller-kubeconfig", "e2e-webhook-kubeconfig"} {
		waitFor(2*time.Minute, fmt.Sprintf("%s secret created", name), func() error {
			_, err := kindctlNoFail("-n", kcpNamespace, "get", "secret", name,
				"-o", "jsonpath={.data.kubeconfig}")

			return err
		})
	}

	// The kubeconfigs target the root shard. Extract the shard URL and rewrite
	// it to the front-proxy URL with the dep-ctrl workspace path. The client cert
	// from root-client-ca works for both front-proxy and direct shard access.
	fpHostname := fmt.Sprintf("kcp-front-proxy.%s.svc.cluster.local", kcpNamespace)
	kcRaw := kindctlSecret("e2e-controller-kubeconfig")
	kcBytes, err := decodeBase64(kcRaw)
	Expect(err).NotTo(HaveOccurred())
	shardURL := extractServerFromKubeconfig(kcBytes)

	// Determine the front-proxy port from the shard URL (both use 6443).
	parsed, err := url.Parse(shardURL)
	Expect(err).NotTo(HaveOccurred())
	fpPort := parsed.Port()
	if fpPort == "" {
		fpPort = "6443"
	}
	inClusterFPURL = "https://" + net.JoinHostPort(fpHostname, fpPort)
	depCtrlURL := inClusterFPURL + "/clusters/" + depCtrlPath

	// Rewrite kubeconfigs: shard URL -> front-proxy + workspace path.
	controllerKubeconfigPath = filepath.Join(tmpDir, "kcp-controller.kubeconfig")
	extractAndRewriteKubeconfig("e2e-controller-kubeconfig", controllerKubeconfigPath,
		shardURL, depCtrlURL)

	webhookKubeconfigPath = filepath.Join(tmpDir, "kcp-webhook.kubeconfig")
	extractAndRewriteKubeconfig("e2e-webhook-kubeconfig", webhookKubeconfigPath,
		shardURL, depCtrlURL)
}

// extractAndRewriteKubeconfig extracts a kubeconfig from a secret, rewrites the
// server URL, and writes it to the given path.
func extractAndRewriteKubeconfig(secretName, outputPath, oldURL, newURL string) {
	GinkgoHelper()
	kcRaw := kindctlSecret(secretName)
	kcBytes, err := decodeBase64(kcRaw)
	Expect(err).NotTo(HaveOccurred())

	rewritten := string(kcBytes)

	// kcp-operator generates two contexts: "base" (bare front-proxy URL) and
	// "default" (front-proxy URL + /clusters/root). We rewrite the base URL to
	// include the workspace path, but this corrupts the "default" entry with a
	// double /clusters/ path. Switch to the "base" context which has the correct URL.
	rewritten = strings.ReplaceAll(rewritten, oldURL, newURL)
	rewritten = strings.ReplaceAll(rewritten, "current-context: default", "current-context: base")

	Expect(os.WriteFile(outputPath, []byte(rewritten), 0o600)).To(Succeed())
}

// decodeBase64 decodes a base64-encoded string.
func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.TrimSpace(s))
}

// bootstrapRBAC uses the admin kubeconfig (system:kcp:admin) to create
// RBAC for the controller and webhook identities.
func bootstrapRBAC() {
	// Apply RBAC in the root workspace via front-proxy.
	run(kubectlBin, "--kubeconfig", kcpHostKubeconfig,
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root", frontProxyNodePort),
		"apply", "-f", filepath.Join(fixturesDir, "root-rbac-bootstrap.yaml"))

	// Apply RBAC in the dep-ctrl workspace (apiexportendpointslices read).
	run(kubectlBin, "--kubeconfig", kcpHostKubeconfig,
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root:%s", frontProxyNodePort, wsDepCtrl),
		"apply", "-f", filepath.Join(fixturesDir, "depctrl-rbac-bootstrap.yaml"))

	// Apply RBAC in consumer workspaces so the webhook can list dependent
	// resources directly when checking deletion protection.
	for _, ws := range []string{wsConsumer1, wsConsumer2} {
		run(kubectlBin, "--kubeconfig", kcpHostKubeconfig,
			"--server", fmt.Sprintf("https://localhost:%s/clusters/root:%s", frontProxyNodePort, ws),
			"apply", "-f", filepath.Join(fixturesDir, "consumer-rbac-bootstrap.yaml"))
	}
}

// extractServerFromKubeconfig extracts the server URL from a kubeconfig YAML.
func extractServerFromKubeconfig(kubeconfig []byte) string {
	// Simple regex extraction — avoids pulling in k8s.io/client-go/tools/clientcmd.
	re := regexp.MustCompile(`server:\s*(https?://\S+)`)
	m := re.FindSubmatch(kubeconfig)
	if len(m) < 2 {
		Fail("could not extract server URL from kubeconfig")
	}

	return string(m[1])
}

// portForwardRe matches the "Forwarding from 127.0.0.1:PORT -> ..." line.
func buildAndLoadImage() {
	run(dockerBin, "build", "-t", imageName, rootDir)
	run(kindBin, "load", "docker-image", imageName, "--name", kindClusterName)
}

func setupKCPWorkspaces() {
	// Label the secondary shard so we can pin consumer2 to it.
	// The kcp-operator registers a kcp Shard object named after the CR.
	waitFor(time.Minute, "shard1 kcp object exists", func() error {
		_, err := kcpctlRootNoFail("get", "shard", "shard1")
		return err
	})
	run(kubectlBin, "--kubeconfig", kcpHostKubeconfig,
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root", frontProxyNodePort),
		"label", "shard", "shard1", "e2e-target=shard1", "--overwrite")

	// Create workspaces. consumer2 is pinned to shard1 via location selector.
	for _, ws := range []string{wsDepCtrl, wsNetworkProvider, wsComputeProvider, wsConsumer1} {
		createWorkspace(ws, "")
	}
	createWorkspace(wsConsumer2, "shard1")

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

// createWorkspace creates a kcp workspace under root. If shardTarget is non-empty,
// the workspace is pinned to that shard via spec.location.selector.
func createWorkspace(name, shardTarget string) {
	GinkgoHelper()
	var manifest string
	if shardTarget != "" {
		manifest = fmt.Sprintf(`apiVersion: tenancy.kcp.io/v1alpha1
kind: Workspace
metadata:
  name: %s
spec:
  location:
    selector:
      matchLabels:
        e2e-target: %s`, name, shardTarget)
	} else {
		manifest = fmt.Sprintf(`apiVersion: tenancy.kcp.io/v1alpha1
kind: Workspace
metadata:
  name: %s`, name)
	}

	cmd := exec.CommandContext(context.Background(), kubectlBin,
		"--kubeconfig", kcpHostKubeconfig,
		"--server", fmt.Sprintf("https://localhost:%s/clusters/root", frontProxyNodePort),
		"apply", "-f", "-",
	)
	cmd.Stdin = strings.NewReader(manifest)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.Run() //nolint:errcheck // ignore AlreadyExists
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
		"--set", "kcpBaseHost="+inClusterFPURL,
		"--wait", "--timeout", "120s",
	)
}

// applyToKind applies a YAML manifest to the kind cluster.
func applyToKind(manifest string) {
	GinkgoHelper()
	cmd := exec.CommandContext(context.Background(), kubectlBin,
		"--context", "kind-"+kindClusterName, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	Expect(cmd.Run()).To(Succeed(), "applying manifest: %s", buf.String())
}
