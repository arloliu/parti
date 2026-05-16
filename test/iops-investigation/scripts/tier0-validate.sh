#!/usr/bin/env bash
# tier0-validate.sh — measurement-chain validation gate.
#
# Tier 0 proves the rig is producing honest signal before any Tier 1+
# campaign is run. It does NOT test the hypotheses; it tests the
# instrument. Four checks:
#
#   1. capture-chain   — empty rig (M1.0), verify all 4 capture files
#                        landed + aggregator strict mode passes.
#   2. wrapper-counters — M1.2 baseline, verify heartbeat / stable-ID /
#                        election RPC rates match the §H3 predictions
#                        within ±50 %.
#   3. reproducibility — M1.2 × N=2000 × 3 reps, verify per-run mean
#                        block_write_iops varies by < 25 % (CV).
#   4. mde             — M1.0 × {500,1000,3000} × 3 reps, run analyze.py
#                        --slopes-only, verify the read-RPC MDE is below
#                        the §R5 acceptance gate (< 0.167 RPC/s/partition).
#
# Each check schedules its runs via run-matrix.sh with compressed
# warmup/capture windows (defaults: 30 s / 60 s) so the full Tier 0
# completes in ~45 minutes. PASS / FAIL is printed per check; the script
# exits non-zero if any check failed so it can gate an automated Tier 1
# kickoff.
#
# Usage:
#   tier0-validate.sh [--seed N] [--results-dir PATH] [--warmup-secs N] \
#                     [--capture-secs N] [--skip CHECK[,CHECK...]] [--dry-run]
#
# Examples:
#   # Default: run all four checks, fresh seed.
#   tier0-validate.sh --seed 42
#
#   # Skip the long MDE check during iteration.
#   tier0-validate.sh --seed 42 --skip mde
#
#   # Preview which runs would execute.
#   tier0-validate.sh --dry-run --seed 42

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RIG_DIR="$(dirname "$SCRIPT_DIR")"

# Prefer the rig-local venv (created from scripts/requirements.txt) for
# analyze.py so the check can run on a host that doesn't have
# pandas/numpy/statsmodels in /usr/bin/python3. Fall back to system
# python3 if no venv is present — the analyze.py call will then fail
# loud with the missing-deps message.
if [[ -x "${RIG_DIR}/.venv/bin/python" ]]; then
    PYTHON_BIN="${RIG_DIR}/.venv/bin/python"
else
    PYTHON_BIN="python3"
fi

# Defaults.
SEED=""
RESULTS_ROOT=""
WARMUP_SECS=30
CAPTURE_SECS=60
SKIP_LIST=""
DRY_RUN=false

# Predicted per-cluster RPC rates for W=5 workers at parti defaults
# (HBI=5s, WorkerIDTTL=75s → renew at 25s, ElectionTimeout=10s → tick at 3.33s).
# These drive the wrapper-counter check.
PREDICTED_HEARTBEAT_OPS_PER_SEC="1.0"     # 5 workers × 1 Put / 5s
PREDICTED_STABLEID_OPS_PER_SEC="0.2"      # 5 workers × 1 Put / 25s
PREDICTED_ELECTION_OPS_PER_SEC="1.5"      # 1 leader × 1/3.33s + 4 followers × 1/3.33s
TOLERANCE_PCT=50                          # ±50 %; wide because startup transients

# Pass criteria for the slope-side checks.
REPRO_CV_MAX="0.25"                       # 25 % across 3 reps
MDE_MAX_RPC_PER_PARTITION="0.167"         # §R5 acceptance gate

usage() {
    cat >&2 <<EOF
Usage: tier0-validate.sh --seed N [options]

Required:
  --seed N            Integer seed for run scheduling. Recorded in the
                      per-check campaign manifests for reproducibility.

Options:
  --results-dir PATH  Where the four per-check campaign dirs land
                      (default: results/tier0-<timestamp>/).
  --warmup-secs N     Compressed warmup window (default: 30).
                      Must be >= 5 s; raise if workers don't reach
                      Stable in time on slow hosts.
  --capture-secs N    Compressed capture window (default: 60).
                      Must be >= 10 s.
  --skip LIST         Comma-separated checks to skip: capture-chain,
                      wrapper-counters, reproducibility, mde.
                      Example: --skip mde,reproducibility for the
                      ~6-min smoke pass.
  --dry-run           Print the four scheduled invocations and exit
                      without running anything.
  -h, --help          Show this message and exit.

A check passes when its observed value lies within the documented
tolerance band. If any check fails, the script exits non-zero — do
not start a Tier 1 campaign until every Tier 0 check passes.
EOF
    exit 2
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --seed)         SEED="$2";          shift 2 ;;
        --results-dir)  RESULTS_ROOT="$2";  shift 2 ;;
        --warmup-secs)  WARMUP_SECS="$2";   shift 2 ;;
        --capture-secs) CAPTURE_SECS="$2";  shift 2 ;;
        --skip)         SKIP_LIST="$2";     shift 2 ;;
        --dry-run)      DRY_RUN=true;       shift ;;
        -h|--help)      usage ;;
        *) echo "tier0-validate.sh: unknown argument: $1" >&2; usage ;;
    esac
done

[[ -z "$SEED" ]] && { echo "tier0-validate.sh: --seed is required" >&2; usage; }
if ! [[ "$SEED" =~ ^-?[0-9]+$ ]]; then
    echo "tier0-validate.sh: --seed must be an integer" >&2
    exit 1
fi
if [[ -z "$RESULTS_ROOT" ]]; then
    RESULTS_ROOT="${RIG_DIR}/results/tier0-$(date +%Y%m%d-%H%M%S)"
fi
mkdir -p "$RESULTS_ROOT"

# Skip set.
declare -A SKIP=()
if [[ -n "$SKIP_LIST" ]]; then
    IFS=',' read -ra _s <<< "$SKIP_LIST"
    for s in "${_s[@]}"; do SKIP["$s"]=1; done
fi

# Track per-check verdict.
declare -A VERDICT=()
declare -A DETAIL=()

skipped() {
    [[ -n "${SKIP[$1]+set}" ]]
}

record() {
    # record <check-name> <PASS|FAIL|SKIP> <one-line detail>
    VERDICT["$1"]="$2"
    DETAIL["$1"]="$3"
    case "$2" in
        PASS) echo "  -> PASS: $3" ;;
        FAIL) echo "  -> FAIL: $3" >&2 ;;
        SKIP) echo "  -> SKIP: $3" ;;
    esac
}

# --- Shared helpers ----------------------------------------------------

# Sum a named column of aggregated.csv across all host rows. The host
# row is the synthetic row carrying cluster-wide RPC rates. Returns the
# integer total ops over the capture window (per-second rates summed at
# 1 Hz = total ops).
sum_host_column() {
    local agg_csv="$1"
    local col="$2"
    awk -F',' -v col="$col" '
        NR==1 {
            for (i=1; i<=NF; i++) if ($i == col) ci = i
            for (i=1; i<=NF; i++) if ($i == "node") ni = i
            if (!ci || !ni) { print "ERR: column not found"; exit 1 }
            next
        }
        $ni == "host" { sum += $ci + 0 }
        END { printf "%.4f\n", sum }
    ' "$agg_csv"
}

# Mean of a named column of aggregated.csv across all host rows.
mean_host_column() {
    local agg_csv="$1"
    local col="$2"
    # The aggregated.csv covers (warmup + capture) seconds — captures
    # start before the harness Reset() and stop after the capture
    # window. RPC columns are zero during warmup (the wrapper is reset
    # at the warmup boundary), so a naive mean over all host rows
    # under-reports rates by `capture / (warmup + capture)`. Skip rows
    # where ALL non-zero RPC activity is zero by gating on the
    # presence of any rpc_write_* column being non-zero in the same
    # row, OR by restricting to the t_s range where the requested
    # column is non-zero. The latter is what we use here: include any
    # second where `col` has produced data at least once for any
    # worker, which approximates the capture-window range. For ops
    # that legitimately remain zero in the capture window (e.g.
    # rpc_read_parti-handoff with two-phase off), this returns 0
    # correctly.
    awk -F',' -v col="$col" '
        NR==1 {
            for (i=1; i<=NF; i++) if ($i == col) ci = i
            for (i=1; i<=NF; i++) if ($i == "node") ni = i
            for (i=1; i<=NF; i++) if ($i == "t_s") ti = i
            if (!ci || !ni || !ti) { print "ERR: column not found"; exit 1 }
            next
        }
        # First pass: find the t_s range where col has any non-zero
        # value. That defines the capture window. Cache rows to a
        # second pass.
        $ni == "host" {
            rows[NR] = $0
            t_vals[NR] = $ti + 0
            c_vals[NR] = $ci + 0
            if (c_vals[NR] > 0) {
                if (min_t == 0 || t_vals[NR] < min_t) min_t = t_vals[NR]
                if (t_vals[NR] > max_t) max_t = t_vals[NR]
            }
        }
        END {
            if (min_t == 0) { print "0"; exit }
            for (k in t_vals) {
                if (t_vals[k] >= min_t && t_vals[k] <= max_t) {
                    sum += c_vals[k]
                    n++
                }
            }
            if (n > 0) printf "%.4f\n", sum / n; else print "0"
        }
    ' "$agg_csv"
}

# Mean of iops_write across all per-container rows (whole-cluster IOPS
# rate). The aggregated.csv has one row per (second, node) where the
# container rows hold iops_*/bytes_* and the host row holds rpc_*.
mean_cluster_block_iops_write() {
    local agg_csv="$1"
    awk -F',' '
        NR==1 {
            for (i=1; i<=NF; i++) if ($i == "iops_write") ci = i
            for (i=1; i<=NF; i++) if ($i == "node") ni = i
            for (i=1; i<=NF; i++) if ($i == "t_s") ti = i
            if (!ci || !ni || !ti) { print "ERR"; exit 1 }
            next
        }
        $ni != "host" && $ni != "" {
            per_sec[$ti] += $ci + 0
        }
        END {
            n = 0; total = 0
            for (t in per_sec) { total += per_sec[t]; n++ }
            if (n > 0) printf "%.4f\n", total / n; else print "0"
        }
    ' "$agg_csv"
}

# Coefficient of variation across a list of numeric values.
coefficient_of_variation() {
    python3 - "$@" <<'PY'
import sys, statistics
vals = [float(v) for v in sys.argv[1:]]
if len(vals) < 2:
    print("0")
    sys.exit(0)
mean = statistics.fmean(vals)
sd = statistics.stdev(vals)
if mean == 0:
    print("inf")
else:
    print(f"{sd/mean:.4f}")
PY
}

# Compare two floats. Returns true if |a-b|/|b| <= tol (b is reference).
within_tolerance_pct() {
    local observed="$1"
    local reference="$2"
    local tol_pct="$3"
    python3 -c "
o, r, t = float('$observed'), float('$reference'), float('$tol_pct') / 100.0
if r == 0: print('false' if abs(o) > t else 'true')
else: print('true' if abs(o - r) / abs(r) <= t else 'false')
"
}

# Less-than comparison on floats: returns true if a < b.
float_lt() {
    python3 -c "print('true' if float('$1') < float('$2') else 'false')"
}

# --- Check 1: capture chain --------------------------------------------

check_capture_chain() {
    local check="capture-chain"
    if skipped "$check"; then record "$check" SKIP "user --skip"; return 0; fi
    echo ""
    echo ">>> CHECK 1: capture-chain (M1.0, 1 rep, N=500, ~3 min)"
    local dir="${RESULTS_ROOT}/${check}"
    if $DRY_RUN; then
        echo "[dry-run] run-matrix.sh --seed $SEED --cells M1.0 --reps 1 --n-values 500"
        record "$check" SKIP "dry-run"
        return 0
    fi
    if ! bash "${SCRIPT_DIR}/run-matrix.sh" \
            --seed "$SEED" \
            --cells M1.0 \
            --reps 1 \
            --n-values 500 \
            --warmup-secs "$WARMUP_SECS" \
            --capture-secs "$CAPTURE_SECS" \
            --results-dir "$dir"; then
        record "$check" FAIL "run-matrix.sh exited non-zero"
        return 1
    fi
    # Locate the single run dir.
    local run_dir
    run_dir=$(find "$dir" -maxdepth 1 -type d -name 'run-*' | head -n1 || true)
    if [[ -z "$run_dir" ]]; then
        record "$check" FAIL "no run-* subdir produced"
        return 1
    fi
    # Verify the four capture files exist and are non-empty.
    local missing=""
    for f in cgroup_io.raw iostat.raw jsz.raw node_exporter.prom aggregated.csv manifest.yaml; do
        if [[ ! -s "$run_dir/$f" ]]; then missing="$missing $f"; fi
    done
    if [[ -n "$missing" ]]; then
        record "$check" FAIL "missing/empty:${missing}"
        return 1
    fi
    # aggregator strict-mode already passed (run-matrix would have failed otherwise).
    record "$check" PASS "all 4 capture sources + aggregated.csv non-empty at $run_dir"
}

# --- Check 2: wrapper counter accuracy ---------------------------------

check_wrapper_counters() {
    local check="wrapper-counters"
    if skipped "$check"; then record "$check" SKIP "user --skip"; return 0; fi
    echo ""
    echo ">>> CHECK 2: wrapper-counters (M1.2, 1 rep, N=500, ~3 min)"
    local dir="${RESULTS_ROOT}/${check}"
    if $DRY_RUN; then
        echo "[dry-run] run-matrix.sh --seed $SEED --cells M1.2 --reps 1 --n-values 500"
        record "$check" SKIP "dry-run"
        return 0
    fi
    if ! bash "${SCRIPT_DIR}/run-matrix.sh" \
            --seed "$SEED" \
            --cells M1.2 \
            --reps 1 \
            --n-values 500 \
            --warmup-secs "$WARMUP_SECS" \
            --capture-secs "$CAPTURE_SECS" \
            --results-dir "$dir"; then
        record "$check" FAIL "run-matrix.sh exited non-zero"
        return 1
    fi
    local run_dir
    run_dir=$(find "$dir" -maxdepth 1 -type d -name 'run-*' | head -n1 || true)
    if [[ -z "$run_dir" || ! -s "$run_dir/aggregated.csv" ]]; then
        record "$check" FAIL "no aggregated.csv"
        return 1
    fi
    # Mean per-second cluster heartbeat Put rate (rpc_write_parti-heartbeat).
    local hb_rate sid_rate elc_rate
    hb_rate=$(mean_host_column "$run_dir/aggregated.csv" "rpc_write_parti-heartbeat")
    sid_rate=$(mean_host_column "$run_dir/aggregated.csv" "rpc_write_parti-stableid")
    # Election uses Create (followers) + Update (leader); count both as writes.
    elc_rate=$(mean_host_column "$run_dir/aggregated.csv" "rpc_write_parti-election")
    local hb_ok sid_ok elc_ok
    hb_ok=$(within_tolerance_pct "$hb_rate" "$PREDICTED_HEARTBEAT_OPS_PER_SEC" "$TOLERANCE_PCT")
    sid_ok=$(within_tolerance_pct "$sid_rate" "$PREDICTED_STABLEID_OPS_PER_SEC" "$TOLERANCE_PCT")
    elc_ok=$(within_tolerance_pct "$elc_rate" "$PREDICTED_ELECTION_OPS_PER_SEC" "$TOLERANCE_PCT")
    local msg
    msg=$(printf 'heartbeat=%s/s (expected ~%s) %s; stable-id=%s/s (expected ~%s) %s; election=%s/s (expected ~%s) %s' \
        "$hb_rate" "$PREDICTED_HEARTBEAT_OPS_PER_SEC" "$hb_ok" \
        "$sid_rate" "$PREDICTED_STABLEID_OPS_PER_SEC" "$sid_ok" \
        "$elc_rate" "$PREDICTED_ELECTION_OPS_PER_SEC" "$elc_ok")
    if [[ "$hb_ok" == "true" && "$sid_ok" == "true" && "$elc_ok" == "true" ]]; then
        record "$check" PASS "$msg"
    else
        record "$check" FAIL "$msg (tolerance ±${TOLERANCE_PCT}%)"
        return 1
    fi
}

# --- Check 3: reproducibility ------------------------------------------

check_reproducibility() {
    local check="reproducibility"
    if skipped "$check"; then record "$check" SKIP "user --skip"; return 0; fi
    echo ""
    echo ">>> CHECK 3: reproducibility (M1.2, 3 reps, N=2000, ~9 min)"
    local dir="${RESULTS_ROOT}/${check}"
    if $DRY_RUN; then
        echo "[dry-run] run-matrix.sh --seed $SEED --cells M1.2 --reps 3 --n-values 2000"
        record "$check" SKIP "dry-run"
        return 0
    fi
    if ! bash "${SCRIPT_DIR}/run-matrix.sh" \
            --seed "$SEED" \
            --cells M1.2 \
            --reps 3 \
            --n-values 2000 \
            --warmup-secs "$WARMUP_SECS" \
            --capture-secs "$CAPTURE_SECS" \
            --results-dir "$dir"; then
        record "$check" FAIL "run-matrix.sh exited non-zero"
        return 1
    fi
    # Compute mean cluster block_write_iops per run.
    local -a per_run_means=()
    while IFS= read -r run_dir; do
        if [[ -s "$run_dir/aggregated.csv" ]]; then
            per_run_means+=("$(mean_cluster_block_iops_write "$run_dir/aggregated.csv")")
        fi
    done < <(find "$dir" -maxdepth 1 -type d -name 'run-*' | sort)
    if (( ${#per_run_means[@]} < 3 )); then
        record "$check" FAIL "only ${#per_run_means[@]} aggregated.csv files; need 3"
        return 1
    fi
    local cv
    cv=$(coefficient_of_variation "${per_run_means[@]}")
    local cv_ok
    cv_ok=$(float_lt "$cv" "$REPRO_CV_MAX")
    if [[ "$cv_ok" == "true" ]]; then
        record "$check" PASS "CV=${cv} across reps [${per_run_means[*]}] (gate < ${REPRO_CV_MAX})"
    else
        record "$check" FAIL "CV=${cv} across reps [${per_run_means[*]}] exceeds ${REPRO_CV_MAX}"
        return 1
    fi
}

# --- Check 4: MDE ------------------------------------------------------

check_mde() {
    local check="mde"
    if skipped "$check"; then record "$check" SKIP "user --skip"; return 0; fi
    echo ""
    echo ">>> CHECK 4: mde (M1.0, 3 N × 3 reps, ~27 min)"
    local dir="${RESULTS_ROOT}/${check}"
    if $DRY_RUN; then
        echo "[dry-run] run-matrix.sh --seed $SEED --cells M1.0 --reps 3 --n-values 500,1000,3000"
        echo "[dry-run] analyze.py --slopes-only --results-dir <dir>"
        record "$check" SKIP "dry-run"
        return 0
    fi
    if ! bash "${SCRIPT_DIR}/run-matrix.sh" \
            --seed "$SEED" \
            --cells M1.0 \
            --reps 3 \
            --n-values 500,1000,3000 \
            --warmup-secs "$WARMUP_SECS" \
            --capture-secs "$CAPTURE_SECS" \
            --results-dir "$dir"; then
        record "$check" FAIL "run-matrix.sh exited non-zero"
        return 1
    fi
    # Run analyze.py --slopes-only.
    local out_dir="${dir}/analysis"
    if ! "$PYTHON_BIN" "${SCRIPT_DIR}/analyze.py" \
            --results-dir "$dir" \
            --slopes-only \
            --out "$out_dir"; then
        record "$check" FAIL "analyze.py exited non-zero (run: python3 -m venv ${RIG_DIR}/.venv && ${RIG_DIR}/.venv/bin/pip install -r ${SCRIPT_DIR}/requirements.txt)"
        return 1
    fi
    if [[ ! -s "$out_dir/mde.csv" ]]; then
        record "$check" FAIL "analyze.py did not produce mde.csv"
        return 1
    fi
    # Read MDE for read_rpc_ops column.
    local mde_value
    mde_value=$(awk -F',' '
        NR==1 {
            for (i=1; i<=NF; i++) if ($i == "column") ci = i
            for (i=1; i<=NF; i++) if ($i == "mde_slope") mi = i
            next
        }
        $ci == "read_rpc_ops" { print $mi; exit }
    ' "$out_dir/mde.csv")
    if [[ -z "$mde_value" ]]; then
        record "$check" FAIL "mde.csv has no read_rpc_ops row"
        return 1
    fi
    local mde_ok
    mde_ok=$(float_lt "$mde_value" "$MDE_MAX_RPC_PER_PARTITION")
    if [[ "$mde_ok" == "true" ]]; then
        record "$check" PASS "MDE(read_rpc_ops)=${mde_value} < ${MDE_MAX_RPC_PER_PARTITION}"
    else
        record "$check" FAIL "MDE(read_rpc_ops)=${mde_value} exceeds ${MDE_MAX_RPC_PER_PARTITION}; host is too noisy or capture window too short"
        return 1
    fi
}

# --- Drive ------------------------------------------------------------

echo "tier0-validate.sh starting"
echo "  seed:         $SEED"
echo "  results dir:  $RESULTS_ROOT"
echo "  warmup secs:  $WARMUP_SECS"
echo "  capture secs: $CAPTURE_SECS"
if [[ -n "$SKIP_LIST" ]]; then
    echo "  skip:         $SKIP_LIST"
fi

check_capture_chain   || true
check_wrapper_counters || true
check_reproducibility  || true
check_mde              || true

echo ""
echo "=== Tier 0 validation summary ==="
printf '%-22s  %-6s  %s\n' "check" "verdict" "detail"
printf '%-22s  %-6s  %s\n' "----------------------" "------" "------"
any_fail=false
any_pass=false
for check in capture-chain wrapper-counters reproducibility mde; do
    verdict="${VERDICT[$check]:-MISSING}"
    detail="${DETAIL[$check]:-}"
    printf '%-22s  %-6s  %s\n' "$check" "$verdict" "$detail"
    [[ "$verdict" == "FAIL" ]] && any_fail=true
    [[ "$verdict" == "PASS" ]] && any_pass=true
done

echo ""
if $any_fail; then
    echo "OVERALL: FAIL — fix the failing check before proceeding to Tier 1." >&2
    exit 1
elif ! $any_pass; then
    # Every check was SKIPped (dry-run or --skip all). No real verdict.
    echo "OVERALL: INCONCLUSIVE — no check actually ran. Re-run without"
    echo "  --dry-run / --skip to gate Tier 1."
    exit 2
else
    echo "OVERALL: PASS — Tier 0 gates cleared; Tier 1 is safe to start."
    exit 0
fi
