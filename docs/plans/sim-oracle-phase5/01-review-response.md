# Phase 5 — Plan Review v1 Response

Codex review at `tmp/sim-oracle-phase5_review.md`. Verdict: REVISE.
Three P0s, two P1s, one P2. All defensible and addressed below; the
plan has been updated accordingly.

## P0-1 — Unknown-owner duplicates make Outcome A unsound — **ACCEPT**

Codex is right. `currentOwners == []` should not silently fall into the
redelivery bucket, because that lets every uninitialized-snapshot cross-
worker duplicate masquerade as legitimate.

**Resolution** (plan §3, §3a, §6, §10 updated):

- New classification: `MessageOwnershipInconclusiveError` /
  `ErrMessageOwnershipInconclusive`. Returned when `currentOwners == []`
  AND the owner snapshot has been initialized (i.e., at least one
  `AssignmentReport` has been ingested).
- New classification: `MessageOwnershipUnobservedError` /
  `ErrMessageOwnershipUnobserved`. Returned when `currentOwners == []`
  AND the snapshot is uninitialized (cold start, before any worker has
  reported).
- Outcome A now requires **all four** to be zero: ownership violations,
  concurrent-owner records (see P0-2), inconclusive records, and
  unobserved records (with snapshot initialized). Unobserved during cold
  start is acceptable but must close out before the first ChaosEvent
  fires — see Outcome A criterion 9.

## P0-2 — Multi-owner snapshots not classified — **ACCEPT**

Right. The previous table left `len(currentOwners) > 1` (without orig
in the set) implicitly classified as handoff redelivery. That masks
two-way split-brain where the prior owner has already dropped its
self-report but a third worker has been concurrently assigned with
the receiving worker.

**Resolution** (plan §3 table revised):

The classifier now checks `len(currentOwners) > 1` **first**, before the
membership tests:

| Owner-set predicate                                  | Classification                |
|------------------------------------------------------|-------------------------------|
| `len(currentOwners) > 1`                             | **ConcurrentOwnersViolation** |
| `len == 1`, contains workerID, !contains origWorker  | Redelivery (handoff)          |
| `len == 1`, contains workerID, contains origWorker   | (impossible — len==1)         |
| `len == 1`, !contains workerID, contains origWorker  | OwnershipViolation (stale)    |
| `len == 1`, !contains workerID, !contains origWorker | OwnershipViolation (stranger) |
| `len == 0`, snapshot initialized                     | OwnershipInconclusive (P0-1)  |
| `len == 0`, snapshot uninitialized                   | OwnershipUnobserved (P0-1)    |
| ownerLookup == nil                                   | Legacy fallback (preserve)    |
| origWorker == workerID                               | Redelivery (same worker)      |

Note: "stranger" — receiver isn't assigned but origWorker isn't either —
is a violation. The receiver shouldn't be processing a partition no one
owns; if owner snapshot is fresh enough to know origWorker is gone, it's
fresh enough to know receiver isn't authorized.

## P0-3 — `workerAssignments` not safe to read cross-goroutine — **ACCEPT**

Confirmed by direct read: `workerAssignments` has no mutex; `mu`
mentioned in §1 of the old plan was hallucinated. Iterating it from the
receipt goroutine while `processAssignments` writes it is a data race
and will produce `fatal error: concurrent map iteration and map write`
under chaos churn.

**Resolution** (plan §1 revised):

Replace `mu`-based reads with an immutable owner snapshot published via
`atomic.Pointer`:

```go
type ownerSnapshot struct {
    perPartition map[int][]string // partitionID -> sorted workerIDs
    initialized  bool             // false until first ingestion
    asOf         time.Time
}

// In Coordinator:
ownerSnapshot atomic.Pointer[ownerSnapshot]
```

- `processAssignments` rebuilds the snapshot on **every** mutation of
  `workerAssignments` (the ingestion loop and the stopped-worker prune)
  and atomically stores a new pointer. Single-writer; no lock needed.
- `CurrentOwnersOf(pid)` loads the snapshot atomically and returns the
  pre-built `perPartition[pid]` slice. Slice is part of an immutable
  snapshot — safe to share without copy.
- `CurrentOwnersOf` also returns a freshness boolean so the classifier
  can distinguish initialized vs uninitialized snapshots (P0-1).

**Scope note**: a pre-existing race at coordinator.go:402 (`startWorker-
CatchUp` reads `c.workerAssignments` from the receipt goroutine) is
**not** in scope here. It predates Phase 5 and is independently fixable
in a follow-up. We do not "improve adjacent code" per [Rule 3](../../AGENTS.md#rule-3--surgical-changes).
Document as a known-issue in `tmp/sim_phase5_known_issues.md` for
future Phase 6 cleanup.

## P1-1 — Audit schema only captures violations, not downgrades — **ACCEPT**

Right. Outcome A claims would be unverifiable without durable records.

**Resolution** (plan §3, §6, §10 updated):

`FailureReport` gains two bounded slices alongside `OwnershipViolations`:

- `ConcurrentOwnerEvents []ClassificationEvidence` — for the new
  ConcurrentOwnersViolation classification (P0-2).
- `InconclusiveOwnerEvents []ClassificationEvidence` — for
  OwnershipInconclusive cases (P0-1).
- (UnobservedOwnerEvents are not retained — they happen pre-baseline
  and are expected; the count alone is sufficient.)

```go
type ClassificationEvidence struct {
    PartitionID    int
    Sequence       int64
    OriginalWorker string
    ReceivingWorker string
    CurrentOwners  []string
    SnapshotAsOf   time.Time
    Reason         string // "concurrent_owners", "inconclusive_owners", "stale_origin", ...
}
```

Bound at `ownershipViolationsCap` (1000, existing field) for both new
slices. Step 6 outcome rules consume these structured records, not
just counts.

## P1-2 — H2 (heartbeat-watcher) deferral lacks an interpretation rule — **ACCEPT (modified)**

Codex argues that without H2 health-gating, a stale resolver could
cause a real admission bug that the new classifier silently downgrades.
Two responses:

**(a) The classifier already won't silently downgrade.** Under P0-1
fixes, any cross-worker duplicate that the discriminator can't explain
lands in `OwnershipInconclusive` (if snapshot initialized) or counts
as un-classified evidence. Outcome A requires all four counters at
zero; H2-induced gate admission bugs will surface as inconclusive
records, not be hidden.

**(b) But Outcome A → enable CI is still risky** if H2 health is
unobservable. The CI gate would pass on a quiet 5m run that happens to
not hit the resolver edge case.

**Resolution** (plan §6 outcome rules + §10 acceptance criterion):

- Outcome A enables gate-on CI **only at the chaos_gate.yaml duration**
  (5m), with `--stop-on-failure=false` and 3-retry budget matching the
  existing simulation-stress job pattern.
- Outcome A does **NOT** enable gate-on at `chaos_comprehensive.yaml`
  duration (8m, aggressive 15-25s chaos). That decision is deferred to
  a future phase after H2 is addressed, per the chaos_gate.yaml header
  context.
- The new CI job, if added, runs `chaos_gate.yaml` specifically — same
  workload that validated the classifier, no scope creep.

## P2 — Draft commit message pre-decides Outcome A — **ACCEPT**

Right. Removed the pre-decided outcome paragraph from the plan's draft
commit message. The final commit message will be written from the actual
run results.

## Plan changes applied

1. §1 — replace `mu`-based reads with `atomic.Pointer` snapshot; add
   `CurrentOwnersOf` freshness return; document scope-out of pre-existing
   line 402 race.
2. §3 — new classification table with cardinality-first check; four
   classification outcomes (Violation / ConcurrentOwners /
   Inconclusive / Unobserved / Redelivery).
3. §3a — replaced; explicit Unobserved vs Inconclusive semantics.
4. §5 — three additional unit tests:
   - `TestClassify_ConcurrentOwners_CurrentAndThird`
   - `TestClassify_OwnershipInconclusive_SnapshotInitialized`
   - `TestClassify_OwnershipUnobserved_SnapshotUninitialized`
   - `TestCurrentOwnersOf_ConcurrentAssignmentIngestion` (race test)
   - `TestFailureReport_IncludesClassificationEvidence`
5. §6 — Outcome A requires all four counters at zero; CI enablement
   scope-limited to chaos_gate.yaml workload.
6. §10 — acceptance criteria 5, 8, 9 revised.
7. Commit message draft — outcome paragraph removed.

The updated plan is at `docs/plans/sim-oracle-phase5/00-plan.md`.
