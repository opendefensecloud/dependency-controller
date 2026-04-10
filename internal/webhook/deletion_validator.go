package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/kcp-dev/logicalcluster/v3"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	v1alpha1 "go.opendefense.cloud/dependency-controller/api/v1alpha1"
)

// DeletionValidator is a validating admission webhook handler that blocks
// deletion of resources that have active Dependency objects pointing to them.
//
// It is multicluster-aware: it extracts the logical cluster name from the
// object being deleted (via the kcp.io/cluster annotation) and uses the
// dep-ctrl manager's cluster client to list Dependencies in that workspace.
type DeletionValidator struct {
	Manager mcmanager.Manager
}

func (v *DeletionValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues("resource", req.Resource, "name", req.Name, "namespace", req.Namespace)

	if req.Operation != "DELETE" {
		return admission.Allowed("")
	}

	// Allow deletion if the resource has the skip-protection annotation.
	if hasSkipProtection(req) {
		logger.Info("skip-protection annotation present, allowing deletion")
		return admission.Allowed("skip-protection annotation present")
	}

	// Extract the logical cluster name from the OldObject's kcp.io/cluster annotation.
	clusterName, err := clusterFromRequest(req)
	if err != nil {
		logger.Error(err, "failed to extract cluster from admission request")
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("failed to extract cluster: %w", err))
	}

	cluster, err := v.Manager.GetCluster(ctx, clusterName)
	if err != nil {
		logger.Error(err, "failed to get cluster", "cluster", clusterName)
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("failed to get cluster %s: %w", clusterName, err))
	}
	c := cluster.GetClient()

	// List all Dependency objects across all namespaces in this workspace.
	// We don't filter by namespace because the dependent and dependency may
	// be in different namespaces.
	var deps v1alpha1.DependencyList
	if err := c.List(ctx, &deps); err != nil {
		logger.Error(err, "failed to list Dependency objects")
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("failed to check dependencies: %w", err))
	}

	var blockers []string
	for _, dep := range deps.Items {
		ref := dep.Spec.Dependency
		if ref.Group == req.Resource.Group &&
			ref.Resource == req.Resource.Resource &&
			ref.Name == req.Name {
			blockers = append(blockers, fmt.Sprintf("%s/%s", dep.Spec.Dependent.Resource, dep.Spec.Dependent.Name))
		}
	}

	if len(blockers) > 0 {
		msg := fmt.Sprintf("cannot delete %s/%s: still referenced by %s",
			req.Resource.Resource, req.Name, strings.Join(blockers, ", "))
		logger.Info("deletion blocked", "blockers", blockers)
		return admission.Denied(msg)
	}

	return admission.Allowed("")
}

// hasSkipProtection checks if the resource being deleted has the
// dependencies.opendefense.cloud/skip-protection annotation set to "true".
// This provides an escape hatch for operators to force-delete resources
// when the normal dependency lifecycle has broken down.
func hasSkipProtection(req admission.Request) bool {
	raw := req.OldObject.Raw
	if len(raw) == 0 {
		raw = req.Object.Raw
	}
	if len(raw) == 0 {
		return false
	}

	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw, &obj.Object); err != nil {
		return false
	}

	annotations := obj.GetAnnotations()
	return annotations["dependencies.opendefense.cloud/skip-protection"] == "true"
}

// clusterFromRequest extracts the logical cluster name from the admission
// request's OldObject (for DELETE) or Object (for other operations).
func clusterFromRequest(req admission.Request) (string, error) {
	raw := req.OldObject.Raw
	if len(raw) == 0 {
		raw = req.Object.Raw
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("no object or old object in admission request")
	}

	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw, &obj.Object); err != nil {
		return "", fmt.Errorf("unmarshaling object: %w", err)
	}

	name := logicalcluster.From(obj)
	if name.Empty() {
		return "", fmt.Errorf("object has no %s annotation", logicalcluster.AnnotationKey)
	}
	return name.String(), nil
}
