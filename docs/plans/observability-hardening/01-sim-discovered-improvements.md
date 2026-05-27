# Observability Hardening — Sim-Discovered Improvements

Status: discovered during the simulation-coverage-audit branch (2026-05-27).
Source: 12 commits of sim hardening + 3-round post-impl-review loop. The sim
touched only `test/simulation/`; this document catalogs production-side
improvement candidates that the sim work surfaced. Validated by a fresh
codex review against current production source.

This is a planning document for a **next round** — none of the candidates
are required to merge the sim coverage branch.

---

## Verdict table

| # | Candidate | Verified | Severity | Size | Sim-side cleanup if landed |
|---|---|---|---|---|---|
| 1 | `UpdateWorkerConsumer(workerID, nil)` is dual-purpose | yes | P1 | M/L | Restores sharp negative gate in `ClaimLossOrderingOracle` |
| 2 | Degraded-reason naming inconsistency | yes | P1 | M | `DegradedReasonOracle` narrows from substring to exact reason families |
| 3 | `OnAssignmentChanged` does not fire on post-Stop revoke | yes | P1 | M | Removes `revocationObservingUpdater` shim |
| 4 | `WithLeadershipProbe` overrides `WithReconcileInterval` | yes, documented | P2 | M | Sim can wire the probe and keep short reconcile cadence for chaos |
| 5 | ~~No `parti.Hooks.OnStateChanged`~~ | **WRONG** — already exists | — | — | Sim should adopt the existing hook (250ms polling can be dropped) |
| 6 | (sim infra, skipped) | — | — | — | — |
| 7 | No exported reconcile-interval getter on `*source.NatsKV` | yes | P3 | S | Sim reads live cadence instead of mirroring config |
| 8 | Plan-vs-code drift / docs gaps | partially | P2 | S | Future sim plans avoid invalid low-TTL examples |
| A | (adjacent) `reconcileLoop` docs contradict probe-mode behavior | yes | P3 | S | — |
| B | (adjacent) `Hooks` lifecycle contract is muddy | yes | P3 | S | — |
| C | (adjacent) `WorkerIDTTL` doc wording: "greater than" vs `>=` | yes | P3 | S | — |

---

## P1 candidates — observability gaps that mask real regressions

### 1. Reasoned `UpdateWorkerConsumer` / explicit revoke API

**Problem.** The same `WorkerConsumerUpdater.UpdateWorkerConsumer(ctx, workerID, partitions)` call site carries both:
- `claimLostShutdown → revokeWorkerConsumer` with `nil` partitions (`manager_election.go:21-30`, called only after `Manager.Stop` returns per `manager_election.go:56-80`).
- The apply loop's legitimate rebalance-to-empty via `handoff.Apply` (`internal/assignment/handoff/direct.go:28-37`, `internal/assignment/handoff/twophase.go:80-84`, dispatched from `manager_assignment.go:1255-1258`).

The public updater interface (`options.go:121-143`) gives observers no reason field. Result: external code (operators, metrics exporters, test harnesses) cannot distinguish a claim-loss safety revoke from a normal rebalance. This forced the sim's `ClaimLossOrderingOracle` to drop a sharp negative gate across three post-impl-review rounds; the final design is audit-only logging.

**Severity.** P1. Not a correctness bug — the consumer is reconciled to zero subjects in both cases. But the observability gap leaves regressions undetectable from outside.

**Fix sketch (additive, backward-compatible).**
- Add an optional interface implementors can satisfy:
  ```go
  type WorkerConsumerRevoker interface {
      RevokeWorkerConsumer(ctx context.Context, workerID string, reason RevokeReason) error
  }

  type RevokeReason int
  const (
      RevokeReasonClaimLost RevokeReason = iota + 1
      RevokeReasonShutdownDrain
      // ...
  )
  ```
- `Manager.revokeWorkerConsumer` type-asserts the updater for `WorkerConsumerRevoker` and uses it if present, falling back to `UpdateWorkerConsumer(ctx, workerID, nil)`.
- Alternatively (broader scope): add `UpdateWorkerConsumerWithReason(ctx, workerID, partitions, reason)` with reasons `AssignmentApply`, `ClaimLostRevoke`, `ShutdownDrain`. Existing implementors are unaffected; new ones can opt in.

**Backward-compat.** Optional-interface approach is source-compatible. Modifying the existing method signature would break every implementor.

---

### 2. Structured degraded reasons for whole-bucket loss

**Problem.** `recordKVError` (`manager_degraded.go:82-99`, threshold trip at `:137-145`) accepts only an `error` and enters degraded with the opaque reason `"KV error threshold exceeded"` — no bucket or subsystem identity. The epoch-fence path produces structured `bucket-recreated:<bucket>` (`manager_setup.go:570-688`). Operators consuming `OnDegraded` (`types/hooks.go:122-132`) get structured identity from the epoch fence but not from the threshold path. The sim's `DegradedReasonOracle` had to widen its substring matcher to absorb both forms.

Call sites that would benefit from bucket/subsystem context: stable-ID/election (`manager_election.go:113-121, 249-253, 288-292`), assignment watchers (`manager_assignment.go:382-396, 424-433, 624-630`), recovery (`manager_degraded.go:249-253`).

**Severity.** P1. Observability gap that lets materially different failures share one reason. Doesn't change state-machine correctness, but weakens incident triage.

**Fix sketch.**
- Thread a typed source through `recordKVError`:
  ```go
  type kvErrorSource struct {
      Bucket    string  // e.g. "parti-stableid"
      Subsystem string  // e.g. "stableid-renewal", "assignment-watcher", "election"
  }
  func (m *Manager) recordKVError(err error, src kvErrorSource) { ... }
  ```
- Reason format: `bucket-unavailable:<bucket>` when bucket is known; `kv-unavailable:<subsystem>` as fallback.
- Migration: stage the change as additive metric/log labels first, then update the reason string in a documented release.

**Backward-compat.** The reason string is part of the public `OnDegraded` contract; consumers that string-match will break. Mitigate by: (a) keep `"KV error threshold exceeded"` as a prefix and append `: <bucket>`/`: <subsystem>`; (b) document the change loudly in CHANGELOG.

---

### 3. Explicit revoke observation surface

**Problem.** `OnAssignmentChanged` fires only from a successful apply pipeline (`manager_assignment.go:1321-1395`). `claimLostShutdown` (`manager_election.go:48-80`) stops the manager FIRST — which cancels `m.ctx`, tears down the apply loop (`manager.go:716-732, 767-793`) — then revokes the worker consumer. The post-Stop revoke bypasses the apply pipeline, so observers see the worker stop but never see the resulting zero-assignment transition.

The sim had to add `revocationObservingUpdater` (`test/simulation/internal/worker/worker.go:981-999`) wrapping the consumer to emit a `RevocationReport`.

**Severity.** P1. Consumer is still revoked, so not a processing-correctness bug. But anyone treating `OnAssignmentChanged` as the complete assignment-transition stream misses the most important transition (claim loss).

**Fix sketch.** Best-solved together with candidate 1:
- Option A (preferred): the same `WorkerConsumerRevoker` from candidate 1 carries the reason. Observers wrap the updater to capture it.
- Option B: add a dedicated `OnWorkerConsumerRevoked(ctx, workerID, reason RevokeReason)` hook on `parti.Hooks`. `claimLostShutdown` dispatches via a fresh bounded context (since `m.ctx` is already cancelled).
- Avoid retrofitting `OnAssignmentChanged` to fire after `Stop` — it would blur lifecycle semantics that existing users rely on.

**Backward-compat.** Both options are additive.

---

## P2 candidates — API friction

### 4. `WithLeadershipProbe` precedence over `WithReconcileInterval`

**Problem.** Documented behavior at `source/nats_kv.go:64-92`: when `WithLeadershipProbe` is set, the source uses fixed leader/follower intervals (30s/5m, package constants at `:19-30`) and IGNORES `WithReconcileInterval`. Implemented at `nextReconcileInterval` (`source/nats_kv.go:1003-1014`). Phase 2 of the sim could not wire the probe AND keep a short reconcile cadence for chaos tests; the sim shipped without the probe.

**Severity.** P2. Documented behavior, not a hidden bug. But it's real API friction for chaos tests and low-latency deployments.

**Fix sketch.** Additive option:
- `WithLeadershipReconcileIntervals(leader, follower time.Duration)` — explicit override of the two constants. Clearer than overloading `WithReconcileInterval`.
- Or: `WithMinReconcileInterval(d)` that clamps both leader and follower cadences from below.

**Backward-compat.** Additive. Existing users keep current precedence by default.

---

### 8. Docs gaps and validator-vs-doc drift

**Problem.**
- `WorkerIDTTL` validator requires `>= HeartbeatTTL` (struct tag at `config.go:355-364`, `Validate` at `:620-624`). The validation guide at `:591-610` correctly says `>=`. **But the Go doc string at `:355-356` says "greater than HeartbeatTTL"** (strict-greater wording). And `docs/CONFIGURATION.md:93-106` describes `WorkerIDTTL` only with a recommendation, not the hard constraint. Result: the impl plan's `6s` examples (with default `HeartbeatTTL=15s`) failed Start, and the discrepancy isn't visible to readers of the user-facing docs.
- `internal/stableid/claimer.go:362-387`: `renew` maps only `jetstream.ErrKeyExists` and bucket/stream-loss sentinels to `ErrClaimLost`. Plain `kv.Delete` does NOT. `claimer.go:437-449` confirms release-side `Delete` tolerates `ErrKeyExists` without surfacing `ErrClaimLost`. Easy for test-writers to model incorrectly (the sim's `stableid_claim_steal` originally tried delete; corrected to revision-bumping `Put`).

**Severity.** P2 docs + P3 maintainer note.

**Fix sketch.**
- Update `docs/CONFIGURATION.md` and `docs/API_REFERENCE.md` to state the hard validation rules inline near the config table: `WorkerIDTTL >= HeartbeatTTL`, `WorkerIDTTL >= 3*HeartbeatInterval`, `WorkerIDTTL >= 300ms`.
- Reconcile Go doc at `config.go:355-356` from "greater than HeartbeatTTL" → "`>= HeartbeatTTL`" to match the validation tag.
- Add a short comment block at the top of `internal/stableid/claimer.go:renew` documenting which renewal failures map to `ErrClaimLost` (revision mismatch via `Update`, bucket/stream loss) and which do not (release-side `Delete` errors).

**Backward-compat.** Docs and comments only.

---

## P3 — minor

### 7. Live `reconcileInterval` accessor on `*source.NatsKV`

`NatsKV` keeps reconcile and probe-cadence fields private (`source/nats_kv.go:201-215`). Exported API has no `Config()`/`Stats()` accessor. External tooling has to mirror the configured value to compute budgets, and cannot see the effective interval (relevant after candidate 4 lands).

**Fix sketch.** Add a non-mutating accessor:
```go
type NatsKVSnapshot struct {
    ReconcileInterval      time.Duration
    EffectiveInterval      time.Duration  // post-probe-selection
    LeadershipProbeEnabled bool
    Revision               uint64
    Known                  int
}
func (s *NatsKV) Snapshot() NatsKVSnapshot
```

**Backward-compat.** Additive.

### 5. ~~`OnStateChanged` hook~~ — wrong; already exists

`types.Hooks.OnStateChanged(ctx, from, to State) error` already exists at `types/hooks.go:58-68` and is invoked from `transitionState` (`manager_state.go:121-133`). The sim's 250ms polling ticker in Phase 7a was unnecessary work — it should consume the existing hook via `parti.WithHooks` and emit `state` IPC frames on transition instead of on tick.

**Action.** No production change. Sim follow-up: replace the polling ticker in `runWorker` (`test/simulation/cmd/simulation/main.go:1422-1436`) with an `OnStateChanged` hook that emits the IPC frame. Estimated effort: < 1 hour. Bonus: state frames become event-driven rather than rate-limited.

---

## Adjacent issues spotted during codex review

### A. `reconcileLoop` docs vs code (P3 docs)
Comment at `source/nats_kv.go:963-968` says `WithReconcileInterval(0)` disables polling. Code only exits on `reconcileInterval <= 0` when NO leadership probe is wired (`:969-970`); `nextReconcileInterval` ignores it when a probe exists (`:1005-1014`). The option-level docs at `:64-92` are clearer. Reconcile the function-level comment with the option-level wording.

### B. Hooks lifecycle contract is muddy (P3 docs)
`types/hooks.go:11-14` says hooks "may not complete before `Stop()` returns." But hooks dispatch via `m.wg.Go` (`manager.go:963-974`), and `Stop` waits on `m.wg` until its context expires (`:789-807`). The true contract is "asynchronous; `Stop` waits up to the caller's shutdown context." Tighten the doc wording so users don't write hooks that block forever expecting `Stop` to detach them.

### C. `WorkerIDTTL` wording (P3 docs)
Field doc says "greater than HeartbeatTTL" (`config.go:355-356`); validation tag and validation guide say `>= HeartbeatTTL` (`config.go:364, 591-610`). Normalize to `>=`. (Subset of candidate 8.)

---

## Recommended priority order for next round

1. **Candidate 1 — reasoned consumer-update / revoke API.** Removes the largest ambiguity. Sim oracles get a durable claim-loss signal; production gains operator-visible reasons for revoke events. The fix unblocks a sharper sim gate on Stop-before-revoke ordering.
2. **Candidate 2 — structured degraded reasons.** Contained observability improvement with direct operator value during bucket-loss incidents. Pair the migration with CHANGELOG notes since reason strings are public.
3. **Candidate 3 — explicit post-Stop revoke observation.** Best implemented alongside #1 (same reasoned revoke surface). Closes the remaining claim-loss visibility gap.

Candidate 4 (probe + reconcile interval composition) is a clean follow-up after #1–#3 — small enough to bundle with any source-package work.

Candidate 5 is a **sim-side TODO**, not production work — the hook already exists.

Adjacent A/B/C are pure docs fixes that can land as a single small PR independently of any of the above.

---

## What this enables for the sim

After candidates 1 + 3 land, the sim can:
- Drop the `revocationObservingUpdater` shim in `test/simulation/internal/worker/worker.go`.
- Restore `ClaimLossOrderingOracle` to a sharp negative gate (revoke without reason `ClaimLostRevoke` = violation).
- Remove the audit-only `sampler_reports_*` log lines from oracle backfill paths.

After candidate 2 lands, `DegradedReasonOracle` can narrow its substring matcher to exact reason families per bucket, distinguishing whole-bucket loss from recreation cleanly.

After candidate 4 lands, the NATSKV-source scenario can wire `WithLeadershipProbe` for production-cadence parity AND keep its 5s reconcile interval for chaos timing.

After candidate 5 follow-up, Phase 7a's process-mode IPC `state` frames become event-driven, simplifying the worker emitter and dropping the 250ms ticker overhead.
