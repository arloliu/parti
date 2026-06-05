#!/usr/bin/env bash
# capture-node-exporter.sh — 5-second node_exporter metrics poller (Phase 3d).
#
# Curls localhost:9100/metrics every 5 seconds and appends each snapshot
# to --output with a timestamp comment header. The Go aggregator (Phase 3e)
# parses the file as a sequence of timestamped Prometheus text-format blocks.
#
# Output format:
#   # t_unix_ns <unix_ns>
#   <prometheus text format metrics>
#   (blank line between snapshots)
#
# Usage:
#   capture-node-exporter.sh --output PATH --duration SECONDS
#
# Requirements: bash 4+, curl. node_exporter must be reachable at localhost:9100
# (bring it up with: docker compose -f docker/docker-compose.yaml \
#   -f scripts/prometheus-node-exporter.yaml up -d).
set -euo pipefail

SCRIPT_NAME="$(basename "$0")"
POLL_INTERVAL=5
NODE_EXPORTER_URL="http://localhost:9100/metrics"

usage() {
    cat >&2 <<EOF
Usage: $SCRIPT_NAME --output PATH --duration SECONDS

Arguments:
  --output PATH      File path to write time-series metrics.
  --duration SECONDS How many seconds to poll.

Output format:
  # t_unix_ns <int>
  <prometheus text format>
  (blank line)
  # t_unix_ns <int>
  ...

Example:
  $SCRIPT_NAME --output /tmp/node_exporter.prom --duration 600
EOF
    exit 2
}

OUTPUT=""
DURATION=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --output)   OUTPUT="$2";   shift 2 ;;
        --duration) DURATION="$2"; shift 2 ;;
        -h|--help)  usage ;;
        *) echo "$SCRIPT_NAME: unknown argument: $1" >&2; usage ;;
    esac
done

[[ -z "$OUTPUT" ]]   && { echo "$SCRIPT_NAME: --output is required" >&2; usage; }
[[ -z "$DURATION" ]] && { echo "$SCRIPT_NAME: --duration is required" >&2; usage; }

if ! [[ "$DURATION" =~ ^[0-9]+$ ]] || [[ "$DURATION" -le 0 ]]; then
    echo "$SCRIPT_NAME: --duration must be a positive integer (seconds), got: $DURATION" >&2
    exit 1
fi

trap 'exit 0' INT TERM

mkdir -p "$(dirname "$OUTPUT")"

end=$(( $(date +%s) + DURATION ))

while [[ $(date +%s) -lt $end ]]; do
    ts_ns=$(date +%s%N)
    # Abort on curl failure — missing node_exporter data breaks the host sanity check.
    body=$(curl -sf --max-time 4 "$NODE_EXPORTER_URL") || {
        echo "$SCRIPT_NAME: curl failed for $NODE_EXPORTER_URL — aborting." >&2
        exit 1
    }
    {
        echo "# t_unix_ns $ts_ns"
        printf '%s\n' "$body"
        echo ""
    } >> "$OUTPUT"
    sleep "$POLL_INTERVAL"
done
