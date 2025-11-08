# Design Doc: JetStream Consumer Scaling Strategy

## 1. Executive Summary

This document proposes a new strategy for managing NATS JetStream consumers within the `parti` library to improve scalability and reduce operational complexity.

The current implementation creates one durable consumer per partition, leading to thousands of consumers at scale (e.g., 2000 partitions = 2000 consumers). This places unnecessary load on the NATS server and complicates client-side logic.

The recommended approach is to refactor the subscription logic to maintain **one durable pull consumer per worker instance**. This single consumer will subscribe to multiple subjects using the `FilterSubjects` configuration. This change will reduce the total consumer count by over 90% (from 2000 to ~25-200), align with NATS best practices, and simplify the overall architecture.

## 2. Background

The `parti` library is designed to manage work partitioning for thousands of partitions. In the current implementation (`subscription/durable_helper.go`), each worker creates a dedicated durable pull consumer for every partition it is assigned.

-   **Partitions**: 2000
-   **Workers**: 25
-   **Partitions per Worker (avg)**: 80
-   **Consumers per Worker**: 80
-   **Total Consumers**: 2000

While this provides fine-grained control, it leads to a "consumer explosion" problem at scale, creating high resource utilization on the NATS server and significant management overhead.

## 3. Considered Approaches

Three approaches were considered to address this scaling challenge.

### Approach A: Multi-Stream Partitioning

This approach splits the single stream into multiple smaller streams (e.g., 100) using deterministic subject hashing.

-   **Publishing**: A message for `dc.tool1.complete` would be hashed and published to a specific stream, e.g., `notification_22`.
-   **Consumption**: The consumer logic would remain the same: one consumer per partition.
-   **Problem**: This does not solve the core issue. It merely distributes the 2000 consumers across 100 streams, while adding the massive operational overhead of managing 100 streams.

### Approach B: Hybrid Multi-Stream Model

This model combines multi-stream partitioning with a per-worker consumer strategy.

-   **Partitioning**: Same as Approach A (100 streams).
-   **Consumption**: A worker creates one consumer *per stream* it needs to subscribe to. If a worker is assigned 80 partitions that are spread across 50 different streams, it would create 50 consumers.
-   **Problem**: This model is the most complex. It still results in a high number of consumers (hundreds or thousands) and requires extremely complex reconciliation logic within the worker to manage a pool of consumers.

### Approach C: Single Stream, One Consumer per Worker (Recommended)

This approach maintains a single stream but fundamentally changes the consumer strategy.

-   **Architecture**: Each worker instance creates and manages exactly one durable pull consumer.
-   **Naming**: The consumer's durable name is derived directly from the worker's **stable ID** (e.g., `parti-worker-5`).
-   **Subscription**: The consumer uses the `FilterSubjects` configuration option to subscribe to all subjects corresponding to its assigned partitions.
-   **Rebalancing**: When partitions are reassigned, the worker simply updates its existing consumer's `FilterSubjects` list using the `CreateOrUpdateConsumer` API call.

## 4. Comparative Analysis

| Aspect | A: Multi-Stream | B: Hybrid Model | C: Single Consumer/Worker (Recommended) |
| :--- | :--- | :--- | :--- |
| **Total Streams** | High (e.g., 100) | High (e.g., 100) | **Low (1)** |
| **Total Consumers** | Very High (2000) | High (500-1500+) | **Low (~25-200)** |
| **NATS Server Load** | Very High | Medium-High | **Lowest** |
| **Operational Complexity** | Very High | High | **Lowest** |
| **Code Complexity** | Low (but flawed) | Very High | **Moderate** |
| **Resilience & Scaling** | Complex | Very Complex | **Simplest & Most Robust** |
| **Alignment with NATS Best Practices** | Anti-Pattern | Mixed | **Ideal** |

**Conclusion**: Approach C is superior across all key metrics. It directly addresses the consumer scaling problem without introducing unnecessary operational or code complexity.

## 5. Detailed Design for Recommended Approach

The implementation will focus on refactoring `subscription/durable_helper.go`.

### 5.1. Architecture

The core principle is a 1:1 mapping between a worker's stable ID and its durable consumer.

-   **Worker `worker-5`** ↔ **Durable Consumer `parti-worker-5`**

This consumer is the single entry point for all messages assigned to that worker.

### 5.2. Consumer Naming Convention

The consumer `Name` and `Durable` name will be identical, constructed as follows:

-   **Format**: `<ConsumerPrefix>-<WorkerID>`
-   **Example**: If `ConsumerPrefix` is "processor" and the worker's stable ID is "instance-123", the consumer name will be `processor-instance-123`.

### 5.3. Subscription Lifecycle Management

The `durable_helper` will no longer manage consumers per partition. Instead, it will manage one consumer per worker.

1.  **Startup / New Assignment**:
    -   A worker receives its list of assigned partitions.
    -   It generates the list of corresponding subjects.
    -   It calls a new function, e.g., `UpdateWorkerConsumer`, with its stable ID and the subject list.
    -   This function calls `jetstream.CreateOrUpdateConsumer` to create or update its single durable consumer with the new `FilterSubjects`.
    -   A single pull loop (`consumer.Messages()`) is started.

2.  **Scale-Up / Rebalance**:
    -   When a worker's partition assignment changes, it simply calls `UpdateWorkerConsumer` again with the new list of subjects. The existing consumer is updated atomically.

    **Two-Phase Handoff (Recommended)**:
    When a partition (subject) moves from worker A to worker B, perform an overlap to avoid delivery gaps:
    1. Leader publishes new assignment map.
    2. Worker B computes added subjects and updates its consumer first (adds new FilterSubjects).
    3. Worker B confirms pull loop receiving messages (optional lightweight health metric).
    4. Worker A then removes those subjects on its next diff cycle.
    5. Any in-flight unacked messages on A will redeliver (AckWait) and be consumed by B.
    This staggered add-then-remove sequence avoids transient “black holes” and preserves continuity during high churn.

    Pull Loop Continuity:
    - Consumer updates do not require restarting the pull loop. Maintain a single long-lived pull loop per worker. Updates to `FilterSubjects` take effect server-side while the loop continues.

3.  **Rolling Update / Restart**:
    -   A restarted worker reclaims its stable ID.
    -   It receives its partition assignment and calls `UpdateWorkerConsumer`.
    -   `CreateOrUpdateConsumer` finds the existing durable consumer by its stable name and reactivates it. Message processing resumes seamlessly.

4.  **Scale-Down**:
    -   When a worker is terminated, its consumer becomes idle.
    -   NATS will automatically clean up the inactive consumer after the `InactiveThreshold` is met. No explicit deletion is required.

### 5.4. `durable_helper.go` Refactoring Plan

-   **Removed**: Legacy `UpdateSubscriptions` (per-partition consumer management) and its associated tests are eliminated entirely—no deprecation layer or migration flag retained.
-   **New Primary Method** (handler provided at construction, not per update):
    ```go
    func (dh *DurableHelper) UpdateWorkerConsumer(
        ctx context.Context,
        workerID string,
        assignedPartitions []types.Partition,
    ) error
    ```
-   **Responsibilities**:
    1.  Generate deduped subject list from `assignedPartitions`.
    2.  Derive durable consumer name from `workerID` (cached after first use).
    3.  Build `jetstream.ConsumerConfig` (only `FilterSubjects` changes between updates; immutable fields preserved).
    4.  Call `CreateOrUpdateConsumer` with retry/backoff.
    5.  Maintain a single long-lived pull loop started once in helper constructor (not restarted on updates).
    6.  Diff new subject list against cached set; skip no-op updates and emit metrics.

#### Update Frequency & Stabilization
To avoid thrashing consumers during rapid topology changes:
- Integrate with existing stabilization windows (cold start vs planned scale operations).
- Coalesce multiple assignment changes within a window into a single consumer update.
- Enforce a minimum interval (e.g. 500ms–2s) between successive CreateOrUpdateConsumer calls per worker.
- Emit a metric (e.g. `parti_consumer_update_skipped_total`) for skipped no-op updates to verify diffing efficacy.
- Guardrail: hard cap on subjects per worker (configurable, default 500). If exceeded, log error and optionally split into secondary consumer (future extension).

#### Consumer Update Failure Recovery
- If `CreateOrUpdateConsumer` fails:
    - Keep the last-known-good consumer configuration active (continue pulling).
    - Retry updates with backoff (bounded retries with jitter).
    - Emit metrics and structured logs with the failed diff to aid diagnosis.
    - Escalate to the leader/manager via health signal if repeated failures exceed threshold.
    - Never tear down the pull loop solely due to update failure.

### 5.5. Partition Ownership Coordination (Manager)
Responsibility separation:
- The Manager is responsible for partition ownership (claim/release) using a coordination backend (e.g., NATS KV).
- The Helper remains a consumer orchestration component and should not implement claiming.

Interface-driven dependency:
- Introduce a small `PartitionClaimer` interface in the manager package and pass it into the helper (or supply claim decisions alongside assignments).

Example interface (conceptual):
```go
type PartitionClaimer interface {
        // Claim attempts to claim ownership of a subject on behalf of workerID.
        Claim(ctx context.Context, subject, workerID string) (ok bool, err error)
        // Release relinquishes ownership when worker no longer serves the subject.
        Release(ctx context.Context, subject, workerID string) error
        // IsOwner returns true if workerID is the current owner of subject.
        IsOwner(ctx context.Context, subject, workerID string) (bool, error)
}
```

Flow:
1. Manager computes assignment, claims subjects for the target worker(s).
2. Only after successful claims, Manager instructs each worker to update its consumer (add subjects).
3. After confirmation/health from the new worker, Manager instructs previous owner to remove subjects and release claims.
4. Helper trusts the Manager’s ownership decisions and focuses on consumer updates and pulling.

## 6. Benefits

1.  **Massive Scalability**: Reduces consumer count by >90%, significantly lowering NATS server load.
2.  **Operational Simplicity**: Eliminates the need for multi-stream workarounds, keeping the infrastructure simple with a single stream.
3.  **Increased Robustness**: Tightly couples the worker's lifecycle with its consumer via the stable ID, simplifying logic for scaling and restarts.
4.  **Reduced Complexity**: Replaces complex per-partition reconciliation logic with a simple, idempotent "update" call.
5.  **Best Practice Alignment**: Follows the recommended NATS pattern of one consumer per application instance.
6.  **Reduced Churn**: Diffed, coalesced updates minimize JetStream metadata operations.
7.  **Cleaner Observability**: One consumer per worker simplifies attribution of lag, redeliveries, and latency.

## 7. Risks and Mitigations

-   **Risk**: A single message handler processing messages from many partitions could be a bottleneck.
    -   **Mitigation**: This is a low risk. The handler can easily be made concurrent using a worker pool pattern if high-CPU processing is required per message. The I/O-bound pull loop itself is highly efficient.
-   **Risk**: A poison pill message on one subject could block processing for all subjects on that consumer.
    -   **Mitigation**: This risk exists in the current model as well. Proper error handling, use of `MaxDeliver`, and dead-letter queue (DLQ) configuration are the correct solutions and are unaffected by this design change.
-   **Risk**: High-frequency assignment changes could trigger excessive UpdateConsumer calls.
    -   **Mitigation**: Use stabilization windows and diff-based suppression; add rate limiting.
-   **Risk**: Subject list growth beyond intended scale (pathological partition explosion).
    -   **Mitigation**: Enforce configurable max subjects; surface loud metrics & logs; consider consumer sharding only if necessary.
-   **Risk**: Redelivery surge after rapid worker failure could temporarily inflate lag metrics.
    -   **Mitigation**: Size AckWait & MaxAckPending appropriately; monitor redelivery ratio and add backoff schedule.
-   **Risk**: Unintended immutable field changes during update (e.g. DeliverPolicy changes).
    -   **Mitigation**: Centralize ConsumerConfig builder; unit test to ensure immutable fields remain untouched on update path.

---

## 8. Observability & SLOs

Minimum metrics (per worker / consumer):
- `parti_consumer_messages_total` (counter)
- `parti_consumer_redeliveries_total` (counter)
- `parti_consumer_inflight` (gauge = pending + ack pending)
- `parti_consumer_update_duration_seconds` (histogram)
- `parti_consumer_subject_count` (gauge)
- `parti_handler_latency_seconds` (histogram: p50/p95/p99)
- `parti_consumer_update_failures_total`

Candidate alerts:
- Redelivery ratio > X% over 5m.
- Subject count near cap (>90%).
- Update failure rate spike (>5% in 10m).
- Inflight backlog sustained above threshold (lag formation).

SLO Examples:
- 99% of messages processed < 2 × p95 handler latency.
- Redeliveries < 3% normal operation.
- Consumer update success rate ≥ 99.9%.

## Message Attribution

With a single consumer per worker serving multiple subjects, attribution is performed by parsing the message subject. Handlers should extract the partition identity from the subject (e.g., from `dc.{tool}.{chamber}.completion`) and include it in logs/metrics to enable per-partition visibility and troubleshooting.

Pattern:
```go
type MessageContext struct {
    Subject     string
    PartitionID string // derived from subject tokens
    WorkerID    string
    ReceivedAt  time.Time
}
```

Maintain idempotency in handlers to tolerate redeliveries.

## 9. Migration Plan (Removed)

No phased migration required—legacy per-partition path and `UpdateSubscriptions` are removed outright. Systems adopting this version use the single-consumer model exclusively. Rollback would require reverting to a previous library version; no in-place dual-mode is supported.

## 10. Testing Plan

Unit Tests:
- Subject diffing (add/remove/no-op cases; deduplication).
- Immutable field preservation on update path.
- Guardrail on max subjects triggers expected error/log.

Integration (embedded NATS):
- End-to-end single consumer receiving multiple subjects; publish messages and assert delivery and ack.
- Rebalance test: apply two-phase handoff; assert no lost messages.
- Rolling restart: reclaim stable ID; confirm consumer continuity.
- High churn scenario: rapid assignment changes coalesced; update suppression metrics increment.

Performance:
- 25 workers × 80 subjects; drive synthetic load at 10k–50k msg/s; record CPU, memory, p99 latency, redelivery ratio.

Failure Injection:
- Simulate NATS outage (connection drop) during update; ensure retry & backoff.
- Inject handler panic; confirm redelivery + MaxDeliver behavior.

Additional Scenarios:
- Consumer update during active processing (ensure no missed messages).
- Rapid scale cycles (25 → 100 → 25 workers in <1 minute) to validate suppression logic.
- Subject cap breach (add beyond limit) returns explicit error.
- NATS server restart mid-rebalance; consumers recover without manual intervention.
- Long-running soak (24h) ensures no memory growth / goroutine leaks.

## 11. Performance & Tuning

Recommended starter ConsumerConfig tuning:
- `AckWait`: 30s (adjust to 3 × p95 handler).
- `MaxAckPending`: 500 (increase if concurrency supports it; watch memory).
- `MaxWaiting`: 256.
- `MaxDeliver`: 3 with optional exponential `BackOff` (e.g. 2s, 5s, 15s).

Pull Strategy:
- Use `Fetch(batch)` with adaptive batch size (e.g. start 50, scale up/down based on handler throughput).
- Optional switch to `FetchBytes` for large payload variance.

Concurrency:
- Single pull loop + worker pool for handler execution (bounded queue size = MaxAckPending).
- Backpressure: stop pulling when queued work > threshold (e.g. 80% of MaxAckPending).

## 12. Edge Cases & Limits

Edge Cases:
- Partition explosion: enforce max subjects; consider sharding consumer only if truly necessary.
- Duplicate subject generation: dedupe before update; log anomaly metric.
- Large reassign wave (scale-down of many workers): may trigger redelivery storm; monitor and temporarily increase pull batch.
- Handler slowdowns: detect via latency histogram; auto-reduce batch size to prevent backlog.

## 13. API Contract Summary (`UpdateWorkerConsumer`)

Inputs:
- `workerID string` (stable ID; must match durable naming convention)
- `assigned []types.Partition`

Behavior:
- Build deduped subject set.
- Diff with previous set; if unchanged → no-op (emit noop metric).
- Create or update consumer (Name = Durable = `<prefix>-<workerID>`).
- Pull loop is persistent (started at helper construction; not restarted here).

Error Modes:
- Context canceled; NATS connectivity error; subject cap exceeded; consumer update failure after retries.

Metrics Emitted:
- Update success/failure, duration, subject count, no-op skipped updates, diff add/remove counts.

## 14. Open Questions & Next Steps

Questions:
- Do we need consumer sharding earlier (e.g., beyond 500 subjects)?
- Should batch size tuning be adaptive based on moving average handler latency?
- Introduce optional wildcard compression if subject taxonomy allows grouping?

Next Steps:
1. Implement diff engine & new helper API.
2. Add metrics instrumentation.
3. Shadow deployment under feature flag.
4. Run performance + failure injection tests.
5. Finalize SLO thresholds; populate dashboards & alerts.
6. Remove legacy per-partition code after successful validation.

## 15. Manager Claim Flow (Draft)

Purpose: Centralize ownership decisions in the Manager, perform two-phase handoff, and provide the helper with a pre-claimed assignment set per worker.

Ownership record (stored in KV or similar):
```json
Key:  parti.claims.<subject>
Value: {
  "owner": "worker-5",
  "fenceToken": "<uuid-or-inc-counter>",
  "claimedAt": "RFC3339",
  "ttlSeconds": 60
}
```

High-level algorithm:
1) Compute desired assignment from current topology.
2) For each subject that changes owner:
   - Attempt Claim(subject, newOwner, fenceToken).
   - On success, stage subject in newOwner.added.
   - On failure, keep subject with current owner; re-evaluate next cycle.
3) Notify new owners to UpdateWorkerConsumer with staged added subjects (plus their unchanged subjects).
4) Wait for health confirmation from new owners (e.g., pulling=true, subject visible metric) or bounded timeout.
5) Notify old owners to UpdateWorkerConsumer to remove the transferred subjects.
6) Release(subject, oldOwner, fenceToken) to clear claim.
7) Publish final assignment snapshot/version.

Sketch in Go (simplified):
```go
type ClaimStore interface {
    Claim(ctx context.Context, subject, owner, fence string, ttl time.Duration) (bool, error)
    Release(ctx context.Context, subject, owner, fence string) error
}

type WorkerPlan struct {
    WorkerID   string
    Add        []types.Partition
    Keep       []types.Partition
    Remove     []types.Partition
}

func (m *Manager) Reconcile(ctx context.Context, desired map[string][]types.Partition) error {
    current := m.snapshotAssignments()           // workerID -> partitions (owned)
    plans := make(map[string]*WorkerPlan)        // workerID -> plan

    // 1) Claim new ownerships
    for workerID, target := range desired {
        for _, p := range target {
            subj := toSubject(p)
            oldOwner := m.lookupOwner(subj)
            if oldOwner == workerID {
                plans[workerID] = addKeep(plans[workerID], workerID, p)
                continue
            }
            ok, err := m.claimStore.Claim(ctx, subj, workerID, m.fenceToken, m.claimTTL)
            if err != nil || !ok {
                // keep at old owner for now
                if oldOwner != "" {
                    plans[oldOwner] = addKeep(plans[oldOwner], oldOwner, p)
                }
                continue
            }
            plans[workerID] = addAdd(plans[workerID], workerID, p)
        }
    }

    // 2) Notify new owners to add (two-phase handoff: add first)
    for workerID, plan := range plans {
        parts := append(plan.Keep, plan.Add...)
        _ = m.helper.UpdateWorkerConsumer(ctx, workerID, parts, m.handler)
    }

    // 3) Wait for health/visibility signal (bounded)
    m.waitForNewOwners(ctx, plans)

    // 4) Compute removals for previous owners
    for subj, oldOwner := range m.ownersBySubject() {
        if newOwner := m.lookupDesiredOwner(subj, desired); newOwner != oldOwner && newOwner != "" {
            p := toPartition(subj)
            plans[oldOwner] = addRemove(plans[oldOwner], oldOwner, p)
        }
    }

    // 5) Notify old owners to remove
    for workerID, plan := range plans {
        if len(plan.Remove) == 0 {
            continue
        }
        parts := subtract(plans[workerID].Keep, plan.Remove)
        parts = append(parts, plan.Add...) // ensure adds remain
        _ = m.helper.UpdateWorkerConsumer(ctx, workerID, parts, m.handler)
    }

    // 6) Release claims for removed subjects
    for workerID, plan := range plans {
        for _, p := range plan.Remove {
            _ = m.claimStore.Release(ctx, toSubject(p), workerID, m.fenceToken)
        }
    }

    // 7) Publish final assignment (versioned)
    return m.publishAssignmentSnapshot(ctx)
}
```

Notes:
- The Manager is the sole authority for claim/release and for two-phase timing.
- Health confirmation can be a simple metric/heartbeat indicating the new worker is pulling for at least one message cycle.
- Fencing (monotonic counter or UUID) prevents stale owners from overwriting fresh claims.

## 16. Helper API: UpdateWorkerConsumer (Skeleton)

Goal: Accept a pre-claimed set of partitions, compute subject set, perform diffed CreateOrUpdateConsumer, and maintain a single long-lived pull loop.

Skeleton:
```go
// UpdateWorkerConsumer applies the full, pre-claimed assignment set for this worker.
// It performs a diff against the last applied set and updates the single durable consumer
// (Name == Durable == <prefix>-<workerID>) using FilterSubjects. No-op if unchanged.
func (dh *DurableHelper) UpdateWorkerConsumer(
    ctx context.Context,
    workerID string,
    partitions []types.Partition,
) error {
    // 1) Build deduped subject list from partitions
    subjects := dh.buildSubjects(partitions)

    // 2) Diff with previous (cached) subject set for worker
    if dh.noChange(workerID, subjects) {
        dh.metrics.IncNoop(workerID)
        return nil
    }

    // 3) Build ConsumerConfig (immutable fields preserved)
    cfg := jetstream.ConsumerConfig{
        Name:              dh.consumerName(workerID),
        Durable:           dh.consumerName(workerID),
        FilterSubjects:    subjects,
        AckPolicy:         dh.config.AckPolicy,
        AckWait:           dh.config.AckWait,
        MaxDeliver:        dh.config.MaxDeliver,
        InactiveThreshold: dh.config.InactiveThreshold,
        MaxWaiting:        dh.config.MaxWaiting,
        MaxAckPending:     /* optional */ 0,
    }

    // 4) CreateOrUpdateConsumer with retry/backoff
    if err := dh.applyConsumerUpdate(ctx, cfg); err != nil {
        dh.metrics.IncUpdateFailure(workerID)
        dh.logger.Error("consumer update failed", "worker", workerID, "error", err)
        return err
    }

    // 5) Ensure single pull loop is running (idempotent start)
    // Pull loop already running (started in constructor); ensure health but do not restart.
    dh.ensurePullLoopHealth(workerID)

    // 6) Cache applied subject set for future diffs
    dh.cacheSubjects(workerID, subjects)

    return nil
}
```

Key points:
- No claim operations in the helper—ownership is guaranteed by the manager.
- Pull loop is not restarted on updates; only consumer config changes.
- Diffing avoids unnecessary updates and thrash; emit metrics for skipped updates.

## 17. Implementation Steps

1) Manager
- Implement ClaimStore (backed by NATS KV) with fencing and TTL.
- Add reconciliation loop to compute desired vs current, claim, and two-phase handoff.
- Emit health confirmation signals from workers (e.g., consumer-visible-subject metric) and wait bounded time.
- Publish versioned assignment snapshots.

2) Helper (subscription)
- Add new `UpdateWorkerConsumer` entrypoint with subject building, diffing, and CreateOrUpdateConsumer application.
- Ensure a single durable consumer per worker (Name == Durable == `<prefix>-<workerID>`).
- Maintain a single long-lived pull loop with Messages() iterator and heartbeat.
- Add metrics: update attempts/success/failure, noop diffs, subject count, pull loop health.

3) Wiring & Migration
- Gate new flow with a feature flag (UnifiedConsumerEnabled).
- In shadow mode, call UpdateWorkerConsumer while keeping legacy per-partition path dormant.
- Validate parity, then drain and remove legacy path; keep rollback toggle.

4) Tests
- Unit: diffing logic, immutable config preservation, backoff retries on update.
- Integration: two-phase handoff end-to-end; rolling restart with stable ID; server restart mid-rebalance.
- Performance: 25×80 subjects load, adaptive tuning validation, redelivery/loss checks.

5) Dashboards/Alerts
- Per-worker consumer health: lag, inflight, redelivery ratio, subject count.
- Update failure rate, update duration, noop diff counts.
- Claim failure rate and time-to-handoff.

## 18. Manager ↔ Helper relationship & notification path

Intent: Keep Manager decoupled from subscription details while allowing it to drive the helper updates immediately after reconciliation/claiming.

Separation of concerns:
- Manager (root package): authority for ID, election, assignment, and (new) partition claims; publishes per-worker assignment snapshots and exposes hooks.
- Helper (subscription subpackage): owns JetStream consumer lifecycle for a worker; applies subject diffs via `UpdateWorkerConsumer`; maintains the pull loop.

Why decouple: Root package must not depend on `subscription` to avoid import cycles. The clean boundary is an application-provided hook that bridges Manager events to Helper calls.

Notification flow (per cycle):
1) Leader Manager reconciles, claims subjects, and writes a new assignment snapshot for each worker (phase 1: new owners include additions; phase 2: old owners receive removals after health check).
2) Each worker’s Manager instance watches its own `assignment.<worker>` key and triggers `OnAssignmentChanged(ctx, old, new)` when version increases.
3) Application wiring calls `helper.UpdateWorkerConsumer(ctx, manager.WorkerID(), new, handler)` from inside the hook. No RPC is required; this is in-process.
4) Helper diffs subjects and updates the single durable consumer in-place; pull loop continues without restart.

Minimal wiring example:
```go
// At startup
dh := subscription.NewDurableHelper(js, durableConfig, logger, metrics)
msgHandler := subscription.MessageHandlerFunc(func(m *jetstream.Msg) error { /* ... */ return nil })

hooks := parti.NewHooks()
hooks.OnAssignmentChanged = func(ctx context.Context, old, new []parti.Partition) error {
        // Single call with full, pre-claimed set for this worker
        return dh.UpdateWorkerConsumer(ctx, mgr.WorkerID(), new, msgHandler)
}

mgr, err := parti.NewManager(cfg, conn, src, strat, parti.WithHooks(&hooks))
```

Contract surface:
- Input: workerID from Manager, partitions from `OnAssignmentChanged` (already pre-claimed by the leader), and the worker’s message handler.
- Behavior: Helper performs idempotent diffed update; does not claim; never restarts the pull loop on subject-only changes.
- Error handling: Hook returns error; Manager logs via existing hook error path; retry is handled by the next assignment version or by operator (optional exponential backoff wrapper can be added in app code).

Two-phase handoff with this wiring:
- Phase 1 (add): Leader publishes assignments where new owners include the moved subjects. Hook on new owners calls `UpdateWorkerConsumer` to add subjects.
- Health gate: Leader observes new owners’ health signals (pulling/lag OK) before proceeding.
- Phase 2 (remove): Leader publishes assignments where old owners exclude the moved subjects. Hook on old owners calls `UpdateWorkerConsumer` to remove subjects. Leader then releases claims.

Alternative (interface-based) wiring:
- Define a narrow `WorkerConsumerUpdater` interface in root (no subpackage references):
    ```go
    type WorkerConsumerUpdater interface {
            UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []parti.Partition) error
    }
    ```
- Provide an Option `WithConsumerUpdater(WorkerConsumerUpdater)` to register an internal hook that invokes it.
- The `subscription.DurableHelper` can implement this interface without import cycles (dependency is from subscription → root only).

### 18.1 Interface Option: WithConsumerUpdater (Detailed Design)

Goal: Allow applications to supply any implementation (typically `subscription.DurableHelper`) that handles consumer updates, without manually wiring a hook every time. Keeps Manager ignorant of subscription details; avoids import cycles.

Interface (in root package `parti`):
```go
// WorkerConsumerUpdater applies a new full assignment set for the given worker.
// Implementation must be idempotent and SHOULD diff internally to avoid unnecessary JetStream updates.
type WorkerConsumerUpdater interface {
    UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []parti.Partition) error
}
```

Option helper:
```go
// WithConsumerUpdater registers a WorkerConsumerUpdater so Manager automatically
// calls it when assignments change. If a custom OnAssignmentChanged hook already
// exists, it will be composed (original runs first; errors are logged then updater invoked).
func WithConsumerUpdater(updater WorkerConsumerUpdater) Option {
    return func(mo *managerOptions) {
        // Ensure hooks struct exists
        if mo.hooks == nil {
            h := hooks.NewNop()
            mo.hooks = &h
        }
        prev := mo.hooks.OnAssignmentChanged
        mo.hooks.OnAssignmentChanged = func(ctx context.Context, oldParts, newParts []parti.Partition) error {
            // previous user hook
            if prev != nil {
                if err := prev(ctx, oldParts, newParts); err != nil {
                    // Manager will log hook errors; continue to updater
                }
            }
            // Obtain workerID via context injection OR (preferred) extend manager invocation
            // so it sets a value in context: ctx = context.WithValue(ctx, parti.WorkerIDContextKey, mgr.WorkerID()).
            // Interim design: Updater requires workerID to be part of context.
            workerID, _ := ctx.Value(parti.WorkerIDContextKey).(string)
            return updater.UpdateWorkerConsumer(ctx, workerID, newParts)
        }
    }
}
```

Context propagation:
- Introduce `WorkerIDContextKey` (unexported type + exported variable) to inject workerID when Manager invokes `OnAssignmentChanged`.
- Alternative: Add a new hook `OnAssignmentChangedWithID(ctx, workerID, old, new)`; if present, Manager prefers it over the legacy one. (Backward-compatible upgrade path.)

Backward compatibility strategy:
1. Phase A: Add context workerID injection + Option; keep existing hook signature.
2. Phase B: Introduce new hook variant; `WithConsumerUpdater` adapts to whichever is available.
3. Phase C: Deprecate old hook once ecosystem updates.

Error handling semantics:
- Updater errors are surfaced via hook error logging already present in Manager (non-fatal, next assignment version retried automatically).
- Applications wanting retries can wrap their updater in a decorator implementing exponential backoff before returning an error.

Metrics integration:
- Manager increments assignment change metrics; updater implementation (e.g., DurableHelper) records update attempt, success/failure, and noop diff metrics.

Edge cases:
- Empty partition set: Updater should update consumer with zero subjects (or optionally keep previous until remove phase completes if two-phase handoff is active).
- Rapid consecutive versions: Updater MUST be safe to receive version N+2 before finishing N+1 (idempotent diff guarantees correctness).
- WorkerID resolution failure (missing context value): Updater returns sentinel error; Manager logs; next version reattempt.

Testing approach:
- Unit test Option composition: existing hook + updater both invoked.
- Inject fake context without workerID → expect error path and log.
- Benchmark path with 0-change (noop) updates to ensure minimal overhead.

Migration note: Initial implementation can bypass context key complexity by temporarily adding workerID as a parameter to the hook invocation; update design doc if that direction chosen.
