package hash

import (
	"encoding/binary"
	"slices"

	"github.com/zeebo/xxh3"

	"github.com/arloliu/parti/types"
)

// Ring implements a consistent hash ring with virtual nodes.
//
// The ring maps partition keys to workers using consistent hashing, which
// provides stable assignments with minimal changes during scaling events.
type Ring struct {
	// nodes contains all virtual nodes on the ring, sorted by hash
	nodes []virtualNode

	// workers holds the unique list of workers present on the ring
	workers []string

	// seed for hash function (0 means no seed)
	seed uint64
}

// virtualNode represents a virtual node on the hash ring.
type virtualNode struct {
	hash      uint64 // Position on the ring
	workerID  string // Worker owning this virtual node
	workerIdx int    // Index of the worker in workers slice
}

// NewRing creates a new consistent hash ring.
//
// Parameters:
//   - workers: List of worker IDs to place on the ring
//   - virtualNodesPerWorker: Number of virtual nodes per worker (higher = better distribution)
//   - seed: Seed for hash function (use 0 for random seed, non-zero for deterministic)
//
// Returns:
//   - *Ring: Initialized hash ring
//
// Example:
//
//	ring := hash.NewRing([]string{"worker-0", "worker-1"}, 150, 0)
//	workerID := ring.GetNode(partitionKey)
func NewRing(workers []string, virtualNodesPerWorker int, seed uint64) *Ring {
	ring := &Ring{
		nodes:   make([]virtualNode, 0, len(workers)*virtualNodesPerWorker),
		workers: nil,
		seed:    seed,
	}

	// Deduplicate workers while preserving order
	if len(workers) > 0 {
		seen := make(map[string]struct{}, len(workers))
		uniq := make([]string, 0, len(workers))
		for _, w := range workers {
			if _, ok := seen[w]; ok {
				continue
			}
			seen[w] = struct{}{}
			uniq = append(uniq, w)
		}
		ring.workers = uniq
	} else {
		ring.workers = []string{}
	}

	// Add virtual nodes for each worker, tracking worker index inline
	for i, workerID := range ring.workers {
		ring.addWorker(workerID, i, virtualNodesPerWorker)
	}

	// Sort nodes by hash for binary search
	slices.SortFunc(ring.nodes, func(a, b virtualNode) int {
		if a.hash < b.hash {
			return -1
		}
		if a.hash > b.hash {
			return 1
		}

		return 0
	})

	return ring
}

// GetNode finds the worker responsible for a partition key.
//
// Uses binary search to find the first virtual node whose hash is >= partition hash.
// If no such node exists (partition hash > all nodes), wraps around to first node.
//
// Parameters:
//   - key: Partition key (typically concatenated partition.Keys)
//
// Returns:
//   - string: Worker ID responsible for this key
func (r *Ring) GetNode(key string) string {
	if len(r.nodes) == 0 {
		return ""
	}

	h := r.hash(key)

	return r.getNodeByHash(h)
}

// GetNodeForPartition finds the worker for a partition.
//
// Partition keys are concatenated with "/" separator for hashing.
//
// Parameters:
//   - partition: Partition to assign
//
// Returns:
//   - string: Worker ID responsible for this partition
func (r *Ring) GetNodeForPartition(partition types.Partition) string {
	if len(partition.Keys) == 0 {
		return ""
	}

	// Hash partition keys using Partition.HashIDSeed which folds each key into
	// a single xxh3 64-bit hash without building an intermediate joined string.
	// This is zero-allocation and stable: earlier keys become the seed for later ones.
	h := partition.HashIDSeed(r.seed)

	return r.getNodeByHash(h)
}

// Workers returns the list of unique workers on the ring.
func (r *Ring) Workers() []string {
	// Return a copy to avoid external mutation
	return append([]string(nil), r.workers...)
}

// GetNodeIndexForPartition returns the worker index responsible for the given partition, or -1 if none.
// This avoids an extra map lookup in hot assignment paths by skipping the workerID indirection.
func (r *Ring) GetNodeIndexForPartition(partition types.Partition) int {
	if len(partition.Keys) == 0 || len(r.nodes) == 0 {
		return -1
	}

	h := partition.HashIDSeed(r.seed)
	idx, found := slices.BinarySearchFunc(r.nodes, h, func(node virtualNode, t uint64) int {
		if node.hash < t {
			return -1
		}
		if node.hash > t {
			return 1
		}

		return 0
	})

	if !found && idx >= len(r.nodes) {
		idx = 0
	}

	return r.nodes[idx].workerIdx
}

// Size returns the total number of virtual nodes on the ring.
func (r *Ring) Size() int {
	return len(r.nodes)
}

// addWorker adds virtual nodes for a worker to the ring.
func (r *Ring) addWorker(workerID string, workerIdx int, virtualNodes int) {
	for i := range virtualNodes {
		// Compute hash for (workerID, i) without building a concatenated string.
		// Fold workerID, then vnode index using previous hash as seed for stable distribution.
		var h uint64
		if r.seed != 0 {
			h = xxh3.HashStringSeed(workerID, r.seed)
		} else {
			h = xxh3.HashString(workerID)
		}

		var ib [8]byte
		binary.LittleEndian.PutUint64(ib[:], uint64(i)) //nolint:gosec
		h = xxh3.HashSeed(ib[:], h)

		r.nodes = append(r.nodes, virtualNode{
			hash:      h,
			workerID:  workerID,
			workerIdx: workerIdx,
		})
	}
}

// hash computes a 64-bit hash of the key using XXH3.
//
// Uses XXH3 for both seeded and unseeded hashing for consistent performance.
func (r *Ring) hash(key string) uint64 {
	if r.seed != 0 {
		return xxh3.HashStringSeed(key, r.seed)
	}

	return xxh3.HashString(key)
}

// hashKeys removed: replaced by types.Partition.HashIDSeed for zero-allocation hashing with seed.

// getNodeByHash returns the worker for a given hash value using binary search over the ring.
func (r *Ring) getNodeByHash(target uint64) string {
	// Binary search for first node >= target
	idx, found := slices.BinarySearchFunc(r.nodes, target, func(node virtualNode, t uint64) int {
		if node.hash < t {
			return -1
		}
		if node.hash > t {
			return 1
		}

		return 0
	})

	// If exact match found or idx points to valid position, use it
	// If idx >= len(nodes), wrap around to first node
	if !found && idx >= len(r.nodes) {
		idx = 0
	}

	return r.nodes[idx].workerID
}

// WeightedRing extends Ring with partition weight awareness.
//
// Assigns partitions considering both consistent hashing and partition weights
// to achieve better load balancing when partition costs vary significantly.
type WeightedRing struct {
	*Ring

	// Track actual weight assigned to each worker
	workerWeights map[string]int64
}

// NewWeighted creates a weighted consistent hash ring.
//
// Parameters:
//   - workers: List of worker IDs
//   - virtualNodesPerWorker: Virtual nodes per worker
//   - seed: Hash seed
//
// Returns:
//   - *WeightedRing: Initialized weighted ring
func NewWeighted(workers []string, virtualNodesPerWorker int, seed uint64) *WeightedRing {
	return &WeightedRing{
		Ring:          NewRing(workers, virtualNodesPerWorker, seed),
		workerWeights: make(map[string]int64),
	}
}

// AssignPartitions assigns partitions to workers using weighted consistent hashing.
//
// Algorithm:
//  1. Use consistent hash ring to get initial candidate worker for each partition
//  2. Track cumulative weight assigned to each worker
//  3. If a worker becomes overloaded (weight > avgWeight * 1.15), try next worker on ring
//  4. This balances load while maintaining high cache affinity
//
// Parameters:
//   - partitions: Partitions to assign
//
// Returns:
//   - map[string][]types.Partition: Worker ID → assigned partitions
func (wr *WeightedRing) AssignPartitions(partitions []types.Partition) map[string][]types.Partition {
	assignments := make(map[string][]types.Partition)
	wr.workerWeights = make(map[string]int64)

	if len(partitions) == 0 {
		return assignments
	}

	// Calculate total weight and average per worker
	totalWeight := int64(0)
	for _, p := range partitions {
		weight := p.Weight
		if weight == 0 {
			weight = 100 // default
		}
		totalWeight += weight
	}

	workers := wr.Workers()
	if len(workers) == 0 {
		return assignments
	}

	avgWeight := totalWeight / int64(len(workers))
	maxWeight := avgWeight * 115 / 100 // Allow 15% over average

	// Assign each partition
	for _, partition := range partitions {
		weight := partition.Weight
		if weight == 0 {
			weight = 100
		}

		// Get consistent hash candidate
		workerID := wr.GetNodeForPartition(partition)

		// If adding this partition would overload the candidate, find next available worker
		if wr.workerWeights[workerID]+weight > maxWeight {
			workerID = wr.findLightestWorker()
		}

		// Assign partition
		assignments[workerID] = append(assignments[workerID], partition)
		wr.workerWeights[workerID] += weight
	}

	return assignments
}

// GetWorkerWeight returns the total weight assigned to a worker.
func (wr *WeightedRing) GetWorkerWeight(workerID string) int64 {
	return wr.workerWeights[workerID]
}

// findLightestWorker returns the worker with lowest current weight.
func (wr *WeightedRing) findLightestWorker() string {
	workers := wr.Workers()
	if len(workers) == 0 {
		return ""
	}

	minWorker := workers[0]
	minWeight := wr.workerWeights[minWorker]

	for _, worker := range workers[1:] {
		if wr.workerWeights[worker] < minWeight {
			minWorker = worker
			minWeight = wr.workerWeights[worker]
		}
	}

	return minWorker
}
