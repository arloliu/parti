package jsutil

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/nats-io/nats.go/jetstream"
)

// EnsureConsumer creates or updates a JetStream consumer with retry logic for transient errors.
//
// It uses a fixed short retry policy (3 attempts, max 200ms delay) suitable for initialization.
// For configuration errors like missing streams, it fails immediately without retrying.
//
// Parameters:
//   - ctx: Context for timeout/cancellation
//   - js: JetStream context
//   - streamName: Name of the stream to create the consumer on
//   - config: Consumer configuration
//
// Returns:
//   - jetstream.Consumer: The consumer instance
//   - error: Any error that occurred after all retries
//
// Example:
//
//	consumer, err := jsutil.EnsureConsumer(ctx, js, "EVENTS", jetstream.ConsumerConfig{
//	    Durable:       "my-consumer",
//	    AckPolicy:     jetstream.AckExplicitPolicy,
//	    FilterSubject: "events.processed",
//	})
func EnsureConsumer(ctx context.Context, js jetstream.JetStream, streamName string, config jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	return EnsureConsumerWithOptions(ctx, js, streamName, config)
}

// EnsureConsumerOption is a functional option for [EnsureConsumerWithOptions].
type EnsureConsumerOption func(*ensureConsumerOptions)

type ensureConsumerOptions struct {
	beforeAttempt func(ctx context.Context) error
}

// WithBeforeAttempt installs a hook invoked before every physical
// CreateOrUpdateConsumer attempt, including retries. A non-nil error from the
// hook aborts the retry loop and is returned to the caller.
//
// This is the per-attempt gating seam used to enforce a rate limit on consumer
// creates. The hook must honour context cancellation; it must not call back
// into any component that holds applyStoreMu or updateMu.
func WithBeforeAttempt(fn func(ctx context.Context) error) EnsureConsumerOption {
	return func(o *ensureConsumerOptions) {
		o.beforeAttempt = fn
	}
}

// EnsureConsumerWithOptions creates or updates a JetStream consumer with retry
// logic for transient errors, with optional per-attempt hooks.
//
// It uses a fixed short retry policy (3 attempts, max 200ms delay) suitable for
// initialization. For configuration errors like missing streams, it fails
// immediately without retrying.
//
// The [WithBeforeAttempt] option installs a hook called before every physical
// CreateOrUpdateConsumer attempt, including retries. A non-nil hook error aborts
// the loop and is returned. This is the seam for rate-limiting consumer creates.
//
// Parameters:
//   - ctx: Context for timeout/cancellation
//   - js: JetStream context
//   - streamName: Name of the stream to create the consumer on
//   - config: Consumer configuration
//   - opts: Functional options (e.g. [WithBeforeAttempt])
//
// Returns:
//   - jetstream.Consumer: The consumer instance
//   - error: Any error that occurred after all retries
func EnsureConsumerWithOptions(ctx context.Context, js jetstream.JetStream, streamName string, config jetstream.ConsumerConfig, opts ...EnsureConsumerOption) (jetstream.Consumer, error) {
	var o ensureConsumerOptions
	for _, opt := range opts {
		opt(&o)
	}

	var lastErr error
	const maxAttempts = 3

	for i := range maxAttempts {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Gate: invoke the before-attempt hook (e.g. a rate limiter) before
		// every physical RPC attempt, including retries. A non-nil error
		// (e.g. context cancellation during a paced wait) aborts here so
		// the apply fails pre-commit and is retried by the manager.
		if o.beforeAttempt != nil {
			if err := o.beforeAttempt(ctx); err != nil {
				return nil, err
			}
		}

		cons, err := js.CreateOrUpdateConsumer(ctx, streamName, config)
		if err == nil {
			return cons, nil
		}

		lastErr = err
		// If stream not found, it's a configuration error, not transient.
		if natsutil.IsStreamNotFound(err) {
			return nil, fmt.Errorf("stream %q not found: %w", streamName, err)
		}

		// Retry only for transient errors or if we haven't exhausted attempts
		if i < maxAttempts-1 {
			// Simple jittered backoff: ~50ms, ~100ms
			//nolint:gosec // weak random is sufficient for jitter
			delay := min(time.Duration(i+1)*50*time.Millisecond+time.Duration(rand.Intn(20))*time.Millisecond, 200*time.Millisecond)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
				continue
			}
		}
	}

	return nil, fmt.Errorf("failed to ensure consumer %q on stream %q after %d attempts: %w", config.Durable, streamName, maxAttempts, lastErr)
}

// IsValidConsumerName checks if a string contains only allowed characters for a NATS consumer name.
//
// Allowed characters: a-z, A-Z, 0-9, -, _
//
// Parameters:
//   - name: The consumer name to validate
//
// Returns:
//   - bool: true if the name contains only valid characters, false otherwise
//
// Example:
//
//	if !jsutil.IsValidConsumerName(consumerName) {
//	    return fmt.Errorf("invalid consumer name: %s", consumerName)
//	}
func IsValidConsumerName(name string) bool {
	for _, r := range name {
		isAlpha := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		isDigit := (r >= '0' && r <= '9')
		isSpecial := r == '-' || r == '_'

		if !isAlpha && !isDigit && !isSpecial {
			return false
		}
	}

	return true
}
