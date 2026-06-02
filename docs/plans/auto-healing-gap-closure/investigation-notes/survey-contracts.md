# Cross-Cutting Survey: Contract & Test-Impact Mapping for Auto-Heal Gap Fixes

Branch/worktree: `auto-heal-gap-investigation` @ HEAD 2453306 (read-only investigation).
Purpose: for every candidate fix across Families A/B/C, map which cross-feature
contracts (AGENTS.md C1–C4) and pinning tests it would *touch*, *risk regressing*, or
*collide with another family's fix*. This is the input the follow-up fix session needs
to pick fixes that don't break the existing degraded/claim-loss contracts.

Every claim below is `file:line` or a named test, verified in code this session.
"VERIFIED" = read directly. "INFER" = reasoned from verified facts.

---

## 0. The contracts and what actually pins them (re-derived, with corrections)

| Contract | Statement | Mechanism (VERIFIED) | Pinning test(s) (VERIFIED) |
|---|---|---|---|
| **C1** | Whole-bucket-missing => every worker enters StateDegraded within a bounded window | `recordKVError` admits connectivity / degrading-JetStream errors only (`manager_degraded.go:165-167`), accumulates `kvErrorWindow` vs `KVErrorThreshold` (`:207`), calls `enterDegraded("KV error threshold exceeded")` (`:215,224`). Whole-bucket entries are `transient=false` and are **never** cleared by `recordKVHealthyOp` (`manager_degraded.go:278-284`). | `TestManager_LiveNATSBucketLoss` (all 3 workers Degraded ≤20s, buckets stay gone), `TestManager_PartialBucketLoss_HeartbeatHealthy` (wipe all EXCEPT heartbeat; all 3 still Degraded ≤25s — the f-d1 class-aware reset guard) |
| **C2** | Peer claim takeover => ONLY that worker enters claim-lost shutdown; others stay healthy | `onClaimerError` (`manager_election.go:106-127`): `ErrClaimLost` + NOT (connectivity OR degrading-JetStream) => `claimLostShutdown` => `StateShutdown`. Connectivity/degrading `ErrClaimLost` is routed to `recordKVError` instead (`:113-115`) so whole-bucket StableID loss degrades the fleet, not self-stops one worker. | **Unit (the real C2 routing proof):** `TestOnClaimerError_ClaimLostStopsWorker` (peer-takeover => stop + OnError), `TestOnClaimerError_TransientErrorUsesKVCircuit` (connectivity => KV circuit, no stop), `TestManager_StopsItselfWhenClaimLost` (e2e revision bump => StateShutdown + consumer revoked). **CORRECTION:** `TestStableID_StaleKeyTakeover_Reclaim` is at the `stableid.Claimer` level only — it proves the new worker reclaims the stale ID; it does NOT exercise `onClaimerError`/`claimLostShutdown` at all. The brief's "C2 pinned by TestStableID_StaleKeyTakeover_Reclaim" is **imprecise**: that test pins the *reclaim* half, the `manager_claimer_error_test.go` trio pins the *self-stop routing* half. |
| **C3** | OnDegraded fires exactly once per Degraded entry per worker | `enterDegraded` CAS-guards `degradedSince` (`manager_degraded.go:309`); hook fires inside the won-CAS branch (`:326-330`). One entry = one hook. **Re-entry only after an intervening `exitDegraded` clears `degradedSince` to 0** (`:356`). | `TestManager_LiveNATSBucketLoss_OnDegradedHook`; also `TestManager_F1_BucketRecreate_TripsDegraded` asserts `require.Len(reasons,1)` for a single epoch tick |
| **C4** | `Manager.Start` returns after the synchronous sanity-check phase, not after StateStable | Async path: `Start` returns post-sanity; `monitorBucketEpochs` + recovery loops run in `m.wg.Go` afterward (`manager_startup_async.go:142`). | `TestStart_*` / `manager_startup_async*_test.go`, `manager_setup_test.go` |

**Key structural fact for the whole matrix (VERIFIED):** the Degraded↔Stable flap in
Families A and B is the *same* exit defect — `attemptRecoveryFromDegraded`
(`manager_degraded.go:376-416`) gates the exit on `currentAssignmentApplied(cur)`
(`:409`) where `cur` comes from `refreshAssignmentFromNATS` which reads **only**
`assignment.<workerID>` from the assignment bucket (`manager_assignment.go:1567-1568`).
It never checks the *failing* op (epoch mismatch in A; heartbeat/election/stableid KV op
in B) recovered. So any "exit gate" fix is the shared lever; the family-specific arming
mechanism (stale `ep.created` in A; re-accumulating heartbeat timeouts in B) is the
other half.

---

## 1. Mechanism re-derivation (independent; where I agree / disagree with 04-proof-findings.md)

### Family A (NP-2 / NP-1) — epoch fence re-fires + recover-on-wrong-signal

VERIFIED both halves:
- (i) `checkBucketEpochs` fires `enterDegraded("bucket-recreated:"+bucket)` on
  `!live.Equal(ep.created)` (`manager_setup.go:684-689`) and **never re-captures
  `ep.created`**. `captureBucketEpoch` writes `ep.created` once (`manager_setup.go:627`),
  only ever called from `ensureKVBucket` at Start (`manager_setup.go:265`). VERIFIED.
- (ii) The exit defect above runs every 1s on connection uptime
  (`manager_degraded.go:130-131`) and exits because the *assignment* bucket is intact.

I **independently confirm** the report's Family A root cause. One nuance the report
glosses (and `TestManager_F1_BucketRecreate_TripsDegraded:117-123` makes explicit):
the cached `ep.kv` handle is opened once via `m.js.KeyValue(ctx, bucket)`
(`manager_setup.go:615`). After a delete+recreate the JetStream client handle
**silently re-binds to the new stream**, so `BucketStreamCreated(ep.kv)` returns the
NEW `Created` and the mismatch is detected on every tick. If the handle did *not*
re-bind, the probe would error and `checkBucketEpochs` would `continue` (`:679-682`)
and never fire. So the re-fire depends on the handle re-binding — a real, verified
behavior the F1 test models at line 121-123. This is load-bearing for the
A:re-capture-ep.created fix (below).

### Family B (NP-3b) — connected-but-KV-unavailable false exit

VERIFIED: a `kv-unavailable`-class fault (`ErrKVUnavailable`-wrapped deadline/no-responders,
`manager_degraded.go:58-72`) on heartbeat/election/stableid leaves the connection UP and
the assignment bucket readable. Exit fires on assignment read + `currentAssignmentApplied`
(`:409-415`). `recordKVHealthyOp` cannot save it (the heartbeat op is itself faulting,
so no success clears the transient entries — `manager_degraded.go:266-288`). Confirmed.

### Family C mech 1 (NP-8) — claim-loss self-stop on outage ≥ WorkerIDTTL

VERIFIED routing: outage ≥ stableID bucket MaxAge (reconciled to `WorkerIDTTL`,
`config.go`) ages out the claim; on reconnect `onClaimerError` sees `ErrClaimLost` that
is NEITHER connectivity NOR degrading => `claimLostShutdown` => `StateShutdown`
(`manager_election.go:113-119`). This is **the same C2 path** — i.e. mech-1 is C2
firing *correctly* (the worker genuinely lost its claim), just fleet-wide. The report's
"Low / doc-only, working-as-designed" verdict is consistent with the code. AGREE.

### Family C mech 2 (NP-8) — MemoryStorage heartbeat-bucket loss => fleet flap

VERIFIED: heartbeat bucket is `MemoryStorage` (`manager_setup.go:156`), gone after a
single-node restart. The leader's calculator calls `WorkerMonitor.GetActiveWorkers`
=> `heartbeatKV.Keys` => `"failed to list heartbeat keys: %w"`
(`internal/assignment/worker_monitor.go:167-176`; same surface at `:212-219` for
`GetHeartbeats`). Note `IsNoKeysFoundError` is handled as empty (`:170-173`) — but a
**missing stream** is `ErrStreamNotFound`, NOT no-keys, so it propagates as an error.
Confirmed.

---

## 2. Candidate-fix × contract/test impact matrix

Columns: does the fix **touch** (T), **risk regressing** (R), or **collide with another
family's fix** (X) the contract/test? Blank = no interaction.

| Fix option | C1 (whole-bucket degrade) + `LiveNATSBucketLoss`, `PartialBucketLoss_HeartbeatHealthy` | C2 (peer-takeover self-stop) + `OnClaimerError_*`, `StopsItselfWhenClaimLost` | C3 (OnDegraded once) + `LiveNATSBucketLoss_OnDegradedHook`, `F1_BucketRecreate` | C4 (Start returns post-sanity) + `Start_*` | Recovery self-heal suite (`AttemptRecovery_*`, `RecoveryGuard_TornRead`) | Epoch entry (`F1_BucketRecreate_TripsDegraded`, `F1_HappyPath`) | f-d1 reset (`recordKVHealthyOp`) | Cross-family collision |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| **A: re-capture `ep.created`** after enterDegraded | | | **R** — must keep "exactly-once per *entry*"; re-capture latches so only ONE entry per recreate. F1 ticks once so it stays green, but verify multi-tick. | | | **T/R** — directly edits `checkBucketEpochs`; `HappyPath` (no false trip) must stay green | | **X** with C2:recreate-on-reconnect (a future intended recreate would NOT re-fire — good) and with C2:tolerate-empty-heartbeat (orthogonal). Removes the X that C2:recreate-on-reconnect otherwise creates. |
| **A: refuse-exit-on-mismatch** (gate `attemptRecoveryFromDegraded` on no outstanding epoch mismatch) | **R (high)** — see §3.1; a poorly-scoped "any-fault-blocks-exit" could keep C1 fine (C1 *wants* terminal Degraded) but could break legit NP-5/NP-9 recovery | | **T** — changes when exit (and thus future re-entry) happens; reduces flap to single entry | | **T/R** — this is the *shared* exit gate; the entire `AttemptRecovery_*` suite pins its current semantics (`Applied_ExitsToStable`, `LatchedVersionAdvance_Rearms`, `ColdZero_ExitsToStable`, `RefreshFails_ReturnsBeforeGuard`) | **T** — needs an "epoch mismatch outstanding" predicate read by recovery | | shares the lever with **B** (one exit-gate fix can close both A's and B's recover-half) |
| **B: post-recovery cooldown** (delay exit / require N stable ticks) | **R (low)** — C1 still trips (entry path untouched); cooldown only delays *exit*, and C1 asserts entry, not fast exit | | **R (low)** — delays exit => delays the next eligible re-entry; one-per-entry invariant intact | | possibly **T** if Start waits | **T/R** — adds time/tick gating to exit; `AttemptRecovery_*` are single-call unit tests, a cooldown that needs ≥2 calls could make `Applied_ExitsToStable` exit on the 2nd call — **rewrite risk** | | shares lever with A |
| **B: verify-failing-op-recovered** (re-probe the op that degraded before exit) | **R (medium)** — must NOT require the *assignment* op alone (that's the current bug); must probe heartbeat/election/stableid too. If it probes ALL ops it strengthens C1's *terminal* property (good) but risks never exiting on a transient | | **R (low)** — none if it only adds a probe before exit | | | **T/R (high)** — fundamentally changes the exit predicate; whole `AttemptRecovery_*` suite + `RecoveryGuard_TornRead` must be re-validated; needs to plumb "which op degraded" (reason) into recovery | | needs a reason→probe map; **X** with A (epoch mismatch is the "failing op" for A — a unified verify path could close A too) |
| **B: per-source error counters** (separate kvErrorWindow per subsystem) | **R (medium)** — `PartialBucketLoss_HeartbeatHealthy` depends on whole-bucket entries from *multiple* wiped buckets summing to threshold while heartbeat succeeds; per-source counters must still let any single faulting source reach threshold | | **R (low)** | | **T (low)** | | **T/R (high)** — rewrites `kvErrorWindow`/`recordKVHealthyOp`/`recordKVError`; `TestManager_recordKVHealthyOp` ("clears only transient", "no-op while degraded") + `TestManager_recordKVError` are the pin | the f-d1 reset semantics (`transient` flag, class-aware clear) are exactly this surface | independent of A |
| **C1(mech1): safe-reclaim** (on reconnect, re-claim same ID instead of self-stop when bucket is intact) | **R (low)** | **R (HIGH)** — see §3.2; this directly weakens the C2 self-stop. Must distinguish "my claim aged out, no peer took it" from "a peer took it" or it reopens split-brain. `OnClaimerError_ClaimLostStopsWorker` + `StopsItselfWhenClaimLost` are the guard | | | | | | **X** with C2:recreate-on-reconnect (both touch the reconnect claim path) |
| **C1(mech1): doc-only** (document M5 bounded by WorkerIDTTL) | | | | | | | | none (no code change) |
| **C2(mech2): recreate-on-reconnect** (recreate missing MemoryStorage heartbeat bucket on reconnect) | **R (medium)** — recreating a Parti-owned bucket in-process is exactly what `LiveNATSBucketLoss:128-132` forbids ("must not be auto-recreated by live workers"); a heartbeat-only carve-out must not leak to other buckets | | **R (medium)** — a recreate produces a new stream `Created` | | | **X (HIGH)** — see §3.3: a recreated heartbeat stream gets a NEW `Created`; the still-stale `ep.created` (Family A unfixed) makes the epoch fence fire `bucket-recreated:heartbeat` on the next tick => re-introduces the A flap on the very bucket this fix recreated | | **X (HIGH)** with Family A: this fix is **unsafe to land before** A:re-capture-ep.created (or it must re-capture the heartbeat epoch itself after recreate) |
| **C2(mech2): tolerate-empty-heartbeat** (calculator path treats `ErrStreamNotFound` on heartbeat Keys as empty during recovery) | **R (medium)** — `GetActiveWorkers`/`GetHeartbeats` currently map only `IsNoKeysFoundError` to empty (`worker_monitor.go:170,214`); widening to `ErrStreamNotFound` risks the leader computing an assignment over an *empty* fleet (revoke-all) during a real heartbeat-bucket *deletion* (C1/M3), which C1 wants to be Degraded, not silently absorbed | | | | | | | independent of A (no recreate, so no new `Created`); SAFER than recreate-on-reconnect w.r.t. the epoch fence |

---

## 3. The three regression/collision risks worth stopping on

### 3.1 Does a stricter exit-gate (A:refuse-exit / B:verify-op) regress C1 *or the f-d1 reset*?

**The f-d1 reset: clean no.** VERIFIED separation of paths: a stricter exit gate lives in
`attemptRecoveryFromDegraded`, and the recovery path's window reset is `recordKVSuccess`
(`manager_degraded.go:393`), a full clear. The f-d1 class-aware reset is
`recordKVHealthyOp` (`:266-288`), which runs on the *entry/clear* side (a healthy periodic
KV op while NOT degraded) and is a **no-op while degraded** (`:267`). An exit-gate change
cannot touch `recordKVHealthyOp` — different function, different trigger. The ONLY fix
option that touches the f-d1 reset is **B:per-source-counters** (it rewrites the
`kvErrorWindow`/`transient`-flag machinery), which the matrix already flags `T/R` on the
`recordKVHealthyOp` column. So the blank f-d1 cells for every other option are backed by
this path separation, not omission.

**C1: no, if scoped to the failing op; and C1 actually *benefits*.** C1 is pinned by
*entry* (`TestManager_LiveNATSBucketLoss` asserts all 3 reach Degraded; it does NOT
assert they exit). A stricter exit gate can only keep them Degraded *longer*, which is
the C1 intent ("restart/rotate", terminal). VERIFIED: `LiveNATSBucketLoss` has no
exit/recovery assertion. **The real regression surface is the recovery *positive*
tests**, not C1: `TestAttemptRecovery_Applied_ExitsToStable`,
`TestAttemptRecovery_ColdZero_ExitsToStable`, NP-5 (`startup-timeout` heals), NP-9
(full quorum loss heals after clear), NP-3a (disarm control). A gate that blocks exit
"while any error window is non-empty" would break NP-5/NP-9/NP-3a (they legitimately
recover). The gate must key on **the still-failing op**, not "any past error" — which
is exactly why B:verify-failing-op-recovered needs the degrade *reason* plumbed into
recovery (currently `attemptRecoveryFromDegraded` is reason-agnostic — VERIFIED, `:406`
comment says "keys on commitment STATE, not the degraded reason").

### 3.2 Does C1(mech1):safe-reclaim regress C2?

**HIGH risk.** C2's whole value is that a *peer takeover* self-stops exactly one worker
(`onClaimerError` => `claimLostShutdown`, pinned by `TestOnClaimerError_ClaimLostStopsWorker`
and `TestManager_StopsItselfWhenClaimLost`). "Re-claim on reconnect instead of stop"
must fire ONLY when the worker's own claim simply *aged out with no peer holding it* —
indistinguishable, at the `ErrClaimLost` surface, from a peer having taken it unless the
fix re-reads the key's current holder. Getting this wrong reopens split-brain (two
workers, same ID). The report calls mech-1 "working-as-designed / doc-only" — I AGREE
this is the safe call; a safe-reclaim is a genuine behavior change that must defend C2,
not a bug fix.

### 3.3 Does C2(mech2):recreate-on-reconnect collide with Family A's epoch fence?

**HIGH risk, and the report does NOT call this out.** VERIFIED chain:
`captureBucketEpoch` is invoked from `ensureKVBucket` for EVERY Parti-owned bucket
including heartbeat (`manager_setup.go:265`, heartbeat ensured at `:156`), so the
heartbeat bucket IS epoch-monitored. If a recreate-on-reconnect fix recreates the
MemoryStorage heartbeat bucket in-process, the new stream gets a strictly-later
`Created`; the epoch monitor still holds the original `ep.created` (Family A unfixed)
and will fire `enterDegraded("bucket-recreated:heartbeat")` on the next
`OperationTimeout` tick — re-introducing the Family A flap on the exact bucket the fix
just restored. **Sequencing constraint:** C2:recreate-on-reconnect must NOT land before
A:re-capture-ep.created, OR must itself re-capture the heartbeat epoch after recreating.
The strictly-safer mech-2 option w.r.t. this collision is
**C2:tolerate-empty-heartbeat** — it adds no new stream `Created`, so it cannot trip the
epoch fence. (It carries its own C1/M3 risk: it must not absorb a genuine heartbeat
*deletion* into "empty fleet".)

---

## 4. Recommended fix combination (lowest contract risk)

INFERRED from the matrix:
1. **Family A:** `re-capture ep.created` after `enterDegraded` (latches one entry per
   recreate; preserves C3-once and `F1_BucketRecreate`/`F1_HappyPath`) **plus** a
   reason-aware exit gate so recovery does not exit while an epoch mismatch is
   outstanding. The latch alone stops the *re-arm*; without the exit-gate the worker
   still exits once on the intact assignment bucket (false-healthy NP-1). Both halves
   are needed for NP-1's resting-state correctness.
2. **Family B:** `verify-failing-op-recovered`, sharing the reason-aware exit gate from
   (1). This is the single lever the report flags as closing both A's and B's
   recover-halves. Validate against the entire `AttemptRecovery_*` suite + NP-5/NP-9/NP-3a.

**CRITICAL framing for the reason-aware exit gate — it is ADDITIVE, not a replacement.**
The new reason/op check must be layered *on top of* the existing
`currentAssignmentApplied(cur)` commitment-state guard (`manager_degraded.go:409`), NOT
substituted for it. That commitment-state guard is a deliberate, documented choice
(the `:395-407` comment + `03-latched-worker-version-commitment-plan.md`) pinned by
`TestAttemptRecovery_LatchedVersionAdvance_RearmsAtNewVersion` and the rest of the
`AttemptRecovery_*` suite. Removing or replacing it reopens the latched-version
false-Stable bug. Exit must require BOTH: (a) the current assignment is applied+acked
(existing), AND (b) the op/condition that caused this Degraded entry has actually
recovered (new). The new gate adds a conjunct; it does not relax the existing one.
3. **Family C mech 1:** doc-only (it is C2 firing correctly).
4. **Family C mech 2:** prefer `tolerate-empty-heartbeat` (no epoch-fence collision) OR,
   if recreate-on-reconnect is chosen, sequence it AFTER A:re-capture-ep.created.

The single highest-leverage, lowest-risk change is the **reason-aware exit gate** in
`attemptRecoveryFromDegraded`: it closes the recover-half of A and all of B, and (because
C1 asserts entry not exit) cannot regress the whole-bucket-degrade contract — provided it
keys on the still-failing op/reason rather than "any non-empty error window" (which would
break the proven NP-5/NP-9/NP-3a recoveries).

---

## 5. Corrections / imprecisions found vs the brief and 04-proof-findings.md

- **C2 pinning (brief):** `TestStableID_StaleKeyTakeover_Reclaim` pins the *reclaim*
  half at the `stableid.Claimer` level; it never touches `onClaimerError`/
  `claimLostShutdown`. The manager-level C2 routing is pinned by
  `manager_claimer_error_test.go` (`TestOnClaimerError_ClaimLostStopsWorker`,
  `TestOnClaimerError_TransientErrorUsesKVCircuit`, `TestManager_StopsItselfWhenClaimLost`).
  A C1:safe-reclaim fix's regression risk lives in *those* tests, not the takeover test.
- **C2(mech2):recreate-on-reconnect × Family A collision:** not surfaced in
  04-proof-findings.md §7. VERIFIED via `manager_setup.go:265` + `:156`. This is a
  hard sequencing constraint, not a soft note.
- **04-proof-findings.md `manager_setup.go:684-690` line cites:** match HEAD
  (`checkBucketEpochs` mismatch+enterDegraded is `:684-690`). `:627` for the single
  `ep.created` capture is exact. No drift found.
- **Bucket-recreate test names (brief + 01-fault-matrix.md):** the brief lists
  `manager_epoch_fence_test.go` and `manager_bucket_recreate_test.go` as suites to anchor
  on. VERIFIED: `manager_bucket_recreate_test.go` does **not exist** (no file, no
  `*bucket_recreate*` glob match). The fault matrix M4 row (`01-fault-matrix.md:12`) cites
  `TestManager_BucketRecreated_EntersDegraded`, which also does not exist by that name. The
  real M4/epoch-entry tests are `TestManager_F1_BucketRecreate_TripsDegraded` and
  `TestManager_F1_HappyPath_NoDegraded` in `manager_epoch_fence_test.go`. Any Family A fix
  validates against F1, not a `BucketRecreated`-named test.
- **NP-2 epoch-fence handle re-bind:** the report's root cause is correct but omits
  *why* the probe keeps succeeding after recreate (the cached `ep.kv` handle silently
  re-binds to the new stream — modeled in `F1_BucketRecreate:117-123`). This matters
  because an alternative "the handle errors so it never re-fires" reading would falsify
  the gap; the handle-rebind behavior is what makes Family A real.
