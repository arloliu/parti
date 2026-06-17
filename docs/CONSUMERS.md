# Parti Consumer Package

> Unified JetStream consumer types for partitioned workloads.

**Related Documentation:**
- [Docs README](README.md) - Documentation map
- [Architecture](ARCHITECTURE.md) - System architecture and concepts
- [Lifecycle Guide](LIFECYCLE.md) - Worker states and handoff
- [Strategies Guide](STRATEGIES.md) - Assignment strategies

---

## Table of Contents

1. [Overview](#overview)
2. [Quick Start](#quick-start)
3. [Consumer Types](#consumer-types)
   - [Queue](#queue)
   - [Static](#static)
   - [Dynamic](#dynamic)
   - [Broadcast](#broadcast)
4. [Stream Retention Policy](#stream-retention-policy)
5. [Message Handler](#message-handler)
6. [WIPHandler — Long-Running Processing](#wiphandler--long-running-processing)
7. [Auto-Recovery](#auto-recovery)
8. [Functional Options](#functional-options)
9. [Consumer Storage Tuning](#consumer-storage-tuning)
10. [Migrating from Legacy Consumer APIs](#migrating-from-legacy-consumer-apis)
11. [Legacy Consumer APIs](#legacy-consumer-apis)

---

## Overview

The `consumer` package provides a unified API for JetStream consumers in partitioned workloads:

| Consumer    | Purpose                                  | Coordination | Lifecycle     |
|-------------|------------------------------------------|--------------|---------------|
| `Queue`     | Load-balanced workers (queue group)      | None         | Start → Stop  |
| `Static`    | Fixed partition (StatefulSet ordinal)    | None         | Start → Stop  |
| `Dynamic`   | Manager-assigned partitions (Parti core) | Via Manager  | Update → Stop |
| `Broadcast` | Fan-out to all instances                 | None         | Start → Stop  |

### Import

```go
import "github.com/arloliu/parti/v2/consumer"
```

> **Migration Note:** The `consumer` package replaces the legacy `subscription`
> package and the old `partition.JSConsumer` API. The `partition` package
> remains public in v2 for static routing and publishing helpers.

---

## Quick Start

### Queue Consumer (Load-Balanced Workers)

```go
handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    fmt.Printf("Processing: %s\n", msg.Subject())
    return nil // auto-ack
})

c, err := consumer.NewQueue(js, "JOBS", "job-workers", "jobs.>", handler)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

if err := c.Start(ctx); err != nil {
    log.Fatal(err)
}
```

### Dynamic Consumer (Parti Manager)

```go
c, err := consumer.NewDynamic(js, "ORDERS", "order-processor", "orders.{{.PartitionID}}", handler)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

// Register with Parti Manager for automatic partition assignment
mgr, _ := parti.NewManager(cfg, js, src, strategy.NewConsistentHash(),
    parti.WithWorkerConsumerUpdater(c),
)
```

---

## Consumer Types

### Queue

Load-balanced consumer where multiple instances share one durable consumer. Each message is delivered to exactly one instance (queue group semantics).

**Use Cases:**
- Classic worker queue patterns
- Stateless message processing
- Horizontal scaling without coordination

**Lifecycle:** `Start(ctx) → Stop(ctx)`

```go
c, err := consumer.NewQueue(
    js,              // jetstream.JetStream
    "JOBS",          // streamName
    "job-workers",   // consumerName (shared across instances)
    "jobs.>",        // filterSubject
    handler,         // MessageHandler
    // options...
)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

if err := c.Start(ctx); err != nil {
    log.Fatal(err)
}
```

**Key Methods:**

| Method       | Description                          |
|--------------|--------------------------------------|
| `Start(ctx)` | Begin consuming messages             |
| `Stop(ctx)`  | Gracefully stop with context timeout |

---

### Static

Consumer bound to a single, fixed partition. Use for StatefulSet deployments where pod ordinal determines partition assignment.

**Use Cases:**
- Kubernetes StatefulSet (pod ordinal → partition)
- Fixed partition ownership
- Zero-coordination partitioning

**Lifecycle:** `Start(ctx) → Stop(ctx)`

```go
c, err := consumer.NewStatic(
    js,                         // jetstream.JetStream
    "EVENTS",                   // streamName
    "processor-0",              // consumerName
    "events.{{partition}}",     // subjectPattern
    10,                         // numPartitions (total)
    0,                          // partition (this instance)
    handler,                    // MessageHandler
    // options...
)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

if err := c.Start(ctx); err != nil {
    log.Fatal(err)
}
```

**Subject Pattern Placeholders:**
- `{{partition}}` - Replaced with partition index (required)
- `{{key}}` - Replaced with partition key (optional, becomes `*` for subscription)

**Key Methods:**

| Method        | Description                          |
|---------------|--------------------------------------|
| `Start(ctx)`  | Begin consuming messages             |
| `Stop(ctx)`   | Gracefully stop with context timeout |
| `Partition()` | Returns the partition index          |
| `Subject()`   | Returns the filter subject           |

---

### Dynamic

Partition-aware consumer that receives assignments from a Parti Manager. Manages multiple internal consumers based on assigned partitions.

**Use Cases:**
- Parti-coordinated workloads
- Dynamic partition assignment
- Elastic scaling with partition rebalancing

**Lifecycle:** `Update(ctx, workerID, partitions) → Stop(ctx)`

```go
c, err := consumer.NewDynamic(
    js,                              // jetstream.JetStream
    "ORDERS",                        // streamName
    "order-processor",               // consumerPrefix
    "orders.{{.PartitionID}}",       // subjectTemplate (Go template)
    handler,                         // MessageHandler
    // options...
)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

// Register with Manager for automatic updates
mgr, _ := parti.NewManager(cfg, js, src, strategy.NewConsistentHash(),
    parti.WithWorkerConsumerUpdater(c),
)
```

**Manual Update (without Manager):**

```go
partitions := []types.Partition{
    {Keys: []string{"partition-0"}},
    {Keys: []string{"partition-1"}},
}
if err := c.Update(ctx, "worker-0", partitions); err != nil {
    log.Fatal(err)
}
```

**Key Methods:**

| Method                                      | Description                                  |
|---------------------------------------------|----------------------------------------------|
| `Update(ctx, workerID, partitions)`         | Update partition assignments                 |
| `Stop(ctx)`                                 | Gracefully stop all partition consumers      |
| `UpdateWorkerConsumer(ctx, id, partitions)` | Implements `WorkerConsumerUpdater` interface |

---

### Broadcast

Fan-out consumer where every instance receives every message. Uses a unique durable name per instance.

**Use Cases:**
- Cache invalidation
- Configuration updates
- Audit logging
- Global event notifications

**Lifecycle:** `Start(ctx) → Stop(ctx)`

> **Important:** The stream MUST use `LimitsPolicy` or `InterestPolicy`. `WorkQueuePolicy` is incompatible because it delivers each message to exactly one consumer.

```go
c, err := consumer.NewBroadcast(
    js,                // jetstream.JetStream
    "EVENTS",          // streamName
    "cache-updater",   // consumerPrefix
    "events.>",        // filterSubject
    handler,           // MessageHandler
    consumer.WithInstanceID("pod-abc123"), // unique per instance
)
if err != nil {
    log.Fatal(err)
}
defer c.Stop(ctx)

if err := c.Start(ctx); err != nil {
    log.Fatal(err)
}
```

**Key Methods:**

| Method       | Description                          |
|--------------|--------------------------------------|
| `Start(ctx)` | Begin consuming messages             |
| `Stop(ctx)`  | Gracefully stop with context timeout |

---

## Stream Retention Policy

The JetStream **retention policy** of the stream a consumer reads from is the
single most consequential — and most error-prone — choice when wiring up a
consumer. It controls *when a message is deleted*, and the wrong policy silently
loses data. Pick it by answering two questions, then check the per-type table.

**The two questions:**

1. **Must messages survive after they are processed** — for replay, multiple
   independent readers, or windows where no consumer covers the subject? →
   **`LimitsPolicy`** (messages retained until age/size/count limits; acks never
   delete).
2. **Is this a dedicated, consume-once queue** with a single non-overlapping
   consumer set, where restricted recovery is acceptable? → **`WorkQueuePolicy`**
   (delivered once, deleted on ack; filters must be disjoint).

`InterestPolicy` is a narrow third option: it retains a message only while a
bound consumer still owes an ack, and **discards any message published when no
consumer covers its subject**.

**Per consumer type:**

| Consumer | Recommended | Also valid (caveats) | Avoid / Forbidden |
|----------|-------------|----------------------|-------------------|
| `Queue`     | **`WorkQueuePolicy`** for a dedicated consume-once queue — one shared durable matches WorkQueue's delete-on-ack model and bounds storage automatically | **`LimitsPolicy`** when the stream is shared, you need replay/retention, or you need `RecoverFromNew` | `InterestPolicy` — messages published with no live consumer are dropped |
| `Static`    | **`LimitsPolicy`** when retention/replay matters (e.g. event-sourced partitions); **`WorkQueuePolicy`** for fixed consume-once partitions (filters are non-overlapping by construction) | — | `InterestPolicy` unless every partition durable exists before publishing |
| `Dynamic`   | **`LimitsPolicy`** — preserves all recovery strategies and replay, and tolerates the brief unassigned windows during handoff | **`WorkQueuePolicy`** is supported (proven lossless across graceful handoff *and* abrupt crash), but it limits recovery to `RecoverFromBeginning`/`RecoveryDisabled` and forgoes replay — pick it only for consume-once work | `InterestPolicy` — an unassigned/just-moved subject's messages can be discarded |
| `Broadcast` | **`LimitsPolicy`** — every instance must receive every message; Limits retains independently of which instances are up | `InterestPolicy` *only* when instance identities are stable, all recipients are registered before publishing, churn is low (or `InactiveThreshold` is short), and dropping messages published while all instances are down is acceptable | **`WorkQueuePolicy`** — single delivery defeats fan-out (rejected) |

**Three cross-cutting rules that bite regardless of type:**

- **WorkQueue restricts recovery.** WorkQueue consumers may only use
  `DeliverAllPolicy`, so `RecoverFromNew` and `RecoverFromLastProcessed` are
  rejected — see [WorkQueuePolicy Restriction](#workqueuepolicy-restriction).
  Only `RecoverFromBeginning`/`RecoveryDisabled` work (and on WorkQueue,
  `RecoverFromBeginning` replays just the unacked backlog).
- **Replicas cap depends on retention.** On any stream, consumer replicas may
  not *exceed* the stream's. On `LimitsPolicy` (the default) any value
  `1…stream.Replicas` is allowed — so `WithConsumerReplicas(1)` works on an RF=5
  stream. On `InterestPolicy`/`WorkQueuePolicy`, a nonzero value must *equal* the
  stream's replica count, so any mismatch (e.g. `WithConsumerReplicas(1)` or `3`
  on an RF=5 stream) is rejected by NATS (`err_code=10134`). The single-replica
  IOPS option in [Consumer Storage Tuning](#consumer-storage-tuning) is therefore
  unavailable on Interest/WorkQueue; pair `WithConsumerMemoryStorage(true)` with
  inherited replicas (the Balanced config) instead.
- **Interest pins messages behind stopped consumers.** A consumer's durable is
  not deleted on `Stop()` (it is garbage-collected only after
  `InactiveThreshold`, default 24h). On `InterestPolicy`, that lingering durable
  still holds interest, so a stopped/churned instance pins its unacked messages
  until GC — the reason `Broadcast` defaults to `LimitsPolicy`, not Interest.

---

## Message Handler

All consumer types use the unified `MessageHandler` interface:

```go
type MessageHandler interface {
    Handle(ctx context.Context, msg jetstream.Msg) error
}
```

**Functional Adapter:**

```go
handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    // Process message
    data := msg.Data()
    _ = data

    return nil  // nil = auto-ack
})
```

**Auto-Acknowledgement (default):**
- `return nil` → Message acknowledged (Ack)
- `return error` → Message negatively acknowledged (Nak)

**Manual Acknowledgement:**

```go
c, _ := consumer.NewQueue(js, "stream", "consumer", "subject.>", handler,
    consumer.WithManualAck(true),
)

// In handler, you must call one of:
msg.Ack()           // Acknowledge
msg.Nak()           // Negative ack (immediate redeliver)
msg.NakWithDelay(d) // Negative ack with delay
msg.Term()          // Terminate (no redeliver)
```

---

## WIPHandler — Long-Running Processing

`WIPHandler` wraps any `MessageHandler` and periodically calls `msg.InProgress()` while the handler is running. This extends the JetStream `AckWait` deadline, preventing the server from redelivering the message before processing finishes.

```go
wrapped := consumer.NewWIPHandler(myHandler, consumer.WIPConfig{
    Interval: 10 * time.Second, // AckWait is 30s, so 10s (AckWait/3) is safe
    Logger:   logger,           // optional — receives heartbeat errors
})

q, err := consumer.NewQueue(js, "LONG-JOBS", "slow-processor", "jobs.slow.>", wrapped,
    consumer.WithAckWait(30*time.Second),
)
```

### Interval Selection

| AckWait | Recommended Interval | Formula     |
|---------|----------------------|-------------|
| 30 s    | 10 s                 | AckWait / 3 |
| 60 s    | 20 s                 | AckWait / 3 |
| 5 min   | 100 s                | AckWait / 3 |

- **Maximum safe**: `AckWait / 2` (minimum margin — any slower and the server may redeliver)
- **Recommended**: `AckWait / 3` (one missed heartbeat still keeps the message alive)
- **Minimum**: 100 ms (intervals below `DefaultWIPMinInterval` are clamped automatically)

### Performance Characteristics

`WIPHandler` uses lazy initialization:
- **Fast handlers** (finish before `Interval`): only a timer is allocated — no goroutine is spawned.
- **Slow handlers** (run past `Interval`): one goroutine per message is started to send periodic heartbeats.

### Compatibility

`WIPHandler` works with all consumer types (`Queue`, `Static`, `Dynamic`, `Broadcast`) and both auto-ack and manual-ack modes. In manual-ack mode the handler is still responsible for calling `msg.Ack/Nak/Term` exactly once; the wrapper only calls `msg.InProgress()`.

```go
// Manual-ack + WIPHandler: handler controls ack, wrapper keeps the message alive
handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    result, err := doLongWork(ctx, msg.Data())
    if err != nil {
        _ = msg.Term() // permanent failure — don't redeliver
        return err
    }
    _ = msg.Ack()
    return nil
})

wrapped := consumer.NewWIPHandler(handler, consumer.WIPConfig{
    Interval: 20 * time.Second,
})
```

### Disabling WIPHandler

Passing `Interval <= 0` returns the original handler unchanged — no allocation, no wrapping:

```go
wrapped := consumer.NewWIPHandler(handler, consumer.WIPConfig{Interval: 0})
// wrapped == handler (no-op)
```

---

## Auto-Recovery

All consumer types can automatically recreate their durable JetStream consumer when it is unexpectedly deleted — for example, after a server restart, an administrative deletion, or an `InactiveThreshold` expiry. Recovery is **disabled by default**; enable it with a single option:

```go
c, err := consumer.NewQueue(js, "JOBS", "job-workers", "jobs.>", handler,
    consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
)
```

### Recovery Strategies

| Strategy                   | Behavior on recreation                                | Risk                          |
|----------------------------|-------------------------------------------------------|-------------------------------|
| `RecoveryDisabled`         | No recreation (default)                               | None                          |
| `RecoverFromNew`           | Skip messages published during the outage             | Message loss during outage    |
| `RecoverFromLastProcessed` | Resume from the message after the last acked one      | Works with any `ManualAck`    |
| `RecoverFromBeginning`     | Replay the entire stream from the start               | Replay storm on large streams |

### Per-Consumer Support

| Consumer    | `RecoveryDisabled` | `RecoverFromNew`             | `RecoverFromLastProcessed`       | `RecoverFromBeginning` |
|-------------|--------------------|-----------------------------|----------------------------------|------------------------|
| `Queue`     | ✓                  | ✓ ¹                          | ✗ (shared durable — unsafe)      | ✓                      |
| `Static`    | ✓                  | ✓ ¹                          | ✓ ¹ (any `ManualAck`)            | ✓                      |
| `Dynamic`   | ✓                  | ✓ ¹                          | ✓ ¹ (any `ManualAck`)            | ✓                      |
| `Broadcast` | ✓                  | ✓                            | ✓ (any `ManualAck`)              | ✓                      |

¹ Not supported on WorkQueuePolicy streams — see [WorkQueuePolicy Restriction](#workqueuepolicy-restriction) below.

### Queue Consumer Restriction

**`RecoverFromLastProcessed` is not supported** for `Queue`. Queue shares one durable consumer across all replicas — each instance processes a different subset of messages, so each would advance the checkpoint independently. The resulting resume position is nondeterministic and could cause some messages to be silently skipped by every instance. Passing `RecoverFromLastProcessed` to `NewQueue` returns `ErrInvalidConfig` immediately.

### WorkQueuePolicy Restriction

NATS only allows `DeliverAllPolicy` when creating consumers on a `WorkQueuePolicy` stream. Both `RecoverFromNew` (`DeliverNewPolicy`) and `RecoverFromLastProcessed` (`DeliverByStartSequencePolicy`) are incompatible — every recovery attempt would silently fail and the consumer would never be recreated.

This affects `Queue`, `Static`, and `Dynamic` consumers. Each detects the combination at startup and returns `ErrInvalidConfig`:
- `Queue.Start` rejects `RecoverFromNew` (only strategy affected — `RecoverFromLastProcessed` is already rejected at construction)
- `Static.Start` rejects `RecoverFromNew` and `RecoverFromLastProcessed`
- `Dynamic.Update` (first call) rejects `RecoverFromNew` and `RecoverFromLastProcessed`

For WorkQueuePolicy streams use `RecoverFromBeginning` or `RecoveryDisabled`. On WorkQueuePolicy streams, acknowledged messages are deleted immediately, so `RecoverFromBeginning` replays only the unacknowledged backlog — not the full stream history.

> **Broadcast** is not listed because `WorkQueuePolicy` is already [incompatible with Broadcast](#broadcast) for a different reason (single delivery defeats fan-out).

### Dynamic Consumer — Two Recovery Mechanisms

`Dynamic` has **two independent recovery mechanisms** that complement each other:

1. **Iterator escalation** — when repeated iterator errors occur within a sliding time window (default: 3 errors in 60 s), the consumer rebinds to the *existing* durable without recreating it. This handles transient network blips and JetStream server restarts where the durable itself survived.

2. **`RecoveryStrategy`** — when the durable consumer has been *deleted* (detected via a consumer-not-found error), `RecoveryStrategy` controls how the recreated consumer resumes delivery.

Both mechanisms operate independently. Iterator escalation fires first for transient failures; `RecoveryStrategy` fires only when the durable is confirmed gone. You can enable both on the same consumer:

```go
c, err := consumer.NewDynamic(js, "ORDERS", "processor", "orders.{{.PartitionID}}", handler,
    consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
    // Iterator escalation is always active; configure its window if needed via DynamicConfig.
)
```

### Strategy Examples

**Skip missed messages (safest for Queue):**

```go
q, err := consumer.NewQueue(js, "JOBS", "job-workers", "jobs.>", handler,
    consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
)
```

**Resume from last processed (at-least-once delivery) — `ManualAck=false` (default):**

```go
c, err := consumer.NewStatic(js, "EVENTS", "processor-0", "events.{{partition}}", 10, 0, handler,
    consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
)
```

When `ManualAck=false`, the framework intercepts every successful handler return, calls `msg.Ack()`, and records the stream sequence number as the checkpoint:

```
Handler returns nil
       │
       ▼
 Framework calls msg.Ack()
       │
       ▼
 Checkpoint = this sequence number  ← advanced in memory
       │
       ▼
 Next message...
```

**Resume from last processed — `ManualAck=true`:**

```go
handler := consumer.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
    // process...
    return msg.Ack() // calling Ack() here advances the checkpoint transparently
})

c, err := consumer.NewStatic(js, "EVENTS", "processor-0", "events.{{partition}}", 10, 0, handler,
    consumer.WithManualAck(true),
    consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
)
```

With `ManualAck=true`, the framework wraps the message before passing it to the handler. The wrapper intercepts `msg.Ack()` and `msg.DoubleAck()` to advance the checkpoint before forwarding the call. From the handler's perspective nothing changes — it still receives a `jetstream.Msg` and calls `Ack()` as usual.

```
Handler calls msg.Ack()
       │
       ▼
 Wrapper intercepts → Checkpoint = this sequence number
       │
       ▼
 Underlying msg.Ack() called
       │
       ▼
 Handler receives the error (or nil)
```

Calling `msg.Nak()`, `msg.Term()`, or `msg.NakWithDelay()` does **not** advance the checkpoint in either mode — only a successful `Ack()` or `DoubleAck()` does.

When the durable consumer is deleted and needs to be recreated, the framework creates a new consumer starting at `checkpoint + 1`, skipping already-processed messages and avoiding a full replay.

> **Note:** The checkpoint is per-process and in-memory. If the process itself restarts, the checkpoint resets and the consumer falls back to stream-level state (`AckFloor`).

**Full stream replay (use sparingly):**

```go
c, err := consumer.NewBroadcast(js, "AUDIT", "audit-logger", "events.>", handler,
    consumer.WithRecoveryStrategy(consumer.RecoverFromBeginning),
)
```

### Stream-Missing Hook (Dynamic only)

`RecoveryStrategy` recreates a deleted **consumer** against an
existing stream. When the underlying **stream itself** is gone —
operator wipe, JetStream restart with non-replicated data loss,
disaster recovery — the library cannot recover automatically because
it does not own stream lifecycle. The `StreamMissingHook` is the
operator-driven escalation seam: Parti detects the missing stream,
invokes the hook, and on a `nil` return rebuilds the consumer against
the freshly-recreated stream.

```go
import (
    "context"
    "github.com/arloliu/parti/v2"
    "github.com/arloliu/parti/v2/consumer"
    "github.com/arloliu/parti/v2/provision"
)

hook := func(streamName string) error {
    // The operator-supplied recreate path. parti.Provision exposes
    // a declarative ApplyStream that recreates the stream in place
    // using the same config Parti expects to consume from.
    p, err := provision.New(js)
    if err != nil {
        return err
    }
    _, err = p.ApplyStream(context.Background(), provision.StreamConfig{
        Name:     streamName,
        Subjects: []string{"orders.>"},
        Storage:  provision.FileStorage,
        Replicas: 3,
    })
    return err
}

c, err := consumer.NewDynamic(js, "ORDERS", "processor",
    "orders.{{.PartitionID}}", handler,
    consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
    consumer.WithStreamMissingHook(hook),
)
```

The hook must obey two operator-facing rules (see
[`types.StreamMissingHook`](https://pkg.go.dev/github.com/arloliu/parti/v2/types#StreamMissingHook)
for the full contract):

1. **Same durable name**, if the operator preserves the durable
   consumer alongside the stream recreate. Parti then resumes from
   the preserved `AckFloor` (no replay).
2. **Compatible config** between the preserved consumer and Parti's
   internal config (DeliverPolicy / AckPolicy / InactiveThreshold).
   An incompatible restored consumer surfaces as a wrapped
   `parti.ErrStreamMissing` via the no-hook path below.

If the operator does NOT preserve a consumer (or recreates with a
different durable name), Parti binds a fresh consumer with
`DeliverAllPolicy` and replays the new stream from sequence 1 — the
expected behavior for a true disaster-recovery recreate.

`StreamMissingHook` requires a non-disabled `RecoveryStrategy` —
specifically `RecoverFromLastProcessed` or `RecoverFromBeginning`.
`RecoveryDisabled` (default) and `RecoverFromNew` are rejected at
construction time because the recreated-stream replay override that
prevents fresh-stream message loss only applies to those two
strategies.

**No-hook escalation route.** When the hook is omitted, or when the
operator hook keeps returning an error, the F2 bounded-retry envelope
eventually exhausts. The library fires the `OnPermanentFailure`
dispatcher with the cause wrapped in `parti.ErrStreamMissing`. The
Parti `Manager` observer is **also** notified (application callback
first, then manager observer), so both app observability and
platform self-healing fire regardless of whether an app callback is
registered. The worker enters **terminal** degraded mode with reason
`"stream-missing-recovery-exhausted"` — it stays `Degraded`
permanently until restarted or rotated; stream recreation alone does
not revive the dead partition-consumer loop. The configured
`Hooks.OnError` fires with the wrapped error, and the readiness probe
can rotate the pod. Branch on the typed sentinel inside `OnError`:

```go
mgr, _ := parti.NewManager(&cfg, js, src, strategy,
    parti.WithWorkerConsumerUpdater(c),
    parti.WithHooks(&parti.Hooks{
        OnError: func(ctx context.Context, err error) error {
            if errors.Is(err, parti.ErrStreamMissing) {
                // page the operator, log to the disaster-recovery
                // runbook, etc.
            }
            return nil
        },
    }),
)
```

If your application owns degrade and rotation signaling itself and
wants to suppress the manager's auto-degraded route, pass
`consumer.WithSuppressManagerDegradeOnStreamMissing()` when
constructing the `Dynamic` consumer.

This route is distinct from the generic KV-error degraded-mode
circuit — the named reason `"stream-missing-recovery-exhausted"`
keeps the cross-feature contract that whole-bucket KV loss is the
sole driver of `"KV error threshold exceeded"`.

**Stream deletion / recovery behavior by consumer type.** The
escalation tier above (bounded retries → `OnPermanentFailure` →
terminal `Degraded`) is Dynamic-only. `Queue`, `Static`, and
`Broadcast` do not own stream lifecycle: when the underlying stream
is gone they log a warning and back off indefinitely, and they
self-heal only if the stream returns AND a `RecoveryStrategy` is
enabled — there is no exhaustion or degrade tier for them. When
`RecoveryStrategy` is disabled, these consumers surface each iterator
restart with a Warn log and the iterator-restart metric label
`recovery_disabled` instead of cycling silently.

### Validation

Incompatible combinations return [`ErrInvalidConfig`](https://pkg.go.dev/github.com/arloliu/parti/v2/consumer#ErrInvalidConfig) — detect them with `errors.Is`:

```go
// RecoverFromLastProcessed on a Queue is rejected at NewQueue time.
_, err := consumer.NewQueue(js, "JOBS", "job-workers", "jobs.>", handler,
    consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
)
if errors.Is(err, consumer.ErrInvalidConfig) {
    log.Fatal("incompatible configuration:", err)
}

// RecoverFromNew on a WorkQueuePolicy stream is rejected at Start time.
q, _ := consumer.NewQueue(js, "JOBS", "job-workers", "jobs.>", handler,
    consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
)
if err := q.Start(ctx); errors.Is(err, consumer.ErrInvalidConfig) {
    log.Fatal("incompatible configuration:", err)
}
```

You can also validate a config struct directly without a NATS connection (catches construction-time errors only):

```go
cfg := consumer.QueueConfig{
    StreamName:       "JOBS",
    ConsumerName:     "job-workers",
    FilterSubject:    "jobs.>",
    RecoveryStrategy: consumer.RecoverFromLastProcessed,
}
if err := cfg.Validate(); err != nil {
    // err wraps ErrInvalidConfig
}
```

---

## Functional Options

All consumer constructors accept functional options. The tables below list the
commonly-used options, not the complete set — see the
[package reference](https://pkg.go.dev/github.com/arloliu/parti/v2/consumer) for
every `With*` option.

```go
c, _ := consumer.NewQueue(js, "stream", "consumer", "subject.>", handler,
    consumer.WithLogger(myLogger),
    consumer.WithMetrics(myCollector),
    consumer.WithAckWait(60*time.Second),
    consumer.WithBatchSize(100),
    consumer.WithMaxDeliver(5),
)
```

**Common Options (selected):**

| Option                            | Description                                   |
|-----------------------------------|-----------------------------------------------|
| `WithLogger(logger)`              | Set custom logger                             |
| `WithMetrics(collector)`          | Set metrics collector                         |
| `WithAckWait(duration)`           | Time before message redelivery                |
| `WithBatchSize(n)`                | Messages per fetch                            |
| `WithMaxDeliver(n)`               | Max redelivery attempts                       |
| `WithMaxAckPending(n)`            | Max unacked messages                          |
| `WithFetchTimeout(duration)`      | Max wait when pulling batch                   |
| `WithManualAck(bool)`             | Disable auto-acknowledgement                  |
| `WithInactiveThreshold(duration)` | Consumer cleanup threshold                    |
| `WithRecoveryStrategy(strategy)`  | Auto-recovery on unexpected consumer deletion |
| `WithConsumerMemoryStorage(bool)` | Store per-consumer state in memory (IOPS lever; **not** live-editable; see [Consumer Storage Tuning](#consumer-storage-tuning)) |
| `WithConsumerReplicas(n)`         | Replica count for consumer state (live-editable; `0` inherits the stream's) |

**Broadcast-Specific Options:**

| Option               | Description                    |
|----------------------|--------------------------------|
| `WithInstanceID(id)` | Set unique instance identifier |

**Dynamic-Specific Options:**

| Option                                         | Description                                                                 |
|------------------------------------------------|-----------------------------------------------------------------------------|
| `WithProcessingGate(cfg)`                      | Enable processing gate for ownership control                                |
| `WithDrainOnRemove(enabled, timeout)`          | Drain messages when partitions are removed; bounded by `DrainOnRemoveTimeout` — if loops fail to stop within the bound, `Update` returns an error and the manager retries; tracking entries are cleared so retries converge; an in-flight handler invocation may still run to completion (best-effort, not a zero-overlap guarantee) |
| `WithMaxConcurrentSubjects(n)`                 | Cap concurrent partitions; excess rejects the whole `Update` with `ErrMaxSubjectsExceeded` |
| `WithOnPermanentFailure(fn)`                   | Application callback for permanent partition failure (fires before manager observer) |
| `WithSuppressManagerDegradeOnStreamMissing()`  | Suppress the manager's auto-degraded route for stream-missing exhaustion    |
| `WithConsumerCreateRate(perSec, burst)`        | Enable per-attempt token-bucket rate limiting on consumer-create RPCs (opt-in, default off) — see [Consumer-Create Rate Limiting](#consumer-create-rate-limiting) |
| `WithConsumerCreateClusterRate(clusterPerSec)` | Fleet-size-aware overlay on `WithConsumerCreateRate`; bounds the cluster-wide aggregate to `clusterPerSec` via `min(perSec, clusterPerSec/N)` — requires `WithConsumerCreateRate`, incompatible with `WithConsumerCreateLimiter`, default 0 (off) |
| `WithConsumerCreateLimiter(l)`                 | Inject a custom or shared `consumer.ConsumerCreateLimiter` (build one with `consumer.NewConsumerCreateLimiter`); non-nil value wins over `WithConsumerCreateRate`; nil is a no-op |

---

## Consumer-Create Rate Limiting

**Default:** disabled (nil limiter). Behavior is unchanged until this option is explicitly configured — with no limiter the gate is a nil-safe no-op on the create paths. (The `golang.org/x/time/rate` dependency becomes direct, and the Prometheus throttle series are registered at zero, but no create path is paced.)

Large dynamic-partition assignments (e.g. a fresh source growing to 20 000 partitions) or mass consumer-recovery events can flood the NATS cluster with `CreateOrUpdateConsumer` RPCs. `WithConsumerCreateRate` installs a per-worker token-bucket that gates **every physical RPC attempt** — including retry attempts — across the initial-assignment add loop and the per-partition recovery/recreation paths.

### Usage

```go
c, err := consumer.NewDynamic(
    js, "my-stream", "prefix", "orders.{{.PartitionID}}",
    handler,
    consumer.WithConsumerCreateRate(100, 256), // 100 creates/s, burst 256
)
```

Or inject a shared limiter to pool the budget across multiple `Dynamic` consumers
in the same process:

```go
limiter, err := consumer.NewConsumerCreateLimiter(100, 256) // 100 creates/s, burst 256
if err != nil { /* perSec must be > 0, burst >= 1 */ }
c1, _ := consumer.NewDynamic(js, "stream-a", ..., consumer.WithConsumerCreateLimiter(limiter))
c2, _ := consumer.NewDynamic(js, "stream-b", ..., consumer.WithConsumerCreateLimiter(limiter))
```

`ConsumerCreateLimiter` is a one-method interface (`Wait(ctx) error`), so you can
also supply your own implementation. A shared/injected limiter does not emit the
per-consumer throttle metrics that `WithConsumerCreateRate` wires up.

### Per-attempt gating

The rate gate fires before **each physical `CreateOrUpdateConsumer` call**, including retries inside `EnsureConsumerWithOptions` and `partitionConsumer.ensureConsumer`. This prevents the 3× retry amplification that would occur if gating were per-logical-create only (exactly when the cluster is already stressed).

### Handoff and readiness interactions

A paced apply holds `applyStoreMu` for its full duration, serialising subsequent applies and blocking `Close`. Additionally:

- **Processing overlap (gate-off):** Two-phase handoff alone does NOT prevent processing overlap (see [`LIFECYCLE.md`](LIFECYCLE.md) §Two-Phase Handoff). With the processing gate **OFF** (the default), enabling create-rate limiting lengthens the window during which the old and new owners are both active. Co-enable the processing gate / pull-gating (`WithProcessingGate`, `WithPullGating`) to suppress pulls for not-yet-committed partitions.

- **Startup watchdog:** `StartupTimeout` (default 60 s) fires `enterDegraded("startup-timeout")` if the worker is still in `StateWaitingAssignment` at the deadline. A paced large cold start (e.g. 200 s at 100/s for 20 000 partitions) will trip this. The watchdog is state-guarded and does **not** abort the apply — it only affects readiness probe rotation. Size `StartupTimeout ≥ ColdStartWindow + ElectionTimeout + estimated paced-apply duration + headroom`, or accept an intentional one-shot startup-degraded rotation.

- **Cancellation / no-partial-commit:** A `Wait` error (e.g. context cancel on shutdown) propagates up, causing the apply to fail pre-commit. `CreateOrUpdateConsumer` is idempotent and `UpdateWorkerConsumer` re-derives `toAdd` from current state on retry, so partial progress is safe and resumable.

### Sizing

```
recommended rate ≈ cluster-create-budget / max-workers
recommended burst ≈ 256 (absorbs small reassignments instantly; validate by load test)
```

Starting values (validate against your cluster): `rate ≈ 100/s, burst ≈ 256`.

### Fleet-size-aware (adaptive) rate: WithConsumerCreateClusterRate

**Default:** 0 (disabled). Requires `WithConsumerCreateRate`. Incompatible with `WithConsumerCreateLimiter`.

`WithConsumerCreateClusterRate(clusterPerSec)` adds a fleet-size-aware overlay
on top of `WithConsumerCreateRate`. Each worker enforces an effective rate of:

```
effective rate = min(perSec, clusterPerSec / N)
```

where `N` is the committed worker-count the manager observes live (updated each
time the committed assignment changes). This bounds the **steady-state** cluster-wide
aggregate to `clusterPerSec` instead of letting it grow as `N × perSec`.

```go
c, err := consumer.NewDynamic(
    js, "my-stream", "prefix", "orders.{{.PartitionID}}",
    handler,
    consumer.WithConsumerCreateRate(100, 256),        // per-worker ceiling + burst
    consumer.WithConsumerCreateClusterRate(500),       // cluster-wide target: 500/s
)
```

**Requirements and restrictions:**

- `WithConsumerCreateRate` must also be set — it supplies the per-worker ceiling
  (`perSec`) and burst. `WithConsumerCreateClusterRate` is rejected at `NewDynamic`
  if used alone.
- Incompatible with `WithConsumerCreateLimiter`: an injected/shared limiter is a
  fixed-rate object that cannot be adaptively retuned, so the combination is rejected
  at `NewDynamic`.
- `clusterPerSec` must be ≥ 0; setting it to 0 disables the adaptive overlay and
  reverts to static per-worker behaviour.

**Sizing guidance:**

```
clusterPerSec ≈ cluster-wide create budget (measure under load)
perSec        ≈ safe per-worker ceiling (transient overshoot cap)
burst         ≈ 256 (absorb small reassignments; keep small if aggregate burst matters)
```

**Caveats:**

- **Steady-state guarantee, not instantaneous.** The effective rate converges once
  all workers in the fleet have observed the same committed N. During a scale-out
  or scale-in transition, workers briefly disagree on N; `perSec` (the per-worker
  ceiling) bounds the per-worker transient overshoot until convergence.
- **Aggregate burst is `Σ burst` across workers, not bounded by `clusterPerSec`.**
  A burst of 256 on 20 workers means the cluster can absorb 5 120 creates instantly
  before the rate kicks in. Keep `burst` small if aggregate burst matters to your
  cluster.
- **Observation lag.** Worker-count updates are eventually-consistent; the worst-case
  lag is the assignment-watcher reconcile floor (approximately 30 s). Retuning is
  not instantaneous.

### Throttle metrics (optional sidecar)

If your `types.WorkerConsumerMetrics` implementation also satisfies `durable.ConsumerCreateThrottleObserver`, throttle events are automatically emitted:

```go
type ConsumerCreateThrottleObserver interface {
    IncrementConsumerCreateThrottled()
    ObserveConsumerCreateThrottleWait(seconds float64)
}
```

Existing metrics implementations that do not define these methods are unaffected — the type-assert simply fails silently.

---

## Consumer Storage Tuning

> **Scope:** this section is about **per-consumer JetStream state** (each
> consumer's own ack/cursor store), tuned with `WithConsumerMemoryStorage` and
> `WithConsumerReplicas`. It is **unrelated** to parti's coordination KV buckets
> (heartbeat/election/etc.), which always use `FileStorage` — see
> [`CONFIGURATION.md`](CONFIGURATION.md). Do not move the coordination buckets to
> memory.

Each consumer persists its delivery state (ack floor, redelivery counts) to a
small per-consumer file. At scale this is the **dominant** NATS write-IOPS
cost — measured at **72–81% of cluster write IOPS** with one consumer per
partition. Moving that state to memory is the single largest IOPS lever.

**It is conditional — the file-backed default is correct for most deployments.**
Only reach for memory storage when one of these is true: ≳10k partitions per
NATS node, provisioned-IOPS billing (e.g. AWS io2/gp3 above baseline),
latency-tail sensitivity, or a tightly-constrained dev/test cluster. Otherwise,
do nothing.

When you *do* need it, choose by how much redelivery the workload tolerates:

| Config | Set | IOPS reduction | Redelivery exposure |
|--------|-----|----------------|---------------------|
| **Default** | (nothing) | baseline | None beyond JetStream's own |
| **Balanced** | `WithConsumerMemoryStorage(true)` + replicas inherited (R≥3) | ~90% at N=1000, ~72% at N=3000 | Redelivery only on a **coordinated cluster-wide restart**; keeps consumer HA |
| **Aggressive** | `WithConsumerMemoryStorage(true)` + `WithConsumerReplicas(1)` | ~99%; per-partition cost goes flat in N | Redelivery on **single-node failure** too — correct only for idempotent handlers |

```go
// Balanced: recommended once the decision above reaches "yes".
c, _ := consumer.NewDynamic(js, "ORDERS", "processor", "orders.{{.PartitionID}}", handler,
    consumer.WithConsumerMemoryStorage(true),  // not live-editable — set before first start
    consumer.WithConsumerReplicas(3),           // live-editable; on a 3-replica stream
)
```

Notes:

- `WithConsumerMemoryStorage` is **not** live-editable — changing it requires
  recreating the consumer. `WithConsumerReplicas` **is** live-editable.
- On `InterestPolicy`/`WorkQueuePolicy` streams, consumer replicas must equal the
  stream's replicas, so the Aggressive (R=1) row is unavailable there — use the
  Balanced row. See [Stream Retention Policy](#stream-retention-policy).
- For capacity numbers (RSS per partition, latency) see the "NATS-Side Cost"
  section in [`OPERATIONS.md`](OPERATIONS.md).

---

## Migrating from Legacy Consumer APIs

The `consumer` package unifies the legacy consumer APIs that previously lived in
the `subscription` package and in `partition.JSConsumer`.

### subscription.WorkerConsumer → consumer.Dynamic

```go
// Legacy (deprecated)
import "github.com/arloliu/parti/subscription"

cfg := subscription.WorkerConsumerConfig{
    StreamName:      "ORDERS",
    ConsumerPrefix:  "processor",
    SubjectTemplate: "orders.{{.PartitionID}}",
}
wc, _ := subscription.NewWorkerConsumer(js, cfg, handler)
defer wc.Close(ctx)

// New (recommended)
import "github.com/arloliu/parti/v2/consumer"

c, _ := consumer.NewDynamic(js, "ORDERS", "processor", "orders.{{.PartitionID}}", handler)
defer c.Stop(ctx)
```

### subscription.BroadcastConsumer → consumer.Broadcast

```go
// Legacy (deprecated)
cfg := subscription.BroadcastConsumerConfig{
    StreamName:     "EVENTS",
    ConsumerPrefix: "broadcast",
    WildcardFilter: "events.>",
}
bc, _ := subscription.NewBroadcastConsumer(js, cfg, handler)
defer bc.Close(ctx)

// New (recommended)
c, _ := consumer.NewBroadcast(js, "EVENTS", "broadcast", "events.>", handler)
defer c.Stop(ctx)
```

### partition.JSConsumer → consumer.Static

```go
// Legacy (deprecated)
import "github.com/arloliu/parti/partition"

cfg := partition.ConsumerConfig{
    StreamName:     "EVENTS",
    ConsumerName:   "processor-0",
    SubjectPattern: "events.{{partition}}",
    NumPartitions:  10,
    Partition:      0,
}
pc, _ := partition.NewJSConsumer(js, cfg, handler)
defer pc.Stop(ctx)

// New (recommended)
c, _ := consumer.NewStatic(js, "EVENTS", "processor-0", "events.{{partition}}", 10, 0, handler)
defer c.Stop(ctx)
```

### Key Differences

| Aspect          | Legacy Packages             | consumer Package          |
|-----------------|-----------------------------|---------------------------|
| API Style       | Config struct + constructor | Positional args + options |
| Method Names    | `Close()` / mixed           | `Stop()` (consistent)     |
| Package Count   | 2 packages                  | 1 unified package         |
| Message Handler | Package-specific            | Unified `MessageHandler`  |

---

## Legacy Consumer APIs

> **Note:** The `subscription` package was removed in v2, and the old
> `partition.JSConsumer` API was replaced by `consumer.Static`. The public
> `partition` package still exists for static routing and publisher/subscriber
> helpers.

### subscription Package

The `subscription` package provided `WorkerConsumer` and `BroadcastConsumer` for JetStream integration with Parti.

**Status:** Deprecated. Use `consumer.Dynamic` and `consumer.Broadcast` instead.

### partition Package

The `partition` package previously exposed `JSConsumer` for static partitioning
scenarios (for example, StatefulSet ordinal-based assignment).

**Status:** `partition.JSConsumer` was removed. Use `consumer.Static` instead;
other `partition` APIs remain supported.

### CompositeConsumerUpdater

The `parti.CompositeConsumerUpdater` continues to work with the new consumer types:

```go
dynamic, _ := consumer.NewDynamic(js, "orders", "processor", "orders.{{.PartitionID}}", handler1)
broadcast, _ := consumer.NewBroadcast(js, "events", "updater", "events.>", handler2)

composite := parti.NewCompositeConsumerUpdater(dynamic, broadcast)

mgr, _ := parti.NewManager(cfg, js, src, strategy.NewConsistentHash(),
    parti.WithWorkerConsumerUpdater(composite),
)
```

---

## Architecture

### Dynamic Consumer Internals

```
    ┌──────────────────────────────────────────────────────┐
    │                    Dynamic Consumer                   │
    │                                                       │
    │   ┌─────────────┐    ┌─────────────────────────────┐ │
    │   │   Manager   │    │  Per-Partition Consumers    │ │
    │   │  Callback   │───▶│  orders.0, orders.1, ...    │ │
    │   └─────────────┘    └──────────────┬──────────────┘ │
    │                                      │                │
    │   Subject Template:                  │  Filter:       │
    │   "orders.{{.PartitionID}}"          │  Multi-subject │
    └──────────────────────────────────────┼────────────────┘
                                           │
                                           ▼
                                    ┌─────────────┐
                                    │   Stream    │
                                    │  "ORDERS"   │
                                    └─────────────┘
```

### Queue Consumer Internals

```
    ┌────────────────────────────────────────────────────────┐
    │                     Queue Consumer                      │
    │                                                         │
    │   ┌────────────┐  ┌────────────┐  ┌────────────┐       │
    │   │ Instance A │  │ Instance B │  │ Instance C │       │
    │   │  (shared)  │  │  (shared)  │  │  (shared)  │       │
    │   └─────┬──────┘  └─────┬──────┘  └─────┬──────┘       │
    │         │               │               │               │
    │         └───────────────┼───────────────┘               │
    │                         │                               │
    │   Consumer Name:        │  Durable: "job-workers"       │
    │   (shared by all)       │  Load-balanced delivery       │
    └─────────────────────────┼───────────────────────────────┘
                              │
                              ▼
                       ┌─────────────┐
                       │   Stream    │
                       │   "JOBS"    │
                       └─────────────┘
```

---

## Thread Safety

All consumer types are thread-safe. Lifecycle methods (`Start`, `Stop`, `Update`) are serialized internally to prevent race conditions.

| Method          | Thread Safety                             |
|-----------------|-------------------------------------------|
| `Start(ctx)`    | Serialized, call once                     |
| `Stop(ctx)`     | Serialized, idempotent                    |
| `Update(...)`   | Serialized, can call multiple times       |
| Message Handler | Called from single goroutine per consumer |

---

## Error Handling

**Constructor Errors:**
- Validation failures (nil JetStream, missing required fields, incompatible option combinations) wrap `consumer.ErrInvalidConfig` — use `errors.Is(err, consumer.ErrInvalidConfig)` to detect them programmatically.

**Runtime Errors:**
- `context.DeadlineExceeded` - Stop timed out
- `consumer.ErrWorkerIDMutation` - Dynamic consumer workerID changed unexpectedly
- `consumer.ErrMaxSubjectsExceeded` - Assignment's deduped subject count exceeds `MaxConcurrentSubjects`; the whole `Update` is rejected before any mutation; the manager retries with backoff and previous owners keep consuming
- `consumer.ErrConsumerStopped` - `Start`/`Update` called after `Stop`/`Close`. Stop is terminal for `Static`, `Broadcast`, and `Dynamic`: construct a new consumer to resume consuming. Deregister the consumer (or stop the manager first) before stopping it, or manager-driven updates will retry loudly against the stopped consumer
