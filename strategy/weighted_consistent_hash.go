package strategy

import (
	"slices"
	"sort"
	"strings"

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
	sortedWorkers, assignments, workerLoad := wch.prepareWorkers(workers)

	// Step 3: Handle empty partition list (all workers get empty assignments)
	if len(partitions) == 0 {
		return assignments, nil
	}

	// Step 4: Compute effective weights (applying defaults for zero-weight partitions)
	// and detect if all weights are equal
	effectiveWeights, totalWeight, allEqual := wch.computeEffectiveWeights(partitions)

	// Step 5: Build consistent hash ring for partition-to-worker mapping
	ring := hash.NewRing(sortedWorkers, wch.virtualNodes, wch.hashSeed)

	// Step 6: Fast path for equal weights - use pure consistent hashing
	// This maximizes cache affinity when load balancing isn't needed
	if allEqual {
		if err := wch.assignEqualWeightPartitions(ring, assignments, partitions); err != nil {
			return nil, err
		}

		return assignments, nil
	}

	// Step 7: Compute distribution thresholds for weighted assignment
	// - extremeCutoff: Partitions heavier than this get round-robin treatment
	// - maxWorkerWeight: Soft cap on worker load (can be exceeded if unavoidable)
	thresholds := wch.computeThresholds(totalWeight, len(partitions), len(sortedWorkers), wch.extremeThreshold, wch.overloadThreshold)
	extremes, normals := splitPartitions(partitions, effectiveWeights, thresholds.extremeCutoff)

	// Step 8: Phase 1 - Assign extreme partitions round-robin across workers
	// This ensures heavy partitions are spread evenly before applying consistent hashing
	wch.assignExtremePartitions(extremes, sortedWorkers, assignments, workerLoad, len(partitions), thresholds.extremeCutoff)

	// Step 9: Phase 2 - Assign normal partitions using consistent hashing with load cap
	// Preserves cache affinity while preventing individual workers from becoming overloaded
	overflowCount, err := wch.assignNormalPartitions(normals, ring, sortedWorkers, assignments, workerLoad, thresholds.maxWorkerWeight)
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

	return assignments, nil
}

func (wch *WeightedConsistentHash) prepareWorkers(workers []string) ([]string, map[string][]types.Partition, map[string]int64) {
	sortedWorkers := append([]string(nil), workers...)
	sort.Strings(sortedWorkers)

	assignments := make(map[string][]types.Partition, len(sortedWorkers))
	workerLoad := make(map[string]int64, len(sortedWorkers))
	for _, worker := range sortedWorkers {
		assignments[worker] = []types.Partition{}
		workerLoad[worker] = 0
	}

	return sortedWorkers, assignments, workerLoad
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

func (wch *WeightedConsistentHash) assignEqualWeightPartitions(
	ring *hash.Ring,
	assignments map[string][]types.Partition,
	partitions []types.Partition,
) error {
	for _, partition := range partitions {
		worker := ring.GetNodeForPartition(partition)
		if worker == "" {
			return ErrNoWorkers
		}
		assignments[worker] = append(assignments[worker], partition)
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

func (wch *WeightedConsistentHash) assignExtremePartitions(
	extremes []partitionEntry,
	workers []string,
	assignments map[string][]types.Partition,
	workerLoad map[string]int64,
	totalPartitions int,
	extremeCutoff float64,
) {
	if len(extremes) == 0 {
		return
	}

	slices.SortFunc(extremes, func(a, b partitionEntry) int {
		if a.weight == b.weight {
			return strings.Compare(joinKeys(a.partition), joinKeys(b.partition))
		}

		if a.weight > b.weight {
			return -1
		}

		return 1
	})

	for idx, entry := range extremes {
		worker := workers[idx%len(workers)]
		assignments[worker] = append(assignments[worker], entry.partition)
		workerLoad[worker] += entry.weight
	}

	wch.logger.Debug(
		"weighted consistent hash detected extreme partitions",
		"extreme_partitions", len(extremes),
		"total_partitions", totalPartitions,
		"extreme_threshold", extremeCutoff,
	)
}

func (wch *WeightedConsistentHash) assignNormalPartitions(
	normals []partitionEntry,
	ring *hash.Ring,
	workers []string,
	assignments map[string][]types.Partition,
	workerLoad map[string]int64,
	maxWorkerWeight float64,
) (int, error) {
	overflowCount := 0

	// Iterate in original discovery order so consistent-hash affinity remains predictable.
	for _, entry := range normals {
		worker := ring.GetNodeForPartition(entry.partition)
		if worker == "" {
			return 0, ErrNoWorkers
		}

		if maxWorkerWeight > 0 && float64(workerLoad[worker]+entry.weight) > maxWorkerWeight {
			worker = wch.findLightestWorker(workers, workerLoad)
			if maxWorkerWeight > 0 && float64(workerLoad[worker]+entry.weight) > maxWorkerWeight {
				overflowCount++
			}
		}

		assignments[worker] = append(assignments[worker], entry.partition)
		workerLoad[worker] += entry.weight
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

func (wch *WeightedConsistentHash) findLightestWorker(workers []string, workerLoad map[string]int64) string {
	lightest := workers[0]
	minLoad := workerLoad[lightest]

	for _, worker := range workers[1:] {
		load := workerLoad[worker]
		if load < minLoad || (load == minLoad && worker < lightest) {
			lightest = worker
			minLoad = load
		}
	}

	return lightest
}

func joinKeys(partition types.Partition) string {
	if len(partition.Keys) == 0 {
		return ""
	}

	return strings.Join(partition.Keys, "\x00")
}
