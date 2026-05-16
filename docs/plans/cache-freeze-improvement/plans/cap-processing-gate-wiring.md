# CapProcessingGate Wiring — Implementation Plan

## Problem statement

`internal/assignment/calculator_audit.go:15` defines:

```go
const requiredAuditCaps = types.CapAckV1 | types.CapTwoPhaseHandoff | types.CapProcessingGate
```

The leader-side audit-escalation path (`maybeEscalateAudit`,
`calculator_audit.go:157`) requires every "behind" worker AND every
escalation target to advertise this full bitmask. Without it, the worker
is filtered out at lines 165 (`cap_missing_behind`) or 182 (no targets,
`cap_missing_targets`), and the audit-repair `EnterScaling("audit_repair")`
call at line 208 never fires.

`grep -rn "SetCapability.*CapProcessingGate" --include="*.go"` returns
matches **only** in `manager_test.go`. **No production code calls
`SetCapability(types.CapProcessingGate, true)`.**

Consequence: in production, every worker advertises `CapProcessingGate=0`,
so the leader's audit-repair path is currently unreachable. Stuck workers
that miss `ApplyGracePeriod + ExtendedApplyGracePeriod` cannot be
reassigned via the audit path. The `manager.go:732` docstring claims
"the consumer/updater calls `SetCapability(types.CapProcessingGate, true)`
when it wraps handlers with the processing gate" — this contract is not
honored.

The bit IS load-bearing for safety: the gate is what guarantees only the
legitimate owner processes during handoff. Without the gate, two workers
could double-process. So `requiredAuditCaps` correctly demands the gate
before any rebalance away from a worker. The current setup is "fail safe
by accident": audit-repair is disabled because no worker reports the cap,
not because the gate isn't safe to require. Fix the wiring, restore the
intended audit-repair behavior.

## Design constraints

1. **v2 API stability.** `parti.Manager`, `parti.WorkerConsumerUpdater`,
   `consumer.Dynamic`, `consumer.DynamicConfig` are public. Any new
   method must be additive, not a breaking change to existing types.
2. **One-way wiring today.** `Manager → consumerUpdater` (via the
   `WorkerConsumerUpdater` interface registered with
   `WithWorkerConsumerUpdater`). There is no `Consumer → Manager`
   backchannel. The fix must either add one or use a different signal
   path (e.g., a constructor option that captures `mgr.SetCapability`
   as a callback at consumer construction time).
3. **Bit reflects RUNTIME wire-up, not config intent.** The plan doc's
   §4.1 / item #52 explicitly demands the bit reflect "actual wireup"
   — flipping false on gate init failure. So the bit is set inside
   `internal/durable/worker_consumer.go:382-388` *after* the
   `newProcessingGate(...)` call returns nil error, not at config
   parse time.
4. **First-success semantics.** WorkerConsumer creates per-subject
   consumers lazily on `UpdateWorkerConsumer`. Cap should be set on
   the first successful gate wrap. If a later per-subject create fails
   with the same gate config, the cap stays true — the gate is wired,
   one subject just failed.
5. **No cap if `ProcessingGate.Enabled=false`** or
   `ProcessingGate=nil`. Trivially derived from config.

## Proposed API: optional `CapabilityReporter` interface

Add to root `parti/` package (new file `capability_reporter.go`):

```go
// CapabilityReporter is an optional interface a WorkerConsumerUpdater
// MAY implement to report runtime capabilities back to the Manager.
//
// When the registered updater (or any child of a composite updater)
// satisfies this interface, Manager queries Capabilities() after each
// handoff apply attempt and ORs the returned bits into its capability
// bitmask via SetCapability.
//
// Implementations MUST be:
//   - Safe for concurrent calls. Capabilities() may be invoked from
//     the manager-apply goroutine (which calls
//     reportConsumerCapabilities after every handoffCoordinator.Apply
//     attempt) and may race with the updater's own UpdateWorkerConsumer
//     calls. The heartbeat publisher does NOT call Capabilities()
//     directly — it reads Manager.Capabilities() (the live bitmask
//     callback) — so the race surface is reporter ↔ updater, not
//     reporter ↔ heartbeat.
//   - Non-blocking. Capabilities() is invoked on every apply attempt;
//     must not perform I/O or acquire locks held by long operations.
//     A simple atomic load is the expected shape.
//   - Monotonic for runtime-wire-up bits such as CapProcessingGate:
//     once a capability has been successfully wired (e.g., a handler
//     wrapped with the processing gate), the corresponding bit MUST
//     remain set for the rest of the updater's lifetime even if a
//     later per-subject create fails. The bit reflects "this updater
//     has at least one wired component", not "all components are
//     currently wired".
//
// Manager integration semantics for the reporter integration are
// OR-only for runtime-wire-up bits: reportConsumerCapabilities calls
// SetCapability(bit, true) for known reported bits and never clears.
// (Manager.SetCapability itself supports clearing bits via active=false,
// used by other components — but the reporter pathway only sets.)
// Implementers should not rely on a returned-zero Capabilities()
// causing the manager to clear a previously-reported bit; it won't.
//
// Returning 0 is always safe (no caps advertised).
type CapabilityReporter interface {
    Capabilities() uint32
}
```

### Composite forwarding

`*CompositeConsumerUpdater` MUST implement `CapabilityReporter` to
preserve composition. The composite holds children as
`[]WorkerConsumerUpdater` (`composite_updater.go:19-20`), so the
manager's interface assertion is on the composite itself, not its
children. Without this, a composite that contains a reporting
WorkerConsumer would still report 0 to the manager.

```go
func (c *CompositeConsumerUpdater) Capabilities() uint32 {
    var bits uint32
    for _, u := range c.updaters {
        if cr, ok := u.(CapabilityReporter); ok {
            bits |= cr.Capabilities()
        }
    }
    return bits
}
```

Composition rule: OR. Capabilities are a bitmask; a feature is
"wired" if any updater in the composite has wired it. Children that
do not implement `CapabilityReporter` contribute 0.

### Public-wrapper forwarding (`consumer.Dynamic`)

`consumer.Dynamic` is the public API users register with
`WithWorkerConsumerUpdater`. It wraps an internal
`durable.WorkerConsumer` in field `inner` (`consumer/dynamic.go:267-277`),
and `Update` / `UpdateWorkerConsumer` delegate to that inner
(`consumer/dynamic.go:314-322`).

`*consumer.Dynamic` MUST implement `CapabilityReporter` by forwarding
to its inner WorkerConsumer:

```go
// Add to consumer/dynamic.go:
func (d *Dynamic) Capabilities() uint32 {
    return d.inner.Capabilities()
}
```

Without this, the manager-side type assertion on the registered
updater (`*consumer.Dynamic`) finds no `CapabilityReporter`, even when
the inner `WorkerConsumer` is reporting `CapProcessingGate`.
Compile-time assertion in a public-package test:

```go
var _ parti.CapabilityReporter = (*consumer.Dynamic)(nil)
```

### Inner reporter (`internal/durable/worker_consumer.go`)

```go
type WorkerConsumer struct {
    // ... existing fields ...
    gateWired atomic.Bool // monotonic: never cleared once set
}

// Capabilities reports the capability bits this consumer has
// successfully wired at runtime. Currently:
//   - CapProcessingGate: set after the first successful handler-wrap
//     via newProcessingGate(...) + g.Wrap.
//
// Safe for concurrent use; non-blocking; monotonic.
func (wc *WorkerConsumer) Capabilities() uint32 {
    var bits uint32
    if wc.gateWired.Load() {
        bits |= types.CapProcessingGate
    }
    return bits
}
```

At `worker_consumer.go:387` after `effectiveHandler = g.Wrap(wc.handler)`:

```go
wc.gateWired.Store(true)
```

The Store is unconditional (CAS not required) because (a) the value
flipped to true is monotonic and (b) repeated true-stores are
idempotent. The bit reflects "at least one subject was successfully
wrapped with the gate"; if a later subject in the same
`UpdateWorkerConsumer` call fails, the bit stays true (which is the
correct semantic — the gate IS wired for the subject(s) that
succeeded).

### Manager wiring — call site and ordering

The manager's apply path is in `manager_assignment.go:applyAssignmentWithPrev`.
The actual updater call is encapsulated inside
`m.handoffCoordinator.Apply(...)` (`manager_assignment.go:745-751`),
which routes through either `internal/assignment/handoff/direct.go:28-40`
(direct mode) or `internal/assignment/handoff/twophase.go:80-110`
(two-phase). The updater can succeed (gate gets wrapped) even when a
later commit/stabilize phase of two-phase fails.

Therefore: sample capabilities **after every `handoffCoordinator.Apply`
return, regardless of error**, and **before** the success-path
`heartbeat.SetAppliedAssignment` + `heartbeat.PublishNow`
(`manager_assignment.go:784-798`). This guarantees:
1. Caps wired by an apply that later errors are still picked up on the
   next sample (because Manager's bitmask is OR-only, so the bit stays
   set).
2. The immediate post-apply heartbeat (`PublishNow`) carries the new
   cap bit, instead of waiting one heartbeat-interval tick.

Define a private helper:

```go
// reportConsumerCapabilities samples the registered consumer updater's
// runtime capabilities (if it implements CapabilityReporter) and ORs
// the reported bits into the Manager's capability bitmask. Idempotent
// and safe to call from any goroutine.
//
// Called after every handoffCoordinator.Apply attempt — even on error
// — because the updater (which actually wraps handlers with the gate)
// may have succeeded before the apply pipeline failed in a later phase.
func (m *Manager) reportConsumerCapabilities() {
    cr, ok := m.consumerUpdater.(CapabilityReporter)
    if !ok {
        return
    }
    bits := cr.Capabilities()
    // Filter to known runtime-wireup bits. Adding new reportable bits
    // (CapAckV1, CapTwoPhaseHandoff in the future) is opt-in here.
    for _, bit := range []uint32{types.CapProcessingGate} {
        if bits&bit != 0 {
            m.SetCapability(bit, true)
        }
    }
}
```

Call site in `applyAssignmentWithPrev` (pseudo-diff):

```go
applyErr := m.handoffCoordinator.Apply(ctx, prev, asgn)

// Sample caps unconditionally — the updater may have wired the gate
// even if a later phase failed.
m.reportConsumerCapabilities()

if applyErr != nil {
    // ... existing error handling ...
    return applyErr
}

// Existing: SetAppliedAssignment + PublishNow.
// reportConsumerCapabilities ran first, so PublishNow's heartbeat
// carries the new cap bit on the very first successful apply.
m.heartbeat.SetAppliedAssignment(...)
m.heartbeat.PublishNow(ctx)
```

This satisfies "bit reflects runtime wire-up": the bit only flips true
after `newProcessingGate` returns nil error AND `g.Wrap` succeeds AND
the manager has had at least one Apply attempt to sample.

## Why CapabilityReporter interface (vs. alternatives)

Alternatives considered:

- **Constructor-time callback** (`consumer.WithCapabilitySetter(mgr.SetCapability)`):
  Requires the user to wire `mgr.SetCapability` into consumer construction.
  Burden on user; easy to forget; defeats the "internal plumbing should
  Just Work" intent. Rejected.
- **Polling at Manager Start**: Misses the case where gate wraps lazily
  on first `UpdateWorkerConsumer`. The first per-subject consumer is
  created during the first apply, not at Start. Rejected.
- **Push from WorkerConsumer to Manager via a registered hook**: Same
  shape as `CapabilityReporter` but inverted (push not pull). Pull is
  simpler — Manager controls timing — and aligns with how the heartbeat
  publisher already pulls via `capsFn` callback. Chosen.

`CapabilityReporter` is interface-assertion-detected (no method on the
existing `WorkerConsumerUpdater` interface), so it's purely additive
and existing `WorkerConsumerUpdater` implementations don't need to
change.

## Exact change set

**Public API additions (root `parti/` package):**
- New file `capability_reporter.go` — `CapabilityReporter` interface +
  Godoc per the §"Proposed API" contract.
- `manager.go` — add private helper
  `(m *Manager) reportConsumerCapabilities()`.

**Manager wiring (`manager_assignment.go`):**
- In `applyAssignmentWithPrev`, after `m.handoffCoordinator.Apply(...)`
  returns (regardless of error), call `m.reportConsumerCapabilities()`
  before any post-apply error return AND before the success-path
  `heartbeat.SetAppliedAssignment` / `heartbeat.PublishNow`.

**Composite forwarding (`composite_updater.go`):**
- Add `(c *CompositeConsumerUpdater) Capabilities() uint32` that
  iterates `c.updaters`, type-asserts each to `CapabilityReporter`,
  and ORs the returned bits.

**Public-wrapper forwarding (`consumer/dynamic.go`):**
- Add `(d *Dynamic) Capabilities() uint32` that forwards to
  `d.inner.Capabilities()`.

**Internal change (`internal/durable/worker_consumer.go`):**
- Add `gateWired atomic.Bool` field on `WorkerConsumer`.
- Store `true` to `gateWired` after the `effectiveHandler = g.Wrap(...)`
  line at `worker_consumer.go:387`.
- Add `(wc *WorkerConsumer) Capabilities() uint32` method returning
  `CapProcessingGate` when `gateWired` is set, else 0.
- Add `sync/atomic` to the import block (currently has `sync` but not
  `sync/atomic`). `types` is already imported — no new import needed
  there.

**Docstring fix (`manager.go:732`):**
- Update the existing comment "the consumer/updater calls
  SetCapability(types.CapProcessingGate, true)..." to point at the
  new `CapabilityReporter` mechanism so the docstring matches the
  implementation.

**Tests:**

1. `TestWorkerConsumer_GateWiredReportsCapProcessingGate` (unit, in
   `internal/durable/worker_consumer_test.go`): construct WorkerConsumer
   with `ProcessingGate.Enabled=true`, drive one `UpdateWorkerConsumer`
   that wraps at least one subject, assert `Capabilities() &
   CapProcessingGate != 0`. Construct with gate disabled, drive same
   call, assert `Capabilities() == 0`.
2. `TestWorkerConsumer_GateBitMonotonic_StaysSetAfterLaterSubjectError`
   (unit): construct with gate enabled; drive `UpdateWorkerConsumer`
   with multiple subjects where the first wraps successfully and a
   later per-subject create is forced to fail; assert
   `Capabilities() & CapProcessingGate != 0` after the failed call.
   Drive a second `UpdateWorkerConsumer`; assert the bit is still set.
3. `TestCompositeConsumerUpdater_CapabilitiesORs` (unit, in
   `composite_updater_test.go`): three children — none reporting; one
   reporting `CapProcessingGate`; one reporting different (synthetic)
   bit. Assert composite returns 0; then `CapProcessingGate`; then the
   OR of all reported bits as composition is added.
4. `TestDynamic_ImplementsCapabilityReporter` (compile-time + behavior,
   in `consumer/dynamic_test.go`): `var _ parti.CapabilityReporter =
   (*consumer.Dynamic)(nil)` plus a runtime test that constructs
   `consumer.Dynamic` with gate enabled, drives one update, asserts
   `cd.Capabilities() & CapProcessingGate != 0`.
5. `TestManager_CapProcessingGate_ReportsAfterFirstApply` (integration,
   in `manager_test.go`): real Manager + `consumer.Dynamic` with gate
   enabled, registered via `WithWorkerConsumerUpdater`; drive an
   assignment; assert `mgr.Capabilities() & CapProcessingGate != 0`;
   read the heartbeat KV directly and assert
   `hb.Capabilities & CapProcessingGate != 0` on the immediate
   post-apply heartbeat (proves `PublishNow` saw the new bit).
6. `TestManager_CapProcessingGate_StaysClearWithoutGate` (integration):
   same setup but gate disabled; drive assignment; assert bit stays 0
   in both `mgr.Capabilities()` and the published heartbeat.
7. `TestManager_CapProcessingGate_SampledAfterUpdaterOnApplyError`
   (integration or hybrid): proves the ordering invariant
   `reportConsumerCapabilities` runs AFTER updater work, not before.
   Use a stub `WorkerConsumerUpdater` that implements
   `CapabilityReporter` with this exact shape:
   - `Capabilities() uint32` returns the value of an
     internal `reported atomic.Uint32` (initially 0).
   - `UpdateWorkerConsumer(ctx, ...)` atomically stores
     `CapProcessingGate` into `reported`, then returns a non-nil error
     (simulating updater succeeding the gate-wrap step but then a later
     phase failing).

   Drive `applyAssignmentWithPrev`; assert it returns an error AND
   `mgr.Capabilities() & CapProcessingGate != 0` after return.
   Because `reported` is 0 before `UpdateWorkerConsumer` runs, a buggy
   implementation that samples BEFORE the updater would fail this
   assertion. The test thus enforces the post-updater sampling order,
   not just the broad "error path samples something" behavior.
8. `TestManager_CapProcessingGate_EmptyAssignmentStaysClear` (integration):
   register `consumer.Dynamic` with gate enabled; drive an apply with
   an EMPTY assignment (no partitions, no subjects to wrap); assert
   bit stays 0. Subsequent apply with a non-empty assignment flips it
   on.

**Skipped per scope:**
- "Force gate init failure flips back to false" (original plan #52
  scenario 3). The new design is monotonic-set per the
  `CapabilityReporter` contract; once the gate has wrapped any subject,
  the bit stays set. This deliberately diverges from the spec text;
  flag for Phase 7 docs sync to align the spec with the implemented
  monotonic semantics.

## Failure modes — addressed in design above

1. **Composite updater path**: handled — `*CompositeConsumerUpdater`
   implements `CapabilityReporter` itself, ORing across children. See
   §"Composite forwarding" and Test #3.
2. **Public-wrapper path**: handled — `*consumer.Dynamic` implements
   `CapabilityReporter` by forwarding to its inner. See
   §"Public-wrapper forwarding" and Test #4.
3. **Apply succeeded-then-errored mid-pipeline**: handled —
   `reportConsumerCapabilities` runs after every Apply attempt
   regardless of error, before any return. See §"Manager wiring —
   call site and ordering" and Test #7.
4. **Per-subject create fails after earlier wrap**: handled — the
   `gateWired.Store(true)` is monotonic; later Store calls are
   idempotent; later subject errors don't clear it. See Test #2.
5. **Race**: `applyAssignmentWithPrev` may run concurrently with
   heartbeat compose (`internal/heartbeat/publisher.go:329-345`).
   `Capabilities()` reads `gateWired atomic.Bool`. Atomicity covers
   the read. `SetCapability` uses `m.capabilities.Or(...)` (atomic
   uint32 Or). No mutex needed.
6. **Heartbeat timing**: handled by ordering —
   `reportConsumerCapabilities` runs BEFORE
   `heartbeat.SetAppliedAssignment` + `heartbeat.PublishNow`, so the
   immediate post-apply heartbeat carries the new bit. No reliance on
   the next ticker.
7. **`UpdateWorkerConsumer` for an empty assignment**: handled —
   `WorkerConsumer` won't create any per-subject consumers, so
   `gateWired` stays false; `Capabilities()` returns 0; manager's bit
   stays 0. Subsequent non-empty apply flips it on. See Test #8.
8. **First Apply on cold-start with no consumerUpdater registered**:
   `m.consumerUpdater` is nil; `reportConsumerCapabilities` early-returns
   on the type assertion. No-op, no panic.

## Out of scope for this plan

- Fixing the spec text in `docs/plans/cache-freeze-improvement/00-original-plan.md`
  to reflect that audit deliberately does NOT escalate on malformed
  commits (calculator_audit.go:127). That is a Phase 7 docs sync.
- Phase 6 E2E tests #68-70.

## Estimated scope

Revised after plan-review v1 (added Dynamic + composite forwarding +
4 additional tests):

- Production: ~120 LOC (Godoc + interface + composite + dynamic +
  worker_consumer changes + manager helper + apply-path call site).
- Tests: ~250 LOC (8 tests across 5 packages).
- Single PR, single commit on success, ~3/4 day of focused work.
