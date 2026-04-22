package consumer_test

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
	staticRecoveryInactiveThreshold = 3 * time.Second
	staticRecoveryExpiryWait        = 2 * staticRecoveryInactiveThreshold
)

// TestStatic_AutoRecovery_ManualAck_RecoverFromLastProcessed_ExplicitDelete verifies
// that RecoverFromLastProcessed works correctly when ManualAck=true. The checkpoint is
// advanced by the framework-intercepted msg.Ack() call inside the handler.
func TestStatic_AutoRecovery_ManualAck_RecoverFromLastProcessed_ExplicitDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "SAR_MLKP"
	consumerName := "sar-mlkp-0"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"sar.mlkp.*"},
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

	sc, err := consumer.NewStatic(js, streamName, consumerName, "sar.mlkp.{{partition}}", 2, 0, handler,
		consumer.WithManualAck(true),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
	)
	require.NoError(t, err, "ManualAck=true + RecoverFromLastProcessed must be accepted")
	require.NoError(t, sc.Start(ctx))
	t.Cleanup(func() { _ = sc.Stop(ctx) })

	for i := range 3 {
		_, err = js.Publish(ctx, "sar.mlkp.0", fmt.Appendf(nil, "pre-%d", i))
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

	// Wait for ack floor to advance, confirming the intercepted Ack advanced the checkpoint.
	require.Eventually(t, func() bool {
		cons, err := js.Consumer(ctx, streamName, consumerName)
		if err != nil {
			return false
		}
		info, err := cons.Info(ctx)
		if err != nil {
			return false
		}

		return info.AckFloor.Stream >= 3
	}, 10*time.Second, 25*time.Millisecond, "ack floor should advance before delete")

	err = js.DeleteConsumer(ctx, streamName, consumerName)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, err := js.Consumer(ctx, streamName, consumerName)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "recovery should recreate the static durable")

	for i := range 3 {
		_, err = js.Publish(ctx, "sar.mlkp.0", fmt.Appendf(nil, "post-%d", i))
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return received.Load() == beforeDelete+3
	}, 30*time.Second, 100*time.Millisecond)

	require.Equal(t, int32(6), received.Load(),
		"RecoverFromLastProcessed with ManualAck=true must not replay already-acked messages")
}

func TestStatic_AutoRecovery_RecoverFromNew_ExplicitDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "SAR_NEW"
	consumerName := "sar-new-0"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"sar.new.*"},
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

	sc, err := consumer.NewStatic(js, streamName, consumerName, "sar.new.{{partition}}", 2, 0, handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
	)
	require.NoError(t, err)
	require.NoError(t, sc.Start(ctx))
	t.Cleanup(func() { _ = sc.Stop(ctx) })

	for i := range 3 {
		_, err = js.Publish(ctx, "sar.new.0", fmt.Appendf(nil, "pre-%d", i))
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

	err = js.DeleteConsumer(ctx, streamName, consumerName)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, err := js.Consumer(ctx, streamName, consumerName)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "recovery should recreate the static durable")

	for i := range 3 {
		_, err = js.Publish(ctx, "sar.new.0", fmt.Appendf(nil, "post-%d", i))
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return received.Load() >= beforeDelete+3
	}, 30*time.Second, 100*time.Millisecond)

	require.Equal(t, int32(6), received.Load())
}

func TestStatic_AutoRecovery_RecoverFromLastProcessed_ExplicitDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "SAR_LKP"
	consumerName := "sar-lkp-0"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"sar.lkp.*"},
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

	sc, err := consumer.NewStatic(js, streamName, consumerName, "sar.lkp.{{partition}}", 2, 0, handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
	)
	require.NoError(t, err)
	require.NoError(t, sc.Start(ctx))
	t.Cleanup(func() { _ = sc.Stop(ctx) })

	for i := range 3 {
		_, err = js.Publish(ctx, "sar.lkp.0", fmt.Appendf(nil, "pre-%d", i))
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
		cons, err := js.Consumer(ctx, streamName, consumerName)
		if err != nil {
			return false
		}
		info, err := cons.Info(ctx)
		if err != nil {
			return false
		}

		return info.AckFloor.Stream >= 3
	}, 10*time.Second, 25*time.Millisecond, "ack floor should advance before delete")

	err = js.DeleteConsumer(ctx, streamName, consumerName)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, err := js.Consumer(ctx, streamName, consumerName)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "recovery should recreate the static durable")

	for i := range 3 {
		_, err = js.Publish(ctx, "sar.lkp.0", fmt.Appendf(nil, "post-%d", i))
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return received.Load() == beforeDelete+3
	}, 30*time.Second, 100*time.Millisecond)

	require.Equal(t, int32(6), received.Load())
}

func TestStatic_AutoRecovery_PassiveExpiry(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "SAR_EXP"
	consumerName := "sar-exp-0"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"sar.exp.*"},
		Storage:  jetstream.MemoryStorage,
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

	sc, err := consumer.NewStatic(js, streamName, consumerName, "sar.exp.{{partition}}", 2, 0, handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
		consumer.WithInactiveThreshold(staticRecoveryInactiveThreshold),
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
	)
	require.NoError(t, err)
	require.NoError(t, sc.Start(ctx))
	t.Cleanup(func() { _ = sc.Stop(ctx) })

	_, err = js.Publish(ctx, "sar.exp.0", []byte("trigger"))
	require.NoError(t, err)

	select {
	case <-handlerReady:
	case <-time.After(15 * time.Second):
		t.Fatal("handler never received trigger message")
	}

	time.Sleep(staticRecoveryExpiryWait)

	close(handlerRelease)

	beforeRecovery := received.Load()
	_, err = js.Publish(ctx, "sar.exp.0", []byte("after-expiry"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return received.Load() > beforeRecovery
	}, 30*time.Second, 100*time.Millisecond)
}

// TestStatic_AutoRecovery_WorkQueuePolicy_RecoverFromNew_RejectsAtStart verifies
// that Start returns ErrInvalidConfig when RecoverFromNew is combined with a
// WorkQueuePolicy stream.
func TestStatic_AutoRecovery_WorkQueuePolicy_RecoverFromNew_RejectsAtStart(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "SWQ_NEW",
		Subjects:  []string{"swq.new.*"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	})

	s, err := consumer.NewStatic(js, "SWQ_NEW", "swq-new-0", "swq.new.{{partition}}", 1, 0, handler,
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
	)
	require.NoError(t, err)

	err = s.Start(ctx)
	require.Error(t, err, "Start must reject RecoverFromNew on a WorkQueuePolicy stream")
	require.ErrorIs(t, err, consumer.ErrInvalidConfig)
}

// TestStatic_AutoRecovery_WorkQueuePolicy_RecoverFromLastProcessed_RejectsAtStart
// verifies that Start returns ErrInvalidConfig when RecoverFromLastProcessed is
// combined with a WorkQueuePolicy stream.
func TestStatic_AutoRecovery_WorkQueuePolicy_RecoverFromLastProcessed_RejectsAtStart(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "SWQ_LKP",
		Subjects:  []string{"swq.lkp.*"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	})

	s, err := consumer.NewStatic(js, "SWQ_LKP", "swq-lkp-0", "swq.lkp.{{partition}}", 1, 0, handler,
		consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
	)
	require.NoError(t, err)

	err = s.Start(ctx)
	require.Error(t, err, "Start must reject RecoverFromLastProcessed on a WorkQueuePolicy stream")
	require.ErrorIs(t, err, consumer.ErrInvalidConfig)
}
