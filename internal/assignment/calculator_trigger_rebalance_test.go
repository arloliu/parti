package assignment

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestTriggerRebalance_NoDuplicateScaleOnNextPoll verifies that a manual
// TriggerRebalance refreshes lastWorkers so the next observeAndDecide poll
// does not fire a spurious planned_scale rebalance for the same topology.
//
// Pre-PR-5 the manual-rebalance path called rebalance directly without
// updating lastWorkers; rebalance refreshes currentWorkers from a fresh
// heartbeat scan, so when TriggerRebalance is invoked while a newly-added
// worker is still pending detection by the worker monitor's watcher, the
// post-trigger state has currentWorkers={w1,w2,w3} but lastWorkers={w1,w2}
// — the next poll sees the diff and enters Scaling for a no-op cycle.
func TestTriggerRebalance_NoDuplicateScaleOnNextPoll(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-trigger-no-dup-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-trigger-no-dup-heartbeat")

	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}},
	}
	strategy := &mockStrategy{}

	// Long HeartbeatTTL keeps all workers alive for the whole test. The worker
	// monitor is stopped after the cluster settles (see below) so its watcher
	// cannot independently observe worker-3 and race the manual
	// TriggerRebalance.
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         60 * time.Second,
		EmergencyGracePeriod: 5 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             10 * time.Millisecond,
	})
	require.NoError(t, err)

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for the initial rebalance to settle.
	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateIdle && calc.CurrentVersion() > 0
	}, 5*time.Second, 25*time.Millisecond, "initial rebalance did not settle")

	// Stop the worker monitor's background watcher so no background poll can
	// rebalance behind our back and perturb the version during the behavioral
	// check below. TriggerRebalance still picks up worker-3 via its own fresh
	// KV scan (GetActiveWorkers is a direct scan, monitor-independent), and
	// IsStarted reads a calculator-level flag, so stopping the monitor does not
	// disable TriggerRebalance.
	require.NoError(t, calc.monitor.Stop())

	versionBeforeTrigger := calc.CurrentVersion()

	// Add a third worker directly to KV. TriggerRebalance fetches the worker
	// set fresh inside rebalance, so it picks up worker-3.
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-3", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	require.NoError(t, calc.TriggerRebalance(ctx))
	versionAfterTrigger := calc.CurrentVersion()
	require.Greater(t, versionAfterTrigger, versionBeforeTrigger,
		"TriggerRebalance must publish a new assignment version")

	// Primary regression guard — deterministic, with no dependence on watcher
	// quiescence or scaling-timer timing: TriggerRebalance must refresh
	// lastWorkers to match the freshly-scanned worker set. This is the exact
	// invariant the W3 fix restored. Without the refresh, lastWorkers stays
	// {worker-1, worker-2} and the next observe cycle re-enters planned_scale
	// for worker-3 (the duplicate scale this test guards against).
	calc.mu.RLock()
	lastWorkers := calc.cloneLastWorkersLocked()
	calc.mu.RUnlock()
	require.True(t, lastWorkers["worker-3"],
		"TriggerRebalance must refresh lastWorkers to include the newly-scanned worker-3")
	require.Len(t, lastWorkers, 3,
		"lastWorkers must equal the post-trigger worker set {worker-1, worker-2, worker-3}")

	// Behavioral confirmation: observing the now-unchanged topology must not
	// start another rebalance. With lastWorkers refreshed, observeAndDecide
	// short-circuits on "no change" before any scaling or rebalance, so the
	// version stays put. The monitor is stopped, so this explicit
	// checkForChanges is the only observation in flight — fully deterministic,
	// with no draining for the absence of a state transition.
	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond, "calculator should return to Idle after TriggerRebalance")
	require.NoError(t, calc.checkForChanges(ctx))
	require.Equal(t, versionAfterTrigger, calc.CurrentVersion(),
		"no further rebalance must run for an unchanged topology after TriggerRebalance")
}
