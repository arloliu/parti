// Package strategy provides built-in assignment strategy implementations.package strategy

// Assignment strategies determine how partitions are distributed across workers.
// The package includes two built-in strategies:
//
//   - ConsistentHash: Weighted consistent hashing with virtual nodes (recommended)
//   - RoundRobin: Simple round-robin distribution
//
// Custom strategies can be implemented by satisfying the parti.AssignmentStrategy interface.
package strategy
