package parti

import (
	"context"
	"errors"
	"testing"

	"github.com/arloliu/parti/types"
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
