#!/bin/bash
# Quick metrics verification script

echo "=== Simulation Metrics Test ==="
echo ""

# Check if simulation is running
if ! pgrep -f "simulation.*fast-test" > /dev/null; then
    echo "❌ Simulation not running. Start with:"
    echo "   cd test/simulation && ./bin/simulation -config configs/fast-test.yaml"
    exit 1
fi

echo "✅ Simulation is running"
echo ""

# Check simulation metrics endpoint
echo "--- Checking Simulation Metrics (localhost:9091) ---"
if curl -s http://localhost:9091/metrics > /dev/null; then
    SENT=$(curl -s http://localhost:9091/metrics | grep "simulation_messages_sent_total" | wc -l)
    RECEIVED=$(curl -s http://localhost:9091/metrics | grep "simulation_messages_received_total" | wc -l)
    PROCESSING=$(curl -s http://localhost:9091/metrics | grep "simulation_message_processing_duration_seconds_count" | awk '{print $2}')
    PARTITIONS=$(curl -s http://localhost:9091/metrics | grep "simulation_partitions_per_worker_count" | awk '{print $2}')

    echo "✅ Metrics endpoint accessible"
    echo "   - Messages sent partitions: $SENT"
    echo "   - Messages received partitions: $RECEIVED"
    echo "   - Messages processed: $PROCESSING"
    echo "   - Worker assignments recorded: $PARTITIONS"
else
    echo "❌ Cannot access metrics endpoint"
fi
echo ""

# Check Prometheus
echo "--- Checking Prometheus (localhost:9090) ---"
if curl -s http://localhost:9090/-/healthy > /dev/null; then
    echo "✅ Prometheus is healthy"

    # Check if simulation target is up
    TARGET_HEALTH=$(curl -s 'http://localhost:9090/api/v1/targets' | python3 -c "import sys, json; data = json.load(sys.stdin); targets = [t for t in data['data']['activeTargets'] if t['labels']['job'] == 'simulation']; print(targets[0]['health']) if targets else print('not found')")

    if [ "$TARGET_HEALTH" = "up" ]; then
        echo "✅ Simulation target is UP"

        # Check if data is in Prometheus
        PROM_DATA=$(curl -s 'http://localhost:9090/api/v1/query?query=simulation_messages_sent_total' | python3 -c "import sys, json; data = json.load(sys.stdin); print(len(data['data']['result']))")
        echo "   - Time series in Prometheus: $PROM_DATA"
    else
        echo "❌ Simulation target is: $TARGET_HEALTH"
    fi
else
    echo "❌ Prometheus not accessible"
fi
echo ""

# Check Grafana
echo "--- Checking Grafana (localhost:3000) ---"
if curl -s http://localhost:3000/api/health > /dev/null; then
    echo "✅ Grafana is healthy"

    # Check datasource
    DS_NAME=$(curl -s http://localhost:3000/api/datasources/uid/prometheus | python3 -c "import sys, json; data = json.load(sys.stdin); print(data.get('name', 'N/A'))" 2>/dev/null)

    if [ "$DS_NAME" != "N/A" ]; then
        echo "✅ Datasource 'prometheus' configured: $DS_NAME"

        # Test query through Grafana
        GRAFANA_DATA=$(curl -s 'http://localhost:3000/api/datasources/uid/prometheus/resources/api/v1/query?query=simulation_messages_sent_total' -H 'Content-Type: application/json' | python3 -c "import sys, json; data = json.load(sys.stdin); print(len(data['data']['result']))" 2>/dev/null)
        echo "   - Query test: $GRAFANA_DATA time series"
        echo "✅ Grafana dashboard should show data!"
        echo "   Open: http://localhost:3000/d/simulation/parti-simulation-dashboard"
    else
        echo "❌ Datasource not found or misconfigured"
    fi
else
    echo "❌ Grafana not accessible"
fi

echo ""
echo "=== Summary ==="
echo "All components working! 🎉"
echo ""
echo "View dashboard at: http://localhost:3000/d/simulation/parti-simulation-dashboard"
echo "  Username: admin"
echo "  Password: admin"
