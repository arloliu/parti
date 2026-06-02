# Family C mechanism 2 (NP-8): MemoryStorage heartbeat-bucket loss => fleet flap

Independent deep-dive on current HEAD (`2453306`, worktree `auto-heal-gap-investigation`).
Read-only investigation. Every code claim cites `file:line` against the worktree source.

Verdict: **confirmed-with-corrections.** The gap is real and the flap mechanism is
exactly as severe as the report claims, but the report **misnames the load-bearing
re-degrade driver** and, as a consequence, **its Opt B fix recommendation does not stop
the flap.** Details below.

---

## 0. TL;DR of the corrections

1. The re-degrade driver is the **heartbeat _publisher_ `kv.Put`**
   (`manager_election.go:424` wires `publisher.SetOnError(m.recordKVOpError)`), NOT the
   leader's calculator "list heartbeat keys" call. The calculator's list errors are
   **swallowed** (logged, never fed to the degraded circuit) — see §1. The report's
   `"failed to list heartbeat keys: stream not found"` names the loudest **log line**,
   not the **degrade driver**. This is also why the gap is **fleet-wide, not
   leader-specific**: every worker publishes a heartbeat, so every worker re-degrades.
2. Because the calculator list is not the driver, the report's **Opt B
   ("calculator path that tolerates an empty heartbeat bucket") fixes a non-driver and
   does not stop the flap.** Worse, "empty" ≠ "missing": the calculator **already**
   tolerates empty (`worker_monitor.go:170`), and the actual failure is a missing
   *stream* that returns a raw error at `calculator.go:1213`. Opt B as framed is doubly
   insufficient (§3, Opt B).
3. The likely re-degrade **reason string is `"kv-unavailable"`**, not `"KV error
   threshold exceeded"`. A cached `kv.Put` after stream loss surfaces
   `nats.ErrNoResponders` (the repo's own empirically-verified surface,
   `source/nats_kv.go:1381-1383`), which `markKVUnavailable` wraps into `ErrKVUnavailable`
   (`manager_degraded.go:67-68`). The flap is identical for all three plausible error
   classes, so this does not change the fix — but the report's mechanism narrative
   implies the wrong reason. Hedged in §1.4.
4. Opt A (recreate-on-reconnect) **does collide with Family A's epoch fence** — the report
   is right — and I sharpen it: today the fence is **silent/dormant** on heartbeat
   (probe errors → Debug + `continue`, `manager_setup.go:679-682`); Opt A would **activate
   a dormant fence simultaneously on every worker** (same new stream `Created`). The two
   fixes must be co-designed (§3, Opt A; §4).

---

## 1. Exact failure path (re-derived from code)

### 1.1 What survives a single-node NATS restart

`ensureCoreKVBuckets` (`manager_setup.go:125-166`) fixes storage per bucket:

- election: `FileStorage` (`:152`)
- heartbeat: **`MemoryStorage`** (`:156`)
- assignment: `FileStorage` (`:160`)
- stableID: `FileStorage` (`manager_setup.go:92`)
- handoff: `FileStorage` (`manager_setup.go:176`)

A single-node embedded restart with a persistent StoreDir reloads the FileStorage
streams but the MemoryStorage `KV_<heartbeatBucket>` stream is **gone**. So post-restart:
heartbeat bucket missing, all others present.

### 1.2 The two ticks that fight (the flap)

**Re-degrade tick — the heartbeat publisher.** Every worker runs a heartbeat
`Publisher` (`internal/heartbeat/publisher.go`). The publish loop calls `p.kv.Put` every
`HeartbeatInterval` (`publisher.go:414`) on a **cached** KV handle captured at Start
(`manager_election.go:419`, `publisher.go:135`). On failure it invokes the registered
`onError` callback (`publisher.go:354-359`), which the manager wires to
`m.recordKVOpError` (`manager_election.go:424`). `recordKVOpError` →
`markKVUnavailable` → `recordKVError` (`manager_degraded.go:235-236`).
**Every** manager runs a heartbeat publisher, leadership-independent: `startHeartbeat`
is invoked once during `Start` (`manager.go:602`) for every worker, before/independent of
election — so this driver fires on followers and leader alike (the basis for the
"fleet-wide, not leader-specific" correction in §5.1).
`stream not found`/`no responders` both pass the `recordKVError` admission gate
(`manager_degraded.go:165` admits degrading-JetStream OR `ErrKVUnavailable`), so after
`KVErrorThreshold` (default 5, `config.go:204`) failures inside `KVErrorWindow`
(default 30s, `config.go:209`) → `enterDegraded(...)` (`manager_degraded.go:207-224`).
At the integration `HeartbeatInterval=500ms` (`config.go:793`) that is **~2.5 s** to
re-trip. **This fires on every worker, leader or not.**

**Recovery tick — the connection monitor.** `monitorNATSConnection` runs a 1 s ticker
(`manager_degraded.go:79`) calling `checkConnectionHealth`. Post-reconnect the connection
is CONNECTED, so the `!isConnected` re-degrade branch (`:103-118`) is never taken; once
connection **uptime** ≥ `ExitThreshold` (`:130`) it calls `attemptRecoveryFromDegraded`
(`:131`). That function:

1. `refreshAssignmentFromNATS()` (`manager_degraded.go:383`) — reads **only**
   `assignment.<workerID>` from the **assignment** bucket (`manager_assignment.go:1567-1568`),
   which is FileStorage and **survived** → succeeds.
2. `recordKVSuccess()` (`:393`) — clears the entire KV-error window
   (`manager_degraded.go:244-250`).
3. `currentAssignmentApplied(cur)` (`:409`) — assignment survived and was already applied
   pre-outage → true → skips the re-arm.
4. `exitDegraded()` (`:415`) → `transitionState(StateStable)` + `degradedSince.Store(0)`
   (`manager_degraded.go:350-356`) → **Stable**.

**The exit gate NEVER reads the heartbeat bucket.** This is the same
"recover-on-wrong-signal" exit defect the report attributes to Families A and B
(`04-proof-findings.md:60-93, 95-120`): the recovery path proves the *assignment* read
is healthy and exits, while the *failing* op (heartbeat publish) is never checked. Once
`degradedSince` is cleared, `recordKVError`'s "already degraded" short-circuit
(`manager_degraded.go:174`) releases, so the heartbeat-Put errors re-accumulate from
zero → re-degrade ~2.5 s later. Oscillation. Connection stays CONNECTED throughout
(asserted by the proof, `np8..._test.go:329`).

### 1.3 Why the calculator "list heartbeat keys" error is NOT the driver

`GetActiveWorkers` returns `failed to list heartbeat keys: %w` on a non-no-keys error
(`worker_monitor.go:175`). Tracing every consumer:

- **Poll path:** `WorkerMonitor.monitorWorkers` ticker → `onChangeCb` → `pollForChanges`
  → `observeAndDecide` → `collectWorkerObservation` → `getActiveWorkers`
  (`calculator.go:1099,1195`). The error returns up to the poll loop, which only
  **logs** it: `m.logger.Error("polling error", ...)` (`worker_monitor.go:282-284`).
  The watcher path is identical: `m.logger.Error("watcher-triggered check failed", ...)`
  (`worker_monitor.go:396`).
- **Rebalance path:** `collectRebalanceWorkers` → `getActiveWorkersFiltered` →
  `getActiveWorkers` (`calculator.go:1489,1319,1195`); the error bubbles to `rebalance`
  which returns it (`calculator.go:1541-1545`) — to the state machine, again logged only.
- **Audit path:** `auditApplied` → `GetHeartbeats`; on error it logs Debug and returns
  (`calculator_audit.go:60-63`).

`grep -n 'recordKVError\|recordKVOpError\|SetOnError' internal/assignment/*.go`
(excluding tests) returns **nothing**: the calculator has **no wiring** into the
manager's degraded circuit for worker-list reads.

**Complete `enterDegraded` caller enumeration** (`grep -n 'enterDegraded(' *.go`,
production), to make the "by elimination" airtight — which of these can fire
post-reconnect on a heartbeat-only loss?

1. `manager_degraded.go:114` `"NATS connection down"` — connection monitor;
   **excluded** (link is CONNECTED post-reconnect).
2. `manager_degraded.go:224` threshold reason — `recordKVError`; **fed by the heartbeat
   Put** (and only heartbeat, since every other bucket survived). **This is the driver.**
3. `manager_assignment.go:412` `assignmentWatcherDegradedReason` — keyed off the
   **assignment** bucket watcher (FileStorage, survived). Not heartbeat.
4. `manager_startup_async.go:47,187` `"startup-background-panic"` / `"startup-timeout"` —
   one-shot **startup** watchdog during `Start`; not a steady-state post-recovery path.
5. `manager_setup.go:83` `"stream-missing-recovery-exhausted"` — via `onStreamMissingError`,
   the dynamic-consumer **event-stream** observer (`composite_updater.go:135`). Not the
   heartbeat bucket.
6. `manager_setup.go:689` `"bucket-recreated:<bucket>"` — epoch fence; **dormant on
   heartbeat today** (probe errors → Debug + `continue`, §3).

Of the six, only (2) can fire post-reconnect on a heartbeat-only loss, and (2) is driven
solely by the heartbeat Put (the only heartbeat-bucket op wired to the circuit; the
calculator reads are unwired). Hence: **publisher Put = driver; calculator list = loud
bystander.**

Note the classification asymmetry in `getActiveWorkers` (`calculator.go:1196-1213`):
`stream not found` is **not** an `IsConnectivityError` (`internal/natsutil/errors.go:114-137`
does not match it), so the cache-fallback branch (`:1197-1211`) is skipped and the raw
error returns at `:1213`. The calculator therefore does NOT even fall back to cached
workers on a missing stream — it just aborts the rebalance. (It would fall back only for
a *connectivity* error.)

### 1.4 The re-degrade reason string (hedged)

The heartbeat publisher holds a **cached** KV handle. The repo's empirically-verified
surface (`source/nats_kv.go:1371-1400`, "verified against nats.go v1.50.0") says a cached
**data** op (`kv.Get`, and by extension `kv.Put`) after stream loss returns
`nats.ErrNoResponders`, while a cached **watcher** rebind returns
`jetstream.ErrStreamNotFound`. So the publisher Put most likely yields
`nats.ErrNoResponders`:

- `markKVUnavailable` (`manager_degraded.go:58-72`): not connectivity, not
  degrading-JetStream, but `errors.Is(err, nats.ErrNoResponders)` → wraps with
  `ErrKVUnavailable` → `recordKVError` sets reason **`"kv-unavailable"`**
  (`manager_degraded.go:216-218`).

If instead the Put surfaces `ErrStreamNotFound` (degrading-JetStream), the reason is
`"KV error threshold exceeded"`. The proof's own diagnostic block calls this a CAS race
between the two reasons (`np8..._test.go:230-238`). **Either way the flap is identical**,
because both classes accumulate to threshold and neither is cleared by
`recordKVHealthyOp` (there is no heartbeat *success* — the bucket is gone, so transient
entries that `recordKVHealthyOp` would clear never get a success to clear them;
`manager_degraded.go:266-288`). I could not capture the live reason empirically: the
integration config uses a NopLogger (`internal/testutil/nats.go:306`) and the flap proof
uses empty hooks (`np8..._test.go:293`), so no per-worker reason is recorded, and I must
not modify production code to add a logger. **Recommendation: do not assert a specific
reason; treat both as in-scope for the fix.**

### 1.5 Recovery-grace interaction (minor)

`exitDegraded` calls `enterRecoveryGracePeriod` only `if m.isLeader.Load()`
(`manager_degraded.go:370-372`). Recovery grace (`RecoveryGracePeriod` default 15s,
`config.go:215`) only gates the leader's *rebalance decisions* (`calculator.go:1063-1066`);
it does **not** gate the heartbeat publisher or `recordKVError`, so it does **not** damp
the flap. Followers get no grace at all. This is why the flap period (~2.5s) is far below
RecoveryGracePeriod.

---

## 2. Severity / topology (part d)

- **Worst case is real but topology-gated.** The single-node embedded restart loses the
  whole MemoryStorage stream — proven (`np8_mech2.out`: HOLD check trips at ~12s,
  connection CONNECTED). A genuine **RF3 rolling restart** that keeps a replicated
  MemoryStorage stream alive across the node bounce would NOT lose the bucket and would
  not hit this. The report's RF3 caveat (`04-proof-findings.md:161-164, 252`) is sound
  and remains the single biggest open uncertainty (un-measured; reasoning only).
  Caveat on the caveat: MemoryStorage replicas still lose all data if **all** replicas
  bounce simultaneously or if quorum is lost during the restart, so RF3 narrows but does
  not eliminate the worst case.
- **heartbeat → FileStorage eliminates mech 2 at the source** (the stream survives the
  restart, so neither the publisher Put nor the calculator list fails). **Cost is
  unquantified, NOT IOPS-free.** The `manager_setup.go:113-115` comment cites IOPS
  finding M1.9 for the **election** bucket only; heartbeat is the *highest-frequency* KV
  op (every worker, every `HeartbeatInterval`), and MEMORY.md's IOPS notes identify the
  per-op state file as the dominant cost (M2.A). Do **not** transfer M1.9's "IOPS-free"
  conclusion to heartbeat. A FileStorage heartbeat bucket needs its own IOPS measurement
  before adoption. (It would also change the C1/partial-loss test semantics — see §3.)

---

## 3. Fix options

| Opt | Mechanism | Surface | Blast radius | Contracts/tests | Residual risk |
|---|---|---|---|---|---|
| A | recreate heartbeat bucket on reconnect | `manager_setup.go` + a reconnect/recovery hook | medium-high | **collides with Family A epoch fence (C-cross)**; touches C1 reasoning | epoch re-capture race; reconnect-storm churn |
| B (as written) | calculator tolerates empty heartbeat list | `worker_monitor.go` / `calculator.go` | low | none | **does not stop the flap** (wrong driver) |
| B' (corrected) | publisher Put self-heals: recreate-or-reopen handle on NotFound | `internal/heartbeat/publisher.go` + manager wiring | medium | C1, C3 | recreate race; must not mask C1 |
| C | gate `attemptRecoveryFromDegraded` exit on heartbeat-op health | `manager_degraded.go:376-416` | medium | C1, C2, C3, C4; shared with Family A/B exit defect | turns flap into *stuck Degraded* if bucket never returns |
| D | heartbeat bucket → FileStorage | `manager_setup.go:156` | low-code, high-ops | C1 partial-loss test, IOPS | unquantified IOPS cost |

### Opt A — recreate-on-reconnect (report's primary rec)

**Mechanism.** On NATS reconnect (or inside `attemptRecoveryFromDegraded`), re-run
`ensureKVBucket` for the heartbeat bucket so a missing `KV_<heartbeat>` stream is
recreated; the publisher's existing handle then resolves again (or is re-opened).

**Where it would hook in.** There is **no existing reconnect handler** that re-ensures
buckets — `ensureCoreKVBuckets` runs only at Start (`manager.go:568`). A natural hook is
the connection-restored branch of `checkConnectionHealth` (`manager_degraded.go:122-126`)
or a `nats.ReconnectHandler`. (The README recommends `MaxReconnects(-1)`, `doc.go:30-33`,
so reconnect events are the expected recovery trigger.)

**CRITICAL interaction with Family A (confirmed + sharpened).** `captureBucketEpoch` runs
for **every** bucket including heartbeat (`ensureKVBucket:265` → `captureBucketEpoch:600`),
caching `ep.created` **once** at Start (`:627`) and **never** re-capturing it. The epoch
monitor probes each bucket on an `OperationTimeout` ticker (`checkBucketEpochs:669-693`):
- **Today (no recreate):** after restart the heartbeat probe `BucketStreamCreated` errors
  (stream gone) → the tick logs Debug and `continue`s (`:679-682`) — **the fence is
  dormant/silent on heartbeat.** It does NOT fire `bucket-recreated`.
- **With Opt A:** recreation gives the new stream a **different `Created`** →
  `!live.Equal(ep.created)` → `enterDegraded("bucket-recreated:heartbeat")`
  (`:684-690`). Opt A would thus **activate a previously-dormant fence**, and because all
  workers recreate against the same new stream, it fires **fleet-wide simultaneously** —
  trading the heartbeat flap for an epoch-fence Degraded (which Family A shows is itself a
  flap, `04-proof-findings.md:58-93`). **Opt A is unsafe unless co-designed with Family A's
  `ep.created` re-capture.** A correct Opt A must re-capture the heartbeat epoch atomically
  with the recreate, and must win the race against the epoch ticker reading the new
  `Created` before the re-capture lands.

**C1 (whole-bucket-missing → all Degraded).** `TestManager_LiveNATSBucketLoss`
(`manager_live_bucket_loss_test.go:83-89`) deletes **all** buckets **live** (no
reconnect — the connection stays up). Opt A keys on a reconnect/connection-restored
event, which does not occur in C1, so Opt A would not fire there and C1 still holds via
the surviving non-transient errors from the other wiped buckets. **C1 is not broken by
Opt A** — but the verdict depends on Opt A firing on a *reconnect*, not on any
bucket-missing observation. If a future Opt A variant recreated buckets on *any* missing
observation, it WOULD risk masking C1. Pin the trigger to reconnect.

**Pros/cons.** Pro: restores the documented MemoryStorage model. Con: epoch-fence
collision (above), reconnect-storm churn (recreate on every flap of the link), and a
recreate race across the fleet (N workers racing to recreate the same bucket — mitigated
by `EnsureKVBucketWithRetry` get-first, `manager_setup.go:255`).

### Opt B — "calculator tolerates empty heartbeat bucket" (report's alt rec)

**This does not stop the flap.** The calculator list error is not the degrade driver
(§1.3); making the calculator tolerate the failure changes a logged error into a
logged-and-tolerated one but leaves the publisher-Put → `recordKVError` loop intact.
Additionally:
- **empty ≠ missing.** `GetActiveWorkers` already maps `IsNoKeysFoundError` →
  `[]string{}` (`worker_monitor.go:170-173`); the calculator already tolerates an
  *empty* bucket. The failure here is a *missing stream*, which returns the raw error at
  `worker_monitor.go:175` → `calculator.go:1213`. So "tolerate empty" addresses a case
  that is already handled.
- Even a "tolerate missing too" variant would only suppress an aborted rebalance, not the
  flap. **Reject Opt B as written.**

### Opt B' — publisher Put self-heals (corrected B; targets the actual driver)

**Mechanism.** In the heartbeat publisher (or its `recordKVOpError` wiring), on a
NotFound/NoResponders Put error, re-open the KV handle via `js.KeyValue` and/or recreate
the bucket before feeding the circuit; only feed `recordKVOpError` after a bounded retry
fails. Surface: `internal/heartbeat/publisher.go:405-420` + the manager's
`SetOnError` wiring (`manager_election.go:424`), or a manager-side recreate before
`recordKVOpError`.

**Blast radius / contracts.** Must NOT mask **C1**: if Put-recreate runs in the
all-buckets-wipe case it would let the heartbeat publisher succeed and fire
`recordKVHealthyOp` — but C1 still degrades via the other wiped buckets' non-transient
errors (`recordKVHealthyOp` only clears *transient* entries, `manager_degraded.go:278-284`;
the wiped FileStorage buckets produce `ErrBucketNotFound` = degrading-JetStream =
non-transient, exactly the masking guard `TestManager_PartialBucketLoss_HeartbeatHealthy`
pins, `manager_live_bucket_loss_test.go:238-338`). C3 (OnDegraded once per entry) is
unaffected. This variant shares Opt A's epoch-fence collision **only if** it recreates
the bucket (vs merely re-opening the handle after NATS itself recreated it). Re-opening
the handle without recreating avoids the epoch trip but only works if something else
recreates the bucket — which nothing does today, so a pure re-open is insufficient.

**Residual risk.** Same recreate race as Opt A; same epoch interaction if it recreates.
This is essentially Opt A relocated to the publisher; the epoch co-design is unavoidable
for any path that recreates the stream.

### Opt C — gate the recovery exit on heartbeat-op health (shared with Family A/B)

**Mechanism.** Make `attemptRecoveryFromDegraded` (`manager_degraded.go:376-416`) refuse
to `exitDegraded` while a heartbeat-op failure is outstanding (e.g. require a recent
heartbeat *success*, or probe the heartbeat bucket as part of the exit gate). This is the
**same exit-gate fix** the report proposes for Families A/B
(`04-proof-findings.md:50-52, 277-282`): a single change to the exit predicate that checks
the *failing* op recovered, not just the assignment read.

**Blast radius / contracts.** Touches the shared recovery gate, so it must preserve:
- **C1**: still must reach Degraded on whole-bucket loss (unchanged — this only affects
  *exit*, not entry).
- **C4** (`Start` returns after sanity-check, not StateStable): unaffected (exit gate runs
  post-Start).
- **C2** (peer-claim takeover routing): unaffected (separate path, `manager_election.go:106-127`).
- **C3**: unaffected.
- The NP-5 recovery proof and NP-3a disarm control (`04-proof-findings.md:168-186`) must
  still pass: the gate must still EXIT once the heartbeat bucket genuinely returns.

**Pros/cons.** Pro: one fix closes the *exit* half of Families A, B, and C-mech-2. Con:
**without a bucket-recreate, Opt C converts the flap into a permanently-stuck Degraded**
(the MemoryStorage bucket never comes back on its own). That is arguably the *correct*
posture ("data lost → stay Degraded, require rotation/re-provision"), matching the
documented live-data-loss contract (`04-proof-findings.md:88-90`), but it is a behavior
change from "flap" to "terminal Degraded" and must be paired with operator docs. Opt C is
the cleanest *correctness* fix; pairing C (stop the false exit) with A/B' (restore the
bucket) gives both correctness and auto-heal.

**Test-contract consequence (important for ungating).** `TestNP8...HeartbeatBucketLossFlap`
asserts the fleet **reaches** all-Stable (`np8..._test.go:325`) and then **holds** it
(`:327`). **Opt C-alone leaves the fleet stuck-Degraded → it fails the *reach*
assertion**, so Opt C-alone cannot be ungated as a drop-in regression guard: the proof's
expectation must be rewritten from "auto-heal to Stable" to "hold Degraded / require
rotation." That is a **product decision about desired behavior** (auto-heal vs.
fail-safe-hold), not a mechanical ungate. Opt C **+** a bucket-recreate (A/B') is what
makes the existing proof pass as written.

### Opt D — heartbeat bucket → FileStorage

**Mechanism.** Change `manager_setup.go:156` from `MemoryStorage` to `FileStorage`.

**Pros/cons.** Pro: eliminates mech 2 at the source (stream survives restart; no flap, no
epoch trip, no recreate race). Con: **unquantified IOPS cost** (§2 — heartbeat is the
highest-frequency op; M1.9's "IOPS-free" applies to election, not heartbeat). Also changes
the semantics of `TestManager_PartialBucketLoss_HeartbeatHealthy` and the
`manager_setup.go:116-117` doc comment ("workers re-publish every interval, so a missed
window is recovered by the next publish" — the rationale for MemoryStorage). Operators who
pre-create the bucket with their own config already override this (`manager_setup.go:121-124`).

### Recommended fix

**Opt C (gate the exit) + Opt A/B' (restore the bucket), co-designed with Family A's
`ep.created` re-capture.** Rationale: Opt C alone is the minimal *correctness* fix and is
shared with Families A and B, but it leaves the fleet stuck-Degraded after a real
MemoryStorage loss; pairing it with a bucket-recreate restores auto-heal. The recreate
**must** re-capture the heartbeat epoch atomically so Family A's fence does not fire — so
this fix **cannot land independently of Family A's fix**. If only one change is permitted,
land **Opt C** (correctness: no false Stable while the heartbeat op is dead) and document
that MemoryStorage heartbeat loss requires re-provision/rotation, deferring auto-heal.
Opt D is the simplest if an IOPS measurement clears it.

---

## 4. Cross-family interactions

- **Family A epoch fence (hard coupling).** Any fix that recreates the heartbeat bucket
  (Opt A, Opt B' recreate variant) trips `checkBucketEpochs` →
  `enterDegraded("bucket-recreated:heartbeat")` (`manager_setup.go:684-690`) unless
  `ep.created` is re-captured. Family A's own fix re-captures `ep.created`
  (`04-proof-findings.md:275-282`); the C-mech-2 recreate must reuse that re-capture and
  win the race against the epoch ticker. **Co-design mandatory.**
- **Family A/B shared exit defect.** Opt C *is* the report's shared exit-gate fix
  (`04-proof-findings.md:50-52, 277-282`); one implementation can close the *exit* half of
  all three families. The entry/latch halves differ per family.
- **F-D1 class-aware reset (commit 421f13c).** `recordKVHealthyOp` clears only transient
  entries (`manager_degraded.go:266-288`). In mech 2 there is no heartbeat *success* (the
  bucket is gone), so the reset never fires and the transient `ErrKVUnavailable` entries
  accumulate uncleared — the reset is irrelevant to this flap (confirming the report's
  analogous note for Family B, `04-proof-findings.md:107-109`).
- **Empty-bucket safety (part c) — two layers, both confirmed.** IF a recreate leaves the
  heartbeat bucket *empty* (recreated, but workers have not yet re-published), a leader
  scanning 0 workers must not revoke everyone. Two independent guards prevent that, and
  which one is active depends on whether leadership survived the outage:
  - **(i) `len(workers)==0` no-op — ALWAYS on.** `rebalance` short-circuits on an empty
    worker set: "no active workers for assignment" → returns without publishing
    (`calculator.go:1571-1577`). This holds even for a **fresh** leader (one elected
    across the outage) whose calculator has no baseline yet. This is the load-bearing
    guard for the realistic post-takeover path.
  - **(ii) F10-A cached hold-current — only once a baseline exists.** If the *same* leader
    continued and already has `lastKnownWorkerCount=3`, scanning 0 hits
    `workerObservationSuspicious(0)` = `0 < 3*Pct` = true (`calculator.go:1294-1299`), so
    `getActiveWorkers` returns `cached, fresh=false` (`:1228-1243`) and `observeAndDecide`
    skips the decision on `!fresh` (`:1038-1043`). But `workerObservationSuspicious` is
    **silent when `lastKnownWorkerCount==0`** (`calculator.go:1295`), so a fresh
    post-takeover leader (lastKnown=0) gets NO F10-A protection — layer (i) is what saves
    that case.
  **So an empty heartbeat bucket does NOT cause spurious revocation**, via (i) always and
  (ii) additionally for a continuous leader. The report's part (c) "safe unknown,
  hold-current" path exists — but the citation must be the `len==0` no-op for the
  realistic (new-leader) case, not F10-A.
  The *missing-stream* (not empty) case is absorbed by neither: the error returns raw at
  `:1213` before the F10-A counter at `:1227` runs, and aborts the rebalance before the
  `len==0` check at `:1571` — but the missing case is also not the degrade driver, so this
  is moot for the flap.

---

## 5. Discrepancies with the report (`04-proof-findings.md`)

1. **Driver misnamed (substantive).** Report (`:136-144, 256-258`): "every worker that
   becomes leader fails `failed to list heartbeat keys: nats: stream not found` in its
   calculator." Correct: the calculator list error is **swallowed** (§1.3); the re-degrade
   driver is the **heartbeat publisher Put** via `recordKVOpError`
   (`manager_election.go:424`), which fires on **every worker, not just the leader**. The
   report's framing wrongly localizes the gap to the leader.
2. **Opt B is ineffective (substantive).** Report's deferred rec (`:289-293`): "a
   calculator path that tolerates an empty heartbeat bucket during recovery." This fixes a
   non-driver and conflates empty (already tolerated) with missing (§3, Opt B). Reject as
   written.
3. **Reason string (minor).** Report implies `"stream not found"` → "KV error threshold";
   the cached-Put surface is more likely `nats.ErrNoResponders` → `"kv-unavailable"`
   (§1.4). Does not change the fix.
4. **Epoch interaction under-stated (sharpening, not contradiction).** Report does not note
   that the heartbeat epoch fence is currently *dormant* (errors → Debug+continue) and that
   Opt A would *activate* it fleet-wide. The collision the report relies on for Family A is
   real and is the load-bearing constraint on Opt A.
5. **IOPS transfer risk (flag).** The report's "(d) FileStorage" framing leans on the IOPS
   work; M1.9's "IOPS-free" finding is about the **election** bucket
   (`manager_setup.go:113-115`), not heartbeat. Do not transfer it.

The report's core verdict — fleet does not auto-heal a single-node MemoryStorage
heartbeat-bucket loss; topology-dependent; needs recreate-or-tolerate — is **correct**.
The mechanism attribution and Opt B are wrong.

---

## 6. Open questions

- Empirically confirm the re-degrade **reason** ("kv-unavailable" vs "KV error threshold
  exceeded") by running the mech-2 proof with a real logger or a reason-capturing
  OnDegraded hook (requires a test-only change, not production code).
- RF3 reality (the report's biggest open item): does a real RF3 rolling restart preserve
  the replicated MemoryStorage stream? Needs the gated `quorum_loss_tier2` 5-node harness
  (`04-proof-findings.md:268-269`).
- Does the heartbeat publisher's cached handle ever self-recover after NATS recreates the
  bucket by some other means (e.g. another worker's Start re-ensuring it)? On main nothing
  re-ensures post-Start, so no — but a fix that has *any* worker recreate the bucket would
  let all cached handles resolve again (pending the epoch-fence trip).
