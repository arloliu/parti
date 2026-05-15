package parti

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestMonitorCommitChanges_ChannelCloseTriggersBackoffAndRestart exercises
// the channel-close → backoff → re-watch loop. We install a watcher KV
// wrapper whose returned watcher has a forceClose() hook; calling it
// closes the Updates() channel mid-stream. monitorCommitChanges must
// observe the close as an error, restart via the backoff loop, and
// deliver a second commit through the re-established watcher.
func TestMonitorCommitChanges_ChannelCloseTriggersBackoffAndRestart(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "watcher-close-restart")

	m, rh, _, _ := newTestManager(t)
	m.assignmentKV = kv

	// Plant a commit so first-time replay always produces a watcher
	// event. Case (d) (worker not in commit) is the simplest shape that
	// successfully drives applyAssignment without payload setup.
	v10 := types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		Workers:        []string{"someone-else"},
		Payloads:       map[string]types.AssignmentPayloadRef{},
	}
	b10, _ := json.Marshal(v10)
	_, err := kv.Create(t.Context(), "assignment._commit", b10)
	require.NoError(t, err)

	wrap := &forceCloseWatcherKV{KeyValue: kv}
	done := make(chan struct{})
	go func() {
		m.monitorCommitChanges(m.ctx, wrap)
		close(done)
	}()

	// Wait until the first commit applies (proves watcher #1 is live).
	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 10
	}, 2*time.Second, 25*time.Millisecond, "first watcher must apply v=10")

	// Force-close watcher #1. The monitor loop must record a KV error
	// and re-Watch (watcher #2) after the backoff.
	wrap.forceCloseLatest()

	// Drop a second commit. Watcher #2's initial replay or subsequent
	// Put event must deliver it. We use a long Eventually because
	// watcherBaseBackoff (2s) + jitter must elapse before the restart.
	v20 := types.AssignmentCommit{
		Version:        20,
		LeaderRevision: 30,
		Workers:        []string{"someone-else"},
		Payloads:       map[string]types.AssignmentPayloadRef{},
	}
	b20, _ := json.Marshal(v20)
	_, err = kv.Put(t.Context(), "assignment._commit", b20)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 20
	}, 8*time.Second, 50*time.Millisecond,
		"restarted watcher must observe the second commit (backoff ~2s)")

	require.GreaterOrEqual(t, wrap.watchCallsLoaded(), int32(2),
		"monitorCommitChanges must re-Watch after channel close")
	require.GreaterOrEqual(t, rh.applyCount.Load(), int64(2),
		"both pre- and post-restart commits must apply")

	m.cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("monitorCommitChanges did not exit after ctx cancel")
	}
}

// forceCloseWatcherKV wraps a jetstream.KeyValue but returns a watcher
// whose Updates() channel can be force-closed by the test. The wrapper
// counts Watch() calls so we can assert the monitor re-watches after a
// channel close.
type forceCloseWatcherKV struct {
	jetstream.KeyValue

	mu         sync.Mutex
	watchCalls atomic.Int32
	latest     *forceCloseWatcher
	upstream   jetstream.KeyWatcher
}

func (f *forceCloseWatcherKV) Watch(ctx context.Context, key string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	up, err := f.KeyValue.Watch(ctx, key, opts...)
	if err != nil {
		return nil, err
	}
	w := newForceCloseWatcher(ctx, up)
	f.mu.Lock()
	f.latest = w
	f.upstream = up
	f.mu.Unlock()
	f.watchCalls.Add(1)

	return w, nil
}

func (f *forceCloseWatcherKV) forceCloseLatest() {
	f.mu.Lock()
	w := f.latest
	f.mu.Unlock()
	if w != nil {
		w.forceClose()
	}
}

func (f *forceCloseWatcherKV) watchCallsLoaded() int32 { return f.watchCalls.Load() }

// forceCloseWatcher proxies an upstream KeyWatcher's Updates() channel
// to a local channel that can be closed via forceClose(). The proxy
// goroutine owns the close of `out` (single writer) so we never
// double-close.
type forceCloseWatcher struct {
	upstream  jetstream.KeyWatcher
	out       chan jetstream.KeyValueEntry
	closeCh   chan struct{}
	closeOnce sync.Once
}

func newForceCloseWatcher(ctx context.Context, up jetstream.KeyWatcher) *forceCloseWatcher {
	w := &forceCloseWatcher{
		upstream: up,
		out:      make(chan jetstream.KeyValueEntry, 8),
		closeCh:  make(chan struct{}),
	}
	go w.proxy(ctx)
	return w
}

func (w *forceCloseWatcher) proxy(ctx context.Context) {
	// Single owner of close(out). Any of ctx/closeCh/upstream-close
	// drains us out of the loop, and we always close out on exit so
	// monitorCommitChanges observes the close as an error.
	defer close(w.out)
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.closeCh:
			return
		case e, ok := <-w.upstream.Updates():
			if !ok {
				return
			}
			select {
			case w.out <- e:
			case <-ctx.Done():
				return
			case <-w.closeCh:
				return
			}
		}
	}
}

func (w *forceCloseWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.out }

func (w *forceCloseWatcher) Stop() error {
	w.closeOnce.Do(func() { close(w.closeCh) })
	return w.upstream.Stop()
}

func (w *forceCloseWatcher) forceClose() {
	w.closeOnce.Do(func() {
		close(w.closeCh)
		// Stop the upstream watcher too so the test does not leak NATS
		// resources between cycles.
		_ = w.upstream.Stop()
	})
}

// TestMonitorCommitChanges_DeleteEventIgnored verifies the commit watcher
// treats a KeyValueDelete as a no-op: no panic, no apply, no LSR mutation.
func TestMonitorCommitChanges_DeleteEventIgnored(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "watcher-delete")

	m, rh, _, _ := newTestManager(t)
	m.assignmentKV = kv

	// Plant a real commit so the watcher sees a Put, then Delete it. The
	// initial Put must apply through case (d) (worker not in commit) so we
	// can verify state stays at v=10 after delete.
	commit := types.AssignmentCommit{
		Version:        10,
		LeaderRevision: 20,
		Workers:        []string{"someone-else"},
		Payloads:       map[string]types.AssignmentPayloadRef{},
	}
	bytes, err := json.Marshal(commit)
	require.NoError(t, err)
	_, err = kv.Create(t.Context(), "assignment._commit", bytes)
	require.NoError(t, err)

	// Spin up the watcher in the background. It'll see the initial value,
	// then we delete the key.
	go m.monitorCommitChanges(m.ctx, kv)
	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 10
	}, time.Second, 10*time.Millisecond, "watcher must process initial commit")

	require.NoError(t, kv.Delete(t.Context(), "assignment._commit"))
	// Watch a beat to ensure the delete is dispatched.
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int64(10), m.CurrentAssignment().Version, "delete must NOT regress the snapshot")

	require.GreaterOrEqual(t, rh.applyCount.Load(), int64(1), "initial Put applied once")
}

// TestMonitorCommitChanges_PeriodicReconcile_RecoversDivergence exercises
// the periodic reconcile ticker in watchCommit by running the real watcher
// against a watcher-stub that DROPS update events. The only path to
// convergence is then the reconcile tick — proving the periodic primitive
// (not the manually-invoked handleCommitValue).
//
// We dial commitReconcileInterval down to 1s via the package-level var
// (testable because manager_assignment.go declares it as var, not const,
// with an explicit "tests may override" contract). 1s gives us enough
// headroom to assert the "no recovery before first tick" invariant.
//
// NOTE: this test does NOT use t.Parallel() because it mutates the package
// global commitReconcileInterval (v2 review P2: avoid parallel races on a
// test-mutable global).
func TestMonitorCommitChanges_PeriodicReconcile_RecoversDivergence(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	kv := partitest.CreateJetStreamKV(t, nc, "watcher-reconcile")

	prev := commitReconcileInterval
	commitReconcileInterval = 1 * time.Second
	t.Cleanup(func() { commitReconcileInterval = prev })

	m, _, _, _ := newTestManager(t)
	m.assignmentKV = kv

	// Plant initial v=5; route through state machine so snapshot reflects
	// the value the reconcile loop will idempotently re-route.
	v5 := types.AssignmentCommit{
		Version:        5,
		LeaderRevision: 10,
		Workers:        []string{"someone-else"},
		Payloads:       map[string]types.AssignmentPayloadRef{},
	}
	bytes5, _ := json.Marshal(v5)
	_, err := kv.Create(t.Context(), "assignment._commit", bytes5)
	require.NoError(t, err)
	m.handleCommitValue(&v5)
	require.Equal(t, int64(5), m.CurrentAssignment().Version)

	// Wrap the KV with a watcher that DROPS all watcher events. The only
	// path the manager has to observe an update is the reconcile ticker's
	// idempotent KV re-read.
	dropWatcher := &droppingWatcherKV{KeyValue: kv}
	go m.monitorCommitChanges(m.ctx, dropWatcher)

	// Update KV out-of-band to v=15 to mimic a watcher missing the event.
	v15 := types.AssignmentCommit{
		Version:        15,
		LeaderRevision: 30,
		Workers:        []string{"someone-else"},
		Payloads:       map[string]types.AssignmentPayloadRef{},
	}
	bytes15, _ := json.Marshal(v15)
	_, err = kv.Put(t.Context(), "assignment._commit", bytes15)
	require.NoError(t, err)

	// Pre-tick assertion: well before the 1s reconcile tick, the watcher
	// is dropping events so the snapshot MUST still be V=5. This proves
	// the test is actually exercising the reconcile path (and not, say,
	// the watcher-replay path leaking through droppingWatcherKV).
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, int64(5), m.CurrentAssignment().Version,
		"snapshot MUST NOT recover before the first reconcile tick — the dropping watcher is the only event source")

	// Now wait past the 1s tick for the reconcile-driven recovery.
	require.Eventually(t, func() bool {
		return m.CurrentAssignment().Version == 15
	}, 4*time.Second, 25*time.Millisecond,
		"watcher-dropped event must be recovered by the periodic reconcile tick")
}

// droppingWatcherKV wraps a jetstream.KeyValue but replaces the watcher
// with one that NEVER delivers updates. The KV-Get path used by the
// reconcile ticker is unaffected, so the test isolates the reconcile
// path from the watcher.
type droppingWatcherKV struct {
	jetstream.KeyValue
}

func (d *droppingWatcherKV) Watch(_ context.Context, _ string, _ ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return &droppingWatcher{updates: make(chan jetstream.KeyValueEntry)}, nil
}

type droppingWatcher struct {
	updates   chan jetstream.KeyValueEntry
	closeOnce sync.Once
}

func (d *droppingWatcher) Updates() <-chan jetstream.KeyValueEntry { return d.updates }
func (d *droppingWatcher) Stop() error {
	d.closeOnce.Do(func() { close(d.updates) })
	return nil
}
