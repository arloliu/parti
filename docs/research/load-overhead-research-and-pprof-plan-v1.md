# Parti Load Overhead — Research Report & pprof Investigation Plan (v1)

Date: 2026-07-04 · Scope: parti v2.8.2 (post scan-suppression / scan-gating), nats-server 2.14.1, nats.go 1.52
Provenance: two orchestrated research passes —
(a) external deep research: 20 primary sources fetched, 25 top claims 3-vote adversarially verified (21 confirmed / 4 refuted);
(b) codebase load audit: 7 subsystem auditors (108 wire-operation inventory entries) + prior-measurement reader, 3 hunting lenses, 25 deduplicated candidates each verified by 3 independent lenses (load-bearing / quantify / remedy) against the AGENTS.md contracts.
All numbers in §3–§6 are carried from the subsystem maps, `docs/plans/iops-investigation/findings.md`, `docs/plans/perf-measurement/03..05-findings-*.md`, and the verified-candidate verdicts — none are new measurements. External claims in §2 carry their sources inline.

---

## 1. Executive summary

1. **The structural cost is the durable-consumer population itself, and it is load-bearing.** One durable per partition is `consumer.Dynamic`'s delivery contract (per-partition cursor, ack isolation, lossless same-durable rebind on handoff). All three verification lenses independently confirmed: RSS ≈ 90 MiB + 0.793 MiB·P (validated to P=10k) and consumer state/raft replication ≈ 72–81% of IOPS are the price of the contract, not overhead. The only structural reduction is FINDING-A (K≪P collapse as a new consumer type, unbuilt); the user-side lever today is fixed-K `partition()` (~300 MiB / ~12 IOPS projected at K=256/N=5000, proven in `test/integration/fixedpartitions/`).
2. **External research confirms the meta layer is the server-side scaling wall — but parti's own rig measurements sit far below the vendor's warning band, and that tension is testable.** Synadia's operational thresholds put clusters above 5,000 total Raft groups in the "needs more meta-leader CPU/IO" regime with 1–3s meta snapshots at 1,000–5,000 groups; parti's report-05 measured ~3 ms/1,000 consumers (25–31 ms at P=10k) on the clean single-purpose rig. Experiment E6 arbitrates directly.
3. **Post-v2.8.2, idle meta-layer consumer churn is a small constant floor** (leader heartbeat poll ~8 creates/min + audit ~4/min + forced gate passes ~2W/10min). The remaining P-linear *request* terms are the idle pull floor (P/5 MSG.NEXT/s + P/2.5 idle-HB/s from BatchSize=1/FetchTimeout=5s) and, with two-phase+gate on, the forced-pass Get term W·(1+P)/300 /s.
4. **8 actionable overhead findings survived 3-lens adversarial verification** (7 reducible + 2 tunable, one shared): the W-fold duplicated cluster-global claim sweep (strongest — all three lenses agreed), the ~2P-read commit/stabilize full walks per rebalance, the O(W²) removal-guard payload fan-out, the source-reconcile full-table transfer, the 200 Hz idle resolver timer, the O(W²/5)/s sweepExpired walk, the startup triple bucket walk, plus two safe cadence knobs (bucket-epoch fence interval, pull-floor FetchTimeout/PullHeartbeat). Most are *not yet material at W≤100 / P≤10k* — the measurement plan (§5) decides which get built, in which order.
5. **A further 8 candidates were killed or bounded by verification** as load-bearing recovery paths or noise (heartbeat poll floor, follower election probe shape, dual assignment watchers, PutIfEpoch double-Get, …) — §4.3/§4.4 records why, so they are not re-litigated.
6. **Profiling methodology is settled** (§2.3): `prof_port` + `/debug/pprof` or the system-account `nats server request profile` path server-side; `net/http/pprof` + explicit `SetBlockProfileRate`/`SetMutexProfileFraction` client-side; `/jsz?consumers=true&raft=true` for per-consumer Raft detail. The perf rig needs a re-pin from v2.3.0 to current main before any campaign (§5 prerequisites).

---

## 2. External research: NATS cost model & profiling methodology (verified claims)

### 2.1 Server-side cost model

- **Raft-group-per-asset.** Each replicated stream owns its own Raft replication group, each R>1 consumer likewise; the meta cluster tracks every asset. Every replicated asset carries baseline metadata/placement/leadership/replication overhead — but **no primary source quantifies per-asset at-rest cost** (no memory/goroutine/IOPS figures exist in the literature; empirical measurement required). Nuances from verification: one R3 consumer = one Raft group with 3 peers; R1 assets have no dedicated Raft group but are still tracked in meta state; ephemeral consumers get no consumer-level Raft group at all.
  Sources: https://www.synadia.com/blog/jetstream-raft-per-stream-scaling · https://www.synadia.com/insights/checks/nats-meta-pending-high (3-0 verified)
- **Consumer lifecycle churn is direct meta-Raft load.** Every consumer create/delete is a Raft operation on the meta group (R>1: proposed by the meta leader, quorum-committed). Concrete conversion: ephemeral-per-request at 100 rps = 12,000 meta Raft ops/min. Symptom cascade: elevated API-pending, slower asset creation and consumer-info, in the extreme meta-leader elections that temporarily halt all JetStream API processing. This retroactively prices exactly what v2.8.2 eliminated (throwaway ordered consumers from `kv.Keys()`/`WatchAll`) and rules out any future design that churns consumers per rebalance epoch.
  Sources: https://www.synadia.com/insights/checks/nats-consumer-churn-high · nats-server `jetstream_cluster.go` assign/removeConsumerOp (3-0 verified)
- **Historical benchmark warning.** On v2.10.9, full-bucket KV WatchAll enumeration of an identical 100k-key bucket varied ~100× (527 ms vs 25–100 s) purely on the temporal *write pattern* of the bucket — maintainer-attributed to server-side stream data layout, fixed before 2.11. Durable lesson: KV-scan benchmarks must use realistic interleaved write patterns (parti's multi-worker heartbeat writes), never synthetic tight-loop population. Do not extrapolate the anomaly itself to 2.14.
  Source: https://github.com/nats-io/nats-server/issues/4987 (3-0 verified)

### 2.2 Operational thresholds and the 10k-partition question

- Synadia META_004: meta snapshot duration warning at **5 s**, critical at **30 s**. Expected: <1 s under 1,000 Raft groups; 1–3 s at 1,000–5,000 groups (on properly provisioned SSD hardware). Sustained slow snapshots stall JetStream API operations and can time out leader elections.
- Synadia META_005: default watermark **5,000 total Raft groups** (counting formula: streams×replicas + consumers×replicas + meta group); beyond it, snapshot times increase and the meta leader needs more CPU and I/O.
  Sources: https://www.synadia.com/insights/checks/nats-meta-snapshot-slow · https://www.synadia.com/insights/checks/nats-meta-pending-high (3-0 verified)
- **Tension to resolve empirically:** 10k per-partition R3 durables ≈ 2× beyond the documented band by consumer groups alone — yet parti's report-05 measured meta snapshots at ~3 ms/1,000 consumers (25–31 ms at P=10k, data-path P99 flat at ~1.4 ms) on the clean rig. Plausible reconciliation: the vendor bands are conservative operational guidance for mixed multi-tenant clusters; a single-purpose cluster with small per-consumer meta state does far better. **E6 tests this directly**; if snapshot duration inflates toward the vendor band at 10k, FINDING-A gains priority.
- Caveats (from verification): all quantitative sizing comes from one source family (Synadia Insights runbooks — maintainer-company operational docs, not marketing); **no independent operator reports of 5k–50k-durable clusters survived verification**. Refuted 0-3: "R3 consumers contribute exactly 3× the meta-state footprint of R1" (direction holds via the counting formula; the linear-3× quantification does not).

### 2.3 Profiling methodology (server + client)

Server-side (nats-server 2.14):
- Enable `prof_port` (no config-reload support). Heap/allocs: `GET /debug/pprof/allocs` (returns instantly). CPU: `GET /debug/pprof/profile?seconds=N` (blocks for the duration). Analyze with `go tool pprof`. The port is **unauthenticated — never expose it**; production alternative: `nats server request profile allocs|cpu` via NATS CLI, **system account only** ($SYS.REQ.SERVER.<id>.PROFILEZ).
  Source: https://docs.nats.io/running-a-nats-service/nats_admin/profiling (3-0 verified)
- JetStream introspection: `/jsz?consumers=true&raft=true` is the documented endpoint for per-consumer detail incl. Raft groups. Parameter implication chain enforced in code: `consumers=true` ⇒ `streams=true` ⇒ `accounts=true` — response size scales with total asset count at 10k consumers (consumer detail nests under stream detail); `leader-only=true` restricts to the meta leader. Monitoring endpoints compute on demand (no background sampling cost when unpolled).
  Sources: https://docs.nats.io/running-a-nats-service/nats_admin/monitoring · monitoring_jetstream · nats-server `monitor.go` cross-check (3-0 verified)
- Refuted 0-3: the prometheus-nats-exporter Raft-filter claim — the Prometheus story for Raft-group signals is **unverified either way**; plan to poll `/jsz` directly (as the harness's capture-jsz does).

Client-side (parti workers):
- Production profiling is officially safe per go.dev, but estimate CPU-profile overhead before enabling; collect one profile at a time. Blank-importing `net/http/pprof` registers all handlers on `http.DefaultServeMux` (custom mux ⇒ manual registration).
- **Block and mutex profiles return empty by default** — the worker must call `runtime.SetBlockProfileRate` and `runtime.SetMutexProfileFraction` at startup (empirically reproduced: heavy contention yields zero samples without them). These are precisely the profiles that would expose nats.go dispatch/flusher and parti lock contention (`pollMu`, `applyStoreMu`).
  Sources: https://go.dev/doc/diagnostics · https://pkg.go.dev/net/http/pprof (3-0 verified)

### 2.4 nats.go v1.52 client cost levers

- `Fetch()`/`FetchNoWait()` create a **new subscription (fresh inbox + ChanSubscribe) per call, no pre-buffering** — documented as worse-performing than `Consume()`/`Messages()` for continuous retrieval (fine for on-demand batches where overhead amortizes).
- `Consume()`/`Messages()` default to **500 buffered messages per context** (DefaultMaxMessages, buffer channel sized exactly to it) with 30s pull expiry — an upper bound of 500×P messages buffered fleet-wide at one active Consume per partition; tunable via `PullMaxMessages`/`PullMaxBytes`/`PullExpiry`. Upper bound, not typical occupancy.
  Source: https://pkg.go.dev/github.com/nats-io/nats.go/jetstream + v1.52.0 `jetstream/pull.go` code-check (3-0 verified)
- Parti note: `internal/durable` issues its own pull requests (BatchSize=1, FetchTimeout=5s defaults) — §3.3/§4.2 covers the resulting idle floor; the 500-message buffer term applies to any future Consume()-based path.

### 2.5 Literature gaps (need measurement, not search)

Per-consumer at-rest cost (memory/goroutines/state-file IOPS, R1 vs R3); effect size of the 2.12+ mitigations (async meta snapshots, `meta_compact*`, consumer pause) on the 5,000-group watermark on 2.14; KV watcher fan-out vs polling cost; consumer-count-reduction trade-offs (multi-filter durables, partition() transform, bucket sharding). These map onto experiments E1/E2/E6 and the existing FINDING-A / virtual-partition assessments.

---

## 3. Parti steady-state load model (from the codebase audit)

Ops/sec as functions of W (workers) and P (partitions). Cost classes: **raft-write** (filestore append, replicated), **meta-proposal** (consumer create/delete, meta-layer Raft), **read** (DIRECT.GET / STREAM.INFO, replica-servable), **push** (delivery messages, in-memory).

### 3.1 Every-worker recurring terms

| Term | Rate (cluster) | Class | Source |
|---|---|---|---|
| Heartbeat publish | 0.2·W /s | raft-write | `internal/heartbeat/publisher.go:414` (5s interval) |
| Follower election probe | 0.6·(W−1) /s (rejected CAS publish + internal Get per tick) | read + pre-append reject (no filestore write) | `internal/election/nats_election.go:140` (ElectionTimeout/3 ≈ 3.33s) |
| Stable-ID claim renew | 0.04·W /s | raft-write | claimer.go:303 (25s) |
| Bucket-epoch fence | 0.5·W /s STREAM.INFO (5 buckets / 10s) | read | `manager_setup.go:691` |
| Alias + commit reconcile Gets | 2 × W/30 ≈ 0.067·W /s | read | `manager_assignment.go:592, :870` |
| Source reconcile Get | W/30 /s, each shipping the **full O(P)-byte gzip table** (skip-guard skips decode only) | read + egress ≈ (W/30)·bytes(P) | `source/nats_kv.go:1169, 1205-1209`; gzip ~2.5 KB @ P=1k, ~25 KB @ P=10k |
| Resolver + sweep gate probes (two-phase + gate on) | 4 × W/30 ≈ 0.133·W /s STREAM.INFO | read | `claim_resolver.go:1184`, `twophase.go:579` |
| Forced full scan passes (both gates, every 20th tick) | ≈ W·(1+P)/300 Gets/s + W/300 meta-proposals/s | meta-proposal + read | ScanGateMaxSkippedPasses=19 |
| Standing ordered ephemerals | ~4·W (alias, commit, claim-resolver, source watchers) + idle-HB pushes W/5s each | meta-proposal at (re)establish only | assignment/source/handoff maps |
| Claim-resolver batch timer | 200 wakeups/s/worker, **zero wire** | client CPU | `claim_resolver.go:884-887` |

### 3.2 Leader-only terms

| Term | Rate | Class | Source |
|---|---|---|---|
| Lease renewal | 0.3 /s | raft-write | `manager_election.go:218` |
| Heartbeat poll scan (suppression floor) | 1 Keys() / 7.5s ⇒ ~8 create+delete pairs/min + O(W) meta-only replay per scan | meta-proposal | `worker_monitor.go:307`; deliberate v2.8.2 floor |
| Apply-audit | (Keys + W serial Gets)/15s ≈ W/15 + 0.13 /s | meta-proposal + read | `calculator_audit.go:59` |
| Heartbeat watcher deliveries | W/5 /s pushes (values discarded) | push | `worker_monitor.go:382` |
| Commit-payload GC | ~2 meta-proposals + ~12 reads / 5 min (+ per commit) | meta-proposal + read | `commit_gc.go:248` |
| livePartitionSet (two-phase) | 2 Gets/30s | read | `manager_handoff.go:85` |
| sweepExpired client walk | O(W²/5) map visits/s on the watcher goroutine | client CPU | `worker_monitor.go:495` |

### 3.3 Per-partition standing terms

| Term | Rate/size | Class | Source |
|---|---|---|---|
| Durable consumer population | P consumers ⇒ RSS ≈ 90 MiB + 0.793 MiB·P; IOPS ≈ 3.6 + 0.028·P (mem+R3); meta-snapshot ≈ 3 ms/1000 consumers | standing raft groups + state | reports 03/04/05, `worker_consumer.go:491` |
| Idle pull floor | P/5 MSG.NEXT/s + P/2.5 idle-HB/s + P/5 expiry statuses/s (BatchSize=1, FetchTimeout=5s) | JS API request + push, zero IOPS | `worker_consumer.go:768-775` |
| Active amplification | ~1 pull request per message at batch=1 | JS API request | same |

### 3.4 Dominant terms at the operating points

Model values use the committed fit (IOPS/CPU/RSS, production mem+R3 config); pull/scan terms are cadence arithmetic. Two-phase + processing-gate terms apply only when on.

| Cell | RSS (model) | IOPS (model) | Idle CPU (model) | Pull floor msgs/s | Forced-pass Gets/s (2φ+gate) | Top per-W request terms |
|---|---|---|---|---|---|---|
| W=10, P=512 | ≈496 MiB | ≈18 | ≈0.15 cores | ~410 | ~17 | fence 5/s, election 5.4/s — all trivial |
| W=50, P=2000 | ≈1,676 MiB (measured 1,660) | ≈60 (measured 62) | ≈0.41 cores | ~1,600 | ~334 | election 29/s, fence 25/s |
| W=100, P=10000 | ≈8,020 MiB (measured 7,916) | ≤283 upper bound (measured 146) | ≈1.82 cores (measured 2.08 incl. load) | ~8,000 | ~3,334 | election 59/s, fence 50/s, source egress ~83 KB/s |

**Dominant terms, all cells:** (1) the P-linear standing consumer population — RSS is the binding ceiling and consumer state/raft replication ≈ 72–81% of IOPS (M1.2/M2.A attribution); (2) the P-linear idle pull-request floor (largest JS-API request count, zero IOPS, embedded unablated in the b·N CPU coefficient); (3) at W=50+/P=2000+ with two-phase+gate, the W·(1+P)/300 forced-pass Get term is the largest *read* term. Meta-proposal churn is a small constant floor post-v2.8.2. Per-W terms are linear and individually small at W≤100.

---

## 4. Verified overhead findings

Verdict legend: each candidate was judged by three independent lenses — *load-bearing* (does a contract/recovery path require it), *quantify* (re-derive the cost from source; material at W=100/P=10k relative to the P-durable baseline?), *remedy* (cheapest contract-preserving change). "Disputed" = lenses split; the convergent safe subset is recorded.

### 4.1 Reducible (fix identified, contract-preserving)

| Finding | Proposed fix | Materiality note |
|---|---|---|
| Source reconcile ships the full O(P) gzip value every 30s/worker even when unchanged; `WithLeadershipProbe` (10× follower cadence cut) has zero production callers | Port the shipped v2.8.2 position gate (`natsutil.ProbeKVStreamPos`, dedicated handle, fail-open, forced full Get every 20th tick) in front of reconcileOnce; document leadership-probe wiring | ≤100 KB/s at 100/10k — pays off only at large W×P (E7 decides) |
| Commit+stabilize phases Get **every** partition of the next assignment (~2P cluster reads per rebalance version, mostly no-ops) | Restrict both phases to the acquired-diff preparePhase already computes; keep unconditional Get+CAS on the mutating subset; sweep arms remain the healer. Do **not** do the cache-consult skip (stale-skip divergence) | Largest rebalance-wave read term (E4 decides) |
| Every worker runs the cluster-global 30s claim sweep — **W-fold duplication of one idempotent chore** (all 3 lenses: reducible) | Leader-gated ticker sweep (`SweepAuthority: m.isLeader.Load`) with follower backstop every Nth tick (~5 min); Apply-origin sweeps, startup hygiene/resume, orphan reap untouched | Divides the dominant W×P Get term by ~W (E8 is the decision experiment) |
| Startup triple walk of the handoff bucket (resolver warm() Keys+P Gets is 100% redundant with the WatchAll initial replay; resume re-walks what hygiene just read) | Drain the watcher initial replay instead of warm(); pass hygiene's snapshot to resume. Probe/fence consolidation and cross-stack mirror **declined** (layering + double-probe independence) | Startup-slice only |
| Claim-resolver 5ms batch timer wakes 200×/s/worker even idle | Event-driven timer arming (arm on first staged item, disarm after flush); identical 5ms coalescing latency. Leave the O(P) COW copy alone unless churn copies measure hot | Client CPU only; promoted if E5 shows ≥1% of a core |
| Leader sweepExpired walks the entire lastSeen map per watcher event (O(W²/5)/s) | Earliest-deadline gate (skip walk while now < min(lastSeen)+hbTTL) — bit-identical flag timing, preserves the 3×hbTTL holiday proof | ~2k visits/s at W=100 (noise); matters at W≫1000 |
| Removal-guard payload fan-out: every worker with removals fetches ALL W payloads per commit (O(W²) reads, O(W·P) bytes per wave) | Leader publishes one content-addressed union-set payload (it already computes the union in checkCoverage); guard fetches 1 ref, legacy fan-out fallback; union key joins GC LiveRefs | Warranted at W≳500 (E4 decides) |

### 4.2 Tunable (mechanism required; cadence/knob is the safe lever)

| Finding | Safe knob |
|---|---|
| Bucket-epoch fence interval conflated with OperationTimeout — tightening OT to the docs-recommended 3s silently **triples** fleet probe rate | New `BucketEpochProbeInterval` (default preserves 10s; 30–60s safe), ticker only — per-probe deadline stays OperationTimeout. Do not centralize on leader or piggyback gates |
| Idle pull floor from BatchSize=1/FetchTimeout=5s (P/5 + P/2.5 + P/5 per second) | Guidance-first; run E2 (the designed-but-never-run M1.5 cell) before touching defaults. Remedy lens: cap `PullHeartbeat` at 2.5s so FetchTimeout=30s cuts MSG.NEXT 6× without slowing ErrNoHeartbeat detection |
| Apply-audit (Keys + W serial Gets / 15s) — disputed, converged safe subset | Bound per-key Gets with the hbTTL/2 opCtx (line-261 wart); optionally AuditInterval → 2×HeartbeatTTL. **Rejected:** watcher-fed cache (TTL expiry emits no watch event → audit_repair targets a corpse, regressing TestE2E_PartialApplyFailureRecovery); grace-hoist (breaks pinned RecordAuditCounts-always-fires) |

### 4.3 Disputed / bounded — do not re-litigate without new evidence

- **Follower election probe (0.6·W req/s):** Get-first probing regresses cross-feature contract 1 — Create's `ErrNoStreamResponse` is the follower's *only* whole-bucket-classified path to Degraded on election-bucket loss (a Get surfaces `ErrNoResponders` → transient). Any change needs Create-fallthrough-on-any-Get-error **plus** a new election-bucket-only wipe integration test (currently unpinned). Diskless request handling; not worth it below W≈1000.
- **PutIfEpoch double-Get (3 wire ops per claim write):** the re-Get is a post-rate-limiter freshness fence with deliberate polarity — same-epoch resets must *lose* to in-flight epoch-bumped transitions; naive rev-threading inverts a recovery race. Optimistic-first-with-fallback is the only acceptable shape; <1% of load anyway.
- **Legacy-compat publish path (W Gets + W alias Puts per publish):** step-11 alias writes are the worker bootstrap channel today (`waitForAssignment` polls `assignment.<id>`); scheduled for removal in v3.0 behind the commit-driven startup redesign. Only per-worker delta-skip + capability caching are safe; noise at ≤0.1 Hz publish cadence.
- **Heartbeat poll floor (Keys every 7.5s):** the deliberate v2.8.2 suppression floor — sole detector when the watcher goes silent (Updates() does not close on server restart); sweep false-flag safety, the 3×hbTTL holiday proof, and the sim-pinned 75s reassignment deadline are coupled to it. A self-observation fence is a full design effort (the deferred FM8-class item), not a cheap change.
- **Dual assignment watchers (2W standing ephemerals):** WatchFiltered merge is mechanically possible but couples two deliberately different failure domains (bounded-exhaustion→Degraded vs unbounded-retry→recordKVOpError, pinned by envelope tests); ~1% of consumer population at P=10k. Correct reduction is the scheduled v3.0 alias removal.
- **Degraded-recovery probe loop (1s, fresh handles):** dedicated cached probe-handle set (halves STREAM.INFO) + per-episode backoff on the two hold states only is agreed safe; gate *order* is pinned auto-heal design. Cost exists only during degraded episodes.
- **Commit fan-out + 30s unconditional commit re-read:** identical-revision redelivery is the case-(c) payload-failure retry path — a decode-site skip-guard starves an unapplied worker. Only a guard with a "settled" conjunct (CurrentAssignment.Version ≥ decoded Version) is safe, saving ~1ms/worker/30s of decode CPU. Not material at W=100.
- **Commit-GC 5-min bucket walk:** reducible in principle (position gate + Nth-pass backstop) but ~0.007% of baseline; Tier-1 fix is raising the interval. **Verification side-find: `_commit_log.<V>` keys are never deleted — long-run bucket growth is a worthier item.**

### 4.4 Not-material (verified, leave alone)

livePartitionSet double commit-Get (0.067 req/s leader); rebalance re-enumeration (+1 scan/rebalance; the single-worker diagnostic Keys is deletable hygiene); waitForAssignment 100ms poll (~1–2s typical lifetime; jittered backoff = free hygiene); WaitState 50ms poll (client-local); drain-on-remove CONSUMER.INFO poll (shared 10s budget, opt-in); post-apply PublishNow (1 Put/worker/rebalance; pinned by ack-before-Stable invariant tests — removal is negative-value).

### 4.5 Load-bearing (the price of the contract)

**One durable per partition** is `consumer.Dynamic`'s delivery contract (per-partition cursor/ack isolation/ordering; same-durable rebind makes handoff lossless). All fitted coefficients already assume the shipped mitigations (memory consumer state + R3). Structural reduction paths: FINDING-A K≪P collapse (decided: new coexisting consumer type, unbuilt) or user-side fixed-K `partition()`. Do not attempt to "optimize" the durable population inside the Dynamic contract, and do not build FINDING-A below ~10k partitions.

---

## 5. Measurement & pprof plan

**Assets:** `test/perf-measurement/` (docker 3/5-node rig, `cmd/harness` knob flags, `internal/instrumentedjs` per-(bucket,op) RPC counting, `cmd/aggregate` merging cgroup/iostat/jsz/rpc_counts, `cmd/fitmodel`, RUNBOOK tiered gates), `test/simulation/` (cadence-contract oracles incl. `heartbeat_scan_flatness.yaml`), `test/integration/fixedpartitions/`.

**Prerequisites:**
1. **Re-pin the IOPS-harness go.mod from parti v2.3.0 → current main (v2.8.2)** — every existing rig campaign predates scan suppression.
2. Run `tier0-validate.sh` (~45 min instrument-honesty gate) and `tier0.5-calibrate.sh` after the re-pin.
3. Add `net/http/pprof` listeners to the harness binary (per in-process manager group or leader-tagged) **with `runtime.SetBlockProfileRate`/`SetMutexProfileFraction` set**; start nats-server with `prof_port` so `/debug/pprof` is reachable alongside the monitoring port (§2.3; never expose either publicly).
4. Capture set per run: `rpc_counts.csv`; `capture-jsz` extended to poll `/jsz?consumers=true&raft=true` (consumer counts, meta-cluster state — note response size scales with asset count, poll modestly); nats-server log grep for meta-snapshot WRN/duration lines; `capture-iostat` + `capture-cgroup-io` + docker stats; 30s client CPU/heap/mutex profiles on the leader manager and one non-leader worker.

**Grid:** P ∈ {512, 2000, 10000} × W ∈ {10, 50, 100} — diagonal cells minimum (10/512, 50/2000, 100/10000). Production arm (consumer memory storage, R=3, file stream) unless stated. Two-phase handoff + processing gate ON for E4/E5/E8, OFF arm for differencing.

### E1 — Post-v2.8.2 idle re-baseline (closes the declared open item)
Idle matrix (no publish), 60s warmup + 60s capture × 3 reps, diagonal cells.
- Capture: rpc_counts by (bucket, op); jsz consumer counts over time (ephemeral churn); iostat/cgroup IOPS.
- Falsifiable predictions: idle IOPS constant ≈ 114 with slope ≈ 0.028/P (v2.8.2 removed *scan* churn, not writes; M1.9: parti KV ≈ 2% of IOPS) — a shift beyond the 1.05e-05 slope MDE means an unmodeled write-side effect. Heartbeat-bucket ephemeral creates ≈ 8/min (poll floor) + audit ≈ 4/min, watcher-triggered ≈ 0 with a stable fleet (upward ⇒ suppression regression; cross-check `heartbeat_scan_flatness` oracle). Handoff-bucket Keys-consumer creates ≈ 2W/10min (forced passes), gated ticks show 4W/30s STREAM.INFO and zero Keys (upward ⇒ gate unlatched).

### E2 — Pull-floor ablation (the designed-but-never-run M1.5)
Cell 100/10000, idle + load (X=200 msg/s), `--fetch-timeout 5s` vs `30s`, `--batch-size 1`.
- Prediction: MSG.NEXT drops ~6× (2000/s → ~333/s); server CPU delta isolates the pull-floor slice of the b·N coefficient (estimate 3–15% of 1.76 idle cores at P=10k ⇒ 0.05–0.25 cores); latency unchanged (messages push to the outstanding pull); IOPS unchanged (floor is diskless). CPU delta ≈ 0 demotes the finding to not-material; latency degradation would contradict the push-to-outstanding-pull model.

### E3 — Batch-size amplification under load
Cell 50/2000, X ∈ {80, 200} msg/s, `--batch-size` 1 vs 16.
- Prediction: JS request rate falls ~X → ~X/batch; the c·X CPU coefficient (0.00066 cores per msg/s) shrinks measurably; latency stays at the ~1.3 ms floor.

### E4 — Rebalance burst anatomy (two-phase + gate ON)
Each diagonal cell: steady state → kill one worker → convergence → re-add, ×3.
- Capture: rpc_counts deltas per 1s (handoff Gets/Puts, assignment Gets/Creates/Puts, payload Gets); leader + one gaining + one losing worker CPU profiles during the wave; claim-watcher delivery counts.
- Predictions: per commit version ≈ 2P cluster-wide handoff reads (commit+stabilize walks) + ~6T Gets + 3T raft writes for T moved partitions; removal-guard fan-out ≈ (workers-with-removals)·(1+W) payload Gets (W² worst case on join); W alias Puts + W payload Creates + W PublishNow Puts. At 100/10000: ~20k handoff Gets + up to ~10k fan-out Gets per wave. Downward ⇒ short-circuits cover more than modeled (downgrades both reducible fixes); upward ⇒ retry-driven re-walks (check scheduleApplyRetry cadence).

### E5 — Client-side profiling (leader + one worker)
30s CPU + heap + mutex profiles: (a) idle, (b) during E4's wave, at 100/10000 and 50/2000.
- Predictions: idle worker — 200 Hz resolver timer visible in scheduler samples but <0.1% CPU (≥1% promotes the event-driven-arming fix). Churn worker — `applyPendingBatch` maps.Copy + `handoff.UnmarshalClaim` dominate resolver CPU; commit-payload gzip/sha256/JSON burst per commit. Leader — sweepExpired/watcher classification predicted *invisible* at W=100; `writePayloads` gzip.BestCompression spikes per rebalance; audit GetHeartbeats wall-time ≈ W×RTT serial per 15s tick (mutex profile: pollMu, applyStoreMu). Any flagged path appearing above ~1% of a core at W≤100 promotes its deferred fix.

### E6 — Server-side meta + filestore (arbitrates §2.2's tension)
Same cells, idle + churn.
- Capture: nats-server `/debug/pprof` CPU+heap on the meta leader; `/jsz?consumers=true&raft=true` (total consumers, raft groups); meta-snapshot log lines; iostat 1s series.
- Predictions: total consumers ≈ P + ~4W ephemerals + fleet durables; meta-snapshot ≈ 3 ms/1000 consumers (25–31 ms at P=10k, never stalls the data path — P99 stays ~1.4 ms); the deterministic ~10× IOPS spike every ~5s appears **only** in file-consumer-state arms (pdflush, vm.dirty_writeback_centisecs=500) and vanishes under memory consumer state (M1.7/M2.A signature). Snapshot duration inflating toward the Synadia band (§2.2) ⇒ escalate; FINDING-A gains priority.

### E7 — Source reconcile egress
Diagonal cells, NatsKV source with the P-sized table, idle.
- Prediction: (W/30)·bytes(P) ⇒ ~83 KB/s at 100/10000, ~4 KB/s at 50/2000 — confirms not-material at this scale; the gate-port fix triggers only if a real deployment profile (bigger tables × bigger fleets) pushes this toward Mbit/s.

### E8 — Sweep W-fold scaling isolation (decision experiment for the leader-gated sweep)
Two-phase ON, fixed P=2000, W ∈ {10, 50, 100}, idle 15 min (covers a forced-pass cycle).
- Prediction: handoff-bucket Gets/s linear in W at fixed P (W·(1+P)/600 gated floor); the leader-scoped-sweep fix would flatten it to ~(1+P)/600 + follower backstop.

---

## 6. Interpretation guide

| Observed signature | Hypothesis confirmed | Decision |
|---|---|---|
| E1: idle IOPS ≈ 114 + 0.028/P; heartbeat creates ≈ 12/min; handoff creates ≈ 2W/10min | v2.8.2 gates working; write cost unchanged | Close the re-measure open item; update RUNBOOK baseline; no code action |
| E1: consumer-create churn above the modeled floor | Suppression/gate regression or unlatched gate | File bug; reproduce with the matching simulation scenario before any fix |
| E1: idle IOPS materially shifted vs v2.3.0-pinned baseline | Unmodeled write-side effect of v2.8.2 | Stop; re-run M1.9-style all-parti-KV-to-memory ablation to isolate first |
| E2: CPU delta ≥ ~0.05 cores at P=10k, latency flat | Pull floor is a real slice of b·N | Ship the PullHeartbeat cap + SCALING.md FetchTimeout=30s guidance for large P; leave defaults |
| E2: CPU delta ≈ 0 | Floor cheaper than estimated | Demote to not-material; document only |
| E4: handoff reads ≈ 2P and removal-guard reads ≈ W² on joins, at user-relevant rebalance frequency | Full-walk and fan-out models correct | Implement in priority order: diff-restricted commit/stabilize (one file), then union-payload ref (schema-additive; only if W≳several hundred is a real target) |
| E4: reads far below model | Memoization already covers it | Downgrade both fixes; correct the §3 model |
| E5: leader GetHeartbeats serial wall-time approaching AuditInterval | Audit pass-shape problem at this RTT×W | Apply the converged safe subset only (opCtx bound + bounded fan-out + optional AuditInterval=2×TTL); watcher-fed cache stays rejected |
| E5: resolver timer/copy or sweepExpired ≥ ~1% of a core | Client hot paths bite earlier than the W≫1000 estimate | Implement event-driven timer arming and/or the sweepExpired deadline gate (both verified contract-preserving, single-file) |
| E5: none of the flagged paths visible | Not-material verdicts hold at ≤100/10k | Keep client items deferred; record profile evidence in the findings doc |
| E6: snapshot tracks 3 ms/1k; no pdflush spike in memory arm | Metacontroller + writeback models stable on 2.14.1 | Keep `meta_compact` untouched (inert below the 8 MB floor); memory consumer state remains the default recommendation |
| E6: snapshot duration inflates or blocking fallback observed | Approaching the ≫10k-consumer regime early (vendor band §2.2 wins) | Escalate; consider compact-threshold tuning; FINDING-A gains priority |
| E8: handoff Gets/s linear in W at fixed P | W-fold sweep duplication confirmed at measured magnitude | Implement leader-gated ticker sweep with follower backstop (SweepAuthority hook); update sweep-cadence simulation oracles |
| Any cell: RSS tracks 90 + 0.793·P and is the user's binding constraint | Structural P-linear cost, not overhead | No library fix inside the Dynamic contract: recommend fixed-K `partition()` (SCALING.md) or scope FINDING-A — do not "optimize" the durable population |
| E7: source egress ≪ 1 Mbit/s at all cells | Source reconcile not-material confirmed | Document WithLeadershipProbe wiring; defer the gate port |

**Standing constraints on any resulting change:** the four AGENTS.md cross-feature contracts are non-negotiable; reconcilers/polls are the load-bearing recovery for silent KV watchers (Updates() does not close on server restart) — every "skip" must fail open with a forced-full-pass backstop; anything touching `manager/`, `internal/assignment/`, or `internal/durable/` goes through `make pre-pr` (-race unit + integration); new monitor/probe traffic needs a live-cluster concurrency stress test per the epoch-monitor template.

---

## 7. Open questions (merged)

1. Measured at-rest cost of one R1 vs R3 durable pull consumer on nats-server 2.14 (RSS, goroutines, state write cadence) — no primary source quantifies it; E1/E6 ladder answers it for parti's config.
2. How far do the 2.12+ mitigations (async meta snapshots, `meta_compact*`, consumer pause) move the effective 5,000-Raft-group watermark on tuned 2.14 hardware — is 10k R3 durables operationally fine, or a redesign trigger? (E6, plus the §2.2 vendor-band tension.)
3. Delivery-semantics and performance trade-off of collapsing 10k per-partition durables into K≪P multi-filter durables or `partition()` transforms — interacts with the existing FINDING-A collapse-as-new-type decision and the virtual-partition assessment.
4. Prometheus export path for meta-health signals (snapshot duration, meta pending, consumer churn — META_004/005/JETSTREAM_006 equivalents): exporter jsz scraping vs $SYS.REQ surfaces — the exporter-coverage claim was refuted, so this is unverified either way.
5. `_commit_log.<V>` keys are never deleted (verification side-find) — long-run assignment-bucket growth needs its own disposition.

## 8. External sources (all 3-0 verified unless noted)

- https://docs.nats.io/running-a-nats-service/nats_admin/profiling — server pprof (`prof_port`, CLI profile path)
- https://docs.nats.io/running-a-nats-service/nats_admin/monitoring + monitoring_jetstream — /jsz and endpoint semantics
- https://www.synadia.com/insights/checks/nats-meta-snapshot-slow (META_004) · nats-meta-pending-high (META_005) · nats-consumer-churn-high (JETSTREAM_006) — thresholds and churn arithmetic (single vendor family; see §2.2 caveats)
- https://www.synadia.com/blog/jetstream-raft-per-stream-scaling — Raft-group-per-asset architecture
- https://github.com/nats-io/nats-server/issues/4987 — KV scan write-pattern sensitivity (historical, fixed pre-2.11)
- https://go.dev/doc/diagnostics · https://pkg.go.dev/net/http/pprof — client profiling safety, block/mutex opt-in
- https://pkg.go.dev/github.com/nats-io/nats.go/jetstream (+ v1.52.0 source check) — Fetch vs Consume cost model, 500-message default buffer
- Refuted (do not cite): prometheus-nats-exporter raft-filter coverage (0-3); linear-3× R3 meta footprint (0-3); go.dev random-replica sampling recommendation (0-3); prof_port config-file-only enablement (1-2)
