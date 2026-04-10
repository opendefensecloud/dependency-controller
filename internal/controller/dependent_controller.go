package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
	"go.opendefense.cloud/dependency-controller/internal/webhook"
)

// DependentReconciler watches a specific dependent resource type (e.g., VirtualMachines)
// and creates/deletes Dependency marker objects based on the references found in the dependent.
//
// It uses two managers:
//   - DependentManager: for reading dependent resources (via the dependent's APIExport VW)
//   - DepCtrlManager: for creating/deleting Dependency objects (via the dep-ctrl's APIExport VW)
type DependentReconciler struct {
	DepCtrlManager   mcmanager.Manager
	DependentManager mcmanager.Manager
	RuleName         string
	Dependent        schema.GroupVersionResource
	DependentKind    schema.GroupVersionKind
	Dependencies     []v1alpha1.DependencyTarget
}

func (r *DependentReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues(
		"rule", r.RuleName,
		"dependent", req.Name,
		"namespace", req.Namespace,
		"cluster", req.ClusterName,
	)

	// Get the cluster client from the dependent's APIExport manager (for reading the dependent).
	depCluster, err := r.DependentManager.GetCluster(ctx, req.ClusterName)
	if err != nil {
		logger.Error(err, "failed to get cluster from dependent manager")
		return ctrl.Result{}, err
	}
	depClient := depCluster.GetClient()

	// Get the cluster client from the dep-ctrl manager (for creating/deleting Dependency objects).
	ctrlCluster, err := r.DepCtrlManager.GetCluster(ctx, req.ClusterName)
	if err != nil {
		// The dep-ctrl manager may not have discovered this cluster yet. Requeue.
		logger.Info("dep-ctrl manager has not discovered cluster yet, requeueing")
		return ctrl.Result{Requeue: true}, nil
	}
	ctrlClient := ctrlCluster.GetClient()

	// Fetch the dependent resource as unstructured.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.DependentKind)

	err = depClient.Get(ctx, req.NamespacedName, obj)
	if apierrors.IsNotFound(err) {
		logger.Info("dependent resource deleted, cleaning up dependencies")
		return ctrl.Result{}, r.cleanupDependencies(ctx, ctrlClient, req.NamespacedName)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("fetching dependent: %w", err)
	}

	// For each dependency target in the rule, resolve the field path and
	// create or update the corresponding Dependency marker object.
	var desiredDeps []string
	for _, dep := range r.Dependencies {
		refName := webhook.ResolveFieldPath(obj.Object, dep.FieldRef.Path)
		if refName == "" {
			continue
		}

		depObjName := dependencyName(r.RuleName, req.Name, dep.Resource, refName)
		desiredDeps = append(desiredDeps, depObjName)

		dependency := &v1alpha1.Dependency{
			ObjectMeta: metav1.ObjectMeta{
				Name:      depObjName,
				Namespace: req.Namespace,
				Labels: map[string]string{
					"dependencies.opendefense.cloud/rule":           r.RuleName,
					"dependencies.opendefense.cloud/dependent-name": req.Name,
				},
			},
			Spec: v1alpha1.DependencySpec{
				Dependent: v1alpha1.ObjectReference{
					Group:     r.Dependent.Group,
					Version:   r.Dependent.Version,
					Resource:  r.Dependent.Resource,
					Name:      req.Name,
					Namespace: req.Namespace,
				},
				Dependency: v1alpha1.ObjectReference{
					Group:     dep.Group,
					Version:   dep.Version,
					Resource:  dep.Resource,
					Name:      refName,
					Namespace: req.Namespace,
				},
				RuleName: r.RuleName,
			},
		}

		existing := &v1alpha1.Dependency{}
		err := ctrlClient.Get(ctx, types.NamespacedName{
			Name:      depObjName,
			Namespace: req.Namespace,
		}, existing)

		if apierrors.IsNotFound(err) {
			logger.Info("creating Dependency", "dependency", depObjName, "target", refName)
			if err := ctrlClient.Create(ctx, dependency); err != nil {
				return ctrl.Result{}, fmt.Errorf("creating Dependency %s: %w", depObjName, err)
			}
		} else if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking Dependency %s: %w", depObjName, err)
		}
	}

	// Clean up Dependency objects that no longer match (e.g., reference changed).
	if err := r.cleanupStaleDependencies(ctx, ctrlClient, req.NamespacedName, desiredDeps); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// cleanupDependencies removes all Dependency objects created by this rule for a specific dependent.
func (r *DependentReconciler) cleanupDependencies(ctx context.Context, c client.Client, dependent types.NamespacedName) error {
	return c.DeleteAllOf(ctx, &v1alpha1.Dependency{},
		client.InNamespace(dependent.Namespace),
		client.MatchingLabels{
			"dependencies.opendefense.cloud/rule":           r.RuleName,
			"dependencies.opendefense.cloud/dependent-name": dependent.Name,
		},
	)
}

// cleanupStaleDependencies removes Dependency objects that are no longer desired.
func (r *DependentReconciler) cleanupStaleDependencies(
	ctx context.Context,
	c client.Client,
	dependent types.NamespacedName,
	desiredNames []string,
) error {
	var existing v1alpha1.DependencyList
	if err := c.List(ctx, &existing,
		client.InNamespace(dependent.Namespace),
		client.MatchingLabels{
			"dependencies.opendefense.cloud/rule":           r.RuleName,
			"dependencies.opendefense.cloud/dependent-name": dependent.Name,
		},
	); err != nil {
		return fmt.Errorf("listing existing dependencies: %w", err)
	}

	desired := make(map[string]struct{}, len(desiredNames))
	for _, n := range desiredNames {
		desired[n] = struct{}{}
	}

	for i := range existing.Items {
		if _, ok := desired[existing.Items[i].Name]; !ok {
			if err := c.Delete(ctx, &existing.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting stale Dependency %s: %w", existing.Items[i].Name, err)
			}
		}
	}
	return nil
}

// dependencyName generates a deterministic name for a Dependency object.
func dependencyName(ruleName, dependentName, depResource, depName string) string {
	name := fmt.Sprintf("%s--%s--%s.%s", ruleName, dependentName, depResource, depName)
	name = strings.ReplaceAll(name, "/", "-")
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}
