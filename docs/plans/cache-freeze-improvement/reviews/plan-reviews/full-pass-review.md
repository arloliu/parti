# Full Pass Review

## Summary

The main architecture is now strong: refs-always payloads, CAS-fenced commits, dual-read migration, heartbeat apply receipts, and capability-gated audit escalation are the right shape for this P0 invariant. I did not find a new P0 design flaw in the settled protocol. The plan is still not quite implementation-ready because several sections contradict the corrected design and a few source/API contracts are referenced without exact signatures. Fix the P1 items below before handing this to implementation agents.

## Findings

### P0 — None Found

No fresh P0 invariant violation found in the current architecture. The remaining issues are implementation hazards: stale migration text, missing step detail, and underspecified API surface.

### P1 — Heartbeat Migration Text Still Contradicts The Dual Decoder

The detailed heartbeat section is now correct: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1327) specifies a dual decoder for v1 JSON and legacy RFC3339 timestamp bytes. That matches current code: [internal/heartbeat/publisher.go](internal/heartbeat/publisher.go#L228) writes `time.Now().Format(time.RFC3339Nano)` and [internal/heartbeat/publisher.go](internal/heartbeat/publisher.go#L231) stores those raw bytes.

But the migration section still says all new KV fields are JSON-additive and that old workers serialize heartbeat objects without the new fields: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1953) and [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1971). The mixed-version exposure section also says an old worker's heartbeat reports `AppliedVersion=V` after an alias-only uncommitted batch: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1055). A legacy worker cannot report that field; it only reports a timestamp.

Failure case: an implementer working from the migration section can incorrectly JSON-unmarshal legacy heartbeats, omit live old workers from `legacy_in_batch`, or rely on audit drift detection that cannot exist for old workers. That can break the mandatory alias barrier in the exact rolling-upgrade path the plan is trying to protect.

Recommended fix:

- Change the migration section to say heartbeat keys are dual-format, not JSON-additive.
- Phrase the zero-field legacy heartbeat as the output of `DecodeHeartbeat(timestampBytes)`, not as what old workers serialize.
- Remove or rewrite the claim that old workers report `AppliedVersion` during alias-published-but-commit-failed exposure.
- Update test #60 expectations so it documents the migration floor accurately: old workers may be unverifiable until a later successful alias/commit overwrites them; audit cannot prove their applied version from heartbeat fields.

Suggested tests are mostly already present (#54-58). Add one assertion to #60 that legacy timestamp heartbeats do not provide `AppliedVersion` and therefore are not used as proof of recovery.

### P1 — Publish Flow References A Post-Alias Leadership Check That Is Not In The Steps

The publish algorithm now has a pre-alias leadership fence, which is good. But the numbered flow jumps from step 6 directly to step 8: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L928-L999). Later prose says both pre-alias and post-alias leadership rechecks exist at steps 5 and 7: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1038), and test #60 refers to leadership being lost between step 7 and step 9: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L696).

Failure case: an implementation agent may add only the pre-alias fence and rely on commit CAS for the rest. The CAS still protects `assignment._commit`, but the legacy alias-visible-uncommitted window becomes unnecessarily wide for old workers. That is exactly the window the response said should be shrunk by a post-alias recheck.

Recommended fix:

```text
7. Post-alias leadership fence:
   read electionKV.leader again; assert revision == claimed LeaderRevision R.
   On mismatch: abort before building/writing assignment._commit.
   The aliases already written are documented mixed-version exposure;
   do not attempt commit CAS after observing leadership loss.
```

Then renumber or update all step references so the alias barrier is step 6, post-alias check is step 7, commit is step 9, commit log is step 10, and best-effort commit-capable aliases are step 11.

Suggested test addition: make `TestPublisher_AliasBarrier_CASFailureAfterAliases_DocumentedMigrationExposure` assert that a detected post-alias leadership loss aborts before attempting the commit CAS.

### P1 — Dual-Read Fallback Is Still Contradicted In Two Places

The primary rule is correct: new workers watch both `assignment._commit` and `assignment.<W>` and use `LeaderRevision` to select the source of truth. That is stated at [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1064) and again in §3.7.

Two stale passages still contradict it:

- The F2 mapping says `assignment.<W>` arriving before `commit.V` is not applicable to new workers because they ignore aliases entirely: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1168-L1170).
- The new-leader recovery pseudocode says existing legacy assignment keys remain applicable to old workers while new workers wait for the first commit: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1221).

Failure case: old leader + new worker, or new-leader to old-leader bounce, regresses to the orphan path already identified in earlier reviews. A new worker that ignores a fresher legacy alias can sit idle on partitions the old leader assigned to it.

Recommended fix:

- Replace the F2 mapping bullet with: `assignment.<W>` before `commit.V` is handled by the dual-read source-of-truth rule; it is applied only when no usable commit exists or the alias has a fresher `LeaderRevision`.
- Replace the recovery comment with: new workers continue the dual-read fallback until the first new commit lands; they do not blindly wait if a valid legacy alias is fresher.

The existing rolling-upgrade tests #46-48 cover this once the text is made consistent.

### P1 — Source API Surface Is Referenced But Not Fully Specified

The plan scopes source write safety to callers using `Modify` / `AddPartitions` / `RemovePartitions`: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L54) and [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L127). But the API summary only defines `Modify` and `WithReconcileInterval`: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L395). The test plan also names `TestE2E_AddPartitionsConvergesToInvariant`: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L704), and the strategy doc says docs should cover `AddPartitions` / `RemovePartitions`: [docs/plans/cache-freeze-improvement/02-implementation-strategy.md](docs/plans/cache-freeze-improvement/02-implementation-strategy.md#L34).

A similar gap exists for source revisions. The publish flow calls `source.Snapshot()`: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L929), and the text references `RevisionedPartitionSource`, but the plan never gives the exact Go interface signature. Current `types.PartitionSource` only has `Start`, `List`, and `Stop`: [types/partition_source.go](types/partition_source.go#L16). Finally, polling cost says leader/follower interval selection uses `WithLeadershipProbe(func() bool)`: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1820), but §2.4/API summary only defines `WithReconcileInterval`.

Failure case: different phase agents can implement different public APIs. One may omit `AddPartitions`, another may write tests against it; one may call `Snapshot` unconditionally on `types.PartitionSource`; another may use an unexported interface. That is churn at best and a source-revision bug at worst.

Recommended fix: add an explicit source API block before implementation starts. Either remove `AddPartitions` / `RemovePartitions` from the invariant scope and tests, or define them concretely, for example:

```go
type RevisionedPartitionSource interface {
    types.PartitionSource
    Snapshot(ctx context.Context) ([]types.Partition, uint64, bool, error)
}

func (s *NatsKV) Snapshot(ctx context.Context) ([]types.Partition, uint64, bool, error)
func (s *NatsKV) AddPartitions(ctx context.Context, partitions ...types.Partition) error
func (s *NatsKV) RemovePartitions(ctx context.Context, partitions ...types.Partition) error
func WithLeadershipProbe(fn func() bool) NatsKVOption
```

Then state the fallback explicitly in the calculator: if `Source` implements `RevisionedPartitionSource`, call `Snapshot`; otherwise call `List` and publish `SourceRevisionKnown=false`.

Suggested tests:

```text
TestNatsKV_AddPartitions_UsesModifyAndPreservesConcurrentAdds
TestNatsKV_RemovePartitions_UsesModifyAndPreservesConcurrentMutations
TestCalculator_RevisionedSourceUsesSnapshot_NonRevisionedSourceFallsBackToList
TestNatsKV_ReconcileInterval_LeadershipProbeSelectsLeaderFollowerCadence
```

### P1 — `AppliedSourceRevKnown=false` Is Still Described As A Skip In One Section

The audit pseudocode correctly requires a known applied source revision whenever the commit has `SourceRevisionKnown=true`: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1563-L1565). Test #50 also encodes that an ack-capable worker omitting `AppliedSourceRevKnown` for a known commit is behind.

But §4.1 still says audit skips workers whose `AppliedSourceRevKnown=false`: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1418-L1422). That statement is only true when the commit itself has `SourceRevisionKnown=false`.

Failure case: an implementer following §4.1 can accidentally let a buggy new worker pass audit by publishing `AppliedVersion`, `AppliedDigest`, and `AppliedSourceRevKnown=false` for a known-revision commit. That would weaken the source-revision part of the applied invariant.

Recommended fix:

```text
Audit skips the source-revision comparison only when commit.SourceRevisionKnown=false.
When commit.SourceRevisionKnown=true, an ack-capable worker must report
AppliedSourceRevKnown=true and the exact AppliedSourceRevision; otherwise it is behind.
```

The existing `TestAudit_KnownCommitRequiresKnownAppliedSourceRevision` is the right test.

### P2 — Step References Drift Around The Publish Flow

Some prose still points at old step numbers. For example, §3.7 calls the mandatory legacy alias barrier step 5 and the best-effort commit-capable alias step 10: [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1187) and [docs/plans/cache-freeze-improvement/00-original-plan.md](docs/plans/cache-freeze-improvement/00-original-plan.md#L1191). In the current flow, step 5 is the pre-alias fence, step 6 is the alias barrier, step 10 is commit-log write, and step 11 is best-effort commit-capable alias write.

This is not a separate correctness issue once the missing step 7 is added, but it will waste reviewer and implementer time. Do a numbering pass after inserting the post-alias leadership check.

## Additional Tests To Add

Most invariant tests are already named in the plan. I would add or tighten only these:

```text
TestPublisher_PostAliasLeadershipLoss_AbortsBeforeCommitCAS
TestNatsKV_AddPartitions_UsesModifyAndPreservesConcurrentAdds
TestNatsKV_RemovePartitions_UsesModifyAndPreservesConcurrentMutations
TestCalculator_RevisionedSourceUsesSnapshot_NonRevisionedSourceFallsBackToList
TestNatsKV_ReconcileInterval_LeadershipProbeSelectsLeaderFollowerCadence
```

Also tighten test #60 so it does not claim legacy timestamp heartbeats prove an old worker's applied version.

## Verdict

Do not start implementation yet, but this is close. No new P0 architecture change is needed. Before implementation, fix the P1 precision issues: make all heartbeat migration text dual-format, insert the explicit post-alias leadership check, remove remaining alias-ignore contradictions, define the source/revision API surface exactly, and correct the `AppliedSourceRevKnown` wording. After that cleanup, the plan is ready to dispatch by phase.
