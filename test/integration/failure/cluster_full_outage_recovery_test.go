package failure_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
)

// TestCluster_FullOutage_AutoHeal exercises the full-quorum-loss + recovery path:
// with three of five nodes down, JetStream loses meta quorum so parti cannot make
// progress; restarting the three nodes (FileStorage state survives via the
// Cluster handle's RestartNode) must let parti auto-heal back to a working state.
//
// Topology: RF5 FileStorage stream, RF5 KV buckets, single worker. Killing 3 of 5
// drops below quorum (3 of 5) for every R5 asset.
//
// Assertions: during the outage publishing fails and the manager enters Degraded
// (KV renewals fail, the leadership lease cannot be held); after the nodes return
// the manager auto-heals to StateStable and publishing is restored. Asserting the
// Degraded entry keeps the auto-heal assertion from passing vacuously.
func TestCluster_FullOutage_AutoHeal(t *testing.T) {
	requireClusterCrashTests(t)

	ctx := context.Background()
	const (
		streamName = "OUTAGE"
		prefix     = "outage"
		subjectTpl = "outage.{{.PartitionID}}"
		numParts   = 4
	)
	subjectFor := func(i int) string { return "outage.partition-" + strconv.Itoa(i) }

	cluster := partitest.StartCluster(t, partitest.WithClusterSize(5))
	nc := cluster.Conn

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	partitest.CreateStream(t, nc, partitest.StreamSpec{
		Name:     streamName,
		Subjects: []string{"outage.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 5,
	})

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

	dyn, err := consumer.NewDynamic(
		js, streamName, prefix, subjectTpl, handler,
		consumer.WithConsumerReplicas(3),
		consumer.WithConsumerMemoryStorage(true),
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
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

	// Baseline: a message flows on partition 0.
	require.Eventually(t, func() bool {
		return len(listConsumerInfos(t, ctx, js, streamName)) == numParts
	}, 20*time.Second, 200*time.Millisecond)
	_, err = js.Publish(ctx, subjectFor(0), []byte("baseline"))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return countFor(subjectFor(0)) >= 1 },
		15*time.Second, 100*time.Millisecond, "baseline not consumed")

	// Kill 3 of 5 nodes (indices 2,3,4) → meta quorum lost.
	t.Log("killing nodes 2, 3, 4 (quorum lost)")
	for _, i := range []int{2, 3, 4} {
		cluster.Servers[i].Shutdown()
	}
	for _, i := range []int{2, 3, 4} {
		cluster.Servers[i].WaitForShutdown()
	}

	// During the outage parti cannot make progress: a stream publish must fail
	// (the R5 stream has only 2 of 5 nodes, below quorum).
	require.Eventually(t, func() bool {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, perr := js.Publish(pctx, subjectFor(1), []byte("during-outage"))
		cancel()
		return perr != nil
	}, 20*time.Second, 500*time.Millisecond, "publish should fail while quorum is lost")
	t.Log("confirmed: publish fails during outage")

	// The manager must enter degraded mode while quorum is lost (KV renewals fail,
	// leadership lease cannot be held). Asserting this makes the later auto-heal
	// assertion self-validating rather than vacuously true.
	require.Eventually(t, func() bool { return mgr.State() == types.StateDegraded },
		30*time.Second, 250*time.Millisecond,
		"manager should enter Degraded during full quorum loss (was %v)", mgr.State())
	t.Log("confirmed: manager entered Degraded during outage")

	// Bring the 3 nodes back in place (same ports + StoreDir → FileStorage survives).
	t.Log("restarting nodes 2, 3, 4")
	for _, i := range []int{2, 3, 4} {
		cluster.RestartNode(i)
	}

	// Auto-heal: the manager must return to StateStable once quorum is restored.
	require.Eventually(t, func() bool { return mgr.State() == types.StateStable },
		60*time.Second, 250*time.Millisecond,
		"manager should auto-heal to StateStable after nodes return (was %v)", mgr.State())
	t.Log("confirmed: manager auto-healed to StateStable")

	// Back to work: publishing to the stream succeeds again (quorum restored).
	require.Eventually(t, func() bool {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, perr := js.Publish(pctx, subjectFor(0), []byte("post-recovery"))
		cancel()
		return perr == nil
	}, 30*time.Second, 500*time.Millisecond, "publish should succeed after recovery")
	t.Log("confirmed: publish succeeds after recovery — parti is back to work")
}
