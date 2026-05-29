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

	// Wait for initial assignments to stabilize (bounded wait loop instead of fixed sleep).
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

	// Final stabilization after churn: poll until every current partition has a
	// claim and ALL claims (current partitions plus any extras left over from the
	// expansion-to-10 rounds) are stable with no pending owner, owned by an active
	// worker. Convergence is eventual — under parallel load it settles within a
	// few hundred ms — so poll the full invariant rather than snapshotting once
	// after a fixed wait, which races a still-settling claim.
	curParts, err := src.List(ctx)
	require.NoError(t, err)
	curSet := make(map[string]struct{}, len(curParts))
	for _, p := range curParts {
		curSet[p.ID()] = struct{}{}
	}
	owners := map[string]struct{}{m1.WorkerID(): {}, m2.WorkerID(): {}}

	var claims []parti.HandoffClaim
	require.Eventually(t, func() bool {
		var ierr error
		claims, ierr = parti.InspectHandoffClaims(ctx, js, bucket)
		if ierr != nil {
			return false
		}
		present := make(map[string]bool, len(claims))
		for _, c := range claims {
			if c.State != parti.HandoffClaimStable || c.PendingOwner != "" {
				return false
			}
			if _, ok := owners[c.Owner]; !ok {
				return false
			}
			present[c.PartitionID] = true
		}
		for pid := range curSet {
			if !present[pid] {
				return false
			}
		}

		return true
	}, 15*time.Second, 100*time.Millisecond,
		"all handoff claims must converge to stable (no pending owner) with every current partition present")

	// Observe CAS conflicts during churn (informational). Depending on timing and
	// a single active leader, it's possible to see zero conflicts; we record the
	// value but do not fail the test on zero.
	snap := mr.Snapshot()
	t.Logf("CAS conflicts observed: %d", snap.CASConflicts)
}
