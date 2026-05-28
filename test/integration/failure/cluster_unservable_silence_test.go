package failure_test

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
)

// TestCluster_TemporalBlip_DoesNotNotify is the false-positive guard for the
// unservable notification: a brief quorum blip (nodes crash then come right back,
// the kind a normal leader election or rolling restart produces) must NOT fire
// WithOnConsumerUnservable, because parti auto-heals it. Only sustained
// unservability (past the window) is operator-actionable.
//
// The window is set to 8s; the crashed nodes are restarted within ~1s, so the
// unservable condition never persists past the window. Delivery resumes and the
// app is never paged.
func TestCluster_TemporalBlip_DoesNotNotify(t *testing.T) {
	requireClusterCrashTests(t)

	ctx := context.Background()
	const (
		streamName = "BLIP"
		prefix     = "blip"
		subjectTpl = "blip.{{.PartitionID}}"
		numParts   = 8
	)
	subjectFor := func(i int) string { return "blip.partition-" + strconv.Itoa(i) }

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
		Subjects: []string{"blip.>"},
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

	var unservableFires atomic.Int64
	dyn, err := consumer.NewDynamic(
		js, streamName, prefix, subjectTpl, handler,
		consumer.WithConsumerReplicas(3),
		consumer.WithConsumerMemoryStorage(true),
		consumer.WithRecoveryStrategy(consumer.RecoverFromNew),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
		// Window generous enough that node restart + raft resync (even under heavy
		// CI load) completes well within it, so a brief blip never crosses it.
		consumer.WithConsumerUnservableThreshold(20*time.Second),
		consumer.WithOnConsumerUnservable(func(_ string, _ error) { unservableFires.Add(1) }),
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

	require.Eventually(t, func() bool {
		return len(listConsumerInfos(t, ctx, js, streamName)) == numParts
	}, 20*time.Second, 200*time.Millisecond)

	for i := 0; i < numParts; i++ {
		_, perr := js.Publish(ctx, subjectFor(i), []byte("baseline"))
		require.NoError(t, perr)
	}
	for i := 0; i < numParts; i++ {
		subj := subjectFor(i)
		require.Eventually(t, func() bool { return countFor(subj) >= 1 },
			15*time.Second, 100*time.Millisecond, "baseline not consumed on %s", subj)
	}

	// Pick a pair to kill, note an affected subject, then bring the nodes RIGHT
	// back — a transient blip well under the 8s unservable window.
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

	cluster.Servers[killA].Shutdown()
	cluster.Servers[killB].Shutdown()
	cluster.Servers[killA].WaitForShutdown()
	cluster.Servers[killB].WaitForShutdown()
	// Restore immediately — a brief blip, far under the 20s unservable window.
	cluster.RestartNode(killA)
	cluster.RestartNode(killB)
	t.Logf("blip: killed+restored node%d+node%d (window=20s)", killA, killB)

	// The (briefly) affected partition resumes delivery...
	base := countFor(affSubj)
	require.Eventually(t, func() bool {
		_, _ = js.Publish(ctx, affSubj, []byte("post-blip"))
		return countFor(affSubj) > base
	}, 45*time.Second, 500*time.Millisecond, "partition %s should resume after the blip", affSubj)

	// ...and the app was NEVER paged: the blip did not cross the unservable window.
	require.Equal(t, int64(0), unservableFires.Load(),
		"a transient blip must NOT fire OnConsumerUnservable (false-positive guard)")
	t.Log("confirmed: transient blip auto-healed silently; app not paged")
}
