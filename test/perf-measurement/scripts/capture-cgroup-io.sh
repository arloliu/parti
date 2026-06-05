#!/usr/bin/env bash
# capture-cgroup-io.sh — 1 Hz cgroup v2 io.stat poller (Phase 3a, primary IOPS source).
#
# Reads /sys/fs/cgroup/system.slice/docker-<id>.scope/io.stat for each
# specified Docker container and writes raw cumulative counters at each
# sample tick. The Go aggregator (Phase 3e) diffs consecutive rows.
#
# Output format (space-separated):
#   # t_unix_ns container device rbytes wbytes rios wios
#   <t_unix_ns> <container> <MAJ:MIN> <rbytes> <wbytes> <rios> <wios>
#
# One row per (container, device) per sample. The first sample records the
# baseline cumulative counters; Phase 3e diffs row[n] - row[n-1] per device
# per container to produce per-second deltas.
#
# Usage:
#   capture-cgroup-io.sh --output PATH --duration SECONDS --containers NAME[,NAME...]
#
# Requirements: bash 4+, docker, cgroup v2 (systemd driver).
# Fail loud if a container's cgroup path does not exist.
set -euo pipefail

SCRIPT_NAME="$(basename "$0")"

usage() {
    cat >&2 <<EOF
Usage: $SCRIPT_NAME --output PATH --duration SECONDS --containers NAME[,NAME,...]

Arguments:
  --output PATH          File path to write raw capture output.
  --duration SECONDS     How many seconds to poll at 1 Hz.
  --containers NAMES     Comma-separated list of Docker container names,
                         e.g. perf-nats-1,perf-nats-2,perf-nats-3

Output format (space-separated):
  # t_unix_ns container device rbytes wbytes rios wios
  <unix_ns> <name> <MAJ:MIN> <rbytes> <wbytes> <rios> <wios>

Example:
  $SCRIPT_NAME --output /tmp/cgroup_io.raw --duration 600 \\
    --containers perf-nats-1,perf-nats-2,perf-nats-3
EOF
    exit 2
}

# Parse arguments.
OUTPUT=""
DURATION=""
CONTAINERS_RAW=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --output)    OUTPUT="$2";     shift 2 ;;
        --duration)  DURATION="$2";   shift 2 ;;
        --containers) CONTAINERS_RAW="$2"; shift 2 ;;
        -h|--help)   usage ;;
        *) echo "$SCRIPT_NAME: unknown argument: $1" >&2; usage ;;
    esac
done

[[ -z "$OUTPUT" ]]           && { echo "$SCRIPT_NAME: --output is required" >&2; usage; }
[[ -z "$DURATION" ]]         && { echo "$SCRIPT_NAME: --duration is required" >&2; usage; }
[[ -z "$CONTAINERS_RAW" ]]   && { echo "$SCRIPT_NAME: --containers is required" >&2; usage; }

# Validate duration is a positive integer.
if ! [[ "$DURATION" =~ ^[0-9]+$ ]] || [[ "$DURATION" -le 0 ]]; then
    echo "$SCRIPT_NAME: --duration must be a positive integer (seconds), got: $DURATION" >&2
    exit 1
fi

# Split container list.
IFS=',' read -ra CONTAINERS <<< "$CONTAINERS_RAW"

# Resolve each container's cgroup io.stat path via its full container ID.
# Docker on cgroup v2 with the systemd driver places containers under:
#   /sys/fs/cgroup/system.slice/docker-<full-id>.scope/io.stat
declare -A CGROUP_PATHS
for name in "${CONTAINERS[@]}"; do
    id=$(docker inspect -f '{{.Id}}' "$name" 2>/dev/null) || {
        echo "$SCRIPT_NAME: cannot inspect container '$name'; is it running?" >&2
        exit 1
    }
    cgroup_path="/sys/fs/cgroup/system.slice/docker-${id}.scope/io.stat"
    if [[ ! -f "$cgroup_path" ]]; then
        echo "$SCRIPT_NAME: cgroup path does not exist for container '$name': $cgroup_path" >&2
        echo "$SCRIPT_NAME: ensure the host uses cgroup v2 with the systemd driver." >&2
        exit 1
    fi
    CGROUP_PATHS["$name"]="$cgroup_path"
done

# Clean exit on SIGINT/SIGTERM — don't leave a partial line.
trap 'exit 0' INT TERM

# Create output directory if needed, then open the file.
mkdir -p "$(dirname "$OUTPUT")"
exec 3>"$OUTPUT"  # fd 3 = output file

echo "# t_unix_ns container device rbytes wbytes rios wios" >&3

# Deadline-based loop to avoid cumulative drift from sleep.
end=$(( $(date +%s) + DURATION ))

while [[ $(date +%s) -lt $end ]]; do
    ts_ns=$(date +%s%N)
    for name in "${CONTAINERS[@]}"; do
        cgroup_path="${CGROUP_PATHS[$name]}"
        # io.stat format per line: MAJ:MIN rbytes=N wbytes=N rios=N wios=N ...
        # Parse each device line; skip lines that don't contain rbytes.
        while IFS= read -r line; do
            [[ "$line" != *"rbytes="* ]] && continue
            dev="${line%% *}"
            rbytes="${line#*rbytes=}"; rbytes="${rbytes%% *}"
            wbytes="${line#*wbytes=}"; wbytes="${wbytes%% *}"
            rios="${line#*rios=}";     rios="${rios%% *}"
            wios="${line#*wios=}";     wios="${wios%% *}"
            echo "$ts_ns $name $dev $rbytes $wbytes $rios $wios" >&3
        done < "$cgroup_path"
    done
    sleep 1
done

exec 3>&-  # close output file
