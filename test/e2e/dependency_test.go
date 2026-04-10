package e2e

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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/yaml"

	"github.com/kcp-dev/logicalcluster/v3"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	"github.com/kcp-dev/sdk/apis/core"

	"github.com/kcp-dev/multicluster-provider/apiexport"
	clusterclient "github.com/kcp-dev/multicluster-provider/client"
	"github.com/kcp-dev/multicluster-provider/envtest"

	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

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

		// === Dep-Ctrl Workspace: APIExport for DependencyRule + Dependency ===

		depRuleSchema := &apisv1alpha1.APIResourceSchema{
			ObjectMeta: metav1.ObjectMeta{Name: "v1alpha1.dependencyrules.dependencies.opendefense.cloud"},
			Spec: apisv1alpha1.APIResourceSchemaSpec{
				Group: "dependencies.opendefense.cloud",
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Kind: "DependencyRule", ListKind: "DependencyRuleList", Plural: "dependencyrules", Singular: "dependencyrule",
				},
				Scope: apiextensionsv1.ClusterScoped,
				Versions: []apisv1alpha1.APIResourceVersion{{
					Name: "v1alpha1", Storage: true, Served: true,
					Schema: runtime.RawExtension{
						Raw: []byte(`{"type":"object","properties":{"spec":{"type":"object","properties":{"dependent":{"type":"object","properties":{"apiExportRef":{"type":"object","properties":{"path":{"type":"string"},"name":{"type":"string"}}},"group":{"type":"string"},"version":{"type":"string"},"kind":{"type":"string"},"resource":{"type":"string"}}},"dependencies":{"type":"array","items":{"type":"object","properties":{"apiExportRef":{"type":"object","properties":{"path":{"type":"string"},"name":{"type":"string"}}},"group":{"type":"string"},"version":{"type":"string"},"resource":{"type":"string"},"fieldRef":{"type":"object","properties":{"path":{"type":"string"}}}}}}}}}}`),
					},
				}},
			},
		}
		Expect(cli.Cluster(depCtrlPath).Create(ctx, depRuleSchema)).To(Succeed())

		depSchema := &apisv1alpha1.APIResourceSchema{
			ObjectMeta: metav1.ObjectMeta{Name: "v1alpha1.dependencies.dependencies.opendefense.cloud"},
			Spec: apisv1alpha1.APIResourceSchemaSpec{
				Group: "dependencies.opendefense.cloud",
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Kind: "Dependency", ListKind: "DependencyList", Plural: "dependencies", Singular: "dependency",
				},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apisv1alpha1.APIResourceVersion{{
					Name: "v1alpha1", Storage: true, Served: true,
					Schema: runtime.RawExtension{
						Raw: []byte(`{"type":"object","properties":{"spec":{"type":"object","properties":{"dependent":{"type":"object","properties":{"group":{"type":"string"},"version":{"type":"string"},"resource":{"type":"string"},"name":{"type":"string"},"namespace":{"type":"string"}}},"dependency":{"type":"object","properties":{"group":{"type":"string"},"version":{"type":"string"},"resource":{"type":"string"},"name":{"type":"string"},"namespace":{"type":"string"}}},"ruleName":{"type":"string"}}}}}`),
						},
				}},
			},
		}
		Expect(cli.Cluster(depCtrlPath).Create(ctx, depSchema)).To(Succeed())

		depCtrlExport := &apisv1alpha2.APIExport{
			ObjectMeta: metav1.ObjectMeta{Name: "dependencies.opendefense.cloud"},
			Spec: apisv1alpha2.APIExportSpec{
				Resources: []apisv1alpha2.ResourceSchema{
					{Name: "dependencyrules", Group: "dependencies.opendefense.cloud", Schema: depRuleSchema.Name, Storage: apisv1alpha2.ResourceSchemaStorage{CRD: &apisv1alpha2.ResourceSchemaStorageCRD{}}},
					{Name: "dependencies", Group: "dependencies.opendefense.cloud", Schema: depSchema.Name, Storage: apisv1alpha2.ResourceSchemaStorage{CRD: &apisv1alpha2.ResourceSchemaStorageCRD{}}},
				},
			},
		}
		Expect(cli.Cluster(depCtrlPath).Create(ctx, depCtrlExport)).To(Succeed())

		// === Network Provider Workspace: APIExport for VPCs ===

		vpcSchema := &apisv1alpha1.APIResourceSchema{
			ObjectMeta: metav1.ObjectMeta{Name: "v1.vpcs.network.test.io"},
			Spec: apisv1alpha1.APIResourceSchemaSpec{
				Group: "network.test.io",
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Kind: "VPC", ListKind: "VPCList", Plural: "vpcs", Singular: "vpc",
				},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apisv1alpha1.APIResourceVersion{{
					Name: "v1", Storage: true, Served: true,
					Schema: runtime.RawExtension{
						Raw: []byte(`{"type":"object","properties":{"spec":{"type":"object","properties":{"cidr":{"type":"string"}}}}}`),
					},
				}},
			},
		}
		Expect(cli.Cluster(networkProvPath).Create(ctx, vpcSchema)).To(Succeed())

		networkExport := &apisv1alpha2.APIExport{
			ObjectMeta: metav1.ObjectMeta{Name: "network.test.io"},
			Spec: apisv1alpha2.APIExportSpec{
				Resources: []apisv1alpha2.ResourceSchema{
					{Name: "vpcs", Group: "network.test.io", Schema: vpcSchema.Name, Storage: apisv1alpha2.ResourceSchemaStorage{CRD: &apisv1alpha2.ResourceSchemaStorageCRD{}}},
				},
			},
		}
		Expect(cli.Cluster(networkProvPath).Create(ctx, networkExport)).To(Succeed())

		// === Compute Provider Workspace: APIExport for VMs ===

		vmSchema := &apisv1alpha1.APIResourceSchema{
			ObjectMeta: metav1.ObjectMeta{Name: "v1.virtualmachines.compute.test.io"},
			Spec: apisv1alpha1.APIResourceSchemaSpec{
				Group: "compute.test.io",
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Kind: "VirtualMachine", ListKind: "VirtualMachineList", Plural: "virtualmachines", Singular: "virtualmachine",
				},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apisv1alpha1.APIResourceVersion{{
					Name: "v1", Storage: true, Served: true,
					Schema: runtime.RawExtension{
						Raw: []byte(`{"type":"object","properties":{"spec":{"type":"object","properties":{"cpu":{"type":"integer"},"vpcRef":{"type":"object","properties":{"name":{"type":"string"}}}}}}}`),
					},
				}},
			},
		}
		Expect(cli.Cluster(computeProvPath).Create(ctx, vmSchema)).To(Succeed())

		computeExport := &apisv1alpha2.APIExport{
			ObjectMeta: metav1.ObjectMeta{Name: "compute.test.io"},
			Spec: apisv1alpha2.APIExportSpec{
				Resources: []apisv1alpha2.ResourceSchema{
					{Name: "virtualmachines", Group: "compute.test.io", Schema: vmSchema.Name, Storage: apisv1alpha2.ResourceSchemaStorage{CRD: &apisv1alpha2.ResourceSchemaStorageCRD{}}},
				},
			},
		}
		Expect(cli.Cluster(computeProvPath).Create(ctx, computeExport)).To(Succeed())

		// === APIBindings ===

		// Both providers bind to dep-ctrl.
		createBinding(ctx, cli, computeProvPath, "dependencies.opendefense.cloud", depCtrlPath)
		createBinding(ctx, cli, networkProvPath, "dependencies.opendefense.cloud", depCtrlPath)

		// Consumer workspaces bind to all three exports.
		for _, cp := range []logicalcluster.Path{consumer1Path, consumer2Path} {
			createBinding(ctx, cli, cp, "dependencies.opendefense.cloud", depCtrlPath)
			createBinding(ctx, cli, cp, "network.test.io", networkProvPath)
			createBinding(ctx, cli, cp, "compute.test.io", computeProvPath)
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
			ep := ep
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
			waitForBinding(ctx, cli, cp, "dependencies.opendefense.cloud")
			waitForBinding(ctx, cli, cp, "network.test.io")
			waitForBinding(ctx, cli, cp, "compute.test.io")
		}

		// === Wait for resources to be listable in consumer workspaces ===

		for _, cp := range []logicalcluster.Path{consumer1Path, consumer2Path} {
			for _, gvk := range []runtimeschema.GroupVersionKind{
				{Group: "network.test.io", Version: "v1", Kind: "VPCList"},
				{Group: "compute.test.io", Version: "v1", Kind: "VirtualMachineList"},
			} {
				gvk, cp := gvk, cp
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

		// Create the webhook installer and DependencyRule reconciler.
		webhookInstaller := controller.NewWebhookInstaller(kcpConfig, webhookURL, caBundle)

		ruleReconciler := controller.NewDependencyRuleReconciler(mgr, kcpConfig, scheme.Scheme)
		ruleReconciler.WebhookInstaller = webhookInstaller

		err = mcbuilder.ControllerManagedBy(mgr).
			Named("dependencyrule").
			For(&v1alpha1.DependencyRule{}).
			Complete(mcreconcile.Func(ruleReconciler.Reconcile))
		Expect(err).NotTo(HaveOccurred())

		// Wire up the webhook handler with the dep-ctrl manager.
		webhookHandler = &depwebhook.DeletionValidator{Manager: mgr}

		// Start the manager.
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(ctx)).To(Succeed())
		}()
	})

	AfterAll(func() {
		cancel()
	})

	Describe("dependency lifecycle", func() {
		It("should create Dependency objects when a VM references a VPC", func() {
			By("creating a VPC in consumer1")
			vpc := newUnstructured("network.test.io", "v1", "VPC", "my-vpc", "default")
			setNestedField(vpc, "10.0.0.0/16", "spec", "cidr")
			Expect(cli.Cluster(consumer1Path).Create(ctx, vpc)).To(Succeed())

			By("creating a DependencyRule in the compute-provider workspace")
			rule := &v1alpha1.DependencyRule{
				ObjectMeta: metav1.ObjectMeta{Name: "vm-dependencies"},
				Spec: v1alpha1.DependencyRuleSpec{
					Dependent: v1alpha1.DependentRef{
						APIExportRef: v1alpha1.APIExportReference{
							Path: computeProvPath.String(),
							Name: "compute.test.io",
						},
						Group:    "compute.test.io",
						Version:  "v1",
						Kind:     "VirtualMachine",
						Resource: "virtualmachines",
					},
					Dependencies: []v1alpha1.DependencyTarget{
						{
							APIExportRef: v1alpha1.APIExportReference{
								Path: networkProvPath.String(),
								Name: "network.test.io",
							},
							Group:    "network.test.io",
							Version:  "v1",
							Resource: "vpcs",
							FieldRef: v1alpha1.FieldReference{Path: ".spec.vpcRef.name"},
						},
					},
				},
			}
			Expect(cli.Cluster(computeProvPath).Create(ctx, rule)).To(Succeed())

			By("creating a VM that references the VPC in consumer1")
			vm := newUnstructured("compute.test.io", "v1", "VirtualMachine", "my-vm", "default")
			setNestedField(vm, "my-vpc", "spec", "vpcRef", "name")
			setNestedField(vm, int64(4), "spec", "cpu")
			Expect(cli.Cluster(consumer1Path).Create(ctx, vm)).To(Succeed())

			By("verifying a Dependency object is created in consumer1")
			envtest.Eventually(GinkgoT(), func() (bool, string) {
				var deps v1alpha1.DependencyList
				if err := cli.Cluster(consumer1Path).List(ctx, &deps, client.InNamespace("default")); err != nil {
					return false, fmt.Sprintf("list: %v", err)
				}
				for _, d := range deps.Items {
					if d.Spec.Dependent.Name == "my-vm" && d.Spec.Dependency.Name == "my-vpc" {
						return true, ""
					}
				}
				return false, fmt.Sprintf("no matching Dependency, got %d items", len(deps.Items))
			}, wait.ForeverTestTimeout, 100*time.Millisecond, "waiting for Dependency")
		})

		It("should not create Dependencies in consumer2 where there are no VMs", func() {
			By("verifying no Dependency objects exist in consumer2")
			var deps v1alpha1.DependencyList
			err := cli.Cluster(consumer2Path).List(ctx, &deps, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred())
			Expect(deps.Items).To(BeEmpty())
		})

		It("should block VPC deletion via webhook when a Dependency exists", func() {
			By("verifying a Dependency object exists in consumer1")
			envtest.Eventually(GinkgoT(), func() (bool, string) {
				var deps v1alpha1.DependencyList
				if err := cli.Cluster(consumer1Path).List(ctx, &deps, client.InNamespace("default")); err != nil {
					return false, fmt.Sprintf("list: %v", err)
				}
				for _, d := range deps.Items {
					if d.Spec.Dependency.Name == "my-vpc" {
						return true, ""
					}
				}
				return false, fmt.Sprintf("no Dependency for my-vpc, got %d items", len(deps.Items))
			}, wait.ForeverTestTimeout, 100*time.Millisecond, "waiting for Dependency to exist")

			By("attempting to delete the VPC in consumer1 — should be blocked by webhook")
			vpc := newUnstructured("network.test.io", "v1", "VPC", "my-vpc", "default")
			err := cli.Cluster(consumer1Path).Delete(ctx, vpc)
			Expect(err).To(HaveOccurred(), "expected deletion to be denied by webhook")
			Expect(err.Error()).To(ContainSubstring("my-vpc"))
		})

		It("should clean up Dependencies when the dependent VM is deleted", func() {
			By("deleting the VM in consumer1")
			vm := newUnstructured("compute.test.io", "v1", "VirtualMachine", "my-vm", "default")
			Expect(cli.Cluster(consumer1Path).Delete(ctx, vm)).To(Succeed())

			By("waiting for Dependency cleanup")
			envtest.Eventually(GinkgoT(), func() (bool, string) {
				var deps v1alpha1.DependencyList
				if err := cli.Cluster(consumer1Path).List(ctx, &deps, client.InNamespace("default")); err != nil {
					return false, fmt.Sprintf("list: %v", err)
				}
				for _, d := range deps.Items {
					if d.Spec.Dependent.Name == "my-vm" {
						return false, "Dependency for my-vm still exists"
					}
				}
				return true, ""
			}, wait.ForeverTestTimeout, 100*time.Millisecond, "waiting for cleanup")
		})

		It("should allow VPC deletion via webhook after Dependencies are cleaned up", func() {
			By("verifying no Dependencies reference the VPC")
			envtest.Eventually(GinkgoT(), func() (bool, string) {
				var deps v1alpha1.DependencyList
				if err := cli.Cluster(consumer1Path).List(ctx, &deps, client.InNamespace("default")); err != nil {
					return false, fmt.Sprintf("list: %v", err)
				}
				for _, d := range deps.Items {
					if d.Spec.Dependency.Name == "my-vpc" {
						return false, "Dependency for my-vpc still exists"
					}
				}
				return true, ""
			}, wait.ForeverTestTimeout, 100*time.Millisecond, "waiting for cleanup")

			By("deleting the VPC in consumer1 — should succeed now")
			vpc := newUnstructured("network.test.io", "v1", "VPC", "my-vpc", "default")
			Expect(cli.Cluster(consumer1Path).Delete(ctx, vpc)).To(Succeed())
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

	server := &http.Server{Handler: mux}
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

// createBinding creates an APIBinding in the given workspace.
func createBinding(ctx context.Context, cli clusterclient.ClusterClient, wsPath logicalcluster.Path, exportName string, exportPath logicalcluster.Path) {
	binding := &apisv1alpha2.APIBinding{
		ObjectMeta: metav1.ObjectMeta{Name: exportName},
		Spec: apisv1alpha2.APIBindingSpec{
			Reference: apisv1alpha2.BindingReference{
				Export: &apisv1alpha2.ExportBindingReference{
					Path: exportPath.String(),
					Name: exportName,
				},
			},
		},
	}
	ExpectWithOffset(1, cli.Cluster(wsPath).Create(ctx, binding)).To(Succeed())
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

func newUnstructured(group, version, kind, name, namespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(runtimeschema.GroupVersionKind{Group: group, Version: version, Kind: kind})
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

func setNestedField(u *unstructured.Unstructured, value interface{}, fields ...string) {
	ExpectWithOffset(1, unstructured.SetNestedField(u.Object, value, fields...)).To(Succeed())
}

func toYAML(obj interface{}) string {
	data, err := yaml.Marshal(obj)
	if err != nil {
		return fmt.Sprintf("<marshal error: %v>", err)
	}
	return string(data)
}
