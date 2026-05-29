package assignment

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/arloliu/parti/v2/types"
)

// mockStateProvider implements types.StateProvider for testing.
type mockStateProvider struct {
	inGrace atomic.Bool
	state   atomic.Int64 // stores types.State; int64 avoids G115 overflow warnings
}

func (m *mockStateProvider) State() types.State {
	return types.State(m.state.Load())
}

func (m *mockStateProvider) SetState(s types.State) {
	m.state.Store(int64(s))
}

func (m *mockStateProvider) IsInRecoveryGrace() bool {
	return m.inGrace.Load()
}

func (m *mockStateProvider) SetGrace(v bool) {
	m.inGrace.Store(v)
}

// mockSource implements types.PartitionSource for testing.
//
// Provides configurable partition lists and error simulation. The partition
// list is guarded by mu so a test can mutate it (via SetPartitions) while the
// calculator's background rebalance goroutine reads it through List, without
// tripping the race detector. Construct-time field initialization
// (&mockSource{partitions: ...}) needs no lock because it happens-before the
// calculator goroutines start.
type mockSource struct {
	mu         sync.RWMutex
	partitions []types.Partition
	err        error
}

// Start implements PartitionSource.Start.
func (m *mockSource) Start(_ context.Context) error {
	return nil
}

// Stop implements PartitionSource.Stop.
func (m *mockSource) Stop(_ context.Context) error {
	return nil
}

// List returns the configured partitions or error.
func (m *mockSource) List(_ context.Context) ([]types.Partition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.err != nil {
		return nil, m.err
	}

	return m.partitions, nil
}

// SetPartitions replaces the partition list under the lock. Tests that change
// the source after the calculator has started (e.g. partition-refresh
// scenarios) must use this instead of writing the field directly, so the write
// is synchronized against concurrent List reads. It assigns a fresh slice
// rather than mutating in place, so a slice already returned by List stays
// valid to iterate without the lock.
func (m *mockSource) SetPartitions(p []types.Partition) {
	m.mu.Lock()
	m.partitions = p
	m.mu.Unlock()
}

// mockStrategy implements types.AssignmentStrategy for testing.
//
// Provides configurable assignment maps and error simulation.
// If no custom assignments are provided, uses simple round-robin.
type mockStrategy struct {
	assignments map[string][]types.Partition
	err         error
}

// Assign returns the configured assignments or performs round-robin.
func (m *mockStrategy) Assign(workers []string, partitions []types.Partition) (map[string][]types.Partition, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.assignments != nil {
		return m.assignments, nil
	}

	// Simple round-robin assignment
	result := make(map[string][]types.Partition)
	for i, part := range partitions {
		workerIdx := i % len(workers)
		worker := workers[workerIdx]
		result[worker] = append(result[worker], part)
	}

	return result, nil
}
