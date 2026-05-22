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

// createStream creates a real JetStream stream for integration tests.
func createStream(t *testing.T, js jetstream.JetStream, cfg jetstream.StreamConfig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := js.CreateStream(ctx, cfg)
	require.NoError(t, err)
}

func TestView_MarkedApplicationStream_SurfacesInStreams(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	createStream(t, js, jetstream.StreamConfig{
		Name:     "app-events",
		Subjects: []string{"events.>"},
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentStream, "prod"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := provision.View(ctx, js, provision.ScopeAll())
	require.NoError(t, err)
	require.Len(t, snap.Streams, 1)
	require.Equal(t, "app-events", snap.Streams[0].Stream)
	require.Equal(t, provision.ComponentStream, snap.Streams[0].Component)
	require.Equal(t, "prod", snap.Streams[0].Instance)
	require.Equal(t, "memory", snap.Streams[0].Storage)
	require.Equal(t, []string{"events.>"}, snap.Streams[0].Subjects)
}

func TestView_UnmarkedNonKVStream_Dropped(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	createStream(t, js, jetstream.StreamConfig{
		Name:     "unmanaged-stream",
		Subjects: []string{"data.>"},
		Storage:  jetstream.MemoryStorage,
		// No Metadata — not Parti-marked.
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := provision.View(ctx, js, provision.ScopeAll())
	require.NoError(t, err)
	require.Empty(t, snap.Streams)
}

func TestView_NonStreamComponentOnNonKV_Dropped(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	// A non-KV stream carrying a control-plane marker is not an application stream.
	createStream(t, js, jetstream.StreamConfig{
		Name:     "wrong-component-stream",
		Subjects: []string{"cp.>"},
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentControlPlaneElection, "prod"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := provision.View(ctx, js, provision.ScopeAll())
	require.NoError(t, err)
	require.Empty(t, snap.Streams)
	// Also not in ControlPlane (it's not KV).
	require.Empty(t, snap.ControlPlane)
}

func TestView_KVBucketsUnaffectedByStreamLogic(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "cp-election",
		History:  1,
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentControlPlaneElection, "prod"),
	})
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "parti-partitions",
		History:  1,
		Storage:  jetstream.FileStorage,
		Metadata: provision.BuildMarker(provision.ComponentPartitionSource, "prod"),
	})
	createStream(t, js, jetstream.StreamConfig{
		Name:     "app-events",
		Subjects: []string{"events.>"},
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentStream, "prod"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := provision.View(ctx, js, provision.ScopeAll())
	require.NoError(t, err)
	require.Len(t, snap.ControlPlane, 1)
	require.Equal(t, "cp-election", snap.ControlPlane[0].Bucket)
	require.Len(t, snap.PartitionSource, 1)
	require.Equal(t, "parti-partitions", snap.PartitionSource[0].Bucket)
	require.Len(t, snap.Streams, 1)
	require.Equal(t, "app-events", snap.Streams[0].Stream)
}

func TestView_StreamsInstanceFilter(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	createStream(t, js, jetstream.StreamConfig{
		Name:     "prod-events",
		Subjects: []string{"prod.events.>"},
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentStream, "prod"),
	})
	createStream(t, js, jetstream.StreamConfig{
		Name:     "stage-events",
		Subjects: []string{"stage.events.>"},
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentStream, "stage"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	scope := provision.ScopeAll()
	scope.Instance = "prod"
	snap, err := provision.View(ctx, js, scope)
	require.NoError(t, err)
	require.Len(t, snap.Streams, 1)
	require.Equal(t, "prod-events", snap.Streams[0].Stream)
	require.Equal(t, "prod", snap.Streams[0].Instance)
}

func TestView_StreamsScopeDisabled_NotIncluded(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	createStream(t, js, jetstream.StreamConfig{
		Name:     "app-events",
		Subjects: []string{"events.>"},
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentStream, "prod"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Scope with Streams=false.
	scope := provision.Scope{
		ControlPlane:    true,
		PartitionSource: true,
		Streams:         false,
	}
	snap, err := provision.View(ctx, js, scope)
	require.NoError(t, err)
	// Streams slot must still be non-nil (empty slice, not nil).
	require.NotNil(t, snap.Streams)
	require.Empty(t, snap.Streams)
}

func TestView_SnapshotStreams_NonNilWhenEmpty(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := provision.View(ctx, js, provision.ScopeAll())
	require.NoError(t, err)
	// Streams must be a non-nil empty slice, not nil.
	require.NotNil(t, snap.Streams)
	require.Empty(t, snap.Streams)
}

func TestView_Streams_DeterministicSort(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	// Create streams in reverse alphabetical order.
	for _, name := range []string{"zebra-stream", "alpha-stream", "middle-stream"} {
		createStream(t, js, jetstream.StreamConfig{
			Name:     name,
			Subjects: []string{name + ".>"},
			Storage:  jetstream.MemoryStorage,
			Metadata: provision.BuildMarker(provision.ComponentStream, "prod"),
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := provision.View(ctx, js, provision.ScopeAll())
	require.NoError(t, err)
	require.Len(t, snap.Streams, 3)
	require.Equal(t, "alpha-stream", snap.Streams[0].Stream)
	require.Equal(t, "middle-stream", snap.Streams[1].Stream)
	require.Equal(t, "zebra-stream", snap.Streams[2].Stream)
}

func TestView_Streams_MaxBytesMaxMsgsMinusOne_ReportsZero(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	// Create a stream; NATS will store MaxBytes=-1 and MaxMsgs=-1 as the
	// "unlimited" sentinel for those two fields when 0 is passed.
	createStream(t, js, jetstream.StreamConfig{
		Name:     "unlimited-stream",
		Subjects: []string{"data.>"},
		Storage:  jetstream.MemoryStorage,
		Metadata: provision.BuildMarker(provision.ComponentStream, ""),
		// 0 means unlimited in config; NATS server rewrites to -1 internally.
		MaxBytes: 0,
		MaxMsgs:  0,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := provision.View(ctx, js, provision.ScopeAll())
	require.NoError(t, err)
	require.Len(t, snap.Streams, 1)
	// View should normalise live -1 → 0.
	require.Equal(t, int64(0), snap.Streams[0].MaxBytes)
	require.Equal(t, int64(0), snap.Streams[0].MaxMsgs)
}
