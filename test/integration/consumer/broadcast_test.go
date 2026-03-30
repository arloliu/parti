package consumer_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/consumer"
	partitesting "github.com/arloliu/parti/v2/partitest"
)

// TestBroadcast_ReceivesAllMessages verifies that a Broadcast consumer receives
// messages from all subjects matching the wildcard filter.
func TestBroadcast_ReceivesAllMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "BC_ALL",
		Subjects:  []string{"bc.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var eastCount, westCount, euCount atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		switch {
		case strings.Contains(msg.Subject(), "region.us-east"):
			eastCount.Add(1)
		case strings.Contains(msg.Subject(), "region.us-west"):
			westCount.Add(1)
		case strings.Contains(msg.Subject(), "region.eu-central"):
			euCount.Add(1)
		}

		return nil
	})

	bc, err := consumer.NewBroadcast(js, "BC_ALL", "bcall", "bc.>", handler,
		consumer.WithInstanceID("inst-1"),
		consumer.WithBatchSize(2),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Stop(ctx) })

	require.NoError(t, bc.Start(ctx))

	for range 5 {
		require.NoError(t, nc.Publish("bc.region.us-east.events", []byte("msg")))
		require.NoError(t, nc.Publish("bc.region.us-west.events", []byte("msg")))
		require.NoError(t, nc.Publish("bc.region.eu-central.events", []byte("msg")))
	}
	require.NoError(t, nc.Flush())

	require.Eventually(t, func() bool {
		return eastCount.Load() >= 5 && westCount.Load() >= 5 && euCount.Load() >= 5
	}, 10*time.Second, 50*time.Millisecond, "all regions should receive 5 messages")
}

// TestBroadcast_IgnoresPartitionUpdates verifies that calling UpdateWorkerConsumer
// with an empty partition list has no effect on message receipt.
func TestBroadcast_IgnoresPartitionUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "BC_IGN",
		Subjects:  []string{"bci.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var p1Count, p2Count atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		switch {
		case strings.Contains(msg.Subject(), "p1"):
			p1Count.Add(1)
		case strings.Contains(msg.Subject(), "p2"):
			p2Count.Add(1)
		}
		return nil
	})

	bc, err := consumer.NewBroadcast(js, "BC_IGN", "bcign", "bci.>", handler,
		consumer.WithInstanceID("inst-1"),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Stop(ctx) })

	require.NoError(t, bc.Start(ctx))

	for range 3 {
		require.NoError(t, nc.Publish("bci.p1.events", []byte("msg")))
		require.NoError(t, nc.Publish("bci.p2.events", []byte("msg")))
	}
	require.NoError(t, nc.Flush())

	require.Eventually(t, func() bool {
		return p1Count.Load() >= 3 && p2Count.Load() >= 3
	}, 5*time.Second, 50*time.Millisecond)

	require.NoError(t, bc.UpdateWorkerConsumer(ctx, "inst-1", nil))

	p1Count.Store(0)
	p2Count.Store(0)

	for range 3 {
		require.NoError(t, nc.Publish("bci.p1.events", []byte("msg2")))
		require.NoError(t, nc.Publish("bci.p2.events", []byte("msg2")))
	}
	require.NoError(t, nc.Flush())

	require.Eventually(t, func() bool {
		return p1Count.Load() >= 3 && p2Count.Load() >= 3
	}, 5*time.Second, 50*time.Millisecond, "p1 and p2 should still be received after empty update")
}

// TestBroadcast_StopPreventsDelivery verifies that after Stop, no more messages
// are delivered to the handler.
func TestBroadcast_StopPreventsDelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "BC_STOP",
		Subjects:  []string{"bcs.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var handled atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		handled.Add(1)
		return nil
	})

	bc, err := consumer.NewBroadcast(js, "BC_STOP", "bcstop", "bcs.>", handler,
		consumer.WithInstanceID("inst-1"),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
	)
	require.NoError(t, err)

	require.NoError(t, bc.Start(ctx))

	for range 3 {
		require.NoError(t, nc.Publish("bcs.test.events", []byte("msg")))
	}
	require.NoError(t, nc.Flush())

	require.Eventually(t, func() bool {
		return handled.Load() >= 3
	}, 5*time.Second, 50*time.Millisecond)

	require.NoError(t, bc.Stop(ctx))

	beforeStop := handled.Load()
	for range 3 {
		require.NoError(t, nc.Publish("bcs.test.events", []byte("msg2")))
	}
	require.NoError(t, nc.Flush())
	require.Never(t, func() bool {
		return handled.Load() != beforeStop
	}, 500*time.Millisecond, 25*time.Millisecond, "no messages should be handled after stop")
}

// TestBroadcast_TwoInstancesFanOut verifies that two Broadcast consumers with
// different InstanceIDs each receive ALL messages (true fan-out guarantee).
// This is the core fan-out contract: unlike Queue (load-balance), Broadcast
// delivers every message to every instance.
func TestBroadcast_TwoInstancesFanOut(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "BC_FAN",
		Subjects:  []string{"bcfan.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
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

	bc1, err := consumer.NewBroadcast(js, "BC_FAN", "bcfan", "bcfan.>", handler1,
		consumer.WithInstanceID("inst-1"),
		consumer.WithBatchSize(2),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)

	bc2, err := consumer.NewBroadcast(js, "BC_FAN", "bcfan", "bcfan.>", handler2,
		consumer.WithInstanceID("inst-2"),
		consumer.WithBatchSize(2),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)

	require.NoError(t, bc1.Start(ctx))
	t.Cleanup(func() { _ = bc1.Stop(ctx) })
	require.NoError(t, bc2.Start(ctx))
	t.Cleanup(func() { _ = bc2.Stop(ctx) })

	totalMessages := int32(10)
	for i := range totalMessages {
		require.NoError(t, nc.Publish("bcfan.events", []byte(fmt.Sprintf("msg-%d", i))))
	}
	require.NoError(t, nc.Flush())

	// Both instances must receive ALL messages (fan-out, not load-balance)
	require.Eventually(t, func() bool {
		return count1.Load() >= totalMessages && count2.Load() >= totalMessages
	}, 10*time.Second, 50*time.Millisecond, "both instances should receive all messages")

	t.Logf("instance1=%d, instance2=%d", count1.Load(), count2.Load())
	require.Equal(t, totalMessages, count1.Load(), "instance 1 should receive all messages")
	require.Equal(t, totalMessages, count2.Load(), "instance 2 should receive all messages")
}
