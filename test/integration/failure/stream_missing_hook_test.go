package failure_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/testutil"
)

// TestStreamMissingHook_FiresOnDeleteStream — T1 from
// docs/plans/self-healing/09-pr9-spec.md §"Integration tests".
//
// Pins the operator-driven escalation path: when an integrated
// Parti manager owns a dynamic partition consumer and the underlying
// JetStream stream is deleted out from under it, the application-
// supplied StreamMissingHook fires within one recovery cycle with the
// correct stream name. This proves:
//
//   - The durable layer's stream-missing classifier routes through
//     the configured hook BEFORE escalating to OnPermanentFailure
//     (i.e. the hook is the first line of operator-driven recovery).
//   - The hook receives the actual stream identifier, not a placeholder
//     or the per-partition subject.
//
// T1 covers the happy path of the hook contract; T4 covers no-hook
// exhaustion and T5 covers hook-returns-error exhaustion. All three
// share the same manager + Dynamic wiring; T1 is the cheapest cycle
// (no envelope exhaustion required) so it is the canonical smoke
// test for the manager-side observer bridge.
func TestStreamMissingHook_FiresOnDeleteStream(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const streamName = "P23_T1_STREAM"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"p23t1.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// Record hook invocations. errChan returns nil on hook fire to keep
	// the post-hook Site B detour quiet (a non-nil return triggers
	// envelope exhaustion — T5's territory, not ours).
	type hookCall struct {
		stream string
		when   time.Time
	}
	hookCh := make(chan hookCall, 4)
	hook := func(stream string) error {
		hookCh <- hookCall{stream: stream, when: time.Now()}
		return nil
	}

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	})

	dyn, err := consumer.NewDynamic(
		js, streamName, "p23t1", "p23t1.{{.PartitionID}}", handler,
		consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
		consumer.WithStreamMissingHook(hook),
		// Tight envelope so the test fails fast if the hook does NOT
		// fire (the no-hook envelope-exhaustion path would otherwise
		// take the full default ~90s).
		consumer.WithRecoveryRetry(consumer.RecoveryRetryConfig{
			MaxAttempts: 4,
			BaseBackoff: 100 * time.Millisecond,
			MaxBackoff:  300 * time.Millisecond,
		}),
		consumer.WithBatchSize(1),
		// PullExpiry has a hard 1s NATS minimum; smaller values fail
		// validation during iter-create, which would mask the
		// stream-missing classification.
		// PullExpiry has a hard 1s NATS minimum; smaller values fail
		// validation during iter-create, which would mask the
		// stream-missing classification under test.
		consumer.WithFetchTimeout(1*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dyn.Stop(context.Background()) })

	cluster := testutil.NewFastWorkerCluster(t, nc, 4)
	defer cluster.StopWorkers()
	mgr := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerConsumerUpdater(dyn))
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	cluster.WaitForStableState(15 * time.Second)

	// Publish a message so the dynamic consumer has an iterator
	// actively pulling from the stream. The stream-missing detour
	// only fires when an iter-create attempt observes
	// ErrStreamNotFound — purely idle consumers do not hit Site A.
	_, err = js.Publish(ctx, "p23t1.partition-0", []byte("seed"))
	require.NoError(t, err, "seed publish must succeed before the stream is deleted")

	// Let the consumer bind + start a pull.
	time.Sleep(500 * time.Millisecond)

	// Delete the stream out from under the consumer. The next pull or
	// iter-create attempt classifies as stream-missing → hook fires.
	require.NoError(t, js.DeleteStream(ctx, streamName))

	select {
	case call := <-hookCh:
		require.Equal(t, streamName, call.stream,
			"StreamMissingHook must receive the exact stream name passed to NewDynamic, not a subject or empty value")
	case <-time.After(15 * time.Second):
		t.Fatal("StreamMissingHook did not fire within 15s of js.DeleteStream — the durable layer is not routing the stream-missing classification to the configured hook")
	}
}

// TestStreamMissingHook_FiresWithDynamicAsCompositeChild pins the
// CompositeConsumerUpdater observer-forwarding contract through a live
// integration: a real manager wraps a Dynamic in a composite, registers
// the composite via WithWorkerConsumerUpdater, and the hook still fires
// when the stream is deleted. Without composite_updater.go's
// SetOnStreamMissingError forwarding to current children, the
// manager-installed observer would never reach the Dynamic and a
// no-hook + composite stack would silently drop the exhaustion event
// (the unit tests pin the registration mechanics; this test pins the
// end-to-end wiring against real NATS).
func TestStreamMissingHook_FiresWithDynamicAsCompositeChild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const streamName = "P23_T1_COMPOSITE_STREAM"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"p23t1c.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var fired atomic.Bool
	hook := func(_ string) error {
		fired.Store(true)
		return nil
	}

	dyn, err := consumer.NewDynamic(
		js, streamName, "p23t1c", "p23t1c.{{.PartitionID}}", handler(),
		consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
		consumer.WithStreamMissingHook(hook),
		consumer.WithRecoveryRetry(consumer.RecoveryRetryConfig{
			MaxAttempts: 4,
			BaseBackoff: 100 * time.Millisecond,
			MaxBackoff:  300 * time.Millisecond,
		}),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dyn.Stop(context.Background()) })

	composite := parti.NewCompositeConsumerUpdater(dyn)

	cluster := testutil.NewFastWorkerCluster(t, nc, 4)
	defer cluster.StopWorkers()
	mgr := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerConsumerUpdater(composite))
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	cluster.WaitForStableState(15 * time.Second)

	_, err = js.Publish(ctx, "p23t1c.partition-0", []byte("seed"))
	require.NoError(t, err)
	time.Sleep(500 * time.Millisecond)

	require.NoError(t, js.DeleteStream(ctx, streamName))

	require.Eventually(t, fired.Load, 15*time.Second, 100*time.Millisecond,
		"StreamMissingHook must fire even when the Dynamic is wrapped in a CompositeConsumerUpdater — the manager-installed observer must forward through the composite to the Dynamic child")
}

// handler returns a no-op MessageHandler used by integration tests
// that only care about the recovery surface (not message processing).
func handler() consumer.MessageHandler {
	return consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	})
}
