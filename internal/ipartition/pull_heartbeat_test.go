package ipartition

import (
	"context"
	"testing"
	"time"

	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/partition"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestJSConsumer_FetchTimeoutAbove60s_ReceivesMessages proves the run() pull
// loop's derivePullHeartbeat wiring end-to-end: before the 30s ceiling was
// added, a FetchTimeout above 60s derived a heartbeat above nats.go's 30s
// PullHeartbeat maximum, iterator creation failed on every attempt inside
// run()'s loop, and no message was ever delivered.
func TestJSConsumer_FetchTimeoutAbove60s_ReceivesMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "hb90",
		Subjects: []string{"hb90.*"},
	})
	require.NoError(t, err)

	_, err = js.Publish(ctx, "hb90.0", []byte("test"))
	require.NoError(t, err)

	processed := make(chan struct{}, 1)
	consumer, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "hb90.{{partition}}",
		},
		StreamName:   "hb90",
		ConsumerName: "hb90-consumer-0",
		Partition:    0,
		FetchTimeout: 90 * time.Second,
	}, messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		processed <- struct{}{}

		return nil
	}))
	require.NoError(t, err)

	require.NoError(t, consumer.Start(ctx))
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = consumer.Stop(stopCtx)
	})

	select {
	case <-processed:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for message processing; iterator creation likely rejected by nats.go (heartbeat above 30s ceiling)")
	}
}

// TestConsumerConfig_Validate_PullHeartbeatCapBounds pins that ConsumerConfig
// enforces the same [500ms, 30s] nats.go PullHeartbeat range as the durable
// and consumer packages: a config built directly through internal/ipartition
// must not be able to smuggle in a cap that natsutil.DerivePullHeartbeat
// would turn into a value nats.go rejects at iterator creation.
func TestConsumerConfig_Validate_PullHeartbeatCapBounds(t *testing.T) {
	validate := func(heartbeatCap time.Duration) error {
		cfg := ConsumerConfig{
			PartitionConfig: partition.PartitionConfig{
				NumPartitions:  2,
				SubjectPattern: "events.{{partition}}",
			},
			StreamName:       "EVENTS",
			ConsumerName:     "consumer-0",
			Partition:        0,
			PullHeartbeatCap: heartbeatCap,
		}

		return cfg.Validate()
	}

	require.NoError(t, validate(0), "0 (disabled) must pass")
	require.Error(t, validate(499*time.Millisecond), "PullHeartbeatCap below the 500ms nats.go PullHeartbeat floor must fail validation")
	require.NoError(t, validate(500*time.Millisecond), "PullHeartbeatCap == 500ms is the boundary and must pass")
	require.NoError(t, validate(30*time.Second), "PullHeartbeatCap == 30s is the boundary and must pass")
	require.Error(t, validate(30*time.Second+time.Nanosecond), "PullHeartbeatCap above the 30s nats.go PullHeartbeat ceiling must fail validation")
}
