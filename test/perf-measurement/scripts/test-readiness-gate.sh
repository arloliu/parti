#!/usr/bin/env bash
# test-readiness-gate.sh — Bash-level smoke test for the Item 1 fix:
# run-matrix.sh's wait_for_ready capture-window readiness gate
# (scripts/_capture_lib.sh).
#
# Exercises wait_for_ready against a real HTTP listener (a tiny Python
# stand-in for cmd/harness/ready.go's /ready endpoint — see ready_test.go
# for the Go-side unit tests of the actual handler) and real background
# processes standing in for the harness PID, so the three outcomes the
# gate must distinguish are proven end-to-end without needing the full
# docker rig:
#
#   1. Cluster becomes ready before the timeout -> gate returns 0 promptly.
#   2. The "harness" process exits before ever becoming ready -> gate
#      returns 1 promptly (does not wait out the full timeout).
#   3. The cluster never becomes ready -> gate returns 1 once the timeout
#      elapses (bounded wait, loud failure — never silently proceeds).
#
# Run with: bash test/perf-measurement/scripts/test-readiness-gate.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=_capture_lib.sh
source "${SCRIPT_DIR}/_capture_lib.sh"

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; (( PASS++ )) || true; }
fail() { echo "  FAIL: $1" >&2; (( FAIL++ )) || true; }

MOCK_PORT=17061
MOCK_PID=""

# start_mock_ready STATUS_CODE — serve STATUS_CODE for every GET on
# MOCK_PORT until stop_mock_ready is called.
start_mock_ready() {
    local status="$1"
    python3 -c '
import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(int(sys.argv[2]))
        self.end_headers()
    def log_message(self, *a):
        pass
http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
' "$MOCK_PORT" "$status" &
    MOCK_PID=$!
    # Give the listener a moment to bind before the first poll.
    sleep 0.3
}

stop_mock_ready() {
    if [[ -n "$MOCK_PID" ]]; then
        kill "$MOCK_PID" 2>/dev/null || true
        wait "$MOCK_PID" 2>/dev/null || true
        MOCK_PID=""
    fi
}

cleanup() {
    stop_mock_ready
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Test 1 — cluster ready immediately: wait_for_ready must return 0 without
# waiting out the timeout.
# ---------------------------------------------------------------------------
echo "=== Test 1: ready immediately -> returns 0 promptly ==="
start_mock_ready 200
sleep 30 &
harness_pid=$!

start_ts=$(date +%s)
if wait_for_ready "127.0.0.1:${MOCK_PORT}" 20 "$harness_pid"; then
    pass "wait_for_ready returned 0 when /ready reports 200"
else
    fail "wait_for_ready returned non-zero when /ready reports 200"
fi
elapsed=$(( $(date +%s) - start_ts ))
if [[ "$elapsed" -le 5 ]]; then
    pass "returned promptly (${elapsed}s), did not wait out the 20s timeout"
else
    fail "took ${elapsed}s to notice an already-ready /ready endpoint"
fi

kill "$harness_pid" 2>/dev/null || true
wait "$harness_pid" 2>/dev/null || true
stop_mock_ready

# ---------------------------------------------------------------------------
# Test 2 — harness process exits before becoming ready: wait_for_ready
# must return 1 as soon as it notices, not wait out the full timeout.
# No mock server is started at all (connection refused on every poll),
# standing in for a harness that died before ever starting its listener.
# ---------------------------------------------------------------------------
echo ""
echo "=== Test 2: harness exits early -> returns 1 promptly ==="
( sleep 0.5 ) &
harness_pid=$!

start_ts=$(date +%s)
if ! wait_for_ready "127.0.0.1:${MOCK_PORT}" 30 "$harness_pid" 2>/tmp/readiness-gate-test2.stderr; then
    pass "wait_for_ready returned non-zero when the harness process died"
else
    fail "wait_for_ready returned 0 despite the harness process dying"
fi
elapsed=$(( $(date +%s) - start_ts ))
if [[ "$elapsed" -le 10 ]]; then
    pass "returned promptly (${elapsed}s), did not wait out the 30s timeout"
else
    fail "took ${elapsed}s to notice the harness process had died"
fi
if grep -q "exited before signaling readiness" /tmp/readiness-gate-test2.stderr; then
    pass "stderr names the harness-exited cause"
else
    fail "stderr did not name the harness-exited cause: $(cat /tmp/readiness-gate-test2.stderr)"
fi
rm -f /tmp/readiness-gate-test2.stderr

# ---------------------------------------------------------------------------
# Test 3 — cluster never becomes ready: wait_for_ready must return 1 once
# the bounded timeout elapses, never silently returning success.
# ---------------------------------------------------------------------------
echo ""
echo "=== Test 3: never ready -> bounded timeout, returns 1 ==="
start_mock_ready 503
sleep 30 &
harness_pid=$!

start_ts=$(date +%s)
if ! wait_for_ready "127.0.0.1:${MOCK_PORT}" 5 "$harness_pid" 2>/tmp/readiness-gate-test3.stderr; then
    pass "wait_for_ready returned non-zero after the timeout elapsed"
else
    fail "wait_for_ready returned 0 despite /ready never reporting 200"
fi
elapsed=$(( $(date +%s) - start_ts ))
if [[ "$elapsed" -ge 4 ]]; then
    pass "waited out the bounded timeout (${elapsed}s >= 4s) rather than giving up early"
else
    fail "returned before the timeout elapsed (${elapsed}s), gate is not bounded correctly"
fi
if grep -q "timed out after 5s" /tmp/readiness-gate-test3.stderr; then
    pass "stderr names the timeout cause"
else
    fail "stderr did not name the timeout cause: $(cat /tmp/readiness-gate-test3.stderr)"
fi
rm -f /tmp/readiness-gate-test3.stderr

kill "$harness_pid" 2>/dev/null || true
wait "$harness_pid" 2>/dev/null || true
stop_mock_ready

# ---------------------------------------------------------------------------
# Summary.
# ---------------------------------------------------------------------------
echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed ==="
if [[ "$FAIL" -gt 0 ]]; then
    exit 1
fi
