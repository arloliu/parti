# Consumer-Create Rate-Limit Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL — use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. This plan is in the **plan track** (`/plan-review` → revise → `/final-plan-review`) and is not yet cleared for implementation.

**Goal:** Bound the rate at which a single worker issues JetStream consumer create/update RPCs, so that a large dynamic-partition assignment (e.g. an empty source growing to 20 000 partitions) or a mass recovery event cannot flood the NATS cluster and drive it to hang / OOM / crash.

**Architecture:** Add one shared, nil-safe, **opt-in** token-bucket limiter to the **Dynamic** consumer path. It is consulted before **every physical `CreateOrUpdateConsumer` RPC attempt** a worker issues for per-subject durables — including retry attempts — across the initial-assignment add loop **and** the per-partition recovery/recreation paths, so a single budget paces both the cold-start burst and a recovery storm. The limiter honors `context` cancellation, so a long paced apply unwinds promptly on shutdown. Pacing inside the (by-design unbounded) apply call is structurally safe for liveness, but has explicit, documented consequences for handoff overlap, readiness, and lock-hold — see the Handoff Timing Contract. The handoff **claim-write** burst is a related but distinct vector, documented here and deferred to a measurement-driven fast-follow.

**Tech Stack:** Go ≥1.25, NATS JetStream, `golang.org/x/time/rate` (already an indirect dependency), existing `internal/durable` consumer machinery, `jsutil.EnsureConsumer`, the `consumer.Dynamic` option surface, and the durable metrics surface (`types.WorkerConsumerMetrics` + a new optional sidecar interface).

**Scope (settled with the requester):**
- **In scope:** opt-in rate limiter on the Dynamic consumer-create path (initial add + recovery recreation), gating **every RPC attempt**, default OFF.
- **Out of scope (fast-follow):** rate-limiting the handoff claim-write path; fleet-aggregate / leader-side ramp.

**Review status:** plan-review rounds 1–3 + final precision pass complete. Round-1: 0 P0 / 4 P1 (closed). Round-2: 1 P0 / 3 P1 / 2 P2 (closed). Round-3: 0 P0 / 1 P1 / 2 P2 — "refinement, not reset; architecture sound" (closed). Precision pass: 0 P0 / 3 P1 (stale wording, nil-precedence, DQ-3) — closed in this revision. **No open P0/P1; implementation-ready.** See Revision Log.

---

## Invariant

> The number of **physical `CreateOrUpdateConsumer` RPC attempts** a single worker issues to the cluster per unit time — *including retry attempts* — is bounded by an operator-configured token-bucket rate (steady rate + burst) **when the limiter is enabled**, regardless of how many partitions are assigned, reassigned, or recovered in one event. When disabled (the default), behavior is byte-for-byte unchanged.

"Per physical attempt" is load-bearing: both `jsutil.EnsureConsumer` (`jsutil/consumer.go:35-44`, up to 3 attempts) and `partitionConsumer.ensureConsumer` (`internal/durable/partition_consumer.go:643-674`, up to 3 attempts) retry on transient errors, so a per-*logical-create* gate would let one token expand to three RPCs — and a transient-error storm (exactly when the cluster is already stressed) would overshoot the configured rate up to 3×.

---

## Background — verified facts

All claims grepped/read against `main` at the worktree base. `file:line` is load-bearing; re-verify if the base moves.

### One durable per partition
`internal/durable/worker_consumer.go:22` — `WorkerConsumer` manages one JetStream durable pull consumer per subject (partition).

### Consumer create/update RPC sites (the flood vectors), grep-verified
1. **Initial-assignment add** — `worker_consumer.go:188-192` serial unpaced loop → `addSubjectLoop` (`:407`) → `ensurePerSubjectConsumer` (`:484-486`) → `jsutil.EnsureConsumer` → **retry loop, up to 3 `js.CreateOrUpdateConsumer` attempts** (`jsutil/consumer.go:35-44`).
2. **Recovery/startup re-ensure** — `partition_consumer.go:643-674` `ensureConsumer` → **own retry loop, up to 3 `js.CreateOrUpdateConsumer` attempts** (`:652`).
3. **Recovery recreation** — `partition_consumer.go:715-717` `recreateFn` → single `js.CreateOrUpdateConsumer` (`:717`), invoked (with its own backoff/retries) by the recovery controller.

Each `partitionConsumer` runs in its own goroutine (`go pc.Run(...)`, `worker_consumer.go:477`), so paths (2)/(3) fire **concurrently and unbounded** during a mass stream-recreation. Path 1 = initial create; paths 2/3 = recovery only (the consumer created in path 1 is passed into the `partitionConsumer`; its `ensureConsumer`/`recreateFn` run only when the durable is later found deleted). All three share one budget.

### Non-flood create paths (out of scope, grep-verified)
`EnsureConsumer` callers that create a single / fixed-1:1 consumer per call: Broadcast (`broadcast_consumer.go:311,498`; partitions ignored, `:154-183`), Queue (`queue.go:376,494`), Static/`ipartition` (`internal/ipartition/consumer.go:283,425`; `JSConsumer` is one `Partition()`/`Subject()`). None is a 0→N mass-create vector.

### Pacing inside the apply is structurally safe for liveness
- `applyAssignmentWithPrevCore` calls `m.handoffCoordinator.Apply(m.ctx, …)` with the manager-lifetime ctx (`manager_assignment.go:1449-1452`), documented "unbounded per attempt" (`AGENTS.md:94-97`, `manager.go:493`).
- Heartbeats run on an independent ticker (`heartbeat.New(...)`, `manager_election.go:418`).
- **But** readiness is NOT unaffected: a startup watchdog fires `enterDegraded("startup-timeout")` if still `WaitingAssignment` after `StartupTimeout` (default **60s**, `config.go:421-425`). A long paced cold start interacts with this — see Handoff Timing Contract §5.

### Existing herd controls (default off) — do not duplicate
`ApplyStartJitter` (`config.go:471-500`, default 0; spreads fleet-wide start times), `AssignmentWatcherDebounce` (`config.go:502-533`, default 0), `PhaseConcurrency` (`config.go:123-127`, zero→20 at `internal/assignment/handoff/coordinator.go:171-173`; bounds two-phase KV-op concurrency via `errgroup.SetLimit` at `twophase.go:261,369,423`). All default off so upgrades are no-ops — this plan follows that convention (D3).

### Processing gate is opt-in and is what prevents handoff overlap
`internal/durable/processing_gate.go:15-19` — the gate is **disabled by default**. `docs/LIFECYCLE.md:267-273` — two-phase handoff alone does **not** reduce processing overlap; only the processing gate / pull-gating suppresses pulls for not-yet-committed partitions. This is central to Handoff Timing Contract §2.

### Claim-write path — corrected enumeration (3 sites; out of scope here, see D4)
1. `twophase.go:163` `updateClaim` → `PutIfEpoch` (`:188,:190`); callers `preparePhase:279`, `commitPhase:376`, `stabilizePhase:430`, reap `:523`; phases concurrency-bounded by `PhaseConcurrency`.
2. `manager_handoff.go:172` `handoffStartupHygiene` → `store.PutIfEpoch` **directly**, sequential loop over all keys (`:152-179`), startup-only, **not** `PhaseConcurrency`-bounded.
3. `manager_handoff.go:226` `runHandoffResume` → `store.PutIfEpoch` **directly**, sequential loop over all keys (`:213-229`), startup-only, **not** bounded.

### Public metrics surface
The durable metrics interface is **public**: `types.WorkerConsumerMetrics` (`types/metrics_collector.go:310-383`). Adding required methods is a source-compatible break for external collectors — avoided via an optional sidecar interface (D7).

### Dependencies present
`go.mod`: `golang.org/x/time v0.15.0 // indirect`, `golang.org/x/sync v0.20.0`. No new module download.

---

## Design Decisions

### D1 — Per-attempt gating: one shared, nil-safe token-bucket limiter (addresses P0)
A single limiter instance per `WorkerConsumer`, threaded into every `partitionConsumer`. It is consulted **before each physical `CreateOrUpdateConsumer` attempt**:
- **Path 1:** the exported `jsutil.EnsureConsumer(ctx, js, stream, cfg)` signature is **preserved** (kept as a thin wrapper) so external code holding it as a function value does not break; a new sibling `jsutil.EnsureConsumerWithOptions(ctx, js, stream, cfg, opts...)` adds a `beforeAttempt func(ctx) error` hook invoked inside the retry loop before each `CreateOrUpdateConsumer`. `worker_consumer.ensurePerSubjectConsumer` calls the sibling, passing `beforeAttempt = limiter.Wait`; a `beforeAttempt` error (e.g. ctx cancel) aborts and is returned. All existing callers (Broadcast/Queue/Static) keep calling `EnsureConsumer` unchanged.
- **Paths 2/3:** call `limiter.Wait(ctx)` inline before each `CreateOrUpdateConsumer` inside `partitionConsumer.ensureConsumer`'s retry loop and in `recreateFn`.
- **Nil = unlimited:** nil limiter ⇒ no wait, zero-cost, no behavior change (the default, D3).
- **Goroutine-safe:** `x/time/rate.Limiter` serializes concurrent `Wait(ctx)` from recovery goroutines to the configured rate.

### D2 — Token bucket (rate + burst), not a concurrency cap
The defect is **rate**, not concurrency (the add loop is already serial). Burst absorbs normal small reassignments instantly; rate caps the flood; the recovery storm's concurrency is incidentally bounded because tokens release only at the rate.

### D3 — Opt-in, default OFF (resolved: DQ-1)
Disabled by default (nil/unlimited), matching the repo's herd-control convention (upgrade = no-op). Enabling is one explicit option. A future default flip is a separate post-soak decision. Recommended-when-enabled values (docs, validate by load test): rate ≈ 100/s, burst ≈ 256. Migration note + sizing formula (`rate ≈ cluster-create-budget / max-workers`) in ops docs.

### D4 — Claim-write path: documented, deferred to fast-follow (resolved: DQ-2)
Three sites, two packages, two lifecycles, partially protected (`PhaseConcurrency` bounds the coordinator phases). This plan documents the gap (incl. the unbounded startup loops, flood-capable on a large-fleet restart) and stubs a measurement-driven fast-follow (`10-claim-write-ratelimit-plan.md`). No silent drop.

### D5 — Limiter injectable for multi-stream processes (+ lock-order contract; addresses P2)
`WithConsumerCreateLimiter(limiter)` lets a process share one budget across several `Dynamic` consumers. **Contract (documented in Godoc):** `Limiter.Wait(ctx)` is invoked while manager/consumer-update locks may be held (`applyStoreMu`, `updateMu`); it MUST honor ctx cancellation and MUST NOT call back into `Manager`, `Dynamic`, or any operation requiring those locks. Default (no injection, no rate): nil/unlimited.

### D6 — Scope: per-worker, not fleet-aggregate
Bounds a single worker's create rate; fleet aggregate = `rate × workers`, mitigated by existing `ApplyStartJitter` + operator sizing. Leader-driven ramp out of scope.

### D7 — Throttle metrics via an optional sidecar interface (addresses P1 metrics break)
Do **not** add methods to the public `types.WorkerConsumerMetrics`. Define a narrow optional interface, e.g.:
```go
type ConsumerCreateThrottleObserver interface {
    IncrementConsumerCreateThrottled()
    ObserveConsumerCreateThrottleWait(seconds float64)
}
```
Type-assert the configured metrics value to it before emitting; internal Nop/Prometheus collectors implement it; external collectors are unaffected. Any unavoidable public-interface growth would be called out as API-breaking — this design avoids it.

### Dependency Decision (DQ-3 — RESOLVED)
Use `golang.org/x/time/rate`, promoting it from indirect to a **direct** dependency (already in the build graph via `go.mod`, so no new module download). A hand-rolled token bucket was considered and rejected as needless reinvention. Reversible: the choice sits behind the `internal/ratelimit.Limiter` interface, so a zero-direct-dep posture can be restored later without touching call sites.

---

## Handoff Timing Contract (paced apply)

A paced apply runs long; these are explicit, tested contracts.

1. **Lock hold.** A paced apply holds `applyStoreMu` across `Apply` (`manager_assignment.go:1404-1451`) and `updateMu` across the remove/add loop (`worker_consumer.go:145-148,:182-192`); `Close` needs `updateMu` (`:217-220`). **Contract:** a paced apply serializes the next apply and blocks `Close` for its duration (no race, only slower). Documented in Godoc.

2. **Pre-commit window and overlap — gate-dependent (addresses P1-C).** Two-phase order is prepare → removal guard → consumer-update (paced) → commit → stabilize (`twophase.go:72-118`). A long paced consumer-update delays commit/stable for the whole assignment; old owners retain transfer partitions and the removal guard returns `ErrRemovalPending` (`manager_assignment.go:1788-1800,:1834-1844`) until commit/stable. **Two-phase handoff alone does NOT prevent processing overlap** (`docs/LIFECYCLE.md:267-273`); the processing gate / pull-gating (opt-in, **off by default** — `processing_gate.go:15-19`) is what suppresses pulls for not-yet-committed partitions. **Contract:** in a deployment with the gate OFF, enabling create-rate limiting **lengthens the processing-overlap window** (both old and new owner active) to the full paced-apply duration. Operators running two-phase handoff SHOULD enable the processing gate / pull-gating when enabling create-rate limiting, or accept the longer overlap. This is stated in option Godoc and ops docs.

3. **Partial progress / crash.** On cancel/crash mid-apply, durables created so far are live but the manager snapshot/ack does not advance on apply error (`manager_assignment.go:1461-1487`), so no commit. **Contract:** safe and resumable — `CreateOrUpdateConsumer` is idempotent and `UpdateWorkerConsumer` re-derives `toAdd` from current state on retry (`computeSubjectDiff`, `worker_consumer.go:308-325`); the "ownership commit requires every loop started" invariant (`:163-177`) holds (partial apply → error → no commit). Overlap during the resumable window is gate-dependent per §2.

4. **Shutdown.** `Manager.Stop` cancels `m.ctx` before waiting (`manager.go:799-805,:850-865`); `Apply` uses `m.ctx`; `rate.Wait` returns `ctx.Err()` on cancel → paced apply unwinds, releases locks. Guidance: stop the manager before closing the consumer.

5. **Startup readiness (addresses P1 startup-timeout).** `StartupTimeout` defaults to 60s (`config.go:421-425`); the startup watchdog enters Degraded (reason `startup-timeout`) once if still `WaitingAssignment` at the deadline (soft — for probe rotation; it does NOT abort the apply, which continues unbounded). **Contract / guidance:** a paced large cold start (e.g. ~200s at 100/s for 20 000 partitions) **may** trip this — the watchdog is **state-guarded** (`manager_startup_async.go:169-176`): it fires only if the worker is still in `StateWaitingAssignment` at the deadline, and it enters Degraded for probe rotation **without aborting** the apply (which continues to completion). Operators enabling create-rate limiting on large cold starts SHOULD size `StartupTimeout` ≥ `ColdStartWindow + ElectionTimeout + estimated paced-apply duration + headroom`, or accept an intentional one-shot startup-degraded rotation. Stated in ops docs.

   **Empirical refinement (pinned by `TestPacedColdStart_*` in `test/integration/manager/`):** in the common single-leader cold start, the calculator moves the worker to `StateScaling` during `ColdStartWindow` *before* the paced apply runs, so the `WaitingAssignment`-guarded watchdog typically does **not** fire — the apply is paced while in `Scaling`. The universal readiness concern is therefore simpler than the watchdog: the worker is not `StateStable` until the paced apply completes, so `WaitState(StateStable, …)` callers and readiness-probe budgets must allow for the estimated apply duration regardless of whether the watchdog fires.

---

## File Structure

- **Add** `internal/ratelimit/ratelimit.go` (+ `ratelimit_test.go`) — nil-safe `Limiter` interface; `rate.Limiter`-backed impl with optional metrics callback; nil-as-unlimited helper.
- **Modify** `jsutil/consumer.go` — keep the exported `EnsureConsumer(ctx, js, stream, cfg)` signature intact (refactor its body into a thin wrapper) and add a sibling `EnsureConsumerWithOptions(ctx, js, stream, cfg, opts...)` providing a `beforeAttempt func(ctx) error` hook invoked before each `CreateOrUpdateConsumer` attempt inside the existing retry loop (D1). The `EnsureConsumer` function-value type and all external callers are unchanged.
- **Modify** `internal/durable/config.go` — add `WorkerConsumerConfig.ConsumerCreateLimiter ratelimit.Limiter` (nil ⇒ unlimited).
- **Modify** `internal/durable/worker_consumer.go` — store the limiter; pass `beforeAttempt = limiter.Wait` into `EnsureConsumerWithOptions` from `ensurePerSubjectConsumer` (replacing its current `EnsureConsumer` call); thread the limiter into `partitionConsumerConfig` in `addSubjectLoop`.
- **Modify** `internal/durable/partition_consumer.go` — add the limiter to `partitionConsumerConfig`; `limiter.Wait(ctx)` before each `CreateOrUpdateConsumer` attempt in `ensureConsumer`'s loop (`:647-672`) and in `recreateFn` (`:717`).
- **Modify** `consumer/options.go` + `consumer/dynamic.go` — `WithConsumerCreateRate(perSec float64, burst int)`, `WithConsumerCreateLimiter(l ratelimit.Limiter)`; build/accept in `NewDynamic`; thread into `WorkerConsumerConfig`. **Default neither set ⇒ nil/unlimited.** Validate `burst >= 1` when `perSec > 0`; reject negative `perSec`. Precedence: a **non-nil** `WithConsumerCreateLimiter` wins over `WithConsumerCreateRate` regardless of option order; `WithConsumerCreateLimiter(nil)` is a **no-op** (ignored — it does not clear a configured rate and is never an explicit "unlimited" override).
- **Add** an optional `ConsumerCreateThrottleObserver` interface (D7) near the durable metrics; type-assert before emitting; implement in internal Nop/Prometheus collectors. Do not modify `types.WorkerConsumerMetrics`.
- **Modify docs** — `docs/CONSUMERS.md` (new subsection), `docs/OPERATIONS.md` (sizing, migration note, the gate-dependency for overlap §2, the StartupTimeout interaction §5, the convergence-latency trade-off), option/`config` Godoc, thundering-herd README cross-ref. Document the claim-write residual (D4).
- **Add (stub)** `docs/plans/consumer-create-rate-limit/10-claim-write-ratelimit-plan.md` (D4 fast-follow).

No production claim-write code is modified.

---

## Tasks

> Each task → fresh implementer subagent (`superpowers:subagent-driven-development`) + per-task post-impl review loop to merge-clean before the next.

### Task 1 — `internal/ratelimit` primitive (+ tests) — P1-D
- [ ] `Limiter` interface; `rate.Limiter`-backed impl; nil-safe helper; optional metrics hook.
- [ ] **Test seam:** wiring/path tests use a project-owned fake `Limiter`; the one real-wrapper test asserts burst-vs-throttle via `rate.Limiter.ReserveN(t0, n).DelayFrom(t0)` / `Allow()` / `Tokens()` — deterministic, no `time.Sleep` (per `300-testing.md`).
- [ ] Tests: burst (Delay==0) up to burst; (burst+1)th has positive `DelayFrom(t0)` matching the rate; `Wait` returns `ctx.Err()` on cancelled ctx; nil limiter never blocks.

### Task 2 — Per-attempt wiring through the Dynamic create paths (+ tests) — P0
- [ ] `jsutil.EnsureConsumerWithOptions` `beforeAttempt` hook (keep `EnsureConsumer` as a 4-arg wrapper); `WorkerConsumerConfig` field; `worker_consumer` passes the hook via the sibling; thread limiter into `partitionConsumerConfig`; inline waits in `ensureConsumer`'s loop and `recreateFn`.
- [ ] Tests (fake JS + fake `Limiter`): transient error twice then success ⇒ limiter consulted **3×** before **3** RPCs — a distinct test for **each** of the three sites: the initial-add path (via `EnsureConsumerWithOptions`), `partitionConsumer.ensureConsumer`, AND `partitionConsumer.recreateFn`; cancellation before attempt 2 aborts; **aggregate** test — serial add loop + concurrent recovery goroutines share one budget; **nil/unlimited** ⇒ no waits, behavior unchanged; partial-progress retry creates only remaining subjects with no committed partial apply (`:163-177`).

### Task 3 — `consumer.Dynamic` options + opt-in default + validation (+ tests)
- [ ] `WithConsumerCreateRate`, `WithConsumerCreateLimiter`; **default nil/unlimited**; validation; Godoc (Contract §1/§2/§5 trade-offs + migration note + gate-dependency).
- [ ] Tests: default leaves limiter nil; rate enables pacing; non-nil injected overrides; reject `burst < 1` (with `perSec>0`) and negative `perSec`; **option precedence in both orders**; `WithConsumerCreateLimiter(nil)` + a positive rate keeps the rate-built limiter (nil injection ignored).

### Task 4 — Throttle metrics via optional sidecar interface (+ tests) — P1 metrics
- [ ] Define `ConsumerCreateThrottleObserver`; type-assert + emit; implement in internal collectors; nil-safe.
- [ ] Tests: records only positive waits (not burst-absorbed); **old-style custom `WorkerConsumerMetrics` (without the sidecar) still compiles and runs** (no break).

### Task 5 — Handoff timing contract tests — P1-C
- [ ] Fake limiter blocking on the Nth create: `Close` blocked until ctx cancel; next apply serializes behind paced one; `Manager.Stop` unblocks via `ctx.Err()`; snapshot/heartbeat ack do not advance on cancel.
- [ ] Two-worker handoff, **gate-off vs gate-on**: gate-off distinguishes "old owner retained + overlap for paced duration"; gate-on shows new-owner pulls suppressed pre-commit; both: removal permitted only after commit/stable (`ErrRemovalPending`).
- [ ] **Startup watchdog:** a paced initial assignment that leaves the worker in `StateWaitingAssignment` past `StartupTimeout` enters Degraded(`startup-timeout`) once (state-guarded, `manager_startup_async.go:169-176`); apply still completes; `Stop` cancels cleanly.
- [ ] Watcher-close/retry apply does not interleave past `applyStoreMu`.

### Task 6 — Docs, claim-write residual, fast-follow stub
- [ ] `docs/CONSUMERS.md`, `docs/OPERATIONS.md`, Godoc, thundering-herd README cross-ref — incl. gate-dependency (§2), StartupTimeout sizing (§5), migration note.
- [ ] Document the claim-write residual (D4). Write `10-claim-write-ratelimit-plan.md` stub.

### Task 7 — Integration test + final validation
- [ ] Integration (`test/integration/...`, embedded NATS, `testing.Short()` guard): large add + mass-recovery are paced; assert via the instrumented JS RPC counter (`test/perf-measurement/internal/instrumentedjs`), event-driven, no sleeps; include a retry-injection case proving per-attempt (not per-logical) pacing.
- [ ] `make lint`, `make test` (`-race`), `make pre-pr` (touches `internal/durable`), per `AGENTS.md`.

**Per-task dispatch matrix (post-settlement):**

| Task | Implementer | Codex effort | Rationale |
|------|-------------|--------------|-----------|
| 1 | `sonnet` | `medium` | Primitive; `ReserveN().DelayFrom()` seam is the subtlety. |
| 2 | `sonnet` | `high` | Per-attempt wiring across `jsutil` + two `partition_consumer` retry loops + the no-partial-commit invariant. Highest-risk task. |
| 3 | `sonnet` | `medium` | Option plumbing + opt-in default; mirror `WithMaxConcurrentSubjects`. |
| 4 | `sonnet` | `medium` | Optional-interface type-assert + public-compat test. |
| 5 | `sonnet` | `high` | Manager/handoff/gate/startup timing; event-driven, no sleeps. |
| 6 | `haiku` | `low` | Docs + stub. |
| 7 | `sonnet` | `high` | Embedded-NATS RPC-counter assertions; no sleeps. |

---

## Risk Assessment

- **Convergence latency when enabled (intended):** ~200s for 20 000 at 100/s. Liveness-safe; readiness consequence handled by Contract §5.
- **Handoff overlap with gate OFF (Contract §2):** enabling pacing lengthens the overlap window unless the processing gate is on. Mitigation: explicit Godoc/ops guidance to co-enable the gate.
- **Startup-degraded on paced cold start (Contract §5):** mitigation: StartupTimeout sizing guidance.
- **Retry amplification (P0):** closed by per-attempt gating; integration test includes retry injection.
- **Lock-hold / `ErrRemovalPending` window:** documented (Contract §1/§2), Task 5 tests.
- **Partial progress on cancel/crash:** safe via idempotent create + diff-on-retry (Contract §3).
- **Public metrics break:** avoided via optional sidecar (D7) + compat test.
- **Claim-write residual (D4):** documented + fast-follow.
- **Test flakiness:** fake `Limiter` + `ReserveN().DelayFrom()` + RPC-counter integration (no sleeps).
- **Dependency promotion (DQ-3):** low.

---

## Decisions for Review (DQ)
1. **DQ-1 — RESOLVED:** opt-in, default OFF. ✔
2. **DQ-2 — RESOLVED:** consumer-creates now; claim-write deferred to documented fast-follow. ✔
3. **DQ-3 — RESOLVED:** use `x/time/rate` (promote indirect→direct); reversible behind the `internal/ratelimit.Limiter` interface. ✔
4. **DQ-4 — RESOLVED (non-blocking guidance):** recommended *when-enabled* starting values rate ≈ 100/s, burst ≈ 256, to be validated by load test before being cited as tuned. Default remains OFF, so this does not block implementation. ✔

---

## Self-Review
- [x] Invariant stated; per-physical-attempt scope explicit; disabled-default no-op covered.
- [x] Every "only path/caller" claim grep-verified with `file:line`; claim-write enumeration corrected (3 sites).
- [x] All throttled-RPC paths enumerated incl. retry loops; claim-write deferred (D4), not dropped.
- [x] Per-attempt gating closes the retry-amplification P0.
- [x] Handoff overlap is gate-dependent and stated as such (Contract §2); startup-timeout interaction stated (§5).
- [x] Public metrics interface not broken (D7); compat test planned.
- [x] Cancellation / no-partial-commit / lock-order designed in and tested.
- [x] Rate-backed test seam decided (`ReserveN().DelayFrom()`); no wall-clock sleeps.
- [x] Numbering consistent across Tasks / File Structure / Tests (re-verify on any future revision).

---

## Revision Log
- **Round 1 → 2:** corrected claim-write enumeration (added `manager_handoff.go:172,:226`); flipped default to opt-in/OFF (D3); added Handoff Timing Contract + Task 5; decided clock seam (Task 1); deferred claim-write to fast-follow (D4).
- **Round 2 → 3:** per-attempt gating to close the retry-amplification P0 (Invariant, D1, jsutil hook, Task 2); made handoff overlap explicitly gate-dependent (Contract §2); added StartupTimeout/readiness contract (§5); moved throttle metrics to an optional sidecar interface to avoid breaking public `types.WorkerConsumerMetrics` (D7); documented injected-limiter lock-order (D5); persisted the round-1/2 review artifacts; tests for retry-pacing, gate-on/off, startup watchdog, and metrics compat.
- **Round 3 → 4:** preserve the exported `jsutil.EnsureConsumer` signature (add sibling `EnsureConsumerWithOptions` instead of a variadic break, D1 / File Structure); softened Contract §5 to reflect the state-guarded watchdog (`manager_startup_async.go:169-176`); added a distinct `recreateFn` per-attempt test (Task 2).
- **Round 4 → 5 (precision pass):** renamed the two stale `EnsureConsumer` bullets to `EnsureConsumerWithOptions` (File Structure / Task 2); defined `WithConsumerCreateLimiter(nil)` precedence (no-op) + test; resolved DQ-3 (use `x/time/rate`, promote→direct) and DQ-4 (non-blocking guidance). No open P0/P1 — implementation-ready.
- **Implementation:** delivered Tasks 1–6 (commit `b8d9f83`); post-impl-review v1 (1 P1 / 2 P2, all test/doc) and v2 (MERGE, 0 P0 / 0 P1) closed; the one v2 P2 (positive-delay test flake) hardened. Integration tests (Task 7 + feasible Task 5) added under `test/integration/{consumer,manager}/`. **Deviations:** (a) the perf-measurement `instrumentedjs` RPC counter is a separate Go module, not importable from `test/integration`, so pacing is proven via an injected recording `ratelimit.Limiter` plus a guaranteed create-time floor; (b) the StartupTimeout watchdog cannot be triggered by apply pacing on a single leader (state moves to `Scaling` before the apply) — Contract §5 refined; the manager tests instead pin Stop-unwinds-mid-pace and reaches-Stable; (c) per-attempt-retry and recovery-storm pacing remain unit-covered; the two-worker gate-on/off overlap timing is left to the documented contract (too brittle to assert deterministically).
