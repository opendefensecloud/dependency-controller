// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"sync"

	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
)

// PermissionClaimReconciler watches DependencyRules and dynamically manages
// permissionClaims on the dep-ctrl APIExport. For each unique dependent
// resource type referenced by any active DependencyRule, a permissionClaim is
// added so the webhook server can watch those resources through the dep-ctrl
// virtual workspace.
type PermissionClaimReconciler struct {
	// DepCtrlManager is the multicluster manager for the dep-ctrl's APIExport.
	DepCtrlManager mcmanager.Manager

	// APIExportName is the name of the dep-ctrl APIExport.
	APIExportName string

	// mu protects trackedRules.
	mu sync.Mutex
	// trackedRules maps rule key (clusterName/ruleName) to dependent {group, resource}.
	trackedRules map[string]groupResource
}

// groupResource is a simple key for deduplication.
type groupResource struct {
	Group    string
	Resource string
}

// Reconcile ensures the dep-ctrl APIExport has permissionClaims for all
// dependent resource types referenced by active DependencyRules.
func (r *PermissionClaimReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("rule", req.Name, "cluster", req.ClusterName)

	cl, err := r.DepCtrlManager.GetCluster(ctx, req.ClusterName)
	if err != nil {
		logger.Error(err, "failed to get cluster")
		return ctrl.Result{}, err
	}

	key := string(req.ClusterName) + "/" + req.Name

	var rule v1alpha1.DependencyRule
	if err := cl.GetClient().Get(ctx, client.ObjectKey{Name: req.Name}, &rule); err != nil {
		if client.IgnoreNotFound(err) == nil {
			r.removeRule(key)
		} else {
			return ctrl.Result{}, err
		}
	} else {
		r.trackRule(key, groupResource{
			Group:    rule.Spec.Dependent.Group,
			Resource: rule.Spec.Dependent.Resource,
		})
	}

	// Reconcile claims on the APIExport.
	if err := r.reconcileAPIExport(ctx); err != nil {
		logger.Error(err, "failed to reconcile APIExport permissionClaims")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *PermissionClaimReconciler) trackRule(key string, gr groupResource) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.trackedRules == nil {
		r.trackedRules = make(map[string]groupResource)
	}

	r.trackedRules[key] = gr
}

func (r *PermissionClaimReconciler) removeRule(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.trackedRules, key)
}

// desiredClaims returns the set of unique {group, resource} pairs across
// all tracked rules.
func (r *PermissionClaimReconciler) desiredClaims() map[groupResource]struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()

	desired := make(map[groupResource]struct{}, len(r.trackedRules))
	for _, gr := range r.trackedRules {
		desired[gr] = struct{}{}
	}

	return desired
}

// reconcileAPIExport reads the dep-ctrl APIExport, ensures it has the right
// permissionClaims, and updates it if needed.
func (r *PermissionClaimReconciler) reconcileAPIExport(ctx context.Context) error {
	localClient := r.DepCtrlManager.GetLocalManager().GetClient()

	var export apisv1alpha2.APIExport
	if err := localClient.Get(ctx, client.ObjectKey{Name: r.APIExportName}, &export); err != nil {
		return fmt.Errorf("getting APIExport %s: %w", r.APIExportName, err)
	}

	desired := r.desiredClaims()
	if !r.updateClaims(&export, desired) {
		return nil
	}

	if err := localClient.Update(ctx, &export); err != nil {
		return fmt.Errorf("updating APIExport %s: %w", r.APIExportName, err)
	}

	log.FromContext(ctx).WithName("permissionclaim").Info("updated APIExport permissionClaims",
		"claimCount", len(export.Spec.PermissionClaims))

	return nil
}

// updateClaims ensures the APIExport has exactly the right set of
// permissionClaims: static claims (like VWC) are preserved, dynamic
// claims for dependent resource types are added/removed as needed.
// Returns true if the export was modified.
func (r *PermissionClaimReconciler) updateClaims(export *apisv1alpha2.APIExport, desired map[groupResource]struct{}) bool {
	// Build a set of currently present dynamic claims.
	currentDynamic := make(map[groupResource]struct{})
	for _, claim := range export.Spec.PermissionClaims {
		gr := groupResource{Group: claim.Group, Resource: claim.Resource}
		if !isStaticClaim(claim) {
			currentDynamic[gr] = struct{}{}
		}
	}

	// Check if the desired set matches the current dynamic set.
	if len(desired) == len(currentDynamic) {
		allMatch := true
		for gr := range desired {
			if _, ok := currentDynamic[gr]; !ok {
				allMatch = false
				break
			}
		}
		if allMatch {
			return false
		}
	}

	// Rebuild: keep static claims, replace dynamic claims with desired.
	var newClaims []apisv1alpha2.PermissionClaim
	for _, claim := range export.Spec.PermissionClaims {
		if isStaticClaim(claim) {
			newClaims = append(newClaims, claim)
		}
	}

	for gr := range desired {
		newClaims = append(newClaims, apisv1alpha2.PermissionClaim{
			GroupResource: apisv1alpha2.GroupResource{
				Group:    gr.Group,
				Resource: gr.Resource,
			},
			Verbs: []string{"get", "list", "watch"},
		})
	}

	// Sort for deterministic output.
	slices.SortFunc(newClaims, func(a, b apisv1alpha2.PermissionClaim) int {
		if a.Group != b.Group {
			if a.Group < b.Group {
				return -1
			}

			return 1
		}
		if a.Resource != b.Resource {
			if a.Resource < b.Resource {
				return -1
			}

			return 1
		}

		return 0
	})

	export.Spec.PermissionClaims = newClaims

	return true
}

// isStaticClaim returns true for permissionClaims that are managed statically
// (declared in the APIExport YAML) rather than dynamically by this reconciler.
func isStaticClaim(claim apisv1alpha2.PermissionClaim) bool {
	return claim.Group == "admissionregistration.k8s.io" && claim.Resource == "validatingwebhookconfigurations"
}
