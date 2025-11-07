#!/bin/bash
# Quick verification script for the simulation system
# This runs a 2-minute test and checks for basic correctness

set -e

echo "=========================================="
echo "Simulation Quick Verification Test"
echo "=========================================="
echo ""

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Build the simulation binary
echo "Building simulation binary..."
cd "$(dirname "$0")"
go build -o bin/simulation cmd/simulation/main.go
if [ $? -ne 0 ]; then
    echo -e "${RED}✗ Build failed${NC}"
    exit 1
fi
echo -e "${GREEN}✓ Build successful${NC}"
echo ""

# Clean up any previous runs
echo "Cleaning up previous test data..."
rm -f /tmp/simulation-test.log
killall -9 simulation 2>/dev/null || true
# Clean up NATS JetStream data directory
rm -rf /tmp/nats 2>/dev/null || true
sleep 2

# Run the simulation for 2 minutes
echo "Running simulation (2 minutes with chaos enabled)..."
echo "Expected behavior:"
echo "  - Messages sent and received"
echo "  - Chaos events every 30-45 seconds"
echo "  - Workers may crash and restart"
echo "  - Reports printed every 30 seconds"
echo ""

# Run with timeout and capture output
timeout 125s ./bin/simulation -config configs/quick-test.yaml > /tmp/simulation-test.log 2>&1 &
SIM_PID=$!

# Wait for simulation to complete
wait $SIM_PID 2>/dev/null || true

echo ""
echo "=========================================="
echo "Verification Results"
echo "=========================================="

# Parse the output
LOG="/tmp/simulation-test.log"

if [ ! -f "$LOG" ]; then
    echo -e "${RED}✗ Log file not found${NC}"
    exit 1
fi

# Show last 50 lines for debugging if needed
# tail -50 "$LOG"

# Check if simulation started
if grep -q "Starting simulation in all-in-one mode" "$LOG"; then
    echo -e "${GREEN}✓ Simulation started${NC}"
else
    echo -e "${RED}✗ Simulation failed to start${NC}"
    echo "Error output:"
    head -20 "$LOG"
    exit 1
fi

# Check if NATS embedded server started
if grep -qi "nats\|embedded" "$LOG" 2>/dev/null; then
    echo -e "${GREEN}✓ NATS server initialized${NC}"
else
    echo -e "${YELLOW}⚠ NATS status unclear${NC}"
fi

# Check if chaos controller started
if grep -q "Chaos controller started" "$LOG"; then
    echo -e "${GREEN}✓ Chaos controller started${NC}"
else
    echo -e "${YELLOW}⚠ Chaos controller not detected${NC}"
fi

# Check if chaos events were injected
CHAOS_COUNT=$(grep -c "\[Chaos\] Handling event" "$LOG" 2>/dev/null || echo "0")
CHAOS_COUNT=$(echo "$CHAOS_COUNT" | tr -d '\n\r' | xargs)
if [ -n "$CHAOS_COUNT" ] && [ "$CHAOS_COUNT" -gt "0" ] 2>/dev/null; then
    echo -e "${GREEN}✓ Chaos events injected: $CHAOS_COUNT events${NC}"
else
    echo -e "${YELLOW}⚠ No chaos events detected (may not have triggered in 2 min)${NC}"
fi

# Check for simulation reports
REPORT_COUNT=$(grep -c "Simulation Report" "$LOG" 2>/dev/null || echo "0")
REPORT_COUNT=$(echo "$REPORT_COUNT" | tr -d '\n\r' | xargs)
if [ -n "$REPORT_COUNT" ] && [ "$REPORT_COUNT" -gt "0" ] 2>/dev/null; then
    echo -e "${GREEN}✓ Reports generated: $REPORT_COUNT reports${NC}"
else
    echo -e "${RED}✗ No simulation reports found${NC}"
    echo "Last 30 lines of log:"
    tail -30 "$LOG"
    exit 1
fi

# Extract final report
FINAL_REPORT=$(grep -A 10 "Simulation Report" "$LOG" | tail -10)

if echo "$FINAL_REPORT" | grep -q "Total Sent:"; then
    SENT=$(echo "$FINAL_REPORT" | grep "Total Sent:" | awk '{print $NF}' | tr -d '\r')
    RECEIVED=$(echo "$FINAL_REPORT" | grep "Total Received:" | awk '{print $NF}' | tr -d '\r')
    GAPS=$(echo "$FINAL_REPORT" | grep "Gaps Detected:" | awk '{print $NF}' | tr -d '\r')
    DUPLICATES=$(echo "$FINAL_REPORT" | grep "Duplicates:" | awk '{print $NF}' | tr -d '\r')

    echo ""
    echo "Final Statistics:"
    echo "  Messages Sent:     ${SENT:-N/A}"
    echo "  Messages Received: ${RECEIVED:-N/A}"
    echo "  Gaps Detected:     ${GAPS:-N/A}"
    echo "  Duplicates:        ${DUPLICATES:-N/A}"
    echo ""

    # Verify correctness (only if we have valid numbers)
    if [ -n "$GAPS" ] && [ -n "$DUPLICATES" ] && [ "$GAPS" = "0" ] && [ "$DUPLICATES" = "0" ]; then
        echo -e "${GREEN}✓✓✓ SUCCESS: No message loss or duplication!${NC}"
        echo -e "${GREEN}    The simulation is working correctly!${NC}"
        EXIT_CODE=0
    elif [ -n "$GAPS" ] && [ -n "$DUPLICATES" ]; then
        echo -e "${RED}✗✗✗ FAILURE: Message gaps or duplicates detected${NC}"
        echo -e "${RED}    Gaps: $GAPS, Duplicates: $DUPLICATES${NC}"
        EXIT_CODE=1
    else
        echo -e "${YELLOW}⚠ Could not verify message correctness${NC}"
        EXIT_CODE=1
    fi

    # Check if messages were actually sent
    if [ -n "$SENT" ] && [ "$SENT" -gt "100" ]; then
        echo -e "${GREEN}✓ Sufficient messages sent ($SENT > 100)${NC}"
    elif [ -n "$SENT" ]; then
        echo -e "${YELLOW}⚠ Low message count ($SENT)${NC}"
    fi
else
    echo -e "${RED}✗ Could not parse final statistics${NC}"
    echo "Last 30 lines of log:"
    tail -30 "$LOG"
    EXIT_CODE=1
fi

echo ""
echo "=========================================="
echo "Test Complete"
echo "=========================================="
echo ""
echo "Log file saved to: /tmp/simulation-test.log"
echo "To view full log: cat /tmp/simulation-test.log"
echo ""

exit ${EXIT_CODE:-1}
