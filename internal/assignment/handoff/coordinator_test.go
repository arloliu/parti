package handoff_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/arloliu/parti/internal/assignment/handoff"
	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

// mockUpdater implements handoff.ConsumerUpdater for testing.
type mockUpdater struct {
	calls    atomic.Int64
	lastID   atomic.Value // string
	lastPart atomic.Value // []types.Partition
	failNext atomic.Bool
}

func (m *mockUpdater) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []types.Partition) error {
	if m.failNext.CompareAndSwap(true, false) {
		return context.DeadlineExceeded
	}
	m.calls.Add(1)
	m.lastID.Store(workerID)
	m.lastPart.Store(partitions)
	return nil
}

func TestDirectCoordinator_Apply(t *testing.T) {
	up := &mockUpdater{}
	coord := handoff.New(handoff.Config{ConsumerUpdater: up}, false)

	oldA := types.Assignment{Version: 1}
	newA := types.Assignment{Version: 2, Partitions: []types.Partition{{Keys: []string{"a"}}}}

	require.NoError(t, coord.Apply(context.Background(), "worker-1", oldA, newA))
	require.Equal(t, int64(1), up.calls.Load())
	stored, ok := up.lastPart.Load().([]types.Partition)
	require.True(t, ok)
	require.Len(t, stored, 1)
	require.Equal(t, "a", stored[0].Keys[0])
}

func TestTwoPhaseCoordinator_ApplyDelegates(t *testing.T) {
	t.Skip("moved to twophase_test.go")
}

func TestCoordinator_IdempotentNoUpdater(t *testing.T) {
	coord := handoff.New(handoff.Config{}, true) // no updater -> no-op
	oldA := types.Assignment{Version: 10}
	newA := types.Assignment{Version: 11}
	require.NoError(t, coord.Apply(context.Background(), "worker-9", oldA, newA))
}

func TestCoordinator_ErrorPropagation(t *testing.T) {
	up := &mockUpdater{}
	up.failNext.Store(true)
	coord := handoff.New(handoff.Config{ConsumerUpdater: up}, false)
	oldA := types.Assignment{Version: 20}
	newA := types.Assignment{Version: 21}
	err := coord.Apply(context.Background(), "worker-err", oldA, newA)
	require.Error(t, err)
	require.Equal(t, int64(0), up.calls.Load()) // failed before counting as successful call
}
