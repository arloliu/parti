package stableid

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
)

// Common errors returned by the claimer.
var (
	ErrNoAvailableID         = errors.New("no available worker ID in pool")
	ErrNotClaimed            = errors.New("worker ID not claimed")
	ErrAlreadyClosed         = errors.New("claimer already closed")
	ErrRenewalAlreadyStarted = errors.New("renewal loop already started")
)

// Claimer handles stable worker ID claiming and background renewal using NATS KV.
//
// Overview:
//   - Claim(ctx) acquires the first available ID in [minID, maxID] using KV Create semantics.
//   - StartRenewal() starts a background goroutine that periodically renews the claim.
//     The provided ctx is used only for initial checks; the goroutine lifetime is controlled
//     by Release() or Close(). Each renewal tick uses its own short timeout context.
//   - Release(ctx) stops renewal and deletes the KV key, freeing the ID for reuse.
//   - Close() stops renewal without deleting the key, for handoff scenarios.
//
// Lifecycle contract:
//   - StartRenewal is idempotent: subsequent calls after success return ErrRenewalAlreadyStarted.
//   - StartRenewal returns ErrNotClaimed if called before a successful Claim.
//   - Release is idempotent and safe for concurrent calls; it waits for renewal to stop when started.
//   - Close is idempotent; once closed, StartRenewal returns ErrAlreadyClosed.
//
// Concurrency and timing:
//   - Renewal interval is ttl/3 with a minimum of 100ms.
//   - Each renew attempt uses context.WithTimeout with duration clamped to [100ms, 5s].
//   - Renew errors are logged and do not stop the loop; loop stops only via Release/Close.
//
// Workers sequentially search the ID pool until finding an available ID.
type Claimer struct {
	kv     jetstream.KeyValue
	prefix string
	minID  int
	maxID  int
	ttl    time.Duration

	workerID string        // Claimed worker ID
	stopCh   chan struct{} // Signal to stop renewal goroutine
	doneCh   chan struct{} // Signal that renewal has stopped
	stopOnce sync.Once     // Ensures stopCh is closed only once

	renewStarted atomic.Int32 // 0 = not started, 1 = started
	closed       atomic.Int32 // 0 = open, 1 = closed (Release/Close called)

	logger types.Logger
}

// NewClaimer creates a new stable ID claimer.
//
// Parameters:
//   - kv: NATS KV bucket for stable IDs
//   - prefix: Worker ID prefix (e.g., "worker")
//   - minID: Minimum ID number (inclusive)
//   - maxID: Maximum ID number (inclusive)
//   - ttl: TTL for ID claims
//   - logger: Logger for debug output
//
// Returns:
//   - *Claimer: New claimer instance
//
// Example:
//
//	claimer := stableid.NewClaimer(kvBucket, "worker", 0, 99, 30*time.Second, logger)
//	workerID, err := claimer.Claim(ctx)
func NewClaimer(kv jetstream.KeyValue, prefix string, minID, maxID int, ttl time.Duration, logger types.Logger) *Claimer {
	if logger == nil {
		logger = logging.NewNop()
	}

	return &Claimer{
		kv:     kv,
		prefix: prefix,
		minID:  minID,
		maxID:  maxID,
		ttl:    ttl,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
		logger: logger,
	}
}

// Claim attempts to claim a stable worker ID from the pool.
//
// Sequentially tries IDs from minID to maxID until finding an available one.
// Uses NATS KV CREATE operation for atomic claiming.
//
// Parameters:
//   - ctx: Context for timeout/cancellation
//
// Returns:
//   - string: Claimed worker ID (e.g., "worker-5")
//   - error: ErrNoAvailableID if pool exhausted, context error, or NATS error
//
// Example:
//
//	workerID, err := claimer.Claim(ctx)
//	if err != nil {
//	    log.Fatalf("Failed to claim ID: %v", err)
//	}
//	log.Printf("Claimed worker ID: %s", workerID)
func (c *Claimer) Claim(ctx context.Context) (string, error) {
	c.logger.Debug("stable ID claim starting", "prefix", c.prefix, "min", c.minID, "max", c.maxID, "ttl", c.ttl)

	// Try each ID sequentially
	for id := c.minID; id <= c.maxID; id++ {
		select {
		case <-ctx.Done():
			c.logger.Debug("stable ID claim cancelled", "tried_ids", id-c.minID)
			return "", ctx.Err()
		default:
		}

		workerID := fmt.Sprintf("%s-%d", c.prefix, id)
		key := c.keyForID(workerID)

		c.logger.Debug("attempting to claim stable ID", "worker_id", workerID, "key", key, "attempt", id-c.minID+1)

		// Try to create the key (atomic operation)
		// Value contains timestamp for monitoring
		value := time.Now().Format(time.RFC3339)

		revision, err := c.kv.Create(ctx, key, []byte(value))
		c.logger.Debug("kv.Create result", "worker_id", workerID, "key", key, "revision", revision, "error", err)

		if err == nil {
			// Successfully claimed this ID
			c.workerID = workerID
			c.logger.Info("stable ID claimed successfully", "worker_id", workerID, "key", key, "revision", revision, "attempts", id-c.minID+1)

			return workerID, nil
		}

		// Check if it's just already claimed (expected) vs other errors
		if !errors.Is(err, jetstream.ErrKeyExists) {
			// Unexpected error (connection issue, etc.)
			c.logger.Error("stable ID claim failed with unexpected error", "worker_id", workerID, "error", err)
			return "", fmt.Errorf("failed to claim ID %s: %w", workerID, err)
		}

		// Key exists - check if it's stale (expired but not yet purged)
		// This can happen with file storage after NATS restart
		entry, getErr := c.kv.Get(ctx, key)
		if getErr != nil {
			// Key was deleted/expired between Create and Get - try to claim it
			c.logger.Debug("key disappeared after Create failed, attempting Put", "worker_id", workerID)
			revision, putErr := c.kv.Put(ctx, key, []byte(value))
			if putErr == nil {
				c.workerID = workerID
				c.logger.Info("stable ID claimed via Put after expiry", "worker_id", workerID, "key", key, "revision", revision)

				return workerID, nil
			}
			// Put failed, someone else got it - try next ID
			c.logger.Debug("Put failed, trying next ID", "worker_id", workerID, "error", putErr, "next_id", id+1)
		} else {
			// Key exists and is valid - skip to next ID
			c.logger.Debug("stable ID actively claimed by another worker", "worker_id", workerID, "revision", entry.Revision(), "next_id", id+1)
		}
	}

	c.logger.Error("no available stable IDs in pool", "prefix", c.prefix, "min", c.minID, "max", c.maxID, "pool_size", c.maxID-c.minID+1)

	return "", ErrNoAvailableID
}

// StartRenewal starts background renewal of the claimed ID.
//
// Renews the ID claim at regular intervals (ttl/3) to maintain the lease.
// Must be called after successful Claim(). The renewal loop's lifetime is
// independent of any external context; use Release() or Close() to stop it.
//
// Returns:
//   - error: ErrNotClaimed if ID not claimed yet
//   - error: ErrRenewalAlreadyStarted if called more than once
//   - error: ErrAlreadyClosed if the claimer was closed
//
// Example:
//
//	workerID, _ := claimer.Claim(ctx)
//	if err := claimer.StartRenewal(); err != nil {
//	    log.Fatalf("Failed to start renewal: %v", err)
//	}
//	defer claimer.Release(ctx)
//
// Idempotent: subsequent successful calls return ErrRenewalAlreadyStarted.
// Safe: returns ErrNotClaimed if called before successful Claim. Returns ErrAlreadyClosed
// if the claimer has been closed.
func (c *Claimer) StartRenewal() error {
	if c.workerID == "" {
		return ErrNotClaimed
	}
	if c.closed.Load() == 1 {
		return ErrAlreadyClosed
	}
	if !c.renewStarted.CompareAndSwap(0, 1) {
		return ErrRenewalAlreadyStarted
	}

	go c.renewalLoop()

	return nil
}

// renewalLoop periodically renews the ID claim until stopped.
func (c *Claimer) renewalLoop() {
	defer close(c.doneCh)

	// Renew at 1/3 of TTL to provide safety margin; enforce minimum interval.
	renewInterval := max(c.ttl/3, 100*time.Millisecond)
	ticker := time.NewTicker(renewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			// Per-iteration timeout (half TTL capped) to avoid indefinite stalls.
			opTimeout := c.ttl / 2
			if opTimeout < 100*time.Millisecond {
				opTimeout = 100 * time.Millisecond
			} else if opTimeout > 5*time.Second {
				opTimeout = 5 * time.Second
			}
			opCtx, cancel := context.WithTimeout(context.Background(), opTimeout)
			if err := c.renew(opCtx); err != nil {
				c.logger.Error("stable ID renewal failed", "worker_id", c.workerID, "error", err)
			}
			cancel()
		}
	}
}

// renew updates the claimed ID's timestamp to maintain the lease.
func (c *Claimer) renew(ctx context.Context) error {
	if c.workerID == "" {
		return ErrNotClaimed
	}
	if c.kv == nil {
		return errors.New("kv bucket is nil")
	}

	key := c.keyForID(c.workerID)
	value := time.Now().Format(time.RFC3339)

	_, err := c.kv.Put(ctx, key, []byte(value))
	if err != nil {
		return fmt.Errorf("failed to renew ID %s: %w", c.workerID, err)
	}

	return nil
}

// Release releases the claimed worker ID and stops renewal.
//
// Should be called during graceful shutdown to free the ID for reuse.
// Safe to call multiple times concurrently - subsequent calls will return ErrNotClaimed.
//
// Parameters:
//   - ctx: Context for timeout
//
// Returns:
//   - error: Release error or context cancellation
//
// Example:
//
//	if err := claimer.Release(ctx); err != nil {
//	    log.Printf("Warning: failed to release ID: %v", err)
//	}
func (c *Claimer) Release(ctx context.Context) error {
	if c.workerID == "" {
		return ErrNotClaimed
	}
	if c.closed.CompareAndSwap(0, 1) {
		// first time close; signal stop
		c.stopOnce.Do(func() { close(c.stopCh) })
	}

	// Wait only if renewal started.
	if c.renewStarted.Load() == 1 {
		select {
		case <-c.doneCh:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			// timeout; proceed to attempt delete anyway
		}
	}

	key := c.keyForID(c.workerID)
	if c.kv != nil {
		if err := c.kv.Delete(ctx, key); err != nil {
			return fmt.Errorf("failed to delete ID %s: %w", c.workerID, err)
		}
	}

	c.workerID = ""

	return nil
}

// Close stops the renewal loop without releasing (deleting) the claimed ID.
// Useful for handoff scenarios where another process will assume renewal soon.
// Idempotent; safe to call multiple times.
func (c *Claimer) Close() {
	if c.closed.CompareAndSwap(0, 1) {
		c.stopOnce.Do(func() { close(c.stopCh) })
	}
}

// WorkerID returns the currently claimed worker ID.
//
// Returns:
//   - string: Claimed worker ID (empty if not claimed)
func (c *Claimer) WorkerID() string {
	return c.workerID
}

// keyForID converts a worker ID to a KV key.
func (c *Claimer) keyForID(workerID string) string {
	return workerID
}
