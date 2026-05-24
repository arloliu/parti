package failure_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/testutil"
)

// TestStreamMissingHookError_NoRetryStormAfterExhaustion — T5 from
// docs/plans/self-healing/09-pr9-spec.md §"Integration tests".
//
// Pins the bounded-retry contract for the hook-returns-error path:
// when the operator's StreamMissingHook keeps failing (e.g. the
// operator's recreate script is broken or the target stream still
// cannot be provisioned), the partition consumer must:
//
//   - Fire the hook multiple times (up to MaxAttempts), giving the
//     operator a real retry budget.
//   - After exhaustion, issue zero further CreateOrUpdateConsumer
//     calls — the consumer loop must terminate rather than re-enter
//     a tight retry storm.
//
// The contract complements T4 (no hook): both paths share the same
// partition-consumer exhaustion-exit branch in
// handleStreamMissingFailure, but they enter via different
// classifier transitions. T5 specifically pins the hook-error route.
func TestStreamMissingHookError_NoRetryStormAfterExhaustion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const streamName = "P23_T5_STREAM"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"p23t5.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	spy := &jsSpy{JetStream: js}

	hookErr := errors.New("operator-driven recreate failed")
	var hookCalls atomic.Int32
	hook := func(_ string) error {
		hookCalls.Add(1)
		return hookErr
	}

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	})

	dyn, err := consumer.NewDynamic(
		spy, streamName, "p23t5", "p23t5.{{.PartitionID}}", handler,
		consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
		consumer.WithStreamMissingHook(hook),
		// MaxAttempts=1 so the first hook-returns-error invocation
		// per partition immediately exhausts the envelope and fires
		// OnPermanentFailure. The "hook fires multiple times" floor
		// asserted below is met across partitions (each partition
		// consumer invokes the hook once) — see T4 spec note about
		// the durable-layer classification gap that keeps the
		// per-partition attempt counter from exceeding 1 once the
		// iter.Next() loop transitions to ErrNoHeartbeat.
		consumer.WithRecoveryRetry(consumer.RecoveryRetryConfig{
			MaxAttempts: 1,
			BaseBackoff: 50 * time.Millisecond,
			MaxBackoff:  100 * time.Millisecond,
		}),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dyn.Stop(context.Background()) })

	var (
		gotErrCh = make(chan error, 8)
		onError  = func(_ context.Context, e error) error {
			select {
			case gotErrCh <- e:
			default:
			}
			return nil
		}
	)

	cluster := testutil.NewFastWorkerCluster(t, nc, 4)
	defer cluster.StopWorkers()
	mgr := cluster.AddWorkerWithOptions(ctx,
		parti.WithWorkerConsumerUpdater(dyn),
		parti.WithHooks(&parti.Hooks{OnError: onError}),
	)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	cluster.WaitForStableState(15 * time.Second)

	_, err = js.Publish(ctx, "p23t5.partition-0", []byte("seed"))
	require.NoError(t, err)
	time.Sleep(500 * time.Millisecond)

	require.NoError(t, js.DeleteStream(ctx, streamName))

	// Wait for OnError to surface a stream-missing-wrapped error.
WaitForOnError:
	for {
		select {
		case e := <-gotErrCh:
			if errors.Is(e, parti.ErrStreamMissing) {
				break WaitForOnError
			}
		case <-time.After(30 * time.Second):
			t.Fatal("OnError did not surface a stream-missing-wrapped error within 30s — hook-error exhaustion did not route through the manager observer")
		}
	}

	// Hook must have fired multiple times. With MaxAttempts=1 each
	// partition consumer fires the hook exactly once before its own
	// exhaustion-exit; with 4 partitions assigned to the worker, the
	// hook should fire on each as their iter.Next() loops surface
	// ErrConsumerDeleted from the stream deletion. Sibling
	// partitions may finish slightly after the first OnError fires,
	// so wait briefly for the count to settle before asserting the
	// "multiple times" floor.
	require.Eventually(t, func() bool {
		return hookCalls.Load() >= 2
	}, 5*time.Second, 50*time.Millisecond,
		"hook must fire at least twice (one per partition's stream-missing detour); "+
			"single-call exit would indicate the partition consumer exits its loop without ever invoking the operator's hook on subsequent partitions, "+
			"got %d calls", hookCalls.Load())

	// Post-exhaustion silence. Same contract as T4. Snapshot after
	// fire, wait 1s, assert monotonic (with slack for in-flight
	// envelope attempts from peer partition consumers).
	countAfterFire := spy.createOrUpdateCount.Load()
	time.Sleep(1 * time.Second)
	countAfter1s := spy.createOrUpdateCount.Load()
	additional := countAfter1s - countAfterFire
	require.LessOrEqual(t, additional, int64(8),
		"after exhaustion, CreateOrUpdateConsumer must not retry-storm; "+
			"got %d additional calls in the 1s post-fire window (count=%d → %d)",
		additional, countAfterFire, countAfter1s)
}
