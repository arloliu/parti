package integration_test

import (
	"testing"
	"time"

	"github.com/arloliu/parti/internal/stableid"
	partitest "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// Integration test: TTL expiry and reclaim path using real JetStream KV.
func TestStableID_TTLExpiration_Reclaim(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	_, nc := partitest.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "itest-stable-ids-ttl",
		TTL:     1 * time.Second,
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// First worker claims without renewal (simulated crash)
	c1 := stableid.NewClaimer(kv, "worker", 0, 9, 1*time.Second, nil)
	id1, err := c1.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", id1)

	// Wait for TTL to expire
	time.Sleep(1500 * time.Millisecond)

	// Second worker should reclaim same ID
	c2 := stableid.NewClaimer(kv, "worker", 0, 9, 1*time.Second, nil)
	id2, err := c2.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker-0", id2)
}
