package controller

const (
	// LabelRule is the label key for the DependencyRule name on Dependency objects.
	LabelRule = "dependencies.opendefense.cloud/rule"

	// LabelRuleCluster is the label key for the logical cluster name of the
	// workspace where the DependencyRule lives.
	LabelRuleCluster = "dependencies.opendefense.cloud/rule-cluster"

	// LabelDependentName is the label key for the dependent resource name.
	LabelDependentName = "dependencies.opendefense.cloud/dependent-name"

	// AnnotationSkipProtection is the annotation key that, when set to "true"
	// on a resource, causes the deletion webhook to skip protection checks.
	AnnotationSkipProtection = "dependencies.opendefense.cloud/skip-protection"
)
