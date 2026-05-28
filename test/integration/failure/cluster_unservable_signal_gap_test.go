package failure_test

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
)

// TestCluster_SustainedQuorumLoss_NotifiesApp is a BLACK-BOX test asserting only
// app-observable outcomes: parti notifies the application when a consumer is
// unservable and needs operator action.
//
// Scenario: an RF5 file stream with RF5 KV (manager stays healthy) and memory-RF3
// dynamic consumers. Two of five nodes are crashed and NOT restored, so some
// consumers permanently lose raft quorum. Parti cannot recreate them (verified:
// DeleteConsumer needs the lost quorum) and cannot resume them (nodes never
// return) — this genuinely requires operator action on NATS.
//
// Requirement pinned: with WithOnConsumerUnservable registered, the app receives
// the unservable signal for the affected subject (while the connection stays UP
// and the manager stays healthy), and the terminal OnPermanentFailure does NOT
// fire. This is the case that, before the feature, left the app blind.
func TestCluster_SustainedQuorumLoss_NotifiesApp(t *testing.T) {
	requireClusterCrashTests(t)

	ctx := context.Background()
	const (
		streamName = "GAP"
		prefix     = "gap"
		subjectTpl = "gap.{{.PartitionID}}"
		numParts   = 8
	)
	subjectFor := func(i int) string { return "gap.partition-" + strconv.Itoa(i) }

	cluster := partitest.StartCluster(t, partitest.WithClusterSize(5))
	nc := cluster.Conn
	nameToIdx := map[string]int{}
	for i, s := range cluster.Servers {
		nameToIdx[s.Name()] = i
	}

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	partitest.CreateStream(t, nc, partitest.StreamSpec{
		Name:     streamName,
		Subjects: []string{"gap.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 5,
	})

	// App-observable signals.
	var mu sync.Mutex
	counts := map[string]int{}
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		mu.Lock()
		counts[msg.Subject()]++
		mu.Unlock()
		return nil
	})
	countFor := func(subj string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[subj]
	}

	var permanentFailures atomic.Int64
	var firesMu sync.Mutex
	fires := map[string]int{} // subject -> unservable-notification count

	dyn, err := consumer.NewDynamic(
		js, streamName, prefix, subjectTpl, handler,
		consumer.WithConsumerReplicas(3),
		consumer.WithConsumerMemoryStorage(true),
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
		// OnPermanentFailure must NOT fire for an unservable (not-exhausted) consumer.
		consumer.WithOnPermanentFailure(func(_ string, _ error) {
			permanentFailures.Add(1)
		}),
		// The unservable-notification seam: parti tells the app a consumer needs
		// operator action. Small window so the test does not wait the 10s default.
		consumer.WithConsumerUnservableThreshold(2*time.Second),
		consumer.WithOnConsumerUnservable(func(subject string, _ error) {
			firesMu.Lock()
			fires[subject]++
			firesMu.Unlock()
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dyn.Stop(context.Background()) })

	cfg := testutil.IntegrationTestConfig()
	cfg.KVBuckets.Replicas = 5

	wc := testutil.NewWorkerClusterWithSource(t, nc,
		source.NewStatic(testutil.CreateTestPartitions(numParts)), cfg)
	wc.AddWorkerWithOptions(ctx, parti.WithWorkerConsumerUpdater(dyn))
	wc.StartWorkers(ctx)
	defer wc.StopWorkers()
	mgr := wc.Workers[0]

	require.Eventually(t, func() bool {
		return len(listConsumerInfos(t, ctx, js, streamName)) == numParts
	}, 20*time.Second, 200*time.Millisecond)

	// Baseline: everything flows.
	for i := 0; i < numParts; i++ {
		_, perr := js.Publish(ctx, subjectFor(i), []byte("baseline"))
		require.NoError(t, perr)
	}
	for i := 0; i < numParts; i++ {
		subj := subjectFor(i)
		require.Eventually(t, func() bool { return countFor(subj) >= 1 },
			15*time.Second, 100*time.Millisecond, "baseline not consumed on %s", subj)
	}

	// Choose the 2-node kill that strips quorum from the most consumers.
	infos := listConsumerInfos(t, ctx, js, streamName)
	peersBySubject := map[string][]int{}
	for _, ci := range infos {
		peersBySubject[ci.Config.FilterSubject] = peerIndices(ci, nameToIdx)
	}
	killA, killB, affectedCount := bestKillPair(peersBySubject, len(cluster.Servers))
	require.Greater(t, affectedCount, 0)

	var affSubj string
	for subj, peers := range peersBySubject {
		if countIn(peers, killA, killB) >= 2 {
			affSubj = subj
			break
		}
	}
	require.NotEmpty(t, affSubj)
	t.Logf("killing node%d+node%d; tracking unservable consumer on %s", killA, killB, affSubj)

	cluster.Servers[killA].Shutdown()
	cluster.Servers[killB].Shutdown()
	cluster.Servers[killA].WaitForShutdown()
	cluster.Servers[killB].WaitForShutdown()

	// OBSERVABLE 1 — delivery on the affected partition stalls.
	stallBase := countFor(affSubj)
	publishWithRetry(t, ctx, js, affSubj, []byte("during-outage")) // stream RF5 still has quorum
	require.Never(t, func() bool { return countFor(affSubj) > stallBase },
		10*time.Second, 500*time.Millisecond, "affected partition %s should stall (unservable)", affSubj)

	// The consumer is unservable while the NATS CONNECTION IS UP (the client
	// reconnects to a surviving node) — this is exactly the detection predicate
	// the future feature keys on: "exists but unservable, connection up".
	require.Equal(t, nats.CONNECTED, nc.Status(),
		"connection must be UP during the stall (consumer-level, not connectivity, failure)")

	// OBSERVABLE 2 — the app IS notified that this consumer needs operator action.
	// (Previously the gap: nothing fired. Now WithOnConsumerUnservable fires.)
	firedFor := func(subj string) int {
		firesMu.Lock()
		defer firesMu.Unlock()

		return fires[subj]
	}
	require.Eventually(t, func() bool { return firedFor(affSubj) > 0 },
		30*time.Second, 250*time.Millisecond,
		"app must be notified (OnConsumerUnservable) that %s is unservable", affSubj)

	// The unservable signal is the RIGHT signal — not the terminal OnPermanentFailure
	// (this is not iterator-creation exhaustion) and not a manager Degraded transition
	// (RF5 KV survived 2-of-5, so the manager stays healthy).
	require.Equal(t, int64(0), permanentFailures.Load(),
		"OnPermanentFailure must not fire for an unservable (not-iterator-exhausted) consumer")
	require.NotEqual(t, types.StateDegraded, mgr.State(),
		"manager stays healthy on partial loss; the per-consumer signal owns this case")

	t.Logf("NOTIFIED: app received OnConsumerUnservable for %s (fires=%d) while connection UP and manager %v",
		affSubj, firedFor(affSubj), mgr.State())
}
