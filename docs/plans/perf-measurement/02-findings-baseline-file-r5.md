# Dynamic Partition-Consumer Performance — Baseline (file + R=5, expensive)

*Part of the [perf-measurement study](README.md). Companion: the
[production config (memory + R=3)](03-findings-production-mem-r3.md), whose
full N-sweep contrasts against this baseline (~86× lower IOPS).*

Status: **first round (single cell, 3 reps)** — 2026-06-04.

This is the first measured round on the perf-measurement rig: the **expensive
default-durability config** — per-partition consumer state file-backed,
replicas inheriting stream **RF=5**. It is **one configuration**, so it gives
point measurements, **not** a scaling curve or a fitted cost model (the affine
model needs ≥3 distinct `N`). Treat these as the calibrated baseline for that
config; the [production-config N-sweep](03-findings-production-mem-r3.md)
produces the scaling answer.

## Configuration

| Param | Value |
|---|---|
| Partitions `N` | 2000 |
| Workers `M` | 40 (= N/50) |
| Aggregate rate `X` | 100 msg/s (per-worker k = 2.5; per-partition ≈ 0.05 msg/s) |
| Payload | ~256 B |
| Consumer | `consumer.Dynamic` (one durable pull consumer per partition) |
| NATS | 5-node cluster, **RF = 5**, **file** storage, v2.12.6 |
| Box | AMD Ryzen 9 9950X3D (NATS pinned to cores 0-7,16-23; harness to 8-15,24-31) |
| Window | 120 s warmup + **120 s capture**, **3 reps**, `make reset` between reps |
| Samples | 12,000 in-window delivered / rep → **36,000 pooled** |

## Headline results (pooled over 3 reps, 36,000 messages)

| Metric | Value |
|---|---|
| **Delivery ratio** | **1.000** (zero loss — every produced in-window message delivered) |
| **Latency P50** | **1.01 ms** |
| Latency P90 | 8.76 ms |
| Latency P95 | 12.9 ms |
| Latency P99 | 28.8 ms |
| Latency P99.9 | 582 ms ⚠ (see tail caveat) |
| Latency max | 861 ms ⚠ |
| **NATS block-write IOPS** (cluster, 5 nodes) | **5,351** (~1,070 / node) |
| **NATS CPU** (cluster) | **0.98 cores** (~0.20 / node) |
| **NATS RSS** (cluster) | **6,008 MiB** (~1,202 MiB / node) |
| Metacontroller snapshot duration | ~5–6 ms (≈5 snapshots over the window) |
| Meta-raft WAL tail (`pending_size`) | ~2.9 MB peak |
| Live consumers (steady state) | ~2,021 (= 2000 partitions + overhead), **stable, no GC churn** |
| Producer health | not producer-bound; P99 send-skew ~1.0 ms |

## Interpretation

**Delivery is lossless and the median is fast.** P50 ≈ 1 ms: parti's
per-partition pull consumers long-poll, so a message is delivered almost
immediately on arrival rather than waiting for a fetch cycle. The 5 s
`FetchTimeout` is a *ceiling*, not the latency.

**The tail is real but partly a harness artifact.** Per-rep P99 was 20–22 ms in
reps 2 & 3 but **242 ms in rep 1**; P99.9 ranged 65 ms → 716 ms across reps.
This rep-to-rep tail variance is the **in-process-harness caveat** (design
§4.1): all 40 workers + the producer share one Go runtime, so a scheduler/GC
pause perturbs the tail in a way a real fleet of separate worker processes
would not. **The P50–P95 numbers are trustworthy; P99+ should be read as
in-process upper bounds, not the production tail.** Confirming the true tail
needs the §8.2 out-of-process validation cell (not yet run).

**IOPS is dominated by RF=5 × per-consumer state.** 5,351 cluster IOPS for 2000
consumers under only 100 msg/s is far above the prior *idle, RF=3* baseline
(216 @ N=1000 / 450 @ N=3000). Two multipliers vs that baseline: **RF=5**
(every consumer-state + message write replicates to 5 peers, not 3) and the
**live write load** (100 msg/s of persisted messages + acks). Per the settled
attribution study, the per-consumer JetStream state-file write is the dominant
component; RF=5 amplifies it. At ~1,070 IOPS/node this sits below AWS gp3
(3,000) and local NVMe, but **above GCP pd-balanced's 600 sustained floor** —
i.e. on pd-balanced this single config would already be IOPS-constrained per
node.

**CPU and memory are cheap; memory is the one to watch at scale.** NATS uses
<0.2 cores/node — not CPU-bound. RSS is ~1.2 GiB/node for 2000 consumers
(~0.6 MiB/consumer/node). If that scales ~linearly, **5000 consumers ≈ 3 GiB
RSS/node** — the dimension most likely to bound a many-partition deployment
before CPU does.

**Metacontroller snapshot is negligible at this scale.** ~5–6 ms per snapshot
at 2000 consumers, ~60 s cadence, WAL tail ~2.9 MB. This is three orders of
magnitude below the ~1.3 s @ ~10k-consumer incident that motivated the study —
confirming snapshot cost scales with consumer count and is a non-issue until
consumer counts get large. The `meta_compact_size` sweep (at N=5000) is where
that knob's effect becomes measurable.

## Does one-consumer-per-partition scale to 2000? (this cell)

At N=2000 / RF=5 / file / light load: **yes, functionally** — lossless
delivery, ~1 ms median, stable consumer set, trivial CPU, sub-10 ms snapshots.
The **two cost pressures** are (1) **per-node IOPS** (~1,070, RF=5-amplified —
would exceed pd-balanced) and (2) **RSS** (~1.2 GiB/node, the likely scaling
ceiling). Whether it scales to 3000/5000 is **not answered here** — that
requires the N-sweep to see if IOPS/RSS grow linearly or worse.

## Caveats / validity
- **Single cell** ⇒ point measurements only, no fitted model. The "+50×"-class
  questions (does cost grow linearly in N?) need N ∈ {1000,2000,3000,5000}.
- **In-process latency tail** (§4.1): P99+ are in-process upper bounds; P50–P95
  are sound. Rep 1's tail was a runtime-pause outlier.
- All NATS figures are **cluster sums across 5 nodes**; per-node ≈ /5.
- IOPS via per-cgroup `io.stat` (clean regardless of device sharing); CPU =
  Δ`cpu.stat`/Δwall (fraction-of-core); RSS = `memory.current`.
- Metric extraction: `cmd/fitmodel --results results/first --dump` (pooled
  latency via HDR-snapshot merge across reps; IOPS/CPU/RSS meaned over the
  post-warmup window, averaged across reps).

## Raw data
`test/perf-measurement/results/first/dyn-n2000-k2_5-file/rep{1,2,3}/`
(`latency.json`, `cgroup_io.raw`, `cgroup_cpumem.raw`, `jsz.raw`,
`manifest.yaml`).

## Recommended next step
Run the N-sweep (`run-load-matrix.sh`, on-grid k∈{1,2,4}) to turn these point
values into IOPS/RSS/latency-vs-N curves + the fitted `a+b·N+c·X` model — the
actual "does it scale to 5000?" answer — and the `meta_compact_size` sweep at
N=5000 for the metacontroller knob.
