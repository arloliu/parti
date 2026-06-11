package consumer

import (
	"context"
	"errors"
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

// TestStaticConfig_Validate_WrapsErrInvalidConfig verifies that every validation
// failure in NewStatic / StaticConfig.Validate is reachable via
// errors.Is(err, ErrInvalidConfig).
func TestStaticConfig_Validate_WrapsErrInvalidConfig(t *testing.T) {
	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	tests := []struct {
		name string
		fn   func() error
	}{
		{
			name: "nil js",
			fn: func() error {
				_, err := NewStatic(nil, "S", "c", "s.{{partition}}", 2, 0, handler)

				return err
			},
		},
		{
			name: "fuda required field (missing StreamName)",
			fn: func() error {
				cfg := StaticConfig{ConsumerName: "c", SubjectPattern: "s.{{partition}}", NumPartitions: 2}
				_ = cfg.SetDefaults()

				return cfg.Validate()
			},
		},
		{
			name: "FetchTimeout below 1s floor",
			fn: func() error {
				cfg := StaticConfig{
					StreamName:     "S",
					ConsumerName:   "c",
					SubjectPattern: "s.{{partition}}",
					NumPartitions:  2,
					CommonConfig:   CommonConfig{FetchTimeout: 100 * time.Millisecond},
				}
				_ = cfg.SetDefaults()

				return cfg.Validate()
			},
		},
		{
			name: "invalid consumer name",
			fn: func() error {
				cfg := StaticConfig{
					StreamName:     "S",
					ConsumerName:   "bad name!",
					SubjectPattern: "s.{{partition}}",
					NumPartitions:  2,
				}
				_ = cfg.SetDefaults()

				return cfg.Validate()
			},
		},
		{
			name: "invalid subject pattern (crosses ipartition boundary)",
			fn: func() error {
				// Pattern with an unrecognized placeholder: the error originates in
				// partutil.ParsePattern inside ipartition.NewJSConsumer and must be
				// wrapped at the NewStatic boundary. Use a non-nil js so the guard
				// checks pass and the pattern parser is reached.
				_, nc := partitest.StartEmbeddedNATS(t)
				js, _ := jetstream.New(nc)
				// "{{foo}}" is not a recognised placeholder; ParsePattern returns an error.
				_, err := NewStatic(js, "S", "c", "s.{{foo}}", 2, 0, handler)

				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn()
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrInvalidConfig),
				"expected errors.Is(err, ErrInvalidConfig), got: %v", err)
		})
	}
}

// TestStatic_StartAfterStop_SentinelPrecedence verifies that calling Start on a
// stopped Static consumer returns ErrConsumerStopped rather than ErrInvalidConfig,
// even when the compat check would fail (WorkQueuePolicy stream + RecoverFromNew).
// The terminal-stopped sentinel must take precedence over the compat preflight.
func TestStatic_StartAfterStop_SentinelPrecedence(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create a WorkQueuePolicy stream — this is the stream type that makes the
	// compat check fail when RecoverFromNew is configured.
	streamName := "STATIC_SENTINEL_PREC"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"sentprec.>"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	// Build a Static with RecoverFromNew — this combination is incompatible with
	// WorkQueuePolicy, so CheckWorkQueueRecoveryCompat will return ErrInvalidConfig
	// if reached.
	s, err := NewStatic(
		js,
		streamName,
		"sentprec-p0",
		"sentprec.{{partition}}",
		2,
		0,
		handler,
		WithRecoveryStrategy(RecoverFromNew),
	)
	require.NoError(t, err)

	// Stop before ever calling Start — marks the inner stopped flag.
	require.NoError(t, s.Stop(ctx))

	// Start must return ErrConsumerStopped, NOT ErrInvalidConfig.
	err = s.Start(ctx)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrConsumerStopped),
		"expected ErrConsumerStopped, got: %v", err)
	require.False(t, errors.Is(err, ErrInvalidConfig),
		"ErrConsumerStopped must take precedence over ErrInvalidConfig, got: %v", err)
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
