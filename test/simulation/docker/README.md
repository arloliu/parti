# Parti Simulation - Docker Observability Stack

This directory contains Docker Compose configuration for running the full observability stack: NATS, Prometheus, and Grafana.

## Quick Start

### 1. Start the Stack

```bash
cd docker
docker-compose up -d
```

This will start:
- **NATS Server** (ports 4222, 8222) - JetStream message broker
- **Prometheus** (port 9090) - Metrics collection
- **Grafana** (port 3000) - Visualization dashboards

### 2. Run the Simulation

```bash
cd ..
./bin/simulation -config configs/realistic.yaml
```

Make sure your config has metrics enabled:
```yaml
metrics:
  prometheus:
    enabled: true
    port: 9091  # Must match Prometheus scrape config
```

### 3. Access Dashboards

- **Grafana**: http://localhost:3000
  - Username: `admin`
  - Password: `admin`
  - Pre-configured dashboard: "Parti Simulation Dashboard"

- **Prometheus**: http://localhost:9090
  - Query metrics directly
  - Check targets: http://localhost:9090/targets

- **NATS Monitoring**: http://localhost:8222
  - Server info: http://localhost:8222/varz
  - JetStream info: http://localhost:8222/jsz

## Architecture

```
┌──────────────┐         ┌──────────────┐
│  Simulation  │────────>│  Prometheus  │
│  (host:9091) │  :15s   │  (port 9090) │
└──────────────┘         └──────┬───────┘
                                │
                                │
┌──────────────┐         ┌──────▼───────┐
│     NATS     │────────>│   Grafana    │
│  (port 4222) │         │  (port 3000) │
└──────────────┘         └──────────────┘
```

## Configuration Files

### Prometheus (`prometheus/prometheus.yml`)
Defines scrape configurations:
- Simulation metrics from `host.docker.internal:9091`
- NATS metrics from `nats:8222`
- Scrape interval: 15 seconds

### Grafana Provisioning
- **Datasources**: Auto-configured Prometheus connection
- **Dashboards**: Pre-loaded "Parti Simulation Dashboard"

### Dashboard Panels
1. **Message Throughput** - Sent vs Received (msg/s)
2. **Active Workers** - Current worker count
3. **Message Gaps** - Total gaps detected (should be 0)
4. **Duplicate Messages** - Total duplicates (should be 0)
5. **System Resources** - Goroutines and memory usage
6. **Chaos Events** - Timeline of chaos engineering events
7. **Partitions Per Worker** - Distribution histogram
8. **Message Processing Latency** - p50, p95, p99
9. **Rebalancing Duration** - Time taken for rebalancing

## Usage Scenarios

### Development Testing
```bash
# Start stack
docker-compose up -d

# Run 2-minute test
./bin/simulation -config configs/quick-test.yaml

# View in Grafana
open http://localhost:3000
```

### Long-Running Stress Test
```bash
# Start stack
docker-compose up -d

# Run 12-hour stress test
./bin/simulation -config configs/stress.yaml

# Monitor in real-time
# Grafana will show:
# - Message throughput trends
# - Worker scaling events
# - Chaos impact on system
# - Performance degradation (if any)
```

### Extreme Load Test
```bash
# Start stack with more resources
docker-compose up -d

# Run extreme config (1500 partitions, high rate)
./bin/simulation -config configs/extreme.yaml

# Watch for:
# - Memory pressure
# - Rebalancing frequency
# - Processing latency increases
```

## Stopping the Stack

```bash
# Stop services
docker-compose down

# Stop and remove volumes (clean slate)
docker-compose down -v
```

## Troubleshooting

### Simulation Metrics Not Showing
1. Check simulation is exposing metrics: `curl http://localhost:9091/metrics`
2. Check Prometheus targets: http://localhost:9090/targets
3. Verify `host.docker.internal` resolves (Linux may need `--add-host`)

### Grafana Dashboard Empty
1. Wait 15-30 seconds for first scrape
2. Check Prometheus has data: http://localhost:9090/graph
3. Verify time range in Grafana (default: last 15 minutes)

### NATS Connection Issues
If simulation can't connect to NATS in Docker:
```yaml
nats:
  mode: external  # Use Docker NATS instead of embedded
  url: "nats://localhost:4222"
```

## Performance Tips

### For High-Throughput Tests
1. Increase Prometheus retention:
   ```yaml
   command:
     - '--storage.tsdb.retention.time=30d'
   ```

2. Adjust scrape interval for less overhead:
   ```yaml
   global:
     scrape_interval: 30s  # Instead of 15s
   ```

3. Use external NATS instead of embedded for better isolation

### For Long-Running Tests
1. Enable checkpointing in simulation config:
   ```yaml
   checkpoint:
     enabled: true
     interval: 30m
     path: "./checkpoints"
   ```

2. Monitor disk space for Prometheus data:
   ```bash
   docker exec simulation-prometheus du -sh /prometheus
   ```

## Customization

### Adding Custom Metrics
1. Edit `prometheus/prometheus.yml` to add new targets
2. Reload Prometheus: `docker-compose restart prometheus`

### Creating Custom Dashboards
1. Design in Grafana UI
2. Export JSON: Settings → JSON Model
3. Save to `grafana/dashboards/`
4. Restart Grafana: `docker-compose restart grafana`

## Advanced Features

### Alerting
Add alerting rules to `prometheus/alerts.yml`:
```yaml
groups:
  - name: simulation
    rules:
      - alert: MessageGapsDetected
        expr: simulation_message_gaps_total > 0
        annotations:
          summary: "Message gaps detected in simulation"
```

### Multi-Simulation Monitoring
Run multiple simulations on different ports:
```bash
# Simulation 1
./bin/simulation -config configs/stress.yaml &  # Port 9091

# Simulation 2 (modify config to use port 9092)
./bin/simulation -config configs/extreme.yaml &  # Port 9092
```

Update `prometheus.yml` to scrape both.

## Resources

- **Grafana Docs**: https://grafana.com/docs/
- **Prometheus Docs**: https://prometheus.io/docs/
- **NATS Docs**: https://docs.nats.io/

## Support

For issues or questions:
1. Check simulation logs: `docker-compose logs simulation`
2. Check Prometheus logs: `docker-compose logs prometheus`
3. Check Grafana logs: `docker-compose logs grafana`
