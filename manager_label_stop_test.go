package parti_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// rebalanceGateLogger implements parti.Logger and, once armed, blocks the
// FIRST calculator "partitions retrieved" Debug line (emitted mid-rebalance,
// after the fresh worker enumeration and before the label-read phase) until
// released. It lets the test park a real partition-lifecycle rebalance — on
// the calculator-owned monitorPartitions goroutine that calc.Stop joins —
// at exactly the point where a concurrent Manager.Stop closes the
// calculator's stop channel, so the resumed label reads abort broadly.
type rebalanceGateLogger struct {
	armed   atomic.Bool
	once    sync.Once
	entered chan struct{} // closed when the gated rebalance reaches the gate
	release chan struct{} // test closes to resume the rebalance
}

func newRebalanceGateLogger() *rebalanceGateLogger {
	return &rebalanceGateLogger{entered: make(chan struct{}), release: make(chan struct{})}
}

func (l *rebalanceGateLogger) Debug(msg string, _ ...any) {
	if msg == "partitions retrieved" && l.armed.Load() {
		l.once.Do(func() { close(l.entered) })
		<-l.release
	}
}
func (l *rebalanceGateLogger) Info(string, ...any)  {}
func (l *rebalanceGateLogger) Warn(string, ...any)  {}
func (l *rebalanceGateLogger) Error(string, ...any) {}
func (l *rebalanceGateLogger) Fatal(string, ...any) {}

// TestManagerStop_NoDeadlock_LabelReadFailureDuringStop reproduces the Task 15
// shutdown deadlock: stopCalculator held m.mu across calc.Stop(), whose joins
// wait for the in-flight rebalance; Stop's stop-channel close makes that
// rebalance's label reads abort broadly, so it re-enters the manager via
// OnLabelReadBroadFailure → recordLabelReadFailure → recordKVError →
// m.mu.Lock() — a circular wait that hung Manager.Stop forever.
//
// Staging (near-deterministic): a gate logger parks a partition-lifecycle
// rebalance right after the fresh worker enumeration; Manager.Stop is then
// started and given time to reach calc.Stop (closing the calculator's stopCh)
// while blocked joining the parked goroutine; the gate is released and the
// resumed label reads abort into the callback. On the unfixed code Stop never
// returns (bounded wait fails the test); fixed, it completes promptly.
//
// Deliberately NO t.Cleanup(mgr.Stop): on the unfixed code a second Stop would
// deadlock the cleanup phase too and convert the failure into a package
// timeout. The single in-test Stop is the assertion.
func TestManagerStop_NoDeadlock_LabelReadFailureDuringStop(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	srcKV := partitest.CreateJetStreamKV(t, nc, "lbl-stop-deadlock-partitions")
	src := source.NewNatsKV(srcKV, "partitions", nil)
	require.NoError(t, src.Update(ctx, []types.Partition{
		{Keys: []string{"p0"}},
		{Keys: []string{"p1"}},
	}))

	gate := newRebalanceGateLogger()
	cfg := testutil.IntegrationTestConfig()

	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(), parti.WithLogger(gate))
	require.NoError(t, err)

	require.NoError(t, mgr.Start(ctx))
	require.NoError(t, <-mgr.WaitState(types.StateStable, 20*time.Second))

	// Arm the gate, then trigger a partition-lifecycle rebalance via a real
	// source change; the watcher drives it on the calculator's
	// monitorPartitions goroutine, which calc.Stop joins.
	gate.armed.Store(true)
	require.NoError(t, src.Update(ctx, []types.Partition{
		{Keys: []string{"p0"}},
		{Keys: []string{"p1"}},
		{Keys: []string{"p2"}},
	}))

	select {
	case <-gate.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the source-change rebalance never reached the gate")
	}

	// Stop while the rebalance is parked mid-flight.
	stopDone := make(chan error, 1)
	go func() { stopDone <- mgr.Stop(context.Background()) }()

	// Wait for Stop to be underway (it transitions to Shutdown synchronously
	// at entry), then give it ample time to reach calc.Stop and close the
	// calculator's stop channel while it blocks joining the parked goroutine.
	require.Eventually(t, func() bool { return mgr.State() == types.StateShutdown },
		10*time.Second, 10*time.Millisecond, "Stop must transition to Shutdown")
	time.Sleep(1 * time.Second)

	// Resume the rebalance: its label reads now abort broadly (stop-cancelled
	// context) and fire the manager's broad-failure callback.
	close(gate.release)

	select {
	case err := <-stopDone:
		require.NoError(t, err, "Manager.Stop must complete cleanly")
	case <-time.After(20 * time.Second):
		t.Fatal("Manager.Stop deadlocked: stopCalculator held m.mu across calc.Stop while the " +
			"in-flight rebalance's broad label-read failure re-entered recordKVError → m.mu.Lock")
	}
}
