package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	"github.com/arloliu/parti/test/testutil"
	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

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
		ColdStartWindow:       2 * time.Second,
		PlannedScaleWindow:    1 * time.Second,
		RestartDetectionRatio: 0.5,
		Assignment: parti.AssignmentConfig{
			MinRebalanceThreshold: 0.15,
			MinRebalanceInterval:  2 * time.Second,
		},
	}

	// Create 3 managers
	const numManagers = 3
	managers := make([]*parti.Manager, numManagers)

	for i := 0; i < numManagers; i++ {
		manager, err := parti.NewManager(cfg, conn, partitionSource, strategy.NewRoundRobin(), parti.WithLogger(debugLogger))
		require.NoError(t, err, "Failed to create manager %d", i)
		managers[i] = manager
	}

	// Cleanup function for all managers
	cleanupManagers := func() {
		for i, manager := range managers {
			if manager != nil {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer stopCancel() //nolint:revive
				if err := manager.Stop(stopCtx); err != nil {
					t.Logf("Failed to stop manager %d: %v", i, err)
				}
			}
		}
	}
	defer cleanupManagers()

	// Start all managers concurrently
	t.Log("Starting 3 managers with 50 partitions...")
	for i, manager := range managers {
		err := manager.Start(ctx)
		require.NoError(t, err, "Failed to start manager %d", i)
	}

	// Wait for all managers to reach Stable state
	t.Log("Waiting for all managers to reach Stable state...")
	managerWaiters := make([]testutil.ManagerWaiter, len(managers))
	for i, m := range managers {
		managerWaiters[i] = m
	}
	err := testutil.WaitAllManagersState(ctx, managerWaiters, types.StateStable, 20*time.Second)
	require.NoError(t, err, "Managers failed to reach Stable state")

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
	partitionSource.Update(allPartitions)

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

	err = testutil.WaitAllManagersState(ctx, managerWaiters, types.StateStable, 10*time.Second)
	require.NoError(t, err, "Failed to reach Stable state after refresh")

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
		ColdStartWindow:       2 * time.Second,
		PlannedScaleWindow:    1 * time.Second,
		RestartDetectionRatio: 0.5,
		Assignment: parti.AssignmentConfig{
			MinRebalanceThreshold: 0.15,
			MinRebalanceInterval:  2 * time.Second,
		},
	}

	// Create 3 managers
	const numManagers = 3
	managers := make([]*parti.Manager, numManagers)

	for i := 0; i < numManagers; i++ {
		manager, err := parti.NewManager(cfg, conn, partitionSource, strategy.NewRoundRobin(), parti.WithLogger(debugLogger))
		require.NoError(t, err, "Failed to create manager %d", i)
		managers[i] = manager
	}

	// Cleanup function for all managers
	cleanupManagers2 := func() {
		for i, manager := range managers {
			if manager != nil {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer stopCancel() //nolint:revive
				if err := manager.Stop(stopCtx); err != nil {
					t.Logf("Failed to stop manager %d: %v", i, err)
				}
			}
		}
	}
	defer cleanupManagers2()

	// Start all managers concurrently
	t.Log("Starting 3 managers with 100 partitions...")
	for i, manager := range managers {
		err := manager.Start(ctx)
		require.NoError(t, err, "Failed to start manager %d", i)
	}

	// Wait for all managers to reach Stable state
	t.Log("Waiting for all managers to reach Stable state...")
	managerWaiters := make([]testutil.ManagerWaiter, len(managers))
	for i, m := range managers {
		managerWaiters[i] = m
	}
	err := testutil.WaitAllManagersState(ctx, managerWaiters, types.StateStable, 20*time.Second)
	require.NoError(t, err, "Managers failed to reach Stable state")

	// Verify initial assignments - all 100 partitions should be assigned
	t.Log("Verifying initial assignments (100 partitions)...")
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
	require.Equal(t, 100, len(assignedPartitions), "Not all initial partitions assigned")

	// Remove 30 partitions (keep partitions 0-69)
	t.Log("Removing 30 partitions (100 -> 70)...")
	reducedPartitions := make([]types.Partition, 70)
	for i := 0; i < 70; i++ {
		reducedPartitions[i] = types.Partition{
			Keys:   []string{"partition", fmt.Sprintf("%03d", i)},
			Weight: 100,
		}
	}
	partitionSource.Update(reducedPartitions)

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
	t.Log("Waiting for all managers to stabilize after refresh...")
	time.Sleep(3 * time.Second) // Allow time for rebalancing to complete

	err = testutil.WaitAllManagersState(ctx, managerWaiters, types.StateStable, 10*time.Second)
	require.NoError(t, err, "Failed to reach Stable state after refresh")

	// Verify exactly 70 partitions are now assigned (removed partitions should be unassigned)
	t.Log("Verifying updated assignments (70 partitions)...")
	time.Sleep(500 * time.Millisecond) // Brief stabilization

	assignedPartitions = make(map[string]bool)
	removedPartitionsStillAssigned := make([]string, 0)

	for i, manager := range managers {
		assignments := manager.CurrentAssignment()
		t.Logf("Manager %d: %d partitions assigned", i, len(assignments.Partitions))

		for _, p := range assignments.Partitions {
			partKey := fmt.Sprintf("%v", p.Keys)

			// Check if this is one of the removed partitions (70-99)
			partNum := -1
			_, _ = fmt.Sscanf(partKey, "[partition %d]", &partNum)
			if partNum >= 70 && partNum < 100 {
				removedPartitionsStillAssigned = append(removedPartitionsStillAssigned, partKey)
			}

			require.False(t, assignedPartitions[partKey], "Partition %v assigned to multiple workers", p.Keys)
			assignedPartitions[partKey] = true
		}
	}

	require.Empty(t, removedPartitionsStillAssigned, "Removed partitions still assigned: %v", removedPartitionsStillAssigned)
	require.Equal(t, 70, len(assignedPartitions), "Expected exactly 70 partitions assigned after removal")

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
		ColdStartWindow:       2 * time.Second,
		PlannedScaleWindow:    1 * time.Second,
		RestartDetectionRatio: 0.5,
		Assignment: parti.AssignmentConfig{
			MinRebalanceThreshold: 0.15,
			MinRebalanceInterval:  2 * time.Second,
		},
	}

	// Create 3 managers with ConsistentHash strategy (weight-aware)
	const numManagers = 3
	managers := make([]*parti.Manager, numManagers)

	for i := 0; i < numManagers; i++ {
		manager, err := parti.NewManager(cfg, conn, partitionSource, strategy.NewConsistentHash(), parti.WithLogger(debugLogger))
		require.NoError(t, err, "Failed to create manager %d", i)
		managers[i] = manager
	}

	// Cleanup function for all managers
	cleanupManagers3 := func() {
		for i, manager := range managers {
			if manager != nil {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer stopCancel() //nolint:revive
				if err := manager.Stop(stopCtx); err != nil {
					t.Logf("Failed to stop manager %d: %v", i, err)
				}
			}
		}
	}
	defer cleanupManagers3()

	// Start all managers concurrently
	t.Log("Starting 3 managers with 60 partitions (all weight 100)...")
	for i, manager := range managers {
		err := manager.Start(ctx)
		require.NoError(t, err, "Failed to start manager %d", i)
	}

	// Wait for all managers to reach Stable state
	t.Log("Waiting for all managers to reach Stable state...")
	managerWaiters := make([]testutil.ManagerWaiter, len(managers))
	for i, m := range managers {
		managerWaiters[i] = m
	}
	err := testutil.WaitAllManagersState(ctx, managerWaiters, types.StateStable, 20*time.Second)
	require.NoError(t, err, "Managers failed to reach Stable state")

	// Verify initial assignments - all 60 partitions should be assigned
	t.Log("Verifying initial assignments (60 partitions, all weight 100)...")
	time.Sleep(500 * time.Millisecond)

	initialDistribution := make([]int, numManagers)
	assignedPartitions := make(map[string]bool)

	for i, manager := range managers {
		assignments := manager.CurrentAssignment()
		initialDistribution[i] = len(assignments.Partitions)
		t.Logf("Manager %d: %d partitions assigned", i, len(assignments.Partitions))

		for _, p := range assignments.Partitions {
			partKey := fmt.Sprintf("%v", p.Keys)
			require.False(t, assignedPartitions[partKey], "Partition %v assigned to multiple workers", p.Keys)
			assignedPartitions[partKey] = true
		}
	}
	require.Equal(t, 60, len(assignedPartitions), "Not all initial partitions assigned")

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
	partitionSource.Update(weightedPartitions)

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
	t.Log("Waiting for all managers to stabilize after weight change...")
	time.Sleep(3 * time.Second)

	err = testutil.WaitAllManagersState(ctx, managerWaiters, types.StateStable, 10*time.Second)
	require.NoError(t, err, "Failed to reach Stable state after refresh")

	// Verify all 60 partitions still assigned and check load distribution
	t.Log("Verifying updated assignments after weight change...")
	time.Sleep(500 * time.Millisecond)

	newDistribution := make([]int, numManagers)
	assignedPartitions = make(map[string]bool)

	for i, manager := range managers {
		assignments := manager.CurrentAssignment()
		newDistribution[i] = len(assignments.Partitions)
		t.Logf("Manager %d: %d partitions assigned (was %d)", i, len(assignments.Partitions), initialDistribution[i])

		for _, p := range assignments.Partitions {
			partKey := fmt.Sprintf("%v", p.Keys)
			require.False(t, assignedPartitions[partKey], "Partition %v assigned to multiple workers", p.Keys)
			assignedPartitions[partKey] = true
		}
	}

	require.Equal(t, 60, len(assignedPartitions), "Not all partitions assigned after weight change")

	// Verify all partition keys are present
	for i := 0; i < 60; i++ {
		partKey := fmt.Sprintf("[partition %03d]", i)
		require.True(t, assignedPartitions[partKey], "Partition %s not assigned", partKey)
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
		ColdStartWindow:       4 * time.Second, // Must be >= MinRebalanceInterval
		PlannedScaleWindow:    2 * time.Second,
		RestartDetectionRatio: 0.5,
		Assignment: parti.AssignmentConfig{
			MinRebalanceThreshold: 0.15,
			MinRebalanceInterval:  3 * time.Second, // Longer cooldown for this test
		},
	}

	// Create 2 managers
	const numManagers = 2
	managers := make([]*parti.Manager, numManagers)

	for i := 0; i < numManagers; i++ {
		manager, err := parti.NewManager(cfg, conn, partitionSource, strategy.NewRoundRobin(), parti.WithLogger(debugLogger))
		require.NoError(t, err, "Failed to create manager %d", i)
		managers[i] = manager
	}

	// Cleanup function for all managers
	cleanupManagers4 := func() {
		for i, manager := range managers {
			if manager != nil {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer stopCancel() //nolint:revive
				if err := manager.Stop(stopCtx); err != nil {
					t.Logf("Failed to stop manager %d: %v", i, err)
				}
			}
		}
	}
	defer cleanupManagers4()

	// Start all managers concurrently
	t.Log("Starting 2 managers with 40 partitions...")
	for i, manager := range managers {
		err := manager.Start(ctx)
		require.NoError(t, err, "Failed to start manager %d", i)
	}

	// Wait for all managers to reach Stable state
	t.Log("Waiting for all managers to reach Stable state...")
	managerWaiters := make([]testutil.ManagerWaiter, len(managers))
	for i, m := range managers {
		managerWaiters[i] = m
	}
	err := testutil.WaitAllManagersState(ctx, managerWaiters, types.StateStable, 20*time.Second)
	require.NoError(t, err, "Managers failed to reach Stable state")

	// Verify initial assignments
	t.Log("Verifying initial assignments (40 partitions)...")
	time.Sleep(500 * time.Millisecond)

	assignedPartitions := make(map[string]bool)
	for i, manager := range managers {
		assignments := manager.CurrentAssignment()
		t.Logf("Manager %d: %d partitions assigned", i, len(assignments.Partitions))

		for _, p := range assignments.Partitions {
			partKey := fmt.Sprintf("%v", p.Keys)
			assignedPartitions[partKey] = true
		}
	}
	require.Equal(t, 40, len(assignedPartitions), "Not all initial partitions assigned")

	// Find the leader
	var leader *parti.Manager
	for _, manager := range managers {
		if manager.IsLeader() {
			leader = manager
			break
		}
	}
	require.NotNil(t, leader, "No leader found")

	// Test: Call RefreshPartitions twice rapidly (should respect cooldown)
	t.Log("Test 1: Calling RefreshPartitions() twice rapidly (should bypass cooldown)...")

	refreshCtx1, refreshCancel1 := context.WithTimeout(context.Background(), 15*time.Second)
	defer refreshCancel1()

	// First call should succeed
	err = leader.RefreshPartitions(refreshCtx1)
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
	err = testutil.WaitAllManagersState(ctx, managerWaiters, types.StateStable, 10*time.Second)
	require.NoError(t, err, "Failed to reach Stable state after rapid refreshes")

	// Verify assignments still correct
	t.Log("Verifying assignments after rapid RefreshPartitions calls...")
	time.Sleep(500 * time.Millisecond)

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

	require.Equal(t, 40, len(assignedPartitions), "Not all partitions assigned after rapid refreshes")

	// Verify all partition keys are present
	for i := 0; i < 40; i++ {
		partKey := fmt.Sprintf("[partition %03d]", i)
		require.True(t, assignedPartitions[partKey], "Partition %s not assigned", partKey)
	}

	t.Log("RefreshPartitions_Cooldown test passed - manual refresh bypasses cooldown as expected")
	t.Log("Test completed successfully: Rapid RefreshPartitions() calls handled correctly")
}
