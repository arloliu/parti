# Provision SDK / partictl — Phase 3: Partition Records

This plan is **Phase 3** of the
[Phased Roadmap](../provision-sdk-cli/00-implementation-plan.md#phased-roadmap)
in the master plan. Phases 1 and 2 built the `provision/` SDK and
`cmd/partictl` CLI that provision the NATS JetStream KV **buckets** Parti
depends on. Phase 3 manages the **contents** of the partition-source bucket
key — the partition records that workers consume.

## What Ships After Phase 3

- Operators can declare the desired partition set inline in `parti-env.yaml`
  under `partitionSource.partitions:` and run
  `partictl partitions plan -f parti-env.yaml` to see a record-level diff
  (added / removed / weight-changed) between the declared set and the live
  KV key contents.
- Operators can run `partictl partitions apply -f parti-env.yaml` to write
  the declared partition set into the partition-source key. Apply is a
  single compare-and-swap (CAS) write of the whole record array; it adds new
  records and reconciles changed weights freely, and removes records only
  when `--prune` is passed.
- The SDK exposes `provision.PlanPartitions` and `provision.ApplyPartitions`
  so the same diff/apply path is callable as a library, not just the CLI.

Subsequent phases keep their own surface: application streams (Phase 4),
dynamic consumer precreation (Phase 5), destructive repair (Phase 6),
Kubernetes controller (Phase 7).

## Background: The Partition-Source Data Model

Ground truth from the current codebase (verified at the cited lines):

- The partition-source bucket is a JetStream KV bucket. It holds **one key**
  (operator-configured, e.g. `partitions/v1`). The value at that key is the
  entire partition table.
- The value is a JSON array of `types.Partition`
  (`types/partition.go:16-26`), gzip-compressed:

  ```go
  type Partition struct {
      Keys   []string `json:"keys"`   // >=1 non-empty key; identifies the partition
      Weight int64    `json:"weight"` // relative processing cost; 0/negative => strategy default
  }
  ```

- Wire format: `json.Marshal` of `[]types.Partition`, then gzip. The read
  path auto-detects gzip via magic bytes `0x1f 0x8b` at offsets 0-1 and
  falls back to plain JSON otherwise. Encode: `encodePartitions`
  (`source/nats_kv.go:1074-1090`). Decode: `decodePartitions`
  (`source/nats_kv.go:993-1041`). **Both are currently unexported.**
- A partition's identity is its `CanonicalID()` — a collision-safe
  length-prefixed encoding of `Keys` (`types/partition.go:82-114`). The read
  path rejects a value containing two records with the same `CanonicalID`
  as corruption (`source/nats_kv.go:1024-1040`).
- Per-record validity (`types.Partition.Validate()`,
  `types/partition.go:34-52`): `Keys` non-empty; no key empty; no key
  contains `.` or whitespace.
- The runtime reads the key through `source.NatsKV` (a watcher plus a
  periodic reconcile loop). The runtime also *can* write it via
  `NatsKV.Update` / `Modify` / `AddPartitions` / `RemovePartitions`, all
  CAS-protected (`source/nats_kv.go:441-631`). Today the partition table is
  otherwise hand-published; there is no operator tooling for it.
- `provision.PartitionSourceConfig` (`provision/config.go:69-82`) describes
  the **bucket** only (`Bucket`, `Key`, `Storage`, `History`, `Replicas`,
  `MaxValueSize`, `TTL`). It does not reference the key contents.

## Invariants Inherited from Phases 1-2

Every invariant from the
[Phase 1 list](../provision-sdk-cli/00-implementation-plan.md#invariants-inherited-by-every-phase)
continues to hold. The load-bearing ones for Phase 3, plus **one new
invariant**:

- Ownership marker shape (`parti.io/managed`, `parti.io/component`,
  `parti.io/instance`) stays in bucket `Metadata` and remains
  informational. Partition **records** carry no per-record ownership
  marker; the bucket marker governs. Phase 3 adds no record-level marker.
- JSON envelope schema stays at `apiVersion: parti.io/provision/v1`.
  Phase 3 adds one new `PlannedAction.Kind` value (`write-partitions`) and
  one new `DriftFinding.Kind` value (`partition-records`); both additive.
  Tooling that filters by existing kinds keeps working.
- Input config `apiVersion: parti.io/v1` accepts additive fields. Phase 3
  adds `partitionSource.partitions`; YAML omitting it loads with no
  behavior change.
- CLI exit codes and their precedence (`cmd/partictl/exitcodes.go:14-25`)
  are stable: `0` ok, `1` runtime, `2` drift (with `-fail-on-drift`), `3`
  validation, `4` NATS. No new codes.
- `Plan` action and drift ordering remains deterministic. Within the
  `write-partitions` action, the `added` / `removed` / `changed` lists are
  each sorted by `CanonicalID`.
- **New invariant — partition wire-format byte-equivalence.** The bytes
  `provision` writes to the partition-source key must be decodable by the
  runtime `source.NatsKV` read path, and vice versa. The shared codec
  (W0) is the contract; both `source` and `provision` call it. This is the
  Phase 3 analogue of the Phase 1 `KeyValueConfig` byte-equivalence
  invariant.

## Non-Goals (Phase 3)

- Do not manage the partition-source **bucket** config here. Bucket
  creation, drift detection, adopt, and safe-update remain the job of
  `partictl plan/apply/adopt` (Phases 1-2). `partictl partitions` operates
  on key **contents** only and assumes the bucket already exists.
- Do not auto-create the partition-source bucket. If the bucket is missing,
  `partictl partitions` fails with a typed error directing the operator to
  run `partictl apply` first (see W3).
- Do not add a per-record ownership marker or per-record metadata beyond
  the existing `types.Partition` fields.
- Do not add `partictl partitions view`. `partitions plan` already renders
  the full live-vs-desired picture; a standalone read-only inventory
  command can land later if demand appears.
- Do not change how the runtime reads or writes partition records. The W0
  extraction is a pure refactor with zero behavior change for `source`.
- Do not introduce record-level CAS per partition. The KV value is a single
  key; apply is one atomic CAS write of the whole array.
- The reconcile policy ladder (`warn` / `adopt` / `safe-update`) does **not**
  govern the `partitions` subcommand: record-level behavior is identical
  under every policy value, and the only record-removal safety control is
  `--prune`. What `partitions` ignores is the policy's *reconcile
  semantics* — not its *validity*. The `policy` value is still subject to
  the inherited static-validation boundary: `PlanPartitions` runs
  `Validate(cfg)` (W2 step 1), and `validateResolved` rejects an
  unsupported or malformed `policy` such as `force`
  (`provision/validate.go:117-127`) with `ErrInvalidConfig`. So a config
  with a bad `policy` fails `partitions plan/apply` at static validation
  (CLI exit `3`), exactly as it fails the bucket commands; a config with a
  *valid* `policy` runs identically regardless of which valid value it is.

## Design

### W0 — Shared partition codec (`internal/partcodec`)

`provision` must produce byte-identical wire output to what `source.NatsKV`
reads. Today the codec lives as unexported `encodePartitions` /
`decodePartitions` in `source/nats_kv.go`. Neither `internal/partutil`
(hash + subject-pattern helpers) nor `internal/ipartition` (consumer config)
is a fit, so the codec moves to a **new package `internal/partcodec`**.

New file `internal/partcodec/partcodec.go`:

```go
package partcodec

// Encode marshals partitions to JSON and gzip-compresses the result.
// It is a pure function: no validation, no dedup. Callers validate first.
func Encode(partitions []types.Partition) ([]byte, error)

// Decode reverses Encode. It auto-detects gzip (magic bytes 0x1f 0x8b at
// offset 0-1) and falls back to plain JSON, unmarshals to []types.Partition,
// validates each record (types.Partition.Validate), and rejects a payload
// containing two records with the same CanonicalID as corruption.
func Decode(data []byte) ([]types.Partition, error)
```

- `Encode` is the verbatim body of `encodePartitions`
  (`source/nats_kv.go:1074-1090`).
- `Decode` is the verbatim body of `decodePartitions`
  (`source/nats_kv.go:993-1041`), including the per-record validate and the
  duplicate-`CanonicalID` rejection.
- `source/nats_kv.go` is edited so `encodePartitions` /
  `decodePartitions` either become thin wrappers calling `partcodec` or are
  deleted and call sites updated. `validateAndDedupe`
  (`source/nats_kv.go:1051-1071`) **stays in `source`** — it is a
  write-path concern, separate from the codec.
- `internal/` packages are importable within module
  `github.com/arloliu/parti/v2`; `provision` already imports
  `internal/kvbuckets`, so importing `internal/partcodec` is consistent.

**Byte-equivalence proof obligation.** "No behavior change" is not
established by inspection — the ground truth here is a gzip byte stream, and
a `gzip.NewWriter` flag default or a `json.Marshal` field-ordering change
would silently break the runtime watcher's magic-byte detection. W0 must
ship a **golden-bytes fixture test**: a fixed `[]types.Partition` input, its
expected encoded bytes captured as a fixture, asserted stable; and a
round-trip test (`Decode(Encode(x)) == x`) plus a cross-check that the
fixture decodes through the live `source` read path. The gzip magic-byte
detection contract is at `source/nats_kv.go:998-1000` (detection) and
`source/nats_kv.go:1001-1017` (gzip-reader fallback / plain-JSON path); the
codec must keep accepting **both** a gzip payload and a plain-JSON payload.
A decode test must cover each of those two input shapes so the dual-format
contract cannot regress.

### W1 — Declared partition set in config

Add one field to `PartitionSourceConfig` (`provision/config.go:69-82`):

```go
// Partitions is the desired partition table for the partitionSource key.
// It is consumed only by PlanPartitions / ApplyPartitions (the
// `partictl partitions` subcommand); the bucket-provisioning commands
// (plan/apply/adopt) ignore it. Omitted/empty => the partitions
// subcommand reports "no partitions declared" rather than treating it as
// an instruction to empty the table.
Partitions []types.Partition `yaml:"partitions,omitempty" json:"partitions,omitempty"`
```

- The records reuse `types.Partition` directly rather than a
  provision-local struct: single source of truth, no conversion layer.
  **Stability commitment:** this couples the `parti-env.yaml` input schema
  to `types.Partition`; any field later added to `types.Partition` becomes
  part of the `parti.io/v1` input schema. `types.Partition` has only `json`
  tags; `gopkg.in/yaml.v3` lowercases field names by default, so `keys:` /
  `weight:` parse correctly with no `yaml` tag needed.
- **File layout — inline is canonical.** The partition set is declared
  inline under `partitionSource.partitions:` in the same `parti-env.yaml`
  every other `partictl` command already loads. This keeps the CLI
  single-file and consistent and needs no new file format or `apiVersion`.
  Tradeoff, stated openly: partition tables can be large (the codec
  gzip-compresses precisely because lists approach the ~1 MB KV value
  limit), and infra config and the partition set have different change
  cadences. Mitigation: the W5 docs describe a split-file pattern (YAML
  anchors / a templating include) for operators with large tables. A
  first-class separate-file format is deferred unless demand appears — see
  [Open Design Decisions](#open-design-decisions).
- **Validation placement.** Per-record validity and
  duplicate-`CanonicalID` rejection for `Partitions` is enforced inside
  `PlanPartitions` / `ApplyPartitions`, **not** in the shared
  `validatePartitionSource` (`provision/validate.go:147`). Bucket commands
  must keep loading env configs that omit `partitions:` — which is every
  existing config today — without new failures. `validatePartitionSource`
  is unchanged.
- A new exported helper `ValidatePartitionSet(partitions []types.Partition)
  error` performs: at least one record; each `types.Partition.Validate()`;
  no two records share a `CanonicalID`. Errors wrap `ErrInvalidConfig`. The
  `partitions` subcommand calls it; the bucket commands do not.
- **Full-table removal is out of scope for Phase 3.** Because
  `ValidatePartitionSet` requires at least one record, `partictl partitions`
  cannot prune the table to zero. The runtime can represent an empty table
  (`decodePartitions` returns an empty list for empty data,
  `source/nats_kv.go:993-996`; `validateAndDedupe` has no minimum-count
  check, `source/nats_kv.go:1051-1071`), but intentionally emptying the
  partition table is a destructive operation with no Phase 3 surface. An
  operator who needs it uses the runtime `source.NatsKV` API directly. The
  W4 CLI tests assert this refusal so an implementer does not infer the
  opposite from the generic removal language.

### W2 — `PlanPartitions`: record-level diff

New file `provision/partition_records.go`:

```go
func PlanPartitions(ctx context.Context, js jetstream.JetStream, cfg Config) (PlanResult, error)
```

Algorithm:

1. **Static config validation — inherited boundary.** Run the inherited
   `Validate(cfg)` first. This enforces the Phase 1 input-schema contract
   before any NATS I/O: an unsupported `apiVersion`
   (`provision/validate.go:59-63`) and an empty `partitionSource.bucket` /
   `.key` (`provision/validate.go:145-153`) are rejected with an error
   wrapping `ErrInvalidConfig`, exactly as the bucket commands enforce it.
   Then require `cfg.PartitionSource != nil` and
   `len(cfg.PartitionSource.Partitions) > 0`; otherwise return an error
   wrapping `ErrInvalidConfig` ("no partitions declared in
   `partitionSource.partitions`"). Finally run `ValidatePartitionSet` on the
   declared records. All three checks complete before step 2's bucket
   lookup, so a malformed config yields CLI exit `3`, never a NATS-class
   error. (`Validate` skips a nil `PartitionSource` without error, so the
   explicit non-nil check here is still required.)
2. Look up the partition-source bucket by exact name
   (`cfg.PartitionSource.Bucket`). If the bucket does not exist, return
   `ErrPartitionBucketMissing` (a typed sentinel that satisfies
   `errors.Is(err, ErrLiveValidation)`), message directing the operator to
   `partictl apply` first.
3. Read the KV key (`cfg.PartitionSource.Key`):
   - key found → `partcodec.Decode` the value → `live []types.Partition`.
   - `ErrKeyNotFound` → `live` is empty.
   **Deleted/purged-key contract (explicit).** nats.go `KeyValue.Get` maps a
   delete or purge marker to `ErrKeyNotFound` indistinguishably from a
   never-written key. Phase 3 deliberately treats all three the same: live
   is the empty table and the declared config is the source of truth. This
   is correct for a declarative tool — if the table was deleted, republishing
   from config is the intended recovery, and `kv.Create` after a delete
   marker recreates the key cleanly (it consumes the delete revision). Phase
   3 does **not** probe KV history to distinguish never-written from
   deleted/purged; the runtime's in-memory `known`/revision distinction
   (`source/nats_kv.go:750-754`, `source/nats_kv.go:868-875`) is a runtime
   recovery concern, not a provisioning one. The W2/W4 tests cover a
   delete-marker and a purge-marker input alongside the never-written case
   and assert all three behave as empty-table first-publish.
4. Compute the diff (pure helper `diffPartitions`):
   - `added`   — `CanonicalID` in desired, not in live.
   - `removed` — `CanonicalID` in live, not in desired.
   - `changed` — same `CanonicalID`, different `Weight`.
   Each list sorted by `CanonicalID`.
5. Build the `PlanResult`:
   - If the diff is non-empty, emit exactly one `PlannedAction` of kind
     `write-partitions` and one `DriftFinding` of kind `partition-records`,
     severity `drift-mutable` (records are always reconcilable in place by
     a KV write), with counts in `Detail`.
   - If the diff is empty, emit no action and one `informational`
     `partition-records` finding.

New types in `provision/types.go`:

```go
const ActionWritePartitions = "write-partitions"
const KindPartitionRecords  = "partition-records"

type WritePartitionsResource struct {
    Bucket  string                  `json:"bucket"`
    Key     string                  `json:"key"`
    Added   []types.Partition       `json:"added"`
    Removed []types.Partition       `json:"removed"`
    Changed []PartitionWeightChange `json:"changed"`
    Before  []types.Partition       `json:"before"` // live records at plan time
    After   []types.Partition       `json:"after"`  // full desired set
}

type PartitionWeightChange struct {
    Keys      []string `json:"keys"`
    OldWeight int64    `json:"oldWeight"`
    NewWeight int64    `json:"newWeight"`
}
```

`Before` / `After` are deep copies (the `Keys` slices reallocated) so the
`PlanResult` is immutable regardless of later apply mutation. The
`PlannedAction.Name` is the bucket name (consistent with `create-kv` /
`update-kv`). There is at most one `write-partitions` action per plan.

### W3 — `ApplyPartitions`: CAS write with `--prune` gate

```go
func ApplyPartitions(ctx context.Context, js jetstream.JetStream, plan PlanResult, prune bool) (Report, error)
```

`ApplyPartitions` executes the single `write-partitions` action in `plan`.
Step order mirrors the Phase 2 `update-kv` apply
(`provision/apply_update.go:50-123`): re-read → no-op short-circuit →
stale-before check → write. Phase 3 adds an apply-boundary validation gate
and a removal gate. Unlike Phase 2's last-writer-wins `UpdateKeyValue`, the
KV entry carries a revision, so the write is a real CAS.

Every `Report` `ApplyPartitions` returns — **including the no-action
case** — carries `APIVersion: "parti.io/provision/v1"`, `Kind: "Report"`,
and non-nil (possibly empty) `Executed` / `Skipped` / `Errors` slices,
exactly as `Apply` initializes them (`provision/apply.go:78-85`). "Empty
Report" never means a zero-value struct — the inherited output-envelope
invariant holds on every return path.

Algorithm:

1. **Action extraction.** Scan `plan.Actions` for `write-partitions`
   actions. Zero → return the initialized envelope `Report` with no executed
   actions (nothing to do). More than one → error wrapping
   `ErrInvalidConfig` ("plan contains N write-partitions actions; expected
   at most one"). Type-assert `Resource` to `*WritePartitionsResource`; a
   failed assertion → error wrapping `ErrInvalidConfig` ("write-partitions
   action carries an unexpected resource type").

2. **Apply-boundary validation (the safety gate).** `PlannedAction.Resource`
   is mutable `any` (`provision/types.go:93-97`); an SDK caller can mutate or
   forge `After` between `PlanPartitions` and `ApplyPartitions`. The runtime
   read path rejects an invalid or duplicate-`CanonicalID` table as KV
   corruption (`source/nats_kv.go:1024-1040`), and `partcodec.Encode` does
   no validation by design — so apply must not trust plan-time validation
   alone. Before any I/O, run `ValidatePartitionSet` on `Resource.After`; on
   failure → error wrapping `ErrInvalidConfig`, no write. Then deep-copy the
   validated set (reallocate each `Keys` slice) into a local `target`; all
   subsequent comparison and encoding use `target`, never the caller-owned
   slice. This makes the exported boundary safe regardless of how `After`
   was produced, including a forged plan that bypassed `PlanPartitions`.

3. **Bucket lookup.** Look up the bucket by exact name. Missing →
   `ErrPartitionBucketMissing` (satisfies `errors.Is(err, ErrLiveValidation)`).

4. **Re-read** the key → `current []types.Partition` and `revision`, or
   `ErrKeyNotFound` (treated as empty `current` per the W2 deleted/purged
   contract). Decode via `partcodec.Decode`.

5. **No-op short-circuit** — runs *before* the removal gate, mirroring the
   Phase 2 order (`provision/apply_update.go:108-123`). If `current`
   canonically equals `target` (same `CanonicalID` set, same per-record
   `Weight`), record `ExecutedAction{Kind: write-partitions, Name: bucket}`
   with `Raced: true` when `current` differs from the plan-time `Before` (a
   concurrent writer already converged the table) and `Raced: false`
   otherwise. No write. Consequence: a plan whose plan-time diff contained
   removals still succeeds with no write — and needs no `--prune` — when the
   live table is already converged. `--prune` gates an *actual removal
   write*, not the plan's history.

6. **Stale-before / genuine race.** If `current` differs from both `Before`
   and `target`, the live table changed since plan time in a way that is not
   the operator's intent. Record a `ResourceError` ("partition source
   changed since plan; re-run partictl partitions plan"), no write.
   `Report.Errors` non-empty → CLI exit `1`.

7. **Removal gate — computed from the re-read, not the plan.** After steps 5
   and 6 the only remaining case is `current == Before && current != target`.
   Compute `liveRemovals` = `CanonicalID`s in `current` absent from
   `target`. If `len(liveRemovals) > 0` and `prune` is false, record a
   `ResourceError` ("apply would remove N partition(s); re-run with
   --prune"), no write. Because `current == Before` here, `liveRemovals`
   equals the plan-time `Removed`; deriving it from `current` rather than
   from the cached plan is what lets the raced-already-converged case in
   step 5 correctly bypass the gate.

8. **Write.** Encode `target` with `partcodec.Encode` and:
   - `ErrKeyNotFound` at step 4 → `kv.Create(ctx, key, data)`.
   - key found → `kv.Update(ctx, key, data, revision)` — CAS on the
     re-read revision.
   On CAS conflict / create-already-exists (a concurrent writer landed
   between step 4 and step 8), record a `ResourceError`: `"partition source
   %q key %q changed concurrently during apply; re-run partictl partitions
   plan"`. One CAS attempt, no retry loop — the race is surfaced honestly
   rather than silently overwritten. CLI exit `1`.

9. Success → `ExecutedAction{Kind: write-partitions, Name: bucket,
   Raced: false}`.

**Failure-return contract.** Resource-level failure and context
cancellation populate the `Report` and the returned `error` distinctly —
all three forms still carry the envelope fields from the paragraph above:

- **Resource-level failure** (step 6 stale-before genuine race, step 7
  removal gate, step 8 CAS conflict / create-already-exists): the `Report`
  carries exactly one `ResourceError`, `Aborted: false`, and an empty
  `Skipped` (there is only ever one action, and it failed rather than being
  skipped). `ApplyPartitions` also returns a non-nil ordinary
  (non-sentinel) `error`, so the CLI reaches exit `1` through
  `classifyError` (`cmd/partictl/exitcodes.go:42-72`). This mirrors
  `applyPlan`, which records a `ResourceError` and returns a wrapped error
  (`provision/apply.go:161-201`).
- **Context cancellation** (from the step-3 bucket lookup, the step-4
  re-read, or the step-8 write): the `Report` carries `Aborted: true`, no
  `ResourceError`, and the single `write-partitions` action in `Skipped`
  with reason `SkipReasonContextCancelled` (`provision/types.go:227-230`).
  `ApplyPartitions` returns `ctx.Err()`, so the CLI exits `4`
  (`cmd/partictl/exitcodes.go:53-55`). This mirrors `applyPlan`'s
  cancellation handling (`provision/apply.go:102-107`).
- **Plan-shape / apply-boundary validation failure** (step 1
  multiple-action or wrong-resource-type, step 2 `ValidatePartitionSet` on
  `After`): returned before any I/O as an `error` wrapping
  `ErrInvalidConfig`; CLI exit `3`. There is no partial `Report` to
  populate — the failure precedes the write entirely.

**Success semantics — honest scope.** A successful `ApplyPartitions` means
*its single CAS landed* — not that the table stays converged to `target`
afterward. The runtime `source.NatsKV.Update` is an authoritative
last-writer-wins replace that retries through CAS conflicts
(`source/nats_kv.go:418-424`, `source/nats_kv.go:453-486`): a runtime
`Update` that began from the same revision will, after losing the CAS,
refresh and re-apply its own replacement at the next revision — overwriting
the `partictl` result, with no error surfaced to either writer. Phase 3
adds no cross-writer coordination (that needs a mechanism beyond one KV
key). The contract is best-effort, matching the Phase 2 honesty posture for
last-writer windows; the W5 docs state it plainly so an operator does not
read apply success as a durability guarantee against a concurrently-racing
runtime writer. A deterministic fake test encodes this: a runtime-style
retry that overwrites after a successful `partictl` CAS.

**Testability seam.** Mirror the Phase 2 `streamReader` / `kvUpdater`
pattern (`provision/apply_update.go`): define a minimal interface (`Get`
returning value+revision, `Create`, `Update`) over the live
`jetstream.KeyValue` so the full step ordering — boundary validation,
no-op, stale-before, removal gate, CAS-conflict — is unit-testable without
a live server.

### W4 — `partictl partitions` subcommand

New file `cmd/partictl/cmd_partitions.go`. Adds a `partitions` command with
two subcommands:

```
partictl partitions plan  -f parti-env.yaml [-json] [-fail-on-drift]
partictl partitions apply -f parti-env.yaml [-json] [--prune] [--dry-run]
```

- `-f` loads the same `parti-env.yaml`; `partitionSource.bucket` /
  `.key` locate the bucket and key, `partitionSource.partitions` is the
  desired set.
- `partitions plan` → `provision.PlanPartitions`, renders the diff (text or
  `-json`). `-fail-on-drift` → exit `2` when the diff is non-empty (reuses
  `hasDrift`, which already treats `drift-mutable` as drift).
- `partitions apply` → `PlanPartitions` then `ApplyPartitions`.
  `--dry-run` aliases `partitions plan` (emits the `Plan` envelope, no
  write), consistent with the existing `apply --dry-run`.
- `--prune` is the only record-removal safety control. The `--policy` flag
  is **not** accepted by `partitions` (policy governs bucket drift only).
- Exit codes via the existing `classifyError` — **no change to
  `cmd/partictl/exitcodes.go` is needed.** `classifyError` already routes
  `ErrLiveValidation` to exit `3` (`cmd/partictl/exitcodes.go:57-58`), and
  `ErrPartitionBucketMissing` is defined (W3) to satisfy `errors.Is(err,
  ErrLiveValidation)`, so it inherits exit `3` with no new sentinel branch.
  Mapping: `ErrInvalidConfig` (including apply-boundary validation failure
  and the multiple-action error) → `3`; `ErrPartitionBucketMissing` → `3`;
  CAS conflict / removal-gate / stale-before (surfaced as `Report.Errors`)
  → `1`; NATS connect / context → `4`; success → `0`.
- JSON output reuses the `parti.io/provision/v1` envelope helpers in
  `cmd/partictl/output.go`.

### W5 — Documentation

- `docs/PROVISION.md`: a "Partition Records" section — the
  `partitionSource.partitions` schema, `partictl partitions plan/apply`,
  the `--prune` semantics, the bucket-must-exist precondition, and the
  large-table split-file pattern.
- Package godoc for the new exported surface (`PlanPartitions`,
  `ApplyPartitions`, `ValidatePartitionSet`, `WritePartitionsResource`,
  `PartitionWeightChange`, `ErrPartitionBucketMissing`,
  `ActionWritePartitions`, `KindPartitionRecords`).
- `CHANGELOG.md`: a new release section.

## Work Items

| ID | Scope | Impl model | Review effort |
|----|-------|------------|---------------|
| W0 | Extract `internal/partcodec` (`Encode`/`Decode`); rewire `source`; golden-bytes + round-trip tests | sonnet | xhigh |
| W1 | `PartitionSourceConfig.Partitions`; `ValidatePartitionSet`; leave `validatePartitionSource` untouched | sonnet | high |
| W2 | `PlanPartitions`, `diffPartitions`, `write-partitions` action + `partition-records` drift kind + new types | sonnet | high |
| W3 | `ApplyPartitions` — CAS write, `--prune` gate, testability seam, `ErrPartitionBucketMissing` | opus  | xhigh |
| W4 | `partictl partitions plan/apply` subcommand; exit-code wiring | sonnet | high |
| W5 | `docs/PROVISION.md`, godoc, `CHANGELOG.md` | sonnet | high |

Per-work-item loop (unchanged from Phases 1-2): sub-spec if the item needs
one → implement → `/simplify` → codex post-impl review → fix → re-review →
squash. W0 and W3 are the sharp items (wire-format byte-equivalence;
CAS/race ordering) and carry `xhigh` review effort.

## Test Plan

Each invariant has an encoding:

- **W0 byte-equivalence:** golden-bytes fixture (fixed input → fixed encoded
  bytes, asserted stable); `Decode(Encode(x)) == x` round trip; `Decode` on
  a plain-JSON payload **and** on a gzip payload (the dual-format contract,
  `source/nats_kv.go:998-1017`); duplicate `CanonicalID` rejection; the
  fixture decodes through the live `source` read path unchanged.
- **W1 validation:** empty/nil `partitions` rejected by `ValidatePartitionSet`
  but **accepted** by `validatePartitionSource` (bucket commands still
  load); invalid record (dot/whitespace/empty key) rejected; duplicate
  `CanonicalID` rejected; YAML round-trip of `keys:` / `weight:`.
- **W2 diff:** add-only, remove-only, weight-change-only, mixed; empty diff
  → informational finding, no action; key-not-found (first publish),
  delete-marker, and purge-marker inputs all → empty live / all desired =
  added; bucket-missing → `ErrPartitionBucketMissing`; deterministic
  `CanonicalID` ordering of every list.
- **W2 static-validation boundary:** `PlanPartitions` rejects a bad
  `apiVersion`, an empty `partitionSource.bucket`, an empty
  `partitionSource.key`, and an omitted/empty `partitionSource.partitions`
  with `ErrInvalidConfig` *before* any bucket lookup; and an env config
  with no `partitionSource.partitions` is still accepted by the ordinary
  bucket commands (`plan`/`apply`/`adopt`) — no cross-contamination.
- **W3 apply (via the seam, no live server):**
  - clean create (key absent); clean CAS update.
  - no-op short-circuit with `Raced` false and true.
  - **apply-boundary validation:** `After` mutated to a duplicate
    `CanonicalID`, to an empty-key record, and via an aliased `Keys` slice
    → `ApplyPartitions` returns `ErrInvalidConfig`, KV bytes unchanged; a
    forged plan built without `PlanPartitions` is likewise refused before
    `Create`/`Update`.
  - plan with zero / two `write-partitions` actions → envelope `Report` /
    `ErrInvalidConfig` respectively; wrong `Resource` concrete type →
    `ErrInvalidConfig`.
  - **no-action `Report` envelope:** zero-action plan returns a `Report`
    with `APIVersion`, `Kind`, and non-nil empty slices.
  - stale-before genuine race → `ResourceError`, no write.
  - removal-gate ordering: plan with removals, live already converged to
    `After`, `prune=false` → no-op success (`Raced=true`), gate bypassed;
    plan with removals, `current == Before != After`, `prune=false` →
    refused before write; same with `prune=true` → written.
  - CAS conflict at write time → `ResourceError`.
  - concurrent-runtime overwrite: a runtime-style CAS-retry write lands
    after a successful `partictl` CAS → `partictl` still reports success
    (encodes the best-effort success semantics).
  - bucket-missing → `ErrPartitionBucketMissing`.
  - **failure-return shapes:** a resource-level failure (stale-before /
    removal gate / CAS conflict) returns a `Report` with one
    `ResourceError`, `Aborted=false`, empty `Skipped`, and a non-nil
    non-sentinel error; a cancelled `ctx` at bucket lookup / re-read /
    write returns a `Report` with `Aborted=true`, no `ResourceError`, the
    `write-partitions` action in `Skipped` with reason `context-cancelled`,
    and `ctx.Err()` as the returned error.
- **W4 CLI:** `partitions plan` exit `0` (no diff) / `2` (`-fail-on-drift`,
  diff present); `partitions apply` exit `0` / `1` (removal gate, CAS
  conflict) / `3` (`ErrPartitionBucketMissing`, apply-boundary validation);
  `--dry-run` performs no write; `--policy` rejected on `partitions`;
  full-table wipe (`partitions: []`, with or without `--prune`) rejected;
  JSON envelope `apiVersion` present on plan and apply output.
- **Integration:** a live-NATS end-to-end test — provision the bucket with
  `partictl apply`, then `partictl partitions apply` first-publish, then a
  second apply with an add + a weight change + (with `--prune`) a removal;
  assert the runtime `source.NatsKV` reads back exactly the declared set.

## Open Design Decisions

Surfaced for `plan-review`; the rest of the plan assumes the stated choice.

1. **Inline vs separate partition file.** Chosen: inline
   `partitionSource.partitions:` as canonical, with a documented split-file
   pattern for large tables. Rejected for now: a first-class separate file
   with its own `kind: PartitionSet`. Reopen only if the large-table case
   is judged the dominant one.
2. **Removal safety.** Chosen: `--prune` gates an *actual removal write*,
   evaluated against the apply-time re-read (W3 step 7), not the plan's
   history. A plan whose plan-time diff contained removals still succeeds
   with no `--prune` when the live table is already converged (W3 step 5).
   Apply refuses the whole operation (atomic single-key write — no partial
   apply) when the write would genuinely remove records and `--prune` was
   not passed. Adds and weight changes never need `--prune`. Matches the
   Phase 1-2 "no destructive default" posture.
3. **`types.Partition` reuse.** Chosen: reuse the runtime struct directly as
   the config record type, accepting the schema-stability coupling stated
   in W1.
4. **Single CAS attempt, no retry loop.** Chosen: `ApplyPartitions` makes
   one CAS write and surfaces a conflict as a `ResourceError`, rather than
   the 5-retry loop `source.NatsKV.Update` uses. Honest-about-the-race,
   consistent with Phase 2's `update-kv` posture; the operator re-runs
   `partitions plan`. `ApplyPartitions` success means the single CAS landed,
   not that the table stays converged against a concurrently-racing runtime
   writer (W3 "Success semantics — honest scope").
5. **Deleted/purged key = empty table.** Chosen: all `ErrKeyNotFound` cases
   (never-written, deleted, purged) are treated identically as an empty
   live table; config is the source of truth and apply recreates the key.
   Phase 3 does not probe KV history to distinguish them (W2 step 3).
6. **Apply-boundary re-validation.** Chosen: `ApplyPartitions` re-runs
   `ValidatePartitionSet` on `After` at the exported boundary (W3 step 2)
   rather than trusting plan-time validation, because `PlannedAction.Resource`
   is mutable `any` and an SDK caller can forge or mutate it.
