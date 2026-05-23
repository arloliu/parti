# P2.3 (F5) — Stream-gone hook + checkpoint reset + epoch fence

Per-PR spec for the ninth PR specced (file-numbering convention is
sequential by spec-written order; P2.2 and P2.4b/c/d were not
specced). In the actual merge sequence this is the 13th PR.
P2.4d (the F2 envelope on `partition_consumer.go`) merged at
`2c359b0`; P2.3 layers the stream-missing escalation on top.

The high-level design lives in `00-fix-plan.md` § P2.3 (lines
905-1191); this per-PR spec is the precise implementation contract —
the file-by-file plan, the test ordering, and the design decisions
that the architectural review resolved.

**Spec version: v10.** Plan-review loop:
- v1: 11 findings (6 P0, 5 P1)
- v2: fixed all v1; introduced 9 new (3 P0, 4 P1, 2 P2)
- v3: fixed all v2; introduced 6 new (2 P0, 4 P1)
- v4: fixed all v3; introduced 5 new (1 P0, 4 P1)
- v5: fixed all v4; introduced 4 new (0 P0, 4 P1)
- v6: fixed all v5; introduced 2 new (0 P0, 2 P1)
- v7: fixed all v6; introduced 1 new P1 + 1 P2
- v8: fixed all v7; introduced 1 new P1 + 1 P2
- v9: fixed all v8; introduced 1 new P1
- v10 (this version): addresses all v9 findings
  (`tmp/09-pr9-spec_plan_review_v9.md`).

The revision log at the end of this file enumerates the v10
changes and finding-to-fix mapping.

## Background

Today, when a `partition_consumer`'s dynamic consumer cannot create
itself because the underlying JetStream stream is absent:

1. `pc.ensureConsumer` returns `jetstream.ErrStreamNotFound` from
   `internal/durable/partition_consumer.go:533`.
2. The iterator-creation envelope (P2.4d, wired at `partition_consumer.go:189-195`)
   eventually exhausts its budget and exits the consumption loop —
   so the worker no longer hammers NATS with create requests.

That is correct as far as it goes, but it gives the operator no
hook to recreate the stream and no path to resume consumption
afterward. Three follow-on hazards remain:

1. **No escalation seam.** A provisioning-owning caller (typically
   `parti.Provision`) has nothing to subscribe to that signals "the
   library's recovery loop has given up trying to find your stream;
   please recreate it." Without that seam, the only recovery is a
   pod rotation.
2. **Monotonic checkpoint trap.** Even if an operator recreates the
   stream out-of-band, the existing `Checkpoint.Seed`
   (`internal/recovery/checkpoint.go:33-40`) is monotonic — a stale
   checkpoint of 100 from the dead stream cannot be lowered. The
   first post-recreate consumer build falls through
   `BuildConfig` (`internal/recovery/config.go:22-28`) to
   `DeliverByStartSequencePolicy` with `OptStartSeq=101`, silently
   skipping every message in the fresh stream until sequence 101
   exists.
3. **Late-ack epoch race.** Manual-ack mode hands a `trackingMsg` to
   the handler and returns
   (`internal/recovery/controller.go:185-188`,
   `internal/recovery/tracking_msg.go:19-37`). A late ack from an
   OLD-stream handler — arriving AFTER any "reset checkpoint"
   logic — would call `AdvanceCheckpoint` and re-raise the
   checkpoint to an old-stream sequence, silently defeating the
   reset. Today no reset exists; once one is introduced, this race
   becomes the dominant failure mode for manual-ack workloads.
4. **Compat re-arm gap.** `consumer.Dynamic`'s `Update` runs
   `CheckWorkQueueRecoveryCompat` under `sync.Once`
   (`consumer/dynamic.go:43-44, 310-315`). `UpdateWorkerConsumer`
   (the manager-facing path, lines 325-327) **bypasses the check
   entirely** — going straight to `d.inner.UpdateWorkerConsumer`.
   If a recreated stream has a different retention policy
   (e.g. `LimitsPolicy → WorkQueuePolicy`), the manager-driven
   path runs with an incompatible recovery strategy and there is
   no warning.

## Design — locked

This PR adds three coordinated mechanisms in one merge unit, plus
the compat re-arm fix. Streams remain category B (the library does
**not** auto-recreate). The hook is the escalation path; the
consumer-side machinery hardens against a stream that *has* been
recreated.

### Mechanism 1 — `StreamMissingHook` public type

```go
// types/stream_hooks.go (placement resolved in v3 — see exported-symbol audit)
//
// StreamMissingHook fires when a dynamic consumer cannot create a
// consumer because the underlying JetStream stream is absent.
//
// Contract:
//   - The hook is the escalation path; the library does not
//     recreate streams (category B).
//   - Returning a nil error indicates the caller has re-created the
//     stream (e.g. via parti.Provision). The library's recovery
//     loop will then re-enter and rebuild the consumer.
//   - Returning a non-nil error or omitting the hook entirely
//     surfaces the loss via the F2 envelope's exhaustion path
//     (P2.4d's OnPermanentFailure) → manager Hooks.OnError +
//     enterDegraded — so the readiness probe can rotate the pod.
//
// Consumer-state restore is OPTIONAL but is bound to two rules:
//
//   - SAME-DURABLE-NAME. If the caller preserves the durable
//     consumer WITH THE SAME NAME Parti was using (the per-subject
//     durable name built by internal/dynamicbuild), and with
//     non-zero AckFloor, Controller.HandleStreamRecreated's
//     SeedCheckpoint picks up the AckFloor via the existing
//     consumer handle. The next BuildConfig produces
//     DeliverByStartSequencePolicy(AckFloor+1).
//
//   - COMPATIBLE CONFIG. If the caller preserves a same-named
//     consumer but with INCOMPATIBLE config (e.g. different
//     DeliverPolicy from Parti's strategy-derived choice, different
//     AckPolicy, different InactiveThreshold), the post-recreate
//     RebuildAfterStreamRecreated call invokes
//     js.CreateOrUpdateConsumer with the Parti-derived config; NATS
//     responds with a consumer-config-mismatch error (NATS error
//     10094 family). v5 — this surfaces as a wrapped ErrStreamMissing
//     (NOT a separate "non-stream-missing" failure as v3 documented;
//     v4 unified the umbrella). The F2 envelope retries until
//     exhaustion, after which OnPermanentFailure fires the manager
//     observer with errors.Is(err, ErrStreamMissing) == true and
//     enterDegraded("stream-missing-recovery-exhausted") trips
//     readiness. The operator's responsibility is to either reconcile
//     the restored consumer config to match Parti's expectations
//     OR delete the consumer so Parti can recreate it fresh on the
//     restored stream (which falls through to the replay path).
//
//   - If the caller recreates the stream with NO consumer (or a
//     consumer with a DIFFERENT durable name), Parti treats it as
//     "no preserved consumer state". The checkpoint stays at 0
//     and the next BuildConfig produces DeliverAllPolicy (replay
//     from sequence 1). The library will create the configured
//     durable on the fresh stream; the differently-named consumer
//     is ignored. Operators restoring from a backup MUST preserve
//     the durable consumer name (and a compatible config) or
//     accept the from-zero replay semantics.
//
// The hook MUST be safe to call from a recovery goroutine and SHOULD
// return promptly. A long-running hook delays the consumer rebuild
// and keeps the F2 envelope's attempt budget ticking.
//
// REQUIRED RecoveryStrategy: configuring StreamMissingHook requires
// WithRecoveryStrategy(RecoverFromLastProcessed) — the common case
// for at-least-once semantics — or WithRecoveryStrategy(RecoverFromBeginning)
// — replay-all, intentional duplicate processing. RecoveryDisabled
// (the default) and RecoverFromNew are both rejected at construction
// because they would either disable the recovery controller entirely
// or silently skip messages published after a fresh-stream recreate
// (RecoverFromNew uses DeliverNewPolicy, which does not pick up the
// recreated-stream replay override). See the consumer-construction
// validation for the exact rejection messages.
type StreamMissingHook func(streamName string) error
```

Wire into:
- `durable.WorkerConsumerConfig.StreamMissingHook`
- `consumer.DynamicConfig` (+ `WithStreamMissingHook` option)

The plumbing target inside `partition_consumer.go` is
`pc.config.StreamMissingHook`; `partitionConsumerConfig` grows the
field.

**Validation — required recovery strategy (v7 — v5-P1.1 + v6-P1.2 fix):**
The library's stream-missing recovery flow relies on
`recovery.Controller` for `HandleStreamRecreated` and
`RebuildAfterStreamRecreated`. The current
`recovery.NewController` returns nil when strategy is
`RecoveryDisabled` (the default). A nil controller cannot perform
either operation.

Additionally, the recreated-stream `BuildConfig` override (which
produces `DeliverAllPolicy` when `recreatedSinceLastBuild=true` AND
checkpoint==0) only lives in the `FromLastProcessed` branch.
`RecoverFromNew` would skip the override and use `DeliverNewPolicy`,
which means messages published after a fresh-stream recreate but
before the consumer is bound would be silently skipped — the exact
hazard P2.3 is designed to eliminate.

v9 validates a NARROWED set of allowed strategies at THREE
public surfaces. Each layer implements its OWN package-local
validation function (v9 — v8-P1 fix: no shared helper, because
`internal/durable/` cannot import `consumer/`; duplicating ~15
lines avoids cross-package coupling that would require a new
shared package or an interface dance):

```go
// consumer/dynamic.go (package-local helper):
func validateStreamMissingHookStrategy(hookConfigured bool, strategy RecoveryStrategy) error {
    if !hookConfigured {
        return nil
    }
    switch strategy {
    case RecoverFromLastProcessed, RecoverFromBeginning:
        return nil // OK — both implement the recreated-stream replay correctly.
    case RecoveryDisabled:
        return fmt.Errorf(
            "consumer: StreamMissingHook requires a non-disabled RecoveryStrategy; " +
            "use WithRecoveryStrategy(RecoverFromLastProcessed) or " +
            "WithRecoveryStrategy(RecoverFromBeginning) to enable the " +
            "stream-missing recovery path")
    case RecoverFromNew:
        return fmt.Errorf(
            "consumer: StreamMissingHook is incompatible with RecoverFromNew " +
            "because the recreated-stream replay override only applies to " +
            "FromLastProcessed and FromBeginning. RecoverFromNew would silently " +
            "skip messages published after a fresh-stream recreate. " +
            "Use WithRecoveryStrategy(RecoverFromLastProcessed) for at-least-once " +
            "semantics, or WithRecoveryStrategy(RecoverFromBeginning) for replay-from-zero.")
    default:
        return fmt.Errorf(
            "consumer: StreamMissingHook with unknown RecoveryStrategy %v", strategy)
    }
}

// internal/durable/config.go (package-local helper — IDENTICAL logic,
// error messages use "durable:" prefix instead of "consumer:"):
func validateStreamMissingHookStrategy(hookConfigured bool, strategy RecoveryStrategy) error {
    if !hookConfigured {
        return nil
    }
    switch strategy {
    case RecoverFromLastProcessed, RecoverFromBeginning:
        return nil
    case RecoveryDisabled:
        return fmt.Errorf(
            "durable: StreamMissingHook requires a non-disabled RecoveryStrategy; " +
            "use RecoveryStrategy: RecoverFromLastProcessed or RecoverFromBeginning")
    case RecoverFromNew:
        return fmt.Errorf(
            "durable: StreamMissingHook is incompatible with RecoverFromNew " +
            "because the recreated-stream replay override only applies to " +
            "FromLastProcessed and FromBeginning. RecoverFromNew would silently " +
            "skip messages published after a fresh-stream recreate. " +
            "Set RecoveryStrategy: RecoverFromLastProcessed or RecoverFromBeginning.")
    default:
        return fmt.Errorf(
            "durable: StreamMissingHook with unknown RecoveryStrategy %v", strategy)
    }
}
```

Called from THREE entry points (v9 — v8-P1 fix):

1. **`consumer.NewDynamic`** option-validation step (after applying
   options, before constructing the inner `durable.WorkerConsumer`).
   Calls the `consumer/` package-local helper.
2. **`consumer.DynamicConfig.Validate`** method (public, separately
   callable — applications using `cfg.Validate()` directly must
   see the rejection too). Calls the same helper.
3. **`durable.WorkerConsumerConfig.Validate`** method (public,
   separately callable — for callers that bypass the
   `consumer.Dynamic` wrapper and use the durable package directly).
   Calls the `durable/` package-local helper.

**Why duplicated, not shared:** moving the helper to a shared
package (`types/`, `internal/recovery/`, or a new
`internal/streamhook/`) would either require those packages to
import `RecoveryStrategy` (which already lives in `internal/recovery`)
plus the typed hook signature, OR add an interface dance. The
~15-line duplication is contained — both helpers reject the same
strategies and accept the same strategies; if they ever diverge, a
test would catch it (see v9 cross-package consistency test below).

**v10 cross-package consistency test (v9-P1 fix)** —
`consumer/streamhook_validation_consistency_test.go` (in
`package consumer_test`):

The v9 spec proposed a helper-level cross-package test, but Go's
package-privacy rules make it impossible to call two
unexported package-local helpers from one test file. v10 rewrites
the requirement to compare **public Validate surfaces** —
`consumer.DynamicConfig.Validate()` and
`durable.WorkerConsumerConfig.Validate()` — which are both
importable from a `consumer_test` package. The test runs a
table-driven matrix of `(strategy, hookConfigured)` inputs and
asserts both surfaces produce equivalent accept/reject outcomes
(error vs nil — the exact error text differs by prefix, that's
fine).

```go
// consumer/streamhook_validation_consistency_test.go (sketch)
package consumer_test

import (
    "testing"
    "github.com/arloliu/parti/v2/consumer"
    "github.com/arloliu/parti/v2/internal/durable"
)

func TestStreamMissingHookStrategy_ConsistentAcrossPublicSurfaces(t *testing.T) {
    cases := []struct{
        name        string
        strategy    consumer.RecoveryStrategy // alias for durable.RecoveryStrategy
        expectError bool
    }{
        {"disabled", consumer.RecoveryDisabled, true},
        {"from-new", consumer.RecoverFromNew, true},
        {"from-last-processed", consumer.RecoverFromLastProcessed, false},
        {"from-beginning", consumer.RecoverFromBeginning, false},
    }
    hook := func(string) error { return nil }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            // DynamicConfig surface
            dc := consumer.DynamicConfig{
                /* ... required fields ... */
                StreamMissingHook: hook,
                RecoveryStrategy:  tc.strategy,
            }
            dErr := dc.Validate()
            // WorkerConsumerConfig surface
            wc := durable.WorkerConsumerConfig{
                /* ... required fields ... */
                StreamMissingHook: hook,
                RecoveryStrategy:  tc.strategy,
            }
            wErr := wc.Validate()
            // Equivalent outcomes (error vs nil); exact text differs.
            if (dErr == nil) != (wErr == nil) {
                t.Fatalf("inconsistent: DynamicConfig.Validate=%v, WorkerConsumerConfig.Validate=%v", dErr, wErr)
            }
            if tc.expectError && dErr == nil {
                t.Fatalf("expected error for strategy %s, got nil from DynamicConfig.Validate", tc.name)
            }
        })
    }
}
```

This pins the cross-package consistency without violating Go's
privacy rules. If a future change adds a new `RecoveryStrategy`
enum value to one surface's validation without updating the
other, the test fails because one returns nil while the other
returns an error.

The hook godoc states the allowed strategies explicitly: callers
configuring `StreamMissingHook` must use `RecoverFromLastProcessed`
(common case, at-least-once) or `RecoverFromBeginning` (replay-all,
intentional duplicate processing).

**Required regression tests (v9 — v7-P1 + v8-P2 fix; full
matrix across all three surfaces × all four strategy variants):**

- `consumer/dynamic_test.go` (`NewDynamic` surface):
  - `TestNewDynamic_StreamMissingHook_RecoverFromNew_Rejected`
  - `TestNewDynamic_StreamMissingHook_RecoveryDisabled_Rejected`
  - `TestNewDynamic_StreamMissingHook_RecoverFromLastProcessed_OK`
  - `TestNewDynamic_StreamMissingHook_RecoverFromBeginning_OK`
- `consumer/dynamic_test.go` (`DynamicConfig.Validate` surface):
  - `TestDynamicConfig_Validate_StreamMissingHook_RecoverFromNew_Rejected`
  - `TestDynamicConfig_Validate_StreamMissingHook_RecoveryDisabled_Rejected`
  - `TestDynamicConfig_Validate_StreamMissingHook_RecoverFromLastProcessed_OK`
  - `TestDynamicConfig_Validate_StreamMissingHook_RecoverFromBeginning_OK`
- `internal/durable/config_test.go` (`WorkerConsumerConfig.Validate` surface):
  - `TestWorkerConsumerConfig_Validate_StreamMissingHook_RecoverFromNew_Rejected`
  - `TestWorkerConsumerConfig_Validate_StreamMissingHook_RecoveryDisabled_Rejected`
  - `TestWorkerConsumerConfig_Validate_StreamMissingHook_RecoverFromLastProcessed_OK`
  - `TestWorkerConsumerConfig_Validate_StreamMissingHook_RecoverFromBeginning_OK`
- Plus the cross-package consistency test (above):
  - `TestStreamMissingHookStrategy_ConsistentAcrossPublicSurfaces`
    (in `consumer/streamhook_validation_consistency_test.go`,
    package `consumer_test` — compares the public
    `DynamicConfig.Validate` and `WorkerConsumerConfig.Validate`
    surfaces).

Total: 12 strategy-validation tests + 1 consistency test = 13.
Each test asserts the specific error type or message-substring so
implementations cannot silently weaken the rejection.

### Mechanism 1b — Public no-hook readiness route (NEW in v2)

The v1 spec assumed "envelope exhaustion → OnPermanentFailure →
existing degraded-mode wiring trips readiness". The v1 plan-review
found that NO such wiring exists today: `consumer.Dynamic` does not
set `WorkerConsumerConfig.OnPermanentFailure`, and `Manager` has no
way to inject callbacks into an externally-created `*Dynamic`. T4
cannot pass as written.

v2 added the surface; v3 fixes the construction-order bug the v2
plan-review identified (v2-P0.1). The bug: `NewDynamic` constructs
`WorkerConsumerConfig` (copying the user-supplied
`OnPermanentFailure` into the durable layer) BEFORE `parti.Manager`
has a chance to call `SetOnStreamMissingError`. A later swap can't
affect the already-copied callback.

v3 fixes this with an **indirection slot owned by `*Dynamic`** that
the durable callback ALWAYS reads at fire time:

```go
// consumer/dynamic.go (v3 fields)
type Dynamic struct {
    // ... existing fields ...
    // userOnPermanentFailure is the application-supplied
    // OnPermanentFailure callback (from WithOnPermanentFailure).
    // Nil if not configured.
    userOnPermanentFailure func(subject string, err error)
    // managerOnStreamMissing is the manager-installed closure
    // registered via SetOnStreamMissingError. Read at fire time
    // through the atomic.Pointer so a Manager.Start call after
    // NewDynamic correctly reaches the durable layer.
    managerOnStreamMissing atomic.Pointer[func(streamName string, err error)]
}

// NewDynamic always installs THIS closure as WorkerConsumerConfig.OnPermanentFailure
// (regardless of whether the user supplied OnPermanentFailure). The
// closure dispatches at fire time:
func (d *Dynamic) onPermanentFailure(subject string, err error) {
    // Application-supplied callback wins if present.
    if d.userOnPermanentFailure != nil {
        d.userOnPermanentFailure(subject, err)
        return
    }
    // Otherwise route stream-missing errors through the
    // manager-installed observer if any. Generic exhaustion
    // (non-stream-missing) flows through unchanged — the
    // partition_consumer's OnPermanent log line already records it
    // at WARN (P2.4d). No new typed-error wrapper is introduced
    // for the generic case (v3-P1.3 resolution: types.ErrConsumerRecoveryExhausted
    // dropped from v2; the existing log + metric path is the
    // operator-visible signal for generic exhaustion).
    if fn := d.managerOnStreamMissing.Load(); fn != nil {
        if errors.Is(err, types.ErrStreamMissing) {
            (*fn)(d.streamName, err)
        }
    }
}
```

`SetOnStreamMissingError` is the public method on `*Dynamic`:

```go
// SetOnStreamMissingError stores a callback invoked when a stream-
// missing-classified permanent failure surfaces from the durable
// layer. Called by Manager.Start (or any wiring layer) AFTER
// NewDynamic. Safe for concurrent use.
func (d *Dynamic) SetOnStreamMissingError(fn func(streamName string, err error)) {
    if fn == nil {
        d.managerOnStreamMissing.Store(nil)
        return
    }
    d.managerOnStreamMissing.Store(&fn)
}
```

The `recovery.StreamMissingObserver` interface (one method:
`SetOnStreamMissingError`) is implemented by `*Dynamic`.
`Manager.Start` (specifically: at the end of `prepareStart` in
`manager_setup.go:18-25`, after `m.ctx` is initialized but before
any worker-consumer interaction) type-asserts the registered
updater and installs the observer:

```go
// manager_setup.go (v5 — concrete addition; the actual field name
// is m.consumerUpdater, not m.workerUpdater — v5 fix for v4-P1.3).
//
// CompositeConsumerUpdater forwarding: when the registered updater
// is a CompositeConsumerUpdater wrapping one or more child updaters,
// we must forward the SetOnStreamMissingError call to each child
// that implements the StreamMissingObserver interface. The composite
// itself does NOT implement the interface directly; instead, v5
// adds a small forwarding method on CompositeConsumerUpdater (see
// composite_updater.go edit below).
if obs, ok := m.consumerUpdater.(recovery.StreamMissingObserver); ok {
    obs.SetOnStreamMissingError(m.onStreamMissingError)
}

// onStreamMissingError is the manager's observer closure. Defined
// as a method so it closes over m and has access to m.ctx,
// m.logError, m.enterDegraded.
func (m *Manager) onStreamMissingError(streamName string, cause error) {
    // m.logError preserves the existing manager.wg + Hooks.OnError
    // tracking (see manager.go:918-947); using m.logError instead
    // of direct hooks.OnError invocation also gives consistent log
    // formatting and uses m.ctx implicitly.
    m.logError(
        fmt.Sprintf("dynamic-consumer stream %q recovery exhausted", streamName),
        "stream", streamName,
        "error", cause,
    )
    // Trip readiness. The reason string is the operator-facing
    // signal that distinguishes stream-missing from KV-bucket-loss
    // (see "Cross-feature contract preservation" below).
    m.enterDegraded("stream-missing-recovery-exhausted")
}
```

**`composite_updater.go` edit (v6 — v4-P1.3 + v5-P1.2 fixes):**

The composite must:
1. Forward `SetOnStreamMissingError` to all CURRENT children that
   implement `recovery.StreamMissingObserver`.
2. Remember the registered observer so a LATER `Add()` call also
   forwards to the newly-added child (v5-P1.2: the manager
   registers the observer once during prepareStart; `Add()` is
   public and can be called afterward — without this fix, late-
   added children would miss the observer).
3. Be race-free between concurrent `Add` and observer-related
   reads.

```go
// composite_updater.go (v6 additions)
type CompositeConsumerUpdater struct {
    updaters     []WorkerConsumerUpdater // existing field (NOT "children")
    mu           sync.Mutex              // new: protects updaters slice + currentObserver
    currentObserver func(streamName string, err error) // new: last registered observer
}

// SetOnStreamMissingError records the callback and forwards it
// to all current updaters that implement
// recovery.StreamMissingObserver. Children that do not implement
// the interface are skipped silently. Subsequent Add() calls
// forward the recorded observer to newly-added children.
//
// Calling SetOnStreamMissingError(nil) clears both the stored
// observer and any installed observer on current children.
func (c *CompositeConsumerUpdater) SetOnStreamMissingError(fn func(streamName string, err error)) {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.currentObserver = fn
    for _, u := range c.updaters {
        if obs, ok := u.(recovery.StreamMissingObserver); ok {
            obs.SetOnStreamMissingError(fn)
        }
    }
}

// Add (existing public method) — v6 wraps it so newly added
// observer-capable children inherit the currently-registered
// stream-missing observer.
func (c *CompositeConsumerUpdater) Add(updaters ...WorkerConsumerUpdater) {
    c.mu.Lock()
    defer c.mu.Unlock()
    for _, u := range updaters {
        if u == nil {
            continue
        }
        c.updaters = append(c.updaters, u)
        if c.currentObserver != nil {
            if obs, ok := u.(recovery.StreamMissingObserver); ok {
                obs.SetOnStreamMissingError(c.currentObserver)
            }
        }
    }
}

// UpdateWorkerConsumer, Capabilities, Len — v6 also takes the mutex
// around their reads of c.updaters (existing code did unsynchronized
// reads; v5-P1.2 noted the race surface). This is a minor adjustment
// to the existing methods.
```

`CompositeConsumerUpdater` itself satisfies
`recovery.StreamMissingObserver` (because it has the required
method); the manager's type-assertion succeeds and the composite
forwards to its children. Tests in `composite_updater_test.go`
pin:
- Children present at construction receive the observer when
  `SetOnStreamMissingError` is called.
- Children added LATER via `Add()` after observer registration
  also receive the observer (v5-P1.2 regression pin).
- `SetOnStreamMissingError(nil)` clears the stored observer and
  the children's observers.
- Non-observer-implementing children are silently skipped.

The closure uses `m.ctx` implicitly through `m.logError`; that
function already runs the registered `Hooks.OnError` under the
manager's wait group, so the bridge gets the full lifecycle
semantics without re-implementing them.

**Construction order, explicitly:**
1. Application calls `consumer.NewDynamic(...)`. The Dynamic
   constructs `WorkerConsumerConfig` with `OnPermanentFailure =
   d.onPermanentFailure` (the indirection closure — ALWAYS, regardless
   of user-supplied option).
2. `durable.NewWorkerConsumer` copies this closure into its config.
3. Application calls `parti.NewManager(..., WithWorkerConsumerUpdater(dynamic))`.
4. `Manager.Start` (or the constructor) type-asserts and calls
   `dynamic.SetOnStreamMissingError(managerClosure)`. The closure is
   stored in `dynamic.managerOnStreamMissing.atomic.Pointer`.
5. When permanent failure fires later, the durable callback (=
   `d.onPermanentFailure`) reads `d.managerOnStreamMissing` AT
   THAT MOMENT and dispatches.

The bug the v2 plan-review found is closed: no copy of a
nil-at-the-time callback ever reaches the durable layer.

**Cross-feature contract preservation:** `types.ErrStreamMissing` is
the unambiguous carrier. It is distinct from
`natsutil.IsDegradingJetStreamError` (which treats raw
`jetstream.ErrStreamNotFound` as degrading and would otherwise
route through `recordKVError`). T4 asserts
`errors.Is(receivedErr, types.ErrStreamMissing)`.

**v4 — explicit `recordKVError` short-circuit (v3-P1.3 fix):**
the v3 spec claimed `recordKVError` short-circuits on
`types.ErrStreamMissing`, but the v3 plan-review found that no
such short-circuit exists in source. v4 adds the explicit edit to
the file plan:

```go
// manager_degraded.go (v4 — add at the top of recordKVError, before
// any other classification):
func (m *Manager) recordKVError(err error) {
    if err == nil {
        return
    }
    // Stream-missing errors are routed through Dynamic's permanent-
    // failure observer to enterDegraded("stream-missing-recovery-exhausted"),
    // NOT through the generic KV error threshold path. Short-circuit
    // here so that a stream-missing error which incidentally wraps
    // jetstream.ErrStreamNotFound (a degrading-JetStream error per
    // natsutil) does not double-count or trip the KV threshold.
    if errors.Is(err, types.ErrStreamMissing) {
        return
    }
    // ... existing classification (connectivity / degrading JS) ...
}
```

T4 asserts the route is `stream-missing-recovery-exhausted`, NOT
`"KV error threshold exceeded"`. This preserves the cross-feature
contract that whole-bucket loss (Parti's own KV buckets) is the
only path through `recordKVError → enterDegraded("KV error threshold exceeded")`.

### Mechanism 2 — Checkpoint reset (`internal/recovery/checkpoint.go`)

```go
// ResetForStreamRecreate clears the checkpoint to zero. Unlike Seed,
// this is NON-monotonic. It is only legal after the caller has
// confirmed (via the OnStreamMissing hook returning nil) that the
// stream is a new identity. The Controller.HandleStreamRecreated
// path is the only legitimate caller; direct use elsewhere will
// silently drop progress.
func (cp *Checkpoint) ResetForStreamRecreate() {
    cp.maxAckedStreamSeq.Store(0)
}
```

No other Checkpoint method changes.

### Mechanism 3 — Stream-epoch generation fence (`internal/recovery/controller.go`)

```go
// Controller (added field, alongside checkpoint)
streamEpoch atomic.Uint64
```

```go
// internal/recovery/tracking_msg.go
type trackingMsg struct {
    jetstream.Msg
    controller *Controller
    epoch      uint64 // captured from controller.streamEpoch at WrapForTracking time
}

func (m *trackingMsg) Ack() error {
    err := m.Msg.Ack()
    if err == nil {
        m.controller.AdvanceCheckpoint(m.Msg, m.epoch)
    }
    return err
}

// (DoubleAck analogous)
```

```go
// internal/recovery/controller.go
func (c *Controller) WrapForTracking(msg jetstream.Msg) jetstream.Msg {
    if c == nil || c.strategy != FromLastProcessed {
        return msg
    }
    return &trackingMsg{
        Msg:        msg,
        controller: c,
        epoch:      c.streamEpoch.Load(), // captured at dispatch time
    }
}

// Signature widened: epoch parameter is the message's dispatch-time
// epoch. AdvanceCheckpoint silently no-ops when the captured epoch
// does not match the current streamEpoch.
func (c *Controller) AdvanceCheckpoint(msg jetstream.Msg, epoch uint64) {
    if c == nil {
        return
    }
    if epoch != c.streamEpoch.Load() {
        return // late ack from a prior stream generation
    }
    c.checkpoint.Advance(msg)
}
```

The non-manual-ack path (`Dispatch` with `manualAck=false`,
`controller.go:190-194`) is implicitly fenced (synchronous Ack on
the dispatch goroutine, so it cannot outlive a `HandleStreamRecreated`
that ran on the recovery goroutine), but the fence still applies:
`Dispatch` captures the current epoch at dispatch time and passes
it to `AdvanceCheckpoint`. Document the invariant:
**`AdvanceCheckpoint` MUST be called with the epoch captured at
message dispatch time, never at ack time.** This invariant survives
any future async-dispatch addition.

### `HandleStreamRecreated` — the load-bearing ordering

```go
// HandleStreamRecreated is called by the partition consumer's
// recovery detour after OnStreamMissing returns nil. It performs
// the steps in this exact order; reversing bump↔reset opens a
// race where an old-epoch ack landing between reset and bump can
// re-raise the (just-zeroed) checkpoint past zero, after which the
// fresh-stream consumer skips messages.
//
//  1. Bump streamEpoch. Any in-flight trackingMsg captured before
//     this point is now from a prior epoch; its late ack will
//     no-op via the fence.
//  2. ResetForStreamRecreate. Drop the stale checkpoint to zero.
//  3. SeedCheckpoint(ctx, infoFn). If the new stream / restored
//     consumer's AckFloor > 0, the seed picks it up; if AckFloor
//     is 0 (fresh stream), the checkpoint stays at 0.
//  4. Set recreatedSinceLastBuild so the next BuildConfig knows
//     to choose DeliverAllPolicy when checkpoint is still 0.
//
// (v5: there was a step 5 "set bypass token" in v2/v3 designs;
// v4 removed the token entirely because Site A/B both call
// RebuildAfterStreamRecreated synchronously after the hook —
// see "Cooldown handling" below.)
//
// onStep is a private test seam. When non-nil, it is invoked
// between each ordered step with the step name. Tests use it to
// inject an old-epoch ack between steps and verify the ordering
// contract deterministically (see T2c-ordering below). Production
// callers always pass nil.
func (c *Controller) HandleStreamRecreated(ctx context.Context, infoFn InfoFunc) {
    c.handleStreamRecreatedWithSteps(ctx, infoFn, nil)
}

func (c *Controller) handleStreamRecreatedWithSteps(
    ctx context.Context,
    infoFn InfoFunc,
    onStep func(step string),
) {
    if c == nil {
        return
    }
    c.streamEpoch.Add(1)
    if onStep != nil { onStep("after_bump") }

    c.checkpoint.ResetForStreamRecreate()
    if onStep != nil { onStep("after_reset") }

    c.SeedCheckpoint(ctx, infoFn)
    if onStep != nil { onStep("after_seed") }

    c.recreatedSinceLastBuild.Store(true)
    if onStep != nil { onStep("after_flag") }
}
```

The exported `HandleStreamRecreated` does NOT expose `onStep`. The
private `handleStreamRecreatedWithSteps` is package-internal and
the test seam used by the ordering test.

**T2c-ordering deterministic test** (added in v2 to replace the
v1 fragile sleep/log-based test): the test calls
`handleStreamRecreatedWithSteps` with an `onStep` callback. The
callback, when invoked with `"after_reset"`, runs:

```go
// Inject an old-epoch ack between reset and bump (this is the
// race window for reset-before-bump). In the correct ordering
// (bump-before-reset), this inject point is "after_reset" and
// the epoch has ALREADY been bumped, so the held tracking msg
// (captured at epoch=1) acks against current epoch=2 and the
// fence drops it. In the broken ordering (reset-before-bump),
// the epoch is still 1 at "after_reset" and the ack passes the
// fence, advancing the checkpoint past zero.
heldMsg.Ack()
```

Test asserts: after `handleStreamRecreatedWithSteps` returns,
`Checkpoint.Value() == seedValue` (not the held msg's seq). On a
hypothetical reset-before-bump implementation, the value would
equal the held msg's stream seq. This test is deterministic — no
sleeps, no scheduling assumptions.

### `BuildConfig` override

**v4 locked signature:** `BuildConfig` is widened to accept the
`recreatedSinceLastBuild` flag as a fourth parameter. (v3 left this
"optional"; the v3 plan-review correctly noted this was inconsistent
with `RebuildAfterStreamRecreated`'s 4-argument call.)

```go
// internal/recovery/config.go (v4 signature)
func BuildConfig(
    base jetstream.ConsumerConfig,
    strategy Strategy,
    checkpoint uint64,
    recreatedSinceLastBuild bool,
) (jetstream.ConsumerConfig, string) {
```

Spec-level contract:

> After stream recreate, the first post-hook `BuildConfig` call
> with `recreatedSinceLastBuild=true` and `checkpoint==0` produces
> `DeliverAllPolicy`, not `DeliverNewPolicy`. Subsequent calls (with
> the flag cleared by `RebuildAfterStreamRecreated`'s Swap) use the
> existing rules.

Existing callers of `BuildConfig` (currently `Controller.recover`)
pass `false` for the new parameter; their behavior is unchanged.

```go
// Inside BuildConfig (FromLastProcessed branch):
switch {
case checkpoint > 0:
    cfg.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
    cfg.OptStartSeq = checkpoint + 1
case recreatedSinceLastBuild:
    cfg.DeliverPolicy = jetstream.DeliverAllPolicy
    return cfg, "stream_recreated_replay_from_start"
default:
    cfg.DeliverPolicy = jetstream.DeliverNewPolicy
    return cfg, "fallback_no_checkpoint"
}
```

**FromBeginning branch:** unchanged from the existing implementation.
`RecoverFromBeginning` already produces `DeliverAllPolicy`
unconditionally (`internal/recovery/config.go:30-31`), so the
recreated-flag override is a no-op in that branch — the replay
contract is satisfied by the base behavior. The `recreatedSinceLastBuild`
flag is still consumed (via Swap-and-clear) inside
`RebuildAfterStreamRecreated` regardless of strategy, so the flag's
one-shot semantics hold across strategy choices.

**FromNew branch:** intentionally NOT modified. The validation at
consumer construction rejects `StreamMissingHook` + `RecoverFromNew`
(see "Validation — required recovery strategy" above), so no
production code path can reach the FromNew branch with
`recreatedSinceLastBuild=true`. Tests pin this rejection.

### Metrics — no public-interface change (v2 resolution of P1.4)

The v1 spec proposed three new metric methods
(`IncrementWorkerConsumerStreamRecreateSuccess/Error` and
`IncrementWorkerConsumerStreamRecreate`). Per the v1 plan-review's
P1.4 finding, adding methods to `types.WorkerConsumerMetrics` is a
breaking change for external implementers.

v2 uses the existing
`types.WorkerConsumerMetrics.IncrementWorkerConsumerIteratorRestart(reason string)`
method (already in the public interface) with new reason labels:

- `"stream_missing_no_hook"` — hook absent; stream-missing detected
- `"stream_recreate_error"` — hook returned a non-nil error
- `"stream_recreate_success"` — hook returned nil; HandleStreamRecreated ran

The exported-symbol audit at the end of this spec reflects this:
no new methods on `types.WorkerConsumerMetrics`.

### Stream-missing detection — wire shape

**Public typed error (resolved in v2 per P1.1 finding):**
`types.ErrStreamMissing` lives in `types/errors.go` (the existing
public-errors file). `parti.ErrStreamMissing` is a top-level alias
following the existing convention (`parti.ErrClaimLost`,
`parti.ErrInvalidConfig`, etc).

```go
// types/errors.go (new sentinel)
//
// ErrStreamMissing indicates that the library's stream-missing
// recovery flow encountered a failure that prevented the dynamic
// consumer from resuming. Despite the name, this is an UMBRELLA
// for the entire stream-missing recovery episode — not just
// "stream object absent". Wrapped causes include:
//
//   - The underlying JetStream stream is absent
//     (jetstream.ErrStreamNotFound).
//   - The StreamMissingHook returned a non-nil error.
//   - After a successful hook, RebuildAfterStreamRecreated failed
//     for any reason (stream still missing, restored consumer has
//     incompatible config such as a different DeliverPolicy or
//     AckPolicy, etc).
//
// This unification means an application's
// errors.Is(err, parti.ErrStreamMissing) check in Hooks.OnError
// returns true for every operator-actionable stream-recovery
// failure mode, not just the literal stream-not-found case. The
// wrapped cause carries the precise underlying NATS error and the
// stream name so the operator can diagnose.
//
// Distinguishing whole-bucket-loss from stream-missing:
//
//   - Whole-bucket loss (a NATS KV bucket configured by Parti for
//     its own internal state, e.g. assignment, heartbeat) flows
//     through Manager.recordKVError → enterDegraded("KV error
//     threshold exceeded"). The cross-feature contract (AGENTS.md)
//     pins this path; ErrStreamMissing MUST NOT route there.
//
//   - Stream missing (the JetStream stream the application's
//     dynamic consumer reads from) flows through the F2 envelope's
//     OnPermanentFailure → ErrStreamMissing → Hooks.OnError +
//     enterDegraded("stream-missing-recovery-exhausted").
//
// callers SHOULD use errors.Is(err, parti.ErrStreamMissing) for
// branching; the wrapped cause (the underlying NATS error and the
// stream name) is preserved.
var ErrStreamMissing = errors.New("parti: stream missing")
```

The internal recovery package wraps `jetstream.ErrStreamNotFound`
WITH `types.ErrStreamMissing`. No internal `recovery.ErrStreamMissing`
exists — the v1 spec's internal sentinel is replaced by the public
one.

The wire shape for the stream-missing signal:

**Signature contract (v3 — locked):**
- `Controller.recover` returns `(jetstream.Consumer, error)`. The
  `bool` from the v1-era contract is dropped; the existing two
  success/failure variants become: nil-consumer + nil-error =
  "backoff this attempt"; non-nil-consumer + nil-error = "success,
  use this consumer"; nil-consumer + non-nil-error = "stream
  missing or fatal" (caller inspects with errors.Is).
- `Controller.Classify` returns `(Action, jetstream.Consumer, error)`.
  Error is non-nil iff Action == ActionStreamMissing AND the
  error wraps `types.ErrStreamMissing`.

1. **Inside `recover()`**: `recreate()` is called at
   `internal/recovery/controller.go:294`. If it returns an error
   wrapping `jetstream.ErrStreamNotFound`, `recover()` returns
   `(nil, fmt.Errorf("%w: %w", types.ErrStreamMissing, err))` and
   **does NOT** update `lastRecoveryTime`, `burst`, `checkpoint`,
   or `inProgress`.

```go
// internal/recovery/controller.go (inside recover, after recreate call)
//
// v4 signature contract (locked): recover returns
// (jetstream.Consumer, error). Two-value, no bool. The previous
// bool was redundant with err == nil semantics.
//   - non-nil consumer + nil error = success
//   - nil consumer + nil error    = backoff this attempt (no state advance)
//   - nil consumer + non-nil error = fatal or stream-missing (use errors.Is to inspect)
newCons, err := recreate(ctx, recoverCfg)
if err != nil {
    if natsutil.IsStreamNotFound(err) {
        // Bail without advancing checkpoint, burst, or
        // lastRecoveryTime; surface to the caller (partition
        // consumer detour) so the hook can recreate the stream.
        return nil, fmt.Errorf("%w: %w", types.ErrStreamMissing, err)
    }
    c.logger.Warn("consumer recovery failed", "error", err)
    return nil, nil  // backoff path; lastRecoveryTime stays unset
}
```

**Classify signature change (resolved in v2 per P1.2 finding):**
the current `Classify` signature is
`(Action, jetstream.Consumer)`. v2 widens to
`(Action, jetstream.Consumer, error)`. The error is non-nil iff
the action is `ActionStreamMissing` (and wraps
`types.ErrStreamMissing`).

The new `Action`:
```go
// internal/recovery/action.go
ActionStreamMissing Action = iota_next_value
```

**File-by-file caller contract** (v3 — every caller of
`recovery.Controller.Classify` updated; the v2 spec's "surfaces to
OnError sink" claim was wrong — non-Dynamic callers don't have one
today. v3 explicitly states "log + backoff" for them):

| Caller | File | v3 contract |
|---|---|---|
| Dynamic partition consumer (P2.3 target) | `internal/durable/partition_consumer.go:294` | Handles ActionStreamMissing via the `handleStreamMissing` detour (hook + RebuildAfterStreamRecreated) |
| Queue consumer | `consumer/queue.go:398-412` | Maps ActionStreamMissing → log at WARN with stream name + the wrapped error, then ActionBackoff. Queue does not own stream lifecycle and intentionally does not surface the typed error to any callback (no existing OnError sink). |
| Broadcast consumer | `internal/durable/broadcast_consumer.go:331-350` | Same as Queue: log + backoff. |
| Internal partition consumer | `internal/ipartition/consumer.go:291-310` | Same as Queue: log + backoff. |

Each non-Dynamic caller adds the `ActionStreamMissing` switch case
with an explicit comment that this consumer does not own stream
lifecycle, so the typed error is logged for operator
observability but NOT surfaced to any callback. A new test in each
file pins this mapping (action == ActionBackoff after the case
runs; the log line is emitted). This prevents the new action
silently falling through to the default branch.

`Classify` returns `(ActionStreamMissing, nil, wrappedErr)` where
`wrappedErr` is the stream-missing error wrapping
`types.ErrStreamMissing`. The Dynamic caller's `switch action`
includes a `case ActionStreamMissing` that inspects the error via
`errors.Is(err, types.ErrStreamMissing)`; non-Dynamic callers may
ignore the error or log it.

### `partition_consumer.go` detour — TWO call sites

Stream-not-found originates at TWO call sites in
`partition_consumer.go` today, and the v1 review found that wiring
the hook only into `handleIteratorFailure` (the iter-runtime path)
misses the more common case. Both sites must route through the same
`handleStreamMissing` helper.

**Site A — iterator-creation path inside the F2 envelope.**
`runIteratorEnvelope` calls `pc.iterFactory(cons, batch, expiry)`
at `partition_consumer.go:222-225` (v2.4d code). The actual
stream-missing signal originates one level deeper: when
`iterFactory` returns an error, the envelope's Work calls
`maybeEscalateIteratorFailures` → `ensureConsumer` →
`js.CreateOrUpdateConsumer`. Inside `ensureConsumer`, the
`natsutil.IsStreamNotFound(err)` check at
`partition_consumer.go:533-535` recognizes the condition but
discards the error.

v3 fix (resolves v2-P0.2):

The Site A detour must do TWO things on a successful hook:
1. Invoke `pc.recovery.HandleStreamRecreated` (resets checkpoint,
   bumps epoch, sets recreated flag, consumes bypass token).
2. **Explicitly rebuild the consumer using the recovery package's
   config-aware path**, NOT the static `ensureConsumer` config.
   Otherwise the next `iterFactory` call uses the old consumer
   handle (or `ensureConsumer` uses `pc.consumerConfig` with the
   static `DeliverAllPolicy` from `dynamicbuild` — bypassing the
   reset checkpoint + `BuildConfig` override).

This is mechanised through a new `recovery.Controller` method:

```go
// RebuildAfterStreamRecreated builds a post-recreate consumer
// config from the (now-reset) checkpoint, the one-shot recreated
// flag, and the strategy; calls recreate(ctx, cfg) to create the
// new consumer on the freshly-restored stream; returns the new
// consumer. Mirrors the success path of recover() but:
//   - reads-and-clears recreatedSinceLastBuild (so BuildConfig
//     produces DeliverAllPolicy when checkpoint==0);
//   - updates lastRecoveryTime and burst on success (same as
//     recover);
//   - does NOT observe the cooldown — Site A/B call this directly
//     after the hook returns nil, and this is the single
//     immediate post-hook rebuild attempt. (The cooldown bypass
//     token from earlier v3 design was dropped in v4; see
//     "Cooldown handling" below.)
//
// On error, wraps the underlying cause with types.ErrStreamMissing
// regardless of whether the underlying error is jetstream.ErrStreamNotFound
// or a consumer-config-mismatch / NATS API error. v4 design
// (v3-P0.2 fix): any failure during post-hook recovery is part of
// the stream-missing recovery flow, so the typed-error class
// surfaces consistently up to the manager observer. The operator
// godoc on StreamMissingHook spells this out.
//
// Called by partitionConsumer.handleStreamMissing AFTER
// HandleStreamRecreated has run successfully.
func (c *Controller) RebuildAfterStreamRecreated(
    ctx context.Context,
    baseCfg jetstream.ConsumerConfig,
    recreate RecreateFunc,
) (jetstream.Consumer, error) {
    if c == nil {
        return nil, errors.New("recovery: nil controller")
    }
    checkpoint := c.checkpoint.Value()
    recreated := c.recreatedSinceLastBuild.Swap(false) // read-and-clear
    cfg, fallback := BuildConfig(baseCfg, c.strategy, checkpoint, recreated)
    if fallback != "" {
        c.logger.Info("rebuild post-recreate", "fallback", fallback)
    }
    newCons, err := recreate(ctx, cfg)
    if err != nil {
        // All errors during post-hook recovery wrap types.ErrStreamMissing
        // so the manager observer route is consistent. Includes
        // both still-missing-stream and incompatible-restored-consumer-config.
        return nil, fmt.Errorf("%w: %w", types.ErrStreamMissing, err)
    }
    c.burst.Reset()
    c.mu.Lock()
    c.lastRecoveryTime = time.Now()
    c.mu.Unlock()
    return newCons, nil
}
```

**Cooldown handling (v4 — v3-P1.4 fix):** the v3 design used a
one-shot `postHookBypassPending` atomic.Bool consumed by CAS in
`recover()` so the post-hook re-entry into `recover` could skip
the 500ms cooldown. The v3 plan-review found this token leaks
when `RebuildAfterStreamRecreated` is called from Site A (which
does NOT re-enter `recover` — it has its own rebuild path).

v4 drops the bypass token entirely. Both Site A and Site B call
`RebuildAfterStreamRecreated` synchronously after the hook returns
nil; the cooldown is irrelevant for that single immediate rebuild.
The normal cooldown (`minRecoveryInterval` in `Controller.recover`)
continues to apply to subsequent unrelated recoveries, but those
won't be back-to-back with the post-hook rebuild because the
rebuild's `lastRecoveryTime` update gives the cooldown a fresh
baseline.

`HandleStreamRecreated`'s step list is reduced accordingly:

```go
func (c *Controller) handleStreamRecreatedWithSteps(
    ctx context.Context,
    infoFn InfoFunc,
    onStep func(step string),
) {
    if c == nil {
        return
    }
    c.streamEpoch.Add(1)
    if onStep != nil { onStep("after_bump") }

    c.checkpoint.ResetForStreamRecreate()
    if onStep != nil { onStep("after_reset") }

    c.SeedCheckpoint(ctx, infoFn)
    if onStep != nil { onStep("after_seed") }

    c.recreatedSinceLastBuild.Store(true)
    if onStep != nil { onStep("after_flag") }
    // v4: no token set. Caller proceeds directly to RebuildAfterStreamRecreated.
}
```

The Site A detour inside the envelope's `Work` closure:

**v5 fix for v4-P1.1 (stale captured `cons`):** the envelope's Work
closure re-loads `pc.consumer` at the START of each attempt rather
than closing over a fixed `cons` value. This is required so that a
mid-envelope rebuild (Site A success path stores a new consumer)
is visible to the next Work attempt. Implementation either:
(a) re-reads `pc.consumer` under `consumerMu.RLock` at the top of
the closure, OR (b) the outer `runIteratorEnvelope` mutates a
closure-local `cons` variable to `newCons` after the rebuild
succeeds. Either works; the sketch below uses (a) for clarity.

```go
// sketch — production code in runIteratorEnvelope's Work closure
// v5: re-load pc.consumer per attempt so post-rebuild attempts use newCons.
pc.consumerMu.RLock()
cons := pc.consumer
pc.consumerMu.RUnlock()

i, err := pc.iterFactory(cons, batch, expiry)
if err == nil {
    iter = i
    return nil
}
// iter-factory failed. Try escalation/remediation; this may surface
// a stream-missing classification.
escErr := pc.maybeEscalateIteratorFailures(workCtx)
if escErr != nil && natsutil.IsStreamNotFound(escErr) {
    // Stream-missing on the iter-creation path. Detour:
    if hookErr := pc.handleStreamMissing(workCtx); hookErr != nil {
        // Hook absent or returned error; the envelope counts this
        // attempt and either retries or exhausts.
        return fmt.Errorf("%w: %w", types.ErrStreamMissing, hookErr)
    }
    // Hook succeeded; HandleStreamRecreated was called inside
    // handleStreamMissing. Explicitly rebuild the consumer through
    // the recovery config path so the next iter creation uses the
    // post-recreate policy.
    newCons, rebuildErr := pc.recovery.RebuildAfterStreamRecreated(
        workCtx, pc.consumerConfig, pc.recreateFn())
    if rebuildErr != nil {
        // Already wrapped with types.ErrStreamMissing by
        // RebuildAfterStreamRecreated (v4 — handles both still-missing
        // and incompatible-restored-config). Counts against envelope budget.
        return rebuildErr
    }
    pc.consumerMu.Lock()
    pc.consumer = newCons
    pc.consumerMu.Unlock()
    // v4 fix for v3-P0.1: create the iterator from the new consumer
    // BEFORE returning nil. Returning nil with iter unset would have
    // passed a nil iterator into processIterator after the envelope
    // Run returns.
    newIter, iterErr := pc.iterFactory(newCons, batch, expiry)
    if iterErr != nil {
        // Iterator creation failed against the freshly rebuilt
        // consumer. Counts as one envelope attempt. The NEXT
        // attempt's Work re-reads pc.consumer at the top (v5 fix
        // for v4-P1.1) and uses newCons, so the retry stays on the
        // fresh handle — handles transient post-recreate
        // iter-creation flakiness without re-running the hook.
        return iterErr
    }
    iter = newIter
    return nil
}
return err
```

`maybeEscalateIteratorFailures` is refactored to return
`(error)` — non-nil on stream-not-found (passed through from
`ensureConsumer`), nil otherwise. The existing escalation behavior
(re-bind durable on transient failures, return the success/no-op
case) is preserved.

**Final-attempt budget edge fix (also v2-P0.2):** the v2 spec's
"return original error after successful hook" pattern would, on
the envelope's last allowed attempt, fire `OnPermanent` before any
post-hook create attempt ran. v3 fixes this because Work returns
`nil` after `RebuildAfterStreamRecreated` succeeds — the envelope's
Run exits with success, not exhaustion. If `RebuildAfterStreamRecreated`
itself fails with stream-still-missing, that error counts as one
attempt (correct behavior — a "hook lied" attempt is real).

**Site B — iterator-runtime path via `handleIteratorFailure`.**
After `processIterator` returns a non-nil iterErr,
`handleIteratorFailure` calls `pc.recovery.Classify` which may call
`recover` → `recreate` → `js.CreateOrUpdateConsumer`. If the stream
has been deleted mid-session and `CreateOrUpdateConsumer` returns
stream-not-found, `recover` bails with `ActionStreamMissing`. The
detour:

```go
// handleIteratorFailure (modified):
action, newCons, classifyErr := pc.recovery.Classify(ctx, iterErr,
    pc.consumerInfoFn(), pc.consumerConfig, pc.recreateFn())
switch action {
case recovery.ActionExit:
    return true
case recovery.ActionContinue:
    // unchanged from P2.4d shape (assign newCons + reset counters + SeedCheckpoint)
case recovery.ActionBackoff:
    // unchanged
case recovery.ActionStreamMissing:
    // classifyErr wraps types.ErrStreamMissing
    if hookErr := pc.handleStreamMissing(ctx); hookErr != nil {
        // Hook absent / errored. Log + backoff; the next iteration's
        // envelope eventually exhausts → OnPermanentFailure.
        return pc.delayWithBackoffOrExit(ctx, "iterate")
    }
    // v5 fix for v4-P0: Site B must ALSO perform the reset-aware
    // rebuild after the hook returns nil (was previously only in
    // Site A). Otherwise the next iteration starts with the stale
    // pc.consumer and either uses it directly or falls into legacy
    // ensureConsumer with the static pc.consumerConfig — bypassing
    // BuildConfig's recreated-flag override.
    newCons, rebuildErr := pc.recovery.RebuildAfterStreamRecreated(
        ctx, pc.consumerConfig, pc.recreateFn())
    if rebuildErr != nil {
        // RebuildAfterStreamRecreated wraps non-success errors with
        // types.ErrStreamMissing (v4 umbrella). Loop continues to a
        // fresh envelope which will retry; eventual exhaustion fires
        // OnPermanentFailure with the wrapped error → manager observer.
        pc.logger.Warn("post-hook rebuild failed",
            "subject", pc.subject, "error", rebuildErr)
        return pc.delayWithBackoffOrExit(ctx, "iterate")
    }
    pc.consumerMu.Lock()
    pc.consumer = newCons
    pc.consumerMu.Unlock()
    // Reset the per-subject iterator-escalation counters so the
    // legacy escalation doesn't fire spuriously on the fresh
    // consumer (mirrors the ActionContinue branch).
    pc.iterEscMu.Lock()
    pc.iterFailureTimes = pc.iterFailureTimes[:0]
    pc.lastEscalation = time.Time{}
    pc.iterEscMu.Unlock()
    return false // outer loop iterates → fresh envelope uses pc.consumer = newCons
}
```

**The shared `pc.handleStreamMissing` helper:**

```go
// handleStreamMissing invokes the configured StreamMissingHook
// and, on success, drives the post-hook recovery sequence:
// HandleStreamRecreated + compat-check reset signal. Returns:
//   - nil if the hook ran and the post-hook sequence completed
//     successfully (caller should retry the recovery work).
//   - a wrapped types.ErrStreamMissing if the hook is absent, the
//     hook returned an error, or the post-hook sequence failed.
//     The caller's envelope/loop counts this against its budget.
func (pc *partitionConsumer) handleStreamMissing(ctx context.Context) error {
    hook := pc.config.StreamMissingHook
    if hook == nil {
        pc.logger.Warn("stream-missing detected; no hook configured",
            "subject", pc.subject, "stream", pc.streamName)
        if pc.config.Metrics != nil {
            pc.config.Metrics.IncrementWorkerConsumerIteratorRestart("stream_missing_no_hook")
        }
        return fmt.Errorf("%w: stream %q", types.ErrStreamMissing, pc.streamName)
    }
    pc.logger.Info("invoking stream-missing hook",
        "subject", pc.subject, "stream", pc.streamName)
    hookErr := hook(pc.streamName)
    if hookErr != nil {
        pc.logger.Warn("stream-missing hook returned error",
            "subject", pc.subject, "stream", pc.streamName,
            "error", hookErr)
        if pc.config.Metrics != nil {
            pc.config.Metrics.IncrementWorkerConsumerIteratorRestart("stream_recreate_error")
        }
        return fmt.Errorf("%w: stream %q: %w", types.ErrStreamMissing, pc.streamName, hookErr)
    }
    // Hook signalled success; perform the recovery-side reset.
    pc.recovery.HandleStreamRecreated(ctx, pc.consumerInfoFn())
    if pc.config.OnStreamRecreated != nil {
        pc.config.OnStreamRecreated()
    }
    if pc.config.Metrics != nil {
        pc.config.Metrics.IncrementWorkerConsumerIteratorRestart("stream_recreate_success")
    }
    return nil
}
```

Note: metric reason labels (`stream_missing_no_hook`,
`stream_recreate_error`, `stream_recreate_success`) are new
operator-facing observability values. The `IncrementWorkerConsumerIteratorRestart`
method itself is unchanged; only the documented set of reason
strings grows.

The "notify Dynamic" step uses `pc.config.OnStreamRecreated func()`
— a new callback on `partitionConsumerConfig`, forwarded from
`WorkerConsumerConfig`, which the `consumer.Dynamic` wrapper wires
to its own `resetCompatCheck` method. This avoids a circular
package import (recovery → durable → consumer would not compile)
and keeps the recovery package isolated.

### Compat re-arm in `consumer.Dynamic` — single atomic Pointer

The v1 spec used two separate atomics (`workQueueCheckedAt` +
`workQueueErr`), which the v1 plan-review correctly identified as
having a publication race: a reader could observe `checkedAt`
non-nil but `workQueueErr` still old, returning nil when the
checker was about to publish a non-nil error. v2 fixes this by
publishing a single immutable result through ONE `atomic.Pointer`:

```go
// compatCheckResult is the immutable outcome of one compat-check
// run. A nil *compatCheckResult means "not yet checked, or reset
// after a stream recreate".
type compatCheckResult struct {
    err error // non-nil iff the check failed
}

// Was:
//   workQueueOnce sync.Once
//   workQueueErr  error
//
// Becomes:
type Dynamic struct {
    // ... existing fields ...
    workQueueResult atomic.Pointer[compatCheckResult]
    workQueueMu     sync.Mutex // serializes the once-style check
}
```

```go
// In Update AND UpdateWorkerConsumer (both paths run the check;
// this is the v2 fix for the bug that UpdateWorkerConsumer
// bypassed CheckWorkQueueRecoveryCompat entirely):
if err := d.ensureCompatChecked(ctx); err != nil {
    return err
}
return d.inner.Update(...) // or UpdateWorkerConsumer
```

```go
// ensureCompatChecked runs the compat check at most once per
// (un)reset cycle. The fast path is a single atomic load; the
// result struct is immutable once published, so there is no
// publication race between the "is it checked" flag and the
// error value (they are one pointer).
func (d *Dynamic) ensureCompatChecked(ctx context.Context) error {
    if r := d.workQueueResult.Load(); r != nil {
        return r.err
    }
    d.workQueueMu.Lock()
    defer d.workQueueMu.Unlock()
    if r := d.workQueueResult.Load(); r != nil {
        // another goroutine published while we waited
        return r.err
    }
    err := CheckWorkQueueRecoveryCompat(ctx, d.js, d.streamName, d.recoveryStrategy)
    d.workQueueResult.Store(&compatCheckResult{err: err})
    return err
}

// resetCompatCheck clears the cached result so the next
// Update/UpdateWorkerConsumer re-runs the check. Takes the
// workQueueMu mutex (v3 fix for the v2-P0.3 stale-store race) so
// it cannot interleave with an in-flight slow-path check that is
// about to store its now-stale result.
func (d *Dynamic) resetCompatCheck() {
    d.workQueueMu.Lock()
    defer d.workQueueMu.Unlock()
    d.workQueueResult.Store(nil)
}
```

v3 fix for v2-P0.3 (stale-store interleaving): the slow path in
`ensureCompatChecked` is mutex-held from "compute" through
"publish":

```go
func (d *Dynamic) ensureCompatChecked(ctx context.Context) error {
    if r := d.workQueueResult.Load(); r != nil {
        return r.err
    }
    d.workQueueMu.Lock()
    defer d.workQueueMu.Unlock()
    if r := d.workQueueResult.Load(); r != nil {
        return r.err
    }
    err := CheckWorkQueueRecoveryCompat(ctx, d.js, d.streamName, d.recoveryStrategy)
    d.workQueueResult.Store(&compatCheckResult{err: err})
    return err
}
```

Race analysis (v3, mutex-fenced):
- Slow path holds `workQueueMu` from "I'm running the check" through
  "result published". `resetCompatCheck` also takes `workQueueMu`,
  so it cannot interleave between an in-flight check's compute and
  publish — it waits.
- Possible interleavings between A (in-progress slow-path check) and
  B (reset):
  1. A loads nil → A locks → A computes → A publishes →
     A unlocks → B locks → B clears → B unlocks. Reader sees A's
     result briefly then nil. Correct.
  2. B locks first → B clears → B unlocks → A loads nil → A locks
     → A computes → A publishes → A unlocks. Reader sees A's fresh
     result. Correct.
  3. (Pre-fix v2 bug, now impossible): A loads nil → A holds the
     lock → B does Store(nil) WITHOUT the lock → A's still-running
     CheckWorkQueueRecoveryCompat completes → A publishes its now-
     stale result → reader sees stale "compatible" while the stream
     has been recreated. v3 closes this by making B take the lock.
- Concurrent readers on the fast path see either (a) an old result
  that was published before any reset, (b) nil (in which case they
  fall into the slow path and serialise with the in-flight check),
  or (c) a fresh result. All three are acceptable.

`resetCompatCheck` is invoked via the `OnStreamRecreated` callback
the partition consumer fires after `HandleStreamRecreated` returns.
`Dynamic` registers a closure during construction that calls its
own `resetCompatCheck`.

The T3 test in v3 is split into two:

- **T3a (deterministic stale-store interleaving — v2-P0.3 pin).**
  Use a `CheckWorkQueueRecoveryCompat` seam (e.g., inject a
  function whose body blocks on a test-controlled channel).
  Goroutine A starts `ensureCompatChecked`; the seam blocks. Test
  swaps the seam to a "compatible" result on resume. Goroutine B
  calls `resetCompatCheck` — it must BLOCK until A unblocks and
  publishes. Test releases A's seam → A publishes "compatible".
  Then B's reset takes the lock → clears. Reader sees nil →
  forces a fresh check on the next call. Without v3's mutex in
  `resetCompatCheck`, A would publish its stale result AFTER B's
  reset, and the reader would see stale "compatible".
- **T3b (concurrent stress — race-detector).** Spawn N goroutines
  each calling `Update`/`UpdateWorkerConsumer`/`resetCompatCheck`
  in a loop for ~2s under `-race`. Assert no detector triggers.
  Complements T3a (which is deterministic about the bug but does
  not exercise the broader race surface).

## Reproducer tests

### Unit tests (`internal/recovery/`)

- **T2c (epoch fence)** — `internal/recovery/controller_epoch_fence_test.go`.
  Prime `Checkpoint.Seed(100)`. Set `streamEpoch=1`. Wrap a tracking
  msg at seq 80 via `WrapForTracking` (captures epoch=1). Call
  `HandleStreamRecreated` (bumps epoch to 2, resets checkpoint to
  0, seeds with a stub infoFn that returns AckFloor=0). Then call
  `Ack()` on the held tracking msg. Assert `Checkpoint.Value() == 0`.
  Without the fence, `AdvanceCheckpoint` would re-raise the
  checkpoint to 80; with it, the late ack is dropped. THIS IS THE
  TIGHTEST INVARIANT — pin first.

- **T2 (fresh-stream replay)** — `internal/recovery/controller_recreated_replay_test.go`.
  Prime `Checkpoint.Seed(100)`. Strategy=`FromLastProcessed`. Call
  `HandleStreamRecreated` with a stub infoFn returning AckFloor=0
  (fresh stream). Then call `BuildConfig` (or `recover` with a spy
  RecreateFunc capturing the config). Assert
  `DeliverPolicy == DeliverAllPolicy`, `OptStartSeq == 0`.

- **T2b (restored-backup variant)** — same file.
  Prime `Checkpoint.Seed(100)`. Strategy=`FromLastProcessed`. Call
  `HandleStreamRecreated` with a stub infoFn returning AckFloor=50.
  Then call `BuildConfig`. Assert
  `DeliverPolicy == DeliverByStartSequencePolicy`,
  `OptStartSeq == 51`.

- **T2d (restored-consumer incompatible config — v4 updated)** —
  same file. Set up the partition consumer with strategy
  `FromLastProcessed` and a Parti-generated consumer config of
  `AckPolicy=AckExplicit, AckWait=30s`. The hook recreates the
  stream with a same-name consumer but with
  `AckPolicy=AckAll` (incompatible). Spy `RecreateFunc` returns
  `nats consumer-config-mismatch` (or the NATS-specific error code
  10094). Assert:
  - `RebuildAfterStreamRecreated` returns an error that wraps
    `types.ErrStreamMissing` (v4: all post-hook recovery errors
    wrap the typed-error class so the Dynamic→manager observer
    route fires consistently).
  - The envelope counts the attempt; permanent failure eventually
    fires after exhaustion.
  - The manager observer DOES receive the wrapped error (verified
    via spy on `SetOnStreamMissingError`), even though the
    underlying cause is consumer-config-mismatch (not stream-not-found).
  - `enterDegraded` is called with reason
    `"stream-missing-recovery-exhausted"`, NOT
    `"KV error threshold exceeded"`.
  This pins the operator contract: incompatible restored consumer
  config is treated as a stream-recovery failure and surfaces
  through the same observer/degraded route as a true stream-missing
  case.

- **T-SiteA-iter (v4 — v3-P0.1 pin) — post-hook iter creation** —
  `internal/durable/partition_consumer_stream_missing_test.go`.
  Drive the iter-creation path into stream-missing detection;
  hook returns nil; `RebuildAfterStreamRecreated` succeeds with a
  fresh consumer. Assert:
  - `pc.iterFactory` is called AGAIN with the new consumer before
    Work returns nil (verified via spy iterFactory counting calls
    with the new consumer reference).
  - The envelope's Run returns nil (success).
  - `iter` returned by `runIteratorEnvelope` is non-nil.
  - `processIterator` receives the new iterator (NOT nil) and
    processes at least one message from the rebuilt consumer
    (verified by publishing a message after the rebuild).
  This pins v3-P0.1: a hypothetical implementation that returns
  nil from Work without setting `iter` would pass a nil iterator
  to `processIterator` and panic on `iter.Next()`.

- **T-SiteA-iter-retry (v6 — v5-P1.4 pin) — fresh-consumer retry uses newCons** —
  same file. Subcase of T-SiteA-iter that pins the v4-P1.1 fix
  (re-read pc.consumer per attempt). Spy iterFactory behavior:
  - First call (before rebuild): returns iter-creation error
    (drives stream-missing detection).
  - Second call (against newCons, immediately after rebuild):
    returns iter-creation error (transient post-recreate flakiness).
  - Third call (next envelope attempt): also against newCons —
    must NOT be against the pre-rebuild consumer.
  Assert that all three calls to iterFactory after the rebuild
  use the SAME consumer reference (newCons), proving the Work
  closure re-reads pc.consumer per attempt. A broken implementation
  that captures cons once before the envelope would invoke the
  third call against the old (deleted/stale) consumer.

- **T-SiteB-rebuild (v7 — v5-P1.3 + v6-P1.1 pin) — Site B hook-success
  reset-aware rebuild** —
  `internal/durable/partition_consumer_stream_missing_site_b_test.go`.
  Drive processIterator into the recovery path that reaches
  ActionStreamMissing. The realistic flow when a JetStream stream
  is deleted is:
  1. The contained consumer is deleted too (NATS behavior).
  2. `iter.Next()` returns `jetstream.ErrConsumerDeleted`
     (NOT direct ErrStreamNotFound — `recovery.ClassifyError` only
     maps consumer-gone/heartbeat to recovery actions).
  3. handleIteratorFailure → `Classify(ErrConsumerDeleted)` →
     `ErrorConsumerGone` → `recover()`.
  4. `recover()` calls `recreate()` = `pc.recreateFn()` =
     `js.CreateOrUpdateConsumer`.
  5. `CreateOrUpdateConsumer` returns `ErrStreamNotFound`
     (the stream is gone).
  6. `recover()` bails with `ActionStreamMissing` + wrapped error.

  Test setup:
  - Inject `iter.Next` returning `jetstream.ErrConsumerDeleted`
    (use the existing `errorOnNextIter` mock from
    `partition_consumer_test.go`).
  - Spy `RecreateFunc` is called via `pc.recreateFn()`. First call
    (during recover): returns `jetstream.ErrStreamNotFound` to
    simulate the stream-gone state. Second call (during
    RebuildAfterStreamRecreated after hook returns nil): returns
    a fresh consumer mock.
  - Hook returns nil.

  Assert:
  - `RebuildAfterStreamRecreated` is called with checkpoint = 0
    AND `recreatedSinceLastBuild = true` (verified via Controller
    state inspection or by capturing the value passed to BuildConfig).
  - The config captured by the SECOND RecreateFunc call has
    `DeliverPolicy == DeliverAllPolicy` (the recreated-flag
    override fired).
  - `pc.consumer` is updated to the new consumer returned by
    RebuildAfterStreamRecreated.
  - handleIteratorFailure returns false (don't exit; outer loop
    iterates with fresh envelope using the new consumer).
  - The next envelope iteration's runIteratorEnvelope.Work reads
    pc.consumer and sees the NEW consumer (via the v6 re-read pattern).

  Original v6 assertions retained below:
  - `RebuildAfterStreamRecreated` is called with the (now-reset)
    checkpoint = 0 and recreatedSinceLastBuild = true.
  - The captured config has `DeliverPolicy == DeliverAllPolicy`
    (the v3 fresh-stream replay invariant via BuildConfig's
    one-shot recreated-flag override).
  - `pc.consumer` is updated to the new consumer returned by
    RebuildAfterStreamRecreated.
  - handleIteratorFailure returns false (don't exit; outer loop
    iterates with fresh envelope using the new consumer).
  - The next envelope iteration's runIteratorEnvelope.Work reads
    pc.consumer and sees the NEW consumer (via the v6 re-read pattern).
  This pins v4-P0 + v5-P1.3: a hypothetical Site B implementation
  that only calls handleStreamMissing without RebuildAfterStreamRecreated
  would either keep using the stale consumer OR fall through to
  legacy ensureConsumer with the static (DeliverAllPolicy from
  dynamicbuild but without the reset-aware checkpoint integration)
  config — either way, the captured config assertion would fail.

- **T6 (recreated flag is one-shot)** — same file. The v1 spec made
  this test non-load-bearing by advancing the checkpoint > 0
  before the second recovery: in BuildConfig, `checkpoint > 0`
  wins before the flag is read, so a stuck flag is masked. v2
  fixes this by driving the second recovery with `checkpoint == 0`:
  1. Trigger the recreate path once (sets the flag); spy
     BuildConfig captures `DeliverAllPolicy` on the first
     post-hook build (consumes the flag).
  2. Reset the checkpoint back to 0 directly (test seam — calls
     `Checkpoint.ResetForStreamRecreate()` to simulate "no
     progress since recreate").
  3. Invoke `recover()` again on an unrelated transient
     classification (`ErrConsumerDeleted`) with the SAME strategy.
  4. Assert the second BuildConfig output uses `DeliverNewPolicy`
     (the `default` branch — flag cleared, no checkpoint), NOT
     `DeliverAllPolicy`.

  On a stuck-flag implementation, the second build would emit
  `DeliverAllPolicy` and replay everything in the (current,
  unrelated) stream from sequence 1 — a real production hazard.

- **T6b (direct read-and-clear primitive)** — same file. Sibling
  unit test that directly exercises the flag's read-and-clear
  semantics, independent of BuildConfig. `controller.recreatedSinceLastBuild`
  is exposed via a private package test helper (e.g.,
  `consumeRecreatedFlag()` returns the old value AND clears the
  flag atomically). Test: set flag → first call returns true and
  clears → second call returns false. Pins the primitive
  separately from the BuildConfig assertion.

- **`HandleStreamRecreated` ordering — deterministic step seam** —
  `internal/recovery/controller_recreated_ordering_test.go`. The
  test uses the private `handleStreamRecreatedWithSteps(ctx,
  infoFn, onStep)` helper (defined under "HandleStreamRecreated"
  above). The `onStep` callback, when invoked at `"after_reset"`,
  calls `Ack()` on a held tracking msg captured at the prior
  epoch. Assert that on return `Checkpoint.Value() == 0` (the
  ack was dropped by the epoch fence). In a hypothetical
  reset-before-bump implementation, `"after_reset"` would mean the
  epoch is still the prior value, the ack would pass the fence,
  and the checkpoint would advance to the held msg's seq —
  deterministically failing this test. No sleeps, no scheduling
  assumptions.

### Unit test (`consumer/`)

- **T3 (compat re-arm both paths)** —
  `consumer/dynamic_compat_rearm_test.go`. Construct a `Dynamic`
  with a mock `js` that returns a stream with
  `LimitsPolicy + FromLastProcessed` (compat-OK) on first
  `StreamInfo` call. Call `Update`; assert compat check ran. Call
  `UpdateWorkerConsumer`; assert compat check **did NOT** re-run
  (it's still cached from Update). Invoke
  `d.resetCompatCheck()` (via the new `OnStreamRecreated`
  callback). Switch the mock `js` to return
  `WorkQueuePolicy + FromLastProcessed` (compat fail). Call
  `UpdateWorkerConsumer`; assert compat check ran and returned
  `ErrInvalidConfig`. Also assert the inverse direction: today
  `UpdateWorkerConsumer` bypasses the check entirely (the test
  fails on parent with the inverse assertion).

### Integration tests (`test/integration/failure/`)

- **T1 (hook fires)** —
  `test/integration/failure/stream_missing_hook_test.go`. Real
  embedded NATS. Configure manager with `StreamMissingHook` that
  records its invocation. Start manager, drive an assignment that
  spins up a partition consumer. Delete the dynamic-consumer
  stream via `js.DeleteStream`. Assert the hook fires within
  one recovery-controller cycle, with the correct stream name.

- **T4 (no hook → OnError + readiness flip)** —
  `test/integration/failure/stream_missing_no_hook_test.go`. Same
  setup, no hook configured. Use a SPY js (wrapping the real
  embedded NATS js) that counts `CreateOrUpdateConsumer` calls.
  Delete the stream. Assert (all):
  - F2 envelope exhausts within `MaxAttempts * MaxBackoff` window.
  - `OnPermanentFailure` (P2.4d's callback) fires once.
  - The manager-level `Hooks.OnError` fires with an error
    satisfying `errors.Is(err, parti.ErrStreamMissing)`. NOT
    classified as KV-bucket-loss (`recordKVError` is not invoked
    with this error — verified by spying on `enterDegraded`'s
    `reason` argument: it equals `"stream-missing-recovery-exhausted"`,
    NOT `"KV error threshold exceeded"`).
  - State transitions to degraded.
  - **Post-exhaustion silence**: from the spy's perspective, the
    count of `CreateOrUpdateConsumer` calls is monotonic after
    permanent failure fires — i.e., zero additional calls in the
    1 second following the OnError signal. This pins the
    no-retry-storm contract specifically for the no-hook path
    (T5 already pins it for the hook-error path; v1 spec had this
    gap per P1.5).

- **T5 (hook returns error → no retry storm)** —
  `test/integration/failure/stream_missing_hook_error_test.go`.
  Same setup, hook returns a non-nil error every call. Delete the
  stream. Assert:
  - The hook fires multiple times (up to MaxAttempts).
  - F2 envelope eventually exhausts.
  - **After exhaustion, no further `CreateOrUpdateConsumer`
    calls are issued** — verifiable via a spy js.

## Verification gates

- `make lint && make test && make test-race && make test-integration`
  green.
- `make pre-pr` chained gate passes (lint + test + test-integration).
- Docs: `docs/CONSUMERS.md` documents `StreamMissingHook` with a
  worked `provision`-based recreate example.
- New exported-symbol audit (v3):
  - `types.ErrStreamMissing` (sentinel `error`); `parti.ErrStreamMissing`
    alias.
  - `types.StreamMissingHook` (`func(streamName string) error`) in
    `types/stream_hooks.go`; `parti.StreamMissingHook` alias.
  - `durable.WorkerConsumerConfig.StreamMissingHook` field.
  - `durable.WorkerConsumerConfig.OnPermanentFailure` was added by
    P2.4d (already exported).
  - `consumer.DynamicConfig.StreamMissingHook` field +
    `consumer.WithStreamMissingHook(StreamMissingHook) DynamicOption`.
    The option's godoc (NOT just the underlying type's godoc) MUST
    document the allowed-strategy restriction and point readers
    to `StreamMissingHook` for the full hook contract. v8 — v7-P2
    fix: option-level godoc is required to prevent drift between
    the option and the underlying type's contract (parallel
    convention to `WithRecoveryStrategy` whose godoc lives on
    the option, not the type).
  - `consumer.DynamicConfig.OnPermanentFailure` field +
    `consumer.WithOnPermanentFailure(...) DynamicOption`.
  - `consumer.Dynamic.SetOnStreamMissingError(func(streamName string, err error))`
    method on `*consumer.Dynamic` (v3 — replaces v2's
    `StreamMissingObserver` interface; the public Dynamic method
    is the wiring surface).
  - `recovery.StreamMissingObserver` interface (one method:
    `SetOnStreamMissingError(...)`) — implemented by
    `*consumer.Dynamic`. `Manager.Start` type-asserts the
    registered updater against this interface.
  - **No new methods on `types.WorkerConsumerMetrics`** — the
    existing `IncrementWorkerConsumerIteratorRestart(reason string)`
    is reused with new reason labels (see "Metrics" subsection).
  - **No new generic-exhaustion typed error** (v3: dropped v2's
    `types.ErrConsumerRecoveryExhausted`; generic exhaustion uses
    the existing log + metric path).
  - `internal/retry`, `internal/recovery`, `internal/durable` stay
    internal. The new `recovery.Controller.RebuildAfterStreamRecreated`
    and `recovery.Controller.handleStreamRecreatedWithSteps` are
    internal-package methods; only `Controller.HandleStreamRecreated`
    is the exported-by-package wrapper.
  - **`manager_degraded.go:recordKVError` is edited** (v4 — v3-P1.3
    fix) to short-circuit on `errors.Is(err, types.ErrStreamMissing)`
    so stream-missing errors do not double-count in the KV error
    threshold. This is an in-tree edit, not a new export.
  - **`manager_setup.go:prepareStart`** (v4 — v3-P1.2 fix) gains
    the type-assertion + `SetOnStreamMissingError` wiring at the
    end of the function. No new exports.
  - **`CompositeConsumerUpdater.SetOnStreamMissingError`** (v5 —
    v4-P1.3 fix) is a new public method on the existing
    `parti.CompositeConsumerUpdater` type that forwards the
    observer registration to each child updater implementing
    `recovery.StreamMissingObserver`. Added so the manager bridge
    works correctly when the registered updater is a composite
    wrapping a `*consumer.Dynamic`.
  - **`Manager.onStreamMissingError`** (v5) is an unexported
    method holding the observer closure; not part of the public
    API.
- Cross-feature contract regression check (AGENTS.md § Cross-feature
  contracts):
  - `TestManager_LiveNATSBucketLoss*` (whole-bucket loss → all
    workers degraded via `recordKVError`) still green; the
    stream-missing path does NOT route through `recordKVError`.
  - `TestStableID_StaleKeyTakeover_Reclaim` (peer claim takeover →
    only that worker) still green.
  - `TestManager_LiveNATSBucketLoss_OnDegradedHook` (one-shot
    OnDegraded per Degraded entry) still green.
- Concurrency stress test for the new monitor-shaped surface (T2c
  race scenario above) executes under `-race`.

## How this trips readiness

Two paths (same as the high-level spec):
1. **Hook present, recreate succeeds.** No readiness trip; consumer
   resumes from AckFloor of the fresh stream (or replays from
   sequence 1 if AckFloor==0).
2. **Hook absent OR hook returns error.** After F2 envelope-bounded
   retries the consumer enters permanent failure →
   `OnPermanentFailure` fires (P2.4d's hook) → the manager's
   degraded-mode wiring (existing) trips readiness → pod rotation.

A pod restart alone does NOT restore the stream
(`Manager.Start` ensures only KV buckets, not message streams).

## Dependencies & sequencing

**Depends on P2.4d** — already merged at `2c359b0`. The F2 envelope
on `partition_consumer.go`'s recovery loop is what turns T4/T5's
"no hook = `OnError` + readiness flip" and "hook returns error =
no retry storm" into observable behavior.

Phase 2 sequence: P2.1 → P2.2 → P2.4a → P2.4b → P2.4c → P2.4d →
**P2.3** → P2.5. P2.3 is the second-to-last self-healing PR.

## Open questions / decisions

All v1 open questions were resolved by the v1 plan-review process.

1. **Error type for OnError in T4.** RESOLVED: public sentinel
   `types.ErrStreamMissing` (with `parti.ErrStreamMissing` alias);
   wraps the underlying NATS error and the stream name. See
   "Public typed error" subsection above.
2. **`StreamMissingHook` placement.** RESOLVED: lives in
   `types/stream_hooks.go` (a new file, not in `types/hooks.go`).
   `types.Hooks` is a manager-lifecycle struct of callbacks; the
   stream-missing hook is a consumer-config callback used at a
   different boundary. Separating the file keeps each boundary's
   documentation surface focused.
3. **HandleStreamRecreated ordering test design.** RESOLVED:
   private `handleStreamRecreatedWithSteps(ctx, infoFn, onStep)`
   helper with deterministic test seam; production callers use the
   exported `HandleStreamRecreated` wrapper. See "T2c-ordering
   deterministic test" subsection above.

## Out of scope

- Library auto-recreating the stream (category B forbidden).
- The retry envelope itself (F2's territory — landed in P2.4a/b/c/d).
- The recovery-controller consolidation (separate, future effort).
- Manager-side stream-missing telemetry beyond the new reason
  labels enumerated above.
- Re-routing existing stream-missing detection in
  `provision/apply_stream.go` or `consumer/static.go` (the spec
  scope is the Dynamic consumer's recovery loop only).

## Revision log

### v2 (2026-05-24)

Addresses all 11 v1 plan-review findings
(`tmp/09-pr9-spec_plan_review_v1.md`, verdict "revise — 6 P0 +
5 P1"):

- **P0.1 (hook detour wrong path):** Routed stream-missing from
  BOTH the iter-creation site (inside `runIteratorEnvelope`'s
  Work closure) AND the iter-runtime site
  (`handleIteratorFailure` → `Classify` → `ActionStreamMissing`)
  through a shared `pc.handleStreamMissing` helper.
- **P0.2 (no-hook readiness wiring missing):** Added explicit
  manager-facing wiring via new public surface
  (`consumer.DynamicConfig.OnPermanentFailure`,
  `recovery.StreamMissingObserver` interface implemented by
  `*Dynamic`, manager-side closure registration in
  `Manager.Start`). T4 can now assert
  `errors.Is(err, parti.ErrStreamMissing)` and degraded state.
- **P0.3 (T6 non-load-bearing):** Restructured T6 to use
  checkpoint==0 for the second recovery; assert second build is
  `DeliverNewPolicy`, not `DeliverAllPolicy`. Added T6b that
  exercises the read-and-clear primitive directly.
- **P0.4 (ordering test fragile):** Added private
  `handleStreamRecreatedWithSteps(ctx, infoFn, onStep)` helper
  with deterministic test seam; the ordering test injects an
  old-epoch ack between "after_reset" and the implicit "after_bump"
  (which, in the correct ordering, has already happened) and
  asserts the checkpoint is unchanged regardless of scheduling.
- **P0.5 (compat re-arm publication race):** Replaced two atomics
  with a single `atomic.Pointer[compatCheckResult]` whose
  `compatCheckResult` is immutable once published. T3 extended
  with a concurrent stress test under `-race`.
- **P0.6 (unsafe cooldown bypass):** Replaced unconditional
  clearing of `lastRecoveryTime` with a bounded one-shot
  `postHookBypassPending` token consumed via CAS in `recover()`.
  At most one cooldown skip per `HandleStreamRecreated` call.
- **P1.1 (OnError public typed error):** Defined
  `types.ErrStreamMissing` + `parti.ErrStreamMissing` alias.
  Internal recovery package wraps `jetstream.ErrStreamNotFound`
  with this public sentinel.
- **P1.2 (Classify caller under-scope):** Audited all four callers
  (Dynamic, Queue, Broadcast, internal/ipartition); each updated
  to handle `ActionStreamMissing` (mapped to `ActionBackoff` for
  non-Dynamic callers that don't own stream lifecycle).
- **P1.3 (restored-consumer same-durable-name rule):** Hook
  godoc explicitly states the same-name requirement and the
  fallback semantics when a different name is supplied.
- **P1.4 (new metrics break public interface):** Dropped the
  proposed new metric methods. Reused
  `IncrementWorkerConsumerIteratorRestart(reason string)` with
  three new reason labels.
- **P1.5 (T4 post-exhaustion silence):** T4 now uses a spy js
  and asserts zero `CreateOrUpdateConsumer` calls in the 1s
  window following permanent-failure firing, plus asserts the
  degraded reason is `"stream-missing-recovery-exhausted"`,
  NOT `"KV error threshold exceeded"` (cross-feature contract
  preservation).
- **P2.1 (hook placement):** Committed to `types/stream_hooks.go`
  (new file) rather than expanding `types/hooks.go`.

### v10 (2026-05-24)

Addresses the single v9 plan-review finding
(`tmp/09-pr9-spec_plan_review_v9.md`, verdict "revise — 0 P0 +
1 P1"):

- **v9-P1 (cross-package consistency test references unexported
  helpers):** the v9 test could not compile because Go forbids
  cross-package access to unexported helpers. v10 rewrites the
  test to compare the PUBLIC `consumer.DynamicConfig.Validate`
  and `durable.WorkerConsumerConfig.Validate` surfaces instead.
  The test lives in `package consumer_test` (which can import
  both `consumer` and `internal/durable`) and asserts equivalent
  accept/reject outcomes across a 4-strategy matrix. The intent
  (catch future divergence) is preserved; the implementation
  shape (compare public surfaces, not helpers) is correctly
  expressible in Go.

### v9 (2026-05-24)

Addresses all v8 plan-review findings
(`tmp/09-pr9-spec_plan_review_v8.md`, verdict "revise — 0 P0 +
1 P1 + 1 P2"):

- **v8-P1 (shared helper crosses package boundary):** v9 replaces
  the cross-package "shared helper" with two PACKAGE-LOCAL helpers
  (one in `consumer/dynamic.go`, one in `internal/durable/config.go`).
  Both implement IDENTICAL strategy-checking logic with minor
  error-message prefix differences. A new cross-package
  consistency test (`TestValidateStreamMissingHookStrategy_ConsistentAcrossPackages`)
  pins that both helpers accept and reject the same set of
  strategies; future divergence is caught by the test.
- **v8-P2 (incomplete test coverage for OK cases):** v9 adds the
  `_OK` test cases for all three Validate surfaces ×
  RecoverFromLastProcessed/RecoverFromBeginning. Total
  validation coverage is now 12 tests (4 strategies × 3 surfaces)
  + 1 cross-package consistency test = 13.

### v8 (2026-05-24)

Addresses all v7 plan-review findings
(`tmp/09-pr9-spec_plan_review_v7.md`, verdict "revise — 0 P0 +
1 P1 + 1 P2"):

- **v7-P1 (RecoverFromNew rejection under-specified):** v8 names
  THREE public validation surfaces (`consumer.NewDynamic`,
  `consumer.DynamicConfig.Validate`,
  `durable.WorkerConsumerConfig.Validate`) and factors the
  rejection logic into a shared `validateStreamMissingHook`
  helper. Lists 8 named regression tests across two test files
  covering all three surfaces × the four strategy variants
  (RecoverFromNew rejected, RecoveryDisabled rejected,
  RecoverFromLastProcessed accepted, RecoverFromBeginning
  accepted).
- **v7-P2 (WithStreamMissingHook option godoc missing):** v8
  adds an explicit requirement in the exported-symbol audit that
  the option's godoc documents the allowed-strategy restriction
  and points to the underlying type — parallel to the existing
  convention where `WithRecoveryStrategy` carries the trade-off
  documentation on the option.

### v7 (2026-05-24)

Addresses all v6 plan-review findings
(`tmp/09-pr9-spec_plan_review_v6.md`, verdict "revise — 0 P0 +
2 P1"):

- **v6-P1.1 (T-SiteB-rebuild iter error can't reach
  ActionStreamMissing):** v7 rewrites the T-SiteB-rebuild test
  setup to drive `iter.Next()` returning `ErrConsumerDeleted`
  (the realistic NATS behavior when a stream is deleted — the
  contained consumer goes with it). The spy `RecreateFunc`
  returns `ErrStreamNotFound` on the recover() call, surfacing
  ActionStreamMissing through the existing classification path.
  The test now exercises the actual code path the implementation
  uses, not a fictional direct-stream-not-found classification.
- **v6-P1.2 (`RecoverFromNew` validation guidance contradicts
  fresh-stream replay contract):** v7 narrows the allowed
  recovery strategies for `StreamMissingHook`. Only
  `RecoverFromLastProcessed` and `RecoverFromBeginning` are
  accepted; `RecoverFromNew` is explicitly rejected with a
  message explaining the message-skip hazard. The hook godoc
  documents the allowed strategies. BuildConfig's FromNew branch
  is intentionally NOT modified — the validation makes the unsafe
  path unreachable. A regression test pins the rejection.

### v6 (2026-05-24)

Addresses all v5 plan-review findings
(`tmp/09-pr9-spec_plan_review_v5.md`, verdict "revise — 0 P0 +
4 P1"):

- **v5-P1.1 (StreamMissingHook + RecoveryDisabled undefined):**
  v6 validates the combination at consumer construction:
  `NewDynamic` (and `WorkerConsumerConfig.Validate`) rejects
  `StreamMissingHook != nil` paired with `RecoveryDisabled` with
  a clear error message. The library documents that the minimal
  enabling strategy is `RecoverFromNew`.
- **v5-P1.2 (composite forwarding not implementation-ready):**
  v6 corrects the field name (`updaters`, not `children`), adds
  a `sync.Mutex` to serialize observer-related reads/writes, and
  threads the registered observer through `Add()` so late-added
  observer-capable children inherit the manager-installed
  observer. Composite_updater tests pin both the
  construction-time and Add-time forwarding paths.
- **v5-P1.3 (Site B reset-aware rebuild lacks regression test):**
  v6 adds T-SiteB-rebuild that drives processIterator into
  iter-runtime stream-missing classification (mid-session
  stream deletion) and asserts the post-hook config passed to
  the spy RecreateFunc has `DeliverAllPolicy` AND that
  pc.consumer is updated to the new consumer.
- **v5-P1.4 (Site A fresh-consumer retry lacks test):**
  v6 adds T-SiteA-iter-retry subcase: spy iterFactory fails on
  the first call against newCons, then asserts subsequent calls
  in the same envelope episode are also against newCons (not
  the pre-rebuild consumer). Pins v4-P1.1's "re-read pc.consumer
  per attempt" invariant.

### v5 (2026-05-24)

Addresses all v4 plan-review findings
(`tmp/09-pr9-spec_plan_review_v4.md`, verdict "revise — 1 P0 +
4 P1"):

- **v4-P0 (Site B hook success skips reset-aware rebuild):**
  Site B's `ActionStreamMissing` success branch now calls
  `pc.recovery.RebuildAfterStreamRecreated`, stores the new
  consumer under `consumerMu`, and resets the per-subject
  iterator-escalation counters. Mirrors Site A's behavior.
- **v4-P1.1 (Site A retry uses stale captured `cons`):** the
  envelope's Work closure now re-reads `pc.consumer` at the start
  of each attempt (under `consumerMu.RLock`) rather than closing
  over a fixed `cons` value. Post-rebuild retries use the fresh
  consumer.
- **v4-P1.2 (ErrStreamMissing contradictory contracts):** the
  godoc for `types.ErrStreamMissing` now explicitly states it is
  an UMBRELLA covering the full stream-missing recovery episode —
  including post-hook restored-config failures — not just literal
  "stream absent". The `StreamMissingHook` godoc removes the
  v3-era "invoked AGAIN" wording and instead states the operator's
  responsibility (reconcile or delete the restored consumer).
- **v4-P1.3 (manager wiring uses wrong field name + missing
  Composite forwarding):** the wiring snippet now uses
  `m.consumerUpdater` (the actual Manager field name). The
  manager closure is extracted as `m.onStreamMissingError` for
  reuse. `parti.CompositeConsumerUpdater` gains a
  `SetOnStreamMissingError` method that forwards to children
  implementing the observer interface.
- **v4-P1.4 (stale `setBypassToken` text in
  `HandleStreamRecreated` section):** removed the step-5 token
  reference from the first `HandleStreamRecreated` section. The
  step list is now bump → reset → seed → flag. All
  `setBypassToken` / `after_token` text outside the v3-revision
  history is removed.

### v4 (2026-05-24)

Addresses all v3 plan-review findings
(`tmp/09-pr9-spec_plan_review_v3.md`, verdict "revise — 2 P0 +
4 P1"):

- **v3-P0.1 (Site A returns nil iterator):** v4 adds the iter-
  creation step inside Site A's success branch — after
  `RebuildAfterStreamRecreated` stores the new consumer,
  `iterFactory(newCons, batch, expiry)` runs and the result is
  assigned to `iter` before Work returns nil. If iterFactory fails
  against the freshly rebuilt consumer, that error counts as one
  envelope attempt (retried on the next iteration). New test
  T-SiteA-iter pins this.
- **v3-P0.2 (incompatible restored-config has no route):**
  `RebuildAfterStreamRecreated` now wraps ALL post-hook recovery
  errors with `types.ErrStreamMissing` (not just stream-not-found
  variants). The Dynamic→manager observer route now fires for
  both true stream-missing and incompatible-restored-config
  cases. T2d updated to assert the observer fires + the
  `enterDegraded` reason is `stream-missing-recovery-exhausted`.
- **v3-P1.1 (signature contract stale in sketches):** v4 locks
  `recover` to `(Consumer, error)` (no bool), removes the
  "optional" language for `BuildConfig`'s 4-arg signature, and
  updates all sketches to use `types.ErrStreamMissing` (not
  unqualified `ErrStreamMissing`).
- **v3-P1.2 (manager wiring under-specified):** v4 includes a
  concrete `manager_setup.go` snippet showing exactly where the
  type-assertion goes (end of `prepareStart`, after `m.ctx` is
  initialized), what closure is installed, and that it uses
  `m.logError` (not a direct `Hooks.OnError` call) so the manager's
  wait-group and lifecycle context are correctly tracked.
- **v3-P1.3 (`recordKVError` short-circuit doesn't exist):** v4
  adds the explicit `manager_degraded.go:recordKVError` edit to
  the file plan with the short-circuit code:
  `if errors.Is(err, types.ErrStreamMissing) { return }`.
- **v3-P1.4 (cooldown bypass token leak):** v4 drops the bypass
  token entirely. Site A/B both call `RebuildAfterStreamRecreated`
  synchronously after the hook; the immediate post-hook rebuild
  doesn't observe the cooldown anyway. `HandleStreamRecreated` no
  longer sets a token; the `setBypassToken` method and
  `postHookBypassPending` field are removed.

### v3 (2026-05-24)

Addresses all v2 plan-review findings
(`tmp/09-pr9-spec_plan_review_v2.md`, verdict "revise — 3 P0 +
4 P1 + 2 P2"):

- **v2-P0.1 (no-hook Dynamic callback bridge):** `*consumer.Dynamic`
  owns a `userOnPermanentFailure` field + a `managerOnStreamMissing
  atomic.Pointer[func]` slot. `NewDynamic` ALWAYS wires
  `WorkerConsumerConfig.OnPermanentFailure` to `d.onPermanentFailure`,
  an indirection closure that reads both at fire time.
  `SetOnStreamMissingError` is a public method on `*Dynamic` that
  swaps the atomic pointer — a `Manager.Start` call after
  `NewDynamic` correctly reaches the durable layer.
- **v2-P0.2 (Site A success doesn't rebuild via reset-aware
  config):** added `Controller.RebuildAfterStreamRecreated(ctx,
  baseCfg, recreate)` which reads-and-clears `recreatedSinceLastBuild`,
  calls `BuildConfig` against the (now-reset) checkpoint, and
  invokes `recreate` to construct the new consumer. Site A's
  detour calls this method after `handleStreamMissing` returns nil,
  then stores the new consumer under `consumerMu`. Work returns
  nil on success → envelope's Run exits successfully → no final-
  attempt-budget edge.
- **v2-P0.3 (compat re-arm reset race):** `resetCompatCheck` takes
  `workQueueMu` (v2 only took it in the slow path of
  `ensureCompatChecked`, allowing a stale-store interleaving).
  T3 split into T3a (deterministic stale-store interleaving) and
  T3b (concurrent race-detector stress).
- **v2-P1.1 (recover/Classify signature inconsistent):** v3 locks
  the signature: `Controller.recover` → `(Consumer, error)`;
  `Controller.Classify` → `(Action, Consumer, error)`. All sketches
  updated.
- **v2-P1.2 (non-Dynamic ActionStreamMissing claims absent sink):**
  v3 explicitly states "log + backoff" for Queue, Broadcast,
  internal/ipartition — no surfacing to any callback. Each adds
  a log line and a test pinning the mapping.
- **v2-P1.3 (`ErrConsumerRecoveryExhausted` not audited):** dropped.
  Generic exhaustion flows through unchanged (log + metric, no
  typed wrapper). v3 scope is `ErrStreamMissing` only.
- **v2-P1.4 (same-durable-name incompatible config undefined):**
  hook godoc explicitly states the compatible-config rule. New
  T2d pins the incompatible-restored-config behavior:
  `RebuildAfterStreamRecreated` returns a non-stream-missing error;
  envelope retries until exhaustion.
- **v2-P2.1 (stale TBD ordering test wording):** ordering test
  block in the reproducer-test list rewritten to reference the
  deterministic `handleStreamRecreatedWithSteps` seam. No sleeps,
  no logs.
- **v2-P2.2 (`consumeBypassToken_locked` name/lock contract):**
  renamed `setBypassToken` (atomic; no lock).

### v2 (2026-05-24)

Addressed all 11 v1 plan-review findings. Verdict from v2 review:
revise — 3 new P0 + 4 new P1 + 2 P2 findings.

### v1 (2026-05-24)

Initial draft. Verdict: revise — 6 P0 + 5 P1 findings.
