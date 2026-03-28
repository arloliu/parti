package assignment_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// Helper functions to reduce cyclomatic complexity

func createTestConfig() *parti.Config {
	return &parti.Config{
		WorkerIDPrefix:        "test-worker",
		WorkerIDMin:           0,
		WorkerIDMax:           99,
		WorkerIDTTL:           10 * time.Second,
		HeartbeatInterval:     300 * time.Millisecond,
		HeartbeatTTL:          1 * time.Second,
		ElectionTimeout:       1 * time.Second,
		StartupTimeout:        5 * time.Second,
		ShutdownTimeout:       2 * time.Second,
		ColdStartWindow:       2 * time.Second,
		PlannedScaleWindow:    2 * time.Second, // Must be >= RebalanceCooldown
		RestartDetectionRatio: 0.5,
		RebalanceCooldown:     1 * time.Second,
	}
}

func setupManagers(t *testing.T, conn *nats.Conn, cfg *parti.Config, src parti.PartitionSource, strategy parti.AssignmentStrategy, numManagers int, logger parti.Logger) []*parti.Manager {
	t.Helper()
	managers := make([]*parti.Manager, numManagers)
	for i := 0; i < numManagers; i++ {
		js, err := jetstream.New(conn)
		require.NoError(t, err)
		manager, err := parti.NewManager(cfg, js, src, strategy, parti.WithLogger(logger))
		require.NoError(t, err, "Failed to create manager %d", i)
		managers[i] = manager
	}

	return managers
}

func startManagers(t *testing.T, ctx context.Context, managers []*parti.Manager) {
	t.Helper()
	t.Logf("Starting %d managers...", len(managers))
	for i, manager := range managers {
		err := manager.Start(ctx)
		require.NoError(t, err, "Failed to start manager %d", i)
	}
}

func waitForStable(t *testing.T, ctx context.Context, managers []*parti.Manager) {
	t.Helper()
	t.Log("Waiting for all managers to reach Stable state...")
	managerWaiters := make([]testutil.ManagerWaiter, len(managers))
	for i, m := range managers {
		managerWaiters[i] = m
	}
	err := testutil.WaitAllManagersState(ctx, managerWaiters, types.StateStable, 20*time.Second)
	require.NoError(t, err, "Managers failed to reach Stable state")
}

func cleanupManagers(t *testing.T, managers []*parti.Manager) {
	t.Helper()
	for i, manager := range managers {
		if manager != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := manager.Stop(stopCtx); err != nil {
				t.Logf("Failed to stop manager %d: %v", i, err)
			}
			stopCancel()
		}
	}
}

func verifyAssignments(t *testing.T, managers []*parti.Manager, expectedCount int) {
	t.Helper()
	t.Logf("Verifying assignments (expected %d partitions)...", expectedCount)
	time.Sleep(500 * time.Millisecond) // Brief stabilization

	assignedPartitions := make(map[string]bool)
	for i, manager := range managers {
		assignments := manager.CurrentAssignment()
		t.Logf("Manager %d: %d partitions assigned", i, len(assignments.Partitions))

		for _, p := range assignments.Partitions {
			partKey := fmt.Sprintf("%v", p.Keys)
			require.False(t, assignedPartitions[partKey], "Partition %v assigned to multiple workers", p.Keys)
			assignedPartitions[partKey] = true
		}
	}
	require.Equal(t, expectedCount, len(assignedPartitions), "Not all partitions assigned")
}

func findLeader(t *testing.T, managers []*parti.Manager) *parti.Manager {
	t.Helper()
	for _, manager := range managers {
		if manager.IsLeader() {
			return manager
		}
	}
	require.Fail(t, "No leader found")

	return nil
}

func refreshPartitions(t *testing.T, leader *parti.Manager) {
	t.Helper()
	t.Log("Calling RefreshPartitions() on leader...")
	refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer refreshCancel()

	err := leader.RefreshPartitions(refreshCtx)
	require.NoError(t, err, "RefreshPartitions failed")
}

func waitForVersionChange(t *testing.T, managers []*parti.Manager, initialVersion int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, m := range managers {
			if m.State() != types.StateStable {
				return false
			}
			if m.CurrentAssignment().Version <= initialVersion {
				return false
			}
		}

		return true
	}, 20*time.Second, 100*time.Millisecond, "Managers failed to reach Stable state with new version")
}

func TestRefreshPartitions_Addition(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	// Setup embedded NATS
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	debugLogger := logging.NewNop()

	// Create initial partition source with 50 partitions
	initialPartitions := make([]types.Partition, 50)
	for i := 0; i < 50; i++ {
		initialPartitions[i] = types.Partition{
			Keys:   []string{"partition", fmt.Sprintf("%03d", i)},
			Weight: 100,
		}
	}

	partitionSource := source.NewStatic(initialPartitions)

	// Create config with fast timeouts for tests
	cfg := createTestConfig()

	// Create 3 managers
	const numManagers = 3
	managers := setupManagers(t, conn, cfg, partitionSource, strategy.NewRoundRobin(), numManagers, debugLogger)

	// Cleanup function for all managers
	defer cleanupManagers(t, managers)

	// Start all managers concurrently
	t.Log("Starting 3 managers with 50 partitions...")
	startManagers(t, ctx, managers)

	// Wait for all managers to reach Stable state
	t.Log("Waiting for all managers to reach Stable state...")
	waitForStable(t, ctx, managers)

	// Verify initial assignments - all 50 partitions should be assigned
	t.Log("Verifying initial assignments (50 partitions)...")
	time.Sleep(500 * time.Millisecond) // Brief stabilization

	assignedPartitions := make(map[string]bool)
	for i, manager := range managers {
		assignments := manager.CurrentAssignment()
		t.Logf("Manager %d: %d partitions assigned", i, len(assignments.Partitions))

		for _, p := range assignments.Partitions {
			partKey := fmt.Sprintf("%v", p.Keys)
			require.False(t, assignedPartitions[partKey], "Partition %v assigned to multiple workers", p.Keys)
			assignedPartitions[partKey] = true
		}
	}
	require.Equal(t, 50, len(assignedPartitions), "Not all initial partitions assigned")

	// Add 20 more partitions to the source
	t.Log("Adding 20 more partitions (50 -> 70)...")
	allPartitions := make([]types.Partition, 70)
	copy(allPartitions, initialPartitions)
	for i := 50; i < 70; i++ {
		allPartitions[i] = types.Partition{
			Keys:   []string{"partition", fmt.Sprintf("%03d", i)},
			Weight: 100,
		}
	}
	err := partitionSource.Update(ctx, allPartitions)
	require.NoError(t, err)

	// Find the leader and call RefreshPartitions
	var leader *parti.Manager
	for _, manager := range managers {
		if manager.IsLeader() {
			leader = manager
			break
		}
	}
	require.NotNil(t, leader, "No leader found")

	t.Log("Calling RefreshPartitions() on leader...")
	refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer refreshCancel()

	err = leader.RefreshPartitions(refreshCtx)
	require.NoError(t, err, "RefreshPartitions failed")

	// Wait for assignments to stabilize after refresh
	// The state machine may transition through Scaling -> Rebalancing -> Stable very quickly
	t.Log("Waiting for all managers to stabilize after refresh...")
	time.Sleep(3 * time.Second) // Allow time for rebalancing to complete

	waitForStable(t, ctx, managers)

	// Verify all 70 partitions are now assigned exactly once
	t.Log("Verifying updated assignments (70 partitions)...")
	time.Sleep(500 * time.Millisecond) // Brief stabilization

	assignedPartitions = make(map[string]bool)
	for i, manager := range managers {
		assignments := manager.CurrentAssignment()
		t.Logf("Manager %d: %d partitions assigned", i, len(assignments.Partitions))

		for _, p := range assignments.Partitions {
			partKey := fmt.Sprintf("%v", p.Keys)
			require.False(t, assignedPartitions[partKey], "Partition %v assigned to multiple workers", p.Keys)
			assignedPartitions[partKey] = true
		}
	}

	require.Equal(t, 70, len(assignedPartitions), "Not all 70 partitions assigned after refresh")

	// Verify all partition keys from partition-000 to partition-069 are present
	for i := 0; i < 70; i++ {
		partKey := fmt.Sprintf("[partition %03d]", i)
		require.True(t, assignedPartitions[partKey], "Partition %s not assigned", partKey)
	}

	t.Log("RefreshPartitions_Addition test passed - all 70 partitions assigned correctly")
	t.Log("Test completed successfully: 50 -> 70 partitions, rebalancing triggered by RefreshPartitions()")
}

func TestRefreshPartitions_Removal(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	// Setup embedded NATS
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	debugLogger := logging.NewNop()

	// Create initial partition source with 100 partitions
	initialPartitions := make([]types.Partition, 100)
	for i := 0; i < 100; i++ {
		initialPartitions[i] = types.Partition{
			Keys:   []string{"partition", fmt.Sprintf("%03d", i)},
			Weight: 100,
		}
	}

	partitionSource := source.NewStatic(initialPartitions)

	// Create config with fast timeouts for tests
	cfg := createTestConfig()

	// Create 3 managers
	const numManagers = 3
	managers := setupManagers(t, conn, cfg, partitionSource, strategy.NewRoundRobin(), numManagers, debugLogger)

	// Cleanup function for all managers
	defer cleanupManagers(t, managers)

	// Start all managers concurrently
	t.Log("Starting 3 managers with 100 partitions...")
	startManagers(t, ctx, managers)

	// Wait for all managers to reach Stable state
	t.Log("Waiting for all managers to reach Stable state...")
	waitForStable(t, ctx, managers)

	// Verify initial assignments - all 100 partitions should be assigned
	verifyAssignments(t, managers, 100)

	// Remove 30 partitions (keep partitions 0-69)
	t.Log("Removing 30 partitions (100 -> 70)...")
	reducedPartitions := make([]types.Partition, 70)
	for i := 0; i < 70; i++ {
		reducedPartitions[i] = types.Partition{
			Keys:   []string{"partition", fmt.Sprintf("%03d", i)},
			Weight: 100,
		}
	}
	err := partitionSource.Update(ctx, reducedPartitions)
	require.NoError(t, err)

	// Find the leader and call RefreshPartitions
	leader := findLeader(t, managers)

	// Capture max initial version
	var initialVersion int64
	for _, m := range managers {
		v := m.CurrentAssignment().Version
		if v > initialVersion {
			initialVersion = v
		}
	}

	refreshPartitions(t, leader)

	// Wait for assignments to stabilize after refresh
	t.Log("Waiting for all managers to stabilize after refresh...")
	waitForVersionChange(t, managers, initialVersion)

	// Verify exactly 70 partitions are now assigned (removed partitions should be unassigned)
	verifyAssignments(t, managers, 70)

	// Verify removed partitions are gone
	assignedPartitions := make(map[string]bool)
	removedPartitionsStillAssigned := make([]string, 0)

	for _, manager := range managers {
		assignments := manager.CurrentAssignment()
		for _, p := range assignments.Partitions {
			partKey := fmt.Sprintf("%v", p.Keys)
			// Check if this is one of the removed partitions (70-99)
			partNum := -1
			_, _ = fmt.Sscanf(partKey, "[partition %d]", &partNum)
			if partNum >= 70 && partNum < 100 {
				removedPartitionsStillAssigned = append(removedPartitionsStillAssigned, partKey)
			}
			assignedPartitions[partKey] = true
		}
	}

	require.Empty(t, removedPartitionsStillAssigned, "Removed partitions still assigned: %v", removedPartitionsStillAssigned)

	// Verify all partition keys from partition-000 to partition-069 are present
	for i := 0; i < 70; i++ {
		partKey := fmt.Sprintf("[partition %03d]", i)
		require.True(t, assignedPartitions[partKey], "Partition %s not assigned", partKey)
	}

	t.Log("RefreshPartitions_Removal test passed - exactly 70 partitions assigned, 30 removed successfully")
	t.Log("Test completed successfully: 100 -> 70 partitions, removed partitions no longer assigned")
}

func TestRefreshPartitions_WeightChange(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	// Setup embedded NATS
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	debugLogger := logging.NewNop()

	// Create initial partition source with 60 partitions, all weight 100
	initialPartitions := make([]types.Partition, 60)
	for i := 0; i < 60; i++ {
		initialPartitions[i] = types.Partition{
			Keys:   []string{"partition", fmt.Sprintf("%03d", i)},
			Weight: 100,
		}
	}

	partitionSource := source.NewStatic(initialPartitions)

	// Create config with fast timeouts for tests
	cfg := createTestConfig()

	// Create 3 managers with ConsistentHash strategy (weight-aware)
	const numManagers = 3
	managers := setupManagers(t, conn, cfg, partitionSource, strategy.NewConsistentHash(), numManagers, debugLogger)

	// Cleanup function for all managers
	defer cleanupManagers(t, managers)

	// Start all managers concurrently
	t.Log("Starting 3 managers with 60 partitions (all weight 100)...")
	startManagers(t, ctx, managers)

	// Wait for all managers to reach Stable state
	t.Log("Waiting for all managers to reach Stable state...")
	waitForStable(t, ctx, managers)

	// Verify initial assignments - all 60 partitions should be assigned
	verifyAssignments(t, managers, 60)

	// Capture initial distribution for logging
	initialDistribution := make([]int, numManagers)
	for i, manager := range managers {
		initialDistribution[i] = len(manager.CurrentAssignment().Partitions)
	}

	// Change weights: First 30 partitions get weight 200, last 30 keep weight 100
	// This should cause redistribution to balance the load
	t.Log("Changing weights: First 30 partitions -> weight 200, last 30 -> weight 100...")
	weightedPartitions := make([]types.Partition, 60)
	for i := 0; i < 30; i++ {
		weightedPartitions[i] = types.Partition{
			Keys:   []string{"partition", fmt.Sprintf("%03d", i)},
			Weight: 200, // Double weight
		}
	}
	for i := 30; i < 60; i++ {
		weightedPartitions[i] = types.Partition{
			Keys:   []string{"partition", fmt.Sprintf("%03d", i)},
			Weight: 100, // Normal weight
		}
	}
	err := partitionSource.Update(ctx, weightedPartitions)
	require.NoError(t, err)

	// Find the leader and call RefreshPartitions
	leader := findLeader(t, managers)

	// Capture max initial version
	var initialVersion int64
	for _, m := range managers {
		v := m.CurrentAssignment().Version
		if v > initialVersion {
			initialVersion = v
		}
	}

	refreshPartitions(t, leader)

	// Wait for assignments to stabilize after refresh
	t.Log("Waiting for all managers to stabilize after weight change...")
	waitForVersionChange(t, managers, initialVersion)

	// Verify all 60 partitions still assigned and check load distribution
	t.Log("Verifying updated assignments after weight change...")
	verifyAssignments(t, managers, 60)

	// Log distribution changes
	for i, manager := range managers {
		assignments := manager.CurrentAssignment()
		t.Logf("Manager %d: %d partitions assigned (was %d)", i, len(assignments.Partitions), initialDistribution[i])
	}

	// Note: We don't strictly verify load balancing here because ConsistentHash
	// prioritizes partition affinity over perfect load balance. The key test is
	// that all partitions remain assigned and the system remains stable.

	t.Log("RefreshPartitions_WeightChange test passed - all 60 partitions still assigned after weight change")
	t.Log("Test completed successfully: Weight change (30 partitions: 100->200) processed correctly")
}

func TestRefreshPartitions_Cooldown(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	// Setup embedded NATS
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	debugLogger := logging.NewNop()

	// Create initial partition source with 40 partitions
	initialPartitions := make([]types.Partition, 40)
	for i := 0; i < 40; i++ {
		initialPartitions[i] = types.Partition{
			Keys:   []string{"partition", fmt.Sprintf("%03d", i)},
			Weight: 100,
		}
	}

	partitionSource := source.NewStatic(initialPartitions)

	// Create config with 3-second cooldown (longer for this test)
	cfg := &parti.Config{
		WorkerIDPrefix:        "test-worker",
		WorkerIDMin:           0,
		WorkerIDMax:           99,
		WorkerIDTTL:           10 * time.Second,
		HeartbeatInterval:     300 * time.Millisecond,
		HeartbeatTTL:          1 * time.Second,
		ElectionTimeout:       1 * time.Second,
		StartupTimeout:        5 * time.Second,
		ShutdownTimeout:       2 * time.Second,
		ColdStartWindow:       4 * time.Second, // Must be >= RebalanceCooldown
		PlannedScaleWindow:    3 * time.Second, // Must be >= RebalanceCooldown
		RestartDetectionRatio: 0.5,
		RebalanceCooldown:     3 * time.Second, // Longer cooldown for this test
	}

	// Create 2 managers
	const numManagers = 2
	managers := setupManagers(t, conn, cfg, partitionSource, strategy.NewRoundRobin(), numManagers, debugLogger)

	// Cleanup function for all managers
	defer cleanupManagers(t, managers)

	// Start all managers concurrently
	t.Log("Starting 2 managers with 40 partitions...")
	startManagers(t, ctx, managers)

	// Wait for all managers to reach Stable state
	t.Log("Waiting for all managers to reach Stable state...")
	waitForStable(t, ctx, managers)

	// Verify initial assignments
	verifyAssignments(t, managers, 40)

	// Find the leader
	leader := findLeader(t, managers)

	// Test: Call RefreshPartitions twice rapidly (should respect cooldown)
	t.Log("Test 1: Calling RefreshPartitions() twice rapidly (should bypass cooldown)...")

	refreshCtx1, refreshCancel1 := context.WithTimeout(context.Background(), 15*time.Second)
	defer refreshCancel1()

	// First call should succeed
	err := leader.RefreshPartitions(refreshCtx1)
	require.NoError(t, err, "First RefreshPartitions failed")
	t.Log("First RefreshPartitions() succeeded")

	// Second call immediately after should also succeed (manual refresh bypasses cooldown)
	time.Sleep(100 * time.Millisecond) // Brief delay to ensure first call processed

	refreshCtx2, refreshCancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer refreshCancel2()

	err = leader.RefreshPartitions(refreshCtx2)
	require.NoError(t, err, "Second RefreshPartitions failed (manual refresh should bypass cooldown)")
	t.Log("Second RefreshPartitions() succeeded (cooldown bypassed as expected)")

	// Wait for stabilization
	time.Sleep(3 * time.Second)
	waitForStable(t, ctx, managers)

	// Verify assignments still correct
	t.Log("Verifying assignments after rapid RefreshPartitions calls...")
	verifyAssignments(t, managers, 40)

	t.Log("RefreshPartitions_Cooldown test passed - manual refresh bypasses cooldown as expected")
	t.Log("Test completed successfully: Rapid RefreshPartitions() calls handled correctly")
}
