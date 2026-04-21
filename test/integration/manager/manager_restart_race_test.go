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

// TestRestart_LeaderTakeoverRace is the regression test for the user-reported
// hang where a replacement pod, starting during a new leader's stabilization
// window after a leader takeover, could not acquire its initial assignment
// before its startup deadline expired.
//
// The user's production scenario uses parti defaults: ColdStartWindow=30s with
// a 15s ctx passed to Start(ctx). This test preserves the same ratio (startup
// deadline < ColdStartWindow) at scaled-down timings so the race reproduces
// deterministically in CI.
//
// Reproduction chain (the real path is emergency-rebalance-driven; we simulate
// the cleanup step manually to keep the test fast and deterministic):
//  1. Terminate a leader pod (graceful Stop releases leadership+heartbeat but
//     leaves its assignment.<id> key in KV).
//  2. An existing follower takes over and enters its stabilization window.
//  3. In production: the terminated pod's heartbeat TTL expires, the new
//     leader runs an emergency rebalance, and that rebalance's workersToRemove
//     cleanup deletes the dead pod's assignment.<id> key. In this test we
//     delete the key explicitly to avoid waiting for TTL expiry.
//  4. Replacement pod starts with a ctx shorter than the leader's
//     stabilization window.
//
// The fix was structural: calc.Start detects whether this is a genuine cold
// start (no existing assignments) or a takeover (discoverHighestVersion > 0).
// On takeover it uses PlannedScaleWindow instead of ColdStartWindow, so a new
// worker arriving immediately after takeover is picked up within seconds, not
// tens of seconds.
func TestRestart_LeaderTakeoverRace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Scaled config that mirrors the production defaults ratio.
	// User: StartupTimeout=30s, ColdStartWindow=30s, passes ctx with 15s timeout.
	// Here: ColdStartWindow=5s, replacement Start ctx=3s. Replacement's deadline
	// expires before leader's cold-start rebalance fires.
	const (
		coldStartWindow    = 5 * time.Second
		replacementTimeout = 3 * time.Second
		electionTimeout    = 1 * time.Second
	)

	cfg := parti.Config{
		WorkerIDPrefix:        "worker",
		WorkerIDMin:           0,
		WorkerIDMax:           20,
		WorkerIDTTL:           10 * time.Second,
		HeartbeatInterval:     200 * time.Millisecond,
		HeartbeatTTL:          2 * time.Second,
		ElectionTimeout:       electionTimeout,
		StartupTimeout:        30 * time.Second, // large; we rely on ctx deadline instead
		ShutdownTimeout:       3 * time.Second,
		ColdStartWindow:       coldStartWindow,
		PlannedScaleWindow:    500 * time.Millisecond,
		RestartDetectionRatio: 0.5,
		RebalanceCooldown:     500 * time.Millisecond,
	}
	require.NoError(t, parti.SetDefaults(&cfg))

	partitions := testutil.CreateTestPartitions(10)
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Start 3 workers concurrently and wait for the cluster to stabilize.
	// This matches the user's "first round all pods start normal" observation.
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
	t.Logf("initial leader: %s", originalLeaderID)

	// Find the leader's index and terminate it — simulates `kubectl delete pod`.
	leaderIndex := -1
	for i, mgr := range cluster.GetWorkers() {
		if mgr.WorkerID() == originalLeaderID {
			leaderIndex = i
			break
		}
	}
	require.NotEqual(t, -1, leaderIndex)

	t.Logf("terminating leader worker-%d", leaderIndex)
	terminationAt := time.Now()
	cluster.RemoveWorker(leaderIndex)

	// Wait for an existing follower to take over leadership AND complete its
	// cold_start_immediate (indicated by its transition into Scaling state).
	// Without this wait, two other paths would mask the race:
	//   1. The replacement itself could win the empty election key before any
	//      follower polls for it.
	//   2. The replacement's heartbeat could be visible to the new leader's
	//      cold_start_immediate read, causing that publish to include it.
	var newLeader *parti.Manager
	require.Eventually(t, func() bool {
		for _, mgr := range cluster.GetActiveWorkers() {
			if mgr.IsLeader() {
				newLeader = mgr
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "no new leader elected from remaining followers")
	t.Logf("new leader: %s (takeover %s after termination)",
		newLeader.WorkerID(), time.Since(terminationAt))

	// Give the new leader time to finish cold_start_immediate. Once it emits
	// Scaling state to followers, its initial rebalance has already published.
	require.Eventually(t, func() bool {
		return newLeader.State() == parti.StateScaling
	}, 3*time.Second, 20*time.Millisecond, "new leader did not enter Scaling state")

	// Simulate the emergency-rebalance cleanup path that would happen under a
	// SIGKILL'd pod: the leader detects the dead worker's heartbeat expiry,
	// runs an emergency rebalance, and deletes the dead worker's assignment
	// key (calculator.go workersToRemove → cleanupStaleAssignments). In this
	// test the previous leader released gracefully, so its assignment key
	// remains as stale data. Deleting it explicitly reproduces the real-world
	// state when the old heartbeat actually expired.
	assignmentBucket := cfg.KVBuckets.AssignmentBucket
	if assignmentBucket == "" {
		assignmentBucket = "parti-assignment"
	}
	assignmentKV, err := js.KeyValue(ctx, assignmentBucket)
	require.NoError(t, err)
	_ = assignmentKV.Delete(ctx, "assignment."+originalLeaderID)
	t.Logf("deleted stale assignment key for %s (simulating emergency-rebalance cleanup)", originalLeaderID)

	takeoverStart := time.Now()

	// Start a replacement manager (k8s reschedules the pod after a few seconds).
	// The replacement races against the new leader's ColdStartWindow.
	replacementMgr, err := parti.NewManager(&cfg, js, src, assignStrat)
	require.NoError(t, err)

	// Pass a ctx shorter than ColdStartWindow, matching the user's 15s < 30s setup.
	startCtx, startCancel := context.WithTimeout(context.Background(), replacementTimeout)
	defer startCancel()

	startErr := replacementMgr.Start(startCtx)
	elapsed := time.Since(takeoverStart)
	t.Logf("replacement Start returned after %s: %v", elapsed, startErr)

	// After the takeover-aware fix, the new leader uses PlannedScaleWindow for
	// its post-takeover stabilization, so a new worker arriving moments later
	// is rolled into that shorter window's rebalance and acquires its
	// assignment well before the replacementTimeout.
	require.NoError(t, startErr, "replacement Start should succeed within replacementTimeout after the takeover fix")
	require.Less(t, elapsed, replacementTimeout, "replacement Start took too long")
	require.NotEmpty(t, replacementMgr.CurrentAssignment().Partitions,
		"replacement should have non-empty assignment")

	// Clean up the replacement explicitly — it's not registered with the cluster helper.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	_ = replacementMgr.Stop(stopCtx)
}
