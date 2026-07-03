package handoff_test

// Concurrency stress test for the scan gate's ticker goroutines, per the
// AGENTS.md "monitor goroutine on a ticker" rule: aggressive cadence,
// concurrent production KV traffic, and the race detector as the primary
// oracle. Patterned on
// test/integration/manager/manager_epoch_monitor_concurrency_test.go.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestHandoffScanGate_ConcurrentTrafficStress races the scan gate's probes
// against production KV traffic on the SAME bucket: gated ticker sweeps
// (coordinator) and gated ticker reconciles (resolver) running at an
// aggressive 20ms cadence — shorter than the components' own (unexported,
// still-2s-default) confirm gap, itself a valid stress shape — against
// concurrent PutIfEpoch writers, Apply-origin sweeps, and GetOwner/
// ForceRefreshPartition readers.
//
// Every component keeps the same per-component handle discipline as the
// flatness test: a dedicated production handle and a dedicated probe
// handle, each its own js.KeyValue(ctx, bucket) call — the same discipline
// that fixed the epoch-monitor race class this test's template pins.
//
// The primary oracle is the race detector (run this file under -race); the
// secondary oracle is a final convergence check that each written claim's
// owner resolves via a fresh Get once the stress traffic stops.
func TestHandoffScanGate_ConcurrentTrafficStress(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	bucket := fmt.Sprintf("itest-handoff-scan-gate-stress-%d", time.Now().UnixNano())
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	const numWorkers = 3
	const cadence = 20 * time.Millisecond
	components := make([]scanGateComponent, numWorkers)
	for i := range components {
		components[i] = newScanGateComponent(t, ctx, js, bucket, cadence)
	}

	pids := make([]string, numWorkers)
	for i := range pids {
		pid := fmt.Sprintf("cx-p%d", i)
		pids[i] = pid
		_, err := components[i].store.PutIfEpoch(ctx, pid, 0,
			handoff.NewInitialClaim(pid, fmt.Sprintf("w%d-0", i), time.Now(), time.Minute))
		require.NoError(t, err)
	}

	for i := range components {
		components[i].coord.Start(ctx)
		require.NoError(t, components[i].resolver.Start(ctx))
	}
	t.Cleanup(func() {
		for i := range components {
			components[i].resolver.Stop()
		}
	})

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// One goroutine per worker writes/updates claims through
	// ClaimStore.PutIfEpoch at ~10ms cadence.
	for i := range numWorkers {
		wg.Go(func() {
			runClaimWriter(ctx, stop, components[i].store, pids[i], i)
		})
	}

	// One goroutine per worker calls coordinator.Apply with a small,
	// unchanging assignment at ~50ms cadence — exercising Apply-origin
	// sweeps racing gated ticker sweeps on sweepMu.
	for i := range numWorkers {
		wg.Go(func() {
			runApplyLoop(ctx, stop, components[i].coord, pids[i], i)
		})
	}

	// One goroutine per resolver calls GetOwner + ForceRefreshPartition at
	// ~10ms cadence.
	for i := range numWorkers {
		wg.Go(func() {
			runResolverReader(ctx, stop, components[i].resolver, pids[i])
		})
	}

	time.Sleep(5 * time.Second)
	close(stop)
	wg.Wait()

	// The concurrent soak's oracle is the race detector: no soak goroutine
	// asserts on t, so a data race surfaces only as the -race binary's
	// non-zero exit (check stderr for WARNING: DATA RACE blocks). The
	// convergence check below is the functional oracle.

	// Final convergence: each written claim's owner resolves via a fresh
	// Get, while the resolvers' background loops are still running (their
	// ticker/watcher must catch up to the last write within a handful of
	// cadences).
	for i, pid := range pids {
		want, _, err := components[i].store.Get(ctx, pid)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			owner, _, _, ok := components[i].resolver.GetOwner(pid)

			return ok && owner == want.Owner
		}, 5*time.Second, 25*time.Millisecond,
			"resolver %d did not converge to the final claim owner for %s", i, pid)
	}
}

// runClaimWriter drives ~10ms-cadence PutIfEpoch churn against pid until
// stop closes. It tracks the epoch it last wrote so successive CAS calls
// keep succeeding against its own writes (no other writer touches this
// pid); a defensive re-read on CAS failure keeps the loop alive if it ever
// does lose a race.
func runClaimWriter(ctx context.Context, stop <-chan struct{}, store handoff.ClaimStore, pid string, workerIdx int) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	epoch := int64(1)
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		next := handoff.Claim{
			PartitionID: pid,
			Owner:       fmt.Sprintf("w%d-%d", workerIdx, i),
			State:       handoff.ClaimStateStable,
			Epoch:       epoch + 1,
			LastUpdated: time.Now().UTC(),
			TTLSeconds:  int64(time.Minute.Seconds()),
		}
		if _, err := store.PutIfEpoch(ctx, pid, epoch, next); err != nil {
			if cur, _, gerr := store.Get(ctx, pid); gerr == nil {
				epoch = cur.Epoch
			}

			continue
		}
		epoch = next.Epoch
	}
}

// runApplyLoop calls coord.Apply with an unchanging small assignment
// (identical previous/next, so no prepare/commit/stabilize churn) at
// ~50ms cadence until stop closes. Apply runs an opportunistic
// Apply-origin sweep unconditionally at its top — never scan-gated — so
// this races the gated ticker sweep on the coordinator's sweepMu exactly
// as production does when Apply and the sweep ticker fire concurrently.
func runApplyLoop(ctx context.Context, stop <-chan struct{}, coord handoff.Coordinator, pid string, workerIdx int) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	assignment := types.Assignment{
		Version:    1,
		Partitions: []types.Partition{{Keys: []string{pid}}},
	}
	workerID := fmt.Sprintf("w%d", workerIdx)
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		_ = coord.Apply(ctx, workerID, assignment, assignment)
	}
}

// runResolverReader calls GetOwner + ForceRefreshPartition at ~10ms
// cadence until stop closes, exercising the resolver's read paths
// concurrently with its own gated reconciler goroutine.
func runResolverReader(ctx context.Context, stop <-chan struct{}, resolver *durable.ClaimBasedResolver, pid string) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		resolver.GetOwner(pid)
		_ = resolver.ForceRefreshPartition(ctx, pid)
	}
}
