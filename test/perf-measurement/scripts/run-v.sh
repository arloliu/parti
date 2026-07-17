#!/usr/bin/env bash
# run-v.sh — Session V (post-hardening validation) single-cell driver.
#
# Mirrors run-e4.sh's readiness-gated capture assembly (reset -> harness
# background start -> wait_for_ready -> cgroup-io/iostat/jsz(+detail)/
# node-exporter captures -> synchronous harness wait -> verify -> aggregate)
# WITHOUT the E5 pprof choreography (no --pprof-addr, no profile subshell —
# profiles are not needed this session). Adds --handoff-log passthrough
# (the discontinuity-event capture flag) and makes the churn schedule
# optional so the same driver runs both the V1 churn cell and the V2
# idle-long cell.
#
# One-off measurement-session script (Session V brief), not part of the
# reusable run-matrix.sh cell loop.
#
# All rig runs are synchronous, blocking, foreground calls — this script
# does not background itself and does not return until the harness process
# (and the capture/aggregate tail) has completed.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RIG_DIR="$(dirname "$SCRIPT_DIR")"

HARNESS_BIN="${RIG_DIR}/cmd/harness/harness"
if [[ -x "${RIG_DIR}/cmd/aggregate/aggregate" ]]; then
    AGGREGATE_BIN="${RIG_DIR}/cmd/aggregate/aggregate"
else
    AGGREGATE_BIN="aggregate"
fi

CONTAINERS_R3="perf-nats-1,perf-nats-2,perf-nats-3"
NATS_MONITOR_R3="localhost:8222,localhost:8223,localhost:8224"

READY_ADDR="127.0.0.1:6061"
READY_TIMEOUT_SECS=1800

SEED=42   # campaign-canonical seed, recorded in run-meta.yaml (no shuffle here)

CELL="" N="" WORKERS="" WARMUP_SECS=300 CAPTURE_SECS=600
CHURN_IDX="-1" CHURN_WAVES=3 CHURN_PLATEAU="90s" CHURN_CONVERGE_TIMEOUT="180s"
OUTPUT_DIR=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --cell) CELL="$2"; shift 2 ;;
        --n) N="$2"; shift 2 ;;
        --workers) WORKERS="$2"; shift 2 ;;
        --churn-worker-idx) CHURN_IDX="$2"; shift 2 ;;
        --churn-waves) CHURN_WAVES="$2"; shift 2 ;;
        --churn-plateau) CHURN_PLATEAU="$2"; shift 2 ;;
        --churn-converge-timeout) CHURN_CONVERGE_TIMEOUT="$2"; shift 2 ;;
        --warmup-secs) WARMUP_SECS="$2"; shift 2 ;;
        --capture-secs) CAPTURE_SECS="$2"; shift 2 ;;
        --output-dir) OUTPUT_DIR="$2"; shift 2 ;;
        *) echo "run-v.sh: unknown argument: $1" >&2; exit 2 ;;
    esac
done

for req in CELL N WORKERS OUTPUT_DIR; do
    if [[ -z "${!req}" ]]; then
        echo "run-v.sh: --${req,,} is required" >&2
        exit 1
    fi
done

RUN_DIR="${RIG_DIR}/${OUTPUT_DIR}"
mkdir -p "$RUN_DIR"

echo "=== V cell ${CELL}  N=${N} workers=${WORKERS} churn_idx=${CHURN_IDX} warmup=${WARMUP_SECS}s capture=${CAPTURE_SECS}s ==="
echo "  run dir: ${RUN_DIR}"

# Pre-run sidecar (seed + cell metadata recorded before anything starts,
# matching run-matrix.sh's run-meta.yaml traceability convention).
{
    echo "seed: ${SEED}"
    echo "cell: ${CELL}"
    echo "n: ${N}"
    echo "workers: ${WORKERS}"
    echo "replicas: 3"
    echo "warmup_secs: ${WARMUP_SECS}"
    echo "capture_secs: ${CAPTURE_SECS}"
    echo "churn_worker_idx: ${CHURN_IDX}"
    echo "handoff_log: handoff-discontinuity.log"
    echo "session: v-post-hardening"
} > "${RUN_DIR}/run-meta.yaml"

# shellcheck source=_capture_lib.sh
source "${SCRIPT_DIR}/_capture_lib.sh"
CAPTURE_PIDS=() CAPTURE_NAMES=() CAPTURE_RCS=()
trap 'echo "run-v.sh: signal received — stopping captures and exiting" >&2; stop_captures; exit 130' INT TERM

echo "  resetting rig (replicas=3)..."
PERF_RIG_NATS_REPLICAS=3 make -C "$RIG_DIR" reset

HARNESS_LOG="${RUN_DIR}/harness.log"
echo "  starting harness; waiting for readiness (timeout ${READY_TIMEOUT_SECS}s)..."

HARNESS_ARGS=(
    --n="$N" --workers="$WORKERS" --replicas=3
    --two-phase=true --consumer-mode=dynamic --consumer-memory-storage
    --warmup="${WARMUP_SECS}s" --capture-window="${CAPTURE_SECS}s"
    --rpc-dump-interval=1s
    --output-dir="$RUN_DIR"
    --ready-addr="$READY_ADDR"
    --handoff-log="${RUN_DIR}/handoff-discontinuity.log"
)
if [[ "$CHURN_IDX" -ge 0 ]]; then
    HARNESS_ARGS+=(
        --churn-worker-idx="$CHURN_IDX" --churn-waves="$CHURN_WAVES"
        --churn-plateau="$CHURN_PLATEAU" --churn-converge-timeout="$CHURN_CONVERGE_TIMEOUT"
    )
fi

PERF_RIG_RUN_INDEX="v-post-hardening-${CELL}" \
PERF_RIG_NATS_IMAGE="${PERF_RIG_NATS_IMAGE:-nats:2.12.6}" \
    "$HARNESS_BIN" "${HARNESS_ARGS[@]}" > "$HARNESS_LOG" 2>&1 &
HARNESS_PID=$!

if ! wait_for_ready "$READY_ADDR" "$READY_TIMEOUT_SECS" "$HARNESS_PID"; then
    echo "  readiness gate FAILED — see ${HARNESS_LOG}" >&2
    kill "$HARNESS_PID" 2>/dev/null || true
    wait "$HARNESS_PID" 2>/dev/null || true
    echo "readiness gate failed" > "${RUN_DIR}/failed.txt"
    exit 1
fi
echo "  cluster ready — starting captures..."

CAPTURE_TOTAL=$(( WARMUP_SECS + CAPTURE_SECS + 60 )) # +60s slack over the harness's own window
bash "${SCRIPT_DIR}/capture-cgroup-io.sh" --output "${RUN_DIR}/cgroup_io.raw" --duration "$CAPTURE_TOTAL" --containers "$CONTAINERS_R3" &
CAPTURE_PIDS+=($!); CAPTURE_NAMES+=("cgroup-io")
bash "${SCRIPT_DIR}/capture-iostat.sh" --output "${RUN_DIR}/iostat.raw" --duration "$CAPTURE_TOTAL" &
CAPTURE_PIDS+=($!); CAPTURE_NAMES+=("iostat")
bash "${SCRIPT_DIR}/capture-jsz.sh" --output "${RUN_DIR}/jsz.raw" --duration "$CAPTURE_TOTAL" --nats-nodes "$NATS_MONITOR_R3" --jsz-detail &
CAPTURE_PIDS+=($!); CAPTURE_NAMES+=("jsz")
if curl -sf --max-time 2 http://localhost:9100/metrics >/dev/null 2>&1; then
    bash "${SCRIPT_DIR}/capture-node-exporter.sh" --output "${RUN_DIR}/node_exporter.prom" --duration "$CAPTURE_TOTAL" &
    CAPTURE_PIDS+=($!); CAPTURE_NAMES+=("node-exporter")
else
    printf '# node_exporter unavailable at run start; stub for aggregator presence check\n' > "${RUN_DIR}/node_exporter.prom"
fi
echo "  captures started (PIDs: ${CAPTURE_PIDS[*]})"

echo "  waiting for harness (pid ${HARNESS_PID}) to complete — this is a synchronous, blocking, foreground wait..."
HARNESS_RC=0
wait "$HARNESS_PID" || HARNESS_RC=$?
echo "  harness exited rc=${HARNESS_RC}"

echo "  stopping captures..."
stop_captures

if ! verify_captures "$RUN_DIR"; then
    echo "  run FAILED (capture verify) — see ${RUN_DIR}/capture-failed.txt" >&2
    exit 1
fi

if [[ "$HARNESS_RC" -ne 0 ]]; then
    printf 'harness exit code %d\n' "$HARNESS_RC" > "${RUN_DIR}/failed.txt"
    echo "  run FAILED (harness_rc=${HARNESS_RC})" >&2
    exit 1
fi

echo "  aggregating..."
if ! "$AGGREGATE_BIN" --run-dir "$RUN_DIR"; then
    echo "  aggregate FAILED" >&2
    echo "aggregate exited non-zero" > "${RUN_DIR}/aggregate-failed.txt"
    exit 1
fi

echo "  run OK: ${RUN_DIR}"
