package consumer

import (
	"context"
	"fmt"

	"github.com/arloliu/parti/v2/internal/ratelimit"
)

// ConsumerCreateLimiter gates consumer-create RPC attempts. Its Wait blocks
// until the limiter grants permission or ctx is cancelled (returning ctx.Err()).
//
// Obtain one from [NewConsumerCreateLimiter] (a token-bucket limiter), or supply
// your own implementation, and pass it to [WithConsumerCreateLimiter] to share a
// single rate budget across multiple [Dynamic] consumers in the same process.
//
// # Lock-order contract
//
// Wait is invoked while internal locks (the Dynamic apply/update mutexes) may be
// held. An implementation MUST honour context cancellation and MUST NOT call
// back into a Manager or Dynamic, or any operation that acquires those locks,
// from within Wait.
type ConsumerCreateLimiter interface {
	// Wait blocks until the limiter grants one token or ctx is cancelled.
	Wait(ctx context.Context) error
}

// NewConsumerCreateLimiter returns a token-bucket [ConsumerCreateLimiter] with
// the given steady rate (events/second) and burst size. It is the public
// constructor for a limiter that can be shared across multiple [Dynamic]
// consumers via [WithConsumerCreateLimiter]; for a single consumer prefer the
// simpler [WithConsumerCreateRate].
//
// perSec must be > 0 and burst must be >= 1. A shared limiter built this way
// does not emit the per-consumer throttle metrics that [WithConsumerCreateRate]
// wires up (an injected limiter bypasses the metrics adapter), since a shared
// budget has no single owning consumer to attribute throttle events to.
func NewConsumerCreateLimiter(perSec float64, burst int) (ConsumerCreateLimiter, error) {
	if perSec <= 0 {
		return nil, fmt.Errorf("NewConsumerCreateLimiter: perSec must be > 0, got %v", perSec)
	}
	if burst < 1 {
		return nil, fmt.Errorf("NewConsumerCreateLimiter: burst must be >= 1, got %d", burst)
	}

	return ratelimit.New(perSec, burst, nil), nil
}
