package assignment

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestWorkerMonitor_StartStop(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		200*time.Millisecond, // Short TTL for fast test
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
	require.Eventually(t, func() bool {
		return callCount.Load() > 0
	}, 1*time.Second, 25*time.Millisecond, "onChange should be called for the initial worker")
	initialCalls := callCount.Load()

	// Should detect disappearance
	require.Eventually(t, func() bool {
		return callCount.Load() > initialCalls
	}, 1*time.Second, 50*time.Millisecond, "onChange should be called when worker disappears")
}

func TestWorkerMonitor_GetActiveWorkers(t *testing.T) {
	t.Parallel()
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

func TestWorkerMonitor_GetHeartbeats_DualDecode(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-monitor-hbs")

	monitor := NewWorkerMonitor(
		hbKV,
		"worker",
		5*time.Second,
		func(ctx context.Context) error { return nil },
		logging.NewNop(),
	)

	ctx := context.Background()

	// Legacy worker: RFC3339Nano timestamp payload (pre-v1 wire format).
	legacy := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := hbKV.Put(ctx, "worker.legacy", []byte(legacy))
	require.NoError(t, err)

	// v1 worker: full JSON heartbeat.
	v1 := types.Heartbeat{
		WorkerID:       "v1",
		SchemaVersion:  1,
		Capabilities:   types.CapAckV1 | types.CapTwoPhaseHandoff,
		LeaderRevision: 7,
		AppliedVersion: 42,
		AppliedDigest:  0xdead,
		AppliedAt:      time.Now().UTC(),
		Timestamp:      time.Now().UTC(),
	}
	v1Bytes, err := jsonMarshal(v1)
	require.NoError(t, err)
	_, err = hbKV.Put(ctx, "worker.v1", v1Bytes)
	require.NoError(t, err)

	// Bogus payload: malformed bytes should be omitted, not raise an error.
	_, err = hbKV.Put(ctx, "worker.bogus", []byte("not-a-timestamp-nor-json"))
	require.NoError(t, err)

	// Non-heartbeat key: ignored entirely.
	_, err = hbKV.Put(ctx, "other.key", []byte("ignored"))
	require.NoError(t, err)

	hbs, err := monitor.GetHeartbeats(ctx)
	require.NoError(t, err)
	require.Len(t, hbs, 2, "legacy + v1 should be decoded; bogus omitted")

	legacyHB, ok := hbs["legacy"]
	require.True(t, ok)
	require.Equal(t, uint8(0), legacyHB.SchemaVersion, "legacy timestamp decodes to SchemaVersion=0")
	require.Equal(t, uint32(0), legacyHB.Capabilities)

	v1HB, ok := hbs["v1"]
	require.True(t, ok)
	require.Equal(t, uint8(1), v1HB.SchemaVersion)
	require.Equal(t, types.CapAckV1|types.CapTwoPhaseHandoff, v1HB.Capabilities)
	require.Equal(t, int64(42), v1HB.AppliedVersion)
	require.Equal(t, uint64(0xdead), v1HB.AppliedDigest)
}

// jsonMarshal is a tiny local helper to avoid pulling encoding/json into the
// import list at the top of this file solely for one test.
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func TestWorkerMonitor_GetActiveWorkers_EmptyPrefix(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

type fakeKeyWatcher struct {
	ctx     context.Context
	updates chan jetstream.KeyValueEntry
	errCh   chan error
}

func (f *fakeKeyWatcher) Context() context.Context {
	return f.ctx
}

func (f *fakeKeyWatcher) Updates() <-chan jetstream.KeyValueEntry {
	return f.updates
}

func (f *fakeKeyWatcher) Stop() error {
	return nil
}

func (f *fakeKeyWatcher) Error() <-chan error {
	return f.errCh
}

func TestWorkerMonitor_ProcessWatcherEvents_ClosedChannel(t *testing.T) {
	t.Parallel()

	updates := make(chan jetstream.KeyValueEntry)
	close(updates)
	errCh := make(chan error)
	close(errCh)

	monitor := &WorkerMonitor{
		logger: logging.NewNop(),
		stopCh: make(chan struct{}),
	}
	monitor.watcher = &fakeKeyWatcher{
		ctx:     context.Background(),
		updates: updates,
		errCh:   errCh,
	}

	ctx := t.Context()

	done := make(chan struct{})
	go func() {
		monitor.processWatcherEvents(ctx)
		close(done)
	}()

	select {
	case <-done:
		return
	case <-time.After(200 * time.Millisecond):
		t.Fatal("processWatcherEvents did not exit after channel close")
	}
}
