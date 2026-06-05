# 00 — NATS IOPS Attribution Plan (v2.3.0)

## Goal

Produce, from measurement (not inspection alone), a **per-subsystem IOPS
budget** for an idle Parti v2.3.0 cluster, parameterised by partition
count. Then verify which mitigation knobs actually move the curve.

"Idle" = workers running, leader elected, partitions assigned, but no
messages flowing through the consumer handlers.

The budget is **four-dimensional**, not one. The plan reasons about each
column separately and the success criteria require each to close
independently:

```
( read_rpc_ops/s ,  write_mutation_ops/s ,  block_read_iops ,  block_write_iops )
```

The user's report measures the last column (PVC IOPS). Mapping
predictions from the first two columns into the last two requires the
M4 calibration step (below) and is the part of the plan most prone to
silent error — hence the column-separation discipline.

Success means: for N ∈ {500, 1000, 2000, 3000} partitions on a 3-replica
NATS cluster, the sum of attributed per-subsystem contributions lands
within the rig-derived ±X % envelope (X established empirically; see
M2.2) of the measured totals **in each of the four columns
independently**, and at least one mitigation knob demonstrably drops
the per-partition slope by an amount larger than the minimum detectable
effect computed from the no-Parti control variance.

The plan deliberately does **not** commit to a code change. Code is a
follow-up, decided after the data.

## Why this matters

The user's reported curve (275 / 412 / 565 PVC IOPS for 1000 / 2000 / 3000
partitions) puts Parti's idle floor near 0.5× the partition count at
steady state on R3 storage. That budget is consumed before the
application sends a single message. If the dominant source is a
30 s-period O(N) loop with no observable benefit at that cadence, raising
the interval costs us nothing and frees substantial disk and replication
budget.

The investigation also closes the diagnostic gap that nothing
in the Parti repo currently quantifies idle IOPS as f(N). The next
operator who runs into a "why is my disk hot" question deserves a
documented number.

## A note on the v2.3.0 default config

The user said "v2.3.0 default" but Parti v2.3.0's library default is
**`EnableTwoPhaseHandoff = false`** (`v2.3.0:config.go:382-394`). When
that flag is false, `handoff.New` returns a `direct` coordinator whose
`Start` is a no-op (`v2.3.0:internal/assignment/handoff/coordinator.go:103-139`,
`v2.3.0:internal/assignment/handoff/direct.go:15-16`), so the sweep
loop in H1 does not run. The user's observed slope is therefore
**evidence that their deployment enables two-phase handoff** (or that
H1 is wrong; either way, recording this flag is load-bearing).

Each measurement run's `manifest.yaml` must record the value of
`EnableTwoPhaseHandoff` and Parti's commit/version. The plan treats
two distinct baselines:

- **B-prod** — `EnableTwoPhaseHandoff = true`, matches the user's
  observed topology. H1 applies.
- **B-lib** — `EnableTwoPhaseHandoff = false` (library default).
  H1 predicts zero sweep cost; the remaining floor must still
  reproduce a comparable curve, otherwise H1 cannot be the whole
  story.

## Hypotheses (pre-registered, falsifiable)

Each hypothesis specifies its **predicted contribution** in each of the
four IOPS columns and its **ablation**.

### H1 — Two-phase handoff sweep dominates the per-partition slope (in B-prod only)

- **Code:** `v2.3.0:internal/assignment/handoff/twophase.go:41-49`
  (ticker) → `:380-418` (`maybeSweepClaims`).
  Default `SweepInterval = 30s` (`v2.3.0:config.go:48-55`,
  `v2.3.0:internal/assignment/handoff/coordinator.go:127-130`).
- **Per worker per sweep:** 1 × `Store.ListKeys()` + N × `Store.Get(pid)`
  (`v2.3.0:internal/assignment/handoff/twophase.go:398-418`). These map
  to KV `Keys(ctx)` and `Get(ctx, key)` on the handoff-claims bucket
  (`v2.3.0:internal/assignment/handoff/kv_store.go:77-87,124-126`).
  **No writes** unless a claim is *expired and non-stable* — in steady
  idle there are zero such claims.
- **Predicted contribution:**
  ```
  read_rpc_ops/s        ≈ 5 × (N + 1) / 30   ≈  N / 6   (in B-prod)
  write_mutation_ops/s  ≈ 0                              (in B-prod, idle)
  block_read_iops       ≈ predicted-read-ops × R_read   (R_read from M4)
  block_write_iops      ≈ 0                              (in B-prod, idle)
  ```
- **Predicted slope (B-lib):** all four columns ≈ 0 contribution.
- **Ablation A (cadence):** raise `Handoff.SweepInterval` to 5 min.
  Predicted: `read_rpc_ops/s` slope drops by 10×. Floor unaffected.
- **Ablation B (mechanism):** set `parti.Config.EnableTwoPhaseHandoff =
  false` and leave `Config.KVBuckets.HandoffBucket` populated (the
  bucket is unused but the validator does not require it to be empty;
  `v2.3.0:config.go:515-523`). Predicted: H1 contribution vanishes;
  if slope persists, it is not H1.
- **Falsification:** if B-prod with 5 min sweep still produces the same
  slope, H1 is wrong about the *mechanism* even if the math fits.

### H2 — Per-partition durable JS pull consumer state churn

Each partition assigned to a worker gets one durable JS pull consumer
in `consumer.Dynamic`. Per-partition consumer creation:
`v2.3.0:internal/durable/worker_consumer.go:447-460`. The iterator
factory wires `PullExpiry = FetchTimeout` and `PullHeartbeat ≈ expiry/2`
at `v2.3.0:internal/durable/worker_consumer.go:723-732`. Default
`FetchTimeout = 5s`.

- **Per consumer at idle:** a pull request is in flight for up to
  `FetchTimeout`; when it expires (server returns no message), a new
  one is issued. Server state (`NumWaiting`, `last_active_time`)
  transitions on each cycle.
- **Open question (the load-bearing one):** does NATS persist these
  transitions to the consumer's stream-state log on disk?
  Empirically uncertain; it depends on NATS server version. **The user's
  observation that memory-KV did not move the curve constrains H2 here:
  if H2 is the slope source, then either the *data stream* (which holds
  the per-partition consumers) is the file-storage source, or NATS
  consumer-state persistence dominates.** Either is testable.
- **Predicted contribution (B-prod):**
  ```
  read_rpc_ops/s        ≈ 0   (server-internal, not client RPC)
  write_mutation_ops/s  ≈ unknown — could be 0 (memory-only state),
                                  could be 2 × N / FetchTimeout
  block_read_iops       ≈ 0
  block_write_iops      ≈ unknown
  ```
- **Ablation A (cadence):** raise `FetchTimeout` to 30 s. If H2 is the
  slope source, magnitude drops ~6×.
- **Ablation B (consumer fan-out):** switch to `consumer.Queue` —
  collapses N consumers to 1. If H2 dominates, slope ≈ 0.
- **Ablation C (storage class):** apply M5 to the **data stream** (not
  just KV buckets). If slope drops, H2's underlying state writes are
  disk-bound.

### H3 — Heartbeat + stable-ID + election: constant write floor

These are O(1) write loops; together they form the visible portion of
the floor.

- **Heartbeat publisher** — every manager. Ticker at
  `v2.3.0:internal/heartbeat/publisher.go:141-153`; loop at `:198-223`;
  KV `Put` at `:225-236`. Default `HeartbeatInterval = 5s`
  (`v2.3.0:config.go:304-309`). **5 workers → 1 KV `Put` / s.**
- **Stable-ID renewal** — every manager. Ticker at
  `v2.3.0:internal/stableid/claimer.go:253-260`; KV `Put` at
  `:289-306`. Renewal cadence `max(WorkerIDTTL/3, 100ms)`. Default
  `WorkerIDTTL = 75s` (`v2.3.0:config.go:297-302`) → 25 s renewal.
  **5 workers → 1 KV `Put` per 5 s.**
- **Leader election** — every manager. Ticker at
  `v2.3.0:manager_election.go:102-190`. Cadence `ElectionTimeout/3`;
  default `ElectionTimeout = 10s` (`v2.3.0:config.go:344-348`) →
  ~3.33 s. Leader renews lease (`kv.Update`); followers re-attempt
  acquisition (`kv.Create` returning `KeyExists`). Idle steady cost:
  ~1.5 ops / s aggregate across cluster.
- **Heartbeat monitor (leader-only)** — `WorkerMonitor` runs only on
  the leader via the Calculator (`v2.3.0:manager.go:365-369`,
  `v2.3.0:internal/assignment/calculator.go:140-146,238-250`). Two
  read paths into the heartbeat bucket:
  - Periodic fallback ticker at
    `v2.3.0:internal/assignment/worker_monitor.go:180-202`, cadence
    `HeartbeatTTL/2`; default `HeartbeatTTL = 15s`
    (`v2.3.0:config.go:310-313`) → 7.5 s polls. Each poll calls
    `getActiveWorkers`, which reduces to **one `heartbeatKV.Keys()`
    call** — it extracts worker IDs from key names and does
    **not** `Get` per worker
    (`v2.3.0:internal/assignment/worker_monitor.go:135-176`;
    `v2.3.0:internal/assignment/calculator.go:508-512,735-765`).
  - Watcher-triggered debounced reads: the leader starts a heartbeat
    watcher (`v2.3.0:internal/assignment/worker_monitor.go:185-191,
    230-237`). Each non-nil watcher event schedules `onChangeCb` with
    a 100 ms debounce, which calls the same `getActiveWorkers` /
    `Keys` path (`v2.3.0:internal/assignment/worker_monitor.go:278-310`).
    Workers publish heartbeats every 5 s
    (`v2.3.0:internal/heartbeat/publisher.go:198-236`), so per-worker
    event rate × debounce-coalesced calls drive an additional `Keys`
    rate proportional to W, not to N. With W = 5 and 5 s heartbeats,
    expect ~1 watcher-triggered `Keys` call per second after
    debouncing.

  Total leader heartbeat-monitor cost: **independent of N.**

- **Predicted contribution (cluster, idle):**
  ```
  read_rpc_ops/s        ≈ (1 / 7.5)              (heartbeat fallback Keys)
                          + ~1                   (heartbeat watcher-driven Keys)
                          + ~1.5                 (election follower Create)
                          ≈ small constant ~2.6 ops/s
  write_mutation_ops/s  ≈ (W / HeartbeatInterval)            = 1.0  (heartbeats)
                          + (W / 25)                          = 0.2  (stable-ID)
                          + (1 / (ElectionTimeout/3))         = 0.3  (leader renew)
                          ≈  ~1.5 ops/s     (cluster total)
  block_*_iops          ≈ M4-scaled values; both small
  ```
- **Ablation:** raise heartbeat interval to 10 s. Predicted: write-ops
  floor drops by 0.5 ops/s. Used to verify floor accounting, not the
  slope.

### H4 — JetStream RAFT / metadata overhead

NATS itself writes RAFT entries for each replicated stream. Even at
idle, the meta-group and per-stream RAFT groups emit periodic
snapshots and heartbeats. Parti's default configuration creates these
streams (verify against `v2.3.0:manager_setup.go:76-96` for the KV
streams and storage classes hardcoded there):

| Stream / KV bucket | Storage class (v2.3.0 hardcoded) | Source |
|---|---|---|
| Election KV | Memory | `v2.3.0:manager_setup.go:76-96` |
| Heartbeat KV | Memory | `v2.3.0:manager_setup.go:76-96` |
| Stable-ID KV | **File** (parti-managed) | `v2.3.0:manager_setup.go:33-39` (`ensureStableIDKV` uses `jetstream.FileStorage`) |
| Assignment KV | File | `v2.3.0:manager_setup.go:76-96` |
| Handoff KV | File | `v2.3.0:manager_setup.go:76-96`, only when two-phase on |
| Data stream | (caller-managed) | external |

- **Predicted contribution:** constant per-node IOPS, independent of N,
  proportional to *number of streams*.
- **Ablation:** the no-Parti control run isolates this baseline.

### H5 — Open hypothesis: residual O(N) slope after H1/H2 ablated

If, after disabling two-phase handoff (H1.B) and switching to a
single-consumer mode (H2.B), there is still a measurable per-partition
slope, then the investigation has surfaced an unattributed O(N)
mechanism. The plan does not pre-commit a specific candidate; the data
defines what to chase. The v2.3.0 loop inventory in the appendix
suggests there is no other steady-state O(N) loop, so H5 firing would
be a real finding worth a follow-up plan.

## Independent variables — design

Independent variables, varied across the run matrix:

- **`N` (partition count):** {500, 1000, 2000, 3000}. Four points for
  slope fitting.
- **`R` (replicas):** primarily 3; one comparison point at 5.
- **`EnableTwoPhaseHandoff`:** {false (B-lib), true (B-prod)}. Drives
  whether H1 applies.
- **`SweepInterval`:** {30s (default), 5min}. Only meaningful when
  two-phase is true.
- **`FetchTimeout`:** {5s (default), 30s}. H2 cadence ablation.
- **`ConsumerMode`:** {Dynamic, Queue, none-attached}. H2 fan-out
  ablation. `none-attached` means no consumer module instantiated, so
  no per-partition pull consumers exist.
- **`HeartbeatInterval`:** {5s (default), 10s}. H3 floor ablation.
- **KV storage class:** {file (default), memory}. M5; applies to all
  parti-managed KV streams via harness pre-creation.
- **Data stream storage class:** {file, memory}. H2 storage-bound test.
- **Parti version:** v2.3.0 (primary); HEAD (one comparison run at
  N = 2000 only).

Dependent variables, measured per run:

- Per-node block I/O: read IOPS, write IOPS, bytes/sec.
- Cluster-level read-RPC counts (instrumented harness — see R3).
- Cluster-level write-mutation counts (instrumented harness).
- NATS server-level: per-stream messages/bytes, per-consumer state
  snapshots.
- Final stream storage classes (verified post-Start; M5 hygiene).

## Test rig

The investigation needs a knob-able, instrumented, **disposable**
N-replica NATS setup that mirrors the user's topology cheaply.

### R1 — NATS cluster topology

`test/perf-measurement/docker/` holds the docker-compose stack
(mirroring the existing `test/simulation/docker/` convention):

- 3 (and, for one comparison run, 5) `nats-server` containers, each
  with JetStream enabled, file storage, **fresh named volumes per run**
  (see "hygiene" below), all mounted at `/data` and placed on the same
  host filesystem so per-container block I/O can be observed
  independently (see R3 for the instrumentation paths).
- Cluster routes wired between all replicas; clients connect via a
  single `nats://` URL with all endpoints.

**Image and version are overridable via env vars** so the rig can run
against the user's production image (which may live in a private
registry) and any NATS server version they care about:

| Env var | Default | Purpose |
|---|---|---|
| `PERF_RIG_NATS_IMAGE` | `nats:2.12.6` | Full image reference, including registry + tag. Override to test a private-registry build or a different version (e.g. `private.registry.example.com/nats:2.11.0`). |
| `PERF_RIG_NATS_REPLICAS` | `3` | Number of NATS containers in the cluster. Set to `5` for the M1.10 R=5 comparison run. |

In the compose YAML:
```yaml
services:
  nats-1:
    image: ${PERF_RIG_NATS_IMAGE:-nats:2.12.6}
    ...
```

The harness reads `PERF_RIG_NATS_IMAGE` at run time and records the
**resolved digest** (not just the tag) in `manifest.yaml` via
`docker image inspect` so re-runs can be reproduced exactly. NATS
server version mismatches between the rig and the user's prod are
flagged in `findings.md`, not silently ignored — pull-consumer
state-persistence behaviour changed across NATS versions and this is
load-bearing for H2.

### R2 — Parti workload harness

`test/perf-measurement/cmd/harness/main.go` — a single binary that:

1. Connects to the NATS cluster.
2. **Pre-creates every Parti-managed KV bucket** as a JetStream stream
   with the desired storage class for this run. Parti's own setup
   honours an existing bucket and only warns on storage-class mismatch
   (`v2.3.0:manager_setup.go:137-165`), so harness-side pre-creation
   is the only way to control storage class on v2.3.0. Required buckets:
   election, heartbeat, stable-ID, assignment, handoff (if
   `EnableTwoPhaseHandoff=true`).
3. Pre-creates the **data stream** with the run-configured storage class.
4. Pre-populates the partition source (NATS-KV) with N partitions.
5. Spawns `--workers` (default 5) `parti.Manager` goroutines, each in
   its own NATS connection, with config built from the run knobs.
6. Each worker configures `consumer.Dynamic` / `consumer.Queue` (per
   run) with a no-op handler; or, for `consumer-mode=none-attached`,
   no consumer module at all.
7. **Verifies storage class** on every stream by issuing `stream info`
   against the cluster and aborts the run if any stream's actual
   storage class disagrees with the manifest.
8. **Waits for all workers `Stable` and not `Degraded`** before
   starting the capture window. Aborts if any worker is `Degraded`.
9. Holds for `--measure-window` (default 10 min) while measurement
   tools run.
10. Stops cleanly.

Flags expose each independent variable above (`--n`, `--replicas`,
`--two-phase`, `--sweep-interval`, `--fetch-timeout`, `--consumer-mode`,
`--heartbeat-interval`, `--kv-storage`, `--data-storage`,
`--parti-version`).

Build target: `make -C experiments harness`.

### R3 — Instrumentation

Per measurement run, the rig captures, with their *budget column*.
Disk IOPS is observed from **three independent sources** so we can
cross-check attribution: cgroup v2 io.stat (per-container, primary),
host iostat (secondary), and node_exporter (host sanity).

| Source | Tool | Captures | Column |
|---|---|---|---|
| **cgroup v2 `io.stat` (primary)** | small Go/bash poller at 1 Hz reading `/sys/fs/cgroup/system.slice/docker-<id>.scope/io.stat` for each NATS container; format `MAJ:MIN rbytes=… wbytes=… rios=… wios=…`; diff between consecutive samples = ops/s per container per device | True per-container IOPS, reads/writes separated, attributable to a specific NATS node | block_read_iops, block_write_iops |
| Host block I/O per node volume (secondary) | `iostat -x -d -t 1` for each container's volume device | Same numbers from a different vantage point; cross-check against cgroup data | block_read_iops, block_write_iops |
| Host-level Prometheus (sanity) | `node_exporter` running on the host, scraped at 5 Hz; metrics `node_disk_reads_completed_total{device=…}`, `node_disk_writes_completed_total{device=…}` | Sums of the cgroup data across containers; should match within ~1% | sanity |
| Per-process I/O inside container | `pidstat -d 1 -p $(pgrep nats-server)` | Another cross-check vs iostat; useful when one process per container | block_*_iops |
| **Harness-injected `jetstream.JetStream` wrapper** | counter middleware on the `JetStream` and `KeyValue` interfaces, classifying every call by KV bucket name | True per-subsystem RPC counts: `Put` / `Get` / `Keys` / `Watch` / `Create` / `Update` / `Delete` per bucket | read_rpc_ops, write_mutation_ops |
| Per-stream message/byte counters | `nats stream info <name> --json` snapshotted every 5 s | Cross-check: total messages should equal sum of harness-counted writes | write_mutation_ops |
| NATS server `varz` | `curl :8222/varz` polled every 5 s | In/out msgs/bytes, slow consumers, server-side total ops | sanity / debugging |
| NATS server `jsz` | `curl :8222/jsz?streams=true&consumers=true` polled every 5 s | Per-stream / per-consumer state snapshot | sanity / debugging |

**Why a JetStream wrapper, not a ClaimStore wrapper:** v2.3.0 has no
public option to inject a `handoff.ClaimStore`; the manager constructs
`handoff.NewNATSClaimStore` internally at
`v2.3.0:manager_setup.go:92-117` and the available `Option`s
(`v2.3.0:options.go:8-16,32-35,57-60,77-80,97-100,115-118,168-171`)
do not include a ClaimStore hook. Wrapping at the
`jetstream.JetStream` boundary works because every parti subsystem
reaches NATS through the JetStream handle the caller passes to
`parti.NewManager` (`v2.3.0:manager.go:173-181,219-228`) and the
consumer module reaches NATS through the JetStream handle passed to
`consumer.NewDynamic` / `consumer.NewQueue`
(`v2.3.0:consumer/dynamic.go:164-230`, `v2.3.0:consumer/queue.go:172-230`).

Implementation sketch (harness side):

- Implement a thin wrapper type `instrumentedJS` that embeds the real
  `jetstream.JetStream` and overrides `KeyValue` / `CreateKeyValue` /
  `CreateOrUpdateKeyValue` / `Stream` to return wrapped handles.
- The wrapped `KeyValue` increments counters tagged by `(bucket_name,
  op_name)` on every method call. The bucket name is known at wrap
  time, so attribution to a parti subsystem (election / heartbeat /
  stable-ID / assignment / handoff) is by bucket name in the wrapper
  itself.
- The harness passes this wrapper into `parti.NewManager` and into
  the consumer constructors. No parti changes required.
- A periodic counter dump (1 Hz) goes to `rpc_counts.csv`.

**Coverage caveat — `source.NatsKV`:** the public partition source
`source.NewNatsKV` accepts a `jetstream.KeyValue` directly rather than
a `JetStream` (`v2.3.0:source/nats_kv.go:49-60,63-105,176-207`), so its
Get/Watch traffic does **not** flow through the manager's wrapped
JetStream. The harness MUST construct the partition-source `KeyValue`
by calling the wrapped JetStream's `KeyValue(...)` method itself and
hand the resulting wrapped handle to `source.NewNatsKV`, so source
calls are counted under the partition-source bucket. The partition
source is idle in steady state (one key, watched, no per-tick traffic
beyond watcher heartbeats), so this is mostly a constant-floor
attribution concern, but the wiring is required for the rpc_counts
totals to be complete.

**Read-RPC counting:** `nats server report jsz` does **not** expose
per-stream Get/Keys RPC rates. The wrapper above is the source of
truth for read RPC attribution; server endpoints are sanity
cross-checks only.

All outputs land under `results/run-NNN/` with `manifest.yaml` (parti
version, NATS server version, full config, partition count, replica
count, wall-clock window, storage classes confirmed). The aggregator
script (`test/perf-measurement/cmd/aggregate/main.go`, Go for parity
with the rest of the rig) produces a per-run CSV:
`(t_s, node, iops_read, iops_write, bytes_read, bytes_write,
rpc_read_<bucket>, rpc_write_<bucket>, stream_msgs_<name>, ...)`.

### R4 — Run hygiene

Each measurement run is preceded by:

- `docker compose down -v` to drop volumes (fresh JetStream state).
- A fresh NATS server start with no prior stream / KV state.
- Harness pre-creates every bucket / stream and verifies storage
  classes match the manifest.
- Harness waits for `Stable` on every worker; aborts if any
  `Degraded`. This guards against the v2.3.0 degraded-mode recovery
  path (`CHANGELOG.md:147-161`) accidentally being exercised during a
  measurement.
- 5 min warmup window (initial bootstrap, watcher startup, claim
  resolver `warm` `:200-244` which is also O(N) but startup-only).
- 10 min capture window.

The plan **excludes startup-only O(N) work** from the steady-state
budget: handoff hygiene (`v2.3.0:manager_setup.go:92-123`,
`v2.3.0:manager_handoff.go:93-140`), handoff resume
(`:145-196`), and resolver `warm`. Warmup IOPS is recorded in
`manifest.yaml` for context but not used to fit the slope.

### R5 — Replication, statistics, and minimum detectable effect

A single 10 min capture is **not** sufficient given docker storage-driver
noise. Each (config, N) cell requires:

- **≥5 independent capture runs** (n=5, not n=3 — see below), each
  with R4 hygiene from scratch.
- Each capture window is split into **6 × 100 s windows** at the
  aggregator step. Per-window mean IOPS is one observation; per-run
  IOPS is the mean of its 6 windows. This gives 30 observations per
  (config, N) cell at the window level for variance estimation, and 5
  observations at the run level for between-run reproducibility.

**Why n=5 over n=3:** with n=3 replicates and a sample-derived mean
and SD, the "±2 SD outlier rule" almost never fires (the rejected
point dominates its own SD). With n=5, between-run SD is estimable
and the outlier rule has a chance to flag a true one-off. If between-
run SD exceeds 30 % of the slope effect anyway, raise to n=10 before
proceeding.

**Slope estimator and MDE:**

- For each column independently, fit `y_ij = β₀ + β₁ × N_i + ε_ij`
  where `i` ∈ {N points} and `j` ∈ {runs} via OLS on all replicate
  observations (not per-N means). This yields slope β̂₁ and standard
  error `SE(β̂₁)`.
- 95 % CI on slope: `β̂₁ ± t₀.₉₇₅,df × SE(β̂₁)` where `df = n_total - 2`.
- **MDE** for slope is `MDE_slope = t₀.₉₇₅,df × SE_no_Parti(β̂₁)`,
  where `SE_no_Parti(β̂₁)` is the OLS slope SE computed against the
  no-Parti control (M1.0) replicates at every N point (the no-Parti
  cluster doesn't depend on N, so the "slope" of the no-Parti control
  is the rig's noise-induced slope). This is the smallest per-partition
  slope the rig can reliably distinguish from noise.
- An ablation is "verified moving the slope" when
  `|β̂₁_ablation - β̂₁_baseline| > MDE_slope` and the sign matches the
  prediction.

**Pre-registered outlier rule (executed only after all runs complete,
not iteratively):** for each (config, N) cell, compute Tukey fences
(median ± 1.5 × IQR) over the 5 run-level means. Any run-mean outside
those fences is excluded; the excluded run is recorded in
`manifest.yaml` and **re-run once**. If the re-run also lies outside
fences, the cell is flagged "noisy" and excluded from the slope fit
with a note in `findings.md`.

The MDE replaces the prior unjustified ±15 % attribution threshold.

## Methodology

### M1 — Run matrix

Each cell = **5 replicates** unless noted. Replicate handling is
specified in R5 above. Reps under "noisy" cells are extended to 10
following the R5 outlier rule.

```
M1.0  No-Parti control            — NATS cluster only, idle. Defines MDE.
                                    N ∈ {500, 1000, 2000, 3000} × 5 reps each
                                    (no-Parti slope must be ≈ 0).

M1.1  B-lib baseline              — v2.3.0, default config
                                    (EnableTwoPhaseHandoff = false),
                                    Dynamic consumer.
                                    N ∈ {500, 1000, 2000, 3000} × 5 reps.

M1.2  B-prod baseline              — same as M1.1 but
                                    EnableTwoPhaseHandoff = true.
                                    N ∈ {500, 1000, 2000, 3000} × 5 reps.

M1.3  H1.A — Sweep interval         — B-prod, SweepInterval = 5 min.
                                    Same Ns × 5 reps.

M1.4  H1.B — Two-phase off          — B-prod, EnableTwoPhaseHandoff = false,
                                    HandoffBucket populated (unused).
                                    [Same Ns × 5 reps. Note: same flag
                                     setting as M1.1 but flagged
                                     explicitly so the ablation is
                                     named, not inferred. M1.4 and
                                     M1.1 are expected to coincide;
                                     if they don't, that itself is a
                                     finding.]

M1.5  H2.A — Fetch timeout          — B-prod, FetchTimeout = 30s.
                                    Same Ns × 5 reps.

M1.6  H2.B — Queue consumer         — B-prod, consumer.Queue.
                                    Same Ns × 5 reps.

M1.7  H2.C — Data stream memory     — B-prod, data stream Storage = memory.
                                    Same Ns × 5 reps.

M1.8  H3 — Heartbeat interval       — B-prod, HeartbeatInterval = 10 s.
                                    N = 2000 only, × 5 reps.

M1.9  M5 — KV storage class         — B-prod, all parti KV buckets
                                    pre-created with Storage = memory.
                                    Includes handoff bucket.
                                    N ∈ {1000, 2000, 3000} × 5 reps.

M1.10 R=5 comparison                — B-prod, replicas = 5. N = 2000 × 5.

M1.11 HEAD comparison               — HEAD parti, B-prod, default knobs.
                                    N = 2000 × 5 reps.
```

Each measurement run is 5 min warmup + 10 min capture = 15 min, plus
~2 min for `docker compose down -v` + cluster start + storage
verification = ~17 min wall clock per run.

**Total run count:** M1.0–M1.11 contain 190 runs at n=5:

```
M1.0   4 N × 5 reps =  20    (no-Parti control)
M1.1   4 N × 5 reps =  20    (B-lib baseline)
M1.2   4 N × 5 reps =  20    (B-prod baseline)
M1.3   4 N × 5 reps =  20    (sweep-interval ablation)
M1.4   4 N × 5 reps =  20    (two-phase off ablation)
M1.5   4 N × 5 reps =  20    (fetch-timeout ablation)
M1.6   4 N × 5 reps =  20    (queue consumer ablation)
M1.7   4 N × 5 reps =  20    (data stream memory ablation)
M1.8   1 N × 5 reps =   5    (heartbeat interval ablation)
M1.9   3 N × 5 reps =  15    (KV storage class)
M1.10  1 N × 5 reps =   5    (R=5 comparison)
M1.11  1 N × 5 reps =   5    (HEAD comparison)
                       ---
total                 190 runs
```

**Wall-clock budget:** 190 × 17 min ≈ **54 hours** of main-matrix
capture time. M4 microbenchmarks add: ~1 hour for the basic
KV-paths table (6 rows × 5 min each + setup), plus M4.1's 48 grid
points at ~3 min each (60 s capture + cluster cycle) ≈ ~2.5 hours. So
M4 totals ~3.5 hours. Plus 10–20 % headroom for re-runs (R5 outlier
rule). Plan for **~70 hours** on a single host. Two parallelisation
options:

- **Single host, sequential:** ~10 working days at ~7 hr/day.
- **Sharded across hosts:** split the matrix across H hosts, but each
  host MUST run its own M1.0 no-Parti control and confirm
  `MDE_slope_host ≈ MDE_slope_other_hosts` within the inter-host
  envelope (compare via two-sample t-test on the no-Parti slope
  estimates). Sharding is only valid when host pooling passes; document
  the host pool in `manifest.yaml`.

**Run scheduling — randomised, not lexicographic:**

The aggregate matrix is **180+10 runs** that span all (config, N)
cells. Lexicographic execution (`all N=500, then all N=1000, ...`)
aliases host thermal drift / background-process noise / disk-cache
warmth into the fitted slope — including the no-Parti control's slope
that drives MDE. **Pre-register a randomised run order across (config,
N, rep)** generated once at the start of the campaign with a recorded
seed. Each run's `manifest.yaml` records its position in the random
schedule. Tukey-fence outliers' re-runs are inserted at a new random
position, not re-played in place.

### M2 — Slope fit and statistical reporting

The estimator and MDE are defined in R5. For each (config) tuple, the
aggregator produces:

- Slope β̂₁ and 95 % CI (per column).
- Intercept β̂₀ and 95 % CI.
- Residual SD per N point.
- Verdict relative to MDE: `slope > MDE`, `slope ≈ baseline`, or
  `noisy/inconclusive`.

R² is reported as a diagnostic (high R² → linear fit is appropriate;
low R² → re-examine residuals for non-linearity) but is not used as a
gate.

### M3 — Attribution

The four budget columns are **not** all attributable from the JetStream
wrapper alone. The wrapper sees client-side KV / JetStream RPCs but
**not** server-internal state mutations like JetStream consumer-state
persistence — which is exactly H2's load-bearing unknown. M3 therefore
defines the attribution per column using two distinct attribution
sources: the wrapper (`rpc_*`) and the M4 calibration table
(`bench_*`).

For each cell of the run matrix, compute:

```
# Client-RPC columns (fully derived from the wrapper)
read_rpc_ops/s          = Σ (wrapper-counted reads per bucket)
write_mutation_ops/s    = Σ (wrapper-counted writes per bucket)

# Block-IO columns (derived from the M4 calibration table)
block_read_iops_kv      = Σ_bucket  M4_R_read[bucket, op]  × rpc_read_per_bucket[bucket, op]
block_write_iops_kv     = Σ_bucket  M4_R_write[bucket, op] × rpc_write_per_bucket[bucket, op]

# H2 server-internal contribution — derived from M4.1's idle-pull
# consumer calibration. Arguments are per-node-role and per-run, with
# C_stream depending on the run's consumer mode (Dynamic=N, Queue=1,
# none-attached=0); see M4.1 for the calibration grid and the full
# lookup rule.
block_write_iops_h2(node, run) = M4_idle_pull_iops_write(C_stream(run), FetchTimeout, data_storage, R, node_role(node))
block_read_iops_h2(node, run)  = M4_idle_pull_iops_read (C_stream(run), FetchTimeout, data_storage, R, node_role(node))

# Final attributed totals (per node)
block_read_iops         = block_read_iops_kv  + block_read_iops_h2
block_write_iops        = block_write_iops_kv + block_write_iops_h2

# Residual (per column)
residual_<column>       = measured_<column> − attributed_<column>
```

A few consequences worth being explicit about:

- **`write_mutation_ops/s` is intentionally KV-only.** Server-internal
  consumer-state transitions are NOT client-visible writes and are
  excluded from this column. H2's block-write contribution is captured
  via `block_write_iops_h2` directly, not by inflating
  `write_mutation_ops/s`.
- **M4's idle-pull subtest is the only way to fold H2 into the budget.**
  If H2's contribution is small (server uses memory-only consumer
  state), the calibration table reports near-zero for the relevant
  combinations and the H2 term vanishes naturally.
- **H2 verification is two-pronged.** First, the ablations
  (M1.5 / M1.6 / M1.7) show the slope-side response to H2 knobs. Second,
  the calibration-based attribution closes the budget arithmetically.
  If the ablations move the slope but the calibration-based attribution
  underestimates the predicted move, the gap itself is information:
  the calibration may be undersized for the real run conditions, or H2
  has a non-linear-in-C component.

Residual that **grows with N** is the H5 signal. With H2 explicitly
folded in, a growing residual cannot be silently misclassified as a
new mystery loop when it is actually undersized H2 calibration.

### M4 — Disk amplification calibration (per path)

Single-rate microbenchmarks at the start of the campaign, against the
idle rig, on the **production NATS server version** and storage class:

| Path | Driver | Records |
|---|---|---|
| KV `Put` (file, R3) | `nats kv put` at 100 ops/s × 60s | bytes/op, block_write_iops/op |
| KV direct `Get` (file, R3, warm) | `nats kv get` at 100 ops/s × 60s | block_read_iops/op |
| KV `Keys` over K keys (file, R3) | repeated `kv.Keys()` over a pre-populated bucket, K ∈ {1000, 3000} | block_read_iops per Keys call; check whether Keys cost is O(K) or amortised |
| KV watch fan-out | bucket with W ∈ {1, 5} watchers, drive 100 puts/s on watched key | additional iops/op per watcher |
| Idle pull consumer (parameterised — see M4.1) | see M4.1 below | per-grid-point `(block_read_iops_per_node, block_write_iops_per_node)` |
| KV `Put` (memory, R3) | same as first, but Storage = memory | should be near-zero |

Each microbenchmark records: read IOPS, write IOPS, bytes, NATS server
version, storage class, replication factor, stream leader placement
(via `stream info`). The output is a calibration table mapping each
RPC/state path to a per-op IOPS factor with confidence bounds.

#### M4.1 — Idle pull consumer calibration grid

M3 attributes H2 server-internal block I/O via
`M4_idle_pull_iops_{read,write}(C_stream, FetchTimeout, data_storage, R, node_role)`.

The key insight that defines the grid: a JS pull consumer's state
lives on its **stream's leader** in R=k mode. All consumers on one
stream contribute their write I/O on the leader node only; follower
nodes see only replication traffic. So per-node block I/O is a
function of how many consumers the stream on this node leads, not the
cluster total. The calibration therefore reports two per-node curves:
one for the **leader** of the data stream and one for each **follower**.

**Grid:**

| Arg | Values | Why |
|---|---|---|
| `C_stream` (consumers on the single calibration data stream, all hosted on its leader) | `{0, 100, 500, 1000, 2000, 3000}` | Spans the matrix N range. |
| `FetchTimeout` (pull expiry) | `{5s, 30s}` | H2.A ablation; default 5 s (`v2.3.0:consumer/options.go:181-190,341-348`); iterator passes it (`v2.3.0:internal/durable/partition_consumer.go:142-199`) and maps to `PullExpiry`+`PullHeartbeat=expiry/2` (`v2.3.0:internal/durable/worker_consumer.go:723-732`). |
| `data_storage` | `{file, memory}` | H2.C ablation flips this. |
| `R` (replication factor) | `{3, 5}` | M1.10 uses R=5. |

That's 6 × 2 × 2 × 2 = **48 grid points**. Each is a 60 s capture
recording `(block_read_iops, block_write_iops)` per node, then
classified into two rows by `node_role`:

- **`leader`** — the node that is the calibration data stream's
  current leader at capture time.
- **`follower`** — every other node in the cluster. All followers
  are averaged into a single row per grid point (per-follower
  iostat readings are also stored for variance).

For each grid point the harness:

1. Creates a fresh data stream with the configured `R` and
   `data_storage`.
2. Creates `C_stream` durable pull consumers on it (no parti
   involvement), each running a no-op pull-iterator loop with the
   configured `FetchTimeout`.
3. Identifies the stream's leader node via `stream info | jq .cluster.leader`
   and records it in the per-grid-point manifest.
4. Captures 60 s of iostat per node.
5. Stores `(C_stream, FetchTimeout, data_storage, R, node_role,
   block_read_iops, block_write_iops, ci_low, ci_high)`.

If leadership moves during a capture, the grid point is discarded and
re-run. (Acceptable rate is < 5 % of grid points; if higher, pin
leadership manually with `nats stream cluster step-down`.)

**M3 lookup rule (consistent with the calibration semantics):** the
production matrix uses a single data stream; the number of pull
consumers on it depends on `ConsumerMode`. At measurement time the
harness records the data stream's leader, and M3 maps `C_stream` from
the run's consumer mode:

```
node_role(this_node, run)   = leader     if this_node == stream_leader(run)
                              follower   otherwise

C_stream(run) = N    if ConsumerMode == Dynamic   (one durable per partition;
                                                  v2.3.0:internal/durable/worker_consumer.go:125-150,447-460)
              = 1    if ConsumerMode == Queue     (single shared durable;
                                                  v2.3.0:consumer/queue.go:172-190,333-352)
              = 0    if ConsumerMode == none-attached
                                                  (no consumer module instantiated)

block_*_iops_h2(this_node, run) = M4_idle_pull_*(C_stream(run), FetchTimeout,
                                                 data_storage, R,
                                                 node_role(this_node, run))
```

`C_stream = 0` returns the M4.1 baseline at zero consumers (stream
exists but no consumers attached), so the H2 term vanishes naturally
for `none-attached` cells. For Queue cells the calibration lookup at
`C_stream = 1` falls between the grid's `0` and `100` rows and is
resolved by the interpolation rule below; if implementers want to
measure Queue's near-baseline case directly, add `1` to the grid.

If the matrix is later extended to multiple data streams (not in this
plan), the lookup generalises to a sum over streams of
`M4_idle_pull_*` per stream, each with its own `C_stream`.

Outputs of M4.1: a CSV with columns
`(C_stream, FetchTimeout, data_storage, R, node_role,
block_read_iops, block_write_iops, ci_low, ci_high)`,
suitable for M3's linear / piecewise-linear interpolation.

**Interpolation rule:** for off-grid `C_stream` (e.g. 600), M3 uses
linear interpolation between bracketing grid points. If the M4.1 data
shows materially non-linear behaviour on any slice (visible curvature
in `block_*_iops` vs `C_stream`), add intermediate grid points before
running M3.

#### M4 calibration must precede M3

Microbenchmarks must run before the main matrix, on the same rig
(otherwise the conversion factors are inapplicable). M4.1 in
particular must complete before any H2-relevant run (M1.5 / M1.6 /
M1.7 / M1.10) so M3 has the table available for attribution.

### M5 — Memory-KV control

For M1.9, the harness pre-creates every Parti KV bucket with
`Storage: Memory` *before* `Manager.Start`. Without harness
pre-creation v2.3.0 has no public API for storage class
(`v2.3.0:manager_setup.go:76-96` hardcodes some buckets to file). Run
hygiene requires the harness to confirm the realised storage class on
each stream via `stream info` and abort on mismatch.

If the user's "memory KV didn't move IOPS" observation reproduces here
*despite* all Parti KV streams being memory-backed:

- And the **data stream** is still file-backed → H2 is the explanation
  (per-partition consumers are the source). H2.C cross-checks this.
- And the data stream is also memory-backed → either the metric is
  op-count-flavoured (not block IOPS) or there is a NATS-internal
  source we haven't named. Confirms an actionable diagnostic for the
  user regardless.

## Verification of candidate mitigations

After attribution, the slope-reducing knobs we expect to recommend (if
the corresponding hypothesis confirms):

1. **`Handoff.SweepInterval = 5 min`** — H1.A. Cheapest mitigation.
2. **`parti.Config.EnableTwoPhaseHandoff = false`** — H1.B. Removes
   sweeper entirely. Only viable for users who don't need atomic
   handoff (the v2.3.0 changelog notes two-phase is opt-in safety, not
   the default).
3. **`FetchTimeout = 30s`** — H2.A. Trades higher latency floor for
   reduced consumer-state churn.
4. **`consumer.Queue` over `consumer.Dynamic`** — H2.B. Only viable if
   the user's workload tolerates round-robin instead of partition-key
   affinity.
5. **Code change in `maybeSweepClaims`** — replace `ListKeys + N×Get`
   with watcher-snapshot replay. **Not in this plan;** a follow-up
   plan is opened only if H1 confirms and the operator cost of the 5 min
   sweep is unacceptable.

A mitigation is "verified" when its measured slope difference from
the B-prod baseline exceeds `MDE_slope` (defined in R5) and the sign
matches the prediction.

## Implementation strategy

See [`01-implementation-strategy.md`](01-implementation-strategy.md) for
the phased build plan, including suggested model + effort per phase
(Sonnet 4.6 medium for boilerplate, Opus 4.7 high for the JetStream
wrapper and aggregate.py, Opus 4.7 xhigh for the findings
interpretation) and review checkpoints (`/post-impl-review` after the
wrapper, aggregator, and M4.1 driver land).

## Deliverables (file-level)

Planning artefacts live under `docs/plans/`; the runnable rig and run
artefacts live under `test/` (mirroring `test/simulation/`). The split
keeps `docs/` to `.md` only and lets the rig grow without bloating
the docs tree.

**Planning artefacts:**

```
docs/plans/iops-investigation/
├── README.md                              (done)
├── 00-attribution-plan.md                 (this file)
├── 01-implementation-strategy.md          (done — phased build + models)
├── reviews/                               (optional; review reports)
└── findings.md                            (TODO — written after enough runs)
```

**Runnable rig (built in Phase 1 onwards):**

```
test/perf-measurement/
├── README.md                              (TODO — how to run the rig)
├── Makefile                               (TODO — make up/down/reset/image-digest)
├── go.mod                                 (TODO — separate module so the rig
│                                           can pin parti v2.3.0 for the main
│                                           matrix and HEAD for M1.11)
├── docker/
│   ├── docker-compose.yaml                (TODO — image overridable via
│   │                                       PERF_RIG_NATS_IMAGE; replicas via
│   │                                       PERF_RIG_NATS_REPLICAS)
│   └── nats-server.conf                   (TODO)
├── cmd/
│   ├── harness/main.go                    (TODO — workload binary)
│   ├── calibrate/main.go                  (TODO — M4 / M4.1 driver)
│   └── aggregate/main.go                  (TODO — per-run CSV; reconciles
│                                           cgroup, iostat, jsz, harness counters)
├── internal/
│   ├── instrumentedjs/                    (TODO — jetstream.JetStream +
│   │                                       jetstream.KeyValue wrapper with
│   │                                       per-(bucket, op) counters)
│   └── storageverify/                     (TODO — stream storage assertion)
├── scripts/
│   ├── capture-cgroup-io.sh               (TODO — 1 Hz io.stat poller, primary)
│   ├── capture-iostat.sh                  (TODO — secondary host-level cross-check)
│   ├── capture-jsz.sh                     (TODO — NATS server stats)
│   ├── prometheus-node-exporter.yaml      (TODO — host-level sanity)
│   └── run-matrix.sh                      (TODO — drives M1.0–M1.11)
├── results/                               (gitignored; one subfolder per run)
│   └── run-NNN-<label>/
│       ├── manifest.yaml                  (parti+NATS version, full config,
│       │                                   confirmed storage classes)
│       ├── cgroup_io.raw                  (primary IOPS source per container)
│       ├── iostat.raw                     (secondary)
│       ├── node_exporter.prom             (host-level sanity)
│       ├── jsz.raw
│       ├── stream_info.snapshots.json
│       ├── rpc_counts.csv                 (from instrumented wrappers)
│       ├── aggregated.csv
│       └── notes.md
└── .gitignore                             (TODO — results/)
```

## Risks and known unknowns

- **Docker storage driver IOPS noise.** overlay2 / btrfs amplify writes
  vs bare-metal ext4. Mitigation: named local volumes on bind-mounted
  ext4 directory; document host filesystem in each run's manifest.
  M1.0 sets the noise floor.
- **iostat vs PVC metric mismatch.** The user's metric may include
  reads-counted-as-IOPS. The four-column budget exposes which signal
  matches.
- **R=3 fan-out per-node.** Each write hits all 3 replicas; per-node
  IOPS = leader-IOPS + 2 × follower-IOPS-share. Reads hit only the
  leader's local state. The predictions split read vs write, so this
  asymmetry is preserved.
- **NATS server version.** Pull-consumer state-persistence behavior
  changed across NATS server versions. **Pinned in `manifest.yaml`**;
  the version must match the user's prod or the discrepancy is logged.
- **Harness fidelity vs. real consumers.** A no-op handler isn't
  identical to no consumer. The `consumer-mode=none-attached`
  ablation isolates this.
- **Two-phase handoff is opt-in on v2.3.0.** The user's observed curve
  is itself the strongest hint that they have it enabled; B-prod is
  the right starting point, but the user must confirm.
- **Storage class is not Parti-public on v2.3.0.** All M5 / M1.9 work
  depends on harness pre-creation. If the harness doesn't get this
  right, M5 silently confounds.
- **Stream leader placement may move during a run.** Per-node IOPS
  predictions assume static leadership. The capture window includes a
  `stream info` snapshot at start and end; runs with leader movement
  are flagged.
- **MDE may be larger than expected slope.** If the rig noise floor
  exceeds the per-partition slope, the experiment is undersized. M1.0
  is the first run for this reason; if MDE > 0.05 IOPS/partition the
  rig needs tuning (longer capture windows, more replicates, dedicated
  hardware) before the matrix is worth running.

## Success criteria (restated)

1. A no-Parti control (M1.0) that establishes `SE_no_Parti(β̂₁)` and
   `MDE_slope` such that `MDE_slope < expected H1 read-RPC slope`.
   With 5 workers and `SweepInterval = 30s`, H1 predicts a cluster
   read-RPC slope of `5 / 30 = 0.167 RPC/s/partition` on the
   handoff bucket (`v2.3.0:internal/assignment/handoff/twophase.go:41-49,398-418`).
   If `MDE_slope` exceeds that, raise n, lengthen captures, or use a
   quieter host before running the matrix. Per-column MDEs are
   established the same way independently.
2. B-prod (M1.2) reproduces the user's curve on `block_write_iops`
   such that the slope difference vs. the user's observed slope is
   within `MDE_slope`, or the discrepancy is explained (storage class,
   NATS version, etc.).
3. Per-subsystem attribution accounts for the measured slope and floor
   in each of the four budget columns within the cell's residual SD
   (the column residual is itself smaller than the cell's
   `2 × SE(β̂₁)`).
4. At least one mitigation (H1 ablation) with measured slope reduction
   exceeding `MDE_slope` in the predicted direction.
5. Written `findings.md` records the per-subsystem IOPS budget,
   confirmed and falsified hypotheses, and the recommended operator
   actions, with raw data linked.

## Out of scope

- Code changes to Parti.
- Multi-region / WAN replication cost.
- Application-side message throughput.
- `consumer.Broadcast`. Only Dynamic / Queue covered.

## Appendix — v2.3.0 background-loop inventory

This is the authoritative loop list against which H5 is checked. Built
from `git ls-tree v2.3.0` plus per-file inspection of `time.NewTicker`
and long-lived `go func` constructs in non-test code.

| Loop | Scope | Cadence / trigger | Per-tick cost | Hypothesis |
|---|---|---|---|---|
| `twoPhaseCoordinator.maybeSweepClaims` (`v2.3.0:internal/assignment/handoff/twophase.go:41-49,380-418`) | Every manager when `EnableTwoPhaseHandoff=true` | `Handoff.SweepInterval`, default 30s | 1 `ListKeys` + N `Get`; `PutIfEpoch` only for expired non-stable claims | **H1** |
| `WorkerConsumer` per-partition pull loop (`v2.3.0:internal/durable/worker_consumer.go:148-150,433-460,723-732`; `v2.3.0:internal/durable/partition_consumer.go:118-199`) | One loop per assigned partition | Pull iterator over `FetchTimeout`+`PullHeartbeat` | Server-side pull state churn; no client RPC at idle | **H2** |
| Heartbeat publisher (`v2.3.0:internal/heartbeat/publisher.go:141-153,198-236`) | Every manager | `HeartbeatInterval`, default 5s | 1 KV `Put` per worker | **H3** |
| Stable-ID renewal (`v2.3.0:internal/stableid/claimer.go:253-306`) | Every manager after ID claim | `max(WorkerIDTTL/3, 100ms)`, default ~25s | 1 KV `Put` per worker | **H3** |
| Leader election renew/poll (`v2.3.0:manager_election.go:102-190`) | Every manager | `ElectionTimeout/3`, default ~3.33s | 1 `Update` (leader) or `Create` (follower) per tick | **H3** |
| Leader `WorkerMonitor` heartbeat poll (`v2.3.0:internal/assignment/worker_monitor.go:180-202`, `:135-176`) | Leader only | `HeartbeatTTL/2`, default 7.5s | 1 `Keys()` on heartbeat bucket (no per-worker `Get`) | **H3** |
| Leader `WorkerMonitor` heartbeat watcher (`v2.3.0:internal/assignment/worker_monitor.go:185-191,230-315`) | Leader only | Watch event-driven | Per heartbeat event | **H3** |
| Per-worker assignment watcher (`v2.3.0:manager.go:387-389`; `v2.3.0:manager_assignment.go:263-333`) | Every manager | Watch event-driven; idle-quiet | None at idle | **H4 / floor** |
| `partitionConsumer.Drain` (`v2.3.0:internal/durable/partition_consumer.go:285-315`) | Per removed subject | 50ms while draining | `Consumer.Info` polling | Not steady idle |
| Manager handoff startup hygiene (`v2.3.0:manager_setup.go:92-123`, `v2.3.0:manager_handoff.go:93-140`) | Two-phase enabled, startup only | once | 1 `ListKeys` + up to N `Get` | Warmup only (excluded) |
| Manager handoff resume (`v2.3.0:manager_handoff.go:145-196`) | Resumable handoff, startup only | once | 1 `ListKeys` + up to N `Get`; possible `PutIfEpoch` | Warmup only |
| `ClaimBasedResolver.warm` (`v2.3.0:internal/durable/claim_resolver.go:200-244`) | Dynamic consumer with processing gate, startup only | once | `Keys` + N `Get` | Warmup only |
| `ClaimBasedResolver.processWatcher` (`v2.3.0:internal/durable/claim_resolver.go:247-300`) | Dynamic consumer with processing gate | Watch event-driven; 5ms batch timer | None at idle (no claim mutations) | **H4 / floor** |
| NATS connection monitor (`v2.3.0:manager_degraded.go:10-30`) | Every manager | 1s | No KV op when healthy | **H4 / floor** |
| Degraded alert monitor (`v2.3.0:manager_degraded.go:297-329`) | Only while degraded | 1m | Metrics only | Not idle steady state |
| Recovery grace timer (`v2.3.0:manager_degraded.go:247-267`) | Leader after exiting degraded | one-shot | No NATS op | Not idle |
| Initial assignment wait (`v2.3.0:manager_election.go:248-282`) | Startup | 100ms until assignment | `fetchAssignment` per tick | Warmup only |
| Calculator partition monitor (`v2.3.0:internal/assignment/calculator.go:245-250,536-568,889-909`) | Leader only, watchable source | Partition source events | Rebalance; O(N) per source update, not per idle tick | Not idle steady state |
| StateMachine scaling timer (`v2.3.0:internal/assignment/state_machine.go:186-218`) | Leader during scaling | One-shot | Triggers rebalance | Not idle |
| `source.NatsKV.watchLoop` (`v2.3.0:source/nats_kv.go:92-104,212-260`) | Managers using NATS-KV source | Partition source events | Decode + notify; O(N) per source update | Not idle |
| `consumer.Queue.runLoop` (`v2.3.0:consumer/queue.go:355-409`) | One per Queue consumer | Iterator expiry/errors | One shared durable pull loop | **H2.B ablation** |
| `consumer.Static` (`v2.3.0:internal/ipartition/consumer.go:247-310`) | One per Static consumer | Iterator expiry/errors | One durable pull loop | Not the Dynamic baseline |

**Confirmed by review:** there is no other steady-state O(N partitions)
loop in v2.3.0 library code comparable to `maybeSweepClaims`. The
closest non-H1 O(N) candidates are startup-only (handoff hygiene,
resume, resolver warm) and the total N per-partition pull loops
covered by H2.
