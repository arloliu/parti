# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [v2.10.1] - 2026-07-17

A small additive follow-up clearing two deferred observability items. No
assignment or routing behavior changes; no API is removed or changed
incompatibly.

### Added

- **`Manager.LabelState()` + `parti.LabelState`** — pull-style label
  observability for deployments without a `MetricsCollector`: the leader
  retains its last published per-label pool sizes and parked partition
  counts, and the accessor returns a copied snapshot ("are any `vip`
  partitions parked right now?" from a health endpoint, zero collector
  code). Leader-only lifecycle mirroring the push gauges: populated
  strictly post-publish, cleared on leadership loss/stop. See "Label
  observability without a metrics pipeline" in `docs/LABELS.md`.
- **`types.HandoffSweepMetricsRecorder`** — optional capability interface a
  `HandoffMetricsRecorder` may additionally implement to receive
  `IncClaimSweepPass(origin, outcome, reason)` for every admitted claim
  sweep that runs a pass body (origin `apply|ticker`, outcome `full|cached`,
  and the closed
  full-pass reason set incl. `mismatch`/`forced`). The bundled
  `PrometheusRecorder` exports it as
  `claim_sweep_passes_total{origin,outcome,reason}`. This is the
  measurement precondition for the post-churn re-latch investigation
  (issue #75): full-pass origins no longer need to be inferred from timing
  signatures. Existing recorder implementations are unaffected;
  `types.NopHandoffMetricsRecorder` gains a no-op `IncClaimSweepPass`.

## [v2.10.0] - 2026-07-17

A hardening and load-overhead release driven by a measurement campaign against
the perf rig (W≤100 workers, P≤10,000 partitions, R=3). Six pre-existing bugs
found by the campaign's review discipline are fixed, the leader now gates
periodic claim sweeps (removing the largest idle KV-read multiplier), and two
new knobs let large deployments cut idle pull-loop CPU without giving up
detection latency. All changes are additive or strictly-tightening; no public
API is removed or changed incompatibly.

### Added

- **`parti.WithBucketEpochProbeInterval(d)` / `Config.BucketEpochProbeInterval`**
  — configures the epoch-fence monitor's bucket-recreation probe period
  (default 10s, previously hardcoded to OperationTimeout). Lower it for faster
  bucket-swap detection or raise it to shave idle KV reads on very large fleets.
- **`consumer.WithPullHeartbeatCap(d)` / `PullHeartbeatCap` on every consumer
  config** — bounds the derived pull heartbeat (normally `FetchTimeout/2`)
  independently of `FetchTimeout`, so raising `FetchTimeout` to cut idle
  pull-request churn (measured: ~0.77 idle / ~1.13 loaded CPU cores saved at
  P=10,000 going 5s→30s, with P99 delivery latency improved) no longer
  stretches deleted-consumer detection. `0` (default) disables the cap; a
  nonzero value must be within nats.go's `PullHeartbeat` validity range
  [500ms, 30s]. See the new "Tuning at high partition counts" section in
  `docs/SCALING.md`.
- **Leader-gated claim sweeps** — the periodic (ticker-origin) claim sweep now
  runs only on the leader, with followers keeping a phase-staggered ~5-minute
  backstop so a wedged or absent leader can never leave expired claims
  unswept. Apply-origin and startup sweeps are unchanged (every worker). At
  W=100 this removes ~99% of steady-state sweep-driven KV reads. Orphan-claim
  reaping is additionally fenced against stale leadership samples
  (generation-checked, revision-conditioned deletes), so a deposed leader's
  in-flight sweep can never delete a claim the new leader considers live.

### Fixed

- **Assignment continuity across coalesced versions** — the prepare phase
  diffs against the worker's last *committed* assignment, and retry/stash
  coalescing routinely leaves that more than one version behind the incoming
  one. Across such a gap, a partition that transited another owner and
  returned (A→B→A) looked unchanged and was silently skipped — leaving the
  worker permanently gated off the partition — and a claim reaped during a
  missed removal/re-add was never recreated. Any non-adjacent version
  transition now fails open into a full prepare walk. This is an explicitly
  partial repair: equal-Version authority divergence and cross-worker claim
  staleness are documented open hazards (pinned by tests) deferred to a
  claim-level commit-identity fence project.
- **Assignment payload fetch failures are retried** — a commit whose payload
  fetch or verification failed was silently dropped; the worker sat on its
  stale assignment until the next unrelated commit. Fetch failures now enter
  the same bounded-backoff retry loop an apply failure uses, coalescing to the
  newest target.
- **Shared accepted-target fence across the retry loops** — the apply-retry
  and (new) fetch-retry loops each track their own in-flight target; without a
  shared fence, a slow retry could apply a target that a sibling loop's newer
  accepted target had superseded — with a removal-guard bypass and, combined
  with the continuity fail-open, a cross-worker claim steal in the worst
  interleaving. Both delivery gates now raise a monotonic highest-accepted
  (Version, LeaderRevision) fence that the apply pipeline checks before any
  coordinator work.
- **Lost wakeup in the assignment retry loops** — a retry stashed in the
  window between a retry loop's final empty-stash check and its exit flag
  clearing was stranded forever. Both loops now re-check the stash after
  deactivating and re-activate themselves (context-guarded to avoid a
  shutdown-time respawn loop).
- **Payload GC vs. adoption race** — the commit-payload garbage collector
  could delete a payload that a concurrent publish was adopting (reusing),
  producing a commit referencing purged data. Adoption now CAS-touches the
  payload to a fresh revision before finalizing, and GC's delete is
  revision-conditioned, so whichever side lands first wins deterministically
  and the loser retries cleanly.
- **Orphan-claim reaping grace across reappearance** — a claim owner that
  disappeared, reappeared (re-vouched), then disappeared again kept its
  original orphan clock, so the reaper could delete the claim without a full
  fresh grace window. Reappearance now resets the clock.
- **Heartbeat scan reads are deadline-bounded** — the worker monitor's
  heartbeat scan could stall indefinitely on a degraded KV; per-key reads now
  run under a per-pass deadline.
- **`FetchTimeout` above 60s no longer breaks pull consumers** — the derived
  pull heartbeat (`FetchTimeout/2`) had no ceiling, and nats.go rejects an
  explicit heartbeat above 30s, so any `FetchTimeout > 60s` made every
  iterator creation fail and the pull loop spin in a restart/warn cycle
  forever. The derivation is now clamped to nats.go's [500ms, 30s] validity
  range at every derivation site (all four consumer engines and the perf rig's
  calibrate tool).

### Documentation

- New **"Tuning at high partition counts"** section in `docs/SCALING.md`:
  measured FetchTimeout guidance, the PullHeartbeatCap pairing (first
  missed-heartbeat signal vs confirmed burst-threshold recovery), and the
  leader-audit cadence note.
- `docs/API_REFERENCE.md` gains the `WithPullHeartbeatCap` row.

## [v2.9.1] - 2026-07-10

Label follow-up APIs that remove the workaround code label consumers were
writing against v2.9.0: a validator so callers can reject a bad label at their
own API boundary, a label-preserving write primitive, a batch validator, an
embeddable no-op metrics base, and a way to actually reach immediate spill. All
additive except one intentional error-text change (below); with zero labels
anywhere in the fleet, assignment output is unchanged from every prior release.

### Added

- **`types.ValidateLabel(s string) error`** — validates a single partition or
  worker label against the library's own rules (at most 64 bytes, no `.`, no
  ASCII whitespace; the empty string is valid). `Partition.Validate` and the
  worker-label normalizer now delegate to it, so a consumer can validate a label
  at its own API boundary with zero risk of the rules drifting.
- **`types.ValidatePartitions(ps []types.Partition) error`** — runs the exact
  per-partition `Validate` plus `CanonicalID` duplicate check the `source.NatsKV`
  write path performs (same first-error order and messages), so a consumer can
  reject a malformed batch at its boundary with a precise message instead of a
  generic error from inside a write.
- **`types.MergeLabels(current, intents) ([]types.Partition, []string, error)`**
  — a label-preserving write primitive keyed on `Partition.ID()`: a `nil` intent
  clears a label, a non-`nil` intent sets it, an absent id is left unchanged, and
  every unmentioned label is preserved. Hand the result to `source.Modify`
  instead of hand-rolling an inherit/set/clear CAS closure. Returns the unmatched
  ids (so a caller can reject a typo'd id) and **fails closed** on an `ID()`
  collision rather than guessing which partition to relabel.
- **`types.NopMetrics`** — an exported composite no-op satisfying the full
  `types.MetricsCollector` and its optional `types.LabelMetrics` extension. Embed
  it and override only the methods you care about (e.g. the label gauges for a
  health endpoint) instead of stubbing the whole eight-interface surface.
- **`parti.WithLabelSpillGrace(d time.Duration)`** — overrides
  `Config.LabelSpillGrace`, and is the only way to reach immediate spill
  (`d == 0`): the non-pointer config field silently re-defaults an explicit `0`
  to `60s`. The option wins over the config field (mirroring `WithWorkerLabels`)
  and rejects a negative duration at `NewManager` with an error wrapping
  `types.ErrInvalidConfig`.

### Changed

- **Label validation error text is now uniform.** The partition-label and
  worker-label validators previously produced different messages (`partition
  label ...` vs `worker label ...`); both now surface `types.ValidateLabel`'s
  canonical `label ...` wording. This is an error-**text** change only — no API
  or behavior change, no change to which labels are accepted or rejected. Code
  that matched on the old per-caller prefixes should match on the shared text.

### Docs

- `docs/LABELS.md`: the VIP promotion workflow now uses `MergeLabels`; a new
  "Validating at your API boundary" note covers `ValidateLabel` /
  `ValidatePartitions`; a "Label observability without a metrics pipeline"
  subsection shows an in-memory collector embedding `types.NopMetrics` with the
  leader-only caveat spelled out; and the spill-grace section documents reaching
  immediate spill via `WithLabelSpillGrace`.

## [v2.9.0] - 2026-07-08

Label-based partition assignment: pin a subset of partitions to a dedicated
worker pool (a "VIP" tier, a latency-sensitive tenant, a GPU-backed task
class) inside the same management plane — one heartbeat/election/assignment
bucket set, one partition list, one leader — instead of running a second
deployment of Parti. Fully additive: with zero labels anywhere in the fleet,
assignment output is unchanged from every prior release. Promote or demote a
partition at runtime by rewriting the partition list; no worker restart
required.

### Added

- **`types.Partition.Label`** — one optional string label per partition. A
  routing hint, not identity: excluded from `CanonicalID`, `HashID`,
  `Compare`, and `PartitionSetDigest`, so relabeling a partition that is
  already on a matching worker moves no ownership.
- **`types.Heartbeat.Labels`** — a worker's label set, fixed for the process
  lifetime and published on every heartbeat.
- **`Config.WorkerLabels`** (yaml `workerLabels`) and **`parti.WithWorkerLabels(labels...)`**
  — configure a worker's label set; the option overrides the config field
  when both are set. Validated, sorted, and deduplicated at construction
  time (charset rules matching partition keys, 64-byte cap, 16 labels max).
- **`Manager.WorkerLabels()`** — returns this worker's resolved (sorted,
  deduplicated) label set. Each call returns a fresh clone, so the caller
  may freely mutate the result without affecting manager state.
- **`Config.UnlabeledPartitionPolicy`** (yaml `unlabeledPartitionPolicy`,
  default `"dedicated"`) — `"dedicated"` routes unlabeled partitions to
  unlabeled workers only (falling back to all workers when none are live);
  `"shared"` routes them to any worker. Leader-side; must be fleet-uniform,
  same contract as the `AssignmentStrategy` choice.
- **`Config.LabelSpillGrace`** (yaml `labelSpillGrace`, default `60s`) — how
  long a label's worker pool must be continuously empty before its
  partitions spill to the fallback ladder (unlabeled workers, or — only in
  an all-labeled fleet — any worker). A partition parked during the grace
  window is deliberately unassigned and durably accounted for in the
  assignment commit (`AssignmentCommit.ParkedCount` / `ParkedDigest`); it is
  never silently dropped from coverage.
- **`types.LabelMetrics`** — optional extension interface a
  `MetricsCollector` may also implement for per-label pool-size and
  parked-count gauges plus spill/label-change/incarnation-reject/fallback
  counters. Existing collectors that don't implement it are unaffected;
  label mode runs without them.
- **`AssignmentPayload.WorkerLabels` / `WorkerLabelsKnown`** and
  **`AssignmentCommit.ParkedCount` / `ParkedDigest`** — wire additions
  carrying labels-of-record and parked-partition accounting through both the
  commit path and the legacy alias path. A worker whose configured labels
  don't match its payload's labels-of-record rejects the assignment outright
  (no consumer attach/detach, no ack) instead of misapplying a payload
  computed for a different process incarnation behind the same stable ID —
  the guard that makes stable-ID takeover across a relabel safe.
- **`provision`**: partition records carry `label` alongside `keys` and
  `weight`; `partictl partitions plan`/`apply` report label-only edits as a
  change (`PartitionWeightChange.OldLabel` / `NewLabel`), never as a
  no-op.
- See [Label-Based Partition Assignment](docs/LABELS.md) for the full
  operator guide: the worker/partition match rule, the park-then-spill
  grace window and its worst-case-stall formula, the stale-incarnation
  guard, and the recommended one-`WorkerIDPrefix`-per-deployment pattern.

### Compatibility and rollout

Two rollout-ordering rules apply before adopting labels on an existing
fleet; skipping either produces a silent failure mode, not an error:

1. **Upgrade every deployment to a label-aware version before labeling any
   partition.** Any worker can win leader election — a mixed fleet flips
   between labeled and legacy (label-blind) assignment on every failover
   until every deployment is upgraded.
2. **Upgrade every writer of the partition list first, including the
   `provision` CLI.** An old writer's full-list rewrite silently strips the
   `Label` field, and the new label-aware change detection faithfully
   propagates that stripped list as a real edit — demoting every labeled
   partition, not leaving them untouched.

On the first commit published by an upgraded leader, every worker's payload
is re-hashed once (the new labels-of-record presence bit enters the
canonical payload bytes even for unlabeled workers), so the fleet runs one
apply+ack cycle with no ownership movement — the same benign shape as a
label-only edit.

## [v2.8.2] - 2026-07-03

Scalability release: Parti's coordination buckets no longer pay steady-state
scan costs. Routine heartbeat refreshes stop triggering full key scans of the
heartbeat bucket — the source of the "JetStream cluster new consumer leader"
log churn at scale — and the two handoff-bucket scan sites (claim-resolver
reconcile, two-phase sweep) are gated behind a cheap read-only
stream-position probe. Idle scan cost now scales with worker count instead
of workers × partitions, cutting idle meta-layer consumer churn and KV read
load by ~95% — the property that matters on the path to 10,000-partition
deployments. All recovery guarantees, public APIs, and defaults are
unchanged; upgrading is a drop-in.

### Changed

- **Heartbeat watcher scan suppression** — routine heartbeat refreshes no
  longer trigger a full key scan of the heartbeat bucket, eliminating
  steady-state ephemeral-consumer churn (~12k consumer creations/hour at
  10 workers × 3s heartbeat interval — the "JetStream cluster new consumer
  leader" log spam at scale). Crash-detection cadence is preserved via a
  client-side expiry sweep that suspends suppression for the crash window;
  non-connectivity enumeration-stall degradation now enters at its designed
  polling cadence (≤ threshold × HeartbeatTTL/2) instead of an undocumented
  heartbeat-write-coupled cadence.
- **Idle handoff-bucket scan gating** — the claim resolver's periodic
  reconcile and the two-phase sweep ticker no longer pay fixed-rate scan
  costs while the handoff bucket is unchanged. Each ticker pass first
  probes the bucket's backing stream position (one read-only
  `STREAM.INFO` request through a dedicated KV handle — no consumer
  creation) and skips the `Keys()`/`ListKeys()` walk and per-key reads
  once two consecutive probes taken 2s apart confirm the bucket
  byte-identical to the last clean pass. At 10 workers / 2,000 claims
  with default intervals this removes ~40 ephemeral consumer
  creations/min (each a JetStream meta-layer Raft proposal) and ~80k
  per-key reads/min at idle across the two scan sites; idle scan cost
  now scales with worker count instead of workers × partitions.
- **Fail-open safety** — full-rate scanning resumes automatically on any
  bucket write, probe failure, or unsafe bucket config, and an
  unconditional full scan still runs at least every 20 passes, bounding
  the effect of a stale probe answer to a single interval. An
  edge-triggered warning fires once if the handoff bucket's config is
  changed to permit TTL-based expiry (which would break position-based
  change detection), with an info line on recovery.
- **Recovery semantics unchanged** — sweep expiry resets, commit
  finalization, and orphan reaping still run every tick (from a cached
  claim view when the bucket is provably unchanged), and `Apply`-path
  sweeps are entirely unaffected. No new public APIs or configuration
  knobs; observability is log-only.

## [v2.8.1] - 2026-06-21

Maintenance release on top of v2.8.0. Two small correctness fixes — a
`-race`-detectable data race on the manager's calculator field when startup and
`Stop` overlap, and a `SourceBucketMissing` gauge that could read inverted under
concurrent source-availability transitions — plus a batch of documentation
corrections and several behavior-preserving internal cleanups (leadership-hook
dedup, a dead tie-break branch, and removal of an orphaned internal package). No
exported-API or behavioral-contract changes; upgrading is a drop-in.

### Fixed

- **Manager startup data race** — `markStartupAssignmentApplied` read the
  `calculator` field without holding the manager lock while a concurrent `Stop`
  could write it (a race detectable under `-race`). The field is now read under
  the read lock.
- **Source bucket-missing gauge consistency** — the `SourceBucketMissing` gauge
  write happened outside the mutex that serializes the underlying availability
  state, so concurrent transitions could leave the gauge inverted until the next
  edge. The gauge write is now ordered with the state change.

### Documentation

- Corrected the `Assignment` struct in the architecture doc, the default
  `StartupTimeout` (60s, not 30s), the force-reassign method name
  (`RefreshPartitions`), the `NewNatsKV` signature, the `strategy` extreme-weight
  threshold default (>20×, configurable via `WithExtremeThreshold`), a `partictl`
  exit-code note, and the `types` / `election` / `heartbeat` package doc
  synopses and examples.

## [v2.8.0] - 2026-06-17

Two opt-in rate-limit controls that bound the per-worker RPC rate Parti drives
against the NATS cluster, plus the seam one of them is built on. Both default
**OFF**, so upgrading is a behavioral no-op; enable them only after measuring
your cluster's safe rate. They address two distinct flood vectors seen at large
fleet scale: a **consumer-create storm** — a `Dynamic` worker whose partition
set grows to tens of thousands, or a mass recovery, issuing back-to-back
`CreateOrUpdateConsumer` RPCs — and a **claim-write storm** — two-phase handoff's
`PutIfEpoch` calls bursting during a large-fleet restart or rapid rebalance.
Either can drive the cluster to hang or OOM under load. Each per-worker limit
now has an optional **fleet-size-aware (adaptive)** variant that bounds the
cluster-wide aggregate (the `N × perWorkerRate` problem) to a configured target
via `min(perWorkerCeiling, clusterRate / N)` — still default OFF, each knob
independent.

### Added

- **`consumer.WithConsumerCreateRate(perSec, burst)`** — opt-in per-worker
  token-bucket that paces every physical `CreateOrUpdateConsumer` attempt
  (including retries) across the initial-assignment add loop and the
  per-partition recovery/recreation paths, from one shared budget. To share one
  budget across multiple `Dynamic` consumers, build a
  **`consumer.ConsumerCreateLimiter`** with **`consumer.NewConsumerCreateLimiter(perSec, burst)`**
  and pass it to **`consumer.WithConsumerCreateLimiter(l)`** (the public limiter
  interface and constructor keep this usable from outside the module — the option
  no longer requires naming an internal type). See `docs/CONSUMERS.md` and
  `docs/OPERATIONS.md` for sizing and the handoff-overlap / `StartupTimeout`
  interactions.
- **`jsutil.EnsureConsumerWithOptions(...)`** with **`jsutil.WithBeforeAttempt(fn)`**
  (and the `jsutil.EnsureConsumerOption` type) — a per-attempt hook seam on the
  retrying consumer-ensure helper, invoked before every physical RPC attempt
  including retries. The existing `jsutil.EnsureConsumer` signature is preserved
  and now delegates to it.
- **`parti.HandoffConfig.ClaimWritePerSec` / `ClaimWriteBurst`** — opt-in
  per-worker token-bucket that paces every physical handoff claim-write
  (`PutIfEpoch`), including CAS retries, across the two-phase coordinator's
  prepare/commit/stabilize/reap phases and the startup hygiene/resume loops,
  from one shared budget. Requires `EnableTwoPhaseHandoff`. It complements
  `PhaseConcurrency` (which caps how many claim-writes are in flight; this caps
  their throughput). `ClaimWriteBurst` must be `>= 1` when the rate is `> 0`
  (rejected at `Config.Validate` otherwise). See `docs/OPERATIONS.md`
  §Claim-Write Rate Limiting and `docs/CONFIGURATION.md`.
- **`consumer.WithConsumerCreateClusterRate(clusterPerSec float64)`** — opt-in
  fleet-size-aware overlay on `WithConsumerCreateRate`. Effective per-worker
  rate = `min(perSec, clusterPerSec / N)`, where `N` is the committed
  worker-count the manager observes live. Bounds the steady-state cluster-wide
  aggregate to `clusterPerSec`; the per-worker ceiling (`perSec`) caps the
  transient overshoot while workers converge on a new N. **Requires**
  `WithConsumerCreateRate` (which supplies the per-worker ceiling and burst);
  rejected at `NewDynamic` if used alone or with an injected
  `WithConsumerCreateLimiter` (an injected/shared limiter is not adaptively
  retuned). `clusterPerSec` must be ≥ 0; 0 = static per-worker behaviour (the
  default). Caveats: the aggregate **burst** is `Σ burst` across workers, not
  bounded by `clusterPerSec` — keep burst small if aggregate burst matters.
  Retuning is eventually-consistent; observation lag ≈ assignment-watcher
  reconcile floor (~30 s). See `docs/CONSUMERS.md` §Consumer-Create Rate
  Limiting.
- **`parti.HandoffConfig.ClaimWriteClusterRate float64`** — opt-in fleet-size-aware
  overlay on `ClaimWritePerSec`. Effective per-worker rate = `min(ClaimWritePerSec,
  ClaimWriteClusterRate / N)`. Bounds the steady-state cluster-wide aggregate;
  `ClaimWritePerSec` caps transient overshoot. **Requires** `ClaimWritePerSec > 0`
  and `EnableTwoPhaseHandoff`; `Config.Validate` errors if `ClaimWriteClusterRate > 0`
  with `ClaimWritePerSec <= 0`; `ValidateWithWarnings` emits an inert WARN if
  `ClaimWriteClusterRate > 0` with two-phase handoff OFF. Default 0 = static
  per-worker behaviour. Same aggregate-burst and retuning-lag caveats apply. See
  `docs/OPERATIONS.md` §Claim-Write Rate Limiting and `docs/CONFIGURATION.md`.
- **Throttle metrics** (Prometheus sidecar; emitted only when the relevant
  collector is wired, and only on positive-delay waits — burst-absorbed RPCs are
  not counted): `parti_worker_consumer_create_throttled_total` /
  `parti_worker_consumer_create_throttle_wait_seconds` for consumer-create, and
  `parti_handoff_claim_write_throttled_total` /
  `parti_handoff_claim_write_throttle_wait_seconds` for claim-write.

## [v2.7.1] - 2026-06-12

Patch release with two fixes: complete shutdown diagnostics, and a garbage
collector for orphaned two-phase handoff claims. No exported API changes.
One behavior change at defaults: the leader's claim sweep now deletes
claims for partitions that have been removed from the partition source
(after a conservative 10-minute verified-absence grace); previously such
claims accumulated forever.

### Fixed

- **Orphaned handoff claims no longer accumulate forever.** The handoff
  bucket deliberately carries no `MaxAge` (stable ownership claims must
  never age out from under the pull-gating resolver), so claims for
  partitions permanently removed from the partition source leaked
  unboundedly — walked by every resolver warm and every claim sweep. The
  two-phase coordinator's claim sweep now deletes a stable, no-pending-owner
  claim once its partition has been continuously absent from BOTH the
  leader's source view and the latest committed assignment for a 10-minute
  grace period — a partition the live commit still references is never an
  orphan, so an owner consuming through a stalled rebalance window is never
  cut off. The delete is revision-checked (compare-and-delete), so a
  concurrently re-added partition's claim transition always wins over the
  reaper. Followers never reap (a config-skewed follower, e.g. an old static
  partition list mid-rolling-upgrade, must not be an authority on which
  partitions exist), and any stretch where the leader cannot verify the
  set — leadership loss, source or commit read failure — restarts the grace
  clock.
- **`Manager.Stop` now reports every failing shutdown component.** Stop
  collected component errors with first-non-nil-wins semantics — and the
  partition-source step unconditionally overwrote an earlier
  leadership-release error — so a shutdown in which multiple components
  failed surfaced only one root cause. All five error sites (leadership
  release, partition source, heartbeat, worker-ID release, stop-timeout)
  now accumulate via `errors.Join`, keeping every cause inspectable with
  `errors.Is`. A healthy stop still returns `nil`.

## [v2.7.0] - 2026-06-11

Dynamic-consumer healing release. It closes a family of silent-stall paths
where a worker reported `Stable` while its consumer was dead or stranded, runs
a correctness sweep across all four consumer types, and fixes an assignment-retry
coalescing bug that could leave partition ownership silently divergent. Three
themes: **stream-missing healing** — a deleted stream or exhausted recovery now
drives the manager to a terminal `Degraded` hold so readiness-driven rotation
can act, instead of flapping back to `Stable` with a dead consumer;
**consumer-wide correctness** — `Stop`/`Close` is now terminal, retry backoff
grows as configured, `Queue` validates compatibility before creating durables,
a key-dispatch idle-exit race is closed, and disabled-recovery stalls become
visible; and **assignment-retry coalescing** — retries now coalesce by the full
applied identity the apply gate trusts. Two new exported symbols
(`consumer.ErrConsumerStopped`, `consumer.WithSuppressManagerDegradeOnStreamMissing`)
make this a minor rather than a patch. One behavior change at defaults:
`WithOnPermanentFailure` no longer suppresses the manager's stream-missing
auto-degraded route.

### Added

- **`consumer.ErrConsumerStopped`** — sentinel returned by `Start`/`Update`
  after a consumer has been stopped or closed. Re-exported from
  `types.ErrConsumerStopped`. Match with `errors.Is`.
- **`consumer.WithSuppressManagerDegradeOnStreamMissing()`** — opt-out option
  that restores the pre-v2.7.0 behavior where registering
  `WithOnPermanentFailure` suppressed the manager's stream-missing
  auto-degraded route (see _Changed_ below).

### Fixed

- **Deleted stream now exhausts recovery instead of stalling silently.** With
  `RecoveryStrategy` enabled and an active iterator, deleting the underlying
  stream surfaces `ErrNoHeartbeat`, and the burst confirmation probe treated
  the stream-scoped `ErrStreamNotFound` answer as "consumer still exists" —
  classifying the permanent loss as transient-forever. The consumer ping-ponged
  between heartbeat failures and backoff indefinitely, stream-missing
  exhaustion never fired, and the worker reported `Stable` with a stalled
  consumer. The probe now routes stream-not-found to the bounded stream-missing
  detour, so exhaustion reaches `OnPermanentFailure` and the manager's terminal
  `Degraded` hold within the configured `RecoveryRetry.MaxAttempts` budget.
  (`Queue`, `Broadcast`, and internal partition consumers now log this case as
  stream-missing instead of an unlabeled transient backoff.)
- **Terminal `Degraded` hold on stream-missing exhaustion.** Manager now stays
  `Degraded` permanently after a dynamic consumer's stream-missing recovery
  exhausts, so readiness-driven rotation can occur; previously it returned to
  `Stable` within seconds while dead partition consumers were still assigned and
  silently not consuming.
- **Assignment retries coalesce by full applied identity.** The apply-retry
  stash and the commit coalesce/drain compared candidates by `Version` alone,
  while the core apply gate orders by `(Version, LeaderRevision)` and the
  applied-ack identity also spans the partition-set digest and source
  revision. A same-version pair — produced by a lost commit CAS racing its own
  pre-published alias — under two consecutive apply failures dropped the
  commit-authority assignment from the stash: the retry applied the stale
  alias digest and nothing in direct mode converged the worker until the next
  rebalance, leaving silent partition-ownership divergence behind a `Stable`
  status. Stash and coalesce comparisons now use the same full identity
  ordering the apply gate trusts. The leader audit additionally logs a
  warning for workers stuck behind in direct mode, where automatic repair is
  deliberately unavailable.
- **`Dynamic.Update` rejects over-cap assignments before any mutation.** Returns
  `ErrMaxSubjectsExceeded` (as documented) when an assignment exceeds
  `MaxConcurrentSubjects`, instead of silently skipping excess partitions — which
  could strand a partition unowned after a committed handoff. The rejection error
  names the stream and fires the
  `parti_worker_consumer_guardrail_violations_total{kind="max_subjects"}` metric.
- **`Dynamic.Update` surfaces remove-timeout failures.** Returns an error when
  removed partition loops fail to stop within `DrainOnRemoveTimeout`, instead of
  reporting success while a handler could still be processing; the manager retries
  the apply and converges.
- **`Stop`/`Close` is terminal for `Static`, `Broadcast`, and the Dynamic worker
  consumer.** Restarting after stop previously half-worked (channels and loop
  state were not rebuilt) and, for `Dynamic`, `Close` nils the gate resolver, so
  a post-Close `Update` restarted pull loops WITHOUT the configured processing
  gate — a silent safety downgrade. `Start`/`Update` after `Stop`/`Close` now
  returns the new `ErrConsumerStopped` sentinel; construct a new consumer to
  restart.
- **Disabled-recovery iterator stalls are now visible.** With `RecoveryStrategy`
  disabled (the default), persistent iterator failures in `Queue`, `Static`, and
  `Broadcast` consumers — e.g. a durable deleted via `InactiveThreshold` expiry —
  retried silently at Debug level forever. Each restart now logs a Warn and fires
  the iterator-restart metric with reason `recovery_disabled`.
- **Broadcast and Dynamic retry backoff actually grows.** The consume-loop
  backoff re-slept a constant base delay, ignoring the configured `Multiplier`,
  `Max`, and `Seed`; it now grows via decorrelated jitter as documented (matching
  `Queue`).
- **Key-worker idle-exit ordering race.** A per-key dispatch worker's idle exit
  now decides under the dispatcher's write lock, closing a window where a
  concurrently dispatched message could land on a worker that was exiting and
  never be processed.
- **Queue validates WorkQueue compatibility before creating the durable.**
  `Queue.Start` previously created the durable consumer first and then failed
  the WorkQueue/recovery compatibility check, orphaning a just-created durable
  on the WorkQueuePolicy stream.
- **Queue closed-iterator hot loop.** An iterator that reports closed while the
  consumer is still running now takes the backoff path instead of respinning
  iterator creation at full speed.
- **All constructor validation failures wrap `ErrInvalidConfig`.**
  `errors.Is(err, consumer.ErrInvalidConfig)` now matches every
  construction-time validation failure across the four consumer constructors;
  struct-tag (range/required) failures were previously returned unwrapped.

### Changed

- **`WithOnPermanentFailure` no longer suppresses the manager observer.**
  Registering `WithOnPermanentFailure` no longer disables the manager's
  auto-degraded route for stream-missing exhaustion: both the application callback
  and the manager observer now fire (callback first). Use the new
  `WithSuppressManagerDegradeOnStreamMissing()` option to restore the previous
  opt-out behavior explicitly.
- **Over-cap assignments surface as visible apply failures.** An assignment that
  keeps a worker over `MaxConcurrentSubjects` indefinitely now surfaces as
  repeating "handoff apply failed" logs with an un-acked assignment version
  (leader-visible), rather than a silent gap — raise the cap or rebalance to
  resolve.
- **`FetchTimeout` below 1s now fails construction.** NATS pull consumers
  enforce a 1s minimum request expiry; configs in `(0, 1s)` previously passed
  validation and stalled at runtime. All consumer constructors now reject them
  with `ErrInvalidConfig`.

### Documentation

- **Zero-overlap claims corrected to the per-tier overlap contract.** Two-phase
  handoff orders the release of a partition (no unowned gap); it does not gate
  consumption. The processing gate is per-message admission control and cannot
  revoke an in-flight handler, so the irreducible window is one in-flight
  invocation plus AckWait-expiry redelivery of that message via the shared
  per-partition durable (mitigation: `consumer.NewWIPHandler`). The fictional
  leader-driven prepare/commit ACK exchange in `docs/LIFECYCLE.md` is replaced
  with the actual worker-driven KV CAS protocol, with a per-configuration-tier
  guarantee table; delivery is at-least-once at every tier.

## [v2.6.1] - 2026-06-07

Maintenance release on top of v2.6.0. Two correctness fixes — a recovery-exit
race that could strand a worker in `Degraded` with no record, and a misleading
operator warning — plus a large documentation sync (scaling, IOPS, and perf
findings) and internal consolidation of the degraded-state machinery. No API
breaks; the only behavior change at defaults is the recovery-exit fix.

### Fixed

- **Exit `Degraded` only on a confirmed `Degraded`→`Stable` transition.**
  `exitDegraded` cleared the degraded record after `transitionState(StateStable)`,
  which returns true vacuously when already `Stable`. A recovery tick landing in
  the enter window — record published by a concurrent `enterDegraded` but its
  state transition not yet run — could clear that in-flight record, stranding the
  worker in `Degraded` with a nil record where both recovery and alerting
  early-return and it cannot self-heal until an unrelated degrade re-arms it. The
  new `casToStableFromDegraded` clears the record only on a genuine `Degraded`→
  `Stable` CAS, refusing the racy window so the in-flight enter completes and
  recovers normally. Pinned by `manager_exit_confirmed_test.go` and the
  enter/recover race stress test.
- **Corrected the KV storage-mismatch startup warning.** `warnOnStorageMismatch`
  claimed parti's defaults are `MemoryStorage` (they are `FileStorage`) and
  pointed operators at `nats kv del <bucket>`, which deletes a key, not a bucket.
  The comment and `Warn` message are now storage-direction-agnostic and use
  `nats kv rm <bucket>` for bucket removal.

### Documentation

- **Operational guides synced with perf, IOPS, and scaling findings.** New
  `docs/SCALING.md`; updates to `docs/CONSUMERS.md`, `docs/OPERATIONS.md`, and
  `docs/CONFIGURATION.md` covering the consumer-replica/stream-replica match
  rule, retention-policy matrix, and large-fleet scaling guidance.
- **Partition-scaling feasibility study and guide.** A K-bounded
  (subject-filtered) consumer assessment, a guide for combining NATS'
  `partition()` subject transform with the existing `consumer.Dynamic`, and a
  standalone POC under `docs/plans/partition-scaling/`. Conclusion:
  `partition()` + `Dynamic` covers K≪N without shipping a new consumer type.

### Internal

- **Degraded-state consolidation.** Collapsed `degradedSince` +
  `lastDegradedReason` into a single `atomic.Pointer[record]`, consolidated the
  degrade-reason and KV-error handling paths, and shared watch-session,
  KV-bucket, and state-transition helpers across the manager. Calculator helpers
  shared and a dead restart-ratio path removed. Pure refactors apart from the
  recovery-exit fix above.
- **Perf-measurement rig.** New dynamic partition-consumer perf-measurement rig
  with design, plan, and baseline/production/metacontroller/queue-floor findings
  (`test/perf-measurement/`, renamed from `test/iops-investigation/`); its
  embedded NATS server/client bumped to the latest release.
- **Fixed-partition integration proofs.** Live-cluster tests proving
  `consumer.Dynamic` over a `WorkQueuePolicy` stream and `partition()` + `Dynamic`
  over a fixed partition count.
- **Test stability.** The full-NATS-outage test now waits for the async
  `OnDegraded` hook rather than reading the counter synchronously.

## [v2.6.0] - 2026-06-02

Hardening release that deepens v2.5.0's self-healing and readies parti for
large fleets. Three themes: **recovery-exit correctness** — a worker no longer
returns to `Stable` (or flaps) while the fault that degraded it still persists;
**thundering-herd hardening** — jitter, bounded handoff concurrency, and watcher
debounce smooth fleet-wide reassignment storms; and a **heartbeat-bucket storage
switch** so a single-node NATS restart no longer flaps the fleet. No API breaks.

### Changed

- **Heartbeat KV bucket defaults to `FileStorage`** (was `MemoryStorage`). With
  `MemoryStorage` a single-node JetStream restart lost the heartbeat stream and
  the fleet oscillated `Degraded`↔`Stable`; persisting it survives the restart.
  The added write IOPS is a flat, partition-count-independent term within the
  envelope accepted for v2.5.0's election-bucket switch. **Existing clusters need
  a one-time manual migration** — until migrated, the new heartbeat-reachability
  guard holds the worker in terminal `Degraded` (rotatable) instead of flapping,
  and a rotation re-creates the bucket as `FileStorage`. See "Heartbeat Bucket
  Storage Migration" in `docs/OPERATIONS.md`.

### Added

- **Thundering-herd hardening for large fleets** (opt-in, default off):
  `ApplyStartJitter` spreads fresh-version applies, `PhaseConcurrency` caps
  in-flight per-partition handoff KV operations, and `AssignmentWatcherDebounce`
  coalesces burst re-elections on both the assignment and commit watchers.

### Fixed — self-healing no longer heals on the wrong signal

- **Recovery exit is now trigger-aware.** It previously returned to `Stable` on a
  healthy *assignment* read while a *different* fault persisted. Three additive
  guards now AND onto the unchanged commitment guard — a live epoch re-probe (a
  wiped-and-recreated bucket stays degraded), a reason-scoped
  heartbeat-`Put`-after-degrade requirement (`kv-unavailable` stalls stay
  degraded), and heartbeat-bucket reachability (a missing heartbeat stream stays
  degraded, no flap) — and claim-loss self-stop is now documented.
- **Degrade on connected-but-KV-unavailable timeouts** — a heartbeat / election /
  stableID stall now escalates (reason `kv-unavailable`) instead of being swallowed.
- **A leader stalled on worker enumeration now degrades instead of silently
  freezing** — a heartbeat-`Keys` scan that sustains a non-connectivity deadline
  (while single-key heartbeat `Put` and election renewal keep succeeding) is
  classified as neither connectivity nor degrading, so it used to be swallowed and
  the leader froze its assignment, never reassigning a departed worker's
  partitions. It now degrades (reason `heartbeat-enumeration-stall`) once sustained
  and exits only after enumeration recovers; losing leadership while in this state
  is not a trap.
- **Self-heal a stuck version-advance** — a leader that bumped the commit version
  but failed to persist every claim no longer reports `Stable` with uncommitted
  claims; per-version commitment drives recovery.
- **KV-error circuit resets on a healthy heartbeat**, so one transient KV error no
  longer latches the fleet into degraded flapping.
- **Quorum-loss resolver no longer self-poisons** — a claim listed-but-unreadable
  during a transient read fault is no longer permanently tombstoned.
- **Handoff transfer removals wait for the gaining worker to commit**, and
  committed claims are finalized via sweep so handoffs converge under contention.
- **Startup robustness** — apply writes the full claim set on retry, the
  stable-readiness gate is corrected, and the `NatsKV` source recovers recreated
  buckets while signaling sustained source-bucket timeouts distinctly from deletion.

### Internal

- **nats.go → v1.52.0, nats-server/v2 → v2.14.1** (test/embedded-server dependency).
- **Simulation-coverage expansion** — ~18 chaos scenarios across KV, handoff,
  process-mode, and source faults, with per-pillar CI jobs; CI split into parallel
  `lint-unit` and `integration` runs. Plus a repo-wide `go fix` modernization pass.

## [v2.5.0] - 2026-05-24

Two themes: **self-healing under NATS infrastructure churn** — manager
paths that previously degraded silently on bucket wipes, stale
subscriptions, or partial KV scans now escalate through the existing
degraded circuit, and bounded-retry envelopes replace open-ended loops on
every monitor goroutine — and **operator tooling** — provision adds
stream + per-partition-consumer management, a `force` policy with
per-resource opt-in, and a Kubernetes operator driving the same provision
path from a `ProvisionedPartiEnv` CRD. `Manager.Start` is now fully
asynchronous (breaking).

### Breaking changes

> **Migration guide:** [`docs/MIGRATING_MANAGER_START.md`](docs/MIGRATING_MANAGER_START.md)

- **`Manager.Start(ctx)` returns after sanity checks**, not after
  `StateStable`. The initial assignment fetch and apply now run in a
  background goroutine. Block on `<-mgr.WaitState(parti.StateStable,
  timeout)` before reading `mgr.CurrentAssignment()`. A soft watchdog
  enters `StateDegraded(reason="startup-timeout")` once if
  `StartupTimeout` elapses without reaching Stable — independent of the
  runner, so it fires the readiness-probe rotation signal even when the
  runner is blocked.

  ```go
  // Before
  if err := mgr.Start(ctx); err != nil { /* handle */ }
  use(mgr.CurrentAssignment())

  // After
  if err := mgr.Start(ctx); err != nil { /* handle */ }
  if err := <-mgr.WaitState(parti.StateStable, 30*time.Second); err != nil { /* handle */ }
  use(mgr.CurrentAssignment())
  ```

### Added — self-healing

- **Worker-set shrink-confirmation defense.** Calculator rebalances now
  guard against silently-truncated heartbeat scans (NATS'
  `KeyValue.Keys()` can return `(partial-slice, nil)` when the watcher
  tears down mid-scan, producing a fresh-looking shrink). Two layers:
  the active-worker scan treats a sharply-shrunk read as suspicious and
  surfaces the cached set with `fresh=false`; the rebalance enforces an
  emergency-confirmation gate that requires `EmergencyDetector` to have
  captured at least one confirmed death. Tunable via
  `WorkerShrinkConfirmationCount` (default `2`) and
  `WorkerShrinkConfirmationThresholdPct` (default `50`). Mirrors the
  existing `PartitionShrinkConfirmation*` doctrine.
- **Bounded-retry envelopes** on `monitorAssignmentChanges`, the source
  watcher restart loop, the claim resolver watcher, and partition
  consumer iter-creation. After the per-episode attempt budget is
  exhausted, each loop enters `StateDegraded` with a named reason
  rather than retrying forever.
- **Epoch fence** detects KV bucket wipe-and-recreate via stream
  `Created` timestamp change and enters degraded rather than treating
  stale state as valid.
- **Stable-ID hardening.** Workers self-stop on a lost claim, take over
  stale claims belonging to dead workers, and reconcile the stable-ID
  bucket `MaxAge` on startup.
- **Source bucket-loss escalation.** `NatsKV` partition source fires
  the new `SourceUnavailableHook` (rate-limited) and exposes a
  `SourceBucketMissing` gauge when the bucket disappears.
- **Stream-missing recovery** for `consumer.Dynamic` — the consumer
  re-creates a deleted application stream and resumes; manager-side
  observer escalates recovery-exhaustion as a degraded signal.

### Added — provisioning

- **`partictl partitions plan|apply`** manages the partition table
  itself (record-level diff, `-prune` gates removals). SDK:
  `provision.PlanPartitions` / `ApplyPartitions`. New config:
  `PartitionSourceConfig.Partitions` declares the table inline.
- **`partictl stream view|plan|apply`** provisions application JetStream
  streams declared under `streams:` in `parti-env.yaml`.
  `provision.Config.Streams []StreamCfg` exposes `name`, `subjects`,
  `retention`, `storage`, `discard`, `replicas`, `maxAge`, `maxBytes`,
  `maxMsgs`, `description`. New actions: `create-stream`,
  `update-stream`, `stamp-stream-marker`. The top-level `plan` / `apply`
  / `adopt` / `view` commands also provision streams automatically when
  `streams:` is set; configs without a `streams:` block are unaffected.
- **`partictl consumers plan|apply`** precreates per-partition durable
  consumers for `dynamicConsumers:` targets opted in via
  `DynamicConsumerCfg.PartitionsRef`. Identity-only / runtime-owns
  model: precreation writes the runtime's NATS-immutable defaults; the
  consumer's `CreateOrUpdateConsumer` on worker start owns mutable
  tunables. A racing `ErrConsumerExists` re-reads and verifies identity
  before recording success. New error:
  `provision.ErrConsumerStreamMissing`.
- **`force` policy** (`provision.PolicyForce`) — strict superset of
  `safe-update` that additionally repairs drift-immutable resources by
  delete/recreate. Gated by a per-resource `allowDeleteRecreate: true`
  opt-in (new field on `controlPlane`, `partitionSource`, each
  `streams` entry, each `dynamicConsumers` entry). New actions:
  `recreate-kv`, `recreate-stream`, `recreate-consumer`. Stale plans
  re-classify before deleting; post-delete create failure wraps
  `ErrRecreateInterrupted` (re-running apply is safe).
- **Kubernetes operator** — nested Go module at `k8s/`
  (`github.com/arloliu/parti/v2/k8s`) reconciles a `ProvisionedPartiEnv`
  CRD (`apiVersion: parti.io/v1alpha1`, short name `ppe`) by driving the
  same `provision.Apply` path. **The root module gains zero new
  dependencies** — the `controller-runtime` / `k8s.io` tree is isolated
  in the nested module. `Status` carries a single `Ready` condition
  (reasons: `Reconciled`, `InvalidSpec`, `SecretMissing`,
  `NATSUnreachable`, `ApplyError`) plus `lastPlan` / `lastApply`
  summaries. Deploy manifests under `k8s/config/`. See
  `docs/KUBERNETES.md`.

### Added — configuration & observability

- **`KVBuckets.Replicas`** — declares desired replica count for
  parti-owned KV buckets; mismatches surface as a startup warning.
- **One-shot startup warnings** when: two-phase handoff is enabled
  without a processing gate; `NatsKV` reconciler is disabled with no
  leadership probe wired; `nats.Conn` has a finite `MaxReconnect`.
- **Election bucket on `FileStorage`** by default — survives JetStream
  restarts in single-node deployments, removing a spurious
  degraded-entry path.

### Fixed

- **`watcher.Stop` race** (`nats.ErrBadSubscription`). A KV watcher
  created with `nats.Context(ctx)` registers an internal nats.go
  goroutine that calls `sub.Unsubscribe` on ctx-cancel, racing callers
  that explicitly call `watcher.Stop`. New
  `natsutil.IsBenignWatcherStopErr` tolerates both
  `ErrConsumerNotFound` (server-side) and `ErrBadSubscription`
  (local-side) at all five `watcher.Stop` callsites.
- **Whole-bucket loss → degraded, not claim-lost shutdown.** Restored
  the contract that all workers enter `StateDegraded` within a bounded
  window on whole-bucket loss; a prior classifier widening had begun
  routing the error to claim-lost self-stop on a single worker.
- **Epoch-probe race on shared stream** — manager now opens a dedicated
  KV handle for the epoch probe, eliminating a race against the
  production watcher on the same cached `*stream`.
- **Assignment-watcher envelope per-episode budget reset** — the
  envelope now resets its attempt counter at the start of each restart
  episode rather than carrying state across episodes.
- **Stable-ID bucket-missing renewal errors classify as `ErrClaimLost`**
  so the worker self-stops cleanly rather than looping.
- **`partictl`** — config load is now inside the operation timeout
  (previously could hang past the user-specified deadline).

### Internal

- **`make pre-pr`** target chains lint + `make test` (unit, `-race`) +
  `make test-integration` (live NATS, `-race`). Required before opening
  PRs that touch `manager/`, `source/`, `stableid/`, `recovery/`,
  `internal/assignment/`, or `internal/durable/`.
- **Monitor-goroutine concurrency stress-test discipline.** New focused
  tests pin races between monitor goroutines (envelope retry loops,
  reconcilers, watchers) and production paths sharing nats.go's cached
  `*stream` state. Template:
  `test/integration/manager/epoch_monitor_concurrency_test.go`.

### Follow-up issues (non-blocking)

- **Apply ctx threading.** `handoffCoordinator.Apply` accepts `m.ctx`
  unbounded per attempt; per-attempt deadline threading would let the
  background runner enforce bounds. Watchdog already covers the failure
  case.
- **Stress-soak test for the WaitingAssignment → Stable window.** Add a
  sibling to `epoch_monitor_concurrency_test.go` for the lifetime
  runner.
- **Deterministic CAS-clobber regression pin.** Add a production test
  hook (`m.testHookBeforeStartupCAS`) for a live-cluster pin; unit
  tests in `manager_startup_async_cas_test.go` cover the guard in
  isolation.
- **Orphan-claim reaper.** With no `MaxAge` on the handoff bucket,
  claims for partitions permanently removed from the source no longer
  expire. Harmless slow leak; add a reaper only if partition-set churn
  makes it material.

## [v2.4.1] - 2026-05-20

This patch release fixes two-phase handoff ownership claims silently
disappearing under pull gating — both an identity-keying mismatch for
multi-key partitions and a KV bucket TTL that expired stable claims —
which could permanently suppress consumer delivery. It also corrects
`Manager.State()` / `OnStateChanged` reporting for partition-lifecycle
rebalances.

### Fixed

- Two-phase handoff ownership claims are now keyed by the partition's dot-joined
  subject identity (`Partition.SubjectKey()`), matching the identity the
  consumer's pull gating and processing gate resolve ownership by. Previously
  claims were keyed by the dash-joined `Partition.ID()`, so for any partition
  with more than one key the consumer's claim resolver could not find the claim
  and pull gating permanently suppressed delivery (`pull gating resolve failed:
  partition not found`). Single-key partitions were unaffected.
- The two-phase handoff KV bucket is no longer created with a `MaxAge` TTL.
  Previously the bucket inherited `KVBuckets.HandoffTTL` (default `2m`) as its
  `MaxAge`, so the stable ownership claims that two-phase handoff writes once and
  never refreshes aged out of the bucket. A pull-gated consumer's claim resolver
  then lost every claim and permanently suppressed delivery
  (`pull gating resolve failed: partition not found`). Stable claims now persist;
  `HandoffTTL` is the coordinator's advisory sweep TTL for stuck in-flight
  handoffs only. A handoff bucket created by an older parti version is healed
  automatically on Manager start — its `MaxAge` is cleared — or, if the NATS user
  lacks stream-update permission, `Manager.Start` fails loudly with remediation
  guidance instead of continuing into a delayed silent outage.
- `Manager.State()` and `OnStateChanged` now correctly reflect partition-lifecycle
  rebalances (previously, partition-source changes ran a rebalance without
  entering `StateRebalancing`). A low-frequency reconcile in
  `monitorCalculatorState` also ensures the manager's projected state recovers
  within ~1 s of a dropped calculator state-machine subscriber event. The
  reconcile path guarantees eventual projection of the current calculator state,
  not replay of every missed transient transition.

### Documentation

- Glossary entry for `Rebalancing` broadened to cover partition-source changes
  in addition to worker-count changes.

## [v2.4.0] - 2026-05-18

This release delivers partition-assignment robustness across six phases of
work: a content-addressable assignment publisher, a commit-driven worker state
machine with leader-side audit/repair, a v1 heartbeat wire format with
capability advertising, atomic partition-list mutation APIs, live consumer-side
processing-gate wiring, and several targeted fixes for latent failure modes
that became materially more likely once audit-repair could trigger escalation.
It also adds two opt-in consumer options that cut per-partition JetStream IOPS
by ~99% on the IOPS-investigation rig, and ships a full investigation report
documenting the storage/replication tradeoffs behind that recommendation.

### Added

- **`consumer.WithConsumerMemoryStorage(bool)`** and
  **`consumer.WithConsumerReplicas(int)`** — universal options that forward to
  `jetstream.ConsumerConfig.MemoryStorage` and `.Replicas`. Combined
  (`MemoryStorage=true`, `Replicas=1`) they reduce per-partition
  `block_write_iops` by ~99% on the IOPS-investigation rig; defaults preserve
  existing behavior. Validation is pass-through (NATS rejects invalid replica
  counts at consumer create time). See
  `docs/plans/iops-investigation/findings.md` §2 for the recommendation and §4
  for the operator decision tree.
- **`source.NatsKV.Modify(ctx, fn)`** — CAS-retried transform for the partition
  list. `fn` receives a fresh KV snapshot on every attempt (never the local
  cache) and must be side-effect-free. Returns `source.ErrUpdateRetryExhausted`
  if the retry budget is exhausted. All concurrent callers are safe.
- **`source.NatsKV.AddPartitions(ctx, partitions...)`** — adds partitions
  without disturbing concurrent mutations; duplicates (matched by `CanonicalID`)
  are silently ignored.
- **`source.NatsKV.RemovePartitions(ctx, partitions...)`** — removes partitions
  by `CanonicalID`; missing partitions are silently ignored.
- **`types.RevisionedPartitionSource`** optional interface — sources that track
  a KV revision (e.g., `source.NatsKV`) implement `Snapshot(ctx) (partitions,
  revision, known, error)`. The calculator uses this to include
  `AppliedSourceRevision` in apply receipts; the leader audit uses `known` to
  gate strict source-revision checks.
- **`types.Partition.CanonicalID()`** — length-prefixed, collision-safe encoding
  of the partition's key tuple (`"<len>:<key>/<len>:<key>/..."`). Suitable as a
  stable map key or for set-equality checks without ambiguity on special chars.
- **`types.Heartbeat` v1 wire format** — workers now publish JSON heartbeats with
  `SchemaVersion`, `Capabilities`, `AppliedVersion`, `AppliedDigest`,
  `AppliedSourceRevision`, and `AppliedSourceRevKnown` in addition to
  `WorkerID` and `Timestamp`. Legacy RFC3339 timestamp strings are still decoded
  as `SchemaVersion=0` with zero capabilities.
- **`types.DecodeHeartbeat(b []byte)`** — decodes both v1 JSON and legacy
  timestamp heartbeats. Malformed payloads return an error; silent degradation
  to an empty struct is intentionally rejected.
- **`types.CapAckV1 / CapTwoPhaseHandoff / CapProcessingGate`** capability bit
  constants. A bit is set only when the corresponding mechanism is actually wired
  at runtime, not merely configured. The leader's audit reads peer capability
  bitmasks from heartbeats to decide whether `audit_repair` escalation is safe.
- **`parti.CapabilityReporter` interface** — optional extension for
  `WorkerConsumerUpdater` implementations. If the registered updater satisfies
  this interface, the Manager ORs reported capability bits into its bitmask after
  each handoff apply. Must be concurrent-safe, non-blocking, and monotonic for
  runtime-wire-up bits.
- **`Manager.SetCapability(capBit, active)`** and **`Manager.Capabilities()`** —
  atomic capability bitmask accessors. `SetCapability` is OR-only for
  reporter-sourced bits; the heartbeat publisher reads `Capabilities()` on every
  publish to embed the live state.
- **`consumer.ResolverConfig.ReconcileInterval`** — cadence at which the
  auto-created claim-based resolver reconciles its cache against KV. Defaults to
  30s; choose a value shorter than `5 × HeartbeatTTL`. Zero uses the default;
  negative values are rejected at startup.
- **One-shot `WARN` at `Manager.Start`** when `EnableTwoPhaseHandoff=true` and
  `5 × HeartbeatTTL < 30s`, reminding operators to lower
  `ResolverConfig.ReconcileInterval` proportionally so the resolver cannot stay
  stale longer than the audit grace period.

### Changed

- **Refs-always commit publisher.** Assignments are now stored as
  three protocol keys instead of one per-worker key:
  - `assignment._commit` — the current commit object (worker→payload-key map)
  - `assignment._commit_log.<V>` — an append-only commit-log entry per version
  - `assignment._payload.<hex(sha256)>` — content-addressable payload blobs
  Workers watch `assignment._commit` and fetch referenced payload keys.
  Content-addressable payloads mean identical assignment slices share a single
  KV entry; a GC pass (`CommitGC`) reaps orphan payload keys.
- **Commit-driven worker state machine + leader audit.** The leader
  now records the active fleet and commit version in `assignment._commit` and
  re-reads it after every apply to verify symmetry between the committed worker
  set and the live heartbeat fleet. Workers that fall behind (applied version
  stale relative to the current commit) may be escalated to `audit_repair`
  reassignment if they advertise `CapProcessingGate`.

### Fixed

- **Live consumer-side wiring of `CapProcessingGate`** (commit `47e7665`).
  The `Dynamic` consumer now implements `CapabilityReporter`; it sets
  `CapProcessingGate` after the first successful per-partition gate wrap. The
  Manager samples this after every `Apply`. Before this fix, no production code
  advertised `CapProcessingGate`, making the `audit_repair` escalation path
  unreachable.
- **Watcher drift detection and active recovery in claim resolver** (commits
  `963afb6`, `5bc46cc`). Silent watcher stalls are now detected via a
  `max(2 × ReconcileInterval, 60s)` deadline and recovered with a watcher
  restart. Observable via `IncReconcileRescue` / `IncWatcherRestart("drift_detected")`
  metrics.
- **`twophase.preparePhase` recovery of stuck-prepare claims** (commit
  `0bbd124`). A latent bug (present since v2.3.0) left claims in
  `(owner=A, pending=B, state=prepare)` permanently when the new leader
  re-acquired before the prepare phase completed. On re-acquire the prepare
  phase now detects and recovers these claims.

### Rolling upgrade from v2.3.0

A rolling upgrade from v2.3.0 to this release is supported — no flag-day
restart is required. The new publisher continues to write the legacy
`assignment.<W>` aliases alongside the new three-key commit format, and the
new worker runs a dual-read source-of-truth selector (`selectAuthority` in
`manager_select_authority.go`) that picks between commit and legacy alias by
`LeaderRevision`. In a mixed-version cluster:

- A v2.3.0 worker reads the legacy alias and ignores the new commit keys.
- A new-version worker reads whichever channel carries the higher
  `LeaderRevision`, so it converges with the v2.3.0 leader during the
  rollout window.

**Operational notes for the upgrade:**

- **Configure JetStream retention on the assignment bucket** before
  upgrading. The new publisher writes one `assignment._commit_log.<V>`
  entry per commit. Without a stream retention policy this grows
  unboundedly; the GC pass reaps payload keys but does not prune the
  commit log.
- **Downgrade is not supported.** Rolling back from this release to v2.3.0
  is untested; an old leader cannot read `assignment._commit` and the
  cluster would fall back to legacy aliases. If you must downgrade, do a
  flag-day restart.

### Deprecated

- **Legacy `assignment.<W>` alias write + dual-read path.** Retained for
  the entire v2.x line to keep v2.3.0 → v2.x rolling upgrades safe.
  Scheduled for removal in v3.0, at which point the publisher will stop
  writing legacy aliases and the dual-read selector
  (`manager_select_authority.go`) and alias barrier
  (`internal/assignment/assignment_publisher.go` step 6 of §3.5) will be
  deleted. Operators must complete the v2.3.0 → v2.x rollout before
  upgrading to v3.0.

### Internal

- Dead `reconcileIntervalSet` field removed from `internal/durable/`; `// P0
  fix:` / `// P1 fix:` debt labels replaced with plain explanatory comments
  (commit `a585801`).
- `internal/assignment/doc.go` updated to reflect the three-key commit-publisher
  model; references to the old single per-worker assignment key removed.
- `Makefile` `TEST_DIRS` / `ALL_GO_FILES` now exclude `./.claude/*` so agent
  worktrees under `.claude/worktrees/` no longer get swept into the test set
  (commit `f24ab2e`).

## [v2.3.0] - 2026-04-22

This release closes a silent-drift failure mode around live NATS JetStream
data loss. Before v2.3.0, if a running Parti cluster lost its KV buckets
without an app-side restart (single-node JetStream with ephemeral storage,
accidental `nats kv rm`, peer promotion with empty state), every worker
reported `Stable` and `CurrentAssignment()` kept returning pre-incident
data while no leader was publishing fresh updates. v2.3.0 routes that
scenario into the existing degraded-mode circuit so it becomes observable
through `State() == Degraded`, the `OnDegraded` hook, and the existing
alert metrics — turning the silent drift into a trigger for the
restart-based recovery that was already correct and tested.

The recovery action itself did not change: restart the pod. What changed
is that silent-drift is now a loud signal instead of a hang.

### Fixed — silent drift on live NATS bucket loss

Every Parti subsystem that writes to or watches a KV bucket now feeds
errors into the existing `recordKVError` circuit — previously only the
`recordKVError` plumbing existed but no call sites were wired, so
`ErrBucketNotFound` / `ErrStreamNotFound` surfaced as silent log lines
that never incremented the error window.

- **Heartbeat publisher** (`heartbeat.Publisher.publishLoop`). Previously
  swallowed errors with a `_ = err` placeholder; now logs the failure
  and invokes a new `SetOnError` callback.
- **Stable-ID renewal** (`stableid.Claimer.renewalLoop`). Same treatment;
  renewal failures against a wiped StableID bucket now feed the circuit.
- **Leader election** (`manager_election.go`). Renew-failure and
  request-failure paths now call `recordKVError` in addition to logging.
- **Assignment watcher** (`manager_assignment.go`). Watcher re-establish
  failures against a wiped assignment bucket now feed the circuit.

Under the default `KVErrorThreshold` (5 errors in 30s) and a 500ms
heartbeat interval, every worker enters `Degraded` within ~2–3 seconds
of the wipe in integration tests.

### Changed — recordKVError short-circuits while already Degraded

To avoid log-spam and unbounded growth of the `kvErrorWindow` slice
during a sustained live-wipe incident (every subsystem retries
indefinitely against the same failure), `recordKVError` now returns
early when `degradedSince != 0`. The circuit still re-arms on recovery:
`exitDegraded` clears `degradedSince` atomically, so a subsequent wave
of errors will register correctly.

No observable behavior change for the common case — the existing
Degraded state transition is the signal operators act on; additional
warnings past the first were not actionable.

### Added

- **`natsutil.IsDegradingJetStreamError`** — predicate covering
  `ErrBucketNotFound` / `ErrStreamNotFound` / `ErrConsumerNotFound`.
  Distinct from `IsConnectivityError`: the NATS connection is up but
  JetStream state is missing. Handles the double-`%w` wrapping used by
  `election.RenewLeadership`.
- **`heartbeat.Publisher.SetOnError(func(error))`** and
  **`stableid.Claimer.SetOnError(func(error))`** — opt-in error-sink
  callbacks. Backed by `atomic.Pointer` so the hot path reads without a
  lock. Manager wires both to `recordKVError`.
- **`TestManager_LiveNATSBucketLoss`** — hardened from a diagnostic test
  into a contract test: all workers must enter `Degraded` within 20s of
  the wipe, and buckets must not be auto-recreated (guards against a
  future regression toward in-process self-healing).
- **`TestManager_LiveNATSBucketLoss_OnDegradedHook`** — asserts the
  `OnDegraded` hook fires once per worker when buckets are wiped live.
- **`TestManager_Restart_AfterNATSBucketLoss`** — locks the restart-path
  recovery contract (buckets are recreated via `ensureKVBucket` on the
  next `Manager.Start`). Recovery completes in ~14s in the test.
- **`examples/degraded-readiness/`** — runnable example wiring
  `OnDegraded` to an HTTP `/readyz` endpoint so a Kubernetes readiness
  probe rotates pods that enter `Degraded`.
- **`docs/OPERATIONS.md` — "Live NATS Data Loss"** — operator runbook
  covering symptoms, causes, resolution (`kubectl rollout restart`),
  prevention (R≥3 JetStream, persistent storage), and expected log
  noise during an incident.

### Internal

- **`golangci-lint` upgraded 2.5.0 → 2.11.4.** Triaged 48 new
  diagnostics; kept new rules with value (`QF1012`, `use-slices-sort`,
  `prealloc`, `gosec G118`), disabled rules that don't fit this repo
  (`gosec G706` for the simulation binary, `revive package-naming` for
  the public `types/` package), and removed 6 stale `//nolint`
  directives whose underlying checks no longer fire.
- **`go fix ./...`** applied across the repo to pick up Go 1.22+
  idioms (`max`/`min`, `range N`, `any` alias). 82 files, no
  behavioral change.
- **`.golangci.yaml` reorganized** by linter category with per-rule
  rationale comments.
- **New agent rule `800-modernize-after-write.md`** — scopes `go fix`
  to touched packages, avoids repo-wide sweeps inside feature commits.

### Migration

No API breaks; no config changes required. Existing `Hooks.OnDegraded`
implementations start receiving live-wipe events automatically on
upgrade — review your hook for any assumptions about the `reason`
string (the new call path fires with `"KV error threshold exceeded"`,
the same reason used by the existing connectivity-loss path).

If you have a Kubernetes readiness probe, `examples/degraded-readiness/`
shows the recommended wiring so pods are rotated automatically when
live-wipe causes Degraded entry.

## [v2.2.0] - 2026-04-21

This release eliminates several silent-hang paths in `Manager.Start` during
leader takeover and addresses the root cause that kept users reaching for
tight KV `MaxValueSize` limits (PVC IOPS pressure on file-backed
JetStream). The hang fixes are the primary story; the default-timing and
storage-type changes are the root-cause mitigation that lets the hang not
happen again for the common reason.

### Fixed — Manager.Start hang on leader takeover

Three independent paths could leave a pod in `waitForAssignment` polling
forever (until `StartupTimeout` fired as `context deadline exceeded`,
which operators experienced as a hang with no actionable error):

- **Calculator-failure lease retention.** If `startCalculator` failed on
  takeover (e.g., a per-worker assignment JSON exceeded the KV bucket's
  `MaxValueSize`), the pre-fix code only logged the error while the new
  leader kept renewing its election lease with no calculator running —
  no assignment was ever published, and followers joining afterward saw
  no key for their worker ID and polled indefinitely. The new leader now
  releases leadership after calculator failure, which lets other pods
  attempt (and fail loudly the same way, making the publish-size error
  visible cluster-wide) or triggers continued election cycling. Also
  adds an `TestKVSizeLimit_LeaderReleasesLeadershipOnFailure`
  integration test that locks the true→false leadership transition.
- **Stale assignment keys after leader death.** A new leader's first
  rebalance left the previous leader's assignment keys in the bucket;
  joining followers could observe a stale assignment belonging to a
  dead worker. Stale keys are now swept on leader takeover.
- **Overly-long stabilization window on takeover.** The takeover path
  previously waited `ColdStartWindow` (default 30s) before publishing
  the post-takeover assignment, which frequently exceeded a replacement
  pod's start timeout. Takeover now uses `PlannedScaleWindow` (default
  10s) instead — matching the semantics of a rolling change rather than
  a cold-start fleet.

### Changed — default timings and KV storage (behavior at defaults)

The calculator-failure hang above was traced back to users setting a
tight `MaxValueSize` on the assignment bucket to control PVC IOPS (e.g.,
NetApp with IOPS quotas). With 3-way replication, per-message fsync, and
heartbeat/election writes at the previous 2-second cadence, a modest
cluster could push hundreds of IOPS into the NATS stream store and
force operators into that workaround. The defaults now aim for a
low-IOPS steady state that makes tight `MaxValueSize` unnecessary in
the first place:

- `HeartbeatInterval`: `2s` → `5s`
- `HeartbeatTTL`: `6s` → `15s`
- `WorkerIDTTL`: `30s` → `75s` (5× `HeartbeatTTL` per existing guidance)
- `ElectionTimeout`: `5s` → `10s`
- `StartupTimeout`: `30s` → `60s` (keeps headroom over
  `ColdStartWindow + ElectionTimeout + 5s`)
- Heartbeat and election KV buckets are now created with `MemoryStorage`
  on fresh deployments. Their data is intrinsically ephemeral
  (heartbeats re-publish every `HeartbeatInterval`; a lost leader key
  simply triggers re-election), so file-backed storage was paying
  replication + fsync cost for no durability benefit.
- Stable-ID, assignment, and handoff buckets continue to use
  `FileStorage` — stable IDs must survive NATS restart to preserve
  worker identity, assignments must remain visible to followers joining
  during an outage, and handoff claims protect two-phase ownership
  transfers.

Net effect for a typical cluster (5 workers, 3-replica JetStream):
steady-state IOPS from parti drops from ~500 to ~30–50, at the cost of
~2.5× slower failover detection (dead-pod detection shifts from ~3–6s
to ~15–30s).

Existing KV buckets are opened as-is — parti does not auto-delete them.
If a pre-existing bucket's storage type differs from the new default,
parti logs a `Warn` on startup with the exact `nats kv del <bucket>`
command for operator-driven migration during a maintenance window.

### Added

- `TestKVSizeLimit_AssignmentPublishFails` — reproduces the
  `MaxValueSize` cold-start publish-failure surface so the clear-error
  path doesn't regress into a silent hang.
- `TestKVSizeLimit_LeaderReleasesLeadershipOnFailure` — locks the
  takeover-time contract that the new leader must release leadership
  when `startCalculator` fails.
- `TestRestart_LeaderTakeoverRace_ProductionTimings` — production-scale
  timing regression for the takeover race (gated behind
  `PARTI_SLOW_TESTS=1`).
- Startup lifecycle idempotency tests, failed-start cleanup tests,
  empty partition source tests, and stale-assignment sweep tests.

### Migration

If your workload needs the prior failover latency (e.g., you have
external SLOs on takeover time and don't have PVC IOPS pressure),
restore the old timings at `DefaultConfig()`:

```go
cfg := parti.DefaultConfig()
cfg.HeartbeatInterval = 2 * time.Second
cfg.HeartbeatTTL      = 6 * time.Second
cfg.WorkerIDTTL       = 30 * time.Second
cfg.ElectionTimeout   = 5 * time.Second
```

To pick up the IOPS drop on an existing deployment, delete the
heartbeat and election buckets during a maintenance window and let
parti recreate them on the next pod start:

```bash
nats kv del parti-heartbeat
nats kv del parti-election
# then roll the deployment
```

Watch for a `Warn` log line beginning with
`KV bucket storage type differs from parti's default` — that's parti
telling you which buckets still need the manual migration step.
