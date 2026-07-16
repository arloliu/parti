#!/usr/bin/env bash
# run-e4.sh — Drives one E4 (rebalance burst anatomy) cell: readiness-gated
# capture, a churn schedule (kill->converge->re-add x N, see cmd/harness's
# --churn-* flags / churn.go), and — for cells with --profiles — the E5
# client-side profile set (idle window + one wave, 30s CPU + heap + mutex).
#
# One-off measurement-session script (S3 brief), not part of the
# reusable run-matrix.sh cell loop: E4/E5's profile-capture choreography
# (poll churn-waves.csv for a wave boundary, fire pprof captures at the
# right instant) doesn't fit run_one's generic per-cell shape, so this
# script duplicates run-matrix.sh's readiness-gate + capture-start
# pattern for a single cell rather than extending run_one's control flow
# for every other cell too. See RUNBOOK.md / docs/research/
# load-overhead-research-and-pprof-plan-v1.md §5 E4/E5 for the
# experiment definitions.
#
# Usage:
#   run-e4.sh --cell E4.b --n 2000 --workers 50 --churn-worker-idx 49 \
#             --warmup-secs 60 --capture-secs 1200 --churn-plateau 90s \
#             --churn-converge-timeout 150s --output-dir results/s3-e4/E4.b \
#             [--profiles]
#
# All rig runs are synchronous, blocking, foreground calls — this script
# does not background itself and does not return until the harness
# process (and the capture/aggregate tail) has completed.
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
# 16060, not the harness default 6060 — this host already has some
# unrelated process bound to 6060 system-wide (confirmed via `ss -tln`
# before this campaign); avoids a bind-in-use abort.
PPROF_ADDR="127.0.0.1:16060"
READY_TIMEOUT_SECS=1800

CELL="" N="" WORKERS="" CHURN_IDX="" WARMUP_SECS=60 CAPTURE_SECS=600
CHURN_WAVES=3 CHURN_PLATEAU="90s" CHURN_CONVERGE_TIMEOUT="150s"
OUTPUT_DIR="" DO_PROFILES=false

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
        --profiles) DO_PROFILES=true; shift ;;
        *) echo "run-e4.sh: unknown argument: $1" >&2; exit 2 ;;
    esac
done

for req in CELL N WORKERS CHURN_IDX OUTPUT_DIR; do
    if [[ -z "${!req}" ]]; then
        echo "run-e4.sh: --${req,,} is required" >&2
        exit 1
    fi
done

RUN_DIR="${RIG_DIR}/${OUTPUT_DIR}"
mkdir -p "$RUN_DIR"

echo "=== E4 cell ${CELL}  N=${N} workers=${WORKERS} churn_idx=${CHURN_IDX} profiles=${DO_PROFILES} ==="
echo "  run dir: ${RUN_DIR}"

# shellcheck source=_capture_lib.sh
source "${SCRIPT_DIR}/_capture_lib.sh"
CAPTURE_PIDS=() CAPTURE_NAMES=() CAPTURE_RCS=()
trap 'echo "run-e4.sh: signal received — stopping captures and exiting" >&2; stop_captures; exit 130' INT TERM

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
    --pprof-addr="$PPROF_ADDR"
    --churn-worker-idx="$CHURN_IDX" --churn-waves="$CHURN_WAVES"
    --churn-plateau="$CHURN_PLATEAU" --churn-converge-timeout="$CHURN_CONVERGE_TIMEOUT"
)
if [[ "$DO_PROFILES" == "true" ]]; then
    # Non-zero only for the cells that actually capture profiles — a
    # normal (non-profiled) harness run pays zero block/mutex overhead
    # (see cmd/harness/main.go flag docs).
    HARNESS_ARGS+=(--block-profile-rate=10000 --mutex-profile-fraction=1)
fi

PERF_RIG_RUN_INDEX="s3-e4-${CELL}" \
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

# ---------------------------------------------------------------------------
# E5 profile capture (only when --profiles was passed): idle window profile
# BEFORE wave 1, wave profile DURING wave 2. Runs as a background subshell so
# it never blocks the harness wait below; PROFILE_PID is joined explicitly
# after the harness exits so this script doesn't return early with a profile
# capture still in flight.
# ---------------------------------------------------------------------------
PROFILE_PID=""
if [[ "$DO_PROFILES" == "true" ]]; then
    (
        PLOG="${RUN_DIR}/profile-log.txt"
        capture_set() {
            local tag="$1"
            local t0 t1 leader_before leader_after
            t0="$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)"
            curl -s --max-time 3 "http://${PPROF_ADDR}/leader" -o "${RUN_DIR}/${tag}-leader-before.json" || true
            leader_before="$(cat "${RUN_DIR}/${tag}-leader-before.json" 2>/dev/null || echo '{}')"
            # heap/mutex are point-in-time snapshots (mutex is CUMULATIVE
            # since process start / last fraction change, not windowed —
            # see the s3-verdict methodology note); fire them right as the
            # 30s CPU capture starts so all three roughly share one instant.
            curl -s --max-time 10 "http://${PPROF_ADDR}/debug/pprof/heap" -o "${RUN_DIR}/${tag}-heap.pprof" &
            local heap_pid=$!
            curl -s --max-time 10 "http://${PPROF_ADDR}/debug/pprof/mutex" -o "${RUN_DIR}/${tag}-mutex.pprof" &
            local mutex_pid=$!
            curl -s --max-time 40 "http://${PPROF_ADDR}/debug/pprof/profile?seconds=30" -o "${RUN_DIR}/${tag}-cpu.pprof"
            wait "$heap_pid" "$mutex_pid" 2>/dev/null || true
            t1="$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)"
            curl -s --max-time 3 "http://${PPROF_ADDR}/leader" -o "${RUN_DIR}/${tag}-leader-after.json" || true
            leader_after="$(cat "${RUN_DIR}/${tag}-leader-after.json" 2>/dev/null || echo '{}')"
            printf '%s: window=[%s,%s] leader_before=%s leader_after=%s\n' "$tag" "$t0" "$t1" "$leader_before" "$leader_after" >> "$PLOG"
        }

        # Wait for the harness's capture window (and churn schedule) to
        # actually start: churn-waves.csv is created (header written)
        # the instant runChurnSchedule launches, i.e. right at
        # captureStart (after warmup) — a more reliable sync signal than
        # guessing warmup+epsilon from this script.
        cw_csv="${RUN_DIR}/churn-waves.csv"
        for _ in $(seq 1 600); do
            [[ -s "$cw_csv" ]] && break
            sleep 1
        done
        if [[ ! -s "$cw_csv" ]]; then
            echo "profile-capture: churn-waves.csv never appeared — aborting profile capture" >> "${RUN_DIR}/profile-log.txt"
            exit 0
        fi

        # --- idle profile: fire immediately, lands inside the plateau ---
        capture_set "idle"

        # --- wave profile: poll for wave 2's kill row, then fire ---
        for _ in $(seq 1 600); do
            if grep -q '^2,kill,' "$cw_csv" 2>/dev/null; then
                break
            fi
            sleep 1
        done
        if grep -q '^2,kill,' "$cw_csv" 2>/dev/null; then
            capture_set "wave2"
        else
            echo "profile-capture: wave 2 kill event never observed within poll budget — no wave profile captured" >> "$PLOG"
        fi
    ) &
    PROFILE_PID=$!
fi

echo "  waiting for harness (pid ${HARNESS_PID}) to complete — this is a synchronous, blocking, foreground wait..."
HARNESS_RC=0
wait "$HARNESS_PID" || HARNESS_RC=$?
echo "  harness exited rc=${HARNESS_RC}"

if [[ -n "$PROFILE_PID" ]]; then
    echo "  joining profile-capture subshell (pid ${PROFILE_PID})..."
    wait "$PROFILE_PID" || true
fi

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
