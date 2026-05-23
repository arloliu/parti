# P0.2 (F8) — `source.WithReconcileInterval(0)` guard

Per-PR spec for the second PR in Phase 0
(`00-fix-plan.md` §P0.2). Lazy-written now per the plan's convention.
Gates: P0.1 is merge-clean (branch `self-healing-p01-f7-conn-config`
committed and surfaced to user).

## Background

`NatsKV.reconcileLoop` (`source/nats_kv.go:807-814`) exits immediately
when `s.reconcileInterval <= 0` AND `s.leadershipProbe == nil`. The
reconciler is the **load-bearing recovery path** for KV watchers after
a NATS server restart — the empirical finding pinned in memory
`project_nats_watcher_empirical_finding` notes that the nats.go KV
watcher's `Updates()` channel does NOT close on server restart, so the
reconciler is the only mechanism that re-syncs the source after a
silently-stalled watcher.

A user who passes `source.WithReconcileInterval(0)` therefore disables
server-restart recovery without any visible signal. The current Godoc
documents the cadence behavior but does not name the **silent-stall
consequence**.

The consumer-resolver counterpart (`consumer/resolver_config.go:61-63`,
`internal/durable/config.go:89`) is already safe — both clamp
non-positive intervals to a sane minimum. The source layer is the
only outstanding gap.

## Scope (source-only — no behavior change)

1. **Godoc.** Update `source.WithReconcileInterval`'s comment to add a
   paragraph naming the silent-stall consequence and pointing to the
   load-bearing reconciler finding (memory pin reference). Keep the
   existing parameter/return notation.
2. **Runtime warning.** At `NatsKV.Start`, emit a `Warn`-level log
   when **both** of the following hold:
   - `s.reconcileInterval <= 0`
   - `s.leadershipProbe == nil`
   This is precisely the condition `reconcileLoop` uses to exit
   immediately. With a leadership probe wired, the reconciler still
   runs (driven by the probe), so the warning would be a false
   positive — guard against it.

The warning is **read-only**; the configuration is not rejected. Per
the plan: "rejection is more disruptive (existing users who explicitly
disable polling break on upgrade); the safer route is to make the
foot-gun loud."

## Design

```go
// In source/nats_kv.go, modify WithReconcileInterval's Godoc:

// WithReconcileInterval sets the periodic-reconcile ticker cadence. A value of
// 0 disables polling entirely. The default is 30s. When WithLeadershipProbe is
// also set the fixed interval is ignored in favour of the leadership-driven
// cadence (leader=30s / follower=5min).
//
// Disabling the reconciler (setting d <= 0 with no leadership probe)
// disables the load-bearing recovery path for the source watcher: after
// a NATS server restart, the nats.go KV watcher's Updates() channel does
// NOT close, and the reconciler is the only mechanism that re-syncs the
// source state. Disable polling only with full awareness that
// server-restart recovery will not work — Start emits a one-shot WARN
// log line when this state is detected so the choice is visible.
//
// Parameters:
//   - d: Reconcile interval (0 disables; default 30s)
//
// Returns:
//   - NatsKVOption: Option function
func WithReconcileInterval(d time.Duration) NatsKVOption { ... }
```

And in `Start` (after the existing watcher seed, before launching
the loops — the logical place is between the lifecycle context
setup at `nats_kv.go:244` and the `go reconcileLoop` launch at
`nats_kv.go:256`):

```go
// Warn when the reconciler is disabled with no leadership probe.
// The reconcileLoop exits immediately on this condition (see
// reconcileLoop at L812-815); without it, a silent NATS server
// restart can leave the source watcher stalled with no recovery
// path. See memory pin project_nats_watcher_empirical_finding.
if s.reconcileInterval <= 0 && s.leadershipProbe == nil {
    s.logger.Warn("source reconciler disabled; server-restart recovery will not work — call source.WithReconcileInterval with a positive duration (or wire a leadership probe via source.WithLeadershipProbe)")
}
```

## Reproducer test list

No correctness reproducer required (no behavior change). Unit tests:

- *T1.* `WithReconcileInterval(0)` + no leadership probe → warning
  emitted exactly once at `Start`.
- *T2.* `WithReconcileInterval(0)` + leadership probe set → silent
  (the probe makes the reconciler still run).
- *T3.* Default reconciler interval (omit `WithReconcileInterval`) →
  silent. The default is 30s (`nats_kv.go`); the warning must not
  fire on the happy path.
- *T4.* Negative interval (`-1s`) + no leadership probe → warning
  emitted (matches the `<= 0` guard).

Use a capture-style logger fixture (reuse `captureLogger` if cleanly
accessible from the source package's test, otherwise write a small
local one — the source package does not import `parti.captureLogger`,
so a local helper is the pragmatic choice).

## Verification gates

- `make lint && make test && make test-race` green.
- Godoc check: `go doc github.com/arloliu/parti/v2/source.WithReconcileInterval`
  shows the new paragraph.
- No exported API change.

## How this trips readiness

Indirect. The warning makes a configuration that produces a
readiness-blind silent stall **operator-visible** at first deploy
rather than during an actual NATS restart event. The actual recovery
mechanism still depends on the reconciler being enabled; this PR
just refuses to let the operator disable it accidentally.

## Out of scope

- `consumer.ResolverConfig` and `internal/durable/config.go` are
  already safe (both clamp non-positive intervals). Do not touch.
- Programmatic rejection (returning an error from
  `WithReconcileInterval` or `Start`). The plan explicitly chooses
  warning over rejection.

## Dependencies & sequencing

Independent. Second of Phase 0 because the warning-helper pattern
this PR mirrors (P0.1's `warnOnFiniteMaxReconnects`) is now familiar
to the codebase.
