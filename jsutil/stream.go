package jsutil

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/nats-io/nats.go/jetstream"
)

// EnsureStream creates or opens a JetStream stream with retry logic for transient errors.
//
// This function uses a pessimistic (get-first) approach:
//  1. Try to open the stream with Stream() - works without create permission
//  2. If stream not found, attempt to create it with retry logic
//  3. Handle race conditions when multiple workers try to create concurrently
//
// This design supports least-privilege deployments where NATS users may only
// have read/write access to pre-provisioned streams without create permission.
//
// Parameters:
//   - ctx: Context for timeout/cancellation
//   - js: JetStream context
//   - config: Stream configuration
//
// Returns:
//   - jetstream.Stream: The stream instance
//   - error: Any error that occurred after all retries
//
// Example:
//
//	stream, err := jsutil.EnsureStream(ctx, js, jetstream.StreamConfig{
//	    Name:      "EVENTS",
//	    Subjects:  []string{"events.>"},
//	    Retention: jetstream.LimitsPolicy,
//	    MaxAge:    24 * time.Hour,
//	})
func EnsureStream(ctx context.Context, js jetstream.JetStream, config jetstream.StreamConfig) (jetstream.Stream, error) {
	return EnsureStreamWithRetry(ctx, js, config, 3)
}

// EnsureStreamWithRetry creates or opens a JetStream stream with configurable retry attempts.
//
// See EnsureStream for behavior details. This variant allows customizing the retry count.
//
// Parameters:
//   - ctx: Context for timeout/cancellation
//   - js: JetStream context
//   - config: Stream configuration
//   - maxRetries: Maximum number of retry attempts (minimum 1)
//
// Returns:
//   - jetstream.Stream: The stream instance
//   - error: Any error that occurred after all retries
func EnsureStreamWithRetry(
	ctx context.Context,
	js jetstream.JetStream,
	config jetstream.StreamConfig,
	maxRetries int,
) (jetstream.Stream, error) {
	if maxRetries < 1 {
		maxRetries = 1
	}

	if config.Name == "" {
		return nil, errors.New("stream name is required")
	}

	// Pessimistic approach: try to open existing stream first.
	// This works without create permission for pre-provisioned streams.
	stream, err := js.Stream(ctx, config.Name)
	if err == nil {
		return stream, nil
	}

	// If stream doesn't exist, try to create it
	if !natsutil.IsStreamNotFound(err) {
		return nil, fmt.Errorf("failed to open stream %q: %w", config.Name, err)
	}

	// Stream not found - attempt creation with retry logic
	var lastErr error

	for attempt := range maxRetries {
		stream, err = js.CreateStream(ctx, config)
		if err == nil {
			return stream, nil
		}

		// Race condition: another worker created it between our get and create
		if errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
			stream, err = js.Stream(ctx, config.Name)
			if err == nil {
				return stream, nil
			}
			// Fall through to retry if Stream() failed
			lastErr = fmt.Errorf("stream exists but failed to open: %w", err)
		} else {
			lastErr = err
		}

		// Check if context is done (don't retry if cancelled/timeout)
		if ctx.Err() != nil {
			return nil, fmt.Errorf("context cancelled during stream creation: %w", ctx.Err())
		}

		// Jittered backoff for remaining attempts
		if attempt < maxRetries-1 {
			delay := calculateBackoff(attempt)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
				// Continue to next attempt
			}
		}
	}

	return nil, fmt.Errorf("failed to create/open stream %q after %d attempts: %w",
		config.Name, maxRetries, lastErr)
}

// calculateBackoff returns a jittered exponential backoff duration.
// Base: 20ms, 40ms, 80ms... with ±10ms jitter, capped at 500ms.
func calculateBackoff(attempt int) time.Duration {
	base := time.Duration(1<<uint(attempt)) * 20 * time.Millisecond //nolint:gosec // attempt is bounded, no overflow risk
	//nolint:gosec // weak random is sufficient for jitter
	jitter := time.Duration(rand.Intn(20)-10) * time.Millisecond
	delay := max(min(base+jitter, 500*time.Millisecond), 10*time.Millisecond)

	return delay
}
