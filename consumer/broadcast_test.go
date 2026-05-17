package consumer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/partitest"
)

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
