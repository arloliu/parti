# Consumer Options API — `WithConsumerMemoryStorage` + `WithConsumerReplicas`

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose two JetStream consumer-config fields — `MemoryStorage` (bool) and `Replicas` (int) — as universal options on parti's public consumer API (`consumer.Queue`, `consumer.Broadcast`, `consumer.Static`, `consumer.Dynamic`). Operators can then opt into the M2.B configuration ("`MemoryStorage = true` + `Replicas = 1`" → 99 % `block_write_iops` reduction at high partition count, per the IOPS investigation) without forking parti or wrapping its JetStream context.

**Architecture:** Add two fields to `consumer.CommonConfig` mirroring how `MaxAckPending` is already exposed. Add two universal `Option` constructors (`WithConsumerMemoryStorage`, `WithConsumerReplicas`) following the existing functional-option pattern in `consumer/options.go`. Each of the four parti consumer types has its own implementation path; each builds at least one `jetstream.ConsumerConfig` literal at consumer-create time, and the new fields must reach every such literal so the recovery snapshot also carries them. **No validation in parti** — pass-through to JetStream's own validator (error code 10126 for `Replicas > stream.Replicas`).

**Tech Stack:** Go 1.x, `github.com/nats-io/nats.go/jetstream`, `github.com/arloliu/fuda` (existing config-defaults helper), `github.com/arloliu/parti/v2/partitest` (existing embedded-NATS helper for tests; imported bare in `consumer/dynamic_test.go` and aliased as `partitesting` in `consumer/queue_test.go` and the `internal/` test files — snippets in this plan match whichever convention the target file already uses).

---

## Background & decisions

### Why universal (not Dynamic-only)

`MemoryStorage` and `Replicas` are JetStream-level concepts that apply to any consumer regardless of which parti consumer type wraps it. The existing universal-option pattern (`WithMaxAckPending`, `WithAckWait`, etc.) lives in `CommonConfig`. Adding here means a single set of `With*` constructors covers all four consumer types, and the API is uniform.

### Why pass-through validation

Per the live experiments in `docs/plans/iops-investigation/findings.md` §8:

- NATS rejects `Replicas > stream.Replicas` at create time with error code 10126 ("consumer config replica count exceeds parent stream"). This is the rule that fires on the **LimitsPolicy** streams the iops investigation measured.
- The rule depends on the stream's current configuration, which the parti consumer constructor doesn't have a cheap, race-free way to validate. Better to forward and surface NATS's error verbatim.

**Additional NATS-server retention-policy constraint** (verified against `nats-server` v2.12.6 source — `server/consumer.go` ~lines 687–701, found during plan-review v2): for **InterestPolicy** and **WorkQueuePolicy** streams there's a second validation branch — nonzero consumer `Replicas` must *equal* the stream's `Replicas`. In other words, on a WorkQueuePolicy stream with `Replicas=3`, only `consumer.Replicas ∈ {0, 3}` is accepted — `Replicas=1` (the M2.B value) is rejected.

This rule fires for **any** parti consumer used on an InterestPolicy or WorkQueuePolicy stream, not just `Queue`. Per the existing Godoc on each public consumer type: `Queue` is the most common WorkQueuePolicy user (see `consumer/queue.go:101+`), but `Dynamic` and `Static` also document WorkQueuePolicy support (with recovery-strategy restrictions — see `consumer/dynamic.go:155+`, `consumer/static.go:100+`). `Broadcast` is explicitly incompatible with WorkQueuePolicy (`consumer/broadcast.go:29+`) so the rule is moot for it. Practically: on InterestPolicy/WorkQueuePolicy streams, the strongest IOPS-reducing knob available is `WithConsumerMemoryStorage(true)` alone (the M2.A recipe). The Godoc on `WithConsumerReplicas` must say this so operators don't ship `Replicas=1` on a WorkQueuePolicy stream and discover the rejection at create time.

### Why `MemoryStorage` is not live-editable (and what docs must say)

The follow-up experiments showed that `nats consumer edit` does not expose a `--memory` flag. Changing `MemoryStorage` on an existing consumer requires delete + recreate, which drops the consumer's ack/delivery offsets. The Godoc must state this explicitly so operators don't reach for "just change it later." `Replicas`, by contrast, IS live-editable in both directions (the raft group expands/shrinks in place and converges within seconds).

### Architectural map: four consumer types, different code paths

The four parti consumer types do NOT share a common create-time `jetstream.ConsumerConfig` construction site. Each has its own. The new fields must land at every site (including recovery snapshots) for the option to actually take effect across the API.

| Consumer | Constructor builds | Build site of `jetstream.ConsumerConfig` |
|---|---|---|
| Dynamic | `durable.WorkerConsumerConfig` | **TWO** literals in `internal/durable/worker_consumer.go`: one in `addSubjectLoop` (line ~414, stored as recovery snapshot) and one in `ensurePerSubjectConsumer` (line ~459, the actual create). Both must be updated. The duplication is pre-existing; a future PR may extract a helper, but for now they are independent literals. |
| Queue | `QueueConfig` (embeds CommonConfig) | ONE literal in `consumer/queue.go:336` (`Queue.ensureConsumer`). The same literal is stored as `q.consumerConfig` (line 349) for recovery, so updating once covers both. |
| Static | `ipartition.ConsumerConfig` (in `consumer/static.go:174`) | ONE literal in `internal/ipartition/consumer.go:229` (`JSConsumer.ensureConsumer`). Same literal is stored as `c.consumerConfig` (line 241) for recovery. |
| Broadcast | `durable.BroadcastConsumerConfig` | ONE literal in `internal/durable/broadcast_consumer.go:267` (`BroadcastConsumer.ensureConsumer`). Same literal is stored as `bc.consumerConfig` (line 211) for recovery. |

### Out of scope (do NOT add)

- A `RescaleConsumerReplicas(ctx, name, n)` helper to live-edit Replicas. Mentioned in earlier discussion as a possible follow-up; defer to a separate PR.
- Removing the `InstrumentedJS.SetConsumerOverrides` interceptor in `test/perf-measurement/`. That cleanup is gated on (a) the iops-investigation merge to origin/main and (b) this PR merging. Will happen in a third, smaller PR.
- Refactoring the duplicated `jetstream.ConsumerConfig` literal in `internal/durable/worker_consumer.go` into a helper. Pre-existing duplication; outside this PR's scope. Track as future-work.
- Stream-level `MemoryStorage` / `Replicas` exposure (those are on `StreamConfig`).
- A placement/affinity option for the consumer raft group. JetStream picks the node for single-replica consumers; controlling it is a separate JetStream feature (`Placement` config) not requested here.

**Known test gap (not a scope decision):** Task 11 covers the LimitsPolicy `Replicas > stream.Replicas` rejection (NATS error 10126). The InterestPolicy/WorkQueuePolicy "nonzero Replicas must equal stream.Replicas" variant is NOT covered by a dedicated test. Rationale: parti is a pass-through — both rules fire from the same `EnsureConsumer` create path, and the NATS-server source owns the retention-policy branch. An additional WorkQueuePolicy assertion would test NATS, not parti. Acceptable to add later if a regression surfaces.

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `consumer/common.go` | `CommonConfig` struct (universal consumer-config fields) | Add `ConsumerMemoryStorage bool` + `ConsumerReplicas int` |
| `consumer/options.go` | `options` struct + `With*` constructor functions | Add 2 fields + 2 universal `Option` constructors |
| `consumer/options_test.go` | Option-application unit tests | Add `TestWithConsumerMemoryStorage` + `TestWithConsumerReplicas` |
| `internal/durable/config.go` | `WorkerConsumerConfig` struct (line 101) | Add 2 fields |
| `internal/durable/worker_consumer.go` | Dynamic `jetstream.ConsumerConfig` literals at lines 414 + 459 | Set 2 fields in BOTH literals |
| `internal/durable/broadcast_config.go` | `BroadcastConsumerConfig` struct (line 36) | Add 2 fields |
| `internal/durable/broadcast_consumer.go` | Broadcast `jetstream.ConsumerConfig` literal at line 267 | Set 2 fields |
| `internal/ipartition/config.go` | `ConsumerConfig` struct (search for `^type ConsumerConfig`) | Add 2 fields |
| `internal/ipartition/consumer.go` | Static `jetstream.ConsumerConfig` literal at line 229 | Set 2 fields |
| `consumer/dynamic.go` | `NewDynamic` builds DynamicConfig + WorkerConsumerConfig | Forward 2 fields through both |
| `consumer/queue.go` | `NewQueue` builds QueueConfig | Forward 2 fields through CommonConfig |
| `consumer/static.go` | `NewStatic` builds StaticConfig + ipartition.ConsumerConfig | Forward 2 fields through both |
| `consumer/broadcast.go` | `NewBroadcast` builds BroadcastConfig + BroadcastConsumerConfig | Forward 2 fields through both |
| `consumer/dynamic_test.go` | Existing `package consumer_test` tests | Add `TestDynamic_ConsumerOptions_AppliedToLiveConsumer` |
| `consumer/queue_test.go` | Existing **`package consumer`** tests (white-box style) | Add `TestQueue_ConsumerOptions_AppliedToLiveConsumer`, the validation pass-through test, AND the Queue recovery-snapshot test (all white-box; unqualified `NewQueue` etc.) |
| `consumer/static_test.go` | Existing **`package consumer`** tests (white-box style) | Add `TestStatic_ConsumerOptions_AppliedToLiveConsumer` (unqualified; uses Static placeholder `{{partition}}`) |
| `consumer/broadcast_test.go` (**NEW** — no test file exists yet) | New `package consumer` test file | Create with `TestBroadcast_ConsumerOptions_AppliedToLiveConsumer` |
| `internal/ipartition/consumer_test.go` (extend) | `package ipartition` tests | Add `TestJSConsumer_RecoverySnapshot_CarriesConsumerOptions` — exercises Static's snapshot at the layer that owns the field |
| `internal/durable/broadcast_consumer_test.go` (extend) | `package durable` tests | Add `TestBroadcastConsumer_RecoverySnapshot_CarriesConsumerOptions` |
| `internal/durable/worker_consumer_loop_test.go` (extend; NOT worker_consumer_test.go — the canonical Dynamic construction pattern lives in the loop file at ~lines 44–76) | `package durable` tests | Add `TestWorkerConsumer_RecoverySnapshotMatchesCreateConfig` — catches drift between the two literals at worker_consumer.go lines 414 and 459 |
| `CHANGELOG.md` | Unreleased section | Entry under "Added" |

---

## Task 1: Add fields to all three internal config structs

This task adds struct fields with no use-sites yet. The build stays clean because the fields are additive zero-value defaults. Doing all three internal struct additions in one commit keeps later tasks focused on a single consumer type each.

**Files:**
- Modify: `internal/durable/config.go` (struct `WorkerConsumerConfig` at line 101)
- Modify: `internal/durable/broadcast_config.go` (struct `BroadcastConsumerConfig` at line 36)
- Modify: `internal/ipartition/config.go` (struct `ConsumerConfig`)

- [ ] **Step 1: Find each struct's `MaxAckPending` field**

Run:
```bash
rg -n '^\s*MaxAckPending\b' internal/durable/config.go internal/durable/broadcast_config.go internal/ipartition/config.go
```
Expected: one line in each file. Open each at the indicated line.

- [ ] **Step 2: Add the two fields immediately after `MaxAckPending` in each struct**

Use this exact text in all three structs (the field comments are identical because the field semantics are identical):

```go
	// ConsumerMemoryStorage forwards to jetstream.ConsumerConfig.MemoryStorage
	// on consumer create. When true, the consumer's delivery/ack state is
	// kept in memory rather than inheriting the stream's storage type.
	// See consumer.WithConsumerMemoryStorage for full semantics and the
	// non-live-editable caveat.
	ConsumerMemoryStorage bool

	// ConsumerReplicas overrides jetstream.ConsumerConfig.Replicas on
	// consumer create. 0 (default) inherits the parent stream's replica
	// count; lower values reduce consumer-state raft replication.
	// See consumer.WithConsumerReplicas for the validation rule (must be
	// ≤ stream replicas, NATS error 10126 on violation).
	ConsumerReplicas int
```

- [ ] **Step 3: Verify the build**

Run: `go build ./...`
Expected: succeeds, no diagnostics.

- [ ] **Step 4: Run existing tests to confirm no regression**

Run: `go test ./internal/durable/... ./internal/ipartition/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/durable/config.go internal/durable/broadcast_config.go internal/ipartition/config.go
git commit -m "feat(internal): add ConsumerMemoryStorage + ConsumerReplicas to internal configs

Adds two fields to WorkerConsumerConfig, BroadcastConsumerConfig, and
ipartition.ConsumerConfig. No use-sites yet — wired up by later
commits per consumer type."
```

---

## Task 2: Add public API (CommonConfig + options + With* constructors + unit tests)

**Files:**
- Modify: `consumer/common.go` (`CommonConfig` struct, after `AckPolicy` field around line 97)
- Modify: `consumer/options.go` (`options` struct around line 135, `// -- Common Options --` section ending around line 435)
- Modify: `consumer/options_test.go` (add two new test functions)

- [ ] **Step 1: Add two fields to `CommonConfig` in `consumer/common.go`**

Insert immediately after the `AckPolicy jetstream.AckPolicy` field (around line 97), before the closing `}` of the struct:

```go
	// ConsumerMemoryStorage, when true, sets the underlying
	// jetstream.ConsumerConfig.MemoryStorage flag on consumer create,
	// keeping the consumer's delivery/ack state in memory rather than
	// inheriting the stream's storage type.
	//
	// Default: false (inherit stream storage).
	//
	// Trade-off: the consumer's delivery/ack offsets are NOT durable
	// across coordinated cluster restart. With ConsumerReplicas ≥ 2,
	// single-node failure is still survivable via raft peers. With
	// ConsumerReplicas = 1, any failure of the consumer-state holder
	// loses ack state and triggers redelivery from DeliverPolicy.
	//
	// IMPORTANT: this field is NOT live-editable on the NATS server.
	// Changing it after the consumer exists requires delete + recreate,
	// which drops ack/delivery offsets. Pick the value at construction
	// time.
	//
	// For at-least-once work-queue patterns with idempotent handlers
	// this is typically safe and yields a large IOPS reduction. See
	// docs/plans/iops-investigation/findings.md §2 for measurements
	// and §4 for the operator decision tree.
	ConsumerMemoryStorage bool

	// ConsumerReplicas overrides the underlying
	// jetstream.ConsumerConfig.Replicas value at consumer create time.
	//
	// Default: 0 (inherit the stream's replica count). Set to 1 to
	// disable consumer-state raft replication (lowest IOPS, no
	// consumer-state HA). Values between 1 and the stream's replica
	// count give intermediate IOPS/HA trade-offs.
	//
	// Constraint: must be ≤ the parent stream's Replicas. NATS rejects
	// invalid values at consumer create with error code 10126
	// ("consumer config replica count exceeds parent stream"). parti
	// does not pre-validate; the JetStream error is surfaced verbatim
	// when the underlying consumer is created or updated
	// (Queue/Static/Broadcast at Start, Dynamic at Update).
	//
	// Unlike ConsumerMemoryStorage, this field IS live-editable on
	// the NATS server via `nats consumer edit --replicas=N`; the raft
	// group expands/shrinks in place.
	ConsumerReplicas int `validate:"gte=0"`
```

- [ ] **Step 2: Add the two fields to `options` struct in `consumer/options.go`**

Locate the `// Common` section of the `options` struct (around line 137). After `ackPolicy jetstream.AckPolicy`, add:

```go
	consumerMemoryStorage bool
	consumerReplicas      int
```

- [ ] **Step 3: Append two `With*` constructors at end of `// -- Common Options --` section**

After `WithAckPolicy` (around line 380) and before `// -- Shared or Specific Options --`, add:

```go
// WithConsumerMemoryStorage sets the underlying
// jetstream.ConsumerConfig.MemoryStorage flag on consumer create.
//
// When true, the consumer's delivery and ack state lives in memory
// rather than inheriting the stream's storage type. The published
// message log is unaffected — it stays wherever the stream is
// configured to live.
//
// Trade-off: consumer state is NOT durable across coordinated cluster
// restart. With Replicas ≥ 2 (the default at stream R ≥ 2), single-
// node failure is survivable via raft peers. With Replicas = 1, any
// failure of the consumer-state holder triggers redelivery from
// DeliverPolicy.
//
// IMPORTANT: this option is NOT live-editable on the NATS server.
// Changing the value after the consumer exists requires delete +
// recreate, which drops ack/delivery offsets.
//
// Measured impact: see docs/plans/iops-investigation/findings.md
// §3 for the cost decomposition and §4 for the decision tree.
//
// Default: false (inherit stream storage type).
func WithConsumerMemoryStorage(enabled bool) Option {
	return universalOpt(func(o *options) {
		o.consumerMemoryStorage = enabled
	})
}

// WithConsumerReplicas overrides the underlying
// jetstream.ConsumerConfig.Replicas value at consumer create time.
//
// 0 (the default) inherits the parent stream's replica count. 1
// disables consumer-state raft replication (lowest IOPS, no
// consumer-state HA). Values between 1 and the stream's Replicas
// give intermediate IOPS/HA trade-offs.
//
// Constraints (validated server-side by NATS, surfaced verbatim
// when the underlying JetStream consumer is created or updated —
// parti does not pre-validate. The error surfaces at different
// times per consumer type: Queue/Static/Broadcast at Start; Dynamic
// at Update):
//
//   - On LimitsPolicy streams (parti's default): must be
//     0 ≤ Replicas ≤ stream.Replicas. Values above stream.Replicas
//     are rejected with NATS error code 10126 ("consumer config
//     replica count exceeds parent stream").
//   - On InterestPolicy and WorkQueuePolicy streams: nonzero
//     Replicas must EQUAL stream.Replicas. So on a WorkQueuePolicy
//     stream with stream.Replicas=3, only Replicas ∈ {0, 3} is
//     accepted; Replicas=1 (the M2.B value) is rejected. This is
//     a NATS-server-side rule (server/consumer.go in nats-server
//     v2.12.6), not a parti choice. Practically, ANY parti consumer
//     used on an InterestPolicy or WorkQueuePolicy stream cannot
//     use Replicas=1 — pair the consumer with
//     WithConsumerMemoryStorage(true) alone for the durability-
//     preserving IOPS reduction on those retention policies.
//
// Unlike WithConsumerMemoryStorage, this option IS live-editable on
// the NATS server (`nats consumer edit --replicas=N`); the raft
// group expands/shrinks in place and converges within seconds.
//
// Negative values are silently ignored (defensive guard; matches
// existing With* style).
//
// Default: 0 (inherit stream replicas).
func WithConsumerReplicas(n int) Option {
	return universalOpt(func(o *options) {
		if n >= 0 {
			o.consumerReplicas = n
		}
	})
}
```

- [ ] **Step 4: Add unit tests to `consumer/options_test.go`**

Append:

```go
func TestWithConsumerMemoryStorage(t *testing.T) {
	o := defaultOptions()
	if o.consumerMemoryStorage {
		t.Errorf("default consumerMemoryStorage = true, want false")
	}

	WithConsumerMemoryStorage(true).apply(&o)
	if !o.consumerMemoryStorage {
		t.Errorf("after WithConsumerMemoryStorage(true), got false")
	}

	WithConsumerMemoryStorage(false).apply(&o)
	if o.consumerMemoryStorage {
		t.Errorf("after WithConsumerMemoryStorage(false), got true")
	}
}

func TestWithConsumerReplicas(t *testing.T) {
	o := defaultOptions()
	if o.consumerReplicas != 0 {
		t.Errorf("default consumerReplicas = %d, want 0", o.consumerReplicas)
	}

	WithConsumerReplicas(3).apply(&o)
	if o.consumerReplicas != 3 {
		t.Errorf("after WithConsumerReplicas(3), got %d", o.consumerReplicas)
	}

	WithConsumerReplicas(1).apply(&o)
	if o.consumerReplicas != 1 {
		t.Errorf("after WithConsumerReplicas(1), got %d", o.consumerReplicas)
	}

	// Negative values are silently ignored (defensive guard).
	o.consumerReplicas = 5
	WithConsumerReplicas(-1).apply(&o)
	if o.consumerReplicas != 5 {
		t.Errorf("after WithConsumerReplicas(-1), got %d, want 5 (unchanged)", o.consumerReplicas)
	}
}
```

- [ ] **Step 5: Run the new tests**

Run: `go test ./consumer/ -run '^TestWithConsumer' -v`
Expected: both new tests PASS.

- [ ] **Step 6: Verify existing consumer tests still pass**

Run: `go test ./consumer/...`
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add consumer/common.go consumer/options.go consumer/options_test.go
git commit -m "feat(consumer): add WithConsumerMemoryStorage and WithConsumerReplicas options

Universal Options following the existing WithMaxAckPending pattern.
Negative Replicas values are silently ignored (defensive guard).
Default values (false / 0) preserve existing behavior. No consumer
type honors the options yet — wired up by later commits."
```

---

## Task 3: Plumb fields through `consumer.Dynamic`

**Files:**
- Modify: `consumer/dynamic.go` — `NewDynamic` builds `cfg.CommonConfig` (around line 195–207) and `workerCfg` (around line 231–265)
- Modify: `internal/durable/worker_consumer.go` — TWO `jetstream.ConsumerConfig` literals at lines 414 and 459

- [ ] **Step 1: Set the fields in `DynamicConfig.CommonConfig` in `NewDynamic`**

In `consumer/dynamic.go`, locate the `cfg := DynamicConfig{ CommonConfig: CommonConfig{ ... } }` literal (around line 194–207). Add inside `CommonConfig{ ... }`, after `AckPolicy: o.ackPolicy,`:

```go
			ConsumerMemoryStorage: o.consumerMemoryStorage,
			ConsumerReplicas:      o.consumerReplicas,
```

- [ ] **Step 2: Forward into `durable.WorkerConsumerConfig` in the same function**

In the same file, find the `workerCfg := durable.WorkerConsumerConfig{ ... }` literal (around line 231–265). Add after `AckPolicy: cfg.AckPolicy,`:

```go
		ConsumerMemoryStorage:       cfg.ConsumerMemoryStorage,
		ConsumerReplicas:            cfg.ConsumerReplicas,
```

- [ ] **Step 3: Locate the FIRST `jetstream.ConsumerConfig` literal in `worker_consumer.go`**

Run:
```bash
rg -n 'jetstream\.ConsumerConfig\{' internal/durable/worker_consumer.go
```
Expected: TWO line numbers (around 414 and 459). Open both.

- [ ] **Step 4: Update the literal in `addSubjectLoop` (line ~414)**

This is the literal stored as the recovery snapshot. After `MaxAckPending: wc.config.MaxAckPending,`, add:

```go
		MemoryStorage:     wc.config.ConsumerMemoryStorage,
		Replicas:          wc.config.ConsumerReplicas,
```

- [ ] **Step 5: Update the literal in `ensurePerSubjectConsumer` (line ~459)**

This is the actual create literal. Same two lines after `MaxAckPending: wc.config.MaxAckPending,`:

```go
		MemoryStorage:     wc.config.ConsumerMemoryStorage,
		Replicas:          wc.config.ConsumerReplicas,
```

> Note: both literals are duplicated by design in the current code. A future PR may extract a helper; out of scope here.

- [ ] **Step 6: Build + test**

Run: `go build ./... && go test ./consumer/ -run TestDynamic -v && go test ./internal/durable/...`
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add consumer/dynamic.go internal/durable/worker_consumer.go
git commit -m "feat(consumer): plumb ConsumerMemoryStorage + ConsumerReplicas through Dynamic

Forwards from options → DynamicConfig.CommonConfig →
durable.WorkerConsumerConfig → the two jetstream.ConsumerConfig
literals in worker_consumer.go (addSubjectLoop's recovery snapshot
at line 414 and the create-time literal in ensurePerSubjectConsumer
at line 459). Both literals must carry the fields so recovery
preserves them."
```

---

## Task 4: Plumb fields through `consumer.Queue`

**Files:**
- Modify: `consumer/queue.go` — `NewQueue` builds `QueueConfig.CommonConfig`, and the `cfg := jetstream.ConsumerConfig{...}` literal in `Queue.ensureConsumer` at line 336

- [ ] **Step 1: Set the fields in `QueueConfig.CommonConfig` literal in `NewQueue`**

Find `cfg := QueueConfig{ CommonConfig: CommonConfig{ ... } }` in `consumer/queue.go` (search for `QueueConfig{`). Add after `AckPolicy: o.ackPolicy,`:

```go
			ConsumerMemoryStorage: o.consumerMemoryStorage,
			ConsumerReplicas:      o.consumerReplicas,
```

- [ ] **Step 2: Set the fields in `Queue.ensureConsumer`'s `jetstream.ConsumerConfig` literal (line ~336)**

After `MaxAckPending: q.config.MaxAckPending,`:

```go
		MemoryStorage:     q.config.ConsumerMemoryStorage,
		Replicas:          q.config.ConsumerReplicas,
```

The same literal is stored as `q.consumerConfig` at line 349 for recovery; updating the literal here covers both initial create and recovery.

- [ ] **Step 3: Build + test**

Run: `go build ./consumer/... && go test ./consumer/ -run TestQueue -v`
Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add consumer/queue.go
git commit -m "feat(consumer): plumb ConsumerMemoryStorage + ConsumerReplicas through Queue"
```

---

## Task 5: Plumb fields through `consumer.Static`

**Files:**
- Modify: `consumer/static.go` — `NewStatic` builds `StaticConfig.CommonConfig` and the `partitionCfg := ipartition.ConsumerConfig{...}` literal (around line 174)
- Modify: `internal/ipartition/consumer.go` — the `cfg := jetstream.ConsumerConfig{...}` literal at line 229

- [ ] **Step 1: Set the fields in `StaticConfig.CommonConfig` in `NewStatic`**

In `consumer/static.go`, find the `cfg := StaticConfig{ CommonConfig: CommonConfig{ ... } }` literal. Add the same two lines after `AckPolicy: o.ackPolicy,`:

```go
			ConsumerMemoryStorage: o.consumerMemoryStorage,
			ConsumerReplicas:      o.consumerReplicas,
```

- [ ] **Step 2: Forward to `ipartition.ConsumerConfig` in the same function (line ~174)**

In the `partitionCfg := ipartition.ConsumerConfig{ ... }` literal, add after `AckPolicy: cfg.AckPolicy,`:

```go
		ConsumerMemoryStorage: cfg.ConsumerMemoryStorage,
		ConsumerReplicas:      cfg.ConsumerReplicas,
```

- [ ] **Step 3: Set the fields in `ipartition.JSConsumer.ensureConsumer` (line 229)**

In `internal/ipartition/consumer.go`, find the `cfg := jetstream.ConsumerConfig{ ... }` literal (line 229). After `MaxWaiting: c.config.MaxWaiting,`:

```go
		MemoryStorage:     c.config.ConsumerMemoryStorage,
		Replicas:          c.config.ConsumerReplicas,
```

The literal is stored as `c.consumerConfig` at line 241 for recovery; updating once covers both.

- [ ] **Step 4: Build + test**

Run: `go build ./... && go test ./consumer/ -run TestStatic -v && go test ./internal/ipartition/...`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add consumer/static.go internal/ipartition/consumer.go
git commit -m "feat(consumer): plumb ConsumerMemoryStorage + ConsumerReplicas through Static

Forwards from options → StaticConfig.CommonConfig →
ipartition.ConsumerConfig → the jetstream.ConsumerConfig literal in
ipartition.JSConsumer.ensureConsumer (line 229). The recovery
snapshot at line 241 references the same literal so it's covered
automatically."
```

---

## Task 6: Plumb fields through `consumer.Broadcast`

**Files:**
- Modify: `consumer/broadcast.go` — `NewBroadcast` builds `BroadcastConfig.CommonConfig` and forwards to `durable.BroadcastConsumerConfig`
- Modify: `internal/durable/broadcast_consumer.go` — the `cfg := jetstream.ConsumerConfig{...}` literal at line 267

- [ ] **Step 1: Set the fields in `BroadcastConfig.CommonConfig` in `NewBroadcast`**

Same pattern as Tasks 3–5. In `consumer/broadcast.go`, find the `BroadcastConfig{ CommonConfig: CommonConfig{ ... } }` literal. Add the two lines after `AckPolicy: o.ackPolicy,`.

- [ ] **Step 2: Forward to `durable.BroadcastConsumerConfig` in the same function**

In the `broadcastCfg := durable.BroadcastConsumerConfig{ ... }` literal (around line 149), add after `AckPolicy: cfg.AckPolicy,`:

```go
		ConsumerMemoryStorage: cfg.ConsumerMemoryStorage,
		ConsumerReplicas:      cfg.ConsumerReplicas,
```

- [ ] **Step 3: Set the fields in `BroadcastConsumer.ensureConsumer` (line 267)**

In `internal/durable/broadcast_consumer.go`, find the `cfg := jetstream.ConsumerConfig{ ... }` literal at line 267. After `MaxAckPending: bc.config.MaxAckPending,`:

```go
		MemoryStorage:     bc.config.ConsumerMemoryStorage,
		Replicas:          bc.config.ConsumerReplicas,
```

The literal is stored as `bc.consumerConfig` at line 211 for recovery; updating once covers both.

- [ ] **Step 4: Build + test**

Run: `go build ./... && go test ./consumer/ -run TestBroadcast -v && go test ./internal/durable/...`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add consumer/broadcast.go internal/durable/broadcast_consumer.go
git commit -m "feat(consumer): plumb ConsumerMemoryStorage + ConsumerReplicas through Broadcast"
```

---

## Task 7: Live integration test for Dynamic

**Files:**
- Modify: `consumer/dynamic_test.go` (package `consumer_test`) — append new test

- [ ] **Step 1: Write the test**

Append to `consumer/dynamic_test.go`:

```go
// TestDynamic_ConsumerOptions_AppliedToLiveConsumer verifies that
// WithConsumerMemoryStorage and WithConsumerReplicas are forwarded
// all the way to the JetStream consumer's live config.
//
// End-to-end coverage for the plumb-through from options to
// jetstream.ConsumerConfig via dynamic.go → worker_consumer.go.
//
// Note: embedded NATS is single-node (cluster R=1). For Replicas
// we can assert Replicas=1 with WithConsumerReplicas(1); the
// default-vs-explicit subcase is covered by the white-box test in
// internal/durable/worker_consumer_loop_test.go (Task 12d) which
// inspects the cfg sent to NATS rather than what the server reports
// back.
func TestDynamic_ConsumerOptions_AppliedToLiveConsumer(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	// no cleanup — partitest.StartEmbeddedNATS registers t.Cleanup internally

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamName := "DYN_OPT"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dynopt.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	dyn, err := consumer.NewDynamic(
		js,
		streamName,
		"dynopt_worker",
		"dynopt.{{.PartitionID}}",
		handler,
		consumer.WithConsumerMemoryStorage(true),
		consumer.WithConsumerReplicas(1),
	)
	require.NoError(t, err)
	defer dyn.Stop(ctx)

	// Update signature: []types.Partition, NOT []int. Mirror the
	// canonical usage in consumer/dynamic_test.go (e.g. the
	// TestDynamic_ImplementsCapabilityReporter test).
	err = dyn.Update(ctx, "worker-0", []types.Partition{{Keys: []string{"p0"}}})
	require.NoError(t, err)

	// Find the per-partition consumer and assert its live Config.
	stream, err := js.Stream(ctx, streamName)
	require.NoError(t, err)

	lister := stream.ListConsumers(ctx)
	var found bool
	for ci := range lister.Info() {
		if !strings.HasPrefix(ci.Name, "dynopt_worker_") {
			continue
		}
		found = true
		require.True(t, ci.Config.MemoryStorage,
			"consumer %q: Config.MemoryStorage = false, want true", ci.Name)
		require.Equal(t, 1, ci.Config.Replicas,
			"consumer %q: Config.Replicas = %d, want 1", ci.Name, ci.Config.Replicas)
	}
	require.NoError(t, lister.Err(), "ListConsumers iteration failed")
	require.True(t, found, "no per-partition consumer was created under the dynopt_worker prefix")
}
```

- [ ] **Step 2: Update imports**

`consumer/dynamic_test.go` is `package consumer_test`. The new test needs `strings` and `types`. The file ALREADY imports `github.com/arloliu/parti/v2/partitest` bare (used by existing tests via `partitest.NopOwnershipResolver{}`), so the snippet calls `partitest.StartEmbeddedNATS(t)` — do NOT add a `partitesting` alias; doing so would import the same path twice and fail to compile.

Required net-new imports for this test:

```go
import (
    // existing imports unchanged (including bare "github.com/arloliu/parti/v2/partitest") ...
    "strings"

    "github.com/arloliu/parti/v2/types"
)
```

Run `goimports -w consumer/dynamic_test.go` if available to auto-organize.

- [ ] **Step 3: Run the test**

Run: `go test ./consumer/ -run TestDynamic_ConsumerOptions_AppliedToLiveConsumer -v`
Expected: PASS.

If FAIL on `Config.MemoryStorage` or `Config.Replicas`: re-trace Task 3 — one of the two literals in `worker_consumer.go` is missing the field.

- [ ] **Step 4: Commit**

```bash
git add consumer/dynamic_test.go
git commit -m "test(consumer): end-to-end coverage for Dynamic with new consumer options"
```

---

## Task 8: Live integration test for Queue

**Files:**
- Modify: `consumer/queue_test.go` (**package `consumer`** — same-package / white-box; verified via `head -1 consumer/queue_test.go`) — append new test

Because `consumer/queue_test.go` is `package consumer` (not `consumer_test`), unqualified names (`NewQueue`, `MessageHandlerFunc`, `WithConsumerMemoryStorage`) work directly — no `consumer.` prefix.

- [ ] **Step 1: Write the test**

Append (mirror the existing `TestQueue_*` tests in the same file for the canonical `NewQueue` signature; the snippet below assumes the 5-positional-arg form `(js, stream, consumerName, filterSubject, handler, ...opts)` — if the actual constructor has a different shape, match what the existing tests use):

```go
// TestQueue_ConsumerOptions_AppliedToLiveConsumer verifies that
// WithConsumerMemoryStorage and WithConsumerReplicas reach the live
// Queue consumer's Config via consumer/queue.go's single
// jetstream.ConsumerConfig literal.
func TestQueue_ConsumerOptions_AppliedToLiveConsumer(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)
	// no cleanup — partitesting.StartEmbeddedNATS registers t.Cleanup internally

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamName := "Q_OPT"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qopt.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	q, err := NewQueue(
		js,
		streamName,
		"qopt-consumer",
		"qopt.>",
		handler,
		WithConsumerMemoryStorage(true),
		WithConsumerReplicas(1),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))
	defer q.Stop(ctx)

	stream, err := js.Stream(ctx, streamName)
	require.NoError(t, err)
	cons, err := stream.Consumer(ctx, "qopt-consumer")
	require.NoError(t, err)

	consInfo, err := cons.Info(ctx)
	require.NoError(t, err)
	require.True(t, consInfo.Config.MemoryStorage, "Config.MemoryStorage = false, want true")
	require.Equal(t, 1, consInfo.Config.Replicas, "Config.Replicas = %d, want 1", consInfo.Config.Replicas)
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./consumer/ -run TestQueue_ConsumerOptions_AppliedToLiveConsumer -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add consumer/queue_test.go
git commit -m "test(consumer): end-to-end coverage for Queue with new consumer options"
```

---

## Task 9: Live integration test for Static

**Files:**
- Modify: `consumer/static_test.go` (**package `consumer`** — same-package / white-box; verified via `head -1 consumer/static_test.go`) — append new test

Three important corrections from plan v2:
1. The file is `package consumer`, so unqualified names work — drop the `consumer.` prefix.
2. Static's subject pattern uses parti's custom placeholder `{{partition}}` (parsed by `internal/partutil/pattern.go`), NOT the Go text/template `{{.PartitionID}}` form Dynamic uses. Mixing them up returns an invalid-pattern error from `partutil.ParsePattern`.
3. `consumer/static_test.go` is currently bare — its only imports are `os`, `testing`, and `require`. The new test adds `context`, `time`, `github.com/arloliu/parti/v2/partitest` (bare, matching `consumer/dynamic_test.go`), `jetstream`, and keeps `os`/`testing`/`require`.

- [ ] **Step 1: Write the test**

Append (match the existing Static tests in the same file for the canonical `NewStatic` signature; the snippet below uses the placeholder syntax confirmed against `internal/partutil/pattern.go:11`):

```go
// TestStatic_ConsumerOptions_AppliedToLiveConsumer verifies that
// WithConsumerMemoryStorage and WithConsumerReplicas reach the live
// Static consumer's Config via the consumer/static.go →
// ipartition.JSConsumer path.
func TestStatic_ConsumerOptions_AppliedToLiveConsumer(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	// no cleanup — partitest.StartEmbeddedNATS registers t.Cleanup internally

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamName := "STATIC_OPT"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"statopt.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	// Static consumer for a specific partition. Use the {{partition}}
	// placeholder (Static's grammar, NOT Dynamic's {{.PartitionID}}).
	// Match the existing TestStatic_* tests in this file for the
	// canonical signature shape.
	s, err := NewStatic(
		js,
		streamName,
		"statopt-p0",
		"statopt.{{partition}}",
		2,    // num partitions
		0,    // this partition
		handler,
		WithConsumerMemoryStorage(true),
		WithConsumerReplicas(1),
	)
	require.NoError(t, err)
	require.NoError(t, s.Start(ctx))
	defer s.Stop(ctx)

	stream, err := js.Stream(ctx, streamName)
	require.NoError(t, err)
	cons, err := stream.Consumer(ctx, "statopt-p0")
	require.NoError(t, err)

	consInfo, err := cons.Info(ctx)
	require.NoError(t, err)
	require.True(t, consInfo.Config.MemoryStorage)
	require.Equal(t, 1, consInfo.Config.Replicas)
}
```

- [ ] **Step 2: Add imports**

`consumer/static_test.go` currently imports only `os`, `testing`, and `require` — the new test needs full embedded-NATS setup. Replace the import block (or add to it) with:

```go
import (
    "context"
    "os"
    "testing"
    "time"

    "github.com/nats-io/nats.go/jetstream"
    "github.com/stretchr/testify/require"

    "github.com/arloliu/parti/v2/partitest"
)
```

(`os` stays for the existing `TestParseStatefulSetOrdinal` test. The snippet does not reference any `nats` package symbol directly, so no `nats.go` import is needed — only `jetstream`. `partitest` is imported bare to match `consumer/dynamic_test.go`'s existing convention; if you instead want to align with `consumer/queue_test.go`'s aliased style, use `partitesting "github.com/arloliu/parti/v2/partitest"` and update the snippet's two `partitest.` call sites to `partitesting.`.)

- [ ] **Step 3: Run + commit**

```bash
go test ./consumer/ -run TestStatic_ConsumerOptions_AppliedToLiveConsumer -v
git add consumer/static_test.go
git commit -m "test(consumer): end-to-end coverage for Static with new consumer options"
```

---

## Task 10: Live integration test for Broadcast

**Files:**
- Create: `consumer/broadcast_test.go` (**NEW** — file does not yet exist; `head -1 consumer/broadcast_test.go` returns "No such file or directory"). Use `package consumer` to match the white-box style used by Queue/Static tests, allowing unqualified names.

- [ ] **Step 1: Find the canonical `NewBroadcast` signature**

Run: `rg -n '^func NewBroadcast' consumer/broadcast.go`
Open the line and copy the signature exactly. The plan assumes Broadcast follows the pattern of other consumers (`js, streamName, instanceID, subjectPattern, handler, ...opts`) — verify against actual.

- [ ] **Step 2: Create the file with the test**

```go
package consumer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/partitest"
)

// TestBroadcast_ConsumerOptions_AppliedToLiveConsumer verifies that
// WithConsumerMemoryStorage and WithConsumerReplicas reach the live
// Broadcast consumer's Config via consumer/broadcast.go →
// internal/durable/broadcast_consumer.go.
//
// Broadcast derives the consumer name from the instance ID + prefix
// at runtime, so we discover it via stream.ListConsumers rather than
// looking up by a fixed name.
func TestBroadcast_ConsumerOptions_AppliedToLiveConsumer(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	// no cleanup — partitest.StartEmbeddedNATS registers t.Cleanup internally

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamName := "BC_OPT"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"bcopt.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	// NewBroadcast signature: match what consumer/broadcast.go exposes.
	// If the signature differs from the (js, stream, prefix, subject,
	// handler, ...opts) shape below, copy the canonical form from the
	// public NewBroadcast Godoc.
	b, err := NewBroadcast(
		js,
		streamName,
		"bcopt",
		"bcopt.>",
		handler,
		WithConsumerMemoryStorage(true),
		WithConsumerReplicas(1),
	)
	require.NoError(t, err)
	require.NoError(t, b.Start(ctx))
	defer b.Stop(ctx)

	stream, err := js.Stream(ctx, streamName)
	require.NoError(t, err)

	lister := stream.ListConsumers(ctx)
	var found bool
	for ci := range lister.Info() {
		if !strings.HasPrefix(ci.Name, "bcopt") {
			continue
		}
		found = true
		require.True(t, ci.Config.MemoryStorage,
			"consumer %q: Config.MemoryStorage = false, want true", ci.Name)
		require.Equal(t, 1, ci.Config.Replicas,
			"consumer %q: Config.Replicas = %d, want 1", ci.Name, ci.Config.Replicas)
	}
	require.NoError(t, lister.Err(), "ListConsumers iteration failed")
	require.True(t, found, "no Broadcast consumer was created under the bcopt prefix")
}
```

- [ ] **Step 3: Run + commit**

```bash
go test ./consumer/ -run TestBroadcast_ConsumerOptions_AppliedToLiveConsumer -v
git add consumer/broadcast_test.go
git commit -m "test(consumer): end-to-end coverage for Broadcast with new consumer options"
```

---

## Task 11: Pass-through validation test (NATS error 10126)

**Files:**
- Modify: `consumer/queue_test.go` (package `consumer`) — append one test

Queue is the simplest target. The constructor `NewQueue` only validates local config; the server call (and any 10126 rejection) happens at `Queue.Start` via `ensureConsumer` → `jsutil.EnsureConsumer`. The test asserts the error surfaces at `Start` time, not at `NewQueue`.

- [ ] **Step 1: Write the test**

Append:

```go
// TestQueue_ConsumerReplicasExceedsStream_Surfaces10126 verifies
// that requesting more consumer replicas than the stream has is
// rejected by NATS with error code 10126, and parti surfaces that
// error verbatim from Queue.Start (pass-through validation; parti
// does not pre-validate).
//
// The constructor NewQueue only applies options and validates local
// config (see consumer/queue.go ~lines 195–233) — it does not call
// NATS. The server consumer create happens in Queue.Start, which is
// where the 10126 surfaces.
func TestQueue_ConsumerReplicasExceedsStream_Surfaces10126(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)
	// no cleanup — partitesting.StartEmbeddedNATS registers t.Cleanup internally

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamName := "Q_VALIDATE"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qvalid.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1, // single-node embedded NATS only supports R=1
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	// Request 2 replicas on a single-replica stream. NewQueue should
	// succeed (it doesn't call NATS); Start should fail with 10126.
	q, err := NewQueue(
		js,
		streamName,
		"qvalid-consumer",
		"qvalid.>",
		handler,
		WithConsumerReplicas(2),
	)
	require.NoError(t, err, "NewQueue should not call NATS; the error should surface at Start")
	require.NotNil(t, q)

	err = q.Start(ctx)
	require.Error(t, err, "Queue.Start should reject Replicas=2 with stream Replicas=1")
	require.Contains(t, err.Error(), "10126",
		"expected NATS error code 10126 in error message, got: %v", err)
}
```

- [ ] **Step 2: Run + commit**

```bash
go test ./consumer/ -run TestQueue_ConsumerReplicasExceedsStream_Surfaces10126 -v
git add consumer/queue_test.go
git commit -m "test(consumer): pass-through validation surfaces NATS error 10126"
```

---

## Task 12: White-box recovery-snapshot tests (per consumer type, in their owning package)

This task catches a class of bugs the integration tests cannot: the snapshot stored for recovery on each consumer must contain the new fields, so a recovery-triggered recreate doesn't silently drop the operator's choices. The single-node embedded NATS resolves `Replicas=0` and `Replicas=1` to the same `Info().Config.Replicas=1`, so integration tests cannot distinguish "explicit 1" from "default inherit." These white-box tests read the unexported `consumerConfig` field on each consumer (the actual cfg sent to NATS).

**The unexported-field owners are NOT all in `package consumer`:**

| Consumer type | Owning package | Snapshot field site | Test file |
|---|---|---|---|
| Queue | `consumer` | `Queue.consumerConfig` (consumer/queue.go:61) | `consumer/queue_test.go` (already `package consumer`) — Task 12a |
| Static | `ipartition` | `JSConsumer.consumerConfig` (internal/ipartition/consumer.go:46) | `internal/ipartition/consumer_test.go` (`package ipartition`) — Task 12b |
| Broadcast | `durable` | `BroadcastConsumer.consumerConfig` (internal/durable/broadcast_consumer.go:66) | `internal/durable/broadcast_consumer_test.go` (`package durable`) — Task 12c |
| Dynamic | `durable` | `partitionConsumer.consumerConfig` AND the literal in `addSubjectLoop`; the two MUST agree | `internal/durable/worker_consumer_loop_test.go` (`package durable`) — Task 12d |

Each sub-task is a separate test in its own file, accessing the unexported field directly via same-package access (no reflection, no test-only accessor).

### Task 12a — Queue recovery snapshot (in consumer/queue_test.go)

- [ ] **Step 1: Append to `consumer/queue_test.go`** (package consumer):

```go
// TestQueue_RecoverySnapshot_CarriesConsumerOptions verifies the
// stored recovery-snapshot config has both new fields set. The
// integration test cannot distinguish "explicit Replicas=1" from
// "default Replicas=0 → server inherits to 1" on a single-node rig;
// this white-box test reads the cfg parti actually sent to NATS.
func TestQueue_RecoverySnapshot_CarriesConsumerOptions(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)
	// no cleanup — partitesting.StartEmbeddedNATS registers t.Cleanup internally

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamName := "Q_SNAP"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qsnap.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	// Case A: explicit options.
	qExplicit, err := NewQueue(js, streamName, "qsnap-explicit", "qsnap.>", handler,
		WithConsumerMemoryStorage(true), WithConsumerReplicas(1))
	require.NoError(t, err)
	require.NoError(t, qExplicit.Start(ctx))
	defer qExplicit.Stop(ctx)

	require.True(t, qExplicit.consumerConfig.MemoryStorage,
		"explicit case: consumerConfig.MemoryStorage = false, want true")
	require.Equal(t, 1, qExplicit.consumerConfig.Replicas,
		"explicit case: consumerConfig.Replicas = %d, want 1", qExplicit.consumerConfig.Replicas)

	// Case B: defaults — parti sent 0 to NATS, server inherits to 1
	// and reports 1 in Info — but the cfg parti sent (and stored for
	// recovery) was 0.
	qDefault, err := NewQueue(js, streamName, "qsnap-default", "qsnap.>", handler)
	require.NoError(t, err)
	require.NoError(t, qDefault.Start(ctx))
	defer qDefault.Stop(ctx)

	require.False(t, qDefault.consumerConfig.MemoryStorage)
	require.Equal(t, 0, qDefault.consumerConfig.Replicas)
}
```

### Task 12b — Static recovery snapshot (in internal/ipartition/consumer_test.go)

The file exists and is `package ipartition`. Append to it. The canonical minimum `ConsumerConfig` shape for `NewJSConsumer` is established by the existing test in the same file (around line 30): `PartitionConfig{NumPartitions, SubjectPattern}` + `StreamName` + `ConsumerName` + `Partition`.

- [ ] **Step 1: Append the test**

```go
// TestJSConsumer_RecoverySnapshot_CarriesConsumerOptions verifies
// that ConsumerMemoryStorage and ConsumerReplicas are forwarded
// into JSConsumer.consumerConfig (the snapshot used for recovery).
//
// The Static-layer integration test cannot distinguish "explicit
// Replicas=1" from "default Replicas=0 → server inherits to 1" on
// a single-node rig; this white-box test reads the cfg parti
// actually sent to NATS.
func TestJSConsumer_RecoverySnapshot_CarriesConsumerOptions(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "ISNAP",
		Subjects: []string{"isnap.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	noopHandler := messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	// Case A: explicit options. Field shape copied from the existing
	// TestJSConsumer test in this file (~line 30).
	jcExplicit, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "isnap.{{partition}}",
		},
		StreamName:            "ISNAP",
		ConsumerName:          "isnap-explicit",
		Partition:             0,
		ConsumerMemoryStorage: true,
		ConsumerReplicas:      1,
	}, noopHandler)
	require.NoError(t, err)
	require.NoError(t, jcExplicit.Start(ctx))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = jcExplicit.Stop(ctx)
	})

	require.True(t, jcExplicit.consumerConfig.MemoryStorage,
		"explicit: consumerConfig.MemoryStorage = false, want true")
	require.Equal(t, 1, jcExplicit.consumerConfig.Replicas,
		"explicit: consumerConfig.Replicas = %d, want 1", jcExplicit.consumerConfig.Replicas)

	// Case B: defaults. Parti sent 0 to NATS, server inherits to 1
	// (Info reports 1) — but the stored snapshot remains 0.
	jcDefault, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "isnap.{{partition}}",
		},
		StreamName:   "ISNAP",
		ConsumerName: "isnap-default",
		Partition:    1,
	}, noopHandler)
	require.NoError(t, err)
	require.NoError(t, jcDefault.Start(ctx))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = jcDefault.Stop(ctx)
	})

	require.False(t, jcDefault.consumerConfig.MemoryStorage)
	require.Equal(t, 0, jcDefault.consumerConfig.Replicas)
}
```

- [ ] **Step 2: Verify imports**

Required imports: `context`, `testing`, `time`, `partitesting "github.com/arloliu/parti/v2/partitest"`, `jetstream`, `"github.com/arloliu/parti/v2/partition"`, `require`. The existing file already imports most; only add what's missing.

### Task 12c — Broadcast recovery snapshot (in internal/durable/broadcast_consumer_test.go)

The file exists and is `package durable`. The canonical minimum-config shape for `BroadcastConsumerConfig` + `NewBroadcastConsumer` is at lines 348–370 of the same file.

- [ ] **Step 1: Append the test (concrete code, mirroring the existing test at line 348)**

```go
// TestBroadcastConsumer_RecoverySnapshot_CarriesConsumerOptions
// verifies that ConsumerMemoryStorage and ConsumerReplicas are
// forwarded into BroadcastConsumer.consumerConfig (the recovery
// snapshot). The Broadcast integration test cannot distinguish
// "explicit Replicas=1" from "default Replicas=0 → server inherits
// to 1" on a single-node rig.
func TestBroadcastConsumer_RecoverySnapshot_CarriesConsumerOptions(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "BCSNAP",
		Subjects: []string{"bcsnap.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	noopHandler := func(_ context.Context, _ jetstream.Msg) error { return nil }

	// Case A: explicit options. Match the BroadcastConsumerConfig
	// shape from the existing test (~line 348).
	cfgExplicit := BroadcastConsumerConfig{
		StreamName:            "BCSNAP",
		ConsumerPrefix:        "bcsnap-explicit",
		ConsumerID:            "snap-worker-1",
		WildcardFilter:        "bcsnap.>",
		ConsumerMemoryStorage: true,
		ConsumerReplicas:      1,
	}
	require.NoError(t, cfgExplicit.SetDefaults())
	bcExplicit, err := NewBroadcastConsumer(js, cfgExplicit, noopHandler)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bcExplicit.Close(ctx)
	})
	// startConsumerLoop is the unexported create-and-snapshot path
	// used by the existing test at broadcast_consumer_test.go:361.
	// The public path (UpdateWorkerConsumer) also goes through
	// startConsumerLoop, but calling startConsumerLoop directly
	// avoids the partitions-update ceremony and matches the
	// canonical snapshot test.
	require.NoError(t, bcExplicit.startConsumerLoop(ctx))

	bcExplicit.consumerMu.RLock()
	storedExplicit := bcExplicit.consumerConfig
	bcExplicit.consumerMu.RUnlock()
	require.True(t, storedExplicit.MemoryStorage)
	require.Equal(t, 1, storedExplicit.Replicas)

	// Case B: defaults.
	cfgDefault := BroadcastConsumerConfig{
		StreamName:     "BCSNAP",
		ConsumerPrefix: "bcsnap-default",
		ConsumerID:     "snap-worker-2",
		WildcardFilter: "bcsnap.>",
	}
	require.NoError(t, cfgDefault.SetDefaults())
	bcDefault, err := NewBroadcastConsumer(js, cfgDefault, noopHandler)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bcDefault.Close(ctx)
	})
	require.NoError(t, bcDefault.startConsumerLoop(ctx))

	bcDefault.consumerMu.RLock()
	storedDefault := bcDefault.consumerConfig
	bcDefault.consumerMu.RUnlock()
	require.False(t, storedDefault.MemoryStorage)
	require.Equal(t, 0, storedDefault.Replicas)
}
```

> Field/method shapes here are pulled from the existing snapshot test at `internal/durable/broadcast_consumer_test.go:348-370`: `WildcardFilter` (not `FilterSubject`), `startConsumerLoop` (not `Start`), and `consumerMu`-guarded reads of `consumerConfig`. `BroadcastConsumer` has no exported `Start`; the public entry is `UpdateWorkerConsumer`, but the canonical snapshot test calls `startConsumerLoop` directly. Use `Close` for cleanup (not `Stop`).

### Task 12d — Dynamic recovery snapshot (in internal/durable/worker_consumer_loop_test.go)

The canonical Dynamic construction pattern is in `worker_consumer_loop_test.go` (around lines 44–76), NOT `worker_consumer_test.go`. It directly constructs `&WorkerConsumer{...}`, parses the subject template, calls `UpdateWorkerConsumer`, then inspects `wc.subjects` map. The relevant fields:
- `wc.subjects` is `map[string]*partitionConsumer` (worker_consumer.go:54)
- `partitionConsumer.consumerConfig` is `jetstream.ConsumerConfig` (partition_consumer.go:49) — the recovery-snapshot field

- [ ] **Step 1: Append the test to `internal/durable/worker_consumer_loop_test.go`** (`package durable`)

```go
// TestWorkerConsumer_RecoverySnapshotMatchesCreateConfig verifies
// that the recovery snapshot built in addSubjectLoop (line ~414 of
// worker_consumer.go) carries ConsumerMemoryStorage and
// ConsumerReplicas. The Dynamic integration test only inspects the
// live consumer's Info().Config (the ensurePerSubjectConsumer
// literal at line ~459); this test inspects the SEPARATE literal
// at line ~414 that's stored for recovery. Drift between the two
// would silently change config on recovery; this test catches it.
func TestWorkerConsumer_RecoverySnapshotMatchesCreateConfig(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "WCSNAP",
		Subjects: []string{"wcsnap.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:            "WCSNAP",
		ConsumerPrefix:        "wcsnap",
		SubjectTemplate:       "wcsnap.{{.PartitionID}}",
		BatchSize:             2,
		ConsumerMemoryStorage: true,
		ConsumerReplicas:      1,
	}
	require.NoError(t, cfg.SetDefaults())

	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	handler := messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         handler,
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}

	parts := []types.Partition{{Keys: []string{"p0"}}}
	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "wcsnap-worker", parts))
	t.Cleanup(func() { _ = wc.Close(ctx) })

	// Wait briefly for the per-subject partition consumer to register.
	subject := "wcsnap.p0"
	deadline := time.Now().Add(3 * time.Second)
	var pc *partitionConsumer
	for time.Now().Before(deadline) {
		wc.mu.RLock()
		pc = wc.subjects[subject]
		wc.mu.RUnlock()
		if pc != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotNil(t, pc, "partition consumer for %s not registered after 3s", subject)

	// The load-bearing assertion: the snapshot stored for recovery
	// (built by addSubjectLoop's literal at line ~414) has both
	// new fields set.
	require.True(t, pc.consumerConfig.MemoryStorage,
		"recovery snapshot: MemoryStorage = false, want true (likely worker_consumer.go:414 missed the field)")
	require.Equal(t, 1, pc.consumerConfig.Replicas,
		"recovery snapshot: Replicas = %d, want 1", pc.consumerConfig.Replicas)
}
```

> The field-access pattern (`wc.subjects[subject]` after grabbing `wc.mu.RLock()`) is taken from the existing test at `worker_consumer_loop_test.go:76`. If the lock name or map field name differs, match the existing test verbatim.

- [ ] **Step 2: Verify imports**

Required imports: `context`, `testing`, `text/template`, `time`, `partitesting`, `jetstream`, `"github.com/arloliu/parti/v2/types"`, `require`. The existing `worker_consumer_loop_test.go` likely already imports most.

### Step (all of 12a–12d): Run + commit

```bash
go test ./consumer/ ./internal/ipartition/... ./internal/durable/... -run 'RecoverySnapshot|RecoverySnapshotMatchesCreateConfig' -v
git add consumer/queue_test.go internal/ipartition/consumer_test.go internal/durable/broadcast_consumer_test.go internal/durable/worker_consumer_loop_test.go
git commit -m "test: white-box recovery-snapshot coverage for new consumer options

Per consumer type, tests the snapshot stored for recovery has both
new fields set. The integration tests in Tasks 7-10 cannot
distinguish 'explicit Replicas=1' from 'default Replicas=0 → server
inherits to 1' on single-node embedded NATS, so these white-box
tests read the cfg parti actually sent to NATS. For Dynamic, the
test specifically catches drift between the duplicated literals at
worker_consumer.go:414 and :459."
```

---

## Task 13: CHANGELOG + final verification + push

- [ ] **Step 1: Add an entry under the "Unreleased" / "Added" section in `CHANGELOG.md`**

```markdown
### Added

- `consumer.WithConsumerMemoryStorage(bool)` and
  `consumer.WithConsumerReplicas(int)` — universal options that forward
  to `jetstream.ConsumerConfig.MemoryStorage` and `.Replicas`. Combined
  (`MemoryStorage=true`, `Replicas=1`) they reduce per-partition
  `block_write_iops` by ~99 % on the IOPS-investigation rig; defaults
  preserve existing behavior. See `docs/plans/iops-investigation/findings.md`
  §2 for the operator recommendation and §4 for the decision tree.
```

- [ ] **Step 2: Run the full CI gate locally**

Run: `make ci`
Expected: lint + test + coverage all PASS.

- [ ] **Step 3: Sanity-check the diff size**

Run: `git diff --stat origin/main`
Expected: ~12–18 files touched, mostly small. The test files are the largest. Total LoC added: ~400–600, mostly Godoc and the integration / white-box tests.

- [ ] **Step 4: Sanity-check the public API surface**

Run: `git diff origin/main -- 'consumer/*.go' | rg '^\+[^+]' | rg 'func (With|New)' | head -20`
Expected: only `WithConsumerMemoryStorage` and `WithConsumerReplicas` are net-new public symbols. No accidental renames or signature changes to existing constructors.

- [ ] **Step 5: Commit CHANGELOG**

```bash
git add CHANGELOG.md
git commit -m "docs(changelog): note WithConsumerMemoryStorage + WithConsumerReplicas"
```

- [ ] **Step 6: Push branch + open PR (only after post-impl-review signs off)**

```bash
git push -u origin worktree-consumer-options-api
gh pr create --title "consumer: WithConsumerMemoryStorage + WithConsumerReplicas options" --body "$(cat <<'EOF'
## Summary

Two new universal `Option` functions on parti's public consumer API
forward to `jetstream.ConsumerConfig.MemoryStorage` and `.Replicas`.
Combined, they implement the M2.B configuration validated by the
IOPS investigation (~99 % reduction in `block_write_iops` at high
partition count, with the JetStream message log staying durable).

- `WithConsumerMemoryStorage(true)`: consumer state in memory.
- `WithConsumerReplicas(1)`: disable consumer-state raft replication.
- Defaults (false / 0) preserve existing behavior.
- Pass-through validation — NATS rejects `Replicas > stream.Replicas`
  with error 10126; parti surfaces it verbatim.

## Tradeoff documentation

Each `With*` Godoc explains:
- What it does and when to use it.
- Durability impact (at-least-once redelivery on the relevant failure class).
- Live-edit status — `Replicas` is editable via `nats consumer edit`;
  `MemoryStorage` requires delete + recreate.
- Reference to `docs/plans/iops-investigation/findings.md` §2 (the
  measured impact and operator decision tree).

## Test plan

- [x] `make lint` — 0 issues
- [x] `make test` — all unit tests pass (race + CGO disabled)
- [x] `TestWithConsumer*` — option-application unit tests
- [x] `TestDynamic_ConsumerOptions_AppliedToLiveConsumer` — end-to-end (embedded NATS)
- [x] `TestQueue_ConsumerOptions_AppliedToLiveConsumer` — end-to-end
- [x] `TestStatic_ConsumerOptions_AppliedToLiveConsumer` — end-to-end
- [x] `TestBroadcast_ConsumerOptions_AppliedToLiveConsumer` — end-to-end
- [x] `TestQueue_ConsumerReplicasExceedsStream_Surfaces10126` — pass-through validation
- [x] `*_RecoverySnapshot_CarriesConsumerOptions` — white-box snapshot coverage

EOF
)"
```

---

## Self-review checklist

Run this before declaring the plan ready for re-review.

**1. Spec coverage:** every behavior in `findings.md` §2 (the recommendation) and §8 (the experiment-derived semantics) is either implemented (the two options) or explicitly out-of-scope (rescale helper, harness interceptor cleanup, placement option). ✓

**2. Placeholder scan:** no "TBD" / "implement later" / "add appropriate error handling" appear. Every code block is pasteable; every grep command produces a usable line number. The only `>` notes are clearly-bounded "if the existing signature differs, look here" caveats — they don't hide work. ✓

**3. Type consistency:** parti-layer field names are `ConsumerMemoryStorage` (bool) and `ConsumerReplicas` (int) everywhere (`CommonConfig`, `WorkerConsumerConfig`, `BroadcastConsumerConfig`, `ipartition.ConsumerConfig`). JetStream-level names are unprefixed `MemoryStorage` / `Replicas` (matching `jetstream.ConsumerConfig`). The prefix asymmetry is intentional — parti-layer names are namespaced to avoid colliding with potential future stream-level options on the same struct. ✓

**4. Per-consumer-type coverage:** all four types plumbed (Tasks 3–6), all four types live-tested (Tasks 7–10). The Dynamic-path correction (worker_consumer.go's TWO literals, not the ipartition path) is explicit in Task 3 step 3. The other three types each have a single literal, called out in §"Architectural map." ✓

**5. Recovery-path coverage:** the snapshot at queue.go:349, broadcast_consumer.go:211, ipartition/consumer.go:241 all reuse the same literal as the create-time cfg, so Tasks 4–6 cover them implicitly. Dynamic is the exception (two distinct literals at lines 414 + 459), which is why Task 3 step 5 explicitly updates BOTH. Task 12's white-box snapshot test is the load-bearing assertion that the stored snapshot has the new fields. ✓

**6. Validation strategy:** pass-through, surface error 10126 verbatim. Documented in Godoc (Task 2) AND covered by an explicit test (Task 11). ✓

**7. Default-vs-explicit Replicas:** the single-node embedded NATS limitation is acknowledged. The integration tests can only assert `Replicas=1` end-to-end on the resolved value, but the white-box snapshot test (Task 12) reads the *sent* cfg and distinguishes `Replicas=0` (default) from `Replicas=1` (explicit). ✓

**8. Live-edit asymmetry:** `MemoryStorage` is explicitly called out as NOT live-editable in both Godoc blocks (Task 2). `Replicas` is called out as IS live-editable. ✓

**9. Out-of-scope items:** listed in §Background. Each is justified (deferred for separate PR, gated on other work, or genuinely future-work). The pre-existing duplication of the Dynamic literals at lines 414+459 is called out as a known smell for a future refactor PR. ✓

---

## Execution handoff

Plan v3 — revised per the v2 plan-review (`tmp/consumer-options-api_v2_review.md`) findings:

- v1 P0 RESOLVED in v2 and preserved here (Dynamic plumbing now correctly targets `durable.WorkerConsumer`'s two literals in worker_consumer.go at lines 414 + 459).
- v1 P1 RESOLVED (Static + Broadcast tests are now live integration tests, not skipped).
- v1 P1 RESOLVED (durable config file ownership: `WorkerConsumerConfig` in `internal/durable/config.go:101`, `BroadcastConsumerConfig` in `internal/durable/broadcast_config.go:36`).
- v2 P1 RESOLVED (test snippets compile as written):
  - Task 7 Dynamic test now uses `[]types.Partition{{Keys: []string{"p0"}}}` (matching the canonical Update signature) and checks `lister.Err()` after iteration.
  - Task 8 Queue test uses `package consumer` (white-box) with unqualified names — the actual file is `package consumer`, not `_test`.
  - Task 9 Static test uses `{{partition}}` placeholder (Static's grammar) instead of `{{.PartitionID}}` (Dynamic's grammar), and `package consumer` style.
  - Task 10 Broadcast test file is now `consumer/broadcast_test.go` (NEW) with concrete code, `package consumer`, no qualifier.
- v2 P1 RESOLVED (recovery snapshot coverage): Task 12 is now split into 12a–12d, with each white-box test living in the package that owns the unexported `consumerConfig` field — Queue in `consumer/`, Static in `internal/ipartition/`, Broadcast in `internal/durable/`, Dynamic in `internal/durable/`. The Dynamic test (12d) specifically catches drift between the duplicated literals at lines 414 + 459.
- v2 P1 RESOLVED (NATS retention-policy validation): `WithConsumerReplicas` Godoc in Task 2 now documents both rules — `Replicas ≤ stream.Replicas` for LimitsPolicy (default; rejected with 10126) AND nonzero Replicas must equal stream.Replicas for InterestPolicy/WorkQueuePolicy. The Background section explains why the M2.B recipe applies to Dynamic/Static/Broadcast but NOT Queue on WorkQueuePolicy streams.
- v2 P2 RESOLVED (Task 11 tightened): the `if err == nil` fallback is gone; NewQueue is expected to succeed (it doesn't call NATS), and the 10126 error is asserted at Start time.

Next step: re-dispatch `plan-review` for a third pass to confirm v3 closes the v2 P1/P2 findings without introducing new issues. After verdict is "ready," execute inline (Tasks 1–13 are sequential, each ≤ 30 min, no parallelism win from subagents).

---

## v4 revision notes

Plan v4 — addresses v3 plan-review findings (`tmp/consumer-options-api_v3_review.md`):

- **v3 P1 RESOLVED (Tasks 8/9 not pasteable):** all integration-test snippets now use `partitesting.StartEmbeddedNATS` (the helper Queue/Broadcast/etc. actually use, NOT `testutil.StartEmbeddedNATS`). Task 9 (Static) explicitly enumerates the imports the bare `static_test.go` needs. Task 10 (Broadcast) NEW file imports cleaned up — no `testutil`.
- **v3 P1 RESOLVED (Tasks 12b/c/d under-specified):** all four snapshot tests are now concrete code with the exact field shapes pulled from the existing test patterns the reviewer cited:
  - 12b uses the `ConsumerConfig{PartitionConfig: {NumPartitions, SubjectPattern}, StreamName, ConsumerName, Partition}` shape from `internal/ipartition/consumer_test.go:30`.
  - 12c uses the `BroadcastConsumerConfig{StreamName, ConsumerPrefix, FilterSubject}` shape from `internal/durable/broadcast_consumer_test.go:348`.
  - 12d uses the direct `&WorkerConsumer{...}` + `UpdateWorkerConsumer` + `wc.subjects[subject]` pattern from `internal/durable/worker_consumer_loop_test.go:44–76` (NOT `worker_consumer_test.go`, which has no relevant helper).
- **v3 P1 RESOLVED (retention-policy caveat too narrow):** Background section + `WithConsumerReplicas` Godoc now state the rule applies to "any parti consumer used on an InterestPolicy or WorkQueuePolicy stream," with `Queue` cited as the most common WorkQueuePolicy user and Dynamic/Static noted as also supporting WorkQueuePolicy (Broadcast is explicitly incompatible).
- **v3 P2 RESOLVED (Godoc constructor wording):** `WithConsumerReplicas` Godoc now says errors surface "when the underlying JetStream consumer is created or updated — Queue/Static/Broadcast at Start, Dynamic at Update" — neutral wording that fits all four types.
- **v3 P2 (WorkQueuePolicy test):** acknowledged as a non-blocking gap (parti's pass-through validation surfaces NATS's errors regardless of retention policy). Not added in v4 to keep the test surface focused; the gap is documented in §Out-of-scope.

---

## v5 revision notes

Plan v5 — addresses v4 plan-review findings (`tmp/consumer-options-api_v4_review.md`):

- **v4 P1 RESOLVED (Task 12c wrong internal API):** Broadcast snapshot test now uses `WildcardFilter` (not `FilterSubject`), `bc.startConsumerLoop(ctx)` (not the nonexistent `bc.Start`), and reads `bc.consumerConfig` under `bc.consumerMu.RLock()` — matching the canonical pattern at `internal/durable/broadcast_consumer_test.go:348-370`. `ConsumerID` set explicitly per case for deterministic durable names.
- **v4 P1 RESOLVED (Task 9 Static imports drift):** removed unused `strings` import from the import block; removed "nats.go" from the import prose since the snippet uses only `jetstream` (no top-level `nats` package symbols). The import block now compiles as written.
- **v4 P2 RESOLVED (Tech Stack helper name):** updated from stale `internal/testutil` to `github.com/arloliu/parti/v2/partitest` (imported as `partitesting`) — matches what every test snippet in the plan actually uses.
- **v4 P2 RESOLVED (stale `worker_consumer_test.go` cross-refs):** the Task 12 owner table and the Dynamic integration-test note now point at `worker_consumer_loop_test.go` (consistent with the file-structure table at line 78 and Task 12d itself).
- **v4 P2 RESOLVED (`CommonConfig.ConsumerReplicas` "from the constructor"):** the field comment now matches the `WithConsumerReplicas` Godoc — "surfaced verbatim when the underlying consumer is created or updated (Queue/Static/Broadcast at Start, Dynamic at Update)."
- **v4 P2 deferred:** the WorkQueuePolicy-rule note in §Out-of-scope sits in §v4 revision-notes rather than the body §Out-of-scope list. Left as-is since (a) the v5 reviewer can see both, and (b) the §Out-of-scope list is for *scope decisions* (what we won't build), not *test gaps* (what we won't test). Test gaps belong in handoff notes.

---

## v6 revision notes

Plan v6 — addresses v5 plan-review findings (`tmp/consumer-options-api_v5_review.md`):

- **v5 P1 RESOLVED (Task 7 Dynamic imports conflict):** `consumer/dynamic_test.go` already imports `github.com/arloliu/parti/v2/partitest` bare (used by existing `partitest.NopOwnershipResolver{}`). Task 7 snippet now calls `partitest.StartEmbeddedNATS(t)` and the import instruction explicitly says NOT to add a `partitesting` alias (doing so would import the same path twice and fail to compile). Net-new imports for Task 7 are only `strings` and `types`.
- **v5 P2 RESOLVED (WorkQueuePolicy test-gap note):** added an explicit "Known test gap" sub-section to §Out of scope explaining why no dedicated WorkQueuePolicy retention-rule test is included (pass-through; testing NATS, not parti). The historical pointer in v4 revision notes is now accurate.
- **Per-file partitest convention codified:** the Tech Stack section now states the rule — match whichever convention the target file already uses. `consumer/dynamic_test.go` is bare; `consumer/queue_test.go` and every `internal/` test file is aliased as `partitesting`; `consumer/static_test.go` (currently bare) and the NEW `consumer/broadcast_test.go` use bare to match `dynamic_test.go`. Each task's snippet matches its target file.
