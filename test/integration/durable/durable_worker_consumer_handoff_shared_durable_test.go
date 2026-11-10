package durable_test

// During a handoff the old and the new owner of a partition pull from the same
// per-partition durable at the same time, so their pull requests share its MaxWaiting.
//
// With the default MaxWaiting of 2 this works only when each owner keeps a
// single pull request waiting, which is what the default iterator does.
// An owner still running the older iterator options overlaps two requests and
// fills both slots; the server then drops the new owner's request without a reply,
// and the new owner starts consuming only after its next missed heartbeat.
// That is the state a rolling upgrade passes through until the last old worker is gone.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestWorkerConsumer_HandoffOnSharedDurable_MaxWaiting2(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	const (
		streamName = "HANDOFFSHARED"
		subject    = "work.1"
		batch      = 100
		expiry     = 30 * time.Second
		// The new owner's request is retried on a missed heartbeat, 2x this after its last activity.
		heartbeat = time.Second
	)

	// overlappingFactory builds the iterator the way parti did before it kept
	// a single pull request outstanding: nats.go re-pulls below half a batch.
	overlappingFactory := func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
		return cons.Messages(
			jetstream.PullMaxMessages(batch),
			jetstream.PullExpiry(expiry),
			jetstream.PullHeartbeat(heartbeat),
		)
	}

	tests := []struct {
		name string
		// oldFactory is the old owner's iterator factory; nil selects the default.
		oldFactory func(jetstream.Consumer, int, time.Duration) (jetstream.MessagesContext, error)
		// oldWaiting is how many pull requests the old owner holds after the seed messages.
		oldWaiting int
		// deadline bounds how long the new owner may take to handle the first
		// message published after the old owner released the partition.
		deadline time.Duration
		// wantMiss is whether the new owner is expected to recover through a missed heartbeat.
		wantMiss bool
	}{
		{name: "both_owners_single_pull", oldWaiting: 1, deadline: time.Second},
		{
			name: "old_owner_overlapping_pulls", oldFactory: overlappingFactory, oldWaiting: 2,
			deadline: 2*heartbeat + 2*time.Second, wantMiss: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			nc, cleanup := testutil.StartEmbeddedNATS(t)
			defer cleanup()
			js, err := jetstream.New(nc)
			require.NoError(t, err)

			_, err = js.CreateStream(ctx, jetstream.StreamConfig{
				Name:     streamName,
				Subjects: []string{"work.*"},
				Storage:  jetstream.MemoryStorage,
			})
			require.NoError(t, err)

			newOwner := func(handled *atomic.Int64, metrics *transientCaptureMetrics,
				factory func(jetstream.Consumer, int, time.Duration) (jetstream.MessagesContext, error),
			) *durable.WorkerConsumer {
				t.Helper()
				wc, wcErr := durable.NewWorkerConsumer(js, durable.WorkerConsumerConfig{
					StreamName:       streamName,
					ConsumerPrefix:   "wkr",
					SubjectTemplate:  "work.{{.PartitionID}}",
					BatchSize:        batch,
					FetchTimeout:     expiry,
					PullHeartbeatCap: heartbeat,
					MaxWaiting:       2,
					MaxAckPending:    1000,
					Metrics:          metrics,
					IteratorFactory:  factory,
				}, func(_ context.Context, msg jetstream.Msg) error {
					handled.Add(1)

					return msg.Ack()
				})
				require.NoError(t, wcErr)

				return wc
			}
			partitions := []parti.Partition{{Keys: []string{"1"}}}

			var oldHandled, newHandled atomic.Int64
			oldWC := newOwner(&oldHandled, newTransientCaptureMetrics(), tt.oldFactory)
			defer oldWC.Close(context.Background())
			require.NoError(t, oldWC.UpdateWorkerConsumer(ctx, "worker-old", partitions))

			// Both owners derive the same durable name; locate it via the stream.
			var name string
			require.Eventually(t, func() bool {
				stream, streamErr := js.Stream(ctx, streamName)
				if streamErr != nil {
					return false
				}
				for ci := range stream.ListConsumers(ctx).Info() {
					name = ci.Name
					return true
				}

				return false
			}, 5*time.Second, 20*time.Millisecond, "per-partition durable not found")
			numWaiting := func() int {
				cons, consErr := js.Consumer(ctx, streamName, name)
				if consErr != nil {
					return -1
				}

				return cons.CachedInfo().NumWaiting
			}

			// More than half a batch: the older iterator issues its second, overlapping pull request.
			for range 60 {
				_, pubErr := js.Publish(ctx, subject, []byte("seed"))
				require.NoError(t, pubErr)
			}
			require.Eventually(t, func() bool { return oldHandled.Load() == 60 }, 10*time.Second, 5*time.Millisecond,
				"old owner did not handle the seed messages")
			require.Eventually(t, func() bool { return numWaiting() == tt.oldWaiting }, 5*time.Second, 20*time.Millisecond,
				"old owner pull requests waiting")

			// The new owner starts consuming while the old owner still holds the partition.
			newMetrics := newTransientCaptureMetrics()
			newWC := newOwner(&newHandled, newMetrics, nil)
			defer newWC.Close(context.Background())
			newStartedAt := time.Now()
			require.NoError(t, newWC.UpdateWorkerConsumer(ctx, "worker-new", partitions))
			// Both slots are in use either way: one request per owner, or two from the old owner.
			require.Eventually(t, func() bool { return numWaiting() == 2 }, 5*time.Second, 20*time.Millisecond,
				"two pull requests must be waiting during the handoff")

			// The old owner releases the partition.
			require.NoError(t, oldWC.Close(ctx))
			releasedAt := time.Now()

			_, err = js.Publish(ctx, subject, []byte("after-release"))
			require.NoError(t, err)
			require.Eventually(t, func() bool { return newHandled.Load() >= 1 }, tt.deadline, 5*time.Millisecond,
				"new owner did not take over within %v of the release", tt.deadline)
			t.Logf("new owner handled its first message %v after the release (%v after it started)",
				time.Since(releasedAt).Round(time.Millisecond), time.Since(newStartedAt).Round(time.Millisecond))

			newMetrics.mu.Lock()
			missed := newMetrics.restarts["heartbeat"]
			newMetrics.mu.Unlock()
			if tt.wantMiss {
				require.Positive(t, missed, "the new owner's dropped pull request is recovered by a missed heartbeat")
			} else {
				require.Zero(t, missed, "two single-pull owners fit in MaxWaiting 2 without a missed heartbeat")
			}
		})
	}
}
