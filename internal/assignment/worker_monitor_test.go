package assignment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

var errFakeKVNotImplemented = errors.New("not implemented by fakeKV stub")

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

// putHeartbeatJSON marshals hb to v1 JSON and Puts it under key. Named
// distinctly from the package's existing putHeartbeat helper (in
// calculator_state_test.go, which Puts a bare "alive" string keyed by
// worker ID) to avoid a redeclaration in this package.
func putHeartbeatJSON(t *testing.T, kv jetstream.KeyValue, key string, hb types.Heartbeat) {
	t.Helper()
	b, err := jsonMarshal(hb)
	require.NoError(t, err)
	_, err = kv.Put(context.Background(), key, b)
	require.NoError(t, err)
}

// TestWorkerMonitor_GetHeartbeatsFor verifies the targeted per-worker fetch
// used by the rebalance path: exactly one bounded Get per listed worker ID,
// no Keys() scan. Per-worker Get/decode failures must be absent from the
// heartbeat map but present in the error map with the original error, so
// the caller can classify the failure (connectivity vs not) rather than
// having it masquerade as a bare "unknown labels" omission.
func TestWorkerMonitor_GetHeartbeatsFor(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	hbKV := partitest.CreateJetStreamKV(t, nc, "test-monitor-hbfor")

	ctx := context.Background()

	// Seed: worker-0 v1 JSON with labels, worker-1 v1 JSON without,
	// worker-2 malformed payload. worker-9 is never written (missing key).
	putHeartbeatJSON(t, hbKV, "heartbeat.worker-0", types.Heartbeat{
		WorkerID: "worker-0", SchemaVersion: 1, Labels: []string{"vip"},
		Timestamp: time.Now(),
	})
	putHeartbeatJSON(t, hbKV, "heartbeat.worker-1", types.Heartbeat{
		WorkerID: "worker-1", SchemaVersion: 1, Timestamp: time.Now(),
	})
	_, err := hbKV.Put(ctx, "heartbeat.worker-2", []byte("not-json-not-time"))
	require.NoError(t, err)

	m := NewWorkerMonitor(hbKV, "heartbeat", 5*time.Second, nil, logging.NewNop())

	got, errs, err := m.GetHeartbeatsFor(ctx, []string{"worker-0", "worker-1", "worker-2", "worker-9"})
	require.NoError(t, err)
	require.Equal(t, []string{"vip"}, got["worker-0"].Labels)
	require.Empty(t, got["worker-1"].Labels)
	_, ok := got["worker-2"]
	require.False(t, ok, "malformed payload omitted from heartbeats")
	require.Error(t, errs["worker-2"], "…but its decode error is preserved for classification")
	_, ok = got["worker-9"]
	require.False(t, ok, "missing key omitted from heartbeats")
	require.Error(t, errs["worker-9"], "…and its Get error is preserved (jetstream.ErrKeyNotFound)")
	require.Len(t, errs, 2)
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

// fakeKeyWatcher is a minimal jetstream.KeyWatcher double for unit tests.
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

// fakeKV is a jetstream.KeyValue double that counts Watch calls and supports
// closing the current Updates channel to simulate watcher disconnect.
type fakeKV struct {
	mu             sync.Mutex
	watchCount     int
	currentWatcher *fakeKeyWatcher
	// keysBlocking, when true, causes Keys to block until ctx.Done().
	keysBlocking bool
	// keys is the server-side truth returned by Keys when keysBlocking is
	// false. The zero value (nil) preserves the pre-existing behavior of
	// every test that does not call SetKeys: Keys returns (nil, nil), which
	// GetActiveWorkers treats as an empty worker set.
	keys []string
}

// SetKeys configures the key list returned by a subsequent (non-blocking)
// Keys call. Safe for concurrent use: GetActiveWorkers may call Keys from a
// callback goroutine while the test goroutine drives the watcher loop.
func (f *fakeKV) SetKeys(keys []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = keys
}

func (f *fakeKV) WatchCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.watchCount
}

func (f *fakeKV) CloseUpdates() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.currentWatcher != nil {
		close(f.currentWatcher.updates)
		f.currentWatcher = nil
	}
}

func (f *fakeKV) Watch(_ context.Context, _ string, _ ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watchCount++
	w := &fakeKeyWatcher{
		ctx:     context.Background(),
		updates: make(chan jetstream.KeyValueEntry),
		errCh:   make(chan error),
	}
	f.currentWatcher = w

	return w, nil
}

func (f *fakeKV) Keys(ctx context.Context, _ ...jetstream.WatchOpt) ([]string, error) {
	if f.keysBlocking {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.keys, nil
}

// Unimplemented stubs required by the jetstream.KeyValue interface.
func (f *fakeKV) Put(_ context.Context, _ string, _ []byte) (uint64, error) { return 0, nil }
func (f *fakeKV) PutString(_ context.Context, _ string, _ string) (uint64, error) {
	return 0, nil
}
func (f *fakeKV) Create(_ context.Context, _ string, _ []byte, _ ...jetstream.KVCreateOpt) (uint64, error) {
	return 0, nil
}
func (f *fakeKV) Update(_ context.Context, _ string, _ []byte, _ uint64) (uint64, error) {
	return 0, nil
}
func (f *fakeKV) Delete(_ context.Context, _ string, _ ...jetstream.KVDeleteOpt) error { return nil }
func (f *fakeKV) Purge(_ context.Context, _ string, _ ...jetstream.KVDeleteOpt) error  { return nil }
func (f *fakeKV) Get(_ context.Context, _ string) (jetstream.KeyValueEntry, error) {
	return nil, jetstream.ErrKeyNotFound
}
func (f *fakeKV) GetRevision(_ context.Context, _ string, _ uint64) (jetstream.KeyValueEntry, error) {
	return nil, jetstream.ErrKeyNotFound
}
func (f *fakeKV) History(_ context.Context, _ string, _ ...jetstream.WatchOpt) ([]jetstream.KeyValueEntry, error) {
	return nil, errFakeKVNotImplemented
}
func (f *fakeKV) WatchAll(_ context.Context, _ ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return nil, errFakeKVNotImplemented
}
func (f *fakeKV) WatchFiltered(_ context.Context, _ []string, _ ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return nil, errFakeKVNotImplemented
}
func (f *fakeKV) ListKeys(_ context.Context, _ ...jetstream.WatchOpt) (jetstream.KeyLister, error) {
	return nil, errFakeKVNotImplemented
}
func (f *fakeKV) ListKeysFiltered(_ context.Context, _ ...string) (jetstream.KeyLister, error) {
	return nil, errFakeKVNotImplemented
}
func (f *fakeKV) Bucket() string { return "fake" }
func (f *fakeKV) Status(_ context.Context) (jetstream.KeyValueStatus, error) {
	return nil, errFakeKVNotImplemented
}
func (f *fakeKV) PurgeDeletes(_ context.Context, _ ...jetstream.KVPurgeOpt) error { return nil }

// TestWorkerMonitor_ProcessWatcherEvents_ClosedChannel verifies that closing
// the watcher Updates channel causes monitorWatcherWithRetry to rewatch — i.e.
// Watch is called at least twice. Previously the function exited permanently on
// channel close; it must now return an error so the retry loop restarts it.
func TestWorkerMonitor_ProcessWatcherEvents_ClosedChannel(t *testing.T) {
	t.Parallel()

	kv := &fakeKV{}
	monitor := NewWorkerMonitor(kv, "worker", 5*time.Second, nil, logging.NewNop())
	// Speed up the retry backoff so the test finishes quickly.
	// Set on the struct (not the package var) to avoid a data race with
	// parallel tests that also create monitors.
	monitor.watchBaseBackoff = 10 * time.Millisecond

	ctx := t.Context()
	require.NoError(t, monitor.Start(ctx))
	t.Cleanup(func() {
		_ = monitor.Stop()
	})

	// Wait for the first watcher session to be active.
	require.Eventually(t, func() bool {
		return kv.WatchCallCount() >= 1
	}, 200*time.Millisecond, 5*time.Millisecond, "first Watch call should happen quickly")

	// Simulate NATS disconnect: close the Updates channel.
	kv.CloseUpdates()

	// The retry loop should re-call Watch within the 10ms backoff + jitter window.
	require.Eventually(t, func() bool {
		return kv.WatchCallCount() >= 2
	}, 500*time.Millisecond, 5*time.Millisecond, "Watch should be called again after channel close")

	require.NoError(t, monitor.Stop())
}

// TestWorkerMonitor_GetActiveWorkers_BoundedTimeout verifies that GetActiveWorkers
// returns quickly (with context.DeadlineExceeded) when kv.Keys blocks indefinitely.
// Without the boundedOpCtx fix, GetActiveWorkers would block until ctx itself is
// cancelled, which is unbounded in production.
func TestWorkerMonitor_GetActiveWorkers_BoundedTimeout(t *testing.T) {
	t.Parallel()

	kv := &fakeKV{keysBlocking: true}
	fakeTTL := 200 * time.Millisecond
	// NewWorkerMonitor is used only for its boundedOpCtx logic; no Start needed.
	monitor := NewWorkerMonitor(kv, "worker", fakeTTL, nil, logging.NewNop())

	ctx := context.Background()
	start := time.Now()
	_, err := monitor.GetActiveWorkers(ctx)
	elapsed := time.Since(start)

	// The call must return with a deadline-exceeded error: the bounded context
	// (hbTTL/2 = 100ms) must have fired well before the test timeout.
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"GetActiveWorkers must return context.DeadlineExceeded when kv.Keys blocks")

	// Elapsed should be roughly fakeTTL/2 (100ms). Allow 3× for CI headroom.
	require.Less(t, elapsed, 3*fakeTTL,
		"GetActiveWorkers returned too late (%v); expected ~%v", elapsed, fakeTTL/2)
}

// fakeKVEntry is a minimal jetstream.KeyValueEntry double for driving
// processWatcherEvents directly. value is nil unless the test carries a
// heartbeat payload (label-fingerprint tests); nil reproduces the pre-existing
// behavior of every classification test — Value() returns nil and the label
// decode is skipped.
type fakeKVEntry struct {
	key   string
	op    jetstream.KeyValueOp
	value []byte
}

func (e *fakeKVEntry) Bucket() string                  { return "fake" }
func (e *fakeKVEntry) Key() string                     { return e.key }
func (e *fakeKVEntry) Value() []byte                   { return e.value }
func (e *fakeKVEntry) Revision() uint64                { return 0 }
func (e *fakeKVEntry) Created() time.Time              { return time.Time{} }
func (e *fakeKVEntry) Delta() uint64                   { return 0 }
func (e *fakeKVEntry) Operation() jetstream.KeyValueOp { return e.op }

// watcherSession drives one processWatcherEvents session against fakeKV.
type watcherSession struct {
	push func(jetstream.KeyValueEntry) // blocks until the loop consumes the entry
	stop func()                        // cancels the session and waits for exit
}

// startWatcherSession starts processWatcherEvents in a goroutine and returns
// a session handle. The fakeKV's unbuffered updates channel means push()
// returns only after the loop has received (though not necessarily fully
// processed) the entry.
func startWatcherSession(t *testing.T, m *WorkerMonitor, kv *fakeKV) *watcherSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.processWatcherEvents(ctx)
	}()
	// Wait for the session's Watch call so currentWatcher is set.
	require.Eventually(t, func() bool { return kv.WatchCallCount() >= 1 }, time.Second, time.Millisecond)

	return &watcherSession{
		push: func(e jetstream.KeyValueEntry) {
			kv.mu.Lock()
			w := kv.currentWatcher
			kv.mu.Unlock()
			require.NotNil(t, w)
			select {
			case w.updates <- e:
			case <-time.After(2 * time.Second):
				t.Fatal("watcher loop did not consume entry")
			}
		},
		stop: func() {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("watcher session did not exit")
			}
		},
	}
}

// newFakeClockAt returns the package's fakeClock (see newFakeClock in
// calculator_state_test.go) initialized to start rather than time.Now(), so
// classifier tests below can pin the same documented base times used
// throughout this file. fakeClock is mutex-guarded, so — unlike the
// atomic.Pointer[time.Time] machinery this replaces — Advance/Now are safe
// to call from the test goroutine while the watcher goroutine concurrently
// reads via m.now.
func newFakeClockAt(start time.Time) *fakeClock {
	c := newFakeClock()
	c.Advance(start.Sub(c.Now()))

	return c
}

// newClassifierMonitor builds a monitor wired to a fake clock and an
// onChange counter, suitable for driving processWatcherEvents directly.
func newClassifierMonitor(hbTTL time.Duration, clock *fakeClock) (*WorkerMonitor, *fakeKV, *atomic.Int32) {
	kv := &fakeKV{}
	var calls atomic.Int32
	m := NewWorkerMonitor(kv, "worker", hbTTL,
		func(ctx context.Context) error { calls.Add(1); return nil },
		logging.NewNop(),
	)
	m.now = clock.Now

	return m, kv, &calls
}

// waitDebounce sleeps past the 100ms debounce window plus scheduling slack.
func waitDebounce() { time.Sleep(250 * time.Millisecond) }

func TestWorkerMonitor_RefreshSuppressed(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))
	m, kv, calls := newClassifierMonitor(10*time.Second, clock)
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	// First PUT of w1 is a join → one check.
	s.push(&fakeKVEntry{key: "worker.w1", op: jetstream.KeyValuePut})
	waitDebounce()
	require.EqualValues(t, 1, calls.Load(), "first PUT (join) must trigger one check")

	// Refreshes within hbTTL: advance clock 3s per beat, push 3 refreshes.
	for i := 0; i < 3; i++ {
		clock.Advance(3 * time.Second)
		s.push(&fakeKVEntry{key: "worker.w1", op: jetstream.KeyValuePut})
	}
	waitDebounce()
	require.EqualValues(t, 1, calls.Load(), "refresh PUTs within hbTTL must be suppressed (no additional checks)")
}

func TestWorkerMonitor_JoinAndLeaveTrigger(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))
	m, kv, calls := newClassifierMonitor(10*time.Second, clock)
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	s.push(&fakeKVEntry{key: "worker.w1", op: jetstream.KeyValuePut}) // join
	waitDebounce()
	require.EqualValues(t, 1, calls.Load())

	s.push(&fakeKVEntry{key: "worker.w2", op: jetstream.KeyValuePut}) // another join
	waitDebounce()
	require.EqualValues(t, 2, calls.Load(), "unknown-key PUT must trigger")

	s.push(&fakeKVEntry{key: "worker.w1", op: jetstream.KeyValueDelete}) // graceful leave
	waitDebounce()
	require.EqualValues(t, 3, calls.Load(), "DELETE must trigger")

	s.push(&fakeKVEntry{key: "worker.w2", op: jetstream.KeyValuePurge}) // purge
	waitDebounce()
	require.EqualValues(t, 4, calls.Load(), "PURGE must trigger")
}

func TestWorkerMonitor_StaleRejoinTriggers(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))
	m, kv, calls := newClassifierMonitor(10*time.Second, clock)
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	s.push(&fakeKVEntry{key: "worker.w1", op: jetstream.KeyValuePut}) // join
	waitDebounce()
	require.EqualValues(t, 1, calls.Load())

	// Gap > hbTTL: the worker's key must have expired server-side; the next
	// PUT is a re-join and must trigger. This is a single stale key with no
	// other tracked worker, so the PUT itself updates lastSeen[key] = now
	// BEFORE sweepExpired runs (worker_monitor.go:434-452) — the same key
	// can no longer be found stale by the sweep in the same event. The
	// check fires from classification (join) alone, so exactly one
	// additional check is expected.
	clock.Advance(11 * time.Second)
	s.push(&fakeKVEntry{key: "worker.w1", op: jetstream.KeyValuePut})
	waitDebounce()
	require.EqualValues(t, 2, calls.Load(), "PUT after >hbTTL gap must trigger exactly once (re-join)")
}

func TestWorkerMonitor_ZeroTTLAlwaysTriggers(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))
	// hbTTL == 0: direct unit seam only (Start would panic on ticker);
	// every PUT must classify as join, reproducing pre-change behavior.
	m, kv, calls := newClassifierMonitor(0, clock)
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	for i := 0; i < 3; i++ {
		s.push(&fakeKVEntry{key: "worker.w1", op: jetstream.KeyValuePut})
		waitDebounce()
	}
	require.EqualValues(t, 3, calls.Load(), "hbTTL==0 must trigger on every PUT (no suppression)")
}

func TestWorkerMonitor_SweepFlagsExpiredWorker(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))
	m, kv, calls := newClassifierMonitor(10*time.Second, clock)
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	// Join A and B.
	s.push(&fakeKVEntry{key: "worker.a", op: jetstream.KeyValuePut})
	s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	waitDebounce()
	joins := calls.Load()

	// B refreshes on a 3s beat; A goes silent. When B's refresh arrives
	// with A stale (>= hbTTL since A's lastSeen), the sweep must flag A
	// and force a check even though B's PUT alone is a suppressed refresh.
	for i := 0; i < 3; i++ { // t=+3s,+6s,+9s — A not yet stale, suppressed
		clock.Advance(3 * time.Second)
		s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	}
	waitDebounce()
	require.Equal(t, joins, calls.Load(), "no check while A is within hbTTL and B refreshes")

	clock.Advance(3 * time.Second) // t=+12s — A stale (12s >= 10s)
	s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	waitDebounce()
	require.Equal(t, joins+1, calls.Load(), "sweep must force a check when a tracked worker exceeds hbTTL")
}

func TestWorkerMonitor_HolidayUnsuppressesThenResumes(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))
	m, kv, calls := newClassifierMonitor(10*time.Second, clock)
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	s.push(&fakeKVEntry{key: "worker.a", op: jetstream.KeyValuePut})
	s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	waitDebounce()

	// Make A stale → sweep fires at B's next refresh, opening a 3×hbTTL holiday.
	clock.Advance(11 * time.Second)
	s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	waitDebounce()
	afterFlag := calls.Load()

	// During the holiday every refresh triggers (crash-window behavior
	// identical to pre-change code).
	for i := 0; i < 3; i++ {
		clock.Advance(3 * time.Second)
		s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
		waitDebounce()
	}
	require.Equal(t, afterFlag+3, calls.Load(), "refreshes during the holiday must trigger checks")

	// Advance past the holiday (3×hbTTL = 30s from the flag at t=+11s,
	// i.e. until t=+41s) with B fresh — suppression resumes. Beat every
	// 3s so B never goes stale itself: pushes land at t=+23..+44s.
	for i := 0; i < 8; i++ {
		clock.Advance(3 * time.Second)
		s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	}
	waitDebounce()
	settled := calls.Load()
	clock.Advance(3 * time.Second)
	s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	waitDebounce()
	require.Equal(t, settled, calls.Load(), "after the holiday, refreshes are suppressed again")
}

func TestWorkerMonitor_OverlappingCrashExtendsHoliday(t *testing.T) {
	t.Parallel()
	// A second worker crossing staleness DURING an open holiday must be
	// swept too, and sweeping re-stamps unsuppressedUntil — the holiday
	// EXTENDS. During a holiday every refresh triggers anyway, so the
	// only observable that discriminates the second sweep is whether
	// refreshes still trigger AFTER the first holiday would have ended.
	clock := newFakeClockAt(time.Unix(1000, 0))
	m, kv, calls := newClassifierMonitor(10*time.Second, clock)
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	// Joins at t=0.
	s.push(&fakeKVEntry{key: "worker.a", op: jetstream.KeyValuePut})
	s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	s.push(&fakeKVEntry{key: "worker.c", op: jetstream.KeyValuePut})
	waitDebounce()

	// Keep B and C fresh through t=+9s while A goes silent.
	for i := 0; i < 3; i++ { // pushes at +3, +6, +9
		clock.Advance(3 * time.Second)
		s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
		s.push(&fakeKVEntry{key: "worker.c", op: jetstream.KeyValuePut})
	}
	waitDebounce()

	// t=+12s: A stale (12s ≥ 10s), B fresh (3s) → sweep flags ONLY A.
	// First holiday: until +12+30 = +42s.
	clock.Advance(3 * time.Second)
	s.push(&fakeKVEntry{key: "worker.c", op: jetstream.KeyValuePut})
	waitDebounce()

	// B goes silent after its +9s beat. C beats on: at t=+21s the sweep
	// finds B stale (12s since +9s) → flags B → holiday RE-STAMPED to
	// +21+30 = +51s.
	for i := 0; i < 3; i++ { // C pushes at +15, +18, +21
		clock.Advance(3 * time.Second)
		s.push(&fakeKVEntry{key: "worker.c", op: jetstream.KeyValuePut})
	}
	waitDebounce()

	// C beats through the ORIGINAL holiday end (+42s): pushes at
	// +24..+45s. Settle, then probe.
	for i := 0; i < 8; i++ {
		clock.Advance(3 * time.Second)
		s.push(&fakeKVEntry{key: "worker.c", op: jetstream.KeyValuePut})
	}
	waitDebounce()
	settled := calls.Load()

	// Probe 1 — t=+48s: AFTER the first holiday (+42s) but INSIDE the
	// extension (+51s): a C refresh must STILL trigger. This is the
	// discriminator: without B's sweep-extension, this push would be
	// suppressed.
	clock.Advance(3 * time.Second)
	s.push(&fakeKVEntry{key: "worker.c", op: jetstream.KeyValuePut})
	waitDebounce()
	require.Equal(t, settled+1, calls.Load(),
		"refresh after the first holiday's end must still trigger — the second crash extended the holiday")

	// Probe 2 — t=+54s: past the extension (+51s): suppressed again.
	clock.Advance(6 * time.Second)
	s.push(&fakeKVEntry{key: "worker.c", op: jetstream.KeyValuePut})
	waitDebounce()
	require.Equal(t, settled+1, calls.Load(), "suppression resumes after the extended holiday")

	// B's rejoin after being swept classifies as a join and triggers.
	s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	waitDebounce()
	require.Equal(t, settled+2, calls.Load(), "swept worker's rejoin must classify as join and trigger")
}

func TestWorkerMonitor_FalseFlagSelfCorrects(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))

	kv := &fakeKV{}
	var calls atomic.Int32
	release := make(chan struct{})
	m := NewWorkerMonitor(kv, "worker", 10*time.Second,
		func(ctx context.Context) error {
			calls.Add(1)
			if calls.Load() == 2 { // block only the sweep-triggered check
				<-release
			}
			return nil
		},
		logging.NewNop(),
	)
	m.now = clock.Now
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	s.push(&fakeKVEntry{key: "worker.a", op: jetstream.KeyValuePut})
	s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	waitDebounce() // check #1 (joins coalesced)

	// Keep B FRESH with 3s beats while A goes silent. These are suppressed
	// refreshes — no checks. Critical for the discriminator below: B must
	// never itself classify as stale, so the only thing that can force a
	// check at t=+12s is the sweep flagging A.
	for i := 0; i < 3; i++ { // pushes at +3, +6, +9
		clock.Advance(3 * time.Second)
		s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	}
	waitDebounce()
	require.EqualValues(t, 1, calls.Load(), "B's fresh refreshes must stay suppressed while A ages")

	// t=+12s: B's entry is a fresh refresh (3s since its last beat) —
	// suppressed by classification alone. The sweep, however, finds A
	// stale (12s ≥ hbTTL) and forces check #2: this assertion FAILS if
	// sweepExpired does not exist or does not fire. (False-flag framing:
	// locally-stale lastSeen[a] simulates delivery delay; A may be alive
	// server-side.) The callback blocks at check #2.
	clock.Advance(3 * time.Second)
	s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	require.Eventually(t, func() bool { return calls.Load() == 2 },
		2*time.Second, 5*time.Millisecond,
		"sweep must force a check off B's suppressed refresh when A is locally stale")

	// A's own delayed PUT arrives while the callback blocks. Push from a
	// goroutine: the loop is busy in onChangeCb, so this send parks until
	// the callback returns.
	pushed := make(chan struct{})
	go func() {
		defer close(pushed)
		kv.mu.Lock()
		w := kv.currentWatcher
		kv.mu.Unlock()
		w.updates <- &fakeKVEntry{key: "worker.a", op: jetstream.KeyValuePut}
	}()

	close(release) // unblock check #2
	select {
	case <-pushed:
	case <-time.After(2 * time.Second):
		t.Fatal("queued entry was not consumed after callback release")
	}
	waitDebounce()
	// A was swept from lastSeen, so its delayed PUT is a join → check #3.
	// Self-correction: the triggered check's real scan (in production)
	// observes A alive; at this unit level we pin the classification and
	// trigger behavior only — see TestWorkerMonitor_FalseFlagSelfCorrectsWithLiveScan
	// for the companion test that proves the scan/emergency half.
	require.EqualValues(t, 3, calls.Load(), "swept-then-reappearing worker must re-trigger exactly once (join)")
}

// TestWorkerMonitor_FalseFlagSelfCorrectsWithLiveScan is the companion to
// TestWorkerMonitor_FalseFlagSelfCorrects: it proves the scan/emergency half
// that the classification-only test above documents as out of scope. Server
// truth (the fake KV's key list) holds A and B alive for the whole test —
// only A's LOCAL lastSeen goes stale (simulating a delivery delay) — so the
// sweep's flag on A is a false flag by construction. The onChange callback
// wired here mirrors the real reconciliation shape used by
// Calculator.observeAndDecideLocked (calculator.go:1017-1049): a fresh
// GetActiveWorkers scan feeds a real EmergencyDetector.CheckEmergency, and
// because A is visible in curr, the detector must clear it and claim
// nothing.
func TestWorkerMonitor_FalseFlagSelfCorrectsWithLiveScan(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))

	kv := &fakeKV{}
	// Server-side truth never changes: both workers stay heartbeat-visible
	// for the whole test, even while A's local lastSeen goes stale.
	kv.SetKeys([]string{"worker.a", "worker.b"})

	detector := NewEmergencyDetector(10 * time.Second)
	detector.now = clock.Now // same fake clock as the monitor: deterministic grace math

	var calls atomic.Int32
	var scanResult []string
	var scanErr error
	var emergencyClaimed atomic.Bool
	var pendingClaimed atomic.Bool
	release := make(chan struct{})

	var m *WorkerMonitor
	m = NewWorkerMonitor(kv, "worker", 10*time.Second,
		func(ctx context.Context) error {
			n := calls.Add(1)
			if n == 2 { // the sweep-triggered check
				// Real reconciliation shape: scan current KV truth via the
				// monitor's own GetActiveWorkers, then reconcile against a
				// real EmergencyDetector exactly as the calculator does.
				workers, err := m.GetActiveWorkers(ctx)
				scanResult = workers
				scanErr = err

				curr := make(map[string]bool, len(workers))
				for _, w := range workers {
					curr[w] = true
				}
				prev := map[string]bool{"a": true, "b": true} // last known-active set before the sweep
				emergency, _, pending := detector.CheckEmergency(prev, curr)
				emergencyClaimed.Store(emergency)
				pendingClaimed.Store(pending)

				<-release // block only the sweep-triggered check, same pattern as the classification-only test
			}

			return nil
		},
		logging.NewNop(),
	)
	m.now = clock.Now
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	s.push(&fakeKVEntry{key: "worker.a", op: jetstream.KeyValuePut})
	s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	waitDebounce() // check #1 (joins coalesced)

	// Keep B fresh with 3s beats while A goes silent locally. Server truth
	// (kv.keys) is untouched throughout: A stays alive from a live-scan
	// perspective.
	for i := 0; i < 3; i++ { // pushes at +3, +6, +9
		clock.Advance(3 * time.Second)
		s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	}
	waitDebounce()
	require.EqualValues(t, 1, calls.Load(), "B's fresh refreshes must stay suppressed while A ages")

	// t=+12s: B's entry is a fresh refresh — suppressed by classification
	// alone. The sweep finds A locally stale (12s >= hbTTL) and forces
	// check #2, which blocks inside the scan/emergency reconciliation above.
	clock.Advance(3 * time.Second)
	s.push(&fakeKVEntry{key: "worker.b", op: jetstream.KeyValuePut})
	require.Eventually(t, func() bool { return calls.Load() == 2 },
		2*time.Second, 5*time.Millisecond,
		"sweep must force a check off B's suppressed refresh when A is locally stale")

	// A's own delayed PUT arrives while the callback blocks. Push from a
	// goroutine: the loop is busy in onChangeCb, so this send parks until
	// the callback returns — reusing the queued-PUT pattern from the
	// classification-only test.
	pushed := make(chan struct{})
	go func() {
		defer close(pushed)
		kv.mu.Lock()
		w := kv.currentWatcher
		kv.mu.Unlock()
		w.updates <- &fakeKVEntry{key: "worker.a", op: jetstream.KeyValuePut}
	}()

	close(release) // unblock check #2
	select {
	case <-pushed:
	case <-time.After(2 * time.Second):
		t.Fatal("queued entry was not consumed after callback release")
	}
	waitDebounce()

	// (a) The sweep-triggered check's real scan observes A alive.
	require.NoError(t, scanErr, "sweep-triggered GetActiveWorkers scan must succeed")
	require.ElementsMatch(t, []string{"a", "b"}, scanResult,
		"sweep-triggered check's real scan must observe A alive alongside B")

	// (b) No emergency is claimed and nothing is left pending: A visible in
	// curr must clear the detector's tracking for A.
	require.False(t, emergencyClaimed.Load(), "A visible in curr must clear the detector; no emergency may be claimed")
	require.False(t, pendingClaimed.Load(), "A visible in curr must clear the disappearance; nothing may remain pending")

	// (c) A was swept from lastSeen, so its delayed PUT classifies as a
	// join → exactly one additional check (#3), no oscillation.
	require.EqualValues(t, 3, calls.Load(), "swept-then-reappearing worker must re-trigger exactly once (join)")
}

func TestWorkerMonitor_SweepDisabledAtZeroTTL(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))
	m, kv, calls := newClassifierMonitor(0, clock)
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	s.push(&fakeKVEntry{key: "worker.a", op: jetstream.KeyValuePut})
	waitDebounce()
	clock.Advance(time.Hour)
	s.push(&fakeKVEntry{key: "worker.a", op: jetstream.KeyValuePut})
	waitDebounce()
	// Both PUTs trigger (zero-TTL: every PUT is a join); the sweep is a
	// guard-disabled no-op and must not panic or double-fire.
	require.EqualValues(t, 2, calls.Load())
}

func TestWorkerMonitor_InitialReplayCoalescesWithinWindow(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))
	m, kv, calls := newClassifierMonitor(10*time.Second, clock)
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	// K replayed keys delivered within one 100ms debounce window →
	// exactly one coalesced check (spec: window-scoped claim; a replay
	// spanning multiple windows may produce multiple checks, same as
	// pre-change code).
	for i := 1; i <= 5; i++ {
		s.push(&fakeKVEntry{key: fmt.Sprintf("worker.w%d", i), op: jetstream.KeyValuePut})
	}
	waitDebounce()
	require.EqualValues(t, 1, calls.Load(), "replay entries within one debounce window must coalesce into one check")
}

func TestWorkerMonitor_RestartSessionResetsClassifierState(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))

	kv := &fakeKV{}
	var calls atomic.Int32
	m := NewWorkerMonitor(kv, "worker", 10*time.Second,
		func(ctx context.Context) error { calls.Add(1); return nil },
		logging.NewNop(),
	)
	m.now = clock.Now
	m.watchBaseBackoff = 10 * time.Millisecond // fast retry, matches existing restart tests

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.monitorWatcherWithRetry(ctx)
	require.Eventually(t, func() bool { return kv.WatchCallCount() >= 1 }, time.Second, time.Millisecond)

	// Session 1: a single join establishes classifier state.
	kv.mu.Lock()
	w := kv.currentWatcher
	kv.mu.Unlock()
	w.updates <- &fakeKVEntry{key: "worker.a", op: jetstream.KeyValuePut}
	waitDebounce()
	require.EqualValues(t, 1, calls.Load())

	// Kill the session; the retry loop must start a fresh one.
	kv.CloseUpdates()
	require.Eventually(t, func() bool { return kv.WatchCallCount() >= 2 }, 2*time.Second, 10*time.Millisecond)

	// Session 2: the SAME key replayed is unknown to the fresh session's
	// lastSeen → join → triggers. (Fresh state; no staleness carryover.)
	kv.mu.Lock()
	w = kv.currentWatcher
	kv.mu.Unlock()
	w.updates <- &fakeKVEntry{key: "worker.a", op: jetstream.KeyValuePut}
	waitDebounce()
	require.EqualValues(t, 2, calls.Load(), "restarted session must rebuild classifier state from replay")
}

func TestWorkerMonitor_PollingCadenceUnaffectedBySuppression(t *testing.T) {
	t.Parallel()
	// Full monitor via Start: the polling ticker (hbTTL/2) must keep
	// invoking onChange regardless of watcher-side suppression — it is
	// the ground-truth crash detector and the enumeration-stall
	// detector's designed sampling source (counter behavior itself is
	// pinned in calculator_audit_test.go).
	kv := &fakeKV{}
	var calls atomic.Int32
	m := NewWorkerMonitor(kv, "worker", 200*time.Millisecond, // ticker at 100ms
		func(ctx context.Context) error { calls.Add(1); return nil },
		logging.NewNop(),
	)
	require.NoError(t, m.Start(context.Background()))
	defer func() { _ = m.Stop() }()

	require.Eventually(t, func() bool { return calls.Load() >= 3 },
		2*time.Second, 10*time.Millisecond,
		"polling ticker must fire at hbTTL/2 cadence independent of watcher suppression")
}
