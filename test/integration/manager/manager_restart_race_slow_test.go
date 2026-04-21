package manager_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestRestart_LeaderTakeoverRace_ProductionTimings is a slow-running variant
// of TestRestart_LeaderTakeoverRace that uses production-scale timings
// (ColdStartWindow=15s) rather than the scaled-down 5s used in the fast test.
//
// The race that was reported in production manifests at production timings
// regardless of the scaling ratio, but running with realistic windows is the
// only way to catch regressions that only show up at real-world durations
// (e.g. a future change that depends on the absolute window length rather
// than the ratio). The fast variant protects against the common case; this
// slow variant protects against the bug the user actually reported.
//
// Gated behind the PARTI_SLOW_TESTS env var so it doesn't add ~20s to every
// CI run. Set PARTI_SLOW_TESTS=1 to enable.
func TestRestart_LeaderTakeoverRace_ProductionTimings(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if os.Getenv("PARTI_SLOW_TESTS") == "" {
		t.Skip("slow test — set PARTI_SLOW_TESTS=1 to enable")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Production-scale timings. ColdStartWindow is large; the replacement's
	// ctx is smaller than ColdStartWindow but much larger than
	// PlannedScaleWindow. After the fix the replacement must land within
	// ~PlannedScaleWindow seconds of takeover.
	const (
		coldStartWindow    = 15 * time.Second
		plannedScaleWindow = 3 * time.Second
		replacementTimeout = 10 * time.Second // < coldStartWindow, > plannedScaleWindow
		electionTimeout    = 2 * time.Second
	)

	cfg := parti.Config{
		WorkerIDPrefix:        "worker",
		WorkerIDMin:           0,
		WorkerIDMax:           20,
		WorkerIDTTL:           30 * time.Second,
		HeartbeatInterval:     500 * time.Millisecond,
		HeartbeatTTL:          3 * time.Second,
		ElectionTimeout:       electionTimeout,
		StartupTimeout:        60 * time.Second,
		ShutdownTimeout:       5 * time.Second,
		ColdStartWindow:       coldStartWindow,
		PlannedScaleWindow:    plannedScaleWindow,
		RestartDetectionRatio: 0.5,
		RebalanceCooldown:     1 * time.Second,
	}
	require.NoError(t, parti.SetDefaults(&cfg))

	partitions := testutil.CreateTestPartitions(12)
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
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
	cluster.WaitForStableState(60 * time.Second)
	originalLeader := cluster.VerifyExactlyOneLeader()
	originalLeaderID := originalLeader.WorkerID()

	leaderIndex := -1
	for i, mgr := range cluster.GetWorkers() {
		if mgr.WorkerID() == originalLeaderID {
			leaderIndex = i
			break
		}
	}
	require.NotEqual(t, -1, leaderIndex)

	cluster.RemoveWorker(leaderIndex)

	// Wait for follower takeover and the new leader's post-takeover Scaling
	// state (indicating cold_start_immediate has published).
	var newLeader *parti.Manager
	require.Eventually(t, func() bool {
		for _, mgr := range cluster.GetActiveWorkers() {
			if mgr.IsLeader() {
				newLeader = mgr
				return true
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond)
	require.Eventually(t, func() bool {
		return newLeader.State() == parti.StateScaling
	}, 5*time.Second, 100*time.Millisecond)

	// Simulate emergency-rebalance cleanup of the dead leader's assignment
	// key (heartbeat-TTL-expiry path in production).
	assignmentBucket := cfg.KVBuckets.AssignmentBucket
	if assignmentBucket == "" {
		assignmentBucket = "parti-assignment"
	}
	assignmentKV, err := js.KeyValue(ctx, assignmentBucket)
	require.NoError(t, err)
	_ = assignmentKV.Delete(ctx, "assignment."+originalLeaderID)

	takeoverStart := time.Now()
	replacementMgr, err := parti.NewManager(&cfg, js, src, assignStrat)
	require.NoError(t, err)

	startCtx, startCancel := context.WithTimeout(context.Background(), replacementTimeout)
	defer startCancel()
	err = replacementMgr.Start(startCtx)
	elapsed := time.Since(takeoverStart)
	t.Logf("replacement Start returned after %s: %v", elapsed, err)

	// At production timings, the fix must ensure the replacement lands within
	// ~PlannedScaleWindow+headroom. Before the fix it would have waited for
	// ColdStartWindow (15s) and exceeded replacementTimeout (10s).
	require.NoError(t, err, "replacement Start must succeed under production timings after the takeover fix")
	require.Less(t, elapsed, plannedScaleWindow+3*time.Second,
		"replacement should land within ~PlannedScaleWindow of takeover, not ColdStartWindow")
	require.NotEmpty(t, replacementMgr.CurrentAssignment().Partitions)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	_ = replacementMgr.Stop(stopCtx)
}
