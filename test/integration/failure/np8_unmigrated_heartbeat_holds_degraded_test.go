package failure_test

import (
	"context"
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

// TestNP8FleetNATSOutage_UnmigratedHeartbeatHoldsDegraded proves the
// un-migrated / existing-cluster path for NP-8 mechanism 2: when the
// heartbeat bucket was pre-created as MemoryStorage (simulating an existing
// cluster that has not yet migrated to FileStorage), a single-node NATS restart
// loses the heartbeat stream.
//
// WITHOUT the heartbeat-bucket reachability guard, the worker degrades with the
// whole-bucket-loss reason "KV error threshold exceeded" (not "kv-unavailable"),
// so the reason-scoped Family B gate is silent, and the recreate-only Family A
// epoch re-probe does not fire for a missing stream (not a recreate). The
// recovery exit succeeds on the unaffected assignment read and the fleet flaps
// Degraded<->Stable.
//
// WITH the reachability guard, the recovery exit refuses to proceed while the
// heartbeat bucket's stream is missing/unreachable, holding the worker
// terminally Degraded for rotation. On rotate the lost bucket no longer
// exists, so the new pod re-creates it as FileStorage — completing the
// migration automatically. This test asserts only the guard: terminal Degraded,
// no flap.
//
// Verify-first order: on the conjunct-less parent the require.Never(anyStable)
// assertion trips because the fleet flaps back to Stable after the restart.
func TestNP8FleetNATSOutage_UnmigratedHeartbeatHoldsDegraded(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	// Shared restartable connection (unlimited reconnect, stable port,
	// FileStorage StoreDir) — same helpers as the sibling NP-8 tests.
	nc, stopNATS, restartNATS := startEmbeddedNATSOutage(t, -1)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)
	// Build the events stream on FileStorage so it survives the restart.
	rfBuildStream(t, ctx, realJS)

	cfg := testutil.IntegrationTestConfig()
	cfg.DegradedBehavior.EnterThreshold = 500 * time.Millisecond
	cfg.DegradedBehavior.ExitThreshold = 500 * time.Millisecond
	// Raise WorkerIDTTL well above the outage window so the stableID claim
	// survives (the claim-loss self-stop of mechanism 1 does NOT fire),
	// isolating the heartbeat-bucket-loss path under test.
	cfg.WorkerIDTTL = 30 * time.Second

	// Pre-create the heartbeat bucket as MemoryStorage BEFORE starting
	// workers, so parti's get-first EnsureKVBucketWithRetry honors the
	// existing bucket (the "un-migrated existing cluster" path) even though
	// the production default is now FileStorage.
	_, err = realJS.CreateKeyValue(ctx, parti.BuildControlPlaneKVConfig(
		cfg.KVBuckets.HeartbeatBucket, cfg.HeartbeatTTL, jetstream.MemoryStorage))
	require.NoError(t, err, "pre-create heartbeat bucket as MemoryStorage")

	src := source.NewStatic(testutil.CreateTestPartitions(6))
	cluster := testutil.NewWorkerClusterWithSource(t, nc, src, cfg)
	for range 3 {
		cluster.AddWorkerWithOptions(ctx, parti.WithHooks(&parti.Hooks{}))
	}
	cluster.StartWorkers(ctx)

	require.NotNil(t, cluster.WaitForLeader(15*time.Second), "baseline: one leader")
	cluster.WaitForPartitionCoverage(6, 15*time.Second)

	workers := cluster.GetActiveWorkers()
	require.Len(t, workers, 3)

	allDegraded := func() bool {
		for _, m := range workers {
			if m.State() != types.StateDegraded {
				return false
			}
		}
		return true
	}
	anyStable := func() bool {
		for _, m := range workers {
			if m.State() == types.StateStable {
				return true
			}
		}
		return false
	}

	// Outage shorter than WorkerIDTTL (claim survives) but long enough to
	// lose the MemoryStorage heartbeat stream on restart.
	stopNATS(t)
	require.Eventually(t, func() bool { return nc.Status() != nats.CONNECTED },
		5*time.Second, 50*time.Millisecond, "fleet did not observe NATS outage")
	time.Sleep(5 * time.Second)
	restartNATS(t)
	require.Eventually(t, func() bool { return nc.Status() == nats.CONNECTED },
		5*time.Second, 50*time.Millisecond, "fleet did not reconnect after NATS restart")

	// All workers must enter Degraded due to the lost MemoryStorage heartbeat
	// bucket ("KV error threshold exceeded" whole-bucket-loss reason).
	require.Eventually(t, allDegraded, 30*time.Second, 200*time.Millisecond,
		"un-migrated MemoryStorage heartbeat loss must drive the whole fleet Degraded")

	// The heartbeat-bucket reachability guard must HOLD terminal Degraded —
	// no flap back to Stable. On the conjunct-less parent the recovery exit
	// heals on the unaffected assignment read and this assertion trips.
	require.Never(t, anyStable, 8*time.Second, 200*time.Millisecond,
		"the reachability guard must HOLD terminal Degraded — no flap back to Stable")

	require.True(t, nc.IsConnected(),
		"the NATS connection must be CONNECTED throughout (this is a missing-bucket, not a connectivity, loss)")
}
