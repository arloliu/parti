# Label-Based Partition Assignment — Implementation Plan

> **For agentic workers:** Execute task-by-task with a fresh review between
> tasks. Steps use checkbox (`- [ ]`) syntax for tracking. Every task ends
> with passing tests and a commit.

**Spec:** `docs/plans/label-assignment/00-design-spec.md` (v5, external
review clean — 5 rounds). Section references (§N) below point into that spec.

**Plan review status:** clean after 3 external review rounds
(`tmp/label-assignment_implplan_v{1..3}_review.md`; round-3 verdict "ready
to execute" conditional on the nil-guard fix in the
`recordLabelReadFailure` adapter, which is applied in Task 4).

**Goal:** Partitions carry an optional label; workers carry a label set;
the leader assigns labeled partitions only to matching workers, with
park-then-spill fallback — VIP partitions promoted/demoted at runtime via
the NATS KV source.

**Architecture:** A grouping layer above the unchanged `AssignmentStrategy`
interface (pools per label, configured strategy runs per pool, merged).
Labels ride existing channels: heartbeats (worker→leader), assignment
payloads (leader→worker, with presence bit), the KV partition list
(operator→leader). Three new leader-side mechanisms: parked-partition
accounting in the publisher contract, a label re-check timer, and a
label-change trigger in the worker monitor. One worker-side mechanism: the
stale-incarnation guard.

**Tech stack:** Go 1.24+, NATS JetStream KV (nats.go), testify, embedded
NATS for tests (`internal/testutil`, `partitest`).

## Global Constraints

- Target version **v2.9.0**, additive only: no breaking change to any
  exported API, wire format, or default behavior (spec I1: zero labels ⇒
  assignment output identical to today).
- **Copy rule (spec §4.1)**: any code copying a `types.Partition` copies
  the struct value (`cp := p`) then re-allocates `Keys` — never enumerate
  fields.
- `Label` is NEVER part of partition identity: `CanonicalID`, `HashID`,
  `Compare`, `PartitionSetDigest` stay label-blind (spec I3).
- Label validation: partition-key charset (non-empty, no dots, no
  whitespace), max 64 bytes. Worker label sets: ≤ 16 labels, sorted,
  deduplicated.
- New config defaults: `UnlabeledPartitionPolicy="dedicated"`,
  `LabelSpillGrace=60s`. Grace `0` = spill immediately.
- Commit messages: no attribution trailers, no plan jargon (no "Task N",
  "§", "P0", reviewer names). Run `make lint` before every commit.
- Tests in `internal/assignment` and `source` follow existing package
  conventions (`partitest.StartEmbeddedNATS(t)` for real-KV unit tests).
  Integration tests live in `test/integration/<pkg>/` and are guarded by
  `testing.Short()`.
- Final gate: `make pre-pr` (lint + `make test` `-race` + `make
  test-integration` `-race`) — this feature touches `internal/assignment/`
  and `source/`, so the gate is mandatory (AGENTS.md).

## File Map

| File | Change |
|---|---|
| `types/partition.go` | `Partition.Label` field + validation; `Assignment.WorkerLabels`/`WorkerLabelsKnown` (runtime + legacy alias wire) |
| `types/heartbeat.go` | `Heartbeat.Labels` |
| `types/assignment_commit.go` | `AssignmentPayload.WorkerLabels`/`WorkerLabelsKnown`; `AssignmentCommit.ParkedCount`/`ParkedDigest` |
| `types/metrics_collector.go` | optional `LabelMetrics` extension interface |
| `source/nats_kv.go` | label-preserving `deepCopyPartitions`/`validateAndDedupe`; label-aware `partitionsEqual` |
| `provision/partition_records.go` | label-preserving `clonePartition`; `diffPartitions` label changes |
| `provision/apply_partitions.go` | label-aware `partitionTablesEqual` |
| `provision/plan.go` (or wherever `PartitionWeightChange` renders) | label-change plan rendering |
| `config.go` | `WorkerLabels`, `UnlabeledPartitionPolicy`, `LabelSpillGrace` + validation/normalization |
| `options.go` | `WithWorkerLabels` |
| `internal/heartbeat/publisher.go` | `SetLabels` + emit in `build()` |
| `internal/assignment/worker_monitor.go` | `GetHeartbeatsFor`; label fingerprints; `SetOnLabelChange` |
| `internal/assignment/labels.go` (new) | pool/group topology + merged assignment computation (pure) |
| `internal/assignment/labels_state.go` (new) | emptySince, defer-once confirmation, re-check timer, `requestLabelRecheck` |
| `internal/assignment/calculator.go` | label read + taxonomy; pipeline wiring in `rebalance`; orphan gauge vs eligible |
| `internal/assignment/config.go` | `UnlabeledPartitionPolicy`, `LabelSpillGrace` |
| `internal/assignment/assignment_publisher.go` | `PublishInput.ParkedPartitions`/`WorkerLabels`; coverage v2; commit parked fields; payload fields; legacy alias copy |
| `manager_assignment.go` | guard in `buildAssignmentFromCommit` callers; reject plumbing |
| `manager.go` | `applyInitialAssignment` reject handling (no alias fallback) |
| `test/integration/assignment/label_*.go` (new) | E2E label suites |
| `docs/`, `README.md`, `CHANGELOG.md` | operator docs + release notes |

Interface names locked for cross-task consistency:

```go
// types
Partition.Label                     string
Heartbeat.Labels                    []string
AssignmentPayload.WorkerLabels      []string
AssignmentPayload.WorkerLabelsKnown bool
Assignment.WorkerLabels             []string
Assignment.WorkerLabelsKnown       bool
AssignmentCommit.ParkedCount        int
AssignmentCommit.ParkedDigest       uint64

// public config (config.go / options.go)
Config.WorkerLabels             []string
Config.UnlabeledPartitionPolicy string   // "dedicated" | "shared"
Config.LabelSpillGrace          time.Duration
func WithWorkerLabels(labels ...string) Option

// internal/assignment
Config.UnlabeledPartitionPolicy string
Config.LabelSpillGrace          time.Duration
func (m *WorkerMonitor) GetHeartbeatsFor(ctx context.Context, workerIDs []string) (map[string]types.Heartbeat, map[string]error, error)
func (m *WorkerMonitor) SetOnLabelChange(fn func())
func buildLabelTopology(in topologyInput) labelTopology
func computeLabelAssignments(strategy types.AssignmentStrategy, topo labelTopology, actions map[string]emptyPoolAction) (map[string][]types.Partition, []types.Partition, error)
func (c *Calculator) requestLabelRecheck(reason string)
var errLabelObservationDeferred error
var errLabelReadBroadFailure error

// internal/heartbeat
func (p *Publisher) SetLabels(labels []string)

// root package
var errLabelIncarnationRejected error   // manager-internal sentinel

// types (metrics extension, type-asserted)
type LabelMetrics interface {
    RecordLabelPoolSize(label string, workers int)
    RecordParkedPartitions(label string, count int)
    IncrementLabelSpill(label string)
    IncrementLabelChangeTrigger()
    IncrementLabelIncarnationReject()
    IncrementUnlabeledFallback()
}
```

---

### Task 1: `Partition.Label` — field, validation, identity blindness

**Files:**
- Modify: `types/partition.go` (struct at :16-26, `Validate` at :34-52)
- Test: `types/partition_label_test.go` (new)

**Interfaces:**
- Consumes: nothing (first task).
- Produces: `types.Partition.Label string` — every later task relies on
  this field existing with `json:"label,omitempty" yaml:"label,omitempty"`.

- [ ] **Step 1: Write the failing tests**

```go
// types/partition_label_test.go
package types_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

func TestPartitionLabel_IdentityBlind(t *testing.T) {
	t.Parallel()

	plain := types.Partition{Keys: []string{"topic", "42"}, Weight: 3}
	vip := types.Partition{Keys: []string{"topic", "42"}, Weight: 3, Label: "vip"}

	require.Equal(t, plain.CanonicalID(), vip.CanonicalID(), "CanonicalID must be label-blind")
	require.Equal(t, plain.HashID(), vip.HashID(), "HashID must be label-blind")
	require.Equal(t, plain.HashIDSeed(7), vip.HashIDSeed(7), "HashIDSeed must be label-blind")
	require.Zero(t, plain.Compare(vip), "Compare must be label-blind")
	require.Equal(t,
		types.PartitionSetDigest([]types.Partition{plain}),
		types.PartitionSetDigest([]types.Partition{vip}),
		"PartitionSetDigest must be label-blind")
}

func TestPartitionLabel_Validate(t *testing.T) {
	t.Parallel()

	valid := func(label string) error {
		return types.Partition{Keys: []string{"k"}, Label: label}.Validate()
	}

	require.NoError(t, valid(""), "empty label = unlabeled, valid")
	require.NoError(t, valid("vip"))
	require.NoError(t, valid("gpu-batch_2"))

	require.Error(t, valid("has space"))
	require.Error(t, valid("has\ttab"))
	require.Error(t, valid("dotted.label"))
	require.Error(t, valid(strings.Repeat("x", 65)), "over 64-byte cap")
	require.NoError(t, valid(strings.Repeat("x", 64)), "exactly 64 bytes ok")
}

func TestPartitionLabel_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	p := types.Partition{Keys: []string{"a"}, Weight: 2, Label: "vip"}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	require.Contains(t, string(b), `"label":"vip"`)

	var back types.Partition
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, p, back)

	// omitempty: unlabeled partitions marshal without the field, so the
	// wire bytes of existing label-free lists are unchanged.
	b2, err := json.Marshal(types.Partition{Keys: []string{"a"}})
	require.NoError(t, err)
	require.NotContains(t, string(b2), "label")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./types/ -run 'TestPartitionLabel' -v`
Expected: compile error — `unknown field Label in struct literal`.

- [ ] **Step 3: Implement**

In `types/partition.go`, extend the struct (after `Weight`):

```go
	// Label optionally pins this partition to workers that carry the same
	// label (see Heartbeat.Labels). Empty means unlabeled: the partition
	// is assigned according to the unlabeled-partition policy. Label is a
	// routing hint, NOT part of partition identity: it does not
	// participate in CanonicalID, HashID, Compare, or PartitionSetDigest.
	Label string `json:"label,omitempty" yaml:"label,omitempty"`
```

Extend `Validate()` (after the Keys loop, before `return nil`):

```go
	if p.Label != "" {
		if len(p.Label) > 64 {
			return fmt.Errorf("partition label exceeds 64 bytes: %q", p.Label)
		}
		if strings.Contains(p.Label, ".") {
			return fmt.Errorf("partition label contains invalid character '.': %q", p.Label)
		}
		if strings.ContainsAny(p.Label, " \t\n\r") {
			return fmt.Errorf("partition label contains whitespace: %q", p.Label)
		}
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./types/ -run 'TestPartitionLabel' -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Run the full types package + lint, commit**

Run: `go test ./types/ && make lint`
Expected: all green (identity funcs untouched, so existing digest tests
still pass).

```bash
git add types/partition.go types/partition_label_test.go
git commit -m "feat(types): add optional routing label to Partition"
```

---

### Task 2: Source label preservation + label-aware change detection

The load-bearing task for the VIP flow (spec §4.1 audit + §10). Both copy
paths currently strip unknown fields, and `partitionsEqual` is
Keys+Weight-only — a label-only rewrite would be silently swallowed
**before and at** the comparison.

**Files:**
- Modify: `source/nats_kv.go` — `deepCopyPartitions` (:1271-1281),
  `validateAndDedupe` (:1314-1334), `partitionsEqual` (:1285-1304)
- Test: `source/nats_kv_label_test.go` (new)

**Interfaces:**
- Consumes: `types.Partition.Label` (Task 1).
- Produces: a `source.NatsKV` whose `Snapshot` returns labels intact and
  whose `Watch` channel fires on label-only edits. Tasks 8/13/14 rely on
  both behaviors.

- [ ] **Step 1: Write the failing unit tests (copy paths + equality)**

```go
// source/nats_kv_label_test.go
package source

import (
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

func TestDeepCopyPartitions_PreservesLabel(t *testing.T) {
	t.Parallel()

	in := []types.Partition{{Keys: []string{"a"}, Weight: 2, Label: "vip"}}
	out := deepCopyPartitions(in)

	require.Equal(t, in, out)
	// Still a deep copy: mutating the copy's Keys must not alias the input.
	out[0].Keys[0] = "mutated"
	require.Equal(t, "a", in[0].Keys[0])
}

func TestValidateAndDedupe_PreservesLabel(t *testing.T) {
	t.Parallel()

	in := []types.Partition{{Keys: []string{"a"}, Weight: 2, Label: "vip"}}
	out, err := validateAndDedupe(in)
	require.NoError(t, err)
	require.Equal(t, "vip", out[0].Label)

	// Same keys + different labels = duplicate identity → error (spec §4.1).
	_, err = validateAndDedupe([]types.Partition{
		{Keys: []string{"a"}, Label: "vip"},
		{Keys: []string{"a"}, Label: "batch"},
	})
	require.Error(t, err)
}

func TestPartitionsEqual_LabelAware(t *testing.T) {
	t.Parallel()

	a := []types.Partition{{Keys: []string{"a"}, Weight: 1}}
	b := []types.Partition{{Keys: []string{"a"}, Weight: 1, Label: "vip"}}

	require.True(t, partitionsEqual(a, a))
	require.False(t, partitionsEqual(a, b), "label-only difference must be a change")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./source/ -run 'TestDeepCopy|TestValidateAndDedupe_Preserves|TestPartitionsEqual_Label' -v`
Expected: `TestDeepCopyPartitions_PreservesLabel` and
`TestValidateAndDedupe_PreservesLabel` FAIL (label stripped);
`TestPartitionsEqual_LabelAware` FAILS (labels compare equal).

- [ ] **Step 3: Implement (struct-value copy rule)**

`deepCopyPartitions` — replace the field-enumerating construction:

```go
func deepCopyPartitions(partitions []types.Partition) []types.Partition {
	result := make([]types.Partition, len(partitions))
	for i, p := range partitions {
		cp := p // struct-value copy: all scalar fields (Weight, Label, future additions)
		cp.Keys = make([]string, len(p.Keys))
		copy(cp.Keys, p.Keys)
		result[i] = cp
	}

	return result
}
```

`validateAndDedupe` — same replacement inside the loop:

```go
		// Deep-copy Keys to protect the encode→write window against caller mutation.
		cp := p // struct-value copy preserves Weight, Label, and future fields
		cp.Keys = make([]string, len(p.Keys))
		copy(cp.Keys, p.Keys)
		result = append(result, cp)
```

`partitionsEqual` — add the label comparison next to Weight:

```go
	for i := range a {
		if a[i].Weight != b[i].Weight {
			return false
		}
		if a[i].Label != b[i].Label {
			return false
		}
		...
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./source/ -run 'TestDeepCopy|TestValidateAndDedupe_Preserves|TestPartitionsEqual_Label' -v`
Expected: PASS.

- [ ] **Step 5: Write the failing end-to-end propagation regression (real KV, watch + reconcile paths)**

This is the regression the spec calls out explicitly (§10): a
`partitionsEqual`-only unit test cannot catch the deep-copy strip, so the
test must drive the full decode → store → notify path against a real
bucket.

```go
// appended to source/nats_kv_label_test.go
import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/partcodec"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go/jetstream"
)

// newLabelTestSource creates a real KV bucket, seeds it with `initial`,
// and returns a started NatsKV plus the raw KV handle for out-of-band
// writes (simulating an external operator/writer process).
func newLabelTestSource(t *testing.T, initial []types.Partition) (*NatsKV, jetstream.KeyValue, context.Context) {
	t.Helper()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "label-src-test"})
	require.NoError(t, err)

	seed, err := partcodec.Encode(initial)
	require.NoError(t, err)
	_, err = kv.Put(ctx, "partitions", seed)
	require.NoError(t, err)

	src := NewNatsKV(kv, "partitions", nil) // nil logger is the package's test convention
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	return src, kv, ctx
}

// TestNatsKV_LabelOnlyEdit_WatchPathPropagates is the spec §10 regression:
// a rewrite that changes ONLY a label (same keys, same weights) must fire
// the Watch signal and the next Snapshot must carry the label.
func TestNatsKV_LabelOnlyEdit_WatchPathPropagates(t *testing.T) {
	t.Parallel()

	initial := []types.Partition{
		{Keys: []string{"p0"}, Weight: 1},
		{Keys: []string{"p1"}, Weight: 1},
	}
	src, kv, ctx := newLabelTestSource(t, initial)

	watchCh := src.Watch(ctx)

	// Label-only rewrite via an out-of-band KV write (external writer).
	promoted := []types.Partition{
		{Keys: []string{"p0"}, Weight: 1, Label: "vip"}, // <- only delta
		{Keys: []string{"p1"}, Weight: 1},
	}
	encoded, err := partcodec.Encode(promoted)
	require.NoError(t, err)
	_, err = kv.Put(ctx, "partitions", encoded)
	require.NoError(t, err)

	select {
	case <-watchCh:
		// change notification observed
	case <-time.After(10 * time.Second):
		t.Fatal("label-only edit did not fire the source Watch signal")
	}

	parts, _, _, err := src.Snapshot(ctx)
	require.NoError(t, err)
	byID := map[string]string{}
	for _, p := range parts {
		byID[p.CanonicalID()] = p.Label
	}
	require.Equal(t, "vip", byID[types.Partition{Keys: []string{"p0"}}.CanonicalID()],
		"snapshot must carry the label through decode → deep-copy → store")
}

// TestNatsKV_LabelOnlyEdit_ReconcilePathPropagates drives the same edit
// through the reconcile path: the watcher is starved by writing while the
// source's watch is torn down, then the periodic reconcile must pick up
// the label-only change and notify.
func TestNatsKV_LabelOnlyEdit_ReconcilePathPropagates(t *testing.T) {
	t.Parallel()

	initial := []types.Partition{{Keys: []string{"p0"}, Weight: 1}}

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "label-src-reconcile"})
	require.NoError(t, err)
	seed, err := partcodec.Encode(initial)
	require.NoError(t, err)
	_, err = kv.Put(ctx, "partitions", seed)
	require.NoError(t, err)

	// Aggressive reconcile; leadership probe true = leader cadence. The
	// watcher is FROZEN (a never-delivering injected watchFn, the same
	// harness shape existing reconcile tests use — see the frozen-watcher
	// pattern around source/nats_kv_test.go:446) so ONLY the reconcile
	// loop can observe the direct KV write. Without freezing, the watch
	// path could deliver first and this test would prove nothing about
	// reconcile.
	src := NewNatsKV(kv, "partitions", nil,
		WithReconcileInterval(200*time.Millisecond),
		WithLeadershipProbe(func() bool { return true }))
	src.watchFn = frozenWatchFn(t) // reuse/adapt the existing test helper
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	watchCh := src.Watch(ctx)

	promoted := []types.Partition{{Keys: []string{"p0"}, Weight: 1, Label: "vip"}}
	encoded, err := partcodec.Encode(promoted)
	require.NoError(t, err)
	_, err = kv.Put(ctx, "partitions", encoded)
	require.NoError(t, err)

	// With the watcher frozen, this notification can ONLY come from the
	// reconcile loop — pinning that reconcile's skip-guard does not skip
	// a label-only change (its identity check is revision-based and every
	// KV Put advances the revision).
	select {
	case <-watchCh:
	case <-time.After(10 * time.Second):
		t.Fatal("label-only edit did not propagate via the reconcile path")
	}

	parts, _, _, err := src.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, "vip", parts[0].Label)
}
```

- [ ] **Step 6: Run the propagation tests**

Run: `go test ./source/ -run 'TestNatsKV_LabelOnlyEdit' -v -race`
Expected: PASS — they should pass immediately given Step 3; if either
fails, a copy/notify path was missed. Do not proceed until green.

Note: if `partitest.StartEmbeddedNATS` differs from the helper the
`source` package's existing tests use, match whatever
`source/nats_kv_test.go` already uses — the assertions, not the harness,
are the contract.

- [ ] **Step 7: Full package + lint + commit**

Run: `go test ./source/ -race && make lint`

```bash
git add source/nats_kv.go source/nats_kv_label_test.go
git commit -m "fix(source): preserve partition labels through copy paths and detect label-only changes"
```

---

### Task 3: Provision writer label support

The in-repo provisioning SDK/CLI is a partition-list writer; without this
task an operator using `provision` erases labels on every plan/apply
round trip (spec §4.1, §11 rollout rule 2).

**Files:**
- Modify: `provision/partition_records.go` — `clonePartition` (:221-234),
  `diffPartitions` (:168-210 including Godoc)
- Modify: `provision/types.go` — `PartitionWeightChange` (:343-352)
- Modify: `provision/apply_partitions.go` — `partitionTablesEqual` (:317-339)
- Test: `provision/partition_label_test.go` (new)

**Interfaces:**
- Consumes: `types.Partition.Label` (Task 1).
- Produces: label-preserving provision round trips; `PartitionWeightChange`
  gains `OldLabel`/`NewLabel` (name kept for API compatibility — Godoc
  widened to "weight or label change").

- [ ] **Step 1: Write the failing tests**

```go
// provision/partition_label_test.go
package provision

import (
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

func TestClonePartition_PreservesLabel(t *testing.T) {
	t.Parallel()

	p := types.Partition{Keys: []string{"a"}, Weight: 2, Label: "vip"}
	got := clonePartition(p)
	require.Equal(t, p, got)
}

func TestDiffPartitions_LabelOnlyChangeIsVisible(t *testing.T) {
	t.Parallel()

	live := []types.Partition{{Keys: []string{"a"}, Weight: 1}}
	desired := []types.Partition{{Keys: []string{"a"}, Weight: 1, Label: "vip"}}

	added, removed, changed := diffPartitions(live, desired)
	require.Empty(t, added)
	require.Empty(t, removed)
	require.Len(t, changed, 1, "label-only edit must surface as a change in plan output")
	require.Equal(t, "", changed[0].OldLabel)
	require.Equal(t, "vip", changed[0].NewLabel)
	require.Equal(t, int64(1), changed[0].OldWeight)
	require.Equal(t, int64(1), changed[0].NewWeight)
}

func TestPartitionTablesEqual_LabelAware(t *testing.T) {
	t.Parallel()

	a := []types.Partition{{Keys: []string{"a"}, Weight: 1}}
	b := []types.Partition{{Keys: []string{"a"}, Weight: 1, Label: "vip"}}

	require.True(t, partitionTablesEqual(a, a))
	require.False(t, partitionTablesEqual(a, b),
		"label-only apply must not be skipped as a no-op")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./provision/ -run 'TestClonePartition_Preserves|TestDiffPartitions_LabelOnly|TestPartitionTablesEqual_Label' -v`
Expected: compile error on `OldLabel` (field missing), then value
mismatches after adding the field.

- [ ] **Step 3: Implement**

`provision/types.go` — extend the change record (keep the exported name):

```go
// PartitionWeightChange records a partition present in both the live and
// declared tables whose Weight or Label differs. Keys identifies the
// partition (it is unchanged — a different key set is a different
// partition, i.e. an add plus a remove, not a change). The name predates
// label support and is kept for API compatibility.
type PartitionWeightChange struct {
	Keys      []string `json:"keys"`
	OldWeight int64    `json:"oldWeight"`
	NewWeight int64    `json:"newWeight"`
	OldLabel  string   `json:"oldLabel,omitempty"`
	NewLabel  string   `json:"newLabel,omitempty"`
}
```

`provision/partition_records.go`:

```go
// clonePartition deep-copies a partition, reallocating its Keys slice.
func clonePartition(p types.Partition) types.Partition {
	cp := p // struct-value copy: Weight, Label, and future fields
	cp.Keys = slices.Clone(p.Keys)

	return cp
}
```

In `diffPartitions`, replace the weight-only change detection:

```go
	for id, d := range desiredByID {
		live, ok := liveByID[id]
		if !ok {
			added = append(added, clonePartition(d))
			continue
		}
		if live.Weight != d.Weight || live.Label != d.Label {
			changed = append(changed, PartitionWeightChange{
				Keys:      slices.Clone(d.Keys),
				OldWeight: live.Weight,
				NewWeight: d.Weight,
				OldLabel:  live.Label,
				NewLabel:  d.Label,
			})
		}
	}
```

Also update the function Godoc line `changed — records present in both
whose Weight differs` to `changed — records present in both whose Weight
or Label differs`.

`provision/apply_partitions.go` — make equality label-aware. The current
implementation maps CanonicalID → Weight; widen the value:

```go
// partitionTablesEqual reports whether a and b describe the same partition
// table: the same set of CanonicalIDs with the same per-record Weight and
// Label, order independent. Both inputs are duplicate-free (partcodec.Decode
// and ValidatePartitionSet reject duplicate CanonicalIDs), so an equal
// length plus a matching lookup is a bijection.
func partitionTablesEqual(a, b []types.Partition) bool {
	if len(a) != len(b) {
		return false
	}

	type wl struct {
		w int64
		l string
	}
	byID := make(map[string]wl, len(a))
	for _, p := range a {
		byID[p.CanonicalID()] = wl{w: p.Weight, l: p.Label}
	}
	for _, p := range b {
		v, ok := byID[p.CanonicalID()]
		if !ok || v.w != p.Weight || v.l != p.Label {
			return false
		}
	}

	return true
}
```

- [ ] **Step 4: Run tests + full provision package**

Run: `go test ./provision/ -race`
Expected: PASS, including existing plan/apply tests (label fields are
omitempty so existing golden JSON stays byte-identical for label-free
tables).

- [ ] **Step 5: Lint + commit**

```bash
make lint
git add provision/
git commit -m "fix(provision): preserve and diff partition labels in plan and apply paths"
```

---

### Task 4: Config surface + heartbeat labels

**Files:**
- Modify: `config.go` — new fields near `WorkerIDPrefix` (:384), defaults,
  `Validate`
- Modify: `options.go` — `WithWorkerLabels`
- Modify: `types/heartbeat.go` — `Labels` field
- Modify: `internal/heartbeat/publisher.go` — `SetLabels` + emit in
  `build()` (:379-404)
- Modify: `manager.go` / `manager_election.go` — resolve labels at
  construction; wire into the heartbeat publisher in `startHeartbeat`
  (manager_election.go:414)
- Modify: `internal/assignment/config.go` — `UnlabeledPartitionPolicy`,
  `LabelSpillGrace` (consumed by Task 8); thread from
  `startCalculator` (manager_assignment.go:116)
- Test: `config_labels_test.go`, `types/heartbeat_test.go` (extend),
  `internal/heartbeat/publisher_test.go` (extend)

**Interfaces:**
- Consumes: Task 1.
- Produces (locked):
  - `Config.WorkerLabels []string` `yaml:"workerLabels"`
  - `Config.UnlabeledPartitionPolicy string` `yaml:"unlabeledPartitionPolicy" default:"dedicated" validate:"oneof=dedicated shared"`
  - `Config.LabelSpillGrace time.Duration` `yaml:"labelSpillGrace" default:"60s" validate:"gte=0"`
  - `func WithWorkerLabels(labels ...string) Option` — overrides
    `Config.WorkerLabels` (option wins; needed because test clusters share
    one Config across workers)
  - `types.Heartbeat.Labels []string` `json:"labels,omitempty"`
  - `func (p *heartbeat.Publisher) SetLabels(labels []string)` — call
    before `Start`, stores a sorted/deduped copy
  - `assignment.Config.UnlabeledPartitionPolicy string`,
    `assignment.Config.LabelSpillGrace time.Duration`
  - normalization helper `normalizeWorkerLabels(labels []string) ([]string, error)`
    in `config.go`: sort, dedupe, validate each label with the same rule
    as `Partition.Validate` (≤64 bytes, no dots/whitespace, non-empty),
    cap 16 labels.

- [ ] **Step 1: Write the failing config tests**

```go
// config_labels_test.go
package parti

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConfig_WorkerLabelsNormalization(t *testing.T) {
	t.Parallel()

	got, err := normalizeWorkerLabels([]string{"vip", "batch", "vip"})
	require.NoError(t, err)
	require.Equal(t, []string{"batch", "vip"}, got, "sorted + deduped")

	_, err = normalizeWorkerLabels([]string{"bad label"})
	require.Error(t, err, "whitespace rejected")
	_, err = normalizeWorkerLabels([]string{"dotted.label"})
	require.Error(t, err, "dots rejected")
	_, err = normalizeWorkerLabels([]string{""})
	require.Error(t, err, "empty label rejected")

	seventeen := make([]string, 17)
	for i := range seventeen {
		seventeen[i] = string(rune('a' + i))
	}
	_, err = normalizeWorkerLabels(seventeen)
	require.Error(t, err, "more than 16 labels rejected")
}

func TestConfig_LabelPolicyDefaults(t *testing.T) {
	t.Parallel()

	cfg := Config{}
	require.NoError(t, SetDefaults(&cfg))
	require.Equal(t, "dedicated", cfg.UnlabeledPartitionPolicy)
	require.Equal(t, 60*time.Second, cfg.LabelSpillGrace)

	cfg.UnlabeledPartitionPolicy = "invalid"
	require.Error(t, cfg.Validate())

	cfg.UnlabeledPartitionPolicy = "shared"
	cfg.LabelSpillGrace = -time.Second
	require.Error(t, cfg.Validate())
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test . -run 'TestConfig_WorkerLabels|TestConfig_LabelPolicy' -v`
Expected: compile errors (missing fields / helper).

- [ ] **Step 3: Implement config fields**

In `config.go`, next to `WorkerIDPrefix`:

```go
	// WorkerLabels is this worker's label set, fixed at process startup.
	// Labeled partitions (Partition.Label) are assigned only to workers
	// whose set contains the partition's label. Validated with
	// partition-key charset rules; sorted and deduplicated. At most 16
	// labels, each at most 64 bytes. Empty = unlabeled worker.
	// WithWorkerLabels overrides this field when both are set.
	WorkerLabels []string `yaml:"workerLabels"`

	// UnlabeledPartitionPolicy controls which workers receive unlabeled
	// partitions. "dedicated" (default): unlabeled workers only, falling
	// back to all workers when no unlabeled worker is live. "shared":
	// all workers. Leader-side; MUST be identical across every manager
	// in the fleet (same contract as the AssignmentStrategy choice).
	UnlabeledPartitionPolicy string `yaml:"unlabeledPartitionPolicy" default:"dedicated" validate:"oneof=dedicated shared"`

	// LabelSpillGrace is how long a label's worker pool must be
	// continuously empty before its partitions spill to the fallback
	// ladder. 0 spills immediately. Leader-side; MUST be fleet-uniform.
	LabelSpillGrace time.Duration `yaml:"labelSpillGrace" default:"60s" validate:"gte=0"`
```

Add the normalization helper (near other config helpers):

```go
// normalizeWorkerLabels validates, sorts, and deduplicates a worker label
// set. Rules mirror Partition.Validate's label rules: non-empty, no dots,
// no whitespace, at most 64 bytes each, at most 16 labels total.
func normalizeWorkerLabels(labels []string) ([]string, error) {
	if len(labels) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		if l == "" {
			return nil, errors.New("worker label cannot be empty")
		}
		if len(l) > 64 {
			return nil, fmt.Errorf("worker label exceeds 64 bytes: %q", l)
		}
		if strings.Contains(l, ".") {
			return nil, fmt.Errorf("worker label contains invalid character '.': %q", l)
		}
		if strings.ContainsAny(l, " \t\n\r") {
			return nil, fmt.Errorf("worker label contains whitespace: %q", l)
		}
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	slices.Sort(out)
	if len(out) > 16 {
		return nil, fmt.Errorf("too many worker labels: %d (max 16)", len(out))
	}

	return out, nil
}
```

Call it from `Config.Validate()` (store the normalized result back onto
the field so downstream consumers see canonical form), and follow the
existing pattern used by other validated fields in that function.

Decision (closes spec §16 open item 3): NO inert-config startup warning
for `WorkerLabels` set with no labeled partitions — unlike an inert rate
limit, a labeled-but-idle worker is a *legitimate steady state* (reserved
capacity awaiting VIP promotion, spec §7 "dedicated reserves capacity").
Log the resolved label set once at Info and stop there.

- [ ] **Step 4: `WithWorkerLabels` option**

In `options.go` (mirror `WithLogger`'s shape; the manager keeps a
`workerLabels []string` resolved at `NewManager` time):

```go
// WithWorkerLabels sets this worker's label set, overriding
// Config.WorkerLabels. Labels are fixed for the manager's lifetime,
// published in every heartbeat, and drive label-based partition
// assignment. Invalid labels cause NewManager to return an error.
//
// Use this instead of Config.WorkerLabels when several workers share one
// Config value (e.g. test clusters) but need distinct labels.
func WithWorkerLabels(labels ...string) Option {
	return func(m *Manager) error {
		normalized, err := normalizeWorkerLabels(labels)
		if err != nil {
			return fmt.Errorf("WithWorkerLabels: %w", err)
		}
		m.workerLabels = normalized

		return nil
	}
}
```

(Adapt the closure signature to the repo's actual `Option` type — check
`options.go:32 WithElectionAgent` and copy its exact shape. If `Option`
does not return error, validate lazily in `NewManager` after options run
and return the error from there.)

In `NewManager`, after options are applied: if `m.workerLabels == nil`,
resolve from `cfg.WorkerLabels` via `normalizeWorkerLabels` (error out on
invalid). Log the resolved set once at Info.

- [ ] **Step 5: Heartbeat field + publisher**

`types/heartbeat.go`, after `Capabilities`:

```go
	// Labels is the worker's label set, fixed for the process lifetime.
	// Sorted and deduplicated at publish time. Empty for unlabeled
	// workers and for pre-label workers (additive JSON field).
	Labels []string `json:"labels,omitempty"`
```

`internal/heartbeat/publisher.go` — add field + setter, then emit:

```go
// SetLabels registers the worker's label set to include in every
// heartbeat. Call before Start. The slice is stored as-is (the manager
// passes an already-normalized copy) and must not be mutated afterwards.
func (p *Publisher) SetLabels(labels []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.labels = labels
}
```

In `build()` add `Labels: p.labels,` to the returned `types.Heartbeat`
(read under the same lock discipline as `capsFn` — copy the slice header
into a local before composing if `build` runs without `p.mu`).

In `manager_election.go:startHeartbeat`, after the publisher is
constructed and before `Start`: `pub.SetLabels(m.workerLabels)`.

- [ ] **Step 6: Thread policy/grace into the calculator config**

`internal/assignment/config.go` — add:

```go
	// UnlabeledPartitionPolicy: "dedicated" (default when empty) routes
	// unlabeled partitions to unlabeled workers only (falling back to all
	// workers when none are live); "shared" routes them to all workers.
	UnlabeledPartitionPolicy string

	// LabelSpillGrace is how long a label pool must be continuously empty
	// before its partitions spill. 0 = immediate spill.
	LabelSpillGrace time.Duration

	// OnLabelReadBroadFailure, if set, is invoked when a rebalance aborts
	// because worker label reads failed broadly (connectivity/degrading-
	// JetStream class, or above the isolated-failure cap). The manager
	// wires this to its KV-error recorder so sustained heartbeat-read
	// trouble trips the degraded circuit (spec §6). Mirrors the
	// OnEnumerationError seam. Must be safe for concurrent use.
	OnLabelReadBroadFailure func(err error)
```

`manager_assignment.go:startCalculator` — add to the `assignment.Config`
literal:

```go
		UnlabeledPartitionPolicy: m.cfg.UnlabeledPartitionPolicy,
		LabelSpillGrace:          m.cfg.LabelSpillGrace,
		OnLabelReadBroadFailure:  m.recordLabelReadFailure,
```

The adapter is NOT a bare pass-through — the routing subtlety lives here.
`m.recordKVError` admits only whole-bucket-loss (connectivity/degrading)
or `ErrKVUnavailable`-wrapped errors; everything else is dropped by
`classifyKVError` (`kv_error_classify.go:56-69`,
`manager_degraded.go:140-143`). A COUNT-based broad label-read failure
(many unclassified per-worker Get failures) carries no admissible class,
so passing it straight through would silently no-op — violating the spec
§6 requirement that broad failures reach the KV-error/degraded machinery.
`ErrKVUnavailable` lives in the root package (`manager_degraded.go:38`)
and cannot be imported by `internal/assignment` (cycle), so the wrapping
happens HERE, in the manager-side adapter:

```go
// recordLabelReadFailure routes a broad label-read failure from the
// calculator into the degraded circuit. Classed causes (connectivity /
// degrading JetStream) pass through and are admitted as whole-bucket
// loss. Count-based broad failures (many unclassified per-worker Get
// failures — a connected-but-KV-misbehaving shape) carry no admissible
// class of their own, so wrap ErrKVUnavailable: they enter the window as
// the F-D1 transient class (DegradeReasonKVUnavailable), which healthy
// ops clear — sustained trouble degrades, a transient blip does not.
func (m *Manager) recordLabelReadFailure(err error) {
	if err == nil {
		return // classifyKVError(nil) is also kvRouteDrop — without this
		//        guard a nil input would be wrapped into a live window entry
	}
	if classifyKVError(err).route == kvRouteDrop {
		err = fmt.Errorf("%w: broad worker label read failure: %w", ErrKVUnavailable, err)
	}
	m.recordKVError(err)
}
```

Unit test for the adapter (root package, alongside the classifier tests):

```go
// TestRecordLabelReadFailure_Routing:
//   connectivity-classed cause  → classifyKVError admits it, non-transient
//     (whole-bucket-loss window entry)
//   bare count-based error      → wrapped ErrKVUnavailable; assert
//     classifyKVError(wrapped) == {route: kvRouteWindow, transient: true}
//     and that repeated calls within KVErrorWindow trip the degraded
//     circuit with DegradeReasonKVUnavailable (reuse the existing
//     degraded-circuit test harness in manager_degraded's tests)
//   nil → no-op (no window entry)
```

- [ ] **Step 7: Tests for heartbeat emission**

Extend `types/heartbeat_test.go`: a v1 JSON heartbeat with
`"labels":["vip"]` decodes to `Labels=[]string{"vip"}`; a legacy
timestamp payload decodes to `Labels=nil`; a v1 payload without the field
decodes to nil (distinct from error).

Extend the publisher test (follow the existing test file's harness for
`build()` or a real KV publish): after `SetLabels([]string{"vip"})`, the
published JSON contains `"labels":["vip"]`; without SetLabels the field
is absent (payload byte-compat for unlabeled fleets).

- [ ] **Step 8: Run, lint, commit**

Run: `go test . ./types/ ./internal/heartbeat/ ./internal/assignment/ -race && make lint`

```bash
git add config.go options.go config_labels_test.go types/heartbeat.go types/heartbeat_test.go internal/heartbeat/ internal/assignment/config.go manager.go manager_election.go manager_assignment.go
git commit -m "feat: worker label configuration published via heartbeats"
```

---

### Task 5: `WorkerMonitor.GetHeartbeatsFor`

**Files:**
- Modify: `internal/assignment/worker_monitor.go` (next to
  `GetHeartbeats`, :240-275)
- Test: `internal/assignment/worker_monitor_test.go` (extend; harness:
  `partitest.StartEmbeddedNATS(t)` like the existing tests)

**Interfaces:**
- Consumes: `types.Heartbeat.Labels` (Task 4).
- Produces:
  `GetHeartbeatsFor(ctx, workerIDs []string) (map[string]types.Heartbeat, map[string]error, error)`
  — one bounded `Get` per listed worker, **no** `Keys()` scan. A worker
  whose Get or decode fails is absent from the heartbeat map and present
  in the error map WITH ITS ORIGINAL ERROR — the caller (Task 8) needs
  the error class (connectivity/degrading-JetStream vs not) to apply the
  spec §6 taxonomy; swallowing the class into a bare omission would let a
  connectivity failure masquerade as "unknown labels" (fatal in a
  1-worker fleet, where max(1,10%) = 1 makes every failure "isolated" by
  count). Final error non-nil only if ctx is done before completion.

- [ ] **Step 1: Write the failing test**

```go
func TestWorkerMonitor_GetHeartbeatsFor(t *testing.T) {
	t.Parallel()

	// Same harness as TestWorkerMonitor_GetHeartbeats_DualDecode: embedded
	// NATS, heartbeat bucket, monitor with prefix "heartbeat".
	_, nc := partitest.StartEmbeddedNATS(t)
	// ... create js + KV bucket exactly as the sibling tests do ...

	// Seed: worker-0 v1 JSON with labels, worker-1 v1 JSON without,
	// worker-2 malformed payload.
	putHeartbeat(t, kv, "heartbeat.worker-0", types.Heartbeat{
		WorkerID: "worker-0", SchemaVersion: 1, Labels: []string{"vip"},
		Timestamp: time.Now(),
	})
	putHeartbeat(t, kv, "heartbeat.worker-1", types.Heartbeat{
		WorkerID: "worker-1", SchemaVersion: 1, Timestamp: time.Now(),
	})
	_, err := kv.Put(ctx, "heartbeat.worker-2", []byte("not-json-not-time"))
	require.NoError(t, err)

	m := NewWorkerMonitor(kv, "heartbeat", 5*time.Second, nil, logger)

	got, errs, err := m.GetHeartbeatsFor(ctx, []string{"worker-0", "worker-1", "worker-2", "worker-9"})
	require.NoError(t, err)
	require.Equal(t, []string{"vip"}, got["worker-0"].Labels)
	require.Empty(t, got["worker-1"].Labels)
	_, ok := got["worker-2"]
	require.False(t, ok, "malformed payload omitted from heartbeats")
	require.Error(t, errs["worker-2"], "…but its decode error is preserved for classification")
	_, ok = got["worker-9"]
	require.False(t, ok, "missing key omitted from heartbeats")
	require.Error(t, errs["worker-9"], "…and its Get error is preserved (jetstream.ErrKeyNotFound)")
	require.Len(t, errs, 2)
}
```

(`putHeartbeat` = small helper marshaling the heartbeat to JSON and
`kv.Put`-ing it; add it to the test file if the existing tests don't
already have an equivalent.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/assignment/ -run 'TestWorkerMonitor_GetHeartbeatsFor' -v`
Expected: compile error (method missing).

- [ ] **Step 3: Implement**

```go
// GetHeartbeatsFor returns decoded heartbeats for exactly the given
// worker IDs, keyed by worker ID. Unlike GetHeartbeats it does NOT run a
// Keys() scan — callers that already hold the active worker list (the
// rebalance path) use this to avoid a second stream-wide enumeration.
//
// Per-worker Get or decode failures omit that worker from the map (logged
// at debug); the caller applies its own isolated-vs-broad failure policy.
// The only non-nil error is context cancellation/deadline.
func (m *WorkerMonitor) GetHeartbeatsFor(ctx context.Context, workerIDs []string) (map[string]types.Heartbeat, map[string]error, error) {
	opCtx, cancel := m.boundedOpCtx(ctx)
	defer cancel()

	out := make(map[string]types.Heartbeat, len(workerIDs))
	fails := make(map[string]error)
	for _, workerID := range workerIDs {
		if err := opCtx.Err(); err != nil {
			return out, fails, fmt.Errorf("heartbeat fetch aborted: %w", err)
		}
		key := m.hbPrefix + "." + workerID
		entry, gerr := m.heartbeatKV.Get(opCtx, key)
		if gerr != nil {
			m.logger.Debug("heartbeat get failed during targeted fetch", "key", key, "error", gerr)
			fails[workerID] = gerr
			continue
		}
		hb, derr := types.DecodeHeartbeat(entry.Value())
		if derr != nil {
			m.logger.Debug("heartbeat decode failed during targeted fetch", "key", key, "error", derr)
			fails[workerID] = derr
			continue
		}
		out[workerID] = hb
	}

	return out, fails, nil
}
```

- [ ] **Step 4: Run tests, lint, commit**

Run: `go test ./internal/assignment/ -run 'TestWorkerMonitor' -race && make lint`

```bash
git add internal/assignment/worker_monitor.go internal/assignment/worker_monitor_test.go
git commit -m "feat(assignment): targeted heartbeat fetch for the rebalance path"
```

---

### Task 6: Label topology + assignment computation (pure core)

The grouping layer (spec §7). Pure functions, no calculator state — the
park/spill/defer decisions arrive as inputs (Task 8 computes them), which
keeps this exhaustively unit-testable.

**Files:**
- Create: `internal/assignment/labels.go`
- Test: `internal/assignment/labels_test.go` (new)

**Interfaces:**
- Consumes: `types.AssignmentStrategy`, `types.Partition.Label`.
- Produces (consumed by Task 8):

```go
type topologyInput struct {
	Workers    []string            // active set, post-filter
	Labels     map[string][]string // workerID → sorted label set (successful reads)
	Unknown    map[string]bool     // workerID → labels unreadable (§6); disjoint from Labels
	Partitions []types.Partition   // source snapshot
	Policy     string              // "dedicated" | "shared" ("" = dedicated)
}

type labelTopology struct {
	Pools        map[string][]string          // label → matching workers (sorted)
	Groups       map[string][]types.Partition // label → partitions; "" key = unlabeled group
	SortedLabels []string                     // sorted keys of Groups, excluding ""
	EmptyLabels  []string                     // labels whose pool is empty (sorted)
	GeneralPool  []string                     // per policy; empty ⇒ caller uses AllWorkers
	FallbackPool []string                     // unlabeled workers; empty ⇒ caller uses AllWorkers
	AllWorkers   []string                     // Workers minus Unknown (sorted)
	UnknownSet   map[string]bool              // pass-through of in.Unknown
}

type emptyPoolAction int

const (
	emptyPoolPark emptyPoolAction = iota
	emptyPoolSpill
)

func buildLabelTopology(in topologyInput) labelTopology
func computeLabelAssignments(strategy types.AssignmentStrategy, topo labelTopology, actions map[string]emptyPoolAction) (map[string][]types.Partition, []types.Partition, error)
```

Contract highlights (each is a named test below):
- merged map keys == `topo.AllWorkers ∪ topo.UnknownSet` exactly; unknown
  workers and pool-less workers get explicit empty slices (spec I8);
- each partition appears exactly once across merged ∪ parked (spec I9/I2);
- unlabeled group → `GeneralPool`, or `AllWorkers` when the general pool
  is empty; labeled group with empty pool → `actions[label]`: park
  (returned in parked slice) or spill to `FallbackPool`/`AllWorkers`;
- deterministic: sorted label iteration; strategies sort internally;
- `Strategy.Assign` is never called with an empty worker list.

- [ ] **Step 1: Write the failing tests**

```go
// internal/assignment/labels_test.go
package assignment

import (
	"testing"

	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

func parts(labels ...string) []types.Partition {
	out := make([]types.Partition, len(labels))
	for i, l := range labels {
		out[i] = types.Partition{Keys: []string{string(rune('a' + i))}, Label: l}
	}
	return out
}

func topo(workers []string, labels map[string][]string, partitions []types.Partition, policy string) labelTopology {
	return buildLabelTopology(topologyInput{
		Workers: workers, Labels: labels, Partitions: partitions, Policy: policy,
	})
}

func TestBuildLabelTopology_PoolsAndGroups(t *testing.T) {
	t.Parallel()

	tp := topo(
		[]string{"w0", "w1", "w2"},
		map[string][]string{"w0": {"vip"}, "w1": {"batch", "vip"}, "w2": nil},
		parts("vip", "", "batch", "ghost"),
		"dedicated",
	)

	require.Equal(t, []string{"w0", "w1"}, tp.Pools["vip"])
	require.Equal(t, []string{"w1"}, tp.Pools["batch"])
	require.Equal(t, []string{"w2"}, tp.GeneralPool, "dedicated: unlabeled workers only")
	require.Equal(t, []string{"w2"}, tp.FallbackPool)
	require.Equal(t, []string{"ghost"}, tp.EmptyLabels, "label with no matching worker")
	require.Len(t, tp.Groups[""], 1)
	require.Len(t, tp.Groups["vip"], 1)
}

func TestBuildLabelTopology_SharedPolicy(t *testing.T) {
	t.Parallel()

	tp := topo(
		[]string{"w0", "w1"},
		map[string][]string{"w0": {"vip"}, "w1": nil},
		parts(""),
		"shared",
	)
	require.Equal(t, []string{"w0", "w1"}, tp.GeneralPool, "shared: all workers")
}

func TestComputeLabelAssignments_MergeContract(t *testing.T) {
	t.Parallel()

	// w0 vip-only; w1 unlabeled; w2 unknown labels; one vip partition,
	// one unlabeled partition. Expect: w0 gets vip, w1 gets unlabeled,
	// w2 present with EMPTY slice (I8 — no stale-assignment leak).
	in := topologyInput{
		Workers:    []string{"w0", "w1", "w2"},
		Labels:     map[string][]string{"w0": {"vip"}, "w1": nil},
		Unknown:    map[string]bool{"w2": true},
		Partitions: parts("vip", ""),
		Policy:     "dedicated",
	}
	tp := buildLabelTopology(in)
	merged, parked, err := computeLabelAssignments(strategy.NewRoundRobin(), tp, nil)
	require.NoError(t, err)
	require.Empty(t, parked)

	require.Len(t, merged, 3, "every active worker gets an entry")
	require.NotNil(t, merged["w2"])
	require.Empty(t, merged["w2"], "unknown-label worker: explicit empty entry")
	require.Len(t, merged["w0"], 1)
	require.Equal(t, "vip", merged["w0"][0].Label)
	require.Len(t, merged["w1"], 1)

	total := 0
	for _, ps := range merged {
		total += len(ps)
	}
	require.Equal(t, 2, total, "each partition exactly once (I9)")
}

func TestComputeLabelAssignments_ParkAndSpill(t *testing.T) {
	t.Parallel()

	in := topologyInput{
		Workers:    []string{"w0"},
		Labels:     map[string][]string{"w0": nil},
		Partitions: parts("vip", ""),
		Policy:     "dedicated",
	}
	tp := buildLabelTopology(in)
	require.Equal(t, []string{"vip"}, tp.EmptyLabels)

	// Park:
	merged, parked, err := computeLabelAssignments(strategy.NewRoundRobin(), tp,
		map[string]emptyPoolAction{"vip": emptyPoolPark})
	require.NoError(t, err)
	require.Len(t, parked, 1)
	require.Equal(t, "vip", parked[0].Label)
	require.Len(t, merged["w0"], 1, "unlabeled partition still assigned")

	// Spill:
	merged, parked, err = computeLabelAssignments(strategy.NewRoundRobin(), tp,
		map[string]emptyPoolAction{"vip": emptyPoolSpill})
	require.NoError(t, err)
	require.Empty(t, parked)
	require.Len(t, merged["w0"], 2, "spilled onto the fallback pool")
}

func TestComputeLabelAssignments_SpillPrefersUnlabeledWorkers(t *testing.T) {
	t.Parallel()

	// vip pool empty; batch pool exists; one unlabeled worker. Spilled
	// vip partitions must land on the unlabeled worker, never invade the
	// batch pool (spec I5).
	in := topologyInput{
		Workers:    []string{"batchw", "plainw"},
		Labels:     map[string][]string{"batchw": {"batch"}, "plainw": nil},
		Partitions: parts("vip"),
		Policy:     "dedicated",
	}
	tp := buildLabelTopology(in)
	merged, _, err := computeLabelAssignments(strategy.NewRoundRobin(), tp,
		map[string]emptyPoolAction{"vip": emptyPoolSpill})
	require.NoError(t, err)
	require.Empty(t, merged["batchw"], "spill must not invade another label's pool")
	require.Len(t, merged["plainw"], 1)
}

// TestComputeLabelAssignments_I1Golden: with zero labels anywhere the
// pipeline output must be identical to a direct Strategy.Assign call.
func TestComputeLabelAssignments_I1Golden(t *testing.T) {
	t.Parallel()

	workers := []string{"w0", "w1", "w2"}
	partitions := make([]types.Partition, 10)
	for i := range partitions {
		partitions[i] = types.Partition{Keys: []string{"p", string(rune('0' + i))}, Weight: int64(i%3 + 1)}
	}

	for _, strat := range []types.AssignmentStrategy{
		strategy.NewRoundRobin(),
		strategy.NewConsistentHash(),
	} {
		direct, err := strat.Assign(workers, partitions)
		require.NoError(t, err)

		tp := buildLabelTopology(topologyInput{
			Workers: workers,
			Labels:  map[string][]string{"w0": nil, "w1": nil, "w2": nil},
			Partitions: partitions,
			Policy:     "dedicated",
		})
		merged, parked, err := computeLabelAssignments(strat, tp, nil)
		require.NoError(t, err)
		require.Empty(t, parked)
		require.Equal(t, direct, merged, "label-free pipeline must equal direct Assign")
	}
}

func TestComputeLabelAssignments_Deterministic(t *testing.T) {
	t.Parallel()

	in := topologyInput{
		Workers: []string{"w2", "w0", "w1"},
		Labels:  map[string][]string{"w0": {"vip"}, "w1": {"batch"}, "w2": nil},
		Partitions: parts("vip", "batch", "", "vip"),
		Policy:     "dedicated",
	}
	tp := buildLabelTopology(in)
	first, _, err := computeLabelAssignments(strategy.NewConsistentHash(), tp, nil)
	require.NoError(t, err)
	for range 20 {
		tp2 := buildLabelTopology(in)
		again, _, err := computeLabelAssignments(strategy.NewConsistentHash(), tp2, nil)
		require.NoError(t, err)
		require.Equal(t, first, again)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/assignment/ -run 'TestBuildLabelTopology|TestComputeLabelAssignments' -v`
Expected: compile errors (file absent).

- [ ] **Step 3: Implement `internal/assignment/labels.go`**

```go
package assignment

import (
	"fmt"
	"slices"

	"github.com/arloliu/parti/v2/types"
)

// topologyInput / labelTopology / emptyPoolAction: see interface block in
// the plan header (copy the type definitions verbatim).

// buildLabelTopology partitions workers and partitions into label pools
// and groups (spec §7). Pure and deterministic: all slices sorted.
func buildLabelTopology(in topologyInput) labelTopology {
	topo := labelTopology{
		Pools:      map[string][]string{},
		Groups:     map[string][]types.Partition{},
		UnknownSet: in.Unknown,
	}

	// Active workers minus unknown-label workers, sorted.
	for _, w := range in.Workers {
		if in.Unknown[w] {
			continue
		}
		topo.AllWorkers = append(topo.AllWorkers, w)
		labels := in.Labels[w]
		if len(labels) == 0 {
			topo.FallbackPool = append(topo.FallbackPool, w)
		}
		for _, l := range labels {
			topo.Pools[l] = append(topo.Pools[l], w)
		}
	}
	slices.Sort(topo.AllWorkers)
	slices.Sort(topo.FallbackPool)
	for l := range topo.Pools {
		slices.Sort(topo.Pools[l])
	}

	if in.Policy == "shared" {
		topo.GeneralPool = topo.AllWorkers
	} else { // "dedicated" and "" default
		topo.GeneralPool = topo.FallbackPool
	}

	for _, p := range in.Partitions {
		topo.Groups[p.Label] = append(topo.Groups[p.Label], p)
	}
	for l := range topo.Groups {
		if l == "" {
			continue
		}
		topo.SortedLabels = append(topo.SortedLabels, l)
		if len(topo.Pools[l]) == 0 {
			topo.EmptyLabels = append(topo.EmptyLabels, l)
		}
	}
	slices.Sort(topo.SortedLabels)
	slices.Sort(topo.EmptyLabels)

	return topo
}

// computeLabelAssignments runs the configured strategy once per pool and
// merges (spec §7). actions supplies the park/spill decision for each
// empty-pool label (Task 8 computes them from grace state); a missing
// entry defaults to park — the conservative choice, and unreachable in
// production because the calculator always populates every EmptyLabels
// entry.
func computeLabelAssignments(
	strategy types.AssignmentStrategy,
	topo labelTopology,
	actions map[string]emptyPoolAction,
) (map[string][]types.Partition, []types.Partition, error) {
	merged := make(map[string][]types.Partition, len(topo.AllWorkers)+len(topo.UnknownSet))
	var parked []types.Partition

	assignGroup := func(pool []string, group []types.Partition, what string) error {
		if len(group) == 0 {
			return nil
		}
		if len(pool) == 0 {
			pool = topo.AllWorkers // guarded final rung; callers pre-check
		}
		if len(pool) == 0 {
			return fmt.Errorf("label pipeline: no workers available for %s", what)
		}
		out, err := strategy.Assign(pool, group)
		if err != nil {
			return fmt.Errorf("label pipeline: %s: %w", what, err)
		}
		for w, ps := range out {
			merged[w] = append(merged[w], ps...)
		}

		return nil
	}

	// Unlabeled group → general pool (ladder: general → all).
	if err := assignGroup(topo.GeneralPool, topo.Groups[""], "unlabeled group"); err != nil {
		return nil, nil, err
	}

	// Labeled groups in sorted order.
	for _, l := range topo.SortedLabels {
		pool := topo.Pools[l]
		if len(pool) > 0 {
			if err := assignGroup(pool, topo.Groups[l], "label "+l); err != nil {
				return nil, nil, err
			}
			continue
		}
		switch actions[l] {
		case emptyPoolSpill:
			// Ladder: unlabeled workers first (never another label's
			// dedicated pool), then all workers.
			if err := assignGroup(topo.FallbackPool, topo.Groups[l], "spilled label "+l); err != nil {
				return nil, nil, err
			}
		default: // emptyPoolPark
			parked = append(parked, topo.Groups[l]...)
		}
	}

	// I8: every active worker appears exactly once — explicit empty
	// entries for workers in no pool and for unknown-label workers.
	for _, w := range topo.AllWorkers {
		if _, ok := merged[w]; !ok {
			merged[w] = []types.Partition{}
		}
	}
	for w := range topo.UnknownSet {
		merged[w] = []types.Partition{}
	}

	return merged, parked, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/assignment/ -run 'TestBuildLabelTopology|TestComputeLabelAssignments' -v -race`
Expected: PASS (7 tests).

Watch one subtlety in the I1 golden test: strategies return entries for
every worker they were given, so the merged result for the label-free
case is exactly the strategy's own map — `require.Equal` on the full map
is intentionally strict (nil-vs-empty slice differences will fail; make
`assignGroup`'s merge preserve the strategy's slices untouched for this
to hold).

- [ ] **Step 5: Lint + commit**

```bash
make lint
git add internal/assignment/labels.go internal/assignment/labels_test.go
git commit -m "feat(assignment): label pool topology and per-pool assignment merge"
```

---

### Task 7: Wire additions — payload labels-of-record + parked commit contract

Everything the leader writes to KV (spec §4.3 + §8.2): labels-of-record on
the per-worker payload (with presence bit), parked metadata on the commit,
and the widened coverage check. No calculator behavior changes yet — the
new inputs default to empty and preserve today's semantics bit-for-bit
until Task 8 populates them.

**Files:**
- Modify: `types/assignment_commit.go` — `AssignmentPayload` (:25-34),
  `AssignmentCommit` (add after `BatchDigest`, :111-114)
- Modify: `types/partition.go` — `Assignment` struct (:218-255)
- Modify: `internal/assignment/assignment_publisher.go` — `PublishInput`
  (:271-310), `checkCoverage` (:521+), payload construction inside
  `writePayloads`, commit literal (:405-418), `buildLegacyAlias`
  (:1327-1341)
- Test: `internal/assignment/assignment_publisher_labels_test.go` (new;
  follow the harness of `assignment_publisher_test.go`)

**Interfaces:**
- Consumes: Task 1.
- Produces (locked; Tasks 8/10/11 depend on exact names):

```go
// types/assignment_commit.go
type AssignmentPayload struct {
	SchemaVersion uint8             `json:"schema_version"`
	Partitions    []Partition       `json:"partitions"`
	// WorkerLabels is the labels-of-record: the label set the leader read
	// from this worker's heartbeat when computing the assignment. Part of
	// the canonical payload bytes (and therefore PayloadHash).
	WorkerLabels []string `json:"worker_labels,omitempty"`
	// WorkerLabelsKnown distinguishes "computed for an unlabeled worker"
	// (true + empty WorkerLabels) from "computed by a pre-label leader"
	// (false). Label-aware leaders ALWAYS set true. Mirrors the
	// SourceRevisionKnown presence-bit precedent.
	WorkerLabelsKnown bool `json:"worker_labels_known,omitempty"`
}

// types/assignment_commit.go — AssignmentCommit additions
	// ParkedCount is the number of partitions intentionally left
	// unassigned in this batch (label pool empty, spill grace not yet
	// expired). Zero when nothing is parked.
	ParkedCount int `json:"parked_count,omitempty"`
	// ParkedDigest is types.PartitionSetDigest over the parked set.
	// Zero when nothing is parked.
	ParkedDigest uint64 `json:"parked_digest,omitempty"`

// types/partition.go — Assignment additions (runtime + legacy alias wire)
	// WorkerLabels / WorkerLabelsKnown mirror the fetched
	// AssignmentPayload's labels-of-record so the worker-side
	// stale-incarnation guard can compare against its own label set.
	WorkerLabels      []string `json:"worker_labels,omitempty"`
	WorkerLabelsKnown bool     `json:"worker_labels_known,omitempty"`

// internal/assignment — PublishInput additions
	// ParkedPartitions is the set deliberately left unassigned this batch.
	// Coverage becomes: assigned ∪ parked == source AND assigned ∩ parked == ∅.
	ParkedPartitions []types.Partition
	// WorkerLabels maps workerID → labels-of-record for payload stamping.
	// Workers absent from the map get WorkerLabels=nil (still Known=true).
	WorkerLabels map[string][]string
```

- [ ] **Step 1: Write the failing tests**

```go
// internal/assignment/assignment_publisher_labels_test.go
package assignment

// Harness note: copy the publisher construction from
// assignment_publisher_test.go (embedded NATS KV + NewAssignmentPublisher
// with test logger/metrics). The assertions below are the contract.

func TestCheckCoverage_ParkedUnionAndDisjointness(t *testing.T) {
	t.Parallel()
	p := newTestPublisher(t) // existing helper or minimal local ctor

	src := []types.Partition{
		{Keys: []string{"a"}}, {Keys: []string{"b"}, Label: "vip"},
	}
	assignedOnly := map[string][]types.Partition{"w0": {src[0]}}

	// (1) parked partition completes coverage:
	require.NoError(t, p.checkCoverage(src, assignedOnly, []types.Partition{src[1]}))

	// (2) partition missing from BOTH assignment and parked → error:
	err := p.checkCoverage(src, assignedOnly, nil)
	require.ErrorIs(t, err, types.ErrCoverageMismatch)

	// (3) partition in BOTH assignment and parked → error:
	both := map[string][]types.Partition{"w0": {src[0], src[1]}}
	err = p.checkCoverage(src, both, []types.Partition{src[1]})
	require.ErrorIs(t, err, types.ErrCoverageMismatch)

	// (4) legacy shape (no parked) unchanged:
	full := map[string][]types.Partition{"w0": {src[0]}, "w1": {src[1]}}
	require.NoError(t, p.checkCoverage(src, full, nil))
}

func TestPublish_ParkedMetadataOnCommit(t *testing.T) {
	t.Parallel()
	// Publish with one parked partition; read back "assignment._commit";
	// require commit.ParkedCount == 1 and commit.ParkedDigest ==
	// types.PartitionSetDigest(parked). BatchDigest must still cover the
	// FULL source set (compare against a second publish without parking).
}

func TestPublish_PayloadCarriesLabelsOfRecord(t *testing.T) {
	t.Parallel()
	// Publish with WorkerLabels{"w0": {"vip"}} and one with nil labels for
	// w1. Fetch both payloads via their refs; require:
	//   payload(w0).WorkerLabels == ["vip"], WorkerLabelsKnown == true
	//   payload(w1).WorkerLabels == nil,     WorkerLabelsKnown == true
	// And PayloadHash(w0-with-labels) != PayloadHash(w0-without-labels):
	// labels-of-record are part of content identity.
}

func TestBuildLegacyAlias_CopiesLabelsOfRecord(t *testing.T) {
	t.Parallel()
	payload := types.AssignmentPayload{
		SchemaVersion: 1,
		Partitions:    []types.Partition{{Keys: []string{"a"}}},
		WorkerLabels:  []string{"vip"},
		WorkerLabelsKnown: true,
	}
	alias := buildLegacyAlias(payload, 7, 3, 0, "steady", 2)
	require.Equal(t, []string{"vip"}, alias.WorkerLabels)
	require.True(t, alias.WorkerLabelsKnown)
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/assignment/ -run 'TestCheckCoverage_Parked|TestPublish_Parked|TestPublish_Payload|TestBuildLegacyAlias_Copies' -v`
Expected: compile errors (fields/params missing).

- [ ] **Step 3: Implement**

1. Add the three type extensions exactly as in the interface block.
2. `checkCoverage(source []types.Partition, assignments map[string][]types.Partition, parked []types.Partition) error`:

```go
func (p *AssignmentPublisher) checkCoverage(source []types.Partition, assignments map[string][]types.Partition, parked []types.Partition) error {
	union := unionPartitions(assignments)
	assignedIDs := canonicalIDSet(union) // sorted, deduped
	parkedIDs := canonicalIDSet(parked)
	expected := canonicalIDSet(source)

	// Disjointness: a partition may not be both assigned and parked.
	overlap := intersectSortedIDs(assignedIDs, parkedIDs) // new tiny helper
	// Union: assigned ∪ parked must equal source as a set, and the raw
	// counts must match (multiset check catches duplicates).
	got := mergeSortedIDs(assignedIDs, parkedIDs) // new tiny helper, dedupes
	rawCount := len(union) + len(parked)
	setOK := equalStringSlices(got, expected)
	multisetOK := rawCount == len(expected)
	if setOK && multisetOK && len(overlap) == 0 {
		return nil
	}
	// ... keep the existing missing/extra/duplicates diagnostics, add
	// "parked" and "overlap" counts to the log line and error, and return
	// types.ErrCoverageMismatch as before.
}
```

Update the single call site (`Publish` step 3) to
`p.checkCoverage(in.SourcePartitions, in.Assignments, in.ParkedPartitions)`.

3. Payload stamping: `writePayloads` gains the labels map. Change its
   signature to accept `in PublishInput` (or add a
   `labels map[string][]string` parameter — pick whichever matches the
   existing style; there is exactly one caller). At the single site where
   `types.AssignmentPayload{...}` is constructed for worker `w`, set:

```go
	WorkerLabels:      in.WorkerLabels[w],
	WorkerLabelsKnown: true,
```

   Note `WorkerLabelsKnown: true` is UNCONDITIONAL — a label-aware leader
   always records presence, including for unlabeled workers (spec §4.3).

4. Commit literal (step 8 of `Publish`): add

```go
	ParkedCount:  len(in.ParkedPartitions),
	ParkedDigest: types.PartitionSetDigest(in.ParkedPartitions),
```

5. `buildLegacyAlias`: add to the returned `types.Assignment`:

```go
	WorkerLabels:      payload.WorkerLabels,
	WorkerLabelsKnown: payload.WorkerLabelsKnown,
```

- [ ] **Step 4: Run new + existing publisher tests**

Run: `go test ./internal/assignment/ -run 'TestCheckCoverage|TestPublish|TestBuildLegacyAlias' -race`
Expected: PASS. Existing coverage tests keep passing because a nil parked
set degenerates to the old check exactly.

One expected ripple: any existing publisher test asserting exact payload
bytes/hashes now sees `worker_labels_known:true` in canonical bytes.
Update those fixtures — this is the one-time fleet-wide payload re-hash
the spec accepts (§4.3 compat note), not a regression.

- [ ] **Step 5: Lint + commit**

```bash
make lint
git add types/assignment_commit.go types/partition.go internal/assignment/assignment_publisher.go internal/assignment/assignment_publisher_labels_test.go
git commit -m "feat(assignment): labels-of-record on payloads and parked-partition commit metadata"
```

---

### Task 8: Calculator — label reads, confirmation state, pipeline wiring

Replaces the single strategy call with the label pipeline (spec §6, §7,
§8.5). After this task the leader assigns by label end-to-end.

**Files:**
- Create: `internal/assignment/labels_state.go`
- Modify: `internal/assignment/calculator.go` — struct fields (near the
  other mu-guarded maps), `rebalance` (:1646-1811: after `snapshotSource`,
  replacing the `c.Strategy.Assign` call at :1707 and extending the
  `PublishInput` literal at :1777), `handleRebalance` sentinel list
  (:1505-1525)
- Modify: `internal/assignment/calculator.go` — `NewCalculatorWithConfig`
  defaults for the two new config fields
- Test: `internal/assignment/labels_state_test.go` (new),
  `internal/assignment/calculator_labels_test.go` (new)

**Interfaces:**
- Consumes: Tasks 5, 6, 7 (`GetHeartbeatsFor`, topology/compute, publisher
  inputs), config fields (Task 4).
- Produces:
  - `errLabelObservationDeferred` — benign-abort sentinel
  - `errLabelReadBroadFailure` — broad heartbeat-read failure abort
  - `(c *Calculator) readWorkerLabels(ctx, workers []string) (labels map[string][]string, unknown map[string]bool, err error)`
  - `(c *Calculator) decideEmptyPoolActions(topo labelTopology, unknown map[string]bool) (map[string]emptyPoolAction, error)`
  - `(c *Calculator) commitLabelObservation(topo labelTopology, parkedCount int)` — post-publish state advance
  - Task 9 consumes: `c.labelState` internals for timer arming.

- [ ] **Step 1: Create the state container + decision logic (failing tests first)**

```go
// internal/assignment/labels_state_test.go — core state-machine tests,
// all with an injected clock (Config.Now).
package assignment

func newLabelStateForTest(now func() time.Time, grace time.Duration) *labelState {
	return newLabelState(grace, now)
}

func TestLabelState_DeferOnceThenPark(t *testing.T) {
	t.Parallel()

	now := time.Now()
	clock := func() time.Time { return now }
	st := newLabelStateForTest(clock, time.Minute)

	// First observation of an empty pool for "vip": defer (spec §8.5).
	act, deferred := st.observeEmptyPools([]string{"vip"})
	require.True(t, deferred)
	require.Empty(t, act)

	// Second consecutive observation: act — inside grace ⇒ park.
	act, deferred = st.observeEmptyPools([]string{"vip"})
	require.False(t, deferred)
	require.Equal(t, emptyPoolPark, act["vip"])

	// emptySince started at the FIRST observation: advancing the clock
	// past grace-from-first flips to spill (confirmation does not extend
	// the grace window).
	now = now.Add(61 * time.Second)
	act, deferred = st.observeEmptyPools([]string{"vip"})
	require.False(t, deferred)
	require.Equal(t, emptyPoolSpill, act["vip"])
}

func TestLabelState_NonEmptyResets(t *testing.T) {
	t.Parallel()

	now := time.Now()
	st := newLabelStateForTest(func() time.Time { return now }, time.Minute)

	_, _ = st.observeEmptyPools([]string{"vip"})
	st.observeNonEmpty([]string{"vip"}) // pool recovered
	_, deferred := st.observeEmptyPools([]string{"vip"})
	require.True(t, deferred, "recovery resets the confirmation streak AND emptySince")
}

func TestLabelState_PruneRemovedLabels(t *testing.T) {
	t.Parallel()

	now := time.Now()
	st := newLabelStateForTest(func() time.Time { return now }, time.Minute)
	_, _ = st.observeEmptyPools([]string{"vip"})
	st.prune(map[string]bool{}) // "vip" no longer in the snapshot
	require.Empty(t, st.emptySince, "stale grace clocks must not leak")
}

func TestLabelState_ZeroGraceSpillsImmediatelyAfterConfirmation(t *testing.T) {
	t.Parallel()

	now := time.Now()
	st := newLabelStateForTest(func() time.Time { return now }, 0)
	_, deferred := st.observeEmptyPools([]string{"vip"})
	require.True(t, deferred, "confirmation still applies at grace=0")
	act, _ := st.observeEmptyPools([]string{"vip"})
	require.Equal(t, emptyPoolSpill, act["vip"], "grace 0 = spill as soon as confirmed")
}

func TestLabelState_UnknownWorkerDeferThenAct(t *testing.T) {
	t.Parallel()

	st := newLabelStateForTest(time.Now, time.Minute)
	require.True(t, st.observeUnknownWorkers([]string{"w2"}), "first: defer")
	require.False(t, st.observeUnknownWorkers([]string{"w2"}), "second consecutive: act")
	st.observeUnknownWorkers(nil) // successful read resets
	require.True(t, st.observeUnknownWorkers([]string{"w2"}), "reset after recovery")
}
```

- [ ] **Step 2: Implement `internal/assignment/labels_state.go`**

```go
package assignment

import (
	"errors"
	"sync"
	"time"
)

// errLabelObservationDeferred is the benign-abort sentinel for the first
// observation of a disruptive label condition (previously non-empty pool
// reads empty; worker labels unreadable). Deliberately distinct from the
// suspicious-observation sentinels: those are swallowed without
// label-aware re-arm; this one arms the label re-check timer.
var errLabelObservationDeferred = errors.New("label observation deferred pending confirmation")

// errLabelReadBroadFailure aborts a rebalance whose heartbeat label reads
// failed broadly (bucket/connectivity-class trouble or more than
// max(1, 10%) of workers unreadable). Never converted into label
// decisions: a broad failure must not empty-assign the fleet.
var errLabelReadBroadFailure = errors.New("worker label read failed broadly")

// labelState tracks per-label grace clocks and defer-once confirmation
// streaks (spec §8.5). All methods are called from the rebalance path
// (serialized by rebalanceMu) but the mutex keeps the timer path (Task 9)
// safe when it inspects remaining grace.
type labelState struct {
	mu    sync.Mutex
	grace time.Duration
	now   func() time.Time

	emptySince    map[string]time.Time // label → first empty observation
	emptyStreak   map[string]int       // label → consecutive empty observations
	unknownStreak map[string]int       // workerID → consecutive unreadable-label observations
}

func newLabelState(grace time.Duration, now func() time.Time) *labelState {
	if now == nil {
		now = time.Now
	}
	return &labelState{
		grace:         grace,
		now:           now,
		emptySince:    map[string]time.Time{},
		emptyStreak:   map[string]int{},
		unknownStreak: map[string]int{},
	}
}

// observeEmptyPools records this rebalance's empty-pool set and returns
// the action per confirmed-empty label. deferred=true means at least one
// label is on its FIRST empty observation — the caller aborts the
// rebalance with errLabelObservationDeferred and arms the re-check timer.
// emptySince always starts at the first observation so the deferral does
// not extend the effective grace window.
func (s *labelState) observeEmptyPools(empty []string) (map[string]emptyPoolAction, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	deferred := false
	actions := make(map[string]emptyPoolAction, len(empty))
	for _, l := range empty {
		if _, ok := s.emptySince[l]; !ok {
			s.emptySince[l] = now
		}
		s.emptyStreak[l]++
		if s.emptyStreak[l] < 2 {
			deferred = true
			continue
		}
		if now.Sub(s.emptySince[l]) < s.grace {
			actions[l] = emptyPoolPark
		} else {
			actions[l] = emptyPoolSpill
		}
	}

	return actions, deferred
}

// observeNonEmpty resets streak and grace clock for recovered pools.
func (s *labelState) observeNonEmpty(labels []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range labels {
		delete(s.emptySince, l)
		delete(s.emptyStreak, l)
	}
}

// observeUnknownWorkers implements defer-once for unreadable labels.
// Returns true when at least one worker is on its first unreadable
// observation (caller defers). Passing the empty set resets everything
// (a fully successful read).
func (s *labelState) observeUnknownWorkers(unknown []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(unknown) == 0 {
		clear(s.unknownStreak)
		return false
	}
	current := make(map[string]bool, len(unknown))
	deferred := false
	for _, w := range unknown {
		current[w] = true
		s.unknownStreak[w]++
		if s.unknownStreak[w] < 2 {
			deferred = true
		}
	}
	// Workers that recovered reset their streak.
	for w := range s.unknownStreak {
		if !current[w] {
			delete(s.unknownStreak, w)
		}
	}

	return deferred
}

// prune drops state for labels absent from the current snapshot.
func (s *labelState) prune(currentLabels map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for l := range s.emptySince {
		if !currentLabels[l] {
			delete(s.emptySince, l)
			delete(s.emptyStreak, l)
		}
	}
}

// minRemainingGrace returns the shortest time until an emptySince clock
// crosses grace, and whether any clock is running. The re-check timer
// (Task 9) arms with this value after a rebalance that parked anything.
func (s *labelState) minRemainingGrace() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	found := false
	var minLeft time.Duration
	now := s.now()
	for _, since := range s.emptySince {
		left := s.grace - now.Sub(since)
		if left < 0 {
			left = 0
		}
		if !found || left < minLeft {
			minLeft, found = left, true
		}
	}

	return minLeft, found
}
```

- [ ] **Step 3: Run the state tests**

Run: `go test ./internal/assignment/ -run 'TestLabelState' -v -race`
Expected: PASS.

- [ ] **Step 4: Wire into the rebalance path (failing test first)**

Test (in `calculator_labels_test.go`, using the package's existing
calculator test harness — embedded NATS, `NewCalculatorWithConfig`, fake
heartbeats seeded into the heartbeat bucket as JSON):

```go
// TestRebalance_LabelRouting_EndToEnd: 2 workers (w0 labels=[vip],
// w1 unlabeled) with live heartbeats, source = 1 vip + 1 plain partition.
// Trigger a rebalance; read the published commit + payloads; assert:
//   - w0's payload contains exactly the vip partition, WorkerLabels=[vip],
//     WorkerLabelsKnown=true
//   - w1's payload contains exactly the plain partition
//   - commit.ParkedCount == 0
//
// TestRebalance_EmptyPool_DeferThenPark: source has a "ghost"-labeled
// partition with no matching worker. First rebalance attempt returns
// errLabelObservationDeferred (assert via handleRebalance treating it as
// benign nil + no publish happened). Second attempt publishes with
// commit.ParkedCount == 1 and the ghost partition in no payload.
```

- [ ] **Step 5: Implement the wiring**

Calculator struct additions:

```go
	// labelState carries per-label grace clocks and defer-once streaks.
	labelState *labelState
```

Init in `NewCalculatorWithConfig`:
`c.labelState = newLabelState(cfg.LabelSpillGrace, cfg.Now)` (after the
existing Now-defaulting; policy string defaulted to "dedicated" when
empty).

`readWorkerLabels` (in calculator.go, near getActiveWorkers):

```go
// readWorkerLabels fetches labels-of-record for the rebalance worker set
// (spec §6). One bounded retry for missing workers; then the taxonomy:
// more than max(1, 10%) unreadable ⇒ errLabelReadBroadFailure (broad
// failures must never become label decisions). Legacy heartbeats decode
// with nil labels — that is a SUCCESSFUL read of an empty set.
func (c *Calculator) readWorkerLabels(ctx context.Context, workers []string) (map[string][]string, map[string]bool, error) {
	hbs, fails, err := c.monitor.GetHeartbeatsFor(ctx, workers)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errLabelReadBroadFailure, err)
	}
	missing := missingWorkers(workers, hbs)
	if len(missing) > 0 { // one inline retry for the stragglers
		retry, retryFails, rerr := c.monitor.GetHeartbeatsFor(ctx, missing)
		if rerr == nil {
			maps.Copy(hbs, retry)
			fails = retryFails // post-retry classification uses the fresh errors
		}
		missing = missingWorkers(workers, hbs)
	}

	// Error-CLASS taxonomy first (spec §6): a connectivity or
	// degrading-JetStream failure is broad by nature, regardless of how
	// many workers it hit — in a 1-worker fleet a count-based rule alone
	// would misclassify it as an isolated unknown and eventually
	// empty-assign the whole fleet. The count rule below only governs
	// UNCLASSIFIED failures (e.g. key-not-found on a live worker,
	// malformed payloads).
	for _, w := range missing {
		ferr := fails[w]
		if natsutil.IsConnectivityError(ferr) || natsutil.IsDegradingJetStreamError(ferr) {
			return nil, nil, fmt.Errorf("%w: worker %s: %w", errLabelReadBroadFailure, w, ferr)
		}
	}

	broadCap := max(1, len(workers)/10)
	if len(missing) > broadCap {
		return nil, nil, fmt.Errorf("%w: %d of %d workers unreadable", errLabelReadBroadFailure, len(missing), len(workers))
	}

	labels := make(map[string][]string, len(hbs))
	for w, hb := range hbs {
		labels[w] = hb.Labels
	}
	unknown := make(map[string]bool, len(missing))
	for _, w := range missing {
		unknown[w] = true
	}

	return labels, unknown, nil
}

// missingWorkers returns workers absent from the heartbeat map, sorted.
func missingWorkers(workers []string, hbs map[string]types.Heartbeat) []string {
	var out []string
	for _, w := range workers {
		if _, ok := hbs[w]; !ok {
			out = append(out, w)
		}
	}
	slices.Sort(out)

	return out
}
```

Compile-order note: this task calls `c.requestLabelRecheck(...)` and
`c.armLabelRecheckAfterRebalance(...)`, both owned by Task 9. Define BOTH
as logging no-op stubs in `labels_state.go` in THIS task so it compiles
and tests standalone:

```go
// requestLabelRecheck is completed by the label re-check machinery; the
// stub records intent so Task 8 is testable standalone.
func (c *Calculator) requestLabelRecheck(reason string) {
	c.Logger.Debug("label recheck requested", "reason", reason)
}

func (c *Calculator) armLabelRecheckAfterRebalance(parkedCount int) {}
```

(Task 9 replaces both bodies; its tests fail until it does.)

In `rebalance` (calculator.go), replace the block at :1706-1711:

```go
	// --- label pipeline (spec §7) ---
	labels, unknown, lerr := c.readWorkerLabels(ctx, workers)
	if lerr != nil {
		// Spec §6: broad label-read failures route to the manager's
		// KV-error/degraded machinery — the OnEnumerationError precedent
		// (config.go seam). Wire a new optional callback:
		//   assignment.Config.OnLabelReadBroadFailure func(err error)
		// which startCalculator (manager_assignment.go:116) connects to
		// the manager's KV-error recorder (the same m.recordKVError
		// routing heartbeat/election ops use — see kv_error_classify.go;
		// match its exact signature at the call site). Nil-safe: skip
		// when unwired (unit-test default).
		if c.OnLabelReadBroadFailure != nil {
			c.OnLabelReadBroadFailure(lerr)
		}
		c.Metrics.RecordRebalanceAttempt(lifecycle, false)
		return c.wrapStopErr(lerr)
	}

	unknownList := slices.Sorted(maps.Keys(unknown))
	unknownDeferred := c.labelState.observeUnknownWorkers(unknownList)

	topo := buildLabelTopology(topologyInput{
		Workers:    workers,
		Labels:     labels,
		Unknown:    unknown,
		Partitions: partitions,
		Policy:     c.UnlabeledPartitionPolicy,
	})

	nonEmpty := make([]string, 0, len(topo.SortedLabels))
	for _, l := range topo.SortedLabels {
		if len(topo.Pools[l]) > 0 {
			nonEmpty = append(nonEmpty, l)
		}
	}
	c.labelState.observeNonEmpty(nonEmpty)
	actions, emptyDeferred := c.labelState.observeEmptyPools(topo.EmptyLabels)

	currentLabels := make(map[string]bool, len(topo.SortedLabels))
	for _, l := range topo.SortedLabels {
		currentLabels[l] = true
	}
	c.labelState.prune(currentLabels)

	if unknownDeferred || emptyDeferred {
		c.requestLabelRecheck("observation_deferred") // Task 9; arms timer + pending flag
		c.Metrics.RecordRebalanceDuration(time.Since(start).Seconds(), lifecycle)
		c.Metrics.RecordRebalanceAttempt(lifecycle, true)
		return errLabelObservationDeferred
	}

	assignments, parked, err := computeLabelAssignments(c.Strategy, topo, actions)
	if err != nil {
		c.Metrics.RecordRebalanceAttempt(lifecycle, false)
		return c.wrapStopErr(fmt.Errorf("assignment calculation failed: %w", err))
	}
```

Orphan gauge (the block currently at :1717-1725) compares against the
ELIGIBLE count:

```go
	eligible := len(partitions) - len(parked)
	if assignedCount != eligible {
		c.Metrics.RecordOrphanedPartitions(eligible - assignedCount)
	} else {
		c.Metrics.RecordOrphanedPartitions(0)
	}
```

`PublishInput` literal (:1777) gains:

```go
		ParkedPartitions:    parked,
		WorkerLabels:        labels,
```

After the publish succeeds (next to the existing tracking-state update),
arm/disarm the re-check timer for parked grace (Task 9 provides the
function; until Task 9 lands, leave a call to a no-op
`c.armLabelRecheckAfterRebalance(len(parked))` defined in
labels_state.go as `func (c *Calculator) armLabelRecheckAfterRebalance(parkedCount int) {}`
so this task compiles and Task 9 replaces the body).

`handleRebalance` (:1505-1525) — add the benign case:

```go
	// A deferred label observation is an explicit "confirm before
	// acting" decision; the label re-check timer re-fires it. Benign.
	if errors.Is(err, errLabelObservationDeferred) {
		return nil
	}
```

Do the same in `triggerPartitionRebalance`'s error handling: treat
`errLabelObservationDeferred` like the suppressed-observation case
(re-arm `pendingPartitionUpdate` is NOT needed — the label re-check timer
owns the retry; just convert to nil).

- [ ] **Step 6: Taxonomy tests (spec §14 small-fleet pins)**

Add to `calculator_labels_test.go`, using the real heartbeat bucket
(stage failures by simply NOT writing heartbeat values for the targeted
workers while keeping them in the `workers` argument):

```go
// TestReadWorkerLabels_SmallFleetTaxonomy:
//   3 workers, heartbeat values present for w0,w1 only:
//     → labels for w0,w1; unknown == {w2}; err == nil   (1-of-3 isolated)
//   2 workers, value present for w0 only:
//     → unknown == {w1}; err == nil                     (1-of-2 isolated: max(1,10%)=1)
//   3 workers, values present for w0 only:
//     → err wraps errLabelReadBroadFailure              (2-of-3 broad)
//   1 worker whose Get fails with a CONNECTIVITY-classed error (stage via
//   a canceled/closed connection or a fake KV returning nats.ErrTimeout):
//     → err wraps errLabelReadBroadFailure              (class beats count:
//       the 1-worker fleet must NOT treat this as isolated unknown)
//   broad failure must abort BEFORE any label decision: assert no commit
//   was published, labelState streaks were not advanced, and
//   OnLabelReadBroadFailure fired exactly once with an error that the
//   MANAGER-side adapter can route (the count-based case is admitted via
//   the ErrKVUnavailable wrap — see the Task 4 adapter and
//   TestRecordLabelReadFailure_Routing, which prove the degraded circuit
//   observes it; this test only pins the calculator half).
```

- [ ] **Step 7: Run the calculator tests**

Run: `go test ./internal/assignment/ -race`
Expected: all green — including every pre-existing calculator test (the
I1 golden path: no labels anywhere ⇒ identical assignments; any failure
here means the pipeline broke legacy behavior — stop and fix).

- [ ] **Step 8: Lint + commit**

```bash
make lint
git add internal/assignment/
git commit -m "feat(assignment): label-aware rebalance pipeline with park and spill"
```

---

### Task 9: Label re-check timer + `requestLabelRecheck`

The guaranteed second observation (spec §8.3): grace expiry and deferral
confirmation must re-fire without any external event, surviving busy
states.

**Files:**
- Modify: `internal/assignment/labels_state.go` (calculator-side methods)
- Modify: `internal/assignment/calculator.go` — fields, `Start` (launch
  the goroutine next to `monitorPartitions`), `Stop` path, lifecycle
  constants next to `lifecyclePartitionUpdate` (:640-644)
- Test: `internal/assignment/labels_recheck_test.go` (new)

**Interfaces:**
- Consumes: Task 8 state; `stateMach.TryClaimRebalancing` /
  `RunClaimedRebalanceErr` (same primitives as `triggerPartitionRebalance`,
  calculator.go:823-846); `ctxFromStopCh`.
- Produces: `requestLabelRecheck(reason string)` — also consumed by the
  worker monitor wiring (Task 11).

Design (mirrors `monitorPartitions` exactly):

```go
const (
	lifecycleLabelRecheck = "label_recheck"
	lifecycleLabelChange  = "label_change"
)

// Calculator fields:
	pendingLabelRecheck atomic.Bool
	labelRecheckCh      chan struct{} // capacity 1; timer + external signals coalesce
```

- `requestLabelRecheck(reason)`: replace Task 8's stub — set
  `pendingLabelRecheck=true`, log at Debug with reason, non-blocking send
  on `labelRecheckCh`. (Task 8 left `requestLabelRecheck` and
  `armLabelRecheckAfterRebalance` as no-op stubs; this task supplies the
  real bodies.)
- `armLabelRecheckAfterRebalance(parkedCount)`: replace Task 8's no-op —
  if `parkedCount > 0`, compute `labelState.minRemainingGrace()` and
  `time.AfterFunc(left+50ms, func() { c.requestLabelRecheck("grace_expiry") })`,
  storing the timer so Stop and re-arm cancel the previous one; if
  nothing parked and nothing pending, cancel any armed timer.
- `monitorLabelRecheck(ctx)` goroutine (started unconditionally in the
  same place `monitorPartitions` starts; guarded by `stopCh`):

```go
func (c *Calculator) monitorLabelRecheck(ctx context.Context) {
	drainTick := time.NewTicker(c.RebalanceGraceDrainInterval)
	defer drainTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-c.labelRecheckCh:
		case <-drainTick.C:
		}
		if !c.pendingLabelRecheck.CompareAndSwap(true, false) {
			continue
		}
		if c.inRecoveryGrace() {
			c.pendingLabelRecheck.Store(true) // retry next tick
			continue
		}
		if !c.stateMach.TryClaimRebalancing(context.Background(), lifecycleLabelRecheck) {
			c.pendingLabelRecheck.Store(true) // busy; sticky flag survives
			// Short retry so a deferral raised INSIDE a running rebalance
			// (claim still held when the wake arrives) confirms in ~2s
			// rather than waiting a full drain tick (spec §16 item 7:
			// re-check well under the drain cadence). Coalesced: the
			// channel has capacity 1.
			time.AfterFunc(2*time.Second, func() {
				select {
				case c.labelRecheckCh <- struct{}{}:
				default:
				}
			})
			continue
		}
		reqCtx, cancel := ctxFromStopCh(context.Background(), c.stopCh, partitionRebalanceRequestTimeout)
		err := c.stateMach.RunClaimedRebalanceErr(reqCtx, lifecycleLabelRecheck)
		cancel()
		if errors.Is(err, errLabelObservationDeferred) {
			// Deferral re-arms itself via requestLabelRecheck inside the
			// rebalance; nothing extra to do (flag already set again).
			continue
		}
		c.restorePendingOnGraceBail(errToPendingLabel(err, c))
	}
}
```

(`errToPendingLabel` = tiny adapter that restores
`pendingLabelRecheck=true` when the error is the recovery-grace bail —
reuse the `restorePendingOnGraceBail` pattern but for the label flag; a
straight copy with the label flag is fine and clearer than
generalizing.)

- [ ] **Step 1: Write the failing tests**

```go
// internal/assignment/labels_recheck_test.go
// Uses the package's existing calculator harness (embedded NATS).

// TestLabelRecheck_GhostLabelProgressesWithoutExternalEvents: the spec §14
// no-external-event progression pin. Source contains a "ghost"-labeled
// partition, no worker ever carries the label, grace = 500ms,
// RebalanceGraceDrainInterval = 200ms. Drive ONE partition-update
// rebalance (the label edit). Then, with no further worker/source events:
//   - within ~2s a commit appears with ParkedCount == 1  (defer → confirm → park)
//   - within ~2s after grace expiry a commit appears with ParkedCount == 0
//     and the ghost partition present in some worker's payload (spill)
// Poll the commit key with require.Eventually; no manual TriggerRebalance
// calls after the first.

// TestLabelRecheck_StickyUnderBusyStateMachine: hold the rebalancing
// claim (TryClaimRebalancing from the test), call requestLabelRecheck,
// assert pendingLabelRecheck stays true and no rebalance ran; release the
// claim; assert the drain tick picks it up (a rebalance runs within one
// RebalanceGraceDrainInterval).

// TestLabelRecheck_DisarmedOnStop: requestLabelRecheck, then Stop; no
// goroutine leak (the package's leak detector / doneCh join covers this)
// and no rebalance after Stop.
```

- [ ] **Step 2: Run to verify failure, then implement as designed above**

Run: `go test ./internal/assignment/ -run 'TestLabelRecheck' -v`
Expected: compile errors, then behavioral failures until the goroutine +
flag land.

- [ ] **Step 3: Full package + lint + commit**

Run: `go test ./internal/assignment/ -race && make lint`

```bash
git add internal/assignment/
git commit -m "feat(assignment): label re-check timer with sticky retry across busy states"
```

---

### Task 10: Worker-side stale-incarnation guard

Spec §9. A worker must never apply a payload whose labels-of-record
mismatch its own labels; rejection is a first-class outcome — no legacy
alias fallback, no apply retry, no ack.

**Files:**
- Modify: `manager_assignment.go` — `buildAssignmentFromCommit` return
  literal (:1212-1220), `applyAssignmentWithPrev` (:1434, guard at top),
  new sentinel + guard helper near it
- Modify: `manager.go` — `applyInitialAssignment` (:716 and :780 call
  sites convert the sentinel to success-without-apply)
- Test: `manager_label_guard_test.go` (new; root package unit tests
  following the style of `manager_apply_test.go`)

**Interfaces:**
- Consumes: `Assignment.WorkerLabels`/`WorkerLabelsKnown` (Task 7),
  `m.workerLabels` (Task 4).
- Produces: `errLabelIncarnationRejected` (manager-internal sentinel);
  guard semantics for Task 14's integration tests.

Key placement fact (verified): the apply paths converge on TWO sibling
entrypoints, not one — `applyAssignmentWithPrev` (watcher-driven commit
:1110 via applyAssignment :1274, legacy alias :663, both startup branches
manager.go:716/:780) AND `applyAssignmentWithPrevSkipJitter`
(:1454, used by the `scheduleApplyRetry` re-attempt path :1745), both of
which call `applyAssignmentWithPrevCore` directly. The guard is a shared
helper invoked at the head of BOTH wrappers, before either reaches Core
and before any failure handler — keeping `scheduleApplyRetry` out of the
reject path (retrying a payload that can never become applicable is the
futile loop the spec forbids). The retry entrypoint is defense in depth:
labels are immutable per process, so a stashed retry that matched at
stash time still matches — but the guard there costs one comparison and
makes the coverage claim true by construction.

- [ ] **Step 1: Write the failing tests**

```go
// manager_label_guard_test.go
package parti

// Construct managers the way manager_apply_test.go does (test-constructed
// Manager with workerLabels set directly; no NATS needed for the guard
// unit tests).

func TestLabelGuard_Matrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		workerLabels []string
		asg          Assignment
		rejected     bool
	}{
		{"known+equal applies", []string{"vip"},
			Assignment{WorkerLabels: []string{"vip"}, WorkerLabelsKnown: true}, false},
		{"known+mismatch rejects", nil,
			Assignment{WorkerLabels: []string{"vip"}, WorkerLabelsKnown: true}, true},
		{"known+empty vs labeled worker rejects", []string{"vip"},
			Assignment{WorkerLabelsKnown: true}, true},
		{"unknown (pre-label payload) applies", []string{"vip"},
			Assignment{}, false},
		{"known+empty vs unlabeled worker applies", nil,
			Assignment{WorkerLabelsKnown: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{workerLabels: tc.workerLabels}
			require.Equal(t, tc.rejected, m.labelIncarnationMismatch(tc.asg))
		})
	}
}

// TestLabelGuard_RejectIsTerminalNoRetryNoAck: build a minimal manager as
// manager_apply_test.go does (with a stub handoff coordinator + heartbeat
// snapshot recorder), feed a mismatched assignment through
// applyAssignmentWithPrev, and assert:
//   - the returned error Is errLabelIncarnationRejected
//   - the handoff coordinator's Apply was NEVER invoked
//   - the applied-snapshot (ack source) was NOT advanced
//   - no apply-retry was scheduled (the retry stash stays empty)
//
// TestLabelGuard_BuildAssignmentCopiesLabels: a commit whose payload has
// WorkerLabels=["vip"], WorkerLabelsKnown=true round-trips into the
// Assignment returned by buildAssignmentFromCommit.
//
// TestLabelGuard_RetryEntrypointAlsoGuarded: feed a mismatched assignment
// through applyAssignmentWithPrevSkipJitter (the scheduleApplyRetry
// entrypoint, manager_assignment.go:1454) and assert the same terminal
// reject: errLabelIncarnationRejected, no coordinator Apply, no ack, no
// re-scheduled retry.
```

- [ ] **Step 2: Run to verify failure**

Run: `go test . -run 'TestLabelGuard' -v`
Expected: compile errors (helper/sentinel missing).

- [ ] **Step 3: Implement**

Sentinel + guard (manager_assignment.go, near the apply pipeline):

```go
// errLabelIncarnationRejected marks an assignment payload computed for a
// different process incarnation behind this worker's stable ID (the
// labels-of-record mismatch this worker's configured labels). Rejection
// is terminal for that payload: no alias fallback, no apply retry, no
// ack — convergence arrives as the next label-correct commit via the
// leader's label-change trigger.
var errLabelIncarnationRejected = errors.New("assignment labels-of-record mismatch worker labels; payload from a different incarnation")

// labelIncarnationMismatch implements the spec §9 guard matrix. Both
// sides are sorted+deduplicated (normalizeWorkerLabels for the worker,
// publisher-normalized labels-of-record for the payload), so slice
// equality is set equality.
func (m *Manager) labelIncarnationMismatch(a Assignment) bool {
	if !a.WorkerLabelsKnown {
		return false // pre-label payload: compat apply
	}

	return !slices.Equal(a.WorkerLabels, m.workerLabels)
}
```

At the very top of BOTH `applyAssignmentWithPrev` AND
`applyAssignmentWithPrevSkipJitter` (before jitter, before
`applyAssignmentWithPrevCore`, before ANY side effect) — extract the
block below into a shared `rejectIfStaleIncarnation(newAssignment) error`
helper called from each:

```go
	if m.labelIncarnationMismatch(newAssignment) {
		m.logger.Warn("rejecting assignment computed for a different incarnation of this worker ID",
			"worker_id", m.WorkerID(),
			"payload_labels", newAssignment.WorkerLabels,
			"worker_labels", m.workerLabels,
			"version", newAssignment.Version)
		if lm, ok := m.metrics.(types.LabelMetrics); ok {
			lm.IncrementLabelIncarnationReject()
		}

		return errLabelIncarnationRejected
	}
```

`buildAssignmentFromCommit` return literal gains:

```go
		WorkerLabels:      payload.WorkerLabels,
		WorkerLabelsKnown: payload.WorkerLabelsKnown,
```

`applyInitialAssignment` (manager.go): both apply branches convert the
sentinel to success-without-apply — the worker stays in
`WaitingAssignment` and the next label-correct commit (delivered by the
existing watcher) applies normally:

```go
			if err := m.applyAssignmentWithPrev(Assignment{}, newAsg); err != nil {
				if errors.Is(err, errLabelIncarnationRejected) {
					m.logger.Warn("startup: current commit is for a different incarnation; waiting for a label-correct commit")
					return nil
				}
				return err
			}
```

and the same conversion around the alias-branch apply at manager.go:780.
Critically, the commit-path reject must NOT fall through to the legacy
alias branch: the guard returns from inside `applyAssignmentWithPrev`
after `buildAssignmentFromCommit` succeeded (`ok==true`), so the existing
`ok==false → alias fallback` route is untouched and never sees rejects.

- [ ] **Step 4: Run tests**

Run: `go test . -run 'TestLabelGuard|TestApply' -race`
Expected: new tests PASS; every existing apply-path test still green
(assignments without `WorkerLabelsKnown` hit the compat branch).

- [ ] **Step 5: Lint + commit**

```bash
make lint
git add manager_assignment.go manager.go manager_label_guard_test.go
git commit -m "feat: reject assignments computed for a different worker incarnation"
```

---

### Task 11: Label-change trigger — monitor fingerprints + calculator wiring

Spec §9 convergence: detect label changes behind live worker IDs from
heartbeat PUTs, level-triggered across watcher sessions, delivered via
`requestLabelRecheck` (never the worker-set change path, which no-ops on
an unchanged set at calculator.go:1052).

**Files:**
- Modify: `internal/assignment/worker_monitor.go` — monitor-lifetime
  fingerprint map + `SetOnLabelChange`; PUT/DELETE handling inside
  `processWatcherEvents` (:381-470)
- Modify: `internal/assignment/calculator.go` — wire
  `SetOnLabelChange` after the `NewWorkerMonitor` call (:269)
- Test: `internal/assignment/worker_monitor_label_test.go` (new)

**Interfaces:**
- Consumes: `requestLabelRecheck` (Task 9), `types.DecodeHeartbeat`.
- Produces: `SetOnLabelChange(fn func())` — invoked (coalesced by the
  caller) whenever a heartbeat PUT's labels differ from the retained
  fingerprint for that key.

Design:

```go
// Monitor fields:
	labelFPMu sync.Mutex
	labelFP   map[string]uint64 // heartbeat key → fingerprint of sorted labels; MONITOR lifetime (survives watcher sessions)
	onLabelChangeCb func()

// labelFingerprint hashes a sorted label set; 0 is reserved for "no labels".
func labelFingerprint(labels []string) uint64 {
	if len(labels) == 0 {
		return 0
	}
	var h xxh3.Hasher
	for i, l := range labels {
		if i > 0 {
			_, _ = h.WriteString("\n")
		}
		_, _ = h.WriteString(l)
	}
	fp := h.Sum64()
	if fp == 0 {
		fp = 1
	}
	return fp
}
```

In `processWatcherEvents`, inside the `jetstream.KeyValuePut` case (after
the existing lastSeen bookkeeping, BEFORE the suppression decision is
final):

```go
			// Label fingerprint: level-triggered across watcher sessions.
			// The map deliberately outlives this session — a takeover PUT
			// that lands while the watch is closed is caught when the next
			// session's initial replay delivers the key's current value
			// and it differs from the retained fingerprint.
			if hb, derr := types.DecodeHeartbeat(entry.Value()); derr == nil {
				fp := labelFingerprint(hb.Labels)
				m.labelFPMu.Lock()
				prev, seen := m.labelFP[key]
				m.labelFP[key] = fp
				m.labelFPMu.Unlock()
				if seen && prev != fp {
					m.logger.Info("worker label change detected", "key", key)
					trigger = true // a label change is never a suppressible refresh
					if m.onLabelChangeCb != nil {
						m.onLabelChangeCb()
					}
				}
			}
```

DELETE/PURGE case: also `delete(m.labelFP, entry.Key())` (a leave;
rejoin-with-new-labels is a join covered by the worker-change path; a
delete+rewrite missed entirely inside one watch gap replays as a PUT
whose value differs from... nothing — first-seen seeds silently, and the
join/leave key-set delta drives the worker-change rebalance instead).

`SetOnLabelChange(fn func())` — plain setter (call before `Start`, like
the constructor callback).

Calculator wiring (calculator.go, immediately after `c.monitor =
NewWorkerMonitor(...)` at :269):

```go
	c.monitor.SetOnLabelChange(func() {
		if lm, ok := c.Metrics.(types.LabelMetrics); ok {
			lm.IncrementLabelChangeTrigger()
		}
		c.requestLabelRecheck("label_change")
	})
```

- [ ] **Step 1: Write the failing tests**

```go
// internal/assignment/worker_monitor_label_test.go
// Harness: embedded NATS heartbeat bucket like the sibling monitor tests.

// TestWorkerMonitor_LabelChangeEscapesSuppression: start the monitor with
// an onChange callback counter AND SetOnLabelChange counter. Publish
// heartbeats for worker-0 with labels ["vip"] every 200ms (hbTTL 5s) —
// after the first, refreshes are suppressed (assert onChange does not
// grow). Then publish the SAME key with labels ["batch"]:
//   - onLabelChange fires within the watcher latency (require.Eventually)
//   - a subsequent identical-labels beat does NOT re-fire (fingerprint
//     updated)
//
// TestWorkerMonitor_LabelChangeAcrossWatcherRestart: publish worker-0
// with ["vip"]; wait for the fingerprint to seed (onLabelChange quiet).
// Stop the watcher session (monitor test hook: kill the watcher via
// m.stopWatcher(); the retry loop restarts it). While the watch is down,
// publish worker-0 with ["batch"]. After the session re-establishes and
// replays, onLabelChange must fire exactly once — the retained-
// fingerprint-vs-replay comparison (spec §9, watcher-restart edge).
//
// TestWorkerMonitor_MalformedPayloadNoFingerprintChurn: a malformed
// heartbeat PUT neither fires onLabelChange nor erases the retained
// fingerprint (the next well-formed identical-labels beat stays quiet).
//
// TestWorkerMonitor_FirstSeenSeedsSilently: a brand-new worker key with
// labels never fires onLabelChange (joins are the worker-change path's
// job).
```

- [ ] **Step 2: Run to verify failure, implement per the design block, re-run**

Run: `go test ./internal/assignment/ -run 'TestWorkerMonitor_Label' -v -race`
Expected: fail (setter missing) → PASS after implementation.

- [ ] **Step 3: End-to-end wiring test (calculator level)**

Extend `calculator_labels_test.go`:

```go
// TestCalculator_TightTakeover_LabelChangeTriggersRebalance: real
// calculator + heartbeat bucket. worker-0 heartbeats with ["vip"], one
// vip partition assigned to it (initial rebalance). Now simulate a tight
// takeover: keep the SAME heartbeat key alive but switch its payload to
// labels [] (a different process incarnation; the key never lapses so
// the worker SET never changes). Assert via require.Eventually that a
// NEW commit is published in which the vip partition is NOT assigned to
// worker-0 (parked or reassigned) — proving the label-change trigger
// bypasses the unchanged-worker-set short-circuit end to end.
```

- [ ] **Step 4: Run full package, lint, commit**

Run: `go test ./internal/assignment/ -race && make lint`

```bash
git add internal/assignment/
git commit -m "feat(assignment): detect worker label changes from heartbeats and trigger rebalance"
```

---

### Task 12: `LabelMetrics` extension interface + lifecycle

Spec §13. Optional interface (type-asserted) so existing
`MetricsCollector` implementors don't break.

**Files:**
- Modify: `types/metrics_collector.go` — add the interface (verbatim from
  the plan header block) with Godoc stating lifecycle semantics
- Modify: `internal/metrics/nop.go` — nop implements it
- Modify: `internal/assignment/calculator.go` — record pool sizes /
  parked counts / spill / fallback after each successful publish; zero
  gauges for labels that left the snapshot
- Test: `internal/assignment/calculator_label_metrics_test.go` (new)

**Interfaces:**
- Consumes: Tasks 8/9/10/11 record points (`IncrementLabelChangeTrigger`
  and `IncrementLabelIncarnationReject` were wired there behind the same
  type assertion).
- Produces: `types.LabelMetrics` — documented contract: per-label gauges
  recomputed on every completed rebalance; a label absent from the
  current snapshot is explicitly zeroed in the same pass; counters
  monotonic per process.

- [ ] **Step 1: Failing test**

```go
// calculator_label_metrics_test.go — fakeLabelMetrics records calls.
// Drive two rebalances through the calculator harness:
//   1) source has vip partition + vip worker → RecordLabelPoolSize("vip",1),
//      RecordParkedPartitions("vip",0)
//   2) source rewritten WITHOUT any vip partition → the same pass emits
//      RecordLabelPoolSize("vip",0) AND RecordParkedPartitions("vip",0)
//      (zeroed, not leaked), and no further "vip" records afterwards.
```

- [ ] **Step 2: Implement**

Calculator keeps `prevMetricLabels map[string]bool`; after each successful
publish:

```go
	if lm, ok := c.Metrics.(types.LabelMetrics); ok {
		current := map[string]bool{}
		for _, l := range topo.SortedLabels {
			current[l] = true
			lm.RecordLabelPoolSize(l, len(topo.Pools[l]))
			lm.RecordParkedPartitions(l, parkedCountByLabel[l])
		}
		for l := range c.prevMetricLabels {
			if !current[l] {
				lm.RecordLabelPoolSize(l, 0)
				lm.RecordParkedPartitions(l, 0)
			}
		}
		c.prevMetricLabels = current
	}
```

(`parkedCountByLabel` is derived from the parked slice right here:
`for _, p := range parked { parkedCountByLabel[p.Label]++ }` — parked
partitions always carry their label. Spill actions call
`lm.IncrementLabelSpill(l)` where the action map is applied; the
unlabeled-group ladder falling through to AllWorkers calls
`lm.IncrementUnlabeledFallback()`.)

Nop implementation in `internal/metrics/nop.go` mirrors the existing nop
method style.

- [ ] **Step 3: Run, lint, commit**

Run: `go test ./internal/assignment/ ./types/ ./internal/metrics/ -race && make lint`

```bash
git add types/metrics_collector.go internal/metrics/nop.go internal/assignment/
git commit -m "feat(metrics): label assignment observability with per-label gauge lifecycle"
```

---

### Task 13: Integration — label-only edit propagates end to end (priority)

The user-priority scenario: a **label-only rewrite (same keys, same
weights, new label) through the production update path** must flow KV
source watch → leader rebalance → new assignments on live managers.
Task 2 proved source-level propagation; this proves the whole system.

**Files:**
- Create: `test/integration/assignment/label_promotion_test.go`

**Interfaces:**
- Consumes: everything through Task 11; `testutil.WorkerCluster`,
  `AddWorkerWithOptions`, `source.NewNatsKV`, `partcodec`.

- [ ] **Step 1: Write the test**

```go
package assignment_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/partcodec"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// partitionsOf returns the CanonicalID set of a manager's current assignment.
func partitionsOf(m *parti.Manager) map[string]bool {
	out := map[string]bool{}
	for _, p := range m.CurrentAssignment().Partitions {
		out[p.CanonicalID()] = true
	}
	return out
}

func TestLabelPromotion_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	// Production-shaped source: partition list in a KV bucket.
	srcKV, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "label-e2e-partitions"})
	require.NoError(t, err)
	initial := []types.Partition{
		{Keys: []string{"p0"}, Weight: 1},
		{Keys: []string{"p1"}, Weight: 1},
		{Keys: []string{"p2"}, Weight: 1},
		{Keys: []string{"p3"}, Weight: 1},
	}
	src := source.NewNatsKV(srcKV, "partitions", nil)
	require.NoError(t, src.Update(ctx, initial)) // seeds the bucket
	require.NoError(t, src.Start(ctx))           // watch + reconcile loops
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	cfg := testutil.IntegrationTestConfig()
	cfg.LabelSpillGrace = 5 * time.Second

	// Same construction shape as manager_e2e_invariant_test.go (which
	// uses testutil.NewWorkerClusterWithSource — prefer that helper if
	// its config parameter fits; otherwise the literal below).
	cluster := &testutil.WorkerCluster{
		Config: cfg, Source: src, Strategy: strategy.NewConsistentHash(),
		NC: nc, JS: js, T: t,
	}
	vipWorker := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("vip"))
	plainWorker := cluster.AddWorkerWithOptions(ctx)
	defer cluster.StopWorkers()
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(20 * time.Second)

	p0 := types.Partition{Keys: []string{"p0"}}.CanonicalID()

	// Phase 0 — dedicated reservation: no labeled partitions yet, so the
	// vip worker idles and the plain worker owns everything (spec §7).
	require.Eventually(t, func() bool {
		return len(partitionsOf(plainWorker)) == 4 && len(partitionsOf(vipWorker)) == 0
	}, 20*time.Second, 200*time.Millisecond,
		"dedicated policy must reserve the labeled worker")

	// Phase 1 — PROMOTION: rewrite the full list with ONLY p0's label
	// changed (same keys, same weights), OUT-OF-BAND via a raw KV write.
	// CRITICAL: do NOT use src.Update here — NatsKV.Update refreshes its
	// local cache and notifies its own listeners directly (without the
	// KV watcher round trip), so a same-instance Update would let this
	// test pass even with watch-path propagation broken. A raw kv.Put is
	// exactly what an external operator/writer process does, and the
	// manager-owned source can only learn about it through its WATCHER —
	// which is the propagation path this test exists to pin.
	promoted := []types.Partition{
		{Keys: []string{"p0"}, Weight: 1, Label: "vip"}, // <- the only delta
		{Keys: []string{"p1"}, Weight: 1},
		{Keys: []string{"p2"}, Weight: 1},
		{Keys: []string{"p3"}, Weight: 1},
	}
	encoded, err := partcodec.Encode(promoted)
	require.NoError(t, err)
	_, err = srcKV.Put(ctx, "partitions", encoded)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return partitionsOf(vipWorker)[p0] && !partitionsOf(plainWorker)[p0]
	}, 30*time.Second, 200*time.Millisecond,
		"label-only promotion must move p0 to the vip worker: KV watch → rebalance → assignment")

	// Coverage invariant across the move: every partition owned exactly once.
	require.Eventually(t, func() bool {
		vip, plain := partitionsOf(vipWorker), partitionsOf(plainWorker)
		if len(vip)+len(plain) != 4 {
			return false
		}
		for id := range vip {
			if plain[id] {
				return false
			}
		}
		return true
	}, 20*time.Second, 200*time.Millisecond, "no orphan, no duplicate during promotion")

	// Phase 2 — DEMOTION: label-only rewrite back, same out-of-band path.
	// Under dedicated policy p0 must return to the plain worker and the
	// vip worker must drain to empty (its KV assignment updated, not
	// stale — see the stale check).
	encoded, err = partcodec.Encode(initial)
	require.NoError(t, err)
	_, err = srcKV.Put(ctx, "partitions", encoded)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return !partitionsOf(vipWorker)[p0] && partitionsOf(plainWorker)[p0] &&
			len(partitionsOf(vipWorker)) == 0
	}, 30*time.Second, 200*time.Millisecond,
		"label-only demotion must move p0 back and drain the reserved worker")
}
```

- [ ] **Step 2: Run it**

Run: `go test ./test/integration/assignment/ -run 'TestLabelPromotion_EndToEnd' -v -race -timeout 300s`
Expected: PASS. If Phase 1 times out, debug order: (1) Task 2 source
tests still green? (2) leader log shows "partition change detected"? (3)
commit content via `nats kv get` — is the label present in the payload?

- [ ] **Step 3: Commit**

```bash
make lint
git add test/integration/assignment/label_promotion_test.go
git commit -m "test(integration): runtime label promotion moves partitions across worker pools"
```

---

### Task 14: Integration — orphan and stale-assignment regressions

Three suites pinning spec I2/I8: nothing is ever silently unassigned, and
no worker's KV assignment goes stale when its pool drains.

**Files:**
- Create: `test/integration/assignment/label_orphan_stale_test.go`

- [ ] **Step 1: Write the stale-assignment (I8) test**

```go
// TestLabelDemotion_NoStaleAssignmentInKV: same 2-worker setup as Task 13
// (vip + plain, dedicated). Promote p0 to vip; wait until the vip worker
// owns it. Demote. Then assert against the ASSIGNMENT KV BUCKET directly
// (not just CurrentAssignment): the commit's payload for the vip worker
// exists and contains ZERO partitions — the merge contract publishes an
// explicit empty entry rather than leaving the old vip payload dangling.
//
// Mechanics: read "assignment._commit" via kvutil.GetJSON[types.AssignmentCommit],
// find commit.Payloads[vipWorkerID], fetch it with
// assignment.FetchAndVerifyCommitPayload (exported helper used by the
// manager), and require len(payload.Partitions) == 0 while
// payload.WorkerLabelsKnown == true. Also require the vip worker's
// heartbeat eventually acks the new version (Heartbeat.AppliedVersion ==
// commit.Version) — the empty assignment was APPLIED, not ignored.
```

- [ ] **Step 2: Write the parked-coverage test**

```go
// TestGhostLabel_ParksThenSpills_NeverOrphans: 1 unlabeled worker,
// LabelSpillGrace = 6s. Source: 3 plain partitions + 1 partition labeled
// "ghost" (no worker will ever carry it).
//
// Phase park: require.Eventually a commit where ParkedCount == 1,
// ParkedDigest == PartitionSetDigest({ghost}), and the union of all
// payload partition sets == the 3 plain partitions (ghost in NO payload).
// Invariant at every observation: assigned ∪ parked == source (fetch all
// payloads, union with parked count, compare CanonicalID sets).
//
// Phase spill: after grace, require.Eventually a commit where
// ParkedCount == 0 and the ghost partition IS in some payload. The
// transition must happen with NO source/worker events — this pins the
// label re-check timer end to end against a live cluster.
//
// Phase recover: add a "ghost"-labeled worker
// (cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("ghost")) +
// start). require.Eventually the ghost partition moves to it (re-home).
```

- [ ] **Step 3: Write the pool-outage lifecycle test**

```go
// TestLabelPoolOutage_ParkSpillRehome: 2 vip workers + 1 plain worker,
// 2 vip partitions + 2 plain, LabelSpillGrace = 8s, dedicated policy.
//
//  1. Stable: vip partitions on vip workers only.
//  2. Stop ONE vip worker: its vip partitions concentrate onto the
//     surviving vip worker (no parking — the pool is non-empty; assert
//     ParkedCount stays 0 through this transition).
//  3. Stop the second vip worker: pool now empty. Within detection +
//     confirmation, a commit parks BOTH vip partitions (ParkedCount == 2)
//     and the plain worker does NOT receive them during grace.
//  4. After grace: spill — vip partitions appear in the plain worker's
//     payload, ParkedCount == 0.
//  5. Restart a vip worker: partitions re-home (plain worker's payload
//     sheds the vip partitions; ParkedCount stays 0).
//
// Every phase asserts the coverage invariant (assigned ∪ parked == source
// by CanonicalID sets) so any orphan window fails loudly. Timing: use
// require.Eventually with 30s windows; heartbeat TTL is 5s and detection
// + defer-once confirmation adds up to ~2 poll cycles.
```

- [ ] **Step 3b: No-ownership-movement regression (spec §4.1 pin)**

```go
// TestLabelOnlyEdit_NoConsumerChurnWhenPlacementUnchanged: 1 worker
// labeled "vip", shared policy (so it also serves unlabeled work).
// Install a recording consumer updater on that worker:
//   rec := &recordingUpdater{}   // implements parti.WorkerConsumerUpdater;
//                                // records each UpdateWorkerConsumer call's
//                                // partition CanonicalID set
//   cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("vip"),
//       parti.WithWorkerConsumerUpdater(rec))
// Steady state: worker owns p0 (unlabeled) among others. Now promote p0
// to "vip" out-of-band (raw kv.Put, label-only edit): placement CANNOT
// change — the only vip-capable worker already owns it.
// Assert, via require.Eventually on the worker's acked version:
//   - a NEW assignment version was applied (payload hash changed), AND
//   - every UpdateWorkerConsumer call after the promotion carries the
//     SAME CanonicalID set as before (no partition added/removed — no
//     detach/attach at the ownership layer).
```

Partial-batch-crash note (spec §14 bullet, resolved by design rather
than a new harness): parked metadata lives ONLY on the commit record,
which is written by the publisher's single CAS — there is no
payload-side parked state to tear. A leader crash between payload writes
and the commit CAS leaves the previous commit (and its parked view)
fully intact, which the existing partial-batch publisher tests already
pin. The one new surface — parked fields round-tripping the CAS — is
covered by `TestPublish_ParkedMetadataOnCommit` (Task 7).

- [ ] **Step 4: Run, lint, commit**

Run: `go test ./test/integration/assignment/ -run 'TestLabelDemotion_NoStale|TestGhostLabel_Parks|TestLabelPoolOutage|TestLabelOnlyEdit_NoConsumerChurn' -v -race -timeout 600s`
Expected: PASS.

```bash
make lint
git add test/integration/assignment/label_orphan_stale_test.go
git commit -m "test(integration): parked-coverage and stale-assignment regressions for labels"
```

---

### Task 15: Integration — incarnation guard + policy matrix

**Files:**
- Create: `test/integration/assignment/label_incarnation_test.go`
- Create: `test/integration/assignment/label_policy_test.go`

- [ ] **Step 1: Incarnation guard, ID-reuse takeover**

```go
// TestStableIDReuse_DifferentLabels_NoStaleExposureAndConverge: force stable-ID
// reuse across label classes with a 1-slot ID window:
//   cfg.WorkerIDMin = 0; cfg.WorkerIDMax = 0   // exactly one stable ID
//   cfg.WorkerIDTTL = 3 * time.Second
// Start manager A with WithWorkerLabels("vip") + a second config bucket
// set... no: single-worker cluster. Source: 1 vip partition + 1 plain.
//  1. A (labels=[vip]) claims worker-0, becomes leader, owns both
//     partitions (vip pool = itself; plain spills to all = itself).
//     Record commit version V1; its payload has WorkerLabels ["vip"].
//  2. Stop A WITHOUT waiting for KV cleanup (hard stop; claim + commit
//     remain until TTL).
//  3. Start B UNLABELED, same config → after claim TTL it claims
//     worker-0. The stale commit V1 still assigns worker-0 the vip
//     partition with labels-of-record ["vip"] ≠ B's [].
//  4. Assert (a) B NEVER applies V1: poll B.CurrentAssignment() —
//     version stays 0 until a NEW commit (version > V1) appears; the
//     guard log line ("different incarnation") is the observable, and
//     B's heartbeat must never ack V1's digest.
//  5. Assert (b) convergence WITHOUT audit repair (default direct mode):
//     B becomes leader (single worker) and its startup initial rebalance
//     publishes V2 with labels-of-record []; B applies V2; the vip
//     partition parks (no vip worker) then spills after grace to B.
//
// The TIGHT-takeover variant (heartbeat key never lapses, leader
// unchanged) cannot be staged with two real managers sharing one process
// — it is pinned at the component level instead:
// TestCalculator_TightTakeover_LabelChangeTriggersRebalance (calculator,
// real KV) + TestWorkerMonitor_LabelChangeAcrossWatcherRestart (monitor).
// This test pins the guard + convergence half with full managers.
```

- [ ] **Step 2: Pre-label compat + mixed-version scoping**

```go
// TestPreLabelCommit_CompatApplies: hand-write a commit + payload WITHOUT
// the presence bit (SchemaVersion 1, no worker_labels_known — exactly what
// a pre-label leader publishes) into the assignment bucket, then start a
// labeled worker whose ID the commit targets. The worker must APPLY it
// (compat branch: !WorkerLabelsKnown). This pins the worker-side half of
// the mixed-version matrix (spec §11 row 1) without needing to run an old
// library version in-process.
```

Scoping note (documented deviation from spec §14's "mixed-version leader
bouncing" bullet): two library versions cannot run in one Go test binary.
The mixed-version matrix is pinned piecewise instead — old-leader
payloads via TestPreLabelCommit_CompatApplies (above), old-worker
heartbeats via the legacy-decode tests (Task 4), and label-blind
list-writers via the rollout-rule docs (Task 17). State this in the PR
description.

- [ ] **Step 3: Policy matrix**

```go
// TestUnlabeledPartitionPolicy_SharedVsDedicated:
//   dedicated (default): labeled worker + plain worker, all partitions
//     unlabeled → labeled worker owns NOTHING (reservation), plain owns all.
//   shared: same fleet, cfg.UnlabeledPartitionPolicy = "shared" on BOTH
//     workers → both workers own partitions (WaitForBalancedAssignments).
//
// TestAllWorkersLabeled_UnlabeledPartitionsFallBack: every worker labeled
// (vip), partitions unlabeled, dedicated policy → generalPool is empty so
// the ladder falls back to all workers; assert full coverage and
// IncrementUnlabeledFallback observable via a fake metrics collector
// installed with parti.WithMetrics.
```

- [ ] **Step 4: Run, lint, commit**

Run: `go test ./test/integration/assignment/ -run 'TestStableIDReuse|TestPreLabelCommit|TestUnlabeledPartitionPolicy|TestAllWorkersLabeled' -v -race -timeout 600s`

```bash
make lint
git add test/integration/assignment/label_incarnation_test.go test/integration/assignment/label_policy_test.go
git commit -m "test(integration): incarnation guard on stable-id reuse and unlabeled-partition policies"
```

---

### Task 16: Integration — label machinery concurrency stress test

AGENTS.md rule: every new monitor goroutine on a ticker gets a
live-cluster `-race` stress test. This feature adds two ticker/watcher
paths: `monitorLabelRecheck` and the per-PUT fingerprint decode.

**Files:**
- Create: `test/integration/assignment/label_stress_test.go` (template:
  `test/integration/manager/manager_epoch_monitor_concurrency_test.go`)

- [ ] **Step 1: Write the test**

```go
// TestLabelMachinery_NoRaceUnderConcurrentTraffic:
//   - 3 workers: labels [vip], [batch], [] — real cluster, embedded NATS
//   - cfg.LabelSpillGrace = 300 * time.Millisecond (aggressive: park/spill
//     transitions churn constantly)
//   - NATS KV source with 12 partitions
//   - For ~5 seconds, from 3 concurrent goroutines:
//       g1: every 150ms rewrite the source flipping labels on a rotating
//           subset (vip ↔ batch ↔ unlabeled) — label-only edits
//       g2: every 200ms read the commit + all payloads and assert the
//           coverage invariant assigned ∪ parked == source (hard fail on
//           any orphan window)
//       g3: stop the batch worker mid-soak and restart it 2s later —
//           exercises pool-empty detection, defer/confirm, park, and
//           re-home while g1's label churn is in flight
//   - Liveness proxy: assignment version strictly increases during the
//     soak (as in the epoch-monitor template)
//   - Pass criterion: no race-detector trigger, no coverage violation,
//     cluster returns to a stable full-coverage state within 20s after
//     the churn stops.
```

- [ ] **Step 2: Run it under race**

Run: `go test ./test/integration/assignment/ -run 'TestLabelMachinery_NoRace' -v -race -timeout 300s`
Expected: PASS with zero `WARNING: DATA RACE` output. This is the test
that historically catches what unit suites cannot (shared nats.go stream
state between monitor and production goroutines).

- [ ] **Step 3: Commit**

```bash
make lint
git add test/integration/assignment/label_stress_test.go
git commit -m "test(integration): label machinery stress under concurrent source churn"
```

---

### Task 17: Documentation + CHANGELOG

**Files:**
- Create: `docs/LABELS.md`
- Modify: `README.md` (feature bullet + link), `docs/STRATEGIES.md`
  (pointer: label routing happens ABOVE strategies; custom strategies
  need no changes), `CHANGELOG.md` (v2.9.0 section)
- Godoc: already written per-symbol in earlier tasks; verify with
  `go doc ./types Partition` etc.

**`docs/LABELS.md` must cover** (source: spec §5, §8.1, §9, §11):
- The model: worker label sets (fixed at startup), one optional partition
  label, membership matching; VIP runtime promotion workflow via
  full-list rewrite.
- Policy knobs: `UnlabeledPartitionPolicy` (dedicated vs shared),
  `LabelSpillGrace` — both **fleet-uniform** (leader-side; same contract
  as the strategy choice).
- Park/spill semantics + the worst-case stall formula:
  detection (heartbeat TTL) + confirmation + `LabelSpillGrace` +
  rebalance/handoff; grace clocks are per-leader-term (failover restarts
  them).
- **Rollout ordering rules** (both from spec §11, verbatim intent):
  upgrade every deployment before labeling anything; upgrade every
  partition-list writer first (including the provision CLI) — an old
  writer's full-list rewrite silently strips labels.
- Recommended pattern: distinct `WorkerIDPrefix` per deployment (makes
  cross-deployment stable-ID takeover structurally impossible; the
  incarnation guard covers the residual same-deployment relabel case).
- What operators see: parked metrics/logs, label-change trigger logs,
  incarnation-reject warnings.

**CHANGELOG v2.9.0**: added (Partition.Label, Config.WorkerLabels /
WithWorkerLabels, UnlabeledPartitionPolicy, LabelSpillGrace,
LabelMetrics, payload labels-of-record + parked commit metadata), the
one-time payload re-hash note (first commit from an upgraded leader
re-applies with no ownership movement), and the two rollout rules.

- [ ] Draft all docs, then run `/doc-sync` scoped to the new/edited pages
  to catch signature drift, then:

```bash
make lint
git add docs/ README.md CHANGELOG.md
git commit -m "docs: label-based partition assignment guide and v2.9.0 changelog"
```

---

### Task 18: Final gate — invariant matrix + pre-PR validation

- [ ] **Step 1: Invariant → test matrix.** Verify each spec invariant has
  a passing, named test; record the table in the PR description:

| Invariant | Test |
|---|---|
| I1 zero-labels identical output | `TestComputeLabelAssignments_I1Golden` + full existing calculator suite green |
| I2 assigned ∪ parked == source | `TestCheckCoverage_ParkedUnionAndDisjointness`, `TestGhostLabel_ParksThenSpills_NeverOrphans`, stress g2 assertions |
| I3 label-blind identity | `TestPartitionLabel_IdentityBlind` |
| I4 label-only change notifies | `TestPartitionsEqual_LabelAware`, `TestNatsKV_LabelOnlyEdit_{Watch,Reconcile}PathPropagates`, `TestLabelPromotion_EndToEnd` |
| I5 spill never invades other pools | `TestComputeLabelAssignments_SpillPrefersUnlabeledWorkers` |
| I6 recheck fires without events | `TestLabelRecheck_GhostLabelProgressesWithoutExternalEvents`, `TestGhostLabel_ParksThenSpills_NeverOrphans` |
| I7 labels immutable per lifetime | `TestConfig_WorkerLabelsNormalization` + heartbeat emission tests |
| I8 merged keys == active set | `TestComputeLabelAssignments_MergeContract`, `TestLabelDemotion_NoStaleAssignmentInKV` |
| I9 one group per partition | `TestComputeLabelAssignments_MergeContract` (count assertion) |
| I10 label survives copy paths | Task 2 + Task 3 round-trip tests |
| I11 guard on both wire paths | `TestLabelGuard_Matrix`, `TestLabelGuard_RejectIsTerminalNoRetryNoAck`, `TestLabelGuard_RetryEntrypointAlsoGuarded`, `TestStableIDReuse_DifferentLabels_NoStaleExposureAndConverge` |
| §4.1 no ownership movement on label-only edit | `TestLabelOnlyEdit_NoConsumerChurnWhenPlacementUnchanged` (+ `TestPublish_PayloadCarriesLabelsOfRecord` for the hash side) |
| I12 defer-once confirmation | `TestLabelState_DeferOnceThenPark`, `TestLabelState_UnknownWorkerDeferThenAct` |
| I13 label-change trigger | `TestWorkerMonitor_LabelChangeEscapesSuppression`, `TestWorkerMonitor_LabelChangeAcrossWatcherRestart`, `TestCalculator_TightTakeover_LabelChangeTriggersRebalance` |

- [ ] **Step 2: Cross-feature contracts** (AGENTS.md): no error
  classification or routing was changed, but run the pinned tests
  explicitly: `TestManager_LiveNATSBucketLoss*`,
  `TestStableID_StaleKeyTakeover_Reclaim`, `TestStart_*`.

- [ ] **Step 3: The gate.**

Run: `make pre-pr`
Expected: lint clean, unit `-race` green, integration `-race` green.
Known load-flakes that are NOT this feature: `TestLeaderElection_ColdStart`,
`TestHandoffConflictStress` (re-run in isolation before blaming the
change).

- [ ] **Step 4: Post-implementation review loop** per project practice:
  `/simplify`, then `/post-impl-review <phase> docs/plans/label-assignment/01-implementation-plan.md v1`,
  iterate to merge-clean.
