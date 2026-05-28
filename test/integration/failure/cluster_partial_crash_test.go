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

// TestCluster_PartialCrash_QuorumLoss_Characterization pins the CURRENT behavior
// when two of five NATS nodes crash under a memory-RF3 dynamic-consumer topology
// on an RF5 FileStorage stream.
//
// Spike finding (tmp/cluster-spike-findings.md): a memory-RF3 consumer that loses
// raft quorum surfaces 503 / NoResponders — NEVER ConsumerNotFound — so parti's
// recovery treats it as transient and does NOT auto-recreate it. This test
// documents that gap and, separately, proves the ONLY recreate path that works
// today: explicit consumer deletion (ErrConsumerDeleted) DOES trigger recreate.
//
// Topology choices:
//   - Stream: RF5 FileStorage (survives 2-node loss; quorum = 3 of 5).
//   - KVBuckets.Replicas = 5 (manager KV survives 2-node loss; worker stays
//     healthy — this isolates "some consumers break" from "whole worker degrades").
//   - Dynamic consumers: RF3 MemoryStorage (each raft group lands on 3 of 5
//     nodes, so a well-chosen 2-node kill strips quorum from a subset).
//   - Single worker owning all partitions, so all per-partition consumers exist
//     and their placement is directly observable.
func TestCluster_PartialCrash_QuorumLoss_Characterization(t *testing.T) {
	requireClusterCrashTests(t)

	ctx := context.Background()
	const (
		streamName = "CRASH1"
		prefix     = "crash1"
		subjectTpl = "crash1.{{.PartitionID}}"
		numParts   = 8
	)

	cluster := partitest.StartCluster(t, partitest.WithClusterSize(5))
	nc := cluster.Conn

	nameToIdx := map[string]int{}
	for i, s := range cluster.Servers {
		nameToIdx[s.Name()] = i
	}

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// RF5 FileStorage stream.
	partitest.CreateStream(t, nc, partitest.StreamSpec{
		Name:     streamName,
		Subjects: []string{"crash1.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 5,
	})

	// Per-subject delivery counters.
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
		consumer.WithRecoveryRetry(consumer.RecoveryRetryConfig{
			MaxAttempts: 5,
			BaseBackoff: 100 * time.Millisecond,
			MaxBackoff:  500 * time.Millisecond,
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dyn.Stop(context.Background()) })

	cfg := testutil.IntegrationTestConfig()
	cfg.KVBuckets.Replicas = 5

	wc := testutil.NewWorkerClusterWithSource(t, nc,
		source.NewStatic(testutil.CreateTestPartitions(numParts)), cfg)
	wc.AddWorkerWithOptions(ctx, parti.WithWorkerConsumerUpdater(dyn))
	wc.StartWorkers(ctx) // blocks until StateStable
	defer wc.StopWorkers()
	mgr := wc.Workers[0]

	// Wait until all per-partition consumers exist.
	require.Eventually(t, func() bool {
		return len(listConsumerInfos(t, ctx, js, streamName)) == numParts
	}, 20*time.Second, 200*time.Millisecond, "expected %d per-partition consumers", numParts)

	// Baseline: every partition's consumer is functional.
	for i := 0; i < numParts; i++ {
		subj := subjectFor(i)
		_, perr := js.Publish(ctx, subj, []byte("baseline"))
		require.NoError(t, perr)
	}
	for i := 0; i < numParts; i++ {
		subj := subjectFor(i)
		require.Eventually(t, func() bool { return countFor(subj) >= 1 },
			15*time.Second, 100*time.Millisecond, "baseline not consumed on %s", subj)
	}

	// Snapshot placement and pick the 2-node kill that breaks the most consumers.
	infos := listConsumerInfos(t, ctx, js, streamName)
	peersBySubject := map[string][]int{}
	nameBySubject := map[string]string{}
	for _, ci := range infos {
		subj := ci.Config.FilterSubject
		peersBySubject[subj] = peerIndices(ci, nameToIdx)
		nameBySubject[subj] = ci.Name
		t.Logf("consumer %s subj=%s peers=%v", ci.Name, subj, peersBySubject[subj])
	}

	killA, killB, affectedCount := bestKillPair(peersBySubject, len(cluster.Servers))
	require.Greater(t, affectedCount, 0, "no consumer loses quorum for any node pair (placement too even)")
	require.Less(t, affectedCount, numParts, "every consumer breaks; need some survivors to compare")
	t.Logf("killing node%d + node%d, expecting %d/%d consumers to lose quorum", killA, killB, affectedCount, numParts)

	affected := map[string]bool{}
	for subj, peers := range peersBySubject {
		affected[subj] = countIn(peers, killA, killB) >= 2
	}

	cluster.Servers[killA].Shutdown()
	cluster.Servers[killB].Shutdown()
	cluster.Servers[killA].WaitForShutdown()
	cluster.Servers[killB].WaitForShutdown()

	// The worker's KV is RF5 (quorum = 3 of 5 alive) so it must stay healthy.
	require.Never(t, func() bool { return mgr.State() == types.StateDegraded },
		3*time.Second, 250*time.Millisecond, "worker should NOT degrade on 2-of-5 loss with RF5 KV")

	// Publish a post-crash message to every partition.
	postBase := map[string]int{}
	for i := 0; i < numParts; i++ {
		subj := subjectFor(i)
		postBase[subj] = countFor(subj)
		publishWithRetry(t, ctx, js, subj, []byte("post-crash")) // stream is RF5, still has quorum
	}

	// Unaffected consumers keep flowing; affected ones stall (no recreate).
	var stalledSubj, stalledName string
	for i := 0; i < numParts; i++ {
		subj := subjectFor(i)
		if affected[subj] {
			if stalledSubj == "" {
				stalledSubj = subj
				stalledName = nameBySubject[subj]
			}
			continue
		}
		require.Eventually(t, func() bool { return countFor(subj) > postBase[subj] },
			15*time.Second, 100*time.Millisecond, "unaffected partition %s should keep consuming", subj)
	}

	// Characterize the gap: the affected consumer does NOT auto-recreate, so its
	// post-crash message is never consumed within a generous window.
	require.Never(t, func() bool { return countFor(stalledSubj) > postBase[stalledSubj] },
		8*time.Second, 500*time.Millisecond,
		"affected partition %s unexpectedly recovered without recreate (spike said World B)", stalledSubj)
	t.Logf("confirmed gap: quorum-lost consumer %s (%s) stalled, parti did not auto-recreate", stalledName, stalledSubj)

	// The recreate-on-delete recovery path DOES work for a consumer that still has
	// quorum. Delete a healthy (unaffected) consumer and assert parti detects the
	// deletion (ErrConsumerDeleted) and recreates it, resuming delivery. 3 nodes
	// remain alive, so an RF3 consumer can be re-placed.
	var healthySubj, healthyName string
	for subj := range affected {
		if !affected[subj] {
			healthySubj, healthyName = subj, nameBySubject[subj]
			break
		}
	}
	require.NotEmpty(t, healthyName, "need at least one healthy consumer to delete")

	healthyBase := countFor(healthySubj)
	require.NoError(t, js.DeleteConsumer(ctx, streamName, healthyName))
	// Publish on each poll: the recreated consumer uses DeliverNew (RecoverFromNew),
	// so only a message published AFTER the new consumer binds is delivered. Polling
	// the publish avoids racing the recreate.
	require.Eventually(t, func() bool {
		_, _ = js.Publish(ctx, healthySubj, []byte("after-delete"))
		return countFor(healthySubj) > healthyBase
	}, 25*time.Second, 500*time.Millisecond,
		"parti should recreate the explicitly-deleted healthy consumer and resume %s", healthySubj)
	t.Logf("confirmed recreate-on-delete path: healthy consumer %s resumed after explicit DeleteConsumer", healthySubj)
}

// --- helpers ---

func subjectFor(partition int) string {
	return "crash1.partition-" + strconv.Itoa(partition)
}

// tryListConsumerInfos lists a stream's consumers, returning any transient error
// (stream lookup or list timeout while the cluster re-elects / resyncs after a
// node restart) instead of failing, so callers can retry.
func tryListConsumerInfos(ctx context.Context, js jetstream.JetStream, stream string) ([]*jetstream.ConsumerInfo, error) {
	s, err := js.Stream(ctx, stream)
	if err != nil {
		return nil, err
	}
	lister := s.ListConsumers(ctx)
	var out []*jetstream.ConsumerInfo
	for ci := range lister.Info() {
		out = append(out, ci)
	}
	if err := lister.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

// listConsumerInfos returns the current consumer infos. On a transient error
// (cluster mid-resync) it logs and returns nil so callers polling inside a
// require.Eventually naturally retry rather than hard-failing on a blip.
func listConsumerInfos(t *testing.T, ctx context.Context, js jetstream.JetStream, stream string) []*jetstream.ConsumerInfo {
	t.Helper()
	infos, err := tryListConsumerInfos(ctx, js, stream)
	if err != nil {
		t.Logf("listConsumerInfos transient error (returning nil; retry if polled): %v", err)
		return nil
	}

	return infos
}

// publishWithRetry publishes to subj, retrying the transient errors ("no response
// from stream", context deadline) that occur while the stream's raft group
// re-elects a leader after nodes are killed. The RF5 stream retains quorum, so
// the publish genuinely succeeds once re-election settles (typically <5s).
func publishWithRetry(t *testing.T, ctx context.Context, js jetstream.JetStream, subj string, data []byte) {
	t.Helper()
	require.Eventually(t, func() bool {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, err := js.Publish(pctx, subj, data)

		return err == nil
	}, 20*time.Second, 250*time.Millisecond, "publish to %s should succeed once the RF5 stream re-elects", subj)
}

// peerIndices returns the node indices hosting a consumer's raft peers
// (leader + replicas).
func peerIndices(ci *jetstream.ConsumerInfo, nameToIdx map[string]int) []int {
	if ci.Cluster == nil {
		return nil
	}
	var p []int
	if ci.Cluster.Leader != "" {
		p = append(p, nameToIdx[ci.Cluster.Leader])
	}
	for _, r := range ci.Cluster.Replicas {
		p = append(p, nameToIdx[r.Name])
	}

	return p
}

func countIn(peers []int, a, b int) int {
	c := 0
	for _, p := range peers {
		if p == a || p == b {
			c++
		}
	}
	return c
}

// bestKillPair finds the node pair whose shutdown strips quorum (>=2 of 3 peers)
// from the most consumers.
func bestKillPair(peersBySubject map[string][]int, nodeCount int) (killA, killB, affected int) {
	killA, killB, affected = 0, 1, -1
	for a := 0; a < nodeCount; a++ {
		for b := a + 1; b < nodeCount; b++ {
			n := 0
			for _, peers := range peersBySubject {
				if countIn(peers, a, b) >= 2 {
					n++
				}
			}
			if n > affected {
				affected, killA, killB = n, a, b
			}
		}
	}

	return killA, killB, affected
}
