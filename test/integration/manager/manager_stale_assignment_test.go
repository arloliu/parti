package manager_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestStaleAssignmentCleanup_OnLeaderTakeover verifies that a new leader's
// first rebalance sweeps assignment keys for workers that no longer have
// active heartbeats. Without this hygiene, a restarting pod that reclaims the
// same stable ID would read a pre-termination assignment from KV during its
// waitForAssignment poll and return from Start() with out-of-date partition
// data (rather than waiting for the new leader to publish a fresh assignment).
//
// The fix: calc.Start seeds currentWorkers from DiscoverHighestVersion's
// returned worker IDs. The first rebalance then computes workersToRemove
// correctly and cleanupStaleAssignments deletes assignment.<oldID> for any ID
// whose heartbeat is no longer present.
func TestStaleAssignmentCleanup_OnLeaderTakeover(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	// Keep the KV windows short enough that the test finishes quickly, but
	// not so short that the new leader's planned_scale window expires before
	// we finish checking.
	cfg.ColdStartWindow = 2 * time.Second
	cfg.PlannedScaleWindow = 500 * time.Millisecond
	cfg.RebalanceCooldown = 300 * time.Millisecond
	cfg.ElectionTimeout = 1 * time.Second
	cfg.HeartbeatInterval = 200 * time.Millisecond
	cfg.HeartbeatTTL = 1 * time.Second
	cfg.WorkerIDTTL = 5 * time.Second

	partitions := testutil.CreateTestPartitions(10)
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cluster := &testutil.WorkerCluster{
		Workers:  make([]*parti.Manager, 0),
		Config:   cfg,
		Source:   src,
		Strategy: assignStrat,
		NC:       nc,
		JS:       js,
		T:        t,
	}
	for range 3 {
		cluster.AddWorker(ctx)
	}
	defer cluster.StopWorkers()

	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(15 * time.Second)

	originalLeader := cluster.VerifyExactlyOneLeader()
	originalLeaderID := originalLeader.WorkerID()

	// Snapshot the assignment KV bucket name and open it.
	assignmentBucket := cfg.KVBuckets.AssignmentBucket
	if assignmentBucket == "" {
		assignmentBucket = "parti-assignment"
	}
	assignmentKV, err := js.KeyValue(ctx, assignmentBucket)
	require.NoError(t, err)

	// Confirm the old leader has an assignment key.
	key := "assignment." + originalLeaderID
	_, err = assignmentKV.Get(ctx, key)
	require.NoError(t, err, "original leader should have an assignment key")

	// Find and terminate the leader.
	leaderIndex := -1
	for i, mgr := range cluster.GetWorkers() {
		if mgr.WorkerID() == originalLeaderID {
			leaderIndex = i
			break
		}
	}
	require.NotEqual(t, -1, leaderIndex)
	cluster.RemoveWorker(leaderIndex)

	// Wait for a new leader to take over.
	require.Eventually(t, func() bool {
		for _, mgr := range cluster.GetActiveWorkers() {
			if mgr.IsLeader() {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond)

	// After the fix, the new leader's cold_start_immediate computes
	// workersToRemove={originalLeaderID} (because currentWorkers was seeded
	// with all existing assignment keys but the old leader's heartbeat is
	// already gone), then cleanupStaleAssignments deletes the stale key.
	// Poll briefly for the delete to land.
	require.Eventually(t, func() bool {
		_, err := assignmentKV.Get(ctx, key)
		return err != nil // ErrKeyNotFound means it was swept
	}, 3*time.Second, 50*time.Millisecond,
		"new leader should have swept the dead worker's stale assignment.<id> key on takeover")
}
