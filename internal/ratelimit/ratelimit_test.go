package ratelimit_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

// TestNilLimiterNeverBlocks verifies that a nil Limiter never blocks.
func TestNilLimiterNeverBlocks(t *testing.T) {
	ctx := t.Context()
	err := ratelimit.Wait(ctx, nil)
	require.NoError(t, err)
}

// TestBurstPassesImmediately verifies that tokens up to burst size are granted
// without delay. Uses ReserveN + DelayFrom to inspect without sleeping.
func TestBurstPassesImmediately(t *testing.T) {
	const burst = 5
	const perSec = 1.0 // low steady rate; burst dominates

	// Use the raw rate.Limiter directly to inspect timing without wall-clock sleep.
	rl := rate.NewLimiter(rate.Limit(perSec), burst)
	t0 := time.Now()

	for i := range burst {
		r := rl.ReserveN(t0, 1)
		delay := r.DelayFrom(t0)
		assert.Equal(t, time.Duration(0), delay, "token %d should be burst-absorbed (delay == 0)", i)
	}
}

// TestThrottleDelayAfterBurst verifies that the (burst+1)th reservation has a
// positive delay equal to 1/rate. No wall-clock sleep is used.
func TestThrottleDelayAfterBurst(t *testing.T) {
	const burst = 3
	const perSec = 2.0

	rl := rate.NewLimiter(rate.Limit(perSec), burst)
	t0 := time.Now()

	// Consume the burst.
	for range burst {
		r := rl.ReserveN(t0, 1)
		_ = r.DelayFrom(t0) // burst-absorbed; ignore
	}

	// The next reservation must incur a delay.
	r := rl.ReserveN(t0, 1)
	delay := r.DelayFrom(t0)
	assert.Greater(t, delay, time.Duration(0), "(burst+1)th token must have positive delay")
	// The delay should be approximately 1/rate = 500ms.
	expectedMin := time.Duration(float64(time.Second) / perSec)
	assert.GreaterOrEqual(t, delay, expectedMin/2, "delay should be roughly 1/rate")
}

// TestWaitReturnsCtxErrOnCancelledCtx verifies that Wait returns ctx.Err()
// when the context is already cancelled and the limiter would block.
func TestWaitReturnsCtxErrOnCancelledCtx(t *testing.T) {
	const burst = 1
	const perSec = 0.001 // effectively blocks after first token
	l := ratelimit.New(perSec, burst, nil)

	// Exhaust the burst so the next Wait would block.
	err := l.Wait(t.Context())
	require.NoError(t, err, "first Wait (burst token) should succeed")

	// Now pre-cancel a new ctx and verify Wait returns ctx.Err().
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // pre-cancel

	err = l.Wait(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

// TestWaitCancelDuringWait verifies that Wait returns when the context is
// cancelled while it is waiting. We use a rate so slow that without
// cancellation we'd wait ~1000s (effectively forever).
//
// Instead of time.Sleep, we use an observer-channel seam: the observer's
// IncrementThrottled fires inside Wait right before the timer select, so
// receiving the channel signal guarantees the goroutine has entered the wait
// and the cancel will race the correct select branch.
func TestWaitCancelDuringWait(t *testing.T) {
	// Signal channel: closed when the goroutine has entered the timer select.
	entered := make(chan struct{})

	obs := &signalObserver{signal: entered}
	// rate 0.001/s → each token takes ~1000s without cancel.
	l := ratelimit.New(0.001, 1, obs)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		// Exhaust the single burst token (immediate, no observer fire).
		_ = l.Wait(context.Background())
		// This call blocks; observer fires before the timer select.
		done <- l.Wait(ctx)
	}()

	// Wait until the goroutine signals it is inside the timer wait,
	// then cancel the context.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not enter the waiting state in time")
	}
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after context cancellation")
	}
}

// signalObserver closes a channel the first time IncrementThrottled is called,
// signalling that Wait is about to enter the timer select.
type signalObserver struct {
	once   sync.Once
	signal chan struct{}
}

func (s *signalObserver) IncrementThrottled() {
	s.once.Do(func() { close(s.signal) })
}

func (s *signalObserver) ObserveWait(_ float64) {}

// TestThrottleObserverCalledOnlyOnActualWait verifies that the observer is
// called for delayed waits but not for burst-absorbed ones.
func TestThrottleObserverCalledOnlyOnActualWait(t *testing.T) {
	obs := &fakeObserver{}

	const burst = 3
	const perSec = 100.0
	l := ratelimit.New(perSec, burst, obs)
	ctx := t.Context()

	// Drain burst — all burst-absorbed, observer should NOT fire.
	for range burst {
		err := l.Wait(ctx)
		require.NoError(t, err)
	}
	assert.Equal(t, 0, obs.increments, "burst-absorbed waits must not trigger observer")

	// Now drive the positive-delay path deterministically. A tight ~1ms timing
	// window would flake if the goroutine is paused long enough for the token to
	// replenish (delay <= 0, observer skipped). Instead use a slow rate so the
	// second Wait is guaranteed to block, an observer that reports the wait the
	// moment it fires (right before the timer select), then cancel so the test
	// never sleeps the full delay.
	obs2 := &recordingSignalObserver{waitCh: make(chan float64, 1)}
	l2 := ratelimit.New(1.0, 1, obs2) // 1/s, burst 1 → the second Wait blocks ~1s
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()

	require.NoError(t, l2.Wait(context.Background())) // drain burst token (immediate, no observer)
	require.Equal(t, int64(0), obs2.increments.Load(), "burst-absorbed wait must not trigger observer")

	done := make(chan error, 1)
	go func() { done <- l2.Wait(ctx2) }()

	select {
	case s := <-obs2.waitCh:
		assert.Greater(t, s, 0.0, "observed wait must be positive")
	case <-time.After(2 * time.Second):
		t.Fatal("observer was not called for the delayed wait")
	}
	cancel2()
	<-done // reap the goroutine (Wait returns context.Canceled once cancelled)

	assert.Equal(t, int64(1), obs2.increments.Load(), "delayed wait must trigger the observer exactly once")
}

// recordingSignalObserver counts throttle events atomically and reports the
// observed wait duration on waitCh from ObserveWait, which the limiter invokes
// right before entering the timer select. A test can therefore prove the
// positive-delay observer path was exercised without a fragile timing window.
type recordingSignalObserver struct {
	increments atomic.Int64
	waitCh     chan float64
}

func (o *recordingSignalObserver) IncrementThrottled()   { o.increments.Add(1) }
func (o *recordingSignalObserver) ObserveWait(s float64) { o.waitCh <- s }

type fakeObserver struct {
	increments int
	waits      []float64
}

func (f *fakeObserver) IncrementThrottled()   { f.increments++ }
func (f *fakeObserver) ObserveWait(s float64) { f.waits = append(f.waits, s) }
