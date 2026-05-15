package manager_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestKVSizeLimit_AssignmentPublishFails reproduces the user-reported symptom
// where manually-created KV buckets with MaxValueSize=512KiB cause pods to
// hang on restart, while parti-auto-created buckets (unlimited) work.
//
// Hypothesis: with many partitions, the leader's per-worker Assignment JSON
// exceeds MaxValueSize. publisher.Publish fails at kv.Put; rebalance returns
// an error; calc.Start returns an error; the new leader ends up with no
// calculator and followers hang on waitForAssignment.
//
// To reproduce we pre-create the parti-assignment bucket with a tight
// MaxValueSize, then attempt to start a cluster. If the hypothesis is correct
// we should see leader Start fail (cold start) or takeover hang (restart).
func TestKVSizeLimit_AssignmentPublishFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Pre-create the assignment bucket with a tight MaxValueSize so publishes
	// exceeding it will fail. Mirrors the user's manual-bucket setup.
	const tightMaxValueSize = 4 * 1024 // 4 KiB — deliberately small
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       "parti-assignment",
		History:      1,
		MaxValueSize: tightMaxValueSize,
	})
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()

	// Build a partition list large enough that a single worker's assignment
	// JSON (if all partitions are routed to that worker) exceeds 4 KiB.
	// Each partition has a Keys slice with one ~100-byte key, so about
	// ~140 bytes of JSON per partition. 200 partitions ≈ 28 KiB — far beyond
	// the 4 KiB bucket limit for a single-worker-gets-all cold_start_immediate.
	partitions := make([]types.Partition, 200)
	for i := range partitions {
		partitions[i] = types.Partition{
			Keys:   []string{fmt.Sprintf("p-%05d-%s", i, longKeySuffix())},
			Weight: 1,
		}
	}
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()

	mgr, err := parti.NewManager(&cfg, js, src, assignStrat)
	require.NoError(t, err)

	startCtx, startCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer startCancel()

	startErr := mgr.Start(startCtx)
	t.Logf("Start returned: %v", startErr)

	// Document the observed behavior. If Start returns a clear error that
	// mentions the publish failure, callers can diagnose. If Start hangs and
	// then times out, the operator sees "context deadline exceeded" with no
	// hint about the real cause — that's the user's production experience.
	if startErr != nil {
		t.Logf("symptom: Start failed with %v — operator sees this error at startup", startErr)
	} else {
		t.Log("symptom: Start succeeded unexpectedly — either hypothesis is wrong or partitions weren't large enough")
	}

	// Clean up regardless.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	_ = mgr.Stop(stopCtx)

	// The test is deliberately assertion-light for now — it's a diagnostic
	// scaffold to confirm the failure mode. Once we know what error surface
	// we want, we can tighten it.
	require.Error(t, startErr, "hypothesis: tight MaxValueSize should cause Start to fail because publisher.Publish is rejected by NATS")
}

// longKeySuffix returns a ~100-char string so each partition's JSON is large
// enough to make 200 partitions' total serialized assignment exceed 4 KiB.
func longKeySuffix() string {
	return "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
}

// TestKVSizeLimit_LeaderReleasesLeadershipOnFailure reproduces the user's
// production scenario: initial cold-start publishes fit the bucket limit
// (because each of N workers gets P/N partitions), but after a pod dies the
// remaining N-1 workers' per-worker assignment size grows to P/(N-1) and
// exceeds the limit. The takeover's calc.Start then fails inside
// monitorLeadership.
//
// Before the fix that's a silent hang: the new leader holds the election
// lease without a working calculator; followers wait forever.
// After the fix the new leader releases leadership so either another pod
// attempts (and also fails loudly) or election cycles, making the failure
// visible cluster-wide instead of silently deadlocking.
func TestKVSizeLimit_LeaderReleasesLeadershipOnFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()

	// Start the assignment bucket generously sized so cold start succeeds.
	// We'll shrink MaxValueSize after the cluster is stable to force the
	// takeover-time publish failure.
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       "parti-assignment",
		History:      1,
		MaxValueSize: 1024 * 1024, // 1 MiB initially — roomy
	})
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	cfg.ElectionTimeout = 1 * time.Second

	// Partitions are large: each ~140 bytes of JSON. 60 partitions ≈ 8.4 KiB
	// if assigned to one worker. After shrink to 4 KiB, any per-worker
	// assignment that tries to hold more than ~28 partitions fails.
	partitions := make([]types.Partition, 60)
	for i := range partitions {
		partitions[i] = types.Partition{
			Keys:   []string{fmt.Sprintf("p-%05d-%s", i, longKeySuffix())},
			Weight: 1,
		}
	}
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()

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
		cluster.AddWorker(ctx, logging.NewNop())
	}
	defer cluster.StopWorkers()

	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(15 * time.Second)

	// Now shrink MaxValueSize so any post-takeover rebalance fails. Phase 3
	// of the assignment protocol switched to refs-always commits where each
	// per-worker payload key is its own (small) KV entry, so the per-worker
	// 4 KiB limit no longer suffices — but a tight 128-byte limit still
	// causes both the payload writes AND the commit CAS to fail, which is
	// what this test cares about (any publish failure must surrender
	// leadership).
	_, err = js.UpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       "parti-assignment",
		History:      1,
		MaxValueSize: 128,
	})
	require.NoError(t, err)
	t.Log("shrunk MaxValueSize to 128 bytes; takeover publish will fail at the payload write step")
	originalLeader := cluster.VerifyExactlyOneLeader()
	t.Logf("initial leader: %s", originalLeader.WorkerID())

	// Kill leader. Remaining 2 workers try to publish, but their per-worker
	// assignment now exceeds 4 KiB. Whichever follower takes over will fail
	// calc.Start.
	leaderIndex := -1
	for i, mgr := range cluster.GetWorkers() {
		if mgr.WorkerID() == originalLeader.WorkerID() {
			leaderIndex = i
			break
		}
	}
	require.NotEqual(t, -1, leaderIndex)

	// Snapshot leadership trackers for the two survivors BEFORE removal so
	// we can count leadership transitions on each after they take over.
	survivorTrackers := make([]*testutil.LeadershipTracker, 0, len(cluster.LeadershipTrackers)-1)
	for i, lt := range cluster.LeadershipTrackers {
		if i == leaderIndex {
			continue
		}
		survivorTrackers = append(survivorTrackers, lt)
	}
	require.Len(t, survivorTrackers, 2)

	cluster.RemoveWorker(leaderIndex)

	// Wait for at least one survivor to claim leadership (OnLeadershipChanged
	// fires with isLeader=true). Without takeover happening, the test is
	// meaningless.
	require.Eventually(t, func() bool {
		for _, lt := range survivorTrackers {
			if lt.Count() > 0 {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond,
		"no survivor ever attempted leadership takeover")

	// Contract: after calc.Start fails for a takeover leader, that leader
	// must release leadership. The LeadershipTracker will therefore record
	// a true→false transition. Before the fix there would only ever be a
	// single isLeader=true entry (stuck leader). After the fix we expect
	// at least one isLeader=false entry (release).
	require.Eventually(t, func() bool {
		for _, lt := range survivorTrackers {
			for _, e := range lt.Entries() {
				if !e.IsLeader {
					return true
				}
			}
		}

		return false
	}, 10*time.Second, 100*time.Millisecond,
		"a takeover leader held leadership forever instead of releasing after calc.Start failed — operators see a silent hang")
}
