# Dynamic Partition-Consumer Performance Study

How parti's `consumer.Dynamic` (one durable pull consumer per partition) costs
and scales as the partition count **N** grows toward 5000 — measured end-to-end
on a real 5-node NATS JetStream cluster, with a fitted cost model and a
predictor CLI.

**Sister study:** the [IOPS-attribution investigation](../iops-investigation/)
(idle, no publish) decomposed *where* the per-consumer IOPS comes from. This
study adds a live producer + end-to-end latency, and turns that into a scaling
curve + cost model.

---

## Status

| Question (original task) | Status |
|---|---|
| Static IOPS / CPU / RSS cost | ✅ measured + modeled |
| Message-delivery latency percentiles | ✅ measured |
| Cost estimation at 2000 / 3000 / 5000 partitions | ✅ fitted model + estimator (validated to N=10k) |
| Does one-consumer-per-partition scale? | ✅ **yes** — verified to **N=10,000** (lossless, flat ~1.4 ms P99) |
| Metacontroller snapshot cost + `meta_compact` tuning | ✅ snapshot ~30 ms @ 10k (async); **knob inert**, don't tune ([04](04-findings-metacontroller.md)) |

---

## Documents

| # | File | What it is |
|---|---|---|
| 00 | [00-design.md](00-design.md) | Design spec for the measurement rig (hardware, clock, producer, latency/ack, matrix, cost model). |
| 01 | [01-implementation-plan.md](01-implementation-plan.md) | TDD implementation plan (9 phases) for the rig. |
| 02 | [02-findings-baseline-file-r5.md](02-findings-baseline-file-r5.md) | **Baseline report** — the *expensive* default config (consumer state file-backed, R=5). Single cell, N=2000, 3 reps. |
| 03 | [03-findings-production-mem-r3.md](03-findings-production-mem-r3.md) | **Production report** — the *recommended* config (memory consumer state, R=3). Full N-sweep, fitted cost model. |
| 04 | [04-findings-metacontroller.md](04-findings-metacontroller.md) | **Metacontroller report** — snapshot cost + `meta_compact_size` sweep, extends scaling to N=10,000. Knob is inert; snapshot is a cheap async op on NATS ≥2.12. |
| — | [model-production-mem-r3.json](model-production-mem-r3.json) | Fitted `a + b·N + c·X` coefficients for the production config (consumed by the estimator CLI). |

**The two configs measured** (the A/B at the heart of the study):

| | Consumer state | Consumer replicas | Stream | Role |
|---|---|---|---|---|
| **Baseline** (02) | file-backed | R=5 (inherits stream) | file, RF=5 | what you pay out of the box |
| **Production** (03) | **memory** | **R=3** | file, RF=5 | the deploy-target (HA, cheap) |

Both run dynamic consumers on a 5-node cluster (RF=5), ~256 B messages.

---

## Key results at a glance

**Production config (memory + R=3) scales to 5000 cleanly** — lossless, flat
latency, all resource costs linear in N (IOPS sub-linear):

| N | P95 | P99 | IOPS (cluster) | CPU (cores) | RSS (cluster) |
|---:|---:|---:|---:|---:|---:|
| 1000 | 1.3 ms | 1.4 ms | 27 | 0.24 | 836 MiB |
| 2000 | 1.3 ms | 1.36 ms | 62 | 0.46 | 1660 MiB |
| 3000 | 1.26 ms | 1.34 ms | 100 | 0.67 | 2442 MiB |
| 5000 | 1.26 ms | 1.34 ms | 144 | 1.06 | 4005 MiB |

Per node at N=5000: **~30 IOPS, ~0.2 cores, ~820 MiB**. Latency is
**N-independent**.

**Production vs the expensive baseline (N=2000):**

| Metric | baseline (file+R5) | production (mem+R3) | improvement |
|---|---:|---:|---:|
| Cluster IOPS | 5,351 | ~62 | **~86×** |
| Cluster RSS | 6,008 MiB | ~1,660 MiB | ~3.6× |
| Latency P99 | 28.8 ms | 1.36 ms | ~21× |
| Latency P99.9 | 582 ms | 1.47 ms | ~400× |

Removing the per-ack consumer state-file write (memory storage) and dropping
R=5 → R=3 turns disk IOPS from a per-node bottleneck into a non-issue.

---

## Using the cost model

Predict cost/latency for a deployment (production config):

```
cd test/iops-investigation
cmd/estimator/estimator \
  --model ../../docs/plans/perf-measurement/model-production-mem-r3.json \
  --n 5000 --k 2 --storage file
```

The model is `cost ≈ a + b·N + c·X` (X = aggregate msg/s = k·N/50). Trust the
IOPS/CPU/RSS coefficients (R² ≥ 0.98); **do not** extrapolate the latency
coefficients (they are noise around a flat ~1.3 ms — see report 03). IOPS is
mildly sub-linear, so its extrapolation beyond N=5000 is a conservative upper
bound.

---

## Open items
- **R=1 arm** — declined: the data shows R=3's raft term is ~0.028 IOPS/partition
  (negligible), so R=1 buys no meaningful IOPS at the cost of consumer-state HA.
- **Out-of-process latency cell** (design §8.2) — not run; would confirm the
  true P99.9 tail (the in-process harness inflates it).
- **The 1.x-sec metacontroller regime (≫10k consumers)** — not reached; the knob
  stays inert and snapshots stay cheap (~30 ms) through N=10,000. If operating at
  ~10⁵ consumers, measure there directly ([04](04-findings-metacontroller.md)).

---

## Rig
Code: [`test/iops-investigation/`](../../../test/iops-investigation/) (separate
Go module). Runners: `scripts/run-armb-matrix.sh` (production config),
`scripts/run-load-matrix.sh` (full baseline matrix). Raw results land under
`results/` (gitignored). Box: AMD Ryzen 9 9950X3D, NATS pinned to cores
0-7,16-23; harness to 8-15,24-31.
