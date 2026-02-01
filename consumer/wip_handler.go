package consumer

import (
	"context"
	"sync"
	"time"

	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
)

// DefaultWIPMinInterval is the minimum allowed heartbeat interval.
// Intervals below this threshold are clamped to prevent excessive overhead.
const DefaultWIPMinInterval = 100 * time.Millisecond

// WIPConfig configures the work-in-progress heartbeat wrapper.
//
// The wrapper periodically calls msg.InProgress() while the handler is running
// to extend the JetStream AckWait deadline for long-running processing.
//
// # Interval Selection Guidelines
//
// Choose Interval based on your AckWait setting:
//   - Recommended: AckWait / 3 (provides safety margin)
//   - Maximum safe: AckWait / 2 (minimum margin)
//   - Example: AckWait=30s → Interval=10s
//
// Very small intervals (< 100ms) are clamped to DefaultWIPMinInterval to prevent
// excessive msg.InProgress() calls which can degrade performance.
//
// # Performance Characteristics
//
// The wrapper uses lazy initialization:
//   - Fast handlers (< Interval): Only a timer allocation, no goroutine spawned
//   - Slow handlers (>= Interval): One goroutine per message during processing
//
// # Compatibility
//
// WIPHandler works with all consumer types (Queue, Static, Dynamic, Broadcast)
// and both auto-ack and manual-ack modes. In manual-ack mode, the handler must
// still call msg.Ack/Nak/Term exactly once; the wrapper only calls InProgress().
type WIPConfig struct {
	// Interval is the heartbeat interval for msg.InProgress() calls.
	//
	// Recommended: set Interval to AckWait/3 for safety margin.
	// Values <= 0 disable heartbeats (returns unwrapped handler).
	// Values < DefaultWIPMinInterval are clamped to prevent overhead.
	Interval time.Duration

	// MinInterval overrides the minimum interval threshold.
	// If zero, DefaultWIPMinInterval (100ms) is used.
	// Set to a negative value to disable clamping (not recommended).
	MinInterval time.Duration

	// Logger receives heartbeat errors. If nil, errors are silently ignored.
	Logger types.Logger
}

// WIPHandler wraps a MessageHandler with automatic msg.InProgress() heartbeats.
//
// The heartbeat starts lazily: if the handler finishes before Interval elapses,
// no heartbeat goroutine is started. This ensures minimal overhead for fast handlers.
//
// Thread Safety: WIPHandler is safe for concurrent use. Each Handle call operates
// independently with its own heartbeat goroutine (if needed).
type WIPHandler struct {
	inner    MessageHandler
	interval time.Duration
	logger   types.Logger
}

// NewWIPHandler creates a work-in-progress handler wrapper.
//
// The wrapper sends periodic msg.InProgress() calls while the underlying handler
// is processing, preventing JetStream from redelivering the message before
// processing completes.
//
// Parameters:
//   - handler: The underlying handler to wrap (nil returns nil)
//   - cfg: Heartbeat configuration (interval, optional min interval, and logger)
//
// Returns:
//   - MessageHandler: The wrapped handler, or the original handler when disabled
//
// Behavior:
//   - cfg.Interval <= 0: Returns handler unchanged (heartbeats disabled)
//   - cfg.Interval < MinInterval: Interval is clamped to MinInterval
//   - handler == nil: Returns nil
//
// Example:
//
//	wrapped := consumer.NewWIPHandler(myHandler, consumer.WIPConfig{
//	    Interval: 10 * time.Second, // AckWait is 30s, so 10s is safe
//	    Logger:   logger,
//	})
func NewWIPHandler(handler MessageHandler, cfg WIPConfig) MessageHandler {
	if handler == nil {
		return nil
	}
	if cfg.Interval <= 0 {
		return handler
	}

	// Determine minimum interval
	minInterval := cfg.MinInterval
	if minInterval == 0 {
		minInterval = DefaultWIPMinInterval
	}

	// Clamp interval to minimum (unless MinInterval is negative, which disables clamping)
	interval := cfg.Interval
	if minInterval > 0 && interval < minInterval {
		interval = minInterval
	}

	return &WIPHandler{
		inner:    handler,
		interval: interval,
		logger:   cfg.Logger,
	}
}

// Handle processes a message while extending its AckWait deadline with heartbeats.
//
// Parameters:
//   - ctx: Request-scoped context for cancellation and deadlines
//   - msg: The JetStream message to process
//
// Returns:
//   - error: Any error returned by the wrapped handler
func (w *WIPHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	stop := w.startHeartbeat(ctx, msg)
	if stop != nil {
		defer stop()
	}

	return w.inner.Handle(ctx, msg)
}

func (w *WIPHandler) startHeartbeat(ctx context.Context, msg jetstream.Msg) func() {
	if w.interval <= 0 {
		return nil
	}

	stopCh := make(chan struct{})
	timer := time.AfterFunc(w.interval, func() {
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := msg.InProgress(); err != nil && w.logger != nil {
					w.logger.Warn("failed to extend message ack deadline", "error", err)
				}
			}
		}
	})

	var once sync.Once

	return func() {
		once.Do(func() {
			close(stopCh)
			timer.Stop()
		})
	}
}
