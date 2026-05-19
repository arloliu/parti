package parti

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

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
