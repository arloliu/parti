# Queue-Floor Comparison & 2.14.2 Meta Re-check — Findings

Status: **complete** — 2026-06-06. Two campaigns on a 5-node NATS **v2.14.2**
cluster: (1) the design §8 **queue-floor** matrix (12 cells × 3 reps) — the
delivery-floor reference the dynamic per-partition path is measured *against*,
never previously run; (2) a **meta_compact spot-check** at N=5000 to re-confirm
report [04](04-findings-metacontroller.md) on the upgraded server.

> **Why this round.** Reports 02/03 measured the *dynamic* per-partition path
> but never ran the `queue` cells the design specified as the comparison floor.
> Without it, "does one-consumer-per-partition scale?" had no measured baseline —
> we could say the dynamic path is cheap and flat-latency, but not how much of
> that is parti's per-partition overhead vs. the raw JetStream delivery floor.
> This round supplies that floor, and doubles as the baseline the parked
> dynamic-consumer-collapse redesign will be measured against (the collapsed
> design is approximately this queue floor).

## Configuration

| Knob | Value |
|---|---|
| Consumer mode | **`queue`** (one tuned durable over the whole stream) vs. `dynamic` (one durable pull consumer per partition, from reports 02/03) |
| Stream | data + KV **{file, memory}**, **RF=5**, 5-node cluster, **NATS v2.14.2** |
| Window | **60 s warmup + 60 s capture**, **3 reps** (matches report 03 for a like-for-like dynamic comparison) |
| Load | `k ∈ {1,2,4}` msg/s per worker; aggregate `X = k·N/50` msg/s; ~256 B payload |
| Box | AMD Ryzen 9 9950X3D; NATS pinned 0-7,16-23, harness 8-15,24-31 (isolation verified live each cell) |

All 12 queue cells **lossless** (delivery ratio 1.000, 3,600–72,000 pooled
samples/cell). Campaign: 36/36 queue + 9/9 meta runs OK, zero failures.

## Terms: partition count vs. consumer count

This report's central result turns on a distinction that is invisible in the
dynamic path alone, so define it up front:

- **Partition count (`N`)** — how finely the work is divided. In the rig it is
  the `--n` value: `N` partition-source records, one subject each
  (`perf.rig.p-0 … perf.rig.p-(N-1)`). The producer publishes across all `N`
  regardless of how the stream is consumed. This is a property of the *workload*.
- **Consumer count** — how many JetStream durable pull consumers are actually
  created to read the stream. This is a property of the *consumption strategy*.

In **`dynamic`** mode (parti's `consumer.Dynamic`, reports 02/03) there is **one
consumer per partition**, so consumer count **= N** — the two numbers move
together and the dynamic data alone cannot tell you which one drives cost. The
**`queue`** floor breaks that coupling: same `N` partitions, same producer, same
load, but **one shared consumer** (consumer count = 1, fixed as `N` grows). Every
"flat in N vs. linear in N" claim below is exactly this controlled comparison —
hold partition count and load fixed, vary only consumer count.

| | partition count `N` | consumer count |
|---|---|---|
| `dynamic` (reports 02/03) | 1000 → 5000 | **= N** (1000 → 5000) |
| `queue` floor (this report) | 1000 → 5000 | **1** (fixed) |

**Practical upshot:** dividing work into 5,000 partitions is cheap; creating
5,000 *consumers* to service them is what costs — which is why a collapse
redesign (fewer consumers for the same `N` partitions) cuts cost without changing
how the work is divided.

## Queue floor — per-cell (cluster sums over 5 nodes; latency pooled over 3 reps)

**File stream** (consumer ack-state file-backed):

| N | k | X | P50 | P95 | P99 | P99.9 | IOPS | CPU | RSS (MiB) |
|---:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| 1000 | 1 | 20 | 0.96 | 1.44 | 1.54 | n/a* | 54.6 | 0.07 | 139 |
| 1000 | 2 | 40 | 0.93 | 1.42 | 1.61 | n/a* | 55.3 | 0.11 | 156 |
| 1000 | 4 | 80 | 0.93 | 1.36 | 1.44 | 1.53 | 56.2 | 0.18 | 208 |
| 2000 | 2 | 80 | 0.92 | 1.36 | 1.44 | 1.54 | 56.7 | 0.18 | 235 |
| 3000 | 2 | 120 | 0.84 | 1.32 | 1.40 | 2.36 | 57.7 | 0.25 | 291 |
| 5000 | 1 | 100 | 0.83 | 1.31 | 1.40 | 2.01 | 58.7 | 0.23 | 325 |
| 5000 | 2 | 200 | 0.82 | 1.30 | 1.38 | 2.24 | 63.5 | 0.40 | 365 |
| 5000 | 4 | 400 | 0.74 | 1.22 | 1.29 | 1.75 | 66.1 | 0.54 | 438 |

**Memory stream** (k=2):

| N | X | P50 | P95 | P99 | IOPS | CPU | RSS (MiB) |
|---:|--:|--:|--:|--:|--:|--:|--:|
| 1000 | 40 | 0.93 | 1.41 | 2.05 | 0.9 | 0.10 | 121 |
| 2000 | 80 | 0.89 | 1.33 | 1.43 | 0.9 | 0.16 | 164 |
| 3000 | 120 | 0.82 | 1.30 | 1.38 | 0.9 | 0.22 | 206 |
| 5000 | 200 | 0.80 | 1.29 | 1.36 | 1.0 | 0.37 | 292 |

Latency in ms. *P99.9 "n/a" = pooled samples below the validity gate (n·(1−p) ≥ 10).

## Key findings

### 1. Dynamic latency is indistinguishable from the delivery floor
Queue-floor P95 is **1.30–1.44 ms** (file) / **1.29–1.41 ms** (memory), flat in N
and *improving* slightly with throughput (more frequent fetches → tighter
cadence). Dynamic production (report 03) P95 is **1.26–1.35 ms** over the same N.
The two are statistically on top of each other — if anything the single-consumer
queue is marginally *higher* at low N. So **parti's per-partition fetch cadence is
not a latency tax**: the ~1.3 ms P95 is the JetStream delivery floor itself, not
per-partition overhead. This is the measured baseline the headline latency claim
previously lacked.

### 2. IOPS scales with consumer count, not partition count
Queue-floor IOPS is **flat in partition count** — it tracks throughput X, not N.
The clean proof is an **equal-X pair**: N=1000-k4 (X=80 → **56.2**) vs N=2000-k2
(X=80 → **56.7**) — identical IOPS at 2× the partitions. With a **memory** stream
it is **~1 IOPS** flat across all N. Dynamic IOPS, by contrast, grows linearly
**27 → 142** (N=1000 → 5000). The difference is the number of *consumers*: the
queue floor has one, the dynamic path has N.

In the production config that residual growth is **not** the attribution study's
M2.A ack-state-*file* cost — `WithConsumerMemoryStorage(true)` already removed
that (report 03's ~86× IOPS drop vs the file-backed baseline). What remains and
scales with N is the **per-consumer R=3 raft state replication** (≈0.028
IOPS/consumer per report 03's open-items note; 0.028 × 5000 ≈ 140, matching the
measured 142). The queue floor's single consumer pays this once instead of N
times. (M2.A — the per-consumer state *file* — dominates instead in a
file-backed-consumer config like report 02's baseline; it is orthogonal to this
memory-config result.)

### 3. RSS is the dominant scaling pressure
Dynamic RSS grows **836 → ~4,100 MiB** (linear, ≈0.6 MiB/consumer/node). The
queue floor is far flatter: **139 → 438 MiB** (file) / **121 → 292 MiB**
(memory), growing with workers/throughput, not partition count. At N=5000 the
dynamic per-partition design carries **~10× the RSS** of the floor.

### 4. CPU tracks throughput, not N
Queue-floor CPU 0.07 → 0.54 cores tracks X; dynamic 0.24 → 1.21 cores carries an
extra per-consumer increment. ~2× headroom at N=5000.

## Dynamic vs. floor — head-to-head (N=5000, equal throughput)

| Metric | dynamic (report 03) | queue floor (file) | queue floor (mem stream) |
|---|---:|---:|---:|
| IOPS (X=100) | 142 | 58.7 | — |
| IOPS (X=400) | 148 | 66.1 | ~1 |
| RSS (MiB) | ~4,100 | 325–438 | 292 |
| CPU (cores) | ~1.0–1.2 | 0.23–0.54 | 0.37 |
| P95 latency | 1.26–1.28 ms | 1.31–1.22 ms | 1.29 ms |

**Crossover:** dynamic IOPS crosses above the file floor around **N≈2000–2500**
(dynamic N=2000 ≈ 62 ≈ floor ~57; by N=3000 dynamic 97–103 ≫ floor ~58). Below
that the file-backed single queue consumer's per-ack writes dominate; above it
the per-partition consumer count wins.

### What this means for the collapse redesign
The queue floor is approximately what a collapsed / shared-consumer design would
achieve — **flat in N**. Collapsing N per-partition consumers toward a shared
set would drive the N-linear costs down toward this floor. Upper-bound benefit at
N=5000: **IOPS ~142 → ~64** (file, ~2.2×) or **→ ~1** (memory stream); **RSS
~4,100 → ~300–440 MiB (~10×)**. **RSS is the larger prize.** Latency is already
at the floor, so a redesign buys cost/footprint, not speed.

## Meta spot-check on 2.14.2 — report 04 re-confirmed

Downward `meta_compact_size` sweep at N=5000 (the test §04's inertness claim
rests on), now on 2.14.2 (pooled over 3 reps each):

| config | meta WAL peak | snapshots/run | snapshot dur (mean / max) |
|---|---:|---:|---:|
| default | 6.17 MB | ~4.3 | 12.6 / 21.2 ms |
| 1 MB | 6.12 MB | ~3.3 | 12.0 / 27.9 ms |
| 4 MB | 6.81 MB | ~2.3 | 13.0 / 18.0 ms |

- **Knob still inert.** The load-bearing evidence is the **WAL not being
  capped**: a 1 MB compaction threshold still let the WAL grow to 6.1 MB
  (matching §04's 2.12.6 finding that the size threshold is consulted only inside
  an already-time-gated snapshot path). The snapshots/run column (4.3 / 3.3 /
  2.3) is *not* a knob effect — it is small-sample variation over 3 short reps,
  not a monotone response to the threshold.
- **Snapshots stay cheap & async.** ~12–13 ms mean (≤28 ms max) at N=5000 —
  three orders below the 1.286 s production incident; P99 data-path latency flat
  at ~1.35 ms throughout (snapshots do not stall delivery).
- **Cross-version stable.** 2.14.2 reproduces report 04's 2.12.6 numbers
  (~13–16 ms / ~5.5–6.2 MB WAL). No `meta_compact` tuning is warranted through
  N=5000 on 2.14.2.

## Version currency

This is the first campaign on **NATS 2.14.2** (reports 02/03/04 ran on 2.12.6).
The `meta-default-N5000` dynamic cell here measured **145.5 IOPS / 4,155 MiB RSS
/ P95 1.275 ms** — within noise of report 03's 2.12.6 N=5000 (**144 IOPS /
~4,000 MiB / P95 1.26 ms**), confirming the **structural costs are
version-insensitive** across the 2.12→2.14 bump. The earlier reports' numbers
stand; only the server image changed.

## Caveats

- **Consumer-state storage confound (IOPS absolute level).** The dynamic
  production config uses *memory* consumer state (R=3); the file-stream queue
  cells use the single durable's *file* ack-state. So below the ~N=2000
  crossover the file queue floor reads *higher* than dynamic — that is the file
  ack cost, not delivery. The structural insight (floor **flat** in N vs dynamic
  **linear** in N) is config-independent; the memory-stream floor (~1 IOPS)
  bounds the irreducible delivery write cost.
- **Cross-version comparison.** The floor is on 2.14.2; reports 02/03 dynamic
  numbers are 2.12.6. The version-currency cell above shows the structural costs
  reproduce, so the comparison holds — but the cleanest same-version anchor is
  the `meta-default` dynamic N=5000 cell measured here.
- **`queue` semantics.** One tuned durable consuming the whole stream is a
  *delivery-floor reference*, not a drop-in for per-partition ownership (no
  per-partition assignment / handoff). It bounds cost and latency; it is not a
  functional substitute.
- **P99.9 gating.** N=1000 k≤2 cells fall below the P99.9 sample gate (same as
  report 03); P50–P99 are valid at every cell. In-process harness tail caveat
  (design §8.2) still applies to P99.9.

## Reproduce

```
cd test/perf-measurement
export PERF_RIG_NATS_IMAGE=nats:2.14.2
# queue floor (12 cells × 3 reps, 60/60)
bash scripts/run-load-matrix.sh --seed 2142 --cells <12 queue labels> \
  --reps 3 --warmup-secs 60 --capture-secs 60 --results-dir ./results/queue-floor
# meta spot-check
bash scripts/run-meta-sweep.sh --seed 2142 --n 5000 --configs default,1MB,4MB \
  --results-dir ./results/meta-2142
# metrics
cmd/fitmodel/fitmodel --results results/queue-floor --dump
```
Raw captures gitignored under `test/perf-measurement/results/{queue-floor,meta-2142}/`.
