# PR-4 Implementation Spec — Heartbeat Watcher Rewatch + Bounded Publish (W2+W13)

Implements **W2** and **W13** from [`00-fix-plan.md`](./00-fix-plan.md).

- **W2 (S3):** `WorkerMonitor.processWatcherEvents` returns permanently when `watcher.Updates()` closes (`worker_monitor.go:329-332`). There is no rewatch loop; the fast-path (~100ms) is silently lost until `Stop`/`Start`. Detection degrades to the polling fallback at `hbTTL/2 = 7.5s` (default), a **75× latency gap**. The fix is a pattern-mirror of PR-1's alias-watcher rewatch: split the watcher goroutine into a watcher-session function and a retry loop with exponential backoff + jitter + `recordKVError` feed. The silent-stall case (NATS server restart leaves `Updates()` open but silent) is already covered by the existing polling fallback (`worker_monitor.go:235-247`) — **no new reconcile tick is needed**.

  Additionally, `monitorWorkers` calls `onChangeCb` with the context passed to `Start` (the outer `ctx`), which may have a deadline set by the caller. Inside the callback, `GetActiveWorkers` calls `kv.Keys` with no per-call timeout beyond whatever the outer context provides. A stuck `kv.Keys` blocks the entire polling goroutine. The fix: derive a short per-call timeout context inside `GetActiveWorkers` from the incoming context, capped at `min(ctx.Deadline-now, hbTTL/2)`.

- **W13 (S3):** `publishLoop` at `publisher.go:302-327` creates a `context.WithTimeout(background, 5s)` on every tick and calls `p.publish(ctx)` synchronously. If the `Put` blocks for the full 5s, the publish goroutine is tied up for 5s — any ticker ticks that fired in the meantime are dropped. Under aggressive tuning (`HeartbeatTTL=3s, HeartbeatInterval=1s`), a single 5s `Put` timeout exceeds `HeartbeatTTL`, causing the worker's heartbeat key to expire while the worker is still healthy. Bounding the timeout to `HeartbeatTTL/2` (Option X) reduces the block but does not decouple the loop from KV latency — a 1.5s block on a 1s ticker still drops the next tick, potentially leaving a 3s gap with TTL=3s. The fix is Option Z: run each `publish` call in a goroutine, gate with an `atomic.Bool` to skip the tick if the prior publish is still in flight (log Warn), and drain in-flight work on `Stop` via a `WaitGroup`.

**Revision history:**

| Version | Date | Notes |
|---|---|---|
| v1 | 2026-05-19 | Initial draft. |
| v2 | 2026-05-19 | Close plan-review findings: P1-A (W13 publish timeout capped at `min(5s, HeartbeatTTL/2)`, TTL threaded into Publisher via new `ttl` field + `New` signature); P1-B (keep `processWatcherEvents` name, rewrite body in-place, update `TestWorkerMonitor_ProcessWatcherEvents_ClosedChannel` to assert rewatch counter instead of plain exit); P1-C (Test 5.1 now asserts `Watch` call counter increments, not just callback); P2-A (Test 5.2 uses context-aware fake that returns `ctx.Err()` when deadline fires); P2-B (LOC estimate revised to 70-80). |
| v3 | 2026-05-19 | Close v2 review findings: P1-A (timeout cap rederived from `HeartbeatInterval/2` instead of `HeartbeatTTL/2`; `ttl` field + `New` signature change dropped — no new parameter needed); P1-caller-inventory (all 18 `publisher_test.go` heartbeat-constructor call sites enumerated in §3.4, plus `doc.go:28` comment example — grep returns 19 hits but line 235 is `jetstream.New`, not `heartbeat.New`); P1-§10.2-overclaim (§10.2 rewritten with Interval/2 math); P2-skip-wording (normalized "record a metric" → "Warn log" in §1 intro and §2.2; §3.4 `recordSkippedPublish` now owns the log — loop calls it without a preceding inline Warn); P2-double-log (pseudocode revised: log lives inside `recordSkippedPublish`, not inline in `publishLoop`); P2-B (LOC wording changed to "gross additions … offset by deletions"). |
| v4 | 2026-05-19 | Close v3 review findings: P1-A (Test 5.3 Setup drops stale `ttl = 200ms`; uses only `interval = 50ms`; derived timeout updated to `interval/2 = 25ms`); P1-B (Test 5.3 fake `Put` explicitly ignores `ctx.Done()` and blocks until release — prevents vacuous pass via context-timeout path; test restructured into two phases: skip-path then recovery); P1-C (§10.2 rewritten to honestly acknowledge zero-margin window at TTL floor and recommend `TTL ≥ 3×Interval`; overclaim removed); P2 (v3 revision-history row corrected to 18 `publisher_test.go` call sites; §3.4 table note corrected). |

---

## 1. Anchors (verified 2026-05-19 against HEAD `89d7fa5`)

| Anchor | File:line | Status |
|---|---|---|
| `processWatcherEvents` — exit on `!ok` (bug site) | `internal/assignment/worker_monitor.go:329-332` | **rewritten in-place** — body replaced; function name kept so `TestWorkerMonitor_ProcessWatcherEvents_ClosedChannel` still compiles (test updated to assert rewatch, not plain exit) |
| `startWatcher` — one-shot watcher setup | `internal/assignment/worker_monitor.go:261-284` | **removed**: watcher session logic absorbed into `processWatcherEvents`; retry logic moves to new `monitorWatcherWithRetry` |
| `monitorWorkers` — polling ticker + watcher start | `internal/assignment/worker_monitor.go:227-257` | **modified** — calls `monitorWatcherWithRetry` (goroutine) + polls; `stopWatcher` calls removed from select arms |
| `stopWatcher` | `internal/assignment/worker_monitor.go:288-298` | **reused** — called on `!ok` error path and on clean exit |
| `GetActiveWorkers` — `kv.Keys` with outer context | `internal/assignment/worker_monitor.go:146-176` | **modified** — derive per-call bounded context |
| `hbTTL` field on `WorkerMonitor` | `internal/assignment/worker_monitor.go:25` | **reused** — drives bounded KV op timeout |
| Commit-watcher backoff constants (template) | `manager_assignment.go:21-23` (`watcherBaseBackoff`, `watcherMaxBackoff`, `watcherJitter`) | **reused as template** — watcher constants declared in `worker_monitor.go` package-private vars |
| Retry loop shape (template: `monitorAssignmentChanges`) | `manager_assignment.go:325-353` | **reference** — rewatch shape to mirror |
| `recordKVError` (degraded-circuit feed) | `manager_degraded.go` via `m.recordKVError` symbol | reference only — `WorkerMonitor` does not have access to `m.recordKVError`; see §2 design call 1 |
| `publishLoop` — synchronous `publish` (bug site) | `internal/heartbeat/publisher.go:302-327` | **modified** — goroutine + in-flight gate + bounded per-goroutine timeout |
| `publish` (the KV Put call) | `internal/heartbeat/publisher.go:357-372` | **reused** — called from spawned goroutine |
| `Publisher.stopCh`, `doneCh` (lifecycle) | `internal/heartbeat/publisher.go:72-73` | **extended** — new `inFlightWG sync.WaitGroup` added; `Stop` waits on it |
| `Publisher.interval` field | `internal/heartbeat/publisher.go:63` | **reused** — per-goroutine timeout derived as `min(maxPublishTimeout, interval/2)`; no new field added |
| `heartbeat.New` constructor signature | `internal/heartbeat/publisher.go:109-135` | **unchanged** — no new parameter; all 18 `publisher_test.go` call sites and `manager_election.go:274` require no edits |
| `recordMetric` (success/failure recording) | `internal/heartbeat/publisher.go:380-389` | **reused** — called from spawned goroutine |
| `onError` (atomic pointer to error callback) | `internal/heartbeat/publisher.go:77` | **reused** — called from spawned goroutine; skipped on tick-skip |
| `HeartbeatInterval` default (`config.go:308`) | `config.go:308` — `default:"5s"` | reference — at default Interval the bounded timeout (`min(5s, 2.5s)`) = 2.5s; tighter than the v1 5s hardcode |
| Validation constraint: `HeartbeatTTL ≥ 2×HeartbeatInterval` only | `config.go:498-504` | reference — no absolute floor on either value; Interval/2 cap guarantees gate clearance at every valid ratio |
| Polling fallback cadence (`hbTTL/2`) | `worker_monitor.go:236` | reference — already covers silent-stall case; no second reconcile tick needed |

Verified against current branch `main` @ `89d7fa5`. Spec author MUST re-verify line numbers immediately before implementing if HEAD has advanced.

---

## 2. Design

### 2.1 Design call 1 — W2 watcher rewatch shape

**Options:**

- **A. Direct pattern-mirror of PR-1's `monitorAssignmentChanges`.** Split `processWatcherEvents` into `watchHeartbeats` (one session) and a retry loop in `monitorWorkers` with backoff + jitter. On `!ok`, return an error; the retry loop backs off and restarts. `recordKVError` is NOT called here — `WorkerMonitor` is an `internal/assignment` type and does not hold a reference to the manager's degraded-circuit; the watcher errors are logged at Warn level and the polling fallback preserves convergence.

- **B. Same pattern but add a new reconcile tick (watchdog for silent stall).** A separate ticker re-invokes `onChangeCb` every `N × hbTTL` if no watcher event has arrived. Redundant with the existing polling ticker (`monitorWorkers:236`) — both tickers would invoke `onChangeCb` on roughly the same cadence.

**Decision: Option A.** The silent-stall case does not require a reconcile tick because `WorkerMonitor` already has an independent polling fallback (`worker_monitor.go:235-247`) that fires at `hbTTL/2` regardless of watcher state. The polling ticker spans watcher close + backoff windows naturally — a watcher dead for 7s is covered within 7.5s. Adding a second tick duplicates the fallback without improving the recovery bound. PR-1's alias-watcher required a reconcile tick because its `monitorAssignmentChanges` had **no** polling fallback; `WorkerMonitor` already has one.

**Bounded KV op timeout (sub-fix):** `GetActiveWorkers` calls `m.heartbeatKV.Keys(ctx)` with the outer context passed to `Start`. A stuck `Keys` call blocks the poll goroutine and prevents both the stopCh and further ticker ticks from being processed. Fix: derive a per-call context with `context.WithTimeout(ctx, min(remaining, m.hbTTL/2))` where remaining = time until ctx deadline (or `hbTTL/2` if ctx has no deadline). If the timeout fires, `Keys` returns an error that the polling path logs and swallows — convergence is preserved at the next tick.

### 2.2 Design call 2 — W13 publisher non-blocking shape

**Options:**

- **X. Hard-bound the `Put` timeout to `HeartbeatTTL/2`.** Reduces the worst-case block, but does not decouple the loop from KV latency. At `HeartbeatTTL=3s, HeartbeatInterval=1s`: cap = 1.5s; a single blocked Put blocks the goroutine for 1.5s; next ticker tick (at t=1s) is dropped; publish resumes at t≥2s; TTL expires at t=3s from last successful write. Under sustained NATS stall, skipped ticks accumulate and the heartbeat key expires.

- **Y. Non-blocking via goroutine + single-slot channel.** Latest-wins coalescing. Larger blast radius; needs its own goroutine and shutdown sequencing.

- **Z. Spawn each `publish` in a goroutine, skip-if-prior-in-flight, WaitGroup for Stop.** On each ticker tick: if an `inFlight` `atomic.Bool` is set (prior publish still running), log a Warn (via `recordSkippedPublish`) and continue — do NOT invoke `onError` (no KV error has occurred; the skip is a liveness observation). If not set, set `inFlight`, spawn a goroutine that calls `publish`, clears `inFlight` when done, and calls the existing `recordMetric` + `onError` on error. `Stop` waits for in-flight goroutines via `sync.WaitGroup`.

**Math justification for Z over X, combined with the bounded per-goroutine timeout (P1-A fix).** The bound must guarantee the in-flight gate clears before the next tick fires, so the next tick can always attempt a fresh publish. Deriving the cap from `HeartbeatInterval` (not `HeartbeatTTL`) directly expresses this: a publish goroutine that times out at `Interval/2` has exited at least `Interval/2` before the next tick, giving the gate time to clear. At the minimum legal config (`HeartbeatTTL = 2×HeartbeatInterval`, e.g. `Interval=1s, TTL=2s`): `timeout = min(5s, 1s/2) = 500ms`. The goroutine exits by t=500ms; the next tick fires at t=1s — the gate is clear. Under `TTL/2` the same config gives `timeout = min(5s, 1s) = 1s`, exactly one full interval, meaning the goroutine may still be running when the next tick fires. `Interval/2` closes this gap at every valid configuration ratio. At default config (`Interval=5s, TTL=15s`): `timeout = min(5s, 2.5s) = 2.5s` — tighter than the v1 5s hardcoded value, still well within TTL. The `interval` field already exists on `Publisher` (`publisher.go:63`); no new field or parameter is required.

**Decision: Option Z with per-goroutine timeout bounded to `min(maxPublishTimeout, p.interval/2)`.** The main loop is never blocked by KV latency. One or more skipped-publish Warn logs surface the stall before any TTL expiry at every valid configuration ratio. The bounded timeout closes the residual gate-clearance gap that the original Z description left open.

---

## 3. Implementation

### 3.1 Rewrite `monitorWorkers` and split into `watchHeartbeats` (`worker_monitor.go`)

**Current shape of the watcher start + goroutine in `monitorWorkers`:**

```go
func (m *WorkerMonitor) monitorWorkers(ctx context.Context) {
    defer close(m.doneCh)

    if err := m.startWatcher(ctx); err != nil {
        m.logger.Warn("failed to start watcher, falling back to polling only", "error", err)
    }

    ticker := time.NewTicker(m.hbTTL / 2)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            if m.onChangeCb != nil {
                if err := m.onChangeCb(ctx); err != nil {
                    m.logger.Error("polling error", "error", err)
                }
            }
        case <-m.stopCh:
            m.stopWatcher()
            return
        case <-ctx.Done():
            m.stopWatcher()
            return
        }
    }
}
```

**Target shape:**

```go
// Package-private backoff constants (mirror of manager_assignment.go:21-23).
// Declared as vars so tests can override.
var (
    workerWatcherBaseBackoff = 2 * time.Second
    workerWatcherMaxBackoff  = 30 * time.Second
    workerWatcherJitter      = 0.3 // ±30%
)

func (m *WorkerMonitor) monitorWorkers(ctx context.Context) {
    defer close(m.doneCh)

    // Start watcher in a separate goroutine with rewatch-on-close.
    go m.monitorWatcherWithRetry(ctx)

    // Polling ticker for worker changes (fallback for silent-stall and
    // watcher-close backoff gaps). Runs independently of the watcher.
    ticker := time.NewTicker(m.hbTTL / 2)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            if m.onChangeCb != nil {
                if err := m.onChangeCb(ctx); err != nil {
                    m.logger.Error("polling error", "error", err)
                }
            }
        case <-m.stopCh:
            return
        case <-ctx.Done():
            return
        }
    }
}

// monitorWatcherWithRetry retries processWatcherEvents on failure with
// exponential backoff + jitter. It exits when ctx is cancelled or
// m.stopCh is closed.
func (m *WorkerMonitor) monitorWatcherWithRetry(ctx context.Context) {
    backoff := workerWatcherBaseBackoff
    for {
        err := m.processWatcherEvents(ctx)
        if err == nil || ctx.Err() != nil {
            return
        }
        // Check stopCh before sleeping.
        select {
        case <-m.stopCh:
            return
        default:
        }
        m.logger.Warn("heartbeat watcher failed, retrying",
            "error", err, "backoff", backoff)

        //nolint:gosec // jitter does not require crypto-secure random
        f := rand.Float64()
        low := 1 - workerWatcherJitter
        high := 1 + workerWatcherJitter
        delay := time.Duration(float64(backoff) * (low + f*(high-low)))

        select {
        case <-ctx.Done():
            return
        case <-m.stopCh:
            return
        case <-time.After(delay):
        }

        backoff = min(backoff*2, workerWatcherMaxBackoff)
    }
}
```

The `stopWatcher` calls in the `case <-m.stopCh` and `case <-ctx.Done()` arms of `monitorWorkers` are removed because `watchHeartbeats` calls `watcher.Stop()` in its deferred cleanup on every exit path (clean or error).

### 3.2 Rewrite `processWatcherEvents` body in-place and fold watcher setup into it (`worker_monitor.go`)

**Decision (P1-B):** Keep the function name `processWatcherEvents` to avoid breaking the existing test `TestWorkerMonitor_ProcessWatcherEvents_ClosedChannel`. The test is updated in §5.1 to assert the rewatch invariant. The function's body is replaced; its signature is unchanged.

**Current `processWatcherEvents` shape (the bug is at lines 329-332):**

```go
func (m *WorkerMonitor) processWatcherEvents(ctx context.Context) {
    // ...
    for {
        select {
        case <-ctx.Done():
            return
        case <-m.stopCh:
            return
        case entry, ok := <-watcher.Updates():
            if !ok {
                m.logger.Debug("watcher update channel closed")
                return          // <-- permanent exit; bug site
            }
            // ... debounce + callback
        }
    }
}
```

**Target shape — `processWatcherEvents` rewritten (same function name, body replaced):**

```go
// processWatcherEvents runs one watch session on all heartbeat keys.
// Channel closure or initial Watch failure is returned as an error so
// monitorWatcherWithRetry can restart with backoff. Context cancellation
// or m.stopCh closure returns nil for clean exit.
//
// The polling fallback in monitorWorkers covers the gap while this session
// is in the backoff window; no reconcile tick is needed here.
func (m *WorkerMonitor) processWatcherEvents(ctx context.Context) error {
    watcher, err := m.heartbeatKV.Watch(ctx, m.hbWatchPattern)
    if err != nil {
        return fmt.Errorf("failed to start heartbeat watcher: %w", err)
    }
    defer func() {
        if serr := watcher.Stop(); serr != nil && !natsutil.IsConsumerNotFound(serr) {
            m.logger.Warn("failed to stop heartbeat watcher", "error", serr)
        }
        m.watcherMu.Lock()
        m.watcher = nil
        m.watcherMu.Unlock()
    }()

    m.watcherMu.Lock()
    m.watcher = watcher
    m.watcherMu.Unlock()
    m.logger.Info("heartbeat watcher started", "pattern", m.hbWatchPattern)

    debounceTimer := time.NewTimer(100 * time.Millisecond)
    debounceTimer.Stop()
    var pendingCheck bool

    for {
        select {
        case <-ctx.Done():
            return nil
        case <-m.stopCh:
            return nil
        case entry, ok := <-watcher.Updates():
            if !ok {
                return errors.New("heartbeat watcher channel closed")  // triggers rewatch
            }
            if entry == nil {
                continue
            }
            m.logger.Debug("watcher: received entry",
                "key", entry.Key(), "operation", entry.Operation())
            if !pendingCheck {
                pendingCheck = true
                debounceTimer.Reset(100 * time.Millisecond)
            }
        case <-debounceTimer.C:
            if pendingCheck {
                pendingCheck = false
                m.logger.Debug("watcher detected change, triggering check")
                if m.onChangeCb != nil {
                    if err := m.onChangeCb(ctx); err != nil {
                        m.logger.Error("watcher-triggered check failed", "error", err)
                    }
                }
            }
        }
    }
}
```

Changes from the current `processWatcherEvents`:
1. **Signature change:** return type changes from `void` to `error` — the caller (`monitorWatcherWithRetry`) uses the return value to decide whether to retry.
2. The function now creates the watcher itself (previously `startWatcher` created it and passed `m.watcher` via the shared field). `startWatcher` is removed.
3. `return` on `!ok` becomes `return errors.New("heartbeat watcher channel closed")` — triggers retry.
4. `ctx.Done()` and `m.stopCh` arms return nil for clean exit.
5. `m.watcher` is managed inside `processWatcherEvents` (set on entry, cleared in deferred cleanup).

`startWatcher` is removed (its logic absorbed into `processWatcherEvents`). `stopWatcher` is retained (used by `Stop` for external cleanup).

Imports added if not already present in the file: `errors`, `math/rand/v2`.

### 3.3 Add bounded KV op timeout to `GetActiveWorkers` (`worker_monitor.go`)

**Current shape:**

```go
func (m *WorkerMonitor) GetActiveWorkers(ctx context.Context) ([]string, error) {
    keys, err := m.heartbeatKV.Keys(ctx)
    // ...
}
```

**Target shape:**

```go
func (m *WorkerMonitor) GetActiveWorkers(ctx context.Context) ([]string, error) {
    opTimeout := m.hbTTL / 2
    if deadline, ok := ctx.Deadline(); ok {
        if remaining := time.Until(deadline); remaining < opTimeout {
            opTimeout = remaining
        }
    }
    opCtx, cancel := context.WithTimeout(ctx, opTimeout)
    defer cancel()

    keys, err := m.heartbeatKV.Keys(opCtx)
    // ... rest unchanged
}
```

This ensures a stuck `Keys` call surfaces as a `context.DeadlineExceeded` error (logged by the polling path) rather than blocking indefinitely. Apply the same pattern to the `kv.Keys` call in `GetHeartbeats` (`worker_monitor.go:192`) for symmetry — same bounded context, same pattern.

### 3.4 Modify `publishLoop` for non-blocking publish (Option Z) with bounded per-goroutine timeout (`publisher.go`)

**P1-A fix:** The per-goroutine `Put` timeout must guarantee the in-flight gate clears before the next tick fires. The correct bound is `min(maxPublishTimeout, p.interval/2)` — derived from `HeartbeatInterval`, not `HeartbeatTTL` (see §2.2 math). The `interval` field already exists on `Publisher` (`publisher.go:63`). **No new field or parameter is added to `heartbeat.New`**; the existing signature is unchanged.

**`heartbeat.New` signature: unchanged.** Current signature (all call sites remain unmodified):

```go
func New(
    kv jetstream.KeyValue,
    prefix string,
    workerID string,
    interval time.Duration,
    metrics types.WorkerMetrics,
    logger types.Logger,
) *Publisher
```

**Caller inventory for `heartbeat.New` — all call sites that must compile after this change** (the signature is unchanged, so no edits are required, but the implementer must verify these are exercised by the new Test 5.3):

| File | Line | Notes |
|---|---|---|
| `manager_election.go` | 274 | Production call site; unchanged |
| `internal/heartbeat/publisher_test.go` | 38 | `TestPublisher_SetOnError_Concurrent` |
| `internal/heartbeat/publisher_test.go` | 70 | (test) |
| `internal/heartbeat/publisher_test.go` | 83 | (test) |
| `internal/heartbeat/publisher_test.go` | 105 | (test) |
| `internal/heartbeat/publisher_test.go` | 118 | (test) |
| `internal/heartbeat/publisher_test.go` | 140 | (test) |
| `internal/heartbeat/publisher_test.go` | 154 | (test) |
| `internal/heartbeat/publisher_test.go` | 166 | (test) |
| `internal/heartbeat/publisher_test.go` | 196 | (test) |
| `internal/heartbeat/publisher_test.go` | 245 | (test) |
| `internal/heartbeat/publisher_test.go` | 280 | (test — loop, multiple publishers) |
| `internal/heartbeat/publisher_test.go` | 308 | (test) |
| `internal/heartbeat/publisher_test.go` | 329 | (test) |
| `internal/heartbeat/publisher_test.go` | 374 | (test) |
| `internal/heartbeat/publisher_test.go` | 403 | (test) |
| `internal/heartbeat/publisher_test.go` | 446 | (test) |
| `internal/heartbeat/publisher_test.go` | 494 | (test) |
| `internal/heartbeat/publisher_test.go` | 520 | `TestPublisher_JSONOutputRoundTrip` |
| `internal/heartbeat/doc.go` | 28 | Comment example only; update to match current signature |

Verified against HEAD `89d7fa5` via `grep -n "New(" internal/heartbeat/publisher_test.go`. The grep returns 19 hits; line 235 is a `jetstream.New` call (not a heartbeat constructor), so the table contains 18 `heartbeat.New` call sites.

**New fields on `Publisher`** (no change to constructor signature):

```go
// inFlight guards concurrent publish attempts; set atomically before
// spawning the goroutine, cleared when the goroutine completes.
inFlight  atomic.Bool
// inFlightWG tracks in-flight publish goroutines so Stop can drain them.
inFlightWG sync.WaitGroup
```

**Package-level constant:**

```go
const maxPublishTimeout = 5 * time.Second
```

**Current `publishLoop` shape:**

```go
func (p *Publisher) publishLoop() {
    defer close(p.doneCh)
    for {
        select {
        case <-p.stopCh:
            return
        case <-p.ticker.C:
            ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
            err := p.publish(ctx)
            cancel()
            if err != nil {
                p.recordMetric(false)
                p.logger.Warn("heartbeat publish failed", ...)
                if onError := p.onError.Load(); onError != nil {
                    (*onError)(err)
                }
            } else {
                p.recordMetric(true)
            }
        }
    }
}
```

**Target shape:**

```go
func (p *Publisher) publishLoop() {
    defer close(p.doneCh)
    defer p.inFlightWG.Wait() // drain any in-flight goroutine before signalling done
    for {
        select {
        case <-p.stopCh:
            return
        case <-p.ticker.C:
            if !p.inFlight.CompareAndSwap(false, true) {
                // Prior publish still in flight: skip this tick.
                // recordSkippedPublish logs at Warn level (single responsibility).
                p.recordSkippedPublish()
                continue
            }
            p.inFlightWG.Add(1)
            go func() {
                defer p.inFlightWG.Done()
                defer p.inFlight.Store(false)

                // Bound the Put timeout so the gate clears before the next tick.
                // At default config (Interval=5s): min(5s, 2.5s) = 2.5s.
                // At tight config (Interval=1s): min(5s, 500ms) = 500ms — exits
                // at least 500ms before the next tick, keeping the gate clear.
                timeout := min(maxPublishTimeout, p.interval/2)
                ctx, cancel := context.WithTimeout(context.Background(), timeout)
                err := p.publish(ctx)
                cancel()

                if err != nil {
                    p.recordMetric(false)
                    p.logger.Warn("heartbeat publish failed",
                        "worker_id", p.workerID, "error", err)
                    if onError := p.onError.Load(); onError != nil {
                        (*onError)(err)
                    }
                } else {
                    p.recordMetric(true)
                }
            }()
        }
    }
}
```

**`recordSkippedPublish`** is a new single-responsibility helper that logs at Warn level. It is the **only** site where the skip warning is emitted — `publishLoop` must NOT log inline before calling it. No metrics counter exists for skipped publishes (see §10.3):

```go
func (p *Publisher) recordSkippedPublish() {
    p.logger.Warn("heartbeat publish in flight, skipping tick",
        "worker_id", p.workerID)
}
```

Before adding a new counter method: check `types/metrics_collector.go` for an existing `RecordHeartbeatSkipped` (or equivalent) symbol. As of HEAD `89d7fa5`, `types.WorkerMetrics` exposes only `RecordHeartbeat(workerID string, success bool)` — no skip counter. A Warn log is sufficient for v1. If a skip counter is added in the future, it should be placed on `types.WorkerMetrics` alongside `RecordHeartbeat`.

**Math justification for the timeout bound.** The timeout `min(5s, interval/2)` guarantees the in-flight goroutine exits at most `interval/2` after spawning, leaving at least `interval/2` before the next tick fires. Gate clearance before the next tick is thus structural, not probabilistic. Combined with Option Z's non-blocking loop, consecutive stalled publishes produce skip Warn logs rather than silent TTL expiry. At default config (`Interval=5s, TTL=15s`): bound = `2.5s`, well within TTL. At the tightest legal config (`Interval=1s, TTL=2s`): bound = `500ms`; TTL/bound = 4×, so the key cannot expire while three consecutive publishes succeed (three × 500ms = 1.5s < TTL = 2s). See §2.2 for the full derivation.

**`Stop` already drains via `<-p.doneCh`** (line 286). Because `publishLoop` defers `p.inFlightWG.Wait()` before `close(p.doneCh)`, the `doneCh` close happens only after all in-flight goroutines have returned. No change to `Stop` is required.

---

## 4. Behavior summary

### Before PR-4

| Scenario | Observed behavior |
|---|---|
| Watcher `Updates()` channel closes (NATS reconnect, broker restart) | `processWatcherEvents` exits; watcher is permanently dead; detection falls to polling at `hbTTL/2 = 7.5s` default |
| Watcher silently stalls (NATS restart without channel close) | Polling already covers; same 7.5s detection fallback |
| `kv.Keys` hangs inside `GetActiveWorkers` | Polling goroutine blocks indefinitely |
| Heartbeat `Put` exceeds 5s (NATS stall) | `publishLoop` goroutine blocked 5s; pending ticks dropped; under `HeartbeatTTL ≤ 5s`, the key can expire while worker is healthy |

### After PR-4

| Scenario | Observed behavior |
|---|---|
| Watcher `Updates()` channel closes | `processWatcherEvents` returns error; `monitorWatcherWithRetry` backs off and restarts the watcher session; polling fallback covers the backoff window |
| Watcher silently stalls | Polling fallback unchanged; still covers at `hbTTL/2` cadence |
| `kv.Keys` hangs inside `GetActiveWorkers` | Bounded at `hbTTL/2` via per-call context; surfaces as a logged error, not a goroutine hang |
| Heartbeat `Put` stalls / times out | In-flight goroutine runs independently with timeout `min(5s, Interval/2)`; main loop skips that tick (Warn log via `recordSkippedPublish`); `Stop` drains the goroutine before signalling done; timed-out publish clears `inFlight` so the next tick retries with the gate clear |

---

## 5. Tests

Three required tests. Each encodes an invariant that fails if the corresponding fix is absent.

### Test 5.1 — Watcher close triggers rewatch (`worker_monitor_test.go`)

**Updates the existing test `TestWorkerMonitor_ProcessWatcherEvents_ClosedChannel`** (currently at `internal/assignment/worker_monitor_test.go:457-489`). The existing test asserts that `processWatcherEvents` exits without panic when the channel closes. This must be replaced with a rewatch assertion as the function no longer exits cleanly on `!ok` — it returns an error that the retry wrapper uses to restart.

**Intent (P1-C):** verify that closing `watcher.Updates()` causes `monitorWatcherWithRetry` to call `processWatcherEvents` again (rewatch) rather than exiting permanently. The test must assert that a new `Watch` call is made, not merely that the callback fires (which polling could also explain).

**Setup:**
- Extend the existing `fakeKeyWatcher` (lines 435-455) with a `Watch` call counter or replace it with a new `fakeKV` double that:
  - exposes a `WatchCallCount() int` accessor, and
  - exposes `CloseUpdates()` to close the current `Updates()` channel (simulating `!ok`).
- Override `workerWatcherBaseBackoff` to 10ms.
- Create a `WorkerMonitor` using the `fakeKV` double (pass it as `heartbeatKV`). The fake's `Watch()` method returns a fresh `fakeKeyWatcher` on each call and increments the call counter.
- Create a callback counter to confirm callbacks fire.

**Action:**
1. Start the monitor.
2. Wait for `WatchCallCount() >= 1` (first watcher session active, up to 200ms).
3. Call `CloseUpdates()` on the current fake watcher.
4. Wait for `WatchCallCount() >= 2` (rewatch occurred, up to 200ms; backoff is 10ms so this is conservative).

**Assertion:**
- `WatchCallCount()` is ≥ 2 after the close — a new `Watch` call was made.
- Stop exits cleanly within 500ms.

**Why it fails without the fix:** current code returns `nil` on `!ok`, exits permanently, and `WatchCallCount()` stays at 1.

**File target:** `internal/assignment/worker_monitor_test.go` (update `TestWorkerMonitor_ProcessWatcherEvents_ClosedChannel`)

### Test 5.2 — Bounded `GetActiveWorkers` KV timeout (`worker_monitor_test.go`)

**Intent:** verify that a slow `kv.Keys` call does not block the polling goroutine beyond `hbTTL/2`. The fake must be context-aware so the test is deterministic rather than relying on a wall-clock sleep.

**Setup:**
- Create a `fakeKV` double (reuse or extend the one introduced in §5.1) whose `Keys(ctx context.Context)` method blocks until `ctx.Done()` fires, then returns `ctx.Err()` (i.e., `context.DeadlineExceeded` when the derived timeout fires). It does NOT use a fixed sleep.
- Create a `WorkerMonitor` with `hbTTL = 200ms` (poll interval = 100ms, bounded KV timeout = 100ms).
- Capture log output via a test logger.

**Action:**
1. Start the monitor.
2. Wait for `Stop` to be called within a deadline of `3 × hbTTL` (600ms):
   - The poll ticker fires at 100ms; `Keys` is called with a 100ms deadline context; the fake unblocks when the context expires.
   - After the timeout, the poll goroutine logs the error and loops back to the select.
3. Stop the monitor.

**Pseudocode:**
```go
fakeTTL := 200 * time.Millisecond
fakeKV := &slowKV{} // Keys blocks until ctx.Done()
monitor := NewWorkerMonitor(fakeKV, "worker", fakeTTL, onChangeCb, testLogger)
require.NoError(t, monitor.Start(ctx))

stopCh := make(chan struct{})
go func() {
    time.Sleep(3 * fakeTTL) // let at least 2 poll ticks fire
    require.NoError(t, monitor.Stop())
    close(stopCh)
}()
select {
case <-stopCh:
case <-time.After(2 * time.Second):
    t.Fatal("Stop hung — poll goroutine is blocked on Keys")
}
```

**Assertion:**
- `Stop` returns before the 2s deadline (the poll goroutine was NOT blocked after the derived context expired).
- The test logger captured at least one "failed to list heartbeat keys" or equivalent error log.

**Why it fails without the fix:** current code passes the outer context (no per-call deadline) into `Keys`; the fake blocks indefinitely; `Stop` never unblocks.

**File target:** `internal/assignment/worker_monitor_test.go`

### Test 5.3 — Overlapping publish skips tick, main loop stays unblocked (`publisher_test.go`)

**Intent:** verify that when a `Put` call blocks for longer than one tick interval, the next tick is skipped (Warn log) instead of blocking the loop, and that after the blocked `Put` is released the gate clears and subsequent ticks publish normally.

**Setup:**
- Create a KV double whose `Put` method **ignores `ctx.Done()`** and blocks until a test-controlled `release` channel is closed. The fake must NOT honor context cancellation — if it did, the publish goroutine would return at `interval/2 = 25ms` (the bounded timeout) before the next tick at `50ms`, clearing the gate and preventing `recordSkippedPublish` from ever being called. The fake must park until the test releases it.
- Create a `Publisher` with `interval = 50ms` only. No `ttl` parameter — `heartbeat.New` is unchanged; the per-goroutine timeout is computed internally as `min(5s, interval/2) = 25ms`.
- Install a counter on `onError` (which must NOT be called on a skipped tick).
- Capture log output via a test logger (skip signal is a Warn log; no new metrics counter exists in v1 — see §10.3).

**Phase 1 — skip path (fake ignores ctx.Done()):**
1. Start the publisher.
2. Wait for the first tick — the first `publish` goroutine is spawned; the fake blocks without returning (ignores the 25ms deadline).
3. Wait `> 100ms` — at least two more ticks fire; the prior publish is still in flight (fake still blocking).
4. Assert the skip warning was logged (inspect test logger for "heartbeat publish in flight, skipping tick").
5. Verify `onError` has NOT been called.

**Phase 2 — recovery path (release the fake):**
6. Close the release channel.
7. Wait `> 50ms` — the publish goroutine unblocks and completes; `inFlight` is cleared.
8. Wait for one more tick to fire and publish successfully (test logger shows success, or `recordMetric(true)` counter increments — no further skip log).
9. Stop the publisher.

**Assertion (combined):**
- `onError` was NOT called at any point during phases 1 or 2.
- Test logger contains ≥ 1 occurrence of "heartbeat publish in flight, skipping tick" (from phase 1).
- After phase 2 release, no further skip log appears (gate cleared, recovery confirmed).
- `Stop` returns within 500ms (WaitGroup drains the in-flight goroutine before `doneCh` closes).

**Why it fails without the fix:** current synchronous `publish` would block the entire `publishLoop` for the duration of the `Put` block; no skip event is recorded.

**File target:** `internal/heartbeat/publisher_test.go`

---

## 6. Migration / backwards compatibility

- No public API changes. `WorkerMonitor` and `Publisher` APIs are unchanged. The new `monitorWatcherWithRetry` method is unexported; `processWatcherEvents` changes signature (return type `error`) but remains unexported.
- `GetActiveWorkers` and `GetHeartbeats` signatures are unchanged; the per-call context is derived internally.
- `heartbeat.New` signature is **unchanged**. The `Publisher` struct gains two unexported fields (`inFlight`, `inFlightWG`). `Publisher.Stop` behavior is unchanged from the caller's perspective: it still blocks until the publish goroutine has fully exited. The only observable difference is that `Stop` no longer returns while a `publish` goroutine is still running (was: possible; now: impossible).
- Error callback semantics (`SetOnError`): the contract is unchanged — the callback is invoked only on actual KV errors. A skipped tick (no KV error) does NOT invoke `onError`. This is explicit in §3.4 and must be preserved.

---

## 10. Known pre-existing issues NOT addressed by PR-4

### 10.1 Watchdog timer for silent watcher stall

The audit (`00-report.md:311`) also suggests "a watchdog timer that rewatches if no event arrives within `N × hbTTL`." This is not included in PR-4 because:

1. The existing polling fallback (`worker_monitor.go:235-247`) already provides convergence recovery at `hbTTL/2` cadence.
2. A watchdog timer adds state (last-event timestamp) and a second ticker, for no improvement in convergence time over the existing polling path.
3. The empirical finding (`project_nats_watcher_empirical_finding.md`) confirms `Updates()` does NOT close on NATS restart — the only benefit of a watchdog would be to re-enable the debounce callback path, which fires within 100ms of events. In practice the watcher resumes delivering events after NATS reconnects; the watchdog is insurance against NATS reconnects that never resume. This is a separate operational concern and a candidate for PR-5+.

**Deferred to:** future PR or PR-6+ if operator evidence surfaces. Not tracked as a gap that PR-4 deliberately introduced.

### 10.2 `HeartbeatTTL` lower-bound validation (W7 territory)

The audit flags that `HeartbeatTTL = 2×HeartbeatInterval` is the minimum allowed by validation (`config.go:500-503`), but does not enforce a minimum absolute value. A user can configure `HeartbeatInterval=500ms, HeartbeatTTL=1s` legally.

**PR-4's Z implementation with the `min(5s, Interval/2)` timeout cap guarantees gate clearance before the next tick at all valid configuration ratios.** At the legal floor (`TTL = 2×Interval`, e.g. `Interval=1s, TTL=2s`): the per-goroutine timeout is `Interval/2 = 500ms`. The goroutine exits by `t=500ms`; the next tick fires at `t=Interval = 1s` — the gate is structurally clear.

**However, the floor case leaves a zero-margin operational window.** After a successful publish at `t=0`, a stalled goroutine spawned at `t=Interval` times out at `t=1.5×Interval`. The next publish attempt is at `t=2×Interval`, which is exactly when the prior heartbeat TTL expires. The skip Warn IS emitted at the next tick attempt (`t=2×Interval`), but the publish that would refresh the key and the TTL expiry coincide at that same moment — the Warn-before-expiry property is structurally preserved, but the operational margin is zero. An operator configuring at the absolute floor has no time slack between the skip Warn and the key expiry.

**Recommended practice:** operators should use `TTL ≥ 3×Interval`. At `TTL = 3×Interval`, the same scenario leaves `Interval` of margin (the key refreshed at `t=0` expires at `t=3×Interval`; the next publish attempt is at `t=2×Interval`, one full interval before expiry). PR-7 (W7 config validation) is the appropriate place to add a `ValidateWithWarnings` advisory that surfaces this recommendation before the configuration goes live.

### 10.3 `SetOnError` contract — no change

`Publisher.SetOnError` registers a callback invoked on each actual publish failure. PR-4 does not change this contract. Skipped-tick events (in-flight gate fired) do NOT invoke `onError` — the caller's error handler is for KV errors, not backpressure signals. Any future PR that changes this classification must update the Godoc for `SetOnError` and the test in §5.3.

### 10.4 `GetHeartbeats` bounded context

§3.3 applies the same bounded-KV-op pattern to `GetHeartbeats.kv.Keys` for symmetry. `GetHeartbeats` is not called by the polling path in production today (it is a public utility method), but it shares the same `kv.Keys` call shape. Bounding it here costs ~3 LOC and prevents the same hang if callers pass a long-lived context.

### 10.5 Degraded-circuit feed gap for watcher errors

`WorkerMonitor` lives in `internal/assignment` and holds no reference to the manager's `recordKVError` function. As a result, watcher errors (channel close, reconnect failures) logged in `monitorWatcherWithRetry` do NOT flow into the degraded-mode circuit — they are logged at Warn level and the polling fallback provides implicit recovery.

By contrast, **publish errors already reach the degraded circuit**: `manager_election.go:277` wires `publisher.SetOnError(m.recordKVError)`, so every failed `Put` in `publishLoop` calls `recordKVError` directly. That path is preserved unchanged by PR-4 (§3.4 calls `(*onError)(err)` from the spawned goroutine exactly as before).

The gap is specific to watcher errors. A future fix could add an `onWatchError func(error)` callback to `NewWorkerMonitor`, wired to `m.recordKVError` at the manager layer, mirroring the publisher pattern. This is not a PR-4 deliverable — the polling fallback ensures convergence even when the watcher is down, so the omission is an observability gap rather than a correctness gap. Tracked as a candidate follow-up (W2 sub-item) for PR-5+.

---

## 11. Verification checklist

Before this PR can be considered ready to merge:

1. **All three new tests pass** under `go test ./... -count=1 -race`:
   - Test 5.1 (watcher close → rewatch)
   - Test 5.2 (bounded `GetActiveWorkers` KV timeout)
   - Test 5.3 (overlapping publish skips tick, loop stays live)
2. **Existing test suite passes** — no regressions in `worker_monitor_test.go`, `publisher_test.go`, or any test matching `*Heartbeat*` or `*Monitor*`.
3. `go vet ./...` and the configured linter pass without new warnings.
4. The backoff constants in `worker_monitor.go` (`workerWatcherBaseBackoff`, etc.) are package-private and do not collide with the `manager_assignment.go` constants.
5. `grep -rn "startWatcher" ./internal/assignment/ | grep -v "_test.go"` returns zero hits (`startWatcher` is removed; its logic is absorbed into `processWatcherEvents`). `processWatcherEvents` is retained but its signature now returns `error` — verify the grep shows no remaining call sites with the old void-return shape. Scope to `./internal/assignment/` — `startWatcher` also exists in `internal/durable/claim_resolver.go` (unrelated) and would produce false positives if searched from the repo root.
6. `Publisher.Stop` does not return before all in-flight `publish` goroutines complete (verified by Test 5.3's Stop-without-hang assertion).
7. `/post-impl-review` (Codex `xhigh` for v1) returns a MERGE verdict.

---

## 12. Model & effort recommendations (from `00-fix-plan.md` §"Per-PR matrix")

| Phase | Tool | Model / effort |
|---|---|---|
| Planning (this spec) | Claude Code | **Sonnet 4.6** — pattern-mirror of commit watcher; bounded publish is a known shape |
| Implementation | Claude Code | **Sonnet 4.6** — mechanical translation of the commit-watcher pattern + non-blocking publish path |
| Plan review (pre-impl) | `/plan-review` | Codex **high** (template is known; primary novelty is Z-impl lifecycle) |
| Post-impl review (v1) | `/post-impl-review` | Codex **xhigh** (touches a load-bearing detection path) |
| Post-impl review (v2+) | `/post-impl-review` | Codex **high** |

Rationale (from `00-fix-plan.md`): PR-4 is a mechanical implementation — the commit-watcher pattern is the template for W2, and the non-blocking publish shape (Z) is a standard goroutine + atomic + WaitGroup pattern. The xhigh first post-impl pass is justified because `WorkerMonitor` and `Publisher` are on the worker liveness critical path; a subtle lifecycle bug (goroutine leak, double-close on `stopCh`) would be operationally severe. Estimated reviewer wall-time: ~5 min.

**LOC estimate:** ~90-100 gross production additions (excluding blank lines and comments), offset by ~60 deletions, for a net delta of ~30-40 lines. Breakdown of gross additions: `monitorWatcherWithRetry` ~20, `processWatcherEvents` body rewrite ~30 (incorporates old `startWatcher` logic), bounded-KV-op context derivation ×2 ~8, `publishLoop` goroutine rewrite + WaitGroup + new fields ~25, `recordSkippedPublish` helper ~5, `maxPublishTimeout` constant ~2. Deletions: original `processWatcherEvents` body (~35), `startWatcher` (~25). No `heartbeat.New` signature change in v3 (saves ~7 lines vs. v2 estimate). If gross additions exceed ~110 lines, review for opportunities to extract a shared `withBoundedCtx` helper or consolidate error paths.
