# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
