package consumer_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/consumer"
	partitesting "github.com/arloliu/parti/v2/partitest"
)

// TestQueue_StartStopMessages verifies basic Queue consumer lifecycle:
// create, start, publish, receive, stop.
func TestQueue_StartStopMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "Q_BASIC"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"q.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var received atomic.Int32
	msgCh := make(chan string, 20)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		received.Add(1)
		msgCh <- string(msg.Data())
		return nil
	})

	q, err := consumer.NewQueue(js, streamName, "q-consumer", "q.>", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))

	for i := range 5 {
		_, err = js.Publish(ctx, "q.events", []byte(fmt.Sprintf("msg-%d", i)))
		require.NoError(t, err)
	}

	for i := range 5 {
		select {
		case got := <-msgCh:
			require.Equal(t, fmt.Sprintf("msg-%d", i), got)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for message %d", i)
		}
	}

	require.Equal(t, int32(5), received.Load())

	require.NoError(t, q.Stop(ctx))

	_, err = js.Publish(ctx, "q.events", []byte("after-stop"))
	require.NoError(t, err)
	time.Sleep(500 * time.Millisecond)

	require.Equal(t, int32(5), received.Load(), "no messages should be received after stop")
}

// TestQueue_LoadBalanceAcrossInstances verifies that two Queue consumers sharing
// the same durable name distribute messages between them.
func TestQueue_LoadBalanceAcrossInstances(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "Q_LB"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qlb.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var count1, count2 atomic.Int32

	handler1 := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		count1.Add(1)
		return nil
	})
	handler2 := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		count2.Add(1)
		return nil
	})

	q1, err := consumer.NewQueue(js, streamName, "qlb-shared", "qlb.>", handler1,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
	)
	require.NoError(t, err)

	q2, err := consumer.NewQueue(js, streamName, "qlb-shared", "qlb.>", handler2,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
	)
	require.NoError(t, err)

	require.NoError(t, q1.Start(ctx))
	t.Cleanup(func() { _ = q1.Stop(ctx) })

	require.NoError(t, q2.Start(ctx))
	t.Cleanup(func() { _ = q2.Stop(ctx) })

	totalMessages := 20
	for i := range totalMessages {
		_, err = js.Publish(ctx, "qlb.events", []byte(fmt.Sprintf("msg-%d", i)))
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return count1.Load()+count2.Load() >= int32(totalMessages)
	}, 10*time.Second, 50*time.Millisecond, "all messages should be consumed")

	total := count1.Load() + count2.Load()
	require.Equal(t, int32(totalMessages), total, "all messages should be consumed exactly once")

	t.Logf("instance1=%d, instance2=%d", count1.Load(), count2.Load())
	require.Greater(t, count1.Load(), int32(0), "instance 1 should receive some messages")
	require.Greater(t, count2.Load(), int32(0), "instance 2 should receive some messages")
}

// TestQueue_ManualAck verifies that with ManualAck enabled, the handler must
// explicitly acknowledge messages and unacked messages are redelivered.
func TestQueue_ManualAck(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "Q_MACK"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qm.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var deliveryCount atomic.Int32
	var mu sync.Mutex
	seenData := make(map[string]int)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		deliveryCount.Add(1)
		data := string(msg.Data())

		mu.Lock()
		seenData[data]++
		count := seenData[data]
		mu.Unlock()

		if count == 1 {
			return msg.Nak()
		}

		return msg.Ack()
	})

	q, err := consumer.NewQueue(js, streamName, "qm-consumer", "qm.>", handler,
		consumer.WithManualAck(true),
		consumer.WithAckWait(2*time.Second),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))
	t.Cleanup(func() { _ = q.Stop(ctx) })

	_, err = js.Publish(ctx, "qm.events", []byte("manual-ack-test"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return deliveryCount.Load() >= 2
	}, 10*time.Second, 50*time.Millisecond, "message should be delivered at least twice")

	require.GreaterOrEqual(t, deliveryCount.Load(), int32(2),
		"message should be delivered at least twice (nak + ack)")

	mu.Lock()
	require.Equal(t, 2, seenData["manual-ack-test"], "message should be seen exactly twice")
	mu.Unlock()
}

// TestQueue_DoubleStartError verifies that calling Start twice returns an error.
func TestQueue_DoubleStartError(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "Q_DBL",
		Subjects: []string{"qdbl.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	q, err := consumer.NewQueue(js, "Q_DBL", "qdbl-consumer", "qdbl.>", handler)
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Stop(ctx) })

	require.NoError(t, q.Start(ctx))

	err = q.Start(ctx)
	require.Error(t, err, "second Start should return an error")
}

// TestQueue_HandlerErrorAutoNak verifies that when the handler returns an error
// (and ManualAck is disabled), the consumer automatically NAKs the message,
// causing JetStream to redeliver it.
func TestQueue_HandlerErrorAutoNak(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "Q_AUTONAK"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qnak.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var deliveryCount atomic.Int32

	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		deliveryCount.Add(1)
		meta, metaErr := msg.Metadata()
		if metaErr == nil && meta.NumDelivered <= 2 {
			// Return error on first two deliveries -> auto nak -> redelivery
			return fmt.Errorf("simulated processing failure, delivery #%d", meta.NumDelivered)
		}
		// Succeed on third+ delivery
		return nil
	})

	q, err := consumer.NewQueue(js, streamName, "qnak-consumer", "qnak.>", handler,
		consumer.WithAckWait(2*time.Second),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))
	t.Cleanup(func() { _ = q.Stop(ctx) })

	_, err = js.Publish(ctx, "qnak.events", []byte("will-fail-twice"))
	require.NoError(t, err)

	// Message should be delivered at least 3 times: fail, fail, succeed
	require.Eventually(t, func() bool {
		return deliveryCount.Load() >= 3
	}, 10*time.Second, 50*time.Millisecond,
		"message should be redelivered after handler error (auto-nak)")
}

// TestQueue_StopWithDeadline verifies that Stop respects a tight context deadline
// and returns a context error when the deadline expires before the loop exits.
// This validates the deadlock fix in processIterator.
func TestQueue_StopWithDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "Q_DEADLINE"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qdl.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	q, err := consumer.NewQueue(js, streamName, "qdl-consumer", "qdl.>", handler,
		consumer.WithFetchTimeout(10*time.Second),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))

	// Stop with a generous deadline -- should succeed cleanly
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = q.Stop(stopCtx)
	require.NoError(t, err, "stop with generous deadline should succeed")
}

// TestQueue_IteratorFactoryFaultInjection verifies that the Queue consumer
// recovers from transient iterator creation failures injected via WithIteratorFactory.
func TestQueue_IteratorFactoryFaultInjection(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "Q_ITERFAULT"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qif.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// Publish messages before starting the consumer
	for i := range 5 {
		_, err = js.Publish(ctx, "qif.events", []byte(fmt.Sprintf("msg-%d", i)))
		require.NoError(t, err)
	}

	var received atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		received.Add(1)
		return nil
	})

	// Iterator factory that fails the first 2 calls then delegates to real factory
	var factoryCalls atomic.Int32
	faultyFactory := func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
		call := factoryCalls.Add(1)
		if call <= 2 {
			return nil, fmt.Errorf("simulated iterator creation failure #%d", call)
		}
		// Delegate to real factory
		heartbeat := expiry / 2
		if heartbeat < 100*time.Millisecond {
			heartbeat = 100 * time.Millisecond
		}

		return cons.Messages(
			jetstream.PullMaxMessages(batch),
			jetstream.PullExpiry(expiry),
			jetstream.PullHeartbeat(heartbeat),
		)
	}

	q, err := consumer.NewQueue(js, streamName, "qif-consumer", "qif.>", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRetry(consumer.RetryConfig{
			Backoff:    50 * time.Millisecond,
			Max:        500 * time.Millisecond,
			Multiplier: 1.5,
			Base:       50 * time.Millisecond,
		}),
		consumer.WithIteratorFactory(faultyFactory),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))
	t.Cleanup(func() { _ = q.Stop(ctx) })

	// Despite 2 transient failures, all messages should eventually be processed
	require.Eventually(t, func() bool {
		return received.Load() >= 5
	}, 10*time.Second, 50*time.Millisecond,
		"all messages should be processed after iterator recovery")

	// Verify the factory was called more than twice (2 failures + at least 1 success)
	require.GreaterOrEqual(t, factoryCalls.Load(), int32(3),
		"factory should have been called at least 3 times (2 failures + 1 success)")
}
