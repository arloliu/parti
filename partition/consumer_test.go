package partition

import (
	"context"
	"testing"
	"time"

	partitesting "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestJSConsumer_StartTwice(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "events",
		Subjects: []string{"events.*.completed.*"},
	})
	require.NoError(t, err)

	consumer, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "events.{{key}}.completed.{{partition}}",
		},
		StreamName:   "events",
		ConsumerName: "consumer-0",
		Partition:    0,
	}, MessageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }))
	require.NoError(t, err)

	require.NoError(t, consumer.Start(ctx))
	err = consumer.Start(ctx)
	require.Error(t, err)

	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	require.NoError(t, consumer.Stop(stopCtx))
}

func TestJSConsumer_StopWithoutStart(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	consumer, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: PartitionConfig{
			NumPartitions:  1,
			SubjectPattern: "events.{{partition}}",
		},
		StreamName:   "events",
		ConsumerName: "consumer-0",
		Partition:    0,
	}, MessageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }))
	require.NoError(t, err)

	stopCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, consumer.Stop(stopCtx))
}

func TestNewJSConsumer_InvalidArgs(t *testing.T) {
	_, err := NewJSConsumer(nil, ConsumerConfig{}, MessageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }))
	require.Error(t, err)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = NewJSConsumer(js, ConsumerConfig{}, nil)
	require.Error(t, err)
}
