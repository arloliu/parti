package integration_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/arloliu/parti/internal/stableid"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// Persistent storage / restart behavior integration test (originally in internal package).
func TestStableID_PersistentStorage(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping persistent storage test in short mode")
	}

	t.Run("revisions across restart with TTL expiry", func(t *testing.T) {
		ctx := t.Context()
		tempDir := t.TempDir()
		storeDir := filepath.Join(tempDir, "nats-store")

		start := func() (*server.Server, *nats.Conn) {
			opts := &server.Options{JetStream: true, Port: -1, StoreDir: storeDir}
			ns, err := server.NewServer(opts)
			require.NoError(t, err)
			go ns.Start()
			require.True(t, ns.ReadyForConnections(10*time.Second))
			nc, err := nats.Connect(ns.ClientURL())
			require.NoError(t, err)

			return ns, nc
		}

		ttl := 2 * time.Second // shorten to keep test fast

		// First run
		ns1, nc1 := start()
		js1, err := jetstream.New(nc1)
		require.NoError(t, err)
		kv1, err := js1.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "parti-stableid", TTL: ttl, Storage: jetstream.FileStorage})
		require.NoError(t, err)

		for i := 0; i < 3; i++ {
			c := stableid.NewClaimer(kv1, "simulation-worker", 0, 999, ttl, nil)
			wid, err := c.Claim(ctx)
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf("simulation-worker-%d", i), wid)
		}

		nc1.Close()
		ns1.Shutdown()

		time.Sleep(ttl + 1*time.Second) // let TTL expire

		// Second run
		ns2, nc2 := start()
		defer nc2.Close()
		defer ns2.Shutdown()
		js2, err := jetstream.New(nc2)
		require.NoError(t, err)
		kv2, err := js2.KeyValue(ctx, "parti-stableid")
		require.NoError(t, err)

		claimed := make([]string, 3)
		for i := 0; i < 3; i++ {
			c := stableid.NewClaimer(kv2, "simulation-worker", 0, 999, ttl, nil)
			id, err := c.Claim(ctx)
			require.NoError(t, err)
			claimed[i] = id
			require.Equal(t, fmt.Sprintf("simulation-worker-%d", i), id)
		}
		require.Len(t, claimed, 3)
	})
}
