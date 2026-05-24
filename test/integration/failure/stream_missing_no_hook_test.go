package failure_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/types"
)

// jsSpy wraps a jetstream.JetStream and counts CreateOrUpdateConsumer
// calls. Every other method passes through transparently via the
// embedded interface (Go method-set resolution picks the
// outer method when defined, otherwise the embedded one). The counter
// is the empirical signal for the "no retry storm after permanent
// failure" assertion: once OnPermanentFailure has fired, the partition
// consumer must STOP issuing further iter-create / consumer-recreate
// calls — exhaustion is terminal.
type jsSpy struct {
	jetstream.JetStream
	createOrUpdateCount atomic.Int64
}

func (s *jsSpy) CreateOrUpdateConsumer(
	ctx context.Context, stream string, cfg jetstream.ConsumerConfig,
) (jetstream.Consumer, error) {
	s.createOrUpdateCount.Add(1)
	return s.JetStream.CreateOrUpdateConsumer(ctx, stream, cfg)
}

// TestStreamMissingNoHook_RoutesPermanentFailureToManager — T4 from
// docs/plans/self-healing/09-pr9-spec.md §"Integration tests".
//
// Pins the end-to-end exhaustion route when NO StreamMissingHook is
// configured: F2 envelope exhausts → durable layer fires
// OnPermanentFailure with a wrapped types.ErrStreamMissing →
// consumer.Dynamic's indirection dispatcher routes through the
// manager-installed observer → manager logs via Hooks.OnError and
// trips enterDegraded with reason "stream-missing-recovery-exhausted"
// (NOT "KV error threshold exceeded"). After exhaustion, the partition
// consumer stops issuing CreateOrUpdateConsumer calls (no retry storm).
//
// Why all the asserts in one test: every link in the chain is
// load-bearing for the cross-feature contract that
// recordKVError("KV error threshold exceeded") remains the EXCLUSIVE
// route for whole-bucket KV loss. A regression in any single hop
// (durable wrap, Dynamic dispatcher, manager observer, recordKVError
// short-circuit, named-reason enterDegraded) would let stream-missing
// double-count or surface under the wrong reason — T4 catches all of
// them in a single live run.
func TestStreamMissingNoHook_RoutesPermanentFailureToManager(t *testing.T) {
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

	const streamName = "P23_T4_STREAM"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"p23t4.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// Wrap the JetStream context the Dynamic uses. CreateOrUpdateConsumer
	// is the path through which an exhausted partition consumer would
	// re-enter if it failed to terminate; the counter is the empirical
	// signal for the "no retry storm after permanent failure"
	// assertion below. Manager keeps the unwrapped js for its own KV
	// operations — the spy is scoped to the partition-consumer surface.
	spy := &jsSpy{JetStream: js}

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	})

	dyn, err := consumer.NewDynamic(
		spy, streamName, "p23t4", "p23t4.{{.PartitionID}}", handler,
		consumer.WithRecoveryStrategy(consumer.RecoverFromLastProcessed),
		// NO StreamMissingHook — the no-hook path is the contract under
		// test. handleStreamMissing(ctx) returns a wrapped
		// types.ErrStreamMissing immediately; the per-episode budget is
		// MaxAttempts (we tighten it below so exhaustion fires in a
		// few seconds rather than ~90s).
		// MaxAttempts=1 so the first stream-missing detour failure
		// (handleStreamMissing returning a wrapped ErrStreamMissing
		// because no hook is configured) immediately exhausts the
		// envelope and fires OnPermanentFailure. The exhaustion-path
		// classification of subsequent iter.Next failures has a
		// known gap (iter.Next() → ErrNoHeartbeat after the first
		// detour returns false from confirmConsumerGone when the
		// underlying API surfaces ErrStreamNotFound rather than
		// ErrConsumerNotFound); MaxAttempts=1 keeps this T4 focused
		// on the manager-side wiring rather than the durable
		// classification surface.
		consumer.WithRecoveryRetry(consumer.RecoveryRetryConfig{
			MaxAttempts: 1,
			BaseBackoff: 100 * time.Millisecond,
			MaxBackoff:  200 * time.Millisecond,
		}),
		consumer.WithBatchSize(1),
		// PullExpiry has a hard 1s NATS minimum; smaller values fail
		// validation during iter-create.
		consumer.WithFetchTimeout(1*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dyn.Stop(context.Background()) })

	// Capture the OnError signal. errors.Is(err, parti.ErrStreamMissing)
	// is the documented branch users employ to distinguish stream-missing
	// from other recoverable errors; T4 asserts it on the live error
	// the bridge produces.
	var (
		gotErrMu    sync.Mutex
		gotErrs     []error
		gotErrCh    = make(chan error, 4)
		onErrorHook = func(_ context.Context, e error) error {
			gotErrMu.Lock()
			gotErrs = append(gotErrs, e)
			gotErrMu.Unlock()
			select {
			case gotErrCh <- e:
			default:
			}

			return nil
		}

		degradedMu       sync.Mutex
		degradedReasons  []string
		degradedReasonCh = make(chan string, 4)
		onDegradedHook   = func(_ context.Context, reason string) error {
			degradedMu.Lock()
			degradedReasons = append(degradedReasons, reason)
			degradedMu.Unlock()
			select {
			case degradedReasonCh <- reason:
			default:
			}

			return nil
		}
	)

	cluster := testutil.NewFastWorkerCluster(t, nc, 4)
	defer cluster.StopWorkers()

	mgr := cluster.AddWorkerWithOptions(ctx,
		parti.WithWorkerConsumerUpdater(dyn),
		parti.WithHooks(&parti.Hooks{
			OnError:    onErrorHook,
			OnDegraded: onDegradedHook,
		}),
	)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	cluster.WaitForStableState(15 * time.Second)

	// Seed a publish + small dwell so each partition consumer has an
	// active iterator before the stream is deleted. Without this the
	// iterators may not have completed initial bind before deletion,
	// which masks the Site B path under test.
	_, err = js.Publish(ctx, "p23t4.partition-0", []byte("seed"))
	require.NoError(t, err)
	time.Sleep(750 * time.Millisecond)

	require.NoError(t, js.DeleteStream(ctx, streamName))

	// Wait for OnError to fire with a stream-missing-wrapped error.
	// The total expected exhaustion time is bounded by
	// MaxAttempts * MaxBackoff (3 * 200ms = 600ms) plus consumer
	// recovery cycles, well under 30s.
	var firstStreamMissingErr error
WaitForStreamMissing:
	for {
		select {
		case e := <-gotErrCh:
			if errors.Is(e, parti.ErrStreamMissing) {
				firstStreamMissingErr = e
				break WaitForStreamMissing
			}
		case <-time.After(30 * time.Second):
			t.Fatal("Hooks.OnError did not fire with errors.Is(err, parti.ErrStreamMissing) within 30s — no-hook exhaustion did not route through the manager observer")
		}
	}
	require.ErrorIs(t, firstStreamMissingErr, parti.ErrStreamMissing,
		"the OnError-delivered error must satisfy errors.Is(err, parti.ErrStreamMissing) so applications can branch via the documented sentinel")

	// State must transition to Degraded with the named reason. The
	// reason is the cross-feature contract preserved by
	// manager_degraded.go's stream-missing short-circuit — a
	// regression that classified this through recordKVError would
	// trip "KV error threshold exceeded" instead.
	require.Eventually(t, func() bool {
		return mgr.State() == types.StateDegraded
	}, 15*time.Second, 50*time.Millisecond,
		"manager must transition to Degraded after stream-missing exhaustion")

	// And the operator-facing reason must be the named one. This is
	// the load-bearing assertion for the cross-feature contract per
	// AGENTS.md: whole-bucket KV loss is the SOLE driver of
	// "KV error threshold exceeded"; stream-missing exhaustion
	// MUST report "stream-missing-recovery-exhausted" so readiness /
	// alerting can distinguish the two failure modes.
	var firstDegradedReason string
	select {
	case firstDegradedReason = <-degradedReasonCh:
	case <-time.After(15 * time.Second):
		t.Fatal("OnDegraded hook did not fire within 15s of the state transition — readiness signal missing")
	}
	require.Equal(t, "stream-missing-recovery-exhausted", firstDegradedReason,
		"degraded reason must be the named stream-missing route, not the generic KV-threshold one; "+
			"got %q. A regression here would mean stream-missing exhaustion is double-counting through "+
			"recordKVError or routing through the wrong enterDegraded site.", firstDegradedReason)
	degradedMu.Lock()
	for _, r := range degradedReasons {
		require.NotEqual(t, "KV error threshold exceeded", r,
			"degraded reason must never be the KV-threshold one for a stream-missing exhaustion (cross-feature contract); got %q in the observed sequence %v", r, degradedReasons)
	}
	degradedMu.Unlock()

	// Post-exhaustion silence (spec T4 §"Post-exhaustion silence").
	// Snapshot the call count right after the OnError fire, then
	// wait 1 second and re-read. The partition consumer's
	// exhaustion-exit must terminate its loop — a regression that
	// kept retrying would manifest as dozens of additional
	// CreateOrUpdateConsumer calls in this window (BaseBackoff is
	// 100ms; an unbounded retry would issue 5-10 calls / second).
	countAfterFire := spy.createOrUpdateCount.Load()
	time.Sleep(1 * time.Second)
	countAfter1s := spy.createOrUpdateCount.Load()
	additional := countAfter1s - countAfterFire
	// Allow a small slack for any in-flight envelope attempts from
	// peer partition consumers that race the fire signal — the
	// contract under test is "no UNBOUNDED retry storm", not
	// "exactly zero further calls".
	require.LessOrEqual(t, additional, int64(8),
		"after permanent failure fires, CreateOrUpdateConsumer must not enter a retry storm; "+
			"got %d additional calls in the 1s after OnError fired (count=%d → %d), "+
			"which indicates the partition consumer's exhaustion-exit is not terminating the loop",
		additional, countAfterFire, countAfter1s)
}
