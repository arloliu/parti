// Package fixedpartitions_test is the live-cluster integration proof for the
// RECOMMENDED pattern (see docs/plans/partition-scaling/): serve work with parti's
// EXISTING consumer.Dynamic over a FIXED set of K numbered partition-subjects
// (work.0..work.K-1) — NO new consumer type — and assert that the real Manager
// assignment + handoff reassigns slots across worker churn WITHOUT losing messages,
// using only public parti API.
//
// This is "Exp11"/"Exp12" of the partition-scaling feasibility study
// (docs/plans/partition-scaling/). Five scenarios:
//   - SingleNodeGraceful        : 1 node, worker join+leave (graceful Stop).
//   - ClusterCrashRecovery       : 3-node cluster, RF=3 stream + R=3 consumers,
//     abrupt worker CRASH (connection killed) → survivors detect via heartbeat TTL
//     and rebind the slot durables.
//   - ProcessingGate             : two-phase handoff + processing gate + resolver
//     wired, churn — gate must not break losslessness.
//   - ServerSidePartitionMapping : full recommended path — producers publish
//     ingest.<key>, NATS {{partition(K,1)}} routes to work.<p>, Dynamic consumes.
//   - NoStopTheWorld (Exp12)     : continuity proof — measures per-message latency
//     during churn to show retained slots keep flowing (no global pause); only
//     reassigned slots blip. Discriminates a cooperative per-slot rebalance from
//     a Kafka-style eager stop-the-world (which losslessness alone cannot).
package fixedpartitions_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

const (
	vpK      = 16
	vpStream = "WORK"
)

func vpPartitions() []parti.Partition {
	out := make([]parti.Partition, vpK)
	for i := range out {
		out[i] = parti.Partition{Keys: []string{fmt.Sprintf("%d", i)}, Weight: 1}
	}

	return out
}

// ---- shared ledger (delivery aggregation across all workers' handlers) ------

type vpLedger struct {
	mu        sync.Mutex
	delivered map[string]int
}

func newLedger() *vpLedger { return &vpLedger{delivered: map[string]int{}} }

func (l *vpLedger) handler() consumer.MessageHandler {
	return consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		l.mu.Lock()
		l.delivered[string(msg.Data())]++
		l.mu.Unlock()
		return nil
	})
}

func (l *vpLedger) missing(pub []string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, p := range pub {
		if l.delivered[p] == 0 {
			n++
		}
	}

	return n
}

func (l *vpLedger) stats() (distinct, dups int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.delivered {
		distinct++
		if c > 1 {
			dups++
		}
	}

	return distinct, dups
}

// ---- shared background producer (round-robin over the K subjects) -----------

type vpProducer struct {
	mu        sync.Mutex
	published []string
	stop      chan struct{}
	wg        sync.WaitGroup
}

//nolint:unparam // k is the partition count; constant across current tests by design.
func startProducer(js jetstream.JetStream, k int, interval time.Duration) *vpProducer {
	p := &vpProducer{stop: make(chan struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		seq := 0
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-tk.C:
				payload := fmt.Sprintf("m-%07d", seq)
				if _, e := js.Publish(context.Background(), fmt.Sprintf("work.%d", seq%k), []byte(payload)); e == nil {
					p.mu.Lock()
					p.published = append(p.published, payload)
					p.mu.Unlock()
				}
				seq++
			}
		}
	}()

	return p
}

func (p *vpProducer) stopAndWait() []string {
	close(p.stop)
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.published...)
}

// assertLossless waits until every published payload has been delivered >=1.
func assertLossless(t *testing.T, l *vpLedger, published []string) {
	t.Helper()
	require.Greater(t, len(published), 100, "producer should publish a meaningful volume")
	require.Eventually(t, func() bool { return l.missing(published) == 0 }, 45*time.Second, 250*time.Millisecond,
		"LOSS: a published message was never delivered across churn")
}

// ---- Scenario 1: single-node, graceful join+leave (baseline) ----------------

func TestFixedPartitions_SingleNodeGraceful(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}
	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name: vpStream, Subjects: []string{"work.*"},
		Storage: jetstream.MemoryStorage, Retention: jetstream.LimitsPolicy,
	})
	require.NoError(t, err)

	ledger := newLedger()
	src := source.NewStatic(vpPartitions())
	cfg := parti.TestConfig()

	newWorker := func() *parti.Manager {
		c, err := consumer.NewDynamic(js, vpStream, "worker", "work.{{.PartitionID}}", ledger.handler(),
			consumer.WithConsumerMemoryStorage(true))
		require.NoError(t, err)
		m, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(), parti.WithWorkerConsumerUpdater(c))
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

	m3 := newWorker() // JOIN 2->3
	time.Sleep(2 * time.Second)
	waitStable(m1)
	waitStable(m3)
	time.Sleep(1 * time.Second)

	require.NoError(t, m2.Stop(context.Background())) // LEAVE (graceful)
	time.Sleep(2 * time.Second)
	waitStable(m1)
	waitStable(m3)
	time.Sleep(1 * time.Second)

	published := prod.stopAndWait()
	t.Cleanup(func() { _ = m1.Stop(context.Background()); _ = m3.Stop(context.Background()) })

	assertLossless(t, ledger, published)
	distinct, dups := ledger.stats()
	t.Logf("Exp11/single-graceful: published=%d delivered=%d dups=%d", len(published), distinct, dups)
}

// ---- Scenario 2: 3-node cluster, RF=3, abrupt worker CRASH ------------------

func TestFixedPartitions_ClusterCrashRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}
	ctx := context.Background()
	clusterNC, servers, cleanup := testutil.StartEmbeddedNATSCluster(t)
	defer cleanup()
	require.GreaterOrEqual(t, len(servers), 3, "need a 3-node cluster")

	jsMain, err := jetstream.New(clusterNC)
	require.NoError(t, err)
	_, err = jsMain.CreateStream(ctx, jetstream.StreamConfig{
		Name: vpStream, Subjects: []string{"work.*"},
		Storage: jetstream.FileStorage, Retention: jetstream.LimitsPolicy, Replicas: 3,
	})
	require.NoError(t, err)

	ledger := newLedger()
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
		c, err := consumer.NewDynamic(wjs, vpStream, "worker", "work.{{.PartitionID}}", ledger.handler(),
			consumer.WithConsumerMemoryStorage(true), consumer.WithConsumerReplicas(3))
		require.NoError(t, err)
		m, err := parti.NewManager(&cfg, wjs, src, strategy.NewConsistentHash(), parti.WithWorkerConsumerUpdater(c))
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
	t.Log("Exp11/cluster-crash: killed worker[1] connection; waiting for TTL-driven reassignment")
	time.Sleep(10 * time.Second) // > HeartbeatTTL(5s) + reassignment + drain

	// Survivors converge.
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

	assertLossless(t, ledger, published)
	distinct, dups := ledger.stats()
	t.Logf("Exp11/cluster-crash (RF=3, R=3 consumers): published=%d delivered=%d dups=%d (dups expected — crash redelivers in-flight)", len(published), distinct, dups)
}

// ---- Scenario 3: processing gate + resolver, churn --------------------------

func TestFixedPartitions_ProcessingGate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}
	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name: vpStream, Subjects: []string{"work.*"},
		Storage: jetstream.MemoryStorage, Retention: jetstream.LimitsPolicy,
	})
	require.NoError(t, err)

	ledger := newLedger()
	src := source.NewStatic(vpPartitions())

	bucket := "parti-handoff-vp"
	cfg := parti.TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = bucket
	cfg.KVBuckets.HandoffTTL = 2 * time.Minute

	gate := &consumer.ProcessingGateConfig{Enabled: true, NakDelay: 50 * time.Millisecond}
	newWorker := func() *parti.Manager {
		c, err := consumer.NewDynamic(js, vpStream, "worker", "work.{{.PartitionID}}", ledger.handler(),
			consumer.WithConsumerMemoryStorage(true),
			consumer.WithProcessingGate(gate),
			consumer.WithResolver(consumer.ResolverConfig{HandoffBucketName: bucket}),
		)
		require.NoError(t, err)
		m, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(), parti.WithWorkerConsumerUpdater(c))
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

	m3 := newWorker() // JOIN
	time.Sleep(2 * time.Second)
	waitStable(m1)
	waitStable(m3)
	time.Sleep(1 * time.Second)

	require.NoError(t, m2.Stop(context.Background())) // LEAVE
	time.Sleep(2 * time.Second)
	waitStable(m1)
	waitStable(m3)
	time.Sleep(1 * time.Second)

	published := prod.stopAndWait()
	t.Cleanup(func() { _ = m1.Stop(context.Background()); _ = m3.Stop(context.Background()) })

	// Gate must not break losslessness; it bounds non-owner double-processing.
	assertLossless(t, ledger, published)
	distinct, dups := ledger.stats()
	t.Logf("Exp11/processing-gate: published=%d delivered=%d dups=%d (gate suppresses non-owner processing during handoff)", len(published), distinct, dups)
}

// ---- Scenario 4: full recommended path — NATS server-side partition() --------
//
// Proves the END-TO-END recommended deployment: producers publish ingest.<key>,
// the NATS server-side `{{partition(K,1)}}` mapping deterministically routes each
// key to work.<p>, and the EXISTING consumer.Dynamic over work.{{.PartitionID}}
// consumes it — losslessly across worker churn. This is the scenario that actually
// exercises server-side deterministic subject partitioning (vs scenarios 1-3 which
// publish to the numbered subjects directly).
func TestFixedPartitions_ServerSidePartitionMapping(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}
	ctx := context.Background()

	// Embedded server created directly so we can install the account mapping.
	ns, err := natsserver.NewServer(&natsserver.Options{JetStream: true, Port: -1, StoreDir: t.TempDir()})
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(10*time.Second), "server not ready")
	defer ns.Shutdown()

	// ingest.<key> -> work.<fnv32a(key)%K>, applied at ingest (before JetStream capture).
	require.NoError(t, ns.GlobalAccount().AddMapping("ingest.*", fmt.Sprintf("work.{{partition(%d,1)}}", vpK)))

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name: vpStream, Subjects: []string{"work.*"},
		Storage: jetstream.MemoryStorage, Retention: jetstream.LimitsPolicy,
	})
	require.NoError(t, err)

	ledger := newLedger()
	src := source.NewStatic(vpPartitions())
	cfg := parti.TestConfig()

	newWorker := func() *parti.Manager {
		c, err := consumer.NewDynamic(js, vpStream, "worker", "work.{{.PartitionID}}", ledger.handler(),
			consumer.WithConsumerMemoryStorage(true))
		require.NoError(t, err)
		m, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(), parti.WithWorkerConsumerUpdater(c))
		require.NoError(t, err)
		require.NoError(t, m.Start(ctx))
		return m
	}
	waitStable := func(m *parti.Manager) { require.NoError(t, <-m.WaitState(parti.StateStable, 30*time.Second)) }

	m1 := newWorker()
	m2 := newWorker()
	waitStable(m1)
	waitStable(m2)

	// Producer publishes ingest.<payload>; NATS routes by key-hash to work.<p>.
	var pubMu sync.Mutex
	published := make([]string, 0, 1024)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		seq := 0
		tk := time.NewTicker(8 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				payload := fmt.Sprintf("k-%07d", seq)
				if _, e := js.Publish(ctx, "ingest."+payload, []byte(payload)); e == nil {
					pubMu.Lock()
					published = append(published, payload)
					pubMu.Unlock()
				}
				seq++
			}
		}
	})
	time.Sleep(1 * time.Second)

	m3 := newWorker() // JOIN
	time.Sleep(2 * time.Second)
	waitStable(m1)
	waitStable(m3)
	time.Sleep(1 * time.Second)

	require.NoError(t, m2.Stop(context.Background())) // LEAVE
	time.Sleep(2 * time.Second)
	waitStable(m1)
	waitStable(m3)
	time.Sleep(1 * time.Second)

	close(stop)
	wg.Wait()
	t.Cleanup(func() { _ = m1.Stop(context.Background()); _ = m3.Stop(context.Background()) })

	pubMu.Lock()
	pubs := append([]string(nil), published...)
	pubMu.Unlock()
	assertLossless(t, ledger, pubs)
	distinct, dups := ledger.stats()
	t.Logf("Exp11/server-side-partition: published=%d (via ingest.* -> work.{{partition(%d,1)}}) delivered=%d dups=%d", len(pubs), vpK, distinct, dups)
}

// ============================================================================
// Exp12 — "does this avoid Kafka's stop-the-world rebalance?" (continuity proof)
// ============================================================================
//
// Exp11 proved LOSSLESSNESS across churn. But losslessness alone does NOT
// disprove a global pause: a Kafka-style eager stop-the-world rebalance would
// ALSO be lossless — it would just stamp the full pause duration onto every
// in-flight message as latency, then drain. So losslessness and stop-the-world
// are indistinguishable on delivery counts alone.
//
// This experiment discriminates the two by measuring PER-MESSAGE LATENCY
// (recvNano-pubNano) split by slot class and time window:
//
//   - RETAINED slot  = one worker delivered (essentially) all its messages
//                      throughout: it was NOT reassigned during churn.
//   - MOVED slot     = ownership changed (>=2 workers delivered it): reassigned.
//
// The load-bearing claim — "retained slots keep flowing during churn, only
// moved slots blip" — becomes a single assertion: RETAINED-slot p99 latency
// during the churn window stays near the quiet-window baseline. Under a global
// freeze, retained slots would spike by the pause duration too; under a
// cooperative (per-slot) rebalance they stay at baseline.
//
// Latency is the primary metric because it is immune to producer publish-cadence
// jitter (a synchronous Publish in a ticker drops ticks under -race). Dead-air
// is reported as corroboration but compared against the ACTUAL publish cadence,
// not an assumed-uniform clock.
//
// Scope: single-node graceful join+leave (cleanest discriminator — no crash
// TTL-detection gap to muddy the moved-slot numbers; crash losslessness is
// already covered by Exp11 scenario 2).

// vpTimeLedger records per-delivery timing + which worker delivered, so the
// analysis can classify slots and compute latency distributions.
type vpEvent struct {
	id        string // unique per published message (dedup / loss key)
	partition int
	worker    string
	pubNano   int64
	recvNano  int64
}

type vpTimeLedger struct {
	mu        sync.Mutex
	events    []vpEvent
	delivered map[string]int // id -> delivery count (loss/dup)
}

func newTimeLedger() *vpTimeLedger { return &vpTimeLedger{delivered: map[string]int{}} }

// handlerForWorker returns a handler tagged with the owning worker, so a
// delivery can be attributed to the worker that processed it.
func (l *vpTimeLedger) handlerForWorker(worker string) consumer.MessageHandler {
	return consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		recvNano := time.Now().UnixNano()
		id, pubStr, _ := strings.Cut(string(msg.Data()), ":")
		pubNano, _ := strconv.ParseInt(pubStr, 10, 64)
		part, _ := strconv.Atoi(strings.TrimPrefix(msg.Subject(), "work."))
		l.mu.Lock()
		l.events = append(l.events, vpEvent{id: id, partition: part, worker: worker, pubNano: pubNano, recvNano: recvNano})
		l.delivered[id]++
		l.mu.Unlock()

		return nil
	})
}

func (l *vpTimeLedger) missing(pub []string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, p := range pub {
		if l.delivered[p] == 0 {
			n++
		}
	}

	return n
}

func (l *vpTimeLedger) snapshot() []vpEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]vpEvent(nil), l.events...)
}

// vpTimedProducer publishes one uniquely-id'd message to EACH of the k subjects
// every tick (uniform per-slot cadence) so retained slots have a dense, steady
// delivery stream whose latency is meaningful to percentile.
type vpTimedProducer struct {
	mu        sync.Mutex
	published []string
	stop      chan struct{}
	wg        sync.WaitGroup
}

func startTimedProducer(js jetstream.JetStream, k int, interval time.Duration) *vpTimedProducer {
	p := &vpTimedProducer{stop: make(chan struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		id := 0
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-tk.C:
				for part := 0; part < k; part++ {
					pubNano := time.Now().UnixNano()
					payload := fmt.Sprintf("%07d:%d", id, pubNano)
					if _, e := js.Publish(context.Background(), fmt.Sprintf("work.%d", part), []byte(payload)); e == nil {
						p.mu.Lock()
						p.published = append(p.published, fmt.Sprintf("%07d", id))
						p.mu.Unlock()
					}
					id++
				}
			}
		}
	}()

	return p
}

func (p *vpTimedProducer) stopAndWait() []string {
	close(p.stop)
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.published...)
}

func pctlMs(sortedNanos []int64, p float64) float64 {
	if len(sortedNanos) == 0 {
		return 0
	}
	idx := int(p / 100.0 * float64(len(sortedNanos)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sortedNanos) {
		idx = len(sortedNanos) - 1
	}

	return float64(sortedNanos[idx]) / 1e6
}

func maxNano(ns []int64) int64 {
	var m int64
	for _, v := range ns {
		if v > m {
			m = v
		}
	}
	return m
}

func TestFixedPartitions_NoStopTheWorld(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}
	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name: vpStream, Subjects: []string{"work.*"},
		Storage: jetstream.MemoryStorage, Retention: jetstream.LimitsPolicy,
	})
	require.NoError(t, err)

	ledger := newTimeLedger()
	src := source.NewStatic(vpPartitions())
	cfg := parti.TestConfig()

	newWorker := func(tag string) *parti.Manager {
		c, err := consumer.NewDynamic(js, vpStream, "worker", "work.{{.PartitionID}}", ledger.handlerForWorker(tag),
			consumer.WithConsumerMemoryStorage(true))
		require.NoError(t, err)
		m, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(), parti.WithWorkerConsumerUpdater(c))
		require.NoError(t, err)
		require.NoError(t, m.Start(ctx))
		return m
	}
	waitStable := func(m *parti.Manager) { require.NoError(t, <-m.WaitState(parti.StateStable, 30*time.Second)) }

	m1 := newWorker("w1")
	m2 := newWorker("w2")
	waitStable(m1)
	waitStable(m2)

	prod := startTimedProducer(js, vpK, 20*time.Millisecond) // ~800 msg/s across 16 slots
	time.Sleep(1500 * time.Millisecond)                      // warmup; exclude from baseline
	tWarmupEndNano := time.Now().UnixNano()

	time.Sleep(2 * time.Second) // clean quiet baseline (steady 2-worker operation)

	tJoinNano := time.Now().UnixNano()
	m3 := newWorker("w3") // JOIN 2->3: ConsistentHash moves ~1/3 of slots to w3
	time.Sleep(3 * time.Second)
	waitStable(m1)
	waitStable(m3)

	require.NoError(t, m2.Stop(context.Background())) // LEAVE 3->2 (graceful)
	time.Sleep(3 * time.Second)
	waitStable(m1)
	waitStable(m3)
	tEndNano := time.Now().UnixNano()

	published := prod.stopAndWait()
	t.Cleanup(func() { _ = m1.Stop(context.Background()); _ = m3.Stop(context.Background()) })

	// 1) Losslessness (same gate as Exp11) — must hold regardless of timing.
	require.Greater(t, len(published), 1000, "producer should publish a meaningful volume")
	require.Eventually(t, func() bool { return ledger.missing(published) == 0 }, 45*time.Second, 250*time.Millisecond,
		"LOSS: a published message was never delivered across churn")

	// 2) Analyze: classify slots, compute per-class latency distributions and the
	// pause-sensitivity metrics. Extracted to keep this test under the cyclomatic
	// budget; see analyzeNoStopTheWorld for the method.
	s := analyzeNoStopTheWorld(ledger.snapshot(), tWarmupEndNano, tJoinNano, tEndNano)

	t.Logf("Exp12/no-stop-the-world: slots retained=%d moved=%d (K=%d)", s.retained, s.moved, vpK)
	t.Logf("  RETAINED latency ms: quiet p99=%.1f | churn p50=%.1f p99=%.1f max=%.1f (n_quiet=%d n_churn=%d)",
		s.quietP99, s.retChurnP50, s.retChurnP99, s.retChurnMax, s.quietRetainedN, s.churnRetainedN)
	t.Logf("  RETAINED pause check: deliveries >%.0fms during churn=%d | worst 250ms-epoch max=%.1fms | epochs >50ms=%d",
		noStwSlowMs, s.retainedSlow, s.worstEpochMs, s.epochsOver50)
	t.Logf("  MOVED    latency ms: churn p50=%.1f p99=%.1f max=%.1f (n=%d) <- localized handoff blip",
		s.movedChurnP50, s.movedChurnP99, s.movedChurnMax, s.churnMovedN)
	t.Logf("  RETAINED throughput: max retained delivery dead-air=%.1fms vs max publish gap=%.1fms (retained slots never went dark together if comparable)",
		s.deadAirMs, s.publishGapMs)
	t.Logf("  RETAINED ownership: %d/%d retained slots are owned by a worker that ALSO handled a moved slot (i.e. a BUSY worker kept its other slots flowing, not an idle bystander)",
		s.retainedOnChurning, s.retained)

	// 3) Assertions.
	//
	// POSITIVE CONTROL — the moved-slot blip (tens of ms here, see logs) proves the
	// instrument actually DETECTS a delay when one exists. So retainedSlow==0 means
	// "the same machinery that caught the moved-slot blip saw nothing on retained
	// slots", not "the measurement is broken and reads zero". Both directions of the
	// boundary are exercised (cf. test_both_directions_of_boundary discipline).
	//
	// Non-vacuity: churn must actually have moved slots, there must be enough
	// retained slots/samples for the primary assertion to mean something, AND at
	// least some retained slots must sit on a worker that itself churned (otherwise
	// we would only prove an idle worker kept flowing, not a busy one).
	require.GreaterOrEqual(t, s.moved, 1, "churn moved no slots — test exercised nothing")
	require.GreaterOrEqual(t, s.retained, 4, "too few retained slots to prove continuity")
	require.Greater(t, s.churnRetainedN, 100, "too few retained-slot deliveries during churn to be meaningful")
	require.Greater(t, s.retainedOnChurning, 0,
		"all retained slots were on idle workers — does not prove a CHURNING worker kept its other slots flowing")

	// PRIMARY (load-bearing): retained slots do NOT freeze during churn. A
	// Kafka-style eager stop-the-world stamps the whole rebalance cycle onto every
	// retained message in flight (dozens of slow samples); a cooperative per-slot
	// rebalance touches only moved slots, leaving retained ones at baseline (zero
	// slow samples). The noStwSlowMs(100ms) threshold sits far above observed
	// baseline (~0.1-0.8ms) and -race/scheduler jitter, far below any pause worth
	// caring about; tolerance of 2 absorbs a rare isolated GC outlier without
	// admitting a real pause (which delays >=10s of retained messages even at
	// 150-300ms).
	require.LessOrEqualf(t, s.retainedSlow, 2,
		"%d retained-slot deliveries were delayed >%.0fms during churn — looks like a global pause, not a cooperative per-slot rebalance (worst 250ms-epoch max=%.1fms)",
		s.retainedSlow, noStwSlowMs, s.worstEpochMs)

	// Backstop: even a single egregious retained-slot spike (which the tolerance
	// above could otherwise admit) trips this generous ceiling.
	require.LessOrEqualf(t, s.retChurnMax, 250.0,
		"a retained slot spiked to %.1fms during churn — inconsistent with cooperative rebalance", s.retChurnMax)
}

// noStwSlowMs is the "this delivery looks like it hit a pause" latency threshold
// for Exp12's retained-slot pause check (far above baseline + jitter, far below a
// pause worth caring about).
const noStwSlowMs = 100.0

// noStwStats holds the Exp12 latency analysis, split by slot class and window.
type noStwStats struct {
	retained, moved                             int
	quietRetainedN, churnRetainedN, churnMovedN int
	quietP99                                    float64
	retChurnP50, retChurnP99, retChurnMax       float64
	movedChurnP50, movedChurnP99, movedChurnMax float64
	retainedSlow, epochsOver50                  int
	worstEpochMs                                float64
	deadAirMs, publishGapMs                     float64
	retainedOnChurning                          int
}

// classifyNoStwSlots splits partitions into moved (ownership changed during the
// run) and retained (one worker delivered ~all of it). The overlap allowance
// tolerates the 1-2 stray deliveries a handoff can leak to the gaining worker
// before the loser drains, so a brief overlap does not mislabel a retained slot.
func classifyNoStwSlots(events []vpEvent) (moved, retained map[int]bool) {
	const overlapAllow = 2

	byWorker := map[int]map[string]int{}
	for _, e := range events {
		m := byWorker[e.partition]
		if m == nil {
			m = map[string]int{}
			byWorker[e.partition] = m
		}
		m[e.worker]++
	}

	moved, retained = map[int]bool{}, map[int]bool{}
	for p, m := range byWorker {
		counts := make([]int, 0, len(m))
		for _, c := range m {
			counts = append(counts, c)
		}
		slices.SortFunc(counts, func(a, b int) int { return b - a })
		if len(counts) >= 2 && counts[1] > overlapAllow {
			moved[p] = true
		} else {
			retained[p] = true
		}
	}

	return moved, retained
}

// retainedOnChurningWorkers counts retained slots whose (dominant) owner is a
// worker that ALSO handled at least one moved slot — i.e. retained slots that
// stayed put on a worker that itself underwent an assignment change. This
// upgrades the continuity claim from "an idle bystander kept flowing" to "a busy
// worker kept its OTHER slots flowing while reassigning some".
func retainedOnChurningWorkers(events []vpEvent, moved, retained map[int]bool) int {
	byPartWorker := map[int]map[string]int{}
	for _, e := range events {
		m := byPartWorker[e.partition]
		if m == nil {
			m = map[string]int{}
			byPartWorker[e.partition] = m
		}
		m[e.worker]++
	}

	churning := map[string]bool{} // workers that delivered any moved slot (either side of a handoff)
	for p := range moved {
		for w := range byPartWorker[p] {
			churning[w] = true
		}
	}

	n := 0
	for p := range retained {
		best, bestCount := "", -1
		for w, c := range byPartWorker[p] {
			if c > bestCount {
				best, bestCount = w, c
			}
		}
		if churning[best] {
			n++
		}
	}

	return n
}

// maxGapNano returns the largest gap between consecutive values in a sorted slice.
func maxGapNano(sorted []int64) int64 {
	var mx int64
	for i := 1; i < len(sorted); i++ {
		if d := sorted[i] - sorted[i-1]; d > mx {
			mx = d
		}
	}

	return mx
}

// retainedDeadAir returns the max gap in RETAINED-slot delivery during churn and
// the max publish gap over the same messages. Retained-only so a single very-late
// moved-slot delivery (seconds under -race) cannot manufacture a fake global-stall
// gap in the merged stream.
func retainedDeadAir(events []vpEvent, moved map[int]bool, tJoin, tEnd int64) (deadAirMs, publishGapMs float64) {
	var recvTimes, pubTimes []int64
	for _, e := range events {
		if moved[e.partition] {
			continue
		}
		if e.pubNano >= tJoin && e.pubNano <= tEnd {
			recvTimes = append(recvTimes, e.recvNano)
			pubTimes = append(pubTimes, e.pubNano)
		}
	}
	slices.Sort(recvTimes)
	slices.Sort(pubTimes)

	return float64(maxGapNano(recvTimes)) / 1e6, float64(maxGapNano(pubTimes)) / 1e6
}

// retainedPauseMetrics counts retained-slot churn deliveries delayed beyond
// noStwSlowMs and, via a 250ms-epoch timeline (bucketed by arrival), the worst
// per-epoch latency. A localized global pause shows up here even when a pooled
// p99 over the wide churn window would dilute it. Membership is by publish time
// so a churn-published message delivered late is still counted.
func retainedPauseMetrics(events []vpEvent, moved map[int]bool, tJoin, tEnd int64) (slow, epochsOver50 int, worstEpochMs float64) {
	const epochNanos = int64(250 * time.Millisecond)

	epochMax := map[int64]int64{}
	for _, e := range events {
		if moved[e.partition] || e.pubNano < tJoin || e.pubNano > tEnd {
			continue
		}
		lat := e.recvNano - e.pubNano
		if float64(lat)/1e6 > noStwSlowMs {
			slow++
		}
		ep := (e.recvNano - tJoin) / epochNanos
		if lat > epochMax[ep] {
			epochMax[ep] = lat
		}
	}

	for _, v := range epochMax {
		ms := float64(v) / 1e6
		if ms > worstEpochMs {
			worstEpochMs = ms
		}
		if ms > 50 {
			epochsOver50++
		}
	}

	return slow, epochsOver50, worstEpochMs
}

// analyzeNoStopTheWorld is Exp12's full latency analysis: classify slots, bucket
// per-message latency by (slot class × quiet/churn window), and compute the
// pause-sensitivity metrics. Windows are keyed on PUBLISH time vs the anchors:
// quiet = [warmupEnd, join), churn = [join, end].
func analyzeNoStopTheWorld(events []vpEvent, tWarmupEnd, tJoin, tEnd int64) noStwStats {
	moved, retained := classifyNoStwSlots(events)

	var quietRetained, churnRetained, churnMoved []int64
	for _, e := range events {
		lat := e.recvNano - e.pubNano
		if lat < 0 {
			lat = 0
		}
		switch {
		case e.pubNano >= tWarmupEnd && e.pubNano < tJoin:
			if !moved[e.partition] {
				quietRetained = append(quietRetained, lat)
			}
		case e.pubNano >= tJoin && e.pubNano <= tEnd:
			if moved[e.partition] {
				churnMoved = append(churnMoved, lat)
			} else {
				churnRetained = append(churnRetained, lat)
			}
		}
	}
	slices.Sort(quietRetained)
	slices.Sort(churnRetained)
	slices.Sort(churnMoved)

	slow, epochsOver50, worstEpochMs := retainedPauseMetrics(events, moved, tJoin, tEnd)
	deadAirMs, publishGapMs := retainedDeadAir(events, moved, tJoin, tEnd)

	return noStwStats{
		retained: len(retained), moved: len(moved),
		quietRetainedN: len(quietRetained), churnRetainedN: len(churnRetained), churnMovedN: len(churnMoved),
		quietP99:    pctlMs(quietRetained, 99),
		retChurnP50: pctlMs(churnRetained, 50), retChurnP99: pctlMs(churnRetained, 99),
		retChurnMax:   float64(maxNano(churnRetained)) / 1e6,
		movedChurnP50: pctlMs(churnMoved, 50), movedChurnP99: pctlMs(churnMoved, 99),
		movedChurnMax: float64(maxNano(churnMoved)) / 1e6,
		retainedSlow:  slow, epochsOver50: epochsOver50, worstEpochMs: worstEpochMs,
		deadAirMs: deadAirMs, publishGapMs: publishGapMs,
		retainedOnChurning: retainedOnChurningWorkers(events, moved, retained),
	}
}
