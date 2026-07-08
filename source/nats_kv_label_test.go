package source

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/partcodec"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestDeepCopyPartitions_PreservesLabel(t *testing.T) {
	t.Parallel()

	in := []types.Partition{{Keys: []string{"a"}, Weight: 2, Label: "vip"}}
	out := deepCopyPartitions(in)

	require.Equal(t, in, out)
	// Still a deep copy: mutating the copy's Keys must not alias the input.
	out[0].Keys[0] = "mutated"
	require.Equal(t, "a", in[0].Keys[0])
}

func TestValidateAndDedupe_PreservesLabel(t *testing.T) {
	t.Parallel()

	in := []types.Partition{{Keys: []string{"a"}, Weight: 2, Label: "vip"}}
	out, err := validateAndDedupe(in)
	require.NoError(t, err)
	require.Equal(t, "vip", out[0].Label)

	// Same keys + different labels = duplicate identity → error (spec §4.1).
	_, err = validateAndDedupe([]types.Partition{
		{Keys: []string{"a"}, Label: "vip"},
		{Keys: []string{"a"}, Label: "batch"},
	})
	require.Error(t, err)
}

func TestPartitionsEqual_LabelAware(t *testing.T) {
	t.Parallel()

	a := []types.Partition{{Keys: []string{"a"}, Weight: 1}}
	b := []types.Partition{{Keys: []string{"a"}, Weight: 1, Label: "vip"}}

	require.True(t, partitionsEqual(a, a))
	require.False(t, partitionsEqual(a, b), "label-only difference must be a change")
}

// newLabelTestSource creates a real KV bucket, seeds it with `initial`,
// and returns a started NatsKV plus the raw KV handle for out-of-band
// writes (simulating an external operator/writer process).
func newLabelTestSource(t *testing.T, initial []types.Partition) (*NatsKV, jetstream.KeyValue, context.Context) {
	t.Helper()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "label-src-test"})
	require.NoError(t, err)

	seed, err := partcodec.Encode(initial)
	require.NoError(t, err)
	_, err = kv.Put(ctx, "partitions", seed)
	require.NoError(t, err)

	src := NewNatsKV(kv, "partitions", nil) // nil logger is the package's test convention
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	return src, kv, ctx
}

// TestNatsKV_LabelOnlyEdit_WatchPathPropagates is the spec §10 regression:
// a rewrite that changes ONLY a label (same keys, same weights) must fire
// the Watch signal and the next Snapshot must carry the label.
func TestNatsKV_LabelOnlyEdit_WatchPathPropagates(t *testing.T) {
	t.Parallel()

	initial := []types.Partition{
		{Keys: []string{"p0"}, Weight: 1},
		{Keys: []string{"p1"}, Weight: 1},
	}
	src, kv, ctx := newLabelTestSource(t, initial)

	watchCh := src.Watch(ctx)

	// Label-only rewrite via an out-of-band KV write (external writer).
	promoted := []types.Partition{
		{Keys: []string{"p0"}, Weight: 1, Label: "vip"}, // <- only delta
		{Keys: []string{"p1"}, Weight: 1},
	}
	encoded, err := partcodec.Encode(promoted)
	require.NoError(t, err)
	_, err = kv.Put(ctx, "partitions", encoded)
	require.NoError(t, err)

	select {
	case <-watchCh:
		// change notification observed
	case <-time.After(10 * time.Second):
		t.Fatal("label-only edit did not fire the source Watch signal")
	}

	parts, _, _, err := src.Snapshot(ctx)
	require.NoError(t, err)
	byID := map[string]string{}
	for _, p := range parts {
		byID[p.CanonicalID()] = p.Label
	}
	require.Equal(t, "vip", byID[types.Partition{Keys: []string{"p0"}}.CanonicalID()],
		"snapshot must carry the label through decode → deep-copy → store")
}

// TestNatsKV_LabelOnlyEdit_ReconcilePathPropagates drives the same edit
// through the reconcile path: the watcher is starved by writing while the
// source's watch is torn down, then the periodic reconcile must pick up
// the label-only change and notify.
func TestNatsKV_LabelOnlyEdit_ReconcilePathPropagates(t *testing.T) {
	t.Parallel()

	initial := []types.Partition{{Keys: []string{"p0"}, Weight: 1}}

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "label-src-reconcile"})
	require.NoError(t, err)
	seed, err := partcodec.Encode(initial)
	require.NoError(t, err)
	_, err = kv.Put(ctx, "partitions", seed)
	require.NoError(t, err)

	// Aggressive fixed-cadence reconcile. NOTE: WithLeadershipProbe is
	// deliberately NOT used here — when a leadership probe is wired,
	// nextReconcileInterval ignores WithReconcileInterval entirely (ties
	// the cadence to leaderReconcileInterval=30s / followerReconcileInterval=5min
	// instead, see nats_kv.go:reconcileLoop), which would make this test's
	// 10s timeout unreachable. The watcher is FROZEN (a never-delivering
	// injected watchFn, the same harness shape existing reconcile tests
	// use — see source/nats_kv_test.go:437
	// TestNatsKV_Reconcile_RecoversFromMissedWatcherEvent) so ONLY the
	// reconcile loop can observe the direct KV write. Without freezing,
	// the watch path could deliver first and this test would prove
	// nothing about reconcile.
	src := NewNatsKV(kv, "partitions", nil, WithReconcileInterval(200*time.Millisecond))
	frozenUpdates := make(chan jetstream.KeyValueEntry) // never written
	src.watchFn = func(_ context.Context) (jetstream.KeyWatcher, error) {
		return &fakeKeyWatcher{updates: frozenUpdates}, nil
	}
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	watchCh := src.Watch(ctx)

	promoted := []types.Partition{{Keys: []string{"p0"}, Weight: 1, Label: "vip"}}
	encoded, err := partcodec.Encode(promoted)
	require.NoError(t, err)
	_, err = kv.Put(ctx, "partitions", encoded)
	require.NoError(t, err)

	// With the watcher frozen, this notification can ONLY come from the
	// reconcile loop — pinning that reconcile's skip-guard does not skip
	// a label-only change (its identity check is revision-based and every
	// KV Put advances the revision).
	select {
	case <-watchCh:
	case <-time.After(10 * time.Second):
		t.Fatal("label-only edit did not propagate via the reconcile path")
	}

	parts, _, _, err := src.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, "vip", parts[0].Label)
}
