package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true

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

// +kubebuilder:object:root=true

// Dependency is a marker object that records a specific dependency relationship
// between two resource instances. Created automatically by the dependency controller.
type Dependency struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DependencySpec `json:"spec"`
}

// DependencySpec identifies the specific dependent and dependency resource instances.
type DependencySpec struct {
	// Dependent identifies the resource that holds the reference.
	Dependent ObjectReference `json:"dependent"`

	// Dependency identifies the resource being depended upon (protected from deletion).
	Dependency ObjectReference `json:"dependency"`

	// RuleName is the name of the DependencyRule that caused this Dependency to be created.
	RuleName string `json:"ruleName"`
}

// ObjectReference identifies a specific resource instance.
type ObjectReference struct {
	// Group is the API group of the resource.
	Group string `json:"group"`

	// Version is the API version of the resource.
	Version string `json:"version"`

	// Resource is the plural resource name.
	Resource string `json:"resource"`

	// Name is the name of the resource instance.
	Name string `json:"name"`

	// Namespace is the namespace of the resource (empty for cluster-scoped resources).
	Namespace string `json:"namespace,omitempty"`
}

// +kubebuilder:object:root=true

// DependencyList contains a list of Dependency objects.
type DependencyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Dependency `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&DependencyRule{}, &DependencyRuleList{},
		&Dependency{}, &DependencyList{},
	)
}
