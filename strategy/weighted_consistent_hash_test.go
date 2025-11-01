package strategy

import (
	"fmt"
	"testing"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/types"
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
	partitions := []types.Partition{
		{Keys: []string{"extreme-0"}, Weight: 5000},
		{Keys: []string{"extreme-1"}, Weight: 4000},
		{Keys: []string{"extreme-2"}, Weight: 3000},
		{Keys: []string{"extreme-3"}, Weight: 2000},
	}

	for i := range 12 {
		partitions = append(partitions, types.Partition{
			Keys:   []string{fmt.Sprintf("normal-%02d", i)},
			Weight: 100,
		})
	}

	strategy := NewWeightedConsistentHash()

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
