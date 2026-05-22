# Provision SDK / partictl — Phase 6: Force + Repair

This plan is **Phase 6** of the
[Phased Roadmap](../provision-sdk-cli/00-implementation-plan.md#phased-roadmap)
in the master plan. Phases 1-5 built a `provision/` SDK and `cmd/partictl` CLI
that, under the `warn` / `adopt` / `safe-update` reconcile policies,
create-missing resources, report drift, and reconcile *mutable* drift in place
— but **never delete or recreate** a resource. Phase 6 adds the gated
**destructive-repair** path: a `force` reconcile policy plus a per-resource
`allowDeleteRecreate` opt-in that together let `apply` delete and recreate an
ownership-proven resource whose drift is `drift-immutable`.

## What Ships After Phase 6

- The `force` reconcile policy is accepted (it was statically rejected in
  Phases 1-5). `force` is a strict superset of `safe-update`: it does
  everything `safe-update` does (create missing, reconcile drift-mutable
  fields in place) **and** repairs `drift-immutable` resources by
  delete/recreate.
- Each provisioned-resource config struct gains an `allowDeleteRecreate`
  boolean. Delete/recreate happens only when **both** layers opt in: the
  `force` policy **and** `allowDeleteRecreate: true` on that specific
  resource. Either layer alone leaves the resource reported-only, exactly as
  in Phases 1-5.
- `provision.Plan` / `Apply` emit a new `recreate-kv` / `recreate-stream` /
  `recreate-consumer` action for an ownership-proven resource (marked, for KV
  buckets and streams; config-derived, for consumers) with `drift-immutable`
  drift when both opt-in layers are satisfied. The action re-reads, deletes,
  and recreates the resource at the desired config.
- `partictl plan` / `apply` / `stream` accept `-policy force`; `partictl
  consumers` accepts `-policy force` (and only `force` — the other policy
  values remain meaningless for consumer precreation).

Phase 7 (K8s Controller) is the final phase; it consumes the SDK and adds no
new provisioning primitive.

## Background: The Destructive-Repair Gap

Ground truth from the current codebase (verified at the cited lines):

- **The `force` policy is reserved but rejected.** `provision/policy.go`
  defines `reservedPolicyForce = "force"`; `validateResolved`
  (`provision/validate.go`) returns `ErrInvalidConfig` "policy %q is not yet
  supported" for it. The `ReconcilePolicy` type and the `PolicyWarn` /
  `PolicyAdopt` / `PolicySafeUpdate` constants are in `policy.go`.
- **`drift-immutable` is reported, never acted on.** Four classifiers emit
  `SeverityDriftImmutable` findings and explicitly emit no action:
  - `classifyControlPlaneDrift` (`provision/plan.go`) — immutable fields:
    `Storage`, `History`, and the `parti.io/component` marker.
  - `classifyPartitionSourceDrift` (`provision/partition_source.go`) —
    immutable: `Storage`, `History`, `component`.
  - `classifyStreamDrift` / `routeStreamFieldDrift`
    (`provision/plan_streams.go`) — immutable: `Storage`, `Retention`,
    `component`.
  - `classifyConsumer` / `checkConsumerIdentityFields`
    (`provision/consumer_records.go`) — immutable: `FilterSubject`,
    `AckPolicy`, `DeliverPolicy`, `MaxWaiting`, `MemoryStorage`.
- **`safe-update` gates its `update-*` actions** with the pattern
  `cfg.Policy == PolicySafeUpdate && marked` — in `planControlPlane`
  (`provision/plan.go`), `planPartitionSource` (`provision/partition_source.go`),
  and `planStreams` (`provision/plan_streams.go`). Phase 6's `force` gating
  mirrors this exactly, with the per-resource flag added.
- **nats.go delete APIs.** `js.DeleteKeyValue(ctx, bucket)` deletes the
  `KV_<bucket>` stream and every entry. `js.DeleteStream(ctx, name)` deletes
  the stream, all its messages, and (cascade) every consumer bound to it.
  `js.DeleteConsumer(ctx, stream, durable)` deletes the consumer and its
  delivery / ack cursor.
- **Control-plane bucket storage** (`buildControlPlaneSpecs`,
  `provision/plan.go`): `parti-stableid`, `parti-assignment`,
  `parti-handoff` are `FileStorage` (durable state); `parti-election`,
  `parti-heartbeat` are `MemoryStorage` (transient). The handoff bucket is
  opt-in via `ControlPlaneConfig.EnableTwoPhaseHandoff`.
- **Cursor loss on consumer recreate.** Deleting a dynamic consumer discards
  its delivery / ack cursor. The recreated consumer is built with
  `DeliverAllPolicy` (the Phase-5 builder default), so without runtime
  recovery handling it would replay every retained message. The runtime
  `consumer` package has recovery strategies (`RecoverFromNew`,
  `RecoverFromLastProcessed`, …) precisely for the "consumer was recreated"
  case; `RecoverFromNew` makes a recreated consumer skip already-published
  history. This is the cursor-loss concern Phase 6's test discipline
  addresses.

## Invariants Inherited from Phases 1-5

Every invariant from the
[Phase 1 list](../provision-sdk-cli/00-implementation-plan.md#invariants-inherited-by-every-phase)
continues to hold. The load-bearing ones for Phase 6:

- **No destructive default.** The Phase 1 Non-Goal — "do not auto-delete or
  recreate existing streams, buckets, or consumers by default" — is *honored*,
  not lifted. Phase 6 makes delete/recreate reachable, but only behind **two
  independent, explicit opt-ins** (the `force` policy and the per-resource
  `allowDeleteRecreate` flag). The default of every existing config is
  unchanged: no `force`, no `allowDeleteRecreate`, no destruction.
- **Ownership proof.** `recreate-*` operates only on resources that are
  provably Parti's to repair, but the *proof* differs by kind:
  - **KV buckets and application streams** carry the Parti ownership marker.
    `recreate-kv` / `recreate-stream` gate on `&& marked` — an unmarked
    resource under `force` still surfaces as `adopted` drift and is never
    deleted; the marker is the proof. The recreated resource is stamped with
    the marker (it is built by the same builders Phases 1-5 use).
  - **Dynamic consumers carry no marker** — Phase 5 deliberately stamps none
    (the runtime's `CreateOrUpdateConsumer` would strip it). For consumers
    the ownership proof is **config derivation**: `PlanConsumers` only ever
    produces a `PlannedConsumer` for a durable name it itself derived, via
    `PerSubjectDurableName`, from a precreation-opted (`PartitionsRef`
    non-empty) `DynamicConsumerCfg` in the operator's config. A
    `recreate-consumer` is therefore intrinsically config-owned — it can only
    target a durable the operator's config declares. There is no separate
    `marked` check for the consumer planner; the config-derivation is the
    proof. A consumer the config does not declare is never seen by
    `PlanConsumers` and so is never a `recreate-consumer` target.
- **JSON output envelope** stays `apiVersion: parti.io/provision/v1`.
  Phase 6 adds three new `PlannedAction.Kind` values — `recreate-kv`,
  `recreate-stream`, `recreate-consumer` — and one new `ReconcilePolicy`
  string value, `force`. All additive; no existing value changes.
- **Input config** `apiVersion: parti.io/v1` accepts additive fields.
  Phase 6 adds an `allowDeleteRecreate` field to four config structs; a
  config omitting it loads with no behavior change (zero value `false`).
- **CLI exit codes and precedence** (`cmd/partictl/exitcodes.go`) are stable:
  `0` ok, `1` runtime, `2` drift, `3` validation, `4` NATS. No new codes — a
  failed `recreate-*` is a resource-level apply failure → exit `1`.
- **`Plan` action and drift ordering remains deterministic** — `(Kind,
  Name)`. The new `recreate-*` kinds sort into the same total order.

## Non-Goals (Phase 6)

- **Do not delete/recreate by default.** Both opt-in layers are mandatory.
  See the inherited "no destructive default" invariant.
- **Do not check for live workers / quiesce the cluster.** `provision` is a
  provisioning tool, not a cluster orchestrator. `apply -policy force` against
  a resource holding live coordination state (`parti-assignment`, an
  application stream mid-workload) **will** disrupt a running cluster — that
  is the operator's responsibility. Phase 6 does **not** probe for active
  workers, attempt to drain them, or refuse the operation when the cluster is
  live. The W5 docs state plainly: quiesce workers before `apply -policy
  force`. Adding quiescence detection would be a half-built cluster manager.
- **Do not preserve resource contents across a recreate.** A `recreate-kv`
  loses every KV entry; a `recreate-stream` loses every message and
  cascade-deletes bound consumers; a `recreate-consumer` loses the cursor.
  Phase 6 does not snapshot-and-restore. `allowDeleteRecreate: true` is the
  operator's explicit acceptance of that loss. (An operator who needs the
  partition table preserved across a partition-source-bucket recreate
  re-runs `partictl partitions apply` afterward — the table lives in config.)
- **Do not add a `partictl recreate` shorthand command.** `adopt` exists as a
  shorthand because adoption is routine and safe. Force-recreate is neither;
  it stays spelled out as `-policy force` so it is never the path of least
  resistance.
- **Do not add a separate `--force` flag.** The opt-in is the `force`
  *policy* value, uniform across `plan` / `apply` / `stream` / `consumers`.
  A parallel `--force` flag would be a second destructive-opt-in surface.
- **Do not weaken any drift classification.** The four classifiers keep
  emitting the exact same `drift-immutable` findings. Phase 6 adds an
  *action* alongside the finding under `force`; it changes no severity.

## Design

The organizing decision (settled with the advisor): **one uniform mechanism,
uniformly gated.** For any resource kind, `apply` emits a `recreate-*` action
when, and only when, all of:

1. the reconcile policy is `force`;
2. the resource is **ownership-proven** — `marked` for a KV bucket or a
   stream, *config-derived* for a consumer (see the "Ownership proof"
   invariant above);
3. the resource carries `drift-immutable` drift; and
4. that resource's config has `allowDeleteRecreate: true`.

No per-kind carve-outs in the mechanism — the two explicit operator opt-in
layers (`force` + `allowDeleteRecreate`) are the entire safety model; the
ownership proof is structural, not an operator knob. The phase does not
second-guess an operator who has set both opt-ins.

### W1 — The `force` policy and the `allowDeleteRecreate` opt-in

**`provision/policy.go`** — add the policy constant:

```go
// PolicyForce is a strict superset of PolicySafeUpdate: it create-misses and
// reconciles drift-mutable fields in place exactly as safe-update does, and
// additionally repairs a drift-immutable resource by delete/recreate — but
// only for an ownership-proven resource (a Parti-marked KV bucket or stream,
// or a config-derived dynamic consumer) whose config sets
// allowDeleteRecreate: true. Destructive and gated; see the Phase 6 plan.
PolicyForce ReconcilePolicy = "force"
```

Delete the `reservedPolicyForce` constant (it is now a real policy).

**`provision/validate.go`** — `validateResolved`'s policy switch accepts
`PolicyForce` alongside `PolicyWarn` / `PolicyAdopt` / `PolicySafeUpdate`;
the `case reservedPolicyForce` rejection is removed. Update the `Validate`
docstring.

**`provision/config.go`** — add `AllowDeleteRecreate bool` to each
provisioned-resource config struct, with `yaml:"allowDeleteRecreate,omitempty"
json:"allowDeleteRecreate,omitempty"` tags and a godoc line:

- `ControlPlaneConfig` — one flag governs **all five** control-plane buckets
  (the struct already treats them uniformly: `Replicas` applies to all five,
  there are no per-bucket sub-structs). A per-bucket flag is not added —
  control-plane buckets are provisioned as a set.
- `PartitionSourceConfig` — one flag for the partition-source bucket.
- `StreamCfg` — one flag per declared stream (the list is per-stream).
- `DynamicConsumerCfg` — one flag per dynamic-consumer target; it governs
  every per-partition durable that target precreates.

`allowDeleteRecreate` needs no validation rule of its own (a bool is always
valid). The W2/W3 gating reads it; an operator who sets it without `force`
gets no destruction (the policy layer is still closed).

**`musttag`**: every struct here is `yaml.Unmarshal`-reachable, so the `yaml`
tag is mandatory (same constraint Phases 3-5 hit).

### W2 — `Plan`: emit `recreate-*` actions

**`provision/types.go`** — new action-kind constants and resource types:

```go
const (
    ActionRecreateKV       = "recreate-kv"
    ActionRecreateStream   = "recreate-stream"
    ActionRecreateConsumer = "recreate-consumer"
)
```

Each `recreate-*` action carries a resource recording the audit surface — the
live `Before`, the desired `After`, and the `drift-immutable` field set that
triggered the repair:

```go
// RecreateKVResource is the Resource on an ActionRecreateKV PlannedAction.
// Before is the live KeyValueConfig at plan time; After is the desired
// config Apply recreates the bucket as; ImmutableDrift names the
// drift-immutable fields that triggered the repair (the audit surface).
type RecreateKVResource struct {
    Before        jetstream.KeyValueConfig `json:"before"`
    After         jetstream.KeyValueConfig `json:"after"`
    ImmutableDrift map[string]any          `json:"immutableDrift"`
}
// RecreateStreamResource — the jetstream.StreamConfig analogue.
// RecreateConsumerResource — Before jetstream.ConsumerConfig, After
//   PlannedConsumer, ImmutableDrift map[string]any.
```

(`recreate-kv` covers both control-plane buckets and the partition-source
bucket — both are KV buckets. The desired `After` is built by the **same
builder the corresponding `create-kv` path uses**, and the two are different:
a control-plane bucket uses `kvbuckets.BuildKeyValueConfig` + `BuildMarker`
(`newCreateKVAction`, `provision/plan.go`); the partition-source bucket uses
the deliberately-separate `buildPartitionSourceKVConfig` +
`BuildMarker(ComponentPartitionSource, …)` (`provision/partition_source.go`),
because its `Storage` / `History` / `Replicas` / `MaxValueSize` are
operator-configurable. `recreate-kv` for the partition-source bucket MUST use
`buildPartitionSourceKVConfig`, not the control-plane builder, or those
operator-set fields are silently lost.)

**The gating helper.** `force` is a superset of `safe-update`, so the
existing `update-*` gates widen from `cfg.Policy == PolicySafeUpdate` to a
helper `policyReconcilesMutable(cfg.Policy)` returning true for both
`PolicySafeUpdate` and `PolicyForce`. A second helper
`policyRecreatesImmutable(cfg.Policy)` returns true only for `PolicyForce`.

**Per-planner change.** `planControlPlane`, `planPartitionSource`,
`planStreams`, and the consumer planner (`PlanConsumers` /
`classifyConsumer`) each restructure their post-classify emission so the
recreate branch and the update branch are **mutually exclusive** per
resource. The current planners append an `update-*` action whenever
`cfg.Policy == PolicySafeUpdate && marked`, *independent of* any immutable
drift; W2 must NOT simply widen that gate and then append recreate after it
(that co-emits both). The explicit ordered logic per resource:

```text
classify the resource → findings   (always; findings populate out.Drift)
hasImmutable := findings contain a drift-immutable finding
recreateOK   := policyRecreatesImmutable(cfg.Policy)
               && <ownership-proven: marked for kv/stream;
                   config-derived for consumer>
               && <this resource>.AllowDeleteRecreate

if hasImmutable && recreateOK:
    emit ONE recreate-<kind> action; do NOT emit any update-<kind>
    for this resource — the recreate produces the fully-desired
    resource, so a co-emitted update would be redundant and could
    race the recreate.
else if policyReconcilesMutable(cfg.Policy) && <ownership-proven>:
    emit update-<kind> if the mutable target differs (the existing
    safe-update path, with the gate widened to also accept force).
# A resource with hasImmutable but NOT recreateOK falls through to
# the else: it gets update-* for any mutable drift, and the
# drift-immutable finding still surfaces — reported, not repaired.
```

Where `policyRecreatesImmutable` returns true only for `PolicyForce`, and
`policyReconcilesMutable` returns true for both `PolicySafeUpdate` and
`PolicyForce` (`force` ⊇ `safe-update`).

The drift findings always surface in `out.Drift` regardless of policy — the
`recreate-*` action and the `drift-immutable` finding **coexist**, exactly as
`update-kv` and its drift finding coexist under `safe-update`. The `Resource`
on the emitted action is `Recreate<Kind>Resource{Before, After,
ImmutableDrift: <the immutable finding's Detail>}`.

`recreate-*` actions sort by `(Kind, Name)` with the rest.

### W3 — `Apply`: execute `recreate-*`

Each `recreate-*` is **one action, atomic from the apply-loop's view**: a
single helper performs re-read → delete → create internally and returns one
`ExecutedAction` or one fail-fast error. This mirrors `applyUpdateKVAction`'s
shape (one action, multi-step internals) and avoids the partial-failure
ambiguity of a separate `delete-*` + `create-*` pair.

**Seam extensions.** The Phase-4 `streamManager` gains `DeleteStream(ctx,
name) error`; the Phase-5 `consumerManager` gains `DeleteConsumer(ctx,
stream, durable) error`. For KV there is no unified seam today (`create-kv`
calls `js.CreateKeyValue` directly, `update-kv` uses `streamReader` /
`kvUpdater`); W3 adds a small `kvRecreator` seam — `StreamInfo`,
`DeleteKeyValue`, `CreateKeyValue` — so the `recreate-kv` step ordering is
unit-testable without a live server, consistent with how Phases 2-5 made
every apply path seam-testable.

**`applyRecreate<Kind>Action(ctx, seam, action) (ExecutedAction, error)`**
— for each kind:

1. **Re-read** live state. Resource gone already (`ErrStreamNotFound` /
   `ErrBucketNotFound` / `ErrConsumerNotFound`) → treat as the create half
   only: proceed to step 4 (the resource is absent; recreating is just
   creating). A re-read error other than not-found → fail-fast. *(This
   resource-gone branch is also how a re-applied stale plan recovers from a
   step-5 post-delete interruption — see step 5 and the recovery note below.)*
2. **Re-classify / no-op short-circuit.** Re-classify the re-read live state.
   If it no longer carries the `drift-immutable` divergence the plan recorded
   (a concurrent operator already repaired it, or the live config now
   matches), record `ExecutedAction{Kind, Name, Raced: true}` and **do not
   delete**. This closes the **plan-time → apply-time** staleness window —
   the common case, where the operator's `force` plan is minutes or hours old
   and the environment moved on. It is a **best-effort** guard, **not** a
   race-free guarantee: see "Stale-plan safety — honest scope" below.
3. **Delete** the live resource (`DeleteKeyValue` / `DeleteStream` /
   `DeleteConsumer`).
4. **Create** the resource at the desired `After` config (the same
   builder path the corresponding `create-*` action uses).
5. **The point of no return is the delete.** Any failure *after* step 3
   succeeds — a step-4 create error, a context cancellation, anything — is
   **not** an ordinary cancellation or skip: the live resource is gone.
   The helper returns a fail-fast non-cancellation error that wraps the
   sentinel `ErrRecreateInterrupted`, names the deleted resource, and states
   the operator must fix any persistent cause and re-run `apply`. It
   **must not** wrap a `context.Canceled` / `DeadlineExceeded` with `%w`
   (that would make `errors.Is(err, context.Canceled)` true and misroute it
   through `foldActionResult`'s pure-cancellation branch, which records no
   `ResourceError`); the cancellation cause is included as message text only.
   `foldActionResult` then records it as a `ResourceError` and skips the
   remainder with `prior-error` → CLI exit `1`. `Aborted` stays false: a
   post-delete interruption is a resource-level failure, not a clean abort.

**Stale-plan safety — honest scope.** NATS exposes no compare-and-delete /
generation-token primitive — `DeleteKeyValue` / `DeleteStream` /
`DeleteConsumer` are unconditional name-based operations. The step-2 re-read
therefore cannot make a destructive apply perfectly race-free: a concurrent
operator who repairs the resource in the narrow re-read→delete window (a few
milliseconds) is not detected, and the stale apply would delete the
repaired resource. Phase 6 does **not** build a distributed provision lock to
close that window — `provision` is a human-driven CLI tool, and the realistic
threat (a stale plan minutes/hours old) is fully covered by the re-read.
Running **two concurrent `force` applies against the same environment is
unsupported** and an operator error; the W5 docs say so. This is the same
"honest scope" posture Phase 3's `ApplyPartitions` takes for its single-CAS
contract.

**Interrupted-recreate recovery.** After a step-5 interruption the resource
is deleted and the desired resource does not exist. Recovery is to re-run
`apply`, having first fixed any *persistent* create-failure cause
(permissions, quota, an invalid desired config — a blind re-run does not fix
those). Two recovery paths, both sound: the **CLI** (`partictl apply` /
`stream apply` / `consumers apply`) re-runs `Plan` / `PlanConsumers` first,
which sees the resource missing and emits an ordinary `create-*` /
`create-consumer` action — no `recreate-*`, no re-deletion; the **SDK
`applyPlan(precomputedPlan)`** path, if a caller re-applies the *same* stale
plan, re-enters `applyRecreate*Action` whose step 1 finds the resource gone
and completes as create-only. Either way the repair finishes; neither
re-deletes anything.

Wire the three new cases into `applyPlan`'s `switch` (control-plane,
partition-source, stream) and into `ApplyConsumers`'s loop (consumer),
folding via the existing `foldActionResult` — which works unchanged because
step 5 deliberately returns a non-cancellation error for any post-delete
failure. A cancellation *before* the delete (step 1/2) is an ordinary clean
abort (`Aborted: true`, nothing destroyed); a cancellation *after* the delete
is the step-5 path (`ResourceError`, `ErrRecreateInterrupted`, exit `1`).

### W4 — CLI: `-policy force`

- `cmd/partictl/policy.go` `validatePolicyFlag` (the CLI-side policy-string
  check) accepts `force` alongside `warn` / `adopt` / `safe-update`.
- `partictl plan` / `apply` / `stream` already pass `-policy` through to
  `Config.Policy`; once `validatePolicyFlag` and `validateResolved` accept
  `force`, they accept `-policy force` with no further change.
- `partictl consumers` (both `consumers plan` and `consumers apply`) gains a
  `-policy` flag. Resolution mechanics — explicit so the implementer does not
  guess:
  - The flag is resolved the same way `plan` / `apply` / `stream` resolve
    theirs: via the existing `resolveAndStampPolicy` helper
    (`cmd/partictl/policy.go`), which reconciles the `-policy` flag against a
    YAML `policy:` field and stamps the effective value into `cfg.Policy`
    before `PlanConsumers` runs. `consumers` does **not** invent a separate
    policy path.
  - After resolution, `consumers` accepts only an effective `cfg.Policy` of
    empty / `warn` (the default — ordinary precreation, unchanged from
    Phase 5) or `force` (enables `recreate-consumer`). An effective `adopt`
    or `safe-update` → `ExitValidation` with a clear message: those policies
    are meaningless for consumer precreation (Phase 5 made `consumers`
    policy-independent for create-missing; `force` is the one policy that
    *changes* `consumers` behavior). This check happens whether the value
    came from the flag or from YAML `policy:`.
  - The resolved `cfg.Policy` threads into `PlanConsumers` → the consumer
    planner's recreate gate, exactly as it threads into the other planners.
- `run.go` usage text: document `force` in the `-policy` line and note it is
  the destructive policy; the existing "Not accepted by partitions or
  consumers" note narrows to "Not accepted by partitions; `consumers` accepts
  only `force`."
- `output.go` text renderers already print arbitrary action `Kind` strings
  generically — the `recreate-*` kinds need no renderer change (verify).
- Exit codes route through the existing `classifyError` — no `exitcodes.go`
  change.

### W5 — Documentation

- `docs/PROVISION.md`: a new "Force + Repair" section — the `force` policy as
  a `safe-update` superset, the per-resource `allowDeleteRecreate` opt-in,
  the **two-layer gating**, and a prominent **destructive-consequences**
  callout: `recreate-kv` loses all KV entries; `recreate-stream` loses all
  messages and cascade-deletes bound consumers; `recreate-consumer` loses the
  cursor (and how the runtime recovery strategies — `RecoverFromNew` — relate
  to that). State plainly: **quiesce workers before `apply -policy force`**;
  `provision` does not check for a live cluster. State the **stale-plan
  honest scope**: the apply-time re-read closes the plan-staleness window but
  NATS has no compare-and-delete, so **two concurrent `force` applies against
  the same environment are unsupported**. Document the **interrupted-recreate
  recovery**: if a recreate fails after the delete, fix any persistent cause
  and re-run `apply`. Add a TOC entry. Update the "Reconcile Policies"
  section to add `force`.
- Package godoc for `PolicyForce`, the `allowDeleteRecreate` fields, the
  `ActionRecreate*` constants, the `Recreate*Resource` types, and
  `ErrRecreateInterrupted`.
- `CHANGELOG.md`: a new `[Unreleased]` entry for the Phase 6 surface.

## Work Items

| ID | Scope | Impl model | Review effort |
|----|-------|------------|---------------|
| W1 | `PolicyForce`; accept `force` in validation; `allowDeleteRecreate` on the four config structs | sonnet | high |
| W2 | `recreate-*` action/resource types; the `policyRecreatesImmutable` gating in the four planners; emit-only-recreate-on-immutable-drift rule | sonnet | xhigh |
| W3 | `recreate-*` apply: seam `Delete*` extensions, the `kvRecreator` seam, the three `applyRecreate*Action` helpers, `applyPlan` / `ApplyConsumers` wiring, `ErrRecreateInterrupted` | opus | xhigh |
| W4 | `-policy force` CLI wiring (`validatePolicyFlag`, the `consumers` `-policy` flag, usage text) | sonnet | high |
| W5 | `docs/PROVISION.md`, godoc, `CHANGELOG.md` | sonnet | high |

Per-work-item loop (unchanged from Phases 1-5): implement → `/simplify` →
codex post-impl review → fix every P0/P1 → re-verify `go build ./...`,
`make lint`, package tests → commit. W2 (the gating correctness — two opt-in
layers, the emit-only-recreate rule) and W3 (destructive apply, the
delete-succeeded-create-failed window, the stale-plan no-op gate) are the
sharp items and carry `xhigh` review effort.

## Test Plan

Each invariant has an encoding:

- **W1 validation:** `force` is accepted by `Validate`; an
  `allowDeleteRecreate: true` config loads and round-trips through YAML; a
  config omitting `allowDeleteRecreate` is unchanged (zero value `false`);
  the four structs all carry the field.
- **W2 the two-layer gate (per resource kind — KV bucket, partition-source,
  stream, consumer):**
  - `force` policy + `allowDeleteRecreate: true` + an ownership-proven
    resource (marked, for kv/stream; config-derived, for consumer) with
    `drift-immutable` drift → exactly one `recreate-*` action, plus the
    `drift-immutable` finding (they coexist), and **no** `update-*` action
    for that resource even if it also has mutable drift (the
    emit-only-recreate rule).
  - `force` policy + `allowDeleteRecreate` **false/omitted** + immutable
    drift → **no** `recreate-*` action; the `drift-immutable` finding still
    surfaces (the per-resource layer is load-bearing).
  - `allowDeleteRecreate: true` + policy **not** `force` (`warn` /
    `safe-update`) + immutable drift → **no** `recreate-*` action (the policy
    layer is load-bearing).
  - **kv/stream:** `force` + `allowDeleteRecreate: true` + an **unmarked**
    resource → no `recreate-*` (still `adopted` drift; the marker gate holds).
  - **consumer:** `recreate-consumer` is emitted only for a durable
    `PlanConsumers` derives from a precreation-opted `DynamicConsumerCfg`
    (`PartitionsRef` non-empty); a consumer not declared by config is never a
    `recreate-consumer` target (the config-derivation ownership proof).
  - `force` on a resource with only **mutable** drift → `update-*` only, no
    `recreate-*` (force ⊇ safe-update).
  - deterministic `(Kind, Name)` ordering with `recreate-*` interleaved.
- **W3 apply (via the seams, no live server):**
  - clean `recreate-kv` / `recreate-stream` / `recreate-consumer`: re-read →
    delete → create; the result re-reads as the desired `After`.
  - **stale-plan no-op:** the re-read live state no longer has the immutable
    drift → `Raced: true`, **no delete** (the plan-staleness window guard).
  - resource already gone at re-read → recreate proceeds as create-only.
  - **delete-succeeded / create-failed:** the create step errors → a
    fail-fast `ResourceError` wrapping `ErrRecreateInterrupted`, `Aborted`
    false, message states the resource was deleted, CLI exit `1`; a re-run
    (after fixing any persistent cause) completes the repair.
  - context cancellation **before** the delete → ordinary clean abort
    (`Aborted: true`, nothing destroyed); cancellation **after** the delete →
    a fail-fast `ResourceError` wrapping `ErrRecreateInterrupted`, `Aborted`
    false, exit `1` (the error does NOT satisfy `errors.Is(_, context.Canceled)`
    — it must not misroute through `foldActionResult`'s cancellation branch).
  - wrong `Resource` concrete type → fail-fast.
- **W3 consumer cursor-loss discipline (the roadmap's named test):** a live
  integration test — precreate a consumer, publish + consume messages, then
  `recreate-consumer` it; simulate the runtime rebinding with the
  `RecoverFromNew` strategy; assert post-recreate publishes are received and
  pre-recreate messages are **not** replayed. This encodes what an operator
  must understand before setting `allowDeleteRecreate` on a consumer.
- **W3 stream destructive-consequences (documentation test):** a live
  integration test that `recreate-stream`s a stream carrying messages and a
  bound consumer, and asserts the messages and the consumer are gone after —
  the test exists to *document and lock* the blast radius, not as a
  regression guard.
- **W4 CLI:** `partictl apply -policy force` and `partictl stream apply
  -policy force` accepted; `partictl consumers apply -policy force` accepted;
  `partictl consumers apply -policy safe-update` → `ExitValidation`;
  `partictl partitions` still rejects `-policy` entirely; a `recreate-*`
  action renders in text and JSON output with the `apiVersion` envelope.
- **Integration end-to-end:** provision a stream with `storage: file`, change
  the config to `storage: memory` + `allowDeleteRecreate: true`, run
  `partictl stream apply -policy force`, and assert the stream is recreated
  as memory storage and re-reads marked.

## Open Design Decisions

Surfaced for `plan-review`; the rest of the plan assumes the stated choice.

1. **Uniform mechanism, uniform two-layer operator gate; kind-specific
   ownership proof.** Chosen: `force` policy + per-resource
   `allowDeleteRecreate` enables `recreate-*` for **any** resource kind with
   `drift-immutable` drift; no per-kind behavior in the operator-facing
   mechanism. The ownership proof is structural and differs by kind — the
   Parti marker for KV buckets and streams, config-derivation for consumers
   (which carry no marker; see the "Ownership proof" invariant). The two
   explicit operator opt-ins are the whole safety model. Rejected:
   restricting delete/recreate to consumers (the least-catastrophic kind) —
   the roadmap's per-resource `allowDeleteRecreate` and the operator's
   deliberate two-layer choice argue for honoring intent uniformly.
2. **`force` is a `safe-update` superset.** Chosen: under `force`, mutable
   drift still gets `update-*` and immutable drift gets `recreate-*`.
   Rejected: `force` doing *only* recreate (would silently stop reconciling
   mutable drift, a surprising regression from `safe-update`).
3. **Emit only `recreate-*` for a resource with any immutable drift.**
   Chosen: a resource with both mutable and immutable drift gets a single
   `recreate-*` (the recreate produces the fully-desired resource; a
   co-emitted `update-*` would be redundant and could even race the
   recreate). Rejected: emitting both and ordering them in Apply.
4. **One `recreate-*` action per kind, atomic re-read→delete→create.**
   Chosen: a single action whose helper does all three steps. Rejected: a
   `delete-*` + `create-*` pair — two apply-loop actions that can partial-fail
   into "deleted, not recreated" with no single owning `ResourceError`.
5. **`consumers` accepts `-policy force` only.** Chosen: `force` is the one
   policy value that changes `consumers apply` behavior, so `consumers` takes
   a `-policy` flag accepting only `force`. Rejected: a separate `--force`
   flag (a second destructive-opt-in surface, breaks symmetry with the other
   three commands); rejected: leaving `consumers` policy-free and
   unreachable by force-recreate.
6. **No live-cluster / quiescence checks.** Chosen: `provision` honors the
   operator's two-layer opt-in and does not probe for running workers;
   the docs own the "quiesce first" instruction. Rejected: refusing
   `recreate-*` when the resource looks live — provision is not a cluster
   orchestrator and a half-built liveness check would give false assurance.
7. **Stale-plan safety is best-effort, not race-free.** Chosen: the
   apply-time re-read no-op check closes the plan-time→apply-time staleness
   window; the narrow re-read→delete window cannot be closed because NATS
   exposes no compare-and-delete primitive, so concurrent `force` applies
   against the same environment are documented as unsupported. Rejected:
   building a distributed provision lock — disproportionate for a
   human-driven CLI tool, and the realistic stale-plan threat is fully
   covered by the re-read. Mirrors Phase 3 `ApplyPartitions`'s honest-scope
   posture for its single-CAS contract.
8. **No content preservation.** Chosen: `recreate-*` destroys resource
   contents; `allowDeleteRecreate: true` is the operator's acceptance.
   Rejected: snapshot/restore — out of scope, and the partition table (the
   one recreatable-bucket payload that matters) is already reconstructable
   from config via `partictl partitions apply`.
