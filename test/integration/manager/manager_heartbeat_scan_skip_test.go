package manager_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestHeartbeatScanSkip_SteadyStateConsumerFlatness proves the headline
// contract: in steady state (all workers heartbeating on schedule) the
// heartbeat bucket accrues ephemeral consumers only at the polling-ticker
// rate, not per heartbeat refresh. Measured directly via JetStream's
// consumer-created advisories on the heartbeat KV stream.
//
// Before the scan-skip change this test fails: every refresh triggers a
// debounced check whose Keys() call creates one ordered consumer, so the
// advisory count tracks the heartbeat write rate (~N_workers/interval)
// instead of the polling budget.
func TestHeartbeatScanSkip_SteadyStateConsumerFlatness(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	partitions := testutil.CreateTestPartitions(10)
	src := source.NewStatic(partitions)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	for range 3 {
		cluster.AddWorker(ctx)
	}
	defer cluster.StopWorkers()
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(15 * time.Second)

	// Count consumer creations on the heartbeat KV stream from NOW
	// (post-startup, so watcher-establishment consumers are excluded).
	var created atomic.Int64
	hbStream := "KV_" + cfg.KVBuckets.HeartbeatBucket
	sub, err := nc.Subscribe(
		fmt.Sprintf("$JS.EVENT.ADVISORY.CONSUMER.CREATED.%s.>", hbStream),
		func(_ *nats.Msg) { created.Add(1) },
	)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	// Soak: several heartbeat intervals of pure steady state.
	soak := 5 * time.Second
	time.Sleep(soak)

	// Budget: polling ticker fires every HeartbeatTTL/2 (= 2.5s at
	// IntegrationTestConfig) on the LEADER's monitor only (one Keys()
	// consumer per tick → ~2-3 in a 5s soak), plus generous slack for
	// stray audit scans → budget ≈ 10. Pre-change, refresh-driven checks
	// produce ~20-25 consumers in the same window (3 workers × 500ms
	// interval, debounce-coalesced), so the two assertions discriminate.
	pollTicks := int64(soak/(cfg.HeartbeatTTL/2)) + 1
	budget := pollTicks*2 + 4 // ×2 + slack: audits, elections, jitter
	refreshRate := int64(soak/cfg.HeartbeatInterval) * 3
	require.Less(t, created.Load(), refreshRate/2,
		"consumer creations must not track the heartbeat refresh rate")
	require.LessOrEqual(t, created.Load(), budget,
		"steady-state consumer creations on the heartbeat stream must stay within the polling budget")

	// Join responsiveness (spec §7.3): a worker joining mid-run must
	// still produce a prompt recompute — suppression must not swallow
	// join events.
	joiner := cluster.AddWorker(ctx)
	require.NoError(t, joiner.Start(ctx))
	require.NoError(t, <-joiner.WaitState(types.StateStable, 15*time.Second)) // WaitState returns <-chan error (manager_state.go:15)
	require.Eventually(t, func() bool {
		return len(joiner.CurrentAssignment().Partitions) > 0
	}, 15*time.Second, 100*time.Millisecond,
		"joining worker must receive partitions promptly (join-triggered check)")
}

// TestHeartbeatScanSkip_CrashDetectionPreserved pins the crash contract:
// a worker that dies WITHOUT deleting its heartbeat key (abrupt
// connection close) is detected and rebalanced-around within the spec's
// disjunction bound. Asserts the bound class, not which path (sweep vs
// polling) observes it first.
func TestHeartbeatScanSkip_CrashDetectionPreserved(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	// Grace-boundary variant folded in: run at the spec's worst valid
	// config, EmergencyGracePeriod == HeartbeatTTL.
	cfg.EmergencyGracePeriod = cfg.HeartbeatTTL

	src := source.NewStatic(testutil.CreateTestPartitions(10))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	for range 2 {
		cluster.AddWorker(ctx)
	}
	defer cluster.StopWorkers()
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(15 * time.Second)

	// The crash victim gets its OWN connection and manager (constructed
	// directly, NOT via the cluster, so cluster helpers only see the two
	// survivors) so we can sever its connection abruptly without touching
	// the survivors. Mirrors AddWorkerWithoutTracking's construction
	// (internal/testutil/nats.go:324) with a dedicated JS handle.
	victimNC, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	victimJS, err := jetstream.New(victimNC)
	require.NoError(t, err)
	victim, err := parti.NewManager(&cfg, victimJS, src, strategy.NewConsistentHash())
	require.NoError(t, err)
	require.NoError(t, victim.Start(ctx))
	require.NoError(t, <-victim.WaitState(types.StateStable, 15*time.Second)) // WaitState returns <-chan error (manager_state.go:15)
	defer func() {                                                            // tolerant: conn is closed by then; Stop takes a ctx (manager.go:797)
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = victim.Stop(sctx)
	}()

	// Guard: the victim must actually own partitions, or the test is vacuous.
	require.Eventually(t, func() bool {
		return len(victim.CurrentAssignment().Partitions) > 0
	}, 15*time.Second, 100*time.Millisecond, "victim must own partitions before the crash")

	// Crash: abrupt close — no graceful heartbeat DELETE. The key persists
	// until HeartbeatTTL expiry (watcher-silent).
	victimNC.Close()

	// Disjunction bound (spec §5.3/§7.2), decomposed at
	// IntegrationTestConfig values (hbTTL=5s, grace==hbTTL here):
	//   key expiry            ≤ hbTTL        = 5s
	//   first-miss observation ≤ hbTTL/2     = 2.5s (max(sweep≈interval, polling))
	//   grace                  = hbTTL       = 5s
	//   confirmation           ≤ hbTTL/2     = 2.5s (polling backstop)
	//   rebalance+apply+slack               = 10s
	// Σ = 3×hbTTL + 10s = 25s. Asserts the bound CLASS (either sweep or
	// polling path may win each leg — do not assert which).
	bound := 3*cfg.HeartbeatTTL + 10*time.Second

	// Survivors must re-cover ALL partitions (the victim's included)
	// within the bound. WaitForPartitionCoverage iterates only the
	// cluster-tracked survivors (internal/testutil/cluster_helpers.go:76).
	cluster.WaitForPartitionCoverage(10, bound)
}

// TestHeartbeatWatcher_NoRaceUnderConcurrentKVTraffic is the
// monitor-goroutine concurrency stress test required by AGENTS.md for
// changes to watcher loops — sibling of the epoch-monitor template.
// The classification state is goroutine-local by design; this test
// verifies no cross-goroutine access sneaks in between the watcher loop,
// the polling ticker, and production KV traffic under -race.
func TestHeartbeatWatcher_NoRaceUnderConcurrentKVTraffic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()
	// Aggressive heartbeat cadence maximizes classification and sweep
	// activity per second.
	cfg.HeartbeatInterval = 100 * time.Millisecond
	cfg.HeartbeatTTL = 300 * time.Millisecond
	cfg.EmergencyGracePeriod = 150 * time.Millisecond

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	src := source.NewStatic(testutil.CreateTestPartitions(10))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	for range 3 {
		cluster.AddWorker(ctx)
	}
	defer cluster.StopWorkers()
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(15 * time.Second)

	// Drive foreign KV traffic against the heartbeat bucket (keys outside
	// the hb prefix) concurrently with the watchers' classification loops.
	hbKV, err := js.KeyValue(context.Background(), cfg.KVBuckets.HeartbeatBucket)
	require.NoError(t, err)
	soakCtx, soakCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer soakCancel()
	go func() {
		i := 0
		for soakCtx.Err() == nil {
			key := fmt.Sprintf("stress.k%d", i%7)
			_, _ = hbKV.Put(soakCtx, key, []byte("x"))
			if i%3 == 0 {
				_ = hbKV.Delete(soakCtx, key)
			}
			i++
			time.Sleep(5 * time.Millisecond)
		}
	}()
	<-soakCtx.Done()

	// Race detector is the assertion; liveness sanity on top.
	for _, mgr := range cluster.GetActiveWorkers() {
		require.NotNil(t, mgr.CurrentAssignment())
	}
}
