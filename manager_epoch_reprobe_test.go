package parti

import (
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestEpochMismatchOutstanding_BehaviorAndTiming is the focused unit + WI-3
// timing guard for the Family A recovery-exit re-probe. attemptRecoveryFromDegraded
// runs on the 1s connection-monitor goroutine, so epochMismatchOutstanding's
// per-tick blocking probe I/O is a timing concern (a real stall risk on health
// detection if a bucket is unreachable). This test pins three regimes:
//   - reachable + unchanged buckets: returns false, FAST (the realistic recovery
//     scenario — buckets are reachable because the exit only runs when connected).
//   - a recreated bucket: returns true (the wipe-and-recreate the gate must catch).
//   - an unreachable server: returns false (probe errors are skipped, not
//     actionable) and is BOUNDED by ~len(buckets) * OperationTimeout — it must not
//     hang the connection-monitor goroutine.
func TestEpochMismatchOutstanding_BehaviorAndTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-NATS test in short mode")
	}

	m, _, _, _ := newTestManager(t)
	srv, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	m.js = js
	m.cfg.OperationTimeout = 2 * time.Second

	ctx := m.ctx
	buckets := []string{"wi3-b0", "wi3-b1", "wi3-b2", "wi3-b3"}
	for _, b := range buckets {
		kv := partitest.CreateJetStreamKV(t, nc, b)
		m.captureBucketEpoch(ctx, b, kv)
	}
	require.Len(t, m.bucketEpochs, len(buckets), "all bucket epochs must be captured")

	// Regime 1: reachable + unchanged -> false, and fast (well under one
	// OperationTimeout for all four buckets combined).
	start := time.Now()
	require.False(t, m.epochMismatchOutstanding(ctx),
		"unchanged reachable buckets must not report a recreate")
	reachableLatency := time.Since(start)
	t.Logf("WI-3 timing: epochMismatchOutstanding over %d reachable buckets = %s", len(buckets), reachableLatency)
	require.Less(t, reachableLatency, m.cfg.OperationTimeout,
		"the reachable-bucket re-probe must be fast (a few round-trips), well under one OperationTimeout")

	// Regime 2: recreate one bucket -> the live Created differs -> true.
	require.NoError(t, js.DeleteKeyValue(ctx, buckets[1]))
	_ = partitest.CreateJetStreamKV(t, nc, buckets[1])
	require.True(t, m.epochMismatchOutstanding(ctx),
		"a recreated bucket's live Created must mismatch the captured epoch -> outstanding")

	// Regime 3 (the WI-3 worst case): the server is gone -> every probe errors and
	// is skipped (returns false), and the whole call is BOUNDED, not a hang. Use a
	// small OperationTimeout so the bound is tight and the test is quick.
	m.cfg.OperationTimeout = 300 * time.Millisecond
	srv.Shutdown()
	srv.WaitForShutdown()

	start = time.Now()
	require.False(t, m.epochMismatchOutstanding(ctx),
		"with the server down every probe errors and is skipped (not actionable) -> false")
	downLatency := time.Since(start)
	t.Logf("WI-3 timing: epochMismatchOutstanding with server DOWN over %d buckets = %s (OperationTimeout=%s each)",
		len(buckets), downLatency, m.cfg.OperationTimeout)
	// Hard upper bound: the per-bucket context.WithTimeout caps each probe, so the
	// total inline block on the connection-monitor goroutine cannot exceed
	// len(buckets)*OperationTimeout plus a small scheduling margin. This is the
	// guarantee that the re-probe degrades gracefully rather than wedging.
	maxBound := time.Duration(len(buckets))*m.cfg.OperationTimeout + 2*time.Second
	require.Less(t, downLatency, maxBound,
		"epochMismatchOutstanding must stay bounded by len(buckets)*OperationTimeout when buckets are unreachable, not hang")
}
