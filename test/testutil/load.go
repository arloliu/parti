// Package testutil provides load testing utilities for stress testing.
package testutil

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// LoadGenerator manages load testing scenarios for stress and performance testing.
//
// Provides utilities for running load tests with configurable worker counts,
// partition counts, churn rates, and duration. Captures performance metrics
// and resource usage during test execution.
type LoadGenerator struct {
	t           *testing.T
	cluster     *WorkerCluster
	resourceMon *ResourceMonitor
	metrics     *LoadMetrics
}

// LoadConfig configures load testing parameters.
//
// Defines the test scenario parameters including worker count, partition count,
// churn behavior, and test duration.
type LoadConfig struct {
	// WorkerCount is the initial number of workers to create
	WorkerCount int

	// PartitionCount is the number of partitions to distribute
	PartitionCount int

	// ChurnRate controls worker join/leave frequency (0 = no churn)
	ChurnRate time.Duration

	// Duration is how long to run the load test
	Duration time.Duration

	// SampleInterval is how often to sample metrics (default: 1s)
	SampleInterval time.Duration

	// Description is a human-readable description of the test
	Description string
}

// LoadMetrics captures performance metrics during load testing.
//
// Tracks rebalance counts, latencies, resource usage over time,
// and provides methods for reporting and analysis.
type LoadMetrics struct {
	// Test configuration
	Config LoadConfig

	// Rebalance metrics
	RebalanceCount   int
	RebalanceLatency []time.Duration

	// Worker metrics
	WorkerJoinLatency  []time.Duration
	WorkerLeaveLatency []time.Duration

	// Resource usage samples
	MemoryUsageMB  []float64
	GoroutineCount []int
	SampleTimes    []time.Time

	// Timing
	StartTime time.Time
	EndTime   time.Time

	// Errors encountered
	Errors []error

	mu sync.Mutex
}

// NewLoadGenerator creates a new load generator for stress testing.
//
// The load generator uses an embedded NATS server and creates a worker cluster
// for testing. The generator tracks performance metrics and resource usage
// throughout the test.
//
// Parameters:
//   - t: Testing context for assertions and logging
//   - nc: NATS connection (use StartEmbeddedNATS)
//   - numPartitions: Number of partitions to create
//
// Returns:
//   - *LoadGenerator: New load generator instance ready for testing
//
// Example:
//
//	nc, cleanup := testutil.StartEmbeddedNATS(t)
//	defer cleanup()
//
//	lg := testutil.NewLoadGenerator(t, nc, 500)
//	defer lg.Cleanup()
//
//	metrics := lg.RunLoadTest(ctx, testutil.LoadConfig{
//	    WorkerCount:    20,
//	    PartitionCount: 500,
//	    Duration:       5 * time.Minute,
//	})
//	t.Logf("Completed load test: %s", metrics.Report())
func NewLoadGenerator(t *testing.T, nc *nats.Conn, numPartitions int) *LoadGenerator {
	return &LoadGenerator{
		t:           t,
		cluster:     NewWorkerCluster(t, nc, numPartitions),
		metrics:     &LoadMetrics{},
		resourceMon: NewResourceMonitor(),
	}
}

// RunLoadTest executes a load test scenario according to the provided configuration.
//
// This is a simplified version that uses the existing WorkerCluster API.
// For now, it doesn't support churn - that can be added later if needed.
//
// The test will:
//  1. Start initial workers
//  2. Monitor resource usage at configured intervals
//  3. Wait for test duration to complete
//  4. Return collected metrics
//
// Parameters:
//   - ctx: Context for cancellation (test will stop if context is cancelled)
//   - config: Load test configuration
//
// Returns:
//   - *LoadMetrics: Collected metrics from the load test
func (lg *LoadGenerator) RunLoadTest(ctx context.Context, config LoadConfig) *LoadMetrics {
	lg.t.Helper()

	// Set defaults
	if config.SampleInterval == 0 {
		config.SampleInterval = 1 * time.Second
	}

	// Initialize metrics
	lg.metrics = &LoadMetrics{
		Config:    config,
		StartTime: time.Now(),
	}

	lg.t.Logf("Starting load test: %s", config.Description)
	lg.t.Logf("  Workers: %d, Partitions: %d, Duration: %v",
		config.WorkerCount, config.PartitionCount, config.Duration)

	// Start resource monitoring
	lg.resourceMon.Start(config.SampleInterval)

	// Note: partitions are already set by NewLoadGenerator via cluster

	// Start workers
	lg.t.Logf("Starting %d workers...", config.WorkerCount)
	workerStartTime := time.Now()

	for i := 0; i < config.WorkerCount; i++ {
		_ = lg.cluster.AddWorker(ctx)
	}

	// Start all workers
	lg.cluster.StartWorkers(ctx)

	// Wait for cluster to stabilize
	lg.cluster.WaitForStableState(30 * time.Second)
	workerStartLatency := time.Since(workerStartTime)
	lg.metrics.RecordWorkerJoin(workerStartLatency)

	lg.t.Logf("All workers started and stable (took %v)", workerStartLatency)

	// Run test for configured duration (sampling metrics periodically)
	testCtx, cancel := context.WithTimeout(ctx, config.Duration)
	defer cancel()

	lg.runMonitoring(testCtx, config)

	// Stop resource monitoring
	resourceReport := lg.resourceMon.Stop()

	// Record final state
	lg.metrics.EndTime = time.Now()

	lg.t.Logf("Load test completed in %v", lg.metrics.Duration())
	lg.t.Logf("Resource report: %s", resourceReport.Summary())

	// Log resource leaks as warnings (cleanup happens after this in defer)
	if memLeak, goroutineLeak := resourceReport.DetectLeaks(); memLeak || goroutineLeak {
		if memLeak {
			lg.t.Logf("WARNING: Possible memory leak - Growth %.2f MB", resourceReport.MemoryGrowthMB)
		}
		if goroutineLeak {
			lg.t.Logf("WARNING: Possible goroutine leak - %d goroutines not cleaned up (may be resolved by defer cleanup)", resourceReport.GoroutineLeak)
		}
	}

	return lg.metrics
}

// runMonitoring samples metrics during the test duration.
func (lg *LoadGenerator) runMonitoring(ctx context.Context, config LoadConfig) {
	lg.t.Helper()

	// Sample metrics periodically
	ticker := time.NewTicker(config.SampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lg.sampleMetrics()
		}
	}
}

// sampleMetrics captures current resource usage.
func (lg *LoadGenerator) sampleMetrics() {
	lg.t.Helper()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	memoryMB := float64(m.Alloc) / 1024 / 1024
	goroutines := runtime.NumGoroutine()

	lg.metrics.mu.Lock()
	lg.metrics.MemoryUsageMB = append(lg.metrics.MemoryUsageMB, memoryMB)
	lg.metrics.GoroutineCount = append(lg.metrics.GoroutineCount, goroutines)
	lg.metrics.SampleTimes = append(lg.metrics.SampleTimes, time.Now())
	lg.metrics.mu.Unlock()
}

// Cleanup releases resources used by the load generator.
func (lg *LoadGenerator) Cleanup() {
	lg.t.Helper()
	lg.cluster.StopWorkers()
}

// RecordRebalance records a rebalance event with its latency.
func (lm *LoadMetrics) RecordRebalance(latency time.Duration) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	lm.RebalanceCount++
	lm.RebalanceLatency = append(lm.RebalanceLatency, latency)
}

// RecordWorkerJoin records a worker join event with its latency.
func (lm *LoadMetrics) RecordWorkerJoin(latency time.Duration) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	lm.WorkerJoinLatency = append(lm.WorkerJoinLatency, latency)
}

// RecordWorkerLeave records a worker leave event with its latency.
func (lm *LoadMetrics) RecordWorkerLeave(latency time.Duration) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	lm.WorkerLeaveLatency = append(lm.WorkerLeaveLatency, latency)
}

// RecordError records an error encountered during the test.
func (lm *LoadMetrics) RecordError(err error) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	lm.Errors = append(lm.Errors, err)
}

// Duration returns the total test duration.
func (lm *LoadMetrics) Duration() time.Duration {
	if lm.EndTime.IsZero() {
		return time.Since(lm.StartTime)
	}

	return lm.EndTime.Sub(lm.StartTime)
}

// AvgRebalanceLatency returns the average rebalance latency.
func (lm *LoadMetrics) AvgRebalanceLatency() time.Duration {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if len(lm.RebalanceLatency) == 0 {
		return 0
	}

	var total time.Duration
	for _, lat := range lm.RebalanceLatency {
		total += lat
	}

	return total / time.Duration(len(lm.RebalanceLatency))
}

// LatencyPercentile returns the Nth percentile latency from rebalance latencies.
//
// Parameters:
//   - p: Percentile (0.0-1.0), e.g., 0.95 for 95th percentile
//
// Returns:
//   - time.Duration: The latency at the specified percentile
func (lm *LoadMetrics) LatencyPercentile(p float64) time.Duration {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if len(lm.RebalanceLatency) == 0 {
		return 0
	}

	sorted := make([]time.Duration, len(lm.RebalanceLatency))
	copy(sorted, lm.RebalanceLatency)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	index := int(float64(len(sorted)) * p)
	if index >= len(sorted) {
		index = len(sorted) - 1
	}

	return sorted[index]
}

// PeakMemoryMB returns the peak memory usage during the test.
func (lm *LoadMetrics) PeakMemoryMB() float64 {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	peak := 0.0
	for _, mem := range lm.MemoryUsageMB {
		if mem > peak {
			peak = mem
		}
	}

	return peak
}

// PeakGoroutines returns the peak goroutine count during the test.
func (lm *LoadMetrics) PeakGoroutines() int {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	peak := 0
	for _, count := range lm.GoroutineCount {
		if count > peak {
			peak = count
		}
	}

	return peak
}

// Report generates a formatted report of the load test metrics.
//
// Returns:
//   - string: Human-readable report summarizing test results
func (lm *LoadMetrics) Report() string {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	report := "\n=== Load Test Report ===\n"
	report += fmt.Sprintf("Description: %s\n", lm.Config.Description)
	report += fmt.Sprintf("Duration: %v\n", lm.Duration())
	report += "\nConfiguration:\n"
	report += fmt.Sprintf("  Workers: %d\n", lm.Config.WorkerCount)
	report += fmt.Sprintf("  Partitions: %d\n", lm.Config.PartitionCount)
	report += fmt.Sprintf("  Churn Rate: %v\n", lm.Config.ChurnRate)

	report += "\nRebalance Metrics:\n"
	report += fmt.Sprintf("  Count: %d\n", lm.RebalanceCount)
	if len(lm.RebalanceLatency) > 0 {
		report += fmt.Sprintf("  Avg Latency: %v\n", lm.AvgRebalanceLatency())
		report += fmt.Sprintf("  P50 Latency: %v\n", lm.LatencyPercentile(0.50))
		report += fmt.Sprintf("  P95 Latency: %v\n", lm.LatencyPercentile(0.95))
		report += fmt.Sprintf("  P99 Latency: %v\n", lm.LatencyPercentile(0.99))
	}

	// Calculate peak memory inline (can't call PeakMemoryMB() while holding lock)
	peakMemory := 0.0
	for _, mem := range lm.MemoryUsageMB {
		if mem > peakMemory {
			peakMemory = mem
		}
	}

	// Calculate peak goroutines inline (can't call PeakGoroutines() while holding lock)
	peakGoroutines := 0
	for _, count := range lm.GoroutineCount {
		if count > peakGoroutines {
			peakGoroutines = count
		}
	}

	report += "\nResource Usage:\n"
	report += fmt.Sprintf("  Peak Memory: %.2f MB\n", peakMemory)
	report += fmt.Sprintf("  Peak Goroutines: %d\n", peakGoroutines)
	report += fmt.Sprintf("  Samples: %d\n", len(lm.MemoryUsageMB))

	if len(lm.Errors) > 0 {
		report += fmt.Sprintf("\nErrors: %d\n", len(lm.Errors))
		for i, err := range lm.Errors {
			if i < 5 { // Show first 5 errors
				report += fmt.Sprintf("  %d: %v\n", i+1, err)
			}
		}
		if len(lm.Errors) > 5 {
			report += fmt.Sprintf("  ... and %d more\n", len(lm.Errors)-5)
		}
	}

	return report
}
