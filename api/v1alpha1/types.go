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
type DependentRef struct {
	// APIExportRef references the APIExport that provides this resource type.
	APIExportRef APIExportReference `json:"apiExportRef"`

	// Group is the API group of the dependent resource.
	Group string `json:"group"`

	// Version is the API version of the dependent resource.
	Version string `json:"version"`

	// Kind is the kind of the dependent resource (e.g., "VirtualMachine").
	Kind string `json:"kind"`

	// Resource is the plural resource name (e.g., "virtualmachines").
	Resource string `json:"resource"`
}

// APIExportReference identifies an APIExport by workspace path and name.
type APIExportReference struct {
	// Path is the workspace path where the APIExport lives (e.g., "root:compute-provider").
	Path string `json:"path"`

	// Name is the name of the APIExport.
	Name string `json:"name"`
}

// DependencyTarget describes a resource type that the dependent depends on.
type DependencyTarget struct {
	// APIExportRef references the APIExport that provides this dependency resource type.
	// Used by the webhook installer to register deletion protection in the correct workspace.
	APIExportRef APIExportReference `json:"apiExportRef"`

	// Group is the API group of the dependency resource.
	Group string `json:"group"`

	// Version is the API version of the dependency resource.
	Version string `json:"version"`

	// Resource is the plural resource name (e.g., "vpcs").
	Resource string `json:"resource"`

	// FieldRef describes where in the dependent resource the dependency is referenced.
	FieldRef FieldReference `json:"fieldRef"`
}

// FieldReference describes a field path in the dependent resource that points to the dependency.
type FieldReference struct {
	// Path is a dot-notation path in the dependent resource pointing to the dependency's name.
	// For example, ".spec.vpcRef.name".
	Path string `json:"path"`
}

// +kubebuilder:object:root=true

// DependencyRuleList contains a list of DependencyRule objects.
type DependencyRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []DependencyRule `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&DependencyRule{}, &DependencyRuleList{},
	)
}
