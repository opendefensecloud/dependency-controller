// Copyright 2026 BWI GmbH and Dependency Controller contributors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster

// DependencyRule declares how a dependent resource type references other resource types.
// Created by API providers alongside their APIExport.
type DependencyRule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DependencyRuleSpec `json:"spec"`
}

// DependencyRuleSpec defines the dependent resource and its dependencies.
type DependencyRuleSpec struct {
	// Dependent identifies the resource type that holds references to other resources.
	Dependent DependentRef `json:"dependent"`

	// Dependencies lists the resource types that the dependent depends on,
	// including how each is referenced.
	Dependencies []DependencyTarget `json:"dependencies"`
}

// DependentRef identifies the dependent resource type and its APIExport.
// The APIExport must be in the same workspace as the DependencyRule.
type DependentRef struct {
	// APIExportName is the name of the APIExport that provides this resource type.
	// The APIExport must be in the same workspace as the DependencyRule — the
	// workspace path is derived from the rule's location, not specified here.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?(\.[a-z]([-a-z0-9]*[a-z0-9])?)*$`
	APIExportName string `json:"apiExportName"`

	// Group is the API group of the dependent resource.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?(\.[a-z]([-a-z0-9]*[a-z0-9])?)*$`
	Group string `json:"group"`

	// Version is the API version of the dependent resource.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^v[1-9][0-9]*([a-z]+[1-9][0-9]*)?$`
	Version string `json:"version"`

	// Kind is the kind of the dependent resource (e.g., "VirtualMachine").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Z][A-Za-z0-9]*$`
	Kind string `json:"kind"`

	// Resource is the plural resource name of the dependent (e.g., "virtualmachines").
	// Used by the webhook to construct the GVR for listing dependent resources.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9]*$`
	Resource string `json:"resource"`
}

// APIExportReference identifies an APIExport by workspace path and name.
type APIExportReference struct {
	// Path is the workspace path where the APIExport lives (e.g., "root:compute-provider").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]*(:[a-z][a-z0-9-]*)*$`
	Path string `json:"path"`

	// Name is the name of the APIExport.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?(\.[a-z]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// DependencyTarget describes a resource type that the dependent depends on.
type DependencyTarget struct {
	// APIExportRef references the APIExport that provides this dependency resource type.
	// Used by the webhook installer to register deletion protection in the correct workspace.
	APIExportRef APIExportReference `json:"apiExportRef"`

	// Group is the API group of the dependency resource.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?(\.[a-z]([-a-z0-9]*[a-z0-9])?)*$`
	Group string `json:"group"`

	// Version is the API version of the dependency resource.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^v[1-9][0-9]*([a-z]+[1-9][0-9]*)?$`
	Version string `json:"version"`

	// Resource is the plural resource name (e.g., "vpcs").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9]*$`
	Resource string `json:"resource"`

	// FieldRef describes where in the dependent resource the dependency is referenced.
	FieldRef FieldReference `json:"fieldRef"`
}

// FieldReference describes a field path in the dependent resource that points to the dependency.
type FieldReference struct {
	// Path is a dot-notation path in the dependent resource pointing to the dependency's name.
	// For example, ".spec.vpcRef.name". The leading dot is optional. Array indexing
	// and wildcards are not supported.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^\.?[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)+$`
	Path string `json:"path"`
}

// +kubebuilder:object:root=true

// DependencyRuleList contains a list of DependencyRule objects.
type DependencyRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []DependencyRule `json:"items"`
}
