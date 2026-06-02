# Adversarial verification — Family C mechanism 2 (NP-8): MemoryStorage heartbeat-bucket loss => fleet flap

Reviewer pass on current HEAD (`2453306`, worktree `auto-heal-gap-investigation`).
Read-only. Goal: REFUTE the finding `family-c-mech2.md`, not confirm it. Every claim
below was re-derived from the cited source; I state separately what I VERIFIED in code
vs what I INFER.

**Verdict: confirmed-with-caveats.** I tried to break the root-cause mechanism, the
proof, and the recommended fix. The note's three substantive corrections to the report
all SURVIVE my attacks and are independently verified. I add one sharpening that makes
the driver argument airtight (followers), one precision fix the note left as an assertion
(the leader-side calculator path is a *true* bystander, now proven via the calc state
machine), and one fix-critique the note under-pressed (the IOPS measurement is the actual
decision gate between Opt D and the complex Opt C+A/B' path, not a footnote).

---

## 1. Root-cause mechanism — attempted refutation, result HOLDS

### 1.1 The driver claim (publisher Put, not calculator list) — VERIFIED, and I strengthen it

The note's central correction to the report: the re-degrade driver is the heartbeat
**publisher `kv.Put`** wired to `recordKVOpError`, NOT the leader calculator's
"failed to list heartbeat keys". I verified every link:

- `internal/heartbeat/publisher.go:414` — `publish` does `p.kv.Put(ctx, key, value)` on a
  cached handle; on error (`:354-359`) invokes the `onError` callback.
- `manager_election.go:424` — `publisher.SetOnError(m.recordKVOpError)`. **VERIFIED.**
- `manager.go:602` — `startHeartbeat` is called in the synchronous `Start` path,
  **before** the leader-only calculator (`manager.go:607 if m.IsLeader()`), and
  independent of leadership. So EVERY worker (leader + followers) publishes heartbeats.
  **VERIFIED.**
- `recordKVOpError` → `markKVUnavailable` → `recordKVError` (`manager_degraded.go:235-236`).
  Both plausible Put error surfaces are ADMITTED:
  - `nats.ErrNoResponders` (cached-data-op surface per `source/nats_kv.go:1381-1383`,
    "verified vs nats.go v1.50.0"): not connectivity, not degrading-JetStream →
    `markKVUnavailable` wraps with `ErrKVUnavailable` (`:67-68`) → admitted via
    `kvUnavailable` (`:164-165`) → reason `kv-unavailable` (`:216-218`).
  - `jetstream.ErrStreamNotFound`: `IsStreamNotFound` true → `IsDegradingJetStreamError`
    true (`internal/natsutil/errors.go:92-99`) → `markKVUnavailable` returns it unchanged
    (`:64`) → admitted (`:165`) → reason `KV error threshold exceeded`.
  Either way, 5 errors / `KVErrorWindow` (30s) at `HeartbeatInterval=500ms` ≈ **2.5s to
  re-trip** post-exit (`enterDegraded` at `:224`). **VERIFIED.** The note's reason-string
  hedge (§1.4) is correct and immaterial to the flap.

**Strengthening (the airtight kill for Opt B, stronger than the note's argument):**
FOLLOWERS run no calculator at all (`startCalculator` is gated `if m.IsLeader()`,
`manager.go:607`). Post-reconnect with the connection CONNECTED and `WorkerIDTTL=30s`
(claim survives), every non-heartbeat bucket is FileStorage and ALIVE, so a follower's
election/stableID/assignment ops all succeed. The ONLY op a follower runs that can fail
is the heartbeat Put against the dead MemoryStorage stream. The proof's HOLD check
requires ALL workers Stable (`np8..._test.go:301-308, 327`). Therefore a follower's
publisher-Put re-degrade alone trips the HOLD — and a *calculator-only* fix (report's
Opt B) is structurally incapable of touching a follower. This makes "Opt B cannot stop
the fleet flap" a structural certainty, not just a wrong-driver argument.

### 1.2 Is the calculator a *pure* bystander? — the note ASSERTED it; I PROVED it

The note says the calculator list error is "swallowed" and the calculator is a "loud
bystander." I verified swallowing (`worker_monitor.go:175` returns the raw error; the
poll consumer at `:282-284` only `logger.Error`s it; no `recordKVError` wiring in
`internal/assignment/*.go` — my grep confirms). BUT the HOLD check fails on ANY non-Stable
state, not just Degraded, so I checked whether the leader's failed rebalance drives the
calc state machine to a non-Idle state that `monitorCalculatorState` maps to
Rebalancing/Emergency (manager state mapping docstring `manager_assignment.go:206`):

- `rebalance` returns the "list heartbeat keys" error at `calculator.go:1542-1544`.
- That error bubbles to `RunClaimedRebalanceErr` (`state_machine.go:368-386`), which calls
  `sm.ReturnToIdle()` **unconditionally** (`:383`) BEFORE returning the error, emitting
  `CalcStateIdle` (`:414`).
- `CalcStateIdle` maps to manager `StateStable` (`monitorCalculatorState`,
  `manager_assignment.go:206`).

**Conclusion (VERIFIED, upgrades the note):** the calculator failure neither feeds the
degraded circuit NOR drives a non-Idle calc state — the rebalance aborts and the machine
returns to Idle. So the leader has NO second, calculator-side contributor to non-Stable;
its only path is the same publisher-Put → `recordKVError` as the followers. The note's
"pure bystander" is therefore exact, not overstated. (The note left this as an assertion;
it is now proven.)

### 1.3 Did anything else feed the degraded circuit post-reconnect? — enumerated, NO

I re-ran the by-elimination the note relies on, widening it to ALL `recordKVOpError` /
`recordKVError` feed sites (`grep`, production only), not just the `enterDegraded` callers:

- `manager_election.go:262, 303` — election renew / follower request. Election bucket is
  **FileStorage** (`manager_setup.go:152`), survives → these succeed post-reconnect.
- `manager_assignment.go:398, 437, 636` — assignment watcher / session. Assignment bucket
  **FileStorage** (`:160`), survives.
- `manager_election.go:114, 126` — `onClaimerError` (stableID renew). StableID bucket
  **FileStorage** (`:92`); with `WorkerIDTTL=30s` the claim survives the 5s outage (this
  is exactly what isolates mech2 from mech1). → succeeds.
- `manager_degraded.go:385` — `attemptRecoveryFromDegraded`'s own `refreshAssignmentFromNATS`
  failure; assignment survived → does not fire.
- `manager_election.go:424` — heartbeat publisher. **The only feed site hitting the dead
  bucket.**

Secondary churn path I checked (the note did not): a follower that wins leadership in
`monitorLeadership` calls `startCalculator(... heartbeatKV)` (`manager_election.go:329`),
and on failure releases leadership (`:330`, `releaseLeadershipAfterCalculatorFailure`).
This could add leadership churn IF `calc.Start` fails on the missing heartbeat stream.
But (a) this does not feed `recordKVError`, and (b) even if it induces re-election churn,
the publisher-Put driver already trips every worker independently, so it is at most a
secondary contributor to the *leader-identity* instability, not to the Degraded flap.
The driver attribution is unaffected. **INFERENCE** (I did not trace whether `calc.Start`
performs a synchronous heartbeat read that fails on a missing stream); flagged as a minor
open item, not load-bearing.

### 1.4 F-D1 reset (commit 421f13c) is genuinely irrelevant — VERIFIED

`recordKVHealthyOp` clears only `transient` entries and only on a periodic KV **success**
(`manager_degraded.go:266-288`). The only wired success feeder is the heartbeat publisher
(`SetOnSuccess(m.recordKVHealthyOp)`, `manager_election.go:432`) — and the heartbeat is
the very op that is failing (bucket gone). No success ever fires, so the transient
entries accumulate uncleared. There is no other `SetOnSuccess` / healthy-op feeder in
production (grep confirms). The note's "reset is irrelevant" holds. **VERIFIED.**

### 1.5 The recover-on-wrong-signal exit — VERIFIED, same defect as Families A/B

`attemptRecoveryFromDegraded` (`manager_degraded.go:376-416`): refreshes ONLY the
assignment bucket (`refreshAssignmentFromNATS`, FileStorage, survived), `recordKVSuccess()`
clears the WHOLE window (`:393`), gates exit solely on `currentAssignmentApplied` (`:409`),
then `exitDegraded` → Stable (`:415`). It NEVER reads the heartbeat bucket. `exitDegraded`
resets `degradedSince=0` (`:356`), releasing `recordKVError`'s already-degraded
short-circuit (`:174`), so heartbeat-Put errors re-accumulate from zero. The 1s connection
monitor (`:79`) drives the exit on uptime ≥ ExitThreshold (`:130-131`); the link is
CONNECTED throughout. Two ticks fight → flap. **VERIFIED.** This is structurally the same
exit defect the report assigns to Families A/B (`04-proof-findings.md:50-52, 60-93,
95-120`), which is why Opt C is genuinely shared.

---

## 2. Does the gated PROOF prove what it claims? — checked, with one important nuance

I read `np8_fleet_nats_outage_leader_continuity_test.go` (both tests) and the `.out`.

**Mech2 test (`...HeartbeatBucketLossFlap`, `:265-331`):**
- Isolation from mech1: `cfg.WorkerIDTTL = 30s` (`:288`) > 5s outage (`:316`), so the
  stableID claim survives and `claimLostShutdown` cannot fire. **Good isolation.**
- Non-vacuity / sequencing: it FIRST asserts the fleet REACHES all-Stable
  (`require.Eventually(np8bAllStable, 30s)`, `:325`) and THEN asserts it HOLDS
  (`require.Never(!np8bAllStable, 8s)`, `:327`), plus connection CONNECTED (`:329`).
  The reach assertion is the non-vacuity guard: a dead recovery path (never reaching
  Stable) would fail at `:325`, not `:327`, and would be a different bug.
- **Evidence (`tmp/repro-current-head/np8_mech2.out`):** failure is at **line 327**
  ("Condition satisfied" on the `require.Never` HOLD), runtime **12.00s**. This proves the
  fleet DID reach all-Stable (the `:325` Eventually passed) and THEN re-entered non-Stable
  within the 8s HOLD window — i.e. a genuine flap, isolated from mech1, connection up.
  **The proof is non-vacuous and proves: flap + mech1-isolation + connectivity-up.**

**The nuance that matters for the discrepancy adjudication:** the mech2 proof uses EMPTY
hooks (`parti.WithHooks(&parti.Hooks{})`, `:293`) and the integration config logs via
NopLogger (`internal/testutil/nats.go:306`). So the proof captures NO `OnDegraded` reason
and NO per-worker driver attribution. **The proof is therefore SILENT on which op is the
driver.** It cannot discriminate the report's "calculator list error" framing from the
note's "publisher Put" framing — both predict the same observable (reach-then-flap,
connection up). The note's central correction (driver = publisher Put) rests 100% on
code-reading (§1.1–1.3), which I independently re-verified. This is the honest, defensible
statement: the test proves the flap; the code proves the driver.

(Mech1 `.out` confirms it is a distinct mechanism: failure is at `:187`
"manager[1] ... context deadline exceeded" reaching Stable — i.e. stuck Shutdown, NOT a
flap — runtime 40s. Cleanly separated.)

---

## 3. Attack on the recommended fix — strongest objections

The note recommends **Opt C (gate the recovery exit on heartbeat-op health) + Opt A/B'
(restore the bucket), co-designed with Family A's `ep.created` re-capture**, and offers
Opt D (heartbeat → FileStorage) as "simplest if IOPS clears."

### 3.1 Opt A epoch-fence collision — VERIFIED, the constraint is real

I confirmed the load-bearing constraint:
- `captureBucketEpoch` caches `ep.created` ONCE at Start (`manager_setup.go:627`), never
  re-captured.
- `checkBucketEpochs` (`:669-693`): TODAY, with the heartbeat stream gone, the probe
  `BucketStreamCreated` errors → `logger.Debug` + `continue` (`:679-682`) → **the fence is
  DORMANT on heartbeat** (does NOT fire `bucket-recreated`).
- A recreate (Opt A / Opt B' recreate variant) gives the new stream a different `Created`
  → `!live.Equal(ep.created)` → `enterDegraded("bucket-recreated:heartbeat")` (`:684-690`).
  Because all workers recreate against the same new stream, this fires **fleet-wide**,
  trading the heartbeat flap for an epoch-fence Degraded (itself a flap per Family A).
  The fence ticker runs at `OperationTimeout` (default 10s, `:649`).
**So any recreate-based fix is unsafe unless `ep.created` is re-captured atomically with
the recreate, winning the race against the epoch ticker.** The note's "co-design with
Family A is mandatory" SURVIVES. This is the strongest objection to Opt A and it stands.

### 3.2 Opt C-alone fails the proof as written — VERIFIED, survives

Opt C without a recreate converts the flap into permanently-stuck Degraded (MemoryStorage
never returns on its own). The proof's `:325` reach-all-Stable assertion would then FAIL
(stuck Degraded never reaches Stable). So Opt C-alone cannot be a drop-in regression
ungate; the proof expectation must be rewritten from auto-heal-to-Stable to
hold-Degraded/require-rotation — a PRODUCT decision, not a mechanical ungate. The note
states this (`:334-341`) and it is correct. **SURVIVES.** I note this is the fail-safe
posture matching the documented live-data-loss contract (`04-proof-findings.md:88-90`).

### 3.3 C1 contract is preserved — VERIFIED for Opt C and Opt A; conditional for B'

- **C1 (whole-bucket-missing => all Degraded):** Opt C only changes the *exit* gate, never
  *entry*, so it cannot suppress the entry path that C1 pins. Opt A keys on a
  reconnect/connection-restored event; `TestManager_LiveNATSBucketLoss` deletes all
  buckets LIVE with the connection UP and NO reconnect, so a reconnect-keyed Opt A does
  not fire there and C1 still holds via the other wiped buckets' non-transient
  `ErrBucketNotFound`. The note's caveat — pin the trigger to *reconnect*, not
  *any-missing-observation* — is the right guard. **VERIFIED, SURVIVES.** Opt B' must not
  let a Put-recreate mask C1: the other wiped buckets still produce non-transient
  (degrading-JetStream) entries that `recordKVHealthyOp` does NOT clear
  (`manager_degraded.go:278-284` keeps non-transient), so C1 holds — confirmed by the
  guard `TestManager_PartialBucketLoss_HeartbeatHealthy` (cited; I did not re-run it,
  read-only). **INFERENCE on the test's exact assertion; the masking-guard logic is
  VERIFIED in code.**
- **C2 / C3 / C4:** Opt C is on the recovery-exit path, orthogonal to peer-claim
  takeover (C2, `manager_election.go:106-127`), to the once-per-entry OnDegraded fire
  (C3, `enterDegraded` is unchanged), and to `Start` returning after sanity-check (C4).
  **VERIFIED orthogonal.**

### 3.4 The fix-critique the note under-pressed: IOPS is the actual decision gate

The note's recommended path (Opt C + A/B' + Family A `ep.created` co-design) is by far the
MOST COMPLEX option, and its ranking over the one-line Opt D rests ENTIRELY on an
**unmeasured** heartbeat-FileStorage IOPS cost. The note correctly debunks transferring
M1.9's "IOPS-free" finding (that is the ELECTION bucket, `manager_setup.go:113-115`;
MEMORY.md M2.A flags the per-op state file as the dominant cost; heartbeat is the
highest-frequency op). But the note buries the consequence in a footnote ("Opt D is the
simplest if an IOPS measurement clears it"). The sharper statement: **if the IOPS
measurement clears, Opt D DOMINATES** — it eliminates the flap at the source with no epoch
trip, no recreate race, and no Family A coupling, for a one-line storage change
(`manager_setup.go:156`), versus a three-way co-design. The IOPS measurement is therefore
the genuine decision gate, not a footnote, and it should be run FIRST. This objection
SURVIVES and is, in my view, the single most actionable correction to the note's fix
ranking. (Caveat the note already makes: Opt D changes the `manager_setup.go:116-117`
MemoryStorage rationale and `TestManager_PartialBucketLoss_HeartbeatHealthy` semantics;
operators who pre-create the bucket override anyway, `:121-124`.)

---

## 4. Discrepancies with the note (mine), and adjudication of the note vs the report

**Where the note is RIGHT against the report (I confirm all three substantive ones):**
1. **Driver misnamed (substantive) — UPHELD.** Report (`04-proof-findings.md:136-144,
   256-258`) localizes the gap to the leader's calculator "list heartbeat keys"; that path
   is swallowed (`worker_monitor.go:282-284`) AND returns the calc machine to Idle/Stable
   (`state_machine.go:383`, §1.2). The driver is the fleet-wide publisher Put. UPHELD and
   strengthened (followers, §1.1; calc-Idle, §1.2).
2. **Opt B ineffective (substantive) — UPHELD.** A calculator-only fix cannot touch a
   follower (which runs no calculator), and the HOLD requires all workers Stable. Plus
   empty (already tolerated, `worker_monitor.go:170-173`) ≠ missing (raw error,
   `calculator.go:1213`). Reject Opt B as written. UPHELD.
3. **Epoch-fence dormant→activated (sharpening) — UPHELD** (§3.1).
4. Reason-string (minor) and IOPS-transfer (flag): UPHELD; the note has these right.

**Where I correct or sharpen the NOTE:**
- The note ASSERTS the calculator is a bystander but did not check whether the calc *state
  machine* drives the leader non-Stable on the rebalance error. I PROVED it returns to
  Idle (`state_machine.go:383` unconditional `ReturnToIdle`) → Stable mapping, so the
  "pure bystander" claim is exact. (Confirms the note; closes a hole it left open.)
- The note's strongest Opt-B refutation should be the FOLLOWER argument (§1.1), which it
  does not make explicitly — followers run no calculator, so a calculator fix is
  structurally incapable of stopping their flap; this is stronger than "wrong driver."
- The note under-ranks Opt D: if IOPS clears, D dominates the complex recommended path
  (§3.4). The IOPS measurement is the decision gate.

**Where the report is NOT wrong:** its CORE verdict — fleet does not auto-heal a
single-node MemoryStorage heartbeat-bucket loss; topology-dependent (RF3 may save it);
needs recreate-or-tolerate — is correct, and the empirical FAIL holds. The report's error
is purely the driver attribution and the resulting Opt B, exactly as the note says.

---

## 5. Residual risk / open items

- **Driver attribution is code-only.** The proof is silent on the reason (empty hooks,
  NopLogger). The publisher-Put-driver conclusion is a code-reading claim (re-verified
  here), not a test-captured one. To make it test-backed, add a reason-capturing
  `OnDegraded` hook to the mech2 proof (test-only change). MEDIUM confidence that the
  reason is `kv-unavailable` vs `KV error threshold exceeded` (depends on the exact Put
  error surface); HIGH confidence the driver is the publisher Put regardless of reason.
- **RF3 topology (the report's biggest open item, unchanged).** Whether a real RF3 rolling
  restart preserves the replicated MemoryStorage stream is unmeasured; needs the gated
  `quorum_loss_tier2` 5-node harness. If RF3 preserves it, the prod severity drops.
- **`calc.Start` on a missing heartbeat stream (minor, INFERENCE).** I did not confirm
  whether a new leader's `startCalculator` fails synchronously on the dead stream and
  induces leadership churn. Not load-bearing (the publisher Put already trips every worker),
  but worth a one-line trace if leadership instability shows up in the fix's proof.
- **Opt D IOPS measurement (the actual fix decision gate).** Must be run before committing
  to the complex Opt C+A/B' path.

## 6. Bottom line

Root-cause mechanism: **HOLDS** (publisher-Put driver, recover-on-wrong-signal exit,
fleet-wide). Proof: **non-vacuous and isolated**, but silent on the driver (driver is
code-proven, not test-proven). Recommended fix: **survives** its strongest objections
(epoch-fence co-design mandatory; Opt C-alone needs a product/test-contract decision; C1
preserved), with one material sharpening — **the heartbeat-FileStorage IOPS measurement is
the real decision gate**, and if it clears, the one-line Opt D dominates the note's complex
recommended path. Verdict: **confirmed-with-caveats.**
