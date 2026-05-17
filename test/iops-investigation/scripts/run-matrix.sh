#!/usr/bin/env bash
# run-matrix.sh — Orchestrates the M1.0–M1.11 IOPS investigation matrix.
#
# See docs/plans/iops-investigation/00-attribution-plan.md §M1 for the full
# cell-to-flag matrix and §R5 for the pre-registered randomised schedule.
#
# Usage:
#   run-matrix.sh [--seed N] [--cells M1.0,M1.1,...] [--n-values 1000,3000] \
#                 [--reps 5] [--warmup-secs 300] [--capture-secs 600] \
#                 [--results-dir results/] [--dry-run]
#
# REQUIRED: --seed N  (recorded in every per-run run-meta.yaml sidecar)
#
# Examples:
#   run-matrix.sh --seed 42 --dry-run
#   run-matrix.sh --seed 42 --results-dir results/run1/
#   run-matrix.sh --seed 42 --cells M1.0,M1.1,M1.2
#   run-matrix.sh --seed 42 --cells M1.8 --reps 5
#
# Structural failures (missing harness binary, wrong directory) abort loudly.
# Per-run harness failures are logged and continue the campaign.
set -euo pipefail

# ---------------------------------------------------------------------------
# Paths — all derived from this script's location; works from any cwd.
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RIG_DIR="$(dirname "$SCRIPT_DIR")"

HARNESS_BIN="${RIG_DIR}/cmd/harness/harness"
# Prefer the locally-built binary at ${RIG_DIR}/cmd/aggregate/aggregate;
# fall back to whatever 'aggregate' is on PATH if the local build is
# absent (e.g. operator installed it system-wide).
if [[ -x "${RIG_DIR}/cmd/aggregate/aggregate" ]]; then
    AGGREGATE_BIN="${RIG_DIR}/cmd/aggregate/aggregate"
else
    AGGREGATE_BIN="aggregate"
fi

# Container names for capture-cgroup-io.sh, indexed by replicas.
CONTAINERS_R3="iops-nats-1,iops-nats-2,iops-nats-3"
CONTAINERS_R5="iops-nats-1,iops-nats-2,iops-nats-3,iops-nats-4,iops-nats-5"

# Monitoring endpoint for capture-jsz.sh.
NATS_MONITOR_R3="localhost:8222"
NATS_MONITOR_R5="localhost:8222"   # single ingress; route discovery handles the rest

# Warmup and capture window durations (seconds).
WARMUP_SECS=300     # 5 min
CAPTURE_SECS=600    # 10 min
CAPTURE_TOTAL=$(( WARMUP_SECS + CAPTURE_SECS ))

# ---------------------------------------------------------------------------
# Cell definitions.
#
# Each cell entry encodes: replicas N-values and the harness flag string.
# M1.0 (control) is flagged specially — it skips the harness.
# M1.11 (HEAD) always fails fast with a pin-swap instruction.
#
# Format of CELL_REPLICAS[cell]: integer (3 or 5)
# Format of CELL_N_VALUES[cell]: space-separated list of N values
# Format of CELL_FLAGS[cell]:    harness flags (empty for control/HEAD)
# ---------------------------------------------------------------------------
declare -A CELL_REPLICAS CELL_N_VALUES CELL_FLAGS CELL_CONTROL CELL_HEAD CELL_NATS_CONFIG

# Helper to register one cell.
#
# Positional args:
#   1 cell        Cell name (e.g. "M1.2")
#   2 replicas    NATS cluster size — 3 or 5 (selects compose profile)
#   3 n_values    Space-separated N list (partition counts)
#   4 flags       Harness flag string ("" for controls)
#   5 is_control  Optional, "true" to skip the harness binary (M1.0)
#   6 is_head     Optional, "true" to refuse the cell (M1.11 pin-swap)
#   7 nats_config Optional, host-side path to a per-cell server.conf
#                 (default: docker-compose's built-in default of
#                 ./nats-server.conf). Used for Plan 02 §M2.C tuning.
_def_cell() {
    local cell="$1" replicas="$2" n_values="$3" flags="$4"
    local is_control="${5:-false}"
    local is_head="${6:-false}"
    local nats_config="${7:-}"
    CELL_REPLICAS["$cell"]="$replicas"
    CELL_N_VALUES["$cell"]="$n_values"
    CELL_FLAGS["$cell"]="$flags"
    CELL_CONTROL["$cell"]="$is_control"
    CELL_HEAD["$cell"]="$is_head"
    CELL_NATS_CONFIG["$cell"]="$nats_config"
}

# M1.0  No-Parti control — NATS only, no harness. N swept to set MDE baseline.
_def_cell M1.0  3 "500 1000 2000 3000" "" "true"

# M1.1  B-lib baseline — EnableTwoPhaseHandoff=false, Dynamic consumer.
_def_cell M1.1  3 "500 1000 2000 3000" "--two-phase=false --consumer-mode=dynamic"

# M1.2  B-prod baseline — EnableTwoPhaseHandoff=true.
_def_cell M1.2  3 "500 1000 2000 3000" "--two-phase=true --consumer-mode=dynamic"

# M1.3  H1.A — B-prod, SweepInterval=5m.
_def_cell M1.3  3 "500 1000 2000 3000" "--two-phase=true --consumer-mode=dynamic --sweep-interval=5m0s"

# M1.4  H1.B — two-phase explicitly off (expected to coincide with M1.1).
_def_cell M1.4  3 "500 1000 2000 3000" "--two-phase=false --consumer-mode=dynamic"

# M1.5  H2.A — B-prod, FetchTimeout=30s.
_def_cell M1.5  3 "500 1000 2000 3000" "--two-phase=true --consumer-mode=dynamic --fetch-timeout=30s"

# M1.6  H2.B — B-prod, Queue consumer (N per-partition → 1 shared).
_def_cell M1.6  3 "500 1000 2000 3000" "--two-phase=true --consumer-mode=queue"

# M1.7  H2.C — B-prod, data stream storage=memory.
_def_cell M1.7  3 "500 1000 2000 3000" "--two-phase=true --consumer-mode=dynamic --data-storage=memory"

# M1.8  H3 — B-prod, HeartbeatInterval=10s. N=2000 only.
_def_cell M1.8  3 "2000" "--two-phase=true --consumer-mode=dynamic --heartbeat-interval=10s"

# M1.9  M5 — B-prod, all KV buckets memory. N∈{1000,2000,3000}.
_def_cell M1.9  3 "1000 2000 3000" "--two-phase=true --consumer-mode=dynamic --kv-storage=memory"

# M1.10 R=5 comparison — B-prod, replicas=5. N=2000 only.
_def_cell M1.10 5 "2000" "--two-phase=true --consumer-mode=dynamic --replicas=5"

# M1.11 HEAD comparison — requires a manual go.mod pin swap; always refused.
_def_cell M1.11 3 "2000" "" "false" "true"

# ---------------------------------------------------------------------------
# Plan 02 — NATS server-side tuning cells (M2.x).
#
# Source: docs/plans/iops-investigation/02-nats-tuning-plan.md +
#         tmp/r1-nats-tuning-research.md §3 (cell list, predictions).
# Baseline for comparison: M1.2 (file-storage, default config).
# N values restricted to {1000, 3000} per Plan 02 §6 budget.
# ---------------------------------------------------------------------------

# M2.A  Consumer state in memory (parti messages remain durable on disk).
#       Plan 02 R1 rank-1 candidate; durability-preserving analog of M1.7.
_def_cell M2.A 3 "1000 3000" "--two-phase=true --consumer-mode=dynamic --consumer-memory-storage=true"

# M2.B  Consumer state in memory AND Replicas=1 (no consumer-state raft).
#       Plan 02 R1 rank-1+rank-2 stacked. M2.A vs M2.B attributes the
#       raft-replication contribution to consumer-state IOPS.
_def_cell M2.B 3 "1000 3000" "--two-phase=true --consumer-mode=dynamic --consumer-memory-storage=true --consumer-replicas=1"

# M2.C  Server-side: jetstream.sync_interval raised from 2m default to 10m.
#       Per-cell server.conf swap via IOPS_RIG_NATS_CONFIG (7th _def_cell arg).
#       Plan 02 R1 rank-3 candidate. Modest win expected; mainly targets
#       the t≈200s spike in findings.md §9 (open question).
_def_cell M2.C 3 "1000 3000" "--two-phase=true --consumer-mode=dynamic" "false" "false" "./nats-server-M2C.conf"

# Ordered default cell list.
ALL_CELLS=(M1.0 M1.1 M1.2 M1.3 M1.4 M1.5 M1.6 M1.7 M1.8 M1.9 M1.10 M1.11 M2.A M2.B M2.C)

# ---------------------------------------------------------------------------
# Usage.
# ---------------------------------------------------------------------------
usage() {
    cat >&2 <<'EOF'
Usage: run-matrix.sh --seed N [options]

Required:
  --seed N            Integer seed for the pre-registered random schedule.
                      Recorded in every per-run manifest and in schedule.tsv.

Options:
  --cells CELLS       Comma-separated cell list (default: all M1.0–M1.11).
                      Example: --cells M1.1,M1.2,M1.8
  --reps N            Replicates per (cell, N) combination (default: 5).
  --n-values LIST     Comma-separated N override filter (default: each cell's
                      full N list from §M1). Only cell-N pairs whose N appears
                      in this list are scheduled. Example: --n-values 1000,3000
                      keeps only the 1000 and 3000 N runs per cell, dropping
                      500 and 2000. Useful for tiered campaigns (see RUNBOOK §3).
  --warmup-secs N     Per-run warmup window in seconds (default: 300 = 5 min).
                      Tier 0 sanity / quick-scan campaigns use 30-60 s; full
                      §M1 matrix uses 300 s (the default).
  --capture-secs N    Per-run capture window in seconds (default: 600 = 10 min).
                      Tier 0 typically uses 60-120 s.
  --results-dir PATH  Parent directory for per-run subdirs (default: results/).
  --dry-run           Print the full randomised schedule and exit without
                      running the rig.
  -h, --help          Show this message and exit.

Environment:
  IOPS_RIG_NATS_IMAGE         Docker image for the NATS cluster (default: nats:2.12.6).
  IOPS_RIG_NATS_IMAGE_DIGEST  Pre-resolved image digest for manifest (optional).

M1.11 note:
  M1.11 (HEAD comparison) is refused by this script. Before running it,
  swap go.mod to the HEAD pin and re-run with --cells M1.11. See README
  "M1.11 manual pin-swap workflow" for instructions.
EOF
    exit 2
}

# ---------------------------------------------------------------------------
# Argument parsing.
# ---------------------------------------------------------------------------
SEED=""
CELLS_ARG=""
N_VALUES_ARG=""
REPS=5
RESULTS_DIR="${RIG_DIR}/results"
DRY_RUN=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --seed)        SEED="$2";        shift 2 ;;
        --cells)       CELLS_ARG="$2";   shift 2 ;;
        --n-values)    N_VALUES_ARG="$2"; shift 2 ;;
        --warmup-secs) WARMUP_SECS="$2"; shift 2 ;;
        --capture-secs) CAPTURE_SECS="$2"; shift 2 ;;
        --reps)        REPS="$2";        shift 2 ;;
        --results-dir) RESULTS_DIR="$2"; shift 2 ;;
        --dry-run)     DRY_RUN=true;     shift ;;
        -h|--help)     usage ;;
        *) echo "run-matrix.sh: unknown argument: $1" >&2; usage ;;
    esac
done

[[ -z "$SEED" ]] && { echo "run-matrix.sh: --seed is required" >&2; usage; }
if ! [[ "$SEED" =~ ^-?[0-9]+$ ]]; then
    echo "run-matrix.sh: --seed must be an integer, got: $SEED" >&2
    exit 1
fi
if ! [[ "$REPS" =~ ^[0-9]+$ ]] || [[ "$REPS" -lt 1 ]]; then
    echo "run-matrix.sh: --reps must be a positive integer, got: $REPS" >&2
    exit 1
fi
if ! [[ "$WARMUP_SECS" =~ ^[0-9]+$ ]] || [[ "$WARMUP_SECS" -lt 5 ]]; then
    echo "run-matrix.sh: --warmup-secs must be an integer >= 5, got: $WARMUP_SECS" >&2
    exit 1
fi
if ! [[ "$CAPTURE_SECS" =~ ^[0-9]+$ ]] || [[ "$CAPTURE_SECS" -lt 10 ]]; then
    echo "run-matrix.sh: --capture-secs must be an integer >= 10, got: $CAPTURE_SECS" >&2
    exit 1
fi
# Recompute total now that user overrides have landed.
CAPTURE_TOTAL=$(( WARMUP_SECS + CAPTURE_SECS ))

# Build active cell list.
if [[ -n "$CELLS_ARG" ]]; then
    IFS=',' read -ra ACTIVE_CELLS <<< "$CELLS_ARG"
else
    ACTIVE_CELLS=("${ALL_CELLS[@]}")
fi

# Validate each cell.
for c in "${ACTIVE_CELLS[@]}"; do
    if [[ -z "${CELL_REPLICAS[$c]+set}" ]]; then
        echo "run-matrix.sh: unknown cell '$c'" >&2
        exit 1
    fi
done

# ---------------------------------------------------------------------------
# Structural pre-flight checks (skipped in --dry-run mode).
#
# Every cell (including M1.0 control) requires the four mandatory capture
# sources cgroup/iostat/jsz/node_exporter, because the aggregator reads
# all of them. We fail loud here BEFORE the schedule starts rather than
# silently skipping capture sources at run time (which used to produce
# late aggregate failures that were then masked as run-OK — see P0/P1
# review findings). node_exporter is treated as soft per §R3: if it's
# unreachable we still allow the campaign to proceed, but a one-line
# stub is written into each run dir so the aggregator's presence check
# passes without consuming a real prometheus blob.
# ---------------------------------------------------------------------------
NODE_EXPORTER_AVAILABLE=false
if [[ "$DRY_RUN" == "false" ]]; then
    # Require the harness binary only when at least one selected cell is a
    # non-control, non-HEAD cell. M1.0-only campaigns run on hosts without
    # the harness binary (they only need NATS + capture tooling).
    needs_harness=false
    for c in "${ACTIVE_CELLS[@]}"; do
        if [[ "${CELL_CONTROL[$c]}" != "true" && "${CELL_HEAD[$c]}" != "true" ]]; then
            needs_harness=true
            break
        fi
    done
    if [[ "$needs_harness" == "true" ]] && [[ ! -x "$HARNESS_BIN" ]]; then
        echo "run-matrix.sh: harness binary not found or not executable: $HARNESS_BIN" >&2
        echo "run-matrix.sh: build it with: go build -o cmd/harness/harness ./cmd/harness" >&2
        exit 1
    fi
    if ! command -v python3 >/dev/null 2>&1; then
        echo "run-matrix.sh: python3 is required for deterministic shuffle but not found." >&2
        exit 1
    fi
    if ! command -v docker >/dev/null 2>&1; then
        echo "run-matrix.sh: docker is required but not found." >&2
        exit 1
    fi

    # Capture-source prerequisites (mandatory for ALL cells including M1.0).
    if [[ ! -d /sys/fs/cgroup/system.slice ]]; then
        echo "run-matrix.sh: cgroup v2 not available at /sys/fs/cgroup/system.slice" >&2
        echo "run-matrix.sh: cgroup_io.raw is a mandatory capture source (aggregator §R3); aborting." >&2
        exit 1
    fi
    if ! command -v iostat >/dev/null 2>&1; then
        echo "run-matrix.sh: iostat not found on PATH (install sysstat)" >&2
        echo "run-matrix.sh: iostat.raw is a mandatory capture source (aggregator §R3); aborting." >&2
        exit 1
    fi
    if ! command -v curl >/dev/null 2>&1; then
        echo "run-matrix.sh: curl not found on PATH; required for jsz capture; aborting." >&2
        exit 1
    fi

    # node_exporter — soft prerequisite. If unavailable we'll stub
    # node_exporter.prom in each run dir so aggregate's presence check
    # passes. The campaign log warns loudly so this isn't silent.
    if curl -sf --max-time 2 http://localhost:9100/metrics >/dev/null 2>&1; then
        NODE_EXPORTER_AVAILABLE=true
    else
        echo "run-matrix.sh: WARNING: node_exporter not reachable at localhost:9100;" >&2
        echo "run-matrix.sh: WARNING: a stub node_exporter.prom will be written per run." >&2
        echo "run-matrix.sh: WARNING: node_exporter is sanity/debug-only per §R3 — campaign will continue." >&2
    fi
fi

# ---------------------------------------------------------------------------
# Build the full unshuffled run list.
#
# Each line: <cell> <N> <rep>
# M1.11 entries are included so they appear in the schedule (and fail fast
# at run time); M1.0 control entries are flagged in CELL_CONTROL.
# ---------------------------------------------------------------------------
declare -A N_FILTER=()
if [[ -n "$N_VALUES_ARG" ]]; then
    IFS=',' read -ra _n_filter_arr <<< "$N_VALUES_ARG"
    for n in "${_n_filter_arr[@]}"; do
        if ! [[ "$n" =~ ^[0-9]+$ ]]; then
            echo "run-matrix.sh: --n-values entries must be positive integers, got: $n" >&2
            exit 1
        fi
        N_FILTER["$n"]=1
    done
fi

UNSHUFFLED=()
for cell in "${ACTIVE_CELLS[@]}"; do
    n_values_str="${CELL_N_VALUES[$cell]}"
    read -ra n_values <<< "$n_values_str"
    for n in "${n_values[@]}"; do
        if [[ ${#N_FILTER[@]} -gt 0 && -z "${N_FILTER[$n]+set}" ]]; then
            continue
        fi
        for (( r=1; r<=REPS; r++ )); do
            UNSHUFFLED+=("${cell} ${n} ${r}")
        done
    done
done

if [[ ${#UNSHUFFLED[@]} -eq 0 ]]; then
    echo "run-matrix.sh: empty schedule — the --cells / --n-values combination produced no runs" >&2
    exit 1
fi

TOTAL_RUNS="${#UNSHUFFLED[@]}"

# ---------------------------------------------------------------------------
# Deterministic shuffle using Python's Mersenne Twister (stable across 3.x).
# ---------------------------------------------------------------------------
SHUFFLED=$(
    printf '%s\n' "${UNSHUFFLED[@]}" | \
    python3 -c '
import random, sys
seed = int(sys.argv[1])
lines = sys.stdin.read().splitlines()
random.seed(seed)
random.shuffle(lines)
print("\n".join(lines))
' "$SEED"
)

# ---------------------------------------------------------------------------
# Write / print schedule.
# ---------------------------------------------------------------------------
SCHEDULE_HEADER="# position\tcell\tN\trep\tnats_config\tflags"

build_schedule_body() {
    local pos=0
    while IFS=' ' read -r cell n rep; do
        pos=$(( pos + 1 ))
        local flags="${CELL_FLAGS[$cell]}"
        local is_control="${CELL_CONTROL[$cell]}"
        local is_head="${CELL_HEAD[$cell]}"
        local nats_config="${CELL_NATS_CONFIG[$cell]:-default}"
        if [[ "$is_control" == "true" ]]; then
            flags="(control — no harness)"
        elif [[ "$is_head" == "true" ]]; then
            flags="(HEAD — manual pin-swap required)"
        else
            # Add the --n and --output-dir placeholders annotated in schedule.
            flags="--n=${n} ${flags}"
        fi
        printf '%d\t%s\t%s\t%s\t%s\t%s\n' "$pos" "$cell" "$n" "$rep" "$nats_config" "$flags"
    done <<< "$SHUFFLED"
}

if [[ "$DRY_RUN" == "true" ]]; then
    echo "# run-matrix.sh dry-run  seed=${SEED}  reps=${REPS}  total_runs=${TOTAL_RUNS}"
    printf '%b\n' "$SCHEDULE_HEADER"
    build_schedule_body
    exit 0
fi

# ---------------------------------------------------------------------------
# Live run — write schedule.tsv first (pre-registered).
# ---------------------------------------------------------------------------
mkdir -p "$RESULTS_DIR"
SCHEDULE_FILE="${RESULTS_DIR}/schedule.tsv"

{
    echo "# run-matrix.sh  seed=${SEED}  reps=${REPS}  total_runs=${TOTAL_RUNS}"
    echo "# generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf '%b\n' "$SCHEDULE_HEADER"
    build_schedule_body
} > "$SCHEDULE_FILE"

echo "run-matrix.sh: schedule written to ${SCHEDULE_FILE}"
echo "run-matrix.sh: ${TOTAL_RUNS} runs scheduled"

# ---------------------------------------------------------------------------
# Campaign manifest — updated at start and end.
# ---------------------------------------------------------------------------
CAMPAIGN_MANIFEST="${RESULTS_DIR}/campaign-manifest.yaml"
CAMPAIGN_START="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

write_campaign_manifest() {
    local end_time="$1"
    local success_count="$2"
    local total="$3"
    shift 3
    local failures=("$@")

    {
        echo "seed: ${SEED}"
        echo "start_time: ${CAMPAIGN_START}"
        echo "end_time: ${end_time}"
        echo "reps: ${REPS}"
        printf 'cells:\n'
        for c in "${ACTIVE_CELLS[@]}"; do
            echo "  - ${c}"
        done
        echo "total_runs: ${total}"
        echo "success_count: ${success_count}"
        echo "failure_count: $(( total - success_count ))"
        if [[ ${#failures[@]} -eq 0 ]]; then
            echo "failures: []"
        else
            echo "failures:"
            for f in "${failures[@]}"; do
                echo "  - ${f}"
            done
        fi
        echo "schedule_file: ${SCHEDULE_FILE}"
        echo "nats_image: ${IOPS_RIG_NATS_IMAGE:-nats:2.12.6}"
    } > "$CAMPAIGN_MANIFEST"
}

write_campaign_manifest "$CAMPAIGN_START" 0 "$TOTAL_RUNS"

# ---------------------------------------------------------------------------
# Track background capture PIDs; clean them up on signal or exit.
# stop_captures / verify_captures live in _capture_lib.sh so the test
# harness can source them independently.
# ---------------------------------------------------------------------------
CAPTURE_PIDS=()
CAPTURE_NAMES=()
CAPTURE_RCS=()

# shellcheck source=_capture_lib.sh
source "${SCRIPT_DIR}/_capture_lib.sh"

trap 'echo "run-matrix.sh: signal received — stopping captures and exiting" >&2; stop_captures; exit 130' INT TERM
trap 'stop_captures' EXIT

# ---------------------------------------------------------------------------
# Per-run helper: start all capture scripts in background.
# Returns the PIDs via CAPTURE_PIDS global.
# ---------------------------------------------------------------------------
start_captures() {
    local run_dir="$1"
    local replicas="$2"
    local duration="$3"

    CAPTURE_PIDS=()
    CAPTURE_NAMES=()

    local containers
    if [[ "$replicas" -eq 5 ]]; then
        containers="$CONTAINERS_R5"
    else
        containers="$CONTAINERS_R3"
    fi

    # Mandatory capture prerequisites were validated by preflight; here
    # we just launch the captures and fail loudly if any one fails to
    # produce an output file at stop_captures() time.

    # cgroup-io — primary IOPS source.
    bash "${SCRIPT_DIR}/capture-cgroup-io.sh" \
        --output "${run_dir}/cgroup_io.raw" \
        --duration "$duration" \
        --containers "$containers" &
    CAPTURE_PIDS+=($!)
    CAPTURE_NAMES+=("cgroup-io")

    # iostat — secondary cross-check.
    bash "${SCRIPT_DIR}/capture-iostat.sh" \
        --output "${run_dir}/iostat.raw" \
        --duration "$duration" &
    CAPTURE_PIDS+=($!)
    CAPTURE_NAMES+=("iostat")

    # jsz/varz — NATS server stats.
    bash "${SCRIPT_DIR}/capture-jsz.sh" \
        --output "${run_dir}/jsz.raw" \
        --duration "$duration" \
        --nats-nodes "$NATS_MONITOR_R3" &
    CAPTURE_PIDS+=($!)
    CAPTURE_NAMES+=("jsz")

    # node-exporter — soft requirement (§R3). When reachable we launch
    # the capture; otherwise we write a one-line stub so the aggregator's
    # presence check still passes. The preflight already warned about
    # this case so it's not silent.
    if [[ "$NODE_EXPORTER_AVAILABLE" == "true" ]]; then
        bash "${SCRIPT_DIR}/capture-node-exporter.sh" \
            --output "${run_dir}/node_exporter.prom" \
            --duration "$duration" &
        CAPTURE_PIDS+=($!)
        CAPTURE_NAMES+=("node-exporter")
    else
        printf '# node_exporter unavailable at run start; stub for aggregator presence check\n' \
            > "${run_dir}/node_exporter.prom"
    fi
}

# ---------------------------------------------------------------------------
# run_one — execute a single scheduled entry.
#
# Arguments: position cell N rep
# ---------------------------------------------------------------------------
run_one() {
    local pos="$1" cell="$2" n="$3" rep="$4"
    local replicas="${CELL_REPLICAS[$cell]}"
    local is_control="${CELL_CONTROL[$cell]}"
    local is_head="${CELL_HEAD[$cell]}"
    local flags="${CELL_FLAGS[$cell]}"

    # M1.11 — refuse immediately.
    if [[ "$is_head" == "true" ]]; then
        echo ""
        echo "run-matrix.sh: ERROR: M1.11 requires a manual go.mod pin-swap to HEAD." >&2
        echo "run-matrix.sh: See README 'M1.11 manual pin-swap workflow' for instructions." >&2
        echo "run-matrix.sh: Skipping M1.11 — mark as failed." >&2
        return 1
    fi

    local run_label
    run_label="$(printf 'run-%03d-%s-N%s-rep%d' "$pos" "$cell" "$n" "$rep")"
    local run_dir="${RESULTS_DIR}/${run_label}"
    mkdir -p "$run_dir"

    echo ""
    echo "=== [${pos}/${TOTAL_RUNS}] ${cell}  N=${n}  rep=${rep}  dir=${run_label} ==="

    # Per-run sidecar (P1 fix): record seed + schedule position + cell
    # metadata BEFORE captures or harness start, so a partial / aborted
    # run still has the rerun traceability §R5 requires. Distinct from
    # the harness's manifest.yaml (which is written last, after capture).
    {
        echo "seed: ${SEED}"
        echo "schedule_position: ${pos}"
        echo "total_runs: ${TOTAL_RUNS}"
        echo "cell: ${cell}"
        echo "n: ${n}"
        echo "rep: ${rep}"
        echo "replicas: ${replicas}"
        echo "is_control: ${is_control}"
        echo "is_head: ${is_head}"
        echo "nats_config: '${CELL_NATS_CONFIG[$cell]:-default}'"
        if [[ "$is_control" == "true" ]]; then
            echo "flags: '(control — no harness)'"
        elif [[ "$is_head" == "true" ]]; then
            echo "flags: '(HEAD — manual pin-swap required)'"
        else
            echo "flags: '--n=${n} ${flags}'"
        fi
    } > "${run_dir}/run-meta.yaml"

    # Reset the rig — always, before every run.
    # Pass IOPS_RIG_NATS_REPLICAS so the Makefile picks up the right profile.
    # Pass IOPS_RIG_NATS_CONFIG when the cell defines a per-cell server.conf
    # (e.g. M2.C); docker-compose interpolates it into the bind-mount.
    # Empty value falls back to docker-compose's built-in default
    # (./nats-server.conf) — same as every M1.x cell.
    local nats_config="${CELL_NATS_CONFIG[$cell]:-}"
    echo "  resetting rig (replicas=${replicas}, nats_config=${nats_config:-default})..."
    if ! IOPS_RIG_NATS_REPLICAS="${replicas}" \
         IOPS_RIG_NATS_CONFIG="${nats_config}" \
         make -C "$RIG_DIR" reset; then
        echo "  FAILED: make reset failed; aborting run" >&2
        echo "make reset failed" > "${run_dir}/failed.txt"
        return 1
    fi

    # Start captures.
    start_captures "$run_dir" "$replicas" "$CAPTURE_TOTAL"
    echo "  captures started (PIDs: ${CAPTURE_PIDS[*]:-none})"

    local harness_rc=0
    local control_start=""

    if [[ "$is_control" == "true" ]]; then
        # M1.0 — no harness; just let captures run for the full window.
        # Record start time now so the window is accurate even if stop
        # or verify takes extra time.
        control_start="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        echo "  M1.0 control: sleeping ${CAPTURE_TOTAL}s (no harness)..."
        sleep "$CAPTURE_TOTAL" || true
        echo "  M1.0 capture window complete."
    else
        # Run the harness binary.
        echo "  running harness (--n=${n} ${flags})..."
        IOPS_RIG_RUN_INDEX="${pos}" \
        IOPS_RIG_NATS_IMAGE="${IOPS_RIG_NATS_IMAGE:-nats:2.12.6}" \
        IOPS_RIG_NATS_IMAGE_DIGEST="${IOPS_RIG_NATS_IMAGE_DIGEST:-}" \
            "$HARNESS_BIN" \
                --n="$n" \
                --output-dir="$run_dir" \
                --warmup="${WARMUP_SECS}s" \
                --capture-window="${CAPTURE_SECS}s" \
                $flags \
            || harness_rc=$?

        if [[ "$harness_rc" -ne 0 ]]; then
            echo "  harness exited with code ${harness_rc}" >&2
        fi
    fi

    # Stop captures regardless of harness outcome.
    echo "  stopping captures..."
    stop_captures

    if [[ "$harness_rc" -ne 0 ]]; then
        printf 'harness exit code %d\n' "$harness_rc" > "${run_dir}/failed.txt"
        echo "  run FAILED (harness_rc=${harness_rc}); continuing campaign."
        return 1
    fi

    # Verify capture outputs: check capture process exit codes (populated
    # by stop_captures) and file size. A capture that wrote partial data
    # then exited non-zero is a hard run failure — no masking.
    if ! verify_captures "$run_dir"; then
        echo "  run FAILED (capture verify); continuing campaign."
        return 1
    fi

    if [[ "$is_control" == "true" ]]; then
        # M1.0 synthetic inputs — written only after captures complete and
        # verify_captures passes. The aggregator requires manifest.yaml +
        # rpc_counts.csv for every run dir; for control runs we synthesize
        # both so the aggregator emits a real aggregated.csv (noise-floor /
        # MDE source per §R5). The synthetic manifest declares status=ok so
        # the aggregator's status gate passes.
        local control_end
        control_end="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        {
            echo "status: ok"
            echo "cell: ${cell}"
            echo "partiVersion: control"
            echo "runIndex: ${pos}"
            echo "startedAt: ${control_start}"
            echo "endedAt: ${control_end}"
        } > "${run_dir}/manifest.yaml"
        # Header-only rpc_counts.csv — no RPC rows for a no-harness run.
        echo "t_unix_ns,worker_idx,bucket,op,count" > "${run_dir}/rpc_counts.csv"
    fi

    # Run aggregator. A failure here is now a hard run failure
    # (previously masked as "run OK" with aggregate-failed.txt) — see
    # P0/P1 review findings.
    if command -v "$AGGREGATE_BIN" >/dev/null 2>&1 || [[ -x "$AGGREGATE_BIN" ]]; then
        echo "  aggregating..."
        if ! "$AGGREGATE_BIN" --run-dir "$run_dir"; then
            echo "  aggregate FAILED; marking aggregate-failed.txt." >&2
            echo "aggregate exited non-zero" > "${run_dir}/aggregate-failed.txt"
            return 1
        fi
    else
        echo "  aggregate binary not found in PATH; marking aggregate-failed.txt (run ${run_dir})" >&2
        echo "aggregate binary not found" > "${run_dir}/aggregate-failed.txt"
        return 1
    fi

    echo "  run OK."
    return 0
}

# ---------------------------------------------------------------------------
# Main campaign loop.
# ---------------------------------------------------------------------------
success_count=0
FAILURE_MSGS=()

pos=0
while IFS=' ' read -r cell n rep; do
    pos=$(( pos + 1 ))
    if ! run_one "$pos" "$cell" "$n" "$rep"; then
        FAILURE_MSGS+=("pos=${pos} cell=${cell} N=${n} rep=${rep}")
    else
        success_count=$(( success_count + 1 ))
    fi
done <<< "$SHUFFLED"

# ---------------------------------------------------------------------------
# Final campaign manifest.
# ---------------------------------------------------------------------------
END_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
write_campaign_manifest "$END_TIME" "$success_count" "$TOTAL_RUNS" "${FAILURE_MSGS[@]+"${FAILURE_MSGS[@]}"}"

echo ""
echo "=== Campaign complete ==="
echo "  Total:   ${TOTAL_RUNS}"
echo "  Success: ${success_count}"
echo "  Failed:  $(( TOTAL_RUNS - success_count ))"
echo "  Manifest: ${CAMPAIGN_MANIFEST}"

if [[ "$success_count" -lt "$TOTAL_RUNS" ]]; then
    exit 1
fi
