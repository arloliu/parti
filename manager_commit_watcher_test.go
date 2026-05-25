package parti

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestMonitorCommitChanges_ChannelCloseTriggersBackoffAndRestart exercises
// the channel-close → backoff → re-watch loop. We install a watcher KV
// wrapper whose returned watcher has a forceClose() hook; calling it
// closes the Updates() channel mid-stream. monitorCommitChanges must
// observe the close as an error, restart via the backoff loop, and
// deliver a second commit through the re-established watcher.
func TestMonitorCommitChanges_ChannelCloseTriggersBackoffAndRestart(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "watcher-close-restart")

	m, rh, _, _ := newTestManager(t)
	m.assignmentKV = kv

	// Plant a commit so first-time replay always produces a watcher
	// event. Case (d) (worker not in commit) is the simplest shape that
	// successfully drives applyAssignment without payload setup.
	v10 := types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		Workers:        []string{"someone-else"},
		Payloads:       map[string]types.AssignmentPayloadRef{},
	}
	b10, _ := json.Marshal(v10)
	_, err := kv.Create(t.Context(), "assignment._commit", b10)
	require.NoError(t, err)

	wrap := &forceCloseWatcherKV{KeyValue: kv}
	done := make(chan struct{})
	go func() {
		m.monitorCommitChanges(m.ctx, wrap)
		close(done)
	}()

	// Wait until the first commit applies (proves watcher #1 is live).
	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 10
	}, 2*time.Second, 25*time.Millisecond, "first watcher must apply v=10")

	// Force-close watcher #1. The monitor loop must record a KV error
	// and re-Watch (watcher #2) after the backoff.
	wrap.forceCloseLatest()

	// Drop a second commit. Watcher #2's initial replay or subsequent
	// Put event must deliver it. We use a long Eventually because
	// watcherBaseBackoff (2s) + jitter must elapse before the restart.
	v20 := types.AssignmentCommit{
		Version:        20,
		LeaderRevision: 30,
		Workers:        []string{"someone-else"},
		Payloads:       map[string]types.AssignmentPayloadRef{},
	}
	b20, _ := json.Marshal(v20)
	_, err = kv.Put(t.Context(), "assignment._commit", b20)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 20
	}, 8*time.Second, 50*time.Millisecond,
		"restarted watcher must observe the second commit (backoff ~2s)")

	require.GreaterOrEqual(t, wrap.watchCallsLoaded(), int32(2),
		"monitorCommitChanges must re-Watch after channel close")
	require.GreaterOrEqual(t, rh.applyCount.Load(), int64(2),
		"both pre- and post-restart commits must apply")

	m.cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("monitorCommitChanges did not exit after ctx cancel")
	}
}

// forceCloseWatcherKV wraps a jetstream.KeyValue but returns a watcher
// whose Updates() channel can be force-closed by the test. The wrapper
// counts Watch() calls so we can assert the monitor re-watches after a
// channel close.
type forceCloseWatcherKV struct {
	jetstream.KeyValue

	mu         sync.Mutex
	watchCalls atomic.Int32
	latest     *forceCloseWatcher
	upstream   jetstream.KeyWatcher
}

func (f *forceCloseWatcherKV) Watch(ctx context.Context, key string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	up, err := f.KeyValue.Watch(ctx, key, opts...)
	if err != nil {
		return nil, err
	}
	w := newForceCloseWatcher(ctx, up)
	f.mu.Lock()
	f.latest = w
	f.upstream = up
	f.mu.Unlock()
	f.watchCalls.Add(1)

	return w, nil
}

func (f *forceCloseWatcherKV) forceCloseLatest() {
	f.mu.Lock()
	w := f.latest
	f.mu.Unlock()
	if w != nil {
		w.forceClose()
	}
}

func (f *forceCloseWatcherKV) watchCallsLoaded() int32 { return f.watchCalls.Load() }

// forceCloseWatcher proxies an upstream KeyWatcher's Updates() channel
// to a local channel that can be closed via forceClose(). The proxy
// goroutine owns the close of `out` (single writer) so we never
// double-close.
type forceCloseWatcher struct {
	upstream  jetstream.KeyWatcher
	out       chan jetstream.KeyValueEntry
	closeCh   chan struct{}
	closeOnce sync.Once
}

func newForceCloseWatcher(ctx context.Context, up jetstream.KeyWatcher) *forceCloseWatcher {
	w := &forceCloseWatcher{
		upstream: up,
		out:      make(chan jetstream.KeyValueEntry, 8),
		closeCh:  make(chan struct{}),
	}
	go w.proxy(ctx)
	return w
}

func (w *forceCloseWatcher) proxy(ctx context.Context) {
	// Single owner of close(out). Any of ctx/closeCh/upstream-close
	// drains us out of the loop, and we always close out on exit so
	// monitorCommitChanges observes the close as an error.
	defer close(w.out)
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.closeCh:
			return
		case e, ok := <-w.upstream.Updates():
			if !ok {
				return
			}
			select {
			case w.out <- e:
			case <-ctx.Done():
				return
			case <-w.closeCh:
				return
			}
		}
	}
}

func (w *forceCloseWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.out }

func (w *forceCloseWatcher) Stop() error {
	w.closeOnce.Do(func() { close(w.closeCh) })
	return w.upstream.Stop()
}

func (w *forceCloseWatcher) forceClose() {
	w.closeOnce.Do(func() {
		close(w.closeCh)
		// Stop the upstream watcher too so the test does not leak NATS
		// resources between cycles.
		_ = w.upstream.Stop()
	})
}

// TestMonitorCommitChanges_DeleteEventIgnored verifies the commit watcher
// treats a KeyValueDelete as a no-op: no panic, no apply, no LSR mutation.
func TestMonitorCommitChanges_DeleteEventIgnored(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "watcher-delete")

	m, rh, _, _ := newTestManager(t)
	m.assignmentKV = kv

	// Plant a real commit so the watcher sees a Put, then Delete it. The
	// initial Put must apply through case (d) (worker not in commit) so we
	// can verify state stays at v=10 after delete.
	commit := types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		Workers:        []string{"someone-else"},
		Payloads:       map[string]types.AssignmentPayloadRef{},
	}
	bytes, err := json.Marshal(commit)
	require.NoError(t, err)
	_, err = kv.Create(t.Context(), "assignment._commit", bytes)
	require.NoError(t, err)

	// Spin up the watcher in the background. It'll see the initial value,
	// then we delete the key.
	go m.monitorCommitChanges(m.ctx, kv)
	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 10
	}, time.Second, 10*time.Millisecond, "watcher must process initial commit")

	require.NoError(t, kv.Delete(t.Context(), "assignment._commit"))
	// Watch a beat to ensure the delete is dispatched.
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int64(10), m.CurrentAssignment().Version, "delete must NOT regress the snapshot")

	require.GreaterOrEqual(t, rh.applyCount.Load(), int64(1), "initial Put applied once")
}

// TestMonitorCommitChanges_PeriodicReconcile_RecoversDivergence exercises
// the periodic reconcile ticker in watchCommit by running the real watcher
// against a watcher-stub that DROPS update events. The only path to
// convergence is then the reconcile tick — proving the periodic primitive
// (not the manually-invoked handleCommitValue).
//
// We dial watcherReconcileInterval down to 1s via the package-level var
// (testable because manager_assignment.go declares it as var, not const,
// with an explicit "tests may override" contract). 1s gives us enough
// headroom to assert the "no recovery before first tick" invariant.
//
// NOTE: this test does NOT use t.Parallel() because it mutates the package
// global watcherReconcileInterval (v2 review P2: avoid parallel races on a
// test-mutable global).
func TestMonitorCommitChanges_PeriodicReconcile_RecoversDivergence(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "watcher-reconcile")

	prev := watcherReconcileInterval
	watcherReconcileInterval = 1 * time.Second
	t.Cleanup(func() { watcherReconcileInterval = prev })

	m, _, _, _ := newTestManager(t)
	m.assignmentKV = kv

	// Plant initial v=5; route through state machine so snapshot reflects
	// the value the reconcile loop will idempotently re-route.
	v5 := types.AssignmentCommit{
		Version:        5,
		LeaderRevision: 10,
		Workers:        []string{"someone-else"},
		Payloads:       map[string]types.AssignmentPayloadRef{},
	}
	bytes5, _ := json.Marshal(v5)
	_, err := kv.Create(t.Context(), "assignment._commit", bytes5)
	require.NoError(t, err)
	m.handleCommitValue(&v5)
	require.Equal(t, int64(5), m.CurrentAssignment().Version)

	// Wrap the KV with a watcher that DROPS all watcher events. The only
	// path the manager has to observe an update is the reconcile ticker's
	// idempotent KV re-read.
	dropWatcher := &droppingWatcherKV{KeyValue: kv}
	go m.monitorCommitChanges(m.ctx, dropWatcher)

	// Update KV out-of-band to v=15 to mimic a watcher missing the event.
	v15 := types.AssignmentCommit{
		Version:        15,
		LeaderRevision: 30,
		Workers:        []string{"someone-else"},
		Payloads:       map[string]types.AssignmentPayloadRef{},
	}
	bytes15, _ := json.Marshal(v15)
	_, err = kv.Put(t.Context(), "assignment._commit", bytes15)
	require.NoError(t, err)

	// Pre-tick assertion: well before the 1s reconcile tick, the watcher
	// is dropping events so the snapshot MUST still be V=5. This proves
	// the test is actually exercising the reconcile path (and not, say,
	// the watcher-replay path leaking through droppingWatcherKV).
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, int64(5), m.CurrentAssignment().Version,
		"snapshot MUST NOT recover before the first reconcile tick — the dropping watcher is the only event source")

	// Now wait past the 1s tick for the reconcile-driven recovery.
	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 15
	}, 4*time.Second, 25*time.Millisecond,
		"watcher-dropped event must be recovered by the periodic reconcile tick")
}

// droppingWatcherKV wraps a jetstream.KeyValue but replaces the watcher
// with one that NEVER delivers updates. The KV-Get path used by the
// reconcile ticker is unaffected, so the test isolates the reconcile
// path from the watcher.
type droppingWatcherKV struct {
	jetstream.KeyValue
}

func (d *droppingWatcherKV) Watch(_ context.Context, _ string, _ ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return &droppingWatcher{updates: make(chan jetstream.KeyValueEntry)}, nil
}

type droppingWatcher struct {
	updates   chan jetstream.KeyValueEntry
	closeOnce sync.Once
}

func (d *droppingWatcher) Updates() <-chan jetstream.KeyValueEntry { return d.updates }
func (d *droppingWatcher) Stop() error {
	d.closeOnce.Do(func() { close(d.updates) })
	return nil
}

// ============================================================================
// Commit watcher debounce tests
// ============================================================================

// fakeCommitEntryWithVersion returns a fake KV entry whose JSON value encodes an
// AssignmentCommit with the given version. The worker list is set to a dummy peer
// so the entry passes the "valid commit" check without involving this worker's ID.
func fakeCommitEntryWithVersion(v int64) jetstream.KeyValueEntry {
	b, _ := json.Marshal(types.AssignmentCommit{
		Version:        v,
		LeaderRevision: uint64(v),
		Workers:        []string{"someone-else"},
		Payloads:       map[string]types.AssignmentPayloadRef{},
	})
	return fakeVersionEntry{value: b}
}

// fakeCommitEntryFor returns a fake KV entry whose JSON value encodes an
// AssignmentCommit that includes workerID with the given payloadHash.
func fakeCommitEntryFor(version int64, workerID, payloadHash string) jetstream.KeyValueEntry {
	payloads := map[string]types.AssignmentPayloadRef{
		workerID: {PayloadHash: payloadHash},
	}
	workers := []string{workerID}
	b, _ := json.Marshal(types.AssignmentCommit{
		Version:        version,
		LeaderRevision: uint64(version),
		Workers:        workers,
		Payloads:       payloads,
	})

	return fakeVersionEntry{value: b}
}

// TestCommitWatcher_DebouncesMultiVersionBurst delivers V=10..V=14 inside a
// rapid burst with a 100 ms debounce window and asserts that only one apply
// runs (the latest version, V=14).
func TestCommitWatcher_DebouncesMultiVersionBurst(t *testing.T) {
	const window = 100 * time.Millisecond
	m, rh, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
	}()

	for v := int64(10); v <= 14; v++ {
		watcher.ch <- fakeCommitEntryWithVersion(v)
		time.Sleep(8 * time.Millisecond)
	}

	time.Sleep(window + 100*time.Millisecond)

	require.Equal(t, int64(14), m.CurrentAssignment().Version)
	require.Equal(t, int64(1), rh.applyCount.Load(), "debounce must collapse commit burst")
}

// TestCommitWatcher_DebounceResetsOnEachEntry verifies the idle-window
// semantics: a steady drip of entries spaced just below the window must keep
// the timer reset and NOT fire until the stream goes idle.
func TestCommitWatcher_DebounceResetsOnEachEntry(t *testing.T) {
	const window = 100 * time.Millisecond
	m, rh, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	v := int64(1)
	for time.Now().Before(deadline) {
		watcher.ch <- fakeCommitEntryWithVersion(v)
		v++
		time.Sleep(50 * time.Millisecond)
	}

	require.Zero(t, rh.applyCount.Load(), "debounce must not fire while stream is busy")

	time.Sleep(window + 100*time.Millisecond)
	require.Equal(t, int64(1), rh.applyCount.Load(), "debounce must fire once after idle")
}

// TestCommitWatcher_DebounceCancelDoesNotFlush verifies that when m.ctx is
// cancelled (simulating Stop) while a debounced commit is pending, the pending
// commit is dropped and no apply runs.
func TestCommitWatcher_DebounceCancelDoesNotFlush(t *testing.T) {
	const window = 5 * time.Second
	m, rh, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window

	watcher := newFakeKeyWatcher()
	done := make(chan struct{})
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
		close(done)
	}()

	watcher.ch <- fakeCommitEntryWithVersion(99)
	time.Sleep(50 * time.Millisecond)
	m.cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("commit watch session did not exit after ctx cancel")
	}

	require.Zero(t, rh.applyCount.Load(), "pending commit must be dropped on ctx cancel")
}

// TestCommitWatcher_PendingCommitFlushesOnClose verifies that a watcher channel
// close while a commit is pending still processes that pending commit exactly once.
func TestCommitWatcher_PendingCommitFlushesOnClose(t *testing.T) {
	const window = 100 * time.Millisecond
	m, rh, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window

	watcher := newFakeKeyWatcher()
	done := make(chan struct{})
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
		close(done)
	}()

	watcher.ch <- fakeCommitEntryWithVersion(42)
	close(watcher.ch)

	<-done
	require.Equal(t, int64(42), m.CurrentAssignment().Version)
	require.Equal(t, int64(1), rh.applyCount.Load(), "pending commit must flush on watcher close")
}

// TestCommitWatcher_DebounceFlushesOnPartitionSetChange pins the P0 invariant
// that ownership-changing intermediate commits are NOT collapsed away. Two
// commits with different Payloads[workerID].PayloadHash are delivered inside
// the debounce window; the test asserts BOTH are processed (one force-flushed,
// one timer-flushed).
func TestCommitWatcher_DebounceFlushesOnPartitionSetChange(t *testing.T) {
	const window = 100 * time.Millisecond
	m, _, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window
	workerID := m.WorkerID()

	// Route flushed commits through the hook so this test exercises the
	// stage/flush decision tree without depending on a real payload KV.
	var (
		mu      sync.Mutex
		flushed []*types.AssignmentCommit
	)
	m.testHookHandleCommitValue = func(c *types.AssignmentCommit) {
		mu.Lock()
		defer mu.Unlock()
		flushed = append(flushed, c)
	}

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
	}()

	// V=10: this worker owns hash="aaaa"
	watcher.ch <- fakeCommitEntryFor(10, workerID, "aaaa")
	time.Sleep(20 * time.Millisecond)

	// V=11: this worker owns hash="bbbb" — partition-set change.
	// stage() MUST force-flush V=10 before staging V=11 so the
	// handoff diff for the intermediate change is observed.
	watcher.ch <- fakeCommitEntryFor(11, workerID, "bbbb")

	// Wait > window for the timer to flush V=11.
	time.Sleep(window + 100*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, flushed, 2,
		"ownership-changing intermediate must not be collapsed away")
	require.Equal(t, int64(10), flushed[0].Version,
		"V=10 force-flushed before V=11 stages")
	require.Equal(t, int64(11), flushed[1].Version,
		"V=11 flushed by the timer after the idle window")
	require.Equal(t, "bbbb", flushed[1].Payloads[workerID].PayloadHash)
}

// TestCommitWatcher_DebounceIgnoresOutOfOrderLowerVersion verifies that a lower
// commit version arriving after a higher staged version does not replace the
// pending higher version. This pins the >= guard in stage().
func TestCommitWatcher_DebounceIgnoresOutOfOrderLowerVersion(t *testing.T) {
	const window = 100 * time.Millisecond
	m, rh, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
	}()

	watcher.ch <- fakeCommitEntryWithVersion(20)
	time.Sleep(20 * time.Millisecond)
	watcher.ch <- fakeCommitEntryWithVersion(15) // out-of-order, lower

	time.Sleep(window + 100*time.Millisecond)

	require.Equal(t, int64(1), rh.applyCount.Load())
	require.Equal(t, int64(20), m.CurrentAssignment().Version,
		"pending must not be overwritten by a lower-version entry")
}

// TestCommitWatcher_DebounceIgnoresOutOfOrderLowerVersionWithDifferentHash pins
// the round-2 ordering fix: the version-order guard MUST run before the
// workerAssignmentChanged flush-on-change check. A stale lower-version commit
// with a different worker hash must NOT flush the newer pending or re-stage
// itself.
func TestCommitWatcher_DebounceIgnoresOutOfOrderLowerVersionWithDifferentHash(t *testing.T) {
	const window = 100 * time.Millisecond
	m, _, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window
	workerID := m.WorkerID()

	// Use the hook so we can directly observe whether V=15 ever reaches
	// the post-debounce dispatch surface. Under the round-2 ordering bug,
	// V=15 would force-flush V=20 (dispatch sees V=20), then V=15 would
	// be the new pending and timer-flush (dispatch sees V=15). That
	// would produce dispatches = [V=20, V=15]. The correct ordering must
	// produce dispatches = [V=20] — V=15 is dropped at the version guard
	// before workerAssignmentChanged runs, so the new pending is never
	// re-staged. Asserting applyCount alone does NOT discriminate: under
	// the buggy path, handleCommitValueOnce would case-(a) no-op V=15
	// (V=15 < cur.Version=V=20) but would still write V=15 to
	// lastObservedCommit, mutating selector-visible state.
	var (
		mu         sync.Mutex
		dispatches []*types.AssignmentCommit
	)
	m.testHookHandleCommitValue = func(c *types.AssignmentCommit) {
		mu.Lock()
		defer mu.Unlock()
		dispatches = append(dispatches, c)
	}

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
	}()

	watcher.ch <- fakeCommitEntryFor(20, workerID, "aaaa")
	time.Sleep(20 * time.Millisecond)
	watcher.ch <- fakeCommitEntryFor(15, workerID, "bbbb")

	time.Sleep(window + 100*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, dispatches, 1,
		"lower-version must be dropped before reaching dispatch — "+
			"never as a force-flush of pending nor as a re-stage")
	require.Equal(t, int64(20), dispatches[0].Version,
		"only V=20 was authoritative; V=15 must never be observed")
	require.Equal(t, "aaaa", dispatches[0].Payloads[workerID].PayloadHash)
}

// TestWorkerAssignmentChanged is a focused unit test pinning the contract of
// the workerAssignmentChanged predicate across schema-edge cases.
func TestWorkerAssignmentChanged(t *testing.T) {
	mk := func(ver int64, members []string, hash string) *types.AssignmentCommit {
		c := &types.AssignmentCommit{Version: ver, Workers: members}
		c.Payloads = map[string]types.AssignmentPayloadRef{}
		for _, w := range members {
			c.Payloads[w] = types.AssignmentPayloadRef{PayloadHash: hash}
		}
		return c
	}
	cases := []struct {
		name string
		prev *types.AssignmentCommit
		next *types.AssignmentCommit
		want bool
	}{
		{"both_absent", mk(1, []string{"other"}, "x"), mk(2, []string{"other"}, "x"), false},
		{"present_to_absent", mk(1, []string{"me", "other"}, "x"), mk(2, []string{"other"}, "x"), true},
		{"absent_to_present", mk(1, []string{"other"}, "x"), mk(2, []string{"me", "other"}, "x"), true},
		{"present_same_hash", mk(1, []string{"me"}, "x"), mk(2, []string{"me"}, "x"), false},
		{"present_different_hash", mk(1, []string{"me"}, "x"), mk(2, []string{"me"}, "y"), true},
		{"nil_prev_present", nil, mk(2, []string{"me"}, "x"), true},
		{"nil_prev_absent", nil, mk(2, []string{"other"}, "x"), false},
		{"empty_payloads_present_in_workers", mk(1, []string{"me"}, "x"), &types.AssignmentCommit{Version: 2, Workers: []string{"me"}, Payloads: map[string]types.AssignmentPayloadRef{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, workerAssignmentChanged(tc.prev, tc.next, "me"))
		})
	}
}
