package source

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestNatsKV_MultiPod_WatcherPropagation pins the cross-instance contract that
// underpins multi-worker (multi-pod) deployments: a partition list written by
// one NatsKV instance must become visible to every other NatsKV instance bound
// to the same bucket+key.
//
// Each pod in a fleet constructs its own NatsKV against a shared KV bucket. Only
// the instance that calls Update/Modify updates its local cache synchronously;
// all other instances learn of the change through their own watcher (this test)
// or reconcile loop (TestNatsKV_MultiPod_ReconcilePropagation). List() is NOT
// leader-gated — every started instance serves the full list from its own cache.
//
// Reconcile is disabled here so the assertion isolates the watcher path.
func TestNatsKV_MultiPod_WatcherPropagation(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_multipod_watch"})
	require.NoError(t, err)

	// Two independent KeyValue handles to the same bucket model two pods, each
	// with its own source instance.
	kvWriter, err := js.KeyValue(ctx, "partitions_multipod_watch")
	require.NoError(t, err)
	kvFollower, err := js.KeyValue(ctx, "partitions_multipod_watch")
	require.NoError(t, err)

	const key = "partitions"
	writer := NewNatsKV(kvWriter, key, nil, WithReconcileInterval(0))
	follower := NewNatsKV(kvFollower, key, nil, WithReconcileInterval(0))

	require.NoError(t, writer.Start(ctx))
	defer func() { _ = writer.Stop(ctx) }()
	require.NoError(t, follower.Start(ctx))
	defer func() { _ = follower.Stop(ctx) }()

	parts := []types.Partition{
		{Keys: []string{"p1"}, Weight: 100},
		{Keys: []string{"p2"}, Weight: 200},
	}
	require.NoError(t, writer.Update(ctx, parts))

	// The writer observes its own Update synchronously (local cache write).
	got, err := writer.List(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2, "writer must see its own Update immediately")

	// The follower must converge via its own watcher.
	require.Eventually(t, func() bool {
		p, listErr := follower.List(ctx)
		return listErr == nil && len(p) == 2
	}, 3*time.Second, 20*time.Millisecond,
		"follower never observed the writer's Update through the watcher")

	fp, err := follower.List(ctx)
	require.NoError(t, err)
	require.Equal(t, parts, fp, "follower's converged list must match the writer's")
}

// TestNatsKV_MultiPod_ReconcilePropagation pins the same cross-instance contract
// for the reconcile path: with the follower's watcher frozen (delivering no
// events), the periodic reconcile loop alone must converge the follower onto the
// writer's published list. This is the load-bearing recovery path when a watcher
// silently stalls (see WithReconcileInterval Godoc).
func TestNatsKV_MultiPod_ReconcilePropagation(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_multipod_reconcile"})
	require.NoError(t, err)

	kvWriter, err := js.KeyValue(ctx, "partitions_multipod_reconcile")
	require.NoError(t, err)
	kvFollower, err := js.KeyValue(ctx, "partitions_multipod_reconcile")
	require.NoError(t, err)

	const key = "partitions"
	writer := NewNatsKV(kvWriter, key, nil, WithReconcileInterval(0))

	// Follower: fast reconcile cadence + a frozen watcher that never delivers an
	// event, so convergence can only come from the reconcile loop.
	follower := NewNatsKV(kvFollower, key, nil, WithReconcileInterval(50*time.Millisecond))
	frozen := make(chan jetstream.KeyValueEntry) // unbuffered, never written
	follower.watchFn = func(_ context.Context) (jetstream.KeyWatcher, error) {
		return &fakeKeyWatcher{updates: frozen}, nil
	}

	require.NoError(t, writer.Start(ctx))
	defer func() { _ = writer.Stop(ctx) }()
	require.NoError(t, follower.Start(ctx))
	defer func() { _ = follower.Stop(ctx) }()

	parts := []types.Partition{{Keys: []string{"p1"}, Weight: 100}}
	require.NoError(t, writer.Update(ctx, parts))

	require.Eventually(t, func() bool {
		p, listErr := follower.List(ctx)
		return listErr == nil && len(p) == 1
	}, 3*time.Second, 20*time.Millisecond,
		"follower never observed the writer's Update through the reconcile loop")
}

// TestNatsKV_NotStarted_NeverConverges documents the failure mode behind the
// most common multi-pod misconfiguration: constructing a NatsKV but never
// calling Start. Update still works (it writes KV and updates the local cache
// directly), so the writing instance sees the list — but an instance that is
// never started runs no watcher and no reconcile loop, so its List stays empty
// regardless of what other instances publish. Start (or Manager.Start, which
// calls it) is mandatory for a follower to observe writes.
func TestNatsKV_NotStarted_NeverConverges(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_multipod_nostart"})
	require.NoError(t, err)

	kvWriter, err := js.KeyValue(ctx, "partitions_multipod_nostart")
	require.NoError(t, err)
	kvFollower, err := js.KeyValue(ctx, "partitions_multipod_nostart")
	require.NoError(t, err)

	const key = "partitions"
	writer := NewNatsKV(kvWriter, key, nil, WithReconcileInterval(0))
	require.NoError(t, writer.Start(ctx))
	defer func() { _ = writer.Stop(ctx) }()

	// Deliberately NOT started.
	follower := NewNatsKV(kvFollower, key, nil)

	require.NoError(t, writer.Update(ctx, []types.Partition{{Keys: []string{"p1"}, Weight: 100}}))

	// Give any propagation a generous window; an unstarted source has no path to
	// converge, so its list must remain empty.
	require.Never(t, func() bool {
		p, listErr := follower.List(context.Background())
		return listErr == nil && len(p) > 0
	}, 500*time.Millisecond, 50*time.Millisecond,
		"unstarted follower must never converge (Start is required)")
}
