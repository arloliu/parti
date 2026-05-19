# W4 — Dynamic-Consumer Alignment Sub-Spec

Companion to [`00-implementation-plan.md`](00-implementation-plan.md). Scope:
the pure `PlanDynamicConsumers` builder, the live
`ValidateLiveDynamicConsumers` companion, the extraction seam in
`internal/durable`, and the v1 equality subset. Public API surface is
not redesigned here — see the master plan's
[Dynamic Consumer Planning](00-implementation-plan.md#dynamic-consumer-planning)
section. This sub-spec only fills in the implementation contract.

## 1. Extraction seam

### Chosen option: (b) — pure-helper sub-package

Create **`internal/dynamicbuild`** (new package). It exports pure
functions and is imported by both `consumer/` (existing runtime path)
and `provision/` (new W4 path). The runtime `WorkerConsumer` keeps its
method receivers but delegates the actual work to the pure helpers.

Rationale:

- The three pieces of logic the master plan calls out
  (`buildSubjects`/`generateSubject`, `perSubjectDurableName`, and the
  `ConsumerConfig` literal in `ensurePerSubjectConsumer`) are tightly
  coupled to `*WorkerConsumer` receiver state
  (`wc.subjectTemplate`, `wc.partitionPrefix/Suffix`, `wc.config.*`).
  Pure functions that take their inputs explicitly are cleaner than
  in-place `*WorkerConsumer` method export and remove the need for
  `provision` to construct a half-initialized `WorkerConsumer`.
- `consumer/` and `provision/` both live under the module root, so
  both may import `internal/dynamicbuild`. This satisfies the
  master-plan invariant *"No private durable-naming code is duplicated
  in `provision`. The pure builder is consumed by `provision`; runtime
  `consumer.Dynamic` continues to call the same shared logic."*
- `consumer.Dynamic` does **not** change its public shape. Internal
  callsites in `internal/durable/worker_consumer.go` are rewritten to
  call `dynamicbuild` helpers; runtime behavior is preserved.

### Exact code to extract

All file:line references are against this worktree's
`internal/durable/worker_consumer.go` and `subject_utils.go`.

| Source                                                       | Target in `internal/dynamicbuild`                              |
|--------------------------------------------------------------|-----------------------------------------------------------------|
| `worker_consumer.go:479-495` (`perSubjectDurableName`)       | `func DurableName(prefix, subject, partitionPrefix, partitionSuffix string) string` |
| `worker_consumer.go:498-525` (`sanitizeConsumerName`, `isAllowedConsumerRune`) | unexported helpers in the new package                          |
| `worker_consumer.go:544-593` (`doBuildSubjects`, `generateSubject`) | `func BuildSubjects(subjectTemplate string, partitions []types.Partition) ([]string, error)` and an internal `generateSubject` |
| `worker_consumer.go:461-473` (the canonical `ConsumerConfig` literal in `ensurePerSubjectConsumer`) | `func ConsumerConfig(durable, subject string, defaults Defaults) jetstream.ConsumerConfig` |
| `subject_utils.go:18-25` (`parseSubjectTemplateParts`)       | re-exported as `ParseSubjectTemplateParts` (already pure)       |

The runtime literal at `worker_consumer.go:414-426` (inside
`addSubjectLoop`) is a **second copy** of the same shape, used to
populate `partitionConsumerOpts.consumerConfig` (metadata for
recovery). W4 must rewrite that callsite to use the same
`dynamicbuild.ConsumerConfig` helper so the two literals cannot drift
from each other in the future. This is a small ancillary refactor —
no behavior change at the runtime level.

`Defaults` is a thin value type that captures the seven runtime
tunables the runtime literal currently reads off
`wc.config.*`:

```go
type Defaults struct {
    AckPolicy             jetstream.AckPolicy
    AckWait               time.Duration
    MaxDeliver            int
    InactiveThreshold     time.Duration
    MaxWaiting            int
    MaxAckPending         int
    ConsumerMemoryStorage bool
    ConsumerReplicas      int
}
```

`provision.PlanDynamicConsumers` constructs `Defaults` using the
runtime-default values listed in §2 below (i.e. what
`durable.WorkerConsumerConfig.SetDefaults()` would produce after
`fuda.SetDefaults` runs against an empty struct). Runtime keeps
passing `wc.config.*` and gets identical behavior.

`PlanDynamicConsumers` **must not** expose `Defaults` as a parameter
in v1 — the master plan explicitly forbids an option variadic on the
builder. The runtime defaults are hard-coded inside
`PlanDynamicConsumers` and asserted by the v1 equality test against
`durable.WorkerConsumerConfig.SetDefaults()` output. (See
[§5 Test plan](#5-test-plan-for-w4).)

## 2. Enumerated equality subset

Runtime evidence: the canonical literal at
`internal/durable/worker_consumer.go:461-473` is the one passed to
`jsutil.EnsureConsumer`, i.e. the bytes that hit JetStream.
`DeliverPolicy` is intentionally absent from the literal — the
runtime relies on the `jetstream.DeliverPolicy` zero value
`DeliverAllPolicy` (`jetstream/consumer_config.go:411`). Likewise
`AckPolicy` defaults to `AckExplicitPolicy` (zero value,
`jetstream/consumer_config.go:493`) when
`durable.WorkerConsumerConfig.AckPolicy` is unset.

The v1 equality table below enumerates **every field on
`jetstream.ConsumerConfig` that runtime touches** in the construction
path of `ensurePerSubjectConsumer`. The implementer must be able to
write the v1 equality test from this table alone.

### v1 in-scope fields (asserted by the W4 test)

| Field            | Runtime source                              | v1 default value             | Reason in scope                          |
|------------------|---------------------------------------------|------------------------------|------------------------------------------|
| `Name`           | `worker_consumer.go:462`                    | `<prefix>_<partitionId>_<hash>` | Identity; deterministic from inputs.   |
| `Durable`        | `worker_consumer.go:463`                    | `<prefix>_<partitionId>_<hash>` | Identity; deterministic from inputs.   |
| `FilterSubject`  | `worker_consumer.go:464`                    | derived from `subjectTemplate`+`partition.SubjectKey()` | Identity; deterministic from inputs. |
| `AckPolicy`      | `worker_consumer.go:465`                    | `jetstream.AckExplicitPolicy` | Semantic constant for Parti dynamic.    |
| `DeliverPolicy`  | **not in literal** (zero-value `DeliverAllPolicy`) | `jetstream.DeliverAllPolicy` | Semantic constant; load-bearing for WorkQueuePolicy compat. |

The pure builder must hard-code `AckPolicy: jetstream.AckExplicitPolicy`
and `DeliverPolicy: jetstream.DeliverAllPolicy` in the
`ConsumerConfig` it returns, **not** rely on Go zero-value coincidence.
The runtime literal currently relies on zero values for both; the
builder's explicit assignment is documented and the test is robust to
any future `iota` reordering in nats.go. (Tightening the runtime
literal the same way is a follow-up nit, **not** a W4 deliverable.)

### v1 out-of-scope fields (tunables; runtime sets them from options)

The implementer must **not** assert these in the W4 equality test.
Each is set in the runtime literal at `worker_consumer.go:466-472`
but its value depends on options the v1 builder does not accept:

| Field                  | Runtime source              | Default after `SetDefaults` | Reason out of scope                                 |
|------------------------|-----------------------------|-----------------------------|-----------------------------------------------------|
| `AckWait`              | `worker_consumer.go:466`    | `30s` (`config.go:174`)     | Tunable via `consumer.WithAckWait`.                 |
| `MaxDeliver`           | `worker_consumer.go:467`    | `-1` (`config.go:178`)      | Tunable via `consumer.WithMaxDeliver`.              |
| `InactiveThreshold`    | `worker_consumer.go:468`    | `24h` (`config.go:220`)     | Tunable via `consumer.WithInactiveThreshold`.       |
| `MaxWaiting`           | `worker_consumer.go:469`    | `2` (`config.go:190`)       | Tunable via `consumer.WithMaxWaiting`.              |
| `MaxAckPending`        | `worker_consumer.go:470`    | `0` (server default)        | Tunable via `consumer.WithMaxAckPending`.           |
| `MemoryStorage`        | `worker_consumer.go:471`    | `false`                     | Tunable via `consumer.WithConsumerMemoryStorage`.   |
| `Replicas`             | `worker_consumer.go:472`    | `0` (inherit stream)        | Tunable via `consumer.WithConsumerReplicas`.        |

### `jetstream.ConsumerConfig` fields runtime does NOT touch (also out of scope)

`Description`, `DeliverSubject`, `OptStartSeq`, `OptStartTime`,
`Heartbeat`, `FlowControl`, `DeliverGroup`, `RateLimit`, `SampleFrequency`,
`HeadersOnly`, `MaxRequestBatch`, `MaxRequestExpires`, `MaxRequestMaxBytes`,
`BackOff`, `Metadata`, `FilterSubjects`, `PauseUntil`, `PriorityPolicy`,
`PriorityGroups`, `PinnedTTL`, `OverflowMinPending`, `OverflowMinAckPending`,
plus any field added by nats.go after this writing. The runtime literal
at `worker_consumer.go:461-473` does not set any of them, so the
builder must not set them either, and the equality test must not
assert them.

## 3. Public surface (recap, do not redesign)

The master plan locks the W4 public surface:

```go
type PlannedConsumer struct {
    StreamName string
    Subject    string
    Durable    string
    Config     jetstream.ConsumerConfig
}

func PlanDynamicConsumers(
    streamName, consumerPrefix, subjectTemplate string,
    partitions []types.Partition,
) ([]PlannedConsumer, error)

func ValidateLiveDynamicConsumers(
    ctx context.Context,
    js jetstream.JetStream,
    cfgs []DynamicConsumerCfg,
) error
```

W4 narrows `provision.PlannedConsumer.Config` from `any`
(`provision/types.go:169`) to `jetstream.ConsumerConfig`.

### Input validation rules (`PlanDynamicConsumers`)

All wrap a single sentinel `provision.ErrInvalidInput` (already
present in v1 conventions; reuse the existing static-validation error
sentinel rather than introducing a new one):

- `streamName == ""` → `fmt.Errorf("%w: streamName is required", ErrInvalidInput)`.
- `consumerPrefix == ""` → `fmt.Errorf("%w: consumerPrefix is required", ErrInvalidInput)`.
  Additionally, `consumerPrefix` must contain only the runes allowed
  by `internal/durable.isAllowedConsumerRune` (a-z, A-Z, 0-9, `-`, `_`);
  same rule and same wording as `consumer.DynamicConfig.Validate`
  (`consumer/dynamic.go:382-385`).
- `subjectTemplate == ""` → `fmt.Errorf("%w: subjectTemplate is required", ErrInvalidInput)`.
  Additionally, the template must contain the `{{.PartitionID}}`
  placeholder and must parse + render to a valid subject — delegate
  to `dynamicbuild.ValidateSubjectTemplate` (a re-export of
  `internal/durable.validateSubjectTemplate`, `subject_utils.go:63-79`).
  Wildcards are allowed (matches runtime).
- `len(partitions) == 0` → `fmt.Errorf("%w: at least one partition is required", ErrInvalidInput)`.
- Any individual partition with no keys → wrapping the runtime error
  from `dynamicbuild.BuildSubjects` (currently `errors.New("partition has no keys")`,
  `worker_consumer.go:576`), unchanged.

Output ordering: deterministic by subject (matches runtime
`buildSubjects`, `worker_consumer.go:559-567`, which sorts subjects
via `slices.Sort`). Two partitions that resolve to the same subject
deduplicate (matches runtime). `PlannedConsumer` entries are emitted
in subject-sorted order.

### `ValidateLiveDynamicConsumers` semantics

- Returns `nil` on success.
- For each `DynamicConsumerCfg`, performs the WorkQueuePolicy
  compatibility check (see §4 below). The recovery-strategy parameter
  passed to the check is **`RecoveryDisabled`** for v1 — the builder
  has no option variadic, so the value mirrors what
  `consumer.Dynamic` would produce with no `WithRecoveryStrategy`
  option. This is the conservative choice: `RecoveryDisabled` and
  `RecoverFromBeginning` always pass `checkWorkQueueRecoveryCompat`
  (`consumer/common.go:187`), so v1 alignment-check will never raise a
  false positive against a WorkQueuePolicy stream.
  Phase 5 (precreation) is where a configurable recovery strategy
  re-enters the picture; v1 documents the default and stops there.
- Returns the **first** error encountered (fail-fast). The error
  is wrapped with the cfg's `StreamName` for operator clarity:
  `fmt.Errorf("dynamic consumer alignment for stream %q: %w", cfg.StreamName, err)`.
- Best-effort connectivity errors: `checkWorkQueueRecoveryCompat`
  swallows transient stream-info fetch failures
  (`consumer/common.go:191-198`); `ValidateLiveDynamicConsumers`
  inherits that behavior verbatim. Operators get the same forgiving
  pre-flight semantics as runtime.

## 4. Live coupling — WorkQueuePolicy compatibility

The runtime function is `consumer.checkWorkQueueRecoveryCompat`
(`consumer/common.go:179-210`), currently unexported and used by all
three runtime consumers (`static.go:236`, `dynamic.go:311`,
`queue.go:280`).

**W4 chosen path: export it.** Rename to
`consumer.CheckWorkQueueRecoveryCompat` (capitalize the leading rune)
and update the three internal callsites. `provision` imports
`consumer` and calls the exported function directly — no shim, no
duplication, no internal indirection.

Rationale:

- Exporting is the minimal-diff path that preserves the master-plan
  invariant *"byte-equivalent error semantics with `Dynamic.Update`"*
  trivially: there is exactly one implementation, in exactly one
  place, and `provision` calls it. The returned error already wraps
  `consumer.ErrInvalidConfig`; provision propagates it unchanged.
- The function is well-scoped, documented, and stable. Exporting it
  costs one Godoc paragraph and changes no behavior at any of the
  three runtime callsites.
- The hard constraint *"any change made for W4 must not regress
  `consumer.Dynamic.Update`"* is satisfied automatically: renaming an
  unexported identifier to exported does not change semantics, and
  the three runtime callsites are mechanically updated.

Alternative paths considered and rejected:

- **Move into `internal/dynamicbuild`**: would force `consumer/` to
  import `internal/dynamicbuild` for what is presently a `consumer/`
  private helper. Net: more code, no benefit.
- **Duplicate in `provision/`**: violates the no-duplication
  invariant. Rejected.

`provision.ValidateLiveDynamicConsumers` returns the
`consumer.CheckWorkQueueRecoveryCompat` error wrapped with the stream
name (see §3). Test coverage pins behavioral equivalence (see §5).

## 5. Test plan (for W4)

All test files land under `provision/` for the public-surface tests
and `internal/dynamicbuild/` for the pure-builder unit tests.

### Pure builder unit tests (`internal/dynamicbuild/*_test.go`)

- **DurableName determinism**: same inputs → same name; subject
  hashing (xxh3) and length truncation match
  `worker_consumer.go:486-494` exactly. Golden table covers
  small-ASCII, unicode (sanitized to `_`), >50-char partition IDs
  (truncation), and collision-prone names.
- **BuildSubjects determinism**: shuffle the input partition slice and
  assert identical sorted, deduplicated output.
- **BuildSubjects template errors**: missing `{{.PartitionID}}`,
  malformed template, partition with zero keys all return matching
  error wraps.
- **ConsumerConfig literal**: assert each of the 12 fields (5 in §2
  in-scope + 7 out-of-scope tunables, given via `Defaults`) matches the
  runtime literal byte-for-byte. This is the seam that prevents
  silent drift between runtime and the extracted helper.

### Provision roundtrip equivalence test (`provision/plan_dynamic_test.go`)

Critical: this is the load-bearing v1 byte-equivalence assertion.

Build the runtime-default golden by exercising the same code
runtime exercises:

```go
runtimeCfg := durable.WorkerConsumerConfig{
    StreamName:      "orders",
    ConsumerPrefix:  "ord-worker",
    SubjectTemplate: "orders.{{.PartitionID}}.>",
}
if err := runtimeCfg.SetDefaults(); err != nil { t.Fatal(err) }
// runtimeCfg now reflects what consumer.Dynamic would carry after
// its own fuda.SetDefaults pass (consumer/dynamic.go:374-381 calls
// SetDefaults via Validate).
expectedConsumerCfg := dynamicbuild.ConsumerConfig(
    dynamicbuild.DurableName(runtimeCfg.ConsumerPrefix, subject, prefix, suffix),
    subject,
    dynamicbuild.Defaults{
        AckPolicy:             runtimeCfg.AckPolicy,
        AckWait:               runtimeCfg.AckWait,
        MaxDeliver:            runtimeCfg.MaxDeliver,
        InactiveThreshold:     runtimeCfg.InactiveThreshold,
        MaxWaiting:            runtimeCfg.MaxWaiting,
        MaxAckPending:         runtimeCfg.MaxAckPending,
        ConsumerMemoryStorage: runtimeCfg.ConsumerMemoryStorage,
        ConsumerReplicas:      runtimeCfg.ConsumerReplicas,
    },
)
```

Then call `provision.PlanDynamicConsumers("orders", "ord-worker",
"orders.{{.PartitionID}}.>", partitions)` and assert, for each
emitted `PlannedConsumer`:

- `Name`, `Durable`, `FilterSubject`, `AckPolicy`, `DeliverPolicy`
  match `expectedConsumerCfg` (the §2 in-scope subset).
- Out-of-scope fields are explicitly **not** asserted (a comment
  marks them as intentionally skipped).

The test must explicitly call `runtimeCfg.SetDefaults()` before
reading defaults; comparing against an un-defaulted zero-value
config is a bug that defeats the purpose of the equivalence
assertion.

### Live coupling integration tests (embedded NATS, `provision/dynamic_live_integration_test.go`)

- **WorkQueuePolicy rejection (negative)**: create a stream with
  `Retention: WorkQueuePolicy`. Call
  `ValidateLiveDynamicConsumers` with a single `DynamicConsumerCfg`
  pointing at the stream. Build a parallel `consumer.Dynamic` with
  `WithRecoveryStrategy(RecoverFromNew)` against the same stream and
  call `Update`. Assert both return errors wrapping
  `consumer.ErrInvalidConfig` and that the messages match the same
  template (modulo the outer stream-name wrap on the provision side).
  Note: the v1 provision builder uses `RecoveryDisabled`, which
  **passes** the WQ check; the test injects `RecoverFromNew`
  via a test-only helper or by exercising the lower-level
  `consumer.CheckWorkQueueRecoveryCompat` directly to prove the
  shared-implementation contract. The shared-function call is what
  pins behavioral equivalence; the provision wrapper is a thin
  forwarder.
- **WorkQueuePolicy with `RecoveryDisabled` (positive)**: the v1
  builder's default strategy. Same stream as above. Assert
  `ValidateLiveDynamicConsumers` returns `nil`.
- **Non-WorkQueuePolicy stream (positive)**: standard
  `InterestPolicy` or `LimitsPolicy` stream with subjects matching
  `subjectTemplate`. Assert `ValidateLiveDynamicConsumers` returns
  `nil`.

### Input-validation tests (`provision/plan_dynamic_validate_test.go`)

- Empty `streamName`, `consumerPrefix`, `subjectTemplate` each return
  the wrapped `ErrInvalidInput`.
- `consumerPrefix` with disallowed characters
  (e.g. `"foo bar"`, `"foo/bar"`) returns the same error wording as
  `consumer.DynamicConfig.Validate` (a string-substring assertion is
  fine).
- `subjectTemplate` missing `{{.PartitionID}}` returns the wrapped
  template-validation error.
- `len(partitions) == 0` returns the wrapped input error.
- A single partition with zero keys returns the wrapped
  `partition has no keys` error from the extracted builder.

### Determinism test

Shuffle a 16-partition input and call `PlanDynamicConsumers` against
each permutation; assert the output `[]PlannedConsumer` slices are
identical (deep-equal).

## 6. Open questions

Each carries a default proposed resolution; the implementer may adopt
the default without further design review unless a reviewer objects.

1. **Should `PlanDynamicConsumers` hard-code `AckPolicy` and
   `DeliverPolicy` rather than relying on Go zero values?**
   *Default: YES.* The pure builder explicitly assigns
   `AckPolicy: jetstream.AckExplicitPolicy` and
   `DeliverPolicy: jetstream.DeliverAllPolicy` in the constructed
   `ConsumerConfig`. Explicit > coincidental; survives any future
   `iota` reordering in nats.go. The runtime literal at
   `worker_consumer.go:461-473` is not modified in W4; that's a
   separate, optional tightening that does not gate this PR.

2. **What recovery strategy does v1 alignment use against
   WorkQueuePolicy streams?**
   *Default: `RecoveryDisabled`.* The W4 builder has no option
   variadic, so the recovery strategy mirrors what
   `consumer.Dynamic` would produce with no `WithRecoveryStrategy`
   option — i.e. the zero value `RecoveryDisabled`
   (`recovery/strategy.go`: zero value). `RecoveryDisabled` and
   `RecoverFromBeginning` both pass the WorkQueuePolicy check
   (`consumer/common.go:186-188`), so v1 alignment-check will never
   raise a false WorkQueuePolicy error. Phase 5 (precreation) is
   where a configurable recovery strategy returns; W4 documents the
   default and stops.

3. **Where do `ErrInvalidInput` and other provision-level error
   sentinels live?**
   *Default: reuse the existing `provision/errors.go` (or the file
   W1/W2 settled on; the implementer reuses whatever sentinel
   `Validate` already uses for static input rejection). No new
   sentinel is introduced in W4.*

4. **Should `Defaults` be exported from `internal/dynamicbuild`?**
   *Default: YES (as `dynamicbuild.Defaults`).* `provision` needs to
   construct one to call `dynamicbuild.ConsumerConfig`. It is an
   internal-package export, not a public-API export, so the
   compatibility surface is internal to the module.

5. **Does W4 modify the secondary `ConsumerConfig` literal at
   `worker_consumer.go:414-426`?**
   *Default: YES, rewrite that callsite to use
   `dynamicbuild.ConsumerConfig` with the same `Defaults`.* This
   prevents the two literals from drifting silently. Behavior is
   unchanged at the runtime layer; the only observable effect is one
   construction call instead of two literal copies.
