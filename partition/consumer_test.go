package partition

import (
	"context"
	"testing"
	"time"

	partitesting "github.com/arloliu/parti/v2/testing"
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

func TestJSConsumer_GracefulShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "graceful",
		Subjects: []string{"graceful.*"},
	})
	require.NoError(t, err)

	// Publish some messages
	for range 5 {
		_, err = js.Publish(ctx, "graceful.0", []byte("test"))
		require.NoError(t, err)
	}

	processed := make(chan struct{}, 10)
	consumer, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "graceful.{{partition}}",
		},
		StreamName:   "graceful",
		ConsumerName: "graceful-consumer-0",
		Partition:    0,
		FetchTimeout: 1 * time.Second,
	}, MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		processed <- struct{}{}
		return nil
	}))
	require.NoError(t, err)

	require.NoError(t, consumer.Start(ctx))

	// Wait for some messages to be processed
	for range 5 {
		select {
		case <-processed:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for message processing")
		}
	}

	// Stop should complete gracefully
	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err = consumer.Stop(stopCtx)
	require.NoError(t, err)
}

func TestJSConsumer_StopTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "timeout",
		Subjects: []string{"timeout.*"},
	})
	require.NoError(t, err)

	// Publish a message
	_, err = js.Publish(ctx, "timeout.0", []byte("test"))
	require.NoError(t, err)

	blockingHandler := make(chan struct{})
	consumer, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "timeout.{{partition}}",
		},
		StreamName:   "timeout",
		ConsumerName: "timeout-consumer-0",
		Partition:    0,
		FetchTimeout: 5 * time.Second,
		ManualAck:    true, // Don't auto-ack so message stays in-flight
	}, MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		<-blockingHandler // Block until closed
		return nil
	}))
	require.NoError(t, err)

	require.NoError(t, consumer.Start(ctx))

	// Give it time to start processing
	time.Sleep(200 * time.Millisecond)

	// Stop with very short timeout - should return context error
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	err = consumer.Stop(stopCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// Unblock handler so goroutine can exit cleanly
	close(blockingHandler)
}

func TestJSConsumer_StopIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "idempotent",
		Subjects: []string{"idempotent.*"},
	})
	require.NoError(t, err)

	consumer, err := NewJSConsumer(js, ConsumerConfig{
		PartitionConfig: PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "idempotent.{{partition}}",
		},
		StreamName:   "idempotent",
		ConsumerName: "idempotent-consumer-0",
		Partition:    0,
		FetchTimeout: 1 * time.Second,
	}, MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	}))
	require.NoError(t, err)

	require.NoError(t, consumer.Start(ctx))

	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// First stop should succeed
	err = consumer.Stop(stopCtx)
	require.NoError(t, err)

	// Second stop should also succeed (idempotent)
	err = consumer.Stop(stopCtx)
	require.NoError(t, err)
}
