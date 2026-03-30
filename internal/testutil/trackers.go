package testutil

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/arloliu/parti/v2/types"
)

// AssignmentTracker records assignment changes for a worker via the OnAssignmentChanged hook.
// All methods are thread-safe and can be called concurrently.
type AssignmentTracker struct {
	WorkerIndex int
	T           *testing.T
	mu          sync.RWMutex
	entries     []AssignmentEntry
	closed      atomic.Bool
	cond        *sync.Cond // signalled on each new entry
}

// AssignmentEntry records a single assignment change event.
type AssignmentEntry struct {
	Version        int64
	PartitionCount int
}

// CreateAssignmentTracker creates a new AssignmentTracker for the given worker index.
func CreateAssignmentTracker(t *testing.T, workerIndex int) *AssignmentTracker {
	at := &AssignmentTracker{
		WorkerIndex: workerIndex,
		T:           t,
		entries:     make([]AssignmentEntry, 0),
	}
	at.cond = sync.NewCond(at.mu.RLocker())

	return at
}

// Hook returns a hook function suitable for Hooks.OnAssignmentChanged.
func (at *AssignmentTracker) Hook() func(context.Context, []types.Partition, []types.Partition) error {
	return func(_ context.Context, _, newPartitions []types.Partition) error {
		if !at.closed.Load() {
			at.T.Logf("Worker %d: assignment changed → %d partitions", at.WorkerIndex, len(newPartitions))
		}

		at.mu.Lock()
		at.entries = append(at.entries, AssignmentEntry{
			PartitionCount: len(newPartitions),
		})
		at.mu.Unlock()
		at.cond.Broadcast()

		return nil
	}
}

// Close marks the tracker as closed to prevent logging after test completion.
func (at *AssignmentTracker) Close() {
	at.closed.Store(true)
}

// Count returns the number of assignment change events recorded.
func (at *AssignmentTracker) Count() int {
	at.mu.RLock()
	defer at.mu.RUnlock()

	return len(at.entries)
}

// Entries returns a copy of all recorded assignment entries.
func (at *AssignmentTracker) Entries() []AssignmentEntry {
	at.mu.RLock()
	defer at.mu.RUnlock()

	result := make([]AssignmentEntry, len(at.entries))
	copy(result, at.entries)

	return result
}

// LeadershipTracker records leadership changes for a worker via the OnLeadershipChanged hook.
// All methods are thread-safe and can be called concurrently.
type LeadershipTracker struct {
	WorkerIndex int
	T           *testing.T
	mu          sync.RWMutex
	entries     []LeadershipEntry
	closed      atomic.Bool
	cond        *sync.Cond
}

// LeadershipEntry records a single leadership change event.
type LeadershipEntry struct {
	IsLeader bool
}

// CreateLeadershipTracker creates a new LeadershipTracker for the given worker index.
func CreateLeadershipTracker(t *testing.T, workerIndex int) *LeadershipTracker {
	lt := &LeadershipTracker{
		WorkerIndex: workerIndex,
		T:           t,
		entries:     make([]LeadershipEntry, 0),
	}
	lt.cond = sync.NewCond(lt.mu.RLocker())

	return lt
}

// Hook returns a hook function suitable for Hooks.OnLeadershipChanged.
func (lt *LeadershipTracker) Hook() func(context.Context, bool) error {
	return func(_ context.Context, isLeader bool) error {
		if !lt.closed.Load() {
			lt.T.Logf("Worker %d: leadership changed → isLeader=%v", lt.WorkerIndex, isLeader)
		}

		lt.mu.Lock()
		lt.entries = append(lt.entries, LeadershipEntry{IsLeader: isLeader})
		lt.mu.Unlock()
		lt.cond.Broadcast()

		return nil
	}
}

// Close marks the tracker as closed to prevent logging after test completion.
func (lt *LeadershipTracker) Close() {
	lt.closed.Store(true)
}

// IsLeader returns true if the last recorded leadership change was to leader.
func (lt *LeadershipTracker) IsLeader() bool {
	lt.mu.RLock()
	defer lt.mu.RUnlock()

	if len(lt.entries) == 0 {
		return false
	}

	return lt.entries[len(lt.entries)-1].IsLeader
}

// Count returns the number of leadership change events recorded.
func (lt *LeadershipTracker) Count() int {
	lt.mu.RLock()
	defer lt.mu.RUnlock()

	return len(lt.entries)
}

// Entries returns a copy of all recorded leadership entries.
func (lt *LeadershipTracker) Entries() []LeadershipEntry {
	lt.mu.RLock()
	defer lt.mu.RUnlock()

	result := make([]LeadershipEntry, len(lt.entries))
	copy(result, lt.entries)

	return result
}
