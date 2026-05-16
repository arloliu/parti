# M1.9 Focused Test — Memory KV Buckets

> **Status: complete.** 6 runs of M1.9 (B-prod baseline +
> `--kv-storage=memory`) at N∈{1000, 3000} × 3 reps, compressed
> cadence 120 s warmup + 120 s capture. Direct comparison with the
> M1.2 baseline runs from Tier 1 at identical N.

## TL;DR

**Memory KV is not a mitigation.** Moving all four parti KV buckets
(heartbeat, stable-ID, election, assignment/handoff) from `file` to
`memory` storage does **not** meaningfully reduce IOPS — neither the
constant baseline nor the per-partition slope changes. The
hypothesis that the ~200 IOPS constant comes from parti's KV
bookkeeping is **falsified**.

## Cross-cell comparison

Capture-window mean `iops_write` (host-summed across 3 nats containers,
mean over t ∈ [120 s, 240 s], 3 reps averaged):

| Cell | N=1000 | N=3000 | Δ vs M1.2 at N=1000 | Δ vs M1.2 at N=3000 |
|---|---:|---:|---:|---:|
| **M1.2** (file KV)     | 216 | 450 | — | — |
| **M1.9** (memory KV)   | 211 | 446 | −5 (~2 %) | −4 (~1 %) |

Slope between the two N points:

| Cell | β₁ (IOPS / partition) |
|---|---:|
| M1.2 | (450 − 216) / 2000 = **0.117** |
| M1.9 | (446 − 211) / 2000 = **0.118** |

Within run-to-run reproducibility (≈ ±5 IOPS at N=1000, ±5 at N=3000),
M1.9 and M1.2 are statistically indistinguishable.

## The late-window spike survives memory KV

The deterministic spike at t ≈ 220 s that inflates the N=3000 mean
is **unchanged** by memory KV:

| Cell | t=220s IOPS (3-rep avg) |
|---|---:|
| M1.2 | 1745 |
| M1.9 | 1710 |

That rules out "the spike is a KV writeback coalesce" — the spike
must come from somewhere else (data stream, JetStream meta-cluster
snapshot, or NATS-internal periodic flush).

## What this rules out

Combined with Tier 1, the per-partition slope is **demonstrably
not** explained by:

- two-phase handoff on/off (Tier 1: M1.1 vs M1.2)
- SweepInterval (Tier 1: M1.2 vs M1.3)
- KV bucket storage class (this test: M1.2 vs M1.9)

Three independent knobs that the original investigation pre-registered
as plausible sources do not move the needle. The cost must be on the
NATS-server side, attached to per-consumer / per-stream state that
parti does not directly write through KV.

## Where to look next

The remaining matrix cells that directly test server-side per-consumer
state:

- **M1.7** — data stream `Storage = memory`. Moves the user data
  stream off disk. If consumer ack/redelivery state is the slope
  driver, this collapses both the constant and the slope.
- **M1.6** — `consumer.Queue` instead of `Dynamic`. Queue consumers
  have a different ack-tracking footprint per server.
- **M1.5** — `FetchTimeout = 30 s` (vs default). Reduces idle-pull
  frequency. If the slope is driven by N×fetch-rate, this drops it
  proportionally.

**Recommendation:** run M1.7 first. It is the strongest knob (moves
the largest disk-state population off disk) and is the highest
prior given that KV memory was ruled out — the data stream is the
only other place where per-partition state writes can land.

## Aside: capture-window cadence verdict

Looking at the time series:

- IOPS at t=0-30 s: low (parti startup, workers reaching Stable).
- IOPS at t=60-120 s: ~70 % of steady state (N=3000 still climbing).
- IOPS at t=120-200 s: steady (~95 % of long-run mean).
- IOPS at t=200-240 s: includes the recurring ~10× spike.

For investigation cadence:

- **Warmup**: 120 s is borderline-OK for N=3000. Could compress to
  60 s for N ≤ 1000 with no loss.
- **Capture**: 120 s is *too short* for robust means at high N
  because of the t ≈ 220 s periodic spike — depending on phase,
  the capture window may or may not include it. The cleanest fix
  is post-hoc steady-window analysis: discard t ∈ [200, capture_end].

## Artifacts

- `results/m19-20260517-143438/run-NNN-M1.9-N{1000,3000}-rep{1,2,3}/aggregated.csv`
- Comparison values computed manually here (not via analyze.py) —
  analyze.py averages across the whole run including warmup, which
  dilutes high-N means; capture-window-only is the honest figure.
