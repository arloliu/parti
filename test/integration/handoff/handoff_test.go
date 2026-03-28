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

// TestTwoPhase_NoBehaviorChange ensures enabling the flag does not change
// assignment results or introduce duplicates (baseline while stub is inert).
func TestTwoPhase_NoBehaviorChange(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: short mode")
	}

	ctx := context.Background()
	// Start NATS
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	parts := []parti.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}}
	src := source.NewStatic(parts)
	curStrategy := strategy.NewConsistentHash()

	// First manager without two-phase handoff
	cfg1 := parti.TestConfig()
	cfg1.EnableTwoPhaseHandoff = false
	mgr1, err := parti.NewManager(&cfg1, js, src, curStrategy)
	require.NoError(t, err)
	require.NoError(t, mgr1.Start(ctx))
	assign1 := waitForAssignment(t, mgr1, len(parts), 3*time.Second)
	require.Len(t, assign1.Partitions, len(parts)) // single worker gets all partitions
	require.NoError(t, mgr1.Stop(context.Background()))

	// Second manager with flag enabled (stub path). Expect identical partition set.
	cfg2 := parti.TestConfig()
	cfg2.EnableTwoPhaseHandoff = true
	mgr2, err := parti.NewManager(&cfg2, js, src, curStrategy)
	require.NoError(t, err)
	require.NoError(t, mgr2.Start(ctx))
	assign2 := waitForAssignment(t, mgr2, len(parts), 3*time.Second)
	require.Len(t, assign2.Partitions, len(parts))
	require.NoError(t, mgr2.Stop(context.Background()))

	// Compare the partition key sets (order doesn't matter with single worker case)
	keys1 := make(map[string]struct{}, len(assign1.Partitions))
	for _, p := range assign1.Partitions {
		keys1[p.Keys[0]] = struct{}{}
	}
	for _, p := range assign2.Partitions {
		_, ok := keys1[p.Keys[0]]
		require.True(t, ok, "partition key missing in stub assignment")
	}
}

// waitForAssignment polls CurrentAssignment until version > 0 and partition count matches expected.
func waitForAssignment(t *testing.T, mgr *parti.Manager, expectedCount int, timeout time.Duration) parti.Assignment {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		assign := mgr.CurrentAssignment()
		if assign.Version > 0 && len(assign.Partitions) == expectedCount {
			return assign
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("assignment not ready within %v", timeout)

	return parti.Assignment{}
}
