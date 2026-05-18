# Sim Oracle Phase 5 — Gate-On Ownership Violation Investigation

## TL;DR

Phase 3 added the `RecordReceivedFromWorker(pid, seq, workerID)` API and
classifies same-`(pid, seq)` arrivals from a **different** workerID than the
recorded "original" worker as `MessageOwnershipViolationError`. Phase 4 ran
`chaos_gate.yaml` (gate-on, 2m) and observed exactly such a violation:
partition=19 seq=153 processed by worker-5, then by worker-0 24 s later after
a `worker_restart` and `network_disconnect_leader` event pair.

This phase determines whether that violation is **(a)** a real parti gate
gap, **(b)** an oracle false positive caused by classifying legitimate
redelivery-after-reassignment as a violation, or **(c)** both — and lays
the structural groundwork (current-owner discriminator, four-way
classification) needed to make that determination soundly.

**Out of scope:** H2 (heartbeat-watcher invariants), H5 (abrupt-kill
semantics), H7 (checkpoint completeness), M2 (RecoveryStrategy), M3
(ResolverConfig tuning), M4 (`source.NatsKV` chaos). All independent;
none blocking gate-on CI **at chaos_gate.yaml duration**. They get their
own phases. Phase 5 does **not** enable gate-on at chaos_comprehensive.yaml
duration — that follow-up requires H2 first (see §6 Outcome A scope).

## Why

The current classifier — at `tracker.go:lookupOrigWorkerLocked` /
`classifyKnownDuplicateLocked` — encodes this invariant:

> Same `(partitionID, partitionSeq)` observed twice with different
> `workerID` → ownership violation.

That invariant is **wrong**. JetStream is at-least-once. When a partition
is reassigned (chaos, scale, restart, leader failure), the new owner
**necessarily** redelivers any un-acked sequences — and those sequences
were originally processed by the previous owner. Same seq, different
workerID, both correct.

The real exclusivity property parti guarantees is:

> At any time T, at most one worker is the **canonical owner** of
> partition P, as recorded in the leader's assignment KV.

A duplicate from a different worker is only a violation if **at the moment
of receipt** that worker is not the canonical owner — or if **two workers
are concurrently the canonical owner**, which is the real split-brain
scenario the Processing Gate is designed to prevent.

Without that discriminator, the gate-on CI signal is useless: every
chaos-induced reassignment generates a false positive, so a real gate
gap would be drowned out.

## Phase plan

### Step 1 — Publish an immutable owner-snapshot via atomic.Pointer

The coordinator already maintains `workerAssignments map[string]map[int]struct{}`
populated from the `OnAssignmentChanged` hook (coordinator.go:1171).
**This map has no mutex.** All current writes happen in one goroutine
(`processAssignments`), but extending reads to the receipt goroutine
without synchronization would cause `fatal error: concurrent map
iteration and map write`.

Solution: publish an **immutable** snapshot via `atomic.Pointer`. The
ingestion goroutine remains the sole writer (no locks); the receipt
goroutine reads atomically.

Add to `Coordinator` (coordinator.go):

```go
// ownerSnapshot is an immutable view of partition→workers at a moment in time.
// Built and published by processAssignments; consumed by the tracker via
// CurrentOwnersOf. The slices are sorted for deterministic test output and
// must NOT be mutated after store; subsequent updates publish a new pointer.
type ownerSnapshot struct {
    perPartition map[int][]string
    initialized  bool      // false until first AssignmentReport ingested
    asOf         time.Time // last update
}

// In struct:
ownerSnap atomic.Pointer[ownerSnapshot]
```

Add public method:

```go
// CurrentOwnersOf returns (workerIDs, snapshotInitialized).
// workerIDs is a slice of workers that report partitionID in their most-recent
// OnAssignmentChanged snapshot. The slice is immutable (do not mutate).
// snapshotInitialized is false during cold start, before any AssignmentReport
// has been processed.
func (c *Coordinator) CurrentOwnersOf(partitionID int) (owners []string, snapshotInitialized bool)
```

Refactor `processAssignments` so every write to `workerAssignments` (the
ingestion loop AND the stopped-worker prune at coordinator.go:1130) is
followed by a rebuild + atomic store of `ownerSnap`. Initialize `ownerSnap`
to `&ownerSnapshot{perPartition: map[int][]string{}, initialized: false}`
in `NewCoordinator` so `Load()` never returns nil.

**Scope note (pre-existing race, NOT in scope):** `startWorkerCatchUp`
at coordinator.go:402 reads `c.workerAssignments` from the receipt
goroutine without synchronization — that race predates Phase 5. Do not
fix in this phase ([Rule 3 — Surgical Changes](../../AGENTS.md#rule-3--surgical-changes)).
Add a one-line note to `tmp/sim_phase5_known_issues.md` for future
cleanup.

### Step 2 — Owner-lookup callback on tracker

Refactor `MessageTracker.RecordReceivedFromWorker` to use an optional
owner-lookup callback set via:

```go
func (t *MessageTracker) SetOwnerLookup(fn func(partitionID int) (owners []string, snapshotInitialized bool))
```

Constraints:
- Lookup may be `nil` (unit tests, legacy callers) — in that case the
  classifier falls back to current behavior (origWorker mismatch →
  violation), preserving pre-Phase-5 semantics for any test or caller
  that hasn't opted in.
- Lookup must not block. Cost is one atomic load + one map lookup.
- Pattern matches existing `SetLogOutOfOrder` / `SetWorkerCacheMax`.

### Step 3 — Refine `classifyKnownDuplicateLocked`

Refined classification table when `(pid, seq)` is a known duplicate
(i.e., `lookupOrigWorkerLocked` returned `known=true`). Predicates
evaluated **top-down**; first match wins.

| #  | Predicate                                                | Classification               |
|----|----------------------------------------------------------|------------------------------|
| 1  | `origWorker == workerID`                                 | Redelivery (same worker)     |
| 2  | `ownerLookup == nil`                                     | OwnershipViolation (legacy fallback) |
| 3  | `len(currentOwners) > 1`                                 | **ConcurrentOwnersViolation** |
| 4  | `len(currentOwners) == 1 && contains(workerID)`          | Redelivery (handoff)         |
| 5  | `len(currentOwners) == 1 && contains(origWorker)`        | OwnershipViolation (stale receipt — receiver not assigned) |
| 6  | `len(currentOwners) == 1 && !contains(either)`           | OwnershipViolation (stranger receiver) |
| 7  | `len(currentOwners) == 0 && snapshotInitialized == true` | **OwnershipInconclusive** (mid-handoff after baseline) |
| 8  | `len(currentOwners) == 0 && snapshotInitialized == false`| **OwnershipUnobserved** (cold start) |

Returns:
- Row 1, 4 → `ErrMessageRedelivery`
- Row 2, 3, 5, 6 → `ErrMessageOwnershipViolation` (with new
  `ConcurrentOwnersViolation` flag on row 3 — see §3a)
- Row 7 → `ErrMessageOwnershipInconclusive` (new sentinel)
- Row 8 → `ErrMessageOwnershipUnobserved` (new sentinel)

#### §3a — Concurrent-owner: violation, but specially flagged

When row 3 fires, the receipt is recorded as a violation **and** as a
concurrent-owner event (P1-1 evidence). The distinction matters for
post-hoc diagnosis: concurrent ownership is split-brain at the
assignment layer (worse than stale-receipt); the failure report should
make this visible.

Decision rationale (countered alternative): keeping concurrent-owner as
a third class (neither violation nor redelivery) would let Outcome A
pass when split-brain is actively occurring. Classifying as violation
is the right safety default.

### Step 4 — Wire `Coordinator.CurrentOwnersOf` into tracker construction

In `NewCoordinator` (coordinator.go:179), after constructing `c.tracker`,
call:

```go
c.tracker.SetOwnerLookup(c.CurrentOwnersOf)
```

This makes the discriminator live for every real run; unit tests that
construct `MessageTracker` directly remain unaffected.

### Step 5 — Add unit tests for the refined classifier

Verify-first per [memory](../../.claude/memory): write the failing tests
**before** implementing §3. File: `tracker_ownership_test.go`. New cases:

1. `TestClassify_NilLookupFallback` — preserves current behavior (legacy
   fallback row 2). orig=worker-5, receipt=worker-0, lookup nil. Expect
   `ErrMessageOwnershipViolation`.
2. `TestClassify_HandoffRedelivery_NewOwnerOnly` — orig=worker-5,
   receipt=worker-0, current=[worker-0], initialized=true. Expect
   `ErrMessageRedelivery` (row 4).
3. `TestClassify_StaleReceipt_OriginalStillOwner` — orig=worker-5,
   receipt=worker-0, current=[worker-5], initialized=true. Expect
   `ErrMessageOwnershipViolation` (row 5).
4. `TestClassify_StrangerReceiver_NeitherAssigned` — orig=worker-5,
   receipt=worker-0, current=[worker-3], initialized=true. Expect
   `ErrMessageOwnershipViolation` (row 6).
5. `TestClassify_ConcurrentOwners_CurrentAndThird` — orig=worker-5,
   receipt=worker-0, current=[worker-0, worker-9], initialized=true.
   Expect `ErrMessageOwnershipViolation` AND the
   ConcurrentOwnersViolation flag (row 3, P0-2).
6. `TestClassify_ConcurrentOwners_AllThree` — orig=worker-5,
   receipt=worker-0, current=[worker-5, worker-0, worker-9],
   initialized=true. Expect `ErrMessageOwnershipViolation` AND
   ConcurrentOwnersViolation flag (row 3).
7. `TestClassify_OwnershipInconclusive_SnapshotInitialized` —
   orig=worker-5, receipt=worker-0, current=[], initialized=true.
   Expect `ErrMessageOwnershipInconclusive` (row 7).
8. `TestClassify_OwnershipUnobserved_SnapshotUninitialized` —
   orig=worker-5, receipt=worker-0, current=[], initialized=false.
   Expect `ErrMessageOwnershipUnobserved` (row 8).
9. `TestClassify_SameWorkerRedelivery` — orig=worker-5, receipt=worker-5,
   current=[worker-5]. Expect `ErrMessageRedelivery` (row 1).
10. `TestClassify_PartitionScenarioFromPhase4` — reproduce the exact
    partition=19 seq=153 chaos_gate finding with `currentOwners` mocked
    to [worker-0] (single new owner). Expect handoff redelivery.

Plus coordinator-level tests (new file `coordinator_owners_test.go`):

11. `TestCurrentOwnersOf_BeforeAnyAssignment` — fresh coordinator, no
    `AssignmentReport` consumed. Expect `owners=[], initialized=false`.
12. `TestCurrentOwnersOf_AfterFirstAssignment` — ingest one
    AssignmentReport for worker-0 covering partition 7. Expect
    `owners=["worker-0"], initialized=true`.
13. `TestCurrentOwnersOf_ConcurrentIngestion` — race-detector test
    running `CurrentOwnersOf` concurrently with `AssignmentReport`
    ingestion. Must pass under `-race`.

### Step 6 — Re-run chaos_gate.yaml under refined classifier

After §1–§5 land:

```bash
go build -o test/simulation/bin/simulation ./test/simulation/cmd/simulation
./test/simulation/bin/simulation \
  --config test/simulation/configs/chaos_gate.yaml \
  --duration 5m --cooldown 30s --stop-on-failure=false \
  --failure-report tmp/sim_phase5_gate_run1.json
```

**Outcome rules (revised post-review):**

Outcome A criteria (all required):
1. `OwnershipViolations` count == 0
2. `ConcurrentOwnerEvents` count == 0
3. `InconclusiveOwnerEvents` count == 0
4. `OwnershipUnobservedPostChaosCount` == 0 (cold-start unobserved
   arrivals are tolerated via the separate `OwnershipUnobservedPreChaos-
   Count`; arrivals after the first ChaosEvent has fired must all be
   classifiable). Split-counter design avoids relying on absolute
   timestamps in the failure report.
5. `TotalSent == TotalReceived` (existing invariant)

To make criterion 4 auditable, `FailureReport` records
`FirstChaosEventAt` (a timestamp set by the chaos dispatch loop on the
first event) and exposes both pre- and post-chaos counters separately.

Implementation (precise contract):

- Coordinator gains `MarkChaosStarted()` and an internal
  `chaosStarted atomic.Bool` (plus `firstChaosAt time.Time` guarded by
  one-time `sync.Once` so the timestamp is captured exactly once).
- The chaos dispatch loop calls `coord.MarkChaosStarted()` **before**
  invoking the chaos handler for the very first event. The exact call
  site is the chaos goroutine in `cmd/simulation/main.go` immediately
  before the `handleChaosEvent(...)` / `handleGoroutineChaos(...)`
  dispatch on the first iteration (gated by a local `firstEvent bool`
  to avoid per-event overhead).
- The classifier reads `chaosStarted.Load()` while incrementing the
  unobserved counter. Since `MarkChaosStarted()` uses `Store(true)`
  with release semantics and the classifier uses `Load()` with acquire
  semantics, the ordering is well-defined: **any receipt classified
  after `MarkChaosStarted()` returns lands in the post-chaos bucket.**
- The classifier holds `t.mu` for the bucket-increment step; the
  `atomic.Bool` is read without holding `t.mu` (it can't deadlock with
  `MarkChaosStarted` because `MarkChaosStarted` is lock-free).
- For receipts in flight at the moment `MarkChaosStarted` is called,
  the bucket assignment is deterministic per the atomic ordering above:
  any receipt whose classifier-side `Load()` returns `true` is
  post-chaos, regardless of when the receipt physically arrived.
  Acceptable: the gate is on aggregate count of unobserved post-chaos
  events, and the "exactly-on-the-boundary" receipt is post-chaos by
  construction.

- **A. All five criteria met** → no exclusivity-violation signal
  detectable by this classifier under this workload. This is consistent
  with (but does not by itself prove) the hypothesis that the prior
  Phase 4 finding was a classifier false positive. **Caveat for H2-class
  bugs**: a heartbeat-watcher / resolver failure where the receiving
  worker is the sole current owner per `workerAssignments` could
  produce a row-4 redelivery classification that hides a real gate
  admission bug (see Risk 5). Outcome A → enable gate-on CI at the
  `chaos_gate.yaml` workload (5m duration, 3-retry budget matching the
  existing simulation-stress pattern). Do **NOT** enable gate-on at
  `chaos_comprehensive.yaml` — that requires H2 first (out of scope).
- **B. Any of criteria 1–4 nonzero** → real signal. Inspect
  `tmp/sim_phase5_gate_run1.json`:
  - `OwnershipViolations` (rows 2, 5, 6) → potential gate gap. Capture
    evidence; file as separate `docs/plans/parti-exclusivity-bug.md`.
  - `ConcurrentOwnerEvents` (row 3) → assignment-layer split-brain.
    Most likely a leader-election bug. Capture; file separately.
  - `InconclusiveOwnerEvents` (row 7) → discriminator staleness too
    high to gate on. Investigate snapshot publication latency; consider
    adding KV-source-direct lookup as a fallback.
  - `OwnershipUnobservedCount > 0` after first chaos event → cluster
    isn't reporting assignments fast enough. Likely worker startup
    sequence issue.
- **C. Mixed signals** → classify each evidence record; pick A or B per
  finding.

Do not enable gate-on CI in case B or C.

### Step 7 — Update chaos_gate.yaml header

Replace the current "two hypotheses" section with one of:

- **Outcome A**: a note that the prior finding was a classifier false
  positive (origWorker != workerID under legitimate handoff). Reference
  this phase's plan.
- **Outcome B/C**: a description of the actual bug class found, with a
  pointer to the separately filed plan.

### Step 8 — Linter, unit tests, race-detector, full local 5m dry-run

Required gates:
- `make lint` — clean.
- `make test` — passes.
- `go test -race ./test/simulation/internal/coordinator -run 'CurrentOwners|RecordReceived|Classify'`
  — passes (catches §1 race regressions).
- §6 5m chaos_gate.yaml run — Outcome A criteria 1–5, or documented
  Outcome B/C with a separately filed investigation plan.

## Risk surface

**Risk 1 — Worker-reported snapshot is not leader-authoritative.**
`workerAssignments` reflects each worker's most-recent
`OnAssignmentChanged` view, not the leader's KV state. There is an
inherent lag. The plan handles this by treating `len(currentOwners) == 0`
post-baseline as `OwnershipInconclusive` rather than auto-classifying
as redelivery — so a stale-snapshot window doesn't silently hide a real
bug. **If `InconclusiveOwnerEvents > 0` consistently**, we know the
snapshot lag is too large to gate on, which is itself a useful signal.

**Risk 2 — Atomic snapshot rebuild cost.** Every `AssignmentReport`
triggers a full rebuild of the `perPartition` map. With ~10 workers and
~50 partitions, that's ~500 entries; in practice assignment reports
arrive at ChaosEvent cadence (15-25s) plus startup, so the rebuild is
cheap. No additional concern.

**Risk 3 — Outcome A still requires interpretation.** Even with the
four-counter gate, a quiet 5m run with no chaos-driven reassignment
events would pass trivially. Mitigation: chaos_gate.yaml has
`min_workers=4, max_workers=8` and 20-30s chaos interval over 5m =
10–15 events. Each event triggers reassignment, so the classifier will
exercise the discriminator on every receipt of a redelivered message.

**Risk 4 — Outcome B (real bug) extends scope.** If §6 surfaces a real
gate bug, the work shifts to library-level investigation. Mitigation:
Phase 5 lands the oracle improvements regardless; the bug investigation
becomes a separately-tracked plan. Phase 5 is still mergeable on its
own.

**Risk 5 — H2-induced gate gap could land in row 4 (handoff redelivery)
and be invisible.** The owner snapshot is worker-reported, not
leader-authoritative. Scenario:

1. H2 heartbeat-watcher bug fails to detect that worker-5 is stuck or
   dead.
2. Resolver/leader reassigns partition 19 to worker-0 (incorrectly,
   because worker-5 still nominally owns it).
3. worker-0 publishes its new `OnAssignmentChanged` snapshot covering
   partition 19; coordinator's `workerAssignments[worker-0]` now
   contains partition 19.
4. worker-5 — being stuck/dead — never publishes a fresh snapshot, but
   if it had a `WorkerStopped` event the coordinator pruned it
   (coordinator.go:1130). If neither happens (silent stall), worker-5
   may also still appear as an owner → row 3 ConcurrentOwners (caught).
5. If worker-5 was pruned for any reason (stoppedWorkersCh fired),
   `currentOwners == [worker-0]` → row 4 redelivery (false negative).

This is a real blind spot. Phase 5's outcome statement is therefore
softened (§6 Outcome A): "no exclusivity-violation signal detectable
by this classifier under this workload" — not "no bug exists". An H2
fix is a Phase 6+ prerequisite for a confident library-level
exclusivity claim. The dev-only `chaos_gate.yaml` CI job is still
useful because cases 1–3 (violation, ConcurrentOwners, Inconclusive)
catch a large class of real bugs; the H2 blind spot is one well-defined
class of misses.

## Acceptance criteria

1. `MessageTracker.SetOwnerLookup(func(int) ([]string, bool))` exists;
   nil-safe.
2. `Coordinator.CurrentOwnersOf(partitionID int) ([]string, bool)` exists.
3. `NewCoordinator` wires `c.tracker.SetOwnerLookup(c.CurrentOwnersOf)`.
4. `classifyKnownDuplicateLocked` respects the §3 table.
5. `FailureReport` includes `OwnershipViolations`, `ConcurrentOwnerEvents`,
   and `InconclusiveOwnerEvents` (each bounded by
   `ownershipViolationsCap`); plus split counters
   `OwnershipUnobservedPreChaosCount` and
   `OwnershipUnobservedPostChaosCount` (no slices); plus
   `FirstChaosEventAt time.Time` for auditability.
6. All 13 new tests pass; all existing tests still pass.
7. `make lint` clean; `go test -race` for the coordinator package passes.
8. Re-run §6 shows Outcome A (criteria 1–5 met) or B/C with a documented
   finding.
9. If Outcome A: gate-on CI job added for **chaos_gate.yaml only**
   (5m duration). chaos_comprehensive.yaml gate-on remains deferred.
10. `chaos_gate.yaml` header updated (§7) and
    `tmp/sim_phase5_known_issues.md` records the pre-existing
    coordinator.go:402 race for follow-up.

## Test plan

- Unit: 10 classifier tests (Step 5 #1-10).
- Unit: 3 coordinator owner-snapshot tests (Step 5 #11-13), including
  a `-race` test for concurrent ingestion + read.
- Local: 5m chaos_gate.yaml run as the merge gate (§6).
- Existing: full `make test` and `make lint` pass.

## Commit message (draft)

The outcome paragraph (B/C vs A) is written from the actual §6 run
results, not pre-decided.

```
feat(simulation): Phase 5 — refine ownership-violation classifier with
current-owner discriminator

The Phase 3 classifier flagged any same-(partition, seq) receipt from a
different worker as an ownership violation. Under JetStream's
at-least-once semantics, legitimate redelivery-after-reassignment
necessarily produces such receipts and is NOT a violation.

Refine the classifier to consult the leader-reported assignment
snapshot (coordinator.workerAssignments, published via atomic.Pointer
to a frozen ownerSnapshot) at the moment of receipt. The classifier
now distinguishes five cases:
- Redelivery (same worker or handoff to a single new owner)
- OwnershipViolation (stale, stranger, or ownerLookup-nil fallback)
- ConcurrentOwnersViolation (split-brain: snapshot reports >1 owner)
- OwnershipInconclusive (mid-handoff after baseline; snapshot empty)
- OwnershipUnobserved (cold start; snapshot uninitialized)

New API:
- MessageTracker.SetOwnerLookup(func(int) ([]string, bool))
- Coordinator.CurrentOwnersOf(partitionID int) ([]string, bool)

FailureReport gains ConcurrentOwnerEvents and InconclusiveOwnerEvents
slices (bounded) plus OwnershipUnobservedCount; OwnershipViolations[i]
gains CurrentOwners for post-hoc analysis.

[Outcome paragraph filled in from §6 run results.]
```
