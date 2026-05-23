# Parti Agent Configuration

This is the authoritative entrypoint for coding agents working in this
repository. Claude Code imports this file from [`CLAUDE.md`](CLAUDE.md); other
agents should read `AGENTS.md` directly.

Parti (`github.com/arloliu/parti/v2`) is a Go library for dynamically
partitioning work across worker instances using NATS JetStream. Detailed project
structure, coding rules, testing rules, documentation standards, workflow, and
review discipline live under [`.agents/rules/`](.agents/rules/).

## Detailed Rules

Read [`.agents/rules/AGENTS.md`](.agents/rules/AGENTS.md) first. It maps task
triggers to the rule files that apply.

Always follow [`.agents/rules/000-agent-contract.md`](.agents/rules/000-agent-contract.md).
It includes the explicit rule: do not guess when source evidence, tests,
benchmarks, docs, or grep can answer.

## Skills

Skills are invocable agent capabilities in [`.agents/skills/`](.agents/skills/).

Available skills:

- `/go-api-review [package]` — Review exported API and README for DX, discoverability, and clarity. Does not read internal source.
- `/qa-review [package]` — Review for correctness, fault tolerance, error propagation, and concurrency safety from a user perspective.
- `/doc-sync [scope]` — Audit and fix `docs/` files and Godoc to match the current API: corrects stale signatures, removes phantom symbols, adds missing entries.
- `/plan-review <plan-path> <short-name>` — Full architectural review of a design plan. Writes a versioned report under `tmp/`. Use after material plan rewrites.
- `/final-plan-review <plan-path>` — Precision pass / pre-implementation sanity check on an architecturally-settled plan. Catches stale text, ambiguous pseudocode, numbering drift — does not redesign.
- `/post-impl-review <phase> <plan-path> <vN>` — Review delivered code against a spec; runs lint/build/test validation. Designed for iterative fix-review loops until merge-clean. For lightweight passes without spec-compliance audit, use `/codex:review` or `/codex:adversarial-review` directly instead.

All skills scope to Parti's public packages by default; specify a subset when
needed (for example, `consumer/` or `docs/CONSUMERS.md`).

The three external-reviewer skills (`plan-review`, `final-plan-review`,
`post-impl-review`) dispatch an outside reviewer through the local skill
workflow, with Copilot `gpt-5.5` as a fallback. Effort defaults vary by task:
`plan-review` and `post-impl-review` (v1/v2) at `xhigh`;
`final-plan-review` and `post-impl-review` v3+ at `high`. Each invocation costs
real tokens and about 2–8 minutes wall time. Do not dispatch speculatively.

## Pre-PR gate

For any PR that touches `manager/`, `source/`, `stableid/`,
`recovery/`, `internal/assignment/`, or `internal/durable/`, run
`make pre-pr` locally before opening the PR. The target chains lint,
`make test` (unit tests with `-race`), and `make test-integration`
(live-NATS scenarios with `-race`). The integration suite catches
contract regressions and concurrency races that the unit suite cannot
reproduce — empirically, both bugs surfaced by self-healing batch 1
slipped past 3 rounds of codex review and were caught only by
`make test-integration -race` under CI load.

## Cross-feature contracts (do not regress)

These contracts live on `main` and any failure-classification or
error-routing change MUST preserve them. Each has a regression test
already in tree; run them when changing how an error is wrapped,
classified, or routed:

1. **Whole-bucket-missing → every worker enters `StateDegraded` within
   a bounded window.** Pinned by `TestManager_LiveNATSBucketLoss` and
   `TestManager_LiveNATSBucketLoss_OnDegradedHook` under
   `test/integration/manager/`. The mechanism: bucket-missing errors
   from stableid / heartbeat / election / assignment-watcher flow
   through `m.recordKVError` → accumulate against `KVErrorThreshold` →
   trip `m.enterDegraded`. A new classifier that routes the error
   elsewhere (e.g., to a self-stop path) regresses this contract.
2. **Peer claim takeover → only that one worker enters claim-lost
   shutdown; others stay healthy.** Pinned by
   `TestStableID_StaleKeyTakeover_Reclaim` under
   `test/integration/stableid/`. The manager's `onClaimerError`
   routes `ErrClaimLost` through `claimLostShutdown` only when the
   wrapped cause is neither connectivity nor degrading-JetStream.
3. **OnDegraded hook fires exactly once per Degraded entry per
   worker.** Pinned by `TestManager_LiveNATSBucketLoss_OnDegradedHook`.

Background: contract (1) was regressed by self-healing's P1.2
(stableid classifier widening) on the integration branch; the fix in
`manager_election.go:onClaimerError` distinguishes "whole-bucket loss"
from "peer takeover" via `natsutil.IsConnectivityError ||
natsutil.IsDegradingJetStreamError` and routes the former through
`recordKVError` instead. Future classifier changes must keep that
distinction.

## Concurrency stress tests for monitor goroutines

When adding a new monitor goroutine on a ticker (e.g.,
`monitorBucketEpochs`, `monitorAssignmentChanges`, source `reconciler`,
F2 envelope retry loops), add a focused concurrency stress test in
`test/integration/<package>/`:

- Start a small real cluster (embedded NATS, 2-3 worker managers)
- Configure the monitor at aggressive cadence (e.g.
  `OperationTimeout=10ms`)
- Drive concurrent KV traffic against the same buckets the monitor
  probes for ~5 seconds
- Assert no race-detector triggers (`go test -race ...`)

The unit tests for the monitor in isolation cannot find races between
the monitor goroutine and production paths sharing nats.go's cached
`*stream` state. Self-healing batch 1's P1.3 hit exactly that — the
unit suite passed, the live-cluster integration suite tripped the
race detector. `test/integration/manager/epoch_monitor_concurrency_test.go`
is the template for this pattern.
