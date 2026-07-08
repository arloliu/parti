package assignment

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// hbValue marshals a v1 JSON heartbeat carrying the given labels so the fakeKV
// watcher harness delivers real, decodeable payloads (fakeKVEntry.Value()). The
// heartbeat's WorkerID is irrelevant to the label fingerprint (only Labels are
// hashed), so it is fixed.
func hbValue(t *testing.T, labels []string) []byte {
	t.Helper()
	data, err := json.Marshal(types.Heartbeat{
		WorkerID:      "w0",
		SchemaVersion: 1,
		Capabilities:  types.CapAckV1,
		Labels:        labels,
		Timestamp:     time.Unix(1000, 0).UTC(),
	})
	require.NoError(t, err)

	return data
}

// labelChangeCounters bundles the two callback counters a label-change monitor
// test observes (keeps newLabelChangeMonitor within the result-count limit).
type labelChangeCounters struct {
	onChange atomic.Int32 // worker-change (onChange) callback invocations
	onLabel  atomic.Int32 // onLabelChange callback invocations
}

// newLabelChangeMonitor builds a monitor wired to a fake clock, an onChange
// (worker-change) counter, and an onLabelChange counter, suitable for driving
// processWatcherEvents directly through the fakeKV harness. hbTTL is fixed at
// 10s — long enough that every same-clock refresh in these tests classifies as
// suppressible, so the label-change escape is what any trigger proves.
func newLabelChangeMonitor(clock *fakeClock) (*WorkerMonitor, *fakeKV, *labelChangeCounters) {
	kv := &fakeKV{}
	c := &labelChangeCounters{}
	m := NewWorkerMonitor(kv, "worker", 10*time.Second,
		func(ctx context.Context) error { c.onChange.Add(1); return nil },
		logging.NewNop(),
	)
	m.now = clock.Now
	m.SetOnLabelChange(func() { c.onLabel.Add(1) })

	return m, kv, c
}

// pushToCurrentWatcher delivers an entry to the fakeKV's active watcher session
// (used across watcher-restart boundaries where the session handle rotates).
func pushToCurrentWatcher(t *testing.T, kv *fakeKV, e jetstream.KeyValueEntry) {
	t.Helper()
	kv.mu.Lock()
	w := kv.currentWatcher
	kv.mu.Unlock()
	require.NotNil(t, w)
	select {
	case w.updates <- e:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher loop did not consume entry")
	}
}

// TestWorkerMonitor_LabelChangeEscapesSuppression: a label change on a live,
// continuously-refreshing key is never a suppressible refresh — it fires
// onLabelChange AND forces a worker-change check even though the same key's
// same-label refreshes are suppressed.
func TestWorkerMonitor_LabelChangeEscapesSuppression(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))
	m, kv, c := newLabelChangeMonitor(clock)
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	// Join with ["vip"] — first-seen seeds the fingerprint silently.
	s.push(&fakeKVEntry{key: "worker.w0", op: jetstream.KeyValuePut, value: hbValue(t, []string{"vip"})})
	waitDebounce()
	require.EqualValues(t, 1, c.onChange.Load(), "join must trigger one worker-change check")
	require.EqualValues(t, 0, c.onLabel.Load(), "first-seen key must seed the fingerprint silently")

	// A same-label refresh within hbTTL is suppressed and does not re-fire.
	clock.Advance(200 * time.Millisecond)
	s.push(&fakeKVEntry{key: "worker.w0", op: jetstream.KeyValuePut, value: hbValue(t, []string{"vip"})})
	waitDebounce()
	require.EqualValues(t, 1, c.onChange.Load(), "same-label refresh must stay suppressed (no worker-change check)")
	require.EqualValues(t, 0, c.onLabel.Load(), "unchanged labels must not fire onLabelChange")

	// Flip labels on the SAME live key: the change escapes suppression.
	clock.Advance(200 * time.Millisecond)
	s.push(&fakeKVEntry{key: "worker.w0", op: jetstream.KeyValuePut, value: hbValue(t, []string{"batch"})})
	waitDebounce()
	require.EqualValues(t, 1, c.onLabel.Load(), "a label change must fire onLabelChange exactly once")
	require.EqualValues(t, 2, c.onChange.Load(), "a label change must escape suppression and force a worker-change check")

	// A subsequent identical-labels beat does NOT re-fire (fingerprint updated).
	clock.Advance(200 * time.Millisecond)
	s.push(&fakeKVEntry{key: "worker.w0", op: jetstream.KeyValuePut, value: hbValue(t, []string{"batch"})})
	waitDebounce()
	require.EqualValues(t, 1, c.onLabel.Load(), "identical labels after a change must not re-fire onLabelChange")
	require.EqualValues(t, 2, c.onChange.Load(), "identical-labels refresh must be suppressed again")
}

// TestWorkerMonitor_LabelChangeAcrossWatcherRestart pins the monitor-lifetime
// fingerprint map: a takeover PUT that lands while the watch is down is caught
// when the next session's initial replay delivers the key's current value and
// it differs from the retained fingerprint (spec §9 watcher-restart edge).
func TestWorkerMonitor_LabelChangeAcrossWatcherRestart(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))

	kv := &fakeKV{}
	var onChange, onLabel atomic.Int32
	m := NewWorkerMonitor(kv, "worker", 10*time.Second,
		func(ctx context.Context) error { onChange.Add(1); return nil },
		logging.NewNop(),
	)
	m.now = clock.Now
	m.watchBaseBackoff = 10 * time.Millisecond // fast retry, matches sibling restart tests
	m.SetOnLabelChange(func() { onLabel.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.monitorWatcherWithRetry(ctx)
	require.Eventually(t, func() bool { return kv.WatchCallCount() >= 1 }, time.Second, time.Millisecond)

	// Session 1: worker-0 with ["vip"] seeds the fingerprint silently.
	pushToCurrentWatcher(t, kv, &fakeKVEntry{key: "worker.w0", op: jetstream.KeyValuePut, value: hbValue(t, []string{"vip"})})
	waitDebounce()
	require.EqualValues(t, 0, onLabel.Load(), "first-seen must seed the fingerprint silently")

	// Kill the session; the retry loop starts a fresh one. While the watch is
	// down a takeover swaps the labels behind the same live worker ID.
	kv.CloseUpdates()
	require.Eventually(t, func() bool { return kv.WatchCallCount() >= 2 }, 2*time.Second, 10*time.Millisecond)

	// Session 2's initial replay delivers the current value ["batch"]; the
	// MONITOR-lifetime fingerprint (retained from session 1) differs → fires once.
	pushToCurrentWatcher(t, kv, &fakeKVEntry{key: "worker.w0", op: jetstream.KeyValuePut, value: hbValue(t, []string{"batch"})})
	waitDebounce()
	require.EqualValues(t, 1, onLabel.Load(), "a label change across a watcher restart must fire exactly once")
}

// TestWorkerMonitor_MalformedPayloadNoFingerprintChurn: a malformed heartbeat
// PUT neither fires onLabelChange nor erases the retained fingerprint (the next
// well-formed identical-labels beat stays quiet, and a genuinely different beat
// still fires against the surviving fingerprint).
func TestWorkerMonitor_MalformedPayloadNoFingerprintChurn(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))
	m, kv, c := newLabelChangeMonitor(clock)
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	// Seed ["vip"].
	s.push(&fakeKVEntry{key: "worker.w0", op: jetstream.KeyValuePut, value: hbValue(t, []string{"vip"})})
	waitDebounce()
	require.EqualValues(t, 0, c.onLabel.Load())

	// A malformed payload must not fire onLabelChange.
	clock.Advance(200 * time.Millisecond)
	s.push(&fakeKVEntry{key: "worker.w0", op: jetstream.KeyValuePut, value: []byte("not-a-heartbeat")})
	waitDebounce()
	require.EqualValues(t, 0, c.onLabel.Load(), "malformed payload must not fire onLabelChange")

	// The retained fingerprint survived: an identical well-formed beat stays quiet.
	clock.Advance(200 * time.Millisecond)
	s.push(&fakeKVEntry{key: "worker.w0", op: jetstream.KeyValuePut, value: hbValue(t, []string{"vip"})})
	waitDebounce()
	require.EqualValues(t, 0, c.onLabel.Load(), "identical labels after a malformed beat must stay quiet")

	// ...and a genuinely different beat still fires — proving the malformed beat
	// did not erase the retained ["vip"] fingerprint.
	clock.Advance(200 * time.Millisecond)
	s.push(&fakeKVEntry{key: "worker.w0", op: jetstream.KeyValuePut, value: hbValue(t, []string{"batch"})})
	waitDebounce()
	require.EqualValues(t, 1, c.onLabel.Load(), "a real label change after a malformed beat must still fire")
}

// TestWorkerMonitor_FirstSeenSeedsSilently: a brand-new worker key with labels
// never fires onLabelChange — joins are the worker-change path's job.
func TestWorkerMonitor_FirstSeenSeedsSilently(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))
	m, kv, c := newLabelChangeMonitor(clock)
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	s.push(&fakeKVEntry{key: "worker.w0", op: jetstream.KeyValuePut, value: hbValue(t, []string{"vip"})})
	waitDebounce()
	require.EqualValues(t, 1, c.onChange.Load(), "a new worker key must trigger the worker-change (join) path")
	require.EqualValues(t, 0, c.onLabel.Load(), "first-seen key must never fire onLabelChange")
}

// TestWorkerMonitor_DeleteDropsFingerprint: a DELETE drops the key's retained
// fingerprint, so a rejoin with different labels is a first-seen join (silent),
// not a spurious label change — the join/leave key-set delta drives the
// worker-change rebalance instead.
func TestWorkerMonitor_DeleteDropsFingerprint(t *testing.T) {
	t.Parallel()
	clock := newFakeClockAt(time.Unix(1000, 0))
	m, kv, c := newLabelChangeMonitor(clock)
	s := startWatcherSession(t, m, kv)
	defer s.stop()

	// Seed ["vip"].
	s.push(&fakeKVEntry{key: "worker.w0", op: jetstream.KeyValuePut, value: hbValue(t, []string{"vip"})})
	waitDebounce()
	require.EqualValues(t, 0, c.onLabel.Load())

	// Graceful leave drops the fingerprint.
	clock.Advance(200 * time.Millisecond)
	s.push(&fakeKVEntry{key: "worker.w0", op: jetstream.KeyValueDelete})
	waitDebounce()

	// Rejoin with DIFFERENT labels: first-seen again → seeds silently, does not
	// fire onLabelChange (would fire if the DELETE had left the stale fingerprint).
	clock.Advance(200 * time.Millisecond)
	s.push(&fakeKVEntry{key: "worker.w0", op: jetstream.KeyValuePut, value: hbValue(t, []string{"batch"})})
	waitDebounce()
	require.EqualValues(t, 0, c.onLabel.Load(), "rejoin after DELETE must seed silently, not fire onLabelChange")
}
