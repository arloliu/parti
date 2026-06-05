# Auto-Healing Deep-Gap Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the three well-verified auto-healing gap families from `05-deep-investigation.md` — Family A (epoch-fence recreate flap), Family B (connected-but-KV-unavailable flap), and Family C (NATS-restart non-heal) — by adding two additive, reason/epoch-aware predicates to the single recovery-exit gate, documenting the claim-loss self-stop boundary, and removing the heartbeat-bucket-loss flap.

**Architecture:** The recovery exit `attemptRecoveryFromDegraded` (`manager_degraded.go:376-416`) is trigger-blind: it exits Degraded on a healthy *assignment* read while a *different* fault (a bucket recreate, or a heartbeat/election/stableid op stall) still persists. The fix keeps the existing `currentAssignmentApplied` commitment guard and adds two **independent ANDed conjuncts** in the same function — a cheap reason-scoped heartbeat-recovery check (Family B) evaluated first, then a live epoch re-probe (Family A). Family C mechanism 1 (claim-loss self-stop past `WorkerIDTTL`) is documented as intended (orchestrator rotation), and mechanism 2 (MemoryStorage heartbeat-bucket loss) is removed by a storage-class change that is decision-gated on a Phase-0 IOPS measurement, with a fully-specified fallback.

**Tech Stack:** Go (go1.26), NATS JetStream KV, the existing `partitest`/`internal/testutil` embedded-NATS helpers, `test/integration/manager`, `test/integration/failure`, the env-gated proof suite (`PARTI_RUN_NP*`), and `docs/OPERATIONS.md`.

---

## Source of truth & scope

- **Spec:** `docs/plans/auto-healing-gap-closure/05-deep-investigation.md` (this plan implements its §7 sequencing). Read §1 (per-family findings), §2 (exit-gate refutation), §3 (cross-family interactions), §4 (contract/test matrix) before editing.
- **In scope:** Phase 0 (measurement/baseline), Phase 1 (Families A + B + C-mech1), Phase 2 (Family C-mech2).
- **Out of scope — carved out to `07-np10-enumeration-stall-plan.md`:** the new gap **NP-10** (silent leader-side heartbeat-enumeration stall). Rationale below in [NP-10 deferral](#np-10-deferred-to-07--why). The investigation report's §7 marks NP-10 "independent of Phases 1–2"; it is also materially more entangled than §5 implied (see deferral note) and must not hold up the well-verified A/B/C fixes.
- **All four gaps reproduce on HEAD `2453306`** (evidence: `tmp/repro-current-head/SUMMARY.txt`; report §0). The failing proofs already exist and are env-gated; this plan ungates each one as its matching fix lands.

## Invariant

The recovery exit must remain **additive**: every conjunct it gains is ANDed onto the existing `currentAssignmentApplied` commitment guard, never replaces it. A worker exits Degraded→Stable only when *all* of: (a) the current assignment is applied+acked, (b) no kv-unavailable degrade has an un-recovered failing op, and (c) no Parti-owned bucket wipe-and-recreate is outstanding. Removing any one conjunct must reopen exactly one proof (the `AttemptRecovery_*` suite pins (a); NP-3b pins (b); NP-1/NP-2 pin (c)).

## File Map

- Modify: `manager.go:192-206` — add two degraded-tracking fields (`lastDegradedReason`, `lastHeartbeatSuccessAt`).
- Modify: `manager_degraded.go:266-288` — stamp `lastHeartbeatSuccessAt` in `recordKVHealthyOp`.
- Modify: `manager_degraded.go:300-337` — set `lastDegradedReason` in `enterDegraded` AFTER the successful `degradedSince` CAS (winner-only); clear it in the rollback branch.
- Modify: `manager_degraded.go:340-373` — clear `lastDegradedReason` in `exitDegraded` before clearing `degradedSince`.
- Modify: `manager_degraded.go:408-416` — add the Family B then Family A conjuncts to the exit gate; add the new `epochMismatchOutstanding` helper.
- Modify: `manager_setup.go:104-119,156` — (Phase 2, Branch D only) heartbeat bucket `MemoryStorage`→`FileStorage` + the storage-choice doc comment.
- Modify: `docs/OPERATIONS.md` — (Phase 1) M5/claim-loss boundary doc; (Phase 2) heartbeat storage migration runbook.
- Modify: `test/integration/manager/np3_kv_unavailable_recovery_test.go` — ungate NP-3b.
- Modify: `test/integration/manager/np1_live_recreate_returns_stable_test.go` — ungate NP-1.
- Modify: `test/integration/failure/np2_epoch_fence_return_to_stable_test.go` — ungate NP-2.
- Modify: `test/integration/failure/np8_fleet_nats_outage_leader_continuity_test.go` — re-purpose NP-8 mech-1 to assert `StateShutdown` + add Shutdown/OnError instrumentation.
- Create: `test/integration/failure/rf3_heartbeat_rolling_restart_test.go` — (Phase 0) the RF3 replicated-MemoryStorage rolling-restart discriminator.
- Modify (Branch D): `test/integration/failure/np8_fleet_nats_outage_leader_continuity_test.go` — ungate NP-8 mech-2.

---

## Phase 0 — Measurement & baseline (gates Phase 2; NOT TDD-shaped)

These tasks produce decisions/baselines, not code under test. Structure is: run the measurement → record the result in this doc → apply the decision rule. They MUST complete before Phase 2 is started; they are independent of Phase 1.

### Task 0.1: Measure heartbeat-bucket FileStorage IOPS (the C-mech2 decision gate)

**Files:** none (measurement + record the result + decision in this section).

- [ ] **Step 1: Measure**

Using the methodology in `docs/plans/iops-investigation/` (the same harness that produced the M1.x/M2.x cells), measure the steady-state **write IOPS added by the heartbeat bucket when it is `FileStorage`** at the production `HeartbeatInterval` and target fleet size. The heartbeat publisher is the highest-frequency periodic KV op (one `Put` per worker per `HeartbeatInterval`), so this is NOT the election-bucket number — do NOT transfer M1.9 (election bucket, ~2% noise). Report §6 #1; reference cell `M2.A` (per-op consumer state file is the dominant cost) for the cost model.

- [ ] **Step 2: Record + apply Decision Rule DR-1**

Record in this doc (replace the bracketed values):

```
hb_fs_iops      = ESTIMATE: ~2-4 IOPS cluster-summed at W=5, R=3 (production HeartbeatInterval=5s);
                  flat in partition count N. Bounded above by ~4-5 IOPS (the measured
                  M1.2-minus-M1.9 all-four-KV-buckets FileStorage delta). Linear in fleet
                  size W: ~4-8 at W=10, ~20-40 at W=50 (R=3). At production server-default
                  R=1 the per-pod figure is roughly 1/3 of the R=3 cluster sum and stays
                  low-single-digit for W up to 50. NOT a measured-this-session block-IOPS
                  number — see methodology note below.
pvc_headroom    = [operator-supplied — not obtainable in this session]
DR-1 decision   = Branch D (operator-selected). hb_fs_iops is a negligible flat,
                  partition-count-independent term well within any cloud-SSD floor;
                  the FileStorage switch sits inside the same envelope already accepted
                  for the v2.5.0 election-bucket switch. Paired with the Branch-C
                  reachability conjunct (see Phase 2 SELECTED note) to cover the
                  manual-migration interim for existing MemoryStorage clusters.
```

**Methodology note (measured vs estimated; W-dependence; partition-independence).**

*What is measured (source: `docs/plans/iops-investigation/findings.md`, focused runs `m19-*` and `m2-*`, R=3, `nats:2.12.6`, ext4, capture-window means, CV < 5%).* The IOPS-investigation harness over-rides **every** Parti KV bucket — including heartbeat — with its single `--kv-storage` knob (`test/perf-measurement/cmd/harness/harness.go:179-185`, line 183; pre-create → `storageverify.Verify` → spawn-workers path in `cmd/harness/main.go:183-222`). The harness runs `--workers 5` by default (`cmd/harness/main.go:59`), matching the operator's reported 5-worker cluster. Therefore cell **M1.9** ("all parti KV buckets `Storage = memory`", −2%/−1% vs the M1.2 file-backed baseline → a flat **~4-5 IOPS** cluster-summed delta) *does* exercise the heartbeat bucket file↔memory: heartbeat was on **FileStorage** in the M1.2 baseline arm and **MemoryStorage** in the M1.9 arm. M1.9 is thus a direct empirical **upper bound** on `hb_fs_iops` — it is the combined FileStorage cost of heartbeat + election + stableID + assignment, of which heartbeat is the highest-frequency (and so dominant) sub-component. (Note: the report §5 "CRITICAL nuance" that heartbeat was MemoryStorage in both M1.9 arms is correct about *production* — `manager_setup.go:156` hardcodes MemoryStorage — but is **wrong about the rig**, which over-rides it; the nuance reasoned from the production hardcode and assumed it carried into the harness. It did not. The valid half of the caution still holds: `hb_fs_iops` is unrelated to the dominant per-partition consumer-state-file cost of M2.A.) Cell **M2.B** independently corroborates: its 2.3-IOPS residual is explicitly "parti's constant coordination floor (heartbeat / stable-ID / election KV puts on R=3)" and is **flat in N** — that flat-in-N result is the *evidence* (not just an assertion) that `hb_fs_iops` is partition-count-independent.

*What is estimated (analytical, this session — not measured here).* (1) Apportioning the ~4-5 IOPS M1.9 bundle down to heartbeat-alone (~2-4 IOPS): heartbeat is W=5 Puts every 5s = 1 Put/s fleet-wide, vs election ~1 Put/3.3s (one leader renewing at `ElectionTimeout/3`, `manager_election.go:221`) and the slower stableID renewal, so heartbeat carries the largest share of the bundle. (2) The W-scaling: heartbeat Put rate = W / HeartbeatInterval Puts/s into a single shared FileStorage stream (one per-worker key, History=1, MaxAge=HeartbeatTTL=15s; each Put is a stream append + index/meta update + the MaxAge expiry of the prior revision — `internal/heartbeat/publisher.go:405-419`, `internal/kvbuckets/builder.go`), so block-write IOPS is linear in W: ~4-8 at W=10, ~20-40 at W=50 (R=3). (3) The R=3→R=1 adjustment: the rig is R=3 throughout; production heartbeat stays server-default R=1 (`manager_setup.go:148`), so the per-pod write IOPS that PVC headroom actually sees is ~1/3 of the R=3 cluster sum.

**Partition-independence — the key framing for DR-1.** `hb_fs_iops` is a **flat additive cost**, independent of partition count N (it is fleet-size-bound: one Put per worker per HeartbeatInterval into one shared stream). This is in deliberate contrast to the dominant per-partition consumer-state-file cost (cell M2.A, the ~80% IOPS driver, which scales 0.117 IOPS/partition). Switching heartbeat to FileStorage adds a flat low-single-digit-to-tens-of-IOPS term that does not grow with N, and at production R=1 sits far under any cloud-SSD sustained-IOPS floor (e.g. GCP pd-balanced 100GB ≈ 600 IOPS) for fleets up to W=50. The operator supplies `pvc_headroom` and applies DR-1 below; the value above does not pre-empt that decision.

**Decision Rule DR-1:** if `hb_fs_iops` fits within `pvc_headroom` — i.e. the heartbeat→FileStorage switch stays inside the same provisioned-IOPS envelope already accepted for the election-bucket `MemoryStorage`→`FileStorage` switch (`manager_setup.go:108-113`), scaled for heartbeat frequency — choose **Branch D** (Task 2.1). Otherwise choose **Branch C** (Task 2.2). If `pvc_headroom` cannot be obtained, default to **Branch C** (the conservative branch that does not add durable IOPS) and note the assumption.

> This rule is the complete answer to "this can't be verified statically": the plan does not need the number to be implementation-ready — Phase 2 specifies BOTH branches and DR-1 selects between them.

### Task 0.2: RF3 replicated-MemoryStorage rolling-restart discriminator

**Files:**
- Create: `test/integration/failure/rf3_heartbeat_rolling_restart_test.go`

This settles report §6 #2: does a real RF3 cluster whose replicated `MemoryStorage` heartbeat bucket survives a one-node-at-a-time rolling restart avoid mechanism 2 entirely (severity drops to Low), or does the fleet still flap? It REUSES only the `partitest.StartEmbeddedNATSClusterN(t,5)` cluster helper (added in `00-fix-plan.md` Task 5); it is NOT a reuse of `quorum_loss_tier2_test.go` (that is a data-plane KV probe with no fleet, no MemoryStorage bucket, no rolling restart).

- [ ] **Step 1: Write the gated discriminator test**

```go
package failure_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestRF3HeartbeatRollingRestart_HoldsStable discriminates C-mech2 severity.
// A 5-node cluster hosts an RF3 (Replicas:3) MemoryStorage heartbeat bucket and
// a real 3-manager fleet. We restart nodes ONE AT A TIME (rolling), keeping
// JetStream + the replicated bucket quorum alive. If replicated MemoryStorage
// survives the rolling restart, the fleet must HOLD all-Stable AND the heartbeat
// stream Created must not change (no recreate). If it flaps, C-mech2 is a real
// fleet gap at RF3 too and Branch C/D must land.
func TestRF3HeartbeatRollingRestart_HoldsStable(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}
	if os.Getenv("PARTI_RUN_RF3_HB_ROLLING_RESTART") == "" {
		t.Skip("opt-in RF3 discriminator (C-mech2 severity); set PARTI_RUN_RF3_HB_ROLLING_RESTART=1 to run")
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()

	servers, nc := partitest.StartEmbeddedNATSClusterN(t, 5)

	cfg := testutil.IntegrationTestConfig()
	cfg.DegradedBehavior.EnterThreshold = 500 * time.Millisecond
	cfg.DegradedBehavior.ExitThreshold = 500 * time.Millisecond
	// Replicate the Parti control-plane buckets so a single-node restart keeps quorum.
	cfg.KVBuckets.Replicas = 3

	src := source.NewStatic(testutil.CreateTestPartitions(6))
	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	for range 3 {
		cluster.AddWorkerWithOptions(ctx, parti.WithHooks(&parti.Hooks{}))
	}
	cluster.StartWorkers(ctx)
	require.NotNil(t, cluster.WaitForLeader(15*time.Second))
	cluster.WaitForPartitionCoverage(6, 15*time.Second)

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	hbBefore := rf3HeartbeatCreated(t, ctx, js, cfg)

	allStable := func() bool {
		for _, m := range cluster.GetActiveWorkers() {
			if m.State() != types.StateStable {
				return false
			}
		}
		return true
	}

	// Rolling restart: stop+start each node in turn, waiting for cluster health
	// between steps so quorum is never lost.
	for i := range servers {
		partitest.RestartClusterNode(t, servers, i)
		require.Eventually(t, allStable, 30*time.Second, 200*time.Millisecond,
			"fleet must stay Stable across rolling restart of node %d", i)
	}

	// HOLD: no flap after the rolling restart completes.
	require.Never(t, func() bool { return !allStable() }, 8*time.Second, 200*time.Millisecond,
		"fleet must HOLD all-Stable after the RF3 rolling restart")
	// Discriminator: the replicated MemoryStorage heartbeat stream must not have
	// been recreated (a changed Created => quorum was lost => C-mech2 still bites).
	require.Equal(t, hbBefore, rf3HeartbeatCreated(t, ctx, js, cfg),
		"replicated heartbeat stream Created must be unchanged (no recreate) across the rolling restart")
}
```

- [ ] **Step 2: Add the helpers the test references**

If `cfg.KVBuckets.Replicas`, `partitest.RestartClusterNode`, or an `rf3HeartbeatCreated` reader do not already exist, add them in this step (do not leave them as references). `rf3HeartbeatCreated` opens a JetStream KV handle for `cfg.KVBuckets.HeartbeatBucket` and returns `kvutil.BucketStreamCreated`. `RestartClusterNode` stops `servers[i]`, waits for it to drain, then starts a replacement on the same routes/port (mirror `startEmbeddedNATSOutage`'s restart half, per node). If the cluster helper genuinely cannot do an in-place single-node restart, downgrade this task to a documented manual runbook in this doc and `log`/note that the discriminator was not automated — do not silently skip it.

- [ ] **Step 3: Run + record**

```bash
PARTI_RUN_RF3_HB_ROLLING_RESTART=1 go test ./test/integration/failure -run TestRF3HeartbeatRollingRestart_HoldsStable -count=1 -v
```

Record in this doc: `RF3 rolling-restart verdict = [HOLDS (C-mech2 severity Low) | FLAPS (C-mech2 real at RF3)]`. This does NOT change DR-1 (Branch D still dominates if IOPS clears); it sizes the residual severity if Branch C is chosen.

> **RESOLUTION (not automated; moot for the chosen branch).** `partitest` has no
> exported in-place single-node restart for a *clustered* node — `StartEmbeddedNATSClusterN`
> (`partitest/nats.go:133`) allocates per-node ports + `t.TempDir()` StoreDir internally and
> does not surface them, and `startClusterNode` (`:190`) is unexported and needs the
> ports/routes it can't reach; there is no `RestartClusterNode`. Building the discriminator
> would require extending `partitest` (expose per-node ports + a restart helper that re-creates
> a node on the same port/routes). Per Task 0.2 Step 2, this task is therefore **downgraded to a
> documented manual runbook** rather than silently skipped. **It is moot for the chosen branch:**
> Branch D makes the heartbeat bucket `FileStorage`, which survives a single-node restart
> *unconditionally* (independent of replication factor), so the "does replicated MemoryStorage
> survive a rolling restart" question only sized the residual severity of **Branch C**, which was
> not chosen. `RF3 rolling-restart verdict = NOT AUTOMATED (partitest lacks in-place node
> restart); moot for the chosen Branch D`. Manual runbook: stand up a real 5-node NATS cluster
> with an RF3 MemoryStorage heartbeat bucket + a 3-worker fleet, restart nodes one at a time
> waiting for cluster health between steps, and observe whether the fleet holds all-Stable and
> the heartbeat stream `Created` is unchanged.

### Task 0.3: `-race` baseline over the five gated flap proofs

**Files:** none (baseline record).

- [ ] **Step 1: Establish the pre-fix concurrency baseline**

The Phase-1 fixes land on the hot `kvErrorWindow`/`degradedSince` paths plus hook goroutines; capture a clean `-race` baseline first so a post-fix race is attributable.

```bash
PARTI_RUN_NP1_LIVE_RECREATE_PROOF=1 PARTI_RUN_NP2_EPOCH_FLAP_PROOF=1 \
PARTI_RUN_NP3_KVUNAVAIL_FLAP_PROOF=1 PARTI_RUN_NP8_FLEET_OUTAGE_PROOF=1 \
PARTI_RUN_NP8_HEARTBEAT_FLAP_PROOF=1 \
  go test -race ./test/integration/manager ./test/integration/failure \
  -run 'TestNP1_LiveBucketRecreate|TestNP2|TestNP3_KVUnavailable|TestNP8FleetNATSOutage' -count=1
```

Expected: the five proofs FAIL their assertions (gaps), but with **NO `-race` data-race report**. Record any pre-existing race separately — it is not introduced by this plan. (Re-run the same command after Phase 1 and Phase 2; the only delta should be assertions flipping FAIL→PASS.)

---

## Phase 1 — Exit-gate fixes (Families A + B) and C-mech1 documentation

Families A and B co-edit `attemptRecoveryFromDegraded`. They share NO state and are independently testable; one branch/PR for review economy is fine. Implement **B first** (Task 1.1) because its conjunct is a cheap atomic read that must be evaluated before A's network re-probe; Task 1.2 then shows the **full combined exit block**.

### Task 1.1: Family B — reason-scoped recover-on-wrong-signal gate (NP-3b)

**Files:**
- Modify: `manager.go:192-206`
- Modify: `manager_degraded.go:266-288` (`recordKVHealthyOp`)
- Modify: `manager_degraded.go:300-337` (`enterDegraded` — store reason after CAS)
- Modify: `manager_degraded.go:340-373` (`exitDegraded` — clear reason before `degradedSince`)
- Modify: `manager_degraded.go:408-416` (`attemptRecoveryFromDegraded`)
- Test: `test/integration/manager/np3_kv_unavailable_recovery_test.go`

- [ ] **Step 1: Confirm the reproducer fails on the parent commit**

NP-3b already exists and is env-gated. Confirm it FAILS unmodified (the gap) before changing any production code:

```bash
PARTI_RUN_NP3_KVUNAVAIL_FLAP_PROOF=1 go test ./test/integration/manager \
  -run 'TestNP3_KVUnavailable_(HeldArmed_DoesNotFalselyExitToStable|Disarm_ReturnsAndHoldsStable)' -count=1 -v
```

Expected: `HeldArmed` FAILS (`require.Zero(degradedExits())` is non-zero — the false-exit flap); `Disarm` PASSES (positive control). This is the proof the fix must flip.

- [ ] **Step 2: Add the two degraded-tracking fields**

In `manager.go`, in the "Degraded mode tracking" block (after `kvErrorWindow`, around `:204`):

```go
	// lastDegradedReason holds the reason string of the active degrade. Written by
	// the WINNING enterDegraded immediately AFTER the degradedSince CAS (losers
	// never write it, so there is no loser-clobber) and cleared to "" by
	// exitDegraded BEFORE it clears degradedSince. The reason-scoped recovery gate
	// treats an empty reason as "this entry's reason is not yet observable" and
	// stays degraded that tick — closing the tiny post-CAS-pre-store window via the
	// degradedSince happens-before, without converting degradedSince to a record.
	// atomic.Value of string ("" when never degraded / between an exit and the next
	// entry's store).
	lastDegradedReason atomic.Value
	// lastHeartbeatSuccessAt is the UnixNano of the most recent successful
	// heartbeat Put (stamped unconditionally in recordKVHealthyOp, even while
	// degraded). The reason-scoped gate uses it to confirm the failing
	// connected-but-KV-unavailable op recovered AFTER we degraded. 0 = never.
	lastHeartbeatSuccessAt atomic.Int64
```

- [ ] **Step 3: Stamp `lastHeartbeatSuccessAt` on heartbeat success**

In `manager_degraded.go`, `recordKVHealthyOp` — stamp BEFORE the degraded early-return so it updates while degraded (that is the signal the heartbeat bucket recovered):

```go
func (m *Manager) recordKVHealthyOp() {
	// Stamp the heartbeat-success time unconditionally: the reason-scoped
	// recovery gate needs to observe a heartbeat Put succeeding AFTER a
	// kv-unavailable degrade. This must run even while degraded (the
	// window-clear below intentionally does not).
	m.lastHeartbeatSuccessAt.Store(time.Now().UnixNano())

	if m.degradedSince.Load() != 0 {
		return
	}
	// ... existing transient-entry clear unchanged ...
```

- [ ] **Step 4: Own the degrade reason atomically with the winning entry (enterDegraded + exitDegraded)**

The reason MUST be written only by the CAS winner and cleared on exit before `degradedSince`, so a losing concurrent `enterDegraded` can never clobber the active reason and a recovery tick never reads a stale reason from a previous entry. (There are many degrade reasons — `NATS connection down`, `startup-timeout`, `assignment-watcher-exhausted`, `stream-missing-recovery-exhausted`, `bucket-recreated:*`, `kv-unavailable` — so a pre-CAS write is NOT safe: a loser could overwrite a real `kv-unavailable` reason with another and skip the reason-scoped gate.)

In `manager_degraded.go`, `enterDegraded` — store the reason AFTER the successful CAS (winner-only), and clear it on the rollback path:

```go
func (m *Manager) enterDegraded(reason string) {
	// Reject degraded entry from terminal Shutdown state.
	if m.State() == StateShutdown {
		return
	}

	now := time.Now()

	// Atomically claim the degraded-entry slot.
	if !m.degradedSince.CompareAndSwap(0, now.UnixNano()) {
		return
	}

	// Only the CAS winner reaches here, so this is the sole writer of the active
	// reason — no loser-clobber. The recovery gate treats the brief
	// CAS-won-but-reason-not-yet-stored window as "" and stays degraded that tick.
	m.lastDegradedReason.Store(reason)

	// Attempt validated state transition. Roll back BOTH reason and degradedSince
	// on failure (clear reason BEFORE degradedSince so the happens-before holds).
	if !m.transitionState(StateDegraded) {
		m.lastDegradedReason.Store("")
		m.degradedSince.Store(0)
		return
	}
	// ... existing hook / metric / alert-monitor unchanged ...
```

In `manager_degraded.go`, `exitDegraded` — clear the reason BEFORE clearing `degradedSince` (`:356`):

```go
	// Clear the reason BEFORE clearing degradedSince: a subsequent enterDegraded
	// can only win its CAS after degradedSince becomes 0, so this clear
	// happens-before that winner's reason store — no clobber, and the gap between
	// an exit and the next store reads as "" (gate stays that tick).
	m.lastDegradedReason.Store("")
	m.degradedSince.Store(0)
```

> Why this closes the window (verify the happens-before): `exitDegraded` does `reason.Store("")` then `degradedSince.Store(0)`; the next `enterDegraded` CAS reads that 0 (synchronizes-with), then stores its reason — so the new store strictly follows the clear, and any recovery tick that observes `degradedSince != 0` with an unstored reason reads `""` and stays. The first-ever degrade reads the zero `atomic.Value` (`Load()` is nil → `""`), same path.

- [ ] **Step 5: Add the Family B conjunct to the exit gate**

In `manager_degraded.go`, `attemptRecoveryFromDegraded`, between the `currentAssignmentApplied` block (`:408-412`) and `m.exitDegraded()` (`:415`):

```go
	cur := m.CurrentAssignment()
	if !m.currentAssignmentApplied(cur) {
		m.scheduleApplyRetry(cur)
		return
	}

	// The degrade reason is written by the winning enterDegraded just after its
	// CAS (Step 4). An empty read means this entry's reason is not yet observable
	// (or we raced an exit) — stay degraded this tick rather than risk skipping a
	// reason-scoped gate below. One-tick delay only.
	reason, _ := m.lastDegradedReason.Load().(string)
	if reason == "" {
		return
	}

	// Family B — reason-scoped recover-on-wrong-signal gate. A kv-unavailable
	// degrade is a connected-but-KV-unavailable op stall on the heartbeat /
	// election / stableid buckets; the commitment guard above reads only the
	// (unaffected) assignment bucket, so it cannot tell the failing op recovered.
	// Require a heartbeat Put success stamped AFTER we degraded. Reason-scoped:
	// a non-kv-unavailable degrade (e.g. "startup-timeout", NP-5) recovers via
	// the commitment guard alone — an UNCONDITIONAL gate regresses NP-5, which
	// has no heartbeat publisher so lastHeartbeatSuccessAt stays 0. Cheap atomic
	// reads, evaluated before the Family A network re-probe added in Task 1.2.
	if reason == degradedReasonKVUnavailable {
		hbAt := m.lastHeartbeatSuccessAt.Load()
		since := m.degradedSince.Load()
		if hbAt == 0 || hbAt <= since {
			m.logger.Debug("recovery: heartbeat KV not recovered since kv-unavailable degrade; staying Degraded",
				"last_heartbeat_success_unixnano", hbAt, "degraded_since_unixnano", since)
			return
		}
	}

	// Success - exit degraded mode
	m.exitDegraded()
```

Why it passes both NP-3b forks: while the fault is **armed**, the heartbeat `Put` keeps timing out → `recordKVHealthyOp` never fires → `lastHeartbeatSuccessAt <= degradedSince` → stay Degraded → `degradedExits()==0`. On **disarm**, the heartbeat `Put` succeeds → `lastHeartbeatSuccessAt > degradedSince` → gate opens → recover and HOLD.

- [ ] **Step 6: Ungate NP-3b**

In `np3_kv_unavailable_recovery_test.go`, delete the `PARTI_RUN_NP3_KVUNAVAIL_FLAP_PROOF` skip block (`:227-230`) so the now-passing proof runs in the default suite as a regression guard. Leave the `testing.Short()` skip.

- [ ] **Step 7: Run NP-3b + the NP-5 / NP-9 / NP-3a-Disarm regressions (`-race`)**

```bash
go test -race ./test/integration/manager \
  -run 'TestNP3_KVUnavailable_(HeldArmed_DoesNotFalselyExitToStable|Disarm_ReturnsAndHoldsStable)' -count=1
go test -race . -run 'TestNP5_BlockedApplyStartupTimeout_RecoversToStableAfterUnblock' -count=1
go test -race ./test/integration/manager -run 'TestManager_LiveNATSBucketLoss|TestManager_KVUnavailable_EntersDegraded' -count=1
```

Expected: all PASS, no `-race` report. **NP-5 is the load-bearing regression** — it degrades with reason `"startup-timeout"` (`manager_np5_blocked_apply_recovery_test.go:122`) and has no heartbeat publisher; the reason-scope must let it recover via the commitment guard (its ASSERTION 4, `:160`, must still reach Stable).

- [ ] **Step 8: Commit**

```bash
make lint
git diff --check
git add manager.go manager_degraded.go test/integration/manager/np3_kv_unavailable_recovery_test.go
git commit -m "fix(degraded): gate recovery exit on heartbeat recovery for kv-unavailable

A connected-but-KV-unavailable op stall degrades with reason kv-unavailable,
but the recovery exit only re-reads the unaffected assignment bucket and so
falsely returns to Stable while heartbeat/election/stableid still fault. Require
a heartbeat Put success stamped after the degrade before exiting a kv-unavailable
degrade; reason-scoped so a startup-timeout degrade still recovers on the
commitment guard alone."
```

### Task 1.2: Family A — live epoch re-probe exit gate (NP-2, NP-1)

**Files:**
- Modify: `manager_degraded.go` (add `epochMismatchOutstanding`; add the A conjunct after the B conjunct)
- Test: `test/integration/failure/np2_epoch_fence_return_to_stable_test.go`, `test/integration/manager/np1_live_recreate_returns_stable_test.go`

- [ ] **Step 1: Confirm both reproducers fail on the parent commit**

```bash
PARTI_RUN_NP2_EPOCH_FLAP_PROOF=1 go test ./test/integration/failure -run 'TestNP2' -count=1 -v
PARTI_RUN_NP1_LIVE_RECREATE_PROOF=1 go test ./test/integration/manager -run 'TestNP1_LiveBucketRecreate' -count=1 -v
```

Expected: NP-2 FAILS (`require.Zero(degradedToStable)` and/or `require.Equal(StateDegraded, finalState)` violated, `:204,:212`); NP-1 FAILS (`require.Empty(healed)` violated, `:248` — workers heal to Stable on the recreated-empty buckets).

- [ ] **Step 2: Add the `epochMismatchOutstanding` helper**

In `manager_degraded.go` (it already imports `context`, `time`, `errors`; add `"github.com/arloliu/parti/v2/kvutil"` to the import block — the same helper `manager_setup.go` uses):

```go
// epochMismatchOutstanding reports whether any Parti-owned bucket's LIVE
// stream-Created no longer matches the value captured at Start — i.e. a
// wipe-and-recreate is still in effect. It is the Family A recovery-exit guard:
// the commitment guard can be satisfied by a version-monotonic republish into a
// recreated-empty bucket, so the exit must additionally refuse while a recreate
// is outstanding (terminal Degraded => restart/rotation, docs/OPERATIONS.md).
//
// This is a LIVE re-probe (not a latch) so there is no pre-arm window: the
// epoch-fence monitor ticks every OperationTimeout (10s) while recovery ticks
// every 1s, so a latch could arm after a faster recovery had already exited.
//
// Probe errors are NOT actionable and are skipped (mirroring checkBucketEpochs'
// continue-on-error): only a successful read with a DIFFERENT Created is a
// recreate. A missing/timing-out bucket is the connection monitor's / Family B's
// concern, not Family A's — this keeps the two exit conjuncts independent (see
// 05-deep-investigation.md §2, the timeout-interpretation conflict).
//
// Concurrency: m.bucketEpochs is written only at Start (captureBucketEpoch) and
// is read-only afterward, so ranging it from this (connection-monitor) goroutine
// is race-free — the SAME lock-free-read contract checkBucketEpochs relies on.
// This holds ONLY while nothing mutates the map after Start; Phase-2 Branch
// C-recreate (Task 2.2) would mutate it, and if selected MUST add a dedicated
// bucketEpochs lock taken by every reader (this helper AND checkBucketEpochs) and
// post-Start writer. It opens a FRESH probe handle per bucket rather than reusing
// ep.kv, which is owned by the monitorBucketEpochs goroutine and is not safe to
// share across goroutines (nats.go KeyValue handles cache *stream state).
func (m *Manager) epochMismatchOutstanding(ctx context.Context) bool {
	if m.js == nil {
		return false
	}
	for bucket, ep := range m.bucketEpochs {
		probeKV, err := m.js.KeyValue(ctx, bucket)
		if err != nil {
			m.logger.Debug("epoch re-probe: open handle failed; not actionable", "bucket", bucket, "error", err)
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, m.cfg.OperationTimeout)
		live, err := kvutil.BucketStreamCreated(probeCtx, probeKV)
		cancel()
		if err != nil {
			m.logger.Debug("epoch re-probe: probe failed; not actionable", "bucket", bucket, "error", err)
			continue
		}
		if !live.Equal(ep.created) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 3: Add the Family A conjunct (full combined exit block)**

In `attemptRecoveryFromDegraded`, add the A conjunct AFTER the Family B conjunct from Task 1.1. The complete block (commitment guard → B → A → exit) reads:

```go
	cur := m.CurrentAssignment()
	if !m.currentAssignmentApplied(cur) {
		m.scheduleApplyRetry(cur)
		return
	}

	// Reason not yet observable for this entry (winner stores it just after the
	// CAS) — stay this tick. See Task 1.1 Step 4.
	reason, _ := m.lastDegradedReason.Load().(string)
	if reason == "" {
		return
	}

	// Family B — reason-scoped recover-on-wrong-signal gate (cheap atomic reads;
	// evaluated first so a kv-unavailable stall short-circuits before the Family A
	// network re-probe below). See Task 1.1 for the full rationale.
	if reason == degradedReasonKVUnavailable {
		hbAt := m.lastHeartbeatSuccessAt.Load()
		since := m.degradedSince.Load()
		if hbAt == 0 || hbAt <= since {
			m.logger.Debug("recovery: heartbeat KV not recovered since kv-unavailable degrade; staying Degraded",
				"last_heartbeat_success_unixnano", hbAt, "degraded_since_unixnano", since)
			return
		}
	}

	// Family A — refuse a recovery exit while a Parti-owned bucket wipe-and-
	// recreate is still outstanding. A live re-probe; a missing/timing-out bucket
	// is skipped (Family B's / the connection monitor's concern), so this conjunct
	// fires only on a CONFIRMED Created mismatch.
	if m.epochMismatchOutstanding(m.ctx) {
		m.logger.Warn("recovery: bucket epoch mismatch outstanding; staying Degraded for restart/rotation")
		return
	}

	// Success - exit degraded mode
	m.exitDegraded()
```

Why it passes NP-1/NP-2: NP-1 wipes+recreates all four buckets; after the leader's version-monotonic republish satisfies the commitment guard, the assignment bucket's live Created differs from the captured value → `epochMismatchOutstanding` returns true → no Degraded→Stable edge → `require.Empty(healed)` holds. NP-2 recreates one bucket → that bucket's Created mismatches → terminal Degraded, `degradedToStable==0`. It does NOT interfere with NP-3b: there the kv-unavailable reason makes Family B short-circuit (return) before the A re-probe runs, so the A probe never executes against the faulted buckets.

- [ ] **Step 4: Ungate NP-2 and NP-1**

Delete the `PARTI_RUN_NP2_EPOCH_FLAP_PROOF` skip (`np2_..._test.go:53-55`) and the `PARTI_RUN_NP1_LIVE_RECREATE_PROOF` skip (`np1_..._test.go:134-137`). Leave the `testing.Short()` skips.

- [ ] **Step 5: Run NP-1/NP-2 + epoch-entry (F1) + C1 regressions (`-race`)**

```bash
go test -race ./test/integration/failure -run 'TestNP2' -count=1
go test -race ./test/integration/manager -run 'TestNP1_LiveBucketRecreate' -count=1
go test -race ./test/integration/manager -run 'TestManager_F1_(BucketRecreate_TripsDegraded|HappyPath_NoDegraded)' -count=1
go test -race . -run 'TestAttemptRecovery' -count=1
```

Expected: all PASS, no `-race` report. The F1 epoch-ENTRY tests must stay green — Family A touches only the recovery EXIT and adds a read-only probe helper; `captureBucketEpoch`/`checkBucketEpochs` are unchanged (`ep.created` stays permanently stale, which is load-bearing). The `AttemptRecovery_*` suite pins the commitment guard the new conjuncts are additive to.

- [ ] **Step 6: Commit**

```bash
make lint
git diff --check
git add manager_degraded.go test/integration/failure/np2_epoch_fence_return_to_stable_test.go test/integration/manager/np1_live_recreate_returns_stable_test.go
git commit -m "fix(degraded): refuse recovery exit while a bucket recreate is outstanding

A wiped-and-recreated Parti control-plane bucket leaves the epoch fence cached
mismatch permanent, but the recovery exit was blind to it and returned to Stable
on a version-monotonic republish into the recreated-empty bucket. Add a live
per-tick epoch re-probe to the recovery exit so a worker stays terminally
Degraded for restart/rotation; probe errors are skipped so the gate stays
independent of the kv-unavailable gate."
```

### Task 1.3: Family C mechanism 1 — document the claim-loss self-stop boundary

The claim-loss self-stop on an outage ≥ `WorkerIDTTL` is **intended** (the split-brain-safe shutdown); recovery is orchestrator rotation. This task documents the boundary and re-purposes the proof into a regression guard. **No production code path changes.**

**Files:**
- Modify: `docs/OPERATIONS.md`
- Modify: `test/integration/failure/np8_fleet_nats_outage_leader_continuity_test.go` (`TestNP8FleetNATSOutage_LeaderContinuityRecoversFleet`)

- [ ] **Step 1: Document the bounded-recovery boundary**

In `docs/OPERATIONS.md`, in the degraded/recovery section, add (corrected framing from report §1 C-mech1):

```markdown
**Recovery bound — worker-ID lease (`WorkerIDTTL`).** M5 recover-to-Stable across
a NATS outage is bounded by `WorkerIDTTL` (default 75s; the stableID bucket
`MaxAge` is reconciled to it). An outage that exceeds `WorkerIDTTL` ages out the
worker-ID lease; on reconnect the renewal sees a revision mismatch, surfaces a
bare `ErrClaimLost`, and the worker **self-stops to `StateShutdown`** — the
deliberate split-brain-safe behavior, since a peer may have taken the slot. The
boundary is minute-scale at defaults (renewal cadence ~`WorkerIDTTL/3`; purge at
last-renewal + `WorkerIDTTL`), so a ~1-minute blip can rotate the **whole fleet**
(every disconnected worker crosses the boundary together). Recovery is
orchestrator rotation (the readiness probe sees `StateShutdown` and the pod is
replaced); Parti does not auto-reclaim a lease that aged out, because at the
`ErrClaimLost` surface it cannot distinguish "my lease expired, slot empty" from
"a peer took the slot". Raise `WorkerIDTTL` above the worst-case outage to move
the boundary.
```

- [ ] **Step 2: Re-purpose the NP-8 mech-1 proof to assert `StateShutdown` + add instrumentation**

In `TestNP8FleetNATSOutage_LeaderContinuityRecoversFleet`, the current `WaitAllManagersState(... StateStable ...)` after restart (`:187-189`) encodes the wrong expectation. Replace it (and the subsequent leader/coverage/flap assertions that assume a live fleet) with the documented `StateShutdown` outcome, and add the Shutdown/OnError instrumentation report §1 recommends:

```go
	// --- POST-RESTART: outage exceeded WorkerIDTTL, so every worker's lease aged
	// out; on reconnect each hits a bare ErrClaimLost and self-stops. The
	// documented contract is StateShutdown + orchestrator rotation, NOT in-process
	// recovery to Stable. Assert the WAD self-stop so this proof guards the
	// boundary rather than a behavior we do not provide. (GetActiveWorkers filters
	// out Shutdown, so read the raw worker set via GetWorkers.)
	require.Eventually(t, func() bool {
		for _, mgr := range cluster.GetWorkers() {
			if mgr.State() != types.StateShutdown {
				return false
			}
		}
		return true
	}, 30*time.Second, 200*time.Millisecond,
		"every worker must self-stop to StateShutdown when the outage exceeds WorkerIDTTL")
```

Add an `OnError`/shutdown counter to the per-worker hooks so the attribution is captured rather than inferred (report §1 "Attribution is INFERENCE"): record each worker's terminal reason via the existing hook surface and `t.Logf` it. Remove the now-unreachable `recovered`/`postRecoveryRedegrade`/leader-continuity/coverage assertions for this test (they assumed a surviving fleet). Update the test's doc comment to state it now guards the WAD self-stop boundary.

- [ ] **Step 3: Ungate the (now-passing) mech-1 proof**

Delete the `PARTI_RUN_NP8_FLEET_OUTAGE_PROOF` skip (`:56-59`). Keep `testing.Short()`.

- [ ] **Step 4: Run + commit**

```bash
go test ./test/integration/failure -run 'TestNP8FleetNATSOutage_LeaderContinuityRecoversFleet' -count=1 -v
make lint
git diff --check
git add docs/OPERATIONS.md test/integration/failure/np8_fleet_nats_outage_leader_continuity_test.go
git commit -m "docs(degraded): document WorkerIDTTL recovery bound; assert claim-loss self-stop

An outage past WorkerIDTTL ages out the worker-ID lease and the worker self-stops
to Shutdown by design (split-brain safe). Document the bound and orchestrator-
rotation recovery, and re-purpose the fleet-outage proof to assert the Shutdown
outcome with terminal-reason instrumentation so it guards the boundary."
```

---

## Phase 2 — Family C mechanism 2 (decision-gated; BOTH branches specified)

Select the branch from **Task 0.1 / DR-1**. Phase 2 depends on Phase 0; Branch C additionally depends on Family A (Task 1.2) having landed. Exactly one of Task 2.1 / Task 2.2 is implemented; record which in this doc.

> **SELECTED: Branch D (Task 2.1) + the Branch-C reachability conjunct (Task 2.2 Step 2, C-hold contract) — a hybrid, not an either/or.** DR-1 resolved to **Branch D** (operator-selected; `hb_fs_iops` is a negligible flat term, Task 0.1). But the either/or framing assumed the migration *happens* — under the chosen **manual** migration an existing `MemoryStorage` heartbeat bucket persists indefinitely, so Branch D alone leaves exactly the un-migrated single-node-restart flap gap (the heartbeat-loss degrade reason is `"KV error threshold exceeded"`, which Family B does not cover and Family A's recreate-only re-probe does not catch for a *missing* stream — verified). The Branch-C reachability conjunct (`heartbeatBucketUnavailable`) is therefore the **complement**: it holds the un-migrated interim terminally Degraded (loud, rotatable; a rotation re-creates the bucket as `FileStorage`) for existing clusters, while Branch D fixes new clusters. C-recreate (the auto-heal variant that mutates `m.bucketEpochs`) was NOT taken — the lock-free `bucketEpochs` contract is preserved. Shipped in commits `9140af6` (Branch D) and `2964949` (reachability conjunct + un-migrated verify-first proof + doc).

The driver of the mech-2 flap is the **heartbeat PUBLISHER `Put`** against the dead `MemoryStorage` stream after a single-node restart (every worker, via `recordKVOpError`), NOT the calculator's "list heartbeat keys" log line (report §1 C-mech2, verified by elimination). So the report's old "calculator tolerates empty heartbeat list" idea is REJECTED — it fixes a non-driver.

### Task 2.1: Branch D — heartbeat bucket `MemoryStorage` → `FileStorage` (if DR-1 = Branch D)

One line; decoupled from Family A (no new stream `Created`, so it cannot re-arm the epoch fence); the bucket survives a single-node restart so neither the publisher `Put` nor the calculator list fails.

**Files:**
- Modify: `manager_setup.go:104-119,156`
- Modify: `docs/OPERATIONS.md`
- Test: `test/integration/failure/np8_fleet_nats_outage_leader_continuity_test.go` (`TestNP8FleetNATSOutage_HeartbeatBucketLossFlap`)

- [ ] **Step 1: Confirm the reproducer fails on the parent commit**

```bash
PARTI_RUN_NP8_HEARTBEAT_FLAP_PROOF=1 go test ./test/integration/failure \
  -run 'TestNP8FleetNATSOutage_HeartbeatBucketLossFlap' -count=1 -v
```

Expected: FAILS — either the reach-all-Stable wait times out or the HOLD check trips (`:325-328`), because the `MemoryStorage` heartbeat stream is gone after the restart and the fleet flaps.

- [ ] **Step 2: Switch the heartbeat bucket to FileStorage**

In `manager_setup.go`, the heartbeat `ensure` call (`:156`):

```go
	heartbeatKV, err = ensure("heartbeat", m.cfg.KVBuckets.HeartbeatBucket, m.cfg.HeartbeatTTL, jetstream.FileStorage)
```

And update the storage-choice doc comment (`:104-119`) so the rationale matches:

```go
//   - heartbeat:  FileStorage   — the heartbeat stream must survive a single-node
//     NATS restart. With MemoryStorage the stream is lost on restart and the
//     fleet flaps Degraded<->Stable (the publisher Put keeps failing against the
//     dead stream). The added write IOPS was measured within the provisioned PVC
//     envelope (see docs/plans/iops-investigation; 06-deep-gap-fix-plan Task 0.1).
```

- [ ] **Step 3: Add the migration runbook**

In `docs/OPERATIONS.md`, mirror the existing election-bucket migration note: an EXISTING `MemoryStorage` heartbeat bucket is NOT auto-converted (`EnsureKVBucketWithRetry` is get-first and honors the existing config, `manager_setup.go:122-124`). Document the operator step to delete+let-recreate (or re-provision) the heartbeat bucket as FileStorage during a maintenance window, and that until then the flap persists on the old bucket.

- [ ] **Step 4: Ungate NP-8 mech-2; run + the PartialBucketLoss regression**

Delete the `PARTI_RUN_NP8_HEARTBEAT_FLAP_PROOF` skip (`:269-272`). Then:

```bash
go test ./test/integration/failure -run 'TestNP8FleetNATSOutage_HeartbeatBucketLossFlap' -count=1 -v
go test ./test/integration/manager -run 'TestManager_PartialBucketLoss_HeartbeatHealthy|TestManager_LiveNATSBucketLoss' -count=1
```

Expected: mech-2 PASSES (the FileStorage heartbeat stream survives → reach AND hold all-Stable). The `PartialBucketLoss`/`LiveNATSBucketLoss` contracts must stay green — the storage class changes durability, not the loss-detection semantics; if `PartialBucketLoss` encodes a MemoryStorage assumption, update its setup to match (report §4 flags this as an `R` cell for Opt D).

- [ ] **Step 5: Commit**

```bash
make lint
git diff --check
git add manager_setup.go docs/OPERATIONS.md test/integration/failure/np8_fleet_nats_outage_leader_continuity_test.go
git commit -m "fix(degraded): store the heartbeat bucket on FileStorage to survive NATS restart

A single-node NATS restart dropped the MemoryStorage heartbeat stream, so the
heartbeat publisher Put kept failing against the dead stream and the fleet
flapped Degraded<->Stable. Persist the heartbeat bucket so the stream survives
the restart; the added IOPS was measured within the provisioned envelope. Adds
the get-first migration runbook for existing deployments."
```

### Task 2.2: Branch C — fail-safe-hold or recreate, on top of Phase-1 Family B (if DR-1 = Branch C)

**Key interaction (verify in Step 1):** once Phase 1's Family B gate has landed, mech-2's *flap* is likely **already closed** — the heartbeat `Put` fails against the dead `MemoryStorage` stream and degrades, and IF that degrade reason is `kv-unavailable` (report §1 C-mech2 says "likely … via `nats.ErrNoResponders` on a cached Put"; report §6 #5 flags this as an OPEN uncertainty), the Family B conjunct already refuses to exit (no heartbeat success since degrade) → the fleet holds **terminally Degraded** instead of flapping. So Branch C is NOT a new flap fix; it is the decision of what to do about the now-stuck-Degraded fleet, plus a reachability backstop for the case where the reason is `"KV error threshold exceeded"` (Family B does not cover that reason). Implement only if DR-1 selected Branch C; Steps 3/4 depend on Family A (Task 1.2).

**Files:**
- Modify: `test/integration/failure/np8_fleet_nats_outage_leader_continuity_test.go` (`TestNP8FleetNATSOutage_HeartbeatBucketLossFlap`)
- Modify (only if Step 1 finds reason ≠ `kv-unavailable`): `manager_degraded.go` (reachability conjunct)
- Modify (C-recreate only): the reconnect path (recreate heartbeat bucket + re-capture epoch atomically)

- [ ] **Step 1 (DR-2): determine the mech-2 degrade reason**

Add a reason-capturing `OnDegraded` hook to `TestNP8FleetNATSOutage_HeartbeatBucketLossFlap` (it currently passes `&parti.Hooks{}`, `:293`) that records each reason into an `atomic.Value`-backed `[]string`, run it with Phase 1 landed, and record:

```
DR-2 mech-2 reason = [kv-unavailable | "KV error threshold exceeded"]
```

```bash
PARTI_RUN_NP8_HEARTBEAT_FLAP_PROOF=1 go test ./test/integration/failure \
  -run 'TestNP8FleetNATSOutage_HeartbeatBucketLossFlap' -count=1 -v
```

If `kv-unavailable`: Family B already holds the fleet Degraded — SKIP Step 2 (no new conjunct). If `"KV error threshold exceeded"`: do Step 2 (Family B does not apply to that reason, so without it the fleet still flaps).

- [ ] **Step 2 (only if DR-2 ≠ `kv-unavailable`): add a heartbeat-reachability exit conjunct**

Add to `manager_degraded.go` and wire it into the exit block (additive, after the Family A conjunct). It is NOT reason-scoped to `kv-unavailable` (that is the whole point — it backstops the `"KV error threshold exceeded"` reason), but it fires only when the heartbeat bucket is genuinely unreachable, so it cannot regress NP-5 (no heartbeat dependency there → bucket reachable → returns false) or NP-3a (fault cleared → bucket reachable):

```go
// heartbeatBucketUnavailable reports whether the heartbeat bucket's stream is
// currently missing/unreachable (a fresh-handle Status probe; fresh handle for
// the same goroutine-safety reason as epochMismatchOutstanding). Used as a
// Branch-C recovery-exit backstop so a worker does not exit to Stable while its
// heartbeat bucket is gone after a single-node restart.
func (m *Manager) heartbeatBucketUnavailable(ctx context.Context) bool {
	if m.js == nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, m.cfg.OperationTimeout)
	defer cancel()
	kv, err := m.js.KeyValue(probeCtx, m.cfg.KVBuckets.HeartbeatBucket)
	if err != nil {
		return true // bucket missing / unreachable
	}
	if _, err := kv.Status(probeCtx); err != nil {
		return true
	}
	return false
}
```

Conjunct (after Family A, before `m.exitDegraded()`):

```go
	// Branch C backstop — refuse to exit while the heartbeat bucket is gone
	// after a single-node restart, for the reason ("KV error threshold exceeded")
	// the Family B kv-unavailable gate does not cover.
	if m.heartbeatBucketUnavailable(m.ctx) {
		m.logger.Debug("recovery: heartbeat bucket unavailable; staying Degraded")
		return
	}
```

- [ ] **Step 3: Choose the contract — C-hold is RECOMMENDED**

Record ONE in this doc:
- **C-hold (fail-safe, no recreate) — RECOMMENDED:** the fleet stays terminally Degraded on heartbeat-bucket loss; recovery is re-provision/rotation. **No new production code mutates `m.bucketEpochs`**, so the Phase-1 lock-free-read contract is preserved. Family B (kv-unavailable reason) and/or Step 2 (other reason) already hold it Degraded. Document in `docs/OPERATIONS.md`: "MemoryStorage heartbeat-bucket loss after a single-node restart is terminal Degraded → re-provision the bucket / rotate." Then in Step 4 rewrite the mech-2 proof from reach-then-hold-**Stable** (`:325-328`) to **hold-Degraded**.
- **C-recreate (auto-heal):** recreate the heartbeat bucket on reconnect so the heartbeat `Put` succeeds again, the gate opens, and the fleet heals — keeping the existing reach-then-hold-Stable proof. Depends on Task 1.2 AND **breaks the Phase-1 lock-free `m.bucketEpochs` contract**: re-capturing the cached epoch is a post-Start WRITE while `checkBucketEpochs` and `epochMismatchOutstanding` range the map lock-free. Holding `m.mu` is NOT sufficient — those readers do not take `m.mu`. So C-recreate additionally requires **introducing a dedicated `m.bucketEpochsMu sync.RWMutex` and converting every reader and post-Start writer to use it** (see code below). Choose C-recreate only if auto-heal of a lost MemoryStorage bucket is a hard requirement and the IOPS budget truly rules out Branch D.

C-recreate concurrency change (REQUIRED if C-recreate is chosen):

```go
// Add to the Manager struct (near bucketEpochs):
//   bucketEpochsMu sync.RWMutex // guards bucketEpochs once it is mutated post-Start (C-recreate)
//
// Convert the two existing lock-free readers to take RLock:
//   - checkBucketEpochs (manager_setup.go:669): wrap the `for bucket, ep := range m.bucketEpochs`
//     in m.bucketEpochsMu.RLock()/RUnlock() (snapshot the entries under RLock, probe outside it
//     to avoid holding the lock across network I/O).
//   - epochMismatchOutstanding (Task 1.2): same — snapshot {bucket: ep.created} under RLock, then
//     probe each outside the lock.
// captureBucketEpoch's Start-time write also takes the Lock (cheap; Start is single-goroutine).

// recreateHeartbeatBucketIfMissing recreates the MemoryStorage heartbeat bucket
// on reconnect when its stream was lost, and re-captures its epoch so the live
// re-probe / epoch fence do not fire bucket-recreated:heartbeat against the new
// stream. The bucketEpochs write is serialized by m.bucketEpochsMu (NOT m.mu —
// the epoch readers do not take m.mu).
func (m *Manager) recreateHeartbeatBucketIfMissing(ctx context.Context) error {
	bucket := m.cfg.KVBuckets.HeartbeatBucket
	if !m.heartbeatBucketUnavailable(ctx) {
		return nil
	}
	kv, err := m.js.CreateKeyValue(ctx, BuildControlPlaneKVConfig(bucket, m.cfg.HeartbeatTTL, jetstream.MemoryStorage))
	if err != nil {
		return fmt.Errorf("recreate heartbeat bucket: %w", err)
	}
	created, err := kvutil.BucketStreamCreated(ctx, kv) // read the new Created outside the lock
	if err != nil {
		return fmt.Errorf("re-capture heartbeat epoch: %w", err)
	}
	m.bucketEpochsMu.Lock()
	m.bucketEpochs[bucket] = bucketEpoch{kv: kv, created: created}
	m.bucketEpochsMu.Unlock()
	return nil
}
```

Call it from the reconnect path (`checkConnectionHealth`'s connection-restored branch, `manager_degraded.go:122-126`), leader-gated if only the leader should recreate. Note: `captureBucketEpoch` itself is NOT reused here (it opens a fresh handle internally and assumes Start-time single-goroutine access); the inline re-capture above takes the lock explicitly.

- [ ] **Step 4: Update the proof to the chosen contract; ungate; run; commit**

Delete the `PARTI_RUN_NP8_HEARTBEAT_FLAP_PROOF` skip (`:269-272`). Set the proof expectation to match the chosen contract: **C-hold** → assert the fleet reaches and HOLDS all-Degraded (not Stable); **C-recreate** → keep the existing reach-then-hold-Stable assertions (`:325-328`). Run `-race`; run the `LiveNATSBucketLoss`/`PartialBucketLoss` regressions and the F1 epoch-entry tests (C-recreate touches the epoch cache). Then:

```bash
make lint
git diff --check
# git add the touched files; commit with a conventional message describing the
# chosen contract, e.g. for C-hold:
#   "docs(degraded): MemoryStorage heartbeat loss is terminal Degraded; assert hold"
# or for C-recreate:
#   "fix(degraded): recreate the heartbeat bucket on reconnect with atomic epoch re-capture"
```

---

## NP-10 deferred to `07` — why

The investigation report (§5) framed NP-10's fix as "route sustained enumeration failure into the manager's degraded circuit with the same `markKVUnavailable`/`recordKVOpError` semantics." Drafting this plan surfaced two reasons that route is **not** implementation-ready as a tail task on the A/B/C core, so NP-10 is carved out to its own proof-first plan (`07-np10-enumeration-stall-plan.md`):

1. **The F-D1 transient-clear defeats the naive route.** NP-10's defining asymmetry is that the single-key heartbeat `Put` keeps **succeeding** while the stream-wide `Keys` scan times out. A successful heartbeat `Put` fires `recordKVHealthyOp`, which clears the transient (`ErrKVUnavailable`-class) entries from `kvErrorWindow` (`manager_degraded.go:266-288`). Routing the scan deadline through `recordKVOpError` adds a transient entry that the very next heartbeat success clears — so the enumeration failures never accumulate to `KVErrorThreshold`. The fix needs a path that does **not** depend on the transient window (a calculator-local consecutive-failure threshold, or a non-transient classification).

2. **It needs its own reason-scoped exit gate.** Even if NP-10 degrades, `attemptRecoveryFromDegraded` would exit on the healthy assignment read while the enumeration is still stalling — the same recover-on-wrong-signal defect as Family B, but for a new reason. Closing it requires a *third* additive exit conjunct plus a calculator→manager "enumeration recovered" success signal (mirroring the heartbeat-success stamp). That is a new seam (`assignment.Config` has no error/success callback today, `internal/assignment/config.go`) and a new predicate, not a tail.

NP-10 is also Medium-confidence / not-harness-proven (report §8). `07` therefore leads with building the reproducer and confirming it fails (verify-first) before any fix — if the gap cannot be reproduced, `07` stops.

---

## Final Verification

- [ ] **Step 1: Format affected Go packages**

```bash
make fmt
```

- [ ] **Step 2: Focused checks across the touched surface**

```bash
go test . -run 'Test(AttemptRecovery|MarkKVUnavailable|RecordKVError_ReadUnavailable_Degrades|NP5_BlockedApply)' -count=1
go test ./test/integration/manager -run 'TestManager_(F1_BucketRecreate_TripsDegraded|F1_HappyPath_NoDegraded|LiveNATSBucketLoss|KVUnavailable_EntersDegraded|PartialBucketLoss_HeartbeatHealthy)|TestNP1_LiveBucketRecreate|TestNP3_KVUnavailable' -count=1
go test ./test/integration/failure -run 'TestNP2|TestNP8FleetNATSOutage' -count=1
```

Expected: PASS (the previously-gated NP-1/NP-2/NP-3b proofs now run by default and pass; NP-8 mech-1 asserts Shutdown; NP-8 mech-2 per the chosen Phase-2 branch).

- [ ] **Step 3: `-race` re-baseline (the delta vs Task 0.3)**

```bash
go test -race ./test/integration/manager ./test/integration/failure \
  -run 'TestNP1_LiveBucketRecreate|TestNP2|TestNP3_KVUnavailable|TestNP8FleetNATSOutage' -count=1
```

Expected: all PASS, no new `-race` report vs the Task 0.3 baseline.

- [ ] **Step 4: Lint + pre-PR gate**

```bash
make lint
git diff --check
make pre-pr
```
