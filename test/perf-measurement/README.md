# IOPS Investigation Rig

Docker-compose based NATS JetStream cluster for the Parti IOPS attribution
investigation. See `docs/plans/iops-investigation/` for the measurement plan.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `PERF_RIG_NATS_IMAGE` | `nats:2.12.6` | Full image reference, including registry and tag. Override to test a specific NATS version or a private-registry build. |
| `PERF_RIG_NATS_REPLICAS` | `3` | Cluster size. Set to `5` for the M1.10 R=5 comparison run. |
| `PERF_RIG_DIR` | `./data` (auto-created) | Base directory for NATS JetStream data. Resolves relative to `docker/`, i.e. `test/perf-measurement/docker/data/`. Docker auto-creates the subdirectories on first `up` — no manual provisioning needed for local runs. Override to a dedicated mount point for measurement runs requiring isolated storage. |

## Bring the rig up and down

```bash
# Start a 3-node cluster (default)
make up

# Start a 5-node cluster (M1.10)
PERF_RIG_NATS_REPLICAS=5 make up

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

**Note (bind mounts + root ownership):** NATS writes `/data` as **root**, so the
bind-mounted host dirs are root-owned and a plain `rm -rf` fails with EPERM.
`make reset` handles this for you — it clears the data from inside a throwaway
root container, then restarts a fresh cluster. To clear manually (e.g. if you
used a custom `PERF_RIG_DIR`):

```bash
docker run --rm -v "$(pwd)/docker/data:/d" alpine sh -c 'rm -rf /d/perf-nats-*'
# or for a custom PERF_RIG_DIR:
docker run --rm -v "${PERF_RIG_DIR}:/d" alpine sh -c 'rm -rf /d/perf-nats-*'
```

Root-owned `docker/data/` also breaks `go ./...` tree-walks (`go build/vet/test`)
with "permission denied" — always clear it (via `make reset` or the command
above) before running Go tooling from the module root.

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

## NATS data directory

The NATS JetStream `store_dir` (`/data` inside each container) is bind-mounted
to the host. By default it lands under `docker/data/` relative to this
directory (i.e. `test/perf-measurement/docker/data/perf-nats-N`). Docker
auto-creates that directory tree on the first `docker compose up` — no manual
provisioning is required for local development and CI runs.

To redirect NATS data to a dedicated mount point (e.g. a T710 NVMe for
measurement runs requiring isolated storage), set `PERF_RIG_DIR`:

```bash
export PERF_RIG_DIR=/mnt/t710
```

The per-node subdirectories (`$PERF_RIG_DIR/perf-nats-1` … `perf-nats-5`) are
also auto-created by Docker on first `up`.

**Cleanup between runs:** `docker compose down -v` does not clear bind-mounted
directories, and the data is **root-owned** (NATS writes as root) so a plain
`rm -rf` fails. **`make reset` does the right thing** — it deletes the data via
a throwaway root container and restarts a fresh cluster, so run it between
measurement runs to guarantee fresh JetStream state. To clear manually:

```bash
# Default path (no PERF_RIG_DIR set):
docker run --rm -v "$(pwd)/docker/data:/d" alpine sh -c 'rm -rf /d/perf-nats-*'

# Custom path:
docker run --rm -v "${PERF_RIG_DIR}:/d" alpine sh -c 'rm -rf /d/perf-nats-*'
```

## Image override

To test a private-registry image or a different NATS version:

```bash
PERF_RIG_NATS_IMAGE=private.registry.example.com/nats:2.11.0 make reset
```

Record `make image-digest` output in the run's `manifest.yaml` to pin the
exact content hash for reproducibility.

## Profiles

The compose file declares 5 NATS services:

- `nats-1`, `nats-2`, `nats-3` — profiles `r3` and `r5`
- `nats-4`, `nats-5` — profile `r5` only

`make up` translates `PERF_RIG_NATS_REPLICAS` to `--profile r<N>`.
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
<unix_ns> perf-nats-1 8:0 12345678 9012345 42 17
```
Raw cumulative counters — Phase 3e diffs `row[n] - row[n-1]` per device.

```bash
scripts/capture-cgroup-io.sh \
  --output results/run-001/cgroup_io.raw \
  --duration 600 \
  --containers perf-nats-1,perf-nats-2,perf-nats-3
```

### capture-cgroup-cpumem.sh (NATS CPU + RSS source)

Sibling of `capture-cgroup-io.sh`: same containers, cadence, and full-ID
resolution, but reads two files in each container's cgroup v2 scope dir —
`cpu.stat`'s `usage_usec` (cumulative CPU time, µs) and `memory.current`
(instantaneous RSS, bytes).

Output format (space-separated, one row per container per sample):
```
# t_unix_ns container usage_usec memory_current_bytes
<unix_ns> perf-nats-1 1234567890 268435456
```
`cmd/fitmodel` diffs consecutive `usage_usec` into a per-second CPU
fraction-of-one-core (1.0 = one full core) and carries `memory.current`
through as instantaneous RSS.

```bash
scripts/capture-cgroup-cpumem.sh \
  --output results/run-001/cgroup_cpumem.raw \
  --duration 600 \
  --containers perf-nats-1,perf-nats-2,perf-nats-3
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

The overlay registers on the existing `perf-net` network and exposes
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

See **[RUNBOOK.md](RUNBOOK.md)** for the complete operator workflow covering
prerequisites, pre-execution gates (M4 calibration, M1.0 control, MDE
validation), the full campaign invocation, outlier handling, the M1.11
pin-swap procedure, sharding across hosts, and the Phase 6 handoff format.

Quick start:

```bash
# Build the harness binary first.
go build -o cmd/harness/harness ./cmd/harness

# Preview the full 190-run schedule (no rig activity).
bash scripts/run-matrix.sh --seed 42 --dry-run

# Run the full campaign.
bash scripts/run-matrix.sh --seed 42 --results-dir results/$(date +%Y%m%d)/
```

## Note on healthchecks

The default `nats:2.12.6` image is scratch-based (no shell, wget, or curl
inside the container). Healthchecks requiring shell utilities are not
available. The cluster is ready when `curl http://localhost:8222/jsz`
returns JSON from the host — use that as the readiness probe in automation.
