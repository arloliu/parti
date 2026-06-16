# NATS Thundering-Herd Hardening

Three independent, opt-in controls to reduce parti's contribution to a
NATS thundering herd during fleet-wide reassignment. All three default to
no-op behavior — upgrading the library is observably identical to current
`main` until an operator opts in.

## Status

| Stage | Outcome |
|---|---|
| Plan (6 rounds of architectural review) | All P0/P1 closed at round 6 |
| Implementation | 3 squashed commits on branch `worktree-thundering-herd-hardening` |
| Per-PR post-implementation reviews | PR-1 v2, PR-2 v1, PR-3 v1 — all **merge** |
| Cross-feature contract gate | All 4 pinned tests pass on the final HEAD |
| PR | [arloliu/parti#21](https://github.com/arloliu/parti/pull/21) |

## Surface

| Commit | Knob | Default | Code change |
|---|---|---|---|
| `f03f8eb` | `Config.ApplyStartJitter` (`time.Duration`) | `0` (off) | Random sleep `[0, jitter)` before fresh-version apply takes `applyStoreMu`; retries bypass jitter via `applyAssignmentWithPrevSkipJitter`. Cap at 10s. Body of `applyAssignmentWithPrev` extracted to `applyAssignmentWithPrevCore` (bit-for-bit identical). |
| `a6191fa` | `HandoffConfig.PhaseConcurrency` (`int`) | `0` → 20 | Surfaces hard-coded `g.SetLimit(20)` in 3 sites of `internal/assignment/handoff/twophase.go`. `0` = default 20, `1` = strictly serial, `2..256` = exact bound. Only active when `EnableTwoPhaseHandoff=true`. |
| `2efad05` | `Config.AssignmentWatcherDebounce` (`time.Duration`) + `ManagerMetrics.RecordApplyAttempt(workerID, version)` | `0` (off) for the debounce | Idle-window timer in `runAssignmentWatchSession` coalesces watcher events. Cap at 1s. Prometheus counter `parti_manager_apply_attempts_total{worker_id}` (single label, version discarded for bounded cardinality). Opt-in `TestApplyCoalescing_UnderReElectionBurst` diagnostic for operator window sizing. |
| follow-up | `Config.AssignmentWatcherDebounce` also covers `assignment._commit` watcher updates | `0` (off) | Rapid commit bursts are staged for one idle window; identical-assignment bursts collapse to one apply, while same-or-higher commits that change this worker's partition set early-flush pending to preserve two-phase handoff. Stale lower-version commits are dropped. |

## Related: consumer-create rate limiting

A fourth, independent opt-in control bounds the per-worker `CreateOrUpdateConsumer` RPC rate during large partition assignments and mass recovery events. See [`docs/plans/consumer-create-rate-limit/00-plan.md`](../consumer-create-rate-limit/00-plan.md) and [`consumer.WithConsumerCreateRate`](../../../consumer/options.go).

## Files in this directory

- [`00-plan.md`](00-plan.md) — Full architectural plan (~2300 lines). Background facts, file structure, risk assessment, three PR specs with task-by-task TDD steps, and a "Self-review" section that summarizes the integrated fixes from plan-review rounds 1-5.
- [`review-trail.md`](review-trail.md) — Consolidated record of the round-6 plan-review verdict and all post-implementation reviews (PR-1 v1+v2, PR-2 v1, PR-3 v1).
- [`deviations.md`](deviations.md) — Two implementation deviations: PR-1's negative startup-budget test relocated to unit-level, and PR-3's diagnostic substituting worker-churn for Raft meta-election (single-node embedded NATS has no peer to elect against).

## Operator upgrade flow

1. Deploy this version with default config — no behavior change.
2. Run the diagnostic once:
   ```bash
   PARTI_RUN_HERD_DIAGNOSTIC=1 go test ./test/integration/manager/ \
     -run TestApplyCoalescing_UnderReElectionBurst -v -count=3
   ```
   Record `AGGREGATE max_burst_size` (call it `<N_before>`) and
   `recommended_debounce_window` (call it `<Y>`).
   
   Optionally, also run the rapid-commit diagnostic to measure commit watcher burst behavior:
   ```bash
   PARTI_RUN_HERD_DIAGNOSTIC=1 go test ./test/integration/manager/ \
     -run 'TestApplyCoalescing_UnderRapidCommitBurst' -v -count=3
   ```
3. Tune the three knobs in production config as desired, e.g.:
   ```yaml
   applyStartJitter: 500ms
   handoff:
     phaseConcurrency: 10
   assignmentWatcherDebounce: 150ms  # use <Y> from step 2
   ```
4. Re-run the diagnostic; expect a significant `max_burst_size` reduction.

**Interpreting the metric:** `parti_manager_apply_attempts_total` counts
every apply pipeline entry, including retries. If `max_burst_size` is high
but `parti_worker_consumer_retry_backoff_seconds` histogram and
`parti_worker_consumer_iterator_restarts_total` counter are also elevated,
the herd metric is reflecting retry pressure rather than watcher-burst
leakage. Check retry metrics first.
