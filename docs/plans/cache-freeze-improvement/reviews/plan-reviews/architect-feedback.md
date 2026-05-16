# Partition Assignment Robustness - Architect Feedback

## Summary

The review findings are accepted. F2 through F6 are direct plan corrections. F1 is the only remaining design choice: how to make committed assignment payloads recoverable and immune to mutable per-worker key corruption.

Workload heads-up: Parti's typical production shape is about 20-40 workers and 1000-2000 partitions, with each worker handling roughly 50 partitions. That 1:50 worker-to-partition ratio means an inline, gzipped commit payload is probably acceptable for the common case and may be the simplest first implementation.

Recommendation: inline payloads in `assignment.commit` are acceptable for the common case **if** the design includes a hard compressed-size guard and an automatic fallback path. For the fallback, use versioned immutable per-worker payload keys referenced by `assignment.commit`. Keep `assignment.<worker>` only as a legacy-compatibility alias for old workers.

Inline-only is simpler, and with 20-40 workers / 1000-2000 partitions it is likely small enough after gzip. The caution is that inline-only makes the single commit object scale with total partition count and turns the safety primitive into a large broadcast object. For a P0 invariant, avoid a design that can fail only because one rebalance exceeds a KV/message size limit.

## F1 - Committed Payload Not Recoverable

Accepted. The split-brain scenario is real: per-worker assignment keys are mutable and unfenced. A losing leader can write `assignment.<worker>` after the winning leader's `assignment.commit` lands, corrupting the payload that the commit's digest references. New workers may reject the corrupted payload, but the correct committed payload is no longer recoverable from KV.

### Recommended Direction

Use a two-tier payload strategy:

1. **Inline first** for the normal profile: put all per-worker payloads inside the gzipped `assignment.commit` value when the compressed commit remains below a conservative limit.
2. **Fallback to immutable refs** when the compressed commit would exceed that limit.

This keeps the common case simple while avoiding a hard scaling cliff.

Inline form:

```go
type AssignmentCommit struct {
    Version             int64
    LeaderRevision      uint64
    SourceRevision      uint64
    SourceRevisionKnown bool
    PublishedAt         time.Time
    Workers             []string

    Assignments map[string]Assignment

    BatchDigest   uint64
    PrevCommitRev uint64
}
```

Ref fallback form:

Use immutable, versioned payload keys and make the commit reference them:

```go
type AssignmentCommit struct {
    Version             int64
    LeaderRevision      uint64
    SourceRevision      uint64
    SourceRevisionKnown bool
    PublishedAt         time.Time
    Workers             []string

    Payloads map[string]AssignmentPayloadRef

    BatchDigest   uint64
    PrevCommitRev uint64
}

type AssignmentPayloadRef struct {
    Key      string // assignment.payload.<leaderRev>.<version>.<workerID>
    Revision uint64 // KV revision returned by Create
    Digest   uint64 // digest of this worker's assignment payload
}
```

Payload keys should be written with `kv.Create`, not `kv.Put` or `kv.Update`, so they are immutable once created. A losing leader may create its own payload keys, but if its commit CAS fails, those keys are never referenced by the winning commit and are inert garbage.

### Publish Flow

1. Compute assignments against a source snapshot.
2. Verify publish-time set equality: assigned partition IDs exactly equal source partition IDs.
3. Build a gzipped inline commit containing `Assignments`.
4. If the compressed commit is under the configured safety limit, use inline mode.
5. If the compressed commit exceeds the limit, write each worker payload to an immutable key:
    `assignment.payload.<leaderRevision>.<version>.<workerID>`, then use ref mode.
6. Recheck leadership.
7. CAS-write `assignment.commit` containing either inline payloads or payload refs, plus digests, workers, source revision metadata, and previous commit revision.
8. Best-effort write/update legacy `assignment.<worker>` aliases for old workers only.
9. Best-effort GC old payload keys by retention policy.

The commit is the single atomic decision point. Payload writes before the commit are inert until referenced. Payload writes from a losing leader remain unreferenced if its commit CAS fails.

### Worker Flow

1. New workers treat `assignment.commit` as the source of truth.
2. If the worker is listed in `commit.Workers`, load its assignment from `commit.Assignments[workerID]` in inline mode or fetch `commit.Payloads[workerID].Key` in ref mode.
3. Verify payload revision when using ref mode, verify digest in both modes, verify leader revision/version/source metadata, then apply.
4. If the worker is not listed in `commit.Workers`, synthesize an empty assignment at `commit.Version`, revoke through the normal apply path, and publish an apply receipt with the empty digest.
5. New workers ignore `assignment.<worker>` except for legacy bootstrap cases. Old workers continue to use the alias unchanged.

### Why Not Inline-Only

Inline payloads are attractive because they avoid a payload-key GC loop and require only one committed object. Given the common 20-40 worker / 1000-2000 partition profile, this is probably the right fast path.

However, inline-only has a poor worst-case profile:

- Every worker watches or fetches every other worker's assignment on each rebalance.
- The commit value grows with `workers x partitions`.
- A large rebalance can hit KV or message payload limits exactly when the control plane needs to repair coverage.
- Compression improves average size but does not remove worst-case size risk.

The ref fallback keeps the commit path safe if a customer grows beyond the usual profile, partition keys become large, or future metadata expands the assignment payload.

### Size Guard

Add a conservative compressed-size limit, exposed as a config knob with a safe default:

```go
type AssignmentPublishConfig struct {
    MaxInlineCommitBytes int // default: conservative, below NATS max payload and KV value limits
}
```

The exact default should be chosen against NATS max payload / KV value limits with margin. If the commit exceeds the limit, fallback to payload refs automatically rather than failing the rebalance.

### GC Policy

GC can be conservative and does not need to participate in correctness:

- Keep payloads for the last N committed versions, or for a time window such as 24 hours.
- Never delete payloads referenced by the current commit.
- Treat GC failures as non-fatal metrics/logs.

## F2 - Commit-Driven Worker State Machine

Accepted. Add an explicit state machine:

- `assignment.<worker>` arrives before `commit.V`: new workers do not apply it. Legacy workers may apply it as today.
- `commit.V` arrives: new workers evaluate the commit and load their payload from the commit payload ref or inline payload.
- `commit.V` lists the worker but has no payload ref for it: reject the commit as malformed for that worker and do not apply.
- `commit.V` does not list the worker: synthesize an empty assignment at `commit.V`, apply through the same apply-then-store-then-ack path, revoke local consumers, and publish the empty digest receipt.
- Assignment key deletion remains ignored for new workers because commit is authoritative. Old workers retain existing behavior.

## F3 - Escalation Requires End-To-End Capabilities

Accepted. Audit-driven reassignment requires proof that the full safety chain is active, not just the manager two-phase flag.

Add heartbeat capability bits:

```go
type Heartbeat struct {
    // existing fields...
    Capabilities uint32
}

const (
    CapAckV1           uint32 = 1 << 0
    CapTwoPhaseHandoff uint32 = 1 << 1
    CapProcessingGate  uint32 = 1 << 2
)
```

Escalation requires all required bits on the behind worker and the target worker. If any bit is missing, audit remains retry-pressure-only and records:

```text
parti.audit.escalation_skipped{reason="cap_missing"}
```

## F4 - SourceRevision Zero Is Overloaded

Accepted. Replace overloaded zero semantics with an explicit known bit:

```go
type Assignment struct {
    SourceRevision      uint64
    SourceRevisionKnown bool
}

type AssignmentCommit struct {
    SourceRevision      uint64
    SourceRevisionKnown bool
}
```

For NatsKV delete/purge events, preserve the actual KV revision from the event where available. Reserve `SourceRevisionKnown=false` only for static or legacy sources that cannot provide a revisioned snapshot.

## F5 - Mixed-Version Invariant Wording

Accepted. Tighten the invariant language:

Strict invariant, enforceable when all active workers report `CapAckV1`: for source revision R, committed batch V, and active worker set W, the union of applied assignments across W equals the source partition ID set at revision R exactly once.

During rolling upgrade, if any worker has `CapAckV1=0`, strict enforcement is suspended for unverifiable workers. The cluster is no worse than today, but the union is provably correct only over the verifiable subset. The invariant becomes fully provable once:

```text
parti.audit.unverifiable_workers == 0
```

## F6 - Update Wording

Accepted. Replace the over-strong wording with:

> CAS makes write conflicts observable and retryable and prevents silent failed writes from going unnoticed. It does not merge divergent authoritative replaces. When two callers issue `Update` with different lists, they serialize through CAS retry and last-writer-wins semantics still apply at the protocol level. Use `Modify` when merge semantics are required.

## Tests To Add For F1

- `TestPublisher_LosingLeaderPayloadWriteCannotCorruptWinningCommit`: winning leader commits refs; losing leader writes mutable alias or its own payloads after the fact; worker still loads the winning payload by commit ref.
- `TestPublisher_InlineCommit_CommonProfileFits`: generate 40 workers and 2000 partitions, about 50 partitions per worker; assert the gzipped inline commit fits under the default inline threshold or document the measured size.
- `TestPublisher_InlineCommit_ExceedsLimitFallsBackToRefs`: force a low inline limit; assert publisher writes immutable payload refs and commits successfully.
- `TestWorker_CommitRefPayloadMissing_ClassifiesMalformed`: commit references missing payload; worker rejects and audit reports malformed commit/payload.
- `TestWorker_CommitRefDigestMismatch_RejectsPayload`: payload exists but digest differs from commit ref; worker rejects.
- `TestWorker_InlineCommitDigestMismatch_RejectsPayload`: inline payload exists but digest differs from commit metadata; worker rejects.
- `TestWorker_RemovedFromCommit_AppliesEmptyAssignmentAndAcks`: worker not in `commit.Workers` revokes all partitions and publishes empty digest receipt.
- `TestPublisher_PayloadGC_DoesNotDeleteCurrentCommitPayloads`: GC keeps current commit payload refs and only removes old unreferenced payloads.

## Final Recommendation

Ship inline commit payloads for the common path, because the current expected workload is small enough that this should be simple and efficient. But do not ship inline-only. Add the hard compressed-size threshold and automatic immutable-ref fallback in the same design so the P0 repair path never depends on a single oversized commit value succeeding.
