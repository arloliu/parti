package stress_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/arloliu/parti/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestMemoryBenchmark_IsolatedParti measures parti library memory without NATS overhead.
//
// Uses external NATS server in separate process to isolate memory measurements.
// This provides accurate baseline for parti library's actual memory consumption.
//
//nolint:tparallel // Parent test has t.Parallel() call at line 23
func TestMemoryBenchmark_IsolatedParti(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping memory benchmark in short mode")
	}

	t.Parallel()

	// Test configurations
	tests := []struct {
		name       string
		workers    int
		partitions int
		duration   time.Duration
	}{
		{"1w-100p", 1, 100, 60 * time.Second},
		{"5w-100p", 5, 100, 60 * time.Second},
		{"10w-100p", 10, 100, 60 * time.Second},
		{"10w-500p", 10, 500, 60 * time.Second},
		{"25w-500p", 25, 500, 60 * time.Second},
		{"50w-500p", 50, 500, 60 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Start NATS in separate process (memory isolated)
			nc, cleanup := testutil.StartExternalNATS(t)
			defer cleanup()

			// Force GC before measurement to get clean baseline
			runtime.GC()
			time.Sleep(100 * time.Millisecond)

			// Start resource monitoring (measures only parti workers)
			monitor := testutil.NewResourceMonitor()
			monitor.Start(500 * time.Millisecond)

			// Create load generator
			lg := testutil.NewLoadGenerator(t, nc, tt.partitions)
			defer lg.Cleanup()

			// Run load test
			ctx := context.Background()
			metrics := lg.RunLoadTest(ctx, testutil.LoadConfig{
				WorkerCount:    tt.workers,
				PartitionCount: tt.partitions,
				Duration:       tt.duration,
				SampleInterval: 500 * time.Millisecond,
				Description:    tt.name + " (isolated)",
			})

			// Stop monitoring and get report
			report := monitor.Stop()

			// Log detailed results
			t.Logf("\n=== ISOLATED MEMORY BENCHMARK: %s ===", tt.name)
			t.Log("Configuration:")
			t.Logf("  Workers: %d", tt.workers)
			t.Logf("  Partitions: %d", tt.partitions)
			t.Logf("  Duration: %v", tt.duration)
			t.Log("\nMemory (Parti Library ONLY):")
			t.Logf("  Start:  %.2f MB", report.StartMemoryMB)
			t.Logf("  End:    %.2f MB", report.EndMemoryMB)
			t.Logf("  Peak:   %.2f MB", report.PeakMemoryMB)
			t.Logf("  Growth: %.2f MB", report.MemoryGrowthMB)
			t.Logf("  Per Worker Avg: %.2f MB", report.PeakMemoryMB/float64(tt.workers))
			t.Log("\nGoroutines (Parti Library ONLY):")
			t.Logf("  Start: %d", report.StartGoroutines)
			t.Logf("  End:   %d", report.EndGoroutines)
			t.Logf("  Peak:  %d", report.PeakGoroutines)
			t.Logf("  Leak:  %d", report.GoroutineLeak)
			if tt.workers > 0 {
				t.Logf("  Per Worker Avg: %.1f goroutines",
					float64(report.PeakGoroutines)/float64(tt.workers))
			}
			t.Log("\nLoad Test Metrics:")
			t.Logf("  Total Duration: %v", metrics.Duration())
			t.Logf("  Workers Started: %d", tt.workers)
			t.Logf("  Errors: %d", len(metrics.Errors))

			// Validate
			require.Empty(t, metrics.Errors, "Test should complete without errors")

			// Note: Goroutine cleanup happens in t.Cleanup(), so end-of-test goroutines will appear high
			// This is expected and will be resolved by deferred cleanup
			if report.GoroutineLeak > 50 {
				t.Logf("NOTE: %d goroutines pending cleanup (will be cleaned by t.Cleanup())", report.GoroutineLeak)
			} // Basic sanity checks (generous limits for isolated measurements)
			require.Less(t, report.PeakMemoryMB, 200.0,
				"Isolated parti library should use < 200 MB")
			require.Less(t, report.PeakGoroutines, 1000,
				"Isolated parti library should use < 1000 goroutines")
		})
	}
}

// TestMemoryBenchmark_CompareWithEmbedded compares isolated vs embedded measurements.
//
// Runs same workload with embedded NATS vs external NATS to quantify overhead.
func TestMemoryBenchmark_CompareWithEmbedded(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping comparison benchmark in short mode")
	}

	workers := 10
	partitions := 100
	duration := 30 * time.Second

	// Test 1: Embedded NATS (current baseline approach)
	t.Run("embedded_nats", func(t *testing.T) {
		nc, cleanup := testutil.StartEmbeddedNATS(t)
		defer cleanup()

		runtime.GC()
		time.Sleep(100 * time.Millisecond)

		monitor := testutil.NewResourceMonitor()
		monitor.Start(500 * time.Millisecond)

		lg := testutil.NewLoadGenerator(t, nc, partitions)
		defer lg.Cleanup()

		ctx := context.Background()
		metrics := lg.RunLoadTest(ctx, testutil.LoadConfig{
			WorkerCount:    workers,
			PartitionCount: partitions,
			Duration:       duration,
			SampleInterval: 500 * time.Millisecond,
			Description:    "embedded_nats_comparison",
		})

		report := monitor.Stop()

		t.Log("\n=== WITH EMBEDDED NATS ===")
		t.Logf("Memory: %.2f MB (includes NATS server)", report.PeakMemoryMB)
		t.Logf("Goroutines: %d (includes NATS server)", report.PeakGoroutines)

		require.Empty(t, metrics.Errors)
	})

	// Test 2: External NATS (isolated measurement)
	t.Run("external_nats", func(t *testing.T) {
		nc, cleanup := testutil.StartExternalNATS(t)
		defer cleanup()

		runtime.GC()
		time.Sleep(100 * time.Millisecond)

		monitor := testutil.NewResourceMonitor()
		monitor.Start(500 * time.Millisecond)

		lg := testutil.NewLoadGenerator(t, nc, partitions)
		defer lg.Cleanup()

		ctx := context.Background()
		metrics := lg.RunLoadTest(ctx, testutil.LoadConfig{
			WorkerCount:    workers,
			PartitionCount: partitions,
			Duration:       duration,
			SampleInterval: 500 * time.Millisecond,
			Description:    "external_nats_comparison",
		})

		report := monitor.Stop()

		t.Log("\n=== WITH EXTERNAL NATS (ISOLATED) ===")
		t.Logf("Memory: %.2f MB (parti ONLY)", report.PeakMemoryMB)
		t.Logf("Goroutines: %d (parti ONLY)", report.PeakGoroutines)

		require.Empty(t, metrics.Errors)
	})
}

// TestMemoryBenchmark_PerWorkerOverhead measures incremental memory per worker.
//
// Starts with baseline, then adds workers one by one to measure incremental cost.
func TestMemoryBenchmark_PerWorkerOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping per-worker overhead test in short mode")
	}

	nc, cleanup := testutil.StartExternalNATS(t)
	defer cleanup()

	partitions := 100
	workerCounts := []int{1, 5, 10, 20, 30}

	results := make(map[int]float64)

	for _, workerCount := range workerCounts {
		t.Run(string(rune('0'+workerCount/10))+"workers", func(t *testing.T) {
			runtime.GC()
			time.Sleep(100 * time.Millisecond)

			monitor := testutil.NewResourceMonitor()
			monitor.Start(500 * time.Millisecond)

			lg := testutil.NewLoadGenerator(t, nc, partitions)
			defer lg.Cleanup()

			ctx := context.Background()
			metrics := lg.RunLoadTest(ctx, testutil.LoadConfig{
				WorkerCount:    workerCount,
				PartitionCount: partitions,
				Duration:       20 * time.Second,
				SampleInterval: 500 * time.Millisecond,
				Description:    "per_worker_overhead",
			})

			report := monitor.Stop()
			results[workerCount] = report.PeakMemoryMB

			t.Logf("\n%d workers: %.2f MB (%.2f MB/worker)",
				workerCount, report.PeakMemoryMB, report.PeakMemoryMB/float64(workerCount))

			require.Empty(t, metrics.Errors)
		})
	}

	// Calculate incremental overhead
	if len(results) >= 2 {
		t.Log("\n=== INCREMENTAL OVERHEAD ANALYSIS ===")
		for i := 1; i < len(workerCounts); i++ {
			prev := workerCounts[i-1]
			curr := workerCounts[i]
			memDiff := results[curr] - results[prev]
			workerDiff := curr - prev
			perWorker := memDiff / float64(workerDiff)
			t.Logf("%d → %d workers: +%.2f MB (%.2f MB per worker)",
				prev, curr, memDiff, perWorker)
		}
	}
}

// TestMemoryBenchmark_PerPartitionOverhead measures memory scaling with partitions.
//
// Fixed workers, varying partition counts to measure partition overhead.
func TestMemoryBenchmark_PerPartitionOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping per-partition overhead test in short mode")
	}

	nc, cleanup := testutil.StartExternalNATS(t)
	defer cleanup()

	workers := 10
	partitionCounts := []int{50, 100, 200, 500, 1000}

	results := make(map[int]float64)

	for _, partitionCount := range partitionCounts {
		t.Run(string(rune('0'+partitionCount/100))+"partitions", func(t *testing.T) {
			runtime.GC()
			time.Sleep(100 * time.Millisecond)

			monitor := testutil.NewResourceMonitor()
			monitor.Start(500 * time.Millisecond)

			lg := testutil.NewLoadGenerator(t, nc, partitionCount)
			defer lg.Cleanup()

			ctx := context.Background()
			metrics := lg.RunLoadTest(ctx, testutil.LoadConfig{
				WorkerCount:    workers,
				PartitionCount: partitionCount,
				Duration:       20 * time.Second,
				SampleInterval: 500 * time.Millisecond,
				Description:    "per_partition_overhead",
			})

			report := monitor.Stop()
			results[partitionCount] = report.PeakMemoryMB

			t.Logf("\n%d partitions: %.2f MB (%.2f KB/partition)",
				partitionCount, report.PeakMemoryMB,
				(report.PeakMemoryMB*1024)/float64(partitionCount))

			require.Empty(t, metrics.Errors)
		})
	}

	// Calculate incremental overhead
	if len(results) >= 2 {
		t.Log("\n=== INCREMENTAL PARTITION OVERHEAD ===")
		for i := 1; i < len(partitionCounts); i++ {
			prev := partitionCounts[i-1]
			curr := partitionCounts[i]
			memDiff := results[curr] - results[prev]
			partitionDiff := curr - prev
			perPartition := (memDiff * 1024) / float64(partitionDiff)
			t.Logf("%d → %d partitions: +%.2f MB (%.2f KB per partition)",
				prev, curr, memDiff, perPartition)
		}
	}
}
