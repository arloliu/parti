# Single Consumer per Worker: Execution Checklist (P0 & P1)

This checklist provides concrete, actionable implementation tasks and acceptance criteria for P0 (foundational resiliency/operability) and P1 (near-term hardening) items.

Authoritative context lives in:
* Roadmap: `single-consumer-implementation-status-and-roadmap.md`
* Analysis: `refactor-single-consumer-analysis.md`

Use this document directly for sprint planning and PR scoping.

---
## Legend
* Task IDs: SC-P0-#, SC-P1-#
* Status Fields (for team use): TODO / IN-PROGRESS / DONE / BLOCKED
* Metrics format: Prometheus `name{label=value}`.
* Time targets are initial; adjust after baseline measurement.

---
## Success Metrics Summary
These are the primary KPIs we will use to judge success across P0 scope. Individual sections below provide full detail and tests.

- External consumer deletion recovery (SC-P0-1): p50 <2s, p95 <5s, p99 <10s; success ≥99.5%/500 trials; zero duplicate message IDs during recovery windows.
- Backoff behavior (SC-P0-2): jitter present (≥50ms stddev across 3 workers); max delay ≤ RetryMax; escalation only after MaxControlRetries exceeded.
- Guardrails (SC-P0-3): warning at ≥90% of cap; violations fail fast (<10ms); workerID immutable by default.
- Health & iterator (SC-P0-4/6): unhealthy after ≥3 consecutive failures in 1 minute; escalation triggers once per window and restores to healthy after success.
- Test flakiness (SC-P0-5): 10 consecutive full runs pass under race; baseline flake rate <1/100.
- Observability (SC-P0-7): required logs present with fields; metrics scrapeable and populate ≥3 histogram buckets under load.

---
## P0 Items (Foundational Resiliency & Operability)

### SC-P0-1 Auto-Recovery for Missing/Invalid Consumer
Implementation Tasks:
1. Add error classifier in `subscription/worker_comsumer.go` mapping JetStream/NATS errors → categories (include code comment table):
   - `ErrConsumerNotFound` → terminal_not_found
   - `ErrStreamNotFound`, permission/authorization errors → terminal_policy
   - Timeouts / temporary network / 5xx → transient
   - Unknown → unknown (logged at WARN + surfaced to metrics)
2. Store last applied subjects + consumer name internally (already cached; expose accessor for recreation).
3. Implement `recreateDurableConsumer(ctx)`:
   - Decorrelated jitter exponential backoff (base 200ms, multiplier 1.6, cap 5s).
   - Attempt cap: 6 (configurable via `DurableConfig.MaxRecreationRetries`).
   - Emit attempt + result metrics.
4. Hook recreation triggers:
   - Terminal_not_found classification from info fetch / update.
   - Iterator loop fatal error classification (e.g., heartbeat stall escalation).
5. Health integration: mark unhealthy after 3 consecutive recreation failures within 1 minute (configurable).
6. Integration test: external deletion (delete consumer via JS API) → recreation + resumed delivery.
7. Stress test: random deletions (Poisson process) validate bounded recovery and no duplicate consumption.
8. Document mapping table in code comments & add unit tests asserting classifier output.

Metrics:
* `parti_worker_consumer_recreations_total{result=success|failure,reason=not_found|iterator_error}`
* `parti_worker_consumer_recreation_attempts_total{reason=not_found|iterator_error}`
* `parti_worker_consumer_recreation_duration_seconds` (Histogram: buckets 0.1, 0.25, 0.5, 1, 2, 5, 10)
* `parti_worker_consumer_health_status{status=healthy|unhealthy}` (Gauge; 1 for healthy, 0 for unhealthy)

Acceptance Criteria:
* External deletion recovery latency: p50 <2s, p95 <5s, p99 <10s; success rate ≥99.5% over 500 trials.
* All attempts emit correct metrics labels (validated via metrics scrape test).
* Health transitions unhealthy after threshold breaches; returns healthy automatically after successful recreation.
* No duplicate message IDs during recreation window (verified in integration test).

Expected Files:
- `subscription/worker_comsumer.go` (error classification, triggers, recreation path, health integration)
- `subscription/config.go` (new tuning fields and defaults)
- `internal/metrics/` and `types/metrics_collector.go` (metric definitions/integration)
- `test/integration/` (external deletion + recovery tests)
- `examples/basic/README.md` (metrics scrape example)

### SC-P0-2 Jittered Exponential Backoff for Control-Plane Ops
Implementation Tasks:
1. Add backoff helper (`subscription/backoff.go`) implementing decorrelated jitter (base, multiplier, cap) + optional seed for deterministic test mode.
2. Extend `DurableConfig` with defaults table (document in godoc):
   - `RetryBase = 200ms`
   - `RetryMultiplier = 1.6`
   - `RetryMax = 5s`
   - `MaxControlRetries = 6`
3. Replace fixed linear retry logic in `UpdateWorkerConsumer` and consumer creation/update paths with helper.
4. Apply backoff to iterator restart (non-recreation) path; maintain separate counters.
5. Unit test: generate sequences across ≥5 random seeds; assert variance and monotonic cap behavior (remove identical-seed expectation).
6. Integration test: inject transient failures; verify total time within theoretical bound (sum of backoff schedule) and eventual success.

Metrics:
* `parti_worker_consumer_control_retries_total{op=create|update|info|iterate}`
* `parti_worker_consumer_retry_backoff_seconds{op=...}` (Histogram optional)

Acceptance Criteria:
* Across ≥3 workers under induced errors, retry start times exhibit ≥50ms stddev variance.
* Max observed delay ≤ configured `RetryMax` in tests.
* Failure escalation only after exceeding `MaxControlRetries`.

Expected Files:
- `subscription/backoff.go` (new helper)
- `subscription/worker_comsumer.go` (replace retry logic)
- `subscription/config.go` (defaults)
- `subscription/doc.go` (package-level docs for behavior)
- `subscription/*_test.go` (unit tests for sequences)

### SC-P0-3 Guardrails: MaxSubjects & WorkerID Immutability
Implementation Tasks:
1. Add `MaxSubjects int` (default 500) and `AllowWorkerIDChange bool` fields to `DurableConfig` (document rationale: conservative starting cap, raise after measurement).
2. Validate subject count pre-update; if > `MaxSubjects`, return typed error `ErrMaxSubjectsExceeded`.
3. Track initial workerID after first successful update; reject changes with `ErrWorkerIDMutation` unless `AllowWorkerIDChange` is true.
4. Add unit tests: exceeding subject limit; workerID mutation rejection; allow override success.
5. Add integration test: large assignment near limit triggers warning metric one step before cap (optional pre-cap threshold at 90%).

Metrics:
* `parti_worker_consumer_subjects_current` (Gauge)
* `parti_worker_consumer_guardrail_violations_total{type=max_subjects|workerid_mutation}`
* `parti_worker_consumer_subjects_threshold_warnings_total` (when >90% of cap)

Acceptance Criteria:
* Updates beyond cap fail fast (<10ms) without modifying consumer (profile in unit test).
* WorkerID mutation disallowed by default; allowed only when override set.
* Threshold warning fires when subject count ≥450 (90% of 500) and violation fires at >500.

Expected Files:
- `subscription/config.go` (new fields + defaults)
- `subscription/worker_comsumer.go` (validation and metrics)
- `subscription/errors.go` (typed guardrail errors)
- `subscription/*_test.go` (unit tests)

### SC-P0-4 Metrics Baseline & Health Accessor
Implementation Tasks:
1. Integrate metrics emission points in update, diff classification, consumer recreation, iterator restart.
2. Implement `Health()` on helper returning struct:
   ```go
   type HelperHealth struct {
       Healthy bool
       ConsecutiveIteratorFailures int
       ConsecutiveRecreationFailures int
       LastError error
       LastUpdated time.Time
   }
   ```
3. Provide `IsHealthy()` convenience method.
4. Expose metrics endpoint example in `examples/basic` README (optional).
5. Add unit tests for health transitions.
6. Add integration test injecting repeated iterator failures -> unhealthy -> recovery.

Metrics (full list to implement):
* `parti_worker_consumer_updates_total{result=success|failure|noop}`
* `parti_worker_consumer_update_latency_seconds` (Histogram; buckets: 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1)
* `parti_worker_consumer_subjects_current`
* `parti_worker_consumer_subject_changes_total{type=add|remove}`
* `parti_worker_consumer_iterator_restarts_total{reason=transient|heartbeat}`
* `parti_worker_consumer_consecutive_iterator_failures` (Gauge)
* Recreation metrics from SC-P0-1
* Guardrail metrics from SC-P0-3

Acceptance Criteria:
* Metrics compile and are scrapeable in example application.
* Health accessor returns unhealthy after threshold exceeded (iterator failures ≥3 in 1min or recreation failures ≥3 in 1min) and reverts after recovery.
* Update latency histogram shows population in ≥3 buckets under load test.

Expected Files:
- `subscription/worker_comsumer.go` (emit metrics, implement Health/IsHealthy)
- `types/metrics_collector.go` and `internal/metrics/` (wire counters/gauges/histograms)
- `examples/basic/README.md` (scrape example)
- `subscription/*_test.go`, `test/integration/` (tests for health transitions)

### SC-P0-5 Test Hardening & Flake Elimination
Implementation Tasks:
1. Add stress test generating rapid assignment changes (simulate 100 updates in <2s) ensuring no panics or deadlocks.
2. Add heartbeat gap simulation test to trigger iterator restarts.
3. Integrate these tests into `scripts/flake_detector.sh` run set.
4. Record baseline flake rate (<1 in 100 runs) before marking DONE.
5. Document test strategies in `test/README.md` (brief subsection).

Acceptance Criteria:
* Flake detector passes 10 consecutive runs with unit+integration under race detector.
* Stress tests produce stable metrics (no runaway retries, recreations, or leaked goroutines).
* Heartbeat failure scenario recovers within defined SLA (<8s p95).

Expected Files:
- `test/stress/`, `test/integration/` (new/expanded tests)
- `scripts/flake_detector.sh` (include new tests)

### SC-P0-6 Iterator Health Escalation
Implementation Tasks:
1. Track consecutive iterator failures + timestamps.
2. If failures ≥3 within 1 minute, trigger escalation path:
   - Force consumer refresh (reapply last subjects) unless refresh already pending.
   - Increment escalation metric.
3. Reset counters after successful stable iteration cycle.
4. Unit test: inject synthetic failure sequence; assert escalation triggered exactly once.
5. Integration test: simulate heartbeat stall; verify escalation reduces subsequent failure rate.

Metrics:
* `parti_worker_consumer_iterator_escalations_total{reason=consecutive_failures}`
* Reuse health status metric (unhealthy set during escalation until success).

Acceptance Criteria:
* Escalation triggers only after configured threshold.
* Post-escalation success path restores healthy status.
* No duplicate escalations for same failure window.

Expected Files:
- `subscription/worker_comsumer.go` (counters, escalation path)
- `subscription/config.go` (thresholds if configurable)
- `subscription/*_test.go` (unit tests)
- `test/integration/` (heartbeat stall scenario)

### SC-P0-7 Operational Readiness & Observability
Implementation Tasks:
1. Enrich logs with structured fields: `worker_id`, `consumer_name`, `subject_count`, `op`, `result`.
2. Observability verification: confirm all critical transitions (healthy→unhealthy, handoff prepare→commit) produce structured logs and metrics.

Metrics:
* `parti_worker_consumer_dump_requests_total` (optional if Dump used externally)

Acceptance Criteria:
* Logs show required fields across major operations (spot-check integration test).

Expected Files:
- `subscription/worker_comsumer.go` (log enrichment)
- `internal/logging/` and `types/logger.go` (ensure structured fields)
- `examples/basic/README.md` (optional log/sample references)

---
## P1 Items (Near-Term Hardening)

### SC-P1-1 Manager-Side Claim Store & Two-Phase Handoff
Implementation Tasks:
1. Define KV schema: key=`claims/<partitionID>`, value JSON: `{owner,pendingOwner,state,epoch,ttl,lastUpdated}` (TTL default = 60s = 2× heartbeat interval).
2. Implement CAS write helper: `UpdateClaim(partitionID, expectedEpoch, newState)` returns new epoch or error.
3. Add handoff state machine in manager: `stable -> prepare -> commit -> stable`.
4. Prepare phase:
   - Write claim with `pendingOwner=B, state=prepare, epoch++`.
   - Issue `UpdateWorkerConsumer` removing subjects from A.
   - Wait for A confirmation (subject set excludes S) or timeout.
5. Commit phase:
   - Write claim with `owner=B, pendingOwner=null, state=commit, epoch++`.
   - Issue `UpdateWorkerConsumer` adding subjects to B.
   - Transition to `stable` after B confirmation.
6. Failure handling:
   - Prepare timeout: retry up to 3; if still failing and owner A healthy → abort (remain A); if A unhealthy → mark unassigned for reassignment task.
   - Commit timeout: retry up to 5 with backoff; before rollback, verify A still healthy; if A unhealthy → unassigned state.
   - CAS conflicts: count conflicts; apply jittered backoff only after ≥2 consecutive conflicts.
7. Crash recovery:
   - On startup, manager scans all claims: if state=prepare or commit, resume appropriate phase.
8. Metrics:
   - `parti_handoff_total{result=success|timeout|abort|rollback}`
   - `parti_handoff_duration_seconds` (Histogram: 0.1, 0.25, 0.5, 1, 2, 5)
   - `parti_handoff_phase_duration_seconds{phase=prepare|commit}`
   - `parti_handoff_cas_conflicts_total`
   - `parti_claim_store_size` (Gauge)
   - `parti_claim_store_stale_total`
9. Integration tests:
   - Happy path A→B handoff without duplicates.
   - Manager crash mid-prepare; new manager resumes.
   - Timeout simulate A not responding; confirm abort logic.
   - Commit timeout with rollback.

Acceptance Criteria:
* Zero duplicate message IDs across ≥1000 simulated handoffs.
* All state transitions emit metrics entries and structured logs.
* Crash recovery for prepare/commit resumes and completes or aborts deterministically.
* CAS conflicts <0.5% of total CAS ops under stress (3 managers competing) and backoff engages only on consecutive conflicts.

Expected Files:
- `manager.go` and `internal/assignment/` (handoff orchestration/state machine integration)
- `internal/kvutil/` (CAS helpers), `internal/natsutil/` (if KV wiring needed)
- `types/election_agent.go`, `internal/election/` (if leadership hooks required)
- `internal/metrics/` (handoff metrics)
- `test/integration/` (handoff scenarios, crash recovery tests)

### SC-P1-2 Debounce / Coalescing of Rapid Updates
Implementation Tasks:
1. Add `UpdateCoalescingWindow time.Duration` to Manager/Helper config (default 75ms).
2. Implement coalescing buffer collecting partitions changes; flush after window or explicit stable signal.
3. Ensure last update in window wins (diff collapsed to final subject set).
4. Emit metric: `parti_assignment_coalesced_batches_total` and `parti_assignment_coalesced_updates_total`.
5. Stress test: 100 rapid updates collapse to <= expected number of batches (approx totalTime/window).

Acceptance Criteria:
* Reduction in update calls: ≥80% when update rate >10/s; ≥50% when 5–10/s.
* No missed partitions (final subject set matches last intended update).
* Latency overhead (flush delay) < window + 20ms.

Expected Files:
- `manager.go` (coalescing buffer)
- `subscription/worker_comsumer.go` (flush signal handling)
- `subscription/config.go` (window setting)
- `test/stress/` (rapid updates collapse tests)

### SC-P1-3 Empty-Assignment Behavior Toggle
Implementation Tasks:
1. Add config flag `OnEmptyAssignment string` with values: `apply-empty-filter` (default), `pause-pull-loop`.
2. Implement branch in `UpdateWorkerConsumer`:
   - If `pause-pull-loop`: stop pulling (retain consumer; keep subjects unchanged internally) until next non-empty update.
3. Unit tests for both modes (empty assignment then non-empty assignment resumes correctly).
4. Integration test verifying no messages consumed while paused.

Acceptance Criteria:
* Both modes function; paused mode yields zero pulls and logs clear status.
* Resuming from paused reinstates pulling within <1s.

Expected Files:
- `subscription/worker_comsumer.go` (pause/resume path)
- `subscription/config.go` (toggle)
- `subscription/*_test.go`, `test/integration/` (behavioral tests)

### SC-P1-4 Performance Tuning Hooks
Implementation Tasks:
1. Add configurable batch size, pull timeout, prefetch: `PullBatchSize`, `PullTimeout`, `PrefetchDepth`.
2. Instrument per-iteration metrics: `parti_pull_batch_size_observed` (Histogram), `parti_pull_cycle_duration_seconds`.
3. Load test with synthetic partitions verifying adjustments shift throughput/latency as expected.

Acceptance Criteria:
* Establish baseline first (Step 0): measure throughput & p95 latency under 100 partitions @ 1000 msg/s each.
* Adjusting batch size/prefetch produces ≥20% throughput delta vs baseline with 95% confidence interval.
* No regressions: zero lost/duplicated messages in load test.

Expected Files:
- `subscription/worker_comsumer.go` (use new knobs)
- `subscription/config.go` (add knobs)
- `subscription/*_test.go` (unit)
- `test/performance/` or `test/stress/` (baseline + tuning validation)

---
## Cross-Cutting Quality Gates
Implementation Tasks:
1. Add new metrics to example application for visibility (optional small exporter snippet).
2. Ensure go doc comments updated for new exported config/structs following project format.
3. Run `golangci-lint` after each major feature; fix introduced issues.
4. Add/update README sections in `docs/OPERATIONS.md` for metrics and health.
5. Security review: KV bucket ACLs, workerID spoof prevention, error message sanitization.
6. Observability verification: structured logging for all critical transitions; metrics presence validated.
7. Metrics namespace standardization (DEFERRED): implement initial names first; schedule rename pass post-adoption.

Acceptance Criteria:
* Lint: PASS
* Tests (unit+integration+stress): PASS under race detector.
* Flake detector: PASS (≥10 consecutive runs).
* Docs updated (Operations & User Guide reference new flags and health).
* Security review completed; no high severity findings.
* Observability checklist items validated in integration tests.

---
## Config/API Changes Summary
This section summarizes new and changed configuration fields introduced by this effort. Defaults are initial targets and may be tuned after baselining.

- Backoff (SC-P0-2): `RetryBase=200ms`, `RetryMultiplier=1.6`, `RetryMax=5s`, `MaxControlRetries=6`
- Guardrails (SC-P0-3): `MaxSubjects=500`, `AllowWorkerIDChange=false`
- Health/Escalation (SC-P0-6): thresholds for consecutive failures within 1 minute (default 3)
- Debounce (SC-P1-2): `UpdateCoalescingWindow=75ms`
- Empty Assignment (SC-P1-3): `OnEmptyAssignment="apply-empty-filter" | "pause-pull-loop"` (default apply)
- Performance (SC-P1-4): `PullBatchSize`, `PullTimeout`, `PrefetchDepth` (no-op until tuned)

All new exported fields must include godoc per project format and be validated in `subscription/config.go`.

---
## API Changes Diff Checklist
Use this checklist whenever a PR introduces or modifies exported APIs (types, functions, config fields) or behavior that callers rely on. Apply before request for review.

Versioning & Scope:
- [ ] Confirm if change is additive, breaking, or behaviorally significant (performance/semantics). Classify: ADDITIVE | BREAKING | SEMANTIC.
- [ ] For BREAKING: ensure mitigation path documented (flag, fallback, temporary alias) and semver bump rationale captured in PR description.

Config / Options:
- [ ] New config fields have explicit defaults and validation in `subscription/config.go`.
- [ ] Defaults match documented values in this checklist (or updated if intentionally changed).
- [ ] Added fields appear in `Config/API Changes Summary` section above (kept in sync).
- [ ] Deprecations: mark with comment `// Deprecated:` + replacement guidance; add to `CHANGELOG.md`.

Godoc & Documentation:
- [ ] Godoc first line starts with identifier name per project style.
- [ ] Multi-paragraph doc includes Parameters/Returns sections if applicable.
- [ ] Example usage updated/added in `examples/` when behavior is non-trivial.
- [ ] Operational impact (metrics, health) reflected in `docs/OPERATIONS.md` or `USER_GUIDE.md`.

Interfaces & Types:
- [ ] No public interface assertions added in non-internal packages (avoid cycles); instead add test-time compile assertions in `*_test.go`.
- [ ] Internal packages adding new types include compile-time interface assertion just after type.
- [ ] Receiver naming consistent and short; no stutter in type names.

Metrics & Observability:
- [ ] New metrics expose clear label cardinality; avoid unbounded labels.
- [ ] Names follow `parti_<area>_<subject>_<metric>` pattern; histogram buckets defined purposefully.
- [ ] Structured logs include new fields when relevant (checked with spot test).
- [ ] Update metrics lists in related sections (e.g., SC-P0-4, SC-P1-1) if expanded.

Testing & Stability:
- [ ] Unit tests cover new code paths (success + error cases).
- [ ] Integration tests updated if handoff/assignment semantics changed.
- [ ] Race detector passes (`go test -race ./...`).
- [ ] Flake detector script includes new stress or scenario tests if instability risk introduced.

Backward Compatibility / Migration:
- [ ] For renamed fields, maintain temporary alias or migration note (if breaking) in docs.
- [ ] Behavior changes (timing, retries) have thresholds updated in acceptance criteria sections.
- [ ] No silent behavior alteration without logging at INFO or WARN on first occurrence.

Performance:
- [ ] Critical path changes benchmarked or profiled (attach summary in PR if throughput/latency affected).
- [ ] No unintended allocations on hot path (use `go test -bench` or profiler validation for changes adding loops/interfaces).

Change Tracking:
- [ ] `CHANGELOG.md` entry added (Added | Changed | Deprecated | Removed | Fixed | Security) with clear description.
- [ ] PR description links to relevant task IDs (e.g., SC-P0-2) and sections.
- [ ] Cross-reference any deferred follow-ups (P2) if introduced by this change.

Sign-off:
- [ ] Reviewer checklist includes verification of godoc, metrics wiring, tests, and CHANGELOG.
- [ ] Maintainer confirms semver decision (patch/minor/major) before merge.

Failure Modes & Rollback:
- [ ] Enumerated potential failure modes in PR (e.g., excessive retries, handoff stuck) and monitoring signals.
- [ ] Defined rollback steps (config revert, feature flag disable) documented if risk ≥ MEDIUM.

Security:
- [ ] Input validation for new config fields (bounds, allowed values).
- [ ] No sensitive data in logs/errors; errors wrapped with context safely using `%w`.

This checklist should be copied into the PR description and checked off explicitly.

---
## Pull Request Checklist (for this refactor)
Use this lightweight checklist on each PR implementing items from this document.

- [ ] Docs: Updated godoc for new exported fields/types; referenced in `docs/OPERATIONS.md` and example README if applicable
- [ ] Metrics: New counters/gauges/histograms wired via `types/metrics_collector.go`; labels follow names in this doc
- [ ] Tests: Unit + integration; race detector green; added to `scripts/flake_detector.sh` where relevant
- [ ] Observability: Structured logs include `worker_id`, `consumer_name`, `subject_count`, `op`, `result`
- [ ] Security: Reviewed any KV access/ACLs and input validation; no secrets in logs
- [ ] Lint: `golangci-lint` passes; no unused or dead code introduced
- [ ] Dependencies: No new external dependencies without approval; if added, pinned and justified

---
## Suggested Implementation Order (with Checkpoints)
1. SC-P0-2 (Backoff) – checkpoint: staging deploy.
2. SC-P0-3 (Guardrails) – checkpoint: cap metrics reviewed.
3. SC-P0-1 (Auto-Recovery) – checkpoint: 24h soak without failures.
4. SC-P0-4 (Metrics & Health) – checkpoint: observability review.
5. SC-P0-6 (Iterator Escalation) – checkpoint: escalation test passes.
6. SC-P0-7 (Operational Readiness) – checkpoint: runbook & Dump available.
7. SC-P0-5 (Flake Hardening) – gate: must pass before P1.
--- P1 start ---
8. SC-P1-2 (Debounce) – checkpoint: update reduction validated.
9. SC-P1-3 (Empty-Assignment Toggle).
10. SC-P1-4 (Performance Hooks) – baseline before changes.
11. SC-P1-1 (Two-Phase Handoff) – checkpoint: canary rollout (10% workers) & metrics stability.

---
## Tracking Template (example for team use)
| ID | Title | Status | Owner | PR | Notes |
|----|-------|--------|-------|----|-------|
| SC-P0-1 | Auto-Recovery | TODO | | | |
| SC-P0-2 | Backoff | IN-PROGRESS | | | |
| SC-P0-3 | Guardrails | TODO | | | |
| SC-P0-4 | Metrics & Health | TODO | | | |
| SC-P0-5 | Flake Hardening | TODO | | | |
| SC-P1-1 | Two-Phase Handoff | TODO | | | |
| SC-P1-2 | Debounce | TODO | | | |
| SC-P1-3 | Empty Assignment Toggle | TODO | | | |
| SC-P1-4 | Performance Hooks | TODO | | | |

---
## Completion Definition (Overall)
* All P0 acceptance criteria met and validated by tests & metrics.
* No critical flakes across 10 consecutive full test runs.
* Operations docs reflect all new metrics/config options.
* P1 features implemented with passing integration tests and no regression in P0 guarantees.

## Known Limitations & Future Work
* Adaptive batch sizing deferred (P2).
* Two-phase handoff assumes reliable Manager ↔ Worker communication; prolonged network partitions may extend prepare phase.
* Health state not persisted across process restarts (in-memory only).
* Metrics require external collector; no built-in persistence or retention.
* `MaxSubjects` enforced at update time, not at partition source generation.
* Metrics namespace standardization deferred until initial adoption to avoid early churn.
