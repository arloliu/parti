#!/usr/bin/env bash
# capture-iostat.sh — secondary host-level iostat capture (Phase 3b).
#
# Runs `iostat -x -d -t 1 <duration>` and writes its output to --output.
# The Go aggregator (Phase 3e) parses the timestamped sections from this
# file; no massaging of iostat output is done here.
#
# The file begins with a one-line header so the aggregator can
# sanity-check that it's looking at the right file format.
#
# Usage:
#   capture-iostat.sh --output PATH --duration SECONDS
#
# Requirements: bash 4+, iostat (sysstat package).
set -euo pipefail

SCRIPT_NAME="$(basename "$0")"

usage() {
    cat >&2 <<EOF
Usage: $SCRIPT_NAME --output PATH --duration SECONDS

Arguments:
  --output PATH       File path to write iostat output.
  --duration SECONDS  How many seconds to run iostat for.

Output format:
  First line: # iostat -x -d -t 1
  Remainder:  raw iostat output (timestamped sections, 1-second interval).

Example:
  $SCRIPT_NAME --output /tmp/iostat.raw --duration 600
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

if ! command -v iostat >/dev/null 2>&1; then
    echo "$SCRIPT_NAME: iostat not found; install the sysstat package." >&2
    exit 1
fi

trap 'exit 0' INT TERM

mkdir -p "$(dirname "$OUTPUT")"

{
    echo "# iostat -x -d -t 1"
    # duration+1 total blocks; the first block covers since-boot (expected by
    # Phase 3e which discards the first block per iostat convention).
    iostat -x -d -t 1 "$DURATION"
} > "$OUTPUT"
