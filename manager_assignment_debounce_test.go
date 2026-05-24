package parti

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// fakeKeyWatcher is a minimal in-process jetstream.KeyWatcher for the
// debounce tests. Stop() returns nil and does NOT close the channel —
// the deferred watcher.Stop() in runAssignmentWatchSession runs at
// function exit. Tests that close the channel explicitly (e.g.
// TestAssignmentWatcher_PendingEntryFlushesOnClose) do so themselves;
// not closing in Stop avoids double-close.
type fakeKeyWatcher struct {
	ch chan jetstream.KeyValueEntry
}

func newFakeKeyWatcher() *fakeKeyWatcher {
	return &fakeKeyWatcher{ch: make(chan jetstream.KeyValueEntry, 32)}
}

func (f *fakeKeyWatcher) Updates() <-chan jetstream.KeyValueEntry { return f.ch }
func (f *fakeKeyWatcher) Stop() error                             { return nil }

var _ jetstream.KeyWatcher = (*fakeKeyWatcher)(nil)

// fakeVersionEntry is a minimal jetstream.KeyValueEntry whose Value()
// returns the JSON encoding of Assignment{Version: v}. Implements all
// methods required by the interface.
type fakeVersionEntry struct {
	value []byte
}

func fakeEntryWithVersion(v int64) jetstream.KeyValueEntry {
	b, _ := json.Marshal(Assignment{Version: v})
	return fakeVersionEntry{value: b}
}

func (e fakeVersionEntry) Bucket() string                  { return "test-bucket" }
func (e fakeVersionEntry) Key() string                     { return "test-key" }
func (e fakeVersionEntry) Value() []byte                   { return e.value }
func (e fakeVersionEntry) Revision() uint64                { return 1 }
func (e fakeVersionEntry) Created() time.Time              { return time.Time{} }
func (e fakeVersionEntry) Delta() uint64                   { return 0 }
func (e fakeVersionEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }

var _ jetstream.KeyValueEntry = fakeVersionEntry{}

// decodeVersion extracts the Version field from a KeyValueEntry's JSON value.
// Uses direct json.Unmarshal to avoid requiring a *Manager receiver.
func decodeVersion(entry jetstream.KeyValueEntry) int64 {
	var a Assignment
	if err := json.Unmarshal(entry.Value(), &a); err != nil {
		return -1
	}
	return a.Version
}

// TestAssignmentWatcher_DebouncesMultiVersionBurst delivers V=10..V=14
// inside 50 ms with a 100 ms debounce window and asserts
// handleAssignmentEntry runs exactly once, with V=14.
func TestAssignmentWatcher_DebouncesMultiVersionBurst(t *testing.T) {
	const window = 100 * time.Millisecond
	m := newTestManagerWithMetrics(t, newRecordingMetrics())
	m.cfg.AssignmentWatcherDebounce = window

	var processed atomic.Int64
	var lastVersion atomic.Int64
	m.testHookHandleAssignment = func(workerID string, e jetstream.KeyValueEntry) {
		processed.Add(1)
		lastVersion.Store(decodeVersion(e))
	}

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runAssignmentWatchSession(m.ctx, nil /* kv */, watcher, nil /* no reconcile */, "worker-1", "key")
	}()

	for v := int64(10); v <= 14; v++ {
		watcher.ch <- fakeEntryWithVersion(v)
		time.Sleep(8 * time.Millisecond)
	}

	// Burst delivered. Wait > debounce + scheduling slack.
	time.Sleep(window + 100*time.Millisecond)

	require.Equal(t, int64(1), processed.Load(), "debounce must collapse burst")
	require.Equal(t, int64(14), lastVersion.Load(), "must process the latest version")
}

// TestAssignmentWatcher_DebounceResetsOnEachEntry verifies the idle-window
// semantics: a steady drip of entries spaced just below the window must
// keep the timer reset and NOT fire until the stream goes idle.
func TestAssignmentWatcher_DebounceResetsOnEachEntry(t *testing.T) {
	const window = 100 * time.Millisecond
	m := newTestManagerWithMetrics(t, newRecordingMetrics())
	m.cfg.AssignmentWatcherDebounce = window

	var processed atomic.Int64
	m.testHookHandleAssignment = func(workerID string, e jetstream.KeyValueEntry) {
		processed.Add(1)
	}

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runAssignmentWatchSession(m.ctx, nil, watcher, nil, "worker-1", "key")
	}()

	// Drip 10 entries spaced 50 ms apart (well below the 100 ms window).
	// Timer must reset each time; no fire during the drip.
	deadline := time.Now().Add(500 * time.Millisecond)
	v := int64(1)
	for time.Now().Before(deadline) {
		watcher.ch <- fakeEntryWithVersion(v)
		v++
		time.Sleep(50 * time.Millisecond)
	}

	require.Zero(t, processed.Load(), "debounce must NOT fire while stream is busy")

	// Now go idle. Wait > window.
	time.Sleep(window + 100*time.Millisecond)
	require.Equal(t, int64(1), processed.Load(), "debounce must fire exactly once after idle")
}

// TestAssignmentWatcher_DebounceCancelDoesNotFlush verifies that when
// m.ctx is cancelled (e.g. via Stop) while a debounced entry is pending,
// the entry is dropped and no apply runs — Stop must not race
// background apply work into the wait group.
func TestAssignmentWatcher_DebounceCancelDoesNotFlush(t *testing.T) {
	const window = 5 * time.Second
	m := newTestManagerWithMetrics(t, newRecordingMetrics())
	m.cfg.AssignmentWatcherDebounce = window

	var processed atomic.Int64
	m.testHookHandleAssignment = func(workerID string, e jetstream.KeyValueEntry) {
		processed.Add(1)
	}

	watcher := newFakeKeyWatcher()
	sessionDone := make(chan struct{})
	go func() {
		_ = m.runAssignmentWatchSession(m.ctx, nil, watcher, nil, "worker-1", "key")
		close(sessionDone)
	}()

	// Deliver an entry — debounce timer starts (5s window).
	watcher.ch <- fakeEntryWithVersion(99)
	time.Sleep(50 * time.Millisecond) // let the debounce arm receive it

	// Now cancel the manager context, simulating Stop's first action.
	// (Fixture is not Stop-safe — cancel directly.)
	m.cancel()

	// Wait for session to exit.
	select {
	case <-sessionDone:
	case <-time.After(2 * time.Second):
		t.Fatal("session did not exit after ctx cancel")
	}

	// Hook MUST NOT have been called.
	require.Zero(t, processed.Load(), "pending entry must be dropped on ctx cancel, not applied during Stop")
}

// TestAssignmentWatcher_PendingEntryFlushesOnClose verifies that a watcher
// channel close while an entry is pending still processes that pending
// entry exactly once.
func TestAssignmentWatcher_PendingEntryFlushesOnClose(t *testing.T) {
	const window = 100 * time.Millisecond
	m := newTestManagerWithMetrics(t, newRecordingMetrics())
	m.cfg.AssignmentWatcherDebounce = window

	var processed atomic.Int64
	var lastVersion atomic.Int64
	m.testHookHandleAssignment = func(workerID string, e jetstream.KeyValueEntry) {
		processed.Add(1)
		lastVersion.Store(decodeVersion(e))
	}

	watcher := newFakeKeyWatcher()
	done := make(chan struct{})
	go func() {
		_ = m.runAssignmentWatchSession(m.ctx, nil, watcher, nil, "worker-1", "key")
		close(done)
	}()

	// Deliver a single entry and immediately close the channel before
	// the debounce window can fire.
	watcher.ch <- fakeEntryWithVersion(42)
	close(watcher.ch)

	<-done
	require.Equal(t, int64(1), processed.Load(), "pending entry must flush on channel close")
	require.Equal(t, int64(42), lastVersion.Load())
}
