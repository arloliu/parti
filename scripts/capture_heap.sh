#!/usr/bin/env bash
set -euo pipefail

# Simple helper to run the simulation, wait, capture a heap profile, then stop it.
# Usage:
#   scripts/capture_heap.sh CONFIG_PATH PORT OUT_FILE SLEEP_SECONDS
# Example:
#   scripts/capture_heap.sh test/simulation/configs/leak-3m-highcard.yaml 9092 /tmp/highcard_heap.pprof 22

if [ "$#" -ne 4 ]; then
  echo "Usage: $0 CONFIG_PATH PORT OUT_FILE SLEEP_SECONDS" >&2
  exit 1
fi

CONFIG=$1
PORT=$2
OUT_FILE=$3
SLEEP_SECS=$4

cd "$(dirname "$0")/.."

echo "Starting simulation with config=$CONFIG on port=$PORT" >&2
# Run via go run in background; we will later kill the compiled binary by pattern.
go run ./test/simulation/cmd/simulation -config "$CONFIG" &
GO_RUN_PID=$!
echo "go run PID: $GO_RUN_PID" >&2

# Give it time to start and generate load.
echo "Sleeping for $SLEEP_SECS seconds before capture..." >&2
sleep "$SLEEP_SECS"

# Capture heap; this blocks until profile is fully written.
echo "Capturing heap from http://localhost:$PORT/debug/pprof/heap -> $OUT_FILE" >&2
if ! curl -fsS "http://localhost:$PORT/debug/pprof/heap" > "$OUT_FILE"; then
  echo "Heap capture failed" >&2
else
  echo "Heap capture completed: $(ls -lh "$OUT_FILE")" >&2
fi

# Terminate the actual simulation binary; don't wait for graceful shutdown.
echo "Stopping simulation processes..." >&2
pkill -f 'test/simulation/cmd/simulation' || true

# Also stop the go run wrapper if still around (non-blocking, no wait).
kill "$GO_RUN_PID" 2>/dev/null || true

echo "Done." >&2
