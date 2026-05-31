package failure_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestWedgedStorage_ReadOnlyFileStore_RecordsOperationSurfaces(t *testing.T) {
	if os.Getenv("PARTI_RUN_WEDGED_STORAGE_PROBE") != "1" {
		t.Skip("set PARTI_RUN_WEDGED_STORAGE_PROBE=1 to run")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	storeDir := t.TempDir()
	nc, shutdown := startFileStoreNATS(t, storeDir)
	defer shutdown()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   "wedged-rf1",
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)
	rev, err := kv.Put(ctx, "existing", []byte("before"))
	require.NoError(t, err)

	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "WEDGED",
		Subjects:  []string{"wedged.>"},
		Storage:   jetstream.FileStorage,
		Replicas:  1,
		Retention: jetstream.LimitsPolicy,
	})
	require.NoError(t, err)
	consumer, err := js.CreateConsumer(ctx, "WEDGED", jetstream.ConsumerConfig{Durable: "wedged"})
	require.NoError(t, err)

	require.NoError(t, chmodTree(storeDir, 0o555))
	defer func() {
		if err := chmodTree(storeDir, 0o755); err != nil {
			t.Logf("restore store permissions: %v", err)
		}
	}()

	results := []probeSurfaceResult{
		runProbeSurface(ctx, "kv-keys", func(ctx context.Context) error {
			_, err := kv.Keys(ctx)
			return err
		}),
		runProbeSurface(ctx, "kv-get", func(ctx context.Context) error {
			_, err := kv.Get(ctx, "existing")
			return err
		}),
		runProbeSurface(ctx, "kv-watch", func(ctx context.Context) error {
			watcher, err := kv.Watch(ctx, "existing", jetstream.UpdatesOnly())
			if err != nil {
				return err
			}
			return watcher.Stop()
		}),
		runProbeSurface(ctx, "kv-create", func(ctx context.Context) error {
			_, err := kv.Create(ctx, "created-after-readonly", []byte("value"))
			return err
		}),
		runProbeSurface(ctx, "kv-update", func(ctx context.Context) error {
			_, err := kv.Update(ctx, "existing", []byte("after"), rev)
			return err
		}),
		runProbeSurface(ctx, "kv-put", func(ctx context.Context) error {
			_, err := kv.Put(ctx, "put-after-readonly", []byte("value"))
			return err
		}),
		runProbeSurface(ctx, "kv-bucket-create", func(ctx context.Context) error {
			_, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
				Bucket:   "wedged-new-rf1",
				Storage:  jetstream.FileStorage,
				Replicas: 1,
			})
			return err
		}),
		runProbeSurface(ctx, "stream-info", func(ctx context.Context) error {
			_, err := stream.Info(ctx)
			return err
		}),
		runProbeSurface(ctx, "stream-publish", func(ctx context.Context) error {
			_, err := js.Publish(ctx, "wedged.p0", []byte("value"))
			return err
		}),
		runProbeSurface(ctx, "stream-create", func(ctx context.Context) error {
			_, err := js.CreateStream(ctx, jetstream.StreamConfig{
				Name:     "WEDGED_NEW",
				Subjects: []string{"wedged-new.>"},
				Storage:  jetstream.FileStorage,
				Replicas: 1,
			})
			return err
		}),
		runProbeSurface(ctx, "consumer-info", func(ctx context.Context) error {
			_, err := consumer.Info(ctx)
			return err
		}),
	}

	errorCount := 0
	for _, result := range results {
		t.Logf("wedged-storage surface=%s class=%s err=%v matrix=M12", result.surface, result.class, result.err)
		if result.err != nil {
			errorCount++
		}
	}
	if errorCount == 0 {
		t.Log("wedged-storage probe observed no errors after chmod; embedded server may retain writable file handles")
	}
}

func startFileStoreNATS(t *testing.T, storeDir string) (*nats.Conn, func()) {
	t.Helper()
	port, err := getFreePortForRestart()
	require.NoError(t, err)

	ns, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      port,
		JetStream: true,
		StoreDir:  storeDir,
		NoLog:     true,
	})
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second), "embedded NATS not ready")

	nc, err := nats.Connect(ns.ClientURL(),
		nats.Timeout(2*time.Second),
		nats.MaxReconnects(-1),
	)
	require.NoError(t, err)

	return nc, func() {
		nc.Close()
		ns.Shutdown()
		ns.WaitForShutdown()
	}
}

func chmodTree(root string, mode os.FileMode) error {
	return filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chmod(path, mode)
	})
}
