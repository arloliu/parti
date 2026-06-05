#!/usr/bin/env bash
# capture-cgroup-cpumem.sh — 1 Hz cgroup v2 CPU + memory poller.
#
# Sibling of capture-cgroup-io.sh: same containers, same cadence, same
# full-ID resolution. Where the io poller reads the per-scope io.stat file,
# this one reads two files in each container's cgroup SCOPE DIRECTORY:
#   cpu.stat       — the "usage_usec" line (cumulative CPU time, microseconds).
#   memory.current — instantaneous resident bytes.
# The Go aggregator (internal/aggregate/cgroup_cpumem.go) diffs consecutive
# usage_usec rows into a per-second CPU fraction and carries memory through.
#
# Output format (space-separated):
#   # t_unix_ns container usage_usec memory_current_bytes
#   <t_unix_ns> <container> <usage_usec> <memory_current_bytes>
#
# One row per (container) per sample (no per-device axis, unlike io.stat).
# The first sample records the baseline cumulative usage_usec; the aggregator
# diffs row[n] - row[n-1] per container to produce a per-second CPU fraction
# (Δusage_usec / Δwall_usec, so 1.0 = one full core). memory_current_bytes is
# instantaneous and carried through (no delta).
#
# Usage:
#   capture-cgroup-cpumem.sh --output PATH --duration SECONDS --containers NAME[,NAME...]
#
# Requirements: bash 4+, docker, cgroup v2 (systemd driver).
# Fail loud if a container's cgroup scope dir (or either file) does not exist.
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
  # t_unix_ns container usage_usec memory_current_bytes
  <unix_ns> <name> <usage_usec> <memory_current_bytes>

Example:
  $SCRIPT_NAME --output /tmp/cgroup_cpumem.raw --duration 600 \\
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

# Resolve each container's cgroup scope dir via its full container ID.
# Docker on cgroup v2 with the systemd driver places containers under:
#   /sys/fs/cgroup/system.slice/docker-<full-id>.scope/
# We read cpu.stat and memory.current from that directory.
declare -A CGROUP_CPU
declare -A CGROUP_MEM
for name in "${CONTAINERS[@]}"; do
    id=$(docker inspect -f '{{.Id}}' "$name" 2>/dev/null) || {
        echo "$SCRIPT_NAME: cannot inspect container '$name'; is it running?" >&2
        exit 1
    }
    scope_dir="/sys/fs/cgroup/system.slice/docker-${id}.scope"
    cpu_path="${scope_dir}/cpu.stat"
    mem_path="${scope_dir}/memory.current"
    if [[ ! -f "$cpu_path" ]]; then
        echo "$SCRIPT_NAME: cgroup cpu.stat does not exist for container '$name': $cpu_path" >&2
        echo "$SCRIPT_NAME: ensure the host uses cgroup v2 with the systemd driver." >&2
        exit 1
    fi
    if [[ ! -f "$mem_path" ]]; then
        echo "$SCRIPT_NAME: cgroup memory.current does not exist for container '$name': $mem_path" >&2
        echo "$SCRIPT_NAME: ensure the host uses cgroup v2 with the systemd driver." >&2
        exit 1
    fi
    CGROUP_CPU["$name"]="$cpu_path"
    CGROUP_MEM["$name"]="$mem_path"
done

# Clean exit on SIGINT/SIGTERM — don't leave a partial line.
trap 'exit 0' INT TERM

# Create output directory if needed, then open the file.
mkdir -p "$(dirname "$OUTPUT")"
exec 3>"$OUTPUT"  # fd 3 = output file

echo "# t_unix_ns container usage_usec memory_current_bytes" >&3

# Deadline-based loop to avoid cumulative drift from sleep.
end=$(( $(date +%s) + DURATION ))

while [[ $(date +%s) -lt $end ]]; do
    ts_ns=$(date +%s%N)
    for name in "${CONTAINERS[@]}"; do
        # cpu.stat format: one "key value" per line; grab usage_usec.
        usage_usec=""
        while read -r key val _; do
            if [[ "$key" == "usage_usec" ]]; then
                usage_usec="$val"
                break
            fi
        done < "${CGROUP_CPU[$name]}"
        if [[ -z "$usage_usec" ]]; then
            echo "$SCRIPT_NAME: no usage_usec line in ${CGROUP_CPU[$name]} for '$name'" >&2
            exit 1
        fi
        # memory.current: a single integer (bytes). `read` returns non-zero on
        # a final line without a trailing newline, which `set -e` would turn
        # into a silent abort — tolerate the rc and validate the value instead.
        mem_bytes=""
        read -r mem_bytes < "${CGROUP_MEM[$name]}" || true
        if [[ -z "$mem_bytes" ]]; then
            echo "$SCRIPT_NAME: empty memory.current at ${CGROUP_MEM[$name]} for '$name'" >&2
            exit 1
        fi
        echo "$ts_ns $name $usage_usec $mem_bytes" >&3
    done
    sleep 1
done

exec 3>&-  # close output file
