package provision_test

import (
	"testing"

	"github.com/arloliu/parti/v2/provision"
	"github.com/stretchr/testify/require"
)

func TestBuildMarker_WithInstance(t *testing.T) {
	t.Parallel()
	m := provision.BuildMarker(provision.ComponentControlPlaneElection, "prod-us-east")
	require.Equal(t, provision.MarkerManagedValue, m[provision.MarkerManagedKey])
	require.Equal(t, provision.ComponentControlPlaneElection, m[provision.MarkerComponentKey])
	require.Equal(t, "prod-us-east", m[provision.MarkerInstanceKey])
}

func TestBuildMarker_EmptyInstanceOmitsKey(t *testing.T) {
	t.Parallel()
	m := provision.BuildMarker(provision.ComponentControlPlaneID, "")
	require.Equal(t, provision.MarkerManagedValue, m[provision.MarkerManagedKey])
	require.Equal(t, provision.ComponentControlPlaneID, m[provision.MarkerComponentKey])
	_, hasInstance := m[provision.MarkerInstanceKey]
	require.False(t, hasInstance, "instance key must be omitted when instance is empty")
}

func TestParseMarker_NilMetadata(t *testing.T) {
	t.Parallel()
	info := provision.ParseMarker(nil)
	require.False(t, info.IsManaged())
	require.Equal(t, "", info.ClassifyComponent())
}

func TestParseMarker_EmptyMetadata(t *testing.T) {
	t.Parallel()
	info := provision.ParseMarker(map[string]string{})
	require.False(t, info.IsManaged())
}

func TestParseMarker_MissingManagedKeyIsUnmarked(t *testing.T) {
	t.Parallel()
	info := provision.ParseMarker(map[string]string{
		provision.MarkerComponentKey: provision.ComponentControlPlaneID,
	})
	require.False(t, info.IsManaged())
	require.Equal(t, "", info.ClassifyComponent())
}

func TestParseMarker_KnownComponent(t *testing.T) {
	t.Parallel()
	info := provision.ParseMarker(map[string]string{
		provision.MarkerManagedKey:   provision.MarkerManagedValue,
		provision.MarkerComponentKey: provision.ComponentControlPlaneAssignment,
		provision.MarkerInstanceKey:  "stage",
	})
	require.True(t, info.IsManaged())
	// Component is classified directly on parse; no need to call ClassifyComponent.
	require.Equal(t, provision.ComponentControlPlaneAssignment, info.Component)
	require.Equal(t, provision.ComponentControlPlaneAssignment, info.ClassifyComponent())
	require.Equal(t, "stage", info.Instance)
}

func TestParseMarker_UnknownComponent(t *testing.T) {
	t.Parallel()
	info := provision.ParseMarker(map[string]string{
		provision.MarkerManagedKey:   provision.MarkerManagedValue,
		provision.MarkerComponentKey: "future-component",
	})
	require.True(t, info.IsManaged())
	// ParseMarker must classify unknown components directly; callers need not
	// call ClassifyComponent to get the "unknown" sentinel.
	require.Equal(t, provision.ComponentUnknown, info.Component)
	require.Equal(t, provision.ComponentUnknown, info.ClassifyComponent())
}

func TestParseMarker_IgnoresUnknownPartiKeys(t *testing.T) {
	t.Parallel()
	info := provision.ParseMarker(map[string]string{
		provision.MarkerManagedKey:   provision.MarkerManagedValue,
		provision.MarkerComponentKey: provision.ComponentControlPlaneID,
		"parti.io/future":            "x",
		"unrelated":                  "y",
	})
	require.True(t, info.IsManaged())
	require.Equal(t, provision.ComponentControlPlaneID, info.ClassifyComponent())
}

func TestBuildMarker_ParseMarker_Roundtrip(t *testing.T) {
	t.Parallel()
	m := provision.BuildMarker(provision.ComponentPartitionSource, "envA")
	parsed := provision.ParseMarker(m)
	require.Equal(t, provision.MarkerManagedValue, parsed.Managed)
	require.Equal(t, provision.ComponentPartitionSource, parsed.Component)
	require.Equal(t, "envA", parsed.Instance)
}

// TestParseMarker_ComponentStream verifies that a stream-marked resource
// is classified as ComponentStream by both ParseMarker and ClassifyComponent.
func TestParseMarker_ComponentStream(t *testing.T) {
	t.Parallel()
	m := provision.BuildMarker(provision.ComponentStream, "prod")
	info := provision.ParseMarker(m)
	require.True(t, info.IsManaged())
	require.Equal(t, provision.ComponentStream, info.Component)
	require.Equal(t, provision.ComponentStream, info.ClassifyComponent())
	require.Equal(t, "prod", info.Instance)
}

// TestClassifyComponent_ComponentStream verifies that ClassifyComponent returns
// ComponentStream (not ComponentUnknown) for a stream-marked resource.
func TestClassifyComponent_ComponentStream(t *testing.T) {
	t.Parallel()
	info := provision.ParseMarker(map[string]string{
		provision.MarkerManagedKey:   provision.MarkerManagedValue,
		provision.MarkerComponentKey: provision.ComponentStream,
	})
	require.True(t, info.IsManaged())
	require.Equal(t, provision.ComponentStream, info.ClassifyComponent())
}
