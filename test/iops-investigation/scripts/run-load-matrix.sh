#!/usr/bin/env bash
# run-load-matrix.sh — Orchestrates the design §8 load + latency matrix.
#
# Sibling of run-matrix.sh (the idle-IOPS M1.x campaign). This runner drives
# the harness in --load mode: an open-loop producer + end-to-end latency
# measurement, with CPU isolation verified live before each cell's window
# opens (design §10). See docs/plans/perf-measurement/01-implementation-plan.md
# Phase 8 (Tasks 8.1, 8.2) and 00-design.md §8 for the cell-to-flag matrix.
#
# Matrix (design §8):
#   dynamic    : N ∈ {1000,2000,3000,5000} × k ∈ {1,2,4} × storage ∈ {file,memory}  (24 cells)
#   queue floor: N ∈ {1000,2000,3000,5000} × storage ∈ {file,memory} at k=2          (8 cells)
#   queue corners: k ∈ {1,4} × N ∈ {1000,5000} storage=file                           (4 cells)
#   ⇒ 36 cells × 3 reps = 108 runs.
#
# Cells are ordered by ASCENDING N (then k, storage, mode) so saturation shows
# up at the last rung — NOT shuffled like run-matrix.sh (this is a ramp, not a
# randomised attribution campaign).
#
# --meta-sweep is a separate mode: N=5000,k=2,file,dynamic against the
# metacontroller snapshot configs {default,16MB,64MB} (design §8.1).
#
# Usage:
#   run-load-matrix.sh --seed N [--cells L1,L2,...] [--reps R] \
#                      [--warmup-secs S] [--capture-secs S] \
#                      [--results-dir PATH] [--dry-run]
#   run-load-matrix.sh --seed N --meta-sweep [--reps R] [--dry-run] ...
#
# REQUIRED: --seed N  (recorded in every per-run run-meta.yaml sidecar).
#
# Resume gate (design §13): a rep dir is SKIPPED only if it is a VALID complete
# cell — manifest.yaml exists AND `status: ok` AND latency.json present. Any
# other state (degraded / interrupted / producer-bound / delivery-deficit, or a
# missing latency.json) is INVALID: the rep dir is DELETED and re-run. A bare
# manifest-presence check would silently accept an invalid cell as done.
#
# Structural failures (missing harness binary, wrong directory) abort loudly.
# Per-cell failures (harness exit, isolation mismatch) are logged and continue
# the campaign — no silent caps (design §9).
set -euo pipefail

# ---------------------------------------------------------------------------
# Paths — all derived from this script's location; works from any cwd.
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RIG_DIR="$(dirname "$SCRIPT_DIR")"
DOCKER_DIR="${RIG_DIR}/docker"

HARNESS_BIN="${RIG_DIR}/cmd/harness/harness"

# Load mode pins R=5 (design §8). Container + monitor lists are fixed at 5.
REPLICAS=5
CONTAINERS="iops-nats-1,iops-nats-2,iops-nats-3,iops-nats-4,iops-nats-5"
NATS_MONITOR="localhost:8222"   # single ingress; route discovery handles the rest

# Harness CPU pin (design §10). NATS is pinned 0-7,16-23 by docker-compose;
# the harness gets the complementary set.
HARNESS_CPUSET="8-15,24-31"

# Warmup and capture window durations (seconds). Design §8 uses 120s/120s.
WARMUP_SECS=120
CAPTURE_SECS=120

# ---------------------------------------------------------------------------
# Cell enumeration.
#
# Each entry is a single space-separated record:  <label> <N> <k> <storage> <mode>
# Built in ASCENDING-N order so the ramp surfaces saturation at the last rung.
# The label doubles as the per-cell results subdir name and the dry-run marker.
# ---------------------------------------------------------------------------
N_VALUES=(1000 2000 3000 5000)
K_VALUES=(1 2 4)
STORAGES=(file memory)

# build_cells populates the CELLS array (label N k storage mode) in ramp order.
CELLS=()
build_cells() {
    CELLS=()
    local n k s
    for n in "${N_VALUES[@]}"; do
        # dynamic: full N × k × storage cross-product.
        for k in "${K_VALUES[@]}"; do
            for s in "${STORAGES[@]}"; do
                CELLS+=("dyn-N${n}-k${k}-${s} ${n} ${k} ${s} dynamic")
            done
        done
        # queue floor: storage ∈ {file,memory} at k=2.
        for s in "${STORAGES[@]}"; do
            CELLS+=("queue-N${n}-k2-${s} ${n} 2 ${s} queue")
        done
        # queue corners: k ∈ {1,4} × N ∈ {1000,5000} storage=file.
        if [[ "$n" == "1000" || "$n" == "5000" ]]; then
            for k in 1 4; do
                CELLS+=("queue-N${n}-k${k}-file ${n} ${k} file queue")
            done
        fi
    done
}

# ---------------------------------------------------------------------------
# Meta-sweep cells (design §8.1). N=5000,k=2,file,dynamic against the three
# metacontroller snapshot configs. nats_config is relative to docker/ to match
# docker-compose's ${IOPS_RIG_NATS_CONFIG:-./nats-server.conf} convention.
#
# Each entry: <label> <nats_config_relative_to_docker_dir>
# ---------------------------------------------------------------------------
META_CELLS=(
    "meta-default ./nats-server.conf"
    "meta-16MB ./nats-server-meta16.conf"
    "meta-64MB ./nats-server-meta64.conf"
)
META_N=5000
META_K=2
META_STORAGE=file
META_MODE=dynamic
# Minimum metacontroller snapshot count for a meta-sweep cell to be considered
# to have exercised compaction (design §8.1). Log-only — never hard-fails the
# campaign (no silent cap; honest accounting in the log).
META_SNAPSHOT_FLOOR=5

# ---------------------------------------------------------------------------
# Usage.
# ---------------------------------------------------------------------------
usage() {
    cat >&2 <<'EOF'
Usage: run-load-matrix.sh --seed N [options]

Required:
  --seed N            Integer seed recorded in every per-run run-meta.yaml.
                      (Run order is a deterministic N-ramp, not shuffled, so
                      the seed is provenance only — recorded, not consumed for
                      ordering.)

Options:
  --cells LABELS      Comma-separated cell-label filter (default: all 36).
                      Example: --cells dyn-N1000-k1-file,queue-N5000-k2-file
  --reps N            Replicates per cell (default: 3).
  --warmup-secs N     Per-run warmup window in seconds (default: 120).
  --capture-secs N    Per-run capture window in seconds (default: 120).
  --results-dir PATH  Parent directory for per-cell subdirs (default: results/).
  --meta-sweep        Run the metacontroller meta_compact_size sweep instead of
                      the main matrix (N=5000,k=2,file,dynamic against
                      {default,16MB,64MB}; design §8.1).
  --dry-run           Print the full cell list + derived flags and exit without
                      running anything.
  -h, --help          Show this message and exit.

Environment:
  IOPS_RIG_NATS_IMAGE         Docker image for the NATS cluster (default: nats:2.12.6).
  IOPS_RIG_NATS_IMAGE_DIGEST  Pre-resolved image digest for manifest (optional).

Out-of-process cell (design §8.2): STRETCH, not built. The runner logs
"§8.2 out-of-process: not run (stretch, cmd/worker not built)" instead.
EOF
    exit 2
}

# ---------------------------------------------------------------------------
# Argument parsing.
# ---------------------------------------------------------------------------
SEED=""
CELLS_ARG=""
REPS=3
RESULTS_DIR="${RIG_DIR}/results"
DRY_RUN=false
META_SWEEP=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --seed)         SEED="$2";         shift 2 ;;
        --cells)        CELLS_ARG="$2";    shift 2 ;;
        --reps)         REPS="$2";         shift 2 ;;
        --warmup-secs)  WARMUP_SECS="$2";  shift 2 ;;
        --capture-secs) CAPTURE_SECS="$2"; shift 2 ;;
        --results-dir)  RESULTS_DIR="$2";  shift 2 ;;
        --meta-sweep)   META_SWEEP=true;   shift ;;
        --dry-run)      DRY_RUN=true;      shift ;;
        -h|--help)      usage ;;
        *) echo "run-load-matrix.sh: unknown argument: $1" >&2; usage ;;
    esac
done

[[ -z "$SEED" ]] && { echo "run-load-matrix.sh: --seed is required" >&2; usage; }
if ! [[ "$SEED" =~ ^-?[0-9]+$ ]]; then
    echo "run-load-matrix.sh: --seed must be an integer, got: $SEED" >&2
    exit 1
fi
if ! [[ "$REPS" =~ ^[0-9]+$ ]] || [[ "$REPS" -lt 1 ]]; then
    echo "run-load-matrix.sh: --reps must be a positive integer, got: $REPS" >&2
    exit 1
fi
if ! [[ "$WARMUP_SECS" =~ ^[0-9]+$ ]] || [[ "$WARMUP_SECS" -lt 5 ]]; then
    echo "run-load-matrix.sh: --warmup-secs must be an integer >= 5, got: $WARMUP_SECS" >&2
    exit 1
fi
if ! [[ "$CAPTURE_SECS" =~ ^[0-9]+$ ]] || [[ "$CAPTURE_SECS" -lt 10 ]]; then
    echo "run-load-matrix.sh: --capture-secs must be an integer >= 10, got: $CAPTURE_SECS" >&2
    exit 1
fi

# Total capture window covers warmup + measurement (jsz polls from startup so
# metacontroller snapshots during stabilisation are captured — design §8.1).
CAPTURE_TOTAL=$(( WARMUP_SECS + CAPTURE_SECS ))

# Build the cell list and apply the optional --cells filter.
build_cells
if [[ -n "$CELLS_ARG" ]]; then
    declare -A _label_filter=()
    IFS=',' read -ra _filter_arr <<< "$CELLS_ARG"
    for l in "${_filter_arr[@]}"; do _label_filter["$l"]=1; done
    FILTERED=()
    for rec in "${CELLS[@]}"; do
        read -r label _ <<< "$rec"
        if [[ -n "${_label_filter[$label]+set}" ]]; then
            FILTERED+=("$rec")
        fi
    done
    # Validate every requested label matched a real cell.
    for l in "${_filter_arr[@]}"; do
        local_match=false
        for rec in "${CELLS[@]}"; do
            read -r label _ <<< "$rec"
            [[ "$label" == "$l" ]] && { local_match=true; break; }
        done
        if [[ "$local_match" == "false" ]]; then
            echo "run-load-matrix.sh: unknown cell label '$l'" >&2
            exit 1
        fi
    done
    CELLS=("${FILTERED[@]}")
fi

# ---------------------------------------------------------------------------
# Dry-run: print the full plan and exit. No live cluster, no side effects.
# ---------------------------------------------------------------------------
if [[ "$DRY_RUN" == "true" ]]; then
    if [[ "$META_SWEEP" == "true" ]]; then
        echo "# run-load-matrix.sh dry-run (meta-sweep)  seed=${SEED}  reps=${REPS}"
        echo "# warmup=${WARMUP_SECS}s capture=${CAPTURE_SECS}s replicas=${REPLICAS}"
        echo "# snapshot floor (log-only gate): ${META_SNAPSHOT_FLOOR}"
        for mc in "${META_CELLS[@]}"; do
            read -r label conf <<< "$mc"
            for (( r=1; r<=REPS; r++ )); do
                echo "META-CELL ${label} rep${r}  conf=${conf}  flags=--load --replicas ${REPLICAS} --n ${META_N} --workers $(( META_N / 50 )) --per-worker-rate ${META_K} --consumer-mode ${META_MODE} --kv-storage ${META_STORAGE} --data-storage ${META_STORAGE} --warmup ${WARMUP_SECS}s --capture-window ${CAPTURE_SECS}s"
            done
        done
        echo "§8.2 out-of-process: not run (stretch, cmd/worker not built)"
        exit 0
    fi

    echo "# run-load-matrix.sh dry-run  seed=${SEED}  reps=${REPS}  cells=${#CELLS[@]}  total_runs=$(( ${#CELLS[@]} * REPS ))"
    echo "# warmup=${WARMUP_SECS}s capture=${CAPTURE_SECS}s replicas=${REPLICAS} harness_cpuset=${HARNESS_CPUSET}"
    echo "# order: ascending N ramp (saturation surfaces at the last rung)"
    for rec in "${CELLS[@]}"; do
        read -r label n k storage mode <<< "$rec"
        for (( r=1; r<=REPS; r++ )); do
            echo "CELL ${label} rep${r}  flags=--load --replicas ${REPLICAS} --n ${n} --workers $(( n / 50 )) --per-worker-rate ${k} --consumer-mode ${mode} --kv-storage ${storage} --data-storage ${storage} --warmup ${WARMUP_SECS}s --capture-window ${CAPTURE_SECS}s"
        done
    done
    # Meta-sweep entries are part of the full plan (design §8.1), so the plain
    # dry-run enumerates them too. Run them live with --meta-sweep.
    echo "# meta-sweep (run live with --meta-sweep): ${#META_CELLS[@]} configs × ${REPS} reps"
    for mc in "${META_CELLS[@]}"; do
        read -r label conf <<< "$mc"
        for (( r=1; r<=REPS; r++ )); do
            echo "META-CELL ${label} rep${r}  conf=${conf}  flags=--load --replicas ${REPLICAS} --n ${META_N} --workers $(( META_N / 50 )) --per-worker-rate ${META_K} --consumer-mode ${META_MODE} --kv-storage ${META_STORAGE} --data-storage ${META_STORAGE} --warmup ${WARMUP_SECS}s --capture-window ${CAPTURE_SECS}s"
        done
    done
    echo "§8.2 out-of-process: not run (stretch, cmd/worker not built)"
    exit 0
fi

# ---------------------------------------------------------------------------
# Structural pre-flight checks (live runs only).
# ---------------------------------------------------------------------------
if [[ ! -x "$HARNESS_BIN" ]]; then
    echo "run-load-matrix.sh: harness binary not found or not executable: $HARNESS_BIN" >&2
    echo "run-load-matrix.sh: build it with: go build -o cmd/harness/harness ./cmd/harness" >&2
    exit 1
fi
if [[ ! -x "${SCRIPT_DIR}/verify-isolation.sh" ]]; then
    echo "run-load-matrix.sh: verify-isolation.sh not found or not executable: ${SCRIPT_DIR}/verify-isolation.sh" >&2
    exit 1
fi
for tool in docker taskset curl iostat jq; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "run-load-matrix.sh: required tool '$tool' not found in PATH" >&2
        exit 1
    fi
done
if [[ ! -d /sys/fs/cgroup/system.slice ]]; then
    echo "run-load-matrix.sh: cgroup v2 not available at /sys/fs/cgroup/system.slice" >&2
    echo "run-load-matrix.sh: cgroup_io.raw is a mandatory capture source; aborting." >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# Capture lifecycle helpers (shared with run-matrix.sh).
# ---------------------------------------------------------------------------
CAPTURE_PIDS=()
CAPTURE_NAMES=()
CAPTURE_RCS=()

# shellcheck source=_capture_lib.sh
source "${SCRIPT_DIR}/_capture_lib.sh"

# Track a live harness PID so the signal trap can tear it down too.
HARNESS_PID=""

trap 'echo "run-load-matrix.sh: signal received — killing harness + captures, exiting" >&2; [[ -n "$HARNESS_PID" ]] && { kill "$HARNESS_PID" 2>/dev/null || true; wait "$HARNESS_PID" 2>/dev/null || true; }; stop_captures; exit 130' INT TERM
trap 'stop_captures' EXIT

mkdir -p "$RESULTS_DIR"

# log — timestamped campaign log line (mirrors run-matrix.sh style).
log() {
    echo "run-load-matrix.sh: $*"
}

# ---------------------------------------------------------------------------
# start_captures — launch the full mandatory capture set into the rep dir.
#
# The harness writes manifest.yaml / latency.json / rpc_counts.csv straight
# into --output-dir (= the rep dir), so the "copy artifacts in" step from the
# spec is satisfied by pointing --output-dir there. jsz/cgroup/iostat are
# captured as sidecars here. jsz polling starts BEFORE the harness so the
# metacontroller's startup snapshots are recorded (design §8.1).
# ---------------------------------------------------------------------------
start_captures() {
    local run_dir="$1"
    local duration="$2"

    CAPTURE_PIDS=()
    CAPTURE_NAMES=()

    bash "${SCRIPT_DIR}/capture-cgroup-io.sh" \
        --output "${run_dir}/cgroup_io.raw" \
        --duration "$duration" \
        --containers "$CONTAINERS" &
    CAPTURE_PIDS+=($!)
    CAPTURE_NAMES+=("cgroup-io")

    bash "${SCRIPT_DIR}/capture-cgroup-cpumem.sh" \
        --output "${run_dir}/cgroup_cpumem.raw" \
        --duration "$duration" \
        --containers "$CONTAINERS" &
    CAPTURE_PIDS+=($!)
    CAPTURE_NAMES+=("cgroup-cpumem")

    bash "${SCRIPT_DIR}/capture-iostat.sh" \
        --output "${run_dir}/iostat.raw" \
        --duration "$duration" &
    CAPTURE_PIDS+=($!)
    CAPTURE_NAMES+=("iostat")

    bash "${SCRIPT_DIR}/capture-jsz.sh" \
        --output "${run_dir}/jsz.raw" \
        --duration "$duration" \
        --nats-nodes "$NATS_MONITOR" &
    CAPTURE_PIDS+=($!)
    CAPTURE_NAMES+=("jsz")
}

# ---------------------------------------------------------------------------
# resume_verdict RUN_DIR — classify an existing rep dir.
#
# Echoes one of:  skip-valid | rerun:<reason> | fresh
#   skip-valid     manifest.yaml present + `status: ok` + latency.json present.
#   rerun:<reason> dir exists but is an INVALID complete cell — caller deletes
#                  it and re-runs. reason names why (status value / missing file).
#   fresh          dir does not exist yet.
# Pure read-only — does NOT delete (caller owns the rm so the log + rm stay
# adjacent and the empty-$dir guard lives in one place).
# ---------------------------------------------------------------------------
resume_verdict() {
    local d="$1"
    if [[ ! -d "$d" ]]; then
        echo "fresh"
        return 0
    fi
    local manifest="${d}/manifest.yaml"
    if [[ ! -f "$manifest" ]]; then
        echo "rerun:no-manifest"
        return 0
    fi
    if ! grep -q '^status: ok$' "$manifest"; then
        # Extract the recorded status for a precise reason (best-effort).
        local st
        st="$(grep -m1 '^status:' "$manifest" | sed 's/^status:[[:space:]]*//' || true)"
        echo "rerun:status=${st:-unknown}"
        return 0
    fi
    if [[ ! -f "${d}/latency.json" ]]; then
        echo "rerun:no-latency-json"
        return 0
    fi
    echo "skip-valid"
}

# ---------------------------------------------------------------------------
# run_cell — execute one (cell, rep) into its rep dir.
#
# Arguments: label N k storage mode rep [nats_config]
# Returns 0 on a completed run, 1 on any per-cell failure (logged; campaign
# continues). Honours the resume gate.
# ---------------------------------------------------------------------------
run_cell() {
    local label="$1" n="$2" k="$3" storage="$4" mode="$5" rep="$6"
    local nats_config="${7:-}"   # relative-to-docker/ path; empty ⇒ compose default
    local workers=$(( n / 50 ))
    local run_dir="${RESULTS_DIR}/${label}/rep${rep}"

    echo ""
    echo "=== ${label} rep${rep}  (N=${n} k=${k} storage=${storage} mode=${mode}) ==="

    # --- Resume gate (design §13) ---------------------------------------
    local verdict
    verdict="$(resume_verdict "$run_dir")"
    case "$verdict" in
        skip-valid)
            log "SKIP ${label} rep${rep}: valid complete cell (status: ok + latency.json present)"
            return 0
            ;;
        rerun:*)
            local reason="${verdict#rerun:}"
            log "RERUN ${label} rep${rep}: invalid cell (${reason}) — deleting ${run_dir} and re-running"
            # Guard the destructive rm against an empty path.
            if [[ -n "$run_dir" && "$run_dir" != "/" ]]; then
                rm -rf "$run_dir"
            fi
            ;;
        fresh) ;;
    esac
    mkdir -p "$run_dir"

    # --- Per-run sidecar (provenance before any run) --------------------
    {
        echo "seed: ${SEED}"
        echo "label: ${label}"
        echo "n: ${n}"
        echo "k: ${k}"
        echo "workers: ${workers}"
        echo "storage: ${storage}"
        echo "mode: ${mode}"
        echo "rep: ${rep}"
        echo "replicas: ${REPLICAS}"
        echo "nats_config: '${nats_config:-default}'"
        echo "warmup_secs: ${WARMUP_SECS}"
        echo "capture_secs: ${CAPTURE_SECS}"
    } > "${run_dir}/run-meta.yaml"

    # --- Reset the rig (pins R=5; swaps per-cell nats_config when set) ---
    log "resetting rig (replicas=${REPLICAS}, nats_config=${nats_config:-default})..."
    if ! IOPS_RIG_NATS_REPLICAS="${REPLICAS}" \
         IOPS_RIG_NATS_CONFIG="${nats_config}" \
         make -C "$RIG_DIR" reset; then
        echo "  FAILED: make reset failed; aborting cell" >&2
        echo "make reset failed" > "${run_dir}/failed.txt"
        return 1
    fi

    # --- Start captures (jsz from startup, for metacontroller snapshots) -
    start_captures "$run_dir" "$CAPTURE_TOTAL"
    log "captures started (PIDs: ${CAPTURE_PIDS[*]:-none})"

    # --- Launch the harness in the BACKGROUND, pinned (Task 6.3 Step 1) --
    # The harness must be live when isolation is verified — you cannot check a
    # dead PID's affinity. So background-launch, sleep, verify, then wait.
    log "launching harness (background, pinned ${HARNESS_CPUSET})..."
    IOPS_RIG_NATS_IMAGE="${IOPS_RIG_NATS_IMAGE:-nats:2.12.6}" \
    IOPS_RIG_NATS_IMAGE_DIGEST="${IOPS_RIG_NATS_IMAGE_DIGEST:-}" \
        taskset -c "$HARNESS_CPUSET" "$HARNESS_BIN" \
            --load \
            --replicas "$REPLICAS" \
            --n "$n" \
            --workers "$workers" \
            --per-worker-rate "$k" \
            --consumer-mode "$mode" \
            --kv-storage "$storage" \
            --data-storage "$storage" \
            --warmup "${WARMUP_SECS}s" \
            --capture-window "${CAPTURE_SECS}s" \
            --output-dir "$run_dir" &
    HARNESS_PID=$!
    log "harness PID=${HARNESS_PID}; sleeping 2s before isolation verify..."
    sleep 2

    # --- Verify isolation against the LIVE PID; abort+continue on mismatch
    if ! "${SCRIPT_DIR}/verify-isolation.sh" "$HARNESS_PID" "$CONTAINERS"; then
        log "ABORT ${label} rep${rep}: isolation mismatch — killing harness PID ${HARNESS_PID}"
        kill "$HARNESS_PID" 2>/dev/null || true
        wait "$HARNESS_PID" 2>/dev/null || true
        HARNESS_PID=""
        stop_captures
        echo "isolation mismatch (cpuset)" > "${run_dir}/failed.txt"
        return 1
    fi

    # --- Let the run complete normally ----------------------------------
    log "isolation OK; waiting for harness to finish (warmup+capture ≈ ${CAPTURE_TOTAL}s)..."
    local harness_rc=0
    wait "$HARNESS_PID" || harness_rc=$?
    HARNESS_PID=""

    # --- Stop captures regardless of harness outcome --------------------
    log "stopping captures..."
    stop_captures

    if [[ "$harness_rc" -ne 0 ]]; then
        printf 'harness exit code %d\n' "$harness_rc" > "${run_dir}/failed.txt"
        log "FAILED ${label} rep${rep}: harness exit ${harness_rc}; continuing campaign."
        return 1
    fi

    # --- Verify capture outputs (sidecar files) -------------------------
    # verify_captures checks node_exporter.prom too; load mode does not capture
    # it, so write a stub for the presence check (node_exporter is sanity-only).
    if [[ ! -s "${run_dir}/node_exporter.prom" ]]; then
        printf '# node_exporter not captured in load mode; stub for presence check\n' \
            > "${run_dir}/node_exporter.prom"
    fi
    if ! verify_captures "$run_dir"; then
        log "FAILED ${label} rep${rep}: capture verify; continuing campaign."
        return 1
    fi

    # --- Resume-gate post-check: the harness must have produced a VALID cell.
    # If the harness itself wrote a non-ok status (degraded/producer-bound/etc.)
    # or skipped latency.json, this rep is NOT a completed cell — surface it.
    local post
    post="$(resume_verdict "$run_dir")"
    if [[ "$post" != "skip-valid" ]]; then
        log "FAILED ${label} rep${rep}: harness produced an invalid cell (${post#rerun:}); continuing campaign."
        return 1
    fi

    log "OK ${label} rep${rep}."
    return 0
}

# ---------------------------------------------------------------------------
# Meta-sweep mode.
# ---------------------------------------------------------------------------
run_meta_sweep() {
    log "META-SWEEP: N=${META_N} k=${META_K} storage=${META_STORAGE} mode=${META_MODE} against ${#META_CELLS[@]} configs × ${REPS} reps"
    local success=0 total=0
    for mc in "${META_CELLS[@]}"; do
        read -r label conf <<< "$mc"
        for (( r=1; r<=REPS; r++ )); do
            total=$(( total + 1 ))
            if run_cell "$label" "$META_N" "$META_K" "$META_STORAGE" "$META_MODE" "$r" "$conf"; then
                success=$(( success + 1 ))
                # Snapshot-count gate (log-only; design §8.1). Counts distinct
                # metacontroller snapshot events seen in jsz.raw — never aborts.
                check_meta_snapshots "${RESULTS_DIR}/${label}/rep${r}/jsz.raw" "$label" "$r"
            fi
        done
    done
    echo ""
    echo "=== Meta-sweep complete: ${success}/${total} cells OK ==="
    echo "§8.2 out-of-process: not run (stretch, cmd/worker not built)"
    [[ "$success" -lt "$total" ]] && return 1
    return 0
}

# check_meta_snapshots JSZ_RAW LABEL REP — log-only snapshot-count gate.
# Extracts the running metacontroller snapshot count from the last jsz line and
# logs whether it cleared META_SNAPSHOT_FLOOR. Never fails the campaign.
check_meta_snapshots() {
    local jsz="$1" label="$2" rep="$3"
    if [[ ! -s "$jsz" ]]; then
        log "META-SNAPSHOT ${label} rep${rep}: jsz.raw missing/empty — cannot evaluate snapshot floor"
        return 0
    fi
    # jsz body exposes a meta-cluster snapshot counter; field name varies by
    # NATS version, so probe the common candidates and take the max seen.
    local count
    count="$(jq -rs '
        [ .[]
          | select(.endpoint == "jsz")
          | (.body.meta_cluster.snapshots
             // .body.cluster.snapshots
             // .body.snapshots
             // 0) ]
        | (max // 0)' "$jsz" 2>/dev/null || echo 0)"
    [[ -z "$count" || "$count" == "null" ]] && count=0
    if (( count >= META_SNAPSHOT_FLOOR )); then
        log "META-SNAPSHOT ${label} rep${rep}: snapshots=${count} (>= floor ${META_SNAPSHOT_FLOOR}) OK"
    else
        log "META-SNAPSHOT ${label} rep${rep}: snapshots=${count} (< floor ${META_SNAPSHOT_FLOOR}) — compaction may not have triggered; cell kept (log-only, no silent cap)"
    fi
}

# ---------------------------------------------------------------------------
# Main.
# ---------------------------------------------------------------------------
if [[ "$META_SWEEP" == "true" ]]; then
    run_meta_sweep
    exit $?
fi

log "schedule: ${#CELLS[@]} cells × ${REPS} reps = $(( ${#CELLS[@]} * REPS )) runs (ascending-N ramp)"
log "results dir: ${RESULTS_DIR}"

success_count=0
fail_count=0
for rec in "${CELLS[@]}"; do
    read -r label n k storage mode <<< "$rec"
    for (( r=1; r<=REPS; r++ )); do
        if run_cell "$label" "$n" "$k" "$storage" "$mode" "$r"; then
            success_count=$(( success_count + 1 ))
        else
            fail_count=$(( fail_count + 1 ))
        fi
    done
done

echo ""
echo "=== Load-matrix campaign complete ==="
echo "  Cells:   ${#CELLS[@]} × ${REPS} reps"
echo "  Success: ${success_count}"
echo "  Failed:  ${fail_count}"
echo "§8.2 out-of-process: not run (stretch, cmd/worker not built)"

[[ "$fail_count" -gt 0 ]] && exit 1
exit 0
