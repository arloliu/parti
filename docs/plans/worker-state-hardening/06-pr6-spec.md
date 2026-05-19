# PR-6 Implementation Spec — Stop Ordering: Early Election Release (W10)

Implements **W10** from [`00-fix-plan.md`](./00-fix-plan.md).

`Manager.Stop` cancels the election renewer via `m.cancel()`, then runs steps
that can be slow (calculator drain, partition source network call) before
calling `ReleaseLeadership`. If those steps exceed the election TTL (10s
default), the lease expires naturally. A second worker acquires leadership via
`Create`; the original worker's eventual `ReleaseLeadership` hits a revision
mismatch. The fix is to release immediately after `m.cancel()`, before any
slow work, with a 2s bounded timeout so Stop never hangs on KV latency.

---

## Revision History

| Version | Date | Change |
|---|---|---|
| v1 | 2026-05-19 | Initial spec + implementation |
| v2 | 2026-05-19 | Close P1 race: add Option B fix (RenewLeadership preserves leader state on ctx errors); update §2, §3, §5 |
| v3 | 2026-05-19 | Close v2 P1+P2: mirror ctx-error exception to RequestLeadership renew path; remove degenerate TestManager_Stop_MonitorRace_ReleaseStillDeletesKey + raceSimElection; add TestNATSElection_RequestLeadership_CancelledRenew_PreservesLeaderStateForRelease |

---

## 1. Anchors (verified 2026-05-19 against HEAD `06133a0`)

| Anchor | File:line | Status |
|---|---|---|
| `Manager.Stop` — `m.cancel()` call site | `manager.go:634` | **modified** — release appended here |
| `Manager.Stop` — `m.mu.Unlock()` after cancel | `manager.go:638` | unchanged; release follows this |
| `Manager.Stop` — existing Step 3 release call | `manager.go:675` | **removed** |
| `releaseLeadershipAfterCalculatorFailure` | `manager_election.go:199` | reference — bounded-release precedent (uses `m.ctx + OperationTimeout`) |
| `ReleaseLeadership` | `internal/election/nats_election.go:212` | reference — idempotent on `ErrNotLeader` |
| `stopCalculator` | `manager_assignment.go:231` | reference — uses `context.Background()` because `m.ctx` is already cancelled; confirms same pattern for Stop |
| `manager_stop_test.go` | `manager_stop_test.go:1-109` | **extended** — `blockingSource` fake + `TestManager_Stop_ReleasesLeadershipBeforeSlowSourceStop` appended |

---

## 2. Design

### 2.1 Early release ordering

One design choice: move `ReleaseLeadership` to immediately after `m.cancel()`
with a 2s bounded timeout.

**Parent context for the bounded call.** After `m.cancel()`, `m.ctx` is
cancelled. Using it as the parent would give an immediately-cancelled context
and make the release a no-op. Use the caller's `ctx` (the `Stop(ctx
context.Context)` parameter) as the parent — the same context the existing
step-3 call uses.

**Timeout value.** A dedicated package-level var `releaseLeadershipTimeout =
2 * time.Second` (well under the 10s election TTL). Not `OperationTimeout`
(which defaults to 10s, matching the TTL — insufficient safety margin).
Test-overridable via the package var.

**Second call at old step-3 site.** Removed entirely. A double-release returns
`ErrNotLeader` which is benign, but the dead code adds noise.

### 2.2 P1 race found in post-impl review v1

After `m.cancel()`, the `monitorLeadership` goroutine is still alive until
`m.wg.Wait()`. Its `select` can pick the ticker branch even when `ctx.Done()`
is ready (Go's `select` is non-deterministic). When it fires:

1. `wasLeader` is `true`.
2. `renewCtx` is derived from `m.ctx` — already cancelled.
3. `RenewLeadership(renewCtx)` calls `kv.Update(renewCtx, ...)` which fails immediately with `context.Canceled`.
4. **Pre-fix**: `RenewLeadership` calls `e.clearLeadership()` on any error, including ctx errors — clears `NATSElection.isLeader`.
5. Stop's subsequent `ReleaseLeadership` sees `isLeader=false`, returns `ErrNotLeader`, skips `kv.Delete`.
6. Leader key remains in KV until TTL expiry — the exact failure mode PR-6 aims to prevent.

### 2.3 Option A (synchronize monitorLeadership exit) — rejected

Adds `electionMonitorDoneCh` to Stop's flow: cancel → wait for monitor exit → release. This appears surgical but **does not fix the race**. By the time the monitor goroutine closes the channel, `e.clearLeadership()` has already been called (step 4 above). The damage to `NATSElection`'s internal state precedes the channel close — Stop's wait unblocks after the state is already cleared. Option A is insufficient alone.

### 2.4 Option C (ForceReleaseLeadership) — rejected

Adding `ForceReleaseLeadership(ctx)` to the election interface that unconditionally calls `kv.Delete` would work but introduces a new exported method surface. Shutdown semantics do not warrant enlarging the public API.

### 2.5 Option B (chosen) — RenewLeadership preserves state on context errors

Modify `NATSElection.RenewLeadership` to skip `e.clearLeadership()` when the error is `context.Canceled` or `context.DeadlineExceeded`. Rationale: a context error means the caller (the manager) is shutting down, not that another worker took the lease. The lease revocation is `Stop`'s job via `ReleaseLeadership`. Real leadership-loss errors (revision mismatch, key-not-found) still clear state.

Trade-offs: ~7 production LOC in `internal/election/nats_election.go`. Narrowest possible fix. Preserves semantics for all callers: a subsequent `ReleaseLeadership` with a valid context will still attempt the delete. A real leadership-loss error on the next tick (if the monitor fires again before ctx.Done() wins) will still clear state correctly.

---

## 3. Implementation

### Option B fix (internal/election/nats_election.go, RenewLeadership)

```go
if err != nil {
    // Only clear on real leadership-loss errors.
    // Context cancellation / deadline means the caller is shutting down,
    // not that the lease was taken. Clearing on ctx error would race with
    // Stop's ReleaseLeadership.
    if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
        e.clearLeadership()
    }
    // ... log ...
    return fmt.Errorf("%w: %w", ErrLeadershipLost, err)
}
```

### Option B mirror (internal/election/nats_election.go, RequestLeadership renew path)

`RequestLeadership` calls `RenewLeadership` when local state already says "this
worker is the leader". The same context-error exception is applied to the error
returned by `RenewLeadership` before deciding whether to call `clearLeadership`:

```go
if isLeader && currentWorkerID == workerID {
    err := e.RenewLeadership(ctx)
    if err == nil {
        return true, nil
    }
    // Context cancellation / deadline: preserve local state for ReleaseLeadership.
    if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
        return false, err
    }
    // Real leadership-loss error: clear and fall through to acquire.
    e.clearLeadership()
}
```

### Early-release shape (manager.go:634-681)

```
m.cancel()
// …m.mu.Unlock()

// Step 1: stopCalculator()
// Step 1.5: connMonitorStop
// Step 1.6: source.Stop(ctx)
// Step 2: heartbeat.Stop()
// Step 3: election.ReleaseLeadership(ctx)   ← slow-path risk
// Step 4: idClaimer.Release(ctx)
```

### Target shape

```
m.cancel()
m.mu.Unlock()

// Release leadership before any slow work.
releaseCtx, releaseCancel := context.WithTimeout(ctx, releaseLeadershipTimeout)
if err := m.election.ReleaseLeadership(releaseCtx); err != nil &&
    !errors.Is(err, election.ErrNotLeader) {
    m.logError("failed to release leadership on stop", "error", err)
    shutdownErr = fmt.Errorf("leadership release failed: %w", err)
}
releaseCancel()

// Step 1: stopCalculator()
// Step 1.5: connMonitorStop
// Step 1.6: source.Stop(ctx)
// Step 2: heartbeat.Stop()
// (Step 3 removed)
// Step 4: idClaimer.Release(ctx)
```

New package-level var (manager.go, alongside existing `var _ = ...`):

```go
// releaseLeadershipTimeout bounds the ReleaseLeadership call in Stop so that
// a slow KV cannot delay shutdown past the election TTL. Must be less than the
// election bucket's TTL (default 10s). Test-overridable.
var releaseLeadershipTimeout = 2 * time.Second
```

Production LOC delta: ~12 lines added in manager.go, ~8 removed ≈ net +4. Option B fix adds ~7 LOC in `internal/election/nats_election.go`. Total production delta: ~+11 LOC.

---

## 4. Behavior summary

### Before PR-6

1. `m.cancel()` — renewer stops receiving ticks; in-flight renewal may finish.
2. `stopCalculator()` — blocks until calculator drains (state transitions + goroutine exits). No wall-clock bound.
3. `source.Stop(ctx)` — network call; no internal bound.
4. `heartbeat.Stop()`.
5. `election.ReleaseLeadership(ctx)` — if elapsed > TTL, key already expired; another worker has taken over.

### After PR-6

1. `m.cancel()` — renewer cancelled.
2. `election.ReleaseLeadership(ctx)` with 2s timeout — key deleted before any slow work. Another worker can acquire leadership immediately via `Create` (normal takeover path, same as TTL expiry).
3. `stopCalculator()`, `source.Stop(ctx)`, `heartbeat.Stop()` — order unchanged.
4. `idClaimer.Release(ctx)` — unchanged.

---

## 5. Tests

### Test 5.1 — `TestManager_Stop_ReleasesLeadershipBeforeSlowSourceStop`

- **Intent:** prove that leadership is released while `source.Stop` is still
  blocking — i.e. the release precedes slow cleanup work.
- **Mechanism:** a `blockingSource` fake whose `Stop` blocks until the test
  closes a gate channel. Note: `stopCalculator` is not used here because it
  type-asserts to `*assignment.Calculator` (which a test fake cannot satisfy);
  `source.Stop` (called at Step 1.6) is the real blocking path.
- **Ordering gate:** assert `releaseCalled` within 500ms of calling
  `mgr.Stop` while the gate is still closed; then unblock the source.
- **File:** `manager_stop_test.go`.

Pre-fix: `releaseCalled` only set at old step 3, after `source.Stop` which
blocks until gate is closed — `Eventually` times out, test fails. Post-fix:
release fires before `source.Stop`, `Eventually` passes while gate is closed.

### Test 5.2 — `TestNATSElection_RenewWithCancelledCtx_PreservesLeaderStateForRelease`

- **Intent:** regression test for Option B fix at the election level.
- **Mechanism:**
  1. Acquire leadership with a real `NATSElection`.
  2. Call `RenewLeadership(cancelledCtx)` — simulates the monitorLeadership race.
  3. Assert `getLeaderState()` still reports `isLeader=true` (not cleared).
  4. Call `ReleaseLeadership(freshCtx)` — assert success.
  5. Verify the KV key is actually deleted.
- **File:** `internal/election/nats_election_test.go`.
- **Failure mode without fix:** step 2 calls `clearLeadership()`, step 3 fails, step 4 returns `ErrNotLeader`, step 5 key still exists.

### Test 5.3 — `TestNATSElection_RequestLeadership_CancelledRenew_PreservesLeaderStateForRelease`

- **Intent:** regression test for the indirect Stop-ordering race: when
  `RequestLeadership`'s internal `RenewLeadership` call fails with a cancelled
  context, local leader state must not be cleared.
- **Mechanism:**
  1. Acquire leadership.
  2. Call `RequestLeadership(cancelledCtx, sameWorkerID, ...)`.
  3. Assert an error is returned.
  4. Assert `getLeaderState()` still reports `isLeader=true`.
  5. Call `ReleaseLeadership(freshCtx)` — assert success and key deleted.
- **File:** `internal/election/nats_election_test.go`.
- **Note:** `TestManager_Stop_MonitorRace_ReleaseStillDeletesKey` and its
  `raceSimElection` mock were removed (v2 P2): the mock implemented fixed
  semantics and could not fail without the production fix. The
  election-level tests (5.2 + 5.3) are the authoritative coverage for
  Option B semantics.

---

## 10. Known limitations

The bounded 2s timeout means that if the KV is completely unavailable at Stop
time, the release call returns a timeout error and the lease expires naturally
after the TTL. This is the same behavior as the pre-fix code and is acceptable:
the election TTL is the ultimate safety net.

---

## 11. Verification checklist

1. `go build ./...`
2. `go vet ./...`
3. `golangci-lint run ./...` — no new warnings.
4. `go test -count=1 -race -run 'TestManager_Stop_' ./`
5. `go test ./... -race -count=1 -timeout 10m`

---

## 12. Model & effort

| Phase | Model / effort |
|---|---|
| Planning (this spec) | Sonnet 4.6 (orchestrator-directed — no design call) |
| Implementation | Sonnet 4.6 direct |
| Plan review | Skipped per `00-fix-plan.md` ("Skip `/plan-review` — too small; ~20 LOC") |
| Post-impl review v1 | Codex **high** |
| Race fix (v2) | Sonnet 4.6 direct |

LOC: ~19 production + ~110 test = ~129 total.
