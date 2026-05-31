package failure_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLatchedWorkerVersionAdvance_DoesNotReportStableUncommitted is the
// reachability spike + verify-first reproducer for the latched-worker
// version-advance defect (docs/plans/auto-healing-quorum-loss-fix/
// 03-latched-worker-version-commitment-plan.md §5.1).
//
// Distinct from TestStartupWriteFault_DegradedRecoveryDoesNotReportStableUncommitted
// (which covers the UN-latched bootstrap worker, fixed on main): here the worker
// FIRST commits V1 cleanly (recording V1 as committed), THEN a
// version advance V1→V2 fails its claim write and the worker degrades.
//
// The bug (on the current base, where the per-version fix is NOT yet present):
//  1. Worker commits V1={p0} cleanly → committed=V1, Stable.
//  2. Claim-write fault armed; source advances to [p0,p1] → V2 published. The V2
//     apply fails the p1 claim write → scheduleApplyRetry(V2), snapshot stays V1.
//  3. Heartbeat-write fault drives the KV-error circuit → Degraded.
//  4. Recovery refresh monotonic-stores V2 into the snapshot. Because committed
//     still equals V1, the old version-only/latch guard is skipped →
//     exitDegraded → Stable, while p1's claim is unwritten (symptom a: false Stable).
//  5. The pending retry reads prev = CurrentAssignment() == V2 == next → empty
//     prepare diff → p1 is never claimed, even after the write fault clears
//     (symptom b: empty-diff non-heal, restart-only).
//
// This test asserts the FIXED behavior, so it is RED on the parent base (verified
// by reverting the three source files to the pre-fix state and re-running — BOTH
// assertions below fail). Both symptoms are recorded over a bounded poll window and
// asserted NON-fatally (assert, not require) so a single run reports both even when
// the first is violated on the parent:
//   - (a) the worker must NOT report Stable while the advanced version's claims are
//     uncommitted. RED on base: the latched worker exits to Stable@advanced version.
//   - (b) once writes recover it must self-heal p1 without a restart. RED on base:
//     the retry self-exited on the empty diff (prev == advanced snapshot == next).
func TestLatchedWorkerVersionAdvance_DoesNotReportStableUncommitted(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)
	fc := &wfFaultController{}

	// Dual-fault JS: handoff bucket faults claims/* writes; heartbeat bucket
	// faults all writes (drives the KV-error circuit from any active state).
	faultJS := newWFFaultJetStreamDual(realJS, fc)

	rfBuildStream(t, ctx, realJS)

	// Start with ONLY p0 so V1={p0}. The advance to [p0,p1] is what makes p1 the
	// newly-acquired partition whose claim write fails at V2.
	src := newWFMutableSource([]types.Partition{{Keys: []string{"p0"}}})

	claimStable := func(pid string) bool {
		_, _, stable := rfReadClaimRevision(t, ctx, realJS, pid)
		return stable
	}

	var degradedCalls atomic.Int64

	// NO faults armed at start: the worker must commit V1 cleanly and latch.
	stack := rfBuildWorkerStackCfg(t, ctx, faultJS, src, 0,
		func(cfg *parti.Config) {
			cfg.DegradedBehavior.KVErrorThreshold = 3
			cfg.DegradedBehavior.ExitThreshold = 1 * time.Second
		},
		func(h *parti.Hooks) {
			h.OnDegraded = func(_ context.Context, _ string) error {
				degradedCalls.Add(1)
				return nil
			}
		},
	)

	// (1) Clean commit of the initial {p0} assignment: record it as committed.
	// The exact starting version is leader/cold-start dependent (not necessarily
	// 1), so capture it rather than asserting a literal.
	require.NoError(t, <-stack.mgr.WaitState(parti.StateStable, 30*time.Second),
		"worker must reach Stable on the clean initial commit")
	require.Eventually(t, func() bool { return claimStable("p0") },
		20*time.Second, 100*time.Millisecond, "p0's claim must be Stable after the clean initial commit")
	vBefore := stack.mgr.CurrentAssignment().Version
	require.Positive(t, vBefore, "expected a committed initial version")

	// (2) Arm the claim-write fault, then advance the source to [p0,p1] → next
	// version. The advance's p1 claim write faults; the worker stays committed on
	// the prior claim set.
	fc.ArmWrites()
	src.set([]types.Partition{{Keys: []string{"p0"}}, {Keys: []string{"p1"}}})

	// The fault firing proves the version-advance apply attempted p1's claim
	// write. (The snapshot itself only advances to the new version via the
	// recovery refresh AFTER Degraded — that monotonic-store is part of the bug
	// mechanism — so we do NOT assert the version moved here.)
	require.Eventually(t, func() bool { return fc.writeFaultsInjected.Load() >= 1 },
		30*time.Second, 25*time.Millisecond,
		"the version-advance apply never attempted a faulted p1 claim write")

	// (3) Arm the heartbeat fault → KV-error circuit → Degraded.
	fc.ArmHeartbeat()
	require.NoError(t, <-stack.mgr.WaitState(parti.StateDegraded, 30*time.Second),
		"the KV-error circuit must enter Degraded under the sustained write fault")

	// Non-vacuous pin: the recovery refresh monotonic-stores the new (still
	// uncommitted) version into the snapshot. This happens on BOTH the parent and
	// the fix — it is the bug precondition — so the "stays degraded" observation
	// below proves the latched recovery GUARD held it, not merely that no Stable
	// transition happened to occur.
	require.Eventually(t, func() bool { return stack.mgr.CurrentAssignment().Version > vBefore },
		30*time.Second, 50*time.Millisecond,
		"the recovery refresh must advance the snapshot past the committed version")
	require.False(t, claimStable("p1"), "precondition: p1's claim is uncommitted while writes are faulted")

	// (a) SYMPTOM A — false Stable. OBSERVED (not require.Never) so the test still
	// reaches symptom (b) on the parent and asserts BOTH RED symptoms
	// independently. On the parent the latched worker skips the version-only guard
	// and exits to Stable@advanced-version with p1 unwritten; with the fix it stays
	// Degraded. Held past several ExitThreshold-spaced recovery ticks.
	sawStableUncommitted := false
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		if stack.mgr.State() == parti.StateStable &&
			stack.mgr.CurrentAssignment().Version > vBefore && !claimStable("p1") {
			sawStableUncommitted = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	assert.False(t, sawStableUncommitted,
		"SYMPTOM A: a latched worker with an uncommitted version-advance must NOT report Stable")

	t.Logf("[%s] DURING fault: writeFaults=%d asg(V=%d parts=%d) state=%s sawStableUncommitted=%v",
		t.Name(), fc.writeFaultsInjected.Load(),
		stack.mgr.CurrentAssignment().Version, len(stack.mgr.CurrentAssignment().Partitions),
		stack.mgr.State(), sawStableUncommitted)

	// --- Disarm: KV writes recover. NO restart. ---
	fc.DisarmWrites()

	// (b) SYMPTOM B — self-heal, asserted INDEPENDENTLY of (a). On the parent the
	// retry self-exited on prev==next==advanced-version (empty diff), so p1 never
	// appears even after the fault clears; with the fix the re-armed apply writes
	// it. Observed (not require.Eventually) so both symptoms are reported.
	healed := false
	for deadline := time.Now().Add(40 * time.Second); time.Now().Before(deadline); {
		if claimStable("p0") && claimStable("p1") {
			healed = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	assert.True(t, healed,
		"SYMPTOM B: after write recovery the re-armed apply must write the FULL claim set (incl. p1), no restart")

	if healed {
		require.NoError(t, <-stack.mgr.WaitState(parti.StateStable, 20*time.Second),
			"worker must reach Stable once the advanced version's claims are committed")
		require.Greater(t, stack.mgr.CurrentAssignment().Version, vBefore,
			"worker must be applied at the advanced version")

		// Consumer layer: a post-recovery publish to p1 is consumed end-to-end.
		base := stack.consumed.Load()
		rfPublish(t, ctx, realJS, "p1", "post-p1")
		require.Eventually(t, func() bool { return stack.consumed.Load() > base },
			40*time.Second, 100*time.Millisecond,
			"after recovery the consumer must pull p1 without any restart")
	}

	// Contract 3: OnDegraded fired exactly once across the held window.
	require.Equal(t, int64(1), degradedCalls.Load(),
		"OnDegraded must fire exactly once per Degraded entry (contract 3)")

	t.Logf("[%s] held Degraded under fault, self-healed V2 to Stable after recovery (no restart)", t.Name())
}
