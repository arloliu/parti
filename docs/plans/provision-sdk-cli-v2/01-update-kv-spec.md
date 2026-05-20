# Sub-spec: `update-kv` plan emission and apply execution

Covers work items **W1** (Plan emission) and **W2** (Apply execution)
from [`00-implementation-plan.md`](00-implementation-plan.md). The two
ship as **one PR**: W1 alone would make `Plan` emit `update-kv` actions
that `applyPlan`'s kind-switch rejects via its `default` case
(`provision/apply.go:132-142`), leaving `provision.Apply` broken for
`safe-update` between merges. The feature is plan-emit plus
apply-execute; splitting it leaves an incoherent intermediate state.

This sub-spec pins the contract details the master plan leaves open. It
does not restate the master plan; read the master plan's "update-kv
emission", "update-kv Apply path", "Canonical KV equality", and
"Snapshot construction" sections first.

## Scope

In scope:

- `UpdateKVResource` exported type.
- `streamConfigToKVConfig` full-projection helper.
- `mergeUpdateKVMetadata` helper (distinct from W0's `mergeMarkerMetadata`).
- `update-kv` emission in `planControlPlane` / `planPartitionSource`,
  gated on `cfg.Policy == PolicySafeUpdate`.
- `update-kv` execution in `applyPlan`: re-read, stale-before check,
  rebuild-after-from-reread, `js.UpdateKeyValue`, fail-fast classes.
- Testability seam (`streamReader` / `kvUpdater`) for deterministic
  apply tests.

Out of scope (later work items):

- `stamp-marker` / `adopt` policy — **W3**.
- `partictl --policy` flag, `partictl adopt` command — **W4**.
- Cross-policy race tests — **W3** owns the seam-based race test;
  this PR only introduces the seam.

## ReconcilePolicy threading

`Plan` and `Apply` already receive `cfg.Policy`. No signature changes.
`planControlPlane` / `planPartitionSource` branch on
`cfg.Policy == PolicySafeUpdate` to decide whether to emit `update-kv`.
`warn` (default) and `adopt` emit no `update-kv`.

## Type: `UpdateKVResource`

```go
// UpdateKVResource is the Resource carried by an ActionUpdateKV
// PlannedAction. Before is the live KeyValueConfig observed at plan
// time; After is the desired target. Both are deep clones (see
// cloneKVConfig) so the Plan output is immutable regardless of later
// Apply or nats.go mutation.
//
// Apply does not write Resource.After verbatim — it re-reads live
// state and rebuilds the target from the re-read snapshot (see Apply
// algorithm). Resource.Before / Resource.After are the audit surface:
// JSON consumers diff them to render exactly which fields change.
type UpdateKVResource struct {
    Before jetstream.KeyValueConfig `json:"before"`
    After  jetstream.KeyValueConfig `json:"after"`
}
```

`PlannedAction.Resource` holds a `*UpdateKVResource` (pointer, matching
the master plan's stated convention). `ActionUpdateKV = "update-kv"` is
added to the action-kind constants in `provision/types.go`.

## Helper: `streamConfigToKVConfig`

W0's `extractLiveKVConfig` (`provision/kvequal.go`) projects a
`StreamConfig` onto only the `kvConfigsEqual` comparison subset. W1
needs the **full** projection so the `update-kv` Before/After preserve
every preserved-from-live field.

W1 adds:

```go
// streamConfigToKVConfig projects a live jetstream.StreamConfig onto a
// jetstream.KeyValueConfig covering every field nats.go's
// KeyValueConfig exposes, so a round-trip through UpdateKeyValue
// preserves preserved-from-live fields.
func streamConfigToKVConfig(sc jetstream.StreamConfig) jetstream.KeyValueConfig
```

Field mapping (nats.go v1.50.0 `jetstream/kv.go:242-274` is the
authority — verify against the cached module):

| KeyValueConfig field | StreamConfig source |
|----------------------|---------------------|
| `Bucket`             | `strings.TrimPrefix(sc.Name, "KV_")` |
| `Description`        | `sc.Description` |
| `MaxValueSize`       | `sc.MaxMsgSize` |
| `History`            | `historyFromStream(sc.MaxMsgsPerSubject)` (W0 clamp helper) |
| `TTL`                | `sc.MaxAge` |
| `MaxBytes`           | `sc.MaxBytes` |
| `Storage`            | `sc.Storage` |
| `Replicas`           | `sc.Replicas` |
| `Placement`          | `sc.Placement` |
| `RePublish`          | `sc.RePublish` |
| `Mirror`             | `sc.Mirror` |
| `Sources`            | `sc.Sources` |
| `Compression`        | `sc.Compression != jetstream.NoCompression` |
| `Metadata`           | `sc.Metadata` |
| `LimitMarkerTTL`     | `sc.SubjectDeleteMarkerTTL` if that is the v1.50.0 backing field — **verify the exact field name in the cached module before wiring; if absent in v1.50.0, omit and note it** |

`extractLiveKVConfig` is redefined to delegate:
`return streamConfigToKVConfig(sc)`. This is a behavior-preserving
change — the classifiers consume `extractLiveKVConfig` only to feed
`kvConfigsEqual` and `wantedControlPlaneKV` / `wantedPartitionSourceKV`,
all of which read only the comparison-subset fields; the additional
fields populated by the full projection are inert for them. The
delegation removes the duplicate projection rather than shipping two.
**This is the only W0-file change W1 makes; call it out explicitly in
the PR description so the reviewer expects it.**

## Helper: `mergeUpdateKVMetadata`

W0's `mergeMarkerMetadata` overlays the **full** Parti marker
(`managed` + `component` + `instance`) so the classifier gate detects
component drift. The `update-kv` After must NOT rewrite
`parti.io/component`: a component-marker mismatch is drift-immutable
(W0-correction commit `a46a5cc`), and silently re-labelling a bucket's
role would mask a misconfiguration.

W1 adds a distinct merge:

```go
// mergeUpdateKVMetadata returns the Metadata map for an update-kv
// target. It clones live, forces parti.io/managed to the current
// schema value, sets or removes parti.io/instance per the desired
// instance, and PRESERVES parti.io/component verbatim from live —
// component is drift-immutable and update-kv never re-labels a
// bucket's role. Non-Parti keys are preserved.
func mergeUpdateKVMetadata(live map[string]string, instance string) map[string]string
```

Algorithm:

1. `merged := maps.Clone(live)`; if nil, `merged = map[string]string{}`.
2. `merged[MarkerManagedKey] = MarkerManagedValue`.
3. If `instance != ""`: `merged[MarkerInstanceKey] = instance`.
   Else: `delete(merged, MarkerInstanceKey)`.
4. `parti.io/component` is left exactly as cloned from live (step 1).
5. Return `merged`.

### The classifier-vs-update-kv asymmetry (do not consolidate)

`mergeMarkerMetadata` (W0, classifier) and `mergeUpdateKVMetadata`
(W1, apply target) are intentionally different:

| | `mergeMarkerMetadata` | `mergeUpdateKVMetadata` |
|---|---|---|
| `parti.io/managed` | set to `v1` | set to `v1` |
| `parti.io/instance` | set / deleted per instance | set / deleted per instance |
| `parti.io/component` | **set to the desired component** | **preserved from live** |
| purpose | make the gate detect component drift so the classifier emits a `drift-immutable` finding | build a target that never re-labels the bucket |

A future reader who "consolidates" these two helpers will reintroduce
the silent component-rewrite bug. Both functions carry a comment
pointing at this table.

## Plan emission algorithm

For each desired bucket in `planControlPlane` / `planPartitionSource`,
after the existing lookup + classify steps:

1. If the stream was not found → existing `create-kv` path, unchanged.
2. If found and `cfg.Policy != PolicySafeUpdate` → existing behavior
   (classify, append drift findings), no `update-kv`.
3. If found and `cfg.Policy == PolicySafeUpdate`:
   a. If the bucket is **unmarked** (`!ParseMarker(info.Config.Metadata).IsManaged()`):
      no `update-kv` (the classifier already emitted the `adopted`
      finding; adoption is W3).
   b. Else build:
      - `before := cloneKVConfig(streamConfigToKVConfig(info.Config))`
      - `after := buildUpdateKVTarget(<spec|ps>, cfg.Instance, before)`
   c. If `kvConfigsEqual(before, after)` → no `update-kv` (no
      operator-expressible field differs; any remaining drift is
      drift-immutable and routes to Phase 6).
   d. Else append a `PlannedAction{Kind: ActionUpdateKV, Name: bucket,
      Resource: &UpdateKVResource{Before: before, After: after}}`.

The classifier still runs in every branch; its drift findings still
populate `PlanResult.Drift`. `update-kv` actions and drift findings
coexist — a bucket with a drift-mutable TTL produces both a
`drift-mutable` finding and an `update-kv` action under `safe-update`.

### `buildUpdateKVTarget`

```go
// buildUpdateKVTarget returns the desired update-kv target: a deep
// clone of before with only the operator-expressible fields
// overwritten. Every other field — drift-detection-only (History,
// Storage), preserved-from-live (Description, MaxBytes, Placement,
// RePublish, Mirror, Sources, Compression, LimitMarkerTTL), and
// Bucket — is inherited from before verbatim.
```

Control-plane operator-expressible overwrites: `TTL = spec.ttl`,
`Replicas = spec.replicas`, `Metadata = mergeUpdateKVMetadata(before.Metadata, instance)`.
Control-plane has no `MaxValueSize` config field — it is
preserved-from-live (left as `before`'s value).

Partition-source operator-expressible overwrites: `TTL = ps.TTL`,
`Replicas = ps.Replicas`, `MaxValueSize = ps.MaxValueSize`,
`Metadata = mergeUpdateKVMetadata(before.Metadata, instance)`.

Because `after` is a clone of `before` with only operator-expressible
fields overwritten, `kvConfigsEqual(before, after) == false` if and
only if at least one operator-expressible field genuinely differs.
That is the emission gate — no need to inspect classifier findings.

`History` / `Storage` are NOT overwritten: if config disagrees with
live on those, `after` keeps `before`'s value, so `UpdateKeyValue`
never attempts the server-rejected mutation. The `drift-immutable`
finding is emitted separately by the classifier.

## Apply execution algorithm

`applyPlan` gains a `case ActionUpdateKV` branch. The `update-kv` and
existing `create-kv` paths share the Phase 1 cancellation
(`provision/apply.go:84-89`) and fail-fast
(`provision/apply.go:122-130`) contracts unchanged.

For each `ActionUpdateKV` action, with `res := action.Resource.(*UpdateKVResource)`
(type-assert; on failure, fail-fast `ResourceError` per the existing
`create-kv` defensive guard at `provision/apply.go:93-103`):

**Step order matters.** The no-op short-circuit MUST precede the
stale-before check. For a genuinely emitted action `Before != After`,
so when a concurrent operator converges the bucket to the desired
state between Plan and Apply, the re-read `live` equals the target but
differs from `res.Before`. If stale-before ran first it would misreport
that convergence as a stale plan; the no-op check must claim it as a
raced success first.

1. **Re-read live state.** `js.Stream(ctx, "KV_"+name).Info(ctx)`.
   - `errors.Is(err, jetstream.ErrStreamNotFound)` → fail-fast
     `ResourceError{Error: "bucket-missing-before-update: KV_<name> no longer exists"}`,
     remaining actions `Skipped{Reason: SkipReasonPriorError}`.
   - context cancellation → existing cancellation contract.
   - other errors → fail-fast wrapped per `provision/apply.go:122-130`.
   - `live := streamConfigToKVConfig(info.Config)`.
2. **Rebuild target from re-read.** `target := buildUpdateKVTarget(<spec|ps>, instance, cloneKVConfig(live))`.
   This re-applies the emission algorithm to the just-re-read snapshot
   so preserved-from-live fields carry the current live value.
3. **No-op short-circuit.** If `kvConfigsEqual(live, target)` →
   record `ExecutedAction{Kind, Name, Raced: true}` (the bucket already
   matches the desired target — this plan or a concurrent one converged
   it); continue. Reuses the existing `ExecutedAction.Raced` field.
4. **Stale-before check.** The bucket is not yet converged. If
   `!kvConfigsEqual(live, res.Before)` → a concurrent writer moved the
   bucket to a third state; fail-fast
   `ResourceError{Error: "stale-before: live state changed since plan; re-run plan"}`,
   remaining actions `Skipped{Reason: SkipReasonPriorError}`.
   Canonical equality means NATS server normalization
   (live `Replicas=1` vs plan `Replicas=0`) does not trip the check.
5. **Write.** `js.UpdateKeyValue(ctx, target)`.
   - `errors.Is(err, jetstream.ErrBucketNotFound)` → fail-fast
     `ResourceError{Error: "bucket-missing-before-update: ..."}` (same
     class as step 1; nats.go maps a mid-write stream disappearance
     to `ErrBucketNotFound`, `jetstream/kv.go:580-583`).
   - context cancellation → existing cancellation contract.
   - other errors (including server Replicas-feasibility rejection) →
     fail-fast wrapped per `provision/apply.go:122-130`.
   - nil → `ExecutedAction{Kind, Name}`.

Apply needs the desired `spec` / `ps` to rebuild the target in step 2.
The cleanest wiring: `applyPlan` already has the `PlanResult`, but the
`PlanResult` does not carry the desired config. Two options — the
implementer picks one and the post-impl review verifies it:

- **Option A**: thread `cfg` into `applyPlan` and re-derive the spec
  for the named bucket. `applyPlan` already runs after `Plan`, which
  has `cfg`; passing `cfg` to `applyPlan` is a one-parameter change.
- **Option B**: have `UpdateKVResource` carry enough to rebuild —
  but `After` already IS the plan-time target, and step 2's whole
  point is to rebuild from a fresh read. Option B cannot rebuild
  preserved-from-live fields from a stale `After`.

**Use Option A.** Step 2 must rebuild from the re-read snapshot; the
desired config (spec/ps) is the only stable input, and `cfg` is the
natural carrier.

## Testability seam

`applyPlan`'s `update-kv` path interacts with NATS through two narrow
interfaces so a fake can deterministically interleave re-read and
write (the master plan's "Testability seam" section):

```go
type streamReader interface {
    StreamInfo(ctx context.Context, bucket string) (*jetstream.StreamInfo, error)
}
type kvUpdater interface {
    UpdateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) error
}
```

Production implementations wrap `jetstream.JetStream`. W1/W2 introduces
the seam and uses it for the stale-before and no-op-convergence unit
tests; W3 reuses it for the cross-policy race test.

## Test plan

Unit (`package provision`, synthetic `StreamInfo`):

- `streamConfigToKVConfig` populates every field; round-trips a config
  with `Placement` / `RePublish` / `Sources` set.
- `extractLiveKVConfig` still returns a value `kvConfigsEqual` reads
  identically after the delegation change (regression guard).
- `mergeUpdateKVMetadata`: forces `managed=v1`; sets instance when
  non-empty; deletes instance key when empty; **preserves a
  mismatched `component` verbatim**; preserves non-Parti keys.
- `buildUpdateKVTarget`: `after` equals `before` on every
  preserved-from-live and drift-detection-only field
  (`reflect.DeepEqual` per field); only operator-expressible fields
  differ.
- Plan emission: `safe-update` against a marked control-plane bucket
  with a TTL diff emits one `update-kv`; with Replicas diff emits one;
  with instance change emits one; with instance removal emits one;
  with a `managed`-version diff emits one.
- Plan emission negative cases: `safe-update` against an unmarked
  bucket emits no `update-kv` (only the `adopted` finding); against a
  bucket whose only drift is `History` / `Storage` / `component`
  emits no `update-kv` (drift-immutable finding only); against an
  in-sync bucket emits no `update-kv`.
- Plan emission under `warn` and `adopt` emits no `update-kv`.
- `update-kv.After` preserves `Description` / `MaxBytes` set on the
  live bucket; preserves a mismatched live `component`.
- Determinism: shuffled inputs produce identical sorted `Actions`
  (`create-kv` sorts before `update-kv`).

Apply, seam-based unit (fake `streamReader` / `kvUpdater`):

- Stale-before: re-read returns a config differing from `Before` on an
  operator-expressible field → fail-fast `ResourceError` containing
  `"stale-before"`; remaining actions `Skipped{prior-error}`.
- No-op convergence: re-read already equals the target → 
  `ExecutedAction{Raced: true}`, no `UpdateKeyValue` call.
- Missing-on-reread: re-read returns `ErrStreamNotFound` → fail-fast
  `"bucket-missing-before-update"`.
- Missing-on-write: `UpdateKeyValue` returns `ErrBucketNotFound` →
  fail-fast `"bucket-missing-before-update"`.

Apply, embedded-NATS integration:

- Per operator-expressible field (control-plane TTL, control-plane
  Replicas, partition-source TTL, partition-source MaxValueSize,
  partition-source Replicas, instance change): pre-create a marked
  bucket with the field drifted → `Apply` with `safe-update` →
  re-`Plan` reports `informational` for that bucket.
- Replicas server-rejection: single-node embedded server, target
  `Replicas=3` → `Apply` fail-fasts with the server error in
  `Report.Errors`, `Aborted=false`.
- Preserved-from-live: pre-create a marked bucket with a non-empty
  `Description` set out-of-band → `Apply` a TTL change with
  `safe-update` → live `Description` unchanged.
- Re-run no-op: a second `Apply` with `safe-update` after convergence
  records `ExecutedAction{Raced: true}` (or emits no `update-kv` at
  plan time) and mutates nothing.

## Acceptance

- `provision.Plan` with `PolicySafeUpdate` emits `update-kv` for marked
  buckets with operator-expressible drift; nothing for unmarked,
  immutable-only, or in-sync buckets.
- `provision.Apply` with `PolicySafeUpdate` reconciles those buckets
  in place, preserves preserved-from-live fields, fail-fasts on
  stale-before / missing-bucket / server rejection.
- `warn` and `adopt` behavior is unchanged.
- `make lint` clean; `make test` green including the new integration
  tests.

## Recommended model + effort

Implementer: **Opus 4.7** (matches the master plan W1/W2 rows — new
public type, mutation path). Post-impl review: **codex `xhigh`**.
