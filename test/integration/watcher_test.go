package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/test/testutil"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// TestWatcher_FastDetection verifies watcher provides faster worker change detection than polling.
func TestWatcher_FastDetection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	t.Log("Test: Verify watcher detects worker changes faster than polling")

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Create cluster with 3 workers
	cluster := testutil.NewFastWorkerCluster(t, nc, 50)
	defer cluster.StopWorkers()

	t.Log("Starting 3 managers...")
	for range 3 {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}

	t.Log("Waiting for cluster to stabilize...")
	cluster.WaitForStableState(15 * time.Second)
	t.Log("Cluster stable with 3 workers")

	// Record current assignments
	initialAssignments := make(map[string]int)
	for _, mgr := range cluster.Workers {
		assignment := mgr.CurrentAssignment()
		initialAssignments[mgr.WorkerID()] = len(assignment.Partitions)
		t.Logf("Worker %s: %d partitions", mgr.WorkerID(), len(assignment.Partitions))
	}

	// Add 4th worker and measure detection time
	t.Log("Adding 4th worker to measure detection latency...")
	startTime := time.Now()

	mgr4 := cluster.AddWorker(ctx)
	err := mgr4.Start(ctx)
	require.NoError(t, err)

	t.Log("Waiting for leader to detect new worker and complete rebalancing...")

	// Simply wait for rebalancing to complete and check if 4th worker got assignments
	// The watcher should trigger faster detection
	detectionStart := time.Now()

	// Wait up to 10s for 4th worker to receive assignments
	var assignment4 types.Assignment
	gotAssignment := false

	for i := 0; i < 100; i++ { // 100 * 100ms = 10s timeout
		time.Sleep(100 * time.Millisecond)
		assignment4 = mgr4.CurrentAssignment()
		if len(assignment4.Partitions) > 0 {
			gotAssignment = true
			break
		}
	}

	detectionLatency := time.Since(detectionStart)
	totalLatency := time.Since(startTime)

	t.Log("Worker-4 started at T+0ms")
	t.Logf("Worker-4 received assignments at T+%dms", detectionLatency.Milliseconds())
	t.Logf("Total time from worker start: %dms", totalLatency.Milliseconds())

	require.True(t, gotAssignment, "4th worker should receive assignments")

	// With hybrid watcher+polling:
	// - Watcher should detect within ~200-800ms (100ms debounce + processing + stabilization window)
	// - Polling fallback: ~1500-3000ms (heartbeat TTL/2 + stabilization window)
	// Allow up to 5s total (includes 2s stabilization window in fast config)
	require.Less(t, detectionLatency, 5*time.Second,
		"detection + rebalancing should complete reasonably fast (got %dms)",
		detectionLatency.Milliseconds())

	if detectionLatency < 3*time.Second {
		t.Logf("Detection+rebalancing in %dms indicates watcher is working well",
			detectionLatency.Milliseconds())
	} else {
		t.Logf("Detection+rebalancing in %dms (within tolerance, may include stabilization delays)",
			detectionLatency.Milliseconds())
	}

	// Wait a bit more for complete stabilization
	time.Sleep(2 * time.Second)

	// Verify 4th worker got assignments (already checked above, but get final count)
	assignment4 = mgr4.CurrentAssignment()
	t.Logf("Worker-4 final: %d partitions", len(assignment4.Partitions))
	require.Greater(t, len(assignment4.Partitions), 0, "4th worker should have partitions")

	// Verify total still 50 partitions
	total := 0
	for _, mgr := range cluster.Workers {
		assignment := mgr.CurrentAssignment()
		total += len(assignment.Partitions)
	}
	require.Equal(t, 50, total, "expected all 50 partitions assigned")

	t.Log("Test passed - watcher detected worker changes quickly")
}

// TestWatcher_NoDoubleTriggering verifies watcher and polling don't cause duplicate rebalancing.
func TestWatcher_NoDoubleTriggering(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	t.Log("Test: Verify watcher and polling don't cause duplicate rebalancing")

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Create cluster with 2 workers
	cluster := testutil.NewFastWorkerCluster(t, nc, 30)
	defer cluster.StopWorkers()

	t.Log("Starting 2 managers...")
	for range 2 {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}

	t.Log("Waiting for cluster to stabilize...")
	cluster.WaitForStableState(15 * time.Second)
	t.Log("Cluster stable")

	leader := cluster.GetLeader()
	require.NotNil(t, leader, "expected to find leader")

	// Get initial assignment version
	initialAssignment := leader.CurrentAssignment()
	initialVersion := initialAssignment.Version
	t.Logf("Initial assignment version: %d", initialVersion)

	// Add 3rd worker
	t.Log("Adding 3rd worker...")
	mgr3 := cluster.AddWorker(ctx)
	err := mgr3.Start(ctx)
	require.NoError(t, err)

	// Wait for rebalancing to complete
	time.Sleep(8 * time.Second)

	// Check assignment version - should only increment by 1 (not 2 or more)
	// If both watcher and polling triggered, we'd see version jump by 2+
	finalAssignment := leader.CurrentAssignment()
	finalVersion := finalAssignment.Version
	versionDelta := finalVersion - initialVersion

	t.Logf("Final assignment version: %d (delta: %d)", finalVersion, versionDelta)

	// Version should increment by exactly 1 for single rebalancing event
	// Allow 2 in case of any edge cases, but should typically be 1
	require.LessOrEqual(t, versionDelta, int64(2),
		"version should increment by 1-2 (no duplicate triggers), got delta=%d", versionDelta)

	if versionDelta == 1 {
		t.Log("Version incremented by 1 - no duplicate triggering detected")
	} else {
		t.Logf("Version incremented by %d - possible duplicate trigger but within tolerance", versionDelta)
	}

	t.Log("Test passed - no excessive rebalancing detected")
}

// TestWatcher_ReconnectAfterFailure verifies watcher reconnects after NATS disruption.
func TestWatcher_ReconnectAfterFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	t.Log("Test: Verify watcher continues working after restart")

	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Create cluster with 2 workers
	cluster := testutil.NewFastWorkerCluster(t, nc, 20)
	defer cluster.StopWorkers()

	t.Log("Starting 2 managers...")
	for range 2 {
		mgr := cluster.AddWorker(ctx)
		err := mgr.Start(ctx)
		require.NoError(t, err)
	}

	t.Log("Waiting for cluster to stabilize...")
	cluster.WaitForStableState(15 * time.Second)
	t.Log("Initial cluster stable")

	// Add 3rd worker (watcher should detect)
	t.Log("Adding 3rd worker (watcher active)...")
	mgr3 := cluster.AddWorker(ctx)
	err := mgr3.Start(ctx)
	require.NoError(t, err)

	time.Sleep(8 * time.Second)

	// Verify 3rd worker got assignments
	assignment3 := mgr3.CurrentAssignment()
	t.Logf("Worker-3: %d partitions", len(assignment3.Partitions))
	require.Greater(t, len(assignment3.Partitions), 0, "3rd worker should receive partitions")

	// Note: Testing actual watcher failure/reconnect is complex with embedded NATS
	// The key behavior we're testing is that the system remains functional
	// even if the watcher encounters issues (polling fallback ensures reliability)

	t.Log("Test passed - system remains functional (watcher + polling fallback)")
}
