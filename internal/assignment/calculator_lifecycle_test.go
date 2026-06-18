package assignment

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pmetrics "github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --- merged from calculator_partition_lifecycle_test.go ---

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

// --- merged from calculator_partition_lifecycle_fsm_test.go ---

// fsmPartitionFixture is a minimal partition-lifecycle test surface. Unlike
// the blocking-KV fixture used by ISSUE-002/003, this fixture wires a real
// publisher path so partition-source changes drive the FSM end-to-end.
type fsmPartitionFixture struct {
	calc          *Calculator
	source        *mockWatchableSource
	heartbeatKV   jetstream.KeyValue
	stateProvider *mockStateProvider
}

func buildFSMPartitionFixture(t *testing.T, busName string, drainInterval time.Duration, plannedScale time.Duration) *fsmPartitionFixture {
	t.Helper()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, busName+"-asgn")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, busName+"-hb")

	// Seed two workers so emergency disappearance has a target.
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-a", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-b", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockWatchableSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		},
	}
	stateProvider := &mockStateProvider{}

	cfg := &Config{
		AssignmentKV:                assignmentKV,
		HeartbeatKV:                 heartbeatKV,
		AssignmentPrefix:            "assignment",
		HeartbeatPrefix:             "worker-hb",
		HeartbeatTTL:                10 * time.Second, // poll fallback at 5s (long; tests do not depend on it)
		Source:                      source,
		Strategy:                    &mockStrategy{},
		EmergencyGracePeriod:        50 * time.Millisecond,
		Cooldown:                    1 * time.Millisecond,
		ColdStartWindow:             20 * time.Millisecond,
		PlannedScaleWindow:          plannedScale,
		RebalanceGraceDrainInterval: drainInterval,
		StateProvider:               stateProvider,
	}
	calc, err := NewCalculator(cfg)
	require.NoError(t, err)

	return &fsmPartitionFixture{
		calc:          calc,
		source:        source,
		heartbeatKV:   heartbeatKV,
		stateProvider: stateProvider,
	}
}

// recordFSMTransitions subscribes to the calculator's state-change channel
// and returns a synchronized recorder. Subscribe BEFORE driving any state
// change so initial replay is captured.
func recordFSMTransitions(calc *Calculator) (*fsmRecorder, func()) {
	ch, unsubscribe := calc.SubscribeToStateChanges()
	r := &fsmRecorder{}
	r.wg.Go(func() {
		for s := range ch {
			r.mu.Lock()
			r.states = append(r.states, s)
			r.mu.Unlock()
		}
	})

	return r, func() {
		unsubscribe()
		r.wg.Wait()
	}
}

type fsmRecorder struct {
	mu     sync.Mutex
	wg     sync.WaitGroup
	states []types.CalculatorState
}

func (r *fsmRecorder) snapshot() []types.CalculatorState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]types.CalculatorState, len(r.states))
	copy(out, r.states)
	return out
}

func (r *fsmRecorder) contains(s types.CalculatorState) bool {
	return slices.Contains(r.snapshot(), s)
}

// TestPartitionLifecycle_DrivesFSMRebalancing verifies PR-3 §5.2 — a
// partition-source change drives Idle → Rebalancing → Idle on the calculator
// FSM (which the manager maps to Stable → Rebalancing → Stable). The test
// asserts eventual presence of the transitions on the calculator-side stream
// (deterministic) rather than cross-tuple ordering of manager hooks (async).
func TestPartitionLifecycle_DrivesFSMRebalancing(t *testing.T) {
	// No t.Parallel(): tests in this file share package-level test seams
	// (partitionRebalanceBlocker, partitionRebalanceRequestTimeout,
	// partitionTailCheckTimeout) so they must run sequentially.
	ctx := t.Context()

	f := buildFSMPartitionFixture(t, "test-fsm-partition", 50*time.Millisecond, 20*time.Millisecond)
	calc, source := f.calc, f.source

	rec, stopRec := recordFSMTransitions(calc)
	defer stopRec()

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for initial cold-start rebalance to land and FSM to settle Idle.
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() >= 1 && calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond, "initial cold-start rebalance must complete")

	startEntries := PartitionRebalanceEntries(calc)
	startVersion := calc.CurrentVersion()

	// Push a partition-source change — this is the new FSM-claim path.
	source.Update([]types.Partition{
		{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
	})

	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > startVersion && calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond,
		"partition update must drive a rebalance via the FSM and return to Idle")

	require.Eventually(t, func() bool {
		return PartitionRebalanceEntries(calc) > startEntries
	}, 2*time.Second, 10*time.Millisecond, "handlePartitionRebalance must run for the partition update")

	// FSM-side notification stream must contain Rebalancing (the FSM claim
	// emits notifyStateChange before invoking the callback) followed
	// eventually by Idle. Presence-based assertion per §5.2.
	require.True(t, rec.contains(types.CalcStateRebalancing),
		"calculator state stream must contain Rebalancing for the partition-source change; got %v", rec.snapshot())
	require.True(t, rec.contains(types.CalcStateIdle),
		"calculator state stream must return to Idle after Rebalancing")
}

// TestPartitionLifecycle_ClaimFailureDuplicateBound verifies PR-3 §5.3 —
// when a partition-source change fires while the FSM is in Scaling, the
// claim is deferred, the drain ticker retries, and the partition-rebalance
// callback runs AT MOST ONCE (entries counter delta == 1).
func TestPartitionLifecycle_ClaimFailureDuplicateBound(t *testing.T) {
	// No t.Parallel(): tests in this file share package-level test seams
	// (partitionRebalanceBlocker, partitionRebalanceRequestTimeout,
	// partitionTailCheckTimeout) so they must run sequentially.
	ctx := t.Context()

	// Long-ish PlannedScaleWindow so we can push the partition update while
	// the FSM is in Scaling but still finish the test promptly.
	f := buildFSMPartitionFixture(t, "test-fsm-dup-bound", 25*time.Millisecond, 200*time.Millisecond)
	calc, source, heartbeatKV := f.calc, f.source, f.heartbeatKV

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()

	require.Eventually(t, func() bool {
		return calc.CurrentVersion() >= 1 && calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond, "initial cold-start rebalance must complete")

	// Trigger a worker-set change to put FSM into Scaling: add a third
	// worker heartbeat.
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-c", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Wait until FSM enters Scaling so the partition push hits a non-Idle
	// claim source.
	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateScaling
	}, 3*time.Second, 5*time.Millisecond, "FSM must enter Scaling after worker join")

	n0 := PartitionRebalanceEntries(calc)

	// Push the partition update while in Scaling: TryClaimRebalancing returns
	// false; pendingPartitionUpdate flips on; drain ticker retries after the
	// in-flight rebalance lands.
	source.Update([]types.Partition{
		{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
	})

	// Allow Scaling to complete, drain-tick to win the now-Idle CAS, and
	// at least two further drain ticks during which a buggy implementation
	// could double-fire.
	time.Sleep(200*time.Millisecond + // PlannedScaleWindow
		4*25*time.Millisecond + // 4 drain ticks
		300*time.Millisecond) // assignment settle

	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond, "FSM must return to Idle after the drain retry completes")

	n1 := PartitionRebalanceEntries(calc)
	require.Equal(t, int64(1), n1-n0,
		"partition-rebalance callback must run exactly once for one dropped partition update (n0=%d, n1=%d)", n0, n1)
}

// TestPartitionLifecycle_GraceFlipPropagatesErrShuttingDown verifies PR-3
// §5.4 — when recovery grace flips between the partition-watch arm pre-check
// (grace=false at that point) and rebalanceMu acquisition (grace flipped to
// true), c.rebalance returns errShuttingDown, the
// handlePartitionRebalance / RunClaimedRebalanceErr / triggerPartitionRebalance
// chain propagates it to restorePendingOnGraceBail, pendingPartitionUpdate
// is restored, and the drain ticker retries when grace lifts.
//
// To sequence the flip deterministically the test uses the
// partitionRebalanceBlocker test seam: handlePartitionRebalance parks on the
// blocker AFTER the partition-watch arm has already accepted the update with
// grace=false, but BEFORE c.rebalance acquires rebalanceMu. While parked the
// test flips grace to true; releasing the blocker then drives c.rebalance into
// shouldDeferForRecoveryGrace, which returns errShuttingDown.
func TestPartitionLifecycle_GraceFlipPropagatesErrShuttingDown(t *testing.T) {
	// No t.Parallel(): tests in this file share package-level test seams
	// (partitionRebalanceBlocker, partitionRebalanceRequestTimeout,
	// partitionTailCheckTimeout) so they must run sequentially.
	ctx := t.Context()

	f := buildFSMPartitionFixture(t, "test-fsm-grace-flip", 25*time.Millisecond, 20*time.Millisecond)
	calc, source, sp := f.calc, f.source, f.stateProvider

	blocker := make(chan struct{})
	SetPartitionRebalanceBlocker(blocker)
	defer SetPartitionRebalanceBlocker(nil)

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()

	require.Eventually(t, func() bool {
		return calc.CurrentVersion() >= 1 && calc.GetState() == types.CalcStateIdle
	}, 2*time.Second, 10*time.Millisecond, "initial cold-start rebalance must complete")

	startEntries := PartitionRebalanceEntries(calc)
	v0 := calc.CurrentVersion()

	// Grace is FALSE here so the partition-watch pre-check accepts the update
	// and triggerPartitionRebalance proceeds to handlePartitionRebalance.
	require.False(t, sp.IsInRecoveryGrace(),
		"grace must be false so the watch-arm pre-check passes")

	source.Update([]types.Partition{
		{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
	})

	// Wait until handlePartitionRebalance is parked on the blocker — this
	// proves the watch-arm accepted the update at grace=false and we are now
	// in the window between the pre-check and rebalanceMu acquisition.
	require.Eventually(t, func() bool {
		return PartitionRebalanceEntries(calc) == startEntries+1
	}, 2*time.Second, 5*time.Millisecond,
		"handlePartitionRebalance must enter and park on the blocker")

	// Flip grace to true while the callback is parked. When we close the
	// blocker, c.rebalance acquires rebalanceMu, calls
	// shouldDeferForRecoveryGrace, sees grace=true, and returns errShuttingDown.
	// A subsequent retry-after-grace-lifts callback also reads from the now-
	// closed channel, which receives immediately — so we do NOT mutate
	// partitionRebalanceBlocker again here (mutating it from this goroutine
	// while the calculator goroutine is reading it races under -race).
	sp.SetGrace(true)
	close(blocker)

	// Phase A: no version increment must land — the rebalance bailed.
	require.Never(t, func() bool {
		return calc.CurrentVersion() > v0
	}, 250*time.Millisecond, 25*time.Millisecond,
		"rebalance must not complete after errShuttingDown bail")

	// pendingPartitionUpdate must be restored by restorePendingOnGraceBail so
	// the drain ticker has work to do once grace lifts.
	require.Eventually(t, func() bool {
		return calc.pendingPartitionUpdate.Load()
	}, 1*time.Second, 10*time.Millisecond,
		"pendingPartitionUpdate must be restored after errShuttingDown bail")

	// Phase B: lift grace; drain-tick retry must complete the rebalance.
	sp.SetGrace(false)
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > v0
	}, 2*time.Second, 10*time.Millisecond,
		"deferred partition update must drain via the FSM after grace lifts")
}

// TestPartitionLifecycle_EmergencyContentionRegression verifies PR-3 §5.5 —
// when a worker disappears during a partition-lifecycle rebalance window
// (so the concurrent worker-poll's TryClaimEmergency was rejected because the
// FSM was Rebalancing), the in-line tail-check after the partition rebalance
// MUST claim Emergency on its own — without waiting for the next worker-monitor
// poll tick.
//
// Sequencing (deterministic via partitionRebalanceBlocker + short
// EmergencyGracePeriod):
//  1. Cold-start lands; lastWorkers = {worker-a, worker-b}, FSM = Idle.
//  2. Install blocker; push partition update.
//  3. handlePartitionRebalance enters (entries++), parks on the blocker. FSM
//     is now Rebalancing.
//  4. Delete worker-a heartbeat. The WorkerMonitor watcher fires a debounced
//     pollForChanges → observeAndDecide which:
//     - sees workers = {b}, prev = {a, b} (lastWorkers unchanged because the
//     P0 fix removed the premature lastWorkers refresh in handlePartitionRebalance);
//     - records firstSeen[a] in EmergencyDetector via CheckEmergency Phase 2;
//     - TryClaimEmergency rejects (FSM is Rebalancing) — the regression case
//     the tail-check is designed to recover.
//  5. Sleep > EmergencyGracePeriod so firstSeen[a] becomes confirmable.
//  6. Close the blocker. c.rebalance runs (with workers={b}; ObserveAlive({b})
//     does not touch firstSeen[a]). handlePartitionRebalance returns; FSM
//     transitions to Idle.
//  7. Tail-check observeAndDecide runs on a fresh context. lastWorkers is still
//     {a, b} (no refresh after the partition rebalance), so CheckEmergency
//     Phase 4 fires for worker-a; the tail-check claims Emergency.
//
// Asserts the FSM stream contains CalcStateEmergency after the partition
// rebalance completes, and that it returns to CalcStateIdle eventually.
func TestPartitionLifecycle_EmergencyContentionRegression(t *testing.T) {
	// No t.Parallel(): tests in this file share package-level test seams
	// (partitionRebalanceBlocker, partitionRebalanceRequestTimeout,
	// partitionTailCheckTimeout) so they must run sequentially.
	ctx := t.Context()

	// Fixture default EmergencyGracePeriod is 50ms — short enough that the
	// worker-monitor's watcher-driven observeAndDecide can register
	// firstSeen[worker-a] and then time it out while the partition rebalance
	// is parked on the blocker.
	f := buildFSMPartitionFixture(t, "test-fsm-emergency-contention", 25*time.Millisecond, 20*time.Millisecond)
	calc, source, heartbeatKV := f.calc, f.source, f.heartbeatKV

	rec, stopRec := recordFSMTransitions(calc)
	defer stopRec()

	blocker := make(chan struct{})
	SetPartitionRebalanceBlocker(blocker)
	defer SetPartitionRebalanceBlocker(nil)

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()

	require.Eventually(t, func() bool {
		return calc.CurrentVersion() >= 1 && calc.GetState() == types.CalcStateIdle
	}, 3*time.Second, 10*time.Millisecond, "initial cold-start must complete")

	startEntries := PartitionRebalanceEntries(calc)

	// Push a partition update — handlePartitionRebalance will park on the
	// blocker AFTER incrementing the entries counter.
	source.Update([]types.Partition{
		{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
	})
	require.Eventually(t, func() bool {
		return PartitionRebalanceEntries(calc) == startEntries+1
	}, 2*time.Second, 5*time.Millisecond,
		"partition rebalance must park on the blocker")

	// Delete worker-a heartbeat: the WorkerMonitor watcher will fire a
	// debounced observeAndDecide which records firstSeen[worker-a] under
	// EmergencyDetector even though the TryClaimEmergency call rejects
	// (FSM is Rebalancing).
	require.NoError(t, heartbeatKV.Delete(ctx, "worker-hb.worker-a"))

	// Wait > debounce (100ms) + > EmergencyGracePeriod (30ms) so the
	// tail-check, when it runs, can confirm worker-a as an emergency loser.
	time.Sleep(250 * time.Millisecond)

	// Release the blocker; c.rebalance runs, returns; tail-check then sees
	// workers={b}, lastWorkers={a,b} (unchanged), and confirms emergency.
	close(blocker)

	// Emergency must be claimed via the tail-check, without waiting for the
	// next worker-monitor poll tick (poll interval is HeartbeatTTL/2 = 5s in
	// this fixture, so anything within ~2s came from the tail-check).
	require.Eventually(t, func() bool {
		return rec.contains(types.CalcStateEmergency)
	}, 2*time.Second, 10*time.Millisecond,
		"tail-check must claim Emergency for the worker that disappeared during the rebalance window; got %v",
		rec.snapshot())

	// FSM must return to Idle after the emergency rebalance.
	require.Eventually(t, func() bool {
		return calc.GetState() == types.CalcStateIdle
	}, 3*time.Second, 10*time.Millisecond,
		"FSM must return to Idle after the tail-check-driven emergency rebalance")
}

// TestPartitionLifecycle_TailCheckUsesFreshContext verifies PR-3 §5.7 — the
// tail-check observeAndDecide is allocated a FRESH stop-aware context, NOT
// the rebalance reqCtx that may already be cancelled by the time
// RunClaimedRebalanceErr returns. A wrong implementation that reused reqCtx
// for the tail-check would exit fast at collectWorkerObservation and strand
// any emergency loser until the next poll tick.
//
// This test asserts BOTH:
//
//   - the structural fresh-context invariant directly via
//     partitionTailCheckEntryHook (reqCtx must be cancelled, tailCtx must be
//     fresh at hook entry);
//   - that the tail-check can actually drive an Emergency claim on the fresh
//     context — proving the budget is real and reaches CheckEmergency.
//
// Sequencing mirrors TestPartitionLifecycle_EmergencyContentionRegression: a
// worker disappears while the partition rebalance is parked, so the
// worker-monitor's watcher-driven observeAndDecide registers firstSeen, the
// blocker is released after gracePeriod, and the tail-check on a fresh
// context claims Emergency.
func TestPartitionLifecycle_TailCheckUsesFreshContext(t *testing.T) {
	// No t.Parallel(): tests in this file share package-level test seams.
	ctx := t.Context()

	f := buildFSMPartitionFixture(t, "test-fsm-fresh-ctx", 25*time.Millisecond, 20*time.Millisecond)
	calc, source, heartbeatKV := f.calc, f.source, f.heartbeatKV

	rec, stopRec := recordFSMTransitions(calc)
	defer stopRec()

	prevReq := SetPartitionRebalanceRequestTimeout(10 * time.Millisecond)
	defer SetPartitionRebalanceRequestTimeout(prevReq)
	prevTail := SetPartitionTailCheckTimeout(500 * time.Millisecond)
	defer SetPartitionTailCheckTimeout(prevTail)

	blocker := make(chan struct{})
	SetPartitionRebalanceBlocker(blocker)
	defer SetPartitionRebalanceBlocker(nil)

	type hookSample struct {
		tailCtxErr error
		reqCtxErr  error
	}
	var (
		sampleMu sync.Mutex
		samples  []hookSample
	)
	SetPartitionTailCheckEntryHook(func(tailCtx context.Context, reqCtxErr error) {
		sampleMu.Lock()
		samples = append(samples, hookSample{tailCtxErr: tailCtx.Err(), reqCtxErr: reqCtxErr})
		sampleMu.Unlock()
	})
	defer SetPartitionTailCheckEntryHook(nil)

	require.NoError(t, calc.Start(ctx))
	defer func() { _ = calc.Stop(ctx) }()

	require.Eventually(t, func() bool {
		return calc.CurrentVersion() >= 1 && calc.GetState() == types.CalcStateIdle
	}, 3*time.Second, 10*time.Millisecond, "initial cold-start must complete")

	n0 := PartitionRebalanceEntries(calc)

	source.Update([]types.Partition{
		{Keys: []string{"p1"}}, {Keys: []string{"p2"}}, {Keys: []string{"p3"}},
	})

	require.Eventually(t, func() bool {
		return PartitionRebalanceEntries(calc) == n0+1
	}, 2*time.Second, 5*time.Millisecond, "partition callback must park on blocker")

	// Drop worker-a while the partition rebalance is parked so the
	// worker-monitor's watcher fires an observeAndDecide that registers
	// firstSeen[worker-a]. Combined with the 50ms park margin (well above the
	// 50ms EmergencyGracePeriod the fixture configures), the tail-check on a
	// FRESH context will confirm worker-a as the emergency loser; a buggy
	// implementation that reused the exhausted reqCtx would exit fast at
	// collectWorkerObservation and never reach Phase 4.
	require.NoError(t, heartbeatKV.Delete(ctx, "worker-hb.worker-a"))

	// 5x margin over the 10ms partitionRebalanceRequestTimeout AND > the
	// 50ms EmergencyGracePeriod (we sleep 250ms to also cover the 100ms
	// watcher debounce + handler latency).
	time.Sleep(250 * time.Millisecond)

	close(blocker)

	require.Eventually(t, func() bool {
		sampleMu.Lock()
		defer sampleMu.Unlock()
		return len(samples) > 0
	}, 2*time.Second, 5*time.Millisecond, "tail-check entry hook must fire")

	sampleMu.Lock()
	got := samples[0]
	sampleMu.Unlock()

	require.Error(t, got.reqCtxErr,
		"reqCtx must be cancelled by the time the tail-check runs (10ms budget + 250ms park)")
	require.NoError(t, got.tailCtxErr,
		"tail-check MUST run on a fresh context — tailCtx must NOT inherit reqCtx's cancellation")

	// Emergency must be claimed by the tail-check on its fresh context.
	// Worker-monitor poll interval in this fixture is HeartbeatTTL/2 = 5s, so
	// anything within 2s came from the tail-check.
	require.Eventually(t, func() bool {
		return rec.contains(types.CalcStateEmergency)
	}, 2*time.Second, 10*time.Millisecond,
		"tail-check on fresh context must claim Emergency for the disappeared worker; got %v",
		rec.snapshot())
}

// --- merged from calculator_initial_assignment_test.go ---

// TestCalculator_InitialAssignment_TwoPhase verifies the two-phase initial assignment.
// This is the critical test that ensures zero coverage gap during cold start.
func TestCalculator_InitialAssignment_TwoPhase(t *testing.T) {
	t.Parallel()
	t.Run("immediate assignment covers partitions before stabilization window", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-twophase-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-twophase-heartbeat")

		// Create ONE worker heartbeat initially (simulating leader only)
		_, err := heartbeatKV.Put(ctx, "worker-hb.worker-0", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		partitions := []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
			{Keys: []string{"p3"}},
			{Keys: []string{"p4"}},
		}
		source := &mockSource{partitions: partitions}
		strategy := &mockStrategy{}

		calc, err := NewCalculator(&Config{
			AssignmentKV:         assignmentKV,
			HeartbeatKV:          heartbeatKV,
			AssignmentPrefix:     "assignment",
			Source:               source,
			Strategy:             strategy,
			HeartbeatPrefix:      "worker-hb",
			HeartbeatTTL:         6 * time.Second,
			EmergencyGracePeriod: 3 * time.Second,
			ColdStartWindow:      200 * time.Millisecond, // Longer window to test behavior
			PlannedScaleWindow:   50 * time.Millisecond,
		})
		require.NoError(t, err)

		// Start calculator (initialAssignment runs in background)
		err = calc.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		// CRITICAL TEST: Verify assignment happens IMMEDIATELY (within 100ms)
		// This proves zero coverage gap - partitions assigned before stabilization window
		var immediateAssignment types.Assignment
		require.Eventually(t, func() bool {
			entry, err := assignmentKV.Get(ctx, "assignment.worker-0")
			if err != nil {
				return false
			}
			err = json.Unmarshal(entry.Value(), &immediateAssignment)

			return err == nil && len(immediateAssignment.Partitions) > 0
		}, 100*time.Millisecond, 10*time.Millisecond, "immediate assignment must happen within 100ms")

		// Verify worker-0 got ALL partitions (it's the only worker)
		require.Len(t, immediateAssignment.Partitions, 4, "worker-0 should get all partitions immediately")
		require.Equal(t, int64(1), immediateAssignment.Version, "first assignment should be version 1")

		t.Logf("IMMEDIATE ASSIGNMENT: worker-0 received %d partitions in <100ms (version %d)",
			len(immediateAssignment.Partitions), immediateAssignment.Version)

		// Now add 2 more workers DURING the stabilization window
		time.Sleep(50 * time.Millisecond) // Let immediate assignment settle
		_, err = heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)
		_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		t.Log("Added worker-1 and worker-2 during stabilization window")

		// Wait for stabilization window to complete + some margin
		time.Sleep(300 * time.Millisecond)

		// Verify FINAL assignment redistributed partitions to all 3 workers
		var finalAssignments [3]types.Assignment
		for i := range 3 {
			entry, err := assignmentKV.Get(ctx, "assignment.worker-"+string(rune('0'+i)))
			require.NoError(t, err, "worker-%d should have assignment", i)

			err = json.Unmarshal(entry.Value(), &finalAssignments[i])
			require.NoError(t, err)
		}

		// Verify version increased (final assignment)
		require.Greater(t, finalAssignments[0].Version, immediateAssignment.Version,
			"final assignment should have higher version")

		// Verify total partition coverage
		totalPartitions := 0
		for i := range 3 {
			totalPartitions += len(finalAssignments[i].Partitions)
			t.Logf("worker-%d: %d partitions (version %d)", i,
				len(finalAssignments[i].Partitions), finalAssignments[i].Version)
		}
		require.Equal(t, 4, totalPartitions, "all 4 partitions should be assigned")

		t.Logf("✅ FINAL ASSIGNMENT: Partitions redistributed across 3 workers (version %d)",
			finalAssignments[0].Version)
	})

	t.Run("handles context cancellation during stabilization wait", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-cancel-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-cancel-heartbeat")

		_, err := heartbeatKV.Put(ctx, "worker-hb.worker-0", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}}
		strategy := &mockStrategy{}

		calc, err := NewCalculator(&Config{
			AssignmentKV:         assignmentKV,
			HeartbeatKV:          heartbeatKV,
			AssignmentPrefix:     "assignment",
			Source:               source,
			Strategy:             strategy,
			HeartbeatPrefix:      "worker-hb",
			HeartbeatTTL:         6 * time.Second,
			EmergencyGracePeriod: 3 * time.Second,
			ColdStartWindow:      5 * time.Second, // Long window
			PlannedScaleWindow:   50 * time.Millisecond,
		})
		require.NoError(t, err)

		// Create cancellable context
		cancelCtx, cancel := context.WithCancel(ctx)

		err = calc.Start(cancelCtx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		// Wait for immediate assignment
		require.Eventually(t, func() bool {
			return calc.CurrentVersion() > 0
		}, 500*time.Millisecond, 10*time.Millisecond, "immediate assignment should complete")

		immediateVersion := calc.CurrentVersion()
		t.Logf("Immediate assignment completed at version %d", immediateVersion)

		// Cancel context during stabilization window
		cancel()

		// Give some time for cancellation to propagate
		time.Sleep(200 * time.Millisecond)

		// Final assignment should NOT happen due to cancellation
		finalVersion := calc.CurrentVersion()
		require.Equal(t, immediateVersion, finalVersion,
			"version should not change after context cancellation")

		t.Logf("✅ Context cancellation prevented final assignment (stayed at version %d)", finalVersion)
	})

	t.Run("single worker scenario - no rebalancing needed", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-single-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-single-heartbeat")

		// Single worker
		_, err := heartbeatKV.Put(ctx, "worker-hb.worker-0", []byte(time.Now().Format(time.RFC3339Nano)))
		require.NoError(t, err)

		source := &mockSource{partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		}}
		strategy := &mockStrategy{}

		calc, err := NewCalculator(&Config{
			AssignmentKV:         assignmentKV,
			HeartbeatKV:          heartbeatKV,
			AssignmentPrefix:     "assignment",
			Source:               source,
			Strategy:             strategy,
			HeartbeatPrefix:      "worker-hb",
			HeartbeatTTL:         6 * time.Second,
			EmergencyGracePeriod: 3 * time.Second,
			ColdStartWindow:      300 * time.Millisecond,
			PlannedScaleWindow:   50 * time.Millisecond,
		})
		require.NoError(t, err)

		err = calc.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		// Wait for both phases to complete
		time.Sleep(500 * time.Millisecond)

		// Should have 2 versions: immediate + final
		require.GreaterOrEqual(t, calc.CurrentVersion(), int64(2),
			"should have at least 2 versions (immediate + final)")

		// Verify worker-0 still has all partitions
		entry, err := assignmentKV.Get(ctx, "assignment.worker-0")
		require.NoError(t, err)

		var assignment types.Assignment
		err = json.Unmarshal(entry.Value(), &assignment)
		require.NoError(t, err)
		require.Len(t, assignment.Partitions, 2, "single worker should keep all partitions")

		t.Logf("✅ Single worker kept all partitions through both phases (version %d)", assignment.Version)
	})

	t.Run("multiple workers present at start", func(t *testing.T) {
		ctx := t.Context()

		_, nc := partitest.StartEmbeddedNATS(t)
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-multi-start-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-multi-start-heartbeat")

		// Create 3 workers BEFORE calculator starts
		for i := range 3 {
			_, err := heartbeatKV.Put(ctx, "worker-hb.worker-"+string(rune('0'+i)),
				[]byte(time.Now().Format(time.RFC3339Nano)))
			require.NoError(t, err)
		}

		source := &mockSource{partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
			{Keys: []string{"p3"}},
			{Keys: []string{"p4"}},
			{Keys: []string{"p5"}},
			{Keys: []string{"p6"}},
		}}
		strategy := &mockStrategy{}

		calc, err := NewCalculator(&Config{
			AssignmentKV:         assignmentKV,
			HeartbeatKV:          heartbeatKV,
			AssignmentPrefix:     "assignment",
			Source:               source,
			Strategy:             strategy,
			HeartbeatPrefix:      "worker-hb",
			HeartbeatTTL:         6 * time.Second,
			EmergencyGracePeriod: 3 * time.Second,
			ColdStartWindow:      300 * time.Millisecond,
			PlannedScaleWindow:   50 * time.Millisecond,
		})
		require.NoError(t, err)

		err = calc.Start(ctx)
		require.NoError(t, err)
		defer func() { _ = calc.Stop(ctx) }()

		// Immediate assignment should distribute across all 3 workers
		require.Eventually(t, func() bool {
			return calc.CurrentVersion() > 0
		}, 200*time.Millisecond, 10*time.Millisecond, "immediate assignment should complete")

		// Verify all 3 workers got assignments immediately
		immediatePartitionCount := 0
		for i := range 3 {
			entry, err := assignmentKV.Get(ctx, "assignment.worker-"+string(rune('0'+i)))
			if err == nil {
				var assignment types.Assignment
				if json.Unmarshal(entry.Value(), &assignment) == nil {
					immediatePartitionCount += len(assignment.Partitions)
					t.Logf("Immediate: worker-%d got %d partitions", i, len(assignment.Partitions))
				}
			}
		}
		require.Equal(t, 6, immediatePartitionCount, "all partitions should be assigned immediately")

		// Wait for final assignment
		time.Sleep(500 * time.Millisecond)

		// Verify final distribution
		finalPartitionCount := 0
		for i := range 3 {
			entry, err := assignmentKV.Get(ctx, "assignment.worker-"+string(rune('0'+i)))
			require.NoError(t, err)

			var assignment types.Assignment
			err = json.Unmarshal(entry.Value(), &assignment)
			require.NoError(t, err)
			finalPartitionCount += len(assignment.Partitions)
			t.Logf("Final: worker-%d has %d partitions", i, len(assignment.Partitions))
		}
		require.Equal(t, 6, finalPartitionCount, "all partitions should remain assigned")

		t.Log("All workers received immediate assignments, maintained coverage through final phase")
	})
}

// TestCalculator_InitialAssignment_ZeroCoverageGap verifies that partitions are assigned continuously.
func TestCalculator_InitialAssignment_ZeroCoverageGap(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-zerogap-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-zerogap-heartbeat")

	// Create leader heartbeat
	_, err := heartbeatKV.Put(ctx, "worker-hb.leader", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	partitions := []types.Partition{{Keys: []string{"p1"}}, {Keys: []string{"p2"}}}
	source := &mockSource{partitions: partitions}
	strategy := &mockStrategy{}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         6 * time.Second,
		EmergencyGracePeriod: 3 * time.Second,
		ColdStartWindow:      200 * time.Millisecond, // Long enough to verify gap
		PlannedScaleWindow:   50 * time.Millisecond,
	})
	require.NoError(t, err)

	startTime := time.Now()
	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Continuously poll assignment to ensure it NEVER disappears or becomes empty
	gapDetected := false
	assignmentCheckDone := make(chan bool)

	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				entry, err := assignmentKV.Get(ctx, "assignment.leader")
				if err != nil {
					// No assignment yet - this is OK only in the first ~50ms
					elapsed := time.Since(startTime)
					if elapsed > 50*time.Millisecond {
						gapDetected = true
						t.Errorf("COVERAGE GAP DETECTED: No assignment after %v", elapsed)
						return
					}

					continue
				}

				var assignment types.Assignment
				if err := json.Unmarshal(entry.Value(), &assignment); err != nil {
					continue
				}

				if len(assignment.Partitions) == 0 {
					gapDetected = true
					t.Error("COVERAGE GAP DETECTED: Empty partition list in assignment")
					return
				}

				// Check if we've passed the stabilization window
				if time.Since(startTime) > 250*time.Millisecond {
					return
				}

			case <-assignmentCheckDone:
				return
			}
		}
	}()

	// Wait for full cycle (immediate + stabilization + final)
	time.Sleep(300 * time.Millisecond)
	close(assignmentCheckDone)

	require.False(t, gapDetected, "No coverage gap should be detected at any point")

	// Verify final state
	entry, err := assignmentKV.Get(ctx, "assignment.leader")
	require.NoError(t, err)

	var finalAssignment types.Assignment
	err = json.Unmarshal(entry.Value(), &finalAssignment)
	require.NoError(t, err)
	require.Len(t, finalAssignment.Partitions, 2, "leader should have both partitions")

	t.Logf("✅ ZERO COVERAGE GAP VERIFIED: Partitions were assigned continuously throughout %v window",
		time.Since(startTime))
}

// TestCalculator_InitialAssignment_Metrics verifies metrics are recorded correctly.
func TestCalculator_InitialAssignment_Metrics(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-metrics-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-metrics-heartbeat")

	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-0", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{partitions: []types.Partition{{Keys: []string{"p1"}}}}
	strategy := &mockStrategy{}

	// Use mock metrics collector
	metrics := &mockMetricsCollector{
		NopMetrics:        pmetrics.NewNop(),
		rebalanceAttempts: make(map[string]int),
	}

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         6 * time.Second,
		EmergencyGracePeriod: 3 * time.Second,
		ColdStartWindow:      100 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Metrics:              metrics,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Ensure the lone leader claims all partitions immediately.
	require.Eventually(t, func() bool {
		entry, err := assignmentKV.Get(ctx, "assignment.worker-0")
		if err != nil {
			return false
		}

		var assignment types.Assignment
		if json.Unmarshal(entry.Value(), &assignment) != nil {
			return false
		}

		return len(assignment.Partitions) == 1
	}, 150*time.Millisecond, 10*time.Millisecond, "leader should get all partitions immediately")

	// The final (post-stabilization) cold_start rebalance fires after the
	// ColdStartWindow timer elapses. Under load that timer plus the rebalance
	// itself can run well past a fixed 200ms budget, so poll for the metric
	// instead of sleeping a fixed amount and asserting once.
	require.Eventually(t, func() bool {
		return metrics.GetRebalanceAttempt("cold_start") == 1
	}, 3*time.Second, 20*time.Millisecond, "final cold_start rebalance should run exactly once")

	// Immediate-assignment phase must have recorded exactly once.
	require.Equal(t, 1, metrics.GetRebalanceAttempt("cold_start_immediate"),
		"immediate assignment should be recorded exactly once")

	// Exactly-once guard: the final cold_start must not run again for the
	// unchanged single-worker topology.
	require.Never(t, func() bool {
		return metrics.GetRebalanceAttempt("cold_start") > 1
	}, 300*time.Millisecond, 50*time.Millisecond,
		"cold_start must run exactly once, not repeatedly")

	t.Logf("✅ Metrics correctly recorded: immediate=%d, final=%d",
		metrics.GetRebalanceAttempt("cold_start_immediate"),
		metrics.GetRebalanceAttempt("cold_start"))
}

// mockMetricsCollector for testing. Embeds NopMetrics so it picks up future
// MetricsCollector methods automatically.
type mockMetricsCollector struct {
	*pmetrics.NopMetrics
	mu                sync.RWMutex
	rebalanceAttempts map[string]int
}

func (m *mockMetricsCollector) RecordRebalanceAttempt(lifecycle string, success bool) {
	if success {
		m.mu.Lock()
		m.rebalanceAttempts[lifecycle]++
		m.mu.Unlock()
	}
}

func (m *mockMetricsCollector) GetRebalanceAttempt(key string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rebalanceAttempts[key]
}

func (m *mockMetricsCollector) RecordRebalanceDuration(duration float64, lifecycle string)   {}
func (m *mockMetricsCollector) RecordWorkerChange(added, removed int)                        {}
func (m *mockMetricsCollector) RecordActiveWorkers(count int)                                {}
func (m *mockMetricsCollector) RecordPartitionCount(count int)                               {}
func (m *mockMetricsCollector) RecordEmergencyRebalance(disappearedWorkers int)              {}
func (m *mockMetricsCollector) RecordCacheUsage(cacheType string, age float64)               {}
func (m *mockMetricsCollector) IncrementCacheFallback(reason string)                         {}
func (m *mockMetricsCollector) RecordStateChangeDropped()                                    {}
func (m *mockMetricsCollector) RecordKVOperationDuration(operation string, duration float64) {}
func (m *mockMetricsCollector) RecordStateTransition(from, to types.State, duration float64) {}
func (m *mockMetricsCollector) RecordLeadershipChange(newLeader string)                      {}
func (m *mockMetricsCollector) RecordDegradedDuration(duration float64)                      {}
func (m *mockMetricsCollector) SetDegradedMode(degraded float64)                             {}
func (m *mockMetricsCollector) SetCacheAge(age float64)                                      {}
func (m *mockMetricsCollector) SetAlertLevel(level int)                                      {}
func (m *mockMetricsCollector) IncrementAlertEmitted(level string)                           {}
func (m *mockMetricsCollector) RecordHeartbeat(workerID string, success bool)                {}
func (m *mockMetricsCollector) RecordAssignmentChange(added, removed int, version int64)     {}
func (m *mockMetricsCollector) IncrementWorkerConsumerControlRetry(op string)                {}
func (m *mockMetricsCollector) RecordWorkerConsumerRetryBackoff(op string, seconds float64)  {}
func (m *mockMetricsCollector) SetWorkerConsumerSubjectsCurrent(count int)                   {}
func (m *mockMetricsCollector) IncrementWorkerConsumerSubjectChange(kind string, count int)  {}
func (m *mockMetricsCollector) IncrementWorkerConsumerGuardrailViolation(kind string)        {}
func (m *mockMetricsCollector) IncrementWorkerConsumerSubjectThresholdWarning()              {}
func (m *mockMetricsCollector) RecordWorkerConsumerUpdate(result string)                     {}
func (m *mockMetricsCollector) ObserveWorkerConsumerUpdateLatency(seconds float64)           {}
func (m *mockMetricsCollector) IncrementWorkerConsumerIteratorRestart(reason string)         {}
func (m *mockMetricsCollector) IncrementWorkerConsumerIteratorEscalation(_ string)           {}
func (m *mockMetricsCollector) SetWorkerConsumerConsecutiveIteratorFailures(count int)       {}
func (m *mockMetricsCollector) SetWorkerConsumerHealthStatus(healthy bool)                   {}
func (m *mockMetricsCollector) IncrementWorkerConsumerRecreationAttempt(reason string)       {}
func (m *mockMetricsCollector) RecordWorkerConsumerRecreation(result string, reason string)  {}
func (m *mockMetricsCollector) ObserveWorkerConsumerRecreationDuration(seconds float64)      {}
func (m *mockMetricsCollector) IncrementWorkerConsumerPullSuppressed(reason string)          {}
func (m *mockMetricsCollector) RecordOrphanedPartitions(count int)                           {}

// --- merged from calculator_recovery_grace_test.go ---

// TestCalculator_RecoveryGrace_BlocksNormalRebalance verifies that non-emergency
// rebalancing is deferred while the StateProvider reports IsInRecoveryGrace == true.
func TestCalculator_RecoveryGrace_BlocksNormalRebalance(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-recovery-grace-blocked-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-recovery-grace-blocked-heartbeat")

	// Register initial worker
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		},
	}
	strategy := &mockStrategy{}

	stateProvider := &mockStateProvider{}
	stateProvider.SetGrace(true) // activate recovery grace from the start

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         2 * time.Second,
		EmergencyGracePeriod: 5 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             1 * time.Millisecond,
		StateProvider:        stateProvider,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Capture the baseline only after the full cold-start sequence (immediate +
	// post-window final rebalance) has settled. A fixed sleep is racy: cold
	// start is not subject to recovery grace, so under load its final
	// rebalance (gated by the 50ms ColdStartWindow) can land AFTER the sleep
	// and bump the version during the measurement window below, masquerading as
	// an unblocked planned_scale. waitStableVersion blocks until the version
	// stops changing, so the baseline is the settled cold-start version.
	initialVersion := waitStableVersion(t, calc)

	// Add a second worker — this would normally trigger a scaling rebalance
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// Wait long enough for the watcher to fire and attempt checkForChanges
	time.Sleep(300 * time.Millisecond)

	// Version must NOT change — recovery grace suppresses non-emergency rebalancing
	blockedVersion := calc.CurrentVersion()
	require.Equal(t, initialVersion, blockedVersion,
		"recovery grace must block non-emergency rebalancing")
}

// TestCalculator_RecoveryGrace_AllowsRebalanceAfterGraceExpires verifies that normal
// rebalancing resumes once IsInRecoveryGrace returns false.
func TestCalculator_RecoveryGrace_AllowsRebalanceAfterGraceExpires(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-recovery-grace-allowed-assignment")
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-recovery-grace-allowed-heartbeat")

	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	source := &mockSource{
		partitions: []types.Partition{
			{Keys: []string{"p1"}},
			{Keys: []string{"p2"}},
		},
	}
	strategy := &mockStrategy{}

	stateProvider := &mockStateProvider{}
	stateProvider.SetGrace(true) // start in recovery grace

	calc, err := NewCalculator(&Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		AssignmentPrefix:     "assignment",
		Source:               source,
		Strategy:             strategy,
		HeartbeatPrefix:      "worker-hb",
		HeartbeatTTL:         2 * time.Second, // long TTL so heartbeats outlive the test
		EmergencyGracePeriod: 5 * time.Second,
		ColdStartWindow:      50 * time.Millisecond,
		PlannedScaleWindow:   50 * time.Millisecond,
		Cooldown:             1 * time.Millisecond,
		StateProvider:        stateProvider,
	})
	require.NoError(t, err)

	err = calc.Start(ctx)
	require.NoError(t, err)
	defer func() { _ = calc.Stop(ctx) }()

	// Wait for the full cold-start stabilization (immediate + post-window
	// final rebalance) to settle before measuring. A fixed sleep is racy under
	// load: cold start is not gated by recovery grace, so a late final
	// rebalance would shift the baseline. waitStableVersion blocks until the
	// version stops changing.
	initialVersion := waitStableVersion(t, calc)

	// Add worker-2 while grace is active — should be blocked
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)

	blockedVersion := calc.CurrentVersion()
	require.Equal(t, initialVersion, blockedVersion, "rebalance must be blocked during grace period")

	// Exit recovery grace; re-send heartbeat for worker-2 so the watcher fires again
	stateProvider.SetGrace(false)
	_, err = heartbeatKV.Put(ctx, "worker-hb.worker-2", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	// The watcher fires within ~100ms (debounce), then scaling window is 50ms.
	// Use Eventually with a generous timeout to avoid flakiness.
	require.Eventually(t, func() bool {
		return calc.CurrentVersion() > initialVersion
	}, 5*time.Second, 50*time.Millisecond,
		"rebalance must proceed after recovery grace expires (initial version: %d, current: %d)",
		initialVersion, calc.CurrentVersion(),
	)
}
