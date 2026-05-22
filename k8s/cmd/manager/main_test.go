package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/arloliu/parti/v2/k8s/api/v1alpha1"
)

// TestNewScheme asserts that newScheme registers both the core Kubernetes types
// (clientgoscheme) and the operator's v1alpha1 CRD types without error, and
// that the scheme recognizes the ProvisionedPartiEnv GVK.
func TestNewScheme(t *testing.T) {
	scheme, err := newScheme()
	require.NoError(t, err, "newScheme() must not return an error")
	require.NotNil(t, scheme)

	gvk := schema.GroupVersionKind{
		Group:   v1alpha1.GroupVersion.Group,
		Version: v1alpha1.GroupVersion.Version,
		Kind:    "ProvisionedPartiEnv",
	}
	require.True(t, scheme.Recognizes(gvk),
		"scheme must recognize the ProvisionedPartiEnv GVK %s", gvk)

	listGVK := schema.GroupVersionKind{
		Group:   v1alpha1.GroupVersion.Group,
		Version: v1alpha1.GroupVersion.Version,
		Kind:    "ProvisionedPartiEnvList",
	}
	require.True(t, scheme.Recognizes(listGVK),
		"scheme must recognize the ProvisionedPartiEnvList GVK %s", listGVK)
}
