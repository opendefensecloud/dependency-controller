// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"testing"

	"github.com/kcp-dev/multicluster-provider/envtest"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	env       *envtest.Environment
	kcpConfig *rest.Config
)

func init() {
	runtime.Must(apisv1alpha1.AddToScheme(scheme.Scheme))
	runtime.Must(apisv1alpha2.AddToScheme(scheme.Scheme))
	runtime.Must(corev1alpha1.AddToScheme(scheme.Scheme))
	runtime.Must(tenancyv1alpha1.AddToScheme(scheme.Scheme))
	runtime.Must(v1alpha1.AddToScheme(scheme.Scheme))
}

func TestController(t *testing.T) {
	RegisterFailHandler(Fail)

	var err error
	env = &envtest.Environment{}
	kcpConfig, err = env.Start()
	NewWithT(t).Expect(err).NotTo(HaveOccurred(), "failed to start kcp envtest environment")
	defer env.Stop() //nolint:errcheck

	RunSpecs(t, "Dependency Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	metricsserver.DefaultBindAddress = "0"
})

var _ = AfterSuite(func() {
	metricsserver.DefaultBindAddress = ":8080"
})
