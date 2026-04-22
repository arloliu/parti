package kvutil

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// EnsureKVBucketWithRetry opens an existing KV bucket or creates it if not found.
//
// This function uses a pessimistic (get-first) approach:
//  1. Try to open the bucket with KeyValue() - works without create permission
//  2. If bucket not found, attempt to create it with retry logic
//  3. Handle race conditions when multiple workers try to create concurrently
//
// This design supports least-privilege deployments where NATS users may only
// have read/write access to pre-provisioned buckets without create permission.
//
// Parameters:
//   - ctx: Context for timeout/cancellation
//   - js: JetStream context
//   - config: KV bucket configuration
//   - maxRetries: Maximum number of retry attempts for creation (default: 3)
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

	// Pessimistic approach: try to open existing bucket first.
	// This works without create permission for pre-provisioned buckets.
	kv, err := js.KeyValue(ctx, config.Bucket)
	if err == nil {
		return kv, nil
	}

	// If bucket doesn't exist, try to create it
	if !errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, fmt.Errorf("failed to open KV bucket %s: %w", config.Bucket, err)
	}

	// Bucket not found - attempt creation with retry logic
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		kv, err := js.CreateKeyValue(ctx, config)
		if err == nil {
			return kv, nil
		}

		// Race condition: another worker created it between our get and create
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
			backoff := time.Duration(1<<uint(attempt)) * 10 * time.Millisecond
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

// OpenKVBucket opens an existing KV bucket without attempting to create it.
//
// Use this when the bucket is expected to be pre-provisioned and the NATS user
// may not have create permission. Returns ErrBucketNotFound if bucket doesn't exist.
//
// Parameters:
//   - ctx: Context for timeout/cancellation
//   - js: JetStream context
//   - bucket: Name of the KV bucket to open
//
// Returns:
//   - jetstream.KeyValue: The opened KV bucket
//   - error: ErrBucketNotFound if bucket doesn't exist, or other JetStream errors
func OpenKVBucket(ctx context.Context, js jetstream.JetStream, bucket string) (jetstream.KeyValue, error) { //nolint:ireturn,nolintlint // return concrete interface type from JetStream
	return js.KeyValue(ctx, bucket)
}

// CreateKVBucket creates a new KV bucket with retry logic.
//
// Use this when you explicitly want to create a bucket and fail if it already exists.
// For create-or-open semantics, use EnsureKVBucket or EnsureKVBucketWithRetry instead.
//
// Parameters:
//   - ctx: Context for timeout/cancellation
//   - js: JetStream context
//   - config: KV bucket configuration
//   - maxRetries: Maximum retry attempts for transient errors (default: 3)
//
// Returns:
//   - jetstream.KeyValue: The newly created KV bucket
//   - error: ErrBucketExists if bucket already exists, or other errors
func CreateKVBucket(
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
		kv, err := js.CreateKeyValue(ctx, config)
		if err == nil {
			return kv, nil
		}

		// Bucket already exists - don't retry, just return error
		if errors.Is(err, jetstream.ErrBucketExists) {
			return nil, err
		}

		lastErr = err

		// Check if context is done
		if ctx.Err() != nil {
			return nil, fmt.Errorf("context cancelled during KV bucket creation: %w", ctx.Err())
		}

		// Exponential backoff for transient errors
		if attempt < maxRetries-1 {
			backoff := time.Duration(1<<uint(attempt)) * 10 * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
				// Continue to next attempt
			}
		}
	}

	return nil, fmt.Errorf("failed to create KV bucket %s after %d attempts: %w",
		config.Bucket, maxRetries, lastErr)
}
