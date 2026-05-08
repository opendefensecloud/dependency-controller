// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kcp-dev/logicalcluster/v3"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var testScheme *runtime.Scheme

func init() {
	testScheme = runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(testScheme)
	_ = tenancyv1alpha1.AddToScheme(testScheme)
}

func newTestResolver(factory func(*rest.Config, client.Options) (client.Client, error)) *workspaceResolver {
	return &workspaceResolver{
		baseCfg:   &rest.Config{Host: "https://kcp.example"},
		scheme:    testScheme,
		newClient: factory,
	}
}

func TestWorkspaceResolver_EnsureResolved_ResolvesPath(t *testing.T) {
	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithLists(&tenancyv1alpha1.WorkspaceList{
			Items: []tenancyv1alpha1.Workspace{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "compute-provider"},
					Spec:       tenancyv1alpha1.WorkspaceSpec{Cluster: "abc123"},
				},
			},
		}).
		Build()

	r := newTestResolver(func(*rest.Config, client.Options) (client.Client, error) {
		return fakeClient, nil
	})

	if err := r.ensureResolved(context.Background(), []string{"root:compute-provider"}); err != nil {
		t.Fatalf("ensureResolved: %v", err)
	}

	if got := r.cache["root:compute-provider"]; got != "abc123" {
		t.Errorf("cache[root:compute-provider] = %q, want %q", got, "abc123")
	}
}

func TestWorkspaceResolver_EnsureResolved_UsesCache(t *testing.T) {
	r := newTestResolver(func(*rest.Config, client.Options) (client.Client, error) {
		t.Fatal("newClient should not be called for cached entries")
		return nil, nil
	})
	r.cache = map[string]string{"root:compute-provider": "abc123"}

	if err := r.ensureResolved(context.Background(), []string{"root:compute-provider"}); err != nil {
		t.Fatalf("ensureResolved: %v", err)
	}
}

func TestWorkspaceResolver_EnsureResolved_ListErrorPropagates(t *testing.T) {
	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("kaboom")
			},
		}).
		Build()

	r := newTestResolver(func(*rest.Config, client.Options) (client.Client, error) {
		return fakeClient, nil
	})

	err := r.ensureResolved(context.Background(), []string{"root:compute-provider"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "list workspaces in root") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestWorkspaceResolver_EnsureResolved_RoutesPerParent verifies that paths
// under different parent workspaces each trigger a list against the matching
// parent: the factory dispatches on cfg.Host (which the production code
// builds as baseCfg.Host + logicalcluster.NewPath(parent).RequestPath()).
func TestWorkspaceResolver_EnsureResolved_RoutesPerParent(t *testing.T) {
	const baseHost = "https://kcp.example"

	clientsByHost := map[string]client.Client{
		baseHost + logicalcluster.NewPath("root:org-a").RequestPath(): fake.NewClientBuilder().
			WithScheme(testScheme).
			WithLists(&tenancyv1alpha1.WorkspaceList{
				Items: []tenancyv1alpha1.Workspace{{
					ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
					Spec:       tenancyv1alpha1.WorkspaceSpec{Cluster: "cluster-a"},
				}, {
					ObjectMeta: metav1.ObjectMeta{Name: "team-b"},
					Spec:       tenancyv1alpha1.WorkspaceSpec{Cluster: "cluster-b"},
				}},
			}).Build(),
		baseHost + logicalcluster.NewPath("root:org-b").RequestPath(): fake.NewClientBuilder().
			WithScheme(testScheme).
			WithLists(&tenancyv1alpha1.WorkspaceList{
				Items: []tenancyv1alpha1.Workspace{{
					ObjectMeta: metav1.ObjectMeta{Name: "team-y"},
					Spec:       tenancyv1alpha1.WorkspaceSpec{Cluster: "cluster-y"},
				}},
			}).Build(),
	}

	calls := map[string]int{}
	factory := func(cfg *rest.Config, _ client.Options) (client.Client, error) {
		calls[cfg.Host]++
		c, ok := clientsByHost[cfg.Host]
		if !ok {
			t.Errorf("unexpected host %q", cfg.Host)
			return nil, errors.New("unexpected host")
		}

		return c, nil
	}

	r := &workspaceResolver{
		baseCfg:   &rest.Config{Host: baseHost},
		scheme:    testScheme,
		newClient: factory,
	}

	paths := []string{"root:org-a:team-a", "root:org-b:team-y"}
	if err := r.ensureResolved(context.Background(), paths); err != nil {
		t.Fatalf("ensureResolved: %v", err)
	}

	if got := r.cache["root:org-a:team-a"]; got != "cluster-a" {
		t.Errorf("cache[root:org-a:team-a] = %q, want cluster-a", got)
	}
	// test if workspace is in cache which was not requested but is in the same parent workspace,
	// to verify that the list result is cached for the entire parent workspace, not just the requested path
	if got := r.cache["root:org-a:team-b"]; got != "cluster-b" {
		t.Errorf("cache[root:org-a:team-b] = %q, want cluster-b", got)
	}
	if got := r.cache["root:org-b:team-y"]; got != "cluster-y" {
		t.Errorf("cache[root:org-b:team-y] = %q, want cluster-y", got)
	}
	if len(calls) != 2 {
		t.Errorf("expected exactly 2 distinct hosts called, got %v", calls)
	}
}

func TestWorkspaceResolver_EnsureResolved_EmptyInputIsNoop(t *testing.T) {
	r := newTestResolver(func(*rest.Config, client.Options) (client.Client, error) {
		t.Fatal("newClient should not be called for empty input")
		return nil, nil
	})

	if err := r.ensureResolved(context.Background(), nil); err != nil {
		t.Fatalf("ensureResolved: %v", err)
	}
}
