package manager_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/ratelimit"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
)

// notifyingLimiter wraps a real token-bucket limiter and signals a channel on
// the first Wait call so the test can detect when the paced apply has begun.
type notifyingLimiter struct {
	mu    sync.Mutex
	once  bool
	ready chan struct{} // closed on first Wait
	inner ratelimit.Limiter
}

func newNotifyingLimiter(l ratelimit.Limiter) *notifyingLimiter {
	return &notifyingLimiter{inner: l, ready: make(chan struct{})}
}

func (n *notifyingLimiter) Wait(ctx context.Context) error {
	n.mu.Lock()
	if !n.once {
		n.once = true
		close(n.ready)
	}
	n.mu.Unlock()

	return n.inner.Wait(ctx)
}

// TestPacedColdStart_StopUnwindsMidPace verifies that Stop() cancels a running
// paced apply promptly by returning the limiter's Wait(ctx) error when m.ctx is
// cancelled. The test waits (event-driven) until the first limiter.Wait is called
// (proving the apply has begun), then calls Stop() and asserts it returns promptly.
//
// # Design choice — fallback from original spec
//
// The original spec aimed to assert that a paced apply trips the StartupTimeout
// watchdog (DegradeReasonStartupTimeout). In practice the watchdog only fires when
// state is still StateWaitingAssignment at the deadline. However, the calculator
// transitions the manager from StateWaitingAssignment to StateScaling during
// ColdStartWindow before dc.Update() is called (the calculator runs concurrently
// and emits CalcStateScaling, which syncStateFromCalculator maps to StateScaling).
// So the watchdog condition (state == StateWaitingAssignment) is never true during
// the paced apply, and the startup-timeout degraded path cannot be reliably triggered
// by apply pacing alone.
//
// The test therefore falls back to: event-driven detection that the paced apply
// has started, then a Stop() that proves the cancellation path through limiter.Wait.
//
// # Scope / not tested here
//
//   - StartupTimeout watchdog via paced apply: not reliably triggerable; see above.
//   - Two-worker gate-on/off overlap timing: too brittle for deterministic assertion.
//   - Per-attempt retry injection: covered by jsutil and internal/durable unit tests.
//   - Recovery-storm pacing: covered by the shared-budget unit test.
func TestPacedColdStart_StopUnwindsMidPace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const streamName = "PCSTT"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"pcstt.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	})

	// N=20, rate=3/s, burst=1 → apply floor ≈ (20-1)/3 ≈ 6.3s.
	// This floor is much longer than the Stop() window below, proving cancellation.
	const n = 20
	nl := newNotifyingLimiter(ratelimit.New(3, 1, nil))

	dc, err := consumer.NewDynamic(js, streamName, "pcstt-worker", "pcstt.{{.PartitionID}}", handler,
		consumer.WithConsumerCreateLimiter(nl),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	// Build the partition list for the Manager's source.
	partitions := make([]parti.Partition, n)
	for i := range n {
		partitions[i] = parti.Partition{Keys: []string{strconv.Itoa(i)}}
	}

	// Use TestConfig (fast timings) with a large StartupTimeout so the watchdog
	// does not fire during the paced apply.
	cfg := parti.TestConfig()
	cfg.WorkerIDPrefix = "pcstt-w"
	cfg.WorkerIDMax = 10
	cfg.StartupTimeout = 60 * time.Second

	src := source.NewStatic(partitions)
	chStrat := strategy.NewConsistentHash()

	mgr, err := parti.NewManager(&cfg, js, src, chStrat,
		parti.WithWorkerConsumerUpdater(dc),
	)
	require.NoError(t, err)

	// Start returns at StateWaitingAssignment; background runner will wait for
	// the calculator to publish assignments (after ColdStartWindow=1s), then call
	// dc.Update() (paced at 3/s).
	startCtx, startCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer startCancel()
	require.NoError(t, mgr.Start(startCtx))

	// Event-driven: wait until the first limiter.Wait fires, proving the paced
	// apply has begun in the background runner.
	select {
	case <-nl.ready:
		t.Log("paced apply started (first limiter.Wait observed); calling Stop")
	case <-time.After(15 * time.Second):
		t.Fatal("paced apply did not start within 15s")
	}

	// Stop immediately: m.ctx is cancelled → limiter.Wait(ctx) returns ctx.Err()
	// → dc.Update returns → the background runner unwinds.
	// The stop context is generous (10s) but the actual unwind should be nearly
	// instantaneous because Wait is blocking on a timer that is cancelled by m.ctx.
	stopCtx, stopCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer stopCancel()

	stopStart := time.Now()
	err = mgr.Stop(stopCtx)
	stopElapsed := time.Since(stopStart)

	// Stop must complete well within the 10s stop context.  An upper bound of 5s is
	// generous headroom; in practice Stop should return in <1s once m.ctx is cancelled.
	require.LessOrEqual(t, stopElapsed, 5*time.Second,
		"Stop must return promptly when m.ctx cancels the paced apply; got %v", stopElapsed)

	// Stop returns nil when the shutdown is clean; DeadlineExceeded is also acceptable
	// if the stop context races with a very slow final goroutine cleanup.
	if err != nil {
		require.ErrorIs(t, err, context.DeadlineExceeded,
			"Stop may return DeadlineExceeded; no other error expected")
	}

	t.Logf("Stop returned after %v (err=%v); paced apply floor ~6.3s without cancellation", stopElapsed, err)
}

// TestPacedColdStart_ReachesStable verifies that with a moderate rate limiter,
// the manager-driven paced apply completes and the manager reaches Stable.
// This is the "happy path" end-to-end proof that the limiter is integrated correctly
// through the full Manager → Dynamic consumer → per-partition create pipeline.
func TestPacedColdStart_ReachesStable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	const streamName = "PCSRS"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"pcsrs.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error {
		return nil
	})

	// N=10, rate=20/s, burst=5 → apply floor = (10-5)/20 = 0.25s (fast enough to complete).
	const n = 10
	limiter := ratelimit.New(20, 5, nil)

	dc, err := consumer.NewDynamic(js, streamName, "pcsrs-worker", "pcsrs.{{.PartitionID}}", handler,
		consumer.WithConsumerCreateLimiter(limiter),
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	partitions := make([]parti.Partition, n)
	for i := range n {
		partitions[i] = parti.Partition{Keys: []string{strconv.Itoa(i)}}
	}

	cfg := parti.TestConfig()
	cfg.WorkerIDPrefix = "pcsrs-w"
	cfg.WorkerIDMax = 10
	cfg.StartupTimeout = 30 * time.Second

	src := source.NewStatic(partitions)
	chStrat := strategy.NewConsistentHash()

	mgr, err := parti.NewManager(&cfg, js, src, chStrat,
		parti.WithWorkerConsumerUpdater(dc),
	)
	require.NoError(t, err)

	startCtx, startCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer startCancel()
	require.NoError(t, mgr.Start(startCtx))

	// Wait for the manager to reach Stable state, indicating the paced apply completed.
	require.NoError(t, <-mgr.WaitState(parti.StateStable, 30*time.Second),
		"manager must reach Stable after paced apply completes")

	require.Equal(t, parti.StateStable, mgr.State())
	t.Logf("manager reached Stable after paced apply (N=%d, rate=20/s, burst=5)", n)

	stopCtx, stopCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopCancel()
	require.NoError(t, mgr.Stop(stopCtx))
}
