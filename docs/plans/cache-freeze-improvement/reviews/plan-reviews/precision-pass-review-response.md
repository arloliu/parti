# Response: Precision Pass Review

## Summary

All six findings accepted. No pushbacks. The reviewer's verdict
("plan is very close, one short cleanup pass before phase 1
dispatch") is now satisfied. Specific edits below.

## Per-finding actions

### P1 — Top-level rollout text dual-format
- Scope summary (line ~120): split into two bullets — `assignment.*`
  keys remain JSON-additive; `heartbeat.<W>` keys are explicitly
  **dual-format**. Old code writes raw RFC3339 timestamp bytes;
  new code writes v1 JSON; `DecodeHeartbeat` accepts both.
- Rollout/risk bullet: rewrote the "additive; old peers treat
  unknown JSON fields as zero" phrasing into two clear cases
  (`assignment.*` keys not read by old peers; heartbeat keys
  handled by the dual decoder).

### P1 — Source API surface ambiguities

**`WithUpdateRetries` ambiguity** — resolved by **keeping it as
public API**. Added to the API surface summary block:
```go
func WithUpdateRetries(n int) NatsKVOption                      // CAS-retry budget for Update() / Modify() (default 5)
var ErrUpdateRetryExhausted error
```
Also added test #67 `TestNatsKV_WithUpdateRetries_ExhaustionReturnsTypedError`.

**Publish flow step 1 fallback** — rewrote step 1 to explicitly
type-assert `RevisionedPartitionSource` and fall back to `List`
with `srcRev=0, srcKnown=false`. Comment notes that the
calculator must never call `Snapshot` directly on
`types.PartitionSource`.

### P2 — Strategy doc stale references
- "publish step 5" → "publish step 6" (and clarified that the
  alias barrier is flanked by leadership rechecks at steps 5
  and 7).
- "tests #46-53" → "tests #47-54, #55-61, #62-67, #68-70" with
  section labels.
- Worked Phase 1 dispatch input list: dropped "§3.4
  (RevisionedPartitionSource)" — that section number doesn't
  exist. Replaced with pointer to the API surface summary
  block, which is where the interface signature now lives.
- "about 45 tests" → "about 70 tests" in the intro.

### P2 — Test plan numbering deduped
- CAS/write-path section ends at #5 (was #5).
- Reconcile/read-path section shifted to start at #6, run to #8.
- All subsequent sections shifted by +1 to match. Final
  numbering: 1..70 contiguous, no duplicates, verified with
  Python check script.
- Total tests: 70 (was 68 + 1 for `WithUpdateRetries` + 1
  renumbering shift).

### P2 — Documentation section expanded
- Phase 7 documentation requirements rewritten to enumerate
  every additive public API by package:
  - **source**: `Modify`, `AddPartitions`, `RemovePartitions`,
    `Snapshot`, `WithReconcileInterval`, `WithLeadershipProbe`,
    `WithUpdateRetries`; concurrent-updates subsection;
    revisioned-vs-static-source subsection.
  - **types**: `RevisionedPartitionSource`, `Partition.CanonicalID`,
    `DecodeHeartbeat`, capability constants, `Heartbeat`,
    `Assignment`, `AssignmentCommit`, `AssignmentPayloadRef`.
  - **manager**: `SetCapability`, `Capabilities`.
- Godoc requirements expanded to cover each new symbol with
  specific guidance (CAS semantics for `Update`, dedupe
  semantics for `Modify` family, dual-format behaviour for
  `DecodeHeartbeat`, runtime-not-config rule for
  `SetCapability`, optional-interface contract for
  `RevisionedPartitionSource`).
- `CHANGELOG.md` entry added to the list.

### P2 — Code-grounding references
- §4.2 audit pseudocode comment: replaced
  `jsutil/consumer.go processing/pull gate` with the actual
  paths — gate wiring at `internal/durable/worker_consumer.go:382-387`,
  gate implementation at `internal/durable/processing_gate.go`.
- §4.4 "two paths today store assignment before Apply" intro
  rewritten with correct line ranges:
  - Update-time path: `manager_assignment.go:365-377`
    (storage) and `:385-389` (Apply).
  - Initial-assignment path: `manager.go:374-386` (wait →
    events → `applyInitialHandoffAsync` → `StateStable`).

## Status

Plan is now precision-clean. Numbering contiguous 1..70; no
JSON-additive contradictions for heartbeat; API surface
complete; strategy doc references current. Phase 1 dispatch
unblocked per
`docs/plans/cache-freeze-improvement/02-implementation-strategy.md`.

Optional last step: one more precision-pass invocation
(Prompt 2 in `docs/plans/cache-freeze-improvement/reviews/plan-reviews/reviewer-prompts.md`)
to confirm no regressions slipped in during this cleanup. If
the reviewer agrees, implementation can start.
