# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Worker-set shrink-confirmation defense.** Calculator rebalances now
  guard against silently-truncated heartbeat-bucket scans. NATS'
  `KeyValue.Keys()` can return `(partial-slice, nil)` when the underlying
  watcher subscription tears down mid-scan, producing a fresh-looking
  observation that the worker set has shrunk when in fact the workers are
  alive. Acting on such an observation reassigns live workers' partitions
  to the survivors, producing transient double ownership. The defense
  composes two layers: (1) `Calculator.getActiveWorkers` treats a
  sharply-shrunk scan as suspicious and surfaces the cached worker set
  with `fresh=false` until a configurable confirmation window has elapsed
  — the last-known worker baseline is never advanced on a suspicious
  read; and (2) `Calculator.rebalance` enforces an
  emergency-confirmation gate: even after the confirmation window
  accepts a shrunk read, the rebalance is skipped (no commit) unless
  `EmergencyDetector` has captured at least one confirmed death. The
  skip surfaces as a benign no-op in the state-machine callbacks, not
  an error. Two new `Config` fields tune the defense:
  `WorkerShrinkConfirmationCount` (default `2` — the number of
  consecutive suspicious scans before the defense accepts the shrink)
  and `WorkerShrinkConfirmationThresholdPct` (default `50` — an
  observed count below `lastKnown × Pct / 100` is suspicious). Mirrors
  the existing `PartitionShrinkConfirmation*` doctrine for the
  partition-source side.
- **Kubernetes operator** — a new nested Go module at `k8s/`
  (`github.com/arloliu/parti/v2/k8s`) that reconciles a `ProvisionedPartiEnv`
  custom resource to NATS infrastructure (control-plane KV buckets,
  partition-source bucket, and application JetStream streams) by driving the
  same `provision.Apply` path the `partictl` CLI uses. The operator adds no
  provisioning logic of its own. The root `github.com/arloliu/parti/v2` module
  gains **zero** new dependencies — the entire `controller-runtime` / `k8s.io`
  dependency tree is isolated in the nested module. See `docs/KUBERNETES.md`
  for the full operator guide.
- **`ProvisionedPartiEnv` CRD** (`apiVersion: parti.io/v1alpha1`,
  `kind: ProvisionedPartiEnv`, short name `ppe`) — a namespaced custom resource
  that declares desired NATS infrastructure. The `Spec` mirrors the
  bucket-and-stream subset of `provision.Config` plus a `nats` connection block
  (NATS server URL and an optional reference to a Kubernetes `Secret` for
  `.creds` / token / NKey auth). The `Status` subresource carries a single
  `Ready` condition with reasons `Reconciled` (success), `InvalidSpec`,
  `SecretMissing`, `NATSUnreachable`, and `ApplyError`, plus `lastPlan` and
  `lastApply` summaries (drift counts, executed/error counts). Deploy manifests
  live under `k8s/config/` (CRD, RBAC, operator Deployment, sample CR and
  Secret).
- `partictl partitions plan` and `partictl partitions apply` manage the
  contents of the partition-source key — the partition table itself —
  separately from the bucket-provisioning commands. `plan` reports the
  record-level diff (added / removed / weight-changed) between the declared
  `partitionSource.partitions` and the live key; `apply` writes the declared
  table with a single compare-and-swap, gating record removals behind
  `-prune`. The `provision` SDK exposes the same surface as `PlanPartitions`
  and `ApplyPartitions`.
- `provision.PartitionSourceConfig.Partitions` declares the desired partition
  table inline in `parti-env.yaml`. The bucket-provisioning commands
  (`plan` / `apply` / `adopt`) ignore the field, so existing config files are
  unaffected.
- `partictl stream view|plan|apply` provides a stream-scoped surface over the
  same provision SDK. `stream view` (with or without `-f`) is an
  instance-scoped inventory that lists every Parti-marked application stream in
  the account; `-f` is optional for `view` and required for `plan` / `apply`.
  `stream plan` and `stream apply` accept `-policy`, `-fail-on-drift`, and
  `-dry-run` identically to the top-level commands and emit the same JSON
  envelope (`apiVersion: parti.io/provision/v1`). The existing `partictl plan`
  / `apply` / `adopt` / `view` commands provision and report streams
  automatically when the config has a `streams:` block; no config change is
  needed for configs that omit `streams:`. New action kinds: `create-stream`,
  `update-stream`, `stamp-stream-marker`. New drift kind: `application-stream`.
- `provision.Config.Streams []StreamCfg` declares application JetStream streams
  inline in `parti-env.yaml` under a `streams:` block. `StreamCfg` exposes the
  common operational knobs (`name`, `subjects`, `retention`, `storage`,
  `discard`, `replicas`, `maxAge`, `maxBytes`, `maxMsgs`, `description`).
  `maxBytes` and `maxMsgs` use `0` for "unlimited" in config (the NATS server
  stores these as -1; `plan` normalises the two representations as equivalent).
  `Storage` and `Retention` divergences classify as `drift-immutable` and are
  never auto-reconciled, including `limits` ↔ `interest` retention changes
  (conservative policy; `force` / delete-recreate is a future phase). Subject
  coverage against `dynamicConsumers:` entries is not validated in this release.
- `partictl consumers plan` and `partictl consumers apply` precreate the
  per-partition durable consumers that a `dynamicConsumers:` target with a
  non-empty `partitionsRef` describes. `plan` reports which consumers are
  missing (`create-consumer` actions + `drift-mutable` findings) and which
  already exist (`informational` findings); `apply` creates the missing ones.
  A target with an empty `partitionsRef` keeps its Phase 1 alignment-check-only
  behavior and is unaffected. The `provision` SDK exposes the same surface as
  `PlanConsumers` and `ApplyConsumers`.
- `DynamicConsumerCfg.PartitionsRef` — setting this field to the
  partition-source bucket name opts a `dynamicConsumers:` target into
  precreation. Must equal `partitionSource.bucket`; validated statically before
  any NATS I/O (`ErrInvalidConfig`, exit 3). Empty means alignment-check only
  (unchanged from Phase 1).
- `provision.ValidateConsumerSet(cfg Config) error` performs static validation
  for consumer precreation: at least one opted-in target, non-empty partition
  set, valid `PartitionsRef`, and valid `StreamName` / `ConsumerPrefix` /
  `SubjectTemplate` on every opted-in target. All errors wrap `ErrInvalidConfig`.
- `provision.ErrConsumerStreamMissing` — returned by `PlanConsumers` when the
  application stream a precreation-opted target names does not exist live. Wraps
  `ErrLiveValidation` (CLI exit 3). Mirrors Phase 3's `ErrPartitionBucketMissing`.
- New `PlannedAction` kind `"create-consumer"` (`ActionCreateConsumer`): emitted
  by `PlanConsumers` for each missing per-partition durable consumer; executed by
  `ApplyConsumers` via `js.CreateConsumer`. A `ErrConsumerExists` create-race
  re-reads the colliding consumer and verifies its identity / immutable fields
  before recording a raced success, so a hand-created consumer squatting the
  deterministic durable name is surfaced as a fail-fast error rather than silently
  accepted.
- **Identity-only / runtime-owns model.** Precreation creates consumers from
  `dynamicbuild.DefaultDynamicDefaults()` — the runtime defaults for the
  NATS-immutable fields (`AckPolicy = AckExplicitPolicy`, `MaxWaiting = 2`,
  `MemoryStorage = false`). The runtime's `CreateOrUpdateConsumer` on worker
  start overwrites the mutable tunables (`AckWait`, `MaxDeliver`, etc.) freely.
  No ownership marker is stamped on consumers (stamping would oscillate against
  the runtime's unconditional overwrite). Consumer tunables are not managed by
  provision; `consumer.Dynamic` options remain the sole source of truth for them.
- **`force` reconcile policy** — `provision.PolicyForce` (`"force"`, accepted by
  `partictl plan`, `apply`, `stream plan/apply`, and `consumers plan/apply`). A
  strict superset of `safe-update`: it create-misses and reconciles drift-mutable
  fields in place as `safe-update` does, and additionally repairs a
  drift-immutable resource by delete/recreate — but only when the resource's
  config also sets `allowDeleteRecreate: true` (the two-layer gate). Under any
  other policy, or for a resource without `allowDeleteRecreate: true`, immutable
  drift is still reported but never repaired.
- **Per-resource `allowDeleteRecreate` opt-in** — a new boolean field on each of
  the four config structs (`controlPlane`, `partitionSource`, each `streams`
  entry, each `dynamicConsumers` entry). When `true` and the policy is `force`,
  Apply deletes and recreates the resource to repair drift-immutable divergences.
  When omitted (the default, `false`), the resource is never deleted by provision
  regardless of policy.
- **`recreate-kv` / `recreate-stream` / `recreate-consumer` actions** — new
  `PlannedAction` kinds (`ActionRecreateKV`, `ActionRecreateStream`,
  `ActionRecreateConsumer`) emitted by `Plan` / `PlanConsumers` under `force`
  when both gate layers opt in. `Apply` / `ApplyConsumers` execute the
  five-step re-read → re-classify → delete → create sequence. A stale plan is
  handled gracefully: the re-classify step skips the delete when the live state
  no longer carries the immutable divergence the plan recorded. A post-delete
  create failure returns a fail-fast error wrapping `provision.ErrRecreateInterrupted`;
  re-running `apply` after fixing the persistent cause is safe (a fresh plan
  emits an ordinary `create-*` action, no re-deletion).

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
