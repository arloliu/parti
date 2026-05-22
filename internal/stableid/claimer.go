package stableid

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// Common errors returned by the claimer.
var (
	ErrNoAvailableID         = errors.New("no available worker ID in pool")
	ErrNotClaimed            = errors.New("worker ID not claimed")
	ErrAlreadyClosed         = errors.New("claimer already closed")
	ErrRenewalAlreadyStarted = errors.New("renewal loop already started")
	// ErrClaimLost is returned by renewal when the claimed ID's KV revision no
	// longer matches — another worker took the ID over, or it expired and was
	// re-created. The renewal loop stops; the claim cannot be recovered in place.
	ErrClaimLost = errors.New("worker ID claim lost")
)

// Claimer handles stable worker ID claiming and background renewal using NATS KV.
//
// Overview:
//   - Claim(ctx) acquires the first available ID in [minID, maxID] using KV Create semantics.
//   - Claim takes over an ID whose key exists but has not been renewed within
//     the stale threshold (3 renewal intervals) — a stale claim left by an
//     ungracefully-stopped worker — using an atomic revision-checked Update so
//     concurrent reclaimers cannot collide.
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
//   - Renewal uses a revision-checked Update; if the claim is lost (the ID was
//     taken over, or expired and re-created), renew returns ErrClaimLost and
//     the renewal loop stops.
//
// Workers sequentially search the ID pool until finding an available ID.
type Claimer struct {
	kv     jetstream.KeyValue
	prefix string
	minID  int
	maxID  int
	ttl    time.Duration

	mu       sync.RWMutex  // Protects workerID field for concurrent access
	workerID string        // Claimed worker ID
	stopCh   chan struct{} // Signal to stop renewal goroutine
	doneCh   chan struct{} // Signal that renewal has stopped
	stopOnce sync.Once     // Ensures stopCh is closed only once

	renewStarted atomic.Int32 // 0 = not started, 1 = started
	closed       atomic.Int32 // 0 = open, 1 = closed (Release/Close called)

	// lastRevision is the KV revision this Claimer last wrote (via Create,
	// takeover Update, or a renewal Update). renew() uses it as the
	// compare-and-swap expectation; Release() uses it for a revision-checked
	// delete. Written by Claim before StartRenewal and by the renewal
	// goroutine afterwards; atomic so Release can read it concurrently.
	lastRevision atomic.Uint64

	logger  types.Logger
	onError atomic.Pointer[func(error)]
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

// SetOnError registers a callback invoked on each background renewal failure.
//
// Safe to call concurrently with the running renewal goroutine; the update is
// atomic and the hot path reads without locking. Passing nil clears the
// callback. The callback must not block on the Claimer's own lifecycle
// (do not call Release from inside it).
//
// Parameters:
//   - fn: Callback invoked with the raw error; ignored if nil
func (c *Claimer) SetOnError(fn func(error)) {
	if fn == nil {
		c.onError.Store(nil)
		return
	}
	c.onError.Store(&fn)
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
			c.mu.Lock()
			c.workerID = workerID
			c.mu.Unlock()
			c.lastRevision.Store(revision)
			c.logger.Info("stable ID claimed successfully", "worker_id", workerID, "key", key, "revision", revision, "attempts", id-c.minID+1)

			return workerID, nil
		}

		// Check if it's just already claimed (expected) vs other errors
		if !errors.Is(err, jetstream.ErrKeyExists) {
			// Unexpected error (connection issue, etc.)
			c.logger.Error("stable ID claim failed with unexpected error", "worker_id", workerID, "error", err)
			return "", fmt.Errorf("failed to claim ID %s: %w", workerID, err)
		}

		// Key exists - check if it's stale (expired but not yet purged).
		// This can happen with file storage after NATS restart.
		entry, getErr := c.kv.Get(ctx, key)
		if getErr != nil {
			// Key was deleted/expired between Create and Get.
			// Retry Create atomically — NOT Put, which is non-atomic and allows
			// two concurrent workers to both "win" the same ID (split-brain).
			c.logger.Debug("key disappeared after Create failed, retrying Create", "worker_id", workerID)
			revision, createErr := c.kv.Create(ctx, key, []byte(value))
			if createErr == nil {
				c.mu.Lock()
				c.workerID = workerID
				c.mu.Unlock()
				c.lastRevision.Store(revision)
				c.logger.Info("stable ID claimed via Create retry after expiry", "worker_id", workerID, "key", key, "revision", revision)

				return workerID, nil
			}
			if !errors.Is(createErr, jetstream.ErrKeyExists) {
				c.logger.Error("stable ID Create retry failed with unexpected error", "worker_id", workerID, "error", createErr)
				return "", fmt.Errorf("failed to claim ID %s: %w", workerID, createErr)
			}
			// Another worker won the Create retry - try next ID
			c.logger.Debug("Create retry lost race, trying next ID", "worker_id", workerID, "next_id", id+1)
		} else {
			// Key exists. If it has not been renewed within the stale
			// threshold its holder is presumed dead — atomically take it over
			// with a revision-checked Update so two reclaiming workers cannot
			// both win the same ID.
			if c.isStale(entry) {
				staleAge := time.Since(entry.Created())
				revision, upErr := c.kv.Update(ctx, key, []byte(value), entry.Revision())
				if upErr == nil {
					c.mu.Lock()
					c.workerID = workerID
					c.mu.Unlock()
					c.lastRevision.Store(revision)
					c.logger.Warn("stable ID reclaimed from stale holder",
						"worker_id", workerID, "key", key, "revision", revision, "stale_age", staleAge)

					return workerID, nil
				}
				if !errors.Is(upErr, jetstream.ErrKeyExists) {
					c.logger.Error("stable ID takeover failed with unexpected error", "worker_id", workerID, "error", upErr)
					return "", fmt.Errorf("failed to take over ID %s: %w", workerID, upErr)
				}
				// Revision moved between Get and Update — another worker
				// renewed or took over. Skip to the next ID.
				c.logger.Debug("stable ID takeover lost race, trying next ID", "worker_id", workerID, "next_id", id+1)
			} else {
				// Key exists and is fresh - skip to next ID
				c.logger.Debug("stable ID actively claimed by another worker", "worker_id", workerID, "revision", entry.Revision(), "next_id", id+1)
			}
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
	c.mu.RLock()
	hasID := c.workerID != ""
	c.mu.RUnlock()

	if !hasID {
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
	ticker := time.NewTicker(c.renewInterval())
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
			err := c.renew(opCtx)
			cancel()
			if err != nil {
				c.mu.RLock()
				wid := c.workerID
				c.mu.RUnlock()
				c.logger.Error("stable ID renewal failed", "worker_id", wid, "error", err)
				if onError := c.onError.Load(); onError != nil {
					(*onError)(err)
				}
				if errors.Is(err, ErrClaimLost) {
					// The claim cannot be recovered in place; stop renewing so
					// we no longer fight the new owner of this ID.
					c.logger.Error("stable ID claim lost, stopping renewal", "worker_id", wid)
					return
				}
			}
		}
	}
}

// renew updates the claimed ID's timestamp to maintain the lease.
//
// It uses a revision-checked Update rather than Put so a worker that lost its
// claim (the ID was taken over, or expired and re-created) detects it instead
// of silently overwriting the new owner's key. A revision mismatch surfaces as
// jetstream.ErrKeyExists (nats.go's wrong-last-sequence sentinel) and is
// translated to ErrClaimLost.
func (c *Claimer) renew(ctx context.Context) error {
	c.mu.RLock()
	wid := c.workerID
	c.mu.RUnlock()

	if wid == "" {
		return ErrNotClaimed
	}
	if c.kv == nil {
		return errors.New("kv bucket is nil")
	}

	key := c.keyForID(wid)
	value := time.Now().Format(time.RFC3339)

	newRev, err := c.kv.Update(ctx, key, []byte(value), c.lastRevision.Load())
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			// Revision mismatch: another worker took this ID over, or it
			// expired and was re-created. The claim is lost.
			return fmt.Errorf("%w: ID %s", ErrClaimLost, wid)
		}
		return fmt.Errorf("failed to renew ID %s: %w", wid, err)
	}
	c.lastRevision.Store(newRev)

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
	c.mu.RLock()
	wid := c.workerID
	c.mu.RUnlock()

	if wid == "" {
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
			// Safety fallback: if renewal goroutine is stuck (e.g., blocked on NATS I/O),
			// proceed to delete the KV key anyway so the ID is freed. 5s is chosen to be
			// well above typical NATS operation timeouts (100ms-5s) while still allowing
			// timely shutdown.
		}
	}

	key := c.keyForID(wid)
	if c.kv != nil {
		// Revision-checked delete: if this ID was taken over (revision moved),
		// the delete is rejected with jetstream.ErrKeyExists — tolerated, so we
		// never clobber the new owner's key.
		err := c.kv.Delete(ctx, key, jetstream.LastRevision(c.lastRevision.Load()))
		if err != nil && !errors.Is(err, jetstream.ErrKeyExists) {
			return fmt.Errorf("failed to delete ID %s: %w", wid, err)
		}
		if errors.Is(err, jetstream.ErrKeyExists) {
			c.logger.Warn("stable ID not deleted on release: claim was already lost",
				"worker_id", wid, "key", key)
		}
	}

	c.mu.Lock()
	c.workerID = ""
	c.mu.Unlock()

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
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.workerID
}

// keyForID converts a worker ID to a KV key.
func (c *Claimer) keyForID(workerID string) string {
	return workerID
}

// minRenewInterval is the floor for the renewal cadence. It also floors the
// stale-takeover threshold (see staleThreshold) so that a healthy holder is
// never judged stale even when ttl is small enough that ttl/3 drops below it.
const minRenewInterval = 100 * time.Millisecond

// renewInterval is the cadence at which the renewal loop refreshes the claim:
// one third of ttl, floored at minRenewInterval.
func (c *Claimer) renewInterval() time.Duration {
	return max(c.ttl/3, minRenewInterval)
}

// staleThreshold is the age past which a claimed ID's key is presumed
// abandoned and may be taken over. It is three renewal intervals: a healthy
// holder renews every renewInterval, so its key must miss roughly three
// consecutive renewals before another worker may reclaim it.
func (c *Claimer) staleThreshold() time.Duration {
	return 3 * c.renewInterval()
}

// isStale reports whether a claimed ID's key has not been renewed within the
// stale threshold and may therefore be taken over.
//
// The reference timestamp is entry.Created() — the NATS server's timestamp of
// the latest revision — so the decision does not depend on the previous
// holder's wall clock; only this worker's clock skew against the NATS server
// matters. A healthy holder renews every renewInterval and the threshold is
// 3×renewInterval, so a false takeover requires this worker's clock to run
// more than 2×renewInterval ahead of the server.
func (c *Claimer) isStale(entry jetstream.KeyValueEntry) bool {
	return time.Since(entry.Created()) > c.staleThreshold()
}
