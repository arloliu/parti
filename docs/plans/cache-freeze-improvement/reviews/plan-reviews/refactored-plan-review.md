# Review: Refactored Partition Assignment Robustness Plan

## Summary

The refactored plan is much stronger than the previous revision. The refs-always design is coherent, content-addressed payloads are now based on canonical payload bytes, the commit-driven worker state machine is much clearer, and audit escalation is correctly tied to end-to-end capabilities rather than only the manager two-phase flag.

The remaining blockers are mostly mixed-version behavior and a few consistency gaps. The biggest issue: new workers cannot ignore legacy `assignment.<worker>` aliases during a rolling upgrade while an old leader may still be active. That creates an actual orphan path and is worse than the current behavior.

## Findings

### P0 - New Workers Ignoring `assignment.<worker>` Breaks Rolling Upgrade With Old Leaders

The plan says new workers watch only `assignment.commit` and ignore the legacy alias. That is correct after the first new leader commit exists, but unsafe during a K8s rolling upgrade.

Failure case:

1. Cluster has an old leader and some new workers.
2. Old leader never writes `assignment.commit`.
3. Old leader observes new worker heartbeat and publishes assignments to `assignment.<worker>`.
4. New worker ignores `assignment.<worker>` because the new plan treats commit as the only source of truth.
5. Partitions assigned to the new worker are not applied.

This is worse than today and can orphan partitions during rollout.

Recommended fix:

- New workers should run a dual-read compatibility mode until they have observed a valid new-schema commit.
- If no valid `assignment.commit` exists, new workers must watch and apply legacy `assignment.<worker>` assignments with `SchemaVersion=0` using the legacy gate plus `LeaderRevision` fencing.
- If a legacy alias arrives from a leader revision newer than the last observed commit leader revision, treat the cluster as temporarily legacy-led and apply the alias through the apply-then-store-then-ack path.
- Once a valid commit with `SchemaVersion>=1` is observed, commit becomes authoritative again.

Suggested state rule:

```text
if valid current commit exists and commit.LeaderRevision >= legacy.LeaderRevision:
    follow commit path
else if legacy assignment exists with SchemaVersion=0 and legacy.LeaderRevision >= lastSeenLeaderRevision:
    follow legacy compatibility path
else:
    wait/reconcile
```

This keeps no-flag-day rolling upgrades truly no-regression.

### P0 - Legacy Alias Writes Cannot Be Best-Effort While Old Workers Are Active

The plan says the new leader writes `assignment.<worker>` aliases for old-worker compatibility after the commit, and that failures are non-fatal. That is unsafe if old workers are still in the active worker set.

Failure case:

1. New leader computes commit V assigning partitions to an old worker.
2. `assignment.commit` succeeds.
3. Best-effort alias write to `assignment.<oldWorker>` fails.
4. Old worker never receives V.
5. New audit classifies old worker as `unverifiable`, trusts it, and does not detect the missed apply.

Recommended fix:

- During mixed-version operation, if a worker is legacy / not `CapAckV1`, its legacy alias write must be part of the pre-commit publish barrier.
- If any required legacy alias write fails, abort the commit and retry/rebalance.
- Best-effort alias writes are only safe once all active workers are commit-capable, or for aliases that are not required for any active legacy worker.

Practical publish ordering during migration:

```text
1. Compute refs/payloads.
2. Detect legacy active workers from heartbeat SchemaVersion/Capabilities.
3. For each legacy worker in the batch, write assignment.<worker> successfully.
4. Recheck leadership.
5. CAS-write assignment.commit.
6. For commit-capable workers, legacy aliases are optional/best-effort.
```

This means old workers may apply slightly before the commit in mixed mode, but that is already the legacy safety floor. It avoids assigning work to a legacy worker without delivering the only signal it understands.

### P1 - SourceRevision Knownness Is Still Inconsistent On Deletes

The plan introduces `SourceRevisionKnown`, which is the right fix, but Pillar 1 still says delete/purge sets revision to `0`.

For NatsKV, delete/purge events have real KV revisions. An empty source after delete is still a known source revision. It should not be encoded as `(SourceRevision=0, SourceRevisionKnown=false)`.

Recommended fix:

- Preserve the delete/purge entry revision from NATS KV.
- Set `SourceRevisionKnown=true` for NatsKV snapshots, including empty snapshots caused by delete/purge.
- Reserve `SourceRevisionKnown=false` only for non-revisioned sources such as static/legacy sources.

### P1 - Audit Should Not Treat Missing `AppliedSourceRevKnown` As Success

The audit pseudocode currently allows source revision match when either side does not know the revision. That is too lenient for ack-capable workers applying a commit with a known source revision.

Current shape:

```go
srcRevMatch := !commit.SourceRevisionKnown ||
               !hb.AppliedSourceRevKnown ||
               hb.AppliedSourceRevision == commit.SourceRevision
```

For a known-revision commit, an ack-capable worker should report the same known revision. Otherwise a broken worker could omit `AppliedSourceRevKnown` and still pass audit.

Recommended shape:

```go
srcRevMatch := !commit.SourceRevisionKnown ||
    (hb.AppliedSourceRevKnown && hb.AppliedSourceRevision == commit.SourceRevision)
```

Also update the apply receipt example so `SetAppliedAssignment` includes `AppliedSourceRevKnown: newAssignment.SourceRevisionKnown`.

### P1 - `CapProcessingGate` Needs A Concrete Wiring Path

The plan requires `CapProcessingGate` for safe audit escalation, which is correct. But the manager heartbeat publisher does not inherently know whether the consumer handler is actually wrapped with the processing gate.

Recommended fix:

Add a concrete reporting API between the consumer/updater layer and the manager heartbeat state, for example:

```go
func (m *Manager) SetProcessingGateActive(active bool)
```

or have the dynamic consumer/updater report capability status when it wires the worker consumer.

The key requirement: the bit must reflect actual runtime wire-up, not only config intent. If the gate fails to initialize or the consumer is not using the gated path, `CapProcessingGate` must be false.

### P1 - Partition Identity Needs A Collision-Safe Canonical Encoding

The plan references partition IDs for set equality and digests. Current `Partition.ID()` joins keys with `-`, but partition keys are not forbidden from containing `-`, so distinct key tuples can collide.

Example:

```text
["a-b", "c"] -> "a-b-c"
["a", "b-c"] -> "a-b-c"
```

Recommended fix:

- Use a canonical tuple encoding for all coverage/digest/dedupe logic.
- Since dots are forbidden by validation, `SubjectKey()` semantics may be acceptable for identity, but a length-prefixed encoding is even safer.
- Do not rely on `Partition.ID()` for correctness-critical coverage checks unless validation is expanded to forbid `-` in keys.

### P2 - Assignment Key Scanning And Cleanup Must Exclude Protocol Keys

The plan adds protocol keys under the same `assignment.` prefix:

```text
assignment.commit
assignment.commit_log.<V>
assignment.payload.<hash>
assignment.<worker>
```

Existing discovery and cleanup logic was written when all `assignment.*` keys were worker aliases. The implementation plan should explicitly update those paths.

Recommended fix:

- `DiscoverHighestVersion` must scan only legacy worker alias keys, not commit/payload/log keys.
- stale assignment cleanup must never delete `assignment.commit`, `assignment.commit_log.*`, or `assignment.payload.*`.
- consider moving protocol keys under clearer subprefixes such as:

```text
assignment/_commit
assignment/_commit_log/<V>
assignment/_payload/<hash>
assignment/<worker>
```

or keep the current names but add strict prefix filters.

## Additional Tests To Add

- `TestRollingUpgrade_OldLeaderNewWorker_AppliesLegacyAlias`: old leader writes only `assignment.<worker>`; new worker with no valid commit applies legacy alias.
- `TestRollingUpgrade_NewLeaderOldWorker_AliasWriteRequiredBeforeCommit`: active legacy worker in batch; alias write failure aborts commit.
- `TestRollingUpgrade_NewToOldLeader_NewWorkerFallsBackToLegacyAlias`: stale commit exists, old leader writes newer legacy alias; new worker follows legacy path until next valid commit.
- `TestNatsKV_DeletePreservesKnownRevision`: delete/purge snapshot is empty with `SourceRevisionKnown=true` and non-zero revision from KV event.
- `TestAudit_KnownCommitRequiresKnownAppliedSourceRevision`: commit known, heartbeat unknown; audit classifies behind.
- `TestHeartbeat_CapProcessingGateReflectsActualWireup`: config says enabled but gate init fails or consumer does not wrap handler; capability bit remains false.
- `TestPartitionCanonicalID_NoTupleCollision`: partitions with ambiguous joined strings remain distinct in coverage/digest logic.
- `TestAssignmentDiscovery_IgnoresProtocolKeys`: `assignment.commit`, `assignment.payload.*`, and `assignment.commit_log.*` do not affect highest legacy version or worker ID discovery.

## Verdict

The plan is close, but I would not start implementation until mixed-version compatibility is tightened. The refactored commit/payload model is a strong foundation; the rollout story is the remaining sharp edge.

The key change is to treat legacy aliases as a migration protocol, not merely old-worker convenience:

- new workers must consume legacy aliases when an old leader is active;
- new leaders must deliver aliases reliably to active old workers before committing work assigned to them;
- once every worker is commit-capable, aliases can become best-effort compatibility noise.
