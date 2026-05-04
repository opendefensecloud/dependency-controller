// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"os"
	"strings"

	"github.com/kcp-dev/multicluster-provider/apiexport"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
	"go.opendefense.cloud/dependency-controller/internal/kcp"
	"go.opendefense.cloud/dependency-controller/internal/webhook"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	utilruntime.Must(apisv1alpha1.AddToScheme(scheme))
	utilruntime.Must(corev1alpha1.AddToScheme(scheme))
	utilruntime.Must(tenancyv1alpha1.AddToScheme(scheme))
}

func main() {
	var apiExportName string
	var webhookPort int
	var tlsCertDir string
	var healthProbeBindAddress string
	flag.StringVar(&apiExportName, "api-export-name", "dependencies.opendefense.cloud", "Name of the dependency-controller's APIExport")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port for the webhook server")
	flag.StringVar(&tlsCertDir, "tls-cert-dir", "/etc/webhook-tls", "Directory containing tls.crt and tls.key for the webhook server")
	flag.StringVar(&healthProbeBindAddress, "health-probe-bind-address", ":8081", "Address to bind the health probe endpoint")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	cfg := ctrl.GetConfigOrDie()

	// Derive front-proxy base config by stripping the /clusters/... workspace
	// path from the kubeconfig host. This base URL is used by the webhook to
	// construct per-request clients targeting specific consumer workspaces.
	baseCfg := rest.CopyConfig(cfg)
	if idx := strings.Index(baseCfg.Host, "/clusters/"); idx != -1 {
		baseCfg.Host = baseCfg.Host[:idx]
	}

	// Resolve the APIExportEndpointSlice name for the dep-ctrl's APIExport.
	directClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create client for endpoint slice discovery")
		os.Exit(1)
	}

	ess, err := kcp.FindEndpointSlice(context.Background(), directClient, apiExportName)
	if err != nil {
		setupLog.Error(err, "unable to find APIExportEndpointSlice", "apiExport", apiExportName)
		os.Exit(1)
	}

	// Create apiexport provider for the dependency-controller's own APIExport.
	depCtrlProvider, err := apiexport.New(cfg, ess.Name, apiexport.Options{
		Scheme: scheme,
	})
	if err != nil {
		setupLog.Error(err, "unable to create apiexport provider")
		os.Exit(1)
	}

	webhookOpts := ctrlwebhook.Options{
		Port:    webhookPort,
		CertDir: tlsCertDir,
	}

	mgr, err := mcmanager.New(cfg, depCtrlProvider, manager.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: healthProbeBindAddress,
		WebhookServer:          ctrlwebhook.NewServer(webhookOpts),
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Create rule registry and cache manager.
	registry := webhook.NewRuleRegistry()

	cacheMgr := &webhook.RuleCacheManager{
		DepCtrlManager: mgr,
		Scheme:         scheme,
		APIExportName:  apiExportName,
		Registry:       registry,
	}

	if err := mcbuilder.ControllerManagedBy(mgr).
		Named("rule-cache-manager").
		For(&v1alpha1.DependencyRule{}).
		Complete(mcreconcile.Func(cacheMgr.Reconcile)); err != nil {
		setupLog.Error(err, "unable to create rule cache manager controller")
		os.Exit(1)
	}

	// Populate the registry before serving webhook requests. The runnable
	// executes after the manager starts and lists all existing DependencyRules
	// from the APIExport virtual workspace.
	initialized := make(chan struct{})
	if err := mgr.GetLocalManager().Add(manager.RunnableFunc(func(ctx context.Context) error {
		if err := cacheMgr.PopulateRegistry(ctx); err != nil {
			return err
		}
		close(initialized)

		return nil
	})); err != nil {
		setupLog.Error(err, "unable to add registry population runnable")
		os.Exit(1)
	}

	// Healthz always returns OK; readyz only after the registry is populated.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to add healthz check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("registry-populated", webhook.ReadyzCheck(initialized)); err != nil {
		setupLog.Error(err, "unable to add readyz check")
		os.Exit(1)
	}

	// Register webhook handler.
	validator := &webhook.DeletionValidator{
		Registry:    registry,
		Initialized: initialized,
		BaseConfig:  baseCfg,
	}
	mgr.GetWebhookServer().Register("/validate", &ctrlwebhook.Admission{Handler: validator})

	setupLog.Info("starting webhook server")

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "webhook server failed")
		os.Exit(1)
	}
}
