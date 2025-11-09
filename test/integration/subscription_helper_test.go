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
	"github.com/arloliu/parti/subscription"
	"github.com/arloliu/parti/test/testutil"
	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
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

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Create stream for WorkerConsumer
	js, err := jetstream.New(conn)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "test-stream", Subjects: []string{"test.sub.>"}})
	require.NoError(t, err)

	// Create durable helper (single-consumer)
	helper, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
		StreamName:      "test-stream",
		ConsumerPrefix:  "worker",
		SubjectTemplate: "test.sub.{{.PartitionID}}",
		MaxRetries:      3,
		RetryBackoff:    100 * time.Millisecond,
	}, subscription.MessageHandlerFunc(func(c context.Context, m jetstream.Msg) error { return nil }))
	require.NoError(t, err)
	defer helper.Close(context.Background())

	// Create test partitions
	partitions := []types.Partition{
		{Keys: []string{"partition-001"}, Weight: 100},
		{Keys: []string{"partition-002"}, Weight: 100},
		{Keys: []string{"partition-003"}, Weight: 100},
	}

	// Test: Create subjects for all partitions via worker consumer
	t.Log("Creating worker consumer subjects for 3 partitions...")
	err = helper.UpdateWorkerConsumer(ctx, "worker-creation", partitions)
	require.NoError(t, err, "Failed to update worker consumer")

	// Verify all subjects are present
	subjects := helper.WorkerSubjects()
	require.Len(t, subjects, 3, "Expected 3 subjects")
	expected := map[string]bool{
		"test.sub.partition-001": true,
		"test.sub.partition-002": true,
		"test.sub.partition-003": true,
	}
	for _, s := range subjects {
		require.True(t, expected[s], "Unexpected subject: %s", s)
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

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
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
		PlannedScaleWindow:    2 * time.Second, // Must be >= MinRebalanceInterval
		RestartDetectionRatio: 0.5,
		Assignment: parti.AssignmentConfig{
			MinRebalanceThreshold: 0.15,
			MinRebalanceInterval:  2 * time.Second,
		},
	}

	// Create 2 managers
	const numManagers = 2
	managers := make([]*parti.Manager, numManagers)

	js, err := jetstream.New(conn)
	require.NoError(t, err)
	for i := 0; i < numManagers; i++ {
		manager, err := parti.NewManager(cfg, js, partitionSource, strategy.NewRoundRobin(), parti.WithLogger(debugLogger))
		require.NoError(t, err, "Failed to create manager %d", i)
		managers[i] = manager
	}

	// Cleanup function for all managers
	cleanupManagers := func() {
		for i, manager := range managers {
			if manager != nil {
				stopCtx, stopCancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer stopCancel() //nolint:revive
				if err := manager.Stop(stopCtx); err != nil {
					t.Logf("Failed to stop manager %d: %v", i, err)
				}
			}
		}
	}
	defer cleanupManagers()

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
	err = testutil.WaitAllManagersState(ctx, managerWaiters, types.StateStable, 20*time.Second)
	require.NoError(t, err, "Managers failed to reach Stable state")

	// Create stream and durable helper for first manager (reuse js)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "test-stream", Subjects: []string{"rebalance.sub.>"}})
	require.NoError(t, err)
	helper, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
		StreamName:      "test-stream",
		ConsumerPrefix:  "worker",
		SubjectTemplate: "rebalance.sub.{{.PartitionID}}",
		MaxRetries:      3,
		RetryBackoff:    100 * time.Millisecond,
	}, subscription.MessageHandlerFunc(func(c context.Context, m jetstream.Msg) error { return nil }))
	require.NoError(t, err)
	defer helper.Close(context.Background())

	// Get initial assignment and create worker subjects
	t.Log("Creating worker subjects for initial assignment...")
	initialAssignment := managers[0].CurrentAssignment()
	err = helper.UpdateWorkerConsumer(ctx, "worker-rebal", initialAssignment.Partitions)
	require.NoError(t, err, "Failed to apply initial worker subjects")

	initialSubjects := helper.WorkerSubjects()
	t.Logf("Initial subjects: %d partitions", len(initialSubjects))
	require.Equal(t, len(initialAssignment.Partitions), len(initialSubjects), "Subject count mismatch")

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

	refreshCtx, refreshCancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer refreshCancel()
	err = leader.RefreshPartitions(refreshCtx)
	require.NoError(t, err, "RefreshPartitions failed")

	// Wait for stabilization
	time.Sleep(3 * time.Second)
	err = testutil.WaitAllManagersState(ctx, managerWaiters, types.StateStable, 10*time.Second)
	require.NoError(t, err, "Failed to reach Stable after rebalance")

	// Get new assignment and update worker consumer subjects
	t.Log("Updating worker subjects based on new assignment...")
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

	err = helper.UpdateWorkerConsumer(ctx, "worker-rebal", newAssignment.Partitions)
	require.NoError(t, err, "Failed to update worker subjects")

	// Verify subject count matches new assignment
	newSubjects := helper.WorkerSubjects()
	t.Logf("New subjects: %d partitions", len(newSubjects))
	require.Equal(t, len(newAssignment.Partitions), len(newSubjects), "Subject count mismatch after rebalance")

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

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Create stream and durable helper
	js, err := jetstream.New(conn)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "test-stream", Subjects: []string{"cleanup.sub.>"}})
	require.NoError(t, err)
	helper, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
		StreamName:      "test-stream",
		ConsumerPrefix:  "worker",
		SubjectTemplate: "cleanup.sub.{{.PartitionID}}",
		MaxRetries:      3,
		RetryBackoff:    100 * time.Millisecond,
	}, subscription.MessageHandlerFunc(func(c context.Context, m jetstream.Msg) error { return nil }))
	require.NoError(t, err)

	// Create test partitions
	partitions := []types.Partition{
		{Keys: []string{"partition-001"}, Weight: 100},
		{Keys: []string{"partition-002"}, Weight: 100},
		{Keys: []string{"partition-003"}, Weight: 100},
		{Keys: []string{"partition-004"}, Weight: 100},
		{Keys: []string{"partition-005"}, Weight: 100},
	}

	// Create subjects
	t.Log("Creating subjects for 5 partitions...")
	err = helper.UpdateWorkerConsumer(ctx, "worker-clean", partitions)
	require.NoError(t, err, "Failed to create subjects")

	subjects := helper.WorkerSubjects()
	require.Len(t, subjects, 5, "Expected 5 subjects")

	// Test: Close helper and verify internal state cleanup (does not delete consumers)
	t.Log("Closing helper and verifying cleanup...")
	err = helper.Close(context.Background())
	require.NoError(t, err, "Failed to close helper")

	// Verify internal subjects cleared
	subjects = helper.WorkerSubjects()
	require.Len(t, subjects, 0, "Expected 0 subjects after close")

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

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Create stream and helper
	js, err := jetstream.New(conn)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "test-stream", Subjects: []string{"err.sub.>"}})
	require.NoError(t, err)
	helper, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
		StreamName:      "test-stream",
		ConsumerPrefix:  "worker",
		SubjectTemplate: "err.sub.{{.PartitionID}}",
		MaxRetries:      3,
		RetryBackoff:    50 * time.Millisecond,
	}, subscription.MessageHandlerFunc(func(c context.Context, m jetstream.Msg) error { return nil }))
	require.NoError(t, err)
	defer helper.Close(context.Background())

	partitions := []types.Partition{
		{Keys: []string{"partition-001"}, Weight: 100},
	}

	// Should succeed even with retries configured
	err = helper.UpdateWorkerConsumer(ctx, "worker-err", partitions)
	require.NoError(t, err, "UpdateWorkerConsumer should succeed")

	subjects := helper.WorkerSubjects()
	require.Len(t, subjects, 1, "Expected 1 subject")

	// Test: Context cancellation
	t.Log("Test 2: Verify context cancellation is respected")

	cancelCtx, cancelFunc := context.WithCancel(t.Context())
	cancelFunc() // Cancel immediately

	morePartitions := []types.Partition{
		{Keys: []string{"partition-002"}, Weight: 100},
	}

	err = helper.UpdateWorkerConsumer(cancelCtx, "worker-err", morePartitions)
	require.Error(t, err, "Should fail with cancelled context")
	require.Equal(t, context.Canceled, err, "Error should be context.Canceled")

	// Verify no new subjects were added
	subjects = helper.WorkerSubjects()
	require.Len(t, subjects, 1, "Should still have 1 subject")

	// Test: Empty partition keys handling
	t.Log("Test 3: Verify empty partition keys are handled gracefully")

	emptyPartitions := []types.Partition{
		{Keys: []string{}, Weight: 100}, // Empty keys
		{Keys: []string{"partition-003"}, Weight: 100},
	}

	err = helper.UpdateWorkerConsumer(ctx, "worker-err", emptyPartitions)
	require.Error(t, err, "Should error on empty keys")

	// Should still have 1 subject from earlier
	subjects = helper.WorkerSubjects()
	require.Len(t, subjects, 1, "Expected 1 subject (invalid update rejected)")

	// Test: Duplicate subscription attempts
	t.Log("Test 4: Verify duplicate subscriptions are handled")

	duplicatePartitions := []types.Partition{
		{Keys: []string{"partition-001"}, Weight: 100}, // Already exists
	}

	err = helper.UpdateWorkerConsumer(ctx, "worker-err", duplicatePartitions)
	require.NoError(t, err, "Should handle duplicates gracefully")

	// Should still have 1 subject (no duplicates added)
	subjects = helper.WorkerSubjects()
	require.Len(t, subjects, 1, "Should not create duplicate subjects")

	t.Log("Test passed - error handling works correctly")
}
