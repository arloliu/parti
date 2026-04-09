package consumer_test

// Tests for Broadcast consumer auto-recovery feature.
//
// Run with:
//
// go test ./test/integration/consumer/ -run TestBroadcast_AutoRecovery -v -timeout 120s

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/consumer"
	partitesting "github.com/arloliu/parti/v2/partitest"
)

const (
	// bcRecoveryInactiveThreshold is materially above the nats-go minimum PullExpiry (1s).
	bcRecoveryInactiveThreshold = 3 * time.Second
	// bcRecoveryExpiryWait is >2× InactiveThreshold so expiry is deterministic.
	bcRecoveryExpiryWait = 2 * bcRecoveryInactiveThreshold
)

// computeBroadcastDurable derives the durable name for a Broadcast consumer.
// Format: <prefix>_broadcast_<instanceID>.
func computeBroadcastDurable(prefix, instanceID string) string {
	return fmt.Sprintf("%s_broadcast_%s", prefix, instanceID)
}

// TestBroadcast_AutoRecovery_ManualAck_RecoverFromLastProcessed_ExplicitDelete verifies
// that RecoverFromLastProcessed works correctly when ManualAck=true on a Broadcast consumer.
func TestBroadcast_AutoRecovery_ManualAck_RecoverFromLastProcessed_ExplicitDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	instanceID := "bc-mlkp-inst"
	streamName := "BC_MLKP"
	consumerPrefix := "bc-mlkp"
	durableName := computeBroadcastDurable(consumerPrefix, instanceID)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"bc.mlkp.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var received atomic.Int32
	msgCh := make(chan string, 20)
	// ManualAck=true handler: calls msg.Ack() explicitly.
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		received.Add(1)
		msgCh <- string(msg.Data())
		return msg.Ack()
	})

	bc, err := consumer.NewBroadcast(js, streamName, consumerPrefix, "bc.mlkp.>", handler,
		consumer.WithInstanceID(instanceID),
		consumer.WithManualAck(true),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
	)
	require.NoError(t, err, "ManualAck=true + RecoverFromLastProcessed must be accepted")
	require.NoError(t, bc.Start(ctx))
	t.Cleanup(func() { _ = bc.Stop(ctx) })

	for i := range 3 {
		_, err = js.Publish(ctx, "bc.mlkp.x", []byte(fmt.Sprintf("pre-%d", i)))
		require.NoError(t, err)
	}
	for range 3 {
		select {
		case <-msgCh:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for pre-delete message")
		}
	}
	beforeDelete := received.Load()

	require.Eventually(t, func() bool {
		cons, err := js.Consumer(ctx, streamName, durableName)
		if err != nil {
			return false
		}
		info, err := cons.Info(ctx)
		if err != nil {
			return false
		}

		return info.AckFloor.Stream >= 3
	}, 10*time.Second, 25*time.Millisecond, "ack floor should advance before delete")

	err = js.DeleteConsumer(ctx, streamName, durableName)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, err := js.Consumer(ctx, streamName, durableName)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "recovery should recreate the broadcast durable")

	for i := range 3 {
		_, err = js.Publish(ctx, "bc.mlkp.x", []byte(fmt.Sprintf("post-%d", i)))
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return received.Load() >= beforeDelete+3
	}, 30*time.Second, 100*time.Millisecond)

	require.LessOrEqual(t, received.Load(), int32(6),
		"RecoverFromLastProcessed with ManualAck=true must not replay already-acked messages")
}

// TestBroadcast_AutoRecovery_RecoverFromNew_ExplicitDelete verifies that when the
// durable consumer is explicitly deleted, the Broadcast consumer recovers using
// RecoverFromNew and does NOT replay already-processed messages.
func TestBroadcast_AutoRecovery_RecoverFromNew_ExplicitDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "BCAR_NEW"
	consPrefix := "bcar-new"
	instanceID := "inst-1"
	durableName := computeBroadcastDurable(consPrefix, instanceID)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"bcar.new.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var received atomic.Int32
	msgCh := make(chan string, 20)
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		received.Add(1)
		msgCh <- string(msg.Data())
		return nil
	})

	bc, err := consumer.NewBroadcast(js, streamName, consPrefix, "bcar.new.>", handler,
		consumer.WithInstanceID(instanceID),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
	)
	require.NoError(t, err)

	require.NoError(t, bc.Start(ctx))
	t.Cleanup(func() { _ = bc.Stop(ctx) })

	// Publish and consume initial batch.
	for i := range 3 {
		_, err = js.Publish(ctx, "bcar.new.events", []byte(fmt.Sprintf("pre-%d", i)))
		require.NoError(t, err)
	}
	for range 3 {
		select {
		case <-msgCh:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for pre-delete message")
		}
	}
	beforeDelete := received.Load()
	require.Equal(t, int32(3), beforeDelete)

	// Explicitly delete the durable consumer.
	err = js.DeleteConsumer(ctx, streamName, durableName)
	require.NoError(t, err)

	// Wait for recovery to recreate the consumer with DeliverNewPolicy before
	// publishing so messages land in the consumer's delivery window.
	require.Eventually(t, func() bool {
		_, err := js.Consumer(ctx, streamName, durableName)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "recovery should recreate the durable consumer")

	// With RecoverFromNew, publish post-delete messages. These MUST be received,
	// but pre-delete messages MUST NOT be replayed.
	for i := range 3 {
		_, err = js.Publish(ctx, "bcar.new.events", []byte(fmt.Sprintf("post-%d", i)))
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return received.Load() >= beforeDelete+3
	}, 30*time.Second, 100*time.Millisecond, "post-recovery messages should be received")

	require.Equal(t, int32(6), received.Load(),
		"RecoverFromNew must not replay pre-delete messages")
}

// TestBroadcast_AutoRecovery_RecoverFromLastProcessed_ExplicitDelete verifies
// that with RecoverFromLastProcessed the consumer resumes from the ack floor
// after explicit deletion, without replaying already-acknowledged messages.
func TestBroadcast_AutoRecovery_RecoverFromLastProcessed_ExplicitDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "BCAR_LKP"
	consPrefix := "bcar-lkp"
	instanceID := "inst-1"
	durableName := computeBroadcastDurable(consPrefix, instanceID)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"bcar.lkp.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var received atomic.Int32
	msgCh := make(chan string, 20)
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		received.Add(1)
		msgCh <- string(msg.Data())
		return nil
	})

	bc, err := consumer.NewBroadcast(js, streamName, consPrefix, "bcar.lkp.>", handler,
		consumer.WithInstanceID(instanceID),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
	)
	require.NoError(t, err)

	require.NoError(t, bc.Start(ctx))
	t.Cleanup(func() { _ = bc.Stop(ctx) })

	// Publish and consume initial batch. All messages will be auto-acked;
	// the checkpoint will track the highest acked stream sequence.
	for i := range 3 {
		_, err = js.Publish(ctx, "bcar.lkp.events", []byte(fmt.Sprintf("pre-%d", i)))
		require.NoError(t, err)
	}
	for range 3 {
		select {
		case <-msgCh:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for pre-delete message")
		}
	}
	beforeDelete := received.Load()
	// Give auto-acks time to be published so the checkpoint advances.
	time.Sleep(100 * time.Millisecond)

	// Explicitly delete the durable consumer.
	err = js.DeleteConsumer(ctx, streamName, durableName)
	require.NoError(t, err)

	// Publish post-delete messages.
	time.Sleep(100 * time.Millisecond)
	for i := range 3 {
		_, err = js.Publish(ctx, "bcar.lkp.events", []byte(fmt.Sprintf("post-%d", i)))
		require.NoError(t, err)
	}

	// With RecoverFromLastProcessed the consumer should resume from the ack floor
	// and receive the 3 post-delete messages without replaying the 3 pre-delete ones.
	require.Eventually(t, func() bool {
		return received.Load() >= beforeDelete+3
	}, 30*time.Second, 100*time.Millisecond, "post-recovery messages should arrive")

	// At most 6 messages total — no replay of acked messages.
	require.LessOrEqual(t, received.Load(), int32(6),
		"RecoverFromLastProcessed must not replay already-acked messages")
}

// TestBroadcast_AutoRecovery_PassiveExpiry verifies that a slow handler causing
// InactiveThreshold expiry (Path B: ErrNoHeartbeat) triggers recovery and that
// the consumer starts receiving new messages again after recovery.
func TestBroadcast_AutoRecovery_PassiveExpiry(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "BCAR_EXP"
	consPrefix := "bcar-exp"
	instanceID := "inst-1"

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"bcar.exp.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	handlerReady := make(chan struct{})
	handlerRelease := make(chan struct{})

	var received atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		received.Add(1)
		if received.Load() == 1 {
			close(handlerReady)
			<-handlerRelease
		}
		return nil
	})

	bc, err := consumer.NewBroadcast(js, streamName, consPrefix, "bcar.exp.>", handler,
		consumer.WithInstanceID(instanceID),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
		consumer.WithInactiveThreshold(bcRecoveryInactiveThreshold),
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
	)
	require.NoError(t, err)

	require.NoError(t, bc.Start(ctx))
	t.Cleanup(func() { _ = bc.Stop(ctx) })

	// Trigger first message so the handler stalls.
	_, err = js.Publish(ctx, "bcar.exp.events", []byte("trigger"))
	require.NoError(t, err)

	select {
	case <-handlerReady:
	case <-time.After(15 * time.Second):
		t.Fatal("handler never received trigger message")
	}

	// Handler is stalling — no Next() is called, so no pull is active on the server.
	// Wait for InactiveThreshold to expire the consumer server-side.
	t.Logf("Sleeping %v to let InactiveThreshold(%v) expire...", bcRecoveryExpiryWait, bcRecoveryInactiveThreshold)
	time.Sleep(bcRecoveryExpiryWait)

	close(handlerRelease)

	// After handler returns, ErrNoHeartbeat fires. Burst detection calls
	// consumer.Info() → ErrConsumerNotFound → recovery triggers.
	// With RecoverFromNew, a new message should be received post-recovery.
	time.Sleep(200 * time.Millisecond)
	beforeRecovery := received.Load()

	_, err = js.Publish(ctx, "bcar.exp.events", []byte("after-expiry"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return received.Load() > beforeRecovery
	}, 30*time.Second, 100*time.Millisecond,
		"post-expiry message should be received after recovery; count=%d", received.Load())
}
