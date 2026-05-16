#!/usr/bin/env bash
# capture-jsz.sh — 5-second NATS /jsz + /varz poller (Phase 3c).
#
# Polls each NATS monitoring endpoint every 5 seconds and appends a JSON
# line (ndjson) per fetch. Aborts immediately on any curl failure because
# missing server stats make the run invalid.
#
# Output format (one JSON object per line):
#   {"t_unix_ns": <unix_ns>, "node": "<host:port>", "endpoint": "jsz|varz", "body": {...}}
#
# Usage:
#   capture-jsz.sh --output PATH --duration SECONDS [--nats-nodes HOST:PORT[,HOST:PORT...]]
#
# Requirements: bash 4+, curl, jq.
set -euo pipefail

SCRIPT_NAME="$(basename "$0")"
POLL_INTERVAL=5

usage() {
    cat >&2 <<EOF
Usage: $SCRIPT_NAME --output PATH --duration SECONDS [--nats-nodes HOST:PORT[,...]]

Arguments:
  --output PATH           File path to write ndjson output.
  --duration SECONDS      How many seconds to poll.
  --nats-nodes HOST:PORT  Comma-separated monitoring endpoints (default: localhost:8222).

Output format (ndjson):
  {"t_unix_ns": <int>, "node": "<host:port>", "endpoint": "jsz|varz", "body": {...}}

Example:
  $SCRIPT_NAME --output /tmp/jsz.raw --duration 600 \\
    --nats-nodes localhost:8222
EOF
    exit 2
}

OUTPUT=""
DURATION=""
NATS_NODES_RAW="localhost:8222"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --output)      OUTPUT="$2";          shift 2 ;;
        --duration)    DURATION="$2";        shift 2 ;;
        --nats-nodes)  NATS_NODES_RAW="$2";  shift 2 ;;
        -h|--help)     usage ;;
        *) echo "$SCRIPT_NAME: unknown argument: $1" >&2; usage ;;
    esac
done

[[ -z "$OUTPUT" ]]   && { echo "$SCRIPT_NAME: --output is required" >&2; usage; }
[[ -z "$DURATION" ]] && { echo "$SCRIPT_NAME: --duration is required" >&2; usage; }

if ! [[ "$DURATION" =~ ^[0-9]+$ ]] || [[ "$DURATION" -le 0 ]]; then
    echo "$SCRIPT_NAME: --duration must be a positive integer (seconds), got: $DURATION" >&2
    exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
    echo "$SCRIPT_NAME: jq is required but not found in PATH." >&2
    exit 1
fi

IFS=',' read -ra NATS_NODES <<< "$NATS_NODES_RAW"

# Clean exit on SIGINT/SIGTERM — flush current state and exit.
trap 'exit 0' INT TERM

mkdir -p "$(dirname "$OUTPUT")"

end=$(( $(date +%s) + DURATION ))

while [[ $(date +%s) -lt $end ]]; do
    ts_ns=$(date +%s%N)
    for node in "${NATS_NODES[@]}"; do
        for endpoint in jsz varz; do
            case "$endpoint" in
                jsz)  url="http://${node}/jsz?streams=true&consumers=true" ;;
                varz) url="http://${node}/varz" ;;
            esac
            # Abort on curl failure — the run is invalid without server stats.
            body=$(curl -sf --max-time 4 "$url") || {
                echo "$SCRIPT_NAME: curl failed for $url — aborting." >&2
                exit 1
            }
            # Build JSON envelope via jq to safely embed the body.
            printf '%s\n' "$body" | \
                jq --arg node "$node" \
                   --arg ep   "$endpoint" \
                   --argjson t "$ts_ns" \
                   '{t_unix_ns: $t, node: $node, endpoint: $ep, body: .}' \
                >> "$OUTPUT"
        done
    done
    sleep "$POLL_INTERVAL"
done
