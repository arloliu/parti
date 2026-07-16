package consumer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/partitest"
)

// TestBroadcastConfig_Validate_WrapsErrInvalidConfig verifies that every
// validation failure in NewBroadcast / BroadcastConfig.Validate is reachable
// via errors.Is(err, ErrInvalidConfig).
func TestBroadcastConfig_Validate_WrapsErrInvalidConfig(t *testing.T) {
	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	tests := []struct {
		name string
		fn   func() error
	}{
		{
			name: "nil js",
			fn: func() error {
				_, err := NewBroadcast(nil, "S", "pfx", "s.>", handler)

				return err
			},
		},
		{
			name: "fuda required field (missing StreamName)",
			fn: func() error {
				cfg := BroadcastConfig{ConsumerPrefix: "pfx", FilterSubject: "s.>"}
				_ = cfg.SetDefaults()

				return cfg.Validate()
			},
		},
		{
			name: "FetchTimeout below 1s floor",
			fn: func() error {
				cfg := BroadcastConfig{
					StreamName:     "S",
					ConsumerPrefix: "pfx",
					FilterSubject:  "s.>",
					CommonConfig:   CommonConfig{FetchTimeout: 100 * time.Millisecond},
				}
				_ = cfg.SetDefaults()

				return cfg.Validate()
			},
		},
		{
			name: "invalid consumer prefix",
			fn: func() error {
				cfg := BroadcastConfig{
					StreamName:     "S",
					ConsumerPrefix: "bad prefix!",
					FilterSubject:  "s.>",
				}
				_ = cfg.SetDefaults()

				return cfg.Validate()
			},
		},
		{
			name: "PullHeartbeatCap below 500ms floor",
			fn: func() error {
				cfg := BroadcastConfig{
					StreamName:     "S",
					ConsumerPrefix: "pfx",
					FilterSubject:  "s.>",
					CommonConfig:   CommonConfig{PullHeartbeatCap: 499 * time.Millisecond},
				}
				_ = cfg.SetDefaults()

				return cfg.Validate()
			},
		},
		{
			name: "PullHeartbeatCap above 30s ceiling",
			fn: func() error {
				cfg := BroadcastConfig{
					StreamName:     "S",
					ConsumerPrefix: "pfx",
					FilterSubject:  "s.>",
					CommonConfig:   CommonConfig{PullHeartbeatCap: 31 * time.Second},
				}
				_ = cfg.SetDefaults()

				return cfg.Validate()
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

// TestBroadcast_ConsumerOptions_AppliedToLiveConsumer verifies that
// WithConsumerMemoryStorage and WithConsumerReplicas reach the live
// Broadcast consumer's Config.
func TestBroadcast_ConsumerOptions_AppliedToLiveConsumer(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamName := "BC_OPT"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"bcopt.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	b, err := NewBroadcast(
		js,
		streamName,
		"bcopt",
		"bcopt.>",
		handler,
		WithConsumerMemoryStorage(true),
		WithConsumerReplicas(1),
	)
	require.NoError(t, err)
	require.NoError(t, b.Start(ctx))
	defer func() { _ = b.Stop(ctx) }()

	stream, err := js.Stream(ctx, streamName)
	require.NoError(t, err)

	lister := stream.ListConsumers(ctx)
	var found bool
	for ci := range lister.Info() {
		if !strings.HasPrefix(ci.Name, "bcopt") {
			continue
		}
		found = true
		require.True(t, ci.Config.MemoryStorage,
			"consumer %q: Config.MemoryStorage = false, want true", ci.Name)
		require.Equal(t, 1, ci.Config.Replicas,
			"consumer %q: Config.Replicas = %d, want 1", ci.Name, ci.Config.Replicas)
	}
	require.NoError(t, lister.Err(), "ListConsumers iteration failed")
	require.True(t, found, "no Broadcast consumer was created under the bcopt prefix")
}
