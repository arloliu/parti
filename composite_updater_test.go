package parti

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// mockUpdater is a test helper that records calls to UpdateWorkerConsumer.
type mockUpdater struct {
	calls      []updateCall
	shouldFail bool
	failErr    error
}

type updateCall struct {
	workerID   string
	partitions []types.Partition
}

func (m *mockUpdater) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error {
	m.calls = append(m.calls, updateCall{workerID: workerID, partitions: partitions})
	if m.shouldFail {
		return m.failErr
	}
	return nil
}

func TestCompositeConsumerUpdater_FansOutToAllUpdaters(t *testing.T) {
	u1 := &mockUpdater{}
	u2 := &mockUpdater{}
	u3 := &mockUpdater{}

	composite := NewCompositeConsumerUpdater(u1, u2, u3)

	parts := []types.Partition{
		{Keys: []string{"a", "1"}},
		{Keys: []string{"b", "2"}},
	}

	err := composite.UpdateWorkerConsumer(context.Background(), "worker-1", parts)
	require.NoError(t, err)

	// Verify all updaters received the call
	require.Len(t, u1.calls, 1)
	require.Len(t, u2.calls, 1)
	require.Len(t, u3.calls, 1)

	// Verify correct arguments
	require.Equal(t, "worker-1", u1.calls[0].workerID)
	require.Equal(t, parts, u1.calls[0].partitions)

	require.Equal(t, "worker-1", u2.calls[0].workerID)
	require.Equal(t, parts, u2.calls[0].partitions)

	require.Equal(t, "worker-1", u3.calls[0].workerID)
	require.Equal(t, parts, u3.calls[0].partitions)
}

func TestCompositeConsumerUpdater_AggregatesErrors(t *testing.T) {
	err1 := errors.New("updater 1 failed")
	err2 := errors.New("updater 2 failed")

	u1 := &mockUpdater{shouldFail: true, failErr: err1}
	u2 := &mockUpdater{shouldFail: true, failErr: err2}
	u3 := &mockUpdater{} // succeeds

	composite := NewCompositeConsumerUpdater(u1, u2, u3)

	err := composite.UpdateWorkerConsumer(context.Background(), "worker-1", nil)
	require.Error(t, err)

	// All updaters should have been called despite errors
	require.Len(t, u1.calls, 1)
	require.Len(t, u2.calls, 1)
	require.Len(t, u3.calls, 1)

	// Error should contain both error messages
	require.ErrorContains(t, err, "updater 1 failed")
	require.ErrorContains(t, err, "updater 2 failed")
}

func TestCompositeConsumerUpdater_SingleUpdater(t *testing.T) {
	u1 := &mockUpdater{}

	composite := NewCompositeConsumerUpdater(u1)

	parts := []types.Partition{{Keys: []string{"x"}}}
	err := composite.UpdateWorkerConsumer(context.Background(), "w1", parts)
	require.NoError(t, err)

	require.Len(t, u1.calls, 1)
	require.Equal(t, "w1", u1.calls[0].workerID)
}

func TestCompositeConsumerUpdater_NoUpdaters(t *testing.T) {
	composite := NewCompositeConsumerUpdater()

	// Should succeed with no updaters
	err := composite.UpdateWorkerConsumer(context.Background(), "w1", nil)
	require.NoError(t, err)
}

func TestCompositeConsumerUpdater_EmptyPartitions(t *testing.T) {
	u1 := &mockUpdater{}
	u2 := &mockUpdater{}

	composite := NewCompositeConsumerUpdater(u1, u2)

	err := composite.UpdateWorkerConsumer(context.Background(), "worker-1", nil)
	require.NoError(t, err)

	require.Len(t, u1.calls, 1)
	require.Len(t, u2.calls, 1)
	require.Nil(t, u1.calls[0].partitions)
	require.Nil(t, u2.calls[0].partitions)
}

func TestCompositeConsumerUpdater_MultipleCalls(t *testing.T) {
	u1 := &mockUpdater{}
	u2 := &mockUpdater{}

	composite := NewCompositeConsumerUpdater(u1, u2)

	// First call
	parts1 := []types.Partition{{Keys: []string{"a"}}}
	err := composite.UpdateWorkerConsumer(context.Background(), "w1", parts1)
	require.NoError(t, err)

	// Second call with different partitions
	parts2 := []types.Partition{{Keys: []string{"b"}}, {Keys: []string{"c"}}}
	err = composite.UpdateWorkerConsumer(context.Background(), "w1", parts2)
	require.NoError(t, err)

	// Both updaters should have 2 calls
	require.Len(t, u1.calls, 2)
	require.Len(t, u2.calls, 2)

	require.Equal(t, parts1, u1.calls[0].partitions)
	require.Equal(t, parts2, u1.calls[1].partitions)
}

// reportingUpdater is a mockUpdater that additionally implements
// CapabilityReporter, used to verify composite OR semantics.
type reportingUpdater struct {
	mockUpdater
	bits uint32
}

func (r *reportingUpdater) Capabilities() uint32 { return r.bits }

// TestCompositeConsumerUpdater_CapabilitiesORs verifies that
// CompositeConsumerUpdater.Capabilities() ORs across only the children
// that implement CapabilityReporter; non-reporting children contribute 0.
func TestCompositeConsumerUpdater_CapabilitiesORs(t *testing.T) {
	const syntheticBit uint32 = 1 << 31

	t.Run("no children report", func(t *testing.T) {
		u1 := &mockUpdater{}
		u2 := &mockUpdater{}
		composite := NewCompositeConsumerUpdater(u1, u2)

		require.Equal(t, uint32(0), composite.Capabilities(),
			"composite must return 0 when no child implements CapabilityReporter")
	})

	t.Run("one child reports CapProcessingGate", func(t *testing.T) {
		u1 := &mockUpdater{} // not a reporter
		u2 := &reportingUpdater{bits: types.CapProcessingGate}
		composite := NewCompositeConsumerUpdater(u1, u2)

		require.Equal(t, types.CapProcessingGate, composite.Capabilities(),
			"composite must report the bit set by its only reporting child")
	})

	t.Run("multiple reporters ORed", func(t *testing.T) {
		u1 := &mockUpdater{} // not a reporter — contributes 0
		u2 := &reportingUpdater{bits: types.CapProcessingGate}
		u3 := &reportingUpdater{bits: syntheticBit}
		composite := NewCompositeConsumerUpdater(u1, u2, u3)

		got := composite.Capabilities()
		require.Equal(t, types.CapProcessingGate|syntheticBit, got,
			"composite must OR bits across all reporting children")
	})
}

// observingUpdater is a mockUpdater that also implements
// recovery.StreamMissingObserver, used to verify the composite's
// SetOnStreamMissingError forwarding (both at registration time and
// for late-Add'd children).
type observingUpdater struct {
	mockUpdater
	mu       sync.Mutex
	observer func(streamName string, err error)
}

func (o *observingUpdater) SetOnStreamMissingError(fn func(streamName string, err error)) {
	o.mu.Lock()
	o.observer = fn
	o.mu.Unlock()
}

func (o *observingUpdater) currentObserver() func(streamName string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.observer
}

// TestCompositeConsumerUpdater_SetOnStreamMissingError_ForwardsToCurrentChildren
// pins the registration-time forwarding contract: children present at
// the time SetOnStreamMissingError is called must receive the
// callback. A non-observer child contributes nothing and silently
// passes through.
func TestCompositeConsumerUpdater_SetOnStreamMissingError_ForwardsToCurrentChildren(t *testing.T) {
	o1 := &observingUpdater{}
	plain := &mockUpdater{} // no observer interface
	o2 := &observingUpdater{}

	composite := NewCompositeConsumerUpdater(o1, plain, o2)

	called := make([]string, 0, 2)
	var mu sync.Mutex
	cb := func(stream string, _ error) {
		mu.Lock()
		called = append(called, stream)
		mu.Unlock()
	}

	composite.SetOnStreamMissingError(cb)

	require.NotNil(t, o1.currentObserver(),
		"observer-capable child present at registration must receive the callback")
	require.NotNil(t, o2.currentObserver(),
		"observer-capable child present at registration must receive the callback")

	// Fire each child's observer; the composite is opaque to children
	// (they invoke whatever callback the bridge installed).
	o1.currentObserver()("stream-a", errors.New("a"))
	o2.currentObserver()("stream-b", errors.New("b"))

	mu.Lock()
	defer mu.Unlock()
	require.ElementsMatch(t, []string{"stream-a", "stream-b"}, called,
		"the bridge must invoke the composite-supplied callback exactly once per child fire")
}

// TestCompositeConsumerUpdater_Add_AfterSetForwardsObserver pins the
// late-Add forwarding contract: SetOnStreamMissingError is called
// once during Manager.Start, but Add() is part of the public surface
// and may be called afterward (e.g. for an additional BroadcastConsumer).
// A newly-added observer-capable child must inherit the currently-
// registered observer or stream-missing exhaustion from the late child
// would be silently dropped.
func TestCompositeConsumerUpdater_Add_AfterSetForwardsObserver(t *testing.T) {
	composite := NewCompositeConsumerUpdater() // start empty

	var seen atomic.Int32
	composite.SetOnStreamMissingError(func(_ string, _ error) {
		seen.Add(1)
	})

	late := &observingUpdater{}
	composite.Add(late)

	require.NotNil(t, late.currentObserver(),
		"a child Add'd after SetOnStreamMissingError must inherit the recorded observer")

	late.currentObserver()("late", errors.New("boom"))
	require.Equal(t, int32(1), seen.Load(),
		"the late-Add'd child's observer must dispatch to the composite-recorded callback")
}

// TestCompositeConsumerUpdater_Add_PreSetDoesNotForward pins the
// inverse: a child Add'd BEFORE any SetOnStreamMissingError call must
// not receive an observer (there is no observer to inherit).
func TestCompositeConsumerUpdater_Add_PreSetDoesNotForward(t *testing.T) {
	composite := NewCompositeConsumerUpdater()
	early := &observingUpdater{}
	composite.Add(early)

	require.Nil(t, early.currentObserver(),
		"Add before SetOnStreamMissingError must leave the child's observer cleared")
}

// TestCompositeConsumerUpdater_SetOnStreamMissingError_NilClears pins
// the documented nil-clear contract: passing nil must wipe the recorded
// observer AND propagate the clear to current observer-capable children
// so they do not keep dispatching to a previously-installed closure.
func TestCompositeConsumerUpdater_SetOnStreamMissingError_NilClears(t *testing.T) {
	o := &observingUpdater{}
	composite := NewCompositeConsumerUpdater(o)

	composite.SetOnStreamMissingError(func(_ string, _ error) {})
	require.NotNil(t, o.currentObserver(),
		"baseline: child receives the callback after Set")

	composite.SetOnStreamMissingError(nil)
	require.Nil(t, o.currentObserver(),
		"Set(nil) must clear the child's observer")

	// And the recorded observer is cleared too: a subsequent Add must
	// NOT install a stale callback.
	late := &observingUpdater{}
	composite.Add(late)
	require.Nil(t, late.currentObserver(),
		"after Set(nil), a subsequent Add must not inherit a stale observer")
}

// TestCompositeConsumerUpdater_SetOnStreamMissingError_SkipsNonObserverChildren
// pins that non-observer children silently pass through registration
// (i.e. SetOnStreamMissingError does not panic on a mixed slice).
func TestCompositeConsumerUpdater_SetOnStreamMissingError_SkipsNonObserverChildren(t *testing.T) {
	plain1 := &mockUpdater{}
	plain2 := &mockUpdater{}
	composite := NewCompositeConsumerUpdater(plain1, plain2)

	require.NotPanics(t, func() {
		composite.SetOnStreamMissingError(func(_ string, _ error) {})
	}, "SetOnStreamMissingError on an all-non-observer composite must be a silent no-op")
}

func TestCompositeConsumerUpdater_PartialFailure(t *testing.T) {
	errFail := errors.New("second updater failed")

	u1 := &mockUpdater{} // succeeds
	u2 := &mockUpdater{shouldFail: true, failErr: errFail}
	u3 := &mockUpdater{} // succeeds

	composite := NewCompositeConsumerUpdater(u1, u2, u3)

	err := composite.UpdateWorkerConsumer(context.Background(), "w1", nil)
	require.Error(t, err)

	// All should still be called
	require.Len(t, u1.calls, 1)
	require.Len(t, u2.calls, 1)
	require.Len(t, u3.calls, 1)

	// Error should contain the failure
	require.ErrorContains(t, err, "second updater failed")
}
