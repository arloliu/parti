package handoff_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestHandoffBucketHasNoMaxAge verifies that when two-phase handoff is enabled
// the manager creates the handoff KV bucket with NO MaxAge.
//
// A bucket-level TTL would age out the stable ownership claims that two-phase
// handoff writes once and never refreshes; the consumer's claim resolver would
// then lose them and pull gating would permanently suppress delivery. The
// coordinator's advisory claim TTL (Config.KVBuckets.HandoffTTL) is recorded on
// each claim record for the stuck-handoff sweep — it is NOT the bucket TTL.
func TestHandoffBucketHasNoMaxAge(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}

	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := parti.TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = "itest-handoff-nomaxage"
	cfg.KVBuckets.HandoffTTL = 2 * time.Minute // advisory sweep TTL, not a bucket TTL

	parts := []parti.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}}
	src := source.NewStatic(parts)

	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash())
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	// The handoff bucket exists and carries no MaxAge.
	kv, err := js.KeyValue(ctx, cfg.KVBuckets.HandoffBucket)
	require.NoError(t, err, "handoff bucket should exist")
	status, err := kv.Status(ctx)
	require.NoError(t, err)
	bs, ok := status.(*jetstream.KeyValueBucketStatus)
	require.True(t, ok, "handoff bucket status should be a KeyValueBucketStatus")
	require.NotNil(t, bs.StreamInfo())
	require.Equal(t, time.Duration(0), bs.StreamInfo().Config.MaxAge,
		"handoff bucket must be created with no MaxAge so stable claims never expire")

	// Stable claims are written and still carry a positive advisory TTLSeconds
	// so the coordinator sweep can recover stuck in-flight handoffs.
	require.Eventually(t, func() bool {
		claims, e := mgr.InspectHandoffClaims(ctx)
		return e == nil && len(claims) > 0
	}, 10*time.Second, 200*time.Millisecond, "two-phase handoff should write claims")

	claims, err := mgr.InspectHandoffClaims(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, claims)
	for _, c := range claims {
		require.Positive(t, c.TTLSeconds,
			"claim %s must carry a positive advisory TTLSeconds for the sweep", c.PartitionID)
	}
}
