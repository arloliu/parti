# Migrating from v1 to v2

This guide covers every breaking change between **Parti v1.7.6** and **v2.0.0**
and shows you how to update your code.

> **Estimated effort**: Most projects can migrate in under an hour.
> Run `go vet ./...` after each section to catch stragglers.

---

## Table of Contents

1. [Module Path](#1-module-path)
2. [Import Path Changes](#2-import-path-changes)
3. [Package Renames & Removals](#3-package-renames--removals)
4. [Consumer API Changes](#4-consumer-api-changes)
5. [Config & SetDefaults](#5-config--setdefaults)
6. [Manager Changes](#6-manager-changes)
7. [Metrics (Interface Segregation)](#7-metrics-interface-segregation)
8. [Error Sentinel Changes](#8-error-sentinel-changes)
9. [Partition Package Changes](#9-partition-package-changes)
10. [Behavioral Changes](#10-behavioral-changes)
11. [New Features](#11-new-features)
12. [Quick Checklist](#12-quick-checklist)

---

## 1. Module Path

The module path moved to a `/v2` suffix per Go module conventions:

```bash
# v1
go get github.com/arloliu/parti

# v2
go get github.com/arloliu/parti/v2
```

**Every** import in your project must be updated. A global find-and-replace
works well:

```
github.com/arloliu/parti  →  github.com/arloliu/parti/v2
```

> **Tip:** `goimports -w .` will fix ordering after the rename.

---

## 2. Import Path Changes

| v1 Import | v2 Import |
|---|---|
| `github.com/arloliu/parti` | `github.com/arloliu/parti/v2` |
| `github.com/arloliu/parti/consumer` | `github.com/arloliu/parti/v2/consumer` |
| `github.com/arloliu/parti/partition` | `github.com/arloliu/parti/v2/partition` |
| `github.com/arloliu/parti/source` | `github.com/arloliu/parti/v2/source` |
| `github.com/arloliu/parti/strategy` | `github.com/arloliu/parti/v2/strategy` |
| `github.com/arloliu/parti/types` | `github.com/arloliu/parti/v2/types` |
| `github.com/arloliu/parti/jsutil` | `github.com/arloliu/parti/v2/jsutil` |
| `github.com/arloliu/parti/kvutil` | `github.com/arloliu/parti/v2/kvutil` |
| `github.com/arloliu/parti/testing` | `github.com/arloliu/parti/v2/partitest` ⚠️ **renamed** |
| `github.com/arloliu/parti/subscription` | **removed** — see [§3](#3-package-renames--removals) |

---

## 3. Package Renames & Removals

### `testing/` → `partitest/`

The test-helper package was renamed from `testing` to `partitest` to avoid
shadowing Go's standard `testing` package.

```go
// v1
import partitest "github.com/arloliu/parti/testing"

// v2 — no alias needed
import "github.com/arloliu/parti/v2/partitest"
```

### `subscription/` — removed

The entire `subscription` package was internalized into `internal/durable`.
You **cannot** import it directly in v2.

Everything you need is re-exported through the `consumer` package:

| v1 (subscription) | v2 (consumer) |
|---|---|
| `subscription.MessageHandler` | `consumer.MessageHandler` |
| `subscription.MessageHandlerFunc` | `consumer.MessageHandlerFunc` |
| `subscription.ProcessingGateConfig` | `consumer.ProcessingGateConfig` |
| `subscription.ResolverConfig` | `consumer.ResolverConfig` |
| `subscription.GateMetrics` | `consumer.GateMetrics` |
| `subscription.ResolverMetrics` | `consumer.ResolverMetrics` |
| `subscription.ErrWorkerIDMutation` | `consumer.ErrWorkerIDMutation` |
| `subscription.ErrMaxSubjectsExceeded` | `consumer.ErrMaxSubjectsExceeded` |

If you used `subscription.NewWorkerConsumer` directly, switch to
`consumer.NewDynamic` (see [§4](#4-consumer-api-changes)).

### `partition/` — scope reduced

Several internal helpers that were exported from the `partition` package have
been moved to internal packages:

| Removed from `partition` | Notes |
|---|---|
| `ConsumerConfig`, `ConsumerOption`, `NewConsumerConfig` | Moved to `internal/ipartition` |
| `MessageHandler`, `MessageHandlerFunc` | Use `consumer.MessageHandler` instead |
| `HashID()`, `computePartition()` | Moved to `internal/partutil` |
| Subject pattern helpers (`NewPattern`, `ParsePattern`, etc.) | Moved to `internal/partutil` |

If you used `partition.JSConsumer` directly, use `consumer.Static` or
`consumer.Dynamic` instead — they wrap the same underlying logic.

---

## 4. Consumer API Changes

### WorkerConsumer → Dynamic

The `subscription.NewWorkerConsumer` struct-based constructor is replaced by
`consumer.NewDynamic` with a functional-options pattern:

```go
// v1
import "github.com/arloliu/parti/subscription"

wc, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
    StreamName:      "ORDERS",
    ConsumerPrefix:  "processor",
    SubjectTemplate: "orders.{{.PartitionID}}.complete",
    ProcessingGate: &subscription.ProcessingGateConfig{
        Enabled: true,
    },
}, handler)

// v2
import "github.com/arloliu/parti/v2/consumer"

dyn, err := consumer.NewDynamic(js, "ORDERS", "processor",
    "orders.{{.PartitionID}}.complete", handler,
    consumer.WithProcessingGate(&consumer.ProcessingGateConfig{Enabled: true}),
)
```

### MessageHandler moved to `consumer`

```go
// v1
handler := subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    return nil
})

// v2
handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    return nil
})
```

### ProcessingGateConfig & ResolverConfig

These types moved from `subscription` to `consumer`. Update type references:

```go
// v1
gate := &subscription.ProcessingGateConfig{Enabled: true}
resolver := subscription.ResolverConfig{...}

// v2
gate := &consumer.ProcessingGateConfig{Enabled: true}
resolver := consumer.ResolverConfig{...}
```

### consumer.WithMetrics — narrower interface

The `WithMetrics` option now accepts `types.WorkerConsumerMetrics` instead of
the full `types.MetricsCollector`. If you pass a full `MetricsCollector`
implementation it still works (the interface is embedded), but if you
implemented the interface yourself, ensure it satisfies `WorkerConsumerMetrics`.

```go
// v1
consumer.WithMetrics(myFullMetricsCollector)

// v2 — still works if myFullMetricsCollector implements MetricsCollector
consumer.WithMetrics(myFullMetricsCollector)

// v2 — or pass a narrower implementation
consumer.WithMetrics(myWorkerConsumerMetrics)
```

---

## 5. Config & SetDefaults

`parti.SetDefaults` now returns an `error` instead of panicking:

```go
// v1 — panics on bad input
parti.SetDefaults(cfg)

// v2 — returns error
if err := parti.SetDefaults(cfg); err != nil {
    log.Fatal(err)
}
```

This is a **compile-time break** if you were ignoring the (previously
non-existent) return value. The compiler will flag it.

---

## 6. Manager Changes

### Start() auto-cleanup on failure

In v1, a failed `Start()` could leave partial resources that required a manual
`Stop()` call. In v2, `Start()` automatically cleans up on failure:

```go
// v1 — had to call Stop even on error
if err := mgr.Start(ctx); err != nil {
    mgr.Stop(ctx) // required to avoid leaks
    return err
}

// v2 — cleanup is automatic
if err := mgr.Start(ctx); err != nil {
    return err // no Stop() needed
}
```

If your code already called `Stop()` after a failed `Start()`, it is safe to
keep doing so — `Stop()` is idempotent.

### Stop() unconditional leadership release

`Stop()` now always attempts `ReleaseLeadership()` regardless of the
`IsLeader()` flag. This eliminates a TOCTOU race window. No code changes
needed — this is purely a reliability improvement.

### CurrentAssignment() aliasing warning

The `Assignment` returned by `CurrentAssignment()` shares its `Partitions`
backing array with internal state. Do not modify the returned slice.

```go
// ⚠️ Unsafe — mutates internal state
a := mgr.CurrentAssignment()
a.Partitions = a.Partitions[:1]

// ✅ Safe — copy first
a := mgr.CurrentAssignment()
p := make([]parti.Partition, len(a.Partitions))
copy(p, a.Partitions)
```

### WithHandoffMetricsRecorder — public type

The option now accepts the public `parti.HandoffMetricsRecorder` (re-exported
from `types.HandoffMetricsRecorder`) instead of the internal
`handoff.MetricsRecorder`:

```go
// v1
parti.WithHandoffMetricsRecorder(myInternalRecorder)

// v2 — same call, but the parameter type is now public
parti.WithHandoffMetricsRecorder(myRecorder) // implements types.HandoffMetricsRecorder
```

If you used the `NopMetrics` sentinel, it is now `types.NopHandoffMetricsRecorder`.

---

## 7. Metrics (Interface Segregation)

The monolithic `types.MetricsCollector` interface is **unchanged** — existing
implementations compile without edits. What's new is that v2 also exports the
five domain-specific sub-interfaces it composes:

| Sub-interface | Used by |
|---|---|
| `parti.ManagerMetrics` | `Manager` internals |
| `parti.CalculatorMetrics` | Assignment calculator |
| `parti.WorkerMetrics` | Worker monitor |
| `parti.AssignmentMetrics` | Assignment publisher |
| `parti.WorkerConsumerMetrics` | `consumer` package |

**Action required:** None unless you were constructing the consumer separately.
In that case, `consumer.WithMetrics` now expects `WorkerConsumerMetrics`
instead of the full `MetricsCollector` (see [§4](#4-consumer-api-changes)).

The new `types.HandoffMetricsRecorder` interface is separate from
`MetricsCollector`. Use `types.NopHandoffMetricsRecorder` as a no-op default.

---

## 8. Error Sentinel Changes

v2 introduces proper sentinel errors for `errors.Is()` matching:

| Sentinel | Package | Use |
|---|---|---|
| `consumer.ErrInvalidConfig` | `consumer` | Wraps all consumer constructor validation errors |
| `types.ErrTwoPhaseHandoffDisabled` | `types` | Returned by `Manager.InspectHandoffClaims()` |
| `types.ErrInvalidPreset` | `types` | Wrapped by `DegradedBehaviorPreset()` errors |
| `types.ErrNoWorkersAvailable` | `types` | Wrapped by `strategy.ErrNoWorkers` |

### Migration examples

```go
// v1 — string matching (fragile)
if err.Error() == "two-phase handoff is disabled" { ... }

// v2 — use errors.Is
if errors.Is(err, types.ErrTwoPhaseHandoffDisabled) { ... }
```

```go
// v1 — no way to distinguish consumer config errors
_, err := consumer.NewDynamic(...)
if err != nil { log.Fatal(err) } // all errors are opaque

// v2 — test the category
if errors.Is(err, consumer.ErrInvalidConfig) {
    // configuration problem — fix inputs
}
```

---

## 9. Partition Package Changes

The `partition` package is still public, but its scope has been reduced to the
core publish/subscribe primitives:

| Still in `partition` | Removed from `partition` |
|---|---|
| `Publisher` | `ConsumerConfig` → internal |
| `Subscriber` | `ConsumerOption` → internal |
| `JSPublisher` | `MessageHandler` → `consumer.MessageHandler` |
| `Config` (publish-side) | `HashID` helpers → internal |
| `NewPublisher`, `NewSubscriber`, `NewJSPublisher` | Subject pattern parsing → internal |
| `Validate` | `JSConsumer` → internal |

### JetStream consumer creation

If you used `partition.NewJSConsumerWithOptions` directly:

```go
// v1
import "github.com/arloliu/parti/partition"

jsc, err := partition.NewJSConsumerWithOptions(
    js, partition.NewConsumerConfig(
        "STREAM", "group", "orders.{{partition}}", handler,
        partition.WithMaxDeliver(5),
    ),
)

// v2 — use consumer.Static or consumer.Dynamic
import "github.com/arloliu/parti/v2/consumer"

static, err := consumer.NewStatic(js, "STREAM", "group",
    "orders.{{.PartitionID}}", 0, handler,
    consumer.WithMaxDeliver(5),
)
```

### partition.Config changes

Consuming-related fields (`JSConsumer`, `ConsumerConfig`, handler types) have
been removed from `partition.Config`. The remaining fields for publishing are
unchanged.

---

## 10. Behavioral Changes

These changes don't require code edits but may affect your system's runtime
behavior:

| Change | Impact |
|---|---|
| **`Partition.Validate()` rejects empty `Keys`** | Partitions with no keys now fail validation. Previously returned `nil`. |
| **`Partition.HashID()` first-key fix** | A bug where the first key was silently dropped when xxh3 returned 0 is fixed. Hash values may change for partitions whose first key hashed to 0 — this is extremely rare. |
| **`Partition.Weight` semantics** | Zero weight now means "use strategy default" (typically 1). Negative weights are treated as zero. |
| **WIPHandler first heartbeat** | First heartbeat now fires immediately at 1×interval instead of waiting for the first tick. |
| **Hook goroutines tracked** | `OnError` hooks now run inside the Manager's WaitGroup, so `Stop()` waits for them. Previously fire-and-forget. |

---

## 11. New Features

These are additions — no migration needed, but they may simplify your code:

- **`consumer.ErrInvalidConfig` sentinel** — `errors.Is()` for config errors.
- **Exported default constants** in `consumer` — `DefaultAckWait`,
  `DefaultMaxDeliver`, `DefaultMaxAckPending`, `DefaultInactiveThreshold`,
  `DefaultFetchBatchSize`, `DefaultFetchMaxWait`, `DefaultMaxWaiting`.
- **Exported metrics sub-interfaces** — `ManagerMetrics`, `CalculatorMetrics`,
  `WorkerMetrics`, `AssignmentMetrics`, `WorkerConsumerMetrics` in the root
  package. Implement only what you need.
- **`HandoffMetricsRecorder` / `NopHandoffMetricsRecorder`** — public types for
  handoff metrics instrumentation.
- **Example tests** — `example_test.go` in the root package demonstrates
  `NewManager` setup.
- **`Manager.Start()` auto-cleanup** — no more manual `Stop()` after failed
  `Start()`.

---

## 12. Quick Checklist

Use this as a step-by-step migration plan:

- [ ] **Update `go.mod`**: `go get github.com/arloliu/parti/v2`
- [ ] **Find-and-replace imports**: `github.com/arloliu/parti` → `github.com/arloliu/parti/v2`
- [ ] **Rename `testing` import**: `parti/testing` → `parti/v2/partitest` (drop alias)
- [ ] **Remove `subscription` imports**: replace with `consumer` equivalents
- [ ] **Update `MessageHandler`**: `subscription.MessageHandler*` → `consumer.MessageHandler*`
- [ ] **Update config types**: `subscription.ProcessingGateConfig` → `consumer.ProcessingGateConfig`
- [ ] **Handle `SetDefaults` error**: `parti.SetDefaults(cfg)` → `if err := parti.SetDefaults(cfg); err != nil { … }`
- [ ] **Update partition consumer usage**: `partition.NewJSConsumerWithOptions` → `consumer.NewStatic`/`consumer.NewDynamic`
- [ ] **Remove `partition.MessageHandler`**: use `consumer.MessageHandler` instead
- [ ] **Check `WithMetrics`**: ensure consumer metrics param satisfies `WorkerConsumerMetrics`
- [ ] **Update error checks**: switch from string matching to `errors.Is()` with new sentinels
- [ ] **Run `go vet ./...` and `go build ./...`**: let the compiler catch remaining issues
- [ ] **Run tests**: verify runtime behavior
