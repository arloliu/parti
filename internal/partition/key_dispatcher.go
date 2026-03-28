package partition

import (
	"context"
	"sync"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// KeyExtractorFunc extracts a routing key from a message.
//
// The extracted key determines which goroutine processes the message.
// Messages with the same key are guaranteed to be processed sequentially
// within that key's goroutine.
type KeyExtractorFunc func(msg jetstream.Msg) string

// keyDispatcher routes messages to per-key goroutines for concurrent processing.
//
// Architecture:
//
//	Main Pull Loop
//	     │
//	     ▼
//	┌─────────────────┐
//	│  keyDispatcher  │
//	│  (routes by key)│
//	└────┬───┬───┬────┘
//	     │   │   │
//	     ▼   ▼   ▼
//	  ┌───┐┌───┐┌───┐
//	  │ A ││ B ││ C │  (per-key workers)
//	  └───┘└───┘└───┘
//
// Behavior:
//   - Messages with the same key are processed sequentially (FIFO within key)
//   - Different keys are processed concurrently in separate goroutines
//   - Goroutines are created on-demand when a new key is encountered
//   - Idle goroutines exit after KeyIdleTimeout
//
// WARNING: This creates an unbounded number of goroutines (one per unique key).
// If your workload has a very large number of unique keys, memory usage will grow
// proportionally. Consider whether your use case is suitable for this pattern.
type keyDispatcher struct {
	logger       types.Logger
	metrics      types.MetricsCollector
	handler      messageHandler
	keyExtractor KeyExtractorFunc
	channelBuf   int
	idleTimeout  time.Duration
	manualAck    bool

	mu      sync.RWMutex
	workers map[string]*keyWorker
	wg      sync.WaitGroup

	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
}

// keyWorker represents a single key's processing goroutine.
type keyWorker struct {
	key     string
	msgCh   chan keyMessage
	done    chan struct{}
	closeCh chan struct{}
}

// keyMessage wraps a message with its context for channel passing.
type keyMessage struct {
	ctx context.Context
	msg jetstream.Msg
}

// newKeyDispatcher creates a new key dispatcher.
//
// The keyExtractor must not be nil - callers should provide a pattern-aware
// extractor from PatternParts.KeyExtractorFunc().
func newKeyDispatcher(
	logger types.Logger,
	metrics types.MetricsCollector,
	handler messageHandler,
	keyExtractor KeyExtractorFunc,
	channelBuf int,
	idleTimeout time.Duration,
	manualAck bool,
) *keyDispatcher {
	if keyExtractor == nil {
		panic("keyExtractor must not be nil")
	}
	if channelBuf <= 0 {
		channelBuf = 32
	}
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Second
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &keyDispatcher{
		logger:       logger,
		metrics:      metrics,
		handler:      handler,
		keyExtractor: keyExtractor,
		channelBuf:   channelBuf,
		idleTimeout:  idleTimeout,
		manualAck:    manualAck,
		workers:      make(map[string]*keyWorker),
		ctx:          ctx,
		cancel:       cancel,
	}
}

// Dispatch routes a message to the appropriate key worker.
//
// If a worker for the key doesn't exist, one is created on-demand.
// This method blocks if the key's channel buffer is full (backpressure).
//
// Returns false if the dispatcher is closed.
func (kd *keyDispatcher) Dispatch(ctx context.Context, msg jetstream.Msg) bool {
	key := kd.keyExtractor(msg)

	for {
		// Fast path: check if worker exists
		kd.mu.RLock()
		worker, exists := kd.workers[key]
		kd.mu.RUnlock()

		if !exists {
			worker = kd.getOrCreateWorker(key)
			if worker == nil {
				// Dispatcher is closed
				return false
			}
		}

		// Send message to worker (blocks if channel is full - backpressure)
		select {
		case worker.msgCh <- keyMessage{ctx: ctx, msg: msg}:
			return true
		case <-worker.closeCh:
			// Worker is closing, wait for cleanup and retry
			select {
			case <-worker.done:
			case <-kd.ctx.Done():
				return false
			}
		case <-kd.ctx.Done():
			return false
		}
	}
}

// getOrCreateWorker retrieves an existing worker or creates a new one.
func (kd *keyDispatcher) getOrCreateWorker(key string) *keyWorker {
	kd.mu.Lock()
	defer kd.mu.Unlock()

	// Check again under write lock (double-check locking)
	if worker, exists := kd.workers[key]; exists {
		return worker
	}

	// Check if dispatcher is closed
	select {
	case <-kd.ctx.Done():
		return nil
	default:
	}

	// Create new worker
	worker := &keyWorker{
		key:     key,
		msgCh:   make(chan keyMessage, kd.channelBuf),
		done:    make(chan struct{}),
		closeCh: make(chan struct{}),
	}
	kd.workers[key] = worker
	kd.wg.Add(1)

	go kd.runWorker(worker)

	kd.logger.Debug("key worker started", "key", key)

	return worker
}

// runWorker is the main loop for a key worker goroutine.
func (kd *keyDispatcher) runWorker(worker *keyWorker) {
	defer func() {
		kd.removeWorker(worker.key)
		close(worker.done)
		kd.wg.Done()
		kd.logger.Debug("key worker stopped", "key", worker.key)
	}()

	timer := time.NewTimer(kd.idleTimeout)
	defer timer.Stop()

	for {
		select {
		case km, ok := <-worker.msgCh:
			if !ok {
				// Channel closed, exit
				return
			}

			// Reset idle timer
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(kd.idleTimeout)

			// Process message
			kd.processMessage(km.ctx, km.msg)

		case <-timer.C:
			// Idle timeout - check if channel is empty before exiting
			if len(worker.msgCh) == 0 {
				close(worker.closeCh)
				return
			}
			// Channel has messages, reset timer and continue
			timer.Reset(kd.idleTimeout)

		case <-kd.ctx.Done():
			// Dispatcher is closing - drain remaining messages
			kd.drainWorker(worker)
			return
		}
	}
}

// processMessage handles a single message with ack/nak logic.
func (kd *keyDispatcher) processMessage(ctx context.Context, msg jetstream.Msg) {
	if kd.manualAck {
		_ = kd.handler.Handle(ctx, msg)
		return
	}

	if err := kd.handler.Handle(ctx, msg); err != nil {
		_ = msg.Nak()
	} else {
		_ = msg.Ack()
	}
}

// drainWorker processes remaining messages in the worker's channel.
func (kd *keyDispatcher) drainWorker(worker *keyWorker) {
	for {
		select {
		case km, ok := <-worker.msgCh:
			if !ok {
				return
			}
			kd.processMessage(km.ctx, km.msg)
		default:
			return
		}
	}
}

// removeWorker removes a worker from the map.
func (kd *keyDispatcher) removeWorker(key string) {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	delete(kd.workers, key)
}

// Close gracefully shuts down the dispatcher.
//
// It signals all workers to stop and waits for them to drain their channels.
// The context controls the maximum wait time.
func (kd *keyDispatcher) Close(ctx context.Context) error {
	var err error
	kd.closeOnce.Do(func() {
		// Signal all workers to stop
		kd.cancel()

		// Wait for all workers to finish
		done := make(chan struct{})
		go func() {
			kd.wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			// All workers finished
		case <-ctx.Done():
			err = ctx.Err()
		}
	})

	return err
}

// ActiveKeys returns the number of currently active key workers.
func (kd *keyDispatcher) ActiveKeys() int {
	kd.mu.RLock()
	defer kd.mu.RUnlock()
	return len(kd.workers)
}
