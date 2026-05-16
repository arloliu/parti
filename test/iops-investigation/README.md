# IOPS Investigation Rig

Docker-compose based NATS JetStream cluster for the Parti IOPS attribution
investigation. See `docs/plans/iops-investigation/` for the measurement plan.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `IOPS_RIG_NATS_IMAGE` | `nats:2.12.6` | Full image reference, including registry and tag. Override to test a specific NATS version or a private-registry build. |
| `IOPS_RIG_NATS_REPLICAS` | `3` | Cluster size. Set to `5` for the M1.10 R=5 comparison run. |

## Bring the rig up and down

```bash
# Start a 3-node cluster (default)
make up

# Start a 5-node cluster (M1.10)
IOPS_RIG_NATS_REPLICAS=5 make up

# Stop all containers (no volume removal)
make down

# Full reset — drop volumes and restart fresh (required between measurement runs)
make reset

# Print the resolved image digest for manifest.yaml
make image-digest
```

Run `make reset` at the start of every measurement run to guarantee fresh
JetStream state. Partial teardowns leave stale stream metadata and will
confound measurements.

## Connecting to the cluster

Only `nats-1` exposes host ports. Use `nats://localhost:4222` as the single
connection URL. NATS route discovery propagates all three (or five) node
addresses to clients automatically.

Monitoring: `http://localhost:8222/jsz`

## "Done means" check (Phase 1 smoke test)

After `make reset`, confirm the cluster is healthy and has no streams:

```bash
curl -sf http://localhost:8222/jsz | python3 -m json.tool | grep '"streams"'
# Expected: "streams": 0,
```

If the `nats` CLI is available, the equivalent check is:
```bash
nats stream ls --server localhost:4222
# Expected: no streams found
```

## Image override

To test a private-registry image or a different NATS version:

```bash
IOPS_RIG_NATS_IMAGE=private.registry.example.com/nats:2.11.0 make reset
```

Record `make image-digest` output in the run's `manifest.yaml` to pin the
exact content hash for reproducibility.

## Profiles

The compose file declares 5 NATS services:

- `nats-1`, `nats-2`, `nats-3` — profiles `r3` and `r5`
- `nats-4`, `nats-5` — profile `r5` only

`make up` translates `IOPS_RIG_NATS_REPLICAS` to `--profile r<N>`.
Both profiles are specified on `make down` / `make reset` so a prior r5
run is fully cleaned up even when transitioning back to r3.

## Harness artifacts

A complete harness run writes two files into `--output-dir`:

- `rpc_counts.csv` — per-tick counter snapshots with columns
  `t_unix_ns, worker_idx, bucket, op, count`.
- `manifest.yaml` — run metadata (status, options, confirmed storage,
  per-worker degraded transitions).

`rpc_counts.csv` is **sparse**: at any given timestamp, only
`(worker_idx, bucket, op)` combinations with at least one observed call
emit a row. Absent rows mean `count=0` for that combination at that
tick — they are NOT missing data. Phase 3 tooling must reconstruct the
full grid by treating absent `(worker, bucket, op)` tuples as zero.

`manifest.yaml` is written **last** and only after `rpc_counts.csv` is
durably committed (tmp + fsync + rename + directory fsync). If a run is
interrupted before the capture window opens (e.g. SIGINT during warmup
or a worker going degraded during warmup), no `manifest.yaml` is
written: the presence of `manifest.yaml` is the contract that a CSV
exists and is complete. Phase 3 should treat a missing manifest as
"discard this run", not as a soft failure.

## Capture scripts

The `scripts/` directory contains four capture helpers for Phase 3.
Each writes to a file under the run's output directory; Phase 3e's
aggregator reads all four sources together.

### capture-cgroup-io.sh (primary IOPS source, 3a)

1 Hz cgroup v2 `io.stat` poller. Requires the host to use cgroup v2
with the systemd driver (`/sys/fs/cgroup/system.slice/docker-<id>.scope/io.stat`).

Output format (space-separated, one row per container per device per sample):
```
# t_unix_ns container device rbytes wbytes rios wios
<unix_ns> iops-nats-1 8:0 12345678 9012345 42 17
```
Raw cumulative counters — Phase 3e diffs `row[n] - row[n-1]` per device.

```bash
scripts/capture-cgroup-io.sh \
  --output results/run-001/cgroup_io.raw \
  --duration 600 \
  --containers iops-nats-1,iops-nats-2,iops-nats-3
```

### capture-iostat.sh (secondary cross-check, 3b)

Wrapper around `iostat -x -d -t 1`. Requires the `sysstat` package.

Output format: raw `iostat` output preceded by a one-line header.
```
# iostat -x -d -t 1
<iostat output — timestamped sections, 1-second interval>
```

```bash
scripts/capture-iostat.sh \
  --output results/run-001/iostat.raw \
  --duration 600
```

### capture-jsz.sh (NATS server stats, 3c)

Polls `/jsz?streams=true&consumers=true` and `/varz` for each NATS
monitoring endpoint every 5 seconds. Requires `curl` and `jq`. Aborts
immediately on any curl failure.

Output format (ndjson, one object per line):
```json
{"t_unix_ns": 1715000000000000000, "node": "localhost:8222", "endpoint": "jsz", "body": {...}}
{"t_unix_ns": 1715000000000000000, "node": "localhost:8222", "endpoint": "varz", "body": {...}}
```

```bash
scripts/capture-jsz.sh \
  --output results/run-001/jsz.raw \
  --duration 600 \
  --nats-nodes localhost:8222
```

### prometheus-node-exporter.yaml + capture-node-exporter.sh (host sanity, 3d)

`scripts/prometheus-node-exporter.yaml` is a docker compose override
that adds a `node-exporter` service to the cluster stack:

```bash
docker compose \
  -f docker/docker-compose.yaml \
  -f scripts/prometheus-node-exporter.yaml \
  up -d
```

The overlay registers on the existing `iops-net` network and exposes
`http://localhost:9100/metrics`. Use `capture-node-exporter.sh` to
record a time series:

```bash
scripts/capture-node-exporter.sh \
  --output results/run-001/node_exporter.prom \
  --duration 600
```

Output format: timestamped Prometheus text-format blocks, separated by
blank lines:
```
# t_unix_ns 1715000000000000000
# HELP node_disk_reads_completed_total ...
node_disk_reads_completed_total{device="sda"} 12345
...

# t_unix_ns 1715000005000000000
...
```

## Running the matrix

`scripts/run-matrix.sh` orchestrates all M1.0–M1.11 measurement cells in a
pre-registered randomised schedule.

### Quick start

```bash
# Build the harness binary first.
go build -o cmd/harness/harness ./cmd/harness

# Preview the full 190-run schedule (no rig activity).
bash scripts/run-matrix.sh --seed 42 --dry-run

# Run the full campaign.
bash scripts/run-matrix.sh --seed 42 --results-dir results/$(date +%Y%m%d)/
```

### Options

```
--seed N            Required. Integer seed; recorded in every run's run-meta.yaml.
--cells CELLS       Comma-separated subset, e.g. M1.1,M1.2,M1.8.
                    Default: all M1.0–M1.11.
--reps N            Replicates per (cell, N) pair. Default: 5.
--results-dir PATH  Parent directory for per-run subdirs. Default: results/.
--dry-run           Print the randomised schedule and exit.
```

### Cell summary

| Cell | Label | N values | Replicas | Key flag(s) |
|------|-------|----------|----------|-------------|
| M1.0 | No-Parti control | 500,1000,2000,3000 | 3 | _(no harness — NATS only)_ |
| M1.1 | B-lib baseline | 500,1000,2000,3000 | 3 | `--two-phase=false` |
| M1.2 | B-prod baseline | 500,1000,2000,3000 | 3 | `--two-phase=true` |
| M1.3 | H1.A sweep=5m | 500,1000,2000,3000 | 3 | `--two-phase=true --sweep-interval=5m0s` |
| M1.4 | H1.B two-phase off | 500,1000,2000,3000 | 3 | `--two-phase=false` |
| M1.5 | H2.A fetch=30s | 500,1000,2000,3000 | 3 | `--two-phase=true --fetch-timeout=30s` |
| M1.6 | H2.B Queue consumer | 500,1000,2000,3000 | 3 | `--two-phase=true --consumer-mode=queue` |
| M1.7 | H2.C data=memory | 500,1000,2000,3000 | 3 | `--two-phase=true --data-storage=memory` |
| M1.8 | H3 heartbeat=10s | 2000 | 3 | `--two-phase=true --heartbeat-interval=10s` |
| M1.9 | M5 KV=memory | 1000,2000,3000 | 3 | `--two-phase=true --kv-storage=memory` |
| M1.10 | R=5 comparison | 2000 | 5 | `--two-phase=true --replicas=5` |
| M1.11 | HEAD comparison | 2000 | 3 | _(manual pin-swap required — see below)_ |

Total at default reps=5: 190 runs (~54 hours wall-clock at 17 min/run).

### Outputs

Each run lands in `results/run-NNN-<cell>-N<N>-rep<r>/`:

- `run-meta.yaml` — pre-run sidecar (seed, schedule position, cell, N, rep, flags).
- `manifest.yaml` — harness run metadata (written last; presence = CSV complete).
  For M1.0 control runs the orchestrator writes a synthetic `status: ok`
  manifest so the aggregator can still produce an `aggregated.csv` rig
  noise-floor row.
- `rpc_counts.csv` — per-tick JetStream RPC counters from the harness wrappers.
  Header-only for M1.0 control runs (no harness, no RPC).
- `cgroup_io.raw` — primary per-container IOPS (cgroup v2 required; preflight enforced).
- `iostat.raw` — secondary host-level cross-check (sysstat required; preflight enforced).
- `jsz.raw` — per-node NATS server stats (ndjson).
- `node_exporter.prom` — host sanity metrics. node_exporter is a soft
  requirement: if it's unreachable a one-line stub is written so the
  aggregator's presence check passes, and the campaign warns loudly.
- `aggregated.csv` — produced by `aggregate --run-dir <path>` after the run.
- `failed.txt` — present only if the harness exited non-zero.
- `capture-failed.txt` — present if any mandatory capture file was missing/empty.
- `aggregate-failed.txt` — present only if the aggregator step failed.

Aggregator / capture-source failures are no longer masked as `run OK`: any
missing capture output or non-zero aggregate exit marks the run failed in
the campaign tally.

The campaign summary is in `results/campaign-manifest.yaml`.
The pre-registered schedule is in `results/schedule.tsv` (written before any run starts).

### M1.0 control runs

M1.0 skips the harness binary. The script spins up the NATS cluster, starts all
capture scripts for the full warmup+capture window (15 min), and then stops them.
For M1.0 the script also synthesizes a minimal `manifest.yaml` (`status: ok`,
`partiVersion: control`) and a header-only `rpc_counts.csv` so the aggregator
treats the run as a normal one and produces an `aggregated.csv` with empty RPC
columns. M1.0 runs establish `MDE_slope` — the no-Parti noise floor — and must
complete before slope attribution is valid.

### M1.11 manual pin-swap workflow

M1.11 compares against a HEAD build of parti. The script **always refuses M1.11**
with an error message because swapping the go.mod pin changes the binary under test
and must be a deliberate operator action, not an automated step.

To run M1.11:

1. In `test/iops-investigation/go.mod`, change the parti require line from the
   pinned v2.3.0 tag to the HEAD commit hash:
   ```
   require github.com/arloliu/parti/v2 v0.0.0-<date>-<hash>
   ```
   Use `go get github.com/arloliu/parti/v2@HEAD` (with a local replace directive
   if HEAD is not yet pushed) or `go mod edit -replace`.

2. Rebuild the harness: `go build -o cmd/harness/harness ./cmd/harness`.

3. Run with `--cells M1.11` only (keep the same `--seed` as the rest of the
   campaign so position numbers are contiguous):
   ```bash
   bash scripts/run-matrix.sh --seed 42 --cells M1.11 --results-dir results/$(date +%Y%m%d)/
   ```

4. After M1.11 completes, restore the `go.mod` pin to v2.3.0 and rebuild before
   running any other cells.

Record the HEAD commit hash and go.mod diff in the M1.11 run's `notes.md` so the
comparison is reproducible.

## Note on healthchecks

The default `nats:2.12.6` image is scratch-based (no shell, wget, or curl
inside the container). Healthchecks requiring shell utilities are not
available. The cluster is ready when `curl http://localhost:8222/jsz`
returns JSON from the host — use that as the readiness probe in automation.
