package subscription_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/consumer"
	"github.com/arloliu/parti/subscription"
	partitesting "github.com/arloliu/parti/testing"
)

// TestWIPHandler_Integration verifies that WIPHandler prevents message redelivery
// during long-running processing by sending periodic InProgress() calls.
//
// This test:
// 1. Creates a stream with a short AckWait (2s)
// 2. Wraps a slow handler (2.5s processing) with WIPHandler (500ms interval)
// 3. Verifies the message is processed exactly once (no redelivery)
func TestWIPHandler_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "wip-test-stream"
	subject := "wip.test"

	// Create stream
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"wip.>"},
	})
	require.NoError(t, err)

	// Publish a test message
	_, err = js.Publish(ctx, subject, []byte("long-running-job"))
	require.NoError(t, err)

	var processCount atomic.Int32
	processingDone := make(chan struct{})

	// Slow handler that takes longer than AckWait
	slowHandler := subscription.MessageHandlerFunc(func(c context.Context, msg jetstream.Msg) error {
		count := processCount.Add(1)
		if count == 1 {
			// First processing: simulate long work
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

	// Create worker consumer with short AckWait
	helper, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
		StreamName:      streamName,
		ConsumerPrefix:  "wip-worker",
		SubjectTemplate: "wip.{{.PartitionID}}",
		AckWait:         2 * time.Second, // Short AckWait - would redeliver without WIP
		BatchSize:       1,
	}, wrappedHandler)
	require.NoError(t, err)
	defer helper.Close(context.Background())

	// Start consumption
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-wip", []parti.Partition{{Keys: []string{"test"}}}))

	// Wait for processing to complete
	select {
	case <-processingDone:
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("processing did not complete in time")
	}

	// Give some time for potential redelivery (should not happen)
	time.Sleep(500 * time.Millisecond)

	// Verify message was processed exactly once (no redelivery)
	require.Equal(t, int32(1), processCount.Load(), "message should be processed exactly once with WIP heartbeats")
}
