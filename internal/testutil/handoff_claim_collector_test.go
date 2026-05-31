package testutil

import (
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestWaitForAllStable_ReadsKVTruthNotWatcherLatest pins the load-robustness fix
// for the TestHandoffConflictStress flake. The gate must terminate on authoritative
// KV state even when the watcher-fed latest map is stale (showing a non-terminal
// state). This deterministically reproduces the CPU-starved CI failure mode: under
// load the collector's watcher goroutine lags behind KV, so latest still reads
// prepare/commit for a claim that KV has already moved to Stable. The old
// implementation trusted latest for present keys and never re-inspected KV, causing
// a false timeout. The fix reads InspectHandoffClaims each poll.
func TestWaitForAllStable_ReadsKVTruthNotWatcherLatest(t *testing.T) {
	ctx := t.Context()
	nc, cleanup := StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := fmt.Sprintf("itest-collector-kvtruth-%d", time.Now().UnixNano())

	// Authoritative KV state: the claim IS stable in KV.
	require.NoError(t, SeedHandoffStableClaim(ctx, js, bucket, "p01", "worker-0", 2*time.Minute))

	collector, err := NewHandoffClaimCollector(ctx, js, bucket)
	require.NoError(t, err)
	defer collector.Stop()

	// Simulate a lagging/coalescing watcher: latest still shows a non-terminal
	// state for the partition that KV has already moved to Stable. Same package,
	// so we can plant the stale value directly for determinism.
	collector.mu.Lock()
	collector.latest["p01"] = parti.HandoffClaim{
		PartitionID: "p01",
		Owner:       "worker-0",
		State:       parti.HandoffClaimPrepare,
	}
	collector.mu.Unlock()

	// Must return true off KV truth well within the deadline despite stale latest.
	// The pre-fix implementation would time out here.
	ok := collector.WaitForAllStable(ctx, []string{"p01"}, 2*time.Second)
	require.True(t, ok, "WaitForAllStable must read authoritative KV state, not stale watcher latest")
}
