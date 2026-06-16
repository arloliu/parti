# Claim-Write Rate Limit — Fast-Follow

> **Status:** Shipped. Consumer-create rate limiting landed first
> (`consumer.WithConsumerCreateRate`); claim-write rate limiting followed as
> `parti.HandoffConfig.ClaimWritePerSec` / `ClaimWriteBurst` (opt-in, default
> off). One per-worker token-bucket gates every physical `PutIfEpoch` across all
> three verified sites from a single shared budget. Operator guidance lives in
> [`docs/OPERATIONS.md` §Claim-Write Rate Limiting](../../OPERATIONS.md#claim-write-rate-limiting).
> The sections below are retained as the design/enumeration record.

---

## Problem

The consumer-create limiter (`00-plan.md`) gates `CreateOrUpdateConsumer` RPCs.
A related but distinct flood vector is the **claim-write path**: `PutIfEpoch`
calls issued during two-phase handoff and startup hygiene. Under a large fleet
restart or rapid rebalance these calls can generate a burst of KV writes that
stresses the NATS cluster independently of the consumer-create rate.

## Verified sites (3 total)

1. **`internal/assignment/handoff/twophase.go:updateClaim`** — `PutIfEpoch` at
   `:163` / `:188` / `:190`; called from `preparePhase` (`:279`),
   `commitPhase` (`:376`), `stabilizePhase` (`:430`), and reap (`:523`). These
   are already **concurrency-bounded** by `HandoffConfig.PhaseConcurrency`
   (default 20, opt-in via `Config.EnableTwoPhaseHandoff`).

2. **`manager_handoff.go:handoffStartupHygiene`** (repo root, package `parti`)
   — `store.PutIfEpoch` directly, sequential loop over all keys, startup-only,
   **not** `PhaseConcurrency`-bounded. Unbounded on large fleets.

3. **`manager_handoff.go:runHandoffResume`** (repo root, package `parti`) —
   `store.PutIfEpoch` directly, sequential loop over all keys, startup-only,
   **not** `PhaseConcurrency`-bounded. Unbounded on large fleets.

Sites 2 and 3 are the primary concern: startup loops that are currently
unbounded and fire sequentially over all KV keys.

> **As shipped:** all three sites are gated by one per-worker
> `ratelimit.Limiter`, built in `manager_setup.go` from
> `HandoffConfig.ClaimWrite{PerSec,Burst}` and threaded into
> `handoff.Config.ClaimWriteLimiter` (site 1) and `m.claimWriteLimiter`
> (sites 2–3). The earlier path note `manager_handoff.go:172/:226` was relative
> to `internal/assignment/handoff/`; the file is actually at the repo root.

## Deferred rationale

- The two unbounded sites are startup-only and typically run once per pod
  restart. Under normal operations they do not generate sustained write bursts.
- Measurement data is needed to quantify the actual burst magnitude in
  production before designing a fix.
- The `PhaseConcurrency` guard already covers the coordinator phases (site 1).
- Adding a limiter to sites 2 and 3 touches two packages and two lifecycles
  with different concerns from the consumer-create path.

## Proposed approach (for measurement-driven implementation)

1. Instrument sites 2 and 3 with counters (already partially done via
   `RecordHandoffRemovalPending`). Add a wall-clock timer to measure the
   startup loop duration at scale.
2. If the burst magnitude justifies it, add `WithClaimWriteRate(perSec, burst)`
   (analogous to `WithConsumerCreateRate`) to `parti.Config` or
   `HandoffConfig`.
3. The limiter should be per-worker (same scope as consumer-create), threaded
   from config into `manager_handoff.go` `handoffStartupHygiene` and
   `runHandoffResume`.
4. Gate-dependency for site 1: `PhaseConcurrency` already bounds concurrency;
   a rate limiter on top would need to compose correctly with `errgroup.SetLimit`.

## Files that would change

- `internal/assignment/handoff/manager_handoff.go` — sites 2, 3
- `internal/assignment/handoff/twophase.go` — site 1 (if needed)
- `internal/assignment/config.go` or `parti/config.go` — new config field
- Docs: `OPERATIONS.md`, `CONSUMERS.md`
