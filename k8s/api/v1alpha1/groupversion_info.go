// Package v1alpha1 contains the API types for the Parti Kubernetes operator
// (group parti.io, version v1alpha1).
//
// +kubebuilder:object:generate=true
// +groupName=parti.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group/version for the Parti operator API.
	GroupVersion = schema.GroupVersion{Group: "parti.io", Version: "v1alpha1"}

	// SchemeBuilder collects the registration functions for this
	// group-version's types.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes registers the v1alpha1 types and the group-version metadata
// with scheme.
func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&ProvisionedPartiEnv{},
		&ProvisionedPartiEnvList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)

	return nil
}
