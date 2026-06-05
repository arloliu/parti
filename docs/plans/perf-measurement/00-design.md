# Dynamic Partition-Consumer Performance Measurement — Design

Status: **DESIGN** (no production code; measurement-rig + analysis only)
Branch: `worktree-perf-measurement`
Author: perf-measurement brainstorming session, 2026-06-03

## 1. Purpose & questions

Measure the **cost and delivery latency** of parti's dynamic partition
consumers (`consumer.Dynamic` → `internal/durable.WorkerConsumer`, one
durable pull consumer per partition) as the partition count scales toward
production sizes (2000 / 3000 / 5000), under a light, realistic delivery
overlay.

Concretely, answer:

1. **Does one-consumer-per-partition scale?** Plot the per-cell cost and
   delivery-latency percentiles against partition count `N` and produce a
   defensible cost-estimation model for arbitrary `N`.
2. **What is the static cost** (block-write IOPS, CPU, RSS) at each `N`,
   under light load — distinct from the prior *idle* IOPS rig.
3. **What are the message-delivery-duration percentiles**
   (P50/P90/P95/P99/P99.9/max) at each `N` and per-worker rate?
4. **What does the JetStream metacontroller snapshot cost** at high
   consumer count, and how do the NATS 2.12+ `meta_compact*` knobs move
   it? (Prior production incident: `Metalayer snapshot took 1.286s` at
   ~10k consumers.)

### 1.1 Framing of the load

The load is deliberately **light**: per-worker rate `k ∈ {1, 2, 4}` msg/s,
so per-partition rate is `k/50 ≈ 0.02–0.08` msg/s. This is intentional —
it isolates the *structural* scaling cost (consumer count, metacontroller
snapshot, per-consumer state-file IOPS) with a realistic light-delivery
overlay. The latency percentiles therefore measure parti's **per-partition
fetch-cadence floor**, not a throughput-saturation regime. The findings
must state this framing explicitly so the numbers are not misread as a
max-throughput benchmark.

## 2. Non-goals

- **Not** re-running the IOPS *attribution* ablations (M1.x). That work is
  settled on `main` (`docs/plans/iops-investigation/findings.md`): parti's
  own KV coordination is ~1% (M1.9 noise); the per-consumer JetStream
  state-file write is ~80% of block-write IOPS (M1.7 ≡ M2.A). We reuse
  those conclusions; we do not reproduce them.
- **Not** a throughput-saturation / max-msg/s benchmark.
- **Not** the dynamic-consumer-collapse redesign (FINDING-A). That is a
  separate design effort (`docs/plans/dynamic-consumer-collapse/`). This
  measurement quantifies the *current* per-partition design so that
  redesign has a baseline.
- **Not** production code changes to parti. The only parti-facing knobs are
  the already-supported consumer storage/replica overrides the IOPS rig
  uses.

## 3. Hardware envelope

Single box (probed 2026-06-03):

- **CPU**: AMD Ryzen 9 9950X3D — 16 cores / 32 threads, single NUMA node,
  ~5.7 GHz boost, large X3D cache.
- **RAM**: 62 GiB total (~44 GiB free at probe time).
- **Disk**: two NVMe — Crucial T710 1 TB (PCIe 5.0, `nvme0n1`) and Samsung
  970 PRO 512 GB (`nvme1n1`, holds the repo). NATS data goes on the
  **Crucial T710**, isolated from the repo/harness disk so the NATS-cgroup
  `io.stat` is clean.
- **Control groups**: cgroup v2; Docker 29.5 with the systemd cgroup
  driver; all 32 CPUs visible to Docker.

This box can reach `N=5000`, but **`N=5000 / RF=5 / 5000 consumers` on a
5-node cluster sharing 16 cores is the saturation ceiling.** The run plan
ramps `N` upward and **stops honestly** if the box saturates (defined in
§9), documenting the ceiling rather than silently truncating coverage.

## 4. Architecture — extend the IOPS rig

The harness lives in the existing **separate module**
`test/perf-measurement/` (`module github.com/arloliu/parti/test/perf-measurement`).
We reuse, unchanged where possible:

- Docker-compose cluster bring-up (`docker/`, `Makefile`), with
  `PERF_RIG_NATS_REPLICAS=5` for the RF=5 cluster.
- `internal/aggregate/` capture: cgroup io/cpu, iostat, jsz, RPC
  aggregation, manifest.
- `cmd/harness` orchestration, worker/manager wiring, partition seeding,
  `InstrumentedJS`, `WaitStableAll`.
- The `tier0.5` `dd` calibration for the chosen NATS data device.

New code (all under the same module):

| Path | Responsibility |
|---|---|
| `internal/load/` | Open-loop, rate-governed producer (CLOCK_MONOTONIC `intended_mono_ns` payloads). |
| `internal/latency/` | HDR histogram recording + cross-worker merge + per-percentile sample gating + export. |
| `internal/costmodel/` | Load-aware per-metric fit `a + b·N + c·X` (per storage) + prediction with R²/residual bounds. |
| `cmd/estimator/` | CLI: `(N, k, storage) → predicted IOPS/CPU/RSS/latency` (derives M=N/50, X=k·M; refuses RF≠5). |
| `cmd/harness` (extend) | Add `--load` mode: start producer, swap noop→latency handler, emit latency rows. |
| `internal/aggregate/jsz.go` (extend) | Capture `meta_cluster.snapshot.last_duration` + snapshot size/count. |

### 4.1 Why in-process, one box

Producer and all `M` parti managers run as goroutines in **one harness
process**, and publish-time/receive-time use a **host-wide
`CLOCK_MONOTONIC`** reading (see §5/§6) — the end-to-end latency number is
free of cross-host clock skew and works unchanged for the §8.2
out-of-process cell. The harness and the NATS cluster are isolated by
**CPU pinning/affinity** (NATS containers via Docker cgroup `cpuset`, the
harness via process affinity — see §10 for the mechanism split) so the
CPU-cost column stays attributable despite co-location.

**Documented caveat (CPU/RSS):** workers are in-process goroutines, not
separate OS processes, so real per-worker *process* overhead (separate
heaps, GC, FD tables) is not captured. The cost model's harness-side
CPU/RSS columns are therefore *aggregate* figures; the load-bearing scaling
signal is the **NATS-side** cgroup metrics (IOPS, CPU, RSS of the cluster
containers), which are unaffected by harness in-processness.

**Documented caveat (latency scope):** in-processness also affects the
*latency tail*, not just CPU/RSS — all `M` workers and the producer share
one Go scheduler, heap, and GC, so a scheduler/GC pause can perturb the tail
in a way a real fleet of separate worker processes would not. The reported
delivery-latency percentiles are therefore **scoped to the in-process
harness** and labelled as such. To bound that bias, the plan includes one
**out-of-process validation cell** (§8.2): at `N=1000` the `M` workers run
as separate OS processes and the latency tail is compared against the
in-process cell. If the deltas are small the in-process tails stand as-is;
if large, the findings carry the measured correction factor. This cell is a
stretch item — if the out-of-process worker harness is not built, the
latency claims remain explicitly in-process-only.

## 5. Producer (`internal/load`) — open-loop, coordinated-omission-safe

- **Rate model**: aggregate target `X = k · M` msg/s, with per-worker rate
  `k ∈ {1, 2, 4}` and `M = N/50`. Messages are distributed round-robin
  across the `N` partition subjects (so per-partition ≈ `X/N = k/50`).
- **Open-loop schedule**: sends are fired on a fixed cadence
  (`interval = 1/X`) **independent of consumer progress**.
- **Host-wide monotonic clock:** Go's `time.Time` monotonic component is
  stripped on marshal, and `UnixNano` carries the wall clock (which NTP can
  step). So timestamps use **Linux `CLOCK_MONOTONIC`** read via
  `golang.org/x/sys/unix.ClockGettime(CLOCK_MONOTONIC)`, which is a
  **host-wide** clock (consistent across processes on the same machine,
  never steps backward). The payload carries the **scheduled-tick**
  `CLOCK_MONOTONIC` nanosecond reading as `intended_mono_ns` (the scheduled
  instant, not the actual publish instant), plus a sequence number and
  partition id; ~256-byte total payload. The receiver reads `CLOCK_MONOTONIC`
  and computes `latency = recv_mono_ns − intended_mono_ns`. Because the
  clock is host-wide this works **both** for the in-process cells and for the
  §8.2 out-of-process cell (the separate worker processes share the same
  host monotonic source) — no per-process epoch.
- **Coordinated-omission correction (Gil Tene)**: latency uses
  `intended_mono_ns`, *not* the actual publish instant. A late publish still
  pays for its own queueing delay, so the tail is not silently
  under-reported.
- **Non-blocking publish**: use bounded `PublishAsync` (in-flight cap) so a
  slow publish ack does not stall the schedule loop.
- **Producer-health / "producer-bound" guard (preregistered):** the producer
  records per-send `actual_publish_mono_ns − intended_mono_ns` skew. A cell
  is flagged **`producer-bound`** (latency numbers reported as suspect, not
  trusted) if **P99 send-skew > 5 ms** OR **>1% of scheduled sends are
  delayed > 10 ms** OR any `PublishAsync` ack errors/timeouts occur during
  the window. PublishAsync errors are counted and surfaced, never dropped.
  (At these light rates — max X = 400 msg/s — this should never trip; the
  guard exists to keep the method honest and to make a future high-X variant
  trustworthy.)

## 6. Latency handler + HDR (`internal/latency`)

- Replaces `noopHandler`. On `Handle`, parse `intended_mono_ns`, read
  `CLOCK_MONOTONIC` (`recv_mono_ns`), compute `recv_mono_ns −
  intended_mono_ns`, record into a **per-worker** `hdrhistogram` (one
  histogram per worker, no shared lock; merged at window end), then
  `return nil`.
- **Ack contract — resolved (no double-ack):** the rig leaves the consumer
  at its default `ManualAck=false`, so a `nil` return **auto-acks** (verified:
  `consumer/common.go`, `internal/ipartition/key_dispatcher.go`,
  `internal/recovery/controller.go`; the existing harness does not pass
  `WithManualAck(true)`). The latency handler therefore **records and returns
  `nil` — it does NOT call `msg.Ack()`** (an explicit Ack on top of auto-ack
  would double-ack and contaminate the per-ack IOPS being measured). This is
  re-confirmed against source before any cell is trusted.
- **Capture window**: a **120-second** steady-state window — record only
  messages whose `intended_mono_ns` falls in `t ∈ [120 s, 240 s]` from run
  start (reusing the rig's convention), excluding the startup ramp (5000 R=5
  consumers take time to create + reach Stable).
- **Per-percentile sample-count gating (anti-noise):** at the lowest cell
  (`N=1000, k=1`, X=20 msg/s) a 120 s window yields only ~2 400 delivered
  samples per rep, so P99.9 would see ~2–3 tail samples = order-statistic
  noise. Rule: a percentile is **only published for a cell if the pooled
  sample count across reps gives ≥ 10 expected samples beyond it** (i.e.
  `n·(1−p) ≥ 10`). For P99.9 that needs ≥ 10 000 pooled samples; cells below
  the threshold either (a) extend the window until met, or (b) report the
  percentile as `n/a (under-sampled, n=…)`. `max` is always reported but
  labelled as a single-sample extreme, not a stable statistic. P50–P99 clear
  the bar at all cells with 3 reps pooled.
- **Outputs per cell**: P50/P90/P95/P99/P99.9(gated)/max, pooled sample
  count, count delivered, count produced, achieved-vs-target throughput,
  producer-health skew summary, and the `producer-bound` flag.

## 7. Consumer modes

- **`dynamic`** — the path under test: one durable pull consumer per
  partition (`consumer.Dynamic`). Fetch params materially shape the latency
  floor, so they are **pinned via explicit harness flags and recorded in the
  manifest per cell**. The current rig exposes only `--fetch-timeout`
  (`cmd/harness/main.go`); this work **adds flags** for the consumer options
  that already exist in `consumer/options.go` — fetch **batch size**,
  **MaxWaiting**, **MaxAckPending**, and **AckWait** — and writes their
  resolved values into `manifest.yaml` alongside each cell. Default values
  are chosen once, documented, and held constant across the whole matrix so
  P95 is comparable cell-to-cell.
- **`queue` (NATS floor)** — the existing single tuned durable
  (`ConsumerModeQueue`) consuming the same stream/subjects, as the
  delivery-floor reference. Same producer, same payload — isolates parti's
  per-partition fetch-cadence overhead from the underlying JetStream
  delivery floor.

## 8. Run matrix

Fixed throughout: **RF = 5** (5-node cluster), `M = N/50`, payload ~256 B,
**120 s** capture window `t ∈ [120 s, 240 s]`, **3 reps** per cell,
`make reset` between runs (fresh JetStream state).

| Dimension | Values |
|---|---|
| `N` (partitions) | 1000, 2000, 3000, 5000 |
| `M` (workers, = N/50) | 20, 40, 60, 100 |
| `k` (per-worker msg/s) | 1, 2, 4 → aggregate `X = k·M` |
| Storage (data stream) | File, Memory |
| ConsumerMode | dynamic (under test), queue (floor) |

- **Dynamic cells**: `N(4) × k(3) × Storage(2)` = **24 cells**.
- **Queue-floor cells**: `N(4) × Storage(2)` at `k=2` = **8 cells**, plus
  **load-invariance corners** `k ∈ {1, 4} × N ∈ {1000, 5000}` at
  Storage=File = **4 cells**, so the floor is shown flat across the same `k`
  range the dynamic path is compared over (not assumed). = **12 cells**.
- **Total**: 36 cells × 3 reps = **108 runs** (light load → short cells;
  dominated by setup + the 120 s window).

Aggregate `X` per `N` (msg/s): N=1000 → {20,40,80}; N=2000 → {40,80,160};
N=3000 → {60,120,240}; N=5000 → {100,200,400}.

### 8.1 Metacontroller (`meta_compact`) sub-matrix

At **`N=5000`** only (highest consumer count), on NATS **2.12.6** (the
rig's default image), sweep:

| Knob | Values |
|---|---|
| `meta_compact_size` | default, `16MB`, `64MB` |

Storage = File, k = 2, dynamic mode. **Verified `/jsz` schema (NATS 2.12.6,
captured live 2026-06-04)** — `meta_cluster.snapshot` carries:
`last_duration` (int **nanoseconds**, `omitempty` — absent until the first
snapshot fires), `pending_size` (int bytes — the meta-raft **WAL tail** that
`meta_compact_size` gates on), `pending_entries` (int), and `last_time`
(RFC3339). **There is NO snapshot `count` or marshal/compressed byte size in
`/jsz`** — those appear only in the server **log** WRN line, and only when
duration > 2s (2.12.6 async snapshotter). So we measure: **duration** =
`last_duration` (the headline metric, always in `/jsz`); **frequency/count** =
number of distinct `last_time` values over the capture; **tail size** =
`pending_size` (what the knob moves). Marshal/compressed size is out of scope
for the `/jsz` parser (log-only).

Implementation requirements (the current `internal/aggregate/jsz.go` parser
extracts only per-stream state, not meta fields):

1. **Verify the `/jsz` schema first** against the actual NATS 2.12.6 image —
   confirm the field path for meta snapshot stats (`meta_cluster.snapshot.*`
   / `MetaSnapshotStats`) by curling a live node before extending the
   parser. No guessing the JSON shape.
2. **Capture from cluster startup through steady state**, not just the
   `[120,240]s` window — the metacontroller snapshots are partly driven by
   consumer-creation churn during startup (5000 R=5 consumers), so the
   first-snapshot duration and the early-churn frequency are part of the
   signal, alongside the steady-state ~60 s-cadence snapshots.
3. **Gate each knob setting on an observed snapshot count ≥ N_min** (e.g.
   ≥ 5 snapshots) over the capture interval, so a setting is compared on a
   distribution of snapshot durations, not a single `last_duration` reading.

This empirically tests the prior `meta_compact_size` analysis: NATS 2.12+
made meta snapshotting async (no `js.mu` stall), and `meta_compact_size`
gates on the meta Raft WAL-tail size to stop the every-~60 s full snapshot.
Expected result: `16MB` cuts snapshot *frequency* in steady state without
inflating per-snapshot duration; `64MB` risks a slower-replay fallback. The
key prior facts are summarised inline here so the measurement is
self-contained (no external-memory dependency).

### 8.2 Out-of-process latency-validation cell (stretch)

One cell at `N=1000, k=2` runs the `M=20` workers as **separate OS
processes** (not in-process goroutines) against the same cluster, to measure
how much the shared Go scheduler/GC of the in-process harness perturbs the
latency tail (§4.1 caveat). This cell is comparable precisely because the
clock is host-wide `CLOCK_MONOTONIC` (§5): the producer process and the
separate worker processes read the same monotonic source, so
`recv_mono_ns − intended_mono_ns` is valid across process boundaries.
Compared head-to-head with the in-process `N=1000, k=2` cell: if the delta
is small, the in-process tails stand; if large, the findings publish the
measured correction factor. Stretch item — if the out-of-process worker
harness (`cmd/worker`) is not built within scope, the latency claims remain
explicitly in-process-only and this cell is logged as not-run.

## 9. Saturation / honesty guards (no silent caps)

A cell is declared **invalid** (and the `N` ramp stops, with the ceiling
documented) if any **preregistered** threshold holds:

- **Startup budget exceeded.** Workers fail to reach Stable within the
  budget, i.e. the cluster cannot create/serve `N×RF` consumers. The
  existing per-worker wait is **30 s** (`StartWorker`), which is too small
  for `N=5000/RF=5`; this work makes the budget **scale with N**
  (`WaitStableAll` timeout = `max(60 s, N × 60 ms)` → 5 min at N=5000),
  preregistered and recorded in the manifest.
- **Producer-bound** flag trips (§5 thresholds: P99 send-skew > 5 ms, or
  >1% sends delayed > 10 ms, or any PublishAsync ack error).
- **Delivery deficit.** `delivered / produced < 95%` over the window → the
  system is not keeping up at this `N`; that is itself a reportable scaling
  result, not a number to silently average.
- **CPU saturation.** The NATS cpuset is **≥ 95% utilised for ≥ 10% of the
  capture window** (sampled from NATS-cgroup `cpu.stat`) → CPU-confounded;
  report and stop.

Whatever is dropped is `log`-reported in the findings ("N=5000 Memory not
captured: cluster did not reach Stable in T s"), never silently omitted.

## 10. cgroup / disk isolation

- **CPU (concrete, not prose).** The current `docker/docker-compose.yaml`
  has **no** `cpuset`/CPU limits and the scripts use no `taskset` — this
  work adds them:
  - Verified host topology (`lscpu -e`): logical CPUs 0–15 are physical
    cores 0–15; logical CPUs 16–31 are their SMT siblings (CPU 16↔core0, …,
    CPU 31↔core15). The split keeps each physical core's two threads on the
    same side:
    - **NATS** (5 containers) → `cpuset: "0-7,16-23"` = physical cores 0–7
      + their siblings (16 logical CPUs).
    - **Harness** → `taskset -c 8-15,24-31` = physical cores 8–15 + their
      siblings (16 logical CPUs).
    The exact `lscpu -e` map is frozen in the manifest at run time.
  - After bring-up, **verify each primitive against the right mechanism**
    (Docker `cpuset` sets a cgroup; `taskset` sets process affinity — they
    are not the same knob):
    - **NATS containers** → read `cpuset.cpus.effective` from each
      container's cgroup; assert it equals `0-7,16-23`.
    - **Harness** → read the process CPU affinity via `sched_getaffinity`
      (equivalently `taskset -pc <pid>`); assert it equals `8-15,24-31`.
    Abort the run if either check fails. Both resolved values + the
    verification results are written to `manifest.yaml`.
- **Disk**: NATS `store_dir` → a Docker named volume on the **Crucial T710
  (`nvme0n1`, PCIe5)**; the harness and repo stay on `nvme1n1`. NATS-cgroup
  `io.stat` therefore reflects only JetStream writes.
- **CPU + RSS capture (same cgroup mechanism as IOPS).** Per NATS container,
  the runner polls the container's cgroup v2 scope dir at 1 Hz —
  `capture-cgroup-cpumem.sh`, sibling of the `io.stat` poller — reading
  `cpu.stat`'s `usage_usec` (cumulative CPU time, µs) and `memory.current`
  (instantaneous RSS, bytes) into `cgroup_cpumem.raw`. `cmd/fitmodel` diffs
  consecutive `usage_usec` into a per-second **CPU fraction-of-one-core**
  (Δusage_usec / Δwall_usec, so 1.0 = one full core), carries `memory.current`
  through as instantaneous RSS, sums each across the 5 NATS containers per
  second, and means over the post-warmup window — yielding `<mode>_cpu_cores`
  and `<mode>_rss_bytes` alongside `<mode>_write_iops`. The capture is optional:
  runs predating it (and the idle M1.x rig) simply omit the metrics.
- **Calibration**: run the rig's `tier0.5` `dd` calibration against the
  T710 once and record it in the manifest, so absolute IOPS are anchored.

## 11. Cost-estimation model (`internal/costmodel`, `cmd/estimator`)

- **Fit — load-aware, not N-only.** The matrix varies both `N` and load
  `X = k·M`, so fitting `cost ≈ a + b·N` alone would fold the load term into
  the structural coefficient and make any prediction at a different `X`
  meaningless. Instead, **per Storage type**, fit the preregistered
  multivariate form:

  `cost ≈ a + b·N + c·X`   (with an optional `N·X` interaction term kept
  only if it materially improves the residuals)

  for each metric (NATS block-write IOPS, NATS CPU, NATS RSS, and — where
  sample-gated — each latency percentile). Because the load is light, `c` is
  expected to be small for the structural metrics (confirming that consumer
  *count*, not light traffic, drives cost) and that is a reportable result
  in itself. Report `a, b, c`, R², and residual spread per metric per
  storage. A metric that is clearly non-linear in `N` (e.g. metacontroller
  snapshot cost — likely super-linear) is **not** forced into the affine
  form: fit the appropriate form or mark it "extrapolate with caution" with
  the raw points shown.
- **Sample-size honesty.** With 4 `N`-points × 3 `k`-points × 3 reps the fit
  has enough rows for `a + b·N + c·X` per storage, but 4 distinct `N` values
  is thin for confident extrapolation far beyond N=5000 — the findings state
  the fit's valid range and widen the caveat outside it.
- **Headline verdict**: the **latency-percentile-vs-N curve** at fixed `k`,
  plus the IOPS/CPU/RSS-vs-N curves, answer "does one-consumer-per-partition
  scale?" with stated confidence bounds and the saturation ceiling from §9.
- **Estimator CLI** (`cmd/estimator`): input `(N, k, storage)` — it
  **derives** `M = N/50` and `X = k·M` itself (it does not accept arbitrary
  `M`/`X` that contradict the design's `M=N/50` coupling), and **refuses**
  (`error`, no prediction) for `RF ≠ 5`, since the model is fit at RF=5 only.
  Output: predicted IOPS / CPU / RSS / latency-percentiles from the fitted
  coefficients, with an explicit caveat banner when `N` is outside the
  measured range (extrapolation).

## 12. Deliverables & layout

```
test/perf-measurement/
  internal/load/            # open-loop producer (monotonic-epoch payloads)
  internal/latency/         # HDR record + merge + percentile gating + export
  internal/costmodel/       # multivariate fit (a + b·N + c·X) + predict
  cmd/estimator/            # estimator CLI (input N,k,storage)
  cmd/harness/              # extended: --load mode, fetch-param flags,
                            #   scaled startup budget, cpuset wiring
  cmd/worker/               # stretch: out-of-process worker for §8.2 cell
  internal/aggregate/jsz.go # extended: verified meta-snapshot capture
  docker/docker-compose.yaml# extended: cpuset pinning + T710 volume
  scripts/                  # extend run-matrix.sh for the load matrix
docs/plans/perf-measurement/
  00-design.md              # this file
  findings.md               # measured cells + model + verdict (final)
```

`findings.md` mirrors the IOPS `findings.md` style: cell-mean table with a
human-readable legend, the cost-model coefficients, the saturation ceiling,
the metacontroller-tuning result, and the operator-facing recommendation.

## 13. Risks & open items

1. **N=5000 / RF=5 feasibility** — 5000 consumers × RF=5 metacontroller +
   5-node cluster on 16 cores is the stated ceiling; §9 guards (scaled
   startup budget, delivery-deficit, CPU-saturation) make a saturated result
   reportable rather than a silent average. Ramp N upward; N=5000 is last.
2. **In-process latency + CPU/RSS caveat** (§4.1) — both the tail and the
   harness-side CPU/RSS are perturbed by the shared Go runtime; latency
   claims are scoped in-process and bounded by the §8.2 out-of-process
   validation cell; the load-bearing cost signal is NATS-side cgroup metrics.
3. **Ack contract — resolved** (§6): default auto-ack, handler returns nil,
   no explicit Ack; re-confirmed against source before trusting cells.
4. **Cost-model form** (§11) — load-aware `a + b·N + c·X` per storage;
   metacontroller cost may be super-linear and is fit/flagged separately; 4
   `N`-points bound extrapolation confidence; the latency floor may be flat
   under light load, which is itself the answer.
5. **Percentile under-sampling — resolved** (§6): per-percentile sample
   gating (`n·(1−p) ≥ 10`); P99.9 reported only when met, else `n/a`.
6. **Wall-clock** — 108 runs × (~setup + 120 s window + per-cell `make
   reset`) is multi-hour; `run-matrix.sh` must be resumable/idempotent per
   cell so a mid-matrix failure does not restart from zero.
7. **jsz meta schema** (§8.1) — verified against the live 2.12.6 image
   before extending the parser; not assumed.

## 14. What success looks like

- A `findings.md` with: per-cell cost + latency-percentile table across the
  N sweep; the affine cost model (coefficients + bounds); the
  metacontroller `meta_compact_size` result at N=5000; the documented
  saturation ceiling; and a clear verdict on whether
  one-consumer-per-partition scales to 5000 on this class of hardware.
- A working `cmd/estimator` that predicts cost for an arbitrary `N`.
- All numbers reproducible via the extended `run-matrix.sh` on the rig.
