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
# OPTIONAL detail arm (--jsz-detail, off by default): per-consumer/raft detail
# (`consumers=true&raft=true`) is the same expensive query the scaling note above
# avoids, so when enabled it is fetched ONLY from the JetStream meta-leader
# (never every node) and ONLY on a slow, independent cadence
# (--jsz-detail-interval, floor 30s — validated below). The leader is resolved
# each lean tick from the /jsz `meta_cluster.leader` server name against the
# `server_name` field of the --nats-nodes' /varz responses already being polled
# that tick (no extra curl calls); if the leader can't be matched to a polled
# node (e.g. it isn't one of --nats-nodes), the detail poll is skipped with a
# WARN — never fatal. Detail runs as a background subshell so a slow/blocked
# detail curl can never delay or skip a lean poll tick, and it writes to a
# SEPARATE file (`<OUTPUT>.detail`, not OUTPUT) — a large detail body written
# concurrently with the lean loop could otherwise interleave partial writes and
# corrupt an ndjson line in the primary capture. Detail lines carry endpoint
# "jsz-detail" for self-description even though they never reach OUTPUT.
#
# Reachability requirement: this generalizes correctly only for nodes whose
# monitoring port is reachable from wherever this script runs — meta-leadership
# can land on any --nats-nodes entry, not just the first. run-matrix.sh's
# NATS_MONITOR_R3/R5 pass every cluster node's host-mapped monitoring port
# (see docker/docker-compose.yaml: nats-1:8222, nats-2:8223, nats-3:8224,
# nats-4:8225, nats-5:8226) for exactly this reason. Passing a --nats-nodes
# list that omits a node means detail silently goes missing (WARN, not fatal)
# whenever leadership is on the omitted node — check the WARN rate in the
# capture log before trusting a --jsz-detail run's coverage.
#
# Output format (one JSON object per line):
#   {"t_unix_ns": <unix_ns>, "node": "<host:port>", "endpoint": "jsz|varz", "body": {...}}
# With --jsz-detail, <OUTPUT>.detail additionally receives:
#   {"t_unix_ns": <unix_ns>, "node": "<host:port>", "endpoint": "jsz-detail", "body": {...}}
#
# Usage:
#   capture-jsz.sh --output PATH --duration SECONDS [--nats-nodes HOST:PORT[,...]]
#                  [--poll-interval SECONDS] [--jsz-detail]
#                  [--jsz-detail-interval SECONDS]
#
# Requirements: bash 4+, curl, jq.
set -euo pipefail

SCRIPT_NAME="$(basename "$0")"
POLL_INTERVAL=5
CURL_MAX_TIME=8              # generous: lean /jsz is fast, but a busy cluster can be slow
JSZ_DETAIL=0
JSZ_DETAIL_INTERVAL=30       # default; validated against the floor below
JSZ_DETAIL_MIN_INTERVAL=30   # floor — per-consumer/raft detail is expensive at scale
CURL_DETAIL_MAX_TIME=25      # generous, but < JSZ_DETAIL_MIN_INTERVAL by design

usage() {
    cat >&2 <<EOF
Usage: $SCRIPT_NAME --output PATH --duration SECONDS [--nats-nodes HOST:PORT[,...]]
                    [--poll-interval SECONDS] [--jsz-detail]
                    [--jsz-detail-interval SECONDS]

Arguments:
  --output PATH                  File path to write ndjson output.
  --duration SECONDS             How many seconds to poll.
  --nats-nodes HOST:PORT         Comma-separated monitoring endpoints (default: localhost:8222).
  --poll-interval SECONDS        Seconds between lean polls (default: 5; use 1 for snapshot sweeps).
  --jsz-detail                   Also poll consumers=true&raft=true /jsz detail from the
                                  meta-leader node only, on a slower cadence. Off by default.
                                  Written to <OUTPUT>.detail, not OUTPUT.
  --jsz-detail-interval SECONDS  Seconds between detail polls (default: 30; floor: 30).

Output format (ndjson):
  {"t_unix_ns": <int>, "node": "<host:port>", "endpoint": "jsz|varz", "body": {...}}
With --jsz-detail, <OUTPUT>.detail additionally receives:
  {"t_unix_ns": <int>, "node": "<host:port>", "endpoint": "jsz-detail", "body": {...}}

Example:
  $SCRIPT_NAME --output /tmp/jsz.raw --duration 600 --poll-interval 1 \\
    --nats-nodes localhost:8222
  $SCRIPT_NAME --output /tmp/jsz.raw --duration 600 --nats-nodes n1:8222,n2:8222,n3:8222 \\
    --jsz-detail --jsz-detail-interval 60
EOF
    exit 2
}

OUTPUT=""
DURATION=""
NATS_NODES_RAW="localhost:8222"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --output)               OUTPUT="$2";               shift 2 ;;
        --duration)             DURATION="$2";             shift 2 ;;
        --nats-nodes)           NATS_NODES_RAW="$2";       shift 2 ;;
        --poll-interval)        POLL_INTERVAL="$2";        shift 2 ;;
        --jsz-detail)           JSZ_DETAIL=1;               shift 1 ;;
        --jsz-detail-interval)  JSZ_DETAIL_INTERVAL="$2";   shift 2 ;;
        -h|--help)              usage ;;
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
if ! [[ "$JSZ_DETAIL_INTERVAL" =~ ^[0-9]+$ ]] || [[ "$JSZ_DETAIL_INTERVAL" -le 0 ]]; then
    echo "$SCRIPT_NAME: --jsz-detail-interval must be a positive integer (seconds), got: $JSZ_DETAIL_INTERVAL" >&2
    exit 1
fi
if [[ "$JSZ_DETAIL_INTERVAL" -lt "$JSZ_DETAIL_MIN_INTERVAL" ]]; then
    echo "$SCRIPT_NAME: --jsz-detail-interval must be >= ${JSZ_DETAIL_MIN_INTERVAL}s (floor; consumers=true&raft=true detail is expensive at scale), got: $JSZ_DETAIL_INTERVAL" >&2
    exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
    echo "$SCRIPT_NAME: jq is required but not found in PATH." >&2
    exit 1
fi

IFS=',' read -ra NATS_NODES <<< "$NATS_NODES_RAW"

OUTPUT_DETAIL="${OUTPUT}.detail"
declare -A node_server_name   # this tick's node -> /varz server_name (jsz-detail only)
leader_name=""                 # this tick's meta_cluster.leader (jsz-detail only)
DETAIL_PID=""
LAST_DETAIL_TS=0

# run_jsz_detail NODE — fetch consumers=true&raft=true /jsz detail from NODE
# and append one envelope line to OUTPUT_DETAIL. Invoked in the background by
# maybe_run_jsz_detail; a curl or jq failure here is logged and non-fatal.
run_jsz_detail() {
    local node="$1"
    local ts_ns url body
    ts_ns=$(date +%s%N)
    url="http://${node}/jsz?consumers=true&raft=true"
    if ! body=$(curl -sf --max-time "$CURL_DETAIL_MAX_TIME" "$url"); then
        echo "$SCRIPT_NAME: WARN jsz-detail curl failed for $url — skipping this detail poll" >&2
        return 0
    fi
    if ! printf '%s\n' "$body" | \
        jq -c --arg node "$node" \
           --arg ep   "jsz-detail" \
           --argjson t "$ts_ns" \
           '{t_unix_ns: $t, node: $node, endpoint: $ep, body: .}' \
        >> "$OUTPUT_DETAIL"; then
        echo "$SCRIPT_NAME: WARN jq failed to encode jsz-detail body — skipping" >&2
    fi
}

# maybe_run_jsz_detail — dispatch one detail poll cycle in the background if
# --jsz-detail is enabled and the detail-interval floor has elapsed since the
# last attempt. Reads the module-level leader_name / node_server_name maps
# populated by the lean loop for the CURRENT tick. Resolution failures (no
# leader determined, or leader not among --nats-nodes) and overlap with a
# still-running previous detail poll are all logged and skipped, never fatal.
# LAST_DETAIL_TS advances on every attempt (including skips) so the WARN rate
# stays bounded to at most once per --jsz-detail-interval.
maybe_run_jsz_detail() {
    [[ "$JSZ_DETAIL" -eq 1 ]] || return 0
    local now
    now=$(date +%s)
    if (( now - LAST_DETAIL_TS < JSZ_DETAIL_INTERVAL )); then
        return 0
    fi
    LAST_DETAIL_TS="$now"

    if [[ -n "$DETAIL_PID" ]] && kill -0 "$DETAIL_PID" 2>/dev/null; then
        echo "$SCRIPT_NAME: WARN previous jsz-detail poll still running — skipping this detail cycle" >&2
        return 0
    fi
    # Reap a finished background job so it doesn't linger.
    if [[ -n "$DETAIL_PID" ]]; then
        wait "$DETAIL_PID" 2>/dev/null || true
    fi

    if [[ -z "$leader_name" ]]; then
        echo "$SCRIPT_NAME: WARN jsz-detail: could not determine meta leader this tick — skipping detail poll" >&2
        return 0
    fi

    local leader_node="" n
    for n in "${!node_server_name[@]}"; do
        if [[ "${node_server_name[$n]}" == "$leader_name" ]]; then
            leader_node="$n"
            break
        fi
    done
    if [[ -z "$leader_node" ]]; then
        echo "$SCRIPT_NAME: WARN jsz-detail: meta leader '$leader_name' not found among --nats-nodes — skipping detail poll" >&2
        return 0
    fi

    run_jsz_detail "$leader_node" &
    DETAIL_PID=$!
}

# Clean exit on SIGINT/SIGTERM — flush current state, best-effort stop any
# in-flight detail poll (it only writes OUTPUT_DETAIL, never OUTPUT), and exit.
trap 'if [[ -n "$DETAIL_PID" ]]; then kill "$DETAIL_PID" 2>/dev/null || true; fi; exit 0' INT TERM

mkdir -p "$(dirname "$OUTPUT")"

end=$(( $(date +%s) + DURATION ))
jsz_ok=0      # successful /jsz polls
jsz_fail=0    # failed /jsz polls (logged, skipped)

while [[ $(date +%s) -lt $end ]]; do
    ts_ns=$(date +%s%N)
    leader_name=""
    node_server_name=()
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
            # jsz-detail only: opportunistically learn the meta leader name and
            # this node's /varz server_name from the SAME bodies already
            # fetched above — no extra curl calls. jq failure here just
            # leaves leader/name resolution incomplete for this tick (never
            # fatal, see maybe_run_jsz_detail).
            if [[ "$JSZ_DETAIL" -eq 1 ]]; then
                if [[ "$endpoint" == jsz && -z "$leader_name" ]]; then
                    if ! leader_name=$(printf '%s' "$body" | jq -r '.meta_cluster.leader // empty'); then
                        leader_name=""
                    fi
                elif [[ "$endpoint" == varz ]]; then
                    if server_name=$(printf '%s' "$body" | jq -r '.server_name // empty'); then
                        [[ -n "$server_name" ]] && node_server_name["$node"]="$server_name"
                    fi
                fi
            fi
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

    maybe_run_jsz_detail

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
