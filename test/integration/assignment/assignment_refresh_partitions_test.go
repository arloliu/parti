package assignment_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/stretchr/testify/require"
)

func TestRefreshPartitions_Addition(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	env := newTestEnv(t, 90*time.Second)

	initialPartitions := makePartitions(50, 0)
	partitionSource := source.NewStatic(initialPartitions)

	cfg := createTestConfig()
	managers := env.SetupManagers(t, cfg, partitionSource, strategy.NewRoundRobin(), 3)

	t.Log("Starting 3 managers with 50 partitions...")
	startManagers(t, env.Ctx, managers)
	waitForStable(t, env.Ctx, managers)
	verifyAssignments(t, managers, 50)

	// Add 20 more partitions (50 → 70)
	t.Log("Adding 20 more partitions (50 -> 70)...")
	allPartitions := makePartitions(70, 0)
	err := partitionSource.Update(env.Ctx, allPartitions)
	require.NoError(t, err)

	refreshAndWait(t, managers)
	verifyAssignments(t, managers, 70)

	t.Log("Test completed successfully: 50 -> 70 partitions, rebalancing triggered by RefreshPartitions()")
}

func TestRefreshPartitions_Removal(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	env := newTestEnv(t, 90*time.Second)

	partitionSource := source.NewStatic(makePartitions(100, 0))

	cfg := createTestConfig()
	managers := env.SetupManagers(t, cfg, partitionSource, strategy.NewRoundRobin(), 3)

	t.Log("Starting 3 managers with 100 partitions...")
	startManagers(t, env.Ctx, managers)
	waitForStable(t, env.Ctx, managers)
	verifyAssignments(t, managers, 100)

	// Remove 30 partitions (keep 0-69)
	t.Log("Removing 30 partitions (100 -> 70)...")
	err := partitionSource.Update(env.Ctx, makePartitions(70, 0))
	require.NoError(t, err)

	refreshAndWait(t, managers)
	verifyAssignments(t, managers, 70)

	// Verify removed partitions are gone
	assignedPartitions := make(map[string]bool)
	var removedStillAssigned []string

	for _, mgr := range managers {
		for _, p := range mgr.CurrentAssignment().Partitions {
			partKey := fmt.Sprintf("%v", p.Keys)
			var partNum int
			_, _ = fmt.Sscanf(partKey, "[partition %d]", &partNum)
			if partNum >= 70 && partNum < 100 {
				removedStillAssigned = append(removedStillAssigned, partKey)
			}

			assignedPartitions[partKey] = true
		}
	}

	require.Empty(t, removedStillAssigned, "Removed partitions still assigned: %v", removedStillAssigned)

	for i := 0; i < 70; i++ {
		partKey := fmt.Sprintf("[partition %03d]", i)
		require.True(t, assignedPartitions[partKey], "Partition %s not assigned", partKey)
	}

	t.Log("Test completed successfully: 100 -> 70 partitions, removed partitions no longer assigned")
}

func TestRefreshPartitions_WeightChange(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	env := newTestEnv(t, 90*time.Second)

	partitionSource := source.NewStatic(makePartitions(60, 100))

	cfg := createTestConfig()
	const numManagers = 3
	managers := env.SetupManagers(t, cfg, partitionSource, strategy.NewConsistentHash(), numManagers)

	t.Log("Starting 3 managers with 60 partitions (all weight 100)...")
	startManagers(t, env.Ctx, managers)
	waitForStable(t, env.Ctx, managers)
	verifyAssignments(t, managers, 60)

	// Record initial distribution
	initialDist := make([]int, numManagers)
	for i, mgr := range managers {
		initialDist[i] = len(mgr.CurrentAssignment().Partitions)
	}

	// Change weights: first 30 → weight 200, last 30 → weight 100
	t.Log("Changing weights: first 30 → weight 200, last 30 → weight 100...")
	weightedPartitions := makePartitions(60, 100)
	for i := 0; i < 30; i++ {
		weightedPartitions[i].Weight = 200
	}

	err := partitionSource.Update(env.Ctx, weightedPartitions)
	require.NoError(t, err)

	refreshAndWait(t, managers)
	verifyAssignments(t, managers, 60)

	for i, mgr := range managers {
		t.Logf("Manager %d: %d partitions assigned (was %d)",
			i, len(mgr.CurrentAssignment().Partitions), initialDist[i])
	}

	t.Log("Test completed successfully: Weight change (30 partitions: 100->200) processed correctly")
}

func TestRefreshPartitions_Cooldown(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	env := newTestEnv(t, 90*time.Second)

	partitionSource := source.NewStatic(makePartitions(40, 0))

	// Longer cooldown for this test
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
		ColdStartWindow:       4 * time.Second,
		PlannedScaleWindow:    3 * time.Second,
		RestartDetectionRatio: 0.5,
		RebalanceCooldown:     3 * time.Second,
	}

	managers := env.SetupManagers(t, cfg, partitionSource, strategy.NewRoundRobin(), 2)

	t.Log("Starting 2 managers with 40 partitions...")
	startManagers(t, env.Ctx, managers)
	waitForStable(t, env.Ctx, managers)
	verifyAssignments(t, managers, 40)

	leader := findLeader(t, managers)
	initialVer := maxVersion(managers)

	// Call RefreshPartitions twice rapidly (manual refresh bypasses cooldown)
	t.Log("Calling RefreshPartitions() twice rapidly (should bypass cooldown)...")

	refreshCtx1, cancel1 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel1()

	err := leader.RefreshPartitions(refreshCtx1)
	require.NoError(t, err, "First RefreshPartitions failed")
	t.Log("First RefreshPartitions() succeeded")

	time.Sleep(100 * time.Millisecond) // Brief delay to ensure first call processed

	refreshCtx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()

	err = leader.RefreshPartitions(refreshCtx2)
	require.NoError(t, err, "Second RefreshPartitions failed (manual refresh should bypass cooldown)")
	t.Log("Second RefreshPartitions() succeeded (cooldown bypassed as expected)")

	waitForVersionChange(t, managers, initialVer)
	verifyAssignments(t, managers, 40)

	t.Log("Test completed successfully: Rapid RefreshPartitions() calls handled correctly")
}
