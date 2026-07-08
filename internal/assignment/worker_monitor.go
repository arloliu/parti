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
	"github.com/zeebo/xxh3"
)

// Package-private backoff constants for the heartbeat watcher retry loop.
// Declared as vars so tests can override.
var (
	workerWatcherBaseBackoff = 2 * time.Second
	workerWatcherMaxBackoff  = 30 * time.Second
	workerWatcherJitter      = 0.3 // ±30%
)

// suppressionHolidayFactor scales HeartbeatTTL into the suppression-holiday
// length opened by sweepExpired. 3×hbTTL covers the single-serialization
// worst-case emergency-confirmation chain:
//
//	firstSeen ≤ flag + 100ms (debounce) + hbTTL/2 (pollMu wait: one
//	in-flight observation) + hbTTL/2 (own scan, boundedOpCtx)
//	deadline  ≤ firstSeen + EmergencyGracePeriod (≤ HeartbeatTTL —
//	enforced by the PUBLIC parti.Config.Validate ltefield tag; the
//	internal assignment.Config does NOT re-enforce it, so direct
//	internal constructions must respect it themselves)
//	confirming entry ≤ deadline + HeartbeatInterval (≤ hbTTL/2)
//	              ≤ flag + 2.5×hbTTL + 100ms  < flag + 3×hbTTL
//	              (margin positive for hbTTL > 200ms; valid sub-200ms
//	              TTLs route through the polling backstop below, whose
//	              hbTTL/2 bound is faster than the debounce there)
//
// Deeper adversarial pollMu stacking falls to the polling backstop
// (+hbTTL/2) — the same worst-case bound the pre-suppression code
// degrades to under identical stacking. Do not shorten: 1×hbTTL fails
// at EmergencyGracePeriod == HeartbeatTTL; 2×hbTTL fails once the
// pollMu wait term is modeled.
const suppressionHolidayFactor = 3

// WorkerMonitor handles worker health detection via NATS KV heartbeats.
//
// It provides hybrid monitoring:
//   - Watcher (primary): Fast detection <100ms via NATS KV Watch.
//     Routine heartbeat refreshes are classified against a session-local
//     lastSeen map and suppressed (no check, no Keys() scan); joins,
//     graceful leaves, and sweep-detected silent expiries trigger checks.
//   - Polling (fallback): Reliable detection ~hbTTL/2 via periodic KV
//     scan; the ground-truth path for silent TTL expiry.
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

	// onLabelChangeCb is invoked (coalesced by the caller) whenever a heartbeat
	// PUT's labels differ from the retained fingerprint for that key — a label
	// change behind a live worker ID that the worker-change path cannot see.
	// Set once via SetOnLabelChange before Start, like onChangeCb; read only by
	// the watcher goroutine thereafter.
	onLabelChangeCb func()

	// labelFP maps a heartbeat key to the fingerprint of its last well-formed
	// label set. It is MONITOR-lifetime (survives watcher-session restarts) so a
	// takeover PUT that lands while the watch is down is caught when the next
	// session's initial replay delivers the key's current value and it differs
	// from the retained fingerprint. Guarded by labelFPMu — the watcher
	// goroutine mutates it and (in principle) concurrent readers are excluded.
	labelFPMu sync.Mutex
	labelFP   map[string]uint64

	logger types.Logger

	// watchBaseBackoff is the initial backoff for the watcher retry loop.
	// Defaults to workerWatcherBaseBackoff; tests may set a smaller value
	// on the struct before calling Start to avoid racy global mutations.
	watchBaseBackoff time.Duration

	// now returns the current time for watcher-entry classification and
	// the expiry sweep. Defaults to time.Now; tests inject a fake clock.
	now func() time.Time

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
		labelFP:          make(map[string]uint64),
		logger:           logger,
		watchBaseBackoff: workerWatcherBaseBackoff,
		now:              time.Now,
		stopCh:           make(chan struct{}),
		doneCh:           make(chan struct{}),
	}
}

// SetOnLabelChange registers a callback invoked when a heartbeat PUT's labels
// differ from the retained fingerprint for that key — a worker-label change
// behind a live worker ID (e.g. a stale-ID takeover) that the worker-set change
// path cannot detect. The callback is coalesced by the caller (the calculator
// routes it through requestLabelRecheck). It must be called before Start, like
// the constructor's onChange callback; the field is read only by the watcher
// goroutine afterward.
func (m *WorkerMonitor) SetOnLabelChange(fn func()) {
	m.onLabelChangeCb = fn
}

// classifyPutSuppressed records a PUT's receive time in lastSeen and reports
// whether it is a suppressible refresh — a key last seen < hbTTL ago while
// outside a suppression holiday means the worker was continuously alive, so
// topology cannot have changed and the expensive Keys()-scan check is skipped.
// It bumps suppressedCount on suppression. hbTTL==0: now.Sub(prev) is never
// negative under a forward clock, so this never suppresses — mirrors the
// explicit guard in sweepExpired.
func (m *WorkerMonitor) classifyPutSuppressed(key string, lastSeen map[string]time.Time, now, unsuppressedUntil time.Time, suppressedCount *uint64) bool {
	prev, known := lastSeen[key]
	lastSeen[key] = now
	if known && now.Sub(prev) < m.hbTTL && now.After(unsuppressedUntil) {
		*suppressedCount++
		return true
	}

	return false
}

// checkLabelChange updates the retained fingerprint for a heartbeat key from
// the PUT payload and reports whether the labels differ from a previously-seen
// value (level-triggered across watcher sessions — labelFP is monitor-lifetime).
// It is called on the watcher hot path once per PUT.
//
// Behavior:
//   - Malformed payload: no-op — neither reports a change nor erases the
//     retained fingerprint (a distinct decode failure must not churn state).
//   - First-seen key: seeds the fingerprint silently and reports no change
//     (joins are the worker-change path's job).
//   - Fingerprint mismatch: retains the new fingerprint, logs, invokes the
//     onLabelChange callback (outside labelFPMu), and reports a change.
func (m *WorkerMonitor) checkLabelChange(key string, value []byte) bool {
	hb, derr := types.DecodeHeartbeat(value)
	if derr != nil {
		return false
	}
	fp := labelFingerprint(hb.Labels)

	m.labelFPMu.Lock()
	prevFP, seen := m.labelFP[key]
	m.labelFP[key] = fp
	m.labelFPMu.Unlock()

	if !seen || prevFP == fp {
		return false
	}

	m.logger.Info("worker label change detected", "key", key)
	if m.onLabelChangeCb != nil {
		m.onLabelChangeCb()
	}

	return true
}

// dropLabelFingerprint forgets a heartbeat key's retained fingerprint on a
// DELETE/PURGE (a leave), so a later rejoin with new labels seeds silently as a
// first-seen join rather than firing a spurious label change.
func (m *WorkerMonitor) dropLabelFingerprint(key string) {
	m.labelFPMu.Lock()
	delete(m.labelFP, key)
	m.labelFPMu.Unlock()
}

// labelFingerprint hashes a label set into a 64-bit fingerprint used to detect
// label changes across heartbeat PUTs. 0 is reserved for "no labels" so an
// unlabeled worker fingerprints stably; a genuine hash that collides with 0 is
// remapped to 1. The input is assumed pre-sorted and deduplicated (heartbeat
// publishers sort labels at publish time), so the fingerprint is order-stable
// for a given label set without re-sorting on the watcher hot path.
func labelFingerprint(labels []string) uint64 {
	if len(labels) == 0 {
		return 0
	}
	var h xxh3.Hasher
	for i, l := range labels {
		if i > 0 {
			_, _ = h.WriteString("\n")
		}
		_, _ = h.WriteString(l)
	}
	fp := h.Sum64()
	if fp == 0 {
		fp = 1
	}

	return fp
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

// GetHeartbeatsFor returns decoded heartbeats for exactly the given
// worker IDs, keyed by worker ID. Unlike GetHeartbeats it does NOT run a
// Keys() scan — callers that already hold the active worker list (the
// rebalance path) use this to avoid a second stream-wide enumeration.
//
// Per-worker Get or decode failures omit that worker from the heartbeat
// map (logged at debug) but populate the error map under that worker ID
// with the original error, so the caller can classify the failure
// (connectivity/degrading-JetStream vs not) rather than have it
// masquerade as a bare "unknown labels" omission. The only non-nil
// returned error is context cancellation/deadline.
func (m *WorkerMonitor) GetHeartbeatsFor(ctx context.Context, workerIDs []string) (map[string]types.Heartbeat, map[string]error, error) {
	opCtx, cancel := m.boundedOpCtx(ctx)
	defer cancel()

	out := make(map[string]types.Heartbeat, len(workerIDs))
	fails := make(map[string]error)
	for _, workerID := range workerIDs {
		if err := opCtx.Err(); err != nil {
			return out, fails, fmt.Errorf("heartbeat fetch aborted: %w", err)
		}
		key := m.hbPrefix + "." + workerID
		entry, gerr := m.heartbeatKV.Get(opCtx, key)
		if gerr != nil {
			m.logger.Debug("heartbeat get failed during targeted fetch", "key", key, "error", gerr)
			fails[workerID] = gerr
			continue
		}
		hb, derr := types.DecodeHeartbeat(entry.Value())
		if derr != nil {
			m.logger.Debug("heartbeat decode failed during targeted fetch", "key", key, "error", derr)
			fails[workerID] = derr
			continue
		}
		out[workerID] = hb
	}

	return out, fails, nil
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
		if err := m.watcher.Stop(); err != nil && !natsutil.IsBenignWatcherStopErr(err) {
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
	// Watcher-session-local classification state: suppressedCount,
	// lastSeen, and unsuppressedUntil are owned exclusively by this
	// goroutine and rebuilt from the initial replay after every watcher
	// restart, so no staleness survives a session boundary. A PUT of a
	// key last seen < hbTTL ago is a refresh — the worker was
	// continuously alive, so topology cannot have changed and the
	// (expensive) Keys()-scan check is skipped. Unknown/stale PUTs
	// (joins) and DELETE/PURGE (graceful leaves) trigger as before.
	var suppressedCount uint64
	lastSeen := make(map[string]time.Time)
	var unsuppressedUntil time.Time
	defer func() {
		m.logger.Debug("watcher session ended", "suppressed_refreshes", suppressedCount)
		if serr := watcher.Stop(); serr != nil && !natsutil.IsBenignWatcherStopErr(serr) {
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

			now := m.now()
			trigger := true
			switch entry.Operation() {
			case jetstream.KeyValuePut:
				key := entry.Key()
				if m.classifyPutSuppressed(key, lastSeen, now, unsuppressedUntil, &suppressedCount) {
					// Refresh under active suppression: worker was
					// continuously alive; skip the check.
					trigger = false
				}

				// Label fingerprint: level-triggered across watcher sessions.
				// A label change is never a suppressible refresh.
				if m.checkLabelChange(key, entry.Value()) {
					trigger = true
				}
			case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
				delete(lastSeen, entry.Key())
				// Drop the retained fingerprint: a leave. A rejoin with new
				// labels is a first-seen join covered by the worker-change path,
				// so it must seed silently rather than fire a spurious change.
				m.dropLabelFingerprint(entry.Key())
			}

			if m.sweepExpired(lastSeen, now, &unsuppressedUntil) {
				trigger = true
			}

			if trigger && !pendingCheck {
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

// sweepExpired flags tracked workers whose heartbeat key must have
// expired server-side (no PUT received for ≥ hbTTL). Flagged entries are
// removed from lastSeen (a later PUT from the same worker classifies as
// a join). When anything is flagged it opens a suppression holiday of
// suppressionHolidayFactor×hbTTL so follow-up observation — including
// emergency-grace progression and confirmation — runs at the
// pre-suppression refresh-triggered cadence for the crash window.
//
// lastSeen records receive time, which trails server publish time, so a
// delayed delivery can flag a still-alive worker (false flag). That is
// safe by construction: the triggered check performs a real Keys() scan
// and an unchanged worker set short-circuits; the cost is one extra
// check when the flagged worker's next PUT re-joins.
//
// hbTTL == 0 disables the sweep (direct unit seams only; Start requires
// a positive hbTTL for its polling ticker).
func (m *WorkerMonitor) sweepExpired(lastSeen map[string]time.Time, now time.Time, unsuppressedUntil *time.Time) bool {
	if m.hbTTL == 0 {
		return false
	}
	holidayUntil := now.Add(suppressionHolidayFactor * m.hbTTL)
	flagged := false
	for k, seen := range lastSeen {
		if now.Sub(seen) >= m.hbTTL {
			delete(lastSeen, k)
			flagged = true
			m.logger.Debug("watcher: heartbeat key expired without event; forcing check",
				"key", k, "holiday_until", holidayUntil)
		}
	}
	if flagged {
		*unsuppressedUntil = holidayUntil
	}

	return flagged
}
