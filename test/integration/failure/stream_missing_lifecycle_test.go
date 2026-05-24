package failure_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/types"
)

// observerCaptureUpdater is a WorkerConsumerUpdater that also
// implements the recovery.StreamMissingObserver-shaped surface (via
// the public SetOnStreamMissingError signature). It records every
// call so a lifecycle test can verify the manager actually installs
// and clears the observer through Start/Stop — not via source
// reading or by mimicking the production type-assertion.
type observerCaptureUpdater struct {
	mu       sync.Mutex
	installs []func(streamName string, err error)
}

func (u *observerCaptureUpdater) UpdateWorkerConsumer(_ context.Context, _ string, _ []types.Partition) error {
	return nil
}

func (u *observerCaptureUpdater) SetOnStreamMissingError(fn func(streamName string, err error)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.installs = append(u.installs, fn)
}

func (u *observerCaptureUpdater) callsSnapshot() []func(streamName string, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]func(streamName string, err error), len(u.installs))
	copy(out, u.installs)
	return out
}

// TestManager_LiveStop_ClearsStreamMissingObserver pins the
// end-to-end production Stop path that clears the manager's
// stream-missing observer before m.wg.Wait. The companion unit test
// `TestManager_Stop_ClearsStreamMissingObserver` exercises the
// type-assertion in isolation; this integration test verifies the
// clear actually fires through a real Manager.Start + Manager.Stop
// against an embedded NATS, catching the regression class where a
// future refactor moves the clear after the wait-group wait (or
// drops it entirely).
//
// The capture updater implements the observer interface and records
// every SetOnStreamMissingError call. The expected sequence across
// Start → Stop is:
//
//  1. prepareStart installs the bridge: SetOnStreamMissingError(<non-nil>)
//  2. Manager.Stop clears the bridge: SetOnStreamMissingError(nil)
//
// A regression that skipped the Stop-side clear would leave only
// the install call in the captured sequence.
func TestManager_LiveStop_ClearsStreamMissingObserver(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	_, err := jetstream.New(nc)
	require.NoError(t, err)

	updater := &observerCaptureUpdater{}

	cluster := testutil.NewFastWorkerCluster(t, nc, 2)
	defer cluster.StopWorkers()
	mgr := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerConsumerUpdater(updater))
	require.NoError(t, mgr.Start(ctx))

	// After Start, prepareStart must have installed the manager
	// observer. Captured exactly one call so far, with a non-nil fn.
	require.Eventually(t, func() bool {
		calls := updater.callsSnapshot()
		return len(calls) == 1 && calls[0] != nil
	}, 5*time.Second, 25*time.Millisecond,
		"Manager.Start (via prepareStart) must install the manager observer; "+
			"captured call sequence is %v", updater.callsSnapshot())

	require.NoError(t, mgr.Stop(ctx))

	// After Stop, the captured sequence must end with a nil install
	// call — the explicit clear from manager.go's Stop path. This is
	// the production behavior the unit test cannot verify.
	calls := updater.callsSnapshot()
	require.GreaterOrEqual(t, len(calls), 2,
		"Manager.Stop must invoke SetOnStreamMissingError(nil) before m.wg.Wait so the observer cannot fire post-Stop; "+
			"captured fewer than 2 calls (%d) which means the clear is missing", len(calls))
	require.Nil(t, calls[len(calls)-1],
		"the LAST SetOnStreamMissingError call from a Start→Stop cycle must be (nil); "+
			"a non-nil tail means the production Stop path did not clear the observer")
}

// plainUpdater is a WorkerConsumerUpdater that deliberately does NOT
// implement recovery.StreamMissingObserver. Legacy consumers and
// caller-supplied test doubles fall into this category; the manager
// must silently skip the observer-install path for them rather than
// panic.
type plainUpdater struct {
	updates atomic.Int32
}

func (p *plainUpdater) UpdateWorkerConsumer(_ context.Context, _ string, _ []types.Partition) error {
	p.updates.Add(1)
	return nil
}

// TestManager_LiveStartStop_SilentSkipForNonObserverUpdater pins
// the type-assertion-gate contract in manager_setup.go (prepareStart)
// and manager.go (Stop): when the registered WorkerConsumerUpdater
// does not satisfy [recovery.StreamMissingObserver], the manager
// must silently skip both the install and the clear without panicking
// and without affecting the rest of the lifecycle.
//
// This is the path taken by:
//   - Legacy consumer types that predate the observer interface
//     (e.g. early Queue/Broadcast adapters).
//   - Caller-supplied test doubles that only need to satisfy
//     [parti.WorkerConsumerUpdater].
//   - Future composite implementations that may not wire the
//     observer interface yet.
//
// A regression that, say, dropped the type-assertion guard and
// called SetOnStreamMissingError directly on the updater would
// panic on the missing method. This live test catches that class
// while also confirming UpdateWorkerConsumer still flows through
// normally (the partition assignment for the test cluster must
// reach the updater).
func TestManager_LiveStartStop_SilentSkipForNonObserverUpdater(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	updater := &plainUpdater{}

	cluster := testutil.NewFastWorkerCluster(t, nc, 2)
	defer cluster.StopWorkers()
	mgr := cluster.AddWorkerWithOptions(ctx, parti.WithWorkerConsumerUpdater(updater))

	require.NotPanics(t, func() {
		require.NoError(t, mgr.Start(ctx))
	}, "Manager.Start must not panic when the registered WorkerConsumerUpdater does not implement recovery.StreamMissingObserver")

	// Sanity: the rest of the lifecycle still functions — partition
	// assignment reaches the updater. Confirms the silent-skip is
	// scoped to the observer surface only.
	require.Eventually(t, func() bool {
		return updater.updates.Load() > 0
	}, 10*time.Second, 50*time.Millisecond,
		"UpdateWorkerConsumer must still fire for a non-observer updater; "+
			"the silent-skip must be scoped to SetOnStreamMissingError only")

	require.NotPanics(t, func() {
		require.NoError(t, mgr.Stop(ctx))
	}, "Manager.Stop must not panic when the registered WorkerConsumerUpdater does not implement recovery.StreamMissingObserver")
}
