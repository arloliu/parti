package assignment_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Shared label-metrics recorder (spec §13)
//
// recordingLabelMetrics is a real MetricsCollector (via the embedded
// internal/metrics.NopMetrics) that ALSO satisfies the optional
// types.LabelMetrics extension, so it can be installed on a live Manager with
// parti.WithMetrics and observe the label counters the manager/calculator emit.
// Only the two counters these tests key on are overridden; every other
// MetricsCollector + LabelMetrics method is inherited (no-op) from NopMetrics.
// ---------------------------------------------------------------------------

type recordingLabelMetrics struct {
	*metrics.NopMetrics

	mu                sync.Mutex
	incarnationReject int
	unlabeledFallback int
}

// Compile-time proof the recorder satisfies BOTH surfaces WithMetrics needs:
// the full MetricsCollector (so parti.WithMetrics accepts it) and the optional
// LabelMetrics extension (so the manager's type-assertion succeeds and the
// label counters actually reach it).
var (
	_ types.MetricsCollector = (*recordingLabelMetrics)(nil)
	_ types.LabelMetrics     = (*recordingLabelMetrics)(nil)
)

func newRecordingLabelMetrics() *recordingLabelMetrics {
	return &recordingLabelMetrics{NopMetrics: metrics.NewNop()}
}

func (m *recordingLabelMetrics) IncrementLabelIncarnationReject() {
	m.mu.Lock()
	m.incarnationReject++
	m.mu.Unlock()
}

func (m *recordingLabelMetrics) IncrementUnlabeledFallback() {
	m.mu.Lock()
	m.unlabeledFallback++
	m.mu.Unlock()
}

func (m *recordingLabelMetrics) rejects() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.incarnationReject
}

func (m *recordingLabelMetrics) fallbacks() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.unlabeledFallback
}

// newKVSource seeds a NatsKV partition source in its own bucket and starts its
// watch/reconcile loops. Mirrors the setup every other label integration test
// uses so these policy tests learn partitions through the production watcher
// path, not an in-process shortcut.
func newKVSource(t *testing.T, ctx context.Context, js jetstream.JetStream, bucket string, parts []types.Partition) *source.NatsKV {
	t.Helper()
	srcKV, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)
	src := source.NewNatsKV(srcKV, "partitions", nil)
	require.NoError(t, src.Update(ctx, parts))
	require.NoError(t, src.Start(ctx))
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	return src
}

// ---------------------------------------------------------------------------
// Policy matrix: dedicated (reserve labeled workers) vs shared (route to all)
// ---------------------------------------------------------------------------

// TestUnlabeledPartitionPolicy_SharedVsDedicated pins spec §7's two unlabeled
// routing policies against a live fleet of one labeled + one plain worker with
// an ALL-UNLABELED source:
//
//   - dedicated (default): the labeled worker is RESERVED for labeled work, so
//     it owns nothing; every unlabeled partition lands on the plain worker.
//   - shared: unlabeled partitions route to ALL workers, so both the labeled
//     and the plain worker carry a share.
//
// The two sub-scenarios run on independent embedded NATS servers so their
// coordination buckets (which share default names) never collide.
func TestUnlabeledPartitionPolicy_SharedVsDedicated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	unlabeled := []types.Partition{
		{Keys: []string{"u-p0"}, Weight: 1},
		{Keys: []string{"u-p1"}, Weight: 1},
		{Keys: []string{"u-p2"}, Weight: 1},
		{Keys: []string{"u-p3"}, Weight: 1},
	}

	t.Run("dedicated_reserves_labeled_worker", func(t *testing.T) {
		nc, cleanup := testutil.StartEmbeddedNATS(t)
		defer cleanup()
		js, err := jetstream.New(nc)
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		src := newKVSource(t, ctx, js, "label-policy-dedicated", unlabeled)

		cfg := testutil.IntegrationTestConfig() // UnlabeledPartitionPolicy defaults to "dedicated"

		cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
		vipWorker := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("vip"))
		plainWorker := cluster.AddWorkerWithOptions(ctx)
		t.Cleanup(cluster.StopWorkers)
		cluster.StartWorkers(ctx)
		cluster.WaitForStableState(20 * time.Second)

		// Dedicated policy reserves the labeled worker: it owns NOTHING while
		// every unlabeled partition lands on the plain worker.
		require.Eventually(t, func() bool {
			return len(partitionsOf(vipWorker)) == 0 && len(partitionsOf(plainWorker)) == len(unlabeled)
		}, 30*time.Second, 200*time.Millisecond,
			"dedicated: labeled worker reserved (owns 0), plain worker owns every unlabeled partition")

		// Hold the reservation for a settle window so a late rebalance cannot
		// silently leak an unlabeled partition onto the reserved worker.
		require.Never(t, func() bool {
			return len(partitionsOf(vipWorker)) != 0
		}, 3*time.Second, 200*time.Millisecond, "reserved worker must stay empty under dedicated policy")
	})

	t.Run("shared_routes_to_all_workers", func(t *testing.T) {
		nc, cleanup := testutil.StartEmbeddedNATS(t)
		defer cleanup()
		js, err := jetstream.New(nc)
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()

		src := newKVSource(t, ctx, js, "label-policy-shared", unlabeled)

		cfg := testutil.IntegrationTestConfig()
		cfg.UnlabeledPartitionPolicy = "shared"

		cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
		vipWorker := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("vip"))
		plainWorker := cluster.AddWorkerWithOptions(ctx)
		t.Cleanup(cluster.StopWorkers)
		cluster.StartWorkers(ctx)
		cluster.WaitForStableState(20 * time.Second)

		// Shared policy routes unlabeled partitions to ALL workers, so both the
		// labeled and plain worker carry a share and the split is balanced.
		require.True(t, cluster.WaitForBalancedAssignments(len(unlabeled), 1, 30*time.Second),
			"shared: unlabeled partitions must balance across both workers")
		require.Eventually(t, func() bool {
			return len(partitionsOf(vipWorker)) > 0 && len(partitionsOf(plainWorker)) > 0
		}, 30*time.Second, 200*time.Millisecond,
			"shared: both the labeled and plain worker must own partitions")

		// Coverage: every unlabeled partition owned exactly once across the fleet.
		vip, plain := partitionsOf(vipWorker), partitionsOf(plainWorker)
		require.Equal(t, len(unlabeled), len(vip)+len(plain), "no duplicate or orphan under shared policy")
		for id := range vip {
			require.NotContains(t, plain, id, "partition %s owned by both workers", id)
		}
	})
}

// TestAllWorkersLabeled_UnlabeledPartitionsFallBack pins the spec §7 fallback
// ladder: when EVERY active worker is labeled and the source is entirely
// unlabeled under the dedicated policy, the unlabeled group's general pool is
// empty, so the ladder falls back to AllWorkers rather than orphaning the
// partitions. The IncrementUnlabeledFallback counter fires once PER unlabeled
// partition of each such rebalance, so the observed count reaches at least the
// partition count (it is monotonic and re-increments on any later rebalance —
// hence >=, not ==).
func TestAllWorkersLabeled_UnlabeledPartitionsFallBack(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	unlabeled := []types.Partition{
		{Keys: []string{"f-p0"}, Weight: 1},
		{Keys: []string{"f-p1"}, Weight: 1},
		{Keys: []string{"f-p2"}, Weight: 1},
		{Keys: []string{"f-p3"}, Weight: 1},
	}
	src := newKVSource(t, ctx, js, "label-fallback-partitions", unlabeled)

	cfg := testutil.IntegrationTestConfig() // dedicated

	// One shared recorder installed on every worker: only the current leader's
	// calculator emits the label counters, and we do not know which worker wins
	// leadership, so a shared thread-safe recorder captures whichever does.
	rec := newRecordingLabelMetrics()

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	// Every worker is labeled "vip" — the general (unlabeled) pool is empty.
	w0 := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("vip"), parti.WithMetrics(rec))
	w1 := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("vip"), parti.WithMetrics(rec))
	t.Cleanup(cluster.StopWorkers)
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(20 * time.Second)

	assignKV := openAssignmentKV(t, ctx, js, cfg)

	// Full coverage via the fallback ladder: every unlabeled partition is owned
	// exactly once across the all-labeled fleet, nothing parked.
	require.Eventually(t, func() bool {
		commit := readLatestCommit(ctx, assignKV)
		if commit == nil || commit.ParkedCount != 0 {
			return false
		}
		w0p, w1p := partitionsOf(w0), partitionsOf(w1)
		if len(w0p)+len(w1p) != len(unlabeled) {
			return false
		}
		for id := range w0p {
			if w1p[id] {
				return false
			}
		}

		return true
	}, 30*time.Second, 200*time.Millisecond,
		"all-labeled fleet must fully cover the unlabeled source via the fallback ladder, nothing parked")

	// The unlabeled-fallback counter must have fired at least once per unlabeled
	// partition (it is per-partition, and re-increments on any later rebalance).
	require.Eventually(t, func() bool {
		return rec.fallbacks() >= len(unlabeled)
	}, 15*time.Second, 100*time.Millisecond,
		"IncrementUnlabeledFallback must fire at least once per unlabeled partition routed to the fallback ladder")

	// Union coverage assertion for a precise failure message.
	assigned, ok := fetchAssignedIDs(ctx, assignKV, readLatestCommit(ctx, assignKV))
	require.True(t, ok)
	require.Equal(t, canonicalIDSet(unlabeled), assigned, "every unlabeled partition assigned exactly once")
}
