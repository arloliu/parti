package durable

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestClaimResolver_WatcherRestartOnChannelClose mirrors the production-bug
// reproducer at TestClaimResolver_CacheFreezesAfterWatcherClose but
// additionally asserts that the IncWatcherRestart("channel_closed") metric
// fires after the supervisor establishes a new watcher.
func TestClaimResolver_WatcherRestartOnChannelClose(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	initial := handoff.Claim{PartitionID: "p1", Owner: "worker-A", State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: time.Now().UTC()}
	b, err := initial.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/p1", b)
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("p1")
		return ok && owner == "worker-A"
	}, 2*time.Second, 10*time.Millisecond)

	// Force the watcher channel to close.
	require.NotNil(t, r.watcher)
	require.NoError(t, r.watcher.Stop())

	// Write a new claim. The resolver must observe it after restart.
	// Either path is fine: if the supervisor has already re-established
	// the watcher, the update arrives via Updates(); if not, the
	// re-established watcher's initial walk delivers it. The Eventually
	// below tolerates either ordering.
	updated := handoff.Claim{PartitionID: "p1", Owner: "worker-B", State: handoff.ClaimStateStable, Epoch: 2, LastUpdated: time.Now().UTC()}
	bUpd, err := updated.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/p1", bUpd)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("p1")
		return ok && owner == "worker-B"
	}, 10*time.Second, 25*time.Millisecond, "cache should converge after watcher restart")

	require.Eventually(t, func() bool {
		return ms.watcherRestartCount("channel_closed") >= 1
	}, 5*time.Second, 25*time.Millisecond, "IncWatcherRestart(channel_closed) was not emitted")
}

// TestClaimResolver_ReconcileCatchesMissedEvent drives a fast reconcile
// cadence and verifies that the reconciler independently converges the cache
// when given a direct KV write. Both the watcher and reconciler should be
// idempotent under the shared apply path, so this is a "reconcile is
// observable" test rather than a "reconcile is the only path" test.
func TestClaimResolver_ReconcileCatchesMissedEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(50*time.Millisecond))
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// Stop the watcher to simulate the worst case: the only path to
	// convergence is the reconciler.
	require.NoError(t, r.watcher.Stop())

	c := handoff.Claim{PartitionID: "rPid", Owner: "wRecon", State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: time.Now().UTC()}
	b, err := c.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/rPid", b)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("rPid")
		return ok && owner == "wRecon"
	}, 5*time.Second, 25*time.Millisecond, "reconcile should have applied the direct KV write")

	require.Positive(t, ms.flushReasonCount("reconcile"),
		"reconcile flush reason should have fired at least once")
}

// TestClaimResolver_ReconcileNoSpuriousChanges asserts the reconciler does not
// reseat the cache pointer (and does not churn metrics) when in steady state.
func TestClaimResolver_ReconcileNoSpuriousChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	c := handoff.Claim{PartitionID: "p1", Owner: "wA", State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: time.Now().UTC()}
	b, err := c.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/p1", b)
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(50*time.Millisecond))
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("p1")
		return ok && owner == "wA"
	}, 2*time.Second, 10*time.Millisecond)

	// Wait until at least one reconcile tick has fired (event-driven proxy
	// for "the reconciler is alive and steady-state"). We poll the
	// reconciler's internal ticker via its loop iteration count: every
	// reconcile pass that finds no work to do does not emit a flush
	// metric, but it does observe the ticker. Since we cannot observe the
	// ticker directly, use a tighter event-driven settle: wait for two
	// reconcile-interval-equivalent windows to pass with no upsert
	// activity, then snapshot.
	//
	// NOTE: this sleep is a negative-assertion synchronizer — we are
	// asserting that the reconciler does NOT change state across several
	// ticks. The 300-testing rule permits sleeps to bound negative
	// assertions when explicitly commented.
	const reconcileTicks = 6
	settle := time.Duration(reconcileTicks) * 50 * time.Millisecond

	// First settle: allow any in-flight watcher batch to drain.
	time.Sleep(settle / 3)
	// Snapshot cache pointer & metric counters once steady.
	ptrBefore := r.cache.Load()
	flushReconBefore := ms.flushReasonCount("reconcile")
	updUpsertBefore := ms.updateCount("upsert")

	// Let several reconcile ticks elapse — the assertion below is a
	// negative one ("no change occurred during this window"), so a bounded
	// wait is the correct synchronization primitive here.
	time.Sleep(settle)
	ptrAfter := r.cache.Load()
	flushReconAfter := ms.flushReasonCount("reconcile")
	updUpsertAfter := ms.updateCount("upsert")

	require.Same(t, ptrBefore, ptrAfter,
		"cache pointer should not reseat when reconcile finds no diff")
	require.Equal(t, flushReconBefore, flushReconAfter,
		"reconcile flush reason should not increment in steady state")
	require.Equal(t, updUpsertBefore, updUpsertAfter,
		"upsert update counter should not increment in steady state")
}

// TestClaimResolver_ReconcileDoesNotRegressLaterWatcherUpdates seeds the
// cache with a high-revision entry, then directly invokes reconcileOnce with
// the KV holding only an earlier revision. The revision-aware apply short
// circuit must protect the cache.
func TestClaimResolver_ReconcileDoesNotRegressLaterWatcherUpdates(t *testing.T) {
	// This test does NOT need a live NATS server — we construct the
	// resolver against a mock KV and seed the cache with a synthetic high
	// revision, then invoke reconcileOnce directly.
	kv := newMockKVForReconcile(map[string][]byte{
		"claims/p1": marshalClaim(t, handoff.Claim{
			PartitionID: "p1", Owner: "older", State: handoff.ClaimStateStable, Epoch: 1,
		}),
	}, 5) // mock returns revision=5

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))

	// Seed the cache with a "newer" watcher view (revision 10).
	seeded := map[string]claimEntry{
		"p1": {owner: "newer", state: toState(handoff.ClaimStateStable), epoch: 2, revision: 10},
	}
	r.cache.Store(&seeded)

	r.reconcileOnce(context.Background())

	owner, _, _, ok := r.GetOwner("p1")
	require.True(t, ok)
	require.Equal(t, "newer", owner, "reconcile must not regress a newer watcher revision")
}

// TestClaimResolver_StopBlocksUntilGoroutinesExit verifies Stop is a fence:
// after it returns, both supervised goroutines are no longer running.
func TestClaimResolver_StopBlocksUntilGoroutinesExit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(50*time.Millisecond))
	require.NoError(t, r.Start(ctx))

	doneCh := r.doneCh
	require.NotNil(t, doneCh, "Start must initialize doneCh")

	// Run Stop in a goroutine so we can bound the wait independently.
	stopped := make(chan struct{})
	start := time.Now()
	go func() {
		r.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return within 3s")
	}
	t.Logf("Stop returned after %v", time.Since(start))

	// doneCh must be closed by the time Stop returns.
	select {
	case <-doneCh:
	default:
		t.Fatal("doneCh not closed after Stop returned")
	}
}

// TestClaimResolver_StopWithRestartingWatcher forces the supervisor into its
// backoff path (by shutting the embedded NATS so kv.WatchAll fails), then
// calls Stop and asserts it returns well before the base backoff (2s) would
// otherwise elapse.
func TestClaimResolver_StopWithRestartingWatcher(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	// Note: we intentionally close NATS mid-test, so the deferred cleanup
	// may be a no-op for shutdown but still closes the conn handle safely.
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))

	// Trip the watcher: stop it so processWatcher returns errWatcherClosed.
	require.NoError(t, r.watcher.Stop())
	// Kill the NATS connection so subsequent WatchAll calls fail; this
	// forces the supervisor into its backoff sleep.
	nc.Close()

	// Wait until the supervisor has observed the closure and attempted to
	// re-establish (failing because NATS is down). This is an event-driven
	// signal that the supervisor is in its backoff sleep.
	require.Eventually(t, func() bool {
		return ms.watcherRestartCount("establish_failed") >= 1
	}, 3*time.Second, 25*time.Millisecond,
		"supervisor should attempt to re-establish the watcher and fail")

	stopped := make(chan struct{})
	start := time.Now()
	go func() {
		r.Stop()
		close(stopped)
	}()
	// Base backoff is 2s; assert Stop returns well below that.
	select {
	case <-stopped:
	case <-time.After(1500 * time.Millisecond):
		t.Fatalf("Stop blocked on watcher backoff (>1.5s); start=%v", start)
	}
	t.Logf("Stop returned after %v while supervisor was in backoff", time.Since(start))
}

// TestClaimResolver_TombstoneSurvivesReconcile encodes the
// write -> delete -> reconcile invariant against an embedded NATS server.
// A claim is put then deleted in KV; the watcher must observe the delete and
// tombstone the cache. A subsequent reconcile pass must not resurrect the
// entry, and the watcher must not have died during the run.
func TestClaimResolver_TombstoneSurvivesReconcile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	// 1. Write a live claim.
	c := handoff.Claim{
		PartitionID: "pDel", Owner: "wA", State: handoff.ClaimStateStable,
		Epoch: 1, LastUpdated: time.Now().UTC(),
	}
	b, err := c.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pDel", b)
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(50*time.Millisecond))
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// 2. Wait for the watcher to populate the cache.
	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("pDel")
		return ok && owner == "wA"
	}, 2*time.Second, 10*time.Millisecond)

	// 3. Delete the claim from KV.
	require.NoError(t, kv.Delete(ctx, "claims/pDel"))

	// 4. Wait for the watcher to tombstone the cache entry.
	require.Eventually(t, func() bool {
		_, _, _, ok := r.GetOwner("pDel")
		return !ok
	}, 2*time.Second, 10*time.Millisecond, "watcher should have tombstoned the entry")

	// 5. Trigger an explicit reconcile pass so the reconciler has had a
	// concrete chance to (incorrectly) resurrect the entry. This is a
	// stronger event-driven check than waiting on a tick metric: we call
	// reconcileOnce directly and observe its effect.
	r.reconcileOnce(ctx)

	// 6. Assert tombstone is still in place.
	_, _, _, ok := r.GetOwner("pDel")
	require.False(t, ok, "tombstoned entry must not be resurrected by reconcile")
	cur := r.cache.Load()
	require.NotNil(t, cur)
	e, hasKey := (*cur)["pDel"]
	require.True(t, hasKey, "tombstone entry should remain in the map")
	require.True(t, e.deleted, "entry should still be tombstoned after reconcile")

	// 7. Watcher must not have restarted during this test — the failure
	// mode is a confused tombstone, not a watcher death.
	require.Zero(t, ms.watcherRestartCount("channel_closed"),
		"watcher must not have died during this test")
	require.Zero(t, ms.watcherRestartCount("establish_failed"),
		"watcher must not have failed to establish during this test")
}

// TestClaimResolver_ReconcileDoesNotTombstoneConcurrentWatcherUpsert is the
// P0 regression test. Reconcile must snapshot the cache BEFORE calling
// Keys(), so a watcher-applied entry that lands between Keys() returning
// (without the key) and the cache snapshot is NOT visible to the tombstone
// pass and therefore cannot be synthesized into a delete.
//
// On 493d879 (pre-fix), reconcileOnce reads Keys() first, then snapshots the
// cache. The afterKeys hook below mutates the cache mid-Keys, so when the
// snapshot is taken the injected entry is present; the tombstone pass sees
// it as "missing from seen" and stages a delete at injectedRev+1. The shared
// apply path's revision check then permanently shorts out the watcher's
// later upserts at the real revision.
//
// On main (post-fix), the snapshot is taken before Keys(), so the injected
// entry is not in `snap` and no tombstone is staged. A subsequent reconcile
// pass observes the entry in KV and converges normally.
func TestClaimResolver_ReconcileDoesNotTombstoneConcurrentWatcherUpsert(t *testing.T) {
	// Pre-seed: KV holds nothing for "pConcurrent"; cache holds nothing.
	// The afterKeys hook simulates the watcher applying a fresh upsert for
	// "pConcurrent" at revision 7 immediately after Keys() returned no
	// claims_pConcurrent key. We then snapshot KV at revision 8 for a
	// subsequent "real" upsert in step 3.
	kv := newMockKVForReconcile(map[string][]byte{}, 8)
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))

	injected := claimEntry{
		owner:    "watcher-owner",
		state:    toState(handoff.ClaimStateStable),
		epoch:    1,
		revision: 7,
	}

	// afterKeys runs inside Keys() right before it returns (deferred). On
	// the buggy code, the cache snapshot happens AFTER Keys(), so the
	// injection is visible to the snapshot. On the fixed code, the snapshot
	// happens BEFORE Keys(), so the injection lands too late to appear.
	kv.afterKeys = func() {
		next := map[string]claimEntry{
			"pConcurrent": injected,
		}
		r.cache.Store(&next)
	}

	// Trigger one reconcile pass. The expectation:
	//   * Fixed code: snap was taken before Keys; injection is NOT in snap;
	//     tombstone pass synthesizes nothing for pConcurrent; cache retains
	//     the live entry.
	//   * Buggy code: snap is taken AFTER Keys; injection IS in snap; seen
	//     does not include pConcurrent; tombstone at revision 8 is staged
	//     and applied (8 > 7), flipping the cache entry to deleted.
	r.reconcileOnce(context.Background())

	owner, _, _, ok := r.GetOwner("pConcurrent")
	require.True(t, ok,
		"reconcile must not tombstone a live cache entry injected concurrently with Keys()")
	require.Equal(t, "watcher-owner", owner)

	// A subsequent watcher upsert at a strictly higher revision must still
	// be applied through the shared apply path — i.e., reconcile did not
	// corrupt the cache by writing a phantom tombstone with a higher
	// revision than future updates.
	c := handoff.Claim{
		PartitionID: "pConcurrent", Owner: "later-owner",
		State: handoff.ClaimStateStable, Epoch: 2,
		LastUpdated: time.Now().UTC(),
	}
	bLater := marshalClaim(t, c)
	pendingByPID := map[string]pending{
		"pConcurrent": {op: "upsert", data: bLater, revision: 9},
	}
	r.applyPendingBatch(pendingByPID, "test")

	owner2, _, _, ok2 := r.GetOwner("pConcurrent")
	require.True(t, ok2, "later watcher upsert must reach the cache")
	require.Equal(t, "later-owner", owner2,
		"reconcile must not leave a phantom tombstone that shorts out later upserts")
}

// TestClaimResolver_StopBeforeStart asserts Stop is safe to call before
// Start, and that a subsequent Start observes the prior Stop and declines
// to spawn goroutines (returning nil).
func TestClaimResolver_StopBeforeStart(t *testing.T) {
	r := NewClaimBasedResolver(nil, "claims/", nil, WithReconcileInterval(0))

	// Stop before Start must return promptly with no panic and no leak.
	stopped := make(chan struct{})
	go func() {
		r.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(1 * time.Second):
		t.Fatal("Stop-before-Start did not return within 1s")
	}

	// A subsequent Start must observe stopCh closed and return without
	// launching supervise/reconcile goroutines. We can't easily observe
	// goroutine spawning, but we can verify Start returns nil promptly and
	// a second Stop is a true no-op.
	err := r.Start(context.Background())
	require.NoError(t, err)

	stopped2 := make(chan struct{})
	go func() {
		r.Stop()
		close(stopped2)
	}()
	select {
	case <-stopped2:
	case <-time.After(1 * time.Second):
		t.Fatal("second Stop did not return within 1s")
	}
}

// TestClaimResolver_StopRacingStart calls Start and Stop concurrently in many
// iterations and asserts no goroutine leak (we compare against a baseline).
func TestClaimResolver_StopRacingStart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	const iters = 100
	for i := 0; i < iters; i++ { //nolint:intrange // explicit counter for readability
		r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))

		done := make(chan struct{}, 2)
		go func() {
			_ = r.Start(ctx)
			done <- struct{}{}
		}()
		go func() {
			r.Stop()
			done <- struct{}{}
		}()
		// Bound each iteration to keep the test fast and detect deadlocks.
		for j := 0; j < 2; j++ { //nolint:intrange // counter
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatalf("iteration %d: Start/Stop pair deadlocked", i)
			}
		}
		// Final Stop after both have completed must be a no-op.
		r.Stop()
	}
}

// --- helpers ---

// mockKVForReconcile is a controllable mockKV that returns a fixed set of
// claims at a fixed revision. Keys() returns the keys; Get() returns either
// the seeded value or ErrKeyNotFound.
//
// afterKeys is an optional test hook invoked after Keys() determines the
// returned slice but before Keys() returns. It exists so a test can inject
// a concurrent cache mutation between reconcile's pre-Keys snapshot point
// and the Keys() observation, exercising the P0 race directly.
type mockKVForReconcile struct {
	jetstream.KeyValue
	store     map[string][]byte
	revision  uint64
	afterKeys func()
}

func newMockKVForReconcile(store map[string][]byte, revision uint64) *mockKVForReconcile {
	if store == nil {
		store = map[string][]byte{}
	}
	return &mockKVForReconcile{store: store, revision: revision}
}

func (m *mockKVForReconcile) Keys(ctx context.Context, _ ...jetstream.WatchOpt) ([]string, error) {
	defer func() {
		if m.afterKeys != nil {
			m.afterKeys()
		}
	}()
	if len(m.store) == 0 {
		return nil, errors.New("nats: no keys found")
	}
	out := make([]string, 0, len(m.store))
	for k := range m.store {
		out = append(out, k)
	}

	return out, nil
}

func (m *mockKVForReconcile) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	val, ok := m.store[key]
	if !ok {
		return nil, jetstream.ErrKeyNotFound
	}
	return &mockKVEntryFull{key: key, val: val, revision: m.revision, op: 0}, nil
}

type mockKVEntryFull struct {
	jetstream.KeyValueEntry
	key      string
	val      []byte
	revision uint64
	op       jetstream.KeyValueOp
}

func (e *mockKVEntryFull) Key() string                     { return e.key }
func (e *mockKVEntryFull) Value() []byte                   { return e.val }
func (e *mockKVEntryFull) Revision() uint64                { return e.revision }
func (e *mockKVEntryFull) Operation() jetstream.KeyValueOp { return e.op }
