# Simulation — Phase 2: Consumer Package Migration

Single-PR plan to migrate `test/simulation/` off the `internal/durable` package
and onto the public `consumer/` package. This is a follow-up to Phase 1
(merged in the prior commits on this branch); it addresses the two CLAUDE.md
compliance items called out in the simulation audit (C5 and M10 in
`tmp/sim_audit/SUMMARY.md`, which is generated artifact under `tmp/` and not
checked in — the migration table below is self-contained and authoritative
for this plan).

## Why

CLAUDE.md is explicit: "Internal packages under `internal/` are private
implementation details — do not reference them in public API or docs." The
simulation has two `internal/durable` imports:

- `test/simulation/internal/worker/worker.go:19` — `durable.WorkerConsumerConfig`,
  `durable.ProcessingGateConfig`, `durable.NewWorkerConsumer`.
- `test/simulation/internal/metrics/collector.go:8` — `durable.GateMetrics`,
  `durable.ResolverMetrics`, `durable.AuditResult`.

Both have public equivalents in `consumer/` and `consumer/metrics.go`. Migration
is **purely mechanical** — no behavior change. The migration table below is
authoritative.

Beyond CLAUDE.md compliance, the migration:

- Unblocks Phase 3 (oracle strengthening) and Phase 5 (new public-API features
  via yaml flags) — most of those phases only need to plumb additional
  `consumer.With*` options once the simulation consumes the public package.
- Routes capability reporting through the public type. The current durable
  updater already reports `CapProcessingGate` (`internal/durable/worker_consumer.go:693`,
  `:712`) and the manager already picks that up via the structural
  `CapabilityReporter` interface (`manager.go:774`, `:800`); `*consumer.Dynamic`
  is a thin wrapper that forwards `Capabilities()` (`consumer/dynamic.go:329-334`).
  No new reporting bits appear after migration — same path, public type.

## Out of scope

- Adding new feature coverage (memory-storage, recovery-strategy yaml flags,
  resolver tuning). Those are Phase 5.
- Removing the dead `Late/Lost/Failures` audit-metric read paths in
  `cmd/simulation/main.go:655-714`. They will continue to read zero (since
  `RecordDrainAudit` is being deleted), which preserves current behavior.
  Their removal is a separate cleanup task.
- Any change in the consumer's runtime behavior. Every option currently set
  must continue to be set with the same value.

## The migration

### Worker (`test/simulation/internal/worker/worker.go`)

**Remove:**
- `"github.com/arloliu/parti/v2/internal/durable"` import.

**Add:**
- `"github.com/arloliu/parti/v2/consumer"` import.

**Replace** the existing `durable.WorkerConsumerConfig{...}` struct literal +
`durable.NewWorkerConsumer(js, helperConfig, worker.processMessage)` call
(`worker.go:236-322`) with `consumer.NewDynamic(...)` using functional options.
Mapping (every field currently set in the worker; this table is the
authoritative reference for the migration):

| Old (durable.WorkerConsumerConfig field) | New |
|---|---|
| `ConsumerPrefix:  "simulation"` | positional arg #3 of `NewDynamic` |
| `SubjectTemplate: "simulation.partition.{{.PartitionID}}"` | positional arg #4 |
| `StreamName:      "SIMULATION"` | positional arg #2 |
| `Logger:          logger` | `consumer.WithLogger(logger)` |
| `ManualAck:       true` | `consumer.WithManualAck(true)` |
| `BatchSize:       cfg.ConsumerBatchSize` | `consumer.WithBatchSize(cfg.ConsumerBatchSize)` |
| `FetchTimeout:    1 * time.Second` | `consumer.WithFetchTimeout(1*time.Second)` |
| `DrainOnRemove:   true` | `consumer.WithDrainOnRemove(true, 0)` (timeout=0 → durable defaults) |
| `MaxDeliver:      50` | `consumer.WithMaxDeliver(50)` |
| `AckWait:         cfg.AckWait` | `consumer.WithAckWait(cfg.AckWait)` |
| `MaxWaiting:      2` | `consumer.WithMaxWaiting(2)` |
| `MaxAckPending:   perSubMaxAckPending` | `consumer.WithMaxAckPending(perSubMaxAckPending)` |
| `PullGatingEnabled: true` | `consumer.WithPullGating(true)` |
| `ProcessingGate: pg` (only when `EnforceExclusiveConsumption`) | `consumer.WithProcessingGate(&consumer.ProcessingGateConfig{...})` |

The handler signature `func(ctx context.Context, msg jetstream.Msg) error`
matches `consumer.MessageHandlerFunc`. Pass
`consumer.MessageHandlerFunc(worker.processMessage)` as the handler argument.

**Processing-gate config:** copy field-for-field from `durable.ProcessingGateConfig`
to `consumer.ProcessingGateConfig`. Identical field set (Enabled, AllowedStates,
WarmupDuration, WarmupAllowedStates, NakDelay, NakJitter, Debug, Metrics).
`Metrics` field type changes from `durable.GateMetrics` to `consumer.GateMetrics` —
the collector adapter return type change (below) lines this up.

**Updater wiring:**
- Worker's `updater parti.WorkerConsumerUpdater` field stays.
- Add a typed `dynamic *consumer.Dynamic` field for the Stop call (since the
  `parti.WorkerConsumerUpdater` interface doesn't expose Stop/Close).
- Assign both: `dynamic := consumer.NewDynamic(...); worker.updater = dynamic; worker.dynamic = dynamic`.

**Resolver metrics:**
- `updater.SetResolverMetrics(...)` becomes `dynamic.SetResolverMetrics(...)`.
- Adapter return type is now `consumer.ResolverMetrics` (see Collector change).

**Shutdown:**
- The current code at `worker.go:669-671` does a type assertion:
  ```go
  if c, ok := w.updater.(interface{ Close(context.Context) error }); ok {
      _ = c.Close(context.Background())
  }
  ```
  `*consumer.Dynamic` exposes `Stop(ctx) error` instead of `Close`. Replace the
  type assertion with a direct `if w.dynamic != nil { _ = w.dynamic.Stop(ctx) }`.

### Metrics collector (`test/simulation/internal/metrics/collector.go`)

**Remove:**
- `"github.com/arloliu/parti/v2/internal/durable"` import.

**Add:**
- `"github.com/arloliu/parti/v2/consumer"` import.

**Adapter return-type changes** (interfaces are method-set identical; no
implementation changes required):
- `GateMetricsAdapter() durable.GateMetrics` → `consumer.GateMetrics`.
- `ResolverMetricsAdapter() durable.ResolverMetrics` → `consumer.ResolverMetrics`.

**Delete `RecordDrainAudit`** (line 856-898). It has no callers outside its
own definition (`grep -rn RecordDrainAudit test/simulation/` returns only the
definition line). The function takes `[]durable.AuditResult` — a type with
**no public equivalent** in `consumer/` — so keeping it would force us to
keep the internal-package import.

Side effects of deleting `RecordDrainAudit`:
- The metric fields `lateMessagesTotal`, `lostMessagesTotal`,
  `auditDurationSeconds`, `auditFailuresTotal`, `holesOpen` are **kept**:
  they are registered with the Prometheus collector at construction
  (`collector.go:477-505`) and the read methods `GetLateMessagesTotal`,
  `GetLostMessagesTotal`, `GetAuditFailuresTotal` are called from
  `test/simulation/cmd/simulation/main.go:659-661, 707-709`. They will keep
  returning 0 (since nothing increments them now or before). This preserves
  current observable behavior; only the unused write path goes away.
- A future Phase 3 task may remove the dead read paths in main.go and the
  unused counters. Marked out-of-scope for Phase 2 to keep the diff small.

## Risks and verification

**Risk 1: Option mapping drift.** If any option's semantics differ between
`durable.WorkerConsumerConfig.X` and `consumer.WithX(...)`, behavior changes.
Mitigation: the migration table is verbatim from the audit; each row is a
1:1 mechanical replacement. `consumer/dynamic.go:194-227` shows that
`NewDynamic` builds the same `CommonConfig + DynamicConfig` fields, then
forwards them into a `durable.WorkerConsumerConfig` via `toSubscriptionGateConfig`
and `toSubscriptionResolverConfig`. So the public path is a thin wrapper over
the same internal types we're leaving behind.

**Risk 2: ResolverConfig defaults.** Worker currently constructs
`durable.WorkerConsumerConfig` with no explicit `Resolver` field. After
migration, `consumer.NewDynamic` applies a default-zero `consumer.ResolverConfig`
which is translated by `toSubscriptionResolverConfig`. Verify the translated
defaults match what the worker had before. (Empirically: both paths produce
the same default resolver since neither sets a value.)

**Risk 3: Capability reporting.** Not a risk — `*consumer.Dynamic.Capabilities()`
forwards to the same inner `*durable.WorkerConsumer.Capabilities()` that the
manager already observes today through the existing structural-interface match
on the durable updater. The migration preserves the reporting path; nothing
new appears on the heartbeat.

**Risk 4: Test breakage from updater type change.** The leak tests, chaos
scenario tests, and other unit tests use `worker.NewWorker(cfg).updater`
indirectly via worker behavior. The migration changes the concrete type but
the interface (`parti.WorkerConsumerUpdater`) is the same. No test changes
expected; rerun the full suite to confirm.

**Verification gates** (all must pass before declaring complete):
1. `grep -rn "internal/durable" test/simulation/` returns **zero** matches.
2. `go build ./test/simulation/...` clean.
3. `make lint` clean.
4. `make test` clean.
5. `go test ./test/simulation/... ./scripts/...` clean.
6. **Behavioral spot-check**: run `./bin/simulation -config configs/dev.yaml`
   for ~30s and confirm no `panic`, no new errors in the log, and the
   periodic report still prints sane numbers. (Optional if the unit suite
   is comprehensive; document if skipped.)

## Implementation order

Single PR, mechanical. Suggested order:

1. **Collector adapter return types** first (it's the dependency for worker).
   - Change `GateMetricsAdapter`/`ResolverMetricsAdapter` return types.
   - Delete `RecordDrainAudit`.
   - Drop the `internal/durable` import.
   - Build will fail in worker.go until step 2.

2. **Worker migration**:
   - Replace `durable.WorkerConsumerConfig{...}` literal with the `consumer.With*`
     options chain.
   - Replace `durable.NewWorkerConsumer(...)` with `consumer.NewDynamic(...)`.
   - Replace `durable.ProcessingGateConfig` with `consumer.ProcessingGateConfig`.
   - Add typed `dynamic *consumer.Dynamic` field; assign both updater and dynamic.
   - Replace the `interface{ Close }` type assertion in Stop with
     `w.dynamic.Stop(ctx)`.
   - Drop the `internal/durable` import.

3. **Lint + test** — run all five verification gates listed above.

4. **Optional behavioral spot-check** — short dev.yaml run if there's
   ambient doubt about the option mapping.

## Commit message (draft)

```
refactor(simulation): migrate from internal/durable to public consumer package

Removes the two CLAUDE.md-forbidden internal-package imports from
test/simulation/ and routes the worker through the public consumer.NewDynamic
factory. No behavior change — every option that was set on
durable.WorkerConsumerConfig is now set via the equivalent consumer.With*
option per the migration table in docs/plans/sim-oracle-phase2/00-plan.md.

- worker.go: durable.NewWorkerConsumer → consumer.NewDynamic with functional
  options. durable.ProcessingGateConfig → consumer.ProcessingGateConfig.
  Replaces the interface-assertion Close on shutdown with a typed
  *consumer.Dynamic.Stop(ctx) call.

- metrics/collector.go: GateMetricsAdapter and ResolverMetricsAdapter now
  return consumer.{Gate,Resolver}Metrics (interface method sets are
  identical to the durable counterparts). RecordDrainAudit is deleted — it
  had no callers outside its own definition and was the only consumer of
  durable.AuditResult, which has no public twin. The Late/Lost/Failures
  Prometheus counters and their Get*-method read paths remain in place;
  they continue to return 0 as before.

Capability reporting path is preserved: *consumer.Dynamic.Capabilities()
forwards to the same inner *durable.WorkerConsumer that the manager
already reads today, so this is a wrapper substitution, not new
observable behavior.
```
