# Exit-Gate Unification — ADVERSARIAL VERIFICATION of the "separate fixes" finding

- **Date:** 2026-06-01
- **HEAD:** 2453306 (branch `auto-heal-gap-investigation`)
- **Reviewer task:** REFUTE the finding's verdict (separate fixes for Families A and B);
  default to "A and B need SEPARATE fixes" and only confirm unification if refutation of
  separateness fails. The finding itself already argues for separate fixes, so my job is the
  mirror: try hard to *resurrect* a single unified exit-gate fix and see whether it survives.
- **Verdict:** **CONFIRMED-WITH-CAVEATS.** The finding's separate-fixes conclusion holds. I
  could not construct a single unified exit-gate mechanism that survives the code. The
  finding's *strongest* argument is not the one it leads with (§2.2 "opposite outcomes",
  which is collapsible); the load-bearing, framing-proof argument is the
  **timeout-interpretation conflict** (below), which I verified directly in code. Two
  corrections to the finding's overconfidence are recorded in §4.

---

## 1. What I independently re-verified in code (anchors confirmed on 2453306)

- **Exit gate is trigger-blind (S1 shared symptom — CONFIRMED).**
  `attemptRecoveryFromDegraded` (manager_degraded.go:376-416) consults only
  `refreshAssignmentFromNATS` (a single `Get(assignment.<workerID>)` from `m.assignmentKV`,
  manager_assignment.go:1561-1571) and `currentAssignmentApplied` (a commitment-state
  compare, manager_assignment.go:1208-1217). It reads neither `m.bucketEpochs` (A) nor
  `m.kvErrorWindow` / any per-source KV-op health (B). Verified line-by-line.
- **`currentAssignmentApplied` is a commitment gate, not a trigger gate** (manager_assignment.go:1208-1217):
  compares `committedAssignmentOrEmpty()` vs the snapshot on `(Version, LeaderRevision,
  PartitionSetDigest, source-rev-when-known)`. Both NP-2 and NP-3a/b arm their fault only
  AFTER Stable (np3 test:180-186; NP-2 reaches Stable at np2 test:124-126 before the recreate
  at :150-159), so `committed == snapshot` and the guard PERMITS the exit. Confirmed.
- **Triggers are structurally disjoint.**
  - A: `enterDegraded("bucket-recreated:" + bucket)` fired directly from `checkBucketEpochs`
    (manager_setup.go:689). This path NEVER calls `recordKVError` and NEVER touches
    `kvErrorWindow`. NP-2 hard-codes the isolation: leaves `KVErrorThreshold` default
    (np2 test:81-83) and asserts `otherDegrades == 0` (np2 test:190-192). The .out shows
    `otherDegrades=0` (np2.out:4). So A is provably not a `kvErrorWindow` event.
  - B: `recordKVOpError` → `markKVUnavailable` → `recordKVError` → `kvErrorWindow` append →
    threshold → `enterDegraded("kv-unavailable")` (manager_degraded.go:235-237, 144-226,
    wired at manager_election.go:126; assignment watcher sites). Entirely a `kvErrorWindow`
    event.
- **No shared fault registry exists in production code.** Grep for `bucketEpochs` /
  `degradedReason` over non-test `*.go`: the only persistent per-trigger state is
  `m.bucketEpochs` (manager.go:110-116) for A and `m.kvErrorWindow` for B. The degrade
  *reason* is a transient string handed to `enterDegraded` and discarded — only
  `test/simulation/.../worker.go:947,980` caches it, never production. So a "verify the
  trigger recovered" gate genuinely has no existing common substrate to build on. Confirmed.
- **C2 isolation (peer takeover) is real** (manager_election.go:106-127): `ErrClaimLost` with
  a non-connectivity / non-degrading cause routes to `claimLostShutdown`, NEVER through
  `attemptRecoveryFromDegraded`. Neither family's fix touches it. Confirmed.

The proof `.out` evidence matches the report verbatim: NP-2 `degradedToStable=9
otherDegrades=0 finalState=Degraded connected=true` (np2.out:4); NP-3b `degradedExits=9
injected=36` connected (np3b.out:6,8); NP-3a PASS (np3a.out:4). Non-vacuity and isolation
guards are present and load-bearing in both tests (np2 test:165-167,172-174,190-197;
np3 test:255-260,267-268).

---

## 2. The decisive refutation of unification: the TIMEOUT-INTERPRETATION CONFLICT

The finding leads with §2.2 "opposite desired outcomes" (A: never exit; B: exit once op
recovers). A sharp reader collapses that: *it is ONE rule — "exit iff the triggering fault
has cleared" — applied to two faults, one permanent-by-construction (A), one transient (B).*
Under that reframing "opposite outcomes" dissolves into "same predicate, different fault
persistence," and the unification argument looks alive again.

So I went after the **strongest** unification candidate, a genuinely single routine (NOT the
"add both conjuncts" strawman the finding already dismissed as a labeling artifact):

> **U′:** before `exitDegraded`, re-validate EVERY Parti-owned bucket with one uniform check —
> "the bucket's stream-`Created` matches the cached epoch AND a probe op against it succeeds."
> Exit only if every bucket passes.
> This *looks* unified: NP-2 (Created mismatch → stay), NP-3b (op fails → stay), NP-3a (both
> pass → exit). One routine, one rule, all buckets.

**U′ dies on the code, not on semantics.** A and B respond *oppositely to the same
observable* — an op timeout against a bucket:

- **Epoch detection (A) treats a timeout as NON-ACTIONABLE.** `checkBucketEpochs` on a probe
  error logs `"epoch fence: probe failed; relying on next tick"` and `continue`s WITHOUT
  degrading (manager_setup.go:679-682). Only a *successful* read returning a different
  `Created` is actionable (`if !live.Equal(ep.created)`, :684-689).
- **KV-op detection (B) treats a timeout as THE FAULT ITSELF.** `markKVUnavailable` wraps a
  bare `context.DeadlineExceeded` / `nats.ErrNoResponders` into `ErrKVUnavailable`
  (manager_degraded.go:67-69); `recordKVError` admits exactly that and accumulates it toward
  the threshold (manager_degraded.go:164-167, 186).

Now run U′ under B's fault (NP-3b): re-validating the heartbeat bucket makes the
`BucketStreamCreated` read itself **time out** (the heartbeat bucket is in the fault set,
np3 test:142-147). U′ must decide what a timeout means:
- If U′ **blocks on the timeout** → correct for B, but it now blocks on a *transient probe
  error*, exactly the false-positive `checkBucketEpochs` was written to avoid (:679-682). It
  would wedge a worker Degraded forever on a single slow epoch probe — a regression A's own
  monitor explicitly refuses.
- If U′ **ignores the timeout** ("rely on next tick", A-correct) → B's fault is invisible to
  the gate; NP-3b still false-exits.

There is no single interpretation of a timeout that is correct for both. To get both right U′
must **tag which check the timeout belongs to** — i.e. split into an epoch-probe check (error
⇒ ignore) and a KV-op-health check (error ⇒ fault present). That tagging IS two mechanisms
with opposite error-handling sharing only an `if`-body. **Unification refuted at the
implementation layer, independent of how abstractly the predicate is phrased.** This is the
argument the finding should have led with; I verified every cited line.

Corollary (the finding's §2.1/§2.3, re-confirmed sharper): a KV-op-recovered gate is
structurally blind to A because a bucket recreate is never a KV-op error — there is no failing
op to verify recovered (manager_setup.go:689 bypasses `recordKVError` entirely). And an
epoch-mismatch gate is inert for B because B never produces a `Created` mismatch. Each gate is
necessary and neither covers the other family.

---

## 3. Attacking the recommended fixes (task step c)

### 3.1 A-opt-2 (epoch-aware exit guard) — strongest objection = C1 regression. DOES NOT survive.

Objection: A-opt-2 adds a pre-`exitDegraded` guard in the same block C1's recoverable
whole-bucket-loss path runs through; could it wedge `TestManager_LiveNATSBucketLoss`?

**Traced the test (not inferred):** `TestManager_LiveNATSBucketLoss`
(manager_live_bucket_loss_test.go:30-145) wipes the **assignment bucket** (line 86), CONFIRMS
it is gone (`require.ErrorIs(... ErrBucketNotFound)`, line 100), and ASSERTS the buckets stay
gone (lines 128-132) — workers are NEVER expected to recover to Stable; the test only checks
Degraded *entry* within 20s and then ends. On the recovery tick `refreshAssignmentFromNATS`'s
`Get(assignment.<workerID>)` returns `ErrBucketNotFound`, so `attemptRecoveryFromDegraded`
bails at manager_degraded.go:383-387 **before** the exit block. **A-opt-2's guard is never
reached on the C1 recoverable path** — confirmed by reading the test, not inferred. Same for
`TestManager_PartialBucketLoss_HeartbeatHealthy` (assignment also wiped, line 298). The
objection does NOT survive: A-opt-2 cannot regress C1. (Residual: a `-race` run is still owed
because the guard reads `m.bucketEpochs` on the connection-monitor goroutine while the epoch
monitor writes it — a real concurrency surface, not a correctness regression.)

### 3.2 B-opt-1 (verify-the-failing-op-recovered gate) — does it break the NP-3a control? NO.

NP-3a disarms the fault (np3 test:300); ops against heartbeat/election/stableid succeed again;
the `kvErrorWindow` drains via `recordKVSuccess` on the next recovery tick
(manager_degraded.go:393) AND a fresh healthy periodic op fires after `degradedSince` → the
B-opt-1 conjunct (`kvErrorCount == 0` + last-healthy-op-after-degradedSince) passes → exit →
NP-3a still recovers and HOLDS Stable (`require.Never(Degraded, 5s)`, np3 test:309-312).
Verified by construction; B-opt-1 narrows *when* B exits but still allows the legitimate exit.
The control is preserved.

Residual on B-opt-1 (inherited, not introduced): the F-D1 global-window-drain inherits the
documented semantic narrowing — a healthy heartbeat clears transient entries attributed to
slower buckets before threshold (04-fd1-flapping-decision.md:80-91). A global drain suffices
for the NP-3b *proof* (it faults heartbeat/election/stableid together, np3 test:142-147); a
real single-non-heartbeat-bucket quorum loss may need per-source counters. This is a
precision/coverage caveat on B-opt-1's strength, not a refutation of separateness.

---

## 4. Corrections to the finding's overconfidence (do not change the verdict)

1. **"A-opt-1 alone fails NP-1" is inferred, not traced — and may be wrong in direction.**
   In NP-1 the assignment bucket is wiped+recreated EMPTY. `refreshAssignmentFromNATS`'s
   `Get(assignment.<workerID>)` against an empty recreated bucket would return key-not-found →
   bail at manager_degraded.go:383-387 → **stay Degraded**, the OPPOSITE of the claimed
   "rests Stable on empty bucket" — UNLESS the still-running leader repopulates
   `assignment.<workerID>` before the recovery tick. The report's "all three rest Stable"
   (04-proof-findings.md:83-93) is *current-HEAD* behavior (no `ep.created` re-capture, so the
   epoch tick keeps the fleet alive long enough for the leader to repopulate), which does not
   directly establish A-opt-1's behavior. The finding inherits the report's confidence here.
   This is an A-opt-1-vs-A-opt-2 completeness question and is orthogonal to separate-vs-unified
   — flagging it so the fix plan traces NP-1 under A-opt-1 rather than asserting it.

2. The finding's discrepancy-with-report list (its 4 items) is accurate and survives scrutiny;
   I confirmed §7.1 (04-proof-findings.md:276-282) is internally muddled (it calls A-opt-2 "the
   shared exit fix" while also saying A "still needs the stale-`ep.created` latch addressed",
   when under A-opt-2 the staleness must be PRESERVED as the load-bearing terminal signal) and
   that §1 line 52 conflates shared-symptom with shared-fix.

---

## 5. Verdict

- **Root-cause mechanism (S1 shared symptom + S2 separate fixes): HOLDS.** Verified in code,
  not parroted. The exit gate is trigger-blind; the two triggers are structurally disjoint;
  no shared fault registry exists; the recommended A-opt-2 / B-opt-1 are genuinely independent
  conjuncts.
- **Unification (the report's S2 "one fix closes A+B"): REFUTED.** The strongest single-routine
  candidate (U′) dies on the timeout-interpretation conflict (manager_setup.go:679-682 vs
  manager_degraded.go:67-69): the same observable (an op timeout) is non-actionable for A's
  detector and IS the fault for B's, so no single predicate can interpret it correctly for
  both. "One function edited, two conjuncts" is a labeling artifact, not unification.
- **Fix attack (step c): both recommended fixes survive their strongest objections.** A-opt-2
  cannot regress C1 (traced: C1's recoverable path bails on the failed assignment read before
  the guard). B-opt-1 does not break the NP-3a control (window drains + healthy op ⇒ exit
  permitted). Residual risks are concurrency (-race owed) and B-opt-1's inherited F-D1
  coverage narrowing — neither refutes separateness.
- **Confidence: high.** Default verdict (separate fixes) was actively challenged via the
  hardest unification steelman I could build and survived.
