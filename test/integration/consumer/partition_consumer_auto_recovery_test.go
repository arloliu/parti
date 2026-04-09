package consumer_test

// Tests for partition consumer (Dynamic backends) auto-recovery feature.
//
// The Dynamic consumer routes through durable.WorkerConsumer → durable.partitionConsumer,
// which implements the full recovery logic.
//
// Run with:
//
// go test ./test/integration/consumer/ -run TestDynamic_AutoRecovery -v -timeout 120s

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
	"github.com/arloliu/parti/v2/types"
)

const (
	// dynRecoveryInactiveThreshold is materially above the nats-go minimum PullExpiry (1s).
	dynRecoveryInactiveThreshold = 3 * time.Second
	// dynRecoveryExpiryWait is >2× InactiveThreshold so expiry is deterministic.
	dynRecoveryExpiryWait = 2 * dynRecoveryInactiveThreshold
)

// findConsumerByFilterSubject scans all consumers on a stream and returns the
// first consumer name whose FilterSubject matches the given subject.
// Returns empty string if not found.
func findConsumerByFilterSubject(t *testing.T, js jetstream.JetStream, streamName, filterSubject string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		t.Logf("findConsumerByFilterSubject: stream error: %v", err)
		return ""
	}
	for name := range stream.ConsumerNames(ctx).Name() {
		c, err := stream.Consumer(ctx, name)
		if err != nil {
			continue
		}
		info, err := c.Info(ctx)
		if err != nil {
			continue
		}
		if info.Config.FilterSubject == filterSubject {
			return name
		}
	}

	return ""
}

// TestDynamic_AutoRecovery_RecoverFromNew_ExplicitDelete verifies that when the
// per-partition durable consumer is explicitly deleted, the Dynamic consumer
// recovers using RecoverFromNew and does NOT replay already-processed messages.
func TestDynamic_AutoRecovery_RecoverFromNew_ExplicitDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "DAR_NEW"
	subject := "dar.new.a"

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dar.new.*"},
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

	dc, err := consumer.NewDynamic(js, streamName, "dar-new", "dar.new.{{.PartitionID}}", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	require.NoError(t, dc.Update(ctx, "worker-1", []types.Partition{{Keys: []string{"a"}}}))

	// Publish and consume initial batch.
	for i := range 3 {
		_, err = js.Publish(ctx, subject, []byte(fmt.Sprintf("pre-%d", i)))
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

	// Find the per-partition durable consumer by filter subject and delete it.
	require.Eventually(t, func() bool {
		return findConsumerByFilterSubject(t, js, streamName, subject) != ""
	}, 5*time.Second, 100*time.Millisecond, "consumer should exist before delete")

	consName := findConsumerByFilterSubject(t, js, streamName, subject)
	require.NotEmpty(t, consName, "consumer name must be found")

	err = js.DeleteConsumer(ctx, streamName, consName)
	require.NoError(t, err)

	// Wait for recovery to recreate the per-partition consumer with DeliverNewPolicy
	// before publishing so messages land in the consumer's delivery window.
	require.Eventually(t, func() bool {
		return findConsumerByFilterSubject(t, js, streamName, subject) != ""
	}, 10*time.Second, 10*time.Millisecond, "recovery should recreate the per-partition consumer")

	// With RecoverFromNew, publish post-delete messages. These MUST be received,
	// but pre-delete messages MUST NOT be replayed.
	for i := range 3 {
		_, err = js.Publish(ctx, subject, []byte(fmt.Sprintf("post-%d", i)))
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return received.Load() >= beforeDelete+3
	}, 30*time.Second, 100*time.Millisecond, "post-recovery messages should be received")

	require.Equal(t, int32(6), received.Load(),
		"RecoverFromNew must not replay pre-delete messages")
}

// TestDynamic_AutoRecovery_RecoverFromLastProcessed_ExplicitDelete verifies
// that with RecoverFromLastProcessed the consumer resumes from the ack floor
// after explicit deletion without replaying already-acknowledged messages.
func TestDynamic_AutoRecovery_RecoverFromLastProcessed_ExplicitDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "DAR_LKP"
	subject := "dar.lkp.a"

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dar.lkp.*"},
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

	dc, err := consumer.NewDynamic(js, streamName, "dar-lkp", "dar.lkp.{{.PartitionID}}", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	require.NoError(t, dc.Update(ctx, "worker-1", []types.Partition{{Keys: []string{"a"}}}))

	// Publish and consume initial batch. All messages will be auto-acked.
	for i := range 3 {
		_, err = js.Publish(ctx, subject, []byte(fmt.Sprintf("pre-%d", i)))
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
	// Give auto-acks time to propagate so the checkpoint advances.
	time.Sleep(100 * time.Millisecond)

	// Find and delete the per-partition consumer.
	require.Eventually(t, func() bool {
		return findConsumerByFilterSubject(t, js, streamName, subject) != ""
	}, 5*time.Second, 100*time.Millisecond, "consumer should exist before delete")

	consName := findConsumerByFilterSubject(t, js, streamName, subject)
	require.NotEmpty(t, consName)

	err = js.DeleteConsumer(ctx, streamName, consName)
	require.NoError(t, err)

	// Publish post-delete messages.
	time.Sleep(100 * time.Millisecond)
	for i := range 3 {
		_, err = js.Publish(ctx, subject, []byte(fmt.Sprintf("post-%d", i)))
		require.NoError(t, err)
	}

	// With RecoverFromLastProcessed the consumer should resume from the ack floor.
	require.Eventually(t, func() bool {
		return received.Load() >= beforeDelete+3
	}, 30*time.Second, 100*time.Millisecond, "post-recovery messages must arrive")

	// At most 6 messages total — no replay of acked messages.
	require.LessOrEqual(t, received.Load(), int32(6),
		"RecoverFromLastProcessed must not replay already-acked messages")
}

// TestDynamic_AutoRecovery_ManualAck_RecoverFromLastProcessed_ExplicitDelete verifies
// that RecoverFromLastProcessed works with ManualAck=true on a Dynamic consumer.
// The framework-intercepted msg.Ack() advances the checkpoint so recovery resumes
// from the correct position.
func TestDynamic_AutoRecovery_ManualAck_RecoverFromLastProcessed_ExplicitDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "DAR_MLKP"
	subject := "dar.mlkp.a"

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dar.mlkp.*"},
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

	dc, err := consumer.NewDynamic(js, streamName, "dar-mlkp", "dar.mlkp.{{.PartitionID}}", handler,
		consumer.WithManualAck(true),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
	)
	require.NoError(t, err, "ManualAck=true + RecoverFromLastProcessed must be accepted")
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	require.NoError(t, dc.Update(ctx, "worker-1", []types.Partition{{Keys: []string{"a"}}}))

	for i := range 3 {
		_, err = js.Publish(ctx, subject, []byte(fmt.Sprintf("pre-%d", i)))
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
	time.Sleep(100 * time.Millisecond) // let intercepted Ack propagate

	require.Eventually(t, func() bool {
		return findConsumerByFilterSubject(t, js, streamName, subject) != ""
	}, 5*time.Second, 100*time.Millisecond, "consumer should exist before delete")

	consName := findConsumerByFilterSubject(t, js, streamName, subject)
	require.NotEmpty(t, consName)

	err = js.DeleteConsumer(ctx, streamName, consName)
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)
	for i := range 3 {
		_, err = js.Publish(ctx, subject, []byte(fmt.Sprintf("post-%d", i)))
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return received.Load() >= beforeDelete+3
	}, 30*time.Second, 100*time.Millisecond, "post-recovery messages must arrive")

	require.LessOrEqual(t, received.Load(), int32(6),
		"RecoverFromLastProcessed with ManualAck=true must not replay already-acked messages")
}

// TestDynamic_AutoRecovery_ActivePullDelete verifies Path A: when the per-partition
// consumer is deleted while Next() is blocking (no messages in stream), the consumer
// receives ErrConsumerDeleted and triggers immediate recovery without waiting for
// the burst escalation threshold.
func TestDynamic_AutoRecovery_ActivePullDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "DAR_APD"
	subject := "dar.apd.a"

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dar.apd.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var received atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		received.Add(1)
		return nil
	})

	dc, err := consumer.NewDynamic(js, streamName, "dar-apd", "dar.apd.{{.PartitionID}}", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	require.NoError(t, dc.Update(ctx, "worker-1", []types.Partition{{Keys: []string{"a"}}}))

	// Wait for the consumer to be registered on the server with no pending messages.
	// iter.Next() is now blocking on the pull request — this is Path A.
	require.Eventually(t, func() bool {
		return findConsumerByFilterSubject(t, js, streamName, subject) != ""
	}, 5*time.Second, 100*time.Millisecond, "consumer should be registered")

	// Give a moment for Next() to block on the pull request.
	time.Sleep(200 * time.Millisecond)

	// Delete the consumer while Next() is blocking — triggers ErrConsumerDeleted (~50ms).
	consName := findConsumerByFilterSubject(t, js, streamName, subject)
	require.NotEmpty(t, consName)
	err = js.DeleteConsumer(ctx, streamName, consName)
	require.NoError(t, err)

	// Wait for recovery to recreate the consumer with DeliverNewPolicy before
	// publishing so the message lands in the consumer's delivery window.
	require.Eventually(t, func() bool {
		return findConsumerByFilterSubject(t, js, streamName, subject) != ""
	}, 10*time.Second, 10*time.Millisecond, "recovery should recreate the per-partition consumer")

	// After recovery (RecoverFromNew), publish a new message. It should be received.
	_, err = js.Publish(ctx, subject, []byte("post-active-pull-delete"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return received.Load() >= 1
	}, 30*time.Second, 100*time.Millisecond, "post-recovery message should be received after active-pull delete")
}

// TestDynamic_AutoRecovery_PassiveExpiry verifies that a slow handler causing
// InactiveThreshold expiry (Path B: ErrNoHeartbeat) triggers recovery.
func TestDynamic_AutoRecovery_PassiveExpiry(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "DAR_EXP"
	subject := "dar.exp.a"

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dar.exp.*"},
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

	dc, err := consumer.NewDynamic(js, streamName, "dar-exp", "dar.exp.{{.PartitionID}}", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
		consumer.WithInactiveThreshold(dynRecoveryInactiveThreshold),
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	require.NoError(t, dc.Update(ctx, "worker-1", []types.Partition{{Keys: []string{"a"}}}))

	// Trigger first message so the handler stalls.
	_, err = js.Publish(ctx, subject, []byte("trigger"))
	require.NoError(t, err)

	select {
	case <-handlerReady:
	case <-time.After(15 * time.Second):
		t.Fatal("handler never received trigger message")
	}

	// Handler is stalling. No Next() is called so no pull is active on the server.
	// Wait for InactiveThreshold to expire the consumer server-side.
	t.Logf("Sleeping %v to let InactiveThreshold(%v) expire...", dynRecoveryExpiryWait, dynRecoveryInactiveThreshold)
	time.Sleep(dynRecoveryExpiryWait)

	close(handlerRelease)

	// After handler returns, ErrNoHeartbeat fires. Burst detection calls
	// consumer.Info() → ErrConsumerNotFound → recovery triggers.
	// With RecoverFromNew, a new message should be received post-recovery.
	time.Sleep(200 * time.Millisecond)
	beforeRecovery := received.Load()

	_, err = js.Publish(ctx, subject, []byte("after-expiry"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return received.Load() > beforeRecovery
	}, 30*time.Second, 100*time.Millisecond,
		"post-expiry message should be received after recovery; count=%d", received.Load())
}

// NOTE: The plan specifies a test for "ErrNoHeartbeat from transient network issue
// (where consumer.Info() returns success) stays on existing escalation/backoff and
// does NOT trigger recovery." This scenario is difficult to reproduce in integration
// with an embedded NATS server because ErrNoHeartbeat only fires when the consumer
// is genuinely gone or unreachable. The logic is verified at the unit level by
// TestClassify_NoHeartbeat_BurstButConsumerStillExists in internal/recovery/controller_test.go.
