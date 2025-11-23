package types

// AssignmentStrategy calculates partition assignments for a set of workers.
//
// Strategies implement different assignment algorithms:
//   - ConsistentHash: Weighted consistent hashing with virtual nodes (>80% cache affinity)
//   - RoundRobin: Simple round-robin distribution (no cache affinity)
//   - Custom: User-defined algorithms
//
// The leader worker calls Assign during:
//   - Initial assignment calculation
//   - Worker scaling (join/leave)
//   - Partition changes (add/remove)
//   - Manual rebalancing
//
// Strategy implementations should:
//   - Be deterministic (same input → same output)
//   - Handle edge cases (no workers, no partitions, weights)
//   - Run quickly (called on hot path)
//   - Be stateless (no side effects)
type AssignmentStrategy interface {
	// Assign calculates partition assignments for the given workers.
	//
	// The strategy should distribute partitions across workers considering:
	//   - Partition weights (if supported)
	//   - Load balancing (even distribution)
	//   - Cache affinity (minimize reassignment)
	//
	// Parameters:
	//   - workers: List of worker IDs to assign partitions to
	//   - partitions: List of partitions to assign
	//
	// Returns:
	//   - map[string][]Partition: Map from workerID to assigned partitions
	//   - error: Assignment error (e.g., ErrNoWorkersAvailable)
	Assign(workers []string, partitions []Partition) (map[string][]Partition, error)
}
