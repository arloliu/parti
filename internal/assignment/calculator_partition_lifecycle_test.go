package assignment

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// blockingAssignmentKV wraps a jetstream.KeyValue and blocks Create/Update
// on the `_commit` key until release is signalled OR ctx is cancelled.
// Per spec §4.1, the wrapper is CTX-AWARE: when ctx fires while blocked,
// it returns ctx.Err() WITHOUT forwarding to the underlying KV. This
// mirrors well-behaved jetstream.KeyValue semantics for ctx-cancelled
// requests and makes the strict §7.3 assertion deterministic.
type blockingAssignmentKV struct {
	jetstream.KeyValue
	commitKey string // full key name we trap, e.g. "assignment._commit"

	releaseCh chan struct{} // closed by Release()
	attempts  chan struct{} // signalled once per intercepted call entering the block
	returns   chan struct{} // signalled once per intercepted call returning

	forwarded atomic.Int64

	mu      sync.Mutex
	blocked bool // when true, intercept commit-key Create/Update
}

func newBlockingAssignmentKV(realKV jetstream.KeyValue, commitKey string) *blockingAssignmentKV {
	return &blockingAssignmentKV{
		KeyValue:  realKV,
		commitKey: commitKey,
		releaseCh: make(chan struct{}),
		attempts:  make(chan struct{}, 16),
		returns:   make(chan struct{}, 16),
	}
}

func (b *blockingAssignmentKV) BlockOnCommitCAS() {
	b.mu.Lock()
	b.blocked = true
	b.mu.Unlock()
}

func (b *blockingAssignmentKV) Release() {
	select {
	case <-b.releaseCh:
		// already released
	default:
		close(b.releaseCh)
	}
}

// CommitAttemptChan fires once per blocked CAS entry.
func (b *blockingAssignmentKV) CommitAttemptChan() <-chan struct{} {
	return b.attempts
}

// CommitReturnedChan fires once per blocked CAS goroutine return.
func (b *blockingAssignmentKV) CommitReturnedChan() <-chan struct{} {
	return b.returns
}

// CommitsForwarded reports the number of CAS attempts that were actually
// forwarded to the underlying KV (i.e., NOT returned via ctx cancellation).
func (b *blockingAssignmentKV) CommitsForwarded() int64 {
	return b.forwarded.Load()
}

// waitForRelease returns (intercepted, err): intercepted is true only when
// the call actually entered the blocked path (signalled "attempts" and
// will signal "returns" on exit). err is ctx.Err() if cancellation won
// the race, nil otherwise. Callers use intercepted to decide whether to
// emit the returns signal.
func (b *blockingAssignmentKV) waitForRelease(ctx context.Context, key string) (bool, error) {
	b.mu.Lock()
	intercept := b.blocked && key == b.commitKey
	b.mu.Unlock()
	if !intercept {
		return false, nil
	}
	// Signal we're about to block.
	select {
	case b.attempts <- struct{}{}:
	default:
	}
	// Block on either ctx cancellation or release.
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case <-b.releaseCh:
	}
	// PR1-V3-003 select-fairness clause: re-check ctx after the select.
	// If both were ready, prefer ctx error semantics so the test can
	// deterministically assert "no forwarding after Stop".
	return true, ctx.Err()
}

func (b *blockingAssignmentKV) signalReturned() {
	select {
	case b.returns <- struct{}{}:
	default:
	}
}

func (b *blockingAssignmentKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	intercepted, err := b.waitForRelease(ctx, key)
	if intercepted {
		defer b.signalReturned()
	}
	if err != nil {
		return 0, err
	}
	if key == b.commitKey {
		// Count forwarded attempts BEFORE issuing the call, so a forwarded
		// CAS that fails (ctx cancellation mid-flight, CAS-lost, network)
		// still counts as "forwarded to the underlying KV". This is the
		// invariant Test 7.3 asserts on.
		b.forwarded.Add(1)
	}

	return b.KeyValue.Create(ctx, key, value, opts...)
}

func (b *blockingAssignmentKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	intercepted, err := b.waitForRelease(ctx, key)
	if intercepted {
		defer b.signalReturned()
	}
	if err != nil {
		return 0, err
	}
	if key == b.commitKey {
		b.forwarded.Add(1)
	}

	return b.KeyValue.Update(ctx, key, value, revision)
}

// partitionLifecycleFixture bundles the constructed test surfaces so the
// helper stays under revive's function-result-limit.
type partitionLifecycleFixture struct {
	calc    *Calculator
	source  *mockWatchableSource
	wrapped *blockingAssignmentKV
	realKV  jetstream.KeyValue
}

// buildPartitionLifecycleCalc constructs a Calculator with a watchable
// source and a blocking commit-CAS wrapper. The returned wrapper allows
// the test to intercept the _commit CAS site.
func buildPartitionLifecycleCalc(t *testing.T, busName string, stateProvider types.StateProvider, drainInterval time.Duration) *partitionLifecycleFixture {
	t.Helper()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	realAssign := partitest.CreateJetStreamKV(t, nc, busName+"-asgn")
	heartbeat := partitest.CreateJetStreamKV(t, nc, busName+"-hb")

	// Heartbeat for one worker so rebalance has something to assign.
	_, err := heartbeat.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	wrapped := newBlockingAssignmentKV(realAssign, "assignment._commit")

	source := &mockWatchableSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		},
	}
	strategy := &mockStrategy{}

	cfg := &Config{
		AssignmentKV:                wrapped,
		HeartbeatKV:                 heartbeat,
		AssignmentPrefix:            "assignment",
		HeartbeatPrefix:             "worker-hb",
		HeartbeatTTL:                2 * time.Second,
		Source:                      source,
		Strategy:                    strategy,
		EmergencyGracePeriod:        5 * time.Second,
		Cooldown:                    1 * time.Millisecond,
		ColdStartWindow:             20 * time.Millisecond,
		PlannedScaleWindow:          20 * time.Millisecond,
		RebalanceGraceDrainInterval: drainInterval,
		StateProvider:               stateProvider,
	}
	calc, err := NewCalculator(cfg)
	require.NoError(t, err)

	return &partitionLifecycleFixture{
		calc:    calc,
		source:  source,
		wrapped: wrapped,
		realKV:  realAssign,
	}
}

// TestCalculator_Stop_BoundedByPartitionRebalance verifies ISSUE-002:
// graceful shutdown is no longer bounded by the detached 30s timeout
// inside monitorPartitions. With the stop-aware ctx wired by ctxFromStopCh,
// Stop cancels the in-flight partition rebalance and returns promptly.
func TestCalculator_Stop_BoundedByPartitionRebalance(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	f := buildPartitionLifecycleCalc(t, "test-stop-bounded", nil, 5*time.Second)
	calc, source, wrapped := f.calc, f.source, f.wrapped

	require.NoError(t, calc.Start(ctx))

	// Allow the initial cold-start rebalance to land (it goes through the
	// state-machine path, not the partition-update path).
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() >= 1 && calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond, "calc should reach Idle after initial rebalance")

	// Now arm the wrapper to block on the NEXT commit CAS.
	wrapped.BlockOnCommitCAS()

	// Fire a partition-source update — this is the path under test
	// (monitorPartitions, not the state machine).
	source.Update([]types.Partition{
		{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
	})

	// Wait until the partition-triggered rebalance reaches the blocked CAS.
	select {
	case <-wrapped.CommitAttemptChan():
	case <-time.After(2 * time.Second):
		t.Fatal("partition rebalance did not reach commit CAS")
	}

	// Now Stop must return promptly — well under the 30s detached timeout.
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	startStop := time.Now()
	err := calc.Stop(stopCtx)
	elapsed := time.Since(startStop)

	require.NoError(t, err, "Stop should return cleanly")
	require.Less(t, elapsed, 2*time.Second, "Stop must not be bounded by the detached 30s timeout")

	// Defensive release; the blocked goroutine has already returned via ctx.
	wrapped.Release()
}

// TestCalculator_Stop_PreventsStaleCommitAfterStop verifies ISSUE-003:
// no assignment commit lands in KV after Stop has begun closing channels.
// The strict invariant is observable in test thanks to the ctx-aware
// blocking wrapper.
func TestCalculator_Stop_PreventsStaleCommitAfterStop(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	f := buildPartitionLifecycleCalc(t, "test-stop-no-stale", nil, 5*time.Second)
	calc, source, wrapped, realKV := f.calc, f.source, f.wrapped, f.realKV

	require.NoError(t, calc.Start(ctx))

	// Allow initial rebalance.
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() >= 1 && calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond, "calc should reach Idle after initial rebalance")

	preStopVersion := calc.CurrentVersion()
	preStopForwarded := wrapped.CommitsForwarded()

	// Capture the real KV `_commit` revision so we can assert it doesn't
	// advance even if some other commit-CAS path attempts a forward.
	preCommitEntry, err := realKV.Get(ctx, "assignment._commit")
	require.NoError(t, err, "initial _commit should exist after first rebalance")
	preCommitRev := preCommitEntry.Revision()

	wrapped.BlockOnCommitCAS()

	source.Update([]types.Partition{
		{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
	})

	// Wait for the blocked CAS attempt.
	select {
	case <-wrapped.CommitAttemptChan():
	case <-time.After(2 * time.Second):
		t.Fatal("partition rebalance did not reach commit CAS")
	}

	// Stop concurrently — closes stopCh, cancels the wrapped ctx.
	stopDone := make(chan struct{})
	go func() {
		_ = calc.Stop(ctx)
		close(stopDone)
	}()

	// The blocked goroutine should observe ctx.Done() and return ctx.Err()
	// WITHOUT forwarding the CAS.
	select {
	case <-wrapped.CommitReturnedChan():
	case <-time.After(3 * time.Second):
		t.Fatal("blocked CAS goroutine did not return after Stop")
	}

	// Defensive release; the blocked goroutine has already returned.
	wrapped.Release()

	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return after Release")
	}

	require.Equal(t, preStopForwarded, wrapped.CommitsForwarded(),
		"no commit CAS should have been forwarded after Stop")
	require.Equal(t, preStopVersion, calc.CurrentVersion(),
		"publisher CurrentVersion must not advance after Stop")

	// Strict §7.3 assertion: the real `_commit` KV revision must be
	// unchanged from the pre-stop snapshot. This is the load-bearing
	// invariant — if a CAS landed despite Stop, this revision advances.
	postCommitEntry, err := realKV.Get(ctx, "assignment._commit")
	require.NoError(t, err)
	require.Equal(t, preCommitRev, postCommitEntry.Revision(),
		"_commit KV revision must not advance after Stop")
}

// TestCalculator_PartitionUpdate_HonoursRecoveryGrace verifies ISSUE-005:
// a partition-source change observed during recovery grace is deferred,
// not dropped. Two phases:
//
//	Phase A — while grace is true, an update fires but no rebalance lands.
//	Phase B — when grace lifts, the drain ticker publishes the deferred update.
func TestCalculator_PartitionUpdate_HonoursRecoveryGrace(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	stateProvider := &mockStateProvider{}

	f := buildPartitionLifecycleCalc(t, "test-grace-defer", stateProvider, 25*time.Millisecond)
	calc, source, wrapped := f.calc, f.source, f.wrapped

	require.NoError(t, calc.Start(ctx))

	require.Eventually(t, func() bool {
		return calc.CurrentVersion() >= 1 && calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond, "calc should reach Idle after initial rebalance")

	// Enter recovery grace AFTER the initial rebalance has landed so the
	// initial commit is in steady state.
	stateProvider.SetGrace(true)

	versionAtGraceEntry := calc.CurrentVersion()

	// Fire a partition change while in grace.
	source.Update([]types.Partition{
		{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
	})

	// Phase A — while grace is true, NO commit should be forwarded for
	// at least one drain interval. We wait through multiple drain ticks
	// to prove deferral.
	require.Never(t, func() bool {
		return calc.CurrentVersion() > versionAtGraceEntry
	}, 200*time.Millisecond, 25*time.Millisecond,
		"rebalance must not fire while leader is in recovery grace")

	// Phase B — lift grace; the drain ticker should pick up the deferred
	// update and publish a new version.
	stateProvider.SetGrace(false)

	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > versionAtGraceEntry
	}, 2*time.Second, 10*time.Millisecond,
		"deferred partition update must be drained after grace lifts")

	_ = wrapped // keep var live; not used for assertions in this test
	_ = calc.Stop(ctx)
}

// TestCtxFromStopCh_Lifecycle verifies the ctxFromStopCh helper contract:
//   - cancellation from either parent ctx or stopCh terminates the returned ctx
//   - the helper goroutine exits cleanly in all cases
//   - calling cancel() is idempotent and does not panic
func TestCtxFromStopCh_Lifecycle(t *testing.T) {
	t.Parallel()

	t.Run("stopCh_closes_first", func(t *testing.T) {
		t.Parallel()
		stopCh := make(chan struct{})
		ctx, cancel := ctxFromStopCh(context.Background(), stopCh, 0)
		defer cancel()
		close(stopCh)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("ctx did not cancel after stopCh closed")
		}
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	})

	t.Run("parent_cancels_first", func(t *testing.T) {
		t.Parallel()
		stopCh := make(chan struct{})
		parent, parentCancel := context.WithCancel(context.Background())
		ctx, cancel := ctxFromStopCh(parent, stopCh, 0)
		defer cancel()
		parentCancel()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("ctx did not cancel after parent cancelled")
		}
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	})

	t.Run("cancel_func_terminates_helper", func(t *testing.T) {
		t.Parallel()
		stopCh := make(chan struct{})
		_, cancel := ctxFromStopCh(context.Background(), stopCh, 0)
		cancel()
		// Subsequent close of stopCh must not panic via cancel().
		close(stopCh)
		// Idempotent cancel.
		cancel()
	})

	t.Run("already_closed_stopCh", func(t *testing.T) {
		t.Parallel()
		stopCh := make(chan struct{})
		close(stopCh)
		ctx, cancel := ctxFromStopCh(context.Background(), stopCh, 0)
		defer cancel()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("ctx did not cancel for already-closed stopCh")
		}
	})

	t.Run("timeout_fires", func(t *testing.T) {
		t.Parallel()
		stopCh := make(chan struct{})
		ctx, cancel := ctxFromStopCh(context.Background(), stopCh, 25*time.Millisecond)
		defer cancel()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("ctx did not cancel on timeout")
		}
		require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	})
}

// Sentinel reference so unused-import linters don't trip in some builds.
var _ = errors.Is
