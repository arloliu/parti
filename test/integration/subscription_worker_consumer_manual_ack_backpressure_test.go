package integration_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/subscription"
	partitesting "github.com/arloliu/parti/testing"
)

// TestWorkerConsumer_ManualAck_Backpressure verifies that ManualAck mode allows
// the handler to implement backpressure via a bounded internal queue, and that
// in-flight messages do not exceed capacity while all messages are eventually ACKed.
func TestWorkerConsumer_ManualAck_Backpressure(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := context.Background()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create stream and subject
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "manual-stream", Subjects: []string{"jobs.*"}})
	require.NoError(t, err)

	// Publish a batch of messages
	total := 10
	for i := 0; i < total; i++ {
		_, err = js.Publish(ctx, "jobs.1", []byte("m"))
		require.NoError(t, err)
	}

	// Bounded work queue to enforce backpressure in handler
	capacity := 2
	workCh := make(chan jetstream.Msg, capacity)
	var maxDepth atomic.Int32
	var acked atomic.Int32

	// Background worker that processes messages slowly and ACKs them, extending AckWait if needed
	go func() {
		// Allow the handler to enqueue up to capacity before we start draining,
		// ensuring we observe backpressure at the intended depth deterministically.
		time.Sleep(50 * time.Millisecond)
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; i < total; i++ {
			msg := <-workCh
			// Optional: extend AckWait once for long work
			_ = msg.InProgress()
			// Simulate processing
			time.Sleep(120 * time.Millisecond)
			_ = msg.Ack()
			acked.Add(1)
		}
	}()

	// Manual-ack handler: enqueue or block to apply backpressure
	handler := subscription.MessageHandlerFunc(func(c context.Context, msg jetstream.Msg) error {
		workCh <- msg // block when full; applies backpressure to the pull loop
		if d := int32(len(workCh)); d > maxDepth.Load() {
			maxDepth.Store(d)
		}

		return nil // manual ack mode: do not Ack here
	})

	// Configure helper with ManualAck enabled and MaxAckPending capped to capacity
	helper, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
		StreamName:      "manual-stream",
		ConsumerPrefix:  "wkr",
		SubjectTemplate: "jobs.{{.PartitionID}}",
		BatchSize:       5,
		AckWait:         2 * time.Second,
		MaxAckPending:   capacity,
		ManualAck:       true,
	}, handler)
	require.NoError(t, err)
	defer helper.Close(context.Background())

	// Start consumption for partition 1
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-MA", []parti.Partition{{Keys: []string{"1"}}}))

	// Wait for all messages to be ACKed
	require.Eventually(t, func() bool { return int(acked.Load()) == total }, 5*time.Second, 50*time.Millisecond)

	// Verify the observed queue depth never exceeded capacity (backpressure effective)
	require.Equal(t, int32(capacity), maxDepth.Load(), "max queue depth should equal capacity")

	// At the end, consumer should have no pending acks
	info, err := helper.WorkerConsumerInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, info.NumAckPending)
}
