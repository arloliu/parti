# Review Response — Round 1 (Phase 3a)

Source: `tmp/sim-oracle-phase3_review.md`.

## P0 — Empty WorkerID fallback contradicted by pseudocode (accepted)

The classification arm `case !known` only triggered when the seq was pruned.
Restore replay (`checkpoint.go:170-183`) passes `""` and the
contiguous-advance/out-of-order paths would call `recordWorkerForSeq(..., "")`,
populating `lastWorkerPerSeq[pid][seq] = ""`. A later real-worker duplicate
would then be `known=true`, `origWorker=""`, `current!=""` → false-positive
ownership violation.

**Fix applied**:
- Classification arm widened to `case !known, origWorker == "", workerID == ""`.
- `recordWorkerForSeq` is a no-op for empty workerID (plan now explicitly
  states this).
- New TDD test #7 (`TestCheckpointRestore_EmptyWorkerID_FallsBackToLegacyDuplicate`)
  guards the restore-replay path.

## P1 — Redelivery metric has no coordinator-visible signal (accepted)

The original pseudocode returned `nil` for redelivery, so the coordinator's
`if err != nil` dispatch couldn't fire `RecordRedelivery()`.

**Fix applied**:
- Introduce a `MessageRedeliveryEvent` typed value with `Error()`/`Unwrap()`
  that satisfies the error interface, and a sentinel
  `ErrMessageRedelivery` for `errors.Is` dispatch.
- Coordinator wiring section now shows two new dispatch arms (redelivery
  + violation) added at the top of the existing `if err != nil` block,
  with explicit note that the redelivery arm does NOT propagate to
  stop-on-failure or DupTracer.
- Plan now documents that "error" semantically means "event of note" not
  "call failed" for these two cases.

## P1 — TDD #5 doesn't actually prove out-of-order worker recording (accepted)

The reviewer's reasoning: my test #5 set seq=2 then seq=1 then duplicated
seq=1; the lookup of seq=1 succeeds via the contiguous-advance path, not
the out-of-order path. A broken implementation that omits worker recording
in the out-of-order branch would still pass.

**Fix applied**: redesigned #5 to set seq=2 via out-of-order, advance via
seq=1, then duplicate **seq=2** (not seq=1). The seq=2 lookup must come
from the out-of-order branch's recording; if that recording is missing,
the duplicate falls back to a plain `MessageDuplicateError` (test fails)
rather than the expected `MessageOwnershipViolationError`. Added an
explicit parent-fail table at the end of the TDD section listing which
tests are the critical regression guards.

## P1 — WorkerCacheMaxPerPartition needs config default + validation (accepted)

**Fix applied**: explicit "Config changes required" subsection in the
Pruning section lists the three files that need edits:
`config.go` (new field), `defaults.go` (default=4096), `validation.go`
(reject ≤0). Plan also explicitly notes that zero is meaningless.

## P2 — Same-ID restarts classified as redelivery, not violation

Accepted as documented behavior. The plan now states that "same logical
worker-ID, different instance" is treated as redelivery — matches parti's
own stable-worker-ID semantics.

## P2 — Pruning trade-off (detection horizon)

Accepted as documented. The plan now describes the cache as a
"guaranteed-detection lookback, not full-history ownership proof."

## P2 — DupTracer remains legacy-only

Accepted as documented. Plan's Coordinator wiring section now explicitly
states that redeliveries and ownership violations bypass DupTracer, and
explains the rationale (the FailureReport ownership-violations list is
the structured replacement; DupTracer remains useful for the legacy
pruned-duplicate path).
