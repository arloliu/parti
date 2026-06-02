# Adversarial verification — Family C mech 1 (NP-8): claim-loss self-stop on outage >= WorkerIDTTL

Reviewer pass date: 2026-06-01
HEAD: 2453306 (worktree `auto-heal-gap-investigation`)
Scope: READ-ONLY. No production code changed. Independent re-read of every cited
anchor + the gated proof + the `.out` evidence under `tmp/repro-current-head/`.

Verdict: **confirmed-with-caveats.** The root-cause code path is correctly traced
and the cross-test WorkerIDTTL contrast genuinely isolates TTL-expiry as the cause.
I could not refute the mechanism. Three caveats sharpen (not overturn) the finding;
one of them materially weakens the finding's stated *reason* for its recommended fix,
though not the fix's conclusion.

---

## What I verified independently (and agree with)

1. **The conflation locus is real.** `claimer.go:362-368`: the renewal `kv.Update(key,
   value, c.lastRevision.Load())` failure switch returns **bare** `ErrClaimLost`
   (`fmt.Errorf("%w: ID %s", ErrClaimLost, wid)`, :368) for the `ErrKeyExists` case,
   with no connectivity / not-found sentinel. The bucket-missing case (:369-371,
   `ErrNoStreamResponse`/`ErrBucketNotFound`/`ErrStreamNotFound`) wraps a sentinel and
   is routed to the degraded circuit instead. So "peer bumped my revision (key
   present)" and "lease aged out (slot empty)" collapse into the identical bare error.

2. **The routing is exactly as described.** `onClaimerError` (`manager_election.go:106-127`):
   `errors.Is(err, ErrClaimLost)` true; `IsConnectivityError || IsDegradingJetStreamError`
   both false for the bare wrap (`natsutil/errors.go:92-100` checks only
   `ErrBucketNotFound`/stream-not-found/consumer-not-found; `:114-137` checks the
   connectivity sentinels + the `"connection refused"`/`"i/o timeout"` text fallback —
   the bare `ErrClaimLost` string carries none of these) → `claimLostShutdown` (:118) →
   `m.Stop` → `StateShutdown`, terminal.

3. **MaxAge==WorkerIDTTL reconciliation is real** (`config.go:366-370`,
   `manager_setup.go:91-98` calling `reconcileStableIDBucketMaxAge` :373+). The stableID
   bucket is **FileStorage** (`manager_setup.go:92`) and relies *entirely* on MaxAge to
   expire abandoned claims (comment :86-90).

4. **Renewal cadence / boundary math is correct.** `renewInterval = max(ttl/3, 100ms)`
   (`claimer.go:491-493`). At 75s default → 25s cadence, purge at `last-renewal + 75s`,
   so the triggering outage is phase-dependent **~50-75s**. The finding's correction of
   the report's "multi-minute" to "minute-scale (~50-75s)" is accurate. This is a valid
   correction *to the report* (04-proof-findings.md:160,:251,:286), and I confirm it.

5. **Discrepancy #3 (C2 pin mis-cite) is correct and material.** I read all three tests.
   `TestStableID_StaleKeyTakeover_Reclaim` lives in `test/integration/stableid/
   stableid_takeover_test.go:21` and exercises the *claimer's* reclaim of a leaked key —
   it does NOT touch `onClaimerError` self-stop routing. The actual self-stop pins are
   `TestOnClaimerError_ClaimLostStopsWorker` (`manager_claimer_error_test.go:53-86`,
   routing-level: feeds bare `ErrClaimLost`, asserts `claimLostShutdown` fires + OnError
   hook) and `TestManager_StopsItselfWhenClaimLost` (`:135-172`, end-to-end: a peer `Put`
   bumps the revision, worker reaches `StateShutdown`, consumer revoked). The task brief's
   contract-C2 citation of the takeover-reclaim test is wrong; the finding's correction is
   right.

---

## Caveat 1 (strongest) — the gated proof does NOT isolate mechanism 1; attribution is INFERENCE

`TestNP8FleetNATSOutage_LeaderContinuityRecoversFleet` instruments only `OnStateChanged`
and `OnDegraded` (np8_..._test.go:110-132). It has **no** hook observing `StateShutdown`
or the claim-lost `OnError`. The captured failure (`tmp/repro-current-head/np8_mech1.out:8`)
is `manager[1] failed to reach state Stable: context deadline exceeded` at line 187 — it
proves the **symptom** (no clean heal), not the **mechanism** (claim-loss self-stop).

Crucially, **both** mechanisms are live in this single test: WorkerIDTTL=5s (mech-1 trigger)
AND the MemoryStorage heartbeat bucket is lost on the single-node restart (mech-2 trigger,
`manager_setup.go:156`). The test cannot tell which one prevented Stable.

What *does* isolate mech-1 is the **cross-test WorkerIDTTL contrast**:
- mech-1 proof (WorkerIDTTL=5s): never reaches Stable, fails at the all-Stable wait (:187).
- mech-2 proof (WorkerIDTTL=30s, `np8_..._test.go:288`): DOES reach all-Stable (:325 passes
  per `np8_mech2.out` failing only at :327, the HOLD check) then flaps.

The only delta is WorkerIDTTL, and the heartbeat-flap driver (mech-2) is TTL-independent —
so "never reaches Stable at 5s vs reaches-then-flaps at 30s" is most parsimoniously
explained by a terminal `StateShutdown` at 5s. That is a sound **inference**, but it is an
inference, not a direct assertion. The finding itself acknowledges this gap (its Option 0(iv)
recommends re-purposing the proof to assert `StateShutdown`). I am hardening a gap the
finding already flags — which is why this is a caveat, not a refutation.

## Caveat 2 — the empirical "claim lost actually fired" chain rests on ONE un-captured diagnostic

The only empirical confirmation that the **bare `ErrClaimLost`** (not a connectivity error)
fired in NP-8 is the narrative `Degraded→Shutdown @8.2s with OnError: "worker ID claim lost:
ID worker-N"` at **04-proof-findings.md:135**. I grepped: that artifact exists ONLY as prose
in 04-proof-findings.md (:135,:147-148) and is re-quoted in the finding note (:75). It is
**not** present in any `.out` under `tmp/` (`grep -rln "claim lost" tmp/` → none; the only
shutdown-bearing string is in the doc). The gated proof cannot regenerate it (no OnError /
Shutdown instrumentation). So the empirical link from "outage > WorkerIDTTL" to "bare
ErrClaimLost fired" rests on a single un-captured diagnostic run.

The related un-pinned wire link: `TestClaimer_WireContract_RevisionMismatchIsErrKeyExists`
(`claimer_test.go:650-681`) pins `ErrKeyExists` only for a **present** key with a stale
revision (Put v2, then Update with the v1 rev). It does **not** cover the **purged-key**
case ("wrong last sequence: 0" — no message for the subject). If a FileStorage reload after
restart did NOT promptly purge the aged message, the renewal Update's `lastRevision` would
still match and renew would **succeed** → mech-1 would never fire. The finding asserts the
purge surfaces as `ErrKeyExists` but does not pin it with a test.

Assessment: residual risk **low, not zero**. NATS MaxAge purges the latest revision too
(this is the documented premise of the stableID bucket's whole expiry/stale-takeover design,
`manager_setup.go:86-90`, `claimer.go:195-201,224-247`), and the @8.2s diagnostic — even
un-captured — corroborates it. But the mechanism's empirical chain is one un-captured run
plus a wire-contract test that covers the adjacent (present-key) case, not the exact
(purged-key) case. Flag it; do not treat it as fully pinned.

## Caveat 3 — the recommended-fix REASONING is part-wrong (its conclusion survives only for partial outages)

The finding's load-bearing argument for "never ship Option 1 (ID-layer re-claim) alone" is
the split-brain steelman (note §(c),:145-165): an outage long enough to age the claim has
expired the heartbeat ~35-60s earlier (HeartbeatTTL=15s << WorkerIDTTL, `config.go:370,381`),
so "the fleet already rebalanced your partitions" and an in-process resume-from-cache would
double-process.

This reasoning is **false for the actual NP-8 full-fleet scenario it analyzes.** In NP-8
*every* worker (including the leader) is disconnected during the outage. With no connected
leader, **no calculator runs and nothing rebalances during the outage** — the leader's
calculator path (`worker_monitor.go:167` `heartbeatKV.Keys`) cannot even execute while the
connection is down, and on reconnect the heartbeat bucket is empty/missing (mech-2). There is
no surviving owner for a resumed worker to double-process against. So the cited
double-processing risk does not materialize in the full-fleet case.

The risk the finding describes is real only in a **partial** outage (one worker network-
partitioned while the fleet + leader survive and rebalance). That is a *different* scenario
than NP-8. And even there, the finding does not trace whether a resumed worker's processing is
genuinely novel double-processing or merely the normal handoff-fenced rebalance overlap that
Parti's pull-gating / handoff coordinator already fences — I did not trace those internals
either, so it is unresolved, not disproven.

Net on the fix: the **objection to the finding's reasoning survives** — "heartbeat already
expired → fleet rebalanced → Option 1 double-processes" is not the operative risk in NP-8.
The **conclusion** ("doc-only is the right near-term call; do not ship a naive in-process
re-claim without verifying the data plane") is still defensible on the weaker, correct
grounds: (i) orchestrator-layer pod rotation already heals cleanly (`StateShutdown` trips the
readiness probe; a fresh `Start` re-claims into the now-empty FileStorage slot), and (ii) any
in-process re-claim that resumes the data plane needs its own integration proof against the
partial-outage rebalance boundary, which does not exist. So doc-only stands; the *justification*
must be corrected away from the full-fleet split-brain framing.

---

## Counterexample attempts that FAILED to break the mechanism

- **"Maybe the first renew after reconnect hits a connectivity error, not ErrKeyExists, so it
  routes to recordKVError and never self-stops."** The renewal op timeout is `ttl/2` clamped to
  [100ms,5s] (`claimer.go:312-317`); on reconnect the connection is CONNECTED (the test gates on
  `nc.Status()==CONNECTED` before asserting, :183-184), so the `kv.Update` reaches the server and
  returns the revision-mismatch `ErrKeyExists`, not a connectivity timeout. Even if one tick
  raced a connectivity error first, the **next** tick (1.67s later at ttl=5s) self-stops, because
  nothing re-claims the purged key (`renewalLoop` :306-337 keeps ticking until `ErrClaimLost`).
  Does not break it.

- **"Maybe FileStorage reload preserves the message so the revision still matches."** Could not
  refute by code (no test pins the purged-key wire surface); recorded as Caveat 2. The MaxAge
  design premise and the @8.2s diagnostic make it low-risk, but it is the one un-pinned link.

- **"Maybe the connectivity/degrading branch swallows it (regressing C1 if a fix touches it)."**
  No: the bare wrap carries no sentinel, so `IsConnectivityError`/`IsDegradingJetStreamError` are
  both false and the C1 branch (`onClaimerError:113-115` → `recordKVError`) is not entered. Any
  fix confined to the `:117-118` else-branch leaves C1's lockstep fan-out
  (`TestManager_LiveNATSBucketLoss`) untouched. Confirms the finding's cross-family claim.

---

## Bottom line

Mechanism: **holds-with-caveats.** Root cause correctly re-derived; the WorkerIDTTL one-variable
contrast isolates TTL-expiry. Two residual risks that matter: (i) mech-1 *attribution within the
proof* is an inference (the proof has no Shutdown/OnError instrumentation; both mechanisms are
live in it) and the empirical "bare ErrClaimLost fired" rests on one un-captured diagnostic +
a wire test that covers the adjacent case; (ii) the finding's stated *reason* for "never ship
Option 1 alone" (full-fleet split-brain) is wrong for NP-8 — no leader rebalances when the whole
fleet is down — though the doc-only conclusion survives on orchestrator-rotation + unproven-data-
plane grounds. Recommend: keep doc-only, but (a) add the `StateShutdown`/OnError instrumentation
the finding's Option 0(iv) proposes so attribution stops being an inference, and (b) re-base the
"don't ship Option 1 alone" justification on the partial-outage case, not the full-fleet one.
