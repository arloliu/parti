package manager_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
)

// fleetObserverUpdater is a test WorkerConsumerUpdater that ALSO implements the
// optional parti.FleetSizeObserver interface. UpdateWorkerConsumer is a no-op
// (this test does not exercise consumer reconciliation); ObserveWorkerCount
// records every cluster worker-count (N) the Manager pushes to it.
//
// This is the observation seam for the fleet-size-aware rate-limit feature: the
// integration package is external (package manager_test) and cannot read the
// Manager's unexported fleet-size fields. By registering this updater with
// parti.WithWorkerConsumerUpdater, the test observes the FULL chain end-to-end
// (commit watcher decode → observeFleetSize freshness fence → ObserveWorkerCount
// push) purely through the recorded N values, with no manager internals and no
// exported test accessor.
//
// All access is mutex-guarded because ObserveWorkerCount runs on the Manager's
// commit-watch goroutine while the test reads from the test goroutine.
type fleetObserverUpdater struct {
	mu   sync.Mutex
	last int
	seen []int
}

// UpdateWorkerConsumer satisfies parti.WorkerConsumerUpdater. It is a no-op: this
// test does not assert on consumer subject reconciliation, only on the fleet-size
// observation that piggybacks on the same updater seam.
func (u *fleetObserverUpdater) UpdateWorkerConsumer(context.Context, string, []parti.Partition) error {
	return nil
}

// ObserveWorkerCount satisfies parti.FleetSizeObserver. The Manager calls it
// whenever the observed committed worker-set size changes (and once at startup,
// before the first apply). n is always >= 1 by contract.
func (u *fleetObserverUpdater) ObserveWorkerCount(n int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.last = n
	u.seen = append(u.seen, n)
}

// lastN returns the most recently observed worker-count (0 if none yet).
func (u *fleetObserverUpdater) lastN() int {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.last
}

// Compile-time assertions: the test updater must satisfy BOTH interfaces, else
// the Manager would never cast it to a FleetSizeObserver and the wiring proof
// would silently pass for the wrong reason.
var (
	_ parti.WorkerConsumerUpdater = (*fleetObserverUpdater)(nil)
	_ parti.FleetSizeObserver     = (*fleetObserverUpdater)(nil)
)

// TestAdaptive_FleetGrowthIsObserved is the end-to-end wiring proof for the
// fleet-size-aware rate-limit feature: a real fleet-size change that a worker
// observes through its commit watcher actually reaches ObserveWorkerCount.
//
// Mechanism under test (manager_assignment.go observeFleetSize, wired into the
// commit-watch onUpdate at the pre-debounce decode point, plus the startup
// bootstrap call in manager.go): the Manager records len(commit.Workers) from
// every committed assignment it decodes and, when the value changes, pushes the
// new N to a registered WorkerConsumerUpdater that implements FleetSizeObserver.
//
// Scenario:
//   - Start worker-1 alone with a fleetObserverUpdater wired in. Wait for Stable.
//   - Assert worker-1 eventually observes N=1 (it sees itself as the sole worker
//     in the committed assignment it bootstraps from).
//   - Start worker-2 joining the same coordination buckets. The leader
//     republishes a commit whose Workers slice now has 2 entries.
//   - Assert worker-1 eventually observes N=2, proving it picked up the fleet
//     growth end-to-end through the commit watcher — not just at its own startup.
//   - Clean shutdown of both.
//
// require.Eventually (not a fixed sleep) is used for both observations: commit
// propagation plus the 30s reconcile floor mean N=2 can take several seconds to
// surface, and the precise timing must never be asserted.
func TestAdaptive_FleetGrowthIsObserved(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	cfg := testutil.IntegrationTestConfig()

	// Enough partitions to form a real cluster and give every worker a slice.
	partitions := testutil.CreateTestPartitions(12)
	src := source.NewStatic(partitions)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	cluster := &testutil.WorkerCluster{
		Workers:  make([]*parti.Manager, 0),
		Config:   cfg,
		Source:   src,
		Strategy: strategy.NewConsistentHash(),
		NC:       nc,
		JS:       js,
		T:        t,
	}
	defer cluster.StopWorkers()

	// Worker-1 carries the observation seam.
	observer := &fleetObserverUpdater{}
	w1 := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerConsumerUpdater(observer))

	require.NoError(t, w1.Start(ctx), "worker-1 failed to start")
	require.NoError(t, <-w1.WaitState(parti.StateStable, 15*time.Second),
		"worker-1 did not reach StateStable")

	// Worker-1 observes itself as the sole committed worker (N=1). This is the
	// startup bootstrap observation (observeFleetSize at the commit-path apply)
	// and/or the first commit-watch delivery.
	require.Eventually(t, func() bool {
		return observer.lastN() == 1
	}, 10*time.Second, 100*time.Millisecond,
		"worker-1 must observe itself as the sole worker (N=1)")

	// Add worker-2. It joins the same coordination buckets, the leader recomputes
	// the assignment, and the committed worker-set grows to N=2. Worker-1's commit
	// watcher must surface that change to its observer.
	w2 := cluster.AddWorkerWithOptions(ctx)
	require.NoError(t, w2.Start(ctx), "worker-2 failed to start")
	require.NoError(t, <-w2.WaitState(parti.StateStable, 15*time.Second),
		"worker-2 did not reach StateStable")

	// The core proof: worker-1 observed the fleet growth end-to-end through the
	// commit watcher. Generous timeout because commit republish + propagation +
	// the reconcile floor can take several seconds.
	require.Eventually(t, func() bool {
		return observer.lastN() == 2
	}, 20*time.Second, 200*time.Millisecond,
		"worker-1 must observe the fleet growth to N=2 through its commit watcher")

	// Sanity: worker-1 should have seen 1 before 2 (monotonic growth), proving the
	// observation tracked the real fleet timeline rather than jumping straight to 2.
	observer.mu.Lock()
	seen := append([]int(nil), observer.seen...)
	observer.mu.Unlock()
	require.Contains(t, seen, 1, "worker-1 should have observed the N=1 phase")
	require.Contains(t, seen, 2, "worker-1 should have observed the N=2 phase")

	// NOTE on the bonus "unchanged-own-slice" sub-goal: with ConsistentHash over
	// 12 partitions, adding worker-2 generally reshuffles worker-1's slice, so this
	// test does not isolate the case where worker-1's own slice is unchanged. The
	// pre-debounce hook placement (observeFleetSize runs in onUpdate BEFORE the
	// apply debounce, so it fires even when this worker's slice does not change) is
	// covered by the unit tests; the N=2 observation here is the live-NATS core
	// proof that the wiring reaches ObserveWorkerCount at all.

	// Cleanup is the deferred cluster.StopWorkers() (StateShutdown-guarded and
	// non-fatal on teardown races); no explicit per-worker Stop needed.
}

// TestAdaptive_CommitChurnRaceClean is the monitor-goroutine concurrency stress
// test required by AGENTS.md for any new work added to a watch/monitor goroutine.
// The fleet-size feature adds an observeFleetSize call on the commit-watch
// goroutine (manager_assignment.go onUpdate + onReconcile), which runs
// concurrently with the production apply/heartbeat/election paths sharing nats.go
// cached *stream state. The unit tests exercise observeFleetSize in isolation and
// cannot find a race between it and those production paths.
//
// This test follows the manager_epoch_monitor_concurrency_test.go template:
//   - Start a small real cluster (3 workers, embedded NATS) with each worker
//     carrying a fleetObserverUpdater so the observeFleetSize → ObserveWorkerCount
//     push is actually exercised (not short-circuited by a nil observer).
//   - Drive ~5 seconds of repeated commit churn by cycling a worker out and back
//     in, forcing the leader to republish commits (each republish flows through
//     every worker's commit watcher and its observeFleetSize call).
//   - Assert no -race trigger, no panic, and clean Stop.
//
// This is a PURE race gate: no rate-value or pacing assertion. Pacing is timing-
// dependent and must never be asserted; correctness of the retune math lives in
// the unit tests.
func TestAdaptive_CommitChurnRaceClean(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Aggressive OperationTimeout drives the watch-session reconcile/monitor
	// cadence high, maximising the number of commit-watch onUpdate/onReconcile
	// invocations (each running observeFleetSize) during the soak window.
	cfg := testutil.IntegrationTestConfig()
	cfg.OperationTimeout = 50 * time.Millisecond

	partitions := testutil.CreateTestPartitions(12)
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	cluster := &testutil.WorkerCluster{
		Workers:  make([]*parti.Manager, 0),
		Config:   cfg,
		Source:   src,
		Strategy: assignStrat,
		NC:       nc,
		JS:       js,
		T:        t,
	}
	defer cluster.StopWorkers()

	// Every worker carries an observer so observeFleetSize actually pushes N on
	// every commit it decodes — the push is the new work we want exercised under
	// the race detector, not just the decode.
	const numWorkers = 3
	for range numWorkers {
		cluster.AddWorkerWithOptions(ctx, parti.WithWorkerConsumerUpdater(&fleetObserverUpdater{}))
	}

	cluster.StartWorkers(ctx)
	cluster.WaitForStableState(15 * time.Second)

	// Drive ~5 seconds of commit churn. Each cycle removes a worker (leader
	// recomputes and republishes a commit with N-1 workers) and adds a fresh one
	// back (republish with N workers), so every surviving worker's commit watcher
	// repeatedly runs observeFleetSize concurrently with heartbeat/election/apply
	// traffic against the same KV buckets. The race detector trips on any
	// unsynchronised cross-goroutine access exercised during this window.
	soakCtx, soakCancel := context.WithTimeout(ctx, 5*time.Second)
	defer soakCancel()

	churnCycles := 0 // single-writer (this goroutine only); diagnostic count
	for soakCtx.Err() == nil {
		// Remove the last worker (simulates a crash → fleet shrinks → republish).
		idx := cluster.WorkerCount() - 1
		if idx < 1 {
			break
		}
		cluster.RemoveWorker(idx)

		// Let the shrink commit propagate, bounded by the soak deadline.
		if !waitOrDeadline(soakCtx, 300*time.Millisecond) {
			break
		}

		// Add a fresh worker back (fleet grows → republish).
		w := cluster.AddWorkerWithOptions(soakCtx, parti.WithWorkerConsumerUpdater(&fleetObserverUpdater{}))
		if err := w.Start(soakCtx); err != nil {
			// soakCtx may have expired mid-start; that is a benign end-of-soak,
			// not a failure of the race gate.
			break
		}

		churnCycles++

		if !waitOrDeadline(soakCtx, 300*time.Millisecond) {
			break
		}
	}

	// Drain the remaining soak window so late deliveries to ObserveWorkerCount
	// settle before the race check below.
	<-soakCtx.Done()

	t.Logf("commit-churn soak complete: %d churn cycles driven", churnCycles)

	// The race gate: if the race detector or any sub-assertion fired during the
	// concurrent soak, the test is already marked failed. t.Failed() is the most
	// reliable in-body signal; Go also writes WARNING: DATA RACE to stderr at
	// detection time.
	require.False(t, t.Failed(),
		"race detector or sub-assertion failed during the commit-churn soak; "+
			"check stderr for WARNING: DATA RACE blocks around observeFleetSize / the commit watcher")
}

// waitOrDeadline sleeps for d unless ctx finishes first. It returns true if the
// full duration elapsed and false if ctx was cancelled/expired during the wait —
// letting the churn loop end cleanly at the soak deadline instead of sleeping
// past it.
func waitOrDeadline(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
