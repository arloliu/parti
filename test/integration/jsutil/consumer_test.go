package jsutil_test

import (
	"context"
	"sync"
	"testing"

	"github.com/arloliu/parti/v2/jsutil"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureConsumer_CreatesNewConsumer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create stream first
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "TEST-CONSUMER",
		Subjects: []string{"test.consumer.>"},
	})
	require.NoError(t, err)

	consumer, err := jsutil.EnsureConsumer(ctx, js, "TEST-CONSUMER", jetstream.ConsumerConfig{
		Durable:       "my-consumer",
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: "test.consumer.events",
	})

	require.NoError(t, err)
	require.NotNil(t, consumer)

	info, err := consumer.Info(ctx)
	require.NoError(t, err)
	assert.Equal(t, "my-consumer", info.Config.Durable)
	assert.Equal(t, "test.consumer.events", info.Config.FilterSubject)
}

func TestEnsureConsumer_UpdatesExistingConsumer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create stream first
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "TEST-UPDATE",
		Subjects: []string{"test.update.>"},
	})
	require.NoError(t, err)

	// Create initial consumer
	_, err = js.CreateOrUpdateConsumer(ctx, "TEST-UPDATE", jetstream.ConsumerConfig{
		Durable:   "updatable",
		AckPolicy: jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err)

	// EnsureConsumer should update without error
	consumer, err := jsutil.EnsureConsumer(ctx, js, "TEST-UPDATE", jetstream.ConsumerConfig{
		Durable:   "updatable",
		AckPolicy: jetstream.AckExplicitPolicy,
	})

	require.NoError(t, err)
	require.NotNil(t, consumer)
}

func TestEnsureConsumer_StreamNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Try to create consumer on non-existent stream
	consumer, err := jsutil.EnsureConsumer(ctx, js, "NON-EXISTENT-STREAM", jetstream.ConsumerConfig{
		Durable:   "orphan-consumer",
		AckPolicy: jetstream.AckExplicitPolicy,
	})

	require.Error(t, err)
	assert.Nil(t, consumer)
	assert.Contains(t, err.Error(), "not found")
	assert.ErrorIs(t, err, jetstream.ErrStreamNotFound)
}

func TestEnsureConsumer_ConcurrentCreation(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create stream first
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "TEST-CONCURRENT-CONSUMER",
		Subjects: []string{"test.concurrent.consumer.>"},
	})
	require.NoError(t, err)

	const numGoroutines = 10
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	results := make(chan error, numGoroutines)

	// Simulate race condition: multiple goroutines trying to create the same consumer
	for range numGoroutines {
		go func() {
			defer wg.Done()
			_, err := jsutil.EnsureConsumer(ctx, js, "TEST-CONCURRENT-CONSUMER", jetstream.ConsumerConfig{
				Durable:   "concurrent-consumer",
				AckPolicy: jetstream.AckExplicitPolicy,
			})
			results <- err
		}()
	}

	wg.Wait()
	close(results)

	// All goroutines should succeed
	for err := range results {
		assert.NoError(t, err, "all concurrent EnsureConsumer calls should succeed")
	}

	// Verify consumer exists
	consumer, err := js.Consumer(ctx, "TEST-CONCURRENT-CONSUMER", "concurrent-consumer")
	require.NoError(t, err)
	require.NotNil(t, consumer)
}

func TestEnsureConsumer_ContextCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create stream first (with valid context)
	ctx := t.Context()
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "TEST-CANCEL-CONSUMER",
		Subjects: []string{"test.cancel.consumer.>"},
	})
	require.NoError(t, err)

	// Create already-cancelled context
	cancelledCtx, cancel := context.WithCancel(t.Context())
	cancel()

	consumer, err := jsutil.EnsureConsumer(cancelledCtx, js, "TEST-CANCEL-CONSUMER", jetstream.ConsumerConfig{
		Durable:   "cancelled-consumer",
		AckPolicy: jetstream.AckExplicitPolicy,
	})

	require.Error(t, err)
	assert.Nil(t, consumer)
}
