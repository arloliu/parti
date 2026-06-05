#!/usr/bin/env bash
# capture-jsz.sh — NATS /jsz + /varz poller (Phase 3c).
#
# Polls each NATS monitoring endpoint every POLL_INTERVAL seconds and appends a
# JSON line (ndjson) per fetch. A failed poll (curl error/timeout) is logged and
# SKIPPED, not fatal — the capture stays valid as long as at least one /jsz poll
# succeeded (a single slow response must not void a whole run). Exits non-zero
# only if every /jsz poll failed.
#
# IMPORTANT (scaling): the /jsz query is `?streams=true` only — it does NOT pass
# `consumers=true`. At thousands of consumers the per-consumer detail array makes
# the response large enough to blow the curl timeout (observed at N=5000), which
# previously aborted the whole capture. The metacontroller snapshot stats live in
# the top-level `meta_cluster` section (returned without either param), and the
# top-level `consumers`/`streams`/`total` counts cover sanity needs. `streams=true`
# stays (cheap: ~6 streams) for cmd/aggregate's per-stream parser.
#
# The raw /jsz body (including meta_cluster.snapshot fields: pending_entries,
# pending_size, last_time, last_duration) is stored verbatim in the envelope.
# Post-hoc analysis uses aggregate.ParseMetaSnapshot to extract snapshot stats.
# For metacontroller snapshot sweeps, pass --poll-interval 1 so rapid
# creation-phase snapshots are not undercounted (last_time shows only the latest).
#
# Output format (one JSON object per line):
#   {"t_unix_ns": <unix_ns>, "node": "<host:port>", "endpoint": "jsz|varz", "body": {...}}
#
# Usage:
#   capture-jsz.sh --output PATH --duration SECONDS [--nats-nodes HOST:PORT[,...]]
#                  [--poll-interval SECONDS]
#
# Requirements: bash 4+, curl, jq.
set -euo pipefail

SCRIPT_NAME="$(basename "$0")"
POLL_INTERVAL=5
CURL_MAX_TIME=8   # generous: lean /jsz is fast, but a busy cluster can be slow

usage() {
    cat >&2 <<EOF
Usage: $SCRIPT_NAME --output PATH --duration SECONDS [--nats-nodes HOST:PORT[,...]]
                    [--poll-interval SECONDS]

Arguments:
  --output PATH            File path to write ndjson output.
  --duration SECONDS       How many seconds to poll.
  --nats-nodes HOST:PORT   Comma-separated monitoring endpoints (default: localhost:8222).
  --poll-interval SECONDS  Seconds between polls (default: 5; use 1 for snapshot sweeps).

Output format (ndjson):
  {"t_unix_ns": <int>, "node": "<host:port>", "endpoint": "jsz|varz", "body": {...}}

Example:
  $SCRIPT_NAME --output /tmp/jsz.raw --duration 600 --poll-interval 1 \\
    --nats-nodes localhost:8222
EOF
    exit 2
}

OUTPUT=""
DURATION=""
NATS_NODES_RAW="localhost:8222"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --output)         OUTPUT="$2";          shift 2 ;;
        --duration)       DURATION="$2";        shift 2 ;;
        --nats-nodes)     NATS_NODES_RAW="$2";  shift 2 ;;
        --poll-interval)  POLL_INTERVAL="$2";   shift 2 ;;
        -h|--help)        usage ;;
        *) echo "$SCRIPT_NAME: unknown argument: $1" >&2; usage ;;
    esac
done

[[ -z "$OUTPUT" ]]   && { echo "$SCRIPT_NAME: --output is required" >&2; usage; }
[[ -z "$DURATION" ]] && { echo "$SCRIPT_NAME: --duration is required" >&2; usage; }

if ! [[ "$DURATION" =~ ^[0-9]+$ ]] || [[ "$DURATION" -le 0 ]]; then
    echo "$SCRIPT_NAME: --duration must be a positive integer (seconds), got: $DURATION" >&2
    exit 1
fi
if ! [[ "$POLL_INTERVAL" =~ ^[0-9]+$ ]] || [[ "$POLL_INTERVAL" -le 0 ]]; then
    echo "$SCRIPT_NAME: --poll-interval must be a positive integer (seconds), got: $POLL_INTERVAL" >&2
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
jsz_ok=0      # successful /jsz polls
jsz_fail=0    # failed /jsz polls (logged, skipped)

while [[ $(date +%s) -lt $end ]]; do
    ts_ns=$(date +%s%N)
    for node in "${NATS_NODES[@]}"; do
        for endpoint in jsz varz; do
            case "$endpoint" in
                # NOTE: streams=true only — NO consumers=true (see header). The
                # meta_cluster snapshot stats + top-level counts come back without it.
                jsz)  url="http://${node}/jsz?streams=true" ;;
                varz) url="http://${node}/varz" ;;
            esac
            # Non-fatal: a failed/slow poll is logged and skipped, not aborted.
            if ! body=$(curl -sf --max-time "$CURL_MAX_TIME" "$url"); then
                echo "$SCRIPT_NAME: WARN curl failed for $url — skipping this poll" >&2
                [[ "$endpoint" == jsz ]] && jsz_fail=$(( jsz_fail + 1 ))
                continue
            fi
            [[ "$endpoint" == jsz ]] && jsz_ok=$(( jsz_ok + 1 ))
            # Build JSON envelope via jq to safely embed the body.
            # -c (compact) produces one line per envelope (ndjson); the
            # aggregator parser expects exactly one JSON object per line.
            # jq failure (e.g. truncated body) is also non-fatal.
            if ! printf '%s\n' "$body" | \
                jq -c --arg node "$node" \
                   --arg ep   "$endpoint" \
                   --argjson t "$ts_ns" \
                   '{t_unix_ns: $t, node: $node, endpoint: $ep, body: .}' \
                >> "$OUTPUT"; then
                echo "$SCRIPT_NAME: WARN jq failed to encode $endpoint body — skipping" >&2
            fi
        done
    done
    sleep "$POLL_INTERVAL"
done

# Valid as long as at least one /jsz poll landed. Report the ratio so a thin
# capture stays visible in the campaign log.
echo "$SCRIPT_NAME: jsz polls: ${jsz_ok} ok, ${jsz_fail} failed" >&2
if [[ "$jsz_ok" -eq 0 ]]; then
    echo "$SCRIPT_NAME: no successful /jsz poll over ${DURATION}s — capture invalid" >&2
    exit 1
fi
exit 0
