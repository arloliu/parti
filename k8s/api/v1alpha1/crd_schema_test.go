package v1alpha1_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// crdFilePath resolves the generated CRD YAML relative to this test file,
// regardless of the working directory go test is invoked from.
func crdFilePath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")

	return filepath.Join(filepath.Dir(thisFile), "..", "..", "config", "crd", "parti.io_provisionedpartienvs.yaml")
}

// loadCRD reads and unmarshals the generated CRD YAML into a typed struct.
func loadCRD(t *testing.T) apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	path := crdFilePath(t)
	data, err := os.ReadFile(path)
	require.NoError(t, err, "reading CRD file %s", path)

	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(data, &crd), "parsing CRD YAML")
	require.NotEmpty(t, crd.Spec.Versions, "CRD has no versions")

	return crd
}

// specProps returns the Properties map of spec.openAPIV3Schema.properties.spec
// from the first CRD version. It fails the test if the structure is missing.
func specProps(t *testing.T, crd apiextensionsv1.CustomResourceDefinition) map[string]apiextensionsv1.JSONSchemaProps {
	t.Helper()
	schema := crd.Spec.Versions[0].Schema
	require.NotNil(t, schema, "version 0 has no schema")
	require.NotNil(t, schema.OpenAPIV3Schema, "version 0 schema has no openAPIV3Schema")

	specProp, ok := schema.OpenAPIV3Schema.Properties["spec"]
	require.True(t, ok, "openAPIV3Schema has no 'spec' property")

	return specProp.Properties
}

// assertMinimumZero fails the test unless the field's minimum bound is present
// and equals 0 — the non-negative contract every bounded numeric field in the
// CRD shares.
func assertMinimumZero(t *testing.T, props map[string]apiextensionsv1.JSONSchemaProps, field string) {
	t.Helper()
	p, ok := props[field]
	require.True(t, ok, "property %q not found in schema", field)
	require.NotNil(t, p.Minimum, "field %q has no minimum bound", field)
	assert.Equal(t, float64(0), *p.Minimum, "field %q minimum", field)
}

// assertMaximum fails the test unless the field's maximum is non-nil and equals want.
func assertMaximum(t *testing.T, props map[string]apiextensionsv1.JSONSchemaProps, field string, want float64) {
	t.Helper()
	p, ok := props[field]
	require.True(t, ok, "property %q not found in schema", field)
	require.NotNil(t, p.Maximum, "field %q has no maximum bound", field)
	assert.Equal(t, want, *p.Maximum, "field %q maximum", field)
}

// TestCRDSchemaBounds asserts that the generated CRD OpenAPI schema carries the
// numeric bounds declared by the kubebuilder markers in provisionedpartienv_types.go.
//
// This test guards against marker drift: if a future edit removes or changes a
// +kubebuilder:validation:Minimum/Maximum marker and the CRD YAML is regenerated,
// this test will fail, forcing a conscious review of the range-safety contract.
func TestCRDSchemaBounds(t *testing.T) {
	crd := loadCRD(t)
	spec := specProps(t, crd)

	// --- controlPlane.replicas: minimum=0 -----------------------------------
	cpProp, ok := spec["controlPlane"]
	require.True(t, ok, "spec has no 'controlPlane' property")
	assertMinimumZero(t, cpProp.Properties, "replicas")

	// --- partitionSource ---------------------------------------------------
	psProp, ok := spec["partitionSource"]
	require.True(t, ok, "spec has no 'partitionSource' property")

	// partitionSource.history: minimum=0, maximum=255
	assertMinimumZero(t, psProp.Properties, "history")
	assertMaximum(t, psProp.Properties, "history", 255)

	// partitionSource.maxValueSize: minimum=0
	assertMinimumZero(t, psProp.Properties, "maxValueSize")

	// partitionSource.replicas: minimum=0
	assertMinimumZero(t, psProp.Properties, "replicas")

	// --- streams[].* -------------------------------------------------------
	streamsProp, ok := spec["streams"]
	require.True(t, ok, "spec has no 'streams' property")
	require.NotNil(t, streamsProp.Items, "streams has no items schema")
	require.NotNil(t, streamsProp.Items.Schema, "streams items schema is nil")
	streamItem := streamsProp.Items.Schema.Properties

	// streams[].replicas: minimum=0
	assertMinimumZero(t, streamItem, "replicas")

	// streams[].maxBytes: minimum=0
	assertMinimumZero(t, streamItem, "maxBytes")

	// streams[].maxMsgs: minimum=0
	assertMinimumZero(t, streamItem, "maxMsgs")
}
