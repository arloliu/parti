#!/usr/bin/env bash
# verify-isolation.sh — Assert CPU isolation for the IOPS measurement rig.
#
# Reads each NATS container's effective cpuset from the host-side cgroup
# (requires cgroup v2 with the systemd driver) and the harness process's
# CPU affinity via taskset, then asserts both match the documented split:
#
#   NATS containers : cpuset.cpus.effective == 0-7,16-23
#   Harness process : CPU affinity         == 8-15,24-31
#
# Exits 0 only if all checks pass. Exits non-zero if any check fails or
# any required tool/path is missing. Prints resolved vs expected values
# for every check so the caller can record them in manifest.yaml.
#
# The cgroup path follows Docker's systemd driver convention:
#   /sys/fs/cgroup/system.slice/docker-<full-id>.scope/cpuset.cpus.effective
# This is the same pattern used by capture-cgroup-io.sh. The nats:2.12.6
# image is scratch-based (no shell), so docker exec cannot be used.
#
# Usage:
#   verify-isolation.sh <HARNESS_PID> <CONTAINERS>
#   verify-isolation.sh --dry-run
#   verify-isolation.sh -h | --help
#
# Arguments:
#   HARNESS_PID   PID of the running harness process.
#   CONTAINERS    Comma-separated Docker container names,
#                 e.g. perf-nats-1,perf-nats-2,perf-nats-3
#
# Requirements: bash 4+, docker, taskset (util-linux), cgroup v2 (systemd driver).
set -euo pipefail

SCRIPT_NAME="$(basename "$0")"

EXPECTED_NATS_CPUSET="0-7,16-23"
EXPECTED_HARNESS_AFFINITY="8-15,24-31"

usage() {
    cat >&2 <<EOF
Usage: $SCRIPT_NAME <HARNESS_PID> <CONTAINERS>
       $SCRIPT_NAME --dry-run

Arguments:
  HARNESS_PID   PID of the running harness process.
  CONTAINERS    Comma-separated Docker container names,
                e.g. perf-nats-1,perf-nats-2,perf-nats-3

Options:
  --dry-run     Print the expected vs actual format without requiring a live
                cluster. Exits 0. Use to confirm the script is present and
                the format is readable before a campaign.
  -h, --help    Show this help text and exit.

Expected isolation split (design §10):
  NATS containers  : cpuset.cpus.effective = $EXPECTED_NATS_CPUSET
  Harness process  : CPU affinity          = $EXPECTED_HARNESS_AFFINITY

Exit codes:
  0  All checks passed.
  1  One or more checks failed (details printed to stdout).
  2  Usage error or missing required tool.
EOF
    exit 2
}

# ---------------------------------------------------------------------------
# --dry-run: print expected/actual template and exit 0.
# ---------------------------------------------------------------------------
if [[ "${1:-}" == "--dry-run" ]]; then
    cat <<EOF
verify-isolation dry-run (no live cluster required)
----------------------------------------------------
Expected NATS cpuset (each container) : $EXPECTED_NATS_CPUSET
Expected harness CPU affinity (PID)   : $EXPECTED_HARNESS_AFFINITY

Live run output format:
  NATS [perf-nats-1] cpuset.cpus.effective: 0-7,16-23  OK
  NATS [perf-nats-2] cpuset.cpus.effective: 0-7,16-23  OK
  NATS [perf-nats-3] cpuset.cpus.effective: 0-7,16-23  OK
  HARNESS [PID]      cpu-affinity-list:     8-15,24-31  OK
  isolation: all checks PASSED

(or MISMATCH with expected vs got on each failing line)
EOF
    exit 0
fi

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
    usage
fi

# ---------------------------------------------------------------------------
# Parse positional arguments.
# ---------------------------------------------------------------------------
if [[ $# -ne 2 ]]; then
    echo "$SCRIPT_NAME: expected 2 arguments (HARNESS_PID CONTAINERS), got $#" >&2
    usage
fi

HARNESS_PID="$1"
CONTAINERS_RAW="$2"

# ---------------------------------------------------------------------------
# Validate tools.
# ---------------------------------------------------------------------------
if ! command -v docker >/dev/null 2>&1; then
    echo "$SCRIPT_NAME: docker is required but not found in PATH." >&2
    exit 2
fi
if ! command -v taskset >/dev/null 2>&1; then
    echo "$SCRIPT_NAME: taskset is required but not found in PATH (install util-linux)." >&2
    exit 2
fi

# ---------------------------------------------------------------------------
# Validate harness PID.
# ---------------------------------------------------------------------------
if ! [[ "$HARNESS_PID" =~ ^[0-9]+$ ]]; then
    echo "$SCRIPT_NAME: HARNESS_PID must be a positive integer, got: $HARNESS_PID" >&2
    exit 2
fi
if ! kill -0 "$HARNESS_PID" 2>/dev/null; then
    echo "$SCRIPT_NAME: process $HARNESS_PID does not exist or is not accessible." >&2
    exit 1
fi

IFS=',' read -ra CONTAINERS <<< "$CONTAINERS_RAW"

# ---------------------------------------------------------------------------
# Check each NATS container's effective cpuset from the host cgroup.
# ---------------------------------------------------------------------------
failed=0

for name in "${CONTAINERS[@]}"; do
    # Resolve full container ID to build the cgroup path (same pattern as
    # capture-cgroup-io.sh; docker exec cannot be used — scratch image).
    id=""
    id=$(docker inspect -f '{{.Id}}' "$name" 2>/dev/null) || {
        echo "NATS [$name] ERROR: cannot inspect container; is it running?"
        failed=1
        continue
    }

    cgroup_path="/sys/fs/cgroup/system.slice/docker-${id}.scope/cpuset.cpus.effective"
    if [[ ! -f "$cgroup_path" ]]; then
        echo "NATS [$name] ERROR: cgroup path not found: $cgroup_path"
        echo "  Ensure the host uses cgroup v2 with the systemd driver."
        failed=1
        continue
    fi

    actual_cpuset=""
    actual_cpuset=$(< "$cgroup_path") || {
        echo "NATS [$name] ERROR: cannot read $cgroup_path"
        failed=1
        continue
    }
    # Trim trailing whitespace/newlines.
    actual_cpuset="${actual_cpuset%$'\n'}"
    actual_cpuset="${actual_cpuset%$'\r'}"

    if [[ "$actual_cpuset" == "$EXPECTED_NATS_CPUSET" ]]; then
        echo "NATS [$name] cpuset.cpus.effective: $actual_cpuset  OK"
    else
        echo "NATS [$name] cpuset.cpus.effective: $actual_cpuset  MISMATCH (expected: $EXPECTED_NATS_CPUSET)"
        failed=1
    fi
done

# ---------------------------------------------------------------------------
# Check harness process CPU affinity via taskset.
# ---------------------------------------------------------------------------
# taskset -pc <pid> prints: "pid N's current affinity list: 8-15,24-31"
taskset_out=""
taskset_out=$(taskset -pc "$HARNESS_PID" 2>/dev/null) || {
    echo "HARNESS [PID $HARNESS_PID] ERROR: taskset failed; process may have exited."
    failed=1
}

if [[ -n "$taskset_out" ]]; then
    # Strip everything up to and including the colon + space.
    actual_affinity="${taskset_out##*: }"
    # Trim trailing whitespace.
    actual_affinity="${actual_affinity%$'\n'}"
    actual_affinity="${actual_affinity%$'\r'}"

    if [[ "$actual_affinity" == "$EXPECTED_HARNESS_AFFINITY" ]]; then
        echo "HARNESS [PID $HARNESS_PID] cpu-affinity-list: $actual_affinity  OK"
    else
        echo "HARNESS [PID $HARNESS_PID] cpu-affinity-list: $actual_affinity  MISMATCH (expected: $EXPECTED_HARNESS_AFFINITY)"
        failed=1
    fi
fi

# ---------------------------------------------------------------------------
# Final verdict.
# ---------------------------------------------------------------------------
if [[ $failed -ne 0 ]]; then
    echo "isolation: FAILED — abort the run, fix the isolation setup."
    exit 1
fi

echo "isolation: all checks PASSED"
exit 0
