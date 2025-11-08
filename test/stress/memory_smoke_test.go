package stress_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestStressSmoke runs a very short load to validate that stress test infrastructure
// (external NATS helpers, load generator, resource monitor) still works. This test
// is intentionally fast (<5s) and always runs (even without PARTI_STRESS) to catch
// obvious regressions without invoking the full suite.
func TestStressSmoke(t *testing.T) {
	// Allow skip in -short to minimize CI latency if desired.
	if testing.Short() {
		t.Skip("Skipping smoke test in short mode")
	}

	// Use embedded NATS for speed; memory isolation not required for smoke.
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	// Small partition & worker counts keep resource usage trivial.
	lg := testutil.NewLoadGenerator(t, nc, 20)
	defer lg.Cleanup()

	ctx := context.Background()
	metrics := lg.RunLoadTest(ctx, testutil.LoadConfig{
		WorkerCount:    2,
		PartitionCount: 20,
		Duration:       3 * time.Second,
		SampleInterval: 500 * time.Millisecond,
		Description:    "stress_smoke",
	})

	// Basic assertions.
	require.Empty(t, metrics.Errors, "Smoke test should not produce errors")
	require.Greater(t, metrics.Duration(), 0*time.Millisecond)
}
