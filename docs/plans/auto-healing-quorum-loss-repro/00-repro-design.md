# Auto-Healing Quorum-Loss Reproduction — Design

- **Date:** 2026-05-29
- **Author:** Claude (perspective A — to be consolidated with an independent Codex investigation)
- **Status:** DRAFT — pending Codex cross-investigation + user review
- **Source incident:** `tmp/parti_auto_healing_issue.md`
- **Scope of this effort:** *Reproduce + baseline + version-compare only.* Designing/implementing the fix is a separate, later step.

---

## 0. Discipline note (don't guess)

The root-cause chain in §2 is a **hypothesis derived from reading the code**, not a
proven fact. The entire point of the reproduction (§3) is to **prove or refute** it.
Per the issue author's instruction, claims about *the handoff bucket specifically*
causing the stall are treated as unverified until a test demonstrates them. Any
statement below marked **(H)** is a hypothesis awaiting empirical confirmation.

---

## 1. Verified facts

### Versions / lineage (verified, not guessed)
- **parti v2.5.0** tagged `2026-05-24 20:01 +0800` (`650c7dc`). Already contains the
  "self-healing" work (whole-bucket-missing → `StateDegraded`), per the self-healing
  status memory (complete 2026-05-24).
- **parti HEAD** = v2.5.0 + 39 commits (`2026-05-29`). Almost all `sim:` / `test:` /
  `docs:`, plus a few `feat(manager/handoff/source)` (apply jitter, watcher/commit
  debounce, handoff phase concurrency, recreated-bucket recovery), a `feat(testutil)`
  3-node cluster helper, and a nats.go bump **v1.50.0 → v1.52.0**. None of these
  obviously target the deadline-exceeded classification gap in §2. **(H)** HEAD likely
  still exhibits the bug — the repro decides.
- **FDC app:** `feat/parti-v2.5.0` branch carried the v2.5.0 bump (`85316a62`,
  `2026-05-24 22:35`); the incident environment (2026-05-28) ran that v2.5.0 build.
  FDC `main` has since been updated to v2.5.0 as well (confirmed `go.mod` =
  `v2.5.0`). The report's "v2.5.0" label is therefore correct.
- **nats.go in prod:** v1.50.0 (per report). Will be pinned to match for the released
  version runs.

### FDC parti configuration (verified from source)
- Constructed in `shared/nats/consumer/dynamic/consumer.go`:
  `parti.DefaultConfig()` + overrides: `HeartbeatInterval=3s`, `HeartbeatTTL=10s`,
  `WorkerIDTTL=45s`, `ElectionTimeout=7s`, `EnableTwoPhaseHandoff=true`,
  KV buckets prefixed `tfdc` (stableid/election/heartbeat/assignment/handoff),
  `AssignmentTTL=0`, `HandoffTTL=0`.
- Defaults left in place: `OperationTimeout=10s`, `StartupTimeout=60s`,
  `KVErrorThreshold=5`, degraded `EnterThreshold=10s` / `ExitThreshold=5s` /
  `KVErrorWindow=30s`.
- Consumer: `particonsumer.NewDynamic(...)` with `WithPullGating(true)`,
  `WithProcessingGate({Enabled:true})`, `WithResolver({HandoffBucketName})`,
  `WithConsumerMemoryStorage(true)`, `WithConsumerReplicas(3)`. 2 worker consumers
  (`defense_flow_fanout`, `controller_event`); partition source = NATS KV
  (`partitions` bucket), ~100 partitions ("tools" / `USERxx`).
- Hooks (`OnAssignmentChanged/OnStateChanged/OnError/OnDegraded`) are **log-only**;
  the app does **not** self-stop or self-heal on degraded/error.

### parti bucket storage (verified, fixed per bucket in `manager_setup.go`)
- election = File, heartbeat = Memory, assignment = File, handoff = File, stableid = File.
- Replica count: `Config.KVBuckets.Replicas` (default 0 → server normalizes to 1).
  Applied only when `> 0`; pre-created buckets keep their own RF (get-first ensure).

### Incident shape (from report)
- NATS cluster v2.10.29, **RF=5, file storage**. KV buckets **RF=3**. Handoff bucket
  raft group on `nats-v1-0, nats-v1-2, nats-v1-3`.
- **F1 (volume offline):** `nats-v1-2` + `nats-v1-3` PVCs offline → handoff bucket
  loses quorum (2/3 replicas gone) → defense **fully** stops; reads return
  `context deadline exceeded`; NATS auto-recovers when PVCs return, but **defense
  only recovers after a pod restart**. ← the "auto-healing" failure, **F1 is the
  focus of this effort**.
- **F2 (session down → read-only FS):** partial failure, JetStream "read-only file
  system" snapshot errors; recovered after NATS pod restart. **Out of scope here.**

---

## 2. Failure model — hypothesis (H), to be proven by §3

1. **Quorum loss, connection survives.** Handoff bucket is RF=3 on 3 of 5 nodes; 2 of
   those die → that bucket has 1/3 replicas → **NO quorum**. Cluster *meta* still has
   3/5 nodes, so the client stays **`CONNECTED`**. Handoff-bucket reads return
   `context deadline exceeded`; the connection itself never drops. **(H)**
2. **The classifier misses it.** `recordKVError` (`manager_degraded.go`) only counts an
   error if `natsutil.IsConnectivityError || IsDegradingJetStreamError`.
   `context.DeadlineExceeded` matches **neither** (`natsutil/errors.go` checks
   `nats.ErrTimeout`, `i/o timeout`, `connection refused`, … but not Go's context
   deadline) → KV-error counter never increments → manager **never enters
   `StateDegraded`** → `attemptRecoveryFromDegraded` **never runs**. **(H)**
3. **Claims never get written.** During the outage, `handoff apply failed: claim get …
   context deadline exceeded` (report logs) → partition ownership claims are never
   persisted to the handoff bucket. **(H)**
4. **The gate latches shut.** `ClaimBasedResolver.reconcileOnce` swallows the deadline
   error from `kv.Keys` and returns without updating its cache
   (`claim_resolver.go`); `GetOwner` returns `!ok`; pull-gating is **fail-closed**
   (`worker_consumer.go: shouldSuppressPull` → `resolve_error` /
   `"partition not found"`), the partition consumer polls every 150 ms and **never
   force-refreshes** (`partition_consumer.go`). **(H)**
5. **No self-heal.** After quorum returns: connection was never lost, manager is still
   `StateStable`, nothing triggers a re-apply, claims stay unwritten → gate stays shut
   → **work stoppage until pod restart** (fresh `Start` → fresh apply → claims written
   → gate opens). The restart-time `wrong last sequence: key exists` logs are the
   *expected* stable-ID reclaim, **not** the bug. **(H)**

### Candidate defect sites (both to be confirmed)
- **(a) Classification gap:** "connected but KV reads timing out" (quorum-loss / slow
  KV) is not modeled as a degrading condition; only connectivity-loss and
  whole-bucket-missing are.
- **(b) Fail-closed gating + no active refresh:** a missing claim suppresses pulls
  indefinitely with no force-refresh and no re-apply trigger. Overlaps a **known
  deferred** "resolver fail-open" follow-up already recorded in memory.

---

## 3. Reproduction design

Two tiers in **one standalone Go module** at `tmp/parti-repro/` (own `go.mod`; harness
depends only on `nats-server/v2` + `nats.go`, **not** on parti internals — true
black-box). parti is consumed purely through its **public** consumer/manager API.

### Tier 1 — deterministic symptom-injection (primary baseline + future regression guard)
- Wrap the `jetstream.KeyValue` handed to the resolver so the **handoff** bucket's
  `Get/Keys/Watch` return `context.DeadlineExceeded` **on command**, while the
  connection stays `CONNECTED` and all other buckets keep working.
- Drive the public API exactly as FDC does: `NewDynamic(...)` + `WithPullGating(true)`
  + `WithProcessingGate` + `WithResolver{HandoffBucketName}`, FDC timings (HB 3s/TTL
  10s, WorkerIDTTL 45s, Election 7s, two-phase handoff, OperationTimeout 10s).
- **Scenario:** 2 workers + N partitions → Stable & pulling → flip handoff reads to
  time out → **assert (i) work stops (pull suppressed) AND (ii) manager stays out of
  `StateDegraded`** (proving the classification gap) → restore handoff reads →
  **assert whether the worker auto-recovers without restart.**
- **Expected on v2.5.0 (H):** stays suppressed → **FAIL** (the bug).
- Fast, fully deterministic, CI-stable. This is the test that pins the exact path.

### Tier 2 — real 5-node embedded cluster (faithful end-to-end), gated `PARTI_REPRO_CLUSTER=1`
- 5 embedded `nats-server`s in a cluster, JetStream/file storage; client seeded with
  **all 5 URLs** so it stays connected through node kills.
- Create parti buckets RF=3 (handoff/election/assignment=file, heartbeat=memory),
  consumer RF=3 memory — matching prod.
- Bring 2 workers to Stable & pulling.
- **Inspect the handoff bucket's actual replica placement** (stream `PeerInfo` /
  cluster info), then **kill the 2 peer nodes that break that bucket's quorum while
  meta-quorum survives (3/5)** → handoff reads time out, connection stays up.
- Same assertions as Tier 1, plus: restart the 2 nodes (quorum restored) → does it
  self-heal without a worker restart?
- Slower; proves the **real causal chain**, not just the symptom. Note: JetStream
  replica placement is automatic and cannot be pinned, so the harness must *read*
  placement and *choose* which nodes to kill (avoiding a meta-quorum kill).

> **Why not a 3-node total-quorum-loss shortcut:** killing 2 of 3 nodes drops the
> meta-leader and likely the client connection, which **trips `IsConnectivityError`**
> and routes into the *handled* Degraded path — reproducing a **different** failure
> mode and masking the production bug. Fidelity is load-bearing here.

### Version matrix (the deliverable that answers "did our refactor fix it?")
Same tests, swap one line (`require <version>`, or `replace => /home/arlo/projects/parti`
for HEAD); nats.go pinned to **v1.50.0** for the released versions.

| version | work stops? | enters Degraded? | auto-recovers after restore? |
|---|---|---|---|
| **v2.4.1** (prior rollback baseline) | ? | ? | ? |
| **v2.5.0** (incident build) | ? | ? | ? |
| **HEAD** (v2.5.0 + 39) | ? | ? | ? |

Measuring all three neutralizes any residual "which version really ran" doubt and
isolates exactly what (if anything) changed across versions.

---

## 4. Deliverables (scope stops *before* the fix)
- `tmp/parti-repro/` module with Tier 1 + Tier 2 tests.
- Filled-in version matrix.
- **Retrospective ("what we got wrong"):** the classification taxonomy never modeled
  "connected but KV reads timing out" (only connectivity-loss and whole-bucket-missing);
  recovery keys off **connection state**, not **KV-read health**; pull-gating is
  fail-closed with no active refresh; and the self-healing test suite never exercised
  *quorum-loss-while-connected* (only whole-bucket-missing and connectivity loss).
- **Promotion note:** once a fix is green, lift the harness into `partitest`
  (5-node + selective kill) and the test into `test/integration/failure/` as a
  permanent regression guard (mirrors the existing integration-discipline rules).

## 5. Out of scope
- Designing / implementing the fix.
- F2 (read-only-filesystem / session-down) variant.

---

## 6. Consolidation (pending)
An independent Codex investigation of the same incident is being run **blind** (from
the raw issue report + the two repos, without this document) to produce a second
root-cause analysis + reproduction recommendation. This section will be updated to
record: (a) where the two analyses **agree** (raises confidence), (b) where they
**disagree** (the disagreement becomes a must-settle assertion in the repro), and
(c) any failure modes / repro angles Codex surfaces that this draft missed. The final
artifact is the consolidated design.
