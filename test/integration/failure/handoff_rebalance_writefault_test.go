package failure_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestHandoffOnlyWriteFault_RebalancePreservesOldOwners(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}
	if os.Getenv("PARTI_RUN_HANDOFF_REBALANCE_PROOF") != "1" {
		t.Skip("set PARTI_RUN_HANDOFF_REBALANCE_PROOF=1 to run known failing proof")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)
	fc := &wfFaultController{}
	faultJS := newWFFaultJetStream(realJS, rfHandoffBucket, fc)

	rfBuildStream(t, ctx, realJS)

	pids := []string{
		"p0", "p1", "p2", "p3", "p4", "p5",
		"p6", "p7", "p8", "p9", "p10", "p11",
	}
	src := newWFMutableSource(wfPartitions(pids))

	stack0 := rfBuildWorkerStack(t, ctx, faultJS, src, 0)
	stack1 := rfBuildWorkerStack(t, ctx, faultJS, src, 1)
	stacks := make([]*rfWorkerStack, 0, 3)
	stacks = append(stacks, stack0, stack1)
	wfWaitStable(t, stacks, 30*time.Second)

	var preOwners map[string]string
	require.Eventually(t, func() bool {
		preOwners = wfAssignmentOwners(stacks)
		return len(preOwners) == len(pids) && wfClaimsMatchOwners(t, ctx, realJS, preOwners)
	}, 30*time.Second, 100*time.Millisecond, "two-worker baseline never committed stable claims")

	preCounts := wfExpectedCountsByWorker(preOwners)
	require.NotZero(t, preCounts[stack0.mgr.WorkerID()], "worker 0 must own baseline partitions")
	require.NotZero(t, preCounts[stack1.mgr.WorkerID()], "worker 1 must own baseline partitions")

	// C2 prerequisite (executable): M7 safety is only claimed for deployments
	// that fence the gaining worker with the processing gate. Assert the gate is
	// actually wired — gateWired flips true during the consumer update that
	// precedes Stable, so a baseline-stable worker that owns partitions must
	// report CapProcessingGate. Behavioral zero-new-consumption alone is NOT a
	// gate proof: a claim-write fault fails preparePhase before the consumer
	// updater runs, so the gaining worker would consume nothing regardless of the
	// gate. This guards against a future rfBuildWorkerStack change silently
	// dropping WithProcessingGate.
	require.NotZero(t, stack0.dyn.Capabilities()&types.CapProcessingGate,
		"worker 0 must report CapProcessingGate (processing gate wired)")
	require.NotZero(t, stack1.dyn.Capabilities()&types.CapProcessingGate,
		"worker 1 must report CapProcessingGate (processing gate wired)")

	fc.ArmWrites()
	stack2 := rfBuildWorkerStack(t, ctx, faultJS, src, 2)
	stacks = append(stacks, stack2)

	require.Eventually(t, func() bool { return fc.writeFaultsInjected.Load() >= 1 },
		30*time.Second, 25*time.Millisecond,
		"adding the third worker did not attempt a faulted handoff claim write")

	t.Logf("[%s] during handoff write fault: new worker state=%s assignment_parts=%d writeFaults=%d",
		t.Name(), stack2.mgr.State(), len(stack2.mgr.CurrentAssignment().Partitions), fc.writeFaultsInjected.Load())

	require.Never(t, func() bool {
		if stack2.mgr.State() == parti.StateStable {
			return true
		}
		return !wfClaimsMatchOwners(t, ctx, realJS, preOwners)
	}, 3*time.Second, 100*time.Millisecond,
		"handoff write failures must not report Stable or rewrite stable owners")

	before0 := stack0.consumed.Load()
	before1 := stack1.consumed.Load()
	before2 := stack2.consumed.Load()
	for _, pid := range pids {
		rfPublish(t, ctx, realJS, pid, "during-"+pid)
	}
	require.Eventuallyf(t, func() bool {
		return stack0.consumed.Load() >= before0+preCounts[stack0.mgr.WorkerID()] &&
			stack1.consumed.Load() >= before1+preCounts[stack1.mgr.WorkerID()] &&
			stack2.consumed.Load() == before2
	}, 30*time.Second, 100*time.Millisecond,
		"old owners must keep consuming their pre-fault partitions while new ownership is uncommitted; "+
			"want old deltas=(%d,%d) got=(%d,%d) new_delta=%d preOwners=%v",
		preCounts[stack0.mgr.WorkerID()], preCounts[stack1.mgr.WorkerID()],
		stack0.consumed.Load()-before0, stack1.consumed.Load()-before1,
		stack2.consumed.Load()-before2, preOwners)

	fc.DisarmWrites()

	require.Eventually(t, func() bool {
		if err := wfAllStable(stacks, 0); err != nil {
			return false
		}
		owners := wfAssignmentOwners(stacks)
		return len(owners) == len(pids) &&
			len(stack2.mgr.CurrentAssignment().Partitions) > 0 &&
			wfClaimsMatchOwners(t, ctx, realJS, owners)
	}, 45*time.Second, 100*time.Millisecond,
		"after handoff writes recover, the rebalance must commit and expose the new worker")

	// The gaining worker has now applied the committed rebalance, so its gate is
	// wired too — completing the C2 assertion for the worker that actually
	// received the transferred partitions.
	require.NotZero(t, stack2.dyn.Capabilities()&types.CapProcessingGate,
		"gaining worker must report CapProcessingGate after applying the committed rebalance")

	baseTotal := wfConsumedTotal(stacks)
	for _, pid := range pids {
		rfPublish(t, ctx, realJS, pid, "after-"+pid)
	}
	require.Eventually(t, func() bool {
		return wfConsumedTotal(stacks) >= baseTotal+int64(len(pids))
	}, 30*time.Second, 100*time.Millisecond,
		"all partitions must resume consumption after rebalance commit")
}

func wfPartitions(pids []string) []types.Partition {
	parts := make([]types.Partition, len(pids))
	for i, pid := range pids {
		parts[i] = types.Partition{Keys: []string{pid}}
	}

	return parts
}

func wfWaitStable(t *testing.T, stacks []*rfWorkerStack, timeout time.Duration) {
	t.Helper()
	for i, s := range stacks {
		require.NoErrorf(t, <-s.mgr.WaitState(parti.StateStable, timeout),
			"worker %d did not reach Stable", i)
	}
}

func wfAllStable(stacks []*rfWorkerStack, timeout time.Duration) error {
	for _, s := range stacks {
		if s.mgr.State() == parti.StateStable {
			continue
		}
		if timeout <= 0 {
			return context.DeadlineExceeded
		}
		if err := <-s.mgr.WaitState(parti.StateStable, timeout); err != nil {
			return err
		}
	}

	return nil
}

func wfAssignmentOwners(stacks []*rfWorkerStack) map[string]string {
	owners := make(map[string]string)
	for _, s := range stacks {
		workerID := s.mgr.WorkerID()
		for _, p := range s.mgr.CurrentAssignment().Partitions {
			owners[p.ID()] = workerID
		}
	}

	return owners
}

func wfExpectedCountsByWorker(owners map[string]string) map[string]int64 {
	counts := make(map[string]int64)
	for _, workerID := range owners {
		counts[workerID]++
	}

	return counts
}

func wfClaimsMatchOwners(t *testing.T, ctx context.Context, realJS jetstream.JetStream, owners map[string]string) bool {
	t.Helper()
	for pid, owner := range owners {
		claimOwner, stable := wfReadStableClaimOwner(t, ctx, realJS, pid)
		if !stable || claimOwner != owner {
			return false
		}
	}

	return true
}

func wfReadStableClaimOwner(t *testing.T, ctx context.Context, realJS jetstream.JetStream, pid string) (string, bool) {
	t.Helper()
	kv, err := realJS.KeyValue(ctx, rfHandoffBucket)
	require.NoError(t, err)
	entry, err := kv.Get(ctx, rfClaimsPrefix+pid)
	if err != nil {
		return "", false
	}
	cl, err := handoff.UnmarshalClaim(entry.Value())
	require.NoError(t, err)

	return cl.Owner, cl.State == handoff.ClaimStateStable
}

func wfConsumedTotal(stacks []*rfWorkerStack) int64 {
	var total int64
	for _, s := range stacks {
		total += s.consumed.Load()
	}

	return total
}
