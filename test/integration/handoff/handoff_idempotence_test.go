package handoff_test

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

// TestHandoffIdempotence_StableClaimUnchanged ensures an existing stable claim owned by the starting worker
// remains unchanged (state & epoch) after initial Apply.
func TestHandoffIdempotence_StableClaimUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}
	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := fmt.Sprintf("itest-handoff-idempotence-%d", time.Now().UnixNano())
	partition := parti.Partition{Keys: []string{"pidem"}}
	pid := partition.ID()

	seedEpoch := int64(3)
	claim := parti.HandoffClaim{
		PartitionID: pid,
		Owner:       "worker-1", // same worker we expect to start as
		State:       parti.HandoffClaimStable,
		Epoch:       seedEpoch,
		LastUpdated: time.Now().UTC(),
		TTLSeconds:  int64((2 * time.Minute).Seconds()),
	}
	err = testutil.SeedHandoffClaim(ctx, js, bucket, claim, 2*time.Minute)
	require.NoError(t, err)

	cfg := parti.TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = bucket
	cfg.KVBuckets.HandoffTTL = 2 * time.Minute
	cfg.WorkerIDMin = 1
	cfg.WorkerIDMax = 2

	src := source.NewStatic([]parti.Partition{partition})
	strategy := strategy.NewConsistentHash()
	mgr, err := parti.NewManager(&cfg, js, src, strategy)
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	time.Sleep(150 * time.Millisecond)
	claims, err := parti.InspectHandoffClaims(ctx, js, bucket)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	got := claims[0]
	require.Equal(t, parti.HandoffClaimStable, got.State)
	require.Equal(t, seedEpoch, got.Epoch, "epoch should remain unchanged for idempotent stable claim")
	require.Equal(t, mgr.WorkerID(), got.Owner)
}
