package failure_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --- NP-10 leader enumeration-stall fault seam -------------------------------
//
// NP-10 reproduces a SILENT FALSE-HEALTHY LEADER: the leader's worker
// enumeration (the heartbeat-bucket Keys scan in WorkerMonitor.GetActiveWorkers,
// which runs leader-only) sustains a context.DeadlineExceeded, while every
// single-key op stays healthy — each worker's own heartbeat Put on the SAME
// bucket, and the leader's election renewal on a SEPARATE bucket. A
// Keys()-scan DeadlineExceeded is classified as neither a connectivity error nor
// a degrading-JetStream error (see calculator_enumeration_stall_test.go), so
// getActiveWorkers returns it raw, the poll loop only logs it, and nothing routes
// it to the manager's degraded circuit. The leader therefore stays StateStable,
// FREEZES its last-computed assignment, and stops reacting to real membership
// changes — a departed worker's partitions are never reassigned, with no
// degraded signal.
//
// The seam is modeled on NP-9's np9Fault* seam but INVERTED: NP-9 faults
// Put/Update/Create/Get/Watch and lets Keys pass through; NP-10 faults ONLY
// Keys() (the enumeration scan) and lets every other op pass through, scoped to
// the heartbeat bucket. That isolation is essential — faulting the whole bucket
// would fail the heartbeat Put and degrade the leader via the kv-unavailable
// circuit (the WRONG reason), masking NP-10.

type np10FaultController struct {
	armed    atomic.Bool
	injected atomic.Int64
}

func (fc *np10FaultController) arm()    { fc.injected.Store(0); fc.armed.Store(true) }
func (fc *np10FaultController) disarm() { fc.armed.Store(false) }
func (fc *np10FaultController) fault() bool {
	if !fc.armed.Load() {
		return false
	}
	fc.injected.Add(1)

	return true
}

type np10FaultJetStream struct {
	jetstream.JetStream
	buckets map[string]struct{}
	fc      *np10FaultController
}

func (f *np10FaultJetStream) wrap(kv jetstream.KeyValue, bucket string) jetstream.KeyValue {
	if _, ok := f.buckets[bucket]; ok {
		return &np10FaultKeyValue{KeyValue: kv, fc: f.fc}
	}

	return kv
}

func (f *np10FaultJetStream) KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.KeyValue(ctx, bucket)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, bucket), nil
}

func (f *np10FaultJetStream) CreateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.CreateKeyValue(ctx, cfg)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, cfg.Bucket), nil
}

func (f *np10FaultJetStream) CreateOrUpdateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.CreateOrUpdateKeyValue(ctx, cfg)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, cfg.Bucket), nil
}

// np10FaultKeyValue faults ONLY Keys() — the leader's worker-enumeration scan —
// while Put/Get/Update/Delete/Watch pass through unchanged. This is NP-10's
// defining asymmetry: the stream-wide enumeration times out while every worker's
// single-key heartbeat Put (and the leader's election renewal on a separate
// bucket) keep succeeding.
type np10FaultKeyValue struct {
	jetstream.KeyValue
	fc *np10FaultController
}

func (k *np10FaultKeyValue) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	if k.fc.fault() {
		return nil, context.DeadlineExceeded
	}

	return k.KeyValue.Keys(ctx, opts...)
}

// TestNP10_LeaderEnumerationStall_FrozenMembership is the end-to-end NP-10
// regression guard. It stands up a leader + 2 followers with full partition
// coverage, arms a Keys-only stall on the heartbeat bucket (the connection stays
// UP and every single-key op keeps succeeding), removes a follower, and asserts
// the leader surfaces the stall instead of silently freezing.
//
// This is a verify-first test: it FAILED on the parent (the leader sat in
// StateStable with the removed follower's partitions orphaned and 0 degrades) and
// PASSES with the fix (the leader degrades with reason "heartbeat-enumeration-
// stall", then recovers on disarm). The three checks:
//   - GAP (the gate): within the armed window the leader must surface the stall —
//     degrade with the enumeration-stall reason, or re-cover. Pre-fix it did
//     neither; the fix degrades it.
//   - CONTROL: disarming the stall lets the leader re-enumerate, see the departed
//     follower, and re-cover — this held even on the parent with no fix, proving
//     the freeze is specifically the enumeration blind spot, not a broken leader.
//   - NON-VACUITY (guards both directions): the Keys fault actually fired
//     (injected > 0), the removal orphaned a partition, and the connection stayed
//     CONNECTED throughout.
func TestNP10_LeaderEnumerationStall_FrozenMembership(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	const (
		numPartitions    = 6
		numWorkers       = 3
		armedObservation = 15 * time.Second
	)

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()

	fc := &np10FaultController{}
	faultJS := &np10FaultJetStream{
		JetStream: realJS,
		fc:        fc,
		// Scope strictly to the heartbeat bucket: faulting any other bucket
		// (election / assignment) would trigger a DIFFERENT real degrade or a
		// leadership loss and mask NP-10.
		buckets: map[string]struct{}{
			cfg.KVBuckets.HeartbeatBucket: {},
		},
	}

	src := source.NewStatic(testutil.CreateTestPartitions(numPartitions))

	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	// Inject the fault wrapper. AddWorkerWithOptions reads cluster.JS at worker
	// creation time, so every manager receives the heartbeat-Keys-faulting JS.
	// The injected>0 precondition below makes any propagation failure loud rather
	// than vacuous.
	cluster.JS = faultJS

	// Per-worker observers. WithHooks here replaces the cluster's tracker hooks,
	// which is fine: every wait/coverage helper reads live manager state, not the
	// trackers. Shared, thread-safe sinks captured by all workers. The gate keys on
	// the SPECIFIC enumeration-stall reason (not "any degrade") so a wrong-reason
	// degrade — e.g. from the leadership path — cannot pass the proof spuriously.
	const enumStallReason = "heartbeat-enumeration-stall"
	var sawEnumStallDegrade atomic.Bool
	var degradedEntries atomic.Int64
	observerHooks := &parti.Hooks{
		OnDegraded: func(_ context.Context, reason string) error {
			if reason == enumStallReason {
				sawEnumStallDegrade.Store(true)
			}

			return nil
		},
		OnStateChanged: func(_ context.Context, _, to parti.State) error {
			if to == parti.StateDegraded {
				degradedEntries.Add(1)
			}

			return nil
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()

	for range numWorkers {
		cluster.AddWorkerWithOptions(ctx, parti.WithHooks(observerHooks))
	}
	cluster.StartWorkers(ctx)
	t.Cleanup(cluster.StopWorkers)

	leader := cluster.WaitForLeader(20 * time.Second)
	require.NotNil(t, leader, "a single leader must be elected")
	cluster.WaitForPartitionCoverage(numPartitions, 20*time.Second)

	// liveCoverage = unique partition IDs across the still-running workers (a
	// removed worker is excluded). Frozen membership shows as coverage < N.
	liveCoverage := func() int {
		uniq := make(map[string]struct{})
		for _, mgr := range cluster.GetActiveWorkers() {
			for _, p := range mgr.CurrentAssignment().Partitions {
				if id := p.ID(); id != "" {
					uniq[id] = struct{}{}
				}
			}
		}

		return len(uniq)
	}

	// Pick a FOLLOWER to remove (never the leader — removing the leader would
	// trigger a real re-election and confound the proof).
	followerIdx := -1
	for i, mgr := range cluster.GetWorkers() {
		if mgr.State() != parti.StateShutdown && mgr.WorkerID() != leader.WorkerID() {
			followerIdx = i
			break
		}
	}
	require.GreaterOrEqual(t, followerIdx, 0, "must have at least one follower to remove")

	// Arm the Keys-only enumeration stall.
	fc.arm()

	// HARD PRECONDITION (vacuity killer): the fault must actually reach the
	// leader's enumeration scan (leader-only WorkerMonitor poll, every hbTTL/2).
	require.Eventually(t, func() bool { return fc.injected.Load() > 0 }, 15*time.Second, 250*time.Millisecond,
		"the heartbeat Keys-only fault must actually fire against the leader's enumeration before we assert anything")
	require.True(t, nc.IsConnected(), "the NATS connection must be up when the stall is armed (this is not a disconnect)")

	// Remove the follower. Its heartbeat key is deleted on Stop (Delete passes
	// through the Keys-only fault), so a NON-stalled leader would see the reduced
	// set on its next scan. Under the stall, the leader's scan times out instead.
	cluster.RemoveWorker(followerIdx)

	// Vacuity guard: the removal must actually orphan ≥1 partition (the leader is
	// frozen, so the departed follower's share is now uncovered). If it held no
	// partitions, coverage would stay full and the gate would pass spuriously.
	require.Less(t, liveCoverage(), numPartitions,
		"removing the follower must orphan at least one partition, else the scenario is vacuous")

	// Observe the armed window WITHOUT failing: does the leader react to the
	// follower's departure — re-cover the orphaned partitions, or surface the
	// stall by degrading?
	reactedWhileArmed := false
	deadline := time.Now().Add(armedObservation)
	for time.Now().Before(deadline) {
		if liveCoverage() == numPartitions || sawEnumStallDegrade.Load() {
			reactedWhileArmed = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	observedCoverage := liveCoverage()
	observedLeaderState := leader.State()

	// CONTROL (recovery) — PASSES on the parent with no fix. Clearing the stall
	// lets the leader re-enumerate, see the departed follower, and re-cover. This
	// is the causal proof: the freeze is the enumeration blind spot, not a broken
	// leader.
	fc.disarm()
	cluster.WaitForPartitionCoverage(numPartitions, 30*time.Second)

	// NON-VACUITY (guards both directions of the STOP gate).
	require.Positive(t, fc.injected.Load(),
		"the Keys-only heartbeat-enumeration fault must have actually fired against the leader")
	require.True(t, nc.IsConnected(),
		"the NATS connection stayed CONNECTED throughout — this is an enumeration stall, not a disconnect")

	// THE GAP — post-fix invariant. FAILS on the parent, proving NP-10 is real;
	// flips green once the fix degrades the leader with the enumeration-stall reason.
	require.True(t, reactedWhileArmed,
		"NP-10 silent false-healthy leader: with the heartbeat Keys scan stalling, the removed follower's "+
			"partitions stayed orphaned (live coverage %d/%d) and the leader sat in %s without surfacing a %q "+
			"degrade (cluster degraded-entries=%d) for %s while the connection stayed up and the fault fired %d "+
			"times. A correct leader must surface the stall (Degraded reason %q) or re-cover. This assertion "+
			"FAILS on the parent commit, proving the gap is real.",
		observedCoverage, numPartitions, observedLeaderState, enumStallReason, degradedEntries.Load(),
		armedObservation, fc.injected.Load(), enumStallReason)
}
