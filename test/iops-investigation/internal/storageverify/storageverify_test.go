package storageverify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// startEmbeddedNATS spins up an in-process NATS server with JetStream
// enabled on a random port and returns a connected JetStream handle
// plus a teardown closure. Mirrors the pattern from instrumentedjs tests.
func startEmbeddedNATS(t *testing.T) (jetstream.JetStream, func()) {
	t.Helper()
	opts := &server.Options{
		JetStream: true,
		Port:      -1,
		StoreDir:  t.TempDir(),
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second), "NATS server not ready")

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cleanup := func() {
		nc.Close()
		ns.Shutdown()
		ns.WaitForShutdown()
	}

	return js, cleanup
}

// createStream is a test helper that creates a JetStream stream with
// the given name, storage type, and replica count.
func createStream(t *testing.T, js jetstream.JetStream, name string, storage jetstream.StorageType, replicas int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     name,
		Subjects: []string{name + ".>"},
		Storage:  storage,
		Replicas: replicas,
	})
	require.NoError(t, err)
}

// TestVerify_Match confirms that a stream whose realised config matches
// the expected entry produces no error.
func TestVerify_Match(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()

	createStream(t, js, "match-stream", jetstream.MemoryStorage, 1)

	ctx := context.Background()
	err := Verify(ctx, js, []Expected{
		{Stream: "match-stream", Storage: jetstream.MemoryStorage, Replicas: 1},
	})
	require.NoError(t, err)
}

// TestVerify_StorageMismatch confirms that a stream created with
// FileStorage but expected as MemoryStorage produces an error that
// names the stream and both storage values.
func TestVerify_StorageMismatch(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()

	createStream(t, js, "storage-stream", jetstream.FileStorage, 1)

	ctx := context.Background()
	err := Verify(ctx, js, []Expected{
		{Stream: "storage-stream", Storage: jetstream.MemoryStorage, Replicas: 1},
	})
	require.Error(t, err)
	msg := err.Error()
	require.Contains(t, msg, "storage-stream", "error must name the stream")
	require.Contains(t, msg, "Memory", "error must mention expected storage (Memory)")
	require.Contains(t, msg, "File", "error must mention actual storage (File)")
}

// TestVerify_ReplicasMismatch confirms that a stream created with
// Replicas=1 but expected with Replicas=3 produces an error that names
// the stream and both replica counts. (An embedded single-node server
// cannot create streams with Replicas=3, so we invert: create with 1,
// expect 3.)
func TestVerify_ReplicasMismatch(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()

	createStream(t, js, "replicas-stream", jetstream.MemoryStorage, 1)

	ctx := context.Background()
	err := Verify(ctx, js, []Expected{
		{Stream: "replicas-stream", Storage: jetstream.MemoryStorage, Replicas: 3},
	})
	require.Error(t, err)
	msg := err.Error()
	require.Contains(t, msg, "replicas-stream", "error must name the stream")
	require.Contains(t, msg, "3", "error must mention expected replicas")
	require.Contains(t, msg, "1", "error must mention actual replicas")
}

// TestVerify_MissingStream confirms that expecting a stream that does
// not exist produces a clear error that names the stream.
func TestVerify_MissingStream(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()

	ctx := context.Background()
	err := Verify(ctx, js, []Expected{
		{Stream: "ghost-stream", Storage: jetstream.MemoryStorage, Replicas: 1},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "ghost-stream", "error must name the missing stream")
}

// TestVerify_MultipleExpected_PartialMismatch confirms that when three
// streams are checked and two have mismatches (with different reasons),
// both mismatches surface via errors.Join and the matching stream does
// not contribute an error.
func TestVerify_MultipleExpected_PartialMismatch(t *testing.T) {
	js, teardown := startEmbeddedNATS(t)
	defer teardown()

	// ok-stream: matches exactly — should not appear in the error.
	createStream(t, js, "ok-stream", jetstream.MemoryStorage, 1)
	// wrong-storage: created with File, expected Memory.
	createStream(t, js, "wrong-storage", jetstream.FileStorage, 1)
	// wrong-replicas: created with 1 replica, expected 3.
	createStream(t, js, "wrong-replicas", jetstream.MemoryStorage, 1)

	ctx := context.Background()
	err := Verify(ctx, js, []Expected{
		{Stream: "ok-stream", Storage: jetstream.MemoryStorage, Replicas: 1},
		{Stream: "wrong-storage", Storage: jetstream.MemoryStorage, Replicas: 1},
		{Stream: "wrong-replicas", Storage: jetstream.MemoryStorage, Replicas: 3},
	})
	require.Error(t, err)

	msg := err.Error()
	// Both mismatched streams must be named in the joined error.
	require.Contains(t, msg, "wrong-storage", "joined error must surface storage mismatch")
	require.Contains(t, msg, "wrong-replicas", "joined error must surface replicas mismatch")
	// The matching stream must not appear.
	require.False(t, strings.Contains(msg, "ok-stream"), "ok-stream must not appear in the error")
}
