package handoff_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// nopUpdater is a no-op WorkerConsumerUpdater to drive the two-phase path without external side effects.
type nopUpdater2 struct{}

func (nopUpdater2) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []parti.Partition) error {
	return nil
}

// makePartitions creates n simple single-key partitions.
func makePartitions(n int) []parti.Partition {
	out := make([]parti.Partition, n)
	for i := range n {
		out[i] = parti.Partition{Keys: []string{fmt.Sprintf("p%02d", i)}}
	}
	return out
}

// TestHandoffConflictStress spins two managers and performs rapid partition set changes to
// exercise concurrent handoff claim updates. It asserts eventual convergence to stable claims
// with no pending owners and ownership by one of the active workers.
func TestHandoffConflictStress(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}

	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := fmt.Sprintf("itest-handoff-stress-%d", time.Now().UnixNano())

	// Shared dynamic source across managers
	src := source.NewStatic(makePartitions(8))
	strategy := strategy.NewConsistentHash()
	upd := nopUpdater2{}

	// Manager config with two-phase enabled, jitter/backoff tuned to surface CAS races
	baseCfg := parti.TestConfig()
	baseCfg.EnableTwoPhaseHandoff = true
	baseCfg.KVBuckets.HandoffBucket = bucket
	baseCfg.KVBuckets.HandoffTTL = 2 * time.Minute
	baseCfg.Handoff.MaxRetries = 3
	baseCfg.Handoff.BaseBackoff = 10 * time.Millisecond
	baseCfg.Handoff.MaxBackoff = 50 * time.Millisecond
	baseCfg.Handoff.Jitter = 0.5
	baseCfg.Handoff.SweepInterval = 0 // allow opportunistic sweeps per-apply

	// Start two managers
	mr := testutil.NewHandoffMetricsRecorder()
	m1, err := parti.NewManager(&baseCfg, js, src, strategy,
		parti.WithWorkerConsumerUpdater(upd),
		parti.WithHandoffMetricsRecorder(mr),
	)
	require.NoError(t, err)
	require.NoError(t, m1.Start(ctx))
	t.Cleanup(func() { _ = m1.Stop(context.Background()) })

	m2, err := parti.NewManager(&baseCfg, js, src, strategy,
		parti.WithWorkerConsumerUpdater(upd),
		parti.WithHandoffMetricsRecorder(mr),
	)
	require.NoError(t, err)
	require.NoError(t, m2.Start(ctx))
	t.Cleanup(func() { _ = m2.Stop(context.Background()) })

	// Wait for initial assignments to stabilize (bounded wait loop instead of fixed sleep)
	// Use KV watcher-based collector for deterministic stabilization
	collector, err := testutil.NewHandoffClaimCollector(ctx, js, bucket)
	require.NoError(t, err)
	t.Cleanup(func() { collector.Stop() })
	// Initial convergence: wait until at least one claim enters stable state.
	// Since partition IDs are unknown until claims appear, poll for any stable.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		claims, _ := parti.InspectHandoffClaims(ctx, js, bucket)
		stableSeen := false
		for _, c := range claims {
			if c.State == parti.HandoffClaimStable {
				stableSeen = true
				break
			}
		}
		if stableSeen {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Rapidly update partitions to induce rebalances; use adaptive stabilization loop.
	// Metrics recorder has been injected via options; continue churn and let conflicts accumulate.
	for i := range 20 {
		if i%2 == 0 {
			_ = src.Update(ctx, makePartitions(10))
		} else {
			_ = src.Update(ctx, makePartitions(8))
		}
		_ = m1.RefreshPartitions(ctx)
		_ = m2.RefreshPartitions(ctx)
		// Proceed to next churn iteration; final stabilization is asserted later
	}

	// Final stabilization after churn
	curParts, err := src.List(ctx)
	require.NoError(t, err)
	ids := make([]string, 0, len(curParts))
	for _, p := range curParts {
		ids = append(ids, p.ID())
	}
	// Allow generous time due to churn and potential lingering prepare/commit windows.
	// The wait polls authoritative KV state, so this deadline only bounds genuine
	// convergence-under-load, not watcher-delivery lag; it returns as soon as KV is
	// all-stable, so a large bound is near-free in the happy path.
	require.True(t, collector.WaitForAllStable(ctx, ids, 15*time.Second))

	// Inspect claims: current partitions must be stable; extras (from earlier expansions) must also be stable
	claims, err := parti.InspectHandoffClaims(ctx, js, bucket)
	require.NoError(t, err)
	// Build current partition set
	curParts, err = src.List(ctx)
	require.NoError(t, err)
	curSet := make(map[string]struct{}, len(curParts))
	for _, p := range curParts {
		curSet[p.ID()] = struct{}{}
	}
	owners := map[string]struct{}{m1.WorkerID(): {}, m2.WorkerID(): {}}
	for _, c := range claims {
		require.Equal(t, parti.HandoffClaimStable, c.State, "claims should converge to stable")
		require.Empty(t, c.PendingOwner, "no pending owner after stabilization")
		if _, ok := owners[c.Owner]; !ok {
			t.Fatalf("unexpected owner %q; expected one of %v", c.Owner, owners)
		}
	}
	// Ensure every current partition has a claim present
	present := make(map[string]bool, len(claims))
	for _, c := range claims {
		present[c.PartitionID] = true
	}
	for pid := range curSet {
		require.True(t, present[pid], "current partition %s must have a claim", pid)
	}

	// Observe CAS conflicts during churn (informational). Depending on timing and a single active leader,
	// it's possible to see zero conflicts; we record the value but do not fail the test on zero.
	snap := mr.Snapshot()
	t.Logf("CAS conflicts observed: %d", snap.CASConflicts)

	// Phase 4 makes the initial-bootstrap apply synchronous-before-StateStable,
	// so the initial prepare → commit transitions complete before the
	// collector (started after Start returns) can observe them, and any
	// partition that doesn't migrate during churn remains invisible to
	// the collector. The convergence assertion now uses InspectHandoffClaims
	// (KV query, sees all current state) rather than the collector's
	// updates-only feed. The intermediate-state observability invariant
	// is covered by unit tests against handleCommitValue's case-(c) path.
	finalClaims, err := parti.InspectHandoffClaims(ctx, js, bucket)
	require.NoError(t, err)
	claimsByPID := make(map[string]parti.HandoffClaim, len(finalClaims))
	for _, c := range finalClaims {
		claimsByPID[c.PartitionID] = c
	}
	for pid := range curSet {
		cl, ok := claimsByPID[pid]
		require.True(t, ok, "partition %s must have a claim", pid)
		require.Equal(t, parti.HandoffClaimStable, cl.State, "partition %s must terminate in Stable", pid)
		require.Empty(t, cl.PendingOwner, "partition %s must not have a pending owner", pid)
	}
}
