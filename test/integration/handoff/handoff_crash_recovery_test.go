package handoff_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/internal/testutil"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// nopUpdater is a no-op WorkerConsumerUpdater to enable the two-phase coordinator path.
type nopUpdater struct{}

func (nopUpdater) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []parti.Partition) error {
	return nil
}

// waitForPartitionClaim polls InspectHandoffClaims until a claim for the partition ID
// satisfies the provided predicate or the context expires.
func waitForPartitionClaim(ctx context.Context, js jetstream.JetStream, bucket, pid string, pred func(parti.HandoffClaim) bool) (parti.HandoffClaim, bool) {
	deadline, _ := ctx.Deadline()
	for time.Now().Before(deadline) {
		claims, err := parti.InspectHandoffClaims(ctx, js, bucket)
		if err == nil {
			for _, c := range claims {
				if c.PartitionID == pid && pred(c) {
					return c, true
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	return parti.HandoffClaim{}, false
}

// TestHandoffCrashRecovery_PrepareInFlight simulates a leader crash after writing a prepare claim.
// We seed a non-expired prepare claim with pendingOwner set to the new worker and start the manager.
// Expect: prepare -> commit -> stable sequence with epoch monotonicity (+2) and ownership updated.
func TestHandoffCrashRecovery_PrepareInFlight(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}

	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := fmt.Sprintf("itest-handoff-crash-prepare-%d", time.Now().UnixNano())
	partition := parti.Partition{Keys: []string{"pcrash"}}
	pid := partition.ID()

	// Seed a prepare claim owned by worker-0 with pendingOwner worker-1 (future new worker).
	seedEpoch := int64(5)
	claim := parti.HandoffClaim{
		PartitionID:  pid,
		Owner:        "worker-0",
		PendingOwner: "worker-1",
		State:        parti.HandoffClaimPrepare,
		Epoch:        seedEpoch,
		LastUpdated:  time.Now().UTC(),
		TTLSeconds:   int64((2 * time.Minute).Seconds()),
	}
	err = testutil.SeedHandoffClaim(ctx, js, bucket, claim, 2*time.Minute)
	require.NoError(t, err)

	cfg := parti.TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = bucket
	cfg.KVBuckets.HandoffTTL = 2 * time.Minute
	// Force manager to avoid worker-0 so it claims worker-1 (min=1 ensures different from 0 when possible).
	cfg.WorkerIDMin = 1
	cfg.WorkerIDMax = 2
	// Add delay to expose commit state before stabilization.
	cfg.Handoff.DelayBeforeStable = 400 * time.Millisecond

	src := source.NewStatic([]parti.Partition{partition})
	strategy := strategy.NewConsistentHash()

	mgr, err := parti.NewManager(&cfg, js, src, strategy, parti.WithWorkerConsumerUpdater(nopUpdater{}))
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	// Wait for commit state (epoch should be seedEpoch+2: one increment from prepare re-write, one from commit).
	commitCtx, cancelCommit := context.WithTimeout(ctx, 3*time.Second)
	defer cancelCommit()
	commitClaim, ok := waitForPartitionClaim(commitCtx, js, bucket, pid, func(c parti.HandoffClaim) bool { return c.State == parti.HandoffClaimCommit })
	require.True(t, ok, "commit state not observed")
	require.Equal(t, seedEpoch+2, commitClaim.Epoch, "epoch must reflect prepare+commit increments")
	require.Equal(t, mgr.WorkerID(), commitClaim.Owner, "owner should switch at commit phase")
	require.Empty(t, commitClaim.PendingOwner)

	// Wait for stable state (epoch should be seedEpoch+3: prepare+commit+stable).
	stableCtx, cancelStable := context.WithTimeout(ctx, 4*time.Second)
	defer cancelStable()
	finalClaim, ok := waitForPartitionClaim(stableCtx, js, bucket, pid, func(c parti.HandoffClaim) bool { return c.State == parti.HandoffClaimStable })
	require.True(t, ok, "stable state not observed")
	require.Equal(t, seedEpoch+3, finalClaim.Epoch, "epoch must increment again on stabilization")
	require.Equal(t, mgr.WorkerID(), finalClaim.Owner)
}

// TestHandoffCrashRecovery_CommitInFlight simulates a crash after commit but before stabilization.
// We seed a commit claim owned by the future worker (no pendingOwner). Manager resume logic
// should finalize commit->stable quickly with a single epoch increment.
func TestHandoffCrashRecovery_CommitInFlight(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}

	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := fmt.Sprintf("itest-handoff-crash-commit-%d", time.Now().UnixNano())
	partition := parti.Partition{Keys: []string{"pcrash2"}}
	pid := partition.ID()

	// Seed a commit claim where owner is worker-1 already (post commit) awaiting stabilization.
	seedEpoch := int64(11)
	commitClaim := parti.HandoffClaim{
		PartitionID: pid,
		Owner:       "worker-1",
		State:       parti.HandoffClaimCommit,
		Epoch:       seedEpoch,
		LastUpdated: time.Now().UTC(),
		TTLSeconds:  int64((2 * time.Minute).Seconds()),
	}
	err = testutil.SeedHandoffClaim(ctx, js, bucket, commitClaim, 2*time.Minute)
	require.NoError(t, err)

	cfg := parti.TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = bucket
	cfg.KVBuckets.HandoffTTL = 2 * time.Minute
	// Ensure we claim worker-1 (min=1) matching commit claim owner.
	cfg.WorkerIDMin = 1
	cfg.WorkerIDMax = 2

	src := source.NewStatic([]parti.Partition{partition})
	strategy := strategy.NewConsistentHash()

	mgr, err := parti.NewManager(&cfg, js, src, strategy, parti.WithWorkerConsumerUpdater(nopUpdater{}))
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	// Poll for stable finalization (resume pass runs async right after initial Apply).
	stableCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	finalClaim, ok := waitForPartitionClaim(stableCtx, js, bucket, pid, func(c parti.HandoffClaim) bool { return c.State == parti.HandoffClaimStable })
	require.True(t, ok, "stable state not observed after resume")
	require.Equal(t, seedEpoch+1, finalClaim.Epoch, "epoch must increment exactly once from commit to stable")
	require.Equal(t, mgr.WorkerID(), finalClaim.Owner)
}
