package ipartition

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	imetrics "github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/recovery"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// scriptedConsumer is a consumer stub whose n-th Messages call (1-based) returns iters[n-1].
// Calls beyond len(iters) get an idle iterator.
type scriptedConsumer struct {
	jetstream.Consumer
	iters   []*testutil.ScriptedIter
	calls   atomic.Int32
	infoErr error
	onInfo  func()
}

func (c *scriptedConsumer) Messages(...jetstream.PullMessagesOpt) (jetstream.MessagesContext, error) {
	call := int(c.calls.Add(1))
	if call <= len(c.iters) {
		return c.iters[call-1], nil
	}

	return testutil.NewScriptedIter(), nil
}

func (c *scriptedConsumer) Info(context.Context) (*jetstream.ConsumerInfo, error) {
	if c.onInfo != nil {
		c.onInfo()
	}
	if c.infoErr != nil {
		return nil, c.infoErr
	}

	return &jetstream.ConsumerInfo{}, nil
}

// jsKeepRun drives JSConsumer.run with scripted iterators.
type jsKeepRun struct {
	c       *JSConsumer
	cons    *scriptedConsumer
	handled chan string
	cancel  context.CancelFunc
}

func startJSKeepRun(t *testing.T, rc *recovery.Controller, byKey bool, cons *scriptedConsumer) *jsKeepRun {
	t.Helper()
	r := &jsKeepRun{cons: cons, handled: make(chan string, 16)}
	handler := messageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		r.handled <- string(msg.Data())

		return nil
	})
	r.c = &JSConsumer{
		config: ConsumerConfig{
			Logger:       logging.NewNop(),
			Metrics:      imetrics.NewNop(),
			FetchTimeout: time.Second,
			BatchSize:    10,
		},
		handler:  handler,
		subject:  "test.0",
		done:     make(chan struct{}),
		rc:       rc,
		consumer: cons,
	}
	if byKey {
		r.c.keyDispatcher = newKeyDispatcher(logging.NewNop(), handler,
			// One key, so the messages keep their order.
			func(jetstream.Msg) string { return "k" }, 8, time.Minute, false, nil, nil)
		t.Cleanup(func() { _ = r.c.keyDispatcher.Close(context.Background()) })
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go r.c.run(ctx)
	t.Cleanup(func() {
		cancel()
		<-r.c.done
	})

	return r
}

func (r *jsKeepRun) requireHandled(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-r.handled:
		require.Equal(t, want, got)
	case <-time.After(2 * time.Second):
		t.Fatalf("message %q was not handled", want)
	}
}

// A missed pull heartbeat must not cost the partition consumer its iterator:
// stopping it discards messages already delivered to the client.
func TestJSConsumer_HeartbeatMiss_KeepsIterator(t *testing.T) {
	for _, tt := range []struct {
		name  string
		rc    *recovery.Controller
		byKey bool
	}{
		{name: "recovery_disabled"},
		{name: "recovery_enabled", rc: recovery.NewController(recovery.ControllerConfig{
			Strategy: recovery.FromNew, FetchTimeout: time.Second, Logger: logging.NewNop(),
		})},
		{name: "dispatch_by_key", byKey: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			iter := testutil.NewScriptedIter(testutil.IterData("a"), testutil.IterMiss(), testutil.IterData("b"))
			r := startJSKeepRun(t, tt.rc, tt.byKey, &scriptedConsumer{iters: []*testutil.ScriptedIter{iter}})

			r.requireHandled(t, "a")
			r.requireHandled(t, "b")
			<-iter.Idle()
			require.Zero(t, iter.Stops(), "the iterator must not be stopped on a missed heartbeat")
			require.Equal(t, int32(1), r.cons.calls.Load(), "the iterator must not be recreated")

			r.cancel()
			<-r.c.done
			require.Positive(t, iter.Stops(), "the kept iterator must be stopped on shutdown")
		})
	}
}

// A kept iterator must still be stopped when the loop exits before using it again.
//
// The context is cancelled from the consumer-info probe that follows the miss.
// processIterator has returned by then and no longer watches the context,
// so only run can stop the iterator.
func TestJSConsumer_HeartbeatMiss_KeptIteratorStoppedOnCancel(t *testing.T) {
	rc := recovery.NewController(recovery.ControllerConfig{
		Strategy: recovery.FromNew, FetchTimeout: time.Second, BurstThreshold: 1, Logger: logging.NewNop(),
	})
	var r *jsKeepRun
	// The startup checkpoint seed also calls Info; only the probe after the miss cancels.
	var missed atomic.Bool
	miss := testutil.IterMiss()
	miss.Before = func() { missed.Store(true) }
	iter := testutil.NewScriptedIter(miss)
	ready := make(chan struct{})
	cons := &scriptedConsumer{iters: []*testutil.ScriptedIter{iter}, onInfo: func() {
		if missed.Load() {
			<-ready
			r.cancel()
		}
	}}
	r = startJSKeepRun(t, rc, false, cons)
	close(ready)

	<-r.c.done
	require.Equal(t, 1, iter.Stops(), "an iterator kept across a miss must be stopped once when the loop exits")
}

// A missed heartbeat that turns out to be a missing stream must replace the iterator.
func TestJSConsumer_HeartbeatMiss_StreamMissing_ReplacesIterator(t *testing.T) {
	rc := recovery.NewController(recovery.ControllerConfig{
		Strategy: recovery.FromNew, FetchTimeout: time.Second, BurstThreshold: 1, Logger: logging.NewNop(),
	})
	iter := testutil.NewScriptedIter(testutil.IterMiss())
	cons := &scriptedConsumer{iters: []*testutil.ScriptedIter{iter}, infoErr: jetstream.ErrStreamNotFound}
	r := startJSKeepRun(t, rc, false, cons)

	require.Eventually(t, func() bool { return r.cons.calls.Load() >= 2 }, 2*time.Second, 5*time.Millisecond,
		"a new iterator must be created after the stream-missing backoff")
	require.Positive(t, iter.Stops(), "the old iterator must be stopped when it is replaced")
}
