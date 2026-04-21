package manager_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestEmptyPartitionSource verifies that a manager starts cleanly when its
// partition source is empty. The expected behaviour is:
//   - Start() returns successfully within its deadline.
//   - The worker's CurrentAssignment has zero partitions.
//   - The manager keeps running so that partitions added later can be picked up.
//
// This guards against regressions where an empty source could cause the
// leader's rebalance to misbehave (orphan detection, publish error, missing
// assignment key) and make waitForAssignment block until ctx expiry.
func TestEmptyPartitionSource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()

	emptySource := source.NewStatic([]types.Partition{})
	assignStrat := strategy.NewConsistentHash()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	// Start a single worker — it will become leader.
	mgr, err := parti.NewManager(&cfg, js, emptySource, assignStrat)
	require.NoError(t, err)

	start := time.Now()
	err = mgr.Start(ctx)
	elapsed := time.Since(start)
	t.Logf("Start returned after %s", elapsed)

	require.NoError(t, err, "Start must not hang on empty partition source")
	require.Less(t, elapsed, 10*time.Second, "Start took too long")

	// Leader should have published an empty assignment for itself.
	require.True(t, mgr.IsLeader(), "single worker should be leader")
	require.Empty(t, mgr.CurrentAssignment().Partitions,
		"assignment partitions should be empty for empty source")

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	require.NoError(t, mgr.Stop(stopCtx))
}

// TestEmptyPartitionSource_MultipleFollowers verifies followers also start
// cleanly when the source is empty — they receive an empty assignment from the
// leader rather than hanging on waitForAssignment.
func TestEmptyPartitionSource_MultipleFollowers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	emptySource := source.NewStatic([]types.Partition{})
	assignStrat := strategy.NewConsistentHash()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	cluster := &testutil.WorkerCluster{
		Workers:  make([]*parti.Manager, 0),
		Config:   cfg,
		Source:   emptySource,
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

	// Every worker has an empty assignment; exactly one is leader.
	cluster.VerifyExactlyOneLeader()
	for i, w := range cluster.GetWorkers() {
		require.Empty(t, w.CurrentAssignment().Partitions,
			"worker %d should have empty assignment", i)
	}
}
