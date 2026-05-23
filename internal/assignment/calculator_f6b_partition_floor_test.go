package assignment

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// mutableSource is a mockSource whose partitions can be mutated between
// rebalance calls, letting the F6-B tests drive specific shape sequences
// (healthy → empty → empty → healthy, etc.) deterministically.
type mutableSource struct {
	mu         sync.Mutex
	partitions []types.Partition
}

func (m *mutableSource) Start(context.Context) error { return nil }
func (m *mutableSource) Stop(context.Context) error  { return nil }
func (m *mutableSource) List(context.Context) ([]types.Partition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]types.Partition, len(m.partitions))
	copy(out, m.partitions)

	return out, nil
}

func (m *mutableSource) set(p []types.Partition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.partitions = p
}

// setupF6BCalculator wires a minimal Calculator with a mutable source and
// one worker heartbeat. The Calculator is constructed but NOT Start()ed —
// these tests drive rebalance directly to make the F6-B counter
// transitions deterministic. Calling Start would spawn monitor goroutines
// that race with the test's explicit rebalance calls (the race detector
// flagged this on the integration branch's first full-suite run; the
// monitor goroutine and test both wrote to partitionShrunkObservations
// concurrently and double-advanced the counter).
//
// confirmCount stays a parameter (even though every current call site
// passes 3) so future tests can exercise different confirmation windows.
//
// logger is optional — passing nil keeps the default nop logger; a
// recording logger lets a test assert against log-level behavior (see
// TestCalculator_F6B_SuspiciousObservation_DoesNotLogPartitionRebalanceFailed
// for the regression-pin case).
//
//nolint:unparam // confirmCount is intentionally configurable for future tests
func setupF6BCalculator(t *testing.T, ctx context.Context, src *mutableSource, confirmCount int, logger types.Logger) *Calculator {
	t.Helper()
	_, nc := partitest.StartEmbeddedNATS(t)
	assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-f6b-assignment-"+t.Name())
	heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-f6b-heartbeat-"+t.Name())

	// One worker so calculator has a non-empty active set.
	_, err := heartbeatKV.Put(ctx, "worker-hb.worker-1", []byte(time.Now().Format(time.RFC3339Nano)))
	require.NoError(t, err)

	calc, err := NewCalculator(&Config{
		AssignmentKV:                     assignmentKV,
		HeartbeatKV:                      heartbeatKV,
		AssignmentPrefix:                 "assignment",
		Source:                           src,
		Strategy:                         &mockStrategy{},
		HeartbeatPrefix:                  "worker-hb",
		HeartbeatTTL:                     5 * time.Second,
		EmergencyGracePeriod:             1 * time.Second,
		ColdStartWindow:                  10 * time.Millisecond,
		PlannedScaleWindow:               10 * time.Millisecond,
		Cooldown:                         0, // test drives rebalance directly
		PartitionShrinkConfirmationCount: confirmCount,
		Logger:                           logger,
	})
	require.NoError(t, err)

	// Pump one rebalance with the healthy starting partitions so
	// lastKnownPartitionCount > 0 (the F6-B guard's enabling condition).
	// Direct call — no Start()-spawned monitor goroutines to race with.
	require.NoError(t, calc.rebalance(ctx, "test-seed"))
	require.Greater(t, calc.lastKnownPartitionCount, 0,
		"sanity: the seed rebalance must populate lastKnownPartitionCount")

	return calc
}

// TestCalculator_F6B_EmptyObservation_SuppressedUntilConfirmation drives
// the calculator with N=10 partitions then injects an empty observation
// PartitionShrinkConfirmationCount times in a row. The first
// (count - 1) calls must return errSuspiciousPartitionObservation
// without advancing lastKnownPartitionCount; the count-th call must
// accept the shrink and update the baseline.
func TestCalculator_F6B_EmptyObservation_SuppressedUntilConfirmation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	src := &mutableSource{partitions: makePartitions(10)}
	calc := setupF6BCalculator(t, ctx, src, 3, nil)

	// Seed baseline was 10 partitions. Now go empty.
	src.set(nil)

	// First (count - 1) = 2 calls: suppressed.
	for i := 1; i <= 2; i++ {
		err := calc.rebalance(ctx, "f6b-test")
		require.ErrorIs(t, err, errSuspiciousPartitionObservation,
			"observation %d/3 must surface errSuspiciousPartitionObservation", i)
		require.Equal(t, 10, calc.lastKnownPartitionCount,
			"suppressed observation MUST NOT advance lastKnownPartitionCount")
		require.Equal(t, i, calc.partitionShrunkObservations,
			"counter must advance once per suppressed observation")
	}

	// Third call: confirmation reached; shrink is honored.
	require.NoError(t, calc.rebalance(ctx, "f6b-test"))
	require.Equal(t, 0, calc.lastKnownPartitionCount,
		"after confirmation the baseline updates to the new (empty) shape")
}

// TestCalculator_F6B_SharplyShrunkObservation_Suppressed mirrors the
// empty case for the "sharply shrunk but non-empty" branch.
func TestCalculator_F6B_SharplyShrunkObservation_Suppressed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	src := &mutableSource{partitions: makePartitions(20)}
	calc := setupF6BCalculator(t, ctx, src, 3, nil)

	// 20 → 3 is an 85% drop, well below the default 50% threshold.
	src.set(makePartitions(3))

	for i := 1; i <= 2; i++ {
		err := calc.rebalance(ctx, "f6b-test")
		require.ErrorIs(t, err, errSuspiciousPartitionObservation,
			"observation %d/3 must surface errSuspiciousPartitionObservation", i)
		require.Equal(t, 20, calc.lastKnownPartitionCount,
			"suppressed observation MUST NOT advance lastKnownPartitionCount")
	}

	// Third call: confirmation reached; shrink is honored.
	require.NoError(t, calc.rebalance(ctx, "f6b-test"))
	require.Equal(t, 3, calc.lastKnownPartitionCount,
		"after confirmation the baseline updates to the new (3-partition) shape")
}

// TestCalculator_F6B_LegitimateGrowth_NotGated verifies the guard
// fires ONLY on shrinks. Growth must always be accepted immediately.
func TestCalculator_F6B_LegitimateGrowth_NotGated(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	src := &mutableSource{partitions: makePartitions(10)}
	calc := setupF6BCalculator(t, ctx, src, 3, nil)

	src.set(makePartitions(50))
	require.NoError(t, calc.rebalance(ctx, "f6b-test"),
		"growth must pass through the guard immediately")
	require.Equal(t, 50, calc.lastKnownPartitionCount)
	require.Equal(t, 0, calc.partitionShrunkObservations,
		"growth must keep the suspicious counter at 0")
}

// TestCalculator_F6B_HealingObservationResetsCounter sets up a
// half-shrunk-then-healed sequence: observation 1 is suspicious
// (counter advances), observation 2 is healthy (counter resets to 0).
// A subsequent suspicious observation then needs the FULL confirmation
// window again — the counter must start fresh.
func TestCalculator_F6B_HealingObservationResetsCounter(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	src := &mutableSource{partitions: makePartitions(10)}
	calc := setupF6BCalculator(t, ctx, src, 3, nil)

	src.set(makePartitions(3)) // 70% drop; suspicious
	err := calc.rebalance(ctx, "f6b-test")
	require.ErrorIs(t, err, errSuspiciousPartitionObservation)
	require.Equal(t, 1, calc.partitionShrunkObservations)

	src.set(makePartitions(10)) // healed
	require.NoError(t, calc.rebalance(ctx, "f6b-test"))
	require.Equal(t, 0, calc.partitionShrunkObservations,
		"healing observation must reset the counter to 0")

	src.set(makePartitions(2)) // suspicious again
	err = calc.rebalance(ctx, "f6b-test")
	require.ErrorIs(t, err, errSuspiciousPartitionObservation,
		"new suspicious sequence must start from counter=0, NOT continue the old one")
	require.Equal(t, 1, calc.partitionShrunkObservations)
}

// TestCalculator_F6B_RebalanceCallbacks_HandleSuspiciousObservation
// pins the per-callback contract for errSuspiciousPartitionObservation:
//
//   - handleRebalance (the worker-monitor lifecycle path) swallows the
//     sentinel — its caller is a periodic poll loop that does not need
//     a re-arm signal; the next poll naturally re-observes.
//   - handlePartitionRebalance (the partition-watcher lifecycle path)
//     PROPAGATES the sentinel so triggerPartitionRebalance can re-arm
//     pendingPartitionUpdate; the watcher fires only on partition-list
//     changes and a single N→0 event must not strand the confirmation
//     window. The lifecycle caller (triggerPartitionRebalance) is the
//     sole user of this callback and translates the sentinel to nil
//     after re-arming.
//
// The contracts diverge deliberately — see the per-function Godoc.
func TestCalculator_F6B_RebalanceCallbacks_HandleSuspiciousObservation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	src := &mutableSource{partitions: makePartitions(10)}
	calc := setupF6BCalculator(t, ctx, src, 3, nil)

	src.set(nil)
	require.NoError(t, calc.handleRebalance(ctx, "f6b-test"),
		"handleRebalance must swallow errSuspiciousPartitionObservation "+
			"(the periodic poll caller re-observes naturally)")
	require.ErrorIs(t, calc.handlePartitionRebalance(ctx, "f6b-test"),
		errSuspiciousPartitionObservation,
		"handlePartitionRebalance must propagate errSuspiciousPartitionObservation "+
			"so triggerPartitionRebalance can re-arm pendingPartitionUpdate")
}

// TestCalculator_F6B_SuspiciousObservation_RearmsPendingPartitionUpdate
// pins the contract that watchable-source-driven shrinks converge. The
// watcher emits exactly one signal when the source goes from N→0 (or
// N→tiny); monitorPartitions then clears pendingPartitionUpdate and
// invokes triggerPartitionRebalance. If the F6-B guard suppresses the
// first observation, the next confirmation tick has to come from the
// drainTick — which only fires when pendingPartitionUpdate is true.
//
// Without a re-arm on the suspicious-observation path the watcher
// signal is consumed once, the counter advances to 1, and the
// confirmation window stalls forever (partitions stay at 0 so no
// further watcher events arrive; drainTick has nothing pending to
// drain). This is the exact "fault-papering" pattern the goal anchor
// in docs/plans/self-healing/README.md warns against.
func TestCalculator_F6B_SuspiciousObservation_RearmsPendingPartitionUpdate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	src := &mutableSource{partitions: makePartitions(10)}
	calc := setupF6BCalculator(t, ctx, src, 3, nil)

	// Mirror monitorPartitions's pre-trigger contract: pendingPartitionUpdate
	// is cleared just BEFORE triggerPartitionRebalance runs (see
	// calculator.go's "Immediate trigger supersedes any pending deferred
	// update" comment in monitorPartitions).
	calc.pendingPartitionUpdate.Store(false)

	// Inject a suspicious shrink the F6-B guard will suppress on this
	// first observation: 10 → 0.
	src.set(nil)

	// Drive the rebalance through the same state-machine path
	// monitorPartitions uses.
	err := calc.triggerPartitionRebalance("f6b-test")
	require.NoError(t, err,
		"the lifecycle caller must see a benign nil; suspicious-observation "+
			"suppression is an explicit skip, not a failure")

	// F6-B suppression took effect: counter advanced, baseline unchanged.
	require.Equal(t, 1, calc.partitionShrunkObservations,
		"the suspicious observation must advance the confirmation counter")
	require.Equal(t, 10, calc.lastKnownPartitionCount,
		"a suppressed observation MUST NOT advance the baseline")

	// The bug this test pins. Without a re-arm, the next drainTick has
	// nothing pending, the watcher will not re-fire (partitions did not
	// change again), and the confirmation window stalls forever.
	require.True(t, calc.pendingPartitionUpdate.Load(),
		"F6-B suspicious-observation suppression MUST re-arm "+
			"pendingPartitionUpdate so the drainTick re-attempts and the "+
			"confirmation window converges; without this the watcher-driven "+
			"shrink path papers over a real fault (counter pinned at 1, "+
			"shrink never applied)")
}

// errorRecordingLogger captures Error-level log messages so a test can
// assert that a benign sentinel does NOT trigger an operator-visible
// "failed" line. It defers other levels to a delegate so a developer
// running the test can still see Info/Warn output via -v.
type errorRecordingLogger struct {
	delegate types.Logger
	mu       sync.Mutex
	errors   []string
}

func (l *errorRecordingLogger) Debug(msg string, kv ...any) { l.delegate.Debug(msg, kv...) }
func (l *errorRecordingLogger) Info(msg string, kv ...any)  { l.delegate.Info(msg, kv...) }
func (l *errorRecordingLogger) Warn(msg string, kv ...any)  { l.delegate.Warn(msg, kv...) }
func (l *errorRecordingLogger) Fatal(msg string, kv ...any) { l.delegate.Fatal(msg, kv...) }
func (l *errorRecordingLogger) Error(msg string, kv ...any) {
	l.mu.Lock()
	l.errors = append(l.errors, msg)
	l.mu.Unlock()
	l.delegate.Error(msg, kv...)
}

func (l *errorRecordingLogger) capturedErrors() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.errors))
	copy(out, l.errors)

	return out
}

// TestCalculator_F6B_SuspiciousObservation_DoesNotLogPartitionRebalanceFailed
// regression-pins the contract: a benign suspicious-observation suppression
// must not surface as an Error-level "partition rebalance failed" log line.
// The state-machine's RunClaimedRebalanceErr unconditionally logs every
// non-nil callback error at Error level; when the F6-B re-arm fix made
// handlePartitionRebalance propagate the sentinel instead of swallowing
// it, every suppressed observation began producing a spurious
// "partition rebalance failed" line — false-failure noise for operators
// tailing the worker logs.
//
// The state-machine path must skip the Error log for the
// errSuspiciousPartitionObservation sentinel specifically; the
// observability of the suppression is preserved by the Warn line that
// partitionInputCredibilityGuard already emits ("ignoring empty
// partition observation pending confirmation").
func TestCalculator_F6B_SuspiciousObservation_DoesNotLogPartitionRebalanceFailed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	rec := &errorRecordingLogger{delegate: partitest.NewTestLogger(t)}

	src := &mutableSource{partitions: makePartitions(10)}
	calc := setupF6BCalculator(t, ctx, src, 3, rec)

	// Mirror monitorPartitions's pre-trigger contract.
	calc.pendingPartitionUpdate.Store(false)

	// Inject a suspicious shrink the F6-B guard will suppress.
	src.set(nil)

	err := calc.triggerPartitionRebalance("f6b-test")
	require.NoError(t, err, "the lifecycle caller must see nil; the sentinel is benign")
	require.True(t, calc.pendingPartitionUpdate.Load(),
		"sanity: the re-arm must still happen (the prior fix is intact)")

	// The regression we're pinning.
	for _, msg := range rec.capturedErrors() {
		require.NotEqual(t, "partition rebalance failed", msg,
			"a benign suspicious-observation suppression MUST NOT surface "+
				"as an Error-level 'partition rebalance failed' log; this is "+
				"false-failure noise for operators tailing the worker logs")
	}
}

func makePartitions(n int) []types.Partition {
	out := make([]types.Partition, n)
	for i := range n {
		out[i] = types.Partition{Keys: []string{fmt.Sprintf("p%d", i)}}
	}

	return out
}
