package assignment_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestAssignmentCorrectness_AllPartitionsAssigned verifies that all partitions
// are assigned exactly once across workers with no orphans or duplicates.
func TestAssignmentCorrectness_AllPartitionsAssigned(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	const (
		numPartitions = 100
		numWorkers    = 5
	)

	env := newTestEnv(t, 60*time.Second)

	partitions := makePartitions(numPartitions, 0)

	// Create config with test-optimized timings
	cfg := parti.TestConfig()
	cfg.WorkerIDPrefix = "test-worker"
	cfg.WorkerIDTTL = 10 * time.Second
	cfg.ColdStartWindow = 2 * time.Second
	cfg.PlannedScaleWindow = 1 * time.Second

	// Create managers with SHARED JetStream handle
	managers := make([]*parti.Manager, numWorkers)
	js, err := jetstream.New(env.Conn)
	require.NoError(t, err)

	for i := range managers {
		mgr, err := parti.NewManager(&cfg, js, source.NewStatic(partitions), strategy.NewConsistentHash(), parti.WithLogger(env.Logger))
		require.NoError(t, err)
		managers[i] = mgr
	}

	defer cleanupManagers(t, managers)

	// Start all managers CONCURRENTLY (simulates real deployment)
	startTime := time.Now()
	var wg sync.WaitGroup
	startErrors := make([]error, numWorkers)

	for i, mgr := range managers {
		idx := i
		m := mgr
		wg.Go(func() {
			t.Logf("Starting manager %d at T+%v", idx, time.Since(startTime))
			if err := m.Start(env.Ctx); err != nil {
				startErrors[idx] = err
				t.Logf("Manager %d failed to start: %v", idx, err)
			} else {
				t.Logf("Manager %d started successfully", idx)
			}
		})
	}

	wg.Wait()

	for i, err := range startErrors {
		require.NoError(t, err, "manager %d failed to start", i)
	}

	waitForStableWithTimeout(t, env.Ctx, managers, 15*time.Second)

	// Poll until assignments converge (no duplicates, correct total)
	verifyAssignments(t, managers, numPartitions)

	// Verify distribution is reasonable (no worker has 0 or too many)
	totalAssigned := 0
	for i, mgr := range managers {
		count := len(mgr.CurrentAssignment().Partitions)
		totalAssigned += count
		expectedMin := numPartitions / (numWorkers * 2) // At least 10
		expectedMax := numPartitions / numWorkers * 2   // At most 40

		require.GreaterOrEqual(t, count, expectedMin,
			"Worker %d has too few partitions (%d < %d)", i, count, expectedMin)
		require.LessOrEqual(t, count, expectedMax,
			"Worker %d has too many partitions (%d > %d)", i, count, expectedMax)
	}

	t.Log("All partitions assigned exactly once")
	t.Logf("Distribution: total=%d, workers=%d, avg=%.1f per worker",
		totalAssigned, numWorkers, float64(totalAssigned)/float64(numWorkers))
}

// TestAssignmentCorrectness_StableAssignments verifies that assignments remain
// stable when there are no topology changes.
func TestAssignmentCorrectness_StableAssignments(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Parallel()

	const (
		numPartitions = 50
		numWorkers    = 3
		observePeriod = 7 * time.Second
	)

	env := newTestEnv(t, 30*time.Second)

	// Use different key prefix to avoid collision with other tests
	partitions := make([]types.Partition, numPartitions)
	for i := range partitions {
		partitions[i] = types.Partition{
			Keys:   []string{"test", "partition", fmt.Sprintf("%03d", i)},
			Weight: 100,
		}
	}

	cfg := parti.TestConfig()
	cfg.WorkerIDPrefix = "test-worker"
	cfg.ColdStartWindow = 3 * time.Second
	cfg.PlannedScaleWindow = 3 * time.Second
	cfg.RebalanceCooldown = 2 * time.Second

	managers := env.SetupManagers(t, &cfg, source.NewStatic(partitions), strategy.NewConsistentHash(), numWorkers)
	startManagersConcurrently(t, env.Ctx, managers)
	waitForStableWithTimeout(t, env.Ctx, managers, 25*time.Second)

	// Poll until assignments converge before recording the baseline
	verifyAssignments(t, managers, numPartitions)

	initialAssignments := make(map[int][]types.Partition, numWorkers)
	for i, mgr := range managers {
		initialAssignments[i] = mgr.CurrentAssignment().Partitions
		t.Logf("Worker %d initial: %d partitions", i, len(initialAssignments[i]))
	}

	// Observe assignments over time — they should not change
	ticker := time.NewTicker(1500 * time.Millisecond)
	ignoreSamples := 3
	defer ticker.Stop()

	changeCount := 0
	observations := 0

	observeCtx, observeCancel := context.WithTimeout(env.Ctx, observePeriod)
	defer observeCancel()

	for {
		select {
		case <-observeCtx.Done():
			goto done
		case <-ticker.C:
			observations++
			for i, mgr := range managers {
				current := mgr.CurrentAssignment().Partitions
				initial := initialAssignments[i]
				if observations <= ignoreSamples {
					initialAssignments[i] = current
					continue
				}
				if !partitionSetsEqual(initial, current) {
					changeCount++
					t.Logf("Worker %d assignment changed at observation %d: %d -> %d partitions",
						i, observations, len(initial), len(current))
					initialAssignments[i] = current
				}
			}
		}
	}

done:
	require.Equal(t, 0, changeCount,
		"Assignments changed %d times during %s with no topology changes", changeCount, observePeriod)
	t.Logf("Assignments remained stable for %s (%d observations)", observePeriod, observations)
}
