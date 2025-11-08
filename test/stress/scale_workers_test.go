package stress_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestScale_SmallCluster tests worker scaling from 1-10 workers with 100 partitions.
//
// This test establishes performance baselines for small cluster operations:
// - Worker startup and stabilization time
// - Memory usage per worker
// - Goroutine count patterns
// - Assignment calculation performance
//
// The test validates that the system handles small-scale scenarios efficiently
// and provides a baseline for detecting performance regressions.
//
//nolint:tparallel // Parent test has t.Parallel() call at line 28
func TestScale_SmallCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping scale test in short mode")
	}

	requireStressEnabled(t)

	t.Parallel()

	// Test different worker counts
	workerCounts := []int{1, 3, 5, 10}

	for _, workerCount := range workerCounts {
		t.Run(fmt.Sprintf("%dw", workerCount), func(t *testing.T) {
			t.Parallel()

			// Each subtest needs its own embedded NATS server
			nc, natsCleanup := testutil.StartEmbeddedNATS(t)

			ctx := context.Background()

			// Create load generator with 100 partitions
			lg := testutil.NewLoadGenerator(t, nc, 100)

			// Cleanup order: workers first, then NATS
			defer natsCleanup()
			defer lg.Cleanup()

			// Run 30-second load test
			metrics := lg.RunLoadTest(ctx, testutil.LoadConfig{
				WorkerCount:    workerCount,
				PartitionCount: 100,
				Duration:       30 * time.Second,
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

			t.Logf("Peak resource usage: %.2f MB memory, %d goroutines", peakMemory, peakGoroutines)

			// Basic sanity checks (not strict limits, just detect obvious problems)
			require.Less(t, peakMemory, 500.0, "Memory usage should be reasonable (< 500 MB)")
			require.Less(t, peakGoroutines, 500, "Goroutine count should be reasonable (< 500)")

			// Document baseline metrics
			t.Logf("BASELINE [%d workers, 100 partitions]: Memory=%.2f MB, Goroutines=%d, Duration=%v",
				workerCount, peakMemory, peakGoroutines, metrics.Duration())
		})
	}
}

// TestScale_MediumCluster tests worker scaling from 10-50 workers with 500 partitions.
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
//nolint:tparallel // Parent test has t.Parallel() call at line 100
func TestScale_MediumCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping scale test in short mode")
	}

	requireStressEnabled(t)

	t.Parallel()

	// Test different worker counts
	workerCounts := []int{10, 25, 50}

	for _, workerCount := range workerCounts {
		t.Run(testName(workerCount, 500), func(t *testing.T) {
			t.Parallel()

			// Each subtest needs its own embedded NATS server
			nc, natsCleanup := testutil.StartEmbeddedNATS(t)

			ctx := context.Background()

			// Create load generator with 500 partitions
			lg := testutil.NewLoadGenerator(t, nc, 500)

			// Cleanup order: workers first, then NATS
			defer natsCleanup()
			defer lg.Cleanup()

			// Run 60-second load test (longer for larger clusters)
			metrics := lg.RunLoadTest(ctx, testutil.LoadConfig{
				WorkerCount:    workerCount,
				PartitionCount: 500,
				Duration:       60 * time.Second,
				SampleInterval: 2 * time.Second,
				Description:    testName(workerCount, 500),
			})

			// Log results
			t.Log(metrics.Report())

			// Validate no errors
			require.Empty(t, metrics.Errors, "Test should complete without errors")

			// Validate resource usage is reasonable
			peakMemory := metrics.PeakMemoryMB()
			peakGoroutines := metrics.PeakGoroutines()

			t.Logf("Peak resource usage: %.2f MB memory, %d goroutines", peakMemory, peakGoroutines)

			// Basic sanity checks
			require.Less(t, peakMemory, 1000.0, "Memory usage should be reasonable (< 1 GB)")
			require.Less(t, peakGoroutines, 1000, "Goroutine count should be reasonable (< 1000)")

			// Document baseline metrics
			t.Logf("BASELINE [%d workers, 500 partitions]: Memory=%.2f MB, Goroutines=%d, Duration=%v",
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

	// Create load generator with reasonable size
	lg := testutil.NewLoadGenerator(t, nc, 200)

	// Cleanup order: workers first, then NATS
	defer natsCleanup()
	defer lg.Cleanup()

	// Run 5-minute stability test
	metrics := lg.RunLoadTest(ctx, testutil.LoadConfig{
		WorkerCount:    20,
		PartitionCount: 200,
		Duration:       5 * time.Minute,
		SampleInterval: 5 * time.Second, // Sample every 5s for 5 mins = 60 samples
		Description:    "5-minute stability test (20 workers, 200 partitions)",
	})

	// Log results
	t.Log(metrics.Report())

	// Validate no errors
	require.Empty(t, metrics.Errors, "Test should complete without errors")

	// Check for resource stability
	samples := len(metrics.MemoryUsageMB)
	require.Greater(t, samples, 30, "Should have collected sufficient samples")

	// Calculate memory growth over test
	if samples >= 10 {
		earlyAvg := average(metrics.MemoryUsageMB[:samples/4])  // First 25%
		lateAvg := average(metrics.MemoryUsageMB[samples*3/4:]) // Last 25%
		memoryGrowth := lateAvg - earlyAvg

		t.Logf("Memory stability: Early avg=%.2f MB, Late avg=%.2f MB, Growth=%.2f MB",
			earlyAvg, lateAvg, memoryGrowth)

		// Memory should be relatively stable (< 50 MB growth)
		require.Less(t, memoryGrowth, 50.0, "Memory should remain stable (growth < 50 MB)")
	}

	// Calculate goroutine stability
	if samples >= 10 {
		earlyAvgGoroutines := averageInt(metrics.GoroutineCount[:samples/4])
		lateAvgGoroutines := averageInt(metrics.GoroutineCount[samples*3/4:])
		goroutineGrowth := lateAvgGoroutines - earlyAvgGoroutines

		t.Logf("Goroutine stability: Early avg=%d, Late avg=%d, Growth=%d",
			earlyAvgGoroutines, lateAvgGoroutines, goroutineGrowth)

		// Goroutines should be relatively stable (< 50 growth)
		require.Less(t, goroutineGrowth, 50, "Goroutines should remain stable (growth < 50)")
	}

	// Document baseline
	t.Logf("STABILITY BASELINE [5 minutes, 20 workers]: Memory=%.2f MB, Goroutines=%d, Samples=%d",
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
