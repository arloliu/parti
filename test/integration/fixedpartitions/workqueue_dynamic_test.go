// This file is the empirical answer to an open question raised while drafting the
// retention-policy guidance for docs/CONSUMERS.md (see
// docs/plans/docs-operational-sync/00-sync-plan.md, item B7):
//
//	Is consumer.Dynamic actually usable over a WorkQueuePolicy stream?
//
// A codex judgment pass argued it is "mechanically possible (single-filter rebind,
// no overlap)" — i.e. because Dynamic keeps ONE durable per partition-subject and
// rebinds it on handoff, it never creates the transient OVERLAPPING filter that
// WorkQueuePolicy rejects with err_code=10100. But the partition-scaling integration
// proofs (Exp11/Exp12 in fixedpartitions_test.go) ALL ran on LimitsPolicy, so the
// WorkQueue claim was UNPROVEN. This test settles it.
//
// It is Exp11 scenario 1 (single-node graceful join+leave) with exactly ONE change —
// the stream is WorkQueuePolicy instead of LimitsPolicy — and it asserts three things:
//
//  1. Dynamic creates its per-subject durables on a WorkQueue stream at all
//     (no DeliverPolicy / retention rejection at Update time).
//  2. A worker join+leave handoff (which forces ~1/3 of slots to rebind to a
//     different worker) does NOT trip err_code=10100 — captured via an OnError hook,
//     not merely inferred from downstream loss.
//  3. Delivery stays LOSSLESS across the churn (WorkQueue's delete-on-ack + same-durable
//     rebind hands the unacked backlog to the gaining worker).
//
// A 10100 on handoff (assertion 2) or any lost message (assertion 3) would REFUTE the
// "mechanically possible" claim and prove WorkQueue is unsafe for Dynamic.
package fixedpartitions_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// errSink collects manager OnError callbacks so the test can assert on their
// content (specifically: did any handoff produce an overlapping-filter / 10100
// error?) rather than only observing the downstream delivery effect.
type errSink struct {
	mu   sync.Mutex
	errs []string
}

func (s *errSink) hook() *parti.Hooks {
	return &parti.Hooks{
		OnError: func(_ context.Context, err error) error {
			if err != nil {
				s.mu.Lock()
				s.errs = append(s.errs, err.Error())
				s.mu.Unlock()
			}
			return nil
		},
	}
}

// overlapErrs returns the captured errors that look like a WorkQueuePolicy
// overlapping-filter rejection (NATS err_code=10100 /
// "filter subject already in use" / "overlap").
func (s *errSink) overlapErrs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.errs {
		le := strings.ToLower(e)
		if strings.Contains(e, "10100") ||
			strings.Contains(le, "overlap") ||
			strings.Contains(le, "filter subject already in use") ||
			(strings.Contains(le, "work queue") && strings.Contains(le, "filter")) {
			out = append(out, e)
		}
	}

	return out
}

func (s *errSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.errs)
}

func TestDynamic_OnWorkQueueStream(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}
	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// THE single difference vs Exp11 scenario 1: WorkQueuePolicy, not LimitsPolicy.
	// File storage so delete-on-ack is exercised against a real store.
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name: vpStream, Subjects: []string{"work.*"},
		Storage: jetstream.FileStorage, Retention: jetstream.WorkQueuePolicy,
	})
	require.NoError(t, err)

	ledger := newLedger()
	sink := &errSink{}
	src := source.NewStatic(vpPartitions())
	cfg := parti.TestConfig()

	newWorker := func() *parti.Manager {
		// Default recovery (disabled): WorkQueue restricts recovery to
		// Beginning/Disabled, and recovery is not what this test probes.
		c, err := consumer.NewDynamic(js, vpStream, "worker", "work.{{.PartitionID}}", ledger.handler())
		require.NoError(t, err)
		m, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(),
			parti.WithWorkerConsumerUpdater(c),
			parti.WithHooks(sink.hook()),
		)
		require.NoError(t, err)
		require.NoError(t, m.Start(ctx))

		return m
	}
	waitStable := func(m *parti.Manager) { require.NoError(t, <-m.WaitState(parti.StateStable, 30*time.Second)) }

	m1 := newWorker()
	m2 := newWorker()
	waitStable(m1)
	waitStable(m2)

	prod := startProducer(js, vpK, 8*time.Millisecond)
	time.Sleep(1 * time.Second)

	m3 := newWorker() // JOIN 2->3: ConsistentHash rebinds ~1/3 of the slots onto a new worker.
	time.Sleep(2 * time.Second)
	waitStable(m1)
	waitStable(m3)
	time.Sleep(1 * time.Second)

	require.NoError(t, m2.Stop(context.Background())) // LEAVE (graceful handoff back onto m1/m3).
	time.Sleep(2 * time.Second)
	waitStable(m1)
	waitStable(m3)
	time.Sleep(1 * time.Second)

	published := prod.stopAndWait()
	t.Cleanup(func() { _ = m1.Stop(context.Background()); _ = m3.Stop(context.Background()) })

	// Assertion 2: no overlapping-filter / 10100 rejection occurred on any handoff.
	overlaps := sink.overlapErrs()
	require.Empty(t, overlaps,
		"WorkQueue+Dynamic handoff produced an overlapping-filter/10100 rejection — REFUTES 'single-filter rebind, no overlap': %v", overlaps)

	// Assertions 1 & 3: durables were created and every message was delivered at
	// least once across the churn (delete-on-ack + rebind handed off cleanly).
	assertLossless(t, ledger, published)
	distinct, dups := ledger.stats()
	t.Logf("WorkQueue+Dynamic (single-node join+leave): published=%d delivered=%d dups=%d | manager errors total=%d overlap/10100=%d",
		len(published), distinct, dups, sink.count(), len(overlaps))
}

// TestDynamic_OnWorkQueueStream_ClusterCrash is the harder half of the WorkQueue
// proof: Exp11 scenario 2 (3-node cluster, RF=3 stream + R=3 consumers, abrupt
// worker CRASH) on a WorkQueuePolicy stream instead of LimitsPolicy.
//
// The crash path stresses what the graceful test cannot: a worker dies WITHOUT
// relinquishing, survivors detect it via heartbeat TTL and rebind its slot
// durables from the cluster, and — on WorkQueue specifically — the in-flight,
// unacked messages on those durables must REDELIVER to the rebinding worker
// (WorkQueue retains unacked, deletes only on ack) rather than being lost.
// Consumer state is MemoryStorage + R=3 (the only replica value NATS accepts on a
// WorkQueue RF=3 stream — consumer replicas must equal stream replicas), so the
// durables survive the crashed client and the survivors rebind the same names
// (no overlapping-filter create → no 10100).
//
// Asserts: no 10100 on the TTL-driven reassignment, and lossless delivery across
// the crash (duplicates EXPECTED — a crash redelivers in-flight work).
func TestDynamic_OnWorkQueueStream_ClusterCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}
	ctx := context.Background()
	clusterNC, servers, cleanup := testutil.StartEmbeddedNATSCluster(t)
	defer cleanup()
	require.GreaterOrEqual(t, len(servers), 3, "need a 3-node cluster")

	jsMain, err := jetstream.New(clusterNC)
	require.NoError(t, err)

	// WorkQueuePolicy, RF=3 — the one difference vs Exp11 scenario 2.
	_, err = jsMain.CreateStream(ctx, jetstream.StreamConfig{
		Name: vpStream, Subjects: []string{"work.*"},
		Storage: jetstream.FileStorage, Retention: jetstream.WorkQueuePolicy, Replicas: 3,
	})
	require.NoError(t, err)

	ledger := newLedger()
	sink := &errSink{}
	src := source.NewStatic(vpPartitions())
	cfg := testutil.IntegrationTestConfig() // WorkerIDMax=100, flake-tuned TTLs

	type worker struct {
		m  *parti.Manager
		nc *nats.Conn
	}
	newWorker := func(i int) *worker {
		wnc, err := nats.Connect(servers[i%len(servers)].ClientURL())
		require.NoError(t, err)
		wjs, err := jetstream.New(wnc)
		require.NoError(t, err)
		// Consumer R=3 (must equal stream replicas on WorkQueue) + memory state so the
		// durable survives the crashed client and is rebindable by a survivor.
		c, err := consumer.NewDynamic(wjs, vpStream, "worker", "work.{{.PartitionID}}", ledger.handler(),
			consumer.WithConsumerMemoryStorage(true), consumer.WithConsumerReplicas(3))
		require.NoError(t, err)
		m, err := parti.NewManager(&cfg, wjs, src, strategy.NewConsistentHash(),
			parti.WithWorkerConsumerUpdater(c),
			parti.WithHooks(sink.hook()),
		)
		require.NoError(t, err)
		require.NoError(t, m.Start(ctx))

		return &worker{m: m, nc: wnc}
	}

	w := []*worker{newWorker(0), newWorker(1), newWorker(2)}
	for _, x := range w {
		require.NoError(t, <-x.m.WaitState(parti.StateStable, 40*time.Second))
	}

	prod := startProducer(jsMain, vpK, 12*time.Millisecond)
	time.Sleep(2 * time.Second)

	// CRASH worker 1: kill its connection (no graceful relinquish). Survivors must
	// detect via heartbeat TTL and rebind its slot durables from the cluster.
	w[1].nc.Close()
	t.Log("WorkQueue+Dynamic/cluster-crash: killed worker[1] connection; waiting for TTL-driven reassignment")
	time.Sleep(10 * time.Second) // > HeartbeatTTL(5s) + reassignment + drain

	require.NoError(t, <-w[0].m.WaitState(parti.StateStable, 40*time.Second))
	require.NoError(t, <-w[2].m.WaitState(parti.StateStable, 40*time.Second))
	time.Sleep(2 * time.Second)

	published := prod.stopAndWait()
	t.Cleanup(func() {
		_ = w[0].m.Stop(context.Background())
		_ = w[2].m.Stop(context.Background())
		w[0].nc.Close()
		w[2].nc.Close()
	})

	// No overlapping-filter / 10100 rejection on the TTL-driven rebind.
	overlaps := sink.overlapErrs()
	require.Empty(t, overlaps,
		"WorkQueue+Dynamic crash-reassignment produced an overlapping-filter/10100 rejection: %v", overlaps)

	// Lossless across the crash (dups expected — crash redelivers in-flight work).
	assertLossless(t, ledger, published)
	distinct, dups := ledger.stats()
	t.Logf("WorkQueue+Dynamic (cluster-crash, RF=3, R=3 consumers): published=%d delivered=%d dups=%d (dups expected) | manager errors total=%d overlap/10100=%d",
		len(published), distinct, dups, sink.count(), len(overlaps))
}
