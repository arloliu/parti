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

// TestManager_LiveNATSBucketLoss exercises the live-wipe scenario: NATS loses
// all Parti-managed KV bucket data WHILE the application workers remain
// running (no restart). The failing node of a non-replicated JetStream
// cluster may come back with empty storage, or an operator may wipe a bucket
// out of band.
//
// Contract (v2.3+): every running worker must enter Degraded state within a
// bounded window of the wipe. Parti does not attempt in-process self-healing
// of lost bucket metadata — recovery is a process restart, but Degraded
// entry is the observable trigger that lets operators (and k8s readiness
// probes wired through OnDegraded) know a restart is needed. See
// docs/OPERATIONS.md "Live NATS data loss" for the runbook.
func TestManager_LiveNATSBucketLoss(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Bring up a 3-worker cluster and let it stabilize.
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
	t.Logf("phase 1: stable, leader=%s", originalLeaderID)

	// Snapshot assignment versions before the wipe so we can observe whether
	// the cluster notices the loss and resumes publishing fresh versions.
	preWipeVersions := make(map[string]int64)
	for _, mgr := range cluster.GetActiveWorkers() {
		preWipeVersions[mgr.WorkerID()] = mgr.CurrentAssignment().Version
	}
	t.Logf("phase 1: pre-wipe assignment versions: %v", preWipeVersions)

	// Wipe all Parti-managed KV buckets while workers are live. The NATS
	// connection stays up, so monitorNATSConnection's "is connected" check
	// still returns true — no automatic degraded-mode entry.
	buckets := []string{
		cfg.KVBuckets.StableIDBucket,
		cfg.KVBuckets.ElectionBucket,
		cfg.KVBuckets.HeartbeatBucket,
		cfg.KVBuckets.AssignmentBucket,
	}
	for _, b := range buckets {
		if err := js.DeleteKeyValue(ctx, b); err != nil &&
			!errors.Is(err, jetstream.ErrBucketNotFound) {
			t.Fatalf("failed to wipe bucket %s: %v", b, err)
		}
	}
	wipedAt := time.Now()
	t.Logf("phase 2: wiped %d buckets at %s", len(buckets), wipedAt.Format(time.RFC3339Nano))

	// Confirm the wipe actually happened — guards against the test silently
	// passing because DeleteKeyValue no-opped.
	_, err = js.KeyValue(ctx, cfg.KVBuckets.AssignmentBucket)
	require.ErrorIs(t, err, jetstream.ErrBucketNotFound)

	// Every worker must enter Degraded within this window. Defaults at
	// IntegrationTestConfig: HeartbeatInterval=500ms, KVErrorThreshold=5,
	// KVErrorWindow=30s → 5 failed publishes accrue in ~2.5s; budget 20s for
	// scheduling jitter, the election timer (3s), and hook invocation.
	const degradedWithin = 20 * time.Second
	require.Eventually(t, func() bool {
		active := cluster.GetActiveWorkers()
		if len(active) != 3 {
			return false
		}
		degraded := 0
		for _, mgr := range active {
			if mgr.State() == types.StateDegraded {
				degraded++
			}
		}

		return degraded == 3
	}, degradedWithin, 500*time.Millisecond,
		"all 3 workers should enter Degraded within %s of bucket wipe", degradedWithin)

	t.Logf("phase 3: all workers reported Degraded within %s", time.Since(wipedAt).Round(100*time.Millisecond))

	// Parti explicitly declines in-process auto re-provisioning — recovery
	// is a process restart. Guard against a future change silently
	// reintroducing it by asserting the wiped buckets stay gone.
	for _, b := range buckets {
		_, err := js.KeyValue(ctx, b)
		require.ErrorIsf(t, err, jetstream.ErrBucketNotFound,
			"bucket %s must not be auto-recreated by live workers", b)
	}

	// Log the post-wipe state for diagnostic context. Assignment versions
	// should be frozen at their pre-wipe values because the leader's
	// publishes all fail against a missing bucket.
	postWipeVersions := make(map[string]int64)
	stateSummary := make(map[string]int)
	for _, mgr := range cluster.GetActiveWorkers() {
		postWipeVersions[mgr.WorkerID()] = mgr.CurrentAssignment().Version
		stateSummary[mgr.State().String()]++
	}
	t.Logf("phase 3: pre-wipe versions=%v post-wipe versions=%v states=%v",
		preWipeVersions, postWipeVersions, stateSummary)
}

// TestManager_LiveNATSBucketLoss_OnDegradedHook asserts that the OnDegraded
// hook fires for every worker when buckets are wiped live. This is the
// integration point k8s readiness probes hang off of.
func TestManager_LiveNATSBucketLoss_OnDegradedHook(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	partitions := testutil.CreateTestPartitions(6)
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	const clusterSize = 3
	degradedReasons := make(chan string, clusterSize*4)
	managers := make([]*parti.Manager, 0, clusterSize)
	for range clusterSize {
		hooks := &parti.Hooks{
			OnDegraded: func(_ context.Context, reason string) error {
				select {
				case degradedReasons <- reason:
				default:
				}
				return nil
			},
		}
		mgr, err := parti.NewManager(&cfg, js, src, assignStrat, parti.WithHooks(hooks))
		require.NoError(t, err)
		managers = append(managers, mgr)
	}

	defer func() {
		for _, mgr := range managers {
			_ = mgr.Stop(context.Background())
		}
	}()

	for _, mgr := range managers {
		require.NoError(t, mgr.Start(ctx))
	}

	// Wait for cluster to stabilize before the wipe.
	require.Eventually(t, func() bool {
		for _, mgr := range managers {
			if mgr.State() != types.StateStable {
				return false
			}
		}
		return true
	}, 15*time.Second, 200*time.Millisecond, "cluster should stabilize before wipe")

	for _, b := range []string{
		cfg.KVBuckets.StableIDBucket,
		cfg.KVBuckets.ElectionBucket,
		cfg.KVBuckets.HeartbeatBucket,
		cfg.KVBuckets.AssignmentBucket,
	} {
		if err := js.DeleteKeyValue(ctx, b); err != nil && !errors.Is(err, jetstream.ErrBucketNotFound) {
			t.Fatalf("failed to wipe bucket %s: %v", b, err)
		}
	}

	// Each manager fires OnDegraded at most once per degraded entry. Expect
	// one reason per worker.
	collected := make([]string, 0, clusterSize)
	timeout := time.After(20 * time.Second)
	for len(collected) < clusterSize {
		select {
		case reason := <-degradedReasons:
			collected = append(collected, reason)
		case <-timeout:
			t.Fatalf("OnDegraded fired %d/%d times within 20s; reasons so far: %v",
				len(collected), clusterSize, collected)
		}
	}
	for _, reason := range collected {
		require.NotEmpty(t, reason, "OnDegraded reason must be non-empty")
	}
	t.Logf("OnDegraded hook fired %d/%d times; reasons=%v", len(collected), clusterSize, collected)
}
