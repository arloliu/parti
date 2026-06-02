# Family C, mechanism 1 (NP-8): claim-loss self-stop on outage >= WorkerIDTTL

Investigation date: 2026-06-01
HEAD: 2453306 (worktree `auto-heal-gap-investigation`, content-identical to `main`)
Scope: READ-ONLY. No production code changed.

Report verdict under review (04-proof-findings.md:156-160, :251, :286-292):
**Low / doc-only** — `claimLostShutdown` is intended split-brain safety, per-worker not
fleet-specific, needs a "multi-minute" outage at the 75s prod default; fix is to document
that M5's "recover to Stable" is bounded by `WorkerIDTTL`.

My verdict: **confirmed-with-corrections.** The mechanism is exactly as the report
root-causes it, and the split-brain rationale for the *peer-takeover* case is genuinely
sound. But the report's framing is imprecise on three load-bearing counts (see §6), and the
deepening questions (a)-(d) expose a real conflation that a fix *could* address — it is not
"impossible," only "not attempted." Whether to fix is a genuine design call, not a slam-dunk
doc-only.

---

## 1. Independently re-derived mechanism (cite-by-cite)

### 1.1 The lease ages out by MaxAge, not by peer action

The stableID KV bucket's `MaxAge` is reconciled to exactly `WorkerIDTTL` on `Manager.Start`
(`config.go:366-369`, enforced by `reconcileStableIDBucketMaxAge`, `manager_setup.go:354-373`,
called from `ensureStableIDKV`, `manager_setup.go:91-98`). The bucket relies *entirely* on
MaxAge to expire abandoned claims — there is no explicit delete-on-expiry; the NATS server
purges the key once its newest revision is older than `MaxAge`.

The renewal loop (`internal/stableid/claimer.go:299-338`) renews at
`renewInterval() = max(ttl/3, 100ms)` (`claimer.go:491-493`). At the **75s default** that is
**25s**. So a live worker's key is rewritten every 25s, and the server purges it at
`last-renewal-time + 75s`.

During a NATS outage the renewal `kv.Update` calls (`claimer.go:362`) fail with connectivity
errors each tick. Those are correctly routed as harmless: `renew()`'s `default` branch wraps
them generically (`claimer.go:385-387`), `onClaimerError` sees a non-`ErrClaimLost` error and
sends it to `recordKVOpError` (`manager_election.go:126`) → the degraded circuit. No self-stop
yet. The worker sits in Degraded ("NATS connection down", `manager_degraded.go:114`) holding
its cached assignment (M5 proof asserts this, `full_nats_outage_test.go:53-54`).

### 1.2 The self-stop fires on the *first* renewal after reconnect

When the outage exceeds `WorkerIDTTL`, the key is purged server-side. On reconnect the next
renewal tick calls `kv.Update(key, value, lastRevision)` with the **stale** non-zero
`lastRevision` (`claimer.go:362`, value from `lastRevision.Store` at the original Claim,
`claimer.go:182`). Because the subject now has **no** message (purged), the server's
expected-last-subject-sequence check fails with "wrong last sequence: 0", which nats.go
surfaces as `jetstream.ErrKeyExists` (wire contract pinned by
`TestClaimer_WireContract_RevisionMismatchIsErrKeyExists`, `claimer_test.go:650-681`).

`renew()` matches `case errors.Is(err, jetstream.ErrKeyExists)` (`claimer.go:365`) and returns
**bare** `ErrClaimLost` — `fmt.Errorf("%w: ID %s", ErrClaimLost, wid)` (`claimer.go:368`), with
**no** connectivity or not-found sentinel wrapped.

`onClaimerError` (`manager_election.go:106-127`):
- `errors.Is(err, stableid.ErrClaimLost)` → true (line 107).
- `IsConnectivityError(err) || IsDegradingJetStreamError(err)` → **both false** (the bare wrap
  carries no `ErrTimeout`/`ErrNoStreamResponse`/`ErrBucketNotFound`/`ErrStreamNotFound`; see
  `natsutil/errors.go:92-100,114-137`).
- → `claimLostShutdown(m)` (line 118).

`claimLostShutdown` (`manager_election.go:56-81`) spawns a goroutine that calls `m.Stop` →
`StateShutdown`, then revokes the worker consumer. This is **terminal**: `NewManager`/`Start`
cannot be re-entered in place (NP-4 confirms a second `Start` returns `ErrAlreadyStarted`,
04-proof-findings.md:40,176-178). Recovery requires external pod rotation.

### 1.3 The self-stop is NOT gated on recovery — it always wins the race

There is no grace window. On reconnect two things race: (i) the 1s connection monitor reaches
`attemptRecoveryFromDegraded` after `ExitThreshold` uptime (`manager_degraded.go:127-132`), and
(ii) the renewal loop fires `kv.Update`→`ErrClaimLost`→`claimLostShutdown`. `claimLostShutdown`
fires unconditionally; nothing checks "did we just recover?" The proof shows (ii) wins:
"all 3 workers Degraded→Shutdown @8.2s with OnError: worker ID claim lost"
(04-proof-findings.md:135). Even if recovery won first, the *next* renewal tick would still
self-stop, because nothing re-claims the purged key.

**This is the conflation locus, verified:** the bare-`ErrClaimLost` branch at
`claimer.go:365-368` (revision-mismatch / "wrong last sequence") is the *single* code point
where "a peer Put/Update'd my key" (revision bumped, key present, owned by another) and "my
lease aged out, the slot is now empty" (no message for the subject) are **collapsed into the
identical error**. They are then routed identically by `onClaimerError:117-118`.

---

## 2. The deepening questions

### (a) Does the code conflate "peer took my ID" with "lease expired, no peer present"?

**Yes — but the precise framing matters.** The error *alone* is genuinely ambiguous: both
surface as bare `ErrClaimLost` (§1.2). The manager does **not** *attempt* to distinguish them
— it is not that distinguishing is impossible. A follow-up `kv.Get(key)` on reconnect *would*
disambiguate:
- **Key absent / no entry** (or a tombstone) ⇒ the lease aged out by MaxAge and **no peer
  holds it** (a peer that re-Created would leave a fresh value). The slot is free.
- **Key present with a fresh value + a revision != my `lastRevision`** ⇒ a peer holds the ID
  (genuine takeover, must stop, C2).

So the correct claim is: **current code conflates the two; it has the information available to
distinguish them but chooses a uniform self-stop.** "Cannot distinguish" overstates it.

Note the full-fleet subtlety: in NP-8 *every* worker is disconnected during the outage, so
**no peer can possibly have taken over** — yet all 3 self-stop on reconnect
(04-proof-findings.md:135, `healed`-style evidence). This is the unnecessary-loss case in its
purest form: the entire fleet rotates for a transient connectivity event, with zero actual ID
contention.

### (b) Is a safe re-claim path feasible WITHOUT regressing C2? Two layers.

**Layer 1 — the ID claim (feasible, low risk).** In the bare-`ErrClaimLost` else-branch
(`onClaimerError:117-118`), before self-stopping, the claimer could `kv.Get(myID)`:
- present+fresh+different-revision ⇒ peer takeover ⇒ `claimLostShutdown` (C2 preserved
  exactly).
- absent/expired ⇒ attempt an **atomic `kv.Create(myID)`** (NOT `Put` — `claimer.go:200-201`
  documents why Create is mandatory: two reclaimers racing must not both win). On Create
  success, store the new revision and resume renewal. On `ErrKeyExists` (a peer won the race
  in the gap) ⇒ fall back to `claimLostShutdown`.

This change lives **only** in the bare-`ErrClaimLost` branch. The connectivity/degrading
branch (`onClaimerError:113-115` → `recordKVError`) is untouched, so the whole-bucket-loss
fan-out (C1) and the OnDegraded-once contract (C3) are unaffected. C2's *peer-present* case
still self-stops because the Get sees the peer's fresh value. The renewal loop already returns
on `ErrClaimLost` (`claimer.go:329-334`), so the re-claim would need a small claimer API
(e.g. `Reclaim(ctx)`) or to be hoisted into the manager.

**Layer 2 — the assignment/data plane (this is the real blast radius).** Re-claiming the ID is
necessary but **not sufficient** for a *safe* in-process resume. While the worker was Degraded
it held its **cached assignment** (the partitions it owned pre-outage). But an outage long
enough to age out the claim (>= 50-75s at defaults) has — ~35-60s earlier — already expired
that worker's **heartbeat** (HeartbeatTTL=15s default; see §(c)), so the surviving fleet has
**rebalanced those partitions to other workers**. If the worker re-claims its ID and resumes
from cached assignment, it risks **transient double-processing** of partitions the fleet
already moved. A *truly* safe in-process re-claim must also drop the cached assignment and
re-enter `waitForAssignment` (`manager_election.go:451-486`) — i.e. replicate clean-restart
semantics in-process. That is a substantially larger change than the ID-layer fix.

There is *some* existing data-plane guard: the claim-loss ordering oracle
(`test/simulation/internal/coordinator/claim_loss_ordering_oracle_test.go`, e.g.
`TestClaimLossOrderingOracle_SuccessorReclaimsStableID_NoViolation:223`) fences post-takeover
message attribution. But that protects a *successor* reclaiming a *released/abandoned* ID, not
an in-process resume by the *original* holder; full data-plane verification of an in-process
re-claim is **out of scope for mech-1** and would need its own proof.

### (c) If doc-only is correct, *why precisely* is re-claim unsafe? The split-brain steelman.

The strongest WAD argument is **not** "a peer re-Created your ID slot" (in the full-fleet case
no peer did). It is the **HeartbeatTTL << WorkerIDTTL** gap:

- Validation forces `WorkerIDTTL >= HeartbeatTTL` (`config.go:370`, tag
  `gtefield=HeartbeatTTL`). Defaults: HeartbeatTTL=15s (`config.go:381`), WorkerIDTTL=75s, the
  5x relationship the doc recommends (`config.go:365`).
- Therefore *any* outage long enough to age out the stable-ID claim (>= ~50-75s) has, **~35-60s
  earlier**, expired this worker's heartbeat. The leader's emergency-rebalance path will have
  declared the worker dead and **reassigned its partitions** to peers that stayed up (or to a
  re-elected leader after recovery).

So the split-brain scenario is concrete: **"the fleet already declared you dead and moved your
work."** A naive in-process re-claim+resume-from-cache would put the worker back to processing
partitions that another worker now owns → double-processing until the next rebalance converges.
The unconditional self-stop guarantees the rotated pod re-enters cleanly through
`waitForAssignment` and picks up only what the *current* leader assigns it. **This is the
legitimate core of the doc-only recommendation, and it is sound for the peer-present and the
heartbeat-already-expired cases — which at defaults is essentially all cases that reach the
trigger.**

The orchestrator-layer rebuttal that makes doc-only defensible: in k8s, `StateShutdown` trips
the readiness/liveness probe, the pod restarts, and `Start` re-claims into the (now-empty) ID
slot cleanly. **The auto-heal happens at the orchestrator layer, not in-process** — which is a
legitimate design posture for a coordination library, not a bug.

### (d) Quantify the boundary. (Correcting the report's "multi-minute".)

- Trigger: an outage that exceeds the time from the worker's **last successful renewal** to
  `last-renewal + WorkerIDTTL`. Renewal cadence is `WorkerIDTTL/3` (`claimer.go:491-493`).
- At the **75s default**: renewal every **25s**, purge at `last-renewal + 75s`. So the
  triggering outage is between **~50s** (outage starts right before a scheduled renewal) and
  **~75s** (outage starts right after one) — **phase-dependent, ~50-75s, NOT "multi-minute"**
  (the report's :160 / :251 framing is imprecise; "minute-scale" is the accurate term).
- The proof uses WorkerIDTTL=5s + ~7s outage (`np8_fleet_..._test.go:80,177` — config 5s in
  `IntegrationTestConfig`, outage held >= 7s), which deterministically exceeds the TTL. The
  one-variable contrast (5s vs 30s WorkerIDTTL, same ~5s outage; 04-proof-findings.md:146-153)
  is a clean causal proof that this is **TTL expiry, not data loss**. I agree with that proof.
- Operationally common? A **minute-scale** NATS unavailability is not everyday but is well
  within realistic incident envelopes (cluster rolling restart that stalls, network partition,
  control-plane upgrade). It is materially more common than the "multi-minute" framing implies.
  Combined with the full-fleet "every worker rotates for one connectivity blip" outcome, this
  is **not negligible** — it means a single ~1-minute NATS hiccup rotates the entire fleet.

---

## 3. Fix options (mechanism, surface, blast radius, contracts, residual risk)

### Option 0 — Doc-only (the report's recommendation)

- **Mechanism:** document on the M5/M6 matrix row that "recover to Stable" is bounded by
  `WorkerIDTTL`; an outage >= WorkerIDTTL self-stops the worker (StateShutdown) and requires
  orchestrator rotation. Update 01-fault-matrix / 03-findings-index and the M5 test header
  (`full_nats_outage_test.go:18`) / OPERATIONS doc.
- **Surface:** docs + test comments only. Ungate nothing (the proof stays a known-FAIL until a
  behavioral fix lands, OR is re-purposed to assert the *documented* self-stop, i.e. assert
  StateShutdown rather than StateStable).
- **Blast radius:** zero code. **Contracts:** none touched.
- **Pros:** honest, zero risk, matches the orchestrator-heals posture.
- **Cons / residual risk:** leaves the full-fleet unnecessary-rotation behavior in place; a
  ~1-minute NATS blip rotates every worker even when no ID was ever contended.

### Option 1 — ID-layer safe re-claim (Layer 1 only)

- **Mechanism:** in `onClaimerError`'s bare-`ErrClaimLost` else-branch
  (`manager_election.go:117-118`), `kv.Get(myID)`; if absent/expired, atomic `kv.Create`
  (resume renewal on success); if present-with-different-fresh-revision or Create loses the
  race, `claimLostShutdown` as today. Likely needs a `Claimer.Reclaim(ctx)` helper in
  `internal/stableid`.
- **Surface:** `manager_election.go:106-127`, `internal/stableid/claimer.go` (new method),
  `claimer.go:329-334` (renewal loop must support a re-armed claim).
- **Blast radius:** the stableID claim path only; degraded circuit untouched.
- **Contracts touched:** **C2** — preserved *only if* the Get-then-Create gate is airtight
  (peer-present ⇒ stop). Must add a unit test mirroring `TestManager_StopsItselfWhenClaimLost`
  (peer Put bumps revision ⇒ still stops) AND a new positive test (key purged, no peer ⇒
  re-claims, stays alive). C1/C3 untouched (connectivity/degrading branch unchanged).
- **Pros:** removes the full-fleet unnecessary rotation when no peer contends.
- **Cons / residual risk:** **does not address Layer 2** — re-claiming the ID while resuming
  from a stale cached assignment risks double-processing partitions the fleet already
  rebalanced (see §(b), §(c)). **Shipping Option 1 alone is arguably worse than doc-only**: it
  trades a clean rotation for a silent double-processing window. Do NOT ship without Layer 2.

### Option 2 — Full in-process re-claim + assignment re-bootstrap (Layer 1 + Layer 2)

- **Mechanism:** Option 1's ID re-claim, AND on a successful re-claim drop the cached
  assignment, revoke the worker consumer, and re-enter `waitForAssignment` so the worker only
  resumes partitions the *current* leader assigns. Effectively "clean restart without process
  death."
- **Surface:** `onClaimerError`, `claimer.go`, plus the assignment-apply / waitForAssignment
  path (`manager_election.go:451-486`, `manager_assignment.go`), and consumer revoke
  (`revokeWorkerConsumer`, `manager_election.go:26-34`).
- **Blast radius:** large — touches the assignment lifecycle, the part most cross-feature
  contracts depend on.
- **Contracts touched:** C2 (as Option 1), plus the assignment-apply / claim-loss-ordering
  oracle suite, plus the live-bucket-loss tests (must not regress C1). Needs an integration
  proof that an in-process re-claim does not double-process across the rebalance boundary.
- **Pros:** the only option that *safely* delivers in-process auto-heal across a
  >= WorkerIDTTL outage.
- **Cons / residual risk:** highest complexity; re-implements restart semantics in-process,
  which is exactly what the orchestrator already does for free. The cost/benefit is poor unless
  there is a concrete requirement to avoid pod rotation on minute-scale outages.

### Option 3 — Raise the effective trigger (config/operational guidance)

- **Mechanism:** guide operators to set `WorkerIDTTL` comfortably above their worst-case NATS
  outage envelope (it already defaults to 75s; the trade-off is slower ID reclamation after a
  genuine worker exit, `config.go:362-364`).
- **Surface:** docs only. **Blast radius / contracts:** none.
- **Pros:** trivial; pushes the boundary out.
- **Cons:** does not change behavior at the boundary, just relocates it; longer TTL slows
  legitimate stale-ID reclamation. A pure palliative.

---

## 4. Recommended fix

**Option 0 (doc-only) as the immediate action, with the report's "multi-minute" corrected to
"minute-scale (~50-75s at defaults)" and the full-fleet unnecessary-rotation outcome stated
explicitly.** I agree with the report that this is the right *near-term* call, but for a
sharper reason than the report gives: the split-brain risk is real (heartbeat already expired ⇒
partitions already rebalanced, §(c)), so a naive in-process re-claim (Option 1 alone) would
*introduce* a double-processing bug. The only behaviorally-safe fix is Option 2, whose
complexity is not justified unless avoiding orchestrator rotation on minute-scale outages
becomes a hard requirement.

Concretely, doc-only should: (i) fix the M5 row to say recovery-to-Stable holds only for
outages **< WorkerIDTTL**; (ii) state the boundary as **~50-75s at defaults** (not
"multi-minute"); (iii) note that an outage past it self-stops **every** disconnected worker
(StateShutdown), recovered by orchestrator rotation; (iv) re-purpose
`TestNP8FleetNATSOutage_LeaderContinuityRecoversFleet` to assert the *documented* self-stop
(StateShutdown) rather than leaving it a permanent known-FAIL, so it becomes a regression guard
for the intended behavior.

If a future requirement demands in-process survival, pursue **Option 2** (never Option 1 in
isolation).

---

## 5. Cross-family interactions

- **Family C mech 2** (heartbeat MemoryStorage loss flap): orthogonal mechanism, but the same
  HeartbeatTTL/WorkerIDTTL relationship that drives mech-1's split-brain argument also governs
  mech-2 timing. A combined NATS-restart fix must handle both: claim survival (mech-1) AND
  heartbeat-bucket reconstruction (mech-2). Note mech-2's proof deliberately raises WorkerIDTTL
  to 30s to *exclude* mech-1 (`np8_..._test.go:286-288`), so the two are cleanly separable.
- **Families A/B** (recover-on-wrong-signal exit in `attemptRecoveryFromDegraded`): unrelated
  code path. Mech-1's self-stop fires via `claimLostShutdown`, never through
  `attemptRecoveryFromDegraded`. A fix to the A/B exit gate does **not** touch mech-1.
- **C1 (whole-bucket-missing ⇒ all Degraded):** any mech-1 fix MUST stay inside the
  bare-`ErrClaimLost` branch (`onClaimerError:117-118`) and leave the connectivity/degrading
  branch (`:113-115` → `recordKVError`) untouched, or it regresses the lockstep fan-out that
  `TestManager_LiveNATSBucketLoss` pins.
- **C2 (peer takeover ⇒ only that worker stops):** the central contract a re-claim fix risks.
  Pinned (correctly) by `TestManager_StopsItselfWhenClaimLost` and
  `TestOnClaimerError_ClaimLostStopsWorker` — see §6.
- **C3 (OnDegraded once):** untouched by any mech-1 fix that stays in the self-stop branch
  (that branch fires OnError, not OnDegraded).
- **C4 (Start returns after sanity-check):** unrelated.

---

## 6. Discrepancies with the report (adversarial pass)

1. **Boundary magnitude is overstated.** Report says "multi-minute" at the 75s default
   (04-proof-findings.md:160, :251, :286). The actual trigger is **~50-75s** (phase-dependent
   on the 25s renewal cadence; §(d)). "Minute-scale," not "multi-minute." This matters: it
   makes the trigger materially more reachable than the report implies.

2. **The full-fleet unnecessary-rotation is under-stated.** The report frames mech-1 as
   "per-worker, not fleet-specific" (:157, :251). True that the *mechanism* is per-worker — but
   in the NP-8 scenario **all** disconnected workers cross the boundary together and **all**
   self-stop, even though (being all disconnected) **no peer could have taken any ID**. So the
   operational outcome *is* fleet-wide: one minute-scale NATS blip rotates the entire fleet for
   zero actual ID contention. The report's "per-worker" framing obscures this.

3. **The C2 pin is mis-cited.** The task brief and the contract list cite
   `TestStableID_StaleKeyTakeover_Reclaim` (`test/integration/stableid/stableid_takeover_test.go`)
   as the C2 pin. That test only exercises the **claimer's reclaim mechanism** (a successor
   takes over a leaked key); it does **not** test the `onClaimerError` self-stop routing at all.
   The actual self-stop pins are **`TestManager_StopsItselfWhenClaimLost`**
   (`manager_claimer_error_test.go:135-172`, full end-to-end peer-takeover ⇒ StateShutdown +
   consumer revoke) and **`TestOnClaimerError_ClaimLostStopsWorker`**
   (`manager_claimer_error_test.go:53-86`, routing-level). Any C2-risking fix must regression
   these two, not the takeover-reclaim test.

4. **"Cannot distinguish" would be too strong** (the report doesn't quite say this, but it's
   worth pinning): the code *conflates* the two causes (§(a)), but a follow-up `kv.Get` *could*
   distinguish them. The report's verdict is right; the reasoning should be "uniform self-stop
   is a deliberate-or-incidental choice given available information," not "the signal is
   irrecoverably ambiguous."

5. **Agreement:** the TTL-vs-data-loss causal proof (one-variable WorkerIDTTL contrast,
   :146-153) is sound and I independently confirm the wire surface that underpins it
   (purge ⇒ "wrong last sequence: 0" ⇒ `ErrKeyExists` ⇒ bare `ErrClaimLost`). The
   doc-only *direction* is defensible; only the framing needs the three corrections above.

---

## 7. Confidence

**High.** The mechanism is a small, fully-traced code path with every step cited; the wire
contract is pinned by an existing real-NATS test; the boundary math follows directly from
`renewInterval` and the MaxAge reconciliation; the split-brain rationale follows from the
`gtefield=HeartbeatTTL` validation and the default 5x ratio. The one residual uncertainty is
**empirical**, not analytical: I did not run a re-claim prototype to confirm that an in-process
resume would actually double-process across the rebalance boundary (Layer 2) — that is asserted
from the assignment-lifecycle reading, and a fix would need its own integration proof. That
uncertainty does not affect the mech-1 root-cause verdict, only the cost estimate of Option 2.
