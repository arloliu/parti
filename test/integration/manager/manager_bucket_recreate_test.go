package manager_test

import (
	"context"
	"errors"
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

// TestManager_Restart_AfterNATSBucketLoss is the regression test for the
// edge case where NATS JetStream loses all data (e.g., a non-replicated
// JetStream node's PVC is wiped, or a single-node deployment is restarted
// with ephemeral storage) and applications are subsequently restarted.
//
// The contract being verified: Parti's manager startup is idempotent against
// missing KV buckets. Each bucket is opened through kvutil.EnsureKVBucketWithRetry
// which falls back to CreateKeyValue when the bucket doesn't exist, so a
// fresh startup on an empty NATS must succeed just as it would on a
// first-ever deployment.
//
// Scenario:
//  1. Cluster of 3 workers starts on empty NATS; leader elected, assignments
//     published, cluster reaches Stable.
//  2. All workers are stopped gracefully (simulating a pod terminate).
//  3. All Parti-managed KV buckets are deleted (simulating NATS data loss
//     during the restart window — stream contents are gone, user message
//     streams would also be recreated by user code, but we focus on the
//     Parti-managed buckets here).
//  4. A brand-new set of 3 workers starts with the same config on the
//     now-empty NATS. Each goes through ensureKVBucket → CreateKeyValue for
//     every bucket.
//
// Invariant: all three new workers reach Stable, exactly one leader is
// elected, every partition is assigned, and no worker's Start() returns an
// error.
func TestManager_Restart_AfterNATSBucketLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	partitions := testutil.CreateTestPartitions(10)
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	// Phase 1: bring up an initial cluster on empty NATS.
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
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(15 * time.Second)
	_ = cluster.VerifyExactlyOneLeader()
	t.Log("phase 1: initial cluster stable")

	// Phase 2: stop all workers cleanly. This releases leadership, heartbeats,
	// and worker IDs, but leaves the KV buckets themselves intact.
	cluster.StopWorkers()
	t.Log("phase 2: initial cluster stopped")

	// Phase 3: wipe all Parti-managed KV buckets, simulating NATS data loss.
	// js.DeleteKeyValue is the same surface an operator would see after a
	// JetStream storage wipe: buckets simply no longer exist.
	bucketsToWipe := []string{
		cfg.KVBuckets.StableIDBucket,
		cfg.KVBuckets.ElectionBucket,
		cfg.KVBuckets.HeartbeatBucket,
		cfg.KVBuckets.AssignmentBucket,
	}
	// HandoffBucket is only provisioned when two-phase handoff is enabled; the
	// default IntegrationTestConfig leaves it disabled, so skip it unless set.
	if cfg.EnableTwoPhaseHandoff && cfg.KVBuckets.HandoffBucket != "" {
		bucketsToWipe = append(bucketsToWipe, cfg.KVBuckets.HandoffBucket)
	}
	for _, bucket := range bucketsToWipe {
		if err := js.DeleteKeyValue(ctx, bucket); err != nil &&
			!errors.Is(err, jetstream.ErrBucketNotFound) {
			t.Fatalf("failed to wipe bucket %s: %v", bucket, err)
		}
	}
	t.Logf("phase 3: wiped %d Parti-managed buckets", len(bucketsToWipe))

	// Confirm the wipe actually happened — a Get on any bucket should now
	// return ErrBucketNotFound. Guards against accidentally passing this test
	// if DeleteKeyValue silently no-oped.
	_, err = js.KeyValue(ctx, cfg.KVBuckets.AssignmentBucket)
	require.Error(t, err, "assignment bucket should be gone after wipe")
	require.ErrorIs(t, err, jetstream.ErrBucketNotFound)

	// Phase 4: start a fresh cluster on the now-empty NATS. These managers
	// have no knowledge of the prior cluster; every bucket lookup will hit
	// ErrBucketNotFound and fall through to CreateKeyValue.
	cluster2 := &testutil.WorkerCluster{
		Workers:  make([]*parti.Manager, 0),
		Config:   cfg,
		Source:   src,
		Strategy: assignStrat,
		NC:       nc,
		JS:       js,
		T:        t,
	}
	for range 3 {
		cluster2.AddWorker(ctx)
	}
	defer cluster2.StopWorkers()

	cluster2.StartWorkers(ctx)
	cluster2.WaitForStableState(15 * time.Second)
	t.Log("phase 4: replacement cluster stable on recreated buckets")

	// Invariants on the replacement cluster.
	require.NotNil(t, cluster2.VerifyExactlyOneLeader())
	require.True(t, cluster2.WaitForAllWorkersHavePartitions(10*time.Second),
		"every worker should have at least one partition after bucket recreation")

	// Spot-check: every bucket now exists again, confirming ensureKVBucket
	// went through the CreateKeyValue branch rather than erroring out.
	for _, bucket := range bucketsToWipe {
		_, err := js.KeyValue(ctx, bucket)
		require.NoError(t, err, "bucket %s should have been recreated during startup", bucket)
	}

	// The new leader's assignment version starts fresh from 0 (no prior
	// assignments to discover), so the first publish increments to version 1.
	// This confirms DiscoverHighestVersion handled the empty-bucket case.
	var leaderAssignment types.Assignment
	for _, mgr := range cluster2.GetActiveWorkers() {
		if mgr.IsLeader() {
			leaderAssignment = mgr.CurrentAssignment()
			break
		}
	}
	require.Greater(t, leaderAssignment.Version, int64(0),
		"leader should have published at least one assignment after bucket recreation")
}
