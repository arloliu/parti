package consumer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/partitest"
)

func TestParseStatefulSetOrdinal(t *testing.T) {
	tests := []struct {
		name     string
		hostname string
		want     int
		wantErr  bool
	}{
		{"valid", "worker-3", 3, false},
		{"valid-multi-digit", "worker-123", 123, false},
		{"invalid-no-dash", "worker", 0, true},
		{"invalid-not-int", "worker-abc", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseStatefulSetOrdinal(tt.hostname)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.want, got)
			}
		})
	}
}

func TestGetPartitionFromEnv(t *testing.T) {
	// Save/Restore env
	originalIndex := os.Getenv("PARTITION_INDEX")
	originalHostname := os.Getenv("HOSTNAME")
	defer func() {
		if originalIndex != "" {
			_ = os.Setenv("PARTITION_INDEX", originalIndex)
		} else {
			_ = os.Unsetenv("PARTITION_INDEX")
		}
		if originalHostname != "" {
			_ = os.Setenv("HOSTNAME", originalHostname)
		} else {
			_ = os.Unsetenv("HOSTNAME")
		}
	}()

	t.Run("from PARTITION_INDEX", func(t *testing.T) {
		require.NoError(t, os.Setenv("PARTITION_INDEX", "5"))
		require.NoError(t, os.Unsetenv("HOSTNAME"))
		got, err := GetPartitionFromEnv()
		require.NoError(t, err)
		require.Equal(t, 5, got)
	})

	t.Run("from HOSTNAME", func(t *testing.T) {
		require.NoError(t, os.Unsetenv("PARTITION_INDEX"))
		require.NoError(t, os.Setenv("HOSTNAME", "app-7"))
		got, err := GetPartitionFromEnv()
		require.NoError(t, err)
		require.Equal(t, 7, got)
	})

	t.Run("error", func(t *testing.T) {
		require.NoError(t, os.Unsetenv("PARTITION_INDEX"))
		require.NoError(t, os.Setenv("HOSTNAME", "invalid"))
		_, err := GetPartitionFromEnv()
		require.Error(t, err)
	})
}

// TestStatic_ConsumerOptions_AppliedToLiveConsumer verifies that
// WithConsumerMemoryStorage and WithConsumerReplicas reach the live
// Static consumer's Config.
func TestStatic_ConsumerOptions_AppliedToLiveConsumer(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamName := "STATIC_OPT"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"statopt.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	s, err := NewStatic(
		js,
		streamName,
		"statopt-p0",
		"statopt.{{partition}}",
		2,
		0,
		handler,
		WithConsumerMemoryStorage(true),
		WithConsumerReplicas(1),
	)
	require.NoError(t, err)
	require.NoError(t, s.Start(ctx))
	defer func() { _ = s.Stop(ctx) }()

	stream, err := js.Stream(ctx, streamName)
	require.NoError(t, err)
	cons, err := stream.Consumer(ctx, "statopt-p0")
	require.NoError(t, err)

	consInfo, err := cons.Info(ctx)
	require.NoError(t, err)
	require.True(t, consInfo.Config.MemoryStorage)
	require.Equal(t, 1, consInfo.Config.Replicas)
}
