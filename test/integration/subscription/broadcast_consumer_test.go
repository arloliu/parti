package subscription_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/subscription"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
)

func TestBroadcastConsumer_Integration_ReceivesAllMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create stream with LimitsPolicy (required for broadcast pattern)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "BC_INTEGRATION",
		Subjects:  []string{"bc.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := subscription.BroadcastConsumerConfig{
		StreamName:     "BC_INTEGRATION",
		ConsumerPrefix: "bcint",
		ConsumerID:     "worker-1",
		WildcardFilter: "bc.>",
		BatchSize:      2,
		FetchTimeout:   2 * time.Second,
	}

	var p1Count, p2Count, p3Count int32
	handler := func(ctx context.Context, msg jetstream.Msg) error {
		switch {
		case strings.Contains(msg.Subject(), "region.us-east"):
			atomic.AddInt32(&p1Count, 1)
		case strings.Contains(msg.Subject(), "region.us-west"):
			atomic.AddInt32(&p2Count, 1)
		case strings.Contains(msg.Subject(), "region.eu-central"):
			atomic.AddInt32(&p3Count, 1)
		}

		return nil
	}

	bc, err := subscription.NewBroadcastConsumer(js, cfg, handler)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Close(ctx) })

	// Start consumer - partition list is ignored but can be passed
	parts := []types.Partition{
		{Keys: []string{"region", "us-east"}},
		{Keys: []string{"region", "us-west"}},
		{Keys: []string{"region", "eu-central"}},
	}
	require.NoError(t, bc.UpdateWorkerConsumer(ctx, "worker-1", parts))

	// Wait for consumer to be ready
	time.Sleep(500 * time.Millisecond)

	// Publish 5 messages to each partition
	for i := 0; i < 5; i++ {
		require.NoError(t, nc.Publish("bc.region.us-east.events", []byte("msg")))
		require.NoError(t, nc.Publish("bc.region.us-west.events", []byte("msg")))
		require.NoError(t, nc.Publish("bc.region.eu-central.events", []byte("msg")))
	}
	_ = nc.Flush()

	// Wait for all messages to be processed
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&p1Count) >= 5 &&
			atomic.LoadInt32(&p2Count) >= 5 &&
			atomic.LoadInt32(&p3Count) >= 5 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	require.GreaterOrEqual(t, atomic.LoadInt32(&p1Count), int32(5), "us-east messages")
	require.GreaterOrEqual(t, atomic.LoadInt32(&p2Count), int32(5), "us-west messages")
	require.GreaterOrEqual(t, atomic.LoadInt32(&p3Count), int32(5), "eu-central messages")
}

func TestBroadcastConsumer_Integration_IgnoresPartitionUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "BC_DYNAMIC",
		Subjects:  []string{"bcd.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := subscription.BroadcastConsumerConfig{
		StreamName:     "BC_DYNAMIC",
		ConsumerPrefix: "bcdyn",
		ConsumerID:     "worker-1",
		WildcardFilter: "bcd.>",
		BatchSize:      1,
		FetchTimeout:   1 * time.Second,
	}

	var p1Count, p2Count int32
	handler := func(ctx context.Context, msg jetstream.Msg) error {
		if strings.Contains(msg.Subject(), "p1") {
			atomic.AddInt32(&p1Count, 1)
		} else if strings.Contains(msg.Subject(), "p2") {
			atomic.AddInt32(&p2Count, 1)
		}
		return nil
	}

	bc, err := subscription.NewBroadcastConsumer(js, cfg, handler)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Close(ctx) })

	// Start consumer
	require.NoError(t, bc.UpdateWorkerConsumer(ctx, "worker-1", nil))

	time.Sleep(500 * time.Millisecond)

	// Publish to both partitions
	for i := 0; i < 3; i++ {
		require.NoError(t, nc.Publish("bcd.p1.events", []byte("msg")))
		require.NoError(t, nc.Publish("bcd.p2.events", []byte("msg")))
	}
	_ = nc.Flush()

	time.Sleep(2 * time.Second)

	// Both should be handled initially
	require.GreaterOrEqual(t, atomic.LoadInt32(&p1Count), int32(3))
	require.GreaterOrEqual(t, atomic.LoadInt32(&p2Count), int32(3))

	// "Reassign" to essentially nothing (empty list)
	// This should have NO effect on message receipt as specific partitions are ignored
	parts2 := []types.Partition{}
	require.NoError(t, bc.UpdateWorkerConsumer(ctx, "worker-1", parts2))

	time.Sleep(500 * time.Millisecond)

	// Reset counters
	atomic.StoreInt32(&p1Count, 0)
	atomic.StoreInt32(&p2Count, 0)

	// Publish again
	for i := 0; i < 3; i++ {
		require.NoError(t, nc.Publish("bcd.p1.events", []byte("msg2")))
		require.NoError(t, nc.Publish("bcd.p2.events", []byte("msg2")))
	}
	_ = nc.Flush()

	time.Sleep(2 * time.Second)

	// Both should STILL be handled because filtering is removed
	require.GreaterOrEqual(t, atomic.LoadInt32(&p1Count), int32(3), "p1 should still be received")
	require.GreaterOrEqual(t, atomic.LoadInt32(&p2Count), int32(3), "p2 should still be received")
}

func TestBroadcastConsumer_Integration_Close(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "BC_CLOSE",
		Subjects:  []string{"bcc.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := subscription.BroadcastConsumerConfig{
		StreamName:     "BC_CLOSE",
		ConsumerPrefix: "bcclose",
		ConsumerID:     "worker-1",
		WildcardFilter: "bcc.>",
		BatchSize:      1,
		FetchTimeout:   1 * time.Second,
	}

	var handled int32
	handler := func(ctx context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&handled, 1)
		return nil
	}

	bc, err := subscription.NewBroadcastConsumer(js, cfg, handler)
	require.NoError(t, err)

	parts := []types.Partition{{Keys: []string{"test"}}}
	require.NoError(t, bc.UpdateWorkerConsumer(ctx, "worker-1", parts))

	time.Sleep(500 * time.Millisecond)

	// Publish some messages
	for i := 0; i < 3; i++ {
		require.NoError(t, nc.Publish("bcc.test.events", []byte("msg")))
	}
	_ = nc.Flush()

	// Wait for messages
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&handled) >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&handled), int32(3))

	// Close should succeed
	err = bc.Close(ctx)
	require.NoError(t, err)

	// After close, publishing more messages should not increment counter
	beforeClose := atomic.LoadInt32(&handled)
	for i := 0; i < 3; i++ {
		require.NoError(t, nc.Publish("bcc.test.events", []byte("msg2")))
	}
	_ = nc.Flush()
	time.Sleep(500 * time.Millisecond)

	require.Equal(t, beforeClose, atomic.LoadInt32(&handled), "no messages should be handled after close")
}
