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

// TestNatsKV_Watch_StaleEventIgnored is a deterministic reproducer for the
// CI flake in TestNatsKV_Watch_Deduplication: under load, a watcher event
// for an earlier KV revision can drain from watcher.Updates() *after* a
// later local Update has already advanced s.partitions. Without revision
// gating, applyLocalLocked sees the stale event's content as a "change"
// (since s.partitions has moved on) and fires a spurious signal — and
// also regresses s.revision, which can cause the next CAS attempt to
// see a stale revision.
//
// The reproducer uses a controllable fake watcher so we can deliver the
// stale event at the exact moment the bug requires, rather than relying
// on scheduler luck.
func TestNatsKV_Watch_StaleEventIgnored(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := context.Background()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_stale_watcher"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))

	// Inject a controllable fake watcher: writes still go through the real
	// kv, but the watchLoop only sees events we manually deliver via fakeUpdates.
	fakeUpdates := make(chan jetstream.KeyValueEntry, 16)
	src.watchFn = func(_ context.Context) (jetstream.KeyWatcher, error) {
		return &fakeKeyWatcher{updates: fakeUpdates}, nil
	}

	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	ch := src.Watch(ctx)

	// 1. Update writes p1; local apply advances s.partitions to [{p1}] at rev=1.
	p1 := []types.Partition{{Keys: []string{"p1"}, Weight: 100}}
	require.NoError(t, src.Update(ctx, p1))
	select {
	case <-ch:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for p1 signal")
	}

	// 2. Update writes p2; local apply advances s.partitions to [{p2}] at rev=2.
	p2 := []types.Partition{{Keys: []string{"p2"}, Weight: 100}}
	require.NoError(t, src.Update(ctx, p2))
	select {
	case <-ch:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for p2 signal")
	}

	// 3. Simulate a watcher event for the *earlier* revision arriving late.
	//    Under the bug, watchLoop -> applyLocal -> applyLocalLocked sees
	//    s.partitions=[{p2}] and incoming sorted=[{p1}] → changed=true → signal.
	stalePayload, encErr := partcodec.Encode(p1)
	require.NoError(t, encErr)
	fakeUpdates <- &fakeKVEntry{
		key:      "config",
		value:    stalePayload,
		revision: 1, // stale: current is 2
		op:       jetstream.KeyValuePut,
	}

	select {
	case <-ch:
		t.Fatal("received spurious signal from stale watcher event (rev 1 < current 2)")
	case <-time.After(500 * time.Millisecond):
		// Expected: stale event is ignored.
	}

	// 4. Verify s.revision did NOT regress.
	_, gotRev, gotKnown, err := src.Snapshot(ctx)
	require.NoError(t, err)
	require.True(t, gotKnown)
	require.Equal(t, uint64(2), gotRev, "stale watcher event must not regress s.revision")

	// 5. Verify s.partitions is still [{p2}] — the stale event must not have
	//    replaced the current state with the older content.
	got, err := src.List(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "p2", got[0].Keys[0], "stale watcher event must not regress s.partitions")
}

// TestNatsKV_Watch_Deduplication asserts that only real changes trigger signals.
func TestNatsKV_Watch_Deduplication(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := context.Background()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_dedup"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	ch := src.Watch(ctx)

	// 1. Initial Update -> Should trigger signal
	p1 := []types.Partition{{Keys: []string{"p1"}, Weight: 100}}
	err = src.Update(ctx, p1)
	require.NoError(t, err)

	select {
	case <-ch:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for initial signal")
	}

	// 2. Identical Update -> Should NOT trigger signal
	err = src.Update(ctx, p1)
	require.NoError(t, err)

	select {
	case <-ch:
		t.Fatal("received signal for identical update")
	case <-time.After(500 * time.Millisecond):
		// Success (timeout expected)
	}

	// 3. Different Update (Weight change) -> Should trigger signal
	p2 := []types.Partition{{Keys: []string{"p1"}, Weight: 200}}
	err = src.Update(ctx, p2)
	require.NoError(t, err)

	select {
	case <-ch:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for weight change signal")
	}

	// 4. Different Update (Key change) -> Should trigger signal
	p3 := []types.Partition{{Keys: []string{"p1", "sub"}, Weight: 200}}
	err = src.Update(ctx, p3)
	require.NoError(t, err)

	select {
	case <-ch:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for key change signal")
	}

	// 5. Multiple partitions
	pMulti := []types.Partition{
		{Keys: []string{"a"}, Weight: 100},
		{Keys: []string{"b"}, Weight: 100},
	}
	err = src.Update(ctx, pMulti)
	require.NoError(t, err)

	select {
	case <-ch:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for multi partition signal")
	}

	// 6. Reordered Multiple partitions -> Should NOT trigger signal (canonical sorting)
	pMultiReordered := []types.Partition{
		{Keys: []string{"b"}, Weight: 100},
		{Keys: []string{"a"}, Weight: 100},
	}
	err = src.Update(ctx, pMultiReordered)
	require.NoError(t, err)

	select {
	case <-ch:
		t.Fatal("received signal for reordered update")
	case <-time.After(500 * time.Millisecond):
		// Success
	}
}

// TestNatsKV_Update_RejectsInvalidPartition (#44) — write a partition with a dot
// in its key; assert Update and Modify return error.
func TestNatsKV_Update_RejectsInvalidPartition(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_invalid"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	invalid := []types.Partition{
		{Keys: []string{"topic.name"}}, // dot in key is invalid
	}

	// Update must reject.
	err = src.Update(ctx, invalid)
	require.Error(t, err, "Update must reject partition with dot in key")

	// Modify must also reject.
	err = src.Modify(ctx, func(_ []types.Partition) []types.Partition {
		return invalid
	})
	require.Error(t, err, "Modify must reject partition with dot in key")

	// KV must remain empty (no write occurred).
	got, listErr := src.List(ctx)
	require.NoError(t, listErr)
	require.Empty(t, got, "no invalid partition must reach KV")
}

// TestNatsKV_Update_RejectsDuplicateIDs (#45) — write two partitions with the
// same CanonicalID(); assert error.
func TestNatsKV_Update_RejectsDuplicateIDs(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_dup_ids"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	// Two partitions with the same CanonicalID (same key set).
	dups := []types.Partition{
		{Keys: []string{"same"}, Weight: 1},
		{Keys: []string{"same"}, Weight: 2},
	}

	err = src.Update(ctx, dups)
	require.Error(t, err, "Update must reject duplicate CanonicalIDs")

	// Modify must also reject.
	err = src.Modify(ctx, func(_ []types.Partition) []types.Partition {
		return dups
	})
	require.Error(t, err, "Modify must reject duplicate CanonicalIDs")
}

// TestNatsKV_List_DeepCopiesKeys (#46) — covers two aliasing hazards:
// (a) caller mutates the input Keys slice after Update returns (store-side aliasing),
// (b) caller mutates the List() output (output-side aliasing).
func TestNatsKV_List_DeepCopiesKeys(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()

	t.Run("caller-side aliasing: mutate input after Update", func(t *testing.T) {
		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_deepcopy_caller"})
		require.NoError(t, err)

		src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
		err = src.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = src.Stop(ctx) }()

		// Build the input with a caller-owned keys slice.
		callerKeys := []string{"original"}
		input := []types.Partition{{Keys: callerKeys, Weight: 100}}
		err = src.Update(ctx, input)
		require.NoError(t, err)

		// Wait for watcher to apply.
		require.Eventually(t, func() bool {
			p, listErr := src.List(ctx)
			return listErr == nil && len(p) == 1
		}, 2*time.Second, 10*time.Millisecond)

		// Mutate the caller's original slice AFTER Update returned.
		callerKeys[0] = "mutated-by-caller"

		// Internal state must still reflect the original value.
		got, err := src.List(ctx)
		require.NoError(t, err)
		require.Equal(t, "original", got[0].Keys[0], "internal keys must not alias caller-owned slice")
	})

	t.Run("output-side aliasing: mutate List output", func(t *testing.T) {
		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_deepcopy_output"})
		require.NoError(t, err)

		src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
		err = src.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = src.Stop(ctx) }()

		initial := []types.Partition{{Keys: []string{"p1"}, Weight: 100}}
		err = src.Update(ctx, initial)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			p, listErr := src.List(ctx)
			return listErr == nil && len(p) == 1
		}, 2*time.Second, 10*time.Millisecond)

		// Get a copy and mutate it.
		got, err := src.List(ctx)
		require.NoError(t, err)
		require.Len(t, got, 1)
		got[0].Keys[0] = "mutated"
		got[0].Weight = 9999

		// Internal state must be unchanged.
		got2, err := src.List(ctx)
		require.NoError(t, err)
		require.Equal(t, "p1", got2[0].Keys[0], "internal keys must not be mutated via List output")
		require.Equal(t, int64(100), got2[0].Weight, "internal weight must not be mutated via List output")
	})
}

// TestNatsKV_Start_RejectsCorruptKVData_InvalidPartition — regression for P0 #4.
// A raw KV value with an invalid partition must cause Start to return an error.
func TestNatsKV_Start_RejectsCorruptKVData_InvalidPartition(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_corrupt_invalid"})
	require.NoError(t, err)

	// Raw-write a partition with a dot in the key (invalid per Validate).
	corruptData := []byte(`[{"keys":["topic.name"],"weight":1}]`)
	_, err = kv.Put(ctx, "config", corruptData)
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.Error(t, err, "Start must return error when KV contains an invalid partition")
}

// TestNatsKV_Start_RejectsCorruptKVData_DuplicateCanonicalID — regression for P0 #4.
// A raw KV value with duplicate CanonicalIDs must cause Start to return an error.
func TestNatsKV_Start_RejectsCorruptKVData_DuplicateCanonicalID(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_corrupt_dupid"})
	require.NoError(t, err)

	// Raw-write two partitions with identical CanonicalIDs.
	corruptData := []byte(`[{"keys":["same"],"weight":1},{"keys":["same"],"weight":2}]`)
	_, err = kv.Put(ctx, "config", corruptData)
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.Error(t, err, "Start must return error when KV contains duplicate CanonicalIDs")
}

// TestNatsKV_Watcher_IgnoresCorruptUpdate_PreservesLocalState — regression for P0 #4.
// When the watcher delivers a corrupt update (duplicate CanonicalIDs), local state
// must be preserved and no listener signal emitted.
func TestNatsKV_Watcher_IgnoresCorruptUpdate_PreservesLocalState(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "partitions_corrupt_watcher"})
	require.NoError(t, err)

	src := NewNatsKV(kv, "config", nil, WithReconcileInterval(0))
	err = src.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = src.Stop(ctx) }()

	// Write valid [A, B] via Update.
	valid := []types.Partition{{Keys: []string{"A"}}, {Keys: []string{"B"}}}
	err = src.Update(ctx, valid)
	require.NoError(t, err)

	ch := src.Watch(ctx)

	// Wait for watcher to apply the valid state.
	require.Eventually(t, func() bool {
		p, listErr := src.List(ctx)
		return listErr == nil && len(p) == 2
	}, 2*time.Second, 10*time.Millisecond)

	// Drain any initial signal.
	select {
	case <-ch:
	default:
	}

	// Raw-write [A, A] (duplicate CanonicalIDs) directly to KV.
	corruptData := []byte(`[{"keys":["A"],"weight":0},{"keys":["A"],"weight":0}]`)
	_, err = kv.Put(ctx, "config", corruptData)
	require.NoError(t, err)

	// Give the watcher time to process the corrupt entry.
	// We expect no listener signal because the corrupt update is rejected.
	select {
	case <-ch:
		t.Fatal("listener fired on corrupt update — local state should not have changed")
	case <-time.After(500 * time.Millisecond):
		// Expected: no signal.
	}

	// Local state must still be [A, B].
	got, err := src.List(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2, "local state must be preserved on corrupt watcher update")
}
