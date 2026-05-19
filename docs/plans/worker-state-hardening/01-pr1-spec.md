# PR-1 Implementation Spec — Legacy Alias Watcher Rewatch + Reconcile (W12)

Implements **W12** from [`00-fix-plan.md`](./00-fix-plan.md).

The legacy assignment-alias watcher (`watchAssignment` + `monitorAssignmentChanges` in `manager_assignment.go`) exits permanently when its `Updates()` channel closes. There is no rewatch, no periodic reconcile. v2.x explicitly supports rolling upgrade from v2.3.0 via `assignment.<W>` aliases, and the authority selector chooses between commit and alias by `LeaderRevision` (`manager_select_authority.go:33-66`, `CHANGELOG.md:116-128, 144-151`). A worker mid-upgrade can miss a fresher alias if its watcher closes — silently, until restart.

**Resolution: mirror `monitorCommitChanges`/`watchCommit` exactly.** The pattern is already in the file 60 lines below the broken watcher and is correctness-tested.

**Revision history:**
- v1 — initial draft against audit v3.
- v2 — first revision against Codex v1 review (added an in-flight identity gate to address what was thought to be a P0 race).
- v3 (this revision) — address Codex v2 review `tmp/01-pr1-spec_pr1-impl-spec_v2_review.md`. **Significant scope narrowing.** Codex's v2 review established that v1's P0 was based on a structurally impossible interleaving: the watcher and reconcile arms share a single goroutine (`manager_assignment.go:331-348`, the proposed §3.1 select-loop), so they cannot race for the same alias. v2's `inFlightAlias` gate was defending against an impossible production scenario. v3 drops it entirely. v2's review also surfaced two **pre-existing latent races** (blocked-commit cross-path stale-store; `scheduleApplyRetry` concurrent with new applies) that are unrelated to W12's watcher-recovery scope; v3 documents them in §10 and defers them to future PRs (W15, W16 in `00-fix-plan.md`).
  - Dropped: §3.4 alias-specific in-flight identity gate. The race it solved doesn't exist in the single-goroutine watcher loop.
  - Dropped: Test 5.3 (same-alias in-flight dedup) and Test 5.5 (concurrent commit + alias). Test 5.3's "fail-before-pass" step was structurally impossible to reproduce through the production loop. Test 5.5 exposed a pre-existing cross-path race that is W15, not PR-1's concern.
  - Renumbered: Tests 5.4→5.3, 5.6→5.4, 5.7→5.5.
  - Added: §10 "Known pre-existing races NOT addressed by PR-1" — documents W15 and W16 with file:line evidence so a future reader sees what this PR deliberately did NOT fix.
  - LOC estimate restored from v2's ~45 LOC + 7 tests back to ~30 LOC + 5 tests, matching v1.
  - v2's other changes (kept in v3): rename target verified to `manager_commit_watcher_test.go:232-252` not `export_test.go`; required update to `TestMonitorAssignmentChanges_RetriesAfterWatcherFailure` (`manager_assignment_fixes_test.go:111-179`); Test 5.2 rewritten to use open-but-silent watcher (mirrors `manager_commit_watcher_test.go:232-301`); `errors.Is(err, jetstream.ErrKeyNotFound)` sentinel; bootstrap-race rationale corrected to startup ordering at `manager.go:446-484`.

---

## 1. Anchors (verified 2026-05-19 against HEAD `e166a84`)

| Anchor | File:line | Status |
|---|---|---|
| Legacy alias monitor (retry loop) | `manager_assignment.go:284-310` | **rewritten** |
| Legacy alias watch session | `manager_assignment.go:313-350` | **rewritten** |
| Legacy alias handler (idempotent) | `manager_assignment.go:352-356` (delete) + downstream | **reused** — already idempotent via `m.handleAssignmentEntry` |
| Commit watcher retry loop (template) | `manager_assignment.go:402-432` | **reference only — not modified** |
| Commit watch session (template) | `manager_assignment.go:435-470` | **reference only — not modified** |
| `commitReconcileInterval` (`var`, 30s default) | `manager_assignment.go:36` | **shared by both watchers** (commit + alias) — see §2 |
| `watcherBaseBackoff`/`watcherMaxBackoff`/`watcherJitter` constants | `manager_assignment.go:21-23` | **reused** |
| Authority selector | `manager_select_authority.go:33-66` | reused — confirms alias path is still load-bearing |
| `recordKVError` (degraded-circuit feed) | `manager_degraded.go` (via `m.recordKVError` symbol) | reused — matches commit watcher's degraded-feed pattern |
| Alias path stale-leader fence | `manager_assignment.go:363-369` | existing — rejects `LR < lastSeen`; sufficient under single-goroutine watcher loop |
| Alias path version fence | `manager_assignment.go:371-374` | existing — rejects `oldVersion >= newVersion` against `CurrentAssignment()` |
| Apply path monotonicity gate | `manager_assignment.go:728-743` | existing — strict `<` version-only check; **see §10 for known limitation under cross-path blocked-apply** |
| Commit-only in-flight gate | `manager_assignment.go:553-565`; manager struct doc `manager.go:118-123` | reference only — informs §10's W15 description |
| `scheduleApplyRetry` separate goroutine | `manager_assignment.go:849-908` | reference only — informs §10's W16 description |
| Commit-watcher silent-stall reconcile test (template for Test 5.2) | `manager_commit_watcher_test.go:232-301` | reused as test shape template |
| `droppingWatcherKV` test double (open-but-silent watcher) | `manager_commit_watcher_test.go:303-324` (per Codex review) | reused for Test 5.2 |
| Test override site for the reconcile interval (NOT in `export_test.go`) | `manager_commit_watcher_test.go:232-252` | rename target — see §3.3 |
| Conflicting existing test (currently asserts close = clean exit) | `manager_assignment_fixes_test.go:111-179` | **must be updated by this PR** — see §3.4 |
| `kvutil.GetJSON` typed-get used by commit watcher | `kvutil/ops.go:24-31` | reference for typed deletion handling on Get |
| `jetstream.ErrKeyNotFound` sentinel | nats.go `jetstream/errors.go` | the correct sentinel for a missing single key (NOT `ErrNoKeysFound`) |

Verified against current branch `main` @ `e166a84`. Spec author MUST re-verify line numbers immediately before implementing if HEAD has advanced.

---

## 2. Design — single shared reconcile cadence, separate watch sessions

The commit watcher and the alias watcher need the same reconcile cadence (idempotent KV re-read every 30s). Two options:

**Option A — share `commitReconcileInterval` (recommended).** Rename to `watcherReconcileInterval` (or keep the existing var as the shared value). Both watchers create independent `time.NewTicker` instances from the same period. No new constant. The existing test override at `manager_commit_watcher_test.go:232-252` drives both paths after rename.

**Option B — separate `aliasReconcileInterval` var.** Slightly clearer intent but doubles the test-override surface for no semantic benefit.

**Decision:** Option A. Rename `commitReconcileInterval` → `watcherReconcileInterval` in a single mechanical pass. Update the test override at `manager_commit_watcher_test.go:232-252` to use the new identifier; see §3.3 for the full rename target list.

If the rename triggers risk concerns (downstream test churn), fall back to Option B; the implementation downstream is identical.

---

## 3. Implementation

### 3.1 Rewrite `watchAssignment` (`manager_assignment.go:313-350`)

**Current shape:**

```go
func (m *Manager) watchAssignment(ctx context.Context, kv jetstream.KeyValue) error {
    workerID := m.WorkerID()
    key := fmt.Sprintf("assignment.%s", workerID)

    watcher, err := kv.Watch(ctx, key)
    if err != nil {
        return fmt.Errorf("failed to watch assignments: %w", err)
    }

    defer func() {
        if err := watcher.Stop(); err != nil && !natsutil.IsConsumerNotFound(err) {
            m.logError("failed to stop watcher", "error", err)
        }
    }()

    for {
        select {
        case <-ctx.Done():
            m.logger.Debug("assignment monitor stopping (context cancelled)", "worker_id", workerID)
            return nil
        case entry, ok := <-watcher.Updates():
            if !ok {
                m.logger.Debug("assignment watcher closed", "worker_id", workerID)
                return nil   // <-- this is the bug surface
            }
            if entry == nil {
                continue
            }
            m.handleAssignmentEntry(workerID, entry)
        }
    }
}
```

**Target shape (mirror of `watchCommit`):**

```go
// watchAssignment runs one watch session on this worker's assignment key.
// Channel closure is returned as an error so monitorAssignmentChanges can
// restart with backoff; context cancellation returns nil for clean exit.
//
// A periodic reconcile tick re-reads the alias idempotently to recover from
// missed watcher events (channel close gaps, NATS reconnects). The reconcile
// cadence is shared with the commit watcher (see watcherReconcileInterval).
func (m *Manager) watchAssignment(ctx context.Context, kv jetstream.KeyValue, reconcileTickC <-chan time.Time) error {
    workerID := m.WorkerID()
    key := fmt.Sprintf("assignment.%s", workerID)

    watcher, err := kv.Watch(ctx, key)
    if err != nil {
        return fmt.Errorf("failed to watch assignments: %w", err)
    }

    defer func() {
        if serr := watcher.Stop(); serr != nil && !natsutil.IsConsumerNotFound(serr) {
            m.logError("failed to stop assignment watcher", "error", serr)
        }
    }()

    for {
        select {
        case <-ctx.Done():
            m.logger.Debug("assignment monitor stopping (context cancelled)", "worker_id", workerID)
            return nil
        case entry, ok := <-watcher.Updates():
            if !ok {
                return errors.New("assignment watcher channel closed")
            }
            if entry == nil {
                continue
            }
            m.handleAssignmentEntry(workerID, entry)
        case <-reconcileTickC:
            // Idempotent re-read. Safe because this select loop is
            // single-goroutine — the reconcile arm cannot run while the
            // watcher arm is mid-apply, and vice versa. Once an apply
            // completes, the version fence at handleAssignmentEntry
            // (manager_assignment.go:371-374) rejects a re-read of the
            // same alias. See §4.
            current, err := kv.Get(ctx, key)
            if err != nil {
                if errors.Is(err, jetstream.ErrKeyNotFound) {
                    // Alias was deleted (or never existed). This is
                    // normal at this point in the lifecycle.
                    continue
                }
                // Transient/connectivity error: do NOT call recordKVError
                // here. The commit watcher's symmetric reconcile arm
                // (manager_assignment.go:464-469) also silently skips on
                // any non-nil Get error; calling recordKVError from a
                // 30s-ticker would amplify degraded-mode entry under
                // transient KV stress.
                continue
            }
            m.handleAssignmentEntry(workerID, current)
        }
    }
}
```

Three changes from current:
1. New `reconcileTickC <-chan time.Time` parameter.
2. `return nil` on `!ok` → `return errors.New("assignment watcher channel closed")`.
3. New `case <-reconcileTickC:` arm that re-Gets the alias. Uses `errors.Is(err, jetstream.ErrKeyNotFound)` for the explicit deletion case; all other errors silently continue (symmetric with commit watcher).

Imports added if not already present: `errors`, `github.com/nats-io/nats.go/jetstream`.

### 3.2 Rewrite `monitorAssignmentChanges` (`manager_assignment.go:284-310`)

**Target shape:**

```go
func (m *Manager) monitorAssignmentChanges(ctx context.Context, kv jetstream.KeyValue) {
    backoff := watcherBaseBackoff
    reconcileTicker := time.NewTicker(watcherReconcileInterval)
    defer reconcileTicker.Stop()

    for {
        err := m.watchAssignment(ctx, kv, reconcileTicker.C)
        if err == nil || ctx.Err() != nil {
            return
        }
        m.logError("assignment watcher failed, retrying", "error", err, "backoff", backoff)
        m.recordKVError(err)

        //nolint:gosec // jitter does not require crypto-secure random
        f := rand.Float64()
        low := 1 - watcherJitter
        high := 1 + watcherJitter
        delay := time.Duration(float64(backoff) * (low + f*(high-low)))

        select {
        case <-ctx.Done():
            return
        case <-time.After(delay):
        }

        backoff = min(backoff*2, watcherMaxBackoff)
    }
}
```

Two changes from current:
1. Create the `reconcileTicker` once, outside the retry loop. Pass `reconcileTicker.C` into each `watchAssignment` session. The ticker spans rewatch boundaries — a missed event during a rewatch backoff is still recovered on the next tick.
2. Add `m.recordKVError(err)` before backoff (matches commit watcher pattern at `manager_assignment.go:294, 417`).

### 3.3 Rename `commitReconcileInterval` → `watcherReconcileInterval`

Single mechanical pass. Codex's review verified the rename target list against current code; the override is **not** in `export_test.go` (which only exposes `CalculatorForTest`):

- `manager_assignment.go:29-36` — declaration (rename + update Godoc to reflect shared usage).
- `manager_assignment.go:409` — usage in `monitorCommitChanges`.
- `manager_commit_watcher_test.go:232-252` — actual test-mutable global; the existing test (`TestMonitorCommitChanges_PeriodicReconcile_RecoversDivergence`) saves, overrides, and restores this value. Renamed identifiers must compile or the suite fails.

Verification step before merging the rename: `grep -rn commitReconcileInterval .` returns zero results inside `parti` package code.

Update Godoc:

```go
// watcherReconcileInterval is the period between idempotent KV re-reads
// of the commit key and the legacy alias key. Recovers from missed
// watcher events (channel close gaps, NATS reconnects) without depending
// on the watcher's resync. Shared by monitorCommitChanges and
// monitorAssignmentChanges.
//
// Declared as a package-level var (not a const) so reconcile-timing
// tests can override it (see manager_commit_watcher_test.go:232-252).
// Production callers MUST NOT mutate this value.
var watcherReconcileInterval = 30 * time.Second
```

### 3.4 Required updates to existing tests

Two existing tests intersect the changes in this PR and must be updated.

**3.4.1 `TestMonitorAssignmentChanges_RetriesAfterWatcherFailure`** (`manager_assignment_fixes_test.go:111-179`).
Currently asserts: watcher whose `Updates()` channel is immediately closed → `monitorAssignmentChanges` exits cleanly after the second `Watch` call.

After this PR: channel close returns a non-nil error → `monitorAssignmentChanges` rewatches indefinitely (until ctx cancel). The test's "exits after second close" expectation is wrong under the new behavior.

**Required change.** Rewrite the test to assert:
- After two consecutive channel closes, `monitorAssignmentChanges` has called `Watch` three times (original + two rewatches) — proves the rewatch path is active.
- After ctx cancel, the goroutine exits cleanly.

If the existing fixture is structurally incompatible with the new behavior, replace the test rather than amend it. A name like `TestMonitorAssignmentChanges_RewatchesOnChannelClose` is appropriate.

**3.4.2 `TestMonitorCommitChanges_PeriodicReconcile_RecoversDivergence`** (`manager_commit_watcher_test.go:232-301`).
Currently saves/restores `commitReconcileInterval`. After §3.3's rename, the identifier must be `watcherReconcileInterval`. Mechanical change; no semantic update needed.

---

## 4. Idempotency contract (verified against current code)

This PR's reconcile arm runs in the SAME goroutine as the watcher arm (§3.1's `select` loop). Codex's v2 review verified — and re-verified against `manager_assignment.go:331-348` — that `handleAssignmentEntry` is called synchronously: while one entry is being processed (including blocking inside `handoffCoordinator.Apply`), the next `select` iteration cannot start. **A watcher event and a reconcile tick for the same alias therefore cannot interleave concurrently within the production loop.**

This is the critical correctness property that lets PR-1 stay narrow. The existing gates on the alias path are sufficient under single-goroutine serialization:

| Gate | Site | What it does | Sufficient under single-goroutine loop? |
|---|---|---|---|
| Stale-leader fence | `manager_assignment.go:363-369` | `if newLR != 0 && newLR < lastSeen → drop` | Yes — once the first apply succeeds and advances LSR, an identical-LR re-read drops (or, with `LR == lastSeen`, falls to the version fence which now rejects because the snapshot advanced). |
| Version fence (entry) | `manager_assignment.go:371-374` | `if oldAssignment.Version >= newAssignment.Version → drop` | Yes — after the first apply completes, `m.CurrentAssignment()` reflects the new version; a re-read of the same alias drops here. |
| Selector | `manager_assignment.go:384-388`, `manager_select_authority.go` | Drops if commit path is fresher | Yes — orthogonal to dedup. |

The reconcile arm's safety story is therefore: **reconcile re-reads an already-applied alias → version fence rejects.** No new gate is needed.

### 4.1 What this PR does NOT solve

Three races exist that the gates above do NOT cover. None of them are introduced by PR-1; all three are pre-existing latent issues. They are documented in **§10** and tracked as W15/W16 in `00-fix-plan.md` for separate future PRs:

- Cross-path stale-store: a blocked commit can overwrite a fresher alias because there's no post-`Apply` authority recheck (`manager_assignment.go:745-781`).
- `scheduleApplyRetry` runs in a separate goroutine and calls `applyAssignment` directly (`manager_assignment.go:849-908`), so it CAN race with watcher-driven applies.
- An out-of-band test or future code path that invokes two concurrent `handleAssignmentEntry` calls would also bypass the version fence's snapshot-staleness window.

PR-1 explicitly defers all three. The watcher recovery path delivered by this PR does not make any of them worse: reconcile only adds another single-goroutine source within the same `select` loop.

---

## 5. Tests

Three new tests under `internal/assignment` or `manager_assignment_test.go` (placement matches the existing commit-watcher test surface — see `manager_commit_watcher_test.go`).

### Test 5.1 — Watcher close triggers rewatch with backoff

**Intent:** verify that closing the watcher's `Updates()` channel causes the retry loop to rewatch, instead of exiting permanently.

**Mechanism:** use a test KV double that exposes `CloseWatcher()` to force a channel close. After the close, publish a new alias and assert the worker observes it.

**Acceptance:**
- After watcher close, `monitorAssignmentChanges` does NOT exit (verifiable by goroutine count or by observing the rewatch attempt via a metric).
- After backoff completes (test override of `watcherBaseBackoff` to 10ms is acceptable), a freshly-published alias triggers `handleAssignmentEntry`.
- Goroutine eventually exits cleanly when `ctx.Done()` fires.

### Test 5.2 — Reconcile-only recovery from silent watcher stall (REWRITTEN per Codex P1)

**Intent:** verify that the reconcile arm recovers from a stalled-but-open watcher — the canonical silent-stall case (NATS server restart that doesn't close `Updates()`). v1 of this spec described stopping the watcher, but a closed watcher exercises Test 5.1's rewatch path instead; the silent-stall case requires an open-but-silent watcher.

**Mechanism (mirrors `manager_commit_watcher_test.go:232-301`):**
- Use a watcher double whose `Updates()` channel is OPEN but never delivers any event (the doubles' published-but-not-watcher-delivered shape).
- Override `watcherReconcileInterval` to ~50ms via the renamed package-private var.
- Publish alias A directly to KV (out of band of the watcher).
- Assert: apply count is 0 before the first reconcile tick (no recovery via watcher).
- Wait > 50ms.
- Assert: apply count is 1 (alias A applied via reconcile path).
- Publish alias B (higher `LeaderRevision`) directly to KV.
- Wait > 50ms.
- Assert: apply count is 2 (alias B applied via reconcile path).

**Acceptance:**
- Apply 0 before first reconcile tick.
- Apply 1 within `[watcherReconcileInterval, watcherReconcileInterval + tolerance]` after alias A is published.
- Apply 2 within the same bound after alias B.
- Goroutine exits cleanly on ctx cancel.

This test specifically proves the reconcile arm is load-bearing for silent-stall recovery, which Test 5.1 does not.

### Test 5.3 (optional) — Reconcile fired between monitor start and first watcher replay

**Intent:** verify that a reconcile tick fired immediately after `monitorAssignmentChanges` starts — but BEFORE the watcher's initial replay completes — does not cause incorrect behavior.

**Why this is the right framing (corrected per v1's Codex P2.2).** The previous spec drafts framed this as "before initial bootstrap" but `applyInitialAssignment` does not route through `handleAssignmentEntry` (it goes through commit handling or `applyAssignmentWithPrev` directly per `manager.go:500-571`). The real protection against pre-bootstrap reconcile is startup ordering: `Manager.Start` runs `waitForAssignment` + `applyInitialAssignment` + transitions to `StateStable` BEFORE calling `monitorAssignmentChanges` (`manager.go:446-484`). So the only window for a "reconcile before first watcher event" is between monitor-start and watcher-replay-end, not before bootstrap.

**Mechanism:** start the manager normally. Override reconcile to ~20ms. After bootstrap completes, observe whether the first reconcile tick can fire before the watcher's initial replay returns its first entry. Assert no panic, no duplicate apply, no state regression.

**Acceptance:** the version fence at `manager_assignment.go:371-374` rejects the redundant entry; assignment count is 1.

This test is optional because the single-goroutine watcher loop + version fence make the scenario benign by construction. Include it as belt-and-braces if the implementer suspects an ordering subtlety.

### Test 5.4 — Alias delete reconcile is a no-op

**Intent:** verify that when the leader deletes `assignment.<W>`, a reconcile tick observing `ErrKeyNotFound` does not change the in-memory assignment or fire hooks.

**Mechanism:** apply alias A normally. Delete the KV key. Wait for a reconcile tick. Assert no change to `m.CurrentAssignment()`, no `OnAssignmentChanged` hook firing, no error logged at higher than Debug level.

**Acceptance:** assignment snapshot unchanged; hook count unchanged.

### Test 5.5 — Graceful shutdown with stalled watcher + active reconcile ticker

**Intent:** verify the reconcile ticker stops cleanly when the manager context is cancelled.

**Mechanism:** start `monitorAssignmentChanges` with the silent-stall watcher double (from Test 5.2). Override reconcile to 10ms so the ticker is firing actively. Cancel the manager context. Assert the goroutine exits within a small deadline (e.g., 500ms).

**Acceptance:** goroutine exits; no goroutine leak detected by the test framework.

---

## 6. Risks and edge cases

### 6.1 Reconcile during legitimate alias deletion (sentinel verified)

`handleAssignmentEntry` at `manager_assignment.go:353-356` ignores delete events. If a leader legitimately deletes an alias and the worker hasn't seen the delete (e.g., during a watcher restart gap), the reconcile arm's `kv.Get` returns `jetstream.ErrKeyNotFound` (Codex verified against nats.go v1.50.0). §3.1's `errors.Is(err, jetstream.ErrKeyNotFound)` branch covers this — the worker keeps its last in-memory assignment until a new commit lands. This matches W11's "delete is not a reassignment primitive" semantics and is consistent with the commit watcher's symmetric handling via `kvutil.GetJSON` (which converts `ErrKeyNotFound` to `(nil, 0, nil)`; `kvutil/ops.go:24-31`).

**Do NOT use `ErrNoKeysFound`** — that sentinel is for empty key listings, not missing single keys (nats.go `jetstream/errors.go:360-361`).

### 6.2 KV.Get failures during reconcile

A non-not-found error from `kv.Get` should NOT trigger watcher rewatch — the watcher itself is still healthy. §3.1's silent `continue` is correct. Do not call `m.recordKVError` here: the commit watcher's symmetric reconcile path (`manager_assignment.go:464-469`) also does NOT feed degraded-mode tracking on Get errors. Calling `recordKVError` from a 30s ticker under transient KV stress would amplify into spurious degraded-mode entry.

### 6.3 Rename collision with downstream callers

`commitReconcileInterval` is package-private (lowercase). Rename is mechanical within the `parti` package. Verify with `grep -rn commitReconcileInterval .` before commit. No public API impact. The test override at `manager_commit_watcher_test.go:232-252` must be renamed in the same commit.

### 6.4 Reconcile during initial bootstrap (rationale corrected per Codex P2.2)

**v1's rationale was wrong.** It claimed the bootstrap path already exercises `handleAssignmentEntry`. It does not: `waitForAssignment` calls `fetchAssignment` and stores the assignment directly (`manager_election.go:305-327`); `applyInitialAssignment` routes through commit handling or `applyAssignmentWithPrev` (`manager.go:500-571`), neither of which goes through `handleAssignmentEntry`.

**The actual protection** is startup ordering: `Manager.Start` waits for `waitForAssignment`, runs `applyInitialAssignment`, transitions to `StateStable`, and only then starts `monitorCommitChanges` and `monitorAssignmentChanges` (`manager.go:446-484`). The reconcile ticker therefore cannot fire until after bootstrap completes.

If a reconcile tick fires BETWEEN monitor-start and the watcher's first replay, it is benign:
- The watcher and reconcile arms share a single goroutine (`manager_assignment.go:331-348` for the current shape; §3.1 for the new shape), so they cannot interleave concurrently.
- After the first apply completes, the version fence (line 372) rejects a same-version re-read.

Test 5.3 (formerly 5.4) is optional belt-and-braces for this exact window.

---

## 7. Acceptance criteria

Before this PR can be considered ready to merge:

1. **All new tests pass** under `go test ./... -count=1 -race`:
   - Test 5.1 (rewatch on close)
   - Test 5.2 (silent-stall reconcile via open-but-silent watcher)
   - Test 5.4 (alias delete reconcile is a no-op)
   - Test 5.5 (graceful shutdown)
   - Test 5.3 if the implementer chose to include it (optional)
2. **Existing tests updated:**
   - `TestMonitorAssignmentChanges_RetriesAfterWatcherFailure` rewritten per §3.4.1.
   - `TestMonitorCommitChanges_PeriodicReconcile_RecoversDivergence` renamed identifier per §3.4.2.
3. **Full test suite passes** — no regressions in `manager_commit_watcher_test.go`, `manager_rolling_upgrade_test.go`, or any test matching `*Apply*` or `*StateMachine*`.
4. `go vet ./...` and the configured linter pass without new warnings.
5. The rename `commitReconcileInterval` → `watcherReconcileInterval` is complete (verify `grep -rn commitReconcileInterval .` returns zero hits inside `parti`).
6. PR description explicitly references §10 — calling out the pre-existing races (W15, W16) that this PR does NOT close — so a future reviewer knows the deferral was deliberate.
7. `/post-impl-review` (Codex `xhigh` for v1) returns a MERGE verdict.

---

## 8. Out of scope (explicitly NOT in this PR)

| Item | Why deferred |
|---|---|
| Audit other watchers (source watcher, claim-resolver watcher) for the same close-no-rewatch shape | Both already have reconcile loops (see audit §4.1 watcher comparison table). Heartbeat watcher is the next candidate — that's **PR-3 (W2+W13)**. |
| Delete-event handling on the alias path (W11) | Doc-only; intentional behavior; ship in the documentation pass. |
| Change `selectAuthority` semantics | Out of scope. The selector is correct; the bug is upstream of it (watcher dying). |
| Persist `lastSeenLeaderRevision` across manager restarts | Separate design discussion. Not blocking. |

---

## 9. Implementation order

Suggested sequence to minimize review surface. Each step is independently committable except where noted.

1. **Step 1: Rename.** `commitReconcileInterval` → `watcherReconcileInterval`. Update `manager_assignment.go` + `manager_commit_watcher_test.go`. Run full suite; verify zero hits for the old identifier and tests still pass.
2. **Step 2: Rewatch on close + update existing test (single commit).** Change `return nil` to `return errors.New(...)` in `watchAssignment`. Simultaneously rewrite `TestMonitorAssignmentChanges_RetriesAfterWatcherFailure` per §3.4.1 to assert rewatch instead of clean exit. Must be in one commit — the test asserts behavior the code change introduces.
3. **Step 3: Add Test 5.1.** Watcher close triggers rewatch. Should pass against Step 2's code.
4. **Step 4: Add the reconcile arm.** Wire `reconcileTickC` through `monitorAssignmentChanges` → `watchAssignment`. Add the `case <-reconcileTickC:` handler with `errors.Is(err, jetstream.ErrKeyNotFound)` short-circuit. Verify nothing breaks.
5. **Step 5: Add Tests 5.2, 5.4, 5.5.** Silent-stall reconcile recovery, delete no-op, graceful shutdown. Optionally add Test 5.3.
6. **Step 6: Full suite under `-race`.** Fix any newly surfaced flakes.
7. **Step 7: Dispatch `/post-impl-review`** against this spec.

If `/post-impl-review` flags an issue, the spec is small enough to amend recent commits.

---

## 10. Known pre-existing races NOT addressed by PR-1

This PR deliberately defers three concurrency concerns. They are documented here so a future reader sees what was considered and intentionally left out of scope. Each is tracked as a separate item in `docs/plans/worker-state-hardening/00-fix-plan.md` for a follow-up PR.

### 10.1 W15 (S2) — Cross-path stale-store: blocked commit overwrites fresher alias

**The race.** A blocked commit (LR=3, V=10) holds the case-(e) `pendingApplyInFlight` flag and is suspended inside `handoffCoordinator.Apply` (`manager_assignment.go:540-553, 582-596, 745`). While blocked, the alias path observes a fresher alias (LR=4, V=10), `selectAuthority` picks the alias, and the alias apply runs to completion — `m.assignment.Store` records LR=4 (`manager_assignment.go:763-781`).

The blocked commit then resumes. Between `handoffCoordinator.Apply` returning and `m.assignment.Store`, there is **no second authority check**. The pre-`Apply` monotonicity gate at `manager_assignment.go:728-743` only rejects `newAssignment.Version < curAssignment.Version` — it does not catch equal `Version` with lower `LeaderRevision`, and it ran against a stale snapshot before the alias-apply happened. So the LR=3 commit can overwrite the LR=4 alias's stored snapshot.

**Why deferred.** This is a **pre-existing latent bug** in the apply path; it exists today without PR-1's changes. PR-1's watcher recovery adds another path through which a fresher alias can be observed (the reconcile arm), but does NOT widen the race window: the cross-path race already exists via concurrent commit-watcher + alias-watcher goroutines.

**Suggested fix shape (for a future PR).** Either (i) post-`Apply` pre-`Store` revalidation that compares the candidate's `(Version, LR)` against `m.CurrentAssignment()` and aborts on regression, or (ii) extend `pendingApplyInFlight` into a shared apply-interlock that coalesces commit and alias applies through a single critical section.

### 10.2 W16 (S3) — `scheduleApplyRetry` concurrent with new applies

**The race.** Failed apply retries run in a separate goroutine via `scheduleApplyRetry` (`manager_assignment.go:849-908`), which calls `applyAssignment` directly and does NOT participate in any of the existing in-flight gates. If a retry of a stale Assignment is in progress while a fresher commit or alias arrives, the retry's stale `Store` can regress the snapshot — same shape as W15 but with a different trigger.

**Why deferred.** Same as W15: pre-existing latent bug, not introduced by PR-1. The retry path was added for resilience under transient apply failures; coordinating it with the main apply path is a separate design question.

**Suggested fix shape.** Whatever mechanism closes W15 should also cover the retry path (e.g., a unified pre-`Store` revalidation applied at every site that calls `m.assignment.Store`).

### 10.3 Contract for any future caller of `handleAssignmentEntry`

**Hard contract.** Any future production code path that calls `handleAssignmentEntry` — or otherwise applies legacy-alias payloads outside the single `watchAssignment` loop introduced by this PR — MUST either:

1. **Serialize with the watcher loop** (e.g., funnel the alias through the same goroutine via a channel), OR
2. **Add post-`Apply` pre-`Store` revalidation** at every site that calls `m.assignment.Store`, OR
3. **Add the v2 `inFlightAlias` identity gate** (dropped from PR-1, documented in §12 scope-changes) which is correct defensive code for any out-of-loop alias source.

**Why this is load-bearing.** §4's idempotency contract relies on the fact that the watcher arm and reconcile arm share one goroutine. Adding a second alias-source goroutine — admin RPC that hot-reloads a worker's alias, test injection, a new lifecycle hook, anything — breaks that invariant. The version fence at `manager_assignment.go:371-374` reads `CurrentAssignment()` (the last-APPLIED snapshot), so under concurrent applies it has a snapshot-staleness window that lets duplicates pass.

A future PR that adds such a source without satisfying one of options 1–3 above will reintroduce v2's same-alias in-flight race AND make the W15 cross-path race more reachable. Treat this as a binding precondition for any PR that adds an alias-source code path.

**Test-injection note.** Unit tests that invoke `handleAssignmentEntry` directly (e.g., to exercise the alias handler in isolation) can violate this contract without production impact, but should still document the violation and use the v2 `inFlightAlias` gate or per-test serialization if they want to mimic production safety.

---

## 11. Model & effort recommendations (from `00-fix-plan.md` §"Per-PR matrix")

| Phase | Tool | Model / effort |
|---|---|---|
| Planning (this spec) | Claude Code | **Opus 4.7** — done |
| Implementation | Claude Code | **Opus 4.7** — rolling-upgrade authority path; need to preserve dual-read selector semantics and the single-goroutine invariant §4 relies on |
| Plan review (pre-impl) | `/plan-review` | Codex **xhigh** (done — v1, v2, v3 reviewed; current spec is post-v3-narrowing) |
| Post-impl review (v1) | `/post-impl-review` | Codex **xhigh** |
| Post-impl review (v2+) | `/post-impl-review` | Codex **high** |

Rationale: PR-1 touches the rolling-upgrade authority path. The implementation surface is small (~30 LOC + 5 tests, pattern-mirror of `monitorCommitChanges`) but the cost of a subtle wrong assumption (incorrect rewatch backoff, breaking the reconcile ticker's idempotency contract, misclassifying `ErrKeyNotFound`) is high because it affects mixed-version cluster authority. Opus 4.7 for implementation; xhigh effort on the first post-impl review pass.

Estimated reviewer wall-time so far: ~25 min across three `/plan-review` rounds (v1 surfaced a P0 that turned out to be structurally impossible; v2 added an in-flight gate; v3 narrowed back and deferred two pre-existing races to PR-2 / W15+W16). Post-impl reviewer wall-time budget: ~10–15 min across 1–2 rounds.

---

## 12. Scope changes across versions

| Item | v1 | v2 | v3 (current) |
|---|---|---|---|
| Idempotency design | "Three gates already exist; verify them" | §3.4 added an in-flight identity gate | **Gate dropped.** v3 rationale: the race the gate addressed is structurally impossible in the single-goroutine watcher loop (Codex v2 P1). §4 now relies on existing version fence + single-goroutine serialization. |
| In-flight identity gate | n/a | New code (~15 LOC) | **Removed.** Documented at §10.3 as the right fix shape IF a future PR introduces an out-of-loop concurrent alias source. |
| Test 5.3 | "Three identical entries" | Strengthened blocked-apply barrier with mandatory fail-before-pass | **Dropped.** Fail-before-pass was structurally impossible to reproduce through the production loop (Codex v2 P1). |
| Test 5.5 | n/a | Concurrent commit + alias `LeaderRevision` ordering | **Dropped.** The race it exposed is W15 (pre-existing cross-path stale-store), not a PR-1 concern. Moved to §10.1. |
| Test renumbering | n/a | Tests 5.1–5.7 | **5.1, 5.2, 5.3 (was 5.4), 5.4 (was 5.6), 5.5 (was 5.7).** |
| Test 5.2 design | "Stop the watcher" (closes channel) | Open-but-silent watcher (mirrors `manager_commit_watcher_test.go:232-301`) | Kept from v2. |
| Existing-test conflict | Not flagged | §3.5.1 rewrite required | **Kept (§3.4.1).** |
| Rename target | "`export_test.go`" (wrong) | `manager_commit_watcher_test.go:232-252` | Kept from v2. |
| Error sentinel | "not found" generic | `errors.Is(err, jetstream.ErrKeyNotFound)` | Kept from v2. |
| Bootstrap-race rationale | "bootstrap exercises `handleAssignmentEntry`" (wrong) | Startup ordering at `manager.go:446-484` | Kept from v2. |
| Cross-path race | Not flagged | Surfaced via Test 5.5 but not addressed in code | **Documented in §10.1 as W15.** Deferred to a separate PR. |
| Apply-retry race | Not flagged | Mentioned in Codex v2 review | **Documented in §10.2 as W16.** Deferred. |
| LOC estimate | ~30 LOC + 2 tests | ~45 LOC + 7 tests | **~30 LOC + 5 tests** (matches v1's original scope). |

