# Latched-Worker Version-Commitment Self-Heal — Follow-Up Plan

- **Date:** 2026-05-31
- **Status:** REVIEW-CLEAN, ready to implement (plan-review v4 = "ready to implement,
  no design gate remains"; reports `tmp/03-latched-worker-version-commitment-plan_review{,_v2,_v3,_v4}.md`).
  v1-v3 each found a real correctness hole, all closed. The recovery guard keys on the
  **applied-ack identity** `(Version, LeaderRevision, PartitionSetDigest,
  source-rev-when-known)`, ungated on non-empty, with an **audit-asymmetric** source
  comparison (cur-unknown always matches; cur-known requires committed known+equal).
  Closes same-version divergence (v1 P0), empty/revoke-all bypass (v2 P0), stale-LR
  ack (v2 P1), source-downgrade-on-alias-refresh (v3 P0). Next: reachability spike (§5.0).
- **Builds on:** the degraded-recovery self-heal work merged via PR #32
  (`docs/plans/auto-healing-quorum-loss-fix/01-degraded-recovery-self-heal-plan.md`
  and `02-degraded-recovery-self-heal-implementation.md`). This plan scopes the
  **single remaining open item** from that series: the
  "Deferred follow-up surfaced by the final review"
  (`02-degraded-recovery-self-heal-implementation.md:766-800`).
- **Origin:** PR4 (F-D3) post-implementation review v1, finding **P1-1**, whose
  *latched* sibling was then surfaced by the PR #32 final review and explicitly
  deferred. Deferred because the clean fix replaces the F-D3 one-way latch with
  per-version commitment tracking, which changes the **prev source for the entire
  apply pipeline** (cross-feature contracts) and warrants its own
  contract-regression pass rather than being bundled into PR #32.
- **Design consensus:** Approach A below was agreed by three independent passes —
  the maintainer, the advisor model, and Codex (`codex exec`, read-only, full
  run). Codex verdict: *"A per-assignment committed source of truth is the right
  invariant; B is another patch on the latch, C weakens the intentionally
  monotonic refresh path. Contract risk: Low."* The four Codex corrections that
  shaped this plan are folded in below (§3.1 callouts).
- **Relationship to the shipped series:** F-D2a/F-D2c/F-D1/F-D3 and the
  degraded-recovery self-heal are all merged to `main`. This is the **last open
  engineering item** from the quorum-loss fix series.

---

## 1. The defect (precise statement)

The same Stable-with-uncommitted-claims defect that PR #32 fixed for the
*un-latched bootstrap* worker also exists for an **already-latched** worker on a
failed version-advance apply. PR #32 is byte-for-byte equivalent to `main` on the
latched path, so it did not regress this — but it *slightly increases its
reachability* (more workers now self-heal and reach the latched state).

Precise sequence:

1. A worker has committed V1 claims, so `initialClaimsCommitted` is latched `true`
   (`manager.go:148-154`).
2. A V2 assignment is published. Apply V2 runs through
   `applyAssignmentWithPrevCore` (`manager_assignment.go:1227+`). Because the
   worker is latched, the F-D3 bootstrap override
   (`manager_assignment.go:1264-1266`) does **not** fire, so `prev =
   CurrentAssignment() = V1` — correct at this instant. But
   `handoffCoordinator.Apply` **fails before Store** (claim-write fault);
   `scheduleApplyRetry(V2)` stashes V2; the snapshot stays V1.
3. The worker enters Degraded (the KV-error circuit, via the renewal/heartbeat/
   election write sites — not the apply path).
4. Recovery `attemptRecoveryFromDegraded` (`manager_degraded.go:315-353`) calls
   `refreshAssignmentFromNATS`, which `monotonicStore`s V2 into the snapshot
   (`manager_assignment.go:1385-1399`, `1502-1524`). Now `CurrentAssignment() ==
   V2`.
5. The recovery guard (`manager_degraded.go:345-349`) tests
   `!initialClaimsCommitted && len(cur.Partitions) > 0`. The latch is **already
   `true`**, so the guard is skipped → `exitDegraded` → **`StateStable` reported
   while V2's newly-assigned claims are unwritten.**
6. The pending retry fires: `prev := m.CurrentAssignment() == V2 == next`, so the
   prepare diff (`internal/assignment/handoff/twophase.go:208-232`) is **empty**
   (it writes only partitions in `next` not in `prev`) → zero claims written → the
   newly-assigned partition is never claimed → **restart-only tail.**

Two symptoms, both must be fixed and **asserted independently**:

- **Symptom (a) — false `Stable`.** The worker reports `StateStable` at V2 while
  owning no claim for V2's newly-assigned partition.
- **Symptom (b) — empty-diff non-heal.** Even after writes recover, the pending
  retry self-terminates with an empty diff; the partition is never claimed until a
  process restart.

> **Framing correction (Codex):** `preparePhase` does **not** release dropped
> claims — it *writes the newly-acquired set* (`next` minus `prev`) and updates the
> consumer to `next.Partitions` (`internal/assignment/handoff/twophase.go:57-93`,
> `208-232`). The bug is precisely that the newly-acquired set goes empty. Release/
> cleanup of dropped claims is a separate phase and is **out of scope** here.

---

## 2. Invariants this fix MUST NOT regress (load-bearing)

The fix changes the **prev source for every apply** (from `CurrentAssignment()` to
the new committed-assignment state). The contract-safety argument rests on a single
invariant — prove it once, and the per-contract review is mechanical:

> **Invariant I0:** in **healthy post-apply steady state**, `committedAssignment ==
> snapshot` on the full **applied-ack identity** `(Version, LeaderRevision,
> PartitionSetDigest, source-rev-when-known)`. The snapshot can be advanced past
> `committedAssignment` by exactly three paths, two of them startup-only and all
> three handled by the guard: (1) **startup pre-advance** — `waitForAssignment`
> stores the initial assignment before `applyInitialAssignment`
> (`manager_election.go:461-474`); (2) **cold-empty startup** — directly stores+acks
> `Assignment{}` without `handoffCoordinator.Apply` (`manager.go:657-681`), which
> matches the zero committed value so the guard exits; (3) **recovery refresh** —
> `monotonicStore` (the only POST-START non-apply snapshot writer,
> `manager_assignment.go:1524`). Every healthy *steady-state* apply
> (`applyAssignmentWithPrevCore`) advances both together. So in steady state they
> diverge **only** on the bug/recovery path — when a refresh stored an assignment
> whose apply had not succeeded; the startup divergences are exactly the
> bootstrap/cold cases the guard re-arms or exits correctly. Divergence is detected by full identity, NOT version alone: the
> publisher can expose two different partition sets at the same version (§3.4), and
> empty/revoke-all assignments all share digest 0 so only Version/LR distinguish
> them — a version-only or non-empty-gated test would mistake an unapplied
> assignment for applied. With the identity guard, changing `prev` from
> `CurrentAssignment()` to `committedAssignment` is a **no-op on every path a
> contract test exercises**; it is observable *only* where it heals.

Because `committedAssignment` advances **only after a successful `Apply`** (never
by `monotonicStore`/refresh), and a successful `Apply` also Stores the same
snapshot under `applyStoreMu`, the snapshot is always `>=` committed. There is no
visible state where committed has advanced past the snapshot.

1. **Cross-feature contract 1** (whole-bucket-missing → every worker
   `StateDegraded`, recovers when the bucket returns). Unaffected: the routing
   lives in `recordKVError` (`manager_degraded.go:128-160`), not the apply path;
   `prev=committedAssignment` is inside `applyAssignmentWithPrevCore`. Under
   whole-bucket loss `refreshAssignmentFromNATS` fails → recovery returns at the
   existing err-check before the guard.
2. **Cross-feature contract 2** (peer-claim-takeover → only that worker enters
   claim-lost shutdown). Untouched — this fix is in the apply/recovery path, not
   the claimer-error path (`manager_election.go:106-127`). `committedAssignment` is
   process-local and is *not* read or reset by claim-loss handling.
3. **Cross-feature contract 3** (OnDegraded fires exactly once per Degraded entry).
   Held by construction: the generalized guard *stays* degraded (returns without
   `exitDegraded`), so `degradedSince` stays non-zero and `enterDegraded`'s CAS
   blocks any re-fire across the held recovery ticks. `recordKVError`
   short-circuits while degraded (`manager_degraded.go:152-159`). **Asserted by a
   test** (§5.3).
4. **Contract 4 (startup readiness CAS).** **`startupAssignmentApplied` is NOT
   touched** (`manager.go:156-161`, `manager_startup_async.go:114-129`). It is the
   readiness/WaitingAssignment→Stable latch, *not* claim commitment. Only the
   `initialClaimsCommitted` latch is generalized. Conflating the two would regress
   contract 4 on a transition this fix does not target. *(Codex constraint.)*
5. **The `(V,LR)` stale gate stays against `CurrentAssignment()`, NOT committed
   state** (`manager_assignment.go:1240-1253`, `1161-1169`). Only the *prev source*
   changes; the gate's comparand is unchanged. *(Codex constraint.)*
6. **Ordering and atomicity.** Under `applyStoreMu`, the success path stays:
   `Apply` success → LSR advance → snapshot Store → **`committedAssignment` Store**
   → heartbeat `SetAppliedAssignment` (`manager_assignment.go:1309-1339`). The
   heartbeat publisher is version-only monotone and accepts equal versions, so
   keeping `SetAppliedAssignment` inside the lock remains load-bearing
   (`internal/heartbeat/publisher.go:176-202`). **Recovery read — decided, not a
   fork:** the guard reads `committedAssignment` (an `atomic.Pointer`) and
   `CurrentAssignment()` (a lock-free atomic, `manager.go:846-860`) **lock-free**,
   NOT under `applyStoreMu` — taking the apply lock on the connection-monitor
   goroutine would serialize the monitor against every apply (and apply holds the
   lock across a possibly-slow `handoffCoordinator.Apply`). The guard reads `cur`
   then `committed` as two independent atomics, so a torn read can observe either
   *new snapshot + old committed* OR *old snapshot + new committed* (committed
   "ahead" of the stale `cur`). **The safety property is NOT "committed never ahead
   of snapshot" — it is "no missed heal, at most a redundant re-arm":** a torn read
   that re-arms a stale `cur` schedules a retry the `(V,LR)` stale gate then drops,
   and because the guard re-evaluates with fresh reads every recovery tick, a
   genuinely-unapplied current assignment is always eventually re-armed — a transient
   torn read costs at most one extra tick, never a permanent false exit. Pinned by a
   hook-based unit test (§5.2) asserting exactly that: an unapplied current
   assignment is always eventually re-armed, and the only torn-read effect is a
   redundant/stale-dropped retry.
7. **Steady-state recovery must not change.** A healthy worker (committed
   applied-ack identity == current's) that degrades for an unrelated reason recovers
   exactly as today: the guard predicate is false → `exitDegraded`. Pinned by the existing negative-space test
   (`manager_degraded_recovery_selfheal_test.go`) plus a new latched-version one
   (§5.4).
8. **Mixed-version / rolling upgrade — safe.** `committedAssignment` is
   process-local in-memory state; it does not change wire format, schema, or
   claim-key layout. A restart begins with committed empty → the first apply writes
   the full set; existing-owner stable claims no-op in prepare/commit
   (`internal/assignment/handoff/twophase.go:289-318`, `351-357`). No cross-process
   inheritance, no same-version gating needed.

---

## 3. The fix (Approach A — universal committed-prev)

Replace the one-way `initialClaimsCommitted atomic.Bool` with a tracked
**last-committed assignment** held separately from the snapshot, updated on **every
successful apply** (not one-way). The apply pipeline diffs `prev` against the
committed assignment instead of `CurrentAssignment()`.

### 3.0 State

```go
// manager.go — replaces initialClaimsCommitted.
//
// committedAssignment holds the last assignment this worker successfully
// applied+acked (a successful handoffCoordinator.Apply through
// applyAssignmentWithPrevCore). Two roles: (1) the prev source for every apply's
// prepare diff; (2) the source of truth for "is the current snapshot assignment
// applied?" — compared on the full applied-ack identity (Version, LeaderRevision,
// PartitionSetDigest, source-rev-when-known), §3.2. Starts as the zero Assignment
// (empty@0) — a never-applied worker diffs against empty → writes the full set
// (subsumes the old F-D3 bootstrap override) and its identity matches a cold
// empty snapshot so a never-assigned worker still exits recovery. Distinct from
// the snapshot (m.assignment): the snapshot can be advanced by the recovery
// refresh's monotonicStore (sole non-test caller, manager_assignment.go:1524)
// past an apply that failed before Store; committedAssignment cannot. Distinct
// from startupAssignmentApplied (readiness CAS). Full Assignment, not just an
// identity tuple: preparePhase needs the previous partitions to drive the diff.
committedAssignment atomic.Pointer[Assignment]
```

`committedAssignment` is a **full `Assignment`** — Codex: a digest can *compare*
but cannot *drive* the `next`-minus-`prev` diff in `preparePhase`
(`internal/assignment/handoff/twophase.go:216-232`).

### 3.1 Apply pipeline (`applyAssignmentWithPrevCore`, `manager_assignment.go`)

Two edits, both inside the existing `applyStoreMu` critical section:

```go
// (unchanged) stale gate compares newAssignment vs CurrentAssignment().
curAssignment := m.CurrentAssignment()
if isApplyResultStale(newAssignment, curAssignment) { ... return nil }

// CHANGED: prev source is the committed assignment, not the passed-in
// oldAssignment / CurrentAssignment(). Generalizes the F-D3 bootstrap override:
//   - never committed  → committed == empty → full set (old override case)
//   - steady V1→V2     → committed == V1 == snapshot → identical diff to today
//   - failed-V2+refresh → committed == V1, snapshot == V2 → diff acquires V2's
//                          new partitions correctly (the heal)
oldAssignment = m.committedAssignmentOrEmpty()

applyErr := m.handoffCoordinator.Apply(m.ctx, workerID, oldAssignment, newAssignment)
if applyErr != nil { ... scheduleApplyRetry(newAssignment); return applyErr }

// ... LSR advance → snapshot Store ...

// CHANGED: record commitment for this version AFTER a successful Apply, on the
// snapshot Store ordering, still under applyStoreMu. Replaces the one-way
// initialClaimsCommitted latch. Updated on EVERY successful apply (so the guard
// reduces to "exit" in steady state); never updated by monotonicStore/refresh.
m.committedAssignment.Store(&newAssignment)

// ... heartbeat SetAppliedAssignment (unchanged ordering) ...
```

> **Why prev = committed, not the passed-in `oldAssignment`.** Today every call
> site (retry, commit-watcher, assignment-watcher) passes some `prev`; with this
> change the passed-in value becomes dead for the diff. Confirm during
> implementation that **no caller legitimately needs a non-current prev** before
> retiring that plumbing; if any does, keep its argument but override here. *(This
> is the advisor's "nice retirement, confirm first" note — a simplification to
> verify, not assume.)*

### 3.2 Recovery guard (`attemptRecoveryFromDegraded`, `manager_degraded.go:345-349`)

The exit-safe condition is **"the current snapshot assignment has been successfully
applied and acked"** — NOT "version is greater" and NOT gated on non-empty. The
identity is the **applied-ack identity**: `(Version, LeaderRevision,
PartitionSetDigest, SourceRevision when known)` — exactly the fields a successful
apply commits to the heartbeat (`manager_assignment.go:1329-1337`) and the leader
audit compares (`internal/assignment/calculator_audit.go:93-99,125-146`).

```go
// currentAssignmentApplied reports whether the current snapshot assignment has
// been successfully applied+acked by this worker, i.e. committedAssignment (set
// only on a successful Apply) matches it on the full applied-ack identity. NOT
// version alone (the publisher can expose two DIFFERENT partition sets at the
// SAME version: a legacy alias is written at proposedVersion before the commit
// CAS, and a batch can abort post-alias or lose the CAS without advancing
// currentVersion — assignment_publisher.go:345,380-398,437-456, metered as
// IncrementAliasVisibleUncommitted; the (V,LR) gate admits same-version higher-LR
// — manager_assignment.go:1161-1169). LeaderRevision/source ARE included: the
// applied-ack heartbeat carries them and the audit flags a mismatch as behind, so
// a same-version/same-digest/higher-LR assignment that only entered the snapshot
// via refresh is NOT applied until re-acked. Empty/revoke-all (case (d),
// buildAssignmentFromCommit) is a real versioned apply path too — empty sets all
// have digest 0, so Version+LR are what distinguish them; hence no len()>0 gate.
func (m *Manager) currentAssignmentApplied(cur Assignment) bool {
    c := m.committedAssignmentOrEmpty()
    // Source comparison is AUDIT-SHAPED (asymmetric), mirroring
    // calculator_audit.go:121-126 — NOT strict flag equality. cur is the
    // authoritative target (the audit's commit), c is what we acked (the
    // audit's heartbeat): cur-unknown always matches; cur-known requires c
    // known AND equal. The degraded refresh reads the legacy alias
    // (assignment.<workerID>), and buildLegacyAlias DROPS source revision
    // (assignment_publisher.go:1316-1341 — SourceRevision intentionally unused),
    // so a refreshed snapshot is source-unknown at a V/LR/digest the worker may
    // have already acked with source KNOWN. Strict flag equality would re-arm and
    // re-ack a DOWNGRADE to AppliedSourceRevKnown=false, which the audit then
    // classifies as behind. The asymmetric form never downgrades a stronger ack.
    srcOK := !cur.SourceRevisionKnown ||
        (c.SourceRevisionKnown && c.SourceRevision == cur.SourceRevision)
    return c.Version == cur.Version &&
        c.LeaderRevision == cur.LeaderRevision &&
        srcOK &&
        types.PartitionSetDigest(c.Partitions) == types.PartitionSetDigest(cur.Partitions)
}

// Stay degraded + re-arm whenever the current assignment is not yet applied+acked:
// never committed (old F-D3 bootstrap), an earlier version (latched advance), a
// DIFFERENT set at the same version (alias/split-brain divergence), a versioned
// empty revoke whose apply failed, or a same-version higher-LR re-issue picked up
// by refresh. cur is read AFTER the refresh so it captures any advance during the
// degraded window. The cold zero-state (Assignment{}) is applied-by-identity
// against the zero committed value, so a never-assigned worker still exits.
cur := m.CurrentAssignment()
if !m.currentAssignmentApplied(cur) {
    m.scheduleApplyRetry(cur)
    return // do NOT exitDegraded until a real apply+ack commits THIS assignment
}
m.exitDegraded()
```

`types.PartitionSetDigest` already exists and is the apply path's own digest
(`manager_assignment.go:1329`). The implementer must confirm the field set against
`heartbeat.AppliedAssignment` (`manager_assignment.go:1329-1337`) and the audit
(`calculator_audit.go`) — match those exactly; do not invent fields. I0 holds
because in **post-start steady state** the snapshot is advanced past
`committedAssignment` only by the recovery refresh's `monotonicStore` (the sole
post-start non-apply snapshot writer, `manager_assignment.go:1524`); the two
startup pre-advance paths (§2 I0) match the zero/bootstrap committed value, and
every steady-state apply advances both together — so including LR/source causes no
healthy-state spurious re-arm.

In steady state `committed == cur` on the full identity → predicate false → exits
exactly as today. The re-armed retry runs through the §3.1 pipeline, so its prev is
`committedAssignment` → the prepare diff acquires the uncommitted partitions (or, for
an empty revoke, the apply re-runs the consumer teardown + re-ack) → self-heal once
writes recover, then the next tick sees the assignment applied and exits to `Stable`
with claims/ack actually current.

### 3.3 The four Codex corrections folded in (record)

1. `preparePhase` writes newly-acquired only; "release dropped" reframed (§1 note).
2. `committedAssignment` is a full `Assignment`, not `(version, digest)` (§3.0).
3. The `(V,LR)` stale gate keeps comparing against `CurrentAssignment()` (§2.5).
4. `startupAssignmentApplied` is untouched (§2.4).

Plus: committed updates **only on successful Apply**, never by refresh (§3.1); the
recovery read is lock-free with a documented benign interleaving (§2.6); the
recovery guard keys on the **applied-ack identity** `(Version, LeaderRevision,
PartitionSetDigest, source-rev-when-known)` and is **not** gated on non-empty, so
neither a same-version partition-set divergence (review v1 P0), an empty/revoke-all
implicit-revoke (review v2 P0), nor a same-version higher-LR refresh (review v2 P1)
can be mistaken for applied (§3.2).

### 3.4 Same-version divergence is IN scope (was the plan-review P0)

The publisher can expose two different partition sets at the **same version** for
one worker: a legacy alias is written at `proposedVersion` before the commit CAS,
and a batch can abort post-alias (`assignment_publisher.go:387-398`) or lose the
commit CAS (`:437-452`) without advancing `currentVersion` (`:456`) — the code
meters this as `IncrementAliasVisibleUncommitted`. Such a same-version, higher-LR
assignment is admitted by the `(V,LR)` gate (`manager_assignment.go:1161-1169`),
observed by the alias watcher (`manager_assignment.go:574-614`) /
`manager_select_authority.go:33-63`, and can land in the snapshot via the degraded
refresh. The §3.2 identity guard (digest, not version) detects this: committed
`N/A` vs current `N/B` → digests differ → stay degraded + re-arm. The reproducer
(§5.1) covers the V1→V2 advance; a unit/protocol discriminator (§5.2) covers the
same-version `N/A`→`N/B` divergence so the digest term is proven non-vacuous.

### 3.5 Residual stale-retry interaction (level-triggered convergence)

If the refresh `monotonicStore`d the current assignment at a **higher
LeaderRevision** than a *stashed* retry, that retry's candidate is stale
(`candidate.LR < cur.LR` → `isApplyResultStale == true`,
`manager_assignment.go:1161-1169`) and is dropped. The **next** recovery tick
re-arms with the current assignment (which the identity guard still flags as
uncommitted), so the level-triggered loop converges; and a higher-LR assignment
normally also arrives as a fresh watcher apply (prev = committed) that heals
directly. Not a new tail — this is the existing stale-gate behavior, now backstopped
by the identity guard re-arming every tick until the current assignment commits.

---

## 4. Why Approach A over the alternatives (consensus record)

- **Approach B (keep the bootstrap latch; add a recovery-only `lastCommittedVersion`
  guard + store the last-committed assignment only to feed the re-armed retry's
  prev).** Narrower blast radius but more special-casing — and it still has to store
  the committed assignment to give the retry a correct prev, so it converges toward
  A while keeping a second, redundant latch. Codex: *"another patch on the latch."*
  Rejected.
- **Approach C (make `refreshAssignmentFromNATS` not advance the snapshot past the
  committed version, so prev stays V1 and the existing retry heals).** Narrowest,
  but it special-cases — and weakens — the intentionally monotonic refresh path,
  which other recovery logic relies on. Codex: *"weakens the intentionally monotonic
  refresh path."* Rejected.
- **Approach A (universal committed-prev).** One source of truth for "claims
  committed for this version," subsuming the F-D3 bootstrap override as its empty
  case. Contract-safe by invariant I0 (no-op except on the bug path). **Chosen** —
  unanimous (maintainer + advisor + Codex).

---

## 5. Test & contract-regression plan (first-class deliverable)

This is why the item was deferred from PR #32. The reachability spike is a **hard
gate**, and both symptoms are asserted **independently**.

### 5.0 Reachability spike FIRST — RED-on-parent or STOP

Before any fix code, build the §5.1 reproducer and confirm it fails on the current
`main`. **If it cannot be driven RED on parent, STOP and reassess** — the merged
fixes may already foreclose the state (per the F-D2b/F-D3 lessons:
`project_quorum_loss_repro_status`, `feedback_verify_first_with_reproducer`). Do not
write the fix for an unreachable state.

### 5.1 RED-on-parent reproducer (both symptoms) — `test/integration/failure/`

Reuse the existing `wfMutableSource` watchable source and the **dual** write-fault
JetStream (`newWFFaultJetStreamDual` + `wfFaultController.{ArmWrites,ArmHeartbeat,
DisarmWrites}`) already in `test/integration/failure/startup_writefault_test.go`.
The dual (claim + heartbeat) fault is required because a single-worker leader's
startup-timeout watchdog does **not** fire (the calculator leaves
WaitingAssignment within ~1s); Degraded is driven via the heartbeat-write KV-error
circuit — the same technique the PR #32 integration test used (its KEY IMPL
FINDING).

Choreography:
1. Start one worker with source `[p0]`, **no faults**. Wait `StateStable`; assert
   p0's claim is Stable. *(This commits V1 — the worker is latched on parent.)*
2. `ArmWrites()` (claim) + `ArmHeartbeat()`, then `src.set([p0, p1])` to publish
   V2. Assert a claim-write fault fired for V2 (counter > 0).
3. Wait `StateDegraded` (heartbeat circuit); hold the fault past several
   recovery/`ExitThreshold` ticks. Recovery reads V2, `monotonicStore`s it, skips
   the latched guard, exits.
4. **Assert symptom (a) — false Stable:** `StateStable` **and**
   `CurrentAssignment().Version == 2` **and** p1's claim is absent / not Stable in
   KV. *(RED on parent: parent reports Stable here.)*
5. `DisarmWrites()`; wait beyond the retry cadence. **Assert symptom (b) —
   non-heal:** p1's claim is still never Stable **and** a publish to p1 is not
   consumed. *(RED on parent: the pending retry self-terminated with prev ==
   CurrentAssignment() == V2 → empty `preparePhase` diff.)*
6. GREEN with the fix: after disarm the worker writes p1's claim at V2 and consumes
   p1 — **no restart**.

Non-vacuity: confirm V2 genuinely landed (snapshot version moved to 2) before the
assertions, so the test proves the heal targets V2 and is not a vacuous pass.

### 5.2 Unit discriminators (with fakes / `recordingHandoff`)

- **`TestApplyCore_PrevIsCommittedNotSnapshot`:** commit V1; advance the snapshot to
  V2 via `monotonicStore` (no apply); apply V2 through core; assert the coordinator
  received `prev == V1` (committed), **not** V2 (snapshot), and that
  `committedAssignment` moves to V2 only after the apply succeeds.
- **`TestApplyCore_CommittedUpdatesEveryApply`:** apply V1 then V2 (both succeed);
  assert `committedAssignment` tracks each (proves not one-way) and that a failed
  apply does **not** advance it.
- **Recovery branch selection (applied-ack identity guard).** Each row sets
  `committedAssignment` and the snapshot, then asserts re-arm-stays-degraded vs exit:
  - `{committed N/A, cur N+1/B, non-empty}` → re-arm (version advance).
  - **`{committed N/A, cur N/B (same V, different digest)}` → re-arm.** *(review v1
    P0 — a version-only guard would WRONGLY exit; proves the digest term.)*
  - **`{committed N/A (non-empty), cur N+1/empty (versioned revoke)}` → re-arm.**
    *(review v2 P0 — a `len(cur)>0` gate would WRONGLY exit; proves the no-len-gate.)*
  - **`{committed empty@N, cur empty@N+1 (both digest 0)}` → re-arm.** *(empty-vs-
    empty across versions; proves Version distinguishes digest-0 sets.)*
  - **`{committed N/A/LR1, cur N/A (same V, same digest, higher LR2 via refresh)}` →
    re-arm.** *(review v2 P1 — proves LR is in the identity so a stale applied-ack
    is re-acked before exit.)*
  - `{committed N/A/LR1, cur N/A/LR1 (full identity match)}` → exits.
  - **`{committed N/A/LR/source-KNOWN=S, cur N/A/LR/source-UNKNOWN (legacy-alias
    refresh)}` → EXITS without re-arm, keeps the known-source ack.** *(review v3 P0 —
    strict flag equality would re-arm and downgrade the ack to source-unknown, which
    the audit flags as behind; the audit-asymmetric predicate must not.)*
  - `{committed source-unknown, cur N/A/LR/source-KNOWN=S}` → re-arm *(weaker acked
    than the known-source target the worker should apply).*
  - `{cold zero: committed Assignment{}, cur Assignment{} (empty@0)}` → exits
    *(never-assigned worker; proves the zero value is "applied-by-identity").*
  - `{refresh fails}` → returns before guard, records KV error.
- **`TestRecoveryGuard_TornRead_NoMissedHeal`:** force both torn-read windows
  (new-snapshot/old-committed AND old-snapshot/new-committed) via a test hook;
  assert a genuinely-unapplied current assignment is **always eventually re-armed**
  and the only torn-read effect is a redundant/stale-dropped retry — NOT a claim
  that committed is never ahead of snapshot (pins §2.6 as reworded).
- **Heartbeat / audit boundary:** assert an equal-version re-apply with a different
  digest OR a higher LR is not treated as applied merely because the version
  matches (guards against the heartbeat publisher's version-only monotonicity,
  `internal/heartbeat/publisher.go:176-202`, and aligns with the audit's
  LR/source/digest comparison, `internal/assignment/calculator_audit.go:93-99`).

### 5.3 Contract-3 assertion (held by construction → pinned)

Assert `OnDegraded` fires **exactly once** across the whole held recovery window
(multiple ticks while the guard holds the worker degraded), reusing the hook-count
style of `TestManager_LiveNATSBucketLoss_OnDegradedHook`.

### 5.4 Negative-space (both-directions-of-boundary discipline)

A worker whose committed applied-ack identity equals its current assignment's, that
degrades for an unrelated reason and whose refresh succeeds, must `exitDegraded` →
`Stable` **exactly as today**. (Per `feedback_test_both_directions_of_boundary`: the
positive heal test alone is consistent with both a correct guard and an always-hold
bug; this negative test discriminates.) The existing
`manager_degraded_recovery_selfheal_test.go` negative case stays GREEN; extend it
for the latched (committed-V1, cur-V1 full-identity-match healthy) path.

### 5.4a Same-version divergence protocol test (review v1 P0 surface)

The §5.2 same-version unit discriminator mutates the struct directly; add one test
that drives the **real** alias-visible-uncommitted shape so the digest guard is
proven against the protocol, not a synthetic mutation: expose a legacy alias for a
worker at version `N` for set A (or commit `N/A`), then expose a *different*
same-version assignment `N/B` (higher LR) for that worker — via the alias watcher /
`monotonicStore` refresh path — without a successful `Apply` of B, and assert the
worker stays degraded + re-arms and ultimately commits B's claims, never reporting
`Stable` at `N/B` with B's claims absent. If a full protocol setup is too heavy,
encode it at the `handleAssignmentEntry` + recovery-guard seam with a faulted apply
for B.

### 5.4b Empty/revoke-all failed-apply test (review v2 P0 surface)

The implicit-revoke case (d) — a worker absent from `commit.Workers` applies a
**versioned empty** assignment that still calls `UpdateWorkerConsumer(empty)` and
publishes `AppliedVersion=N`/digest 0 (`manager_assignment.go:916-925`,
`buildAssignmentFromCommit` 1038-1053, pinned by
`manager_commit_state_machine_test.go:239-248`). Drive it on the `wf` harness or at
the commit-state-machine seam: worker committed `V1` (non-empty); publish a commit
that drops the worker (→ empty@`V2`); fault the empty apply (updater error) before
Store; let recovery refresh `monotonicStore` empty@`V2` into the snapshot. Assert
the worker stays degraded + re-arms (NOT exits as `Stable` with the revoke
un-applied / consumer not torn down / ack stale at `V1`), and on disarm completes
the revoke apply, publishes `AppliedVersion=V2`, and exits. RED on the version-only
or `len>0`-gated guard; GREEN on the applied-ack-identity guard.

### 5.5 Concurrency `-race` stress test (AGENTS.md monitor-goroutine requirement)

`attemptRecoveryFromDegraded` runs on the **connection-monitor goroutine** and this
fix keeps its apply-issuing side effect (`scheduleApplyRetry`) **and** adds a new
shared field (`committedAssignment`) read on that goroutine and written on the apply
goroutine. Add / extend a focused `-race` stress test in
`test/integration/failure/` (template: the existing
`degraded_recovery_rearm_concurrency_test.go`, and AGENTS.md's
`epoch_monitor_concurrency_test.go` pattern): drive the real recovery goroutine
(armed write fault) ↔ apply pipeline ↔ commit-watcher against the same handoff
bucket for ~5s; assert no race-detector trips on `committedAssignment` / snapshot /
`applyStoreMu` / `stashedApplyRetry`.

### 5.6 Mandatory gate (AGENTS.md pre-PR + cross-feature contracts)

- The **3 cross-feature contract regression tests**:
  `TestManager_LiveNATSBucketLoss`, `TestManager_LiveNATSBucketLoss_OnDegradedHook`,
  `TestStableID_StaleKeyTakeover_Reclaim`.
- `make test-integration -race` (recovery-path change on a shared monitor
  goroutine — the unit suite cannot reproduce the goroutine race).
- `make pre-pr` (touches `manager/`).

---

## 6. Phasing

Single PR (one model change across the apply core + recovery guard + its test
surface). Per the repo's standard loop: **reachability spike first** (§5.0:
RED-on-parent or STOP) → implement → `/simplify` → review gate (`/codex:review`,
fall back to `/post-impl-review` for spec-compliance) → fix-loop to merge-clean →
squash on merge. `make pre-pr` + the 3 contract tests + `make test-integration
-race` on the final tree.

**Model/effort:** Opus, high. Small diff but it sits on the contract-pinned shared
apply/recovery path; rigor goes on invariant I0, the contract-regression gate, and
the `-race` stress test.

**Commit-message discipline:** no plan/PR jargon (`feedback_no_plan_jargon_in_commits`),
no attribution trailers (`feedback_no_commit_attribution`). PR title shape:
`fix(manager): track claim commitment per version so a failed version-advance self-heals`.

---

## 7. Out of scope

- F-D3 option 3b (don't pre-advance the snapshot in `waitForAssignment`) — a larger
  startup-ordering change; A closes the defect without it. Remains deferred.
- Same-V/higher-LR stale-gate residual (§3.4) — pre-existing, converges via the
  level-triggered loop; not introduced or worsened here.
- Release/cleanup of *dropped* claims on a version advance (a separate phase from
  `preparePhase`) — unrelated to this defect.
- The F-D1 flapping-tuning and pull-gating fail-open decisions (00-fix-plan §7) —
  tuning/policy questions.
