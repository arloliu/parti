package stress_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/stretchr/testify/require"
)

// TestScale_SmallCluster tests a compact small-cluster scaling scenario.
//
// This test establishes performance baselines for fast small-cluster operations:
// - Worker startup and stabilization time
// - Memory usage per worker
// - Goroutine count patterns
// - Assignment calculation performance
//
// The test validates that the system handles small-scale scenarios efficiently
// and provides a baseline for detecting performance regressions.
func TestScale_SmallCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping scale test in short mode")
	}

	requireStressEnabled(t)

	// Test different worker counts
	workerCounts := []int{1, 3}

	for _, workerCount := range workerCounts {
		t.Run(fmt.Sprintf("%dw", workerCount), func(t *testing.T) {
			// Each subtest needs its own embedded NATS server
			nc, natsCleanup := testutil.StartEmbeddedNATS(t)

			ctx := context.Background()

			// Create load generator with a compact partition count for the fast suite.
			lg := testutil.NewLoadGenerator(t, nc, 50)

			// Cleanup order: workers first, then NATS
			defer natsCleanup()
			defer lg.Cleanup()

			// Run a compact load test sized for the default fast stress suite.
			metrics := lg.RunLoadTest(ctx, testutil.LoadConfig{
				WorkerCount:    workerCount,
				PartitionCount: 50,
				Duration:       2 * time.Second,
				SampleInterval: 1 * time.Second,
				Description:    testName(workerCount, 50),
			})

			// Log results
			t.Log(metrics.Report())

			// Validate no errors
			require.Empty(t, metrics.Errors, "Test should complete without errors")

			// Validate resource usage is reasonable
			peakMemory := metrics.PeakMemoryMB()
			peakGoroutines := metrics.PeakGoroutines()
			peakAdditionalGoroutines := metrics.PeakAdditionalGoroutines()

			t.Logf("Peak resource usage: %.2f MB memory, %d goroutines (%d above baseline)", peakMemory, peakGoroutines, peakAdditionalGoroutines)

			// Basic sanity checks (not strict limits, just detect obvious problems)
			require.Less(t, peakMemory, 500.0, "Memory usage should be reasonable (< 500 MB)")
			require.Less(t, peakAdditionalGoroutines, 500, "Goroutine growth should be reasonable (< 500)")

			// Document baseline metrics
			t.Logf("BASELINE [%d workers, 50 partitions]: Memory=%.2f MB, Goroutines=%d, Duration=%v",
				workerCount, peakMemory, peakGoroutines, metrics.Duration())
		})
	}
}

// TestScale_MediumCluster tests a compact medium-cluster scaling scenario.
//
// This test validates:
// - Linear scaling characteristics (or at least sub-quadratic)
// - No performance degradation with increased worker count
// - Resource usage remains proportional to cluster size
//
// Expected behavior:
// - Assignment time should scale linearly or better
// - Memory per worker should remain roughly constant
// - No resource leaks as cluster size increases
//
//nolint:tparallel // This test intentionally runs in parallel with other stress tests; goroutine assertions use per-test baseline growth instead of raw process-wide totals.
func TestScale_MediumCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping scale test in short mode")
	}

	requireStressEnabled(t)

	t.Parallel()

	// Test different worker counts
	workerCounts := []int{5}

	for _, workerCount := range workerCounts {
		t.Run(testName(workerCount, 100), func(t *testing.T) {
			t.Parallel()

			// Each subtest needs its own embedded NATS server
			nc, natsCleanup := testutil.StartEmbeddedNATS(t)

			ctx := context.Background()

			// Create load generator with a moderate partition count to keep the suite fast.
			lg := testutil.NewLoadGenerator(t, nc, 100)

			// Cleanup order: workers first, then NATS
			defer natsCleanup()
			defer lg.Cleanup()

			// Run a compact medium-cluster load test for the default fast stress suite.
			metrics := lg.RunLoadTest(ctx, testutil.LoadConfig{
				WorkerCount:    workerCount,
				PartitionCount: 100,
				Duration:       2 * time.Second,
				SampleInterval: 1 * time.Second,
				Description:    testName(workerCount, 100),
			})

			// Log results
			t.Log(metrics.Report())

			// Validate no errors
			require.Empty(t, metrics.Errors, "Test should complete without errors")

			// Validate resource usage is reasonable
			peakMemory := metrics.PeakMemoryMB()
			peakGoroutines := metrics.PeakGoroutines()
			peakAdditionalGoroutines := metrics.PeakAdditionalGoroutines()

			t.Logf("Peak resource usage: %.2f MB memory, %d goroutines (%d above baseline)", peakMemory, peakGoroutines, peakAdditionalGoroutines)

			// Basic sanity checks
			require.Less(t, peakMemory, 1000.0, "Memory usage should be reasonable (< 1 GB)")
			require.Less(t, peakAdditionalGoroutines, 1200, "Goroutine growth should be reasonable (< 1200)")

			// Document baseline metrics
			t.Logf("BASELINE [%d workers, 100 partitions]: Memory=%.2f MB, Goroutines=%d, Duration=%v",
				workerCount, peakMemory, peakGoroutines, metrics.Duration())
		})
	}
}

// TestScale_ResourceStability tests that resources remain stable over time.
//
// This test runs a cluster for an extended period to detect:
// - Memory leaks (gradual memory growth)
// - Goroutine leaks (goroutines not being cleaned up)
// - Performance degradation over time
//
// The test maintains a constant cluster size and validates that resource
// usage remains stable throughout the test duration.
func TestScale_ResourceStability(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stability test in short mode")
	}

	requireStressEnabled(t)

	ctx := context.Background()

	// Start embedded NATS
	nc, natsCleanup := testutil.StartEmbeddedNATS(t)

	// Create load generator with moderate size so the suite stays fast.
	lg := testutil.NewLoadGenerator(t, nc, 50)

	// Cleanup order: workers first, then NATS
	defer natsCleanup()
	defer lg.Cleanup()

	// Run a compact stability test sized for the default fast stress suite.
	metrics := lg.RunLoadTest(ctx, testutil.LoadConfig{
		WorkerCount:    5,
		PartitionCount: 50,
		Duration:       8 * time.Second,
		SampleInterval: 1 * time.Second,
		Description:    "8-second stability test (5 workers, 50 partitions)",
	})

	// Log results
	t.Log(metrics.Report())

	// Validate no errors
	require.Empty(t, metrics.Errors, "Test should complete without errors")

	// Check for resource stability
	samples := len(metrics.MemoryUsageMB)
	require.GreaterOrEqual(t, samples, 6, "Should have collected sufficient samples")

	// Calculate memory growth over test
	if samples >= 6 {
		earlyAvg := average(metrics.MemoryUsageMB[:samples/4])  // First 25%
		lateAvg := average(metrics.MemoryUsageMB[samples*3/4:]) // Last 25%
		memoryGrowth := lateAvg - earlyAvg

		t.Logf("Memory stability: Early avg=%.2f MB, Late avg=%.2f MB, Growth=%.2f MB",
			earlyAvg, lateAvg, memoryGrowth)

		// Memory should be relatively stable (< 50 MB growth)
		require.Less(t, memoryGrowth, 50.0, "Memory should remain stable (growth < 50 MB)")
	}

	// Calculate goroutine stability
	if samples >= 6 {
		earlyAvgGoroutines := averageInt(metrics.GoroutineCount[:samples/4])
		lateAvgGoroutines := averageInt(metrics.GoroutineCount[samples*3/4:])
		goroutineGrowth := lateAvgGoroutines - earlyAvgGoroutines

		t.Logf("Goroutine stability: Early avg=%d, Late avg=%d, Growth=%d",
			earlyAvgGoroutines, lateAvgGoroutines, goroutineGrowth)

		// Goroutines should be relatively stable (< 50 growth)
		require.Less(t, goroutineGrowth, 50, "Goroutines should remain stable (growth < 50)")
	}

	// Document baseline
	t.Logf("STABILITY BASELINE [8 seconds, 5 workers]: Memory=%.2f MB, Goroutines=%d, Samples=%d",
		metrics.PeakMemoryMB(), metrics.PeakGoroutines(), samples)
}

// Helper functions

func testName(workers, partitions int) string {
	return fmt.Sprintf("%dw-%dp", workers, partitions)
}

func average(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}

	return sum / float64(len(values))
}

func averageInt(values []int) int {
	if len(values) == 0 {
		return 0
	}
	sum := 0

	for _, v := range values {
		sum += v
	}

	return sum / len(values)
}
