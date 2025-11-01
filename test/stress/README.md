# Stress & Performance Tests

This directory contains stress tests and performance benchmarks for the parti library.

## Overview

Stress tests focus on:
- **Memory benchmarking** - Accurate memory consumption measurements
- **Scalability testing** - Worker scaling from 1-50+ workers
- **Performance baselines** - Reference metrics for regression detection
- **Long-duration testing** - Stability under sustained load

These tests are separated from functional integration tests because they:
- Run for longer durations (1-5 minutes per test)
- Require external NATS for accurate memory isolation
- Test performance characteristics rather than functional correctness
- Establish baselines for capacity planning

## Test Files

### `memory_benchmark_test.go`
Measures parti library memory consumption without NATS overhead contamination.

**Tests:**
- `TestMemoryBenchmark_IsolatedParti` - Measures memory across 6 configurations (1-50 workers)
- `TestMemoryBenchmark_CompareWithEmbedded` - Validates NATS overhead isolation
- `TestMemoryBenchmark_PerWorkerOverhead` - Calculates incremental per-worker memory cost
- `TestMemoryBenchmark_PerPartitionOverhead` - Measures partition count impact

**Key Features:**
- Uses external NATS server in separate process (see Phase 1 infrastructure)
- Binary caching for 250,000x speedup (first compile: 628ms, cached: 2.51µs)
- Accurate isolation: removes ~2.4 MB NATS overhead (~38% reduction)
- Measurements: Peak memory plateaus at ~5.5 MB for 10+ workers

### `scale_workers_test.go`
Tests worker scaling patterns and establishes performance baselines.

**Tests:**
- `TestScale_SmallCluster` - 1-10 workers with 100 partitions (30s load test)
- `TestScale_LargeCluster` - 10-50 workers with 500 partitions (60s load test)
- `TestScale_RapidScaling` - 1→5→10 workers in rapid succession (stress test)

**Validates:**
- Worker startup and stabilization time (~4s per worker)
- Memory efficiency with increasing worker count
- Goroutine count patterns
- Assignment calculation performance
- System stability under scale-up scenarios

## Running Tests

### Run All Stress Tests
```bash
# Run all stress tests (requires -tags=integration)
make test-stress

# Or directly with go test
go test -v -timeout 15m ./test/stress
```

### Run Specific Tests
```bash
# Memory benchmarks only
go test -v -timeout 10m ./test/stress -run "TestMemoryBenchmark"

# Scale tests only
go test -v -timeout 10m ./test/stress -run "TestScale"

# Single configuration
go test -v -timeout 3m ./test/stress -run "TestMemoryBenchmark_IsolatedParti/1w-100p"
```

### Quick Sanity Check
```bash
# Run shortest test to verify setup
go test -v -timeout 3m ./test/stress -run "TestMemoryBenchmark_IsolatedParti/1w-100p"
```

## Requirements

### External NATS Infrastructure
Memory benchmarks use external NATS server for process isolation:
- **Binary:** Compiled from `test/cmd/nats-server/main.go`
- **Cache Location:** `/tmp/parti-nats-server-<sha256-hash>`
- **Port Allocation:** Dynamic via `net.Listen("tcp", "127.0.0.1:0")`
- **Cleanup:** Automatic via `t.Cleanup()` deferred functions

See `test/testutil/external_nats.go` for implementation details.

### Time Requirements
- **Memory benchmarks:** 1-5 minutes per test (60s load + startup/shutdown)
- **Scale tests:** 30-60 seconds per test
- **Full suite:** ~15-20 minutes

Use `-timeout 15m` to avoid test timeouts.

## Test Design Philosophy

### Why Separate from Integration Tests?

1. **Duration:** Stress tests run 10-100x longer than functional tests
2. **Purpose:** Performance measurement vs functional correctness
3. **Infrastructure:** Requires external NATS for accurate memory isolation
4. **Execution:** Not suitable for CI fast-feedback loops
5. **Analysis:** Results require baseline comparison and trending

### Measurement Accuracy

**Problem:** Embedded NATS server contaminates memory measurements
- Embedded NATS: 6.45 MB / 129 goroutines
- External NATS: 4.01 MB / 76 goroutines
- **Contamination:** ~2.44 MB / ~53 goroutines (~38% overhead)

**Solution:** External NATS process isolation
- Separate OS process for NATS server
- Measurements capture only parti library consumption
- Binary caching eliminates compilation overhead
- Results are accurate and reproducible

See `docs/ISOLATED_MEMORY_BENCHMARKING.md` for complete design.

## Performance Baselines

### Memory Consumption (Isolated Measurements)

| Configuration | Workers | Partitions | Peak Memory | Per Worker |
|--------------|---------|------------|-------------|------------|
| 1w-100p | 1 | 100 | 4.93 MB | 4.93 MB |
| 5w-100p | 5 | 100 | 5.22 MB | 1.04 MB |
| 10w-100p | 10 | 100 | **5.44 MB** | 0.54 MB |
| 10w-500p | 10 | 500 | **5.44 MB** | 0.54 MB |
| 25w-500p | 25 | 500 | **5.44 MB** | 0.22 MB |
| 50w-500p | 50 | 500 | **5.44 MB** | 0.11 MB |

**Key Insight:** Memory plateaus at ~5.5 MB for 10+ workers (excellent horizontal scaling)

### Worker Startup Time
- **Average:** ~4s per worker to reach Stable state
- **Pattern:** Linear scaling (50 workers = ~200s total startup)
- **Variance:** ±0.2s typical variation

### Goroutine Count
- **10+ workers:** Stabilizes at ~470 goroutines
- **Per worker:** ~9-47 goroutines depending on configuration
- **Fixed overhead:** ~400 goroutines for core infrastructure

See `docs/PHASE2_COMPLETE.md` for complete analysis.

## Troubleshooting

### "Binary compilation failed"
```bash
# Clean cache and rebuild
go clean -testcache
rm /tmp/parti-nats-server-*
go test -v -timeout 3m ./test/stress -run "TestMemoryBenchmark_IsolatedParti/1w-100p"
```

### "Test timeout"
Increase timeout for slow machines:
```bash
go test -v -timeout 30m ./test/stress
```

### "Address already in use"
External NATS uses random ports, but race conditions can occur:
```bash
# Kill any orphaned processes
pkill -f parti-nats-server
```

### "Goroutine leak detected"
Expected behavior - goroutines cleaned by `t.Cleanup()` after measurements.
Check logs for "NOTE: X goroutines pending cleanup" messages.

## Related Documentation

- `docs/ISOLATED_MEMORY_BENCHMARKING.md` - Complete design specification
- `docs/MEMORY_BENCHMARK_QUICKSTART.md` - Quick reference guide
- `docs/PHASE1_COMPLETE.md` - External NATS infrastructure details
- `docs/PHASE2_COMPLETE.md` - Memory benchmark results and analysis
- `test/testutil/external_nats.go` - External NATS helper functions
- `test/cmd/nats-server/main.go` - Standalone NATS server binary

## CI/CD Integration

### GitHub Actions Example
```yaml
# Run stress tests on schedule (nightly)
name: Stress Tests
on:
  schedule:
    - cron: '0 2 * * *'  # 2 AM daily
  workflow_dispatch:

jobs:
  stress:
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-go@v4
        with:
          go-version: '1.25'
      - name: Run Stress Tests
        run: |
          go test -v -timeout 20m ./test/stress
```

### Performance Regression Detection
```bash
# Compare current run with baseline
go test -v -timeout 15m ./test/stress | tee current.log
diff baseline.log current.log

# Alert on >10% memory increase
# Alert on >50% duration increase
```

## Contributing

When adding new stress tests:

1. **Use `stress_test` package:** Maintain consistency with existing tests
2. **Require integration build tag:** `//go:build integration`
3. **Use external NATS:** Call `testutil.StartExternalNATS(t)` for memory tests
4. **Document baselines:** Include expected memory/duration in comments
5. **Add to README:** Update test list and baselines table
6. **Timeout appropriately:** Use `-timeout` values 2-3x expected duration
7. **Measure accurately:** Use `testutil.LoadGenerator` for consistent metrics
8. **Clean up resources:** Always use `t.Cleanup()` or `defer` for teardown

---

**Last Updated:** Phase 3 completion (test reorganization)
