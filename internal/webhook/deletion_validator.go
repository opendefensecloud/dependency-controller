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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"go.opendefense.cloud/dependency-controller/internal/fieldpath"
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
// On each DELETE request it constructs a temporary dynamic client scoped to
// the consumer workspace (via the front-proxy) and lists dependent resources
// directly, filtering by field path to find references to the deleted resource.
type DeletionValidator struct {
	Registry *RuleRegistry

	// Initialized is closed once the rule registry has been populated with
	// all existing DependencyRules. Until then, DELETE requests are denied
	// to prevent deletions slipping through before the registry is ready.
	Initialized <-chan struct{}

	// BaseConfig is the front-proxy REST config without a workspace path.
	// Per-request clients are created by copying this config and setting
	// Host to {base}/clusters/{logicalClusterName}.
	BaseConfig *rest.Config
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

	// Build a dynamic client scoped to the consumer workspace.
	dynClient, err := v.workspaceClient(clusterName.String())
	if err != nil {
		logger.Error(err, "failed to create workspace client")
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("creating workspace client: %w", err))
	}

	// Query each matching rule's dependent resources in the consumer workspace.
	var blockers []string
	for _, entry := range entries {
		dependents, err := v.listDependents(ctx, dynClient, entry, req.Name, req.Namespace)
		if err != nil {
			logger.Error(err, "failed to query dependents", "rule", entry.Key)
			return admission.Errored(http.StatusInternalServerError, fmt.Errorf("checking dependencies for rule %s: %w", entry.Key, err))
		}

		blockers = append(blockers, dependents...)
	}

	if len(blockers) > 0 {
		msg := fmt.Sprintf("cannot delete %s/%s: still referenced by %s",
			req.Resource.Resource, req.Name, strings.Join(blockers, ", "))
		logger.Info("deletion blocked", "blockers", blockers)

		return admission.Denied(msg)
	}

	return admission.Allowed("")
}

// workspaceClient creates a dynamic client targeting a specific workspace
// through the front-proxy.
func (v *DeletionValidator) workspaceClient(clusterName string) (dynamic.Interface, error) {
	cfg := rest.CopyConfig(v.BaseConfig)
	cfg.Host = fmt.Sprintf("%s/clusters/%s", strings.TrimRight(cfg.Host, "/"), clusterName)

	return dynamic.NewForConfig(cfg)
}

// listDependents lists dependent resources in the consumer workspace and
// returns the names of those that reference the deleted resource via the
// rule's field path.
func (v *DeletionValidator) listDependents(
	ctx context.Context,
	dynClient dynamic.Interface,
	entry RuleEntry,
	targetName, namespace string,
) ([]string, error) {
	gvr := entry.State.DependentGVR

	var res dynamic.ResourceInterface
	if namespace != "" {
		res = dynClient.Resource(gvr).Namespace(namespace)
	} else {
		res = dynClient.Resource(gvr)
	}

	list, err := res.List(ctx, metav1.ListOptions{})
	if err != nil {
		// If the dependent resource type doesn't exist in this workspace
		// (e.g., consumer hasn't bound the provider APIExport), there are
		// no dependents — allow deletion.
		if errors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("listing %s: %w", gvr, err)
	}

	var blockers []string
	for _, item := range list.Items {
		val := fieldpath.Resolve(item.Object, entry.MatchedField.FieldPath)
		if val == targetName {
			blockers = append(blockers, fmt.Sprintf("%s/%s", entry.State.DependentGVK.Kind, item.GetName()))
		}
	}

	return blockers, nil
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
