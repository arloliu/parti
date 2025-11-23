// Package kvutil provides utilities for working with NATS JetStream KeyValue stores.
package kvutil

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// EnsureKVBucketWithRetry creates or opens a KV bucket with retry logic.
//
// This function handles race conditions when multiple workers try to create
// the same bucket concurrently. It will retry with exponential backoff if
// the creation fails due to transient errors.
//
// Parameters:
//   - ctx: Context for timeout/cancellation
//   - js: JetStream context
//   - config: KV bucket configuration
//   - maxRetries: Maximum number of retry attempts (default: 3)
//
// Returns:
//   - jetstream.KeyValue: The KV bucket instance
//   - error: Any error that occurred after all retries
//
// Example:
//
//	kv, err := kvutil.EnsureKVBucketWithRetry(ctx, js, jetstream.KeyValueConfig{
//	    Bucket: "my-bucket",
//	    TTL:    5 * time.Second,
//	}, 3)
func EnsureKVBucketWithRetry(
	ctx context.Context,
	js jetstream.JetStream,
	config jetstream.KeyValueConfig,
	maxRetries int,
) (jetstream.KeyValue, error) {
	if maxRetries <= 0 {
		maxRetries = 3
	}

	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Try to create the bucket
		kv, err := js.CreateKeyValue(ctx, config)
		if err == nil {
			return kv, nil
		}

		// If bucket already exists, just open it
		if errors.Is(err, jetstream.ErrBucketExists) {
			kv, err := js.KeyValue(ctx, config.Bucket)
			if err == nil {
				return kv, nil
			}
			// Fall through to retry if KeyValue() failed
			lastErr = fmt.Errorf("bucket exists but failed to open: %w", err)
		} else {
			lastErr = err
		}

		// Check if context is done (don't retry if cancelled/timeout)
		if ctx.Err() != nil {
			return nil, fmt.Errorf("context cancelled during KV bucket creation: %w", ctx.Err())
		}

		// Exponential backoff: 10ms, 20ms, 40ms...
		if attempt < maxRetries-1 {
			backoff := time.Duration(1<<uint(attempt)) * 10 * time.Millisecond //nolint:gosec // attempt is bounded by maxRetries (5), no overflow risk
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
				// Continue to next attempt
			}
		}
	}

	return nil, fmt.Errorf("failed to create/open KV bucket %s after %d attempts: %w",
		config.Bucket, maxRetries, lastErr)
}

// EnsureKVBucket creates or opens a JetStream KeyValue bucket with the given TTL.
//
// This is a convenience wrapper around EnsureKVBucketWithRetry that sets the
// bucket name and TTL and uses a small default retry budget suitable for tests
// and lightweight helpers.
//
// Parameters:
//   - ctx: Context for timeout/cancellation
//   - js: JetStream context
//   - bucket: Name of the KV bucket to create or open
//   - ttl: Time-to-live for keys in the bucket
//
// Returns:
//   - jetstream.KeyValue: The opened or newly created KV bucket
//   - error: Any error encountered while creating or opening the bucket
func EnsureKVBucket(ctx context.Context, js jetstream.JetStream, bucket string, ttl time.Duration) (jetstream.KeyValue, error) { //nolint:ireturn,nolintlint // return concrete interface type from JetStream
	cfg := jetstream.KeyValueConfig{Bucket: bucket, TTL: ttl}
	return EnsureKVBucketWithRetry(ctx, js, cfg, 3)
}
