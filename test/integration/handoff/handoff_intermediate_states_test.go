package handoff_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// testUpdater is a no-op updater to enable two-phase flows.
type testUpdater struct{}

func (testUpdater) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []parti.Partition) error {
	return nil
}

// waitForClaimState polls InspectHandoffClaims until any claim for the given partition
// reaches the target state, or the context is done.
func waitForClaimState(ctx context.Context, js jetstream.JetStream, bucket, partitionID string, state parti.HandoffClaimState) (parti.HandoffClaim, bool) {
	deadline, _ := ctx.Deadline()
	for {
		claims, err := parti.InspectHandoffClaims(ctx, js, bucket)
		if err == nil {
			for _, c := range claims {
				if c.PartitionID == partitionID && c.State == state {
					return c, true
				}
			}
		}
		if time.Now().After(deadline) {
			return parti.HandoffClaim{}, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHandoffIntermediateStates_DelaysExposePrepareCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}

	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Pre-create the handoff bucket and seed a stable claim owned by another worker (worker-0).
	bucket := "itest-handoff-states"
	partition := parti.Partition{Keys: []string{"p1"}}
	err = testutil.SeedHandoffStableClaim(ctx, js, bucket, partition.ID(), "worker-0", 2*time.Minute)
	require.NoError(t, err)

	cfg := parti.TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = bucket
	cfg.KVBuckets.HandoffTTL = 2 * time.Minute
	// Force our worker to avoid worker-0 so prepare triggers.
	cfg.WorkerIDMin = 1
	cfg.WorkerIDMax = 2
	// Expose intermediate states with delays.
	cfg.Handoff.DelayAfterPrepare = 300 * time.Millisecond
	cfg.Handoff.DelayBeforeStable = 300 * time.Millisecond

	src := source.NewStatic([]parti.Partition{partition})
	curStrategy := strategy.NewConsistentHash()

	mgr, err := parti.NewManager(&cfg, js, src, curStrategy, parti.WithWorkerConsumerUpdater(testUpdater{}))
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	// Phase 4 made the initial-bootstrap apply synchronous-before-StateStable,
	// so the whole prepare → commit → stable sequence is complete by the
	// time Start returns. The DelayAfterPrepare / DelayBeforeStable still
	// run, they just run inside Start's call stack. We can no longer
	// reliably poll for the intermediate states from a test running after
	// Start returns; verify the terminal stable state and ownership swap.
	stableCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c, ok := waitForClaimState(stableCtx, js, bucket, partition.ID(), parti.HandoffClaimStable)
	require.True(t, ok, "expected stable state to become visible")
	require.Equal(t, mgr.WorkerID(), c.Owner, "ownership should switch to our worker")
}
