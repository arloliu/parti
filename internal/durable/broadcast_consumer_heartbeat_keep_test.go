package durable

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/recovery"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// broadcastKeepRun drives BroadcastConsumer.runLoop with scripted iterators.
type broadcastKeepRun struct {
	bc           *BroadcastConsumer
	handled      chan string
	factoryCalls atomic.Int32
	cancel       context.CancelFunc
}

// startBroadcastKeepRun starts a loop whose n-th iterator (1-based) comes from iters.
// Calls beyond len(iters) get an idle iterator.
func startBroadcastKeepRun(
	t *testing.T, rc *recovery.Controller, cons jetstream.Consumer, iters ...*testutil.ScriptedIter,
) *broadcastKeepRun {
	t.Helper()
	r := &broadcastKeepRun{handled: make(chan string, 16)}
	r.bc = &BroadcastConsumer{
		config: BroadcastConsumerConfig{
			StreamName:     "BC_KEEP",
			ConsumerPrefix: "bc",
			WildcardFilter: "bc.>",
			BatchSize:      10,
			FetchTimeout:   time.Second,
			Retry: RetryConfig{
				Base: 10 * time.Millisecond, Backoff: 10 * time.Millisecond, Multiplier: 1.0, Max: 10 * time.Millisecond,
			},
		},
		logger: logging.NewNop(),
		handler: messageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
			r.handled <- string(msg.Data())

			return nil
		}),
		consumer: cons,
		loopDone: make(chan struct{}),
		iterFactory: func(_ jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
			call := int(r.factoryCalls.Add(1))
			if call <= len(iters) {
				return iters[call-1], nil
			}

			return testutil.NewScriptedIter(), nil
		},
		recovery: rc,
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go r.bc.runLoop(ctx)
	t.Cleanup(func() {
		cancel()
		<-r.bc.loopDone
	})

	return r
}

func (r *broadcastKeepRun) requireHandled(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-r.handled:
		require.Equal(t, want, got)
	case <-time.After(2 * time.Second):
		t.Fatalf("message %q was not handled", want)
	}
}

// A missed pull heartbeat must not cost the broadcast consumer its iterator:
// stopping it discards messages already delivered to the client.
func TestBroadcastConsumer_HeartbeatMiss_KeepsIterator(t *testing.T) {
	for _, tt := range []struct {
		name string
		rc   *recovery.Controller
	}{
		{name: "recovery_disabled"},
		{name: "recovery_enabled", rc: recovery.NewController(recovery.ControllerConfig{
			Strategy: recovery.FromNew, FetchTimeout: time.Second, Logger: logging.NewNop(),
		})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			iter := testutil.NewScriptedIter(testutil.IterData("a"), testutil.IterMiss(), testutil.IterData("b"))
			r := startBroadcastKeepRun(t, tt.rc, &hbConsumer{}, iter)

			r.requireHandled(t, "a")
			r.requireHandled(t, "b")
			<-iter.Idle()
			require.Zero(t, iter.Stops(), "the iterator must not be stopped on a missed heartbeat")
			require.Equal(t, int32(1), r.factoryCalls.Load(), "the iterator must not be recreated")

			r.cancel()
			<-r.bc.loopDone
			require.Positive(t, iter.Stops(), "the kept iterator must be stopped on shutdown")
		})
	}
}

// A kept iterator must still be stopped when the loop exits before using it again.
//
// The context is cancelled from the consumer-info probe that follows the miss.
// processIterator has returned by then and no longer watches the context,
// so only runLoop can stop the iterator.
func TestBroadcastConsumer_HeartbeatMiss_KeptIteratorStoppedOnCancel(t *testing.T) {
	rc := recovery.NewController(recovery.ControllerConfig{
		Strategy: recovery.FromNew, FetchTimeout: time.Second, BurstThreshold: 1, Logger: logging.NewNop(),
	})
	var r *broadcastKeepRun
	// The startup checkpoint seed also calls Info; only the probe after the miss cancels.
	var missed atomic.Bool
	miss := testutil.IterMiss()
	miss.Before = func() { missed.Store(true) }
	iter := testutil.NewScriptedIter(miss)
	ready := make(chan struct{})
	cons := &hbConsumer{onInfo: func() {
		if missed.Load() {
			<-ready
			r.cancel()
		}
	}}
	r = startBroadcastKeepRun(t, rc, cons, iter)
	close(ready)

	<-r.bc.loopDone
	require.Equal(t, 1, iter.Stops(), "an iterator kept across a miss must be stopped once when the loop exits")
}

// A missed heartbeat that turns out to be a missing stream must replace the iterator.
func TestBroadcastConsumer_HeartbeatMiss_StreamMissing_ReplacesIterator(t *testing.T) {
	rc := recovery.NewController(recovery.ControllerConfig{
		Strategy: recovery.FromNew, FetchTimeout: time.Second, BurstThreshold: 1, Logger: logging.NewNop(),
	})
	iter := testutil.NewScriptedIter(testutil.IterMiss())
	r := startBroadcastKeepRun(t, rc, &hbConsumer{infoErr: jetstream.ErrStreamNotFound}, iter)

	require.Eventually(t, func() bool { return r.factoryCalls.Load() >= 2 }, 2*time.Second, 5*time.Millisecond,
		"a new iterator must be created after the stream-missing backoff")
	require.Positive(t, iter.Stops(), "the old iterator must be stopped when it is replaced")
}
