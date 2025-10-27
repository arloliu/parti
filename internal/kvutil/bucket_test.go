package kvutil

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	partitest "github.com/arloliu/parti/testing"
)

// TestConcurrentKVBucketCreation verifies that multiple goroutines can safely
// create the same KV bucket concurrently without errors.
func TestConcurrentKVBucketCreation(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	t.Run("5 concurrent creates - same bucket", func(t *testing.T) {
		bucketName := "test-concurrent-bucket"
		numWorkers := 5

		var wg sync.WaitGroup
		errChan := make(chan error, numWorkers)
		kvs := make([]jetstream.KeyValue, numWorkers)

		// Start 5 goroutines all trying to create the same bucket
		for i := 0; i < numWorkers; i++ {
			wg.Add(1) //nolint:revive // Standard pattern for concurrent operations
			go func(idx int) {
				defer wg.Done()

				// Simulate what ensureKVBucket does
				cfg := jetstream.KeyValueConfig{
					Bucket:  bucketName,
					History: 1,
					TTL:     5 * time.Second,
				}

				kv, err := js.CreateKeyValue(ctx, cfg)
				if err != nil {
					// If bucket exists, try to get it
					if errors.Is(err, jetstream.ErrBucketExists) {
						kv, err = js.KeyValue(ctx, bucketName)
					}
				}

				if err != nil {
					errChan <- err
					return
				}

				kvs[idx] = kv
			}(i)
		}

		wg.Wait()
		close(errChan)

		// Check if any errors occurred
		var errList []error
		for err := range errChan {
			errList = append(errList, err)
		}

		require.Empty(t, errList, "All goroutines should successfully get the KV bucket")

		// Verify all got valid KV instances
		for i, kv := range kvs {
			require.NotNil(t, kv, "Worker %d should have valid KV instance", i)
		}

		t.Logf("✅ All %d workers successfully created/opened the same KV bucket", numWorkers)
	})

	t.Run("10 concurrent creates with retry - stress test", func(t *testing.T) {
		bucketName := "test-stress-bucket"
		numWorkers := 10

		var wg sync.WaitGroup
		successCount := make(chan int, numWorkers)

		for i := 0; i < numWorkers; i++ {
			wg.Add(1) //nolint:revive // Standard pattern for concurrent operations
			go func(idx int) {
				defer wg.Done()

				// Retry logic
				var err error
				maxRetries := 5

				for attempt := 0; attempt < maxRetries; attempt++ {
					cfg := jetstream.KeyValueConfig{
						Bucket:  bucketName,
						History: 1,
						TTL:     5 * time.Second,
					}

					kv, err := js.CreateKeyValue(ctx, cfg)
					if err == nil {
						_ = kv // Use the kv to avoid unused warning
						successCount <- idx
						return
					}

					// If bucket exists, get it
					if errors.Is(err, jetstream.ErrBucketExists) {
						kv, err = js.KeyValue(ctx, bucketName)
						if err == nil {
							_ = kv
							successCount <- idx
							return
						}
					}

					// Brief backoff before retry
					if attempt < maxRetries-1 {
						time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
					}
				}

				t.Logf("Worker %d failed after %d retries: %v", idx, maxRetries, err)
			}(i)
		}

		wg.Wait()
		close(successCount)

		successful := 0
		for range successCount {
			successful++
		}

		require.Equal(t, numWorkers, successful, "All workers should succeed with retry logic")
		t.Logf("✅ All %d workers succeeded with retry logic", numWorkers)
	})
}

// TestKVBucketCreationTimeout verifies behavior when context times out during creation.
func TestKVBucketCreationTimeout(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	t.Run("short timeout - should fail gracefully", func(t *testing.T) {
		// Use extremely short timeout
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
		defer cancel()

		// Force timeout by sleeping first
		time.Sleep(1 * time.Millisecond)

		cfg := jetstream.KeyValueConfig{
			Bucket:  "test-timeout-bucket",
			History: 1,
		}

		_, err := js.CreateKeyValue(ctx, cfg)

		// Should fail with context error
		require.Error(t, err)
		t.Logf("Got expected error: %v", err)
	})

	t.Run("sufficient timeout - should succeed", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		cfg := jetstream.KeyValueConfig{
			Bucket:  "test-success-bucket",
			History: 1,
		}

		kv, err := js.CreateKeyValue(ctx, cfg)
		require.NoError(t, err)
		require.NotNil(t, kv)

		t.Log("✅ Successfully created bucket with sufficient timeout")
	})
}

// TestEnsureKVBucketWithRetry tests the retry utility function.
func TestEnsureKVBucketWithRetry(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)

	ctx := context.Background()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	t.Run("successful creation on first try", func(t *testing.T) {
		cfg := jetstream.KeyValueConfig{
			Bucket:  "test-retry-bucket-1",
			History: 1,
			TTL:     5 * time.Second,
		}

		kv, err := EnsureKVBucketWithRetry(ctx, js, cfg, 3)
		require.NoError(t, err)
		require.NotNil(t, kv)

		t.Log("✅ Bucket created successfully on first try")
	})

	t.Run("bucket exists - should open it", func(t *testing.T) {
		bucketName := "test-retry-bucket-2"

		// Create bucket first
		cfg := jetstream.KeyValueConfig{
			Bucket:  bucketName,
			History: 1,
			TTL:     5 * time.Second,
		}

		kv1, err := js.CreateKeyValue(ctx, cfg)
		require.NoError(t, err)
		require.NotNil(t, kv1)

		// Try to create again - should open existing
		kv2, err := EnsureKVBucketWithRetry(ctx, js, cfg, 3)
		require.NoError(t, err)
		require.NotNil(t, kv2)

		t.Log("✅ Successfully opened existing bucket")
	})

	t.Run("concurrent creates with retry - 10 workers", func(t *testing.T) {
		bucketName := "test-retry-bucket-3"
		numWorkers := 10

		var wg sync.WaitGroup
		errors := make(chan error, numWorkers)
		kvs := make([]jetstream.KeyValue, numWorkers)

		cfg := jetstream.KeyValueConfig{
			Bucket:  bucketName,
			History: 1,
			TTL:     5 * time.Second,
		}

		for i := 0; i < numWorkers; i++ {
			wg.Add(1) //nolint:revive // Standard pattern for concurrent operations
			go func(idx int) {
				defer wg.Done()

				kv, err := EnsureKVBucketWithRetry(ctx, js, cfg, 5)
				if err != nil {
					errors <- err
					return
				}

				kvs[idx] = kv
			}(i)
		}

		wg.Wait()
		close(errors)

		// Check results
		var errList []error
		for err := range errors {
			errList = append(errList, err)
		}

		require.Empty(t, errList, "All workers should succeed with retry")

		// Verify all got valid KV instances
		for i, kv := range kvs {
			require.NotNil(t, kv, "Worker %d should have valid KV instance", i)
		}

		t.Logf("✅ All %d workers succeeded concurrently with retry logic", numWorkers)
	})

	t.Run("context timeout - should fail gracefully", func(t *testing.T) {
		shortCtx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
		defer cancel()

		// Force timeout
		time.Sleep(1 * time.Millisecond)

		cfg := jetstream.KeyValueConfig{
			Bucket:  "test-retry-bucket-4",
			History: 1,
		}

		_, err := EnsureKVBucketWithRetry(shortCtx, js, cfg, 3)
		require.Error(t, err)
		require.Contains(t, err.Error(), "context")

		t.Logf("✅ Failed gracefully with context timeout: %v", err)
	})
}
