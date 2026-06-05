# Metacontroller Snapshot Cost & `meta_compact` Tuning

*Part of the [perf-measurement study](README.md). Builds on the
[production-config N-sweep](03-findings-production-mem-r3.md); extends it to
N=10,000.*

Status: **complete** — 2026-06-05. Downward `meta_compact_size` sweep at N=5000
+ default-vs-1MB at N=10,000 (production consumer config: memory state, R=3,
file stream RF=5, k=2; 5-node cluster, NATS v2.12.6).

## The question

The metacontroller (JetStream's meta-raft group) periodically snapshots the
cluster's asset definitions/placement. A prior incident saw this take **~1.x
seconds** at high consumer counts. This round measures: how does snapshot cost
scale, does `meta_compact_size` tuning help, and where does the 1.x-sec regime
actually start?

## Method

Snapshot stats come from `/jsz` → `meta_cluster.snapshot` (`pending_entries`,
`pending_size` = meta-raft WAL tail, `last_time`, `last_duration`), polled at
**1 Hz** (hardened capture — see "jsz capture" note below). `meta_compact_size`
is the meta-raft **WAL-size compaction threshold**. Verify-first finding (from
the Arm B run): the meta WAL peaks at only ~2–6 MB at N≤5000, so an *upward*
16/64 MB sweep is inert — this sweep goes **downward** (1 MB / 4 MB, threshold
the WAL would actually cross) and pushes N to 10,000.

## Results

### Snapshot cost scales gently and stays cheap

| N | meta entries (peak) | meta WAL peak | snapshots / run | snapshot duration (mean / max) |
|---:|---:|---:|---:|---:|
| 1000 | ~1,800 | ~2.0 MB | 4 | 3 / 3 ms |
| 2000 | ~2,550 | ~2.7 MB | 4 | 6 / 6 ms |
| 3000 | ~4,460 | ~4.8 MB | 3 | 9 / 9 ms |
| 5000 | ~5,400 | ~5.6 MB | 3–4 | 13 / 15 ms |
| **10000** | ~7,100–10,200 | ~7–10 MB | 6–8 | **25–31 / 37–60 ms** |

Snapshot duration grows ~linearly-to-mildly-superlinearly with N (~3 ms/1000
consumers), reaching **~30 ms mean / ~60 ms max at 10,000 consumers**. That is
still **20–40× below the ~1.x-second incident**. Delivery stayed lossless and
P99 latency flat (~1.4 ms) throughout — the snapshots do not stall the data path.

### `meta_compact_size` is inert at N ≤ 10,000

| N | config | meta WAL peak | snapshots | mean dur |
|---:|---|---:|---:|---:|
| 5000 | default | ~5.5 MB | 4 | 13 ms |
| 5000 | 4 MB | ~6.2 MB | 2–3 | 16 ms |
| 5000 | **1 MB** | ~5.6 MB | 3 | 15 ms |
| 10000 | default | 7.1–8.0 MB | 6–7 | 26–31 ms |
| 10000 | **1 MB** | 7.9–**10.3 MB** | 6–8 | 23 ms |

Setting `meta_compact_size` from default down to **1 MB changed nothing** — the
WAL still grew to 5–10 MB (one 10k/1MB rep reached **10.3 MB**, 10× the supposed
1 MB cap) with the same snapshot frequency and duration. Verified the config was
genuinely loaded (the container had `nats-server-meta1.conf` mounted at
`/etc/nats/nats-server.conf`), so this is real, not a harness artifact.

**Interpretation (source-grounded).** Reading `monitorCluster` in nats-server's
`jetstream_cluster.go`, two independent facts explain the data:

1. **Why `1 MB` is inert.** The size-based meta snapshot is gated by a
   **hardcoded `compactSizeMin = 8 MB`** outer check plus a 30 s minimum interval
   (`nb > compactSizeMin && time.Since(lastSnapTime) > minSnapDelta` →
   `doSnapshot`). `meta_compact_size` (`szthresh`) is only consulted *inside*
   `doSnapshot`, so any value **below 8 MB cannot engage** — the outer gate never
   lets the size path run there. That is precisely why `1 MB` changed nothing and
   the WAL still grew past it (to 10.3 MB). The knob can only push the threshold
   *above* 8 MB (snapshot *less* often); it cannot make snapshots smaller/more
   frequent. The snapshots we *did* observe at ≤10k are predominantly **forced**
   snapshots from creation-phase meta churn (peer entries / leadership catch-up as
   thousands of consumer definitions replicate), which bypass `szthresh` entirely.
2. **Why it never stalls the data path.** Meta snapshots are **asynchronous by
   default** (the code comments confirm "default async snapshots";
   `JetStreamMetaCompactSync` reverts to blocking), with a blocking *fallback*
   only once the WAL exceeds **10× the threshold** (~80 MB at the 8 MB default).
   At ≤10k the WAL never approaches that, so the ~30 ms snapshot stays a cheap
   background op (P99 stayed ~1.4 ms). The original ~1.x-sec incident was almost
   certainly the *pre-async* (sync) path or a 10×-fallback at far higher scale.

*Version note:* the constants above are from the nats-server **2.14.1** source
(the rig's embedded test dependency); the Docker cluster under test ran **2.12.6**.
Async meta snapshotting landed in 2.12, so the qualitative mechanism holds, but
the exact constants (8 MB / 30 s / 10×) are 2.14.x values and may differ slightly
in 2.12.6. The *empirical* inertness (1 MB → no effect, WAL to 10.3 MB) is from
2.12.6 directly.

## Bonus: parti scales to N=10,000

The 10k runs double as a scaling-ceiling probe. parti **converged at 10,000
partitions** (200 workers, memory+R3) in ~6 min, lossless, with P99 latency
**flat at ~1.4 ms** — identical to N=1000. The Arm B cost model
([03](03-findings-production-mem-r3.md)) extrapolates to this 2×-out-of-range
point near-perfectly:

| metric @ N=10k (X=400) | model predicted | measured |
|---|---:|---:|
| CPU cores | 2.08 | 2.08 (exact) |
| RSS | 8258 MiB | 7916 (~4%) |
| Write IOPS | ≤293 (upper bound) | 146 (even more sub-linear) |
| P99 latency | flat ~1.3 ms | 1.39 ms |

## Tuning guidance (the actionable answer)

- **At ≤10,000 consumers on NATS ≥2.12: do nothing.** The snapshot is a ~30 ms
  async background op. `meta_compact_size` is inert here because it cannot drop
  below the hardcoded 8 MB floor, and the WAL rarely sustains >8 MB anyway — so
  there is no knob to turn. The 1.x-sec problem does not occur.
- **Do NOT lower `meta_compact_size`.** Setting it below 8 MB does nothing
  (gated out); it only has effect *above* 8 MB, where it makes snapshots *rarer*.
- **Ensure NATS ≥ 2.12 and keep snapshots async** (do not set
  `JetStreamMetaCompactSync`). Async snapshotting is what removed the meta
  snapshot from the critical path — that, not `meta_compact_size`, is what fixes
  the 1.x-sec stall.
- **At very high scale (≫10k), the real lever is the async→blocking fallback.**
  A snapshot reverts to blocking once the WAL exceeds **10× `meta_compact_size`**
  (~80 MB at the 8 MB default). If you operate where the meta WAL can reach that,
  *raise* `meta_compact_size` to push the fallback point higher (keeping snapshots
  async), rather than lowering it. Measure at your actual scale before tuning —
  this study did not reach the fallback regime.

## Caveats
- **Single-node jsz vantage** (`localhost:8222`); each member snapshots its own
  meta-raft log, so one member is representative, but absolute counts are that
  node's view.
- **Snapshot count via 1 Hz `last_time` diffing** can undercount bursts of
  snapshots closer than 1 s; durations are from few (3–8) samples per run, so
  treat the duration *band* (not individual values) as the signal.
- **"Inert" means "no measurable effect across default→1 MB at N≤10,000."** The
  knob may engage at a scale where the meta log crosses NATS's internal
  compaction floor; this study did not reach it (peak ~10k meta entries).
- jsz capture: `/jsz?streams=true` (no `consumers=true`) at 1 Hz, non-fatal
  polls — the hardening that made N≥5000 jsz capture reliable (the per-consumer
  array previously blew the curl timeout).

## Raw data
`test/iops-investigation/results/meta-N5000/meta-{default,1MB,4MB}-N5000/rep{1,2,3}/`
and `results/meta-N10000/meta-{default,1MB}-N10000/rep{1,2}/` (gitignored).
Extract: `jq -rs '[.[]|select(.endpoint=="jsz")|.body.meta_cluster.snapshot]' <jsz.raw>`.
