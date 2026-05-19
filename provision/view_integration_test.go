package provision_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// newJS returns a jetstream.JetStream attached to a fresh embedded NATS
// server. Cleanup is registered via t.Cleanup automatically by partitest.
func newJS(t *testing.T) jetstream.JetStream {
	t.Helper()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	return js
}

func createKV(t *testing.T, js jetstream.JetStream, cfg jetstream.KeyValueConfig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := js.CreateKeyValue(ctx, cfg)
	require.NoError(t, err)
}

func TestView_EmptyServer_ReturnsEmptySnapshot(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := provision.View(ctx, js, provision.ScopeAll())
	require.NoError(t, err)
	require.Equal(t, provision.APIVersionProvisionV1, snap.APIVersion)
	require.Equal(t, provision.KindSnapshot, snap.Kind)
	require.Empty(t, snap.ControlPlane)
	require.Empty(t, snap.PartitionSource)
}

func TestView_MixedMarkedAndUnmarked_OnlyMarkedReturned(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	// Marked bucket: stamped with Parti marker, control-plane:election.
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "marked-election",
		History:  1,
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentControlPlaneElection, ""),
	})
	// Unmarked bucket: no Parti metadata at all.
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:  "external-bucket",
		History: 1,
		Storage: jetstream.MemoryStorage,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := provision.View(ctx, js, provision.ScopeAll())
	require.NoError(t, err)
	require.Len(t, snap.ControlPlane, 1)
	require.Equal(t, "marked-election", snap.ControlPlane[0].Bucket)
	require.Equal(t, provision.ComponentControlPlaneElection, snap.ControlPlane[0].Component)
}

func TestView_InstanceFilter_OnlyMatchingInstance(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "prod-election",
		History:  1,
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentControlPlaneElection, "prod"),
	})
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "stage-election",
		History:  1,
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentControlPlaneElection, "stage"),
	})
	// Marked without instance.
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "shared-election",
		History:  1,
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentControlPlaneElection, ""),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	scope := provision.ScopeAll()
	scope.Instance = "prod"
	snap, err := provision.View(ctx, js, scope)
	require.NoError(t, err)
	require.Len(t, snap.ControlPlane, 1)
	require.Equal(t, "prod-election", snap.ControlPlane[0].Bucket)
	require.Equal(t, "prod", snap.ControlPlane[0].Instance)
}

func TestView_InventoryMode_IncludesAllInstances(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "prod-election",
		History:  1,
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentControlPlaneElection, "prod"),
	})
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "stage-election",
		History:  1,
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentControlPlaneElection, "stage"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := provision.View(ctx, js, provision.ScopeAll())
	require.NoError(t, err)
	require.Len(t, snap.ControlPlane, 2)
}

func TestView_PartitionSourceCategory(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "parti-partitions",
		History:  1,
		Storage:  jetstream.FileStorage,
		Metadata: provision.BuildMarker(provision.ComponentPartitionSource, ""),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := provision.View(ctx, js, provision.ScopeAll())
	require.NoError(t, err)
	require.Empty(t, snap.ControlPlane)
	require.Len(t, snap.PartitionSource, 1)
	require.Equal(t, "parti-partitions", snap.PartitionSource[0].Bucket)
}

func TestView_CancelledContext(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snap, err := provision.View(ctx, js, provision.ScopeAll())
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, snap)
}
