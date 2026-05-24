# Migrating: `Manager.Start` returns at `StateWaitingAssignment`

This guide covers the **breaking change to `Manager.Start(ctx)`** introduced
in the upcoming release. Most projects can migrate in 5–15 minutes per
call site. Run `go vet ./...` and your test suite after each section.

**Related:**
- [CHANGELOG.md `[Unreleased]` → Breaking changes](../CHANGELOG.md) — the canonical entry.
- [LIFECYCLE.md](LIFECYCLE.md) — the updated lifecycle / state-machine doc.
- [API_REFERENCE.md](API_REFERENCE.md) — Godoc-equivalent reference for the new `Start` contract.
- [Reference: Hooks](REFERENCE.md) — `OnDegraded` now fires on `startup-timeout`.

---

## Table of contents

1. [What changed](#1-what-changed)
2. [Why](#2-why)
3. [Are you affected?](#3-are-you-affected)
4. [The minimal migration](#4-the-minimal-migration)
5. [Common patterns](#5-common-patterns)
6. [New observable signal: `OnDegraded("startup-timeout")`](#6-new-observable-signal-ondegradedstartup-timeout)
7. [Edge cases](#7-edge-cases)
8. [Verification checklist](#8-verification-checklist)
9. [FAQ](#9-faq)

---

## 1. What changed

**Before:** `Manager.Start(ctx)` blocked until the worker reached `StateStable` — i.e., until the initial partition assignment had been fetched and applied. When `Start` returned `nil`, `mgr.CurrentAssignment()` was guaranteed to be populated.

**After:** `Manager.Start(ctx)` returns once the **synchronous sanity-check phase** succeeds (claim worker ID → ensure KV buckets → participate in election → start heartbeat → start calculator if leader). The initial assignment fetch and apply run in a background goroutine. When `Start` returns `nil`, the state is `StateWaitingAssignment` (or any later valid state if the background runner already raced ahead) and `mgr.CurrentAssignment()` may be empty.

```
                              Old contract (blocking)
    Start(ctx) ─────────────────────────────────────────────▶ returns
              │                                              │
              └─ claim / buckets / election / heartbeat /    │
                 calculator / waitForAssignment /            │
                 applyInitialAssignment / transition Stable ─┘
                                                             │
                                                             ▼
                                            CurrentAssignment() populated



                              New contract (returns early)
    Start(ctx) ──────────────────▶ returns
              │                    │
              └─ claim / buckets / │
                 election /        │
                 heartbeat /       │   background goroutine
                 calculator        │   ─────────────────────────────────▶
                                   │   waitForAssignment / applyInitial /
                                   │   transition Stable
                                   ▼
                          State == WaitingAssignment
                          (or any later valid state)
                          CurrentAssignment() may be empty
                          ─────────────────────────────▶ WaitState(StateStable, ...) ─▶ ready
```

**Unchanged:**
- `mgr.WorkerID()` is still reliable immediately after `Start` returns.
- `mgr.Stop(ctx)` semantics are unchanged.
- Synchronous-phase failures still return from `Start` as a non-nil error, and the auto-cleanup defer still calls `Stop` on those failures.
- Hook signatures (`OnAssignmentChanged`, `OnPartitionsAssigned/Revoked`, `OnStateChanged`, `OnDegraded`) — no changes.
- `StartupTimeout` still bounds the documented end-to-end startup budget (from `Start` invocation to `StateStable`).

---

## 2. Why

The blocking `Start` made the caller responsible for a duration that the worker fundamentally cannot bound: how long until the leader publishes the initial assignment. In an empty / cold-start cluster the wait is short; in a 30-second `ColdStartWindow` deployment with leader election contention it can approach `StartupTimeout`. Callers ended up either:

1. Setting `StartupTimeout` aggressively low and watching `Start` fail under normal cold-start conditions (false probe failures, pod rotation that masked the real wait).
2. Setting it generously and letting `Start` hold the caller's goroutine for tens of seconds on every cold start.

Splitting the call surfaces the actual contract: the **synchronous phase is bounded** (sanity checks that fail fast), and the **assignment phase is best-effort with delegated recovery** (the assignment watcher, `scheduleApplyRetry`, and the NATS connection monitor handle anything the runner's first attempt fails). A new soft watchdog provides the `OnDegraded("startup-timeout")` signal for probe-driven pod rotation without coupling that signal to the runner's progress.

---

## 3. Are you affected?

**Yes**, you need to migrate, if your code does any of the following immediately after `Start` returns:

- Reads `mgr.CurrentAssignment()` and asserts non-empty.
- Subscribes a consumer that depends on the assignment being applied.
- Starts a load generator, producer, or worker goroutine that assumes the manager is ready.
- Marks a worker "ready" / "healthy" externally (e.g., a non-probe readiness signal).

**No**, you do not need to migrate, if your code:

- Only calls `mgr.Start(ctx)` and `mgr.Stop(ctx)` for lifecycle (no immediate read of `CurrentAssignment()`).
- Uses `WorkerConsumerUpdater` (the manager invokes `UpdateWorkerConsumer` asynchronously regardless of refactor; the updater contract is unchanged).
- Relies on `Hooks.OnAssignmentChanged` to react to assignment updates (hooks fire from the background runner the same way they fired from synchronous `Start`).
- Tests `Start` error paths (`require.Error(t, mgr.Start(ctx))`).
- Tests degraded-mode behavior where the manager intentionally never reaches `StateStable`.

**Quick check:** `grep -rn 'mgr.Start\|manager.Start' --include='*.go' .` then audit each call site against the criteria above.

---

## 4. The minimal migration

Add a `WaitState` block immediately after `Start` wherever you depend on the manager being ready.

**Before:**

```go
if err := mgr.Start(ctx); err != nil {
    return fmt.Errorf("start manager: %w", err)
}
use(mgr.CurrentAssignment())
```

**After:**

```go
if err := mgr.Start(ctx); err != nil {
    return fmt.Errorf("start manager: %w", err)
}
if err := <-mgr.WaitState(parti.StateStable, 30*time.Second); err != nil {
    _ = mgr.Stop(context.Background())
    return fmt.Errorf("manager did not reach StateStable: %w", err)
}
use(mgr.CurrentAssignment())
```

**Key points:**

- `WaitState(state, timeout)` returns a `<-chan error`. The channel always produces exactly one value, then closes. Receive with `<-mgr.WaitState(...)`.
- On `WaitState` timeout, **you own the cleanup**. `Manager.Start`'s auto-cleanup defer only fires on its own non-nil return — once `Start` returned `nil`, any later failure path must call `mgr.Stop(...)` to tear down the running background goroutines (runner, watchdog, heartbeat). Use a short bounded context for `Stop` (e.g., 10 s) so a slow shutdown can't hang your caller.
- Pick a timeout that bounds your readiness expectations. `30 * time.Second` matches the default `StartupTimeout`; if you've configured a longer `StartupTimeout`, increase the `WaitState` timeout to match (otherwise `WaitState` returns timeout while the runner is still happily retrying).

---

## 5. Common patterns

### 5.1 Server / long-running process

```go
mgr, err := parti.NewManager(cfg, js, src, strategy.NewConsistentHash())
if err != nil {
    return err
}
if err := mgr.Start(ctx); err != nil {
    return err
}
if err := <-mgr.WaitState(parti.StateStable, 30*time.Second); err != nil {
    _ = mgr.Stop(context.Background())
    return fmt.Errorf("manager did not reach StateStable: %w", err)
}
defer mgr.Stop(context.Background())

// Now safe to process work.
runApplicationLoop(ctx, mgr)
```

### 5.2 Test helper

```go
func startManager(t *testing.T) *parti.Manager {
    t.Helper()
    mgr, err := parti.NewManager(cfg, js, src, strategy.NewConsistentHash())
    require.NoError(t, err)
    require.NoError(t, mgr.Start(t.Context()))
    if err := <-mgr.WaitState(parti.StateStable, 10*time.Second); err != nil {
        _ = mgr.Stop(context.Background())
        t.Fatalf("manager did not reach StateStable: %v", err)
    }
    t.Cleanup(func() { _ = mgr.Stop(context.Background()) })
    return mgr
}
```

### 5.3 Hook-driven application (no migration needed)

If your application reacts to assignments via `Hooks.OnAssignmentChanged` rather than polling `CurrentAssignment()`, **no migration is needed** — the hook fires from the background runner on the first successful apply, exactly as it fired from synchronous `Start` before. Your hook handler runs whenever the partition slice changes; readiness emerges naturally.

```go
hooks := &parti.Hooks{
    OnAssignmentChanged: func(ctx context.Context, oldPartitions, newPartitions []parti.Partition) error {
        rewireSubscriptions(newPartitions)
        return nil
    },
}
mgr, _ := parti.NewManager(cfg, js, src, strategy.NewConsistentHash(), parti.WithHooks(hooks))
if err := mgr.Start(ctx); err != nil {
    return err
}
// No WaitState needed — the hook will fire when assignment lands.
```

### 5.4 Kubernetes readiness probe

The probe pattern is unchanged: check `mgr.State() == parti.StateStable` in your `/readyz` handler. The new behavior is that the probe may briefly observe `StateWaitingAssignment` after `Start` returns; the probe should treat that as "not ready yet" exactly as it would treat `StateInit` or `StateClaimingID` pre-refactor.

See `examples/degraded-readiness/` for the canonical wiring.

---

## 6. New observable signal: `OnDegraded("startup-timeout")`

If `StartupTimeout` elapses from `Start` invocation without the manager reaching `StateStable`, a soft watchdog fires `enterDegraded` with reason `"startup-timeout"` **exactly once**. The runner keeps retrying — so if a transient outage resolves before the pod is killed, the manager naturally recovers to `StateStable`.

**If you implement `Hooks.OnDegraded`**, you may see a new reason value:

```go
hooks := &parti.Hooks{
    OnDegraded: func(ctx context.Context, reason string) error {
        switch reason {
        case "KV error threshold exceeded": // existing reason — connectivity loss
            metrics.RecordDegraded("kv_errors")
        case "startup-timeout": // NEW reason
            metrics.RecordDegraded("startup_timeout")
        case "bucket-recreated:" + bucketName:
            metrics.RecordDegraded("bucket_recreated")
        case "stream-missing-recovery-exhausted":
            metrics.RecordDegraded("stream_missing")
        default:
            metrics.RecordDegraded("unknown")
        }
        return nil
    },
}
```

A degraded entry from `"startup-timeout"` is **not guaranteed to self-heal while the runner is blocked** inside the apply (`handoffCoordinator.Apply` is unbounded per attempt — same property as pre-refactor `Start`). Once the runner returns or the pod is rotated, normal recovery applies. The intended response is probe-driven pod rotation, which the existing `OnDegraded`-wired readiness probe already does.

---

## 7. Edge cases

### 7.1 Worker draws zero partitions

In a cluster with more workers than partitions, the assignment strategy may give one or more workers an empty partition slice. Under the new contract:

- The worker still reaches `StateStable`.
- `OnAssignmentChanged(ctx, []parti.Partition{}, []parti.Partition{})` fires at least once (leader settling may produce more than one empty fire).
- `OnPartitionsAssigned` and `OnPartitionsRevoked` do **not** fire (empty diff).

This was the same pre-refactor; nothing changed. If your hook implementation handles `len(newPartitions) == 0` with side effects, it will still fire on this path.

### 7.2 Cold-start with empty source

If the partition source is empty at the moment the leader publishes (no partitions to assign), the worker still reaches `StateStable`. The cold-start-empty path at `manager.go` (the `Version=0 && empty` bypass) skips `OnAssignmentChanged` to avoid phantom empty→empty hook fires. This is unchanged from pre-refactor.

### 7.3 `StartupTimeout` semantics

`StartupTimeout` still bounds the total time from `Start` invocation to reaching `StateStable`, but its **enforcement shape changed**. Pre-refactor: `Start` would return `context.DeadlineExceeded` when the budget elapsed. Post-refactor: the watchdog fires `OnDegraded("startup-timeout")`. Set your readiness probe to rotate the pod on `StateDegraded` (or specifically on `reason == "startup-timeout"`) and the operational effect is equivalent.

If you set `StartupTimeout` very low for tests (e.g., 1ms) hoping to trigger the watchdog instantly, note that `StartupTimeout` also bounds the synchronous sanity-phase context — a 1ms timeout will fail the bucket-creation RPC inside `Start` before the watchdog gets a chance to fire. Drive the watchdog directly in unit tests via the unexported `startStartupTimeoutWatchdog` (same-package), or set `StartupTimeout` to a value larger than your sync phase but small enough to elapse before your live `WaitState` budget.

### 7.4 Stop during background startup

Calling `mgr.Stop(ctx)` while the background runner is mid-flight is supported and clean. The runner detects `m.ctx` cancellation via `sleepWithCancel` and exits before the watchdog fires; final state is `StateShutdown`, never lingering in `StateDegraded`. No special handling required.

### 7.5 Apply boundedness — unchanged from pre-refactor

The runner's `applyInitialAssignment` calls `handoffCoordinator.Apply(m.ctx, ...)` which is **unbounded per attempt**. A stuck `WorkerConsumerUpdater` can block the runner inside one apply attempt until `Stop` is called. This is identical to pre-refactor `Start` (which called the same chain). The soft watchdog fires `OnDegraded("startup-timeout")` regardless, providing the probe-rotation signal even when the runner is blocked.

---

## 8. Verification checklist

After migrating, verify with these steps:

- [ ] **Grep for unmigrated call sites:**
  ```bash
  grep -rn 'mgr\.Start\|manager\.Start' --include='*.go' .
  ```
  For each result, check whether the next line reads `CurrentAssignment()` or any state-dependent value.

- [ ] **Run `go vet ./...`.** Catches the most common mistake: forgetting the `<-` on `WaitState`'s channel return.

- [ ] **Run your test suite under `-race`.** The migration converts what was a synchronous call into a multi-goroutine handshake; race conditions in your test setup (e.g., reading shared state from a hook before the test's main goroutine waited) will surface here.

- [ ] **Run `make test-integration` (or your equivalent).** Verifies the full Start → WaitState → ready handshake works against a live NATS cluster.

- [ ] **Smoke-test the cold-start path.** Start a single worker against an empty cluster and confirm it reaches `StateStable` within your expected budget. If it doesn't, check whether `StartupTimeout` is large enough to cover `ColdStartWindow + ElectionTimeout + (assignment publish latency)`.

- [ ] **Smoke-test the probe-driven rotation path.** Set `StartupTimeout` artificially low against a real cluster (e.g., 2s if your cold-start window is 30s). Confirm `OnDegraded` fires with `reason == "startup-timeout"` and your readiness probe rotates the pod.

- [ ] **If you implement `Hooks.OnDegraded`, add a case for `"startup-timeout"`** (or fall through to a default branch that records the reason in metrics).

---

## 9. FAQ

**Q: Will my pre-refactor code keep working if I don't add `WaitState`?**

A: It will *compile* and *start*. The runtime symptoms depend on what your code does after `Start`:

- Read `CurrentAssignment().Partitions` immediately → may get an empty slice (was guaranteed non-empty before).
- Start a producer that publishes to subjects assuming the worker's consumer subscription is live → may produce messages that no consumer is yet pulling.
- Run a `for { state := mgr.State(); if state == StateStable { break }; time.Sleep(...) }` polling loop → still works, but `WaitState` is the idiomatic replacement.

**Q: Why a breaking change instead of an opt-in flag?**

A: The blocking behavior was hiding correctness issues (false probe timeouts on cold start, false `StartupTimeout` failures during transient leader churn) that an opt-in flag wouldn't have surfaced. The migration is small, mechanical, and covered by the verification checklist above.

**Q: What's the migration cost for a project with many call sites?**

A: For a project with 20–30 `Start` call sites, expect 1–2 hours of mechanical work plus a test-suite run. The pattern is identical at every site; once you've migrated the first 3–4, the rest are copy-paste.

**Q: Does this change affect rolling upgrade from v2.4.x?**

A: No. The change is in the **worker startup path**, which is internal to each pod. Pods running the old and new code coexist normally — they speak the same wire format for assignments, heartbeats, and capability advertising. Roll one pod at a time exactly as you do today.

**Q: Is the documented "Apply boundedness" issue a regression?**

A: No — pre-refactor `Start` had the exact same property. The runner calls `handoffCoordinator.Apply(m.ctx, ...)` which can be blocked by a stuck `WorkerConsumerUpdater`; this was true before the refactor too. The plan tracked threading a per-attempt context through the handoff coordinator as a follow-up; until that lands, the watchdog provides the probe-rotation signal.

**Q: I want to delete this whole refactor and go back to the blocking `Start`.**

A: That's not supported, but the migration shape is symmetric — if you must wrap `Start` to look blocking, write a thin helper:

```go
func StartAndWait(mgr *parti.Manager, ctx context.Context, timeout time.Duration) error {
    if err := mgr.Start(ctx); err != nil {
        return err
    }
    if err := <-mgr.WaitState(parti.StateStable, timeout); err != nil {
        _ = mgr.Stop(context.Background())
        return fmt.Errorf("manager did not reach StateStable: %w", err)
    }
    return nil
}
```

This matches the migration pattern at every call site with zero further changes.
