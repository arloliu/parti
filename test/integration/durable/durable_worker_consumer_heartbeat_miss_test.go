package durable_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/natsutil"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// hbInjectIter wraps a real iterator. Its Next() delegates normally, except
// that exactly once, after the first message was returned and the server
// reports every message delivered to the client, it returns
// jetstream.ErrNoHeartbeat without touching the real iterator.
type hbInjectIter struct {
	jetstream.MessagesContext

	cons    jetstream.Consumer
	wantSeq uint64
	calls   int
	state   *hbInjectState
}

// hbInjectState is shared by every wrapper so the miss fires once overall,
// even if a (buggy) restart builds a second iterator.
type hbInjectState struct {
	claimed  atomic.Bool
	injected atomic.Bool // set just before the error is returned
	// undelivered is set, before injected, when the server never reported every message delivered.
	// Next() may run off the test goroutine, so the test asserts on this flag itself.
	undelivered atomic.Bool
}

func (w *hbInjectIter) Next(opts ...jetstream.NextOpt) (jetstream.Msg, error) {
	w.calls++
	if w.calls != 2 || !w.state.claimed.CompareAndSwap(false, true) {
		return w.MessagesContext.Next(opts...)
	}

	// Wait until the server has delivered every message into this iterator's
	// inbox, so they are buffered client-side when the miss is injected.
	delivered := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info, err := w.cons.Info(context.Background())
		if err == nil && info.Delivered.Consumer >= w.wantSeq {
			delivered = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	w.state.undelivered.Store(!delivered)
	w.state.injected.Store(true)

	return nil, jetstream.ErrNoHeartbeat
}

// TestWorkerConsumer_HeartbeatMiss_InFlightMessagesSurvive is a FAULT
// INJECTION test. A real ErrNoHeartbeat fires with an empty client buffer, so
// this does not reproduce the exact production state. It proves the property:
// messages already in flight to the iterator's inbox survive a heartbeat miss,
// i.e. they are handled promptly (well under AckWait) with NumDelivered == 1
// instead of being discarded by Stop() and redelivered after AckWait.
func TestWorkerConsumer_HeartbeatMiss_InFlightMessagesSurvive(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	const total = 10

	modes := []struct {
		name     string
		strategy durable.RecoveryStrategy
	}{
		{"recovery_disabled", durable.RecoveryDisabled},
		{"recovery_enabled", durable.RecoverFromLastProcessed},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			ctx := t.Context()
			_, nc := partitesting.StartEmbeddedNATS(t)
			js, err := jetstream.New(nc)
			require.NoError(t, err)

			_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "hb-stream", Subjects: []string{"hb.*"}})
			require.NoError(t, err)
			for range total {
				_, err = js.Publish(ctx, "hb.1", []byte("m"))
				require.NoError(t, err)
			}

			var (
				mu           sync.Mutex
				seenSeq      = map[uint64]uint64{} // stream seq -> NumDelivered
				factoryCalls atomic.Int32
				state        hbInjectState
			)
			handler := func(_ context.Context, msg jetstream.Msg) error {
				md, mdErr := msg.Metadata()
				if mdErr != nil {
					return mdErr
				}
				mu.Lock()
				seenSeq[md.Sequence.Stream] = md.NumDelivered
				mu.Unlock()

				return msg.Ack()
			}

			wc, err := durable.NewWorkerConsumer(js, durable.WorkerConsumerConfig{
				StreamName:       "hb-stream",
				ConsumerPrefix:   "wkr",
				SubjectTemplate:  "hb.{{.PartitionID}}",
				BatchSize:        total,
				MaxAckPending:    100,
				AckWait:          3 * time.Second,
				RecoveryStrategy: mode.strategy,
				IteratorFactory: func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
					factoryCalls.Add(1)
					inner, iterErr := cons.Messages(
						jetstream.PullMaxMessages(batch),
						jetstream.PullExpiry(expiry),
						jetstream.PullHeartbeat(natsutil.DerivePullHeartbeat(expiry, 0)),
					)
					if iterErr != nil {
						return nil, iterErr
					}

					return &hbInjectIter{MessagesContext: inner, cons: cons, wantSeq: total, state: &state}, nil
				},
			}, handler)
			require.NoError(t, err)
			defer wc.Close(context.Background())

			require.NoError(t, wc.UpdateWorkerConsumer(ctx, "worker-HB", []parti.Partition{{Keys: []string{"1"}}}))

			require.Eventually(t, state.injected.Load, 10*time.Second, 10*time.Millisecond, "heartbeat miss was never injected")
			require.False(t, state.undelivered.Load(),
				"precondition broken: server did not report all %d messages delivered before the miss was injected", total)

			require.Eventually(t, func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(seenSeq) == total
			}, 1500*time.Millisecond, 10*time.Millisecond,
				"all %d messages must be handled within 1.5s of the miss (AckWait is 3s)", total)

			mu.Lock()
			for seq, delivered := range seenSeq {
				require.Equal(t, uint64(1), delivered, "message seq %d was redelivered", seq)
			}
			mu.Unlock()
			require.Equal(t, int32(1), factoryCalls.Load(), "iterator must not be recreated after a heartbeat miss")
		})
	}
}
