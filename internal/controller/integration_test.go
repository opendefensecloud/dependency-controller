// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/kcp-dev/multicluster-provider/apiexport"
	clusterclient "github.com/kcp-dev/multicluster-provider/client"
	"github.com/kcp-dev/multicluster-provider/envtest"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	"github.com/kcp-dev/sdk/apis/core"
	registrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
	"go.opendefense.cloud/dependency-controller/internal/controller"
	depwebhook "go.opendefense.cloud/dependency-controller/internal/webhook"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Dependency Controller", Ordered, func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc

		cli             clusterclient.ClusterClient
		depCtrlPath     logicalcluster.Path
		networkProvPath logicalcluster.Path
		computeProvPath logicalcluster.Path
		consumer1Path   logicalcluster.Path
		consumer2Path   logicalcluster.Path
		pathVars        map[string]string
	)

	BeforeAll(func() {
		ctx, cancel = context.WithCancel(context.Background())

		var err error
		cli, err = clusterclient.New(kcpConfig, client.Options{})
		Expect(err).NotTo(HaveOccurred())

		// --- Create 5 workspaces ---

		_, depCtrlPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("dep-ctrl"))
		_, networkProvPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("network-provider"))
		_, computeProvPath = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("compute-provider"))
		_, consumer1Path = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("consumer1"))
		_, consumer2Path = envtest.NewWorkspaceFixture(GinkgoT(), cli, core.RootCluster.Path(), envtest.WithNamePrefix("consumer2"))

		// === Dep-Ctrl Workspace: APIExport for DependencyRule ===

		applyFixtures(ctx, cli, depCtrlPath,
			"../../config/kcp/apiresourceschema-dependencyrules.dependencies.opendefense.cloud.yaml",
			"../../config/kcp/apiexport-dependencies.opendefense.cloud.yaml",
		)

		// === Network Provider Workspace: APIExport for VPCs ===

		applyFixtures(ctx, cli, networkProvPath,
			"../../test/fixtures/apiresourceschema-vpcs.yaml",
			"../../test/fixtures/apiexport-network.test.io.yaml",
		)

		// === Compute Provider Workspace: APIExport for VMs ===

		applyFixtures(ctx, cli, computeProvPath,
			"../../test/fixtures/apiresourceschema-virtualmachines.yaml",
			"../../test/fixtures/apiexport-compute.test.io.yaml",
		)

		// === APIBindings ===

		pathVars = map[string]string{
			"${DEP_CTRL_PATH}":         depCtrlPath.String(),
			"${NETWORK_PROVIDER_PATH}": networkProvPath.String(),
			"${COMPUTE_PROVIDER_PATH}": computeProvPath.String(),
		}

		// Both providers bind to dep-ctrl.
		for _, wsPath := range []logicalcluster.Path{computeProvPath, networkProvPath} {
			applyFixture(ctx, cli, wsPath, "../../test/fixtures/apibinding-dependencies.opendefense.cloud.yaml", pathVars)
		}

		// Consumer workspaces bind to network and compute exports.
		for _, cp := range []logicalcluster.Path{consumer1Path, consumer2Path} {
			applyFixture(ctx, cli, cp, "../../test/fixtures/apibinding-network.test.io.yaml", pathVars)
			applyFixture(ctx, cli, cp, "../../test/fixtures/apibinding-compute.test.io.yaml", pathVars)
		}

		// === Wait for all APIExportEndpointSlices to have endpoints ===

		for _, ep := range []struct {
			path logicalcluster.Path
			name string
		}{
			{depCtrlPath, "dependencies.opendefense.cloud"},
			{networkProvPath, "network.test.io"},
			{computeProvPath, "compute.test.io"},
		} {
			envtest.Eventually(GinkgoT(), func() (bool, string) {
				endpoints := &apisv1alpha1.APIExportEndpointSlice{}
				if err := cli.Cluster(ep.path).Get(ctx, client.ObjectKey{Name: ep.name}, endpoints); err != nil {
					return false, fmt.Sprintf("get %s/%s: %v", ep.path, ep.name, err)
				}

				return len(endpoints.Status.APIExportEndpoints) > 0, toYAML(endpoints)
			}, wait.ForeverTestTimeout, 100*time.Millisecond, "waiting for endpoints %s in %s", ep.name, ep.path)
		}

		// === Wait for all bindings to be bound ===

		waitForBinding(ctx, cli, computeProvPath, "dependencies.opendefense.cloud")
		waitForBinding(ctx, cli, networkProvPath, "dependencies.opendefense.cloud")
		for _, cp := range []logicalcluster.Path{consumer1Path, consumer2Path} {
			waitForBinding(ctx, cli, cp, "network.test.io")
			waitForBinding(ctx, cli, cp, "compute.test.io")
		}

		// === Wait for resources to be listable in consumer workspaces ===

		for _, cp := range []logicalcluster.Path{consumer1Path, consumer2Path} {
			for _, gvk := range []runtimeschema.GroupVersionKind{
				{Group: "network.test.io", Version: "v1", Kind: "VPCList"},
				{Group: "compute.test.io", Version: "v1", Kind: "VirtualMachineList"},
			} {
				envtest.Eventually(GinkgoT(), func() (bool, string) {
					u := &unstructured.UnstructuredList{}
					u.SetGroupVersionKind(gvk)
					if err := cli.Cluster(cp).List(ctx, u); err != nil {
						return false, fmt.Sprintf("list %s in %s: %v", gvk.Kind, cp, err)
					}

					return true, ""
				}, wait.ForeverTestTimeout, 100*time.Millisecond, "waiting for %s in %s", gvk.Kind, cp)
			}
		}

		// === Set up the webhook server ===

		caBundle, webhookURL := startWebhookServer(ctx)

		// === Set up the dependency-controller ===

		depCtrlConfig := rest.CopyConfig(kcpConfig)
		depCtrlConfig.Host += depCtrlPath.RequestPath()

		depCtrlProvider, err := apiexport.New(depCtrlConfig, "dependencies.opendefense.cloud", apiexport.Options{
			Scheme: scheme.Scheme,
		})
		Expect(err).NotTo(HaveOccurred())

		mgr, err := mcmanager.New(depCtrlConfig, depCtrlProvider, manager.Options{
			Scheme: scheme.Scheme,
		})
		Expect(err).NotTo(HaveOccurred())

		// Create the rule cache manager and registry.
		registry := depwebhook.NewRuleRegistry()

		cacheMgr := &depwebhook.RuleCacheManager{
			DepCtrlManager: mgr,
			BaseConfig:     kcpConfig,
			Scheme:         scheme.Scheme,
			APIExportName:  "dependencies.opendefense.cloud",
			Registry:       registry,
		}

		err = mcbuilder.ControllerManagedBy(mgr).
			Named("rule-cache-manager").
			For(&v1alpha1.DependencyRule{}).
			Complete(mcreconcile.Func(cacheMgr.Reconcile))
		Expect(err).NotTo(HaveOccurred())

		// Populate the registry before serving webhook requests.
		initialized := make(chan struct{})
		err = mgr.GetLocalManager().Add(manager.RunnableFunc(func(ctx context.Context) error {
			if err := cacheMgr.PopulateRegistry(ctx); err != nil {
				return err
			}
			close(initialized)

			return nil
		}))
		Expect(err).NotTo(HaveOccurred())

		// Create the controller-side reconciler (webhook install only, no RBAC in envtest).
		webhookInstaller := controller.NewWebhookInstaller(kcpConfig, webhookURL, caBundle)

		ruleReconciler := controller.NewDependencyRuleReconciler(mgr)
		ruleReconciler.BaseConfig = kcpConfig
		ruleReconciler.WebhookInstaller = webhookInstaller

		err = mcbuilder.ControllerManagedBy(mgr).
			Named("dependencyrule").
			For(&v1alpha1.DependencyRule{}).
			Complete(mcreconcile.Func(ruleReconciler.Reconcile))
		Expect(err).NotTo(HaveOccurred())

		// Wire up the webhook handler with the rule registry.
		webhookHandler = &depwebhook.DeletionValidator{Registry: registry, Initialized: initialized}

		// Start the manager.
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(ctx)).To(Succeed())
		}()

		// Wait for the rule registry to be populated before running tests.
		Eventually(func() bool {
			select {
			case <-initialized:
				return true
			default:
				return false
			}
		}, wait.ForeverTestTimeout, 100*time.Millisecond).Should(BeTrue(), "waiting for rule registry to be initialized")
	})

	AfterAll(func() {
		cancel()
	})

	Describe("dependency lifecycle", func() {
		It("should block VPC deletion when a VM references it", func() {
			By("creating a VPC in consumer1")
			applyFixture(ctx, cli, consumer1Path, "../../test/fixtures/vpc-my-vpc.yaml", nil)

			By("creating a DependencyRule in the compute-provider workspace")
			applyFixture(ctx, cli, computeProvPath, "../../test/fixtures/dependencyrule-vm-dependencies.yaml", pathVars)

			By("creating a VM that references the VPC in consumer1")
			applyFixture(ctx, cli, consumer1Path, "../../test/fixtures/vm-my-vm.yaml", nil)

			By("waiting for the webhook to block VPC deletion")
			envtest.Eventually(GinkgoT(), func() (bool, string) {
				vpcToDelete := loadFixture("../../test/fixtures/vpc-my-vpc.yaml", nil)
				err := cli.Cluster(consumer1Path).Delete(ctx, vpcToDelete)
				if err == nil {
					// The indexed cache hadn't synced the VM yet — recreate the VPC and retry.
					_ = cli.Cluster(consumer1Path).Create(ctx, loadFixture("../../test/fixtures/vpc-my-vpc.yaml", nil))
					return false, "deletion was not blocked, recreated VPC"
				}
				if apierrors.IsNotFound(err) {
					// VPC was deleted before webhook caught it — recreate and retry.
					_ = cli.Cluster(consumer1Path).Create(ctx, loadFixture("../../test/fixtures/vpc-my-vpc.yaml", nil))
					return false, "VPC not found, recreated"
				}
				if !apierrors.IsForbidden(err) {
					return false, fmt.Sprintf("unexpected error: %v", err)
				}
				if strings.Contains(err.Error(), "still referenced by") {
					return true, ""
				}

				return false, fmt.Sprintf("forbidden but not a dependency block: %v", err)
			}, wait.ForeverTestTimeout, 100*time.Millisecond, "waiting for webhook to block deletion")
		})

		It("should not affect consumer2 where there are no VMs", func() {
			By("creating a VPC in consumer2")
			applyFixture(ctx, cli, consumer2Path, "../../test/fixtures/vpc-isolated-vpc.yaml", nil)

			By("deleting the VPC in consumer2 — should succeed (no dependents)")
			Expect(cli.Cluster(consumer2Path).Delete(ctx, loadFixture("../../test/fixtures/vpc-isolated-vpc.yaml", nil))).To(Succeed())
		})

		It("should allow VPC deletion after the dependent VM is deleted", func() {
			By("deleting the VM in consumer1")
			Expect(cli.Cluster(consumer1Path).Delete(ctx, loadFixture("../../test/fixtures/vm-my-vm.yaml", nil))).To(Succeed())

			By("waiting for VPC deletion to be allowed")
			envtest.Eventually(GinkgoT(), func() (bool, string) {
				err := cli.Cluster(consumer1Path).Delete(ctx, loadFixture("../../test/fixtures/vpc-my-vpc.yaml", nil))
				if err != nil {
					return false, fmt.Sprintf("deletion still blocked: %v", err)
				}

				return true, ""
			}, wait.ForeverTestTimeout, 100*time.Millisecond, "waiting for VPC to be deletable")
		})

		It("should stop blocking after the DependencyRule is deleted", func() {
			By("creating a new VPC in consumer1")
			applyFixture(ctx, cli, consumer1Path, "../../test/fixtures/vpc-cleanup-vpc.yaml", nil)

			By("creating a VM referencing the VPC")
			applyFixture(ctx, cli, consumer1Path, "../../test/fixtures/vm-cleanup-vm.yaml", nil)

			By("waiting for the webhook to block VPC deletion")
			envtest.Eventually(GinkgoT(), func() (bool, string) {
				err := cli.Cluster(consumer1Path).Delete(ctx, loadFixture("../../test/fixtures/vpc-cleanup-vpc.yaml", nil))
				if err == nil {
					_ = cli.Cluster(consumer1Path).Create(ctx, loadFixture("../../test/fixtures/vpc-cleanup-vpc.yaml", nil))
					return false, "deletion was not blocked, recreated VPC"
				}
				if apierrors.IsNotFound(err) {
					_ = cli.Cluster(consumer1Path).Create(ctx, loadFixture("../../test/fixtures/vpc-cleanup-vpc.yaml", nil))
					return false, "VPC not found, recreated"
				}
				if apierrors.IsForbidden(err) && strings.Contains(err.Error(), "still referenced by") {
					return true, ""
				}

				return false, fmt.Sprintf("unexpected error: %v", err)
			}, wait.ForeverTestTimeout, 100*time.Millisecond, "waiting for webhook to block deletion")

			By("deleting the DependencyRule")
			Expect(cli.Cluster(computeProvPath).Delete(ctx, loadFixture("../../test/fixtures/dependencyrule-vm-dependencies.yaml", pathVars))).To(Succeed())

			By("verifying the webhook is removed from the network provider workspace")
			envtest.Eventually(GinkgoT(), func() (bool, string) {
				cfg := rest.CopyConfig(kcpConfig)
				cfg.Host += networkProvPath.RequestPath()
				c, err := client.New(cfg, client.Options{})
				if err != nil {
					return false, fmt.Sprintf("creating client: %v", err)
				}
				whCfg := &registrationv1.ValidatingWebhookConfiguration{}
				err = c.Get(ctx, types.NamespacedName{Name: "dependency-controller"}, whCfg)
				if apierrors.IsNotFound(err) {
					return true, ""
				}
				if err != nil {
					return false, fmt.Sprintf("get webhook: %v", err)
				}

				return false, "webhook still exists"
			}, wait.ForeverTestTimeout, 100*time.Millisecond, "waiting for webhook removal")

			By("verifying VPC deletion now succeeds")
			Expect(cli.Cluster(consumer1Path).Delete(ctx, loadFixture("../../test/fixtures/vpc-cleanup-vpc.yaml", nil))).To(Succeed())
		})
	})
})

// webhookHandler is set during BeforeAll and used by the webhook server.
var webhookHandler admission.Handler

// startWebhookServer generates a self-signed CA and server cert, starts an HTTPS
// server on a random port serving the DeletionValidator at /validate-deletion,
// and returns the PEM-encoded CA bundle and the webhook URL.
// The handler is wired up lazily via the package-level webhookHandler variable
// so that the manager can be created after the server starts.
func startWebhookServer(ctx context.Context) (caBundle []byte, url string) {
	// Generate self-signed CA.
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(1 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	caCert, err := x509.ParseCertificate(caCertDER)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})

	// Generate server cert signed by CA.
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"localhost"},
	}
	serverCertDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	tlsCert := tls.Certificate{
		Certificate: [][]byte{serverCertDER},
		PrivateKey:  serverKey,
	}

	// Start listener on a random port.
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	port := listener.Addr().(*net.TCPAddr).Port

	// Wire up the admission handler via a lazy wrapper that delegates to
	// the package-level webhookHandler (set once the manager is ready).
	mux := http.NewServeMux()
	mux.Handle("/validate-deletion", &webhook.Admission{Handler: admission.HandlerFunc(
		func(ctx context.Context, req admission.Request) admission.Response {
			if webhookHandler == nil {
				return admission.Allowed("webhook not ready")
			}

			return webhookHandler.Handle(ctx, req)
		},
	)})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		defer GinkgoRecover()
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			Fail(fmt.Sprintf("webhook server failed: %v", err))
		}
	}()
	go func() {
		<-ctx.Done()
		server.Close() //nolint:errcheck
	}()

	return caPEM, fmt.Sprintf("https://127.0.0.1:%d/validate-deletion", port)
}

// waitForBinding waits until an APIBinding reaches the Bound phase.
func waitForBinding(ctx context.Context, cli clusterclient.ClusterClient, wsPath logicalcluster.Path, bindingName string) {
	envtest.Eventually(GinkgoT(), func() (bool, string) {
		b := &apisv1alpha2.APIBinding{}
		if err := cli.Cluster(wsPath).Get(ctx, client.ObjectKey{Name: bindingName}, b); err != nil {
			return false, fmt.Sprintf("get: %v", err)
		}

		return b.Status.Phase == apisv1alpha2.APIBindingPhaseBound, fmt.Sprintf("phase: %s", b.Status.Phase)
	}, wait.ForeverTestTimeout, 100*time.Millisecond, "waiting for binding %s in %s", bindingName, wsPath)
}

func toYAML(obj any) string {
	data, err := yaml.Marshal(obj)
	if err != nil {
		return fmt.Sprintf("<marshal error: %v>", err)
	}

	return string(data)
}

// loadFixture reads a YAML fixture file, performs placeholder substitution
// from the replacements map, and returns the object as an unstructured resource.
// For typed resources (APIResourceSchema, APIExport, APIBinding, DependencyRule),
// it unmarshals into the correct Go type first to ensure validation.
func loadFixture(path string, replacements map[string]string) client.Object {
	raw, err := os.ReadFile(path)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "reading fixture %s", path)

	content := string(raw)
	for k, v := range replacements {
		content = strings.ReplaceAll(content, k, v)
	}
	data := []byte(content)

	var meta metav1.TypeMeta
	ExpectWithOffset(1, yaml.Unmarshal(data, &meta)).To(Succeed(), "parsing kind from %s", path)

	switch meta.Kind {
	case "APIResourceSchema":
		o := &apisv1alpha1.APIResourceSchema{}
		ExpectWithOffset(1, yaml.Unmarshal(data, o)).To(Succeed(), "unmarshaling %s", path)

		return o
	case "APIExport":
		o := &apisv1alpha2.APIExport{}
		ExpectWithOffset(1, yaml.Unmarshal(data, o)).To(Succeed(), "unmarshaling %s", path)

		return o
	case "APIBinding":
		o := &apisv1alpha2.APIBinding{}
		ExpectWithOffset(1, yaml.Unmarshal(data, o)).To(Succeed(), "unmarshaling %s", path)

		return o
	case "DependencyRule":
		o := &v1alpha1.DependencyRule{}
		ExpectWithOffset(1, yaml.Unmarshal(data, o)).To(Succeed(), "unmarshaling %s", path)

		return o
	default:
		o := &unstructured.Unstructured{}
		ExpectWithOffset(1, yaml.Unmarshal(data, &o.Object)).To(Succeed(), "unmarshaling %s", path)

		return o
	}
}

// applyFixture loads a YAML fixture with placeholder substitution and creates
// it in the given workspace.
func applyFixture(ctx context.Context, cli clusterclient.ClusterClient, wsPath logicalcluster.Path, path string, replacements map[string]string) {
	obj := loadFixture(path, replacements)
	ExpectWithOffset(1, cli.Cluster(wsPath).Create(ctx, obj)).To(Succeed(), "creating fixture %s in %s", path, wsPath)
}

// applyFixtures loads multiple YAML fixtures and creates them in the given workspace.
func applyFixtures(ctx context.Context, cli clusterclient.ClusterClient, wsPath logicalcluster.Path, paths ...string) {
	for _, p := range paths {
		applyFixture(ctx, cli, wsPath, p, nil)
	}
}
