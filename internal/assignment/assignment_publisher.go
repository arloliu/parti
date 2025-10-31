package assignment

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/arloliu/parti/types"
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
	keys, err := p.assignmentKV.Keys(ctx)
	if err != nil {
		return fmt.Errorf("failed to list KV keys: %w", err)
	}

	p.logger.Debug("discovering highest version", "total_keys", len(keys), "prefix", p.prefix)

	highestVersion := int64(0)
	checkedCount := 0
	for _, key := range keys {
		// Skip non-assignment keys (heartbeats, etc.)
		if !strings.HasPrefix(key, p.keyPrefix) {
			p.logger.Debug("skipping non-assignment key", "key", key, "prefix", p.prefix)
			continue
		}

		checkedCount++
		entry, err := p.assignmentKV.Get(ctx, key)
		if err != nil {
			p.logger.Debug("failed to read assignment key", "key", key, "error", err)
			continue // Skip entries we can't read
		}

		var asgn types.Assignment
		if err := json.Unmarshal(entry.Value(), &asgn); err != nil {
			p.logger.Debug("failed to unmarshal assignment", "key", key, "error", err)
			continue // Skip malformed entries
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

// cleanupStaleAssignments removes assignment keys for workers not in the active set.
//
// This is a reusable cleanup method that can be called during:
//   - Normal publishing (to remove assignments for departed workers)
//   - Calculator shutdown (to clean slate for new leader)
//
// Parameters:
//   - ctx: Context for cancellation
//   - activeWorkers: Map of worker IDs that should retain assignments (nil = delete all)
//
// Returns:
//   - error: Nil on success, error on KV operation failure (non-fatal, logs warnings)
func (p *AssignmentPublisher) cleanupStaleAssignments(ctx context.Context, activeWorkers map[string]bool) error {
	existingKeys, err := p.assignmentKV.Keys(ctx)
	if err != nil {
		p.logger.Warn("failed to list keys for cleanup", "error", err)
		return fmt.Errorf("failed to list keys: %w", err)
	}

	deletedCount := 0
	for _, key := range existingKeys {
		if !strings.HasPrefix(key, p.keyPrefix) {
			continue
		}

		// Extract worker ID from key (format: "prefix.workerID")
		workerID := strings.TrimPrefix(key, p.keyPrefix)

		// Check if this worker should retain assignment
		shouldDelete := activeWorkers == nil || !activeWorkers[workerID]

		if shouldDelete {
			p.logger.Debug("deleting stale assignment", "key", key, "worker_id", workerID)
			if err := p.assignmentKV.Delete(ctx, key); err != nil {
				p.logger.Warn("failed to delete stale assignment", "key", key, "error", err)
				// Continue with other deletions even if one fails
			} else {
				deletedCount++
			}
		}
	}

	if deletedCount > 0 {
		p.logger.Info("cleaned up stale assignments", "deleted_count", deletedCount)
	}

	return nil
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

	return p.cleanupStaleAssignments(ctx, nil) // nil = delete all
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
//   - lifecycle: Lifecycle phase ("cold_start", "planned_scale", "emergency", "restart")
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
//	err := publisher.Publish(ctx, []string{"worker-1", "worker-2"}, assignments, "planned_scale")
func (p *AssignmentPublisher) Publish(
	ctx context.Context,
	workers []string,
	assignments map[string][]types.Partition,
	lifecycle string,
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
	// This is critical to prevent workers from processing stale assignments
	activeWorkers := make(map[string]bool, len(assignments))
	for workerID := range assignments {
		activeWorkers[workerID] = true
	}

	if err := p.cleanupStaleAssignments(ctx, activeWorkers); err != nil {
		// Log warning but continue with publishing - cleanup is best-effort
		p.logger.Warn("stale assignment cleanup failed, continuing with publish", "error", err)
	}

	// Publish assignments to KV
	for workerID, parts := range assignments {
		assignment := types.Assignment{
			Version:    p.currentVersion,
			Lifecycle:  lifecycle,
			Partitions: parts,
		}

		data, err := json.Marshal(assignment)
		if err != nil {
			return fmt.Errorf("failed to marshal assignment: %w", err)
		}

		key := p.keyPrefix + workerID
		p.logger.Debug("publishing assignment", "key", key, "worker_id", workerID, "partitions", len(parts), "version", p.currentVersion)
		if _, err := p.assignmentKV.Put(ctx, key, data); err != nil {
			return fmt.Errorf("failed to publish assignment: %w", err)
		}
	}

	p.logger.Debug("all assignments published successfully", "version", p.currentVersion, "workers", len(assignments))

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
