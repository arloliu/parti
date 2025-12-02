# Parti Simulation Review & Design Discussion

## 1. What indicates message loss?

In the context of `parti`, message loss is defined as a **permanent gap in the monotonic sequence of messages for a specific partition**.

The simulation detects this via the `Coordinator` and `MessageTracker`:
*   **Detection Mechanism:** The producer attaches a monotonically increasing `PartitionSequence` (1, 2, 3...) to every message sent to a specific partition.
*   **Gap Identification:** The `MessageTracker` maintains a `lastReceivedPerPartition` counter. If it receives sequence `N` when expecting `M` (where `N > M`), it records a "hole" for sequences `M` through `N-1`.
*   **Loss Confirmation:** A hole becomes a "Gap" (confirmed loss) if:
    1.  It is not filled within a reasonable time window (out-of-order delivery).
    2.  The simulation ends and the hole remains.
    3.  The `AgeOut` mechanism escalates the hole after a timeout.

**Critical Indicators:**
*   `GapCount > 0` in the final report.
*   `PendingHoles > 0` at the end of a drain period.
*   `ErrMessageGap` logged by the Coordinator.

## 2. Key Acceptance Criteria

To certify `parti` as production-ready, the simulation must demonstrate the following behaviors under stress:

### Functional Correctness (The "Must Haves")
1.  **Zero Message Loss:** `GapCount == 0` after all producers stop and workers drain.
2.  **Exactly-Once / At-Least-Once Delivery:**
    *   Ideally `DuplicateCount == 0`.
    *   Acceptable: `DuplicateCount > 0` *only* during rebalancing events (at-least-once), provided the application can handle idempotency. `parti` claims "stable worker IDs" and "leader-based assignment" to minimize this, so high duplicates indicate a flaw in the handoff protocol.
3.  **Full Coverage:** `UnassignedPartitions == 0` during steady state. Every partition must have an owner.

### Operational Quality (The "Should Haves")
4.  **Stable Assignment:** `LocalityRatio > 0.8` (or similar threshold). When a new worker joins, existing workers should not shuffle partitions amongst themselves unnecessarily.
5.  **Fast Convergence:** `Avg Rebalance Duration` should be within low seconds (e.g., < 5s) even with 100+ workers.
6.  **Resilience:** The system must recover (return to zero unassigned, zero gaps) after:
    *   Worker crash/restart.
    *   Leader failure.
    *   Network partition (simulated).

## 3. How to capture real errors?

The current simulation prints a summary, but for an SDET workflow, we need actionable artifacts when a failure occurs.

### Current State
*   Logs to stdout.
*   Prints a summary report at the end.
*   Prometheus metrics (if enabled).

### Recommendations for Improvement
1.  **Stop-On-Failure Mode:** Add a flag `--stop-on-error` to immediately halt the simulation when a Gap is confirmed. This preserves the state (logs, NATS queues) for investigation.
2.  **Failure Artifact Dump:** On failure, the simulation should write a `failure_report_<timestamp>.json` containing:
    *   The specific Partition ID and Sequence Number of the gap.
    *   The `WorkerID` that *should* have owned that partition at that time (derived from the assignment history).
    *   The `ProducerID` that sent the missing message.
    *   A timeline of events (rebalances, crashes) leading up to the gap.
3.  **Gap Timeline Visualization:** Generate a simple HTML/text timeline showing:
    *   T=0: Msg 100 sent to P1.
    *   T=1: Worker A crashes.
    *   T=2: Rebalance starts.
    *   T=3: Msg 101 sent to P1.
    *   T=4: Worker B assigned P1.
    *   T=5: Msg 101 received by B.
    *   *Result: Msg 100 lost?*
4.  **Log Correlation:** Ensure all logs (Producer, Worker, Coordinator) share a synchronized timestamp and correlation IDs (PartitionID is a good candidate) to grep the life of a missing message.

## 4. SDET / Black Box Perspective

As an SDET treating `parti` as a black box, I don't care about the internal `hashring` implementation or `raft` leader election details. I care about the **contract**:

> "If I send a message to Partition X, *someone* in the cluster will process it exactly once, eventually."

### Test Strategy
1.  **Boundary Testing:**
    *   **Scale:** 1 partition, 1 worker -> 10k partitions, 500 workers.
    *   **Throughput:** 1 msg/sec -> 100k msg/sec (saturate the workers).
    *   **Payload:** Empty messages, massive messages (1MB+).

2.  **Chaos / Destructive Testing:**
    *   **The "Kill -9" Test:** Randomly `kill -9` worker processes. Does the cluster recover?
    *   **The "Split Brain" Test:** Isolate the Leader from the rest of the cluster. Does a new leader emerge? Do assignments stabilize?
    *   **The "Slow Neighbor" Test:** One worker is paused (SIGSTOP) for 10 seconds. Does the group eject it? When it returns (SIGCONT), does it recover gracefully?

3.  **Observability as a Feature:**
    *   The simulation *is* the test harness. It needs to be runnable in CI/CD.
    *   **Action:** Create a `ci-stress.sh` script that runs the simulation with a specific "flaky" config (high chaos) for 1 hour and asserts exit code 0.

## Action Plan

### Phase 1: Hardening the Simulation (Immediate)
- [x] **Implement `Stop-On-Failure`:** Allow the coordinator to signal a global shutdown immediately upon detecting a confirmed gap.
- [x] **Structured Failure Reporting:** Write a JSON report on exit if failures occurred.
- [x] **CI Integration:** Create a GitHub Action workflow that runs a 15-minute chaos simulation on every PR to `main`.

### Phase 2: Advanced Scenarios (Short Term)
- [x] **Chaos Primitives:** Implement `WorkerPause` (Slow Consumer) and `NetworkDisconnect` (Network Partition) capabilities.
- [x] **Chaos Integration:** Integrate chaos scenarios into the main simulation loop (`ChaosController`).
- [x] **Network Partition Simulation:** Use the `NetworkDisconnect` primitive to simulate network isolation between workers during the simulation.
- [x] **Slow Consumer Simulation:** Use the `WorkerPause` primitive to inject artificial latency into specific workers.

### Phase 3: Analysis Tools (Long Term)
- [x] **Trace Visualizer:** A tool to ingest the `failure_report.json` and visualize the timeline of the lost message.
