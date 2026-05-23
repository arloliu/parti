package parti

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestMonitorAssignmentChanges_ExhaustionEntersDegraded is the P2.4c
// reproducer pinned by `docs/plans/self-healing/00-fix-plan.md` § P2.4c:
//
//   - T1: Delete the assignment bucket while the manager runs; assert
//     bounded retries and that the envelope's permanent-failure path
//     explicitly calls enterDegraded("assignment-watcher-exhausted").
//
// Before the F2 envelope is wired into monitorAssignmentChanges, the
// loop retries forever — recordKVError on the connectivity/NotFound
// path eventually trips degraded, but a watcher whose Watch() call
// returns a non-recordable error (or one that hasn't accumulated to
// KVErrorThreshold) leaves the worker spinning without operator-
// visible escalation. The envelope's exhaustion gives a named,
// idempotent escalation signal regardless of which error path the
// per-attempt failure took.
//
// The assignment bucket is a HARD correctness dependency (workers
// cannot make assignment decisions without it), so exhaustion routes
// through enterDegraded directly rather than through the generic
// recordKVError counter.
func TestMonitorAssignmentChanges_ExhaustionEntersDegraded(t *testing.T) {
	// Tighten envelope so the test completes in a few seconds rather
	// than the production worst-case ~80s. Save and restore so other
	// tests in the package see the production defaults.
	origMax := watcherMaxAttempts
	origBase := watcherBaseBackoff
	origCap := watcherMaxBackoff
	watcherMaxAttempts = 3
	watcherBaseBackoff = 20 * time.Millisecond
	watcherMaxBackoff = 50 * time.Millisecond
	defer func() {
		watcherMaxAttempts = origMax
		watcherBaseBackoff = origBase
		watcherMaxBackoff = origCap
	}()

	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := "p24c-assignment-exhaust-" + t.Name()
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	m, _, _, _ := newTestManager(t)
	// Wire OnDegraded so the test can capture the reason argument.
	reasonSpy := &assignmentWatcherReasonSpy{}
	m.hooks = &Hooks{OnDegraded: reasonSpy.record}
	m.state.Store(int32(StateStable))
	// enterDegraded spawns monitorDegradedAlerts which needs a non-zero
	// AlertInterval; newTestManager leaves cfg zero-valued so we set
	// it explicitly here.
	m.cfg.DegradedAlert = DegradedAlertConfig{AlertInterval: time.Second}
	// Set KVErrorThreshold above watcherMaxAttempts so the test can
	// observe the envelope's exhaustion path specifically — without
	// this, recordKVError's threshold trips at the zero-value (any
	// degrading error trips on the first call) and degradedSince is
	// claimed by "KV error threshold exceeded" before the envelope
	// has a chance to escalate with the named reason this PR adds.
	// In production both paths can race; whichever wins, the worker
	// ends up Degraded. This test pins the envelope-specific reason.
	m.cfg.DegradedBehavior = DegradedBehaviorConfig{
		KVErrorThreshold: 100,
		KVErrorWindow:    30 * time.Second,
	}

	// Wrap with the force-close shim so the test can deterministically
	// trigger the supervise restart path. Without this the watcher
	// channel does not close on bucket deletion (the empirical nats.go
	// surface — see project_nats_watcher_empirical_finding).
	wrap := &forceCloseWatcherKV{KeyValue: kv}

	done := make(chan struct{})
	go func() {
		m.monitorAssignmentChanges(m.ctx, wrap)
		close(done)
	}()

	// Wait for the first watcher to be established by the goroutine.
	require.Eventually(t, func() bool {
		return wrap.watchCallsLoaded() >= 1
	}, 2*time.Second, 10*time.Millisecond, "initial Watch must run")

	// Delete the bucket. Subsequent kv.Watch calls against a deleted
	// bucket return jetstream.ErrStreamNotFound (per the empirical
	// surface pinned in project_nats_kv_delete_surface) — the envelope
	// must classify these as Transient and bound the retries.
	require.NoError(t, js.DeleteKeyValue(t.Context(), bucket))

	// Force-close the current watcher to push the loop into its
	// restart path. Subsequent Watch() calls hit the deleted bucket
	// and fail, exhausting the envelope budget.
	wrap.forceCloseLatest()

	// Worst case wall time: MaxAttempts × (base + cap-bounded backoff)
	// + jitter slack ≈ 3 × 50ms × 1.3 ≈ 200ms. Give 5s of headroom.
	require.Eventually(t, func() bool {
		return reasonSpy.has("assignment-watcher-exhausted")
	}, 5*time.Second, 25*time.Millisecond,
		"envelope exhaustion MUST call enterDegraded(\"assignment-watcher-exhausted\"); "+
			"observed reasons so far: %v", reasonSpy.snapshot())

	// The monitor must exit after exhaustion so the goroutine
	// doesn't continue generating kv.Watch API load.
	m.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorAssignmentChanges did not exit after exhaustion + ctx cancel")
	}

	// State must be Degraded.
	require.Equal(t, StateDegraded, m.State(),
		"manager must be in StateDegraded after envelope exhaustion")
}

// assignmentWatcherReasonSpy captures OnDegraded reasons so the
// reproducer can assert on the named exhaustion reason.
type assignmentWatcherReasonSpy struct {
	mu      sync.Mutex
	reasons []string
}

func (s *assignmentWatcherReasonSpy) record(_ context.Context, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reasons = append(s.reasons, reason)

	return nil
}

func (s *assignmentWatcherReasonSpy) has(reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reasons {
		if r == reason {
			return true
		}
	}

	return false
}

func (s *assignmentWatcherReasonSpy) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.reasons))
	copy(out, s.reasons)

	return out
}
