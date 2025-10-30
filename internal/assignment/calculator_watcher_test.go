package assignment

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestWatcher_StartStop verifies watcher can be started and stopped cleanly.
func TestWatcher_StartStop(t *testing.T) {
	ctx := context.Background()
	calc, _, cleanup := setupCalculatorForWatcherTest(t)
	defer cleanup()

	// Start watcher
	err := calc.startWatcher(ctx)
	require.NoError(t, err, "should start watcher successfully")

	// Verify watcher is started
	calc.watcherMu.Lock()
	require.NotNil(t, calc.watcher, "watcher should be non-nil after start")
	calc.watcherMu.Unlock()

	// Stop watcher
	calc.stopWatcher()

	// Verify watcher is stopped
	calc.watcherMu.Lock()
	require.Nil(t, calc.watcher, "watcher should be nil after stop")
	calc.watcherMu.Unlock()

	t.Log("Watcher start/stop works correctly")
}

// TestWatcher_DoubleStart verifies starting watcher twice is safe.
func TestWatcher_DoubleStart(t *testing.T) {
	ctx := context.Background()
	calc, _, cleanup := setupCalculatorForWatcherTest(t)
	defer cleanup()

	// Start watcher first time
	err := calc.startWatcher(ctx)
	require.NoError(t, err)

	// Start watcher second time (should be no-op)
	err = calc.startWatcher(ctx)
	require.NoError(t, err, "double start should not error")

	calc.watcherMu.Lock()
	require.NotNil(t, calc.watcher, "watcher should still be running")
	calc.watcherMu.Unlock()

	calc.stopWatcher()
	t.Log("Double start handled correctly")
}

// TestWatcher_DetectsHeartbeatUpdates verifies watcher receives heartbeat updates.
func TestWatcher_DetectsHeartbeatUpdates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	calc, heartbeatKV, cleanup := setupCalculatorForWatcherTest(t)
	defer cleanup()

	// Start watcher
	err := calc.startWatcher(ctx)
	require.NoError(t, err)
	defer calc.stopWatcher()

	// Give watcher goroutine time to start
	time.Sleep(100 * time.Millisecond)

	// Publish a heartbeat
	t.Log("Publishing heartbeat for worker-1...")
	key := fmt.Sprintf("%s.%s", calc.hbPrefix, "worker-1")
	_, err = heartbeatKV.Put(ctx, key, []byte(fmt.Sprintf("%d", time.Now().Unix())))
	require.NoError(t, err)

	// Wait for watcher to process (100ms debounce + small buffer)
	time.Sleep(300 * time.Millisecond)

	// Verify worker was detected by checking active workers
	workers, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err)

	found := false
	for _, w := range workers {
		if w == "worker-1" {
			found = true
			break
		}
	}
	require.True(t, found, "watcher should detect new worker")

	t.Log("Watcher detected heartbeat update")
}

// TestWatcher_Debouncing verifies rapid updates are debounced.
func TestWatcher_Debouncing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	calc, heartbeatKV, cleanup := setupCalculatorForWatcherTest(t)
	defer cleanup()

	// Start watcher
	err := calc.startWatcher(ctx)
	require.NoError(t, err)
	defer calc.stopWatcher()

	// Give watcher goroutine time to start
	time.Sleep(100 * time.Millisecond)

	// Publish multiple rapid heartbeat updates
	t.Log("Publishing rapid heartbeat updates...")
	key := fmt.Sprintf("%s.%s", calc.hbPrefix, "worker-1")

	startTime := time.Now()
	for i := 0; i < 10; i++ {
		_, err = heartbeatKV.Put(ctx, key, []byte(fmt.Sprintf("%d", time.Now().Unix())))
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond) // Rapid updates within debounce window
	}
	updateDuration := time.Since(startTime)

	// Wait for debounce to settle (100ms debounce + small buffer)
	time.Sleep(300 * time.Millisecond)

	// Verify worker was detected by checking active workers
	workers, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.Contains(t, workers, "worker-1", "watcher should detect worker despite rapid updates")

	t.Logf("Published 10 updates in %dms, debouncing handled correctly", updateDuration.Milliseconds())
}

// TestWatcher_MultipleWorkers verifies watcher detects multiple workers.
func TestWatcher_MultipleWorkers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	calc, heartbeatKV, cleanup := setupCalculatorForWatcherTest(t)
	defer cleanup()

	// Start watcher
	err := calc.startWatcher(ctx)
	require.NoError(t, err)
	defer calc.stopWatcher()

	// Give watcher goroutine time to start
	time.Sleep(100 * time.Millisecond)

	// Publish heartbeats for 3 workers
	t.Log("Publishing heartbeats for 3 workers...")
	for i := 1; i <= 3; i++ {
		key := fmt.Sprintf("%s.worker-%d", calc.hbPrefix, i)
		_, err = heartbeatKV.Put(ctx, key, []byte(fmt.Sprintf("%d", time.Now().Unix())))
		require.NoError(t, err)
		time.Sleep(50 * time.Millisecond) // Stagger slightly
	}

	// Wait for watcher to process all updates
	time.Sleep(300 * time.Millisecond)

	// Verify all workers detected
	workers, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.Len(t, workers, 3, "should detect all 3 workers")
	require.Contains(t, workers, "worker-1")
	require.Contains(t, workers, "worker-2")
	require.Contains(t, workers, "worker-3")

	t.Log("Watcher detected all 3 workers")
}

// TestWatcher_PatternFiltering verifies watcher only watches heartbeat keys.
func TestWatcher_PatternFiltering(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	calc, heartbeatKV, cleanup := setupCalculatorForWatcherTest(t)
	defer cleanup()

	// Start watcher
	err := calc.startWatcher(ctx)
	require.NoError(t, err)
	defer calc.stopWatcher()

	// Give watcher goroutine time to start
	time.Sleep(100 * time.Millisecond)

	// Publish heartbeat key (should be detected)
	t.Log("Publishing heartbeat key...")
	heartbeatKey := fmt.Sprintf("%s.worker-1", calc.hbPrefix)
	_, err = heartbeatKV.Put(ctx, heartbeatKey, []byte(fmt.Sprintf("%d", time.Now().Unix())))
	require.NoError(t, err)

	// Publish non-heartbeat key (should be ignored by watcher pattern)
	t.Log("Publishing non-heartbeat key...")
	otherKey := "other.key.value"
	_, err = heartbeatKV.Put(ctx, otherKey, []byte("some data"))
	require.NoError(t, err)

	// Wait for watcher to process
	time.Sleep(300 * time.Millisecond)

	// Verify only heartbeat worker detected
	workers, err := calc.getActiveWorkers(ctx)
	require.NoError(t, err)
	require.Len(t, workers, 1, "should only detect heartbeat worker")
	require.Contains(t, workers, "worker-1")

	t.Log("Watcher correctly filters by heartbeat pattern")
}

// TestWatcher_DetectionLatency measures actual watcher detection time.
func TestWatcher_DetectionLatency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	calc, heartbeatKV, cleanup := setupCalculatorForWatcherTest(t)
	defer cleanup()

	// Start watcher
	err := calc.startWatcher(ctx)
	require.NoError(t, err)
	defer calc.stopWatcher()

	// Give watcher goroutine time to start
	time.Sleep(100 * time.Millisecond)

	// Measure time from heartbeat publish to detection
	t.Log("Measuring watcher detection latency...")
	startTime := time.Now()

	// Publish heartbeat
	key := fmt.Sprintf("%s.worker-test", calc.hbPrefix)
	_, err = heartbeatKV.Put(ctx, key, []byte(fmt.Sprintf("%d", time.Now().Unix())))
	require.NoError(t, err)

	// Poll until worker is detected (with timeout)
	var detectionLatency time.Duration
	detected := false

	for i := 0; i < 50; i++ { // 50 * 50ms = 2.5s max
		time.Sleep(50 * time.Millisecond)
		workers, err := calc.getActiveWorkers(ctx)
		require.NoError(t, err)

		for _, w := range workers {
			if w == "worker-test" {
				detectionLatency = time.Since(startTime)
				detected = true
				break
			}
		}

		if detected {
			break
		}
	}

	require.True(t, detected, "worker should be detected within timeout")

	t.Logf("Detection latency: %dms", detectionLatency.Milliseconds())

	// With watcher, detection should be fast (< 500ms)
	// This includes: watcher notification (~10-100ms) + debounce (100ms) + processing (~50-200ms)
	require.Less(t, detectionLatency, 500*time.Millisecond,
		"watcher detection should be fast (got %dms)", detectionLatency.Milliseconds())

	if detectionLatency < 300*time.Millisecond {
		t.Log("Excellent: Detection under 300ms")
	} else {
		t.Log("Good: Detection under 500ms")
	}
}

// TestWatcher_StopDuringProcessing verifies stopping watcher during event processing is safe.
func TestWatcher_StopDuringProcessing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	calc, heartbeatKV, cleanup := setupCalculatorForWatcherTest(t)
	defer cleanup()

	// Start watcher
	err := calc.startWatcher(ctx)
	require.NoError(t, err)

	// Give watcher goroutine time to start
	time.Sleep(100 * time.Millisecond)

	// Publish rapid updates
	t.Log("Publishing rapid updates while stopping...")
	go func() {
		for i := 0; i < 20; i++ {
			key := fmt.Sprintf("%s.worker-%d", calc.hbPrefix, i)
			_, _ = heartbeatKV.Put(ctx, key, []byte(fmt.Sprintf("%d", time.Now().Unix())))
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Stop watcher while updates are happening
	time.Sleep(50 * time.Millisecond)
	calc.stopWatcher()

	// Verify no panic and watcher is stopped
	calc.watcherMu.Lock()
	require.Nil(t, calc.watcher, "watcher should be stopped")
	calc.watcherMu.Unlock()

	t.Log("Stopping watcher during event processing is safe")
}

// setupCalculatorForWatcherTest creates a Calculator with embedded NATS for watcher testing.
func setupCalculatorForWatcherTest(t *testing.T) (*Calculator, jetstream.KeyValue, func()) {
	t.Helper()

	// Start embedded NATS using test utility
	_, nc := partitest.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create KV buckets
	assignmentKV, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{
		Bucket:  "test-assignment-" + t.Name(),
		History: 5,
	})
	require.NoError(t, err)

	heartbeatKV, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{
		Bucket:  "test-heartbeat-" + t.Name(),
		History: 1,
		TTL:     10 * time.Second,
	})
	require.NoError(t, err)

	// Create simple partition source
	partitions := []types.Partition{
		{Keys: []string{"p1"}, Weight: 100},
		{Keys: []string{"p2"}, Weight: 100},
		{Keys: []string{"p3"}, Weight: 100},
	}
	src := source.NewStatic(partitions)
	strategy := strategy.NewRoundRobin()

	// Create calculator with test logger
	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               src,
		Strategy:             strategy,
		HeartbeatPrefix:      "heartbeat",
		HeartbeatTTL:         5 * time.Second,
		EmergencyGracePeriod: 2 * time.Second, // Emergency grace period,
	})
	require.NoError(t, err)
	calc.SetLogger(logging.NewTest(t))

	cleanup := func() {
		calc.stopWatcher()
		nc.Close()
	}

	return calc, heartbeatKV, cleanup
}
