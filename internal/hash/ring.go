package hash

import (
	"encoding/binary"
	"sort"
	"strings"

	"github.com/arloliu/parti/types"
	"github.com/zeebo/xxh3"
)

// Ring implements a consistent hash ring with virtual nodes.
//
// The ring maps partition keys to workers using consistent hashing, which
// provides stable assignments with minimal changes during scaling events.
type Ring struct {
	// nodes contains all virtual nodes on the ring, sorted by hash
	nodes []virtualNode

	// workerMap maps virtual node index to worker ID
	workerMap map[int]string

	// seed for hash function (0 means no seed)
	seed uint64
}

// virtualNode represents a virtual node on the hash ring.
type virtualNode struct {
	hash     uint64 // Position on the ring
	workerID string // Worker owning this virtual node
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
		nodes:     make([]virtualNode, 0, len(workers)*virtualNodesPerWorker),
		workerMap: make(map[int]string),
		seed:      seed,
	}

	// Add virtual nodes for each worker
	for _, workerID := range workers {
		ring.addWorker(workerID, virtualNodesPerWorker)
	}

	// Sort nodes by hash for binary search
	sort.Slice(ring.nodes, func(i, j int) bool {
		return ring.nodes[i].hash < ring.nodes[j].hash
	})

	return ring
}

// addWorker adds virtual nodes for a worker to the ring.
func (r *Ring) addWorker(workerID string, virtualNodes int) {
	// Pre-allocate buffer for virtual node key to avoid allocations
	buf := make([]byte, 8)

	for i := range virtualNodes {
		// Create unique key for each virtual node
		// Format: "worker-0#<i>" where i is encoded as little-endian uint64
		binary.LittleEndian.PutUint64(buf, uint64(i)) //nolint:gosec // index overflow is not a concern
		vnodeKey := workerID + "#" + string(buf)
		hash := r.hash(vnodeKey)

		r.nodes = append(r.nodes, virtualNode{
			hash:     hash,
			workerID: workerID,
		})
	}
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

	hash := r.hash(key)

	// Binary search for first node >= hash
	idx := sort.Search(len(r.nodes), func(i int) bool {
		return r.nodes[i].hash >= hash
	})

	// Wrap around if we've gone past the end
	if idx >= len(r.nodes) {
		idx = 0
	}

	return r.nodes[idx].workerID
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

	// Concatenate keys with separator
	key := strings.Join(partition.Keys, "/")

	return r.GetNode(key)
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

// Workers returns the list of unique workers on the ring.
func (r *Ring) Workers() []string {
	seen := make(map[string]bool)
	workers := make([]string, 0)

	for _, node := range r.nodes {
		if !seen[node.workerID] {
			seen[node.workerID] = true
			workers = append(workers, node.workerID)
		}
	}

	return workers
}

// Size returns the total number of virtual nodes on the ring.
func (r *Ring) Size() int {
	return len(r.nodes)
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

// GetWorkerWeight returns the total weight assigned to a worker.
func (wr *WeightedRing) GetWorkerWeight(workerID string) int64 {
	return wr.workerWeights[workerID]
}
