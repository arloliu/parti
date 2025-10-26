package strategy

import (
	"errors"

	"github.com/arloliu/parti/types"
)

// RoundRobin implements simple round-robin partition assignment.
type RoundRobin struct{}

var _ types.AssignmentStrategy = (*RoundRobin)(nil)

// NewRoundRobin creates a new round-robin strategy.
//
// The strategy distributes partitions evenly across workers in a simple
// round-robin fashion. This provides predictable assignment but does not
// preserve cache affinity during scaling.
//
// Returns:
//   - *RoundRobin: Initialized round-robin strategy
//
// Example:
//
//	strategy := strategy.NewRoundRobin()
//	mgr := parti.NewManager(&cfg, conn, src, parti.WithStrategy(strategy))
func NewRoundRobin() *RoundRobin {
	return &RoundRobin{}
}

// Assign calculates partition assignments using round-robin distribution.
//
// The algorithm:
//  1. Sort workers and partitions for deterministic assignment
//  2. Distribute partitions evenly in round-robin fashion
//
// Parameters:
//   - workers: List of worker IDs (e.g., ["worker-0", "worker-1"])
//   - partitions: List of partitions to assign
//
// Returns:
//   - map[string][]types.Partition: Map from workerID to assigned partitions
//   - error: Assignment error (e.g., no workers available)
//
// Example:
//
//	assignments, err := strategy.Assign(
//	    []string{"worker-0", "worker-1"},
//	    partitions,
//	)
func (rr *RoundRobin) Assign(workers []string, partitions []types.Partition) (map[string][]types.Partition, error) {
	if len(workers) == 0 {
		return nil, errors.New("no workers available for assignment")
	}

	// Initialize assignments map
	assignments := make(map[string][]types.Partition)
	for _, w := range workers {
		assignments[w] = []types.Partition{}
	}

	// Distribute partitions round-robin across workers
	for i, p := range partitions {
		workerIdx := i % len(workers)
		worker := workers[workerIdx]
		assignments[worker] = append(assignments[worker], p)
	}

	return assignments, nil
}
