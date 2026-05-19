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

	// Long HeartbeatTTL so the worker monitor's polling fallback (hbTTL/2) is
	// slow enough that we can win the race against the watcher and call
	// TriggerRebalance before the new heartbeat key is observed.
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

	versionBeforeTrigger := calc.CurrentVersion()

	// Subscribe AFTER the cluster has settled so the initial cold-start
	// Scaling cycle does not leak into the assertion.
	states, unsubscribe := calc.SubscribeToStateChanges()
	defer unsubscribe()

	// Add a third worker directly to KV. The watcher may or may not have
	// observed it yet — that is irrelevant because TriggerRebalance fetches
	// the worker set fresh inside rebalance.
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-3", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	require.NoError(t, calc.TriggerRebalance(ctx))
	versionAfterTrigger := calc.CurrentVersion()
	require.Greater(t, versionAfterTrigger, versionBeforeTrigger,
		"TriggerRebalance must publish a new assignment version")

	// Force a worker-set observation by calling checkForChanges (the same
	// entry point the worker monitor uses). With the W3 fix, lastWorkers is
	// already aligned with the post-trigger currentWorkers, so no Scaling
	// transition should fire. Without the fix, lastWorkers is stale and
	// checkForChanges will detect a planned_scale.
	require.NoError(t, calc.checkForChanges(ctx))

	// Wait briefly for any Scaling transition to surface on the subscriber.
	scalingObserved := false
	drain := time.After(500 * time.Millisecond)
drainLoop:
	for {
		select {
		case s := <-states:
			if s == types.CalcStateScaling {
				scalingObserved = true
				break drainLoop
			}
		case <-drain:
			break drainLoop
		}
	}

	require.False(t, scalingObserved,
		"observed CalcStateScaling after TriggerRebalance — lastWorkers was not refreshed and the next checkForChanges re-entered planned_scale")
	require.Equal(t, versionAfterTrigger, calc.CurrentVersion(),
		"no further rebalance must run for an unchanged topology after TriggerRebalance")
}
