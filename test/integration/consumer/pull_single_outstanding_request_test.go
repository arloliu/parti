package consumer_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// optsAs narrows options shared by every consumer type to one constructor's option type.
func optsAs[T any](t *testing.T, opts []consumer.Option) []T {
	t.Helper()
	out := make([]T, len(opts))
	for i, o := range opts {
		narrowed, ok := o.(T)
		require.True(t, ok, "option %d does not apply to this consumer type", i)
		out[i] = narrowed
	}

	return out
}

// TestPull_SingleOutstandingRequest_NoStrandAtMaxWaiting2 pins that no
// consumer type strands its iterator by overlapping pull requests.
//
// It replays the publish pattern of TestVerify_PullMaxWaitingDropsOverlappingPull.
// With nats.go's default re-pull threshold that pattern makes one iterator
// overlap three pull requests, the server silently drops the third because
// MaxWaiting is 2, and the next message waits for a missed heartbeat.
// A consumer that re-pulls only when nothing is outstanding never overlaps,
// so the last message must arrive at once.
// The waiting count is sampled after each phase; it bounds the overlap, it does not trace every request.
func TestPull_SingleOutstandingRequest_NoStrandAtMaxWaiting2(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	const (
		batch = 100
		// A stranded iterator recovers only after 2x this heartbeat (4s),
		// far beyond the delivery deadline asserted below.
		heartbeat = 2 * time.Second
		expiry    = 30 * time.Second
	)

	opts := []consumer.Option{
		consumer.WithBatchSize(batch),
		consumer.WithFetchTimeout(expiry),
		consumer.WithPullHeartbeatCap(heartbeat),
		consumer.WithMaxWaiting(2),
		consumer.WithMaxAckPending(1000),
	}

	tests := []struct {
		name  string
		start func(t *testing.T, js jetstream.JetStream, stream string, h consumer.MessageHandler)
	}{
		{"dynamic", func(t *testing.T, js jetstream.JetStream, stream string, h consumer.MessageHandler) {
			t.Helper()
			dc, err := consumer.NewDynamic(js, stream, "w", "single.{{.PartitionID}}", h, optsAs[consumer.DynamicOption](t, opts)...)
			require.NoError(t, err)
			t.Cleanup(func() { _ = dc.Stop(context.Background()) })
			require.NoError(t, dc.Update(t.Context(), "worker-1", []parti.Partition{{Keys: []string{"0"}}}))
		}},
		{"static", func(t *testing.T, js jetstream.JetStream, stream string, h consumer.MessageHandler) {
			t.Helper()
			sc, err := consumer.NewStatic(js, stream, "s", "single.{{partition}}", 1, 0, h, optsAs[consumer.StaticOption](t, opts)...)
			require.NoError(t, err)
			t.Cleanup(func() { _ = sc.Stop(context.Background()) })
			require.NoError(t, sc.Start(t.Context()))
		}},
		{"broadcast", func(t *testing.T, js jetstream.JetStream, stream string, h consumer.MessageHandler) {
			t.Helper()
			bopts := append(optsAs[consumer.BroadcastOption](t, opts), consumer.WithInstanceID("i1"))
			bc, err := consumer.NewBroadcast(js, stream, "b", "single.>", h, bopts...)
			require.NoError(t, err)
			t.Cleanup(func() { _ = bc.Stop(context.Background()) })
			require.NoError(t, bc.Start(t.Context()))
		}},
		{"queue", func(t *testing.T, js jetstream.JetStream, stream string, h consumer.MessageHandler) {
			t.Helper()
			q, err := consumer.NewQueue(js, stream, "q", "single.>", h, optsAs[consumer.QueueOption](t, opts)...)
			require.NoError(t, err)
			t.Cleanup(func() { _ = q.Stop(context.Background()) })
			require.NoError(t, q.Start(t.Context()))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nc, cleanup := testutil.StartEmbeddedNATS(t)
			defer cleanup()

			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()

			js, err := jetstream.New(nc)
			require.NoError(t, err)

			const streamName = "SINGLEPULL"
			stream, err := js.CreateStream(ctx, jetstream.StreamConfig{
				Name:     streamName,
				Subjects: []string{"single.*"},
				Storage:  jetstream.MemoryStorage,
			})
			require.NoError(t, err)

			var handled atomic.Int64
			tt.start(t, js, streamName, consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
				handled.Add(1)

				return nil
			}))

			// Each consumer type names its durable differently; the stream has exactly one.
			var durable string
			require.Eventually(t, func() bool {
				for name := range stream.ConsumerNames(ctx).Name() {
					durable = name
				}

				return durable != ""
			}, 5*time.Second, 20*time.Millisecond, "durable consumer not created")

			numWaiting := func() int {
				t.Helper()
				cons, consErr := js.Consumer(ctx, streamName, durable)
				require.NoError(t, consErr)

				return cons.CachedInfo().NumWaiting
			}

			total := 0
			// step publishes n messages and waits until all are handled within timeout.
			step := func(n int, timeout time.Duration, phase string) {
				t.Helper()
				for range n {
					_, pubErr := js.Publish(ctx, "single.0", []byte("x"))
					require.NoError(t, pubErr)
				}
				total += n
				want := int64(total)
				require.Eventually(t, func() bool { return handled.Load() >= want }, timeout, 5*time.Millisecond,
					"%s: handled %d of %d", phase, handled.Load(), want)
				assert.LessOrEqual(t, numWaiting(), 1, "%s: more than one pull request is waiting", phase)
			}

			step(60, 10*time.Second, "backlog")
			step(42, 10*time.Second, "second wave")
			step(49, 10*time.Second, "drain")
			// A stranded iterator would hold this message for 2x heartbeat.
			step(1, 1500*time.Millisecond, "after drain")
		})
	}
}
