package election

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/arloliu/parti/v2/types"
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
	kv           jetstream.KeyValue
	key          string
	mu           sync.RWMutex
	workerID     string
	revision     uint64
	termRevision uint64
	isLeader     bool
	logger       types.Logger
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
		logger:   nil,
	}
}

// NewNATSElectionWithLogger creates a new election agent with an optional logger for instrumentation.
//
// Parameters:
//   - kv: KV bucket used for election coordination
//   - key: Leader key name (e.g., "leader")
//   - logger: Optional structured logger; when nil, logging is disabled
//
// Returns:
//   - *NATSElection: New election agent instance with logging enabled
func NewNATSElectionWithLogger(kv jetstream.KeyValue, key string, logger types.Logger) *NATSElection {
	e := NewNATSElection(kv, key)
	e.logger = logger
	return e
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
	value := fmt.Appendf(nil, "%s:%d", workerID, time.Now().Unix())

	start := time.Now()
	if e.logger != nil {
		e.logger.Debug("election.request_start", "worker_id", workerID, "key", e.key)
	}
	revision, err := e.kv.Create(ctx, e.key, value)
	elapsed := time.Since(start)
	if err != nil {
		// Key already exists - check if we can take over
		if errors.Is(err, jetstream.ErrKeyExists) {
			if e.logger != nil {
				e.logger.Debug("election.request_exists", "worker_id", workerID, "key", e.key, "elapsed", elapsed)
			}
			return false, nil
		}
		if e.logger != nil {
			e.logger.Error("election.request_error", "worker_id", workerID, "key", e.key, "elapsed", elapsed, "error", err)
		}

		return false, fmt.Errorf("failed to create leader key: %w", err)
	}

	// Successfully acquired leadership
	e.setLeaderState(true, workerID, revision, revision)
	if e.logger != nil {
		e.logger.Info("election.lead_acquired", "worker_id", workerID, "key", e.key, "revision", revision, "elapsed", elapsed)
	}

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
	value := fmt.Appendf(nil, "%s:%d", workerID, time.Now().Unix())

	start := time.Now()
	if e.logger != nil {
		e.logger.Debug("election.renew_start", "worker_id", workerID, "key", e.key, "rev", revision)
	}
	newRevision, err := e.kv.Update(ctx, e.key, value, revision)
	elapsed := time.Since(start)
	if err != nil {
		e.clearLeadership()
		if e.logger != nil {
			e.logger.Warn("election.renew_failed", "worker_id", workerID, "key", e.key, "prev_rev", revision, "elapsed", elapsed, "error", err)
		}
		return fmt.Errorf("%w: %w", ErrLeadershipLost, err)
	}

	// Update our revision
	e.mu.Lock()
	e.revision = newRevision
	e.mu.Unlock()
	if e.logger != nil {
		e.logger.Debug("election.renew_ok", "worker_id", workerID, "key", e.key, "new_rev", newRevision, "elapsed", elapsed)
	}

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

	e.setLeaderState(false, "", 0, 0)

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
	start := time.Now()
	entry, err := e.kv.Get(ctx, e.key)
	elapsed := time.Since(start)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			e.clearLeadership()

			return false, nil
		}
		if e.logger != nil {
			e.logger.Error("election.verify_error", "key", e.key, "elapsed", elapsed, "error", err)
		}

		return false, fmt.Errorf("failed to get leader key: %w", err)
	}

	// Check if the key still has our worker ID and revision
	if entry.Revision() != revision {
		e.clearLeadership()

		return false, nil
	}
	if e.logger != nil {
		e.logger.Debug("election.verify_ok", "key", e.key, "rev", revision, "elapsed", elapsed)
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

// Revision returns the NATS KV revision at the time the current leader first
// acquired leadership. This value is stable for the entire leadership term —
// it does not advance on renewals, only when a new leader takes over.
//
// Embed this value in published assignments as LeaderRevision so workers can
// detect and discard assignments from a former leader: an assignment whose
// LeaderRevision is lower than the current term revision was published during
// a previous leadership term.
//
// Returns:
//   - uint64: Term-epoch revision, or 0 if not the leader
func (e *NATSElection) Revision() uint64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.termRevision
}

// CheckLeadership verifies that this election agent still holds the live
// leadership term claimed by the caller.
//
// Unlike Revision (which returns a cached value updated only on
// takeover/clear), CheckLeadership performs a live kv.Get on the election
// leader key and verifies BOTH:
//
//   - the live key's value still names this agent's worker ID (i.e. another
//     worker has not taken over after a TTL expiry / overwrite), AND
//   - the live key's revision is >= the claimed term-epoch revision (renewals
//     monotonically advance the KV revision, but they do not start a new
//     term, so a higher live revision is consistent with the same term).
//
// This is the live fence the assignment publisher uses for its pre-alias and
// post-alias leadership rechecks (publish steps 5 and 7 of §3.5). A former
// leader whose cached termRevision has not yet been cleared cannot pass this
// check after another worker has taken over: the live key's value will name
// the new worker, or the key may be absent during the takeover window.
//
// Returns:
//   - nil if the live leader key exists, its value names this agent's worker,
//     and its revision is >= claimed.
//   - types.ErrLeadershipRevisionMismatch (wrapped with detail) if the leader
//     key is absent, names a different worker, or has a revision below
//     claimed.
//   - any other error if the KV read itself fails (transient — caller may
//     treat as abort).
//
// Parameters:
//   - ctx: Context for the kv.Get call.
//   - claimed: The term-epoch revision the caller asserts they hold (i.e.
//     the value previously returned by Revision()).
func (e *NATSElection) CheckLeadership(ctx context.Context, claimed uint64) error {
	if claimed == 0 {
		return fmt.Errorf("%w: claimed revision is zero", types.ErrLeadershipRevisionMismatch)
	}
	e.mu.RLock()
	myWorker := e.workerID
	myTerm := e.termRevision
	e.mu.RUnlock()
	if myWorker == "" || myTerm == 0 {
		return fmt.Errorf("%w: agent does not currently hold leadership", types.ErrLeadershipRevisionMismatch)
	}
	if claimed != myTerm {
		// The claim does not match this agent's current term — either the
		// caller is stale (we took over again at a fresh term) or the claim
		// is from a different agent entirely.
		return fmt.Errorf("%w: claimed=%d local_term=%d", types.ErrLeadershipRevisionMismatch, claimed, myTerm)
	}
	entry, err := e.kv.Get(ctx, e.key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return fmt.Errorf("%w: leader key absent (claimed=%d)", types.ErrLeadershipRevisionMismatch, claimed)
		}
		return fmt.Errorf("election kv.Get(%s): %w", e.key, err)
	}
	// Renewals advance the live KV revision while termRevision stays put, so
	// require live >= claimed (not equal).
	if entry.Revision() < claimed {
		return fmt.Errorf("%w: claimed=%d live=%d (live revision below claimed term)",
			types.ErrLeadershipRevisionMismatch, claimed, entry.Revision())
	}
	// Verify the live value still names this worker. Leader values have the
	// form "<workerID>:<unix-timestamp>" (see RequestLeadership/RenewLeadership).
	val := string(entry.Value())
	prefix := myWorker + ":"
	if !strings.HasPrefix(val, prefix) {
		return fmt.Errorf("%w: live leader is %q, not %q", types.ErrLeadershipRevisionMismatch, val, myWorker)
	}

	return nil
}

// getLeaderState returns the current leadership state (thread-safe).
func (e *NATSElection) getLeaderState() (isLeader bool, workerID string, revision uint64) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.isLeader, e.workerID, e.revision
}

// setLeaderState updates the leadership state (thread-safe).
// termRevision should be set to revision on acquisition and 0 on clear; it is
// not updated on renewal so it remains stable for the lifetime of a leadership term.
func (e *NATSElection) setLeaderState(isLeader bool, workerID string, revision, termRevision uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.isLeader = isLeader
	e.workerID = workerID
	e.revision = revision
	e.termRevision = termRevision
}

// clearLeadership resets all leadership state (thread-safe).
//
// Used on involuntary loss (renewal failure, key mismatch). Zeros revision and
// workerID so that Revision() does not return a stale value after loss.
func (e *NATSElection) clearLeadership() {
	e.setLeaderState(false, "", 0, 0)
}
