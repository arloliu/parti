# Degraded-Recovery Self-Heal — Follow-Up Plan (F-D3 tail)

- **Date:** 2026-05-30
- **Status:** DRAFT (plan-review loop not yet run).
- **Builds on:** `docs/plans/auto-healing-quorum-loss-fix/00-fix-plan.md` §3
  "Deferred follow-up". This plan scopes the single deferred item from that fix
  series: the sustained-write-fault Degraded-recovery path can report `Stable`
  while claims are still uncommitted, and a version advance during the Degraded
  window leaves a narrow non-heal (restart-only) tail.
- **Origin:** PR4 (F-D3) post-implementation review v1, finding **P1-1**
  (`tmp/00-fix-plan_PR4-F-D3_post_implementation_review_v1.md`). Deferred from
  PR4 because the clean fix touches the **generic recovery path shared by all
  degraded reasons** (cross-feature contracts 1 and 3) and warrants its own
  contract-regression pass rather than being bundled into PR4.
- **Relationship to the shipped series:** F-D2a/F-D2c/F-D1/F-D3 are all merged to
  `main` (PRs #27/#28/#29). F-D2b was dropped as dead code. This is the last
  open engineering item from the quorum-loss fix series; the remaining open
  items in 00-fix-plan §7 are tuning/decision questions, not code.

---

## 1. The defect (precise statement)

A worker that **(re)starts during a sustained claim-write fault** — assignment KV
**reads** still succeed, claim **writes** fail — reaches a state where:

- `waitForAssignment` pre-advances the in-memory snapshot to the full partition
  set (`manager_election.go`), so `CurrentAssignment()` is **non-empty**;
- every bootstrap apply fails its claim write → `scheduleApplyRetry` keeps
  retrying → `initialClaimsCommitted` is **never latched** (it latches only on a
  successful empty-prev → non-empty-next apply, `manager_assignment.go:1305`);
- the startup-timeout watchdog correctly fires
  `enterDegraded("startup-timeout")`.

So far so good — the worker is Degraded with no claims written, which is correct.
The bug is in **recovery**. On the connection-monitor goroutine, once the
connection has been up past `ExitThreshold`, `checkConnectionHealth` calls
`attemptRecoveryFromDegraded` (`manager_degraded.go:115`). That function:

1. calls `refreshAssignmentFromNATS()` — a **read** via `monotonicStore`, NOT an
   apply (`manager_assignment.go:1503`), which **succeeds** (reads are healthy);
2. unconditionally calls `exitDegraded()` → `StateStable`
   (`manager_degraded.go:323-331`).

**Result:** the worker reports `StateStable` with **zero claims written**. Two
residual edges (both named in 00-fix-plan §3, both pre-dating F-D3):

- **Edge (a) — misleading `Stable`-while-uncommitted window.** The worker looks
  healthy to operators/hooks while it owns no claims and processes nothing.
- **Edge (b) — narrow non-heal tail (restart-only).** If a version advance lands
  during the Degraded window, `refreshAssignmentFromNATS` monotonic-stores a
  version (V2) strictly higher than the pending retry's coalesced version (V1).
  The retry re-reads `prev := m.CurrentAssignment()` at apply time and
  stale-gate-drops (`isApplyResultStale`, `manager_assignment.go:1161`), then the
  retry loop self-exits. Claims are never written until a process restart.

**Why this is still strictly better than pre-F-D3.** Before F-D3 the worker
reached `Stable`-with-no-claims *immediately* via the empty-diff "success" and
never self-healed. With F-D3 the retry stays alive (latch false) and self-heals
once writes recover — except for edge (b). This plan closes both edges so the
worker actively self-heals in all cases without a restart.

---

## 2. Invariants this fix MUST NOT regress (load-bearing)

`attemptRecoveryFromDegraded → exitDegraded` is the **generic recovery path
shared by every degraded reason** (`"NATS connection down"`,
`"KV error threshold exceeded"`, `"kv-unavailable"`, `"startup-timeout"`,
`"stream-missing-recovery-exhausted"`). The guard MUST NOT change recovery for
any worker that has already committed its claims.

**The guard keys on STATE, not on the degraded reason — and that is
deliberate.** The condition `!initialClaimsCommitted && len(CurrentAssignment().
Partitions) > 0` means precisely *"this worker holds an assignment but has never
once committed claims for it."* `initialClaimsCommitted` is a one-way latch set
only on a successful empty-prev → non-empty-next claim write
(`manager_assignment.go:1305`), so any worker that ever reached Stable-with-claims
has it permanently `true` and is immune. In the un-latched state, calling
`exitDegraded → Stable` is the Stable-without-claims bug **regardless of which
reason degraded the worker** — a NATS-down-during-startup or
kv-unavailable-during-startup worker that recovers reads-first hits the exact same
defect as the startup-timeout path. Therefore the guard *should* cover all of
them. **A reviewer suggestion to narrow the guard to
`degradedReason == "startup-timeout"` is explicitly rejected:** the degraded
reason is not persisted today (`enterDegraded` only logs/hooks it,
`manager_degraded.go:240-277`), and narrowing would *reintroduce* the bug for the
NATS-down/kv-unavailable startup variants. This is approach C from §4, rejected
for adding state and *less* coverage. The state guard is strictly the right
predicate; §1's "startup-bootstrap case" is the dominant trigger, not the only
in-scope one.

1. **Cross-feature contract 1** (whole-bucket-missing → every worker
   `StateDegraded`, recovers when the bucket returns). Unaffected by construction:
   under whole-bucket loss `refreshAssignmentFromNATS` **fails** (the read
   errors), so `attemptRecoveryFromDegraded` returns at the existing err-check
   **before** reaching the new guard. The kv-error circuit is untouched.
2. **Cross-feature contract 3** (OnDegraded fires exactly once per Degraded
   entry per worker). Held **by construction**: the fix *stays* degraded
   (returns without `exitDegraded`), so `degradedSince` stays non-zero and
   `enterDegraded`'s CAS (`manager_degraded.go:249`) blocks any OnDegraded
   re-fire across the held recovery ticks. This is a positive property of the
   stay-degraded design vs any exit-then-re-enter alternative. **Asserted by a
   test** (§5), not merely assumed.
3. **Cross-feature contract 2** (peer-claim-takeover → only that worker enters
   claim-lost shutdown). Untouched — this fix is in the degraded-recovery path,
   not the claimer-error path.
4. **Steady-state recovery must not change.** A worker that has committed claims
   (`initialClaimsCommitted == true`) recovers exactly as today: the guard is
   skipped. Pinned by a both-directions negative-space test (§5).
5. **Monotonic snapshot semantics.** The re-armed apply uses the **current**
   (post-refresh) assignment, so it passes the stale gate
   (`isApplyResultStale(cur, cur) == false`) without weakening the gate for any
   other path.
6. **Mixed-version / rolling upgrade — safe.** During a rolling upgrade an
   old-version worker (no guard) and a new-version worker (with guard) coexist.
   The fix adds only an extra **trigger** for *one worker's own*
   `scheduleApplyRetry`; it does NOT change which claim keys get written.
   `handoffCoordinator.Apply(ctx, workerID, old, new)` writes claims **only for
   the calling worker's own assignment** (`manager_assignment.go:1270`), and
   `scheduleApplyRetry` already exists on the pre-fix apply path — so the *set* of
   `(worker, partition)` claim-key writes the cluster performs is unchanged; the
   fix only re-issues a worker's own already-possible retry. The per-claim CAS
   (`UpdateClaim` Create/Update) arbitrates any concurrent writer **version-
   agnostically** (the loser CAS-fails), so an old worker and a new worker racing
   the same key is resolved identically with or without this fix. This fix
   introduces no wire-format, schema, or claim-key-layout change, so version-
   agnostic CAS arbitration is exactly the pre-fix behavior (the argument assumes
   the upgrade does not itself restructure claim keys — a separate concern any
   key-layout migration would own, orthogonal to this fix). The fix is therefore
   safe in a mixed-version cluster and requires no same-version gating.

---

## 3. The fix (approach B — gate the exit + re-arm a bootstrap apply)

Single site: `attemptRecoveryFromDegraded` in `manager_degraded.go`. After the
existing `refreshAssignmentFromNATS()` success check, before `exitDegraded`:

```go
func (m *Manager) attemptRecoveryFromDegraded() {
    if m.degradedSince.Load() == 0 {
        return
    }

    if err := m.refreshAssignmentFromNATS(); err != nil {
        m.logger.Warn("failed to refresh assignment during recovery", "error", err)
        m.recordKVError(err)
        return
    }

    // The refresh succeeded (assignment reads are healthy). Record that
    // success regardless of which branch we take below — the read just
    // proved the KV-error window is stale.
    m.recordKVSuccess()

    // F-D3 follow-up: a worker that started during a sustained claim-write
    // fault has a non-empty assignment (waitForAssignment pre-advanced it) but
    // never latched initialClaimsCommitted — no claims were ever written.
    // Refresh-only recovery would exitDegraded → Stable with zero claims.
    // Instead stay degraded and re-arm a bootstrap apply for the CURRENT
    // (post-refresh) assignment so the worker actively self-heals once writes
    // recover. cur is read AFTER the refresh so it captures any version advance
    // that landed during the degraded window (fixes edge (b)).
    cur := m.CurrentAssignment()
    if !m.initialClaimsCommitted.Load() && len(cur.Partitions) > 0 {
        m.scheduleApplyRetry(cur)
        return // do NOT exitDegraded — wait until a real apply latches the flag
    }

    m.exitDegraded()
}
```

### 3.1 Why each load-bearing detail is correct

- **`cur` captured after the refresh.** The refresh is what advances the snapshot
  to the higher version. Reading `cur` *before* the refresh would re-arm the stale
  lower version, which is exactly the version edge (b) stale-drops — silently
  leaving (b) unfixed. This ordering is the entire fix for edge (b).
- **Re-arm with `cur`, not a pinned version.** `scheduleApplyRetry` coalesces to
  the highest pending version and re-reads `prev := m.CurrentAssignment()` at
  apply time. Because `cur` is the current snapshot, `isApplyResultStale(cur, cur)`
  is `false` → the apply passes the stale gate. With `initialClaimsCommitted ==
  false`, the bootstrap override in `applyAssignmentWithPrevCore`
  (`manager_assignment.go:1264`) forces empty-prev → the prepare diff is the FULL
  partition set → all claims written once the write fault clears.
- **Level-triggered convergence.** `attemptRecoveryFromDegraded` runs every
  recovery tick (~1s, bounded by `ExitThreshold`). Each tick re-reads the
  then-current version and re-arms. If a burst of version advances (V2, V3, …)
  lands during the outage, each tick targets the latest; the worker converges to
  self-heal once advances quiesce and writes recover. A V_{n+1} advance landing
  *between* a re-arm and its apply will stale-drop that one retry, but the **next**
  tick re-arms with V_{n+1} — so the level-triggered loop is self-correcting. (The
  edge-(b) test MUST advance the version *during* the degraded window, not just
  once before, to exercise this.)
- **Latch flips → exit on the next tick.** Once the re-armed apply succeeds it
  latches `initialClaimsCommitted = true` (the existing latch at
  `manager_assignment.go:1305`). The *next* recovery tick then skips the guard and
  `exitDegraded`s normally → `StateStable` with claims actually present.

### 3.2 Decisions pinned

- **`recordKVSuccess()` is called on both branches** (moved above the guard). The
  refresh succeeded, so the KV-error window the read just exercised is stale;
  recording success is correct and not branch-dependent. Harmless on the held
  path since the circuit short-circuits while degraded. *(Deliberate — surfaced in
  review of the draft so it is not left accidental.)*
- **Stay-degraded, not exit-then-re-enter.** Staying degraded is what gives the
  free contract-3 guarantee (§2.2). No reason-tracking field is added.
- **No new degraded-reason persistence.** The reviewer offered reason-tracking
  (`startup-timeout`-only gating) as an alternative. Rejected (and see §2's
  state-vs-reason rationale): the un-latched state — assignment held, claims never
  committed — is the precise condition where `exitDegraded → Stable` is wrong, no
  matter which reason degraded the worker. Gating on `startup-timeout` would add a
  manager field *and* reintroduce the bug for the NATS-down/kv-unavailable startup
  variants. The state guard is the correct predicate; the reason is irrelevant to
  whether exiting is safe.

---

## 4. Why approach B over the alternatives (record)

- **Approach A (gate the exit only, `return` without re-arming):** closes edge (a)
  but leaves edge (b) restart-only — recovery would depend on the original
  `scheduleApplyRetry` loop still being alive and not having been stale-dropped.
  Rejected: it codifies a known non-heal tail.
- **Approach C (persist degraded reason, gate only `startup-timeout`):** most
  surgical in intent but most invasive in code (new manager field, every
  `enterDegraded` call site writes it). Rejected: the §3 guard is provably
  equivalent without the field.
- **Approach B (gate + re-arm):** the "clean fix" 00-fix-plan §3 describes
  ("self-heals once writes recover"). Self-scoping guard, contracts 1/3 untouched,
  cost over A is a few lines + one concurrency test. **Chosen.**

---

## 5. Test & contract-regression plan (first-class deliverable)

This is the reason the item was deferred from PR4. The test surface is the center
of this work, not an appendix.

### 5.1 RED-on-parent reproducers (both edges)

Extend the existing write-axis fault seam in
`test/integration/failure/startup_writefault_test.go` (the `wfFaultController` /
`wfFaultKeyValue` harness already faults ONLY the per-claim write path and can be
armed/disarmed mid-run).

- **(a) sustained-fault, no version advance** — the PR4 v1 reviewer's exact
  recipe:
  1. Arm the write fault before the worker starts.
  2. Hold it **past `StartupTimeout + DegradedBehavior.ExitThreshold`**.
  3. Assert the worker reaches `StateDegraded` (watchdog).
  4. Assert it does **NOT** transition to `StateStable` while
     `initialClaimsCommitted == false` and claims are absent in KV. *(RED on
     parent: parent reaches `Stable` via refresh-only recovery.)*
  5. Disarm writes; assert a bootstrap apply writes the **full** claim set and the
     worker reaches `StateStable` — **no restart**.
- **(b) version advance during the degraded window** — exercises the
  after-refresh `cur` capture:
  1. Arm the write fault; start the worker; let it reach `StateDegraded`.
  2. While degraded, publish a **version advance** (V1→V2) via the watchable
     source (cold-start/rejoin rebalance shape).
  3. Disarm writes; assert the worker self-heals by writing the **full claim set
     at V2** with **no restart**. *(RED on parent: the V1 retry stale-drops on the
     V2 advance and self-exits → restart-only.)*
  - Non-vacuity: confirm the V2 advance genuinely landed (snapshot version moved)
    before disarming, so the test proves the re-arm targets V2 and not a vacuous
    pass.

### 5.2 Concurrency `-race` stress test (AGENTS.md monitor-goroutine requirement)

`attemptRecoveryFromDegraded` runs on the **connection-monitor goroutine**, and
this fix adds an **apply-issuing side effect** (`scheduleApplyRetry`) to it. Per
AGENTS.md "Concurrency stress tests for monitor goroutines", add a focused
`-race` stress test in `test/integration/manager/` (template:
`manager_epoch_monitor_concurrency_test.go`) that drives the recovery goroutine ↔
`scheduleApplyRetry` ↔ commit-watcher interplay concurrently against the same
handoff bucket for ~5s and asserts no race-detector trips. The per-claim CAS
(`UpdateClaim` Create/Update) already makes a racing double-write safe (loser
CAS-fails); this test pins that the **re-arm side effect** introduces no new race
on the shared snapshot / `applyStoreMu` / `stashedApplyRetry` state.

### 5.3 Contract-3 assertion (held by construction → pinned)

Assert `OnDegraded` fires **exactly once** across the whole held recovery window
(multiple recovery ticks while the guard holds the worker degraded), reusing the
hook-count style of `TestManager_LiveNATSBucketLoss_OnDegradedHook`.

### 5.4 Negative-space (both-directions-of-boundary discipline)

A **steady-state** worker (`initialClaimsCommitted == true`) that enters Degraded
for an *unrelated* reason and whose refresh succeeds must `exitDegraded` →
`Stable` **exactly as today** — the guard must not hold it. (Per
`feedback_test_both_directions_of_boundary`: the positive test alone is consistent
with both a correct guard and an always-hold bug; this negative test discriminates.)

### 5.5 Mandatory gate (per AGENTS.md pre-PR + cross-feature contracts)

- The **3 cross-feature contract regression tests**:
  `TestManager_LiveNATSBucketLoss`, `TestManager_LiveNATSBucketLoss_OnDegradedHook`,
  `TestStableID_StaleKeyTakeover_Reclaim`.
- `make test-integration -race` (this is a recovery-path change on a shared
  monitor goroutine — the unit suite cannot reproduce the goroutine race).
- `make pre-pr` (touches `manager/`).
- Unit coverage for `attemptRecoveryFromDegraded` branch selection with fakes:
  {latch false + non-empty → re-arm + stays degraded}, {latch true → exits},
  {empty assignment → exits}, {refresh fails → returns before guard, records
  KV error}.

---

## 6. Phasing

Single PR (one localized site + its test surface). Per the repo's standard loop:
implement (verify-first: both RED-on-parent reproducers compile and fail on the
parent before the fix) → `/simplify` → review gate (`/codex:review`, fall back to
`/post-impl-review` for spec-compliance) → fix-loop to merge-clean → squash on
merge. `make pre-pr` + the 3 contract tests + `make test-integration -race` on the
final tree.

**Model/effort:** Opus, high. The change is small and the spec is airtight, but it
sits on the contract-pinned shared recovery path; rigor goes on the
contract-regression gate and the `-race` stress test, not on the 9-line diff.

---

## 7. Out of scope

- F-D3 option 3b (don't pre-advance the snapshot in `waitForAssignment`) — a
  larger startup-ordering change; 3a + this follow-up close the defect without it.
  Remains deferred (00-fix-plan §3).
- The F-D1 flapping-tuning decision and the pull-gating fail-open decision
  (00-fix-plan §7) — tuning/policy questions, not this code path.
- F2 (read-only-filesystem) incident variant (00-fix-plan §6).
