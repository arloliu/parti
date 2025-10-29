package integration_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	"github.com/arloliu/parti/subscription"
	"github.com/arloliu/parti/test/testutil"
	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionHelper_Creation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	// Setup embedded NATS
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create subscription helper
	helper := subscription.NewHelper(conn, subscription.Config{
		Stream:            "test-stream",
		MaxRetries:        3,
		RetryBackoff:      100 * time.Millisecond,
		ReconcileInterval: 5 * time.Second,
	})
	defer helper.Close()

	// Create test partitions
	partitions := []types.Partition{
		{Keys: []string{"partition-001"}, Weight: 100},
		{Keys: []string{"partition-002"}, Weight: 100},
		{Keys: []string{"partition-003"}, Weight: 100},
	}

	// Message counter
	var msgCount atomic.Int32
	handler := func(msg *nats.Msg) {
		msgCount.Add(1)
	}

	// Test: Create subscriptions for all partitions
	t.Log("Creating subscriptions for 3 partitions...")
	err := helper.UpdateSubscriptions(ctx, partitions, nil, handler)
	require.NoError(t, err, "Failed to create subscriptions")

	// Verify all partitions are active
	activePartitions := helper.ActivePartitions()
	require.Len(t, activePartitions, 3, "Expected 3 active subscriptions")

	// Verify subscription IDs match
	expectedIDs := map[string]bool{
		"partition-001": true,
		"partition-002": true,
		"partition-003": true,
	}
	for _, partID := range activePartitions {
		require.True(t, expectedIDs[partID], "Unexpected partition ID: %s", partID)
	}

	t.Log("Test passed - all 3 subscriptions created successfully")
}

func TestSubscriptionHelper_UpdateOnRebalance(t *testing.T) {
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

	// Create initial partition source with 30 partitions
	initialPartitions := make([]types.Partition, 30)
	for i := 0; i < 30; i++ {
		initialPartitions[i] = types.Partition{
			Keys:   []string{fmt.Sprintf("partition-%03d", i)},
			Weight: 100,
		}
	}

	partitionSource := source.NewStatic(initialPartitions)

	// Create config
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
			MinRebalanceInterval:     2 * time.Second,
		},
	}

	// Create 2 managers
	const numManagers = 2
	managers := make([]*parti.Manager, numManagers)

	for i := 0; i < numManagers; i++ {
		manager, err := parti.NewManager(cfg, conn, partitionSource, strategy.NewRoundRobin(), parti.WithLogger(debugLogger))
		require.NoError(t, err, "Failed to create manager %d", i)
		managers[i] = manager

		defer func(m *parti.Manager, idx int) {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			if err := m.Stop(stopCtx); err != nil {
				t.Logf("Failed to stop manager %d: %v", idx, err)
			}
		}(manager, i)
	}

	// Start managers
	t.Log("Starting 2 managers with 30 partitions...")
	for i, manager := range managers {
		err := manager.Start(ctx)
		require.NoError(t, err, "Failed to start manager %d", i)
	}

	// Wait for stable state
	managerWaiters := make([]testutil.ManagerWaiter, len(managers))
	for i, m := range managers {
		managerWaiters[i] = m
	}
	err := testutil.WaitAllManagersState(ctx, managerWaiters, types.StateStable, 20*time.Second)
	require.NoError(t, err, "Managers failed to reach Stable state")

	// Create subscription helper for first manager
	helper := subscription.NewHelper(conn, subscription.Config{
		Stream:            "test-stream",
		MaxRetries:        3,
		RetryBackoff:      100 * time.Millisecond,
		ReconcileInterval: 5 * time.Second,
	})
	defer helper.Close()

	// Get initial assignment and create subscriptions
	t.Log("Creating subscriptions for initial assignment...")
	initialAssignment := managers[0].CurrentAssignment()
	err = helper.UpdateSubscriptions(ctx, initialAssignment.Partitions, nil, func(msg *nats.Msg) {})
	require.NoError(t, err, "Failed to create initial subscriptions")

	initialActive := helper.ActivePartitions()
	t.Logf("Initial active subscriptions: %d partitions", len(initialActive))
	require.Equal(t, len(initialAssignment.Partitions), len(initialActive), "Subscription count mismatch")

	// Trigger rebalance by adding partitions
	t.Log("Adding 10 partitions to trigger rebalance...")
	allPartitions := make([]types.Partition, 40)
	copy(allPartitions, initialPartitions)
	for i := 30; i < 40; i++ {
		allPartitions[i] = types.Partition{
			Keys:   []string{fmt.Sprintf("partition-%03d", i)},
			Weight: 100,
		}
	}
	partitionSource.Update(allPartitions)

	// Find leader and refresh
	var leader *parti.Manager
	for _, manager := range managers {
		if manager.IsLeader() {
			leader = manager
			break
		}
	}
	require.NotNil(t, leader, "No leader found")

	refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer refreshCancel()
	err = leader.RefreshPartitions(refreshCtx)
	require.NoError(t, err, "RefreshPartitions failed")

	// Wait for stabilization
	time.Sleep(3 * time.Second)
	err = testutil.WaitAllManagersState(ctx, managerWaiters, types.StateStable, 10*time.Second)
	require.NoError(t, err, "Failed to reach Stable after rebalance")

	// Get new assignment and update subscriptions
	t.Log("Updating subscriptions based on new assignment...")
	newAssignment := managers[0].CurrentAssignment()

	// Calculate added and removed partitions
	oldPartMap := make(map[string]types.Partition)
	for _, p := range initialAssignment.Partitions {
		oldPartMap[p.Keys[0]] = p
	}

	newPartMap := make(map[string]types.Partition)
	for _, p := range newAssignment.Partitions {
		newPartMap[p.Keys[0]] = p
	}

	var added, removed []types.Partition
	for key, p := range newPartMap {
		if _, exists := oldPartMap[key]; !exists {
			added = append(added, p)
		}
	}
	for key, p := range oldPartMap {
		if _, exists := newPartMap[key]; !exists {
			removed = append(removed, p)
		}
	}

	t.Logf("Rebalance resulted in %d added, %d removed partitions", len(added), len(removed))

	err = helper.UpdateSubscriptions(ctx, added, removed, func(msg *nats.Msg) {})
	require.NoError(t, err, "Failed to update subscriptions")

	// Verify subscription count matches new assignment
	newActive := helper.ActivePartitions()
	t.Logf("New active subscriptions: %d partitions", len(newActive))
	require.Equal(t, len(newAssignment.Partitions), len(newActive), "Subscription count mismatch after rebalance")

	t.Log("Test passed - subscriptions updated correctly on rebalance")
}

func TestSubscriptionHelper_Cleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	// Setup embedded NATS
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create subscription helper
	helper := subscription.NewHelper(conn, subscription.Config{
		Stream:            "test-stream",
		MaxRetries:        3,
		RetryBackoff:      100 * time.Millisecond,
		ReconcileInterval: 5 * time.Second,
	})

	// Create test partitions
	partitions := []types.Partition{
		{Keys: []string{"partition-001"}, Weight: 100},
		{Keys: []string{"partition-002"}, Weight: 100},
		{Keys: []string{"partition-003"}, Weight: 100},
		{Keys: []string{"partition-004"}, Weight: 100},
		{Keys: []string{"partition-005"}, Weight: 100},
	}

	// Create subscriptions
	t.Log("Creating subscriptions for 5 partitions...")
	err := helper.UpdateSubscriptions(ctx, partitions, nil, func(msg *nats.Msg) {})
	require.NoError(t, err, "Failed to create subscriptions")

	activePartitions := helper.ActivePartitions()
	require.Len(t, activePartitions, 5, "Expected 5 active subscriptions")

	// Test: Close helper and verify all subscriptions are cleaned up
	t.Log("Closing helper and verifying cleanup...")
	err = helper.Close()
	require.NoError(t, err, "Failed to close helper")

	// Verify all subscriptions are removed
	activePartitions = helper.ActivePartitions()
	require.Len(t, activePartitions, 0, "Expected 0 active subscriptions after close")

	t.Log("Test passed - all subscriptions cleaned up successfully")
}

func TestSubscriptionHelper_ErrorHandling(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	// Setup embedded NATS
	srv, conn := partitest.StartEmbeddedNATS(t)
	defer srv.Shutdown()
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Test: Retry logic on subscription failures
	t.Log("Test 1: Verify retry logic handles subscription creation")

	helper := subscription.NewHelper(conn, subscription.Config{
		Stream:            "test-stream",
		MaxRetries:        3,
		RetryBackoff:      50 * time.Millisecond,
		ReconcileInterval: 5 * time.Second,
	})
	defer helper.Close()

	partitions := []types.Partition{
		{Keys: []string{"partition-001"}, Weight: 100},
	}

	// Should succeed even with retries configured
	err := helper.UpdateSubscriptions(ctx, partitions, nil, func(msg *nats.Msg) {})
	require.NoError(t, err, "Subscription should succeed")

	activePartitions := helper.ActivePartitions()
	require.Len(t, activePartitions, 1, "Expected 1 active subscription")

	// Test: Context cancellation
	t.Log("Test 2: Verify context cancellation is respected")

	cancelCtx, cancelFunc := context.WithCancel(context.Background())
	cancelFunc() // Cancel immediately

	morePartitions := []types.Partition{
		{Keys: []string{"partition-002"}, Weight: 100},
	}

	err = helper.UpdateSubscriptions(cancelCtx, morePartitions, nil, func(msg *nats.Msg) {})
	require.Error(t, err, "Should fail with cancelled context")
	require.Equal(t, context.Canceled, err, "Error should be context.Canceled")

	// Verify no new subscriptions were added
	activePartitions = helper.ActivePartitions()
	require.Len(t, activePartitions, 1, "Should still have 1 active subscription")

	// Test: Empty partition keys handling
	t.Log("Test 3: Verify empty partition keys are handled gracefully")

	emptyPartitions := []types.Partition{
		{Keys: []string{}, Weight: 100}, // Empty keys
		{Keys: []string{"partition-003"}, Weight: 100},
	}

	err = helper.UpdateSubscriptions(ctx, emptyPartitions, nil, func(msg *nats.Msg) {})
	require.NoError(t, err, "Should handle empty keys gracefully")

	// Should only have partition-001 and partition-003 (skipped empty keys)
	activePartitions = helper.ActivePartitions()
	require.Len(t, activePartitions, 2, "Expected 2 active subscriptions (skipped empty keys)")

	// Test: Duplicate subscription attempts
	t.Log("Test 4: Verify duplicate subscriptions are handled")

	duplicatePartitions := []types.Partition{
		{Keys: []string{"partition-001"}, Weight: 100}, // Already exists
	}

	err = helper.UpdateSubscriptions(ctx, duplicatePartitions, nil, func(msg *nats.Msg) {})
	require.NoError(t, err, "Should handle duplicates gracefully")

	// Should still have 2 active subscriptions (no duplicates added)
	activePartitions = helper.ActivePartitions()
	require.Len(t, activePartitions, 2, "Should not create duplicate subscriptions")

	t.Log("Test passed - error handling works correctly")
}
