package consumer_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestQueue_HeartbeatMiss_DeliveredMessageSurvives_MaxDeliver1 shows what a
// missed pull heartbeat costs when the iterator is stopped.
//
// The server has already sent the second message to the iterator when the miss is reported.
// Stopping the iterator drops it, whether it has reached the client or not,
// and with MaxDeliver 1 the server never sends it again: the message is lost.
// Keeping the iterator hands it to the handler.
func TestQueue_HeartbeatMiss_DeliveredMessageSurvives_MaxDeliver1(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const streamName = "QMISS"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"qmiss.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	for range 2 {
		_, err = js.Publish(ctx, "qmiss.a", []byte("x"))
		require.NoError(t, err)
	}

	var injector testutil.HeartbeatMissInjector
	var factoryCalls atomic.Int32
	var handled atomic.Int32
	q, err := consumer.NewQueue(js, streamName, "q", "qmiss.>",
		consumer.MessageHandlerFunc(func(context.Context, jetstream.Msg) error {
			handled.Add(1)

			return nil
		}),
		consumer.WithBatchSize(10),
		consumer.WithMaxAckPending(10),
		consumer.WithMaxDeliver(1),
		consumer.WithAckWait(time.Second),
		consumer.WithFetchTimeout(5*time.Second),
		consumer.WithIteratorFactory(func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
			iter, iterErr := cons.Messages(natsutil.PullMessagesOpts(batch, expiry, 0)...)
			if iterErr != nil {
				return nil, iterErr
			}
			factoryCalls.Add(1)

			return injector.Wrap(iter, cons, 2), nil
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	require.NoError(t, q.Start(ctx))

	require.Eventually(t, injector.Injected, 10*time.Second, 10*time.Millisecond, "heartbeat miss was never injected")
	require.False(t, injector.Undelivered(), "precondition broken: the server did not deliver both messages before the miss")

	// Longer than AckWait: a redelivery, were one allowed, would have happened by now.
	require.Eventually(t, func() bool { return handled.Load() == 2 }, 4*time.Second, 10*time.Millisecond,
		"the message delivered before the missed heartbeat was lost (handled %d of 2)", handled.Load())
	require.Equal(t, int32(1), factoryCalls.Load(), "the iterator must not be recreated")
}
