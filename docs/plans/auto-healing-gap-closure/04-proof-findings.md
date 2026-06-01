# Auto-Healing Proof Findings (Non-Proven Scenario Sweep)

Date: 2026-06-01
Branch/worktree: `main` (worktree `auto-healing-gap-closure`)
Method: each non-proven auto-heal scenario from `tmp/parti-non-proven-auto-heal-scenarios.md`
was turned into a focused executable proof, then run **serially** (one test at a time,
`-run X -count=1`) against current `main` so verdicts are free of parallel-load
artifacts. Surprises were re-run in isolation and root-caused with diagnostics.

> Scope discipline: this task **only proves and characterizes** behavior. No
> production code was changed. All confirmed gaps are **deferred** to a follow-up
> fix session. The failing proofs are committed env-gated (opt-in) so `make test`
> stays green; remove the gate when the matching fix lands and they become
> regression proofs. Per-finding confidence is in §6.

---

## 0. Doc reconciliation (read this first)

`tmp/parti-non-proven-auto-heal-scenarios.md` (dated 2026-05-31) predates the
auto-healing-gap-closure work that landed on `main` the same day (commits
`a11e4fd` test proofs, `991d0e7` docs). Two of its highest-priority "non-proven"
items are now **proven on current main**, verified this session by running the
existing tests:

| Doc item | New status | Evidence (run this session) |
|---|---|---|
| **NP-6** finite `MaxReconnects` exhaustion → closed conn, never falsely Stable | **PROVEN** (M6) | `TestFullNATSOutage_FiniteReconnects_DegradesClosedConnection` PASS (5.54s) |
| **NP-7** NATS server restart, unlimited reconnect → single worker returns Stable | **PROVEN** (M5) | `TestFullNATSOutage_UnlimitedReconnects_RecoversFleet` PASS (5.34s) |

The remaining doc items and the newly-discovered cases are characterized below.

---

## 1. Verdict summary

| Scenario | Property under test | Verdict | Proof (gated?) |
|---|---|---|---|
| NP-5 | blocked startup Apply → Degraded(`startup-timeout`) → recovers to Stable after unblock | **AUTO-HEALS (proven)** | `TestNP5_BlockedApplyStartupTimeout_RecoversToStableAfterUnblock` (ungated, PASS 0.14s) |
| NP-4 | synchronous `Start()` failure → returns error, auto-Stops, no in-process heal, must Start fresh | **CONTRACT confirmed (no-heal)** | `TestNP4_SyncStartFailure_NoSelfHeal_StableIDFault` (ungated, PASS 3.03s) |
| NP-9 | full quorum loss (all 4 coordination buckets + Watch) → reason arbitration + recovery | **AUTO-HEALS (proven)** | `TestNP9_FullQuorumLoss_KVUnavailableWins_RecoversToStable` (ungated, PASS 4.68s) |
| NP-3a | connected-but-KV-unavailable, then fault **cleared** → recovers and holds Stable | **AUTO-HEALS (proven, control)** | `TestNP3_KVUnavailable_Disarm_ReturnsAndHoldsStable` (ungated, PASS 9.68s) |
| **NP-3b** | connected-but-KV-unavailable **held active** → must not falsely exit Degraded | **GAP: flap** (deferred Finding A) | `TestNP3_KVUnavailable_HeldArmed_DoesNotFalselyExitToStable` (gated FAIL) |
| **NP-2** | one Parti bucket recreated under a live worker → must stay terminally Degraded (M4) | **GAP: flap** | `TestNP2EpochFence_NonAssignmentRecreate_DegradedDoesNotFlap` (gated FAIL) |
| **NP-1** | operator wipes+recreates **all** buckets under a live 3-worker fleet → must not heal in-process | **GAP: fleet flap + false-healthy** | `TestNP1_LiveBucketRecreate_MustNotReturnToStable` (gated FAIL) |
| **NP-8** | 3-manager fleet across a NATS server restart → all return Stable, one leader | **GAP: no fleet auto-heal** (2 mechanisms) | `TestNP8FleetNATSOutage_LeaderContinuityRecoversFleet` (mech 1, gated FAIL) + `TestNP8FleetNATSOutage_HeartbeatBucketLossFlap` (mech 2, gated FAIL) |

Four confirmed gaps in three families below. Family C is fully distinct; **Families
A and B share their load-bearing half** — the recover-on-wrong-signal exit in
`attemptRecoveryFromDegraded` (exits to Stable on a healthy *assignment* read while a
*different* trigger is still firing). A adds a stale-epoch latch on top of that exit
defect. A single fix to the exit gate could close both A and B.

---

## 2. Confirmed gaps

### Family A — epoch-fence recreate → recovery-exit FLAP (NP-2, NP-1)

**Root cause (two interacting facts).** (i) `checkBucketEpochs`
(`manager_setup.go:684-690`) fires `enterDegraded("bucket-recreated:<bucket>")` on a
stream-`Created` mismatch but **never re-captures** `ep.created`; the cached epoch is
written once at startup (`manager_setup.go:627`) and stays stale forever, so the epoch
tick keeps re-degrading. (ii) Meanwhile the connection monitor
(`manager_degraded.go:97-134`, 1s tick) calls `attemptRecoveryFromDegraded`
(`manager_degraded.go:376-416`) on every tick (the connection never dropped), which
refreshes only the assignment bucket (`refreshAssignmentFromNATS`,
`manager_assignment.go:1561-1568`), gates the exit solely on `currentAssignmentApplied`
(`:409-415`), and `exitDegraded()`s to Stable — i.e. it recovers on the wrong signal.
Fact (ii) is the **same recover-on-wrong-signal exit defect as Family B**; A only adds
the stale-epoch latch (i) that keeps re-arming the Degraded side. The two ticks fight →
a sustained **Degraded↔Stable flap**, violating the M4 contract of *terminal* Degraded
for operator rotation. Orthogonal to commit `421f13c` (`recordKVHealthyOp` clears the
`kvErrorWindow`; the epoch fence calls `enterDegraded` directly and never touches that
window).

- **NP-2** (minimal reproducer): single worker, recreate the **heartbeat** bucket once.
  Evidence: `bucketRecreatedDegrades=10, degradedToStable=9, otherDegrades=0,
  finalState=Degraded, connected=true` over a ~10s window (≈1 Hz oscillation).
  The hard invariant `require.Zero(degradedToStable)` failed with 9. Because
  `enterDegraded` CAS-guards `degradedSince`, ≥2 `bucket-recreated` entries can only
  occur after intervening exits → irrefutable oscillation.
- **NP-1** (realistic, **more severe**): live 3-worker fleet, operator wipes then
  recreates **all four** buckets empty (no restart). Evidence: `healed=[0 1 2]` — all
  three workers flapped ~8 Degraded↔Stable cycles each, `bucket-recreated:*` fired on
  all three, and **all three ended in `Stable` on empty recreated buckets**
  (false-healthy: a readiness probe would mark them Ready despite lost coordination
  data). Run time 67.7s. This violates the documented "live data loss → degraded +
  restart/rotation, no in-process heal" contract (`manager_live_bucket_loss_test.go`,
  `docs/OPERATIONS.md`).

Severity: **High.** NP-1's false-healthy resting state is the worst outcome — it can
route work to a worker whose coordination state was wiped.

### Family B — connected-but-KV-unavailable → recover-on-wrong-signal FLAP (NP-3b)

This is the **explicitly-deferred "Finding A"** from the F-D1 work
(`docs/plans/auto-healing-quorum-loss-fix/04-fd1-flapping-decision.md`) made
executable — confirmation, not a new discovery.

**Root cause.** A connected-but-KV-unavailable fault times out
election/heartbeat/stableid but leaves the connection UP and the assignment bucket
readable. `attemptRecoveryFromDegraded` fires on **connection uptime** every 1s
(`manager_degraded.go:97-134`) and exits Degraded as soon as the *assignment* read
succeeds and `currentAssignmentApplied` is true — i.e. it recovers on the wrong
signal, never checking that the *failing* op recovered. The still-faulting heartbeat
re-accumulates to threshold and re-degrades → flap. `421f13c`'s `recordKVHealthyOp`
cannot help: it only fires on a heartbeat-**Put success**, and the heartbeat bucket
is itself faulting.

Evidence: with the fault held armed, `degradedExits=9` (hard `require.Zero` failed),
`injected=34` (fault genuinely active throughout), `kvUnavailable` OnDegraded fired
≥2 (re-entry proves the flap), connection stayed CONNECTED. Run time 13.0s.

Positive control **NP-3a** PASSES: once the fault is genuinely **cleared**, the
manager recovers to Stable and **holds** it (`require.Never(Degraded, 5s)`). This
proves NP-3b's exit is a *false* exit, not a dead recovery path.

Severity: **Medium-High.** Readiness oscillates while a real KV quorum loss persists,
defeating the "keep readiness degraded" M2 policy.

### Family C — NATS server restart → fleet does NOT auto-heal (NP-8)

The single-worker M5 proof (`TestFullNATSOutage_UnlimitedReconnects_RecoversFleet`)
recovers to Stable, but a **3-manager fleet** does not. Two **distinct** mechanisms,
each with its own gated proof and supporting diagnostic evidence:

1. **Outage ≥ `WorkerIDTTL` → claim-loss self-stop** (proof:
   `TestNP8FleetNATSOutage_LeaderContinuityRecoversFleet`, gated). The stableID
   bucket's MaxAge is reconciled to `WorkerIDTTL` (`config.go:366`). When the outage
   exceeds it, the worker-ID claim ages out; on reconnect each worker hits
   `stableid.ErrClaimLost` → `claimLostShutdown` (the **deliberate split-brain
   self-stop**, `manager_election.go:107-118`) and ends in **`StateShutdown`** — never
   returning to Stable. Diagnostic (`WorkerIDTTL`=5s, ~5s outage): all 3 workers
   `Degraded→Shutdown @8.2s` with `OnError: "worker ID claim lost: ID worker-N"`.
2. **MemoryStorage heartbeat-bucket loss → fleet flap** (proof:
   `TestNP8FleetNATSOutage_HeartbeatBucketLossFlap`, gated). The heartbeat bucket is
   `MemoryStorage` (`manager_setup.go:156`) and is gone after a single-node restart.
   With `WorkerIDTTL` raised above the outage (claims survive, so mechanism 1 is
   excluded): no self-stop, but the fleet **oscillates Degraded↔Stable and never holds
   Stable** — every worker that becomes leader fails
   `failed to list heartbeat keys: nats: stream not found` in its calculator. The proof
   reaches all-Stable transiently then the HOLD check trips within ~2s (flap period).

**Causal evidence that mechanism (1) is TTL expiry, not data loss** (the load-bearing
argument): the two diagnostics use the *same* ~5s outage (outage `@3.2s`, reconnect
`@8.2s`) and change **only** `WorkerIDTTL` (5s vs 30s); at 5s every worker self-stops
with "claim lost", at 30s none do. One variable, one effect. (Secondary: M5 passes
with a short outage, so a single worker's claim is intact on reconnect → FileStorage
persists. The `"wrong last sequence: 0"` election error seen at reconnect is
*consistent with* TTL expiry — the FileStorage stream survived but its time-bounded
lease key aged out — not with data loss, which surfaces as `"stream not found"`, the
MemoryStorage heartbeat case.)

Severity is split:
- **Mechanism (1): Low / doc-only.** `claimLostShutdown` is intended split-brain
  safety; it is **per-worker, not fleet-specific** (a single worker with a
  >`WorkerIDTTL` outage hits it too — M5 dodges it only by being short), and at the
  **75s** production default (`config.go:370`) it needs a multi-minute outage. The fix
  is to **document** that M5's "recover to Stable" is bounded by `WorkerIDTTL`.
- **Mechanism (2): Medium-High (operational), topology-dependent.** A real **RF3
  cluster rolling restart** that keeps replicated MemoryStorage alive may not hit it;
  this single-node embedded restart exercises the worst case (full MemoryStorage loss).
  This is the genuinely fleet-specific gap.

---

## 3. Proven (no gap)

- **NP-5** — a handoff `Apply` blocked during startup drives Degraded(`startup-timeout`)
  via the watchdog; the runner cannot self-exit (`casToStableFromWaitingAssignment`
  CAS fails from Degraded); after the Apply unblocks, `attemptRecoveryFromDegraded`
  heals the same process to Stable (single `startup-timeout` entry, no re-arm). Proves
  the "…unless the runner recovers" clause of the startup-timeout taxonomy.
- **NP-4** — a synchronous-phase `Start()` failure (faulted stableID claim) returns
  `"failed to claim worker ID"`, auto-Stops to `StateShutdown`, does **not** self-heal
  after the fault clears (no recovery goroutine was ever spawned), and a second
  `Start` returns `ErrAlreadyStarted`. Confirms the caller-must-Start-fresh contract.
- **NP-9** — under full quorum loss (all four coordination buckets + Watch faulting,
  connection UP), the fast heartbeat/election threshold path wins the `enterDegraded`
  CAS, so the first reason is **`kv-unavailable`** (not `assignment-watcher-exhausted`);
  after the fault clears the manager recovers to Stable via the watcher-independent
  Get-based refresh, with exactly one Degraded entry / one Stable exit (no flap).
  Note the contrast with NP-3b: NP-9 faults the assignment bucket too, so recovery
  *cannot* falsely exit while the fault is active — which is why NP-9 does not flap.
- **NP-3a** — see Family B (positive control).

---

## 4. New non-proven cases (answer to "are there more?")

Beyond NP-1…NP-7, the sweep surfaced these previously-undocumented non-proven cases.
They should be added to the matrix / scenario doc:

- **NP-8a — claim-loss self-stop boundary on connection-down.** "Recover to Stable
  when NATS returns" (M5) holds only for outages **shorter than `WorkerIDTTL`**. A
  longer outage → `ErrClaimLost` → `claimLostShutdown` → rotation-required, not
  in-process heal. Currently unproven and undocumented on the M5 row.
- **NP-8b — MemoryStorage heartbeat-bucket loss → fleet flap.** A NATS restart that
  loses the (MemoryStorage) heartbeat bucket leaves any worker that becomes leader
  unable to enumerate active workers; the fleet oscillates Degraded↔Stable and never
  holds Stable. Topology-dependent (RF3 replication may save it); worst case proven
  here (`TestNP8FleetNATSOutage_HeartbeatBucketLossFlap`).
- **NP-2/NP-1 epoch-fence recovery-exit flap** (Family A) — the M4 row currently
  proves *Degraded entry* only; the *return-to-Stable* behavior is a flap, not the
  terminal Degraded the contract claims.

The completeness hunt found **no** additional realistic in-process auto-heal gap:
M1 (RF3 handoff-claim quorum loss) is data-plane-proven and manager-vacuous; a
leader-only NATS partition needs a new selective-disconnect harness (deferred); M10
stream-missing recovery is caller-owned policy, out of scope for a Parti guarantee.

---

## 5. Test artifact inventory

| File | Package | Tests | Gating |
|---|---|---|---|
| `manager_np5_blocked_apply_recovery_test.go` | `parti` (root) | NP-5 | ungated (PASS) |
| `test/integration/manager/np4_sync_start_failure_no_heal_test.go` | `manager_test` | NP-4 | ungated (PASS) |
| `test/integration/failure/np9_full_quorum_loss_arbitration_test.go` | `failure_test` | NP-9 | ungated (PASS) |
| `test/integration/manager/np3_kv_unavailable_recovery_test.go` | `manager_test` | NP-3a (ungated PASS), NP-3b (gated FAIL) | `PARTI_RUN_NP3_KVUNAVAIL_FLAP_PROOF=1` |
| `test/integration/failure/np2_epoch_fence_return_to_stable_test.go` | `failure_test` | NP-2 | `PARTI_RUN_NP2_EPOCH_FLAP_PROOF=1` |
| `test/integration/manager/np1_live_recreate_returns_stable_test.go` | `manager_test` | NP-1 | `PARTI_RUN_NP1_LIVE_RECREATE_PROOF=1` |
| `test/integration/failure/np8_fleet_nats_outage_leader_continuity_test.go` | `failure_test` | NP-8 mech 1 (`...RecoversFleet`), NP-8 mech 2 (`...HeartbeatBucketLossFlap`) | `PARTI_RUN_NP8_FLEET_OUTAGE_PROOF=1` / `PARTI_RUN_NP8_HEARTBEAT_FLAP_PROOF=1` |

Run a gated proof, e.g.:
`PARTI_RUN_NP2_EPOCH_FLAP_PROOF=1 go test ./test/integration/failure -run TestNP2 -count=1 -v`

NP-6/NP-7 are existing tests on `main`; their PASS this session is captured in
`tmp/np-results/np6_np7_fullnatsoutage.out` (untracked; `tmp/` is gitignored).

---

## 6. Confidence assessment

Shared basis for every verdict: each run was **serial** (no parallel-load artifacts);
each failing proof asserts a **deterministic invariant** with non-vacuity guards; an
independent critic verified every cited count matches the captured `.out` evidence
verbatim and that the code root-causes hold.

| Finding | Verdict | Confidence | Basis / caveat |
|---|---|---|---|
| NP-2 epoch-fence flap | gap | **Very high** | 9 flaps in ~10s; isolation guards held (`otherDegrades=0`, connected); single hard code fact (`ep.created` never re-captured); same mechanism reproduced by NP-1. |
| NP-1 fleet flap | gap | **Very high** | Ran twice (original + post-refactor), identical `healed=[0 1 2]`, ~8 cycles/worker. |
| NP-3b false-exit flap | gap | **Very high** | 9 false exits with `injected=34` (fault active); independently corroborated as the documented deferred "Finding A", not a fresh guess. |
| NP-5 blocked-apply recovery | proven | **Very high** | Unit-level, asserts real state transitions (CAS-from-Degraded fails, committed V=1). |
| NP-4 sync-Start no-heal | proven | **Very high** | Deterministic contract assertions (`ErrAlreadyStarted`, stays Shutdown). |
| NP-3a disarm control | proven | **High** | Positive control; `require.Never(Degraded, 5s)` after recovery. |
| NP-9 arbitration + recovery | proven | **High, one caveat** | Recovery + no-flap solid. "`kv-unavailable` wins" rests on a **timing race** (fast threshold vs slow watcher exhaustion); it won deterministically here but a different config/load could flip the winner. |
| NP-8 mech 1 claim-loss self-stop | gap | **High behavior, contextual severity** | Deterministic, reproduced 3×, **causally proven** by the TTL contrast (one variable). But likely working-as-designed (split-brain safety), per-worker not fleet-specific, needs a multi-minute outage at the 75s prod default. |
| NP-8 mech 2 heartbeat-loss flap | gap | **High in harness, MEDIUM for prod** | Deterministic in the single-node embedded restart. **Topology caveat:** a real RF3 rolling restart may keep replicated MemoryStorage alive and not hit it (flagged as reasoning, not measured). |
| NP-6 / NP-7 now proven | proven | **Very high** | Existing `main` tests; PASS captured this session. |

What bounds confidence:

- Tests use a **single-node embedded NATS** with **accelerated config** (`WorkerIDTTL`=5s,
  sub-second thresholds). Findings that are timing- or topology-dependent — **NP-8 mech 2**
  (RF3 may save it) and **NP-9 arbitration** (race) — carry a production-extrapolation
  caveat. The flap/self-stop *mechanisms* are real code paths; their *production impact*
  depends on NATS topology and outage duration.
- Runs were for pass/fail correctness, **not `-race`** (the invariants do not need it). A
  `-race` pass over the hook-goroutine-touched harness code would be a cheap extra check.

To raise confidence further (not done here):

1. Run the gated failing proofs with `-race -count=5` to confirm flap stability.
2. Reproduce **NP-8 mech 2 on a real 5-node cluster** (the repo's gated `quorum_loss_tier2`
   harness) to settle the RF3-topology question — the single biggest open uncertainty.

---

## 7. Deferred fix recommendations (NOT done here)

1. **Family A (NP-2/NP-1) — epoch fence vs recovery.** Either re-capture `ep.created`
   after `enterDegraded` so the fence latches once, or make
   `attemptRecoveryFromDegraded` refuse to exit while an epoch mismatch is
   outstanding. The proofs assert the invariant and are agnostic to which fix lands.
   Prioritize NP-1's false-healthy resting state.
   Because Family A's exit defect is shared with Family B, a single fix to the
   `attemptRecoveryFromDegraded` exit gate (option 2) may also resolve A's recovery
   half — but A still needs the stale-`ep.created` latch addressed.
2. **Family B (NP-3b)** — implement the deferred Finding A fix (post-recovery cooldown
   / verify the failing op recovered before exiting / per-source error counters), per
   `04-fd1-flapping-decision.md`. Then ungate NP-3b.
3. **Family C (NP-8) — two separate items.** (1, mechanism 1) **Document** that M5's
   recover-to-Stable is bounded by `WorkerIDTTL` (claim-loss self-stop is intended
   split-brain safety, not a bug); update the M5/M6 matrix row. (2, mechanism 2) Make
   fleet recovery resilient to a missing MemoryStorage heartbeat bucket
   (recreate-on-reconnect, or a calculator path that tolerates an empty heartbeat
   bucket during recovery). When fixed, ungate `TestNP8FleetNATSOutage_*` — the
   `...RecoversFleet` proof guards mechanism 1, `...HeartbeatBucketLossFlap` guards
   mechanism 2.

When a fix lands, remove the matching `PARTI_RUN_*` gate so the proof runs as a
regression guard.
