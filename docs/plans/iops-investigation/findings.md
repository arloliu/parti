# NATS IOPS Investigation — Findings

> **Status:** complete. Operator-facing summary. Measured on a local
> 3-node R=3 NATS rig (`nats:2.12.6`, ext4 host). Raw data and
> reproducibility scripts under `test/iops-investigation/results/`.

**Problem.** An operator reported per-partition `block_write_iops`
scaling with parti's partition count on a NATS JetStream cluster:
~278 IOPS cluster-summed at N=1000 partitions, ~440 at N=2000.
The question for this investigation: which subsystem drives that
cost, can it be reduced, and what's the durability tradeoff of
each mitigation?

**TL;DR.** The dominant cost is **per-consumer JetStream state-file
writes** (~80 % of the per-N cost) plus **raft replication of that
state** to R=3 peers (~9–28 %, growing with N). Parti's own
coordination protocol contributes ~1 %; the JetStream message log
itself contributes essentially nothing for this idle-publish
workload. The single most effective fix is setting
`jetstream.ConsumerConfig.MemoryStorage = true` *and* `Replicas = 1`
on parti-managed consumers (cell **M2.B**) — **99 % reduction** at
both N=1000 and N=3000, with at-least-once consumer-state redelivery
as the only tradeoff. The published message log stays durable.

**Caveat (read §1 first).** The absolute IOPS reported above are
**not high** for typical cloud SSD PVCs (AWS gp3, GCP pd-balanced,
etc.). Most operators will find the default config is already well
under their disk's sustained-IOPS floor. The mitigation matters
only at very high N, on tightly-provisioned disks, or where the
periodic kernel `pdflush` burst pattern (~1,500–1,800 IOPS for one
second every ~5 s, see §3) is causing latency-tail issues.

---

## 1. Is this actually a problem for you?

**Probably not, for a typical k8s + cloud-SSD deployment.** The
baseline cost we measured at the operator's reported scale is small
in absolute terms; the per-partition slope only matters once N is
high enough or the disk is tight enough. Calibrate before you tune.

**What we measured (default config M1.2, R=3, file-backed):**

| Partitions (N) | block_write_iops cluster-summed | Per-NATS-pod (R=3) |
|---:|---:|---:|
| 1,000 | 216 | ~72 |
| 3,000 | 450 | ~150 |
| 10,000 (extrapolated, slope 0.117) | ~1,400 | ~470 |
| 100,000 (extrapolated) | ~11,900 | ~3,970 |

**What cloud SSD PVCs deliver per 100 GB:**

| Disk | Sustained IOPS for 100 GB | Headroom at N=3000 (150/pod) |
|---|---:|---|
| GCP pd-balanced (default) | 600 | 4× |
| AWS gp3 (default modern) | 3,000 baseline | 20× |
| GCP pd-ssd | 3,000 | 20× |
| AWS io2, GCP pd-extreme | 10,000+ provisioned | 60×+ |
| AWS gp2 (legacy) | 300 + burst | 2× |
| Local NVMe | 50,000+ | 300×+ |

For most deployments **the M1.2 baseline is well below the disk
floor.** Tune only if at least one applies:

1. **N ≥ 10,000 partitions per pod** — the per-partition slope
   pushes per-pod IOPS toward the disk's sustained limit.
2. **Provisioned-IOPS billing** (AWS io2 at \$0.065/IOPS-month, gp3
   above 3,000) at fleet scale — savings stack.
3. **Latency-tail sensitivity.** The default config produces
   periodic ~1,500–1,800 IOPS bursts every ~5 s (kernel `pdflush`
   writeback; explained in §3). The mean is fine; the tail isn't,
   on disks with weak burst behaviour or noisy neighbours.
4. **Tightly-constrained dev/test clusters** (small PVCs, shared
   nodes, no I/O isolation).

If none of those apply, **stop here**. Default config is fine.
The rest of this document is for when one of them does.

---

## 2. The recommended fix (when you need it)

Set both fields on every parti-managed JetStream consumer:

```go
jetstream.ConsumerConfig{
    MemoryStorage: true,
    Replicas:      1,
    // ... existing fields
}
```

Result on the test rig (cell **M2.B**):

| N | Before (M1.2) | After (M2.B) | Reduction |
|---:|---:|---:|---:|
| 1,000 | 216 IOPS | 2.3 IOPS | **−99 %** |
| 3,000 | 450 IOPS | 2.3 IOPS | **−99 %** |

Per-partition slope collapses from **0.117 IOPS/partition** to
**~0**. IOPS becomes flat in N. The remaining 2.3 IOPS is parti's
constant coordination floor (heartbeat / stable-ID / election KV
puts on R=3) and does not scale.

**Trade-off:** consumer delivery/ack offsets are no longer durable.

- `Replicas = 1` removes consumer-state HA → single-node failure of
  the consumer-state holder triggers redelivery from `DeliverPolicy`.
- `MemoryStorage = true` keeps the consumer state in memory →
  coordinated cluster restart triggers the same redelivery.
- **The message log stays on file-backed JetStream at R=3** —
  published messages are still durable.

For any work queue with idempotent handlers (the common parti
pattern), at-least-once redelivery is the expected contract. This
trade is correct.

**If even single-node consumer-state loss is unacceptable:** drop
`Replicas = 1` and ship only `MemoryStorage = true` (cell **M2.A**).
Reduction is 90 % at N=1000 and 72 % at N=3000. Single-node failure
recovers via raft peers; only coordinated cluster restart triggers
redelivery.

**Parti API status:** `MemoryStorage` and `Replicas` are not yet
exposed on `consumer.Dynamic`. The IOPS investigation harness applies
them by intercepting `CreateOrUpdateConsumer` in its NATS wrapper
(see `test/iops-investigation/internal/instrumentedjs/`). Promoting
the fields to public options on `consumer.Dynamic` is a small follow-up
(~1–2 h) — recommended once an operator wants to deploy M2.B.

---

## 3. Where the IOPS actually come from

Per-N cost decomposition, attributed by ablation:

| Source | N=1000 | N=3000 | Evidence |
|---|---:|---:|---|
| Per-consumer state-file disk writes (per-ack, per-housekeeping-tick) | 175 IOPS (81 %) | 322 IOPS (72 %) | M1.2 − M2.A |
| Per-consumer raft log replication to R=3 peers | 19 IOPS (9 %) | 125 IOPS (28 %) | M2.A − M2.B |
| Parti coordination KV puts + NATS server overhead | ~2 IOPS (~1 %) | ~2 IOPS (~1 %) | M2.B (flat in N) |
| JetStream message log fsync | <1 IOPS | <1 IOPS | M2.A ≈ M1.7 (within 1–2 %) |
| **Total** | **216** | **450** | M1.2 baseline |

Reading this:

- The message log itself contributes essentially nothing — this
  workload is idle (no high-rate publish), and the file-vs-memory
  data-stream comparison (M1.7 vs M2.A) lands within noise.
- 81 % of cost is **per-ack writes to the consumer state file**.
  `MemoryStorage = true` removes this.
- 9–28 % is **raft log replication of consumer state** to peers.
  Scales with N because raft groups scale 1:1 with consumers.
  `Replicas = 1` removes this.
- Only the parti-coordination floor (~2 IOPS) is parti-side.
  Everything else lives in JetStream's consumer machinery.

### About the periodic spike

Default config produces a ~10× burst every ~5 s in the per-second
time series (peaks ~1,500–1,800 IOPS at N=3000 between long flat
stretches at ~130 IOPS). This is **Linux kernel `pdflush`** flushing
dirty pages on the `vm.dirty_writeback_centisecs` cadence (500 cs =
5 s, verified on host). It is *not* a JetStream snapshot or raft
event.

Raising `jetstream.sync_interval` (cell **M2.C**: 2 m → 10 m) makes
this **worse**, not better — fewer fsyncs means more dirty pages
between writebacks, so each pdflush cycle has more to flush. The
M2.C mean (399 IOPS at N=3000) looks like a small win, but the
burst pattern means latency-tail-sensitive workloads should avoid
this knob.

Moving consumer state to memory (M2.A / M2.B) removes the dirty
pages entirely; the bursts disappear.

---

## 4. Decision tree

Pick the **first** matching branch.

```
Q1. Can the workload tolerate at-least-once redelivery on
    single-node failure or coordinated cluster restart?
    (True for any work-queue with idempotent handlers.)
    YES → MemoryStorage = true, Replicas = 1.  (cell M2.B)
          99 % reduction. Per-partition cost collapses.
    NO  → continue.

Q2. Can the workload tolerate redelivery ONLY on coordinated
    cluster-wide restart (single-node failure must still preserve
    ack position via raft peers)?
    YES → MemoryStorage = true, Replicas inherited (R≥3).  (M2.A)
          90 % at N=1000, 72 % at N=3000.
    NO  → continue.

Q3. Is the workload's actual disk-IOPS pressure measurably high
    (e.g. N ≥ 10K per pod, or noisy-neighbour latency tail)?
    NO  → stop. Default config is fine.
    YES → architectural change: collapse N pull-consumers into one
          consumer with subject filtering (the consumer.Queue
          pattern, only valid if any worker can ack any message).
          Non-trivial scope; only worth it if M2.A is rejected
          AND the IOPS pressure is real.
```

Most operators land at **Q1 → M2.B** or **Q3 NO → no action**.

---

## 5. Matrix legend

Each cell is `M{phase}.{tag}` and tests parti at default config
**plus exactly one knob changed**.

| Cell | What's changed vs default | Why tested |
|---|---|---|
| M1.0 | parti not running (NATS-only) | Noise floor |
| M1.1 | `EnableTwoPhaseHandoff = false` | H1 — two-phase cost (FALSIFIED) |
| **M1.2** | **default config** | **Baseline. All comparisons land here.** |
| M1.3 | `Handoff.SweepInterval = 5 min` | H1 — sweep cost (FALSIFIED) |
| M1.7 | data stream `Storage = memory` | H2.C — data-stream cost (SUPPORTED; superseded by M2.A) |
| M1.9 | all parti KV buckets `Storage = memory` | H3 — parti KV cost (FALSIFIED — only ~2 % saved) |
| **M2.A** | `consumer.MemoryStorage = true` | Refines M1.7: shows the cost is the consumer state file, not the message log |
| **M2.B** | `consumer.MemoryStorage = true` AND `Replicas = 1` | Refines M2.A: shows the post-M2.A residual is raft replication |
| M2.C | `jetstream.sync_interval = "10m"` (server-config) | Tests the deferred-fsync mitigation. Modest win; bad latency tail. |

---

## 6. Methodology

- **Rig:** docker-compose, 3-node NATS R=3, `nats:2.12.6`, ext4 host.
- **Capture window:** 120 s warmup + 120 s capture per run. Means
  computed from per-second `block_write_iops` samples in the
  capture window only (warmup-dilution would otherwise lower
  high-N means; CV across reps is < 5 %).
- **Replication:** 3 reps per (cell, N). Tier 0 calibration showed
  CV = 0.6 % at N=2000 — 3 reps is over-precise for slope
  comparisons but cheap.
- **Validation gates before testing:** capture-chain integrity,
  wrapper-counter sanity vs first-principles RPC rates,
  reproducibility CV, MDE_slope below the §R5 detection threshold,
  cgroup write-bytes matched a calibrated `dd` payload exactly.
- **Honest caveat:** `analyze.py` averages whole runs (warmup
  included), which dilutes high-N means. All numbers in this
  document use capture-window-only awk on `aggregated.csv`.

Full methodology and validation: `00-attribution-plan.md`,
`RUNBOOK.md`, `tier1-findings.md`, `m17-findings.md`,
`m19-findings.md`.

---

## 7. Raw data

All under `test/iops-investigation/results/`. Campaign seed `42`,
NATS image `nats:2.12.6`, R=3.

- `tier1-20260517-032602/` — M1.0 / M1.1 / M1.2 / M1.3 (36 runs)
- `m17-20260517-153942/` — M1.7 focused (6 runs)
- `m19-20260517-143438/` — M1.9 focused (6 runs)
- `m2-20260517-181718/` — M2.A / M2.B / M2.C (18 runs)
- `tier0-real-20260516-231220/` — capture-chain + reproducibility
- `tier0.5-*` — absolute calibration via `dd`

---

## 8. Scope notes (and quick follow-up experiments)

### Follow-up experiments — consumer Replicas semantics (2026-05-17)

Three quick `nats` CLI experiments against an R=3 docker rig
resolved questions that the planned `WithConsumerMemoryStorage` /
`WithConsumerReplicas` public-API addition needed answers for.
Total rig time: ~5 min, no harness involved.

- **Placement.** With stream R=3 and consumer `Replicas=1`,
  JetStream places the consumer raft as a **single-member group**
  on one node of its choosing — independent of the stream leader.
  For `Replicas ≥ 2`, the raft group spans the chosen subset, again
  independently of stream placement. Consumer leader election runs
  separately from stream leader election; the two can land on
  different nodes. Operators cannot easily predict which node will
  hold a single-replica consumer without inspecting via
  `nats consumer info`.

- **Validation rule.** NATS rejects `consumer.Replicas > stream.Replicas`
  at create time with **error code 10126** ("consumer config
  replica count exceeds parent stream"). This is the *only*
  constraint that fires in practice — the stream-replicas rule is
  stricter than the cluster-size rule, so cluster-size never gets
  a chance to apply. On stream R=3, `Replicas ∈ {0,1,2,3}` accepted;
  `Replicas ∈ {4,5}` rejected with 10126.

- **Live reconfiguration.**
  - **`Replicas` is live-editable in both directions**
    (`nats consumer edit … --replicas=N`). Tested 1 → 3 (group
    expanded, all followers converged "current" within ~3 s; the
    raft group name `C-R1F-ul8wBvXN` was preserved — true expand,
    not recreate) and 3 → 1 (collapsed to single-member group).
  - **`MemoryStorage` is NOT live-editable.** `nats consumer edit`
    exposes no flag for it; changing it requires delete + recreate,
    which drops the consumer's ack / delivery offsets. The parti
    API docstring must call this out.

### Still genuinely out of scope

- Raft snapshot / compaction behaviour at `Replicas=1` over long
  uptime with many state changes. Would require either a multi-hour
  run with jsz-counter inspection or NATS source-code reading.
  Not a blocker for the API plan.
- Exact per-event micro-mechanism of the post-M2.A residual.
  Empirically attributed to consumer raft log activity; the
  specific NATS-internal event (raft heartbeat fsync vs delivery
  tracker tick vs periodic compaction) was not isolated.
- R=5 behaviour (M1.10) and R=5 × consumer-Replicas grid — less
  critical now that M2.B removes the R-sensitive term from the
  per-partition cost.
- N > 3000 — extrapolation in §1 is the 2-point linear fit; not
  measured. M2.B is flat in N, so this matters only for the M1.2
  baseline.
- `FetchTimeout` ablation (M1.5) and `consumer.Queue` ablation
  (M1.6) — both relevant only if M2.A is rejected.

---

## 9. Cross-checks against pre-registered predictions

Predictions were recorded in this section *before* the M2.x runs
(see git history `89322e0`). Logged here so future-you can see what
actually held vs what surprised.

| Prediction (R1) | Measured N=1000 | Verdict |
|---|---:|---|
| M2.A: 80–130 IOPS (40–63 % cut) | **21.6** | Underestimated. Win was 90 %, not 40–63 %. |
| M2.B: 40–80 IOPS (63–82 % cut) | **2.3** | Vastly underestimated. Win was 99 %; per-partition slope is now ~0. |
| M2.C: 170–210 IOPS, win from spike elimination | **168.9** | Mean held; spike interpretation wrong — pdflush bursts persisted and got bigger. |
| `sync_interval = always` smoke: rig honors knob | confirmed via `/jsz` | Held. |

The directional prediction (M2.A and M2.B win big; M2.C is modest)
was right. The magnitude of M2.A/M2.B wins was significantly larger
than R1's conservative bands. The "spike" attribution flipped from
"single JetStream event" (R1) to "kernel pdflush, periodic" (post-runs).
