#!/usr/bin/env bash
# run-meta-sweep.sh — metacontroller snapshot / meta_compact_size sweep.
#
# Answers "metalayer cost + how to tune meta_compact" by varying
# meta_compact_size (the meta-raft WAL-size compaction threshold) at a fixed
# partition count and measuring the resulting snapshot frequency / duration /
# WAL peak. Verify-first finding (from the Arm B run): at N<=5000 the meta WAL
# peaks at ~2-6 MB, so the upward 16/64 MB sweep is INERT — this runner sweeps
# DOWNWARD (1/4 MB) so the threshold is actually crossed, and supports pushing
# N to 10000 to reach the high-cost regime behind the "1.x second" incident.
#
# Fixed config (the production consumer config — meta-layer cost is driven by
# asset COUNT, not consumer storage, so this is consistent and immaterial):
#   dynamic consumers, file data+KV, RF=5, MEMORY consumer state, R=3, k=2.
#
# One invocation = one N across a list of meta_compact configs. Run twice:
#   run-meta-sweep.sh --seed 42 --n 5000  --configs default,1MB,4MB
#   run-meta-sweep.sh --seed 42 --n 10000 --configs default,4MB
#
# jsz is polled at 1 Hz (--poll-interval 1) so rapid creation-phase snapshots
# are not undercounted. Reuses the shared capture lifecycle, per-cell isolation
# verify, resume gate, and the startup-aware capture duration from the Arm B
# runner. Requires the hardened capture-jsz.sh (no consumers=true; non-fatal).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RIG_DIR="$(dirname "$SCRIPT_DIR")"
HARNESS_BIN="${RIG_DIR}/cmd/harness/harness"

REPLICAS=5
CONTAINERS="perf-nats-1,perf-nats-2,perf-nats-3,perf-nats-4,perf-nats-5"
NATS_MONITOR="localhost:8222"
HARNESS_CPUSET="8-15,24-31"
STORAGE=file
MODE=dynamic
K=2
CONSUMER_REPLICAS=3
JSZ_POLL_INTERVAL=1

WARMUP_SECS=60
CAPTURE_SECS=60
REPS=3

# config label -> nats config file (relative to docker/). "default" => compose default.
config_file() {
    case "$1" in
        default) echo "" ;;
        1MB)     echo "./nats-server-meta1.conf" ;;
        4MB)     echo "./nats-server-meta4.conf" ;;
        16MB)    echo "./nats-server-meta16.conf" ;;
        64MB)    echo "./nats-server-meta64.conf" ;;
        *)       echo "__INVALID__" ;;
    esac
}

# capture_duration N — cover startupBudget(N)=max(120s,N*120ms) + warmup + capture
# + margin so the sidecars never self-expire mid-window (see run-armb-matrix.sh).
capture_duration() {
    local n="$1"
    local budget=$(( n * 120 / 1000 ))
    (( budget < 120 )) && budget=120
    echo $(( budget + WARMUP_SECS + CAPTURE_SECS + 60 ))
}

usage() {
    cat >&2 <<EOF
Usage: run-meta-sweep.sh --seed N --n PARTITIONS --configs L1,L2,... [options]

Required:
  --seed N         Integer seed recorded in every run-meta.yaml.
  --n PARTITIONS   Partition count for this sweep (e.g. 5000 or 10000).
  --configs LIST   Comma-separated meta_compact labels from:
                   default, 1MB, 4MB, 16MB, 64MB.

Options:
  --reps N            Replicates per config (default: 3).
  --results-dir PATH  Parent dir for per-cell subdirs (default: results/meta-N<PARTITIONS>).
  --warmup-secs N     Warmup seconds (default: 60).
  --capture-secs N    Capture seconds (default: 60).
  --dry-run           Print the plan and exit.
  -h, --help          Show this message.
EOF
    exit 2
}

SEED=""
N=""
CONFIGS_ARG=""
RESULTS_DIR=""
DRY_RUN=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --seed)         SEED="$2";         shift 2 ;;
        --n)            N="$2";            shift 2 ;;
        --configs)      CONFIGS_ARG="$2";  shift 2 ;;
        --reps)         REPS="$2";         shift 2 ;;
        --results-dir)  RESULTS_DIR="$2";  shift 2 ;;
        --warmup-secs)  WARMUP_SECS="$2";  shift 2 ;;
        --capture-secs) CAPTURE_SECS="$2"; shift 2 ;;
        --dry-run)      DRY_RUN=true;      shift ;;
        -h|--help)      usage ;;
        *) echo "run-meta-sweep.sh: unknown argument: $1" >&2; usage ;;
    esac
done

[[ -z "$SEED" ]]        && { echo "run-meta-sweep.sh: --seed is required" >&2; usage; }
[[ -z "$N" ]]           && { echo "run-meta-sweep.sh: --n is required" >&2; usage; }
[[ -z "$CONFIGS_ARG" ]] && { echo "run-meta-sweep.sh: --configs is required" >&2; usage; }
[[ "$SEED" =~ ^-?[0-9]+$ ]] || { echo "run-meta-sweep.sh: --seed must be an integer" >&2; exit 1; }
[[ "$N" =~ ^[0-9]+$ ]]      || { echo "run-meta-sweep.sh: --n must be a positive integer" >&2; exit 1; }
[[ "$REPS" =~ ^[0-9]+$ && "$REPS" -ge 1 ]] || { echo "run-meta-sweep.sh: --reps must be >= 1" >&2; exit 1; }
[[ -z "$RESULTS_DIR" ]] && RESULTS_DIR="${RIG_DIR}/results/meta-N${N}"

IFS=',' read -ra CONFIGS <<< "$CONFIGS_ARG"
for c in "${CONFIGS[@]}"; do
    [[ "$(config_file "$c")" == "__INVALID__" ]] && { echo "run-meta-sweep.sh: unknown config label '$c' (want default|1MB|4MB|16MB|64MB)" >&2; exit 1; }
done

WORKERS=$(( N / 50 ))

if [[ "$DRY_RUN" == "true" ]]; then
    echo "# run-meta-sweep.sh dry-run  seed=${SEED}  N=${N}  k=${K}  configs=${CONFIGS_ARG}  reps=${REPS}"
    echo "# fixed: ${MODE} consumer, ${STORAGE} data+KV, RF=${REPLICAS}, MEMORY consumer state, R=${CONSUMER_REPLICAS}"
    echo "# window: warmup=${WARMUP_SECS}s capture=${CAPTURE_SECS}s  jsz poll=${JSZ_POLL_INTERVAL}s  sidecar=$(capture_duration "$N")s"
    for c in "${CONFIGS[@]}"; do
        for (( r=1; r<=REPS; r++ )); do
            echo "CELL meta-${c}-N${N} rep${r}  nats_config='$(config_file "$c")'  flags=--n ${N} --workers ${WORKERS} --per-worker-rate ${K} --consumer-memory-storage=true --consumer-replicas=${CONSUMER_REPLICAS} --kv-storage ${STORAGE} --data-storage ${STORAGE}"
        done
    done
    exit 0
fi

[[ -x "$HARNESS_BIN" ]] || { echo "run-meta-sweep.sh: harness binary not found: $HARNESS_BIN (build: go build -o cmd/harness/harness ./cmd/harness)" >&2; exit 1; }
[[ -x "${SCRIPT_DIR}/verify-isolation.sh" ]] || { echo "run-meta-sweep.sh: verify-isolation.sh missing" >&2; exit 1; }
for tool in docker taskset curl iostat jq; do command -v "$tool" >/dev/null 2>&1 || { echo "run-meta-sweep.sh: missing tool '$tool'" >&2; exit 1; }; done
[[ -d /sys/fs/cgroup/system.slice ]] || { echo "run-meta-sweep.sh: cgroup v2 unavailable" >&2; exit 1; }

CAPTURE_PIDS=(); CAPTURE_NAMES=(); CAPTURE_RCS=()
# shellcheck source=_capture_lib.sh
source "${SCRIPT_DIR}/_capture_lib.sh"

HARNESS_PID=""
trap 'echo "run-meta-sweep.sh: signal — killing harness + captures" >&2; [[ -n "$HARNESS_PID" ]] && { kill "$HARNESS_PID" 2>/dev/null || true; wait "$HARNESS_PID" 2>/dev/null || true; }; stop_captures; exit 130' INT TERM
trap 'stop_captures' EXIT

mkdir -p "$RESULTS_DIR"
log() { echo "run-meta-sweep.sh: $*"; }

# Local start_captures: same set as the Arm B runner, but jsz polls at 1 Hz.
start_captures() {
    local run_dir="$1" duration="$2"
    CAPTURE_PIDS=(); CAPTURE_NAMES=()
    bash "${SCRIPT_DIR}/capture-cgroup-io.sh"     --output "${run_dir}/cgroup_io.raw"     --duration "$duration" --containers "$CONTAINERS" & CAPTURE_PIDS+=($!); CAPTURE_NAMES+=("cgroup-io")
    bash "${SCRIPT_DIR}/capture-cgroup-cpumem.sh" --output "${run_dir}/cgroup_cpumem.raw" --duration "$duration" --containers "$CONTAINERS" & CAPTURE_PIDS+=($!); CAPTURE_NAMES+=("cgroup-cpumem")
    bash "${SCRIPT_DIR}/capture-iostat.sh"        --output "${run_dir}/iostat.raw"        --duration "$duration" & CAPTURE_PIDS+=($!); CAPTURE_NAMES+=("iostat")
    bash "${SCRIPT_DIR}/capture-jsz.sh"           --output "${run_dir}/jsz.raw"           --duration "$duration" --nats-nodes "$NATS_MONITOR" --poll-interval "$JSZ_POLL_INTERVAL" & CAPTURE_PIDS+=($!); CAPTURE_NAMES+=("jsz")
}

resume_verdict() {
    local d="$1"
    [[ ! -d "$d" ]] && { echo "fresh"; return 0; }
    [[ ! -f "${d}/manifest.yaml" ]] && { echo "rerun:no-manifest"; return 0; }
    if ! grep -q '^status: ok$' "${d}/manifest.yaml"; then
        echo "rerun:status=$(grep -m1 '^status:' "${d}/manifest.yaml" | sed 's/^status:[[:space:]]*//' || echo unknown)"; return 0
    fi
    [[ ! -f "${d}/latency.json" ]] && { echo "rerun:no-latency-json"; return 0; }
    echo "skip-valid"
}

# run_cell config_label rep
run_cell() {
    local clabel="$1" rep="$2"
    local nats_config; nats_config="$(config_file "$clabel")"
    local label="meta-${clabel}-N${N}"
    local run_dir="${RESULTS_DIR}/${label}/rep${rep}"

    echo ""
    echo "=== ${label} rep${rep}  (N=${N} k=${K} R=${CONSUMER_REPLICAS} mem-consumer file/RF5  meta_compact=${clabel}) ==="

    local verdict; verdict="$(resume_verdict "$run_dir")"
    case "$verdict" in
        skip-valid) log "SKIP ${label} rep${rep}: valid complete cell"; return 0 ;;
        rerun:*) log "RERUN ${label} rep${rep}: invalid (${verdict#rerun:}) — deleting"; [[ -n "$run_dir" && "$run_dir" != "/" ]] && rm -rf "$run_dir" ;;
        fresh) ;;
    esac
    mkdir -p "$run_dir"

    {
        echo "seed: ${SEED}"; echo "label: ${label}"; echo "n: ${N}"; echo "k: ${K}"
        echo "workers: ${WORKERS}"; echo "storage: ${STORAGE}"; echo "mode: ${MODE}"
        echo "consumer_memory_storage: true"; echo "consumer_replicas: ${CONSUMER_REPLICAS}"
        echo "meta_compact: ${clabel}"; echo "nats_config: '${nats_config:-default}'"
        echo "rep: ${rep}"; echo "replicas: ${REPLICAS}"
        echo "warmup_secs: ${WARMUP_SECS}"; echo "capture_secs: ${CAPTURE_SECS}"
        echo "jsz_poll_interval: ${JSZ_POLL_INTERVAL}"
    } > "${run_dir}/run-meta.yaml"

    log "resetting rig (replicas=${REPLICAS}, meta_compact=${clabel}, nats_config=${nats_config:-default})..."
    if ! PERF_RIG_NATS_REPLICAS="${REPLICAS}" PERF_RIG_NATS_CONFIG="${nats_config}" make -C "$RIG_DIR" reset; then
        echo "make reset failed" > "${run_dir}/failed.txt"; log "FAILED ${label} rep${rep}: make reset"; return 1
    fi

    local cap_dur; cap_dur="$(capture_duration "$N")"
    start_captures "$run_dir" "$cap_dur"
    log "captures started (sidecar ${cap_dur}s, jsz poll ${JSZ_POLL_INTERVAL}s, PIDs: ${CAPTURE_PIDS[*]:-none})"

    log "launching harness (background, pinned ${HARNESS_CPUSET})..."
    PERF_RIG_NATS_IMAGE="${PERF_RIG_NATS_IMAGE:-nats:2.12.6}" \
    PERF_RIG_NATS_IMAGE_DIGEST="${PERF_RIG_NATS_IMAGE_DIGEST:-}" \
        taskset -c "$HARNESS_CPUSET" "$HARNESS_BIN" \
            --load --replicas "$REPLICAS" --n "$N" --workers "$WORKERS" \
            --per-worker-rate "$K" --consumer-mode "$MODE" \
            --kv-storage "$STORAGE" --data-storage "$STORAGE" \
            --consumer-memory-storage=true --consumer-replicas="$CONSUMER_REPLICAS" \
            --warmup "${WARMUP_SECS}s" --capture-window "${CAPTURE_SECS}s" \
            --output-dir "$run_dir" &
    HARNESS_PID=$!
    log "harness PID=${HARNESS_PID}; sleeping 2s before isolation verify..."
    sleep 2

    if ! "${SCRIPT_DIR}/verify-isolation.sh" "$HARNESS_PID" "$CONTAINERS"; then
        log "ABORT ${label} rep${rep}: isolation mismatch — killing harness"
        kill "$HARNESS_PID" 2>/dev/null || true; wait "$HARNESS_PID" 2>/dev/null || true
        HARNESS_PID=""; stop_captures
        echo "isolation mismatch (cpuset)" > "${run_dir}/failed.txt"; return 1
    fi

    log "isolation OK; waiting for harness (startup + warmup ${WARMUP_SECS}s + capture ${CAPTURE_SECS}s)..."
    local harness_rc=0
    wait "$HARNESS_PID" || harness_rc=$?
    HARNESS_PID=""

    log "stopping captures..."
    stop_captures

    if [[ "$harness_rc" -ne 0 ]]; then
        printf 'harness exit code %d\n' "$harness_rc" > "${run_dir}/failed.txt"
        log "FAILED ${label} rep${rep}: harness exit ${harness_rc}; continuing."; return 1
    fi

    [[ ! -s "${run_dir}/node_exporter.prom" ]] && printf '# node_exporter not captured in load mode\n' > "${run_dir}/node_exporter.prom"
    if ! verify_captures "$run_dir"; then
        log "FAILED ${label} rep${rep}: capture verify; continuing."; return 1
    fi

    local post; post="$(resume_verdict "$run_dir")"
    if [[ "$post" != "skip-valid" ]]; then
        log "FAILED ${label} rep${rep}: harness produced invalid cell (${post#rerun:}); continuing."; return 1
    fi

    log "OK ${label} rep${rep}."
    return 0
}

log "META-SWEEP: N=${N} k=${K} (memory+R${CONSUMER_REPLICAS}, ${STORAGE}, RF=${REPLICAS}) × configs {${CONFIGS_ARG}} × ${REPS} reps"
log "results dir: ${RESULTS_DIR}"

success=0; fail=0
for c in "${CONFIGS[@]}"; do
    for (( r=1; r<=REPS; r++ )); do
        if run_cell "$c" "$r"; then success=$(( success + 1 )); else fail=$(( fail + 1 )); fi
    done
done

echo ""
echo "=== Meta-sweep complete (N=${N}) ==="
echo "  Configs: ${CONFIGS_ARG} × ${REPS} reps"
echo "  Success: ${success}"
echo "  Failed:  ${fail}"
[[ "$fail" -gt 0 ]] && exit 1
exit 0
