# Auto-Healing Consolidation — Investigation & Consolidation Plan

**Status:** DRAFT (investigation complete; execution not started)
**Baseline:** `main` @ v2.6.0
**Goal:** Consolidate the self-healing / degraded-recovery subsystem — which grew one
fault-family at a time across v2.4.0→v2.6.0 — into clean, maintainable code, **without
changing behavior**.

## Framing

A 7-reader reconnaissance over the auto-healing surface found that the *logic* is correct
and unusually well-tested (the self-heal/recovery suites are dense, the concurrency
invariants are carefully reasoned). The problem is **accidental structural complexity**:
state scattered as loose atomics with hand-enforced ordering, decisions smeared across
functions and files, the same shapes copy-pasted, and a recovery-exit gate that grows by
one inline `if` per fault family. This is a *refactor*, not a redesign.

### Guiding principles

1. **Behavior-preserving by default.** The handful of items that change behavior or public
   API are quarantined in Phase 4 and require explicit sign-off; they are never bundled into
   a "cleanup" commit.
2. **Characterization-tests-FIRST.** A behavior-preserving refactor is only safe where the
   behavior is locked. Phase 0 closes the coverage gaps the recon found *before* any
   production change. No refactor proceeds against thin-coverage code.
3. **Preserve the load-bearing invariants** (see below). Every comment in
   `manager_degraded.go` encodes a hard-won concurrency/contract decision.
4. **One consolidation per commit**, each run through the standard gate (lint + `-race` unit
   + `-race` integration) and a cross-model review, matching the workflow that shipped the
   recovery fixes.

### Load-bearing invariants that MUST survive (the traps)

These are the ways a well-intentioned "simplification" silently breaks correctness. Each is
called out at the relevant step.

- **T1 — Recovery-gate dual nature.** `attemptRecoveryFromDegraded` mixes two structurally
  different guard types: (a) **reason-scoped stamped-signal** gates (only 2 of 9 reasons:
  `kv-unavailable`→heartbeat-stamp, `heartbeat-enumeration-stall`→enumeration-stamp), and
  (b) **global live-reprobe backstops** (epoch mismatch, heartbeat-bucket reachability) that
  are *deliberately not* reason-scoped — they provide cross-reason defense-in-depth. A naive
  "each reason declares its own predicate" registry that attaches the global probes to
  specific reasons would **silently drop** that defense (e.g. a `kv-unavailable` degrade that
  *also* has a recreated bucket is currently caught by the global epoch conjunct). Only the 2
  stamped-signal gates become table-driven; the globals stay a fixed ordered list. The
  empty-reason early-return must stay *before* any reason comparison.
- **T2 — Infinite-retry contract.** The commit watcher and worker watcher restart **forever**
  by design; the assignment watcher is **bounded** (6 attempts → degrade → exit). `retry.Envelope`
  *panics* on `MaxAttempts<=0` and cannot express infinite retry. Any restart-loop unification
  must preserve infinite semantics (e.g. `MaxAttempts=0`), and the existing tests are
  positive-space only (they prove *one* restart) — so a harness that accidentally imposed a
  budget would stay green. Needs a negative-space characterization test.
- **T3 — Two shrink counters, never one.** Worker-shrink (F10-A) and partition-shrink (F6-B)
  share an algebra but must remain **two independent instances** (test T7 explicitly forbids a
  shared confirmation window). The worker counter is `c.mu`-guarded (two call paths converge);
  the partition counter is deliberately lock-free (one path). A shared *kernel value type* must
  not embed a lock.
- **T4 — Reason-ownership protocol is already correct — do not touch its logic.**
  `lastDegradedReason` is stored *after* the `degradedSince` CAS and cleared *before* it; the
  empty-reason recovery guard closes the post-CAS-pre-store window. This is pinned by a
  deterministic clobber test + a `-race` storm. Encapsulating it in a type is fine; changing
  the ordering is not.
- **T5 — Whole-bucket-loss is the ONLY path to the `KV error threshold exceeded` reason**
  (the AGENTS.md contract). Transient (`kv-unavailable`) entries are clearable by a healthy op;
  whole-bucket entries accumulate. Any classification extraction must preserve this exactly.

---

## Complexity map (synthesized findings)

Churn confirms where the ROI is (`git log v2.4.0..v2.6.0`, non-test, healing-related):
`manager.go` 20 · `manager_assignment.go` 17 · `manager_setup.go` 14 · `source/nats_kv.go` 10 ·
`internal/assignment/calculator.go` 9 · `manager_degraded.go` 7 · `manager_election.go` 5.
`manager_degraded.go`'s 7 commits are the live accretion point — each added one recovery-exit
conjunct.

| # | Cluster | Severity | Flagged by | Root cause |
|---|---------|----------|-----------|------------|
| C1 | **Degrade-reason taxonomy sprawl** — 9 reasons, 3 const + 6 inline literals across 5 files, re-typed in tests; recovery gates branch on the strings | HIGH | degraded, recovery, lifecycle, churn | No single reason registry |
| C2 | **Error-classification fragmentation** — `IsConnectivityError‖IsDegradingJetStreamError` union triplicated; classify→(drive,transient,reason) smeared across `markKVUnavailable`+`recordKVError`+`onClaimerError`; predicates split across 4 packages, `errors.Is` vs `strings.Contains` | HIGH | degraded | No single error taxonomy |
| C3 | **Recovery-exit gate is a 100-line inline conjunct cascade** — heterogeneous guards, ordering load-bearing but prose-only, grows by one `if` per fault family | HIGH | degraded, recovery, lifecycle, churn | No guard abstraction (T1) |
| C4 | **Scattered degraded-state** — ~12 loose atomics on `Manager` with hand-enforced ordering; no `IsDegraded()`/`DegradedReason()` accessor (raw `.Load()!=0` in 6+ sites) | HIGH | lifecycle, churn | No cohesive state type (T4) |
| C5 | **Duplicated bucket-probe loops** — `epochMismatchOutstanding` + `checkBucketEpochs` + `heartbeatBucketUnavailable` repeat range/fresh-handle/timeout/`BucketStreamCreated`/`.Equal` with verbatim rationale comments | HIGH/med | degraded, recovery, lifecycle, churn | No shared probe helper |
| C6 | **Monitor/watcher duplication** — 3 watch-restart loops w/ divergent escalation; 2 near-identical select-loop sessions differing only in debounce; jitter one-liner ×6+; watcher consts duplicated across packages; 3 shutdown conventions | HIGH | monitors | No session/restart harness (T2) |
| C7 | **Calculator stacked defenses** — `getActiveWorkers` 70-line funnel mixing 4 concerns; cache-fallback block written twice; F10-A/F6-B shrink windows duplicated; `observeAndDecide` mixed manual/deferred unlock | HIGH | calculator | Layers stacked inline (T3) |
| C8 | **Test fault-seam duplication** — 6 copy-pasted KV-wrapper seams (ku/np9/np10/rf/wf/sim, ~400+ lines); per-op selection by method-override not data; inconsistent controller APIs; 3 bespoke env-gates; dangling `tmp/parti-repro` citations | HIGH | tests | No shared exported fault toolkit |
| C9 | **Dead / superseded code** — write-only `lastAssignment`/`lastAssignmentAt` cache (+ wasted `clonePartitions`); redundant `connMonitorStop`; dead `selectStabilizationWindow` (+ orphaned `RestartRatio`); phantom `bucket-unavailable:<x>` sim-oracle reason; `#nosec G115` ×3 | mixed | calculator, lifecycle, churn | PR-by-PR accretion |

Out of scope (do **not** fold in): `internal/recovery/*` (durable-consumer recovery — a
separate, already-clean subsystem); the NATS-server-lifecycle and bucket-delete fault families
(distinct from the KV-wrapper seam); `provision/*` churn (separate SDK workstream); the
`selectAuthority` legacy-alias (already tracked for v3.0 removal).

---

## Phased plan

Ordering rule: **safety net → free wins → localized extractions → structural consolidations →
flagged/deferred.** Risk rises and dependency depth increases left-to-right.

### Phase 0 — Safety net (characterization tests only; zero production change)

Close the coverage gaps so the downstream refactors are provably behavior-preserving. Every
test here must satisfy a **two-part gate** — green-on-`main` alone is necessary but NOT
sufficient (a test that only ever passes is a hole in the net, not a thread of it):

1. **GREEN** on current `main` (no production change), AND
2. **RED under a discriminating perturbation** — the specific realistic mistake the
   Phase-1/2/3 step it guards could make (reorder/drop a guard, hold a lock through a critical
   section, move a boundary, impose a finite budget). Inject the perturbation, watch it fail in
   the predicted mode (assertion / hang / `-race` / panic), then revert and commit green. If a
   test cannot be made to fail under that perturbation, the **test** is vacuous and wrong — fix
   the test, not the perturbation. `0.6` is the template (negative-space); the other five must
   each carry their own perturbation. Two are at high vacuous-green risk and are re-scoped below.

> **The net is only real if it asserts on _observable behavior_, not internal fields.** Several
> Phase-0 tests guard the exact Phase-3 refactors that *move the internals* — a test that reads
> `m.degradedSince.Load()` or pokes `lastDegradedReason` gets rewritten by 3.1 and passes
> vacuously, proving nothing. Drive via the stable surface: induce the fault, assert `State()` /
> the `OnDegraded` reason / partition coverage / exit-or-stay-Degraded. The integration proofs
> (np3/np8/np9/np10) already do this — they are the load-bearing cross-refactor net for the
> structural work; the new unit tests assert outcomes, not field values.
>
> **One deliberate exception (T4):** the `lastDegradedReason` store-after-CAS / clear-before-since
> ordering is an *internal atomic invariant* with no observable surface — the existing
> `manager_reason_ownership_test.go` tests rightly inspect the fields and rely on `-race`. These
> stay **white-box** and are *rewritten against the new `degradedState` type* as part of 3.1 (they
> move with the internals they guard). Observable tests cannot catch a store-ordering regression.

> **Status (2026-06-02 — Phase 0 complete).** 0.1 `b7734db`→`1c4f1cb`, 0.2 `12cbecf`, 0.3
> `b7734db`, 0.4 `25cc378` landed as new gate-verified nets (each shown green on real code AND
> red under its discriminating perturbation). **0.5 was already covered** by the existing
> `TestPollForChanges_EmergencyPath_ReleasesPollMuBeforeRebalance`
> (`internal/assignment/calculator_state_test.go`) — verified green on real code (`-race`) and red
> under the 3.4 perturbation (move `pollMu.Unlock` after `RunClaimedRebalance`); the recon
> over-counted it as a gap, so no new test was added. **0.6 is relocated to Phase 4.2** (it nets
> only the deferred restart-loop refactor; grep-confirmed that no Phase 1–3 step touches
> `monitorCommitChanges` / `monitorWatcherWithRetry` / the restart-backoff loop). 0.1's empty-reason
> claim resolved as a non-vacuous PRESENCE test (the guard's placement among reason-scoped gates is
> behaviour-irrelevant — an empty reason never matches them — but its presence before `exitDegraded`
> is load-bearing and observable via `State()`), distinct from the T4 ordering test.

- **0.1** Recovery-gate precedence test (locks T1 ordering). **Re-scope (vacuous-green
  risk):** the gate is a conjunction of blocking guards — exit iff *no* guard blocks — so pure
  order *among blocking guards* is outcome-independent and a naive "first-blocking-wins" test
  locks nothing. The narrow OBSERVABLE claim is: the **empty-reason early-return blocks exit
  even when a reason-scoped signal is satisfied** (a blank reason must not ride a satisfied
  heartbeat/enum stamp out of Degraded). Construct the scenario where placement *changes* the
  observable `State()` outcome; if it cannot be shown observably, fold this into the **T4
  white-box exception** (it overlaps the post-CAS-pre-store window already guarded by
  `manager_reason_ownership_test.go`) rather than ship a vacuous observable test — and state
  explicitly what 0.1 adds beyond T4.
- **0.2** Unit characterization for the two integration-only recovery conjuncts: the
  `kv-unavailable` heartbeat-after-degrade gate and the `heartbeat-bucket` backstop
  (`manager_degraded.go:478-486, 503-506`).
- **0.3** Alert-level computation tests — `calculateAlertLevel`/`emitDegradedAlert`/
  `monitorDegradedAlerts` currently have **zero** coverage; any refactor touching the alert
  sub-feature is unprotected today.
- **0.4** `getActiveWorkers` end-to-end ordering test: one scan sequence exercising
  connectivity→enum-fail→recover→suspicious (currently split across two files).
- **0.5** `observeAndDecide` concurrency assertion: a poll can proceed while the emergency
  rebalance runs (locks the pollMu-released-before-rebalance invariant). **Re-scope
  (vacuous-green risk — the canonical concurrency trap):** the test MUST *actually overlap* the
  poll with the rebalance window — start the emergency rebalance, attempt a concurrent poll that
  can only proceed if `pollMu` was released, and show it **hangs/fails under `-race`** when
  `pollMu` is held through the rebalance. A test that doesn't force the overlap passes trivially
  and locks nothing. Perturbation = move the `Unlock` to after the rebalance call. **DONE — already
  covered** by `TestPollForChanges_EmergencyPath_ReleasesPollMuBeforeRebalance`, which drives a real
  emergency, parks the rebalance inside a blocking `Strategy.Assign`, and proves a concurrent
  `pollForChanges` is not blocked; verified green on real code and red under the perturbation. (No
  new test added; the recon over-counted this gap.)
- **0.6** Restart-loop negative-space (T2). **Relocated to Phase 4.2** — it nets only the deferred
  `restart-loop-helper` refactor, and no Phase 1–3 step touches the restart-budget loops. See 4.2 for
  the carried-forward design.

### Phase 1 — Free wins (delete dead code + cheap centralizations; low risk, independent)

> **Status (2026-06-02 — execution).** 1.1 `f33b34f` (reason-registry), 1.2 `6a2f10c`
> (delete write-only assignment cache), 1.3 `6896e23` (drop connMonitorStop), 1.4 `24e6e4f`
> (delete dead stabilization-window selector + mark RestartDetectionRatio orphaned), 1.5
> `05976bc` (windowLenInt32 helper) — all behavior-preserving, each with lint + unit `-race`
> green, and the full `make test-integration -race` suite GREEN (exit 0) after the set.
> **1.5 partially deferred:** the three test/sim-only hygiene items (`envgate-registry`,
> `tmp/parti-repro` seam citations, the phantom `bucket-unavailable:<x>` simulation-oracle
> reason) are split into a focused test-hygiene follow-up — they are non-load-bearing, and the
> sim-oracle reason correction needs careful investigation of the bucket-delete chaos oracle
> (which production reason it should expect) rather than a rushed edit. They do not gate Phase 2.

- **1.1 `reason-registry` (keystone).** Centralize all 9 degrade reasons into one
  exported `DegradeReason` const block (preserve string *values* verbatim — frozen contract,
  T-dependent); a constructor for the `bucket-recreated:<bucket>` dynamic suffix. Update every
  `enterDegraded` site to reference the consts, and tests too — **except** keep at least one
  test per operator-facing reason pinning the *literal* string
  (`require.Equal(t, "kv-unavailable", reason)`). If prod and every test share one const they
  drift together and a rename passes silently; the literal pin guards the const→value mapping.
  Unblocks Phase 3 (C3). **Inventory to migrate** (verify each before/after):

  | reason string | current form | site | recovery-scoped? |
  |---|---|---|---|
  | `kv-unavailable` | const | `manager_degraded.go:21` | yes (heartbeat stamp) |
  | `heartbeat-enumeration-stall` | const | `manager_degraded.go:28` | yes (enum stamp, leader-gated) |
  | `assignment-watcher-exhausted` | const | `manager_assignment.go:53` (entry :414) | no |
  | `KV error threshold exceeded` | literal | `manager_degraded.go:223` | no (global heartbeat-backstop) |
  | `NATS connection down` | literal | `manager_degraded.go:122` | no |
  | `stream-missing-recovery-exhausted` | literal | `manager_setup.go:83` | no |
  | `bucket-recreated:<bucket>` | literal prefix | `manager_setup.go:693` | no (global epoch-backstop) |
  | `startup-timeout` | literal | `manager_startup_async.go:187` | no |
  | `startup-background-panic` | literal | `manager_startup_async.go:47` | no |

  **Docs sync:** `docs/API_REFERENCE.md:886-897` lists the operator-facing reasons but is missing
  at least **`startup-background-panic`** *and* **`heartbeat-enumeration-stall`** — reconcile the
  *full* registry against the doc as part of this step (don't assume only one is missing).
- **1.2 `delete-lastassignment-cache`** — remove the write-only `lastAssignment`/
  `lastAssignmentAt` fields + the wasted `clonePartitions` (C9).
- **1.3 `drop-connmonitorstop`** — delete the redundant stop channel; `m.ctx.Done()` already
  exits the loop (C9).
- **1.4 `delete-selectStabilizationWindow`** — remove the dead heuristic + its two
  self-referential tests (C9). Flag `RestartRatio`/`RestartDetectionRatio` as now-orphaned →
  the *removal* is Phase 4 (public API), but because deleting the selector makes the knob a
  silent no-op while the public docs still describe it as behavior (`config.go:402-406`,
  `docs/API_REFERENCE.md:942,1309-1310`, `internal/assignment/doc.go:110-112`), this step must
  **mark the knob as currently no-op/orphaned in those docs** so the surface isn't misleading
  before the Phase-4 removal lands.
- **1.5 Hygiene** — `envgate-registry` (one `testutil.RequireOptInProof(t, NAME)` for the 3
  bespoke `PARTI_RUN_*` gates), fix dangling `tmp/parti-repro` seam citations, correct the
  phantom `bucket-unavailable:<x>` simulation-oracle reason, `windowLenInt32` helper for the 3
  `#nosec G115` bounds checks (C8/C9).

### Phase 2 — Localized extractions (low risk, well-covered, mostly independent)

> **Status (2026-06-02 — execution).** **2.1 DONE + codex-clean** (`4a46ad4` wrapped-ErrClaimLost
> characterization → `6965e2c` classifier extraction → `c66d296` comment nit): pure
> `classifyKVError` + shared `isWholeBucketLoss` extracted; `markKVUnavailable` kept a separate
> pre-step so direct `recordKVError` callers keep exact routing; lint + unit `-race` + full
> integration `-race` (exit 0) green; non-vacuous perturbation check passed; codex MERGE-WITH-NITS,
> no P0/P1, independently verified T5 + the 3-way `onClaimerError` split + **agreed with the split
> design over a unified classifier** (a unified one would couple the KV circuit to `stableid` and
> create dead routes). **2.3–2.5 also DONE (see below); 2.6–2.7 not yet started.**
>
> **2.2 DONE (reduced to the in-mandate win; `3032ae6`).** The full `shrinkGuard` VALUE-type
> migration was **rejected as out-of-mandate** after measuring the blast radius: the four baseline
> fields (`lastKnownWorkerCount`/`lastKnownPartitionCount`/`workerShrunkObservations`/
> `partitionShrunkObservations`) have **86 reference sites, ~48 in tests** that assert directly on
> the fields (`require.Equal(t, N, calc.partitionShrunkObservations)`). Moving them behind two guard
> instances to dedup ~15 lines of arithmetic — and branching internally on the
> worker-resets-on-`baseline==0` vs partition-leaves-untouched difference — would *add* a type plus a
> conditional and rewrite 48 assertions, the opposite of "simplify, low risk". **Done instead:**
> extracted the one genuine shared invariant, the sharp-shrink ratio, into a pure free function
> `shrinkSuspicious(observed, lastKnown, thresholdPct)` (zero blast radius — callers pass field
> values), locking the multiplied `observed*100 < lastKnown*Pct` form (vs the truncation-prone
> pre-divided form) that was previously enforced only by a comment and had already drifted in the
> partition config doc. A direct `TestShrinkSuspicious` pins the small-count truncation boundary the
> guards' scenario tests never reach (perturbation: pre-divide → red). **The cache-fallback half of
> 2.2 moves to 3.3** — the helper's right shape is driven by `getActiveWorkers`'s linearization
> (which 3.3 performs), and the Block-1-Warn vs Block-2-silent asymmetry can't be resolved cleanly in
> isolation. T3/T7 are untouched: the fields and the two guards stay exactly as they were.
>
> **2.2 + 2.3 codex verdict: MERGE, zero P0/P1/P2.** Independently verified 2.2's short-circuit /
> empty-vs-shrunk split / baseline-zero counter semantics and agreed with the pure-predicate
> reduction; verified 2.3's single-deadline-covers-open+read, per-call `defer cancel()`
> non-accumulation, skip-on-error, handle choice, and that leaving `heartbeatBucketUnavailable` /
> `captureBucketEpoch` out is correct.
>
> **2.3 DONE (`bc098f1`).** Extracted `probeBucketCreated(ctx, bucket, cached)` unifying the
> bounded stream-Created probe in `epochMismatchOutstanding` (fresh handle, `cached=nil`) and
> `checkBucketEpochs` (cached `ep.kv`); the single `OperationTimeout` deadline covering open+read
> (the bounded-inline-stall guarantee) now lives in one place. Dropped the now-unused `kvutil`
> import. lint + unit `-race` + integration `-race` (exit 0) green; perturbation = drop the bound →
> server-down probe took 20s vs the 3.2s `TestEpochMismatchOutstanding` bound → red.
>
> **2.4 DONE, reduced (`6add33f`).** Did the two unambiguous wins: `ensureCoreKVBuckets` returns a
> named `coreKVBuckets` struct (drops a `//nolint:revive` result-limit suppression), and the
> verbatim-shared post-update concurrent recheck became `bucketMaxAgeReconciled(ctx, kv, want)`.
> **Declined the headline reconciler merge** — the two reconcilers' operator-facing fail-loud
> messages + success logs diverge substantively (different consequences, format arities); a
> policy-struct merge would relocate that text into closures and ADD a core + struct on top (net
> MORE code). lint + unit `-race` + integration `-race` (exit 0) green; perturbation = recheck
> always-true → the two `*_FailLoudWhenUpdateDenied` tests went red.
>
> **2.5 DONE (`0bf2975`).** Extracted `emitTransitionEffects(from, to)` (log + OnStateChanged hook +
> RecordStateTransition metric) shared by `transitionState` and `casToStableFromWaitingAssignment`,
> so the two commit paths emit an identical transition by construction (was kept in sync by a comment);
> the by-value `from` also removed transitionState's manual `capturedFrom` copy. Added
> `TestCasToStableFromWaitingAssignment_EmitsStateChangeHook` (the cas tests asserted only `State()`).
> lint + unit `-race` + integration `-race` green; perturbation = drop the hook in the emitter → both
> the transitionState hook-tracking net AND the new cas net went red.
>
> **2.4 + 2.5 codex verdict: MERGE-WITH-NITS, no P0/P1.** Agreed with leaving the two reconcilers
> separate; verified the struct wiring, the recheck predicate, the emitter byte-identity, and the
> by-value closure-capture safety. One P2 (detached `ensureCoreKVBuckets` godoc) fixed in `683fcba`.
>
> **2.6 DONE + codex MERGE-WITH-NITS (`13b3937`, doc nit `842af8f`).** Extracted `runWatchSession`
> owning the watch-session select loop + shutdown-race rules, shared by `runAssignmentWatchSession`
> and `runCommitWatchSession`; the two callers pass `watchSessionHandlers` closures carrying their
> divergent decode/debounce/reconcile logic. The caller asymmetry (commit wraps `commitDebouncer`;
> assignment wraps a raw pending+timer, latest-wins) is deliberate and behavior-preserving. lint +
> unit `-race` + integration `-race` (exit 0) green. **Perturbation (advisor-mandated, since the
> shutdown-race rules ARE the justification):** flush-on-`ctx.Done` turned BOTH
> `*_DebounceCancelDoesNotFlush` red; no-flush-on-channel-close turned BOTH `*_PendingFlushesOnClose`
> red — one shared-skeleton corruption reddens both watchers' nets. codex P2 (godoc overstated the
> no-flush guarantee as global vs per-arm) fixed.
>
> **2.7 SEQUENCING (user decision, 2026-06-02): Phase 3 BEFORE 2.7.** 2.7 (`kvfault-toolkit`) is a
> large test-only mega-refactor (≈18 near-identical fault types across 6 seams + sim token-disarm)
> that does not block Phase 3. Phase 3 is the production structural consolidation and the named
> finish line, so it ran first. **OUTCOME: Phase 3 completed; then 2.7 DECLINED** after the six-seam
> characterization (see the 2.7 bullet + `01-kvfault-seam-characterization.md`). **Phase 2 is
> "2.1–2.6 done, codex-clean; 2.7 declined."** The consolidation pass is complete.

- **2.1 `degrade-decision-taxonomy`** — extract a pure, table-tested classifier whose decision
  carries **route/owner semantics**, not just `{drive, transient, reason}`. `onClaimerError`
  routes `ErrClaimLost` peer-takeover to `claimLostShutdown`, connectivity/degrading-wrapped
  claim loss to `recordKVError`, and other renew failures to `recordKVOpError` (which itself
  excludes the peer-takeover branch) — so the decision must distinguish at least
  `drop / kvWindow / kvUnavailableWindow / claimLostShutdown / streamMissingObserver`.
  `recordKVError` becomes window bookkeeping over the decision; `markKVUnavailable`,
  `recordKVOpError`, and `onClaimerError` reuse it. Absorbs the triplicated union helper.
  Add table tests for **`ErrClaimLost` peer-takeover vs `ErrClaimLost` wrapping
  connectivity/degrading** (`manager_claimer_error_test.go` pins bare-`ErrClaimLost` shutdown and
  a non-claim connectivity error today, but **not** the wrapped-`ErrClaimLost` routing case — add
  it). **Preserve T5.** (C2)
- **2.2 `shrink-guard-kernel` + `cache-fallback-helper`** — **DONE, reduced** (`3032ae6`): shared
  `shrinkSuspicious` ratio function only (full value-type migration rejected on an 86-site blast
  radius; see status note). Cache-fallback dedup **deferred to 3.3** (its shape depends on the
  linearization). (C7)
- **2.3 `unify-bucket-probe`** — **DONE** (`bc098f1`). one `probeBucketCreated(ctx, bucket)` helper (fresh handle +
  `OperationTimeout`, the goroutine-safety rationale in one place); the monitor degrades on
  mismatch, the recovery re-probe returns a bool. Fresh-vs-cached handle stays a parameter. (C5)
- **2.4 `unify-maxage-reconcilers` + `ensure-bucket-policy-helper`** — **DONE, reduced** (`6add33f`; reconciler merge declined — see status note). collapse the two
  near-identical MaxAge reconcilers and the 3 ensure+timeout+reconcile call sites; return
  buckets via a struct to drop the 4-value-return `//nolint`. (C9/setup)
- **2.5 `targeted-transition-primitive`** — **DONE** (`0bf2975`). extract `emitTransitionEffects(from,to)` so
  `casToStableFromWaitingAssignment` stops hand-duplicating `transitionState`'s hook+metric
  body. (C4)
- **2.6 `session-loop-harness`** — **DONE** (`13b3937`). extract the watch-session select skeleton (the
  flush-unless-ctx-cancelled + reconcile-swallow rules) with an **injectable debounce
  strategy**; commit injects its version-guard+payload-hash debouncer, assignment injects
  latest-wins. Highest-value monitor win; does NOT touch the restart/escalation layer (C6).
- **2.7 `kvfault-toolkit`** — **DECLINED (user decision, 2026-06-02).** A parallel six-seam
  characterization (`01-kvfault-seam-characterization.md`) showed the seams do NOT share the planned
  `Rule{Buckets,Ops,KeyPrefix,err}` model: op-sets are each seam's essence (Keys-only / Get-only /
  write-only / +Watch / a superset), `wf` has two independently-armed flags, `simKV` has a
  token-generation disarm pinned by two tests + a Delete/Purge carve-out, and the prefix convention
  conflicts (empty=all vs empty=none). A ~250-line engine encoding all of it replaces ~6×30 lines of
  honest per-seam wrappers — marginal/negative net complexity, large blast radius (6 files across
  failure/manager/simulation + their exact-count/reason proofs). Decisive: for a FAULT HARNESS a
  subtly-wrong shared injector silently makes the auto-healing proofs **vacuously pass** — strictly
  worse than duplication, so the bar is HIGHER than for production code. Same disposition as the 2.4
  reconciler-merge and 3.1. ~~one **exported** op-selective KV-fault package
  (`Controller` + data-driven `[]Rule{Buckets, Ops, KeyPrefix, err}` JetStream wrapper, with
  sim's token-generation disarm). Port the 6 seams onto it (failure_test first, then
  manager_test, then simulation). The proofs' `injected`-counter + exact-reason assertions are
  the characterization tests. Biggest test-code win (C8).

### Phase 3 — Structural consolidations (depend on Phase 0 tests + Phase 1/2 primitives)

> **Status (2026-06-02 — execution). 3.1 DECLINED after the advisor-mandated pre-cut design check.**
> The full atomic→struct migration fails the same bar 2.2/2.4 failed, with bigger numbers: the
> degrade/recovery fields have **70+ scattered direct references across 14 test files**
> (`degradedSince` alone = 37: 27 `.Load()` asserts + 10 `.Store()` setups; `kvErrorCount` = 17;
> `lastDegradedReason` = 11), with **no harness chokepoint** — so it is NOT a near-zero-blast-radius
> change. And it would NOT reduce net complexity: **T4 is already enforced in `enterDegraded` /
> `exitDegraded`, the only writers** (the fields are package-private — no rogue writer to fence out),
> so a struct's `enter()`/`exit()` would *relocate* the hand-written ordering, not make it
> un-break-able. The plan's `Manager.IsDegraded()`/`DegradedReason()` public API is **also skipped**:
> no consumer wants it today (`State() == StateDegraded` already covers the boolean; the only
> `DegradedReason()` users live in the simulation and read the reason from the `OnDegraded` HOOK, not
> by polling the manager), and speculative public surface is a worse forward-commitment than internal
> churn. **The one version that WOULD pay off is a behavior change, deferred to Phase 4:** collapse
> `{degradedSince, lastDegradedReason}` into a single `atomic.Pointer[record]` swapped atomically —
> that genuinely eliminates the post-CAS/pre-store window and lets the empty-reason recovery gate be
> *deleted* (killing the subtlety instead of relocating it). Removing a gate is not
> behavior-preserving → its own proof. **3.2/3.3/3.4 do NOT depend on the struct:** 3.2's
> `{signalGetter, leaderOnly}` table reads reason/since/signal-timestamps identically whether they
> are bare fields or struct methods. Proceeding to 3.2.
>
> **PHASE 3 COMPLETE (2026-06-02).** 3.1 declined (above); **3.2** (`6f85e3a`) codex **MERGE, 0
> findings**; **3.3** (`efddae3`) + **3.4** (`1dc6383`) reviewed together → codex **CHANGES-NEEDED**
> with one valid P1 (3.4 moved the degraded-cache skip log under `pollMu`; the parent unlocked
> before it — a slow injected Logger would extend the poll critical section). Fixed in `25a0212` via
> a `pollAction` enum so the wrapper logs that skip (and runs the emergency rebalance) only after the
> lock releases; **clean re-review → MERGE, 0 findings**. All four steps: lint 0 + unit `-race` +
> integration `-race` (exit 0, all 11 packages) + non-vacuous perturbation. **The cross-model gate
> earned its keep here — codex caught a real behavior delta the empirical suite did not.**

> **HOLISTIC POST-IMPL REVIEW (2026-06-02).** Beyond the per-unit passes, ran a whole-branch
> cross-model review (codex `-s read-only`) over the full production diff `91ada73..HEAD` (13 files,
> 596+/392-), targeting the cross-cutting issues the per-step reviews structurally couldn't see:
> aggregate behavior-preservation, new-helper interactions, the load-bearing invariants across the
> whole change, leftover/dead code, and whether it actually simplified. **Verdict: MERGE — zero
> P0/P1/P2.** Codex re-verified each invariant end-to-end: T5 (`classifyKVError` keeps stream-missing
> out of the threshold path), `isWholeBucketLoss` claim-loss split, recovery-exit ordering, the
> assignment-bounded vs commit-infinite watcher-restart separation, `observeAndDecide`
> release-before-rebalance + post-release degraded-cache log, and `emitTransitionEffects` parity on
> both state-commit paths. The formal done-gate (per-unit + holistic post-impl review) is now
> satisfied; nothing to fold.

> **`/simplify` pass (2026-06-02).** Ran the 4-angle cleanup review (reuse / simplification /
> efficiency / altitude) over the same diff. Reuse, efficiency, and altitude: zero findings — the new
> helpers dedup genuine duplication, add no hot-path waste, and sit at the right abstraction boundary.
> Simplification surfaced ONE in-scope item: the internal `assignment.Config.RestartRatio` field went
> write-only-dead within this branch (its sole reader, `selectStabilizationWindow`, was deleted in
> `24e6e4f`) and was being carried as a documented no-op. Removed the field, its default, and the
> public→internal mapping (commit `db69e2c`); the public `RestartDetectionRatio` knob is unchanged
> (still validated, still a no-op, public removal deferred to a major version). Behavior-preserving;
> gated lint 0 + `go test -race ./internal/assignment` + `go build ./...`.

- **3.1 `degraded-state-struct`** — **DECLINED (see status note); the value-add version is a Phase-4
  behavior change.** ~~encapsulate the ~12 degrade/recovery atomics into one type
  with `enter(reason)`/`exit()`/`reason()`/`since()`/`isDegraded()` so the store-ordering
  (**T4**) is impossible to call out of order; `Manager` gains `IsDegraded()`/`DegradedReason()`.
  Sub-structs for the conn-down/up and recovery-grace pairs.~~ (C4)
- **3.2 `recovery-guard-pipeline`** — **DONE, reduced** (`6f85e3a`; codex MERGE, zero findings). Shared
  `recoverySignalStalled` for the two reason-scoped gates; order PRESERVED (no reorder — log-order +
  live re-probe execution are observable), backstops left as sequential calls. Added an equal-stamp
  boundary test so the `<=` is perturbation-netted. ~~turn the cascade into: refresh → commitment guard →
  ordered guard list → `exitDegraded`. **Per T1**, only the 2 reason-scoped stamped-signal
  gates become a `{signalGetter, leaderOnly}` table; the global backstops stay a fixed ordered
  list. Reads `degraded-state-struct` + `reason-registry`. (C3)
- **3.3 `getActiveWorkers-linearize`** — **DONE** (cache-fallback half). Extracted
  `cacheFallbackOrDegraded` (the 2.2-deferred dedup) with an `onHit` hook for the connectivity-Warn
  vs suspicious-silent asymmetry; the function was already linear so no further restructure. ~~restructure into explicit stages
  (classify → cache-or-threshold → credibility → result); ordering invariants become the linear
  sequence, not comments. **Absorbs 2.2's deferred cache-fallback-or-`ErrDegraded` dedup** — extract
  the helper here, where its shape (and whether the connectivity-Warn vs suspicious-shrink-silent
  asymmetry collapses) is driven by the actual linearized use. (C7)
- **3.4 `observeAndDecide-unlock`** — **DONE**. Split into `observeAndDecideLocked` (single `defer`
  unlock, returns `runEmergency`) + a thin wrapper that runs `RunClaimedRebalance` after the lock
  releases. The 0.5 release-before-rebalance test pins it (perturbation: rebalance inside the locked
  core → red). (C7)

### Phase 4 — Flagged / deferred (behavior or API change — separate sign-off, NOT a cleanup)

- **4.1 `delete-restartratio`** — remove the orphaned public `RestartDetectionRatio` knob
  (deprecation cycle; public-API break).
- **4.6 `degraded-record-pointer` (from 3.1)** — collapse `{degradedSince, lastDegradedReason}` into
  one `atomic.Pointer[record]` swapped atomically. This eliminates the post-CAS/pre-store window and
  lets the empty-reason recovery-exit gate (`manager_recovery_emptyreason_test.go`) be **deleted** —
  killing the T4 subtlety instead of relocating it. **Behavior change** (removes a gate) → needs its
  own reproducer/proof; out of scope for the behavior-preserving 3.1.
- **4.2 `restart-loop-helper`** — unify the 3 watch-restart loops behind one policy-parameterized
  helper. **High risk (T2)** — must preserve infinite-retry. Fold in `jitter-helper` +
  within-`parti` `watcher-const-centralize` here.
  - **Gating commit (carried forward from Phase 0.6 — write FIRST, before the helper).** A
    negative-space net proving the commit watcher (`monitorCommitChanges`) and worker monitor
    (`monitorWatcherWithRetry`) restart **forever** under sustained failure and **never**
    self-terminate or self-degrade. The existing `TestMonitorCommitChanges_ChannelCloseTriggersBackoffAndRestart`
    is positive-space only (proves *one* restart) — a bounded budget would still pass it.
    - **Harness:** `forceCloseWatcherKV` / `forceCloseLatest` / `watchCallsLoaded`
      (`manager_commit_watcher_test.go`) for the commit watcher; a `Watch`-always-fails wrapper +
      `mon.watchBaseBackoff` (a settable field) for the worker monitor.
    - **Speed:** both backoffs are **test-tunable vars**, not consts — `watcherBaseBackoff`
      (`manager_assignment.go:26-28`) and `WorkerMonitor.watchBaseBackoff` — so drive restarts at a
      sub-second cadence; do NOT wait the 2s production backoff.
    - **Assertions (observable):** drive several consecutive failures and assert `State()` stays
      `StateStable`, no `OnDegraded` fires (reuse `assignmentWatcherReasonSpy`), the re-`Watch`
      count keeps growing, and the monitor goroutine does not exit until `ctx` is cancelled. For the
      worker monitor (which has no degrade path of its own) assert on its own re-subscribe count +
      non-termination, NOT on an unrelated `Manager.State()`.
    - **Discriminating perturbation:** wrap the loop in a bounded `retry.Envelope`
      (`MaxAttempts=N`, `OnPermanent→enterDegraded`) — mirrors exactly the mistake this step could
      make. The test must go red (watcher self-terminates / self-degrades after N). Note honestly
      that a negative-space test catches budgets ≤ the number of restarts driven; drive enough to
      cover a realistic small budget.
- **4.3 `unify-classifier-home` (substring deletion)** — consolidate classifier packages onto
  `errors.Is`; deleting a `strings.Contains` fallback needs a test proving the wrapped-sentinel
  path now covers it (med risk).
- **4.4** Merge `internal/recovery/classify.go`'s parallel taxonomy with C2 — out of scope for a
  behavior-preserving pass; revisit only if a unified taxonomy is desired.
- **4.5** `selectAuthority` legacy-alias removal → defer to the planned **v3.0** cut.

---

## Per-step verification gate

Each numbered step is its own branch/commit and must pass, before merge:
`make lint` (0 issues) · `go test -race ./internal/assignment .` · `go test -race ./test/integration/...`
· the Phase-0 characterization tests still green · `/simplify` (no-churn) · a cross-model
(`codex`) review · the commit/PR message free of internal jargon (per repo convention).

## Candidate inventory (quick reference)

| id | phase | risk | effort | coverage | cluster |
|----|-------|------|--------|----------|---------|
| reason-registry | 1.1 | low | M | well | C1 |
| delete-lastassignment-cache | 1.2 | low | S | none(dead) | C9 |
| drop-connmonitorstop | 1.3 | low | S | thin | C9 |
| delete-selectStabilizationWindow | 1.4 | low | S | thin(dead) | C9 |
| envgate/seam-citation/sim-reason/windowLenInt32 | 1.5 | low | S | n/a | C8/C9 |
| degrade-decision-taxonomy | 2.1 | low | M | partial→0.x | C2 |
| shrink-guard-kernel + cache-fallback-helper | 2.2 | low | M/S | well | C7 |
| unify-bucket-probe | 2.3 | low/med | M | partial | C5 |
| unify-maxage-reconcilers + ensure-bucket-policy | 2.4 | low | M | well/partial | setup |
| targeted-transition-primitive | 2.5 | low | M | well | C4 |
| session-loop-harness | 2.6 | med | M | partial | C6 |
| kvfault-toolkit | 2.7 | low | L | well | C8 |
| degraded-state-struct | 3.1 | low-med | L | well | C4 |
| recovery-guard-pipeline | 3.2 | med | M/L | well(+0.1/0.2) | C3 |
| getActiveWorkers-linearize | 3.3 | low | M | partial(+0.4) | C7 |
| observeAndDecide-unlock | 3.4 | med | M | partial(+0.5) | C7 |
| delete-restartratio | 4.1 | **high (API)** | M | thin | C9 |
| restart-loop-helper | 4.2 | **high (T2)** | M | partial(+0.6) | C6 |
| unify-classifier substring-delete | 4.3 | med | M | partial | C2 |

## Decisions (resolved)

- **Scope:** execute **Phases 0–3** (full behavior-preserving consolidation, incl.
  `kvfault-toolkit`). **Phase 4 deferred** (public-API / behavior changes — separate sign-off).
- **Execution venue:** this dedicated `auto-healing-consolidation` worktree/branch; each numbered
  step is its own commit run through the per-step gate, reviewed before the next.
- **Plan review:** codex read-only plan-review loop until SOUND (see Review trail).

## Review trail

- **2026-06-02 — codex (`gpt-5.5`, read-only) round 1 → SOUND-WITH-FIXES, no P0.** All five
  load-bearing claims independently verified against source (dead-code zero-readers; T1
  global-vs-reason-scoped; T2 `retry.New` panics on `MaxAttempts<=0` + infinite commit/worker
  watchers; T3 two counters / cross-isolation test; T5 classifier precedence). Folded in: P1.1
  (Phase 0 white-box carve-out for the T4 reason-ownership ordering tests, rewritten against the
  new type in 3.1); P1.2 (2.1 classifier decision carries route/owner semantics inc.
  `claimLostShutdown`, + `ErrClaimLost` table tests); P2.1 (reason inventory table + API_REFERENCE
  sync in 1.1); P2.2 (1.4 marks `RestartDetectionRatio` no-op in docs pending Phase-4 removal).
- **2026-06-02 — codex round 2 (confirmation) → SOUND-WITH-FIXES, no residual/new P0/P1.** All
  three checked fixes confirmed resolved against source. Two residual P2 precision nits folded in:
  the API_REFERENCE omission note now names both `startup-background-panic` *and*
  `heartbeat-enumeration-stall` (reconcile the full registry); the 2.1 "pinned today" overclaim
  softened (the wrapped-`ErrClaimLost` routing test does not exist today — add it). **Loop closed:
  no P0/P1 outstanding.**
- **2026-06-02 — execution-start amendment (advisor, pre-Phase-0).** Hardened the Phase-0
  acceptance gate: green-on-`main` is necessary but not sufficient; every safety-net test must
  also go **RED under a discriminating perturbation** that mirrors its guarded step's realistic
  mistake (else the net has vacuous-green holes — this is the repo's verify-first / test-both-
  directions discipline). Re-scoped **0.1** (gate is a blocking-guard conjunction → order among
  blocking guards is outcome-independent; the observable claim is the empty-reason early-return,
  else fold into the T4 white-box exception) and **0.5** (must actually overlap poll×rebalance
  and fail under `-race` when `pollMu` is held through the rebalance). Gate-tightening + scope
  narrowing only — no claim weakened, so no fresh codex round required.
- **2026-06-02 — Phase 0 complete (execution).** 0.1/0.2/0.3/0.4 landed as new gate-verified nets;
  0.5 found already covered by an existing gate-verified test (no new test added); 0.6 relocated to
  Phase 4.2 (nets only the deferred restart-loop refactor; grep-confirmed no Phase 1–3 step touches
  `monitorCommitChanges` / `monitorWatcherWithRetry` / the restart-backoff loop). Every new net was
  shown green on real code AND red under its discriminating perturbation before commit. Several
  workflow-drafted tests were corrected during authoring (a method mistaken for a reassignable
  field, a `js==nil` short-circuit, a stale `>=`→`>` perturbation that was itself vacuous, a name
  collision with an existing `blockingStrategy`) — the empirical red/green gate, not the drafts,
  certified each net.
- **2026-06-02 — Phase 1 codex review → MERGE-WITH-NITS, no P0/P1.** codex (read-only, given the
  pre-run lint/unit-`-race`/integration-`-race` green tails per the external-reviewer contract)
  independently verified all five load-bearing claims: degrade-reason values byte-identical (all
  pinned), `connMonitorStop` removal behavior-preserving (monitor exits on `ctx.Done()`; the old
  "already closed" comment was indeed wrong), `lastAssignment`/`selectStabilizationWindow` truly
  dead before deletion, exported untyped `DegradeReason*` consts the right call (a named type would
  not help while `OnDegraded` takes `string`), `windowLenInt32` equivalent at both sites. One P2
  (stale `internal/assignment/doc.go` rebalance-strategy prose describing the removed selector) —
  fixed in `df7cdf8` (grepped the whole doc for parallel stale text). Phase 1 done.
- **2026-06-02 — Phase 2.1 codex review → MERGE-WITH-NITS, no P0/P1.** codex (read-only, given the
  green lint/unit-`-race`/integration-`-race` tails + the perturbation result) independently verified:
  `recordKVError` precedence preserved; T5 preserved (whole-bucket `transient=false`, transient
  entries cleared on healthy ops, threshold reason split intact); `markKVUnavailable` correctly kept
  separate with exactly two direct `recordKVError` callers (recovery refresh + bucket-loss-wrapped
  claim loss); the 3-way `onClaimerError` split unchanged; and explicitly **agreed with the split
  design** (a unified classifier would mix lifecycle shutdown into the KV circuit and create dead
  routes). One P2 (imprecise "classify both ways" comment) fixed in `c66d296`. The full integration
  `-race` suite also passed (exit 0) — the cross-feature contracts a classification change must
  clear. 2.1 done.
