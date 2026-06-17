# Adaptive (fleet-size-aware) rate limiting for consumer-create and claim-write

Status: design (pre-implementation)
Date: 2026-06-17
Target release: v2.8.0 (the same release that first ships the static per-worker
rate limits, so the adaptive knob is designed into the API before it freezes)

## 1. Problem

v2.8.0 introduces two opt-in, default-off, per-worker token-bucket rate limits:

- consumer-create — gates every physical `CreateOrUpdateConsumer` RPC
  (`consumer.WithConsumerCreateRate(perSec, burst)` / `WithConsumerCreateLimiter`).
- claim-write — gates every physical `PutIfEpoch` in the two-phase handoff
  (`parti.HandoffConfig.ClaimWritePerSec` / `ClaimWriteBurst`).

Both gates fire on **every** worker (Parti's apply is decentralized: the leader
calculates and publishes assignments; every worker — leader and followers —
reads its own slice and applies it locally, creating consumers and writing
claims). Each worker enforces its own per-worker rate independently.

Therefore the **cluster-wide** rate is `N × per_worker_rate`, where `N` is the
worker count. `N` is operationally uncontrollable (it can be 1 or 30 depending
on deployment and on transient fleet events), so a per-worker setting of
`100/s` yields anywhere from `100/s` to `3000/s` of aggregate load against the
shared NATS metacontroller. The operator cannot bound the aggregate.

## 2. Goal and control model

Let the operator bound the **aggregate** while keeping a **per-worker ceiling**.
The effective per-worker rate each worker enforces becomes:

```
effective_per_worker_rate = min( perWorkerMax , clusterRate / observed_N )
```

- `perWorkerMax` — the existing per-worker knob (`WithConsumerCreateRate.perSec`,
  `HandoffConfig.ClaimWritePerSec`). Reinterpreted as a **ceiling**.
- `clusterRate` — a **new** aggregate target knob.
- `observed_N` — the cluster worker-count this worker currently observes,
  clamped to `>= 1`.

Properties (the reason this model was chosen over a pure `clusterRate / N`):

- **Steady-state aggregate is `clusterRate`.** This is a *steady-state* (post-
  convergence) guarantee, not an instantaneous one. Once every live worker has
  observed the *same* committed worker-set (they converge because `N` derives
  from the monotonic committed assignment — §4), each runs at
  `clusterRate / N`, so the sustained aggregate sums to `clusterRate`.
- **No single worker exceeds `perWorkerMax`,** regardless of `N` — the only
  *local*, always-true bound. This is what bounds the **transient**.
- **Transient bound (be honest — workers do not retune atomically).** While a
  fresh commit propagates (watcher delivery skew, the 30s reconcile floor,
  watcher gaps, and slow-to-observe workers), workers briefly disagree on `N`.
  The instantaneous configured aggregate is
  `Σ_i min(perWorkerMax, clusterRate / observed_N_i)`. Worst case is during a
  *grow* `a → b`: workers still on the old commit use `clusterRate / a` while
  `b` workers are active, so the aggregate can reach
  `b · min(perWorkerMax, clusterRate / a)` — i.e. up to a `b/a` overshoot factor
  when `perWorkerMax` is the non-binding (high) ceiling. The `perWorkerMax`
  ceiling caps this at `perWorkerMax · N`. Commit monotonicity converges all
  workers back to `clusterRate` after propagation.
- **Burst is per-worker and does not scale with `N`.** The instantaneous
  aggregate *burst* is `Σ_i burst_i` (up to `N · burst`) and is **not** bounded
  by `clusterRate`. Operators who must bound the aggregate burst keep `burst`
  small. (This matches the static-limit semantics; burst is the per-worker
  instantaneous allowance.)
- **In-flight reservations are not retroactively retuned.** A `SetRate` that
  *lowers* the rate does not cancel reservations already granted by `Wait`
  (golang.org/x/time/rate semantics: `SetLimitAt` may let existing reservations
  violate or underutilize the new limit). The effect is a brief, bounded
  transient that self-corrects.
- **Optional future tightening (non-goal for v1):** a conservative-N strategy
  (e.g. tighten immediately on grow, decay slowly on shrink) would shrink the
  grow-transient. Deliberately deferred — the ceiling already bounds it and the
  chosen model accepts the ceiling-bounded transient.

## 3. Public API (frozen in v2.8.0)

The design is **purely additive** to the static knobs already built for v2.8.0.
When the new cluster knob is unset (`0`), behavior is **identical to today**
(fixed per-worker rate); the limiter is still built exactly as it is now.

### consumer-create (consumer package)

- `WithConsumerCreateRate(perSec float64, burst int)` — **exists.** `perSec` is
  now documented as the per-worker ceiling; `burst` is unchanged.
- `WithConsumerCreateClusterRate(clusterPerSec float64)` — **new.** Aggregate
  target. Effective rate = `min(perSec, clusterPerSec / N)`.
- Requires `WithConsumerCreateRate` to also be set (it supplies `burst` and the
  ceiling). Using the cluster option without the per-worker option is a
  construction error from `NewDynamic` (fail-fast; it would otherwise be silently
  inert). To express "aggregate cap with no meaningful per-worker ceiling", set
  `perSec` to a high value.
- `WithConsumerCreateLimiter` (injected custom/shared limiter) — **exists,
  unchanged.** Adaptive retuning is **not** applied to an injected limiter
  (Parti cannot assume a third-party limiter is reconfigurable); the cluster
  option is rejected at `NewDynamic` when an injected limiter is supplied. The
  injected-limiter path remains the way to share one fixed budget across
  consumers.

### claim-write (root package)

- `HandoffConfig.ClaimWritePerSec` / `ClaimWriteBurst` — **exist.** `PerSec` is
  now the per-worker ceiling.
- `HandoffConfig.ClaimWriteClusterRate float64` — **new.** Aggregate target;
  `validate:"gte=0"`. Effective rate = `min(ClaimWritePerSec, ClaimWriteClusterRate / N)`.
- Requires `ClaimWritePerSec > 0` (ceiling + enables the limiter) and
  `EnableTwoPhaseHandoff`. `ClaimWriteClusterRate > 0` with `ClaimWritePerSec == 0`
  is a `Config.Validate` error. `ClaimWriteClusterRate > 0` with
  `EnableTwoPhaseHandoff == false` is an inert-config WARN in
  `ValidateWithWarnings` (mirrors the existing `ClaimWritePerSec`-without-two-phase
  warning at config.go ~817).

## 4. Observing N

### Source: the committed worker-set

`N = len(commit.Workers)` from the committed assignment
(`types.AssignmentCommit.Workers`, the sorted worker-id list; already surfaced as
`Assignment.TotalWorkers = len(commit.Workers)` at
`manager_assignment.go:1109,1146`).

Chosen over the heartbeat active-worker scan (`GetActiveWorkers`,
`internal/assignment/worker_monitor.go`) because:

- **Consistency.** Every worker reads the *same* committed `Workers` list, so all
  workers compute the *same* `N` and the *same* `clusterRate / N`. This is what
  makes the aggregate bound actually hold. A heartbeat scan is eventually
  consistent and per-worker-divergent.
- **Availability.** The heartbeat scan is only consumed by the leader's
  calculator during rebalance; followers have no live view of it.
- It is monotone via commit `Version`, so it converges after a fleet change.

### Hook: the pre-debounce commit-decode point

Critical constraint discovered during grounding: the commit watcher's
**debouncer suppresses *apply* dispatch** when `N` changes but *this worker's*
partition slice is unchanged (`workerAssignmentChanged`, `manager_assignment.go:790`).
So hooking N-observation at apply time would **miss** fleet changes that don't
touch this worker's partitions — the exact common case (a peer joins/leaves; my
slice is stable).

Therefore N-observation is hooked **before** the debouncer, where every commit
the watcher delivers is decoded — but **fenced on commit freshness** so a stale
commit cannot regress N (see the P0 below):

- In `runCommitWatchSession` (`manager_assignment.go:856-874`), both the
  `onUpdate` handler (every watcher event) and the `onReconcile` handler (the
  30s re-fetch floor) decode the commit and call `db.stage(commit)`. Insert
  `m.observeFleetSize(commit)` immediately after a successful decode and
  **before** `db.stage(commit)`.
- This delivers prompt N updates on every *fresh* commit, plus a 30s reconcile
  floor, fully decoupled from the apply debounce/dedup and the two-phase handoff
  diff. Deletes (`decodeCommitEntry` returns `(nil,false)` on a KV delete,
  `manager_assignment.go:893`) and failed reconcile reads do not observe N; the
  next watcher event or reconcile tick covers them (bounded by the reconcile
  floor). N is never zero in practice: the publisher does not publish a commit
  with an empty worker-set (`assignment_publisher.go:332`), and a worker absent
  from `Workers` is a revoke-for-this-worker, not empty-N.

**P0 — fence against stale-N regression.** The pre-debounce decode point sees
*every* decoded commit, including ones the debouncer/apply path would reject as
stale: `stage()` drops a lower-version pending race at `manager_assignment.go:781`,
and the reconcile arm can surface an older snapshot racing a newer in-flight
watcher event (the comment at `:778-780`). An *unconditional* observe would
regress the limiter to an older worker-count. `observeFleetSize` must therefore
replicate the apply path's freshness gate — pre-`workerAssignmentChanged`,
pre-apply, but **post-freshness** — using the **same `(Version, LeaderRevision)`
lex prefix** the apply stale gate / commit dedup use (`commitSupersedesForStash`,
`manager_assignment.go:1291`, additionally compares `BatchDigest`/source at an
equal `(V, LR)`; that tail is irrelevant here because N is fixed for a given
`(V, LR)`, so the lex prefix alone is the correct and sufficient fence).

`observeFleetSize(commit *types.AssignmentCommit)`:

1. `n := len(commit.Workers)`; clamp `n` to `>= 1` (defensive; the publisher
   won't emit empty `Workers`).
2. Under a small `fleetMu sync.Mutex` (commits are infrequent; this is not a hot
   path):
   - **Freshness fence:** if `(commit.Version, commit.LeaderRevision)` does not
     strictly supersede the last observed `(version, leaderRevision)` (same lex
     rule as `commitSupersedesForStash`), return — do **not** regress N.
   - Record the new `(version, leaderRevision)`.
   - If `n == lastObservedN`, return (version advanced but N unchanged → no
     retune).
   - Record `lastObservedN = n` and retune:
     - claim-write (manager-local): if `ClaimWriteClusterRate > 0` and
       `m.claimWriteLimiter` implements `ratelimit.RateSetter`, call
       `SetRate(min(ClaimWritePerSec, ClaimWriteClusterRate / n))`.
     - consumer-create (push): if `m.fleetSizeObserver != nil`, call
       `m.fleetSizeObserver.ObserveWorkerCount(n)`.
   The lock is held across the retune + push so a higher-version observe cannot
   interleave and leave the consumer/limiter at a stale N. Both calls are
   non-blocking and non-reentrant by contract (§7), so holding `fleetMu` across
   them is safe (mirrors the `CapabilityReporter` non-blocking/non-reentrant
   contract).

The cold-start ordering (consumer receives N before its first create storm) is
handled by the bootstrap path below.

### Cold-start / bootstrap ordering

At construction (before any commit) the limiters are built at `perWorkerMax` (the
ceiling) — the safe upper bound. To tighten before the first consumer-create
storm:

- **Commit bootstrap path:** `manager.go` builds the initial Assignment from the
  commit at `:692` and applies it at `:701`. Insert `m.observeFleetSize(commit)`
  **between those two lines** — after the commit is decoded, before
  `applyAssignmentWithPrev`, so the consumer's `ObserveWorkerCount` runs before
  the first create. The freshness fence is a no-op here (first observation).
- **Alias-fallback path:** if the commit read/verify fails, startup falls back to
  applying the legacy alias (`manager.go:759`; `waitForAssignment` pre-stores the
  alias into `m.assignment` at `manager_election.go:473`). The alias carries no
  `commit.Workers`. Handling: if the alias Assignment has `TotalWorkers > 0`,
  observe it (`observeFleetSize` accepting an explicit `(version, leaderRev, n)`
  for this path); otherwise the worker runs **ceiling-bounded** until the first
  commit is observed by the watcher, which then retunes. This residual is safe by
  the `perWorkerMax` ceiling and self-corrects on the first commit.

Because `setupHandoff` (which builds `m.claimWriteLimiter`, `manager_setup.go:209`)
runs **synchronously during `Start`**, before the commit watcher
(`monitorCommitChanges`) is spawned in the background runner
(`manager_startup_async.go:128`), the claim-write limiter always exists before
any `observeFleetSize` call — so the first observation retunes an already-built
limiter. This ordering is **load-bearing**: keep limiter construction ahead of
the first N observation if this startup sequence is ever refactored.

## 5. Live retune mechanism

### 5.1 ratelimit primitive — add `SetRate`

`internal/ratelimit`:

- The `Limiter` interface stays `Wait(ctx) error`-only. **Do not** add `SetRate`
  to the interface: a user-injected `consumer.ConsumerCreateLimiter` (Wait-only)
  is stored structurally as a `ratelimit.Limiter`, and widening the interface
  would break that path.
- Add a method on the concrete type:
  `func (l *TokenBucketLimiter) SetRate(perSec float64)` →
  `l.rl.SetLimit(rate.Limit(perSec))`. `rate.Limiter.SetLimit` takes the
  limiter's internal mutex and is safe to call concurrently with `Wait`
  (golang.org/x/time/rate is documented goroutine-safe).
- Add a narrow optional-capability interface used by callers that hold the
  limiter only as the `Limiter` interface:
  ```go
  // RateSetter is implemented by limiters whose steady-state rate can be
  // retuned at runtime (the built-in TokenBucketLimiter). Adaptive rate
  // limiting type-asserts a Limiter to RateSetter; a limiter that does not
  // implement it keeps its constructed rate.
  type RateSetter interface{ SetRate(perSec float64) }
  ```
- `*TokenBucketLimiter` satisfies `RateSetter`. Optionally add `Limit() float64`
  for tests/metrics (read current rate). `SetRate` keeps the existing burst
  unchanged (burst is the per-worker instantaneous allowance and does not scale
  with N).

Because the one `*TokenBucketLimiter` instance is reference-shared across all
gate sites (claim-write: coordinator + startup hygiene + resume; consumer-create:
`WorkerConsumer.limiter` + every `PartitionConsumer`), a single `SetRate`
retunes every gate at once.

### 5.2 claim-write — manager-local

Fully contained in the root package:

- New config field + validation + inert-warning (§3, §8).
- `buildClaimWriteLimiter` is unchanged in *when* it builds (still iff
  `ClaimWritePerSec > 0`); its constructed rate stays `ClaimWritePerSec` (the
  ceiling) — the safe upper bound until the first N is observed.
- `observeFleetSize` (§4) type-asserts `m.claimWriteLimiter` to `RateSetter` and
  calls `SetRate(min(ClaimWritePerSec, ClaimWriteClusterRate / n))` when N
  changes and `ClaimWriteClusterRate > 0`.

### 5.3 consumer-create — manager pushes N, consumer computes

The consumer-create knobs and the limiter live on the consumer; only the manager
knows N. So the manager **pushes N** and the consumer **owns the policy**:

- New optional interface in the root package, mirroring `CapabilityReporter` but
  in the push direction:
  ```go
  // FleetSizeObserver is an optional interface a WorkerConsumerUpdater MAY
  // implement to receive the observed cluster worker-count (N) for
  // fleet-size-aware (adaptive) rate limiting. The Manager calls
  // ObserveWorkerCount whenever the committed worker-set size changes (and
  // once at startup, before the first apply). Implementations MUST be
  // non-blocking, safe for concurrent use, and MUST NOT call back into the
  // Manager or any apply/update path (mirrors the CapabilityReporter contract
  // and the D5 lock-order rule).
  type FleetSizeObserver interface{ ObserveWorkerCount(n int) }
  ```
- Manager: `m.fleetSizeObserver = asFleetSizeObserver(options.consumerUpdater)`
  at construction (`manager.go:447`, beside `capReporter`). `asFleetSizeObserver`
  mirrors `asCapabilityReporter` (manager.go:981).
- `CompositeConsumerUpdater` forwards `ObserveWorkerCount(n)` to every child that
  implements `FleetSizeObserver` (mirrors its `Capabilities()` forwarding at
  composite_updater.go:157-183) and asserts
  `var _ FleetSizeObserver = (*CompositeConsumerUpdater)(nil)`.
  - **Late-add replay (P1).** `CompositeConsumerUpdater.Add` admits children
    after construction (composite_updater.go:100). A push-only observer would
    leave a late-added adaptive consumer at the ceiling rate until the next N
    change. So the composite **caches the last observed N** and **replays it** to
    any newly-added `FleetSizeObserver` child in `Add` — exactly mirroring the
    existing `SetOnStreamMissingError` store-and-replay at composite_updater.go:137.
    `ObserveWorkerCount` updates the cache (under the composite's existing mutex)
    and forwards; `Add` replays the cached N (when one has been observed) to the
    new child.
- `consumer.Dynamic` implements `ObserveWorkerCount(n int)`:
  - It keeps `consumerCreatePerSec` (ceiling), the new
    `consumerCreateClusterRate`, and a `ratelimit.RateSetter` handle obtained by
    type-asserting the limiter resolved at `NewDynamic` (non-nil only for the
    built-in `WithConsumerCreateRate` path with a cluster rate set).
  - On call: if the `RateSetter` handle is non-nil and `clusterRate > 0`, call
    `SetRate(min(perSec, clusterRate / max(1, n)))`. Otherwise no-op.

## 6. Edge cases and ordering

- **`clusterRate == 0` (default):** no retune path is armed; the limiter keeps
  its constructed `perSec`. Byte-for-byte identical to today. Full backward
  compatibility.
- **N clamp:** `observeFleetSize` clamps `n >= 1` (a revoke-all commit can omit
  this worker; division must never see 0).
- **Cold-start window:** at construction (before any commit) the limiter rate is
  `perSec` (the ceiling). The initial-bootstrap commit fetch observes N and
  pushes it **before** the first apply/create storm, so the storm runs at the
  adapted rate in the normal path. Any residual transient is bounded by the
  ceiling (`perWorkerMax × N`) — the §2 guarantee.
- **N-transition transient:** while a fresh commit propagates, workers briefly
  disagree on N; the per-worker ceiling bounds the aggregate overshoot (§2
  formula), in-flight reservations are not retroactively retuned, and commit
  monotonicity converges all workers to the same N.
- **Stale / out-of-order commit:** the reconcile arm or a watcher race can
  surface a lower-`(Version, LeaderRevision)` commit; the freshness fence in
  `observeFleetSize` drops it, so N never regresses (P0, §4).
- **Deletes / failed reconcile reads:** do not observe N; covered by the next
  watcher event or the 30s reconcile floor.
- **Injected limiter (`WithConsumerCreateLimiter`):** not retunable; the cluster
  option is rejected at `NewDynamic` when an injected limiter is present.
- **Shrink to N=1:** effective rate rises to `min(perSec, clusterRate)` —
  correct (one worker may use the whole budget, still capped at the ceiling).

## 7. Concurrency

- `observeFleetSize` is invoked from the commit-watch goroutine
  (`onUpdate`/`onReconcile`) and the initial-bootstrap path. These can overlap
  (bootstrap apply runs in the background runner while the watch session spawns),
  so the freshness fence + retune + push run under a dedicated `fleetMu`
  serializing them; the fence guarantees only strictly-superseding
  `(Version, LeaderRevision)` commits retune, so a stale observe never regresses
  N even under concurrency. `fleetMu` is never held with `applyStoreMu`/`updateMu`
  and guards no I/O.
- `rate.Limiter.SetLimit`/`SetLimitAt` is internally mutex-guarded; concurrent
  with the `Wait`/`Reserve` calls issued from apply / handoff / partition-consumer
  goroutines it is race-free. Caveat (§2): a rate *decrease* does not cancel
  reservations already granted — a bounded, self-correcting transient, not a race.
- `FleetSizeObserver.ObserveWorkerCount` and the consumer's `RateSetter` call are
  non-blocking, lock-free (compute + `SetRate`), and must not re-enter the
  manager — codified in the interface contract — so holding `fleetMu` across the
  push is safe.
- `manager_assignment.go` is in the pre-PR gate set, and this adds work to a
  monitor-goroutine path; per repo rules the change needs an integration
  concurrency stress test (template:
  `test/integration/manager/epoch_monitor_concurrency_test.go`).

## 8. Config and validation

- `config.go`: add `HandoffConfig.ClaimWriteClusterRate float64` `validate:"gte=0"`
  immediately after `ClaimWriteBurst`. In `Config.Validate` (~706): error if
  `ClaimWriteClusterRate > 0 && ClaimWritePerSec == 0`; the existing
  `burst >= 1 when perSec > 0` rule already covers burst because cluster requires
  perSec. In `ValidateWithWarnings` (~817): inert WARN when
  `ClaimWriteClusterRate > 0 && !EnableTwoPhaseHandoff`.
- `consumer/options.go`: add `consumerCreateClusterRate float64` field +
  `WithConsumerCreateClusterRate(clusterPerSec float64)` option. In
  `resolveConsumerCreateLimiter` (`consumer/dynamic.go:735`): explicitly reject
  `clusterPerSec < 0` (mirrors the existing `perSec >= 0` check at
  `consumer/dynamic.go:746` and the claim-write `gte=0`); error if cluster set
  without `WithConsumerCreateRate`, or with an injected limiter; otherwise build
  the `*TokenBucketLimiter` as today and capture its `RateSetter` handle.

## 9. Metrics (optional, low priority)

Following the existing D7 sidecar pattern
(`claimWriteThrottleObserver` / `ConsumerCreateThrottleObserver`), optionally add
a sidecar that records the live effective per-worker rate and observed N as
gauges (e.g. `parti_claim_write_effective_rate`,
`parti_consumer_create_effective_rate`, `parti_observed_worker_count`). Emitted
on each retune. Type-asserted at build time; public
`HandoffMetricsRecorder` / `WorkerConsumerMetrics` interfaces stay unchanged.
This is observability sugar and may ship in a follow-up if it risks scope creep.

## 10. Testing

Unit:
- `ratelimit`: `SetRate` changes the steady-state rate; concurrent
  `SetRate` + `Wait` under `-race`; `RateSetter` type-assertion; `SetRate`
  preserves burst.
- `observeFleetSize`: rate computation `min(ceiling, cluster/N)` across N
  transitions; clamp at N=1; no-op when N unchanged; no-op when `clusterRate==0`;
  claim-write `SetRate` is called with the expected value.
- **Stale-fence (P0):** feeding a commit with a *lower* `(Version, LeaderRevision)`
  than the last observed one must **not** retune (no N regression); an
  out-of-order reconcile snapshot is ignored; a strictly-superseding commit does
  retune.
- consumer `Dynamic.ObserveWorkerCount`: recomputes and retunes the built-in
  limiter; no-op for injected limiter / no cluster rate.
- composite forwarding: `ObserveWorkerCount` reaches all `FleetSizeObserver`
  children; **late-add replay (P1):** a child added via `Add` *after* an N
  observation receives the cached N immediately.
- config: validation errors (cluster without per-worker; cluster + injected
  limiter; negative cluster rate) and inert-config warnings.
- Negative-space (per repo discipline): a *single* N change that returns to the
  original N must not leave a wrong rate; observing the same N repeatedly issues
  no `SetRate`; a stale commit issues no `SetRate`.

Integration (`test/integration/manager/`):
- Fleet grows/shrinks (workers join/leave) while this worker's slice is
  unchanged → this worker still retunes (proves the pre-debounce hook).
- Steady-state aggregate bound: with `clusterRate` set and a *stable* N workers
  (post-convergence), measured aggregate create/claim-write rate stays
  `<= clusterRate` within per-worker burst tolerance (`Σ burst`). Asserted at
  steady state, not during a transition.
- Concurrency stress: aggressive commit churn + concurrent applies under `-race`
  (monitor-goroutine rule).
- Cross-feature contracts (1)-(4) from AGENTS.md must still pass unchanged — this
  feature touches neither failure classification nor error routing.

## 11. Non-goals

- No distributed rate oracle / no cross-worker coordination beyond the shared
  committed `Workers` list.
- No retuning of injected custom limiters.
- No change to `PhaseConcurrency` (caps simultaneity; orthogonal to rate).
- No change to burst semantics (burst stays the per-worker instantaneous
  allowance; it does not scale with N).
- No change to the static-rate default-off behavior.

## 12. Backward compatibility and version

All additions are new optional config fields, a new option, and two new optional
interfaces. No released API changes (the static rate limits are themselves
unreleased, shipping in this same v2.8.0). With the cluster knobs unset, runtime
behavior is identical to the static design. Target version remains **v2.8.0**.

## 13. File touch-list (for the implementation plan)

- `internal/ratelimit/ratelimit.go` — `SetRate` method, `RateSetter` interface,
  optional `Limit()`.
- `config.go` — `ClaimWriteClusterRate` field, `Validate`, `ValidateWithWarnings`.
- `manager.go` — `fleetMu sync.Mutex` guarding the last-observed tuple
  `(lastFleetVersion, lastFleetLeaderRev, lastObservedN)` (kept together so the
  freshness fence and the recorded N never split); `fleetSizeObserver`;
  `asFleetSizeObserver`; construction wiring beside `capReporter` (manager.go:447).
- `manager_assignment.go` — `observeFleetSize`, hook in `runCommitWatchSession`
  `onUpdate`/`onReconcile`; initial-bootstrap observe in `manager.go:692` path.
- `fleet_size_observer.go` (new) — `FleetSizeObserver` interface.
- `composite_updater.go` — forward `ObserveWorkerCount`.
- `consumer/options.go` — `consumerCreateClusterRate` field +
  `WithConsumerCreateClusterRate`.
- `consumer/dynamic.go` — capture `RateSetter` handle, `ObserveWorkerCount`,
  validation in `resolveConsumerCreateLimiter`.
- `manager_handoff_ratelimit.go` — (no build-time change; retune lives in
  `observeFleetSize`) optional metrics sidecar if §9 is included.
- Tests per §10.
