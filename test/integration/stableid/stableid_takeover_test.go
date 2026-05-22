package stableid_test

import (
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/stableid"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// Integration test: the stale-claim takeover reclaim path, using real
// JetStream KV.
//
// Unlike TestStableID_TTLExpiration_Reclaim — which relies on the bucket TTL
// purging the abandoned key so the next worker reclaims it via a plain Create —
// this exercises the takeover mechanism that reclaims a leaked worker ID while
// its key is still present (not yet purged). The bucket has no TTL, so the key
// is never purged: a reclaim here can only be a staleness takeover.
func TestStableID_StaleKeyTakeover_Reclaim(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// No TTL: the abandoned key is never purged, so the only way a second
	// worker can reclaim the ID is the staleness takeover path.
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "itest-stable-ids-takeover",
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// staleThreshold = 3 * max(ttl/3, 100ms) = 400ms for ttl = 400ms.
	const ttl = 400 * time.Millisecond

	// First worker claims worker-0, then crashes ungracefully: no renewal and
	// no Release, so the key is left behind.
	c1 := stableid.NewClaimer(kv, "worker", 0, 9, ttl, nil)
	id1, err := c1.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", id1)

	// Wait past the stale threshold. The key is NOT purged (bucket has no TTL).
	time.Sleep(ttl + 200*time.Millisecond)

	// The leaked key is still present — so a reclaim is provably a takeover,
	// not a Create after TTL expiry.
	_, err = kv.Get(ctx, "worker-0")
	require.NoError(t, err, "key must still exist (bucket has no TTL); reclaim must be a takeover")

	// Second worker reclaims worker-0 via the staleness takeover.
	c2 := stableid.NewClaimer(kv, "worker", 0, 9, ttl, nil)
	id2, err := c2.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", id2,
		"the restarted worker must take over the stale key, not leak to worker-1")
}
