package parti

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

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
