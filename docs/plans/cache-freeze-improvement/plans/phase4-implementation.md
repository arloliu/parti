# Phase 4 Implementation Plan — Calculator + Worker State Machine

> **Note on phase boundary.** The brief includes §4.4 (apply-then-store-then-ack) and §4.5 (leader fencing on legacy alias) inside Phase 4, even though the strategy table buckets them into Phase 5. Execute against the brief. Do not split.

## Scope summary

This phase replaces the per-worker `assignment.<W>` watcher with a dual-read state machine that follows `assignment._commit` as the source of truth and falls back to `assignment.<W>` only when the alias has a strictly fresher `LeaderRevision`. It adds a leader-side audit loop on `Calculator` that classifies workers into `fullyApplied` / `behind` / `unverifiable` against the live commit, gated by capability bits and grace windows. It unifies the update-time and initial-assignment paths through a single `applyAssignment(newAssignment)` that applies-then-stores-then-acks; the `StateStable` transition on first boot waits for that ack to be published. The legacy alias path is preserved (with leader fencing) for rolling-upgrade compatibility. The publisher and GC internals from Phase 3 are not touched; `WorkerMonitor.GetHeartbeats` is added as a thin extension used by the audit.

## Invariants this phase must preserve

- `assignment._commit` is the authoritative source of truth whenever `commit.LeaderRevision >= legacyEntry.LeaderRevision`. The legacy alias wins only when its `LeaderRevision` is strictly greater than the commit's (§3.6 case 2) or when no commit exists.
- `lastSeenLeaderRevision` is monotonically non-decreasing across both commit and legacy alias arrivals and is set to `max(lastSeen, observedRev)` only after a successful state-machine action (case (a) is a no-op so it still updates lastSeen; case (b) does NOT update lastSeen; cases (c) successful and (d) update lastSeen).
- A `StateStable` transition on the initial-assignment path may never happen before `applyAssignment` has called `SetAppliedAssignment` + `PublishNow`. First-boot workers never report `AppliedVersion=0` while claiming Stable.
- `applyAssignment` order is invariant: Apply → Store → Ack (`SetAppliedAssignment`+`PublishNow`) → Hooks. On Apply failure: do NOT store, do NOT ack, do NOT invoke hooks; mark degraded and schedule retry.
- The publisher's monotonicity (`SetAppliedAssignment` never regresses `AppliedVersion`) means a retry after a higher commit lands cannot ack a stale lower version.
- Audit never marks a worker `behind` purely because `currentSourceRevision > commit.SourceRevision`. That mismatch belongs to `monitorPartitions`/publish, not the audit.
- Audit escalation requires the full safety chain (`CapAckV1 | CapTwoPhaseHandoff | CapProcessingGate`) on both the behind worker and at least one target worker, AND `cfg.EnableTwoPhaseHandoff=true`. If any predicate fails, emit the `audit_escalation_skipped{reason=…}` metric and return.
- `srcRevMatch` is strict: when `commit.SourceRevisionKnown=true`, a worker that reports `AppliedSourceRevKnown=false` is `behind` (never `unverifiable`).
- Commit watcher recovers from `Updates()` channel close with exponential backoff. Periodic reconcile (`commitReconcileInterval`, default 30s) re-reads and re-routes idempotently.
- Legacy alias path retains its stale-leader fence (§4.5) and version gate; it must NOT apply when a fresher commit exists for the same worker.
- GC lifecycle (`commit_gc.go`) is untouched. `LiveRefs` semantics from Phase 3 remain.
- All comparisons in audit are against the in-memory `LastCommit()` snapshot, NOT a fresh source list.

## File-by-file changes

### `internal/assignment/assignment_publisher.go`

Phase 3 left `LastCommitRev() uint64` but no full-commit accessor. The audit needs `commit.Workers`, `commit.Payloads`, `commit.SourceRevisionKnown`, `commit.PublishedAt`, `commit.LeaderRevision`, `commit.Version`. **Add an in-memory cached accessor**:

- Add field on `AssignmentPublisher` (near line 102, alongside `lastCommitRev`):
  ```go
  lastCommit *types.AssignmentCommit // nil until first successful commit CAS or bootstrap
  ```
- After successful commit CAS at line 388 (where `p.currentVersion = proposedVersion` is set), also store a deep copy of the commit struct: `p.lastCommit = &commitCopy`. The copy is required because `Payloads` is a map.
- In `Start` of the publisher (or via a separate `BootstrapLastCommit(ctx)` called from `Calculator.Start` between `discoverHighestVersion` and the immediate-assignment rebalance at line 205-244 of `calculator.go`): `kv.Get("<prefix>._commit")` once; if present, unmarshal and seed `p.lastCommit`. If absent (cold/rolling-upgrade-against-old-leader case), leave nil.
- New public accessor:
  ```go
  // LastCommit returns a defensive copy of the most recently observed
  // AssignmentCommit (from a successful CAS or from bootstrap). Returns nil
  // when no commit exists yet (pre-first-commit bootstrap). Safe to call
  // concurrently.
  func (p *AssignmentPublisher) LastCommit() *types.AssignmentCommit
  ```
  Holds `p.mu`, returns a copy with cloned `Payloads` map and `Workers` slice.

Rationale: cheaper than per-tick KV get, matches the spec call shape.

### `types/partition.go`

Add two fields to `Assignment` (current struct at line 217-243). The current `applyAssignment` from the commit path needs to surface these to the heartbeat ack:

```go
type Assignment struct {
    // ... existing fields ...
    SourceRevision      uint64 `json:"source_revision,omitempty"`
    SourceRevisionKnown bool   `json:"source_revision_known,omitempty"`
}
```

Both default to zero on legacy alias decode (alias envelope doesn't carry them — verified at `assignment_publisher.go:1098-1109`), which encodes "not known" correctly.

### `internal/assignment/worker_monitor.go`

Add `GetHeartbeats` (spec §4.1 reader-side). Insert after `GetActiveWorkers` (line 146):

```go
// GetHeartbeats returns decoded heartbeats for all workers with active
// heartbeat keys. Decoded via types.DecodeHeartbeat which accepts both v1
// JSON (new workers) and legacy RFC3339 timestamp (old workers). Workers
// whose payload fails to decode are omitted; decode errors are logged at
// debug level.
func (m *WorkerMonitor) GetHeartbeats(ctx context.Context) (map[string]types.Heartbeat, error)
```

Implementation: walk `heartbeatKV.Keys(ctx)`, for each heartbeat key extract `workerID` via the same prefix-strip as `GetActiveWorkers`, `Get` the entry, `types.DecodeHeartbeat(entry.Value())`, populate map. Same error handling as `GetActiveWorkers` for `IsNoKeysFoundError`. Return empty map (not error) on no keys.

### `internal/assignment/calculator.go`

#### New methods

```go
// auditApplied performs one pass of the leader-side apply audit (§4.2).
// Called periodically from auditLoop. Reads c.publisher.LastCommit() and
// the heartbeat map, classifies workers, records metrics, and (only past
// ExtendedApplyGracePeriod, with full cap chain and EnableTwoPhaseHandoff)
// schedules audit_repair rebalance for the behind set.
func (c *Calculator) auditApplied(ctx context.Context)

// auditLoop drives auditApplied on c.AuditInterval ticks. Stops on c.stopCh
// or ctx.Done. Run as a goroutine in Start.
func (c *Calculator) auditLoop(ctx context.Context)
```

`auditApplied` implementation follows §4.2 verbatim:

1. `commit := c.publisher.LastCommit()`; if nil, return (pre-commit bootstrap).
2. `hbs, err := c.monitor.GetHeartbeats(ctx)`; on err, log debug and return.
3. Build `fullyApplied`, `behind`, `unverifiable` maps. The set of workers being audited = `union(commit.Workers, keys(hbs))` — a worker that has a heartbeat but isn't in the commit is implicitly revoked (case d on its end); skip from audit. Audit only iterates `commit.Workers`.
4. For each `w` in `commit.Workers`:
   - `hb, ok := hbs[w]`; if `!ok`, treat as `unverifiable` (no heartbeat — distinct signal from "legacy") and increment `audit_unverifiable_reason{missing_heartbeat}` (use existing `RecordWorkerBehind` or add new audit metric — see Metrics below).
   - If `hb.SchemaVersion == 0` or `hb.Capabilities & CapAckV1 == 0`, classify `unverifiable`.
   - `srcRevMatch := !commit.SourceRevisionKnown || (hb.AppliedSourceRevKnown && hb.AppliedSourceRevision == commit.SourceRevision)`.
   - `ref, hasRef := commit.Payloads[w]`.
   - Switch:
     - `!hasRef && containsString(commit.Workers, w)` → `behind` (malformed commit).
     - `hb.LeaderRevision != commit.LeaderRevision || hb.AppliedVersion != commit.Version || !srcRevMatch || (hasRef && hb.AppliedDigest != ref.SetDigest)` → `behind`.
     - Default → `fullyApplied`.
5. `c.Metrics.RecordAuditCounts(len(fullyApplied), len(behind), len(unverifiable))` (new method — see Metrics).
6. Grace check: `if time.Since(commit.PublishedAt) < c.ApplyGracePeriod { return }`.
7. Step 1 retry pressure: for each `w` in `behind`, `c.Metrics.RecordWorkerBehind(w, commit.Version)`.
8. Extended grace check: `if time.Since(commit.PublishedAt) < c.ExtendedApplyGracePeriod { return }`.
9. Capability filtering: build `behindReassignable` (full caps on the behind worker) and `targets` (full caps on a `fullyApplied` worker). Emit `RecordAuditEscalationSkipped("cap_missing_behind", w)` for each filtered-out behind worker, `RecordAuditEscalationSkipped("cap_missing_targets", "")` once if no targets.
10. If `!c.EnableTwoPhaseHandoff`, emit `RecordAuditEscalationSkipped("direct_mode", "")` and return.
    - NOTE: `Config` does not currently carry `EnableTwoPhaseHandoff`. **Add `EnableTwoPhaseHandoff bool` to `internal/assignment/config.go` Config struct** (line 51, after `PlannedScaleWindow`), wire it from `manager_assignment.go startCalculator` (line 89-107) where the calculator Config literal is built, sourcing `m.cfg.EnableTwoPhaseHandoff`.
11. Schedule rebalance: invoke `c.stateMach.EnterScaling(ctx, "audit_repair", 0)` (or a new `EnterAuditRepair` method on StateMachine if it needs to bypass cooldown — recommend reusing `EnterScaling` with zero window since the behind set is already known; do NOT bypass cooldown because audit repair is non-emergency).

#### New fields on `Calculator`

Insert after `wg sync.WaitGroup` (line 63):

```go
// auditDoneCh signals audit loop exit. Closed by auditLoop on return so Stop
// can wait deterministically.
auditDoneCh chan struct{}

// ApplyGracePeriod and ExtendedApplyGracePeriod gate the audit's escalation
// behavior (§4.2). Defaults: 2×HeartbeatTTL and 5×HeartbeatTTL.
ApplyGracePeriod         time.Duration
ExtendedApplyGracePeriod time.Duration

// AuditInterval is how often auditApplied runs. Default: HeartbeatTTL.
AuditInterval time.Duration
```

Add to `Config` struct in `internal/assignment/config.go` (after line 51):

```go
// ApplyGracePeriod is the time after commit.PublishedAt before audit
// emits retry-pressure metrics. Default: 2 × HeartbeatTTL.
ApplyGracePeriod time.Duration

// ExtendedApplyGracePeriod is the time after commit.PublishedAt before
// audit may escalate via two-phase handoff. Default: 5 × HeartbeatTTL.
ExtendedApplyGracePeriod time.Duration

// AuditInterval is the period between auditApplied runs.
// Default: HeartbeatTTL.
AuditInterval time.Duration

// EnableTwoPhaseHandoff mirrors Manager.cfg.EnableTwoPhaseHandoff so the
// audit can skip escalation when two-phase mode is off.
EnableTwoPhaseHandoff bool
```

Add to `SetDefaults()` (after line 125):

```go
if c.ApplyGracePeriod == 0 {
    c.ApplyGracePeriod = 2 * c.HeartbeatTTL
}
if c.ExtendedApplyGracePeriod == 0 {
    c.ExtendedApplyGracePeriod = 5 * c.HeartbeatTTL
}
if c.AuditInterval == 0 {
    c.AuditInterval = c.HeartbeatTTL
}
```

#### Wiring in `Start`/`Stop`

In `Start` (calculator.go), after `c.monitor.Start(ctx)` succeeds at line 261, before the GC start at line 266:

```go
// Bootstrap the publisher's LastCommit cache so audit can run from t=0.
c.publisher.BootstrapLastCommit(ctx) // ignore err; logs internally

// Start the audit loop. AuditInterval, ApplyGracePeriod,
// ExtendedApplyGracePeriod come from c.Config.
c.auditDoneCh = make(chan struct{})
c.wg.Go(func() { c.auditLoop(ctx) })
```

In `Stop` (calculator.go, around line 341, after `c.wg.Wait()`): the `wg.Wait` already covers the audit goroutine, so no extra logic needed — but keep `auditDoneCh` for tests that want to verify the loop terminated (tests can poll `select { case <-c.auditDoneCh: ... }`).

#### `monitorPartitions` (lines 568-600)

No code change required. Already rebalances on source change, which is the spec-aligned way to handle `currentSourceRevision > commit.SourceRevision`. **Add a comment block (above line 580 "partition change detected")** documenting that source-revision-vs-current is handled here, not in audit, per §4.2 final paragraph.

### `manager_assignment.go`

#### New: `monitorCommitChanges(ctx, kv)`

Replaces `monitorAssignmentChanges` as the primary watcher for new (CapAckV1) workers. The legacy per-worker watcher (`monitorAssignmentChanges`/`watchAssignment`/`handleAssignmentEntry`) **stays** for rolling-upgrade compat — both run concurrently. The dual-read selector (`selectAuthority`) resolves which one's payload to apply on any given event.

```go
// monitorCommitChanges watches assignment._commit with exponential-backoff
// restart on channel close. Runs a periodic reconcile every
// commitReconcileInterval (default 30s) that re-fetches the commit and
// routes idempotently through handleCommitEntry. Two-value receive on
// watcher.Updates(); !ok triggers backoff + re-watch (closes A2 / §4.3).
func (m *Manager) monitorCommitChanges(ctx context.Context, kv jetstream.KeyValue)
```

Implementation pattern mirrors current `monitorAssignmentChanges` at lines 269-296:

```go
const (
    commitWatcherBaseBackoff   = watcherBaseBackoff
    commitWatcherMaxBackoff    = watcherMaxBackoff
    commitReconcileInterval    = 30 * time.Second
)

func (m *Manager) monitorCommitChanges(ctx context.Context, kv jetstream.KeyValue) {
    backoff := commitWatcherBaseBackoff
    reconcileTicker := time.NewTicker(commitReconcileInterval)
    defer reconcileTicker.Stop()
    for {
        err := m.watchCommit(ctx, kv, reconcileTicker.C)
        if err == nil || ctx.Err() != nil { return }
        m.logError("commit watcher failed, retrying", "error", err, "backoff", backoff)
        m.recordKVError(err)
        // (jittered sleep — same recipe as line 281-291)
        ...
        backoff = min(backoff*2, commitWatcherMaxBackoff)
    }
}

func (m *Manager) watchCommit(ctx context.Context, kv jetstream.KeyValue, reconcileTickC <-chan time.Time) error {
    key := "assignment._commit" // m.cfg.AssignmentPrefix + "._commit"
    watcher, err := kv.Watch(ctx, key)
    if err != nil { return fmt.Errorf("failed to watch commit: %w", err) }
    defer func() { _ = watcher.Stop() }()
    for {
        select {
        case <-ctx.Done():
            return nil
        case entry, ok := <-watcher.Updates():
            if !ok {
                // Channel closed — surface as error so monitorCommitChanges restarts
                // with backoff. Distinct from ctx.Done because the watcher may have
                // died for transient reasons (NATS reconnect, server restart).
                return errors.New("commit watcher channel closed")
            }
            if entry == nil { continue }
            m.handleCommitEntry(entry)
        case <-reconcileTickC:
            // Periodic reconcile: re-read commit and route idempotently.
            current, _, err := kvutil.GetJSON[types.AssignmentCommit](ctx, kv, key)
            if err != nil || current == nil { continue }
            m.handleCommitValue(current)
        }
    }
}
```

#### New: `handleCommitEntry(entry jetstream.KeyValueEntry)` and `handleCommitValue(commit *types.AssignmentCommit)`

```go
// handleCommitEntry decodes a watcher entry and routes via handleCommitValue.
// Deletion events are ignored — a deleted commit is treated as "no commit",
// which the dual-read selector handles correctly (legacy alias may still win).
func (m *Manager) handleCommitEntry(entry jetstream.KeyValueEntry) {
    if entry.Operation() == jetstream.KeyValueDelete { return }
    var commit types.AssignmentCommit
    if err := json.Unmarshal(entry.Value(), &commit); err != nil {
        m.logError("failed to unmarshal commit", "error", err)
        return
    }
    m.handleCommitValue(&commit)
}

// handleCommitValue implements §3.6 case 1 (commit-path state machine).
// Cases (a)-(e) and the F2 transition mapping.
func (m *Manager) handleCommitValue(commit *types.AssignmentCommit) {
    workerID := m.WorkerID()
    current := m.CurrentAssignment()

    // Case (a): commit.Version <= currentAppliedVersion → no-op (still update lastSeen).
    if commit.Version <= current.Version {
        m.updateLastSeenLeaderRevision(commit.LeaderRevision)
        return
    }
    // Case (b): stale-leader fence.
    if commit.LeaderRevision < m.lastSeenLeaderRevision.Load() {
        m.metrics.RecordStaleLeaderRejected() // new metric — see Metrics
        return // do NOT update lastSeen
    }

    // Dual-read selector — case 1 of §3.6 source-of-truth rule. The legacy
    // watcher's last-seen alias is consulted if it's fresher. For simplicity
    // we use a sentinel "lastSeenAlias" stored on the Manager (see below).
    if !m.selectAuthorityFavorsCommit(commit) {
        // Legacy alias is fresher; ignore this commit until alias path either
        // matches it or the next commit arrives at a higher LeaderRevision.
        return
    }

    // Coalescing — case (e). If an apply is already in flight, just stash
    // the highest pending target and return. The in-flight apply re-checks
    // pendingTargetVersion on completion.
    if !m.tryReservePendingApply(commit.Version) {
        m.stashPendingTarget(commit)
        return
    }
    defer m.releasePendingApply()

    var newAssignment types.Assignment
    if ref, ok := commit.Payloads[workerID]; ok && containsString(commit.Workers, workerID) {
        // Case (c)
        bytes, err := m.assignmentKV.Get(m.ctx, ref.Key)
        if err != nil { m.metrics.RecordPayloadFetchError(); return }
        plain, err := gzipDecompress(bytes.Value())
        if err != nil { m.metrics.RecordPayloadDecompressError(); return }
        if hex.EncodeToString(sha256.Sum256(plain)[:]) != ref.PayloadHash {
            m.metrics.RecordPayloadHashMismatch(); return
        }
        var payload types.AssignmentPayload
        if err := json.Unmarshal(plain, &payload); err != nil {
            m.metrics.RecordPayloadDecodeError(); return
        }
        if computeSetDigest(payload.Partitions) != ref.SetDigest {
            m.metrics.RecordSetDigestMismatch(); return
        }
        newAssignment = types.Assignment{
            Version:             commit.Version,
            Lifecycle:           commit.Lifecycle,
            Partitions:          payload.Partitions,
            LeaderRevision:      commit.LeaderRevision,
            SourceRevision:      commit.SourceRevision,
            SourceRevisionKnown: commit.SourceRevisionKnown,
            TotalWorkers:        len(commit.Workers),
        }
    } else if !containsString(commit.Workers, workerID) {
        // Case (d): worker not in commit → synthesize empty.
        newAssignment = types.Assignment{
            Version:             commit.Version,
            Lifecycle:           commit.Lifecycle,
            Partitions:          nil,
            LeaderRevision:      commit.LeaderRevision,
            SourceRevision:      commit.SourceRevision,
            SourceRevisionKnown: commit.SourceRevisionKnown,
            TotalWorkers:        len(commit.Workers),
        }
    } else {
        // Case (c) ref == nil → malformed.
        m.metrics.RecordCommitPayloadMissing(); return
    }

    m.updateLastSeenLeaderRevision(commit.LeaderRevision)
    m.applyAssignment(newAssignment)

    // After apply, re-check pendingTargetVersion (case (e) coalesce gate).
    if pending := m.takeStashedPendingTarget(); pending != nil && pending.Version > newAssignment.Version {
        m.handleCommitValue(pending)
    }
}
```

**Hash-verification failure semantics**: on hash/digest/decode failure, emit the metric, leave `lastSeenLeaderRevision` UNCHANGED, do NOT clear pending state — the worker will retry on the next watcher event or reconcile tick. The commit is NOT considered "handled".

**Coalescing primitives** (new fields on Manager):
```go
// pendingApplyInFlight serializes commit-path applies. False = idle.
pendingApplyInFlight atomic.Bool
// stashedCommit holds the highest pending commit observed during an
// in-flight apply. Replaced (not appended) on each new arrival.
stashedCommit atomic.Pointer[types.AssignmentCommit]
```

`tryReservePendingApply(v)` is `pendingApplyInFlight.CompareAndSwap(false, true)`. `releasePendingApply()` is `Store(false)`. `stashPendingTarget` does CAS on `stashedCommit`, keeping only the highest-Version commit. `takeStashedPendingTarget` swaps to nil and returns.

#### New: `selectAuthority(commit, legacyEntry, lastSeen) AuthorityChoice`

Pure function for testability:

```go
type AuthorityChoice int
const (
    AuthorityNone AuthorityChoice = iota
    AuthorityCommit
    AuthorityLegacyAlias
)

// selectAuthority implements §3.6 source-of-truth selection rule (case 1/2/3).
// commit may be nil (no commit observed yet). legacyEntry may have zero
// LeaderRevision when no legacy alias has been seen.
func selectAuthority(commit *types.AssignmentCommit, legacyEntry *types.Assignment, lastSeen uint64) AuthorityChoice {
    hasCommit := commit != nil
    hasLegacy := legacyEntry != nil

    // Case 1: commit wins.
    if hasCommit && (!hasLegacy || commit.LeaderRevision >= legacyEntry.LeaderRevision) {
        return AuthorityCommit
    }
    // Case 2: legacy alias wins (and is fresh enough).
    if hasLegacy && legacyEntry.LeaderRevision >= lastSeen &&
        (!hasCommit || legacyEntry.LeaderRevision > commit.LeaderRevision) {
        return AuthorityLegacyAlias
    }
    // Case 3: no usable authority.
    return AuthorityNone
}
```

`selectAuthorityFavorsCommit(commit)` is a Manager method that wraps the pure function using `m.lastSeenAlias.Load()` (new atomic Pointer field tracking the most recent legacy alias entry observed by `handleAssignmentEntry`).

#### Unified: `applyAssignment(newAssignment types.Assignment) error`

Replaces the body of `applyAssignmentUpdate` (lines 366-379) and the async `applyInitialHandoffAsync` apply at `manager_handoff.go:75-78`. Both call sites invoke `applyAssignment`:

```go
// applyAssignment is the single apply-then-store-then-ack pipeline (§4.4).
// Returns nil on success, error on Apply failure (Store and Ack skipped).
// Caller is responsible for retry scheduling on error.
func (m *Manager) applyAssignment(newAssignment types.Assignment) error {
    workerID := m.WorkerID()
    oldAssignment := m.CurrentAssignment()

    // 1. Apply via handoff coordinator.
    if err := m.handoffCoordinator.Apply(m.ctx, workerID, oldAssignment, newAssignment); err != nil {
        m.logError("handoff apply failed", "error", err)
        m.markDegraded("apply failed", err)
        m.scheduleApplyRetry(newAssignment) // new helper — bounded exponential backoff
        return err
    }

    // 2. Store the now-applied assignment.
    m.assignment.Store(newAssignment)

    m.logger.Info("assignment applied",
        "worker_id", workerID,
        "old_version", oldAssignment.Version,
        "new_version", newAssignment.Version,
        "old_partitions", len(oldAssignment.Partitions),
        "new_partitions", len(newAssignment.Partitions),
    )

    // 3. Publish ack via heartbeat.
    appliedDigest := computeAppliedDigest(newAssignment.Partitions) // xxh3 over CanonicalIDs — reuse computeSetDigest equivalent
    if hb, ok := m.heartbeat.(*heartbeat.Publisher); ok {
        hb.SetAppliedAssignment(heartbeat.AppliedAssignment{
            LeaderRevision:        newAssignment.LeaderRevision,
            AppliedVersion:        newAssignment.Version,
            AppliedDigest:         appliedDigest,
            AppliedSourceRevision: newAssignment.SourceRevision,
            AppliedSourceRevKnown: newAssignment.SourceRevisionKnown,
            AppliedAt:             time.Now(),
        })
        if err := hb.PublishNow(m.ctx); err != nil {
            m.logError("heartbeat publish-now after apply failed", "error", err)
            // Non-fatal — next tick picks up the snapshot.
        }
    }

    // 4. Metrics + hooks.
    m.recordAssignmentMetrics(oldAssignment, newAssignment)
    m.invokeAssignmentChangedHooks(workerID, oldAssignment, newAssignment)
    return nil
}
```

`computeAppliedDigest` lives in the parti package (mirror of `internal/assignment.computeSetDigest`; either un-export and reuse via small package-local copy or hoist to `types/partition.go` as `PartitionSetDigest([]Partition) uint64`). **Recommend hoisting to `types/partition.go`** as `func PartitionSetDigest(parts []Partition) uint64` — it's already needed in both packages.

`m.heartbeat` is a `heartbeatPublisher` interface today (manager.go:121-124). **Extend that interface** to add `SetAppliedAssignment(snap heartbeat.AppliedAssignment)` and `PublishNow(ctx context.Context) error`. Update `heartbeat.NewNop()` to satisfy the extended interface (no-op).

`scheduleApplyRetry`: new method that stores the failed assignment in an atomic Pointer and starts a goroutine with bounded exponential backoff (initial 1s, max 30s, jitter ±20%) that re-invokes `applyAssignment`. On retry success, the goroutine self-terminates. Multiple failed assignments coalesce: keep the highest-Version (same coalescing primitive as `stashedCommit` reused).

#### Modified: `handleAssignmentEntry` (lines 337-354) — keep legacy alias path with fences

```go
func (m *Manager) handleAssignmentEntry(workerID string, entry jetstream.KeyValueEntry) {
    if entry.Operation() == jetstream.KeyValueDelete {
        m.logger.Debug("ignoring assignment deletion during leader transition")
        return
    }
    newAssignment, ok := m.decodeAssignmentEntry(entry)
    if !ok { return }

    // §4.5 leader fence — reject stale leader.
    if newAssignment.LeaderRevision < m.lastSeenLeaderRevision.Load() {
        m.metrics.RecordStaleLeaderRejected()
        return
    }
    oldAssignment := m.CurrentAssignment()
    if oldAssignment.Version >= newAssignment.Version { return }

    // Record this as the most-recent legacy alias observation so the dual-read
    // selector can consult it on commit arrivals.
    m.lastSeenAlias.Store(&newAssignment)

    // Dual-read selector — case 2 vs 3.
    commit := m.lastSeenCommit() // pulls the latest commit observed by the commit watcher
    choice := selectAuthority(commit, &newAssignment, m.lastSeenLeaderRevision.Load())
    if choice != AuthorityLegacyAlias {
        // Either commit is fresher (drop alias) or both stale (no-op).
        return
    }

    // Apply through unified pipeline. Legacy alias-derived Assignment carries
    // zero SourceRevision/SourceRevisionKnown by design — encoded as
    // "unknown" downstream.
    m.lastSeenLeaderRevision.Store(newAssignment.LeaderRevision)
    _ = m.applyAssignment(newAssignment)
}
```

`m.lastSeenCommit()`: new helper returning the most recent commit observed by `monitorCommitChanges` (stored on Manager as `atomic.Pointer[types.AssignmentCommit]` named `lastObservedCommit`; populated in `handleCommitValue` before the apply).

#### Removed/replaced

- `applyAssignmentUpdate` (lines 366-379) — deleted. Its responsibilities migrate to `applyAssignment`.
- `applyHandoffAndHooks` (lines 381-420) — keep the hook-invocation logic but extract into `invokeAssignmentChangedHooks(workerID, old, new)`. The Apply call moves into `applyAssignment` step 1. Hooks become step 4.

#### Manager state additions (in `manager.go`)

Add fields to `Manager` struct (after line 102):

```go
// lastSeenLeaderRevision is the highest LeaderRevision this worker has
// observed and accepted from either the commit watcher (case (c)/(d) success)
// or the legacy alias path. Stale-leader fences read this; successful
// state-machine actions update it.
lastSeenLeaderRevision atomic.Uint64

// lastSeenAlias is the most-recent decoded legacy-alias assignment observed
// by the legacy watcher. Consulted by handleCommitValue's dual-read selector.
lastSeenAlias atomic.Pointer[types.Assignment]

// lastObservedCommit is the most-recent decoded commit observed by the
// commit watcher. Consulted by handleAssignmentEntry's dual-read selector.
lastObservedCommit atomic.Pointer[types.AssignmentCommit]

// pendingApplyInFlight + stashedCommit implement case (e) coalescing.
pendingApplyInFlight atomic.Bool
stashedCommit        atomic.Pointer[types.AssignmentCommit]
```

### `manager.go`

#### Initial-assignment bootstrap path — lines 374-389

Replace lines 387-389 with synchronous initial apply gated through `applyAssignment`:

```go
// Step 5: Wait for assignment (unchanged at line 380-385).
m.transitionState(StateWaitingAssignment)
if err := m.waitForAssignment(startupCtx, assignmentKV, heartbeatKV); err != nil {
    return fmt.Errorf("failed to get assignment: %w", err)
}

// Step 5.5: Apply the initial assignment via the unified pipeline.
// Must complete (Apply → Store → Ack) BEFORE transitioning to StateStable —
// otherwise the worker reports AppliedVersion=0 while claiming stable.
//
// emitInitialAssignmentEvents is folded into applyAssignment via the
// invokeAssignmentChangedHooks step. applyInitialHandoffAsync is deleted.
initial := m.CurrentAssignment()
if len(initial.Partitions) > 0 || initial.Version > 0 {
    // Refresh from KV via handleCommitValue if the commit watcher has
    // already populated lastObservedCommit; otherwise apply what waitForAssignment
    // stored (legacy alias path).
    if commit := m.lastObservedCommit.Load(); commit != nil && commit.Version >= initial.Version {
        // Re-route through commit path so SourceRevision flows correctly.
        m.handleCommitValue(commit)
    } else {
        if err := m.applyAssignment(initial); err != nil {
            // Apply failed: stay in WaitingAssignment, scheduleApplyRetry already
            // running. Caller's startup deadline (ctx) governs how long we wait.
            return fmt.Errorf("initial apply failed: %w", err)
        }
    }
}

// Step 6: Transition to stable state only after initial apply + ack published.
m.transitionState(StateStable)

// Start background workers.
m.wg.Go(func() { m.monitorCommitChanges(m.ctx, assignmentKV) })  // NEW — primary watcher
m.wg.Go(func() { m.monitorAssignmentChanges(m.ctx, assignmentKV) }) // legacy compat — kept
m.monitorNATSConnection()
```

Delete `emitInitialAssignmentEvents` and `applyInitialHandoffAsync` from `manager_handoff.go` (lines 48-83); their responsibilities are now inside `applyAssignment`.

### `types/partition.go` Assignment additions

(See above — adds `SourceRevision` and `SourceRevisionKnown`.) Also add:

```go
// PartitionSetDigest returns xxh3 over the sorted CanonicalIDs of parts,
// joined with '\n'. Identical to internal/assignment.computeSetDigest;
// hoisted to types so the manager's apply ack and the publisher's set-
// digest share a single source of truth.
func PartitionSetDigest(parts []Partition) uint64
```

Implementation copied from `internal/assignment/assignment_publisher.go:1007-1021`. Then update `internal/assignment/assignment_publisher.go` to delegate `computeSetDigest` to `types.PartitionSetDigest`.

### `types/metrics_collector.go` — new audit metrics

Add to `CalculatorMetrics` interface (or a new `AuditMetrics` interface composed in):

```go
// RecordAuditCounts records the audit's classification counts as gauges.
RecordAuditCounts(fullyApplied, behind, unverifiable int)

// RecordWorkerBehind records a behind-classified worker observation
// (one call per audit pass per behind worker).
RecordWorkerBehind(workerID string, commitVersion int64)

// RecordAuditEscalationSkipped records a skipped escalation with reason:
// "cap_missing_behind" | "cap_missing_targets" | "direct_mode".
RecordAuditEscalationSkipped(reason, workerID string)

// RecordStaleLeaderRejected counts assignments/commits rejected by the
// stale-leader fence (worker-side).
RecordStaleLeaderRejected()

// Worker-side payload classification metrics (commit-path state machine).
RecordCommitPayloadMissing()
RecordPayloadFetchError()
RecordPayloadDecompressError()
RecordPayloadDecodeError()
RecordPayloadHashMismatch()
RecordSetDigestMismatch()
```

Update `internal/metrics/nop.go` (and any Prometheus collector if present) to implement these as no-ops. The metric *name* convention should match §4.2 callouts: emit Prometheus names `parti_audit_fully_applied`, `parti_audit_behind`, `parti_audit_unverifiable`, `parti_audit_escalation_skipped{reason}`, `parti_worker_stale_leader_rejected`, `parti_worker_commit_payload_missing`, `parti_worker_payload_fetch_error`, `parti_worker_payload_hash_mismatch`, `parti_worker_set_digest_mismatch`.

## State-machine transitions (formal)

Table: precondition → action → metric → state-mutation. `LSR` = `lastSeenLeaderRevision`. `cur` = `CurrentAssignment()`.

| Case | Precondition | Action | Metric | LSR mutation | Pending state |
|---|---|---|---|---|---|
| (a) | `commit.Version <= cur.Version` | no-op | — | `LSR = max(LSR, commit.LR)` | unchanged |
| (b) | `commit.LR < LSR` | drop | `RecordStaleLeaderRejected` | unchanged | unchanged |
| (c)-ref-nil | `W in Workers && Payloads[W]==nil` | drop | `RecordCommitPayloadMissing` | unchanged | unchanged |
| (c)-fetch-err | ref present, `kv.Get` error | drop | `RecordPayloadFetchError` | unchanged | unchanged |
| (c)-decompress | gzip decode fail | drop | `RecordPayloadDecompressError` | unchanged | unchanged |
| (c)-hash | `hex(sha256(plain)) != ref.PayloadHash` | drop | `RecordPayloadHashMismatch` | unchanged | unchanged |
| (c)-digest | `xxh3(sorted) != ref.SetDigest` | drop | `RecordSetDigestMismatch` | unchanged | unchanged |
| (c)-ok | all checks pass | `applyAssignment(payload)` | (assignment_change recorded by recordAssignmentMetrics) | `LSR = max(LSR, commit.LR)` | clear |
| (d) | `W NOT in commit.Workers` | `applyAssignment(empty)` | — | `LSR = max(LSR, commit.LR)` | clear |
| (e) | `pendingApplyInFlight==true` | stash highest-version | — | unchanged | `stashedCommit = max(stashed, commit)` |

## Dual-read source-of-truth rule (formal)

```go
func selectAuthority(commit *types.AssignmentCommit, legacyEntry *types.Assignment, lastSeen uint64) AuthorityChoice {
    hasCommit := commit != nil
    hasLegacy := legacyEntry != nil

    // Case 1: commit wins.
    //   commit exists AND (no legacy OR commit.LR >= legacy.LR)
    if hasCommit && (!hasLegacy || commit.LeaderRevision >= legacyEntry.LeaderRevision) {
        return AuthorityCommit
    }
    // Case 2: legacy alias wins (compat-path).
    //   legacy exists AND legacy.LR >= lastSeen
    //                 AND (no commit OR legacy.LR > commit.LR)
    if hasLegacy && legacyEntry.LeaderRevision >= lastSeen &&
        (!hasCommit || legacyEntry.LeaderRevision > commit.LeaderRevision) {
        return AuthorityLegacyAlias
    }
    // Case 3: no usable authority — wait.
    return AuthorityNone
}
```

Naming corresponds 1:1 with §3.6:
- `commit.LeaderRevision` = field on `types.AssignmentCommit.LeaderRevision`.
- `legacyEntry.LeaderRevision` = field on `types.Assignment.LeaderRevision`.
- `lastSeen` = `Manager.lastSeenLeaderRevision.Load()`.

## Capability gating in audit (formal)

```go
const requiredCaps = types.CapAckV1 | types.CapTwoPhaseHandoff | types.CapProcessingGate

// Classification stage:
if hb.SchemaVersion == 0 || (hb.Capabilities & types.CapAckV1) == 0 {
    unverifiable[w] = true
    continue
}

// Escalation-skip predicates (after ExtendedApplyGracePeriod check):
for w := range behind {
    if (hbs[w].Capabilities & requiredCaps) != requiredCaps {
        c.Metrics.RecordAuditEscalationSkipped("cap_missing_behind", w)
        continue
    }
    behindReassignable = append(behindReassignable, w)
}
for w := range fullyApplied {
    if (hbs[w].Capabilities & requiredCaps) == requiredCaps {
        targets = append(targets, w)
    }
}
if len(behindReassignable) == 0 || len(targets) == 0 {
    c.Metrics.RecordAuditEscalationSkipped("cap_missing_targets", "")
    return
}
if !c.EnableTwoPhaseHandoff {
    c.Metrics.RecordAuditEscalationSkipped("direct_mode", "")
    return
}
```

The three skip paths emit distinct `reason` label values: `cap_missing_behind`, `cap_missing_targets`, `direct_mode`.

## Test plan

All new tests live in `internal/assignment/` or in the parti root package, mirroring existing conventions (`TestPublisher_*` for assignment-package, `TestManager_*`/`TestMonitor*` for root). Each test uses the existing `testutil` NATS harness (search existing tests for setup patterns — `assignment_publisher_test.go` line 1-50 shows the canonical bootstrap).

### State-machine tests — `internal/assignment/commit_state_machine_test.go` (new file)

Setup pattern shared by all: a real NATS JetStream embedded via the existing test helpers, a `types.AssignmentCommit` written to `"assignment._commit"`, the worker's payload written to `"assignment._payload.<hex>"`. Each test instantiates a `Manager` (or a smaller `commitRouter` test double if Manager wiring is too heavy) and asserts on `m.CurrentAssignment()` + the heartbeat KV's most-recent value + metric counters.

| Test name | File | Setup | Action | Assertion |
|---|---|---|---|---|
| `TestCommitStateMachine_Case_A_VersionAtOrBelowCurrent_NoOp` | `internal/assignment/commit_state_machine_test.go` | Worker currently at V=5; write commit V=5 | `handleCommitValue(commit)` | `m.CurrentAssignment().Version == 5` unchanged; no Apply call (use a mock handoff coordinator that counts calls — assert 0); `lastSeenLeaderRevision == commit.LR`. Encodes "case a no-op but updates LSR". |
| `TestCommitStateMachine_Case_B_StaleLeaderRevision_RejectsAndEmitsMetric` | same | `LSR = 100`; write commit at V=10 with `LeaderRevision=50` | `handleCommitValue` | assignment unchanged; mock metrics records `RecordStaleLeaderRejected` exactly once; `LSR` unchanged (NOT bumped to 50). |
| `TestCommitStateMachine_Case_C_PayloadRefNil_ClassifiesMalformed` | same | commit at V=10 lists worker in `Workers` but `Payloads` map missing the worker's key | `handleCommitValue` | no Apply; `RecordCommitPayloadMissing` recorded; LSR unchanged. |
| `TestCommitStateMachine_Case_C_PayloadFetchError_NoApply` | same | commit references payload key that doesn't exist in KV | `handleCommitValue` | `RecordPayloadFetchError` recorded; no Apply. |
| `TestCommitStateMachine_Case_C_PayloadHashMismatch_RejectsPayload` | same | write payload bytes that DON'T match ref.PayloadHash (e.g. manually Put corrupted bytes at ref.Key) | `handleCommitValue` | `RecordPayloadHashMismatch` recorded; no Apply; LSR unchanged. |
| `TestCommitStateMachine_Case_C_SetDigestMismatch_RejectsPayload` | same | payload bytes hash matches but contains a different partition set than ref.SetDigest claims | `handleCommitValue` | `RecordSetDigestMismatch` recorded; no Apply. |
| `TestCommitStateMachine_Case_C_FullChainSucceeds_AppliesAndAcks` | same | well-formed commit + payload; worker in batch | `handleCommitValue` | `m.CurrentAssignment().Version == commit.Version`; `m.CurrentAssignment().SourceRevisionKnown == commit.SourceRevisionKnown`; mock heartbeat publisher records `SetAppliedAssignment` exactly once with matching version, then `PublishNow`. |
| `TestCommitStateMachine_Case_D_WorkerNotInBatch_AppliesEmptyAssignment` | same | commit at V=10 omits this worker from `Workers` | `handleCommitValue` | `m.CurrentAssignment().Version == 10`; `len(.Partitions) == 0`; heartbeat ack has `AppliedDigest == 0`; `applyAssignment` invoked. |
| `TestCommitStateMachine_Case_E_CoalescesInFlightApply_HighestVersionWins` | same | block the mock handoff coordinator's `Apply` on a barrier; trigger `handleCommitValue(v=10)`, then v=11, then v=12 while v=10 is blocked; release barrier | `handleCommitValue` sequence | after release: v=10 applies, then case (e) re-runs with v=12 (skipping v=11 because stashed is highest-only); final `CurrentAssignment().Version == 12`. |

### Dual-read selector tests — `internal/assignment/select_authority_test.go` (new file, table-driven)

Pure-function tests on `selectAuthority`:

| Test name | Inputs | Expected |
|---|---|---|
| `TestSelectAuthority_Case1_CommitOnly` | commit V=1 LR=10, legacy=nil, lastSeen=0 | `AuthorityCommit` |
| `TestSelectAuthority_Case1_CommitFresherThanLegacy` | commit LR=20, legacy LR=15, lastSeen=10 | `AuthorityCommit` |
| `TestSelectAuthority_Case1_CommitEqualLegacy` | commit LR=20, legacy LR=20, lastSeen=10 | `AuthorityCommit` (tie → commit) |
| `TestSelectAuthority_Case2_LegacyFresherThanCommit_HandoffWindow` | commit LR=10, legacy LR=20, lastSeen=10 | `AuthorityLegacyAlias` (new-leader→old-leader handoff) |
| `TestSelectAuthority_Case2_LegacyOnly_NoCommit` | commit=nil, legacy LR=10, lastSeen=10 | `AuthorityLegacyAlias` |
| `TestSelectAuthority_Case3_LegacyBelowLastSeen` | commit=nil, legacy LR=5, lastSeen=10 | `AuthorityNone` |
| `TestSelectAuthority_Case3_NoCommitNoLegacy` | both nil | `AuthorityNone` |

### Audit tests — `internal/assignment/calculator_audit_test.go` (new file)

Setup: spin up calculator with a wrapped `WorkerMonitor` whose `GetHeartbeats` returns a fixture map; preload `publisher.lastCommit` via `BootstrapLastCommit` after writing a known commit. Use mock metrics that captures calls into slices for assertion.

| Test name | Setup | Action | Assertion |
|---|---|---|---|
| `TestCalculatorAudit_FullyApplied_AllInSync` | commit V=10 LR=20 SourceRev=5 SrcKnown=true; 3 workers, all hbs match (AppliedVersion=10, LR=20, AppliedSourceRevision=5, AppliedSourceRevKnown=true, AppliedDigest=ref.SetDigest) | `auditApplied(ctx)` | metrics: `RecordAuditCounts(3, 0, 0)`; no `RecordWorkerBehind`; no escalation. |
| `TestCalculatorAudit_Behind_VersionMismatch` | as above but worker-A reports AppliedVersion=9 | `auditApplied` | `RecordAuditCounts(2, 1, 0)`; `RecordWorkerBehind("worker-A", 10)` exactly once. |
| `TestCalculatorAudit_Behind_DigestMismatch` | worker-B reports AppliedDigest != ref.SetDigest | `auditApplied` | worker-B classified behind. |
| `TestCalculatorAudit_Unverifiable_LegacyWorker` | worker-C has `SchemaVersion=0` (legacy timestamp heartbeat) | `auditApplied` | worker-C in `unverifiable`; NOT in behind; does NOT trigger escalation even past ExtendedApplyGracePeriod. |
| `TestCalculatorAudit_StrictSrcRevMatch_KnownCommitButUnknownWorker_Behind` | commit.SourceRevisionKnown=true; worker-D reports `AppliedSourceRevKnown=false` but everything else matches | `auditApplied` | worker-D classified `behind` (per P1 #4 stricter rule), NOT skipped/unverifiable. |
| `TestCalculatorAudit_NonStrictWhenCommitUnknown` | commit.SourceRevisionKnown=false; worker-E reports `AppliedSourceRevKnown=false` | `auditApplied` | worker-E classified `fullyApplied` (audit skips source-rev check when commit declares unknown). |
| `TestCalculatorAudit_BehindWorkerMissingCaps_EscalationSkipped` | worker-A behind but reports only `CapAckV1` (no two-phase, no gate); ExtendedApplyGracePeriod elapsed | `auditApplied` | `RecordAuditEscalationSkipped("cap_missing_behind", "worker-A")` recorded; no `EnterScaling` call on state machine. |
| `TestCalculatorAudit_TargetsMissingCaps_EscalationSkipped` | worker-A behind with full caps; worker-B fully applied but missing `CapProcessingGate`; ExtGrace elapsed | `auditApplied` | `RecordAuditEscalationSkipped("cap_missing_targets", "")` once; no escalation. |
| `TestCalculatorAudit_DirectMode_SkipsEscalation` | full caps everywhere, behind+targets ok, but `c.EnableTwoPhaseHandoff=false` | `auditApplied` | `RecordAuditEscalationSkipped("direct_mode", "")` recorded; no escalation. |
| `TestCalculatorAudit_BeforeGrace_NoMetricsForBehind` | commit.PublishedAt = now (within ApplyGracePeriod) | `auditApplied` | `RecordAuditCounts` recorded (always) but NO `RecordWorkerBehind` calls. |
| `TestCalculatorAudit_PreCommitBootstrap_NoOp` | `publisher.LastCommit() == nil` | `auditApplied` | returns immediately; no metrics recorded. |

### Watcher reconcile tests — `manager_commit_watcher_test.go` (new file at parti root)

| Test name | Setup | Action | Assertion |
|---|---|---|---|
| `TestMonitorCommitChanges_ChannelCloseTriggersBackoffAndRestart` | watch wrapper that forcibly closes its update channel after first event | start `monitorCommitChanges`; observe initial event; channel closes | second commit written ~5s later is still observed (watcher re-established); metric `parti.kv_error` recorded once. |
| `TestMonitorCommitChanges_PeriodicReconcile_RecoversDivergence` | start watcher; pause delivery (test double); update commit out-of-band to V=15 (watcher misses it); wait > `commitReconcileInterval` | observe via `CurrentAssignment` | after the next reconcile tick: `CurrentAssignment().Version == 15`. |
| `TestMonitorCommitChanges_DeleteEventIgnored` | write commit V=10, then Delete the key | watcher fires Delete | `CurrentAssignment().Version == 10` unchanged (no panic, no apply). |

### Apply-then-store-then-ack tests — `manager_apply_assignment_test.go` (new file)

| Test name | Setup | Action | Assertion |
|---|---|---|---|
| `TestApplyAssignment_InitialBootstrap_AckPublishedBeforeStateStable` | start Manager with a fixture initial assignment available in KV | observe state transitions via `monitorCalculatorState` or atomic snapshot | at the moment `m.State() == StateStable`, the heartbeat in KV decodes to `AppliedVersion == initial.Version`. Test reads heartbeat KV immediately after `Start` returns; encodes invariant "ack-before-stable". |
| `TestApplyAssignment_UpdatePath_AckPublishedAfterApplySucceeds` | running manager at V=1; write commit V=2 | observe heartbeat KV | within 1s of the commit watcher firing: heartbeat `AppliedVersion == 2`. Mock handoff coordinator instrumented to assert call ordering: Apply call timestamp < heartbeat Put timestamp. |
| `TestApplyAssignment_ApplyFails_NoStoreNoAck` | mock handoff coordinator returns error on Apply | invoke `applyAssignment(newAssignment)` | `m.CurrentAssignment().Version` unchanged from before; heartbeat in KV has `AppliedVersion == oldVersion`; metric: degraded mode entered; retry scheduled. |
| `TestApplyAssignment_AckMonotonic_NoVersionRegression` | Apply v=2 succeeds; immediately call `applyAssignment(v=1)` (e.g. from a late watcher event) | observe heartbeat publisher snapshot | snapshot still reflects v=2 (publisher's monotone invariant). |

### Rolling upgrade integration tests — `manager_rolling_upgrade_test.go` (new file)

| Test name | Setup | Action | Assertion |
|---|---|---|---|
| `TestRollingUpgrade_NewWorker_AppliesLegacyAliasWhenNoCommit` | publish only legacy `assignment.<W>` (no `assignment._commit`); start new-worker Manager | manager startup | `CurrentAssignment().Version == legacy.Version`; heartbeat ack reflects it. |
| `TestRollingUpgrade_NewLeaderToOldLeaderHandoff_AliasOverridesStaleCommit` | write commit V=10 LR=20; then write legacy alias V=11 LR=25 | trigger commit watcher then alias watcher | final `CurrentAssignment().Version == 11`; `LSR == 25`. Validates §3.6 "previously-fragile handoff" case. |
| `TestRollingUpgrade_AliasFresherThanCommit_NextCommitTakesOver` | continuation of above: write commit V=12 LR=30 | observe | `CurrentAssignment().Version == 12`. |

### Test setup notes

- Reuse existing `assignment/internal/testutil` harness if present (search via `grep -l "func setupKV\|func newTestPublisher" internal/assignment/*_test.go` before writing).
- Mock `handoff.Coordinator` should expose synchronization primitives (`barrier`, `applyCount`, `applyOrder` slice) to test ordering.
- Mock heartbeat publisher: extract a `heartbeatAckPublisher` interface (`SetAppliedAssignment`, `PublishNow`) so tests can inject a recording double without spinning up the real NATS-backed publisher.

## Order of implementation

Numbered list — implement in this order. Run package tests after each step. Worktree commits at each numbered boundary make rollback cheap.

1. **`types/partition.go`** — add `SourceRevision`, `SourceRevisionKnown` fields to `Assignment`; add `PartitionSetDigest` function. Run `go build ./...` to confirm no callers break (Phase 3 publisher writes legacy aliases with the old `Assignment` shape — additive optional fields are safe).
2. **`types/metrics_collector.go`** — add audit + state-machine metric methods to the interface. Update `internal/metrics/nop.go` and any prom collector to no-op the new methods. Run `go test ./internal/metrics/...` and `go vet ./...`.
3. **`internal/assignment/worker_monitor.go`** — add `GetHeartbeats`. Write a focused unit test that confirms legacy-timestamp + v1-JSON dual decode via `types.DecodeHeartbeat`. Tests for this method live in `internal/assignment/worker_monitor_test.go`.
4. **`internal/assignment/assignment_publisher.go`** — add `lastCommit` field, populate on successful CAS, add `LastCommit()` accessor, add `BootstrapLastCommit(ctx)`. Add a regression test in `assignment_publisher_v1_review_test.go` that verifies `LastCommit()` returns the most recent commit and that it survives a publisher restart via `BootstrapLastCommit`.
5. **`internal/assignment/config.go`** — add `ApplyGracePeriod`, `ExtendedApplyGracePeriod`, `AuditInterval`, `EnableTwoPhaseHandoff` fields + defaults.
6. **`internal/assignment/calculator.go`** — add `auditApplied`, `auditLoop`, new fields, wire into `Start`/`Stop`. Add audit tests from the table above. Cite that this commit must NOT add `monitorPartitions` changes (still source-rev-vs-current logic stays where it is).
7. **`manager.go`** state additions: `lastSeenLeaderRevision`, `lastSeenAlias`, `lastObservedCommit`, `pendingApplyInFlight`, `stashedCommit`.
8. **`manager_assignment.go`** — implement `applyAssignment` (unified); refactor `handleAssignmentEntry` to use it with `selectAuthority`; delete `applyAssignmentUpdate` and inline `applyHandoffAndHooks` step ordering. Add `selectAuthority` pure-function tests first (cheapest). Then run existing `manager_assignment_test.go` and `manager_assignment_fixes_test.go` to catch regressions on the legacy alias path.
9. **`manager_assignment.go`** — add `monitorCommitChanges`, `watchCommit`, `handleCommitEntry`, `handleCommitValue`. Wire into `manager.go Start` step 6. Add state-machine tests.
10. **`manager.go`** initial-assignment bootstrap reordering: state transitions through `applyAssignment`-completion barrier. Delete `applyInitialHandoffAsync` + `emitInitialAssignmentEvents` from `manager_handoff.go`. Add `TestApplyAssignment_InitialBootstrap_AckPublishedBeforeStateStable`.
11. **Wire `heartbeatPublisher` interface extension**: add `SetAppliedAssignment`/`PublishNow` to `manager.go:121`. Update `heartbeat.NewNop()`.
12. **Add watcher reconcile tests** (channel-close, periodic reconcile, delete-ignored).
13. **Add rolling-upgrade tests**.
14. Run full `go test ./...` and `golangci-lint run ./...` — gate the phase on green.

Between each step, run the touched package's tests and `go vet ./...`. Do not advance to the next step on a red bar.

## Out of scope

- Phase 3 publisher internals (commit/CAS/alias barrier/GC).
- Phase 5 (manager wiring) work beyond what's necessary for §4.4 and §4.5 to compile and test inside Phase 4. The brief explicitly pulls those two subsections into Phase 4; do not regress that.
- Phase 6 integration tests under `test/simulation/...`.
- Documentation updates (`docs/API_REFERENCE.md`, CHANGELOG) — Phase 7.

## Notes on design calls baked into this plan

1. `LastCommit()` is added as a cached accessor on the publisher (cheaper than per-tick KV get).
2. `Assignment` gets `SourceRevision`/`SourceRevisionKnown` fields directly rather than a parallel carrier.
3. Hash-verification failure in case (c) leaves pending state intact for retry.
4. `WorkerMonitor.GetHeartbeats` is added even though it wasn't in the brief's "files you will be modifying" list — required by `auditApplied`.
5. The strategy doc and the brief disagree on whether §4.4/§4.5 are Phase 4 or Phase 5; followed the brief (Phase 4).
