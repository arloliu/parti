# `internal/assignment` — Priority Fix Plan (post-IOPS-investigation)

**Context shift:** the IOPS investigation in `.claude/worktrees/iops-investigation/docs/plans/iops-investigation/findings.md` (HEAD `0373c19`, 2026-05-17) **falsified** my earlier CC-IOPS hypothesis stack. Tier 1 evidence:

- M1.1 vs M1.2 (`EnableTwoPhaseHandoff=false` vs `true`): Δβ₁ = +0.003 ± 0.013 IOPS/partition → **no effect**
- M1.2 vs M1.3 (`SweepInterval=30s` vs `5min`): Δβ₁ = −0.001 ± 0.013 → **no effect**
- M1.2 vs M1.9 (file KV vs memory KV): Δ ≈ 1-2% → **no effect**
- M1.2 vs M1.7 (file data stream vs memory data stream): −90% at N=1000, −72% at N=3000 → **dominant source**

Cost decomposition at N=1000: **88% data stream**, 10% parti coordination, 2% NATS overhead.

This invalidates my earlier `05-issue-catalog.md` CC-IOPS-1..6 ranking. This document re-ranks all 8 correctness ISSUEs against the IOPS-investigation evidence, marks over-engineering items explicitly, and adds the architecturally important finding the investigation surfaced.

---

## Re-ranked findings

### Tier P0 — Architectural design INVESTIGATION (not a priority fix)

**FINDING-A (NEW, from IOPS investigation §5 — DOWNGRADED per Codex)** — **Collapse N pull-consumers into a single subject-filtered consumer — but ONLY for `consumer.Dynamic`.**

- **Codex correction:** I originally ranked this "highest user-visible IOPS leverage." Codex caught that the 4 consumer types aren't symmetric:
  - `Queue` (`consumer/queue.go:21-27`, `:333-352`) — already uses ONE shared durable with wildcard filter. No collapse needed.
  - `Static` (`consumer/static.go:14-16`) — one fixed partition consumer; not N-per-partition.
  - `Broadcast` (`durable/broadcast_consumer.go:139-167`) — intentionally one durable per instance, ignores partition assignments.
  - **`Dynamic` (`durable/worker_consumer.go:128-155`, `:376-471`) — the ONLY consumer type with the N-per-partition geometry the IOPS investigation identifies.**

- **What this means:** the "collapse" idea applies narrowly to `Dynamic`. The collapse design must preserve:
  - Dynamic ownership gating (`partition X owned by worker Y` per claim state)
  - Drain-on-remove semantics (partition leaves a worker → consumer stops processing it)
  - Per-partition recovery / checkpoint state
  - Assignment updates (the whole reason two-phase handoff exists)

- **Status:** **OPEN ARCHITECTURAL QUESTION REQUIRING USER BUY-IN.** Not a priority fix; not part of `internal/assignment` work; needs:
  1. Confirmation that user's workload is on `consumer.Dynamic` (vs `Queue`/`Static`/`Broadcast`)
  2. Stronger evidence than the M1.6 cell that was NOT run (`findings.md:223-236`)
  3. A separate design plan (`docs/plans/dynamic-consumer-collapse/`)
- **Pre-condition:** the user should first try M1.6 (`consumer.Queue` ablation) since `Queue` already has the single-consumer geometry — that ablation would attribute the data-stream cost between (a) per-consumer state and (b) `Dynamic`'s specific per-partition durable churn.

### Tier P1 — Correctness, ship-before-next-release

**ISSUE-003 (S2/C-Med)** — `Stop` racing in-flight `rebalance` lets a stale commit land on KV after `Stop` has begun.
- **Impact:** post-step-down commit can become authoritative on the next leader if its first publish is delayed.
- **Effort:** ~10 LOC (`ctxFromStopCh` helper) + 1 unit test + 1 chaos test.
- **Fix:** thread a stop-aware ctx through `rebalance` so `publisher.Publish` cancels on Stop.
- **Rationale for P1:** S2 correctness; same helper also fixes ISSUE-002 (Codex confirmed bundle is appropriate as "partition-triggered rebalance lifecycle fixes").

**ISSUE-007 (S2/C-Med — UPGRADED per Codex)** — Audit grace windows wall-clock based; clock skew across leader handoff suppresses or prematurely triggers real audit-repair escalation.
- **Codex correction:** I originally ranked this S3 calling audit "extra pressure." Codex caught that `maybeEscalateAudit` (`calculator_audit.go:157-208`) gates *real recovery paths* (audit_repair rebalance) for behind workers, not just retry-pressure metrics. Under unfavourable clock skew the recovery path is either suppressed (worker stays behind) or prematurely triggered (cluster ping-pongs). Same severity tier as ISSUE-003.
- **Impact:** depends on operational clock discipline. With strict NTP it's noise; with container drift / suspend-resume it's a real correctness bug.
- **Effort:** ~20 LOC + 1 test.
- **Fix:** introduce `observedAtMonotonic` field on `Publisher.lastCommit`, set at `BootstrapLastCommit` and after successful CAS; audit uses it instead of `commit.PublishedAt`.

### Tier P2 — Correctness, opportunistic

**ISSUE-002 (S3/C-High)** — `monitorPartitions` ctx detach delays `Stop` up to 30s.
- **Impact:** graceful shutdown blocks per leader; visible during pod rollouts.
- **Effort:** ~5 LOC if ISSUE-003 fix lands first (reuse `ctxFromStopCh` helper).
- **Fix:** wrap the existing 30s `context.WithTimeout` with a stop-cancellable parent.
- **Rationale for P2:** S3 — annoying but not data-corrupting. Bundle with ISSUE-003 / ISSUE-005.

**ISSUE-005 (S3/C-High)** — `monitorPartitions` bypasses `IsInRecoveryGrace`.
- **Impact:** single premature rebalance after a degraded-mode recovery coinciding with a partition source event.
- **Effort:** ~3 LOC (add the same check that `checkForChanges:700` does).
- **Rationale for P2:** trivial fix; bundle with ISSUE-002/003 PR (Codex reframe: "partition-triggered rebalance lifecycle fixes").

**ISSUE-001 (S2/C-High — DOWNGRADED per Codex)** — Publisher CAS-loss leaves leader stuck (`assignment_publisher.go:374-386`).
- **Codex correction:** I originally ranked this P1, claiming production manifest is `ErrLeadershipLostPreAlias` loop. Codex caught the logical hole: if production wires `LeaderCheck` (`manager_assignment.go:115-122`) and the pre-alias fence (`assignment_publisher.go:318` → `:503-509`) detects a real leadership mismatch via live KV (`nats_election.go:345-369`), then refreshing `lastCommitRev` does NOT fix the actual problem — the leader has actually lost leadership and should give up gracefully. The "stuck CAS-loop" symptom only manifests when a *valid leader* has a stale `lastCommitRev` while still holding the live lease.
- **What this means:** without a concrete repro of "valid leader, stale lastCommitRev, no concurrent winner" the bug is theoretical. The window exists (CAS lost to a former-leader-now-dead's committed write that survives) but is narrow.
- **Demoted to P2 hardening** pending repro evidence. If the repro materialises, promote back to P1.
- **Effort:** ~50 LOC + 1 unit test + 1 chaos test.
- **Fix:** on `ErrCommitCASFailed`, re-read `_commit` revision (inline or via `BootstrapLastCommit`) and update `p.lastCommitRev`. **Defensive, not high-priority** until repro.

### Tier P3 — Cosmetic / docs / micro-perf (bundle in housekeeping PR)

**ISSUE-006 (S4/C-High)** — Emergency-bypass comment-vs-code drift (`calculator.go:691-694`).
- **Fix:** correct the comment to reflect the actual deferral behaviour.
- **Effort:** ~2 LOC. Pure docs.

**ISSUE-008 (S4/C-High)** — Cold-start serial O(K) Gets in `DiscoverHighestVersion`.
- **Impact:** ~5s added to leader startup for K=1000 legacy workers.
- **Fix options:** (a) skip per-key Gets when `_commit` exists (we already seed from it); (b) parallelise via errgroup if true cold start.
- **Effort:** ~15 LOC + 1 benchmark.
- **Rationale for P3:** only matters at startup; production clusters rarely have K=1000 legacy aliases.

### Tier P4 — DESIGN DISCUSSION REQUIRED (do NOT bundle)

**ISSUE-004 (S3/C-High but DANGEROUS to "fix" naively)** — `publisher.CleanupAllAssignments` has no production caller; docstring is misleading.
- **Impact:** dead method + misleading docstring. NOT actually a correctness bug today.
- **Advisor catch:** the docstring's "graceful Calculator shutdown" probably means "entire-cluster shutdown" (admin tool / test teardown), not "leader step-down" — `CleanupAllAssignments` sweeps EVERY non-protocol key including currently-active worker aliases.
- **Recommendation:**
  - **Immediate:** correct the docstring to clarify the intended use case (admin tooling / cluster teardown). ~5 LOC.
  - **Discussion:** is a leader-step-down-safe variant needed? If yes, requires a new method (`CleanupInactiveAssignments(activeWorkers []string)`) — separate design pass.
- **Effort (docstring fix only):** ~5 LOC. Discussion is open-ended.

---

## NOT A PRIORITY IOPS TARGET — but surviving non-IOPS concerns flagged

> **Codex correction:** my original framing said "falsified." That's too strong — the investigation falsified each as the dominant IOPS slope source, not as a correctness/health concern. The reframing matters: a future author shouldn't read "falsified" and treat these as safe to refactor away.

The pattern: each item below is not a priority IOPS target, **but each one has a surviving non-IOPS concern worth preserving in any future change.**

### CC-IOPS-1 — `twophase.maybeSweepClaims` background sweep
- **Investigation evidence:** M1.1 (two-phase off) vs M1.2 (on) = Δβ₁ +0.003 IOPS/partition; M1.3 (sweep=5min) shows zero slope effect (`findings.md:158-164`). Confirmed: not the steady-state IOPS slope source.
- **What I got wrong:** I claimed "sweep is read-only." It isn't. The expired-non-stable branch (`handoff/twophase.go:462-481`) writes via `updateClaim → PutIfEpoch` (`twophase.go:160-166`; `kv_store.go:116-121`). The sweep has a **write-side recovery role** (stuck-handoff reset) that any future "let's delete this loop" thinking must preserve.
- **Verdict:** **Not a priority IOPS target.** Preserve the stuck-handoff reset semantics in any future change.

### CC-IOPS-2 — `commit_gc.RunOnce` payload sweep
- **Investigation evidence:** not directly tested. The dominant cost is measurably data-stream (`findings.md:91-99`).
- **What I got wrong:** calling it "falsified" — it was simply not tested. The default 5-min interval makes it implausible as the IOPS slope source, but the per-pass cost is O(P) Gets (`commit_gc.go:248-267`).
- **Verdict:** **Not a priority IOPS target.** Defer optimisation until measurement evidence; document the O(P) per-pass behaviour for any future operator runbook.

### CC-IOPS-3 — `Calculator.auditApplied` per-tick heartbeat scan
- **Investigation evidence:** H3 floor of ~0.9 ops/s matches measurement across all parti cells (`findings.md:125-131`); audit's W Gets per 15s contributes ~0.4 ops/s — within the noise floor.
- **Verdict:** **Not a priority IOPS target.** Audit correctness is independent and must remain (see ISSUE-007 above for its actual scheduling-correctness concern).

### CC-IOPS-4 — `assignment_publisher.classifyLegacyWorkers` per-Publish O(W) Gets
- **Investigation evidence:** Publish is event-driven, not steady-state. M1.1/M1.2 baseline identical regardless of Publish frequency.
- **What I got wrong:** treating it as ruled out by steady-state slope evidence. The investigation does NOT rule out rollout/churn cost — rapid worker join/leave during a deploy can drive Publishes back-to-back, each doing K Gets. Not measured.
- **Verdict:** **Not a steady-state IOPS target.** Could matter during chaotic deploys — measure first if a concern surfaces.

### CC-IOPS-5 — `updateClaim` double-Get pattern
- **Investigation evidence:** M1 evidence rules it out as the observed steady-state slope.
- **What I got wrong:** painting it as "not worth fixing." The double-read is real (`twophase.go:138-143` → `kv_store.go:89-97`) and any future contention spike (lots of concurrent Applies) doubles read load. **Any optimisation MUST preserve epoch/revision CAS semantics** (`twophase.go:160-166`; `kv_store.go:89-121`) — a sloppy "just remove the second Get" change breaks correctness.
- **Verdict:** **Not a priority IOPS target.** Optional code-quality cleanup with strict CAS-preservation contract.

### CC-IOPS-6 — Removed-partition / dead-owner claim accumulation
- **Investigation evidence:** sweep cost itself is not the IOPS source (M1.3 negative).
- **What I got wrong:** the investigation does NOT falsify unbounded growth as a long-running cluster-health concern. Stable claims are never removed (`kv_store.go:13-30`; `twophase.go:458-460`). For multi-year clusters with partition churn, KV bucket size and `stream info` latency could degrade independent of IOPS.
- **Verdict:** **Not a priority IOPS target.** Surviving long-running-cluster-health concern — measure claim-store growth in a long-running test before declaring this fully closed.

---

## Recommended PR sequencing (revised per Codex)

Concrete suggestion for how to ship the P1/P2/P3 work:

### PR-1 — Partition-triggered rebalance lifecycle fixes (ISSUE-002 + ISSUE-003 + ISSUE-005)
- Bundle, framed as "partition-triggered rebalance lifecycle fixes" per Codex (not narrowly "Stop should fence work" — that framing under-sells ISSUE-005).
- Add `ctxFromStopCh` helper (small new utility).
- Thread it through `monitorPartitions` (ISSUE-002) and `rebalance` (ISSUE-003).
- Add `IsInRecoveryGrace` check in `monitorPartitions` (ISSUE-005).
- ~30 LOC + ~250 LOC tests (3 tests).
- **Was PR-2 in original sequencing — promoted because ISSUE-001 was downgraded.**

### PR-2 — ISSUE-007 (audit clock-skew, upgraded to S2)
- Standalone; adds `observedAtMonotonic` field to `lastCommit`.
- ~20 LOC + 1 test.
- **Was PR-3 — promoted because audit-repair scheduling correctness was under-ranked.**

### PR-3 — Housekeeping (ISSUE-006 + ISSUE-008 + ISSUE-004 docstring)
- Docs + micro-perf.
- ~25 LOC + 1 benchmark.

### PR-4 (defensive, requires repro evidence first) — ISSUE-001 Publisher CAS-loss recovery
- Standalone; affects only `assignment_publisher.go`.
- **Gating:** construct a repro showing "valid leader passes pre-alias fence, fails CAS, stays stuck" before merging. Without repro the symptom is theoretical and the fix may mask other bugs.
- ~50 LOC + ~150 LOC tests.

### Separate investigation (not a PR) — FINDING-A (consumer collapse for `Dynamic`)
- Scope as `docs/plans/dynamic-consumer-collapse/` — narrow to `consumer.Dynamic` only.
- Pre-requisite #1: run M1.6 (`consumer.Queue` ablation) to attribute the data-stream cost more precisely.
- Pre-requisite #2: confirm user workload is `Dynamic`.
- Then design/implement; multi-week effort if pursued.

---

## Skip list (do NOT do, and why)

| Item | Skip because |
|---|---|
| Raise `SweepInterval` to 5min | Tier 1 (M1.3) measured: zero IOPS benefit, adds handoff latency. |
| Disable `EnableTwoPhaseHandoff` | Tier 1 (M1.1) measured: zero IOPS benefit, reintroduces v2.2.x bug. |
| Move parti KV buckets to memory storage | M1.9 measured: 1-2% effect (noise). Loses coordination durability for no real win. |
| Optimise `maybeSweepClaims` for IOPS | H1 falsified; sweep is not the cost source. |
| Optimise GC payload sweep for IOPS | Negligible vs data stream cost. |
| Optimise `classifyLegacyWorkers` for IOPS | Event-driven, not steady-state. |
| Optimise `updateClaim` double-Get for IOPS | Not the slope source per Tier 1. |
| Add `Delete` API to ClaimStore for IOPS | The cost driver isn't claim count, it's data-stream consumer state. |

---

## What the user should actually do for IOPS (per investigation §5)

| Rank | Action | Measured effect |
|---|---|---|
| 1 | **Set user data stream `Storage = memory`** | −90% at N=1000, −72% at N=3000 |
| 2 | Tune server-side: `MaxAckPending`, `AckWait`, JS snapshot interval | Variable; not measured in this campaign |
| 3 | **Long term:** collapse N pull-consumers into single subject-filtered consumer (FINDING-A above) | Architectural; expected to eliminate the dominant per-consumer state cost |

The 1st action is operator-side (NATS config). The 3rd action is a parti redesign. Nothing in `internal/assignment` is the right surface for IOPS optimisation.

---

## Summary (revised per Codex)

- **PR-1 (ISSUE-002+003+005 bundle, ~30 LOC):** partition-triggered rebalance lifecycle fixes. Concrete S2/S3 wins from one `ctxFromStopCh` helper.
- **PR-2 (ISSUE-007, ~20 LOC):** audit clock-skew → use monotonic observed-at. Promoted to S2 because audit-repair gates real recovery, not just metrics.
- **PR-3 (housekeeping, ~25 LOC):** ISSUE-006 comment fix + ISSUE-008 cold-start opt + ISSUE-004 docstring correction.
- **PR-4 (DEFENSIVE, gated on repro):** ISSUE-001 CAS-loss recovery. Downgraded to P2 because production's pre-alias fence makes the symptom theoretical without an explicit repro.
- **6 IOPS items (CC-IOPS-1..6):** not priority targets for IOPS reduction; each has surviving non-IOPS concerns flagged in the rewritten §"Not a priority IOPS target" section (preserve write-side semantics, CAS preservation, long-running-cluster health).
- **1 architectural investigation (FINDING-A):** narrowed to `consumer.Dynamic` only; gated on user buy-in + an M1.6 ablation that wasn't run; not part of `internal/assignment` work.

Total in-scope correctness effort: **~75 LOC + 5 tests + 1 benchmark + 1 docstring fix** (excluding PR-4 which is gated on repro). Shippable in a focused few days; PR-4 and FINDING-A are research-gated, not coding-gated.
