package durable_test

// A consumer-leader stepdown must not cost a WorkerConsumer a missed heartbeat.
//
// nats-server tracks waiting pull requests for a new leader by reply inbox,
// and one nats.go iterator uses a single inbox for all of its pull requests.
// When two requests of one iterator overlap and one of them ends,
// the inbox is forgotten although the other request still waits,
// so a new leader sends no "409 Leadership Change"
// and the iterator waits for a missed heartbeat
// (see TestWorkerConsumer_LeaderStepdownMissedHeartbeat_KeepsIterator).
//
// The default iterator keeps one pull request outstanding,
// so the request waiting at the stepdown is always the one the server tracks.
// These scenarios drive the real WorkerConsumer with its default iterator factory
// on an R3 durable and step the consumer leader down in each pull state.
//
// Run with:
//
//	go test ./test/integration/durable/ -run TestWorkerConsumer_LeaderStepdown_SinglePull -count=1 -race -v

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestWorkerConsumer_LeaderStepdown_SinglePull_NoMissedHeartbeat(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	const (
		batch  = 10
		expiry = 6 * time.Second
		// A missed heartbeat surfaces 2x this after the last server activity (4s),
		// far beyond the delivery deadline asserted after the stepdown.
		heartbeat = 2 * time.Second
		// How long after the first pull request the 6th handler returns.
		// With nats.go's default re-pull threshold this is when a second,
		// overlapping pull request would be issued.
		dwell = 2 * time.Second
	)

	tests := []struct {
		name string
		// seed is the number of messages handled before the stepdown.
		seed int
		// minPulls is how many pull requests must have been issued before the stepdown.
		minPulls int32
		// afterFirstPull delays the stepdown until the first pull request is this old.
		afterFirstPull time.Duration
	}{
		// The first pull request is still waiting.
		{name: "first_pull_waiting", seed: 3, minPulls: 1},
		// The first pull request expired partially filled and was replaced.
		{name: "repull_after_expiry", seed: 6, minPulls: 2, afterFirstPull: expiry + 500*time.Millisecond},
		// The first pull request was filled completely and was replaced.
		{name: "repull_after_full_batch", seed: batch, minPulls: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			nc, _, cleanup := testutil.StartEmbeddedNATSCluster(t)
			defer cleanup()
			js, err := jetstream.New(nc)
			require.NoError(t, err)

			_, err = js.CreateStream(ctx, jetstream.StreamConfig{
				Name:     stepdownHBStream,
				Subjects: []string{"work.*"},
				Storage:  jetstream.MemoryStorage,
				Replicas: 3,
			})
			require.NoError(t, err)

			// Observe the consumer's pull requests as the server receives them.
			var pulls atomic.Int32
			var firstPullAt atomic.Pointer[time.Time]
			sub, err := nc.Subscribe("$JS.API.CONSUMER.MSG.NEXT."+stepdownHBStream+".*", func(*nats.Msg) {
				if pulls.Add(1) == 1 {
					now := time.Now()
					firstPullAt.Store(&now)
				}
			})
			require.NoError(t, err)
			defer func() { _ = sub.Unsubscribe() }()

			var handled atomic.Int64
			handler := func(_ context.Context, msg jetstream.Msg) error {
				if handled.Add(1) == 6 && tt.afterFirstPull > 0 {
					// Deliberate timing dwell, not a wait for state.
					// A message is only delivered to a pull request, so the first pull was seen.
					for firstPullAt.Load() == nil {
						time.Sleep(time.Millisecond)
					}
					time.Sleep(time.Until(firstPullAt.Load().Add(dwell)))
				}

				return msg.Ack()
			}

			metrics := newTransientCaptureMetrics()
			wc, err := durable.NewWorkerConsumer(js, durable.WorkerConsumerConfig{
				StreamName:       stepdownHBStream,
				ConsumerPrefix:   "wkr",
				SubjectTemplate:  "work.{{.PartitionID}}",
				BatchSize:        batch,
				FetchTimeout:     expiry,
				PullHeartbeatCap: heartbeat,
				MaxWaiting:       2,
				MaxAckPending:    100,
				ConsumerReplicas: 3,
				Metrics:          metrics,
			}, handler)
			require.NoError(t, err)
			defer wc.Close(context.Background())

			require.NoError(t, wc.UpdateWorkerConsumer(ctx, "worker-SD", []parti.Partition{{Keys: []string{"1"}}}))

			// The durable name is derived internally; locate it via the stream.
			cons := testutil.WaitSoleConsumer(t, js, stepdownHBStream)

			// peakWaiting is the most pull requests any consumer info reported waiting.
			peakWaiting := 0
			waitInfo := func(timeout time.Duration, what string, pred func(*jetstream.ConsumerInfo) bool) *jetstream.ConsumerInfo {
				t.Helper()

				return testutil.WaitConsumerInfo(t, cons, timeout, what, func(ci *jetstream.ConsumerInfo) bool {
					peakWaiting = max(peakWaiting, ci.NumWaiting)

					return pred(ci)
				})
			}

			for range tt.seed {
				_, pubErr := js.Publish(ctx, stepdownHBSubject, []byte("seed"))
				require.NoError(t, pubErr)
			}
			require.Eventually(t, func() bool { return handled.Load() >= int64(tt.seed) }, 10*time.Second, 20*time.Millisecond,
				"seed messages not handled")
			require.Eventually(t, func() bool {
				first := firstPullAt.Load()

				return first != nil && pulls.Load() >= tt.minPulls && time.Since(*first) >= tt.afterFirstPull
			}, expiry+10*time.Second, 20*time.Millisecond, "pull requests before the stepdown")
			before := waitInfo(5*time.Second, "one pull request waiting with current replicas",
				func(ci *jetstream.ConsumerInfo) bool {
					return ci.NumWaiting == 1 && testutil.ConsumerReplicasCurrent(ci, 2)
				})
			t.Logf("before stepdown: pulls=%d first pull age=%v",
				pulls.Load(), time.Since(*firstPullAt.Load()).Round(time.Millisecond))

			stepdownAt := time.Now()
			testutil.RequestConsumerLeaderStepdown(t, nc, cons)
			// Through waitInfo, so the samples taken during the election count towards peakWaiting.
			waitInfo(10*time.Second, "new consumer leader", func(ci *jetstream.ConsumerInfo) bool {
				return ci.Cluster.Leader != "" && ci.Cluster.Leader != before.Cluster.Leader
			})
			t.Logf("new leader %v after stepdown", time.Since(stepdownAt).Round(time.Millisecond))

			// 1. Delivery resumes at once, not after a missed heartbeat.
			_, err = js.Publish(ctx, stepdownHBSubject, []byte("after-stepdown"))
			require.NoError(t, err)
			publishedAt := time.Now()
			want := int64(tt.seed) + 1
			require.Eventually(t, func() bool { return handled.Load() >= want }, 1500*time.Millisecond, 5*time.Millisecond,
				"message published after the stepdown was not handled promptly (missed heartbeat takes %v)", 2*heartbeat)
			t.Logf("post-stepdown message handled in %v", time.Since(publishedAt).Round(time.Millisecond))

			// 2. No heartbeat is missed, even once the detection window has passed.
			time.Sleep(time.Until(stepdownAt.Add(2*heartbeat + 2*time.Second)))
			require.Zero(t, stepdownHBMissed(metrics), "a consumer leader stepdown must not cause a missed heartbeat")

			// 3. No sampled consumer info showed overlapping pull requests.
			waitInfo(5*time.Second, "final consumer info", func(*jetstream.ConsumerInfo) bool { return true })
			require.LessOrEqual(t, peakWaiting, 1, "more than one pull request was waiting")
		})
	}
}
