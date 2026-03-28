package handoff_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestHandoffResume_FinalizesMultipleCommitClaims seeds multiple commit claims owned by the worker
// (simulating a crash after commit, before stable for several partitions) and verifies the resume pass
// finalizes each to stable with a single epoch increment.
func TestHandoffResume_FinalizesMultipleCommitClaims(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}
	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := fmt.Sprintf("itest-handoff-resume-%d", time.Now().UnixNano())
	partitions := []parti.Partition{{Keys: []string{"r1"}}, {Keys: []string{"r2"}}, {Keys: []string{"r3"}}}

	// Worker will claim worker-1 using narrow ID range.
	seedEpoch := int64(20)
	for _, p := range partitions {
		claim := parti.HandoffClaim{
			PartitionID: p.ID(),
			Owner:       "worker-1",
			State:       parti.HandoffClaimCommit,
			Epoch:       seedEpoch,
			LastUpdated: time.Now().UTC(),
			TTLSeconds:  int64((3 * time.Minute).Seconds()),
		}
		err = testutil.SeedHandoffClaim(ctx, js, bucket, claim, 3*time.Minute)
		require.NoError(t, err)
	}

	cfg := parti.TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = bucket
	cfg.KVBuckets.HandoffTTL = 3 * time.Minute
	cfg.WorkerIDMin = 1
	cfg.WorkerIDMax = 2
	// Provide a dummy partition so initial Apply runs and triggers the resume pass; it won't affect seeded claims.
	src := source.NewStatic([]parti.Partition{{Keys: []string{"noop"}}})
	strategy := strategy.NewConsistentHash()

	mgr, err := parti.NewManager(&cfg, js, src, strategy)
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	// Allow resume pass to run (triggered async after initial Apply attempt).
	time.Sleep(300 * time.Millisecond)
	claims, err := parti.InspectHandoffClaims(ctx, js, bucket)
	require.NoError(t, err)

	// Filter out the "noop" partition claim created by the manager's normal operation
	var targetClaims []parti.HandoffClaim
	for _, c := range claims {
		if c.PartitionID != "noop" {
			targetClaims = append(targetClaims, c)
		}
	}

	require.Len(t, targetClaims, len(partitions))
	for _, c := range targetClaims {
		require.Equal(t, parti.HandoffClaimStable, c.State, "claim must be stabilized")
		require.Equal(t, seedEpoch+1, c.Epoch, "epoch must increment exactly once from commit->stable")
		require.Equal(t, mgr.WorkerID(), c.Owner, "owner retained across stabilization")
	}
}
