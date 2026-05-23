# Self-Healing Findings — Review Trail (Consolidated)

Compact summary of the 9-round review loop that produced
`findings.md`. Each round's verbatim transcript was retired once its findings
were folded into the authoritative findings document — `findings.md` is the
source of truth. This file exists only to record **what was flagged and how it
was resolved**, so a future reader can trace the evolution without re-loading
9 verbatim files.

Reviewer: external (Codex `xhigh`) over rounds 1, 2, 3, 5, 9; final-confirmation
spot-checks in rounds 4, 6, 7, 8.

## Round 1 — substantive review

**Verdict.** Not yet implementation-ready. Several high-impact claims were
stale or overstated.

Findings:

- **P0 — F8** falsely claimed the public `consumer.ResolverConfig.ReconcileInterval = 0`
  disables the claim-resolver reconciler. Stale: that path documents 0 as the
  30 s default and rejects only negative values (`consumer/resolver_config.go:61-63`,
  `internal/durable/config.go:87-89`). The foot-gun is real only for
  `source.WithReconcileInterval(0)` (`source/nats_kv.go:50-63,807-814`) and
  for direct internal-resolver use.
  Fix: scope F8 to source only.
- **P0 — F9** misread `RenewLeadership`. It claimed every renew failure calls
  `clearLeadership()`. Actually, `context.Canceled` and `context.DeadlineExceeded`
  skip the clear (`internal/election/nats_election.go:190-207`), so the
  agent retains `workerID` / `termRevision` on ambiguous failures, and a later
  `RequestLeadership` re-renews into the same term
  (`internal/election/nats_election.go:112-130`). The split-brain-safety
  conclusion still holds — but for different reasons.
  Fix: split definite-loss errors from context-timeout errors; explain term
  retention.
- **P0 — F5** said the permanent-failure state must surface to readiness so
  pod rotation can recover a stream wipe. Runtime restart does NOT create
  message streams — `Manager.Start` ensures only the Parti KV buckets
  (`manager.go:417,434,441`); `ensureConsumer` bails on stream-not-found
  (`internal/durable/partition_consumer.go:441-460`).
  Fix: state plainly that readiness can stop a silently wedged pod and alert
  orchestration, but the stream itself must be restored via an
  `OnStreamMissing` hook or a provisioning path.
- **P1 — F5 compat path.** `Dynamic.Update` runs `CheckWorkQueueRecoveryCompat`
  under `sync.Once`, but the manager-facing `Dynamic.UpdateWorkerConsumer`
  bypasses it (`consumer/dynamic.go:309-327`).
  Fix: require the compat re-arm on both paths.
- **P1 — F10-B.** Overstated as a three-flag requirement. The real model is
  one: a configured processing gate auto-enables pull gating
  (`internal/durable/config.go:295-305`).
  Fix: warn/reject when two-phase handoff is enabled and the consumer does
  not report `CapProcessingGate`.
- **P1 — F1** attributed the leader-revision reset to a `parti-assignment`
  bucket wipe. Wrong: `LeaderRevision` is sourced from the election agent
  (`internal/election/nats_election.go` via `manager_election.go:316-323`),
  not the assignment bucket. An assignment-bucket wipe resets commit/CAS
  history, not the leader-revision space.
  Fix: split — election-bucket wipe ⇒ leader-revision; assignment-bucket
  wipe ⇒ commit/CAS state.
- **P2 — F10-C** partition-fencing status was too strong ("locked,
  unimplemented, deferred"). README still has pending P1 fold-in.
  Fix: name the pending P1 work explicitly.

## Round 2 — Round-1 fix verification + scan

All Round-1 P0s **correctly fixed**. F1, F5 compat, F10-C **correctly fixed**.

**P1 — F10-B remained wrong.** The R10-B recommendation still placed the
check at `Start`, but `CapProcessingGate` is a *runtime* "successfully wired"
bit — the durable consumer sets it only inside `addSubjectLoop` after a gated
handler is wrapped (`internal/durable/worker_consumer.go:386-398, 604-623`),
and the manager samples capabilities only after
`handoffCoordinator.Apply` (`manager_assignment.go:851-859` →
`manager.go:815-832`). A literal `Start`-time check would false-positive on
correctly-configured gated consumers.

Fix: place the check at the capability-sampling point (after the first
non-empty two-phase apply), not at `Start`.

## Round 3 — Round-2 fix verification

R10-B core is correctly fixed (post-apply lifecycle). **P1 — F10 change-risk
paragraph remained stale**: still called the warning "a startup warning"
even though the recommendation, ranking, and Phase-0 sequencing had all been
updated to "first two-phase apply."

Fix: clean up the stale phrasing in the change-risk text.

## Round 4 — final confirmation

R10-B change-risk text correctly fixed. Final glance clean. **No P0/P1
remaining.**

## Round 5 — F6 restructuring pass

F6 was split into F6-A (source-loss signalling) and F6-B (empty/shrunk
partition-input freeze). Two issues surfaced after the split:

- **P0 — F6-B** still treated an *erroring* partition-source observation
  as one that could publish zero partitions. Wrong: `rebalance` returns
  immediately on `snapshotSource` error before assignment calculation or
  publish (`internal/assignment/calculator.go:1279-1283`).
  Fix: scope F6-B to **non-erroring** empty/shrunk observations only;
  explicitly state the erroring path is already correctly handled.
- **P1 — Cross-references** still said umbrella "F6" where the text
  specifically described F6-A.
  Fix: rewrite the two stale cross-references.

## Round 6 — Round-5 fix verification

F9-A correctly fixed (one-line code diff vs operational migration cleanly
separated; F1 dependency named).

**Still wrong:**
- F6-B's recommendation now has the right contract, but the finding's
  *description* still said an erroring source proceeds to recompute/republish.
- Executive summary still had a bare `F9` bullet where the rest of the
  doc had moved to `F9-A`.

Fix: rewrite the F6-B finding description; promote the executive-summary
bullet to `F9-A`.

## Round 7 — Round-6 fix verification

F6-B finding correctly fixed; F9-A bullet correctly fixed; remaining bare
"F9" references are umbrella-section contexts only (acceptable).

**P1 — Stale F6-B error-path overstatements** still appeared *outside* the
main finding/recommendation: in the executive summary, the
dynamic-partitioning summary, the failure-mode matrix, the impact paragraph,
and the ranking table — each still implied an empty/erroring source could
reassign or publish zero.

Fix: scope every cross-reference of F6-B to "empty / sharply shrunk
*non-erroring*" observations.

## Round 8 — Round-7 fix verification

All five F6-B cross-references correctly fixed (executive summary,
dynamic-partitioning summary, matrix row 13, impact paragraph, ranking
table row).

**Final glance clean. No P0/P1 remaining.**

## Round 9 — Companion-addition validation

Round 9 evaluated the F9-A "OperationTimeout ≤ ElectionTimeout/3"
companion warning addition. Every technical claim was confirmed against
source:

- `OperationTimeout` default `10s` (`config.go:358-360`).
- `ElectionTimeout` default `10s` (`config.go:362-366`).
- Renew ticker interval `ElectionTimeout/3` (`manager_election.go:191-192`).
- Renew bounded by `context.WithTimeout(m.ctx, m.cfg.OperationTimeout)`
  (`manager_election.go:205-209`).
- `monitorLeadership` is single-goroutine ticker; a hanging renew
  blocks subsequent ticks (`manager_election.go:191-275`).
- `warnOnShortAuditGrace` is a valid one-shot startup-warning pattern
  reference (`manager_setup.go:385-408`).

The companion addition was accepted as part of the F9-A PR.

## Net result (findings review)

After 9 rounds, every P0 was resolved, every P1 closed, and the only
P2 finding (F10-C status) was corrected. The findings document
(`findings.md`) is the artefact that absorbed every fix; this trail
exists so a future reader knows the document's history without
re-loading nine verbatim transcripts.

---

## Implementation-plan review (a separate 3-round loop)

After the findings doc landed, the phased implementation plan
(`00-fix-plan.md`) went through its own external-reviewer loop via
the `/plan-review` skill. Reports archived under
`tmp/00-fix-plan_self-healing-v{1,2,3}_review.md`.

### Plan-review round 1 (v1)

External reviewer flagged **3 P0** and **2 P1** items in the initial
draft of `00-fix-plan.md`:

- **P0 — F5 checkpoint reseed** would still skip messages on a fresh
  recreated stream. The plan said "fetch new stream's `AckFloor` and
  overwrite the in-memory checkpoint", but `Checkpoint.Seed` is
  monotonic (`internal/recovery/checkpoint.go:30-40`) and
  `BuildConfig`'s `FromLastProcessed`-with-`checkpoint==0` falls
  through to `DeliverNewPolicy` (`internal/recovery/config.go:22-28`).
  Both paths skip messages on a fresh stream.
- **P0 — F5/P2.4d dependency.** The Phase-2 sequence had P2.3 (F5)
  before P2.4a, but P2.3's "no hook = `OnError` + readiness flip"
  test requires the envelope wired into `partition_consumer.go`
  (P2.4d's territory), not the source watcher (P2.4a).
- **P0 — F6-B readiness claim unreachable.** The "How this trips
  readiness" line said F6-A would eventually fire for a sustained
  non-erroring empty source — but F6-A only triggers on
  `ErrBucketNotFound`, never on a non-erroring observation.
- **P1 — F10-A pseudocode** referenced fictitious APIs
  (`HasConfirmedDeaths()`) and the wrong layer
  (`WorkerMonitor.GetActiveWorkers` instead of
  `Calculator.getActiveWorkers`).
- **P1 — F9-A runbook** used `nats kv del` (key delete) instead of
  `nats kv rm` (bucket removal), and invented a
  `parti version --features` command that doesn't exist.

### Plan-review round 2 (v2)

Round-1 items closed; two new findings raised by the revisions:

- **P0 — F5 reset ordering + generation fencing.** The new
  `ResetForStreamRecreate` design didn't pin the control-flow
  ordering. `Controller.recover` reads checkpoint + calls
  `BuildConfig` *before* `recreate`
  (`internal/recovery/controller.go:273-294`), so a hook fired
  inside `recreate` would still see the stale config. Separately,
  manual-ack mode (`controller.go:185-188`, `tracking_msg.go:19-37`)
  could let a late ack from the OLD-stream handler raise the
  checkpoint after the reset, defeating it.
- **P1 — `shrunkObservations` collision** between F6-B (P2.2) and
  F10-A (P2.5): both added a field with that name to the same
  `Calculator` struct. Additionally, F6-B T1's boundary wording was
  off-by-one — it described `PartitionShrinkConfirmationCount`
  total observations of suppression where the design suppresses
  only `N-1`.
- **P2 — F9-A wording** claimed "the rest are already `FileStorage`"
  but heartbeat is `MemoryStorage` (`manager_setup.go:93`); the
  companion warning's remedy said "defaults: 10s / 30s" but both
  timeouts default to 10s.

### Plan-review round 3 (v3) — clean

All v2 items closed. Verdict: **ready to implement**. The only
remaining gate is the plan's existing requirement that P2.4d land
before P2.3 — which is now stated consistently across the PR
table, the F5 dependency paragraph, and the P2.4d section.

Net effect of the plan-review loop:

- F5's design now pins the stream-missing control-flow ordering
  explicitly (detection → hook → `HandleStreamRecreated`
  [`streamEpoch++` → `ResetForStreamRecreate` → re-seed → set the
  one-shot `recreatedSinceLastBuild` flag] → re-enter `recover`).
- A stream-epoch generation fence on `Checkpoint.Advance` prevents
  late acks from prior stream generations from raising the reset
  checkpoint.
- `BuildConfig` gains a new rule: on the recreated-stream path with
  checkpoint 0, deliver via `DeliverAllPolicy` (not
  `DeliverNewPolicy`), replaying the fresh stream from sequence 1.
- F6-B and F10-A use distinct counter names
  (`partitionShrunkObservations` and `workerShrunkObservations`),
  and a cross-feature isolation test (P2.5 T7) locks the separation.
- F6-B's readiness policy is now explicit: confirmed shrink is
  treated as legitimate operator intent; the library cannot
  distinguish a buggy source from intent; mitigation is operator
  vigilance via warn logs + the new
  `parti_partition_observation_suspicious_total` counter.
- F9-A's operator runbook uses `nats kv rm` consistently with
  `docs/OPERATIONS.md`, and the verification steps use real
  commands (release-tag check, log grep, `nats stream ls`,
  `nats kv info | grep Storage`).
