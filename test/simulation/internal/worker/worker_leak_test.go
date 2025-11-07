package worker

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/arloliu/parti/test/simulation/internal/coordinator"
	partitesting "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// countGoroutines returns the current number of goroutines.
func countGoroutines() int {
	return runtime.NumGoroutine()
}

// TestWorker_GoroutineLeak verifies that a single Start() call doesn't leak goroutines.
func TestWorker_GoroutineLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping goroutine leak test in short mode")
	}

	// Start embedded NATS
	_, nc := partitesting.StartEmbeddedNATS(t)

	// Create JetStream context
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create stream
	_, err = js.CreateStream(context.Background(), jetstream.StreamConfig{
		Name:     "SIMULATION",
		Subjects: []string{"simulation.partition.>"},
	})
	require.NoError(t, err)

	// Baseline goroutine count
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := countGoroutines()
	t.Logf("Baseline goroutines: %d", baseline)

	// Create worker
	cfg := Config{
		ID:                  "leak-test-worker-1",
		NC:                  nc,
		JS:                  js,
		PartitionCount:      10,
		AssignmentStrategy:  "consistent-hash",
		ProcessingDelayMin:  1 * time.Millisecond,
		ProcessingDelayMax:  5 * time.Millisecond,
		CoordinatorReportCh: make(chan coordinator.ReceivedMessage, 100),
		MetricsCollector:    nil, // Don't use metrics to avoid registration conflicts
	}

	worker, err := NewWorker(cfg)
	require.NoError(t, err)

	// Create a context that we'll cancel
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start worker
	err = worker.Start(ctx)
	require.NoError(t, err)

	// Allow goroutines to start
	time.Sleep(500 * time.Millisecond)

	// Stop worker
	worker.Stop()
	cancel()

	// Allow goroutines to exit
	time.Sleep(500 * time.Millisecond)
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	// Check goroutine count
	afterStop := countGoroutines()
	leaked := afterStop - baseline
	t.Logf("After stop: %d, Leaked: %d", afterStop, leaked)

	// Allow some tolerance for background goroutines (NATS, test framework, parti manager components)
	// Worker creates: 1 consumeLoop + parti manager (heartbeat, election, assignment calculator)
	// Should be close to baseline after stop (within 15 goroutines for cleanup timing)
	require.Less(t, leaked, 15, "Expected no significant goroutine leak")
}

// TestWorker_MultipleStarts verifies that multiple Start() calls don't create duplicate goroutines.
func TestWorker_MultipleStarts(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping goroutine leak test in short mode")
	}

	// Start embedded NATS
	_, nc := partitesting.StartEmbeddedNATS(t)

	// Create JetStream context
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create stream
	_, err = js.CreateStream(context.Background(), jetstream.StreamConfig{
		Name:     "SIMULATION",
		Subjects: []string{"simulation.partition.>"},
	})
	require.NoError(t, err)

	// Baseline goroutine count
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := countGoroutines()
	t.Logf("Baseline goroutines: %d", baseline)

	// Create worker
	cfg := Config{
		ID:                  "leak-test-worker-2",
		NC:                  nc,
		JS:                  js,
		PartitionCount:      10,
		AssignmentStrategy:  "consistent-hash",
		ProcessingDelayMin:  1 * time.Millisecond,
		ProcessingDelayMax:  5 * time.Millisecond,
		CoordinatorReportCh: make(chan coordinator.ReceivedMessage, 100),
		MetricsCollector:    nil, // Don't use metrics to avoid registration conflicts
	}

	worker, err := NewWorker(cfg)
	require.NoError(t, err)

	// Create a context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Call Start() multiple times (simulating chaos restarts)
	for i := 0; i < 3; i++ {
		err = worker.Start(ctx)
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)
	}

	// Allow goroutines to stabilize
	time.Sleep(500 * time.Millisecond)

	// Check goroutine count
	afterStarts := countGoroutines()
	leaked := afterStarts - baseline
	t.Logf("Baseline: %d, After 3 starts: %d, Leaked: %d", baseline, afterStarts, leaked)

	// Should only have 1 consumeLoop goroutine + manager goroutines (heartbeat, election, assignment calculator)
	// With the fix, leaked should be ~25-35 (1 consumeLoop + manager infrastructure)
	// Without the fix, leaked would be ~75-105 (3 consumeLoops + 3x manager overhead)
	// The key is that multiple Start() calls don't multiply the goroutines
	require.Less(t, leaked, 50, "Expected only 1 consumeLoop goroutine, not 3")

	// Stop worker
	worker.Stop()
	cancel()

	// Allow goroutines to exit
	time.Sleep(500 * time.Millisecond)
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	// Verify cleanup
	afterStop := countGoroutines()
	finalLeaked := afterStop - baseline
	t.Logf("After stop: %d, Final leaked: %d", afterStop, finalLeaked)
	// Allow tolerance for NATS/manager goroutines that may not have fully cleaned up yet
	require.Less(t, finalLeaked, 15, "Expected cleanup of all worker goroutines")
}
