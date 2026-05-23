// Package retry implements the bounded-retry envelope used to wrap
// the long-lived retry loops in the Parti runtime (source watcher
// restart, handoff watcher restart, assignment watcher, dynamic
// consumer recovery). The envelope replaces forever-retry loops with
// a bounded attempt budget, exponential backoff with a hard ceiling,
// jitter to avoid thundering herds, and a one-shot escalation callback
// on exhaustion so the worker can take itself out of rotation rather
// than generate infinite API load against a vanished resource.
//
// The package is internal — callers wire their site-specific Work,
// Classify, and OnPermanent functions through the Config struct.
package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Class categorises an error returned by the work function.
type Class int

const (
	// Transient: retry after backoff (subject to the attempt budget).
	Transient Class = iota
	// GiveUp: stop immediately; the resource is gone and waiting longer
	// will not help. Fires the permanent-failure callback.
	GiveUp
	// Fatal: bubble the error up to the caller without retry and without
	// firing the permanent-failure callback. Use this when the caller
	// must inspect the error directly (e.g. configuration mistakes).
	Fatal
)

// ErrExhausted is returned by Run when the attempt budget is exhausted
// (after firing OnPermanent) or when Classify returned GiveUp.
var ErrExhausted = errors.New("retry envelope: attempt budget exhausted")

// Config configures a bounded-retry envelope.
type Config struct {
	// Work is invoked on each attempt. nil is not permitted.
	Work func(ctx context.Context) error
	// Classify maps a Work error to a Class. If nil, every error is
	// treated as Transient.
	Classify func(err error) Class
	// OnPermanent is invoked at most once when the envelope gives up
	// (attempt budget exhausted OR Classify returned GiveUp). The
	// caller's last error is passed in. The callback runs synchronously
	// on the envelope's goroutine — it must be non-blocking.
	OnPermanent func(err error)
	// OnProgress is invoked after each failed attempt with the (1-based)
	// attempt number and the error returned. Optional.
	OnProgress func(attempt int, err error)
	// BaseBackoff is the delay before the second attempt; doubles each
	// step up to MaxBackoff. Required.
	BaseBackoff time.Duration
	// MaxBackoff caps the per-attempt delay. Required.
	MaxBackoff time.Duration
	// MaxAttempts is the total attempt budget per Run invocation. After
	// the Nth failure (counting from 1) the envelope fires OnPermanent
	// and returns ErrExhausted. Must be > 0.
	MaxAttempts int
	// Jitter is the ± fraction applied to each backoff delay
	// (0..1 reasonable; 0 disables). Avoids synchronized reconnect
	// thundering herds across worker fleets.
	Jitter float64
	// Clock and Sleep are optional indirections for tests; production
	// uses time.Now and time.After.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// Envelope is the bounded-retry runner.
type Envelope struct {
	cfg Config
}

// New builds an Envelope from the given Config. Panics on invalid
// configuration (Work nil, MaxAttempts <= 0, MaxBackoff <= 0,
// BaseBackoff <= 0). These are programmer errors, not user errors —
// fail loudly.
func New(cfg Config) *Envelope {
	if cfg.Work == nil {
		panic("retry.New: Work is required")
	}
	if cfg.MaxAttempts <= 0 {
		panic(fmt.Sprintf("retry.New: MaxAttempts must be > 0, got %d", cfg.MaxAttempts))
	}
	if cfg.BaseBackoff <= 0 {
		panic("retry.New: BaseBackoff must be > 0")
	}
	if cfg.MaxBackoff < cfg.BaseBackoff {
		panic(fmt.Sprintf("retry.New: MaxBackoff (%v) must be >= BaseBackoff (%v)", cfg.MaxBackoff, cfg.BaseBackoff))
	}
	if cfg.Classify == nil {
		cfg.Classify = func(error) Class { return Transient }
	}
	if cfg.Sleep == nil {
		cfg.Sleep = defaultSleep
	}

	return &Envelope{cfg: cfg}
}

// Run drives the envelope. Returns:
//   - nil on success
//   - ctx.Err() if the context cancels at any point (no OnPermanent fired)
//   - the original Work error on a Fatal classification (no OnPermanent fired)
//   - ErrExhausted on attempt-budget exhaustion or GiveUp (OnPermanent fired
//     exactly once with the most recent Work error)
//
// Concurrency: Run is single-shot; one Envelope per logical retry loop.
// Reusing the same Envelope concurrently is not supported.
func (e *Envelope) Run(ctx context.Context) error {
	var lastErr error
	for attempt := 1; attempt <= e.cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := e.cfg.Work(ctx)
		if err == nil {
			return nil
		}
		lastErr = err

		class := e.cfg.Classify(err)
		switch class {
		case Fatal:
			return err
		case GiveUp:
			e.firePermanent(err)
			return ErrExhausted
		case Transient:
			// fall through to backoff
		}

		if e.cfg.OnProgress != nil {
			e.cfg.OnProgress(attempt, err)
		}

		if attempt == e.cfg.MaxAttempts {
			break
		}

		delay := e.backoffFor(attempt)
		if sleepErr := e.cfg.Sleep(ctx, delay); sleepErr != nil {
			return sleepErr
		}
	}
	e.firePermanent(lastErr)

	return ErrExhausted
}

func (e *Envelope) firePermanent(err error) {
	if e.cfg.OnPermanent != nil {
		e.cfg.OnPermanent(err)
	}
}

// backoffFor returns the (jittered) delay between the Nth and (N+1)th
// attempt. attempt is 1-based.
func (e *Envelope) backoffFor(attempt int) time.Duration {
	// Exponential: base × 2^(attempt-1), capped at MaxBackoff.
	delay := e.cfg.BaseBackoff
	for i := 1; i < attempt && delay < e.cfg.MaxBackoff; i++ {
		delay *= 2
	}
	if delay > e.cfg.MaxBackoff {
		delay = e.cfg.MaxBackoff
	}
	if e.cfg.Jitter <= 0 {
		return delay
	}
	// Apply ± jitter fraction.
	low := 1 - e.cfg.Jitter
	high := 1 + e.cfg.Jitter
	//nolint:gosec // not a security context; rand/v2 is fine for jitter
	f := rand.Float64()
	scale := low + f*(high-low)

	return time.Duration(float64(delay) * scale)
}

// defaultSleep is the production Sleep — returns nil after d elapses or
// ctx.Err() if the context cancels first.
func defaultSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
