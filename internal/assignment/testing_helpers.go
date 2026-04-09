package assignment

import (
	"context"
	"sync/atomic"

	"github.com/arloliu/parti/v2/types"
)

// mockStateProvider implements types.StateProvider for testing.
type mockStateProvider struct {
	inGrace atomic.Bool
}

func (m *mockStateProvider) State() types.State {
	return types.StateStable
}

func (m *mockStateProvider) IsInRecoveryGrace() bool {
	return m.inGrace.Load()
}

func (m *mockStateProvider) SetGrace(v bool) {
	m.inGrace.Store(v)
}

// mockSource implements types.PartitionSource for testing.
//
// Provides configurable partition lists and error simulation.
type mockSource struct {
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
	if m.err != nil {
		return nil, m.err
	}

	return m.partitions, nil
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
