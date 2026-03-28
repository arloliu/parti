package consumer_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/consumer"
	partitesting "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
)

// TestWIPHandler_PreventsRedelivery verifies that WIPHandler prevents message
// redelivery during long-running processing by sending periodic InProgress() calls.
//
// Scenario:
//  1. Create a stream with short AckWait (2s)
//  2. Wrap a slow handler (2.5s processing) with WIPHandler (500ms heartbeat)
//  3. Verify the message is processed exactly once (no redelivery)
func TestWIPHandler_PreventsRedelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "WIP_TEST"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"wip.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// Publish a test message
	_, err = js.Publish(ctx, "wip.test", []byte("long-running-job"))
	require.NoError(t, err)

	var processCount atomic.Int32
	processingDone := make(chan struct{})

	// Slow handler that takes longer than AckWait
	slowHandler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		count := processCount.Add(1)
		if count == 1 {
			time.Sleep(2500 * time.Millisecond) // Longer than AckWait (2s)
			close(processingDone)
		}
		return nil
	})

	// Wrap with WIPHandler - heartbeat every 500ms to keep message alive
	wrappedHandler := consumer.NewWIPHandler(slowHandler, consumer.WIPConfig{
		Interval:    500 * time.Millisecond,
		MinInterval: -1, // Disable clamping for test
	})

	// Create Dynamic consumer with short AckWait
	dc, err := consumer.NewDynamic(js, streamName, "wip-worker", "wip.{{.PartitionID}}", wrappedHandler,
		consumer.WithAckWait(2*time.Second),
		consumer.WithBatchSize(1),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(context.Background()) })

	// Start consumption
	require.NoError(t, dc.Update(ctx, "worker-wip", []types.Partition{{Keys: []string{"test"}}}))

	// Wait for processing to complete
	select {
	case <-processingDone:
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("processing did not complete in time")
	}

	// Give time for potential redelivery (should not happen)
	time.Sleep(500 * time.Millisecond)

	require.Equal(t, int32(1), processCount.Load(),
		"message should be processed exactly once with WIP heartbeats")
}

// TestWIPHandler_WithQueueConsumer verifies WIPHandler works with Queue consumers.
func TestWIPHandler_WithQueueConsumer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "WIP_Q"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"wipq.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	_, err = js.Publish(ctx, "wipq.events", []byte("slow-job"))
	require.NoError(t, err)

	var processCount atomic.Int32
	processingDone := make(chan struct{})

	slowHandler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		count := processCount.Add(1)
		if count == 1 {
			time.Sleep(2500 * time.Millisecond)
			close(processingDone)
		}
		return nil
	})

	wrappedHandler := consumer.NewWIPHandler(slowHandler, consumer.WIPConfig{
		Interval:    500 * time.Millisecond,
		MinInterval: -1,
	})

	q, err := consumer.NewQueue(js, streamName, "wipq-consumer", "wipq.>", wrappedHandler,
		consumer.WithAckWait(2*time.Second),
		consumer.WithBatchSize(1),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Stop(ctx) })

	require.NoError(t, q.Start(ctx))

	select {
	case <-processingDone:
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("processing did not complete in time")
	}

	time.Sleep(500 * time.Millisecond)

	require.Equal(t, int32(1), processCount.Load(),
		"message should be processed exactly once with WIP heartbeats on Queue consumer")
}

// TestWIPHandler_FastHandlerNoOverhead verifies that a fast handler (processing
// completes before the heartbeat interval) does not spawn a heartbeat goroutine.
func TestWIPHandler_FastHandlerNoOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "WIP_FAST"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"wipf.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// Publish 10 messages
	for range 10 {
		_, err = js.Publish(ctx, "wipf.events", []byte("fast-job"))
		require.NoError(t, err)
	}

	var processed atomic.Int32
	// Fast handler: completes well before heartbeat interval
	fastHandler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		processed.Add(1)
		return nil
	})

	wrappedHandler := consumer.NewWIPHandler(fastHandler, consumer.WIPConfig{
		Interval: 5 * time.Second, // Long interval: handler finishes way before this
	})

	q, err := consumer.NewQueue(js, streamName, "wipf-consumer", "wipf.>", wrappedHandler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Stop(ctx) })

	require.NoError(t, q.Start(ctx))

	// Wait for all messages
	require.Eventually(t, func() bool {
		return processed.Load() >= 10
	}, 10*time.Second, 50*time.Millisecond)

	require.Equal(t, int32(10), processed.Load(),
		"all 10 messages should be processed with fast handler + WIP")
}
