package failure_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestHandoffRemovalGuard_NoRaceUnderConcurrentApplies is the concurrency
// stress regression pin required by AGENTS.md for the handoff removal guard.
//
// The guard (Manager.guardHandoffRemoval) adds claim-store + assignment-commit
// KV reads INSIDE the apply critical section: currentCommitPartitionSet reads
// `assignment._commit` and each referenced `assignment._payload.*` via the
// shared m.assignmentKV handle on every guarded apply that removes a partition.
// Those reads run concurrently with the manager's assignment-change watcher and
// commit watcher operating on the SAME buckets — and this repo has a documented
// history of nats.go cached *stream concurrency races under live load
// (manager_epoch_monitor_concurrency_test.go). The guardHandoffRemoval unit
// tests exercise the decision logic in isolation and cannot find a race between
// the guard's KV reads and the production watcher goroutines.
//
// Mechanism:
//   - Start a real 3-worker two-phase cluster (rfBuildWorkerStack wires
//     EnableTwoPhaseHandoff + the processing gate, so the guard is live).
//   - Churn the partition set between a full and a reduced set every ~150ms for
//     ~5s. Each reduction forces the leader to republish an assignment that
//     removes partitions, so followers apply with len(removed) > 0 and the guard
//     fans out its commit/payload KV reads while the watchers are active.
//   - Assert no race-detector trigger (t.Failed()) and that the cluster stays
//     live (re-converges to Stable, consumption keeps progressing).
//
// Scope: partition-set churn over a FIXED worker set produces GLOBAL removals,
// so this pin races the guard's commit/payload reads on m.assignmentKV
// (currentCommitPartitionSet) against the watchers. The guard's per-partition
// claim-store Get is reached only for TRANSFER removals (partition still in the
// commit batch) and is raced under -race by
// TestHandoffOnlyWriteFault_RebalancePreservesOldOwners, which grows the worker
// set to force real transfers. Together the two tests cover both read surfaces.
//
// Run under `go test -race` (the make pre-pr integration gate) to be meaningful.
func TestHandoffRemovalGuard_NoRaceUnderConcurrentApplies(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	t.Parallel()

	// 120s parent budget (matching the sibling write-fault proof) must exceed the
	// worst-case sum of the sequential phase budgets below (30s wait + 5s soak +
	// 45s re-converge + 30s consumption = 110s); a tighter ctx could fire
	// mid-phase as a hard error under full-suite -race CPU load.
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)
	rfBuildStream(t, ctx, realJS)

	fullPIDs := []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "p10", "p11"}
	reducedPIDs := fullPIDs[:8] // dropping 4 partitions forces removals on republish

	// Each worker gets its own watchable source instance so that whichever worker
	// wins leadership reliably receives the re-list signal (wfMutableSource.Watch
	// returns a single per-instance channel; sharing one instance across workers
	// would let a follower drain the leader's signal).
	const workerCount = 3
	sources := make([]*wfMutableSource, workerCount)
	stacks := make([]*rfWorkerStack, workerCount)
	for i := range workerCount {
		sources[i] = newWFMutableSource(wfPartitions(fullPIDs))
		stacks[i] = rfBuildWorkerStack(t, ctx, realJS, sources[i], i)
	}
	wfWaitStable(t, stacks, 30*time.Second)

	maxVersion := func() int64 {
		var v int64
		for _, s := range stacks {
			if cv := s.mgr.CurrentAssignment().Version; cv > v {
				v = cv
			}
		}

		return v
	}
	initialVersion := maxVersion()

	setAll := func(pids []string) {
		parts := wfPartitions(pids)
		for _, s := range sources {
			s.set(parts)
		}
	}

	// Soak: alternate the partition set to drive a steady stream of guarded
	// applies (with removals on every step down) concurrent with the assignment
	// and commit watchers. Publish along the way so the consumer pull path adds
	// further concurrent KV traffic against the same connection.
	soak, soakCancel := context.WithTimeout(ctx, 5*time.Second)
	defer soakCancel()
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	reduced := false
	pub := 0
	for {
		stop := false
		select {
		case <-soak.Done():
			stop = true
		case <-ticker.C:
			reduced = !reduced
			if reduced {
				setAll(reducedPIDs)
			} else {
				setAll(fullPIDs)
			}
			rfPublish(t, ctx, realJS, fullPIDs[pub%len(fullPIDs)], "soak")
			pub++
		}
		if stop {
			break
		}
	}

	// Non-vacuity: the churn must have driven real rebalances (and thus guarded
	// applies), not silently no-op'd — the assignment version must have advanced.
	require.Greater(t, maxVersion(), initialVersion,
		"partition-set churn must advance the assignment version (guarded applies actually fired)")

	// Settle on the full set and require the cluster to re-converge — proves the
	// guarded-apply churn did not wedge the fleet, only stressed the race window.
	setAll(fullPIDs)
	require.Eventually(t, func() bool {
		return wfAllStable(stacks, 0) == nil &&
			len(wfAssignmentOwners(stacks)) == len(fullPIDs)
	}, 45*time.Second, 200*time.Millisecond,
		"cluster must re-converge to a full stable assignment after the guarded-apply soak")

	// Liveness sanity: consumption still flows after the soak.
	baseTotal := wfConsumedTotal(stacks)
	for _, pid := range fullPIDs {
		rfPublish(t, ctx, realJS, pid, "post-soak-"+pid)
	}
	require.Eventually(t, func() bool {
		return wfConsumedTotal(stacks) >= baseTotal+int64(len(fullPIDs))
	}, 30*time.Second, 100*time.Millisecond,
		"all partitions must resume consumption after the guarded-apply soak")

	require.False(t, t.Failed(),
		"race detector or a sub-assertion failed during the guarded-apply soak; "+
			"check stderr for WARNING: DATA RACE — the guard's commit/payload KV reads "+
			"share m.assignmentKV with the assignment and commit watcher goroutines")
}
