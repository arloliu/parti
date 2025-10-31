package assignment

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/internal/logging"
	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

func TestWorkerMonitor_StartStop(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-monitor-hb")

	callCount := atomic.Int32{}
	onChange := func(ctx context.Context) error {
		callCount.Add(1)
		return nil
	}

	monitor := NewWorkerMonitor(
		hbKV,
		"worker",
		5*time.Second,
		onChange,
		logging.NewNop(),
	)

	ctx := context.Background()
	err := monitor.Start(ctx)
	require.NoError(t, err)

	// Give monitor time to start
	time.Sleep(100 * time.Millisecond)

	// Stop should succeed
	err = monitor.Stop()
	require.NoError(t, err)
}

func TestWorkerMonitor_DetectsWorkerAppearance(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-monitor-appearance")

	callCount := atomic.Int32{}
	ctxReceived := atomic.Bool{}

	onChange := func(ctx context.Context) error {
		callCount.Add(1)
		if ctx != nil {
			ctxReceived.Store(true)
		}

		return nil
	}

	monitor := NewWorkerMonitor(
		hbKV,
		"worker",
		2*time.Second,
		onChange,
		logging.NewNop(),
	)

	ctx := context.Background()
	err := monitor.Start(ctx)
	require.NoError(t, err)
	defer func() {
		_ = monitor.Stop()
	}()

	// Add a worker heartbeat
	_, err = hbKV.Put(ctx, "worker.w1", []byte("alive"))
	require.NoError(t, err)

	// Wait for detection (watcher should trigger fast, or polling after ~1s)
	require.Eventually(t, func() bool {
		return callCount.Load() > 0
	}, 3*time.Second, 100*time.Millisecond, "onChange should be called when worker appears")

	// Verify context was passed
	require.True(t, ctxReceived.Load(), "context should be non-nil")
}

func TestWorkerMonitor_DetectsWorkerDisappearance(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-monitor-disappearance")

	callCount := atomic.Int32{}
	onChange := func(ctx context.Context) error {
		callCount.Add(1)
		return nil
	}

	monitor := NewWorkerMonitor(
		hbKV,
		"worker",
		1*time.Second, // Short TTL for fast test
		onChange,
		logging.NewNop(),
	)

	ctx := context.Background()

	// Add a worker before starting monitor
	_, err := hbKV.Put(ctx, "worker.w1", []byte("alive"))
	require.NoError(t, err)

	err = monitor.Start(ctx)
	require.NoError(t, err)
	defer func() {
		_ = monitor.Stop()
	}()

	// Wait for initial detection
	time.Sleep(200 * time.Millisecond)
	initialCalls := callCount.Load()

	// Let the heartbeat expire (TTL = 1s)
	time.Sleep(1500 * time.Millisecond)

	// Should detect disappearance
	require.Eventually(t, func() bool {
		return callCount.Load() > initialCalls
	}, 2*time.Second, 100*time.Millisecond, "onChange should be called when worker disappears")
}

func TestWorkerMonitor_GetActiveWorkers(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-monitor-active")

	monitor := NewWorkerMonitor(
		hbKV,
		"worker",
		5*time.Second,
		func(ctx context.Context) error { return nil },
		logging.NewNop(),
	)

	ctx := context.Background()

	// Add workers
	_, err := hbKV.Put(ctx, "worker.w1", []byte("alive"))
	require.NoError(t, err)
	_, err = hbKV.Put(ctx, "worker.w2", []byte("alive"))
	require.NoError(t, err)
	_, err = hbKV.Put(ctx, "other.key", []byte("should-ignore"))
	require.NoError(t, err)

	workers, err := monitor.GetActiveWorkers(ctx)
	require.NoError(t, err)
	require.Len(t, workers, 2)
	require.Contains(t, workers, "w1")
	require.Contains(t, workers, "w2")
	require.NotContains(t, workers, "other")
}

func TestWorkerMonitor_GetActiveWorkers_EmptyPrefix(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-monitor-empty")

	monitor := NewWorkerMonitor(
		hbKV,
		"worker",
		5*time.Second,
		func(ctx context.Context) error { return nil },
		logging.NewNop(),
	)

	ctx := context.Background()

	workers, err := monitor.GetActiveWorkers(ctx)
	require.NoError(t, err)
	require.Empty(t, workers)
}

func TestWorkerMonitor_CallbackError(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-monitor-error")

	callCount := atomic.Int32{}
	onChange := func(ctx context.Context) error {
		callCount.Add(1)
		return context.Canceled // Return error to test error handling
	}

	monitor := NewWorkerMonitor(
		hbKV,
		"worker",
		2*time.Second,
		onChange,
		logging.NewNop(),
	)

	ctx := context.Background()
	err := monitor.Start(ctx)
	require.NoError(t, err)
	defer func() {
		_ = monitor.Stop()
	}()

	// Add a worker to trigger callback
	_, err = hbKV.Put(ctx, "worker.w1", []byte("alive"))
	require.NoError(t, err)

	// Should still call callback even if it returns error
	require.Eventually(t, func() bool {
		return callCount.Load() > 0
	}, 3*time.Second, 100*time.Millisecond)
}

func TestWorkerMonitor_StopBeforeStart(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-monitor-stop-first")

	monitor := NewWorkerMonitor(
		hbKV,
		"worker",
		5*time.Second,
		func(ctx context.Context) error { return nil },
		logging.NewNop(),
	)

	// Stop without starting should return error
	err := monitor.Stop()
	require.Error(t, err)
	require.ErrorIs(t, err, types.ErrWorkerMonitorNotStarted)
}

func TestWorkerMonitor_DoubleStart(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-monitor-double-start")

	monitor := NewWorkerMonitor(
		hbKV,
		"worker",
		5*time.Second,
		func(ctx context.Context) error { return nil },
		logging.NewNop(),
	)

	ctx := context.Background()

	// First start should succeed
	err := monitor.Start(ctx)
	require.NoError(t, err)

	// Second start should fail
	err = monitor.Start(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, types.ErrWorkerMonitorAlreadyStarted)

	// Cleanup
	err = monitor.Stop()
	require.NoError(t, err)
}

func TestWorkerMonitor_DoubleStop(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-monitor-double-stop")

	monitor := NewWorkerMonitor(
		hbKV,
		"worker",
		5*time.Second,
		func(ctx context.Context) error { return nil },
		logging.NewNop(),
	)

	ctx := context.Background()
	err := monitor.Start(ctx)
	require.NoError(t, err)

	// Give monitor time to start
	time.Sleep(100 * time.Millisecond)

	// First stop should succeed
	err = monitor.Stop()
	require.NoError(t, err)

	// Second stop should be idempotent (no error)
	err = monitor.Stop()
	require.NoError(t, err)
}

func TestWorkerMonitor_StartAfterStop(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-monitor-restart")

	monitor := NewWorkerMonitor(
		hbKV,
		"worker",
		5*time.Second,
		func(ctx context.Context) error { return nil },
		logging.NewNop(),
	)

	ctx := context.Background()

	// Start and stop
	err := monitor.Start(ctx)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	err = monitor.Stop()
	require.NoError(t, err)

	// Try to start again - should fail (monitor cannot be reused)
	err = monitor.Start(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, types.ErrWorkerMonitorAlreadyStopped)
}

func TestWorkerMonitor_MultipleWorkerChanges(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-monitor-multiple")

	callCount := atomic.Int32{}
	onChange := func(ctx context.Context) error {
		callCount.Add(1)
		return nil
	}

	monitor := NewWorkerMonitor(
		hbKV,
		"worker",
		2*time.Second,
		onChange,
		logging.NewNop(),
	)

	ctx := context.Background()
	err := monitor.Start(ctx)
	require.NoError(t, err)
	defer func() {
		_ = monitor.Stop()
	}()

	// Add multiple workers in sequence
	for i := 1; i <= 3; i++ {
		_, err = hbKV.Put(ctx, fmt.Sprintf("worker.w%d", i), []byte("alive"))
		require.NoError(t, err)
		time.Sleep(200 * time.Millisecond)
	}

	// Should detect multiple changes
	require.Eventually(t, func() bool {
		return callCount.Load() >= 3
	}, 5*time.Second, 100*time.Millisecond)
}
