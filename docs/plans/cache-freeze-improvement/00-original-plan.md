# Partition Assignment End-to-End Robustness Plan

> **Companion documents (read before implementing):**
> - [`docs/plans/cache-freeze-improvement/02-implementation-strategy.md`](./partition_assignment_implementation_strategy.md)
>   — phase-by-phase dispatch guide (which model, which effort,
>   which packages, which review gates, in what order). Implementing
>   agents should consult this document *before starting each phase*
>   to know what scope to take on, what model/effort to use, and
>   what review gates to clear before merging. The plan below is
>   the authoritative spec; the strategy doc is the operational
>   playbook for executing it.
> - [`docs/plans/cache-freeze-improvement/reviews/plan-reviews/architect-feedback.md`](./partition_assignment_architect_feedback.md),
>   [`docs/plans/cache-freeze-improvement/01-counter-proposal-refs-always.md`](./partition_assignment_counter_proposal_refs_always.md),
>   [`docs/plans/cache-freeze-improvement/reviews/plan-reviews/counter-proposal-feedback.md`](./partition_assignment_counter_proposal_feedback.md),
>   [`docs/plans/cache-freeze-improvement/reviews/plan-reviews/refactored-plan-review.md`](./partition_assignment_refactored_plan_review.md),
>   [`docs/plans/cache-freeze-improvement/reviews/plan-reviews/refactored-plan-review-response.md`](./partition_assignment_refactored_plan_review_response.md)
>   — review history and design-decision rationale.

## Problem statement

User reports: *"sometimes when new partitions are added in the partition source
(NatsKV), the leader doesn't assign part of the added partitions to candidates."*

The reported symptom has at least three independent producers, sitting at
three different layers of the system. Fixing only the source layer is
necessary but not sufficient. The real goal is an **end-to-end coverage
invariant**:

> For source revision **R** and active worker set **W**, there exists a
> committed assignment batch **V** such that the union of *applied*
> assignments across healthy workers in **W** equals the source partition
> ID set at revision **R** exactly once, and the leader has observed every
> worker's apply receipt.

The current code architecture cannot make this invariant true because it
breaks at every layer:

| Class | Layer | Mechanism | Today's behaviour |
|-------|-------|-----------|-------------------|
| **W1** | Source / write | Two writers race on `kv.Put`; last writer clobbers the other | Partitions never reach the leader |
| **W2** | Source / write | Single writer does `List(); append; Update()` with stale in-memory cache | Same lost-update outcome |
| **R1** | Source / read | Watcher channel closes (reconnect/server-side consumer GC) and `watchLoop` doesn't restart | Leader's view freezes |
| **R2** | Source / read | Watcher delivers `KeyValueDelete`/`Purge` without fan-out to listeners | Leader keeps assigning to deleted partitions |
| **R3** | Source / read | Listener registration races first update arrival, or decode error skips an update | Update silently dropped |
| **B1** | Publish | Per-worker assignment keys written sequentially with no commit marker; crash leaves partial batch | Some workers on V, others on V-1; partitions can be double-owned or unowned |
| **B2** | Publish | Stale worker keys cleaned **before** new batch is committed | Window where partition has no owner key |
| **A1** | Apply / ack | Leader has no read-back signal that workers actually applied — `assignment` KV write is treated as success | Apply failures (`handoffCoordinator.Apply` returns error and we continue at `manager_assignment.go:380-390`) leave assigned-but-not-running partitions |
| **A2** | Apply / ack | Worker assignment watcher closes silently (`manager_assignment.go:320-323`), monitor stops, future assignments never seen | Worker stuck on old version while leader thinks it's current |
| **F1** | Fencing | Worker accepts any higher-Version assignment regardless of `LeaderRevision` (`manager_assignment.go:348-352`) | Stale split-brain leader with high version can poison the cluster |
| **S1** | Source / validation | `decodePartitions` accepts any JSON without `Partition.Validate`, no dedupe, no deep copy | Invalid or duplicate IDs make coverage checks meaningless |

The fix has **four pillars**, not two:

1. **Source write safety (CAS + safe mutation helpers)** — closes W1; restricts the orphan invariant to callers using `Modify` / `AddPartitions` / `RemovePartitions`.
2. **Source read robustness (watch + periodic reconcile + delete fan-out + watcher restart)** — closes R1/R2/R3.
3. **Batch publish with commit marker + source-revision tagging + CAS-fenced commit** — closes B1/B2/F1.
4. **Apply receipts via heartbeat + leader audit + worker reconcile + two-phase-gated escalation** — closes A1/A2.

Plus cross-cutting:

- **Source validation/dedupe** — closes S1.

### Invariant scope (explicit)

The "no orphan partitions" invariant has a **strict form** (enforceable
only post-migration) and a **migration form** (no regression vs. today
during the rolling upgrade).

> **Strict invariant** — enforceable iff every active worker reports
> `CapAckV1` in its heartbeat AND `parti.audit.unverifiable_workers == 0`:
> for source revision **R** (where `SourceRevisionKnown=true`), the
> active worker set **W**, and the latest committed assignment batch
> **V** in `assignment._commit`, the union of *applied* assignments
> across **W** equals the source partition ID set at revision **R**
> exactly once.

> **Migration form** — during rolling upgrade, any worker with
> `CapAckV1=0` is `unverifiable`. Strict enforcement is suspended for
> the unverifiable subset. The cluster is no worse than today (no
> regression) but the union is provably correct only over the
> verifiable subset. Strict enforcement automatically engages once
> the unverifiable set empties; no operator action required.

What is **explicitly out of scope** of this invariant:

- `source.Update(list)` callers who pass a stale wholesale list.
  `Update` is preserved as an authoritative-replace primitive ("I own
  the complete desired state") and is documented as such; misuse is
  the caller's bug, not parti's.
- Audit-driven reassignment of a heartbeating-but-behind worker
  requires the **full safety chain** active on both the behind worker
  and the target: `CapAckV1 | CapTwoPhaseHandoff | CapProcessingGate`.
  If any capability bit is missing, audit downgrades to retry-pressure
  only and waits for heartbeat expiry before reassigning. This is
  surfaced via the metric
  `parti.audit.escalation_skipped{reason="cap_missing_..."}` so
  operators can see when the safety net is inactive.

These pillars are independent failure surfaces but they share the
correctness story: the leader needs to *know* the coverage invariant holds,
and the only way to know is to see every layer's commit point.

---

## Scope

Files touched:
- `source/nats_kv.go` + `source/nats_kv_test.go` + `source/nats_kv_dedup_test.go` — Pillar 1, 2, S1
- `internal/assignment/assignment_publisher.go` + sibling tests — Pillar 3 (commit marker, batch publish)
- `internal/assignment/calculator.go` + sibling tests — Pillar 3 (source-revision plumb-through, audit loop) and Pillar 4 (assignment-applied invariant verification)
- `internal/assignment/worker_monitor.go` — Pillar 4 (heartbeat payload extension on read side; leader interprets new fields)
- `manager_assignment.go` + sibling tests — Pillar 4 (worker assignment watcher reconcile + restart, apply receipt write, leader fencing on `LeaderRevision`)
- `manager_handoff.go` + `composite_updater.go` — Pillar 4 (Apply failure → mark degraded, retry until applied; do not store assignment before successful Apply)
- `types/partition.go` / `types/heartbeat.go` (new or extended) — Pillar 4 heartbeat payload, plus `Modify`-related helper types
- `docs/API_REFERENCE.md` and `docs/CONSUMERS.md` — public API additions + operator-facing migration notes

Files **explicitly removed from the prior carveout**:
- `internal/assignment/calculator.go` is now in scope (was: "not touched"). The end-to-end invariant cannot be verified from the source layer alone.

Public-API impact: **additive only at the parti-user surface.** `Update()`, `NewNatsKV()`, `Manager.*` keep their signatures. New methods are additive. Internal KV schemas evolve in two distinct ways:
- **`assignment.*` keys**: JSON-additive — old decoders ignore unknown fields, new decoders treat missing fields as zero.
- **`heartbeat.<W>` keys**: **dual-format**. Old code writes raw RFC3339 timestamp bytes; new code writes v1 JSON. The new reader (`DecodeHeartbeat`) accepts both formats. Old readers cannot parse v1 JSON heartbeats but do not need to (they don't run the new audit).

See "Backward compatibility & migration" for the full schema-evolution story.

---

## Pillar 1 — Write-side: CAS-fenced Update + safe mutation helpers

**API scope statement.** The "no orphan partitions" invariant holds for
callers using `Modify` / `AddPartitions` / `RemovePartitions`. `Update`
is preserved as an authoritative-replace primitive ("I own the
complete desired state"), gains CAS to protect concurrent
authoritative writers from clobbering each other, but does **not**
protect against caller-provided stale lists. Callers who do
`List(); append; Update(list)` are misusing the API; the safe pattern
is `Modify`.

### 1.1 Track current revision

Add a field to `NatsKV` populated by the watcher and the initial `Get`:

```go
type NatsKV struct {
    // ...existing fields...
    revision uint64  // last observed KV revision
    known    bool    // false only for ErrKeyNotFound at Start() before
                     // any watcher event; once any KV event arrives
                     // (including delete/purge), known stays true
}
```

`Start()` seeds `s.revision` from the initial `kv.Get`
(`entry.Revision()`) with `known=true`. On `ErrKeyNotFound`,
`revision=0, known=false` (the source has never been written by
anyone — distinct from "the source was written then deleted").

`watchLoop` updates `(s.revision, s.known)` whenever it processes a
non-nil entry, **before** the listener fan-out, under the same
`s.mu.Lock()`. Crucially, **delete/purge events preserve the
delete entry's KV revision** (`entry.Revision()` is non-zero on
delete events in NATS KV) and set `known=true`. An empty source
caused by a delete is still a *known* source revision and is
distinct from a never-written source.

`Snapshot(ctx)` returns `(partitions, s.revision, s.known, nil)`.
The `known` value flows through to `AssignmentCommit.SourceRevisionKnown`
and `Heartbeat.AppliedSourceRevKnown` (§3.3, §4.1) so the audit can
distinguish "revisioned source, empty due to delete" from
"non-revisioned source" — the former is auditable, the latter is
not.

### 1.2 `Update()` becomes CAS-safe with internal retry

Current code calls `kv.Put` unconditionally. Replace with a CAS loop:

```go
func (s *NatsKV) Update(ctx context.Context, partitions []types.Partition) error {
    data, err := encode(partitions)
    if err != nil {
        return err
    }

    const maxAttempts = 5
    for attempt := 0; attempt < maxAttempts; attempt++ {
        s.mu.RLock()
        rev := s.revision
        s.mu.RUnlock()

        var newRev uint64
        if rev == 0 {
            newRev, err = s.kv.Create(ctx, s.key, data)
        } else {
            newRev, err = s.kv.Update(ctx, s.key, data, rev)
        }

        if err == nil {
            // Optimistically update local state so an immediate List() sees
            // the new value without waiting for the watcher round-trip.
            s.applyLocal(partitions, newRev, notifyListeners: false)
            return nil
        }

        if !isCASConflict(err) {
            return fmt.Errorf("failed to update partitions in KV: %w", err)
        }

        // Refresh revision from KV (don't trust the stale local one) and retry.
        if rerr := s.refreshFromKV(ctx); rerr != nil {
            return fmt.Errorf("refresh after CAS conflict: %w", rerr)
        }
    }
    return fmt.Errorf("failed to update partitions: exhausted %d CAS retries", maxAttempts)
}
```

Notes:
- Semantics unchanged for the *single-writer wholesale-replace* case
  — the retry just re-issues the same put with a fresh revision until
  it lands.
- `isCASConflict` checks for both `jetstream.ErrKeyExists` (when
  creating) and the wrong-revision error returned by `Update`.
- **What CAS does and doesn't do (F6 correction).** CAS makes write
  conflicts **observable and retryable** and prevents silent failed
  writes from going unnoticed. It does **not** merge divergent
  authoritative replaces. When two callers issue `Update` with
  different lists, they serialize through CAS retry and last-writer-
  wins semantics still apply at the protocol level — both writes
  succeed in series, observable to the watcher, but the earlier one
  is no longer visible. Use `Modify` (§1.3) when merge semantics are
  required.
- Compressed-payload-size error message is preserved.

### 1.3 New `Modify()` helper for read-modify-write callers

The caller pattern `list, _ := List(); list = append(list, p); Update(list)`
loses updates even with CAS, because the read can be stale and the caller's
intent ("add p") gets replayed against whatever the conflict resolves to.

Add a single-function API that does CAS-retry with a *fresh* read each
iteration:

```go
// Modify atomically transforms the partition list using fn, retrying on
// concurrent writes. fn receives a fresh snapshot from KV (not the local
// cache) on every attempt and must be deterministic and side-effect-free —
// it may be invoked multiple times.
func (s *NatsKV) Modify(ctx context.Context, fn func([]types.Partition) []types.Partition) error
```

Implementation: loop reads `kv.Get(ctx, key)`, decodes, applies `fn`,
encodes, calls `kv.Update` with the observed revision, retries on conflict.
On `ErrKeyNotFound`, treat as empty list with revision 0 and use `kv.Create`.

Document the canonical usage:

```go
err := src.Modify(ctx, func(current []types.Partition) []types.Partition {
    return append(current, types.Partition{Keys: []string{"new"}})
})
```

### 1.4 `applyLocal` helper

Small refactor: the watcher's "sort + compare + replace + fan out" block and
`Update`'s "I just wrote this, update local cache" block should share one
internal helper to ensure they apply identical canonicalization (sort) and
revision tracking. Same listener fan-out semantics (skip if buffer full).

---

## Pillar 2 — Read-side: watch + periodic reconcile

### 2.1 Add a reconcile ticker alongside the watcher

`Start()` spawns a second goroutine:

```go
go s.watchLoop(s.ctx, s.watcher)
go s.reconcileLoop(s.ctx)
```

```go
func (s *NatsKV) reconcileLoop(ctx context.Context) {
    interval := s.reconcileInterval  // default 30s, see 2.4
    if interval <= 0 {
        return  // disabled
    }
    t := time.NewTicker(interval)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            s.reconcileOnce(ctx)
        }
    }
}

func (s *NatsKV) reconcileOnce(ctx context.Context) {
    entry, err := s.kv.Get(ctx, s.key)
    // Treat ErrKeyNotFound as empty list (mirrors Start).
    // Log + return on other errors.
    // Decode, sort, then go through the same applyLocal path the watcher
    // uses — diff against s.partitions, fan out to listeners only on change.
}
```

Cost: one `kv.Get` per interval per process. Negligible.

### 2.2 Idempotency

The reconciliation path **shares** the watcher's "compare → replace → fan
out" code via `applyLocal`. If the watcher already saw the latest, the poll
is a no-op (the `partitionsEqual` check short-circuits, no listener signal).
This is the only correctness invariant: poll and watch must converge on
identical local state given identical KV state.

### 2.3 Watcher channel close handling (closes R1)

Replace `entry := <-watcher.Updates()` with `entry, ok := <-watcher.Updates()`.
On `!ok`:

```go
case entry, ok := <-watcher.Updates():
    if !ok {
        // Channel closed — re-establish watcher with backoff.
        // Reconcile loop continues to function as the safety net during
        // this period.
        if err := s.restartWatcher(ctx); err != nil {
            // Log; reconcile will keep us correct until next attempt.
            backoff()
            continue
        }
        return  // exit current watchLoop; restartWatcher spawned a new one
    }
```

Mirror the exponential-backoff pattern from `manager_assignment.go:268`
(`monitorAssignmentChanges`). Even if `restartWatcher` keeps failing, the
reconcile loop guarantees eventual convergence within one interval.

### 2.4 Configuration

Add a constructor option:

```go
type NatsKVOption func(*NatsKV)

func WithReconcileInterval(d time.Duration) NatsKVOption  // 0 disables; default 30s

func NewNatsKV(kv jetstream.KeyValue, key string, logger types.Logger, opts ...NatsKVOption) *NatsKV
```

Backwards-compatible: existing `NewNatsKV(kv, key, logger)` calls still
compile; they get the default 30s reconcile. Tests that want deterministic
behaviour pass `WithReconcileInterval(0)` to disable polling, or a small
value to drive timing.

### 2.5 Fix delete/purge fan-out (closes R2)

In `watchLoop`, the delete/purge branch currently sets `s.partitions = nil`
and `continue`s without notifying listeners. Move it through `applyLocal`
so listeners fire on transitions to/from empty, **and preserve the
delete entry's KV revision** (matching the design in §1.1 — empty
source caused by delete is still a *known* source revision):

```go
if entry.Operation() == jetstream.KeyValueDelete || entry.Operation() == jetstream.KeyValuePurge {
    // NATS KV delete events carry their own revision; preserve it so
    // Snapshot() returns (nil, entry.Revision(), known=true). Do NOT
    // pass revision=0 here — that would conflate a known-empty source
    // with a never-written source and break the audit's source-revision
    // check (review P1 #3).
    s.applyLocal(nil, entry.Revision(), true /*known*/, true /*notify*/)
    continue
}
```

The `applyLocal` helper signature carries both `revision` and `known`
so callers cannot accidentally drop the knownness bit.

---

## API surface summary

Complete exported API (closes review full-pass P1 #4 — every
identifier referenced in the plan or tests is given a concrete
signature here):

```go
// ── types package (additive — does NOT modify PartitionSource) ──

// RevisionedPartitionSource is an OPTIONAL extension interface.
// Sources that track revisions (NatsKV) implement it; sources
// that don't (Static) do not. The calculator type-asserts and
// falls back to List() with SourceRevisionKnown=false when the
// assertion fails.
type RevisionedPartitionSource interface {
    types.PartitionSource
    Snapshot(ctx context.Context) (partitions []types.Partition,
                                    revision uint64,
                                    known bool,
                                    err error)
}

// Partition gains a collision-safe canonical identity.
// ID() remains unchanged for durable consumer names / logs.
func (p Partition) CanonicalID() string

// Heartbeat decoder accepts both v1 JSON and legacy RFC3339
// timestamp string formats.
func DecodeHeartbeat(b []byte) (Heartbeat, error)

// Heartbeat capability bitmask values.
const (
    CapAckV1            uint32 = 1 << 0
    CapTwoPhaseHandoff  uint32 = 1 << 1
    CapProcessingGate   uint32 = 1 << 2
)

// ── source package (NatsKV — existing signatures preserved, new methods additive) ──

// Existing — signatures unchanged, semantics hardened.
func NewNatsKV(kv jetstream.KeyValue, key string, logger types.Logger, opts ...NatsKVOption) *NatsKV
func (s *NatsKV) Start(ctx context.Context) error
func (s *NatsKV) Stop(ctx context.Context) error
func (s *NatsKV) List(ctx context.Context) ([]types.Partition, error)
func (s *NatsKV) Watch(ctx context.Context) <-chan struct{}
func (s *NatsKV) Update(ctx context.Context, partitions []types.Partition) error // now CAS-fenced

// New on NatsKV.
func (s *NatsKV) Snapshot(ctx context.Context) (partitions []types.Partition,
                                                 revision uint64,
                                                 known bool,
                                                 err error)        // implements RevisionedPartitionSource
func (s *NatsKV) Modify(ctx context.Context, fn func([]types.Partition) []types.Partition) error
func (s *NatsKV) AddPartitions(ctx context.Context, partitions ...types.Partition) error    // sugar over Modify; dedupes by CanonicalID
func (s *NatsKV) RemovePartitions(ctx context.Context, partitions ...types.Partition) error // sugar over Modify; matches by CanonicalID

// New NatsKVOption constructors.
func WithReconcileInterval(d time.Duration) NatsKVOption        // single interval (default 30s)
func WithLeadershipProbe(fn func() bool) NatsKVOption           // when set, reconcile uses leader=30s / follower=5min cadence
func WithUpdateRetries(n int) NatsKVOption                      // CAS-retry budget for Update() / Modify() (default 5)

// Typed error surfaced when Update/Modify exhaust the CAS retry budget.
var ErrUpdateRetryExhausted error

// ── manager (parti package) ──

// New on Manager — runtime capability reporting from the
// consumer/updater and two-phase coordinator (§4.1).
func (m *Manager) SetCapability(cap uint32, active bool)
func (m *Manager) Capabilities() uint32   // current bitmask (atomic read)
```

No removals. No breaking changes. `types.PartitionSource` is
**not** extended; `RevisionedPartitionSource` is a separate
optional interface satisfied by NatsKV but not by Static, so
existing custom-source implementations keep compiling.

Calculator usage pattern (the only place the optional interface
is consumed):

```go
var partitions []types.Partition
var srcRev uint64
var srcKnown bool
if rs, ok := c.Source.(types.RevisionedPartitionSource); ok {
    partitions, srcRev, srcKnown, err = rs.Snapshot(ctx)
} else {
    partitions, err = c.Source.List(ctx)
    srcRev, srcKnown = 0, false
}
```

---

## Test plan

Tests are distributed across `source/`, `internal/assignment/`,
`internal/heartbeat/`, and top-level `manager_*_test.go` files
according to the package they exercise. End-to-end invariant tests
live under `test/` (the existing integration harness) using
`partitest.StartEmbeddedNATS`.

### CAS / write-path (source/)
1. `TestNatsKV_Modify_ConcurrentWritersDoNotLoseEachOther` — N=10
   goroutines each call `Modify` to add one unique partition; assert KV
   ends with all 10. (Renamed from earlier draft, which mislabeled this
   as `Update_*` despite using `Modify`.)
2. `TestNatsKV_Update_CASRetryOnConflict` — pre-bump revision via raw KV
   put between `Update`'s read and write; assert eventual success
   (authoritative-replace semantics, not lost-update protection).
3. `TestNatsKV_Update_ImmediateListSeesNewValue` — call `Update(x)`;
   call `List()` synchronously immediately; assert it returns `x`
   (in-memory cache freshness).
4. `TestNatsKV_Modify_SeesFreshKVNotCache` — concurrently `Update` then
   `Modify`; assert the `Modify` callback sees the post-`Update`
   snapshot read directly from KV (not the local cache).
5. `TestNatsKV_Update_IsAuthoritativeReplace_NotLostUpdateSafe` —
   pre-write `[A,B,C]`, call `Update([A,B])` from a stale caller; assert
   KV ends at `[A,B]`. Documents the API contract: `Update` is
   wholesale replace; callers needing merge semantics must use
   `Modify`.

### Reconcile / read-path
6. `TestNatsKV_Reconcile_RecoversFromMissedWatcherEvent` — start source with
   short reconcile (200ms); inject a missed event by directly `kv.Put`ing
   while listener consumption is paused; assert listener eventually fires
   *and* `List()` matches KV.
7. `TestNatsKV_Reconcile_NoSignalWhenInSync` — assert that the poll does not
   spuriously fire listeners when KV and cache agree.
8. `TestNatsKV_WatcherRestart_OnChannelClose` — close the watcher channel
   (or stop+restart NATS); assert source recovers and a subsequent `Update`
   is observed.

### Delete / purge
9. `TestNatsKV_DeleteOperation_NotifiesListeners` — `Update(x)`, drain
   listener; `kv.Delete(key)`; assert listener fires and `List()` returns
   empty.

### Existing tests
- `TestNatsKV_Watch_Deduplication` must continue to pass unchanged.

### Batch publish / commit / payload refs (Pillar 3, `internal/assignment/`)
10.  `TestPublisher_Crash_BeforeCommit_PayloadsInert` — write payload
    keys, kill calculator before `assignment._commit` write; restart
    new leader: assert orphan payload keys exist but are not
    referenced by any committed commit; assert next publish writes
    a fresh commit at V+1; assert orphans eventually reaped by GC.
11. `TestPublisher_LegacyBootstrap_NoCommit_RecoversViaDiscoverHighestVersion`
    — pre-populate KV with legacy `assignment.<W>` keys at V=N, no
    `assignment._commit`; start new-version leader; assert it discovers
    N, publishes V=N+1 with the first commit, doesn't reset to V=1.
12. `TestPublisher_CommitCAS_AbortsOnStaleLeader` — two calculators
    with different LeaderRevisions race; both write payload keys;
    assert the stale leader's `kv.Update(assignment._commit, ..., prevRev)`
    CAS fails and the stale leader aborts; assert workers apply only
    the winning leader's commit.
13. `TestPublisher_CommitCAS_AbortsOnLeadershipLost` — calculator
    writes payload keys, then loses election; assert the pre-commit
    leadership re-check aborts the batch (no commit written, no
    workers apply).
14. `TestPublisher_LosingLeaderPayloadWriteCannotCorruptWinningCommit`
    (F1 architect test) — winning leader L1 commits payload refs;
    losing leader L2 then writes its own payload keys (which kv.Create
    either rejects as ErrKeyExists with identical content, or accepts
    at a different digest key). After L2's commit CAS fails, assert
    no worker observes L2's intent: workers fetch payload by L1's
    refs and apply L1's payload bytes.
15. `TestPublisher_InlineSizeRegression_DoesNotApply` — (no inline
    mode in refs-always design) sanity test that commit blob never
    contains inline `Partitions` arrays beyond the `Payloads` map of
    refs; commit size stays under 10 KB for the test profile.
16. `TestWorker_CommitRefPayloadMissing_ClassifiesMalformed` (F1
    architect test) — commit references a payload key that doesn't
    exist (force-deleted via direct KV ops); worker emits
    `parti.worker.commit_payload_missing`, does not apply; audit
    classifies as malformed and schedules re-publish.
17. `TestWorker_CommitRefDigestMismatch_RejectsPayload` (F1 architect
    test) — payload exists but its sha256 differs from
    `ref.PayloadHash`; worker emits
    `parti.worker.payload_hash_mismatch`, rejects, does not apply.
18. `TestWorker_PayloadSetDigestMismatch_RejectsPayload` — payload
    hash matches but `xxh3(sorted partition IDs in payload) !=
    ref.SetDigest`; worker emits `parti.worker.set_digest_mismatch`,
    rejects.
19. `TestPublisher_ErrKeyExists_VerifiedAndReused` — pre-populate
    `assignment._payload.<hex(sha256)>` with the exact same canonical
    bytes the publisher will produce; assert publish-time
    `kv.Create` returns `ErrKeyExists`, verification succeeds,
    metric `payloads_reused` increments, ref carries the
    pre-existing revision.
20. `TestPublisher_ErrKeyExists_HashMismatchSurfacesCollisionError`
    — pre-populate the same key with different bytes (force-write
    via direct KV ops); assert publisher returns
    `ErrPayloadHashCollisionOrCorruption` and aborts the batch.
21. `TestPublisher_CrossCommitReuse_PayloadUnchanged` — publish V
    with unchanged slice for W; publish V+1 with the same slice for W;
    assert W's payload key is the same; assert
    `kv.Create` for that key in V+1 returns `ErrKeyExists` (reuse);
    assert wire bytes written for that worker in V+1 ≈ 0.
22. `TestWorker_RemovedFromCommit_AppliesEmptyAssignmentAndAcks`
    (F1 architect test) — commit V+1 omits W from `commit.Workers`;
    W's previous AppliedVersion was V; assert W applies empty
    assignment at V+1 through the receipt path, revokes consumers,
    publishes ack with empty `AppliedDigest`.
23. `TestPublisher_SetEqualityCoversAllPartitions` — inject a buggy
    strategy that drops one partition; assert publisher aborts the
    batch at publish-time invariant check (does not write commit);
    metric `parti.publisher.batch_aborted{reason="coverage_mismatch"}`
    increments.
24. `TestPublisher_SourceRevisionInCommit` — call `Snapshot` against a
    KV source at a known revision; assert published `assignment._commit`
    carries `SourceRevision` and `SourceRevisionKnown=true`.
25. `TestPublisher_StaticSource_SourceRevisionUnknown` — assert
    calculator falls back to `List()` for sources that don't
    implement `RevisionedPartitionSource`; assert commit carries
    `SourceRevisionKnown=false`; assert audit skips source-revision
    check for assignments from this snapshot.
26. `TestPublisher_PayloadGC_DoesNotDeleteCurrentCommitPayloads`
    (F1 architect test) — run several publishes; trigger GC pass;
    assert all payloads referenced by `assignment._commit` and by the
    last K `assignment._commit_log.*` entries are retained; assert
    older unreferenced payloads outside retention window are deleted.
27. `TestPublisher_CommitLog_WriteFailureDoesNotBlockCommit` — inject
    failure on `kv.Create(assignment._commit_log.V)`; assert publish
    succeeds (commit was already written before the log); assert GC
    handles missing log entry by being more conservative (keeps more
    payloads).

### Apply receipts / audit (Pillar 4, `internal/assignment/`, `internal/heartbeat/`, `manager_*_test.go`)
28. `TestHeartbeat_SetAppliedAssignmentMonotone` — call
    `SetAppliedAssignment` with V=5 then V=3; assert the V=3 update
    is ignored.
29. `TestHeartbeat_PublishNow_LeaderObservesWithinOneRoundTrip` — call
    `SetAppliedAssignment` then `PublishNow`; assert
    `WorkerMonitor.GetHeartbeats` on the leader returns the new
    `AppliedVersion` in < HeartbeatInterval.
30. `TestHeartbeat_CapabilitiesReflectWiredComponents` — start a
    worker with two-phase off; assert `Capabilities & CapTwoPhaseHandoff == 0`;
    start with two-phase on AND processing-gate wired; assert
    `Capabilities == CapAckV1 | CapTwoPhaseHandoff | CapProcessingGate`;
    start with two-phase on but processing-gate not configured;
    assert `CapProcessingGate` is NOT set (bits reflect actual
    wire-up, not config alone).
31. `TestApply_HeartbeatReflectsAppliedVersion` — publish V; assert
    every new-schema worker's heartbeat updates to `AppliedVersion=V`
    within Apply grace window.
32. `TestApply_FailureKeepsHeartbeatBack_RetryPressureOnly` — inject
    Apply failure on one worker; assert heartbeat stays on V-1;
    assert leader audit classifies as `behind` but does NOT escalate
    within `ApplyGracePeriod`; assert metric
    `parti.audit.workers_behind` increments.
33. `TestApply_ExtendedGrace_CapMissing_SkipsEscalation` — same as 31
    but extend past `ExtendedApplyGracePeriod` with the behind worker
    missing `CapTwoPhaseHandoff` (or `CapProcessingGate`); assert no
    rebalance fires; assert
    `parti.audit.escalation_skipped{reason="cap_missing_behind"}`
    increments.
34. `TestApply_ExtendedGrace_TargetCapMissing_SkipsEscalation` — same
    as 31 but the only available *targets* lack the required caps;
    assert no rebalance fires; assert
    `parti.audit.escalation_skipped{reason="cap_missing_targets"}`
    increments.
35. `TestApply_ExtendedGrace_DirectMode_SkipsEscalation` — same as 31
    with `EnableTwoPhaseHandoff = false`; assert
    `parti.audit.escalation_skipped{reason="direct_mode"}` increments;
    assert no rebalance fires.
36. `TestApply_ExtendedGrace_FullChain_EscalatesViaClaims` — same as
    31 with full safety chain on all workers; extend past
    `ExtendedApplyGracePeriod`; assert audit-driven rebalance fires;
    assert the target worker claims via two-phase; assert the
    **stuck worker's processing-gate observes the ownership change
    and stops consuming** (test reads the processing-gate state
    directly to confirm — does not rely on the stuck worker's
    coordinator releasing, which it cannot since its apply is
    broken).
37. `TestApply_DigestMismatchClassifiesBehind` — manually corrupt the
    `AppliedDigest` field in a worker's heartbeat to differ from
    `commit.Payloads[W].SetDigest`; assert audit classifies behind.
38. `TestApply_SourceRevisionInCommit_NotCurrent` — publish commit V
    at source revision S; advance source to S+1 without publishing
    V+1; assert audit does NOT mark any worker behind (they
    correctly applied commit V); assert a rebalance for V+1 is
    separately scheduled by `monitorPartitions`.
39. `TestApply_SourceRevisionUnknownSkipsAuditCheck` — publish from a
    static source (`SourceRevisionKnown=false`); change worker's
    `AppliedSourceRevision` to garbage; assert audit ignores the
    field and classifies based on other criteria.
40. `TestCommitWatcher_RestartOnChannelClose` — close the worker's
    `assignment._commit` watcher; assert it re-establishes and applies
    a subsequent commit.
41. `TestCommitWatcher_ReconcileCatchesMissedEvent` — write
    `assignment._commit` directly while bypassing the watcher channel;
    assert reconcile applies it within reconcile interval.
42. `TestApply_RejectStaleLeaderRevision` — write a commit with
    higher Version but lower LeaderRevision than the new worker's
    last seen; assert worker rejects.
43. `TestApply_InitialAssignment_GoesThroughReceiptPath` — start a
    fresh worker; assert it does NOT transition to `StateStable`
    until `applyAssignment` completes (including ack publish);
    assert the leader's audit sees the new worker as
    `fully_applied` immediately upon `StateStable`.

### Source validation / dedupe (S1)
44. `TestNatsKV_Update_RejectsInvalidPartition` — write a partition
    with a dot in its key; assert `Update` and `Modify` return error.
45. `TestNatsKV_Update_RejectsDuplicateIDs` — write two partitions
    with the same `CanonicalID()`; assert error.
46. `TestNatsKV_List_DeepCopiesKeys` — mutate the returned slice;
    assert internal state unchanged.

### Mixed-version rolling-upgrade (review P0 #1 + P0 #2 + P1)
47. `TestRollingUpgrade_OldLeaderNewWorker_AppliesLegacyAlias` — old
    leader writes only `assignment.<W>` (no commit). New worker with
    `CapAckV1=1` but observing no `assignment._commit` falls back to
    the legacy-compat path, applies the alias, publishes apply
    receipt. Assert partitions are not orphaned.
48. `TestRollingUpgrade_NewLeaderOldWorker_AliasWriteRequiredBeforeCommit`
    — fleet contains one legacy worker (`CapAckV1=0`); inject a
    failure on `kv.Put(assignment.<oldW>)`; assert publisher aborts
    the batch before writing `assignment._commit`; assert metric
    `parti.publisher.alias_barrier_failed` increments; assert no
    commit at version V exists in KV.
49. `TestRollingUpgrade_NewToOldLeader_NewWorkerFallsBackToLegacyAlias`
    — pre-populate `assignment._commit` at `LeaderRevision=R1`;
    write a legacy alias at `LeaderRevision=R2>R1`; assert the new
    worker switches from commit path to legacy-compat path and
    applies the alias. Then write a fresh commit at
    `LeaderRevision=R3>R2`; assert the worker switches back to
    commit path.
50. `TestNatsKV_DeletePreservesKnownRevision` (review P1 #3) —
    populate the source key, then delete it; assert
    `Snapshot()` returns `(empty, deleteEntryRevision != 0, true,
    nil)` — empty partition list with `known=true` and the actual
    delete entry's KV revision.
51. `TestAudit_KnownCommitRequiresKnownAppliedSourceRevision`
    (review P1 #4) — publish a commit with
    `SourceRevisionKnown=true`; force a worker's heartbeat to
    omit `AppliedSourceRevKnown`; assert audit classifies the
    worker as `behind`, not `fully_applied`.
52. `TestHeartbeat_CapProcessingGateReflectsActualWireup`
    (review P1 #5) — config says `EnableTwoPhaseHandoff=true` but
    consumer is configured without the processing gate; assert
    heartbeat shows `CapTwoPhaseHandoff=true` but
    `CapProcessingGate=false`. Reconfigure consumer with the
    gate; assert both bits flip to true. Force gate init failure;
    assert `CapProcessingGate` flips back to false.
53. `TestPartitionCanonicalID_NoTupleCollision` (review P1 #6) —
    construct `["a-b", "c"]` and `["a", "b-c"]`; assert
    `ID()` collides on `"a-b-c"`, assert `CanonicalID()` does NOT
    collide (`"3:a-b/1:c"` vs `"1:a/3:b-c"`); assert digest and
    set-equality paths distinguish them.
54. `TestAssignmentDiscovery_IgnoresProtocolKeys` (review P2 #7) —
    pre-populate `assignment._commit`, `assignment._commit_log.5`,
    `assignment._payload.<hash>`, and `assignment.worker-1`;
    invoke `DiscoverHighestVersion`; assert it returns
    `worker-1`'s legacy version only, never tries to interpret
    protocol keys as worker IDs; assert `cleanupStaleAssignments`
    never deletes protocol keys.

### Heartbeat dual decoder + alias barrier hardening (follow-up review P0/P1)
55. `TestHeartbeat_DecodeLegacyTimestampString` — feed
    `time.Now().Format(time.RFC3339Nano)` bytes into
    `DecodeHeartbeat`; assert it returns
    `{SchemaVersion:0, Capabilities:0, Timestamp:parsed, ...zero}`
    without error.
56. `TestHeartbeat_DecodeV1JSON` — feed a v1 JSON payload (starts
    with `{`); assert all new fields decode correctly and
    `SchemaVersion=1`, `Capabilities` matches the writer's bits.
57. `TestHeartbeat_DecodeMalformed_ReturnsError` — feed neither
    valid JSON nor parseable timestamp; assert the decoder returns
    an error (not a silently-degraded `CapAckV1=0` heartbeat).
58. `TestWorkerMonitor_GetHeartbeats_MixedLegacyTimestampAndJSON`
    — populate KV with a mix of legacy timestamp keys and v1 JSON
    keys; assert `GetHeartbeats` returns both worker IDs with
    correctly-classified `SchemaVersion`/`Capabilities`.
59. `TestPublisher_LegacyAliasBarrier_UsesTimestampHeartbeatAsLegacyWorker`
    — populate a worker's heartbeat as a raw timestamp string;
    publish a batch including that worker; assert the publisher
    correctly classifies it into `legacy_in_batch` and treats its
    alias write as mandatory pre-commit.
60. `TestPublisher_AliasBarrier_RechecksLeadershipBeforeAliasWrites`
    (review P1 #5) — leader writes payloads, then loses leadership
    before reaching the alias barrier (step 5 leadership check);
    assert the publisher aborts the batch before any alias write
    lands; assert no `assignment.<W>` for the batch's workers is
    modified.
61. `TestPublisher_AliasBarrier_CASFailureAfterAliases_DocumentedMigrationExposure`
    (review P1 #5) — leader writes payloads + aliases (steps 4-6),
    leadership is lost between step 7 and step 9 CAS; assert the
    commit CAS fails, the batch aborts, BUT some old workers may
    already have applied the aliases. Documents the mixed-version
    floor; assert the cluster recovers to V-1 as the authoritative
    view; assert metric `parti.publisher.alias_visible_uncommitted`
    fires. **Additional assertion (full-pass review P1 #1):** the
    legacy timestamp heartbeats of those old workers do NOT show
    `AppliedVersion=V` — they show only `Timestamp` (the legacy
    payload doesn't carry ack fields). Recovery does not depend on
    the audit detecting drift via legacy ack fields; the next
    successful publish unconditionally overwrites the alias.

### Source API surface tests (full-pass review P1 #4)
62. `TestPublisher_PostAliasLeadershipLoss_AbortsBeforeCommitCAS`
    — payloads + aliases written successfully (steps 4-6); inject
    leadership loss between step 6 and step 7; assert the
    post-alias leadership recheck (step 7) fires, the commit CAS
    is NEVER attempted, and `parti.publisher.commit_aborts` does
    NOT increment (the abort happened before the CAS, not at it).
63. `TestNatsKV_AddPartitions_UsesModifyAndPreservesConcurrentAdds`
    — N=10 goroutines each call `AddPartitions(uniquePartition)`;
    assert KV ends with all 10; assert no `Update` was called
    directly (internally implemented via `Modify`); assert dedupe
    by `CanonicalID` (calling twice with the same partition is a
    no-op, not an error).
64. `TestNatsKV_RemovePartitions_UsesModifyAndPreservesConcurrentMutations`
    — pre-populate with 10 partitions; N=5 goroutines each call
    `RemovePartitions` with a non-overlapping subset of 2;
    concurrently a 6th goroutine `AddPartitions` an 11th; assert
    final state is the unremoved 0 + the new 1; assert all
    Modify calls succeed (no lost updates).
65. `TestCalculator_RevisionedSourceUsesSnapshot_NonRevisionedSourceFallsBackToList`
    — calculator with NatsKV source asserts `Snapshot` is called
    and commit carries `SourceRevisionKnown=true`; calculator with
    Static source (no `RevisionedPartitionSource` impl) asserts
    `List` is called and commit carries `SourceRevisionKnown=false`.
66. `TestNatsKV_ReconcileInterval_LeadershipProbeSelectsLeaderFollowerCadence`
    — `WithLeadershipProbe(fn)` where `fn` toggles; assert the
    reconcile timer fires every 30s when leader, every 5min when
    follower; assert no toggle missed.

67. `TestNatsKV_WithUpdateRetries_ExhaustionReturnsTypedError`
    (precision-pass review P1 #2) — set `WithUpdateRetries(1)`;
    inject a persistent CAS conflict (background goroutine bumps
    the revision faster than retry); assert `Update` returns
    `ErrUpdateRetryExhausted` (typed) rather than a wrapped NATS
    error.

### End-to-end invariant
68. `TestE2E_AddPartitionsConvergesToInvariant` — start 3 workers;
    add 10 partitions via `Modify`; assert within bounded time
    every partition has exactly one owner across the worker fleet
    and every worker's `AppliedDigest` matches its
    `commit.Payloads[W].SetDigest`.
69. `TestE2E_PartitionAdditionDuringLeaderChange` — same scenario
    but kill the leader mid-add; assert eventual invariant.
70. `TestE2E_PartialApplyFailureRecovery` — same scenario but
    inject handoff failure on one worker; assert invariant after
    audit-driven reassignment (full capability chain enabled).

---

## Documentation

Phase 7 documents every additive public API from the API surface
summary. Required coverage:

- `docs/API_REFERENCE.md`:
  - **Source package**: `Modify`, `AddPartitions`,
    `RemovePartitions`, `Snapshot`, `WithReconcileInterval`,
    `WithLeadershipProbe`, `WithUpdateRetries`. Add a "concurrent
    updates" subsection explaining when to prefer `Modify` over
    `Update`. Add a "revisioned vs static source" subsection
    explaining the optional `RevisionedPartitionSource` interface.
  - **Types package**: `RevisionedPartitionSource` interface,
    `Partition.CanonicalID`, `DecodeHeartbeat`, capability
    constants (`CapAckV1` / `CapTwoPhaseHandoff` /
    `CapProcessingGate`), `Heartbeat` payload, `Assignment`
    payload, `AssignmentCommit` schema, `AssignmentPayloadRef`.
  - **Manager**: `SetCapability`, `Capabilities`. Note that
    `SetCapability` is called by the consumer/updater and
    two-phase coordinator at wire-up, not by user code.
- Godoc:
  - `Update` — note CAS semantics, retry bound,
    `ErrUpdateRetryExhausted`, and the contract that it is the
    "authoritative replace" primitive (not lost-update safe).
  - `Modify` / `AddPartitions` / `RemovePartitions` — concurrent
    mutation semantics; dedupe by `CanonicalID`; idempotency on
    duplicate add.
  - `NatsKV` type — "watch + periodic reconcile" design, default
    interval, optional leader/follower cadence.
  - `RevisionedPartitionSource` — optional extension; document
    that `known=false` from `Snapshot` (or absence of the
    interface) disables source-revision audit checks for
    assignments from that snapshot.
  - `DecodeHeartbeat` — dual-format behaviour (v1 JSON or legacy
    RFC3339 timestamp), legacy synthesizes
    `{SchemaVersion:0, Capabilities:0, Timestamp:parsed}`,
    malformed payloads return parse errors (not silently
    degraded).
  - `Manager.SetCapability` — runtime wire-up reporting, not
    config; bit must reflect actual wire-up state of the
    corresponding safety mechanism.
- `CHANGELOG.md` — entry for the release that lands these changes.

---

## Rollout / risk

- Changes span **source**, **internal/assignment** (publisher,
  calculator, worker monitor), **internal/heartbeat** (publisher),
  **top-level manager** (assignment watcher, handoff coordinator,
  initial-assignment path), **types** (heartbeat type, schema-version
  fields), and **docs**. The earlier "all in source" framing has been
  retired with the scope expansion.
- Reconcile cost (leader 30s / followers 5min): see "Polling cost"
  section — corrected to ~33 RPS at 1000 nodes for follower polls in
  steady state, well under NATS load budgets; configurable.
- CAS retry on `assignment._commit` is bounded; a perpetually-conflicting
  publish loop self-aborts and surfaces metric
  `parti.publisher.commit_aborts`. The fix is operator-level (resolve
  split-brain), not infinite retry.
- New protocol elements:
  - `assignment.*` keys (`_commit`, `_commit_log.<V>`,
    `_payload.<hex(sha256)>`) — old peers do not read these.
  - Heartbeat `SchemaVersion` + `Capabilities` + ack fields are
    in the **new v1 JSON heartbeat payload**. Old heartbeats are
    raw RFC3339 timestamp bytes; new readers use
    `DecodeHeartbeat` (§4.1) to accept both formats. Old readers
    cannot consume v1 JSON heartbeats, but only run the legacy
    timestamp-based liveness path and don't need to.

  Full bidirectional-tolerance analysis in "K8s rolling upgrade
  constraint" below.
- Audit-driven escalation is **off** when the full capability chain
  (`CapAckV1 | CapTwoPhaseHandoff | CapProcessingGate`) is missing on
  the behind worker or all available targets. Documented prominently;
  surfaced via metric `parti.audit.escalation_skipped{reason="..."}`
  so operators can see when the safety net is inactive.
- Refs-always design: every commit references content-addressable
  payload keys (sha256). GC retention defaults: last 10 commits +
  24h window. Commit blob stays small (kilobytes, not tens of
  kilobytes) regardless of fleet size.

---

## Pillar 3 — Publish-side: refs-always commit with content-addressable payloads

### 3.1 Why mutable per-worker writes are unsafe

`assignment_publisher.go:230-255` writes one `assignment.<W>` key at a
time as a mutable `kv.Put`. Two failure shapes follow:

1. **Sequential partial batch.** A crash between worker 3 and worker 4
   leaves workers 1–3 on Version V and 4–N on Version V-1. If V
   reassigns partition P from W4 to W1, then W1 owns P (V) and W4 still
   owns P (V-1) — double processing. The reverse direction strands P
   until V-1 cleanup arrives.
2. **Split-brain payload corruption.** Even with a CAS-fenced commit
   marker, two leaders racing can write the *same* `assignment.<W>` key
   with different payloads. The losing leader's per-worker write can
   land *after* the winning leader's commit, leaving KV in a state
   where the commit references a digest that does not match the
   per-worker key. New workers detect the mismatch and refuse to apply
   — but the correct committed payload is unrecoverable from KV.

The fix replaces the mutable per-worker key with an **immutable
content-addressable payload key** referenced by the commit. The
critical property: a committed payload is, by KV invariant, the bytes
the commit references.

### 3.2 The three-key model

Pillar 3 introduces three logical key classes in the assignment KV
bucket. All protocol keys use a reserved `_` prefix on the
sub-component so they cannot collide with worker IDs (a worker
named `commit` or `payload` would otherwise be ambiguous). Old
`assignment.<W>` keys remain as legacy aliases (§3.7) but are no
longer the new-worker steady-state source of truth — dual-read
fallback applies during rolling upgrade (§3.6).

```
assignment._commit                    # singleton; current commit; watched by workers
assignment._commit_log.<V>            # immutable per-version log; consumed by GC and debug
assignment._payload.<hex(sha256)>     # immutable content-addressable payload bytes

assignment.<W>                        # legacy mutable alias (rolling-upgrade compat path)
```

Roles:
- **`assignment._commit`** — the single atomic decision point.
  CAS-fenced. New workers watch this key. Contains payload refs,
  not payloads.
- **`assignment._commit_log.<V>`** — immutable record of which
  payloads commit V referenced. GC walks the last K records to
  determine the live payload set. Workers never read it.
- **`assignment._payload.<hex(sha256)>`** — content-addressable,
  written with `kv.Create`. Identical content → identical key →
  cross-commit reuse. Workers fetch their own payload on commit
  changes.

**Existing code that scans the assignment bucket
(`DiscoverHighestVersion`, `cleanupStaleAssignments`, etc.) MUST
filter keys starting with `assignment._` as protocol keys and not
attempt to interpret them as worker IDs.** Failing to do so would
break legacy bootstrap discovery and could erroneously delete
protocol keys during cleanup.

### 3.3 Type definitions and hash separation

```go
// AssignmentPayload contains ONLY stable per-worker content.
// Version / leader / source / lifecycle metadata go in the commit so
// that identical partition slices across commits hash to the same key
// (cross-commit reuse).
type AssignmentPayload struct {
    SchemaVersion uint8       `json:"schema_version"`     // 1 = current
    Partitions    []Partition `json:"partitions"`
}

type AssignmentPayloadRef struct {
    Key         string `json:"key"`           // "assignment._payload.<hex(sha256)>"
    PayloadHash string `json:"payload_hash"`  // authoritative; matches Key suffix
    SetDigest   uint64 `json:"set_digest"`    // xxh3 over sorted partition IDs (audit/metrics only)
    Revision    uint64 `json:"revision"`      // diagnostic; KV revision at Create/Get time
}

type AssignmentCommit struct {
    Version             int64                            `json:"version"`
    LeaderRevision      uint64                           `json:"leader_revision"`
    SourceRevision      uint64                           `json:"source_revision,omitempty"`
    SourceRevisionKnown bool                             `json:"source_revision_known,omitempty"`
    PublishedAt         time.Time                        `json:"published_at"`
    Workers             []string                         `json:"workers"`
    Payloads            map[string]AssignmentPayloadRef  `json:"payloads"`
    BatchDigest         uint64                           `json:"batch_digest"` // xxh3 over the full sorted batch partition IDs
    PrevCommitRev       uint64                           `json:"prev_commit_rev,omitempty"`
}

// CommitLog is an immutable per-version record for GC and debugging.
// Workers do not read this key.
type AssignmentCommitLog struct {
    Version        int64     `json:"version"`
    LeaderRevision uint64    `json:"leader_revision"`
    PublishedAt    time.Time `json:"published_at"`
    PayloadKeys    []string  `json:"payload_keys"`  // sorted; what GC must retain
}
```

**Hash separation (from F1 architect correction).**
- `PayloadHash` (sha256, 64 hex chars) is **authoritative** for content
  identity. It is the key suffix and the verification target on read.
- `SetDigest` (xxh3 of sorted partition `CanonicalID()`s) is for
  audit/metrics comparisons only — fast equality check that doesn't
  require fetching the payload. Never used for content identity.
- `BatchDigest` (xxh3 of all sorted partition `CanonicalID()`s across
  the batch) is the publish-time coverage proof, compared against
  the source's partition set digest. Direct equality only; never
  unioned.

Hashes are over **canonical** representations:
- `PayloadHash`: sha256 over the canonical-JSON serialization of
  `AssignmentPayload` (each Partition's Keys preserved in source
  order, Partitions sorted by `CanonicalID()`, no whitespace).
  Compression is applied to the *stored* bytes but the hash is over
  the canonical uncompressed form.
- `SetDigest` / `BatchDigest`: xxh3 over the
  `CanonicalID()`-joined-with-`\n` partition list, sorted
  lexicographically by `CanonicalID()`.

**Collision-safe partition identity (closes review P1 #6).** The
existing `Partition.ID()` joins keys with `-`, but `-` is allowed
in keys, so distinct tuples can collide on identity (e.g.
`["a-b", "c"]` and `["a", "b-c"]` both produce `"a-b-c"`). For
coverage, dedupe, and digest logic — anywhere correctness depends
on tuple identity — use a new collision-safe encoding instead:

```go
// CanonicalID returns a length-prefixed, collision-safe encoding
// of the partition's key tuple, suitable for set-equality and
// digest logic. The encoding is fully length-driven, so any
// character (including '/', '-', ':') may appear in keys without
// ambiguity.
//
// Format: per key, "<len>:<key bytes>", joined by '/'. Parser
// reads the integer prefix to know exactly how many bytes belong
// to each key, so separator characters inside keys are never
// confused with the joiner.
//
// Example:
//   ["a-b", "c"]  → "3:a-b/1:c"
//   ["a",  "b-c"] → "1:a/3:b-c"
// (no collision)
func (p Partition) CanonicalID() string
```

`Partition.ID()` remains the human-readable form for durable
consumer names, logs, and any non-correctness display. Anywhere
the plan says "partition ID" in a digest/coverage/dedupe context,
read it as `CanonicalID()`.

**No expansion of `Partition.Validate` required (review P2 #6
correction).** A length-prefixed encoding parses unambiguously
regardless of what characters appear in keys. Existing validation
rules (forbid `.` and whitespace) are sufficient for correctness —
adding `/` to the forbidden set would needlessly break any user
who already has `/` in their keys.

### 3.5 Publish flow

Single path, no mode branching:

```
1. Source snapshot (with optional-interface fallback):
     if rs, ok := source.(types.RevisionedPartitionSource); ok:
         partitions, srcRev, srcKnown, err = rs.Snapshot(ctx)
     else:
         partitions, err = source.List(ctx)
         srcRev, srcKnown = 0, false
     # Calculator must NEVER call Snapshot directly on
     # types.PartitionSource — it's not on the base interface.
2. Compute assignments[W] = strategy.Assign(workers, partitions).
3. Verify publish-time invariant (set equality — see §3.8):
     sorted(union of assignments[W].Partitions IDs) == sorted(partition IDs)
   On mismatch: abort batch; surface metric; do not publish.
4. For each worker W:
     payload_W     = AssignmentPayload{SchemaVersion: 1, Partitions: assignments[W]}
     bytes_W       = canonicalMarshal(payload_W)
     payloadHash_W = hex(sha256(bytes_W))
     key_W         = "assignment._payload." + payloadHash_W
     setDigest_W   = xxh3(sorted partition IDs in assignments[W])

     rev_W, err = kv.Create(ctx, key_W, gzip(bytes_W))
     switch err:
       nil:
         metric: parti.assignment.payloads_created += 1
       jetstream.ErrKeyExists:
         existing, gerr = kv.Get(ctx, key_W)
         if gerr != nil: abort batch
         existingHash = hex(sha256(decompress(existing.Value())))
         if existingHash != payloadHash_W: return ErrPayloadHashCollisionOrCorruption
         rev_W = existing.Revision()
         metric: parti.assignment.payloads_reused += 1
       default:
         abort batch
     refs[W] = AssignmentPayloadRef{
         Key: key_W, PayloadHash: payloadHash_W,
         SetDigest: setDigest_W, Revision: rev_W,
     }
     metric: parti.assignment.payload_bytes_written += len(gzip(bytes_W))   // on create only
5. Pre-alias leadership fence (closes review P1 #5):
   read electionKV.leader; assert revision == claimed LeaderRevision R.
   On mismatch: abort batch before writing any aliases. A stale
   leader would otherwise dump legacy aliases into KV for a batch
   it will never get to commit.
6. Heartbeat-aware legacy alias barrier (closes review P0 #2):
   Read all active worker heartbeats (via DecodeHeartbeat which
   handles both v1 JSON and legacy timestamp string formats — §4.1).
   Classify:
     legacy_in_batch = { W in workers :
                         hb[W].Capabilities & CapAckV1 == 0 }
   (A worker whose heartbeat parsed as a legacy timestamp string
    also lands in legacy_in_batch — its Capabilities is 0 by the
    decoder rule.)
   For each W in legacy_in_batch:
     # The legacy alias is the ONLY signal old workers can read.
     # Writing it before commit ensures old workers actually
     # receive their slice of V; failing to do so silently
     # orphans the partitions assigned to them.
     for attempt in 0..2:
       err = kv.Put(ctx, "assignment." + W, legacyEnvelope(payload_W, V, R, srcRev))
       if err == nil: break
       backoff_with_jitter()
     if err != nil:
       metric: parti.publisher.alias_barrier_failed += 1
       abort batch; surface error to audit (will trigger republish)
7. Post-alias leadership fence (closes review P1 #5; restored from
   prior renumbering loss per full-pass review P1 #2):
   read electionKV.leader again; assert revision == claimed
   LeaderRevision R.
   On mismatch: abort BEFORE building/writing assignment._commit.
   The aliases already written at step 6 are documented
   mixed-version exposure — do not attempt commit CAS after
   observing leadership loss, because doing so could either (a)
   succeed and create a stale-leader-committed batch, or (b) fail
   the CAS but waste KV operations and confuse metrics.
8. Build AssignmentCommit{Version:V, LeaderRevision:R, SourceRevision:srcRev,
        SourceRevisionKnown:srcKnown, Workers:sortedWorkers, Payloads:refs,
        BatchDigest:xxh3(sorted batch CanonicalIDs), PrevCommitRev:lastCommitRev}.
9. CAS-write assignment._commit:
     kv.Update(ctx, "assignment._commit", commitBytes, lastCommitRev)
     (or kv.Create if no prior commit)
   On CAS failure: abort; another leader committed; surrender.
   On success: THIS IS THE COMMIT POINT.
   metric: parti.assignment.commit_bytes_written += len(commitBytes)
10. Best-effort write commit log:
     kv.Create(ctx, "assignment._commit_log." + V, commitLogBytes)
   Failures non-fatal — GC becomes more conservative if this is missing.
11. Best-effort legacy alias for COMMIT-CAPABLE workers (compat noise only):
     for each W in workers, W NOT in legacy_in_batch:
       kv.Put(ctx, "assignment." + W, legacyEnvelope(payload_W, V, R, srcRev))
   Failures non-fatal — these workers read the commit anyway; aliases
   are written only to keep the KV state consistent if the cluster
   later rolls back to a pre-fix version.
12. Best-effort GC (§3.9).
```

**Why this is safe even under split-brain.**
- Step 4 with `kv.Create` plus hash-verification on `ErrKeyExists`
  is the immutability guarantee. A losing leader's `Create` either
  succeeds with bytes that hash to the same key (byte-identical to
  the winning leader's payload by sha256 collision resistance), or
  its commit CAS at step 9 will lose to the winning leader's
  commit, leaving the losing leader's payload as inert garbage.
  **No payload write can ever contradict a committed ref.**
- Step 9 is the atomic decision. Either it wins the CAS and the
  commit is the cluster's view, or it fails and the leader's prior
  payload writes are unreferenced.
- Steps 10–12 are post-commit hygiene; failures do not affect
  correctness.

**Why the legacy alias barrier at step 6 is mandatory during
migration.** An old worker (one without `CapAckV1`) cannot read
`assignment._commit`. The only path by which it receives its V
assignment is the legacy alias key. If that alias write fails and
the leader proceeds to commit anyway, the cluster declares the
batch successful while the old worker is still running V-1 — and
the audit classifies the old worker as `unverifiable` and trusts
it. The partitions assigned to that old worker in V are silently
orphaned. Pre-commit barrier with bounded retry + abort prevents
this; the cluster surfaces the failure loudly rather than rotting
quietly.

**Documented mixed-version exposure: alias-published-but-commit-failed
(closes review P1 #5).** The publish flow writes legacy aliases at
step 6 and the commit CAS at step 9. Between those steps, a stale
leader could (in principle) lose the CAS race even though its
alias writes succeeded — leaving old workers with a V they applied
locally while the cluster's committed view stays at V-1. The
pre-alias and post-alias leadership rechecks (steps 5 and 7)
shrink this window to milliseconds but do not close it entirely:
NATS election state can drift between a read and a CAS, and old
workers have no way to participate in the commit protocol that
would let them gate on the cluster's actual commit point.

This is the unavoidable mixed-version floor:
- During migration: an old worker may briefly observe and apply a
  batch the cluster did not commit. This is **no worse than the
  pre-fix protocol** (which had the same exposure for all
  workers), and it disappears once the unverifiable-worker set
  empties.
- Once all workers are `CapAckV1`-capable: the alias barrier is
  not exercised (no `legacy_in_batch`); only steps 8-9 run; full
  CAS-fenced commit semantics apply uniformly.

**Why this doesn't permanently desynchronize the cluster.** Even
though an old worker may have applied the uncommitted V locally,
the cluster's authoritative view remains the latest CAS-succeeded
commit (V-1 or earlier in this scenario). The old worker
**cannot report `AppliedVersion=V` via heartbeat** because its
heartbeat is a raw timestamp string with no ack fields — it
remains `unverifiable` to the audit. On the next successful
publish, the new leader recomputes assignments against a fresh
source snapshot, writes a new alias (with whatever the new V's
slice for the old worker is), and the old worker overwrites its
local state. The locally-applied-but-uncommitted V was a
transient ghost; the next committed batch overwrites it without
needing any audit signal. The pre-fix protocol had the same
exposure for all workers; the new protocol only carries it for
the unverifiable subset and only until they upgrade.

### 3.6 Commit-driven worker state machine with rolling-upgrade fallback (closes F2, review P0 #1)

New workers watch **both** `assignment._commit` and their own
legacy alias `assignment.<W>`. In steady state the commit is
authoritative; during a rolling upgrade where an old leader is
still active and no new commit exists yet, the legacy alias is
the only thing being written, and new workers must apply it
through a compatibility path. Otherwise partitions assigned to
new workers by the old leader are silently orphaned.

**Source-of-truth selection rule** (evaluated whenever either
key changes):

```
let commit       = read(assignment._commit)              # may be nil
let legacyEntry  = read(assignment.<W>)                  # may be nil
let lastSeen     = lastSeenLeaderRevision

case 1: commit != nil and (legacyEntry == nil OR
                           commit.LeaderRevision >= legacyEntry.LeaderRevision):
    → follow commit path (state machine below)

case 2: legacyEntry != nil and
        legacyEntry.SchemaVersion == 0 and
        legacyEntry.LeaderRevision >= lastSeen and
        (commit == nil OR legacyEntry.LeaderRevision > commit.LeaderRevision):
    → follow legacy-compat path (apply via receipt path, gated on
      stale-leader fence; same semantics as today's old worker)

case 3: otherwise:
    → wait; no usable authority observable yet
```

Combinations under rolling upgrade:
- **Steady state (all new)**: only the commit path fires; legacy
  aliases are best-effort compat noise.
- **New leader + new workers + lingering old workers**: same.
- **Old leader + new workers**: no commit exists, legacy entries
  arrive from the old leader; new workers take the legacy-compat
  path. Same observable behaviour as today's old worker.
- **New-leader → old-leader handoff (the previously-fragile case)**:
  commit V exists from the prior new leader at `LeaderRevision=R1`;
  new old leader writes legacy aliases at `LeaderRevision=R2 > R1`;
  new workers see `legacyEntry.LeaderRevision > commit.LeaderRevision`
  → legacy-compat path → apply. Bounded divergence resolves cleanly
  when the next new leader takes over and writes V+1 with `R3 > R2`.

The audit on a new leader doesn't observe the legacy-compat path
directly, but the worker's heartbeat ack still records
`AppliedVersion = legacyEntry.Version`,
`AppliedDigest = digest(legacyPartitions)`, etc., so the next new
leader's commit V (at `LeaderRevision >= R2`) lets the audit
verify catch-up at that point. The legacy-compat path is not a
hole in the observability story; it just defers strict
enforcement until a new leader writes a commit.

**Commit-path state machine** (case 1 above). New workers watch
`assignment._commit`. State transitions:

```
On commit.V update or initial fetch:

(a) commit.Version <= currentAppliedVersion:
       no-op (already applied or stale)

(b) commit.LeaderRevision < lastSeenLeaderRevision:
       reject (stale leader fence)
       emit parti.worker.stale_leader_rejected

(c) W in commit.Workers:
       ref = commit.Payloads[W]
       if ref == nil:
           classify "malformed commit"
           emit parti.worker.commit_payload_missing
           do not apply; audit will detect and re-publish
       else:
           bytes, err = kv.Get(ref.Key)
           if err: emit parti.worker.payload_fetch_error; abort transition
           plain = decompress(bytes)
           if hex(sha256(plain)) != ref.PayloadHash:
               emit parti.worker.payload_hash_mismatch
               do not apply
           payload = decodeJSON(plain)
           if xxh3(sorted partition IDs in payload) != ref.SetDigest:
               emit parti.worker.set_digest_mismatch
               do not apply
           applyAssignment(payload, commit.Version, commit.LeaderRevision,
                           commit.SourceRevision, commit.SourceRevisionKnown)

(d) W NOT in commit.Workers:
       # Worker has been removed from the active set, OR was never in
       # this batch. Synthesize an empty assignment at commit.Version
       # and apply through the same receipt path so the leader's audit
       # observes the empty digest.
       applyAssignment(emptyPayload, commit.Version, commit.LeaderRevision,
                       commit.SourceRevision, commit.SourceRevisionKnown)

(e) commit.V arrives while applyAssignment for an earlier V is in flight:
       coalesce — only the highest pending V is acted on when the
       in-flight Apply completes. Worker maintains a single
       "pendingTargetVersion" and re-runs the gate when Apply returns.

lastSeenLeaderRevision = max(lastSeenLeaderRevision, commit.LeaderRevision)
```

**Mapping to F2 transitions:**
- "`assignment.<W>` arrives before `commit.V`" — handled by the
  dual-read source-of-truth rule. If no commit exists, or the
  alias has a fresher `LeaderRevision`, the alias is applied via
  the legacy-compat path. If a fresher commit exists, the alias
  is ignored (the commit's payload ref is authoritative). New
  workers do NOT ignore aliases entirely (closes review
  full-pass P1 #3).
- "`commit.V` lists worker but has no payload ref for it" —
  case (c) with `ref == nil`. Classified as malformed; do not
  apply.
- "Worker not in `commit.Workers`" — case (d). Synthesize empty,
  apply, publish ack with empty-list `AppliedDigest`. This is
  the authoritative "you're revoked" signal under the
  commit-driven model, replacing the current ignored-delete
  behaviour at `manager_assignment.go:336-340`.
- Assignment key deletion — ignored for new workers (the commit
  is authoritative when one exists; under the legacy-compat
  fallback path the deletion is observed the same way old
  workers observe it today).

### 3.7 Legacy alias and new-leader recovery

#### Legacy alias during rolling upgrade

While old-version worker pods remain in the fleet, the publisher
writes the mutable `assignment.<W>` key on every commit. For
**legacy (`CapAckV1=0`) workers in the batch**, this write is the
mandatory pre-commit alias barrier (step 6 of §3.5) — failures abort
the batch. For **commit-capable workers**, the alias write is
best-effort (step 11 of §3.5) — these workers read the commit
anyway, so the alias is compatibility noise to support possible
rollback to a pre-fix version. The payload mirrors what's in
`commit.Payloads[W]` plus the legacy envelope fields (`Version`,
`LeaderRevision`, `SourceRevision`). Old workers' apply logic is
unchanged — they read this key, version-gate on `Version`, apply.

**New workers do not ignore the alias.** Per §3.6, new workers
watch *both* `assignment._commit` and their own `assignment.<W>`
and apply the source-of-truth selection rule to pick which path to
follow. In steady state the commit wins; during rolling upgrade
against an old leader (where no commit exists, or a stale commit
exists at a lower `LeaderRevision` than the current legacy alias),
the alias wins. This is what closes the rolling-upgrade orphan
path identified in review P0 #1.

#### New-leader recovery

On takeover (`Calculator.Start`):

```go
commit := read("assignment._commit")
if commit == nil {
    // No prior commit exists. Two sub-cases:
    //   (a) genuinely first-ever leader on a fresh bucket
    //   (b) rolling upgrade against a previously-running OLD-leader
    //       cluster with existing legacy assignment.<W> keys at some
    //       version N
    workerIDs, highestV := publisher.DiscoverHighestVersion(ctx)
    publisher.currentVersion = highestV   // 0 if case (a)
    // Next publish writes Version=highestV+1 and the first commit.
    // Existing legacy assignment.<W> keys remain applicable to old
    // workers under their existing apply rules. **New workers
    // continue the dual-read fallback** (§3.6) until the first
    // new commit lands — they do NOT blindly wait if a valid legacy
    // alias is fresher than any prior commit. (closes review
    // full-pass P1 #3.)
} else {
    publisher.currentVersion = commit.Version
    // Any orphan payload keys (created by prior leaders but never
    // referenced by a committed commit) are inert garbage — GC will
    // reap them under retention. Workers cannot observe them.
}
```

`DiscoverHighestVersion` is preserved purely as a one-shot
compatibility bootstrap, only used until the first commit lands.

### 3.8 Publish-time set-equality check

Replace the `assignedCount == len(partitions)` check at
`calculator.go:921-929` with strict **set equality** (step 3 of §3.5):

```go
expected := sortedPartitionIDs(snapshot.partitions)
got      := sortedPartitionIDs(union(assignments[W].Partitions for W in workers))
if !equal(expected, got):
    metric: parti.publisher.batch_aborted{reason="coverage_mismatch"}
    // refuse to publish — strategy is buggy
    return ErrCoverageMismatch
```

The count check accepts duplicates as long as totals match. Set
equality catches duplicates as well as missing partitions and is the
publish-side half of the end-to-end invariant. Audit at runtime
verifies the apply-side half (§4.2).

### 3.9 GC for payload keys

GC is conservative and never participates in correctness:

```
periodic (every N commits or every T minutes):
    live = {}
    for i in 0..K-1:
        log = read("assignment._commit_log." + (currentVersion - i))
        if log != nil: live |= set(log.PayloadKeys)
    # Defensive: also include current commit's refs:
    commit = read("assignment._commit")
    if commit != nil: live |= set(ref.Key for ref in commit.Payloads.values())

    candidates = list keys under "assignment._payload."
    for key in candidates:
        if key not in live and key.age > retention_window:
            kv.Delete(key)   # best-effort; failures non-fatal
```

Retention defaults:
- Last `K = 10` commits' payloads retained even if otherwise
  unreferenced (forensic window).
- Time window: 24h.
- GC pass cadence: every 5 minutes.

All configurable. GC failures emit
`parti.gc.payload_delete_errors` but never block publish.

### 3.10 Frequency benefit is strategy-dependent

The refs-always bandwidth advantage assumes that an incremental
source change perturbs few worker slices — i.e. `N_changed <<
len(workers)`. This holds for consistent-hash strategies (one
partition addition moves at most one worker's slice). It does **not**
hold for strategies that reshuffle widely on small input changes
(e.g. round-robin over a sorted partition list, where inserting a
partition near the front can shift the alignment of many workers'
slices).

For wide-reshuffle strategies, refs-always is still safe and the
commit is still small, but the per-rebalance write count and
bandwidth approach the "rewrite-every-worker" worst case
(`N_changed ≈ len(workers)` instead of `N_changed ≈ 1`). The new
metrics make this observable in production:

```
parti.assignment.payloads_reused        # cross-commit reuse counter (Counter)
parti.assignment.payloads_created       # new payload writes (Counter)
parti.assignment.payload_bytes_written  # bytes written to payload keys per commit (Histogram)
parti.assignment.commit_bytes_written   # commit blob size per commit (Histogram)
```

At the user's scale (20–30 workers, ~2000 partitions) with
consistent-hash strategy, `payloads_reused / payloads_created` should
trend toward ~30:1 in steady state, with `payload_bytes_written`
near zero per typical incremental rebalance and `commit_bytes_written`
< 5 KB.

---

## Pillar 4 — Apply-side: receipts, audit, reconcile

### 4.1 Heartbeat payload extension (the ack channel)

Today the heartbeat writer (in `internal/heartbeat/publisher.go`) writes
only a timestamp; the leader-side `WorkerMonitor` reads the keys for
liveness. The receipt mechanism is **worker-side write, leader-side
read**. Make this explicit by putting the new state-mutation API on the
heartbeat publisher, not on `WorkerMonitor`.

#### New payload type

```go
// types/heartbeat.go (new file)
type Heartbeat struct {
    WorkerID                 string    `json:"worker_id"`
    SchemaVersion            uint8     `json:"schema_version,omitempty"`   // 0=legacy, >=1=ack-capable
    Capabilities             uint32    `json:"capabilities,omitempty"`     // bitmask; see CapXxx constants
    LeaderRevision           uint64    `json:"leader_revision,omitempty"`  // last leader term this worker accepted
    AppliedVersion           int64     `json:"applied_version,omitempty"`  // last assignment.Version successfully applied
    AppliedDigest            uint64    `json:"applied_digest,omitempty"`   // xxh3 of sorted partition IDs the worker is running
    AppliedSourceRevision    uint64    `json:"applied_source_revision,omitempty"`
    AppliedSourceRevKnown    bool      `json:"applied_source_revision_known,omitempty"`
    AppliedAt                time.Time `json:"applied_at,omitempty"`
    Timestamp                time.Time `json:"timestamp"`                  // existing liveness field
}

// Capability bitmask values. A worker sets a bit iff the corresponding
// safety mechanism is actually wired up and active in this process,
// not merely configured. The leader's audit uses this to decide
// whether reassignment escalation is safe (closes F3).
const (
    CapAckV1            uint32 = 1 << 0  // publishes apply receipts (AppliedVersion etc.)
    CapTwoPhaseHandoff  uint32 = 1 << 1  // manager runs the two-phase handoff coordinator
    CapProcessingGate   uint32 = 1 << 2  // consumer handlers are wrapped with the processing gate
)
```

**Wire format and dual decoder (review P0 — heartbeat format).**
Current code writes raw RFC3339 timestamp bytes, not JSON:

```go
// existing internal/heartbeat/publisher.go behaviour:
value := []byte(time.Now().Format(time.RFC3339Nano))
_, err := p.kv.Put(ctx, key, value)
```

New code writes JSON-encoded `Heartbeat` objects. The leader-side
reader must accept **both** formats so legacy pods in a mixed-version
fleet remain observable. The dual decoder rule:

```go
func DecodeHeartbeat(b []byte) (Heartbeat, error) {
    // Try v1 JSON first — new payloads always start with '{'.
    if len(b) > 0 && b[0] == '{' {
        var hb Heartbeat
        if err := json.Unmarshal(b, &hb); err != nil {
            return Heartbeat{}, fmt.Errorf("v1 JSON parse: %w", err)
        }
        // Defensive: a malformed v1 payload must NOT silently degrade
        // to "legacy worker" — surface the parse error so the audit
        // treats this as a read failure, not a CapAckV1=0 classification.
        return hb, nil
    }
    // Fallback: legacy timestamp string (RFC3339 / RFC3339Nano).
    ts, err := time.Parse(time.RFC3339Nano, string(b))
    if err != nil {
        ts, err = time.Parse(time.RFC3339, string(b))
    }
    if err != nil {
        return Heartbeat{}, fmt.Errorf("legacy timestamp parse: %w", err)
    }
    return Heartbeat{
        SchemaVersion: 0,
        Capabilities:  0,
        Timestamp:     ts,
        // all other fields zero — classifies as `unverifiable` in audit
    }, nil
}
```

Key behaviours:
- **Legacy timestamp byte payload → `{SchemaVersion=0, Capabilities=0, Timestamp=parsed}`**.
  This is the correct classification for old workers: alive but not
  ack-capable; the audit and the alias barrier both treat them as
  legacy.
- **v1 JSON payload → full `Heartbeat`**, with all new fields
  populated per the writer's runtime capability state.
- **Malformed payload (neither valid JSON nor parseable timestamp) →
  read error**, surfaced to the caller. Do NOT silently treat as
  legacy: a malformed heartbeat is a different signal (key corrupted
  or wrong sender), and silently degrading it to `CapAckV1=0` would
  mask the problem.

`WorkerMonitor.GetHeartbeats` walks all heartbeat keys, decodes each
via `DecodeHeartbeat`, and returns a map with parse errors logged at
debug level. Workers that fail to decode are omitted from the
returned map (effectively treated as "heartbeat missing" by the
audit, which is the right signal — the writer is broken, not just
legacy).

`Capabilities` reflects **runtime wire-up state**, not config
intent — see "Capability reporting API" below.

`AppliedSourceRevKnown` mirrors `commit.SourceRevisionKnown`: when
the assignment came from a `RevisionedPartitionSource` that
returned `known=true`, the worker records `true` here; otherwise
`false`.

**Audit gating on `AppliedSourceRevKnown` (closes review
full-pass P1 #5):** the audit's source-revision check is skipped
**only when `commit.SourceRevisionKnown=false`** (e.g., the commit
was published from a Static source). When the commit declares
`SourceRevisionKnown=true`, an ack-capable worker MUST report
`AppliedSourceRevKnown=true` AND a matching
`AppliedSourceRevision`; a worker that reports
`AppliedSourceRevKnown=false` for a known-revision commit is
classified `behind`, not skipped. This prevents a buggy or
adversarial worker from trivially passing audit by omitting the
known bit.

#### Capability reporting API (closes review P1 #5)

The manager has no inherent way to know whether the consumer
handler is actually wrapped with the processing gate — that's a
property of how the consumer was wired up, not of manager config.
Adding a concrete reporting API so the consumer/updater layer can
flip the bit at wire-up time:

```go
// SetCapability flips a capability bit on the manager's heartbeat
// publisher. Called by the component that actually wires the
// corresponding safety mechanism — not by config-reading code.
// Examples:
//   - The two-phase handoff coordinator calls
//     m.SetCapability(CapTwoPhaseHandoff, true) after successfully
//     starting (and (..., false) on Stop).
//   - The consumer/updater calls
//     m.SetCapability(CapProcessingGate, true) when it wraps
//     handlers with the processing gate, and (..., false) if
//     gate initialization fails or the consumer falls back to
//     an ungated path.
//   - The heartbeat publisher itself sets CapAckV1 at startup
//     since ack-publishing capability is intrinsic to the new
//     publisher.
func (m *Manager) SetCapability(cap uint32, active bool)
```

The heartbeat publisher reads the current capability bitmask via
`m.Capabilities()` (atomic load) when composing each heartbeat.

**Key requirement: the bit reflects runtime reality.** If
`EnableTwoPhaseHandoff = true` in config but the coordinator
fails to start, `CapTwoPhaseHandoff` stays false. If the
processing gate is configured but the consumer falls back to a
non-gated handler, `CapProcessingGate` stays false. The audit
trusts this signal; misconfiguration drift cannot fool it.

#### Writer-side API (worker-side, `internal/heartbeat/publisher.go`)

The publisher maintains an in-memory `applied` snapshot, updated only
after Apply succeeds (see §4.4). The periodic tick republishes that
snapshot atomically with a fresh `Timestamp`, never clobbering applied
fields.

```go
// Concrete additions to the heartbeat publisher.
type Publisher struct {
    // ...existing fields...
    mu      sync.RWMutex
    applied appliedSnapshot     // struct mirroring the ack fields
}

type appliedSnapshot struct {
    SchemaVersion         uint8
    LeaderRevision        uint64
    AppliedVersion        int64
    AppliedDigest         uint64
    AppliedSourceRevision uint64
    AppliedSourceRevKnown bool      // mirrors commit.SourceRevisionKnown
    AppliedAt             time.Time
}

// SetAppliedAssignment records a successful Apply. Called from the
// manager AFTER handoffCoordinator.Apply returns success (§4.4).
// Thread-safe; idempotent; monotone in AppliedVersion.
func (p *Publisher) SetAppliedAssignment(snap appliedSnapshot) {
    p.mu.Lock()
    defer p.mu.Unlock()
    if snap.AppliedVersion < p.applied.AppliedVersion {
        return // monotone — don't regress
    }
    p.applied = snap
    p.applied.SchemaVersion = 1
}

// PublishNow forces an out-of-band heartbeat publish, used by the
// manager immediately after SetAppliedAssignment so the leader's audit
// observes the new applied state within one heartbeat round-trip
// instead of waiting up to HeartbeatInterval.
func (p *Publisher) PublishNow(ctx context.Context) error { ... }

// (existing) The periodic ticker calls p.build() which composes
// {Timestamp: now, ...p.applied (snapshot copy)} under the read lock.
```

#### Reader-side (leader-side, `internal/assignment/worker_monitor.go`)

`WorkerMonitor` decodes the extended payload but doesn't mutate it.
Add:

```go
// GetHeartbeats returns the decoded heartbeats for all active workers.
// Fields beyond Timestamp are zero for legacy (SchemaVersion=0) writers.
func (m *WorkerMonitor) GetHeartbeats(ctx context.Context) (map[string]Heartbeat, error)
```

The audit (§4.2) consumes this map.

Wire-size cost: ~48 bytes added per heartbeat. Negligible.

### 4.2 Leader audit loop

Add to the calculator a periodic auditor independent of the
worker-change-driven rebalance. **All comparisons are against the
current commit**, not the current source snapshot — source advancing
past commit V is a signal to publish V+1, not a signal that workers are
behind on V.

```go
func (c *Calculator) auditApplied(ctx context.Context) {
    commit       := c.publisher.LastCommit()
    if commit == nil { return }                  // pre-commit bootstrap, nothing to audit yet

    workers, hbs := c.workerMonitor.GetHeartbeats(ctx)

    var (
        fullyApplied = map[string]bool{}
        behind       = map[string]bool{}
        unverifiable = map[string]bool{}
    )

    const requiredCaps = CapAckV1 | CapTwoPhaseHandoff | CapProcessingGate

    for _, w := range workers {
        hb := hbs[w]
        if hb.SchemaVersion == 0 || hb.Capabilities & CapAckV1 == 0 {
            // Legacy or non-ack-capable worker — cannot prove apply.
            // Trust the alive signal, count as unverifiable. Does NOT
            // participate in audit-driven escalation.
            unverifiable[w] = true
            continue
        }
        // Source-revision check (review P1 #4 correction): if the
        // commit declares a known source revision, the ack-capable
        // worker MUST also know its source revision and it MUST
        // match. A worker that omits AppliedSourceRevKnown for a
        // known-revision commit is classified behind — otherwise a
        // broken/buggy worker could trivially pass audit by simply
        // not reporting.
        srcRevMatch := !commit.SourceRevisionKnown ||
                       (hb.AppliedSourceRevKnown &&
                        hb.AppliedSourceRevision == commit.SourceRevision)
        ref, hasRef := commit.Payloads[w]

        switch {
        case !hasRef && contains(commit.Workers, w):
            // Worker is in the batch but commit has no payload ref —
            // malformed commit. Treat as behind (worker can't apply).
            behind[w] = true
        case hb.LeaderRevision   != commit.LeaderRevision,
             hb.AppliedVersion   != commit.Version,
             !srcRevMatch,
             hasRef && hb.AppliedDigest != ref.SetDigest:
            behind[w] = true
        default:
            fullyApplied[w] = true
        }
    }

    // Update gauges (observable).
    c.metrics.RecordAuditCounts(len(fullyApplied), len(behind), len(unverifiable))

    // Apply-grace gate: only escalate after the worker has had time to
    // apply + publish a fresh heartbeat. Default: 2 × HeartbeatTTL.
    if since(commit.PublishedAt) < c.ApplyGracePeriod {
        return
    }

    // ─── Behind workers ───
    // Step 1 (always): retry pressure. The manager-side apply-retry
    // loop (§4.4) is already running on the worker; the audit's job
    // here is to surface the metric, not to act yet.
    for w := range behind {
        c.metrics.RecordWorkerBehind(w, commit.Version)
    }

    // Step 2 (extended grace, e.g. 5 × HeartbeatTTL): escalation
    // gated on FULL safety chain (closes F3).
    if since(commit.PublishedAt) < c.ExtendedApplyGracePeriod { return }

    // The behind set must be empty of any worker whose capabilities
    // don't include the full safety chain. Targets must also have the
    // full safety chain. If any required cap is missing on EITHER the
    // behind worker OR the available targets, skip escalation.
    behindReassignable := []string{}
    for w := range behind {
        if hbs[w].Capabilities & requiredCaps != requiredCaps {
            c.metrics.RecordAuditEscalationSkipped("cap_missing_behind", w)
            continue
        }
        behindReassignable = append(behindReassignable, w)
    }
    targets := []string{}
    for w := range fullyApplied {
        if hbs[w].Capabilities & requiredCaps == requiredCaps {
            targets = append(targets, w)
        }
    }
    if len(behindReassignable) == 0 || len(targets) == 0 {
        c.metrics.RecordAuditEscalationSkipped("cap_missing_targets")
        return
    }
    // Manager-side belt-and-braces: also refuse escalation if the
    // leader's own EnableTwoPhaseHandoff config is off. The capability
    // bits should already encode this, but defensive.
    if !c.cfg.EnableTwoPhaseHandoff {
        c.metrics.RecordAuditEscalationSkipped("direct_mode")
        return
    }

    // Safety provided by the ClaimsRegistry (claims.go:77-84): the
    // target worker CAS-claims via two-phase; the processing-gate
    // (wired in internal/durable/worker_consumer.go:382-387, gate
    // implementation in internal/durable/processing_gate.go) on the
    // OLD worker observes the ownership change and stops delivering
    // messages. This is NOT a graceful release by the old worker's
    // local coordinator (whose apply is, by definition, stuck) —
    // the gate is the serialization point.
    c.logger.Warn("audit: escalating behind workers via two-phase handoff",
        "behind_workers", behindReassignable)
    c.scheduleRebalance("audit_repair", reassignFrom: behindReassignable)

    // ─── Coverage proof ───
    // Cluster-wide coverage holds by *transitivity*, not by a runtime
    // union of digests (xxh3 is not homomorphic):
    //   (a) publish-time set-equality check at §3.8 proved
    //       union(assigned partition IDs) == source partition IDs.
    //   (b) per-worker audit above proved each W is running
    //       commit.Payloads[W].SetDigest.
    //   (c) commit marker is atomically committed (CAS-fenced).
    // Therefore the cluster covers source at commit.SourceRevision iff
    // every W in commit.Workers is fullyApplied or unverifiable.
}
```

**Grace windows:**
- `ApplyGracePeriod` (retry-pressure threshold): `2 × HeartbeatTTL`.
- `ExtendedApplyGracePeriod` (escalation threshold, two-phase only):
  `5 × HeartbeatTTL`.
- Both configurable.

**Where source revision drives action.** If `currentSourceRevision >
commit.SourceRevision`, the audit does NOT mark workers behind — it
schedules a new rebalance at the next opportunity. The source-revision
mismatch belongs in `monitorPartitions` (which already triggers a
rebalance on partition update) and in the publish-side path.

### 4.3 Worker commit watcher reconcile (closes A2 / finding #2)

Under the commit-driven model, new workers watch
`assignment._commit` (a singleton key) instead of `assignment.<W>`.
The watcher reliability concern from finding #2 still applies — the
existing code at `manager_assignment.go:300-334` exits silently when
its watcher channel closes (`!ok` → `return nil`). Apply the same
fix:

1. Two-value receive on `watcher.Updates()`.
2. On `!ok`, treat as transient: re-establish watcher with
   exponential backoff (same pattern as `monitorAssignmentChanges`
   retry loop — reuse).
3. Add a periodic reconcile (`commitReconcileInterval`, default 30s)
   that re-reads `assignment._commit` and routes
   through `handleAssignmentEntry` idempotently. Silent when in sync;
   triggers apply when divergent.

### 4.4 Apply-then-store-then-ack (closes A1 / finding #7)

Two code paths today store assignment **before** Apply has succeeded:

1. **Update-time path**: `manager_assignment.go:365-377`
   (`applyAssignmentUpdate` stores `newAssignment`) then
   `manager_assignment.go:385-389` (`applyHandoffAndHooks` calls
   `handoffCoordinator.Apply`); on Apply failure, logs and continues.
2. **Initial-assignment path**: `manager.go:374-386` waits for
   the assignment to land, emits events, calls
   `applyInitialHandoffAsync`, then transitions to `StateStable`
   while the Apply runs asynchronously.

Both must be unified through the same apply-then-store-then-ack
sequence:

```go
func (m *Manager) applyAssignment(newAssignment Assignment) {
    // 1. Apply first. On failure: mark degraded, schedule retry, return.
    //    Do NOT store. Do NOT publish ack.
    if err := m.handoffCoordinator.Apply(m.ctx, m.WorkerID(),
                                          m.CurrentAssignment(), newAssignment); err != nil {
        m.markDegraded("apply failed", err)
        m.scheduleApplyRetry(newAssignment)   // bounded exponential backoff
        return
    }

    // 2. Store the now-applied assignment as the worker's current state.
    m.assignment.Store(newAssignment)

    // 3. Publish ack via heartbeat. SetAppliedAssignment + PublishNow
    //    so the leader's audit observes within one round-trip, not after
    //    the next periodic tick (which could be up to HeartbeatInterval).
    m.heartbeatPublisher.SetAppliedAssignment(appliedSnapshot{
        SchemaVersion:         1,
        LeaderRevision:        newAssignment.LeaderRevision,
        AppliedVersion:        newAssignment.Version,
        AppliedDigest:         digest(newAssignment.Partitions),  // xxh3 over CanonicalIDs
        AppliedSourceRevision: newAssignment.SourceRevision,
        AppliedSourceRevKnown: newAssignment.SourceRevisionKnown, // review P1 #4: must flow through
        AppliedAt:             time.Now(),
    })
    if err := m.heartbeatPublisher.PublishNow(m.ctx); err != nil {
        m.logError("heartbeat publish-now after apply failed", err)
        // Non-fatal: next periodic tick will pick up the snapshot.
    }

    // 4. User-facing hooks last.
    m.invokeHook(OnAssignmentChanged, ...)
}
```

Both call sites — the update-time watcher and the initial-assignment
bootstrap — call `applyAssignment`. The state machine cannot transition
to `StateStable` from the initial-assignment path until
`applyAssignment` returns with the ack published; otherwise the worker
reports `AppliedVersion=0` while claiming to be stable, breaking the
invariant on first boot.

Failure mode change: instead of silently telling the world we're on V
while running V-1, we stay on V-1 (heartbeat reflects V-1), and the
leader's audit sees the drift. The behind-classification triggers
retry pressure; only after extended grace AND only when two-phase is
enabled does it escalate to reassignment (§4.2).

### 4.5 Leader fencing on `LeaderRevision` (closes F1 / finding #6)

The commit-driven worker state machine in §3.6 already enforces
stale-leader fencing on every `commit.V` arrival (case (b)). The same
fence applies during the legacy alias path that old workers continue
to use:

```go
// In handleAssignmentEntry (manager_assignment.go:336-353) — legacy
// path retained for old-version pods that haven't been replaced yet.
if newAssignment.LeaderRevision < m.lastSeenLeaderRevision.Load() {
    return  // assignment from a stale leader term
}
if newAssignment.Version <= oldAssignment.Version {
    return  // already at or past this version
}
m.lastSeenLeaderRevision.Store(newAssignment.LeaderRevision)
```

A split-brain higher-version write from a stale leader is now
rejected at both the commit-watcher path (new workers) and the legacy
per-worker watcher path (old workers).

### 4.6 Source validation + dedupe (closes S1 / finding #8)

In `nats_kv.go decodePartitions`:

```go
for _, p := range partitions {
    if err := p.Validate(); err != nil {
        return nil, fmt.Errorf("invalid partition %v: %w", p.Keys, err)
    }
}
// Dedupe by CanonicalID — review P1 #4 correction. Partition.ID()
// is collision-prone (joins with '-' which is allowed in keys), so
// it must not be used for correctness checks like dedupe. CanonicalID
// is length-prefixed and collision-safe.
seen := map[string]struct{}{}
for _, p := range partitions {
    id := p.CanonicalID()
    if _, dup := seen[id]; dup {
        return nil, fmt.Errorf("duplicate partition canonical ID: %s", id)
    }
    seen[id] = struct{}{}
}
// Deep-copy Keys on store.
```

Same path applied in `Update`/`Modify` before the CAS write — bad data
never lands in KV.

---

## Polling cost — corrected arithmetic and scope

Earlier draft said 1000 nodes × 1 poll / 30s ≈ 2 RPS. **Wrong** — it's
1000 / 30 ≈ **33 RPS**. Decision for the implementation:

- **Leader**: reconcile interval **30s** (snappy partition discovery,
  drives the audit loop).
- **Followers**: reconcile interval **5 min** (cold warm cache for
  potential leader takeover; the followers don't drive rebalance and
  their assignment-watcher reconcile already covers them).

At 1000 nodes: leader contributes ~0.033 RPS, followers ~3.3 RPS,
total ≈ 3.4 RPS to NATS. At 10000 nodes: ~33 RPS, still negligible.

Both intervals are configurable via `NatsKVOption` (leader-vs-follower
detection is via a callback the manager registers: `WithLeadershipProbe(func() bool)`).

---

## Backward compatibility & migration

The intent is: an app pinned to current parti can upgrade to the post-fix
version with no code changes and no operational coordination. This section
walks every interop surface and identifies the one residual caveat.

### Wire format
JSON-encoded `[]Partition`, optional gzip envelope, single KV key. **Unchanged.**
Old and new versions read each other's writes byte-for-byte. Reading a value
written by the other version is indistinguishable from reading a value
written by the same version.

### `Update()` signature
```go
func (s *NatsKV) Update(ctx context.Context, partitions []types.Partition) error
```
**Unchanged.** Callers recompile and re-link with no source edits.

### `NewNatsKV` signature
Becomes variadic:
```go
func NewNatsKV(kv jetstream.KeyValue, key string, logger types.Logger, opts ...NatsKVOption) *NatsKV
```
Existing 3-arg call sites compile unchanged (`opts...` is empty). New
defaults (30s reconcile) apply silently.

### Server-side wire interaction
Old `kv.Put` and new `kv.Update(rev)` are both standard JetStream KV ops
against the same key. The server does not require all clients to use the
same op type — they coexist.

### Mixed-version rolling upgrade (the only behaviour question)

During a rolling deploy, the fleet temporarily contains both old workers
(using `kv.Put`) and new workers (using `kv.Update` with CAS). The matrix:

| New writer attempts | Concurrent old writer issues `kv.Put` | Outcome |
|---|---|---|
| `kv.Update(rev=R)` | nothing | success, revision R+1 |
| `kv.Update(rev=R)` | `kv.Put` happens first (revision now R+1) | CAS conflict → new writer refreshes revision → retries → succeeds at R+2 |
| `kv.Update(rev=R)` | `kv.Put` happens after | both succeed; revision becomes R+2 |
| `kv.Put` (old worker) | anything | always succeeds (no CAS) |

Concretely:
- **No data corruption** in either direction. Every successful call lands
  the caller's exact payload.
- **New workers are protected from each other** even mid-migration — their
  CAS interlock works as designed.
- **New workers are not protected from old workers' clobbers**, because
  old workers don't participate in CAS. This is unavoidable: you can't
  unilaterally upgrade a protocol from one side.
- **Old workers are unchanged** — they keep the pre-fix race exposure they
  had before. The bug isn't worse during migration than it was on the
  prior release; it's simply not yet fixed for those workers.

Translation: the lost-update bug is fully fixed *once the last old worker
finishes rolling out*. During the rolling window, it is partially fixed
(new↔new is safe, new↔old is no worse than before). No flag day required.

### CAS retry budget vs. old-writer churn

If an old worker is hammering `kv.Put` at high frequency, a new worker's
CAS retry (default 5 attempts) might exhaust before landing. We mitigate:

- Use exponential backoff between attempts (a few ms → tens of ms) so the
  new writer is statistically more likely to find a quiet revision.
- Allow tuning via option:
  ```go
  func WithUpdateRetries(n int) NatsKVOption  // default 5
  ```
- On exhaustion, return a typed error (`ErrUpdateRetryExhausted`) so
  callers can surface or retry at their own cadence rather than wrapping
  an opaque NATS error.

In practice partition lists change at human timescales (deploys, scale
events), not at sustained kHz. Exhaustion in real workloads is implausible.

### Reconcile loop interaction with old fleet

The reconcile loop only **reads** KV. Old workers don't know it exists and
are unaffected. Old workers don't poll, so they keep their existing watcher
fragility — but again, no worse than today.

### `Modify()` introduction
Purely additive. No existing call site references it, so no code change is
forced. Documented as the recommended path for callers who previously did
`List(); mutate; Update(list)`.

### Operator-facing migration checklist

1. Upgrade parti dependency, recompile. No source changes required.
2. Deploy in your normal rolling fashion.
3. (Optional, recommended) After all instances are on the new version,
   change any in-app `List(); append; Update(list)` sequence to
   `Modify(ctx, fn)`. This is the change that fully eliminates the
   lost-update class for *intentional* concurrent writers.

### Downgrade path

A new-version writer that runs against an old-version fleet downgrades
cleanly — wire format identical, CAS becomes effectively a no-op latency
overhead. Pinning back to the old version is a redeploy; no KV cleanup
required.

### K8s rolling upgrade constraint

Parti users deploy via K8s rolling upgrade. We **cannot** choose to roll
workers before leaders — the binary is the same; whichever pod currently
holds the leader lease IS the leader, regardless of role. During a
rolling upgrade the cluster passes through every mix:

```
old leader + old workers
  → old leader + (some old, some new) workers
  → new leader (lease-holder pod got replaced) + (some old, some new) workers
  → new leader + all new workers
```

Mid-upgrade the leader role can also bounce back (an old pod drains
slowly and briefly reclaims the lease before exiting). We have no
operational lever to constrain this ordering.

The design constraint that falls out: **every new protocol element must
be tolerated by old code, AND every new code path must tolerate the
absence of new protocol elements written by old code.** No flag day, no
required sequencing, no "first do X then Y."

### Bidirectional-tolerance rules

KV evolution falls into two categories:
- **`assignment.*` keys**: all new fields on the v1 JSON payload
  are additive (old decoders ignore unknown fields, new decoders
  treat missing fields as zero-value).
- **`heartbeat.<W>` keys**: **dual-format**, not JSON-additive.
  Old code writes raw RFC3339 timestamp bytes; new code writes v1
  JSON. The new reader uses `DecodeHeartbeat` (§4.1) which
  accepts both formats and synthesizes a zero-field `Heartbeat`
  for the legacy timestamp form. **Old workers do not serialize
  a "JSON Heartbeat object without the new fields" — they
  serialize a timestamp byte string with no object structure at
  all.**

The behaviour rules below describe what each role uses to decide
what to do with what it reads.

#### Schema-version marker on heartbeat
Add `SchemaVersion` and `Capabilities` fields to the v1 JSON
heartbeat payload so both new-worker presence AND active
safety-mechanism status are self-describing:

```go
type Heartbeat struct {
    // existing fields ...
    SchemaVersion uint8   // 0 = pre-fix; ≥1 = ack-capable
    Capabilities  uint32  // bitmask: CapAckV1 | CapTwoPhaseHandoff | CapProcessingGate
    // new fields (only meaningful when SchemaVersion ≥ 1) ...
}
```

Old workers do not serialize this struct at all — they write raw
timestamp bytes via the legacy writer. The reader's
`DecodeHeartbeat` returns
`{SchemaVersion:0, Capabilities:0, Timestamp:parsed, all other
fields zero}` for legacy payloads, which is what classifies them
as `unverifiable` in the audit. The audit's reliable discriminator
between "old worker that doesn't speak ack" and "new worker that
should be acking but isn't" is therefore the decoder's branch
selection, not a JSON field count.

#### KV schema evolution table

| Key | New field | Writer behaviour | Reader behaviour |
|---|---|---|---|
| `assignment._commit` (NEW key, singleton) | full schema | New leader writes after all payload keys land; old leader never writes | New worker: source of truth (§3.6). Old worker: never reads — no effect. |
| `assignment._commit_log.<V>` (NEW key) | full schema | New leader best-effort write; old leader never writes | Workers never read; only GC. Old worker: never reads. |
| `assignment._payload.<hex(sha256)>` (NEW key) | immutable | New leader `kv.Create`; old leader never writes | New worker: fetched by ref from commit. Old worker: never reads. |
| `assignment.<W>` (legacy) | mutable per-worker | New leader: pre-commit barrier (mandatory) for legacy workers in batch, best-effort for commit-capable workers (§3.5 steps 6 and 11); old leader writes unchanged | New worker: **dual-read fallback path** per §3.6 — applies when no usable commit exists or alias `LeaderRevision` is fresher. Old worker: source of truth (current behaviour). |
| `heartbeat.<W>` (**dual-format**) | v1 JSON: `SchemaVersion`, `Capabilities`, `AppliedVersion`, `AppliedDigest`, `AppliedSourceRevision`, `AppliedSourceRevKnown`, `LeaderRevision`, `AppliedAt`, `Timestamp`. Legacy: RFC3339Nano timestamp bytes (no object). | New worker writes v1 JSON after successful Apply; old worker writes raw timestamp bytes via the existing legacy writer | New leader: `DecodeHeartbeat` handles both formats (§4.1); legacy decodes to zero-field heartbeat with only `Timestamp` populated. Old leader: still reads timestamp; cannot parse v1 JSON but doesn't need to (it doesn't run the audit). |

#### Dual-read gating during rolling upgrade (new worker)

New workers watch **both** `assignment._commit` and their own
legacy alias `assignment.<W>`, and apply the source-of-truth
selection rule from §3.6:
- Prefer commit when it exists and is at least as fresh
  (`LeaderRevision`-wise) as any legacy alias.
- Fall back to the legacy alias when no commit exists, or when an
  old leader has written a higher-`LeaderRevision` alias than the
  last commit observed (mid-handoff to old-leader case).

This is the closure to review P0 #1 — without dual-read, an old
leader operating during the rolling upgrade silently orphans
partitions assigned to new workers.

Combination outcomes during rolling upgrade:
- **New leader + new worker**: commit path; full safety.
- **Old leader + new worker**: no `assignment._commit` exists.
  New worker takes the legacy-compat path on its own
  `assignment.<W>` and applies normally. No regression vs.
  pre-fix behaviour; the audit classifies the old leader's
  workers based on whatever heartbeat acks they publish (still
  meaningful, just without a commit to verify against).
- **New leader + old worker**: old worker reads the legacy alias
  `assignment.<W>`, which the new leader writes as a **mandatory
  pre-commit barrier** (step 6 of §3.5) — not best-effort.
  Failures abort the batch loudly. Audit classifies old workers
  as `unverifiable` and skips escalation.
- **Mid-leader handoff (new → old)**: a stale `assignment._commit`
  remains in KV with the prior new leader's `LeaderRevision=R1`.
  The now-active old leader writes legacy aliases at
  `LeaderRevision=R2 > R1`. New workers' source-of-truth rule
  sees `legacyEntry.LeaderRevision > commit.LeaderRevision` and
  switches to the legacy-compat path. When the next new leader
  takes over with `R3 > R2`, it writes a new commit; new workers
  switch back to the commit path. No partitions orphaned at any
  point in the chain.
- **New leader crashes mid-batch (between payload writes and commit
  write)**: payload keys exist as orphans; no commit references
  them. New workers see no change. Old workers may read whatever
  the previous commit's legacy aliases said (unchanged). Next
  leader's publish CAS-writes a fresh commit. Orphan payloads are
  reaped by GC.

#### Audit gating (new leader interpreting heartbeats)

New leader's audit loop classifies each worker by comparing the
heartbeat to **the current commit**, not the current source snapshot
(see §4.2 for the rationale):

```
for each W in active workers:
    hb = heartbeat[W]
    if hb.SchemaVersion == 0 || hb.Capabilities & CapAckV1 == 0:
        # Old or non-ack-capable worker — cannot prove "applied".
        # Trust the alive signal, do NOT classify as behind.
        classify(W, unverifiable)
        continue
    ref, hasRef = commit.Payloads[W]
    srcRevMatch = !commit.SourceRevisionKnown ||
                  (hb.AppliedSourceRevKnown &&
                   hb.AppliedSourceRevision == commit.SourceRevision)
    switch:
      case !hasRef && W in commit.Workers:
          classify(W, behind)   # malformed commit; worker can't apply
      case hb.LeaderRevision   != commit.LeaderRevision,
           hb.AppliedVersion   != commit.Version,
           !srcRevMatch,
           hasRef && hb.AppliedDigest != ref.SetDigest:
          classify(W, behind)
      default:
          classify(W, fully_applied)
```

Action policy (matches §4.2):
- `fully_applied` → healthy.
- `behind` within `ApplyGracePeriod` → no action (worker is in its
  retry window).
- `behind` past `ApplyGracePeriod` → metric
  `parti.audit.workers_behind` increments; manager-side apply-retry
  loop continues. **No reassignment.**
- `behind` past `ExtendedApplyGracePeriod` AND full capability chain
  on behind worker AND full chain on at least one target →
  schedule audit-driven rebalance; safety provided by ClaimsRegistry +
  processing-gate (§4.2).
- `behind` past `ExtendedApplyGracePeriod` with any required cap
  missing → metric `parti.audit.escalation_skipped{reason="cap_missing_..."}`
  increments; wait for heartbeat TTL (vanished-worker path is the only
  safe trigger when caps are insufficient).
- `unverifiable` (old or non-ack-capable worker) → trust the alive
  signal; never escalate.

The key concession to rolling upgrade: **audit enforcement power is
proportional to the fraction of new workers carrying the full
capability chain.** At 0% capable workers, audit is informational
only. At 100% capable workers, audit fully enforces. In between, only
the capable subset participates in escalation; the rest are trusted.

#### Coverage check during the mixed window

The publisher's set-equality check at publish time (§3.8) is
leader-internal and always enforced once the leader upgrades — it
proves `union(assigned partition IDs) == source partition IDs`
before the commit is written. This is the **single canonical
coverage proof**.

At audit time the leader does **not** recompute a runtime union of
digests. xxh3 is not a homomorphic set fingerprint; a 64-bit hash
union cannot detect duplicates or prove disjoint-union semantics.
Instead, coverage holds by transitivity:

```
(a) publish-time set-equality check at §3.8 proved that
    union(assigned partition IDs in commit V) == source partition IDs
    at SourceRevision S (where SourceRevisionKnown=true).
(b) audit per-worker check above proved each capable W in
    commit.Workers is running exactly commit.Payloads[W].SetDigest.
(c) commit V was atomically committed (CAS-fenced) and its referenced
    payloads are immutable (content-addressable), so the bytes the
    worker fetches via ref are byte-identical to the bytes the leader
    committed.
∴  the cluster covers source at revision S iff every W in
   commit.Workers is fully_applied or unverifiable.
```

During the migration window, `unverifiable` workers are assumed to be
running what we wrote to them (same trust level as today — they can't
confirm). Once the rolling upgrade completes, `unverifiable` is empty
and the proof is strict.

### What still doesn't get fixed during the rolling window

Honest accounting:

- **Apply-failure detection on old workers** is unchanged. If an old
  worker's `handoffCoordinator.Apply` fails, it logs and continues with
  stale assignment; the leader can't tell. Same exposure as today,
  resolved on pod replacement.
- **Lost-update protection on old writers** is unchanged. An old worker
  calling `source.Update()` still does blind `kv.Put`. CAS protects
  new↔new and absorbs new↔old collisions, but two olds racing is the
  same as today.
- **Watcher reconcile on old workers' assignment watchers** is
  unchanged. An old worker whose `assignment.<W>` watcher closes
  silently remains stuck. Same exposure as today, resolved on pod
  replacement.

In every case the floor is "no worse than current". New code never
breaks old code; it just doesn't help it. Full benefit lands once the
rolling upgrade completes — which it always does, since K8s replaces
every pod in finite time.

### Operator-facing checklist

1. Upgrade parti dependency, recompile, deploy via your normal rolling
   strategy. No source changes required, no flag day, no manual KV
   migration, no required order.
2. (Optional, recommended after upgrade) Replace any
   `List(); append; Update(list)` call sites with `Modify(ctx, fn)`.
3. Monitor `parti.audit.unverifiable_workers` (added in Pillar 4) — it
   should drop to zero once the rolling upgrade finishes. A non-zero
   steady-state value means an old-version pod is still running.
4. Monitor `parti.audit.coverage_violations` — should be zero in steady
   state; non-zero indicates the invariant repair loop fired.

---

## Out of scope (deliberately deferred)

- Exposing the listener signal type to carry "delete vs change" — the
  current `chan struct{}` is enough for the rebalance trigger, which always
  re-reads `List()` anyway.
- Caching strategy for hot `List()` callers — current copy-on-read is fine.
- Auto-recovery from a permanently-broken bucket — the manager's degraded-mode
  circuit (`m.recordKVError`) already handles that at a higher level.
