# M1.7 Focused Test — Data Stream Memory Storage

> **Status: complete.** 6 runs of M1.7 (B-prod baseline +
> `--data-storage=memory`) at N∈{1000, 3000} × 3 reps, compressed
> cadence 120 s warmup + 120 s capture. Direct comparison with M1.2
> (Tier 1) and M1.9 (memory KV) at identical N.

## TL;DR

**The per-partition IOPS cost is on the data stream, not parti.**
Setting the user data stream to `Storage = memory` collapses
`block_write_iops` by **90 % at N=1000** and **72 % at N=3000**.
The mysterious late-window spike at t ≈ 200–220 s **disappears
completely** under memory data storage. The §H2.C hypothesis is
supported: per-partition pull-consumer state on the data stream is
the dominant slope source.

## Cross-cell comparison (capture-window mean, t ∈ [120, 240] s)

| Cell | N=1000 | N=3000 | Δ vs M1.2 (N=1000) | Δ vs M1.2 (N=3000) |
|---|---:|---:|---:|---:|
| **M1.2** (file KV, file data)     | 216 ± 4  | 450 ± 3  | —          | —          |
| **M1.9** (memory KV, file data)   | 211 ± 2  | 446 ± 5  | −2 % (noise) | −1 % (noise) |
| **M1.7** (file KV, memory data)   | **22 ± 0.2** | **128 ± 0.5** | **−90 %** | **−72 %** |

Reproducibility for M1.7 is exceptionally tight (stdev < 1 IOPS),
suggesting the residual IOPS in M1.7 is the *deterministic* parti
coordination floor — not noise.

## Slope between N=1000 and N=3000

| Cell | β₁ (IOPS / partition) |
|---|---:|
| M1.2 | (450 − 216) / 2000 = **0.117** |
| M1.9 | (446 − 211) / 2000 = **0.118** |
| **M1.7** | (128 − 22) / 2000 = **0.053** |

**Memory data storage cuts the slope by 55 %.** What remains in M1.7
(slope ≈ 0.053) must be parti's own per-partition work — that
matches the small but non-zero per-partition read-RPC slope we saw
in Tier 1 (parti scans, election KV writes per partition).

The N=1000 absolute value of 22 IOPS is essentially parti's
"warming the cluster" baseline; the slope from 22 → 128 is the
per-partition coordination cost in parti's library code.

## The late-window spike disappears

| Cell | iops_write at t = 220 s (3-rep avg, N=3000) |
|---|---:|
| M1.2 | 1745 |
| M1.9 | 1710 |
| **M1.7** | **131** |

Recall: this spike fires at the same wall-clock offset across reps,
scales with N, and was unchanged by memory KV. It is now gone. The
spike was an event on the data stream — likely a JetStream
per-stream snapshot, page-cache writeback for the data stream's
filestore, or consumer state checkpoint.

This is consistent with the slope finding: the slope source AND the
periodic burst are both attributable to data-stream-side state on
disk.

## What this means for users

The headline number — **278 IOPS at N=1000, 440 at N=2000** in the
user's report — is consistent with the cost structure we have now
identified:

| Source | Approx contribution at N=1000 |
|---|---:|
| Data stream consumer state + page-cache flush + raft commits | ~190 IOPS (87 %) |
| Parti coordination (heartbeat / stable-ID / election / assignment KVs) | ~22 IOPS (10 %) |
| NATS server overhead (meta-cluster, log etc.) | ~3 IOPS (rounded) |

The IOPS the user sees is **almost entirely the JetStream data
stream**, not parti's coordination protocol.

## Mitigations the operator can apply

1. **If message durability across restarts is not required:** set
   the data stream's `Storage = memory`. This is the most direct
   90 % win. With R≥3 the cluster still survives single-node
   restarts (peers carry state); only a coordinated cluster-wide
   restart loses messages.
2. **If durability is required:** the cost is fundamentally in
   JetStream's per-consumer state machinery. Knobs to tune
   *server-side* (none are parti's):
   - **`MaxAckPending`** — caps how much per-consumer redelivery
     state is held. Default is conservatively high; a tighter
     cap reduces ack-tracking footprint.
   - **`AckWait`** — longer AckWait means fewer redelivery
     re-arms per partition per unit time.
   - **Consumer kind** — M1.6 (`consumer.Queue`) tests whether a
     queue group has a smaller per-server footprint than N
     individual `Dynamic` consumers (untested in this pass).
   - **Server tuning** — `max_pending`, `max_outstanding_ack`,
     and the JetStream snapshot interval, set in the NATS
     server config.
3. **Parti-side ideas worth exploring** (separate effort):
   - Whether parti could use a **single consumer with subject
     filtering** instead of N individual partition consumers.
     This would collapse N copies of consumer state into one.
     This is a non-trivial design change but is exactly where
     the 90 % cost lives.

## What's *not* supported by this evidence

- "Lowering SweepInterval / disabling two-phase handoff helps" —
  Tier 1 falsified this; the slopes were identical.
- "Parti's KV bookkeeping is the IOPS source" — M1.9 falsified
  this; memory KV is within 1 % of file KV.
- "The slope is a 30 s ticker" — the slope is uniform across the
  capture window (no periodic structure in M1.7), only the data
  stream had the spike.

## Cadence note

M1.7's reproducibility (stdev < 1 IOPS at fixed N) was much
tighter than M1.2's (stdev ~3-5 IOPS) — because the disk-bound
spike in M1.2 was the only source of intra-cell variance, and
memory storage removed it. Going forward:

- **Memory-storage cells** can use a much shorter capture window
  (60 s would be plenty) without loss of signal.
- **Disk-storage cells** at high N need capture ≥ 240 s to
  robustly include / exclude the periodic spike. The compromise
  of 120 s (used here) gave ~1 % run-to-run variance for the
  signal of interest, which is good enough for ablation
  comparison but would be tight for absolute reporting.

## Artifacts

- `results/m17-20260517-153942/run-NNN-M1.7-N{1000,3000}-rep{1,2,3}/aggregated.csv`
- All 6 runs status `ok`, manifest committed.
- Comparison values computed inline (not via analyze.py — see
  M1.9 findings for the warmup-dilution caveat).
