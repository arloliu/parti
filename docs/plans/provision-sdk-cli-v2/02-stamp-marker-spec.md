# Sub-spec: `stamp-marker` plan emission and apply execution (adopt policy)

Covers work item **W3** from [`00-implementation-plan.md`](00-implementation-plan.md):
the `adopt` reconcile policy's `stamp-marker` action — plan emission and
apply execution.

W3 ships as one PR. Unlike W1 (plan emission alone left `Apply` broken),
W3 delivers both halves of `stamp-marker`, so `provision.Apply` with
`PolicyAdopt` is fully functional after this PR. The CLI surface
(`partictl adopt`, `--policy`) is **W4** and is out of scope here.

Read the master plan's "stamp-marker emission", "stamp-marker Apply
path", "Cross-policy race window", and "Reconcile Policy Ladder"
sections first. This sub-spec pins the contract details those leave
open and does not restate them.

## Scope

In scope:

- `StampMarkerResource` exported type; `ActionStampMarker` constant.
- `adopt` policy plan emission: `stamp-marker` for config-named
  buckets that exist live and are unmarked; suppression of `create-kv`
  and `update-kv` under `adopt`.
- `stamp-marker` execution in `applyPlan`: re-read, recompute merged
  metadata, no-op short-circuit, write, missing-on-reread/write
  fail-fast.
- Reuse of the W0 `mergeMarkerMetadata` helper and the W1
  `streamReader` / `kvUpdater` seam.
- The deterministic cross-policy race test.

Out of scope:

- `partictl adopt` command and `partictl --policy` flag — **W4**.
- Any change to `update-kv` (W1/W2) or the drift classifiers (W0).
- `force` policy — Phase 6.

## Type: `StampMarkerResource`

```go
// StampMarkerResource is the Resource carried by an ActionStampMarker
// PlannedAction. MergedMetadata is the full Metadata map the action
// will write: the union of the live bucket's existing keys and the
// Parti marker keys (parti.io/managed, parti.io/component, and
// parti.io/instance when the instance is non-empty).
//
// PartiKeys lists exactly the metadata keys the action adds or
// changes relative to the live bucket, so operator review can verify
// that no non-Parti key is being modified.
//
// Apply does not write MergedMetadata verbatim — it re-reads live
// state and recomputes the merge against the re-read metadata (see
// the Apply algorithm). MergedMetadata / PartiKeys are the audit
// surface for `plan` / `apply -dry-run` output.
type StampMarkerResource struct {
    Bucket         string            `json:"bucket"`
    MergedMetadata map[string]string `json:"mergedMetadata"`
    PartiKeys      []string          `json:"partiKeys"`
}
```

`PlannedAction.Resource` holds a `*StampMarkerResource` (pointer,
matching the `create-kv` value-type and `update-kv` pointer-type
precedent — pointer because the struct carries a map and a slice).
`ActionStampMarker = "stamp-marker"` is added to the action-kind
constants in `provision/types.go`.

## Metadata merge: reuse `mergeMarkerMetadata`

`stamp-marker` operates only on **unmarked** buckets. An unmarked
bucket has no `parti.io/managed` key (`MarkerInfo.IsManaged()` is
false), so there is no live Parti `component` value to preserve — the
action establishes the full marker from the config-derived component.

This is exactly what the W0 `mergeMarkerMetadata(live, component, instance)`
helper does: clone live metadata, overlay `parti.io/managed=v1`,
`parti.io/component=<component>`, and `parti.io/instance=<instance>`
(deleting the instance key when instance is empty), preserving every
non-Parti key. **W3 reuses `mergeMarkerMetadata`; it adds no new merge
helper.**

This is *not* the same as `update-kv`'s `mergeUpdateKVMetadata`, which
preserves the live component because `update-kv` operates on
already-marked buckets where a component mismatch is drift-immutable.
The distinction holds: `stamp-marker` adopts unmarked buckets and
writes the component; `update-kv` reconciles marked buckets and never
touches the component. The two helpers' existing
"do not consolidate" comments already document the asymmetry.

`PartiKeys` is computed as the keys whose value differs between the
live metadata and the merged result, in **either direction** —
additions, overwrites, **and removals**. The removal case is real: an
unmarked bucket carrying a stray `parti.io/instance=old` adopted under
an empty `cfg.Instance` has that key deleted by `mergeMarkerMetadata`,
and that deletion is a change the operator should see in the audit
output. For a cleanly unmarked bucket `PartiKeys` is
`[parti.io/managed, parti.io/component]` plus `parti.io/instance` when
the instance is set. Computed with a small local helper, not a new
exported function. The helper compares the two maps key-by-key:
a key present in one and absent in the other, or present in both with
differing values, is included.

## Plan emission

The `adopt` policy's per-bucket emission rules, applied in
`planControlPlane` and `planPartitionSource` after the existing
lookup + classify steps:

1. **Stream not found** — under `adopt`, emit **no action** but **do
   emit an `informational` drift finding**. Adopt does not create
   buckets (Reconcile Policy Ladder: adopt's "create missing" is
   *no*), so the action list stays empty for this bucket. But the
   bucket must not vanish silently from the plan: an operator running
   `adopt` against a config naming five buckets, one of them missing,
   would otherwise see four `stamp-marker` actions and conclude the
   environment is fully adopted. Emit:

   ```
   DriftFinding{
     Severity: SeverityInformational,
     Kind:     <KindControlPlaneKV | KindPartitionSource>,
     Name:     bucket,
     Detail:   {"reason": "bucket missing; adopt does not create — run apply with warn or safe-update"},
   }
   ```

   The severity vocabulary is unchanged (`informational` already
   exists). `-fail-on-drift` treats `informational` as non-drift, so
   exit codes are unaffected. (`warn` and `safe-update` keep their
   existing `create-kv` emission for a missing bucket; only `adopt`
   substitutes this finding.)
2. **Stream found and marked** (`ParseMarker(...).IsManaged()`) —
   emit no `stamp-marker` (already adopted). The classifier still
   runs and its drift findings still populate `PlanResult.Drift`; an
   operator who runs `adopt` against an already-marked but drifted
   bucket sees the drift finding and is directed (W5 docs) to
   `safe-update`. This is the master plan's "adoption is not
   approval" rule.
3. **Stream found and unmarked** — emit a `stamp-marker` action.
   Build it from the live snapshot:
   - `live := streamConfigToKVConfig(info.Config)` (the W1 full
     projection).
   - `merged := mergeMarkerMetadata(live.Metadata, <component>, cfg.Instance)`.
   - `partiKeys := keysAddedOrChanged(live.Metadata, merged)`.
   - emit `PlannedAction{Kind: ActionStampMarker, Name: bucket,
     Resource: &StampMarkerResource{Bucket: bucket,
     MergedMetadata: merged, PartiKeys: partiKeys}}`.

The `<component>` is the config-derived component for that bucket:
the control-plane component for each control-plane spec
(`spec.component`), `ComponentPartitionSource` for the
partition-source bucket.

Under `adopt`, `update-kv` is never emitted (that is `safe-update`
only). Under `warn` and `safe-update`, `stamp-marker` is never
emitted. The three policies' action sets stay disjoint per the
Reconcile Policy Ladder.

Determinism: `ActionStampMarker = "stamp-marker"` sorts after both
`create-kv` and `update-kv` alphabetically; `sortActions` keeps the
output stable with no new sort code. (No single plan mixes
`stamp-marker` with the other kinds — policies are disjoint — but the
ordering is well-defined regardless.)

## Apply execution

`applyPlan` gains a `case ActionStampMarker` branch. It shares the
Phase 1 cancellation and fail-fast contracts and the W1
`streamReader` / `kvUpdater` seam unchanged.

For each `ActionStampMarker` action, with
`res := action.Resource.(*StampMarkerResource)` (type-assert; on
failure, fail-fast `ResourceError` per the existing defensive guard):

1. **Re-read live state.** `reader.StreamInfo(ctx, action.Name)`.
   - `errors.Is(err, jetstream.ErrStreamNotFound)` → fail-fast
     `ResourceError` `"bucket-missing-before-stamp: KV_<name> no longer exists"`,
     remaining actions `Skipped{Reason: SkipReasonPriorError}`.
   - context cancellation → existing cancellation contract.
   - other errors → fail-fast wrapped.
   - `live := streamConfigToKVConfig(info.Config)`.
2. **Recompute the merge against the re-read metadata.**
   `merged := mergeMarkerMetadata(live.Metadata, <component>, cfg.Instance)`.
   Recomputing (rather than trusting `res.MergedMetadata`) means a
   non-Parti key added between plan and apply flows into the written
   map — `stamp-marker` preserves the *current* non-Parti keys.
3. **No-op short-circuit.** Build the target: `target := cloneKVConfig(live)`
   with `target.Metadata = merged`. If `kvConfigsEqual(live, target)`
   → record `ExecutedAction{Kind, Name, Raced: true}` (the bucket was
   concurrently adopted, or already carries an equivalent marker) and
   continue. No write.
4. **Write.** `updater.UpdateKeyValue(ctx, target)`.
   - `errors.Is(err, jetstream.ErrBucketNotFound)` → fail-fast
     `"bucket-missing-before-stamp: ..."` (same class as step 1).
   - context cancellation → existing cancellation contract.
   - other errors → fail-fast wrapped.
   - nil → `ExecutedAction{Kind, Name}`.

`stamp-marker` has **no stale-before check**. Unlike `update-kv` it
carries no plan-time expectation of the bucket's non-metadata fields:
it re-reads live and writes that snapshot back with the marker
merged. A concurrent change to a non-metadata field between plan and
apply is simply picked up by the re-read. The one race it cannot
close is a concurrent change landing between *its own* re-read
(step 1) and write (step 4) — see below.

The `component` needed in step 2 is re-derived the same way the
`update-kv` apply path re-derives its spec: via the `cfg` threaded
into `applyPlan` (W1's "Option A") plus the `cpSpecs` map. The
partition-source component is the constant `ComponentPartitionSource`.
If `action.Name` matches neither a control-plane spec in `cpSpecs`
nor `cfg.PartitionSource.Bucket`, fail-fast with a defensive
`ResourceError` — `"stamp-marker target %q is not described by the
resolved config"` — exactly parallel to `buildUpdateKVTargetForBucket`.
Plan should never emit such an action; the guard catches a wiring bug
rather than a runtime race.

The `update-kv` and `stamp-marker` apply paths live together in
`provision/apply_update.go`; `applyStampMarkerAction` is the new
entry point called from the `applyPlan` kind-switch.

## Cross-policy race window

`stamp-marker` re-reads live state (step 1) and writes the re-read
config plus merged metadata (step 4). The window between the two is
short but not zero. If a concurrent operator runs `safe-update` and
changes a field such as `TTL` on the same bucket inside that window,
`stamp-marker`'s write carries the value it read in step 1 — the
stale value — and reverts the concurrent change.

Phase 2 does not close this window: NATS `UpdateStream` has no
expected-revision / CAS token. The contract is the master plan's
"Cross-policy race window" rule: `stamp-marker` *intends* to touch
only the marker, and under serial operation it does; under concurrent
operators it is best-effort and may lose a race with a field-changing
writer. Operators serialize `adopt` and `safe-update` themselves (the
documented `adopt` then `safe-update` two-step).

This window is genuine and the test plan demonstrates it
deterministically rather than papering over it.

## Test plan

Unit (`package provision`, synthetic `StreamInfo` / seam fakes):

- `StampMarkerResource` shape; `ActionStampMarker` constant.
- Plan emission: `adopt` against an unmarked control-plane bucket
  emits one `stamp-marker`; against an unmarked partition-source
  bucket emits one; against a marked bucket emits none.
- Plan emission: `adopt` against a missing bucket emits no action and
  one `informational` drift finding whose `Detail["reason"]` mentions
  that adopt does not create.
- Plan emission: `adopt` emits no `update-kv`; `warn` and
  `safe-update` emit no `stamp-marker`.
- `StampMarkerResource.MergedMetadata` carries the Parti marker keys
  plus every pre-existing non-Parti key; `PartiKeys` lists exactly
  the added/changed keys (`parti.io/managed`, `parti.io/component`,
  and `parti.io/instance` when instance is set — and only those).
- Plan emission with empty `cfg.Instance`: `MergedMetadata` has no
  `parti.io/instance` key; `PartiKeys` is the two-element set.
- `PartiKeys` removal case: an unmarked bucket carrying a stray
  `parti.io/instance=old` adopted under empty `cfg.Instance` →
  `MergedMetadata` omits `parti.io/instance` and `PartiKeys` includes
  it (a removal counts as a change).
- Apply, seam-based: `applyStampMarkerAction` against an unmarked
  re-read writes a target whose Metadata is the merged map and whose
  every non-metadata field equals the re-read snapshot; against an
  already-marked re-read records `ExecutedAction{Raced: true}` with
  no write; `ErrStreamNotFound` on re-read → fail-fast
  `bucket-missing-before-stamp`; `ErrBucketNotFound` on write →
  same class; wrong `Resource` type → fail-fast; cancelled re-read →
  cancellation propagates.
- **Cross-policy race (deterministic).** A fake `streamReader`
  returns an unmarked bucket with `TTL=10s` and a non-Parti metadata
  key. Run `applyStampMarkerAction`; the fake `kvUpdater` captures
  the write target. Assert the captured target's `TTL` equals the
  re-read `10s` (not any "desired" value — `stamp-marker` has no
  desired non-metadata values) and its `Metadata` is the Parti
  marker plus the non-Parti key. A comment block states: this
  demonstrates the race window — a concurrent `safe-update` changing
  `TTL` between this re-read and write would be reverted, the
  documented best-effort cross-policy contract.

Embedded-NATS integration:

- `adopt` against an unmarked bucket named by config: after `Apply`,
  the live bucket carries `parti.io/managed=v1`, the correct
  `parti.io/component`, and `parti.io/instance` (when configured).
- Non-Parti metadata preserved: pre-create an unmarked bucket with
  `{"custom.io/team": "x"}` in Metadata → `Apply` under `adopt` →
  live Metadata contains both `custom.io/team=x` and the Parti keys.
- Preserved-from-live: pre-create an unmarked bucket with
  `Description`, `MaxBytes`, `TTL`, `MaxValueSize`, and an explicit
  `Replicas` set → `Apply` under `adopt` → all five live values are
  unchanged; only Metadata gains the Parti keys.
- Idempotent: a second `Apply` under `adopt` after the bucket is
  adopted emits no `stamp-marker` (plan-time) / records a raced
  no-op, and mutates nothing.
- Adopt does not create: `adopt` against a config naming a missing
  bucket produces no action, creates nothing, and surfaces one
  `informational` drift finding for that bucket.
- Adopt then safe-update: after `adopt` marks a bucket, a subsequent
  `Plan` under `safe-update` classifies it normally (informational
  when in sync, drift findings when not) — proving adoption made the
  bucket visible to the safe-update path.

## Acceptance

- `provision.Plan` with `PolicyAdopt` emits `stamp-marker` for every
  config-named bucket that exists live and is unmarked, and nothing
  for marked or missing buckets.
- `provision.Apply` with `PolicyAdopt` stamps the Parti marker on
  those buckets, preserves every non-Parti metadata key and every
  preserved-from-live field, is idempotent, and fails fast on a
  missing bucket.
- `warn` and `safe-update` behavior is unchanged.
- `make lint` clean; `make test` green including the new integration
  tests.

## Recommended model + effort

Implementer: **Opus 4.7** (new public type, mutation path). Post-impl
review: **codex `xhigh`**.
