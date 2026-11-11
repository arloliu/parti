package assignment_test

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
)

// TestRejoinAfterAbsorbedLeave_GetsPartitions verifies that a worker which stops
// and comes back under the same worker ID is assigned partitions again
// when a partition update rebalanced its departure in between.
//
// The partition update publishes an assignment without the departed worker.
// It is not a worker-change rebalance,
// so the leader's record of the last worker-change set still lists that worker.
// The rejoin then reproduces that record exactly.
// A leader that compares only against it sees no change
// and leaves the rejoined worker without partitions.
func TestRejoinAfterAbsorbedLeave_GetsPartitions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	cfg.WorkerIDPrefix = "rejoin"
	cfg.WorkerIDMax = 9
	// A long emergency grace keeps the departure pending,
	// so the only rebalance that publishes it is the one caused by the partition update.
	cfg.WorkerIDTTL = 30 * time.Second
	cfg.HeartbeatTTL = 30 * time.Second
	cfg.EmergencyGracePeriod = 30 * time.Second

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "parti-source-rejoin"})
	require.NoError(t, err)
	// Every worker gets its own source on the shared bucket, as separate processes would:
	// a Manager stops its source when it stops, which must not blind the other workers.
	newSource := func() *source.NatsKV {
		src := source.NewNatsKV(kv, "config", nil)
		require.NoError(t, src.Start(ctx))
		t.Cleanup(func() { _ = src.Stop(context.Background()) })

		return src
	}
	writer := source.NewNatsKV(kv, "config", nil)
	require.NoError(t, writer.Update(ctx, testutil.CreateTestPartitions(12)))

	cluster := testutil.NewWorkerClusterWithSource(t, nc, newSource(), cfg)
	t.Cleanup(cluster.StopWorkers)
	addWorker := func() *parti.Manager {
		cluster.Source = newSource()

		return cluster.AddWorkerWithOptions(ctx)
	}
	for range 3 {
		addWorker()
	}
	cluster.StartWorkers(ctx)
	leader := cluster.WaitForLeader(10 * time.Second)
	require.True(t, cluster.WaitForAllWorkersHavePartitions(20*time.Second), "initial assignment did not reach every worker")

	var leaving *parti.Manager
	var staying []*parti.Manager
	for _, mgr := range cluster.GetWorkers() {
		if leaving == nil && mgr != leader {
			leaving = mgr
			continue
		}
		staying = append(staying, mgr)
	}
	require.NotNil(t, leaving)
	leavingID := leaving.WorkerID()

	stopCtx, stopCancel := context.WithTimeout(ctx, 5*time.Second)
	require.NoError(t, leaving.Stop(stopCtx))
	stopCancel()

	// The partition update is what rebalances the departure.
	updated := testutil.CreateTestPartitions(13)
	require.NoError(t, writer.Update(ctx, updated))
	require.Eventually(t, func() bool {
		return ownedBy(staying) == len(updated)
	}, 20*time.Second, 100*time.Millisecond,
		"precondition: the partition update must publish an assignment that leaves out the departed worker")

	rejoined := addWorker()
	require.NoError(t, rejoined.Start(ctx))
	require.Equal(t, leavingID, rejoined.WorkerID(), "precondition: the new worker must take over the released worker ID")

	require.Eventually(t, func() bool {
		return len(rejoined.CurrentAssignment().Partitions) > 0
	}, 20*time.Second, 100*time.Millisecond, "the rejoined worker must be assigned partitions")
}

// ownedBy returns the number of distinct partitions assigned across workers.
func ownedBy(workers []*parti.Manager) int {
	seen := make(map[string]struct{})
	for _, mgr := range workers {
		for _, p := range mgr.CurrentAssignment().Partitions {
			seen[types.Partition{Keys: p.Keys}.SubjectKey()] = struct{}{}
		}
	}

	return len(seen)
}
