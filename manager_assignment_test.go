package parti

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment"
	"github.com/arloliu/parti/v2/internal/heartbeat"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// stubCalculator satisfies assignmentCalculator but is NOT assignment.NopCalculator.
type stubCalculator struct{}

func (s *stubCalculator) Start(context.Context) error { return nil }
func (s *stubCalculator) Stop(context.Context) error  { return nil }
func (s *stubCalculator) SubscribeToStateChanges() (<-chan types.CalculatorState, func()) {
	return nil, func() {}
}
func (s *stubCalculator) TriggerRebalance(context.Context) error { return nil }
func (s *stubCalculator) GetState() types.CalculatorState        { return types.CalcStateIdle }

func TestManager_calculateAndPublish(t *testing.T) {
	t.Run("returns error for NopCalculator", func(t *testing.T) {
		m := &Manager{calculator: assignment.NewNopCalculator()}
		err := m.calculateAndPublish(t.Context())
		require.Error(t, err)
		require.Contains(t, err.Error(), "calculator not started")
	})

	t.Run("respects cancelled context", func(t *testing.T) {
		m := &Manager{calculator: &stubCalculator{}}

		ctx, cancel := context.WithCancel(t.Context())
		cancel() // Cancel immediately

		start := time.Now()
		err := m.calculateAndPublish(ctx)
		elapsed := time.Since(start)

		require.ErrorIs(t, err, context.Canceled)
		require.Less(t, elapsed, 100*time.Millisecond,
			"must return immediately on cancelled context, not sleep 500ms")
	})

	t.Run("respects context deadline", func(t *testing.T) {
		m := &Manager{calculator: &stubCalculator{}}

		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()

		start := time.Now()
		err := m.calculateAndPublish(ctx)
		elapsed := time.Since(start)

		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Less(t, elapsed, 200*time.Millisecond,
			"must return at context deadline, not sleep full 500ms")
	})

	t.Run("completes normally with active context", func(t *testing.T) {
		m := &Manager{calculator: &stubCalculator{}}

		start := time.Now()
		err := m.calculateAndPublish(t.Context())
		elapsed := time.Since(start)

		require.NoError(t, err)
		require.GreaterOrEqual(t, elapsed, 450*time.Millisecond,
			"should wait ~500ms when context is not cancelled")
	})
}

// fakeKeyWatcher is a minimal in-process jetstream.KeyWatcher for the
// debounce tests. Stop() returns nil and does NOT close the channel —
// the deferred watcher.Stop() in runAssignmentWatchSession runs at
// function exit. Tests that close the channel explicitly (e.g.
// TestAssignmentWatcher_PendingEntryFlushesOnClose) do so themselves;
// not closing in Stop avoids double-close.
type fakeKeyWatcher struct {
	ch chan jetstream.KeyValueEntry
}

func newFakeKeyWatcher() *fakeKeyWatcher {
	return &fakeKeyWatcher{ch: make(chan jetstream.KeyValueEntry, 32)}
}

func (f *fakeKeyWatcher) Updates() <-chan jetstream.KeyValueEntry { return f.ch }
func (f *fakeKeyWatcher) Stop() error                             { return nil }

var _ jetstream.KeyWatcher = (*fakeKeyWatcher)(nil)

// fakeVersionEntry is a minimal jetstream.KeyValueEntry whose Value()
// returns the JSON encoding of Assignment{Version: v}. Implements all
// methods required by the interface.
type fakeVersionEntry struct {
	value []byte
}

func fakeEntryWithVersion(v int64) jetstream.KeyValueEntry {
	b, _ := json.Marshal(Assignment{Version: v})
	return fakeVersionEntry{value: b}
}

func (e fakeVersionEntry) Bucket() string                  { return "test-bucket" }
func (e fakeVersionEntry) Key() string                     { return "test-key" }
func (e fakeVersionEntry) Value() []byte                   { return e.value }
func (e fakeVersionEntry) Revision() uint64                { return 1 }
func (e fakeVersionEntry) Created() time.Time              { return time.Time{} }
func (e fakeVersionEntry) Delta() uint64                   { return 0 }
func (e fakeVersionEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }

var _ jetstream.KeyValueEntry = fakeVersionEntry{}

// decodeVersion extracts the Version field from a KeyValueEntry's JSON value.
// Uses direct json.Unmarshal to avoid requiring a *Manager receiver.
func decodeVersion(entry jetstream.KeyValueEntry) int64 {
	var a Assignment
	if err := json.Unmarshal(entry.Value(), &a); err != nil {
		return -1
	}
	return a.Version
}

// TestAssignmentWatcher_DebouncesMultiVersionBurst delivers V=10..V=14
// inside 50 ms with a 100 ms debounce window and asserts
// handleAssignmentEntry runs exactly once, with V=14.
func TestAssignmentWatcher_DebouncesMultiVersionBurst(t *testing.T) {
	const window = 100 * time.Millisecond
	m := newTestManagerWithMetrics(t, newRecordingMetrics())
	m.cfg.AssignmentWatcherDebounce = window

	var processed atomic.Int64
	var lastVersion atomic.Int64
	m.testHookHandleAssignment = func(workerID string, e jetstream.KeyValueEntry) {
		processed.Add(1)
		lastVersion.Store(decodeVersion(e))
	}

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runAssignmentWatchSession(m.ctx, nil /* kv */, watcher, nil /* no reconcile */, "worker-1", "key")
	}()

	for v := int64(10); v <= 14; v++ {
		watcher.ch <- fakeEntryWithVersion(v)
		time.Sleep(8 * time.Millisecond)
	}

	// Burst delivered. Wait > debounce + scheduling slack.
	time.Sleep(window + 100*time.Millisecond)

	require.Equal(t, int64(1), processed.Load(), "debounce must collapse burst")
	require.Equal(t, int64(14), lastVersion.Load(), "must process the latest version")
}

// TestAssignmentWatcher_DebounceResetsOnEachEntry verifies the idle-window
// semantics: a steady drip of entries spaced just below the window must
// keep the timer reset and NOT fire until the stream goes idle.
func TestAssignmentWatcher_DebounceResetsOnEachEntry(t *testing.T) {
	const window = 100 * time.Millisecond
	m := newTestManagerWithMetrics(t, newRecordingMetrics())
	m.cfg.AssignmentWatcherDebounce = window

	var processed atomic.Int64
	m.testHookHandleAssignment = func(workerID string, e jetstream.KeyValueEntry) {
		processed.Add(1)
	}

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runAssignmentWatchSession(m.ctx, nil, watcher, nil, "worker-1", "key")
	}()

	// Drip 10 entries spaced 50 ms apart (well below the 100 ms window).
	// Timer must reset each time; no fire during the drip.
	deadline := time.Now().Add(500 * time.Millisecond)
	v := int64(1)
	for time.Now().Before(deadline) {
		watcher.ch <- fakeEntryWithVersion(v)
		v++
		time.Sleep(50 * time.Millisecond)
	}

	require.Zero(t, processed.Load(), "debounce must NOT fire while stream is busy")

	// Now go idle. Wait > window.
	time.Sleep(window + 100*time.Millisecond)
	require.Equal(t, int64(1), processed.Load(), "debounce must fire exactly once after idle")
}

// TestAssignmentWatcher_DebounceCancelDoesNotFlush verifies that when
// m.ctx is cancelled (e.g. via Stop) while a debounced entry is pending,
// the entry is dropped and no apply runs — Stop must not race
// background apply work into the wait group.
func TestAssignmentWatcher_DebounceCancelDoesNotFlush(t *testing.T) {
	const window = 5 * time.Second
	m := newTestManagerWithMetrics(t, newRecordingMetrics())
	m.cfg.AssignmentWatcherDebounce = window

	var processed atomic.Int64
	m.testHookHandleAssignment = func(workerID string, e jetstream.KeyValueEntry) {
		processed.Add(1)
	}

	watcher := newFakeKeyWatcher()
	sessionDone := make(chan struct{})
	go func() {
		_ = m.runAssignmentWatchSession(m.ctx, nil, watcher, nil, "worker-1", "key")
		close(sessionDone)
	}()

	// Deliver an entry — debounce timer starts (5s window).
	watcher.ch <- fakeEntryWithVersion(99)
	time.Sleep(50 * time.Millisecond) // let the debounce arm receive it

	// Now cancel the manager context, simulating Stop's first action.
	// (Fixture is not Stop-safe — cancel directly.)
	m.cancel()

	// Wait for session to exit.
	select {
	case <-sessionDone:
	case <-time.After(2 * time.Second):
		t.Fatal("session did not exit after ctx cancel")
	}

	// Hook MUST NOT have been called.
	require.Zero(t, processed.Load(), "pending entry must be dropped on ctx cancel, not applied during Stop")
}

// TestAssignmentWatcher_PendingEntryFlushesOnClose verifies that a watcher
// channel close while an entry is pending still processes that pending
// entry exactly once.
func TestAssignmentWatcher_PendingEntryFlushesOnClose(t *testing.T) {
	const window = 100 * time.Millisecond
	m := newTestManagerWithMetrics(t, newRecordingMetrics())
	m.cfg.AssignmentWatcherDebounce = window

	var processed atomic.Int64
	var lastVersion atomic.Int64
	m.testHookHandleAssignment = func(workerID string, e jetstream.KeyValueEntry) {
		processed.Add(1)
		lastVersion.Store(decodeVersion(e))
	}

	watcher := newFakeKeyWatcher()
	done := make(chan struct{})
	go func() {
		_ = m.runAssignmentWatchSession(m.ctx, nil, watcher, nil, "worker-1", "key")
		close(done)
	}()

	// Deliver a single entry and immediately close the channel before
	// the debounce window can fire.
	watcher.ch <- fakeEntryWithVersion(42)
	close(watcher.ch)

	<-done
	require.Equal(t, int64(1), processed.Load(), "pending entry must flush on channel close")
	require.Equal(t, int64(42), lastVersion.Load())
}

// TestMonitorAssignmentChanges_ChannelCloseTriggersBackoffAndRestart exercises
// PR-1's W12 fix: the legacy per-worker alias watcher (`assignment.<W>`)
// must NOT exit permanently when its Updates() channel closes. Instead,
// channel close becomes a retryable error so monitorAssignmentChanges loops
// through the same exponential-backoff + re-Watch path as monitorCommitChanges.
//
// Mirrors TestMonitorCommitChanges_ChannelCloseTriggersBackoffAndRestart in
// shape (forceCloseWatcherKV + post-restart Put).
func TestMonitorAssignmentChanges_ChannelCloseTriggersBackoffAndRestart(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "alias-close-restart")

	m, rh, _, _ := newTestManager(t)
	m.assignmentKV = kv

	key := fmt.Sprintf("assignment.%s", m.WorkerID())

	// Plant initial alias V=4/LR=15. With no commit observed, the dual-read
	// selector picks the alias and applyAssignment runs.
	v4 := Assignment{
		Version:        4,
		LeaderRevision: 15,
	}
	b4, err := json.Marshal(v4)
	require.NoError(t, err)
	_, err = kv.Create(t.Context(), key, b4)
	require.NoError(t, err)

	wrap := &forceCloseWatcherKV{KeyValue: kv}
	done := make(chan struct{})
	go func() {
		m.monitorAssignmentChanges(m.ctx, wrap)
		close(done)
	}()

	// Watcher #1 must observe the initial replay and apply the alias.
	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 4
	}, 2*time.Second, 25*time.Millisecond, "first watcher must apply V=4")

	// Force-close watcher #1. The monitor must record a KV error and
	// re-Watch (watcher #2) after the backoff window.
	wrap.forceCloseLatest()

	// Drop a second alias at higher Version+LR. Watcher #2's initial
	// replay (or a subsequent Put delivery) must apply it. Long
	// Eventually because watcherBaseBackoff (2s) + jitter precedes the
	// restart.
	v5 := Assignment{
		Version:        5,
		LeaderRevision: 16,
	}
	b5, err := json.Marshal(v5)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, b5)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 5
	}, 8*time.Second, 50*time.Millisecond,
		"restarted alias watcher must observe the second alias (backoff ~2s)")

	require.GreaterOrEqual(t, wrap.watchCallsLoaded(), int32(2),
		"monitorAssignmentChanges must re-Watch after channel close")
	require.GreaterOrEqual(t, rh.applyCount.Load(), int64(2),
		"both pre- and post-restart aliases must apply")

	m.cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("monitorAssignmentChanges did not exit after ctx cancel")
	}
}

// silentWatcherKV wraps a jetstream.KeyValue but always returns a
// silent-watcher whose Updates() channel is OPEN and NEVER delivers any
// event. KV.Get is delegated to the upstream KV intact, so the reconcile
// arm of watchAssignment can still re-read the key. This is the
// canonical "watcher stalled but not closed" double for testing the
// reconcile path in isolation from the watcher-replay path.
//
// Mirrors droppingWatcherKV (used by the commit-watcher reconcile test)
// but kept as a distinct type because of its different semantic: Stop()
// here returns nil WITHOUT closing the channel, so the production
// `select { case <-Updates() }` never fires and watchAssignment cannot
// observe a "channel closed" signal.
type silentWatcherKV struct {
	jetstream.KeyValue
	mu      sync.Mutex
	watcher *silentWatcher
}

func (s *silentWatcherKV) Watch(_ context.Context, _ string, _ ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	w := &silentWatcher{updates: make(chan jetstream.KeyValueEntry)}
	s.mu.Lock()
	s.watcher = w
	s.mu.Unlock()
	return w, nil
}

type silentWatcher struct {
	updates   chan jetstream.KeyValueEntry
	closeOnce sync.Once
}

func (w *silentWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }

// Stop closes the channel — but only when the production code Stops the
// watcher (i.e., on context cancel / function exit). The test never
// invokes Stop from outside; cancel propagation lets watchAssignment's
// own deferred Stop close the channel cleanly.
func (w *silentWatcher) Stop() error {
	w.closeOnce.Do(func() { close(w.updates) })
	return nil
}

// TestMonitorAssignmentChanges_PeriodicReconcile_RecoversSilentStall
// (PR-1 Test 5.2) exercises the reconcile arm of watchAssignment by
// running the real watcher against a stub that NEVER delivers updates
// over its Updates() channel (modeling a NATS server stall that doesn't
// close the channel). The only convergence path is then the reconcile
// ticker's idempotent KV re-read, proving the reconcile arm is
// load-bearing for silent-stall recovery.
//
// Mirrors TestMonitorCommitChanges_PeriodicReconcile_RecoversDivergence
// in shape: 1s reconcile interval, pre-tick assertion that recovery has
// NOT happened, then post-tick assertion that it has.
//
// NOTE: does NOT use t.Parallel() because it mutates the package global
// watcherReconcileInterval (matching the commit-watcher reconcile test's
// rationale).
func TestMonitorAssignmentChanges_PeriodicReconcile_RecoversSilentStall(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "alias-reconcile")

	prev := watcherReconcileInterval
	watcherReconcileInterval = 1 * time.Second
	t.Cleanup(func() { watcherReconcileInterval = prev })

	m, rh, _, _ := newTestManager(t)
	m.assignmentKV = kv

	key := fmt.Sprintf("assignment.%s", m.WorkerID())

	// Wrap the KV with a silent-watcher: the only path to convergence
	// is the reconcile ticker's KV.Get.
	silent := &silentWatcherKV{KeyValue: kv}
	go m.monitorAssignmentChanges(m.ctx, silent)

	// Put alias A out-of-band. Watcher never sees it.
	vA := Assignment{Version: 7, LeaderRevision: 21}
	bA, err := json.Marshal(vA)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, bA)
	require.NoError(t, err)

	// Pre-tick: well before 1s, no convergence is possible (the watcher
	// is silent). This rules out the watcher accidentally leaking
	// through the stub.
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, int64(0), rh.applyCount.Load(),
		"no apply must happen before the first reconcile tick")
	require.Equal(t, int64(0), m.CurrentAssignment().Version,
		"snapshot MUST NOT advance before the first reconcile tick")

	// Wait past the 1s reconcile tick: alias A must apply via the
	// reconcile path.
	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 7
	}, 4*time.Second, 25*time.Millisecond,
		"alias A must apply via the periodic reconcile tick")
	require.GreaterOrEqual(t, rh.applyCount.Load(), int64(1),
		"reconcile tick must trigger apply for alias A")

	// Put alias B at a higher Version/LR. Same recovery path: only
	// observable via reconcile.
	vB := Assignment{Version: 8, LeaderRevision: 22}
	bB, err := json.Marshal(vB)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, bB)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 8
	}, 4*time.Second, 25*time.Millisecond,
		"alias B must apply via the periodic reconcile tick")
	require.GreaterOrEqual(t, rh.applyCount.Load(), int64(2),
		"second reconcile tick must trigger second apply")
}

// TestMonitorAssignmentChanges_ReconcileDeleteIsNoOp (PR-1 Test 5.4)
// verifies that when the alias key is deleted out-of-band and the
// reconcile tick observes ErrKeyNotFound, no state change happens. The
// last-applied snapshot is preserved and no spurious apply / handoff
// fires.
//
// Same global mutation as Test 5.2 — no t.Parallel().
func TestMonitorAssignmentChanges_ReconcileDeleteIsNoOp(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "alias-reconcile-delete")

	prev := watcherReconcileInterval
	watcherReconcileInterval = 500 * time.Millisecond
	t.Cleanup(func() { watcherReconcileInterval = prev })

	m, rh, _, _ := newTestManager(t)
	m.assignmentKV = kv

	key := fmt.Sprintf("assignment.%s", m.WorkerID())

	// Plant alias A first so an apply has happened before the delete.
	vA := Assignment{Version: 4, LeaderRevision: 12}
	bA, err := json.Marshal(vA)
	require.NoError(t, err)
	_, err = kv.Create(t.Context(), key, bA)
	require.NoError(t, err)

	go m.monitorAssignmentChanges(m.ctx, kv)

	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 4
	}, 2*time.Second, 25*time.Millisecond, "initial alias must apply")

	applyAfterInitial := rh.applyCount.Load()

	// Delete the alias. The next reconcile tick must observe
	// ErrKeyNotFound and silently continue: snapshot and apply count
	// must remain unchanged.
	require.NoError(t, kv.Delete(t.Context(), key))

	// Wait through 3+ reconcile ticks to confirm no spurious apply.
	time.Sleep(2 * time.Second)

	cur := m.CurrentAssignment()
	require.Equal(t, int64(4), cur.Version,
		"delete must NOT regress the in-memory snapshot")
	require.Equal(t, uint64(12), cur.LeaderRevision,
		"delete must NOT regress LeaderRevision")
	require.Equal(t, applyAfterInitial, rh.applyCount.Load(),
		"reconcile tick on deleted key MUST NOT trigger an apply")
}

// TestMonitorAssignmentChanges_GracefulShutdownWithActiveTicker
// (PR-1 Test 5.5) verifies that the reconcile ticker stops cleanly
// when the manager's context is cancelled, even under a stalled
// watcher (the silent-stall scenario). Guards against goroutine
// leaks on shutdown.
//
// Same global mutation as Test 5.2 — no t.Parallel().
func TestMonitorAssignmentChanges_GracefulShutdownWithActiveTicker(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "alias-shutdown")

	prev := watcherReconcileInterval
	watcherReconcileInterval = 10 * time.Millisecond
	t.Cleanup(func() { watcherReconcileInterval = prev })

	m, _, _, _ := newTestManager(t)
	m.assignmentKV = kv

	silent := &silentWatcherKV{KeyValue: kv}
	done := make(chan struct{})
	go func() {
		m.monitorAssignmentChanges(m.ctx, silent)
		close(done)
	}()

	// Let the reconcile ticker fire several times to confirm the
	// goroutine is actively in the select loop.
	time.Sleep(80 * time.Millisecond)

	m.cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("monitorAssignmentChanges did not exit within 500ms of ctx cancel")
	}
}

// ============================================================================
// P6: ID-based diff in recordAssignmentMetrics
// ============================================================================

// captureMetrics wraps NopMetrics and records RecordAssignmentChange calls.
type captureMetrics struct {
	*metrics.NopMetrics
	mu      sync.Mutex
	added   int
	removed int
	version int64
}

func (c *captureMetrics) RecordAssignmentChange(added, removed int, version int64) {
	c.mu.Lock()
	c.added = added
	c.removed = removed
	c.version = version
	c.mu.Unlock()
}

// TestRecordAssignmentMetrics table-drives the partition-diff accounting in
// recordAssignmentMetrics. Each row stages an old/new partition-ID set and
// asserts the added/removed counts the ID-based diff must report. The
// IDBasedDiff row additionally asserts the version field is forwarded.
func TestRecordAssignmentMetrics(t *testing.T) {
	t.Parallel()

	partitionsFromKeys := func(keys []string) []Partition {
		parts := make([]Partition, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, Partition{Keys: []string{k}})
		}

		return parts
	}

	cases := []struct {
		name string
		// oldKeys/newKeys are single-key partition IDs.
		oldKeys     []string
		newKeys     []string
		wantAdded   int
		wantRemoved int
		// checkVersion asserts cm.version == 2 (only IDBasedDiff does this);
		// for the others the version field is not part of the original
		// contract, so it is left unchecked.
		checkVersion bool
	}{
		{
			// Old: [A, B], New: [B, C] — same length but C replaces A.
			// Length-based diff would produce added=0, removed=0.
			// ID-based diff must produce added=1 (C), removed=1 (A).
			name:         "IDBasedDiff",
			oldKeys:      []string{"A", "B"},
			newKeys:      []string{"B", "C"},
			wantAdded:    1,
			wantRemoved:  1,
			checkVersion: true,
		},
		{
			// Pure growth: adding partitions without removing any reports
			// added>0, removed=0.
			name:        "PureGrowth",
			oldKeys:     []string{"A"},
			newKeys:     []string{"A", "B"},
			wantAdded:   1,
			wantRemoved: 0,
		},
		{
			// Pure shrink: removing partitions without adding any reports
			// added=0, removed>0.
			name:        "PureShrink",
			oldKeys:     []string{"A", "B"},
			newKeys:     []string{"A"},
			wantAdded:   0,
			wantRemoved: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cm := &captureMetrics{NopMetrics: metrics.NewNop()}
			m := &Manager{metrics: cm, logger: logging.NewNop()}

			oldAsgn := Assignment{Version: 1, Partitions: partitionsFromKeys(tc.oldKeys)}
			newAsgn := Assignment{Version: 2, Partitions: partitionsFromKeys(tc.newKeys)}

			m.recordAssignmentMetrics(oldAsgn, newAsgn)

			cm.mu.Lock()
			defer cm.mu.Unlock()
			require.Equal(t, tc.wantAdded, cm.added)
			require.Equal(t, tc.wantRemoved, cm.removed)
			if tc.checkVersion {
				require.Equal(t, int64(2), cm.version)
			}
		})
	}
}

// ============================================================================
// P5: Assignment watcher retry on transient failure
// ============================================================================

// mockKeyWatcher is a minimal jetstream.KeyWatcher implementation that signals
// the caller via a closed channel and does nothing else.
type mockKeyWatcher struct {
	updatesCh chan jetstream.KeyValueEntry
	errCh     chan error
}

func newMockKeyWatcher() *mockKeyWatcher {
	ch := make(chan jetstream.KeyValueEntry)
	close(ch) // Immediately signal Updates() closed → watchAssignment returns err.
	return &mockKeyWatcher{
		updatesCh: ch,
		errCh:     make(chan error),
	}
}

func (w *mockKeyWatcher) Context() context.Context                { return context.Background() }
func (w *mockKeyWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updatesCh }
func (w *mockKeyWatcher) Stop() error                             { return nil }
func (w *mockKeyWatcher) Error() <-chan error                     { return w.errCh }

// mockRetryKV fails Watch on the first call and succeeds (with a watcher
// whose Updates() channel is immediately closed) on subsequent calls.
type mockRetryKV struct {
	jetstream.KeyValue
	watchCalls atomic.Int32
}

func (m *mockRetryKV) Watch(_ context.Context, _ string, _ ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	call := m.watchCalls.Add(1)
	if call == 1 {
		return nil, errors.New("transient NATS error")
	}
	return newMockKeyWatcher(), nil
}

// TestMonitorAssignmentChanges_RewatchesOnChannelClose verifies that
// monitorAssignmentChanges treats both an initial Watch error and a closed
// Updates() channel as recoverable conditions: each triggers a re-Watch via
// the backoff loop instead of exiting permanently. The monitor only stops
// when its context is cancelled.
//
// W12 / PR-1 changed `watchAssignment` to return an error on channel close
// (previously it returned nil → clean exit). Before that change, a closed
// channel silently terminated the monitor and any subsequent alias updates
// (including rolling-upgrade alias.<W>) were missed until process restart.
func TestMonitorAssignmentChanges_RewatchesOnChannelClose(t *testing.T) {
	t.Parallel()

	// Generous timeout: 2s base backoff + jitter for the first rewatch,
	// 4s + jitter for the second.
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	kv := &mockRetryKV{}
	m := &Manager{
		logger: logging.NewNop(),
	}
	m.workerID.Store("worker-0")

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.monitorAssignmentChanges(ctx, kv)
	}()

	// Three Watch calls = original + two rewatches: proves the rewatch
	// path triggers on both the initial Watch error AND on each
	// subsequent channel-close.
	require.Eventually(t, func() bool {
		return kv.watchCalls.Load() >= 3
	}, 12*time.Second, 50*time.Millisecond,
		"monitorAssignmentChanges must rewatch after Watch error AND after channel close")

	// Cancel context: goroutine must exit cleanly.
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("monitorAssignmentChanges did not exit after ctx cancel")
	}
}

// ============================================================================
// P7: monitorCalculatorState channel-based sync (no time.Sleep)
// ============================================================================

// TestStartCalculator_NoRaceBetweenSubscribeAndStart verifies that the ready channel
// mechanism ensures monitorCalculatorState has subscribed before calc.Start is called.
// It uses the race detector (go test -race) to catch races that the old time.Sleep(10ms)
// approach could not reliably prevent.
//
// The test checks the observable guarantee: the subscription is established before
// Start returns, so the initial state push from SubscribeToStateChanges is never missed.
func TestMonitorCalculatorState_ReadyChannelEstablishesSubscriptionFirst(t *testing.T) {
	t.Parallel()

	// Build a minimal calculator mock whose SubscribeToStateChanges records when
	// Subscribe was called relative to when the ready channel is closed.
	var subscribedAt atomic.Int64  // UnixNano when Subscribe was called
	var readyClosedAt atomic.Int64 // UnixNano when readyCh was closed (set in goroutine)

	stateCh := make(chan types.CalculatorState, 4)
	close(stateCh) // signal "nothing to send"; goroutine will exit immediately

	calcMock := &monitorTestCalculator{
		onSubscribe: func() {
			subscribedAt.Store(time.Now().UnixNano())
		},
		stateCh: stateCh,
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancelled immediately so the monitor loop exits after subscribing

	m := &Manager{
		logger: logging.NewNop(),
		ctx:    ctx,
	}

	readyCh := make(chan struct{})

	go func() {
		m.monitorCalculatorState(calcMock, readyCh)
	}()

	// Capture when readyCh closes (i.e., when subscription is established).
	<-readyCh
	readyClosedAt.Store(time.Now().UnixNano())

	sub := subscribedAt.Load()
	ready := readyClosedAt.Load()

	require.NotZero(t, sub, "Subscribe must have been called")
	require.LessOrEqual(t, sub, ready,
		"Subscribe must be called before or at the same time as readyCh close")
}

// monitorTestCalculator satisfies the assignmentCalculator interface for testing.
type monitorTestCalculator struct {
	onSubscribe func()
	stateCh     chan types.CalculatorState
	state       atomic.Int64 // types.CalculatorState; controllable by tests
}

func (c *monitorTestCalculator) Start(context.Context) error { return nil }
func (c *monitorTestCalculator) Stop(context.Context) error  { return nil }
func (c *monitorTestCalculator) SubscribeToStateChanges() (<-chan types.CalculatorState, func()) {
	if c.onSubscribe != nil {
		c.onSubscribe()
	}
	return c.stateCh, func() {}
}
func (c *monitorTestCalculator) TriggerRebalance(context.Context) error { return nil }
func (c *monitorTestCalculator) GetState() types.CalculatorState {
	return types.CalculatorState(c.state.Load())
}
func (c *monitorTestCalculator) setState(s types.CalculatorState) {
	c.state.Store(int64(s))
}

// TestMonitorCalculatorState_ReconcileRecoversDroppedState verifies the
// Cause-A fix from PR-3 §5.1: a calculator state change that is dropped by
// the subscriber buffer AND remains current at the next reconcile tick is
// projected to the manager within 2 * calculatorStateReconcileInterval.
//
// The reconcile invariant is "eventual projection of the current calculator
// state, NOT replay of completed transitions" — this test asserts the former
// only.
func TestMonitorCalculatorState_ReconcileRecoversDroppedState(t *testing.T) {
	// NOTE: NOT t.Parallel() — this test mutates the package-level
	// calculatorStateReconcileInterval var, which is read concurrently by
	// every other test that starts monitorCalculatorState (e.g.
	// TestMonitorCalculatorState_ReadyChannelEstablishesSubscriptionFirst).
	// Running in parallel triggers a -race finding on that read/write.
	// Shorten the reconcile interval so the test runs deterministically and
	// quickly. Reset on cleanup so other tests are unaffected.
	prev := calculatorStateReconcileInterval
	calculatorStateReconcileInterval = 50 * time.Millisecond
	t.Cleanup(func() { calculatorStateReconcileInterval = prev })

	var (
		mu       sync.Mutex
		recorded [][2]types.State
		recordCh = make(chan struct{}, 16)
		hooksCfg types.Hooks
	)
	hooksCfg = types.Hooks{
		OnStateChanged: func(_ context.Context, from, to types.State) error {
			mu.Lock()
			recorded = append(recorded, [2]types.State{from, to})
			mu.Unlock()
			select {
			case recordCh <- struct{}{}:
			default:
			}

			return nil
		},
	}

	m := &Manager{
		cfg:       Config{DegradedAlert: DegradedAlertConfig{AlertInterval: time.Minute}},
		hooks:     &hooksCfg,
		metrics:   metrics.NewNop(),
		logger:    logging.NewNop(),
		heartbeat: heartbeat.NewNop(),
	}
	m.state.Store(int32(StateStable))
	m.startupAssignmentApplied.Store(true)
	m.workerID.Store("worker-test")
	m.assignment.Store(Assignment{})
	m.ctx, m.cancel = context.WithCancel(context.Background())
	t.Cleanup(m.cancel)

	// stateCh stays open but the test never sends on it — simulates a
	// dropped event burst. The reconcile arm must surface the current state
	// via GetState.
	stateCh := make(chan types.CalculatorState, 4)
	calc := &monitorTestCalculator{stateCh: stateCh}
	calc.setState(types.CalcStateIdle)

	readyCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.monitorCalculatorState(calc, readyCh)
	}()
	<-readyCh

	// Drive calculator into Scaling without sending on stateCh: every event
	// is treated as "dropped". Reconcile must observe Scaling within
	// 2 * calculatorStateReconcileInterval.
	calc.setState(types.CalcStateScaling)

	require.Eventually(t, func() bool {
		return m.State() == StateScaling
	}, 5*calculatorStateReconcileInterval, 10*time.Millisecond,
		"reconcile must project current calculator state after a subscriber drop")

	require.Equal(t, StateScaling, m.State())

	// Stop the goroutine cleanly.
	m.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorCalculatorState did not exit on ctx cancel")
	}
	m.wg.Wait()

	// OnStateChanged recorder must contain exactly one (Stable, Scaling)
	// tuple from the reconcile-driven projection. No replay of any
	// intermediate state — none existed for the dropped burst.
	mu.Lock()
	defer mu.Unlock()
	var got [][2]types.State
	for _, p := range recorded {
		if p[0] == StateStable && p[1] == StateScaling {
			got = append(got, p)
		}
	}
	require.Len(t, got, 1, "exactly one (Stable, Scaling) transition expected; got recorder=%v", recorded)
}

// TestPartitionLifecycle_DrivesManagerStateRebalancing verifies PR-3 §5.2 at
// the Manager surface: when the calculator transitions Idle → Rebalancing → Idle
// (the canonical partition-lifecycle path under the FSM), the Manager's public
// State() reaches StateRebalancing and the OnStateChanged hook fires both
// (Stable, Rebalancing) and (Rebalancing, Stable) tuples.
//
// This complements the calculator-FSM coverage in
// TestPartitionLifecycle_DrivesFSMRebalancing (which exercises the calculator
// state stream end-to-end via real NATS / partition source); this test drives
// the calculator-state channel directly so the manager-side projection is
// proved in isolation.
func TestPartitionLifecycle_DrivesManagerStateRebalancing(t *testing.T) {
	// NOT t.Parallel(): mutates calculatorStateReconcileInterval (see the
	// reconcile test for the same reason).
	prev := calculatorStateReconcileInterval
	calculatorStateReconcileInterval = 1 * time.Hour // disable reconcile drift
	t.Cleanup(func() { calculatorStateReconcileInterval = prev })

	var (
		mu       sync.Mutex
		recorded [][2]types.State
		recordCh = make(chan struct{}, 16)
		hooksCfg types.Hooks
	)
	hooksCfg = types.Hooks{
		OnStateChanged: func(_ context.Context, from, to types.State) error {
			mu.Lock()
			recorded = append(recorded, [2]types.State{from, to})
			mu.Unlock()
			select {
			case recordCh <- struct{}{}:
			default:
			}

			return nil
		},
	}

	m := &Manager{
		cfg:       Config{DegradedAlert: DegradedAlertConfig{AlertInterval: time.Minute}},
		hooks:     &hooksCfg,
		metrics:   metrics.NewNop(),
		logger:    logging.NewNop(),
		heartbeat: heartbeat.NewNop(),
	}
	m.state.Store(int32(StateStable))
	m.startupAssignmentApplied.Store(true)
	m.workerID.Store("worker-test")
	m.assignment.Store(Assignment{})
	m.ctx, m.cancel = context.WithCancel(context.Background())
	t.Cleanup(m.cancel)

	stateCh := make(chan types.CalculatorState, 4)
	calc := &monitorTestCalculator{stateCh: stateCh}
	calc.setState(types.CalcStateIdle)

	readyCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.monitorCalculatorState(calc, readyCh)
	}()
	<-readyCh

	// Drive Idle → Rebalancing → Idle through the subscriber channel, as a
	// partition-source change would.
	calc.setState(types.CalcStateRebalancing)
	stateCh <- types.CalcStateRebalancing

	require.Eventually(t, func() bool {
		return m.State() == StateRebalancing
	}, 2*time.Second, 5*time.Millisecond,
		"Manager.State() must reach StateRebalancing after calc emits CalcStateRebalancing")

	calc.setState(types.CalcStateIdle)
	stateCh <- types.CalcStateIdle

	require.Eventually(t, func() bool {
		return m.State() == StateStable
	}, 2*time.Second, 5*time.Millisecond,
		"Manager.State() must return to StateStable after calc emits CalcStateIdle")

	// Stop the monitor cleanly so the recorder snapshot is final.
	m.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorCalculatorState did not exit on ctx cancel")
	}
	m.wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	var sawEnter, sawExit bool
	for _, p := range recorded {
		if p[0] == StateStable && p[1] == StateRebalancing {
			sawEnter = true
		}
		if p[0] == StateRebalancing && p[1] == StateStable {
			sawExit = true
		}
	}
	require.True(t, sawEnter,
		"OnStateChanged must record (Stable, Rebalancing); got %v", recorded)
	require.True(t, sawExit,
		"OnStateChanged must record (Rebalancing, Stable); got %v", recorded)
}

// TestRollingUpgrade_NewWorker_AppliesLegacyAliasWhenNoCommit verifies §3.6
// case 2 / case 3 in the alias-only world: a new (CapAckV1-capable) worker
// applies the legacy alias when no commit key exists.
func TestRollingUpgrade_NewWorker_AppliesLegacyAliasWhenNoCommit(t *testing.T) {
	t.Parallel()
	m, rh, _, _ := newTestManager(t)
	// No commit observed.
	m.lastObservedCommit.Store(nil)

	// Mimic the watcher firing on a legacy alias entry. We invoke
	// handleAssignmentEntry by directly poking the post-decode path so we
	// can stage the Assignment value precisely.
	legacy := Assignment{
		Version:        4,
		LeaderRevision: 15,
		Partitions:     []Partition{{Keys: []string{"alpha"}}, {Keys: []string{"beta"}}},
	}
	m.lastSeenAlias.Store(&legacy)
	choice := selectAuthority(nil, &legacy, 0)
	require.Equal(t, AuthorityLegacyAlias, choice)
	require.NoError(t, m.applyAssignment(legacy))

	cur := m.CurrentAssignment()
	require.Equal(t, int64(4), cur.Version)
	require.Equal(t, uint64(15), cur.LeaderRevision)
	require.Equal(t, int64(1), rh.applyCount.Load())
}
