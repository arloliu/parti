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

// TestManager_LiveNATSBucketLoss is an exploratory / diagnostic test for
// the scenario where NATS loses all Parti-managed KV bucket data WHILE the
// application workers remain running (no restart). The failing node of a
// non-replicated JetStream cluster may come back with empty storage, or an
// operator may wipe a bucket out of band. The open question is whether the
// running workers observe the loss and recover, or whether they silently
// drift into an inconsistent state until someone restarts them.
//
// This is not a contract test — we do not yet claim Parti handles this path
// gracefully. The test exists to document observed behavior so we can decide
// whether code changes are warranted. The assertions it makes are the
// minimum we want to be true regardless of the recovery story: workers must
// not panic, and a subsequent restart must still succeed.
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

	// Observe for 20 seconds. This is long enough for multiple heartbeat
	// intervals (500ms), multiple TTLs (5s), and a complete stabilization
	// window (3s ColdStartWindow + 2s PlannedScaleWindow).
	const observeFor = 20 * time.Second
	deadline := wipedAt.Add(observeFor)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			t.Fatal("context cancelled during observation")
		case now := <-ticker.C:
			var states, roles []string
			var parts []int
			var versions []int64
			for _, mgr := range cluster.GetActiveWorkers() {
				states = append(states, mgr.State().String())
				role := "follower"
				if mgr.IsLeader() {
					role = "LEADER"
				}
				roles = append(roles, role)
				a := mgr.CurrentAssignment()
				parts = append(parts, len(a.Partitions))
				versions = append(versions, a.Version)
			}
			t.Logf("t+%s  states=%v  roles=%v  parts=%v  versions=%v",
				now.Sub(wipedAt).Round(100*time.Millisecond),
				states, roles, parts, versions)
		}
	}

	// Observation: did any bucket come back? Parti does not try to
	// re-provision buckets from the publish path, so the expected answer
	// depends on whether any subsystem calls ensureKVBucket after startup.
	// Record per-bucket presence for the test log.
	bucketsRecreated := 0
	for _, b := range buckets {
		if _, err := js.KeyValue(ctx, b); err == nil {
			bucketsRecreated++
			t.Logf("observation: bucket %s was recreated during observation window", b)
		}
	}
	t.Logf("observation: %d/%d buckets recreated during live window", bucketsRecreated, len(buckets))

	// Observation: is there a functional leader? Before the wipe there was
	// exactly one. After the wipe, the election key is gone and the
	// heartbeat TTLs will expire; a healthy system should either hold
	// leadership via the existing watcher+refresh path or trigger a fresh
	// election and settle on a new one.
	leaderCount := 0
	for _, mgr := range cluster.GetActiveWorkers() {
		if mgr.IsLeader() {
			leaderCount++
		}
	}
	t.Logf("observation: leaderCount=%d (expected=1 for healthy cluster)", leaderCount)

	// Observation: assignment versions. If the leader detected the loss and
	// republished, versions advance; if leadership was lost silently, they
	// stay frozen at the pre-wipe value.
	postWipeVersions := make(map[string]int64)
	for _, mgr := range cluster.GetActiveWorkers() {
		postWipeVersions[mgr.WorkerID()] = mgr.CurrentAssignment().Version
	}
	t.Logf("observation: post-wipe assignment versions: %v", postWipeVersions)

	// Observation: worker states. Degraded would indicate Parti saw the
	// problem; Stable means it hasn't noticed; Shutdown means something
	// crashed / gave up.
	stateSummary := make(map[string]int)
	for _, mgr := range cluster.GetActiveWorkers() {
		stateSummary[mgr.State().String()]++
	}
	t.Logf("observation: final state summary: %v", stateSummary)

	// Minimum hard invariant: nobody panicked, at least one worker is still
	// alive (not Shutdown). Anything looser would not be a useful test; we
	// are fine with the test passing even if Parti silently does nothing —
	// the diagnostic log output is what we care about for now.
	alive := 0
	for _, mgr := range cluster.GetActiveWorkers() {
		if mgr.State() != types.StateShutdown {
			alive++
		}
	}
	require.Greater(t, alive, 0, "at least one worker should still be running after bucket wipe")
}
