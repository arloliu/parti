package assignment

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
)

// WorkerMonitor handles worker health detection via NATS KV heartbeats.
//
// It provides hybrid monitoring:
//   - Watcher (primary): Fast detection <100ms via NATS KV Watch
//   - Polling (fallback): Reliable detection ~1.5s via periodic KV scan
//
// The monitor runs in a background goroutine and invokes a callback
// when worker topology changes are detected.
type WorkerMonitor struct {
	heartbeatKV    jetstream.KeyValue
	hbPrefix       string
	hbTTL          time.Duration
	hbWatchPattern string // cached "hbPrefix.*"

	watcher   jetstream.KeyWatcher
	watcherMu sync.Mutex

	// Callback invoked when changes are detected (from polling or watcher)
	onChangeCb func(ctx context.Context) error

	logger types.Logger

	// Lifecycle management
	mu      sync.Mutex
	started bool
	stopped bool
	stopCh  chan struct{}
	doneCh  chan struct{}
}

// NewWorkerMonitor creates a new worker monitor.
//
// Parameters:
//   - heartbeatKV: NATS KV bucket for worker heartbeats
//   - hbPrefix: Prefix for heartbeat keys (e.g., "worker")
//   - hbTTL: Heartbeat TTL duration
//   - onChange: Callback invoked when worker changes are detected
//   - logger: Logger for monitoring events
//
// Returns:
//   - *WorkerMonitor: A new worker monitor instance
func NewWorkerMonitor(
	heartbeatKV jetstream.KeyValue,
	hbPrefix string,
	hbTTL time.Duration,
	onChange func(ctx context.Context) error,
	logger types.Logger,
) *WorkerMonitor {
	return &WorkerMonitor{
		heartbeatKV:    heartbeatKV,
		hbPrefix:       hbPrefix,
		hbTTL:          hbTTL,
		hbWatchPattern: fmt.Sprintf("%s.*", hbPrefix),
		onChangeCb:     onChange,
		logger:         logger,
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
	}
}

// Start begins monitoring workers in a background goroutine.
//
// The monitor uses a hybrid approach:
//  1. NATS KV watcher for fast detection (~100ms)
//  2. Periodic polling as fallback (every hbTTL/2)
//
// Parameters:
//   - ctx: Context for cancellation (affects watcher lifetime)
//
// Returns:
//   - error: Error if already started or already stopped
func (m *WorkerMonitor) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check stopped first - once stopped, cannot restart
	if m.stopped {
		return types.ErrWorkerMonitorAlreadyStopped
	}
	if m.started {
		return types.ErrWorkerMonitorAlreadyStarted
	}

	m.started = true
	go m.monitorWorkers(ctx)

	return nil
}

// Stop stops the worker monitor and waits for cleanup.
//
// This method blocks until all monitoring goroutines have exited.
// It is safe to call Stop multiple times - subsequent calls will return immediately.
//
// Returns:
//   - error: Error if Stop called before Start, nil otherwise
func (m *WorkerMonitor) Stop() error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return types.ErrWorkerMonitorNotStarted
	}
	if m.stopped {
		m.mu.Unlock()
		return nil // Already stopped - idempotent
	}
	m.stopped = true
	m.mu.Unlock()

	// Signal stop
	close(m.stopCh)

	// Wait for monitor goroutine to finish
	<-m.doneCh

	// Cleanup watcher
	m.stopWatcher()

	return nil
}

// GetActiveWorkers retrieves the list of workers with active heartbeats.
//
// This method scans the heartbeat KV bucket for keys matching the configured
// prefix and extracts worker IDs from the key names.
//
// Parameters:
//   - ctx: Context for cancellation
//
// Returns:
//   - []string: List of active worker IDs
//   - error: Nil on success, error on KV access failure
func (m *WorkerMonitor) GetActiveWorkers(ctx context.Context) ([]string, error) {
	// List all keys with heartbeat prefix
	keys, err := m.heartbeatKV.Keys(ctx)
	if err != nil {
		// Handle "no keys found" as empty list
		if types.IsNoKeysFoundError(err) {
			m.logger.Debug("no heartbeat keys found")
			return []string{}, nil
		}

		return nil, fmt.Errorf("failed to list heartbeat keys: %w", err)
	}

	m.logger.Debug("scanning heartbeat keys", "total_keys", len(keys), "hb_prefix", m.hbPrefix)

	workers := make([]string, 0, len(keys))
	for _, key := range keys {
		// Extract worker ID from key (format: "hbPrefix.workerID")
		if len(key) > len(m.hbPrefix)+1 && key[:len(m.hbPrefix)] == m.hbPrefix {
			workerID := key[len(m.hbPrefix)+1:]
			workers = append(workers, workerID)
			m.logger.Debug("found active worker heartbeat", "key", key, "worker_id", workerID)
		} else {
			m.logger.Debug("skipping non-heartbeat key", "key", key, "hb_prefix", m.hbPrefix)
		}
	}

	m.logger.Debug("active workers discovered", "count", len(workers), "workers", workers)

	return workers, nil
}

// monitorWorkers runs the hybrid monitoring loop.
//
// This is the main goroutine that coordinates watcher and polling.
// It signals doneCh when exiting to allow Stop() to complete.
func (m *WorkerMonitor) monitorWorkers(ctx context.Context) {
	defer close(m.doneCh)

	// Start watcher for fast detection
	if err := m.startWatcher(ctx); err != nil {
		m.logger.Warn("failed to start watcher, falling back to polling only", "error", err)
	}

	// Polling ticker for worker changes (fallback)
	ticker := time.NewTicker(m.hbTTL / 2) // Check twice per TTL
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Check for worker changes via polling (fallback)
			if m.onChangeCb != nil {
				if err := m.onChangeCb(ctx); err != nil {
					m.logger.Error("polling error", "error", err)
				}
			}

		case <-m.stopCh:
			m.stopWatcher()
			return

		case <-ctx.Done():
			m.stopWatcher()
			return
		}
	}
}

// startWatcher starts the NATS KV watcher for fast worker change detection.
func (m *WorkerMonitor) startWatcher(ctx context.Context) error {
	m.watcherMu.Lock()
	defer m.watcherMu.Unlock()

	if m.watcher != nil {
		return nil // Already started
	}

	// Watch all heartbeat keys with prefix pattern
	watcher, err := m.heartbeatKV.Watch(ctx, m.hbWatchPattern)
	if err != nil {
		return fmt.Errorf("failed to start watcher: %w", err)
	}

	m.watcher = watcher
	m.logger.Info("watcher started for fast worker detection",
		"pattern", m.hbWatchPattern,
		"hbPrefix", m.hbPrefix,
	)

	// Start goroutine to process watcher events
	go m.processWatcherEvents(ctx)

	return nil
}

// stopWatcher stops the NATS KV watcher.
func (m *WorkerMonitor) stopWatcher() {
	m.watcherMu.Lock()
	defer m.watcherMu.Unlock()

	if m.watcher != nil {
		if err := m.watcher.Stop(); err != nil {
			m.logger.Warn("failed to stop watcher", "error", err)
		}
		m.watcher = nil
		m.logger.Debug("watcher stopped")
	}
}

// processWatcherEvents processes events from the NATS KV watcher.
//
// This goroutine debounces rapid events to avoid excessive checks.
// It notifies the calculator when worker topology changes are detected.
func (m *WorkerMonitor) processWatcherEvents(ctx context.Context) {
	m.logger.Debug("watcher event processor goroutine started")
	defer m.logger.Debug("watcher event processor goroutine stopped")

	m.watcherMu.Lock()
	watcher := m.watcher
	m.watcherMu.Unlock()

	if watcher == nil {
		m.logger.Warn("watcher is nil, cannot process events")
		return
	}

	// Debounce rapid events
	debounceTimer := time.NewTimer(100 * time.Millisecond)
	debounceTimer.Stop() // Stop initially
	var pendingCheck bool

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case entry := <-watcher.Updates():
			if entry == nil {
				// Watcher stopped or initial replay done
				m.logger.Debug("watcher: nil entry (replay done or stopped)")
				continue
			}

			// Heartbeat key changed (worker added or updated)
			m.logger.Debug("watcher: received entry", "key", entry.Key(), "operation", entry.Operation())

			// Schedule a debounced check
			if !pendingCheck {
				pendingCheck = true
				debounceTimer.Reset(100 * time.Millisecond)
			}

		case <-debounceTimer.C:
			if pendingCheck {
				pendingCheck = false
				m.logger.Debug("watcher detected change, triggering check")

				// Invoke callback to handle the change
				if m.onChangeCb != nil {
					if err := m.onChangeCb(ctx); err != nil {
						m.logger.Error("watcher-triggered check failed", "error", err)
					}
				}
			}
		}
	}
}
