//go:build integration
// +build integration

package integration_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/internal/logger"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// TestManager_StartStop tests basic lifecycle.
func TestManager_StartStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Start NATS server
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	// Create config with faster timeouts for tests
	cfg := parti.Config{
		WorkerIDPrefix:        "test-worker",
		WorkerIDMin:           0,
		WorkerIDMax:           99,
		WorkerIDTTL:           3 * time.Second,
		HeartbeatInterval:     300 * time.Millisecond,
		HeartbeatTTL:          1 * time.Second,
		ElectionTimeout:       1 * time.Second,
		StartupTimeout:        5 * time.Second,
		ShutdownTimeout:       2 * time.Second,
		ColdStartWindow:       500 * time.Millisecond,
		PlannedScaleWindow:    300 * time.Millisecond,
		RestartDetectionRatio: 0.5,
	}

	// Create partition source
	partitions := []types.Partition{
		{Keys: []string{"partition-1"}, Weight: 100},
		{Keys: []string{"partition-2"}, Weight: 100},
	}
	src := source.NewStatic(partitions)

	// Create assignment strategy
	strategy := strategy.NewConsistentHash()

	// Create manager
	mgr, err := parti.NewManager(&cfg, conn, src, strategy)
	require.NoError(t, err)
	require.NotNil(t, mgr)

	// Start manager - use t.Context() which auto-cancels on test end
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	err = mgr.Start(ctx)
	require.NoError(t, err)

	// Verify state
	require.Equal(t, parti.StateStable, mgr.State())
	require.NotEmpty(t, mgr.WorkerID())

	t.Logf("Worker started with ID: %s, IsLeader: %v", mgr.WorkerID(), mgr.IsLeader())

	// Stop manager - use t.Context() instead of new context
	stopCtx, stopCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopCancel()

	err = mgr.Stop(stopCtx)
	require.NoError(t, err)

	// Verify shutdown state
	require.Equal(t, parti.StateShutdown, mgr.State())
}

// TestManager_MultipleWorkers tests multiple workers coordinating.
func TestManager_MultipleWorkers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Start NATS server
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	// Create debug logger for troubleshooting
	debugLogger := logger.NewTest(t)
	debugLogger.Info("TEST: Debug logger created successfully")

	// Create config with faster timeouts
	cfg := parti.Config{
		WorkerIDPrefix:        "multi-worker",
		WorkerIDMin:           0,
		WorkerIDMax:           99,
		WorkerIDTTL:           3 * time.Second,
		HeartbeatInterval:     300 * time.Millisecond,
		HeartbeatTTL:          1 * time.Second,
		ElectionTimeout:       1 * time.Second,
		StartupTimeout:        5 * time.Second,
		ShutdownTimeout:       2 * time.Second,
		ColdStartWindow:       500 * time.Millisecond,
		PlannedScaleWindow:    300 * time.Millisecond,
		RestartDetectionRatio: 0.5,
	}

	// Create partition source
	partitions := []types.Partition{
		{Keys: []string{"partition-1"}, Weight: 100},
		{Keys: []string{"partition-2"}, Weight: 100},
		{Keys: []string{"partition-3"}, Weight: 100},
	}
	src := source.NewStatic(partitions)

	// Create assignment strategy
	strategy := strategy.NewConsistentHash()

	// Create 3 workers
	workers := make([]*parti.Manager, 3)
	for i := range workers {
		mgr, err := parti.NewManager(&cfg, conn, src, strategy, parti.WithLogger(debugLogger))
		require.NoError(t, err)
		workers[i] = mgr
	}

	// Start all workers concurrently - use t.Context()
	startCtx, startCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer startCancel()

	var wg sync.WaitGroup
	startErrors := make([]error, len(workers))
	for i, mgr := range workers {
		wg.Add(1) //nolint:revive // Explicit error handling requires this pattern
		go func(idx int, m *parti.Manager) {
			defer wg.Done()
			if err := m.Start(startCtx); err != nil {
				startErrors[idx] = err
			}
			t.Logf("Worker %d started with ID: %s", idx, m.WorkerID())
		}(i, mgr)
	}
	wg.Wait()

	// Check for start errors
	for i, err := range startErrors {
		require.NoError(t, err, "worker %d failed to start", i)
	}

	// Verify all workers started with faster timeout
	deadline := time.Now().Add(8 * time.Second)
	for i, mgr := range workers {
		for time.Now().Before(deadline) {
			state := mgr.State()
			if state == parti.StateStable {
				break
			}
			t.Logf("Worker %d in state %s, waiting...", i, state)
			time.Sleep(200 * time.Millisecond)
		}
		require.Equal(t, parti.StateStable, mgr.State(), "worker %d should be stable", i)
		require.NotEmpty(t, mgr.WorkerID(), "worker %d should have ID", i)
	}

	// Verify exactly one leader
	leaderCount := 0
	for _, mgr := range workers {
		if mgr.IsLeader() {
			leaderCount++
		}
	}
	require.Equal(t, 1, leaderCount, "should have exactly one leader")

	// Stop all workers - use t.Context()
	stopCtx, stopCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopCancel()

	for i, mgr := range workers {
		err := mgr.Stop(stopCtx)
		require.NoError(t, err, "worker %d stop failed", i)
		t.Logf("Worker %d stopped", i)
	}
}
