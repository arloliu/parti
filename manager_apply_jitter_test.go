package parti

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestApplyAssignmentWithPrev_JitterApplied verifies that when
// ApplyStartJitter is set, applyAssignmentWithPrev sleeps for a duration
// in [0, ApplyStartJitter) before invoking handoffCoordinator.Apply.
// The sampler seam is used to pin the jitter to a deterministic value so the
// assertion proves the sleep actually happened (elapsed >= forced), not merely
// that the path was reached.
func TestApplyAssignmentWithPrev_JitterApplied(t *testing.T) {
	const jitter = 200 * time.Millisecond
	const forced = 100 * time.Millisecond // well within jitter cap; test completes quickly
	m := newTestManagerWithJitter(t, jitter)

	// Force a deterministic sample so elapsed proves the sleep occurred.
	m.applyJitterSampler = func(time.Duration) time.Duration { return forced }

	var observed atomic.Int64
	rc := &recordingCoordinator{}
	m.handoffCoordinator = rc

	start := time.Now()
	rc.onApply = func(_ context.Context) {
		observed.Store(time.Since(start).Nanoseconds())
	}
	_ = m.applyAssignment(Assignment{Version: 1})

	elapsed := time.Duration(observed.Load())
	require.GreaterOrEqual(t, elapsed, forced, "sleep must have actually happened")
	require.LessOrEqual(t, elapsed, forced+50*time.Millisecond, "elapsed must not compound beyond forced+50ms")
}

// TestApplyAssignmentWithPrev_JitterZeroIsNoop verifies that the default
// jitter=0 introduces no measurable delay.
func TestApplyAssignmentWithPrev_JitterZeroIsNoop(t *testing.T) {
	m := newTestManagerWithJitter(t, 0)

	var observed atomic.Int64
	rc := &recordingCoordinator{}
	m.handoffCoordinator = rc

	start := time.Now()
	rc.onApply = func(_ context.Context) {
		observed.Store(time.Since(start).Nanoseconds())
	}
	_ = m.applyAssignment(Assignment{Version: 1})

	require.Less(t, time.Duration(observed.Load()), 5*time.Millisecond)
}

// TestApplyAssignmentWithPrev_JitterCancelledByCtx verifies that ctx
// cancellation during the jitter sleep aborts the apply and Apply was
// never invoked. The sampler seam forces a 5s sleep so the 50ms cancel
// deterministically races the sleep regardless of PRNG output.
func TestApplyAssignmentWithPrev_JitterCancelledByCtx(t *testing.T) {
	m := newTestManagerWithJitter(t, 5*time.Second)

	rc := &recordingCoordinator{}
	m.handoffCoordinator = rc

	// Force a deterministic long sleep so cancellation always races it.
	m.applyJitterSampler = func(time.Duration) time.Duration { return 5 * time.Second }

	go func() {
		time.Sleep(50 * time.Millisecond)
		m.cancel() // simulate Stop's ctx cancellation; fixture is not Stop-safe
	}()

	start := time.Now()
	err := m.applyAssignment(Assignment{Version: 1})
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, elapsed, 1*time.Second)
	require.Zero(t, rc.applyCount.Load(), "Apply must not be called when ctx is cancelled mid-jitter")
}

// TestApplyAssignmentWithPrev_JitterNoRaceUnderConcurrentEntrants exercises
// the race-detector requirement raised by plan-review v1 P0. Two goroutines
// invoke applyAssignment concurrently with jitter enabled. Under -race,
// any shared-state write that escaped applyStoreMu would be flagged.
func TestApplyAssignmentWithPrev_JitterNoRaceUnderConcurrentEntrants(t *testing.T) {
	m := newTestManagerWithJitter(t, 50*time.Millisecond)

	rc := &recordingCoordinator{}
	m.handoffCoordinator = rc

	var wg sync.WaitGroup
	wg.Go(func() {
		_ = m.applyAssignment(Assignment{Version: 1})
	})
	wg.Go(func() {
		_ = m.applyAssignment(Assignment{Version: 2})
	})
	wg.Wait()

	// At least the higher version must have been applied; the lower
	// version may be dropped by the stale gate. The point of the test is
	// the race detector, not the count.
	require.GreaterOrEqual(t, rc.applyCount.Load(), int64(1))
}

// TestApplyAssignmentRetry_DoesNotJitter proves that scheduleApplyRetry's
// retry goroutine routes through applyAssignmentWithPrevSkipJitter (the
// non-jittering sibling), so a fleet-wide ApplyStartJitter does NOT
// compound on top of the retry's own exponential backoff.
func TestApplyAssignmentRetry_DoesNotJitter(t *testing.T) {
	const jitter = 2 * time.Second
	m := newTestManagerWithJitter(t, jitter)

	// Count fresh-wrapper jitter entries via the test hook. Hook fires
	// BEFORE the sleep so the test does not block on it.
	var jitterFires atomic.Int64
	m.testHookApplyJittered = func() { jitterFires.Add(1) }

	// Coordinator: first Apply fails (n==1 <= failUntilCount==1), second
	// succeeds. Deterministic, no mid-flight mutation.
	rc := &recordingCoordinator{}
	rc.failUntilCount.Store(1)
	m.handoffCoordinator = rc

	// Drive a fresh-version apply. It fails, scheduleApplyRetry queues
	// a retry. The retry succeeds, terminating the loop.
	go func() { _ = m.applyAssignment(Assignment{Version: 1}) }()

	// Wait for both attempts to complete.
	require.Eventually(t, func() bool {
		return rc.applyCount.Load() >= 2
	}, 10*time.Second, 50*time.Millisecond,
		"expected fresh attempt + retry; saw applyCount=%d", rc.applyCount.Load())

	// The fresh attempt jittered once. The retry must NOT jitter; total
	// expected is exactly 1.
	require.Equal(t, int64(1), jitterFires.Load(),
		"scheduleApplyRetry must route through SkipJitter; jitter hook fired %d times (expected 1 fresh attempt only)",
		jitterFires.Load())
}
