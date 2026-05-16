# Tier 1 Findings — Fast Evidence (M1.0–M1.3)

> **Status: complete.** Tier 1 ran 36 of the §M1 matrix's 190 runs
> (4 cells × 3 N × 3 reps, compressed cadence 120 s warmup + 120 s
> capture) on 2026-05-17, after Tier 0 (chain validation) and
> Tier 0.5 (absolute calibration) both passed. The campaign answers
> H1 — *"is the per-partition IOPS slope coming from two-phase
> handoff / the SweepInterval ticker?"* — and rules out a sweep-only
> mitigation. Inputs: `results/tier1-20260517-032602/analysis/`.

## TL;DR

**H1 is falsified.** Two-phase handoff is not the source of the
per-partition slope, and lengthening `SweepInterval` to 5 min does
not move the slope. The dominant per-partition cost is on the NATS
server side, not in parti's library code. Tier 2 (H2.A/B/C
ablations) is the next sensible step.

| Hypothesis | Verdict | Evidence |
|---|---|---|
| **H1.A** — `SweepInterval = 5 min` reduces per-partition slope | **falsified** | Δ(M1.3 − M1.2) block_write_iops slope = `-0.001 ± 0.013` per partition, indistinguishable from zero. |
| **H1.B** — two-phase handoff causes the per-partition slope | **falsified** | Δ(M1.2 − M1.1) block_write_iops slope = `+0.003 ± 0.013` per partition; B-lib (off) and B-prod (on) have indistinguishable slopes. |

## What Tier 1 ran

| Cell | Config | N | Reps | Runs |
|---|---|---|---:|---:|
| M1.0 | No-Parti control (defines MDE) | 500, 1000, 3000 | 3 | 9 |
| M1.1 | B-lib baseline (`EnableTwoPhaseHandoff=false`) | 500, 1000, 3000 | 3 | 9 |
| M1.2 | B-prod baseline (`EnableTwoPhaseHandoff=true`) | 500, 1000, 3000 | 3 | 9 |
| M1.3 | H1.A — `SweepInterval=5min` | 500, 1000, 3000 | 3 | 9 |

All 36 runs succeeded (manifest status `ok`, aggregator clean). Compressed
cadence chosen because Tier 0 reproducibility check showed CV = 0.0061 across
reps at 60 s capture — 120 s gives 2× safety margin and brought the campaign
from 9.3 h (full cadence) to 4.5 h (compressed).

## Slope table

Read directly from `analysis/slope_table.csv`; selected columns:

| Cell | column | β₁ (per partition) | β₀ (intercept) | R² | n | verdict |
|---|---|---:|---:|---:|---:|---|
| M1.0 | read_rpc_ops | 0.0 | 0.0 | n/a | 9 | below_mde |
| M1.0 | write_mutation_ops | 0.0 | 0.0 | n/a | 9 | below_mde |
| M1.0 | block_read_iops | 0.0 | 0.0 | n/a | 9 | below_mde |
| M1.0 | block_write_iops | **3.21e-06** ± 4.45e-06 | 0.182 | 0.07 | 9 | below_mde |
| M1.1 | read_rpc_ops | −5.99e-06 ± 1.6e-05 | 0.618 | 0.02 | 9 | above_mde |
| M1.1 | write_mutation_ops | +1.43e-06 ± 9.7e-07 | 0.903 | 0.24 | 9 | above_mde |
| M1.1 | block_read_iops | −2.31e-04 ± 5.3e-04 | 0.887 | 0.03 | 9 | above_mde |
| M1.1 | block_write_iops | **+0.0762** ± 9.0e-03 | 115.9 | 0.91 | 9 | above_mde |
| M1.2 | read_rpc_ops | +2.07e-05 ± 3.9e-06 | 0.618 | 0.80 | 9 | above_mde |
| M1.2 | write_mutation_ops | −3.99e-07 ± 1.1e-06 | 0.904 | 0.02 | 9 | above_mde |
| M1.2 | block_read_iops | +9.78e-04 ± 6.6e-04 | −0.706 | 0.24 | 9 | above_mde |
| M1.2 | block_write_iops | **+0.0797** ± 8.8e-03 | 113.9 | 0.92 | 9 | above_mde |
| M1.3 | read_rpc_ops | +3.54e-06 ± 8.7e-06 | 0.596 | 0.02 | 9 | above_mde |
| M1.3 | write_mutation_ops | +6.46e-07 ± 8.4e-07 | 0.904 | 0.08 | 9 | above_mde |
| M1.3 | block_read_iops | −2.65e-04 ± 6.0e-04 | 1.018 | 0.03 | 9 | above_mde |
| M1.3 | block_write_iops | **+0.0787** ± 9.1e-03 | 114.5 | 0.91 | 9 | above_mde |

**MDE** (from M1.0 no-Parti control):

| column | SE_no_parti | MDE_slope |
|---|---:|---:|
| read_rpc_ops | 0.0 | 0.0 |
| write_mutation_ops | 0.0 | 0.0 |
| block_read_iops | 0.0 | 0.0 |
| block_write_iops | 4.45e-06 | **1.05e-05** |

## Where the IOPS comes from

The dominant column is **`block_write_iops`** with R² ≈ 0.91 across
all three parti cells (M1.1–M1.3). The other three budget columns
either sit at zero (control) or have slopes orders of magnitude
smaller than block_write_iops — they are not the load-bearing
quantity for this investigation.

Decompose `block_write_iops` at N = 1000 per the fit:

| Cell | β₀ (constant) | β₁ × N (slope·N) | total | slope share |
|---|---:|---:|---:|---:|
| M1.0 (no parti) | 0.18 | 0.003 | 0.18 | 0 % |
| M1.1 (parti, two-phase off) | 115.9 | 76.2 | **192.1** | 40 % |
| M1.2 (parti, two-phase on) | 113.9 | 79.7 | **193.6** | 41 % |
| M1.3 (parti, sweep=5min) | 114.5 | 78.7 | **193.2** | 41 % |

Two observations:

1. **A large constant ~114 IOPS appears the moment parti is connected**,
   regardless of N or configuration. This is parti's idle bookkeeping
   on JetStream — heartbeat KV, stable-ID KV, leader-election KV,
   handoff KV — all of which generate base write traffic the moment a
   manager attaches. None of these scale with N.
2. **A per-partition slope ~0.08 IOPS/partition rides on top**,
   essentially identical across M1.1/M1.2/M1.3. At N = 1000 this
   slope contributes ~80 IOPS — about 40 % of total. At N = 3000
   it would dominate (~240 IOPS / 354 total = ~68 %).

The slope is unchanged whether two-phase handoff is on or off, and
unchanged when SweepInterval moves from 30 s default to 5 min. The
H1 narrative — "the sweep ticker walks every partition periodically
and the cost is proportional to N" — is therefore not what we are
seeing.

## What's driving the slope, then?

Per the four-column budget logic: if the slope shows up in
`block_write_iops` but not in `read_rpc_ops` or `write_mutation_ops`,
parti is **not issuing more RPCs at higher N** — meaning the slope is
generated by something downstream of parti's calls into NATS. Most
plausible source given that information:

- **JetStream's per-consumer state writes.** Each parti partition
  maps to a server-side JetStream subscription. As N grows, the NATS
  server keeps more consumer state, raft progress markers, ack
  records, etc., on its own. Those writes don't appear in parti's
  RPC counters (they're internal to NATS) but they hit
  `/proc/diskstats` and `cgroup io.stat` exactly as we measured.

This is what M1.5 / M1.6 / M1.7 (H2.A/B/C in the §M1 matrix) test
directly:

- **M1.5** — `FetchTimeout = 30 s` (idle-pull frequency change)
- **M1.6** — `consumer.Queue` instead of `Dynamic` (consumer kind)
- **M1.7** — data stream `Storage = memory` (move state off disk)

If H2 is right, one of those three should collapse the slope
visibly (β₁ for block_write_iops drops by ≥ 5× toward MDE) without
needing any change to parti itself.

## Cross-check: cgroup vs iostat divergence

Per Tier 0.5's empirical calibration band (cgroup ≤ iostat × 4 on this
host under known workloads), all 36 Tier 1 runs landed inside the
documented band. The harness's typical cgroup-vs-iostat ratio during
M1.2 windows tracks JetStream's bio coalescing factor (cg often
> ios) — this is not an instrument bug; see RUNBOOK §3 Tier 0.5.

## Recommendation

1. **Do not ship a "raise SweepInterval to 5 min" mitigation.**
   Tier 1 conclusively shows no slope reduction; users would pay the
   coordination latency cost for zero IOPS benefit.
2. **Run Tier 2 (H2.A/B/C ablations).** ~7 h additional wall-clock at
   compressed cadence; covers `consumer.Queue`, `FetchTimeout=30s`,
   and data-stream memory storage. The slope must be attributable to
   one of those three knobs before a defensible mitigation can be
   recommended.
3. **Defer the full 190-run §M1 matrix.** Tier 1 + Tier 2 should be
   enough evidence to either ship a mitigation or escalate to deeper
   investigation. The matrix's exhaustive four-column attribution at
   n = 5 is overkill until we have a real candidate to attribute
   precisely.

## Artifacts

- `results/tier1-20260517-032602/analysis/slope_table.csv` (16 rows)
- `results/tier1-20260517-032602/analysis/mde.csv` (4 rows)
- `results/tier1-20260517-032602/analysis/tukey_outliers.tsv`
- `results/tier1-20260517-032602/campaign-manifest.yaml`
- `results/tier1-20260517-032602/run-NNN-MX.Y-N{500,1000,3000}-rep{1,2,3}/aggregated.csv` × 36

Parti commit: `ef8402e` (worktree branch `worktree-iops-investigation`).
NATS image: `nats:2.12.6`. Rig: docker compose R3 profile.
Host: see `manifest.yaml` per run.
