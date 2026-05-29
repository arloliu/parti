package handoff

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// TestTwoPhase_CommitPhase_DoesNotHijackStableForeignClaim reproduces the
// stuck-commit orphan observed under concurrent rebalance churn
// (TestHandoffConflictStress, ~1/30 under CPU load).
//
// Mechanism: during rapid rebalancing, a worker B's local assignment can
// transiently include a partition that another worker A actually owns
// (B's calculator version lags or disagrees with the durable claim). Because
// the partition is in B's `previous` AND `next`, preparePhase treats it as
// already-held and does NOT run a prepare (no pendingOwner=B is recorded).
// commitPhase then iterates ALL of B's next partitions and hits the
// `cur.Owner != workerID` branch with an EMPTY pending owner: NextCommit keeps
// owner=A (no pending to promote) but flips state stable -> commit. B's
// stabilizePhase then skips the claim (cur.Owner != B), orphaning A's claim in
// `commit`. A is not running an Apply for this partition (it already owns it
// stably), so nothing finalizes it back to stable until the TTL-expiry sweep
// (~minutes later). The pull gate suppresses the partition with
// state_not_allowed(commit) for that entire window.
//
// Correct behavior: a worker must not transition a claim it does not own into
// commit unless there is an actual handoff pending TO it
// (cur.PendingOwner == workerID). Committing a stable, foreign-owned claim with
// no pending handoff is spurious and must be a no-op.
//
// On the unfixed code this test FAILS: post-Apply the claim is {owner=A,
// state=commit}. On the fixed code it PASSES: B's spurious commit is a no-op
// and the claim stays {owner=A, state=stable}.
func TestTwoPhase_CommitPhase_DoesNotHijackStableForeignClaim(t *testing.T) {
	t.Parallel()

	const (
		workerA = "worker-A"
		workerB = "worker-B"
		pid     = "p-foreign-stable"
	)

	store := newMemStore()

	// A owns the partition, cleanly stable. LastUpdated=now so the opportunistic
	// sweep cannot mask the bug (only a transition path can change the claim).
	owned := Claim{
		PartitionID: pid,
		Owner:       workerA,
		State:       ClaimStateStable,
		Epoch:       5,
		LastUpdated: time.Now().UTC(),
		TTLSeconds:  int64((2 * time.Minute).Seconds()),
	}
	store.data[pid] = owned
	store.rev[pid] = 5

	cfg := Config{
		ConsumerUpdater: nopUpdater{},
		Store:           store,
		Now:             time.Now,
		TTL:             2 * time.Minute,
		SweepInterval:   0,
		MaxRetries:      3,
		BaseBackoff:     1 * time.Millisecond,
		MaxBackoff:      5 * time.Millisecond,
	}
	coord := New(cfg, true)

	// B's local assignment transiently includes pid in BOTH previous and next
	// (B believes it already holds pid), so preparePhase skips it as
	// already-held and commitPhase runs straight against A's stable claim.
	prev := types.Assignment{Version: 7, Lifecycle: "stable", Partitions: []types.Partition{{Keys: []string{pid}}}}
	next := types.Assignment{Version: 8, Lifecycle: "stable", Partitions: []types.Partition{{Keys: []string{pid}}}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, coord.Apply(ctx, workerB, prev, next))

	got, _, err := store.Get(ctx, pid)
	require.NoError(t, err)

	require.Equalf(t, ClaimStateStable, got.State,
		"worker B must not hijack A's stable claim into commit. Got state=%s owner=%s pending=%q. "+
			"This is the stuck-commit orphan: commitPhase's cur.Owner != workerID branch "+
			"calls NextCommit on a foreign stable claim with no pending owner, flipping it to "+
			"commit; B's stabilizePhase then skips it (not its claim), leaving A's claim "+
			"orphaned in commit until the TTL sweep.",
		got.State, got.Owner, got.PendingOwner)
	require.Equal(t, workerA, got.Owner,
		"A must remain the owner; B has no pending handoff to it")
	require.Empty(t, got.PendingOwner)
	require.Equal(t, owned.Epoch, got.Epoch,
		"a spurious foreign commit must be a no-op and not advance the epoch")
}
