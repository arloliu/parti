# PR-2 Implementation Spec — Pre-Apply Serialized (V, LR) Gate (W15+W16)

Implements **W15** and **W16** from [`00-fix-plan.md`](./00-fix-plan.md). Closes the two pre-existing latent races documented in PR-1's spec §10.

The current apply pipeline has a structural hole: between the pre-Apply monotonicity gate (`manager_assignment.go:777`) and `m.assignment.Store` (`manager_assignment.go:817`), there is **no synchronization across apply paths**. Three independent goroutines call `applyAssignment` today — commit watcher (`manager_assignment.go:618`), alias watcher (`manager_assignment.go:434`), and `scheduleApplyRetry`'s background goroutine (`manager_assignment.go:928`) — and `twoPhaseCoordinator.Apply` has no top-level serialization (`internal/assignment/handoff/twophase.go:21-27, 57-113`). So a stale Apply can run prepare → apply → commit → stabilize concurrently with a fresher Apply, then overwrite the fresher snapshot. The pre-Apply gate at line 777 sees a stale snapshot (the winner hasn't stored yet) and lets the loser through; the loser then stores after the winner.

**Resolution: serialize the entire `(stale-check, Apply, Store)` sequence under a single mutex `applyStoreMu`, with a strict `(Version, LeaderRevision)` lex-ordered stale check at the head of the critical section.** Pre-Apply serialization (not post-Apply revalidation) avoids running the loser's Apply at all — closing the orphaned-stable-claim concern that pure post-Apply revalidation would otherwise leave open. The same helper is shared with `refreshAssignmentFromNATS`, closing the cross-path Store race surfaced by plan-review v1.

**Revision history:**
- v1 — initial draft proposing post-Apply revalidation. Plan-review (Copilot `gpt-5.5 xhigh`) returned 3 P0 / 3 P1: (P0-1) read-then-write gate non-atomic; (P0-2) sweep does not reclaim stable claims so loser Apply leaks orphan claims; (P0-3) V=0 carve-out admits regression. See `tmp/02-pr2-spec_pr2-w15-w16_review.md`.
- v2 — switched design to pre-Apply mutex serialization, tightened V=0 carve-out, brought `refreshAssignmentFromNATS` under `monotonicStore`. Plan-review v2 returned 2 P0 / 1 P1 / 2 P2: (P0-A) failed-Apply partial-claim orphan when retry is later stale-dropped; (P0-B) heartbeat Ack outside `applyStoreMu` can regress same-V/lower-LR because the publisher's monotonicity is V-only. See `tmp/02-pr2-spec_pr2-w15-w16_v2_review.md`.
- v3 (this revision) — addresses v2 P0/P1/P2:
  - P0-A (failed-Apply partial-claim orphan): documented in §10 as a **pre-existing coordinator-level limitation** PR-2 does not introduce and does not close. Includes empirical reachability analysis and scope-out rationale.
  - P0-B (Ack regression outside lock): **fixed**. `SetAppliedAssignment` is moved INSIDE `applyStoreMu` so the heartbeat publisher cannot observe out-of-order `(V, LR)` snapshots. `PublishNow` stays OUTSIDE the lock (network IO is bounded by the publisher's internal queue, not by lock-holder progress).
  - P1-A (monotonicStore contract): **fixed**. Godoc says refresh-only; the "Used by" list no longer contains `applyAssignmentWithPrev`.
  - P2-A (test-seam Stores): §6.1's table is explicitly titled "production `m.assignment.Store` sites" and a sub-note lists known test-only direct Stores.
  - P2-B (metric name): renamed (v2) `RecordPostApplyStaleStoreDropped` → (v3) `RecordStaleSnapshotStoreDropped`. Godoc says it counts "candidates dropped by the pre-Apply / refresh stale-snapshot gate" with no implication that Apply ran.

---

## 1. Anchors (verified 2026-05-19 against HEAD `34d82c2` — PR-1 merged)

| Anchor | File:line | Status |
|---|---|---|
| `applyAssignmentWithPrev` — single apply pipeline | `manager_assignment.go:764-851` | **modified** — serialized under `applyStoreMu`; gate at head of critical section |
| Pre-Apply monotonicity gate (V-only, V=0 carve-out) | `manager_assignment.go:777-779` | **replaced** — old V-only gate becomes part of the new `(V, LR)` lex gate inside the critical section |
| `handoffCoordinator.Apply` call site | `manager_assignment.go:783` | reference only |
| `m.assignment.Store(newAssignment)` | `manager_assignment.go:817` | **inside** the new critical section |
| `m.assignment.Store(curAssignment)` in `refreshAssignmentFromNATS` | `manager_assignment.go:970` | **modified** — routes through `monotonicStore` (gate + LSR-advance + Store), so it cannot regress mid-Apply |
| `refreshAssignmentFromNATS` caller (degraded recovery) | `manager_degraded.go:228-244` | reference only — gains correctness automatically from §3.4 below |
| Alias-path applyAssignment caller | `manager_assignment.go:434` | reference — one of three runtime race participants |
| Commit-path applyAssignment caller (case c/d) | `manager_assignment.go:618` | reference — one of three runtime race participants |
| `scheduleApplyRetry` goroutine | `manager_assignment.go:892-944` | reference — third runtime race participant |
| `pendingApplyInFlight` (commit-only case-(e) coalescer) | `manager_assignment.go:590, 608, 629` | **left alone** — case-(e) coalescing remains intact; `applyStoreMu` is a separate lower-level primitive |
| `stashedApplyRetry` (retry-only stash) | `manager_assignment.go:894-902, 924` | **left alone** |
| Other `m.assignment.Store` sites (PRE-RUNTIME) | `manager.go:297` (`NewManager` init), `manager.go:551` (cold-bootstrap empty), `manager_election.go:320` (`waitForAssignment`) | **left alone** — all three execute before `monitorCommitChanges` / `monitorAssignmentChanges` start (see §6.1 lifecycle table) |
| `RecordStaleLeaderRejected` (sibling metric pattern) | `types/metrics_collector.go:95-99`, `internal/metrics/nop.go:203-205`, recording fixture `manager_commit_state_machine_test.go:46` | template for the new `RecordStaleSnapshotStoreDropped` metric |
| `recordingHandoff.blockFirstApply` fixture | `manager_commit_state_machine_test.go:57-100` | **extended** — add a version-keyed barrier; see §5 |
| `recordingMetrics` fixture | `manager_commit_state_machine_test.go:26-55` | **extended** — add the new counter |
| Authority selector (alias vs commit) | `manager_select_authority.go:33-66` | reference only — orthogonal to PR-2 |
| `twoPhaseCoordinator.Apply` is NOT internally serialized | `internal/assignment/handoff/twophase.go:21-27, 57-113` | reference — motivates pre-Apply (not post-Apply) serialization |

**Pre-existing tmp/ artifacts unrelated to this PR:** `tmp/02-pr2-spec_pr2-impl-spec_v1_review.md` and `tmp/02-pr2-spec_pr2-impl-spec_v2_review.md` review the `assignment-correctness-fixes` plan's PR-2 spec, not this `worker-state-hardening` PR-2. Ignore them for revision context. PR-2's v1 plan-review is at `tmp/02-pr2-spec_pr2-w15-w16_review.md`.

Verified against current branch `main` @ `34d82c2` (PR-1 already merged). Spec author MUST re-verify line numbers immediately before implementing if HEAD has advanced.

---

## 2. Design — pre-Apply mutex serialization with `(V, LR)` lex gate

### 2.1 The bug at a glance

Three goroutines can be inside `applyAssignmentWithPrev` simultaneously, each holding their own local copy of `newAssignment`:

| Path | Goroutine | Trigger |
|---|---|---|
| Commit watcher | `monitorCommitChanges` → `watchCommit` | new `assignment._commit` value (or PR-1 reconcile tick) |
| Alias watcher | `monitorAssignmentChanges` → `watchAssignment` | new `assignment.<W>` value (or PR-1 reconcile tick) |
| Apply retry | `scheduleApplyRetry`'s spawned goroutine | retry of a previously-failed apply, scheduled with exponential backoff |

The current race window inside `applyAssignmentWithPrev` is:

```
T0: candidate reads m.CurrentAssignment() at line 766 (curAssignment)
T1: pre-Apply gate at line 777 — V-only check vs curAssignment
T2: candidate calls handoffCoordinator.Apply (LONG: prepare → apply → commit → stabilize)
T3: candidate stores m.assignment.Store(newAssignment) at line 817
```

`[T1, T3]` is currently unprotected. The pre-Apply gate's `Version != 0 && Version < cur.Version` check misses (i) same-V-higher-LR cross-leader races (W15) and (ii) any "raced ahead during my Apply" scenario, because `cur.Version` was read BEFORE the winner's Store.

### 2.2 The minimal fix: pre-Apply serialization

Hold a single mutex `applyStoreMu` across the entire `(stale-check, Apply, Store)` sequence. The critical section's head re-reads `m.CurrentAssignment()` and compares the candidate's `(Version, LeaderRevision)` against the live snapshot. If stale, the function returns BEFORE calling Apply — so no coordinator-side prepare/commit/stabilize runs for the loser.

**Why pre-Apply, not post-Apply.** Plan-review v1's P0-2 surfaced that `twoPhaseCoordinator.preparePhase` can create a fresh stable claim immediately (`internal/assignment/handoff/twophase.go:249-264`), and `commitPhase` + `stabilizePhase` finalize stable ownership (`twophase.go:344-370, 396-418`). The sweep loop explicitly skips stable claims (`twophase.go:455-460`). Therefore a loser whose Apply ran to completion can leave orphaned stable claims for partitions not in the winner's snapshot. Sweep does not reclaim them. The only safe fix is to **not run loser Apply at all** — which requires serialization BEFORE Apply, not after.

**Mutex, not CAS.** Plan-review v1's P0-1 suggested CAS as an alternative atomicity primitive, but `m.assignment` stores `Assignment` (a struct containing `[]Partition`) via `atomic.Value`. `atomic.Value.CompareAndSwap` requires comparable types; `Assignment` is not comparable (slice fields). A `sync.Mutex` is the smallest correct primitive. (Switching to `atomic.Pointer[Assignment]` would enable pointer CAS but is a wider refactor with no correctness benefit over the mutex.)

### 2.3 Alternative considered: shared apply-interlock via `pendingApplyInFlight`

A natural alternative is to repurpose `pendingApplyInFlight` (currently commit-only) as a cross-path interlock. This was Option (ii) in v1's §2.3.

**Rejected**, because:

1. **Different scope.** `pendingApplyInFlight` is the load-bearing primitive for commit-from-commit case-(e) coalescing (stash the higher-Version target, drain on flag release). Reusing it for cross-path serialization would entangle two different concerns. The new `applyStoreMu` is purely a serialization primitive with no coalescing semantics, leaving case-(e) intact at the commit-handler layer (`manager_assignment.go:589-601, 629-633`).
2. **Alias-side stash design surface.** Repurposing the flag would force adding an alias-side stash or relying entirely on PR-1's reconcile arm to recover dropped aliases. The mutex approach simply blocks the alias path until the winner returns, then runs the alias check + Apply normally.
3. **Wider blast radius.** `pendingApplyInFlight`'s acquire/release boundaries are carefully tuned (lines 590, 608, 629) and have specific ordering requirements with `stashedCommit` drain. Moving them would risk an unintended semantic shift.

### 2.4 The stale check — strict `(V, LR)` lex order with safe V=0

```go
// isApplyResultStale returns true iff the candidate is older than the
// live snapshot in (Version, LeaderRevision) lex order. Used at the head
// of the applyStoreMu critical section to drop loser candidates BEFORE
// any coordinator-side side effect.
//
// V=0 semantics (W15+W16 fix, plan-review v1 P0-3):
//   - candidate.V=0 over cur.V=0: not stale. Permits the cold-bootstrap
//     path's idempotent re-apply over the just-initialized Assignment{}
//     snapshot (manager.go:297 → applyInitialAssignment).
//   - candidate.V=0 over cur.V>0: STALE. A V=0 retry/legacy candidate
//     would otherwise regress a real snapshot. The pre-existing pre-Apply
//     gate at line 777 admits this incorrectly; PR-2's helper does not.
//
// Same V same LR: not stale (idempotent reapply path).
// Same V lower LR: STALE (the W15 cross-leader case).
// Lower V: STALE (the W16 stale-retry case).
func isApplyResultStale(candidate, cur Assignment) bool {
    if candidate.Version == 0 {
        // V=0 only OK over V=0 snapshot.
        return cur.Version != 0
    }
    if candidate.Version != cur.Version {
        return candidate.Version < cur.Version
    }
    return candidate.LeaderRevision < cur.LeaderRevision
}
```

**Compatibility with the existing pre-Apply gate at line 777.** The old gate's `newAssignment.Version != 0 && newAssignment.Version < curAssignment.Version` is strictly more permissive than `isApplyResultStale`:

| Scenario | Old gate (line 777) | New `isApplyResultStale` |
|---|---|---|
| candidate.V=10, cur.V=5 | proceed | proceed |
| candidate.V=5, cur.V=10 | drop | drop |
| candidate.V=0, cur.V=0 | proceed | proceed |
| candidate.V=0, cur.V=10 | **proceed** (current bug) | **drop** (fix) |
| candidate.V=10, cur.V=10 | proceed | proceed (idempotent) |
| candidate.V=10/LR=3, cur.V=10/LR=5 | **proceed** (W15 bug) | **drop** (fix) |
| candidate.V=10/LR=5, cur.V=10/LR=3 | proceed | proceed |

The old gate is REPLACED by the new helper inside the critical section. PR-2 does not need to keep the old line-777 gate as a fast-path — the new helper runs inside `applyStoreMu` and is just an `atomic.Value.Load` + tuple comparison, ~20ns. Saving the cost of a contended Apply is the dominant gain; saving the ~20ns of the check itself is noise.

---

## 3. Implementation

### 3.1 Add the `applyStoreMu` mutex to `Manager`

**Location:** `manager.go` near the existing `mu` field. Brief Godoc:

```go
// applyStoreMu serializes the (stale-check, handoff Apply, snapshot Store,
// LSR advance) critical section across all callers — commit watcher
// (handleCommitValue → applyAssignment), alias watcher (handleAssignmentEntry
// → applyAssignment), the scheduleApplyRetry goroutine, and the operator
// recovery refresh path (refreshAssignmentFromNATS → monotonicStore).
//
// Without serialization, three independent goroutines can each pass the
// pre-Apply gate against a stale snapshot, run handoffCoordinator.Apply
// concurrently, and Store in arbitrary order — losing the W15 cross-leader
// race and the W16 apply-retry race. See PR-2 spec docs/plans/worker-state-
// hardening/02-pr2-spec.md.
//
// Lock contract: applyStoreMu MUST NOT be acquired while holding m.mu.
// All Manager methods that take m.mu run quickly and do not call into the
// apply pipeline, so there is no acquisition-order hazard today; this rule
// is documented to keep it that way.
applyStoreMu sync.Mutex
```

### 3.2 Refactor `applyAssignmentWithPrev`

**Current shape** (`manager_assignment.go:764-851`): pre-Apply V-only gate at 777, Apply at 783, LSR advance at 812, Store at 817, ack/hooks at 833+.

**Target shape:**

```go
func (m *Manager) applyAssignmentWithPrev(oldAssignment, newAssignment Assignment) error {
    workerID := m.WorkerID()

    // Critical section: (stale check, Apply, LSR advance, Store) atomic
    // across all apply-pipeline callers. See applyStoreMu Godoc.
    m.applyStoreMu.Lock()

    curAssignment := m.CurrentAssignment()

    // Stale gate (W15+W16 close). Drops the candidate BEFORE Apply, so
    // no coordinator-side prepare/commit/stabilize runs for a loser —
    // avoiding orphaned stable claims (plan-review v1 P0-2).
    if isApplyResultStale(newAssignment, curAssignment) {
        m.applyStoreMu.Unlock()
        m.metrics.RecordStaleSnapshotStoreDropped()
        m.logger.Info("apply dropped by (V, LR) stale gate",
            "worker_id", workerID,
            "candidate_version", newAssignment.Version,
            "candidate_leader_revision", newAssignment.LeaderRevision,
            "current_version", curAssignment.Version,
            "current_leader_revision", curAssignment.LeaderRevision,
        )
        return nil
    }

    // Apply via handoff coordinator. Holds applyStoreMu throughout — this
    // serializes coordinator Apply calls across paths and is the load-
    // bearing change for W15+W16.
    applyErr := m.handoffCoordinator.Apply(m.ctx, workerID, oldAssignment, newAssignment)

    // Capability sampling runs unconditionally — semantics unchanged from
    // the prior implementation. Safe under the mutex: reportConsumerCapabilities
    // does not call back into the apply pipeline.
    m.reportConsumerCapabilities()

    if applyErr != nil {
        m.applyStoreMu.Unlock()
        m.logError("handoff apply failed", "error", applyErr)
        m.scheduleApplyRetry(newAssignment)
        return applyErr
    }

    // LSR advance + Store + heartbeat snapshot update. Order: LSR before
    // Store (PR-1 §4 invariant), then SetAppliedAssignment inside the lock
    // (plan-review v2 P0-B: the heartbeat publisher's monotonicity is
    // V-only, not (V, LR) lex, so a same-V/lower-LR Ack from a slow loser
    // could regress the heartbeat after the lock is released).
    m.updateLastSeenLeaderRevision(newAssignment.LeaderRevision)
    m.assignment.Store(newAssignment)
    if hook := m.testHookAfterApplyStore; hook != nil {
        hook(newAssignment)
    }
    appliedDigest := types.PartitionSetDigest(newAssignment.Partitions)
    m.heartbeat.SetAppliedAssignment(heartbeat.AppliedAssignment{
        LeaderRevision:        newAssignment.LeaderRevision,
        AppliedVersion:        newAssignment.Version,
        AppliedDigest:         appliedDigest,
        AppliedSourceRevision: newAssignment.SourceRevision,
        AppliedSourceRevKnown: newAssignment.SourceRevisionKnown,
        AppliedAt:             time.Now(),
    })

    m.applyStoreMu.Unlock()

    // Logging + heartbeat PUBLISH + hooks + metrics: outside the lock.
    //   - PublishNow does network IO; holding applyStoreMu through it
    //     would needlessly serialize publish. SetAppliedAssignment above
    //     already updated the heartbeat's in-memory snapshot under the
    //     lock, so PublishNow always sends a snapshot consistent with
    //     the just-stored manager state.
    //   - invokeAssignmentChangedHooks fires user hooks asynchronously
    //     via the wg-managed invokeHook helper.
    m.logger.Info("assignment applied", ...)  // existing fields
    if err := m.heartbeat.PublishNow(m.ctx); err != nil {
        m.logError("heartbeat publish-now after apply failed", "error", err)
    }

    m.recordAssignmentMetrics(oldAssignment, newAssignment)
    m.invokeAssignmentChangedHooks(workerID, oldAssignment, newAssignment)

    return nil
}
```

**Key changes vs current:**
1. `applyStoreMu.Lock()` at function head, three `applyStoreMu.Unlock()` paths (stale drop, apply error, success after Store + SetAppliedAssignment). Explicit `Unlock` instead of `defer` so `PublishNow` and hooks run outside the lock.
2. Pre-Apply gate at line 777 replaced by `isApplyResultStale` inside the locked region.
3. Apply, LSR advance, Store, AND `SetAppliedAssignment` now under the same lock — no third party can interleave, and the heartbeat's in-memory snapshot can never observe out-of-order `(V, LR)` (plan-review v2 P0-B fix).
4. `PublishNow`, metrics, and hooks moved AFTER the unlock so they don't block other apply paths.

### 3.3 Extract `monotonicStore(newAssignment)` helper

Both `applyAssignmentWithPrev`'s critical section and `refreshAssignmentFromNATS` should share the same `(stale-check + LSR advance + Store)` logic. Extract:

```go
// monotonicStore performs the (gate-check, LSR-advance, Store) sequence
// under applyStoreMu. Returns true if the Store landed (candidate was
// fresh enough); false if the candidate was dropped as stale.
//
// Callers MUST NOT hold applyStoreMu. The helper acquires and releases
// it itself.
//
// REFRESH-PATH ONLY. Used by refreshAssignmentFromNATS to bring an
// authoritative KV snapshot under the same (V, LR) gate as the apply
// pipeline. Not usable from applyAssignmentWithPrev because that
// function must hold applyStoreMu ACROSS handoffCoordinator.Apply (see
// §3.2). Inlining the same logic in both call sites is intentional.
//
// NOT used by pre-Start lifecycle Stores (manager.go:297, manager.go:551,
// manager_election.go:320) — those execute before monitor goroutines start
// and have no concurrency exposure.
func (m *Manager) monotonicStore(newAssignment Assignment) bool {
    m.applyStoreMu.Lock()
    defer m.applyStoreMu.Unlock()

    cur := m.CurrentAssignment()
    if isApplyResultStale(newAssignment, cur) {
        m.metrics.RecordStaleSnapshotStoreDropped()
        return false
    }

    m.updateLastSeenLeaderRevision(newAssignment.LeaderRevision)
    m.assignment.Store(newAssignment)
    return true
}
```

This is NOT used directly by `applyAssignmentWithPrev` — that function needs the lock held across `Apply` as well, so it inlines the critical section. The helper is for the simpler refresh path only.

### 3.4 Wire `refreshAssignmentFromNATS` through `monotonicStore`

**Current shape** (`manager_assignment.go:953-980`): fetches KV value, unmarshals, calls `m.assignment.Store(curAssignment)` directly. Does NOT update LSR. Does NOT acquire any lock.

**Target shape:**

```go
func (m *Manager) refreshAssignmentFromNATS() error {
    workerID := m.WorkerID()
    if workerID == "" {
        return errors.New("worker ID not set")
    }

    key := fmt.Sprintf("assignment.%s", workerID)
    entry, err := m.assignmentKV.Get(m.ctx, key)
    if err != nil {
        return fmt.Errorf("failed to get assignment from KV: %w", err)
    }

    var curAssignment Assignment
    if err := json.Unmarshal(entry.Value(), &curAssignment); err != nil {
        return fmt.Errorf("failed to unmarshal assignment: %w", err)
    }

    if !m.monotonicStore(curAssignment) {
        // The current snapshot is fresher than what KV reports — common
        // when refresh races the apply pipeline. Not an error.
        m.logger.Debug("refresh skipped: snapshot already at-or-newer than KV",
            "version", curAssignment.Version,
            "leader_revision", curAssignment.LeaderRevision,
        )
        return nil
    }

    m.lastAssignmentAt.Store(time.Now().UnixNano())
    m.lastAssignment.Store(m.clonePartitions(curAssignment.Partitions))

    m.logger.Info("assignment refreshed from NATS",
        "version", curAssignment.Version,
        "partitions", len(curAssignment.Partitions),
    )

    return nil
}
```

**Two semantic differences from the current implementation:**

1. **Acquires `applyStoreMu`** via the helper. Concurrent apply pipelines cannot interleave.
2. **Advances LSR** on a successful refresh. The current implementation does NOT, which means a refresh that races with a stale apply could let the apply's stale-leader fence (line 366) over-fire on subsequent commits. By advancing LSR consistently with the snapshot, the LSR-before-Store invariant holds for the refresh path too.

**Bookkeeping (`lastAssignmentAt`, `lastAssignment`):** these are tracked by the refresh path for observability (`m.lastAssignment.Store` clones the partitions). They are not in the apply pipeline's critical-path and remain outside the mutex.

### 3.5 Add the new metric

Three sites:

1. **Interface declaration** in `types/metrics_collector.go` near `RecordStaleLeaderRejected` at line 95-99:
   ```go
   // RecordStaleSnapshotStoreDropped counts assignment candidates dropped
   // by the pre-Apply / refresh stale-snapshot gate (W15+W16). A nonzero
   // counter indicates concurrent apply paths racing for the same worker;
   // small numbers are expected under churn (rolling upgrade, leader
   // handoff).
   //
   // Semantics: this counter increments BEFORE handoffCoordinator.Apply
   // runs (for `applyAssignmentWithPrev`'s pre-Apply gate) or BEFORE Store
   // (for `refreshAssignmentFromNATS` via the `monotonicStore` helper).
   // It does NOT fire on Apply errors — those go through scheduleApplyRetry
   // without firing this counter.
   RecordStaleSnapshotStoreDropped()
   ```
2. **Nop implementation** in `internal/metrics/nop.go` near line 203-205:
   ```go
   func (n *NopMetrics) RecordStaleSnapshotStoreDropped() {}
   ```
3. **Test fixture** in `manager_commit_state_machine_test.go`:
   - Add `staleStoreDropped atomic.Int64` field to `recordingMetrics`.
   - Add method `func (r *recordingMetrics) RecordStaleSnapshotStoreDropped() { r.staleStoreDropped.Add(1) }`.

If the project's metrics emit to Prometheus / OTel through a non-Nop concrete type, that type also needs the new method. Verification step before commit: `grep -rn "RecordStaleLeaderRejected" .` and add `RecordStaleSnapshotStoreDropped` at every same site.

### 3.6 Extend `recordingHandoff` with a version-keyed barrier

Plan-review v1 P1-2 identified that the existing one-shot `blockFirstApply` CAS is insufficient for Test 5.2 (the retry path needs a SECOND block after the first Apply failed).

Add to `recordingHandoff`:

```go
// blockOnVersion gates Apply on next.Version == blockOnVersion. Tests set
// the value via SetBlockOnVersion before triggering the apply they want
// to barrier. The barrier is one-shot per version — once released, the
// blocker is cleared. blockOnVersionReady is closed when the barrier hits;
// blockOnVersionRelease is closed by the test to unblock.
blockOnVersion          atomic.Int64
blockOnVersionReady     atomic.Pointer[chan struct{}]
blockOnVersionRelease   atomic.Pointer[chan struct{}]

// SetBlockOnVersion arms a barrier on the next Apply call whose
// next.Version equals v. The caller can wait on the returned `ready`
// channel for "the barrier was hit" and close the returned `release`
// channel to unblock.
func (h *recordingHandoff) SetBlockOnVersion(v int64) (ready, release chan struct{}) {
    ready = make(chan struct{})
    release = make(chan struct{})
    h.blockOnVersionReady.Store(&ready)
    h.blockOnVersionRelease.Store(&release)
    h.blockOnVersion.Store(v)
    return ready, release
}

// Apply (modified) — at function head, check blockOnVersion BEFORE
// blockFirstApply. If the barrier fires, signal ready and wait on release.
func (h *recordingHandoff) Apply(...) error {
    if v := h.blockOnVersion.Load(); v != 0 && v == next.Version {
        h.blockOnVersion.Store(0)  // one-shot
        ready := h.blockOnVersionReady.Swap(nil)
        release := h.blockOnVersionRelease.Swap(nil)
        if ready != nil { close(*ready) }
        if release != nil { <-*release }
    }
    // ... existing blockFirstApply logic
}
```

The existing `blockFirstApply` one-shot stays — Test 5.1 (cross-path) uses it, Test 5.2 (retry) uses `SetBlockOnVersion`.

### 3.7 Required updates to existing tests

**None expected.** The new gate only fires when an apply candidate is strictly stale against the live snapshot — a scenario the existing tests do not construct. If a test breaks, investigate before patching: it likely depended on undefined cross-apply interleavings.

If a test mutates `m.assignment.Store` directly (test-only seam) and expects no gate fire, that's fine — the helper is only invoked from `applyAssignmentWithPrev` and `refreshAssignmentFromNATS`.

---

## 4. Idempotency contract and race analysis

### 4.1 Race: W15 cross-path stale-store

**Setup.** Worker is mid-handoff. Commit `C1`(`V=10, LR=3`) arrives on the commit watcher and enters `applyAssignmentWithPrev`. Inside, it acquires `applyStoreMu`. Its handoff coordinator Apply blocks on a slow prepare-phase CAS. Meanwhile alias `A1`(`V=10, LR=4`) arrives on the alias watcher — a new leader (LR=4) has published the same partitioning.

**Sequence with PR-2's lock.**
1. `C1` acquires `applyStoreMu`, checks stale: cur is `Assignment{}` (V=0), `isApplyResultStale((10,3), (0,0))` = false. Proceeds to Apply.
2. `C1`'s Apply blocks.
3. `A1`'s goroutine enters `applyAssignmentWithPrev`, tries to acquire `applyStoreMu`, **blocks** waiting for `C1`.
4. `C1`'s Apply returns nil. `C1` advances LSR to 3, stores `(V=10, LR=3)`. Releases lock.
5. `A1` acquires `applyStoreMu`. Checks stale: cur is `(V=10, LR=3)`, `isApplyResultStale((10, 4), (10, 3))` = false (alias has higher LR). Proceeds.
6. `A1` Apply, advances LSR to 4, stores `(V=10, LR=4)`. Releases.

**Snapshot ends at `(V=10, LR=4)`.** The fresher leader's view wins. No regression. The metric does NOT fire (nothing was stale). The cost: `A1` waited for `C1`'s Apply to complete, which is acceptable — `A1` could not have safely run concurrently with `C1` anyway.

**Reverse sequence (older leader's commit arrives FIRST under the lock).** If `A1` enters and acquires the lock FIRST (its watcher fired first), then `A1` applies and stores `(V=10, LR=4)`. `C1` waits, then acquires, checks stale: cur is `(V=10, LR=4)`, `isApplyResultStale((10, 3), (10, 4))` = true. **`C1` drops, metric fires.** Snapshot ends at `(V=10, LR=4)`. Correct.

### 4.2 Race: W16 apply-retry stale-store

**Setup.** Commit `C1`(`V=5, LR=10`) fails Apply (handoff coordinator returns error). `scheduleApplyRetry` stashes `C1` and arms the retry goroutine. Before the retry fires, commit `C2`(`V=10, LR=15`) arrives on the commit watcher.

**Sequence with PR-2's lock.**
1. `C1` fails Apply under `applyStoreMu`, releases lock, schedules retry.
2. `C2` enters `applyAssignmentWithPrev`, acquires lock, checks stale: cur is `Assignment{}`, OK. Applies, advances LSR to 15, stores `(V=10, LR=15)`. Releases.
3. Retry goroutine wakes, calls `applyAssignment(C1)`, enters `applyAssignmentWithPrev`, acquires lock.
4. Checks stale: cur is `(V=10, LR=15)`, `isApplyResultStale((5, 10), (10, 15))` = true. **`C1` retry drops, metric fires.** No Apply runs for `C1`. Releases lock.

**Snapshot ends at `(V=10, LR=15)`.** No regression. No orphaned coordinator claims (no loser Apply ran).

**Counterfactual without the lock:** in the current code, `C2`'s Apply and `C1`'s retry Apply could interleave; the retry could finish Apply after `C2`'s Store and then store `(V=5, LR=10)` regressing the snapshot.

### 4.3 Race: cross-path with `refreshAssignmentFromNATS`

**Setup.** Degraded recovery invokes `refreshAssignmentFromNATS` while an apply pipeline goroutine is mid-`applyAssignmentWithPrev`.

**Sequence with PR-2's helper.**
1. Apply goroutine holds `applyStoreMu`, is mid-Apply.
2. Refresh goroutine calls `monotonicStore(fetchedAssignment)`, tries to acquire `applyStoreMu`, blocks.
3. Apply finishes its critical section, releases.
4. Refresh acquires, checks stale: if `fetchedAssignment` is fresher than the just-stored snapshot, it stores (and LSR advances); if not, it drops with metric.

Either way: no regression, no missing LSR advance.

### 4.4 Same `(V, LR)` idempotent reapply

The bootstrap path (`applyInitialAssignment`) calls `applyAssignmentWithPrev(Assignment{}, fetched)`. `fetched.Version` typically equals the version `waitForAssignment` stored at line 320. So at the time `applyAssignmentWithPrev` runs its stale check, `cur.Version == fetched.Version` and `cur.LeaderRevision == fetched.LeaderRevision`. `isApplyResultStale` returns false (idempotent reapply path). Apply runs, snapshot stays unchanged but coordinator state is reconciled. Correct.

### 4.5 Lock contention under steady state

The mutex is acquired only inside `applyAssignmentWithPrev` and `monotonicStore`. Under steady state (no churn), there's at most one apply per watcher tick — contention is zero. Under churn (multiple watchers + retry firing simultaneously), the mutex serializes 2-3 applies. Each apply's critical section is dominated by `handoffCoordinator.Apply` (the prepare/apply/commit/stabilize phases). Worst case: ~3× Apply duration sequentially. This is the same total work the system was doing before — just serialized instead of concurrent. No throughput regression except in the degenerate "all three paths fire at once" case, which is bounded by watcher cadence (a few per second at most) and retry backoff (≥1s).

### 4.6 Lock ordering with `m.mu`

`m.mu` is the Manager's main lock, used for short critical sections (degraded-mode bookkeeping, configuration reads). It is NEVER acquired while the apply pipeline is running (no callee of `applyAssignmentWithPrev` takes `m.mu`). PR-2's `applyStoreMu` is therefore independent. Documented in the Godoc: "`applyStoreMu` MUST NOT be acquired while holding `m.mu`."

`m.assignment` is an `atomic.Value` and does not require a lock for Load. The mutex protects the higher-level invariant (atomic decide-then-commit), not the underlying field.

---

## 5. Tests

Five tests under a new file `manager_apply_serialization_test.go` (or appended to `manager_assignment_test.go`). All reuse `recordingHandoff`, `recordingMetrics`, and `newTestManager` from `manager_commit_state_machine_test.go`.

### Test 5.1 — W15: blocked commit Apply does not regress fresher alias snapshot

**Intent:** prove `applyStoreMu` serializes commit Apply and alias Apply, and that the loser's stale gate fires correctly.

**Mechanism:**
1. `newTestManager(t)`. Snapshot starts at `Assignment{}` (V=0).
2. Set `rh.blockFirstApply.Store(true)`. The first Apply call will signal `firstApplyReady` and wait for `releaseFirst`.
3. Launch goroutine G1: `m.applyAssignment(C1)` where `C1 = Assignment{Version: 10, LeaderRevision: 3}`.
4. Wait for `<-rh.firstApplyReady`. G1 is now blocked inside Apply, holding `applyStoreMu`.
5. Launch goroutine G2: `m.applyAssignment(A1)` where `A1 = Assignment{Version: 10, LeaderRevision: 4}`. G2 blocks on `applyStoreMu`.
6. Release G1: `close(rh.releaseFirst)`. G1's Apply returns nil, advances LSR to 3, stores `(V=10, LR=3)`, releases `applyStoreMu`.
7. G2 acquires the lock. Stale check: cur is `(V=10, LR=3)`, candidate is `(V=10, LR=4)`. Not stale. G2 Applies, advances LSR to 4, stores `(V=10, LR=4)`.
8. Wait for both G1 and G2 to return.
9. **Assertions:**
   - `m.CurrentAssignment()` is `(V=10, LR=4)`.
   - `m.lastSeenLeaderRevision.Load()` is `4`.
   - `rh.applyCount == 2` (both ran).
   - `rm.staleStoreDropped.Load() == 0` (G2 was not stale).

**Reverse-ordering variant (5.1b):** swap G1 and G2's candidates (G1 applies `A1` (V=10, LR=4); G2 applies `C1` (V=10, LR=3)). Same setup; assert at end:
   - `m.CurrentAssignment()` is `(V=10, LR=4)` (G1 won the lock first; G2 dropped as stale).
   - `rh.applyCount == 1` (only G1's Apply ran).
   - `rm.staleStoreDropped.Load() == 1`.

The reverse variant proves the gate FIRES on the loser path.

### Test 5.2 — W16: stale apply-retry does not regress fresher snapshot

**Intent:** prove the gate catches stale retry candidates without running their Apply.

**Mechanism:**
1. `newTestManager(t)`. `m.assignment` starts at `Assignment{}`.
2. Set `rh.errOnce` to return an error on the next Apply call.
3. `m.applyAssignment(C1)` where `C1 = (V=5, LR=10)` — Apply fails, `scheduleApplyRetry` arms with 1s+jitter backoff.
4. `m.applyAssignment(C2)` where `C2 = (V=10, LR=15)` — succeeds, snapshot now `(V=10, LR=15)`, LSR=15.
5. Wait `> 1.5s` for the retry to fire (use `require.Eventually` with a 5s deadline).
6. **Assertions:**
   - `m.CurrentAssignment()` is still `(V=10, LR=15)`.
   - `m.lastSeenLeaderRevision.Load()` is still `15`.
   - `rm.staleStoreDropped.Load() == 1` (retry dropped by the gate).
   - `rh.applyCount == 2` (initial failed C1 + C2 succeeded; retry's Apply DID NOT run because the gate dropped it before Apply).

**Why no barrier is needed:** with pre-Apply serialization, the retry's stale check fires BEFORE its Apply call, so the retry's Apply never runs for `C1` (V=5) once `C2`'s snapshot is stored. The version-keyed barrier from `SetBlockOnVersion` is therefore unused for this test and reserved for future scenarios that need to interleave specific Apply calls.

### Test 5.3 — Idempotent reapply same `(V, LR)` is not stale

**Intent:** prove the gate's lex comparison correctly admits idempotent reapply.

**Mechanism:**
1. `newTestManager(t)`. Apply `A1 = (V=5, LR=10)` — succeeds, snapshot `(V=5, LR=10)`.
2. Re-apply `A1` again via `m.applyAssignment(A1)`.
3. **Assertions:**
   - `m.CurrentAssignment()` is `(V=5, LR=10)`.
   - `rm.staleStoreDropped.Load() == 0` — gate did NOT fire.
   - `rh.applyCount == 2` — both Applies ran (idempotent).

### Test 5.4 — V=0 carve-out: only `V=0 over V=0` admits; `V=0 over V>0` drops

**Intent:** prove the corrected V=0 semantics (plan-review v1 P0-3).

**Mechanism (two phases):**
1. **Phase A — bootstrap idempotent path (admits):**
   - `newTestManager(t)`. Snapshot is `Assignment{}` (V=0).
   - Apply `Assignment{}` (V=0, LR=0).
   - **Assert:** `rh.applyCount == 1` (Apply ran), `rm.staleStoreDropped.Load() == 0`.
2. **Phase B — regression case (drops):**
   - Apply `A1 = (V=10, LR=20)` — succeeds, snapshot `(V=10, LR=20)`.
   - Apply `Assignment{}` (V=0, LR=0) — should be dropped as stale.
   - **Assert:** `m.CurrentAssignment()` is `(V=10, LR=20)`, `rm.staleStoreDropped.Load() == 1`, `rh.applyCount == 2` (Phase A's bootstrap apply + A1; the V=0 candidate's Apply did NOT run).

### Test 5.5 — Gate does NOT fire on Apply failure

**Intent:** prove the stale-gate metric is reserved for "candidate was stale" cases; Apply failures schedule retry without firing the metric.

**Mechanism:**
1. `newTestManager(t)`. Snapshot is `Assignment{}` (V=0).
2. Set `rh.errOnce` to return an error on the next Apply call.
3. `m.applyAssignment(A1)` where `A1 = (V=10, LR=20)` — gate admits (candidate is fresh), Apply runs, returns error. `scheduleApplyRetry` arms.
4. **Assertions:**
   - `rm.staleStoreDropped.Load() == 0` — gate did NOT fire (the candidate was fresh; only Apply failed).
   - `rh.applyCount == 1` — the failed Apply call still counts.
   - `m.CurrentAssignment()` is still `Assignment{}` (Store never ran).

This proves the gate's semantics: fires only on stale candidates, not on Apply errors.

### Test 5.6 — `refreshAssignmentFromNATS` honors the gate

**Intent:** prove the refresh path is now serialized and obeys the (V, LR) ordering.

**Mechanism:**
1. `newTestManager(t)`. `m.assignmentKV` wired to a real embedded NATS KV (use `partitest.StartEmbeddedNATS` + `partitest.CreateJetStreamKV`).
2. Plant alias `(V=5, LR=10)` in KV at `assignment.<workerID>`.
3. Call `m.refreshAssignmentFromNATS()`.
4. **Assert:** `m.CurrentAssignment()` is `(V=5, LR=10)`, `m.lastSeenLeaderRevision.Load() == 10`, `rm.staleStoreDropped.Load() == 0`.
5. Apply `A2 = (V=10, LR=20)` via `m.applyAssignment(A2)` — succeeds, snapshot `(V=10, LR=20)`.
6. Overwrite KV value to `(V=5, LR=11)` (something staler in V dimension but fresher in LR).
7. Call `m.refreshAssignmentFromNATS()` again.
8. **Assert:** snapshot still `(V=10, LR=20)` (refresh dropped as stale), `rm.staleStoreDropped.Load() == 1`.

This proves both: (a) refresh admits a fresh candidate and advances LSR, (b) refresh drops a stale candidate via the same gate.

### Acceptance

All six tests pass. Test 5.1 (especially the reverse-ordering variant) and Test 5.2 are the load-bearing W15/W16 coverage. Test 5.4 proves the V=0 carve-out is now safe. Test 5.6 proves the refresh-path correctness.

---

## 6. Risks and edge cases

### 6.1 Production `m.assignment.Store` sites and their concurrency model

Per plan-review v1 P1-1 and v2 P2-A, every production Store site must be accounted for:

| Site | File:line | Concurrency | Reason it is safe (or PR-2's protection) |
|---|---|---|---|
| `NewManager` initialization | `manager.go:297` | None — no monitors started yet | Pre-Start lifecycle |
| `waitForAssignment` observational | `manager_election.go:320` | None — runs in `Manager.Start` BEFORE `monitorCommitChanges` / `monitorAssignmentChanges` are spawned (`manager.go:446-484`) | Pre-Start lifecycle |
| Cold-bootstrap empty | `manager.go:551` | None — same pre-Start window | Pre-Start lifecycle |
| `applyAssignmentWithPrev` success | `manager_assignment.go:817` | Concurrent with watcher + retry + refresh | **Serialized under `applyStoreMu`** |
| `refreshAssignmentFromNATS` | `manager_assignment.go:970` | Concurrent with apply pipeline (called from `manager_degraded.go:235`) | **Serialized under `applyStoreMu` via `monotonicStore` helper** |

All five sites are now explicit. The pre-Start lifecycle Stores (sites 1, 2, 3) execute in `Manager.Start` before any watcher or retry goroutine is spawned. They cannot race with the apply pipeline.

**Test-only direct Stores.** Per plan-review v2 P2-A, the following test files also call `m.assignment.Store` directly to seed test state. They intentionally bypass the gate because the manager is not running in those tests (no goroutines spawned). They are NOT a concurrency concern:

- `manager_commit_state_machine_test.go:160, 176`
- `manager_hook_tracking_test.go:39`
- `manager_stop_test.go:50`

Future test files MAY do the same for state-seeding purposes; the rule is "only when the manager has no active goroutines."

### 6.2 Lock-ordering: `applyStoreMu` vs `m.mu` vs `pendingApplyInFlight`

| Pair | Order rule | Verification |
|---|---|---|
| `applyStoreMu` vs `m.mu` | **No combined acquisition** — callers MUST NOT hold `m.mu` while entering the apply pipeline (or vice versa). The two locks protect disjoint state; they should never be held together. | `grep` for callers of `applyAssignment`, `applyAssignmentWithPrev`, `monotonicStore` — confirm none hold `m.mu`. |
| `applyStoreMu` vs `pendingApplyInFlight` | `pendingApplyInFlight` is set BEFORE acquiring `applyStoreMu` (commit case-(e) path: line 590 CAS, then line 618 `applyAssignment` which inside calls `applyStoreMu.Lock`). Released AFTER `applyStoreMu` is released (line 629, after `applyAssignment` returned). | No deadlock: alias and retry paths never touch `pendingApplyInFlight`. Commit-from-commit re-entry is prevented by case-(e) coalescing (CAS fail → stash → return). |
| `applyStoreMu` vs `m.handoffCoordinator`'s internal locks | `handoffCoordinator.Apply` does not call back into the Manager (one-way dispatch). | Confirmed by `grep` of `internal/assignment/handoff/`. |
| `applyStoreMu` vs `m.heartbeat.appliedMu` | `SetAppliedAssignment` runs INSIDE `applyStoreMu` (§3.2 fix for plan-review v2 P0-B). The publisher's `appliedMu` is a leaf lock with no callback into the Manager (`internal/heartbeat/publisher.go:178-195`), so this order is acyclic. `PublishNow` runs OUTSIDE `applyStoreMu`. | See §3.2 design and `internal/heartbeat/publisher.go`. |

### 6.3 Apply-retry: stale retry under sustained churn

If the apply pipeline is under churn and the retry path repeatedly fires stale candidates, the metric `RecordStaleSnapshotStoreDropped` will increment each time. This is benign — the snapshot is always at the freshest version, just the retry stash repeatedly hits the gate. Operators should treat a persistently rising counter under no churn as a signal that the retry stash is misconfigured (e.g., retries firing faster than watcher cadence).

A pathological case: the retry stash holds `(V=5)`, snapshot is at `(V=10)`. Retry fires, drops. Stash is now empty (the retry goroutine cleared it via `Swap(nil)` at line 924). No more retries fire. Steady state: counter is at 1, snapshot stays at V=10. Correct.

### 6.4 Heartbeat ack ordering — `SetAppliedAssignment` INSIDE the lock, `PublishNow` outside

`heartbeat.SetAppliedAssignment` is called INSIDE `applyStoreMu` (§3.2 fix for plan-review v2 P0-B). The publisher's monotone invariant is V-only (`internal/heartbeat/publisher.go:170-195`): it rejects a lower `AppliedVersion` but ACCEPTS an equal-V call and overwrites `LeaderRevision`. Out-of-order Ack at same V with lower LR would therefore regress the heartbeat snapshot. Holding `applyStoreMu` across `SetAppliedAssignment` eliminates this: a slow apply path cannot post its Ack after a winner has stored, because the winner's `applyStoreMu` window also covered SetAppliedAssignment.

`heartbeat.PublishNow` is called AFTER `applyStoreMu.Unlock`. This is safe because:

- `SetAppliedAssignment` has already published the winning snapshot to the publisher's in-memory state, under the lock.
- `PublishNow` only triggers the publisher to send the current state to NATS. If two `PublishNow` calls race (one from the loser apply's defer, one from the winner's), the NATS heartbeat stream may see two messages — but both reflect the publisher's then-current `(V, LR)`, which is the winner's. No regression.
- Holding `applyStoreMu` across `PublishNow` would serialize network IO on the apply hot path. The publisher already coalesces ticks; serializing here would only inflate apply latency.

**Edge case retired.** The v2 spec described an "out-of-order Ack" race scenario that depended on Ack-outside-lock. With v3's inside-lock fix, the scenario is unreachable: any Ack the publisher sees comes from the goroutine that won the `applyStoreMu` race.

### 6.5 Hook reentry

Hook callbacks (`OnAssignmentChanged`, `OnPartitionsAssigned`, `OnPartitionsRevoked`) run via `invokeHook` in a separate goroutine (`m.wg.Go`). Even if a hook were to call back into `applyAssignment` (it shouldn't), it would acquire `applyStoreMu` in a new goroutine — no self-deadlock. Documented as a contract: "Hooks MUST NOT call back into the Manager's apply pipeline."

### 6.6 Test seam `testHookAfterApplyStore` runs INSIDE the lock

The test hook at line 818 fires immediately after `m.assignment.Store(newAssignment)`. With PR-2, it runs INSIDE `applyStoreMu`. Existing test usage assumes the hook can call `m.CurrentAssignment()`, which is lock-free — still works. If a future test uses this hook to call `applyAssignment` or `refreshAssignmentFromNATS`, the test would self-deadlock. Document the constraint in the hook's Godoc.

### 6.7 Test fixture `recordingHandoff` extension — production-side safety

The new `blockOnVersion` mechanism on `recordingHandoff` is test-only. It adds three atomic fields plus a `SetBlockOnVersion` method. No production type changes. The fixture lives in `manager_commit_state_machine_test.go` (test-only file). No risk to production code.

---

## 7. Acceptance criteria

1. **All new tests pass** under `go test ./... -count=1 -race`:
   - Test 5.1 (W15 cross-path, both forward and reverse-ordering)
   - Test 5.2 (W16 apply-retry)
   - Test 5.3 (idempotent reapply admitted)
   - Test 5.4 (V=0 carve-out: admit V=0/V=0, drop V=0/V>0)
   - Test 5.5 (Apply failure does NOT fire the gate)
   - Test 5.6 (refresh path honors the gate)
2. **Existing tests unchanged or pass without modification** — `manager_commit_state_machine_test.go`, `manager_assignment_watcher_test.go`, `manager_assignment_fixes_test.go`, `manager_commit_watcher_test.go`, `manager_rolling_upgrade_test.go`. If any breaks, investigate before patching.
3. **Full test suite passes** — `make test` green; `go vet ./...` and `make lint` clean.
4. **Metric wired** — `RecordStaleSnapshotStoreDropped` declared in `types/metrics_collector.go`, implemented as no-op in `internal/metrics/nop.go`, recorded in the test fixture, called from both `applyAssignmentWithPrev` and `monotonicStore`.
5. **Lock contracts documented** — Godoc on `applyStoreMu` enumerates the lock-ordering rules from §6.2.
6. **PR description explicitly notes:**
   - Closes W15 and W16 from `00-fix-plan.md`.
   - Documents the change in V=0 semantics (no longer admits V=0 over a real snapshot).
   - Notes `refreshAssignmentFromNATS` now advances LSR consistently with its Store.
   - Lists the lock-ordering rules.
7. `/post-impl-review` (Codex `xhigh` for v1) returns a MERGE verdict.

---

## 8. Out of scope (explicitly NOT in this PR)

| Item | Why deferred |
|---|---|
| Convert `m.assignment` to `atomic.Pointer[Assignment]` and use CAS instead of mutex | Wider refactor, no correctness benefit. Mutex is the smaller correct primitive. |
| Repurpose `pendingApplyInFlight` as the cross-path interlock | Would entangle case-(e) coalescing with PR-2's serialization. Both can coexist as independent primitives. |
| Add `releaseClaims` cleanup hooks for any orphaned coordinator state | Under pre-Apply serialization, loser Apply does not run — no orphan claims to release. |
| Change `selectAuthority` semantics | Orthogonal. Selector is correct; this PR addresses the apply pipeline race. |
| `handoffCoordinator.Apply` internal serialization | Concurrent Applies cannot happen under PR-2 (the mutex serializes the callers). The coordinator's lack of internal mutex is now unobservable. |
| W17, W18, ... (future watcher hardening) | Tracked separately in `00-fix-plan.md`. |
| Persist `lastSeenLeaderRevision` across restarts | Separate concern; see `00-fix-plan.md` "Doc" tier. |

---

## 9. Implementation order

Suggested sequence to minimize review surface:

1. **Step 1: Add the metric.** Declare `RecordStaleSnapshotStoreDropped` on the metrics interface, implement on Nop, extend the test fixture. Independently committable.
2. **Step 2: Add `applyStoreMu` field + `isApplyResultStale` helper + `monotonicStore` helper.** No callers wired yet. Compiles cleanly. Independently committable.
3. **Step 3: Wire `applyAssignmentWithPrev` to use `applyStoreMu` and the helper.** Single commit. Run full suite — existing tests must still pass.
4. **Step 4: Wire `refreshAssignmentFromNATS` to use `monotonicStore`.** Single commit. Run full suite.
5. **Step 5: Extend `recordingHandoff` with `SetBlockOnVersion`.** Test-only change. Verify existing tests using `blockFirstApply` still pass.
6. **Step 6: Add Tests 5.1 (both variants), 5.2, 5.3, 5.4, 5.5, 5.6.** Can be one commit or split — recommend splitting if any single test surfaces unexpected interactions.
7. **Step 7: Full suite under `-race`.** Address any newly-surfaced flakes.
8. **Step 8: Dispatch `/post-impl-review`.**

---

## 10. Known limitations NOT addressed by PR-2

### 10.1 The two-phase coordinator's `Apply` lacks internal serialization

`twoPhaseCoordinator.Apply` (`internal/assignment/handoff/twophase.go:57`) has no top-level mutex. PR-2 closes the concurrent-call window at the caller level (only one apply runs at a time inside `applyStoreMu`), so this is now an unreachable hazard. A future refactor of the coordinator could add internal serialization for defense-in-depth; not required for PR-2's correctness.

### 10.2 Heartbeat `PublishNow` outside the lock (not a limitation; design choice)

`PublishNow` runs AFTER `applyStoreMu.Unlock` (§6.4). This is deliberate: the publisher's in-memory snapshot has already been atomically updated INSIDE the lock via `SetAppliedAssignment`, so the publish is guaranteed to reflect the winning `(V, LR)`. Holding `applyStoreMu` across the network call would needlessly serialize NATS publish on the apply hot path. The publisher already coalesces concurrent calls.

If a future telemetry signal shows out-of-order publish latency creating visible operator confusion, the publisher itself can be tightened to lex-monotonic `(V, LR)` — this would address the same concern at the layer where ordering matters.

### 10.3 Hooks can call back into the apply pipeline

Hooks are fired from a separate goroutine (`invokeHook`), so a callback into `applyAssignment` would not self-deadlock. But it could create unexpected reentrancy (an apply triggered from a hook is essentially a recursive apply). Documented as a hook contract; not enforced in code.

### 10.4 Persisted LSR across worker restart

`lastSeenLeaderRevision` is in-memory only. A restarted worker loses its LSR and could accept commits from any leader on cold start. Tracked separately; not addressed by PR-2.

### 10.5 Failed-Apply partial-claim orphans when retry is stale-dropped (plan-review v2 P0-A)

**The concern.** A first Apply can:
1. Pass `applyStoreMu`'s pre-Apply stale gate.
2. Successfully run `preparePhase`, which for partitions with no prior owner calls `NewInitialClaim` (`internal/assignment/handoff/twophase.go:251-264`) — creating a stable claim owned by this worker.
3. Fail in a later phase (consumer update, commit, or stabilize).
4. Return error; `scheduleApplyRetry` arms the retry goroutine (`manager_assignment.go:793-796`).

If a fresher apply lands a snapshot in `applyStoreMu` BEFORE the retry fires, the retry's stale gate drops the retry before re-running Apply. The stable claim from step 2 is orphaned: the worker doesn't process the partition (snapshot doesn't include it) and sweep skips stable claims (`internal/assignment/handoff/twophase.go:458-460`).

**Why this is a pre-existing coordinator concern, NOT introduced by PR-2.** Three pieces of evidence:

1. **Sweep doesn't reclaim stable claims at all** (`twophase.go:458-460`). Even without PR-2, a stable claim owned by a worker whose snapshot doesn't include the partition is orphaned. The current code's only path to reclaiming is "a future leader assigns the partition to a different worker, which causes that worker's prepare to handoff from the orphan owner." This works as long as future leaders include the partition in someone's assignment.
2. **Retry's prepare is idempotent on already-stable claims** (`twophase.go:282-313`, the `cur.Owner == workerID` branches return the claim as-is). Even if the retry's Apply runs to completion, prepare does NOT clean up partitions in the previous failed candidate that are not in the retry's candidate — the loser-partition-set cleanup is a coordinator-design gap that exists today.
3. **Sweep skipping stable expired claims is a pre-existing semantic** (`twophase.go:458-460`). Stable claims have TTLs, but expired stable claims are intentionally left alone (no deletion API exists at this layer per the comment at line 459). Orphans of any kind (failed Apply, worker stop without heartbeat refresh of an unowned partition, source partition deletion) all share the same fate.

**Empirical reachability.** The orphan only matters if **all three** conditions hold:
- The first Apply fails AFTER `preparePhase` succeeded for at least one partition (i.e., between the prepare-phase return and the eventual error in apply/commit/stabilize).
- A fresher snapshot lands and excludes that partition from its assignment.
- No future leader ever re-includes the partition for any worker.

Under steady-state operation with stable partition sets (Kafka topics rarely deleted), the third condition is the common case for blocking observable harm: as long as the partition continues to be assigned to SOMEONE in future commits, the orphan resolves via NextPrepare handoff. The persistent-orphan case is "partition was permanently removed from the source while the failed Apply's claim was stable" — a tail-latency operator concern, not a PR-2 correctness regression.

**What PR-2 changes.** Under current code, a stale retry whose Apply runs would (a) re-acquire stable claims for partitions it still owns (no-op), and (b) potentially fail and re-retry. So retries with persistent failure modes could still hit the same orphan eventually. PR-2's deterministic stale-drop slightly widens the window from "may retry forever" to "definitely drops on first fresher snapshot." This is a SMALL change to the reachability of the orphan, not a NEW orphan class.

**Why deferred from PR-2.**
1. **Pre-existing.** The orphan-claim semantic exists today and is fundamental to the two-phase coordinator's design.
2. **Out of layer.** Closing it requires either (a) coordinator-level cleanup of loser-partition stable claims, OR (b) a "stale retry releases its prepare-claimed partitions" hook driven by the Manager. Both are coordinator-design changes, not apply-pipeline changes.
3. **Larger scope.** A clean fix would extend sweep to optionally reclaim stable claims (with TTL gating to avoid stomping live owners), or change the coordinator to return a list of "claims created" so the caller can release them on Apply error.

**Future-PR follow-up.** A dedicated coordinator-side PR should:
- Either extend sweep to reclaim expired stable claims with stronger TTL semantics, OR
- Have the coordinator return a list of created-during-prepare claims so the caller can `releaseClaims` on Apply error.

Tracked as **W19** in `00-fix-plan.md` (Tier P2 cont.).

**Production telemetry signal.** Operators concerned about this case can monitor:
- The new `RecordStaleSnapshotStoreDropped` counter (an increment indicates a stale-drop event; not all such events leak claims).
- Existing coordinator metrics: `ClaimStoreSize` (`twophase.go:447-449`) trending upward without explanation suggests orphan accumulation.

---

## 11. Model & effort recommendations (from `00-fix-plan.md` §"Per-PR matrix")

| Phase | Tool | Model / effort |
|---|---|---|
| Planning (this spec v2) | Claude Code | **Opus 4.7** — design call between post-Apply revalidation (rejected) vs pre-Apply serialization (chosen); subtle invariants around `applyStoreMu` lock-ordering and the V=0 carve-out |
| Plan review (v1) | `/plan-review` | Copilot `gpt-5.5 xhigh` (Codex sandbox env-fail; ran via fallback) — DONE, 3 P0 / 3 P1 / 2 P2 |
| Plan review (v2) | `/plan-review` | Codex **xhigh** (or Copilot `gpt-5.5 xhigh` fallback) — pending |
| Implementation | Claude Code | **Opus 4.7** — touches the apply pipeline's critical section; lock-ordering and V=0 semantics are correctness-critical |
| Post-impl review (v1) | `/post-impl-review` | Codex **xhigh** |
| Post-impl review (v2+) | `/post-impl-review` | Codex **high** |

Estimated reviewer wall-time: plan-review v2 ~5-10 min; post-impl-review v1 ~10-15 min. PR's overall reviewer budget: ~25 min across the planning loop and ~15 min for post-impl.

---

## 12. Scope changes across versions

| Item | v1 | v2 | v3 (current) |
|---|---|---|---|
| Design choice | Post-Apply read-then-write revalidation | Pre-Apply mutex serialization (`applyStoreMu`) | Same as v2 |
| Atomicity | Non-atomic decide-then-commit | Atomic under `applyStoreMu` | Same as v2 |
| Heartbeat Ack location | Outside lock (V-only monotonicity argument) | Outside lock (incorrect — plan-review v2 P0-B: publisher monotonicity is V-only, not (V, LR) lex) | **Inside lock** — `SetAppliedAssignment` moved under `applyStoreMu`; `PublishNow` stays outside (network IO) |
| Orphaned coordinator claims (loser-before-Apply path) | "Sweep reclaims them" (wrong) | "No loser Apply runs" | Same as v2 |
| Orphaned claims (failed-Apply partial-prepare path) | Not addressed | Not addressed (introduced by plan-review v2 P0-A) | **Documented in §10.5 as pre-existing coordinator-level limitation** with reachability analysis and follow-up plan |
| V=0 carve-out | "candidate.V=0 → never stale" | "V=0 stale unless cur.V=0 too" | Same as v2 |
| `refreshAssignmentFromNATS` | Out of scope | Under `monotonicStore` | Same as v2 |
| `monotonicStore` "Used by" | n/a | Contradictory: listed `applyAssignmentWithPrev` (plan-review v2 P1) | **Refresh-only; explicit "not callable from `applyAssignmentWithPrev` because it must hold the lock across Apply"** |
| Production Store-site enumeration | Incomplete | Five production sites in §6.1 | Same as v2, plus test-only Stores explicitly listed (plan-review v2 P2-A) |
| Test 5.2 barrier | "Second barrier needed" | Gate drops retry before Apply | Same as v2 |
| Test 5.4 V=0 | "V=0 over V=10 admitted" (perpetuates bug) | Phase A admits V=0/V=0; Phase B drops V=0/V>0 | Same as v2 |
| Metric name | `RecordStaleApplyDropped` | `RecordPostApplyStaleStoreDropped` (Godoc still mentioned post-Apply) | **`RecordStaleSnapshotStoreDropped`** with refresh-path-aware Godoc that does not imply Apply ran |
| Test count | 5 | 6 | Same as v2 |
| LOC estimate | ~25 LOC + 150 test LOC | ~50 LOC + 200 test LOC | Same as v2 |
| Lock contracts | Not specified | Explicit Godoc + §6.2 table | Same as v2 |
