# phase4_followup_gap1 Post-Implementation Review (v1)

## Summary

The implementation delivers the gap #1 spec: `consumer.ResolverConfig.ReconcileInterval` is public, defaulted, validated, copied into the durable config, and passed into the auto-created claim resolver. The zero-value footgun is avoided by config defaulting before resolver construction, while direct `NewClaimBasedResolver(..., WithReconcileInterval(0))` callers still keep the documented "0 disables polling" behavior. The Manager warning fires synchronously once during `Start` when two-phase handoff is enabled and `5 × HeartbeatTTL < 30s`, and the warning tells operators to tune `consumer.ResolverConfig.ReconcileInterval`. No P0/P1 findings; recommend merge.

## Spec Compliance

| Requirement | Status | Evidence |
|---|---|---|
| R1 — `consumer.ResolverConfig.ReconcileInterval` | compliant | Public field has Godoc, `default:"30s"`, and `validate:"gte=0"` at `consumer/resolver_config.go:45-63`; Godoc explains `parti.Config.ExtendedApplyGracePeriod`, default `5 × HeartbeatTTL`, and short-HeartbeatTTL tuning at `consumer/resolver_config.go:53-59`. Consumer validation runs defaults then `fuda.Validate` at `consumer/dynamic.go:362-367`, and negative coverage asserts an error containing `ReconcileInterval` at `consumer/resolver_config_test.go:24-34`. |
| R2 — Internal mirror + plumbing | compliant | Internal durable mirror has `ReconcileInterval time.Duration` with `default:"30s" validate:"gte=0"` at `internal/durable/config.go:79-89`. `toSubscriptionResolverConfig` copies it at `consumer/dynamic.go:398-406`. |
| R3 — Resolver wiring | compliant | `NewWorkerConsumer` applies defaults before validation and resolver creation at `internal/durable/worker_consumer.go:80-85` and `:114-117`; `ensureGateResolver` unconditionally passes `WithReconcileInterval(wc.config.Resolver.ReconcileInterval)` at `internal/durable/worker_consumer.go:610-620`. The default test proves zero config becomes 30s before resolver creation and reaches `resolver.reconcileInterval` at `internal/durable/claim_resolver_config_test.go:86-88` and `:102-108`. Direct resolver callers are unaffected because `WithReconcileInterval(0)` still sets the interval to 0 at `internal/durable/claim_resolver.go:36-53`, and `reconcileLoop` returns when `<= 0` at `internal/durable/claim_resolver.go:671-677`. |
| R4 — Manager startup warning | compliant | `Start` calls `m.warnOnShortAuditGrace()` once, immediately after `prepareStart`, at `manager.go:340-356`; `prepareStart` returns `ErrAlreadyStarted` before that call if the manager is already running at `manager_setup.go:16-23`. The helper gates on `EnableTwoPhaseHandoff`, computes `5 * HeartbeatTTL`, compares against a named `resolverReconcileDefault`, and logs WARN with `consumer.ResolverConfig.ReconcileInterval` in the text at `manager_setup.go:202-230`. The local constant has the required duplication comment at `manager_setup.go:202-206`. |

## Findings

None.

## Test Coverage Audit

| Test | Status | Evidence |
|---|---|---|
| 1. Plumbing default (30s) at durable layer | present-and-meaningful | `TestWorkerConsumer_DefaultReconcileIntervalApplies` leaves the field zero, asserts `cfg.SetDefaults()` normalizes it to 30s, starts the resolver path, and asserts the in-package resolver interval is 30s at `internal/durable/claim_resolver_config_test.go:61-108`. |
| 2. Plumbing non-default at durable layer | present-and-meaningful | `TestWorkerConsumer_PassesReconcileIntervalToResolver` configures `1 * time.Second`, calls `ensureGateResolver`, and asserts the concrete resolver interval is 1s at `internal/durable/claim_resolver_config_test.go:14-58`. |
| 3. `consumer.ResolverConfig` defaults to 30s | present-and-meaningful | `TestResolverConfig_DefaultsReconcileInterval` runs `fuda.SetDefaults(&cfg)` and asserts `30*time.Second` at `consumer/resolver_config_test.go:11-18`. |
| 4. `consumer.ResolverConfig` rejects negative | present-and-meaningful | `TestResolverConfig_RejectsNegativeReconcileInterval` sets `-1s`, calls `DynamicConfig.Validate`, and asserts a validation error mentioning `ReconcileInterval` at `consumer/resolver_config_test.go:21-34`. |
| 5. `toSubscriptionResolverConfig` copies the field | present-and-meaningful | `TestToSubscriptionResolverConfig_CopiesReconcileInterval` sets `7s`, converts, and asserts the durable output carries `7s` at `consumer/resolver_config_test.go:37-50`. |
| 6. Manager warns on short HeartbeatTTL with two-phase | present-and-meaningful | `TestManager_WarnsOnShortHeartbeatTTLWithTwoPhase` starts with `2s` TTL and two-phase enabled, then asserts exactly one captured WARN substring at `manager_resolver_reconcile_warning_test.go:123-133`; the spy records WARN messages and counts only WARN entries containing `claim resolver reconcile interval` at `manager_resolver_reconcile_warning_test.go:27-67`. |
| 7. Manager does NOT warn on long HeartbeatTTL | present-and-meaningful | `TestManager_NoWarnWhenHeartbeatTTLLongEnough` starts with `15s` TTL and two-phase enabled, then asserts zero matching WARNs at `manager_resolver_reconcile_warning_test.go:136-144`. |
| 8. Manager does NOT warn when two-phase disabled | present-and-meaningful | `TestManager_NoWarnWhenTwoPhaseDisabled` starts with `2s` TTL and two-phase disabled, then asserts zero matching WARNs at `manager_resolver_reconcile_warning_test.go:147-155`. |

## Operator-visible warning text

Captured from a one-off scenario test that constructed a Manager with `HeartbeatTTL = 2s`, `EnableTwoPhaseHandoff = true`, called `Start`, and captured the spy logger output:

```text
audit grace (5 × HeartbeatTTL) is shorter than the default claim resolver reconcile interval (30s); after a silent watcher stall the leader can escalate audit_repair before the worker's resolver cache has recovered. Set consumer.ResolverConfig.ReconcileInterval to at most HeartbeatTTL to close this gap. [heartbeat_ttl 2s audit_grace 10s resolver_reconcile_default 30s]
```

## Latent-bug audit

Double-fire: `warnOnShortAuditGrace` is called from the `Start` sequence only at `manager.go:340-356`, not from audit loops; repeated concurrent `Start` calls return `ErrAlreadyStarted` before the warning call at `manager_setup.go:16-23`, so it fires at most once per successful Start. Race: the helper reads immutable post-construction config (`m.cfg`) and logs through `m.logger` before startup goroutines are launched at `manager.go:340-356`, so no synchronization hazard was found. Validation timing: consumer-layer negative values fail in `DynamicConfig.Validate` before `NewDynamic` constructs the durable worker at `consumer/dynamic.go:226-228` and `:362-367`; durable-layer validation also runs before `ensureGateResolver` at `internal/durable/worker_consumer.go:80-85`. Default-tag behavior: zero `ReconcileInterval` is normalized by `SetDefaults` before resolver wiring (`internal/durable/config.go:269-272`, `internal/durable/worker_consumer.go:80-85`) and is directly asserted in tests at `consumer/resolver_config_test.go:14-18` and `internal/durable/claim_resolver_config_test.go:86-88`.

## Lint / Build / Test Status

`make lint`:

```text
===== make_lint =====
Checking golangci-lint version...
✓ golangci-lint 2.11.4 is installed
Running linters...
0 issues.
===== make_lint exit=0 =====
```

`go test ./internal/durable/... -race -count=1 -timeout 120s`:

```text
===== durable_race =====
ok  	github.com/arloliu/parti/v2/internal/durable	14.395s
===== durable_race exit=0 =====
```

`go test ./consumer/... -race -count=1 -timeout 120s`:

```text
===== consumer_race =====
ok  	github.com/arloliu/parti/v2/consumer	1.614s
===== consumer_race exit=0 =====
```

`go test ./... -race -count=1 -short -timeout 300s` tail:

```text
ok  	github.com/arloliu/parti/v2/test/integration/manager	9.587s
ok  	github.com/arloliu/parti/v2/test/integration/misc	1.006s
ok  	github.com/arloliu/parti/v2/test/integration/partition	1.011s
ok  	github.com/arloliu/parti/v2/test/integration/stableid	2.543s
?   	github.com/arloliu/parti/v2/test/simulation/cmd/simulation	[no test files]
?   	github.com/arloliu/parti/v2/test/simulation/internal/config	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/coordinator	8.842s
?   	github.com/arloliu/parti/v2/test/simulation/internal/logging	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/metrics	1.010s
?   	github.com/arloliu/parti/v2/test/simulation/internal/natsutil	[no test files]
?   	github.com/arloliu/parti/v2/test/simulation/internal/producer	[no test files]
ok  	github.com/arloliu/parti/v2/test/simulation/internal/worker	1.013s
ok  	github.com/arloliu/parti/v2/test/stress	1.011s
ok  	github.com/arloliu/parti/v2/types	1.008s
===== all_short_race exit=0 =====
```

`go vet ./...`:

```text
===== go_vet =====
===== go_vet exit=0 =====
```

`go build ./...`:

```text
===== go_build =====
===== go_build exit=0 =====
```

## Verdict

merge. Zero P0, zero P1, zero P2 findings.
