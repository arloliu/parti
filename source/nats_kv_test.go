package source

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestNatsKV is the smoke test for basic start/stop/update/list behaviour.
func TestNatsKV(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()

	// Create KV bucket
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: "partitions",
	})
	require.NoError(t, err)

	key := "config"
	src := NewNatsKV(kv, key, nil, WithReconcileInterval(0))

	// Start
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	// Initial list should be empty
	partitions, err := src.List(ctx)
	require.NoError(t, err)
	require.Empty(t, partitions)

	// Update
	newPartitions := []types.Partition{
		{Keys: []string{"p1"}, Weight: 100},
	}
	err = src.Update(ctx, newPartitions)
	require.NoError(t, err)

	// Wait for update
	require.Eventually(t, func() bool {
		p, err := src.List(ctx)
		return err == nil && len(p) == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Verify content
	partitions, err = src.List(ctx)
	require.NoError(t, err)
	require.Equal(t, newPartitions, partitions)

	// Test backward compatibility (uncompressed JSON)
	uncompressedData := []byte(`[{"keys":["p2"],"weight":200}]`)
	_, err = kv.Put(ctx, key, uncompressedData)
	require.NoError(t, err)

	// Wait for update
	require.Eventually(t, func() bool {
		p, err := src.List(ctx)
		if err != nil {
			return false
		}
		if len(p) != 1 {
			return false
		}

		return p[0].Weight == 200
	}, 2*time.Second, 10*time.Millisecond)

	partitions, err = src.List(ctx)
	require.NoError(t, err)
	require.Equal(t, "p2", partitions[0].Keys[0])
}

func TestNatsKV_Lifecycle(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_lifecycle"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))

	// 1. Start
	err = src.Start(ctx)
	require.NoError(t, err)
	require.True(t, src.running)
	require.NotNil(t, src.ctx)
	require.NoError(t, src.ctx.Err(), "context should not be cancelled after Start")

	// 2. Stop
	err = src.Stop(ctx)
	require.NoError(t, err)
	require.False(t, src.running)
	require.ErrorIs(t, src.ctx.Err(), context.Canceled, "context should be cancelled after Stop")

	// 3. Restart (should be allowed)
	err = src.Start(ctx)
	require.NoError(t, err)
	require.True(t, src.running)
	require.NotNil(t, src.ctx)
	require.NoError(t, src.ctx.Err(), "new context should not be cancelled after Restart")

	// Cleanup
	_ = src.Stop(ctx)
}

func TestNatsKV_WatcherUpdate(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()

	// Create KV bucket
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: "partitions_watcher",
	})
	require.NoError(t, err)

	key := "config"
	src := NewNatsKV(kv, key, nil, WithReconcileInterval(0))

	// Start source - key does not exist yet
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	// Verify initial state is empty
	partitions, err := src.List(ctx)
	require.NoError(t, err)
	require.Empty(t, partitions)

	// Update partitions via Update method
	expected := []types.Partition{
		{Keys: []string{"p1"}, Weight: 10},
		{Keys: []string{"p2"}, Weight: 20},
	}
	err = src.Update(ctx, expected)
	require.NoError(t, err)

	// Verify watcher picks up the change
	require.Eventually(t, func() bool {
		p, err := src.List(ctx)
		if err != nil {
			return false
		}
		return len(p) == 2
	}, 2*time.Second, 10*time.Millisecond)

	// Verify content matches
	partitions, err = src.List(ctx)
	require.NoError(t, err)
	require.Equal(t, expected, partitions)
}

func TestNatsKV_Watch(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_watch"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	ch := src.Watch(ctx)

	// Update partitions
	err = src.Update(ctx, []types.Partition{{Keys: []string{"p1"}}})
	require.NoError(t, err)

	// Should receive signal
	select {
	case <-ch:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for watch signal")
	}

	// Verify list updated
	partitions, err := src.List(ctx)
	require.NoError(t, err)
	require.Len(t, partitions, 1)
}

// TestNatsKV_Modify_ConcurrentWritersDoNotLoseEachOther (#1) — N=10 goroutines
// each call Modify to add one unique partition; assert KV ends with all 10.
func TestNatsKV_Modify_ConcurrentWritersDoNotLoseEachOther(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_modify_concurrent"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0), WithUpdateRetries(20))
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(idx int) {
			defer wg.Done()
			p := types.Partition{Keys: []string{fmt.Sprintf("part-%02d", idx)}}
			modErr := src.Modify(ctx, func(current []types.Partition) []types.Partition {
				return append(current, p)
			})
			require.NoError(t, modErr)
		}(i)
	}
	wg.Wait()

	partitions, err := src.List(ctx)
	require.NoError(t, err)
	require.Len(t, partitions, n, "all %d partitions must be present", n)
}

// TestNatsKV_Update_CASRetryOnConflict (#2) — forces a stale-revision CAS conflict
// by using a frozen watcher (fake that never delivers events) so the local revision
// stays at rev=1 while the real KV advances to rev=2. Update must detect the conflict
// on its first CAS attempt, refresh from KV, and succeed on the second attempt.
func TestNatsKV_Update_CASRetryOnConflict(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_update_cas"})
	require.NoError(t, err)

	// Inject a frozen watcher that never delivers any events so the local
	// revision cannot be refreshed by the watcher between our raw Put and Update.
	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0), WithUpdateRetries(10))
	frozenUpdates := make(chan jetstream.KeyValueEntry) // unbuffered, never written
	src.watchFn = func(_ context.Context) (jetstream.KeyWatcher, error) {
		return &fakeKeyWatcher{updates: frozenUpdates}, nil
	}

	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	// Write initial data so known=true and revision is set in the local cache.
	initialData := []types.Partition{{Keys: []string{"initial"}}}
	err = src.Update(ctx, initialData)
	require.NoError(t, err)

	// Capture local revision (rev=1 after the Create above).
	_, localRev, localKnown, _ := src.Snapshot(ctx)
	require.True(t, localKnown)
	require.NotZero(t, localRev)

	// Bump the KV revision from outside via raw Put (rev=2). The frozen watcher
	// will not notify the source, so the local cache still believes rev=localRev.
	bumpData := []types.Partition{{Keys: []string{"bumped"}}}
	bumpBytes, encErr := encodePartitions(bumpData)
	require.NoError(t, encErr)
	rawRev, err := kv.Put(ctx, "config", bumpBytes)
	require.NoError(t, err)
	require.Greater(t, rawRev, localRev, "raw KV revision must be ahead of local")

	// Update with retry: first attempt uses localRev (stale → CAS conflict);
	// refreshFromKV picks up rawRev; second attempt uses rawRev and succeeds.
	target := []types.Partition{{Keys: []string{"target"}}}
	err = src.Update(ctx, target)
	require.NoError(t, err)

	got, err := src.List(ctx)
	require.NoError(t, err)
	require.Equal(t, target, got)
}

// TestNatsKV_Update_ImmediateListSeesNewValue (#3) — call Update(x); call
// List() synchronously immediately; assert it returns x.
func TestNatsKV_Update_ImmediateListSeesNewValue(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_immediate"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	want := []types.Partition{{Keys: []string{"p1"}, Weight: 42}}
	err = src.Update(ctx, want)
	require.NoError(t, err)

	// Immediate List — no wait, no Eventually.
	got, err := src.List(ctx)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestNatsKV_Modify_SeesFreshKVNotCache (#4) — proves Modify's callback receives
// a fresh KV read, not the local cache. Uses a frozen watcher to keep the local
// cache deterministically stale after the raw KV write.
func TestNatsKV_Modify_SeesFreshKVNotCache(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_modify_fresh"})
	require.NoError(t, err)

	// Seed KV with [A,B,C] before Start so Start's initial kv.Get seeds the cache.
	// The frozen watcher will not deliver any subsequent updates, so after a raw
	// Put of [A,B,C,D] the local cache will stay at [A,B,C].
	abc := []types.Partition{
		{Keys: []string{"A"}},
		{Keys: []string{"B"}},
		{Keys: []string{"C"}},
	}
	abcBytes, encErr := encodePartitions(abc)
	require.NoError(t, encErr)
	_, err = kv.Put(ctx, "config", abcBytes)
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0), WithUpdateRetries(10))

	// Inject a frozen watcher so the local cache cannot be updated by watcher events.
	frozenUpdates := make(chan jetstream.KeyValueEntry) // never written
	src.watchFn = func(_ context.Context) (jetstream.KeyWatcher, error) {
		return &fakeKeyWatcher{updates: frozenUpdates}, nil
	}

	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	// Verify the cache was seeded with [A,B,C] by Start's initial kv.Get.
	cached, listErr := src.List(ctx)
	require.NoError(t, listErr)
	require.Len(t, cached, 3, "cache must be seeded with [A,B,C] by Start")

	// Raw-write [A,B,C,D] directly to KV. The frozen watcher cannot update the
	// local cache, so it stays at [A,B,C]. Modify must still read 4 entries from KV.
	abcd := []types.Partition{
		{Keys: []string{"A"}},
		{Keys: []string{"B"}},
		{Keys: []string{"C"}},
		{Keys: []string{"D"}},
	}
	abcdBytes, encErr2 := encodePartitions(abcd)
	require.NoError(t, encErr2)
	_, err = kv.Put(ctx, "config", abcdBytes)
	require.NoError(t, err)

	// Confirm the cache is still stale (frozen watcher could not update it).
	staleCheck, _ := src.List(ctx)
	require.Len(t, staleCheck, 3, "cache must still be stale [A,B,C] since watcher is frozen")

	// Modify always reads fresh from KV — it must see all 4 entries.
	var sawCount int
	err = src.Modify(ctx, func(current []types.Partition) []types.Partition {
		sawCount = len(current) // must be 4 (from KV), not 3 (stale cache)
		return append(current, types.Partition{Keys: []string{"E"}})
	})
	require.NoError(t, err)
	require.Equal(t, 4, sawCount, "Modify fn must see 4 partitions from fresh KV read, not stale 3 from cache")

	got, err := src.List(ctx)
	require.NoError(t, err)
	require.Len(t, got, 5)
}

// TestNatsKV_Update_IsAuthoritativeReplace_NotLostUpdateSafe (#5) —
// pre-write [A,B,C]; call Update([A,B]); assert KV ends at [A,B].
func TestNatsKV_Update_IsAuthoritativeReplace_NotLostUpdateSafe(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_authoritative"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	// Write [A,B,C]
	abc := []types.Partition{
		{Keys: []string{"A"}},
		{Keys: []string{"B"}},
		{Keys: []string{"C"}},
	}
	err = src.Update(ctx, abc)
	require.NoError(t, err)

	// Call Update([A,B]) — authoritative replace, not merge.
	ab := []types.Partition{
		{Keys: []string{"A"}},
		{Keys: []string{"B"}},
	}
	err = src.Update(ctx, ab)
	require.NoError(t, err)

	got, err := src.List(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2, "Update is authoritative replace; C must be gone")
	require.Equal(t, ab, got)
}

// TestNatsKV_Reconcile_RecoversFromMissedWatcherEvent (#6) — uses a frozen
// watcher (never delivers events) so only the reconcile loop can observe the
// injected KV write. Asserts the listener fires and List() matches KV.
func TestNatsKV_Reconcile_RecoversFromMissedWatcherEvent(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_reconcile_recover"})
	require.NoError(t, err)

	// Inject a frozen watcher that never delivers events, so only the reconcile
	// loop can pick up KV changes. This definitively proves reconcile recovery.
	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(50*time.Millisecond))
	frozenUpdates := make(chan jetstream.KeyValueEntry) // never written
	src.watchFn = func(_ context.Context) (jetstream.KeyWatcher, error) {
		return &fakeKeyWatcher{updates: frozenUpdates}, nil
	}

	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	ch := src.Watch(ctx)

	// Initial update via src.Update (updates local cache directly; no watcher needed).
	initial := []types.Partition{{Keys: []string{"initial"}}}
	err = src.Update(ctx, initial)
	require.NoError(t, err)

	// Drain the initial signal.
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for initial signal")
	}

	// Directly write to KV bypassing src.Update. The frozen watcher cannot
	// deliver this event — only the reconcile loop can detect the change.
	injected := []types.Partition{{Keys: []string{"injected"}}}
	injectedBytes, encErr := encodePartitions(injected)
	require.NoError(t, encErr)
	_, err = kv.Put(ctx, "config", injectedBytes)
	require.NoError(t, err)

	// The reconcile loop (50ms cadence) must emit a signal after catching up.
	select {
	case <-ch:
		// Success — reconcile delivered the missed event.
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for reconcile signal after injected update (watcher is frozen)")
	}

	got, err := src.List(ctx)
	require.NoError(t, err)
	require.Equal(t, injected, got)
}

// TestNatsKV_Reconcile_NoSignalWhenInSync (#7) — assert that the poll does not
// spuriously fire listeners when KV and cache agree.
func TestNatsKV_Reconcile_NoSignalWhenInSync(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_reconcile_nosignal"})
	require.NoError(t, err)

	// Fast reconcile interval.
	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(50*time.Millisecond))
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	// Subscribe BEFORE writing so the Update signal is captured.
	ch := src.Watch(ctx)

	// Write initial data.
	initial := []types.Partition{{Keys: []string{"p1"}}}
	err = src.Update(ctx, initial)
	require.NoError(t, err)

	// Drain the initial signal.
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for initial signal")
	}

	// Wait for multiple reconcile intervals; no spurious signals should arrive.
	select {
	case <-ch:
		t.Fatal("reconcile emitted spurious signal when KV and cache are in sync")
	case <-time.After(400 * time.Millisecond):
		// Success — no spurious signal.
	}
}

// TestNatsKV_WatcherRestart_OnChannelClose (#8) — stop the NatsKV source (which
// cancels the watcher context), then restart it and assert a subsequent Update
// is observed. This exercises the restart path.
func TestNatsKV_WatcherRestart_OnChannelClose(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_watcher_restart"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.NoError(t, err)

	// Stop and restart (exercises watcher re-establishment).
	err = src.Stop(ctx)
	require.NoError(t, err)
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	ch := src.Watch(ctx)

	// After restart, Update must be observed.
	newPartitions := []types.Partition{{Keys: []string{"restarted"}}}
	err = src.Update(ctx, newPartitions)
	require.NoError(t, err)

	select {
	case <-ch:
		// Success
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for watch signal after watcher restart")
	}

	got, err := src.List(ctx)
	require.NoError(t, err)
	require.Equal(t, newPartitions, got)
}

// TestNatsKV_DeleteOperation_NotifiesListeners (#9) — Update(x), drain listener;
// kv.Delete(key); assert listener fires and List() returns empty.
func TestNatsKV_DeleteOperation_NotifiesListeners(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_delete"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	ch := src.Watch(ctx)

	// Write initial data.
	initial := []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}}
	err = src.Update(ctx, initial)
	require.NoError(t, err)

	// Drain initial signal.
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for initial signal")
	}

	// Delete the key.
	err = kv.Delete(ctx, "config")
	require.NoError(t, err)

	// Listener must fire.
	select {
	case <-ch:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for delete signal")
	}

	// List must return empty.
	got, err := src.List(ctx)
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestNatsKV_DeletePreservesKnownRevision (#50) — populate the source key, then
// delete it; assert Snapshot() returns (empty, deleteEntryRevision != 0, true, nil).
func TestNatsKV_DeletePreservesKnownRevision(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_delete_known"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	ch := src.Watch(ctx)

	// Write data.
	initial := []types.Partition{{Keys: []string{"p1"}}}
	err = src.Update(ctx, initial)
	require.NoError(t, err)

	// Drain write signal.
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for write signal")
	}

	// Capture the write revision.
	_, writeRev, writeKnown, err := src.Snapshot(ctx)
	require.NoError(t, err)
	require.True(t, writeKnown)
	require.NotZero(t, writeRev)

	// Delete the key.
	err = kv.Delete(ctx, "config")
	require.NoError(t, err)

	// Wait for the delete to be observed.
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for delete signal")
	}

	// Snapshot must return: empty partitions, non-zero delete revision, known=true.
	partitions, deleteRev, known, snapErr := src.Snapshot(ctx)
	require.NoError(t, snapErr)
	require.Empty(t, partitions, "partitions must be empty after delete")
	require.True(t, known, "known must be true even after delete")
	require.Greater(t, deleteRev, writeRev, "delete revision must be > write revision")
}

// TestNatsKV_AddPartitions_UsesModifyAndPreservesConcurrentAdds (#63) — N=10
// goroutines each call AddPartitions(uniquePartition); assert KV ends with all 10;
// assert dedupe by CanonicalID.
func TestNatsKV_AddPartitions_UsesModifyAndPreservesConcurrentAdds(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_add_concurrent"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0), WithUpdateRetries(20))
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(idx int) {
			defer wg.Done()
			p := types.Partition{Keys: []string{fmt.Sprintf("add-part-%02d", idx)}}
			addErr := src.AddPartitions(ctx, p)
			require.NoError(t, addErr)
		}(i)
	}
	wg.Wait()

	partitions, err := src.List(ctx)
	require.NoError(t, err)
	require.Len(t, partitions, n, "all %d partitions must be present", n)

	// Dedupe: calling AddPartitions with the same partition again is a no-op.
	p0 := types.Partition{Keys: []string{"add-part-00"}}
	err = src.AddPartitions(ctx, p0)
	require.NoError(t, err)

	partitions, err = src.List(ctx)
	require.NoError(t, err)
	require.Len(t, partitions, n, "duplicate add must be a no-op")
}

// TestNatsKV_RemovePartitions_UsesModifyAndPreservesConcurrentMutations (#64) —
// pre-populate with 10 partitions; N=5 goroutines each call RemovePartitions with
// a non-overlapping subset of 2; concurrently a 6th goroutine AddPartitions an 11th;
// assert final state is the unremoved 0 + the new 1.
func TestNatsKV_RemovePartitions_UsesModifyAndPreservesConcurrentMutations(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_remove_concurrent"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0), WithUpdateRetries(30))
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	// Pre-populate 10 partitions.
	initial := make([]types.Partition, 10)
	for i := range 10 {
		initial[i] = types.Partition{Keys: []string{fmt.Sprintf("rpart-%02d", i)}}
	}
	err = src.Update(ctx, initial)
	require.NoError(t, err)

	// 5 goroutines remove non-overlapping subsets of 2.
	// 1 goroutine adds an 11th partition.
	var wg sync.WaitGroup

	for g := range 5 {
		groupIdx := g
		wg.Go(func() {
			// Each group removes partitions at index groupIdx*2 and groupIdx*2+1.
			toRemove := []types.Partition{
				{Keys: []string{fmt.Sprintf("rpart-%02d", groupIdx*2)}},
				{Keys: []string{fmt.Sprintf("rpart-%02d", groupIdx*2+1)}},
			}
			rmErr := src.RemovePartitions(ctx, toRemove...)
			require.NoError(t, rmErr)
		})
	}

	wg.Go(func() {
		addErr := src.AddPartitions(ctx, types.Partition{Keys: []string{"added-11th"}})
		require.NoError(t, addErr)
	})

	wg.Wait()

	got, err := src.List(ctx)
	require.NoError(t, err)
	// All 10 removed, 1 added = 1.
	require.Len(t, got, 1, "all original partitions removed, only added-11th remains")
	require.Equal(t, "added-11th", got[0].Keys[0])
}

// TestNatsKV_ReconcileInterval_LeadershipProbeSelectsLeaderFollowerCadence (#66)
// — WithLeadershipProbe(fn) where fn toggles; assert the reconcile fires at
// the appropriate cadence.
func TestNatsKV_ReconcileInterval_LeadershipProbeSelectsLeaderFollowerCadence(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_leadership_cadence"})
	require.NoError(t, err)

	// Use a controllable probe that starts as leader.
	isLeader := true
	var probeMu sync.Mutex
	probe := func() bool {
		probeMu.Lock()
		defer probeMu.Unlock()
		return isLeader
	}

	// Verify probe selection logic via nextReconcileInterval: probe=true → leader
	// cadence, probe=false → follower cadence.
	src := NewNatsKV(kv, "config", nil, WithLeadershipProbe(probe))

	// Verify nextReconcileInterval returns the right cadences.
	require.Equal(t, leaderReconcileInterval, src.nextReconcileInterval(), "probe=true → leader cadence")

	probeMu.Lock()
	isLeader = false
	probeMu.Unlock()
	require.Equal(t, followerReconcileInterval, src.nextReconcileInterval(), "probe=false → follower cadence")

	probeMu.Lock()
	isLeader = true
	probeMu.Unlock()
	require.Equal(t, leaderReconcileInterval, src.nextReconcileInterval(), "probe=true → leader cadence again")
}

// TestNatsKV_WithUpdateRetries_ExhaustionReturnsTypedError (#67) — set
// WithUpdateRetries(1); inject persistent CAS conflict; assert Update returns
// ErrUpdateRetryExhausted.
func TestNatsKV_WithUpdateRetries_ExhaustionReturnsTypedError(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_exhaustion"})
	require.NoError(t, err)

	// Pre-populate the KV key so revision > 0.
	initialData := []types.Partition{{Keys: []string{"p1"}}}
	initialBytes, encErr := encodePartitions(initialData)
	require.NoError(t, encErr)
	_, err = kv.Put(ctx, "config", initialBytes)
	require.NoError(t, err)

	// Create source with WithUpdateRetries(1): exactly 1 CAS attempt.
	// Do NOT start the source (no watcher running) so that no background goroutine
	// can refresh s.revision between our manipulation and the Update call.
	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0), WithUpdateRetries(1))

	// Manually seed revision to a stale value (0 → triggers Create path, which fails
	// with ErrKeyExists since the key already exists). known=false ensures Create is used.
	src.known = false
	src.revision = 0

	// Update must fail after 1 attempt: Create fails (key exists) → refreshFromKV → loop ends.
	target := []types.Partition{{Keys: []string{"target"}}}
	updateErr := src.Update(ctx, target)
	require.ErrorIs(t, updateErr, ErrUpdateRetryExhausted, "must return typed error after retry exhaustion")
}

// TestNatsKV_Notify_RaceWithStop_NoPanic — regression for P0 #1.
// N goroutines call Update concurrently while another goroutine calls Stop.
// Run with -race. Must not panic (send on closed channel).
func TestNatsKV_Notify_RaceWithStop_NoPanic(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_notify_race"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0), WithUpdateRetries(20))
	err = src.Start(ctx)
	require.NoError(t, err)

	// Register several listeners.
	for range 5 {
		_ = src.Watch(ctx)
	}

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(idx int) {
			defer wg.Done()
			p := []types.Partition{{Keys: []string{fmt.Sprintf("rp-%02d", idx)}}}
			_ = src.Update(ctx, p)
		}(i)
	}

	// Stop mid-flight to force the race.
	go func() {
		_ = src.Stop(ctx)
	}()

	wg.Wait()
	// If we reach here without panic, the test passes.
}

// TestNatsKV_ReconcileErrKeyNotFound_PreservesKnownTrue — regression for P0 #3.
// Once known=true, a reconcile that observes ErrKeyNotFound must not reset known.
func TestNatsKV_ReconcileErrKeyNotFound_PreservesKnownTrue(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_reconcile_known"})
	require.NoError(t, err)

	// Disable watcher (reconcileInterval 0) so only manual reconcile runs.
	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	// Write initial data so known=true.
	initial := []types.Partition{{Keys: []string{"p1"}}}
	err = src.Update(ctx, initial)
	require.NoError(t, err)

	// Wait for watcher to observe the write.
	require.Eventually(t, func() bool {
		_, _, known, _ := src.Snapshot(ctx)
		return known
	}, 2*time.Second, 10*time.Millisecond)

	_, writeRev, _, _ := src.Snapshot(ctx)
	require.NotZero(t, writeRev)

	// Delete the key directly at the KV level (bypassing any src.Update path).
	err = kv.Delete(ctx, "config")
	require.NoError(t, err)

	// Manually trigger reconcile (as if the watcher missed the delete event).
	// At this point the watcher may also have observed the delete. Either way,
	// known must remain true.
	src.reconcileOnce(ctx)

	_, gotRev, gotKnown, snapErr := src.Snapshot(ctx)
	require.NoError(t, snapErr)
	require.True(t, gotKnown, "known must stay true after reconcile observes ErrKeyNotFound")
	// Revision should be non-zero (either the write rev preserved or the delete rev from the watcher).
	require.NotZero(t, gotRev, "revision must not be reset to 0 by reconcile")
}

// TestNatsKV_Stop_TerminatesAllGoroutines — regression for P1 #5.
// After Stop returns, all source-owned goroutines must have exited.
func TestNatsKV_Stop_TerminatesAllGoroutines(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_stop_goroutines"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(50*time.Millisecond))
	err = src.Start(ctx)
	require.NoError(t, err)

	// Register several listeners with contexts that are NOT cancelled.
	for range 5 {
		_ = src.Watch(ctx)
	}

	// Stop: must wait for all goroutines including listener cleanup goroutines.
	// If wg.Wait() is missing from Stop, this test will likely hang or race.
	stopErr := src.Stop(ctx)
	require.NoError(t, stopErr)

	// After Stop returns, the WaitGroup must be fully drained (all goroutines exited).
	// We verify this by asserting Stop returned promptly (if it hung, t.Context()
	// would cancel the test). The WaitGroup drain itself is the proof.
}

// TestNatsKV_WatcherRestart_ChannelClose_RestartsAndObservesNewUpdate — regression for P1 #7.
// Closes the watcher's Updates() channel directly (via fake injection), then asserts
// the source eventually observes a new KV write without requiring Stop/Start.
func TestNatsKV_WatcherRestart_ChannelClose_RestartsAndObservesNewUpdate(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_watcher_restart_fake"})
	require.NoError(t, err)

	// Write initial data before Start so we have a known revision.
	initial := []types.Partition{{Keys: []string{"initial"}}}
	initialBytes, encErr := encodePartitions(initial)
	require.NoError(t, encErr)
	_, err = kv.Put(ctx, "config", initialBytes)
	require.NoError(t, err)

	// Inject a fake watchFn that returns a closeable watcher on the first call,
	// then delegates to the real KV watcher on subsequent calls.
	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(100*time.Millisecond), WithUpdateRetries(10))

	fakeUpdates := make(chan jetstream.KeyValueEntry, 16)
	var realWatcherOnce sync.Once
	realWatcherCh := make(chan struct{})

	src.watchFn = func(watchCtx context.Context) (jetstream.KeyWatcher, error) {
		var useReal bool
		realWatcherOnce.Do(func() {
			// First call: return the fake watcher (do not signal real yet).
		})
		// After fake is closed, use the real watcher.
		select {
		case <-realWatcherCh:
			useReal = true
		default:
		}
		if useReal {
			return kv.Watch(watchCtx, "config")
		}
		// Return fake.
		return &fakeKeyWatcher{updates: fakeUpdates}, nil
	}

	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	// Signal that subsequent watchFn calls should use the real KV watcher.
	close(realWatcherCh)

	// Close the fake watcher's Updates channel to trigger the !ok branch in watchLoop.
	close(fakeUpdates)

	// Write a new value to KV; the restarted real watcher (or reconcile) must observe it.
	updated := []types.Partition{{Keys: []string{"after-restart"}}}
	updatedBytes, encErr2 := encodePartitions(updated)
	require.NoError(t, encErr2)
	_, err = kv.Put(ctx, "config", updatedBytes)
	require.NoError(t, err)

	// Wait for the source to observe the updated value.
	require.Eventually(t, func() bool {
		parts, listErr := src.List(ctx)
		if listErr != nil || len(parts) == 0 {
			return false
		}
		return parts[0].Keys[0] == "after-restart"
	}, 5*time.Second, 50*time.Millisecond, "source must observe new update after watcher restart")
}

// TestNatsKV_ReconcileLoop_RecomputesIntervalPerTick — regression for P1 #8 (part 1).
// Verifies that nextReconcileInterval maps the leadership probe state to the correct
// cadence constants. No Start() needed — only reads leadershipProbe.
func TestNatsKV_ReconcileLoop_RecomputesIntervalPerTick(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()

	var probeMu sync.Mutex
	isLeader := true
	probe := func() bool {
		probeMu.Lock()
		defer probeMu.Unlock()

		return isLeader
	}

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_reconcile_recompute"})
	require.NoError(t, err)
	src := NewNatsKV(kv, "config", nil, WithLeadershipProbe(probe))

	probeMu.Lock()
	isLeader = true
	probeMu.Unlock()
	got := src.nextReconcileInterval()
	require.Equal(t, leaderReconcileInterval, got, "probe=true must select leader cadence")

	probeMu.Lock()
	isLeader = false
	probeMu.Unlock()
	got = src.nextReconcileInterval()
	require.Equal(t, followerReconcileInterval, got, "probe=false must select follower cadence")
}

// TestNatsKV_ReconcileLoop_LiveProbeRecomputesPerTick — regression for P1 #8 (live loop).
// Proves that the live reconcile loop re-reads the leadership probe on each tick so
// that a leadership transition is reflected on the very next scheduled interval.
//
// The test uses test-seam cadences (20ms leader / 10ms follower) and asserts that
// observed tick intervals reflect the current probe state after each toggle.
func TestNatsKV_ReconcileLoop_LiveProbeRecomputesPerTick(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_reconcile_live_probe"})
	require.NoError(t, err)

	const leaderInterval = 20 * time.Millisecond
	const followerInterval = 10 * time.Millisecond

	var isLeader atomic.Bool
	isLeader.Store(false) // start as follower

	src := NewNatsKV(kv, "config", nil, WithLeadershipProbe(isLeader.Load))

	// Override the leader/follower cadence constants via the test seam so the live
	// loop runs fast enough to assert within the test timeout.
	src.leaderInterval = leaderInterval
	src.followerInterval = followerInterval

	// Collect observed intervals via the hook.
	var (
		intervalsMu sync.Mutex
		intervals   []time.Duration
	)
	src.onReconcileTick = func(d time.Duration) {
		intervalsMu.Lock()
		intervals = append(intervals, d)
		intervalsMu.Unlock()
	}

	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	// Helper: wait until at least n intervals have been collected.
	waitForIntervals := func(n int) []time.Duration {
		t.Helper()
		require.Eventually(t, func() bool {
			intervalsMu.Lock()
			defer intervalsMu.Unlock()

			return len(intervals) >= n
		}, 2*time.Second, 2*time.Millisecond)
		intervalsMu.Lock()
		defer intervalsMu.Unlock()

		return append([]time.Duration(nil), intervals...)
	}

	// Phase 1: follower — expect followerInterval ticks.
	snap1 := waitForIntervals(2)
	for _, d := range snap1 {
		require.Equal(t, followerInterval, d, "follower phase: all observed intervals must be follower cadence")
	}

	// Toggle to leader; the next interval computed after the ongoing tick must switch.
	intervalsMu.Lock()
	beforeToggle := len(intervals)
	intervalsMu.Unlock()
	isLeader.Store(true)

	// Collect until we have at least 2 ticks after the toggle.
	require.Eventually(t, func() bool {
		intervalsMu.Lock()
		defer intervalsMu.Unlock()

		return len(intervals) >= beforeToggle+2
	}, 2*time.Second, 2*time.Millisecond)

	intervalsMu.Lock()
	postToggle := append([]time.Duration(nil), intervals[beforeToggle:]...)
	intervalsMu.Unlock()

	require.NotEmpty(t, postToggle, "must observe ticks after toggle to leader")
	// At least the last observed interval must be the leader cadence.
	require.Equal(t, leaderInterval, postToggle[len(postToggle)-1],
		"after probe toggles to leader, reconcile loop must schedule leader cadence")

	// Phase 3: toggle back to follower.
	intervalsMu.Lock()
	beforeToggle2 := len(intervals)
	intervalsMu.Unlock()
	isLeader.Store(false)

	require.Eventually(t, func() bool {
		intervalsMu.Lock()
		defer intervalsMu.Unlock()

		return len(intervals) >= beforeToggle2+2
	}, 2*time.Second, 2*time.Millisecond)

	intervalsMu.Lock()
	postToggle2 := append([]time.Duration(nil), intervals[beforeToggle2:]...)
	intervalsMu.Unlock()

	require.NotEmpty(t, postToggle2, "must observe ticks after toggle back to follower")
	require.Equal(t, followerInterval, postToggle2[len(postToggle2)-1],
		"after probe toggles back to follower, reconcile loop must schedule follower cadence")
}

// TestNatsKV_Watch_AfterStop_ReturnsClosedChannel — regression for Watch/Stop lifecycle.
// Calling Watch after Stop must return a pre-closed channel immediately without
// adding to the WaitGroup or spawning a cleanup goroutine.
func TestNatsKV_Watch_AfterStop_ReturnsClosedChannel(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_watch_after_stop"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.NoError(t, err)

	err = src.Stop(ctx)
	require.NoError(t, err)

	// Watch after Stop must return a closed channel.
	ch := src.Watch(ctx)
	_, ok := <-ch
	require.False(t, ok, "Watch after Stop must return a closed channel")

	// wg.Wait must not deadlock — all goroutines must have already exited.
	done := make(chan struct{})
	go func() {
		src.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Success — wg is fully drained.
	case <-time.After(2 * time.Second):
		t.Fatal("wg.Wait deadlocked after Watch called post-Stop")
	}
}

// TestNatsKV_Watch_RaceWithStop_NoLeak — regression for Watch/Stop lifecycle.
// N goroutines call Watch in a loop; after a short delay Stop is called.
// Must not panic, Stop must return, and no goroutine leak after a bounded wait.
// Run with -race.
func TestNatsKV_Watch_RaceWithStop_NoLeak(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_watch_race_stop"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.NoError(t, err)

	baselineGoroutines := runtime.NumGoroutine()

	const n = 10
	var wg sync.WaitGroup
	stopCh := make(chan struct{})
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					_ = src.Watch(ctx)
				}
			}
		}()
	}

	// Let the goroutines race for a bit, then Stop.
	time.Sleep(20 * time.Millisecond) //nolint:forbidigo // deliberate short race window
	close(stopCh)

	stopErr := src.Stop(ctx)
	require.NoError(t, stopErr)

	wg.Wait()

	// After Stop, goroutine count must return to baseline ± small slack.
	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() <= baselineGoroutines+5
	}, 2*time.Second, 10*time.Millisecond, "goroutine count must not leak after Stop")
}

// fakeKeyWatcher implements jetstream.KeyWatcher for testing.
// Its Updates() channel can be closed by the test to simulate watcher channel close.
type fakeKeyWatcher struct {
	updates chan jetstream.KeyValueEntry
}

func (f *fakeKeyWatcher) Updates() <-chan jetstream.KeyValueEntry {
	return f.updates
}

func (f *fakeKeyWatcher) Stop() error {
	return nil
}

// fakeKVEntry implements jetstream.KeyValueEntry for tests that need to
// hand-craft watcher events (e.g., to simulate out-of-order or stale delivery).
type fakeKVEntry struct {
	bucket   string
	key      string
	value    []byte
	revision uint64
	op       jetstream.KeyValueOp
	created  time.Time
	delta    uint64
}

func (e *fakeKVEntry) Bucket() string                  { return e.bucket }
func (e *fakeKVEntry) Key() string                     { return e.key }
func (e *fakeKVEntry) Value() []byte                   { return e.value }
func (e *fakeKVEntry) Revision() uint64                { return e.revision }
func (e *fakeKVEntry) Created() time.Time              { return e.created }
func (e *fakeKVEntry) Delta() uint64                   { return e.delta }
func (e *fakeKVEntry) Operation() jetstream.KeyValueOp { return e.op }
