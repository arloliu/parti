package failure_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/stableid"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestStableID_TakesOverStaleKeyAfterFileStorageRestart verifies the scenario
// the claimer source comment names ("file storage after NATS restart"): a
// file-backed stableID key survives a real NATS server restart, and a restarted
// worker reclaims it via the stale-takeover path rather than leaking to the
// next ID. The bucket MaxAge is long (key never purged) and the Claimer ttl is
// short (staleness threshold elapses fast), so the reclaim is deterministically
// a takeover of a still-present key — asserted by Get calls before the Claim.
func TestStableID_TakesOverStaleKeyAfterFileStorageRestart(t *testing.T) {
	nc, restart := startEmbeddedNATSWithRestart(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := context.Background()
	const bucket = "stableid-filestore-restart"
	// Long bucket MaxAge: the key is never purged, so the reclaim must be a
	// stale takeover, not the absent-key Create path. Short Claimer ttl: the
	// staleness threshold (3×max(ttl/3,100ms) == 1s) elapses quickly.
	const bucketMaxAge = time.Hour
	const claimerTTL = time.Second

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  bucket,
		Storage: jetstream.FileStorage,
		TTL:     bucketMaxAge,
	})
	require.NoError(t, err)

	// A worker claims worker-0, then exits ungracefully (no Release).
	first := stableid.NewClaimer(kv, "worker", 0, 9, claimerTTL, nil)
	wid, err := first.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", wid)

	// Restart NATS on the same port + StoreDir.
	restart(t)

	kv2, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)

	// The file-backed key must have survived the restart.
	_, err = kv2.Get(ctx, "worker-0")
	require.NoError(t, err, "file-backed stableID key must survive a NATS restart")

	// Age it past the staleness threshold; the long bucket MaxAge keeps it
	// present, so this is the "stale but not purged" condition.
	time.Sleep(3 * claimerTTL)
	_, err = kv2.Get(ctx, "worker-0")
	require.NoError(t, err, "key must still be present before the reclaim (long bucket MaxAge)")

	// A restarted worker must reclaim worker-0 via the stale-takeover path.
	second := stableid.NewClaimer(kv2, "worker", 0, 9, claimerTTL, nil)
	wid, err = second.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", wid,
		"a restarted worker must take over the stale file-backed key, not leak to worker-1")
}
