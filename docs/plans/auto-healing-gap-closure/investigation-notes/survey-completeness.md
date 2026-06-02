# Cross-Cutting Survey: Completeness Challenge + Uncertainty Scoping

Branch/worktree: `auto-heal-gap-investigation` @ HEAD `2453306`.
Method: independent code re-derivation against the source under this worktree,
challenging `04-proof-findings.md` rather than parroting it. Every claim cites
`file:line` or a test name. VERIFIED = read in code this session; INFER = reasoned
from verified facts but not directly observed.

Contract IDs (C1..C4) refer to AGENTS.md "Cross-feature contracts" (restated in
the task brief).

---

## Part 0 — What I independently confirmed (so the challenges below are grounded)

- **Family A mechanism (VERIFIED).** `captureBucketEpoch` writes `ep.created`
  exactly once at Start (`manager_setup.go:627`); `checkBucketEpochs` fires
  `enterDegraded("bucket-recreated:"+bucket)` on a `Created` mismatch and
  `return`s, but **never re-captures** `ep.created` (`manager_setup.go:684-690`).
  The cached epoch stays stale forever, so the `OperationTimeout`-cadence tick
  (`monitorBucketEpochs`, default 10s) keeps re-degrading. The 1s connection
  monitor (`checkConnectionHealth`, `manager_degraded.go:97-134`) calls
  `attemptRecoveryFromDegraded` because the connection never dropped; that exits
  on a healthy **assignment** read (`manager_degraded.go:383-415`). The two ticks
  fight → flap. Report §2 Family A is **accurate**.

- **Family B mechanism (VERIFIED).** `attemptRecoveryFromDegraded` gates the exit
  solely on `refreshAssignmentFromNATS()` success + `currentAssignmentApplied(cur)`
  (`manager_degraded.go:383,409-415`). It never checks the *failing* op recovered.
  `recordKVHealthyOp` only clears `transient` entries on a heartbeat **Put
  success** (`manager_degraded.go:266-288`, wired at `manager_election.go:432`), so
  it cannot help when the heartbeat bucket itself is the faulting op. Report §2
  Family B is **accurate**, and the "shared exit defect with Family A" framing
  (§1) is correct.

- **C2 routing (VERIFIED).** `onClaimerError` routes `ErrClaimLost` to
  `claimLostShutdown` ONLY when the wrapped cause is neither connectivity nor
  degrading-JetStream (`manager_election.go:107-118`); the bucket-loss sub-branch
  feeds `recordKVError` instead. Peer takeover (bucket fine) → single-worker
  Shutdown; whole-bucket loss → fleet-wide degraded. Matches C2.

- **No reconnect-triggered bucket creation exists today (VERIFIED).** Buckets are
  created/opened only in the synchronous Start path (`manager.go:568`
  `ensureCoreKVBuckets`, `manager_setup.go:92/176`). No monitor goroutine
  re-provisions on reconnect. So the C2/A interactions in Part 1 are **fix
  risks**, not current bugs.

---

## PART 1 — Completeness challenge (break the §4 "no additional gap" claim)

### Monitor-goroutine inventory (the search space)

Started at/after Start: `monitorNATSConnection` (1s), `monitorBucketEpochs`
(`OperationTimeout`), `monitorAssignmentChanges`, `monitorCommitChanges`,
`monitorCalculatorState` (leader), `monitorLeadership` (`ElectionTimeout/3`),
`monitorDegradedAlerts`, the heartbeat publisher loop (`HeartbeatInterval`), the
`startStartupTimeoutWatchdog`, the handoff coordinator sweep, and — leader only —
the `WorkerMonitor` (watcher + `hbTTL/2` poll) plus the `Calculator`'s
poll/rebalance/audit loops.

I checked each for the "connection up but a subsystem silently wedged /
falsely-healthy" shape. Results:

| Goroutine | Feeds degraded circuit on sustained KV error? | Evidence |
|---|---|---|
| `monitorAssignmentChanges` | YES (`recordKVOpError` + `assignment-watcher-exhausted`) | `manager_assignment.go:398,412` |
| `monitorCommitChanges` | YES (`recordKVOpError`) | `manager_assignment.go:636` |
| heartbeat publisher | YES (`SetOnError(m.recordKVOpError)`) | `manager_election.go:424` |
| election renew/acquire | YES (`recordKVOpError`) | `manager_election.go:262,303` |
| stableID renew | YES (via `onClaimerError`→`recordKVError`/`recordKVOpError`) | `manager_election.go:114,126` |
| epoch monitor | YES (`enterDegraded`) | `manager_setup.go:689` |
| **`WorkerMonitor` poll (leader)** | **NO — logged only** | `internal/assignment/worker_monitor.go:282-284` |
| **`Calculator` worker-enumeration** | **NO — bare error, logged in caller** | `calculator.go:1213`; poll caller is logged-only |

The last two rows are the break.

### BREAK CANDIDATE — NP-10: leader-side silent worker-enumeration stall (False-healthy leader)

**Claim:** §4's "the completeness hunt found **no** additional realistic
in-process auto-heal gap" is too strong. There is a distinct in-process gap on the
**leader** that none of NP-1..NP-9 exercise and that is *worse* than NP-3b because
it does not even flap — it is fully silent.

**Discriminating trigger (the scenario nobody proved).** The heartbeat bucket is
reachable on a live connection but its **`Keys` scan times out** (quorum-loss /
slow-RAFT / large-bucket deadline) while the worker's **own single-key heartbeat
`Put` keeps succeeding**. This is a realizable asymmetry: a `Keys` enumeration is
a stream-wide operation, a `Put` is a single-subject append; under partial quorum
degradation or load the scan can deadline while the append still commits.

**Why it is silent (load-bearing code facts, all VERIFIED):**
1. `WorkerMonitor.GetActiveWorkers` returns `context.DeadlineExceeded` wrapped as
   `"failed to list heartbeat keys: %w"` (`worker_monitor.go:175`).
2. `Calculator.getActiveWorkers` only consults its cache / degrades for
   `natsutil.IsConnectivityError(err)` (`calculator.go:1197-1213`). For everything
   else it `return nil, false, err` (`:1213`).
3. **`IsConnectivityError` does NOT match a bare `context.DeadlineExceeded`**:
   the sentinel list (`internal/natsutil/errors.go:120-129`) does not include it,
   and the string fallback only catches `"connection refused"` / `"i/o timeout"`
   (`:135-136`) — not `"context deadline exceeded"`. (The manager's own KV-op
   sites paper over this with `markKVUnavailable`, `manager_degraded.go:58-72`, but
   the calculator/worker-monitor path has **no equivalent wrap**.)
4. The leader's poll loop logs the returned error and continues
   (`worker_monitor.go:282-284`); the rebalance/poll callers in the calculator
   log and continue. **None routes to `m.recordKVError`** — the calculator has no
   wiring into the manager degraded circuit at all (verified: no `SetOnError`
   bridge from calculator to manager; only the heartbeat publisher, watchers,
   election, and stableID feed it).

**Consequence.** A leader whose heartbeat-scan deadlines persistently: (a) holds
`StateStable` (its own heartbeat Put succeeds, assignment reads succeed, election
renew succeeds — so no circuit trips); (b) is **blind to worker topology** — it
sees `getActiveWorkers` fail, falls back to a stale cache or an empty list, and
keeps publishing assignments computed from stale membership. A readiness probe
marks it Ready. This is a **false-healthy leader serving stale/incorrect
assignments**, the same failure *class* the report rates "worst" for NP-1, but
reached by a realistic partial-degradation trigger and **without any operator
signal**.

**Relation to known findings.** This is the **leader-side analog of NP-3b**, but
NP-3b at least *flaps* (the heartbeat Put is the faulting op there, so the circuit
trips and re-trips). Here the scan path has **no circuit at all**, so it is
silent. It is also adjacent to the C-mech-2 surface (§Part 2 below) but distinct:
mech-2 is a *missing* bucket (`ErrStreamNotFound`, degrading-JetStream class,
which DOES degrade via the heartbeat Put); NP-10 is a *reachable-but-slow* bucket
(`DeadlineExceeded`, unclassified, which does NOT).

**Severity: High. Confidence: Medium** (code-derived this session; NOT proven with
an executable harness — that is the honest gap vs. NP-1..NP-9). A proof would
inject a `Keys`-only deadline on the heartbeat bucket on the leader while leaving
`Put`/assignment `Get` healthy, then assert the leader neither degrades nor holds
a correct assignment. The report's §4 dismissals (M1 data-plane-proven, leader
partition deferred, M10 caller-owned) do **not** cover this scan-deadline path.

**Caveat / steelman of the report.** One could argue this collapses into "a
leader-only NATS partition needs a selective-disconnect harness (deferred)." It
does not: a *partition* drops the connection (connection monitor catches it); this
is connection-UP with a per-operation deadline on one bucket — exactly the
F-D1/kv-unavailable shape the manager's own sites guard but the
calculator/worker-monitor path does not. So it is a genuinely separate omission.

### Cleared (not gaps)

- `monitorCommitChanges` — feeds `recordKVOpError` on watch-restart failure
  (`manager_assignment.go:636`), same as the assignment watcher. Properly wired.
- `monitorCalculatorState` — pure local state mirror; its only error is
  `syncStateFromCalculator` (logged), no KV dependency that can silently wedge
  readiness independent of the calculator itself.

---

## PART 1b — Cross-family interactions where fixing one gap re-opens another

### Interaction X1 (Family A re-arm via recreate-on-reconnect) — the named lead

If the **Family C / Family A** fix recreates a missing bucket on reconnect
(one of the §7.3 "recreate-on-reconnect" options for mech-2, or a re-provision to
heal NP-1), the recreated bucket's backing stream gets a **new `Created`
timestamp**. `monitorBucketEpochs` compares the live `Created` against the
**Start-time** cached `ep.created` (`manager_setup.go:684`, never re-captured),
so on every *other* worker the epoch fence fires `enterDegraded("bucket-recreated:
<b>")` — **re-opening Family A** fleet-wide. VERIFIED: the fence has no
"recreated-by-us" suppression and no re-capture. Any recreate fix MUST also
re-capture `ep.created` (and ideally distinguish self-recreate from operator
wipe), or it trades C for A. This is the strongest concrete interaction.

### Interaction X2 (C2 masking via broad re-provision)

The task named C2 specifically. A **broad** re-provision-on-reconnect that
recreates the **stableID** bucket would change `ErrClaimLost` classification:
`onClaimerError` (`manager_election.go:107-118`) routes a claim loss whose cause
is degrading-JetStream (bucket/stream missing → `IsDegradingJetStreamError`,
`errors.go:97-99`) into the `recordKVError` whole-bucket branch, NOT into
`claimLostShutdown`. If a reconnect handler has just recreated/emptied the
stableID bucket, a **legitimate peer takeover** that surfaces around the same
window could be misread as "bucket loss" → degraded-and-ride instead of the
intended self-stop, risking split-brain (two workers believing they hold the same
ID). This is **conditional** on a broad-recreate fix being chosen (INFER from
verified routing); a narrow heartbeat-only recreate does not trip it. Flag it so a
C-mech-2 fix is scoped to the heartbeat bucket, not a blanket re-provision.

### Interaction X3 (exit-gate hardening vs. NP-9 arbitration / startup)

The shared A/B fix candidate — "refuse to exit Degraded until the *failing* op
recovered" — must not regress the **C4** contract (Start returns after the
sanity phase, not StateStable) or NP-9's clean single-entry recovery. NP-9
recovers via the watcher-independent Get-based refresh (`refreshAssignmentFromNATS`,
`manager_assignment.go:1561-1599`) precisely because it faults the assignment
bucket too; a per-source "did the failing op recover" gate must treat the
assignment Get as the recovery signal for the assignment-faulting case, or it
could deadlock recovery (no op ever re-probed). INFER; the NP-9 row in §3 already
notes "recovery cannot falsely exit while the fault is active," so the gate has to
preserve that property, not just add a blanket cooldown.

### Asymmetry the report under-states (calibration)

§1 says "a single fix to the exit gate could close both A and B." True for the
*exit half*, but the three families want **different terminal outcomes**, so one
fix does NOT close all three:
- **B (kv-unavailable held):** correct outcome is **auto-recover** when KV
  returns. Exit-gate fix + per-source recovery check is right.
- **A (epoch recreate):** correct outcome is **terminal Degraded** (operator
  rotation) — AND it needs the stale-`ep.created` latch fixed; the exit gate alone
  leaves the fence re-arming.
- **C-mech-2 (MemoryStorage heartbeat bucket gone):** nothing recreates a
  MemoryStorage bucket in-process, so the correct outcome is **terminal Degraded
  pending operator/recreate**, NOT auto-recover. An over-eager "auto-recover when
  the failing op returns" would never fire (the op never returns) — which is
  actually the *desired* terminal behavior, but only if the exit gate is the thing
  holding it, not a flap.

---

## PART 1c — Mech-2 attribution: the report is IMPRECISE (not wrong)

Report §2 Family C mech-2 says the flap is because "every worker that becomes
leader fails `failed to list heartbeat keys: nats: stream not found` in its
calculator." That attributes the **degraded *entry*** to the leader calculator.
The calculator's enumeration error is **logged-only** and never calls
`enterDegraded` (Part 1 table; `worker_monitor.go:282-284`, `calculator.go:1213`).

The actual degraded-*entry* driver is the **heartbeat publisher's `Put`**: after a
single-node restart the MemoryStorage heartbeat bucket's stream is gone, so the
Put fails with `ErrStreamNotFound` (degrading-JetStream class, `errors.go:97-99`)
→ `recordKVOpError` → `recordKVError` accumulates at the 500ms cadence to
`KVErrorThreshold` → `enterDegraded("KV error threshold exceeded")`
(`manager_degraded.go:207-224`). Recovery exits on the **FileStorage assignment
read** (survives the restart) → flap. The NP-8 test's OWN comments corroborate
this: `np8_fleet_nats_outage_leader_continuity_test.go:230-238` explicitly says
workers "degrade with 'KV error threshold exceeded'" via the heartbeat path and
treats the calculator's list error as a *symptom*, not the driver.

**Why this matters for the fix:** it means mech-2 is **the same shared-exit defect
as Families A and B** (recover-on-wrong-signal: assignment-read exit while the
heartbeat op still faults), NOT a separate calculator-enumeration bug. The
calculator's `"list heartbeat keys: stream not found"` is a real *secondary*
symptom (the leader cannot compute a correct assignment), but it is downstream of
the same missing bucket and does not itself drive readiness. The fix surface is
the exit gate + a heartbeat-bucket recreate-or-stay-terminal decision — not a
calculator change. The report's *conclusion* (mech-2 is a real fleet flap, not a
clean heal) stands; only the mechanism sentence needs correcting.

---

## PART 2 — Uncertainty scoping (priority-ordered, each: needed / worth it / priority)

### (i) NP-8 mech-2 on a REAL RF3 5-node cluster — does replicated MemoryStorage survive a rolling restart?

**Located harness:** `test/integration/failure/quorum_loss_tier2_test.go`
(`TestRF3SelectivePeerFault_HandoffQuorumLoss`, gated `PARTI_RUN_QUORUM_LOSS_TIER2=1`),
plus the reusable `partitest.StartEmbeddedNATSClusterN(t, 5)` helper.

**Finding (VERIFIED by reading the test):** the existing tier2 test does NOT
settle this question and cannot be reused as-is. It is a **data-plane KV-surface
probe**: it creates a single `Replicas:3` *handoff* bucket (FileStorage,
`:32-37`), stops two non-meta peers (`:48-54`), and asserts that `kv.Get/Put/
Status` produce *some* error surface (`:90-91`). It does **not** (a) start a
3-manager Parti fleet, (b) create a **MemoryStorage** heartbeat bucket with
Replicas>1, or (c) perform a **rolling restart** (it shuts peers down and never
restarts them within the assertion window). Only `StartEmbeddedNATSClusterN` is
directly reusable.

**What's needed:** a new gated test on the 5-node helper that (1) pre-creates the
heartbeat bucket as `MemoryStorage, Replicas:3` (the open question is whether
replicated MemoryStorage survives one peer bouncing), (2) starts a real
3-manager fleet against it, (3) **rolling-restarts** one node at a time
(`srv.Shutdown(); restart same StoreDir/port`) keeping quorum, and (4) asserts the
fleet holds Stable AND the heartbeat bucket's stream `Created` is unchanged (no
silent wipe). The single-node embedded restart in
`np8_..._HeartbeatBucketLossFlap` is the RF1 worst case; this is the RF3 control.

**Worth it / priority: HIGH (the single biggest open uncertainty).** It
discriminates the two divergent prod outcomes (replicated MemoryStorage survives →
mech-2 is single-node-only and severity drops to Low; does not survive → mech-2 is
a genuine fleet gap needing the recreate fix). The fix scope for C-mech-2 depends
entirely on this answer, so it should run BEFORE committing to a mech-2 fix. Cost:
moderate (new harness; 5-node embedded clusters are CPU-heavy — keep gated).

### (ii) NP-9 arbitration race — what config/load flips "kv-unavailable wins"?

**Mechanism (VERIFIED):** under full quorum loss, two paths race to win the
`enterDegraded` CAS (`manager_degraded.go:309`):
- **Fast path:** heartbeat/election/stableid KV ops at high cadence →
  `recordKVError` → trips when the window hits `KVErrorThreshold` within
  `KVErrorWindow` → reason `kv-unavailable` (when marked) or `KV error threshold
  exceeded` (`manager_degraded.go:207-224`). At the 500ms heartbeat cadence this is
  fast.
- **Slow path:** the assignment watcher's bounded-retry envelope exhausts
  (`watcherMaxAttempts` over `watcherBaseBackoff`..`watcherMaxBackoff`) →
  `OnPermanent` → `enterDegraded("assignment-watcher-exhausted")`
  (`manager_assignment.go:407-412`).

**Knobs that flip the winner (the answer the report flagged but did not derive):**
- Raising `DegradedBehavior.KVErrorThreshold` or shrinking `KVErrorWindow` slows
  the fast path → the watcher-exhaustion reason can win instead.
- Lowering the heartbeat cadence (`HeartbeatInterval`) slows fast-path
  accumulation → same flip.
- Shortening `watcherBaseBackoff`/`watcherMaxBackoff` or lowering
  `watcherMaxAttempts` speeds the slow path → it can beat the threshold.
- Load that delays the heartbeat goroutine (CPU starvation) delays the fast path
  non-deterministically.
The accelerated test config (sub-second thresholds, 500ms heartbeat) makes the
fast path win deterministically; production defaults (75s WorkerIDTTL,
`KVErrorThreshold` default) widen the window where the watcher could win.

**What's needed:** a parameterized variant of
`TestNP9_FullQuorumLoss_KVUnavailableWins...` that sweeps `KVErrorThreshold` and
`watcherMaxAttempts` and asserts which reason wins per config — to document the
boundary, not to "fix" it (either reason is correct policy; only the operator
runbook text depends on which appears).

**Worth it / priority: MEDIUM-LOW.** Both outcomes are valid Degraded entries with
correct operator actions (the taxonomy table covers both). The value is
documentation accuracy (so OPERATIONS.md does not over-promise a single reason),
not a correctness fix. Low risk if deferred.

### (iii) `-race -count=5` over the hook-goroutine-touched harness

**Why (VERIFIED):** `enterDegraded` spawns `monitorDegradedAlerts`
(`manager_degraded.go:336`) and fires `OnDegraded` from a hook goroutine
(`:326-330`); `attemptRecoveryFromDegraded` and `recordKVError` mutate
`kvErrorWindow` under `m.mu` while the 1s connection monitor and 500ms heartbeat
success path (`recordKVHealthyOp`) also touch it. The flap proofs drive rapid
Degraded↔Stable cycles through exactly these paths — a natural place for a data
race on `kvErrorWindow`, `degradedSince`, or the per-worker hook counters in the
tests. The report (§6) explicitly notes the proofs were run pass/fail, **not**
`-race`.

**What's needed:** run the gated proofs under race:
`PARTI_RUN_NP2_EPOCH_FLAP_PROOF=1` (`np2_epoch_fence_return_to_stable_test.go`),
`PARTI_RUN_NP3_KVUNAVAIL_FLAP_PROOF=1` (`np3_kv_unavailable_recovery_test.go`),
`PARTI_RUN_NP1_LIVE_RECREATE_PROOF=1` (`np1_live_recreate_returns_stable_test.go`),
`PARTI_RUN_NP8_FLEET_OUTAGE_PROOF=1` / `PARTI_RUN_NP8_HEARTBEAT_FLAP_PROOF=1`
(`np8_fleet_nats_outage_leader_continuity_test.go`), each with `-race -count=5
-run <Name>`. Per the integration-test-discipline memory, monitor-goroutine
changes need concurrency stress; the per-worker `atomic.Bool`/counter hooks in the
NP-8 test (`recovered[]`, `badReason[]`, the flap counters) are the most likely
race site if any are non-atomic.

**Worth it / priority: MEDIUM, and CHEAP.** It is a low-cost confidence check (no
new code), and it should be a **gate before any exit-gate fix lands** because the
fix will add code on these exact hot paths. Do it once now to establish a clean
race baseline, then re-run after the fix.

---

## Summary of challenges to `04-proof-findings.md`

1. **§4 completeness claim is breakable (NP-10):** a leader-side
   heartbeat-`Keys`-scan `DeadlineExceeded` is silently swallowed
   (`worker_monitor.go:282-284`, `calculator.go:1213`, `IsConnectivityError`
   misses bare `DeadlineExceeded` at `errors.go:120-136`) → false-healthy leader
   serving stale assignments. Not covered by the §4 dismissals. High / Medium-conf.
2. **§2 Family C mech-2 attribution is imprecise:** the degraded *entry* is the
   heartbeat-Put threshold path, not the calculator enumeration (corroborated by
   the test's own comments at lines 230-238). This unifies mech-2 with the A/B
   shared-exit defect and re-scopes the fix.
3. **§7 "one exit-gate fix closes A and B" under-states the asymmetry:** the three
   families want different terminal outcomes (B auto-recover, A terminal + latch
   fix, C-mech-2 terminal pending recreate).
4. **§Recommendation "settle mech-2 on the tier2 harness" overstates reuse:** the
   tier2 test is a data-plane KV probe with no fleet, no MemoryStorage bucket, and
   no rolling restart; only `StartEmbeddedNATSClusterN` is reusable.
5. **Cross-family fix risks (X1/X2):** recreate-on-reconnect re-opens Family A via
   the never-re-captured `ep.created`; a broad re-provision can mask a C2 peer
   takeover by reclassifying `ErrClaimLost`.

Where the report is RIGHT (do not relitigate): Families A and B mechanisms,
NP-9/NP-4/NP-5/NP-6/NP-7 verdicts, the "shared exit half" insight, and that
mech-2 is a real fleet flap (only its mechanism sentence is off).
