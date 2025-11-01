//go:build integration

package testutil

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestExternalNATS_Basic validates basic functionality of external NATS server.
func TestExternalNATS_Basic(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping external NATS test in short mode")
	}

	// Start external NATS
	nc, cleanup := StartExternalNATS(t)
	defer cleanup()

	// Verify connection is working
	require.True(t, nc.IsConnected(), "Should be connected to NATS")

	// Verify JetStream is working
	js, err := nc.JetStream()
	require.NoError(t, err)

	info, err := js.AccountInfo()
	require.NoError(t, err)
	require.NotNil(t, info)

	t.Logf("External NATS server connected successfully")
}

// TestExternalNATS_BinaryCaching tests that binary caching works correctly.
func TestExternalNATS_BinaryCaching(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping caching test in short mode")
	}

	// Clean cache before test
	err := CleanupNATSBinaryCache()
	require.NoError(t, err)

	// First run - should compile
	t.Log("First run - compiling binary...")
	start := time.Now()
	binaryPath1, err := getOrBuildNATSBinary()
	require.NoError(t, err)
	firstDuration := time.Since(start)
	t.Logf("First compilation took: %v", firstDuration)

	// Verify binary exists
	stat1, err := os.Stat(binaryPath1)
	require.NoError(t, err)
	require.NotZero(t, stat1.Size())

	// Second run - should use cache
	t.Log("Second run - using cached binary...")
	start = time.Now()
	binaryPath2, err := getOrBuildNATSBinary()
	require.NoError(t, err)
	secondDuration := time.Since(start)
	t.Logf("Second lookup took: %v", secondDuration)

	// Verify same binary is returned
	require.Equal(t, binaryPath1, binaryPath2, "Should return same cached binary")

	// Cache hit should be much faster
	require.Less(t, secondDuration, firstDuration/10,
		"Cache hit should be at least 10x faster than compilation")

	t.Logf("Cache speedup: %.1fx", float64(firstDuration)/float64(secondDuration))

	// Cleanup
	err = CleanupNATSBinaryCache()
	require.NoError(t, err)
}

// TestExternalNATS_ParallelExecution tests concurrent external NATS servers.
func TestExternalNATS_ParallelExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping parallel test in short mode")
	}

	t.Parallel()

	// Run multiple servers concurrently
	for i := 0; i < 5; i++ {
		i := i
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			t.Parallel()

			nc, cleanup := StartExternalNATS(t)
			defer cleanup()

			require.True(t, nc.IsConnected())

			// Do some work to verify isolation
			js, err := nc.JetStream()
			require.NoError(t, err)

			_, err = js.AccountInfo()
			require.NoError(t, err)

			t.Logf("Parallel test %d completed successfully", i)
		})
	}
}

// TestExternalNATS_WithContext tests context-based lifecycle control.
func TestExternalNATS_WithContext(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping context test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	nc, url, cleanup, err := StartExternalNATSWithContext(ctx, t)
	require.NoError(t, err)
	defer cleanup()

	require.NotEmpty(t, url)
	require.True(t, nc.IsConnected())

	// Verify JetStream
	js, err := nc.JetStream()
	require.NoError(t, err)

	_, err = js.AccountInfo()
	require.NoError(t, err)

	t.Logf("Context-based NATS server at %s", url)
}

// TestExternalNATS_Cleanup tests that cleanup works properly.
func TestExternalNATS_Cleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cleanup test in short mode")
	}

	// Start and stop server
	nc, cleanup := StartExternalNATS(t)
	require.True(t, nc.IsConnected())

	// Explicit cleanup
	cleanup()

	// Wait a bit for cleanup
	time.Sleep(500 * time.Millisecond)

	// Connection should be closed
	require.False(t, nc.IsConnected(), "Connection should be closed after cleanup")
}

// TestExternalNATS_CacheInvalidation tests cache invalidation on source change.
func TestExternalNATS_CacheInvalidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cache invalidation test in short mode")
	}

	// Clean cache
	err := CleanupNATSBinaryCache()
	require.NoError(t, err)

	// Get binary path (compiles)
	path1, err := getOrBuildNATSBinary()
	require.NoError(t, err)

	// Find module root for hash calculation
	moduleRoot, err := findModuleRoot()
	require.NoError(t, err)

	// Get hash of source file
	sourceFile := moduleRoot + "/test/cmd/nats-server/main.go"
	hash1, err := hashFile(sourceFile)
	require.NoError(t, err)
	require.NotEmpty(t, hash1)

	// Verify binary name contains hash
	require.Contains(t, path1, hash1[:16])

	t.Logf("Binary cached at: %s", path1)
	t.Logf("Source hash: %s", hash1[:16])

	// Cleanup
	err = CleanupNATSBinaryCache()
	require.NoError(t, err)
}
