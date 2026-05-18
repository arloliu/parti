# Review Response — Round 1 (Phase 4)

Source: `tmp/sim-oracle-phase4_review.md`.

## P1 — Missed chaos.go registration sites (accepted)

Round-1 reviewer correctly noted that `chaos.go` has more typed
surfaces than the plan called out:

- `generateEventParams` switch (~chaos.go:177): the existing
  `NetworkDisconnectEvent` arm produces a randomized 5–30s duration.
  Without an entry for the leader variant, scheduled events arrive with
  empty params and the dispatcher's 10s fallback doesn't mirror behavior.
- `String()` switch (~chaos.go:307): without an entry, logs use the
  "Unknown Event" path.

Also confirmed: there is **no typed YAML parser** — chaos events are stored
as plain `[]string` and cast to `ChaosEvent` (`config.go:176`, `chaos.go:82`).

**Fix applied:** rewrote §3 with an explicit registration-sites table
covering all four `chaos.go` surfaces + three `main.go` switches.
Implementation order updated to reflect the four chaos.go edits.

## P1 — Gate-on CI validation requires an 8-minute dry run (accepted)

Round-1 reviewer's point: a 30s authoring run isn't strong evidence for
8m CI behavior with gate-on. The Processing Gate adds NAK retries under
handoff; cumulative effect over 8 minutes is materially different from 30s.

**Fix applied:** verification gate 4 is now an explicit 8-minute local
dry run matching the exact CI invocation
(`--config chaos_comprehensive.yaml --duration 8m --cooldown 90s
--stop-on-failure=false`). Treated as a hard merge prerequisite, not a
post-merge rollout risk.

## P1 — New CI jobs need deterministic proof (accepted)

Round-1 reviewer noted: a 30s focused config without cooldown is too
probabilistic, and `chaos_network_disconnect.yaml` could "pass" without
either event firing at all.

**Fix applied:**
- Added a unit test (`TestNetworkDisconnectLeaderEvent_ParamsAndStringer`)
  that exercises the new event's params + stringer deterministically,
  bypassing the chaos scheduler.
- Extended `chaos_network_disconnect.yaml` to 90s + 15s cooldown +
  explicit `gap_aging: 60s`. Over 90s with 5–10s interval, ~10 events
  fire; with two configured names, leader-disconnect has high
  probability of firing at least once.

## P2 — RoundRobin job needs explicit drain/gap-aging contract (accepted)

Round-1 reviewer noted: RoundRobin reassignment churn is higher than
WeightedConsistentHash; can't blindly copy `gap_aging: 240s` or use the
45s default.

**Fix applied:** §5 now specifies explicit `duration: 90s`,
`gap_aging: 60s`, `interval: "10s-15s"`, and `--cooldown 20s` from the CI
job. Plan also explains why each value is chosen.

## P2 — Process-mode behavior for new event (accepted)

Round-1 reviewer noted: process-mode `leader_failure` doesn't do real
leader lookup (it kills the first running worker), and the plan didn't
specify what process-mode should do for the new variant.

**Fix applied:** new "Process-mode behavior" subsection in §3 says the
new event is **all-in-one only**; process-mode logs "process mode does
not support leader-disconnect; skipping" and returns. All Phase 4 CI
configs use all-in-one, so the skip is invisible in CI.
