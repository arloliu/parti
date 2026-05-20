# Parti Provision SDK and CLI — Phase 2 Plan ("Safe Update + Adopt")

## Problem Statement

Phase 1 of the Parti provision SDK and `partictl` CLI shipped a `warn`
reconcile policy: it creates missing control-plane and partition-source
buckets, reports drift on existing resources, and refuses to mutate. Phase 1
gives operators visibility into desired-vs-live state but offers no path to
*resolve* drift without leaving the tool.

Two real operator workflows are still painful after Phase 1:

1. **Brownfield NATS.** Operators with pre-existing Parti control-plane or
   partition-source buckets (created manually, by NATS CLI, or by an older
   tool) see every such bucket reported as `adopted` drift on every `plan` /
   `apply` run. Phase 1 emits no action to transition those resources into the
   managed set; the operator's only option is to manually stamp the Parti
   ownership marker via the NATS CLI, then re-run `plan`.

2. **In-place reconcile.** Operators who tune a `parti-env.yaml` TTL,
   metadata, or size limit see Phase 1 report `drift-mutable` findings but
   refuse to act on them. Operators must drop down to the NATS CLI to do
   the in-place update — and once they do, the next `partictl plan` run
   reports the bucket as `informational` rather than crediting the operator's
   tool with the change.

The result is a tool that knows what should change but can't make it
happen. Phase 2 closes that gap for the **non-destructive** half of the
mutation surface — fields NATS can reconcile via `UpdateStream` without
delete/recreate, plus the adoption transition for unmarked resources.

Destructive repair (delete/recreate of immutable-drift buckets like a
History change) remains a Phase 6 problem.

## End-State Vision

This plan is **Phase 2** of the
[Phased Roadmap](../provision-sdk-cli/00-implementation-plan.md#phased-roadmap)
in the master plan. After this phase ships:

- Operators can run `partictl adopt -f parti-env.yaml` to stamp the Parti
  ownership marker on every resource named by config that exists live but
  lacks the marker. Adoption never touches data or config — only the
  marker keys are added; non-Parti metadata keys are preserved.
- Operators can run `partictl apply -f parti-env.yaml --policy=safe-update`
  to create missing resources and reconcile in-place every drift-mutable
  field on resources that already carry the Parti marker.
- Operators can declare `controlPlane.replicas` to ask Parti's control-plane
  KV buckets to run with a non-default replica count. Replicas changes are
  carried through `safe-update`; the NATS server enforces cluster-peer
  feasibility.

Subsequent phases (3–7) still own their own surface: partition records
(Phase 3), application streams (Phase 4), dynamic consumer precreation
(Phase 5), destructive repair (Phase 6), Kubernetes controller (Phase 7).

## Invariants Inherited from Phase 1

Every invariant in the
[Phase 1 "Invariants inherited by every phase"](../provision-sdk-cli/00-implementation-plan.md#invariants-inherited-by-every-phase)
section continues to hold in Phase 2, with **one narrowing** called out
below. The full list, restated for clarity:

- Ownership marker shape (`parti.io/managed`, `parti.io/component`,
  `parti.io/instance`) stays in `Metadata` and remains informational.
- JSON envelope schema stays at `apiVersion: parti.io/provision/v1`. Phase 2
  adds new `PlannedAction.Kind` string values (`update-kv`, `stamp-marker`)
  and new `ReconcilePolicy` values (`adopt`, `safe-update`); all additive.
  Operator CI that keys on `apiVersion` continues to work; tooling that
  filters by `kind == "create-kv"` continues to ignore the new kinds.
- Input config `apiVersion: parti.io/v1` accepts additive fields. Phase 2
  adds `controlPlane.replicas`; YAML omitting the field continues to load
  with no behavior change.
- CLI exit codes 0–4 are stable. Phase 2 introduces no new codes.
- `Plan` action ordering remains deterministic `(Kind, Name)`. New kinds
  sort alphabetically against existing `create-kv`.
- Resource lookup is always by exact NATS name first; the marker still
  never authorizes mutation.

### The one narrowing — control-plane byte-equivalence

Phase 1 promised that the provisioned control-plane `KeyValueConfig` is
byte-equivalent to `manager_setup.go:ensureKVBucket` via the shared
`internal/kvbuckets.BuildKeyValueConfig` builder (`internal/kvbuckets/builder.go`).
Phase 2 **narrows** this invariant to accommodate `controlPlane.replicas`:

> When `ControlPlaneConfig.Replicas == 0` (the default, matching omitted
> YAML), the produced `KeyValueConfig` remains byte-equivalent to the
> Phase 1 builder output — including `Replicas` left zero, which nats.go
> normalizes to 1 server-side. When `ControlPlaneConfig.Replicas > 0`, the
> Phase 2 plan-emission code stamps that value onto the post-builder
> `KeyValueConfig`. Byte-equivalence is intentionally broken for the
> Replicas field only; every other field (Bucket, History, Storage, TTL,
> Metadata) remains byte-equivalent.

Practical consequence: the `internal/kvbuckets.BuildKeyValueConfig`
function **behavior and signature** are not modified in Phase 2. It
continues to return the historical literal. W0 updates only the
**comment block** in `internal/kvbuckets/builder.go` to document the
narrowed invariant and to note that the provision package stamps
`Replicas` post-builder. The Phase 2 plan emitter calls the builder,
then conditionally sets `Replicas` on the result before stamping the
Parti marker. Runtime `Manager.Start` continues to call the builder
with no Replicas concept.

This split is deliberate: Phase 2 introduces a provision-only Replicas
contract. The runtime manager does not gain a Replicas concept; operators
that scale control-plane replicas do it via provision, not via the runtime.

## Goals

- Add a `safe-update` reconcile policy that performs in-place
  `UpdateKeyValue` for the **operator-expressible** drift-mutable fields
  on Parti-marked resources: `Metadata` (component / instance keys),
  `TTL`, `MaxValueSize` (partition-source only), and `Replicas`. Per-field
  integration test coverage is required. Fields that exist on the live
  resource but are not represented in the v2 input config (`Description`,
  `MaxBytes`) are **preserved verbatim** from the live `Before` snapshot
  during `update-kv` — Phase 2 never resets a field the operator cannot
  express in YAML.
- Add an `adopt` reconcile policy that stamps the Parti ownership marker
  on resources named by config that exist live but lack the marker, while
  preserving every non-Parti metadata key already on the resource.
- Add a `partictl adopt -f parti-env.yaml [-dry-run]` command that is
  syntactic sugar for `partictl apply -f parti-env.yaml --policy=adopt`.
- Add a `--policy` flag to `partictl apply` (and to `partictl plan` for
  symmetric dry-run behavior). The flag and the YAML `policy:` field
  must agree when both are present (see
  [CLI Additions](#cli-additions)); the CLI does **not** silently
  override a YAML policy. The flag is the canonical way to select a
  policy when YAML omits `policy:`.
- Add `controlPlane.replicas` to `ControlPlaneConfig`. Drift detection
  treats `Replicas` as drift-mutable under `safe-update` (deviating from
  Phase 1, which classified it as drift-immutable conservatively).
- Specify a single **canonical KV equality** function used uniformly by
  drift detection, `update-kv` action suppression, and stale-before
  comparison. The function normalizes NATS server-side defaults
  (`Replicas 0↔1`, `MaxBytes 0↔-1`, `MaxMsgSize 0↔-1`) so a clean
  default install does not flap between desired and live.
- Preserve every Phase 1 invariant (with the single narrowing above).

## Non-Goals (Phase 2)

The following stay out of scope for Phase 2; the target phase or follow-up
is cited so deferred work has a clear home.

- Do not delete or recreate buckets to repair immutable drift. **History**,
  **Storage**, and **bucket rename** remain drift-immutable. Destructive
  repair lands in **Phase 6 (Force + Repair)**.
- Do not add an `--auto-adopt` convenience flag on `apply --policy=safe-update`
  that would stamp the marker and run safe-update in one shot. Phase 2
  enforces the explicit two-step (`adopt` then `safe-update`) for an
  auditable ownership transition. The convenience flag may land in a Phase
  2.x follow-up if real operator pain emerges.
- Do not pre-check NATS cluster peer count to validate that a requested
  `controlPlane.replicas` is feasible before calling `UpdateKeyValue`. The
  server enforces feasibility and Apply fail-fasts on its rejection. A
  cluster-size probe in `ValidateLive` is a Phase 2.x follow-up if needed.
- Do not add `Description` or `MaxBytes` fields to `ControlPlaneConfig`
  or `PartitionSourceConfig` in Phase 2. The v2 YAML schema has no way
  to express either field, and Phase 2 does **not** invent one. The
  `update-kv` Apply path back-fills both fields from the live `Before`
  snapshot, so operator-set values placed via the NATS CLI or another
  tool are preserved across `safe-update`. If real demand emerges,
  adding them is a Phase 2.x scope item.
- Do not solve concurrent-operator coordination beyond best-effort
  stale-before detection (see [Safety Rules](#safety-rules)). NATS
  `UpdateStream` provides no CAS / expected-revision token. Two
  operators running any combination of `safe-update`, `adopt`, or
  `force` against the same bucket are last-writer-wins on every field
  in the written `StreamConfig`; the `update-kv` stale-before check is
  best-effort only and closes the plan→apply window, not the
  re-read→write window. Phase 2 documents this honestly rather than
  introducing distributed locking.
- Do not write partition records. Partition-source bucket creation and
  drift-detection still ships; partition record management remains in
  **Phase 3 (Partition Records)**.
- Do not pre-create dynamic consumers or update existing ones. The Phase 1
  alignment-check surface is unchanged. Phase 2 adds no consumer
  mutation. **Phase 5 (Dynamic Precreate)** owns precreation; **Phase 6**
  owns destructive repair.
- Do not change the JSON envelope `apiVersion`. New `PlannedAction.Kind`
  and `ReconcilePolicy` string values are additive within `parti.io/provision/v1`.
- Do not bump the input config `apiVersion`. `controlPlane.replicas` is
  additive within `parti.io/v1`.

## Reconcile Policy Ladder

Phase 2 introduces two new `ReconcilePolicy` values. The policy is a
**set of allowed reconcile actions per resource**; the actions and drift
shapes are unchanged across the ladder.

| Policy        | Create missing | Stamp markers | Update mutable | Delete-immutable | Phase |
|---------------|----------------|---------------|-----------------|-------------------|-------|
| `warn`        | yes            | no            | no              | no                | 1     |
| `adopt`       | **no**         | **yes**       | no              | no                | **2** |
| `safe-update` | yes            | no            | **yes** (marked only) | no          | **2** |
| `force`       | yes            | no            | yes             | yes               | 6     |

Per-policy plan emission:

- **`warn`** (default): emit `create-kv` for missing buckets; report drift
  on existing buckets; emit no other actions. Unchanged from Phase 1.
- **`adopt`**: emit `stamp-marker` for buckets named by config that exist
  live and are unmarked. Emit no `create-kv` (missing buckets are out of
  scope for adopt; operator must run `warn` or `safe-update` to create).
  Emit no `update-kv` (adoption never touches non-marker fields).
- **`safe-update`**: emit `create-kv` for missing buckets; emit `update-kv`
  for **marked** buckets whose drift-mutable fields differ from desired;
  emit no action for **unmarked** buckets (they remain `adopted` drift —
  operator must run `adopt` first). Emit no `stamp-marker`.

This separation enforces the rule that **safe-update never silently takes
ownership of an unmarked bucket**. The operator's two-step is:

```
$ partictl adopt -f parti-env.yaml
# ... reviews stamp-marker actions ...
$ partictl apply -f parti-env.yaml --policy=safe-update
```

After `adopt`, every previously-unmarked bucket carries the Parti marker;
the safe-update step then sees marked buckets and emits `update-kv` for
any remaining drift-mutable fields.

## Per-Field Mutability Matrix

Phase 2 splits every field on `jetstream.KeyValueConfig` (the union of
fields nats.go v1.50.0 lets a caller write via `UpdateKeyValue` —
`jetstream/kv.go:242-274`) into **three categories**:

1. **Operator-expressible (overwrite-on-update).** YAML can express
   the desired value. `safe-update` reconciles live to desired.
2. **Drift-detection-only (preserved-from-live but emits drift).**
   YAML can express the desired value, but NATS rejects in-place
   updates. Plan emits a `drift-immutable` finding when live and
   desired disagree; `update-kv.After` still inherits the field from
   the live `Before` snapshot so the actual `UpdateKeyValue` call
   does not attempt the rejected mutation. Phase 6 force will own
   the destructive repair.
3. **Preserved-from-live (no drift detection).** YAML has no way to
   express the desired value. Plan never emits drift for this
   field. `update-kv.After` inherits it from the live snapshot.

The **default category** for any `KeyValueConfig` field not enumerated
below is **preserved-from-live (no drift detection)**. This is the
forward-compat rule: when nats.go adds a new field, Phase 2 preserves
it across update-kv and stamp-marker without changes to the plan or
classifier. The implementation enforces this by **copying the entire
re-read live config into `After`** before overwriting the
operator-expressible fields (see [Apply Semantics for New Actions](#apply-semantics-for-new-actions)).

| Field            | Stream-config location       | Phase 2 category               | Notes |
|------------------|------------------------------|--------------------------------|-------|
| `Metadata`       | `Config.Metadata`            | operator-expressible           | Merged map (Parti marker keys + live non-Parti keys). Also reachable via `stamp-marker`. |
| `TTL`            | `Config.MaxAge`              | operator-expressible           | Both resource kinds. |
| `MaxValueSize`   | `Config.MaxMsgSize`          | operator-expressible (PS only) | Partition-source only. Control-plane has no `MaxValueSize` field on `ControlPlaneConfig`; treated as preserved-from-live there. |
| `Replicas`       | `Config.Replicas`            | operator-expressible           | Both resource kinds. Conditional on cluster peer count; NATS enforces. |
| `History`        | `Config.MaxMsgsPerSubject`   | drift-detection-only           | Emits `drift-immutable`; Phase 6 owns repair. |
| `Storage`        | `Config.Storage`             | drift-detection-only           | Emits `drift-immutable`; Phase 6 owns repair. |
| `Bucket`         | resource identity            | identity-only (not drift)      | Bucket name is the resource lookup key. A YAML rename produces a missing target (handled by `create-kv`) and leaves the old bucket outside this config's scope; there is no in-place bucket-field drift path. Excluded from `kvConfigsEqual`. |
| `Description`    | `Config.Description`         | preserved-from-live            | YAML cannot express; never drifted. |
| `MaxBytes`       | `Config.MaxBytes`            | preserved-from-live            | YAML cannot express; never drifted. |
| `Placement`      | `Config.Placement`           | preserved-from-live            | YAML cannot express; preserved verbatim. |
| `RePublish`      | `Config.RePublish`           | preserved-from-live            | YAML cannot express; preserved verbatim. |
| `Mirror`         | `Config.Mirror`              | preserved-from-live            | YAML cannot express; preserved verbatim. |
| `Sources`        | `Config.Sources`             | preserved-from-live            | YAML cannot express; preserved verbatim. |
| `Compression`    | `Config.Compression`         | preserved-from-live            | YAML cannot express; preserved verbatim. |
| `LimitMarkerTTL` | `Config.LimitMarkerTTL`      | preserved-from-live            | YAML cannot express; preserved verbatim. |
| _any future field_ | _whichever location_       | preserved-from-live (default)  | nats.go upgrades that add a `KeyValueConfig` field are automatically preserved without plan changes. |

Notes:

- The **preserved-from-live** category is the key safety property
  Phase 2 adds. The `update-kv` and `stamp-marker` Apply paths
  construct `After` by **copying the entire re-read live config**
  and then overwriting **only** the operator-expressible fields. An
  operator setting `Description: "owned by team X"` via the NATS CLI,
  or a cluster admin setting `Placement` tags or `Compression`,
  survives a Phase 2 `safe-update` Apply that changes only TTL.
- The drift severity vocabulary is unchanged: `informational`,
  `drift-mutable`, `drift-immutable`, `adopted`. **Preserved-from-live**
  fields never produce drift findings. **Drift-detection-only** fields
  produce `drift-immutable` findings when desired and live disagree;
  Phase 6 force will resolve them by delete/recreate.
- The current `classifyPartitionSourceDrift` code that emits
  `drift-mutable` findings for `Description` and `MaxBytes`
  (`provision/partition_source.go:149-170`) is a Phase 1 inheritance
  that **W0 removes** (those fields are now preserved-from-live and
  not classified at all).
- The same Metadata field is reachable through two different actions
  depending on policy: under `adopt`, the action is `stamp-marker`
  and *only* the marker keys are merged into the re-read live
  Metadata. Under `safe-update`, the action is `update-kv` and the
  full target config (including merged Metadata) is written.
- W0 changes the Replicas severity classification (drift-mutable
  instead of drift-immutable) for both resource kinds. The current
  `classifyPartitionSourceDrift` Replicas check
  (`provision/partition_source.go:138-147`) moves from the
  `immutable` branch to the `mutable` branch in W0.
- `Replicas` conditional-mutability is enforced by the NATS server,
  not by `provision`. A `safe-update` Apply that requests
  `Replicas=3` against a single-node NATS cluster will see
  `js.UpdateKeyValue` return an error; Apply records it as a
  fail-fast `ResourceError` per the existing contract.

### Canonical KV equality

Drift detection, `update-kv` action suppression (the "no-op when
desired == live" rule), and the stale-before check at Apply time all
use a single canonical equality function. The function compares **only
the operator-expressible and drift-detection-only fields** from the
[Per-Field Mutability Matrix](#per-field-mutability-matrix);
preserved-from-live fields are intentionally excluded from comparison
because the operator cannot express them and they are always inherited
from live state.

Fields compared by `kvConfigsEqual`:

- `Metadata`: nil-equal-to-empty-map; key-by-key string compare.
- `TTL` (stream `MaxAge`): nanosecond-equal; no normalization.
- `MaxValueSize` (stream `MaxMsgSize`): treat `0` and `-1` as equal
  (server stores "no limit" as `-1`, `jetstream/kv.go:639-642`).
- `Replicas`: treat `0` and `1` as equal (nats.go normalizes
  config `Replicas == 0` to server `Replicas = 1`,
  `jetstream/kv.go:628-631`).
- `History` (stream `MaxMsgsPerSubject`): integer-equal.
- `Storage`: byte-equal.

`Bucket` is **not** compared by `kvConfigsEqual`. The bucket name is
the resource identity used by exact-name lookup
(`provision/plan.go:107-117`, `provision/partition_source.go:53-65`);
a YAML rename produces a `create-kv` for the new name and leaves the
old name outside this config's scope. There is no in-resource
bucket-field drift to compare.

Fields **not** compared by `kvConfigsEqual` (preserved-from-live;
operator cannot express them in YAML, so a difference cannot be
intentional drift):

- `Description`, `MaxBytes`, `Placement`, `RePublish`, `Mirror`,
  `Sources`, `Compression`, `LimitMarkerTTL`, and any future
  `KeyValueConfig` field added by nats.go.

The canonical equality function lives in a single helper
(`provision/kvequal.go::kvConfigsEqual`) and is the **only** equality
check the safe-update path uses. Existing Phase 1 drift classifiers
that perform their own ad-hoc normalization
(`provision/partition_source.go:135-147,160-185`) are refactored in
**W0** to call the helper instead, so the normalization rules live
in one place.

### Snapshot construction (Before / After / target)

`KeyValueConfig` carries pointer and slice fields — `Placement
*Placement`, `RePublish *RePublish`, `Mirror *StreamSource`,
`Sources []*StreamSource` (nats.go v1.50.0
`jetstream/kv.go:242-260`). A plain Go struct assignment is a
**shallow copy**: the pointer values are duplicated but the
pointees are shared with the source. This matters because nats.go's
`prepareKeyValueConfig` may mutate entries in `cfg.Sources` while
building the stream config (`jetstream/kv.go:700-720`), and because
the plan-time `Resource.Before` value must remain immutable for the
audit / JSON output regardless of later Apply or nats.go behavior.

Phase 2 specifies **deep clone** for every snapshot construction:

- Building Plan-time `Resource.Before` (from `StreamInfo.Config`):
  shallow-copy the struct, then deep-clone `Placement`, `RePublish`,
  `Mirror`, and every element of `Sources`. Same rule for the
  `Metadata` map (always cloned to a fresh `map[string]string`).
- Building Plan-time `Resource.After`: deep-clone of Before, then
  overwrite operator-expressible fields.
- Building Apply-time rebuilt target: deep-clone of the re-read
  live config, then overwrite operator-expressible fields. The
  rebuilt target is passed to `js.UpdateKeyValue`; deep cloning
  insulates `Resource.Before` (still in the Plan output) from any
  mutation nats.go performs on the target.

Implementation detail: a single helper
(`provision/clone_kvconfig.go::cloneKVConfig(jetstream.KeyValueConfig) jetstream.KeyValueConfig`)
covers all three call sites. The helper is responsible for the
pointer/slice cloning rules and the forward-compat preservation
unit test calls it directly; if nats.go adds a new pointer-bearing
field, the test fails until `cloneKVConfig` is updated to deep-clone it.

"Byte copy of Before" elsewhere in this plan is shorthand for
"shallow copy + deep-clone of pointer/slice fields per
`cloneKVConfig`."

The forward-compat preservation unit test asserts equality with
`reflect.DeepEqual`, not pointer-identity, on every
non-operator-expressible field.

### Stale-before check uses operator-expressible subset only

The Apply-time stale-before check (see
[`update-kv` Apply path](#update-kv-apply-path) step 2) calls
`kvConfigsEqual(reread, Resource.Before)`. Because `kvConfigsEqual`
ignores preserved-from-live fields, the check is **not** sensitive to
a concurrent operator changing `Description`, `Placement`, or any
other preserved field between plan and apply — Apply simply inherits
the new value at re-read time. The check fires only when an
operator-expressible field or a drift-detection-only field has
changed, which is the exact race the check is designed to catch.

## Proposed SDK Additions

All additions are in the `provision` package.

### Policy constants

```go
// PolicyAdopt stamps the Parti ownership marker on resources named by
// config that exist live and are unmarked. Adopt creates no missing
// resources and updates no non-marker fields.
//
// Validate rejected PolicyAdopt in v1; Phase 2 accepts it.
const PolicyAdopt ReconcilePolicy = "adopt"

// PolicySafeUpdate performs create-missing PLUS in-place UpdateKeyValue
// for drift-mutable fields on Parti-marked resources. Unmarked resources
// continue to surface as "adopted" drift and are not mutated under
// safe-update. Operators run `partictl adopt` first to transition them.
//
// Validate rejected PolicySafeUpdate in v1; Phase 2 accepts it.
const PolicySafeUpdate ReconcilePolicy = "safe-update"
```

The Phase 1 `reservedPolicySafeUpdate` and `reservedPolicyForce` string
constants in `provision/policy.go` change behavior:

- `safe-update` is now accepted by `Validate`; the reserved string is
  removed.
- `force` remains reserved (Phase 6).

### Action kind constants

```go
const (
    // ActionUpdateKV is emitted by Plan under PolicySafeUpdate when a
    // Parti-marked resource has drift-mutable field differences. Apply
    // calls js.UpdateKeyValue(ctx, after) after verifying the live state
    // still matches before. Resource is *UpdateKVResource.
    ActionUpdateKV = "update-kv"

    // ActionStampMarker is emitted by Plan under PolicyAdopt when a
    // resource named by config exists live and is unmarked. Apply reads
    // live state, merges the Parti marker keys with any existing non-Parti
    // metadata keys, and calls js.UpdateKeyValue to write the merged
    // result. Resource is *StampMarkerResource.
    ActionStampMarker = "stamp-marker"
)
```

### Action resource shapes

Both new actions carry richer resource payloads than Phase 1's
`create-kv`, so operator JSON tooling and audit consumers see what is
changing. **The shapes below ship as exported Go types in `provision`.**

```go
// UpdateKVResource is the Resource carried by an ActionUpdateKV action.
// Before is the live KeyValueConfig as observed at plan time; After is
// the target. Apply re-reads live state before mutating and fails fast
// (without mutation) if the re-read does not match Before; this is a
// best-effort guard against last-writer-wins races on UpdateStream
// (which carries no expected-revision token in nats.go v1.50.0).
//
// The Before/After pair is also the audit surface: JSON consumers can
// diff Before vs After to render exactly which fields will change.
type UpdateKVResource struct {
    Before jetstream.KeyValueConfig `json:"before"`
    After  jetstream.KeyValueConfig `json:"after"`
}

// StampMarkerResource is the Resource carried by an ActionStampMarker
// action. MergedMetadata is the full Metadata map that will be written:
// it is the union of (a) any non-Parti keys present on the live
// resource at plan time and (b) the Parti marker keys for the target
// component and instance.
//
// PartiKeys lists exactly the keys the action will add (or change), so
// operator review can verify no non-Parti key is being silently
// modified.
type StampMarkerResource struct {
    Bucket          string            `json:"bucket"`
    MergedMetadata  map[string]string `json:"mergedMetadata"`
    PartiKeys       []string          `json:"partiKeys"`
}
```

Both `ActionUpdateKV` and `ActionStampMarker` populate
`PlannedAction.Resource` with a **pointer** to the corresponding struct
(matching Phase 1's pattern of carrying a `jetstream.KeyValueConfig`
value in `Resource any`). Apply asserts the concrete type and fail-fasts
on a mismatch (`provision/apply.go:93-103` shows the existing pattern).

### Config additions

```go
type ControlPlaneConfig struct {
    // ... existing fields unchanged ...

    // Replicas is the desired number of NATS stream replicas for every
    // control-plane KV bucket. 0 (the default) means "leave the
    // KeyValueConfig.Replicas field unset"; nats.go normalizes that to
    // 1 server-side. Non-zero values trigger drift-mutable detection
    // under safe-update; the NATS server enforces feasibility (cluster
    // peer count) at apply time.
    //
    // Replicas applies uniformly to every control-plane bucket. v2
    // does not support per-bucket replica overrides.
    Replicas int `yaml:"replicas,omitempty" json:"replicas,omitempty"`
}
```

`PartitionSourceConfig.Replicas` already exists in Phase 1 (treated as
drift-immutable then). Phase 2 reclassifies it to drift-mutable under
safe-update; the field is **unchanged** structurally — only the
classifier changes (`classifyPartitionSourceDrift` in
`provision/partition_source.go:93-250`).

`Validate` extensions for the new field:

- `controlPlane.replicas < 0` is rejected (`ErrInvalidConfig`).
- Mirroring the existing partition-source validation
  (`provision/validate.go:162-164`), no upper bound is enforced; the
  server rejects infeasible values at apply time.

## Plan-emission details

### `partictl plan` and policy selection

The SDK's `Plan(ctx, js, cfg)` reads `cfg.Policy` and emits actions per
[Reconcile Policy Ladder](#reconcile-policy-ladder). The CLI accepts a
`--policy` flag on `plan`, `apply`, and `adopt`; the flag and the YAML
`policy:` field must **agree** when both are present. The CLI does not
silently override YAML — see [CLI Additions](#cli-additions) for the
conflict-rejection contract. When YAML omits `policy:`, the flag is
the canonical selection mechanism. When both are present and equal,
the flag is redundant but accepted.

### Order of operations: adopted drift suppresses field drift

When a marked bucket exists, the per-resource classifier reports its
drift findings normally. When an **unmarked** bucket named by config
exists, the existing Phase 1 classifier returns early with a single
`adopted` finding (`provision/plan.go:155-166`,
`provision/partition_source.go:96-107`). Phase 2 preserves that
short-circuit:

- Under `safe-update`, an adopted bucket emits **no** `update-kv`
  action and no field-drift findings — only the `adopted` finding.
  The operator must run `adopt` first.
- Under `adopt`, an adopted bucket emits a `stamp-marker` action;
  the field-drift comparison is skipped entirely (it would surface
  on the next `safe-update` run).
- Under `warn` (default), behavior is unchanged.

This is the codex review's "adoption is not approval" rule: the
operator who runs `adopt` is not signing off on the bucket's current
configuration. After adoption, the *next* `safe-update` plan reveals
the field-level drift that was hidden under the adopted finding —
including any drift-immutable findings that route to Phase 6.

### `update-kv` emission

For each Parti-marked bucket where `kvConfigsEqual(desired, live)`
is false (the canonical equality function defined in
[Canonical KV equality](#canonical-kv-equality)):

1. Build the **Before** value: convert the live `StreamInfo.Config`
   into a `KeyValueConfig`-equivalent shape covering **every** field
   the nats.go `KeyValueConfig` knows about, not just the comparison
   subset. This is the entire live snapshot.
2. Build the **After** value: start with a **byte copy of Before**,
   then overwrite only the operator-expressible fields with their
   desired values:
   - `Metadata`: merged (live non-Parti keys + Parti marker keys
     for the resolved component and `cfg.Instance`).
   - `TTL`: `cfg.<resource>.TTL`.
   - `MaxValueSize` (partition-source only): `cfg.PartitionSource.MaxValueSize`.
   - `Replicas`: `cfg.ControlPlane.Replicas` (control-plane) or
     `cfg.PartitionSource.Replicas` (partition-source).
   Every field that is not operator-expressible — including
   drift-detection-only fields (`History`, `Storage`, `Bucket`) and
   every preserved-from-live field (`Description`, `MaxBytes`,
   `Placement`, `RePublish`, `Mirror`, `Sources`, `Compression`,
   `LimitMarkerTTL`, **and any future field**) — is inherited from
   Before verbatim. The "copy Before, then overwrite operator-
   expressible" algorithm enforces the [Per-Field Mutability
   Matrix](#per-field-mutability-matrix) at the code level.
3. If `kvConfigsEqual(Before, After)` is true, emit no action;
   record the bucket as `informational` drift if it was a candidate
   at all. (This branch is rare because step 2 only ran when an
   operator-expressible field differed; but the post-merge equality
   check guards against bugs where the merge produces an equivalent
   value, e.g. metadata maps differing only by ordering of
   non-Parti keys.)
4. Otherwise emit a `PlannedAction` with `Kind: ActionUpdateKV` and
   `Resource: &UpdateKVResource{Before: live, After: target}`.

The After value **never** changes drift-detection-only fields. If
the live bucket has `History=3` and config asks for `History=1`,
`update-kv.After.History` stays at `3`; the `drift-immutable`
finding is emitted separately as in Phase 1, and the operator is
told to route through Phase 6 force when that ships.

The After value **never** resets preserved-from-live fields either.
If the live bucket has `Description="owned by team X"` set out-of-band
or `Placement` tags configured by a cluster admin,
`update-kv.After` carries those values verbatim regardless of any
other change in the same Apply.

### `stamp-marker` emission

For each unmarked bucket named by config that exists live:

1. Read live `Metadata` from `StreamInfo`.
2. Build `MergedMetadata`: start with a copy of every key in live
   metadata, then write the three Parti keys (`parti.io/managed=v1`,
   `parti.io/component=<component>`, `parti.io/instance=<cfg.Instance>`
   only when non-empty).
3. Build `PartiKeys`: the list of keys whose value differs between
   live metadata and `MergedMetadata` — i.e. exactly the keys the
   action will add or change.
4. Emit a `PlannedAction` with `Kind: ActionStampMarker` and
   `Resource: &StampMarkerResource{Bucket, MergedMetadata, PartiKeys}`.

`BuildMarker` (`provision/marker.go:83-93`) returns only Parti keys,
so it is **not** used directly here. Phase 2 adds a thin helper:

```go
// mergeMarker returns the metadata map to write when stamping the
// Parti marker onto an unmanaged bucket. Live non-Parti keys are
// preserved; Parti marker keys are written/overwritten; the parti.io/
// prefix is the only prefix authoritatively owned by Parti, so any
// other parti.io/* key not in the marker set is left in place
// (forward-compat with future Parti marker keys).
func mergeMarker(live map[string]string, component, instance string) (merged map[string]string, partiKeys []string)
```

This helper is reachable from both the `stamp-marker` plan path and
the `update-kv` plan path (which also writes a full Metadata map
including the marker, merged with live non-Parti keys).

## Apply Semantics for New Actions

### `update-kv` Apply path

For each `ActionUpdateKV` in the plan:

1. **Re-read live state.** Call `js.Stream(ctx, kvStreamPrefix+bucket).Info(ctx)`
   to get the current live `KeyValueConfig`-equivalent fields.
   - **`ErrStreamNotFound`** at this step → fail-fast `ResourceError`
     with message `"bucket-missing-before-update: KV_<bucket> no longer exists"`,
     `Aborted=false`, remaining actions in
     `Skipped{Reason: SkipReasonPriorError}` per the Phase 1
     fail-fast contract (`provision/apply.go:120-130`). No
     auto-create attempt — adopt and safe-update do not create
     resources that disappeared mid-Apply.
   - Other lookup errors → classified per Phase 1's
     `classifyLiveError` pattern (`provision/validate_live.go:232-247`).
2. **Stale-before check (canonical).** Use `kvConfigsEqual(reread, Resource.Before)`
   from [Canonical KV equality](#canonical-kv-equality). If unequal,
   fail-fast: `ResourceError` message
   `"stale-before: live state changed since plan; re-run plan"`,
   identifying the bucket. Remaining actions go to
   `Skipped{Reason: SkipReasonPriorError}`. The canonical equality
   means that NATS server normalization (e.g. live `Replicas=1` vs
   plan `Replicas=0`) does **not** trip the check.
3. **Rebuild After from re-read.** Apply the same Plan-side
   algorithm but starting from the **just-re-read** live snapshot:
   byte-copy the re-read config into the target, then overwrite only
   the operator-expressible fields from `cfg`. This guarantees
   every preserved-from-live field (`Description`, `MaxBytes`,
   `Placement`, `RePublish`, `Mirror`, `Sources`, `Compression`,
   `LimitMarkerTTL`, and any future field) carries the *current*
   live value, not the stale plan-time value. The result is the
   actual `KeyValueConfig` that will be written. **The plan-time
   `Resource.After` is used for audit / JSON output only; the value
   actually written is the just-rebuilt target.**
4. **No-op short-circuit.** If `kvConfigsEqual(reread, rebuilt_after)`,
   skip the mutation; record `ExecutedAction{Raced: true}` (a
   concurrent operator already converged the bucket). Continue to
   the next action.
5. **Call `js.UpdateKeyValue(ctx, rebuilt_after)`.** The full target
   is written.
   - **`errors.Is(err, jetstream.ErrBucketNotFound)`** (the bucket
     was deleted in the re-read→write window) → fail-fast
     `ResourceError` reusing the same message class as the
     re-read miss: `"bucket-missing-before-update: KV_<bucket> no
     longer exists"`. This unifies the two missing-bucket exits
     so operator tooling can pattern-match on a single class.
     Remaining actions go to `Skipped{Reason: SkipReasonPriorError}`.
   - NATS server rejects History/Storage mismatches; we never set
     those fields differently from Before, so they pass through
     unchanged.
   - The server may reject Replicas changes when the cluster lacks
     peers; that surfaces as a normal non-cancellation Apply error
     per the existing fail-fast contract.
   - Any other error → standard fail-fast per
     `provision/apply.go:120-130`.
6. **No success-side re-read.** Once `UpdateKeyValue` returns nil,
   record an `ExecutedAction` and continue. The next `partictl plan`
   run is the canonical post-apply verification surface.

Phase 1's Plan→Apply race handling for `create-kv` uses
`ErrBucketExists` / `ErrStreamNameAlreadyInUse` to mark
`Raced=true` (`provision/apply.go:111-116`). The `update-kv` path has
two race exits: (a) stale-before fail-fast when live drifted from
the plan-time Before, and (b) no-op `Raced=true` when re-read shows
the target state already converged. Both are deliberate — operators
see whether their plan was overtaken (case a) or whether their
intent was already realized by someone else (case b).

### `stamp-marker` Apply path

For each `ActionStampMarker` in the plan:

1. **Re-read live state.** Call `Stream(...).Info(ctx)`.
   - **`ErrStreamNotFound`** → fail-fast `ResourceError` with
     `"bucket-missing-before-stamp: KV_<bucket> no longer exists"`,
     same remaining-actions handling as `update-kv` step 1.
2. **Re-merge.** Recompute `MergedMetadata` from the **live**
   metadata at re-read time (not from the plan's snapshot).
   Stamping the Parti marker is idempotent on the Parti keys; if a
   non-Parti key has changed since plan time, the new value flows
   into the merged map (this is the "non-Parti keys preserved"
   safety property at re-read granularity).
3. **No-op short-circuit.** If every key in the recomputed merged
   map already matches live metadata, no mutation is needed;
   record `ExecutedAction{Raced: true}` (the bucket was
   concurrently adopted or never needed stamping at this point)
   and continue.
4. **Build target `KeyValueConfig`.** Byte-copy the re-read live
   `KeyValueConfig` and overwrite **only** `Metadata` with the
   recomputed merged map. **Every** other field — operator-
   expressible (`TTL`, `MaxValueSize`, `Replicas`), drift-
   detection-only (`Storage`, `History`, `Bucket`), and
   preserved-from-live (`Description`, `MaxBytes`, `Placement`,
   `RePublish`, `Mirror`, `Sources`, `Compression`,
   `LimitMarkerTTL`, future fields) — is inherited from the
   re-read snapshot verbatim. `stamp-marker` writes one and only
   one field-class change: Metadata. This minimizes the
   cross-policy race window — see
   [Cross-policy race window](#cross-policy-race-window).
5. **Call `js.UpdateKeyValue(ctx, target)`.**
   - **`errors.Is(err, jetstream.ErrBucketNotFound)`** (deleted in
     the re-read→write window) → fail-fast `ResourceError` reusing
     the re-read miss class: `"bucket-missing-before-stamp:
     KV_<bucket> no longer exists"`. Same remaining-actions
     handling as `update-kv` step 5.
   - Other server-side rejections (extremely unlikely for a
     metadata-only delta) fail-fast per the existing contract.
6. Record `ExecutedAction` on success.

The `update-kv` and `stamp-marker` Apply paths share enough plumbing
that they should live in one helper file
(`provision/apply_update.go`); each kind has its own entry point
called from the kind-switch in `applyPlan`.

### Testability seam

The Apply helper for `update-kv` and `stamp-marker` is built around
two narrow interfaces that act as injection seams so tests can
deterministically interleave the re-read and write steps:

```go
// streamReader is the read-side of the Apply helper. The production
// implementation calls js.Stream(...).Info(ctx).
type streamReader interface {
    StreamInfo(ctx context.Context, bucket string) (*jetstream.StreamInfo, error)
}

// kvUpdater is the write-side. The production implementation calls
// js.UpdateKeyValue(ctx, cfg).
type kvUpdater interface {
    UpdateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) error
}
```

The Apply entry point accepts a single combined interface (production
implementations wrap `jetstream.JetStream`). Tests inject a fake that
blocks between the read and the write so the cross-policy race test
(see [Test Plan](#test-plan)) can deterministically drive the
interleaving and assert the *exact* written target.

### Cross-policy race window

`stamp-marker` re-reads live state immediately before calling
`UpdateKeyValue` (step 1) and writes the full re-read config with
the merged Metadata (step 4). The window between step 1 and step 5
is short but not zero. If another operator successfully writes a
new TTL / Replicas / MaxValueSize value into the same bucket during
that window, `stamp-marker`'s `UpdateKeyValue` will overwrite that
value with what step 1 read (the **stale** value the other operator
just updated past).

Phase 2 **does not close this window** because NATS `UpdateStream`
exposes no expected-revision / CAS primitive
(`/home/arlo/.vfox/sdks/golang/packages/pkg/mod/github.com/nats-io/nats.go@v1.50.0/jetstream/jetstream.go:138-740`).
The honest contract is:

- `stamp-marker` is **best-effort** with respect to concurrent
  `safe-update` or `force` on the same bucket. The mutation
  *intends* to touch only Metadata; in the presence of a concurrent
  field-changing mutation, it *may* overwrite that mutation if it
  loses the race between the re-read (step 1) and the write
  (step 5).
- Operators who care about cross-policy ordering must serialize
  `adopt` and `safe-update` themselves — the documented two-step
  is the contract Phase 2 ships (`adopt` first, then `safe-update`).
- The race-detection test in [Test Plan](#test-plan) demonstrates
  the window and the operator-visible outcome; the plan does not
  promise this race is closed.

This honesty supersedes the earlier "adopt never touches data or
config" wording in the [End-State Vision](#end-state-vision): the
*intent* of adopt is to touch only the marker, and under serial
operation that intent holds; under concurrent operators the actual
guarantee is "intends to touch only the marker, may lose a race
with concurrent updaters."

### Cancellation, fail-fast, and partial reports

Phase 1's cancellation contract
(`provision/apply.go:25-34`, `83-90`, `117-121`) and fail-fast
contract (`provision/apply.go:33-34`, `122-130`) cover the new
action kinds without modification. No new `Skipped.Reason`
constants are introduced — failures surface as `ResourceError`
entries with descriptive messages (`"stale-before: ..."`,
`"bucket-missing-before-update: ..."`,
`"bucket-missing-before-stamp: ..."`); downstream actions use the
existing `SkipReasonPriorError`.

## CLI Additions

### `partictl apply --policy`

Add a `--policy` flag accepting `warn`, `adopt`, `safe-update`,
or `force` (the last rejected at flag-parse time with
"not supported in v1/v2"). The flag is parsed but does **not**
silently override `cfg.Policy`. Resolution rules:

| `--policy` flag | YAML `policy:` | Effective policy | Outcome |
|-----------------|----------------|------------------|---------|
| absent          | absent         | `warn` (default) | proceed |
| absent          | `X`            | `X`              | proceed |
| `X`             | absent         | `X`              | proceed |
| `X`             | `X` (same)     | `X`              | proceed |
| `X`             | `Y` (different)| —                | **exit 3** with `partictl apply: --policy=X conflicts with cfg.policy=Y in <file>. Pick one or remove the cfg.policy field.` |

The CLI never overrides the YAML silently. When the flag and the
YAML disagree, the operator picks which source of truth wins by
editing one of them.

**`policy: ""` is treated as absent.** The conflict check inspects
the **raw unmarshalled** `cfg.Policy` string before
`provision.normalize` defaults the empty string to `PolicyWarn`
(`provision/validate.go:61-64`). An operator who writes
`policy: ""` (or omits the field entirely) sees the same behavior:
no conflict with any `--policy=X`, and the effective policy is `X`
(or `warn` if `--policy` is also absent). This matches operator
intent — writing `policy: ""` typically means "I haven't decided",
not "I specifically chose `warn`."

Symmetric flag-vs-config conflict checks also apply to
`partictl plan --policy=X` and `partictl adopt -f` (which forbids
any non-empty `cfg.policy` other than `adopt`; `cfg.policy: warn`
plus `partictl adopt -f` still exits 3 with the same message
form; `cfg.policy: ""` proceeds because it is absent).

### `partictl plan --policy`

`partictl plan` also accepts `--policy`. Plan output reflects the
chosen policy's action emission rules (no actions for `adopt` /
`safe-update` against missing or unmarked resources when the
policy excludes them, per
[Reconcile Policy Ladder](#reconcile-policy-ladder)). `-fail-on-drift`
behavior is unchanged.

### `partictl adopt -f cfg.yaml [-dry-run]`

A new top-level command. Shorthand for `partictl apply -f cfg.yaml --policy=adopt`,
with one delta: `partictl adopt -dry-run` is symmetric with
`partictl apply -dry-run` and emits the same `PlanResult` JSON the
plan command would emit under `--policy=adopt`.

Per codex review: `--dry-run` on `adopt` is free given the shared
Plan / Apply infrastructure. It is required for parity with the
`apply --dry-run` operator workflow.

`partictl adopt` requires `-f`. There is no standalone "adopt
everything Parti-shaped" mode — adoption is config-scoped, by
exact NATS name, just like every other config-scoped action in
the SDK.

If `cfg.Policy` is `adopt` (operator already wrote it in YAML),
`partictl adopt` runs normally. If `cfg.Policy` is anything else
non-empty, the policy-conflict check above fires.

## Safety Rules

Building on the Phase 1 Safety Rules section
(`docs/plans/provision-sdk-cli/00-implementation-plan.md:848-868`).
The Phase 1 rules continue to apply. Phase 2 adds:

- **Adoption is not approval of the bucket's config.** Running
  `partictl adopt` stamps the Parti marker. It does not assert that
  the bucket's History, Storage, Replicas, TTL, or any other field
  matches the desired config. The next `partictl plan` run after
  adoption reveals every field-level drift that was hidden under
  the `adopted` finding — including any drift-immutable drift that
  routes to Phase 6 force.
- **`safe-update` preserves fields the YAML cannot express.** The
  `update-kv` Apply path constructs `After` by copying the
  just-re-read live snapshot and overwriting only the
  operator-expressible fields. `Description` and `MaxBytes` are
  **never** reset by Phase 2 even when other fields change in the
  same Apply. Operators who set those fields out-of-band keep them.
- **UpdateStream is last-writer-wins; the stale-before check is
  best-effort only.** NATS `$JS.API.STREAM.UPDATE` carries no
  expected-revision token. Two operators running any combination of
  `safe-update`, `adopt`, or `force` against the same bucket can
  race. The `update-kv` stale-before check (Apply step 2) closes
  the plan→apply window, but the re-read→write window (between
  Apply step 1 and step 5) remains open. A concurrent operator's
  mutation that lands in that window will be overwritten if the
  loser of the race finishes second. See
  [Cross-policy race window](#cross-policy-race-window) for the
  precise contract.
- **`adopt` is intent-only on data and config.** The `stamp-marker`
  Apply path *intends* to touch only the Parti marker keys; in the
  presence of concurrent field-changing writers, the unavoidable
  re-read→write window may overwrite a concurrent field update.
  Operators who care about cross-policy ordering serialize their
  own runs.
- **`cfg.Instance` changes are an operator-expressible Metadata
  mutation.** If an operator stamps `instance=A` via `adopt`, then
  edits cfg to `instance=B` and runs `safe-update`, Plan emits
  `update-kv` with a new Metadata map carrying `parti.io/instance=B`
  (preserving live non-Parti keys). This is intentional —
  `cfg.Instance` is operator-controlled and lives in the
  `parti.io/instance` marker key, which is squarely in the
  operator-expressible category. Operators who change instance
  labels know they are renaming an environment's logical identity.
- **Replicas changes are conditional on cluster size.** A
  `safe-update` Apply that bumps `Replicas` against a NATS cluster
  with fewer peers than the requested value will fail-fast. Apply
  records a `ResourceError` with the underlying NATS error;
  `Aborted` stays false; remaining actions go to
  `Skipped{Reason: SkipReasonPriorError}` per the existing
  fail-fast contract.
- **`partictl adopt` never mutates non-Parti metadata keys.** The
  `stamp-marker` Apply path's merged metadata starts from the live
  Metadata map and only writes the Parti marker keys. Any
  non-Parti key with the same name as a marker key (e.g., an
  operator manually setting `parti.io/managed: malformed`) is
  overwritten with the canonical value; everything else is
  preserved verbatim, subject to the race window above.
- **`partictl adopt` is config-scoped.** Adoption requires `-f`.
  There is no "adopt everything you find" mode; adoption walks
  the buckets named by config and stamps only those.
- **Missing-on-reread is a fail-fast.** If a bucket existed at
  plan time but is gone when Apply re-reads it, both `update-kv`
  and `stamp-marker` fail-fast with a clear `ResourceError`. Apply
  never auto-creates a deleted bucket; that would race with the
  delete itself.

The Phase 1 [Output schema versioning](../provision-sdk-cli/00-implementation-plan.md#output-schema-versioning)
rule continues to hold: every JSON-emitting struct (`Snapshot`,
`PlanResult`, `Report`) carries `apiVersion: parti.io/provision/v1`
and a `Kind` field. New `PlannedAction.Kind` values are additive
within the v1 envelope.

## Work Items

Each work item ships as its own PR. The standard per-PR loop matches
[Phase 1's Implementation Workflow](../provision-sdk-cli/00-implementation-plan.md#implementation-workflow):

```
sub-spec (if needed) → impl → /simplify → /codex:review (or /post-impl-review)
  → fix → re-review → squash on merge verdict
```

Phase 1 conventions for sub-specs, `/simplify`, codex effort levels, and
squash-on-merge carry over. Sub-specs live under
`docs/plans/provision-sdk-cli-v2/` named `<NN>-<wname>-spec.md` (e.g.,
`02-w2-update-kv-apply-spec.md`).

### Recommended model + effort per work item

| W  | Title | Sub-spec needed? | Sub-spec planner | Implementer | Codex review effort |
|----|-------|------------------|------------------|-------------|---------------------|
| W0 | **Classifier + config shape + equality / clone helpers only** (no new action kinds). Accept `PolicyAdopt` + `PolicySafeUpdate` in `Validate`; remove the "rejected in v1" string for safe-update (keep force reserved); add `controlPlane.replicas` field. Add the canonical equality helper `kvConfigsEqual` covering the operator-expressible + drift-detection-only subset. Add the deep-clone helper `cloneKVConfig` covering every pointer / slice field (`Placement`, `RePublish`, `Mirror`, `Sources`, `Metadata`) plus the forward-compat preservation unit test that exercises it. Refactor `classifyControlPlaneDrift` and `classifyPartitionSourceDrift` to use `kvConfigsEqual`. Reclassify `Replicas` from drift-immutable to drift-mutable on both resource kinds. Remove `drift-mutable` emission for `Description` and `MaxBytes` from `classifyPartitionSourceDrift` (preserved-from-live). Update only the comment block in `internal/kvbuckets/builder.go` to document the narrowed invariant (function behavior unchanged). | No | — | **Sonnet 4.6** | `xhigh` |
| W1 | **Plan-emission path for `update-kv` only**. New `UpdateKVResource` type. Per-policy emission gating (`update-kv` only under `safe-update`). The Before-build / After-build algorithm: Before = full live `KeyValueConfig`-equivalent extracted from `StreamInfo.Config` (every nats.go field, not just the comparison subset), constructed via the W0 `cloneKVConfig` helper so pointer/slice fields are deep-cloned and Plan output stays immutable across later mutations. After = `cloneKVConfig(Before)` with only operator-expressible fields overwritten. `kvConfigsEqual(Before, After)` drives the no-action short-circuit. W1 does **not** modify classifier code, the canonical equality helper, or the clone helper (those landed in W0); W1 only consumes them. | **Yes** | **Opus 4.7** | **Opus 4.7** | `xhigh` |
| W2 | Apply path for `update-kv`: re-read live state with `ErrStreamNotFound` → fail-fast `bucket-missing-before-update`; stale-before canonical equality check; rebuild-After-from-reread; no-op short-circuit when re-read shows convergence; `js.UpdateKeyValue` call; fail-fast error wrapping. Per-field integration tests for every operator-expressible field (Metadata, TTL, MaxValueSize, Replicas including server-rejection on undersized cluster). Plan→Apply race tests (stale-before path AND no-op convergence path) | **Yes** (shared with W1 or its own — sub-spec author decides) | **Opus 4.7** | **Opus 4.7** | `xhigh` |
| W3 | `stamp-marker` plan + apply for `adopt` policy: `StampMarkerResource` type, `mergeMarker` helper, Apply path re-reads live and writes the full re-read config with merged Metadata (preserves-from-live every non-Metadata field), missing-on-reread fail-fast. Integration tests asserting (a) Parti keys stamped, (b) non-Parti metadata keys preserved verbatim, (c) preserved-from-live fields (Description, MaxBytes, TTL, MaxValueSize, Replicas) unchanged by adopt, (d) idempotent on re-run, (e) **cross-policy race window** demonstrated with explicit timing comment | **Yes** | **Opus 4.7** | **Opus 4.7** | `xhigh` |
| W4 | CLI plumbing: `partictl apply --policy`, `partictl plan --policy`, `partictl adopt -f [-dry-run]`, **flag-vs-config conflict detection** (exit 3 when both supplied and disagree; same-value or single-source proceeds), per-policy golden-test outputs, exit-code coverage | No | — | **Sonnet 4.6** | `high` |
| W5 | Documentation: `docs/PROVISION.md` Phase 2 section updates plus a new operator playbook covering brownfield NATS adoption, the `adopt → safe-update` sequence, the Replicas server-rejection contract, **the cross-policy race window**, **the preserved-from-live contract**, and the last-writer-wins UpdateStream caveat | No | — | **Sonnet 4.6** | `high` (or run `/doc-sync` against the updated SDK first to derive deltas) |

Rationale, in line with Phase 1's W0–W5 rationale section:

- **W0** changes the policy-acceptance surface and adds a Config field.
  The byte-equivalence invariant narrowing is documented in the W0 PR
  via an updated comment block in `internal/kvbuckets/builder.go` and a
  test in `provision/plan_test.go` asserting that `Replicas==0` still
  produces the byte-equivalent `KeyValueConfig` for control-plane
  buckets. **`xhigh`** review (not `medium`) because the policy-string
  acceptance change is load-bearing for every later work item; getting
  the surface wrong here would force every later PR to re-litigate.
- **W1** is the new public-API surface for plan emission. A sub-spec
  is required because the `update-kv` Resource shape (the
  `UpdateKVResource` struct, the stamp-merge helper interaction, the
  "drift-immutable fields stay in After" rule) needs to be pinned
  before code is written. Opus 4.7 + `xhigh` to match the W1
  precedent.
- **W2** is the mutation path. The stale-before contract, the
  re-read semantics, and the per-field test matrix justify a
  sub-spec (it may be co-located with W1's if the author prefers a
  single spec covering both, but the actions are
  separable). `xhigh` because mutation.
- **W3** is the second mutation path. Separate sub-spec because the
  merge semantics (which keys live, which keys overwrite, idempotency
  rules) are a distinct contract. `xhigh` because mutation + this is
  the operator's first taste of "Parti takes ownership of pre-existing
  resources" — surface mistakes here are very visible.
- **W4** is CLI glue. No public-SDK reasoning beyond what W1–W3 already
  established. `high` (not `xhigh`) per Phase 1's W5 precedent.
- **W5** is docs. Phase 1's docs surface already includes a Phase 2
  placeholder; W5 fills it in and adds the operator playbook the
  brownfield workflow requires.

### Final plan review before W0 starts

Per Phase 1 precedent, this plan runs `/plan-review` against
`/post-impl-review`'s sibling — an architectural pass — until the
verdict is CLEAN. Then a single `/final-plan-review` pass before W0
opens; the latter is the precision pass (effort `high`), not
another architectural round.

## Test Plan

Static / unit:

- `Validate` accepts `policy: adopt` and `policy: safe-update`; rejects
  `policy: force` with the "lands in Phase 6" message; rejects unknown
  policy values with the existing message.
- `ControlPlaneConfig.Replicas < 0` is rejected; `0` accepts; positive
  values accept.
- `Plan` with `cfg.Policy = adopt` emits `stamp-marker` for unmarked
  buckets and no other action; emits no action for missing buckets;
  emits no action for already-marked buckets.
- `Plan` with `cfg.Policy = safe-update` emits `create-kv` for
  missing buckets, `update-kv` for marked buckets with drift on at
  least one operator-expressible field, and reports `adopted` drift
  (no action) for unmarked buckets named by config.
- **`Plan` no-op against default install**: a freshly-created
  control-plane bucket via Phase 1's `create-kv` (live `Replicas=1`,
  `MaxBytes=-1`, `MaxMsgSize=-1`, no Description) against a config
  with omitted `replicas` and no overrides produces **no**
  `update-kv` action under `policy=safe-update`. The canonical
  equality function suppresses the action despite the raw
  difference in field representation.
- `Plan` byte-equivalence for control-plane buckets when
  `ControlPlane.Replicas == 0`: every `create-kv` and `update-kv`
  After value strips Metadata and Replicas, then compares
  byte-for-byte against the W0 builder output for the same inputs
  (preserves the narrowed invariant).
- `update-kv.After` preserves drift-immutable fields from `Before`
  when the desired config disagrees: assert no `History` / `Storage`
  divergence between After and Before.
- **`update-kv.After` preserves preserved-from-live fields**: pre-set
  `Description="owned by team X"` and `MaxBytes=1<<30` on a marked
  bucket; build `update-kv` Plan for an unrelated TTL change;
  assert `After.Description == "owned by team X"` and
  `After.MaxBytes == 1<<30` (preserved verbatim from Before).
- `kvConfigsEqual` normalization: explicit table-driven tests for
  every normalization rule (`Replicas 0↔1`, `MaxBytes 0↔-1`,
  `MaxMsgSize 0↔-1`, `Metadata nil↔{}`).
- `mergeMarker` preserves non-Parti keys verbatim; writes the
  three Parti keys when instance is set; writes two Parti keys
  when instance is empty (no `parti.io/instance` key); returns
  `partiKeys` listing only keys that differ between live and
  merged.

Embedded-NATS integration (per-field — these are the user prompt's
"per-field test coverage required"):

- For each **operator-expressible** field (Metadata, TTL,
  MaxValueSize for partition-source, Replicas for both): pre-create
  a Parti-marked bucket with the field differing from cfg →
  `plan --policy=safe-update` reports `drift-mutable` and emits
  `update-kv` → `apply --policy=safe-update` converges the live
  state → re-`plan` reports `informational`.
- `Replicas` field: above matrix plus a sub-test where the
  embedded NATS server is single-node and the target Replicas is
  3 → `apply` fail-fasts with the server error in `Report.Errors`,
  `Aborted=false`, remaining actions in `Skipped{Reason:
  SkipReasonPriorError}`.
- **Preserved-from-live** fields the YAML can never express:
  pre-create a Parti-marked bucket with operator-set
  `Description="owned by team X"`, `MaxBytes=1<<30`, **and**
  every other preserved-from-live field the embedded NATS test
  harness supports (at minimum: `Compression=true`; `Placement`
  with a tag set; `RePublish` with a destination subject;
  `LimitMarkerTTL > 0` if supported) → cfg requests a different
  TTL but does not (and cannot) mention any of the above →
  `apply --policy=safe-update` → live `Description`, `MaxBytes`,
  `Compression`, `Placement`, `RePublish`, `LimitMarkerTTL` are
  **all unchanged**; only TTL converged. Test fails if any
  preserved-from-live field changes value.
- **Forward-compat preservation**: a unit test that builds a
  synthetic `KeyValueConfig`-equivalent with every field nats.go
  v1.50.0 exposes set to a non-zero value, runs the W1
  After-build algorithm against it, and asserts that **every
  field except the operator-expressible ones is byte-equal to
  Before**. If nats.go adds a new field to `KeyValueConfig` and
  the W1 algorithm fails to copy it, this test fails immediately
  (rather than waiting for a brownfield operator to notice).
- For each **drift-immutable** field (History, Storage): pre-create
  a Parti-marked bucket with the field differing from cfg →
  `plan --policy=safe-update` reports `drift-immutable` and emits
  **no** `update-kv` action → `apply` is a no-op for that bucket.
- **No-op against default install**: pre-create a Parti-marked
  bucket via `apply --policy=warn` (live `Replicas=1`,
  `MaxBytes=-1`, `MaxMsgSize=-1`) → call
  `plan --policy=safe-update` with cfg omitting `replicas` →
  expect **no** `update-kv` action (canonical equality suppression).
- `adopt` plan/apply: pre-create an unmarked bucket with name
  matching config → `plan --policy=adopt` emits `stamp-marker` →
  `apply --policy=adopt` writes the merged metadata → re-`plan`
  under `safe-update` reports `informational` (or further drift
  on non-marker fields, which is the explicit "adoption is not
  approval" test).
- `adopt` preserves non-Parti metadata: pre-create an unmarked
  bucket with `{"custom.io/foo": "bar"}` in Metadata → `apply
  --policy=adopt` → live Metadata contains
  `{custom.io/foo: bar, parti.io/managed: v1, parti.io/component:
  <component>}`. Test fails if the non-Parti key is missing or
  altered.
- `adopt` preserves preserved-from-live fields: pre-create an
  unmarked bucket with `Description`, `MaxBytes`, `TTL`,
  `MaxValueSize`, and `Replicas` all explicitly set →
  `apply --policy=adopt` → live values for all five fields are
  unchanged; only Metadata is augmented.
- Plan→Apply stale-before race for `update-kv`: pre-stage a marked
  bucket with drift on TTL → call `provision.Plan` →
  out-of-band update of live TTL to a third value (neither
  Before nor After) → call `provision.Apply` with the original
  plan → expect fail-fast `ResourceError` containing
  `"stale-before"`; remaining actions are
  `Skipped{Reason: SkipReasonPriorError}`.
- Plan→Apply no-op convergence race for `update-kv`: pre-stage a
  marked bucket with drift on TTL → call `provision.Plan` →
  out-of-band update of live TTL to the desired (After) value →
  call `provision.Apply` with the original plan → expect
  `ExecutedAction{Raced: true}` and no mutation attempt.
- Plan→Apply race for `stamp-marker` (re-merge against current
  live): pre-stage an unmarked bucket with a non-Parti key
  `{"custom.io/foo": "bar"}` → call `provision.Plan` → stamp
  an additional non-Parti key out-of-band
  (`{"custom.io/baz": "qux"}`) → call `provision.Apply` →
  `ExecutedAction` records the merged metadata containing BOTH
  non-Parti keys plus the Parti marker.
- **Cross-policy race for `stamp-marker` (deterministic)**: uses
  the [Testability seam](#testability-seam) to inject a fake
  `streamReader` / `kvUpdater`. Setup: an unmarked bucket with
  Metadata `{custom.io/foo: bar}` and TTL=10s. The fake reader
  returns this snapshot on first call. Apply enters
  `stamp-marker`, completes its re-read, then **blocks before
  the write**. The test then mutates the fake "live" state to
  TTL=20s (simulating a concurrent `safe-update` that landed
  between stamp-marker's read and write). The test unblocks
  stamp-marker. Assert:
  1. The `UpdateKeyValue` call target carries the **re-read
     snapshot's** values (TTL=10s, the stale value) plus the
     merged Metadata (Parti keys + `custom.io/foo: bar`).
  2. `ExecutedAction` is recorded with no error.
  3. The test comment block cites this plan section and
     documents that the deterministic outcome demonstrates the
     race window's existence; the production NATS path has the
     same algorithm and the same window.
  This test is **deterministic** (exact assertion on the write
  target) and **encodes the contract from
  [Cross-policy race window](#cross-policy-race-window)**.
  Without this seam, the test would be timing-dependent and
  unreliable; with it, the test passes when the algorithm
  matches the documented behavior and fails when the
  implementation accidentally closes (or widens) the window.
- **Missing-on-reread**: pre-stage an `update-kv` Plan → delete
  the underlying `KV_<bucket>` stream out-of-band → call
  `provision.Apply` → expect fail-fast `ResourceError` with
  `"bucket-missing-before-update"`, `Aborted=false`, remaining
  actions in `Skipped{Reason: SkipReasonPriorError}`. Same test
  for `stamp-marker` → `"bucket-missing-before-stamp"`.
- Order-of-operations: pre-stage an unmarked bucket whose other
  fields ALSO disagree with cfg → `plan --policy=safe-update`
  reports only `adopted` finding (no `update-kv`) → run `adopt`
  flow → re-run `plan --policy=safe-update` and observe `update-kv`
  emission for the previously-hidden field drift. If the hidden
  drift is immutable, observe `drift-immutable` instead.

CLI:

- `partictl adopt -f cfg.yaml -dry-run` emits the same JSON as
  `partictl plan -f cfg.yaml --policy=adopt`.
- `partictl apply --policy=safe-update` and `partictl apply` (no
  flag, default warn) produce different plans against the same
  drifted environment; golden-test the two outputs.
- Policy-vs-config conflict: cfg.policy=warn + `--policy=safe-update`
  exits 3 with a message naming both sources. Symmetric for
  `partictl adopt -f cfg.yaml` when cfg.policy=safe-update.
- Exit-code precedence is unchanged: connection failure during
  `safe-update` apply still maps to exit 4; static validation
  failure of an unknown policy maps to exit 3; runtime failure
  (server-rejected Replicas) maps to exit 1.

## Acceptance Criteria

- An operator with a brownfield NATS environment can run
  `partictl adopt -f parti-env.yaml`, see exactly which buckets
  are about to be stamped (via `-dry-run -json`), then commit the
  stamp. After the run, every config-named bucket carries
  `parti.io/managed=v1` plus the appropriate `parti.io/component`,
  and **every** other live field (Description, MaxBytes, TTL,
  MaxValueSize, Replicas, plus non-Parti metadata keys) is
  unchanged.
- An operator can edit `parti-env.yaml` to change any
  operator-expressible field (TTL, MaxValueSize for
  partition-source, Replicas, or cfg.Instance) on any marked
  bucket, then run `partictl apply -f parti-env.yaml
  --policy=safe-update` and see those fields reconciled in place
  with a single Apply. **Fields the YAML cannot express
  (`Description`, `MaxBytes`) are never modified by this run** —
  operator-set values placed via the NATS CLI or another tool
  survive Phase 2 safe-update.
- Re-running `safe-update` after a successful run is a no-op:
  drift reports `informational` for the affected buckets, no
  `update-kv` action is emitted. This holds even when live values
  carry NATS server-side normalized defaults (`Replicas=1` from
  config `0`, `MaxBytes=-1` from default install).
- An unmarked bucket named by config is NEVER mutated by
  `--policy=safe-update`. Phase 2 surfaces it as `adopted` drift,
  emits no action, and forces the operator into the adopt flow.
- An immutable-drift bucket (History or Storage mismatch) is
  NEVER mutated by `--policy=safe-update`. Phase 2 surfaces it as
  `drift-immutable`, emits no action, and points operators at the
  Phase 6 force path.
- A Replicas bump against an undersized cluster fails fast with the
  underlying NATS error wrapped into a `ResourceError`; the partial
  `Report` carries `Aborted=false` and the surviving plan actions
  in `Skipped{Reason: SkipReasonPriorError}`. CLI exits 1.
- A `safe-update` Apply whose live state drifted from the plan-time
  Before fails fast at the stale-before check with
  `"stale-before"` in the `ResourceError` message; no mutation
  occurs. A `safe-update` Apply whose live state already converged
  to the desired After records `ExecutedAction{Raced: true}` and
  no mutation occurs.
- A bucket that existed at plan time but was deleted before Apply
  reached it produces a fail-fast `ResourceError`
  (`"bucket-missing-before-update"` or
  `"bucket-missing-before-stamp"`); no auto-create.
- An `adopt` Apply that races with a concurrent operator's adopt on
  the same bucket is idempotent at the Metadata level: the live
  Parti marker keys after both runs are the same regardless of
  order. The plan documents (and a test demonstrates) that the
  race window for non-Parti fields under cross-policy concurrent
  writes is **not** closed; that contract is honest in the docs.
- The CLI rejects `--policy=X` plus `cfg.policy=Y` (where `X != Y`)
  with exit 3 and a message naming both sources. The CLI never
  silently overrides YAML.
- Every JSON output payload still carries
  `apiVersion: parti.io/provision/v1` and a `kind` field. New
  `PlannedAction.Kind` and `ReconcilePolicy` strings appear under
  the same envelope.
- Control-plane `KeyValueConfig` byte-equivalence to
  `manager_setup.go:ensureKVBucket` still holds when
  `ControlPlaneConfig.Replicas == 0`. When non-zero, only the
  `Replicas` field deviates; all others remain byte-equivalent.
  A unit test pins this contract.

## Assumptions

- `partictl` callers running Phase 2 are using NATS server 2.6 or
  newer (live Replicas changes supported). Older NATS servers will
  reject Replicas changes at apply time; Phase 2 treats that as a
  normal server-rejection error and does not pre-detect server
  version.
- `nats.go` v1.50.0's `UpdateKeyValue` semantics are stable for
  the duration of Phase 2. The implementation depends on
  `UpdateKeyValue` routing to `UpdateStream`
  (`jetstream/kv.go:572-590` in the cached module); a future
  nats.go version that changes this routing would require
  reassessment.
- Concurrent operators running `safe-update` simultaneously
  against the same Parti environment is a real-world scenario
  for CI-driven deployments; the stale-before check is sufficient
  best-effort protection. Tighter coordination (server-side CAS,
  distributed lock) is out of scope.
- Operators understand that `adopt` and `safe-update` are
  intentionally separate steps. The plan's documentation (W5)
  pins this in the operator playbook; the CLI does not offer a
  `--auto-adopt` shortcut in Phase 2.
- Replicas downscale (e.g., from 3 to 1) is a valid safe-update
  operation and not specially handled. The NATS server may or may
  not enforce a hold-down on quorum loss during downscale; that
  is out of scope for Phase 2 — operators run replica downscales
  during maintenance windows, same as any cluster size change.
- Phase 2 changes the partition-source drift-classification surface
  in two ways relative to Phase 1: (a) `Replicas` moves from
  `drift-immutable` to `drift-mutable`; (b) `Description` and
  `MaxBytes` **stop** producing drift findings entirely (they are
  preserved-from-live). Phase 1 callers that key on those
  severities for those fields must update; the change is
  announced in the W5 docs and the Phase 2 release notes. The
  partition-source `drift-mutable` severity is otherwise reserved
  for the operator-expressible fields enumerated in the
  [Per-Field Mutability Matrix](#per-field-mutability-matrix).
