package assignment

import (
	"errors"
	"testing"
	"time"

	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// TestCalculator_CacheFallback_ConnectivityError tests that Calculator falls back to cache
// when connectivity errors occur.
func TestCalculator_CacheFallback_ConnectivityError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx := t.Context()
	_, ncConn := partitest.StartEmbeddedNATS(t)
	heartbeatKV := partitest.CreateJetStreamKV(t, ncConn, "test-cache-fallback-heartbeat")
	assignmentKV := partitest.CreateJetStreamKV(t, ncConn, "test-cache-fallback-assignment")

	// Create initial worker heartbeats
	initialWorkers := []string{"worker-1", "worker-2", "worker-3"}
	for _, workerID := range initialWorkers {
		_, err := heartbeatKV.Put(ctx, "worker-hb."+workerID, []byte("active"))
		require.NoError(t, err)
	}

	// Create calculator
	cfg := &Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}}}},
		Strategy:             &mockStrategy{},
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         6 * time.Second,
		EmergencyGracePeriod: 100 * time.Millisecond,
		Cooldown:             100 * time.Millisecond,
		ColdStartWindow:      200 * time.Millisecond,
		PlannedScaleWindow:   100 * time.Millisecond,
		RestartRatio:         0.5,
	}

	calc, err := NewCalculator(cfg)
	require.NoError(t, err)
	require.NotNil(t, calc)

	// Start calculator
	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() {
		_ = calc.Stop(ctx)
	}()

	// Wait for initial assignment to complete
	time.Sleep(300 * time.Millisecond)

	// Verify cache is populated
	cached, age, ok := calc.getCachedWorkers()
	require.True(t, ok, "cache should be populated after successful fetch")
	require.Len(t, cached, 3)
	require.Less(t, age, 1*time.Second)

	// Close NATS connection to simulate connectivity error
	ncConn.Close()

	// Wait a bit to ensure connection is closed
	time.Sleep(100 * time.Millisecond)

	// Try to fetch workers - should fall back to cache
	workers, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err, "should use cache when connectivity error occurs")
	require.Len(t, workers, 3, "should return cached workers")
	require.ElementsMatch(t, initialWorkers, workers)
}

// TestCalculator_CacheFallback_NoCacheAvailable tests that Calculator returns ErrDegraded
// when no cache is available during connectivity errors.
func TestCalculator_CacheFallback_NoCacheAvailable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx := t.Context()
	_, ncConn := partitest.StartEmbeddedNATS(t)
	heartbeatKV := partitest.CreateJetStreamKV(t, ncConn, "test-no-cache-heartbeat")
	assignmentKV := partitest.CreateJetStreamKV(t, ncConn, "test-no-cache-assignment")

	// Create calculator
	cfg := &Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}},
		Strategy:             &mockStrategy{},
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         6 * time.Second,
		EmergencyGracePeriod: 100 * time.Millisecond,
		Cooldown:             100 * time.Millisecond,
		ColdStartWindow:      200 * time.Millisecond,
		PlannedScaleWindow:   100 * time.Millisecond,
		RestartRatio:         0.5,
	}

	calc, err := NewCalculator(cfg)
	require.NoError(t, err)
	require.NotNil(t, calc)

	// DON'T start calculator - no cache will be populated

	// Close NATS connection to simulate connectivity error
	ncConn.Close()

	// Wait a bit to ensure connection is closed
	time.Sleep(100 * time.Millisecond)

	// Try to fetch workers - should return ErrDegraded
	workers, err := calc.getActiveWorkers(ctx)
	require.Error(t, err, "should return error when no cache available")
	require.True(t, errors.Is(err, types.ErrDegraded), "should return ErrDegraded")
	require.Nil(t, workers)
}

// TestCalculator_CacheUpdate_OnSuccess tests that cache is updated on successful fetches.
func TestCalculator_CacheUpdate_OnSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx := t.Context()
	_, ncConn := partitest.StartEmbeddedNATS(t)
	heartbeatKV := partitest.CreateJetStreamKV(t, ncConn, "test-cache-update-heartbeat")
	assignmentKV := partitest.CreateJetStreamKV(t, ncConn, "test-cache-update-assignment")

	// Create initial worker heartbeats
	initialWorkers := []string{"worker-1", "worker-2"}
	for _, workerID := range initialWorkers {
		_, err := heartbeatKV.Put(ctx, "worker-hb."+workerID, []byte("active"))
		require.NoError(t, err)
	}

	// Create calculator
	cfg := &Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}},
		Strategy:             &mockStrategy{},
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         6 * time.Second,
		EmergencyGracePeriod: 100 * time.Millisecond,
		Cooldown:             100 * time.Millisecond,
		ColdStartWindow:      200 * time.Millisecond,
		PlannedScaleWindow:   100 * time.Millisecond,
		RestartRatio:         0.5,
	}

	calc, err := NewCalculator(cfg)
	require.NoError(t, err)
	require.NotNil(t, calc)

	// Fetch workers - should populate cache
	workers, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.Len(t, workers, 2)

	// Verify initial cache
	cached1, age1, ok1 := calc.getCachedWorkers()
	require.True(t, ok1)
	require.Len(t, cached1, 2)
	require.Less(t, age1, 1*time.Second)

	// Add a new worker
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-3", []byte("active"))
	require.NoError(t, err)

	// Wait a moment
	time.Sleep(50 * time.Millisecond)

	// Fetch workers again - should update cache
	workers, err = calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.Len(t, workers, 3)

	// Verify cache was updated with new data
	cached2, age2, ok2 := calc.getCachedWorkers()
	require.True(t, ok2)
	require.Len(t, cached2, 3, "cache should contain 3 workers after update")
	require.Less(t, age2, 1*time.Second, "cache should be fresh")
	require.ElementsMatch(t, []string{"worker-1", "worker-2", "worker-3"}, cached2, "cache should contain all workers")
}
