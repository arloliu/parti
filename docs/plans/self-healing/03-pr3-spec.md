# P0.3 (F10-B) — Two-phase handoff misconfiguration warning

Per-PR spec for the third PR in Phase 0
(`00-fix-plan.md` §P0.3). Lazy-written per the plan's convention; prior
PR (P0.2, branch `self-healing-p02-f8-reconcile-guard`) is committed
and surfaced.

## Background

`Config.EnableTwoPhaseHandoff` (`config.go:412`, default false) gates
the manager-side two-phase handoff coordinator. When true, the
coordinator publishes per-partition claims into the handoff bucket and
relies on the consumer wiring a **processing gate** that consults
those claims before delivering messages. If the consumer does NOT
wire a gate (`internal/durable/processing_gate.go`'s
`ProcessingGate.Enabled` is false), the claims are written and
**never consulted** — two-phase handoff is effectively disabled even
though the manager-side flag is on. The result is a misconfigured
deployment that silently behaves as if the flag were false.

The manager already has the signal it needs: `consumerUpdater`'s
optional `CapabilityReporter.Capabilities()` returns a bitmask
including `CapProcessingGate` when the consumer's handler is
gate-wrapped (`internal/durable/worker_consumer.go:386-398,
604-623`). `Manager.reportConsumerCapabilities` (`manager.go:822`)
samples this after every `handoffCoordinator.Apply`. Today, if the
bit never appears, nothing surfaces the discrepancy.

A `Start`-time check is not viable — the gate bit is set when the
consumer wraps the handler, which happens **during** an apply (not
during construction). The check must fire on or after the first
apply.

## Scope (read-only — no behavior change)

Add a one-shot WARN inside `reportConsumerCapabilities` that fires
when **all** of the following hold (after the existing capability
merge):

1. `m.cfg.EnableTwoPhaseHandoff == true`
2. `m.capProcessingGateWarned` has not been set yet
3. `m.capabilities.Load() & uint32(types.CapProcessingGate) == 0`

Setting the warned guard is one-shot for the lifetime of the manager.
A subsequent apply that DOES wire the gate (operator rolled the
consumer config and re-deployed) will not re-trigger the warning —
that is the plan's intended behavior; a single noisy log line at the
first apply is enough to surface the issue.

## Design

Add an `atomic.Bool` guard field on `Manager`:

```go
// capProcessingGateWarned guards the one-shot warning that fires
// when EnableTwoPhaseHandoff is true and the consumer has not
// reported CapProcessingGate after the first sampling opportunity.
// One-shot per Manager lifetime (matches the plan's design).
capProcessingGateWarned atomic.Bool
```

Modify `reportConsumerCapabilities` to call a new
`maybeWarnMissingProcessingGate` at the end (after the existing
short-circuit + Or-in-newBits logic). When `capReporter == nil` or
when `missing == 0` (gate already in `m.capabilities`), the function
returns early as today and the warning is correctly silent.

```go
func (m *Manager) reportConsumerCapabilities() {
    if m.capReporter == nil {
        return
    }
    missing := reportableCapBits &^ m.capabilities.Load()
    if missing == 0 {
        return // already includes CapProcessingGate; nothing to warn about
    }
    newBits := m.capReporter.Capabilities() & missing
    if newBits != 0 {
        m.capabilities.Or(newBits)
    }
    m.maybeWarnMissingProcessingGate()
}

func (m *Manager) maybeWarnMissingProcessingGate() {
    if !m.cfg.EnableTwoPhaseHandoff {
        return
    }
    if m.capProcessingGateWarned.Load() {
        return
    }
    if m.capabilities.Load()&uint32(types.CapProcessingGate) != 0 {
        return // gate is present post-merge; silent
    }
    if !m.capProcessingGateWarned.CompareAndSwap(false, true) {
        return // raced with another goroutine
    }
    m.logger.Warn(
        "two-phase handoff is enabled but the consumer reports no processing gate; "+
            "partition claims are written and never consulted",
        "remedy", "wire a processing gate on the consumer (e.g. consumer.Dynamic) so claims fence delivery",
    )
}
```

**Limitation acknowledged in code comments.** If the consumer
updater does NOT implement `CapabilityReporter`
(`m.capReporter == nil`), no `Capabilities()` call ever happens; the
warning cannot fire because the signal is unavailable. This is
intentional and documented — the warning depends on capability
reporting. Consumers that bypass capability reporting are responsible
for their own gate wiring verification.

## Reproducer test list

- *T1 (must fail on parent — primary).* Construct a `Manager` with
  `EnableTwoPhaseHandoff = true`, a `CapabilityReporter` stub that
  returns `0` (no gate bit). Drive one `applyAssignmentWithPrev`
  call. Assert the warning is emitted exactly once. On parent (no
  warning logic), the assertion fails (zero warnings).
- *T2.* Same setup but the stub returns `CapProcessingGate`. Drive
  apply. Assert **no** warning is emitted. Prevents false-positive
  on the happy path.
- *T3 (idempotency).* Same as T1 (gate absent). Drive apply twice in
  succession. Assert the warning is emitted exactly **once**
  (the `capProcessingGateWarned` guard's load-bearing property).
- *T4 (flag-off silence).* `EnableTwoPhaseHandoff = false`. Stub
  returns 0. Drive apply. Assert no warning. Confirms the warning
  is gated by the flag.
- *T5 (nil reporter silence).* `EnableTwoPhaseHandoff = true`,
  `m.capReporter = nil`. Drive apply. Assert no warning fires —
  the documented limitation. (No `Capabilities()` call happens, so
  the gate signal is undetectable.)

## Verification gates

- `make lint && make test && make test-race` green.
- No exported API change.
- New manager struct field is unexported.
- Code-review attention: the guard is single-shot per Manager
  lifetime; subsequent applies with a wired gate do NOT reset it
  (documented in the field's Godoc).

## How this trips readiness

It doesn't directly. The warning makes a misconfiguration that today
silently neutralises two-phase handoff **operator-visible**. The
operator's remedy is a deployment-time fix (enable the gate on the
consumer); readiness rotation cannot recover this on its own because
the misconfiguration is in the consumer's wiring, not in the
manager's runtime state.

## Out of scope

- Rejecting the misconfiguration at `Start` — explicitly forbidden by
  the plan (the gate bit is set at runtime, not at construction).
- Adding a construction-time "gate-capable" predicate — listed as a
  future follow-up in the plan, not in scope here.
- Synthesising the warning for consumers that don't implement
  `CapabilityReporter` — out of scope per the design rationale
  (signal undetectable).

## Dependencies & sequencing

Independent. Last of Phase 0 because the test (driving apply with a
stub updater) is the most integration-shaped of the Phase 0 tests;
landing P0.1/P0.2 first establishes the warning-helper rhythm at the
mechanical end.
