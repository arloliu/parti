package jsutil

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

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
	var lastErr error
	const maxAttempts = 3

	for i := range maxAttempts {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		cons, err := js.CreateOrUpdateConsumer(ctx, streamName, config)
		if err == nil {
			return cons, nil
		}

		lastErr = err
		// If stream not found, it's a configuration error, not transient.
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			return nil, fmt.Errorf("stream %q not found: %w", streamName, err)
		}

		// Retry only for transient errors or if we haven't exhausted attempts
		if i < maxAttempts-1 {
			// Simple jittered backoff: ~50ms, ~100ms
			//nolint:gosec // weak random is sufficient for jitter
			delay := time.Duration(i+1)*50*time.Millisecond + time.Duration(rand.Intn(20))*time.Millisecond
			if delay > 200*time.Millisecond {
				delay = 200 * time.Millisecond
			}

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
