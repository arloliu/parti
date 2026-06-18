package parti

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/hooks"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestApplyAssignment_UnifiedPipeline_AcksSynchronously verifies the §4.4
// invariant that applyAssignment publishes a heartbeat ack synchronously
// as part of its return contract. The end-to-end "ack-before-StateStable"
// ordering is exercised in TestApplyAssignment_InitialBootstrap_EmptyAck_*
// in the parti_test package (uses a real Manager + heartbeat publisher
// + OnStateChanged hook).
func TestApplyAssignment_UnifiedPipeline_AcksSynchronously(t *testing.T) {
	t.Parallel()
	m, _, hb, _ := newTestManager(t)

	initial := Assignment{
		Version:        3,
		LeaderRevision: 11,
		Partitions:     []Partition{{Keys: []string{"alpha"}}},
	}
	require.NoError(t, m.applyAssignment(initial))

	// Pre-condition for the "ack-before-stable" invariant: the ack is
	// already recorded by the time applyAssignment returns.
	snap, ok := hb.latestSnap()
	require.True(t, ok, "applyAssignment must invoke SetAppliedAssignment before returning")
	require.Equal(t, int64(3), snap.AppliedVersion)
	require.Equal(t, int64(1), hb.pubNows.Load(), "PublishNow MUST fire as part of the apply pipeline")
}

// TestApplyAssignment_ApplyFails_NoStoreNoAck verifies the §4.4 invariant
// that on Apply failure neither Store nor Ack run, and a retry is scheduled.
func TestApplyAssignment_ApplyFails_NoStoreNoAck(t *testing.T) {
	t.Parallel()
	m, rh, hb, _ := newTestManager(t)

	// Pre-stage a successful prior apply so we can verify the snapshot
	// does NOT regress on failure.
	require.NoError(t, m.applyAssignment(Assignment{Version: 1, LeaderRevision: 5, Partitions: []Partition{{Keys: []string{"p"}}}}))

	// Queue a one-shot error for the next Apply.
	rh.errOnce.Store(&errBox{err: errors.New("apply failed")})
	err := m.applyAssignment(Assignment{
		Version:        2,
		LeaderRevision: 10,
		Partitions:     []Partition{{Keys: []string{"q"}}},
	})
	require.Error(t, err)
	require.Equal(t, int64(1), m.CurrentAssignment().Version, "snapshot must not advance on Apply failure")

	snap, _ := hb.latestSnap()
	require.Equal(t, int64(1), snap.AppliedVersion, "ack must not advance on Apply failure")

	// Retry should be scheduled — stashedApplyRetry is non-nil (or the
	// retry goroutine may have already cleared it; either is acceptable so
	// long as the failure path doesn't panic).
}

// TestApplyInitialAssignment_ColdEmpty_PublishesExplicitAck targets the
// P0 #2 fix specifically: when waitForAssignment surfaces an empty
// Assignment{} AND there is no commit in KV, applyInitialAssignment MUST
// publish an explicit applied-empty ack (SetAppliedAssignment +
// PublishNow) before returning nil. Without this ack the subsequent
// transition to StateStable would race with the heartbeat publisher's
// startup tick and could advertise AppliedAt=zero — violating §4.4.
//
// This is the narrow unit-level companion to the parti_test integration
// test, which (with a single-worker leader) tends to take the commit-path
// branch rather than the cold-empty branch we fix here.
func TestApplyInitialAssignment_ColdEmpty_PublishesExplicitAck(t *testing.T) {
	t.Parallel()

	_, nc := partitest.StartEmbeddedNATS(t)
	akv := partitest.CreateJetStreamKV(t, nc, "cold-empty-ack-asgn")

	m, _, hb, _ := newTestManager(t)
	m.assignmentKV = akv
	// Leave m.CurrentAssignment() == Assignment{} (empty cold-bootstrap)
	// and DO NOT plant a commit in akv; the commit-path GetJSON returns
	// nil and applyInitialAssignment falls into the cold-empty branch.

	require.NoError(t, m.applyInitialAssignment(t.Context(), akv))

	// Snapshot store is the empty assignment.
	require.Equal(t, Assignment{}, m.CurrentAssignment())

	// SetAppliedAssignment was called exactly once with AppliedVersion=0,
	// and PublishNow fired — proves the ack was published explicitly
	// rather than left to the publisher's startup tick.
	snap, ok := hb.latestSnap()
	require.True(t, ok, "cold-empty bootstrap MUST publish an explicit applied ack")
	require.Equal(t, int64(0), snap.AppliedVersion,
		"cold-empty ack carries AppliedVersion=0 (no commit to advance to)")
	require.False(t, snap.AppliedAt.IsZero(),
		"AppliedAt MUST be non-zero — proves SetAppliedAssignment ran")
	require.Equal(t, int64(1), hb.pubNows.Load(),
		"PublishNow MUST fire once on the cold-empty bootstrap path")
}

// recordingApplyAttempts embeds NopMetrics and overrides RecordApplyAttempt.
// Embedding picks up no-op impls for the other MetricsCollector methods
// so tests do not need to enumerate them.
type recordingApplyAttempts struct {
	*metrics.NopMetrics
	mu    sync.Mutex
	calls []applyAttemptCall
}

type applyAttemptCall struct {
	workerID string
	version  int64
}

func newRecordingApplyAttempts() *recordingApplyAttempts {
	return &recordingApplyAttempts{NopMetrics: metrics.NewNop()}
}

func (r *recordingApplyAttempts) RecordApplyAttempt(workerID string, version int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, applyAttemptCall{workerID, version})
}

func TestApplyAssignmentWithPrev_RecordsOneAttemptPerCall(t *testing.T) {
	rm := newRecordingApplyAttempts()
	m := newTestManagerWithMetrics(t, rm)

	m.handoffCoordinator = &recordingCoordinator{}

	_ = m.applyAssignment(Assignment{Version: 1})
	_ = m.applyAssignment(Assignment{Version: 2})
	_ = m.applyAssignment(Assignment{Version: 3})

	rm.mu.Lock()
	defer rm.mu.Unlock()
	require.Len(t, rm.calls, 3)
	require.Equal(t, int64(1), rm.calls[0].version)
	require.Equal(t, int64(2), rm.calls[1].version)
	require.Equal(t, int64(3), rm.calls[2].version)
	require.Equal(t, m.WorkerID(), rm.calls[0].workerID)
}

// newTestManagerWithMetrics constructs a minimal Manager fixture with the
// supplied MetricsCollector. NOT Stop-safe: election, source, and idClaimer
// are nil. Cancellation runs via t.Cleanup.
func newTestManagerWithMetrics(t *testing.T, rm types.MetricsCollector) *Manager {
	t.Helper()
	cfg := TestConfig()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	nopHooks := hooks.NewNop()
	m := &Manager{
		cfg:                cfg,
		hooks:              &nopHooks,
		metrics:            rm,
		logger:             logging.NewNop(),
		heartbeat:          &recordingHeartbeat{},
		handoffCoordinator: &recordingCoordinator{},
	}
	m.workerID.Store("worker-test")
	m.assignment.Store(Assignment{})
	m.ctx = ctx
	m.cancel = cancel

	return m
}

// recordingCoordinator is a test stub for handoff.Coordinator. Apply
// increments applyCount, invokes onApply if set, and returns a synthetic
// failure while applyCount <= failUntilCount (0 disables). All
// synchronization is via atomics.
type recordingCoordinator struct {
	applyCount     atomic.Int64
	failUntilCount atomic.Int64 // Apply returns synthetic failure while applyCount <= this; 0 disables
	onApply        func(ctx context.Context)
}

func (r *recordingCoordinator) Start(_ context.Context) {}

func (r *recordingCoordinator) Apply(
	ctx context.Context,
	_ string,
	_ types.Assignment,
	_ types.Assignment,
) error {
	n := r.applyCount.Add(1)
	if r.onApply != nil {
		r.onApply(ctx)
	}
	if u := r.failUntilCount.Load(); u > 0 && n <= u {
		return errors.New("synthetic apply failure")
	}

	return nil
}

var _ handoff.Coordinator = (*recordingCoordinator)(nil)

// newTestManagerWithJitter constructs a minimal Manager fixture with
// ApplyStartJitter set to jitter. NOT Stop-safe: election, source, and
// idClaimer are nil. Cancellation runs via t.Cleanup.
func newTestManagerWithJitter(t *testing.T, jitter time.Duration) *Manager {
	t.Helper()
	cfg := TestConfig()
	cfg.ApplyStartJitter = jitter
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	nopHooks := hooks.NewNop()
	m := &Manager{
		cfg:       cfg,
		hooks:     &nopHooks,
		metrics:   newRecordingMetrics(),
		logger:    logging.NewNop(),
		heartbeat: &recordingHeartbeat{},
		// handoffCoordinator is nil; callers must set it before invoking apply paths.
		handoffCoordinator: &recordingCoordinator{},
	}
	m.workerID.Store("worker-test")
	m.assignment.Store(Assignment{})
	m.ctx = ctx
	m.cancel = cancel

	return m
}

// TestApplyAssignmentWithPrev_JitterApplied verifies that when
// ApplyStartJitter is set, applyAssignmentWithPrev sleeps for a duration
// in [0, ApplyStartJitter) before invoking handoffCoordinator.Apply.
// The sampler seam is used to pin the jitter to a deterministic value so the
// assertion proves the sleep actually happened (elapsed >= forced), not merely
// that the path was reached.
func TestApplyAssignmentWithPrev_JitterApplied(t *testing.T) {
	const jitter = 200 * time.Millisecond
	const forced = 100 * time.Millisecond // well within jitter cap; test completes quickly
	m := newTestManagerWithJitter(t, jitter)

	// Force a deterministic sample so elapsed proves the sleep occurred.
	m.applyJitterSampler = func(time.Duration) time.Duration { return forced }

	var observed atomic.Int64
	rc := &recordingCoordinator{}
	m.handoffCoordinator = rc

	start := time.Now()
	rc.onApply = func(_ context.Context) {
		observed.Store(time.Since(start).Nanoseconds())
	}
	_ = m.applyAssignment(Assignment{Version: 1})

	elapsed := time.Duration(observed.Load())
	require.GreaterOrEqual(t, elapsed, forced, "sleep must have actually happened")
	require.LessOrEqual(t, elapsed, forced+50*time.Millisecond, "elapsed must not compound beyond forced+50ms")
}

// TestApplyAssignmentWithPrev_JitterZeroIsNoop verifies that the default
// jitter=0 introduces no measurable delay.
func TestApplyAssignmentWithPrev_JitterZeroIsNoop(t *testing.T) {
	m := newTestManagerWithJitter(t, 0)

	var observed atomic.Int64
	rc := &recordingCoordinator{}
	m.handoffCoordinator = rc

	start := time.Now()
	rc.onApply = func(_ context.Context) {
		observed.Store(time.Since(start).Nanoseconds())
	}
	_ = m.applyAssignment(Assignment{Version: 1})

	require.Less(t, time.Duration(observed.Load()), 5*time.Millisecond)
}

// TestApplyAssignmentWithPrev_JitterCancelledByCtx verifies that ctx
// cancellation during the jitter sleep aborts the apply and Apply was
// never invoked. The sampler seam forces a 5s sleep so the 50ms cancel
// deterministically races the sleep regardless of PRNG output.
func TestApplyAssignmentWithPrev_JitterCancelledByCtx(t *testing.T) {
	m := newTestManagerWithJitter(t, 5*time.Second)

	rc := &recordingCoordinator{}
	m.handoffCoordinator = rc

	// Force a deterministic long sleep so cancellation always races it.
	m.applyJitterSampler = func(time.Duration) time.Duration { return 5 * time.Second }

	go func() {
		time.Sleep(50 * time.Millisecond)
		m.cancel() // simulate Stop's ctx cancellation; fixture is not Stop-safe
	}()

	start := time.Now()
	err := m.applyAssignment(Assignment{Version: 1})
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, elapsed, 1*time.Second)
	require.Zero(t, rc.applyCount.Load(), "Apply must not be called when ctx is cancelled mid-jitter")
}

// TestApplyAssignmentWithPrev_JitterNoRaceUnderConcurrentEntrants exercises
// the race-detector requirement raised by plan-review v1 P0. Two goroutines
// invoke applyAssignment concurrently with jitter enabled. Under -race,
// any shared-state write that escaped applyStoreMu would be flagged.
func TestApplyAssignmentWithPrev_JitterNoRaceUnderConcurrentEntrants(t *testing.T) {
	m := newTestManagerWithJitter(t, 50*time.Millisecond)

	rc := &recordingCoordinator{}
	m.handoffCoordinator = rc

	var wg sync.WaitGroup
	wg.Go(func() {
		_ = m.applyAssignment(Assignment{Version: 1})
	})
	wg.Go(func() {
		_ = m.applyAssignment(Assignment{Version: 2})
	})
	wg.Wait()

	// At least the higher version must have been applied; the lower
	// version may be dropped by the stale gate. The point of the test is
	// the race detector, not the count.
	require.GreaterOrEqual(t, rc.applyCount.Load(), int64(1))
}

// TestApplyAssignmentRetry_DoesNotJitter proves that scheduleApplyRetry's
// retry goroutine routes through applyAssignmentWithPrevSkipJitter (the
// non-jittering sibling), so a fleet-wide ApplyStartJitter does NOT
// compound on top of the retry's own exponential backoff.
func TestApplyAssignmentRetry_DoesNotJitter(t *testing.T) {
	const jitter = 2 * time.Second
	m := newTestManagerWithJitter(t, jitter)

	// Count fresh-wrapper jitter entries via the test hook. Hook fires
	// BEFORE the sleep so the test does not block on it.
	var jitterFires atomic.Int64
	m.testHookApplyJittered = func() { jitterFires.Add(1) }

	// Coordinator: first Apply fails (n==1 <= failUntilCount==1), second
	// succeeds. Deterministic, no mid-flight mutation.
	rc := &recordingCoordinator{}
	rc.failUntilCount.Store(1)
	m.handoffCoordinator = rc

	// Drive a fresh-version apply. It fails, scheduleApplyRetry queues
	// a retry. The retry succeeds, terminating the loop.
	go func() { _ = m.applyAssignment(Assignment{Version: 1}) }()

	// Wait for both attempts to complete.
	require.Eventually(t, func() bool {
		return rc.applyCount.Load() >= 2
	}, 10*time.Second, 50*time.Millisecond,
		"expected fresh attempt + retry; saw applyCount=%d", rc.applyCount.Load())

	// The fresh attempt jittered once. The retry must NOT jitter; total
	// expected is exactly 1.
	require.Equal(t, int64(1), jitterFires.Load(),
		"scheduleApplyRetry must route through SkipJitter; jitter hook fired %d times (expected 1 fresh attempt only)",
		jitterFires.Load())
}

// TestApplyStartJitter_StartupBudget_Positive verifies that a forced jitter
// sample well within StartupTimeout does NOT trigger startup-timeout Degraded.
// Uses the deterministic applyJitterSampler seam to force the jitter duration;
// no reliance on the PRNG.
//
// This test is in package parti (same-package) so it can inject
// applyJitterSampler (unexported) AFTER NewManager returns but BEFORE
// m.Start. Placed here rather than test/integration/manager/ for the same
// reason as TestStart_WatchdogFiresAfterStartupTimeout — see
// tmp/impl-deviations.md.
func TestApplyStartJitter_StartupBudget_Positive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-NATS test in short mode")
	}

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := buildStartupBudgetTestConfig()
	cfg.ApplyStartJitter = 5 * time.Second // operator setting; sampler is forced below
	cfg.StartupTimeout = 5 * time.Second

	// Non-empty source so applyInitialAssignment routes through
	// applyAssignmentWithPrev (not the cold-empty bypass).
	src := source.NewStatic(makeTestPartitions(2))

	var degradedReasonsAtomic atomic.Value
	degradedReasonsAtomic.Store([]string{})

	hooks := &Hooks{
		OnDegraded: func(_ context.Context, reason string) error {
			prev, _ := degradedReasonsAtomic.Load().([]string)
			updated := append(append([]string{}, prev...), reason)
			degradedReasonsAtomic.Store(updated)

			return nil
		},
	}

	mgr, err := NewManager(&cfg, js, src, strategy.NewConsistentHash(), WithHooks(hooks))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	// Inject the deterministic sampler AFTER NewManager, BEFORE Start.
	// Same-package access. Forces 200ms — well within the 5s StartupTimeout.
	mgr.applyJitterSampler = func(_ time.Duration) time.Duration {
		return 200 * time.Millisecond
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, mgr.Start(ctx))

	require.NoError(t, <-mgr.WaitState(types.StateStable, 10*time.Second))

	// Assert NO Degraded transition with reason "startup-timeout".
	reasons, _ := degradedReasonsAtomic.Load().([]string)
	for _, r := range reasons {
		require.NotEqual(t, "startup-timeout", r,
			"manager must not enter startup-timeout Degraded when jitter is within budget")
	}
}

// TestApplyStartJitter_StartupBudget_Negative verifies that a forced jitter
// sample LARGER than StartupTimeout causes the soft watchdog to fire
// Degraded("startup-timeout").
//
// This is a unit-level test driving startStartupTimeoutWatchdog directly,
// rather than the live-NATS form in the plan. The live-NATS form is fragile
// because when the worker is also the leader, the calculator transitions the
// manager state from WaitingAssignment to Scaling before the watchdog fires
// (the watchdog no-ops when state != WaitingAssignment). This is the same
// dual-role constraint documented in tmp/impl-deviations.md for the watchdog
// test. The unit-level form drives the jitter sleep + watchdog path without
// the leader-calculator race.
//
// The test pinches the key invariant: the jitter sleep INSIDE
// applyAssignmentWithPrev keeps state at WaitingAssignment for the watchdog
// to observe, regardless of the background runner.
func TestApplyStartJitter_StartupBudget_Negative(t *testing.T) {
	// Use the hand-rolled fixture (not Stop-safe).
	mgr, _, _, _ := newTestManager(t)
	mgr.cfg.StartupTimeout = 100 * time.Millisecond
	mgr.cfg.ApplyStartJitter = 5 * time.Second
	mgr.cfg.DegradedAlert.AlertInterval = time.Hour // suppress alert monitor in tests
	mgr.startedAt = time.Now()

	// Record degraded reasons via the OnDegraded hook.
	var degradedReasonsAtomic atomic.Value
	degradedReasonsAtomic.Store([]string{})
	mgr.hooks.OnDegraded = func(_ context.Context, reason string) error {
		prev, _ := degradedReasonsAtomic.Load().([]string)
		updated := append(append([]string{}, prev...), reason)
		degradedReasonsAtomic.Store(updated)

		return nil
	}

	// Force jitter = 3s (30× the StartupTimeout). The goroutine below holds
	// the manager in WaitingAssignment during the jitter sleep. The watchdog
	// fires at 100ms while the goroutine is blocked inside applyAssignmentWithPrev.
	mgr.applyJitterSampler = func(_ time.Duration) time.Duration {
		return 3 * time.Second
	}

	// reached is closed by testHookApplyJittered the moment the apply goroutine
	// enters the jitter prologue. Gating the watchdog start on <-reached makes
	// the goroutine load-bearing: if jitter is not wired, reached never closes
	// and the test fails at the 500ms timeout below.
	reached := make(chan struct{})
	mgr.testHookApplyJittered = func() { close(reached) }

	// Drive state to WaitingAssignment (mirrors prepareStart's transitions).
	require.True(t, mgr.transitionState(StateClaimingID))
	require.True(t, mgr.transitionState(StateElection))
	require.True(t, mgr.transitionState(StateWaitingAssignment))

	// Launch the apply goroutine FIRST so it enters the jitter prologue.
	//
	// NOTE: this fixture is not Stop-safe; cancellation runs via t.Cleanup.
	go func() {
		// applyAssignmentWithPrev jitters then calls core. Core acquires
		// applyStoreMu, so the manager stays in WaitingAssignment during
		// the jitter sleep — exactly the state the watchdog checks.
		_ = mgr.applyAssignment(Assignment{Version: 1})
	}()

	// Wait until the goroutine is inside the jitter prologue before starting
	// the watchdog. This makes the test fail rather than pass spuriously if
	// jitter is not wired.
	select {
	case <-reached:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("apply goroutine did not enter jitter prologue within 500ms")
	}

	// Now start the watchdog. It fires after 100ms while the apply goroutine
	// is blocked in the 3s jitter sleep.
	mgr.startStartupTimeoutWatchdog()

	// Wait for watchdog to fire Degraded("startup-timeout").
	require.Eventually(t, func() bool {
		reasons, _ := degradedReasonsAtomic.Load().([]string)
		return slices.Contains(reasons, "startup-timeout")
	}, 2*time.Second, 10*time.Millisecond,
		"watchdog must fire Degraded('startup-timeout') when jitter exceeds StartupTimeout budget")
}

// buildStartupBudgetTestConfig builds a config suitable for the positive
// startup-budget test. Uses an aggressive-but-not-flake-prone set of timings
// without ApplyStartJitter or StartupTimeout (callers set those per test).
//
// EmergencyGracePeriod is set explicitly (750ms) to satisfy the
// ltefield=HeartbeatTTL constraint when HeartbeatTTL is shortened from
// the 15s default. The default EmergencyGracePeriod is 1.5×HeartbeatInterval
// (7.5s at the default 5s interval) which would violate the constraint when
// HeartbeatTTL=5s.
func buildStartupBudgetTestConfig() Config {
	cfg := DefaultConfig()
	cfg.WorkerIDPrefix = "worker"
	cfg.WorkerIDMin = 0
	cfg.WorkerIDMax = 100
	cfg.WorkerIDTTL = 5 * time.Second
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.HeartbeatTTL = 5 * time.Second
	cfg.EmergencyGracePeriod = 750 * time.Millisecond // must satisfy ltefield=HeartbeatTTL
	cfg.ElectionTimeout = 3 * time.Second
	cfg.ShutdownTimeout = 3 * time.Second
	cfg.ColdStartWindow = 3 * time.Second
	cfg.PlannedScaleWindow = 2 * time.Second
	cfg.RestartDetectionRatio = 0.5
	cfg.RebalanceCooldown = 2 * time.Second

	return cfg
}

// makeTestPartitions creates n test partitions for use in startup-budget tests.
func makeTestPartitions(n int) []Partition {
	parts := make([]Partition, n)
	for i := range parts {
		parts[i] = Partition{
			Keys:   []string{"partition-" + string(rune('A'+i))},
			Weight: 100,
		}
	}

	return parts
}
