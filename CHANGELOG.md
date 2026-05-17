# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
