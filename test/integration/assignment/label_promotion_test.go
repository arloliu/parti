package assignment_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/partcodec"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// partitionsOf returns the CanonicalID set of a manager's current assignment.
func partitionsOf(m *parti.Manager) map[string]bool {
	out := map[string]bool{}
	for _, p := range m.CurrentAssignment().Partitions {
		out[p.CanonicalID()] = true
	}

	return out
}

// TestLabelPromotion_EndToEnd is the user-priority end-to-end proof that a
// label-only rewrite (same keys, same weights, new label) pushed through the
// production update path flows KV source watch → leader rebalance → new
// assignments on live managers. Task 2 proved source-level propagation in
// isolation; this proves the whole system converges.
//
// The rewrites in Phase 1/2 go through a RAW kv.Put (out-of-band), NOT
// src.Update. NatsKV.Update refreshes its local cache and notifies its own
// listeners directly (without the KV watcher round trip), so a same-instance
// Update would let this test pass even with the watch path broken. A raw
// kv.Put is exactly what an external operator/writer process does, and the
// manager-owned source can only learn about it through its WATCHER — the
// propagation path this test exists to pin.
func TestLabelPromotion_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Generous overall budget: the phased Eventually windows below can sum to
	// well over 100s under CI load, and every NATS KV op (Put/Update/Get) is
	// bounded by this ctx. Kept comfortably under the 300s test -timeout.
	ctx, cancel := context.WithTimeout(t.Context(), 280*time.Second)
	defer cancel()

	// Production-shaped source: partition list in a KV bucket, learned via a
	// live watcher + reconcile loop.
	srcKV, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "label-e2e-partitions"})
	require.NoError(t, err)
	initial := []types.Partition{
		{Keys: []string{"p0"}, Weight: 1},
		{Keys: []string{"p1"}, Weight: 1},
		{Keys: []string{"p2"}, Weight: 1},
		{Keys: []string{"p3"}, Weight: 1},
	}
	src := source.NewNatsKV(srcKV, "partitions", nil)
	require.NoError(t, src.Update(ctx, initial)) // seeds the bucket
	require.NoError(t, src.Start(ctx))           // watch + reconcile loops
	t.Cleanup(func() { _ = src.Stop(context.Background()) })

	cfg := testutil.IntegrationTestConfig()
	cfg.LabelSpillGrace = 5 * time.Second

	// NewWorkerClusterWithSource wires the shared source + config and defaults
	// the strategy to consistent-hash (same construction the manager E2E
	// invariant test uses). Its config parameter fits our LabelSpillGrace tweak,
	// so it is preferred over a hand-rolled WorkerCluster literal.
	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	vipWorker := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerLabels("vip"))
	plainWorker := cluster.AddWorkerWithOptions(ctx)
	t.Cleanup(cluster.StopWorkers)
	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(20 * time.Second)

	p0 := types.Partition{Keys: []string{"p0"}}.CanonicalID()

	// Phase 0 — dedicated reservation: no labeled partitions yet, so the vip
	// worker idles and the plain worker owns everything (spec §7 dedicated
	// policy reserves labeled workers for labeled partitions only).
	require.Eventually(t, func() bool {
		return len(partitionsOf(plainWorker)) == 4 && len(partitionsOf(vipWorker)) == 0
	}, 20*time.Second, 200*time.Millisecond,
		"dedicated policy must reserve the labeled worker")

	// Phase 1 — PROMOTION: rewrite the full list with ONLY p0's label changed
	// (same keys, same weights), OUT-OF-BAND via a raw KV write (see the
	// function doc for why this must not be src.Update).
	promoted := []types.Partition{
		{Keys: []string{"p0"}, Weight: 1, Label: "vip"}, // <- the only delta
		{Keys: []string{"p1"}, Weight: 1},
		{Keys: []string{"p2"}, Weight: 1},
		{Keys: []string{"p3"}, Weight: 1},
	}
	encoded, err := partcodec.Encode(promoted)
	require.NoError(t, err)
	_, err = srcKV.Put(ctx, "partitions", encoded)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return partitionsOf(vipWorker)[p0] && !partitionsOf(plainWorker)[p0]
	}, 30*time.Second, 200*time.Millisecond,
		"label-only promotion must move p0 to the vip worker: KV watch → rebalance → assignment")

	// Coverage invariant across the move: every partition owned exactly once.
	require.Eventually(t, func() bool {
		vip, plain := partitionsOf(vipWorker), partitionsOf(plainWorker)
		if len(vip)+len(plain) != 4 {
			return false
		}
		for id := range vip {
			if plain[id] {
				return false
			}
		}

		return true
	}, 20*time.Second, 200*time.Millisecond, "no orphan, no duplicate during promotion")

	// Phase 2 — DEMOTION: label-only rewrite back, same out-of-band path. Under
	// dedicated policy p0 must return to the plain worker and the vip worker
	// must drain to empty (its KV assignment updated, not stale).
	encoded, err = partcodec.Encode(initial)
	require.NoError(t, err)
	_, err = srcKV.Put(ctx, "partitions", encoded)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return !partitionsOf(vipWorker)[p0] && partitionsOf(plainWorker)[p0] &&
			len(partitionsOf(vipWorker)) == 0
	}, 30*time.Second, 200*time.Millisecond,
		"label-only demotion must move p0 back and drain the reserved worker")
}
