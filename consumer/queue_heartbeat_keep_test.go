package consumer

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

// queueKeepRun drives Queue.runLoop with scripted iterators.
type queueKeepRun struct {
	q            *Queue
	handled      chan string
	factoryCalls atomic.Int32
	cancel       context.CancelFunc
}

// infoStubConsumer is a consumer stub whose Info runs onInfo, then fails with err if set.
type infoStubConsumer struct {
	jetstream.Consumer
	err    error
	onInfo func()
}

func (c *infoStubConsumer) Info(context.Context) (*jetstream.ConsumerInfo, error) {
	if c.onInfo != nil {
		c.onInfo()
	}
	if c.err != nil {
		return nil, c.err
	}

	return &jetstream.ConsumerInfo{}, nil
}

// startQueueKeepRun starts a Queue loop whose n-th iterator (1-based) comes from iters.
// Calls beyond len(iters) get an idle iterator.
func startQueueKeepRun(t *testing.T, rc *recovery.Controller, cons jetstream.Consumer, iters ...*testutil.ScriptedIter) *queueKeepRun {
	t.Helper()
	r := &queueKeepRun{handled: make(chan string, 16)}
	r.q = &Queue{
		config: QueueConfig{
			CommonConfig: CommonConfig{BatchSize: 10, FetchTimeout: time.Second},
			StreamName:   "Q_KEEP",
			ConsumerName: "q-keep",
			Retry: RetryConfig{
				Base: 10 * time.Millisecond, Backoff: 10 * time.Millisecond, Multiplier: 1.0, Max: 10 * time.Millisecond,
			},
		},
		logger:   logging.NewNop(),
		metrics:  &queueSignalMetrics{},
		consumer: cons,
		loopDone: make(chan struct{}),
		handler: MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
			r.handled <- string(msg.Data())

			return nil
		}),
		iterFactory: func(_ jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
			call := int(r.factoryCalls.Add(1))
			if call <= len(iters) {
				return iters[call-1], nil
			}

			return testutil.NewScriptedIter(), nil
		},
		consumerConfig: jetstream.ConsumerConfig{Durable: "q-keep"},
		recovery:       rc,
		retryRNG:       newRetryRNG(1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go r.q.runLoop(ctx)
	t.Cleanup(func() {
		cancel()
		<-r.q.loopDone
	})

	return r
}

func (r *queueKeepRun) requireHandled(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-r.handled:
		require.Equal(t, want, got)
	case <-time.After(2 * time.Second):
		t.Fatalf("message %q was not handled", want)
	}
}

// A missed pull heartbeat must not cost the Queue its iterator:
// stopping it discards messages already delivered to the client.
func TestQueue_HeartbeatMiss_KeepsIterator(t *testing.T) {
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
			r := startQueueKeepRun(t, tt.rc, &stubConsumer{}, iter)

			r.requireHandled(t, "a")
			r.requireHandled(t, "b")
			<-iter.Idle()
			require.Zero(t, iter.Stops(), "the iterator must not be stopped on a missed heartbeat")
			require.Equal(t, int32(1), r.factoryCalls.Load(), "the iterator must not be recreated")

			r.cancel()
			<-r.q.loopDone
			require.Positive(t, iter.Stops(), "the kept iterator must be stopped on shutdown")
		})
	}
}

// A kept iterator must still be stopped when the loop exits before using it again.
//
// The context is cancelled from the consumer-info probe that follows the miss.
// processIterator has returned by then and no longer watches the context,
// so only runLoop can stop the iterator.
func TestQueue_HeartbeatMiss_KeptIteratorStoppedOnCancel(t *testing.T) {
	rc := recovery.NewController(recovery.ControllerConfig{
		Strategy: recovery.FromNew, FetchTimeout: time.Second, BurstThreshold: 1, Logger: logging.NewNop(),
	})
	var r *queueKeepRun
	ready := make(chan struct{})
	cons := &infoStubConsumer{onInfo: func() {
		<-ready
		r.cancel()
	}}
	iter := testutil.NewScriptedIter(testutil.IterMiss())
	r = startQueueKeepRun(t, rc, cons, iter)
	close(ready)

	<-r.q.loopDone
	require.Equal(t, 1, iter.Stops(), "an iterator kept across a miss must be stopped once when the loop exits")
}

// A missed heartbeat that turns out to be a missing stream must replace the iterator.
func TestQueue_HeartbeatMiss_StreamMissing_ReplacesIterator(t *testing.T) {
	rc := recovery.NewController(recovery.ControllerConfig{
		Strategy: recovery.FromNew, FetchTimeout: time.Second, BurstThreshold: 1, Logger: logging.NewNop(),
	})
	iter := testutil.NewScriptedIter(testutil.IterMiss())
	r := startQueueKeepRun(t, rc, &infoStubConsumer{err: jetstream.ErrStreamNotFound}, iter)

	require.Eventually(t, func() bool { return r.factoryCalls.Load() >= 2 }, 2*time.Second, 5*time.Millisecond,
		"a new iterator must be created after the stream-missing backoff")
	require.Positive(t, iter.Stops(), "the old iterator must be stopped when it is replaced")
}
