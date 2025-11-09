# Production Readiness Analysis: Single Consumer per Worker Refactor

This report evaluates the updated, implemented refactor to a single durable JetStream consumer per worker (multi-subject via `FilterSubjects`) and prioritizes follow-ups required to reach strong production readiness. It is grounded in the implementation status & roadmap at `docs/design/06-implementation/single-consumer-implementation-status-and-roadmap.md` and the current repo state.

## Executive summary

The refactor successfully reduces churn, simplifies lifecycle management, and improves cache affinity by consolidating per-partition consumers into a single durable consumer per worker. The core path is implemented and tested, with lazy start semantics, idempotent updates, and integration with the Manager via `WithWorkerConsumerUpdater`. The design direction is sound for production use, but several resilience and operability gaps remain. The most critical gaps are around automatic recovery (consumer deletion, heartbeat loss), bounded behavior under large subject sets or rapid update bursts, and first-class observability/metrics.

Short list of P0 (must have before broad production):
- Automatic detection and recovery when the worker consumer is externally deleted or becomes unusable
- Jittered exponential backoff for all control-plane API calls (create/update/iterate) to avoid coordinated retry storms
- Guardrails: upper bound on subject set (`MaxSubjects`) and workerID immutability after first successful update
- Metrics and health signals sufficient for SLOs and on-call triage
- Stress tests for rapid consecutive updates and heartbeat-induced iterator recreation; flake detector must pass

With these addressed, the refactor will be materially production-ready for general workloads.

## What changed vs original plan (brief)

- Implemented: single long-lived pull loop; immutable handler injection; `UpdateWorkerConsumer` with deterministic subject ordering and no-op diff; Manager integration via `WithWorkerConsumerUpdater`; examples updated.
- Not implemented (deferred): eager start; separate abstractions for `applyConsumerUpdate` and `ensurePullLoopHealth` (logic inlined); adaptive batch sizing; automated consumer recreation; internal debounce/coalescing; full metrics suite.

## Verification status (current)

- Unit + integration tests cover lifecycle, no-op diffs, and Manager-driven updates; pull loop stability is validated implicitly.
- Gaps noted: no tests yet for external consumer deletion, rapid update stress, heartbeat iterator recreation.
- A recent flake detection run failed (exit 1); production readiness requires that flake detection consistently passes, particularly around timing-sensitive areas (heartbeats/iterator recovery and rapid updates).

## Production readiness assessment

### 1) Correctness and data semantics

- Deterministic subject set computation with dedupe + sort eliminates ordering churn. Good.
- Empty assignment behavior relies on server’s `FilterSubjects` semantics. Risk if the server treats empty filters unexpectedly.
- WorkerID is part of consumer identity; mutation mid-run silently overwrites state. This should be guarded.

Recommendations:
- Define and test empty-assignment semantics explicitly (acceptance test against embedded NATS).
- Add a guard to reject workerID change after first successful update unless explicitly allowed via config.

### 2) Reliability and resilience

- Current retries use a fixed linear backoff without jitter; this risks retry storms and thundering herds.
- No automatic recovery if the consumer is externally deleted or becomes inconsistent; recovery only happens on a subsequent update.
- Heartbeat/iterator failures are logged and reconstructed at the iterator-level but lack escalation and a bounded recovery strategy.

Recommendations (P0):
- Use jittered exponential backoff (decorrelated jitter) for control-plane operations (create/update/info calls and iterator restarts).
- Implement consumer existence check and auto-recreation path on “not found”/terminal iterator errors with a capped retry budget and metrics.
- Add failure escalation path: after N iterator restarts within T, force a consumer refresh (re-apply last subject set) and surface a health signal.

### 3) Performance and scalability

- Consolidation to a single consumer should reduce server resource usage and client churn.
- No explicit subject count cap; large assignments could cause large filter lists and control-plane bloat.
- Adaptive batch sizing is not yet implemented; acceptable for MVP but leaves throughput unoptimized under skew.

Recommendations:
- Add `MaxSubjects` config with a sane default and clear error; emit a warning metric when near the threshold.
- Document and (optionally) expose batch/prefetch parameters for pull loops; plan adaptive tuning as P1.

### 4) Operability and observability

- Logging is present; `WorkerSubjects()` and `WorkerConsumerInfo(ctx)` provide introspection but no metrics.
- Lacking core metrics for updates, latency, iterator restarts, and health. On-call triage would be impaired.

Recommendations (P0):
- Implement the metrics suite proposed in the plan:
  - `parti_worker_consumer_updates_total{result=success|failure|noop}`
  - `parti_worker_consumer_update_latency_seconds` (histogram)
  - `parti_worker_consumer_subjects_current` (gauge)
  - `parti_worker_consumer_subject_changes_total{type=add|remove}`
  - `parti_worker_consumer_recreations_total` (auto-recovery events)
  - Iterator restart counters and consecutive failure gauges
- Expose a health/readiness check accessor that reflects iterator health and recent failures.

### 5) Safety/guardrails

- No debounce/coalescing: rapid assignment churn could produce control-plane load spikes.
- No hard limits: potential OOM/log flooding if a misconfigured source pushes large subject diffs.

Recommendations:
- Add a configurable coalescing window (50–100ms) with tracing to show when updates were coalesced.
- Enforce caps (`MaxSubjects`, max update rate) and emit warning metrics when approaching limits.

### 6) Compatibility and API contracts

- Public contracts are clear: `NewDurableHelper`, `UpdateWorkerConsumer`, `WorkerConsumerUpdater`, `WithWorkerConsumerUpdater`.
- Ensure docs (godoc) follow the project’s standardized comment format for all exported items and include examples.

### 7) Rollout, failure domains, and recovery

- Lazy start prevents consumer creation without stable workerID; good for correctness.
- Missing two-phase handoff and claim store are out of scope, but note they’re valuable for zero-loss cutovers during leader/assignment changes.

Recommendations:
- Define a progressive rollout plan (canary → 10% → 50% → 100%) with metrics-based promotion and rollback triggers.
- When two-phase handoff lands, ensure compatibility with single-consumer updater semantics.

## Prioritized follow-ups (P0/P1/P2)

P0 — Blockers for broad production
1. Auto-recovery for missing/invalid consumer
   - Detect `Consumer Not Found`/terminal iterator errors and re-create from last known subjects with capped, jittered exponential retries.
   - Metric: `parti_worker_consumer_recreations_total`, `*_recreation_failures_total`.
   - Health: expose unhealthy if recreation fails N times in T.
2. Retry policy overhaul with jittered exponential backoff
   - Apply to create/update/info and iterator restart paths; configurable ceilings.
3. Guardrails and validation
   - Enforce `MaxSubjects` with clear error and metric; reject workerID mutation after first success (configurable escape hatch).
4. Observability baseline
   - Implement core metrics listed above; add iterator restart counters and recent-failure gauges.
   - Add a `DurableHelper.Health()` accessor with recent error state for readiness checks.
5. Test hardening & flake elimination
   - Add stress tests for rapid consecutive updates (with coalescing off/on) and heartbeat-induced iterator recreation.
   - Ensure `scripts/flake_detector.sh` passes consistently (unit+integration+stress).

P1 — Near-term improvements
1. Debounce/coalescing window for rapid updates (50–100ms default)
2. Performance tuning hooks
   - Expose batch/prefetch/pull timeouts; document tuning guidance.
3. Empty assignment semantics
   - Explicit acceptance test and option to pause pull loop vs applying empty filter.
4. Enhanced failure escalation
   - After consecutive iterator failures, force a consumer refresh and optionally raise a structured event/hook.

P2 — Nice-to-have/longer term
1. Adaptive batch sizing and deeper performance auto-tuning
2. Richer introspection APIs and admin tooling (dump current subjects, last update reason, last error)
3. Two-phase handoff & claim store integration (tracked separately)

## Acceptance criteria (for P0)

- Failure recovery
  - Given an externally deleted consumer, the helper auto-recreates it and resumes pulling without user action within a bounded time (e.g., < 10s p95) and increments `*_recreations_total`.
  - Given repeated iterator failures (e.g., simulated heartbeat loss), the helper escalates after N restarts in T, performs a consumer refresh, and exposes a non-healthy state until recovery.
- Retry policy
  - All control-plane operations use jittered exponential backoff; mean retry rates are spread across instances (verified in tests by timing windows).
- Guardrails
  - `MaxSubjects` enforced; attempts to exceed fail fast with a clear error and metric; no uncontrolled growth in filter lists.
  - Changing `workerID` after first success returns a well-typed error unless an explicit override is set.
- Observability
  - Metrics exposed for updates, latency, current subject count, subject add/remove deltas, recreations, iterator restarts, and consecutive failure counters.
  - Health accessor reflects recent failure state and is usable in readiness checks.
- Test stability
  - `scripts/flake_detector.sh` consistently returns success across multiple runs with `TEST_SET=unit,integration,stress`.

## Rollout plan and ops playbook

1. Enable metrics and health endpoints; deploy to a small canary (5–10%).
2. Watch: recreations, iterator restarts, update latency, consumer info fetch failures, subject count, and error-rate SLOs.
3. If stable after a fixed observation window (e.g., 24–48h), ramp to 50%, then 100%.
4. Rollback trigger: sustained non-zero health failures, recreation failure spikes, or p95 update latency regressions > 2x baseline.

Operator playbook snippets:
- If health unhealthy due to recreation failures: check NATS server logs for permissions/limits; verify `MaxSubjects`; consider shrinking assignment or increasing limits; on-call can force a manual `UpdateWorkerConsumer`.
- If rapid update storms observed: temporarily increase the coalescing window; validate Manager stabilization windows.

## Mapping to code and docs

- Public API
  - `subscription`: `NewDurableHelper`, `(*DurableHelper).UpdateWorkerConsumer`, `WorkerSubjects`, `WorkerConsumerInfo`
  - Root: `WorkerConsumerUpdater`, `WithWorkerConsumerUpdater`
- Tests needing addition (P0/P1)
  - `subscription/*_test.go`: external consumer deletion; heartbeat-induced iterator recovery; rapid updates with/without debounce
  - `test/stress/`: longer-run iterator restart scenarios with jitter verification
- Docs to update
  - Add metrics + health to `docs/OPERATIONS.md` and `docs/USER_GUIDE.md`
  - Extend `docs/design/06-implementation/single-consumer-implementation-status-and-roadmap.md` Observability/Edge Cases with acceptance criteria

## Risks and mitigations (updated)

- Retry storms → Use jittered exponential backoff; cap retries and surface health
- External consumer deletion → Auto-recreate with bounded retries and metrics
- Large subject sets → Enforce `MaxSubjects`; document subject derivation constraints
- Rapid consecutive updates → Introduce debounce/coalescing and monitor coalesced counts
- Empty assignments → Explicitly test; add option to pause vs apply empty filters

## Timeline proposal (suggested)

- Week 1–2: P0 recovery + retry + guardrails; implement baseline metrics and health; add tests; get flake detector to green
- Week 3: P1 coalescing + empty-assignment behavior; performance hooks; docs
- Week 4+: Adaptive batch sizing and deeper tuning (P2) as needed by workload profiles

---

This analysis is intended as the foundation for the next refactoring/enhancement iterations. Completing P0 items brings the single-consumer design to strong production readiness with clear signals and bounded failure modes.
