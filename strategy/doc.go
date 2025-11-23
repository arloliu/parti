// Package strategy provides built-in assignment strategy implementations.
//
// Assignment strategies determine how partitions are distributed across workers.
// The package includes three built-in strategies:
//
//   - WeightedConsistentHash: Weighted consistent hashing with extreme partition handling and soft load caps (recommended for weighted workloads)
//   - ConsistentHash: Standard consistent hashing with virtual nodes (recommended for equal-weight partitions)
//   - RoundRobin: Simple round-robin distribution
//
// # Strategy Selection Guide
//
// WeightedConsistentHash:
//   - Use when partitions have significantly different processing costs
//   - Balances cache affinity with load distribution
//   - Handles extreme partitions (2x+ average weight) via round-robin
//   - Applies soft load caps to prevent worker overload
//   - Configuration: virtual nodes, hash seed, overload threshold, extreme threshold
//
// ConsistentHash:
//   - Use when all partitions have equal or similar weights
//   - Maximizes cache affinity during scaling events
//   - Minimal configuration: virtual nodes, hash seed
//
// RoundRobin:
//   - Use for simple, stateless workloads
//   - Guarantees even distribution
//   - No cache affinity preservation
//
// Custom strategies can be implemented by satisfying the types.AssignmentStrategy interface.
package strategy
