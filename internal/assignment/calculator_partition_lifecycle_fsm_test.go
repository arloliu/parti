package assignment

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// fsmPartitionFixture is a minimal partition-lifecycle test surface. Unlike
// the blocking-KV fixture used by ISSUE-002/003, this fixture wires a real
// publisher path so partition-source changes drive the FSM end-to-end.
type fsmPartitionFixture struct {
	calc          *Calculator
	source        *mockWatchableSource
	heartbeatKV   jetstream.KeyValue
	stateProvider *mockStateProvider
}

func buildFSMPartitionFixture(t *testing.T, busName string, drainInterval time.Duration, plannedScale time.Duration) *fsmPartitionFixture {
	t.Helper()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, busName+"-asgn")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, busName+"-hb")

	// Seed two workers so emergency disappearance has a target.
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-a", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-b", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockWatchableSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		},
	}
	stateProvider := &mockStateProvider{}

	cfg := &Config{
		AssignmentKV:                assignmentKV,
		HeartbeatKV:                 heartbeatKV,
		AssignmentPrefix:            "assignment",
		HeartbeatPrefix:             "worker-hb",
		HeartbeatTTL:                10 * time.Second, // poll fallback at 5s (long; tests do not depend on it)
		Source:                      source,
		Strategy:                    &mockStrategy{},
		EmergencyGracePeriod:        50 * time.Millisecond,
		Cooldown:                    1 * time.Millisecond,
		ColdStartWindow:             20 * time.Millisecond,
		PlannedScaleWindow:          plannedScale,
		RebalanceGraceDrainInterval: drainInterval,
		StateProvider:               stateProvider,
	}
	calc, err := NewCalculator(cfg)
	require.NoError(t, err)

	return &fsmPartitionFixture{
		calc:          calc,
		source:        source,
		heartbeatKV:   heartbeatKV,
		stateProvider: stateProvider,
	}
}

// recordFSMTransitions subscribes to the calculator's state-change channel
// and returns a synchronized recorder. Subscribe BEFORE driving any state
// change so initial replay is captured.
func recordFSMTransitions(calc *Calculator) (*fsmRecorder, func()) {
	ch, unsubscribe := calc.SubscribeToStateChanges()
	r := &fsmRecorder{}
	r.wg.Go(func() {
		for s := range ch {
			r.mu.Lock()
			r.states = append(r.states, s)
			r.mu.Unlock()
		}
	})

	return r, func() {
		unsubscribe()
		r.wg.Wait()
	}
}

type fsmRecorder struct {
	mu     sync.Mutex
	wg     sync.WaitGroup
	states []types.CalculatorState
}

func (r *fsmRecorder) snapshot() []types.CalculatorState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]types.CalculatorState, len(r.states))
	copy(out, r.states)
	return out
}

func (r *fsmRecorder) contains(s types.CalculatorState) bool {
	return slices.Contains(r.snapshot(), s)
}

// TestPartitionLifecycle_DrivesFSMRebalancing verifies PR-3 §5.2 — a
// partition-source change drives Idle → Rebalancing → Idle on the calculator
// FSM (which the manager maps to Stable → Rebalancing → Stable). The test
// asserts eventual presence of the transitions on the calculator-side stream
// (deterministic) rather than cross-tuple ordering of manager hooks (async).
func TestPartitionLifecycle_DrivesFSMRebalancing(t *testing.T) {
	// No t.Parallel(): tests in this file share package-level test seams
	// (partitionRebalanceBlocker, partitionRebalanceRequestTimeout,
	// partitionTailCheckTimeout) so they must run sequentially.
	ctx := t.Context()

	f := buildFSMPartitionFixture(t, "test-fsm-partition", 50*time.Millisecond, 20*time.Millisecond)
	calc, source := f.calc, f.source

	rec, stopRec := recordFSMTransitions(calc)
	defer stopRec()

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial cold-start rebalance to land and FSM to settle Idle.
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() >= 1 && calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond, "initial cold-start rebalance must complete")

	startEntries := PartitionRebalanceEntries(calc)
	startVersion := calc.CurrentVersion()

	// Push a partition-source change — this is the new FSM-claim path.
	source.Update([]types.Partition{
		{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
	})

	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > startVersion && calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond,
		"partition update must drive a rebalance via the FSM and return to Idle")

	require.Eventually(t, func() bool {
		return PartitionRebalanceEntries(calc) > startEntries
	}, 2*time.Second, 10*time.Millisecond, "handlePartitionRebalance must run for the partition update")

	// FSM-side notification stream must contain Rebalancing (the FSM claim
	// emits notifyStateChange before invoking the callback) followed
	// eventually by Idle. Presence-based assertion per §5.2.
	require.True(t, rec.contains(types.CalcStateRebalancing),
		"calculator state stream must contain Rebalancing for the partition-source change; got %v", rec.snapshot())
	require.True(t, rec.contains(types.CalcStateIdle),
		"calculator state stream must return to Idle after Rebalancing")
}

// TestPartitionLifecycle_ClaimFailureDuplicateBound verifies PR-3 §5.3 —
// when a partition-source change fires while the FSM is in Scaling, the
// claim is deferred, the drain ticker retries, and the partition-rebalance
// callback runs AT MOST ONCE (entries counter delta == 1).
func TestPartitionLifecycle_ClaimFailureDuplicateBound(t *testing.T) {
	// No t.Parallel(): tests in this file share package-level test seams
	// (partitionRebalanceBlocker, partitionRebalanceRequestTimeout,
	// partitionTailCheckTimeout) so they must run sequentially.
	ctx := t.Context()

	// Long-ish PlannedScaleWindow so we can push the partition update while
	// the FSM is in Scaling but still finish the test promptly.
	f := buildFSMPartitionFixture(t, "test-fsm-dup-bound", 25*time.Millisecond, 200*time.Millisecond)
	calc, source, heartbeatKV := f.calc, f.source, f.heartbeatKV

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()

	require.Eventually(t, func() bool {
		return calc.CurrentVersion() >= 1 && calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond, "initial cold-start rebalance must complete")

	// Trigger a worker-set change to put FSM into Scaling: add a third
	// worker heartbeat.
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-c", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Wait until FSM enters Scaling so the partition push hits a non-Idle
	// claim source.
	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateScaling
	}, 3*time.Second, 5*time.Millisecond, "FSM must enter Scaling after worker join")

	n0 := PartitionRebalanceEntries(calc)

	// Push the partition update while in Scaling: TryClaimRebalancing returns
	// false; pendingPartitionUpdate flips on; drain ticker retries after the
	// in-flight rebalance lands.
	source.Update([]types.Partition{
		{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
	})

	// Allow Scaling to complete, drain-tick to win the now-Idle CAS, and
	// at least two further drain ticks during which a buggy implementation
	// could double-fire.
	time.Sleep(200*time.Millisecond + // PlannedScaleWindow
		4*25*time.Millisecond + // 4 drain ticks
		300*time.Millisecond) // assignment settle

	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond, "FSM must return to Idle after the drain retry completes")

	n1 := PartitionRebalanceEntries(calc)
	require.Equal(t, int64(1), n1-n0,
		"partition-rebalance callback must run exactly once for one dropped partition update (n0=%d, n1=%d)", n0, n1)
}

// TestPartitionLifecycle_GraceFlipPropagatesErrShuttingDown verifies PR-3
// §5.4 — when recovery grace flips between the partition-watch arm pre-check
// (grace=false at that point) and rebalanceMu acquisition (grace flipped to
// true), c.rebalance returns errShuttingDown, the
// handlePartitionRebalance / RunClaimedRebalanceErr / triggerPartitionRebalance
// chain propagates it to restorePendingOnGraceBail, pendingPartitionUpdate
// is restored, and the drain ticker retries when grace lifts.
//
// To sequence the flip deterministically the test uses the
// partitionRebalanceBlocker test seam: handlePartitionRebalance parks on the
// blocker AFTER the partition-watch arm has already accepted the update with
// grace=false, but BEFORE c.rebalance acquires rebalanceMu. While parked the
// test flips grace to true; releasing the blocker then drives c.rebalance into
// shouldDeferForRecoveryGrace, which returns errShuttingDown.
func TestPartitionLifecycle_GraceFlipPropagatesErrShuttingDown(t *testing.T) {
	// No t.Parallel(): tests in this file share package-level test seams
	// (partitionRebalanceBlocker, partitionRebalanceRequestTimeout,
	// partitionTailCheckTimeout) so they must run sequentially.
	ctx := t.Context()

	f := buildFSMPartitionFixture(t, "test-fsm-grace-flip", 25*time.Millisecond, 20*time.Millisecond)
	calc, source, sp := f.calc, f.source, f.stateProvider

	blocker := make(chan struct{})
	SetPartitionRebalanceBlocker(blocker)
	defer SetPartitionRebalanceBlocker(nil)

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()

	require.Eventually(t, func() bool {
		return calc.CurrentVersion() >= 1 && calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond, "initial cold-start rebalance must complete")

	startEntries := PartitionRebalanceEntries(calc)
	v0 := calc.CurrentVersion()

	// Grace is FALSE here so the partition-watch pre-check accepts the update
	// and triggerPartitionRebalance proceeds to handlePartitionRebalance.
	require.False(t, sp.IsInRecoveryGrace(),
		"grace must be false so the watch-arm pre-check passes")

	source.Update([]types.Partition{
		{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
	})

	// Wait until handlePartitionRebalance is parked on the blocker — this
	// proves the watch-arm accepted the update at grace=false and we are now
	// in the window between the pre-check and rebalanceMu acquisition.
	require.Eventually(t, func() bool {
		return PartitionRebalanceEntries(calc) == startEntries+1
	}, 2*time.Second, 5*time.Millisecond,
		"handlePartitionRebalance must enter and park on the blocker")

	// Flip grace to true while the callback is parked. When we close the
	// blocker, c.rebalance acquires rebalanceMu, calls
	// shouldDeferForRecoveryGrace, sees grace=true, and returns errShuttingDown.
	// A subsequent retry-after-grace-lifts callback also reads from the now-
	// closed channel, which receives immediately — so we do NOT mutate
	// partitionRebalanceBlocker again here (mutating it from this goroutine
	// while the calculator goroutine is reading it races under -race).
	sp.SetGrace(true)
	close(blocker)

	// Phase A: no version increment must land — the rebalance bailed.
	require.Never(t, func() bool {
		return calc.CurrentVersion() > v0
	}, 250*time.Millisecond, 25*time.Millisecond,
		"rebalance must not complete after errShuttingDown bail")

	// pendingPartitionUpdate must be restored by restorePendingOnGraceBail so
	// the drain ticker has work to do once grace lifts.
	require.Eventually(t, func() bool {
		return calc.pendingPartitionUpdate.Load()
	}, 1*time.Second, 10*time.Millisecond,
		"pendingPartitionUpdate must be restored after errShuttingDown bail")

	// Phase B: lift grace; drain-tick retry must complete the rebalance.
	sp.SetGrace(false)
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > v0
	}, 2*time.Second, 10*time.Millisecond,
		"deferred partition update must drain via the FSM after grace lifts")
}

// TestPartitionLifecycle_EmergencyContentionRegression verifies PR-3 §5.5 —
// when a worker disappears during a partition-lifecycle rebalance window
// (so the concurrent worker-poll's TryClaimEmergency was rejected because the
// FSM was Rebalancing), the in-line tail-check after the partition rebalance
// MUST claim Emergency on its own — without waiting for the next worker-monitor
// poll tick.
//
// Sequencing (deterministic via partitionRebalanceBlocker + short
// EmergencyGracePeriod):
//  1. Cold-start lands; lastWorkers = {worker-a, worker-b}, FSM = Idle.
//  2. Install blocker; push partition update.
//  3. handlePartitionRebalance enters (entries++), parks on the blocker. FSM
//     is now Rebalancing.
//  4. Delete worker-a heartbeat. The WorkerMonitor watcher fires a debounced
//     pollForChanges → observeAndDecide which:
//     - sees workers = {b}, prev = {a, b} (lastWorkers unchanged because the
//     P0 fix removed the premature lastWorkers refresh in handlePartitionRebalance);
//     - records firstSeen[a] in EmergencyDetector via CheckEmergency Phase 2;
//     - TryClaimEmergency rejects (FSM is Rebalancing) — the regression case
//     the tail-check is designed to recover.
//  5. Sleep > EmergencyGracePeriod so firstSeen[a] becomes confirmable.
//  6. Close the blocker. c.rebalance runs (with workers={b}; ObserveAlive({b})
//     does not touch firstSeen[a]). handlePartitionRebalance returns; FSM
//     transitions to Idle.
//  7. Tail-check observeAndDecide runs on a fresh context. lastWorkers is still
//     {a, b} (no refresh after the partition rebalance), so CheckEmergency
//     Phase 4 fires for worker-a; the tail-check claims Emergency.
//
// Asserts the FSM stream contains CalcStateEmergency after the partition
// rebalance completes, and that it returns to CalcStateIdle eventually.
func TestPartitionLifecycle_EmergencyContentionRegression(t *testing.T) {
	// No t.Parallel(): tests in this file share package-level test seams
	// (partitionRebalanceBlocker, partitionRebalanceRequestTimeout,
	// partitionTailCheckTimeout) so they must run sequentially.
	ctx := t.Context()

	// Fixture default EmergencyGracePeriod is 50ms — short enough that the
	// worker-monitor's watcher-driven observeAndDecide can register
	// firstSeen[worker-a] and then time it out while the partition rebalance
	// is parked on the blocker.
	f := buildFSMPartitionFixture(t, "test-fsm-emergency-contention", 25*time.Millisecond, 20*time.Millisecond)
	calc, source, heartbeatKV := f.calc, f.source, f.heartbeatKV

	rec, stopRec := recordFSMTransitions(calc)
	defer stopRec()

	blocker := make(chan struct{})
	SetPartitionRebalanceBlocker(blocker)
	defer SetPartitionRebalanceBlocker(nil)

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()

	require.Eventually(t, func() bool {
		return calc.CurrentVersion() >= 1 && calc.GetState() == types.CalcStateIdle
	}, 3*time.Second, 10*time.Millisecond, "initial cold-start must complete")

	startEntries := PartitionRebalanceEntries(calc)

	// Push a partition update — handlePartitionRebalance will park on the
	// blocker AFTER incrementing the entries counter.
	source.Update([]types.Partition{
		{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
	})
	require.Eventually(t, func() bool {
		return PartitionRebalanceEntries(calc) == startEntries+1
	}, 2*time.Second, 5*time.Millisecond,
		"partition rebalance must park on the blocker")

	// Delete worker-a heartbeat: the WorkerMonitor watcher will fire a
	// debounced observeAndDecide which records firstSeen[worker-a] under
	// EmergencyDetector even though the TryClaimEmergency call rejects
	// (FSM is Rebalancing).
	require.NoError(t, heartbeatKV.Delete(ctx, "worker-hb.worker-a"))

	// Wait > debounce (100ms) + > EmergencyGracePeriod (30ms) so the
	// tail-check, when it runs, can confirm worker-a as an emergency loser.
	time.Sleep(250 * time.Millisecond)

	// Release the blocker; c.rebalance runs, returns; tail-check then sees
	// workers={b}, lastWorkers={a,b} (unchanged), and confirms emergency.
	close(blocker)

	// Emergency must be claimed via the tail-check, without waiting for the
	// next worker-monitor poll tick (poll interval is HeartbeatTTL/2 = 5s in
	// this fixture, so anything within ~2s came from the tail-check).
	require.Eventually(t, func() bool {
		return rec.contains(types.CalcStateEmergency)
	}, 2*time.Second, 10*time.Millisecond,
		"tail-check must claim Emergency for the worker that disappeared during the rebalance window; got %v",
		rec.snapshot())

	// FSM must return to Idle after the emergency rebalance.
	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateIdle
	}, 3*time.Second, 10*time.Millisecond,
		"FSM must return to Idle after the tail-check-driven emergency rebalance")
}

// TestPartitionLifecycle_TailCheckUsesFreshContext verifies PR-3 §5.7 — the
// tail-check observeAndDecide is allocated a FRESH stop-aware context, NOT
// the rebalance reqCtx that may already be cancelled by the time
// RunClaimedRebalanceErr returns. A wrong implementation that reused reqCtx
// for the tail-check would exit fast at collectWorkerObservation and strand
// any emergency loser until the next poll tick.
//
// This test asserts BOTH:
//
//   - the structural fresh-context invariant directly via
//     partitionTailCheckEntryHook (reqCtx must be cancelled, tailCtx must be
//     fresh at hook entry);
//   - that the tail-check can actually drive an Emergency claim on the fresh
//     context — proving the budget is real and reaches CheckEmergency.
//
// Sequencing mirrors TestPartitionLifecycle_EmergencyContentionRegression: a
// worker disappears while the partition rebalance is parked, so the
// worker-monitor's watcher-driven observeAndDecide registers firstSeen, the
// blocker is released after gracePeriod, and the tail-check on a fresh
// context claims Emergency.
func TestPartitionLifecycle_TailCheckUsesFreshContext(t *testing.T) {
	// No t.Parallel(): tests in this file share package-level test seams.
	ctx := t.Context()

	f := buildFSMPartitionFixture(t, "test-fsm-fresh-ctx", 25*time.Millisecond, 20*time.Millisecond)
	calc, source, heartbeatKV := f.calc, f.source, f.heartbeatKV

	rec, stopRec := recordFSMTransitions(calc)
	defer stopRec()

	prevReq := SetPartitionRebalanceRequestTimeout(10 * time.Millisecond)
	defer SetPartitionRebalanceRequestTimeout(prevReq)
	prevTail := SetPartitionTailCheckTimeout(500 * time.Millisecond)
	defer SetPartitionTailCheckTimeout(prevTail)

	blocker := make(chan struct{})
	SetPartitionRebalanceBlocker(blocker)
	defer SetPartitionRebalanceBlocker(nil)

	type hookSample struct {
		tailCtxErr error
		reqCtxErr  error
	}
	var (
		sampleMu sync.Mutex
		samples  []hookSample
	)
	SetPartitionTailCheckEntryHook(func(tailCtx context.Context, reqCtxErr error) {
		sampleMu.Lock()
		samples = append(samples, hookSample{tailCtxErr: tailCtx.Err(), reqCtxErr: reqCtxErr})
		sampleMu.Unlock()
	})
	defer SetPartitionTailCheckEntryHook(nil)

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()

	require.Eventually(t, func() bool {
		return calc.CurrentVersion() >= 1 && calc.GetState() == types.CalcStateIdle
	}, 3*time.Second, 10*time.Millisecond, "initial cold-start must complete")

	n0 := PartitionRebalanceEntries(calc)

	source.Update([]types.Partition{
		{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
	})

	require.Eventually(t, func() bool {
		return PartitionRebalanceEntries(calc) == n0+1
	}, 2*time.Second, 5*time.Millisecond, "partition callback must park on blocker")

	// Drop worker-a while the partition rebalance is parked so the
	// worker-monitor's watcher fires an observeAndDecide that registers
	// firstSeen[worker-a]. Combined with the 50ms park margin (well above the
	// 50ms EmergencyGracePeriod the fixture configures), the tail-check on a
	// FRESH context will confirm worker-a as the emergency loser; a buggy
	// implementation that reused the exhausted reqCtx would exit fast at
	// collectWorkerObservation and never reach Phase 4.
	require.NoError(t, heartbeatKV.Delete(ctx, "worker-hb.worker-a"))

	// 5x margin over the 10ms partitionRebalanceRequestTimeout AND > the
	// 50ms EmergencyGracePeriod (we sleep 250ms to also cover the 100ms
	// watcher debounce + handler latency).
	time.Sleep(250 * time.Millisecond)

	close(blocker)

	require.Eventually(t, func() bool {
		sampleMu.Lock()
		defer sampleMu.Unlock()
		return len(samples) > 0
	}, 2*time.Second, 5*time.Millisecond, "tail-check entry hook must fire")

	sampleMu.Lock()
	got := samples[0]
	sampleMu.Unlock()

	require.Error(t, got.reqCtxErr,
		"reqCtx must be cancelled by the time the tail-check runs (10ms budget + 250ms park)")
	require.NoError(t, got.tailCtxErr,
		"tail-check MUST run on a fresh context — tailCtx must NOT inherit reqCtx's cancellation")

	// Emergency must be claimed by the tail-check on its fresh context.
	// Worker-monitor poll interval in this fixture is HeartbeatTTL/2 = 5s, so
	// anything within 2s came from the tail-check.
	require.Eventually(t, func() bool {
		return rec.contains(types.CalcStateEmergency)
	}, 2*time.Second, 10*time.Millisecond,
		"tail-check on fresh context must claim Emergency for the disappeared worker; got %v",
		rec.snapshot())
}
