package testutil

import (
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// WaitForLeader waits until exactly one active worker in the cluster is leader.
// Returns the leader Manager. Fails the test if no single leader is found within timeout.
func (wc *WorkerCluster) WaitForLeader(timeout time.Duration) *parti.Manager {
	wc.T.Helper()

	var leader *parti.Manager
	require.Eventually(wc.T, func() bool {
		leader = nil
		leaderCount := 0

		wc.mu.RLock()
		workers := make([]*parti.Manager, len(wc.Workers))
		copy(workers, wc.Workers)
		wc.mu.RUnlock()

		for _, mgr := range workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			if mgr.IsLeader() {
				leaderCount++
				leader = mgr
			}
		}

		return leaderCount == 1
	}, timeout, 100*time.Millisecond, "expected exactly one leader within timeout")

	return leader
}

// WaitForNewLeader waits until exactly one active worker is leader AND it is
// different from oldLeaderID. Returns the new leader Manager.
func (wc *WorkerCluster) WaitForNewLeader(oldLeaderID string, timeout time.Duration) *parti.Manager {
	wc.T.Helper()

	var leader *parti.Manager
	require.Eventually(wc.T, func() bool {
		leader = nil
		leaderCount := 0

		wc.mu.RLock()
		workers := make([]*parti.Manager, len(wc.Workers))
		copy(workers, wc.Workers)
		wc.mu.RUnlock()

		for _, mgr := range workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			if mgr.IsLeader() {
				leaderCount++
				leader = mgr
			}
		}

		return leaderCount == 1 && leader != nil && leader.WorkerID() != oldLeaderID
	}, timeout, 100*time.Millisecond, "expected a new leader (different from %s) within timeout", oldLeaderID)

	return leader
}

// WaitForPartitionCoverage waits until the total number of unique partition IDs
// across all active workers equals expectedCount.
func (wc *WorkerCluster) WaitForPartitionCoverage(expectedCount int, timeout time.Duration) {
	wc.T.Helper()

	require.Eventually(wc.T, func() bool {
		wc.mu.RLock()
		workers := make([]*parti.Manager, len(wc.Workers))
		copy(workers, wc.Workers)
		wc.mu.RUnlock()

		uniq := make(map[string]struct{})
		for _, mgr := range workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			assignment := mgr.CurrentAssignment()
			for _, p := range assignment.Partitions {
				if id := p.ID(); id != "" {
					uniq[id] = struct{}{}
				}
			}
		}

		return len(uniq) == expectedCount
	}, timeout, 100*time.Millisecond,
		"expected %d unique partitions across active workers within timeout", expectedCount)
}

// WaitForAssignmentVersion waits until all active workers have an assignment
// version >= minVersion.
func (wc *WorkerCluster) WaitForAssignmentVersion(minVersion int64, timeout time.Duration) {
	wc.T.Helper()

	require.Eventually(wc.T, func() bool {
		wc.mu.RLock()
		workers := make([]*parti.Manager, len(wc.Workers))
		copy(workers, wc.Workers)
		wc.mu.RUnlock()

		for _, mgr := range workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			if mgr.CurrentAssignment().Version < minVersion {
				return false
			}
		}

		return true
	}, timeout, 200*time.Millisecond,
		"expected all active workers to reach assignment version >= %d within timeout", minVersion)
}

// WaitForLeaderAndCoverage waits until a new leader (different from oldLeaderID)
// is elected AND the total unique partition count equals expectedPartitions.
// Returns the new leader Manager.
func (wc *WorkerCluster) WaitForLeaderAndCoverage(oldLeaderID string, expectedPartitions int, timeout time.Duration) *parti.Manager {
	wc.T.Helper()

	var leader *parti.Manager
	require.Eventually(wc.T, func() bool {
		leader = nil
		leaderCount := 0
		uniq := make(map[string]struct{})

		wc.mu.RLock()
		workers := make([]*parti.Manager, len(wc.Workers))
		copy(workers, wc.Workers)
		wc.mu.RUnlock()

		for _, mgr := range workers {
			if mgr.State() == types.StateShutdown {
				continue
			}
			if mgr.IsLeader() {
				leaderCount++
				leader = mgr
			}
			assignment := mgr.CurrentAssignment()
			for _, p := range assignment.Partitions {
				if id := p.ID(); id != "" {
					uniq[id] = struct{}{}
				}
			}
		}

		return leaderCount == 1 && leader != nil &&
			leader.WorkerID() != oldLeaderID &&
			len(uniq) == expectedPartitions
	}, timeout, 100*time.Millisecond,
		"expected new leader (not %s) and %d partitions within timeout", oldLeaderID, expectedPartitions)

	return leader
}

// NeverChanges asserts that sampleFn returns the same value throughout duration.
// sampleFn is called every 50ms. The test fails if the value ever differs from
// the initial sample.
//
// This is a generic negative assertion helper that replaces specific
// AssertNoVersionChange / AssertNoRedelivery / NeverLeaderChanges patterns.
func NeverChanges[T comparable](t *testing.T, sampleFn func() T, duration time.Duration, msgAndArgs ...any) {
	t.Helper()

	initial := sampleFn()
	require.Never(t, func() bool {
		return sampleFn() != initial
	}, duration, 50*time.Millisecond, msgAndArgs...)
}
