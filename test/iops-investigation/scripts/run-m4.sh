#!/usr/bin/env bash
# run-m4.sh — Drives the M4 7-row KV-paths microbenchmarks and the M4.1
# 48-point idle-pull grid, with per-row capture lifecycle.
#
# See docs/plans/iops-investigation/00-attribution-plan.md §M4 and §M4.1
# for the calibration specs.
#
# Usage:
#   run-m4.sh [--m4-only] [--m41-only] [--results-dir results/m4/] \
#             [--nats-urls URL] [--dry-run]
#
# Rig cycles (documented here — each costs ~2 min of rig reset time):
#   Cycle 1 (R=3): M4 basic 7 rows + M4.1 R=3 chunk (24 grid points).
#     bring up with IOPS_RIG_NATS_REPLICAS=3 once at start.
#   Cycle 2 (R=5): M4.1 R=5 chunk (24 grid points).
#     teardown+up with IOPS_RIG_NATS_REPLICAS=5 before the R=5 chunk.
#   The rig is NOT reset between rows/points within a chunk — calibrate
#   creates and deletes its own bucket/stream per invocation.
#
# Note on row count: the plan §M4 table has 7 distinct measurement rows
# (kv-put file, kv-get file, kv-keys K=1000, kv-keys K=3000, kv-watch W=1,
# kv-watch W=5, kv-put memory). The task description says "6 rows" which
# is a typo in the task; we implement all 7 from the plan table. The
# dry-run therefore prints 7 (or 48, or 55) lines, not 54.
#
# Structural failures abort. Per-row failures write failed.txt and continue.
# Leader-move on idle-pull retries up to N_RETRIES=2 times.
set -euo pipefail

# ---------------------------------------------------------------------------
# Paths.
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RIG_DIR="$(dirname "$SCRIPT_DIR")"

CALIBRATE_BIN="${RIG_DIR}/cmd/calibrate/calibrate"
REDUCE_M41_BIN="${RIG_DIR}/cmd/calibrate/reduce-m41/reduce-m41"

CONTAINERS_R3="iops-nats-1,iops-nats-2,iops-nats-3"
CONTAINERS_R5="iops-nats-1,iops-nats-2,iops-nats-3,iops-nats-4,iops-nats-5"
NATS_MONITOR="localhost:8222"

# Capture window per row/point in seconds. M4.1 spec: 60 s.
CAPTURE_SECS=60

# Max leader-move retries for idle-pull grid points.
N_RETRIES=2

# ---------------------------------------------------------------------------
# Defaults.
# ---------------------------------------------------------------------------
RUN_M4=true
RUN_M41=true
RESULTS_DIR="${RIG_DIR}/results/m4"
NATS_URLS="nats://localhost:4222"
DRY_RUN=false

# ---------------------------------------------------------------------------
# Arg parsing.
# ---------------------------------------------------------------------------
usage() {
    cat >&2 <<'EOF'
Usage: run-m4.sh [options]

Options:
  --m4-only           Run only the basic M4 KV table (skips M4.1 grid).
  --m41-only          Run only the M4.1 48-point idle-pull grid.
  --results-dir PATH  Parent directory for per-row subdirs (default: results/m4/).
  --nats-urls URL     NATS server URL(s) (default: nats://localhost:4222).
  --dry-run           Print the schedule and exit without running anything.
  -h, --help          Show this message and exit.

Environment:
  IOPS_RIG_NATS_IMAGE  Docker image for the NATS cluster (default: nats:2.12.6).
EOF
    exit 2
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --m4-only)      RUN_M41=false; shift ;;
        --m41-only)     RUN_M4=false;  shift ;;
        --results-dir)  RESULTS_DIR="$2"; shift 2 ;;
        --nats-urls)    NATS_URLS="$2";   shift 2 ;;
        --dry-run)      DRY_RUN=true;     shift ;;
        -h|--help)      usage ;;
        *) echo "run-m4.sh: unknown argument: $1" >&2; usage ;;
    esac
done

if [[ "$RUN_M4" == "false" && "$RUN_M41" == "false" ]]; then
    echo "run-m4.sh: --m4-only and --m41-only are mutually exclusive" >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# M4 basic schedule.
# Format per entry: "label|subdir_suffix|subcmd|extra_flags"
#   subdir_suffix is appended to "m4_" for the per-row results directory.
#   subcmd is the calibrate sub-command name.
#   extra_flags does NOT include --nats-urls; that is appended at run time.
# kv-keys and kv-watch are invoked once per row with a single value so
# each invocation maps 1:1 to a results directory and a CSV row.
# ---------------------------------------------------------------------------
M4_ROWS=(
    "kv-put(file,R3)|kv-put-file|kv-put|--storage file --replicas 3"
    "kv-get(file,R3,warm)|kv-get-file|kv-get|--storage file --replicas 3"
    "kv-keys(file,R3,K=1000)|kv-keys-k1000|kv-keys|--keys 1000 --storage file --replicas 3"
    "kv-keys(file,R3,K=3000)|kv-keys-k3000|kv-keys|--keys 3000 --storage file --replicas 3"
    "kv-watch(file,R3,W=1)|kv-watch-w1|kv-watch|--watchers 1 --storage file --replicas 3"
    "kv-watch(file,R3,W=5)|kv-watch-w5|kv-watch|--watchers 5 --storage file --replicas 3"
    "kv-put(memory,R3)|kv-put-mem|kv-put|--storage memory --replicas 3"
)

# M4.1 grid dimensions (6×2×2×2 = 48 points).
C_STREAM_VALUES=(0 100 500 1000 2000 3000)
FETCH_TIMEOUT_VALUES=("5s" "30s")
DATA_STORAGE_VALUES=("file" "memory")
REPLICA_VALUES=(3 5)

# ---------------------------------------------------------------------------
# Dry-run: print schedule and exit.
# ---------------------------------------------------------------------------
if [[ "$DRY_RUN" == "true" ]]; then
    echo "# run-m4.sh dry-run"
    if [[ "$RUN_M4" == "true" ]]; then
        for row in "${M4_ROWS[@]}"; do
            IFS='|' read -r label subdir subcmd flags <<< "$row"
            echo "M4   ${label}  dir=m4_${subdir}"
        done
    fi
    if [[ "$RUN_M41" == "true" ]]; then
        for R in "${REPLICA_VALUES[@]}"; do
            for storage in "${DATA_STORAGE_VALUES[@]}"; do
                for ft in "${FETCH_TIMEOUT_VALUES[@]}"; do
                    for C in "${C_STREAM_VALUES[@]}"; do
                        echo "M4.1 c=${C} ft=${ft} storage=${storage} R=${R}  dir=m41_c${C}_t${ft}_s${storage}_r${R}"
                    done
                done
            done
        done
    fi
    exit 0
fi

# ---------------------------------------------------------------------------
# Structural preflight (mirrors run-matrix.sh conventions).
# ---------------------------------------------------------------------------
if [[ ! -x "$CALIBRATE_BIN" ]]; then
    echo "run-m4.sh: calibrate binary not found or not executable: $CALIBRATE_BIN" >&2
    echo "run-m4.sh: build it with: go build -o cmd/calibrate/calibrate ./cmd/calibrate" >&2
    exit 1
fi
if [[ "$RUN_M41" == "true" && ! -x "$REDUCE_M41_BIN" ]]; then
    echo "run-m4.sh: reduce-m41 binary not found or not executable: $REDUCE_M41_BIN" >&2
    echo "run-m4.sh: build it with: go build -o cmd/calibrate/reduce-m41/reduce-m41 ./cmd/calibrate/reduce-m41" >&2
    exit 1
fi
if [[ ! -d /sys/fs/cgroup/system.slice ]]; then
    echo "run-m4.sh: cgroup v2 not available at /sys/fs/cgroup/system.slice; aborting." >&2
    exit 1
fi
if ! command -v iostat >/dev/null 2>&1; then
    echo "run-m4.sh: iostat not found on PATH (install sysstat); aborting." >&2
    exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
    echo "run-m4.sh: curl not found on PATH; required for jsz capture; aborting." >&2
    exit 1
fi

# node_exporter — soft prerequisite (same policy as run-matrix.sh §R3).
NODE_EXPORTER_AVAILABLE=false
if curl -sf --max-time 2 http://localhost:9100/metrics >/dev/null 2>&1; then
    NODE_EXPORTER_AVAILABLE=true
else
    echo "run-m4.sh: WARNING: node_exporter not reachable; stub will be written per row." >&2
fi

# ---------------------------------------------------------------------------
# Source _capture_lib.sh (stop_captures + verify_captures).
# The three arrays MUST be declared before sourcing.
# ---------------------------------------------------------------------------
CAPTURE_PIDS=()
CAPTURE_NAMES=()
CAPTURE_RCS=()

# shellcheck source=_capture_lib.sh
source "${SCRIPT_DIR}/_capture_lib.sh"

trap 'echo "run-m4.sh: signal received — stopping captures and exiting" >&2; stop_captures; exit 130' INT TERM
trap 'stop_captures' EXIT

# ---------------------------------------------------------------------------
# start_row_captures ROW_DIR REPLICAS
# Starts cgroup-io, iostat, jsz, and (if available) node-exporter captures.
# ---------------------------------------------------------------------------
start_row_captures() {
    local row_dir="$1"
    local replicas="$2"

    CAPTURE_PIDS=()
    CAPTURE_NAMES=()

    local containers
    if [[ "$replicas" -eq 5 ]]; then
        containers="$CONTAINERS_R5"
    else
        containers="$CONTAINERS_R3"
    fi

    bash "${SCRIPT_DIR}/capture-cgroup-io.sh" \
        --output "${row_dir}/cgroup_io.raw" \
        --duration "$CAPTURE_SECS" \
        --containers "$containers" &
    CAPTURE_PIDS+=($!)
    CAPTURE_NAMES+=("cgroup-io")

    bash "${SCRIPT_DIR}/capture-iostat.sh" \
        --output "${row_dir}/iostat.raw" \
        --duration "$CAPTURE_SECS" &
    CAPTURE_PIDS+=($!)
    CAPTURE_NAMES+=("iostat")

    bash "${SCRIPT_DIR}/capture-jsz.sh" \
        --output "${row_dir}/jsz.raw" \
        --duration "$CAPTURE_SECS" \
        --nats-nodes "$NATS_MONITOR" &
    CAPTURE_PIDS+=($!)
    CAPTURE_NAMES+=("jsz")

    if [[ "$NODE_EXPORTER_AVAILABLE" == "true" ]]; then
        bash "${SCRIPT_DIR}/capture-node-exporter.sh" \
            --output "${row_dir}/node_exporter.prom" \
            --duration "$CAPTURE_SECS" &
        CAPTURE_PIDS+=($!)
        CAPTURE_NAMES+=("node-exporter")
    else
        printf '# node_exporter unavailable; stub for aggregator presence check\n' \
            > "${row_dir}/node_exporter.prom"
    fi
}

# ---------------------------------------------------------------------------
# Campaign state.
# ---------------------------------------------------------------------------
mkdir -p "$RESULTS_DIR"

CAMPAIGN_START="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
CAMPAIGN_MANIFEST="${RESULTS_DIR}/campaign-manifest.yaml"
# M4.1 per-node calibration table (M3-consumable, plan §M4.1 line 772-775).
CONSOLIDATED_CSV="${RESULTS_DIR}/m4_calibration.csv"
# Basic M4 KV-paths table (per-RPC schema; separate from M4.1 because the
# tables have different shapes and M3 looks them up independently).
KV_PATHS_CSV="${RESULTS_DIR}/m4_kv_paths.csv"

success_count=0
fail_count=0
total_rows=0
declare -A RETRY_COUNTS
FAILURE_MSGS=()

NATS_IMAGE="${IOPS_RIG_NATS_IMAGE:-nats:2.12.6}"
NATS_IMAGE_DIGEST=""
if command -v docker >/dev/null 2>&1; then
    NATS_IMAGE_DIGEST="$(docker image inspect "$NATS_IMAGE" \
        --format '{{index .RepoDigests 0}}' 2>/dev/null || true)"
fi

# M4.1 schema header (9 columns) — matches calibrate.M41RowSchemaHeader.
M41_HEADER="c_stream,fetch_timeout_s,data_storage,replicas,node_role,block_read_iops,block_write_iops,ci_low,ci_high"
echo "$M41_HEADER" > "$CONSOLIDATED_CSV"
# Basic-KV header (driver-row schema from emitCSV in cmd/calibrate/main.go).
KV_PATHS_HEADER="path,storage,replicas,rate,duration_s,bytes_per_op,read_rpc_ops,write_rpc_ops,wrapper_total_ops"
echo "$KV_PATHS_HEADER" > "$KV_PATHS_CSV"

# ---------------------------------------------------------------------------
# run_m4_row LABEL SUBDIR SUBCMD FLAGS
# Runs one basic M4 calibration row with full capture lifecycle.
# ---------------------------------------------------------------------------
run_m4_row() {
    local label="$1" subdir="$2" subcmd="$3" flags="$4"
    local row_dir="${RESULTS_DIR}/${subdir}"
    total_rows=$(( total_rows + 1 ))
    mkdir -p "$row_dir"

    echo ""
    echo "=== M4 ${label}  dir=${subdir} ==="

    start_row_captures "$row_dir" 3
    echo "  captures started (PIDs: ${CAPTURE_PIDS[*]:-none})"

    local cal_rc=0
    # shellcheck disable=SC2086
    "$CALIBRATE_BIN" "$subcmd" $flags --nats-urls "$NATS_URLS" \
        > "${row_dir}/calibrate.csv" \
        2> "${row_dir}/calibrate.stderr" \
        || cal_rc=$?

    stop_captures

    if [[ "$cal_rc" -ne 0 ]]; then
        printf 'calibrate exit code %d\n' "$cal_rc" > "${row_dir}/failed.txt"
        echo "  FAILED (calibrate rc=${cal_rc}); continuing." >&2
        fail_count=$(( fail_count + 1 ))
        FAILURE_MSGS+=("${subdir}(calibrate rc=${cal_rc})")
        return 1
    fi

    if ! verify_captures "$row_dir"; then
        echo "  FAILED (capture verify); continuing." >&2
        fail_count=$(( fail_count + 1 ))
        FAILURE_MSGS+=("${subdir}(capture-verify)")
        return 1
    fi

    cat "${row_dir}/calibrate.csv" >> "$KV_PATHS_CSV"
    success_count=$(( success_count + 1 ))
    echo "  OK."
    return 0
}

# ---------------------------------------------------------------------------
# run_m41_point C_STREAM FETCH_TIMEOUT DATA_STORAGE REPLICAS
# Retries up to N_RETRIES on leader-move exit. Each attempt gets its own
# attempt-N subdir; on success the winning calibrate.csv is promoted to
# the point's top-level dir for consolidation.
# ---------------------------------------------------------------------------
run_m41_point() {
    local C="$1" ft="$2" storage="$3" replicas="$4"
    local subdir="m41_c${C}_t${ft}_s${storage}_r${replicas}"
    local row_dir="${RESULTS_DIR}/${subdir}"
    total_rows=$(( total_rows + 1 ))
    mkdir -p "$row_dir"
    RETRY_COUNTS["$subdir"]=0

    echo ""
    echo "=== M4.1 c=${C} ft=${ft} storage=${storage} R=${replicas}  dir=${subdir} ==="

    local attempt=0
    local max_attempts=$(( N_RETRIES + 1 ))

    while [[ "$attempt" -lt "$max_attempts" ]]; do
        attempt=$(( attempt + 1 ))
        if [[ "$attempt" -gt 1 ]]; then
            echo "  retry ${attempt}/${max_attempts} (leader-move)"
            RETRY_COUNTS["$subdir"]=$(( RETRY_COUNTS["$subdir"] + 1 ))
        fi

        local attempt_dir="${row_dir}/attempt-${attempt}"
        mkdir -p "$attempt_dir"

        start_row_captures "$attempt_dir" "$replicas"
        echo "  captures started (PIDs: ${CAPTURE_PIDS[*]:-none})"

        local cal_rc=0
        "$CALIBRATE_BIN" idle-pull \
            --c-stream "$C" \
            --fetch-timeout "$ft" \
            --data-storage "$storage" \
            --replicas "$replicas" \
            --duration "${CAPTURE_SECS}s" \
            --nats-urls "$NATS_URLS" \
            > "${attempt_dir}/calibrate.csv" \
            2> "${attempt_dir}/calibrate.stderr" \
            || cal_rc=$?

        stop_captures

        if [[ "$cal_rc" -ne 0 ]]; then
            if grep -q "stream leader moved during capture" "${attempt_dir}/calibrate.stderr" 2>/dev/null; then
                echo "  leader-move detected (attempt ${attempt}/${max_attempts})" >&2
                if [[ "$attempt" -lt "$max_attempts" ]]; then
                    continue
                fi
                printf 'leader-move after %d attempts\n' "$attempt" > "${row_dir}/failed.txt"
                echo "  FAILED (leader-move, retries exhausted); continuing." >&2
                fail_count=$(( fail_count + 1 ))
                FAILURE_MSGS+=("${subdir}(leader-move,attempts=${attempt})")
                return 1
            fi
            printf 'calibrate exit code %d\n' "$cal_rc" > "${row_dir}/failed.txt"
            echo "  FAILED (calibrate rc=${cal_rc}); continuing." >&2
            fail_count=$(( fail_count + 1 ))
            FAILURE_MSGS+=("${subdir}(calibrate rc=${cal_rc})")
            return 1
        fi

        if ! verify_captures "$attempt_dir"; then
            if [[ "$attempt" -lt "$max_attempts" ]]; then
                echo "  capture verify failed (attempt ${attempt}); retrying..." >&2
                continue
            fi
            printf 'capture verify failed after %d attempts\n' "$attempt" > "${row_dir}/failed.txt"
            echo "  FAILED (capture verify, retries exhausted); continuing." >&2
            fail_count=$(( fail_count + 1 ))
            FAILURE_MSGS+=("${subdir}(capture-verify)")
            return 1
        fi

        # Success: promote winning CSV + reduce raw cgroup capture to
        # the M4.1 per-node table M3 consumes (plan §M4.1 line 772-775).
        cp "${attempt_dir}/calibrate.csv" "${row_dir}/calibrate.csv"
        cp "${attempt_dir}/cgroup_io.raw" "${row_dir}/cgroup_io.raw"

        local reduce_rc=0
        "$REDUCE_M41_BIN" \
            --point-dir "${row_dir}" \
            --output "${row_dir}/m41_rows.csv" \
            2> "${row_dir}/reduce-m41.stderr" \
            || reduce_rc=$?
        if [[ "$reduce_rc" -ne 0 ]]; then
            printf 'reduce-m41 exit code %d\n' "$reduce_rc" > "${row_dir}/failed.txt"
            echo "  FAILED (reduce-m41 rc=${reduce_rc}); continuing." >&2
            fail_count=$(( fail_count + 1 ))
            FAILURE_MSGS+=("${subdir}(reduce-m41 rc=${reduce_rc})")
            return 1
        fi
        cat "${row_dir}/m41_rows.csv" >> "$CONSOLIDATED_CSV"
        success_count=$(( success_count + 1 ))
        echo "  OK."
        return 0
    done
}

# ---------------------------------------------------------------------------
# R=3 rig cycle: M4 basic + M4.1 R=3 chunk.
# ---------------------------------------------------------------------------
echo ""
echo "run-m4.sh: bringing up R=3 rig..."
IOPS_RIG_NATS_REPLICAS=3 make -C "$RIG_DIR" reset

if [[ "$RUN_M4" == "true" ]]; then
    echo ""
    echo "=== M4 basic KV table (7 rows, R=3) ==="
    for row in "${M4_ROWS[@]}"; do
        IFS='|' read -r label subdir subcmd flags <<< "$row"
        run_m4_row "$label" "m4_${subdir}" "$subcmd" "$flags" || true
    done
fi

if [[ "$RUN_M41" == "true" ]]; then
    echo ""
    echo "=== M4.1 idle-pull grid: R=3 chunk (24 points) ==="
    for storage in "${DATA_STORAGE_VALUES[@]}"; do
        for ft in "${FETCH_TIMEOUT_VALUES[@]}"; do
            for C in "${C_STREAM_VALUES[@]}"; do
                run_m41_point "$C" "$ft" "$storage" 3 || true
            done
        done
    done
fi

# ---------------------------------------------------------------------------
# R=5 rig cycle: M4.1 R=5 chunk (M4 basic is all R=3).
# ---------------------------------------------------------------------------
if [[ "$RUN_M41" == "true" ]]; then
    echo ""
    echo "run-m4.sh: bringing up R=5 rig..."
    IOPS_RIG_NATS_REPLICAS=5 make -C "$RIG_DIR" reset

    echo ""
    echo "=== M4.1 idle-pull grid: R=5 chunk (24 points) ==="
    for storage in "${DATA_STORAGE_VALUES[@]}"; do
        for ft in "${FETCH_TIMEOUT_VALUES[@]}"; do
            for C in "${C_STREAM_VALUES[@]}"; do
                run_m41_point "$C" "$ft" "$storage" 5 || true
            done
        done
    done
fi

# ---------------------------------------------------------------------------
# Campaign manifest.
# ---------------------------------------------------------------------------
END_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

{
    echo "start_time: ${CAMPAIGN_START}"
    echo "end_time: ${END_TIME}"
    echo "total_rows: ${total_rows}"
    echo "success_count: ${success_count}"
    echo "failure_count: ${fail_count}"
    echo "nats_image: ${NATS_IMAGE}"
    if [[ -n "$NATS_IMAGE_DIGEST" ]]; then
        echo "nats_image_digest: ${NATS_IMAGE_DIGEST}"
    fi
    if [[ ${#FAILURE_MSGS[@]} -eq 0 ]]; then
        echo "failures: []"
    else
        echo "failures:"
        for f in "${FAILURE_MSGS[@]}"; do
            echo "  - ${f}"
        done
    fi
    if [[ ${#RETRY_COUNTS[@]} -eq 0 ]]; then
        echo "retry_counts: {}"
    else
        echo "retry_counts:"
        for key in "${!RETRY_COUNTS[@]}"; do
            if [[ "${RETRY_COUNTS[$key]}" -gt 0 ]]; then
                echo "  ${key}: ${RETRY_COUNTS[$key]}"
            fi
        done
    fi
    echo "consolidated_csv: ${CONSOLIDATED_CSV}"
    echo "kv_paths_csv: ${KV_PATHS_CSV}"
} > "$CAMPAIGN_MANIFEST"

# ---------------------------------------------------------------------------
# Summary.
# ---------------------------------------------------------------------------
echo ""
echo "=== M4 campaign complete ==="
echo "  Total:    ${total_rows}"
echo "  Success:  ${success_count}"
echo "  Failed:   ${fail_count}"
echo "  M4.1 CSV: ${CONSOLIDATED_CSV}"
echo "  M4 KV:    ${KV_PATHS_CSV}"
echo "  Manifest: ${CAMPAIGN_MANIFEST}"

if [[ "$fail_count" -gt 0 ]]; then
    exit 1
fi
