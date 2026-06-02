# Adversarial verification — Family A (NP-2 + NP-1) epoch-fence finding

Target: `family-a-epoch.md` (+ its JSON verdict). HEAD = `2453306` on
`auto-heal-gap-investigation`. Read-only. Every claim cites `file:line` or a test name.
Goal was to **refute**, not confirm. Net: the root cause holds; the finding's headline
recommended fix (**latched** Opt-2) is **overstated as "necessary AND sufficient, high
confidence"** — its sufficiency for NP-1 is not established and its own
self-suppression argument is mis-stated for NP-1. The fix the finding *dismissed*
(live re-probe at exit) is the robust one.

Verdict: **confirmed-with-caveats.**

---

## 1. Root-cause mechanism — independently re-derived, HOLDS

### Half (i): epoch fence never re-captures `ep.created` (re-arm)
- `captureBucketEpoch` writes the cached Created exactly once: map init at
  `manager_setup.go:613`, sole assignment at `:627`. Verified by grep — the only
  writers of `m.bucketEpochs` are `manager_setup.go:613` and `:627`; the only reader
  outside `manager_setup.go` is the field decl `manager.go:116`. **Zero readers in the
  exit path.** (grep of `bucketEpochs` across non-test `.go`.)
- `checkBucketEpochs` (`manager_setup.go:669-693`) reads live Created via cached
  `ep.kv` (`:677`), fires `enterDegraded("bucket-recreated:"+bucket)` on
  `!live.Equal(ep.created)` (`:684,:689`), then `return`s (`:690`) — **never updates
  `ep.created`.** Mismatch is therefore permanent for the process lifetime.
- Tick cadence = `OperationTimeout` (`manager_setup.go:649-652`). **Important and used
  below: NP-2 overrides this to 1s (`np2_..._test.go:76`); NP-1 does NOT override it,
  so it is the 10s default** (`config.go:410` `default:"10s"`; `IntegrationTestConfig`
  does not set it).

### Half (ii): recovery exits on the wrong signal
- Connection monitor ticks 1s (`manager_degraded.go:79`); conn never drops in
  NP-1/NP-2 so it stays on the up-branch and, once up for `ExitThreshold`, calls
  `attemptRecoveryFromDegraded` every tick (`:130-131`).
- `attemptRecoveryFromDegraded` (`manager_degraded.go:376-416`) refreshes ONLY the
  assignment bucket (`refreshAssignmentFromNATS` → `assignmentKV.Get("assignment.<id>")`,
  `manager_assignment.go:1567-1568`) and gates exit solely on
  `currentAssignmentApplied` (`:409`), whose predicate (`manager_assignment.go:1208-1217`)
  compares Version/LR/digest/source-rev and **never consults `m.bucketEpochs`.** The
  wipe signal is structurally invisible to the exit. CONFIRMED.

### Exit-path completeness (a refutation attempt that FAILED — supports the finding)
I tried to break Opt-2's sufficiency by finding a `Degraded→Stable` edge that does NOT
go through `attemptRecoveryFromDegraded`/`exitDegraded` (which Opt-2 gates). Result:
**there is none.**
- Transition matrix `manager_state.go:160-171`: `StateDegraded` exits ONLY to
  `{StateStable, StateShutdown}`.
- The other `transitionState(StateStable)` callers cannot fire from Degraded:
  `stopCalculator` (`manager_assignment.go:277`) only fires from
  `Scaling/Rebalancing/Emergency` (`:271-272`); `syncStateFromCalculator`
  (`manager_state.go:220-232`) only maps Idle→Stable from `isCalculatorOwnedActiveState`
  (excludes Degraded); `markStartupAssignmentApplied` (`manager_startup_async.go:114-128`)
  is CAS-once on `startupAssignmentApplied` (`:115`) — already true post-startup in
  NP-1/NP-2 so it no-ops, and its `:128` `transitionState(StateStable)` is gated behind
  `isCalculatorOwnedActiveState` (`:121`).
- `exitDegraded` is the **only** `Degraded→Stable` edge, and it is reached only via
  `attemptRecoveryFromDegraded:415`. So gating that one function covers the exit. The
  finding's structural premise (gate the one exit) is sound. **Checked & cleared.**

### Version-monotonicity sub-path (finding §(b)) — HOLDS
`p.currentVersion` is in-memory (`assignment_publisher.go:106`); `DiscoverHighestVersion`
only ever RAISES it (`:882-884` commit-seed, `:917-919` legacy-scan), never lowers. So
after the wipe the leader keeps its pre-wipe version and republishes higher into the
empty bucket. Corroborated by `np1.out`: pre-wipe versions `map[worker-0:2 worker-1:2
worker-2:2]`, and worker-0 shows `Stable→Rebalancing→Stable` right after the wipe (a real
recalc+republish). VERIFIED. This is a genuine addition the report omits.

---

## 2. Proof non-vacuity / isolation — checked, the proofs are sound

**NP-2** (`np2_..._test.go`): non-vacuity guard `bucketRecreatedDegrades>=1` (`:172`);
sanity that recreate produced a strictly-later Created (`:165`); isolation
`otherDegrades==0` (`:190`) and `connected==true` (`:196`); primary hard invariant
`require.Zero(degradedToStable)` (`:204`). `np2.out`: `degradedToStable=9,
otherDegrades=0, finalState=Degraded, connected=true`. The CAS argument is valid:
`enterDegraded` CAS-guards `degradedSince` (`manager_degraded.go:309`), so 10 entries
require 9 intervening `exitDegraded`s — irrefutable oscillation. PROOF VALID, non-vacuous.

**NP-1** (`np1_..._test.go`): non-vacuity `all 3 reach Degraded` (`:207-210`); hard
invariant `require.Empty(healed)` where `healed` = workers with a `Degraded→Stable`
edge strictly after `recreatedAt` (`:79-87,:248`). `np1.out`: `healed=[0 1 2]`,
all end Stable on empty buckets. PROOF VALID. The discriminator (bucket-recreated
fired) is logged, not gated — correct (it would otherwise couple the proof to the bug
mechanism).

One nuance I checked and discarded as a refutation: OnStateChanged and OnDegraded are
dispatched **asynchronously** (`invokeHook` → `m.wg.Go`, `manager.go:1026-1034`), so the
`transitions` and `reasons` slices in `np1Probe` have NO cross-goroutine ordering
guarantee. Any argument of the form "the first `Degraded→Stable` precedes the first
`bucket-recreated` reason because the slices are co-ordered" is **invalid**. (The proof
itself does not rely on such co-ordering, so the proof is unaffected.)

---

## 3. Attack on the recommended fix — the finding's "necessary AND sufficient (high)" is OVERSTATED

The finding recommends **latched** Opt-2: set `m.epochFenceTripped` at
`manager_setup.go:689` (inside `checkBucketEpochs`, on the first post-recreate fence
*observation*) and `AND !epochFenceTripped` into the exit gate. Three corrections, two
of which the finding gets backwards.

### 3a. Opt-1-alone-fails-both-proofs — HOLDS (the finding's strongest correct point)
Traced against the unchanged exit gate, re-capturing `ep.created` after `enterDegraded`
quiets the fence but leaves half (ii) intact, so the next recovery tick exits to Stable
and the worker RESTS Stable: NP-2 `degradedToStable=1` (fails `:204`), `finalState=Stable`
(fails `:212`); NP-1 `healed=[0 1 2]` (fails `:248`). Strictly worse than the flap
(removes the recurring Degraded windows a readiness probe samples). This directly
contradicts `04-proof-findings.md:278` ("the proofs are agnostic to which fix lands").
The finding is RIGHT here, and the report IS imprecise. CONFIRMED.

### 3b. The finding's self-suppression mechanism is MIS-STATED for NP-1
The finding (family-a-epoch.md:293-298; JSON openQuestion #1) argues latched-Opt-2 is
sufficient-and-complete because *"the first `bucket-recreated` entry sets the latch,
then `degradedSince` stays set so `enterDegraded`'s CAS no-ops the repeat
`bucket-recreated` entries."* **That causal chain is false in NP-1.**

In NP-1 every worker is **already Degraded via `kv-unavailable`** before any
`bucket-recreated` fires (`np1.out` reasons all start `[kv-unavailable,
bucket-recreated:*, ...]`; the wipe trips the KV-error threshold path
`recordKVError`→`enterDegraded` BEFORE the recreate even happens). So the
`bucket-recreated` `enterDegraded` at `manager_setup.go:689` ALREADY no-ops on the held
`degradedSince` — *with or without* Opt-2. The latch is armed not by a "first
bucket-recreated entry" (there is none under latched-Opt-2 from a fresh window) but by
the fence *observation* (`checkBucketEpochs` mismatch), independent of the CAS. The
finding conflated NP-2 (where the first degrade IS the fence, so latch-on-entry ==
latch-on-observation) with NP-1 (where it is not). Under latched-Opt-2 in NP-1, the
correct statement is "**zero** bucket-recreated OnDegraded entries fire," not
"repeats are suppressed." The finding's openQuestion #1 is mis-framed.

### 3c. Latched-Opt-2 sufficiency for NP-1 is NOT ESTABLISHED — and the cadence cuts against it
The latch arms only on the **first post-recreate fence observation**. Before that, the
exit gate is byte-identical to unfixed code. So latched-Opt-2 fails NP-1 iff a worker
completes a `Degraded→Stable` exit in the window **[republish lands] → [first
post-recreate fence tick arms the latch]**.

What I verified about that window:
- The exit cannot fire in phase 2 (buckets deleted) or before the leader republishes:
  `refreshAssignmentFromNATS` does `assignmentKV.Get("assignment.<id>")`
  (`manager_assignment.go:1568`); on a deleted/empty bucket it errors / key-not-found,
  so `attemptRecoveryFromDegraded` returns early (`:387` / via the predicate). So the
  earliest possible exit is gated on the leader republish chain. (This kills a naive
  "a pre-latch exit necessarily exists in phase 2" claim — it does not.)
- BUT the cadences are lopsided in NP-1: the recovery loop attempts an exit **every 1s**
  (`manager_degraded.go:79`; conn up long ago, so `ExitThreshold` is already satisfied),
  while the epoch fence — and thus the latch — only fires **every 10s** in NP-1
  (`OperationTimeout` default, NOT overridden; §1). NP-2's "fence fires fast" intuition
  used a 1s fence (`np2_..._test.go:76`) and does not transfer.
- The leader republish path is **not gated on Degraded** (no `StateDegraded`/
  `degradedSince` guard in `manager_election.go` or `assignment_publisher.go` — grep
  empty), and `np1.out` shows the leader actually does `Stable→Rebalancing→Stable`
  (recalc+republish) post-wipe. `enterRecoveryGracePeriod` is entered only AFTER
  `exitDegraded` and gates post-exit rebalancing, not the exit itself
  (`manager_degraded.go:370,418-438`).

So in NP-1 there is a **~10s window per fence period** in which the leader can
republish a higher-version assignment and the 1-Hz recovery loop can satisfy
`currentAssignmentApplied` and `exitDegraded` **before** the next 10s fence tick arms
the latch. Whether the very first such exit beats the first post-recreate fence tick is
**timing-dependent and unproven** — the finding asserts it passes NP-1 with "high
confidence" without establishing this. The 10s fence cadence makes the window large, so
the conservative read is that **latched-Opt-2 may still allow >=1 exit and FAIL NP-1's
`require.Empty(healed)`.** Note the current (unfixed) `np1.out` already shows multiple
`Degraded→Stable` edges interleaved with the 10s `bucket-recreated` ticks — direct
evidence that exits routinely occur inside the inter-fence gaps. The latch does not
close the FIRST such gap.

### 3d. The finding INVERTED latch vs live-reprobe
The finding calls the latch "simplest and race-free" and treats
`epochMismatchOutstanding()` — a live re-probe of `m.bucketEpochs` at exit time
(family-a-epoch.md:217-220) — as a lesser variant. **Backwards on "race-free."** The
re-probe form has **no pre-arm window**: `ep.created` is pinned pre-wipe permanently
(half (i)), so any exit attempt after the recreate sees the mismatch inline and refuses;
before the recreate, `refreshAssignmentFromNATS` fails anyway. It is robust to placement
and to the 10s-vs-1s cadence skew. The **latch** is precisely what introduces the
(non-trivial, 10s-wide) race. So the robust recommendation is the form the finding
dismissed.

> Caveat on the re-probe form: it adds one `BucketStreamCreated` (stream-info) probe per
> bucket on each 1s recovery tick while Degraded. That is a real (small) extra KV cost
> and a possible new failure surface — but `checkBucketEpochs` already swallows probe
> errors with `continue` (`manager_setup.go:679-682`), so an errored probe at exit time
> should be treated conservatively (refuse exit OR fall through — design decision). Worth
> a config/perf note, not a blocker. A cheap middle ground also exists that the finding
> did not consider: arm the latch from the threshold/`recordKVError` path too, or set it
> the instant ANY bucket epoch is known-stale rather than only on the fence tick — but
> the live re-probe is the cleanest "no pre-arm window" answer.

---

## 4. Contract / control regressions — checked

- **C1** (whole-bucket-missing → bounded Degraded entry, `TestManager_LiveNATSBucketLoss`):
  entry is via `recordKVError`→`enterDegraded` (`manager_degraded.go:144-225`), untouched
  by an exit-gate change. `np1.out` confirms `kv-unavailable` is the first reason on every
  worker. UNAFFECTED by either Opt-2 form.
- **C2** (peer claim takeover → only that worker self-stops, `onClaimerError`,
  `manager_election.go:106-127`): different reason class; the epoch latch / re-probe is
  set only by the epoch fence, never by `onClaimerError`. UNRELATED. UNAFFECTED.
- **C3** (OnDegraded once per entry): IMPROVED under either form (terminal Degraded = one
  entry, not N). Note §3b: under latched-Opt-2, NP-1 fires **zero** bucket-recreated
  entries, which is *fine* for C3 but undercuts the finding's stated rationale for why
  Opt-1 is unnecessary.
- **C4** (Start returns after sanity phase): unrelated.
- **NP-3a disarm control / NP-5 blocked-apply recovery**: both must keep exiting Degraded
  normally. Under latched-Opt-2 the flag is set ONLY by the epoch fence, so non-epoch
  reasons (kv-unavailable cleared, startup-timeout) still exit. Under the live-reprobe
  form, `epochMismatchOutstanding()` returns false when no bucket was recreated (the
  probe equals the cached created), so those recoveries also still exit. **Neither form
  regresses NP-3a/NP-5.** (Verified by construction; not re-run — proofs are caller-owned.)
- **NP-9** (full quorum loss incl. assignment bucket): the assignment bucket is faulting,
  so `refreshAssignmentFromNATS` fails and recovery cannot falsely exit anyway; an
  exit-gate AND-clause is inert there. UNAFFECTED.

---

## 5. Discrepancies with the finding (not just the report)

1. Finding's recommended fix (**latched** Opt-2) sufficiency for NP-1 is **overstated**:
   asserted "necessary AND sufficient, high confidence, passes NP-1" without establishing
   the latch arms before the first post-recreate exit. The 10s NP-1 fence cadence vs 1s
   recovery cadence makes this a real, ~10s-wide window. (§3c)
2. Finding's self-suppression mechanism (its reason Opt-1 is "unnecessary once Opt-2
   lands") is **mis-stated for NP-1**: `kv-unavailable` already holds `degradedSince`, so
   the CAS already no-ops the bucket-recreated entry regardless of Opt-2; the latch is
   armed by the fence *observation*, not a bucket-recreated *entry*. (§3b)
3. Finding **inverted** latch-vs-reprobe: the live re-probe is the race-free form; the
   latch is what introduces the race. (§3d)

The finding's corrections of the *report* (Opt-1-alone fails both proofs; the proofs are
NOT fix-agnostic; "one function, two predicates" for A+B; version-monotonicity) are all
**valid and survive** — those are the finding's real contribution.

---

## 6. Cross-family interaction (A vs B)
Confirmed composable: A's fence calls `enterDegraded` directly and never touches
`kvErrorWindow` (`manager_setup.go:689`), while B (NP-3b) lives entirely in
`recordKVError`/`kvErrorWindow`/threshold (`manager_degraded.go:144-225`). A unified
`attemptRecoveryFromDegraded` exit gate would AND two independent predicates (A:
epoch-mismatch-outstanding; B: failing-op-recovered). The finding's "one function, two
predicates" framing is correct; the report's "one fix closes both" is the imprecise
version. For A, the predicate should be the **live re-probe** (`epochMismatchOutstanding`),
not the latch — same reasoning as §3d.

---

## 7. Bottom line
- Root-cause mechanism (both halves) + version-monotonicity: **HOLDS**, independently
  re-derived from code.
- Proofs NP-2/NP-1: **VALID, non-vacuous, well-isolated.**
- Opt-1-alone-fails / report-is-imprecise: **HOLDS** (finding's real win).
- Opt-2 **necessity**: holds. Opt-2 **latched-form sufficiency for NP-1**: **NOT
  established / likely insufficient** due to a ~10s pre-arm race; the finding's
  "high-confidence necessary AND sufficient" is overstated.
- Correct fix surface: gate the single exit (`attemptRecoveryFromDegraded`), but with the
  **live re-probe** `epochMismatchOutstanding()` (the finding's dismissed variant), not
  the latch — the re-probe has no pre-arm window. If the latch is kept for simplicity, it
  MUST be armed before any post-recreate exit (e.g. on the first whole-bucket-loss degrade
  or via a synchronous probe), and the fix's test must assert NP-1's FIRST exit is
  blocked, not merely that the worker eventually settles Degraded.

Verdict: **confirmed-with-caveats.**
