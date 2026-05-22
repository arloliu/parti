package provision_test

// W3 consumer cursor-loss discipline — the roadmap's named integration test.
//
// A provision recreate-consumer action delete/recreates a per-partition
// durable. Its delete step discards the consumer's delivery / ack cursor; the
// recreate builds a fresh durable. This test documents what an operator must
// understand before setting allowDeleteRecreate on a consumer: a runtime
// Dynamic consumer using the RecoverFromNew strategy rebinds the recreated
// durable WITHOUT replaying the pre-recreate history.
//
// The recreate-consumer's destructive step is its DeleteConsumer call (W3
// applyRecreateConsumerAction step 3). To exercise the runtime's rebinding
// deterministically this test triggers that same delete directly on a durable a
// runtime Dynamic consumer owns — "simulate the runtime rebinding via
// RecoverFromNew", per the W3 test plan — and asserts the no-replay outcome.
// The applyRecreateConsumerAction delete→create sequence itself is covered by
// the provision-package unit and integration tests.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/dynamicbuild"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
)

// TestRecreateConsumer_CursorLoss_RecoverFromNew verifies that when a
// recreate-consumer deletes a per-partition durable out from under a running
// runtime Dynamic consumer, recovery with RecoverFromNew receives post-recreate
// publishes and does NOT replay the pre-recreate messages — the cursor-loss
// discipline an operator must understand before opting a consumer into
// allowDeleteRecreate.
func TestRecreateConsumer_CursorLoss_RecoverFromNew(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const (
		streamName = "RCCL_NEW"
		prefix     = "rccl-new"
		template   = "rccl.new.{{.PartitionID}}"
		subject    = "rccl.new.a"
	)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"rccl.new.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// A runtime Dynamic consumer with RecoverFromNew: when its per-partition
	// durable is deleted (the recreate-consumer destructive step), recovery
	// rebinds with DeliverNewPolicy and skips already-published history.
	var received atomic.Int32
	msgCh := make(chan string, 20)
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		received.Add(1)
		msgCh <- string(msg.Data())

		return nil
	})

	dc, err := consumer.NewDynamic(js, streamName, prefix, template, handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	require.NoError(t, dc.Update(ctx, "worker-1", []types.Partition{{Keys: []string{"a"}}}))

	// Publish and consume the pre-recreate batch.
	for i := range 3 {
		_, perr := js.Publish(ctx, subject, fmt.Appendf(nil, "pre-%d", i))
		require.NoError(t, perr)
	}
	for range 3 {
		select {
		case <-msgCh:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for a pre-recreate message")
		}
	}
	beforeRecreate := received.Load()
	require.Equal(t, int32(3), beforeRecreate)

	// The per-partition durable a recreate-consumer targets — the same
	// deterministic name the runtime WorkerConsumer derives.
	pre, suf, ok := dynamicbuild.ParseSubjectTemplateParts(template)
	require.True(t, ok, "the subject template must parse")
	durable := dynamicbuild.PerSubjectDurableName(prefix, subject, pre, suf)

	require.Eventually(t, func() bool {
		_, cerr := js.Consumer(ctx, streamName, durable)

		return cerr == nil
	}, 5*time.Second, 50*time.Millisecond, "the per-partition durable must exist before recreate")

	// The recreate-consumer destructive step: DeleteConsumer discards the
	// delivery / ack cursor (W3 applyRecreateConsumerAction step 3).
	require.NoError(t, js.DeleteConsumer(ctx, streamName, durable))

	// The runtime detects the missing durable and rebinds it via RecoverFromNew.
	require.Eventually(t, func() bool {
		_, cerr := js.Consumer(ctx, streamName, durable)

		return cerr == nil
	}, 15*time.Second, 100*time.Millisecond,
		"recovery must rebind the per-partition durable after the recreate delete")

	// Publish the post-recreate batch. With RecoverFromNew these MUST arrive and
	// the pre-recreate messages MUST NOT be replayed.
	for i := range 3 {
		_, perr := js.Publish(ctx, subject, fmt.Appendf(nil, "post-%d", i))
		require.NoError(t, perr)
	}

	require.Eventually(t, func() bool {
		return received.Load() >= beforeRecreate+3
	}, 30*time.Second, 100*time.Millisecond, "post-recreate messages must be received")

	require.Equal(t, int32(6), received.Load(),
		"RecoverFromNew must not replay the pre-recreate messages after a recreate-consumer")
}
