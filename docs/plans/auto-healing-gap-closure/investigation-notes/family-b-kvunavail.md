# Family B (NP-3b) — recover-on-wrong-signal under sustained connected-but-KV-unavailable

Independent code-derived deep dive. Investigation branch `auto-heal-gap-investigation`,
HEAD `2453306`. READ-ONLY: no production code changed. Every claim cites `file:line`
in the worktree
`/home/arlo/projects/parti/.claude/worktrees/auto-heal-gap-investigation/` or a test name.

This is the explicitly-deferred **"Finding A"** from
`docs/plans/auto-healing-quorum-loss-fix/04-fd1-flapping-decision.md` (§Investigation,
§Deferred). The proof `TestNP3_KVUnavailable_HeldArmed_DoesNotFalselyExitToStable`
turns that deferral into an executable FAIL.

---

## 0. TL;DR

- **(a) Wrong-signal exit CONFIRMED in code.** `attemptRecoveryFromDegraded`
  (`manager_degraded.go:376-416`) gates the exit on (1) a successful **assignment**
  read (`refreshAssignmentFromNATS`, `:383`) plus (2) `currentAssignmentApplied(cur)`
  (`:409`). It NEVER checks that the op that *triggered* the degrade recovered. The
  recovery driver is connection **uptime** (`checkConnectionHealth`,
  `manager_degraded.go:127-133`), which is satisfied throughout — the fault never drops
  the TCP connection. So while heartbeat/election/stableid keep timing out but the
  assignment bucket is readable, the manager exits Degraded→Stable; the still-faulting
  heartbeat re-accumulates to threshold → re-degrade → flap.

- **(b) Three Finding-A candidates evaluated.** "Verify the failing op recovered" is
  **not free**: there is **no per-source health signal and no stored degrade reason** to
  gate on. The reason is a transient string handed to `enterDegraded` (`:300`) and
  never persisted; `kvErrorWindow` is a flat `[]kvErrorEvent{at, transient}`
  (`manager.go:204`, `manager_degraded.go:33-36`) with **no source tag**. So candidate 2
  resolves to either (a) reuse the one existing per-source success signal — heartbeat
  `SetOnSuccess` (`manager_election.go:432`) — as a proxy "F-D1 KV health recovered"
  exit gate, or (b) build per-source tracking, which **collapses candidate 2 into
  candidate 3**. **Even option (a) is not a pure one-liner:** `attemptRecoveryFromDegraded`
  is the single recovery exit for ALL degrade reasons, so the gate MUST be reason-scoped
  to `kv-unavailable` — and that forces storing the degrade reason (new
  `m.lastDegradedReason`), which does not exist today. An *unconditional* gate regresses
  **NP-5** (startup-timeout recovery, an ungated GREEN proof) — VERIFIED in §4.2/§6.

- **(c) NP-9 doesn't flap because it faults the assignment bucket AND its Watch too**
  (`np9...:188`, `:142-148`), so `refreshAssignmentFromNATS` FAILS in
  `attemptRecoveryFromDegraded` (`:383`) → early return → never reaches `exitDegraded`.
  NP-3b deliberately **excludes** the assignment bucket from the fault set
  (`np3...:142-147`), so the read succeeds → false exit. Any fix must keep NP-9's clean
  one-entry/one-exit recovery (it currently passes).

- **(d) Recommendation = scope-dependent.** **B-only:** candidate 2 implemented as a
  *reason-scoped heartbeat-success-since-degrade* exit gate — smallest surface that
  preserves C1 (entry path untouched), preserves the f-d1 class-aware reset, holds NP-3b
  Degraded, does NOT regress NP-5 (because reason-scoped), and still recovers NP-9.
  **A+B unified:** the report's "single exit-gate fix closes
  both" (04-proof-findings.md:52, 280-282) is **loose** — it is true that the *exit
  defect* is shared, but the *fixing predicate differs by family*, so the honest unified
  form is a generalized per-cause "degrade-cause registry" (candidate 3's family), a
  meaningfully bigger design than a one-line exit tweak. See §5 (discrepancy) and §7.

---

## 1. The mechanism, re-derived from code (part a)

### 1.1 The recovery driver is connection uptime, fired every 1s

`monitorNATSConnection` ticks every 1s (`manager_degraded.go:79`) →
`checkConnectionHealth` (`:97`). With the connection UP (the whole premise of the
fault), it takes the else-branch at `:127`: once `connUpSince` has been set for
`ExitThreshold`, it calls `attemptRecoveryFromDegraded` (`:130-132`). The connection
never drops under a KV-unavailable fault, so `connUpSince` is set the moment the manager
starts and `attemptRecoveryFromDegraded` fires on **every 1s tick** for the entire
degrade.

### 1.2 The exit gate checks the WRONG op

`attemptRecoveryFromDegraded` (`manager_degraded.go:376-416`):

1. `:378` — bail if not degraded.
2. `:383` — `refreshAssignmentFromNATS()`. On error: log, `recordKVError(err)`, return
   (stay degraded). On success: continue.
3. `:393` — `recordKVSuccess()` clears the **entire** `kvErrorWindow` (both transient and
   whole-bucket entries — `:244-250`). This is the load-bearing aggravator: it wipes the
   heartbeat-attributed transient entries that *were* accumulating, resetting the
   re-degrade clock so the next flap cycle starts from zero.
4. `:408-412` — `cur := m.CurrentAssignment(); if !currentAssignmentApplied(cur) { scheduleApplyRetry; return }`.
5. `:415` — else `exitDegraded()` → `transitionState(StateStable)` (`:350`).

`refreshAssignmentFromNATS` (`manager_assignment.go:1561-1599`) reads exactly one key:
`assignment.<workerID>` via `m.assignmentKV.Get` (`:1568`). It touches **only the
assignment bucket**. It has zero knowledge of heartbeat/election/stableid health.

So the exit predicate is: *"connection up for ExitThreshold AND assignment-key read
succeeds AND that assignment is already applied"*. None of the three conjuncts observes
the failing op. In NP-3b the assignment bucket is unfaulted, all three are satisfied →
**false exit**.

### 1.3 Why the assignment-applied guard does NOT save us here

The NP-3b harness arms the fault only **after** the manager reaches Stable
(`np3...:176-191`, with `require.Equal(StateStable, ...)` at `:185` immediately before
`fc.arm()`). At that instant `committedAssignment == snapshot`, so post-fault
`currentAssignmentApplied(cur)` is **true** and the guard at `:409` is NOT taken — the
test deliberately drives the `exitDegraded` branch, not the `scheduleApplyRetry`
stay-degraded branch (test comment `np3...:180-187`). This is correct test design: it
isolates the pure exit defect, not the bootstrap/latched-apply path that the
applied-guard exists to cover.

### 1.4 The re-degrade half (the other half of the flap)

After the false exit, `degradedSince` is cleared (`exitDegraded` `:356`), so the
`recordKVError` short-circuit at `:174` (`if m.degradedSince.Load() != 0 { return }`) no
longer fires. The still-faulting heartbeat publisher keeps calling
`SetOnError → recordKVOpError → recordKVError` (`manager_election.go:424`,
`manager_degraded.go:235-237`). Each marked `ErrKVUnavailable` timeout
(`markKVUnavailable` `:58-72`, wrapping `context.DeadlineExceeded` from
`kuFaultKeyValue.Put` `manager_kv_read_unavailable_test.go:95-98`) re-appends to
`kvErrorWindow` (`:186`). After `KVErrorThreshold` (=3 in the test, `np3...:129`) it
re-enters Degraded with reason `kv-unavailable` (`:216-224`). → flap.

### 1.5 Why `recordKVHealthyOp` (421f13c) cannot help

`recordKVHealthyOp` (`manager_degraded.go:266-288`) fires only from heartbeat
**Put success** (`manager_election.go:432`). Under NP-3b the heartbeat bucket is **in the
fault set** (`np3...:145`), so heartbeat Put always returns `DeadlineExceeded`
(`...:95-98`) → `SetOnError` path, never `SetOnSuccess`. `recordKVHealthyOp` is never
called. The report (04-proof-findings.md:107-109) states this correctly. Additionally,
`recordKVHealthyOp` early-returns while degraded (`:267-269`), so even on a different
fault shape it would not record successes during a degrade.

### 1.6 Empirical confirmation (re-run on HEAD 2453306)

`tmp/repro-current-head/np3b.out`: `--- FAIL ... re-entries=9 injected=36` over a 13.03s
window. `injected` rising start→mid→end (test asserts `np3...:257-260`) proves the fault
was active throughout; `re-entries=9` means 9 Stable→Degraded re-entries, each of which
*requires* a prior false Degraded→Stable exit. Connection stayed CONNECTED
(`np3...:255-256`). The report cited `degradedExits=9, injected=34`; the re-run shows the
same magnitude (re-entries=9, injected=36). **Verdict on the report's root cause for
Family B: CONFIRMED.**

---

## 2. What signals actually exist to gate on (the crux of part b)

I grepped the entire non-test tree for any per-source error/health bookkeeping:

- `kvErrorWindow []kvErrorEvent` — `manager.go:204`. Each entry is
  `kvErrorEvent{at time.Time, transient bool}` (`manager_degraded.go:33-36`). **No
  source/bucket identity.** `transient` only distinguishes F-D1 timeouts from
  whole-bucket-loss; it does NOT say *which* op faulted.
- The degrade **reason** string (`"kv-unavailable"`, `"bucket-recreated:<b>"`,
  `"NATS connection down"`, …) is passed to `enterDegraded(reason)` (`:300`), logged
  (`:320-323`), and handed to the `OnDegraded` hook (`:328`). It is **never stored on the
  Manager**. There is no `m.lastDegradedReason`. (`degradedReason` only exists in
  `test/simulation/...`, not production.)
- The **only** per-source success signal in production is the heartbeat
  `Publisher.SetOnSuccess` (`internal/heartbeat/publisher.go:177-196`) wired to
  `recordKVHealthyOp` (`manager_election.go:432`). Election renew, stableid renew, and
  assignment refresh have **no success callback** — only error callbacks
  (`recordKVOpError` at `manager_election.go:262,303`, `manager_assignment.go:398,437,636`).

**Conclusion for part (b):** "Verify the failing op recovered" has no off-the-shelf
"failing op" identity. The cheapest real proxy is "has a periodic heartbeat **succeeded**
since we degraded?" — because heartbeat is the highest-frequency periodic KV op and is
the one source that already has a success hook. Any finer-grained answer requires new
per-source bookkeeping (candidate 3).

---

## 3. Why NP-9 doesn't flap but NP-3b does (part c) — and what the fix must not break

| | NP-3b (Family B, flaps) | NP-9 (full quorum loss, clean) |
|---|---|---|
| Fault set | Election, Heartbeat, StableID (`np3...:142-147`) | Election, Heartbeat, StableID, **Assignment** (`np9...:184-189`) |
| Watch faulted? | No — Watch passes through (`kuFaultKeyValue` has no Watch override) | **Yes** (`np9FaultKeyValue.Watch` `:142-148`) |
| `refreshAssignmentFromNATS` during recovery | `assignmentKV.Get` **succeeds** → exit predicate satisfied | `assignmentKV.Get` **faults** (`np9...:130-136`) → `attemptRecoveryFromDegraded` returns at `:383-387` → never exits |
| Fault lifecycle in test | **held armed** the whole window (`np3...` no disarm) | armed then **disarmed** (`np9...:233,253`) before asserting recovery |
| Result | Degraded↔Stable flap (9 re-entries) | exactly 1 entry / 1 exit (`np9...:267-270`) |

The discriminator is purely **whether the assignment Get succeeds while the trigger op is
still faulting**. NP-9's recovery is gated by the *same* assignment read that NP-3b
exploits — but in NP-9 that read also faults, so the wrong-signal gate happens to be
*correct by coincidence* (the assignment bucket is part of the same quorum loss). NP-9 is
NOT a counter-example to the defect; it's a case where the defective gate accidentally
agrees with reality.

**Fix-safety implication:** any exit-gate fix must remain *permissive* once the genuine
fault clears. NP-9's recovery path is the watcher-independent `Get`-based refresh
(`np9...:150-157` comment, executed after disarm). A heartbeat-success-since-degrade gate
(candidate 2) does **not** harm NP-9: after disarm, heartbeat Put resumes succeeding
(bucket is in the fault set but disarmed), so the gate opens and recovery proceeds. I
verified the NP-9 fault set includes the heartbeat bucket (`np9...:186`), so a heartbeat
success after disarm is guaranteed before the `WaitState(Stable, 20s)` at `np9...:254`.
The control NP-3a (`np3...:285-313`) is the analogous proof for Family B: after
`fc.armed.Store(false)` (`:300`), heartbeat resumes, recovery returns to Stable, and
HOLDS (`require.Never(Degraded, 5s)` `:309-312`).

---

## 4. Fix options (part b) — full map

All three are from `04-fd1-flapping-decision.md:118-123`. Common surface: the exit
decision in `attemptRecoveryFromDegraded` (`manager_degraded.go:376-416`) and/or the
re-entry path (`recordKVError` / `enterDegraded`).

### 4.1 Candidate 1 — post-recovery cooldown ("don't re-degrade for N seconds after exiting")

- **Mechanism.** After `exitDegraded`, stamp `m.recoveredAt = now`. In `recordKVError`
  (or `enterDegraded`), suppress re-entry for reason `kv-unavailable` while
  `now - recoveredAt < cooldown`. Surface: `manager_degraded.go` (new field +
  `exitDegraded` `:340-373` stamp + `recordKVError` `:144-226` suppress branch). ~25 LOC.
- **Blast radius.** Touches the re-entry path only; the exit predicate is unchanged.
- **Contracts.** **C1 RISK (real):** C1 says whole-bucket loss → every worker Degraded
  within a bounded window. A blanket cooldown that also suppresses **non-transient**
  (whole-bucket) re-entry would *delay* C1's re-entry after any prior recovery — it is
  the only candidate that perturbs C1's bounded *re-entry* timing. Must scope the
  cooldown to `transient`/`kv-unavailable` entries only (mirroring the class split the
  f-d1 work already established). Even so, it does not break C1's pinning tests
  (`TestManager_LiveNATSBucketLoss*`) — those kill heartbeat too and have no *prior*
  recovery to start a cooldown — but it weakens the contract's worst-case bound. f-d1
  class-aware reset: **untouched** (cooldown is orthogonal to the window).
- **Pros.** Smallest conceptual change; no new health signal needed.
- **Cons / residual risk.** **Does not fix the root cause** — it rate-limits the flap but
  the manager STILL falsely exits to Stable, just less often. The false-Stable *rest
  periods* remain, which is exactly what the M2 "keep readiness degraded under a real
  quorum loss" policy (04-proof-findings.md:119-120) is trying to prevent. NP-3b's hard
  gate is `require.Zero(degradedExits)` (`np3...:275`) — a cooldown that still permits ≥1
  false exit **does not pass the proof**. Band-aid; rejected as a standalone fix.

### 4.2 Candidate 2 — verify the F-D1 KV op recovered before exiting (RECOMMENDED for B-only)

- **Mechanism.** Add a heartbeat-success timestamp that updates **regardless of degraded
  state**, then gate the exit on "a heartbeat succeeded *after* we degraded". Concretely:
  - New `m.lastHeartbeatSuccessAt atomic.Int64` (UnixNano), stamped from a new tiny
    callback wired alongside `recordKVHealthyOp` at `manager_election.go:432` — or stamped
    inside a thin wrapper. **Gotcha (must not reuse `recordKVHealthyOp` as-is):**
    `recordKVHealthyOp` early-returns while degraded (`manager_degraded.go:267-269`), so
    it records nothing during a degrade — useless as a "recovered since degrade" signal.
    The new stamp must fire on heartbeat success unconditionally.
  - In `attemptRecoveryFromDegraded`, before `exitDegraded` (`:415`), add the gate.

  **CRITICAL — the gate MUST be reason-scoped, NOT unconditional.**
  `attemptRecoveryFromDegraded` (`manager_degraded.go:376-416`) is the **single recovery
  exit for EVERY degrade reason** — `kv-unavailable`, `NATS connection down`,
  `bucket-recreated:<b>`, **and `startup-timeout`** — not just F-D1. An *unconditional*
  gate `if lastHeartbeatSuccessAt <= degradedSince { return }` would therefore add a
  heartbeat-health precondition to **all** recoveries. That **regresses NP-5**
  (`TestNP5_BlockedApplyStartupTimeout_RecoversToStableAfterUnblock`, an **ungated,
  currently-GREEN** unit proof, 04-proof-findings.md:170-174):
    - **VERIFIED:** NP-5 uses `newTestManager` (`manager_commit_state_machine_test.go:152-173`),
      which injects a `recordingHeartbeat` stub (`:163`) and **never starts a real
      publisher** — `SetOnSuccess` is never wired and no periodic heartbeat success ever
      fires. NP-5 then calls `m.attemptRecoveryFromDegraded()` **directly**
      (`manager_np5_blocked_apply_recovery_test.go:158`), expecting Degraded→Stable
      (assertion 4, `:160-161`).
    - Under an unconditional gate, `lastHeartbeatSuccessAt` stays 0, `degradedSince` is
      non-zero, so `0 <= degradedSince` is TRUE → gate **closed** → NP-5 would NOT exit
      to Stable → **assertion 4 FAILS.** This is a real regression in a passing contract,
      independent of any production Start-ordering subtlety (the unit test never starts
      heartbeat at all).
  - **Therefore the gate must apply only to the F-D1 reason.** That requires storing the
    degrade reason on the Manager — which **does not exist today** (§2: the reason is
    passed transiently to `enterDegraded` `:300` and never persisted). So candidate 2
    needs a new `m.lastDegradedReason atomic.Pointer[string]` (or an
    `m.degradedIsKVUnavailable atomic.Bool`), set in `enterDegraded`. The gate becomes:
    `if reasonIsKVUnavailable && lastHeartbeatSuccessAt <= degradedSince { return }`. This
    is the **reason-scoped variant** — it is the only correct form.
- **Surface.** `manager.go` (2 fields: stamp + reason), `manager_degraded.go:300`
  (`enterDegraded` records the reason), `manager_election.go:432` (wire the stamp),
  `manager_degraded.go:376-416` (reason-scoped guard). ~25-30 LOC. No change to
  `recordKVError` threshold logic or the f-d1 window.
- **Blast radius.** Exit path + a new degrade-reason field set in `enterDegraded`. The
  reason field is read-only elsewhere; entry/threshold path otherwise untouched. **Note
  the blast radius is larger than "exit path only" once the NP-5-forced reason-scoping is
  accounted for** — storing the reason is new state on a contract-pinned struct.
- **Contracts.**
  - **C1 PRESERVED.** C1 is an **entry** contract; this is an **exit** gate scoped to
    `kv-unavailable`. Whole-bucket loss enters Degraded with reason
    `"KV error threshold exceeded"` (`manager_degraded.go:215`), NOT `kv-unavailable`, so
    the gate does not even apply to it — and on whole-bucket loss heartbeat never succeeds
    anyway, so even if it applied the gate would stay *closed* (stickier, never less).
    `TestManager_LiveNATSBucketLoss*` unaffected.
  - **C3 PRESERVED.** OnDegraded fires once per entry (`enterDegraded` CAS `:309`); this
    fix removes spurious *exits* (and the spurious *re-entries* that each fire OnDegraded).
    Net: fewer fires, still once-per-entry.
  - **C2 / C4 UNAFFECTED** (claim-lost shutdown and Start-returns-after-sanity are
    independent of this gate).
  - **f-d1 class-aware reset PRESERVED.** `recordKVHealthyOp`'s transient-only clear is
    untouched; the new stamp + reason are *separate* signals used only at exit.
  - **NP-5 (startup-timeout recovery) PRESERVED** — because the gate is reason-scoped, a
    `startup-timeout` degrade is not subject to the heartbeat-health check and recovers
    exactly as today. This is the load-bearing reason the gate must be scoped.
- **Pros.** Directly addresses the wrong-signal exit. Passes NP-3b's `require.Zero` gate
  (heartbeat never succeeds while armed → gate never opens → zero exits). Recovers NP-9
  and NP-3a (heartbeat resumes after disarm → gate opens). Does not regress NP-5. Far
  cheaper than candidate 3's per-source matrix.
- **Cons / residual risk.**
  - **Needs new degrade-reason storage** (forced by NP-5, above). This is the bookkeeping
    the fd1 doc associated with the heavier options; it is *small* here (one field + one
    write site) but it is real, and it nudges candidate 2 a step toward candidate 3 — it
    is no longer a pure "one-line exit guard". Be honest about this in review.
  - **Heartbeat-only proxy.** It gates exit on *heartbeat* recovery, not on the *specific*
    failing op. A pathological fault that knocks out **only** election or **only** stableid
    while heartbeat stays healthy would let this gate open and exit early — a residual
    narrower version of the same bug. But that exact shape would also have been *cleared*
    from the f-d1 window by `recordKVHealthyOp` (heartbeat success clears transient
    entries) and so likely never reaches the re-degrade threshold in the first place
    (04-fd1...:81-91, the accepted "semantic narrowing"). So this residual is *consistent
    with the already-accepted f-d1 coverage change*, not a new regression. (This is the
    one corner where candidate 2 and candidate 3 diverge observably — see §9.)
  - Subtle ordering: must stamp `lastHeartbeatSuccessAt` strictly *after* the Put returns
    success; reusing the same atomic the publisher already touches avoids a new lock.
  - Does NOT help Family A at all (see §5 / §6).

### 4.3 Candidate 3 — per-source error counters / generalized degrade-cause registry

- **Mechanism.** Replace the flat `kvErrorWindow` with per-source windows keyed by op
  source (heartbeat / election / stableid / assignment / commit), OR add an explicit
  "outstanding degrade causes" set that each trigger registers on enter and clears on its
  own recovery. Exit refuses while any cause is outstanding. `recordKVOpError` would carry
  a source tag (its 5 call sites already know their source:
  `manager_election.go:262,303`, `manager_assignment.go:398,437,636`,
  `manager_election.go:424` for heartbeat). Each source needs a **success** signal too —
  today only heartbeat has one (`SetOnSuccess`), so this requires adding success
  callbacks to election renew / stableid renew / assignment refresh.
- **Surface.** Large: `kvErrorEvent` gains a source field (`manager_degraded.go:33`); all
  5 `recordKVOpError` sites pass a source; `recordKVError`/`recordKVHealthyOp`/
  `recordKVSuccess` become per-source; new success callbacks in election/stableid/
  assignment subsystems. 100+ LOC across ≥4 files plus internal subsystems.
- **Contracts.** Can preserve C1 (whole-bucket entries still accumulate per their source,
  and a whole-bucket loss faults *all* sources so all counters trip) and the f-d1 reset
  (per-source transient clear). But the surface area means real regression risk to the
  exact contract-pinned paths the fd1 doc warns about (04-fd1...:122-124).
- **Pros.** Most precise: gates exit on the actual failing source's recovery. Is the
  *only* candidate that also gives Family A a clean lever IF the epoch fence is made to
  register an outstanding "bucket-recreated" cause (i.e. it generalizes to the per-cause
  registry — see §5).
- **Cons / residual risk.** Large blast radius into subsystems (heartbeat/election/
  stableid publishers) that currently have no success hook. The fd1 doc explicitly defers
  this as the heaviest option (04-fd1...:119-124). Highest review/test cost.

---

## 5. Discrepancy with the report (be adversarial)

**The report's "a single fix to the exit gate could close both A and B" is LOOSE**
(04-proof-findings.md:48-52, restated at 280-282). Precise correction:

- **Shared:** the *exit defect* — both families exit via `attemptRecoveryFromDegraded`
  on the assignment-read signal while a different trigger still fires. TRUE.
- **Different:** the *re-degrade trigger*, and therefore the *fixing predicate*.
  - Family B re-degrades through `recordKVError` → `kvErrorWindow` threshold
    (`manager_degraded.go:207-224`). A heartbeat-health exit gate (candidate 2) closes it.
  - Family A re-degrades through `checkBucketEpochs → enterDegraded("bucket-recreated:<b>")`
    fired **directly** (`manager_setup.go:684-690`), which **bypasses `kvErrorWindow`
    entirely** and never touches any KV-health signal.

I **verified** that NP-2 deletes+recreates the heartbeat bucket as a fresh **healthy**
MemoryStorage stream with no fault injection (`np2...:150-159`), and deliberately leaves
`KVErrorThreshold` at default so no `kv-unavailable` co-fires (`np2...:81-83`, asserts
`otherDegrades=0` `np2...:188-189`). So after the recreate, heartbeat **Put succeeds**
against the new bucket. Therefore a heartbeat-success exit gate (candidate 2) would see
healthy heartbeats and **OPEN** — it would NOT stop Family A's flap. Family A's exit is
driven back by the epoch tick re-firing on a stale `ep.created` (never re-captured,
`manager_setup.go:684-690`), which a KV-health gate does not observe.

**Honest unified-fix statement:** closing BOTH A and B with one mechanism requires a
*generalized* "refuse to exit while ANY degrade cause is outstanding" gate — each
trigger (kv-unavailable threshold, bucket-recreated epoch fence, …) registers a cause and
clears it on its own recovery. That is candidate 3's family, NOT a one-line exit tweak.
The report's prose understates this; the §7 deferred-fix note (04-proof-findings.md:280-282)
*does* hedge ("but A still needs the stale-`ep.created` latch addressed"), so the report
is internally inconsistent — the summary (line 52) oversells, the recommendation
(line 282) walks it back. Flag the summary as the imprecise line.

Otherwise the report's Family B section (04-proof-findings.md:95-120) is **accurate**: the
wrong-signal exit, the `recordKVHealthyOp`-can't-help reasoning, the NP-9 contrast
(line 184-185), and the NP-3a control are all confirmed against code. The cited evidence
(`degradedExits=9, injected=34`) matches the HEAD re-run (re-entries=9, injected=36) in
magnitude.

---

## 6. Verification trace (proves the recommended candidate 2 — reason-scoped variant)

For the **reason-scoped** heartbeat-success-since-degrade exit gate (§4.2): the gate
applies only when the outstanding degrade reason is `kv-unavailable`. ENTRY (C1) is
untouched because the gate is purely an EXIT predicate. Cases 1-5 exercise the KV path;
cases 6-7 exercise NON-KV recoveries that the reason-scoping must leave intact (the hole
the unconditional gate would have created).

1. **NP-3b held-armed** (`TestNP3_KVUnavailable_HeldArmed...`): reason is `kv-unavailable`
   → gate applies; heartbeat Put faults the whole window → `lastHeartbeatSuccessAt` never
   advances past `degradedSince` → gate stays closed → `require.Zero(degradedExits)`
   (`np3...:275`) PASSES. ✔ (gated proof becomes a regression guard — ungate per
   04-proof-findings.md:295).
2. **NP-9 post-disarm** (`TestNP9_FullQuorumLoss...`): reason is `kv-unavailable`
   (`np9...:240`) → gate applies; after `fc.disarm()` (`np9...:253`) heartbeat (in the
   fault set, `np9...:186`) resumes succeeding → stamp advances past `degradedSince` →
   gate opens → recovery proceeds. NP-9's `1 entry / 1 exit` invariant (`np9...:267-270`)
   still holds. ✔
3. **NP-3a disarm control** (`TestNP3_KVUnavailable_Disarm...`): reason `kv-unavailable`
   → gate applies; heartbeat resumes after `armed.Store(false)` (`np3...:300`) → gate
   opens → recovers and HOLDS (`require.Never(Degraded, 5s)` `:309-312`). ✔
4. **C1 whole-bucket loss** (`TestManager_LiveNATSBucketLoss*`): reason is
   `"KV error threshold exceeded"` (`manager_degraded.go:215`), NOT `kv-unavailable` →
   gate does NOT apply; recovery is unchanged from today. AND the **entry** path is
   untouched: non-transient entries still accumulate via `recordKVError`. Worker still
   enters Degraded within the bounded window. ✔
5. **Family A (NP-2)** — explicitly NOT covered: reason is `bucket-recreated:<heartbeat>`
   → gate does NOT apply (and heartbeat succeeds against the recreated bucket anyway).
   Still flaps. Confirms candidate 2 is B-only. ✔ (negative trace)
6. **NP-5 startup-timeout recovery** (`TestNP5_BlockedApplyStartupTimeout...`, ungated,
   GREEN): reason is `startup-timeout` → gate does NOT apply → `attemptRecoveryFromDegraded`
   reaches `exitDegraded` exactly as today → assertion 4 (`...np5...:160-161`) still
   PASSES. **This is the case the unconditional gate would have BROKEN** (§4.2): NP-5's
   `newTestManager` never starts heartbeat, so an unconditional gate is unconditionally
   closed. Reason-scoping is precisely what saves it. ✔
7. **NATS connection-down recovery** (the EnterThreshold/ExitThreshold path,
   `checkConnectionHealth` `:103-133`): reason `"NATS connection down"` → gate does NOT
   apply. (On a genuine reconnect, heartbeat would resume anyway, but reason-scoping makes
   this independent of heartbeat timing.) No regression to the connection-recovery
   contract. ✔

---

## 7. Recommendation (part d) and justification against the fd1 doc

**Pick by scope.**

- **If this work item is Family-B-only:** implement **candidate 2** as a **reason-scoped**
  heartbeat-success-since-degrade exit gate (§4.2) — the gate fires only for a
  `kv-unavailable` degrade, which requires storing the degrade reason
  (`m.lastDegradedReason`, new state set in `enterDegraded`). It is the smallest *correct*
  surface that (i) makes NP-3b pass its `require.Zero` gate, (ii) preserves C1 (entry path
  untouched; reason scoping means the gate does not even apply to the whole-bucket
  `"KV error threshold exceeded"` reason), (iii) preserves the f-d1 class-aware reset
  (separate signal), (iv) keeps NP-9/NP-3a recovery, and (v) **does not regress NP-5** —
  the load-bearing reason it must be reason-scoped, since an unconditional gate closes
  NP-5's heartbeat-less unit recovery (VERIFIED §4.2/§6). Its only residual — gating on
  heartbeat rather than the *exact* failing op — is *already-accepted coverage* under the
  f-d1 "semantic narrowing" (04-fd1...:81-91): an election/stableid-only fault that leaves
  heartbeat healthy is the same shape the f-d1 reset already lets through, so candidate 2
  introduces no *new* regression relative to the shipped f-d1 decision.

- **If A and B are to be closed together:** do **NOT** rely on the report's implied
  one-line shared fix. The honest unified design is **candidate 3's family** — a
  generalized per-cause/per-source outstanding-degrade-cause registry where the epoch
  fence also registers a `bucket-recreated` cause (and Family A *additionally* needs the
  stale-`ep.created` re-capture latch, since even a perfect exit gate leaves the epoch
  tick re-firing). This is bigger and touches the contract-pinned subsystems.

**Why this aligns with the fd1 decision record.** The fd1 doc deferred Finding A precisely
because it "touches the contract-pinned recovery path" and was "narrower… taken only if
observed" (04-fd1...:54-58, 116-124). NP-3b *is* the observation that retires the
deferral. Of the three candidates the doc lists, it explicitly frames them as a spectrum:
cooldown (band-aid) → verify-the-failing-op (targeted) → per-source counters (heaviest).
Candidate 2 is the doc's middle option realized in its **lightest faithful form** (reuse
the one existing per-source success hook as the "failing op recovered" proxy), which is
the right answer when the goal is to close B without paying candidate 3's full
subsystem-wide cost — and it leaves the door open to upgrade to candidate 3 later if a
single non-heartbeat-bucket quorum loss is ever observed to escape (the exact escape the
fd1 doc flagged at 04-fd1...:87-91).

**Reject candidate 1** as a standalone fix: it does not pass NP-3b's `require.Zero` gate
and weakens C1's bounded re-entry; useful at most as a defensive add-on, not the fix.

---

## 8. Cross-family interactions

- **Family A (NP-2/NP-1).** Shares the exit defect (§5) but NOT the re-degrade trigger.
  Candidate 2 does not touch it. A unified close needs candidate 3 + epoch re-capture.
  Sequencing: if both land, the epoch-fence re-capture (or an "epoch mismatch outstanding"
  registry entry) is independent of the KV-health gate and can be reviewed separately.
- **Family C (NP-8).** Distinct mechanisms (claim-lost self-stop / MemoryStorage
  heartbeat-bucket loss, 04-proof-findings.md:122-164). Note one *adjacency*: NP-8 mech 2
  is a heartbeat-bucket loss that surfaces as `"stream not found"` in the leader
  calculator (`internal/assignment/worker_monitor.go:175,218`) — a whole-bucket
  (non-transient) classification, NOT an F-D1 transient. So candidate 2's heartbeat-health
  gate is irrelevant to NP-8 (the heartbeat Put there fails as whole-bucket loss, keeping
  the gate closed — correct, it should stay degraded). No conflict.
- **C2 (peer-claim takeover) / C4 (Start contract).** Independent of the recovery exit
  gate; no interaction.
- **f-d1 Finding B (recordKVHealthyOp, 421f13c).** Candidate 2 lives *beside* it (separate
  always-on stamp vs the degraded-gated transient-clear); both keep the heartbeat as the
  canonical "KV serving" source, so they are conceptually coherent.

---

## 9. Open questions / things I did NOT verify empirically

- I did not *run* a prototype of candidate 2; the verification trace (§6) is derived from
  code + the existing gated proofs, not from a patched binary. The gated proofs
  (`PARTI_RUN_NP3_KVUNAVAIL_FLAP_PROOF=1`, NP-9 ungated) are the right validation harness
  once a fix lands. **Mandatory regression check when candidate 2 lands:** re-run the
  ungated **NP-5** proof (`go test ./... -run TestNP5_BlockedApplyStartupTimeout`) — it is
  the contract an unconditional gate would silently break, and reason-scoping is what keeps
  it GREEN. Also re-run NP-9 (recovery must still complete) and the `LiveNATSBucketLoss`
  C1 pinning tests.
- **NP-5 regression under an unconditional gate is VERIFIED by code reading, not by a
  patched run:** NP-5's `newTestManager` never starts a heartbeat publisher
  (`manager_commit_state_machine_test.go:163`, injects a stub) and calls
  `attemptRecoveryFromDegraded()` directly (`...np5...:158`), so an unconditional
  `lastHeartbeatSuccessAt <= degradedSince` gate is unconditionally closed there. I did not
  execute a patched binary to observe the failure; the conclusion is from the unit test's
  construction. Confidence high but flagged as inference-from-code, not an observed fail.
- The heartbeat-only proxy's residual (§4.2 cons) — an election/stableid-only sustained
  fault with healthy heartbeat — is *argued* to be already-covered by the f-d1 reset, not
  proven by a dedicated test. If A+B reviewers want certainty, add a proof that faults
  ONLY election (heartbeat healthy) and asserts it either (a) never reaches threshold
  (f-d1 reset wins) or (b) holds Degraded under candidate 3. This is the one corner where
  candidate 2 and candidate 3 diverge observably.
- `ExitThreshold`/`EnterThreshold`/`RecoveryGracePeriod` production defaults (5s/10s/15s;
  conservative preset 10s/30s/20s — `config.go:196,191,215,250-254`) mean a *production*
  flap period is ~seconds-to-tens-of-seconds, slower than the test's accelerated config;
  the *mechanism* is identical, only the cadence differs (consistent with the report's
  §6 production-extrapolation caveat).
