// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/kcp-dev/logicalcluster/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	// AnnotationSkipProtection is the annotation key that, when set to "true"
	// on a resource, causes the deletion webhook to skip protection checks.
	AnnotationSkipProtection = "dependencies.opendefense.cloud/skip-protection"
)

// ReadyzCheck returns a healthz.Checker that reports healthy once the given
// channel is closed (i.e., the rule registry has been populated).
func ReadyzCheck(initialized <-chan struct{}) func(*http.Request) error {
	return func(_ *http.Request) error {
		select {
		case <-initialized:
			return nil
		default:
			return fmt.Errorf("rule registry not yet populated")
		}
	}
}

// DeletionValidator is a validating admission webhook handler that blocks
// deletion of resources that have active dependents referencing them.
//
// It queries indexed caches maintained by the RuleCacheManager's per-rule
// multicluster managers. No Dependency marker objects are needed.
type DeletionValidator struct {
	Registry *RuleRegistry

	// Initialized is closed once the rule registry has been populated with
	// all existing DependencyRules. Until then, DELETE requests are denied
	// to prevent deletions slipping through before the registry is ready.
	Initialized <-chan struct{}
}

func (v *DeletionValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues("resource", req.Resource, "name", req.Name, "namespace", req.Namespace)

	if req.Operation != "DELETE" {
		return admission.Allowed("")
	}

	// Block all DELETE requests until the registry has been populated.
	select {
	case <-v.Initialized:
	default:
		return admission.Denied("dependency webhook not yet initialized, retry later")
	}

	// Parse the object for skip-protection annotation and cluster extraction.
	obj, err := objectFromRequest(req)
	if err != nil {
		logger.Error(err, "failed to parse object from admission request")
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("failed to parse object: %w", err))
	}

	// Allow deletion if the resource has the skip-protection annotation.
	if obj.GetAnnotations()[AnnotationSkipProtection] == "true" {
		logger.Info("skip-protection annotation present, allowing deletion")
		return admission.Allowed("skip-protection annotation present")
	}

	// Extract the logical cluster name from the kcp.io/cluster annotation.
	clusterName := logicalcluster.From(obj)
	if clusterName.Empty() {
		err := fmt.Errorf("object has no %s annotation", logicalcluster.AnnotationKey)
		logger.Error(err, "failed to extract cluster from admission request")

		return admission.Errored(http.StatusBadRequest, err)
	}

	// Find all rules that protect this resource type.
	targetGVR := schema.GroupVersionResource{
		Group:    req.Resource.Group,
		Version:  req.Resource.Version,
		Resource: req.Resource.Resource,
	}

	entries := v.Registry.FindByTargetGVR(targetGVR)
	if len(entries) == 0 {
		return admission.Allowed("")
	}

	// Query each matching rule's indexed cache for dependents referencing this resource.
	var blockers []string
	for _, entry := range entries {
		if !entry.State.IsReady() {
			msg := fmt.Sprintf("dependency check unavailable for rule %s: cache warming up, retry later", entry.Key)
			logger.Info(msg)

			return admission.Denied(msg)
		}

		cluster, err := entry.State.Manager.GetCluster(ctx, clusterName.String())
		if err != nil {
			// Cluster not known to this rule's manager — no dependents here.
			logger.V(1).Info("cluster not found in rule manager, skipping", "rule", entry.Key, "cluster", clusterName)
			continue
		}

		// Query the field index for dependents referencing the deleted resource name.
		// Scoped to the same namespace — cross-namespace references are not supported.
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(entry.State.DependentGVK.GroupVersion().WithKind(entry.State.DependentGVK.Kind + "List"))

		err = cluster.GetCache().List(ctx, list,
			client.MatchingFields{entry.MatchedField.FieldPath: req.Name},
			client.InNamespace(req.Namespace),
		)
		if err != nil {
			logger.Error(err, "failed to query indexed cache", "rule", entry.Key)
			return admission.Errored(http.StatusInternalServerError, fmt.Errorf("failed to check dependencies for rule %s: %w", entry.Key, err))
		}

		for _, item := range list.Items {
			blockers = append(blockers, fmt.Sprintf("%s/%s", entry.State.DependentGVK.Kind, item.GetName()))
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

// objectFromRequest extracts the unstructured object from the admission
// request's OldObject (for DELETE) or Object (for other operations).
func objectFromRequest(req admission.Request) (*unstructured.Unstructured, error) {
	raw := req.OldObject.Raw
	if len(raw) == 0 {
		raw = req.Object.Raw
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("no object or old object in admission request")
	}

	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw, &obj.Object); err != nil {
		return nil, fmt.Errorf("unmarshaling object: %w", err)
	}

	return obj, nil
}
