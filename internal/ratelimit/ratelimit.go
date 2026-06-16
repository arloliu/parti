// Package ratelimit provides a minimal token-bucket rate limiter abstraction
// for bounding per-worker JetStream consumer-create RPC rates.
//
// The primary production implementation wraps [golang.org/x/time/rate.Limiter].
// All types in this package are nil-safe: a nil [Limiter] value is treated as
// unlimited and never waits.
package ratelimit

import (
	"context"
	"time"

	"golang.org/x/time/rate"
)

// Limiter is the interface callers use to gate RPC attempts.
//
// Wait blocks until the limiter grants a token or the context is cancelled.
// A nil Limiter is treated as unlimited (see [Wait]).
//
// Contract (per D5): Wait is invoked while internal locks such as
// applyStoreMu and updateMu may be held. Implementations MUST honour
// context cancellation and MUST NOT call back into Manager, Dynamic,
// or any operation that acquires those locks.
type Limiter interface {
	// Wait blocks until the limiter grants a token or ctx is cancelled.
	Wait(ctx context.Context) error
}

// TokenBucketLimiter wraps [rate.Limiter] and implements [Limiter].
// It optionally emits throttle metrics via [ThrottleObserver].
type TokenBucketLimiter struct {
	rl       *rate.Limiter
	observer ThrottleObserver
}

var _ Limiter = (*TokenBucketLimiter)(nil)

// ThrottleObserver is an optional metrics hook for throttle events.
// It is separate from the sidecar defined in internal/durable so that the
// ratelimit primitive stays independent of the durable package.
type ThrottleObserver interface {
	// IncrementThrottled records one throttled wait event.
	IncrementThrottled()
	// ObserveWait records the actual wait duration in seconds.
	ObserveWait(seconds float64)
}

// New returns a [TokenBucketLimiter] backed by a token bucket with the
// given steady-state rate (events/second) and burst size.
// observer may be nil; when non-nil it is called on every positive-delay wait.
func New(perSec float64, burst int, observer ThrottleObserver) *TokenBucketLimiter {
	return &TokenBucketLimiter{
		rl:       rate.NewLimiter(rate.Limit(perSec), burst),
		observer: observer,
	}
}

// Wait blocks until the limiter grants a token or ctx is cancelled.
// A positive delay triggers an optional metrics callback.
func (l *TokenBucketLimiter) Wait(ctx context.Context) error {
	// Use Reserve to detect whether this attempt will actually wait,
	// so we can emit metrics only on real throttling (not burst-absorbed).
	r := l.rl.Reserve()
	delay := r.Delay()

	if delay <= 0 {
		// Token was available immediately; no wait, no metrics.
		return nil
	}

	// We will actually be delayed. Cancel the reservation if ctx is already done.
	if ctx.Err() != nil {
		r.Cancel()
		return ctx.Err()
	}

	// Emit throttle metrics before sleeping.
	if l.observer != nil {
		l.observer.IncrementThrottled()
		l.observer.ObserveWait(delay.Seconds())
	}

	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		r.Cancel()
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Wait is a nil-safe helper: if l is nil it returns nil immediately without
// blocking. Call it in place of l.Wait(ctx) when l may be nil.
func Wait(ctx context.Context, l Limiter) error {
	if l == nil {
		return nil
	}
	return l.Wait(ctx)
}
