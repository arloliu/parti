# Exit-Gate Unification — Adversarial Refutation of "one fix closes A+B"

- **Date:** 2026-06-01
- **HEAD:** 2453306 (branch `auto-heal-gap-investigation`)
- **Assignment:** Refute (or survive the refutation of) the report's §1 claim that
  Families A (epoch-fence recreate) and B (connected-but-KV-unavailable) **share the
  load-bearing exit defect** in `attemptRecoveryFromDegraded`, and that "a single fix to
  the exit gate could close BOTH A and B."
- **Verdict:** **Confirmed-with-corrections.** The shared-*symptom* half of the report is
  accurate; the shared-*fix* conclusion is **refuted**. The two families need **separate,
  independent** fixes (at most coordinated-but-separate). They additionally want *opposite
  desired outcomes* at the exit gate, so no single mechanism can satisfy both.

---

## 0. What the report actually claims (so the refutation is honest)

§1 (04-proof-findings.md:48-52) and §7.1 (lines 276-282) make **two distinct sub-claims**.
They must be judged separately or the verdict over/under-claims:

- **Sub-claim S1 (shared symptom):** both A and B "exit to Stable on a healthy *assignment*
  read while a *different* trigger is still firing" — the recover-on-wrong-signal exit in
  `attemptRecoveryFromDegraded`. **This is ACCURATE** (verified below).
- **Sub-claim S2 (shared fix):** "A single fix to the exit gate could close both A and B"
  (with the caveat "A still needs the stale-`ep.created` latch addressed"). **This is the
  claim under attack, and it is REFUTED.**

So the correct verdict is **confirmed-with-corrections**, not a blanket `refuted`: the
per-family root causes the report states are sound; the correction is "shared function,
disjoint missing checks, opposite desired outcomes ⇒ no single mechanism fix."

---

## 1. Independent re-derivation of the recovery state machine

### 1.1 The exit path is trigger-blind (the shared symptom S1 — confirmed)

The recovery loop runs on the connection monitor (`monitorNATSConnection`, 1s ticker,
`manager_degraded.go:76-94`). On each tick `checkConnectionHealth`
(`manager_degraded.go:97-134`):

- If connected and `connUpSince` has exceeded `ExitThreshold`, it calls
  `attemptRecoveryFromDegraded` (`:128-132`). Recovery is gated on connection **uptime**,
  not on any subsystem health.

`attemptRecoveryFromDegraded` (`manager_degraded.go:376-416`) does exactly four things:

1. `if m.degradedSince.Load() == 0 { return }` — bail if not degraded (`:378`).
2. `refreshAssignmentFromNATS()` — reads **only** `assignment.<workerID>` from
   `m.assignmentKV` (`manager_assignment.go:1561-1568`). On error: `recordKVError(err)` +
   return (stay degraded, `:383-387`).
3. `recordKVSuccess()` — wipes the **entire** `kvErrorWindow` (`manager_degraded.go:393`,
   impl `:244-250`).
4. `cur := m.CurrentAssignment(); if !m.currentAssignmentApplied(cur) { scheduleApplyRetry;
   return }` else `exitDegraded()` (`:408-415`).

**The load-bearing fact:** this function reads **neither** `m.bucketEpochs` (Family A's
trigger state) **nor** any per-source KV-op health (Family B's failing
heartbeat/election/stableid). The only health signals it consults are (a) a single
assignment-bucket Get and (b) `currentAssignmentApplied`, a *commitment-state* comparison.
So whenever the assignment bucket is readable and the snapshot is committed, it exits —
regardless of why the worker degraded. That is sub-claim S1, and it is **confirmed**.

### 1.2 `currentAssignmentApplied` is a commitment gate, NOT a trigger gate

`currentAssignmentApplied` (`manager_assignment.go:1208-1217`) compares
`committedAssignmentOrEmpty()` against the snapshot on the applied-ack identity
`(Version, LeaderRevision, PartitionSetDigest, source-rev-when-known)`. The
latched-worker-commitment plan (`03-latched-worker-version-commitment-plan.md:404-407`)
states the design intent explicitly:

> "The guard keys on commitment STATE, not the degraded reason — exiting with an unapplied
> assignment is wrong regardless of why we degraded."

This is the crux of why the latched fix does **not** close either family: it was *designed*
to be reason-agnostic. In both A and B the worker has a fully-committed snapshot
(`committed == snapshot`), so `currentAssignmentApplied(cur) == true` and the guard
**permits** the exit. The NP-3b harness even arms its fault *only after* Stable precisely so
`committedAssignment == snapshot` and "the recovery path under test is the false-exit branch
... not the stay-degraded `scheduleApplyRetry` branch" (np3 test comment,
`np3_kv_unavailable_recovery_test.go:180-186`). So the existing exit gate's only sub-checks
are orthogonal to both triggers.

### 1.3 The two triggers are structurally DISJOINT (the wedge)

Enumerating every `enterDegraded` entry surface (grep over `*.go`, non-test):

- **Family A trigger:** `manager_setup.go:689` —
  `m.enterDegraded("bucket-recreated:" + bucket)`, fired **directly** from
  `checkBucketEpochs` (`manager_setup.go:669-693`) when the live stream-`Created` differs
  from the cached `ep.created`. This path **never** calls `recordKVError` and **never**
  touches `kvErrorWindow`. The NP-2 proof hard-codes this isolation: it deliberately leaves
  `KVErrorThreshold` at default so a transient heartbeat-Put miss "cannot independently trip
  'kv-unavailable' and mask the epoch-fence flap" (`np2_...:81-83`), and asserts
  `otherDegrades == 0` (`:190-192`). So Family A is provably *not* a `kvErrorWindow` event.
- **Family B trigger:** `recordKVOpError` → `markKVUnavailable` → `recordKVError` →
  `kvErrorWindow` append → threshold → `enterDegraded("kv-unavailable")`
  (`manager_degraded.go:235-237`, `144-225`). Wired at every periodic KV-op site: election
  renew (`manager_election.go:126`), election ticks (`:262`, `:303`), heartbeat publisher
  `SetOnError` (`:424`), assignment watcher (`manager_assignment.go:398`, `:437`, `:636`).
  Family B is *entirely* a `kvErrorWindow` event.

The two triggers live in **different state** (`m.bucketEpochs` map vs `m.kvErrorWindow`
slice), are driven by **different monitors** (epoch ticker at `OperationTimeout` vs per-op
error callbacks), and produce **different reason strings** (`bucket-recreated:<b>` vs
`kv-unavailable`). The recovery exit gate inspects *neither*.

---

## 2. What a "fixed" exit gate would have to be — and why it cannot be one mechanism

The natural unified candidate (task step b): **"exit only when the op(s) that TRIGGERED the
degrade have demonstrably recovered."** Today the degrade reason is just a **string** passed
to `enterDegraded` and discarded — there is **no per-trigger health signal** anywhere. So a
"verify the trigger recovered" gate *requires new bookkeeping*: a record of "what made me
degrade + is it healthy now." The question is whether **one** such mechanism can cover both.
It cannot, for three independent reasons.

### 2.1 Disjoint health probes (the trigger-recovered signals differ)

- To know **Family B recovered**, the gate must observe that the *failing periodic KV op*
  (heartbeat/election/stableid) is succeeding again. The natural signal is "the
  `kvErrorWindow` has drained / a healthy op intervened" — i.e. the F-D1 candidates from
  `04-fd1-flapping-decision.md:118-123`: post-recovery cooldown, verify-the-failing-op-
  recovered, or per-source error counters. All three are expressed in **kvErrorWindow / KV-op
  terms**.
- To know **Family A recovered**, the gate must compare the live stream-`Created` against the
  cached epoch — i.e. read `m.bucketEpochs`. A bucket recreate **never enters
  `kvErrorWindow`** (§1.3), so a gate "verify the failing KV op recovered" is **structurally
  blind to Family A**: there is no KV-op error to verify recovered. This directly answers
  task step (c): *an exit-gate fix expressed in KV-op terms does not cover A at all.*

### 2.2 Opposite desired outcomes (the deepest reason — a single predicate cannot encode both)

This is the decisive refutation. The two families want the gate to do **opposite** things
once the assignment read succeeds:

- **Family B's correct behavior is to auto-heal.** Once the KV op recovers, the worker
  **should** return to Stable. Positive control **NP-3a PASSES**
  (`np3_...:285-313`): after disarm the manager recovers to Stable and **holds** it
  (`require.Never(Degraded, 5s)`). B's bug is exiting *too early* (before the op recovers);
  the fix narrows *when* it exits but still exits.
- **Family A's correct behavior is to NEVER auto-heal.** The M4 contract is **terminal**
  Degraded so a readiness probe rotates the pod (NP-2 asserts `finalState == StateDegraded`,
  `np2_...:211-214`; NP-1 asserts no `Degraded→Stable` after recreate). A bucket recreate is
  permanent-by-design data loss; recovery is a process restart, not an in-process exit
  (`docs/OPERATIONS.md`, `manager_live_bucket_loss_test.go`).

A single "exit iff trigger recovered" predicate would have to encode "trigger B is
transiently-unhealthy (exit when it clears)" **and** "trigger A is permanently-unhealthy-by-
design (never exit)." That *is* the per-trigger registry the task hypothesizes — and it is two
disjoint, independently-necessary probes (kvErrorWindow drain vs epoch-Created compare) living
in the same `if` body. "One fix" is then true only trivially (one function edited); the
*mechanisms* are independent. There is no shared computation.

### 2.3 Concrete break case (task step c — fixing B breaks/misses A)

Implement the F-D1 "verify the failing KV op recovered" gate for B: before `exitDegraded`,
require that the `kvErrorWindow` is empty *and* a fresh healthy heartbeat/election op has been
observed. Run Family A's NP-2 against it:

- The bucket recreate fires `enterDegraded("bucket-recreated:<heartbeat>")` with the
  `kvErrorWindow` **empty** (NP-2 keeps it empty by design, §1.3).
- The new B-gate sees an empty window + (after the heartbeat bucket is recreated, even empty)
  succeeding heartbeat ops → "trigger recovered" → **`exitDegraded` → Stable**.
- The epoch tick re-degrades on the next `OperationTimeout` poll → **the A flap is
  unchanged.** The B-fix is inert for A; worse, by green-lighting "KV ops healthy" it actively
  *permits* the wrong-direction exit A must forbid.

Symmetrically, an A-gate ("refuse to exit while an epoch mismatch is outstanding", §3.2) is
inert for B: B never has an epoch mismatch, so the A-gate always passes and B still exits on
the wrong signal. Neither gate covers the other family. **Refutation of S2 complete.**

---

## 3. The stale-`ep.created` latch is genuinely orthogonal (task step d)

`captureBucketEpoch` writes `ep.created` **once** at Start (`manager_setup.go:627`);
`checkBucketEpochs` **never re-captures** it (`:669-693`). So after a recreate the cached
value stays stale forever and every epoch tick re-fires `enterDegraded`. This is **purely a
Family A property** — B has no epoch state at all. Two fix shapes interact with it oppositely:

- **A-1 — re-capture `ep.created` after firing.** This latches the fence to fire **once**.
  But by itself it is **actively wrong**: it stops re-degradation, so the very next recovery
  tick exits to Stable on the (now recreated, possibly **empty**) assignment bucket and the
  worker **rests Stable on wiped coordination data** — the NP-1 false-healthy resting state
  (`healed=[0 1 2]`, all three rest `Stable` on empty buckets, `04-proof-findings.md:83-93`).
  A-1 alone converts a flap into a *silent* false-healthy, which is **worse**.
- **A-2 — refuse to exit while an epoch mismatch is outstanding.** This makes the
  **never-re-captured staleness load-bearing in the RIGHT direction**: a permanently-stale
  `ep.created` is exactly what keeps the mismatch detectable forever ⇒ terminal Degraded. Under
  A-2 you do **not** want the latch "fixed" — you want the staleness **preserved** and you add
  an epoch-aware exit check that reads `m.bucketEpochs` inside `attemptRecoveryFromDegraded`.

Either way, A needs an **epoch-aware** change (re-capture, or epoch-outstanding exit check)
that B neither needs nor benefits from. The latch is orthogonal to any KV-op exit gate and is
**still required** regardless of what is done for B.

### 3.1 Discrepancy with the report (§7.1)

The report's §7.1 (lines 276-282) is **muddled on its own option 2**:

> "Because Family A's exit defect is shared with Family B, a single fix to the
> `attemptRecoveryFromDegraded` exit gate (option 2) may also resolve A's recovery half — but
> A still needs the stale-`ep.created` latch addressed."

- It frames A-2 as "the shared exit fix" while also saying "A still needs the stale-
  `ep.created` latch addressed." Under A-2 the stale `ep.created` must be **kept**, not
  "addressed" — preserving the staleness is what makes Degraded terminal. The phrasing is
  backwards for its own option 2.
- The two A options are not interchangeable as the report implies ("the proofs are agnostic to
  which fix lands", §7.1 line 278): **A-1 alone fails NP-1** (rests Stable on empty buckets),
  so it is not a complete fix. Only A-2 (or A-1 *plus* a recovery-exit guard) satisfies both
  NP-2's terminal-Degraded and NP-1's no-false-healthy invariants.

This is a real, citable discrepancy: the report's tidy "shared exit fix + latch on top"
framing does not hold up against its own NP-1 evidence.

---

## 4. Viable fix options (mechanism, surface, blast radius, contracts, risk)

### Family B options (KV-op-recovery gated)

**B-opt-1 — Verify-the-failing-op-recovered exit gate.**
- *Mechanism:* before `exitDegraded` in `attemptRecoveryFromDegraded`, additionally require
  that the degraded-circuit window has drained AND a fresh healthy periodic op was observed
  since the degrade (e.g. gate on `kvErrorCount == 0` plus a "last healthy heartbeat after
  degradedSince" timestamp). Builds on the existing `recordKVHealthyOp` (421f13c) signal.
- *Surface:* `manager_degraded.go:408-415` (add condition before `exitDegraded`); reuse
  `kvErrorWindow`/`kvErrorCount` + a new `lastHealthyOpAt atomic.Int64`.
- *Blast radius:* recovery exit only; runs on the connection-monitor goroutine — needs the
  `-race` stress per AGENTS.md (template `degraded_recovery_rearm_concurrency_test.go`).
- *Contracts:* **C1** unaffected (whole-bucket entries are non-transient and keep the worker
  degraded — but note: under whole-bucket loss the *assignment* read also fails, so recovery
  already bails at `:383-387` before the gate). **C2** is the worker-takeover claim path —
  untouched. **C3** held by construction (staying degraded keeps `degradedSince` non-zero, so
  `enterDegraded`'s CAS blocks re-fire). **C4** untouched (Start returns after sanity check).
- *Cons / residual risk:* the F-D1 semantic-narrowing already means a non-heartbeat sustained
  F-D1 timeout may be cleared by a healthy heartbeat before threshold
  (`04-fd1-flapping-decision.md:80-91`); a global "window drained" gate inherits that. A
  per-source counter variant is more precise but larger. **Structurally blind to A** (§2.3).

**B-opt-2 — Post-recovery cooldown.**
- *Mechanism:* after `exitDegraded`, suppress a re-exit for N seconds; if the trigger re-fires
  within the cooldown the worker stays degraded longer. Cheapest, but it does not *verify*
  recovery — it only damps the flap *frequency*. NP-3b's `require.Zero(degradedExits)` would
  still FAIL (it allows zero exits, not slower exits). **Insufficient for the NP-3b invariant
  as written; also blind to A.** Listed for completeness; not recommended.

### Family A options (epoch-aware)

**A-opt-1 — Re-capture `ep.created` after firing (latch-once).**
- *Mechanism:* in `checkBucketEpochs`, after `enterDegraded`, set
  `ep.created = live; m.bucketEpochs[bucket] = ep`.
- *Surface:* `manager_setup.go:684-690`.
- *Blast radius:* epoch monitor only.
- *Residual risk:* **FAILS NP-1** — stops re-degradation but the recovery loop then rests the
  worker Stable on the empty recreated bucket (false-healthy). **Not a complete fix alone**;
  must be paired with an exit guard (A-opt-2) or it regresses NP-1 into a silent false-healthy.
- *Contract note:* it changes the epoch fence from "re-fire every tick" to "fire once," which
  is the *intended* one-shot; but the exit half must still be closed.

**A-opt-2 — Epoch-aware exit guard (recommended for A).**
- *Mechanism:* in `attemptRecoveryFromDegraded`, before `exitDegraded`, refuse to exit if any
  `bucketEpoch` still shows a live-vs-cached `Created` mismatch (read `m.bucketEpochs` and
  re-probe, or cache an `epochMismatchOutstanding atomic.Bool` set by `checkBucketEpochs`).
  Keep `ep.created` permanently stale so the mismatch remains detectable ⇒ terminal Degraded.
- *Surface:* `manager_degraded.go:408-415` (new pre-exit check) + a flag set at
  `manager_setup.go:689`.
- *Blast radius:* recovery exit + epoch monitor; both already on background goroutines —
  `-race` stress required (template `epoch_monitor_concurrency_test.go`).
- *Contracts:* **C1** — must verify the whole-bucket-loss tests still recover: those wipe the
  bucket without recreating a *different* stream identity in the recovery window, and recovery
  bails on the failed assignment read anyway, so the epoch guard is not reached on the
  recover-able path. Verify `TestManager_LiveNATSBucketLoss` still recovers. **C3** held (stays
  degraded ⇒ no re-fire). **C2/C4** untouched.
- *Residual risk:* a flag-based variant must avoid a stuck-degraded false-positive if a probe
  transiently fails (the epoch monitor already treats a probe *error* as "rely on next tick",
  `manager_setup.go:679-682`; the guard should only block on a confirmed mismatch, not a probe
  error). Satisfies BOTH NP-2 (terminal) and NP-1 (no false-healthy).

### The unified candidate (rejected)

**U — single combined exit predicate.** Add both an epoch-mismatch check AND a KV-op-recovered
check before `exitDegraded`. This is "one function edited" but **two disjoint mechanisms** with
**opposite intents** (A: block forever; B: block until op recovers) sharing no computation
(§2.2). It is not a "single fix to the exit gate" in any meaningful sense — it is A-opt-2 and
B-opt-1 implemented in the same `if`. Calling it unification is a labeling artifact.

---

## 5. Cross-family interactions

- Both fixes edit the **same window** (`manager_degraded.go:408-415`, just before
  `exitDegraded`). If both land they must compose: the exit fires only when **both** the
  epoch-mismatch guard (A) AND the KV-op-recovered guard (B) pass. They are independent
  conjuncts — no shared state, no ordering dependency — so this is *coordinated-but-separate*,
  not unified. A single PR touching this block is reasonable for review economy, but the two
  conjuncts are independently testable and independently necessary.
- **C1 interaction (whole-bucket loss):** the strongest interaction risk. Both NP-2 (A) and
  the C1 contract test (`TestManager_LiveNATSBucketLoss`) involve a missing/recreated bucket.
  The discriminator is that C1's recover-able path fails the **assignment read** in
  `refreshAssignmentFromNATS` and bails at `:383-387` *before* either new guard. Any fix must
  re-run C1's two pinned tests `-race` to confirm the guards are not reached on the
  legitimately-recoverable path. (AGENTS.md cross-feature contract requirement.)
- **421f13c (`recordKVHealthyOp`) interaction:** it clears only *transient* entries on a
  healthy op while not degraded. It is upstream of the *entry* side (prevents B from
  degrading), not the *exit* side. B-opt-1 reuses its "healthy op observed" signal but for the
  exit gate. No conflict; complementary.

---

## 6. Verdict

- **rootCauseVerdict: confirmed-with-corrections.**
  - Sub-claim S1 (shared *symptom* — both exit via `attemptRecoveryFromDegraded` on the
    assignment-read signal while a different trigger persists): **confirmed.**
  - Sub-claim S2 (shared *fix* — one exit-gate change closes both): **refuted.**
- **Family relationship: fully independent** (at most coordinated-but-separate if both
  conjuncts share one PR). The recovery exit gate inspects neither trigger's state; the two
  triggers are structurally disjoint (`bucketEpochs` vs `kvErrorWindow`); the desired exit
  outcomes are opposite (A: never exit / terminal; B: exit once the op recovers); and a
  KV-op-recovered gate is structurally blind to A's epoch tick.
- **A's stale-`ep.created` latch is orthogonal and still required** regardless of any B fix;
  under the recommended A-2 the staleness is *preserved* (load-bearing), contradicting the
  report's "the latch still needs addressing" phrasing.
- **Recommended:** treat as two separate fixes — **A-opt-2** (epoch-aware exit guard, keep
  `ep.created` stale) for Family A, and **B-opt-1** (verify-the-failing-op-recovered exit gate)
  for Family B — landed with a shared `-race` regression pass on `manager_degraded.go`'s exit
  block and a mandatory re-run of the C1 contract tests.

## 7. Open questions

- Does C1's `TestManager_LiveNATSBucketLoss` ever reach the recovery exit block, or does it
  always bail on the failed assignment read? (Strongly inferred = always bails, but worth a
  direct trace before A-opt-2 lands, since A-opt-2's guard sits in that block.)
- For B-opt-1, is a global `kvErrorWindow`-drained gate sufficient for NP-3b, or is a
  per-source counter required? NP-3b faults heartbeat/election/stableid together, so the
  global gate suffices for the *proof*; a real single-non-heartbeat-bucket quorum loss (the
  F-D1 documented coverage gap, `04-fd1-flapping-decision.md:80-91`) may need per-source.
- A-opt-2 flag vs re-probe: a cached `epochMismatchOutstanding` flag avoids a probe in the
  hot recovery tick but risks staleness; a re-probe is authoritative but adds a KV read per
  recovery tick. Decide during the fix plan.
