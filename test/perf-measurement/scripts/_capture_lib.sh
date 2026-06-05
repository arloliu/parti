#!/usr/bin/env bash
# _capture_lib.sh — Shared capture-lifecycle helpers for run-matrix.sh.
#
# Sourced by run-matrix.sh and by test/scripts/test-capture-failure.sh.
# Do not execute directly.
#
# Globals exported (caller must declare these as arrays before sourcing):
#   CAPTURE_PIDS  — PIDs of background capture processes.
#   CAPTURE_NAMES — Names parallel to CAPTURE_PIDS (for error messages).
#   CAPTURE_RCS   — Exit codes populated by stop_captures.

# stop_captures — send SIGTERM to all running capture PIDs, wait for each,
# and record their exit codes in CAPTURE_RCS.  Clears CAPTURE_PIDS and
# CAPTURE_NAMES on exit so the caller can detect a double-stop safely.
stop_captures() {
    CAPTURE_RCS=()
    if [[ ${#CAPTURE_PIDS[@]} -gt 0 ]]; then
        kill "${CAPTURE_PIDS[@]}" 2>/dev/null || true
        for pid in "${CAPTURE_PIDS[@]}"; do
            local rc=0
            wait "$pid" 2>/dev/null || rc=$?
            CAPTURE_RCS+=("$rc")
        done
        CAPTURE_PIDS=()
        CAPTURE_NAMES=()
    fi
}

# verify_captures RUN_DIR — check that:
#   1. Every capture process recorded in CAPTURE_RCS exited 0.
#   2. Every mandatory capture file exists and is non-empty.
# Returns 0 on success, 1 on any failure.  On failure writes a human-readable
# capture-failed.txt into RUN_DIR.
verify_captures() {
    local run_dir="$1"
    local failed=()

    # Check exit codes recorded by stop_captures.
    local i
    for (( i=0; i<${#CAPTURE_RCS[@]}; i++ )); do
        if [[ "${CAPTURE_RCS[$i]}" -ne 0 ]]; then
            failed+=("${CAPTURE_NAMES[$i]:-capture[$i]}(rc=${CAPTURE_RCS[$i]})")
        fi
    done

    # Check file size.
    local missing=()
    for f in cgroup_io.raw iostat.raw jsz.raw node_exporter.prom; do
        local p="${run_dir}/${f}"
        if [[ ! -s "$p" ]]; then
            missing+=("$f")
        fi
    done

    if [[ ${#failed[@]} -gt 0 || ${#missing[@]} -gt 0 ]]; then
        {
            [[ ${#failed[@]} -gt 0 ]]  && printf 'capture exit failures: %s\n' "${failed[*]}"
            [[ ${#missing[@]} -gt 0 ]] && printf 'missing or empty capture outputs: %s\n' "${missing[*]}"
        } > "${run_dir}/capture-failed.txt"
        [[ ${#failed[@]} -gt 0 ]]  && echo "  capture verify FAILED: non-zero exit from ${failed[*]}" >&2
        [[ ${#missing[@]} -gt 0 ]] && echo "  capture verify FAILED: missing/empty ${missing[*]}" >&2
        return 1
    fi
    return 0
}
