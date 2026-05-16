# Follow-Up Review: Revised Partition Assignment Robustness Plan

## Summary

The response accepted the right findings, and the revised plan is now much closer to implementation-ready. The big architecture looks sound:

- refs-always with content-addressable payloads;
- dual-read rolling-upgrade migration path;
- mandatory legacy alias barrier for active old workers;
- known source revisions;
- runtime capability-gated audit escalation.

I would do one more cleanup pass before implementation. The only sharp remaining issue is the heartbeat wire-format mismatch: current legacy heartbeats are raw timestamp strings, not JSON objects. The rest are stale contradictions or guardrails that could trip an implementer if left ambiguous.

## Findings

### P0 - Heartbeat Rolling-Upgrade Wire Format Is Still Misstated

The plan says heartbeat changes are JSON-additive and that old workers serialize without the new fields. That is not accurate for current code. Old heartbeat payloads are raw RFC3339 timestamp bytes, not JSON objects.

Current old writer behavior:

```go
value := []byte(time.Now().Format(time.RFC3339Nano))
_, err := p.kv.Put(ctx, key, value)
```

New `WorkerMonitor.GetHeartbeats` and the heartbeat-aware alias barrier must explicitly support both formats:

```text
legacy timestamp string -> Heartbeat{
    SchemaVersion: 0,
    Capabilities:  0,
    Timestamp:     parsed timestamp,
}

new JSON object -> full Heartbeat payload
```

If this is not specified, a new leader may fail to classify legacy workers correctly, and the mandatory alias barrier can misbehave during rollout.

Recommended plan edits:

- Replace "JSON-additive" heartbeat wording with "dual decoder: legacy timestamp string or v1 JSON object."
- Add tests for legacy timestamp heartbeat decode.
- Make `WorkerMonitor.GetHeartbeats` tolerant of both formats.
- Ensure malformed heartbeat payloads are treated as liveness/read errors, not silently as `CapAckV1=0` unless timestamp parse succeeds.

Suggested tests:

```text
TestHeartbeat_DecodeLegacyTimestampString
TestWorkerMonitor_GetHeartbeats_MixedLegacyTimestampAndJSON
TestPublisher_LegacyAliasBarrier_UsesTimestampHeartbeatAsLegacyWorker
```

### P1 - Stale Text Still Contradicts Dual-Read Migration Rule

The new §3.6 correctly says new workers watch both `assignment._commit` and `assignment.<worker>`. But stale text remains elsewhere saying new workers ignore aliases and watch only the commit.

This should be corrected everywhere to:

```text
New workers treat assignment._commit as authoritative in steady state, but also watch/read assignment.<worker> for the dual-read legacy fallback during rolling upgrade.
```

Specific stale areas to clean:

- §3.7 legacy alias section still says new workers ignore the alias.
- KV schema table still says `assignment.<W>` is ignored by new workers.

### P1 - Delete/Purge Revision Preservation Is Still Contradicted In §2.5

§1.1 now correctly says NatsKV delete/purge events preserve the delete entry revision and keep `known=true`. But §2.5 still uses a snippet that passes revision `0`:

```go
s.applyLocal(nil, 0 /*revision*/, true /*notify*/)
```

That should become something like:

```go
s.applyLocal(nil, entry.Revision(), true /*known*/, true /*notify*/)
```

or whatever final helper signature carries both revision and knownness.

The key requirement: a revisioned empty source must remain `(revision=deleteEntryRevision, known=true)`, not `(0, false)`.

### P1 - Source Dedupe Still Uses Collision-Prone `ID()`

The plan adds `CanonicalID()` for correctness, but the validation/dedupe pseudocode still uses `p.ID()`:

```go
if _, dup := seen[p.ID()]; dup {
    return nil, fmt.Errorf("duplicate partition ID: %s", p.ID())
}
seen[p.ID()] = struct{}{}
```

This should use `p.CanonicalID()`:

```go
id := p.CanonicalID()
if _, dup := seen[id]; dup {
    return nil, fmt.Errorf("duplicate partition canonical ID: %s", id)
}
seen[id] = struct{}{}
```

`Partition.ID()` can remain for durable names/logging, but any correctness-critical coverage/digest/dedupe path should use `CanonicalID()`.

### P1 - Legacy Alias Barrier Needs A Pre-Alias Leadership Fence And CAS-Failure Accounting

The revised publish flow writes mandatory legacy aliases before the leadership recheck and before the CAS commit. That is necessary for old workers, but it also means a leader that already lost leadership could still publish legacy aliases for an uncommitted batch.

Recommended hardening:

1. Recheck leadership immediately before legacy alias barrier writes.
2. Stop alias writes if leadership is lost.
3. Recheck leadership again before the commit CAS, as already planned.
4. Document and test the unavoidable migration exposure where alias writes succeed but the later commit CAS fails.

This exposure exists because old workers cannot participate in the new commit protocol. The plan should name it explicitly as a mixed-version floor: alias-before-commit is required to avoid orphaning legacy workers, but may allow old workers to observe a batch that the new commit does not later win. This is no worse than the old protocol during migration, and disappears once legacy workers are gone.

Suggested tests:

```text
TestPublisher_AliasBarrier_RechecksLeadershipBeforeAliasWrites
TestPublisher_AliasBarrier_CASFailureAfterAliases_DocumentedMigrationExposure
```

### P2 - Forbidding `/` For `CanonicalID()` Is Unnecessary

The plan proposes expanding `Partition.Validate()` to forbid `/` because the example `CanonicalID()` uses `/` as a separator. A length-prefixed encoding does not need that restriction if the parser reads lengths.

Avoid making existing valid keys invalid unless `/` is already unsafe for NATS subject reasons.

Better direction:

```text
CanonicalID = length-prefixed binary-safe/string-safe tuple encoding.
No additional key character restrictions are required for CanonicalID.
```

If a human-readable separator is still desired for display, keep that separate from the correctness identity.

## Verdict

The revised plan is close. I would not ask for another architectural redesign. The remaining work is a precision pass:

- specify dual heartbeat decoding for legacy timestamp strings and v1 JSON;
- remove stale alias-ignore text;
- make delete/purge revision snippets match the known-revision design;
- replace remaining `ID()` uses with `CanonicalID()` in correctness paths;
- add a leadership fence around the alias barrier;
- avoid unnecessary `/` validation expansion.

After those edits, the plan is ready to hand to implementation planning.
