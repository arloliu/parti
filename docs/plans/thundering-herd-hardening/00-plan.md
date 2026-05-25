# NATS Thundering-Herd Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reduce parti's contribution to a NATS thundering-herd during fleet-wide reassignment by adding three independent controls — (1) optional per-worker jitter on the apply path so a 20-worker fleet does not start consumer recreation in lockstep, (2) an operator-tunable bound on per-worker handoff concurrency (currently a hard-coded `errgroup.SetLimit(20)` repeated in three sites), and (3) a measurement-driven assignment-watcher idle-window debounce that collapses Raft-re-election bursts (V=N..V=N+k delivered in <100 ms) into a single apply.

**Architecture:** Three sequenced PRs against `main`, each independently shippable and each off-by-default (operator opt-in). PR-1 is purely additive (new `Config` field; default zero preserves current behavior). PR-2 is a refactor + new config field that surfaces an existing hard-coded constant. PR-3 adds a counter, runs a diagnostic that measures multi-version burst behavior, and ships an off-by-default debounce whose recommended window is derived from the diagnostic. All three are opt-in so an upgrade to the new parti version is a no-op on running fleets; operators dial in the hardening after reading the release notes. Out of scope: multi-stream sharding (already supported via running multiple `Manager` instances), NATS-server / PVC / IOPS metrics (not parti's to emit), and changing the default behavior of any of the three knobs (a future PR may flip defaults after the field has soaked in production deployments).

**Tech Stack:** Go 1.22+, NATS JetStream KV (`github.com/nats-io/nats.go/jetstream` v1.50.0), `testify`, `errgroup`, `math/rand/v2` (top-level concurrency-safe functions, already imported as `rand` in `internal/assignment/handoff/twophase.go:7`), embedded NATS server via `partitest`. No new external dependencies.

---

## Background — verified facts

These were verified against the codebase before writing this plan. Implementers do not need to re-verify, but must not contradict them.

### Apply pipeline — operator surface

- **Manager field naming.** `Manager.cfg Config` (not `m.config`) at `manager.go:53`. Tests/code that read the config use `m.cfg.<Field>`. `Manager.handoffCoordinator handoff.Coordinator` is an unexported field at `manager.go:71`, assignable from same-package `_test.go` files.
- **Constructor:** `NewManager(cfg *Config, js jetstream.JetStream, source PartitionSource, strategy AssignmentStrategy, opts ...Option) (*Manager, error)` at `manager.go:283`. Options include `WithMetrics(MetricsCollector)` at `options.go:78`. Tests requiring a hand-rolled `*Manager` (no NATS) follow the pattern at `manager_commit_state_machine_test.go:141-170`.
- **`Stop` requires a context.** `func (m *Manager) Stop(ctx context.Context) error` at `manager.go:663`. Tests must call `defer m.Stop(context.Background())`.
- **Existing test-hook precedent:** `Manager.testHookAfterApplyStore func(Assignment)` at `manager.go:189-199` is the documented pattern for production-side test-only hooks (unexported nil-default field, set ONLY by same-package tests before the relevant goroutine starts, with an explicit concurrency contract in the Godoc). PR-3's `testHookHandleAssignment` follows this pattern exactly.
- **Per-worker apply fan-out is bounded today.** `internal/assignment/handoff/twophase.go:236`, `:344`, `:398` each have `g, gCtx := errgroup.WithContext(ctx); g.SetLimit(20)`. A worker assigned 100 partitions executes at most 20 in-flight consumer-claim operations per phase. The literal `20` is **not** wired to `HandoffConfig`; it is inlined in three sites. The handoff package's `New(cfg Config, enableTwoPhase bool)` constructor (`internal/assignment/handoff/coordinator.go:107-139`) already normalizes other zero-valued config fields (e.g. `MaxRetries`, `BaseBackoff`, `SweepInterval`); PR-2 follows that pattern and normalizes `PhaseConcurrency` there rather than introducing an accessor.
- **Direct mode is single-call.** `internal/assignment/handoff/direct.go:36` calls `WorkerConsumerUpdater.UpdateWorkerConsumer(ctx, workerID, next.Partitions)` once with the full partition list. PR-2's `PhaseConcurrency` knob has no effect in direct mode by design (the loop being bounded does not exist).
- **Apply entry point is `applyAssignmentWithPrev`.** All paths (`applyAssignment`, `applyInitialAssignment`, `scheduleApplyRetry`'s coalesced retry, the commit-watcher's `handleCommitValue`, the legacy-alias `handleAssignmentEntry`) funnel into `applyAssignmentWithPrev` (`manager_assignment.go:906`). It holds `applyStoreMu` across (stale-check, `handoffCoordinator.Apply`, LSR advance, snapshot store, heartbeat ack), so it is the **only** serialization point at which a per-worker delay can be injected without breaking the LSR ordering invariant.
- **`isApplyResultStale` does NOT drop same-version duplicates** (`manager_assignment.go:885-892`). Its contract: same `Version` and same `LeaderRevision` returns `false` (intentionally — idempotent re-apply over `Assignment{}` during cold bootstrap). Same-version dedup against an already-stored snapshot lives in `handleAssignmentEntry`'s `if oldAssignment.Version >= newAssignment.Version { return }` (`manager_assignment.go:524-525`) and in `handleCommitValue`'s symmetric `commit.Version <= cur.Version` check (`manager_assignment.go:671-679`). This shapes PR-3 (see PR-3 Background fact).

### Watcher pipeline

- **The assignment watcher does NOT debounce.** `runAssignmentWatchSession` (`manager_assignment.go:449-501`) calls `handleAssignmentEntry` immediately on every `watcher.Updates()` delivery. A re-election burst that publishes V=10, V=11, V=12, V=13, V=14 inside one short window produces one apply per version. The (V, LR) stale gate inside `applyAssignmentWithPrev` does not coalesce across distinct versions — only the last apply matters for the in-memory snapshot, but the first four still call `handoffCoordinator.Apply` and execute prepare/commit/stabilize phases. This is the gap PR-3 closes.
- **Heartbeat watcher already debounces at 100 ms.** `internal/assignment/worker_monitor.go:368-390` uses a `time.NewTimer(100*time.Millisecond)` and a `pendingCheck` boolean. New entries while pending do **not** reset the timer; the timer fires once 100 ms after the first entry that flipped `pendingCheck` to true. This is **not** the right pattern for PR-3 — see Task 3.4 for the correct idle-window semantics.

### Existing jitter / retry coalescing

- **`scheduleApplyRetry` collapses pending retries to the latest version via CAS.** `manager_assignment.go:1079-1132`. A retry of V=10 that is still queued when V=11 fails-and-queues will be supplanted by V=11. This does NOT coalesce successful first-applies of distinct versions — it only collapses *retry* attempts.
- **`internal/durable/backoff.go` provides decorrelated-jitter ("Full Jitter")** consumed by the assignment-watcher restart envelope. There is no jitter on the first apply of a freshly observed assignment version. PR-1 fills that gap.

### Coordinator + ClaimStore interfaces (PR-1 + PR-2 test scaffolding)

- **`handoff.Coordinator` interface** (`internal/assignment/handoff/coordinator.go:39-55`) has exactly two methods:
  - `Start(ctx context.Context)` — **no return value**.
  - `Apply(ctx context.Context, workerID string, previous, next types.Assignment) error`.
  There is no `Stop`. Fakes must match.
- **`handoff.ClaimStore` interface** (`internal/assignment/handoff/kv_store.go:13-58`) has exactly three methods:
  - `Get(ctx, partitionID) (Claim, uint64, error)`
  - `PutIfEpoch(ctx, partitionID, expectedEpoch, next) (uint64, error)`
  - `ListKeys(ctx) ([]string, error)`
  There is no `Update` method. Fakes must intercept `PutIfEpoch`.
- **An existing in-memory `ClaimStore` test helper exists at `internal/assignment/handoff/claim_test.go:14-67`.** Type `memStore` (constructor `newMemStore()`) is the concrete base PR-2's `observingClaimStore` wraps.

### Metrics surface (PR-3 scaffolding)

- **`types.MetricsCollector` is composed**, not flat (`types/metrics_collector.go:9-18`). It embeds `ManagerMetrics`, `CalculatorMetrics`, `WorkerMetrics`, `AssignmentMetrics`, `PublisherMetrics`, `GCMetrics`, `AuditMetrics`, `WorkerConsumerMetrics`. PR-3 adds a new method to `ManagerMetrics`.
- **The no-op implementation is `internal/metrics.NopMetrics`** (`internal/metrics/nop.go:5-12`), NOT a `types.NoopMetricsCollector`. Any new interface method requires a corresponding no-op method on `*NopMetrics`.
- **`internal/metrics.PrometheusCollector` embeds `*NopMetrics`** (`internal/metrics/prometheus.go:15-16`) and overrides only the methods it implements. Constructor `NewPrometheus(reg prometheus.Registerer, namespace string) *PrometheusCollector` (`:84-92`) does **not** receive a worker ID — there is no `workerID` field. Worker-scoped labels are passed per-call: `RecordHeartbeat(workerID string, success bool)` at `:617`. PR-3's new method follows the same pattern.

### Configuration

- **`HandoffConfig` lives at `config.go:96-122`** (the parti-side handoff knob struct). It is consumed only when `EnableTwoPhaseHandoff=true` (`config.go:438+`). `Validate` runs after `SetDefaults`/`fuda.SetDefaults` (`manager.go:297-303`, `config.go:477-480`), so `default:"0"` tags are applied before validation.
- **Single-stream is by design.** `internal/ipartition/config.go:27` defines `ConsumerConfig.StreamName` as a single required string. Multi-stream is a multi-`Manager` deployment topology, out of scope here.

---

## File Structure

**Modified:**
- `config.go` — add `Config.ApplyStartJitter` (PR-1), `HandoffConfig.PhaseConcurrency` (PR-2), `Config.AssignmentWatcherDebounce` (PR-3). Extend `Validate` for all three. Godoc for all three.
- `manager_assignment.go` — inject jitter sleep at top of `applyAssignmentWithPrev` (PR-1); record `RecordApplyAttempt` adjacent to the existing `RecordStaleSnapshotStoreDropped` site (PR-3 Task 3.2); rewrite `runAssignmentWatchSession`'s select loop to an idle-window debounce (PR-3 Task 3.4).
- `internal/assignment/handoff/coordinator.go` — extend `Config` (the coordinator config, not parti's) with `PhaseConcurrency int` and normalize zero to 20 inside `New(cfg, enableTwoPhase)` alongside the existing `MaxRetries`/`BaseBackoff`/`SweepInterval` defaults (PR-2).
- `internal/assignment/handoff/twophase.go` — replace the three literal `g.SetLimit(20)` with `g.SetLimit(t.cfg.PhaseConcurrency)` (PR-2).
- `manager_setup.go` — wire `cfg.Handoff.PhaseConcurrency` into the handoff coordinator's `Config` during `setupHandoffCoordinator` (PR-2).
- `types/metrics_collector.go` — add `RecordApplyAttempt(workerID string, version int64)` to `ManagerMetrics` (PR-3).
- `internal/metrics/nop.go` — add no-op `RecordApplyAttempt` (PR-3).
- `internal/metrics/prometheus.go` — implement `RecordApplyAttempt` against a new `parti_manager_apply_attempts_total` counter labeled `worker_id` only (the `version` argument is discarded at the Prometheus collector to keep cardinality bounded; test/diagnostic collectors retain per-version detail) (PR-3).

**Created:**
- `manager_apply_jitter_test.go` — root `parti` package tests for PR-1 (jitter occurs, default is no-op, cancellation respected, no race under concurrent entrants).
- `manager_apply_jitter_helpers_test.go` — `recordingCoordinator` fake matching `handoff.Coordinator` exactly.
- `internal/assignment/handoff/twophase_concurrency_test.go` — PR-2 tests asserting concurrency=20 default, custom limit honored, and `PhaseConcurrency=1` serializes.
- `internal/metrics/prometheus_apply_attempts_test.go` — PR-3 metric registration + label test.
- `manager_apply_attempts_test.go` — PR-3 unit tests asserting `RecordApplyAttempt` fires exactly once per `applyAssignmentWithPrev` call.
- `manager_assignment_debounce_test.go` — PR-3 Task 3.4 tests for idle-window collapse, multi-version burst, and watcher-close while pending.
- `test/integration/manager/apply_coalescing_test.go` — PR-3 Task 3.3 diagnostic (skipped by default).

**Not modified (deliberately):**
- `internal/assignment/handoff/direct.go` — direct mode passes the full partition list to `WorkerConsumerUpdater` in one call. Adding a bound here would require changing the updater contract; out of scope.
- `internal/assignment/worker_monitor.go` — heartbeat-watcher debounce is already in place and is *not* the right template for PR-3 (it uses a first-event-only timer; PR-3 uses an idle-window reset-on-each-entry timer).

**Boundaries:** PR-1 (jitter) is purely additive on a hot path. PR-2 (concurrency knob) is a refactor that surfaces an existing constant; default behavior is unchanged. PR-3 measures, then closes a confirmed gap. None of the three PRs touch the cross-feature contracts pinned in `AGENTS.md` (whole-bucket-missing → Degraded, peer claim takeover, `OnDegraded` once, `Start` returns after sanity checks); each PR's pre-PR gate runs those contracts explicitly.

---

## Risk Assessment

All three PRs are opt-in by design: with zero-valued defaults (`ApplyStartJitter=0`, `PhaseConcurrency=0`→20-via-normalization, `AssignmentWatcherDebounce=0`), an upgrade from `main` to the post-PR-3 version produces **no observable behavior change**. The risk envelope is therefore dominated by (a) the code-side refactor PR-1 introduces (extract-core), (b) operator-side misuse when knobs are enabled, and (c) the small surface of new fields and methods on hot paths.

### Per-PR risk

| PR | Risk | Reasoning |
|---|---|---|
| **PR-1 — `ApplyStartJitter`** | **LOW** | Default 0 is a no-op; the new sleep is interruptible via `m.ctx.Done()`. The largest code change is the extract of `applyAssignmentWithPrev` body into `applyAssignmentWithPrevCore`, but the extract preserves all existing behavior bit-for-bit (verified by codex against `manager_assignment.go:906-997`). Misconfigured `ApplyStartJitter > StartupTimeout` is bounded by `Validate`'s 10s cap and by the documented `<= StartupTimeout/4` guidance + pinning test. Retries do NOT pay the jitter (separate `applyAssignmentWithPrevSkipJitter` path); test `TestApplyAssignmentRetry_DoesNotJitter` drives the real `scheduleApplyRetry` goroutine. Cross-feature contracts unaffected at default 0. Reversibility: trivial — unset the field. |
| **PR-2 — `PhaseConcurrency`** | **LOW** | Pure surface-up of an existing inlined constant. `handoff.New` normalizes 0→20 alongside existing zero-defaulting (`MaxRetries`, `BaseBackoff`, `SweepInterval`), so an omitted field produces identical behavior to main. The dangerous misconfiguration is `PhaseConcurrency=0` reaching `errgroup.SetLimit(0)` (which would deadlock); the defaulting in `New` + `TestTwoPhase_PhaseConcurrency_DefaultsTo20`'s `peak > 1` assertion pin this. `PhaseConcurrency=1` (serial mode) is explicitly documented and tested. Only active when `EnableTwoPhaseHandoff=true` — direct mode is untouched. Reversibility: trivial — unset the field. |
| **PR-3 — `RecordApplyAttempt` + `AssignmentWatcherDebounce`** | **LOW–MEDIUM** | Counter is observation-only; Prometheus impl is single-label (`{worker_id}`) so cardinality is bounded by fleet size. Debounce default 0 = off → no behavior change at upgrade. When enabled, the debounce changes apply timing on the hot watcher path; the design includes (1) `ctx.Err()` guard on channel-close branch to avoid Stop-races, (2) explicit no-flush on `ctx.Done()`, (3) idle-window reset-on-each-entry semantics distinct from the heartbeat-watcher template, (4) hand-rolled fake watcher tests for burst-collapse, drip-reset, cancel-no-flush, and close-flush. The medium element: the debounce wraps a select loop that previously had only two arms (ctx + Updates) and now has four (ctx + Updates + timer + reconcile); the reset-on-each-entry timer Stop/drain/Reset pattern is subtle. Reversibility: trivial — unset the field. |

### Aggregate risk

**Upgrade-time risk: VERY LOW.** No default behavior change in any PR. An operator who upgrades and does not edit config gets the same runtime semantics as on `main`. The four cross-feature contracts (`TestManager_LiveNATSBucketLoss`, `TestStableID_StaleKeyTakeover_Reclaim`, `TestManager_LiveNATSBucketLoss_OnDegradedHook`, `TestStart_ReturnsBeforeStable`) are re-run in each PR's pre-PR gate.

**Misconfiguration risk: LOW.** `Validate()` rejects negative or absurd values for all three knobs (caps at 10s for `ApplyStartJitter`, 256 for `PhaseConcurrency`, 1s for `AssignmentWatcherDebounce`). The operator-facing contract is documented in field Godoc and reinforced by tests (especially the `=1 serial` and `>StartupTimeout` cases).

**Refactor risk: LOW.** PR-1's extract-core preserves the existing apply body verbatim; PR-2 normalizes a single integer in an existing constructor; PR-3 adds a select arm and a metric call. None of the changes touches Raft, KV CAS, leader-election protocol, claim state machine, or any other invariant carrier.

**Operational risk: LOW.** The opt-in debounce ships with a release-note flow: deploy → diagnostic → enable with measured value → re-run diagnostic to confirm collapse. Operators who skip the diagnostic and enable a too-small window will see reduced (but not zero) coalescing benefit; operators who enable a too-large window will see inflated reassignment latency bounded by `Validate`'s 1s cap.

**Rollback story.** All three knobs are reversible by setting the field to 0 and restarting the manager. Since defaults are no-op, even a complete unwind of operator config is safe. There is no schema migration, no on-disk format change, no KV bucket reshape — nothing requires forward/back compatibility handling.

### Risk if shipped buggy

| Scenario | Severity | Mitigation in plan |
|---|---|---|
| Jitter sleep deadlocks (forgets to honor `ctx.Done()`) | Medium — workers stuck mid-Stop | `TestApplyAssignmentWithPrev_JitterCancelledByCtx` pins ctx-cancellation aborts the sleep |
| Jitter races against concurrent apply entrants | High — data race | `TestApplyAssignmentWithPrev_JitterNoRaceUnderConcurrentEntrants` under `-race`; no shared state writes before lock (verified across 5 review rounds) |
| `PhaseConcurrency=0` reaches `SetLimit(0)` (deadlock) | High — apply hang | Normalized in `handoff.New`; pinned by `TestTwoPhase_PhaseConcurrency_DefaultsTo20`'s `peak > 1` assertion |
| Debounce drops a pending entry on Stop and re-applies stale state on restart | High — split-brain risk on next leader cycle | Pending entries are intentionally dropped on `ctx.Done()`; next watcher session re-reads current state via the reconcile arm and the version gate ensures idempotency |
| Debounce timer leaks on session exit | Medium — goroutine accumulation | `defer watcher.Stop()` + timer is local to the function scope; no separate goroutine spawned |
| Prometheus counter cardinality explosion | Medium — Prometheus OOM | Single-label `{worker_id}`; pinned by `TestPrometheus_RecordApplyAttempt_BoundedLabels` |
| Retry path picks up jitter (compound delay) | Low — recovery latency inflation | `applyAssignmentWithPrevSkipJitter` sibling; pinned by `TestApplyAssignmentRetry_DoesNotJitter` driving the actual scheduler |

### Recommendation

Ship as three independent PRs in order (PR-1 → PR-2 → PR-3). Each PR is independently revertable. Recommend a two-week soak between PR-3 merge and any decision to flip the debounce default on; during soak, operators run the diagnostic on at least one production cluster and observe whether `AGGREGATE max_burst_size` justifies the change.

---

## PR-1 — Per-worker apply-start jitter

Goal: when a fresh assignment version lands on a 20-worker fleet, workers spread their apply start times across a configurable window rather than firing in lockstep.

### Task 1.1: Add `Config.ApplyStartJitter` with validation

**Files:**
- Modify: `config.go` (after the existing `Handoff HandoffConfig` field around `:454`; extend `Validate`)
- Test: `config_test.go` (add validation cases)

- [ ] **Step 1: Write the failing validation test**

In `config_test.go`, add:

```go
func TestConfig_ApplyStartJitter_Validation(t *testing.T) {
	cases := []struct {
		name    string
		jitter  time.Duration
		wantErr bool
	}{
		{"zero is allowed (disables jitter)", 0, false},
		{"positive within cap is allowed", 500 * time.Millisecond, false},
		{"negative is rejected", -1 * time.Millisecond, true},
		{"above 10s cap is rejected", 11 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := TestConfig()
			cfg.ApplyStartJitter = tc.jitter
			err := cfg.Validate()
			if tc.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "ApplyStartJitter")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test . -race -run TestConfig_ApplyStartJitter_Validation -v`
Expected: FAIL — field does not exist yet.

- [ ] **Step 3: Add the field and validation**

In `config.go`, add the field after `Handoff`:

```go
// ApplyStartJitter, when > 0, randomly delays fresh-version applies
// (watcher / commit / alias / initial-bootstrap, all funneled through
// applyAssignmentWithPrev) by a uniformly distributed duration in
// [0, ApplyStartJitter) before taking applyStoreMu. This spreads
// JetStream consumer create/destroy load across a worker fleet that
// observed the same new assignment version simultaneously (e.g. after
// a leader re-election).
//
// Retries (scheduleApplyRetry) do NOT pay this jitter. The retry path
// has its own exponential-backoff envelope with decorrelated jitter;
// compounding apply-start jitter on top would inflate recovery latency
// without spreading any fleet because a retry is one worker, not a fleet.
//
// Default 0 disables jitter (backwards compatible). Recommended starting
// point for a 20-worker fleet creating 100 consumers each: 500ms.
//
// Hard-capped at 10s by Validate(): a larger value would dwarf the
// LSR-advancement bookkeeping and risk masking apply failures behind
// what would look like apply latency.
//
// Startup-runner consequence: the startup background runner's first
// apply (applyInitialAssignment, manager_startup_async.go) also funnels
// through applyAssignmentWithPrev for non-empty commits/aliases. With
// jitter enabled, the runner sleeps up to ApplyStartJitter before its
// first apply attempt. The soft watchdog measures StartupTimeout from
// Start invocation (AGENTS.md "Apply boundedness" section), so operators
// MUST configure ApplyStartJitter << StartupTimeout. Recommended:
// ApplyStartJitter <= StartupTimeout / 4. A focused test pins this
// (TestApplyStartJitter_StartupBudget).
ApplyStartJitter time.Duration `yaml:"applyStartJitter" default:"0" validate:"gte=0"`
```

In `Validate()`, after the existing `EnableTwoPhaseHandoff` block, add:

```go
if cfg.ApplyStartJitter < 0 {
	return errors.New("ApplyStartJitter must be >= 0")
}
if cfg.ApplyStartJitter > 10*time.Second {
	return errors.New("ApplyStartJitter must be <= 10s")
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `CGO_ENABLED=1 go test . -race -run TestConfig_ApplyStartJitter_Validation -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go
git commit -m "feat(config): add ApplyStartJitter knob with validation"
```

### Task 1.2: Inject jitter at the top of `applyAssignmentWithPrev`

**Files:**
- Modify: `manager_assignment.go:906` (entry of `applyAssignmentWithPrev`, BEFORE `applyStoreMu.Lock()`)
- Create: `manager_apply_jitter_helpers_test.go` (the fake `Coordinator`)
- Create: `manager_apply_jitter_test.go` (the tests)

**Design notes (responding to plan-review v1 P0 + v2 P1):**
- No production-side instrumentation field. Tests measure elapsed time via a local `time.Now()` captured *inside the test* before invoking `m.applyAssignment`, then read in the fake `Coordinator.Apply` callback. No `Manager.applyEnterT`.
- No per-Manager PRNG, no mutex. Use top-level `rand.Int64N` from `math/rand/v2`. The repo already imports `rand "math/rand/v2"` (`internal/assignment/handoff/twophase.go:7`) and uses `rand.Float64()` for jitter; v2 top-level functions are documented as safe for concurrent use.
- A focused `-race` test exercises two concurrent apply entrants with jitter enabled (see Task 1.2 Step 5).
- Test helpers use `NewManager(&cfg, js, source, strategy, parti.WithMetrics(rm))` — the public constructor with the `WithMetrics` option. There is no `cfg.MetricsCollector` field.
- Tests call `defer m.Stop(context.Background())` — `Stop` requires a context (`manager.go:663`).
- The jitter implementation reads the config field as `m.cfg.ApplyStartJitter` (`manager.go:53` defines the field as `cfg Config`, not `config`).

- [ ] **Step 1: Create the test helper file with a fake `Coordinator` matching the real interface**

Create `manager_apply_jitter_helpers_test.go`:

```go
package parti

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/types"
)

// recordingCoordinator implements handoff.Coordinator exactly:
//   - Start(ctx context.Context)           — no return value
//   - Apply(ctx, workerID, prev, next types.Assignment) error
// (There is no Stop method on handoff.Coordinator — see
// internal/assignment/handoff/coordinator.go:39-55.)
//
// Apply invokes onApply (if set), then returns applyErr (defaults nil).
type recordingCoordinator struct {
	applyCount atomic.Int64
	onApply    func(ctx context.Context)
	applyErr   error
}

func (r *recordingCoordinator) Start(ctx context.Context) {
	// no-op
}

func (r *recordingCoordinator) Apply(
	ctx context.Context,
	workerID string,
	previous types.Assignment,
	next types.Assignment,
) error {
	r.applyCount.Add(1)
	if r.onApply != nil {
		r.onApply(ctx)
	}
	return r.applyErr
}

// Compile-time assertion the fake matches the real interface.
var _ handoff.Coordinator = (*recordingCoordinator)(nil)

// IMPORTANT: this fixture is NOT Stop-safe. It deliberately leaves
// m.election, m.source, m.idClaimer, and other lifecycle dependencies
// nil because the apply-path tests do not need them. Tests using this
// fixture MUST NOT call m.Stop() — Stop dereferences those nil fields
// (manager.go:706-747) and will panic. Cancel via t.Cleanup(cancel)
// (registered below) instead.
func newTestManagerWithJitter(t *testing.T, jitter time.Duration) *Manager {
	t.Helper()
	// Build the hand-rolled fixture mirroring the pattern at
	// manager_commit_state_machine_test.go:141-170. The fixture must
	// initialize at minimum the fields that applyAssignmentWithPrev
	// reads:
	//   - cfg (with ApplyStartJitter set)
	//   - ctx, cancel
	//   - logger
	//   - metrics
	//   - hooks (zero-value *Hooks is fine; invokeHook tolerates nil
	//     individual hook fields)
	//   - handoffCoordinator (will be overwritten by the test)
	//   - heartbeat (a stub that no-ops SetAppliedAssignment / PublishNow)
	//   - workerID (a fixed string)
	//   - assignment (atomic.Pointer initialized to &Assignment{})
	//   - lastSeenLeaderRevision (atomic.Int64 zero is fine)
	//   - applyStoreMu (zero value)
	// See manager.go:52-90 for the full Manager field set and the
	// existing fixture in manager_commit_state_machine_test.go for the
	// exact construction pattern.
	cfg := TestConfig()
	cfg.ApplyStartJitter = jitter
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	m := buildTestManagerFixture(t, cfg, ctx, cancel) // shared helper
	return m
}
```

`buildTestManagerFixture` is the shared base used by `newTestManagerWithJitter`, `newTestManagerWithMetrics`, and `newTestManagerForDebounce`. Its body is the field-by-field construction mirroring `manager_commit_state_machine_test.go:141-170` plus the additional fields listed above. Implementer: create this helper once; the three named helpers wrap it with their specific config overrides.

- [ ] **Step 2: Write the failing jitter-applied tests**

Create `manager_apply_jitter_test.go`:

```go
package parti

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestApplyAssignmentWithPrev_JitterApplied verifies that when
// ApplyStartJitter is set, applyAssignmentWithPrev sleeps for a duration
// in [0, ApplyStartJitter) before invoking handoffCoordinator.Apply.
// Measurement is done locally inside the test — no production field needed.
func TestApplyAssignmentWithPrev_JitterApplied(t *testing.T) {
	const jitter = 200 * time.Millisecond
	m := newTestManagerWithJitter(t, jitter)
	// NOTE: this fixture is not Stop-safe; cancellation runs via t.Cleanup.

	var observed atomic.Int64
	rc := &recordingCoordinator{
		onApply: func(ctx context.Context) {
			// 'start' is captured below; copy via closure-local variable.
		},
	}
	m.handoffCoordinator = rc

	start := time.Now()
	rc.onApply = func(ctx context.Context) {
		observed.Store(time.Since(start).Nanoseconds())
	}
	_ = m.applyAssignment(Assignment{Version: 1})

	elapsed := time.Duration(observed.Load())
	require.GreaterOrEqual(t, elapsed, time.Duration(0))
	require.LessOrEqual(t, elapsed, jitter+50*time.Millisecond)
}

// TestApplyAssignmentWithPrev_JitterZeroIsNoop verifies that the default
// jitter=0 introduces no measurable delay.
func TestApplyAssignmentWithPrev_JitterZeroIsNoop(t *testing.T) {
	m := newTestManagerWithJitter(t, 0)
	// NOTE: this fixture is not Stop-safe; cancellation runs via t.Cleanup.

	var observed atomic.Int64
	rc := &recordingCoordinator{}
	m.handoffCoordinator = rc

	start := time.Now()
	rc.onApply = func(ctx context.Context) {
		observed.Store(time.Since(start).Nanoseconds())
	}
	_ = m.applyAssignment(Assignment{Version: 1})

	require.Less(t, time.Duration(observed.Load()), 5*time.Millisecond)
}

// TestApplyAssignmentWithPrev_JitterCancelledByCtx verifies that ctx
// cancellation during the jitter sleep aborts the apply and Apply was
// never invoked.
func TestApplyAssignmentWithPrev_JitterCancelledByCtx(t *testing.T) {
	m := newTestManagerWithJitter(t, 5*time.Second)
	// NOTE: this fixture is not Stop-safe; cancellation runs via t.Cleanup.

	rc := &recordingCoordinator{}
	m.handoffCoordinator = rc

	go func() {
		time.Sleep(50 * time.Millisecond)
		m.cancel() // simulate Stop's ctx cancellation; fixture is not Stop-safe
	}()

	start := time.Now()
	err := m.applyAssignment(Assignment{Version: 1})
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, elapsed, 1*time.Second)
	require.Zero(t, rc.applyCount.Load(), "Apply must not be called when ctx is cancelled mid-jitter")
}

// TestApplyAssignmentWithPrev_JitterNoRaceUnderConcurrentEntrants exercises
// the race-detector requirement raised by plan-review v1 P0. Two goroutines
// invoke applyAssignment concurrently with jitter enabled. Under -race,
// any shared-state write that escaped applyStoreMu would be flagged.
func TestApplyAssignmentWithPrev_JitterNoRaceUnderConcurrentEntrants(t *testing.T) {
	m := newTestManagerWithJitter(t, 50*time.Millisecond)
	// NOTE: this fixture is not Stop-safe; cancellation runs via t.Cleanup.

	rc := &recordingCoordinator{}
	m.handoffCoordinator = rc

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = m.applyAssignment(Assignment{Version: 1})
	}()
	go func() {
		defer wg.Done()
		_ = m.applyAssignment(Assignment{Version: 2})
	}()
	wg.Wait()

	// At least the higher version must have been applied; the lower
	// version may be dropped by the stale gate. The point of the test is
	// the race detector, not the count.
	require.GreaterOrEqual(t, rc.applyCount.Load(), int64(1))
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `CGO_ENABLED=1 go test . -race -run TestApplyAssignmentWithPrev_Jitter -v`
Expected: FAIL — `applyAssignmentWithPrev` does not yet sleep.

- [ ] **Step 4: Implement the jitter sleep — extract core, exempt retries**

**Design correction (responding to plan-review v3 P1):** the jitter sleep must NOT fire on `scheduleApplyRetry`'s re-entry. Retries already pay their own exponential backoff in `scheduleApplyRetry` (`manager_assignment.go:1079-1132`); adding `[0, ApplyStartJitter)` on top would compound the latency for no fleet-spread benefit (the retry is for a single worker, not a fleet). The fix is a small refactor: extract the existing `applyAssignmentWithPrev` body (minus jitter) into a `applyAssignmentWithPrevCore(old, new)` helper, then have `scheduleApplyRetry`'s goroutine call a non-jittering sibling.

Step 4a: extract the core.

In `manager_assignment.go`, rename the body of the existing `applyAssignmentWithPrev` (`:906`) to `applyAssignmentWithPrevCore(oldAssignment, newAssignment Assignment) error`. Keep its signature, lock acquisition (`m.applyStoreMu.Lock()`), stale gate, `handoffCoordinator.Apply`, LSR advance, snapshot store, heartbeat ack — everything as it is on `main` today. The only line that does NOT move into `core` is the new jitter sleep added below.

Step 4b: re-add `applyAssignmentWithPrev` as a thin wrapper that jitters then calls core.

In `manager_assignment.go`:

```go
// applyAssignmentWithPrev is the fresh-version apply entry (watcher,
// commit, alias, initial-bootstrap). It jitters once before calling
// core, so a fleet of workers observing the same fresh version spreads
// its JetStream consumer create/destroy load.
//
// Retries (scheduleApplyRetry) must NOT jitter — see
// applyAssignmentWithPrevSkipJitter below. Retries already pay their
// own exponential backoff; adding jitter on top compounds latency for
// no fleet-spread benefit (the retry is one worker, not a fleet).
func (m *Manager) applyAssignmentWithPrev(oldAssignment, newAssignment Assignment) error {
	// PR-1 — fleet-wide apply spread.
	// Sleep [0, ApplyStartJitter) BEFORE acquiring applyStoreMu (in
	// core) so a fleet that observed the same new assignment version
	// simultaneously (e.g. post leader re-election) spreads JetStream
	// consumer create/destroy load. Sleep is interruptible by ctx so
	// Stop() aborts promptly.
	//
	// No shared-state writes happen here. math/rand/v2 top-level
	// functions are concurrency-safe (math/rand/v2/rand.go:12-13); the
	// repo already uses them for handoff jitter in
	// internal/assignment/handoff/twophase.go:190-195.
	if jitter := m.cfg.ApplyStartJitter; jitter > 0 {
		d := m.sampleApplyJitter(jitter)
		select {
		case <-time.After(d):
		case <-m.ctx.Done():
			return m.ctx.Err()
		}
	}

	return m.applyAssignmentWithPrevCore(oldAssignment, newAssignment)
}

// applyAssignmentWithPrevSkipJitter is the retry entry. It calls core
// directly, skipping the apply-start jitter sleep. Used by
// scheduleApplyRetry so a retry's compound delay is bounded by the
// retry-backoff envelope alone.
func (m *Manager) applyAssignmentWithPrevSkipJitter(oldAssignment, newAssignment Assignment) error {
	return m.applyAssignmentWithPrevCore(oldAssignment, newAssignment)
}
```

Ensure the file's import block has `rand "math/rand/v2"`. If `math/rand/v2` is not already imported, add it.

Add the sampler method on `*Manager`. It accepts a unexported `applyJitterSampler` override so tests can force deterministic durations:

```go
// applyJitterSampler, when non-nil, overrides ApplyStartJitter's random
// sampling. Set ONLY by tests in this package (same contract as
// testHookAfterApplyStore at manager.go:189-199): assign before the
// goroutine that calls applyAssignmentWithPrev starts, do not mutate
// afterwards. Production MUST leave this nil.
applyJitterSampler func(max time.Duration) time.Duration

// sampleApplyJitter returns the jitter sleep duration for one apply
// invocation. Defaults to a uniform sample in [0, max) via math/rand/v2's
// top-level (concurrency-safe) Int64N. Tests override applyJitterSampler
// to force specific durations.
func (m *Manager) sampleApplyJitter(max time.Duration) time.Duration {
	if s := m.applyJitterSampler; s != nil {
		return s(max)
	}
	//nolint:gosec // jitter does not require crypto secure random
	return time.Duration(rand.Int64N(int64(max)))
}
```

The deterministic startup-budget test (Step 7) uses `m.applyJitterSampler = func(_ time.Duration) time.Duration { return ... }` to force the desired duration.

Step 4c: change `scheduleApplyRetry`'s retry goroutine to use the SkipJitter entry.

In `manager_assignment.go`, find the existing retry call at `:1111-1116`:

```go
if err := m.applyAssignment(*pending); err != nil {
	// applyAssignment already re-stashed the failure; keep going.
}
```

`applyAssignment(new)` → `applyAssignmentWithPrev(...)` → jitters. Replace with the SkipJitter form, computing `prev` inline:

```go
prev := m.CurrentAssignment()
if err := m.applyAssignmentWithPrevSkipJitter(prev, *pending); err != nil {
	// applyAssignmentWithPrevSkipJitter already re-stashed via core's
	// failure path; keep going.
}
```

This bypasses `applyAssignment`'s implicit re-jitter.

Step 4d: add the regression test.

In `manager_apply_jitter_test.go`, add:

**Test seam — `testHookApplyJittered`.** To prove `scheduleApplyRetry` actually routes to the SkipJitter path (rather than just proving SkipJitter itself doesn't sleep), add an observation hook that the fresh-version wrapper sets and the SkipJitter path never sets. The retry test then observes whether the scheduler hit the jittering route.

In `manager.go`, add the field with the same nil-default contract as `testHookAfterApplyStore`:

```go
// testHookApplyJittered, when non-nil, is invoked synchronously inside
// the fresh-version applyAssignmentWithPrev wrapper IMMEDIATELY before
// the jitter sleep begins (and only when ApplyStartJitter > 0).
// applyAssignmentWithPrevSkipJitter (the retry path) does NOT invoke
// this hook. Used by TestApplyAssignmentRetry_DoesNotJitter to prove
// scheduleApplyRetry routes to the non-jittering sibling.
//
// Concurrency contract: same as testHookAfterApplyStore at
// manager.go:189-199 — set ONLY by same-package tests before the
// relevant goroutine starts, never mutated after. Production MUST leave
// this nil.
testHookApplyJittered func()
```

Wire it in `applyAssignmentWithPrev`'s jitter prologue:

```go
if jitter := m.cfg.ApplyStartJitter; jitter > 0 {
	if hook := m.testHookApplyJittered; hook != nil {
		hook()
	}
	d := m.sampleApplyJitter(jitter)
	// ...select as before
}
```

**Coordinator extension** — `recordingCoordinator` gets a deterministic fail-first-N-attempts mechanism so the test does NOT mutate `applyErr` mid-flight (which would be a data race under `-race`):

```go
// In manager_apply_jitter_helpers_test.go, extend recordingCoordinator:
type recordingCoordinator struct {
	applyCount     atomic.Int64
	failUntilCount atomic.Int64 // Apply returns synthetic failure while applyCount <= this; 0 disables
	onApply        func(ctx context.Context)
}

func (r *recordingCoordinator) Apply(
	ctx context.Context, workerID string, previous, next types.Assignment,
) error {
	n := r.applyCount.Add(1)
	if r.onApply != nil {
		r.onApply(ctx)
	}
	if u := r.failUntilCount.Load(); u > 0 && n <= u {
		return errors.New("synthetic apply failure")
	}
	return nil
}
```

All synchronization is via atomics; no mutex, no race-detector hazard.

Then the test drives the actual scheduler with a deterministic first-fail / second-succeed shape:

```go
// TestApplyAssignmentRetry_DoesNotJitter proves that scheduleApplyRetry's
// retry goroutine routes through applyAssignmentWithPrevSkipJitter (the
// non-jittering sibling), so a fleet-wide ApplyStartJitter does NOT
// compound on top of the retry's own exponential backoff.
func TestApplyAssignmentRetry_DoesNotJitter(t *testing.T) {
	const jitter = 2 * time.Second
	m := newTestManagerWithJitter(t, jitter)

	// Count fresh-wrapper jitter entries via the test hook. Hook fires
	// BEFORE the sleep so the test does not block on it.
	var jitterFires atomic.Int64
	m.testHookApplyJittered = func() { jitterFires.Add(1) }

	// Coordinator: first Apply fails (n==1 <= failUntilCount==1), second
	// succeeds. Deterministic, no mid-flight mutation.
	rc := &recordingCoordinator{}
	rc.failUntilCount.Store(1)
	m.handoffCoordinator = rc

	// Drive a fresh-version apply. It fails, scheduleApplyRetry queues
	// a retry. The retry succeeds, terminating the loop.
	go func() { _ = m.applyAssignment(Assignment{Version: 1}) }()

	// Wait for both attempts to complete.
	require.Eventually(t, func() bool {
		return rc.applyCount.Load() >= 2
	}, 10*time.Second, 50*time.Millisecond,
		"expected fresh attempt + retry; saw applyCount=%d", rc.applyCount.Load())

	// The fresh attempt jittered once. The retry must NOT jitter; total
	// expected is exactly 1.
	require.Equal(t, int64(1), jitterFires.Load(),
		"scheduleApplyRetry must route through SkipJitter; jitter hook fired %d times (expected 1 fresh attempt only)",
		jitterFires.Load())
}
```

This test drives the actual `scheduleApplyRetry` goroutine and pins the routing — not just SkipJitter's local behavior. Race-clean under `-race`: all coordinator state is read/written via atomics; the test mutates no shared state after `go ...`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test . -race -run TestApplyAssignmentWithPrev_Jitter -v`
Expected: PASS — all four sub-tests.

- [ ] **Step 6: Run the full unit suite to check no regressions**

Run: `make test`
Expected: PASS, no new failures. Pay particular attention to any test that asserts on apply-completion timing (search `grep -n "applyAssignment\|applyAssignmentWithPrev" *_test.go`); none should be sensitive at the 0 ms default.

- [ ] **Step 7: Add the startup-budget regression test**

The plan-review v2 P1 raised that with jitter enabled, the startup background runner sleeps before its first apply, which can race the soft startup watchdog (`AGENTS.md` "Apply boundedness"; `manager_startup_async.go`). Pin this with a focused integration test in `test/integration/manager/`:

```go
// TestApplyStartJitter_StartupBudget_Positive verifies that a small
// jitter (well below StartupTimeout) does NOT trigger startup-timeout
// Degraded. Uses the deterministic applyJitterSampler seam to force the
// jitter duration; no reliance on the PRNG.
//
// Boots a single manager via the existing integration test harness
// (model on TestStart_ReturnsBeforeStable). The non-empty initial apply
// path must be exercised — the cold-empty bootstrap bypasses
// applyAssignmentWithPrev (manager.go:607-646), so the test setup must
// seed an initial non-empty assignment.
func TestApplyStartJitter_StartupBudget_Positive(t *testing.T) {
	cfg := testutil.IntegrationTestConfig()
	cfg.ApplyStartJitter = 5 * time.Second // operator setting; sampler is forced
	cfg.StartupTimeout = 5 * time.Second

	// Build manager via integration harness; before m.Start(), inject
	// the deterministic sampler.
	m := buildIntegrationManager(t, cfg) // helper from test/integration/manager
	m.applyJitterSampler = func(_ time.Duration) time.Duration {
		return 200 * time.Millisecond // 4% of StartupTimeout — safely within budget
	}
	// Seed a non-empty initial assignment so applyInitialAssignment takes
	// the non-empty path and exercises applyAssignmentWithPrev's jitter.

	require.NoError(t, m.Start(context.Background()))
	require.NoError(t, m.WaitState(StateStable, 10*time.Second))
	// Assert NO Degraded transition with reason "startup-timeout":
	require.NotContains(t, collectDegradedReasons(m), "startup-timeout")
}

// TestApplyStartJitter_StartupBudget_Negative deterministically forces a
// jitter sample LARGER than StartupTimeout and asserts the soft watchdog
// fires Degraded with reason "startup-timeout".
func TestApplyStartJitter_StartupBudget_Negative(t *testing.T) {
	cfg := testutil.IntegrationTestConfig()
	cfg.ApplyStartJitter = 5 * time.Second // operator setting; sampler is forced
	cfg.StartupTimeout = 200 * time.Millisecond

	m := buildIntegrationManager(t, cfg)
	m.applyJitterSampler = func(_ time.Duration) time.Duration {
		return 2 * time.Second // 10x StartupTimeout — deterministically misses budget
	}
	// Seed a non-empty initial assignment (same as positive case).

	require.NoError(t, m.Start(context.Background()))

	// Wait long enough for the watchdog to fire.
	require.Eventually(t, func() bool {
		return containsReason(collectDegradedReasons(m), "startup-timeout")
	}, 3*time.Second, 50*time.Millisecond)
}
```

The two helpers `buildIntegrationManager(t, cfg)` and `collectDegradedReasons(m)` either already exist in `test/integration/manager/` or are trivial to add by mirroring the pattern used in `TestStart_ReturnsBeforeStable`'s neighbors. The non-empty-initial-assignment seeding is the load-bearing precondition — implementers must verify their test setup does NOT take the cold-empty bypass (`manager.go:607-646`).

Import block for the new test file:

```go
import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/stretchr/testify/require"
)
```

`testutil.IntegrationTestConfig()` is the canonical integration-test config builder at `internal/testutil/nats.go:36-38`. Existing integration tests already use it (`test/integration/manager/manager_live_bucket_loss_test.go:37-41`, `manager_startup_async_test.go:24-31`). `parti.TestConfig()` is a different (root-package) helper for unit tests; do not confuse them.

- [ ] **Step 8: Commit**

```bash
git add manager_assignment.go manager_apply_jitter_test.go manager_apply_jitter_helpers_test.go test/integration/manager/apply_jitter_startup_test.go
git commit -m "feat(manager): jitter apply start to spread fleet-wide reassignment"
```

### Task 1.3: Pre-PR gate for PR-1

- [ ] **Step 1: Run `make pre-pr`**

Run: `make pre-pr`
Expected: PASS. The `pre-pr` target chains lint + `make test -race` + `make test-integration`. PR-1 touches `manager_assignment.go`, on the AGENTS.md pre-PR-required list.

- [ ] **Step 2: Run the cross-feature contracts explicitly**

```bash
CGO_ENABLED=1 go test ./test/integration/manager/ -race -run 'TestManager_LiveNATSBucketLoss|TestManager_LiveNATSBucketLoss_OnDegradedHook|TestStart_ReturnsBeforeStable' -v
CGO_ENABLED=1 go test ./test/integration/stableid/ -race -run TestStableID_StaleKeyTakeover_Reclaim -v
```

Expected: PASS. PR-1's jitter sleep is interruptible and writes nothing before `applyStoreMu.Lock()`, so the four pinned invariants are preserved.

- [ ] **Step 3: Open PR**

```bash
git push -u origin <branch>
gh pr create --title "feat(manager): apply-start jitter for fleet-wide spread" --body "$(cat <<'EOF'
## Summary
- Adds optional `Config.ApplyStartJitter` (default 0; no behavior change)
- When > 0, `applyAssignmentWithPrev` sleeps a uniform [0, jitter) duration before taking applyStoreMu
- Spreads JetStream consumer create/destroy load when N workers observe the same new assignment version simultaneously
- Recommended starting point: 500ms for a 20-worker / 100-partition fleet

## Test plan
- [x] `make pre-pr` (lint + unit-race + integration)
- [x] Cross-feature contracts (bucket-loss, peer-takeover, OnDegraded-once, Start-returns-before-Stable)
- [x] `TestApplyAssignmentWithPrev_JitterNoRaceUnderConcurrentEntrants` under -race
EOF
)"
```

---

## PR-2 — Configurable handoff phase concurrency

Goal: surface the hard-coded `g.SetLimit(20)` repeated in `internal/assignment/handoff/twophase.go` as `HandoffConfig.PhaseConcurrency`, so operators can lower it under pressure (e.g. to 5) or raise it on capable clusters. Default 20 preserves current behavior.

### Task 2.1: Add `HandoffConfig.PhaseConcurrency` field + validation

**Files:**
- Modify: `config.go:96-122` (add field to `HandoffConfig`); `Validate` extension.
- Test: `config_test.go` (validation cases).

**Operator contract (responding to plan-review v1 P2):**
- `PhaseConcurrency == 0` → use default `20` (preserves historical behavior under field omission).
- `PhaseConcurrency == 1` → strictly serial (one in-flight per partition per phase).
- `PhaseConcurrency >= 2 && <= 256` → that exact bound.
- `PhaseConcurrency > 256` → rejected by `Validate`.
- `PhaseConcurrency < 0` → rejected by `Validate`.

Godoc documents this contract; Task 2.3 tests prove `PhaseConcurrency=1` is honored and never exceeds 1 in-flight.

- [ ] **Step 1: Write the failing validation test**

In `config_test.go`:

```go
func TestHandoffConfig_PhaseConcurrency_Validation(t *testing.T) {
	cases := []struct {
		name        string
		concurrency int
		wantErr     bool
	}{
		{"zero is allowed (sentinel for default 20)", 0, false},
		{"one is allowed (serial mode)", 1, false},
		{"positive within cap is allowed", 50, false},
		{"negative is rejected", -1, true},
		{"above 256 cap is rejected", 257, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := TestConfig()
			cfg.EnableTwoPhaseHandoff = true
			cfg.HandoffBucket = "test-handoff"
			cfg.HandoffTTL = 1 * time.Minute
			cfg.Handoff.PhaseConcurrency = tc.concurrency
			err := cfg.Validate()
			if tc.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "PhaseConcurrency")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=1 go test . -race -run TestHandoffConfig_PhaseConcurrency_Validation -v`
Expected: FAIL — field does not exist.

- [ ] **Step 3: Add the field**

In `config.go`, inside `HandoffConfig`, after the `Jitter` field:

```go
// PhaseConcurrency caps the number of in-flight per-partition KV claim
// operations during each of the prepare, commit, and stabilize phases
// of the two-phase handoff coordinator. A worker reassigned 100
// partitions completes each phase in ceil(100 / effective_limit) waves.
//
// Operator contract:
//   - 0   → use default 20 (preserves the historical hard-coded behavior
//                          when this field is omitted or zero-valued).
//   - 1   → strictly serial (one in-flight per phase). Useful when NATS
//                          headroom is exhausted.
//   - 2..256 → exact bound.
//
// To request "low concurrency", set 5–10. Do NOT set 0 expecting "no
// parallelism"; 0 is the default-20 sentinel.
//
// Hard-capped at 256 by Validate(): higher values stop being meaningful
// because KV operations queue server-side regardless of client parallelism.
PhaseConcurrency int `yaml:"phaseConcurrency" default:"0" validate:"gte=0,lte=256"`
```

In `Validate()`, inside the existing `if cfg.EnableTwoPhaseHandoff {` block, add:

```go
if cfg.Handoff.PhaseConcurrency < 0 {
	return errors.New("Handoff.PhaseConcurrency must be >= 0")
}
if cfg.Handoff.PhaseConcurrency > 256 {
	return errors.New("Handoff.PhaseConcurrency must be <= 256")
}
```

- [ ] **Step 4: Verify the test passes**

Run: `CGO_ENABLED=1 go test . -race -run TestHandoffConfig_PhaseConcurrency_Validation -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go
git commit -m "feat(config): add Handoff.PhaseConcurrency knob"
```

### Task 2.2: Thread `PhaseConcurrency` into the handoff coordinator's `Config` and default it in `New`

**Files:**
- Modify: `internal/assignment/handoff/coordinator.go` (add field to coordinator's `Config`; normalize in `New`)
- Modify: `manager_setup.go` — find `setupHandoffCoordinator` (or equivalent) and pass `cfg.Handoff.PhaseConcurrency`.

**Design correction (responding to plan-review v2 P2):** the handoff package's `New(cfg Config, enableTwoPhase bool)` constructor at `internal/assignment/handoff/coordinator.go:107-139` already normalizes zero-valued config fields in place (e.g. `MaxRetries = 3`, `BaseBackoff = 50ms`, `SweepInterval = 30s`). PR-2 follows that pattern: default `PhaseConcurrency` in `New`, then read the field directly at the three `SetLimit` call sites. No accessor method.

- [ ] **Step 1: Locate the construction site**

Run: `grep -rn "handoff.Config{\|handoff.NewTwoPhase\|handoff.New\|setupHandoffCoordinator" /home/arlo/projects/parti --include='*.go'`

Identify the production site (likely `manager_setup.go`) where the handoff `Config` is built and passed to `handoff.New`.

- [ ] **Step 2: Add `PhaseConcurrency int` to the coordinator-side `Config`**

In `internal/assignment/handoff/coordinator.go`, inside the `Config` struct (the package-internal one used by `twoPhaseCoordinator`, not the parti-side `HandoffConfig`):

```go
// PhaseConcurrency caps in-flight per-partition KV ops per phase. Zero or
// negative is the "use default 20" sentinel set by the parti-side
// HandoffConfig contract; New() normalizes the value in place.
PhaseConcurrency int
```

In the `New(cfg Config, enableTwoPhase bool) Coordinator` function (at `:107`), add the defaulting alongside the existing field normalizations:

```go
// Apply defaults for retry/backoff
if cfg.MaxRetries <= 0 {
	cfg.MaxRetries = 3
}
// ...existing defaults...
if cfg.PhaseConcurrency <= 0 {
	cfg.PhaseConcurrency = 20
}
```

After this normalization, the three `SetLimit` sites read `t.cfg.PhaseConcurrency` directly — no accessor.

- [ ] **Step 3: Pass `PhaseConcurrency` from the construction site**

In whichever file builds `handoff.Config{...}` (most likely `manager_setup.go`), add the field:

```go
handoff.Config{
	// ...existing fields
	PhaseConcurrency: cfg.Handoff.PhaseConcurrency,
}
```

Test-only construction sites can omit the field; the sentinel 0 already means default-20.

- [ ] **Step 4: Build and run existing handoff tests**

Run: `CGO_ENABLED=1 go test ./internal/assignment/handoff/... -race -v`
Expected: PASS — no behavior change yet.

- [ ] **Step 5: Commit**

```bash
git add internal/assignment/handoff/coordinator.go manager_setup.go
git commit -m "refactor(handoff): plumb PhaseConcurrency through coordinator Config"
```

### Task 2.3: Replace the three hard-coded `SetLimit(20)` with `t.cfg.PhaseConcurrency`

**Files:**
- Modify: `internal/assignment/handoff/twophase.go:236`, `:344`, `:398`.
- Create: `internal/assignment/handoff/twophase_concurrency_test.go`.

- [ ] **Step 1: Build the observing claim store on top of the existing `memStore`**

`memStore` is the in-memory `ClaimStore` already defined at `internal/assignment/handoff/claim_test.go:14-67`. Its actual methods are `Get`, `PutIfEpoch`, `ListKeys` — not `Update`. The observing fake wraps `memStore` and instruments `PutIfEpoch`, which is the method the two-phase coordinator drives.

Add to `internal/assignment/handoff/twophase_concurrency_test.go`:

```go
package handoff

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// observingClaimStore wraps the package-local memStore (see
// claim_test.go:14-67) and instruments PutIfEpoch so tests can observe
// in-flight concurrency. Implements ClaimStore (Get/PutIfEpoch/ListKeys).
type observingClaimStore struct {
	inner    *memStore
	inFlight atomic.Int32
	peak     atomic.Int32
	holdFor  time.Duration
}

func newObservingClaimStore(hold time.Duration) *observingClaimStore {
	return &observingClaimStore{inner: newMemStore(), holdFor: hold}
}

func (o *observingClaimStore) Get(ctx context.Context, partitionID string) (Claim, uint64, error) {
	return o.inner.Get(ctx, partitionID)
}

func (o *observingClaimStore) PutIfEpoch(
	ctx context.Context, partitionID string, expectedEpoch int64, next Claim,
) (uint64, error) {
	cur := o.inFlight.Add(1)
	defer o.inFlight.Add(-1)
	for {
		old := o.peak.Load()
		if cur <= old || o.peak.CompareAndSwap(old, cur) {
			break
		}
	}
	if o.holdFor > 0 {
		time.Sleep(o.holdFor)
	}
	return o.inner.PutIfEpoch(ctx, partitionID, expectedEpoch, next)
}

func (o *observingClaimStore) ListKeys(ctx context.Context) ([]string, error) {
	return o.inner.ListKeys(ctx)
}

// compile-time assertion
var _ ClaimStore = (*observingClaimStore)(nil)
```

- [ ] **Step 2: Write the failing concurrency-respected tests**

Add to the same file:

```go
// TestTwoPhase_PhaseConcurrency_HonorsLimit verifies that setting
// PhaseConcurrency=N causes preparePhase to run at most N in-flight
// updateClaim calls at any instant.
func TestTwoPhase_PhaseConcurrency_HonorsLimit(t *testing.T) {
	const partitions = 50
	const limit = 5

	store := newObservingClaimStore(10 * time.Millisecond)

	// Construct via the public `New(cfg Config, enableTwoPhase bool)`
	// constructor at internal/assignment/handoff/coordinator.go:107 —
	// NOT a `NewTwoPhase` (which does not exist). Passing `true` selects
	// the two-phase coordinator. `New` is responsible for defaulting
	// PhaseConcurrency=20 when zero, so this test path exercises the
	// real defaulting site (see existing usage at
	// internal/assignment/handoff/twophase_test.go:145-194).
	coord := New(Config{
		Store:            store,
		TTL:              1 * time.Minute,
		PhaseConcurrency: limit,
	}, true)

	parts := make([]types.Partition, partitions)
	for i := range parts {
		parts[i] = types.Partition{Keys: []string{fmt.Sprintf("p%d", i)}}
	}

	err := coord.Apply(
		context.Background(),
		"worker-1",
		types.Assignment{},
		types.Assignment{Partitions: parts, Version: 1},
	)
	require.NoError(t, err)
	require.LessOrEqual(t, store.peak.Load(), int32(limit), "peak in-flight exceeded limit")
}

// TestTwoPhase_PhaseConcurrency_DefaultsTo20 proves that zero
// PhaseConcurrency is normalized to 20 by `handoff.New`. This is the
// critical defaulting-path test: if the normalization is bypassed,
// `errgroup.SetLimit(0)` prevents new goroutines from being added
// (errgroup.go:136-150) and the Apply call would hang.
func TestTwoPhase_PhaseConcurrency_DefaultsTo20(t *testing.T) {
	const partitions = 50

	store := newObservingClaimStore(10 * time.Millisecond)

	// PhaseConcurrency omitted — sentinel 0; New must normalize to 20.
	coord := New(Config{
		Store: store,
		TTL:   1 * time.Minute,
	}, true)

	parts := make([]types.Partition, partitions)
	for i := range parts {
		parts[i] = types.Partition{Keys: []string{fmt.Sprintf("p%d", i)}}
	}

	err := coord.Apply(
		context.Background(),
		"worker-1",
		types.Assignment{},
		types.Assignment{Partitions: parts, Version: 1},
	)
	require.NoError(t, err)
	require.LessOrEqual(t, store.peak.Load(), int32(20), "peak in-flight exceeded default 20")
	require.Greater(t, store.peak.Load(), int32(1), "default must be parallel, not serial")
}

// TestTwoPhase_PhaseConcurrency_OneIsSerial proves the operator contract:
// PhaseConcurrency=1 means one in-flight per phase, ever.
func TestTwoPhase_PhaseConcurrency_OneIsSerial(t *testing.T) {
	const partitions = 20

	store := newObservingClaimStore(5 * time.Millisecond)

	coord := New(Config{
		Store:            store,
		TTL:              1 * time.Minute,
		PhaseConcurrency: 1,
	}, true)

	parts := make([]types.Partition, partitions)
	for i := range parts {
		parts[i] = types.Partition{Keys: []string{fmt.Sprintf("p%d", i)}}
	}

	err := coord.Apply(
		context.Background(),
		"worker-1",
		types.Assignment{},
		types.Assignment{Partitions: parts, Version: 1},
	)
	require.NoError(t, err)
	require.Equal(t, int32(1), store.peak.Load(), "PhaseConcurrency=1 must be strictly serial")
}
```

If `New` requires additional `Config` fields (e.g. `Now`, `Logger`, `Metrics`), supply zero-valued or no-op defaults — `New` already fills sensible defaults for `Metrics`/`Now` (`internal/assignment/handoff/coordinator.go:108-114`). Cross-reference existing two-phase tests for the minimal `Config` shape: `internal/assignment/handoff/twophase_test.go:145-194`.

- [ ] **Step 3: Run the tests to verify they fail**

Run: `CGO_ENABLED=1 go test ./internal/assignment/handoff/... -race -run TestTwoPhase_PhaseConcurrency -v`
Expected: FAIL — `Config.PhaseConcurrency` is plumbed but `SetLimit(20)` is still hard-coded.

- [ ] **Step 4: Replace the three SetLimit sites**

In `internal/assignment/handoff/twophase.go`, change each of:

```go
g.SetLimit(20) // Limit concurrent KV operations
```

to:

```go
g.SetLimit(t.cfg.PhaseConcurrency)
```

(Three sites: `:236`, `:344`, `:398`.) The field is read directly because `New` has already defaulted zero values to 20.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/assignment/handoff/... -race -run TestTwoPhase_PhaseConcurrency -v`
Expected: PASS — all three.

- [ ] **Step 6: Commit**

```bash
git add internal/assignment/handoff/twophase.go internal/assignment/handoff/twophase_concurrency_test.go
git commit -m "feat(handoff): honor PhaseConcurrency in prepare/commit/stabilize"
```

### Task 2.4: Pre-PR gate for PR-2

- [ ] **Step 1: `make pre-pr`**

Run: `make pre-pr`
Expected: PASS.

- [ ] **Step 2: Open PR**

```bash
gh pr create --title "feat(handoff): configurable phase concurrency (default 20)" --body "$(cat <<'EOF'
## Summary
- Adds `HandoffConfig.PhaseConcurrency` (default 0 → 20; 1 = serial; 2..256 = exact)
- Replaces 3 hard-coded `g.SetLimit(20)` sites in `internal/assignment/handoff/twophase.go` with `t.cfg.PhaseConcurrency`; the zero-sentinel is normalized to 20 in `handoff.New` alongside the existing field defaults
- Operator contract documented in field Godoc and proven by `TestTwoPhase_PhaseConcurrency_OneIsSerial`

## Test plan
- [x] `make pre-pr`
- [x] PhaseConcurrency=1 → strictly serial (TestTwoPhase_PhaseConcurrency_OneIsSerial)
- [x] PhaseConcurrency=0 → default 20 (TestTwoPhase_PhaseConcurrency_DefaultsTo20)
- [ ] Reviewer: confirm direct mode (`EnableTwoPhaseHandoff=false`) is unaffected
EOF
)"
```

---

## PR-3 — Apply-attempt counter, multi-version-burst diagnostic, opt-in assignment-watcher debounce

Goal: provide operators a measurement-driven path to close the watcher-side gap where a Raft re-election that publishes V=N, V=N+1, ..., V=N+k inside a short burst window produces k+1 immediate apply pipeline entries. PR-3 adds the counter, runs a diagnostic that measures the burst's *duration and apply count*, then ships an off-by-default idle-window debounce on `runAssignmentWatchSession` whose recommended window is informed by the diagnostic.

**Why opt-in default (responding to plan-review v2 P1):** changing default behavior at upgrade time would silently alter apply latency on every running parti deployment, including those that have never observed a thundering herd. The opt-in design lets operators upgrade safely, run the diagnostic, observe what their fleet experiences, then enable the knob with a measured window. A future PR may flip the default after the field has soaked in production deployments — that decision is intentionally deferred. The release notes for PR-3 must include the recommended configuration block and the documented order of operations: upgrade → run diagnostic → set `AssignmentWatcherDebounce` → re-run diagnostic to confirm collapse.

PR-3 ships all three components together: the counter is the diagnostic's measurement instrument; the diagnostic sizes the recommended window; the debounce gives operators the mechanism to close the gap.

### Task 3.1: Add `RecordApplyAttempt(workerID, version)` to `ManagerMetrics`

**Files:**
- Modify: `types/metrics_collector.go` (add to `ManagerMetrics`)
- Modify: `internal/metrics/nop.go` (add no-op)
- Modify: `internal/metrics/prometheus.go` (Prometheus impl)
- Create: `internal/metrics/prometheus_apply_attempts_test.go`

**Design notes (responding to plan-review v1 P1 + v2 P1):**
- Method name and shape mirror `RecordHeartbeat(workerID string, success bool)` (`types/metrics_collector.go:213-218`, `internal/metrics/prometheus.go:617`): worker ID is a method argument, not a field on the collector. `PrometheusCollector` has no `workerID` field — see `internal/metrics/prometheus.go:15-20`, `:84-92`.
- The no-op type is `internal/metrics.NopMetrics` (`internal/metrics/nop.go:5-12`). There is no `types.NoopMetricsCollector`.
- The interface method signature accepts `(workerID string, version int64)` so test/diagnostic collectors retain per-version detail in memory. The Prometheus implementation drops the `version` label — Prometheus labels with unbounded cardinality (assignment version monotonically increases) would create a new time-series per worker per version forever, undercutting the plan's goal of reducing operational load. Existing Prometheus assignment instrumentation records the latest version as a *gauge value*, not a label (`internal/metrics/prometheus.go:626-636`). PR-3 follows that precedent.
- Prometheus metric: `parti_manager_apply_attempts_total{worker_id}` — counter, one label. The `version` argument is observed by the recording collector used in tests/diagnostics; the production Prometheus collector simply increments without per-version cardinality.

- [ ] **Step 1: Add the interface method to `ManagerMetrics`**

In `types/metrics_collector.go`, inside the `ManagerMetrics` interface (around `:20-57`):

```go
// RecordApplyAttempt records each invocation of the manager's apply
// pipeline (applyAssignmentWithPrev) BEFORE the (V, LR) stale gate runs.
// Used to diagnose Raft-re-election bursts: a clean coalescing should
// produce one apply per (worker, version); a leaky watcher path produces
// N for N watcher deliveries.
//
// Parameters:
//   - workerID: Stable worker identifier (caller passes m.WorkerID()).
//   - version: The candidate assignment's Version field.
RecordApplyAttempt(workerID string, version int64)
```

- [ ] **Step 2: Add the no-op impl**

In `internal/metrics/nop.go`, add (under "// ManagerMetrics implementation"):

```go
// RecordApplyAttempt discards the apply-attempt counter.
func (n *NopMetrics) RecordApplyAttempt(_ /* workerID */ string, _ /* version */ int64) {
	// No-op
}
```

- [ ] **Step 3: Add the Prometheus counter field**

In `internal/metrics/prometheus.go`, add a field next to existing manager-section counters:

```go
mApplyAttempts *prometheus.CounterVec
```

Inside `ensureRegistered`'s `p.once.Do(...)` callback, register with **`worker_id` as the only label** (the `version` argument is observed by test/diagnostic collectors but is unbounded for Prometheus):

```go
p.mApplyAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: p.namespace,
	Subsystem: "manager",
	Name:      "apply_attempts_total",
	Help:      "Total invocations of applyAssignmentWithPrev counted before the (V, LR) stale gate. A higher rate after a NATS leader re-election indicates the watcher debounce did not collapse a burst.",
}, []string{"worker_id"})

p.reg.MustRegister(p.mApplyAttempts)
```

(Match the exact registration pattern used by the sibling `RecordHeartbeat` registration — find it via `grep -n "wHeartbeats\|wHeartbeats =" internal/metrics/prometheus.go`.)

Add the implementation method, mirroring `RecordHeartbeat`'s shape (the `version` argument is intentionally unused at the Prometheus collector — it's there for the interface contract and consumed by test/diagnostic collectors):

```go
func (p *PrometheusCollector) RecordApplyAttempt(workerID string, _ int64) {
	p.ensureRegistered()
	p.mApplyAttempts.WithLabelValues(workerID).Inc()
}
```

No `strconv` needed.

- [ ] **Step 4: Write a registration / label test**

Create `internal/metrics/prometheus_apply_attempts_test.go`:

```go
package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestPrometheus_RecordApplyAttempt_BoundedLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	p := NewPrometheus(reg, "parti")

	// Same worker, different versions: must aggregate to a single series
	// (the version argument is discarded by the Prometheus impl to avoid
	// unbounded cardinality).
	p.RecordApplyAttempt("worker-3", 42)
	p.RecordApplyAttempt("worker-3", 43)
	p.RecordApplyAttempt("worker-3", 44)
	p.RecordApplyAttempt("worker-7", 42)

	expected := strings.NewReader(`
# HELP parti_manager_apply_attempts_total Total invocations of applyAssignmentWithPrev counted before the (V, LR) stale gate. A higher rate after a NATS leader re-election indicates the watcher debounce did not collapse a burst.
# TYPE parti_manager_apply_attempts_total counter
parti_manager_apply_attempts_total{worker_id="worker-3"} 3
parti_manager_apply_attempts_total{worker_id="worker-7"} 1
`)
	require.NoError(t, testutil.GatherAndCompare(reg, expected, "parti_manager_apply_attempts_total"))
}
```

- [ ] **Step 5: Run the metrics package tests**

Run: `CGO_ENABLED=1 go test ./internal/metrics/... -race -v`
Expected: PASS. Both the new test and all existing tests.

- [ ] **Step 6: Commit**

```bash
git add types/metrics_collector.go internal/metrics/nop.go internal/metrics/prometheus.go internal/metrics/prometheus_apply_attempts_test.go
git commit -m "feat(metrics): RecordApplyAttempt counter for coalescing diagnosis"
```

### Task 3.2: Wire `RecordApplyAttempt` into `applyAssignmentWithPrev`

**Files:**
- Modify: `manager_assignment.go:906` (`applyAssignmentWithPrev`)
- Create: `manager_apply_attempts_test.go`

**Design correction (responding to plan-review v1 P0 + v4 P1):**
- The metric is recorded **BEFORE** the stale gate, NOT after. The first version of this plan claimed "after" with the rationale that `isApplyResultStale` collapses same-version duplicates; that claim was wrong (`isApplyResultStale` returns `false` for same V, same LR — see Background, `manager_assignment.go:885-892`). Recording before the gate captures every watcher-driven pipeline entry, which is exactly what the diagnostic needs to count.
- Suppression dynamics: under a re-election burst V=10..V=14 delivered to a single worker, this counter increments 5 times (once per delivery, before the gate). With the Task 3.4 debounce in place, the counter increments 1 time (only the collapsed final delivery enters the pipeline). The metric is therefore the direct diagnostic of debounce effectiveness.
- **Placement after PR-1's extract refactor:** PR-1 extracts the stale-gate / Apply / LSR-advance body into `applyAssignmentWithPrevCore`; only the jitter sleep lives in the `applyAssignmentWithPrev` wrapper. The `RecordApplyAttempt` call goes inside **`applyAssignmentWithPrevCore`**, immediately after `m.applyStoreMu.Lock()` and BEFORE `if isApplyResultStale(...) { ... }`. Placing it in core ensures BOTH fresh applies AND retries (via `applyAssignmentWithPrevSkipJitter` → core) are counted. This is intentional: a worker that retries 5 times against the cluster represents 5x prepare/commit/stabilize work; the metric must reflect that cluster load, not just fresh-version load.

- [ ] **Step 1: Write the failing one-call-per-invocation test**

Create `manager_apply_attempts_test.go`:

```go
package parti

import (
	"context"
	"sync"
	"testing"

	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// recordingApplyAttempts embeds NopMetrics and overrides RecordApplyAttempt.
// Embedding picks up no-op impls for the other ~60 MetricsCollector methods
// so tests do not need to enumerate them.
type recordingApplyAttempts struct {
	*metrics.NopMetrics
	mu    sync.Mutex
	calls []applyAttemptCall
}

type applyAttemptCall struct {
	workerID string
	version  int64
}

func newRecordingApplyAttempts() *recordingApplyAttempts {
	return &recordingApplyAttempts{NopMetrics: metrics.NewNop()}
}

func (r *recordingApplyAttempts) RecordApplyAttempt(workerID string, version int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, applyAttemptCall{workerID, version})
}

// compile-time assertion: the embedded NopMetrics satisfies MetricsCollector,
// so the wrapping type does too.
var _ types.MetricsCollector = (*recordingApplyAttempts)(nil)

func TestApplyAssignmentWithPrev_RecordsOneAttemptPerCall(t *testing.T) {
	rm := newRecordingApplyAttempts()
	// Reuse the same hand-rolled *Manager pattern as
	// newTestManagerWithJitter (see manager_apply_jitter_helpers_test.go).
	// Set m.metrics = rm directly on the fixture; do not look for a
	// cfg.MetricsCollector field — metrics are injected via the
	// WithMetrics(...) option (options.go:78) when using the public
	// NewManager constructor.
	m := newTestManagerWithMetrics(t, rm)
	// NOTE: this fixture is not Stop-safe; cancellation runs via t.Cleanup.

	// Replace the real coordinator with a no-op so Apply succeeds without
	// any KV traffic. recordingCoordinator is defined in
	// manager_apply_jitter_helpers_test.go (same package).
	m.handoffCoordinator = &recordingCoordinator{}

	_ = m.applyAssignment(Assignment{Version: 1})
	_ = m.applyAssignment(Assignment{Version: 2})
	_ = m.applyAssignment(Assignment{Version: 3})

	rm.mu.Lock()
	defer rm.mu.Unlock()
	require.Len(t, rm.calls, 3)
	require.Equal(t, int64(1), rm.calls[0].version)
	require.Equal(t, int64(2), rm.calls[1].version)
	require.Equal(t, int64(3), rm.calls[2].version)
	require.Equal(t, m.WorkerID(), rm.calls[0].workerID)
}
```

`newTestManagerWithMetrics` is a tiny helper analogous to `newTestManagerWithJitter` — same hand-rolled fixture, but sets `m.metrics = rm` (the `Manager.metrics` field at `manager.go:62`). The two helpers should share a private `newTestManagerBase()` builder.

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=1 go test . -race -run TestApplyAssignmentWithPrev_RecordsOneAttemptPerCall -v`
Expected: FAIL — `RecordApplyAttempt` is never invoked.

- [ ] **Step 3: Wire the call BEFORE the stale gate, inside the extracted core**

PR-3 ships AFTER PR-1's extract-core refactor. By the time the implementer reaches this step, the apply pipeline is:

```
applyAssignmentWithPrev (jitters, calls core)
       └── applyAssignmentWithPrevCore (locks, stale-gate, Apply, LSR, Store, ack)

applyAssignmentWithPrevSkipJitter (retry entry, calls core directly)
       └── applyAssignmentWithPrevCore
```

The `RecordApplyAttempt` call lives in **`applyAssignmentWithPrevCore`**, immediately after `m.applyStoreMu.Lock()` and BEFORE the existing `if isApplyResultStale(...) { ... }` block:

```go
func (m *Manager) applyAssignmentWithPrevCore(oldAssignment, newAssignment Assignment) error {
	workerID := m.WorkerID()
	m.applyStoreMu.Lock()

	// PR-3 — count pipeline entries before the stale gate. This metric
	// measures every apply attempt (fresh OR retry — both routes call
	// core), so a worker that retries 5x against the cluster increments
	// 5x; the metric reflects cluster prepare/commit/stabilize load.
	// Recording after the stale gate would miss multi-version bursts
	// because isApplyResultStale returns false for distinct versions
	// (manager_assignment.go:885-892).
	m.metrics.RecordApplyAttempt(workerID, newAssignment.Version)

	curAssignment := m.CurrentAssignment()
	if isApplyResultStale(newAssignment, curAssignment) {
		// ...existing stale-drop path
	}
	// ...rest of core
}
```

Placement inside core (rather than inside the fresh wrapper) is the intentional choice: it counts both fresh applies and retries, which is what diagnostic interpretation requires for cluster-load measurement (a worker that retries 5 times against the cluster represents 5x prepare/commit/stabilize work).

**Interpretation caveat** (responding to plan-review v5 P1): because the metric counts retries, a single fresh apply that fails and retries 3 times will show as 4 attempts in the metric. The diagnostic itself does not separate fresh-vs-retry attempts. Operators reading the diagnostic must therefore cross-reference `RecordWorkerConsumerRetryBackoff` (already implemented in `internal/metrics/prometheus.go`) before concluding that a high `max_burst_size` reflects watcher-burst leakage rather than retry pressure. The PR-3 operator flow (Task 3.5 PR body) explicitly calls this out: the post-debounce diagnostic comparison must be made under conditions where retry counts are stable, not under apply-failure pressure.

A future PR could split the metric into separate counters (`apply_attempts_fresh_total`, `apply_attempts_retry_total`) if operators find the combined value too ambiguous in practice. PR-3 deliberately ships the combined counter because it accurately measures cluster load — the most important single signal for the herd problem.

- [ ] **Step 4: Run to verify it passes**

Run: `CGO_ENABLED=1 go test . -race -run TestApplyAssignmentWithPrev_RecordsOneAttemptPerCall -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add manager_assignment.go manager_apply_attempts_test.go
git commit -m "feat(manager): record RecordApplyAttempt for coalescing diagnosis"
```

### Task 3.3: Multi-version-burst diagnostic + window-sizing measurement

**Files:**
- Create: `test/integration/manager/apply_coalescing_test.go`.

**Purpose (responding to plan-review v1 P0):**
The diagnostic measures two things across a Raft-re-election burst:
1. **Total apply attempts per worker per burst window** (the herd magnitude). This is the per-version-agnostic count.
2. **Inter-arrival times of those attempts** (sized in milliseconds). This is the input to Task 3.4's default debounce window.

A "burst" here is defined operationally: all `RecordApplyAttempt` invocations for a single worker that occur within `idleGap` of each other, where `idleGap` starts at 50 ms (well below the smallest plausible debounce). The diagnostic outputs the maximum burst size, the 95th-percentile inter-arrival gap inside a burst, and the maximum burst duration. The implementer writes these numbers into the PR description; the recommended `Config.AssignmentWatcherDebounce` value (operator-facing recommendation, not code-side default) is derived from `max burst duration + 50 ms safety margin`. The code-side default remains `0` (opt-in).

The test is opt-in: it requires real-cluster manipulation and is too noisy for default CI.

- [ ] **Step 1: Write the diagnostic scenario**

Create `test/integration/manager/apply_coalescing_test.go`:

```go
package manager_integration

import (
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
	// ... project integration-test imports (cluster harness, partitest, etc.)
)

// recordingBurstCollector embeds NopMetrics and timestamps every
// RecordApplyAttempt for the diagnostic.
type recordingBurstCollector struct {
	*metrics.NopMetrics
	mu    sync.Mutex
	calls []burstSample
}

type burstSample struct {
	at       time.Time
	workerID string
	version  int64
}

func (r *recordingBurstCollector) RecordApplyAttempt(workerID string, version int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, burstSample{time.Now(), workerID, version})
}

var _ types.MetricsCollector = (*recordingBurstCollector)(nil)

// TestApplyCoalescing_UnderReElectionBurst boots a 3-node embedded NATS
// cluster, starts N parti managers (default N=20), waits for steady
// state, induces a JetStream leader re-election by killing the current
// leader node, then collects RecordApplyAttempt samples for 10 seconds.
//
// It reports:
//   - max apply attempts per worker per burst window (HERD MAGNITUDE)
//   - 95th-percentile inter-arrival gap inside a burst (DEBOUNCE WINDOW INPUT)
//   - max burst duration                                (DEBOUNCE WINDOW INPUT)
//
// Opt-in: requires PARTI_RUN_HERD_DIAGNOSTIC=1.
func TestApplyCoalescing_UnderReElectionBurst(t *testing.T) {
	if os.Getenv("PARTI_RUN_HERD_DIAGNOSTIC") != "1" {
		t.Skip("set PARTI_RUN_HERD_DIAGNOSTIC=1 to run")
	}

	const (
		numWorkers = 20
		idleGap    = 50 * time.Millisecond
		soakAfter  = 10 * time.Second
	)

	collectors := make([]*recordingBurstCollector, numWorkers)
	for i := range collectors {
		collectors[i] = &recordingBurstCollector{NopMetrics: metrics.NewNop()}
	}

	// 1) Boot 3-node embedded NATS cluster. Use existing helper —
	//    search `grep -rn "Start3NodeCluster\|StartCluster\|NewEmbeddedCluster" test/integration/manager`
	//    and reuse whatever the bucket-loss integration test boots.
	//
	// 2) Start numWorkers managers, each with its collector wired in.
	//
	// 3) Wait until every manager reports StateStable
	//    (use existing `WaitState(StateStable, timeout)` helper).
	//
	// 4) Identify the JetStream meta-leader and forcibly stop that NATS
	//    node. The cluster will hold an election; assignment-bucket
	//    leadership flips; the new leader re-publishes the active
	//    assignment, which the workers' assignment-watchers see.
	//
	// 5) Sleep soakAfter to capture the full burst tail.
	//
	// 6) Compute per-worker statistics: group consecutive samples into
	//    bursts whenever the inter-arrival gap is <= idleGap; record
	//    burst size, burst duration, and inter-arrival gaps.
	//
	// 7) Log a results table — t.Logf only, not t.Fatal — so the
	//    diagnostic can be re-run and trend over time.

	// Implementer: fill steps (1)-(5) using existing integration-test
	// helpers; steps (6)-(7) are pure computation, code provided:

	results := analyzeBursts(collectors, idleGap)
	for workerID, r := range results {
		t.Logf(
			"worker=%s max_burst_size=%d max_burst_duration=%s p95_inter_arrival=%s total_attempts=%d",
			workerID, r.MaxBurstSize, r.MaxBurstDuration, r.P95InterArrival, r.TotalAttempts,
		)
	}

	// 8) Print the aggregate recommendation in a single banner line so
	//    the PR-3 author can paste it into the PR description and into
	//    Task 3.4's default-derivation comment.
	t.Logf(
		"AGGREGATE max_burst_size=%d max_burst_duration=%s recommended_debounce_window=%s",
		aggregateMaxBurstSize(results),
		aggregateMaxBurstDuration(results),
		recommendedWindow(results),
	)

	t.Fatal("implementer: replace this Fatal with the cluster wiring " +
		"described in steps (1)-(5) above")
}

type burstReport struct {
	MaxBurstSize     int
	MaxBurstDuration time.Duration
	P95InterArrival  time.Duration
	TotalAttempts    int
}

func analyzeBursts(collectors []*recordingBurstCollector, idleGap time.Duration) map[string]burstReport {
	out := make(map[string]burstReport)
	for _, c := range collectors {
		c.mu.Lock()
		samples := append([]burstSample(nil), c.calls...)
		c.mu.Unlock()

		sort.Slice(samples, func(i, j int) bool { return samples[i].at.Before(samples[j].at) })

		var (
			currentBurst       []burstSample
			gaps               []time.Duration
			maxSize            int
			maxDur             time.Duration
			workerID           string
		)
		flush := func() {
			if len(currentBurst) > maxSize {
				maxSize = len(currentBurst)
			}
			if len(currentBurst) >= 2 {
				dur := currentBurst[len(currentBurst)-1].at.Sub(currentBurst[0].at)
				if dur > maxDur {
					maxDur = dur
				}
			}
			currentBurst = nil
		}
		for i, s := range samples {
			workerID = s.workerID
			if i > 0 {
				gap := s.at.Sub(samples[i-1].at)
				if gap <= idleGap {
					gaps = append(gaps, gap)
					currentBurst = append(currentBurst, s)
					continue
				}
				flush()
			}
			currentBurst = []burstSample{s}
		}
		flush()

		out[workerID] = burstReport{
			MaxBurstSize:     maxSize,
			MaxBurstDuration: maxDur,
			P95InterArrival:  percentile(gaps, 0.95),
			TotalAttempts:    len(samples),
		}
	}
	return out
}

func percentile(xs []time.Duration, p float64) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	idx := int(float64(len(xs)-1) * p)
	return xs[idx]
}

func aggregateMaxBurstSize(rs map[string]burstReport) int {
	m := 0
	for _, r := range rs {
		if r.MaxBurstSize > m {
			m = r.MaxBurstSize
		}
	}
	return m
}

func aggregateMaxBurstDuration(rs map[string]burstReport) time.Duration {
	var m time.Duration
	for _, r := range rs {
		if r.MaxBurstDuration > m {
			m = r.MaxBurstDuration
		}
	}
	return m
}

// recommendedWindow rounds the aggregate max burst duration up to the
// nearest 50 ms and adds a 50 ms safety margin. Caps at 1 second to keep
// reassignment-latency overhead bounded.
func recommendedWindow(rs map[string]burstReport) time.Duration {
	d := aggregateMaxBurstDuration(rs)
	step := 50 * time.Millisecond
	rounded := ((d + step - 1) / step) * step
	w := rounded + 50*time.Millisecond
	if w < 50*time.Millisecond {
		w = 50 * time.Millisecond
	}
	if w > 1*time.Second {
		w = 1 * time.Second
	}
	return w
}
```

- [ ] **Step 2: Wire the cluster setup**

The cluster steps (1)-(5) in the test body are intentionally narrative — they reuse helpers from existing integration tests (almost certainly `TestManager_LiveNATSBucketLoss`'s setup in `test/integration/manager/`). The implementer:

Run: `grep -rn "Start.*Cluster\|EmbeddedCluster\|NewCluster" test/integration/manager/ --include='*.go' | head -20`

Identify the cluster harness API, then fill in the test body. The final `t.Fatal("implementer: replace this Fatal ...")` is the deliberate sentinel: until that line is removed and the body is wired, the test fails loudly under `PARTI_RUN_HERD_DIAGNOSTIC=1`. Reviewer must verify the sentinel is removed before merge.

- [ ] **Step 3: Run the diagnostic 3 times and record results**

Run: `CGO_ENABLED=1 PARTI_RUN_HERD_DIAGNOSTIC=1 go test ./test/integration/manager/ -race -run TestApplyCoalescing_UnderReElectionBurst -v -count=3`

Record (for the PR description):
- `AGGREGATE max_burst_size = <N>`
- `AGGREGATE max_burst_duration = <Xms>`
- `recommended_debounce_window = <Yms>`

The numerical result `<Y>` is documented in the PR description as the operator-facing recommendation for `Config.AssignmentWatcherDebounce`. The code-side default in Task 3.4 remains `0` (opt-in).

- [ ] **Step 4: Commit the diagnostic**

```bash
git add test/integration/manager/apply_coalescing_test.go
git commit -m "test(integration): apply-coalescing diagnostic with burst sizing"
```

### Task 3.4: Add `Config.AssignmentWatcherDebounce` + idle-window debounce in `runAssignmentWatchSession`

**Files:**
- Modify: `config.go` (add field + validation)
- Modify: `manager_assignment.go:449-501` (`runAssignmentWatchSession`)
- Create: `manager_assignment_debounce_test.go`

**Design correction (responding to plan-review v1 P1 + v2 P1):**
- The debounce ships with `default:"0"` (disabled). Operators opt in after running the diagnostic. See the opt-in rationale at the top of PR-3.
- The debounce is an **idle-window reset-on-each-entry timer**, NOT the heartbeat-watcher's first-event-only pattern. They are different shapes — the heartbeat watcher's timer fires 100 ms after the first delivery that flipped `pendingCheck` to true; subsequent deliveries inside that 100 ms do NOT reset the timer. PR-3 needs idle-window semantics: each new delivery resets the timer, and the timer only fires when the stream has been idle for the full window. This is the right shape because the goal is to wait for the burst to *end*, not to fire at fixed-time after burst start.
- Recommended window comes from Task 3.3's `recommended_debounce_window` output, documented in the release notes. The default remains `0` regardless — the recommendation is operator-facing, not code-side.
- The `reconcileTickC` arm is preserved untouched. The debounce only collapses watcher deliveries, not the periodic reconciler. The reconcile arm re-reading the current key while a debounced entry is pending for the SAME key is safe: `handleAssignmentEntry`'s `oldAssignment.Version >= newAssignment.Version` gate at `manager_assignment.go:524-525` drops the duplicate after the first one runs.
- **Flushing behavior — corrected for round-2 P1:** pending entries flush ONLY on watcher channel close (a session-restart situation where the latest pending entry must be preserved). They do NOT flush on `ctx.Done()` — `Stop` cancels `m.ctx` as its first shutdown action (`manager.go:681`); applying a pending entry post-Stop would call `handleAssignmentEntry → applyAssignmentWithPrev → handoffCoordinator.Apply(m.ctx, ...)` with an already-cancelled context, and a failure there would call `scheduleApplyRetry` which spawns a wait-group goroutine while `Stop` is waiting for goroutines to drain. Dropping pending entries on Stop is correct: the manager is intentionally leaving service. A focused test pins this behavior (`TestAssignmentWatcher_DebounceCancelDoesNotFlush`).

- [ ] **Step 1: Add `Config.AssignmentWatcherDebounce` with validation**

In `config.go`, after the `ApplyStartJitter` field from Task 1.1:

```go
// AssignmentWatcherDebounce is the idle-window duration used to coalesce
// rapid bursts of assignment-watcher events into a single apply. When > 0,
// runAssignmentWatchSession holds the latest received entry in a pending
// slot and (re)starts a timer of this duration on each delivery; the
// timer only fires when the watcher stream has been idle for the full
// window, at which point handleAssignmentEntry processes the pending
// entry exactly once.
//
// A Raft re-election can publish V=N, V=N+1, ..., V=N+k inside a short
// window; without debouncing, all k+1 versions enter the apply pipeline
// and each invokes a full prepare/commit/stabilize handoff cycle. With
// debouncing, only the final (highest) version is applied.
//
// Default 0 disables debouncing (preserves pre-PR-3 behavior). Recommended
// value derived from the apply-coalescing diagnostic
// (PARTI_RUN_HERD_DIAGNOSTIC=1) plus a 50 ms safety margin — typically
// in the 100–300 ms range.
//
// Hard-capped at 1 second by Validate(): a larger window would dwarf
// reasonable reassignment-latency budgets and risk masking real apply
// failures behind what looks like apply slowness.
AssignmentWatcherDebounce time.Duration `yaml:"assignmentWatcherDebounce" default:"0" validate:"gte=0"`
```

In `Validate()`, after the `ApplyStartJitter` validation block:

```go
if cfg.AssignmentWatcherDebounce < 0 {
	return errors.New("AssignmentWatcherDebounce must be >= 0")
}
if cfg.AssignmentWatcherDebounce > 1*time.Second {
	return errors.New("AssignmentWatcherDebounce must be <= 1s")
}
```

- [ ] **Step 2: Write the failing burst-collapse + close-while-pending tests**

Create `manager_assignment_debounce_test.go`:

```go
package parti

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// fakeKeyWatcher is a minimal in-process jetstream.KeyWatcher for the
// debounce tests. Find any existing watcher fake in the repo first:
//   grep -rn "implements jetstream.KeyWatcher\|jetstream.KeyWatcher =" .
// If a richer fake exists, prefer it. Otherwise this minimal one suffices.
type fakeKeyWatcher struct {
	ch     chan jetstream.KeyValueEntry
	closed atomic.Bool
}

func newFakeKeyWatcher() *fakeKeyWatcher {
	return &fakeKeyWatcher{ch: make(chan jetstream.KeyValueEntry, 32)}
}

func (f *fakeKeyWatcher) Updates() <-chan jetstream.KeyValueEntry { return f.ch }
func (f *fakeKeyWatcher) Stop() error                              { return nil }

// (Implementer: confirm jetstream.KeyWatcher's full method set — there
// may be a Context() or other accessors to satisfy.)

// TestAssignmentWatcher_DebouncesMultiVersionBurst delivers V=10..V=14
// inside 50 ms with a 100 ms debounce window and asserts
// handleAssignmentEntry runs exactly once, with V=14.
func TestAssignmentWatcher_DebouncesMultiVersionBurst(t *testing.T) {
	const window = 100 * time.Millisecond
	cfg := TestConfig()
	cfg.AssignmentWatcherDebounce = window
	m := newTestManagerForDebounce(t, cfg)
	// NOTE: this fixture is not Stop-safe; cancellation runs via t.Cleanup.

	var processed atomic.Int64
	var lastVersion atomic.Int64
	m.testHookHandleAssignment = func(workerID string, e jetstream.KeyValueEntry) {
		processed.Add(1)
		lastVersion.Store(decodeVersion(e)) // helper that pulls Version from value bytes
	}

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runAssignmentWatchSession(m.ctx, nil /* kv */, watcher, nil /* no reconcile */, "worker-1", "key")
	}()

	for v := int64(10); v <= 14; v++ {
		watcher.ch <- fakeEntryWithVersion(v) // helper: builds a jetstream.KeyValueEntry
		time.Sleep(8 * time.Millisecond)
	}

	// Burst delivered. Wait > debounce + scheduling slack.
	time.Sleep(window + 100*time.Millisecond)
	require.Equal(t, int64(1), processed.Load(), "debounce must collapse burst")
	require.Equal(t, int64(14), lastVersion.Load(), "must process the latest version")
}

// TestAssignmentWatcher_DebounceResetsOnEachEntry verifies the idle-window
// semantics: a steady drip of entries spaced just below the window must
// keep the timer reset and NOT fire until the stream goes idle.
func TestAssignmentWatcher_DebounceResetsOnEachEntry(t *testing.T) {
	const window = 100 * time.Millisecond
	cfg := TestConfig()
	cfg.AssignmentWatcherDebounce = window
	m := newTestManagerForDebounce(t, cfg)
	// NOTE: this fixture is not Stop-safe; cancellation runs via t.Cleanup.

	var processed atomic.Int64
	m.testHookHandleAssignment = func(workerID string, e jetstream.KeyValueEntry) {
		processed.Add(1)
	}

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runAssignmentWatchSession(m.ctx, nil, watcher, nil, "worker-1", "key")
	}()

	// Drip 10 entries spaced 50 ms apart (well below the 100 ms window).
	// Timer must reset each time; no fire during the drip.
	deadline := time.Now().Add(500 * time.Millisecond)
	v := int64(1)
	for time.Now().Before(deadline) {
		watcher.ch <- fakeEntryWithVersion(v)
		v++
		time.Sleep(50 * time.Millisecond)
	}

	require.Zero(t, processed.Load(), "debounce must NOT fire while stream is busy")

	// Now go idle. Wait > window.
	time.Sleep(window + 100*time.Millisecond)
	require.Equal(t, int64(1), processed.Load(), "debounce must fire exactly once after idle")
}

// TestAssignmentWatcher_DebounceCancelDoesNotFlush verifies that when
// m.ctx is cancelled (e.g. via Stop) while a debounced entry is pending,
// the entry is dropped and no apply runs — Stop must not race
// background apply work into the wait group.
func TestAssignmentWatcher_DebounceCancelDoesNotFlush(t *testing.T) {
	const window = 5 * time.Second
	cfg := TestConfig()
	cfg.AssignmentWatcherDebounce = window
	m := newTestManagerForDebounce(t, cfg)

	var processed atomic.Int64
	m.testHookHandleAssignment = func(workerID string, e jetstream.KeyValueEntry) {
		processed.Add(1)
	}

	watcher := newFakeKeyWatcher()
	sessionDone := make(chan struct{})
	go func() {
		_ = m.runAssignmentWatchSession(m.ctx, nil, watcher, nil, "worker-1", "key")
		close(sessionDone)
	}()

	// Deliver an entry — debounce timer starts (5s window).
	watcher.ch <- fakeEntryWithVersion(99)
	time.Sleep(50 * time.Millisecond) // let the debounce arm receive it

	// Now cancel the manager context, simulating Stop's first action.
	// (Fixture is not Stop-safe — cancel directly.)
	m.cancel()

	// Wait for session to exit.
	select {
	case <-sessionDone:
	case <-time.After(2 * time.Second):
		t.Fatal("session did not exit after ctx cancel")
	}

	// Hook MUST NOT have been called.
	require.Zero(t, processed.Load(), "pending entry must be dropped on ctx cancel, not applied during Stop")
}

// TestAssignmentWatcher_PendingEntryFlushesOnClose verifies that a watcher
// channel close while an entry is pending still processes that pending
// entry exactly once.
func TestAssignmentWatcher_PendingEntryFlushesOnClose(t *testing.T) {
	const window = 100 * time.Millisecond
	cfg := TestConfig()
	cfg.AssignmentWatcherDebounce = window
	m := newTestManagerForDebounce(t, cfg)
	// NOTE: this fixture is not Stop-safe; cancellation runs via t.Cleanup.

	var processed atomic.Int64
	var lastVersion atomic.Int64
	m.testHookHandleAssignment = func(workerID string, e jetstream.KeyValueEntry) {
		processed.Add(1)
		lastVersion.Store(decodeVersion(e))
	}

	watcher := newFakeKeyWatcher()
	done := make(chan struct{})
	go func() {
		_ = m.runAssignmentWatchSession(m.ctx, nil, watcher, nil, "worker-1", "key")
		close(done)
	}()

	// Deliver a single entry and immediately close the channel before
	// the debounce window can fire.
	watcher.ch <- fakeEntryWithVersion(42)
	close(watcher.ch)

	<-done
	require.Equal(t, int64(1), processed.Load(), "pending entry must flush on channel close")
	require.Equal(t, int64(42), lastVersion.Load())
}
```

The test references three helpers — `newTestManagerForDebounce`, `decodeVersion`, `fakeEntryWithVersion` — and a production-side `testHookHandleAssignment` hook. The implementer creates them as follows:

1. `newTestManagerForDebounce`: constructs a `*Manager` with the given `Config` and a no-op `handoffCoordinator`. Same shape as `newTestManagerWithJitter` in `manager_apply_jitter_helpers_test.go`.
2. `decodeVersion(entry jetstream.KeyValueEntry) int64`: calls `m.decodeAssignmentEntry(entry)` and returns the `Version`. Move to helpers file if shared.
3. `fakeEntryWithVersion(v int64) jetstream.KeyValueEntry`: returns a hand-rolled `jetstream.KeyValueEntry` implementation whose `Value()` is the JSON encoding of `Assignment{Version: v}`. Find any existing fake entry in tests first: `grep -rn "fakeEntry\|implements jetstream.KeyValueEntry" .`.
4. `Manager.testHookHandleAssignment func(workerID string, entry jetstream.KeyValueEntry)`: an unexported nil-default field on `*Manager`, mirroring the existing `testHookAfterApplyStore` precedent at `manager.go:189-199`. Add a Godoc comment in the same shape:

```go
// testHookHandleAssignment, when non-nil, is invoked instead of
// handleAssignmentEntry from runAssignmentWatchSession's debounce-fire
// and channel-close-flush branches. Set ONLY by tests in this package
// to assert idle-window debounce semantics without booting NATS.
// Production code MUST NOT set this field; it is nil-default. See
// TestAssignmentWatcher_DebouncesMultiVersionBurst.
//
// Concurrency contract: tests must set this field BEFORE spawning the
// runAssignmentWatchSession goroutine and MUST NOT mutate it
// afterwards. The session reads the field from a single goroutine.
testHookHandleAssignment func(workerID string, entry jetstream.KeyValueEntry)
```

Do NOT gate the field behind `//go:build test` — Go does not automatically set a `test` build tag for `go test` runs, and the production-side select reads the field regardless. The nil-default contract is sufficient. The session reads the field non-atomically; per the contract above tests set it once before the goroutine starts and never mutate it after, so no race.

- [ ] **Step 3: Run to verify the four tests fail**

Run: `CGO_ENABLED=1 go test . -race -run TestAssignmentWatcher_ -v`
Expected: FAIL — debounce not wired.

- [ ] **Step 4: Implement the idle-window debounce in `runAssignmentWatchSession`**

In `manager_assignment.go`, rewrite the body of `runAssignmentWatchSession` (currently `manager_assignment.go:449-501`) to:

```go
func (m *Manager) runAssignmentWatchSession(
	ctx context.Context,
	kv jetstream.KeyValue,
	watcher jetstream.KeyWatcher,
	reconcileTickC <-chan time.Time,
	workerID, key string,
) error {
	defer func() {
		if serr := watcher.Stop(); serr != nil && !natsutil.IsBenignWatcherStopErr(serr) {
			m.logError("failed to stop assignment watcher", "error", serr)
		}
	}()

	window := m.cfg.AssignmentWatcherDebounce
	debounce := window > 0

	var (
		pending jetstream.KeyValueEntry
		timer   *time.Timer
		timerC  <-chan time.Time
	)
	if debounce {
		timer = time.NewTimer(time.Hour) // any duration; we Stop before use
		timer.Stop()
		timerC = timer.C
	}
	// flush dispatches the pending entry through the hook or production
	// handler. Called only from the timer-fire arm and the channel-close
	// arm; NEVER from the ctx-cancel arm.
	flush := func() {
		if pending == nil {
			return
		}
		entry := pending
		pending = nil
		if hook := m.testHookHandleAssignment; hook != nil {
			hook(workerID, entry)
		} else {
			m.handleAssignmentEntry(workerID, entry)
		}
	}

	for {
		select {
		case <-ctx.Done():
			// Intentionally do NOT flush. Stop has cancelled m.ctx; running
			// apply work now would invoke handoffCoordinator.Apply with an
			// already-dead context, then scheduleApplyRetry would spawn a
			// wait-group goroutine while Stop is draining the wait group.
			// Dropping the pending entry is the correct shutdown behavior.
			m.logger.Debug("assignment monitor stopping (context cancelled)", "worker_id", workerID)
			return nil

		case entry, ok := <-watcher.Updates():
			if !ok {
				// Session-restart path. Flush the pending entry so a
				// re-election whose final delivery arrived just before
				// the channel closed is not lost — the caller restarts
				// the watcher and the version gate makes a repeat safe.
				//
				// EXCEPT if Stop has already cancelled m.ctx: a
				// connection-side close racing Stop cancellation can
				// surface here, and flushing would apply work during
				// shutdown (the round-2 P1 hazard reapplied to the
				// !ok branch).
				if ctx.Err() != nil {
					return nil
				}
				flush()
				return errors.New("assignment watcher channel closed")
			}
			if entry == nil {
				continue
			}
			if !debounce {
				m.handleAssignmentEntry(workerID, entry)
				continue
			}
			pending = entry
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(window)

		case <-timerC:
			flush()

		case <-reconcileTickC:
			current, gerr := kv.Get(ctx, key)
			if gerr != nil {
				if errors.Is(gerr, jetstream.ErrKeyNotFound) {
					continue
				}
				continue
			}
			m.handleAssignmentEntry(workerID, current)
		}
	}
}
```

Notes:
- `timer.Stop()` + drain-then-Reset is the idiomatic Go pattern for safely resetting a `*time.Timer`. Go 1.23+ relaxed this (a saved channel reference no longer requires the explicit drain), but the explicit pattern is portable and matches the rest of the repo. Verify the project Go version with `head -3 go.mod`; if 1.23+, the pattern is still correct but can be simplified to a bare `timer.Reset(window)` in a follow-up cleanup if desired.
- The `flush()` on channel-close is critical: a re-election whose final delivery lands just before the watcher session closes would otherwise be lost. The caller restarts the watcher; the post-restart reconcile arm or the next watcher event re-presents the latest state, and `handleAssignmentEntry`'s `Version >=` gate (`manager_assignment.go:524-525`) drops the duplicate.
- `flush()` does NOT run on `ctx.Done()`. See the ctx-cancel arm's comment for the rationale.
- Default `AssignmentWatcherDebounce=0` skips all of the above and preserves the pre-PR-3 immediate-process behavior.

Add `Manager.testHookHandleAssignment func(workerID string, entry jetstream.KeyValueEntry)` to the struct (unexported field, zero-valued in production).

- [ ] **Step 5: Run the debounce tests + existing assignment-watcher tests**

Run: `CGO_ENABLED=1 go test . -race -run 'TestAssignmentWatcher|TestRunAssignmentWatch|TestHandleAssignment' -v`
Expected: PASS. With `AssignmentWatcherDebounce=0` (the default used by `TestConfig()`), the new code path is bypassed and existing tests are unaffected. With the new debounce tests' explicit windows, the four new tests pass.

- [ ] **Step 6: Document the recommended operator value**

Read the `recommended_debounce_window=<Y>` value from Task 3.3 step 3. Document `<Y>` in the PR description as the recommended operator setting for `Config.AssignmentWatcherDebounce`. If `<Y>` is meaningfully outside the 100–300 ms heuristic range, also call it out in the field Godoc as a tuning note. The code-side `default:"0"` remains unchanged — operators opt in after upgrade per the PR-3 opt-in flow.

- [ ] **Step 7: Commit**

```bash
git add config.go manager_assignment.go manager_assignment_debounce_test.go config_test.go
git commit -m "feat(manager): debounce assignment watcher to collapse re-election bursts"
```

### Task 3.5: Pre-PR gate for PR-3

- [ ] **Step 1: `make pre-pr`**

Run: `make pre-pr`
Expected: PASS. PR-3 touches `manager_assignment.go`, on the AGENTS.md pre-PR-required list.

- [ ] **Step 2: Run the cross-feature contract tests explicitly**

```bash
CGO_ENABLED=1 go test ./test/integration/manager/ -race -run 'TestManager_LiveNATSBucketLoss|TestManager_LiveNATSBucketLoss_OnDegradedHook|TestStart_' -v
CGO_ENABLED=1 go test ./test/integration/stableid/ -race -run TestStableID_StaleKeyTakeover_Reclaim -v
```

Expected: PASS. With `AssignmentWatcherDebounce=0` in the default `TestConfig()`, the debounce path is inert and the four contracts behave exactly as on `main`. If a contract test happens to set `AssignmentWatcherDebounce>0`, ensure the test's timing budget accommodates the window (most should not).

- [ ] **Step 3: Open PR**

```bash
gh pr create --title "feat(manager): apply-attempts counter + assignment-watcher debounce" --body "$(cat <<'EOF'
## Summary
- Adds `ManagerMetrics.RecordApplyAttempt(workerID, version)` counter; the Prometheus impl labels only `{worker_id}` to keep cardinality bounded (the `version` argument is observed by test/diagnostic collectors). Series: `parti_manager_apply_attempts_total{worker_id}`.
- Adds `Config.AssignmentWatcherDebounce` idle-window timer that collapses Raft-re-election bursts before they enter the apply pipeline.
- **Off-by-default.** Upgrade is a no-op on running fleets; operators opt in after running the diagnostic.
- Diagnostic `TestApplyCoalescing_UnderReElectionBurst` (opt-in via `PARTI_RUN_HERD_DIAGNOSTIC=1`) measures burst magnitude and sizes the recommended window.

## Operator upgrade flow
1. Deploy this version with default config (no behavior change).
2. Run the diagnostic once: `PARTI_RUN_HERD_DIAGNOSTIC=1 go test ./test/integration/manager/ -run TestApplyCoalescing_UnderReElectionBurst -v -count=3`. Record the `AGGREGATE max_burst_size` (call it `<N_before>`) and the `recommended_debounce_window` value (call it `<Y>`).
3. Enable the knob in production config:
   ```yaml
   assignmentWatcherDebounce: 150ms  # example; use <Y> from step 2
   ```
4. Re-run the diagnostic. Compare `AGGREGATE max_burst_size` to `<N_before>`; expect a significant reduction.

**Interpreting the diagnostic:** `parti_manager_apply_attempts_total` counts every apply pipeline entry, including retries. If `max_burst_size` is high but retry metrics (`parti_worker_consumer_retry_backoff_seconds` histogram, `parti_worker_consumer_iterator_restarts_total` counter) are also elevated, the herd metric is reflecting retry pressure, not watcher-burst leakage. Check retry metrics first before raising the debounce window.

## Measurement (from Task 3.3, this PR's source data)
- AGGREGATE max_burst_size = **<N>**
- AGGREGATE max_burst_duration = **<Xms>**
- recommended_debounce_window = **<Yms>**

## Test plan
- [x] `make pre-pr`
- [x] Cross-feature contracts (bucket-loss, peer-takeover, OnDegraded-once, Start-returns-before-Stable) with `AssignmentWatcherDebounce=0` default
- [x] `TestAssignmentWatcher_DebouncesMultiVersionBurst` (V=10..V=14 → 1 apply at V=14)
- [x] `TestAssignmentWatcher_DebounceResetsOnEachEntry` (50 ms drip never fires)
- [x] `TestAssignmentWatcher_DebounceCancelDoesNotFlush` (Stop during pending entry does not apply)
- [x] `TestAssignmentWatcher_PendingEntryFlushesOnClose` (channel close flushes pending)
- [x] `TestPrometheus_RecordApplyAttempt_BoundedLabels` (no per-version label)
EOF
)"
```

---

## Out of scope (intentionally not implemented)

- **Multi-stream sharding.** Parti already supports running multiple `Manager` instances against different streams; this is a deployment-topology choice. If documentation does not currently describe the pattern, add a one-page note under `docs/` in a separate doc-only PR. No library change.
- **NATS-server / PVC / IOPS observability.** These metrics are not parti's to emit. The user should scrape NATS server metrics via `nats-exporter` and PVC IOPS via `kube-state-metrics`. parti already emits everything in its scope.
- **Direct-mode `WorkerConsumerUpdater` fan-out bound.** The contract gives the updater the full partition list in one call; bounding inside parti would require changing the updater contract. Operators using direct mode must implement bounding inside their own `WorkerConsumerUpdater`. Add a doc note.

---

## Self-review

**Spec coverage:**

| Source-analysis fix | This plan |
|---|---|
| Fix 2 — per-worker jitter | PR-1 |
| Fix 3 — bounded create | PR-2 |
| Fix 4 — KV debounce | PR-3 |
| Fix 5 — multi-stream | Out of scope (already supported via multiple `Manager` instances) |
| Fix 6 — observability | Mostly already implemented; PR-3 adds the missing apply-attempts counter |

**Plan-review v1 findings addressed (round 1 → round 2):**

| Finding | Where addressed |
|---|---|
| P0 — PR-3 metric measures wrong thing | Task 3.2 metric placement now BEFORE the stale gate; Task 3.3 diagnostic measures total per-worker per-burst-window (not per-version); Task 3.4 promoted from conditional to unconditional |
| P0 — PR-1 race on `applyEnterT` + PRNG | Task 1.2 uses local `time.Now()` inside test fake; production uses top-level `rand.Int64N`; no `Manager.applyEnterT`, no per-Manager PRNG; added `TestApplyAssignmentWithPrev_JitterNoRaceUnderConcurrentEntrants` |
| P1 — Metrics API doesn't compile | Task 3.1 uses `RecordApplyAttempt(workerID string, version int64)` on `ManagerMetrics`; no-op in `internal/metrics.NopMetrics`; `PrometheusCollector` uses arg-based worker ID (mirrors `RecordHeartbeat`) |
| P1 — Test scaffolds stale | Task 1.2 `recordingCoordinator` now matches actual `handoff.Coordinator` (only `Start(ctx)` no return + `Apply(...) error`); Task 2.3 `observingClaimStore` wraps existing `memStore` and intercepts `PutIfEpoch` (not `Update`) |
| P1 — Debounce window not derived + timer semantics ambiguous | Task 3.3 explicitly computes `recommended_debounce_window`; Task 3.4 specifies idle-window reset-on-each-entry semantics with the full Stop/drain/Reset pattern in code; promotes `Config.AssignmentWatcherDebounce` to a real deliverable |
| P2 — PhaseConcurrency contract | Task 2.1 documents `0 → 20`, `1 → serial`, `2..256 → exact`; Task 2.3 proves serial-mode with `TestTwoPhase_PhaseConcurrency_OneIsSerial` |

**Plan-review v2 findings addressed (round 2 → round 3):**

| Finding | Where addressed |
|---|---|
| P1 — Root-package snippets don't compile (`m.config`, `New(...)`, `cfg.MetricsCollector`, `m.Stop()`) | All snippets now use `m.cfg`, `NewManager(&cfg, ..., WithMetrics(...))` or the hand-rolled fixture pattern at `manager_commit_state_machine_test.go:141-170`, and `m.Stop(context.Background())`. Background section now spells out the Manager field/constructor surface explicitly so future edits cannot drift. |
| P1 — Apply-start jitter consumes startup budget | `ApplyStartJitter` Godoc now documents the startup-runner consequence and recommends `ApplyStartJitter <= StartupTimeout / 4`. New Task 1.2 Step 7 adds `TestApplyStartJitter_StartupBudget` integration test pinning both positive (jitter within budget) and negative (jitter > budget → startup-timeout Degraded) cases. |
| P1 — PR-3 claims debounce closes the default gap but ships disabled | PR-3 goal section rewritten to honestly describe opt-in. Release-notes guidance added: upgrade → diagnostic → enable → confirm. Default remains 0; recommendation comes from operator-side diagnostic, not code-side default. A future PR may flip the default after production soak. |
| P1 — `flush()` on `ctx.Done()` runs apply during Stop | `runAssignmentWatchSession` no longer flushes on `ctx.Done()`. Channel-close flush retained (session-restart path). Pinned by new `TestAssignmentWatcher_DebounceCancelDoesNotFlush`. |
| P1 — Prometheus `version` label = unbounded cardinality | `RecordApplyAttempt(workerID, version int64)` signature unchanged (test/diagnostic collectors retain per-version detail). Prometheus impl drops the `version` argument with `_ int64` and registers only `{worker_id}`. Test renamed to `TestPrometheus_RecordApplyAttempt_BoundedLabels`. |
| P2 — `phaseConcurrency()` accessor is style break | Dropped accessor. `New(cfg, enableTwoPhase)` now normalizes `cfg.PhaseConcurrency = 20` in place alongside the existing `MaxRetries`/`BaseBackoff`/`SweepInterval` normalizations. The three `SetLimit` sites read `t.cfg.PhaseConcurrency` directly. |
| P2 — Test hook Godoc contract | `testHookHandleAssignment` Godoc now mirrors `testHookAfterApplyStore` (`manager.go:189-199`) verbatim: unexported nil-default field, set ONLY by same-package tests before goroutine starts, production MUST NOT set, no `//go:build test` gate. |

**Plan-review v3 findings addressed (round 3 → round 4):**

| Finding | Where addressed |
|---|---|
| P1 — Hand-rolled apply fixtures not Stop-safe | Helper Godoc now explicitly warns the fixture is NOT Stop-safe and uses `t.Cleanup(cancel)`. All apply-path test snippets switched from `defer m.Stop(...)` to direct `m.cancel()` / cleanup. Helper docs list the full set of fields the fixture must initialize for `applyAssignmentWithPrev` (including heartbeat stub, hooks, handoffCoordinator). |
| P1 — `TestApplyStartJitter_StartupBudget` was random | Split into deterministic positive and negative cases. Both inject `m.applyJitterSampler` (new test-only seam) to force exact jitter durations. Positive forces 200ms with 5s budget; negative forces 2s with 200ms budget — neither relies on PRNG sampling. Setup notes call out the cold-empty bypass and require seeding a non-empty initial assignment. |
| P1 — `NewTwoPhase` does not exist | All three PR-2 test snippets switched to `New(Config{...}, true)` (the actual constructor at `internal/assignment/handoff/coordinator.go:107`). `TestTwoPhase_PhaseConcurrency_DefaultsTo20` now specifically asserts `peak > 1` to prove the defaulting path actually feeds `SetLimit` with a positive value (a bypassed normalization would deadlock with `SetLimit(0)`). |
| P1 — Apply-start jitter compounds with retry backoff | `applyAssignmentWithPrev` body extracted to `applyAssignmentWithPrevCore`; fresh-version `applyAssignmentWithPrev` jitters then calls core; new `applyAssignmentWithPrevSkipJitter` calls core directly. `scheduleApplyRetry`'s retry goroutine switched to `applyAssignmentWithPrevSkipJitter`. `ApplyStartJitter` Godoc updated to state retries do NOT jitter. New `TestApplyAssignmentRetry_DoesNotJitter` pins the behavior. |
| P1 — Channel-close flush during Stop race | `runAssignmentWatchSession`'s `!ok` branch now checks `if ctx.Err() != nil { return nil }` before `flush()`. A connection-side close racing Stop cancellation no longer flushes a pending entry into the apply pipeline. |
| P2 — Opt-in stale wording | Task 3.4 Step 6 renamed to "Document the recommended operator value"; PR-3 PR-body boilerplate now says "off-by-default; enable after the diagnostic". |
| P2 — File map / PR-body residual `phaseConcurrency()` and `{worker_id, version}` | File map entries (`internal/assignment/handoff/coordinator.go`, `twophase.go`, `prometheus.go`) updated to match the corrected implementation. PR-2 boilerplate updated to reference `t.cfg.PhaseConcurrency`. PR-3 boilerplate updated to `{worker_id}`. Task 2.3 header retitled to `t.cfg.PhaseConcurrency`. |
| P2 — Debounce test count was "three" after adding cancel case | Both step messages updated to "four tests". |

**Plan-review v4 findings addressed (round 4 → round 5):**

| Finding | Where addressed |
|---|---|
| P1 — Retry-jitter test only covered the helper, not the scheduler route | Added `testHookApplyJittered` (same nil-default contract as `testHookAfterApplyStore`) that the fresh `applyAssignmentWithPrev` wrapper fires before its jitter sleep. `applyAssignmentWithPrevSkipJitter` does not fire it. `TestApplyAssignmentRetry_DoesNotJitter` now drives the actual `scheduleApplyRetry` goroutine (via a failing-then-succeeding coordinator) and asserts the jitter hook fires exactly once (for the fresh attempt) and ZERO times during the retry. |
| P1 — PR-3 metric placement still targeted pre-refactor body | Task 3.2 Step 3 rewritten to specify placement in `applyAssignmentWithPrevCore`, immediately after `m.applyStoreMu.Lock()` and before `isApplyResultStale`. Includes an ASCII diagram of the post-PR-1 call structure. Explicit decision documented: BOTH fresh applies AND retries are counted (placement in core ensures this), because the metric measures cluster prepare/commit/stabilize load — retries do that work too. |
| P2 — PR-3 release notes omitted re-run/confirm + example config | PR-3 PR body now includes a 4-step "Operator upgrade flow" section: deploy → diagnostic → enable knob with example YAML → re-run diagnostic to confirm collapse. |
| P2 — PR-3 test plan body omitted cancel-does-not-flush test | PR-3 PR body test plan now lists all five expected tests, including `TestAssignmentWatcher_DebounceCancelDoesNotFlush` and `TestPrometheus_RecordApplyAttempt_BoundedLabels`. |

**Plan-review v5 findings addressed (round 5 → round 6):**

| Finding | Where addressed |
|---|---|
| P1 — Retry-jitter test had three correctness hazards | Extended `recordingCoordinator` with `failUntilCount atomic.Int64` so the first N applies deterministically fail (no mid-flight `applyErr` mutation, no data race). Test now waits on `rc.applyCount.Load() >= 2` (the real counter, not a local). Asserts `jitterFires == 1` (exactly one fresh attempt jitters; retry does not) instead of comparing to a snapshot. |
| P1 — Diagnostic interpretation inconsistent with implementation | Task 3.2 now explicitly documents that the metric counts retries and adds an "interpretation caveat" pointing operators to `RecordWorkerConsumerRetryBackoff`. PR-3 operator flow updated: replaced "expect ≈ 1" with "compare before/after; expect significant reduction" plus a new paragraph telling operators to check retry metrics before raising the debounce window. |
| P1 — `IntegrationTestConfig()` unqualified | All snippets now use `testutil.IntegrationTestConfig()`. Import block added showing the `internal/testutil` import. Helper-pair note clarifies `parti.TestConfig()` vs `testutil.IntegrationTestConfig()`. |
| P2 — PR-3 heredoc escaped backticks | PR-3 PR body's escaped `` \` `` reverted to unescaped backticks, matching PR-1 and PR-2 (which use the same `<<'EOF'` heredoc style with no backslashes). |

**Type consistency:**
- `Config.ApplyStartJitter` (PR-1, manager-level)
- `HandoffConfig.PhaseConcurrency` (PR-2, handoff-coordinator-level)
- `Config.AssignmentWatcherDebounce` (PR-3, manager-level)
- `RecordApplyAttempt(workerID string, version int64)` on `ManagerMetrics` (PR-3)
All names consistent across all task references.

**Cross-feature contract impact:** PR-1 (jitter sleep at apply entry) and PR-3 Task 3.4 (assignment-watcher debounce) both affect apply timing. All four pinned contracts are explicitly re-run in each PR's pre-PR gate: `TestManager_LiveNATSBucketLoss`, `TestManager_LiveNATSBucketLoss_OnDegradedHook`, `TestStableID_StaleKeyTakeover_Reclaim`, `TestStart_ReturnsBeforeStable`. Defaults (jitter=0, debounce=0) keep all paths inert when operators have not opted in.
