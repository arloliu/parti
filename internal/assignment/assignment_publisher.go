package assignment

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// AssignmentPublisher handles publishing partition assignments to NATS KV.
//
// Manages version monotonicity across leader changes by discovering
// the highest existing version on startup.
type AssignmentPublisher struct {
	assignmentKV jetstream.KeyValue
	prefix       string
	keyPrefix    string // cached "prefix."

	mu             sync.Mutex
	currentVersion int64
	lastRebalance  time.Time

	logger  types.Logger
	metrics types.AssignmentMetrics
}

// NewAssignmentPublisher creates a new assignment publisher.
//
// Parameters:
//   - assignmentKV: NATS KV bucket for assignments
//   - prefix: Prefix for assignment keys (e.g., "assignment")
//   - logger: Logger for publishing events
//   - metrics: Metrics collector for assignment operations
//
// Returns:
//   - *AssignmentPublisher: A new publisher instance
func NewAssignmentPublisher(
	assignmentKV jetstream.KeyValue,
	prefix string,
	logger types.Logger,
	metrics types.AssignmentMetrics,
) *AssignmentPublisher {
	return &AssignmentPublisher{
		assignmentKV: assignmentKV,
		prefix:       prefix,
		keyPrefix:    fmt.Sprintf("%s.", prefix),
		logger:       logger,
		metrics:      metrics,
	}
}

// DiscoverHighestVersion scans KV for the highest existing assignment version.
//
// This ensures version monotonicity across leader changes by finding the
// maximum version number from existing assignments.
//
// Parameters:
//   - ctx: Context for cancellation
//
// Returns:
//   - error: Nil on success, error on KV access failure
func (p *AssignmentPublisher) DiscoverHighestVersion(ctx context.Context) error {
	keys, err := kvutil.ListKeys(ctx, p.assignmentKV, p.keyPrefix, false)
	if err != nil {
		return fmt.Errorf("failed to list KV keys: %w", err)
	}

	p.logger.Debug("discovering highest version", "total_keys", len(keys), "prefix", p.prefix)

	highestVersion := int64(0)
	checkedCount := 0
	for _, key := range keys {
		checkedCount++
		asgn, _, err := kvutil.GetJSON[types.Assignment](ctx, p.assignmentKV, key)
		if err != nil {
			p.logger.Debug("failed to read/unmarshal assignment", "key", key, "error", err)
			continue // Skip entries we can't read or are malformed
		}
		if asgn == nil {
			continue // Should not happen given ListKeys, but safe to check
		}

		p.logger.Debug("found assignment", "key", key, "version", asgn.Version)
		if asgn.Version > highestVersion {
			highestVersion = asgn.Version
		}
	}

	p.mu.Lock()
	p.currentVersion = highestVersion
	p.mu.Unlock()

	if highestVersion > 0 {
		p.logger.Info("discovered existing assignments", "highest_version", highestVersion, "checked_keys", checkedCount)
	} else {
		p.logger.Debug("no existing assignments found", "checked_keys", checkedCount)
	}

	return nil
}

// cleanupStaleAssignments removes assignment keys for specific workers.
//
// This is a targeted cleanup method that avoids scanning the entire KV bucket.
//
// Parameters:
//   - ctx: Context for cancellation
//   - workersToRemove: List of worker IDs whose assignments should be deleted
func (p *AssignmentPublisher) cleanupStaleAssignments(ctx context.Context, workersToRemove []string) {
	if len(workersToRemove) == 0 {
		return
	}

	deletedCount := 0
	for _, workerID := range workersToRemove {
		key := p.keyPrefix + workerID
		p.logger.Debug("deleting stale assignment", "key", key, "worker_id", workerID)
		if err := kvutil.DeleteKey(ctx, p.assignmentKV, key); err != nil {
			p.logger.Warn("failed to delete stale assignment", "key", key, "error", err)
			// Continue with other deletions even if one fails
		} else {
			deletedCount++
		}
	}

	if deletedCount > 0 {
		p.logger.Info("cleaned up stale assignments", "deleted_count", deletedCount)
	}
}

// CleanupAllAssignments removes all assignment keys from KV.
//
// This should be called when the Calculator stops to provide a clean slate
// for the new leader. It's safe to call even if cleanup fails - the new
// leader will discover existing versions and maintain monotonicity.
//
// Parameters:
//   - ctx: Context for cancellation (recommend 5s timeout)
//
// Returns:
//   - error: Nil on success, error on KV operation failure (non-fatal)
func (p *AssignmentPublisher) CleanupAllAssignments(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.logger.Info("cleaning up all assignments from KV")

	// For full cleanup, we still need to list keys since we don't know who exists
	keys, err := p.assignmentKV.Keys(ctx)
	if err != nil {
		return fmt.Errorf("failed to list keys for full cleanup: %w", err)
	}

	var workersToRemove []string
	for _, key := range keys {
		if strings.HasPrefix(key, p.keyPrefix) {
			workersToRemove = append(workersToRemove, strings.TrimPrefix(key, p.keyPrefix))
		}
	}

	p.cleanupStaleAssignments(ctx, workersToRemove)

	return nil
}

// Publish publishes partition assignments to NATS KV.
//
// This method increments the version number, serializes assignments to JSON,
// and publishes them to the KV bucket with keys formatted as "prefix.workerID".
//
// Parameters:
//   - ctx: Context for cancellation
//   - workers: List of active worker IDs
//   - assignments: Map of worker ID to partition assignments
//   - workersToRemove: List of worker IDs that have left the cluster and need cleanup
//   - lifecycle: Lifecycle phase ("cold_start", "planned_scale", "emergency", "restart")
//   - leaderRevision: NATS KV revision of the leader key at publish time (0 if unknown)
//
// Returns:
//   - error: Nil on success, error on marshaling or KV operation failure
//
// Example:
//
//	assignments := map[string][]types.Partition{
//	    "worker-1": {{Keys: []string{"p1"}}, {Keys: []string{"p2"}}},
//	    "worker-2": {{Keys: []string{"p3"}}, {Keys: []string{"p4"}}},
//	}
//	err := publisher.Publish(ctx, []string{"worker-1", "worker-2"}, assignments, nil, "planned_scale", 0)
func (p *AssignmentPublisher) Publish(
	ctx context.Context,
	workers []string,
	assignments map[string][]types.Partition,
	workersToRemove []string,
	lifecycle string,
	leaderRevision uint64,
) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.logger.Debug("publishing assignments", "lifecycle", lifecycle, "worker_count", len(workers))

	if len(workers) == 0 {
		p.logger.Info("no active workers for assignment")
		return nil
	}

	// Increment version
	p.currentVersion++

	p.logger.Debug("publishing assignments to KV", "version", p.currentVersion, "worker_count", len(assignments))

	// Delete assignments for workers that are no longer active
	p.cleanupStaleAssignments(ctx, workersToRemove)

	// Publish assignments in sorted worker order for deterministic write sequence.
	// Note: NATS KV does not support multi-key atomic writes; a crash between
	// individual puts may leave a partial batch in KV. The TotalWorkers field
	// lets workers detect that some assignments may still be in flight.
	totalWorkers := len(assignments)
	sortedWorkers := make([]string, 0, totalWorkers)
	for wid := range assignments {
		sortedWorkers = append(sortedWorkers, wid)
	}
	sort.Strings(sortedWorkers)

	for _, workerID := range sortedWorkers {
		parts := assignments[workerID]
		assignment := types.Assignment{
			Version:        p.currentVersion,
			Lifecycle:      lifecycle,
			Partitions:     parts,
			LeaderRevision: leaderRevision,
			TotalWorkers:   totalWorkers,
		}

		key := p.keyPrefix + workerID
		p.logger.Debug("publishing assignment", "key", key, "worker_id", workerID, "partitions", len(parts), "version", p.currentVersion)
		if _, err := kvutil.PutJSON(ctx, p.assignmentKV, key, assignment); err != nil {
			return fmt.Errorf("failed to publish assignment: %w", err)
		}
	}

	p.logger.Debug("all assignments published successfully", "version", p.currentVersion, "workers", len(assignments))

	// Diagnostic summary: per-worker partition counts for visibility during investigations.
	if len(assignments) > 0 {
		counts := make([]any, 0, len(assignments)*2)
		for wid, parts := range assignments {
			counts = append(counts, wid, len(parts))
		}

		logArgs := append([]any{"version", p.currentVersion, "lifecycle", lifecycle}, counts...)
		if len(assignments) <= 5 {
			p.logger.Info("assignment distribution", logArgs...)
		} else {
			p.logger.Debug("assignment distribution (suppressed)", append(logArgs, "worker_count", len(assignments))...)
		}
	}

	// Update last rebalance time
	p.lastRebalance = time.Now()

	// Record metrics
	for _, parts := range assignments {
		p.metrics.RecordAssignmentChange(len(parts), 0, p.currentVersion)
	}

	p.logger.Info("assignments published",
		"version", p.currentVersion,
		"workers", len(workers),
		"lifecycle", lifecycle)

	return nil
}

// CurrentVersion returns the current assignment version.
//
// This method is thread-safe and can be called concurrently.
//
// Returns:
//   - int64: Current version number (0 if no assignments published yet)
func (p *AssignmentPublisher) CurrentVersion() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.currentVersion
}

// LastRebalanceTime returns the time of the last successful rebalance.
//
// This is used by the calculator to enforce rebalance cooldown periods.
//
// Returns:
//   - time.Time: Time of last rebalance (zero time if never rebalanced)
func (p *AssignmentPublisher) LastRebalanceTime() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.lastRebalance
}
