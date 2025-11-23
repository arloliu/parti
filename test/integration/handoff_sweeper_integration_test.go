package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/internal/testutil"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestHandoffSweeper_ExpiredCommitReset validates opportunistic sweeper resets an expired commit claim
// to stable (owner unchanged, pendingOwner cleared) without epoch increment.
func TestHandoffSweeper_ExpiredCommitReset(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}
	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := fmt.Sprintf("itest-handoff-sweeper-%d", time.Now().UnixNano())
	partition := parti.Partition{Keys: []string{"psweep"}}
	pid := partition.ID()

	ttl := 1 * time.Second
	seedEpoch := int64(13)

	cfg := parti.TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = bucket
	cfg.KVBuckets.HandoffTTL = 2 * time.Minute
	// Force worker ID different from owner so Apply path treats partition as newly acquired and triggers sweeper early.
	cfg.WorkerIDMin = 1
	cfg.WorkerIDMax = 2
	// Aggressive sweep interval to guarantee execution.
	cfg.Handoff.SweepInterval = 0

	// Keep a simple source; hygiene runs before calculation starts.
	// Use a noop partition to trigger Apply (and thus the sweeper) without triggering
	// a handoff for the seeded "psweep" partition.
	src := source.NewStatic([]parti.Partition{{Keys: []string{"noop"}}})
	strategy := strategy.NewConsistentHash()
	// Seed the expired commit BEFORE manager start so startup hygiene handles it.
	expiredAt := time.Now().UTC().Add(-5 * ttl)
	commitClaim := parti.HandoffClaim{
		PartitionID: pid,
		Owner:       "worker-2",
		State:       parti.HandoffClaimCommit,
		Epoch:       seedEpoch,
		LastUpdated: expiredAt,
		TTLSeconds:  int64(ttl.Seconds()),
	}
	err = testutil.SeedHandoffClaim(ctx, js, bucket, commitClaim, 2*time.Minute)
	require.NoError(t, err)

	mr := testutil.NewHandoffMetricsRecorder()
	mgr, err := parti.NewManager(&cfg, js, src, strategy, parti.WithHandoffMetricsRecorder(mr))
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })
	// Wait briefly for hygiene to run during Start
	time.Sleep(200 * time.Millisecond)
	claims, err := parti.InspectHandoffClaims(ctx, js, bucket)
	require.NoError(t, err)

	// Filter out the "noop" partition claim
	var targetClaims []parti.HandoffClaim
	for _, c := range claims {
		if c.PartitionID != "noop" {
			targetClaims = append(targetClaims, c)
		}
	}

	require.Len(t, targetClaims, 1)
	got := targetClaims[0]
	// Sweeper should have reset expired commit to stable without epoch change.
	require.Equal(t, parti.HandoffClaimStable, got.State)
	require.Equal(t, seedEpoch, got.Epoch, "epoch must not increment on sweeper reset")
	require.Equal(t, commitClaim.Owner, got.Owner, "owner preserved")
	require.True(t, got.LastUpdated.After(expiredAt))

	// Metrics: sweeper should have recorded at least one stale claim increment
	snap := mr.Snapshot()
	require.GreaterOrEqual(t, snap.StaleClaims, 1, "expected stale claim increment from sweeper reset (maybeSweepClaims)")
	require.NotEmpty(t, snap.ClaimStoreSizes, "expected claim store size samples during sweep")
}
