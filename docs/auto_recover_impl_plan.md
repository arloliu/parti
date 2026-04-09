# Auto-Recovery and Replay Storm Prevention

This document proposes a unified mechanism across all `parti` consumer types (`Queue`, `Static`, `Dynamic`, `Broadcast`) to automatically recover from durable consumer deletion while preventing "backlog replay storms".

## Background Context

Currently, consumers call `ensureConsumer` at startup. If the durable consumer is unexpectedly deleted on the NATS JetStream server (e.g., expired `InactiveThreshold`, accidental CLI deletion, or KV metadata loss):
- `Queue` and `Broadcast` fail to iterate and typically block without recreation.
- `Dynamic` and `Static` (via `partitionConsumer`) have `maybeEscalateIteratorFailures` which attempts to recreate the consumer.
- **The Weakpoint & Replay Storm**: Recreating a consumer uses the original `jetstream.ConsumerConfig`. By default, NATS sets `DeliverPolicy: jetstream.DeliverAllPolicy`. Thus, when deleted, JetStream forgets its `AckFloor`. Upon recreation, the stream *replays all messages from the very beginning*, causing a massive backlog replay storm.

## Design Decisions

### Detection: Iterator Errors Only (No Background Poller)

Empirically verified against a real embedded NATS server (see `TestVerify_InactiveThresholdSemantics` in `test/integration/consumer/inactive_threshold_verify_test.go`), consumer deletion surfaces through **two distinct paths** depending on whether an active pull request is in flight at the moment of deletion.

**Path A — Active pull (Next() is blocked, waiting for a message):**
When `iter.Next()` is blocked and a pull request is actively waiting on the server, deleting the consumer causes the server to send a 409 response to the pending pull within ~50ms. The nats-go client translates this into `jetstream.ErrConsumerDeleted`, which `iter.Next()` returns. Detection is immediate.

**Path B — Between pulls (Next() has returned and is not being called again):**
When no active pull request is waiting on the server (handler is processing, or consumer has just been drained), the deletion is **not** delivered as a 409. Instead, the next heartbeat interval arrives from the server — but the consumer is gone, so no heartbeat reply is sent. After 2 missed heartbeats (2 × `PullHeartbeat`, defaulting to ~1s), nats-go returns `jetstream.ErrNoHeartbeat`. This is the primary deletion detection path in practice because most delete events happen while the handler is executing between consecutive `Next()` calls.

**Critical observable behaviours (empirically verified):**
- After `ErrNoHeartbeat`, the iterator does **not** self-stop. Subsequent `Next()` calls on the same iterator return `ErrNoHeartbeat` repeatedly.
- Creating a brand-new `cons.Messages(...)` iterator on a stale consumer handle (where the consumer is already deleted server-side) also returns `ErrNoHeartbeat` repeatedly — **not** `ErrConsumerDeleted`. There is no second-attempt 409.
- The **only reliable confirmation** that the consumer is gone, after receiving `ErrNoHeartbeat`, is to call `consumer.Info()`, which returns `jetstream.ErrConsumerNotFound` (HTTP 404) immediately.

**Detection latency summary:**
- Active-pull deletion: ~50ms (`ErrConsumerDeleted` directly from `iter.Next()`).
- Between-pulls deletion: ~2 × `PullHeartbeat` (~1s with default heartbeat of 500ms).

A background `consumer.Info()` poller was considered but rejected:
- **Covered by heartbeat path**: `ErrNoHeartbeat` + a single `consumer.Info()` call on burst is equivalent and requires no dedicated goroutine.
- **Wasteful**: Polling `Info()` on every consumer (potentially hundreds in a `Dynamic` setup) generates sustained API load.
- **Complex**: Introduces a second goroutine lifecycle that races with iterator-based detection.

All consumer types will rely solely on iterator error detection plus an on-demand `consumer.Info()` confirmation for triggering recovery.

### `InactiveThreshold` Expiry Is a First-Class Recovery Source

This plan must treat server-side cleanup caused by `InactiveThreshold` exactly the same as an explicit `DeleteConsumer`.

For pull consumers, JetStream defines inactivity as: **no pull requests received by the server for longer than `InactiveThreshold`**. It is not based on whether messages are flowing.

Empirically verified practical consequences:
- The nats-go `Messages()` iterator does **not** issue pull requests autonomously. It only has an active pull on the server when `iter.Next()` is blocked waiting for a message. When `Next()` is not being called (handler is processing, or nobody is draining the iterator), **no pull request is on the server**.
- Therefore, **a slow handler causes InactiveThreshold expiry**. If the time between one `Next()` call returning and the next `Next()` call being made exceeds `InactiveThreshold`, the consumer expires. This was verified empirically: with `InactiveThreshold=3s`, sleeping for 6s between `Next()` calls expired the consumer.
- **A running but never-drained iterator also expires the consumer.** If `Messages()` is called but `Next()` is never called, the consumer expires within `InactiveThreshold`.
- `InactiveThreshold` expiry therefore occurs whenever `Next()` is not actively blocking: slow handlers, recovery/backoff windows, and process downtime are all equivalent from the server's perspective.
- When expiry happens, the detection path is the same as for between-pulls explicit deletion: `ErrNoHeartbeat` after 2 × `PullHeartbeat`, followed by `ErrConsumerNotFound` on `consumer.Info()` confirmation.

Test plans must stop the iterator (or not call `Next()` for long enough) and wait for `InactiveThreshold` to trigger passive expiry. Note the minimum `PullExpiry` enforced by nats-go is **1 second** — set `InactiveThreshold` materially above 1s (e.g., 3s) and sleep for at least 2× that value to make tests deterministic.

### Initial Create vs Recovery Recreate

`ensureConsumer()` is called both during initial `Start()` and during failure recovery. The `DeliverPolicy` override must only apply during *recovery*, not initial creation. On initial creation, the user's original config (or server default) is respected.

A separate `recoverConsumer()` method builds a fresh config from the stored base config + recovery parameters each time, avoiding permanent mutation of the stored `consumerConfig`.

### Default: Disabled (opt-in)

Auto-recovery is disabled by default (zero value = no recovery). This preserves current behavior for existing users. Enabling it is a conscious decision since it silently alters `DeliverPolicy` on recreation.

### Error Classification: Recover Only on Confirmed Consumer Deletion

Recovery must only trigger for errors that mean the durable consumer is actually gone. Not all iterator failures should recreate the consumer.

**Empirically verified sentinel behaviour:**

There are **three relevant errors**, each arising from a distinct path. Understanding which one surfaces and when is critical to implement correct recovery trigger logic:

| Error | Source | When it surfaces |
|---|---|---|
| `jetstream.ErrConsumerDeleted` | `iter.Next()` | **Only** when consumer is deleted while an active pull request is in flight (i.e., `Next()` is currently blocking). ~50ms detection latency. |
| `jetstream.ErrNoHeartbeat` | `iter.Next()` | When consumer is deleted (or becomes unreachable) **between** `Next()` calls — no active pull at the time of deletion. ~2× `PullHeartbeat` (~1s) detection latency. **Also returned by a new iterator on a stale consumer handle.**  |
| `jetstream.ErrConsumerNotFound` | `consumer.Info()` | API confirmation call after `ErrNoHeartbeat` burst. Returned immediately (no network wait). |

**Key finding**: `ErrNoHeartbeat` is the **most common** deletion detection signal (covers slow handlers, backoff gaps, and passive expiry), while `ErrConsumerDeleted` only fires for the narrower active-pull case. After `ErrNoHeartbeat`, the iterator does **not** self-stop and does **not** transition to `ErrConsumerDeleted` — it returns `ErrNoHeartbeat` on every subsequent `Next()` call, indefinitely.

Classification rules:
- **Recover immediately** on `ErrConsumerDeleted` — this is a confirmed 409 from the server; no further check needed.
- **Check then recover** on `ErrNoHeartbeat` burst — after the existing burst threshold, call `consumer.Info()`. If it returns `ErrConsumerNotFound`, trigger recovery. If `Info()` succeeds or returns a transient error, stay on the existing iterator-restart and jittered-backoff path (likely a transient network issue, not a deleted consumer).
- **Do not recover** on normal shutdown paths: `context.Canceled` or `jetstream.ErrMsgIteratorClosed`.
- **Do not hot-loop** on recovery failures from invalid config, missing stream, or permissions — log distinctly and use bounded backoff.

For `partitionConsumer`, the existing `maybeEscalateIteratorFailures` burst-then-confirm pattern is already structurally correct for the `ErrNoHeartbeat` path. `ErrConsumerDeleted` adds a new bypass: it skips the burst threshold and recovers immediately.

The existing `natsutil.IsConsumerNotFound` covers API-path confirmation. A new `natsutil.IsConsumerGone` helper adds `ErrConsumerDeleted` for the iterator fast path.

### Ack-Path Error Behavior

`msg.Ack()` is a fire-and-forget `nats.Conn.Publish` to the reply subject. It succeeds as long as the NATS connection is up, regardless of whether the consumer still exists server-side. A consumer deletion does **not** cause `msg.Ack()` to fail.

Only `msg.DoubleAck()` (sync `Request`) could theoretically surface a consumer-deleted error, but `DoubleAck` is not used in any parti consumer loop.

Consequence: **the ack path is not a detection vector for consumer deletion.** All detection flows through `iter.Next()` errors. The checkpoint tracker should still advance on successful Ack calls because the ack was published (even if the server may discard it), and the message was already processed by the handler. On recovery, any messages between the checkpoint and the true server-side ack floor will simply be redelivered (at-least-once semantics).

### Recovery Serialization

Each consumer instance must allow only one recovery attempt in flight at a time.

- Add a per-consumer `recoverMu` or atomic in-progress guard.
- Repeated iterator errors or confirmation-path not-found errors while recovery is running should not start parallel recreations.
- A successful recovery swaps the consumer handle under lock and resets relevant transient failure counters.
- A failed recovery keeps the current backoff semantics and never races against shutdown.

### ManualAck Scope for `RecoverFromLastProcessed`

The current plan overstates `ManualAck` support. Tracking handler return is not sufficient, and tracking only the helper-owned `msg.Ack()` path does not observe user-driven `Ack`, `AckSync`, or `DoubleAck` calls.

For the initial implementation:
- `RecoverFromLastProcessed` is supported only when `ManualAck=false`.
- Validation should reject `RecoverFromLastProcessed` with `ManualAck=true`.
- A future version can lift this restriction by introducing a full `jetstream.Msg` wrapper that intercepts all successful ack variants.

This keeps the first version correct and avoids claiming stronger resume semantics than the implementation can actually guarantee.

## Proposed Changes

### 1. `consumer/options.go` — Recovery Strategy and Option

Add a `RecoveryStrategy` type and a single `WithRecoveryStrategy(strategy)` functional option, consistent with the existing option pattern:

```go
// RecoveryStrategy defines how a recreated consumer decides where to resume
// after an unexpected deletion. The zero value means recovery is disabled.
type RecoveryStrategy int

const (
	// RecoveryDisabled is the zero value. No auto-recovery is performed.
	// The consumer will fail on iterator errors without attempting recreation.
	// This preserves the current default behavior.
	RecoveryDisabled RecoveryStrategy = iota

	// RecoverFromNew recreates the consumer to only receive newly published messages.
	// Maps to: DeliverPolicy = DeliverNewPolicy
	// Pros: Zero replay storm. Safe default for Queue consumers.
	// Cons: Any unacknowledged messages since deletion are skipped (data loss).
	RecoverFromNew

	// RecoverFromLastProcessed recreates the consumer starting at
	// (highest_acked_stream_sequence + 1).
	// Maps to: DeliverPolicy = DeliverByStartSequencePolicy, OptStartSeq = tracked + 1
	//
	// The consumer tracks a recovery checkpoint seeded from consumer info and
	// advanced on successful helper-owned ack.
	//
	// Pros: Minimizes gaps and replays; preserves at-least-once semantics closely.
	// Cons: Requires a trustworthy checkpoint. In the initial implementation,
	// it is supported only when ManualAck=false and is not supported for Queue
	// consumers because shared durables make cross-instance resume nondeterministic.
	RecoverFromLastProcessed

	// RecoverFromBeginning recreates the consumer to deliver all messages in the stream.
	// Maps to: DeliverPolicy = DeliverAllPolicy
	// WARNING: Causes a complete backlog replay storm. Use only for small/bounded streams.
	RecoverFromBeginning
)
```

The `options` struct gains a single field:

```go
// In the existing options struct:
recoveryStrategy RecoveryStrategy // zero value = RecoveryDisabled
```

Exposed via:

```go
// WithRecoveryStrategy enables auto-recovery on consumer deletion and controls
// the DeliverPolicy used when recreating the consumer.
//
// By default, recovery is disabled (RecoveryDisabled). When enabled, iterator
// errors that indicate consumer deletion trigger automatic recreation with the
// specified strategy.
func WithRecoveryStrategy(strategy RecoveryStrategy) Option {
	return optionFunc(func(o *options) {
		o.recoveryStrategy = strategy
	})
}
```

Validation rules for the initial implementation:
- `Queue` supports only `RecoveryDisabled`, `RecoverFromNew`, and `RecoverFromBeginning`.
- `Broadcast`, `Static`, and `Dynamic` support `RecoverFromLastProcessed` only when `ManualAck=false`.

### 2. Implementation in Consumer Types

#### Recovery Checkpoint Tracking (for `RecoverFromLastProcessed`)

The recovery checkpoint should not rely only on in-process acks. A newly started process may bind to an existing durable whose server-side `AckFloor` is already far ahead, and the durable may still be deleted before this process acks its first message.

Use a `recoveryCheckpoint` tracker with two inputs:
- **Seed from server state** after the initial `ensureConsumer()` or successful recovery by reading the consumer's current ack floor from `ConsumerInfo`.
- **Advance from successful helper-owned ack** when `ManualAck=false`.

This avoids the "no local checkpoint yet" replay-storm edge case without reintroducing a background polling loop.

```go
// recoveryCheckpoint stores the highest known safe resume point.
// It is seeded from consumer info and advanced on successful helper-owned ack.
type recoveryCheckpoint struct {
	maxAckedStreamSeq atomic.Uint64
}

func (t *recoveryCheckpoint) seed(streamAckFloor uint64) {
	for {
		old := t.maxAckedStreamSeq.Load()
		if streamAckFloor <= old || t.maxAckedStreamSeq.CompareAndSwap(old, streamAckFloor) {
			return
		}
	}
}

// advance is called after a successful msg.Ack() when ManualAck=false.
// It monotonically advances the checkpoint to the message's stream sequence.
func (t *recoveryCheckpoint) advance(msg jetstream.Msg) {
	md, err := msg.Metadata()
	if err != nil {
		return // reply subject unparseable; skip silently
	}
	seq := md.Sequence.Stream
	for {
		old := t.maxAckedStreamSeq.Load()
		if seq <= old || t.maxAckedStreamSeq.CompareAndSwap(old, seq) {
			return
		}
	}
}
```

> **Note:** `msg.Metadata()` parses the reply subject string — no network call, nanosecond overhead. Safe for high-throughput paths.

#### Recovery Config Builder

Each consumer type gets a `recoverConsumer()` method that builds a *fresh* config from the stored base config + recovery parameters. The stored `consumerConfig` is never mutated.

```go
func recoveryConfig(base jetstream.ConsumerConfig, strategy RecoveryStrategy, checkpoint uint64) (jetstream.ConsumerConfig, string) {
	cfg := base // copy

	// Clear stale delivery-policy fields from the base config to prevent
	// confusion when switching policies (e.g., base had OptStartSeq set).
	cfg.OptStartSeq = 0
	cfg.OptStartTime = nil

	switch strategy {
	case RecoverFromNew:
		cfg.DeliverPolicy = jetstream.DeliverNewPolicy
	case RecoverFromLastProcessed:
		if checkpoint == 0 {
			cfg.DeliverPolicy = jetstream.DeliverNewPolicy
			return cfg, "fallback_no_checkpoint"
		}
		cfg.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
		cfg.OptStartSeq = checkpoint + 1
	case RecoverFromBeginning:
		cfg.DeliverPolicy = jetstream.DeliverAllPolicy
	}
	return cfg, ""
}
```

If `RecoverFromLastProcessed` has no checkpoint yet, recovery falls back to `RecoverFromNew` and emits a structured warning. This is lossy, but it avoids a replay storm and keeps the behavior explicit.

#### Recovery Trigger Helper

Centralize recovery classification so all consumer types use the same rules:

```go
// shouldRecoverConsumer returns true only for ErrConsumerDeleted —
// the unambiguous 409 fast path that requires no further confirmation.
// ErrNoHeartbeat is handled separately because it requires an Info() check
// to distinguish transient network issues from actual consumer deletion.
func shouldRecoverConsumerImmediate(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, context.Canceled):
		return false
	case errors.Is(err, jetstream.ErrMsgIteratorClosed):
		return false
	default:
		return errors.Is(err, jetstream.ErrConsumerDeleted)
	}
}

// shouldRecoverAfterInfo is called after consumer.Info() returns ErrConsumerNotFound
// (i.e., the existing maybeEscalateIteratorFailures confirm-path).
// This handles the ErrNoHeartbeat → consumer.Info() → ErrConsumerNotFound path.
func shouldRecoverAfterInfo(infoErr error) bool {
	return natsutil.IsConsumerNotFound(infoErr)
}
```

#### [NEW] `internal/natsutil/errors.go` — Consumer-Gone Detection

Extend the error detection helpers to cover both the iterator-path and API-path sentinels:

```go
// IsConsumerGone reports whether err is an unambiguous 409 "consumer deleted"
// signal from iter.Next(). This is the fast path — no API confirmation needed.
// Do NOT include ErrNoHeartbeat here: it is ambiguous (could be transient network)
// and requires a consumer.Info() check before triggering recovery.
func IsConsumerGone(err error) bool {
	return errors.Is(err, jetstream.ErrConsumerDeleted)
}
```

The existing `IsConsumerNotFound` is preserved for the `consumer.Info()` confirmation path.

#### [MODIFY] `consumer/queue.go`

- Add `recoveryStrategy RecoveryStrategy`, `recoverMu`, and a recovery in-progress guard to `Queue`.
- Reject `RecoverFromLastProcessed` during Queue construction. Shared durables make per-process checkpoint-based resume unsafe and nondeterministic.
- Classify `iter.Next()` errors: recover only on confirmed consumer deletion. Keep existing backoff for other iterator errors.
- `recoverConsumer()` builds a fresh config with `recoveryConfig(...)` and rebinds with `jsutil.EnsureConsumer`.
- **Default recommendation for Queue**: `RecoverFromNew`, documented in godoc.

#### [MODIFY] `internal/durable/broadcast_consumer.go`

- Add `recoveryStrategy`, `recoveryCheckpoint`, `recoverMu`, and a recovery in-progress guard to `BroadcastConsumer`.
- Seed the checkpoint from consumer info after initial bind and after each successful recovery.
- When `ManualAck=false`, advance the checkpoint after successful helper-owned `msg.Ack()`.
- Detect `ErrConsumerDeleted` from iterator errors via `shouldRecoverConsumerImmediate` — bypass normal backoff and recover immediately.
- Keep `ErrNoHeartbeat` on the existing restart/backoff path; on burst threshold, call `consumer.Info()` — if `ErrConsumerNotFound`, call `recoverConsumer()`.
- Add `recoverConsumer()` using `recoveryConfig()`.

#### [MODIFY] `internal/durable/partition_consumer.go`

- Add `recoveryStrategy`, `recoveryCheckpoint`, `recoverMu`, and a recovery in-progress guard to `partitionConsumer`.
- Seed the checkpoint from consumer info after initial bind and after each successful recovery.
- When `ManualAck=false`, advance the checkpoint after successful helper-owned `msg.Ack()`.
- On `ErrConsumerDeleted` (detected via `shouldRecoverConsumerImmediate`), bypass the burst threshold and call `recoverConsumer()` immediately.
- Keep the existing `maybeEscalateIteratorFailures` logic for `ErrNoHeartbeat`: burst escalation → `consumer.Info()` → if `ErrConsumerNotFound`, call `recoverConsumer()`. This two-step path is the most common deletion detection route.
- Ack-path detection is not needed (fire-and-forget Publish).
- `recoverConsumer()` uses `recoveryConfig()` to build a fresh config from the stored base.

#### [NEW] `internal/natsutil/errors.go`

- Add `IsConsumerGone(err) bool` that checks only `ErrConsumerDeleted` — the unambiguous 409 fast path from `iter.Next()`.
- Preserve existing `IsConsumerNotFound` for the `consumer.Info()` confirmation path after `ErrNoHeartbeat` burst.
- Do NOT include `ErrNoHeartbeat` in `IsConsumerGone` — it is ambiguous and requires API confirmation first.

#### [MODIFY] `internal/durable/config.go`

- Thread `RecoveryStrategy` through `WorkerConsumerConfig` → `partitionConsumerConfig`.
- Add validation to reject `RecoverFromLastProcessed` when `ManualAck=true`.

#### [MODIFY] `consumer/options.go`

- Add `recoveryStrategy RecoveryStrategy` to `options` struct.
- Add `WithRecoveryStrategy` option function.

### 3. Error Handling Requirements

- Recovery attempts must log `strategy`, `reason`, `durable`, and `subject` where applicable.
- Add outcome-oriented metrics such as `recovery_attempt_total{reason,outcome,strategy}` and `recovery_fallback_total{reason}`.
- Preserve the original error in logs when recovery fails; do not replace it with a generic message.
- If another process or goroutine wins the recreate race first, rebinding successfully via `EnsureConsumer` counts as a successful recovery.
- If shutdown begins while recovery is in progress, recovery must abort and not restart the consumer loop.

### 4. Edge Cases and Explicit Behavior

- **Delete while message is in flight**: `msg.Ack()` is fire-and-forget and will not fail when the consumer is deleted. The checkpoint advances because the handler already processed the message. On recovery, messages between the local checkpoint and the server-side ack floor may be redelivered; handlers must remain idempotent.
- **Fresh process, no local ack yet**: `RecoverFromLastProcessed` first seeds from consumer info. If there is still no usable checkpoint, fall back to `RecoverFromNew` with a warning.
- **Repeated deletion signals**: concurrent delete-related iterator errors and API-path not-found confirmations are serialized behind one recovery attempt.
- **Generic transport instability**: no consumer recreation until a confirmed consumer-deleted error is observed; otherwise stay on iterator restart + backoff.
- **Queue shared durable**: checkpoint-based resume is intentionally out of scope for v1 because multiple processes can race with different local checkpoints.
- **`DeliverLastPerSubject` is not part of the initial design**: it can skip intermediate backlog messages and is not a reliable replacement for resume-from-ack-floor semantics.
- **Stream retention / compaction**: if `OptStartSeq` in `RecoverFromLastProcessed` points to a sequence that has been removed by stream retention policy, NATS delivers from the next available sequence. This is correct at-least-once behavior — some old messages are simply gone. No special handling needed.
- **Seeding failure**: if `ConsumerInfo()` fails after `ensureConsumer()` succeeds (transient error), log a warning and proceed with checkpoint=0. The fallback-to-`RecoverFromNew` path in `recoveryConfig` handles this safely.
- **Passive expiry from `InactiveThreshold`**: recovery must work for both explicit deletion and server-side passive expiry. Since pull requests are only active when `Next()` is blocking, **slow handlers inherently risk passive expiry** — if handler processing time exceeds `InactiveThreshold`, the consumer will expire. The detection path is the same as between-pulls deletion: `ErrNoHeartbeat` → `consumer.Info()` → `ErrConsumerNotFound` → recovery. The implementation *must not* rely on an iterator running to keep the consumer alive between handler calls.
- **`ErrNoHeartbeat` is not self-resolving**: after receiving `ErrNoHeartbeat`, the iterator does NOT stop and does NOT transition to `ErrConsumerDeleted` on subsequent calls. If the recovery code only watches for `ErrConsumerDeleted`, it will miss the most common deletion signal entirely.
- **Recovery window can itself cause expiry**: if `recoverConsumer()` takes longer than `InactiveThreshold` (e.g., due to NATS server unavailability or retry backoff), the freshly recreated consumer could expire before a new iterator binds to it. The implementation should seed the checkpoint and start the new iterator promptly after successful recreation.
- **`recoveryConfig` must clear stale policy fields**: when switching `DeliverPolicy` (e.g., from `DeliverByStartSequencePolicy` to `DeliverNewPolicy` for the fallback case), zero out `OptStartSeq` and `OptStartTime` from the base config copy. NATS ignores these fields for non-matching policies, but clearing them prevents confusion in logs, tests, and future server behavior changes.
- **`recoverConsumer` must respect context cancellation**: the recovery method must check the consumer's context before and after the `CreateOrUpdateConsumer` network call, aborting promptly if shutdown has begun.

### 5. Per-Consumer-Type Support Matrix and Recommendations

| Consumer Type      | Supported Strategies in v1                                                               | Recommended Strategy                           | Rationale                                                                                       |
|--------------------|------------------------------------------------------------------------------------------|------------------------------------------------|-------------------------------------------------------------------------------------------------|
| `Queue`            | `RecoveryDisabled`, `RecoverFromNew`, `RecoverFromBeginning`                             | `RecoverFromNew`                               | Shared durable — checkpoint-based resume is unsafe across processes                             |
| `Broadcast`        | `RecoveryDisabled`, `RecoverFromNew`, `RecoverFromLastProcessed`, `RecoverFromBeginning` | `RecoverFromNew` or `RecoverFromLastProcessed` | One durable per instance; checkpoint tracking is deterministic when `ManualAck=false`           |
| `Static`/`Dynamic` | `RecoveryDisabled`, `RecoverFromNew`, `RecoverFromLastProcessed`, `RecoverFromBeginning` | `RecoverFromLastProcessed`                     | One per-subject durable per worker; checkpoint tracking is deterministic when `ManualAck=false` |

These support boundaries are intentional for the initial implementation. They prioritize correct semantics over exposing every possible strategy everywhere.

## Verification Plan

### Automated Tests

Add integration tests in `test/integration/consumer/`:

#### `queue_auto_recovery_test.go`
- Publish messages → assert Queue consumer processes them.
- Delete consumer on NATS server (`js.DeleteConsumer`).
- Assert iterator error triggers recreation (not a hang).
- With `RecoverFromNew`: assert no replay of already-processed messages.
- With `RecoverFromBeginning`: assert full replay (control test).
- Verify Queue rejects `RecoverFromLastProcessed` at construction time.
- Verify repeated delete-related errors produce one serialized recovery attempt.
- Add a passive-expiry variant: configure `InactiveThreshold=3s`, process a message, then sleep `> InactiveThreshold` without calling `Next()` (simulating a slow handler). Assert that `ErrNoHeartbeat` fires, `consumer.Info()` confirms `ErrConsumerNotFound`, and recovery triggers without backlog replay.

#### `broadcast_auto_recovery_test.go`
- Same pattern as Queue, adapted for broadcast fan-out.
- With `RecoverFromLastProcessed`: assert replay starts from seeded/tracked checkpoint, not from stream beginning.
- Verify `ManualAck=true` rejects `RecoverFromLastProcessed`.
- Add a passive-expiry variant: process a message, then sleep `> InactiveThreshold` without calling `Next()`. Assert `ErrNoHeartbeat` fires, `consumer.Info()` confirms deletion, and recovery proceeds without explicit deletion.

#### `partition_consumer_auto_recovery_test.go`
- Publish messages to a specific partition subject.
- Delete the per-subject consumer.
- Delete consumer with an active `Next()` call blocked: assert `ErrConsumerDeleted` triggers immediate `recoverConsumer` without waiting for burst escalation.
- Delete consumer between `Next()` calls (slow handler): assert `ErrNoHeartbeat` burst followed by `consumer.Info()` → `ErrConsumerNotFound` triggers `recoverConsumer`.
- With `RecoverFromLastProcessed`: assert resume starts from seeded/tracked checkpoint.
- Verify `ErrNoHeartbeat` from transient network issue (where `consumer.Info()` returns success) stays on the existing escalation/backoff path and does NOT trigger recovery.
- Verify `ManualAck=true` rejects `RecoverFromLastProcessed`.
- Add a passive-expiry variant: process a message, then sleep `> InactiveThreshold` without calling `Next()`. Assert recovery triggers with same outcome as explicit deletion.

#### Unit Tests
- `recoveryConfig()`: verify correct `DeliverPolicy`, `OptStartSeq`, fallback-to-new behavior when no checkpoint exists, and that stale `OptStartSeq`/`OptStartTime` from the base config are zeroed.
- `shouldRecoverConsumerImmediate()`: returns true only for `ErrConsumerDeleted`; returns false for `ErrNoHeartbeat`, `ErrMsgIteratorClosed`, `context.Canceled`.
- `shouldRecoverAfterInfo()`: returns true when `consumer.Info()` returns `ErrConsumerNotFound` after `ErrNoHeartbeat` burst.
- `IsConsumerGone()`: matches only `ErrConsumerDeleted` (the 409 fast path). Does NOT match `ErrNoHeartbeat`.
- `recoveryCheckpoint`: seed from consumer info and advance monotonically on successful helper-owned ack.
- Recovery serialization: repeated delete signals cause only one in-flight recovery.

### Test Notes for `InactiveThreshold`

All points below are empirically verified by `TestVerify_InactiveThresholdSemantics` in `test/integration/consumer/inactive_threshold_verify_test.go`.

- **Blocking the handler DOES cause expiry.** The iterator only has an active pull on the server while `Next()` is blocked. When `Next()` returns and the handler is processing, there is no active pull. If handler processing time exceeds `InactiveThreshold`, the consumer expires.
- **A never-drained iterator also causes expiry.** Calling `Messages()` but never calling `Next()` — even with a running background goroutine — expires the consumer within `InactiveThreshold`.
- **A long `FetchTimeout` or `PullExpiry` does NOT prevent expiry.** Only an actively blocked `Next()` keeps the server from treating the consumer as idle.
- **`ErrNoHeartbeat` repeats indefinitely.** After the consumer expires or is deleted between `Next()` calls, both the original and any new iterator on the stale handle return `ErrNoHeartbeat` on every `Next()` call, forever. The iterator does NOT self-stop and does NOT surface `ErrConsumerDeleted`.
- **`consumer.Info()` is the reliable confirmation.** After `ErrNoHeartbeat`, calling `consumer.Info()` returns `ErrConsumerNotFound` immediately if the consumer is gone.
- To trigger passive expiry in tests: call `Next()` to receive a message, then sleep for `> InactiveThreshold` without calling `Next()` again. No need to call `iter.Stop()` — just hold the handler.
- `PullExpiry` minimum enforced by nats-go is **1 second**. Set `InactiveThreshold` to at least 2–3× `PullExpiry` (e.g., `PullExpiry=1s`, `InactiveThreshold=3s`) for reliable test timing. Sleep for `2×InactiveThreshold` to ensure deterministic expiry.
- Passive expiry and explicit deletion differ ONLY in how the first signal is detected: active-pull deletion → `ErrConsumerDeleted` (immediate); between-pulls or slow-handler expiry → `ErrNoHeartbeat` (2× heartbeat interval). Both resolve to the same recovery path once `consumer.Info()` confirms `ErrConsumerNotFound`.
