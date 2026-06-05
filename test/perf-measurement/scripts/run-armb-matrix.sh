#!/usr/bin/env bash
# run-armb-matrix.sh — "Arm B" / production-config scaling sweep.
#
# Focused sibling of run-load-matrix.sh. Where run-load-matrix.sh sweeps the
# expensive DEFAULT-durability baseline across file+memory cluster storage and
# dynamic+queue modes, this runner measures ONE config — the one you'd actually
# deploy — across the N×k grid:
#
#   dynamic consumers, file data stream, RF=5 (5-node cluster),
#   per-partition consumer state in MEMORY (WithConsumerMemoryStorage), with
#   consumer Replicas = R (default 3 = HA-preserving, tolerate-1-failure).
#
# Rationale (see docs/plans/perf-measurement/findings*.md + the prior IOPS
# study's §4/§5 decision tree): the default file+R5 consumer state file is the
# ~72-81% IOPS dominator; memory storage removes it, R=3 keeps single-node HA.
# This sweep builds the cost model + latency curve for THAT config as N→5000.
#
# Matrix:  N ∈ {1000,2000,3000,5000} × k ∈ {1,2,4}  = 12 cells × REPS.
#
# Window: warmup/capture default to 60s/60s (HALF the baseline's 120/120). The
# harness anchors its warmup AFTER WaitStableAll + producer start, so the long
# consumer-creation IOPS ramp lives in STARTUP, not warmup — warmup+capture both
# sit on the steady-state plateau, which is flat to ~±10% over any 60s slice
# (verified against results/first + results/load1 cgroup_io.raw time series).
#
# Reuses the proven capture lifecycle (_capture_lib.sh), per-cell isolation
# verify (verify-isolation.sh), and resume gate (status:ok + latency.json) from
# run-load-matrix.sh. Does NOT modify that script.
#
# Usage:
#   run-armb-matrix.sh --seed N [--results-dir PATH] [--reps R]
#                      [--warmup-secs S] [--capture-secs S]
#                      [--consumer-replicas R] [--cells L1,L2,...] [--dry-run]
#
# REQUIRED: --seed N  (recorded in every per-run run-meta.yaml).
set -euo pipefail

# ---------------------------------------------------------------------------
# Paths — derived from this script's location; works from any cwd.
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RIG_DIR="$(dirname "$SCRIPT_DIR")"
HARNESS_BIN="${RIG_DIR}/cmd/harness/harness"

# Fixed for this study (production config on the user's typical RF=5 cluster).
REPLICAS=5
CONTAINERS="perf-nats-1,perf-nats-2,perf-nats-3,perf-nats-4,perf-nats-5"
NATS_MONITOR="localhost:8222"
HARNESS_CPUSET="8-15,24-31"
STORAGE=file          # data + KV stream storage (durable messages)
MODE=dynamic          # one pull consumer per partition

# Sweep axes.
N_VALUES=(1000 2000 3000 5000)
K_VALUES=(1 2 4)

# Defaults (overridable by flags).
WARMUP_SECS=60
CAPTURE_SECS=60
REPS=3
CONSUMER_REPLICAS=3   # R: per-consumer raft replica count (memory-backed)

# ---------------------------------------------------------------------------
# Usage.
# ---------------------------------------------------------------------------
usage() {
    cat >&2 <<'EOF'
Usage: run-armb-matrix.sh --seed N [options]

Required:
  --seed N             Integer seed recorded in every per-run run-meta.yaml.

Options:
  --results-dir PATH   Parent dir for per-cell subdirs (default: results/armb).
  --reps N             Replicates per cell (default: 3).
  --warmup-secs N      Per-run warmup window seconds (default: 60).
  --capture-secs N     Per-run capture window seconds (default: 60).
  --consumer-replicas R  Per-consumer raft replica count R (default: 3).
                       Must be 1 <= R <= 5 (the stream RF). Memory storage is
                       always on in this runner.
  --cells LABELS       Comma-separated cell-label filter (default: all 12).
                       Example: --cells armb-N5000-k4,armb-N1000-k1
  --dry-run            Print the plan and exit; no live cluster, no side effects.
  -h, --help           Show this message.

Environment:
  PERF_RIG_NATS_IMAGE         NATS image (default: nats:2.12.6).
  PERF_RIG_NATS_IMAGE_DIGEST  Pre-resolved digest for manifest (optional).
EOF
    exit 2
}

# ---------------------------------------------------------------------------
# Argument parsing.
# ---------------------------------------------------------------------------
SEED=""
RESULTS_DIR="${RIG_DIR}/results/armb"
CELLS_ARG=""
DRY_RUN=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --seed)              SEED="$2";              shift 2 ;;
        --results-dir)       RESULTS_DIR="$2";       shift 2 ;;
        --reps)              REPS="$2";              shift 2 ;;
        --warmup-secs)       WARMUP_SECS="$2";       shift 2 ;;
        --capture-secs)      CAPTURE_SECS="$2";      shift 2 ;;
        --consumer-replicas) CONSUMER_REPLICAS="$2"; shift 2 ;;
        --cells)             CELLS_ARG="$2";         shift 2 ;;
        --dry-run)           DRY_RUN=true;           shift ;;
        -h|--help)           usage ;;
        *) echo "run-armb-matrix.sh: unknown argument: $1" >&2; usage ;;
    esac
done

[[ -z "$SEED" ]] && { echo "run-armb-matrix.sh: --seed is required" >&2; usage; }
if ! [[ "$SEED" =~ ^-?[0-9]+$ ]]; then
    echo "run-armb-matrix.sh: --seed must be an integer, got: $SEED" >&2; exit 1
fi
if ! [[ "$REPS" =~ ^[0-9]+$ ]] || [[ "$REPS" -lt 1 ]]; then
    echo "run-armb-matrix.sh: --reps must be a positive integer, got: $REPS" >&2; exit 1
fi
if ! [[ "$WARMUP_SECS" =~ ^[0-9]+$ ]] || [[ "$WARMUP_SECS" -lt 5 ]]; then
    echo "run-armb-matrix.sh: --warmup-secs must be an integer >= 5, got: $WARMUP_SECS" >&2; exit 1
fi
if ! [[ "$CAPTURE_SECS" =~ ^[0-9]+$ ]] || [[ "$CAPTURE_SECS" -lt 10 ]]; then
    echo "run-armb-matrix.sh: --capture-secs must be an integer >= 10, got: $CAPTURE_SECS" >&2; exit 1
fi
if ! [[ "$CONSUMER_REPLICAS" =~ ^[0-9]+$ ]] || [[ "$CONSUMER_REPLICAS" -lt 1 || "$CONSUMER_REPLICAS" -gt "$REPLICAS" ]]; then
    echo "run-armb-matrix.sh: --consumer-replicas must be 1..${REPLICAS} (the stream RF), got: $CONSUMER_REPLICAS" >&2; exit 1
fi

# capture_duration N — seconds the capture sidecars must run to FULLY cover the
# harness's measurement window. The harness does startup (WaitStableAll, up to
# its default budget max(120s, N*120ms)) BEFORE warmup+capture, so the sidecars
# must outlast startup+warmup+capture or they self-expire mid-window. The parser
# windows [post-warmup-start, EOF] (no upper bound), so an early EOF silently
# TRUNCATES the IOPS/CPU/RSS mean — and at high N (startup > sidecar duration)
# leaves the window EMPTY. The sidecars are killed by stop_captures the instant
# the harness exits, so a generous upper bound is free; it only prevents early
# self-expiry. Mirrors run-first-round.sh's fixed CAPDUR=800, scaled per-N.
capture_duration() {
    local n="$1"
    local budget=$(( n * 120 / 1000 ))   # N*120ms in s = harness default startup budget
    (( budget < 120 )) && budget=120
    echo $(( budget + WARMUP_SECS + CAPTURE_SECS + 60 ))
}

# ---------------------------------------------------------------------------
# Cell enumeration: N × k, ascending-N ramp.  Record: <label> <N> <k>
# ---------------------------------------------------------------------------
CELLS=()
for n in "${N_VALUES[@]}"; do
    for k in "${K_VALUES[@]}"; do
        CELLS+=("armb-N${n}-k${k} ${n} ${k}")
    done
done

# Apply optional --cells filter.
if [[ -n "$CELLS_ARG" ]]; then
    declare -A _want=()
    IFS=',' read -ra _arr <<< "$CELLS_ARG"
    for l in "${_arr[@]}"; do _want["$l"]=1; done
    FILTERED=()
    for rec in "${CELLS[@]}"; do
        read -r label _ <<< "$rec"
        [[ -n "${_want[$label]+x}" ]] && FILTERED+=("$rec")
    done
    for l in "${_arr[@]}"; do
        ok=false
        for rec in "${CELLS[@]}"; do read -r label _ <<< "$rec"; [[ "$label" == "$l" ]] && { ok=true; break; }; done
        [[ "$ok" == false ]] && { echo "run-armb-matrix.sh: unknown cell label '$l'" >&2; exit 1; }
    done
    CELLS=("${FILTERED[@]}")
fi

# ---------------------------------------------------------------------------
# Dry-run.
# ---------------------------------------------------------------------------
if [[ "$DRY_RUN" == "true" ]]; then
    echo "# run-armb-matrix.sh dry-run  seed=${SEED}  cells=${#CELLS[@]}  reps=${REPS}  total_runs=$(( ${#CELLS[@]} * REPS ))"
    echo "# config: ${MODE} consumer, ${STORAGE} data+KV, RF=${REPLICAS}, memory consumer state, R=${CONSUMER_REPLICAS}"
    echo "# window: warmup=${WARMUP_SECS}s capture=${CAPTURE_SECS}s  harness_cpuset=${HARNESS_CPUSET}"
    echo "# capture-sidecar duration (covers startupBudget+warmup+capture+60): N1000=$(capture_duration 1000)s N2000=$(capture_duration 2000)s N3000=$(capture_duration 3000)s N5000=$(capture_duration 5000)s"
    echo "# order: ascending-N ramp"
    for rec in "${CELLS[@]}"; do
        read -r label n k <<< "$rec"
        for (( r=1; r<=REPS; r++ )); do
            echo "CELL ${label} rep${r}  flags=--load --replicas ${REPLICAS} --n ${n} --workers $(( n / 50 )) --per-worker-rate ${k} --consumer-mode ${MODE} --kv-storage ${STORAGE} --data-storage ${STORAGE} --consumer-memory-storage=true --consumer-replicas=${CONSUMER_REPLICAS} --warmup ${WARMUP_SECS}s --capture-window ${CAPTURE_SECS}s"
        done
    done
    exit 0
fi

# ---------------------------------------------------------------------------
# Structural pre-flight (live runs only).
# ---------------------------------------------------------------------------
if [[ ! -x "$HARNESS_BIN" ]]; then
    echo "run-armb-matrix.sh: harness binary not found/executable: $HARNESS_BIN" >&2
    echo "run-armb-matrix.sh: build it with: go build -o cmd/harness/harness ./cmd/harness" >&2
    exit 1
fi
if [[ ! -x "${SCRIPT_DIR}/verify-isolation.sh" ]]; then
    echo "run-armb-matrix.sh: verify-isolation.sh not found/executable" >&2; exit 1
fi
for tool in docker taskset curl iostat jq; do
    command -v "$tool" >/dev/null 2>&1 || { echo "run-armb-matrix.sh: required tool '$tool' not in PATH" >&2; exit 1; }
done
if [[ ! -d /sys/fs/cgroup/system.slice ]]; then
    echo "run-armb-matrix.sh: cgroup v2 not available; cgroup_io.raw is mandatory; aborting." >&2; exit 1
fi

# ---------------------------------------------------------------------------
# Capture lifecycle (shared helpers).
# ---------------------------------------------------------------------------
CAPTURE_PIDS=()
CAPTURE_NAMES=()
CAPTURE_RCS=()
# shellcheck source=_capture_lib.sh
source "${SCRIPT_DIR}/_capture_lib.sh"

HARNESS_PID=""
trap 'echo "run-armb-matrix.sh: signal — killing harness + captures" >&2; [[ -n "$HARNESS_PID" ]] && { kill "$HARNESS_PID" 2>/dev/null || true; wait "$HARNESS_PID" 2>/dev/null || true; }; stop_captures; exit 130' INT TERM
trap 'stop_captures' EXIT

mkdir -p "$RESULTS_DIR"
log() { echo "run-armb-matrix.sh: $*"; }

start_captures() {
    local run_dir="$1" duration="$2"
    CAPTURE_PIDS=(); CAPTURE_NAMES=()
    bash "${SCRIPT_DIR}/capture-cgroup-io.sh"     --output "${run_dir}/cgroup_io.raw"     --duration "$duration" --containers "$CONTAINERS" & CAPTURE_PIDS+=($!); CAPTURE_NAMES+=("cgroup-io")
    bash "${SCRIPT_DIR}/capture-cgroup-cpumem.sh" --output "${run_dir}/cgroup_cpumem.raw" --duration "$duration" --containers "$CONTAINERS" & CAPTURE_PIDS+=($!); CAPTURE_NAMES+=("cgroup-cpumem")
    bash "${SCRIPT_DIR}/capture-iostat.sh"        --output "${run_dir}/iostat.raw"        --duration "$duration" & CAPTURE_PIDS+=($!); CAPTURE_NAMES+=("iostat")
    bash "${SCRIPT_DIR}/capture-jsz.sh"           --output "${run_dir}/jsz.raw"           --duration "$duration" --nats-nodes "$NATS_MONITOR" & CAPTURE_PIDS+=($!); CAPTURE_NAMES+=("jsz")
}

# resume_verdict RUN_DIR -> skip-valid | rerun:<reason> | fresh  (read-only)
resume_verdict() {
    local d="$1"
    [[ ! -d "$d" ]] && { echo "fresh"; return 0; }
    local manifest="${d}/manifest.yaml"
    [[ ! -f "$manifest" ]] && { echo "rerun:no-manifest"; return 0; }
    if ! grep -q '^status: ok$' "$manifest"; then
        local st; st="$(grep -m1 '^status:' "$manifest" | sed 's/^status:[[:space:]]*//' || true)"
        echo "rerun:status=${st:-unknown}"; return 0
    fi
    [[ ! -f "${d}/latency.json" ]] && { echo "rerun:no-latency-json"; return 0; }
    echo "skip-valid"
}

# run_cell label N k rep -> 0 on completed run, 1 on per-cell failure (logged).
run_cell() {
    local label="$1" n="$2" k="$3" rep="$4"
    local workers=$(( n / 50 ))
    local run_dir="${RESULTS_DIR}/${label}/rep${rep}"

    echo ""
    echo "=== ${label} rep${rep}  (N=${n} k=${k} R=${CONSUMER_REPLICAS} mem-consumer file/RF5) ==="

    local verdict; verdict="$(resume_verdict "$run_dir")"
    case "$verdict" in
        skip-valid) log "SKIP ${label} rep${rep}: valid complete cell"; return 0 ;;
        rerun:*)
            log "RERUN ${label} rep${rep}: invalid cell (${verdict#rerun:}) — deleting ${run_dir}"
            [[ -n "$run_dir" && "$run_dir" != "/" ]] && rm -rf "$run_dir"
            ;;
        fresh) ;;
    esac
    mkdir -p "$run_dir"

    {
        echo "seed: ${SEED}"
        echo "label: ${label}"
        echo "n: ${n}"
        echo "k: ${k}"
        echo "workers: ${workers}"
        echo "storage: ${STORAGE}"
        echo "mode: ${MODE}"
        echo "consumer_memory_storage: true"
        echo "consumer_replicas: ${CONSUMER_REPLICAS}"
        echo "rep: ${rep}"
        echo "replicas: ${REPLICAS}"
        echo "warmup_secs: ${WARMUP_SECS}"
        echo "capture_secs: ${CAPTURE_SECS}"
    } > "${run_dir}/run-meta.yaml"

    log "resetting rig (replicas=${REPLICAS}, ${STORAGE} storage)..."
    if ! PERF_RIG_NATS_REPLICAS="${REPLICAS}" make -C "$RIG_DIR" reset; then
        echo "make reset failed" > "${run_dir}/failed.txt"
        log "FAILED ${label} rep${rep}: make reset"; return 1
    fi

    local cap_dur; cap_dur="$(capture_duration "$n")"
    start_captures "$run_dir" "$cap_dur"
    log "captures started (sidecar duration ${cap_dur}s, PIDs: ${CAPTURE_PIDS[*]:-none})"

    log "launching harness (background, pinned ${HARNESS_CPUSET})..."
    PERF_RIG_NATS_IMAGE="${PERF_RIG_NATS_IMAGE:-nats:2.12.6}" \
    PERF_RIG_NATS_IMAGE_DIGEST="${PERF_RIG_NATS_IMAGE_DIGEST:-}" \
        taskset -c "$HARNESS_CPUSET" "$HARNESS_BIN" \
            --load \
            --replicas "$REPLICAS" \
            --n "$n" \
            --workers "$workers" \
            --per-worker-rate "$k" \
            --consumer-mode "$MODE" \
            --kv-storage "$STORAGE" \
            --data-storage "$STORAGE" \
            --consumer-memory-storage=true \
            --consumer-replicas="$CONSUMER_REPLICAS" \
            --warmup "${WARMUP_SECS}s" \
            --capture-window "${CAPTURE_SECS}s" \
            --output-dir "$run_dir" &
    HARNESS_PID=$!
    log "harness PID=${HARNESS_PID}; sleeping 2s before isolation verify..."
    sleep 2

    if ! "${SCRIPT_DIR}/verify-isolation.sh" "$HARNESS_PID" "$CONTAINERS"; then
        log "ABORT ${label} rep${rep}: isolation mismatch — killing harness PID ${HARNESS_PID}"
        kill "$HARNESS_PID" 2>/dev/null || true
        wait "$HARNESS_PID" 2>/dev/null || true
        HARNESS_PID=""; stop_captures
        echo "isolation mismatch (cpuset)" > "${run_dir}/failed.txt"
        return 1
    fi

    log "isolation OK; waiting for harness (startup + warmup ${WARMUP_SECS}s + capture ${CAPTURE_SECS}s)..."
    local harness_rc=0
    wait "$HARNESS_PID" || harness_rc=$?
    HARNESS_PID=""

    log "stopping captures..."
    stop_captures

    if [[ "$harness_rc" -ne 0 ]]; then
        printf 'harness exit code %d\n' "$harness_rc" > "${run_dir}/failed.txt"
        log "FAILED ${label} rep${rep}: harness exit ${harness_rc}; continuing."
        return 1
    fi

    # node_exporter not captured in load mode; stub for the presence check.
    [[ ! -s "${run_dir}/node_exporter.prom" ]] && printf '# node_exporter not captured in load mode\n' > "${run_dir}/node_exporter.prom"
    if ! verify_captures "$run_dir"; then
        log "FAILED ${label} rep${rep}: capture verify; continuing."; return 1
    fi

    local post; post="$(resume_verdict "$run_dir")"
    if [[ "$post" != "skip-valid" ]]; then
        log "FAILED ${label} rep${rep}: harness produced invalid cell (${post#rerun:}); continuing."
        return 1
    fi

    log "OK ${label} rep${rep}."
    return 0
}

# ---------------------------------------------------------------------------
# Main.
# ---------------------------------------------------------------------------
log "Arm B: ${#CELLS[@]} cells × ${REPS} reps = $(( ${#CELLS[@]} * REPS )) runs (ascending-N ramp)"
log "config: ${MODE} consumer, ${STORAGE} data+KV, RF=${REPLICAS}, MEMORY consumer state, R=${CONSUMER_REPLICAS}"
log "window: warmup=${WARMUP_SECS}s capture=${CAPTURE_SECS}s   results: ${RESULTS_DIR}"

success_count=0
fail_count=0
for rec in "${CELLS[@]}"; do
    read -r label n k <<< "$rec"
    for (( r=1; r<=REPS; r++ )); do
        if run_cell "$label" "$n" "$k" "$r"; then
            success_count=$(( success_count + 1 ))
        else
            fail_count=$(( fail_count + 1 ))
        fi
    done
done

echo ""
echo "=== Arm B campaign complete ==="
echo "  Cells:   ${#CELLS[@]} × ${REPS} reps"
echo "  Success: ${success_count}"
echo "  Failed:  ${fail_count}"
[[ "$fail_count" -gt 0 ]] && exit 1
exit 0
