package stableid

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Common errors returned by the claimer.
var (
	ErrNoAvailableID = errors.New("no available worker ID in pool")
	ErrNotClaimed    = errors.New("worker ID not claimed")
	ErrAlreadyClosed = errors.New("claimer already closed")
)

// Claimer handles stable worker ID claiming and renewal.
//
// It uses NATS KV for atomic ID claiming with TTL-based leases.
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
}

// NewClaimer creates a new stable ID claimer.
//
// Parameters:
//   - kv: NATS KV bucket for stable IDs
//   - prefix: Worker ID prefix (e.g., "worker")
//   - minID: Minimum ID number (inclusive)
//   - maxID: Maximum ID number (inclusive)
//   - ttl: TTL for ID claims
//
// Returns:
//   - *Claimer: New claimer instance
//
// Example:
//
//	claimer := stableid.NewClaimer(kvBucket, "worker", 0, 99, 30*time.Second)
//	workerID, err := claimer.Claim(ctx)
func NewClaimer(kv jetstream.KeyValue, prefix string, minID, maxID int, ttl time.Duration) *Claimer {
	return &Claimer{
		kv:     kv,
		prefix: prefix,
		minID:  minID,
		maxID:  maxID,
		ttl:    ttl,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
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
	// Try each ID sequentially
	for id := c.minID; id <= c.maxID; id++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		workerID := fmt.Sprintf("%s-%d", c.prefix, id)
		key := c.keyForID(workerID)

		// Try to create the key (atomic operation)
		// Value contains timestamp for monitoring
		value := time.Now().Format(time.RFC3339)

		_, err := c.kv.Create(ctx, key, []byte(value))
		if err == nil {
			// Successfully claimed this ID
			c.workerID = workerID
			return workerID, nil
		}

		// Check if it's just already claimed (expected) vs other errors
		if !errors.Is(err, jetstream.ErrKeyExists) {
			// Unexpected error (connection issue, etc.)
			return "", fmt.Errorf("failed to claim ID %s: %w", workerID, err)
		}

		// ID already claimed, try next one
	}

	return "", ErrNoAvailableID
}

// StartRenewal starts background renewal of the claimed ID.
//
// Renews the ID claim at regular intervals (ttl/3) to maintain the lease.
// Must be called after successful Claim(). The context is used for timeout
// control during renewal operations but won't stop the renewal loop - use
// Release() for graceful shutdown.
//
// Parameters:
//   - ctx: Context for timeout control during renewal operations
//
// Returns:
//   - error: ErrNotClaimed if ID not claimed yet
//
// Example:
//
//	workerID, _ := claimer.Claim(ctx)
//	if err := claimer.StartRenewal(ctx); err != nil {
//	    log.Fatalf("Failed to start renewal: %v", err)
//	}
//	defer claimer.Release(ctx)
func (c *Claimer) StartRenewal(ctx context.Context) error {
	if c.workerID == "" {
		return ErrNotClaimed
	}

	go c.renewalLoop(ctx)

	return nil
}

// renewalLoop periodically renews the ID claim until stopped.
func (c *Claimer) renewalLoop(ctx context.Context) {
	defer close(c.doneCh)

	// Renew at 1/3 of TTL to provide safety margin
	renewInterval := c.ttl / 3
	ticker := time.NewTicker(renewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			if err := c.renew(ctx); err != nil {
				// Log error but continue trying
				// In production, this should use the configured logger
				_ = err
			}
		}
	}
}

// renew updates the claimed ID's timestamp to maintain the lease.
func (c *Claimer) renew(ctx context.Context) error {
	if c.workerID == "" {
		return ErrNotClaimed
	}

	key := c.keyForID(c.workerID)
	value := time.Now().Format(time.RFC3339)

	// Update the existing key with timeout from context
	_, err := c.kv.Update(ctx, key, []byte(value), 0) // revision 0 = latest
	if err != nil {
		return fmt.Errorf("failed to renew ID %s: %w", c.workerID, err)
	}

	return nil
}

// Release releases the claimed worker ID and stops renewal.
//
// Should be called during graceful shutdown to free the ID for reuse.
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

	// Stop renewal goroutine
	close(c.stopCh)

	// Wait for renewal to stop with timeout
	select {
	case <-c.doneCh:
		// Renewal stopped cleanly
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		// Renewal didn't stop, continue anyway
	}

	// Delete the key from KV
	key := c.keyForID(c.workerID)
	if err := c.kv.Delete(ctx, key); err != nil {
		return fmt.Errorf("failed to delete ID %s: %w", c.workerID, err)
	}

	c.workerID = ""

	return nil
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
