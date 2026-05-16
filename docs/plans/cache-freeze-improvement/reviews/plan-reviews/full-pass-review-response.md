# Response: Full Pass Review

## Summary

All six findings accepted. No pushbacks. Reviewer explicitly
states no new P0 architectural change is needed — these are all
precision issues, several of which were mistakes I introduced in
the most recent renumbering and migration-text edits. Plan
refactored in `docs/plans/cache-freeze-improvement/00-original-plan.md`.

## Per-finding actions

### P1 — Heartbeat migration text dual-format
- Bidirectional-tolerance subsection rewritten: KV evolution
  split into two categories. `assignment.*` keys remain
  JSON-additive; **`heartbeat.<W>` keys are dual-format, not
  JSON-additive.** Explicit text now says old workers serialize
  raw timestamp bytes, not Heartbeat objects without new fields.
- KV schema table row for `heartbeat.<W>` updated to label it
  `(dual-format)` with v1 JSON and legacy timestamp described
  separately in the writer/reader columns.
- §3.5 mixed-version exposure rewritten: removed the false
  claim that old workers report `AppliedVersion=V` via
  heartbeat. The new text correctly says legacy heartbeats
  carry no ack fields and recovery does not depend on audit
  drift detection for legacy workers; it relies on the next
  successful publish unconditionally overwriting the alias.
- Test #60 (`AliasBarrier_CASFailureAfterAliases`) gained an
  additional assertion that legacy timestamp heartbeats of the
  affected workers do NOT show `AppliedVersion=V`.

### P1 — Missing post-alias leadership fence (step 7)
- Restored as step 7 of the publish flow. Step list now reads:
  `1 snapshot → 2 assign → 3 set-equality → 4 payload writes →
  5 pre-alias fence → 6 alias barrier → 7 post-alias fence →
  8 build commit → 9 commit CAS → 10 commit log → 11 best-effort
  commit-capable alias → 12 GC`.
- Post-alias fence aborts before commit CAS on leadership loss;
  documented mixed-version exposure paragraph already
  acknowledged this window.
- New test #61: `TestPublisher_PostAliasLeadershipLoss_AbortsBeforeCommitCAS`
  asserts the post-alias check fires and the commit CAS is
  never attempted (no `commit_aborts` metric increment).

### P1 — Dual-read contradicted in F2 mapping and recovery
- F2 mapping bullet rewritten: "`assignment.<W>` before
  `commit.V`" no longer says "not applicable to new workers". It
  explicitly says handled by the dual-read source-of-truth rule
  (apply via legacy-compat path when no usable commit or alias
  is fresher; ignore when a fresher commit exists).
- New-leader-recovery pseudocode comment updated: new workers
  continue dual-read fallback until the first new commit lands;
  they do NOT blindly wait if a valid legacy alias is fresher.

### P1 — Source API surface explicit
- Greatly expanded the "API surface summary" section. Every
  identifier referenced anywhere in the plan or tests now has a
  concrete Go signature, organized by package:
  - `types`: `RevisionedPartitionSource` (optional extension
    interface, NOT added to `PartitionSource`), `Partition.CanonicalID`,
    `DecodeHeartbeat`, `Cap*` constants.
  - `source` (NatsKV): `Snapshot`, `Modify`, `AddPartitions`,
    `RemovePartitions`, `WithReconcileInterval`,
    `WithLeadershipProbe`.
  - `manager`: `SetCapability`, `Capabilities`.
- Calculator usage pattern for the type-assert-with-fallback
  spelled out.
- New tests #62-65 cover the new APIs:
  - `TestNatsKV_AddPartitions_UsesModifyAndPreservesConcurrentAdds`
  - `TestNatsKV_RemovePartitions_UsesModifyAndPreservesConcurrentMutations`
  - `TestCalculator_RevisionedSourceUsesSnapshot_NonRevisionedSourceFallsBackToList`
  - `TestNatsKV_ReconcileInterval_LeadershipProbeSelectsLeaderFollowerCadence`

### P1 — `AppliedSourceRevKnown=false` skip clarification
- §4.1 sentence "Audit's source-revision check skips workers
  whose `AppliedSourceRevKnown=false`" rewritten to specify:
  the skip applies **only when** `commit.SourceRevisionKnown=false`
  (e.g., commit from a Static source). When the commit declares
  `SourceRevisionKnown=true`, an ack-capable worker reporting
  `AppliedSourceRevKnown=false` is classified `behind`, not
  skipped. Closes the loophole where a buggy worker could
  trivially pass audit by omitting the known bit.
- Existing test #50 already encodes this correctly; no test
  addition needed.

### P2 — Step number drift in §3.7
- Swept stale references: §3.7 prose now correctly says steps
  6 and 11 (alias barrier and best-effort commit-capable alias)
  instead of the old 5 and 10. KV schema table row likewise
  updated.

## Test plan

Total tests grew from 63 to 68 entries. The five additions
(post-alias leadership, AddPartitions/RemovePartitions,
revisioned source fallback, leadership probe cadence) are all
review-required. Test #60 received a clarifying assertion. End-
to-end invariant tests renumbered #66-68.

## Plan status

Reviewer's verdict: "after that cleanup, the plan is ready to
dispatch by phase." The cleanup is now applied. Implementation
can proceed per
`docs/plans/cache-freeze-improvement/02-implementation-strategy.md`, with the
Prompt 2 (precision-pass) reviewer-prompt available as a final
sanity check if desired before phase 1 dispatch.
