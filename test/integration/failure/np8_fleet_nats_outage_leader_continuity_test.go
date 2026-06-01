package failure_test

import (
	"context"
	"os"
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
// to a 3-MANAGER fleet sharing ONE restartable NATS connection.
//
// Scenario NP-8 — KNOWN FAILING on current main (gated, opt-in). It asserts the
// auto-heal property the single-worker M5 proof claims, but for a 3-manager fleet
// across an outage >= WorkerIDTTL:
//
//  1. All 3 managers enter Degraded during the outage.
//  2. All 3 managers return Stable after the server restart.
//  3. Exactly ONE leader after recovery (continuity OR clean re-election, NOT
//     split-brain). Leadership is dropped during the outage by design, so we LOG
//     continuity but do NOT assert it.
//  4. Assignments republished/observed fleet-wide (6-partition coverage holds;
//     version does not regress).
//  5. NO post-recovery flap.
//
// WHY IT FAILS ON MAIN (see docs/plans/auto-healing-gap-closure/04-proof-findings.md,
// finding NP-8). The fleet does NOT auto-heal across a NATS server restart, via two
// distinct mechanisms unmasked by the single-worker M5 proof:
//   - Outage >= WorkerIDTTL ages out the stableID claim (the bucket MaxAge is
//     reconciled to WorkerIDTTL). On reconnect each worker hits ErrClaimLost ->
//     claimLostShutdown (the deliberate split-brain self-stop) and ends in
//     StateShutdown — never returning to Stable. (manager_election.go:107-118)
//   - Even when the claim survives (WorkerIDTTL raised above the outage), the
//     MemoryStorage heartbeat bucket is gone after a single-node restart, so the
//     leader's calculator fails "list heartbeat keys: stream not found" and the
//     fleet flaps Stable<->Degraded indefinitely.
//
// A real RF3 cluster ROLLING restart that preserves replicated MemoryStorage and
// keeps the outage under WorkerIDTTL may recover; this single-node embedded restart
// exercises the worst case. Gated opt-in so it does not break `make test`; remove
// the gate when the fix lands and it becomes a regression proof.
func TestNP8FleetNATSOutage_LeaderContinuityRecoversFleet(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}
	if os.Getenv("PARTI_RUN_NP8_FLEET_OUTAGE_PROOF") == "" {
		t.Skip("opt-in KNOWN-FAILING proof (NP-8 fleet does not auto-heal a NATS restart); " +
			"set PARTI_RUN_NP8_FLEET_OUTAGE_PROOF=1 to run")
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

	// Per-worker recovery / flap / reason tracking. Each manager gets its own
	// OnStateChanged + OnDegraded hooks. Because AddWorkerWithOptions injects the
	// cluster tracker hooks FIRST and our WithHooks LAST, and WithHooks overwrites
	// (options.go: o.hooks = hooks; applied in order), OUR hooks win — the cluster
	// trackers are dropped. The cluster wait helpers read mgr.IsLeader() /
	// CurrentAssignment() directly, not the trackers, so this is intentional.
	recovered := make([]*atomic.Bool, np8NumWorkers)
	postRecoveryRedegrade := make([]*atomic.Int64, np8NumWorkers)
	badReason := make([]*atomic.Bool, np8NumWorkers) // any OnDegraded reason != "NATS connection down"
	natsDownReasons := make([]*atomic.Int64, np8NumWorkers)

	for i := range np8NumWorkers {
		recovered[i] = &atomic.Bool{}
		postRecoveryRedegrade[i] = &atomic.Int64{}
		badReason[i] = &atomic.Bool{}
		natsDownReasons[i] = &atomic.Int64{}

		rec := recovered[i]
		redeg := postRecoveryRedegrade[i]
		bad := badReason[i]
		downCount := natsDownReasons[i]

		hooks := &parti.Hooks{
			OnStateChanged: func(_ context.Context, from, to types.State) error {
				// Count Stable->Degraded transitions that happen AFTER the fleet
				// has first recovered post-restart. A genuine post-recovery
				// re-degrade (the heartbeat-MemoryStorage flap loop) increments
				// this; the single intentional outage degrade does not, because
				// it precedes the recovered flag being set.
				if from == types.StateStable && to == types.StateDegraded && rec.Load() {
					redeg.Add(1)
				}

				return nil
			},
			OnDegraded: func(_ context.Context, reason string) error {
				if reason == "NATS connection down" {
					downCount.Add(1)
				} else {
					bad.Store(true)
				}

				return nil
			},
		}

		cluster.AddWorkerWithOptions(ctx, parti.WithHooks(hooks))
	}

	cluster.StartWorkers(ctx)

	// --- Baseline: one leader, full 6-partition coverage, version snapshot. ---
	leaderBefore := cluster.WaitForLeader(15 * time.Second).WorkerID()
	cluster.WaitForPartitionCoverage(6, 15*time.Second)

	// Snapshot each active manager's pre-outage assignment version. The minimum
	// across the fleet is the floor the post-recovery republish must not regress
	// below (the assignment bucket is FileStorage and survives the restart).
	var np8VersionFloor int64 = 1 << 62
	for _, mgr := range cluster.GetActiveWorkers() {
		if v := mgr.CurrentAssignment().Version; v < np8VersionFloor {
			np8VersionFloor = v
		}
	}
	t.Logf("pre-outage leader=%s version-floor=%d", leaderBefore, np8VersionFloor)

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

	// All 3 return Stable after the restart.
	require.NoError(t,
		testutil.WaitAllManagersState(ctx, waiters, types.StateStable, 30*time.Second),
		"all 3 managers must return Stable after the NATS restart")

	// Arm the post-recovery flap detector: from here on, any Stable->Degraded
	// transition is a genuine re-degrade (the gap_flap symptom).
	for _, r := range recovered {
		r.Store(true)
	}

	// --- Exactly ONE leader after recovery (continuity OR clean re-election). ---
	leaderAfter := cluster.WaitForLeader(20 * time.Second)
	require.NotNil(t, leaderAfter, "exactly one leader must exist after recovery")
	// DIAGNOSTIC ONLY: leadership is dropped during the outage by design, so
	// continuity is not guaranteed and is NOT asserted.
	t.Logf("post-recovery leader=%s continuity=%v",
		leaderAfter.WorkerID(), leaderAfter.WorkerID() == leaderBefore)

	// --- Assignments republished / observed fleet-wide. ---
	cluster.WaitForPartitionCoverage(6, 20*time.Second)
	// Assignment version must not regress below the pre-outage floor (FileStorage
	// assignment bucket survives the restart; the leader republishes on recovery).
	cluster.WaitForAssignmentVersion(np8VersionFloor, 20*time.Second)

	// --- NO post-recovery flap. The catch for a fleet-wide heartbeat-MemoryStorage
	// re-degrade loop. Two complementary checks: a sampled require.Never and the
	// hook-driven re-degrade counters (catch transitions sampling would miss). ---
	require.Never(t, func() bool {
		for _, mgr := range cluster.GetActiveWorkers() {
			if mgr.State() == parti.StateDegraded {
				return true
			}
		}

		return false
	}, 5*time.Second, 100*time.Millisecond,
		"no manager may re-enter Degraded after the fleet recovers (heartbeat-flap loop)")

	for i := range np8NumWorkers {
		require.Zero(t, postRecoveryRedegrade[i].Load(),
			"worker %d must not re-enter Degraded after recovery", i)
	}

	// --- Negative-space (DIAGNOSTIC ONLY): which OnDegraded reason each manager
	// reported. The connection-status monitor fires enterDegraded("NATS connection
	// down") for every manager once EnterThreshold of downtime elapses, but the
	// generic KV-error circuit (recordKVError) ALSO admits connectivity errors and
	// can reach its threshold (KVErrorThreshold=5 within KVErrorWindow=30s, ~2.5s
	// at the 500ms heartbeat cadence) and degrade with "KV error threshold
	// exceeded" / "kv-unavailable" — whichever trigger wins the CAS sets the
	// reason. That race makes a per-worker "only NATS connection down" assertion a
	// reason-string probe, not a recovery signal, so we LOG it rather than assert.
	// The real auto-heal invariants (all-Stable, one-leader, coverage, no-flap)
	// are asserted above and below.
	var np8FleetDownReasons int64
	for i := range np8NumWorkers {
		np8FleetDownReasons += natsDownReasons[i].Load()
		t.Logf("worker %d OnDegraded: nats-down-count=%d other-reason=%v",
			i, natsDownReasons[i].Load(), badReason[i].Load())
	}
	require.Positive(t, np8FleetDownReasons,
		"at least one manager must report \"NATS connection down\" — the outage must "+
			"be classified as a connectivity loss somewhere in the fleet")
}

// TestNP8FleetNATSOutage_HeartbeatBucketLossFlap is the mechanism-(2) companion to
// the self-stop proof above. It raises WorkerIDTTL ABOVE the outage so the stableID
// claim SURVIVES (the claim-loss self-stop of mechanism 1 does NOT fire), isolating
// the second non-auto-heal mechanism: the MemoryStorage heartbeat bucket is gone
// after a single-node restart, so any worker that becomes leader fails
// "list heartbeat keys: stream not found" in its calculator and the fleet oscillates
// Degraded<->Stable without ever HOLDING Stable.
//
// KNOWN FAILING on current main (gated, opt-in). See
// docs/plans/auto-healing-gap-closure/04-proof-findings.md finding NP-8 mechanism (2).
// This is the worst case (full MemoryStorage loss on a single-node restart); a real
// RF3 cluster whose replicated MemoryStorage survives a rolling restart may not hit
// it. Gated so it does not break `make test`; remove the gate when the fix lands.
func TestNP8FleetNATSOutage_HeartbeatBucketLossFlap(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}
	if os.Getenv("PARTI_RUN_NP8_HEARTBEAT_FLAP_PROOF") == "" {
		t.Skip("opt-in KNOWN-FAILING proof (NP-8 mechanism 2: heartbeat-bucket-loss fleet flap); " +
			"set PARTI_RUN_NP8_HEARTBEAT_FLAP_PROOF=1 to run")
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

	// Outage shorter than WorkerIDTTL (claim survives) but long enough that the
	// single-node restart loses the MemoryStorage heartbeat bucket.
	stopNATS(t)
	require.Eventually(t, func() bool { return nc.Status() != nats.CONNECTED },
		5*time.Second, 50*time.Millisecond, "fleet did not observe outage")
	time.Sleep(5 * time.Second)
	restartNATS(t)
	require.Eventually(t, func() bool { return nc.Status() == nats.CONNECTED },
		5*time.Second, 50*time.Millisecond, "fleet did not reconnect")

	// The fleet must reach all-Stable and HOLD it. On main it cannot: the leader's
	// calculator fails "list heartbeat keys: stream not found" so the fleet
	// oscillates Degraded<->Stable. Either the all-Stable wait times out, or it is
	// reached transiently and the HOLD check below fails — both prove no clean heal.
	require.Eventually(t, np8bAllStable, 30*time.Second, 200*time.Millisecond,
		"fleet must reach all-Stable after the restart")
	require.Never(t, func() bool { return !np8bAllStable() }, 8*time.Second, 200*time.Millisecond,
		"fleet must HOLD all-Stable; on main the missing MemoryStorage heartbeat bucket makes it flap")
	require.True(t, nc.IsConnected(),
		"the NATS connection must be CONNECTED throughout (mechanism 2 is not a connectivity loss)")
}
