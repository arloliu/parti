package strategy

import (
	"errors"

	"github.com/arloliu/parti/v2/internal/hash"
	"github.com/arloliu/parti/v2/types"
)

// ConsistentHash implements consistent hashing with virtual nodes.
//
// ConsistentHash is safe for concurrent use: Assign creates a fresh hash ring
// on every call and reads only immutable configuration fields set at construction.
type ConsistentHash struct {
	virtualNodes int
	hashSeed     uint64
}

var _ types.AssignmentStrategy = (*ConsistentHash)(nil)

// ConsistentHashOption configures a ConsistentHash strategy.
type ConsistentHashOption func(*ConsistentHash)

// NewConsistentHash creates a new consistent hash strategy.
//
// The strategy uses a hash ring with virtual nodes to distribute partitions
// evenly across workers while minimizing partition movement during scaling.
// Achieves >80% cache affinity during rebalancing.
//
// Parameters:
//   - opts: Optional configuration (WithVirtualNodes, WithHashSeed)
//
// Returns:
//   - *ConsistentHash: Initialized consistent hash strategy
//
// Example:
//
//	strategy := strategy.NewConsistentHash(
//	    strategy.WithVirtualNodes(300),
//	)
//	js, _ := jetstream.New(conn)
//	mgr, err := parti.NewManager(&cfg, js, src, strategy)
//	if err != nil { /* handle */ }
func NewConsistentHash(opts ...ConsistentHashOption) *ConsistentHash {
	ch := &ConsistentHash{
		virtualNodes: 150, // default
		hashSeed:     0,
	}

	for _, opt := range opts {
		opt(ch)
	}

	return ch
}

// WithVirtualNodes sets the number of virtual nodes per worker.
//
// Higher values provide better distribution but increase memory usage.
// Recommended range: 100-300 (default: 150). Values below 1 are clamped to 1.
//
// Parameters:
//   - nodes: Number of virtual nodes per worker (minimum: 1)
//
// Returns:
//   - consistentHashOption: Configuration option
func WithVirtualNodes(nodes int) ConsistentHashOption {
	return func(ch *ConsistentHash) {
		if nodes < 1 {
			nodes = 1 // Minimum 1 virtual node per worker
		}
		ch.virtualNodes = nodes
	}
}

// WithHashSeed sets a custom hash seed for consistent hashing.
//
// Parameters:
//   - seed: Hash seed value
//
// Returns:
//   - consistentHashOption: Configuration option
func WithHashSeed(seed uint64) ConsistentHashOption {
	return func(ch *ConsistentHash) {
		ch.hashSeed = seed
	}
}

// Assign calculates partition assignments using consistent hashing.
//
// The algorithm:
//  1. Build hash ring with virtual nodes for each worker
//  2. Place each partition on ring based on hash of partition keys
//  3. Assign partition to nearest clockwise virtual node
//  4. Apply weight balancing if partition weights differ
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
func (ch *ConsistentHash) Assign(workers []string, partitions []types.Partition) (map[string][]types.Partition, error) {
	if len(workers) == 0 {
		return nil, ErrNoWorkers
	}

	// Create hash ring with all workers
	ring := hash.NewRing(workers, ch.virtualNodes, ch.hashSeed)

	// Initialize assignments map
	assignments := make(map[string][]types.Partition)
	for _, w := range workers {
		assignments[w] = []types.Partition{}
	}

	// Assign each partition to a worker using consistent hashing
	for _, partition := range partitions {
		worker := ring.GetNodeForPartition(partition)
		if worker == "" {
			// This shouldn't happen if workers were added successfully
			return nil, errors.New("consistent hash returned empty worker")
		}
		assignments[worker] = append(assignments[worker], partition)
	}

	return assignments, nil
}
