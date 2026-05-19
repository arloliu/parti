package parti

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// P6: ID-based diff in recordAssignmentMetrics
// ============================================================================

// captureMetrics wraps NopMetrics and records RecordAssignmentChange calls.
type captureMetrics struct {
	*metrics.NopMetrics
	mu      sync.Mutex
	added   int
	removed int
	version int64
}

func (c *captureMetrics) RecordAssignmentChange(added, removed int, version int64) {
	c.mu.Lock()
	c.added = added
	c.removed = removed
	c.version = version
	c.mu.Unlock()
}

// TestRecordAssignmentMetrics_IDBasedDiff verifies that when two assignments have the
// same number of partitions but different partition IDs, recordAssignmentMetrics
// correctly reports both an addition and a removal instead of reporting zeros.
func TestRecordAssignmentMetrics_IDBasedDiff(t *testing.T) {
	t.Parallel()

	cm := &captureMetrics{NopMetrics: metrics.NewNop()}
	m := &Manager{metrics: cm, logger: logging.NewNop()}

	// Old: [A, B], New: [B, C] — same length but C replaces A.
	// Length-based diff would produce added=0, removed=0.
	// ID-based diff must produce added=1 (C), removed=1 (A).
	oldAsgn := Assignment{
		Version:    1,
		Partitions: []Partition{{Keys: []string{"A"}}, {Keys: []string{"B"}}},
	}
	newAsgn := Assignment{
		Version:    2,
		Partitions: []Partition{{Keys: []string{"B"}}, {Keys: []string{"C"}}},
	}

	m.recordAssignmentMetrics(oldAsgn, newAsgn)

	cm.mu.Lock()
	defer cm.mu.Unlock()
	require.Equal(t, 1, cm.added, "one partition (C) was added")
	require.Equal(t, 1, cm.removed, "one partition (A) was removed")
	require.Equal(t, int64(2), cm.version)
}

// TestRecordAssignmentMetrics_PureGrowth verifies that adding partitions without
// removing any reports added>0, removed=0.
func TestRecordAssignmentMetrics_PureGrowth(t *testing.T) {
	t.Parallel()

	cm := &captureMetrics{NopMetrics: metrics.NewNop()}
	m := &Manager{metrics: cm, logger: logging.NewNop()}

	oldAsgn := Assignment{Version: 1, Partitions: []Partition{{Keys: []string{"A"}}}}
	newAsgn := Assignment{Version: 2, Partitions: []Partition{{Keys: []string{"A"}}, {Keys: []string{"B"}}}}

	m.recordAssignmentMetrics(oldAsgn, newAsgn)

	cm.mu.Lock()
	defer cm.mu.Unlock()
	require.Equal(t, 1, cm.added)
	require.Equal(t, 0, cm.removed)
}

// TestRecordAssignmentMetrics_PureShrink verifies that removing partitions without
// adding any reports added=0, removed>0.
func TestRecordAssignmentMetrics_PureShrink(t *testing.T) {
	t.Parallel()

	cm := &captureMetrics{NopMetrics: metrics.NewNop()}
	m := &Manager{metrics: cm, logger: logging.NewNop()}

	oldAsgn := Assignment{Version: 1, Partitions: []Partition{{Keys: []string{"A"}}, {Keys: []string{"B"}}}}
	newAsgn := Assignment{Version: 2, Partitions: []Partition{{Keys: []string{"A"}}}}

	m.recordAssignmentMetrics(oldAsgn, newAsgn)

	cm.mu.Lock()
	defer cm.mu.Unlock()
	require.Equal(t, 0, cm.added)
	require.Equal(t, 1, cm.removed)
}

// ============================================================================
// P5: Assignment watcher retry on transient failure
// ============================================================================

// mockKeyWatcher is a minimal jetstream.KeyWatcher implementation that signals
// the caller via a closed channel and does nothing else.
type mockKeyWatcher struct {
	updatesCh chan jetstream.KeyValueEntry
	errCh     chan error
}

func newMockKeyWatcher() *mockKeyWatcher {
	ch := make(chan jetstream.KeyValueEntry)
	close(ch) // Immediately signal Updates() closed → watchAssignment returns err.
	return &mockKeyWatcher{
		updatesCh: ch,
		errCh:     make(chan error),
	}
}

func (w *mockKeyWatcher) Context() context.Context                { return context.Background() }
func (w *mockKeyWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updatesCh }
func (w *mockKeyWatcher) Stop() error                             { return nil }
func (w *mockKeyWatcher) Error() <-chan error                     { return w.errCh }

// mockRetryKV fails Watch on the first call and succeeds (with a watcher
// whose Updates() channel is immediately closed) on subsequent calls.
type mockRetryKV struct {
	jetstream.KeyValue
	watchCalls atomic.Int32
}

func (m *mockRetryKV) Watch(_ context.Context, _ string, _ ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	call := m.watchCalls.Add(1)
	if call == 1 {
		return nil, errors.New("transient NATS error")
	}
	return newMockKeyWatcher(), nil
}

// TestMonitorAssignmentChanges_RewatchesOnChannelClose verifies that
// monitorAssignmentChanges treats both an initial Watch error and a closed
// Updates() channel as recoverable conditions: each triggers a re-Watch via
// the backoff loop instead of exiting permanently. The monitor only stops
// when its context is cancelled.
//
// W12 / PR-1 changed `watchAssignment` to return an error on channel close
// (previously it returned nil → clean exit). Before that change, a closed
// channel silently terminated the monitor and any subsequent alias updates
// (including rolling-upgrade alias.<W>) were missed until process restart.
func TestMonitorAssignmentChanges_RewatchesOnChannelClose(t *testing.T) {
	t.Parallel()

	// Generous timeout: 2s base backoff + jitter for the first rewatch,
	// 4s + jitter for the second.
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	kv := &mockRetryKV{}
	m := &Manager{
		logger: logging.NewNop(),
	}
	m.workerID.Store("worker-0")

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.monitorAssignmentChanges(ctx, kv)
	}()

	// Three Watch calls = original + two rewatches: proves the rewatch
	// path triggers on both the initial Watch error AND on each
	// subsequent channel-close.
	require.Eventually(t, func() bool {
		return kv.watchCalls.Load() >= 3
	}, 12*time.Second, 50*time.Millisecond,
		"monitorAssignmentChanges must rewatch after Watch error AND after channel close")

	// Cancel context: goroutine must exit cleanly.
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("monitorAssignmentChanges did not exit after ctx cancel")
	}
}

// ============================================================================
// P7: monitorCalculatorState channel-based sync (no time.Sleep)
// ============================================================================

// TestStartCalculator_NoRaceBetweenSubscribeAndStart verifies that the ready channel
// mechanism ensures monitorCalculatorState has subscribed before calc.Start is called.
// It uses the race detector (go test -race) to catch races that the old time.Sleep(10ms)
// approach could not reliably prevent.
//
// The test checks the observable guarantee: the subscription is established before
// Start returns, so the initial state push from SubscribeToStateChanges is never missed.
func TestMonitorCalculatorState_ReadyChannelEstablishesSubscriptionFirst(t *testing.T) {
	t.Parallel()

	// Build a minimal calculator mock whose SubscribeToStateChanges records when
	// Subscribe was called relative to when the ready channel is closed.
	var subscribedAt atomic.Int64  // UnixNano when Subscribe was called
	var readyClosedAt atomic.Int64 // UnixNano when readyCh was closed (set in goroutine)

	stateCh := make(chan types.CalculatorState, 4)
	close(stateCh) // signal "nothing to send"; goroutine will exit immediately

	calcMock := &monitorTestCalculator{
		onSubscribe: func() {
			subscribedAt.Store(time.Now().UnixNano())
		},
		stateCh: stateCh,
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancelled immediately so the monitor loop exits after subscribing

	m := &Manager{
		logger: logging.NewNop(),
		ctx:    ctx,
	}

	readyCh := make(chan struct{})

	go func() {
		m.monitorCalculatorState(calcMock, readyCh)
	}()

	// Capture when readyCh closes (i.e., when subscription is established).
	<-readyCh
	readyClosedAt.Store(time.Now().UnixNano())

	sub := subscribedAt.Load()
	ready := readyClosedAt.Load()

	require.NotZero(t, sub, "Subscribe must have been called")
	require.LessOrEqual(t, sub, ready,
		"Subscribe must be called before or at the same time as readyCh close")
}

// monitorTestCalculator satisfies the assignmentCalculator interface for testing.
type monitorTestCalculator struct {
	onSubscribe func()
	stateCh     chan types.CalculatorState
	state       atomic.Int64 // types.CalculatorState; controllable by tests
}

func (c *monitorTestCalculator) Start(context.Context) error { return nil }
func (c *monitorTestCalculator) Stop(context.Context) error  { return nil }
func (c *monitorTestCalculator) SubscribeToStateChanges() (<-chan types.CalculatorState, func()) {
	if c.onSubscribe != nil {
		c.onSubscribe()
	}
	return c.stateCh, func() {}
}
func (c *monitorTestCalculator) TriggerRebalance(context.Context) error { return nil }
func (c *monitorTestCalculator) GetState() types.CalculatorState {
	return types.CalculatorState(c.state.Load())
}
func (c *monitorTestCalculator) setState(s types.CalculatorState) {
	c.state.Store(int64(s))
}
