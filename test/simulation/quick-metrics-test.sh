#!/bin/bash
# Quick test runner for metrics verification

set -e

echo "🚀 Starting Fast Metrics Test"
echo ""

# Check if Docker services are running
echo "Checking Docker services..."
if ! docker compose -f docker/docker-compose.yml ps | grep -q "Up"; then
    echo "Starting Docker stack..."
    cd docker && docker compose up -d && cd ..
    echo "Waiting for services to be ready..."
    sleep 10
else
    echo "✅ Docker services already running"
fi

# Kill any existing simulation
pkill -f "simulation -config" 2>/dev/null || true
sleep 2

# Build latest version
echo ""
echo "Building simulation..."
go build -o bin/simulation cmd/simulation/main.go

# Start fast test in background
echo ""
echo "Starting simulation with fast-test config..."
nohup ./bin/simulation -config configs/fast-test.yaml > /tmp/fast-test.log 2>&1 &
SIM_PID=$!
echo "Simulation PID: $SIM_PID"

# Wait for metrics to accumulate
echo ""
echo "Waiting 20 seconds for metrics to accumulate..."
for i in {20..1}; do
    echo -ne "  $i seconds remaining...\r"
    sleep 1
done
echo ""

# Verify metrics
echo ""
./verify-metrics.sh

# Show sample metrics
echo ""
echo "=== Sample Metrics ==="
echo ""
echo "Message rates (first 5 partitions):"
curl -s http://localhost:9091/metrics | grep "simulation_messages_sent_total" | head -5

echo ""
echo "Processing stats:"
curl -s http://localhost:9091/metrics | grep "simulation_message_processing_duration_seconds" | grep -E "(count|sum)"

echo ""
echo "System stats:"
curl -s http://localhost:9091/metrics | grep -E "simulation_(goroutines|memory)"

echo ""
echo "=== Test Complete ==="
echo ""
echo "Simulation is still running (PID: $SIM_PID)"
echo "To stop: kill $SIM_PID"
echo ""
echo "View logs: tail -f /tmp/fast-test.log"
echo "View dashboard: http://localhost:3000/d/simulation/parti-simulation-dashboard"
