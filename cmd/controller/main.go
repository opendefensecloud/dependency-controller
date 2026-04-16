// Copyright 2026 Open Defense and dependency-controller contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"os"

	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/kcp-dev/multicluster-provider/apiexport"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
	"go.opendefense.cloud/dependency-controller/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	utilruntime.Must(apisv1alpha1.AddToScheme(scheme))
	utilruntime.Must(corev1alpha1.AddToScheme(scheme))
	utilruntime.Must(tenancyv1alpha1.AddToScheme(scheme))
	utilruntime.Must(rbacv1.AddToScheme(scheme))
}

func main() {
	var apiExportName string
	var kcpBaseHost string
	var rootShardHost string
	var systemMasterPath string
	var serviceAccountName string
	var serviceAccountNamespace string
	var webhookURL string
	var webhookCABundlePath string
	var healthProbeBindAddress string
	flag.StringVar(&apiExportName, "api-export-name", "dependencies.opendefense.cloud", "Name of the dependency-controller's APIExport")
	flag.StringVar(&kcpBaseHost, "kcp-base-host", "", "Base kcp host URL (without workspace path). If empty, derived from kubeconfig.")
	flag.StringVar(&rootShardHost, "root-shard-host", "", "Direct URL of the root shard (bypasses kcp front proxy). Required for RBAC management in system:master.")
	flag.StringVar(&systemMasterPath, "system-master-path", "system:master", "Workspace path for system:master (for RBAC management)")
	flag.StringVar(&serviceAccountName, "service-account-name", "dependency-controller", "Service account name for RBAC binding")
	flag.StringVar(&serviceAccountNamespace, "service-account-namespace", "default", "Service account namespace for RBAC binding")
	flag.StringVar(&webhookURL, "webhook-url", "", "URL of the dependency-webhook server (e.g. https://dependency-webhook.ns.svc:443/validate)")
	flag.StringVar(&webhookCABundlePath, "webhook-ca-bundle-path", "", "Path to CA bundle PEM file for the webhook server's TLS certificate")
	flag.StringVar(&healthProbeBindAddress, "health-probe-bind-address", ":8081", "Address to bind the health probe endpoint")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	cfg := ctrl.GetConfigOrDie()

	// Derive base config (root kcp URL without workspace path).
	baseCfg := rest.CopyConfig(cfg)
	if kcpBaseHost != "" {
		baseCfg.Host = kcpBaseHost
	}

	// Create apiexport provider for the dependency-controller's own APIExport.
	depCtrlProvider, err := apiexport.New(cfg, apiExportName, apiexport.Options{
		Scheme: scheme,
	})
	if err != nil {
		setupLog.Error(err, "unable to create apiexport provider")
		os.Exit(1)
	}

	mgr, err := mcmanager.New(cfg, depCtrlProvider, manager.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: healthProbeBindAddress,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to add healthz check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to add readyz check")
		os.Exit(1)
	}

	// Create RBAC manager targeting system:master workspace via the root shard.
	rbacCfg := rest.CopyConfig(cfg)
	if rootShardHost != "" {
		rbacCfg.Host = rootShardHost
	}
	rbacCfg.Host += logicalcluster.NewPath(systemMasterPath).RequestPath()

	systemMasterClient, err := client.New(rbacCfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create system:master client")
		os.Exit(1)
	}

	rbacMgr := &controller.RBACManager{
		Client:                  systemMasterClient,
		ServiceAccountName:      serviceAccountName,
		ServiceAccountNamespace: serviceAccountNamespace,
	}

	// Register the multicluster DependencyRule reconciler.
	reconciler := controller.NewDependencyRuleReconciler(mgr)
	reconciler.RBACManager = rbacMgr

	// Wire up webhook installer if configured.
	if webhookURL != "" {
		var caBundle []byte
		if webhookCABundlePath != "" {
			caBundle, err = os.ReadFile(webhookCABundlePath)
			if err != nil {
				setupLog.Error(err, "unable to read webhook CA bundle", "path", webhookCABundlePath)
				os.Exit(1)
			}
		}
		reconciler.WebhookInstaller = controller.NewWebhookInstaller(baseCfg, webhookURL, caBundle)
	}

	if err := mcbuilder.ControllerManagedBy(mgr).
		Named("dependencyrule").
		For(&v1alpha1.DependencyRule{}).
		Complete(mcreconcile.Func(reconciler.Reconcile)); err != nil {
		setupLog.Error(err, "unable to create DependencyRule controller")
		os.Exit(1)
	}

	setupLog.Info("starting manager")

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager failed")
		os.Exit(1)
	}
}
