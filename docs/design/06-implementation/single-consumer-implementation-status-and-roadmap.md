# Single Consumer per Worker: Implementation Status and Roadmap (Updated Priorities)

This document summarizes the implemented refactor described originally in `docs/stream_partition_discussion.md`, replacing the per-partition consumer model with a single durable consumer per worker. It also updates prioritization to reflect current needs, including promoting the manager-side claim store with two-phase handoff to P1.

## Prioritization update (summary)

- P0 (foundational resiliency/operability):
  - Auto-recovery when the worker consumer is externally deleted or becomes unusable
  - Jittered exponential backoff on control-plane operations
  - Guardrails: `MaxSubjects` cap; guard against workerID mutation after first successful update
  - Metrics baseline (updates, latency, subjects, subject changes, recreations, iterator restarts) and a health/readiness accessor
  - Stress tests for rapid updates and heartbeat-induced iterator recreation; flake detector green

- P1 (near-term hardening):
  - Manager-side claim store & two-phase handoff (prepare/drain → commit/activate) for safe, auditable reassignment
  - Debounce/coalescing of rapid consecutive updates (50–100ms default)
  - Empty-assignment behavior switch (apply empty filter vs pause) with explicit tests
  - Performance tuning hooks (batch/prefetch/pull timeouts)

- P2 (later):
  - Richer introspection/admin tooling
  - Two-phase handoff enhancements (rollback paths, richer fencing) as needed

## Scope

Implemented:
* Replace `UpdateSubscriptions` with `UpdateWorkerConsumer` (single durable consumer; multi-subject via `FilterSubjects`).
* Inject immutable message handler at helper construction.
* Maintain a single long-lived pull loop started lazily on first update (NOT eagerly in constructor).
* Provide `WithWorkerConsumerUpdater` option (final name) for Manager-driven updates without import cycles.
* Add observability helpers: `WorkerSubjects()` and `WorkerConsumerInfo(ctx)`.

Deferred (tracked with priority above):
* Manager-side claim store & two-phase handoff (P1).
* Internal debounce/coalescing of rapid consecutive updates (P1).
* Automatic consumer recreation on external deletion/heartbeat starvation (P0).

## Deliverables

Completed:
* New helper constructor (`NewDurableHelper`) in `subscription/`.
* Removal of `UpdateSubscriptions` (legacy code & tests purged).
* Root option `WithWorkerConsumerUpdater` and Manager wiring (invoked on initial + subsequent assignment changes before hooks).
* Updated examples & docs referencing single-consumer pattern.
* Integration + unit tests validating update sequencing and no-op diff behavior.
* Partition helpers (`Partition.SubjectKey()`, `Partition.ID()`, `Partition.Compare()`) leveraged indirectly for deterministic subject naming.

Outstanding / Nice-to-have:
* Metrics instrumentation (planned — see Observability section).
* Test coverage for external consumer deletion & heartbeat-induced iterator recovery.

## Phased Work Breakdown

### Phase 1 — Helper API & Core Logic (Implemented with Adjustments)

Files:
- `subscription/durable_helper.go`
- `subscription/helper.go` (if entrypoints live here)
- `subscription/*_test.go`

Tasks:
1. Add constructor with handler injection (DONE):
   - Final signature: `NewDurableHelper(conn *nats.Conn, cfg DurableConfig, handler MessageHandler) (*DurableHelper, error)`.
   - No eager update: Pull loop starts lazily upon first successful `UpdateWorkerConsumer` (prevents creating a consumer without a stable `workerID`).
   - Handler is mandatory & immutable; no runtime mutation API.
2. Implement `UpdateWorkerConsumer(ctx, workerID, partitions) error` (DONE):
   - Generates subjects (template expansion) → dedupe via map → sort deterministically.
   - No-op fast path if subjects unchanged & same workerID.
   - Uses `CreateOrUpdateConsumer` with fixed retry (linear backoff).
   - Hot-reload semantics: pull loop never restarted; only subject set updated.
   - Caches last applied sorted subjects for idempotency.
   - Enhancement backlog: jittered exponential backoff; detection & recovery if consumer externally deleted.
3. Internal helpers (PARTIAL vs original draft):
   - Implemented: `buildSubjects`, `generateSubject`, `sanitizeConsumerName`, iterator-based pull loop (`runWorkerPullLoop`).
   - Not implemented: separate `applyConsumerUpdate`, `ensurePullLoopHealth` abstractions (logic inlined for simplicity).
   - Not needed: `consumerName` (constructed inline with sanitization).
4. Remove legacy per-partition paths (DONE): legacy APIs removed; per-partition structures retained only for historical code paths still referenced by older functions (clean for worker mode).

Acceptance (ACHIEVED):
* Unit tests cover: lifecycle create → expand → shrink; empty assignment; no-op diff; info access before init errors.
* Integration test validates Manager triggers updates initial + refreshed assignment.
* Pull loop continuity (no restart) validated implicitly by message delivery across updates.

### Phase 2 — Root Option: WithWorkerConsumerUpdater (Implemented)

Files:
* `options.go` (option + interface)
* `manager.go` (invocation prior to hooks)

Tasks:
1. Define interface in root:
   ```go
   type WorkerConsumerUpdater interface {
       UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []Partition) error
   }
   ```
2. Add `WithWorkerConsumerUpdater(updater WorkerConsumerUpdater) Option` (final name):
   - Compose with existing `OnAssignmentChanged` hook.
   - Inject workerID into context before invoking composed hook (define `WorkerIDContextKey`).
   - Call `updater.UpdateWorkerConsumer(ctx, workerID, newParts)`.
3. Ensure Manager sets workerID in context when dispatching `OnAssignmentChanged`.

Acceptance (ACHIEVED):
* Integration test asserts updater invocation.
* Context workerID implicitly managed by Manager (documented in code comments).

### Phase 3 — Examples Update (Completed)

Files:
- `examples/basic/main.go` and READMEs under `examples/`

Tasks:
1. Switch examples to new helper constructor and `UpdateWorkerConsumer`.
2. Show wiring via hooks or `WithConsumerUpdater`.

Acceptance:
- Examples build and run locally against embedded NATS (if existing scaffolding).

### Phase 4 — Tests & Cleanup (Completed + Gaps Identified)

Files:
- Remove: legacy tests tied to `UpdateSubscriptions`.
- Add/Update: `subscription/*_test.go`, integration tests invoking helper with multiple subjects.

Tasks:
1. Delete legacy per-partition tests and any race-specific tests that no longer apply.
2. Add tests:
   - No-op diff suppression increments counter.
   - Add/remove sequences keep pull loop stable and deliver messages.
   - Failure on CreateOrUpdateConsumer triggers retry then surfaces error.
3. Validate docs: ensure design doc and this plan reflect repository state.

Acceptance (ACHIEVED) plus gaps:
* All existing unit & integration tests pass.
* Gaps: no tests yet for external consumer deletion, concurrent rapid updates stress, heartbeat-induced iterator recreation.

## Detailed API Contracts (Final Implemented Signatures)

Helper constructor:
```go
func NewDurableHelper(conn *nats.Conn, cfg DurableConfig, handler MessageHandler) (*DurableHelper, error)
```
Notes:
* No eager consumer creation; first `UpdateWorkerConsumer` starts pull loop.
* `handler` must implement `MessageHandler`; immutable post-construction.

Update method:
```go
func (dh *DurableHelper) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error
```

Observability helpers:
```go
func (dh *DurableHelper) WorkerSubjects() []string
func (dh *DurableHelper) WorkerConsumerInfo(ctx context.Context) (*jetstream.ConsumerInfo, error)
```

Root option:
```go
type WorkerConsumerUpdater interface {
   UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []Partition) error
}

func WithWorkerConsumerUpdater(updater WorkerConsumerUpdater) Option
```

## Observability & Metrics

Current state:
* Logging: structured logs for creation, reuse, errors, iterator failures.
* Introspection: `WorkerSubjects()`, `WorkerConsumerInfo()`.

Planned metrics (baseline):
* `parti_worker_consumer_updates_total{result=success|failure|noop}`
* `parti_worker_consumer_update_latency_seconds` (histogram)
* `parti_worker_consumer_subjects_current` (gauge)
* `parti_worker_consumer_subject_changes_total{type=add|remove}`
* `parti_worker_consumer_recreations_total` (auto-recovery events), heartbeat gap counters.

Proposed config addition: `MaxSubjects` to enforce an upper bound and fail fast.

## Risks & Mitigations

| Risk | Current Mitigation | Future Enhancement |
|------|--------------------|--------------------|
| JetStream transient errors | Linear retry w/ fixed backoff | Add jitter + exponential backoff (P0) |
| External consumer deletion | Next update recreates; iterator errors logged | Auto-detect & reapply last subjects (P0) |
| Empty assignment semantics | Apply empty `FilterSubjects` (requires validation) | Option to skip consumer update or pause loop (P1) |
| Large subject set | None (no cap) | Enforce `MaxSubjects` config + metric warning (P0) |
| Heartbeat loss | Iterator recreation loop | Escalate after N failures → force consumer refresh (P0/P1) |
| Rapid successive updates | Manager stabilization windows | Internal debounce (coalescing window) (P1) |
| WorkerID change misuse | Overwrites internal state silently | Explicit guard / error if workerID changes (P0) |
| Retry storms | Fixed aligned intervals | Add jitter to spread load (P0) |

## Done Criteria (Current Status)

Achieved:
* No references to `UpdateSubscriptions` remain.
* Single pull loop stable across updates (lazy start once).
* Option-based wiring via `WithWorkerConsumerUpdater` executed before hooks.
* CI passing (build, unit, integration at time of refactor completion).

Outstanding (with priorities):
* Metrics instrumentation (P0 baseline).
* Edge-case recovery tests (external deletion, heartbeat stress) (P0).
* Retry strategy improvements (P0).
* Manager-side claim store & two-phase handoff (P1).
* Debounce/coalescing (P1).
* Guard against workerID mutation (P0).

## Execution Checklist (Revised)

1) Helper
* [x] Constructor with handler (lazy start, not eager).
* [x] `UpdateWorkerConsumer` diff + apply + cache.
* [x] Legacy per-partition API removed.
* [ ] Metrics hooks (P0).

2) Root
* [x] `WorkerConsumerUpdater` interface.
* [x] `WithWorkerConsumerUpdater` option wired.

3) Examples & Docs
* [x] Examples migrated.
* [x] Docs updated for single-consumer model.
* [ ] Design doc updated with edge cases & metrics (this document and analysis).

4) Tests
* [x] Lifecycle & no-op tests.
* [x] Integration updater test.
* [ ] External deletion / heartbeat stress tests (P0).

5) Quality Gates
* [x] Build / Lint / Tests PASS (baseline).
* [ ] Add performance regression harness (P2).

6) Enhancements Backlog (Prioritized)
* [P0] Jittered exponential retry.
* [P0] `MaxSubjects` config + enforcement.
* [P0] Auto recreation on consumer not found.
* [P0] Metrics suite implementation + health accessor.
* [P1] Manager-side claim store & two-phase handoff.
* [P1] Debounce consecutive updates (50–100ms coalescing window).
* [P1] Empty-assignment behavior toggle + tests.

---

This status and roadmap reflect the real implementation plus prioritized future improvements for observability, resilience, and operational safeguards. Two-phase handoff is now explicitly tracked as P1.
