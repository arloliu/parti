package durable_test

// Losing the server that leads a per-partition durable must not cost a
// WorkerConsumer a missed heartbeat: the next consumer leader tells the waiting
// pull request about the change, and delivery resumes once that leader is elected.
//
// The server is shut down in-process, which is an orderly shutdown:
// it hands its leaderships over before it stops, as a rolling restart does.
// A crashed server, which hands nothing over, is not covered here.
// The scenarios differ in whether the consumer's connection is to the lost server,
// in which case it also reconnects.
//
// Run with:
//
//	go test ./test/integration/durable/ -run TestWorkerConsumer_LeaderNodeShutdown -count=1 -race -v

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestWorkerConsumer_LeaderNodeShutdown_SinglePull_NoMissedHeartbeat(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	const (
		batch  = 10
		expiry = 30 * time.Second
		// Long enough that a slow leader change alone cannot cause a miss,
		// so a miss means the new leader did not notify the waiting pull request.
		heartbeat = 12 * time.Second
		seed      = 3
		after     = 5
	)

	tests := []struct {
		name string
		// clientOnLeader connects the consumer to the server that will be shut down.
		clientOnLeader bool
	}{
		{name: "client_on_another_server"},
		{name: "client_on_the_lost_server", clientOnLeader: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			adminNC, servers, cleanup := testutil.StartEmbeddedNATSCluster(t)
			defer cleanup()
			adminJS, err := jetstream.New(adminNC)
			require.NoError(t, err)

			_, err = adminJS.CreateStream(ctx, jetstream.StreamConfig{
				Name:     stepdownHBStream,
				Subjects: []string{"work.*"},
				Storage:  jetstream.MemoryStorage,
				Replicas: 3,
			})
			require.NoError(t, err)

			// The consumer has its own connection, to a known server.
			clientServer := servers[0]
			consNC, err := nats.Connect(clientServer.ClientURL(), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond))
			require.NoError(t, err)
			defer consNC.Close()
			consJS, err := jetstream.New(consNC)
			require.NoError(t, err)

			var handled atomic.Int64
			metrics := newTransientCaptureMetrics()
			wc, err := durable.NewWorkerConsumer(consJS, durable.WorkerConsumerConfig{
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
			}, func(_ context.Context, msg jetstream.Msg) error {
				handled.Add(1)

				return msg.Ack()
			})
			require.NoError(t, err)
			defer wc.Close(context.Background())
			require.NoError(t, wc.UpdateWorkerConsumer(ctx, "worker-NS", []parti.Partition{{Keys: []string{"1"}}}))

			cons := testutil.WaitSoleConsumer(t, adminJS, stepdownHBStream)

			for range seed {
				_, pubErr := adminJS.Publish(ctx, stepdownHBSubject, []byte("seed"))
				require.NoError(t, pubErr)
			}
			require.Eventually(t, func() bool { return handled.Load() >= seed }, 10*time.Second, 20*time.Millisecond,
				"seed messages not handled")

			// Move the consumer leader until it is, or is not, on the consumer's server.
			onClientServer := func(ci *jetstream.ConsumerInfo) bool { return ci.Cluster.Leader == clientServer.Name() }
			var before *jetstream.ConsumerInfo
			for range 20 {
				before = testutil.WaitConsumerInfo(t, cons, 10*time.Second, "one pull request waiting with current replicas",
					func(ci *jetstream.ConsumerInfo) bool {
						return ci.Cluster.Leader != "" && ci.NumWaiting == 1 && testutil.ConsumerReplicasCurrent(ci, 2)
					})
				if onClientServer(before) == tt.clientOnLeader {
					break
				}
				testutil.StepDownConsumerLeader(t, adminNC, cons, before.Cluster.Leader)
			}
			require.Equal(t, tt.clientOnLeader, onClientServer(before), "could not place the consumer leader")
			missedBefore := stepdownHBMissed(metrics)

			// Publish through a server that survives.
			var lost, survivor *server.Server
			for _, s := range servers {
				switch {
				case s.Name() == before.Cluster.Leader:
					lost = s
				case survivor == nil:
					survivor = s
				}
			}
			require.NotNil(t, lost)
			require.NotNil(t, survivor)
			pubNC, err := nats.Connect(survivor.ClientURL(), nats.MaxReconnects(-1))
			require.NoError(t, err)
			defer pubNC.Close()
			pubJS, err := jetstream.New(pubNC)
			require.NoError(t, err)

			lostAt := time.Now()
			lost.Shutdown()
			lost.WaitForShutdown()

			// The stream may have lost its leader too; retry until it accepts messages again.
			for i := range after {
				require.Eventually(t, func() bool {
					pctx, cancel := context.WithTimeout(ctx, time.Second)
					defer cancel()
					_, pubErr := pubJS.Publish(pctx, stepdownHBSubject, []byte("after"), jetstream.WithMsgID(fmt.Sprintf("after-%d", i)))

					return pubErr == nil
				}, 30*time.Second, 50*time.Millisecond, "stream did not accept a message after the server was lost")
			}
			publishedAt := time.Now()

			want := int64(seed + after)
			require.Eventually(t, func() bool { return handled.Load() >= want }, 2*heartbeat+10*time.Second, 20*time.Millisecond,
				"messages published after the server was lost were not handled")
			t.Logf("delivery resumed %v after the server was lost (%v after the stream accepted the messages)",
				time.Since(lostAt).Round(10*time.Millisecond), time.Since(publishedAt).Round(10*time.Millisecond))

			require.Zero(t, stepdownHBMissed(metrics)-missedBefore,
				"losing the consumer leader's server must not cause a missed heartbeat")
		})
	}
}

// stepdownHBMissed returns how many missed heartbeats the metrics recorded.
func stepdownHBMissed(m *transientCaptureMetrics) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.restarts["heartbeat"]
}
