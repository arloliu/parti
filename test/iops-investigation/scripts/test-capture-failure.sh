#!/usr/bin/env bash
# test-capture-failure.sh — Smoke test for P0 fix: capture exit-status tracking.
#
# Verifies two invariants:
#   1. A capture that writes partial data then exits non-zero causes
#      verify_captures to fail (CAPTURE_RCS propagation).
#   2. The M1.0 synthetic manifest (status: ok) is never written when
#      capture validation fails.
#
# Run with: bash test/iops-investigation/scripts/test-capture-failure.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Source the capture helpers (stop_captures, verify_captures).
# shellcheck source=_capture_lib.sh
source "${SCRIPT_DIR}/_capture_lib.sh"

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; (( PASS++ )) || true; }
fail() { echo "  FAIL: $1" >&2; (( FAIL++ )) || true; }

# ---------------------------------------------------------------------------
# Test 1 — A background stub that writes one line then exits 1 is caught by
#           stop_captures + verify_captures.
# ---------------------------------------------------------------------------
echo "=== Test 1: partial-write + non-zero exit is caught ==="

run_dir="$(mktemp -d)"
trap 'rm -rf "$run_dir"' EXIT

CAPTURE_PIDS=()
CAPTURE_NAMES=()
CAPTURE_RCS=()

# Stub: write one line to cgroup_io.raw then exit 1 (simulates a
# capture script that partially writes output and then fails, e.g.
# capture-jsz.sh after a transient curl failure). The stub exits
# immediately so `wait $pid` in stop_captures sees rc=1 directly.
bash -c "echo 'partial data' > '${run_dir}/cgroup_io.raw'; exit 1" &
CAPTURE_PIDS+=($!)
CAPTURE_NAMES+=("stub-cgroup-io")

# Well-behaved stubs: write content then sleep, responding to SIGTERM
# with exit 0 via trap, just like the real capture scripts.
for f in iostat.raw jsz.raw node_exporter.prom; do
    bash -c "trap 'exit 0' INT TERM; printf 'ok\n' > '${run_dir}/${f}'; sleep 60" &
    CAPTURE_PIDS+=($!)
    CAPTURE_NAMES+=("stub-${f%.raw}")
done

# Give stubs time to write their output files before stop_captures kills them.
sleep 0.2

stop_captures

# CAPTURE_RCS[0] must be 1 (the stub exit code).
if [[ "${CAPTURE_RCS[0]:-0}" -eq 1 ]]; then
    pass "CAPTURE_RCS[0] == 1 (non-zero exit recorded)"
else
    fail "CAPTURE_RCS[0] expected 1, got '${CAPTURE_RCS[0]:-<empty>}'"
fi

# verify_captures must return 1.
if ! verify_captures "$run_dir" 2>/dev/null; then
    pass "verify_captures returned non-zero for failed capture"
else
    fail "verify_captures returned 0 but should have failed"
fi

# capture-failed.txt must exist.
if [[ -f "${run_dir}/capture-failed.txt" ]]; then
    pass "capture-failed.txt written"
else
    fail "capture-failed.txt missing"
fi

# No manifest.yaml must exist (synthetic manifest not written on failure).
if [[ ! -f "${run_dir}/manifest.yaml" ]]; then
    pass "no manifest.yaml after capture failure"
else
    fail "manifest.yaml exists but should not after capture failure"
fi

# ---------------------------------------------------------------------------
# Test 2 — verify_captures succeeds when all captures exit 0.
# ---------------------------------------------------------------------------
echo ""
echo "=== Test 2: all-clean captures pass verify ==="

run_dir2="$(mktemp -d)"
trap 'rm -rf "$run_dir" "$run_dir2"' EXIT

CAPTURE_PIDS=()
CAPTURE_NAMES=()
CAPTURE_RCS=()

for f in cgroup_io.raw iostat.raw jsz.raw node_exporter.prom; do
    bash -c "trap 'exit 0' INT TERM; printf 'ok\n' > '${run_dir2}/${f}'; sleep 60" &
    CAPTURE_PIDS+=($!)
    CAPTURE_NAMES+=("stub-${f%.raw}")
done

sleep 0.2
stop_captures

# All CAPTURE_RCS should be 0.
all_zero=true
for rc in "${CAPTURE_RCS[@]}"; do
    if [[ "$rc" -ne 0 ]]; then
        all_zero=false
        break
    fi
done
if [[ "$all_zero" == "true" ]]; then
    pass "all CAPTURE_RCS are 0"
else
    fail "unexpected non-zero in CAPTURE_RCS: ${CAPTURE_RCS[*]}"
fi

if verify_captures "$run_dir2" 2>/dev/null; then
    pass "verify_captures returned 0 for clean captures"
else
    fail "verify_captures failed for clean captures"
fi

# ---------------------------------------------------------------------------
# Summary.
# ---------------------------------------------------------------------------
echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed ==="
if [[ "$FAIL" -gt 0 ]]; then
    exit 1
fi
