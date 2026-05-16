# Phase 4 Follow-up — Gap 1: Expose ReconcileInterval + Audit-Grace Safety Warning

## Origin

After landing the claim-resolver watcher restart + periodic reconcile fix
(commit `5bc46cc`) and the NATS-restart integration test (commit
`ac4c383`), a v3 post-impl review surfaced a gap: real NATS server
restarts do NOT close the watcher channel — recovery in that case is
delivered by the periodic reconciler only, and the default reconcile
interval is 30 s.

The user's reported production failure ran with a short `HeartbeatTTL`
(inferred ~4 s from the 20 s symptom window), which implies an
`ExtendedApplyGracePeriod` of ~20 s — *shorter* than the resolver's
30 s reconcile period. In that configuration, after a silent watcher
stall the leader-side audit can fire `audit_repair` while the worker's
resolver cache is still stale, producing the cascading reassignment
behaviour the original report describes.

This phase exposes `ReconcileInterval` through the consumer-facing
`ResolverConfig` and adds a Manager-startup warning when the implied
audit grace is shorter than the default reconcile period.

## Scope

Files in scope:

- `consumer/resolver_config.go` — add `ReconcileInterval` field.
- `internal/durable/config.go` — mirror the field on the internal
  `ResolverConfig`.
- `consumer/dynamic.go` — `toSubscriptionResolverConfig` carries the
  field across.
- `internal/durable/worker_consumer.go` — `ensureGateResolver` passes
  `durable.WithReconcileInterval(d)` to `NewClaimBasedResolver` when
  the field is non-zero.
- `manager.go` (or `manager_state.go` / `manager_audit_wireup_test.go`
  area — wherever fits the existing startup-warning style) — emit a
  warning when `EnableTwoPhaseHandoff` is true and the configured
  `HeartbeatTTL × 5` is less than the resolver's default reconcile.
- Tests in `internal/durable/`, `consumer/`, and one Manager test for
  the warning.

Files explicitly out of scope:

- Anything in `internal/assignment/handoff/` (the parked preparePhase
  bug is a separate fix).
- Anything in `internal/assignment/calculator_audit.go` — the audit
  itself is unchanged.
- `source/`, `manager_assignment.go` watcher loops, election code.
- Heartbeat publisher.

## Detailed requirements

### R1. `consumer.ResolverConfig.ReconcileInterval`

Add to `consumer/resolver_config.go:ResolverConfig`:

```go
// ReconcileInterval is the cadence at which the auto-created claim-based
// resolver re-lists the handoff bucket and reconciles its cache against
// KV. This is the recovery mechanism for silent watcher stalls (the
// nats.go KV watcher does NOT surface NATS server restarts as Updates()
// channel close; only an explicit Stop / connection close / subscription
// teardown does). After such a stall the cache stays stale for at most
// one reconcile period.
//
// Choose a value shorter than parti.Config.ExtendedApplyGracePeriod
// (default 5 × HeartbeatTTL) so the leader-side audit cannot escalate
// audit_repair while the worker's resolver cache is still stale. With
// the default HeartbeatTTL=15s, this gives an audit grace of 75s and
// the default 30s reconcile is comfortably inside it. If you have tuned
// HeartbeatTTL below ~6s, set ReconcileInterval to HeartbeatTTL or
// HeartbeatTTL/2.
//
// Zero uses the default (30s). Negative values are rejected at startup.
// Ignored when a custom OwnershipResolver is provided.
ReconcileInterval time.Duration `default:"30s" validate:"gte=0"`
```

Default tag `30s`, validate `gte=0`. Match the project's `fuda`
defaulting convention used by the surrounding fields.

### R2. Internal mirror + plumbing

Mirror the same field on `internal/durable/config.go:ResolverConfig`
with the same default and validation.

Update `consumer/dynamic.go:toSubscriptionResolverConfig` to copy the
new field across:

```go
return durable.ResolverConfig{
    OwnershipResolver:   cfg.OwnershipResolver,
    HandoffBucketName:   cfg.HandoffBucketName,
    HandoffClaimsPrefix: cfg.HandoffClaimsPrefix,
    BatchWindow:         cfg.BatchWindow,
    BatchMaxItems:       cfg.BatchMaxItems,
    ReconcileInterval:   cfg.ReconcileInterval,
}
```

### R3. Pass the option to the resolver

In `internal/durable/worker_consumer.go:ensureGateResolver` at the
`NewClaimBasedResolver` call site:

```go
resolver := durable.NewClaimBasedResolver(
    kv,
    wc.config.Resolver.HandoffClaimsPrefix,
    wc.logger,
    durable.WithReconcileInterval(wc.config.Resolver.ReconcileInterval),
)
```

When `ReconcileInterval == 0`, `WithReconcileInterval(0)` would
disable polling entirely per the resolver's option semantics. Avoid
that footgun: if `wc.config.Resolver.ReconcileInterval == 0`, pass
the resolver's existing default by NOT calling
`WithReconcileInterval` at all (so the resolver's
`defaultReconcileInterval` constant of 30s applies). Document this
explicitly in a comment at the call site.

Alternatively, normalise zero → 30 s at the config-defaulting layer
via the `default:"30s"` tag, then unconditionally pass the value.
**Prefer this approach** — it keeps the resolver-package contract
("0 disables polling") intact for direct callers and makes the
consumer-facing default explicit.

### R4. Manager startup warning

At a suitable Manager startup point (after `m.cfg.SetDefaults()` and
before `m.cfg.Validate()` returns, or immediately after the
calculator's config is finalised — pick whichever fits the existing
style), emit a single warning when:

- `m.cfg.EnableTwoPhaseHandoff == true`, AND
- `5 × m.cfg.HeartbeatTTL < 30 * time.Second`.

The threshold mirrors the resolver's default reconcile cadence.

Suggested warning text:

```
"audit grace (5 × HeartbeatTTL) is shorter than the default claim
resolver reconcile interval (30s); after a silent watcher stall the
leader can escalate audit_repair before the worker's resolver cache
has recovered. Set consumer.ResolverConfig.ReconcileInterval to at
most HeartbeatTTL to close this gap."
```

Use the existing `m.logger` at level WARN. Fire exactly once at
startup; do not repeat on every audit cycle. Place the call so it
runs on EVERY manager start, not gated behind a flag that might
disable it in tests.

### R5. Validation

Project-wide validation gates:

1. `make lint` — clean.
2. `go test ./... -race -count=1 -short -timeout 300s` — green.
3. `go vet ./...` — clean.
4. `go build ./...` — clean.

## Test coverage

### Plumbing tests

Add in `internal/durable/claim_resolver_test.go` (or a new
`claim_resolver_config_test.go` alongside):

1. **`TestWorkerConsumer_PassesReconcileIntervalToResolver`** — build
   a `WorkerConsumer` with `ResolverConfig{ReconcileInterval: 1s}`,
   start it, assert the resolver's internal `reconcileInterval`
   field (test seam in same package) equals 1 s. (Existing tests
   already exercise the resolver's internals in-package.)

2. **`TestWorkerConsumer_DefaultReconcileIntervalApplies`** — leave
   the field zero. After `WorkerConsumerConfig.SetDefaults()` /
   `fuda.SetDefaults`, assert the resolver was started with the 30 s
   default (or that the resolver's internal field shows 30 s).

### Consumer-layer test

In `consumer/dynamic_test.go` or a new
`consumer/resolver_config_test.go`:

3. **`TestResolverConfig_DefaultsReconcileInterval`** — instantiate
   `ResolverConfig{}` and run defaults; assert `ReconcileInterval ==
   30 * time.Second`.

4. **`TestResolverConfig_RejectsNegativeReconcileInterval`** — set
   `ReconcileInterval = -1`; assert validation returns an error.

5. **`TestToSubscriptionResolverConfig_CopiesReconcileInterval`** —
   build a `consumer.ResolverConfig` with a non-default value, run
   `toSubscriptionResolverConfig`, assert the durable equivalent
   carries it through.

### Manager warning test

In `manager_audit_wireup_test.go` or a new
`manager_resolver_reconcile_warning_test.go`:

6. **`TestManager_WarnsOnShortHeartbeatTTLWithTwoPhase`** —
   construct a config with `HeartbeatTTL = 2 * time.Second`,
   `EnableTwoPhaseHandoff = true`. Start the manager with a captured
   logger spy. Assert the resolver-reconcile warning fires exactly
   once with the expected substring.

7. **`TestManager_NoWarnWhenHeartbeatTTLLongEnough`** — same with
   `HeartbeatTTL = 15 * time.Second`. Assert no resolver-reconcile
   warning.

8. **`TestManager_NoWarnWhenTwoPhaseDisabled`** — `HeartbeatTTL = 2 *
   time.Second`, `EnableTwoPhaseHandoff = false`. Assert no warning
   (audit doesn't run, so the grace mismatch is irrelevant).

Look at the existing logger-spy pattern in
`manager_audit_wireup_test.go` to match the style.

## Non-goals

- Do NOT change the resolver's default reconcile interval from 30 s.
- Do NOT add a hard validation error for the grace/reconcile
  inversion — a warning is correct; the operator may have reasons
  to accept the gap.
- Do NOT plumb `ReconcileInterval` from `parti.Config` directly. The
  resolver is a consumer-layer concern; the Manager only emits a
  warning suggesting tuning at the consumer config layer.
- Do NOT touch the `twophase.preparePhase` short-circuit — separate
  follow-up.

## Risk / rollback

- API addition only; no breaking changes. New field is additive on
  both `ResolverConfig` structs.
- Existing callers continue to compile and get the default 30 s.
- Rollback: revert the commit. No data plane state.
