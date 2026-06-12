# Parti Lifecycle & State Management

> Worker lifecycle, stable IDs, two-phase handoff, and degraded mode.

**Related Documentation:**
- [Docs README](README.md) - Documentation map
- [Architecture](ARCHITECTURE.md) - System architecture and concepts
- [Configuration Guide](CONFIGURATION.md) - Configuration options
- [Consumer Helpers](CONSUMERS.md) - JetStream consumer management
- [Migrating: `Manager.Start` returns at `StateWaitingAssignment`](MIGRATING_MANAGER_START.md) - breaking change in the upcoming release; affects every caller that reads `CurrentAssignment()` immediately after `Start`

---

## Table of Contents

1. [Worker Lifecycle](#worker-lifecycle)
2. [Stable ID Renewal Lifecycle](#stable-id-renewal-lifecycle)
3. [Two-Phase Handoff](#two-phase-handoff)
4. [Degraded Mode](#degraded-mode)

---

## Worker Lifecycle

Workers progress through a defined state machine:

### State Machine

```
                              ┌─────────────────────────────────────┐
                              │            Normal Flow              │
                              │                                     │
    ┌────────┐    ┌───────────▼───┐    ┌──────────┐       ┌─────────────────────┐
    │  INIT  │───▶│ CLAIMING_ID   │───▶│ ELECTION │───▶   │ WAITING ASSIGNMENT  │
    └────────┘    └───────────────┘    └──────────┘       │  [Start returns ◀]  │
                                                          └─────────┬───────────┘
                                                                    │ (background runner)
                  ┌─────────────────────────────────────────────────┘
                  ▼
            ┌──────────┐
            │  STABLE  │◄────────────────────────────────┐
            └────┬─────┘                                 │
                 │                                       │
     ┌───────────┼───────────┬───────────┐               │
     ▼           ▼           ▼           ▼               │
┌─────────┐ ┌─────────┐ ┌─────────┐ ┌──────────┐         │
│ SCALING │ │EMERGENCY│ │DEGRADED │ │ SHUTDOWN │         │
└────┬────┘ └────┬────┘ └────┬────┘ └──────────┘         │
     │           │           │                           │
     ▼           │           │                           │
┌───────────┐    │           │                           │
│REBALANCING│────┼───────────┴───────────────────────────┘
└───────────┘    │
                 └─────────────────────────────────────────▶ STABLE
```

**Start return point:** `Manager.Start(ctx)` returns once the worker has
reached `WaitingAssignment` — i.e., the stable worker ID is claimed, KV
buckets exist, election has been run, and heartbeat + calculator are
wired. The transition to `Stable` happens in a background goroutine after
the initial assignment lands and is applied. Use
`Manager.WaitState(StateStable, timeout)` to block until the manager is
ready to process work.

The background runner is best-effort and single-attempt: if the initial
assignment fetch or apply fails, the runner logs the error and falls
through to monitor startup. Subsequent retries are driven by existing
recovery mechanisms — `monitorAssignmentChanges` redelivers when the
leader publishes; `scheduleApplyRetry` (inside `applyAssignmentWithPrev`)
retries failed applies; `monitorNATSConnection` drives
`attemptRecoveryFromDegraded` on reconnect.

A separate watchdog goroutine fires `enterDegraded("startup-timeout")`
once if the manager is still in `WaitingAssignment` after `StartupTimeout`
(measured from `Start` invocation). This is the probe-rotation signal.
The runner itself does not enter or exit degraded.

**Startup-timeout-degraded recovery is not guaranteed self-healing while
the runner is blocked.** Once monitors start, `monitorNATSConnection`
calls `attemptRecoveryFromDegraded` on its `ExitThreshold` tick even
without a prior disconnect, so the runner-succeeds-but-watchdog-already-
fired case recovers automatically. But if the runner is stuck inside the
unbounded `handoffCoordinator.Apply(m.ctx, ...)` call, the monitor set
has not started yet — startup-timeout-degraded then stays until the
runner returns or the pod is rotated by the probe. This is the documented
trade-off of inheriting pre-refactor Start's apply boundedness.

Apply boundedness is unchanged from pre-refactor Start:
`handoffCoordinator.Apply(m.ctx, ...)` is unbounded per attempt. A stuck
consumer updater can block the runner inside one apply attempt until
Stop. The watchdog still fires for probe rotation in that case.

### State Descriptions

| State               | Description                                              |
|---------------------|----------------------------------------------------------|
| `Init`              | Initial state before any operations                      |
| `ClaimingID`        | Claiming stable worker ID from NATS KV                   |
| `Election`          | Participating in leader election                         |
| `WaitingAssignment` | Waiting for initial partition assignment                 |
| `Stable`            | Normal operation with stable assignment                  |
| `Scaling`           | Dynamic scaling event detected (workers joining/leaving) |
| `Rebalancing`       | Partition rebalancing in progress                        |
| `Emergency`         | Worker crash detected, immediate rebalance               |
| `Degraded`          | Operating with cached data due to NATS issues            |
| `Shutdown`          | Graceful shutdown in progress                            |

### State Transitions

```go
// Access current state
state := mgr.State()

// Monitor state changes via hooks
hooks := &parti.Hooks{
    OnStateChanged: func(ctx context.Context, from, to parti.State) error {
        log.Printf("State: %s → %s", from, to)
        return nil
    },
}
```

---

## Stable ID Renewal Lifecycle

**Stable Worker IDs** are the foundation of Parti's partition affinity. Each worker claims a stable ID from a pool (e.g., `worker-0`, `worker-1`) stored in NATS KV, which persists across restarts for a TTL window.

### How It Works

The Stable ID lifecycle consists of four key operations managed by an internal `Claimer`:

1. **Claim(ctx)**: Acquires the first available ID from the pool `[WorkerIDMin, WorkerIDMax]`
   - Uses NATS KV `Create` semantics for atomic claiming
   - Tries each ID sequentially until finding an available one
    - Returns an error if the pool is exhausted
   - ID is valid for `WorkerIDTTL` duration

2. **StartRenewal()**: Starts background renewal to keep the ID alive
   - Renews every `ttl/3` (minimum 100ms)
   - Must be called after `Claim()`
    - Idempotent: subsequent calls return an error
   - Each renewal uses a short timeout context (100ms–5s)
   - Transient failures are logged and retried on the next tick; a detected claim loss (`ErrClaimLost`) stops the loop and triggers a worker self-stop

3. **Release(ctx)**: Stops renewal and deletes the KV key
   - Frees the ID for immediate reuse by other workers
    - Idempotent: subsequent calls return an error
   - Waits for renewal goroutine to stop before returning

4. **Close()**: Stops renewal but **keeps** the KV key
   - Used for handoff scenarios where the ID should remain claimed
    - After `Close()`, `StartRenewal()` returns an error

### Manager Integration

The Manager handles the Stable ID lifecycle automatically:

```go
// During Start():
// 1. Claim ID
workerID, err := claimer.Claim(ctx)
if err != nil {
    return fmt.Errorf("claim ID: %w", err)
}

// 2. Start background renewal (no context needed)
if err := claimer.StartRenewal(); err != nil {
    return fmt.Errorf("start renewal: %w", err)
}

// During Stop():
// Release ID (stops renewal + deletes key)
_ = claimer.Release(ctx)
```

### Key Behaviors

**Renewal Timing:**
- Interval: `WorkerIDTTL / 3` (minimum 100ms)
- Each tick uses its own timeout: `min(max(ttl/3, 100ms), 5s)`
- Transient failures are retried on the next tick; a lost claim stops the renewal loop and triggers a worker self-stop via `Manager.Stop`, surfacing the cause through the `OnError` hook

**Restart Behavior:**
- If a worker restarts within the TTL window, it will reclaim its previous ID
- If a worker's key is stale (not renewed within 3 renewal intervals), a restarting worker reclaims it via an atomic revision-checked takeover, recovering worker IDs leaked by ungraceful exits even when the bucket TTL has not yet expired
- On `Manager.Start`, the stableID bucket's `MaxAge` is reconciled to exactly `WorkerIDTTL`; an operator-created bucket with a different TTL (including `0` / unlimited) is corrected automatically
- This preserves partition affinity during rolling updates

**Pool Exhaustion:**
- If all IDs are in use, new workers will fail to start
- Increase `WorkerIDMax` (or reduce `WorkerIDTTL`) to allow more concurrent workers

**Thread Safety:**
- All operations are safe for concurrent use
- Internal state protected by atomics and sync primitives

### Configuration

Control Stable ID behavior via the `Config` struct:

```go
cfg := &parti.Config{
    WorkerIDPrefix: "worker",      // Prefix for IDs
    WorkerIDMin:    0,             // Minimum ID number
    WorkerIDMax:    999,           // Maximum ID number (1000 workers)
    WorkerIDTTL:    75*time.Second, // TTL for ID claims
}
```

**Recommendations:**
- `WorkerIDTTL`: 3-5x `HeartbeatTTL` (default 75s is 5x the default HeartbeatTTL of 15s)
- `WorkerIDMax`: Set to maximum expected workers + buffer

---

## Two-Phase Handoff

When `EnableTwoPhaseHandoff` is true, workers coordinate partition reassignment through KV-backed ownership claims so that the old owner releases a partition only after the new owner has durably claimed it. This orders the **release** side of a handoff (no unowned gap) and minimizes processing overlap — it does not by itself gate message consumption (see the per-tier table below).

### The Protocol

There is no leader→worker message exchange. Each worker independently drives its own side of the handoff against a shared KV bucket of per-partition claims, using CAS (revision-checked) writes:

```
   NEW OWNER (W2, gaining P1)            OLD OWNER (W1, losing P1)
        │                                     │
   1. Prepare: CAS-write claim                │
      {P1: owner=W2, state=prepare}           │
        │                                1. Removal guard: read P1's claim;
   2. Apply: update consumer,               keep consuming P1 until the claim
      start consuming P1's subject          shows a DIFFERENT owner in
        │                                   commit or stable state
   3. Commit: CAS claim → commit            │
   4. Stabilize: CAS claim → stable     2. Claim shows W2 commit/stable →
        │                                   remove P1's subject locally
        ▼                                     ▼
```

A background sweeper reconciles stale or interrupted claims toward `stable` on `SweepInterval`, so a worker crashing mid-handoff does not strand a claim. On the leader, the same sweep also reaps orphaned claims — stable claims whose partition has been removed from the partition source — after the partition has been continuously absent from BOTH the leader's source view and the latest committed assignment for a 10-minute grace period (a partition the live commit still references is never an orphan, even if the source already dropped it). The delete is revision-checked, so a partition re-added concurrently always wins over the reaper; followers never reap (their source view could be config-skewed during a rolling upgrade), and any stretch where the leader cannot verify the set restarts the grace clock.

### Handoff States

| State     | Description                                                        |
|-----------|--------------------------------------------------------------------|
| `Stable`  | Partition ownership is finalized                                   |
| `Prepare` | New owner has claimed the partition; old owner may still be consuming |
| `Commit`  | New owner is consuming; old owner may now remove the subject       |

### Configuration

```go
cfg := &parti.Config{
    EnableTwoPhaseHandoff: true,
    Handoff: parti.HandoffConfig{
        SweepInterval: 30 * time.Second,  // Cleanup stale claims
        MaxRetries:    3,                  // CAS retry limit
        BaseBackoff:   50 * time.Millisecond,
        MaxBackoff:    500 * time.Millisecond,
        Jitter:        0.2,
    },
}
```

### What Each Tier Guarantees

Per-partition durables are **shared by name** across workers (`<prefix>_<partitionID>_<hash>` — no worker ID), so delivery is at-least-once at every tier and overlap is bounded, never zero:

| Configuration | What it adds |
|---------------|--------------|
| No two-phase (default) | Assignments switch directly; old and new owner may briefly consume concurrently during the switch. |
| `EnableTwoPhaseHandoff` only | Orders the release: the old owner keeps a partition until the new owner's claim reaches commit/stable, so no unowned gap. Claims are written but **never consulted on the consume path** — the manager logs a warning when the consumer reports no processing gate. Does not reduce overlap by itself. |
| + Processing gate (`ProcessingGate`) | Per-message, pre-handler admission control: the non-owner NAKs deliveries instead of invoking the handler. Bounds overlap to already-in-flight handler invocations; it cannot revoke a handler that has started. |
| + Pull gating (`PullGatingEnabled`) | Checks ownership before issuing new pull requests, suppressing new iterator episodes on a revoked partition. Narrows, but does not close, the window. |

### The Irreducible Window

At the strongest tier one overlap window remains: a handler invocation already in flight on the old owner, plus AckWait-expiry redelivery of that **same message** through the shared per-partition durable to the new owner — whose gate correctly admits it. Mitigation: wrap long-running handlers with `consumer.NewWIPHandler`, which sends periodic in-progress signals so AckWait does not expire mid-handler.

The gate's ownership answer comes from a cached resolver. If the KV claim watcher stalls, a stale "I am the owner, state stable" answer can be served for up to `ReconcileInterval` (default 30s) before the periodic reconcile corrects it — a bounded, self-correcting window.

### When to Enable

Enable two-phase handoff (with a processing gate) when:
- Cross-worker duplicate processing must be minimized — it cannot be eliminated, so handlers should remain idempotent
- Partitions have stateful resources (connections, caches)
- Ordering disruption during rebalancing must be kept short

Keep disabled (default) when:
- Brief duplicate processing is acceptable
- Lower latency during rebalancing is preferred
- Simpler operational model is desired

---

## Degraded Mode

**Degraded mode** allows workers to continue processing with cached partition assignments when NATS connectivity is lost.

**Philosophy**: *"Stale but stable is better than fresh but broken"*

### How It Works

```
                    Normal Operation
                          │
          ┌───────────────┴───────────────┐
          │                               │
          ▼                               ▼
    ┌───────────┐                   ┌───────────┐
    │ NATS      │                   │ KV Errors │
    │ Disconnect│                   │ Threshold │
    └─────┬─────┘                   └─────┬─────┘
          │                               │
          │ EnterThreshold (10s)          │ KVErrorThreshold (5)
          │                               │
          └───────────────┬───────────────┘
                          │
                          ▼
                   ┌─────────────┐
                   │  DEGRADED   │
                   │    MODE     │
                   │             │
                   │ • Freeze    │
                   │   assignment│
                   │ • Continue  │
                   │   processing│
                   │ • Emit      │
                   │   alerts    │
                   └──────┬──────┘
                          │
                          │ NATS restored + ExitThreshold (5s)
                          │
                          ▼
                   ┌─────────────┐
                   │   STABLE    │
                   └─────────────┘
```

### Behavior

When a worker enters `StateDegraded`:

1. **Freezes Assignment**: Current partition assignment is locked
2. **Continues Processing**: Worker keeps processing assigned partitions
3. **Emits Alerts**: Escalating alerts based on duration:
   - 30s: Info level
   - 2m: Warn level
   - 5m: Error level
   - 10m: Critical level
4. **Ignores Updates**: No rebalancing or election participation

### Recovery

When NATS connectivity is restored for `ExitThreshold`:

1. Worker transitions back to `StateStable`
2. **Recovery Grace Period** starts (default: 15s)
3. During grace period, leader won't trigger emergency rebalance
4. Resumes normal election and assignment participation

**Terminal degraded reasons.** Some degraded entries cannot self-heal and the
worker stays `Degraded` permanently regardless of connectivity recovery.
`stream-missing-recovery-exhausted` is terminal: the dead partition-consumer
loop cannot restart in-process, so stream recreation alone does not exit
`Degraded`. The expected resolution is worker rotation (recreate the stream,
then rotate the pod). See the [Degraded Reason Taxonomy](OPERATIONS.md#degraded-reason-taxonomy) for
per-reason operator actions.

### Configuration

```go
cfg := &parti.Config{
    DegradedBehavior: parti.DegradedBehaviorConfig{
        EnterThreshold:      10 * time.Second,  // Time to enter degraded
        ExitThreshold:       5 * time.Second,   // Time to exit degraded
        KVErrorThreshold:    5,                  // Consecutive KV errors
        KVErrorWindow:       30 * time.Second,  // Error counting window
        RecoveryGracePeriod: 15 * time.Second,  // Post-recovery grace
    },
    DegradedAlert: parti.DegradedAlertConfig{
        InfoThreshold:     30 * time.Second,
        WarnThreshold:     2 * time.Minute,
        ErrorThreshold:    5 * time.Minute,
        CriticalThreshold: 10 * time.Minute,
        AlertInterval:     1 * time.Minute,
    },
}
```

### Monitoring Degraded Mode

Use `OnStateChanged` (and optionally `OnError`) hooks:

```go
hooks := &parti.Hooks{
    OnStateChanged: func(ctx context.Context, from, to parti.State) error {
        if to == parti.StateDegraded {
            alerting.Send(alerting.Warning, "Worker entered degraded mode")
        }
        if from == parti.StateDegraded && to != parti.StateDegraded {
            alerting.Send(alerting.Info, "Worker recovered from degraded mode")
        }
        return nil
    },
}
```

### Implications

**During Degraded Mode:**
- No new partition assignments
- No leadership changes
- No rebalancing
- Potential for stale assignments

**After Recovery:**
- Full resynchronization with current state
- Possible assignment changes based on actual worker count
- Grace period prevents false emergencies

See [Configuration Guide](CONFIGURATION.md#degraded-mode-configuration) for all options.

## Watcher Delete-Event Semantics

The three NATS KV watchers parti runs on each manager — assignment alias,
commit, and heartbeat — all intentionally ignore `KeyValueDelete` operations.

| Watcher | What a delete means | Why parti ignores it |
|---|---|---|
| Assignment alias (`assignment.<worker>`) | Legacy v2.3.0 alias removed during rolling upgrade | The commit log is now the authority; deleting the alias is not a reassignment primitive. The reconcile tick re-reads the key and treats a missing alias as a no-op. |
| Commit log (`commit.*`) | Old commit gc'd by the leader | A deleted commit does not erase the application's view of the world; the manager already holds its last applied snapshot. The reconcile tick treats a missing commit as a no-op for the same reason. |
| Worker heartbeat (`worker-hb.<worker>`) | Worker stopped or its lease expired | The TTL-driven absence is the load-bearing signal; the explicit delete is redundant with that path and the calculator's emergency detector already handles disappearances on the next poll. |

The consequence is that "delete a key and watch parti react" is not a supported
extension point. Operators wishing to force a reassignment should call
`Manager.TriggerRebalance` (leader-only) or restart the affected worker.

## Cold-Start Worker-Monitor Start Gap

When a leader bootstraps from cold start, the calculator runs an initial
rebalance against the workers it discovered during `discoverHighestVersion`,
seeds `lastWorkers` from that set, and only then starts the heartbeat
`WorkerMonitor`. The gap between the initial rebalance completing and the
monitor's first poll is intentionally short: workers added during the gap
are still picked up on the monitor's first poll (which fetches the fresh
heartbeat set), so the only observable effect is a brief delay before the
calculator notices a join that landed inside the boot window. There is no
correctness consequence — `currentWorkers` is re-fetched on every rebalance
trigger — but operators tracing the very first poll cycle may see one
"planned_scale" transition after a cold start even when the cluster appears
to have been stable. This is expected and self-resolves within one
`PollInterval`.
