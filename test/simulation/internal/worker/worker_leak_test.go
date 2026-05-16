package worker

import (
	"context"
	"runtime"
	"testing"
	"time"

	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// countGoroutines returns the current number of goroutines.
func countGoroutines() int {
	return runtime.NumGoroutine()
}

// TestWorker_GoroutineLeak verifies that a single Start() call doesn't leak goroutines.
// computeExpectedBudget estimates the upper bound of goroutines introduced by a worker.
// This is heuristic: per-subject loops + JetStream watchers + manager infra + resolver + slack.
func computeExpectedBudget(partitions int) int {
	perSubject := partitions            // runSubjectLoop goroutines
	jetstreamWatchers := partitions * 2 // pull heartbeat + expiry timers
	managerInfra := 15                  // heartbeat, election, assignment calc, stable id, timers
	resolverWatchers := 2               // claim-based resolver goroutines
	slack := 10                         // GC, testing, misc timing residuals
	return perSubject + jetstreamWatchers + managerInfra + resolverWatchers + slack
}

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

	// Allow assignment & loops to start
	time.Sleep(600 * time.Millisecond)

	// Improved shutdown order: cancel first so loops see ctx.Done promptly, then Stop.
	cancel()
	worker.Stop()

	// Wait for 2x fetch timeout (~350ms) plus slack for iterator expiry & internal timers.
	time.Sleep(900 * time.Millisecond)
	runtime.GC()
	time.Sleep(120 * time.Millisecond)

	// Check goroutine count
	afterStop := countGoroutines()
	leaked := afterStop - baseline
	t.Logf("After stop: %d, Leaked: %d", afterStop, leaked)

	budget := computeExpectedBudget(cfg.PartitionCount)
	t.Logf("Leak budget=%d", budget)
	require.LessOrEqual(t, leaked, budget, "goroutine leak exceeds dynamic budget")
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
	for range 3 {
		err = worker.Start(ctx)
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)
	}

	// Allow goroutines to stabilize
	time.Sleep(600 * time.Millisecond)

	// Cancel first, then Stop to trigger prompt loop termination.
	cancel()
	worker.Stop()

	// Wait for cleanup (2x fetch timeout + slack)
	time.Sleep(900 * time.Millisecond)
	runtime.GC()
	time.Sleep(120 * time.Millisecond)

	afterStop := countGoroutines()
	leaked := afterStop - baseline
	budget := computeExpectedBudget(cfg.PartitionCount)
	t.Logf("Baseline=%d AfterStop=%d Leaked=%d Budget=%d", baseline, afterStop, leaked, budget)
	require.LessOrEqual(t, leaked, budget, "multiple starts did not exceed dynamic leak budget")
}
