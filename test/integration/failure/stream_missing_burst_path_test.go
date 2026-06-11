package failure_test

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/types"
)

// TestStreamMissingNoHook_HeartbeatBurstPath_ExhaustsWithMultiAttemptBudget
// closes the gap the T4 test (stream_missing_no_hook_test.go) works around
// with MaxAttempts=1. With a multi-attempt budget (the DEFAULT is 8), the
// post-deletion failure sequence routes through the heartbeat-burst path:
// the iterator stays bound, iter.Next() surfaces ErrNoHeartbeat, and the
// burst confirmation probes consumer.Info() — which, for a deleted STREAM,
// answers with the stream-scoped ErrStreamNotFound rather than
// ErrConsumerNotFound.
//
// Pre-fix, that probe answer was treated as "consumer still exists" and the
// loop fell into unbounded backoff: the stream-missing failure counter never
// reached MaxAttempts, OnPermanentFailure never fired, the manager observer
// never saw the loss, and the worker reported Stable with a permanently
// stalled consumer — the silent-stall class one layer above the terminal
// Degraded hold. Post-fix, the probe routes to the bounded stream-missing
// detour and exhaustion fires within MaxAttempts cycles regardless of which
// error (consumer-deleted vs heartbeat-burst) each cycle surfaces.
func TestStreamMissingNoHook_HeartbeatBurstPath_ExhaustsWithMultiAttemptBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const streamName = "BURSTPATH_STREAM"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"burstpath.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	})

	dyn, err := consumer.NewDynamic(
		js, streamName, "burstpath", "burstpath.{{.PartitionID}}", handler,
		consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
		// NO StreamMissingHook: exhaustion must fire OnPermanentFailure and
		// route to the manager observer. MaxAttempts=3 (NOT the T4
		// workaround value of 1) so the budget requires the per-cycle
		// classification to keep converging on the stream-missing detour —
		// the exact multi-attempt sequencing the heartbeat-burst gap broke.
		consumer.WithRecoveryRetry(consumer.RecoveryRetryConfig{
			MaxAttempts: 3,
			BaseBackoff: 100 * time.Millisecond,
			MaxBackoff:  300 * time.Millisecond,
		}),
		consumer.WithBatchSize(1),
		// PullExpiry has a hard 1s NATS minimum; PullHeartbeat = expiry/2,
		// so ErrNoHeartbeat detection cycles at ~1-2s.
		consumer.WithFetchTimeout(1*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dyn.Stop(context.Background()) })

	cluster := testutil.NewFastWorkerCluster(t, nc, 4)
	defer cluster.StopWorkers()

	mgr := cluster.AddWorkerWithOptions(ctx,
		parti.WithWorkerConsumerUpdater(dyn),
	)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	cluster.WaitForStableState(15 * time.Second)

	// Seed a publish + dwell so partition consumers have ACTIVE, bound
	// iterators before deletion — the heartbeat-burst path under test only
	// exists for an already-bound iterator (a failed creation routes through
	// Site A instead).
	_, err = js.Publish(ctx, "burstpath.partition-0", []byte("seed"))
	require.NoError(t, err)
	time.Sleep(750 * time.Millisecond)

	require.NoError(t, js.DeleteStream(ctx, streamName))

	// Exhaustion must reach the manager within the multi-attempt budget:
	// ~3 cycles x (heartbeat detection ~1-2s + burst window accumulation +
	// detour backoff) plus scheduling slack. 45s is generous for the fixed
	// path and far below the pre-fix behavior (never: the loop ping-pongs
	// between ErrNoHeartbeat and backoff without ever counting a
	// stream-missing failure).
	require.Eventually(t, func() bool {
		return mgr.State() == types.StateDegraded
	}, 45*time.Second, 250*time.Millisecond,
		"stream deletion with an active iterator and a multi-attempt recovery budget must exhaust "+
			"through the stream-missing detour and reach Degraded; a timeout here means the "+
			"heartbeat-burst Info probe is misclassifying the deleted stream as a transient issue "+
			"and the consumer is stalled while the worker reports Stable")
}
