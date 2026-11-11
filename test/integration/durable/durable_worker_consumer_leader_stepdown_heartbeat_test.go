package durable_test

// Reproducer: a consumer-leader stepdown on an R3 per-partition durable, while
// two pull requests from one iterator have overlapped and the first has expired,
// makes nats-server v2.14.1 send no "409 Leadership Change" to the iterator's
// shared inbox.
// The blocked Next() then fails with jetstream.ErrNoHeartbeat even
// though the iterator is healthy and recovers by itself on the next Next().
// The raw nats.go characterization lives in test/integration/consumer
// (pull_leader_stepdown_heartbeat_verify_test.go).
//
// This test drives the same choreography through parti's real WorkerConsumer.
// Desired behavior: the missed heartbeat must not tear down the iterator.
//
// Run with:
//
//	go test ./test/integration/durable/ -run TestWorkerConsumer_LeaderStepdownMissedHeartbeat_KeepsIterator -count=1 -race -v

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

const (
	stepdownHBStream    = "STEPDOWNHB"
	stepdownHBSubject   = "work.1"
	stepdownHBBatch     = 10
	stepdownHBExpiry    = 4 * time.Second
	stepdownHBHeartbeat = 500 * time.Millisecond
	// stepdownHBDwell is how long after the first pull request the handler holds
	// its return, so the consumer's next Next() (and therefore its second pull
	// request) is issued when the first is ~2s old.
	stepdownHBDwell = 2 * time.Second
)

func TestWorkerConsumer_LeaderStepdownMissedHeartbeat_KeepsIterator(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

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

	var handled atomic.Int64
	// firstPullAt is when the first iterator was built;
	// its first Next() issues the first pull request right after.
	var firstPullAt atomic.Pointer[time.Time]
	handler := func(_ context.Context, msg jetstream.Msg) error {
		n := handled.Add(1)
		if n == 6 {
			// Deliberate timing dwell, not a wait for state: it separates the
			// expiry of the first pull request from that of the second.
			// It is measured from the first pull, so time spent before the
			// publish does not eat into that pull's life; no wait if already past.
			time.Sleep(time.Until(firstPullAt.Load().Add(stepdownHBDwell)))
		}

		return msg.Ack()
	}

	metrics := newTransientCaptureMetrics()
	var factoryCalls atomic.Int32
	wc, err := durable.NewWorkerConsumer(js, durable.WorkerConsumerConfig{
		StreamName:       stepdownHBStream,
		ConsumerPrefix:   "wkr",
		SubjectTemplate:  "work.{{.PartitionID}}",
		BatchSize:        stepdownHBBatch,
		FetchTimeout:     stepdownHBExpiry,
		PullHeartbeatCap: stepdownHBHeartbeat,
		MaxAckPending:    100,
		ConsumerReplicas: 3,
		Metrics:          metrics,
		IteratorFactory: func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
			if factoryCalls.Add(1) == 1 {
				now := time.Now()
				firstPullAt.Store(&now)
			}

			return cons.Messages(
				jetstream.PullMaxMessages(batch),
				jetstream.PullExpiry(expiry),
				jetstream.PullHeartbeat(stepdownHBHeartbeat),
			)
		},
	}, handler)
	require.NoError(t, err)
	defer wc.Close(context.Background())

	require.NoError(t, wc.UpdateWorkerConsumer(ctx, "worker-SD", []parti.Partition{{Keys: []string{"1"}}}))

	// The durable name is derived internally; locate it via the stream.
	cons := testutil.WaitSoleConsumer(t, js, stepdownHBStream)
	require.Equal(t, 3, cons.CachedInfo().Config.Replicas, "durable must be R3")

	for range 6 {
		_, err := js.Publish(ctx, stepdownHBSubject, []byte("seed"))
		require.NoError(t, err)
	}

	// The 6th handler returns ~2s after the first pull request, so the second is issued then.
	pullsStart := time.Now()
	testutil.WaitConsumerInfo(t, cons, 8*time.Second, "both pulls waiting",
		func(ci *jetstream.ConsumerInfo) bool { return ci.NumWaiting == 2 })
	testutil.WaitConsumerInfo(t, cons, stepdownHBExpiry+time.Second, "first pull expired",
		func(ci *jetstream.ConsumerInfo) bool { return ci.NumWaiting == 1 })
	before := testutil.WaitConsumerInfo(t, cons, time.Second, "replicas current with exactly one pull waiting",
		func(ci *jetstream.ConsumerInfo) bool {
			require.Equal(t, 1, ci.NumWaiting, "precondition broken: client re-pulled after first pull expired")

			return testutil.ConsumerReplicasCurrent(ci, 2)
		})
	t.Logf("pull overlap reached %v after publish", time.Since(pullsStart).Round(time.Millisecond))

	stepdownAt := time.Now()
	testutil.StepDownConsumerLeader(t, nc, cons, before.Cluster.Leader)

	// 1. Precondition: the real missed heartbeat happened.
	require.Eventually(t, func() bool { return stepdownHBMissed(metrics) >= 1 }, 5*time.Second, 20*time.Millisecond,
		"scenario did not reproduce: no IncrementWorkerConsumerIteratorRestart(\"heartbeat\") within 5s of the "+
			"consumer leader stepdown (this test characterizes the pinned nats-server version)")
	t.Logf("heartbeat metric seen %v after stepdown", time.Since(stepdownAt).Round(time.Millisecond))

	// 2. The consumer still delivers.
	_, err = js.Publish(ctx, stepdownHBSubject, []byte("after-stepdown"))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return handled.Load() >= 7 }, 5*time.Second, 20*time.Millisecond,
		"message published after the stepdown was not handled")

	// 3. Desired behavior: the missed heartbeat did not cost the iterator.
	require.Equal(t, int32(1), factoryCalls.Load(),
		"a missed heartbeat after a leader stepdown must keep the iterator; it was torn down and recreated")
}
