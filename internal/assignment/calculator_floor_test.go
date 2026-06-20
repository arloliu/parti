package assignment

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --- merged from calculator_f10a_worker_floor_test.go ---

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

// --- merged from calculator_f6b_partition_floor_test.go ---

// mutableSource is a mockSource whose partitions can be mutated between
// rebalance calls, letting the F6-B tests drive specific shape sequences
// (healthy → empty → empty → healthy, etc.) deterministically.
type mutableSource struct {
	mu         sync.Mutex
	partitions []types.Partition
}

func (m *mutableSource) Start(context.Context) error { return nil }
func (m *mutableSource) Stop(context.Context) error  { return nil }
func (m *mutableSource) List(context.Context) ([]types.Partition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]types.Partition, len(m.partitions))
	copy(out, m.partitions)

	return out, nil
}

func (m *mutableSource) set(p []types.Partition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.partitions = p
}

// setupF6BCalculator wires a minimal Calculator with a mutable source and
// one worker heartbeat. The Calculator is constructed but NOT Start()ed —
// these tests drive rebalance directly to make the F6-B counter
// transitions deterministic. Calling Start would spawn monitor goroutines
// that race with the test's explicit rebalance calls (the race detector
// flagged this on the integration branch's first full-suite run; the
// monitor goroutine and test both wrote to partitionShrunkObservations
// concurrently and double-advanced the counter).
//
// confirmCount stays a parameter (even though every current call site
// passes 3) so future tests can exercise different confirmation windows.
//
// logger is optional — passing nil keeps the default nop logger; a
// recording logger lets a test assert against log-level behavior (see
// TestCalculator_F6B_SuspiciousObservation_DoesNotLogPartitionRebalanceFailed
// for the regression-pin case).
//
//nolint:unparam // confirmCount is intentionally configurable for future tests
func setupF6BCalculator(t *testing.T, ctx context.Context, src *mutableSource, confirmCount int, logger types.Logger) *Calculator {
	t.Helper()
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-f6b-assignment-"+t.Name())
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-f6b-heartbeat-"+t.Name())

	// One worker so calculator has a non-empty active set.
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	calc, err := NewCalculator(&Config{
		AssignmentKV:                     assignmentKV,
		HeartbeatKV:                      heartbeatKV,
		AssignmentPrefix:                 "assignment",
		Source:                           src,
		Strategy:                         &mockStrategy{},
		HeartbeatPrefix:                  "worker-hb",
		HeartbeatTTL:                     5 * time.Second,
		EmergencyGracePeriod:             1 * time.Second,
		ColdStartWindow:                  10 * time.Millisecond,
		PlannedScaleWindow:               10 * time.Millisecond,
		Cooldown:                         0, // test drives rebalance directly
		PartitionShrinkConfirmationCount: confirmCount,
		Logger:                           logger,
	})
	require.NoError(t, err)

	// Pump one rebalance with the healthy starting partitions so
	// lastKnownPartitionCount > 0 (the F6-B guard's enabling condition).
	// Direct call — no Start()-spawned monitor goroutines to race with.
	require.NoError(t, calc.rebalance(ctx, "test-seed"))
	require.Greater(t, calc.lastKnownPartitionCount, 0,
		"sanity: the seed rebalance must populate lastKnownPartitionCount")

	return calc
}

// TestCalculator_F6B_EmptyObservation_SuppressedUntilConfirmation drives
// the calculator with N=10 partitions then injects an empty observation
// PartitionShrinkConfirmationCount times in a row. The first
// (count - 1) calls must return errSuspiciousPartitionObservation
// without advancing lastKnownPartitionCount; the count-th call must
// accept the shrink and update the baseline.
func TestCalculator_F6B_EmptyObservation_SuppressedUntilConfirmation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	src := &mutableSource{partitions: makePartitions(10)}
	calc := setupF6BCalculator(t, ctx, src, 3, nil)

	// Seed baseline was 10 partitions. Now go empty.
	src.set(nil)

	// First (count - 1) = 2 calls: suppressed.
	for i := 1; i <= 2; i++ {
		err := calc.rebalance(ctx, "f6b-test")
		require.ErrorIs(t, err, errSuspiciousPartitionObservation,
			"observation %d/3 must surface errSuspiciousPartitionObservation", i)
		require.Equal(t, 10, calc.lastKnownPartitionCount,
			"suppressed observation MUST NOT advance lastKnownPartitionCount")
		require.Equal(t, i, calc.partitionShrunkObservations,
			"counter must advance once per suppressed observation")
	}

	// Third call: confirmation reached; shrink is honored.
	require.NoError(t, calc.rebalance(ctx, "f6b-test"))
	require.Equal(t, 0, calc.lastKnownPartitionCount,
		"after confirmation the baseline updates to the new (empty) shape")
}

// TestCalculator_F6B_SharplyShrunkObservation_Suppressed mirrors the
// empty case for the "sharply shrunk but non-empty" branch.
func TestCalculator_F6B_SharplyShrunkObservation_Suppressed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	src := &mutableSource{partitions: makePartitions(20)}
	calc := setupF6BCalculator(t, ctx, src, 3, nil)

	// 20 → 3 is an 85% drop, well below the default 50% threshold.
	src.set(makePartitions(3))

	for i := 1; i <= 2; i++ {
		err := calc.rebalance(ctx, "f6b-test")
		require.ErrorIs(t, err, errSuspiciousPartitionObservation,
			"observation %d/3 must surface errSuspiciousPartitionObservation", i)
		require.Equal(t, 20, calc.lastKnownPartitionCount,
			"suppressed observation MUST NOT advance lastKnownPartitionCount")
	}

	// Third call: confirmation reached; shrink is honored.
	require.NoError(t, calc.rebalance(ctx, "f6b-test"))
	require.Equal(t, 3, calc.lastKnownPartitionCount,
		"after confirmation the baseline updates to the new (3-partition) shape")
}

// TestCalculator_F6B_LegitimateGrowth_NotGated verifies the guard
// fires ONLY on shrinks. Growth must always be accepted immediately.
func TestCalculator_F6B_LegitimateGrowth_NotGated(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	src := &mutableSource{partitions: makePartitions(10)}
	calc := setupF6BCalculator(t, ctx, src, 3, nil)

	src.set(makePartitions(50))
	require.NoError(t, calc.rebalance(ctx, "f6b-test"),
		"growth must pass through the guard immediately")
	require.Equal(t, 50, calc.lastKnownPartitionCount)
	require.Equal(t, 0, calc.partitionShrunkObservations,
		"growth must keep the suspicious counter at 0")
}

// TestCalculator_F6B_HealingObservationResetsCounter sets up a
// half-shrunk-then-healed sequence: observation 1 is suspicious
// (counter advances), observation 2 is healthy (counter resets to 0).
// A subsequent suspicious observation then needs the FULL confirmation
// window again — the counter must start fresh.
func TestCalculator_F6B_HealingObservationResetsCounter(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	src := &mutableSource{partitions: makePartitions(10)}
	calc := setupF6BCalculator(t, ctx, src, 3, nil)

	src.set(makePartitions(3)) // 70% drop; suspicious
	err := calc.rebalance(ctx, "f6b-test")
	require.ErrorIs(t, err, errSuspiciousPartitionObservation)
	require.Equal(t, 1, calc.partitionShrunkObservations)

	src.set(makePartitions(10)) // healed
	require.NoError(t, calc.rebalance(ctx, "f6b-test"))
	require.Equal(t, 0, calc.partitionShrunkObservations,
		"healing observation must reset the counter to 0")

	src.set(makePartitions(2)) // suspicious again
	err = calc.rebalance(ctx, "f6b-test")
	require.ErrorIs(t, err, errSuspiciousPartitionObservation,
		"new suspicious sequence must start from counter=0, NOT continue the old one")
	require.Equal(t, 1, calc.partitionShrunkObservations)
}

// TestCalculator_F6B_RebalanceCallbacks_HandleSuspiciousObservation
// pins the per-callback contract for errSuspiciousPartitionObservation:
//
//   - handleRebalance (the worker-monitor lifecycle path) swallows the
//     sentinel — its caller is a periodic poll loop that does not need
//     a re-arm signal; the next poll naturally re-observes.
//   - handlePartitionRebalance (the partition-watcher lifecycle path)
//     PROPAGATES the sentinel so triggerPartitionRebalance can re-arm
//     pendingPartitionUpdate; the watcher fires only on partition-list
//     changes and a single N→0 event must not strand the confirmation
//     window. The lifecycle caller (triggerPartitionRebalance) is the
//     sole user of this callback and translates the sentinel to nil
//     after re-arming.
//
// The contracts diverge deliberately — see the per-function Godoc.
func TestCalculator_F6B_RebalanceCallbacks_HandleSuspiciousObservation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	src := &mutableSource{partitions: makePartitions(10)}
	calc := setupF6BCalculator(t, ctx, src, 3, nil)

	src.set(nil)
	require.NoError(t, calc.handleRebalance(ctx, "f6b-test"),
		"handleRebalance must swallow errSuspiciousPartitionObservation "+
			"(the periodic poll caller re-observes naturally)")
	require.ErrorIs(t, calc.handlePartitionRebalance(ctx, "f6b-test"),
		errSuspiciousPartitionObservation,
		"handlePartitionRebalance must propagate errSuspiciousPartitionObservation "+
			"so triggerPartitionRebalance can re-arm pendingPartitionUpdate")
}

// TestCalculator_F6B_SuspiciousObservation_RearmsPendingPartitionUpdate
// pins the contract that watchable-source-driven shrinks converge. The
// watcher emits exactly one signal when the source goes from N→0 (or
// N→tiny); monitorPartitions then clears pendingPartitionUpdate and
// invokes triggerPartitionRebalance. If the F6-B guard suppresses the
// first observation, the next confirmation tick has to come from the
// drainTick — which only fires when pendingPartitionUpdate is true.
//
// Without a re-arm on the suspicious-observation path the watcher
// signal is consumed once, the counter advances to 1, and the
// confirmation window stalls forever (partitions stay at 0 so no
// further watcher events arrive; drainTick has nothing pending to
// drain). This is the exact "fault-papering" pattern the goal anchor
// in docs/plans/self-healing/README.md warns against.
func TestCalculator_F6B_SuspiciousObservation_RearmsPendingPartitionUpdate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	src := &mutableSource{partitions: makePartitions(10)}
	calc := setupF6BCalculator(t, ctx, src, 3, nil)

	// Mirror monitorPartitions's pre-trigger contract: pendingPartitionUpdate
	// is cleared just BEFORE triggerPartitionRebalance runs (see
	// calculator.go's "Immediate trigger supersedes any pending deferred
	// update" comment in monitorPartitions).
	calc.pendingPartitionUpdate.Store(false)

	// Inject a suspicious shrink the F6-B guard will suppress on this
	// first observation: 10 → 0.
	src.set(nil)

	// Drive the rebalance through the same state-machine path
	// monitorPartitions uses.
	err := calc.triggerPartitionRebalance("f6b-test")
	require.NoError(t, err,
		"the lifecycle caller must see a benign nil; suspicious-observation "+
			"suppression is an explicit skip, not a failure")

	// F6-B suppression took effect: counter advanced, baseline unchanged.
	require.Equal(t, 1, calc.partitionShrunkObservations,
		"the suspicious observation must advance the confirmation counter")
	require.Equal(t, 10, calc.lastKnownPartitionCount,
		"a suppressed observation MUST NOT advance the baseline")

	// The bug this test pins. Without a re-arm, the next drainTick has
	// nothing pending, the watcher will not re-fire (partitions did not
	// change again), and the confirmation window stalls forever.
	require.True(t, calc.pendingPartitionUpdate.Load(),
		"F6-B suspicious-observation suppression MUST re-arm "+
			"pendingPartitionUpdate so the drainTick re-attempts and the "+
			"confirmation window converges; without this the watcher-driven "+
			"shrink path papers over a real fault (counter pinned at 1, "+
			"shrink never applied)")
}

// errorRecordingLogger captures Error-level log messages so a test can
// assert that a benign sentinel does NOT trigger an operator-visible
// "failed" line. It defers other levels to a delegate so a developer
// running the test can still see Info/Warn output via -v.
type errorRecordingLogger struct {
	delegate types.Logger
	mu       sync.Mutex
	errors   []string
}

func (l *errorRecordingLogger) Debug(msg string, kv ...any) { l.delegate.Debug(msg, kv...) }
func (l *errorRecordingLogger) Info(msg string, kv ...any)  { l.delegate.Info(msg, kv...) }
func (l *errorRecordingLogger) Warn(msg string, kv ...any)  { l.delegate.Warn(msg, kv...) }
func (l *errorRecordingLogger) Fatal(msg string, kv ...any) { l.delegate.Fatal(msg, kv...) }
func (l *errorRecordingLogger) Error(msg string, kv ...any) {
	l.mu.Lock()
	l.errors = append(l.errors, msg)
	l.mu.Unlock()
	l.delegate.Error(msg, kv...)
}

func (l *errorRecordingLogger) capturedErrors() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.errors))
	copy(out, l.errors)

	return out
}

// TestCalculator_F6B_SuspiciousObservation_DoesNotLogPartitionRebalanceFailed
// regression-pins the contract: a benign suspicious-observation suppression
// must not surface as an Error-level "partition rebalance failed" log line.
// The state-machine's RunClaimedRebalanceErr unconditionally logs every
// non-nil callback error at Error level; when the F6-B re-arm fix made
// handlePartitionRebalance propagate the sentinel instead of swallowing
// it, every suppressed observation began producing a spurious
// "partition rebalance failed" line — false-failure noise for operators
// tailing the worker logs.
//
// The state-machine path must skip the Error log for the
// errSuspiciousPartitionObservation sentinel specifically; the
// observability of the suppression is preserved by the Warn line that
// partitionInputCredibilityGuard already emits ("ignoring empty
// partition observation pending confirmation").
func TestCalculator_F6B_SuspiciousObservation_DoesNotLogPartitionRebalanceFailed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	rec := &errorRecordingLogger{delegate: partitest.NewTestLogger(t)}

	src := &mutableSource{partitions: makePartitions(10)}
	calc := setupF6BCalculator(t, ctx, src, 3, rec)

	// Mirror monitorPartitions's pre-trigger contract.
	calc.pendingPartitionUpdate.Store(false)

	// Inject a suspicious shrink the F6-B guard will suppress.
	src.set(nil)

	err := calc.triggerPartitionRebalance("f6b-test")
	require.NoError(t, err, "the lifecycle caller must see nil; the sentinel is benign")
	require.True(t, calc.pendingPartitionUpdate.Load(),
		"sanity: the re-arm must still happen (the prior fix is intact)")

	// The regression we're pinning.
	for _, msg := range rec.capturedErrors() {
		require.NotEqual(t, "partition rebalance failed", msg,
			"a benign suspicious-observation suppression MUST NOT surface "+
				"as an Error-level 'partition rebalance failed' log; this is "+
				"false-failure noise for operators tailing the worker logs")
	}
}

func makePartitions(n int) []types.Partition {
	out := make([]types.Partition, n)
	for i := range n {
		out[i] = types.Partition{Keys: []string{fmt.Sprintf("p%d", i)}}
	}

	return out
}

// --- merged from calculator_shrink_suspicious_test.go ---

// TestShrinkSuspicious pins the shared sharp-shrink predicate used by both the
// worker (workerObservationSuspicious) and partition
// (partitionInputCredibilityGuard) credibility guards. The small-count case is
// the load-bearing one: it discriminates the multiplied form
// (observed*100 < lastKnown*Pct) from the pre-divided form
// (observed < lastKnown*Pct/100), which integer-truncates at small counts and
// would silently miss a real shrink. Both guards delegate here so the two
// cannot drift back to the truncation-prone shape.
func TestShrinkSuspicious(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		observed     int
		lastKnown    int
		thresholdPct int
		want         bool
	}{
		{"drop to exactly the threshold is not suspicious", 5, 10, 50, false}, // 500 < 500 == false
		{"drop below the threshold is suspicious", 4, 10, 50, true},           // 400 < 500
		{"empty observation is suspicious", 0, 10, 50, true},                  // 0 < 500
		{"no drop is not suspicious", 10, 10, 50, false},                      // 1000 < 500 == false
		{"growth is not suspicious", 20, 10, 50, false},                       // 2000 < 500 == false
		// Truncation guard: with the multiplied form a 1-of-3 observation is a
		// >50% drop and suspicious (1*100=100 < 3*50=150). The pre-divided form
		// would compute 1 < (3*50)/100 = 1 < 1 = false and miss it.
		{"small-count truncation: multiplied form catches the drop", 1, 3, 50, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, shrinkSuspicious(tt.observed, tt.lastKnown, tt.thresholdPct))
		})
	}
}
