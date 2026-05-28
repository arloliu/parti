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

// TestCluster_NodePVCLoss_SurvivesTransparently covers the "file storage / PVC
// broken on one node" corner case: a single node loses its persistent volume and
// restarts empty. With an RF5 FileStorage stream + RF5 KV on a 5-node cluster,
// quorum (3 of 5) holds throughout, so the wiped node re-replicates from peers and
// parti keeps working with no degradation and no operator intervention.
func TestCluster_NodePVCLoss_SurvivesTransparently(t *testing.T) {
	requireClusterCrashTests(t)

	ctx := context.Background()
	const (
		streamName = "PVC"
		prefix     = "pvc"
		subjectTpl = "pvc.{{.PartitionID}}"
		numParts   = 4
	)
	subjectFor := func(i int) string { return "pvc.partition-" + strconv.Itoa(i) }

	cluster := partitest.StartCluster(t, partitest.WithClusterSize(5))
	nc := cluster.Conn

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	partitest.CreateStream(t, nc, partitest.StreamSpec{
		Name:     streamName,
		Subjects: []string{"pvc.>"},
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

	require.Eventually(t, func() bool {
		return len(listConsumerInfos(t, ctx, js, streamName)) == numParts
	}, 20*time.Second, 200*time.Millisecond)

	// Baseline.
	for i := 0; i < numParts; i++ {
		_, perr := js.Publish(ctx, subjectFor(i), []byte("baseline"))
		require.NoError(t, perr)
	}
	for i := 0; i < numParts; i++ {
		subj := subjectFor(i)
		require.Eventually(t, func() bool { return countFor(subj) >= 1 },
			15*time.Second, 100*time.Millisecond, "baseline not consumed on %s", subj)
	}

	// Simulate PVC loss on one node: shut it down and restart with an empty
	// StoreDir. Quorum (3 of 5) is never lost.
	const victim = 1
	cluster.Servers[victim].Shutdown()
	cluster.Servers[victim].WaitForShutdown()
	cluster.RestartNodeWiped(victim)
	t.Logf("node %d restarted with wiped StoreDir (PVC loss)", victim)

	// The worker must not degrade — quorum held throughout.
	require.Never(t, func() bool { return mgr.State() == types.StateDegraded },
		3*time.Second, 250*time.Millisecond, "worker should not degrade on single-node PVC loss")

	// Parti keeps working: publish to every partition and confirm consumption
	// continues after the node rejoins and re-replicates.
	base := make([]int, numParts)
	for i := 0; i < numParts; i++ {
		base[i] = countFor(subjectFor(i))
	}
	for i := 0; i < numParts; i++ {
		subj := subjectFor(i)
		want := base[i]
		require.Eventually(t, func() bool {
			_, _ = js.Publish(ctx, subj, []byte("post-pvc"))
			return countFor(subj) > want
		}, 25*time.Second, 500*time.Millisecond, "partition %s should keep consuming after PVC loss", subj)
	}
	t.Log("confirmed: parti survived single-node PVC loss transparently")
}
