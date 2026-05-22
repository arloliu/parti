# Provision SDK / partictl — Phase 5: Dynamic Precreate

This plan is **Phase 5** of the
[Phased Roadmap](../provision-sdk-cli/00-implementation-plan.md#phased-roadmap)
in the master plan. Phases 1-2 built the `provision/` SDK and `cmd/partictl`
CLI that provision the control-plane and partition-source KV **buckets**.
Phase 3 added partition-record management; Phase 4 added application-stream
provisioning. Phase 5 adds **optional precreation of the per-partition durable
consumers** that the Parti runtime binds to an application stream — today
`provision` only *alignment-checks* dynamic consumers, never creates them.

## What Ships After Phase 5

- Operators can declare a dynamic-consumer target in `parti-env.yaml` under
  the existing `dynamicConsumers:` block and opt that target into
  **precreation** by setting its `partitionsRef`. Running
  `partictl consumers plan -f parti-env.yaml` shows which per-partition
  durable consumers are missing; `partictl consumers apply -f parti-env.yaml`
  creates them.
- The SDK exposes `provision.PlanConsumers` and `provision.ApplyConsumers` so
  the same plan/apply path is callable as a library, mirroring the Phase 3
  `PlanPartitions` / `ApplyPartitions` pair.
- Precreation is **identity-only**. Of the consumer-identity fields, `Name`,
  `Durable`, and `FilterSubject` are derived deterministically from config,
  and `DeliverPolicy` is the fixed `DeliverAllPolicy` the shared builder
  hard-codes — all four are byte-identical between provision and the runtime
  unconditionally. `AckPolicy`, `MaxWaiting`, and `MemoryStorage` are
  NATS-immutable and precreated from the runtime defaults
  (`AckExplicitPolicy` / `2` / file storage): **Phase 5 precreation supports
  only dynamic consumers that use the default `AckPolicy`, `MaxWaiting`, and
  `MemoryStorage`** (see "The runtime-owns model" and "The immutable-field
  contract" below). Mutable runtime-owned tunables are advisory — the runtime
  overwrites them on start.
- A `dynamicConsumers:` target without a `partitionsRef` keeps its Phase 1
  behavior exactly: alignment-check only, never precreated.

Subsequent phases keep their own surface: destructive repair (Phase 6),
Kubernetes controller (Phase 7).

## Background: The Dynamic-Consumer Data Model

Ground truth from the current codebase (verified at the cited lines):

- **The shared builder already exists.** Phase 1's W4 extracted the
  consumer-construction logic into `internal/dynamicbuild`. Two callers use
  it: the runtime (`internal/durable/worker_consumer.go`, via
  `jsutil.EnsureConsumer`) and the provision SDK
  (`provision/dynamic_consumers.go` `PlanDynamicConsumers`). The package
  docstring states the contract explicitly
  (`internal/dynamicbuild/builder.go:1-19`): "Both callers must agree
  byte-for-byte on identity fields (Name, Durable, FilterSubject, AckPolicy,
  DeliverPolicy). The Defaults struct captures the runtime-tunable fields …
  provision passes runtime defaults, and the runtime passes its configured
  values." **Phase 5 therefore needs no W0 builder extraction** — the builder
  is already shared.
- **`dynamicbuild.ConsumerConfig`** (`internal/dynamicbuild/builder.go:80-95`)
  builds a `jetstream.ConsumerConfig`. `Name`, `Durable`, `FilterSubject`,
  and the hard-coded `DeliverPolicy` are deterministic from `(durable,
  subject)`; the remaining fields come from a `Defaults` struct. The
  `Defaults` struct carries seven fields, but they are **not** all
  runtime-tunable: `AckPolicy`, `MaxWaiting`, and `ConsumerMemoryStorage` are
  NATS-immutable and governed by the immutable-field contract below (Phase 5
  precreates them from the runtime defaults); only `AckWait`, `MaxDeliver`,
  `InactiveThreshold`, `MaxAckPending`, and `ConsumerReplicas` are genuinely
  runtime-owned mutable tunables.
- **`provision.PlanDynamicConsumers`** (`provision/dynamic_consumers.go:49-106`)
  is a pure builder: given `(streamName, consumerPrefix, subjectTemplate,
  partitions)` it returns a deterministic `[]PlannedConsumer`. It performs no
  I/O and is **not** wired into `Plan` (`provision/plan.go` leaves the
  `DynamicConsumers` slot intentionally empty). Before Phase 5 it passed
  `dynamicbuild.Defaults{AckPolicy: jetstream.AckExplicitPolicy}` — every
  other tunable left at the Go zero value; **W1 changes it to build from
  `dynamicbuild.DefaultDynamicDefaults()`** so the NATS-immutable fields match
  the runtime (see "The immutable-field contract").
- **`PlannedConsumer`** (`provision/types.go`) carries `StreamName`,
  `Subject`, `Durable`, and `Config jetstream.ConsumerConfig`.
- **Durable naming** (`internal/dynamicbuild/builder.go:97-124`):
  `PerSubjectDurableName(consumerPrefix, subject, partitionPrefix,
  partitionSuffix)` returns `<consumerPrefix>_<sanitizedPartitionID>_<hash>`,
  where `<hash>` is the 16-hex `xxh3` of the **subject**. The name is fully
  deterministic and collision-resistant for ordinary partition identifiers —
  the 64-bit subject-hash suffix makes a two-subject name collision
  vanishingly unlikely, though `xxh3` is non-cryptographic and the partition-id
  portion is sanitized and truncated, so the name is **not** a proof of
  subject identity. Apply must still verify a live consumer's identity fields
  on any create-race (see W3); the deterministic name is the *locator*, not
  the *correctness boundary*.
- **Runtime consumer creation** goes through `jsutil.EnsureConsumer`
  (`jsutil/consumer.go`), which calls `js.CreateOrUpdateConsumer` with retry.
  The runtime calls this **every time a worker starts a dynamic consumer** —
  precreation does not remove that call.
- **`DynamicConsumerCfg`** (`provision/config.go`) has `StreamName`,
  `ConsumerPrefix`, `SubjectTemplate`, and `PartitionsRef` (currently
  **unused**).
- **nats.go consumer API** (`nats.go@v1.50.0/jetstream`): `js.Consumer(ctx,
  stream, name)` looks one up, returning `ErrConsumerNotFound` if absent.
  `js.CreateConsumer(ctx, stream, cfg)` creates one; if a consumer with that
  name already exists **and its config differs**, it returns
  `ErrConsumerExists`; if it exists with an identical config it succeeds.
  `FilterSubject` is immutable on an existing consumer.

### The runtime-owns model (the central Phase 5 decision)

The runtime calls `CreateOrUpdateConsumer` on every worker start, and NATS
**overwrites** a consumer's config on update (it does not merge). This forces
a single decision: **does provision or the runtime own a dynamic consumer's
config?** Phase 5 chooses **the runtime owns it.** Consequences, all
deliberate:

- **Provision precreates from the runtime defaults.** `ApplyConsumers`
  creates each consumer with what `dynamicbuild.ConsumerConfig` produces from
  `dynamicbuild.DefaultDynamicDefaults()`: the identity / immutable fields
  (`Name`, `Durable`, `FilterSubject`, `AckPolicy`, `DeliverPolicy`,
  `MaxWaiting`, `MemoryStorage` — see "The immutable-field contract") carry
  the runtime-default values, and the mutable tunables carry the runtime
  defaults too. When the runtime later starts and calls
  `CreateOrUpdateConsumer` with its own configured tunables, it updates the
  consumer's *mutable* tunables to the runtime's
  values — by design. Precreation makes the consumer **exist** ahead of the
  workers; the runtime still owns its mutable tuning.
- **No ownership marker on consumers.** Phases 1-4 stamp the Parti marker in
  a resource's `Metadata`. A consumer marked by `provision` would have its
  `Metadata` **stripped** the first time the runtime's
  `CreateOrUpdateConsumer` ran without setting `Metadata` — producing an
  endless re-stamp loop (`apply` stamps → worker restart strips → `plan`
  reports drift → `apply` stamps …). Phase 5 therefore stamps **no marker on
  consumers at all.** `provision` locates a Parti consumer the only way that
  is stable across runtime overwrites: by its **deterministic durable name**,
  recomputed from config via `PerSubjectDurableName`.
- **Phase 5 does not extend `DynamicConsumerCfg` with consumer tunables.**
  The source of truth for `AckWait`, `MaxDeliver`, etc. is the application's
  `consumer.Dynamic` options, not a provisioning YAML. Mirroring them into
  `DynamicConsumerCfg` (as `ControlPlaneConfig` mirrors `parti.Config`) would
  create a *second* place they can drift. The `ControlPlaneConfig` mirror
  works because provision and the runtime both provision the *same*
  infrastructure; a consumer's tuning is owned by one side only.
- **Precreation is not a least-privilege enabler.** Because the runtime still
  calls `CreateOrUpdateConsumer` unconditionally, precreation does **not** let
  a runtime run without consumer-write permission. Phase 5's value is
  **pre-flight readiness** (the consumers exist and are inspectable before
  workers start) and **drift visibility** (`plan` reports missing consumers).
  A true least-privilege path would require a runtime change and is out of
  scope.

### The immutable-field contract — precreate with the runtime defaults

NATS rejects a `CreateOrUpdateConsumer` that changes certain consumer fields
on an existing consumer. From the NATS server's update-rejection list
(`nats-server v2.12.6 server/consumer.go:2272-2313`), the ones reachable on a
Parti dynamic (pull) consumer are: **`DeliverPolicy`**, **`AckPolicy`**, and
**`MaxWaiting`** ("max waiting can not be updated", `consumer.go:2312-2313`).
**`MemoryStorage`** is also immutable at the NATS-API level — changing the
consumer storage type requires delete/recreate (`consumer/options.go:452`
documents the `WithConsumerMemoryStorage` option as not live-editable).

These fields **cannot be "owned by the runtime and overwritten later"** — if
provision precreates a consumer whose immutable fields differ from the
runtime's, the runtime's own `CreateOrUpdateConsumer` on worker start *fails*.
So provision must precreate the immutable fields at exactly the runtime's
values. The contract:

- **Provision precreates from the runtime defaults.** The shared
  `internal/dynamicbuild` package exposes `DefaultDynamicDefaults() Defaults`
  — the `Defaults` a default-configured `consumer.Dynamic` uses, mirroring
  `consumer.defaultOptions()` (`consumer/options.go:185-197`).
  `PlanDynamicConsumers` builds its expected `jetstream.ConsumerConfig` from
  `DefaultDynamicDefaults()`, so a precreated consumer carries the runtime
  defaults for **every** immutable field: `AckPolicy = AckExplicitPolicy`
  (the only ack policy valid for at-least-once partition work),
  `DeliverPolicy = DeliverAllPolicy` (hard-coded in the builder),
  `MaxWaiting = 2` (the runtime `DefaultMaxWaiting`), `MemoryStorage = false`
  (file storage). The genuinely mutable runtime-owned tunables (`AckWait`,
  `MaxDeliver`, `InactiveThreshold`, `MaxAckPending`, `ConsumerReplicas` —
  the last is explicitly live-editable per `consumer/options.go:511`) are
  also set from the defaults but the runtime overwrites them freely on start.
- **Precreation supports only `consumer.Dynamic` instances that do not
  override `WithAckPolicy`, `WithConsumerMemoryStorage`, or `WithMaxWaiting`.**
  An operator who overrides one of those to a non-default value must not set
  `partitionsRef` on that target — a documented operator responsibility, the
  same class as `ControlPlaneConfig` mirroring `parti.Config`. The W5 docs
  state it plainly. (`DeliverPolicy` is never operator-overridable — the
  builder hard-codes it.)
- **It is not a silent footgun for the present-consumer case.** When
  `PlanConsumers` finds a live consumer (one the runtime already created), it
  compares the live `FilterSubject`, `AckPolicy`, `DeliverPolicy`,
  `MaxWaiting`, and `MemoryStorage` against the expected runtime-default
  values; any mismatch is reported as a `drift-immutable` `dynamic-consumer`
  finding (W2), so a misconfigured opt-in surfaces on the next `plan`. The
  only window provision genuinely cannot see is a *never-yet-created*
  consumer whose future runtime will use a non-conforming value — hence the
  documented operator responsibility.
- **A test pins `DefaultDynamicDefaults()` against the runtime.** The W4
  byte-equivalence roundtrip test (`provision/dynamic_consumers_test.go`)
  constructs a `durable.WorkerConsumerConfig`, calls `SetDefaults()`, and
  asserts the immutable fields of `DefaultDynamicDefaults()` equal the
  runtime's — so a future runtime-default change that diverges fails the
  test rather than silently breaking precreation.

### The "byte-equivalent to runtime" gate — which reading

The Phase 1 roadmap gates Phase 5 on the W4 builder being "proven
byte-equivalent to the runtime." There are two readings; **Phase 5 adopts
reading (b)** and states it as an inherited invariant:

- (a) *Full* `ConsumerConfig` wire-format equivalence (every field). The
  builder does **not** meet this — provision passes zero tunables, the
  runtime passes configured ones.
- (b) **Equivalence on the identity / immutable fields** — the fields that
  determine *which* consumer is *the* consumer for a `(prefix, subject)` pair
  and that NATS will not let an update change. `Name`, `Durable`,
  `FilterSubject` are derived deterministically and `DeliverPolicy` is the
  hard-coded `DeliverAllPolicy` (`internal/dynamicbuild/builder.go:86`) —
  these four are byte-identical between the two callers unconditionally.
  `AckPolicy`, `MaxWaiting`, and `MemoryStorage` are byte-identical **under
  the immutable-field contract above** (both sides build from the runtime
  defaults); a violation of that contract is caught as `drift-immutable`, not
  silently accepted. The gate is met under reading (b): provision creates the
  consumer the runtime will recognize and adopt, not a byte-identical tuning.

## Invariants Inherited from Phases 1-4

Every invariant from the
[Phase 1 list](../provision-sdk-cli/00-implementation-plan.md#invariants-inherited-by-every-phase)
continues to hold. The load-bearing ones for Phase 5:

- **Ownership marker.** `parti.io/managed` / `parti.io/component` /
  `parti.io/instance` stay in resource `Metadata`, informational only.
  Phase 5 adds **no** marker to consumers and **no** new component value (the
  `ComponentDynamicConsumer` constant already exists and stays unused by the
  precreation path — see "The runtime-owns model").
- **JSON output envelope** stays `apiVersion: parti.io/provision/v1`. Phase 5
  adds, all additive: one new `PlannedAction.Kind` value — `create-consumer`.
  It reuses the **existing** `DriftFinding.Kind` value `dynamic-consumer`
  (`KindDynamicConsumer`, `provision/types.go`) and the existing
  `ConsumerState` / `PlannedConsumer` types. No existing value changes.
- **Input config** `apiVersion: parti.io/v1` accepts additive fields.
  Phase 5 adds **no** new config field — it gives the existing
  `DynamicConsumerCfg.PartitionsRef` field a defined meaning (see W1).
- **CLI exit codes and precedence** (`cmd/partictl/exitcodes.go:14-25`) are
  stable: `0` ok, `1` runtime, `2` drift, `3` validation, `4` NATS. No new
  codes — consumer errors map onto the existing `ErrInvalidConfig` /
  `ErrLiveValidation` sentinels and the existing `classifyError` table.
- **`Plan` action and drift ordering remains deterministic** — `(Kind,
  Name)`. `PlanConsumers` sorts its `create-consumer` actions and
  `dynamic-consumer` findings the same way.
- **Identity-field byte-equivalence with the runtime** — the Phase-5 analogue
  of the Phase 1/3/4 byte-equivalence invariants. For a given `(streamName,
  consumerPrefix, subjectTemplate, partition)` tuple, `provision` and the
  runtime construct byte-identical `Name`, `Durable`, `FilterSubject`, and
  `DeliverPolicy`, and identical `AckPolicy` / `MaxWaiting` / `MemoryStorage`
  under the immutable-field contract above. The contract is the shared
  `internal/dynamicbuild` package; both callers route through it. Phase 5
  introduces no second construction path.

## Non-Goals (Phase 5)

- **Do not stamp an ownership marker on consumers.** See "The runtime-owns
  model." Consumers are located by deterministic durable name, never by
  marker.
- **Do not manage consumer tunables.** `provision` precreates identity-only
  and never updates a consumer's `AckWait` / `MaxDeliver` / `MaxAckPending` /
  etc. There is **no `update-consumer` action.** A live consumer whose
  tunables differ from any value is **not** drift — the runtime owns tuning.
- **Do not delete or recreate consumers.** A live consumer whose *identity*
  diverges (a name collision carrying a different `FilterSubject` — only
  reachable by a hand-created consumer, since the durable name encodes the
  subject hash) is reported `drift-immutable` and never mutated.
  **Phase 6 (Force + Repair)** owns the gated delete/recreate path.
- **Do not precreate consumers from the top-level `plan` / `apply` / `adopt`
  commands.** Those commands keep their Phase 1-4 behavior — they ignore
  `dynamicConsumers:` for mutation. Precreation is reached only through the
  new `partictl consumers` command (and the `PlanConsumers` /
  `ApplyConsumers` SDK functions). This preserves every existing config's
  behavior and makes precreation explicitly opt-in.
- **Do not enable least-privilege runtime deployments.** See "The runtime-owns
  model" — that needs a runtime change, out of scope.
- **Do not change the `dynamicConsumers:` alignment-check surface.**
  `ValidateLive` / `ValidateLiveDynamicConsumers` keep their Phase 1 behavior
  for every target; Phase 5 only *adds* the `consumers` plan/apply path.
- **Do not read partitions from a live KV key during planning.**
  `PlanConsumers` resolves partitions from the config's declared
  `partitionSource.partitions` set (W1) — a deterministic, I/O-free
  resolution. Reconciling against the *live* partition table is the job of
  `partictl partitions` (Phase 3).
- **Do not certify the live partition table.** Honest scope, stated plainly:
  a successful `partictl consumers apply` means the consumers for the
  **declared** `partitionSource.partitions` set exist — **not** that they
  match the live partition table the runtime will actually read. The live
  table can be mutated independently (`source.NatsKV.AddPartitions` /
  `RemovePartitions` / `Modify`), exactly as Phase 3's `ApplyPartitions`
  documents its own honest-scope limit. The intended operator workflow is
  `partictl partitions apply` (converge the live table to the declared set)
  **then** `partictl consumers apply` (precreate for that set). The W5 docs
  state this and the recommended sequencing.
- **Do not precreate consumers that violate the immutable-field contract.**
  See "The immutable-field contract." A `consumer.Dynamic` configured with a
  non-explicit `WithAckPolicy`, a non-default `WithMaxWaiting`, or
  `WithConsumerMemoryStorage(true)` must not be opted into precreation;
  `plan` reports a `drift-immutable` finding if such a consumer is found live.

## Design

The organizing decision mirrors **Phase 3**: dynamic-consumer precreation
gets its own SDK entrypoints (`PlanConsumers` / `ApplyConsumers`) and its own
two-level CLI command (`partictl consumers plan|apply`), rather than being
folded into the top-level `Plan` / `Apply`. Rationale: precreation is opt-in
and operates on *derived* resources (per-partition consumers generated from a
template plus a partition set), exactly the shape that earned partition
records their own surface in Phase 3. The top-level commands stay untouched.

### W1 — `PartitionsRef` semantics, validation, partition resolution

**`DynamicConsumerCfg.PartitionsRef` becomes the precreation opt-in.** No new
config field is added; the existing unused field is given a contract:

- `PartitionsRef` **empty** → the target is alignment-check-only (the Phase 1
  behavior, unchanged). `PlanConsumers` skips it.
- `PartitionsRef` **non-empty** → the target is opted into precreation. Its
  value must equal `cfg.PartitionSource.Bucket` — it names the
  partition-source the consumer's partition set is drawn from. This makes the
  field a checkable cross-reference rather than a free-form string, and keeps
  the door open for multiple partition sources in a later phase. A
  `PartitionsRef` that does not match `cfg.PartitionSource.Bucket` is
  `ErrInvalidConfig`.

A new exported helper `ValidateConsumerSet(cfg Config) error` performs the
Phase-5 static validation (called by `PlanConsumers` and the CLI before any
NATS I/O):

- At least one `DynamicConsumerCfg` has a non-empty `PartitionsRef` (else
  "no dynamic consumers opted into precreation").
- For every target with a non-empty `PartitionsRef`: `cfg.PartitionSource`
  is non-nil, `cfg.PartitionSource.Partitions` is non-empty (reuse
  `ValidatePartitionSet` from Phase 3), and `PartitionsRef ==
  cfg.PartitionSource.Bucket`.
- Each opted-in target's `StreamName` / `ConsumerPrefix` / `SubjectTemplate`
  pass the same checks `PlanDynamicConsumers` already enforces
  (`provision/dynamic_consumers.go:53-77`) — non-empty, prefix charset,
  template contains `{{.PartitionID}}`.
- All errors wrap `ErrInvalidConfig` (CLI exit `3`).

A new internal helper `resolveConsumerPartitions(cfg Config) []types.Partition`
returns `cfg.PartitionSource.Partitions` (the declared set). It is trivial
today — the indirection exists so a future phase can resolve a live read
without touching callers.

### W2 — `PlanConsumers`: per-partition consumer diff

New file `provision/consumer_records.go` (named for symmetry with Phase 3's
`partition_records.go`):

```go
func PlanConsumers(ctx context.Context, js jetstream.JetStream, cfg Config) (PlanResult, error)
```

Algorithm:

1. **Static validation** — run the inherited `Validate(cfg)` first (the
   Phase 1 input-schema boundary), then `ValidateConsumerSet(cfg)`. Both
   complete before any NATS I/O, so a malformed config yields CLI exit `3`,
   never a NATS-class error.
2. For each `DynamicConsumerCfg` with a non-empty `PartitionsRef`, in config
   order:
   - Build the expected consumer set with the existing
     `PlanDynamicConsumers(streamName, consumerPrefix, subjectTemplate,
     resolveConsumerPartitions(cfg))`. This reuses the proven builder
     verbatim — no second construction path.
   - For each expected `PlannedConsumer`, look it up live:
     `js.Consumer(ctx, streamName, durable)`. **The lookup key is the
     deterministic durable name, so `Name` and `Durable` need no separate
     field comparison** — a consumer the lookup returns necessarily carries
     that exact `Name` / `Durable` (nats.go's lookup signature takes only the
     stream and the consumer name); the durable name is therefore proven by
     the lookup itself, and the comparisons below cover only the remaining
     identity / immutable fields.
     - `ErrConsumerNotFound` → emit one `create-consumer` `PlannedAction`
       (Resource: the `PlannedConsumer`) and one `dynamic-consumer`
       `DriftFinding`, severity `drift-mutable`.
     - found, and every remaining identity / immutable field of the live
       consumer matches the expected — `info.Config.FilterSubject ==
       expected.Subject`, `info.Config.AckPolicy ==
       jetstream.AckExplicitPolicy`, `info.Config.DeliverPolicy ==
       jetstream.DeliverAllPolicy`, `info.Config.MaxWaiting ==
       expected.Config.MaxWaiting`, `info.Config.MemoryStorage == false` →
       one `informational` `dynamic-consumer` finding (the consumer exists and
       is correct; provision does not inspect the runtime-owned mutable
       tunables).
     - found, but any identity / immutable field differs → one
       `drift-immutable` `dynamic-consumer` finding whose `Detail` names the
       offending field. Reachable causes: a different `FilterSubject` (a
       hand-created durable-name collision — see the durable-name note in
       Background), or a non-`AckExplicitPolicy` `AckPolicy` / a non-default
       `MaxWaiting` / a `MemoryStorage: true` (a target wrongly opted into
       precreation — see
       "The immutable-field contract"). No action — Phase 6 owns
       delete/recreate.
   - Stream-not-found while looking up a consumer surfaces as a typed error
     directing the operator to provision the stream first
     (`ErrConsumerStreamMissing`, satisfying `errors.Is(err,
     ErrLiveValidation)` so it maps to CLI exit `3`, mirroring Phase 3's
     `ErrPartitionBucketMissing`).
3. `create-consumer` actions sort by `(Kind, Name)` where `Name` is the
   durable name; `dynamic-consumer` findings sort the same way. Determinism
   is preserved.

New constant in `provision/types.go`:

```go
// ActionCreateConsumer is emitted by PlanConsumers for each per-partition
// durable consumer that a precreation-opted DynamicConsumerCfg describes
// and that does not exist live. Apply calls js.CreateConsumer. Resource is
// a PlannedConsumer value.
const ActionCreateConsumer = "create-consumer"
```

`PlanConsumers` returns the standard `PlanResult` envelope (`APIVersion`,
`Kind: "Plan"`, non-nil `Actions` / `Drift`).

### W3 — `ApplyConsumers`: idempotent create

New file `provision/apply_consumers.go`:

```go
func ApplyConsumers(ctx context.Context, js jetstream.JetStream, plan PlanResult) (Report, error)
```

`ApplyConsumers` executes every `create-consumer` action in `plan`. Consumer
creation is **idempotent on identity** — a consumer that already exists with
the same identity is the desired outcome — so there is no `--prune`-style
gate and no stale-before dance; the design is closer to the `create-kv` /
`create-stream` paths than to `update-stream`. The one re-read it performs is
verification-only, on the `ErrConsumerExists` create-race path (below).

**Testability seam** — mirror the Phase 4 `streamManager` seam
(`provision/apply_stream.go`):

```go
// consumerManager is the apply-side seam for the create-consumer action.
type consumerManager interface {
    // CreateConsumer creates a consumer on a stream. A consumer that already
    // exists with an identical config succeeds; one that exists with a
    // differing config surfaces as jetstream.ErrConsumerExists.
    CreateConsumer(ctx context.Context, stream string, cfg jetstream.ConsumerConfig) error
    // ConsumerInfo re-reads a consumer's live config by durable name. A
    // missing consumer surfaces as jetstream.ErrConsumerNotFound. Used to
    // verify identity on a create-race.
    ConsumerInfo(ctx context.Context, stream, durable string) (*jetstream.ConsumerInfo, error)
}
```

`jsConsumerManager` is the production adapter over `jetstream.JetStream`.

`applyCreateConsumerAction(ctx, mgr, action) (ExecutedAction, error)`:

- Type-assert `action.Resource` to a `PlannedConsumer` (defensive guard →
  fail-fast error if wrong type, mirroring the create-stream guard).
- Call `mgr.CreateConsumer(ctx, res.StreamName, res.Config)`.
- `err == nil` → `ExecutedAction{Kind, Name, Raced: false}`.
- `errors.Is(err, jetstream.ErrConsumerExists)` **or**
  `errors.Is(err, jetstream.ErrConsumerNameAlreadyInUse)` → a consumer with
  this durable name already exists. **This is not blindly a success.**
  `ErrConsumerExists` specifically means "exists with a *differing* config" —
  and the difference might be in an identity field (a hand-created durable-name
  collision carrying a wrong `FilterSubject`, or a runtime that created the
  consumer with a non-`AckExplicitPolicy` policy), not only in a runtime-owned
  tunable. Verify before blessing it:
  - Re-read the live consumer via `mgr.ConsumerInfo(ctx, res.StreamName,
    res.Durable)` and compare its remaining identity / immutable fields
    (`FilterSubject`, `AckPolicy`, `DeliverPolicy`, `MaxWaiting`,
    `MemoryStorage`) against `res.Config`. `Name` / `Durable` need no
    comparison — the re-read key is
    `res.Durable`, so a returned consumer carries that durable by
    construction (the same reasoning as the W2 lookup).
  - All identity / immutable fields match → the only differences are
    runtime-owned mutable tunables → **raced success**,
    `ExecutedAction{Kind, Name, Raced: true}` (the consumer analogue of the
    `create-stream` `ErrStreamNameAlreadyInUse` handling).
  - Any identity / immutable field differs → fail-fast `ResourceError` naming
    the offending field — an identity divergence the operator resolves via the
    Phase 6 repair path. This is the apply-time analogue of the W2
    `drift-immutable` classification.
  - The re-read itself returns `ErrConsumerNotFound` (the consumer was
    deleted between the failed create and the re-read) → fail-fast
    `ResourceError` (a genuine concurrent-delete race; the operator re-runs
    `consumers plan`).
- `errors.Is(err, jetstream.ErrStreamNotFound)` → fail-fast
  `consumer-stream-missing` error (the stream was deleted between plan and
  apply).
- `context.Canceled` / `DeadlineExceeded` → cancellation, surfaced to the
  caller.
- any other error → fail-fast resource error.

`ApplyConsumers` folds each result into the `Report` with the standard
cancellation / fail-fast / skip contract — reuse the Phase 4
`foldActionResult` helper (`provision/apply.go`) verbatim; it is already
generic over action kinds. Every returned `Report` carries the envelope
fields (`APIVersion`, `Kind: "Report"`, non-nil slices), including the
no-action case.

`ApplyConsumers` does **not** re-run `Plan`; it executes a caller-supplied
plan, exactly as `ApplyPartitions` does. The CLI (W4) runs `PlanConsumers`
then `ApplyConsumers`.

### W4 — `partictl consumers` subcommand

New file `cmd/partictl/cmd_consumers.go`. A two-level command mirroring
`cmd/partictl/cmd_partitions.go`:

```
partictl consumers plan  -f <config> [-json] [-fail-on-drift]
partictl consumers apply -f <config> [-json] [-dry-run]
```

- `-f` loads the same `parti-env.yaml`.
- `consumers plan` → `provision.PlanConsumers`, renders the diff (text or
  `-json`). `-fail-on-drift` → exit `2` when **non-informational** drift is
  present (reuse `hasDrift`, which already excludes `informational` findings;
  a fully-precreated consumer set emits only `informational` findings and
  must exit `0`).
- `consumers apply` → `PlanConsumers` then `ApplyConsumers`. `--dry-run`
  aliases `consumers plan`.
- `--policy` is **not** accepted by `consumers` — precreation is
  policy-independent (a consumer either exists or is created; there is no
  warn/safe-update/adopt distinction for it), exactly as `partitions` omits
  `--policy`. The inherited static-validation boundary still rejects an
  invalid `policy` *value* in the config via `Validate`.
- Exit codes route through the existing `classifyError` — no `exitcodes.go`
  change. `ErrConsumerStreamMissing` satisfies `errors.Is(err,
  ErrLiveValidation)` → exit `3`.
- Register `consumers` in `run.go`'s dispatch and usage text.
- `output.go` text renderers already print arbitrary action/drift `Kind`
  strings generically (`renderPlanText` / `renderReportText`); the
  `create-consumer` kind needs no renderer change. Verify this during
  implementation.
- **Two-level command + NATS flag note.** `runWithNATS` splices `-server`
  after `args[0]` and breaks two-level commands; `cmd_consumers_test.go`
  must build args manually (`runArgs("consumers", "plan", "-server", url,
  …)`), exactly as `cmd_partitions_test.go` does.

### W5 — Documentation

- `docs/PROVISION.md`: a new "Dynamic Consumer Precreation" section — the
  `partitionsRef` opt-in, `partictl consumers plan/apply`, the identity-only
  / runtime-owns model (and *why* there is no marker and no tunable
  management), the immutable-field contract (precreation supports only
  `AckExplicitPolicy`, file-storage consumers), the honest-scope limit (precreation targets
  the *declared* partition set — run `partictl partitions apply` first when
  live-table readiness is needed), and the precreation-is-not-least-privilege
  caveat. Add a TOC entry.
- Package godoc for the new exported surface (`PlanConsumers`,
  `ApplyConsumers`, `ValidateConsumerSet`, `ActionCreateConsumer`,
  `ErrConsumerStreamMissing`).
- `CHANGELOG.md`: a new entry under `[Unreleased]`.

## Work Items

| ID | Scope | Impl model | Review effort |
|----|-------|------------|---------------|
| W1 | `PartitionsRef` opt-in contract; `ValidateConsumerSet`; `resolveConsumerPartitions` | sonnet | high |
| W2 | `PlanConsumers`, `ActionCreateConsumer`, consumer lookup + drift classification, `ErrConsumerStreamMissing` | sonnet | xhigh |
| W3 | `ApplyConsumers`, `consumerManager` seam, `applyCreateConsumerAction`, raced-success handling | opus | xhigh |
| W4 | `partictl consumers plan/apply`; `run.go` wiring | sonnet | high |
| W5 | `docs/PROVISION.md`, godoc, `CHANGELOG.md` | sonnet | high |

Per-work-item loop (unchanged from Phases 1-4): implement → `/simplify` →
codex post-impl review → fix every P0/P1 → re-verify `go build ./...`,
`make lint`, package tests → commit. W2 (drift classification + the
runtime-overwrite interaction) and W3 (apply idempotency / raced-success
correctness) are the sharp items and carry `xhigh` review effort.

## Test Plan

Each invariant has an encoding:

- **W1 validation:** a target with empty `partitionsRef` is skipped by
  `ValidateConsumerSet` and by `PlanConsumers` (still alignment-checkable);
  a non-empty `partitionsRef` that does not equal
  `partitionSource.bucket` → `ErrInvalidConfig`; a precreation-opted target
  with no `partitionSource` declared, or an empty `partitions` list →
  `ErrInvalidConfig`; an invalid `consumerPrefix` / `subjectTemplate` →
  `ErrInvalidConfig`; a config with **no** opted-in target →
  `ErrInvalidConfig` ("no dynamic consumers opted into precreation").
- **W2 plan:** all consumers missing → one `create-consumer` action +
  `drift-mutable` finding per partition; all present with matching identity
  fields → all `informational`, no actions; mixed; a present consumer with a
  mismatched `FilterSubject` → `drift-immutable` (`Detail` names
  `filterSubject`), no action; a present consumer with a non-`AckExplicitPolicy`
  `AckPolicy` → `drift-immutable` (`Detail` names `ackPolicy`), no action; a
  present consumer with a non-default `MaxWaiting` → `drift-immutable`
  (`Detail` names `maxWaiting`), no action; a present consumer with
  `MemoryStorage: true` → `drift-immutable` (`Detail` names `memoryStorage`),
  no action; stream-not-found → `ErrConsumerStreamMissing`; the per-partition durable
  names match `PerSubjectDurableName` exactly (a regression guard against a
  second naming path); deterministic `(Kind, Name)` ordering; an empty
  `partitionsRef` target produces neither actions nor findings.
- **W2 identity-equivalence:** a focused test that `PlanConsumers`'s expected
  `PlannedConsumer.Config` identity / immutable fields (`Name`, `Durable`,
  `FilterSubject`, `AckPolicy`, `DeliverPolicy`, `MaxWaiting`,
  `MemoryStorage`) equal what `dynamicbuild.ConsumerConfig` produces, and that
  `dynamicbuild.DefaultDynamicDefaults()` matches a default-`SetDefaults()`
  `durable.WorkerConsumerConfig` on the immutable fields — the inherited
  identity-field byte-equivalence invariant.
- **W3 apply (via the seam, no live server):** clean `create-consumer`;
  `ErrConsumerExists` / `ErrConsumerNameAlreadyInUse` followed by a re-read
  whose identity / immutable fields **match** → `Raced: true` success; the
  same followed by a re-read with a mismatched `FilterSubject`, `AckPolicy`,
  `DeliverPolicy`, `MaxWaiting`, and `MemoryStorage` → each a fail-fast
  `ResourceError` naming the field; `ErrConsumerExists` followed by a re-read returning
  `ErrConsumerNotFound` (concurrent delete) → fail-fast; wrong `Resource`
  type → fail-fast; stream-gone (`ErrStreamNotFound`) at create time →
  `consumer-stream-missing` fail-fast; context cancellation → `Aborted: true`,
  the action in `Skipped` with `context-cancelled`, `ctx.Err()` returned; a
  non-cancellation error fail-fasts and skips the remainder with
  `prior-error`; the no-action plan returns an envelope `Report`.
- **W4 CLI:** `consumers plan` exit `0` (no diff, and `0` when only
  `informational` findings are present) / `2` (`-fail-on-drift` with
  non-informational drift present); `consumers apply` exit `0` / `1` (a
  server-rejected create)
  / `3` (`ErrConsumerStreamMissing`, bad config); `--dry-run` performs no
  write; `--policy` rejected on `consumers`; JSON envelope `apiVersion`
  present; two-level args built manually (the `runWithNATS` caveat).
- **Integration — the no-oscillation invariant (the load-bearing test):**
  a live-NATS end-to-end test — provision a stream, `partictl consumers
  apply` to precreate the per-partition consumers, then **simulate a worker
  start** by calling `jsutil.EnsureConsumer` (the runtime's exact path) for
  each consumer with a runtime `Defaults` taken from
  `dynamicbuild.DefaultDynamicDefaults()` but with the **mutable** tunables
  (`AckWait`, `MaxDeliver`, `InactiveThreshold`, `MaxAckPending`,
  `ConsumerReplicas`) **deliberately overridden** to non-default values — so
  the test proves runtime-owned mutable fields do not register as drift. The
  immutable fields (`AckPolicy`, `MaxWaiting`, `MemoryStorage`) are kept at
  the runtime defaults, since varying them would (correctly) make the
  runtime's own `CreateOrUpdateConsumer` fail — that is exactly the failure
  the contract avoids by precreating from the runtime defaults.
  Then re-run `PlanConsumers` and assert it emits **zero
  `create-consumer` actions** and that **`hasDrift(plan.Drift)` is false** —
  i.e. every `dynamic-consumer` finding is `informational` (W2 emits one
  informational finding per existing consumer; the test asserts finding
  *severity*, not an empty `Drift` slice). The runtime's
  `CreateOrUpdateConsumer` overwrite must not make a precreated consumer look
  missing or non-informationally drifted. This encodes the central
  runtime-owns decision.
- **Integration — precreate then read back:** after `consumers apply`, each
  expected durable resolves via `js.Consumer` with the expected
  `FilterSubject`.
- **Integration — create-race identity verification:** between
  `PlanConsumers` and `ApplyConsumers`, hand-create a consumer with one of
  the planned durable names but a wrong `FilterSubject`; assert
  `ApplyConsumers` fails that action with a `ResourceError` (the apply-time
  re-read catches the identity divergence) rather than reporting a raced
  success.
- **Integration — live partition mismatch (honest scope):** apply partition
  records for {A,B}, mutate the live key via `source.NatsKV.AddPartitions` to
  add C, then run `PlanConsumers`; assert it plans consumers for {A,B} only
  (the declared set) — encoding the documented honest-scope limit, not a live
  certification.

## Open Design Decisions

Surfaced for `plan-review`; the rest of the plan assumes the stated choice.

1. **Runtime owns the consumer config; no marker on consumers.** Chosen:
   precreate identity-only, locate consumers by deterministic durable name,
   stamp no `Metadata` marker. Rejected: marking consumers — it oscillates
   against the runtime's unconditional `CreateOrUpdateConsumer` overwrite.
   The alternative (a runtime change to merge `Metadata`) is a cross-package
   change deferred out of Phase 5.
2. **`PartitionsRef` as the precreation opt-in, valued as the
   partition-source bucket name.** Chosen: non-empty `partitionsRef` opts a
   target into precreation and must equal `partitionSource.bucket`. Rejected:
   a new `precreate: bool` field (redundant with the already-present unused
   field) and a free-form `partitionsRef` (un-checkable). Reopen if a config
   ever needs multiple partition sources.
3. **Separate `PlanConsumers` / `ApplyConsumers` + `partictl consumers`
   command.** Chosen: mirror Phase 3's partition-records surface; the
   top-level `plan` / `apply` stay untouched and precreation is opt-in by
   command. Rejected: folding consumers into the top-level `Plan` / `Apply`
   (would precreate on every `apply`, changing Phase 1-4 behavior).
4. **No `update-consumer`, no tunable management.** Chosen: provision creates
   identity-only and never updates a consumer; tunables are the runtime's.
   Rejected: an `update-consumer` action — it would fight the runtime for
   ownership of fields provision has no source of truth for.
5. **Precreation accepts a brief mutable-tunable gap.** Chosen: a precreated
   consumer carries the runtime-default mutable tunables (`AckWait`,
   `MaxDeliver`, `InactiveThreshold`, `MaxAckPending`, `ConsumerReplicas`)
   until the runtime first starts and `CreateOrUpdateConsumer`s them to the
   app-configured values. This is correct because precreation's purpose is
   *existence and readiness visibility*, not runtime tuning; only the
   immutable fields must match exactly. Documented plainly in W5.
6. **"Byte-equivalent to runtime" gate — reading (b).** Chosen: the gate is
   met by identity-field equivalence (the fields that determine consumer
   identity), which the shared `internal/dynamicbuild` builder already
   guarantees. Full-wire-format equivalence is neither met nor required for
   identity-only precreation.
7. **Precreate from the runtime defaults; immutable fields (`AckPolicy`,
   `MaxWaiting`, `MemoryStorage`) must match.** Chosen: provision precreates
   from `dynamicbuild.DefaultDynamicDefaults()`, so the NATS-immutable fields
   carry the runtime-default values (`AckExplicitPolicy`, `MaxWaiting=2`,
   file storage). The NATS server rejects a `CreateOrUpdateConsumer` that
   changes any of these (`server/consumer.go:2272-2313`; `MemoryStorage` per
   the storage-type rule), so precreation must match the runtime or runtime
   startup fails. A dynamic consumer that overrides `WithAckPolicy`,
   `WithMaxWaiting`, or `WithConsumerMemoryStorage` to a non-default value
   must not be opted into precreation — a documented operator responsibility
   — and `plan` surfaces a `drift-immutable` finding if such a consumer is
   found live, so the misconfiguration is not silent. Rejected: adding
   per-field overrides to `DynamicConsumerCfg` — all three are NATS-immutable,
   so a config/runtime disagreement already fails the runtime's
   `CreateOrUpdateConsumer` loudly; the fields would add a config-drift
   surface for no safety gain. **The `MaxWaiting` immutability was found
   empirically during W1/W2 implementation and confirmed against the NATS
   server source — the prior plan-review rounds missed it because they
   checked `consumer/options.go` annotations, not the server's
   update-rejection list.** Reopen if a real non-default
   `AckPolicy`/`MaxWaiting`/`MemoryStorage` dynamic-consumer use case appears.
8. **Apply verifies identity on the `ErrConsumerExists` create-race.** Chosen:
   `ApplyConsumers` re-reads a colliding consumer and compares identity fields
   before recording a raced success; an identity mismatch is a fail-fast
   `ResourceError`. Rejected: treating every `ErrConsumerExists` as success —
   it would bless a hand-created or wrong-policy consumer as the operator's.
9. **Honest scope — declared set, not live table.** Chosen: `PlanConsumers`
   resolves partitions from `cfg.PartitionSource.Partitions`; a successful
   `consumers apply` certifies the declared set, not the live partition table.
   The intended workflow is `partitions apply` then `consumers apply`.
   Rejected: a live KV read during `PlanConsumers` — it would couple consumer
   planning to partition-table liveness and duplicate Phase 3's job. Mirrors
   Phase 3 `ApplyPartitions`'s own honest-scope posture.
