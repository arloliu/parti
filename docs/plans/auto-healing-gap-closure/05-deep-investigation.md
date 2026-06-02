# Auto-Healing Gap — Deep Investigation (Synthesis)

- **Date:** 2026-06-01
- **HEAD:** `2453306` on worktree `auto-heal-gap-investigation` (content-identical to `main`).
- **Scope:** READ-ONLY. No production code was changed. This report synthesizes 5
  family/exit-gate findings (each independently re-derived AND adversarially verified) plus
  2 cross-cutting surveys. Every claim cites `file:line` or a test name; INFERENCE is flagged
  as such.
- **Governing rule used throughout:** where an investigation note's headline and its
  adversarial verification note diverge, **the verification conclusion governs.** Several
  load-bearing recommendations below differ from the louder first-pass headline for this
  reason (Family A fix form; C-mech1 fix reasoning; C-mech2 decision gate). These are called
  out explicitly.
- **Detailed source notes** (read for full depth) live under
  `docs/plans/auto-healing-gap-closure/investigation-notes/`:
  `family-a-epoch.md`(+`-verify`), `family-b-kvunavail.md`(+`-verify`),
  `family-c-mech1.md`(+`-verify`), `family-c-mech2.md`(+`-verify`),
  `exit-gate-unification.md`(+`-verify`), `survey-contracts.md`, `survey-completeness.md`.
- **Cross-model review (2026-06-01):** an independent reviewer (codex `gpt-5.5`, reasoning `xhigh`,
  read-only) verified every load-bearing claim against the code and returned **sound-with-fixes**,
  agreeing with all mechanisms (A re-probe, B reason-scoped gate, C-mech1 doc-only, C-mech2 Opt D,
  the exit-gate refutation, and NP-10). Its corrections — one factual error in §4 and three
  precision-of-wording points (C-mech1 §1, C3 §4, NP-10 §5/§7) — were independently re-verified and
  folded in below, tagged "(cross-model review)". Review artifact: `tmp/codex-review-last.md`.

---

## 0. Method + current-HEAD reproduction status

Method: each gap is an executable env-gated proof; the investigator re-ran the failing
proofs and the controls **serially** on HEAD `2453306`. The `.out` evidence is under
`tmp/repro-current-head/` (`SUMMARY.txt` + per-proof `.out`). The deep investigation then
independently re-derived each root cause from code, ran an adversarial refutation pass, and
mapped contract/test impact.

**Every confirmed gap reproduces on HEAD `2453306`. No expected-FAIL proof secretly PASSED.**

| Proof | Property | Verdict on HEAD 2453306 | Evidence |
|---|---|---|---|
| **NP-2** | one bucket recreated under a live worker must stay *terminally* Degraded | **FAIL (gap)** `degradedToStable=9, otherDegrades=0, finalState=Degraded, connected=true` | `np2.out:4,7` |
| **NP-1** | operator wipes+recreates **all** buckets under live 3-worker fleet must not heal in-process | **FAIL (gap)** `healed=[0 1 2]`, all end Stable on empty buckets, ~67s | `SUMMARY.txt:7`, `np1.out` |
| **NP-3b** | sustained connected-but-KV-unavailable must not falsely exit Degraded | **FAIL (gap)** `re-entries=9, injected=36`, CONNECTED, 13.0s | `np3b.out:4,8` |
| **NP-8 mech1** | 3-mgr fleet across NATS restart, outage ≥ WorkerIDTTL | **FAIL (gap)** `manager[1] failed to reach Stable: context deadline exceeded`, 40.3s | `np8_mech1.out:8` |
| **NP-8 mech2** | 3-mgr fleet, MemoryStorage heartbeat-bucket loss | **FAIL (gap)** reaches all-Stable then HOLD trips ~12s, CONNECTED | `np8_mech2.out` (`:325` reach passes, `:327` HOLD fails) |
| NP-3a | KV-unavailable then **cleared** → recovers and holds (positive control) | **PASS** 9.68s | `np3a.out` |
| NP-6 / NP-7 | finite-reconnect close / unlimited-reconnect single-worker recover | **PASS** | `np6_np7.out` |

The NP-3a PASS is decisive: it proves NP-3b's exit is a *false* exit, not a dead recovery
path. The NP-2/NP-3b CAS argument is irrefutable — `enterDegraded` CAS-guards `degradedSince`
(`manager_degraded.go:309`), so N Degraded entries require N−1 intervening `exitDegraded`
calls; 9–10 entries ⇒ a genuine oscillation, not a single transition.

---

## 1. Per-family deep findings

### Family A — epoch-fence re-degrade + recover-on-wrong-signal flap (NP-2, NP-1)

**Verdict: CONFIRMED (root cause) + CORRECTED (report fix framing) + CAVEATED (recommended
fix form).**

**Root cause, re-derived (two interacting facts):**
- **(i) The epoch fence never re-captures `ep.created`.** `captureBucketEpoch` writes the
  cached Created **once** at Start (`manager_setup.go:627`; only writers of `m.bucketEpochs`
  are the map-init `:613` and `:627` — verified by grep). `checkBucketEpochs`
  (`manager_setup.go:669-693`, read directly) reads the **live** Created via the cached probe
  handle (`:677`), fires `enterDegraded("bucket-recreated:"+bucket)` on `!live.Equal(ep.created)`
  (`:684,689`), then returns (`:690`) — **never updating `ep.created`.** The mismatch is
  permanent for the process lifetime; every tick re-fires. (The cached `ep.kv` handle
  transparently re-binds to the recreated stream — proven by NP-2 firing 10×; if the stale
  handle errored, `:679-682` would `continue` and the fence would never fire.)
- **(ii) Recovery exits on the wrong signal.** `attemptRecoveryFromDegraded`
  (`manager_degraded.go:376-416`, read directly) reads ONLY `assignment.<workerID>`
  (`refreshAssignmentFromNATS` → `manager_assignment.go:1567-1568`) and gates exit solely on
  `currentAssignmentApplied(cur)` (`:408-415`). The comment at `:405-406` confirms this is
  **reason-agnostic by design** ("keys on commitment STATE, not the degraded reason"). It
  **never consults `m.bucketEpochs`** — the wipe signal is structurally invisible to the exit.

The 10s epoch tick and the 1s recovery tick fight → sustained Degraded↔Stable flap, violating
the documented terminal-Degraded contract (`docs/OPERATIONS.md:126,750-762`: bucket-recreated ⇒
rotate; Parti deliberately does not self-heal). Orthogonal to `421f13c`/`recordKVHealthyOp`
(the fence calls `enterDegraded` directly, never touching `kvErrorWindow`).

**Why NP-1 RESTS Stable (not just flaps) — added, the report omits this.** The publisher's
in-memory `currentVersion` (`assignment_publisher.go:106,345`) is only ever **raised** by
`DiscoverHighestVersion` (`:882-884,917-919`), never lowered. After the wipe the leader keeps
its pre-wipe version and republishes a *higher* version into the empty bucket; the worker
genuinely re-applies it, satisfying `currentAssignmentApplied` against actually-committed-but-
on-wiped-coordination-state data. The false-healthy is **structural and version-monotonic**,
not a counter-reset artifact (`np1.out`: worker-0 shows `Stable→Rebalancing→Stable` post-wipe).
This is the worst outcome: a readiness probe marks the pod Ready while handoff claims, commit
history, and the worker-ID lease were wiped.

**Fix options:**

| Option | Mechanism | Passes NP-2/NP-1? | Notes |
|---|---|---|---|
| **A-reprobe** (RECOMMENDED) | In `attemptRecoveryFromDegraded` before `exitDegraded`, `AND` `!epochMismatchOutstanding()`, a live re-probe of each `m.bucketEpochs` entry against cached `ep.created`. Keep `ep.created` permanently stale (load-bearing). | **Yes (both)** | **No pre-arm window** — `ep.created` is pinned, so any post-recreate exit attempt refuses inline. Cost: one `BucketStreamCreated` probe per bucket per recovery tick while Degraded + a probe-error policy decision (`checkBucketEpochs:679-682` currently swallows probe errors with `continue`). |
| **A-latch** | Latch `m.epochFenceTripped` at `manager_setup.go:689`, `AND !tripped` into the exit gate. | **NP-2 yes; NP-1 NOT established** | **Verification caveat (governs):** the latch arms only on the first post-recreate fence *observation*, which ticks every **10s** in NP-1 (`OperationTimeout` default `config.go:410`, NOT overridden), while recovery runs every **1s** and the leader republish is not Degraded-gated. A ~10s pre-arm window exists where a higher-version republish + 1Hz recovery can exit before the latch arms → risks failing `require.Empty(healed)` (`np1_..._test.go:248`). Use only if armed before any post-recreate exit. |
| **A-recapture (Opt-1)** | Re-capture `ep.created` after firing so the fence latches once. | **Fails BOTH alone** | Standalone it CEMENTS false-healthy: stops re-degrade, worker rests Stable on the wiped bucket → strictly WORSE than the flap (removes the Degraded windows a readiness probe needs). Only acceptable layered on a recovery-exit guard, where it merely quiets log spam — and is likely **unnecessary** because a never-exiting guard keeps `degradedSince` set so `enterDegraded`'s CAS self-suppresses repeats. |

**Recommended fix:** **A-reprobe** — gate `exitDegraded` on a live `epochMismatchOutstanding()`
re-probe, preserving the permanently-stale `ep.created` as the terminal-Degraded signal.
Legitimate recovery is via process restart (re-runs `ensureKVBucket → captureBucketEpoch`,
`docs/OPERATIONS.md:760-762`). **The fix's test must assert NP-1's FIRST exit is blocked**, not
merely that the worker eventually settles Degraded.

**Report discrepancies (all verified):**
- `04-proof-findings.md:278` "the proofs are agnostic to which fix lands" is **FALSE** — Opt-1
  alone fails both proofs; only an exit-gate fix passes.
- `:275-282` presents Opt-1/Opt-2 as co-equal "Either…or…" alternatives. They are not: the
  exit-gate fix is necessary; Opt-1 standalone is strictly worse than status quo.
- `:51-52,280-282` "a single fix could close both A and B" — same *function*, **different
  predicates** (see §2).

**Residual risk:** A-reprobe's only cost is the per-tick probe + a probe-error policy decision
(refuse-exit-on-probe-error is conservative and matches the M4 terminal intent). The latched
variant's NP-1 sufficiency was NOT settled empirically (read-only; no patched-build run). C1
entry untouched (`np1.out` shows `kv-unavailable` first on every worker, then `bucket-recreated:*`).

---

### Family B — recover-on-wrong-signal under sustained connected-but-KV-unavailable (NP-3b)

**Verdict: CONFIRMED (root cause) + CORRECTED (report "single fix closes A+B" is loose).** This
is the explicitly-deferred "Finding A" from the F-D1 work made executable.

**Root cause, re-derived.** Recovery is driven by connection **uptime**, not the failing op:
`monitorNATSConnection` ticks 1s (`manager_degraded.go:79`) → `checkConnectionHealth` (`:97`);
the connection never drops under a KV-unavailable fault, so once up for `ExitThreshold` it calls
`attemptRecoveryFromDegraded` every tick. That exits on a successful assignment read +
`currentAssignmentApplied` — **never checking the heartbeat/election/stableid op that triggered
the degrade recovered**. NP-3b excludes the assignment bucket from the fault set
(`np3_..._test.go:142-147`), so the read succeeds → false exit. `recordKVSuccess` (`:393`) then
wipes the entire `kvErrorWindow`, resetting the re-degrade clock; the still-faulting heartbeat
re-accumulates to `KVErrorThreshold` → re-degrade → flap. `recordKVHealthyOp` (421f13c) cannot
help: it fires only on heartbeat Put **success** (the heartbeat bucket is itself faulting) and
early-returns while degraded (`:267-269`).

**NP-9 contrast (verified):** NP-9 faults the assignment bucket AND its Watch
(`np9_..._test.go:184-189,142-148`), so `refreshAssignmentFromNATS` FAILS at `:383` → early
return → never reaches `exitDegraded`. That is why NP-9 recovers cleanly (1 entry / 1 exit) and
does not flap. The defective gate is *accidentally correct* in NP-9 because the assignment
bucket is part of the same quorum loss.

**The crux for the fix (the report misses this): there is no off-the-shelf "failing op" signal.**
`kvErrorWindow` is a flat `[]kvErrorEvent{at, transient}` (`manager.go:204`,
`manager_degraded.go:33-36`) with **no source tag**; the degrade reason is passed transiently to
`enterDegraded` (`:300`) and **never persisted**. The only per-source success signal in
production is the heartbeat `SetOnSuccess` (`manager_election.go:432`).

**Fix options:**

| Candidate | Mechanism | Passes NP-3b `require.Zero`? | Notes |
|---|---|---|---|
| **2 — reason-scoped heartbeat-success-since-degrade gate** (RECOMMENDED B-only) | New `m.lastHeartbeatSuccessAt` stamped unconditionally on heartbeat Put success; new `m.lastDegradedReason` set in `enterDegraded`; gate `if reasonIsKVUnavailable && lastHeartbeatSuccessAt <= degradedSince { return }`. | **Yes** | **Reason-scoping is load-bearing:** `attemptRecoveryFromDegraded` is the SINGLE recovery exit for ALL reasons; an UNCONDITIONAL gate **regresses NP-5** (VERIFIED — `newTestManager` never starts a heartbeat publisher, `manager_commit_state_machine_test.go:163`; NP-5 calls `attemptRecoveryFromDegraded` directly `np5...:158` expecting Degraded→Stable). So it is **not a one-liner** — it forces persisting the degrade reason that does not exist today. |
| 1 — post-recovery cooldown | Suppress re-entry for N seconds after exit. | **No** (allows ≥1 false exit; `require.Zero` is zero exits, not slower) | Also risks C1 bounded re-entry if not class-scoped. Reject as standalone. |
| 3 — per-source error counters / degrade-cause registry | Tag `kvErrorEvent` by source; add success hooks to election/stableid/assignment. | Yes | 100+ LOC across ≥4 files + subsystems with no success hook today. Heaviest; the honest unified A+B substrate (epoch fence registers a `bucket-recreated` cause). |

**Recommended fix (B-only):** **Candidate 2, reason-scoped.** Smallest correct surface that
passes NP-3b, preserves C1 (entry contract; whole-bucket loss enters with reason `"KV error
threshold exceeded"` `:215`, not `kv-unavailable`, so the gate does not apply), preserves the
f-d1 class-aware reset (separate signal), keeps NP-9/NP-3a recovery, and does NOT regress NP-5.

**Report discrepancy:** `04-proof-findings.md:52` "a single fix to the exit gate could close both
A and B" is **loose** — the exit *defect* is shared, but the re-degrade *trigger* and thus the
fixing *predicate* differ (B: kv-unavailable threshold; A: epoch fence fired directly,
bypassing `kvErrorWindow`). The report is internally inconsistent: `:52` oversells, `:282` walks
it back. (Numeric drift, not a correctness issue: report cited `degradedExits=9, injected=34`;
HEAD re-run shows `re-entries=9, injected=36`.)

**Residual risk (verified):** (1) **Ordering hazard** — `m.lastDegradedReason` must be set BEFORE
or atomically with the `degradedSince` CAS (`:309`), else the 1s tick can read a stale/empty
reason and allow one false exit on the first degrade. Mitigate by ordering or treating
empty-reason as gate-applies. (2) Heartbeat-only proxy: an election-only/stableid-only sustained
fault with healthy heartbeat could open the gate early — argued (not proven) to be already let
through by the shipped f-d1 narrowing, so no *new* regression. (3) B-only; does not close A.
Fix not prototyped; mandatory on landing: ungate NP-3b + re-run ungated NP-5, NP-9,
`LiveNATSBucketLoss` (ideally `-race`).

---

### Family C mechanism 1 — claim-loss self-stop on outage ≥ WorkerIDTTL (NP-8)

**Verdict: CONFIRMED (mechanism, WAD direction) + CORRECTED (boundary magnitude, fleet outcome,
C2 pin) + CAVEATED (fix reasoning + attribution evidence).**

**Root cause, re-derived.** The stableID bucket MaxAge is reconciled to `WorkerIDTTL`
(`config.go:366-369`; `reconcileStableIDBucketMaxAge`, `manager_setup.go:354-373`; bucket is
FileStorage `:92`). Renewal runs at `max(ttl/3,100ms)` (`claimer.go:491-493`) = 25s at the 75s
default; the server purges the key at `last-renewal+75s`. During an outage renewal Updates fail
with connectivity errors routed harmlessly to `recordKVOpError`. When the outage exceeds
`WorkerIDTTL` the key is purged; on reconnect the next `kv.Update(key,val,staleLastRevision)`
hits "wrong last sequence: 0" → nats.go surfaces `jetstream.ErrKeyExists` (wire pinned by
`TestClaimer_WireContract_RevisionMismatchIsErrKeyExists`, `claimer_test.go:650-681`). `renew()`
returns **bare** `ErrClaimLost` (`claimer.go:365-368`) with no connectivity/not-found sentinel.
`onClaimerError` (`manager_election.go:106-127`, read directly): `ErrClaimLost` true,
`IsConnectivityError`/`IsDegradingJetStreamError` both false → `claimLostShutdown` (`:118`) →
terminal `StateShutdown` (NP-4 confirms a second `Start` returns `ErrAlreadyStarted`).

The **conflation locus** (`claimer.go:365-368`) collapses "a peer bumped my revision (key
present)" and "my lease aged out (slot empty)" into the identical bare error, routed identically.
The code *could* distinguish them with a follow-up `kv.Get` (absent ⇒ expired-no-peer;
fresh-value+different-revision ⇒ peer holds it) but does not attempt it. In NP-8 every worker is
disconnected, so **no peer can have taken over**, yet all 3 self-stop — the pure unnecessary-loss
case.

**Fix options:**

| Option | What | Risk |
|---|---|---|
| **0 — doc-only** (RECOMMENDED near-term) | Document M5 recover-to-Stable is bounded by WorkerIDTTL; an outage past it self-stops the worker (StateShutdown) ⇒ orchestrator rotation. Re-purpose the NP-8 mech-1 proof to assert StateShutdown so it becomes a regression guard. | None to code. |
| 1 — ID-layer safe re-claim alone | `kv.Get` before self-stop; atomic `kv.Create` if absent. | **Do NOT ship alone** — resuming from stale cached assignment may double-process partitions the fleet rebalanced (in a *partial* outage). |
| 2 — full in-process re-claim + assignment re-bootstrap | Re-claim + drop cache + re-enter `waitForAssignment`. | Only behaviorally-safe in-process fix; re-implements restart semantics; needs a new data-plane proof. |
| 3 — raise effective trigger via config guidance | Set WorkerIDTTL above worst-case outage. | Palliative; relocates the boundary. |

**Recommended fix:** **Option 0 (doc-only)** near-term, with corrected framing. Never ship Option
1 in isolation.

**Corrections (verified):**
- **Boundary is minute-scale (~50-75s at defaults), NOT "multi-minute"** (report `:160,251,286`).
  Renewal cadence 25s; purge at last-renewal+75s ⇒ phase-dependent ~50-75s. Materially more
  reachable than implied.
- **Operational outcome is fleet-wide.** The mechanism is per-worker, but all disconnected
  workers cross the boundary together and all self-stop for zero ID contention — one ~1-minute
  blip rotates the entire fleet.
- **C2 pin mis-cited.** The brief cites `TestStableID_StaleKeyTakeover_Reclaim`; that test is at
  the `stableid.Claimer` level (reclaim half only) and does NOT exercise `onClaimerError`
  self-stop routing. The real pins are `TestManager_StopsItselfWhenClaimLost`
  (`manager_claimer_error_test.go:135-172`) and `TestOnClaimerError_ClaimLostStopsWorker` (`:53-86`).

**Caveats from adversarial verification (governing):**
- **Fix-reasoning correction.** The investigation's split-brain steelman ("heartbeat expired
  ~35-60s earlier ⇒ fleet already rebalanced ⇒ Option 1 double-processes") is **FALSE for the
  NP-8 full-fleet case**: with every worker (incl. the leader) disconnected, no successful
  worker-enumeration or rebalance can run. (cross-model review precision: a leader's calculator may
  still be *running* — `manager.go:607`, `manager_election.go:329` — but `heartbeatKV.Keys` fails
  (`worker_monitor.go:167-175`), so it cannot enumerate workers or rebalance during the outage; the
  earlier "no calculator runs" phrasing was imprecise.) Nothing rebalances during the outage. The double-processing risk exists only in a **partial** outage (one worker partitioned,
  leader survives). The doc-only **conclusion survives** on the weaker correct grounds
  (orchestrator rotation heals cleanly; an in-process re-claim needs an unproven partial-outage
  data-plane proof), but the **justification must be re-grounded** away from full-fleet split-brain.
- **Attribution is INFERENCE.** The gated proof instruments only `OnStateChanged`/`OnDegraded`
  (no Shutdown/OnError hook) and runs **both** mechanisms simultaneously (WorkerIDTTL=5s AND
  MemoryStorage heartbeat loss). Mech-1 attribution rests on the cross-test WorkerIDTTL contrast
  (5s never-Stable vs 30s reaches-Stable-then-flaps) — sound but an inference. The empirical
  "bare ErrClaimLost fired" chain rests on a single un-captured diagnostic (`04-proof-findings.md:135`,
  absent from all `.out` files). The wire-contract test covers the *present-key* revision-mismatch,
  not the exact *purged-key* "wrong last sequence: 0" case. Low residual risk (MaxAge purge is the
  documented design premise), not zero. **Recommend adding StateShutdown/OnError instrumentation.**

---

### Family C mechanism 2 — MemoryStorage heartbeat-bucket loss → fleet flap (NP-8)

**Verdict: CONFIRMED (gap is real, flap as severe as claimed) + CORRECTED (report MISNAMES the
driver, so its Opt B is ineffective).**

**Root cause, re-derived.** After a single-node NATS restart only the MemoryStorage heartbeat
bucket's stream is gone (`manager_setup.go:156`, verified); election/assignment/stableid/handoff
are FileStorage and survive. Two ticks fight:
- **Re-degrade driver = the heartbeat PUBLISHER Put**, not the calculator. Every worker runs a
  heartbeat publisher (`startHeartbeat`, `manager.go:602`, leadership-independent). The cached-handle
  Put fails against the dead stream → `onError` → `recordKVOpError` (`manager_election.go:424`) →
  `recordKVError` → after `KVErrorThreshold` (5) within `KVErrorWindow` re-`enterDegraded` (~2.5s
  at the 500ms test interval). **This fires on every worker** — the gap is fleet-wide, not
  leader-specific.
- **Recovery tick = `attemptRecoveryFromDegraded`**, which reads only the surviving FileStorage
  assignment bucket and exits to Stable — the **same recover-on-wrong-signal exit defect** as
  Families A/B. The exit gate NEVER reads the heartbeat bucket.

**Driver mis-attribution (substantive, verified by elimination):** the calculator's "failed to
list heartbeat keys" error (`internal/assignment/worker_monitor.go:175`) is **swallowed** — poll
path logs Error (`:282-284`), watcher path logs Error (`:396`), audit logs Debug (`calculator_audit.go:60-63`);
grep of `recordKVError|recordKVOpError|SetOnError` over `internal/assignment/*.go` returns nothing.
A full `enterDegraded(` caller enumeration leaves only `recordKVError` (threshold) able to fire
post-reconnect on heartbeat-only loss, and only the publisher Put feeds it. The report names the
loudest **log line**, not the driver. (Reason string is likely `kv-unavailable` via
`nats.ErrNoResponders` on a cached Put, `source/nats_kv.go:1381-1383`, not "KV error threshold
exceeded" — flap is identical either way.)

**Fix options:**

| Opt | Mechanism | Verdict |
|---|---|---|
| **D — heartbeat bucket → FileStorage** (`manager_setup.go:156`) | Stream survives restart: no flap, no epoch trip, no recreate race, **no Family A coupling**. One line. | **Decision-gated on IOPS.** If the IOPS measurement clears, **Opt D dominates** all other options. Do NOT transfer M1.9's "IOPS-free" finding — that is the **election** bucket; heartbeat is the highest-frequency KV op (M2.A flags per-op state file as dominant cost). **Run the IOPS measurement FIRST.** |
| **C — gate the recovery exit on heartbeat-op health** | Shared with the Families A/B exit-gate fix. | Minimal *correctness* fix, but **alone leaves the fleet stuck-Degraded** (MemoryStorage never returns on its own) → **fails the proof's reach-all-Stable assertion** (`np8_..._test.go:325`). Requires a product/test-contract decision (auto-heal vs fail-safe-hold), not a mechanical ungate. |
| **A/B′ — recreate the bucket on reconnect** | Restores auto-heal so Opt C passes the existing proof. | **Trips Family A's currently-dormant heartbeat epoch fence** (`checkBucketEpochs:679-682` `continue` on probe error today; a recreate gives a new `Created` → `enterDegraded("bucket-recreated:heartbeat")` fleet-wide). **Cannot land independently of Family A's fix** — must re-capture/re-probe the heartbeat epoch atomically. |
| **B (report's alt) — calculator tolerates empty heartbeat list** | — | **REJECT.** Fixes a non-driver; flap persists. Also empty≠missing (empty already tolerated `worker_monitor.go:170-173`; the failure is a *missing stream* `:175 → calculator.go:1213`). |

**Recommended fix:** **Run the IOPS measurement first** (the real decision gate). **If it clears,
Opt D** (one line, decoupled from Family A). **If not, Opt C** (gate the exit) + **Opt A/B′**
(recreate the bucket), co-designed with Family A's epoch-fence fix, OR Opt C alone with docs
("MemoryStorage heartbeat loss requires re-provision/rotation").

**Residual risk:** Driver attribution is code-only, not test-captured (NopLogger
`testutil/nats.go:306`; empty hooks `np8_..._test.go:293`) — add a reason-capturing OnDegraded
hook to make it test-backed. **RF3 topology remains the single biggest open uncertainty:** a real
RF3 rolling restart that keeps replicated MemoryStorage alive may not hit mech-2 (severity drops
to Low) — unmeasured (see §6).

---

## 2. The exit-gate unification question (adversarial verdict)

**Task instruction honored: do not default to the tidy "one fix".**

**Verdict: the report's §1 claim splits in two —**
- **S1 (shared SYMPTOM): CONFIRMED.** Both A and B exit through the trigger-blind
  `attemptRecoveryFromDegraded` on a healthy assignment read while a different trigger persists.
  Verified: the function reads neither `m.bucketEpochs` (A's state) nor any per-source KV-op
  health (B's state); `currentAssignmentApplied` is a commitment gate, reason-agnostic by design
  (`manager_degraded.go:405-406`).
- **S2 (shared FIX — "a single fix to the exit gate closes both"): REFUTED.** The families need
  **separate, independent** predicates (at most coordinated-but-separate).

**The decisive refutation — the timeout-interpretation conflict (framing-proof, not predicate-
phrasing).** A's epoch detector treats an op timeout as **non-actionable**: `checkBucketEpochs`
logs "probe failed; relying on next tick" and `continue`s WITHOUT degrading (`manager_setup.go:679-682`,
read directly); only a successful read with a *different* Created is actionable. B's KV-op
detector treats the **same timeout as THE fault**: `markKVUnavailable` wraps
`context.DeadlineExceeded`/`nats.ErrNoResponders` into `ErrKVUnavailable`
(`manager_degraded.go:67-69`) which `recordKVError` accumulates. A single unified "re-validate
every bucket before exit" routine must either block-on-timeout (B-correct, but wedges A on a
transient probe error — the exact false-positive `:679-682` avoids) or ignore-on-timeout
(A-correct, but B's fault becomes invisible and NP-3b still false-exits). **No single
timeout-interpretation is correct for both** ⇒ two mechanisms with opposite error-handling.

Three reinforcing reasons:
1. **Structurally disjoint triggers.** A enters via `enterDegraded("bucket-recreated:<b>")` fired
   directly from `checkBucketEpochs` (`manager_setup.go:689`), never touching `kvErrorWindow`
   (NP-2 hard-codes this isolation: `KVErrorThreshold` default, `otherDegrades==0`
   `np2_..._test.go:81-83,190-192`). B enters entirely via `recordKVError`/`kvErrorWindow`.
2. **A KV-op-recovered gate is structurally BLIND to A** — a bucket recreate is never a KV-op
   error, so there is no "failing op" to verify recovered.
3. **Opposite desired outcomes.** B must AUTO-HEAL once the op clears (NP-3a PASSES). A must
   NEVER auto-heal — terminal Degraded for pod rotation (NP-2 asserts `finalState==StateDegraded`).

**Conclusion:** A+B is **one function edited, TWO independent predicates** (A: epoch-mismatch
outstanding; B: failing-op-recovered), **ADDITIVE to the existing `currentAssignmentApplied`
guard — never a replacement** (survey-contracts §4; the commitment guard is pinned by
`TestAttemptRecovery_LatchedVersionAdvance_RearmsAtNewVersion` and removing it reopens the
latched-version false-Stable bug). A shared PR for review economy is fine; the conjuncts are
independently testable and independently necessary. **Do NOT read "one fix closes both" as "one
predicate closes both."**

---

## 3. Cross-family interactions

| Interaction | Severity | Detail |
|---|---|---|
| **C-mech2-recreate × Family A epoch fence** | **HIGH (the headline)** | `captureBucketEpoch` runs for EVERY bucket incl. heartbeat (`manager_setup.go:265,156`). The fence is **dormant on heartbeat today** (probe errors → Debug+`continue`, `:679-682`). Any in-process recreate of the heartbeat bucket gives a new `Created`, **activating the dormant fence fleet-wide** (`enterDegraded("bucket-recreated:heartbeat")` `:684-690`) — trading the heartbeat flap for an epoch-fence flap. **A C-mech2 recreate fix CANNOT land before Family A's epoch fix** (or must re-capture/re-probe the heartbeat epoch atomically). **Opt D (FileStorage) avoids this entirely** — no new stream Created. |
| A & B share `attemptRecoveryFromDegraded` exit block | medium | Both fixes edit `manager_degraded.go:408-415`; independent ANDed conjuncts, no shared state/ordering. |
| C1 (whole-bucket loss) vs A/B exit guards | medium | Strongest regression-test surface. C1's recover-able path FAILS the assignment Get in `refreshAssignmentFromNATS` and bails at `:383-387` **before** either guard — verified by tracing `TestManager_LiveNATSBucketLoss` (wipes assignment bucket, asserts entry only, no exit assertion). Re-run C1 `-race` to confirm the guards are not reached on the recoverable path. |
| C1(mech1) safe-reclaim × C2 self-stop | **HIGH (if pursued)** | A re-claim-on-reconnect fix must fire ONLY when the claim aged out with no peer holding it; indistinguishable at the `ErrClaimLost` surface from a peer takeover unless it re-reads the current holder. Getting it wrong reopens split-brain. (Doc-only avoids this.) |
| Broad re-provision × C2 (X2) | medium | A broad reconnect re-provision of the **stableID** bucket could reclassify a legitimate peer takeover's `ErrClaimLost` as degrading-JetStream → degraded-and-ride instead of self-stop. Scope any C-mech2 fix to the heartbeat bucket only. |
| f-d1 `recordKVHealthyOp` (421f13c) | none | Entry/clear side, no-op while degraded (`manager_degraded.go:267`); exit uses `recordKVSuccess` (`:393`) — different function. Exit-gate fixes cannot touch the f-d1 reset (only B-candidate-3 would). |

---

## 4. Contract & test-impact matrix (from survey-contracts)

Contracts pinned by (corrected): **C1** `TestManager_LiveNATSBucketLoss` /
`TestManager_PartialBucketLoss_HeartbeatHealthy`; **C2** `TestManager_StopsItselfWhenClaimLost`
/ `TestOnClaimerError_ClaimLostStopsWorker` (NOT `TestStableID_StaleKeyTakeover_Reclaim`, which is
claimer-level reclaim only); **C3** `TestManager_F1_BucketRecreate_TripsDegraded` (the strongest
once-per-entry pin — single-tick `require.Len(reasons, 1)`,
`manager_epoch_fence_test.go:125-135`) / `TestManager_LiveNATSBucketLoss_OnDegradedHook` (collects
`clusterSize` reasons but does not itself assert no-duplicates after collection); **C4**
`manager_startup_async*_test.go`.

Note (corrected by cross-model review): the test NAME `TestManager_BucketRecreated_EntersDegraded`
(brief / `01-fault-matrix.md:12`) **does not exist** — the real epoch-entry tests are
`TestManager_F1_BucketRecreate_TripsDegraded` / `TestManager_F1_HappyPath_NoDegraded` in
`manager_epoch_fence_test.go`. The earlier claim that the FILE
`test/integration/manager/manager_bucket_recreate_test.go` does not exist was **WRONG**: that file
**does** exist but contains `TestManager_Restart_AfterNATSBucketLoss` (`:44`) — a restart-recovery
test, not an epoch-entry test.

T=touches, R=risk-regress, X=cross-family collision; blank=no interaction.

| Fix | C1 | C2 | C3 | C4 | Recovery `AttemptRecovery_*` suite | Epoch entry (F1) | f-d1 reset | Cross-family |
|---|---|---|---|---|---|---|---|---|
| A: re-probe / refuse-exit-on-mismatch | R (C1 wants terminal; guard not reached on recover-able path — verified) | | T (exit/re-entry timing; terminal ⇒ 1 entry) | | T/R (shared exit block) | T (needs "mismatch outstanding") | | shares exit block with B |
| A: re-capture `ep.created` | | | R (one entry per recreate; F1 ticks once) | | | T/R (edits `checkBucketEpochs`; F1 HappyPath must stay green) | | removes the X that recreate-on-reconnect creates |
| B: reason-scoped heartbeat-success gate | R med (must not regress C1 entry; reason-scoped so it does not apply to "KV error threshold exceeded") | R low | R (fewer fires, once-per-entry) | | T/R high (changes exit predicate; reason plumbed) | | | X with A (same block, ADDITIVE) |
| B: per-source counters | R med (PartialBucketLoss sums multi-bucket entries) | R low | | T low | T high (rewrites `kvErrorWindow`) | | T/R high (the f-d1 surface) | independent of A |
| C1(mech1): safe-reclaim | R low | **R HIGH** (split-brain) | | | | | | X with reconnect claim path |
| C1(mech1): doc-only | | | | | | | | none |
| C2(mech2): recreate-on-reconnect | R med (`LiveNATSBucketLoss` forbids in-process recreate) | | R med (new Created) | | | **X HIGH** (re-arms epoch fence) | | **X HIGH** with Family A — land AFTER A's fix |
| C2(mech2): heartbeat→FileStorage (Opt D) | R (changes PartialBucketLoss semantics) | | | | | | | independent of A; IOPS gate |

**Single highest-leverage, lowest-risk lever:** the reason/epoch-aware exit gate in
`attemptRecoveryFromDegraded` (two ADDITIVE conjuncts) — it closes the exit half of A and all of
B and cannot regress C1 (which asserts entry, not exit) or the f-d1 reset (different function),
provided it keys on the still-failing op/epoch rather than "any non-empty error window".

---

## 5. Completeness — the attempt to break §4 "no additional gap"

**§4's claim is breakable.** New gap **NP-10 — leader-side silent worker-enumeration stall**
(severity High, **confidence Medium** — code-derived, NOT harness-proven):

If the heartbeat-bucket **`Keys` scan times out** (`DeadlineExceeded`) while the worker's own
single-key heartbeat **`Put` keeps succeeding** (a realizable asymmetry: stream-wide scan vs
single-subject append under partial quorum/load), then `WorkerMonitor.GetActiveWorkers` wraps it
as "failed to list heartbeat keys" (`worker_monitor.go:175`); `Calculator.getActiveWorkers` only
caches/degrades for `IsConnectivityError` (`calculator.go:1197-1213`) and returns a bare error
otherwise (`:1213`); the poll loop logs and continues (`worker_monitor.go:282-284`). The
calculator has **no wiring into the degraded circuit**. Empirically (scratch test, removed):
`IsConnectivityError(context.DeadlineExceeded)` is **false** (bare and wrapped),
`IsDegradingJetStreamError` is **false** — so the scan deadline is classified by neither and is
logged-only. Result: a leader holds `StateStable` (own Put/assignment-read/election-renew all
succeed) while **blind to worker topology**, serving assignments from stale/empty membership — a
**false-healthy leader**. This is the leader-side analog of NP-3b but with NO circuit, so it is
**silent (does not even flap)**. Distinct from C-mech2 (a *missing* stream `ErrStreamNotFound`,
which DOES degrade via the Put) and from "leader-only NATS partition" (which drops the connection;
this is connection-UP with a per-op deadline). Not covered by §4's dismissals.

**Minimal fix direction:** route sustained enumeration failure into the manager's degraded circuit
with the same `markKVUnavailable` / `recordKVOpError` semantics the manager's own KV-op sites use.
NOTE (cross-model review — wiring caveat): `markKVUnavailable`/`recordKVOpError` live on the
**manager** side, and `assignment.Config` has **no error-callback seam** today
(`internal/assignment/config.go:35-150`; `manager_assignment.go:132-151`). So the fix is NOT a call
from inside `internal/assignment`; it must add an explicit manager→calculator error hook (or
equivalent wiring) to surface sustained enumeration failure to the manager circuit without creating
an import cycle. Add a focused gated proof (Keys-only deadline on the heartbeat bucket on the
leader; assert the leader neither degrades nor serves a correct assignment).

**Cleared (not gaps):** `monitorCommitChanges` feeds `recordKVOpError` on watch-restart failure
(`manager_assignment.go:636`); `monitorCalculatorState` is a pure local state mirror with no
independent KV dependency.

---

## 6. Open uncertainties + how to settle (priority-ordered)

| # | Uncertainty | How to settle | Priority |
|---|---|---|---|
| 1 | **C-mech2 IOPS cost of heartbeat→FileStorage (Opt D)** | Measure heartbeat-bucket FileStorage IOPS at production cadence (highest-freq KV op). This is the **C-mech2 fix decision gate** — do NOT transfer M1.9 (election bucket). | **HIGH — run first** |
| 2 | **RF3 rolling-restart preserves replicated MemoryStorage?** | NEW gated test on `partitest.StartEmbeddedNATSClusterN(t,5)`: MemoryStorage Replicas:3 heartbeat bucket + real 3-mgr fleet + one-node-at-a-time rolling restart; assert holds-Stable AND heartbeat stream `Created` unchanged. The existing `quorum_loss_tier2_test.go` is a data-plane KV probe (no fleet, no MemoryStorage bucket, no rolling restart) and **cannot** be reused as-is — only the cluster helper is. Discriminates C-mech2 severity (Low if survives, fleet-gap if not). | **HIGH** |
| 3 | **`-race -count=5` over the 5 gated flap proofs** | Run NP-1/2/3b/8mech1/8mech2 under `-race -count=5` to establish a clean concurrency baseline (the exit-gate fixes land on the hot `kvErrorWindow`/`degradedSince` paths + hook goroutines). Cheap; do before AND after any fix. | **MEDIUM (cheap)** |
| 4 | **C-mech1 attribution + wire surface** | Add StateShutdown/OnError instrumentation to the NP-8 mech-1 proof (stop the attribution being an inference); add a wire test for the *purged-key* "wrong last sequence: 0" → ErrKeyExists case. | MEDIUM |
| 5 | **C-mech2 re-degrade reason string** | Add a reason-capturing OnDegraded hook to the mech-2 proof (kv-unavailable vs "KV error threshold exceeded"). Flap identical either way; doc accuracy only. | LOW |
| 6 | **NP-9 arbitration race (which reason wins)** | Parameterize the NP-9 test sweeping `KVErrorThreshold` vs `watcherMaxAttempts`. Both outcomes are valid Degraded entries — doc accuracy, not correctness. | LOW |

---

## 7. Recommended fix sequencing for the follow-up FIX session

Dependency-ordered. **Bold edges are hard blockers.**

**Phase 0 — measure/baseline (run before committing to any fix):**
1. **IOPS measurement** for heartbeat→FileStorage (gates the C-mech2 path choice).
2. **RF3 tier2 rolling-restart proof** (gates C-mech2 severity).
3. **`-race -count=5` baseline** over the 5 gated proofs.

**Phase 1 — independent, low-coupling fixes (parallelizable):**
- **Family B:** reason-scoped heartbeat-success exit gate (Candidate 2) in
  `attemptRecoveryFromDegraded`, ADDITIVE to `currentAssignmentApplied`. Validate: ungate NP-3b;
  re-run **ungated NP-5**, NP-9, NP-3a, C1 (`-race`).
- **Family A:** epoch-aware exit gate via **live `epochMismatchOutstanding()` re-probe** (NOT the
  latch — verification: the latch has a ~10s NP-1 pre-arm race) in the same exit block. Test must
  assert NP-1's **first** exit is blocked. Validate: NP-2, NP-1, C1, F1 entry tests (`-race`).
  > A and B co-edit `manager_degraded.go:408-415` as two independent ANDed conjuncts — one PR for
  > review economy is fine; they share no state.
- **Family C mech 1:** **doc-only** + re-purpose the proof to assert `StateShutdown` + add
  Shutdown/OnError instrumentation. No code-path change. Re-ground the rationale on orchestrator
  rotation, not full-fleet split-brain.

**Phase 2 — C-mech2 (depends on Phase 0 results):**
- **If IOPS clears → Opt D** (heartbeat→FileStorage). One line; **decoupled from Family A**; no
  recreate race. Update `PartialBucketLoss` semantics + the `manager_setup.go:116-117` doc comment.
- **Else → Opt C (exit gate) + Opt A/B′ (recreate).** **Opt A/B′ recreate MUST land AFTER Family A's
  epoch fix** (re-capture/re-probe the heartbeat epoch atomically, or it re-arms the fence
  fleet-wide). Or Opt C alone + docs (terminal Degraded ⇒ re-provision; this requires rewriting
  the NP-8 mech-2 proof expectation from reach-Stable to hold-Degraded — a product decision).

**Phase 3 — completeness:**
- **NP-10:** add a gated proof + wiring that surfaces sustained heartbeat-enumeration failure into
  the degraded circuit via a new manager→calculator error-hook seam (NOT a call from inside
  `internal/assignment` — see §5). Independent of Phases 1–2.

**What blocks what (DAG):**
- Phase 0 #1/#2 **block** the C-mech2 path choice.
- Family A's epoch fix **blocks** C-mech2-via-recreate (NOT C-mech2-via-Opt-D).
- Family A and Family B are mutually independent (coordinated-but-separate; same function).
- C-mech1 (doc), NP-10, and Family A/B are otherwise independent.

---

## 8. Confidence + what bounds it

**Overall: High on the code-derived mechanisms and reproduction; Medium on two extrapolations.**

| Finding | Confidence | What bounds it |
|---|---|---|
| All four gaps reproduce on HEAD 2453306 | **Very high** | Real `.out` files; CAS argument makes N-entry oscillation irrefutable. |
| Family A root cause (both halves + version-monotonicity) | **High (verified code)** | Anchors read directly (`manager_degraded.go:376-416`, `manager_setup.go:669-693`). |
| Family A: **latched** Opt-2 sufficiency for NP-1 | **Low (INFERENCE, contested)** | The ~10s pre-arm race was NOT settled empirically (read-only; no patched-build run). The **re-probe** variant is recommended precisely because it has no pre-arm window. |
| Family B root cause + NP-5 regression of an unconditional gate | **High** | NP-5 regression is VERIFIED by code reading (`newTestManager` never starts heartbeat), not a patched run. |
| Family B: candidate 2 not prototyped | **Medium (until run)** | Verification trace is code+proofs, not a patched binary. Mandatory: ungate NP-3b + re-run NP-5/NP-9/C1. |
| C-mech1 mechanism | **High** | Wire surface, routing, boundary math all verified. |
| C-mech1: **attribution within the proof** | **Medium (INFERENCE)** | Proof runs both mechanisms, no Shutdown/OnError hook; "bare ErrClaimLost fired" rests on one un-captured diagnostic (`04-proof-findings.md:135`); wire test covers the adjacent present-key case. |
| C-mech1: doc-only justification | conclusion High, **reasoning corrected** | Full-fleet split-brain steelman is FALSE (no leader rebalances); conclusion survives on orchestrator-rotation grounds. |
| C-mech2 driver = publisher Put | **High (code by elimination)** | Attribution is code-only, not test-captured (NopLogger + empty hooks) — add a reason-capturing hook to make it test-backed. |
| C-mech2 production severity | **Medium** | RF3 topology unmeasured — the single biggest open uncertainty. |
| Exit-gate unification = independent | **High** | Timeout-interpretation conflict + structurally disjoint triggers + opposite outcomes, all verified. |
| NP-10 (new gap) | **Medium** | Code-derived + empirical classification check; NOT harness-proven. |

**Every place a recommendation rests on inference rather than verified code** is flagged inline
above (Family-A latched sufficiency, Family-B candidate-2 not-run, C-mech1 attribution + Layer-2
double-processing, C-mech2 RF3 topology + reason string, NP-10 not-harness-proven). All four gaps
themselves and all four root-cause mechanisms are verified code, not inference.
