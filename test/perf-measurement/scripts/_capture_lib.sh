#!/usr/bin/env bash
# _capture_lib.sh — Shared capture-lifecycle helpers for run-matrix.sh.
#
# Sourced by run-matrix.sh and by scripts/test-capture-failure.sh and
# scripts/test-readiness-gate.sh. Do not execute directly.
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

# wait_for_ready READY_ADDR TIMEOUT_SECS HARNESS_PID — poll the harness's
# /ready endpoint (cmd/harness/ready.go) every 2s until it reports 200,
# the harness process exits on its own, or TIMEOUT_SECS elapses.
#
# This is run-matrix.sh's capture-window readiness gate (RUNBOOK.md
# "Capture-window readiness gate"): run_one starts the harness in the
# background and calls this BEFORE starting the external capture
# scripts, so a capture window never starts while the cluster is still
# provisioning (workers/assignments not yet settled) — a fixed
# wall-clock capture start landed inside provisioning at N>=2000.
#
# Returns 0 once /ready reports 200. Returns 1 (with a stderr message)
# if the harness process exits before becoming ready, or if
# TIMEOUT_SECS elapses first — the caller must never start captures on
# a non-zero return.
wait_for_ready() {
    local ready_addr="$1" timeout_secs="$2" harness_pid="$3"
    local poll_interval=2
    local waited=0

    while true; do
        if ! kill -0 "$harness_pid" 2>/dev/null; then
            echo "  readiness gate: harness process (pid ${harness_pid}) exited before signaling readiness" >&2
            return 1
        fi
        if curl -sf --max-time 2 "http://${ready_addr}/ready" >/dev/null 2>&1; then
            return 0
        fi
        if [[ "$waited" -ge "$timeout_secs" ]]; then
            echo "  readiness gate: timed out after ${timeout_secs}s waiting for http://${ready_addr}/ready" >&2
            return 1
        fi
        sleep "$poll_interval"
        waited=$(( waited + poll_interval ))
    done
}
