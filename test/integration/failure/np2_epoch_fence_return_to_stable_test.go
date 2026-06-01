package failure_test

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestNP2EpochFence_NonAssignmentRecreate_DegradedDoesNotFlap probes the M4
// contract for the epoch fence: once a Parti-owned bucket is wiped and
// recreated under a live worker, the worker should enter terminal Degraded
// ("bucket-recreated:<bucket>") so a readiness probe can rotate the pod and a
// fresh Start re-provisions the buckets — it must NOT silently recover to
// Stable while the recreated bucket is still a different stream.
//
// Scenario: a single live worker, then ONE delete+recreate of the HEARTBEAT
// bucket (a NON-assignment Parti-owned bucket). The recreate gives the backing
// stream a strictly-later Created timestamp.
//
// Root cause this probe pins (do NOT fix here, just prove):
//   - checkBucketEpochs (manager_setup.go:684-690) compares the live stream
//     Created against the cached ep.created but NEVER re-captures ep.created
//     after firing. So every subsequent epoch tick (OperationTimeout cadence)
//     re-enters Degraded with reason "bucket-recreated:<heartbeat>".
//   - attemptRecoveryFromDegraded (manager_degraded.go:376-416) runs on the
//     connection monitor (1s) once the connection has been up for ExitThreshold.
//     It refreshes assignment from the STILL-INTACT assignment bucket, the
//     refresh succeeds, the snapshot is already applied, so it calls
//     exitDegraded -> Stable.
//
// The two loops fight: epoch tick re-degrades, recovery loop re-stabilizes.
// That is a Degraded<->Stable FLAP, not the M4 terminal-Degraded contract.
//
// PREDICTED VERDICT on current main: FAILS at the primary invariant
// (np2DegradedToStable >= 1) — the recovery loop exits to Stable on the intact
// assignment bucket while the epoch fence keeps re-degrading on the recreated
// heartbeat bucket. The corroboration (final state == Degraded) is also
// expected to be unreliable on main because the worker oscillates.
func TestNP2EpochFence_NonAssignmentRecreate_DegradedDoesNotFlap(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}
	if os.Getenv("PARTI_RUN_NP2_EPOCH_FLAP_PROOF") == "" {
		t.Skip("opt-in KNOWN-FAILING proof (NP-2 epoch-fence Degraded<->Stable flap); " +
			"set PARTI_RUN_NP2_EPOCH_FLAP_PROOF=1 to run")
	}

	t.Parallel()

	// ~5 epoch ticks (OperationTimeout=1s) plus headroom for the recovery loop
	// to fight back. The whole probe fits comfortably under this bound.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	// Fast epoch tick so the fence fires several times within the observation
	// window; OperationTimeout is the monitorBucketEpochs cadence. The
	// non-strict OperationTimeout <= ElectionTimeout/3 boundary holds
	// (ElectionTimeout=3s).
	cfg.OperationTimeout = 1 * time.Second
	// Recover quickly so the flap is observable within the window: the recovery
	// loop attempts exitDegraded after ExitThreshold of healthy connection.
	cfg.DegradedBehavior.ExitThreshold = 1 * time.Second
	cfg.DegradedBehavior.EnterThreshold = 2 * time.Second
	// Deliberately DO NOT lower KVErrorThreshold: leave it at the default so a
	// transient heartbeat-Put miss during the delete window cannot independently
	// trip "kv-unavailable" and mask the epoch-fence flap under test.

	heartbeatBucket := cfg.KVBuckets.HeartbeatBucket

	// Hook counters. Hooks fire from background goroutines, so all shared state
	// is atomic.
	var (
		np2BucketRecreatedDegrades atomic.Int64 // OnDegraded with the heartbeat bucket-recreated reason
		np2OtherDegrades           atomic.Int64 // OnDegraded with ANY other reason (isolation guard)
		np2DegradedToStable        atomic.Int64 // OnStateChanged Degraded -> Stable (the flap signal)
	)

	bucketRecreatedReason := "bucket-recreated:" + heartbeatBucket
	hooks := &parti.Hooks{
		OnDegraded: func(_ context.Context, reason string) error {
			if reason == bucketRecreatedReason {
				np2BucketRecreatedDegrades.Add(1)
			} else {
				np2OtherDegrades.Add(1)
			}

			return nil
		},
		OnStateChanged: func(_ context.Context, from, to parti.State) error {
			if from == parti.StateDegraded && to == parti.StateStable {
				np2DegradedToStable.Add(1)
			}

			return nil
		},
	}

	// One worker, 2 partitions. The single live worker is the leader and owns
	// every partition; its assignment bucket stays intact throughout, which is
	// exactly what lets the recovery loop keep exiting to Stable.
	src := source.NewStatic(testutil.CreateTestPartitions(2))

	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(), parti.WithHooks(hooks))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	require.NoError(t, mgr.Start(ctx))
	require.NoError(t, <-mgr.WaitState(parti.StateStable, 30*time.Second),
		"worker must reach Stable before the heartbeat bucket is recreated")

	// Discard the Init->...->Stable startup transitions: from here on, every
	// Degraded entry and every Degraded->Stable transition is caused by the
	// recreate under test.
	np2BucketRecreatedDegrades.Store(0)
	np2OtherDegrades.Store(0)
	np2DegradedToStable.Store(0)

	// Capture the original backing-stream Created so the sanity check below can
	// prove the recreate produced a strictly-later stream identity.
	origCreated, err := func() (time.Time, error) {
		kv, kerr := js.KeyValue(ctx, heartbeatBucket)
		if kerr != nil {
			return time.Time{}, kerr
		}

		return kvutil.BucketStreamCreated(ctx, kv)
	}()
	require.NoError(t, err, "failed to read the original heartbeat bucket stream Created")

	// --- The trigger: ONE wipe+recreate of the heartbeat bucket. ---
	// Recreate with the SAME config the manager uses for the heartbeat bucket
	// (MemoryStorage + HeartbeatTTL — see manager_setup.go ensureCoreKVBuckets).
	require.NoError(t, js.DeleteKeyValue(ctx, heartbeatBucket),
		"deleting the heartbeat bucket must succeed")
	// time.Now() advances so the recreated stream gets a strictly-later Created.
	time.Sleep(60 * time.Millisecond)
	newKV, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  heartbeatBucket,
		Storage: jetstream.MemoryStorage,
		TTL:     cfg.HeartbeatTTL,
	})
	require.NoError(t, err, "recreating the heartbeat bucket must succeed")

	// Sanity: the recreate genuinely produced a later stream Created (otherwise
	// the epoch fence has nothing to detect and the probe would be vacuous).
	newCreated, err := kvutil.BucketStreamCreated(ctx, newKV)
	require.NoError(t, err)
	require.True(t, newCreated.After(origCreated),
		"recreate must produce a strictly-later backing-stream Created (orig=%s new=%s)",
		origCreated, newCreated)

	// NON-VACUITY (expected to pass on main): the epoch fence must fire at least
	// once for the recreated heartbeat bucket. If this never fires the whole
	// probe is meaningless.
	require.Eventually(t, func() bool { return np2BucketRecreatedDegrades.Load() >= 1 },
		6*time.Second, 100*time.Millisecond,
		"epoch fence never fired for the recreated heartbeat bucket — the probe is vacuous")

	// Observe the full window (>= 5 epoch ticks at OperationTimeout=1s) so the
	// recovery loop has ample opportunity to fight the fence back to Stable.
	time.Sleep(10 * time.Second)

	// Snapshot the final counts as evidence before asserting.
	recreatedDegrades := np2BucketRecreatedDegrades.Load()
	degradedToStable := np2DegradedToStable.Load()
	otherDegrades := np2OtherDegrades.Load()
	finalState := mgr.State()
	t.Logf("[%s] epoch-fence evidence: bucketRecreatedDegrades=%d degradedToStable=%d otherDegrades=%d finalState=%s connected=%v",
		t.Name(), recreatedDegrades, degradedToStable, otherDegrades, finalState, nc.IsConnected())

	// ISOLATION: no OTHER degrade reason (kv-unavailable / threshold) fired, so
	// nothing is masking or co-driving the epoch-fence path under test.
	require.Zero(t, otherDegrades,
		"a non-epoch degrade reason fired (%d) — it would mask/co-drive the epoch flap under test",
		otherDegrades)

	// ISOLATION: this is epoch-driven, not a connection drop. The NATS
	// connection must remain CONNECTED throughout.
	require.True(t, nc.IsConnected(),
		"the NATS connection must remain CONNECTED throughout (the flap is epoch-driven, not a disconnect)")

	// PRIMARY HARD INVARIANT (FAILS on main = the gap): once Degraded for the
	// recreated heartbeat bucket, the worker must NOT recover to Stable. The M4
	// contract is terminal Degraded for operator rotation. On main the recovery
	// loop exits to Stable on the still-intact assignment bucket, so this is
	// >= 1.
	require.Zero(t, degradedToStable,
		"M4 contract violated: worker recovered Degraded->Stable %d time(s) after a Parti-owned "+
			"bucket was recreated; the epoch fence never re-captures ep.created so it keeps "+
			"re-degrading while the recovery loop keeps exiting to Stable on the intact "+
			"assignment bucket — a flap instead of terminal Degraded",
		degradedToStable)

	// CORROBORATION (also fails on main): the worker must END Degraded.
	require.Equal(t, parti.StateDegraded, finalState,
		"after a Parti-owned bucket recreate the worker must remain terminally Degraded, not "+
			"oscillate back to %s", finalState)

	// Evidence note: >= 2 bucket-recreated entries proves oscillation, because
	// enterDegraded's degradedSince CAS rejects re-entry until an intervening
	// exit clears it. A second bucket-recreated entry therefore can only happen
	// after the worker exited Degraded in between. (Not asserted as a hard
	// invariant — the primary degradedToStable check is the load-bearing one;
	// this is logged above as corroborating evidence.)
}
