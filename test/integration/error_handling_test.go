//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/test/testutil"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// TestErrorHandling_ConcurrentStart verifies that concurrent Start() calls are safe.
func TestErrorHandling_ConcurrentStart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	t.Log("Test: Verify concurrent Start() calls are handled safely")

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// Create a cluster and add one worker (without starting it yet)
	cluster := testutil.NewFastWorkerCluster(t, nc, 20)
	defer cluster.StopWorkers()

	mgr := cluster.AddWorkerWithoutTracking(ctx)

	// Try to start the manager concurrently from multiple goroutines
	const numGoroutines = 10
	errorsCh := make(chan error, numGoroutines)

	t.Log("Starting manager from 10 goroutines concurrently...")

	for range numGoroutines {
		go func() {
			err := mgr.Start(ctx)
			errorsCh <- err
		}()
	}

	// Collect results
	var successCount int
	var alreadyStartedCount int

	for range numGoroutines {
		err := <-errorsCh
		if err == nil {
			successCount++
		} else if err == parti.ErrAlreadyStarted {
			alreadyStartedCount++
		} else {
			t.Errorf("Unexpected error from Start(): %v", err)
		}
	}

	t.Logf("Results: %d successful, %d already-started errors", successCount, alreadyStartedCount)

	// Verify: Exactly one Start() should succeed
	require.Equal(t, 1, successCount, "expected exactly 1 successful Start()")
	require.Equal(t, numGoroutines-1, alreadyStartedCount, "expected %d ErrAlreadyStarted", numGoroutines-1)

	// Verify manager is running
	require.NotEqual(t, "", mgr.WorkerID(), "manager should have claimed a worker ID")

	t.Log("Test passed - concurrent Start() calls handled safely")
}

// TestErrorHandling_ConcurrentStop verifies that concurrent Stop() calls are safe.
func TestErrorHandling_ConcurrentStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	t.Log("Test: Verify concurrent Stop() calls are handled safely")

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// Create cluster and start a manager
	cluster := testutil.NewFastWorkerCluster(t, nc, 20)
	defer cluster.StopWorkers()

	mgr := cluster.AddWorkerWithoutTracking(ctx)
	err := mgr.Start(ctx)
	require.NoError(t, err)

	// Wait for stable state
	t.Log("Waiting for manager to reach Stable state...")
	waitCh := mgr.WaitState(types.StateStable, 10*time.Second)
	select {
	case err := <-waitCh:
		require.NoError(t, err, "failed to reach Stable state")
	case <-ctx.Done():
		t.Fatal("context cancelled before reaching Stable state")
	}

	t.Log("Manager reached Stable state, now stopping concurrently...")

	// Try to stop the manager concurrently from multiple goroutines
	const numGoroutines = 10
	errorsCh := make(chan error, numGoroutines)

	stopCtx := context.Background()
	for range numGoroutines {
		go func() {
			err := mgr.Stop(stopCtx)
			errorsCh <- err
		}()
	}

	// Collect results
	var successCount int
	var notStartedCount int

	for range numGoroutines {
		err := <-errorsCh
		if err == nil {
			successCount++
		} else if err == parti.ErrNotStarted {
			notStartedCount++
		} else {
			t.Errorf("Unexpected error from Stop(): %v", err)
		}
	}

	t.Logf("Results: %d successful, %d not-started errors", successCount, notStartedCount)

	// Verify: At least one Stop() should succeed, rest should get ErrNotStarted
	require.GreaterOrEqual(t, successCount, 1, "at least 1 Stop() should succeed")
	require.Equal(t, numGoroutines, successCount+notStartedCount, "all calls should return either success or ErrNotStarted")

	t.Log("Test passed - concurrent Stop() calls handled safely")
}

// TestErrorHandling_StopDuringStateTransition verifies Stop() works during state transitions.
func TestErrorHandling_StopDuringStateTransition(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	t.Log("Test: Verify Stop() during state transitions is handled gracefully")

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Create cluster
	cluster := testutil.NewFastWorkerCluster(t, nc, 100)
	defer cluster.StopWorkers()

	// Add and start 2 managers
	for range 2 {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}

	t.Log("Managers started, waiting to reach Stable state...")

	// Wait for both to reach Stable
	cluster.WaitForStableState(15 * time.Second)
	t.Log("Both managers in Stable state")

	// Add a third worker to trigger scaling
	mgr3 := cluster.AddWorker(ctx)
	err := mgr3.Start(ctx)
	require.NoError(t, err)

	t.Log("Third manager started, triggering scaling...")

	// Wait a moment for scaling to begin
	time.Sleep(200 * time.Millisecond)

	// Stop the first manager during scaling/rebalancing
	t.Log("Stopping first manager during state transition...")
	stopCtx := context.Background()
	err = cluster.Workers[0].Stop(stopCtx)
	require.NoError(t, err)

	t.Log("First manager stopped successfully during transition")

	// Wait for remaining managers to stabilize
	t.Log("Waiting for remaining managers to stabilize...")
	time.Sleep(3 * time.Second)

	t.Log("Test passed - Stop() during state transitions handled gracefully")
}

// TestErrorHandling_ContextCancellation verifies context cancellation is respected.
func TestErrorHandling_ContextCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	t.Log("Test: Verify context cancellation is respected during startup")

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Create a cluster and add a manager (without starting)
	cluster := testutil.NewFastWorkerCluster(t, nc, 20)
	defer cluster.StopWorkers()

	mgr := cluster.AddWorkerWithoutTracking(context.Background())

	// Create an already-cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	t.Log("Starting manager with already-cancelled context...")

	// Start should fail immediately due to context cancellation
	startTime := time.Now()
	err := mgr.Start(ctx)
	elapsed := time.Since(startTime)

	t.Logf("Start() returned after %v with error: %v", elapsed, err)

	// Verify: Should fail with context error quickly
	require.Error(t, err, "Start() should fail due to context cancellation")
	require.Contains(t, err.Error(), "context canceled", "error should mention context cancellation")
	require.Less(t, elapsed, 1*time.Second, "Should fail quickly with cancelled context")

	t.Log("Test passed - context cancellation respected during startup")
}
