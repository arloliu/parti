# P0.1 (F7) — Connection-config docs + `MaxReconnects` startup warning

Per-PR spec for the first PR in the self-healing phased plan
(`00-fix-plan.md` §P0.1). Lazy-written now per the plan's convention
("specs are written only when the prior PR is merge-clean"; this is the
first PR, so the gate is trivially satisfied).

## Background

`Manager` does not hold a `*nats.Conn` directly; it holds a
`jetstream.JetStream`. The underlying connection is reachable via
`m.js.Conn()` (returns `*nats.Conn`; verified against nats.go v1.50.0
`jetstream/jetstream.go:571`). The `Opts` field on `*nats.Conn` exposes
the reconnect-budget knob. The plan anchor "manager.go:258, 410-414 —
conn is caller-injected" is correct in spirit (the connection IS
caller-owned, constructed externally and threaded in via
`jetstream.New(nc)`); the warning reaches the conn through
`js.Conn().Opts`.

**Field-name note (discovered during implementation, not in the plan).**
The setter is `nats.MaxReconnects(...)` (plural — `nats.go:1108`) but
the field on `nats.Options` is `MaxReconnect` (singular —
`nats.go:374`). The helper, test, and log message all use the singular
field name when referring to the field, and the plural name when
referring to the setter. The user-visible docs use the plural setter
form since that is what callers invoke. This naming asymmetry is a
nats.go quirk, not a Parti quirk.

## Scope (read-only / docs-only — no behavior change)

1. **Docs.** Add a new "NATS Client Connection" subsection to
   `docs/OPERATIONS.md` (after the existing "NATS Server Configuration"
   subsection) documenting the required connection posture for a Parti
   manager: `MaxReconnects = -1` (unlimited), reasonable
   `ReconnectWait`/`ReconnectJitter`, `RetryOnFailedConnect = true`.
   Name the failure mode finite `MaxReconnects` produces (the
   `CLOSED` zombie connection that today eventually trips degraded
   mode via connection monitoring; review §F7).
2. **Godoc.** Update the `doc.go` package-comment example to call out
   the connection posture in a one-line comment above the
   `nats.Connect` call. The example body stays compact.
3. **Example.** Update `examples/basic/main.go` to use the documented
   connection options. The example is the public entry point — must
   not teach the foot-gun.
4. **Runtime warning.** Add `(m *Manager) warnOnFiniteMaxReconnects()`
   alongside `warnOnShortAuditGrace` in `manager_setup.go`. Invoke
   from `Manager.Start` after the existing `warnOnShortAuditGrace`
   call (`manager.go:394`). The warning is **read-only** — it
   inspects `m.js.Conn().Opts.MaxReconnects` and emits a single
   `Warn`-level log line if the value is finite (i.e. `>= 0`).
   `-1` (unlimited) is silent. `m.js.Conn()` returning `nil` is
   defensively silent (test-double safety).

## Design

```go
// In manager_setup.go, alongside warnOnShortAuditGrace:

// warnOnFiniteMaxReconnects emits a one-shot WARN when the caller-owned
// nats.Conn is configured with a finite MaxReconnects. A finite
// MaxReconnects turns a transient NATS outage into a permanently CLOSED
// zombie connection — the manager's connection monitor then enters
// degraded mode and the readiness probe rotates the pod. This is the
// documented posture (see docs/OPERATIONS.md "NATS Client Connection"),
// but the warning surfaces the misconfiguration at Start so operators
// catch it during smoke test rather than during the first outage.
//
// MaxReconnects == -1 (unlimited) is the recommended posture and is
// silent here. Anything else (0 disables reconnect entirely; positive
// finite caps) warns.
func (m *Manager) warnOnFiniteMaxReconnects() {
    if m.js == nil {
        return
    }
    conn := m.js.Conn()
    if conn == nil {
        return // defensive (test doubles that bypass the real conn)
    }
    max := conn.Opts.MaxReconnects
    if max < 0 {
        return // -1 = unlimited, the recommended posture
    }
    m.logger.Warn(
        "nats.Conn is configured with a finite MaxReconnects; on a sustained "+
            "NATS outage the connection will exhaust its budget and stay CLOSED, "+
            "forcing degraded mode and pod rotation. Configure the connection "+
            "with nats.MaxReconnects(-1) and a reasonable nats.ReconnectWait/"+
            "ReconnectJitter (see docs/OPERATIONS.md \"NATS Client Connection\").",
        "max_reconnects", max,
    )
}
```

Call site in `Manager.Start` (after the existing
`m.warnOnShortAuditGrace()` at `manager.go:394`):

```go
m.warnOnShortAuditGrace()
m.warnOnFiniteMaxReconnects()
```

## Reproducer test list

No correctness reproducer is required (no behavior change). One unit
test for the warning emission, table-driven over the three relevant
inputs:

- *T1.* `MaxReconnects = -1` → no warning.
- *T2.* `MaxReconnects = 0` → warning emitted exactly once.
- *T3.* `MaxReconnects = 5` (finite positive) → warning emitted
  exactly once.
- *T4.* `js == nil` → silent (defensive); no panic.
- *T5.* `js.Conn() == nil` → silent (defensive); no panic.

The test uses the existing `captureLogger` pattern from
`pull_gating_repro_test.go:23-71` (move to a shared test helper if
needed; preferred: keep local since one other call site already uses
it and the helper is tiny). The test does **not** spin up a real
NATS server — it constructs the `Manager` directly with a real
`*nats.Conn` whose `Opts.MaxReconnects` is set to each table value.
Connect to an embedded NATS server (existing
`internal/testutil/nats.StartEmbeddedNATS`) so `js.Conn()` returns a
real conn whose `Opts` are populated.

## Verification gates

- `make lint && make test && make test-race` green.
- New exported symbols: **none** (the helper is unexported).
- Docs spot check: `go doc github.com/arloliu/parti/v2` shows the
  updated package-comment example.
- `examples/basic/main.go` builds (`go build ./examples/basic`).
- Manual: construct a `Manager` against an embedded NATS with
  `nats.MaxReconnects(5)` and confirm the warning appears once at
  `Start`; switch to `nats.MaxReconnects(-1)` and confirm silence.

## How this trips readiness

Indirect. The warning itself does not flip readiness. It documents
the posture so finite `MaxReconnects` (which today turns an outage
into a `CLOSED` zombie that *does* enter degraded mode and rotate the
pod) is operator-visible at first deploy rather than during an
outage.

## Test-helper alignment (in scope, ancillary)

`partitest.StartEmbeddedNATS` connects with `nats.MaxReconnects(3)`
(`partitest/nats.go:78`). Once the warning is wired, every integration
test using this helper (354 call sites) would emit the warning — the
helper itself models the foot-gun this PR documents away. Change the
helper to `nats.MaxReconnects(-1)` to (a) silence the spurious
warning noise in tests, and (b) make the test infrastructure model the
recommended posture. This is a single-line change and is congruent
with this PR's teaching.

Also reviewed: `test/simulation/internal/natsutil/embedded.go:19`
(`StartEmbeddedNATS` — separate simulation helper). Check whether it
sets `MaxReconnects` and align if so.

## Out of scope

- Programmatic enforcement (rejecting finite `MaxReconnects` outright).
  The plan explicitly rules this out (review §F7); the warning is the
  agreed-upon mitigation.
- Touching `cmd/partictl/natsconn.go` — that is an operator tool with
  different lifecycle assumptions. Not in scope.
- Any other connection posture knob (TLS, auth). Only the
  reconnect-budget knob produces the silent-zombie outcome.

## Dependencies & sequencing

Independent. First PR of Phase 0 because it is the smallest
no-behavior-change change in the plan.
