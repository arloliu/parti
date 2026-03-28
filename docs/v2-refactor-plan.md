# Parti v2 Refactoring Plan

## Goal

Restructure the public package layout so that library users interact with a clean,
non-confusing API. Break backward compatibility where needed (v2 module path).

## v2 Public API Principles

1. **Users import `consumer/` to consume.** All four consumer types live here.
   No other package is needed for consuming messages.
2. **Users import `partition/` only to publish** to statically partitioned subjects,
   or to use low-level core-NATS static routing helpers (Subscriber).
3. **Internal packages must not leak through public signatures.** No public struct
   field, method parameter, or return type may reference `internal/`.
4. **One concept gets one public type.** `MessageHandler` is defined once in
   `consumer/`. Config types are owned by `consumer/` — `internal/durable/` and
   `internal/partition/` receive translated internal config, not public types.
5. **Defaults are explicit.** Tuning-relevant defaults (AckWait, MaxDeliver,
   MaxAckPending, InactiveThreshold, FetchBatchSize, FetchMaxWait) are exported
   as named constants in `consumer/` so advanced users can reference them.

---

## Target Layout

```
github.com/arloliu/parti/v2/
│
├── (root)                      Manager, Config, Options, re-exported type aliases
│
├── types/                      Shared contracts (leaf package — no project imports)
│
├── consumer/                   Unified consumer API (THE user-facing package)
│   ├── handler.go              Single MessageHandler definition
│   └── defaults.go             Tuning-relevant default constants
│
├── partition/                  Static partition routing (publish + low-level subscribe)
│   ├── publisher.go            NewPublisher   (core NATS, fire-and-forget)
│   ├── js_publisher.go         NewJSPublisher (JetStream, with ack)
│   ├── subscriber.go           NewSubscriber  (core NATS, push-based, no JetStream)
│   └── config.go               PartitionConfig
│
├── source/                     Partition sources (API unchanged, import path adds /v2)
├── strategy/                   Assignment strategies (API unchanged, import path adds /v2)
├── jsutil/                     JetStream utilities (API unchanged, import path adds /v2)
├── kvutil/                     KV utilities (API unchanged, import path adds /v2)
├── partitest/                  Test helpers (renamed from testing/)
│
└── internal/
    ├── durable/                ← renamed from public subscription/
    ├── partition/              ← consumer-side of public partition/ (JSConsumer, core, pattern)
    ├── election/
    ├── heartbeat/
    ├── stableid/
    ├── assignment/
    ├── hooks/
    ├── hash/
    ├── logging/
    ├── metrics/
    ├── natsutil/
    └── testutil/
```

## What Changes

| v1 Public                                | v2 Status                  | Reason                                      |
|------------------------------------------|----------------------------|---------------------------------------------|
| `subscription/`                          | `internal/durable/`        | Implementation detail of consumer.Dynamic/Broadcast |
| `subscription.MessageHandler`            | Deleted                    | Use consumer.MessageHandler                 |
| `subscription.WorkerConsumer`            | `internal/durable`         | Wrapped by consumer.Dynamic                 |
| `subscription.BroadcastConsumer`         | `internal/durable`         | Wrapped by consumer.Broadcast               |
| `subscription.ClaimResolver`             | `internal/durable`         | Implementation detail                       |
| `partition.JSConsumer`                   | `internal/partition`       | Wrapped by consumer.Static                  |
| `partition.MessageHandler`               | Deleted                    | Use consumer.MessageHandler                 |
| `partition.NATSMessageHandler`           | `internal/partition`       | Only used by Subscriber internals           |
| `partition.Publisher`                    | **Stays** in `partition/`  | User-facing publisher                       |
| `partition.JSPublisher`                  | **Stays** in `partition/`  | User-facing publisher                       |
| `partition.Subscriber`                   | **Stays** in `partition/`  | Core-NATS push subscriber — no overlap with JetStream consumer/ |
| `partition.PartitionPublisher` interface | **Stays** in `partition/`  | Useful for mocking                          |
| `testing/`                               | `partitest/`               | Avoid shadowing stdlib testing              |
| `consumer.MessageHandler`                | **THE** canonical handler  | Single definition, no duplicates            |

## Leaked Types to Absorb into consumer/

These `subscription` types currently leak through `consumer` configs/methods:

| Leaked Type                            | Current Usage in consumer/                  | v2 Resolution                          |
|----------------------------------------|---------------------------------------------|----------------------------------------|
| `subscription.ProcessingGateConfig`    | `DynamicConfig.ProcessingGate`              | Define `consumer.ProcessingGateConfig` |
| `subscription.ResolverConfig`          | `DynamicConfig.Resolver`                    | Define `consumer.ResolverConfig`       |
| `subscription.ResolverMetrics`         | `Dynamic.SetResolverMetrics()`              | Define `consumer.ResolverMetrics`      |
| `subscription.GateMetrics`             | `consumer.WithGateMetrics()` option         | Define `consumer.GateMetrics`          |
| `subscription.RetryConfig`             | `BroadcastConfig`, `DynamicConfig`          | Already have `consumer.RetryConfig`    |
| `subscription.MessageHandler`          | Adapter in broadcast.go, dynamic.go         | Remove adapter, use func type          |

## Phases

Each phase is one or more atomic commits. Phases are ordered by dependency —
later phases depend on earlier ones compiling successfully.

---

### Phase 0: Freeze Public API Decisions

**Commit:** (no commit — decision checkpoint only)

Before writing any code, confirm these decisions are final:

- [ ] Run `go doc` on `subscription/` and `partition/` — export every symbol
- [ ] Classify each as: keep (public), move (internal), replace (consumer/), delete
- [ ] Confirm no public signature in `consumer/` will reference `internal/` after migration
- [ ] Confirm `partition.Subscriber` stays public (justified: core-NATS push subscriber,
      no overlap with JetStream consumer/; only used in its own package + tests)
- [ ] Confirm `WorkerConsumerUpdater` stays (justified: interface uses `types.Partition`,
      not subscription types; 4 implementations, 9+ integration test sites)
- [ ] Decide which subscription default constants to preserve in `consumer/defaults.go`
      (recommended: `DefaultAckWait`, `DefaultMaxDeliver`, `DefaultMaxAckPending`,
       `DefaultInactiveThreshold`, `DefaultFetchBatchSize`, `DefaultFetchMaxWait`)
- [ ] Sign off on this plan

---

### Phase 1: Module Path + Go Version

**Commit:** `chore: bump module path to v2`

- [ ] Update `go.mod`: `module github.com/arloliu/parti/v2`
- [ ] Update ALL import paths project-wide: `parti` → `parti/v2`
- [ ] Update examples/, test/ imports
- [ ] Verify: `go build ./...` passes

---

### Phase 2: Rename `testing/` → `partitest/`

**Commit:** `refactor: rename testing/ to partitest/`

- [ ] `git mv testing/ partitest/`
- [ ] Update all imports: `parti/v2/testing` → `parti/v2/partitest`
- [ ] Verify: `go build ./...` passes

---

### Phase 3: Move `subscription/` → `internal/durable/`

This is the largest phase. Split into sub-steps.

#### Phase 3a: Absorb leaked types into consumer/

**Commit:** `refactor(consumer): define own config types for ProcessingGate, Resolver, metrics`

Before moving subscription/ internal, define consumer-owned copies of leaked types.
These will initially be type aliases (or thin copies) so consumer/ compiles against
both old and new during the transition.

**Config ownership rule:** After this phase, `consumer/` is the source of truth for
all public config types. `internal/durable/` will receive translated internal config
structs — it must never appear in a public signature. Add field-parity tests to
catch future drift between consumer config and internal config.

- [ ] `consumer/gate_config.go`: Define `ProcessingGateConfig` struct
- [ ] `consumer/resolver_config.go`: Define `ResolverConfig` struct
- [ ] `consumer/metrics.go`: Define `GateMetrics`, `ResolverMetrics` interfaces
- [ ] `consumer/defaults.go`: Export tuning-relevant defaults as named constants
      (`DefaultAckWait`, `DefaultMaxDeliver`, `DefaultMaxAckPending`,
       `DefaultInactiveThreshold`, `DefaultFetchBatchSize`, `DefaultFetchMaxWait`)
- [ ] Update `consumer/config.go`: Use consumer-owned types in DynamicConfig, BroadcastConfig
- [ ] Update `consumer/options.go`: Use consumer-owned types in option functions
- [ ] Update `consumer/dynamic.go`: Map consumer types → subscription types internally
- [ ] Update `consumer/broadcast.go`: Same
- [ ] Add field-parity tests: consumer config ↔ internal config mapping
- [ ] Verify: `go build ./...` && `go test ./consumer/...` pass

#### Phase 3b: Remove handler adapters

**Commit:** `refactor(consumer): remove MessageHandler adapter layer`

- [ ] Make `internal/durable` (still named subscription/ at this point) accept
      `func(context.Context, jetstream.Msg) error` instead of its own MessageHandler
- [ ] Remove `toSubscriptionHandler()` from consumer/dynamic.go
- [ ] Remove `toSubscriptionHandler()` from consumer/broadcast.go
- [ ] Delete `subscription/handler.go` (the interface + HandlerFunc)
- [ ] Verify: `go build ./...` && `go test ./...` pass

#### Phase 3c: Move the package

**Commit:** `refactor: move subscription/ to internal/durable/`

- [ ] `git mv subscription/ internal/durable/`
- [ ] Rename package declaration: `package subscription` → `package durable`
- [ ] Update all internal imports
- [ ] Verify: `go build ./...` passes
- [ ] Run full test suite: `make test-all`

---

### Phase 4: Split `partition/` — move consumer-side to `internal/partition/`

#### Phase 4a: Remove partition.MessageHandler adapter

**Commit:** `refactor(consumer): remove partition MessageHandler adapter`

- [ ] Make internal partition JSConsumer accept `func(context.Context, jetstream.Msg) error`
- [ ] Remove `toPartitionHandler()` from consumer/static.go
- [ ] Remove partition.MessageHandler, partition.MessageHandlerFunc (keep NATSMessageHandler
      internal if Subscriber still needs it)
- [ ] Verify: `go build ./...` && `go test ./...` pass

#### Phase 4b: Move consumer-side files

**Commit:** `refactor: move partition consumer-side to internal/partition/`

Files to move to `internal/partition/`:
- [ ] `partition/js_consumer.go` → `internal/partition/js_consumer.go`
- [ ] `partition/partition_core.go` → `internal/partition/partition_core.go`
- [ ] `partition/pattern.go` → `internal/partition/pattern.go`
- [ ] `partition/hasher.go` → `internal/partition/hasher.go`
- [ ] `partition/key_dispatcher.go` → `internal/partition/key_dispatcher.go`
- [ ] `partition/handler.go` → `internal/partition/handler.go` (NATSMessageHandler only)
- [ ] Move corresponding test files

Dependency resolution:
- [ ] Public `partition/publisher.go` imports `internal/partition` for partitionCore
- [ ] Public `partition/subscriber.go` imports `internal/partition` for partitionCore
- [ ] `consumer/static.go` imports `internal/partition` for JSConsumer
- [ ] Update all import paths
- [ ] Verify: `go build ./...` && `go test ./...` pass

#### Phase 4c: Clean public partition/ surface

**Commit:** `refactor: clean partition/ package — publishers and routing only`

Files that STAY in public `partition/`:
- [ ] `partition/publisher.go`
- [ ] `partition/js_publisher.go`
- [ ] `partition/subscriber.go` (core-NATS push subscriber, justified: no JetStream
      overlap, used for StatefulSet fire-and-forget patterns)
- [ ] `partition/config.go` (PartitionConfig — shared by publishers + subscriber)
- [ ] `partition/options.go` (publisher/subscriber options only — remove consumer options)
- [ ] `partition/errors.go`
- [ ] `partition/doc.go` (update: remove references to JSConsumer)
- [ ] `partition/validate.go`
- [ ] Remove consumer option functions from `partition/options.go`
- [ ] Remove `ConsumerConfig`, `ConsumerOption`, `NewConsumerConfig` from public surface
- [ ] Verify: `go build ./...` && `go test ./...` pass

---

### Phase 4.5: Godoc Review Checkpoint

**Commit:** (no commit — review only)

The package story has now fundamentally changed. Review godoc BEFORE continuing
to catch issues early rather than at Phase 9.

- [ ] `go doc ./consumer/` — verify single MessageHandler, no subscription/partition leaks
- [ ] `go doc ./partition/` — verify only publisher/subscriber/config, no JSConsumer
- [ ] `go doc .` — verify root re-exports are clean
- [ ] Verify no public type references `internal/` packages
- [ ] Update one example (e.g. `examples/basic/`) to smoke-test the new import story

---

### Phase 5: Unify MessageHandler

**Commit:** `refactor: single MessageHandler in consumer package`

At this point, `subscription.MessageHandler` and `partition.MessageHandler` are already
deleted (Phases 3b, 4a). This step cleans up any remaining duplication.

- [ ] Confirm `consumer.MessageHandler` is the only public handler interface
- [ ] Confirm `consumer.MessageHandlerFunc` is the only public adapter
- [ ] Update doc.go across packages to reference `consumer.MessageHandler`
- [ ] Verify: `go build ./...` passes

---

### Phase 6: Rename `partIdx` → `partitionIndex` in consumer.NewStatic

**Commit:** `refactor(consumer): rename partIdx to partitionIndex`

- [ ] Update `consumer/static.go`: parameter name `partIdx` → `partitionIndex`
- [ ] Update `consumer/config.go`: `StaticConfig.PartIdx` → `StaticConfig.PartitionIndex`
- [ ] Update all references in consumer/ and tests
- [ ] Verify: `go build ./...` && `go test ./consumer/...` pass

---

### Phase 7: Audit root package re-exports

**Commit:** `refactor: audit and clean root package re-exports for v2`

All 14 sentinel errors in `errors.go` come from `types/` — they are NOT affected
by the subscription/partition internalization. No errors need removal.

All type aliases in `types.go` come from `types/` — also unaffected.

`WorkerConsumerUpdater` and `CompositeConsumerUpdater` STAY. They are deeply
embedded: root interface → Manager field → option → 4 implementations (consumer.Dynamic,
consumer.Broadcast, durable.WorkerConsumer, durable.BroadcastConsumer) → composite
fan-out → 9+ integration test sites → simulation code → 3 doc files. The interface
is defined in terms of `[]Partition` (from `types/`), not subscription types, so
it has no dependency on internal packages.

- [ ] Verify `types.go`: Confirm all aliases still point to `types/` — no changes needed
- [ ] Verify `errors.go`: Confirm all errors still point to `types/` — no changes needed
- [ ] Verify `WorkerConsumerUpdater`: Confirm signature uses only `types.Partition` — no changes needed
- [ ] Verify: `go build ./...` passes

---

### Phase 8: Update examples and documentation

**Commit:** `docs: update examples and docs for v2 layout`

- [ ] Update `examples/basic/main.go` with v2 imports
- [ ] Add example showing consumer + partition.Publisher together
- [ ] Update `consumer/doc.go` with publisher pairing table
- [ ] Update `README.md` for v2
- [ ] Update `.github/copilot-instructions.md` project structure section

---

### Phase 9: Final validation

**Commit:** (no commit — validation only)

- [ ] `go vet ./...`
- [ ] `golangci-lint run`
- [ ] `make test-all`
- [ ] Review godoc output for all public packages
- [ ] Confirm no `subscription` or `partition.JSConsumer` in public API
- [ ] Confirm user import patterns match the v2 design

---

## Dependency Order Diagram

```
Phase 0 (freeze API decisions)
  │
  ▼
Phase 1 (module path)
  │
  ▼
Phase 2 (rename testing → partitest)
  │
  ▼
Phase 3a (absorb leaked types + defaults) ──► Phase 3b (remove handler adapters) ──► Phase 3c (move subscription → internal/durable)
                                                                                         │
                                                                                         ▼
                                              Phase 4a (remove partition handler) ──► Phase 4b (move files) ──► Phase 4c (clean partition/ surface)
                                                                                                                    │
                                                                                                                    ▼
                                                                                                              Phase 4.5 (godoc checkpoint + smoke example)
                                                                                                                    │
                                                                                                                    ▼
                                                                                                              Phase 5 (unify handler)
                                                                                                                    │
                                                                                                                    ▼
                                                                                                              Phase 6 (rename partIdx)
                                                                                                                    │
                                                                                                                    ▼
                                                                                                              Phase 7 (audit re-exports)
                                                                                                                    │
                                                                                                                    ▼
                                                                                                              Phase 8 (docs/examples)
                                                                                                                    │
                                                                                                                    ▼
                                                                                                              Phase 9 (final validation)
```

## Notes

- Each phase MUST compile and pass tests before moving to the next
- Phase 0 (freeze decisions) prevents refactoring into a moving target
- Phases 3 and 4 are the riskiest — they touch the most files
- Phase 3a (absorb leaked types) is intentionally done BEFORE the move so that
  consumer/ never has a broken compile state
- The handler adapter removal (3b, 4a) is done BEFORE the package moves so
  we delete the interfaces while they're still easy to find
- Phase 4 is split into three sub-commits (4b move, 4c clean surface) for
  reviewability
- Phase 4.5 (godoc checkpoint) catches package story issues EARLY rather than
  at Phase 9 when they're expensive to fix
- `partition/` public surface shrinks but does NOT disappear — publishers and
  core-NATS Subscriber stay
- `WorkerConsumerUpdater` is NOT removed — it uses `types.Partition`, not
  subscription types, and is deeply embedded (4 implementations, 9+ test sites)
- Root re-exported errors and type aliases are NOT affected — they all come
  from `types/`, not from subscription/ or partition/
- Config ownership: after Phase 3a, `consumer/` owns all public config schemas;
  internal packages receive translated config. Field-parity tests guard against drift.
- Six tuning-relevant defaults are promoted to `consumer/` as named constants;
  the rest become internal (users never needed to reference them directly)

---

## Breaking Changes & Migration Guide

### Module Path

| v1 | v2 |
|----|-----|
| `github.com/arloliu/parti` | `github.com/arloliu/parti/v2` |

All import paths change. This is a Go modules major version bump.

```diff
- import "github.com/arloliu/parti"
+ import "github.com/arloliu/parti/v2"

- import "github.com/arloliu/parti/consumer"
+ import "github.com/arloliu/parti/v2/consumer"
```

---

### Package Removals

#### `subscription/` → removed from public API

The entire `subscription/` package moves to `internal/durable/`. Users MUST
stop importing `github.com/arloliu/parti/subscription`.

**v1 direct users of subscription types** (rare — most users go through `consumer/`):

```diff
- import "github.com/arloliu/parti/subscription"
-
- wc, _ := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{...})
- bc, _ := subscription.NewBroadcastConsumer(js, subscription.BroadcastConsumerConfig{...})
+ import "github.com/arloliu/parti/v2/consumer"
+
+ dc, _ := consumer.NewDynamic(js, stream, prefix, template, handler, ...opts)
+ bc, _ := consumer.NewBroadcast(js, stream, prefix, subject, handler, ...opts)
```

#### `partition/` consumer-side → removed from public API

`partition.JSConsumer`, `partition.PartitionJSConsumer`, and `partition.NewJSConsumer`
move to `internal/partition/`. `partition.NewConsumerConfig` is also removed.

```diff
- import "github.com/arloliu/parti/partition"
-
- jsc, _ := partition.NewJSConsumer(js, streamName, partition.ConsumerConfig{...})
+ import "github.com/arloliu/parti/v2/consumer"
+
+ sc, _ := consumer.NewStatic(js, stream, name, pattern, numPart, partIdx, handler, ...opts)
```

**Stays in `partition/`:** `Publisher`, `JSPublisher`, `Subscriber`, `PartitionPublisher`
interface, `PartitionConfig`, all publisher/subscriber options, `StatefulSetIndex`,
`StatefulSetConsumerName`, and all error sentinels.

#### `testing/` → renamed to `partitest/`

```diff
- import "github.com/arloliu/parti/testing"
-
- ns := testing.StartEmbeddedNATS(t)
+ import "github.com/arloliu/parti/v2/partitest"
+
+ ns := partitest.StartEmbeddedNATS(t)
```

---

### Deleted Types — Full List

#### subscription types (all deleted from public API)

| Deleted Symbol | v2 Replacement |
|---|---|
| `subscription.MessageHandler` | `consumer.MessageHandler` |
| `subscription.HandlerFunc` | `consumer.MessageHandlerFunc` |
| `subscription.WorkerConsumer` | `consumer.Dynamic` (wraps it) |
| `subscription.WorkerConsumerConfig` | `consumer.DynamicConfig` |
| `subscription.NewWorkerConsumer()` | `consumer.NewDynamic()` |
| `subscription.NewWorkerConsumerConfig()` | Defaults baked into `consumer.NewDynamic()` |
| `subscription.BroadcastConsumer` | `consumer.Broadcast` (wraps it) |
| `subscription.BroadcastConsumerConfig` | `consumer.BroadcastConfig` |
| `subscription.NewBroadcastConsumer()` | `consumer.NewBroadcast()` |
| `subscription.NewBroadcastConsumerConfig()` | Defaults baked into `consumer.NewBroadcast()` |
| `subscription.ClaimBasedResolver` | Internal (auto-created by `consumer.Dynamic`) |
| `subscription.NewClaimBasedResolver()` | Internal |
| `subscription.ProcessingGateConfig` | `consumer.ProcessingGateConfig` |
| `subscription.ResolverConfig` | `consumer.ResolverConfig` |
| `subscription.RetryConfig` | `consumer.RetryConfig` |
| `subscription.GateMetrics` | `consumer.GateMetrics` |
| `subscription.ResolverMetrics` | `consumer.ResolverMetrics` |
| `subscription.MessageTracker` | Internal |
| `subscription.TrackerPolicy` | Internal |
| `subscription.NAKReason` + 6 constants | Internal |
| `subscription.AuditResult` | Internal |
| `subscription.NewMessageTracker()` | Internal |
| `subscription.NewSimpleTracker()` | Internal |
| `subscription.ErrConsumerClosed` | Re-exported from `consumer` if needed |
| `subscription.ErrInvalidConfig` | Re-exported from `consumer` if needed |

#### subscription default constants

**Promoted to `consumer/` (tuning-relevant for advanced users):**

| v1 Constant | v2 Constant | Value | Why public |
|---|---|---|---|
| `subscription.DefaultAckWait` | `consumer.DefaultAckWait` | 30s | Must exceed max processing time |
| `subscription.DefaultMaxDeliver` | `consumer.DefaultMaxDeliver` | -1 | Redelivery policy, DLQ relevance |
| `subscription.DefaultMaxAckPending` | `consumer.DefaultMaxAckPending` | 1 | Throughput vs ordering trade-off |
| `subscription.DefaultInactiveThreshold` | `consumer.DefaultInactiveThreshold` | 24h | Server-side consumer GC |
| `subscription.DefaultFetchBatchSize` | `consumer.DefaultFetchBatchSize` | 1 | Pre-fetch depth |
| `subscription.DefaultFetchMaxWait` | `consumer.DefaultFetchMaxWait` | 5s | Pull expiry, idle CPU impact |

**Deleted (internal machinery — not useful for users to reference):**

| Deleted Constant | Reason |
|---|---|
| `DefaultDrainTimeout` | Internal lifecycle |
| `DefaultRetryMaxAttempts` | Internal backoff |
| `DefaultRetryInitialDelay` | Internal backoff |
| `DefaultRetryMaxDelay` | Internal backoff |
| `DefaultRetryMultiplier` | Internal backoff |
| `DefaultRetryJitter` | Internal backoff |
| `DefaultClaimBatchWindow` | Internal resolver batching |
| `DefaultClaimBatchMaxItems` | Internal resolver batching |
| `DefaultGateNakDelay` | Internal gate |
| `DefaultGateNakJitter` | Internal gate |
| `DefaultIteratorTimeout` | Internal pull loop |
| `DefaultIdleHeartbeat` | Rarely changed; redundant with NATS defaults |

#### partition types (consumer-side only — deleted from public API)

| Deleted Symbol | v2 Replacement |
|---|---|
| `partition.MessageHandler` | `consumer.MessageHandler` |
| `partition.MessageHandlerFunc` | `consumer.MessageHandlerFunc` |
| `partition.NATSMessageHandler` | Internal (used by `Subscriber` only) |
| `partition.NATSMessageHandlerFunc` | Internal |
| `partition.PartitionJSConsumer` | No interface — use `consumer.Static` directly |
| `partition.JSConsumer` | `consumer.Static` (wraps it) |
| `partition.NewJSConsumer()` | `consumer.NewStatic()` |
| `partition.ConsumerConfig` | `consumer.StaticConfig` |
| `partition.NewConsumerConfig()` | Defaults baked into `consumer.NewStatic()` |
| `partition.ConsumerOption` | `consumer.StaticOption` |
| `partition.KeyExtractorFunc` | Internal |

#### partition consumer option functions (deleted)

| Deleted Option | v2 Replacement |
|---|---|
| `partition.WithPartitionIndex()` | Constructor arg in `consumer.NewStatic()` |
| `partition.WithNumPartitions()` | Constructor arg in `consumer.NewStatic()` |
| `partition.WithConsumerName()` | Constructor arg in `consumer.NewStatic()` |
| `partition.WithStreamName()` | Constructor arg in `consumer.NewStatic()` |
| `partition.WithSubjectPattern()` | Constructor arg in `consumer.NewStatic()` |
| `partition.WithKeyExtractor()` | `consumer.WithKeyExtractor()` (StaticOption) |
| `partition.WithMaxDeliver()` | `consumer.WithMaxDeliver()` |
| `partition.WithAckWait()` | `consumer.WithAckWait()` |
| `partition.WithIdleHeartbeat()` | `consumer.WithIdleHeartbeat()` |
| `partition.WithInactiveThreshold()` | `consumer.WithInactiveThreshold()` |

---

### Renamed Symbols

| v1 | v2 | Package |
|---|---|---|
| `consumer.NewStatic(... partIdx int ...)` | `consumer.NewStatic(... partitionIndex int ...)` | `consumer` |
| `consumer.StaticConfig.PartIdx` | `consumer.StaticConfig.PartitionIndex` | `consumer` |

---

### Signature Changes

#### `consumer.Dynamic.SetResolverMetrics()`

```diff
- func (d *Dynamic) SetResolverMetrics(m subscription.ResolverMetrics)
+ func (d *Dynamic) SetResolverMetrics(m consumer.ResolverMetrics)
```

Same interface shape, different package. Users who implemented `subscription.ResolverMetrics`
can switch the import — no method changes needed.

#### `consumer.DynamicConfig` fields

```diff
  type DynamicConfig struct {
      BaseConfig
-     ProcessingGate *subscription.ProcessingGateConfig
+     ProcessingGate *consumer.ProcessingGateConfig
-     Resolver       subscription.ResolverConfig
+     Resolver       consumer.ResolverConfig
      // ... other fields unchanged
  }
```

#### Options that accepted subscription types

```diff
- consumer.WithProcessingGate(cfg *subscription.ProcessingGateConfig)
+ consumer.WithProcessingGate(cfg *consumer.ProcessingGateConfig)

- consumer.WithResolver(cfg subscription.ResolverConfig)
+ consumer.WithResolver(cfg consumer.ResolverConfig)

- consumer.WithGateMetrics(m subscription.GateMetrics)
+ consumer.WithGateMetrics(m consumer.GateMetrics)
```

---

### Symbols That Stay Unchanged (API shape — import paths change to `/v2`)

#### Root `parti` package
- `Manager`, `Config`, all `Option` functions
- All type aliases (`State`, `Partition`, `Assignment`, etc.) — sourced from `types/`
- All 14 re-exported errors — sourced from `types/`, NOT from subscription/partition
- `WorkerConsumerUpdater`, `CompositeConsumerUpdater` — interface uses `types.Partition`,
  no dependency on internal packages; deeply embedded (4 implementations, 9+ test sites)
- `WithWorkerConsumerUpdater()`, `WithHooks()`, `WithMetrics()`, etc.

#### `consumer` package (existing API surface)
- `NewQueue()`, `NewBroadcast()`, `NewDynamic()`, `NewStatic()` — same signatures
  (except `partIdx` → `partitionIndex` in `NewStatic`)
- `MessageHandler`, `MessageHandlerFunc`
- `BaseConfig`, `QueueConfig`, `RetryConfig`
- All `With*()` option functions (except those that change type parameters above)
- `WIPHandler`, `WIPConfig`, `NewWIPHandler()`
- `Queue.Start/Stop`, `Broadcast.Start/UpdateWorkerConsumer/Stop`,
  `Dynamic.Update/Stop`, `Static.Start/Stop`
- **New in v2:** `consumer.DefaultAckWait`, `consumer.DefaultMaxDeliver`, etc.
  (tuning-relevant defaults promoted from deleted `subscription.Default*` constants)

#### `partition` package (publish + low-level core-NATS routing)
- `Publisher`, `JSPublisher` — static partition publishers
- `Subscriber` — core-NATS push subscriber for StatefulSet fire-and-forget patterns.
  Stays public because it is fundamentally different from JetStream consumers:
  no ack, no durable, no pull. Not an escape hatch for `consumer/`.
- `PartitionPublisher` interface
- `PartitionConfig`
- `NewPublisher()`, `NewJSPublisher()`, `NewSubscriber()`
- `StatefulSetIndex()`, `StatefulSetConsumerName()`
- All publisher option functions: `WithHasher()`, `WithLogger()`,
  `WithMetrics()`, `WithReplicas()`
- All error sentinels: `ErrEmptyKey`, `ErrInvalidKey`, `ErrInvalidPattern`,
  `ErrPartitionOutOfRange`, `ErrInvalidNumPartitions`, `ErrMissingPartitionPlaceholder`

#### `source/`, `strategy/`, `types/`, `jsutil/`, `kvutil/`
- API shape unchanged — same types, same functions
- Import paths change: `parti/source` → `parti/v2/source`, etc.

---

### Migration Checklist for Library Users

1. **Update `go.mod`:**
   ```
   go get github.com/arloliu/parti/v2@latest
   ```

2. **Find-replace import paths:**
   ```
   parti" → parti/v2"
   ```

3. **Remove `subscription` imports:**
   - Replace `subscription.ProcessingGateConfig` → `consumer.ProcessingGateConfig`
   - Replace `subscription.ResolverConfig` → `consumer.ResolverConfig`
   - Replace `subscription.ResolverMetrics` → `consumer.ResolverMetrics`
   - Replace `subscription.GateMetrics` → `consumer.GateMetrics`
   - Replace `subscription.RetryConfig` → `consumer.RetryConfig`
   - If using `subscription.WorkerConsumer` directly → switch to `consumer.NewDynamic()`
   - If using `subscription.BroadcastConsumer` directly → switch to `consumer.NewBroadcast()`

4. **Remove `partition` consumer imports:**
   - Replace `partition.JSConsumer` / `partition.NewJSConsumer()` → `consumer.NewStatic()`
   - Replace `partition.MessageHandler` → `consumer.MessageHandler`
   - Keep `partition.Publisher` / `partition.JSPublisher` — unchanged

5. **Rename `testing` import:**
   ```
   parti/v2/testing → parti/v2/partitest
   ```

6. **Rename `partIdx`:**
   - `StaticConfig.PartIdx` → `StaticConfig.PartitionIndex`

7. **Update references to subscription default constants** (if used):
   - `subscription.DefaultAckWait` → `consumer.DefaultAckWait`
   - `subscription.DefaultMaxDeliver` → `consumer.DefaultMaxDeliver`
   - `subscription.DefaultMaxAckPending` → `consumer.DefaultMaxAckPending`
   - `subscription.DefaultInactiveThreshold` → `consumer.DefaultInactiveThreshold`
   - Other `subscription.Default*` constants are removed — use functional options

8. **Compile and fix any remaining type mismatches**

9. **Run tests**
