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

-   **Deprecate/Remove**: The current `UpdateSubscriptions` logic that iterates through `added` and `removed` partitions to create/stop per-partition consumers will be removed. The internal tracking map `consumers` (mapping partition ID to consumer) will also be removed.
-   **Create**: A new primary method will be introduced, conceptually like this:
    ```go
    func (dh *DurableHelper) UpdateWorkerConsumer(
        ctx context.Context,
        workerID string, // The stable ID of this worker
        assignedPartitions []types.Partition,
        handler MessageHandler,
    ) error
    ```
-   **Modify**: This new function will be responsible for:
    1.  Generating a list of subjects from `assignedPartitions`.
    2.  Constructing the durable consumer name from `workerID`.
    3.  Creating a `jetstream.ConsumerConfig` with the `FilterSubjects` field populated.
    4.  Calling `stream.CreateOrUpdateConsumer(ctx, cfg)`.
    5.  Managing a single pull loop for the worker's consumer.
    6.  Performing diff-based updates only when the subject set truly changes (idempotent behavior).

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

## 9. Migration Plan

Phased rollout from per-partition to per-worker consumer:
1. **Flag Introduction**: Add config flag `UnifiedConsumerEnabled`.
2. **Shadow Mode**: Create per-worker consumer; keep per-partition consumers but stop pulling from them (do not delete yet).
3. **Validation**: Compare message counts & lag; ensure parity.
4. **Drain & Cleanup**: Stop pull loops for partition consumers; delete them explicitly or let inactivity TTL reap them.
5. **Full Switch**: Enable flag cluster-wide; remove legacy code paths.
6. **Post-Monitoring**: Watch redelivery, backlog, update failure metrics for regression.

Rollback Strategy: Re-enable legacy path if redelivery ratio or lag breaches SLO for N consecutive windows.

Zero-Downtime Variant (Optional):
1. Dual mode: run unified consumer alongside legacy per-partition consumers; deduplicate via message ID hash (subject + sequence).
2. Compare delivery parity metrics for a burn-in window (e.g., 1–2 days).
3. Disable legacy pull loops after confidence threshold met.
4. Delete legacy consumers after 2 × InactiveThreshold.

Network Partition Resilience:
- Ensure `InactiveThreshold` > maximum expected transient network outage.
- Worker heartbeats TTL shorter than consumer inactivity threshold to detect liveness sooner.
- On reconnect, validate consumer existence via `Consumer.Info`; recreate if missing.
- Partition claiming (manager) should re-verify ownership after reconnect; fence stale owners by comparing claim timestamps.

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
- `workerID string`
- `assigned []types.Partition`
- `handler MessageHandler`

Behavior:
- Build deduped subject set.
- Diff with previous set; if unchanged → no-op.
- Create or update consumer (Name = Durable = `<prefix>-<workerID>`).
- Ensure pull loop running; adjust batch size heuristics.

Error Modes:
- Context canceled; NATS connectivity error; subject cap exceeded.

Metrics Emitted:
- Update success/failure, duration, subject count, no-op skipped updates.

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
