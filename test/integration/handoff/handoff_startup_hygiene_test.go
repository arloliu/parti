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

// TestHandoffStartupHygiene_ExpiredPrepareReset verifies that expired non-stable claims are reset to stable
// without epoch increment and pendingOwner cleared during manager startup hygiene pass.
func TestHandoffStartupHygiene_ExpiredPrepareReset(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}

	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := fmt.Sprintf("itest-handoff-hygiene-%d", time.Now().UnixNano())
	partition := parti.Partition{Keys: []string{"phyg"}}
	pid := partition.ID()

	seedEpoch := int64(7)
	// Expired prepare claim: LastUpdated far in past relative to TTL.
	ttl := 1 * time.Second
	expiredAt := time.Now().UTC().Add(-5 * ttl)
	claim := parti.HandoffClaim{
		PartitionID:  pid,
		Owner:        "worker-9",
		PendingOwner: "worker-1",
		State:        parti.HandoffClaimPrepare,
		Epoch:        seedEpoch,
		LastUpdated:  expiredAt,
		TTLSeconds:   int64(ttl.Seconds()),
	}
	err = testutil.SeedHandoffClaim(ctx, js, bucket, claim, 2*time.Minute)
	require.NoError(t, err)

	cfg := parti.TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = bucket
	cfg.KVBuckets.HandoffTTL = 2 * time.Minute
	cfg.WorkerIDMin = 1
	cfg.WorkerIDMax = 2

	// Do not include the partition in the initial assignment so hygiene effect is directly observable
	// without a subsequent prepare overwrite.
	src := source.NewStatic(nil)
	strategy := strategy.NewConsistentHash()
	mgr, err := parti.NewManager(&cfg, js, src, strategy)
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	// Allow hygiene to run during startup (should have finished by now).
	time.Sleep(150 * time.Millisecond)
	claims, err := parti.InspectHandoffClaims(ctx, js, bucket)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	got := claims[0]
	require.Equal(t, pid, got.PartitionID)
	require.Equal(t, parti.HandoffClaimStable, got.State, "expired prepare must reset to stable")
	require.Empty(t, got.PendingOwner, "pending owner cleared on hygiene reset")
	require.Equal(t, seedEpoch, got.Epoch, "epoch must not increment on hygiene reset")
	require.True(t, got.LastUpdated.After(expiredAt), "LastUpdated should be refreshed to now")
}
