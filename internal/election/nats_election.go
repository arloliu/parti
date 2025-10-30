package election

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/types"
)

// Common errors for election operations.
var (
	ErrNotLeader       = errors.New("not the leader")
	ErrLeadershipLost  = errors.New("leadership was lost")
	ErrInvalidDuration = errors.New("invalid lease duration")
)

// NATSElection implements leader election using NATS KV store.
//
// Uses atomic KV operations for leader election:
//   - Create (atomic): Acquire leadership if key doesn't exist
//   - Update (with revision): Renew leadership if still holding the lease
//   - Delete: Release leadership
//
// The leader key contains the worker ID and is automatically deleted
// when the TTL expires, allowing automatic failover.
//
// All fields are protected by mu for thread-safe concurrent access.
type NATSElection struct {
	kv       jetstream.KeyValue
	key      string
	mu       sync.RWMutex
	workerID string
	revision uint64
	isLeader bool
}

// Compile-time assertion that NATSElection implements ElectionAgent.
var _ types.ElectionAgent = (*NATSElection)(nil)

// NewNATSElection creates a new NATS KV-based election agent.
//
// The KV bucket should be configured with a short TTL (e.g., 10-30s)
// for automatic leader failover when the leader crashes.
//
// Parameters:
//   - kv: JetStream KV bucket for election coordination
//   - key: Key name for leadership claim (e.g., "leader")
//
// Returns:
//   - *NATSElection: New election agent instance
//
// Example:
//
//	kv, _ := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
//	    Bucket:  "parti-election",
//	    TTL:     30 * time.Second,
//	    Storage: jetstream.FileStorage,
//	})
//	election := election.NewNATSElection(kv, "leader")
func NewNATSElection(kv jetstream.KeyValue, key string) *NATSElection {
	return &NATSElection{
		kv:       kv,
		key:      key,
		workerID: "",
		revision: 0,
		isLeader: false,
	}
}

// RequestLeadership attempts to acquire or maintain leadership.
//
// Uses atomic Create operation for initial acquisition and Update for renewal.
// The lease duration is enforced by the KV bucket's TTL configuration.
//
// Parameters:
//   - ctx: Context for timeout
//   - workerID: The worker ID requesting leadership
//   - leaseDuration: Lease duration in seconds (unused, TTL set at bucket level)
//
// Returns:
//   - bool: true if leadership acquired/held, false otherwise
//   - error: Election error or context cancellation
func (e *NATSElection) RequestLeadership(ctx context.Context, workerID string, leaseDuration int64) (bool, error) {
	if leaseDuration <= 0 {
		return false, ErrInvalidDuration
	}

	// Check if already leader with same workerID
	isLeader, currentWorkerID, _ := e.getLeaderState()

	// If already leader with same workerID, try to renew
	if isLeader && currentWorkerID == workerID {
		err := e.RenewLeadership(ctx)
		if err == nil {
			return true, nil
		}
		// Leadership lost, fall through to try acquiring again
		e.clearLeadership()
	}

	// Try to acquire leadership atomically
	value := []byte(fmt.Sprintf("%s:%d", workerID, time.Now().Unix()))

	revision, err := e.kv.Create(ctx, e.key, value)
	if err != nil {
		// Key already exists - check if we can take over
		if errors.Is(err, jetstream.ErrKeyExists) {
			return false, nil
		}

		return false, fmt.Errorf("failed to create leader key: %w", err)
	}

	// Successfully acquired leadership
	e.setLeaderState(true, workerID, revision)

	return true, nil
}

// RenewLeadership renews the current leadership lease.
//
// Uses Update with revision check to ensure we still hold the lease.
// If another worker claimed leadership, this will fail.
//
// Parameters:
//   - ctx: Context for timeout
//
// Returns:
//   - error: ErrNotLeader if not the leader, ErrLeadershipLost if lost, nil on success
func (e *NATSElection) RenewLeadership(ctx context.Context) error {
	isLeader, workerID, revision := e.getLeaderState()

	if !isLeader {
		return ErrNotLeader
	}

	// Update with our current revision to renew
	value := []byte(fmt.Sprintf("%s:%d", workerID, time.Now().Unix()))

	newRevision, err := e.kv.Update(ctx, e.key, value, revision)
	if err != nil {
		e.clearLeadership()

		return fmt.Errorf("%w: %w", ErrLeadershipLost, err)
	}

	// Update our revision
	e.mu.Lock()
	e.revision = newRevision
	e.mu.Unlock()

	return nil
}

// ReleaseLeadership voluntarily releases leadership.
//
// Deletes the leader key to allow immediate failover to another worker.
//
// Parameters:
//   - ctx: Context for timeout
//
// Returns:
//   - error: Release error or context cancellation
func (e *NATSElection) ReleaseLeadership(ctx context.Context) error {
	isLeader, _, _ := e.getLeaderState()

	if !isLeader {
		return ErrNotLeader
	}

	err := e.kv.Delete(ctx, e.key)
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("failed to delete leader key: %w", err)
	}

	e.setLeaderState(false, "", 0)

	return nil
}

// IsLeader checks if this worker is currently the leader.
//
// Verifies leadership by checking if the key exists and matches our worker ID.
//
// Parameters:
//   - ctx: Context for timeout
//
// Returns:
//   - bool: true if this worker is the leader
//   - error: Check error or context cancellation
func (e *NATSElection) IsLeader(ctx context.Context) (bool, error) {
	isLeader, _, revision := e.getLeaderState()

	if !isLeader {
		return false, nil
	}

	// Verify leadership by checking the key
	entry, err := e.kv.Get(ctx, e.key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			e.clearLeadership()

			return false, nil
		}

		return false, fmt.Errorf("failed to get leader key: %w", err)
	}

	// Check if the key still has our worker ID and revision
	if entry.Revision() != revision {
		e.clearLeadership()

		return false, nil
	}

	return true, nil
}

// WorkerID returns the current leader's worker ID.
//
// Returns:
//   - string: Worker ID if this instance is the leader, empty otherwise
func (e *NATSElection) WorkerID() string {
	_, workerID, _ := e.getLeaderState()
	return workerID
}

// getLeaderState returns the current leadership state (thread-safe).
func (e *NATSElection) getLeaderState() (isLeader bool, workerID string, revision uint64) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.isLeader, e.workerID, e.revision
}

// setLeaderState updates the leadership state (thread-safe).
func (e *NATSElection) setLeaderState(isLeader bool, workerID string, revision uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.isLeader = isLeader
	e.workerID = workerID
	e.revision = revision
}

// clearLeadership clears the leadership flag (thread-safe).
func (e *NATSElection) clearLeadership() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.isLeader = false
}
