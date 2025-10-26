package subscription

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/arloliu/parti"
	"github.com/nats-io/nats.go"
)

// Config configures the subscription helper.
type Config struct {
	Stream            string
	MaxRetries        int
	RetryBackoff      time.Duration
	ReconcileInterval time.Duration
}

// Helper provides automatic subscription management with reconciliation.
type Helper struct {
	conn   *nats.Conn
	config Config

	// State tracking
	mu            sync.RWMutex
	subscriptions map[string]*nats.Subscription // partition ID -> subscription
}

// NewHelper creates a new subscription helper.
//
// The helper manages NATS subscriptions with automatic retry logic and
// periodic reconciliation to ensure subscription health.
//
// Parameters:
//   - conn: NATS connection
//   - cfg: Helper configuration
//
// Returns:
//   - *Helper: Initialized subscription helper
//
// Example:
//
//	helper := subscription.NewHelper(natsConn, subscription.Config{
//	    Stream:            "work-stream",
//	    MaxRetries:        3,
//	    RetryBackoff:      time.Second,
//	    ReconcileInterval: 30 * time.Second,
//	})
func NewHelper(conn *nats.Conn, cfg Config) *Helper {
	return &Helper{
		conn:          conn,
		config:        cfg,
		subscriptions: make(map[string]*nats.Subscription),
	}
}

// UpdateSubscriptions reconciles subscriptions based on partition changes.
//
// Creates subscriptions for added partitions and removes subscriptions for
// removed partitions. Includes retry logic and error handling.
//
// Parameters:
//   - ctx: Context for cancellation
//   - added: Partitions newly assigned to this worker
//   - removed: Partitions no longer assigned to this worker
//   - handler: Message handler function
//
// Returns:
//   - error: Subscription error if reconciliation fails
//
// Example:
//
//	err := helper.UpdateSubscriptions(ctx, added, removed, func(msg *nats.Msg) {
//	    // Handle message
//	})
func (h *Helper) UpdateSubscriptions(
	ctx context.Context,
	added, removed []parti.Partition,
	handler nats.MsgHandler,
) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Unsubscribe from removed partitions
	for _, partition := range removed {
		// Use first key as partition identifier (typically the unique ID)
		if len(partition.Keys) == 0 {
			continue
		}
		partID := partition.Keys[0]

		if sub, exists := h.subscriptions[partID]; exists {
			if err := sub.Unsubscribe(); err != nil {
				// Log error but continue with other partitions
				// In production, you might want to inject a logger here
				_ = err
			}
			delete(h.subscriptions, partID)
		}
	}

	// Subscribe to added partitions with retry logic
	for _, partition := range added {
		// Use first key as partition identifier
		if len(partition.Keys) == 0 {
			continue
		}
		partID := partition.Keys[0]

		// Skip if already subscribed
		if _, exists := h.subscriptions[partID]; exists {
			continue
		}

		// Build subject from stream and partition
		subject := fmt.Sprintf("%s.%s", h.config.Stream, partID)

		// Try to subscribe with retries
		var sub *nats.Subscription
		var err error

		for attempt := 0; attempt <= h.config.MaxRetries; attempt++ {
			// Check context cancellation
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			// Attempt subscription
			sub, err = h.conn.Subscribe(subject, handler)
			if err == nil {
				break
			}

			// Retry with backoff if not last attempt
			if attempt < h.config.MaxRetries {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(h.config.RetryBackoff):
					// Continue to next attempt
				}
			}
		}

		// If subscription failed after all retries, return error
		if err != nil {
			return fmt.Errorf("failed to subscribe to partition %s after %d attempts: %w",
				partID, h.config.MaxRetries+1, err)
		}

		// Store subscription for later cleanup
		h.subscriptions[partID] = sub
	}

	return nil
}

// Close unsubscribes from all active partitions and cleans up resources.
//
// Returns:
//   - error: Cleanup error if any subscriptions fail to close
//
// Example:
//
//	defer helper.Close()
func (h *Helper) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	var firstErr error

	for partID, sub := range h.subscriptions {
		if err := sub.Unsubscribe(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("failed to unsubscribe from partition %s: %w", partID, err)
		}
	}

	// Clear all subscriptions
	h.subscriptions = make(map[string]*nats.Subscription)

	return firstErr
}

// ActivePartitions returns the list of currently subscribed partition IDs.
//
// Returns:
//   - []string: List of active partition IDs
func (h *Helper) ActivePartitions() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	partitions := make([]string, 0, len(h.subscriptions))
	for partID := range h.subscriptions {
		partitions = append(partitions, partID)
	}

	return partitions
}
