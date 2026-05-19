package assignment

import (
	"context"
	"errors"
	"fmt"
	rand "math/rand/v2"
	"sync"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// Package-private backoff constants for the heartbeat watcher retry loop.
// Declared as vars so tests can override.
var (
	workerWatcherBaseBackoff = 2 * time.Second
	workerWatcherMaxBackoff  = 30 * time.Second
	workerWatcherJitter      = 0.3 // ±30%
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

	// watchBaseBackoff is the initial backoff for the watcher retry loop.
	// Defaults to workerWatcherBaseBackoff; tests may set a smaller value
	// on the struct before calling Start to avoid racy global mutations.
	watchBaseBackoff time.Duration

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
		heartbeatKV:      heartbeatKV,
		hbPrefix:         hbPrefix,
		hbTTL:            hbTTL,
		hbWatchPattern:   fmt.Sprintf("%s.*", hbPrefix),
		onChangeCb:       onChange,
		logger:           logger,
		watchBaseBackoff: workerWatcherBaseBackoff,
		stopCh:           make(chan struct{}),
		doneCh:           make(chan struct{}),
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
	opCtx, cancel := m.boundedOpCtx(ctx)
	defer cancel()

	// List all keys with heartbeat prefix
	keys, err := m.heartbeatKV.Keys(opCtx)
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

// GetHeartbeats returns the decoded heartbeats for every worker with an
// active heartbeat key. The map is keyed by worker ID.
//
// Decoding accepts both v1 JSON heartbeats (new workers) and legacy
// RFC3339 timestamp strings (pre-v1 workers); see types.DecodeHeartbeat.
// Workers whose payload fails to decode are silently omitted — a malformed
// heartbeat is logged at debug level but does not fail the entire scan.
//
// Returns:
//   - map[string]types.Heartbeat: Decoded heartbeats, keyed by worker ID
//   - error: Non-nil only on KV access failure that prevents listing keys
func (m *WorkerMonitor) GetHeartbeats(ctx context.Context) (map[string]types.Heartbeat, error) {
	opCtx, cancel := m.boundedOpCtx(ctx)
	defer cancel()

	keys, err := m.heartbeatKV.Keys(opCtx)
	if err != nil {
		if types.IsNoKeysFoundError(err) {
			return map[string]types.Heartbeat{}, nil
		}

		return nil, fmt.Errorf("failed to list heartbeat keys: %w", err)
	}

	out := make(map[string]types.Heartbeat, len(keys))
	for _, key := range keys {
		// Use the same prefix-strip rule as GetActiveWorkers.
		if len(key) <= len(m.hbPrefix)+1 || key[:len(m.hbPrefix)] != m.hbPrefix {
			continue
		}
		workerID := key[len(m.hbPrefix)+1:]

		entry, gerr := m.heartbeatKV.Get(ctx, key)
		if gerr != nil {
			m.logger.Debug("heartbeat get failed during scan", "key", key, "error", gerr)
			continue
		}
		hb, derr := types.DecodeHeartbeat(entry.Value())
		if derr != nil {
			m.logger.Debug("heartbeat decode failed during scan", "key", key, "error", derr)
			continue
		}
		out[workerID] = hb
	}

	return out, nil
}

// boundedOpCtx returns a child context with a deadline bounded to hbTTL/2.
// If hbTTL is zero (e.g. unit tests that construct WorkerMonitor directly),
// the parent context is returned unchanged so callers are not immediately
// cancelled. The caller must always call the returned cancel function.
func (m *WorkerMonitor) boundedOpCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.hbTTL == 0 {
		return context.WithCancel(ctx)
	}
	opTimeout := m.hbTTL / 2
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < opTimeout {
			opTimeout = remaining
		}
	}

	return context.WithTimeout(ctx, opTimeout)
}

// monitorWorkers runs the hybrid monitoring loop.
//
// This is the main goroutine that coordinates watcher and polling.
// It signals doneCh when exiting to allow Stop() to complete.
func (m *WorkerMonitor) monitorWorkers(ctx context.Context) {
	defer close(m.doneCh)

	// Start watcher in a separate goroutine with rewatch-on-close.
	go m.monitorWatcherWithRetry(ctx)

	// Polling ticker for worker changes (fallback for silent-stall and
	// watcher-close backoff gaps). Runs independently of the watcher.
	ticker := time.NewTicker(m.hbTTL / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if m.onChangeCb != nil {
				if err := m.onChangeCb(ctx); err != nil {
					m.logger.Error("polling error", "error", err)
				}
			}

		case <-m.stopCh:
			return

		case <-ctx.Done():
			return
		}
	}
}

// monitorWatcherWithRetry retries processWatcherEvents on failure with
// exponential backoff + jitter. It exits when ctx is cancelled or
// m.stopCh is closed.
func (m *WorkerMonitor) monitorWatcherWithRetry(ctx context.Context) {
	backoff := m.watchBaseBackoff
	for {
		err := m.processWatcherEvents(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		select {
		case <-m.stopCh:
			return
		default:
		}
		m.logger.Warn("heartbeat watcher failed, retrying", "error", err, "backoff", backoff)

		//nolint:gosec // jitter does not require crypto-secure random
		f := rand.Float64()
		low := 1 - workerWatcherJitter
		high := 1 + workerWatcherJitter
		delay := time.Duration(float64(backoff) * (low + f*(high-low)))

		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-time.After(delay):
		}

		backoff = min(backoff*2, workerWatcherMaxBackoff)
	}
}

// stopWatcher stops the NATS KV watcher.
func (m *WorkerMonitor) stopWatcher() {
	m.watcherMu.Lock()
	defer m.watcherMu.Unlock()

	if m.watcher != nil {
		if err := m.watcher.Stop(); err != nil && !natsutil.IsConsumerNotFound(err) {
			m.logger.Warn("failed to stop watcher", "error", err)
		}
		m.watcher = nil
		m.logger.Debug("watcher stopped")
	}
}

// processWatcherEvents runs one watch session on all heartbeat keys.
// Channel closure or initial Watch failure is returned as an error so
// monitorWatcherWithRetry can restart with backoff. Context cancellation
// or m.stopCh closure returns nil for clean exit.
func (m *WorkerMonitor) processWatcherEvents(ctx context.Context) error {
	watcher, err := m.heartbeatKV.Watch(ctx, m.hbWatchPattern)
	if err != nil {
		return fmt.Errorf("failed to start heartbeat watcher: %w", err)
	}
	defer func() {
		if serr := watcher.Stop(); serr != nil && !natsutil.IsConsumerNotFound(serr) {
			m.logger.Warn("failed to stop heartbeat watcher", "error", serr)
		}
		m.watcherMu.Lock()
		m.watcher = nil
		m.watcherMu.Unlock()
	}()

	m.watcherMu.Lock()
	m.watcher = watcher
	m.watcherMu.Unlock()
	m.logger.Info("heartbeat watcher started", "pattern", m.hbWatchPattern)

	debounceTimer := time.NewTimer(100 * time.Millisecond)
	debounceTimer.Stop()
	var pendingCheck bool

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-m.stopCh:
			return nil
		case entry, ok := <-watcher.Updates():
			if !ok {
				return errors.New("heartbeat watcher channel closed")
			}
			if entry == nil {
				continue
			}
			m.logger.Debug("watcher: received entry", "key", entry.Key(), "operation", entry.Operation())
			if !pendingCheck {
				pendingCheck = true
				debounceTimer.Reset(100 * time.Millisecond)
			}
		case <-debounceTimer.C:
			if pendingCheck {
				pendingCheck = false
				m.logger.Debug("watcher detected change, triggering check")
				if m.onChangeCb != nil {
					if err := m.onChangeCb(ctx); err != nil {
						m.logger.Error("watcher-triggered check failed", "error", err)
					}
				}
			}
		}
	}
}
