# Dynamic Partition-Consumer Performance — Production Config (memory + R=3)

*Part of the [perf-measurement study](README.md). Companion: the expensive
[baseline (file + R=5)](02-findings-baseline-file-r5.md).*

Status: **complete N-sweep (12 cells × 3 reps, 36 runs)** — 2026-06-05.

> **Configuration: PRODUCTION / recommended (`MemoryStorage=true`, `Replicas=3`).**
> Per-partition dynamic consumers keep delivery/ack state in **memory**,
> replicated as a **3-way raft group** (tolerate-1-failure HA). The message
> stream is **file-backed, RF=5** (durable messages, 5-node cluster). This is
> parti's documented "recommended default" (IOPS decision tree Q2 → M2.A). It
> is the cheap, HA-preserving config — the opposite of the **expensive baseline**
> (file consumer state + R=5; see [`02-findings-baseline-file-r5.md`](02-findings-baseline-file-r5.md),
> N=2000 round, and the prior IOPS-attribution study). The `Replicas=1` arm
> (max reduction, no consumer-HA) was not run.

## Configuration

| Param | Value |
|---|---|
| Consumer | `consumer.Dynamic`, **`WithConsumerMemoryStorage(true)`**, **`WithConsumerReplicas(3)`** |
| Partitions `N` | 1000, 2000, 3000, 5000 |
| Workers `M` | `N/50` → 20, 40, 60, 100 |
| Per-worker rate `k` | 1, 2, 4 → aggregate `X = k·M` = 20…400 msg/s |
| Payload | ~256 B |
| Stream | data + KV **file** storage, **RF=5**, 5-node cluster, NATS v2.12.6 |
| Box | AMD Ryzen 9 9950X3D (NATS pinned cores 0-7,16-23; harness 8-15,24-31) |
| Window | **60 s warmup + 60 s capture**, **3 reps**, `make reset` (disk wiped) between every rep |

## Headline: it scales to 5000 — cleanly

All four cost dimensions are **linear in N** with gentle slopes, latency is
**flat (N-independent)**, and delivery is **lossless** at every cell.

### Per-cell measurements (cluster sums over 5 nodes; latency pooled over 3 reps)

| N | X | P50 | P95 | P99 | P99.9 | IOPS | CPU (cores) | RSS (MiB) |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1000 | 20 | 0.85 | 1.35 | 1.45 | n/a* | 27 | 0.24 | 836 |
| 1000 | 80 | 0.87 | 1.31 | 1.39 | 1.50 | 28 | 0.29 | 908 |
| 2000 | 40 | 0.82 | 1.30 | 1.37 | n/a* | 62 | 0.44 | 1613 |
| 2000 | 160 | 0.79 | 1.27 | 1.34 | 1.62 | 65 | 0.52 | 1728 |
| 3000 | 60 | 0.77 | 1.25 | 1.34 | 2.00 | 97 | 0.63 | 2367 |
| 3000 | 240 | 0.78 | 1.26 | 1.32 | 3.00 | 103 | 0.75 | 2508 |
| 5000 | 100 | 0.78 | 1.28 | 1.35 | 2.76 | 142 | 1.02 | 3941 |
| 5000 | 400 | 0.79 | 1.26 | 1.33 | 4.61 | 148 | 1.21 | 4103 |

Latency in ms. *P99.9 "n/a" = pooled sample count below the validity gate
(`n·(1−p) ≥ 10`) at the lowest-rate cells; it pools fine at higher X.

### Fitted cost model (`cost ≈ a + b·N + c·X`, OLS over 12 cells)

| Metric | a | b (per partition) | c (per msg/s) | R² |
|---|---:|---:|---:|---:|
| Write IOPS (cluster) | 3.6 | 0.0280 | 0.0237 | **0.983** |
| CPU cores (cluster) | 0.059 | 0.000176 | 0.00066 | **0.9995** |
| RSS bytes (cluster) | 90 MiB | 0.793 MiB | 0.673 MiB | **0.9998** |
| Latency P50/P90/P95/P99 | ~flat | ≈0 (noise) | ≈0 | 0.5–0.65 |
| Latency P99.9 | 0.48 ms | 318 ns | 5.9 µs | 0.94 |

The IOPS/CPU/RSS fits are near-perfect linear (R² ≥ 0.98). The latency
percentiles P50–P99 have b ≈ 0 and low R² **because there is no trend to fit** —
latency does not scale with N.

### Out-of-sample validation (the real scaling test)

In-sample R² near 1.0 only proves the plane passes through its own training
points. To test *extrapolation* — the claim that actually matters — the model
was refit on **N ∈ {1000,2000,3000} only** (9 cells) and used to predict the
held-out **N=5000** cells:

| Metric | predicted @ N=5000 | measured | error |
|---|---:|---:|---:|
| CPU cores | 1.01–1.22 | 1.02–1.21 | **±2.3 %** |
| RSS | 3904–4154 MiB | 3941–4103 MiB | **±1.2 %** |
| Write IOPS | 168–177 | 142–148 | **+18–19 %** (over-predict) |

**CPU and RSS extrapolate cleanly** — the linear model from low N lands the high
end within noise. **IOPS is mildly sub-linear**: the low-N trend over-predicts
N=5000 by ~18 %, i.e. IOPS grows *slower* than linear as N rises (raft-write
amortization at higher consumer counts). This is favorable for scaling, but it
means the shipped 12-point IOPS coefficient is best read as a **conservative
upper bound** when extrapolating beyond N=5000 — real IOPS will be lower. CPU
and RSS coefficients can be trusted for extrapolation.

## Interpretation

**Cost is dominated by the per-consumer term (`b·N`), nearly flat in
throughput.** IOPS goes 27 → 148 from N=1000 → 5000 (~0.028 IOPS/partition),
but barely moves with X (N=5000: 142 → 148 as X goes 100 → 400). With consumer
state in memory there is no per-ack state-file write; the residual IOPS is the
**R=3 raft log of consumer state + the file data-stream message log**. Per node
at N=5000 that's **~30 IOPS** — trivial on any disk class.

**Latency is flat and tight.** P50 ~0.8 ms, P95 ~1.3 ms, P99 ~1.34 ms at every
N, lossless, with near-zero rep-to-rep variance. Memory consumer state removes
the per-ack disk-write stalls that produced the file baseline's tail. The only
N-dependent latency is **P99.9**, which grows 1.5 → 4.6 ms with N×k — this is
the **in-process-harness tail** (design §4.1): 100 worker managers + the
producer share one Go runtime at N=5000, so a GC/scheduler pause perturbs the
extreme tail. P50–P95 are production-representative; **P99.9 is an in-process
upper bound, not a parti property** (a real fleet of separate processes would
not share that runtime).

**RSS is the dimension to watch, but it's far from a wall.** ~0.79 MiB/partition
→ ~4.1 GiB cluster (~820 MiB/node) at N=5000. Linear, predictable.

## memory+R=3 vs the expensive file+R=5 baseline

At **N=2000** (the one cell measured in both configs):

| Metric | file + R5 (baseline) | memory + R3 (this) | improvement |
|---|---:|---:|---:|
| Cluster IOPS | 5,351 | ~62 | **~86× lower** |
| Cluster RSS | 6,008 MiB | ~1,660 MiB | ~3.6× lower |
| Cluster CPU | 0.98 cores | ~0.46 cores | ~2× lower |
| Latency P99 | 28.8 ms | 1.36 ms | ~21× lower |
| Latency P99.9 | 582 ms | 1.47 ms | ~400× lower |

The IOPS collapse is the headline: removing the per-consumer state file (the
~72–81 % dominator from the attribution study) and dropping R=5 → R=3 turns the
disk-IOPS pressure from "would exceed GCP pd-balanced per node" into a non-issue.

## Does one-consumer-per-partition scale to 5000? (production config)

**Yes — comfortably.** Lossless delivery, flat ~1.3 ms P95/P99, and all
resource costs linear (IOPS sub-linear) in N with small slopes. Per node at
N=5000: ~30 IOPS, ~0.21 cores, ~820 MiB RSS. Extrapolating the model to
**N=10,000** (X=200): **≤**~290 cluster IOPS (~58/node — upper bound, IOPS is
sub-linear so real is lower), ~2.0 cluster cores (~0.4/node), ~8.1 GiB cluster
RSS (~1.6 GiB/node). Still no wall; **RSS is the first thing you'd budget for**
(the only cost growing materially with N).

## Caveats / validity
- **Single consumer config.** This is memory + R=3 only. The `R=1` arm (which
  the prior study showed removes the raft-replication residual and goes flat in
  N) was not run — so "is R=3's raft term a problem at scale?" is answered
  directly here: **no, it's ~0.028 IOPS/partition, negligible.**
- **In-process latency tail** (§4.1): P50–P95 sound; P99.9 is an in-process
  upper bound that compounds with N. The §8.2 out-of-process cell (not run)
  would confirm the true tail.
- **Do NOT extrapolate the latency coefficients.** The model.json latency fits
  have negative b (fitted noise around a flat ~1.3 ms), so the estimator will
  predict latency *decreasing* with N — nonsense. Treat latency as **flat
  ~1.3 ms P95/P99 independent of N**; ignore the latency b·N terms.
- **`/jsz` monitoring endpoint does not scale to 5000 consumers.** With
  `consumers=true` the endpoint serializes per-consumer detail and times out
  under 5000-consumer load, so the jsz sidecar aborted on 8 of the N=5000 reps.
  This is a **monitoring-endpoint serialization cost — a distinct mechanism from
  raft snapshot duration**, so it is *not* evidence about the metacontroller
  snapshot question (which remains open, pending the `meta_compact` sweep). It
  affects **only** the snapshot sidecar — latency and cgroup IOPS/CPU/RSS are
  intact for all 36 cells. The meta-sweep needs a hardened jsz capture (retry +
  drop `consumers=true`) first.
- All NATS figures are **cluster sums across 5 nodes**; per-node ≈ /5.

## Raw data & model
- Fitted model (committed): [`model-production-mem-r3.json`](model-production-mem-r3.json)
  (= `test/perf-measurement/results/armb/model-armb.json`).
- Raw per-cell captures (gitignored):
  `test/perf-measurement/results/armb/armb-N{1000,2000,3000,5000}-k{1,2,4}/rep{1,2,3}/`
  (`latency.json`, `cgroup_io.raw`, `cgroup_cpumem.raw`, `manifest.yaml`).
- Reproduce metrics: `cmd/fitmodel/fitmodel --results results/armb --dump`.
- Predict: `cmd/estimator/estimator --model docs/plans/perf-measurement/model-production-mem-r3.json --n <N> --k <k> --storage file`.
