# Response: Refactored Plan Review

## Summary

All seven findings accepted. No pushbacks. The P0 ones expose real
regressions in the rolling-upgrade story — my refactor treated
legacy aliases as "old-worker convenience" when they need to be
treated as the migration protocol. Plan refactor underway with the
specific edits below.

The key reframing your review forces: **legacy aliases are not
backward-compatible noise, they are the migration data path**. New
workers must read them when no usable commit exists; new leaders
must write them as a barrier before committing work to an active
legacy worker. Once the unverifiable-worker set empties, aliases
revert to best-effort.

## Accept summary, per finding

### P0 #1 — New workers must read legacy aliases when no usable commit exists

Accept. My plan documented the "new worker stays dormant during
old-leader window" outcome but downplayed it as "resolves in
minutes." That's not good enough — it's an active partition orphan
path. Adopting your state rule verbatim:

```
if valid commit exists and commit.LeaderRevision >= legacy.LeaderRevision:
    follow commit path (§3.6 state machine)
else if legacy assignment exists with SchemaVersion=0 and legacy.LeaderRevision >= lastSeenLeaderRevision:
    follow legacy-compat path (apply through receipt path with stale-leader fence)
else:
    wait/reconcile
```

This is added to §3.6 and the migration section.

### P0 #2 — Legacy alias writes must be pre-commit barrier for active legacy workers

Accept. Refactoring publish flow §3.5:

```
1. Compute refs/payloads.
2. Read heartbeats; classify legacy_in_batch = {W in workers : hb[W].Capabilities & CapAckV1 == 0}.
3. For each W in legacy_in_batch: kv.Put assignment.<W> with legacy envelope.
   Retry with bounded backoff (3 attempts) on transient failures.
   On exhaustion: abort batch, surface parti.publisher.alias_barrier_failed metric.
4. Recheck leadership.
5. CAS-write assignment.commit.
6. For commit-capable workers: legacy aliases remain best-effort (compatibility noise only).
```

Active legacy workers get their alias guaranteed before the commit
lands. Persistent NATS failures fail the batch loudly rather than
silently orphaning partitions.

### P1 #3 — Preserve delete-entry revision

Accept. Pillar 1 §1.1 currently says "Delete/Purge sets revision
to 0" — that loses information NATS KV gives us for free. Will
update to preserve the actual KV delete-entry revision and keep
`SourceRevisionKnown=true` for empty snapshots from revisioned
sources.

### P1 #4 — Tighten `srcRevMatch`

Accept. Adopting your shape:

```go
srcRevMatch := !commit.SourceRevisionKnown ||
    (hb.AppliedSourceRevKnown && hb.AppliedSourceRevision == commit.SourceRevision)
```

Also updating §4.4 `SetAppliedAssignment` example to include
`AppliedSourceRevKnown: newAssignment.SourceRevisionKnown`.

### P1 #5 — `CapProcessingGate` needs a concrete wiring path

Accept. Adding a reporting API the consumer/updater layer calls
when it actually wires the gate:

```go
// On Manager:
func (m *Manager) SetCapability(cap uint32, active bool)

// Consumer/updater calls this at wire-up time:
m.SetCapability(types.CapProcessingGate, true)
```

The heartbeat publisher reads this state when composing the
heartbeat. Bit reflects runtime wire-up, not config intent. If the
gate fails to initialize or the consumer isn't using the gated
path, the bit stays false.

### P1 #6 — Collision-safe partition identity

Accept. `Partition.ID()` uses `-` as separator, but `-` is allowed
in keys, so distinct tuples can collide on identity. Adding:

```go
// CanonicalID returns a collision-safe encoding of the partition's
// key tuple, suitable for digest, set-equality, and dedupe logic.
// Uses length-prefixed encoding: "<n>:<key>" per key, joined by
// "/" (which is forbidden in keys by validation).
func (p Partition) CanonicalID() string
```

All §3.3 / §3.8 digest and set-equality logic switches from `ID()`
to `CanonicalID()`. Human-readable `ID()` retained for durable
consumer names and logs only.

### P2 #7 — Protocol-key prefix disambiguation

Accept. Renaming protocol keys with a reserved underscore prefix
so worker-ID collisions with `commit`/`payload`/`commit_log` are
impossible:

```
assignment._commit                 (was assignment.commit)
assignment._commit_log.<V>         (was assignment.commit_log.<V>)
assignment._payload.<hex(sha256)>  (was assignment.payload.<hex>)
assignment.<W>                     (legacy alias — unchanged)
```

`DiscoverHighestVersion` and `cleanupStaleAssignments` paths get
explicit prefix filters that exclude any key starting with
`assignment._`.

### Tests

Adding all eight tests you listed, plus follow-on tests for the
heartbeat-aware publish barrier and the dual-read worker state
machine. Numbering picks up at 46 from the current test plan.

## What's not changing

The core architecture stays: refs-always, content-addressable
payloads, three-key model, sha256 over canonical payload bytes,
capability-gated audit escalation, transitive coverage proof. Your
review didn't challenge those, and the corrections fit cleanly
within them — they tighten the migration story without disturbing
the steady-state design.

## Plan-text status

Refactor in progress against `docs/plans/cache-freeze-improvement/00-original-plan.md`.
Will signal completion when the edits land. The implementation
strategy file (`docs/plans/cache-freeze-improvement/02-implementation-strategy.md`)
gets a small update too — phase 1 (source-layer) now needs to add
`Partition.CanonicalID` and the delete-revision-preserving snapshot,
and phase 3 (publisher) gains the heartbeat-aware barrier.
