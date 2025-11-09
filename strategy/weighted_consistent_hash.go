package strategy

import (
	"slices"
	"sync"

	"github.com/arloliu/parti/internal/hash"
	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/types"
)

const (
	defaultVirtualNodes      = 150
	defaultOverloadThreshold = 1.3
	defaultExtremeThreshold  = 2.0
	defaultWeight            = int64(1)

	minOverloadThreshold = 1.15
	minExtremeThreshold  = 1.5
)

// WeightedConsistentHash implements weighted consistent hashing with extreme partition handling.
type WeightedConsistentHash struct {
	virtualNodes      int
	hashSeed          uint64
	overloadThreshold float64
	extremeThreshold  float64
	defaultWeight     int64
	logger            types.Logger

	// ring cache to avoid rebuilding when worker set and config are unchanged
	cacheMu       sync.RWMutex
	cachedWorkers []string
	cachedVirtual int
	cachedSeed    uint64
	cachedRing    *hash.Ring
}

var _ types.AssignmentStrategy = (*WeightedConsistentHash)(nil)

// WeightedConsistentHashOption configures a WeightedConsistentHash strategy.
type WeightedConsistentHashOption func(*WeightedConsistentHash)

type partitionEntry struct {
	partition types.Partition
	weight    int64
	index     int
}

type distributionThresholds struct {
	extremeCutoff   float64
	maxWorkerWeight float64
}

// NewWeightedConsistentHash creates a new weighted consistent hash strategy.
//
// Parameters:
//   - opts: Optional configuration (WithWeightedVirtualNodes, WithWeightedHashSeed, WithOverloadThreshold, WithExtremeThreshold, WithDefaultWeight, WithWeightedLogger)
//
// Returns:
//   - *WeightedConsistentHash: Initialized weighted consistent hash strategy ready for use.
func NewWeightedConsistentHash(opts ...WeightedConsistentHashOption) *WeightedConsistentHash {
	wch := &WeightedConsistentHash{
		virtualNodes:      defaultVirtualNodes,
		hashSeed:          0,
		overloadThreshold: defaultOverloadThreshold,
		extremeThreshold:  defaultExtremeThreshold,
		defaultWeight:     defaultWeight,
		logger:            logging.NewNop(),
	}

	for _, opt := range opts {
		if opt != nil {
			opt(wch)
		}
	}

	wch.normalizeConfig()

	return wch
}

// WithWeightedVirtualNodes sets the number of virtual nodes per worker.
func WithWeightedVirtualNodes(nodes int) WeightedConsistentHashOption {
	return func(wch *WeightedConsistentHash) {
		wch.virtualNodes = nodes
	}
}

// WithWeightedHashSeed sets a custom hash seed for consistent hashing.
func WithWeightedHashSeed(seed uint64) WeightedConsistentHashOption {
	return func(wch *WeightedConsistentHash) {
		wch.hashSeed = seed
	}
}

// WithOverloadThreshold sets the maximum allowed load variance per worker.
func WithOverloadThreshold(threshold float64) WeightedConsistentHashOption {
	return func(wch *WeightedConsistentHash) {
		wch.overloadThreshold = threshold
	}
}

// WithExtremeThreshold sets the multiplier used to classify extreme partitions.
func WithExtremeThreshold(threshold float64) WeightedConsistentHashOption {
	return func(wch *WeightedConsistentHash) {
		wch.extremeThreshold = threshold
	}
}

// WithDefaultWeight sets the default weight applied when a partition reports zero weight.
func WithDefaultWeight(weight int64) WeightedConsistentHashOption {
	return func(wch *WeightedConsistentHash) {
		wch.defaultWeight = weight
	}
}

// WithWeightedLogger sets the logger used for configuration warnings and debug diagnostics.
func WithWeightedLogger(logger types.Logger) WeightedConsistentHashOption {
	return func(wch *WeightedConsistentHash) {
		wch.logger = logger
	}
}

// Assign calculates partition assignments using weighted consistent hashing with extreme partition handling.
//
// The algorithm balances two competing goals:
//  1. Cache affinity - Keep partitions on the same workers across rebalancing (via consistent hashing)
//  2. Load balance - Prevent workers from being overloaded by heavy partitions
//
// Algorithm Overview:
//
//  1. Validation - Check for workers and normalize partition weights
//  2. Equal-weight fast path - When all partitions have the same weight, use pure consistent hashing
//  3. Two-phase weighted assignment:
//     a. Extreme partitions - Distribute heavy partitions (weight > avgWeight * extremeThreshold) round-robin
//     b. Normal partitions - Assign remaining partitions using consistent hashing with soft load cap
//
// The soft load cap (avgWeight * overloadThreshold) allows some imbalance to preserve cache affinity,
// but reassigns partitions to the lightest worker when the cap is exceeded.
//
// Parameters:
//   - workers: List of worker IDs to assign partitions to
//   - partitions: List of partitions to distribute across workers
//
// Returns:
//   - map[string][]types.Partition: Worker ID → assigned partitions
//   - error: ErrNoWorkers if workers list is empty, nil otherwise
//
// Example:
//
//	strategy := NewWeightedConsistentHash(
//	    WithOverloadThreshold(1.3),     // Allow 30% overload
//	    WithExtremeThreshold(2.0),      // Partitions 2x average are "extreme"
//	)
//	assignments, err := strategy.Assign(workers, partitions)
//	if err != nil {
//	    log.Fatal(err)
//	}
func (wch *WeightedConsistentHash) Assign(workers []string, partitions []types.Partition) (map[string][]types.Partition, error) {
	// Step 1: Validate that we have workers to assign to
	if len(workers) == 0 {
		return nil, ErrNoWorkers
	}

	// Step 2: Sort workers for deterministic assignment and initialize tracking structures
	sortedWorkers, workerLoad, buckets := wch.prepare(workers, len(partitions))

	// Step 3: Handle empty partition list (all workers get empty assignments)
	if len(partitions) == 0 {
		return convertBucketsToMap(sortedWorkers, buckets), nil
	}

	// Step 4: Compute effective weights (applying defaults for zero-weight partitions)
	// and detect if all weights are equal
	effectiveWeights, totalWeight, allEqual := wch.computeEffectiveWeights(partitions)

	// Step 5: Build or reuse consistent hash ring for partition-to-worker mapping
	ring := wch.getOrBuildRing(sortedWorkers)

	// Step 6: Fast path for equal weights - use pure consistent hashing
	// This maximizes cache affinity when load balancing isn't needed
	if allEqual {
		if err := wch.assignEqualWeightPartitionsByRingIndex(ring, buckets, partitions); err != nil {
			return nil, err
		}

		return convertBucketsToMap(sortedWorkers, buckets), nil
	}

	// Step 7: Compute distribution thresholds for weighted assignment
	// - extremeCutoff: Partitions heavier than this get round-robin treatment
	// - maxWorkerWeight: Soft cap on worker load (can be exceeded if unavoidable)
	thresholds := wch.computeThresholds(totalWeight, len(partitions), len(sortedWorkers), wch.extremeThreshold, wch.overloadThreshold)
	extremes, normals := splitPartitions(partitions, effectiveWeights, thresholds.extremeCutoff)

	// Step 8: Phase 1 - Assign extreme partitions round-robin across workers
	// This ensures heavy partitions are spread evenly before applying consistent hashing
	wch.assignExtremePartitionsIndexed(extremes, sortedWorkers, buckets, workerLoad, len(partitions), thresholds.extremeCutoff)

	// Step 9: Phase 2 - Assign normal partitions using consistent hashing with load cap
	// Preserves cache affinity while preventing individual workers from becoming overloaded
	overflowCount, err := wch.assignNormalPartitionsByRingIndex(normals, ring, buckets, workerLoad, thresholds.maxWorkerWeight)
	if err != nil {
		return nil, err
	}

	// Step 10: Log diagnostic info if soft cap was exceeded
	// This helps operators tune thresholds for their workload
	if overflowCount > 0 {
		wch.logger.Debug(
			"weighted consistent hash exceeded soft cap",
			"overflow_count", overflowCount,
			"max_worker_weight", thresholds.maxWorkerWeight,
			"total_weight", totalWeight,
		)
	}

	return convertBucketsToMap(sortedWorkers, buckets), nil
}

// getOrBuildRing returns a cached ring if the sorted workers and config match; otherwise builds and caches a new ring.
func (wch *WeightedConsistentHash) getOrBuildRing(sortedWorkers []string) *hash.Ring {
	wch.cacheMu.RLock()
	cached := wch.cachedRing
	if cached != nil && wch.cachedVirtual == wch.virtualNodes && wch.cachedSeed == wch.hashSeed && slices.Equal(wch.cachedWorkers, sortedWorkers) {
		wch.cacheMu.RUnlock()
		return cached
	}
	wch.cacheMu.RUnlock()

	// Build new ring outside lock
	newRing := hash.NewRing(sortedWorkers, wch.virtualNodes, wch.hashSeed)

	// Cache it
	wch.cacheMu.Lock()
	// Store a copy of workers to avoid external mutation risks
	wch.cachedWorkers = append([]string(nil), sortedWorkers...)
	wch.cachedVirtual = wch.virtualNodes
	wch.cachedSeed = wch.hashSeed
	wch.cachedRing = newRing
	wch.cacheMu.Unlock()

	return newRing
}

func (wch *WeightedConsistentHash) prepare(workers []string, partitionsLen int) ([]string, []int64, [][]types.Partition) {
	sortedWorkers := append([]string(nil), workers...)
	slices.Sort(sortedWorkers)

	workerLoad := make([]int64, len(sortedWorkers))

	// Buckets for per-worker assignments; pre-size capacity heuristically to reduce growslice.
	buckets := make([][]types.Partition, len(sortedWorkers))
	// Heuristic: assume roughly even distribution, allocate ceil(partitions/workers) per bucket.
	capPer := 0
	if len(sortedWorkers) > 0 && partitionsLen > 0 {
		capPer = (partitionsLen + len(sortedWorkers) - 1) / len(sortedWorkers)
		if capPer < 1 {
			capPer = 1
		}
	}
	for i := range buckets {
		if capPer > 0 {
			buckets[i] = make([]types.Partition, 0, capPer)
		} else {
			buckets[i] = make([]types.Partition, 0)
		}
	}

	return sortedWorkers, workerLoad, buckets
}

func (wch *WeightedConsistentHash) computeEffectiveWeights(partitions []types.Partition) ([]int64, int64, bool) {
	effectiveWeights := make([]int64, len(partitions))
	totalWeight := int64(0)
	allEqual := true
	var firstWeight int64

	for i, partition := range partitions {
		weight := wch.effectiveWeight(partition.Weight)
		effectiveWeights[i] = weight
		totalWeight += weight

		if i == 0 {
			firstWeight = weight

			continue
		}

		if weight != firstWeight {
			allEqual = false
		}
	}

	return effectiveWeights, totalWeight, allEqual
}

func (wch *WeightedConsistentHash) assignEqualWeightPartitionsByRingIndex(
	ring *hash.Ring,
	buckets [][]types.Partition,
	partitions []types.Partition,
) error {
	for _, partition := range partitions {
		idx := ring.GetNodeIndexForPartition(partition)
		if idx < 0 {
			return ErrNoWorkers
		}
		buckets[idx] = append(buckets[idx], partition)
	}

	return nil
}

func (wch *WeightedConsistentHash) computeThresholds(
	totalWeight int64,
	partitionCount, workerCount int,
	extremeMultiplier, overloadMultiplier float64,
) distributionThresholds {
	avgPartitionWeight := float64(0)
	if partitionCount > 0 {
		avgPartitionWeight = float64(totalWeight) / float64(partitionCount)
	}

	avgWorkerWeight := float64(0)
	if workerCount > 0 {
		avgWorkerWeight = float64(totalWeight) / float64(workerCount)
	}

	return distributionThresholds{
		extremeCutoff:   avgPartitionWeight * extremeMultiplier,
		maxWorkerWeight: avgWorkerWeight * overloadMultiplier,
	}
}

func splitPartitions(
	partitions []types.Partition,
	effectiveWeights []int64,
	extremeCutoff float64,
) (extremes []partitionEntry, normals []partitionEntry) {
	extremes = make([]partitionEntry, 0)
	normals = make([]partitionEntry, 0, len(partitions))

	for idx, partition := range partitions {
		entry := partitionEntry{partition: partition, weight: effectiveWeights[idx], index: idx}
		if extremeCutoff > 0 && float64(entry.weight) > extremeCutoff {
			extremes = append(extremes, entry)

			continue
		}

		normals = append(normals, entry)
	}

	return extremes, normals
}

func (wch *WeightedConsistentHash) assignExtremePartitionsIndexed(
	extremes []partitionEntry,
	workers []string,
	buckets [][]types.Partition,
	workerLoad []int64,
	totalPartitions int,
	extremeCutoff float64,
) {
	if len(extremes) == 0 {
		return
	}

	slices.SortFunc(extremes, func(a, b partitionEntry) int {
		if a.weight == b.weight {
			return a.partition.Compare(b.partition)
		}
		if a.weight > b.weight {
			return -1
		}

		return 1
	})

	for i, entry := range extremes {
		widx := i % len(workers)
		buckets[widx] = append(buckets[widx], entry.partition)
		workerLoad[widx] += entry.weight
	}

	wch.logger.Debug(
		"weighted consistent hash detected extreme partitions",
		"extreme_partitions", len(extremes),
		"total_partitions", totalPartitions,
		"extreme_threshold", extremeCutoff,
	)
}

func (wch *WeightedConsistentHash) assignNormalPartitionsByRingIndex(
	normals []partitionEntry,
	ring *hash.Ring,
	buckets [][]types.Partition,
	workerLoad []int64,
	maxWorkerWeight float64,
) (int, error) {
	overflowCount := 0

	// Iterate in original discovery order so consistent-hash affinity remains predictable.
	for _, entry := range normals {
		idx := ring.GetNodeIndexForPartition(entry.partition)
		if idx < 0 {
			return 0, ErrNoWorkers
		}

		if maxWorkerWeight > 0 && float64(workerLoad[idx]+entry.weight) > maxWorkerWeight {
			idx = wch.findLightestWorkerIndexed(workerLoad)
			if maxWorkerWeight > 0 && float64(workerLoad[idx]+entry.weight) > maxWorkerWeight {
				overflowCount++
			}
		}

		buckets[idx] = append(buckets[idx], entry.partition)
		workerLoad[idx] += entry.weight
	}

	return overflowCount, nil
}

func (wch *WeightedConsistentHash) normalizeConfig() {
	if wch.logger == nil {
		wch.logger = logging.NewNop()
	}

	if wch.virtualNodes < 1 {
		wch.logger.Warn("virtual nodes must be positive; clamping to 1", "provided", wch.virtualNodes, "using", 1)
		wch.virtualNodes = 1
	}

	if wch.overloadThreshold < minOverloadThreshold {
		wch.logger.Warn("overload threshold too low; clamping to minimum", "provided", wch.overloadThreshold, "using", minOverloadThreshold)
		wch.overloadThreshold = minOverloadThreshold
	}

	if wch.extremeThreshold < minExtremeThreshold {
		wch.logger.Warn("extreme threshold too low; clamping to minimum", "provided", wch.extremeThreshold, "using", minExtremeThreshold)
		wch.extremeThreshold = minExtremeThreshold
	}

	if wch.defaultWeight < 1 {
		wch.logger.Warn("default weight must be positive; clamping to 1", "provided", wch.defaultWeight, "using", 1)
		wch.defaultWeight = 1
	}
}

func (wch *WeightedConsistentHash) effectiveWeight(weight int64) int64 {
	if weight > 0 {
		return weight
	}

	return wch.defaultWeight
}

func (wch *WeightedConsistentHash) findLightestWorkerIndexed(workerLoad []int64) int {
	lightestIdx := 0
	minLoad := workerLoad[0]

	for i := 1; i < len(workerLoad); i++ {
		load := workerLoad[i]
		if load < minLoad || (load == minLoad && i < lightestIdx) {
			lightestIdx = i
			minLoad = load
		}
	}

	return lightestIdx
}

func convertBucketsToMap(workers []string, buckets [][]types.Partition) map[string][]types.Partition {
	res := make(map[string][]types.Partition, len(workers))
	for i, w := range workers {
		res[w] = buckets[i]
	}

	return res
}
