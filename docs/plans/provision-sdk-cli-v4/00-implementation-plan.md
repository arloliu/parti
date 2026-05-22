# Provision SDK / partictl — Phase 4: Streams

This plan is **Phase 4** of the
[Phased Roadmap](../provision-sdk-cli/00-implementation-plan.md#phased-roadmap)
in the master plan. Phases 1-2 built the `provision/` SDK and `cmd/partictl`
CLI that provision the NATS JetStream KV **buckets** Parti depends on (the
control-plane buckets and the partition-source bucket). Phase 3 added
management of the partition-source key **contents** (the partition records).
Phase 4 manages the **application JetStream streams** — the streams that carry
the partitioned work messages Parti's partition-aware consumers read from.

## What Ships After Phase 4

- Operators can declare application streams inline in `parti-env.yaml` under a
  new `streams:` block and run `partictl plan -f parti-env.yaml` /
  `partictl apply -f parti-env.yaml` to provision them, exactly as the
  control-plane and partition-source buckets are provisioned today. A config
  that omits `streams:` behaves identically to today (the field is additive).
- The reconcile-policy ladder governs streams uniformly: `warn` creates
  missing streams and reports drift; `safe-update` additionally reconciles
  drift-mutable fields in place on Parti-marked streams; `adopt` stamps the
  Parti ownership marker on an unmarked stream named by config.
- `partictl view` lists Parti-marked application streams alongside the
  control-plane and partition-source buckets.
- A new two-level command `partictl stream view|plan|apply` provides a
  stream-scoped surface over the same SDK — useful when an operator owns the
  application streams but not the control plane, or wants to plan streams in
  isolation.
- The SDK surface (`provision.Plan`, `provision.Apply`, `provision.View`,
  `provision.ValidateLive`) processes streams natively as one more `Config`
  section; no new top-level SDK entrypoint is added.

Subsequent phases keep their own surface: dynamic consumer precreation
(Phase 5), destructive repair (Phase 6), Kubernetes controller (Phase 7).

## Background: Application Streams in Parti

Ground truth from the current codebase (verified at the cited lines):

- **The Parti runtime never creates application streams.** Parti's
  partition-aware consumers (`consumer.Dynamic`, `internal/durable`,
  `internal/ipartition/consumer.go`) bind to a stream **by name** and never
  create it. The stream-creation utility `jsutil.EnsureStream`
  (`jsutil/stream.go:14-43`) is a public helper for *application* code; it
  takes a caller-supplied `jetstream.StreamConfig` and is not called anywhere
  in the runtime manager, `internal/durable`, `internal/dynamicbuild`, or
  `internal/ipartition`. The only `js.UpdateStream` call in the runtime
  (`manager_setup.go:180-183`, `handoffStreamUpdate`) operates on a **KV
  bucket's** backing stream, not an application stream.
- **Consequence — no byte-equivalence obligation, no W0.** Phases 1 and 3
  each began with a W0 refactor that extracted a shared builder/codec
  (`internal/kvbuckets.BuildKeyValueConfig`, `internal/partcodec`) because
  the runtime *also* writes the same resource and the two writers had to
  stay byte-identical. **Phase 4 has no such obligation**: there is no
  runtime application-stream builder to be equivalent to. `jsutil.EnsureStream`
  accepts a fully-formed `jetstream.StreamConfig` — it contains no
  config-construction logic. The `StreamCfg` → `jetstream.StreamConfig`
  mapping is entirely `provision`'s own concern. Phase 4 therefore has **no
  W0 work item**.
- **`jetstream.StreamConfig` carries `Metadata`.** Like
  `jetstream.KeyValueConfig`, `jetstream.StreamConfig` has a
  `Metadata map[string]string` field. The Parti ownership marker
  (`parti.io/managed`, `parti.io/component`, `parti.io/instance`) is stamped
  there, exactly as it is on KV buckets — informational, never authorizing
  mutation.
- **Stream identity is its `Name`.** Unlike KV buckets, whose backing stream
  is named `KV_<bucket>`, an application stream's NATS stream name *is* its
  config `Name` — there is no prefix. Resource lookup is `js.Stream(ctx,
  cfg.Name)`.
- **Subjects.** A stream's `Subjects` is the list of subject patterns it
  captures. Parti's per-partition consumers attach a `FilterSubject` derived
  from a subject template (`internal/dynamicbuild.BuildSubjects`); for those
  consumers to receive messages, the stream's `Subjects` must cover the
  rendered partition subjects. Validating that coverage requires resolving
  partition data against a `DynamicConsumerCfg` and is **out of scope for
  Phase 4** (see Non-Goals) — Phase 4 provisions the stream exactly as
  declared.
- **`provision.DynamicConsumerCfg`** (`provision/config.go:103-108`) already
  references an application stream by `StreamName`. Phase 4 does not change
  the dynamic-consumer surface; it only adds the ability to *provision* the
  stream that surface references.

## Invariants Inherited from Phases 1-3

Every invariant from the
[Phase 1 list](../provision-sdk-cli/00-implementation-plan.md#invariants-inherited-by-every-phase)
continues to hold. The load-bearing ones for Phase 4:

- **Ownership marker shape.** `parti.io/managed`, `parti.io/component`,
  `parti.io/instance` stay in the resource `Metadata` and remain
  informational. Phase 4 adds **one new component value**, `ComponentStream`
  (`"stream"`), and stamps it on `jetstream.StreamConfig.Metadata`. The
  marker never authorizes mutation; resource lookup is by exact NATS name.
- **JSON output envelope** stays at `apiVersion: parti.io/provision/v1`.
  Phase 4 adds, all additive:
  - three new `PlannedAction.Kind` values — `create-stream`, `update-stream`,
    `stamp-stream-marker`;
  - one new `DriftFinding.Kind` value — `application-stream`;
  - one new `Snapshot` field — `streams`.
  No existing value or field changes; tooling keyed on existing values keeps
  working.
- **Input config** `apiVersion: parti.io/v1` accepts additive fields. Phase 4
  adds the top-level `streams:` list; a config omitting it loads with no
  behavior change.
- **CLI exit codes and their precedence** (`cmd/partictl/exitcodes.go:14-25`)
  are stable: `0` ok, `1` runtime, `2` drift, `3` validation, `4` NATS. No new
  codes — stream errors map onto the existing sentinels (`ErrInvalidConfig`,
  `ErrLiveValidation`) and the existing `classifyError` table.
- **`Plan` action and drift ordering remains deterministic.** Actions and
  drift sort by `(Kind, Name)` (`sortActions` / `sortDrift`,
  `provision/plan.go:449-467`); the new stream actions and findings sort into
  the same total order with no special-casing.
- **No-builder note (Phase-4-specific).** The Phase 1 byte-equivalence
  invariant ("provisioned `KeyValueConfig` stays byte-equivalent to the
  runtime builder") has **no application-stream analogue** — see Background.
  Phase 4 introduces no shared builder because the runtime builds no
  application `StreamConfig`.

## Non-Goals (Phase 4)

- **Do not validate subject coverage against dynamic consumers.** Phase 4
  provisions a stream with exactly the `Subjects` declared in config. It does
  **not** cross-check that those subjects cover the partition subjects a
  declared `DynamicConsumerCfg` would need (that requires resolving
  `PartitionsRef` against partition data). **Phase 5 (Dynamic Precreate)**,
  which owns the consumer surface, is the home for that cross-check. See
  [Open Design Decisions](#open-design-decisions).
- **Do not auto-delete or recreate streams.** A stream with drift-immutable
  divergence (`Storage`, `Retention`) is reported as `drift-immutable` and
  never mutated. **Phase 6 (Force + Repair)** introduces the gated
  delete/recreate path. This matches the Phase 1-2 "no destructive default"
  posture.
- **Do not manage consumers bound to the stream.** Phase 4 provisions the
  stream only. Per-partition durable consumers remain a **Phase 5** concern;
  the existing alignment-check surface (`ValidateLiveDynamicConsumers`) is
  unchanged.
- **Do not manage stream message contents.** Phase 4 manages stream
  *config*, never published messages. (This is the stream analogue of Phase 3
  managing the partition-source bucket's key contents — but there is no
  Phase-4 equivalent: stream payloads are application data, not provisioning
  state.)
- **Do not expose mirror / source / republish / placement / per-subject
  limits.** Phase 4 exposes a deliberately small, common `StreamConfig` field
  subset (see W1). A live stream's other fields are preserved-from-live
  verbatim by `update-stream`, never drift-classified. Widening the field set
  is a documented follow-up if demand appears.
- **Do not change the reconcile-policy ladder.** `warn` / `adopt` /
  `safe-update` keep their Phase 1-2 semantics; streams are simply one more
  resource kind they govern. `force` remains rejected (Phase 6).

## Design

The organizing decision: **streams are a first-class `Config` resource kind,
processed by the same `View` / `Plan` / `ValidateLive` / `Apply` SDK
functions that already process control-plane and partition-source buckets.**
Streams have the same lifecycle as a KV bucket — create-missing, drift-report,
safe-update, adopt — so they reuse the existing policy ladder, action loop,
cancellation contract, and report envelope rather than duplicating them. This
is distinct from the Phase 3 decision to give partition *records* their own
`PlanPartitions` / `ApplyPartitions` entrypoints: records have genuinely
different mechanics (single-key CAS, `--prune`, policy-independent), whereas
streams do not. The `partictl stream` command (W5) is a CLI-level scoping
convenience over the unified SDK, not a separate SDK surface.

### W1 — `StreamCfg` config, marker component, static validation

**`provision/config.go`** — add the `Streams` field to `Config`:

```go
type Config struct {
    APIVersion       string                 `yaml:"apiVersion"                 json:"apiVersion"`
    Instance         string                 `yaml:"instance,omitempty"         json:"instance,omitempty"`
    Policy           ReconcilePolicy        `yaml:"policy,omitempty"           json:"policy,omitempty"`
    ControlPlane     *ControlPlaneConfig    `yaml:"controlPlane,omitempty"     json:"controlPlane,omitempty"`
    PartitionSource  *PartitionSourceConfig `yaml:"partitionSource,omitempty"  json:"partitionSource,omitempty"`
    DynamicConsumers []DynamicConsumerCfg   `yaml:"dynamicConsumers,omitempty" json:"dynamicConsumers,omitempty"`
    Streams          []StreamCfg            `yaml:"streams,omitempty"          json:"streams,omitempty"`
}
```

and the `StreamCfg` struct:

```go
// StreamCfg declares one application JetStream stream provision manages.
// It exposes a deliberately small subset of jetstream.StreamConfig; every
// other live-stream field is preserved-from-live by update-stream and never
// drift-classified (see Non-Goals).
type StreamCfg struct {
    Name        string        `yaml:"name"                  json:"name"`
    Subjects    []string      `yaml:"subjects"              json:"subjects"`
    Retention   string        `yaml:"retention,omitempty"   json:"retention,omitempty"`   // "limits" | "workqueue" | "interest"; default "limits"
    Storage     string        `yaml:"storage,omitempty"     json:"storage,omitempty"`     // "file" | "memory"; default "file"
    Discard     string        `yaml:"discard,omitempty"     json:"discard,omitempty"`     // "old" | "new"; default "old"
    Replicas    int           `yaml:"replicas,omitempty"    json:"replicas,omitempty"`    // 0 = server default (1)
    MaxAge      time.Duration `yaml:"maxAge,omitempty"      json:"maxAge,omitempty"`       // 0 = unlimited
    MaxBytes    int64         `yaml:"maxBytes,omitempty"   json:"maxBytes,omitempty"`     // 0 = unlimited
    MaxMsgs     int64         `yaml:"maxMsgs,omitempty"    json:"maxMsgs,omitempty"`      // 0 = unlimited
    Description string        `yaml:"description,omitempty" json:"description,omitempty"`
}
```

- **`musttag` requires the `yaml` tags.** `StreamCfg` is reachable through
  `yaml.Unmarshal` (the CLI loads YAML), so every field needs a `yaml` tag —
  the same constraint Phase 3 hit with `types.Partition`.
- **`Subjects` has no `omitempty`** — an empty subjects list is a validation
  error, not an omittable default, and emitting `subjects: []` in round-trip
  output is correct.
- **Defaulting (in `normalize`, `provision/validate.go`).** Deep-copy the
  `Streams` slice (mirroring the `DynamicConsumers` copy at
  `validate.go:108-112`) so caller-owned entries are not mutated. Per entry:
  empty `Retention` → `"limits"`, empty `Storage` → `"file"`, empty `Discard`
  → `"old"`. `Name`, `Subjects`, the numeric limits, and `Description` are
  not defaulted.
- **`Unlimited` convention — `MaxBytes` / `MaxMsgs` only.** `MaxBytes` and
  `MaxMsgs` use `0` for "unlimited" in config; the NATS server rewrites a
  zero limit to `-1` for these two fields, so Plan's drift classifier
  normalizes config-`0` and live-`-1` as equivalent (see W3
  `streamConfigsEqual`). This mirrors the existing
  `PartitionSourceConfig.MaxValueSize` convention (`config.go:79-83`).
  **`MaxAge` is different**: the NATS server keeps `0` as the unlimited value
  and rejects a negative `MaxAge`, so `MaxAge` carries no `0`↔`-1` rewrite —
  config `0` maps to live `0`. Negative `MaxAge` is rejected at static
  validation (below).

**`provision/validate.go`** — `validateResolved` calls a new `validateStreams`
when `len(cfg.Streams) > 0`:

- `Name` must be non-empty.
- Stream `Name`s must be unique within the config (a duplicate is
  `ErrInvalidConfig`).
- `Subjects` must contain at least one entry; no entry may be empty or
  contain whitespace. (Subject *syntax* beyond non-empty/no-whitespace is
  left to the NATS server, surfaced at apply time — `provision` does not
  reimplement NATS subject parsing.)
- `Retention` ∈ {`limits`, `workqueue`, `interest`}; `Storage` ∈ {`file`,
  `memory`}; `Discard` ∈ {`old`, `new`} (each already defaulted by
  `normalize`).
- `Replicas`, `MaxAge`, `MaxBytes`, `MaxMsgs` must each be `>= 0`.
- All errors wrap `ErrInvalidConfig` (CLI exit `3`).

**`provision/marker.go`** — add the component constant and wire it into both
classification switches:

```go
// ComponentStream marks an application JetStream stream provision manages.
ComponentStream = "stream"
```

Add `ComponentStream` to the `switch` in `ParseMarker`
(`marker.go:109-120`) and `ClassifyComponent` (`marker.go:63-74`) so a
stream-marked resource classifies as `ComponentStream` rather than
`ComponentUnknown`.

### W2 — `View`: application-stream inventory

**`provision/types.go`** — add the `Streams` slot to `Snapshot` and a
`StreamState` type:

```go
type Snapshot struct {
    APIVersion       string          `json:"apiVersion"`
    Kind             string          `json:"kind"`
    ObservedAt       time.Time       `json:"observedAt"`
    ControlPlane     []KVBucketState `json:"controlPlane"`
    PartitionSource  []KVBucketState `json:"partitionSource"`
    DynamicConsumers []ConsumerState `json:"dynamicConsumers"`
    Streams          []StreamState   `json:"streams"`
}

// StreamState is the live view of an application JetStream stream.
type StreamState struct {
    Stream    string        `json:"stream"`
    Component string        `json:"component,omitempty"`
    Instance  string        `json:"instance,omitempty"`
    Managed   string        `json:"managed,omitempty"`
    Subjects  []string      `json:"subjects,omitempty"`
    Retention string        `json:"retention,omitempty"`
    Storage   string        `json:"storage,omitempty"`
    Discard   string        `json:"discard,omitempty"`
    Replicas  int           `json:"replicas,omitempty"`
    MaxAge    time.Duration `json:"maxAge,omitempty"`
    MaxBytes  int64         `json:"maxBytes,omitempty"`
    MaxMsgs   int64         `json:"maxMsgs,omitempty"`
}
```

- `Snapshot.Streams` is initialized to a non-nil empty slice in `View`
  (mirroring the other slots, `view.go:33-36`), so the JSON envelope shape is
  stable.

**`provision/scope.go`** — add `Streams bool` to `Scope`; `ScopeAll` sets it
`true`; `ScopeFromConfig` sets it `len(cfg.Streams) > 0`.

**`provision/view.go`** — `View` already walks `js.ListStreams` once. Today it
calls `kvBucketStateFromStreamInfo`, which returns `false` for any stream
whose name lacks the `KV_` prefix (`view.go:105-109`) — those are silently
dropped. W2 adds a second classifier:

- For each `*jetstream.StreamInfo`: if the name has the `KV_` prefix, the
  existing KV path runs. Otherwise, a new `streamStateFromStreamInfo`
  inspects it as a candidate application stream.
- `streamStateFromStreamInfo` parses the marker. The default View filter
  still applies: an **unmarked** stream is dropped (consistent with unmarked
  KV buckets). A marked stream whose component is `ComponentStream` becomes a
  `StreamState`; a marked stream whose component is anything else
  (control-plane / partition-source / unknown markers wrongly on a non-KV
  stream) is dropped — it is not an application stream.
- The instance filter (`scope.Instance`) applies identically.
- `MaxBytes` / `MaxMsgs` normalize live `-1` → `0` for the report (mirroring
  the `MaxMsgSize` normalization at `view.go:116-120`).
- Results sort by `Stream` name. The short-circuit at `view.go:40-42` widens
  to `!scope.ControlPlane && !scope.PartitionSource && !scope.Streams`.

### W3 — `Plan`: create / update / stamp stream actions + drift

**`provision/types.go`** — new action-kind and drift-kind constants and
resource types:

```go
const (
    ActionCreateStream      = "create-stream"
    ActionUpdateStream      = "update-stream"
    ActionStampStreamMarker = "stamp-stream-marker"
)

const KindApplicationStream = "application-stream"

// UpdateStreamResource is the Resource on an ActionUpdateStream. Before is
// the live StreamConfig at plan time; After is the desired target. Both are
// deep clones so Plan output is immutable. Apply re-reads live and rebuilds
// the target from the re-read snapshot — Before/After are the audit surface.
type UpdateStreamResource struct {
    Before jetstream.StreamConfig `json:"before"`
    After  jetstream.StreamConfig `json:"after"`
}

// StreamStampMarkerResource is the Resource on an ActionStampStreamMarker.
// Mirrors StampMarkerResource but for a stream: Stream is the stream name,
// MergedMetadata is the full Metadata map the action writes, PartiKeys lists
// exactly the keys the action adds or changes for operator review.
type StreamStampMarkerResource struct {
    Stream         string            `json:"stream"`
    MergedMetadata map[string]string `json:"mergedMetadata"`
    PartiKeys      []string          `json:"partiKeys"`
}
```

`ActionCreateStream` carries a plain `jetstream.StreamConfig` as its
`Resource` (mirroring `create-kv`, `plan.go:245-256`).

The three stream actions deliberately mirror `create-kv` / `update-kv` /
`stamp-marker` one-to-one, so streams inherit the proven Phase 1-2 policy
semantics with zero conflation. A distinct `stamp-stream-marker` kind (rather
than reusing `stamp-marker`) keeps the JSON `kind` field unambiguous — every
`kind` value maps to exactly one `Resource` shape.

**`provision/plan_streams.go`** (new file) — `planStreams(ctx, js, cfg, out)`,
called from `Plan` after `planPartitionSource` when `len(cfg.Streams) > 0`:

For each declared `StreamCfg`:

1. Look up the stream by exact name: `js.Stream(ctx, cfg.Name)` — **no
   prefix** (unlike `KV_<bucket>`).
2. `ErrStreamNotFound`:
   - policy `adopt` → no action, one `informational` `application-stream`
     finding ("stream missing; adopt does not create — run apply with warn or
     safe-update"), mirroring `missingUnderAdoptFinding`
     (`plan.go:181-190`).
   - policy `warn` / `safe-update` → one `create-stream` action whose
     `Resource` is the `jetstream.StreamConfig` built by
     `buildStreamConfig(streamCfg, cfg.Instance)` (W-helper: maps `StreamCfg`
     fields, sets `Metadata = BuildMarker(ComponentStream, instance)`, and
     translates the unlimited convention to NATS form — config `0` →
     `-1` for `MaxBytes` / `MaxMsgs` only; `MaxAge` passes through, `0`
     staying `0`).
3. Stream exists → fetch `stream.Info(ctx)`, classify drift via
   `classifyStreamDrift`, append findings. Then, by policy:
   - `safe-update` **and** the stream is Parti-marked: build the update
     target; if it differs from live, emit an `update-stream` action.
   - `adopt` **and** the stream is **un**marked: emit a
     `stamp-stream-marker` action (via a `newStampStreamMarkerAction` helper
     mirroring `newStampMarkerAction`, `plan.go:196-209`).
   - Drift findings and the action coexist in every policy, exactly as the KV
     path does.

**`classifyStreamDrift`** mirrors `classifyControlPlaneDrift`
(`plan.go:261-363`). It builds a `wanted` `jetstream.StreamConfig` from the
desired `StreamCfg` and the live config via `wantedStreamConfig` (below), then
compares `wanted` against live with `streamConfigsEqual`:

- Unmarked live stream → one `adopted` finding (mirrors `plan.go:262-273`).
- Marked + `streamConfigsEqual(wanted, live)` → one `informational` finding.
- Marked + diverged → a per-field comparison of `live` against `wanted`
  routes each differing field to an `addImmutable` or `addMutable`
  accumulator — exactly as `classifyControlPlaneDrift` does
  (`plan.go:286-342`) — producing up to two findings:
  - **`drift-immutable`** accumulates: `Storage` (NATS rejects a file↔memory
    change on `UpdateStream`), any `Retention` divergence (see the
    conservative-retention note below), and a `component`-marker mismatch
    (mirrors `plan.go:337-342`). The safe remediation for an immutable
    divergence is operator-driven delete/recreate (Phase 6).
  - **`drift-mutable`** accumulates: `Subjects`, `Discard`, `Replicas`,
    `MaxAge`, `MaxBytes`, `MaxMsgs`, `Description`, and the `managed` /
    `instance` marker fields. NATS `UpdateStream` accepts all of these. The
    server remains the final authority — an update it nonetheless rejects
    (e.g. a `Replicas` change with no cluster peers) surfaces as an
    apply-time `ResourceError` (W4).

**Conservative retention treatment.** The NATS server does *not* reject every
retention change — it rejects only transitions to or from `WorkQueuePolicy`,
while `limits` ↔ `interest` is accepted (subject to a consumer-replica
constraint that can make the update fail until bound consumers are adjusted).
Phase 4 deliberately classifies **every** `Retention` divergence as
`drift-immutable` rather than modelling that partial, consumer-coupled
mutability: a stream's retention policy is a fundamental property, and the
operator-driven delete/recreate path (Phase 6) is the safe remediation. This
plan does **not** claim NATS rejects all retention changes — it states
Phase 4's chosen conservative policy. `limits` ↔ `interest` safe-update
support is a possible follow-up; see [Open Design Decisions](#open-design-decisions).

**`wantedStreamConfig(streamCfg, instance, live)`** is the classifier-side
target builder, the stream analogue of `wantedControlPlaneKV`
(`plan.go:365-377`). It clones `live` and overwrites **every** Phase-4-managed
field with the desired value — the mutable ones (`Subjects`, `Discard`,
`Replicas`, `MaxAge`, `MaxBytes`, `MaxMsgs`, `Description`) **and the
immutable ones (`Storage`, `Retention`)** — then sets `Metadata` via
`mergeMarkerMetadata(live.Metadata, ComponentStream, instance)`, which
preserves non-Parti metadata keys while setting `parti.io/component` to the
desired value so a component mismatch is *detected*. **Overwriting `Storage`
and `Retention` here is essential**: `wanted` must carry the *desired* value
of every field so `streamConfigsEqual(wanted, live)` returns false on a
storage- or retention-only divergence — otherwise the classifier would
short-circuit to `informational` and silently mask immutable drift.
`wantedControlPlaneKV` writes desired `Storage` / `History` into `wanted` for
exactly this reason (`plan.go:368-371`). The immutable/mutable *split* lives
entirely in the classifier's per-field routing (the `addImmutable` /
`addMutable` accumulators above), **not** in what `wanted` contains. Because
`wanted` carries `live`'s non-Parti metadata verbatim, `streamConfigsEqual`
never reports spurious drift on an operator-added key such as `owner=payments`.
The `update`-side helper `buildStreamUpdateTarget` is the deliberate opposite —
it inherits `Storage` / `Retention` from `live` so an `update-stream` never
writes an immutable change (see below).

**`streamConfigsEqual`** (the stream analogue of `kvConfigsEqual`) compares
two `jetstream.StreamConfig` values over the Phase-4-managed fields plus
`Metadata`, normalizing server defaults so a config that omits a field shows
no spurious drift against a server-defaulted live stream:

- `Subjects` compared as a **set** (sorted copy) — order is not semantically
  meaningful.
- `Storage` empty ↔ `FileStorage`; `Retention` empty ↔ `LimitsPolicy`;
  `Discard` empty ↔ `DiscardOld` (NATS server defaults).
- `Replicas` `0` ↔ `1` (reuse `normalizeReplicas`, `plan.go:318`).
- `MaxBytes` / `MaxMsgs`: `0` ↔ `-1` — the NATS server rewrites a zero limit
  to `-1` for these two fields. **`MaxAge` is not normalized this way**: the
  server keeps `0` as the unlimited value and rejects a negative `MaxAge`, so
  `MaxAge` is compared directly as a duration (`0` equals only `0`).
- `Metadata` compared as a map.
- All other `StreamConfig` fields are ignored (preserved-from-live; see
  Non-Goals).

The **update** target is built by a separate `buildStreamUpdateTarget` helper
(used by `update-stream`, **not** by the classifier): a deep clone of the live
`StreamConfig` with only the Phase-4-managed mutable fields overwritten and
`Metadata` merged via `mergeUpdateKVMetadata` (force `managed`, set/clear
`instance`, **preserve `component`** verbatim from live). `wantedStreamConfig`
and `buildStreamUpdateTarget` are intentionally distinct — exactly as the KV
path keeps `mergeMarkerMetadata` and `mergeUpdateKVMetadata` distinct to avoid
the silent component-rewrite bug (`plan.go:385-430`): the classifier helper
rewrites `component` to the desired value so a mismatch is *flagged*, while
the update helper preserves `component` so `update-stream` never silently
re-labels a stream's role. Drift-immutable fields (`Storage`, `Retention`)
are inherited from live in the update target too, so an immutable divergence
is reported but never written.

### W4 — `Apply` + `ValidateLive`: stream apply path

**`provision/apply_stream.go`** (new file) — a stream apply seam mirroring the
Phase 3 `partitionKV` seam (`apply_partitions.go:27-87`) and the Phase 2
`streamReader` / `kvUpdater` seam (`apply_update.go:15-48`):

```go
// streamManager is the apply-side seam for the stream actions. The production
// implementation wraps a live jetstream.JetStream; tests inject a fake to
// drive the re-read / no-op / stale-before / write steps without a server.
type streamManager interface {
    StreamInfo(ctx context.Context, name string) (*jetstream.StreamInfo, error)
    CreateStream(ctx context.Context, cfg jetstream.StreamConfig) error
    UpdateStream(ctx context.Context, cfg jetstream.StreamConfig) error
}
```

Three apply functions, each returning `(ExecutedAction, error)` with the
return contract of `applyUpdateKVAction` (`apply_update.go:62-69`) — nil error
records the action, a `context.Canceled`/`DeadlineExceeded` error signals
cancellation, any other error is a fail-fast resource error:

- **`applyCreateStreamAction`** — type-assert `Resource` to
  `jetstream.StreamConfig`, call `CreateStream`. On
  `jetstream.ErrStreamNameAlreadyInUse` treat as a Plan→Apply race:
  `ExecutedAction{Raced: true}` (mirrors the `create-kv` race handling,
  `apply.go:130-135`). This is a direct `js.CreateStream`, **not**
  `jsutil.EnsureStream` — `provision`'s apply contract is one attempt with
  the race surfaced honestly, consistent with the `create-kv` path; the
  `jsutil` retry loop belongs to runtime/application code.
- **`applyUpdateStreamAction`** — re-read live `StreamInfo`; rebuild the
  target from the re-read snapshot via `buildStreamUpdateTarget`; no-op
  short-circuit when live already equals the target (`Raced: true`);
  stale-before check (`streamConfigsEqual(live, res.Before)`) — a genuine
  third-state change → fail-fast resource error; otherwise `UpdateStream`.
  Step order is identical to `applyUpdateKVAction` (`apply_update.go:84-141`).
  A `StreamInfo` miss → a `stream-missing-before-update` fail-fast error
  (mirrors `bucketMissingBeforeUpdate`).
- **`applyStampStreamMarkerAction`** — re-read live `StreamInfo`; recompute
  the merged metadata against the re-read `Metadata` via
  `mergeMarkerMetadata`; no-op short-circuit when the merge is already
  applied (`Raced: true`); otherwise `UpdateStream` with a clone of the live
  config carrying only `Metadata` changed. No stale-before check — identical
  to `applyStampMarkerAction` (`apply_update.go:181-250`).

**`provision/apply.go`** — `applyPlan`'s `switch action.Kind` gains three
cases (`ActionCreateStream`, `ActionUpdateStream`, `ActionStampStreamMarker`),
each delegating to the helper above and folding the result into
`Report.{Executed, Skipped, Errors, Aborted}` with the **exact** cancellation
/ fail-fast / skip handling the existing `ActionUpdateKV` and
`ActionStampMarker` cases use (`apply.go:151-189`). The production
`streamManager` adapter (`jsStreamManager`) is constructed once before the
loop, alongside `reader` / `updater` (`apply.go:99-101`).

**`provision/validate_live.go`** — `ValidateLive` gains a per-stream info
probe after the partition-source probe, run when `len(cfg.Streams) > 0`:

- For each declared stream, `js.Stream(ctx, cfg.Name)` then `stream.Info`.
  `ErrStreamNotFound` is **not** an error (Apply will create it) — identical
  to `probeBucketInfo` (`validate_live.go:126-142`), but with the raw stream
  name, no `KV_` prefix.
- Permission / reachability errors classify through the existing
  `classifyLiveError` (`ErrLiveValidation` → exit `3`; reachability → exit
  `4`). A failure builds a `liveErrorReport(KindApplicationStream,
  cfg.Name, err)`.

### W5 — `partictl stream` command + existing-command wiring

**Existing commands inherit streams for free.** Because `provision.Plan` /
`Apply` / `View` / `ValidateLive` now process `Config.Streams`, the existing
`partictl plan` / `apply` / `adopt` / `validate` / `view` commands provision
and report streams automatically whenever the loaded config has a `streams:`
block — no per-command code change beyond **output rendering**:

- `cmd/partictl/output.go` text renderers (`renderSnapshotText`,
  `emitPlan`, `emitReport`) gain stream sections: a `Streams` block in the
  Snapshot renderer, and recognition of the three new action kinds and the
  `application-stream` drift kind in the plan/report renderers. JSON output
  needs no change — it marshals the structs directly.

**`cmd/partictl/cmd_stream.go`** (new file) — a two-level command mirroring
`cmd_partitions.go` (`cmd/partictl/cmd_partitions.go`):

```
partictl stream view  [-f <config>] [-json] [-instance <name>]
partictl stream plan   -f <config>  [-json] [-fail-on-drift] [-policy <p>]
partictl stream apply  -f <config>  [-json] [-dry-run] [-policy <p>]
```

`stream` is a CLI-level **scoping** convenience: each sub-subcommand runs the
ordinary `provision.View` / `Plan` / `Apply` against a **stream-only config
view** — a copy of the loaded `Config` with `ControlPlane` and
`PartitionSource` set to `nil` and `DynamicConsumers` cleared. Because `Plan`
and `Apply` already skip nil sections (`plan.go:52-68`), the result is
naturally scoped to streams with no new SDK code.

**Command sequence — explicit so error ordering is deterministic.** For
`stream plan` and `stream apply` (and `stream view` when `-f` is given):

1. Load `parti-env.yaml` → the full `Config`. A load/parse failure → exit `3`.
2. Apply any `-policy` flag override to the loaded `Config` (`plan` / `apply`
   only — `view` has no `-policy`), exactly as the top-level commands do.
3. Run **`provision.Validate` on the FULL loaded `Config`** — *not* the
   stream-only view. A malformed non-stream section (`controlPlane`,
   `partitionSource`) is a broken config file and is rejected with exit `3`,
   mirroring how top-level `plan` validates the full config before connecting
   (`cmd_plan.go:59-73`) and `apply` / `adopt` do via `runReconcile`
   (`cmd/partictl/reconcile.go`). `stream` commands never silently tolerate
   an invalid non-stream section.
4. Derive the stream-only config view (nil `ControlPlane` /
   `PartitionSource`, cleared `DynamicConsumers`).
5. Connect to NATS; run `provision.View` / `Plan` / `Apply` against the
   derived view. `Plan` / `Apply` re-run their own internal validation on the
   derived view — a harmless re-check, since step 3 validated the superset.

Per sub-subcommand:

- `stream view` — **`-f` is optional.** Without `-f`: inventory mode — no
  config is loaded, steps 1-4 are skipped, and the instance filter comes from
  the `-instance` flag. With `-f`: steps 1, 3, 4 run (there is no `-policy`),
  and the instance filter comes from `cfg.Instance` (`-instance` is ignored).
  Either way it calls `provision.View` with a `Scope` of `{Streams: true}`
  plus that instance filter. **Scoping is by kind and instance only — never
  by config stream name.** `provision.View` walks every stream and filters by
  marker / component / instance (`view.go:44-86`); `Scope` carries no name
  set and W2 adds none. This is identical to how top-level `view` handles the
  optional-`-f` split (`cmd_view.go:48-74`). A config that names only some of
  the account's marked application streams therefore still sees **all**
  marked streams in its instance; `stream view` is an instance-scoped
  inventory, not a per-stream lookup. (A name-scoped view surface, e.g.
  `Scope.StreamNames`, is a possible follow-up — see
  [Open Design Decisions](#open-design-decisions) — but Phase 4 does not add
  one.)
- `stream plan` — **`-f` required.** Steps 1-5 with `provision.Plan`;
  `-fail-on-drift` → exit `2` when the plan carries drift; `-policy` accepted
  (warn / adopt / safe-update), exactly as top-level `plan`.
- `stream apply` — **`-f` required.** Steps 1-5 with `provision.Apply`;
  `-dry-run` aliases `stream plan` (emits the `Plan` envelope, writes
  nothing); `-policy` accepted.
- Exit codes route through the existing `classifyError` — no
  `exitcodes.go` change.
- Register `stream` in `run.go`'s dispatch `switch` and the top-level usage
  text; remove `stream` from the "Deferred commands" list (`run.go:74-75`).
- **Two-level command + NATS flag note.** Per the saved CLI convention,
  `runWithNATS` splices `-server` after `args[0]` and breaks two-level
  commands; `cmd_stream.go` tests must build args manually (`runArgs("stream",
  "plan", "-server", url, ...)`), exactly as `cmd_partitions_test.go` does.

### W6 — Documentation

- **`docs/PROVISION.md`**: a new "Application Streams" section — the
  `streams:` config schema, how `plan` / `apply` / `adopt` treat streams, the
  `partictl stream` command, the drift-immutable `Storage` / `Retention`
  caveat, and the subject-coverage Non-Goal (with a forward pointer to
  Phase 5). Add the section to the Table of Contents. Update the Overview
  paragraph that currently says `provision` "never manages application
  streams" (`docs/PROVISION.md:35-37`).
- **Package godoc** for every new exported symbol: `StreamCfg`,
  `StreamState`, `Snapshot.Streams`, `Scope.Streams`, `ComponentStream`,
  `ActionCreateStream`, `ActionUpdateStream`, `ActionStampStreamMarker`,
  `KindApplicationStream`, `UpdateStreamResource`,
  `StreamStampMarkerResource`.
- **`CHANGELOG.md`**: a new release section under `[Unreleased]` for the
  Phase 4 stream surface.

## Work Items

| ID | Scope | Impl model | Review effort |
|----|-------|------------|---------------|
| W1 | `StreamCfg` + `Config.Streams`; `normalize` defaulting; `validateStreams`; `ComponentStream` marker | sonnet | high |
| W2 | `View` stream inventory: `StreamState`, `Snapshot.Streams`, `Scope.Streams`, `streamStateFromStreamInfo` | sonnet | high |
| W3 | `planStreams`, `classifyStreamDrift`, `streamConfigsEqual`, `buildStreamConfig` / `buildStreamUpdateTarget`, new action/drift kinds + resource types | sonnet | xhigh |
| W4 | `streamManager` seam; `applyCreateStreamAction` / `applyUpdateStreamAction` / `applyStampStreamMarkerAction`; `applyPlan` wiring; `ValidateLive` stream probe | opus | xhigh |
| W5 | `partictl stream view/plan/apply`; `run.go` dispatch; `output.go` stream rendering | sonnet | high |
| W6 | `docs/PROVISION.md`, godoc, `CHANGELOG.md` | sonnet | high |

Per-work-item loop (unchanged from Phases 1-3): implement → `/simplify` →
codex post-impl review → fix every P0/P1 → re-verify `go build ./...`,
`make lint`, package tests → commit. W3 and W4 are the sharp items
(drift-classification correctness; apply concurrency + seam ordering) and
carry `xhigh` review effort.

## Test Plan

Each invariant has an encoding:

- **W1 config / validation:** `streams:` YAML round-trips (`name`,
  `subjects`, `retention`, etc.); `normalize` defaults `retention`/`storage`/
  `discard` and deep-copies the slice (caller's slice not mutated); empty
  `name`, empty/whitespace subject, empty `Subjects`, duplicate stream
  `Name`, bad `retention`/`storage`/`discard` enum, negative numeric → each
  `ErrInvalidConfig`; a config with no `streams:` still validates (no
  cross-contamination); `ParseMarker` / `ClassifyComponent` classify
  `ComponentStream`.
- **W2 view:** a marked application stream surfaces in `Snapshot.Streams`; an
  unmarked non-KV stream is dropped; a non-KV stream marked with a
  non-`stream` component is dropped; KV buckets still surface under
  `ControlPlane` / `PartitionSource` unaffected; the instance filter applies;
  live `MaxBytes`/`MaxMsgs` of `-1` report as `0`; `Scope.Streams` gates the
  walk; `Snapshot.Streams` is non-nil even when empty; deterministic sort.
- **W3 plan:** missing stream → `create-stream` under warn/safe-update,
  informational finding under adopt; converged marked stream →
  informational; mutable-only drift (subjects/discard/replicas/maxAge/
  maxBytes/maxMsgs/description) → `drift-mutable`; a **storage-only**, a
  **retention-only**, and a **component-only** divergence each →
  `drift-immutable` and are never masked as `informational` (the regression
  guard that `wantedStreamConfig` overlays — not inherits — the desired
  immutable fields); mixed mutable+immutable → both findings;
  unmarked stream → `adopted` finding, plus `stamp-stream-marker` under
  adopt; marked stream + mutable drift under safe-update → `update-stream`
  action; a `Retention` divergence — **including `limits`↔`interest`** —
  classifies `drift-immutable` with no `update-stream` emitted (the
  conservative-retention policy); `streamConfigsEqual` normalizes
  subjects-as-set, server-default storage/retention/discard, `replicas`
  `0↔1`, and `MaxBytes`/`MaxMsgs` `0↔-1`, while comparing `MaxAge` directly
  (config `0` equals live `0`; a live `-1` is **not** equal to a config `0`);
  `wantedStreamConfig` carries live's non-Parti metadata so an operator-added
  key (`owner=payments`) yields no spurious drift, while a
  `parti.io/component` mismatch still classifies `drift-immutable`;
  deterministic `(Kind, Name)` ordering with the new kinds interleaved among
  `create-kv` etc.
- **W4 apply (via the seam, no live server):** clean `create-stream`;
  create-stream race (`ErrStreamNameAlreadyInUse`) → `Raced: true`;
  `update-stream` no-op short-circuit (`Raced` true/false); `update-stream`
  stale-before genuine race → `ResourceError`, no write; each of
  `create-stream` / `update-stream` / `stamp-stream-marker` with a wrong
  `Resource` concrete type → fail-fast resource error; `stamp-stream-marker`
  no-op and write; `stream-missing-before-update` (the stream vanishes
  between plan and the `update-stream` re-read) and the stamp analogue
  `stream-missing-before-stamp` (the stream vanishes before the
  `stamp-stream-marker` re-read **and** at write time — both miss points,
  mirroring the KV stamp at `apply_update.go:204` / `:236`); context
  cancellation at create / re-read / write → `Aborted: true`, action in
  `Skipped` with `context-cancelled`, `ctx.Err()` returned; fail-fast on a
  non-cancellation error skips the remainder with `prior-error`; a
  server-rejected mutable update (e.g. NATS refuses a `Subjects` change)
  surfaces as a `ResourceError` → exit `1`.
- **W4 ValidateLive:** stream-info probe passes for an existing stream and
  for `ErrStreamNotFound`; a permission error → `ErrLiveValidation` (exit
  `3`); a reachability error → exit `4`; cancellation → `ctx.Err()`.
- **W5 CLI:** `partictl stream plan` exit `0` (no drift) / `2`
  (`-fail-on-drift`, drift present); `stream apply` exit `0` / `1`
  (server-rejected update) / `3` (bad config); `stream apply -dry-run` writes
  nothing; `stream view` lists streams; with two marked application streams
  in one instance and a config naming only one, `stream view -f` lists
  **both** (the instance-scoped-inventory contract — not a name filter);
  `-policy` accepted on `stream plan/apply` and rejects `force`; top-level
  `partictl plan`/`apply` on a config with a `streams:` block provisions
  streams too; JSON envelope `apiVersion` present on `stream` plan/apply/view
  output; two-level args built manually (the `runWithNATS` caveat).
- **Integration (live NATS):** an end-to-end test — `partictl apply` a config
  with a `streams:` block creates the stream marked with `ComponentStream`;
  `partictl view` lists it; a second `apply` under `safe-update` after a
  `maxAge` change reconciles it; `apply` under `adopt` against a
  pre-existing unmarked stream stamps the marker;
  `provision.View` reads the stream back with the expected config.

## Open Design Decisions

Surfaced for `plan-review`; the rest of the plan assumes the stated choice.

1. **Streams as a unified `Config` resource vs. a separate SDK entrypoint.**
   Chosen: streams flow through the existing `Plan` / `Apply` / `View` /
   `ValidateLive` functions as one more `Config` section, governed by the
   existing reconcile-policy ladder. Rejected: a Phase-3-style separate
   `PlanStreams` / `ApplyStreams` pair. Rationale: partition records earned
   their own entrypoint because their mechanics genuinely differ (single-key
   CAS, `--prune`, policy-independent); streams have the *same* lifecycle as
   KV buckets, so a separate entrypoint would duplicate the whole policy /
   action-loop / cancellation / envelope machinery for no semantic gain.
2. **`partictl stream` as a CLI scoping wrapper.** Chosen: `stream
   view/plan/apply` loads the full config and runs the unified SDK against a
   stream-only config view (non-stream sections nil'd). The roadmap and the
   continuation brief both call for the command; this implementation honors
   it without a parallel SDK surface. Reopen only if a stream-only SDK
   entrypoint is independently justified.
3. **Three distinct stream action kinds.** Chosen: `create-stream` /
   `update-stream` / `stamp-stream-marker`, mirroring `create-kv` /
   `update-kv` / `stamp-marker` one-to-one, so streams inherit the proven
   policy semantics and every JSON `kind` maps to exactly one `Resource`
   shape. Rejected: folding adoption into `update-stream` (loses the
   distinct operator-review surface and the no-stale-before stamp
   semantics) or reusing the `stamp-marker` kind for two `Resource` shapes
   (ambiguous JSON contract).
4. **No subject-coverage cross-check (deferred to Phase 5).** Chosen:
   Phase 4 provisions a stream with exactly the declared `Subjects` and does
   not verify they cover the partition subjects a `DynamicConsumerCfg` would
   need. The check requires resolving `PartitionsRef` against partition data,
   which is the Phase 5 (Dynamic Precreate) domain. Reopen if plan-review
   judges the silent-misconfiguration risk high enough to warrant at least an
   informational drift finding in Phase 4 when both `streams:` and
   `dynamicConsumers:` are declared.
5. **Exposed `StreamConfig` field subset.** Chosen: `Name`, `Subjects`,
   `Retention`, `Storage`, `Discard`, `Replicas`, `MaxAge`, `MaxBytes`,
   `MaxMsgs`, `Description` — the common operational knobs. Mirror / source /
   republish / placement / per-subject limits / `MaxConsumers` are
   preserved-from-live and never drift-classified. Reopen per-field if a
   concrete demand profile appears.
6. **Direct `js.CreateStream` vs `jsutil.EnsureStream`.** Chosen: the apply
   path calls `js.CreateStream` / `js.UpdateStream` directly and surfaces a
   race as `Raced: true`, consistent with the `create-kv` apply contract.
   Rejected: `jsutil.EnsureStream`, whose get-first + retry loop is a
   runtime/application convenience and would import a different
   error-handling posture into `provision`'s one-attempt-honest-race model.
7. **Conservative retention immutability.** Chosen: every `Retention`
   divergence classifies `drift-immutable`; safe-update never reconciles it.
   The NATS server actually accepts `limits` ↔ `interest` (rejecting only
   transitions involving `workqueue`), so this is stricter than the server.
   Rationale: retention is a fundamental stream property and the `limits` ↔
   `interest` update is consumer-replica-coupled — it can fail until bound
   consumers are adjusted, so modelling that partial mutability is
   disproportionate for Phase 4. Reopen if operators need in-place
   `limits` ↔ `interest` reconciliation; the change would be localized to
   `classifyStreamDrift` and `buildStreamUpdateTarget`.
8. **No name-scoped `View`.** Chosen: `provision.View` and `Scope` keep their
   kind + instance filter; `partictl stream view` is an instance-scoped
   inventory that lists every marked application stream in the instance, not
   only those named by `-f`. Rejected for Phase 4: a `Scope.StreamNames`
   config-name filter. A per-stream lookup surface can be added later if
   operators need to inspect one named stream in isolation.
