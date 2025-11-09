#!/usr/bin/env bash
# parti/scripts/flake_detector.sh
# Long-running flake detection harness for Parti tests.
# Repeatedly runs unit, integration, and stress tests to surface intermittent (flaky) failures.
# Stops early when a test that previously passed starts failing, and emits diagnostic context.
#
# Environment Variables:
#   MAX_RUNS          Maximum iterations (default: 1000)
#   TEST_SET          Comma-separated set: unit,integration,stress (default: unit,integration,stress)
#   PARALLELISM       go test -p value (default: 4)
#   RACE              Enable race detector for unit/integration (default: 1) set to 0 to disable
#   STRESS            Enable long stress tests (PARTI_STRESS=1) (default: 1)
#   TEST_TIMEOUT      Timeout for unit/integration runs (go test -timeout) (default: 6m)
#   STRESS_TIMEOUT    Timeout for stress runs (go test -timeout) (default: 20m)
#   SLOW_LOG_THRESHOLD_SECONDS  Log tests exceeding this duration (default: 15)
#   REPORT_JSON       Path to JSON summary file (optional)
#   SAVE_LOGS_DIR     Directory to save individual run logs (optional)
#   GOFLAGS           Additional go flags (optional)
#
# Exit Codes:
#   0  All iterations completed with no flaky failures
#   2  Flaky test detected (failed after one or more previous passes)
#   3  Script misconfiguration
#
# Strategy:
#   - Run selected test sets sequentially inside each iteration for clearer attribution
#   - Track pass/fail state per package. If a package passes then later fails -> flaky
#   - Capture first failing run's full output and stop.
#   - Stress tests run with PARTI_STRESS=1 only if included.
#   - Integration tests always run with race detector unless RACE=0.
#
# JSON Report Schema (if REPORT_JSON set):
# {
#   "iterations_completed": <int>,
#   "max_runs": <int>,
#   "tests": {
#       "unit": {"runs": <int>, "failures": <int>},
#       "integration": {"runs": <int>, "failures": <int>},
#       "stress": {"runs": <int>, "failures": <int>}
#   },
#   "flakes": [
#       {"package": "<pkg>", "first_pass_run": <int>, "failed_run": <int>, "test_set": "unit|integration|stress"}
#   ],
#   "duration_seconds": <float>
# }
#
set -euo pipefail

MAX_RUNS=${MAX_RUNS:-1000}
TEST_SET=${TEST_SET:-unit,integration,stress}
PARALLELISM=${PARALLELISM:-4}
RACE=${RACE:-1}
STRESS=${STRESS:-1}
TEST_TIMEOUT=${TEST_TIMEOUT:-6m}
STRESS_TIMEOUT=${STRESS_TIMEOUT:-20m}
SLOW_LOG_THRESHOLD_SECONDS=${SLOW_LOG_THRESHOLD_SECONDS:-15}
REPORT_JSON=${REPORT_JSON:-}
SAVE_LOGS_DIR=${SAVE_LOGS_DIR:-}
GOFLAGS=${GOFLAGS:-}

START_TS=$(date +%s)
IFS=',' read -r -a TEST_SET_ARR <<<"$TEST_SET"

declare -A pkg_first_pass_run

declare -i unit_runs=0 integration_runs=0 stress_runs=0
declare -i unit_failures=0 integration_failures=0 stress_failures=0

log() { printf "[%s] %s\n" "$(date -Iseconds)" "$*"; }
err() { printf "[ERROR] %s\n" "$*" >&2; }

if [[ $MAX_RUNS -lt 1 ]]; then
  err "MAX_RUNS must be >= 1"
  exit 3
fi

if [[ -n $SAVE_LOGS_DIR ]]; then
  mkdir -p "$SAVE_LOGS_DIR"
fi

run_go_test() {
  local mode=$1 # unit|integration|stress
  local run=$2
  local env_prefix=(env)
  local packages=""
  local race_flag=""
  local timeout="$TEST_TIMEOUT"

  case $mode in
    unit)
      packages=$(make -s -n test-unit | awk '/go test/ {for (i=1;i<=NF;i++) if ($i ~ /^\./ || $i ~ /^github/) print $i}' | paste -sd ' ' -)
      race_flag=$([[ $RACE == 1 ]] && echo "-race" || echo "")
      ;;
    integration)
      packages="./test/integration/..."
      race_flag=$([[ $RACE == 1 ]] && echo "-race" || echo "")
      ;;
    stress)
      packages="./test/stress/..."
      if [[ $STRESS == 1 ]]; then
        env_prefix+=(PARTI_STRESS=1)
      fi
      timeout="$STRESS_TIMEOUT"
      ;;
    *)
      err "Unknown test mode: $mode"
      exit 3
      ;;
  esac

  local logfile="/dev/null"
  if [[ -n $SAVE_LOGS_DIR ]]; then
    logfile="$SAVE_LOGS_DIR/run_${run}_${mode}.log"
  fi

  log "Run $run | $mode tests starting"
  set +e
  local t_start=$(date +%s)
  # shellcheck disable=SC2086
  "${env_prefix[@]}" GOFLAGS="$GOFLAGS" CGO_ENABLED=1 go test $packages -count=1 -timeout="$timeout" -p "$PARALLELISM" $race_flag -failfast 2>&1 | tee "$logfile"
  local status=${PIPESTATUS[0]}
  set -e
  local t_end=$(date +%s)
  local elapsed=$((t_end - t_start))

  if [[ $elapsed -ge $SLOW_LOG_THRESHOLD_SECONDS ]]; then
    log "Slow $mode run (>=${SLOW_LOG_THRESHOLD_SECONDS}s): ${elapsed}s"
  fi

  # Collect packages (lines like 'ok   \t<package>')
  local passed_packages failed_packages
  passed_packages=$(grep -E '^ok\s' "$logfile" | awk '{print $2}') || true
  failed_packages=$(grep -E '^(FAIL|--- FAIL:)' "$logfile" | awk '{print $2}' | sed 's/\.go$//' | sort -u) || true

  if [[ $mode == unit ]]; then ((unit_runs++)); fi
  if [[ $mode == integration ]]; then ((integration_runs++)); fi
  if [[ $mode == stress ]]; then ((stress_runs++)); fi

  if [[ $status -ne 0 ]]; then
    # Determine flake: any failed package that had a previous pass
    for pkg in $failed_packages; do
      if [[ -n ${pkg_first_pass_run[$mode:$pkg]:-} ]]; then
        log "Flaky failure detected in package $pkg (first passed on run ${pkg_first_pass_run[$mode:$pkg]} now failing run $run in set $mode)"
        emit_report "$run" "$mode" "$pkg"
        log "Diagnostics: showing last 50 lines of failing log"
        tail -n 50 "$logfile" >&2 || true
        exit 2
      fi
    done
  fi

  # Record first pass for packages that passed this run and not recorded yet
  for pkg in $passed_packages; do
    if [[ -z ${pkg_first_pass_run[$mode:$pkg]:-} ]]; then
      pkg_first_pass_run[$mode:$pkg]=$run
    fi
  done

  if [[ $status -ne 0 ]]; then
    if [[ $mode == unit ]]; then ((unit_failures++)); fi
    if [[ $mode == integration ]]; then ((integration_failures++)); fi
    if [[ $mode == stress ]]; then ((stress_failures++)); fi
  fi

  log "Run $run | $mode tests completed (status=$status, elapsed=${elapsed}s)"
  return $status
}

emit_report() {
  local final_run=$1
  local flaky_mode=$2
  local flaky_pkg=$3
  if [[ -z $REPORT_JSON ]]; then
    return
  fi
  local end_ts=$(date +%s)
  local duration=$(python3 - <<EOF
import json,os,time
print(f"{end_ts-START_TS:.2f}")
EOF
  )
  python3 - <<EOF
import json, os
report = {
  "iterations_completed": final_run,
  "max_runs": int(os.environ.get("MAX_RUNS", "0")),
  "tests": {
    "unit": {"runs": int(os.environ.get("UNIT_RUNS", "0")), "failures": int(os.environ.get("UNIT_FAILURES", "0"))},
    "integration": {"runs": int(os.environ.get("INTEGRATION_RUNS", "0")), "failures": int(os.environ.get("INTEGRATION_FAILURES", "0"))},
    "stress": {"runs": int(os.environ.get("STRESS_RUNS", "0")), "failures": int(os.environ.get("STRESS_FAILURES", "0"))}
  },
  "flakes": [{"package": flaky_pkg, "first_pass_run": os.environ.get(f"FIRST_PASS_{flaky_mode}_{flaky_pkg}", "unknown"), "failed_run": final_run, "test_set": flaky_mode}],
  "duration_seconds": float(os.environ.get("DURATION", "0"))
}
with open(os.environ["REPORT_JSON"], 'w') as f:
  json.dump(report, f, indent=2)
EOF
}

log "Starting flake detection: MAX_RUNS=${MAX_RUNS} TEST_SET=${TEST_SET}"

run=1
while [[ $run -le $MAX_RUNS ]]; do
  for mode in "${TEST_SET_ARR[@]}"; do
    if ! run_go_test "$mode" "$run"; then
      # Failure without prior pass does not classify as flaky; continue loop unless early stop desired
      # We stop only when flaky logic triggers above. Non-flaky failure is direct failure.
      log "Non-flaky failure encountered in $mode on run $run (exiting)."
      exit 1
    fi
  done
  ((run++))
  if (( run % 10 == 0 )); then
    log "Progress: completed $((run-1)) iterations"
  fi
done

END_TS=$(date +%s)
TOTAL_DURATION=$((END_TS-START_TS))
log "Completed all ${MAX_RUNS} iterations without flaky failures (duration=${TOTAL_DURATION}s)."
exit 0
