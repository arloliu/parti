// Package testutil provides resource monitoring utilities for stress testing.
package testutil

import (
	"fmt"
	"runtime"
	"sync"
	"time"
)

// ResourceMonitor tracks system resource usage over time.
//
// Provides periodic sampling of memory usage and goroutine counts to detect
// resource leaks and monitor resource consumption during load tests.
type ResourceMonitor struct {
	startMemory     uint64
	startGoroutines int
	samples         []ResourceSample
	done            chan struct{}
	wg              sync.WaitGroup
	mu              sync.RWMutex
}

// ResourceSample captures resource usage at a point in time.
type ResourceSample struct {
	Timestamp      time.Time
	MemoryMB       float64
	GoroutineCount int
	HeapObjects    uint64
}

// ResourceReport summarizes resource usage over a monitoring period.
//
// Provides metrics for detecting resource leaks and understanding resource
// consumption patterns during load tests.
type ResourceReport struct {
	// StartMemoryMB is the memory usage at monitoring start
	StartMemoryMB float64

	// EndMemoryMB is the memory usage at monitoring end
	EndMemoryMB float64

	// MemoryGrowthMB is the total memory growth (end - start)
	MemoryGrowthMB float64

	// StartGoroutines is the goroutine count at start
	StartGoroutines int

	// EndGoroutines is the goroutine count at end
	EndGoroutines int

	// GoroutineLeak is the number of goroutines that didn't cleanup
	GoroutineLeak int

	// PeakMemoryMB is the maximum memory usage observed
	PeakMemoryMB float64

	// PeakGoroutines is the maximum goroutine count observed
	PeakGoroutines int

	// Samples contains all resource samples collected
	Samples []ResourceSample

	// Duration is how long monitoring ran
	Duration time.Duration
}

// NewResourceMonitor creates a new resource monitor.
//
// Call Start() to begin periodic sampling, and Stop() to end sampling
// and receive a ResourceReport.
//
// Returns:
//   - *ResourceMonitor: New monitor instance ready to start
//
// Example:
//
//	monitor := testutil.NewResourceMonitor()
//	monitor.Start(1 * time.Second) // Sample every second
//	defer func() {
//	    report := monitor.Stop()
//	    if memLeak, goroutineLeak := report.DetectLeaks(); memLeak || goroutineLeak {
//	        t.Errorf("Resource leak detected!")
//	    }
//	}()
func NewResourceMonitor() *ResourceMonitor {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return &ResourceMonitor{
		startMemory:     m.Alloc,
		startGoroutines: runtime.NumGoroutine(),
		samples:         make([]ResourceSample, 0, 100),
		done:            make(chan struct{}),
	}
}

// Start begins periodic resource sampling.
//
// Launches a goroutine that samples memory and goroutine counts at the
// specified interval. Call Stop() to end sampling.
//
// Parameters:
//   - interval: How often to sample resources (e.g., 1 * time.Second)
func (rm *ResourceMonitor) Start(interval time.Duration) {
	rm.wg.Add(1)

	go func() {
		defer rm.wg.Done()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-rm.done:
				// Take one final sample before exiting
				rm.sample()
				return
			case <-ticker.C:
				rm.sample()
			}
		}
	}()
}

// Stop ends sampling and returns the final resource report.
//
// Signals the sampling goroutine to stop, waits for it to complete,
// and generates a ResourceReport summarizing resource usage.
//
// Returns:
//   - ResourceReport: Summary of resource usage during monitoring
func (rm *ResourceMonitor) Stop() ResourceReport {
	close(rm.done)
	rm.wg.Wait()

	rm.mu.RLock()
	defer rm.mu.RUnlock()

	if len(rm.samples) == 0 {
		return ResourceReport{}
	}

	startSample := rm.samples[0]
	endSample := rm.samples[len(rm.samples)-1]

	// Calculate peaks
	peakMemory := 0.0
	peakGoroutines := 0
	for _, sample := range rm.samples {
		if sample.MemoryMB > peakMemory {
			peakMemory = sample.MemoryMB
		}
		if sample.GoroutineCount > peakGoroutines {
			peakGoroutines = sample.GoroutineCount
		}
	}

	// Calculate duration
	duration := endSample.Timestamp.Sub(startSample.Timestamp)

	// Calculate growth/leak
	memoryGrowth := endSample.MemoryMB - startSample.MemoryMB
	goroutineLeak := endSample.GoroutineCount - startSample.GoroutineCount

	return ResourceReport{
		StartMemoryMB:   startSample.MemoryMB,
		EndMemoryMB:     endSample.MemoryMB,
		MemoryGrowthMB:  memoryGrowth,
		StartGoroutines: startSample.GoroutineCount,
		EndGoroutines:   endSample.GoroutineCount,
		GoroutineLeak:   goroutineLeak,
		PeakMemoryMB:    peakMemory,
		PeakGoroutines:  peakGoroutines,
		Samples:         append([]ResourceSample(nil), rm.samples...),
		Duration:        duration,
	}
}

// sample captures current resource usage.
func (rm *ResourceMonitor) sample() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	sample := ResourceSample{
		Timestamp:      time.Now(),
		MemoryMB:       float64(m.Alloc) / 1024 / 1024,
		GoroutineCount: runtime.NumGoroutine(),
		HeapObjects:    m.HeapObjects,
	}

	rm.mu.Lock()
	rm.samples = append(rm.samples, sample)
	rm.mu.Unlock()
}

// DetectLeaks checks for resource leaks based on growth thresholds.
//
// Returns true if memory grew significantly or goroutines weren't cleaned up.
//
// Thresholds:
//   - Memory leak: Growth > 50 MB
//   - Goroutine leak: Growth > 10 goroutines
//
// Returns:
//   - memoryLeak: true if significant memory growth detected
//   - goroutineLeak: true if goroutines weren't cleaned up
func (rr *ResourceReport) DetectLeaks() (memoryLeak, goroutineLeak bool) {
	const (
		memoryLeakThresholdMB  = 50.0 // 50 MB growth considered a leak
		goroutineLeakThreshold = 10   // 10 goroutines not cleaned up
	)

	memoryLeak = rr.MemoryGrowthMB > memoryLeakThresholdMB
	goroutineLeak = rr.GoroutineLeak > goroutineLeakThreshold

	return memoryLeak, goroutineLeak
}

// Summary returns a formatted summary of the resource report.
//
// Returns:
//   - string: Human-readable summary of resource usage
func (rr *ResourceReport) Summary() string {
	return fmt.Sprintf(
		"Memory: %.2f → %.2f MB (%.2f growth), Goroutines: %d → %d (%+d), Peak: %.2f MB / %d goroutines, Duration: %v, Samples: %d",
		rr.StartMemoryMB, rr.EndMemoryMB, rr.MemoryGrowthMB,
		rr.StartGoroutines, rr.EndGoroutines, rr.GoroutineLeak,
		rr.PeakMemoryMB, rr.PeakGoroutines,
		rr.Duration, len(rr.Samples),
	)
}
