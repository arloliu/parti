package parti

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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
	// Not t.Parallel() — mutates package-level test-seam vars; see
	// the sibling test's comment for the rationale.
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

// TestMonitorAssignmentChanges_NonRecordableError_EnvelopeIsOnlyEscalation
// is the load-bearing regression for the F2 envelope on this site. The
// first reproducer above used a contrived KVErrorThreshold=100 so the
// envelope's escalation path could be observed in isolation. Under
// production defaults (KVErrorThreshold=5) the deleted-bucket failure
// pattern would trip recordKVError's threshold path FIRST and the
// envelope's named-reason path would never reach OnDegraded — making
// the new escalation effectively dead code for that specific pattern.
//
// This test exercises the failure class the F2 envelope is uniquely
// load-bearing for: a non-recordable error returned by kv.Watch on
// every attempt. recordKVError silently drops the error on each call
// (it does not match IsConnectivityError or IsDegradingJetStreamError),
// so the threshold counter never advances and the threshold-path
// escalation CANNOT trip. Only the envelope's MaxAttempts budget can
// surface this failure to operators — which is exactly the
// "Hole 1: channel-close sentinel slips past recordKVError" case from
// the PR's design analysis.
//
// Configuration uses PRODUCTION-DEFAULT KVErrorThreshold so the test
// directly demonstrates the fix is not dead code. The envelope's
// MaxAttempts is tightened only for test speed; the bounds-vs-escalation
// behavior is the same as production.
func TestMonitorAssignmentChanges_NonRecordableError_EnvelopeIsOnlyEscalation(t *testing.T) {
	// Not t.Parallel() — mutates package-level test-seam vars
	// (watcherMaxAttempts/BaseBackoff/MaxBackoff) and reading them
	// concurrently with the sibling test's defer-restore raced under
	// the race detector. Sequential execution of the envelope tests
	// keeps the seams deterministic.
	const testMaxAttempts = 3
	origMax := watcherMaxAttempts
	origBase := watcherBaseBackoff
	origCap := watcherMaxBackoff
	watcherMaxAttempts = testMaxAttempts
	watcherBaseBackoff = 20 * time.Millisecond
	watcherMaxBackoff = 50 * time.Millisecond
	defer func() {
		watcherMaxAttempts = origMax
		watcherBaseBackoff = origBase
		watcherMaxBackoff = origCap
	}()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Real bucket exists; the wrapper KV intercepts Watch() and
	// returns the simulated error before reaching JetStream.
	bucket := "p24c-nonrecord-" + t.Name()
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	m, _, _, _ := newTestManager(t)
	reasonSpy := &assignmentWatcherReasonSpy{}
	m.hooks = &Hooks{OnDegraded: reasonSpy.record}
	m.state.Store(int32(StateStable))
	m.cfg.DegradedAlert = DegradedAlertConfig{AlertInterval: time.Second}
	// PRODUCTION-DEFAULT threshold. If the test passes the envelope's
	// named-reason path must be what escalated — recordKVError's path
	// is physically incapable of reaching threshold=5 with all errors
	// dropped silently.
	m.cfg.DegradedBehavior = DegradedBehaviorConfig{
		KVErrorThreshold: 5,
		KVErrorWindow:    30 * time.Second,
	}

	// errors.New produces an error that wraps no NATS sentinel and is
	// not types.ErrConnectivity. natsutil.IsConnectivityError(err) and
	// IsDegradingJetStreamError(err) both return false. recordKVError
	// drops it silently at manager_degraded.go:84.
	nonRecordable := errors.New("simulated non-recordable transient")

	wrap := &nonRecordableWatchFailKV{
		KeyValue: kv,
		failErr:  nonRecordable,
	}

	done := make(chan struct{})
	go func() {
		m.monitorAssignmentChanges(m.ctx, wrap)
		close(done)
	}()

	require.Eventually(t, func() bool {
		return reasonSpy.has(assignmentWatcherDegradedReason)
	}, 5*time.Second, 25*time.Millisecond,
		"under production-default KVErrorThreshold=5, only the envelope's "+
			"named-reason path can escalate a sustained non-recordable error; "+
			"if this assertion fails the F2 envelope's escalation is dead code "+
			"for this failure class. observed reasons: %v", reasonSpy.snapshot())

	// CRITICAL invariant — proves the threshold path COULD NOT have
	// done the escalation; only the envelope's named-reason path
	// could. If kvErrorCount > 0 the test's premise is broken (the
	// error was actually recordable) and the assertion above is no
	// longer a load-bearing regression check.
	require.Equal(t, int32(0), m.kvErrorCount.Load(),
		"sanity: kvErrorCount must stay at 0 since every error was "+
			"dropped by recordKVError's classifier; the named-reason "+
			"escalation in this test path can ONLY have come from the "+
			"envelope's OnPermanent callback")

	m.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorAssignmentChanges did not exit after exhaustion + ctx cancel")
	}

	require.Equal(t, StateDegraded, m.State())
	require.GreaterOrEqual(t, wrap.callCount.Load(), int32(testMaxAttempts),
		"envelope must have called kv.Watch at least MaxAttempts times "+
			"before firing OnPermanent")
}

// nonRecordableWatchFailKV intercepts Watch() and returns a fixed
// non-recordable error on every call. Used by the "envelope is the
// only escalation path" test above to prove the F2 envelope is
// load-bearing for failure classes that bypass recordKVError's
// classifier.
type nonRecordableWatchFailKV struct {
	jetstream.KeyValue
	failErr   error
	callCount atomic.Int32
}

func (k *nonRecordableWatchFailKV) Watch(_ context.Context, _ string, _ ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	k.callCount.Add(1)

	return nil, k.failErr
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
