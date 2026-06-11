package consumer_test

// Tests for Queue consumer auto-recovery feature.
//
// Run with:
//
// go test ./test/integration/consumer/ -run TestQueue_AutoRecovery -v -timeout 120s

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
	// queueRecoveryInactiveThreshold is materially above the nats-go minimum PullExpiry (1s).
	queueRecoveryInactiveThreshold = 3 * time.Second
	// queueRecoveryExpiryWait is >2× InactiveThreshold to ensure deterministic expiry.
	queueRecoveryExpiryWait = 2 * queueRecoveryInactiveThreshold
)

// TestQueue_AutoRecovery_RejectsRecoverFromLastProcessed verifies that
// NewQueue rejects RecoverFromLastProcessed at construction time because
// shared durables make per-process checkpoint tracking unsafe.
func TestQueue_AutoRecovery_RejectsRecoverFromLastProcessed(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	})

	_, err = consumer.NewQueue(js, "stream", "consumer", "subject.>", handler,
		consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
	)
	require.Error(t, err, "NewQueue must reject RecoverFromLastProcessed")
}

// TestQueue_AutoRecovery_WorkQueuePolicy_RecoverFromNew_RejectsAtStart verifies
// that Start returns ErrInvalidConfig when RecoverFromNew is combined with a
// WorkQueuePolicy stream. NATS only allows DeliverAllPolicy on work-queue streams;
// RecoverFromNew maps to DeliverNewPolicy, so every recovery attempt would silently
// fail. The check at Start time surfaces this as a clear configuration error.
func TestQueue_AutoRecovery_WorkQueuePolicy_RecoverFromNew_RejectsAtStart(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "QWQ_VAL",
		Subjects:  []string{"qwq.val.>"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	})

	q, err := consumer.NewQueue(js, "QWQ_VAL", "qwq-val", "qwq.val.>", handler,
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
	)
	require.NoError(t, err, "NewQueue itself must not reject RecoverFromNew (stream type unknown at construction)")

	err = q.Start(ctx)
	require.Error(t, err, "Start must reject RecoverFromNew on a WorkQueuePolicy stream")
	require.ErrorIs(t, err, consumer.ErrInvalidConfig)
}

// TestQueue_Start_IncompatibleConfig_LeavesNoDurable pins startup hygiene:
// the WorkQueue/recovery compatibility check must run BEFORE the durable is
// created. Pre-fix, a failed Start left an exclusive durable on the
// WorkQueuePolicy stream that blocked every other consumer for
// InactiveThreshold (default 24h).
func TestQueue_Start_IncompatibleConfig_LeavesNoDurable(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "QWQ_NODUR",
		Subjects:  []string{"qwq.nodur.>"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	})

	q, err := consumer.NewQueue(js, "QWQ_NODUR", "qwq-nodur", "qwq.nodur.>", handler,
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
	)
	require.NoError(t, err)

	err = q.Start(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, consumer.ErrInvalidConfig)

	// The durable must NOT have been created — a lingering exclusive durable
	// on a WorkQueuePolicy stream blocks every other consumer for up to
	// InactiveThreshold (default 24h), causing NATS err 10100.
	_, consErr := js.Consumer(ctx, "QWQ_NODUR", "qwq-nodur")
	require.Error(t, consErr, "durable must NOT exist after a failed Start (compat check must precede ensureConsumer)")
}

// TestQueue_AutoRecovery_RecoverFromNew_ExplicitDelete verifies that when the
// durable consumer is explicitly deleted, the Queue consumer recovers using
// RecoverFromNew and does NOT replay already-processed messages.
func TestQueue_AutoRecovery_RecoverFromNew_ExplicitDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "QAR_NEW"
	consumerName := "qar-new"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qar.new.>"},
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

	q, err := consumer.NewQueue(js, streamName, consumerName, "qar.new.>", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))
	t.Cleanup(func() { _ = q.Stop(ctx) })

	// Publish and consume initial batch.
	for i := range 3 {
		_, err = js.Publish(ctx, "qar.new.events", fmt.Appendf(nil, "pre-%d", i))
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

	// Delete the consumer while the Queue is running.
	err = js.DeleteConsumer(ctx, streamName, consumerName)
	require.NoError(t, err)

	// Wait for recovery to recreate the consumer with DeliverNewPolicy before
	// publishing so messages land in the consumer's delivery window.
	require.Eventually(t, func() bool {
		_, err := js.Consumer(ctx, streamName, consumerName)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "recovery should recreate the durable consumer")

	// With RecoverFromNew, publish post-delete messages. These MUST be received
	// (recovery works), but the pre-delete messages should NOT be replayed.
	for i := range 3 {
		_, err = js.Publish(ctx, "qar.new.events", fmt.Appendf(nil, "post-%d", i))
		require.NoError(t, err)
	}

	// Wait for the post-delete messages to arrive.
	require.Eventually(t, func() bool {
		return received.Load() >= beforeDelete+3
	}, 30*time.Second, 100*time.Millisecond, "post-recovery messages should be received")

	// Total should be exactly 6 — no replay of pre-delete messages.
	require.Equal(t, int32(6), received.Load(),
		"RecoverFromNew must not replay pre-delete messages")
}

// TestQueue_AutoRecovery_RecoverFromBeginning_ExplicitDelete verifies that
// RecoverFromBeginning causes a full backlog replay after deletion.
func TestQueue_AutoRecovery_RecoverFromBeginning_ExplicitDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "QAR_BEG"
	consumerName := "qar-beg"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qar.beg.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var received atomic.Int32
	msgCh := make(chan string, 100)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		received.Add(1)
		msgCh <- string(msg.Data())
		return nil
	})

	q, err := consumer.NewQueue(js, streamName, consumerName, "qar.beg.>", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromBeginning),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))
	t.Cleanup(func() { _ = q.Stop(ctx) })

	// Publish and consume initial batch.
	for i := range 3 {
		_, err = js.Publish(ctx, "qar.beg.events", fmt.Appendf(nil, "before-%d", i))
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool {
		return received.Load() >= 3
	}, 10*time.Second, 50*time.Millisecond, "initial messages should be consumed")

	// Delete the consumer while the Queue is running.
	err = js.DeleteConsumer(ctx, streamName, consumerName)
	require.NoError(t, err)

	// With RecoverFromBeginning, all 3 pre-delete messages should be replayed.
	require.Eventually(t, func() bool {
		return received.Load() >= 6
	}, 30*time.Second, 100*time.Millisecond,
		"RecoverFromBeginning should replay all pre-delete messages; got %d", received.Load())
}

// TestQueue_AutoRecovery_PassiveExpiry verifies that a slow handler causing
// InactiveThreshold expiry on the server (Path B: ErrNoHeartbeat) triggers
// recovery via the burst-detection path.
func TestQueue_AutoRecovery_PassiveExpiry(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "QAR_EXP"
	consumerName := "qar-exp"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qar.exp.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	handlerReady := make(chan struct{})   // signals first message received
	handlerRelease := make(chan struct{}) // signals handler to return

	var received atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		received.Add(1)
		if received.Load() == 1 {
			// Signal that handler has the message and is about to stall.
			close(handlerReady)
			// Block here, simulating a slow handler that exceeds InactiveThreshold.
			<-handlerRelease
		}

		return nil
	})

	fetchTimeout := 1 * time.Second // nats-go minimum PullExpiry

	q, err := consumer.NewQueue(js, streamName, consumerName, "qar.exp.>", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(fetchTimeout),
		consumer.WithInactiveThreshold(queueRecoveryInactiveThreshold),
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))
	t.Cleanup(func() { _ = q.Stop(ctx) })

	// Publish first message to trigger the slow handler.
	_, err = js.Publish(ctx, "qar.exp.events", []byte("first"))
	require.NoError(t, err)

	// Wait for handler to be holding the first message.
	select {
	case <-handlerReady:
	case <-time.After(15 * time.Second):
		t.Fatal("handler never received first message")
	}

	// Handler is now stalling. Wait for > InactiveThreshold so the consumer
	// expires server-side. No pull request is in flight during this window, so
	// the server classifies the consumer as inactive.
	t.Logf("Sleeping %v to let InactiveThreshold(%v) expire...", queueRecoveryExpiryWait, queueRecoveryInactiveThreshold)
	time.Sleep(queueRecoveryExpiryWait)

	// Release the handler.
	close(handlerRelease)

	// After the handler returns, the consumer loop re-enters processIterator.
	// ErrNoHeartbeat fires because the consumer expired. The burst-detection path
	// calls consumer.Info() which returns ErrConsumerNotFound, triggering recovery.
	// With RecoverFromNew, a new message published after expiry should be received.
	time.Sleep(200 * time.Millisecond)
	beforeRecovery := received.Load()

	_, err = js.Publish(ctx, "qar.exp.events", []byte("after-expiry"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return received.Load() > beforeRecovery
	}, 30*time.Second, 100*time.Millisecond,
		"post-expiry message should be received after recovery; count=%d", received.Load())
}

// TestQueue_AutoRecovery_ActivePullDelete verifies Path A: when the consumer is
// deleted while Next() is blocking (no messages in stream), the consumer receives
// ErrConsumerDeleted and triggers immediate recovery without waiting for burst
// escalation threshold.
func TestQueue_AutoRecovery_ActivePullDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "QAR_APD"
	consumerName := "qar-apd"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qar.apd.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var received atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		received.Add(1)
		return nil
	})

	q, err := consumer.NewQueue(js, streamName, consumerName, "qar.apd.>", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))
	t.Cleanup(func() { _ = q.Stop(ctx) })

	// Wait for consumer to be registered and Next() to be blocking.
	require.Eventually(t, func() bool {
		_, err := js.Consumer(ctx, streamName, consumerName)
		return err == nil
	}, 5*time.Second, 100*time.Millisecond, "consumer should be registered")
	time.Sleep(200 * time.Millisecond) // give time for Next() to block

	// Delete while Next() is blocking — triggers ErrConsumerDeleted (~50ms).
	err = js.DeleteConsumer(ctx, streamName, consumerName)
	require.NoError(t, err)

	// Wait for recovery to recreate.
	require.Eventually(t, func() bool {
		_, err := js.Consumer(ctx, streamName, consumerName)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "recovery should recreate the consumer")

	// Post-recovery message should be received.
	_, err = js.Publish(ctx, "qar.apd.events", []byte("post-active-delete"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return received.Load() >= 1
	}, 30*time.Second, 100*time.Millisecond,
		"post-recovery message should be received after active-pull delete")
}

// TestQueue_AutoRecovery_WorkQueuePolicy_RecoverFromBeginning_Succeeds verifies
// that when a stream uses WorkQueuePolicy, RecoverFromBeginning successfully
// recreates the consumer after deletion. DeliverAllPolicy is the only deliver
// policy WorkQueue streams accept, and RecoverFromBeginning maps exactly to it.
func TestQueue_AutoRecovery_WorkQueuePolicy_RecoverFromBeginning_Succeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "QWQ_BEG"
	consumerName := "qwq-beg"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"qwq.beg.>"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var received atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		received.Add(1)
		return nil
	})

	q, err := consumer.NewQueue(js, streamName, consumerName, "qwq.beg.>", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromBeginning),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))
	t.Cleanup(func() { _ = q.Stop(ctx) })

	// Publish and consume initial messages. WorkQueuePolicy removes acked
	// messages from the stream, so no backlog remains after consumption.
	for i := range 3 {
		_, err = js.Publish(ctx, "qwq.beg.events", fmt.Appendf(nil, "init-%d", i))
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool {
		return received.Load() >= 3
	}, 10*time.Second, 50*time.Millisecond, "initial messages should be received")

	// Delete the consumer while the Queue is running.
	require.NoError(t, js.DeleteConsumer(ctx, streamName, consumerName))

	// RecoverFromBeginning maps to DeliverAllPolicy, which WorkQueue streams
	// accept. Wait for the recovery controller to recreate the durable.
	require.Eventually(t, func() bool {
		_, err := js.Consumer(ctx, streamName, consumerName)
		return err == nil
	}, 10*time.Second, 50*time.Millisecond,
		"recovery must recreate consumer: DeliverAllPolicy is valid on WorkQueue streams")

	// Publish post-recovery messages. They must be delivered to the recreated consumer.
	for i := range 3 {
		_, err = js.Publish(ctx, "qwq.beg.events", fmt.Appendf(nil, "post-%d", i))
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool {
		return received.Load() >= 6
	}, 15*time.Second, 50*time.Millisecond,
		"post-recovery messages should be received; got %d", received.Load())
}

// TestQueue_AutoRecovery_DisabledPreservesDefaultBehavior verifies that when
// RecoveryStrategy is not set (Disabled by default), the Queue consumer retries
// with backoff on iterator errors but does NOT recreate the consumer with a
// strategy-adjusted DeliverPolicy.
func TestQueue_AutoRecovery_DisabledPreservesDefaultBehavior(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "QAR_DIS"
	consumerName := "qar-dis"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qar.dis.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var received atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		received.Add(1)
		return nil
	})

	// Create Queue WITHOUT WithRecoveryStrategy — defaults to RecoveryDisabled.
	q, err := consumer.NewQueue(js, streamName, consumerName, "qar.dis.>", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)

	require.NoError(t, q.Start(ctx))
	t.Cleanup(func() { _ = q.Stop(ctx) })

	// Publish and consume a message to confirm it works.
	_, err = js.Publish(ctx, "qar.dis.events", []byte("before"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return received.Load() >= 1
	}, 10*time.Second, 50*time.Millisecond)

	// Delete the consumer. With Disabled strategy, the consumer should NOT be
	// automatically recreated via the recovery controller.
	err = js.DeleteConsumer(ctx, streamName, consumerName)
	require.NoError(t, err)

	// Wait enough time for recovery to have triggered if it were enabled.
	time.Sleep(3 * time.Second)

	// The consumer should NOT have been recreated by the recovery controller.
	_, consErr := js.Consumer(ctx, streamName, consumerName)
	require.Error(t, consErr, "consumer should NOT be recreated when recovery is disabled")
}
