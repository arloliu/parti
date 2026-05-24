package assignment

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/partitest"
)

// setupF10ACalculator wires a Calculator backed by a real heartbeat KV
// pre-populated with seedWorkers active heartbeats, runs one seed
// rebalance to establish lastKnownWorkerCount, and returns the
// calculator ready for shrink-injection tests.
//
// Direct-call test fixture — the calculator is NOT Start()ed, mirroring
// the F6-B fixture's discipline (see setupF6BCalculator Godoc for why
// monitor goroutines must not race the explicit rebalance calls). The
// race detector flags any monitor goroutine writing to
// workerShrunkObservations while the test's explicit rebalance is
// reading it.
//
//nolint:unparam // confirmCount is intentionally configurable for future tests
func setupF10ACalculator(
	t *testing.T,
	ctx context.Context,
	seedWorkers int,
	confirmCount int,
	thresholdPct int,
) (*Calculator, jetstream.KeyValue) {
	t.Helper()
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-f10a-assignment-"+t.Name())
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-f10a-heartbeat-"+t.Name())

	for i := range seedWorkers {
		key := fmt.Sprintf("worker-hb.worker-%03d", i)
		_, err := heartbeatKV.Put(ctx, key, []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)
	}

	src := &mutableSource{partitions: makePartitions(seedWorkers)}

	calc, err := NewCalculator(&Config{
		AssignmentKV:                            assignmentKV,
		HeartbeatKV:                             heartbeatKV,
		AssignmentPrefix:                        "assignment",
		Source:                                  src,
		Strategy:                                &mockStrategy{},
		HeartbeatPrefix:                         "worker-hb",
		HeartbeatTTL:                            5 * time.Second,
		EmergencyGracePeriod:                    1 * time.Second,
		ColdStartWindow:                         10 * time.Millisecond,
		PlannedScaleWindow:                      10 * time.Millisecond,
		Cooldown:                                0,
		WorkerShrinkConfirmationCount:           confirmCount,
		WorkerShrinkConfirmationThresholdPct:    thresholdPct,
		PartitionShrinkConfirmationCount:        3,
		PartitionShrinkConfirmationThresholdPct: 50,
	})
	require.NoError(t, err)

	// Seed rebalance so lastKnownWorkerCount tracks the heartbeat
	// scan. The direct call avoids the monitor goroutine race.
	require.NoError(t, calc.rebalance(ctx, "test-seed"))
	require.Equal(t, seedWorkers, calc.lastKnownWorkerCount,
		"sanity: seed rebalance must establish lastKnownWorkerCount")

	return calc, heartbeatKV
}

// shrinkHeartbeatsToThree deletes 7 of the 10 seeded heartbeat keys
// (003..009), leaving workers 000..002 as the only active heartbeats.
// This simulates the calculator-visible effect of a truncated Keys()
// read — a sharply-shrunk worker scan from 10 to 3 (70 % drop, well
// past the 50 % threshold). The calculator cannot distinguish a real
// shrink from a truncated read at this layer; the defense fires
// identically. The actual nats.go truncation behavior is pinned by
// T0 in test/integration/failure/heartbeat_truncated_keys_test.go.
func shrinkHeartbeatsToThree(t *testing.T, ctx context.Context, kv jetstream.KeyValue) {
	t.Helper()
	for i := 3; i < 10; i++ {
		key := fmt.Sprintf("worker-hb.worker-%03d", i)
		require.NoError(t, kv.Delete(ctx, key))
	}
}

// TestCalculator_F10A_SuspiciousShrink_DegradesToCached drives the
// in-getActiveWorkers defense (T1 in 00-fix-plan.md §P2.5): prime
// lastKnownWorkerCount=10, inject a 3-worker observation, assert the
// calculator returns the cached 10-worker set with fresh=false and
// does NOT advance lastKnownWorkerCount. On parent + T0 (no defense):
// returns the 3-worker set with fresh=true and advances the baseline
// to 3 — the buggy behavior the defense closes.
func TestCalculator_F10A_SuspiciousShrink_DegradesToCached(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	calc, kv := setupF10ACalculator(t, ctx, 10, 2, 50)

	// Inject a sharp shrink: 10 -> 3 (70 % drop, well past the 50 %
	// threshold).
	shrinkHeartbeatsToThree(t, ctx, kv)

	workers, fresh, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.False(t, fresh,
		"suspicious shrink MUST surface as cached (fresh=false) so callers "+
			"that gate state mutations on fresh do not act on the phantom shrink")
	require.Len(t, workers, 10,
		"suspicious shrink MUST return the cached worker set, not the shrunken read")
	require.Equal(t, 10, calc.lastKnownWorkerCount,
		"a suppressed observation MUST NOT advance the baseline")
	require.Equal(t, 1, calc.workerShrunkObservations,
		"the counter must advance once per suppressed observation")
}

// TestCalculator_F10A_SuspiciousShrink_RebalanceFloor drives the
// rebalance-side floor (T2). The defense and floor compose: the first
// (ConfirmCount-1) calls degrade to cached so the floor does not see
// the shrunk count; on the ConfirmCount-th call the defense surfaces
// the shrunk read fresh and the floor blocks because no deaths have
// been confirmed. On parent (no defense, no floor): both calls commit
// new assignments to the 3-worker set.
func TestCalculator_F10A_SuspiciousShrink_RebalanceFloor(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	calc, kv := setupF10ACalculator(t, ctx, 10, 2, 50)

	shrinkHeartbeatsToThree(t, ctx, kv)

	// Call 1: defense degrades to cached (counter=1 < ConfirmCount=2).
	// Rebalance receives the cached 10-worker set and proceeds
	// harmlessly — the cached assignment is the prior trusted shape.
	require.NoError(t, calc.rebalance(ctx, "f10a-test"),
		"first call: defense degrades to cached so rebalance is a no-op")
	require.Equal(t, 1, calc.workerShrunkObservations,
		"the per-poll confirmation counter must advance once per suspicious read")
	require.Equal(t, 10, calc.lastKnownWorkerCount,
		"a cached-fallback rebalance MUST NOT advance the worker baseline")

	// Call 2: defense accepts (counter=2 >= ConfirmCount=2); the
	// floor receives the shrunk read fresh, sees no
	// emergency-confirmed deaths, and blocks the commit.
	require.ErrorIs(t, calc.rebalance(ctx, "f10a-test"), errSuspiciousWorkerObservation,
		"second call: defense surfaces the shrunk read fresh; rebalance floor MUST block "+
			"pending emergency confirmation")
	require.Equal(t, 10, calc.lastKnownWorkerCount,
		"the floor MUST NOT advance the worker baseline")
}

// TestCalculator_F10A_FloorReleasedByEmergencyConfirmation drives T3:
// the floor must release as soon as EmergencyDetector has captured
// confirmed deaths into c.disappearedWorkers. The emergency rebalance
// branch consumes that buffer (calculator.go's collectRebalanceWorkers
// only clears it on lifecycle=="emergency"); the floor's check must
// see the pre-clear state so an emergency rebalance is never wrongly
// suppressed.
func TestCalculator_F10A_FloorReleasedByEmergencyConfirmation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	calc, kv := setupF10ACalculator(t, ctx, 10, 10, 50)

	// Populate disappearedWorkers as if EmergencyDetector confirmed
	// deaths for 7 of the original 10.
	calc.mu.Lock()
	calc.disappearedWorkers = []string{
		"worker-003", "worker-004", "worker-005",
		"worker-006", "worker-007", "worker-008", "worker-009",
	}
	calc.mu.Unlock()

	shrinkHeartbeatsToThree(t, ctx, kv)

	require.NoError(t, calc.rebalance(ctx, "emergency"),
		"the floor MUST release when c.disappearedWorkers has confirmed deaths; "+
			"emergency rebalance must proceed and commit the new assignment")
}

// TestCalculator_F10A_CounterResetOnHealedRead drives T5: a single
// suspicious observation advances the counter; a subsequent healthy
// observation (no shrink) must reset it to 0 and advance the baseline.
// A later shrink then needs the FULL confirmation window again.
func TestCalculator_F10A_CounterResetOnHealedRead(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	calc, kv := setupF10ACalculator(t, ctx, 10, 3, 50)

	shrinkHeartbeatsToThree(t, ctx, kv) // suspicious
	workers, fresh, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.False(t, fresh)
	require.Len(t, workers, 10)
	require.Equal(t, 1, calc.workerShrunkObservations)
	require.Equal(t, 10, calc.lastKnownWorkerCount)

	// Heal: restore back to 10 active heartbeats.
	for i := 3; i < 10; i++ {
		key := fmt.Sprintf("worker-hb.worker-%03d", i)
		_, err := kv.Put(ctx, key, []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)
	}

	workers, fresh, err = calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.True(t, fresh, "healthy observation must be fresh")
	require.Len(t, workers, 10)
	require.Equal(t, 0, calc.workerShrunkObservations,
		"healthy observation must reset the counter")

	// A new shrink must consume the full confirmation window again.
	shrinkHeartbeatsToThree(t, ctx, kv)
	workers, fresh, err = calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.False(t, fresh)
	require.Len(t, workers, 10)
	require.Equal(t, 1, calc.workerShrunkObservations,
		"new suspicious sequence must start from counter=0, NOT continue the prior one")
}

// TestCalculator_F10A_LegitimateMassDeath_ProceedsAfterConfirmation
// drives T6: a real mass-death scenario (the emergency lifecycle has
// populated disappearedWorkers with the missing names) MUST NOT be
// suppressed by the floor. The composition with EmergencyDetector is
// the load-bearing guarantee that legitimate large drops still get
// served once detection has run.
func TestCalculator_F10A_LegitimateMassDeath_ProceedsAfterConfirmation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// ConfirmCount=1 collapses the defense to immediate acceptance so
	// the floor takes over on the first call; this isolates the
	// "emergency confirmation gate" behavior under test.
	calc, kv := setupF10ACalculator(t, ctx, 10, 1, 50)

	// First: shrink WITHOUT any emergency confirmation. Defense
	// accepts immediately (ConfirmCount=1); floor blocks because no
	// deaths confirmed.
	shrinkHeartbeatsToThree(t, ctx, kv)
	require.ErrorIs(t, calc.rebalance(ctx, "f10a-test"), errSuspiciousWorkerObservation,
		"sanity: without emergency-confirmed deaths the floor fires")
	require.Equal(t, 10, calc.lastKnownWorkerCount,
		"sanity: floor MUST NOT advance the baseline")

	// Now EmergencyDetector confirms the 7 missing workers (simulated
	// by populating the buffer directly). The next emergency
	// rebalance must proceed.
	calc.mu.Lock()
	calc.disappearedWorkers = []string{
		"worker-003", "worker-004", "worker-005",
		"worker-006", "worker-007", "worker-008", "worker-009",
	}
	calc.mu.Unlock()

	require.NoError(t, calc.rebalance(ctx, "emergency"),
		"the legitimate mass-death rebalance MUST proceed once "+
			"EmergencyDetector has populated c.disappearedWorkers")
	require.Equal(t, 3, calc.lastKnownWorkerCount,
		"the confirmed shrink MUST advance the baseline to the new (3-worker) shape")
}

// TestCalculator_F10A_CrossFeatureCounterIsolation drives T7: the
// F10-A counter and the P2.2 F6-B counter must NOT share state on the
// same Calculator. A partition-suspicious observation must not
// advance workerShrunkObservations and vice versa — the bug a single
// shared counter would create is "either lifecycle's shrink fills the
// other's confirmation window so the other never escalates".
func TestCalculator_F10A_CrossFeatureCounterIsolation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	calc, kv := setupF10ACalculator(t, ctx, 10, 3, 50)

	// Sanity: both counters start at 0.
	require.Equal(t, 0, calc.workerShrunkObservations)
	require.Equal(t, 0, calc.partitionShrunkObservations)

	// Partition-suspicious observation: shrink the source from 10 -> 2
	// (80 % drop, past the 50 % threshold). Workers are unchanged.
	src, ok := calc.Source.(*mutableSource)
	require.True(t, ok)
	src.set(makePartitions(2))

	err := calc.rebalance(ctx, "f10a-test")
	require.ErrorIs(t, err, errSuspiciousPartitionObservation,
		"partition shrink must surface the partition-side sentinel")
	require.Equal(t, 1, calc.partitionShrunkObservations,
		"the partition counter must advance")
	require.Equal(t, 0, calc.workerShrunkObservations,
		"the worker counter MUST NOT advance on a partition-side suspicion")

	// Restore source so the partition guard does not interfere.
	src.set(makePartitions(10))
	// Run a healthy rebalance to clear partitionShrunkObservations and
	// re-seat the partition baseline at 10. Worker baseline already 10.
	require.NoError(t, calc.rebalance(ctx, "f10a-test"))
	require.Equal(t, 0, calc.partitionShrunkObservations)

	// Worker-suspicious observation: shrink heartbeats from 10 -> 3.
	shrinkHeartbeatsToThree(t, ctx, kv)
	_, fresh, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.False(t, fresh)
	require.Equal(t, 1, calc.workerShrunkObservations,
		"the worker counter must advance")
	require.Equal(t, 0, calc.partitionShrunkObservations,
		"the partition counter MUST NOT advance on a worker-side suspicion")
}

// TestCalculator_F10A_NoDoubleOwnershipAcrossTruncationWindow drives
// T4 — the user-visible severity claim. Even across several
// suspicious polls (defense degrading then accepting then being
// floor-blocked), the calculator MUST NOT reassign the
// apparently-missing workers' partitions to the survivors. The
// double-ownership the spec's review §F10-A "severity is conditional"
// section names — two workers (the original assignee, still up but
// invisible in the scan, and a new assignee that took the partition
// on a phantom reassign) — would surface as a 0-key entry for one of
// workers 003..009 in currentAssignments after a published reassign.
//
// Assertion: currentAssignments retains entries for all original
// workers (0..9) across the truncation window. On parent (no defense,
// no floor): the first rebalance with a 3-worker view publishes a
// reassign that removes 003..009 from currentAssignments, failing the
// assertion.
func TestCalculator_F10A_NoDoubleOwnershipAcrossTruncationWindow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	calc, kv := setupF10ACalculator(t, ctx, 10, 2, 50)

	// Sanity: after seed, every original worker is in
	// currentAssignments (round-robin gave each one a partition).
	calc.mu.RLock()
	seedAssignmentCount := len(calc.currentAssignments)
	calc.mu.RUnlock()
	require.Equal(t, 10, seedAssignmentCount,
		"sanity: seed rebalance must assign at least one partition to each of the 10 workers")

	shrinkHeartbeatsToThree(t, ctx, kv)

	// Drive WorkerShrinkConfirmationCount + extras to cover the
	// degradation phase, the acceptance phase, and the floor-block
	// repeats. None should reassign worker 003..009's partitions to
	// workers 000..002.
	const polls = 5
	for i := range polls {
		err := calc.rebalance(ctx, "f10a-test")
		if err != nil {
			require.ErrorIs(t, err, errSuspiciousWorkerObservation,
				"poll %d: only errSuspiciousWorkerObservation is acceptable", i)
		}
	}

	calc.mu.RLock()
	postAssignmentCount := len(calc.currentAssignments)
	missing := make([]string, 0, 10)
	for i := range 10 {
		key := fmt.Sprintf("worker-%03d", i)
		if _, ok := calc.currentAssignments[key]; !ok {
			missing = append(missing, key)
		}
	}
	calc.mu.RUnlock()

	require.Equal(t, 10, postAssignmentCount,
		"across the truncation window, currentAssignments must retain entries for "+
			"all 10 original workers; missing=%v", missing)
	require.Empty(t, missing,
		"the floor + defense MUST prevent reassigning the apparently-missing "+
			"workers' (003..009) partitions to the survivors (000..002); a missing entry "+
			"is the double-ownership signature this PR closes")
}
