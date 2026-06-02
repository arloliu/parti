package failure_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestNP8FleetNATSOutage_LeaderContinuityRecoversFleet extends the single-worker
// M5 full-NATS-outage proof (TestFullNATSOutage_UnlimitedReconnects_RecoversFleet)
// to a 3-MANAGER fleet sharing ONE restartable NATS connection, across an outage
// that exceeds WorkerIDTTL.
//
// Scenario NP-8 mechanism 1 — guards the WORKING-AS-DESIGNED claim-loss self-stop
// boundary (see docs/OPERATIONS.md "Recovery bound — worker-ID lease"). M5
// recover-to-Stable is bounded by WorkerIDTTL: an outage that exceeds it ages out
// the stableID worker-ID lease (the stableID bucket MaxAge is reconciled to
// WorkerIDTTL). On reconnect each worker's renewal sees a revision mismatch,
// surfaces a bare ErrClaimLost, and onClaimerError routes it to claimLostShutdown
// (manager_election.go:106-118) — the deliberate split-brain-safe self-stop, since
// a peer may have taken the slot. The documented contract is therefore NOT
// in-process recovery to Stable; it is:
//
//  1. All 3 managers enter Degraded during the outage.
//  2. After the restart, every worker self-stops to StateShutdown (the lease aged
//     out; the bare ErrClaimLost is unrecoverable in place).
//  3. Recovery is orchestrator rotation (the readiness probe sees StateShutdown and
//     the pod is replaced); Parti does not auto-reclaim an aged-out lease.
//
// This proof asserts the Shutdown outcome and captures each worker's terminal error
// (the ErrClaimLost) via the OnError hook, so it GUARDS the boundary rather than a
// behavior Parti does not provide. Raise WorkerIDTTL above the worst-case outage to
// move the boundary (a real RF3 cluster ROLLING restart that keeps the outage under
// WorkerIDTTL preserves the lease and may recover in-process; this single-node
// embedded restart exercises the past-boundary worst case).
//
// Mechanism 2 (heartbeat-bucket loss when the claim survives) is isolated by the
// companion TestNP8FleetNATSOutage_HeartbeatBucketLossFlap below.
func TestNP8FleetNATSOutage_LeaderContinuityRecoversFleet(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// Shared restartable connection (unlimited reconnect, stable port, FileStorage
	// StoreDir) — the same helper the single-worker M5 proof uses.
	nc, stopNATS, restartNATS := startEmbeddedNATSOutage(t, -1)

	// Build the events stream on FileStorage so it survives the restart. We use
	// a fresh jetstream context on the shared conn (rfBuildStream targets the
	// "events.>" subjects with FileStorage).
	realJS, err := jetstream.New(nc)
	require.NoError(t, err)
	rfBuildStream(t, ctx, realJS)

	// Fleet config: fast degrade/recover thresholds so the outage is observed
	// quickly, but otherwise the standard integration config (valid:
	// ExitThreshold <= EnterThreshold, RecoveryGracePeriod default >= 5s).
	cfg := testutil.IntegrationTestConfig()
	cfg.DegradedBehavior.EnterThreshold = 500 * time.Millisecond
	cfg.DegradedBehavior.ExitThreshold = 500 * time.Millisecond

	src := source.NewStatic(testutil.CreateTestPartitions(6))
	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)

	const np8NumWorkers = 3

	// Per-worker terminal-error capture. Each manager gets its own OnError hook so
	// the bare ErrClaimLost that drives the self-stop is captured, not inferred
	// (report §1: attribution must be captured). Because AddWorkerWithOptions
	// injects the cluster tracker hooks FIRST and our WithHooks LAST, and WithHooks
	// overwrites (options.go: o.hooks = hooks; applied in order), OUR hooks win —
	// the cluster trackers are dropped. The cluster wait helpers read
	// mgr.IsLeader() / CurrentAssignment() / mgr.State() directly, not the
	// trackers, so this is intentional.
	//
	// We store the FIRST error whose text contains "claim" (the ErrClaimLost) so a
	// later connectivity error in the reconnect window cannot clobber the
	// attribution we want to prove. Each value is seeded with "" so the
	// .Load().(string) in the terminal-state logf never panics on a worker that
	// emitted no error.
	terminalErr := make([]*atomic.Value, np8NumWorkers)

	for i := range np8NumWorkers {
		terminalErr[i] = &atomic.Value{}
		terminalErr[i].Store("")

		te := terminalErr[i]
		hooks := &parti.Hooks{
			OnError: func(_ context.Context, err error) error {
				if err != nil && strings.Contains(err.Error(), "claim") {
					te.CompareAndSwap("", err.Error())
				}

				return nil
			},
		}

		cluster.AddWorkerWithOptions(ctx, parti.WithHooks(hooks))
	}

	cluster.StartWorkers(ctx)

	// --- Baseline: one leader, full 6-partition coverage. ---
	require.NotNil(t, cluster.WaitForLeader(15*time.Second), "baseline: one leader")
	cluster.WaitForPartitionCoverage(6, 15*time.Second)

	// Build the []ManagerWaiter once for the all-managers-state waits. We use
	// GetActiveWorkers (mutex-guarded) rather than the raw Workers field.
	activeWorkers := cluster.GetActiveWorkers()
	waiters := make([]testutil.ManagerWaiter, len(activeWorkers))
	for i, mgr := range activeWorkers {
		waiters[i] = mgr
	}

	// --- OUTAGE: stop the server, hold the window open, observe Degraded. ---
	outageStart := time.Now()
	stopNATS(t)
	require.Eventually(t, func() bool { return nc.Status() != nats.CONNECTED },
		5*time.Second, 50*time.Millisecond, "fleet did not observe NATS outage")

	// All 3 enter Degraded during the outage.
	require.NoError(t,
		testutil.WaitAllManagersState(ctx, waiters, types.StateDegraded, 15*time.Second),
		"all 3 managers must enter Degraded during the NATS outage")

	// Hold the outage >= 7s, deterministically exceeding WorkerIDTTL (5s in the
	// integration config) so the stableID claim ages out: on reconnect each worker
	// hits ErrClaimLost -> claimLostShutdown. (Gating on all-3-Degraded already
	// consumes a beat; sleep the remainder.)
	if rem := 7*time.Second - time.Since(outageStart); rem > 0 {
		time.Sleep(rem)
	}

	// --- RESTART: new server on the same port + StoreDir; client reconnects. ---
	restartNATS(t)
	require.Eventually(t, func() bool { return nc.Status() == nats.CONNECTED },
		5*time.Second, 50*time.Millisecond, "fleet did not reconnect after NATS restart")

	// --- POST-RESTART: the outage exceeded WorkerIDTTL, so every worker's lease
	// aged out; on reconnect each hits a bare ErrClaimLost and self-stops. The
	// documented contract is StateShutdown + orchestrator rotation, NOT in-process
	// recovery to Stable. Assert the WAD self-stop so this proof guards the
	// boundary rather than a behavior we do not provide. The pre-captured waiters
	// hold the same manager refs (claimLostShutdown calls Stop, not RemoveWorker),
	// so WaitState observes them reach Shutdown even though GetActiveWorkers would
	// now filter them out.
	require.NoError(t,
		testutil.WaitAllManagersState(ctx, waiters, types.StateShutdown, 30*time.Second),
		"every worker must self-stop to StateShutdown when the outage exceeds WorkerIDTTL")

	// Terminal-reason instrumentation (report §1: attribution must be captured, not
	// inferred): log each worker's final state and the terminal error captured via
	// the OnError hook.
	for i := range np8NumWorkers {
		t.Logf("worker %d: final state=%s terminal-error=%q",
			i, cluster.GetWorkers()[i].State(), terminalErr[i].Load())
	}

	// Attribution ASSERTED, not merely inferred (report §1): every worker's terminal
	// error must be the bare ErrClaimLost ("worker ID claim lost"), proving the
	// StateShutdown above is the claim-loss self-stop specifically — not some other
	// shutdown path. claimLostShutdown drains the OnError hook via Stop before the
	// worker reaches Shutdown, so terminalErr is populated by now; Eventually guards
	// any residual async.
	require.Eventually(t, func() bool {
		for i := range np8NumWorkers {
			s, _ := terminalErr[i].Load().(string)
			if !strings.Contains(s, "claim") {
				return false
			}
		}

		return true
	}, 5*time.Second, 100*time.Millisecond,
		"each worker's terminal error must be the bare ErrClaimLost (worker ID claim lost)")
}

// TestNP8FleetNATSOutage_HeartbeatBucketLossFlap is the mechanism-(2) companion to
// the self-stop proof above. It raises WorkerIDTTL ABOVE the outage so the stableID
// claim SURVIVES (the claim-loss self-stop of mechanism 1 does NOT fire), isolating
// the second non-auto-heal mechanism: with MemoryStorage the heartbeat bucket was
// lost after a single-node restart, so any worker that became leader failed
// "list heartbeat keys: stream not found" in its calculator and the fleet oscillated
// Degraded<->Stable without ever HOLDING Stable.
//
// With FileStorage the heartbeat bucket now survives the single-node restart and the
// fleet reaches AND holds all-Stable (the test asserts reach-all-Stable then
// require.Never flap, which now holds). This test is the regression guard for
// NP-8 mechanism 2. See docs/plans/auto-healing-gap-closure/04-proof-findings.md.
func TestNP8FleetNATSOutage_HeartbeatBucketLossFlap(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	nc, stopNATS, restartNATS := startEmbeddedNATSOutage(t, -1)
	realJS, err := jetstream.New(nc)
	require.NoError(t, err)
	rfBuildStream(t, ctx, realJS)

	cfg := testutil.IntegrationTestConfig()
	cfg.DegradedBehavior.EnterThreshold = 500 * time.Millisecond
	cfg.DegradedBehavior.ExitThreshold = 500 * time.Millisecond
	// Raise WorkerIDTTL well above the outage so the stableID claim survives and
	// mechanism 1 (claim-loss self-stop) does NOT fire — isolating mechanism 2.
	cfg.WorkerIDTTL = 30 * time.Second

	src := source.NewStatic(testutil.CreateTestPartitions(6))
	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	for range 3 {
		cluster.AddWorkerWithOptions(ctx, parti.WithHooks(&parti.Hooks{}))
	}
	cluster.StartWorkers(ctx)
	require.NotNil(t, cluster.WaitForLeader(15*time.Second))
	cluster.WaitForPartitionCoverage(6, 15*time.Second)
	workers := cluster.GetActiveWorkers()
	require.Len(t, workers, 3)

	np8bAllStable := func() bool {
		for _, m := range workers {
			if m.State() != types.StateStable {
				return false
			}
		}

		return true
	}

	// Outage shorter than WorkerIDTTL (claim survives), long enough that a
	// MemoryStorage heartbeat bucket would have been lost on the single-node
	// restart — with the FileStorage default the stream now survives.
	stopNATS(t)
	require.Eventually(t, func() bool { return nc.Status() != nats.CONNECTED },
		5*time.Second, 50*time.Millisecond, "fleet did not observe outage")
	time.Sleep(5 * time.Second)
	restartNATS(t)
	require.Eventually(t, func() bool { return nc.Status() == nats.CONNECTED },
		5*time.Second, 50*time.Millisecond, "fleet did not reconnect")

	// With the FileStorage heartbeat default the stream survives the restart, so
	// the fleet must reach all-Stable and HOLD it. Before the fix (MemoryStorage)
	// the stream was lost: the heartbeat publisher Put and the leader's "list
	// heartbeat keys" failed, and the fleet oscillated Degraded<->Stable, never
	// holding — either the all-Stable wait timed out or the HOLD check below tripped.
	require.Eventually(t, np8bAllStable, 30*time.Second, 200*time.Millisecond,
		"fleet must reach all-Stable after the restart")
	require.Never(t, func() bool { return !np8bAllStable() }, 8*time.Second, 200*time.Millisecond,
		"fleet must HOLD all-Stable; the FileStorage heartbeat stream survives the single-node restart")
	require.True(t, nc.IsConnected(),
		"the NATS connection must be CONNECTED throughout (mechanism 2 is not a connectivity loss)")
}
