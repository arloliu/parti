package assignment

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// buildLabelRecheckCalc constructs a Started-ready Calculator wired to a
// watchable source and a short RebalanceGraceDrainInterval so the label
// re-check monitor drives progression without external events. The heartbeat
// KV can be wrapped (wrapHB) to stage a connectivity-degraded enumeration for
// the spin-hazard test; sp (optional, nil-safe) wires a StateProvider for the
// recovery-grace tests. One unlabeled worker w0 is seeded — enough for the
// ghost partition to spill onto once grace expires.
func buildLabelRecheckCalc( //nolint:revive // argument-limit: test fixture bundles the scenario's coordinated knobs
	t *testing.T,
	name string,
	src *mockWatchableSource,
	grace, drain time.Duration,
	wrapHB func(kv jetstream.KeyValue) jetstream.KeyValue,
	sp types.StateProvider,
) (*Calculator, jetstream.KeyValue) {
	t.Helper()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	asgnKV := partitest.CreateJetStreamKV(t, nc, name+"-asgn")
	realHB := partitest.CreateJetStreamKV(t, nc, name+"-hb")

	putLabeledHeartbeat(t, ctx, realHB, "w0", nil)

	hbKV := realHB
	if wrapHB != nil {
		hbKV = wrapHB(realHB)
	}

	calc, err := NewCalculator(&Config{
		AssignmentKV:                asgnKV,
		HeartbeatKV:                 hbKV,
		AssignmentPrefix:            "assignment",
		HeartbeatPrefix:             "worker-hb",
		HeartbeatTTL:                30 * time.Second,
		Source:                      src,
		Strategy:                    &mockStrategy{},
		EmergencyGracePeriod:        5 * time.Second,
		Cooldown:                    0,
		ColdStartWindow:             10 * time.Millisecond,
		PlannedScaleWindow:          10 * time.Millisecond,
		LabelSpillGrace:             grace,
		RebalanceGraceDrainInterval: drain,
		StateProvider:               sp,
	})
	require.NoError(t, err)

	return calc, asgnKV
}

// waitCalcIdle blocks until the calculator has published its initial commit and
// returned to Idle.
func waitCalcIdle(t *testing.T, calc *Calculator) {
	t.Helper()
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() >= 1 && calc.GetState() == types.CalcStateIdle
	}, 3*time.Second, 10*time.Millisecond, "calc should reach Idle after the initial rebalance")
}

// TestLabelRecheck_GhostLabelProgressesWithoutExternalEvents is the spec §14
// no-external-event progression pin. A "ghost"-labeled partition that no worker
// ever carries must progress defer → confirm → park → (grace) → spill driven
// ONLY by the internal re-check timer, with a single external event (the label
// edit) at the start.
func TestLabelRecheck_GhostLabelProgressesWithoutExternalEvents(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const grace = 600 * time.Millisecond
	const drain = 150 * time.Millisecond

	// Start with a single plain partition so the cold-start rebalance succeeds
	// (no empty label pool yet).
	src := &mockWatchableSource{partitions: []types.Partition{{Keys: []string{"p"}}}}
	calc, asgnKV := buildLabelRecheckCalc(t, "lbl-recheck-ghost", src, grace, drain, nil, nil)

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()
	waitCalcIdle(t, calc)

	// ONE external event: the label edit that introduces the ghost partition.
	src.Update([]types.Partition{{Keys: []string{"p"}}, {Keys: []string{"g"}, Label: "ghost"}})

	// Phase 1 — with no further external events, the re-check timer must drive
	// defer → confirm → park within grace.
	require.Eventually(t, func() bool {
		commit := readCalcCommit(t, ctx, asgnKV)
		return commit != nil && commit.ParkedCount == 1
	}, 2*time.Second, 10*time.Millisecond, "the ghost pool must park after the deferred observation is confirmed")

	// Phase 2 — after grace expiry the grace-expiry timer must re-fire on its
	// own and the ghost must spill (ParkedCount back to 0, ghost present in some
	// worker's payload).
	require.Eventually(t, func() bool {
		commit := readCalcCommit(t, ctx, asgnKV)
		if commit == nil || commit.ParkedCount != 0 {
			return false
		}

		return ghostPresentInAnyPayload(t, ctx, asgnKV, commit)
	}, 3*time.Second, 20*time.Millisecond, "after grace the ghost partition must spill into a worker payload")
}

// ghostPresentInAnyPayload reports whether any committed payload carries a
// partition labeled "ghost".
func ghostPresentInAnyPayload(t *testing.T, ctx context.Context, kv jetstream.KeyValue, commit *types.AssignmentCommit) bool {
	t.Helper()
	for _, ref := range commit.Payloads {
		p := readCalcPayload(t, ctx, kv, ref.Key)
		for _, part := range p.Partitions {
			if part.Label == "ghost" {
				return true
			}
		}
	}

	return false
}

// TestLabelRecheck_StickyUnderBusyStateMachine pins that a re-check request
// raised while the state machine is busy (claim held) is not lost: the sticky
// pendingLabelRecheck flag survives, no rebalance runs while busy, and the
// drain tick picks it up once the claim releases.
func TestLabelRecheck_StickyUnderBusyStateMachine(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const drain = 100 * time.Millisecond

	src := &mockWatchableSource{partitions: []types.Partition{{Keys: []string{"p"}}}}
	calc, _ := buildLabelRecheckCalc(t, "lbl-recheck-busy", src, time.Hour, drain, nil, nil)

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()
	waitCalcIdle(t, calc)

	// Hold the rebalancing claim so the monitor cannot claim.
	require.True(t, calc.stateMach.TryClaimRebalancing(context.Background(), "test-hold"),
		"test must be able to claim the idle state machine")

	base := PartitionRebalanceEntries(calc)
	calc.requestLabelRecheck("grace_expiry")

	// While the claim is held: the sticky flag survives and no label-recheck
	// rebalance runs (the counter does not advance) across several drain ticks.
	require.Eventually(t, func() bool { return calc.pendingLabelRecheck.Load() }, 2*drain, drain/4,
		"pendingLabelRecheck must stay set while the state machine is busy")
	time.Sleep(3 * drain)
	require.Equal(t, base, PartitionRebalanceEntries(calc),
		"no label-recheck rebalance may run while the claim is held")
	require.True(t, calc.pendingLabelRecheck.Load(), "the flag must still be pending after the busy window")

	// Release the claim WITHOUT running a rebalance; the drain tick must now
	// pick up the pending re-check.
	calc.stateMach.ReturnToIdle()
	require.Eventually(t, func() bool {
		return PartitionRebalanceEntries(calc) > base
	}, 2*time.Second, drain/2, "the drain tick must service the pending re-check once the claim releases")
}

// TestLabelRecheck_NonFreshDoesNotSpin is the adjudicated liveness pin: a
// calculator pinned in a persistently non-fresh (connectivity-degraded)
// observation must NOT spin — the "non_fresh_observation" re-check fired from
// inside the rebalance sets the sticky flag but does not wake the monitor
// synchronously, so re-attempts happen on the drain cadence, not at full speed.
func TestLabelRecheck_NonFreshDoesNotSpin(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const drain = 100 * time.Millisecond

	failKV := &keysFailKV{keyErr: nats.ErrTimeout}
	src := &mockWatchableSource{partitions: []types.Partition{{Keys: []string{"a"}}}}
	calc, _ := buildLabelRecheckCalc(t, "lbl-recheck-spin", src, time.Hour, drain,
		func(kv jetstream.KeyValue) jetstream.KeyValue {
			failKV.KeyValue = kv
			return failKV
		}, nil)

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()
	waitCalcIdle(t, calc)

	// Degrade every enumeration: getActiveWorkers now falls back to the cached
	// worker list with fresh=false, so each rebalance takes the non-fresh path
	// and re-fires requestLabelRecheck("non_fresh_observation").
	failKV.fail.Store(true)

	// Prime the monitor once (grace_expiry does the immediate send).
	base := PartitionRebalanceEntries(calc)
	calc.requestLabelRecheck("grace_expiry")

	// No starvation: the pending re-check must be serviced repeatedly on the
	// drain cadence.
	require.Eventually(t, func() bool {
		return PartitionRebalanceEntries(calc)-base >= 2
	}, 8*drain, drain/2, "non-fresh re-checks must still re-attempt on the drain cadence (no starvation)")

	// No spin: over a fixed 3-drain window the re-attempts stay bounded to
	// roughly one per drain tick, not a runaway full-speed loop.
	mid := PartitionRebalanceEntries(calc)
	time.Sleep(3 * drain)
	delta := PartitionRebalanceEntries(calc) - mid
	require.LessOrEqual(t, delta, int64(8),
		"non-fresh re-checks must not spin: at most ~1 rebalance per drain tick (got %d over 3 drain intervals)", delta)
}

// TestLabelRecheck_DisarmedOnStop pins that the re-check monitor joins cleanly
// on Stop (no goroutine leak — Stop's wg.Wait would hang otherwise) and that no
// rebalance fires after Stop, even with a re-check requested.
func TestLabelRecheck_DisarmedOnStop(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	const drain = 100 * time.Millisecond

	src := &mockWatchableSource{partitions: []types.Partition{{Keys: []string{"p"}}}}
	calc, _ := buildLabelRecheckCalc(t, "lbl-recheck-stop", src, time.Hour, drain, nil, nil)

	require.NoError(t, calc.Start(ctx))
	waitCalcIdle(t, calc)

	// A re-check pending at Stop time must not leak the monitor goroutine.
	calc.requestLabelRecheck("grace_expiry")

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, calc.Stop(stopCtx), "Stop must return cleanly (monitor goroutine joins)")

	// After Stop, another re-check request must drive no rebalance.
	postStopVersion := calc.CurrentVersion()
	base := PartitionRebalanceEntries(calc)
	calc.requestLabelRecheck("grace_expiry")
	time.Sleep(3 * drain)

	require.False(t, calc.IsStarted(), "calc must be stopped")
	require.Equal(t, base, PartitionRebalanceEntries(calc), "no rebalance may run after Stop")
	require.Equal(t, postStopVersion, calc.CurrentVersion(), "assignment version must not advance after Stop")
}

// TestLabelRecheck_GraceFlipRestoresPendingAndDefersPublish mirrors
// TestPartitionLifecycle_GraceFlipPropagatesErrShuttingDown for the
// label_recheck lifecycle: when recovery grace flips between the monitor's
// pre-claim inRecoveryGrace check (grace=false) and rebalanceMu acquisition
// (grace=true), the in-rebalance shouldDeferForRecoveryGrace re-check must
// bail with errShuttingDown, restorePendingLabelOnGraceBail must restore
// pendingLabelRecheck, no publish may land, and the drain tick must complete
// the re-check once grace lifts.
//
// The flip is sequenced deterministically via the partitionRebalanceBlocker
// seam: handlePartitionRebalance (the shared claim-path callback) parks on the
// blocker AFTER the monitor's pre-claim check accepted the re-check at
// grace=false, but BEFORE c.rebalance acquires rebalanceMu.
func TestLabelRecheck_GraceFlipRestoresPendingAndDefersPublish(t *testing.T) {
	// No t.Parallel(): uses the shared partitionRebalanceBlocker test seam.
	ctx := t.Context()

	const drain = 25 * time.Millisecond

	sp := &mockStateProvider{}
	src := &mockWatchableSource{partitions: []types.Partition{{Keys: []string{"p"}}}}
	calc, _ := buildLabelRecheckCalc(t, "lbl-recheck-graceflip", src, time.Hour, drain, nil, sp)

	blocker := make(chan struct{})
	SetPartitionRebalanceBlocker(blocker)
	defer SetPartitionRebalanceBlocker(nil)

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()
	waitCalcIdle(t, calc)

	startEntries := PartitionRebalanceEntries(calc)
	v0 := calc.CurrentVersion()

	// Grace is FALSE so the monitor's pre-claim check passes and the claimed
	// rebalance enters handlePartitionRebalance, parking on the blocker.
	require.False(t, sp.IsInRecoveryGrace(),
		"grace must be false so the monitor's pre-claim check passes")
	calc.requestLabelRecheck("grace_expiry")

	require.Eventually(t, func() bool {
		return PartitionRebalanceEntries(calc) == startEntries+1
	}, 2*time.Second, 5*time.Millisecond,
		"the label-recheck rebalance must enter the callback and park on the blocker")

	// Flip grace while the callback is parked; releasing the blocker drives
	// c.rebalance into shouldDeferForRecoveryGrace, which must bail. The
	// closed channel also lets the retry-after-grace-lifts callback through
	// immediately (do not mutate the blocker again — that races under -race).
	sp.SetGrace(true)
	close(blocker)

	// No publish may land after the bail.
	require.Never(t, func() bool {
		return calc.CurrentVersion() > v0
	}, 250*time.Millisecond, 25*time.Millisecond,
		"rebalance must not publish after the grace-flip errShuttingDown bail")

	// restorePendingLabelOnGraceBail must restore the sticky flag so the drain
	// tick has work once grace lifts. (While grace holds, each tick re-stores
	// it via the monitor's inRecoveryGrace branch.)
	require.Eventually(t, func() bool {
		return calc.pendingLabelRecheck.Load()
	}, time.Second, 10*time.Millisecond,
		"pendingLabelRecheck must be restored after the grace-flip bail")

	// Lift grace: the drain tick must service the restored re-check.
	sp.SetGrace(false)
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > v0
	}, 2*time.Second, 10*time.Millisecond,
		"the restored label re-check must drain and publish after grace lifts")
}
