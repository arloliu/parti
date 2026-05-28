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
)

// TestCluster_PartialCrash_QuorumRestored_Resumes answers: once a quorum-lost
// memory-RF3 consumer regains quorum (the nodes that hosted its peers come back),
// does parti resume on the SAME consumer without a recreate?
//
// Hypothesis (verified by this test): yes. The dynamic-consumer iterator treats
// the quorum-loss surface (503/NoResponders) as transient and backs off-and-retries
// indefinitely, so it resumes once the consumer is reachable again. And because a
// memory-RF3 consumer keeps exactly one surviving peer (only 2 of 3 are killed), the
// raft group recovers its state from that peer when the 2 nodes restart empty — so
// the consumer is the same one (unchanged Created timestamp), not a fresh recreate.
func TestCluster_PartialCrash_QuorumRestored_Resumes(t *testing.T) {
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

	partitest.CreateStream(t, nc, partitest.StreamSpec{
		Name:     streamName,
		Subjects: []string{"crash1.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 5,
	})

	var mu sync.Mutex
	counts := map[string]int{}
	// payloadHits[subject][payload] = delivery count, for no-loss / dup tracking.
	payloadHits := map[string]map[string]int{}
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		mu.Lock()
		counts[msg.Subject()]++
		if payloadHits[msg.Subject()] == nil {
			payloadHits[msg.Subject()] = map[string]int{}
		}
		payloadHits[msg.Subject()][string(msg.Data())]++
		mu.Unlock()

		return nil
	})
	countFor := func(subj string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[subj]
	}
	// uniqueDelivered returns how many of the given payloads were delivered at
	// least once, and the max delivery count seen for any one of them.
	uniqueDelivered := func(subj string, payloads []string) (got, maxDup int) {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range payloads {
			if n := payloadHits[subj][p]; n > 0 {
				got++
				if n > maxDup {
					maxDup = n
				}
			}
		}

		return got, maxDup
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

	// Snapshot placement, pick the best 2-node kill, and capture one affected
	// consumer's identity (name + Created) so we can tell resume from recreate.
	infos := listConsumerInfos(t, ctx, js, streamName)
	peersBySubject := map[string][]int{}
	infoBySubject := map[string]*jetstream.ConsumerInfo{}
	for _, ci := range infos {
		peersBySubject[ci.Config.FilterSubject] = peerIndices(ci, nameToIdx)
		infoBySubject[ci.Config.FilterSubject] = ci
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
	origName := infoBySubject[affSubj].Name
	origCreated := infoBySubject[affSubj].Created
	t.Logf("killing node%d+node%d; tracking affected consumer %s (subj=%s)", killA, killB, origName, affSubj)

	// Crash the two nodes.
	cluster.Servers[killA].Shutdown()
	cluster.Servers[killB].Shutdown()
	cluster.Servers[killA].WaitForShutdown()
	cluster.Servers[killB].WaitForShutdown()

	// Confirm the affected consumer stalls: a message published now is not consumed.
	stallBase := countFor(affSubj)
	publishWithRetry(t, ctx, js, affSubj, []byte("during-outage")) // stream is RF5, still has quorum
	require.Never(t, func() bool { return countFor(affSubj) > stallBase },
		6*time.Second, 500*time.Millisecond, "affected consumer %s should stall while quorum is lost", affSubj)
	t.Logf("confirmed stall on %s", affSubj)

	// Bring the two nodes back → the consumer's raft group regains quorum.
	cluster.RestartNode(killA)
	cluster.RestartNode(killB)
	t.Log("restarted both nodes; waiting for the stalled consumer to resume")

	// The iterator should resume on its own. Publishing on each poll guarantees a
	// message lands once delivery is flowing again (covers any deliver-policy nuance).
	resumeBase := countFor(affSubj)
	require.Eventually(t, func() bool {
		_, _ = js.Publish(ctx, affSubj, []byte("post-recovery"))
		return countFor(affSubj) > resumeBase
	}, 40*time.Second, 500*time.Millisecond,
		"parti should resume consuming %s after quorum is restored", affSubj)
	t.Logf("confirmed resume on %s after quorum restored", affSubj)

	// Was it the SAME consumer (resumed) or a fresh recreate? Compare identity.
	// Retry the lookup: right after node restart the stream/consumer metadata can
	// transiently time out while the restarted nodes resync.
	var found *jetstream.ConsumerInfo
	require.Eventually(t, func() bool {
		cur, lerr := tryListConsumerInfos(ctx, js, streamName)
		if lerr != nil {
			return false
		}
		for _, ci := range cur {
			if ci.Config.FilterSubject == affSubj {
				found = ci
				return true
			}
		}

		return false
	}, 20*time.Second, 250*time.Millisecond, "consumer for %s should be listable after recovery", affSubj)
	require.NotNil(t, found, "consumer for %s should exist after recovery", affSubj)
	if found.Name == origName && found.Created.Equal(origCreated) {
		t.Logf("RESULT: parti resumed on the SAME consumer %s (Created unchanged) — no recreate", origName)
	} else {
		t.Logf("RESULT: consumer was recreated: name %s->%s, created %v->%v",
			origName, found.Name, origCreated, found.Created)
	}
	require.Equal(t, origName, found.Name, "expected the same consumer name after quorum restored")
	require.True(t, found.Created.Equal(origCreated),
		"expected the same consumer (unchanged Created) — resume, not recreate")

	// Message-delivery correctness after recovery: publish a batch of uniquely
	// identified messages and require every one to be delivered (no loss). Because
	// recovery is "resume the same consumer" — not a parallel recreate — there is
	// no second consumer racing on the subject, so gross duplication must not occur.
	const batch = 20
	payloads := make([]string, batch)
	for i := 0; i < batch; i++ {
		payloads[i] = "recovered-" + strconv.Itoa(i)
		_, perr := js.Publish(ctx, affSubj, []byte(payloads[i]))
		require.NoError(t, perr)
	}
	require.Eventually(t, func() bool {
		got, _ := uniqueDelivered(affSubj, payloads)
		return got == batch
	}, 30*time.Second, 200*time.Millisecond, "all %d post-recovery messages must be delivered (no loss)", batch)

	_, maxDup := uniqueDelivered(affSubj, payloads)
	require.LessOrEqual(t, maxDup, 2,
		"no gross duplication after resume (at-least-once allows rare redelivery, not a parallel consumer); maxDup=%d", maxDup)
	t.Logf("delivery correctness after resume: all %d unique messages delivered, maxDup=%d", batch, maxDup)
}
