# Adversarial verification — Family B (NP-3b) recover-on-wrong-signal finding

Reviewer: independent agent. Branch `auto-heal-gap-investigation`, HEAD `2453306`.
READ-ONLY. Goal: REFUTE the Family-B finding (root cause, proof non-vacuity, recommended
fix). Every claim below is checked against the actual code in the worktree or a captured
`.out`.

Verdict: **confirmed-with-caveats.** I could not break the root-cause mechanism, the
proof is non-vacuous and isolated, and the recommended fix (reason-scoped
heartbeat-success-since-degrade exit gate) survives my strongest objections. The
finding's discrepancy call against report line 52 is itself CORRECT and well-evidenced.
Caveats are all narrow (see §5).

---

## 1. Root-cause mechanism — attempted refutation, FAILED to break

Re-read the full chain in code:

- Recovery driver is connection uptime, 1s tick: `monitorNATSConnection`
  (`manager_degraded.go:79`) → `checkConnectionHealth` (`:97`) → with connection UP and
  `connUpSince` held for `ExitThreshold`, calls `attemptRecoveryFromDegraded` every tick
  (`:127-133`). VERIFIED.
- `attemptRecoveryFromDegraded` (`:376-416`) gates exit on (1) `refreshAssignmentFromNATS`
  success (`:383`) and (2) `currentAssignmentApplied(cur)` (`:409`); on success calls
  `recordKVSuccess()` (`:393`) which wipes the ENTIRE `kvErrorWindow` (`:244-250`), then
  `exitDegraded()` (`:415`). It NEVER inspects the failing op. VERIFIED.
- `refreshAssignmentFromNATS` (`manager_assignment.go:1561-1599`) reads exactly one key
  `assignment.<workerID>` from `m.assignmentKV` (`:1568`). Zero knowledge of
  heartbeat/election/stableid. VERIFIED.
- The fault wrapper `kuFaultKeyValue` faults `Put`/`Update`/`Create`/`Get`
  (`manager_kv_read_unavailable_test.go:95-125`) on the `{Election, Heartbeat, StableID}`
  buckets only (`np3...:142-147`). The assignment bucket is excluded, so its `Get`
  succeeds while the trigger ops keep timing out. VERIFIED — this is exactly the wrong
  signal.
- Re-degrade half: after the false exit, `degradedSince==0` so `recordKVError`'s
  short-circuit (`:174`) reopens; the faulting heartbeat publisher
  (`SetOnError→recordKVOpError→recordKVError`, `manager_election.go:424`) re-accumulates
  `ErrKVUnavailable` entries (`markKVUnavailable :58-72`, `recordKVError :186`) to
  `KVErrorThreshold=3` → re-enter `kv-unavailable` Degraded (`:216-224`). VERIFIED.
- `recordKVHealthyOp` (`421f13c`, `manager_degraded.go:266-288`) cannot help: it fires
  only on heartbeat Put SUCCESS (`manager_election.go:432`, publisher `onSuccess` at
  `internal/heartbeat/publisher.go:360-364`), and the heartbeat bucket is faulted, so
  success never fires; it also early-returns while degraded (`:267-269`). VERIFIED.

I looked for an escape path the finding missed:
- **Could the worker hit `claimLostShutdown` and stop the flap?** NP-3b faults the
  stableID bucket and `IntegrationTestConfig` sets `WorkerIDTTL=5s` over a ~13s window.
  But `claimLostShutdown` fires on `stableid.ErrClaimLost` (a peer-takeover signal,
  `manager_election.go` onClaimerError); the faulting stableID *renew* surfaces a plain
  `DeadlineExceeded`, which routes to `recordKVOpError` (the degrade circuit), NOT to the
  claim-lost branch. With a single worker there is no peer to trigger `ErrClaimLost`.
  So the flap does NOT get masked by a self-stop. This is a path the finding did not
  spell out, but it CONFIRMS rather than breaks the mechanism (the flap is the genuine
  observed behavior). Noted as caveat §5.3.
- **Could `currentAssignmentApplied` block the exit instead?** No — the harness arms the
  fault only after Stable (`np3...:176-191`), so `committedAssignment==snapshot` and the
  guard at `:409` is not taken (test design comment `:180-187`). VERIFIED; this is correct
  isolation of the pure exit defect.

Conclusion: root cause **holds**. No missed path makes it vacuous or wrong.

## 2. Proof non-vacuity / isolation — verified against the test and .out

`TestNP3_KVUnavailable_HeldArmed_DoesNotFalselyExitToStable`
(`np3_kv_unavailable_recovery_test.go:223-279`):
- Non-vacuity guards evaluated BEFORE the hard gate: `nc.IsConnected()` (`:255`),
  `injectedMid>injectedStart` and `injectedEnd>injectedMid` (`:257-260`) — proves the
  fault is genuinely active across the whole window, so a zero-exit result would be
  meaningful. VERIFIED.
- Hard gate `require.Zero(degradedExits())` (`:275`) is the single load-bearing assertion;
  `degradedToStable` counts only `Degraded→Stable` edges via `OnStateChanged`
  (`:62-64`). VERIFIED that this is the false-exit edge.
- `.out` evidence (`tmp/repro-current-head/np3b.out`): `FAIL ... Should be zero, but was
  9`, `re-entries=9 injected=36`, 13.03s. Each re-entry requires a prior false exit
  (`enterDegraded` CAS-guards `degradedSince` at `:309`), so 9 re-entries is irrefutable
  oscillation. VERIFIED.
- Positive control NP-3a (`:285-313`) PASSES on main: after disarm, recovers and HOLDS
  (`require.Never(Degraded,5s)`), proving NP-3b's exit is FALSE, not a dead path. VERIFIED
  by construction (heartbeat resumes after `armed.Store(false)`).

The proof proves what it claims. Non-vacuous, isolated.

## 3. Recommended fix (Candidate 2, reason-scoped) — strongest objections

**Objection A — "the gate is unnecessary; reuse `recordKVHealthyOp`."** Survives the
finding: `recordKVHealthyOp` early-returns while degraded (`:267-269`), so it records
nothing during a degrade and is useless as a "recovered SINCE degrade" signal. The fix
correctly calls for a SEPARATE always-on stamp. VERIFIED the early-return exists.

**Objection B — "an unconditional gate is simpler and still correct."** Does NOT survive;
the finding is right that it regresses NP-5. VERIFIED end-to-end:
`TestNP5_BlockedApplyStartupTimeout...` uses `newTestManager`
(`manager_commit_state_machine_test.go:152-173`) which injects a `recordingHeartbeat`
stub (`:163`) and never calls `startHeartbeat`/`SetOnSuccess` — no heartbeat success ever
fires. NP-5 calls `attemptRecoveryFromDegraded()` directly (`np5...:158`) expecting
Degraded→Stable (assertion 4, `:160-161`). Under an unconditional
`lastHeartbeatSuccessAt <= degradedSince` gate, `lastHeartbeatSuccessAt` stays 0 →
gate closed → assertion 4 FAILS. So reason-scoping is load-bearing, and reason-scoping
requires new `m.lastDegradedReason` state (the reason is passed transiently to
`enterDegraded :300` and never persisted — VERIFIED there is no `m.lastDegradedReason`
in production today). The finding's "not a pure one-liner" honesty is correct.

**Objection C — "the gate could keep the manager stuck Degraded in production."** Does NOT
survive as a regression: if the genuine fault clears but heartbeat recovers last, the
manager stays Degraded slightly longer — MORE conservative, the correct direction for the
M2 keep-readiness-degraded policy. NP-3a proves it DOES open once heartbeat resumes. No
deadlock: heartbeat publishes every `HeartbeatInterval`, so a real recovery opens the
gate within one interval.

**Objection D — "candidate 2 breaks NP-9 or C1."** Does NOT survive.
- NP-9: VERIFIED by reading the actual test
  (`np9_full_quorum_loss_arbitration_test.go`), NOT the finding's contrast table. The
  fault set genuinely includes all four coordination buckets — Election, Heartbeat,
  StableID, AND Assignment (`:184-189`) — plus a faulting `Watch()` override
  (`:142-148`). The first OnDegraded reason is genuinely `kv-unavailable` (`:240`), the
  fast threshold path winning the CAS as the report claims. While armed, the assignment
  `Get` faults (`:130-136`) so `refreshAssignmentFromNATS` fails at `:383` and the exit
  is never reached. After `disarm()` (`:253`), heartbeat (in NP-9's fault set, `:186`)
  and assignment resume → the reason-scoped gate (reason is `kv-unavailable` → applies)
  opens on the post-disarm heartbeat success → recovery proceeds; the test asserts
  exactly 1 Degraded entry / 1 Stable exit (`:263-270`), preserved. Note: even if NP-9's
  reason were NOT `kv-unavailable`, an inapplicable scoped gate simply falls through to
  the normal exit, so NP-9 recovers either way — the gate cannot make NP-9 worse.
- C1 (whole-bucket loss): enters Degraded with reason `"KV error threshold exceeded"`
  (`manager_degraded.go:215`), NOT `kv-unavailable`, so the reason-scoped gate does not
  even apply; and heartbeat never succeeds under whole-bucket loss anyway, so even if it
  applied the gate stays closed (stickier). `TestManager_LiveNATSBucketLoss*` unaffected.
  The fix is exit-only; the C1 ENTRY path (`recordKVError`→threshold→`enterDegraded`) is
  untouched. VERIFIED.

**Objection E — "the heartbeat-only proxy is a hole: an election-only or stableid-only
sustained fault with healthy heartbeat opens the gate early."** This survives as a REAL
residual but is correctly characterized by the finding: that exact shape is already let
through by the shipped f-d1 `recordKVHealthyOp` semantic narrowing (a heartbeat success
clears the transient election/stableid entries before they reach threshold), so candidate
2 introduces NO NEW regression relative to the f-d1 decision. It is the one corner where
candidates 2 and 3 diverge observably and the finding flags it as an open question with a
proposed dedicated test. Accepted as a known, bounded residual — not a refutation.

Net: the recommended fix survives. The reason-scoping is genuinely load-bearing.

## 4. Discrepancy with report line 52 — INDEPENDENTLY CONFIRMED

Report `04-proof-findings.md:52`: "A single fix to the exit gate could close both A and
B." The finding calls this LOOSE/overstated. I verified this is correct:
- Family A re-degrades via `checkBucketEpochs → enterDegraded("bucket-recreated:<b>")`
  fired DIRECTLY (`manager_setup.go:684-690`), bypassing `kvErrorWindow` entirely, and
  `ep.created` is never re-captured (cached once at `:627`). VERIFIED.
- NP-2 (`np2...:150-159`) recreates the heartbeat bucket as a fresh HEALTHY MemoryStorage
  stream and leaves `KVErrorThreshold` at default, asserting `otherDegrades==0`
  (`:81-83, :190-192`). So after recreate heartbeat Put SUCCEEDS and the degrade reason
  is purely `bucket-recreated:<heartbeat>`. VERIFIED.
- Therefore a reason-scoped `kv-unavailable` exit gate (candidate 2) does NOT apply to
  Family A's `bucket-recreated` reason, AND an unscoped heartbeat-success gate would OPEN
  (heartbeat is healthy). Either way candidate 2 does NOT close Family A. VERIFIED.
- The report is internally inconsistent: line 52 oversells, line 280-282 hedges ("A still
  needs the stale-`ep.created` latch addressed"). The finding flags line 52 as the
  imprecise one — correct.

The honest unified A+B fix is candidate 3's family (per-cause registry) PLUS the Family A
`ep.created` re-capture latch. Confirmed.

## 5. Caveats on the finding (do not change the verdict)

5.1 Numeric: finding/report numbers (`re-entries=9 injected=36` vs report
`degradedExits=9 injected=34`) are same-magnitude; the `.out` matches the finding's
numbers verbatim. Not a correctness issue.

5.2 Production cadence: defaults are EnterThreshold=10s, ExitThreshold=5s,
KVErrorThreshold=5, KVErrorWindow=30s (`config.go:191,196,204,209`). The finding lists
"5s/10s/15s" in Exit/Enter/RGP order — consistent with the code. Mechanism identical to
test, only cadence differs. OK.

5.3 NEW observation (the finding did not spell this out): NP-3b faults the stableID bucket
with `WorkerIDTTL=5s` over ~13s, yet does NOT self-stop via `claimLostShutdown`, because
a faulting renew surfaces `DeadlineExceeded` (→ degrade circuit) not `ErrClaimLost`, and
a single worker has no peer to trigger claim takeover. This CONFIRMS the flap is the
genuine behavior and is not masked by the C2 self-stop path. Worth noting for any future
multi-worker variant of this proof, where a peer takeover could change the dynamics.

5.4 Not empirically prototyped: candidate 2 was not built/run; §6 of the finding is
code-derived. The mandatory validation harness on landing (NP-3b ungated, NP-5 ungated,
NP-9, LiveNATSBucketLoss) is correct and sufficient. Agreed.

5.5 IMPLEMENTATION-ORDERING RISK (not a design refutation, but must be specified for the
implementer): the reason-scoped gate reads `m.lastDegradedReason`, which the design sets
in `enterDegraded`. The 1s recovery tick (`checkConnectionHealth → attemptRecoveryFromDegraded`)
observes `degradedSince != 0` as the "am I degraded" trigger (`:378`). `enterDegraded`
sets `degradedSince` via CAS at `:309` BEFORE doing anything else. If the implementation
writes `m.lastDegradedReason` AFTER the CAS, there is a tiny window where the recovery
tick sees `degradedSince != 0` but a stale/empty reason → `reasonIsKVUnavailable` is
false → the gate does not apply → one false exit before the reason lands → a single flap
on the first degrade. Mitigation: set the reason BEFORE (or atomically with) the
`degradedSince` CAS, OR treat an empty/unknown reason conservatively as "gate applies"
(stay degraded). A `-race` run plus the ungated NP-3b proof would catch a regression
here. This is the one concrete way a careless implementation could reintroduce the flap.

## 6. Bottom line

Root cause: **holds.** Proof: **non-vacuous and isolated.** Recommended fix: **survives**
all objections; reason-scoping is load-bearing (NP-5) and the new `m.lastDegradedReason`
state is genuinely required. Report-line-52 discrepancy: **independently confirmed.** No
counterexample found. Verdict: confirmed-with-caveats (all caveats narrow).
