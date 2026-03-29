package strategy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

type recordingLogger struct {
	debugMessages []string
	warnMessages  []string
}

func (l *recordingLogger) Debug(msg string, _ ...any) {
	l.debugMessages = append(l.debugMessages, msg)
}

func (l *recordingLogger) Info(string, ...any) {}

func (l *recordingLogger) Warn(msg string, _ ...any) {
	l.warnMessages = append(l.warnMessages, msg)
}

func (l *recordingLogger) Error(string, ...any) {}

func (l *recordingLogger) Fatal(string, ...any) {}

func readDevicePartitions(t *testing.T, path string) []types.Partition {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err, "failed to read device partition file")

	type deviceEntry struct {
		Key    string `json:"key"`
		Weight int64  `json:"weight"`
	}

	type deviceData struct {
		Partitions []deviceEntry `json:"partitions"`
	}

	var td deviceData
	err = json.Unmarshal(data, &td)
	require.NoError(t, err, "failed to unmarshal device partition data")

	partitions := make([]types.Partition, len(td.Partitions))
	for i, partition := range td.Partitions {
		partitions[i] = types.Partition{
			Keys:   []string{partition.Key},
			Weight: partition.Weight,
		}
	}

	return partitions
}

func TestWeightedConsistentHash_NoWorkers(t *testing.T) {
	strategy := NewWeightedConsistentHash()

	_, err := strategy.Assign(nil, nil)

	require.ErrorIs(t, err, ErrNoWorkers)
}

func TestWeightedConsistentHash_ZeroPartitions(t *testing.T) {
	workers := []string{"worker-0", "worker-1"}
	strategy := NewWeightedConsistentHash()

	assignments, err := strategy.Assign(workers, nil)

	require.NoError(t, err)
	require.Len(t, assignments, len(workers))
	for _, worker := range workers {
		require.Contains(t, assignments, worker)
		require.Len(t, assignments[worker], 0)
	}
}

func TestWeightedConsistentHash_EqualWeightsMatchesConsistentHash(t *testing.T) {
	workers := []string{"worker-0", "worker-1", "worker-2"}
	partitions := []types.Partition{
		{Keys: []string{"p-0"}, Weight: 100},
		{Keys: []string{"p-1"}, Weight: 100},
		{Keys: []string{"p-2"}, Weight: 100},
		{Keys: []string{"p-3"}, Weight: 100},
	}

	seed := uint64(42)

	weighted := NewWeightedConsistentHash(
		WithWeightedHashSeed(seed),
	)
	consistent := NewConsistentHash(
		WithHashSeed(seed),
	)

	weightedAssignments, err := weighted.Assign(workers, partitions)
	require.NoError(t, err)

	consistentAssignments, err := consistent.Assign(workers, partitions)
	require.NoError(t, err)

	require.Equal(t, consistentAssignments, weightedAssignments)
}

func TestWeightedConsistentHash_DistributesExtremesEvenly(t *testing.T) {
	workers := []string{"worker-0", "worker-1", "worker-2", "worker-3"}
	partitions := make([]types.Partition, 0, 16)
	partitions = append(partitions,
		types.Partition{Keys: []string{"extreme-0"}, Weight: 5000},
		types.Partition{Keys: []string{"extreme-1"}, Weight: 4000},
		types.Partition{Keys: []string{"extreme-2"}, Weight: 3000},
		types.Partition{Keys: []string{"extreme-3"}, Weight: 2000},
	)

	for i := range 12 {
		partitions = append(partitions, types.Partition{
			Keys:   []string{fmt.Sprintf("normal-%02d", i)},
			Weight: 100,
		})
	}

	// Use a lower extreme threshold to ensure our test partitions are treated as extreme.
	// Avg weight = (14000 + 1200) / 16 = 950.
	// With threshold 2.0, cutoff = 1900.
	// All partitions >= 2000 will be extreme.
	strategy := NewWeightedConsistentHash(WithExtremeThreshold(2.0))

	assignments, err := strategy.Assign(workers, partitions)
	require.NoError(t, err)

	extremeAssignments := make(map[string]int)
	for worker, parts := range assignments {
		for _, partition := range parts {
			if partition.Weight >= 2000 {
				extremeAssignments[worker]++
			}
		}
	}

	require.Len(t, extremeAssignments, len(workers))
	for worker, count := range extremeAssignments {
		require.Equal(t, 1, count, "worker %s should have exactly one extreme partition", worker)
	}
}

func TestWeightedConsistentHash_LogsOverflow(t *testing.T) {
	logger := &recordingLogger{}

	strategy := NewWeightedConsistentHash(
		WithWeightedLogger(logger),
		WithOverloadThreshold(1.15),
		WithExtremeThreshold(10.0),
	)

	workers := []string{"worker-0", "worker-1"}
	partitions := []types.Partition{
		{Keys: []string{"heavy"}, Weight: 10000},
		{Keys: []string{"light-0"}, Weight: 1000},
		{Keys: []string{"light-1"}, Weight: 1000},
	}

	_, err := strategy.Assign(workers, partitions)
	require.NoError(t, err)

	require.Contains(t, logger.debugMessages, "weighted consistent hash exceeded soft cap")
}

func TestWeightedConsistentHash_ConfigValidation(t *testing.T) {
	logger := logging.NewTest(t)

	strategy := NewWeightedConsistentHash(
		WithWeightedLogger(logger),
		WithWeightedVirtualNodes(0),
		WithOverloadThreshold(0.5),
		WithExtremeThreshold(1.1),
		WithDefaultWeight(0),
	)

	require.Equal(t, 1, strategy.virtualNodes)
	require.Equal(t, minOverloadThreshold, strategy.overloadThreshold)
	require.Equal(t, minExtremeThreshold, strategy.extremeThreshold)
	require.Equal(t, int64(1), strategy.defaultWeight)
}

func TestWeightedConsistentHash_WithDeviceData(t *testing.T) {
	// Convert to types.Partition
	partitions := readDevicePartitions(t, filepath.Join("testdata", "partition_by_devices.json"))
	totalWeight := int64(0)
	for _, p := range partitions {
		totalWeight += p.Weight
	}

	t.Logf("Loaded %d partitions with total weight %d", len(partitions), totalWeight)

	// Calculate average partition weight for extreme detection
	avgPartitionWeight := float64(totalWeight) / float64(len(partitions))

	// Tune parameters for the test to achieve "close to 1" distribution for extreme partitions.
	// The dataset contains some very heavy partitions (approx 1.5x average worker load).
	// We set overloadThreshold to 1.2 to ensure that once a worker takes one of these heavy partitions,
	// it is considered overloaded and takes no further partitions.
	// We set extremeThreshold to 20.0 to correctly identify these heavy partitions as "extreme"
	// relative to the average partition weight (which is small due to many small partitions).
	overloadThreshold := 1.2
	extremeThreshold := 20.0
	extremeCutoff := avgPartitionWeight * extremeThreshold

	extremePartitions := make(map[string]bool)
	for _, p := range partitions {
		if float64(p.Weight) > extremeCutoff {
			extremePartitions[p.Keys[0]] = true
		}
	}
	t.Logf("Identified %d extreme partitions (cutoff: %.2f)", len(extremePartitions), extremeCutoff)

	// Create strategy
	strategy := NewWeightedConsistentHash(
		WithOverloadThreshold(overloadThreshold),
		WithExtremeThreshold(extremeThreshold),
		WithMinPartitionCount(0.3), // Require at least 30% of average partition count
	)

	// Define workers - 50 workers
	workers := make([]string, 50)
	for i := 0; i < 50; i++ {
		workers[i] = fmt.Sprintf("worker-%d", i+1)
	}

	// Calculate assignment
	assignments, err := strategy.Assign(workers, partitions)
	require.NoError(t, err, "Assign failed")

	verifyAssignments(t, assignments, partitions, totalWeight, extremePartitions, len(workers))
}

func verifyAssignments(t *testing.T, assignments map[string][]types.Partition, partitions []types.Partition, totalWeight int64, extremePartitions map[string]bool, workerCount int) {
	t.Helper()

	assignedCount := 0
	workerWeights := make(map[string]int64)

	for workerID, parts := range assignments {
		var wWeight int64
		hasExtreme := false
		for _, p := range parts {
			wWeight += p.Weight
			if extremePartitions[p.Keys[0]] {
				hasExtreme = true
			}
		}
		workerWeights[workerID] = wWeight
		assignedCount += len(parts)

		if hasExtreme {
			t.Logf("Worker %s has extreme partition. Total partitions: %d, Total Weight: %d", workerID, len(parts), wWeight)
			// Check if count meets minimum requirement (approx 30% of avg 25 = 7)
			if len(parts) < 7 {
				t.Errorf("Worker %s has extreme partition but only %d partitions (expected >= 7)", workerID, len(parts))
			}
		}
	}

	require.Equal(t, len(partitions), assignedCount, "not all partitions assigned")

	// Calculate imbalance
	avgWeight := float64(totalWeight) / float64(workerCount)
	t.Logf("Average weight per worker: %.2f", avgWeight)

	for workerID, weight := range workerWeights {
		imbalance := (float64(weight) - avgWeight) / avgWeight * 100
		// Only log significant imbalances to avoid spamming
		if imbalance > 20 || imbalance < -20 {
			t.Logf("Worker %s imbalance: %.2f%% (Weight: %d)", workerID, imbalance, weight)
		}
	}
}

func TestWeightedConsistentHash_MinPartitionCount(t *testing.T) {
	// Setup:
	// 3 Workers
	// 1 Extreme Partition (Weight 1000)
	// 30 Normal Partitions (Weight 10)
	//
	// Stats:
	// Total Partitions: 31
	// Avg Partitions per Worker: 10.33
	// Total Weight: 1300
	// Avg Weight per Worker: 433.33

	workers := []string{"worker-1", "worker-2", "worker-3"}

	partitions := make([]types.Partition, 0, 31)
	// Add extreme partition
	partitions = append(partitions, types.Partition{
		Keys:   []string{"extreme-1"},
		Weight: 1000,
	})
	// Add normal partitions
	for i := 0; i < 30; i++ {
		partitions = append(partitions, types.Partition{
			Keys:   []string{fmt.Sprintf("normal-%d", i)},
			Weight: 10,
		})
	}

	// Case 1: Without MinPartitionCount (Factor 0.0)
	// Worker with extreme partition should be overloaded immediately and take no more partitions.
	t.Run("WithoutMinPartitionCount", func(t *testing.T) {
		strategy := NewWeightedConsistentHash(
			WithOverloadThreshold(1.1), // Max load ~476. Extreme(1000) is definitely overloaded.
			WithExtremeThreshold(2.0),
			WithMinPartitionCount(0.0),
		)

		assignments, err := strategy.Assign(workers, partitions)
		require.NoError(t, err)

		// Find the worker with the extreme partition
		var extremeWorker string
		for w, parts := range assignments {
			for _, p := range parts {
				if p.Keys[0] == "extreme-1" {
					extremeWorker = w
					break
				}
			}
		}
		require.NotEmpty(t, extremeWorker, "extreme partition not assigned")

		// Verify that the extreme worker has ONLY 1 partition (the extreme one)
		// because it was overloaded and MinPartitionCount was 0.
		// Note: There is a tiny chance consistent hashing assigns a normal partition to it
		// BEFORE it realizes it's overloaded if we didn't sort/prioritize extreme assignment,
		// but our logic assigns extremes first, so it starts with 1000 weight.
		// Then normal assignment starts. It checks overload (1000 > 476) -> True.
		// It checks minCount (1 >= 0) -> True.
		// So it should shed load.
		count := len(assignments[extremeWorker])
		require.Equal(t, 1, count, "worker with extreme partition should have only 1 partition when MinPartitionCount is 0")
	})

	// Case 2: With MinPartitionCount (Factor 0.4 -> Min 4 partitions)
	// Worker with extreme partition should take at least 4 partitions despite being overloaded.
	t.Run("WithMinPartitionCount", func(t *testing.T) {
		strategy := NewWeightedConsistentHash(
			WithOverloadThreshold(1.1),
			WithExtremeThreshold(2.0),
			WithMinPartitionCount(0.4), // 31/3 * 0.4 = 4.13 -> Min 4
		)

		assignments, err := strategy.Assign(workers, partitions)
		require.NoError(t, err)

		// Find the worker with the extreme partition
		var extremeWorker string
		for w, parts := range assignments {
			for _, p := range parts {
				if p.Keys[0] == "extreme-1" {
					extremeWorker = w
					break
				}
			}
		}
		require.NotEmpty(t, extremeWorker, "extreme partition not assigned")

		// Verify that the extreme worker has at least 4 partitions
		count := len(assignments[extremeWorker])
		t.Logf("Extreme worker %s has %d partitions", extremeWorker, count)
		require.GreaterOrEqual(t, count, 4, "worker with extreme partition should have at least 4 partitions")
	})
}

func TestWeightedConsistentHash_MinPartitionCount_WithDeviceData(t *testing.T) {
	// Load real device data
	partitions := readDevicePartitions(t, filepath.Join("testdata", "partition_by_devices.json"))

	totalWeight := int64(0)
	for _, p := range partitions {
		totalWeight += p.Weight
	}

	// Calculate stats
	partitionCount := len(partitions)
	workerCount := 50
	avgPartitionCount := float64(partitionCount) / float64(workerCount)
	avgPartitionWeight := float64(totalWeight) / float64(partitionCount)

	t.Logf("Stats: Total Partitions: %d, Total Weight: %d", partitionCount, totalWeight)
	t.Logf("Stats: Avg Partition Count: %.2f, Avg Partition Weight: %.2f", avgPartitionCount, avgPartitionWeight)

	// Identify extreme partitions
	extremeThreshold := 20.0
	extremeCutoff := avgPartitionWeight * extremeThreshold
	extremePartitions := make(map[string]bool)
	for _, p := range partitions {
		if float64(p.Weight) > extremeCutoff {
			extremePartitions[p.Keys[0]] = true
		}
	}
	t.Logf("Identified %d extreme partitions (cutoff: %.2f)", len(extremePartitions), extremeCutoff)

	// Define workers
	workers := make([]string, workerCount)
	for i := 0; i < workerCount; i++ {
		workers[i] = fmt.Sprintf("worker-%d", i+1)
	}

	// Test Case 1: Without MinPartitionCount (Factor 0.0)
	// Expectation: Workers with extreme partitions should have very few partitions (likely 1)
	t.Run("WithoutMinPartitionCount", func(t *testing.T) {
		strategy := NewWeightedConsistentHash(
			WithOverloadThreshold(1.2),
			WithExtremeThreshold(extremeThreshold),
			WithMinPartitionCount(0.0),
		)

		assignments, err := strategy.Assign(workers, partitions)
		require.NoError(t, err)

		lowCountWorkers := 0
		for workerID, parts := range assignments {
			hasExtreme := false
			for _, p := range parts {
				if extremePartitions[p.Keys[0]] {
					hasExtreme = true

					break
				}
			}

			if hasExtreme {
				t.Logf("Worker %s (Extreme): %d partitions", workerID, len(parts))
				if len(parts) <= 2 {
					lowCountWorkers++
				}
			}
		}

		// We expect all workers with extreme partitions to have shed load and ended up with ~1 partition
		require.Equal(t, len(extremePartitions), lowCountWorkers, "All extreme workers should have low partition count without min factor")
	})

	// Test Case 2: With MinPartitionCount (Factor 0.3 -> 30%)
	// Expectation: Workers with extreme partitions should have at least 30% of avg count
	// Avg count is ~25.16. 30% is ~7.5. So we expect at least 7 partitions.
	t.Run("WithMinPartitionCount", func(t *testing.T) {
		minFactor := 0.3
		expectedMinCount := int(avgPartitionCount * minFactor)
		t.Logf("Testing with MinPartitionCountFactor: %.2f (Expected Min: %d)", minFactor, expectedMinCount)

		strategy := NewWeightedConsistentHash(
			WithOverloadThreshold(1.2),
			WithExtremeThreshold(extremeThreshold),
			WithMinPartitionCount(minFactor),
		)

		assignments, err := strategy.Assign(workers, partitions)
		require.NoError(t, err)

		for workerID, parts := range assignments {
			hasExtreme := false
			for _, p := range parts {
				if extremePartitions[p.Keys[0]] {
					hasExtreme = true

					break
				}
			}

			if hasExtreme {
				t.Logf("Worker %s (Extreme): %d partitions", workerID, len(parts))
				require.GreaterOrEqual(t, len(parts), expectedMinCount,
					"Worker %s with extreme partition should meet min partition count", workerID)
			}
		}
	})
}
