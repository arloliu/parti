package consumer_test

// TestVerify_PullLeaderStepdownHeartbeat characterizes nats-server v2.14
// (with nats.go v1.55.0) on a real 3-node embedded cluster.
//
// Hypothesis under test, derived from server/consumer.go
// (addClusterPendingRequest, removeClusterPendingRequest, checkPendingRequests):
// an R>1 pull consumer replicates its waiting pull requests to followers
// as a SET keyed by reply subject.
// A new consumer leader sends "409 Leadership Change" to every key in that set,
// which makes nats.go reset its pending count and re-issue the pull.
// A Messages() iterator uses ONE inbox for all of its pull requests,
// so when two of its requests overlap and the first one ends,
// the replicated remove deletes the key while the second request still waits.
// A leader change then produces no 409 for that inbox,
// the client keeps believing a pull is outstanding,
// and Next() returns jetstream.ErrNoHeartbeat after 2x the pull heartbeat.
//
// OBSERVED (hypothesis confirmed, 10/10 runs of each case under -race):
//   - Overlapping pulls: after the stepdown the shared inbox receives
//     zero "409 Leadership Change" messages, the new leader reports NumWaiting=0,
//     and the blocked Next() returns ErrNoHeartbeat about 1s (2x heartbeat) later.
//     The same iterator then recovers by itself:
//     the following Next() re-pulls and delivers a newly published message.
//   - Single pull (control): the inbox receives one 409 about 15ms after the stepdown,
//     the client re-pulls on the new leader, and Next() returns the next message with no error.
//
// The 15ms figure also rules out a slow election as the cause of the ErrNoHeartbeat.
// Not verified: the followers' replicated set itself (unexported server state);
// "replicas Current with Lag 0" plus the missing 409 is the evidence for it.
//
// Run with:
//
//	go test ./test/integration/consumer/ -run TestVerify_PullLeaderStepdownHeartbeat -count=1 -race -v

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/internal/testutil"
)

const (
	stepdownPullMaxMessages = 10
	stepdownPullExpiry      = 4 * time.Second
	stepdownPullHeartbeat   = 500 * time.Millisecond
	// stepdownPullDwell separates the expiries of the two overlapping pulls;
	// it is also the width of the window in which the stepdown must land.
	stepdownPullDwell = 2 * time.Second
	stepdownNextWait  = 5 * time.Second
)

// leadershipChangeSniffer records every "409 Leadership Change" status message
// the server sends to any inbox on the connection.
type leadershipChangeSniffer struct {
	mu      sync.Mutex
	arrived []time.Time
}

func sniffLeadershipChanges(t *testing.T, nc *nats.Conn) *leadershipChangeSniffer {
	t.Helper()
	sniffer := &leadershipChangeSniffer{}
	sub, err := nc.Subscribe("_INBOX.>", func(msg *nats.Msg) {
		if msg.Header.Get("Status") != "409" || !strings.Contains(msg.Header.Get("Description"), "Leadership Change") {
			return
		}
		sniffer.mu.Lock()
		sniffer.arrived = append(sniffer.arrived, time.Now())
		sniffer.mu.Unlock()
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	return sniffer
}

// since reports how many 409s arrived and how long after start each one did.
func (s *leadershipChangeSniffer) since(start time.Time) (int, []time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	offsets := make([]time.Duration, 0, len(s.arrived))
	for _, at := range s.arrived {
		offsets = append(offsets, at.Sub(start).Round(time.Millisecond))
	}

	return len(offsets), offsets
}

func TestVerify_PullLeaderStepdownHeartbeat(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	nc, _, cleanup := testutil.StartEmbeddedNATSCluster(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	makeConsumer := func(t *testing.T, streamName, consName string) jetstream.Consumer {
		t.Helper()
		_, err := js.CreateOrUpdateStream(t.Context(), jetstream.StreamConfig{
			Name:     streamName,
			Subjects: []string{streamName + ".evt"},
			Storage:  jetstream.MemoryStorage,
			Replicas: 3,
		})
		require.NoError(t, err)
		cons, err := js.CreateOrUpdateConsumer(t.Context(), streamName, jetstream.ConsumerConfig{
			Durable:   consName,
			AckPolicy: jetstream.AckExplicitPolicy,
			Replicas:  3,
		})
		require.NoError(t, err)

		return cons
	}

	messages := func(t *testing.T, cons jetstream.Consumer) jetstream.MessagesContext {
		t.Helper()
		iter, err := cons.Messages(
			jetstream.PullMaxMessages(stepdownPullMaxMessages),
			jetstream.PullExpiry(stepdownPullExpiry),
			jetstream.PullHeartbeat(stepdownPullHeartbeat),
		)
		require.NoError(t, err)

		return iter
	}

	type nextResult struct {
		msg jetstream.Msg
		err error
		at  time.Time
	}
	blockInNext := func(iter jetstream.MessagesContext) <-chan nextResult {
		resCh := make(chan nextResult, 1)
		go func() {
			msg, err := iter.Next()
			resCh <- nextResult{msg: msg, err: err, at: time.Now()}
		}()

		return resCh
	}

	// Two pull requests from one iterator overlap, the first expires,
	// then the consumer leader changes while the second is still waiting.
	t.Run("OverlappingPulls_NoLeadershipChange409_ErrNoHeartbeat", func(t *testing.T) {
		const stream, name = "STEPDOWNHB1", "stepdown-overlap"
		cons := makeConsumer(t, stream, name)
		sniffer := sniffLeadershipChanges(t, nc)

		for range 6 {
			_, err := js.Publish(t.Context(), stream+".evt", []byte("seed"))
			require.NoError(t, err)
		}
		iter := messages(t, cons)
		defer iter.Stop()

		// The first Next issues pull1 (batch 10); six deliveries leave client pending at 4.
		for range 6 {
			msg, err := iter.Next(jetstream.NextMaxWait(stepdownNextWait))
			require.NoError(t, err)
			require.NoError(t, msg.Ack())
		}

		// Deliberate timing dwell, not a wait for state:
		// it separates the expiry of pull1 from the expiry of pull2.
		time.Sleep(stepdownPullDwell)

		// This Next issues pull2 (batch 6, client pending back to 10) and blocks.
		pull2At := time.Now()
		resCh := blockInNext(iter)

		testutil.WaitConsumerInfo(t, cons, 2*time.Second, "both pulls waiting",
			func(info *jetstream.ConsumerInfo) bool { return info.NumWaiting == 2 })
		testutil.WaitConsumerInfo(t, cons, stepdownPullExpiry, "pull1 expired",
			func(info *jetstream.ConsumerInfo) bool { return info.NumWaiting == 1 })
		// The 408 of pull1 leaves client pending at 6 (>= threshold 5), so the client must not re-pull.
		before := testutil.WaitConsumerInfo(t, cons, time.Second, "replicas current",
			func(info *jetstream.ConsumerInfo) bool {
				require.Equal(t, 1, info.NumWaiting, "precondition broken: client re-pulled after pull1 expired")

				return testutil.ConsumerReplicasCurrent(info, 2)
			})
		require.Less(t, time.Since(pull2At), stepdownPullExpiry-500*time.Millisecond,
			"precondition broken: stepdown would land too close to pull2 expiry")

		stepdownAt := time.Now()
		after := testutil.StepDownConsumerLeader(t, nc, cons, before.Cluster.Leader)
		election := time.Since(stepdownAt)
		t.Logf("leader %s -> %s visible after<=%v; new leader NumWaiting=%d; pull2 age at stepdown=%v",
			before.Cluster.Leader, after.Cluster.Leader, election.Round(time.Millisecond),
			after.NumWaiting, stepdownAt.Sub(pull2At).Round(time.Millisecond))

		var res nextResult
		select {
		case res = <-resCh:
		case <-time.After(stepdownNextWait):
			count, offsets := sniffer.since(stepdownAt)
			info, _ := cons.Info(t.Context())
			require.FailNowf(t, "blocked Next did not return",
				"RESULT overlapping: Next still blocked %v after stepdown | 409s=%d at %v | info=%+v",
				stepdownNextWait, count, offsets, info)
		}
		count, offsets := sniffer.since(stepdownAt)
		t.Logf("RESULT overlapping: err=%v | msg=%v | stepdown->Next return=%v | new leader visible after<=%v | "+
			"409 Leadership Change count=%d at %v | new leader NumWaiting=%d",
			res.err, res.msg != nil, res.at.Sub(stepdownAt).Round(time.Millisecond),
			election.Round(time.Millisecond), count, offsets, after.NumWaiting)

		require.ErrorIs(t, res.err, jetstream.ErrNoHeartbeat)
		require.Zero(t, count, "hypothesis: no 409 Leadership Change reaches the shared inbox")
		require.Zero(t, after.NumWaiting, "hypothesis: the new leader has no waiting request")

		// Same iterator, not stopped: the next Next must re-pull and deliver.
		_, err := js.Publish(t.Context(), stream+".evt", []byte("after-stepdown"))
		require.NoError(t, err)
		msg, err := iter.Next(jetstream.NextMaxWait(stepdownNextWait))
		require.NoError(t, err, "same iterator must self-recover after ErrNoHeartbeat")
		require.Equal(t, "after-stepdown", string(msg.Data()))
		require.NoError(t, msg.Ack())
		late, _ := sniffer.since(stepdownAt)
		require.Zero(t, late, "no late 409 Leadership Change may explain the recovery")
		t.Log("RESULT overlapping: same iterator recovered and delivered the next message; still no 409")
	})

	// Control: one outstanding pull, so its reply subject is still in the replicated set.
	t.Run("SinglePull_LeadershipChange409_TransparentRepull", func(t *testing.T) {
		const stream, name = "STEPDOWNHB2", "stepdown-single"
		cons := makeConsumer(t, stream, name)
		sniffer := sniffLeadershipChanges(t, nc)

		iter := messages(t, cons)
		defer iter.Stop()
		resCh := blockInNext(iter)

		before := testutil.WaitConsumerInfo(t, cons, 2*time.Second, "single pull waiting, replicas current",
			func(info *jetstream.ConsumerInfo) bool {
				require.LessOrEqual(t, info.NumWaiting, 1, "precondition broken: more than one pull outstanding")

				return info.NumWaiting == 1 && testutil.ConsumerReplicasCurrent(info, 2)
			})

		stepdownAt := time.Now()
		after := testutil.StepDownConsumerLeader(t, nc, cons, before.Cluster.Leader)
		election := time.Since(stepdownAt)
		repulled := testutil.WaitConsumerInfo(t, cons, stepdownNextWait, "client re-pulled on the new leader",
			func(info *jetstream.ConsumerInfo) bool { return info.NumWaiting == 1 })

		_, err := js.Publish(t.Context(), stream+".evt", []byte("after-stepdown"))
		require.NoError(t, err)

		var res nextResult
		select {
		case res = <-resCh:
		case <-time.After(stepdownNextWait):
			count, offsets := sniffer.since(stepdownAt)
			require.FailNowf(t, "blocked Next did not return",
				"RESULT single: Next still blocked %v after publish | 409s=%d at %v",
				stepdownNextWait, count, offsets)
		}
		count, offsets := sniffer.since(stepdownAt)
		t.Logf("RESULT single: err=%v | msg=%v | leader %s -> %s | new leader visible after<=%v | "+
			"409 Leadership Change count=%d at %v | new leader NumWaiting=%d then %d",
			res.err, res.msg != nil, before.Cluster.Leader, after.Cluster.Leader,
			election.Round(time.Millisecond), count, offsets, after.NumWaiting, repulled.NumWaiting)

		require.NoError(t, res.err, "control: a single outstanding pull must survive the leader change")
		require.Equal(t, "after-stepdown", string(res.msg.Data()))
		require.NoError(t, res.msg.Ack())
		// The sniffer has its own subscription, so its callback can run after Next() returned.
		require.Eventually(t, func() bool {
			n, _ := sniffer.since(stepdownAt)

			return n >= 1
		}, stepdownNextWait, 10*time.Millisecond, "control: the new leader must send 409 Leadership Change")
	})
}
