# Parti Provision SDK and CLI Plan

## Problem Statement

Parti users who run dynamic partitioning with a NATS KV partition source must
currently prepare several NATS resources by hand: Parti manager KV buckets, the
two-phase handoff bucket when handoff is enabled, and the NATS KV bucket/key
that stores partition definitions. Some deployments also want Parti-aware
checks for application streams or dynamic per-partition consumers, but the
Parti-specific control plane is the first problem to solve.

That setup is operationally fragile because creation and update behavior is
split across runtime startup, application bootstrap code, and manual NATS CLI
commands. The current manager startup path creates or opens control-plane KV
buckets, but it is not a full provisioning workflow: it does not expose a
read-only view of current settings, does not manage the partition-source
bucket/key as a first-class environment object, and intentionally opens
pre-existing KV buckets without reconciling their config.

Operators need a repeatable way to view, validate, plan, apply, and audit the
Parti environment from config before workers start. The workflow must make
missing resources easy to create, make drift visible, and avoid silently
destroying or rewriting runtime state such as consumer ack cursors.

Before this tool, a human operator must read Parti runtime config, infer the
required NATS buckets, create resources manually, and then debug startup-time
failures when permissions or resource settings are wrong. After this tool, the
operator can run a read-only `view`/`plan` in CI or during a change window,
review exactly what Parti expects, and apply only the safe missing-resource
changes that were planned.

## End-State Vision

In the mature state, `provision/` and `partictl` are the canonical operator
surface for Parti environments. They cover the full lifecycle of every NATS
resource Parti runtime depends on, with reconcile policies that scale from
"observe-only" to "fully repair from declarative config":

- **Full resource coverage**: control-plane KV buckets, partition-source
  bucket **and its records**, application JetStream streams, dynamic
  per-partition consumers.
- **Three reconcile policies**:
  - `warn` (this plan): create missing, never mutate existing.
  - `safe-update`: live-edit fields that NATS can reconcile in place
    (`Description`, `Metadata`, `MaxBytes`, `MaxValueSize`, `TTL`) with
    explicit per-field test coverage. Non-live-editable fields remain
    `drift-immutable` and require explicit destructive intent.
  - `force` with per-resource `allowDeleteRecreate`: destructive repair for
    immutable drift (`History`, `Storage`, replica downgrades). Gated on
    cursor-loss test discipline.
- **Adoption**: `partictl adopt` stamps the Parti ownership marker on
  resources created outside the tool, transitioning `adopted` drift into the
  managed set without touching data.
- **Generators**: emit a starter `parti-env.yaml` from an existing runtime
  `parti.Config`; emit a snapshot YAML from live NATS state for review,
  diffing, and onboarding.
- **Optional Kubernetes integration**: a controller built on top of the SDK
  reconciles a `ProvisionedPartiEnv`-style CRD to the same `Plan`/`Apply`
  paths the CLI uses; no logic duplication.
- **Templating story**: thin overlays for dev/stage/prod evaluated SDK-side
  on top of the same `Config` struct, once a real demand profile justifies
  the design cost.

The SDK and CLI remain the canonical surface even after the K8s layer lands —
the controller is a consumer of the SDK, not a replacement.

## Phased Roadmap

This plan specifies **Phase 1 (POC)** in implementation detail. Each later
phase will get its own design plan, dispatched through the same codex-review
loop documented under [Implementation Workflow](#implementation-workflow).
Phases are designed to be shippable independently in the order shown;
sequencing respects safety (mutation comes after reporting; destructive
repair comes last) but does not require strict serial release.

| Phase | Codename            | Adds                                                                                                              | Status        |
|-------|---------------------|-------------------------------------------------------------------------------------------------------------------|---------------|
| v1    | POC                 | `warn` policy; control-plane + partition-source bucket provisioning; dynamic-consumer alignment; `partictl` CLI   | **This plan** |
| v2    | Safe Update + Adopt | `safe-update` policy with per-field mutability tests; `partictl adopt`; `controlPlane.replicas`                   | TBD           |
| v3    | Partition Records   | `partictl partitions plan/apply` for partition-source key contents; record-level diff/dry-run                     | TBD           |
| v4    | Streams             | `streams:` block on `Config`; `partictl stream view/plan/apply`; stream ownership and adoption                    | TBD           |
| v5    | Dynamic Precreate   | Optional dynamic-consumer precreation, gated on alignment having proven byte-equivalent to runtime in v1          | TBD           |
| v6    | Force + Repair      | `force` policy; per-resource `allowDeleteRecreate`; cursor-loss test discipline for consumer delete/recreate      | TBD           |
| v7    | K8s Controller      | CRD types; controller built on the SDK; reconcile loop reuses `Plan`/`Apply`                                      | TBD           |

Cross-cutting follow-ups (not gated on the phase order):

- **Templating / overlays** — thin SDK-evaluated overlay for env-specific
  values. Can land any time after v1 once a real demand profile is known.
- **Config generators** — `partictl init` (from runtime `parti.Config`) and
  `partictl emit` (from a live `Snapshot`). Either side of the round-trip
  can ship independently.

### Invariants inherited by every phase

Later phases extend the v1 contract but **never break it**:

- Ownership marker shape (`parti.io/managed`, `parti.io/component`,
  `parti.io/instance`) stays in `Metadata` and remains informational.
- JSON envelope schema stays at `apiVersion: parti.io/provision/v1` until a
  v2 schema is explicitly designed; new fields are additive within v1.
- Input config `apiVersion: parti.io/v1` accepts additive fields; a breaking
  schema change bumps to `parti.io/v2` and v1-loaders continue to work.
- CLI exit codes and their precedence (see [Exit codes](#exit-codes)) are
  stable; new codes only appear above code 4 to avoid CI breakage.
- `Plan` action ordering remains deterministic `(Kind, Name)`.
- Provisioned control-plane `KeyValueConfig` remains byte-equivalent to
  `manager_setup.go:ensureKVBucket` (the W0 shared builder is the contract).
- Resource lookup is always by exact NATS name first; the marker never
  authorizes mutation.

## Goals

- Add a public `provision/` SDK for Parti/NATS environment viewing, planning,
  validation, application, and reporting.
- Add a `cmd/partictl` CLI using the standard library `flag.FlagSet`; do not
  add a CLI framework dependency in v1.
- Support desired-state config as Go structs with both `yaml` and `json` tags.
  The CLI officially loads YAML in v1; SDK callers can construct structs
  directly or unmarshal from their own format.
- Require a top-level `apiVersion: parti.io/v1` field on every YAML config so
  future schema changes can migrate cleanly.
- Make the v1 pillars explicitly Parti-specific:
  - Control-plane KV bucket provisioning and drift reporting.
  - NATS KV partition-source bucket provisioning. Partition-record management
    lands in **Phase 3 (Partition Records)**.
  - Dynamic-consumer alignment checks through a shared pure planning helper
    plus a live-validation companion, without duplicating private durable
    naming rules.
- Support read-only operational workflows:
  - `view`: inspect live NATS/Parti resource settings filtered by the Parti
    ownership marker (see [Resource Ownership Marker](#resource-ownership-marker)).
  - `validate`: validate config statically and, with `-live`, pre-flight live
    reachability and permissions.
  - `plan`: compute desired-vs-live drift and proposed actions.
  - `apply --dry-run`: convenience alias for validation plus plan; emits the
    same `Plan` JSON as `plan`.
- Ship v1 with a single `ReconcilePolicy` value: `warn` (create missing
  resources, report drift, never mutate existing resources). `safe-update`
  lands in **Phase 2** and `force` lands in **Phase 6**, each with explicit
  field/test coverage.

## Non-Goals (Phase 1)

Items below are out of scope for Phase 1. Where applicable, the target phase
from the [Phased Roadmap](#phased-roadmap) is cited so deferred work has a
clear home rather than an unbounded "follow-up plan" promise.

- Do not auto-delete or recreate existing streams, buckets, or consumers by
  default. **Phase 6 (Force + Repair)** introduces the gated destructive path.
- Do not silently migrate any existing resource's config. v1 only creates
  missing resources and reports drift. **Phase 2 (Safe Update + Adopt)**
  introduces in-place mutation for live-editable fields.
- Do not write partition contents. v1 provisions the partition-source bucket
  only. **Phase 3 (Partition Records)** adds `partictl partitions plan/apply`.
- Do not replace `Manager.Start` runtime ensure behavior. The provision tool
  is a human-controlled provisioning and drift-audit layer that complements
  runtime startup. This non-goal is permanent across all phases.
- Do not pre-create dynamic consumers by copying private durable naming logic
  into `provision`. This non-goal is permanent; the v4/`internal/durable`
  extraction seam (W4) is the authorized path.
- Do not pre-create dynamic consumers at all in Phase 1. v1 does
  alignment-check only. **Phase 5 (Dynamic Precreate)** adds optional
  precreation after the W4 builder is proven byte-equivalent to runtime.
- Do not solve Kubernetes CRD/operator-controller integration. **Phase 7
  (K8s Controller)** adds it as a layer on top of the SDK, not a replacement.
- Do not provide a templating or overlays system. Dev/stage/prod differences
  are handled by separate rendered YAML files in Phase 1; first-class
  overlays are a cross-cutting follow-up.
- Do not provision or audit application JetStream streams. v1 exposes no
  stream-related public API, action kind, permission, scope, or CLI output.
  **Phase 4 (Streams)** adds the full stream surface.
- Do not expose `safe-update` or `force` reconcile policies. v1 statically
  rejects them. **Phase 2** adds `safe-update`; **Phase 6** adds `force`.

## Config Relationship

`parti.Config` remains the runtime manager configuration used by application
workers. The provision config must not become an unrelated duplicate source of
truth.

Recommended pattern:

- The provision config embeds the manager-facing `parti.Config` control-plane
  fields needed to derive bucket names, TTLs, and handoff enablement (see
  [Control-Plane Config Mapping](#control-plane-config-mapping)).
- Applications should keep one canonical config source and load the relevant
  sections into both runtime `parti.Config` and `provision.Config`.
- `partictl validate -f parti-env.yaml` checks that the provisioned
  control-plane settings are internally valid before workers use them.
- A future generator can emit a starter `parti-env.yaml` from an existing
  runtime config, but v1 does not require a generator to land.

## Control-Plane Config Mapping

`ControlPlaneConfig` mirrors the runtime manager fields that govern
control-plane KV bucket derivation. Each runtime bucket is derived from
`parti.Config` as shown below; v1 `plan`/`apply` must produce a
`jetstream.KeyValueConfig` literal that matches runtime's
`manager_setup.go:ensureKVBucket` exactly (Bucket, History=1, Storage, optional
TTL), plus the Parti ownership marker in `Metadata` (the only intentional
divergence from runtime).

Runtime evidence:
- Stable ID: `m.cfg.KVBuckets.StableIDBucket`, `m.cfg.WorkerIDTTL`,
  `FileStorage` (`manager_setup.go:35`).
- Election: `m.cfg.KVBuckets.ElectionBucket`, `m.cfg.ElectionTimeout`,
  `MemoryStorage` (`manager_setup.go:76`).
- Heartbeat: `m.cfg.KVBuckets.HeartbeatBucket`, `m.cfg.HeartbeatTTL`,
  `MemoryStorage` (`manager_setup.go:80`).
- Assignment: `m.cfg.KVBuckets.AssignmentBucket`,
  `m.cfg.KVBuckets.AssignmentTTL`, `FileStorage` (`manager_setup.go:84`).
- Handoff: `m.cfg.KVBuckets.HandoffBucket`, `m.cfg.KVBuckets.HandoffTTL`,
  `FileStorage` (`manager_setup.go:96`), gated by
  `m.cfg.EnableTwoPhaseHandoff` (`manager.go:407`).
- `KeyValueConfig` literal sets `Bucket`, `History: 1`, `Storage`, and
  conditionally `TTL` (`manager_setup.go:148-156`). Replicas is **not** set.

| Runtime bucket | Source fields on `ControlPlaneConfig` | NATS KV config produced |
|----------------|----------------------------------------|--------------------------|
| Stable ID      | `StableIDBucket`, `WorkerIDTTL`        | `Bucket: <StableIDBucket>`, `Storage: FileStorage`, `History: 1`, `TTL: WorkerIDTTL` (if > 0) |
| Election       | `ElectionBucket`, `ElectionTimeout`    | `Bucket: <ElectionBucket>`, `Storage: MemoryStorage`, `History: 1`, `TTL: ElectionTimeout` (if > 0) |
| Heartbeat      | `HeartbeatBucket`, `HeartbeatTTL`      | `Bucket: <HeartbeatBucket>`, `Storage: MemoryStorage`, `History: 1`, `TTL: HeartbeatTTL` (if > 0) |
| Assignment     | `AssignmentBucket`, `AssignmentTTL`    | `Bucket: <AssignmentBucket>`, `Storage: FileStorage`, `History: 1`, `TTL: AssignmentTTL` (if > 0) |
| Handoff (opt)  | `HandoffBucket`, `HandoffTTL`, gated by `EnableTwoPhaseHandoff` | `Bucket: <HandoffBucket>`, `Storage: FileStorage`, `History: 1`, `TTL: HandoffTTL` (if > 0) |

Notes:

- Bucket name fields default to the same values as the runtime
  `KVBucketConfig` (`config.go:15,18,21,24,37`): `parti-stableid`,
  `parti-election`, `parti-heartbeat`, `parti-assignment`, `parti-handoff`.
  **Defaulting order:** `Validate` first applies these defaults to any
  bucket-name field whose unmarshalled value is the empty string (the YAML
  zero value), then rejects any bucket name that is still empty after
  defaulting. A field omitted from YAML and a field written as `""` are
  treated identically: both default. Operators who need to assert "no
  bucket" cannot do so via empty string in v1; the field would simply be
  re-populated with the runtime default.
- TTL field names match `parti.Config` exactly: `WorkerIDTTL`,
  `ElectionTimeout`, `HeartbeatTTL`, `AssignmentTTL`, `HandoffTTL`. Note
  `ElectionTimeout` (not `ElectionTTL`) and `EnableTwoPhaseHandoff` (not
  `EnableHandoff`); using runtime names eliminates any mapping ambiguity.
- TTL is omitted from `KeyValueConfig` when its value is zero, exactly as
  runtime does (`manager_setup.go:154-156`). `AssignmentTTL=0` is the runtime
  default and means "no expiration" — provision must produce a
  `KeyValueConfig` without a `TTL` field in that case.
- `EnableTwoPhaseHandoff=false` means the handoff bucket is omitted from
  `Plan`. v1 never deletes an existing handoff bucket even if the gate is
  toggled off later; that drift is reported but not actioned.
- **Replicas:** `ControlPlaneConfig` has no `Replicas` field in v1, mirroring
  the runtime `KeyValueConfig` literal which leaves `Replicas` unset (nats.go
  normalizes zero replicas to 1 server-side). If operators require
  multi-replica control-plane KV, they must pre-create those buckets manually
  in v1; provision will report them as `adopted` drift. A `Replicas` field
  lands in **Phase 2 (Safe Update + Adopt)** alongside the safe-update
  contract for live-replica changes.
- The Parti ownership marker is added to `KeyValueConfig.Metadata` (see
  [Resource Ownership Marker](#resource-ownership-marker)). The marker is the
  only intentional divergence from runtime's literal `KeyValueConfig`.

## Resource Ownership Marker

Every resource created by `partictl apply` is stamped with a Parti-managed
marker so `view`, `plan`, and `validate` can distinguish Parti-owned resources
from unrelated NATS objects that happen to share a name.

**Storage location:** the marker lives in the resource's `Metadata` map, not
`Description`. NATS KV `Metadata` (`jetstream.KeyValueConfig.Metadata`) is
designed for structured key-value annotation; `Description` is reserved for
human-readable text and may be set by operators for unrelated reasons.

**Marker keys** (all under the `parti.io/` prefix):

- `parti.io/managed`: schema version, e.g. `v1`.
- `parti.io/component`: one of `control-plane:id`, `control-plane:election`,
  `control-plane:heartbeat`, `control-plane:assignment`,
  `control-plane:handoff`, `partition-source`, or `dynamic-consumer`.
- `parti.io/instance`: optional logical environment identifier (e.g.
  `prod-us-east`); set from `Config.Instance` when provided. The marker is an
  **annotation, not a namespace** — NATS bucket names are globally unique in
  an account, so the marker cannot make two environments share the same
  bucket name. Operators who want true multi-environment isolation must give
  each environment unique bucket names (e.g., `parti-prod-stableid` vs
  `parti-staging-stableid`); the instance marker lets `view`/`plan` filter
  inventory by environment after that.

**Parsing rules:**

- Default `view` filter: a resource is "Parti-managed" iff `Metadata` contains
  the key `parti.io/managed` with any non-empty value.
- Component classification: the `parti.io/component` value selects the
  resource category in `view`/`plan` output. Unknown component values are
  surfaced as `component: unknown` but otherwise treated as Parti-managed.
- Unknown `parti.io/*` keys are ignored (forward compatibility).
- Malformed metadata (missing `parti.io/managed`) is treated as **unmarked**;
  no error is raised.

**Authority boundary:**

- The marker is informational. It never authorizes mutation. Specifically: an
  `apply` operation **never** uses marker presence to decide whether to
  create, update, or skip a resource. Resource lookup is always by exact NATS
  name.
- Config-scoped `plan`/`apply` must always resolve every desired resource by
  exact NATS name first. The marker is read only after the resource is found
  and can only affect the **drift classification** of an existing resource,
  never its existence determination. An implementation that filters by marker
  before name-resolution is a bug.

**Drift classification using the marker:**

- Resource missing entirely → `create-kv` action (Plan emits this in v1).
- Resource present and marked → `informational` (matches) or `drift-mutable` /
  `drift-immutable` based on field comparison.
- Resource present and **unmarked** but named by config → `adopted` drift,
  severity reported; v1 emits no action for this case.

## Proposed SDK

Add a public `provision` package with these core concepts:

```go
// APIVersionV1 is the only config schema accepted by v1.
const APIVersionV1 = "parti.io/v1"

// ReconcilePolicy controls how Apply handles drift. v1 only supports Warn.
type ReconcilePolicy string

const (
    PolicyWarn ReconcilePolicy = "warn"
    // PolicySafeUpdate lands in Phase 2; PolicyForce lands in Phase 6.
    // Each adds explicit field/test coverage. v1 rejects them at Validate().
)

// Config is the desired environment state. APIVersion is required.
type Config struct {
    APIVersion       string                 `yaml:"apiVersion" json:"apiVersion"`
    Instance         string                 `yaml:"instance,omitempty" json:"instance,omitempty"`
    Policy           ReconcilePolicy        `yaml:"policy,omitempty" json:"policy,omitempty"` // default "warn"
    ControlPlane     *ControlPlaneConfig    `yaml:"controlPlane,omitempty" json:"controlPlane,omitempty"`
    PartitionSource  *PartitionSourceConfig `yaml:"partitionSource,omitempty" json:"partitionSource,omitempty"`
    DynamicConsumers []DynamicConsumerCfg   `yaml:"dynamicConsumers,omitempty" json:"dynamicConsumers,omitempty"`
    // Streams is intentionally absent in v1. See Non-Goals.
}

// ControlPlaneConfig mirrors the runtime fields that drive control-plane KV
// bucket creation. Field names and YAML tags match runtime parti.Config and
// parti.KVBucketConfig exactly (some fields live under KVBuckets in runtime;
// see the mapping note below) so callers can copy values without renaming.
type ControlPlaneConfig struct {
    // Bucket names (runtime origin: parti.KVBucketConfig).
    StableIDBucket   string `yaml:"stableIdBucket"   json:"stableIdBucket"`
    ElectionBucket   string `yaml:"electionBucket"   json:"electionBucket"`
    HeartbeatBucket  string `yaml:"heartbeatBucket"  json:"heartbeatBucket"`
    AssignmentBucket string `yaml:"assignmentBucket" json:"assignmentBucket"`
    HandoffBucket    string `yaml:"handoffBucket"    json:"handoffBucket"`

    // TTLs. WorkerIDTTL/HeartbeatTTL/ElectionTimeout live on parti.Config;
    // AssignmentTTL and HandoffTTL live on parti.KVBucketConfig.
    WorkerIDTTL     time.Duration `yaml:"workerIdTtl"     json:"workerIdTtl"`
    ElectionTimeout time.Duration `yaml:"electionTimeout" json:"electionTimeout"`
    HeartbeatTTL    time.Duration `yaml:"heartbeatTtl"    json:"heartbeatTtl"`
    AssignmentTTL   time.Duration `yaml:"assignmentTtl"   json:"assignmentTtl"`   // 0 = no expiration
    HandoffTTL      time.Duration `yaml:"handoffTtl"      json:"handoffTtl"`

    // Gate for the optional handoff bucket. Name matches parti.Config.
    EnableTwoPhaseHandoff bool `yaml:"enableTwoPhaseHandoff" json:"enableTwoPhaseHandoff"`
}

type PartitionSourceConfig struct {
    Bucket       string        `yaml:"bucket"       json:"bucket"`
    Key          string        `yaml:"key"          json:"key"`           // v1 provisions the bucket only; key is metadata
    Replicas     int           `yaml:"replicas,omitempty"     json:"replicas,omitempty"`
    Storage      string        `yaml:"storage,omitempty"      json:"storage,omitempty"`     // "file" | "memory"; default "file"
    History      uint8         `yaml:"history,omitempty"      json:"history,omitempty"`     // default 1
    MaxValueSize int32         `yaml:"maxValueSize,omitempty" json:"maxValueSize,omitempty"`
    TTL          time.Duration `yaml:"ttl,omitempty"          json:"ttl,omitempty"`
}

type DynamicConsumerCfg struct {
    StreamName      string `yaml:"streamName"               json:"streamName"`
    ConsumerPrefix  string `yaml:"consumerPrefix"           json:"consumerPrefix"`
    SubjectTemplate string `yaml:"subjectTemplate"          json:"subjectTemplate"`
    PartitionsRef   string `yaml:"partitionsRef,omitempty"  json:"partitionsRef,omitempty"` // path or inline reference; load is caller-owned
    // v1 intentionally exposes no further options. The pure builder runs
    // against runtime defaults (see PlanDynamicConsumers comment below).
    // Broader option surface (storage/replicas, ackWait, maxDeliver, retry,
    // recovery strategy, etc.) lands in Phase 5 when consumer precreation
    // ships. Operators with non-default runtime consumer.Dynamic options
    // must continue to rely on the runtime to create those consumers;
    // v1 alignment-check only asserts the byte-equivalent subset listed
    // under PlanDynamicConsumers.
}

// Scope selects which resource kinds and optionally which Parti instance View
// considers.
type Scope struct {
    ControlPlane     bool
    PartitionSource  bool
    DynamicConsumers bool
    // Instance optionally restricts results to resources whose
    // parti.io/instance metadata equals this value. Empty means
    // "all instances, including unstamped" (inventory mode).
    Instance string
}

func ScopeAll() Scope                  // every kind v1 understands; Instance=""
func ScopeFromConfig(cfg Config) Scope // every kind set in cfg; Instance=cfg.Instance

// Snapshot is the read-only live state returned by View. All resource slices
// are plural to allow multiple Parti environments in one NATS account
// (distinguished by parti.io/instance marker).
type Snapshot struct {
    APIVersion       string           `json:"apiVersion"` // always "parti.io/provision/v1"
    Kind             string           `json:"kind"`       // always "Snapshot"
    ObservedAt       time.Time        `json:"observedAt"` // included in JSON only; omitted from text
    ControlPlane     []KVBucketState  `json:"controlPlane"`
    PartitionSource  []KVBucketState  `json:"partitionSource"`
    DynamicConsumers []ConsumerState  `json:"dynamicConsumers"`
}

// Plan is the deterministic list of actions Apply would take. Actions are
// sorted by (Kind, Name) so diffs are stable across runs.
type Plan struct {
    APIVersion string           `json:"apiVersion"` // always "parti.io/provision/v1"
    Kind       string           `json:"kind"`       // always "Plan"
    Actions    []PlannedAction  `json:"actions"`
    Drift      []DriftFinding   `json:"drift"`
}

// PlannedAction.Kind in v1 is always "create-kv". Stream action kinds land
// in Phase 4 (Streams); consumer-create lands in Phase 5 (Dynamic Precreate);
// update-* lands in Phase 2 (Safe Update); delete-* lands in Phase 6 (Force).
// None are emitted by v1.
type PlannedAction struct {
    Kind     string `json:"kind"` // v1: "create-kv"
    Name     string `json:"name"`
    Resource any    `json:"resource"` // the would-be jetstream.KeyValueConfig
}

// DriftFinding.Severity:
//   - "informational": resource exists and matches; no action needed
//   - "drift-mutable":   fields differ but live-edit is safe (reserved; v1 never emits)
//   - "drift-immutable": fields differ and require delete/recreate (reported only)
//   - "adopted":         resource exists without the Parti marker (reported only)
type DriftFinding struct {
    Severity string         `json:"severity"`
    Kind     string         `json:"kind"`
    Name     string         `json:"name"`
    Detail   map[string]any `json:"detail,omitempty"`
}

// Report is what Apply returns: what ran, what was skipped, what failed.
type Report struct {
    APIVersion string           `json:"apiVersion"` // always "parti.io/provision/v1"
    Kind       string           `json:"kind"`       // always "Report"
    Executed   []ExecutedAction `json:"executed"`
    Skipped    []SkippedAction  `json:"skipped"`
    Errors     []ResourceError  `json:"errors"`
    Aborted    bool             `json:"aborted"` // true if ctx was cancelled mid-apply
}
```

**Output schema versioning.** Every JSON-emitting struct (`Snapshot`, `Plan`,
`Report`) carries `APIVersion: "parti.io/provision/v1"` and a `Kind` field.
Operator CI keys on these fields; future versions add fields under the same
APIVersion or bump to v2.

Top-level entry points:

```go
func View(ctx context.Context, js jetstream.JetStream, scope Scope) (Snapshot, error)
func Validate(cfg Config) error
func ValidateLive(ctx context.Context, js jetstream.JetStream, cfg Config) (Report, error)
func Plan(ctx context.Context, js jetstream.JetStream, cfg Config) (Plan, error)
func Apply(ctx context.Context, js jetstream.JetStream, cfg Config) (Report, error)
```

### `Validate` / `ValidateLive` / `Plan` / `Apply` boundary

These are distinct phases and must not overlap:

- **`Validate(cfg)`** — pure static validation: required `APIVersion` match,
  required fields, name patterns, policy is `warn`, mutually exclusive
  options. No I/O.
- **`ValidateLive(ctx, js, cfg)`** — pre-flight: JetStream reachability,
  account info access, and the minimum set of permissions listed in the
  [Permissions](#permissions) table for the resource kinds in `cfg`. Does
  **not** compare config to live resources.
- **`Plan(ctx, js, cfg)`** — assumes pre-flight passed; queries each named
  resource by exact name and computes drift. Reads bucket/stream info only;
  does **not** probe the partition-source configured-key read permission
  (that probe is `ValidateLive`-only). Surfaces permission errors only if
  they prevent the resource read.
- **`Apply`** — v1 **always** calls `Validate(cfg)` and `ValidateLive(ctx, js,
  cfg)` internally before mutation. There is no skip flag in v1. A future
  `ApplyValidated` fast path can be added in a later phase if a real
  performance need surfaces; v1 deliberately omits it.

### Cancellation contract

For all entry points, `ctx` cancellation maps to the standard Go contract:

- `View`, `Validate`, `ValidateLive`, `Plan`: return zero-value result and
  `ctx.Err()` on cancellation. These operations are read-only and do not
  produce partial state.
- `Apply`: if cancellation is observed **before** any mutation, returns
  `(Report{Aborted: true}, ctx.Err())` with empty `Executed`. If cancellation
  is observed **after** at least one mutation attempt, returns `(partial,
  ctx.Err())` where `partial.Aborted = true`, `partial.Executed` lists
  completed actions, and remaining planned actions are added to
  `partial.Skipped` with reason `context-cancelled`. Cancellation is not added
  to `partial.Errors`; only resource-level errors live there.
- CLI maps `ctx.Err()` to exit code 4 when caused by `-timeout`, or to the
  operating-system signal default for `SIGINT`/`SIGTERM`.

### Apply failure semantics (non-cancellation)

When an `Apply` action fails for an ordinary resource reason (NATS error,
server-side validation rejection, permission denied at create time, etc.),
`Apply` is **fail-fast**:

- Actions completed before the failure remain in `Report.Executed`.
- The failed action is recorded once in `Report.Errors` as a
  `ResourceError` whose `Name`/`Kind` identify the resource and whose
  error message is the underlying NATS error string.
- All remaining planned actions are added to `Report.Skipped` with
  reason `prior-error`.
- `Report.Aborted` stays **false**. `Aborted` is reserved for context
  cancellation only — it is the marker that distinguishes "the caller
  pulled the plug" from "a resource operation failed."
- `Apply` returns the underlying NATS error wrapped with the resource
  name; the partial `Report` is still returned alongside.
- CLI exit code maps to `1` (runtime/generic error) per the
  [Exit code precedence](#exit-code-precedence) table.

This rule applies to every mutation phase in v1 (control-plane bucket
create, partition-source bucket create). Later phases that add multi-step
actions (e.g., update-then-stamp) inherit the same fail-fast contract:
any sub-step failure stops the run with the partial `Report` as above.

### Permissions

The permission table is API-call-accurate. Operators who grant exactly these
will have `validate -live` pass and `apply` succeed for the corresponding
resources.

| Entry point  | Required NATS API subjects |
|--------------|----------------------------|
| `View` (no `-f`)         | `$JS.API.STREAM.LIST` (preferred; returns configs + metadata in one call) or `$JS.API.STREAM.NAMES` + `$JS.API.STREAM.INFO.*` (two-step fallback) |
| `View` (`-f` scoped)     | `$JS.API.STREAM.INFO.KV_<bucket>` for each named bucket |
| `Validate`               | none (pure) |
| `ValidateLive`           | `$JS.API.INFO`; `$JS.API.STREAM.INFO.KV_<bucket>` for each named bucket; for `partitionSource`, additionally the configured-key read permission (see below) |
| `Plan`                   | `$JS.API.INFO`; `$JS.API.STREAM.INFO.KV_<bucket>` for each named bucket. **Does not** probe the partition-source configured-key read permission — that probe is `ValidateLive`-only. |
| `Apply` (v1, create-kv)  | `ValidateLive` permissions plus `$JS.API.STREAM.CREATE.KV_<bucket>` for each missing bucket |
| Dynamic consumer alignment (live check) | `$JS.API.STREAM.INFO.<stream>` for each named stream; `$JS.API.CONSUMER.INFO.<stream>.<consumer>` (per-durable; preferred) or `$JS.API.CONSUMER.LIST.<stream>` (broader; only when comparing the full consumer set) |

Partition-source key read permissions:

- nats.go `kv.Get` calls `stream.GetLastMsgForSubject` under the hood. The API
  subject used depends on whether the underlying stream has `AllowDirect`.
- **Primary (direct path, `AllowDirect=true`):**
  `$JS.API.DIRECT.GET.KV_<bucket>.$KV.<bucket>.<key>`. nats.go-created KV
  streams always set `AllowDirect=true`
  (`github.com/nats-io/nats.go v1.50.0`, `jetstream/kv.go:669-690`; see
  `go.mod` for the pinned version).
- **Fallback (non-direct path, `AllowDirect=false`):**
  `$JS.API.STREAM.MSG.GET.KV_<bucket>`. This matters because Parti opens
  pre-existing KV buckets without reconciling config
  (`kvutil/bucket.go:12-20`); operators who created their partition-source
  bucket outside nats.go may not have `AllowDirect` enabled.
- `ValidateLive` should attempt the appropriate subject based on the live
  bucket's `AllowDirect` setting (read once via `STREAM.INFO`). To avoid a
  TOCTOU race when an operator toggles `AllowDirect` between the info read
  and the probe, on permission failure `ValidateLive` re-reads `STREAM.INFO`
  once and retries with the path implied by the refreshed value. Operators
  who want fully race-free validation while reconfiguring streams should
  grant both `$JS.API.DIRECT.GET.KV_<bucket>.$KV.<bucket>.<key>` and
  `$JS.API.STREAM.MSG.GET.KV_<bucket>`.

Notes:

- nats.go supports custom JetStream API prefixes (`<prefix>.STREAM.INFO.<stream>`)
  and domains (`$JS.<domain>.API.STREAM.INFO.<stream>`); substitute the
  configured prefix/domain when documenting deployment-specific grants.
- For the `partitionSource` bucket, `ValidateLive` probes the configured key
  with a get; **key non-existence is not an error** in v1 (partition records
  are managed by Phase 3). The probe only verifies the operator can read the
  key when records exist.
- `ValidateLive` never subscribes a watch (a transient subscription has no
  good failure signal); the closest read permission is verified instead.
- `ValidateLive` never writes the partition-source key.
- `$KV.<bucket>.<key>` is the stored message subject, **not** an API
  permission subject. It must not be granted as a JetStream API permission.

## Dynamic Consumer Planning

Two helpers, separated by purity:

```go
// PlanDynamicConsumers is a pure builder. It validates input and constructs
// PlannedConsumer entries using the same subject generation, durable naming,
// and ConsumerConfig logic as runtime consumer.Dynamic, scoped to the v1
// alignment-check surface. It performs no I/O.
//
// v1 surface and equality scope:
//   - The builder accepts only stream/prefix/template/partitions; it has no
//     option variadic. Runtime equality is asserted against runtime defaults
//     for every other field on durable.WorkerConsumerConfig.
//   - The PlannedConsumer.Config the test compares against runtime is the
//     subset deterministically produced from those inputs at runtime
//     defaults: Durable, FilterSubject(s), AckPolicy=AckExplicit,
//     DeliverPolicy=DeliverAll, and any other field that internal/durable
//     unconditionally sets at runtime defaults (see W4 sub-spec for the
//     exact, enumerated list and the corresponding source lines).
//   - All other fields are explicitly out of v1 equality scope and the W4
//     test must not assert them. The list lives in the W4 sub-spec, not
//     in this plan, so it can be refined as the durable extraction lands.
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

// ValidateLiveDynamicConsumers performs the live compatibility checks that
// runtime Dynamic.Update performs, including the WorkQueuePolicy recovery
// strategy compatibility check. v1 ValidateLive calls this for each
// DynamicConsumerCfg entry.
func ValidateLiveDynamicConsumers(
    ctx context.Context,
    js jetstream.JetStream,
    cfgs []DynamicConsumerCfg,
) error
```

**Extraction strategy.** Extract the pure builder from the
`internal/durable.WorkerConsumer` methods that already construct subjects,
durable names, and consumer configs (e.g., `buildSubjects` / `generateSubject`,
`perSubjectDurableName`, `ensurePerSubjectConsumer` config construction). The
`provision` package consumes the exported builder; runtime `consumer.Dynamic`
continues to call the same shared logic. No private durable-naming code is
duplicated in `provision`.

**Live coupling.** `ValidateLiveDynamicConsumers` calls the same
`checkWorkQueueRecoveryCompat` path used by `Dynamic.Update`. This is required
because pure planning cannot detect that a WorkQueuePolicy stream rejects
`RecoverFromNew` / `RecoverFromLastProcessed` — that decision depends on live
stream config. Tests must cover both:

1. **Scoped** pure config equality: for the enumerated subset of
   `ConsumerConfig` fields produced deterministically from `(stream, prefix,
   subjectTemplate, partitions)` at runtime defaults (Durable, FilterSubject,
   AckPolicy=AckExplicit, DeliverPolicy=DeliverAll, plus any other field the
   extracted builder unconditionally sets — the exact list is fixed in the
   W4 sub-spec), the planned `ConsumerConfig` matches what runtime
   `consumer.Dynamic` would build for the same partitions at the same
   defaults. Fields outside this enumerated subset (AckWait, MaxDeliver,
   ConsumerReplicas, RecoveryStrategy, etc.) are explicitly **out of
   equality scope for v1**.
2. Live WorkQueuePolicy rejection: `ValidateLiveDynamicConsumers` returns the
   same error as `Dynamic.Update` against an incompatible stream.

Implementation priority:

1. Pure builder + read-only alignment validation land in v1.
2. Optional consumer **precreation** lands in **Phase 5 (Dynamic Precreate)**
   after the pure builder is proven byte-equivalent to runtime in Phase 1.
3. Delete/recreate of existing dynamic consumers lands in **Phase 6
   (Force + Repair)**; cursor-loss behavior is covered by that phase's test
   discipline.

## YAML Config Examples

The v1 CLI consumes YAML files matching the `provision.Config` struct shape
defined under [Proposed SDK](#proposed-sdk). All field names below are
canonical — they correspond verbatim to the `yaml` tags on the public structs.

### Minimal — control plane only

```yaml
apiVersion: parti.io/v1
controlPlane:
  workerIdTtl: 75s
  electionTimeout: 10s
  heartbeatTtl: 15s
```

Bucket names default to the same values as `parti.KVBucketConfig`
(`parti-stableid`, `parti-election`, `parti-heartbeat`, `parti-assignment`,
`parti-handoff`), so they can be omitted unless overridden. `assignmentTtl`
defaults to `0` ("no expiration"). Handoff is off unless
`enableTwoPhaseHandoff: true`.

### Full — every v1 block populated

```yaml
# Schema version. Required. v1 accepts only "parti.io/v1".
apiVersion: parti.io/v1

# Optional logical environment name. Stamped on every created resource as
# parti.io/instance metadata. Used for `partictl view -instance=...`
# filtering. The marker is annotation-only — NATS bucket names are globally
# unique, so true multi-environment isolation requires distinct bucket names
# per environment.
instance: prod-us-east

# v1 only accepts "warn" (create missing, never mutate existing).
# safe-update and force are rejected by static Validate in v1.
policy: warn

controlPlane:
  # Bucket names. Defaults match parti.KVBucketConfig defaults.
  stableIdBucket: parti-stableid
  electionBucket: parti-election
  heartbeatBucket: parti-heartbeat
  assignmentBucket: parti-assignment
  handoffBucket: parti-handoff

  # TTLs. Field names and values mirror parti.Config exactly so a single
  # canonical source can populate both this file and runtime parti.Config.
  workerIdTtl: 75s
  electionTimeout: 10s
  heartbeatTtl: 15s
  assignmentTtl: 0s         # 0 = no expiration (matches runtime default)
  handoffTtl: 2m

  # Gate for the optional handoff bucket. When true, handoffTtl must be > 0.
  enableTwoPhaseHandoff: true

partitionSource:
  # Bucket that holds the partition definition KV. Provisioned in v1.
  bucket: parti-partitions
  # Key under which the partition list lives. v1 provisions the bucket
  # only — partition record writes land in Phase 3 (Partition Records).
  key: partitions/v1
  replicas: 3
  storage: file              # "file" | "memory"; default "file"
  history: 1                 # default 1
  maxValueSize: 1048576      # optional
  ttl: 0s                    # optional; 0 = no expiration

dynamicConsumers:
  # v1: alignment-check only. Precreation is a deferred follow-up.
  - streamName: orders
    consumerPrefix: ord-worker
    subjectTemplate: "orders.{partition_id}.>"
    partitionsRef: ./partitions.yaml   # caller-owned partition list
```

### Intentionally absent from v1

- `controlPlane.replicas` — runtime `KeyValueConfig` doesn't set Replicas
  (nats.go normalizes 0→1 server-side). Operators needing multi-replica
  control-plane KV must pre-create those buckets; they will appear as
  `adopted` drift in v1. The field lands in **Phase 2 (Safe Update + Adopt)**
  alongside the safe-update contract for live-replica changes.
- `streams:` block — **Phase 4 (Streams)**; no field on `Config` in v1.
- `partitions:` records — partition-source bucket is provisioned but its key
  contents are never written by v1.
- Templating / overlays — none. Dev/stage/prod are separate rendered files.
- `policy: safe-update` and `policy: force` — rejected by static `Validate`.

### Validation behavior

The YAML-unmarshal test (see [Test Plan](#test-plan)) asserts every nested
field above populates via its declared YAML tag, not Go default-name mapping.
A representative YAML file matching the "Full" example is the canonical
fixture for that test.

## CLI

Add `cmd/partictl` with a testable `run(args []string, stdout, stderr io.Writer) int`
entrypoint.

Commands (v1):

- `partictl view [-f parti-env.yaml]`
- `partictl validate -f parti-env.yaml [-live]`
- `partictl plan -f parti-env.yaml`
- `partictl apply -f parti-env.yaml [-dry-run]`

Deferred to later phases (see [Phased Roadmap](#phased-roadmap)):

- `partictl partitions plan/apply` — **Phase 3 (Partition Records)**.
- `partictl adopt` — **Phase 2 (Safe Update + Adopt)**.
- `partictl stream view/plan/apply` — **Phase 4 (Streams)**.
- `partictl init` / `partictl emit` (config generators) — cross-cutting
  follow-up, not gated on a specific phase.

Common flags:

- `-f`: YAML config path.
- `-server`: NATS URL; default from `NATS_URL`, then `nats.DefaultURL`.
- `-creds`, `-nkey`, `-token`: authentication options.
- `-timeout`: operation timeout (default 30s).
- `-json`: emit machine-readable output (`Snapshot`, `Plan`, or `Report`
  depending on command; each carries `apiVersion` + `kind`).
- `-fail-on-drift`: when `plan` (or `apply -dry-run`) finds any unresolved
  drift, exit with code 2. In v1, `warn` policy never resolves drift, so any
  non-informational drift triggers code 2 when the flag is set.
- `-instance`: for `view` (no `-f`) and `validate`, restrict results to
  resources whose `parti.io/instance` metadata matches this value. Ignored
  when `-f` is set (config-scoped commands derive instance from
  `Config.Instance`). Default is "" (all instances).

### Exit codes

| Code | Meaning                                                  |
|------|----------------------------------------------------------|
| 0    | success / `ok`                                           |
| 1    | runtime / generic error (operation failed after preflight passed) |
| 2    | drift detected and not resolved (only with `-fail-on-drift`) |
| 3    | static or live validation error                          |
| 4    | NATS connect, auth, or context timeout/cancel failure    |

### Exit code precedence

When multiple error conditions occur during a single command, the highest-
priority code wins. Highest priority first:

1. Command/flag parse error → code `3`.
2. Static `Validate` failure → code `3`.
3. NATS connect, auth, or context cancellation/timeout → code `4`.
4. Live validation failure (`ValidateLive`) → code `3`.
5. Runtime operation failure during `plan`/`apply` (e.g., partial NATS error)
   → code `1`.
6. Unresolved drift with `-fail-on-drift` set → code `2`.
7. Otherwise → code `0`.

A context-deadline or `SIGINT` during `apply` maps to code `4`; the partial
`Report` is still emitted on stdout when `-json` is set.

### Output

- `view`: live resource table scoped by `-f` (or all marker-matched if absent).
- `validate`: validation errors or `ok`.
- `plan`: deterministic action table (sorted by `Kind`, then `Name`) plus
  drift findings grouped by severity.
- `apply`: executed/skipped/failed action table; with `-dry-run`, emits the
  same `Plan` payload as `plan` (byte-identical for `-json`).

## Safety Rules

- Default and only v1 policy is `warn`: non-disruptive, create-missing only.
- v1 emits exactly one `PlannedAction.Kind`: `"create-kv"`. Stream and
  consumer action kinds are reserved and never emitted in v1.
- Existing resource drift is reported but never mutated in v1.
- Config-scoped operations always resolve resources by **exact NATS name
  first**. The ownership marker only affects drift classification of resources
  that already exist by name. Marker-first filtering of config-scoped
  operations is a bug.
- `apply` never writes when `-dry-run` is set.
- Partition content writes are out of scope for v1; the partition-source
  bucket is provisioned but never written to.
- Planner output is deterministic: actions and drift findings are sorted by
  `(Kind, Name)`. Wall-clock timestamps are excluded from default text output
  and included only in `-json` output (e.g., `Snapshot.ObservedAt`).
- `plan` is the canonical dry run. `apply --dry-run` calls the same planning
  path and emits an identical `Plan` JSON payload.
- `Validate` and `ValidateLive` are both called by `Apply` in v1. There is no
  skip path.

### KV field mutability (informational; v1 does not mutate)

When `safe-update` lands in **Phase 2** and `force` lands in **Phase 6**, the
following NATS KV field mutability rules apply. v1 reports drift only — it
neither mutates nor delete/recreates — but the categories below are recorded
here so later phases inherit a consistent contract.

| Field          | Mutability        | v1 drift severity   |
|----------------|-------------------|---------------------|
| `Description`  | mutable in place  | drift-mutable       |
| `Metadata`     | mutable in place  | drift-mutable       |
| `MaxBytes`     | mutable in place  | drift-mutable       |
| `MaxValueSize` | mutable in place  | drift-mutable       |
| `TTL`          | mutable in place  | drift-mutable       |
| `History`      | immutable         | drift-immutable     |
| `Storage`      | immutable         | drift-immutable     |
| `Replicas`     | conditionally mutable (server-version-dependent) | drift-immutable (conservative) |

v1 reports immutable drift as **manual remediation required** in the
`plan`/`apply -dry-run` output. Operators must decide whether to delete and
recreate themselves; v1 will not.

## Work Items

0. **Extract shared `KeyValueConfig` builder.** Move the
   `(*Manager).ensureKVBucket` `KeyValueConfig` literal
   (`manager_setup.go:148-156`) behind a pure, exported builder that takes
   `(bucket string, ttl time.Duration, storage jetstream.StorageType)` and
   returns `jetstream.KeyValueConfig` without performing I/O. Place it in an
   importable package (`internal/kvbuckets` or similar, re-exported via a thin
   `parti` wrapper for `provision`'s use). Update `manager_setup.go` to call
   the new builder; behavior is unchanged. This is a prerequisite for the
   byte-equivalence invariant in W1/W2 — without it, provision tests would
   either duplicate the literal (defeating the invariant) or call unexported
   runtime code. W0 ships as a refactor PR with no behavior change.
1. Add `provision.Config`, `apiVersion` handling, `Validate` (static), `View`,
   and read-only `Plan` for Parti control-plane KV buckets. Implement marker
   read/parse helpers. Each `create-kv` action's `KeyValueConfig` is built by
   calling the W0 shared builder and then adding the Parti `Metadata` marker.
   (`apply` deferred to W2.)
2. Add create-missing `Apply` for control-plane KV buckets, stamping the
   ownership marker in `Metadata`. Add `ValidateLive` (API-call-accurate
   pre-flight permissions/reachability). Wire `Apply` to always call
   `Validate` + `ValidateLive` internally.
3. Add partition-source bucket provisioning (`view`/`plan`/`apply`). Partition
   record management remains deferred.
4. Add dynamic-consumer alignment: pure `PlanDynamicConsumers` builder
   (extracting shared logic from `internal/durable`) plus
   `ValidateLiveDynamicConsumers` for WorkQueuePolicy compatibility checks.
5. Add `cmd/partictl` with `view`, `validate [-live]`, `plan`,
   `apply [-dry-run]`. Wire exit codes (with documented precedence) and
   `-json` output (with `apiVersion`/`kind` envelopes).

Work items 0–5 form the **Phase 1** launch gate; W0 is a no-behavior-change
refactor prerequisite. Phases 2–7 each land their own work items in their
own design plans (see [Phased Roadmap](#phased-roadmap)). Each later phase
inherits this plan's invariants verbatim — the W0 shared `KeyValueConfig`
builder, the ownership marker shape, the JSON envelope schema, the CLI exit
codes, and the determinism rules.

## Implementation Workflow

Each work item ships as its own PR. The standard per-PR loop, matching the
project convention recorded in CLAUDE memory and `AGENTS.md`:

```
sub-spec (if needed) → impl → /simplify → /codex:review (or /post-impl-review)
  → fix → re-review → squash on merge verdict
```

- A **sub-spec** is a short markdown file (1–2 pages) under
  `docs/plans/provision-sdk-cli/` named `<NN>-<wname>-spec.md` (e.g.
  `02-w2-apply-validatelive-spec.md`). It is required when a work item adds
  public API surface or new contract semantics not fully nailed down in this
  plan. For mechanical refactors (W0) and straightforward CLI wiring (W5),
  the work-item bullet above is sufficient.
- **`/simplify`** runs after the implementation lands to fold redundancy and
  cut accidental complexity before review.
- **`/codex:review`** is the default post-impl reviewer. Fall back to
  `/post-impl-review` (copilot) only if codex is unavailable. Effort levels
  match `AGENTS.md`: `xhigh` for v1/v2 reviews of new public-API PRs, `high`
  for refactor PRs and v3+ rounds.
- **Squash** on merge once the reviewer returns a clean verdict.

### Recommended model + effort per work item

The implementer column refers to the local Claude Code model that writes the
code (or this CLI's planner if a sub-spec is required first). The reviewer
column is the codex effort level for the post-impl review.

| W  | Title                                            | Sub-spec needed? | Sub-spec planner          | Implementer       | Codex review effort |
|----|--------------------------------------------------|------------------|---------------------------|-------------------|---------------------|
| W0 | Shared `KeyValueConfig` builder (refactor)       | No               | —                         | **Sonnet 4.6**    | `medium`            |
| W1 | `Config`, `Validate`, `View`, read-only `Plan`   | No (this plan covers it) | —                  | **Opus 4.7**      | `xhigh`             |
| W2 | `Apply` + `ValidateLive` (perm probes, retry)    | **Yes**          | **Opus 4.7**              | **Opus 4.7**      | `xhigh`             |
| W3 | Partition-source bucket view/plan/apply          | No               | —                         | **Sonnet 4.6**    | `high`              |
| W4 | Dynamic-consumer alignment (extraction + live)   | **Yes**          | **Opus 4.7**              | **Opus 4.7**      | `xhigh`             |
| W5 | `cmd/partictl` CLI (commands, exit codes, JSON)  | No               | —                         | **Sonnet 4.6**    | `high`              |

Rationale:

- **W0** is a behavior-preserving refactor of a 9-line literal in
  `manager_setup.go` behind a new pure helper. Sonnet 4.6 is the efficient
  default; `medium` codex effort is enough to verify the literal is
  preserved and no caller drifts.
- **W1** introduces the public `provision.Config`, `Plan`, `Snapshot`, and
  `View` surface. Opus 4.7 for the implementer because the public-API shape
  is load-bearing on every later PR and on operator JSON tooling. `xhigh`
  review because new public API.
- **W2** adds `Apply` and `ValidateLive` with the API-call-accurate
  permission probes and the `AllowDirect` single-retry. The permission table
  is correct in the plan but the per-probe sequence (info read → probe →
  refresh-on-failure → retry) needs a short sub-spec to nail down error
  classes, retry conditions, and which permission failures map to which
  exit codes. Opus 4.7 for both the sub-spec and the implementation; `xhigh`
  review because the mutation path is here.
- **W3** is parallel to W1/W2 but for the partition-source bucket only.
  Sonnet 4.6 is sufficient because the shape mirrors W1/W2's contract; the
  partition-source-specific subtleties (immutable `History`, AllowDirect
  probe, never-write-key invariant) are already locked in this plan.
- **W4** is the dynamic-consumer alignment work. A sub-spec is required
  because the pure builder must be extracted from `internal/durable` at a
  named seam (subject build, durable name, ConsumerConfig construction) and
  `ValidateLiveDynamicConsumers` must reuse `checkWorkQueueRecoveryCompat`
  without copying. Opus 4.7 for both the sub-spec and the implementation
  because the extraction touches runtime code paths; `xhigh` review.
- **W5** is the CLI surface (flag parsing, exit-code precedence, JSON
  envelopes, golden tests). Sonnet 4.6 because there is no new public-API
  reasoning beyond what the SDK already establishes; the CLI is glue. `high`
  codex review (not `xhigh`) because the SDK underneath has already been
  reviewed at `xhigh` and the CLI surface is mostly mechanical.

### Final plan review before W0 starts

This plan has already cleared four rounds of `codex:codex-rescue` review
(see `tmp/operator/codex-review-v1.md` through `v4.md`, verdict CLEAN).
Before opening the W0 PR, run a single `/final-plan-review` pass to catch
any residual drift introduced by additions to this plan (YAML examples,
this workflow section). That review is `high` effort — a precision pass,
not another architectural round.

## Test Plan

Static / unit:

- `provision.Config` defaults, YAML/JSON tags, required `apiVersion` (only
  `parti.io/v1` accepted), rejection of `safe-update` / `force` policy values
  in v1.
- `Validate` applies runtime bucket-name defaults to omitted/empty
  `StableIDBucket` / `ElectionBucket` / `HeartbeatBucket` /
  `AssignmentBucket` / `HandoffBucket` before validating; rejects any
  bucket name still empty after defaulting (this case is unreachable from
  valid YAML and exists only to guard against API callers explicitly
  zeroing a field after defaulting). Rejects zero `WorkerIDTTL` /
  `ElectionTimeout` / `HeartbeatTTL`, and `EnableTwoPhaseHandoff=true`
  with zero `HandoffTTL`. `AssignmentTTL=0` is accepted (matches runtime
  default "no expiration"). Includes a test asserting the minimal YAML
  example (control-plane block with only the three required TTLs) passes
  validation after defaulting.
- Deterministic `Plan` output: shuffle inputs, assert identical sorted output.
- `Scope`, `ScopeAll`, `ScopeFromConfig` correctness.
- Exit code mapping for each error class, including precedence, via the
  testable `run` entrypoint.
- JSON envelope: `Snapshot`, `Plan`, `Report` all carry `apiVersion` and
  `kind` fields in their JSON encoding.

Embedded-NATS integration:

- `view` against an empty server returns an empty snapshot.
- `view` against a server with mixed marked / unmarked buckets includes only
  marked ones by default.
- `view` (no `-f`, no `-instance`) against a server with two
  `parti.io/instance` values returns both groups under their respective
  control-plane / partition-source slices (inventory mode).
- `view -instance=prod` returns only resources stamped with
  `parti.io/instance=prod`; unstamped resources and other instance values are
  excluded.
- `view -instance=prod -f cfg.yaml` (where cfg.yaml has
  `instance: staging`): the `-f` config wins; `-instance` is ignored and
  results are config-scoped to staging resources.
- `plan` against an empty server lists `create-kv` actions for each
  control-plane and partition-source bucket in config; each control-plane
  action's would-be `KeyValueConfig` is **byte-equivalent** to what the shared
  W0 builder returns for the same inputs (Bucket, History=1, Storage, TTL when
  > 0; Replicas unset), plus the Parti ownership marker in `Metadata`. The
  test calls the W0 builder directly and compares against `provision.Plan`'s
  emitted `KeyValueConfig` with `Metadata` removed.
- Unmarshalling a representative `parti-env.yaml` populates every nested
  field on `ControlPlaneConfig`, `PartitionSourceConfig`, and
  `DynamicConsumerCfg` via the documented YAML tags. Test fails if any
  nested field uses Go default-name mapping rather than its declared tag.
- `apply` creates missing control-plane buckets with the correct
  `Metadata["parti.io/managed"]` and `Metadata["parti.io/component"]`;
  re-running `plan` reports no drift; re-running `apply` is a no-op.
- **Marker-doesn't-hide-by-name invariant:** create a bucket manually (no
  marker) with a name matching config; `plan` reports `adopted` drift and
  emits **no** `create-kv` action. `apply` performs no mutation. Re-run with
  a different name: `plan` correctly emits `create-kv` for the unrelated
  desired bucket.
- `plan` against a manually-created bucket with mismatched mutable fields
  reports `drift-mutable`; with mismatched immutable fields reports
  `drift-immutable`. v1 emits no `update-*` actions in either case.
- `ValidateLive` returns permission errors that map to API subjects in the
  [Permissions](#permissions) table when those grants are missing.
- `ValidateLive` for `partitionSource` succeeds when the bucket exists and is
  readable but the configured key does not yet exist. The test covers both
  `AllowDirect=true` (nats.go-created) and `AllowDirect=false` (pre-existing)
  bucket variants, asserting the appropriate API subject is probed in each
  case.
- `Apply` with cancellation **before** any mutation: returns `Report{}`
  with `Aborted=true`, empty `Executed`, error is `ctx.Err()`.
- `Apply` with cancellation **mid-way**: returns partial `Report` with
  `Aborted=true`, `Executed` lists completed creates, remaining actions are
  in `Skipped` with reason `context-cancelled`, error is `ctx.Err()`.
- `Apply` with a **non-cancellation** mid-flight failure (e.g., server
  rejects a create due to a forced server error): returns partial `Report`
  with `Aborted=false`, `Executed` lists completed creates, the failing
  action is in `Errors` once, remaining planned actions are in `Skipped`
  with reason `prior-error`, and the returned error wraps the underlying
  NATS error with the resource name.
- Dynamic-consumer alignment: planned durable names and the enumerated
  subset of `ConsumerConfig` fields (defined in the W4 sub-spec) match
  what `consumer.Dynamic` would create for the same partitions at runtime
  defaults; fields outside the enumerated subset are not asserted.
- Dynamic-consumer live: `ValidateLiveDynamicConsumers` returns the same
  WorkQueuePolicy recovery-strategy error as `Dynamic.Update` against an
  incompatible stream.

CLI:

- Each command's text and `-json` output is golden-tested against fixtures.
- `apply -dry-run` and `plan` emit byte-identical `-json` output.
- `-fail-on-drift` exits 2 when drift is present and 0 otherwise.
- Exit-code precedence: simultaneous invalid YAML + unreachable NATS → exit
  3 (parse wins). Permission denied during `plan` → exit 3. Drift +
  successful read → exit 2 with `-fail-on-drift`, exit 0 without.

## Acceptance Criteria

- A user can run `partictl view` against any NATS server with credentials and
  see every Parti-managed resource (filtered by the `parti.io/managed`
  metadata key) without supplying a config.
- A user can run `partictl plan -f parti-env.yaml` against an empty NATS
  server and see the exact Parti control-plane and partition-source resources
  that would be created. Each control-plane `create-kv` action's
  `KeyValueConfig` is byte-equivalent to what `manager_setup.go:ensureKVBucket`
  would build for the same `parti.Config` values (Bucket, History=1, Storage,
  TTL when > 0; Replicas unset), plus the Parti ownership marker in
  `Metadata`.
- A user can run `partictl apply -f parti-env.yaml` to create missing
  control-plane buckets and the partition-source bucket, each stamped with
  `Metadata["parti.io/managed"] = "v1"` and the appropriate
  `parti.io/component` value.
- Re-running `apply` against matching resources is a no-op and reports the
  same plan.
- Re-running `plan` against drifted resources reports drift without mutation;
  immutable drift is flagged as **manual remediation required**.
- A manually-created bucket without the Parti marker is reported as `adopted`
  drift and **never mutated** by v1 `apply`.
- `partictl validate -f parti-env.yaml -live` catches unavailable JetStream,
  missing per-subject NATS permissions (matching the
  [Permissions](#permissions) table), and unsupported policy values before
  workers start.
- `partictl apply -f parti-env.yaml -dry-run` performs no mutation and emits
  a `Plan` JSON byte-identical to `partictl plan`.
- `Apply` cancelled mid-flight returns a partial `Report` with `Aborted=true`
  whose `Executed` + `Skipped` cover every planned action.
- Dynamic-consumer alignment checks report whether planned durable names
  and the enumerated subset of config fields (defined in the W4 sub-spec)
  match what runtime `consumer.Dynamic` would create at runtime defaults.
- `ValidateLiveDynamicConsumers` rejects WorkQueuePolicy streams with
  incompatible recovery strategies, matching the runtime check.
- CLI exits with the documented codes and precedence for each error class.
- Every `-json` output payload carries `apiVersion: "parti.io/provision/v1"`
  and a `kind` field.
- `partictl view -instance=<name>` returns only resources whose
  `parti.io/instance` metadata matches `<name>`. When `-instance` is absent,
  no-config `view` returns all marker-matched resources grouped by instance
  (including unstamped resources under instance `""`).
- `partictl validate -f parti-env.yaml -live` correctly probes the
  partition-source key using `$JS.API.DIRECT.GET.KV_<bucket>.$KV.<bucket>.<key>`
  when the bucket has `AllowDirect=true` and falls back to
  `$JS.API.STREAM.MSG.GET.KV_<bucket>` otherwise.
- The before/after operational impact is visible in review: a fresh
  environment can move from manual NATS setup to one validated plan/apply
  loop, while drift is reported before runtime startup rather than discovered
  as worker failures.

## Assumptions

- CLI file input is YAML-only in v1.
- `apiVersion: parti.io/v1` is the only schema accepted by v1; mismatched
  versions are a static validation error.
- All JSON output payloads use `apiVersion: parti.io/provision/v1` (note the
  `provision/` segment to distinguish output schema from input config schema).
- Public config structs include JSON tags even though the v1 CLI does not need
  a JSON loader.
- The default provision path is create-missing plus drift reporting; no
  mutation of existing resources in v1.
- Destructive repair (delete/recreate) is out of scope for v1; it lands in
  **Phase 6 (Force + Repair)**.
- Kubernetes controller integration lands in **Phase 7 (K8s Controller)** as
  a layer on top of the SDK, not a replacement.
- Dev/stage/prod differences are handled by separate rendered config files
  or SDK-owned config construction in v1; first-class overlays are a
  cross-cutting follow-up (see [Phased Roadmap](#phased-roadmap)).
- Partition record management (`partictl partitions plan/apply`) lands in
  **Phase 3 (Partition Records)**; v1 provisions the bucket but never
  writes records.
- Stream provisioning lands in **Phase 4 (Streams)**; v1 exposes no
  stream-related public API.
