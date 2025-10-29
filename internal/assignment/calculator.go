package assignment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/internal/metrics"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/puzpuzpuz/xsync/v4"
)

// stateSubscriber is a helper for managing state change subscriptions.
type stateSubscriber struct {
	ch     chan types.CalculatorState
	mu     sync.Mutex
	closed bool
}

// trySend sends a state update to the subscriber's channel without blocking.
func (s *stateSubscriber) trySend(state types.CalculatorState, metricsCollector types.MetricsCollector) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	select {
	case s.ch <- state:
	default:
		// Subscriber is slow or not ready; they will get the next update.
		// TODO: Add metricsCollector.RecordSlowSubscriber() when available
		_ = metricsCollector // Avoid unused parameter warning
	}
}

// close safely closes the subscriber's channel.
func (s *stateSubscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}

// Calculator manages partition assignment calculation and distribution.
//
// The calculator runs on the leader worker and is responsible for:
//   - Monitoring worker health via heartbeats
//   - Calculating partition assignments using the configured strategy
//   - Publishing assignments to NATS KV for worker discovery
//   - Handling rebalancing triggers (worker join/leave events)
//   - Applying rebalance cooldown to prevent thrashing
//
// The calculator does NOT run on follower workers.
type Calculator struct {
	assignmentKV jetstream.KeyValue // KV bucket for assignments
	heartbeatKV  jetstream.KeyValue // KV bucket for heartbeats
	prefix       string
	source       types.PartitionSource
	strategy     types.AssignmentStrategy

	// Configuration
	hbPrefix        string
	hbTTL           time.Duration
	cooldown        time.Duration
	minThreshold    float64
	restartRatio    float64
	coldStartWindow time.Duration
	plannedScaleWin time.Duration

	// Optional dependencies
	metrics types.MetricsCollector
	logger  types.Logger

	// State management
	mu                 sync.RWMutex
	started            bool
	currentVersion     int64
	currentWorkers     map[string]bool
	currentAssignments map[string][]types.Partition
	lastRebalance      time.Time

	// Calculator state machine
	calcState     atomic.Int32    // types.CalculatorState
	scalingStart  time.Time       // When scaling window started
	scalingReason string          // Reason: "cold_start", "planned_scale", "emergency", "restart"
	lastWorkers   map[string]bool // Previous worker set for comparison

	// Emergency detection
	emergencyDetector *EmergencyDetector // Hysteresis-based emergency detection

	// State management fan-out
	subscribers      *xsync.Map[string, *stateSubscriber]
	nextSubscriberID atomic.Uint64

	// Hybrid monitoring: watcher (primary) + polling (fallback)
	watcher   jetstream.KeyWatcher
	watcherMu sync.Mutex // Protects watcher lifecycle

	// Lifecycle
	stopCh chan struct{}
	doneCh chan struct{}
}

// NewCalculator creates a new assignment calculator.
//
// The calculator monitors worker health and calculates partition assignments.
// It should only be started on the leader worker.
//
// Parameters:
//   - assignmentKV: NATS KV bucket for storing assignments
//   - heartbeatKV: NATS KV bucket for reading worker heartbeats
//   - prefix: Key prefix for assignment storage (e.g., "assignment")
//   - source: Partition source for discovering available partitions
//   - strategy: Assignment strategy for distributing partitions
//   - hbPrefix: Heartbeat key prefix for monitoring workers
//   - hbTTL: Heartbeat TTL duration for detecting dead workers
//   - emergencyGracePeriod: Minimum time workers must be missing before emergency (hysteresis)
//
// Returns:
//   - *Calculator: New calculator instance ready to start
func NewCalculator(
	assignmentKV jetstream.KeyValue,
	heartbeatKV jetstream.KeyValue,
	prefix string,
	source types.PartitionSource,
	strategy types.AssignmentStrategy,
	hbPrefix string,
	hbTTL time.Duration,
	emergencyGracePeriod time.Duration,
) *Calculator {
	c := &Calculator{
		assignmentKV:       assignmentKV,
		heartbeatKV:        heartbeatKV,
		prefix:             prefix,
		source:             source,
		strategy:           strategy,
		hbPrefix:           hbPrefix,
		hbTTL:              hbTTL,
		cooldown:           10 * time.Second,
		minThreshold:       0.2,
		restartRatio:       0.5,
		coldStartWindow:    30 * time.Second,
		plannedScaleWin:    10 * time.Second,
		metrics:            metrics.NewNop(),
		logger:             logging.NewNop(),
		currentWorkers:     make(map[string]bool),
		currentAssignments: make(map[string][]types.Partition),
		lastWorkers:        make(map[string]bool),
		subscribers:        xsync.NewMap[string, *stateSubscriber](),
		stopCh:             make(chan struct{}),
		doneCh:             make(chan struct{}),
	}
	// Initialize calculator state to Idle
	c.calcState.Store(int32(types.CalcStateIdle))

	// Initialize emergency detector with configured grace period
	c.emergencyDetector = NewEmergencyDetector(emergencyGracePeriod)

	return c
}

// SetCooldown sets the rebalance cooldown duration.
//
// The cooldown prevents excessive rebalancing by enforcing a minimum time
// between consecutive rebalance operations.
//
// Parameters:
//   - cooldown: Minimum time between rebalances (default: 10s)
func (c *Calculator) SetCooldown(cooldown time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cooldown = cooldown
}

// SetMinThreshold sets the minimum rebalance threshold.
//
// Rebalancing only occurs if the partition imbalance exceeds this threshold.
//
// Parameters:
//   - threshold: Minimum partition distribution imbalance (default: 0.2 = 20%)
func (c *Calculator) SetMinThreshold(threshold float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.minThreshold = threshold
}

// SetRestartDetectionRatio sets the restart detection ratio.
//
// Used to distinguish between cold starts (many workers) and planned scaling
// (few workers). Affects stabilization window selection.
//
// Parameters:
//   - ratio: Fraction of workers that indicates cold start (default: 0.5)
func (c *Calculator) SetRestartDetectionRatio(ratio float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restartRatio = ratio
}

// SetStabilizationWindows sets the cold start and planned scale windows.
//
// These windows determine how long to wait before initial assignment:
//   - Cold start: Longer window (30s) to wait for most workers
//   - Planned scale: Shorter window (10s) for quick response
//
// Parameters:
//   - coldStart: Wait time for cold start scenarios (default: 30s)
//   - plannedScale: Wait time for planned scaling (default: 10s)
func (c *Calculator) SetStabilizationWindows(coldStart, plannedScale time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.coldStartWindow = coldStart
	c.plannedScaleWin = plannedScale
}

// SetMetrics sets the metrics collector.
func (c *Calculator) SetMetrics(m types.MetricsCollector) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m == nil {
		c.metrics = metrics.NewNop()
	} else {
		c.metrics = m
	}
}

// SetLogger sets the logger.
func (c *Calculator) SetLogger(l types.Logger) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l == nil {
		c.logger = logging.NewNop()
	} else {
		c.logger = l
	}
}

// SubscribeToStateChanges returns a channel for state updates and a function to unsubscribe.
func (c *Calculator) SubscribeToStateChanges() (<-chan types.CalculatorState, func()) {
	id := c.nextSubscriberID.Add(1)
	key := strconv.FormatUint(id, 10)

	// Buffer size of 4 allows Idle -> Scaling -> Rebalancing -> Idle transitions
	// to be queued without dropping states when subscriber is slow to process
	sub := &stateSubscriber{ch: make(chan types.CalculatorState, 4)}
	c.subscribers.Store(key, sub)

	// Immediately send the current state.
	sub.trySend(c.GetState(), c.metrics)

	unsubscribe := func() {
		c.removeSubscriber(key)
	}

	return sub.ch, unsubscribe
}

func (c *Calculator) removeSubscriber(key string) {
	if sub, ok := c.subscribers.LoadAndDelete(key); ok {
		sub.close()
	}
}

// emitStateChange notifies all subscribers of a state transition.
func (c *Calculator) emitStateChange(state types.CalculatorState) {
	oldState := c.GetState()
	if oldState == state {
		return // No change, no notification needed.
	}

	c.calcState.Store(int32(state)) //nolint:gosec // G115: state is bounded enum, safe conversion
	c.logger.Info("state transition", "from", oldState, "to", state)

	c.subscribers.Range(func(key string, sub *stateSubscriber) bool {
		sub.trySend(state, c.metrics)
		return true
	})
}

// discoverHighestVersion scans existing assignments in KV to find the highest version.
// This ensures version monotonicity across leader changes.
func (c *Calculator) discoverHighestVersion(ctx context.Context) error {
	keys, err := c.assignmentKV.Keys(ctx)
	if err != nil {
		return fmt.Errorf("failed to list KV keys: %w", err)
	}

	c.logger.Debug("discovering highest version", "total_keys", len(keys), "prefix", c.prefix)

	highestVersion := int64(0)
	checkedCount := 0
	for _, key := range keys {
		// Skip non-assignment keys (heartbeats, etc.)
		if !strings.HasPrefix(key, c.prefix+".") {
			c.logger.Debug("skipping non-assignment key", "key", key, "prefix", c.prefix)
			continue
		}

		checkedCount++
		entry, err := c.assignmentKV.Get(ctx, key)
		if err != nil {
			c.logger.Debug("failed to read assignment key", "key", key, "error", err)
			continue // Skip entries we can't read
		}

		var asgn types.Assignment
		if err := json.Unmarshal(entry.Value(), &asgn); err != nil {
			c.logger.Debug("failed to unmarshal assignment", "key", key, "error", err)
			continue // Skip malformed entries
		}

		c.logger.Debug("found assignment", "key", key, "version", asgn.Version)
		if asgn.Version > highestVersion {
			highestVersion = asgn.Version
		}
	}

	c.mu.Lock()
	c.currentVersion = highestVersion
	c.mu.Unlock()

	if highestVersion > 0 {
		c.logger.Info("discovered existing assignments", "highest_version", highestVersion, "checked_keys", checkedCount)
	} else {
		c.logger.Debug("no existing assignments found", "checked_keys", checkedCount)
	}

	return nil
}

// Start begins monitoring workers and calculating assignments.
//
// This method should only be called on the leader worker. It:
//  1. Performs initial assignment (with stabilization window)
//  2. Starts background monitoring for worker changes
//  3. Triggers rebalancing when workers join/leave
//
// Parameters:
//   - ctx: Context for cancellation and timeout
//
// Returns:
//   - error: Start error (e.g., already started, KV operation failed)
func (c *Calculator) Start(ctx context.Context) error {
	c.logger.Info("calculator Start() called")

	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return errors.New("calculator already started")
	}
	c.started = true
	c.mu.Unlock()

	// Discover highest version from existing assignments to ensure monotonicity across leader changes
	if err := c.discoverHighestVersion(ctx); err != nil {
		c.logger.Warn("failed to discover existing versions, starting from 0", "error", err)
	}

	c.logger.Info("performing initial assignment")

	// Perform initial assignment with stabilization window
	if err := c.initialAssignment(ctx); err != nil {
		c.mu.Lock()
		c.started = false
		c.mu.Unlock()

		return fmt.Errorf("initial assignment failed: %w", err)
	}

	// Start hybrid monitoring: watcher (primary) + polling (fallback)
	go c.monitorWorkers(ctx)

	return nil
}

// Stop stops the calculator and waits for background goroutines to finish.
//
// Returns:
//   - error: Stop error (e.g., not started)
func (c *Calculator) Stop() error {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return errors.New("calculator not started")
	}
	c.mu.Unlock()

	// Stop watcher if running
	c.watcherMu.Lock()
	if c.watcher != nil {
		if err := c.watcher.Stop(); err != nil {
			c.logger.Error("failed to stop watcher", "error", err)
		}
		c.watcher = nil
	}
	c.watcherMu.Unlock()

	close(c.stopCh)
	<-c.doneCh

	c.mu.Lock()
	c.started = false
	c.mu.Unlock()

	// Clean up all subscribers.
	c.subscribers.Range(func(key string, sub *stateSubscriber) bool {
		c.subscribers.Delete(key)
		sub.close()
		return true
	})

	return nil
}

// IsStarted returns true if the calculator is currently running.
func (c *Calculator) IsStarted() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.started
}

// GetState returns the current calculator state.
//
// Returns:
//   - types.CalculatorState: Current calculator state (type-safe enum)
func (c *Calculator) GetState() types.CalculatorState {
	return types.CalculatorState(c.calcState.Load())
}

// CurrentVersion returns the current assignment version.
func (c *Calculator) CurrentVersion() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentVersion
}

// TriggerRebalance forces an immediate rebalance, bypassing cooldown.
//
// This is useful when partitions are added/removed dynamically and you want
// to redistribute them immediately without waiting for the next worker change.
//
// Parameters:
//   - ctx: Context for operation timeout
//
// Returns:
//   - error: Rebalance error
func (c *Calculator) TriggerRebalance(ctx context.Context) error {
	if !c.IsStarted() {
		return errors.New("calculator not started")
	}

	c.logger.Info("manual rebalance triggered")

	return c.rebalance(ctx, "manual-refresh")
}

// initialAssignment performs the first assignment with stabilization window.
func (c *Calculator) initialAssignment(ctx context.Context) error {
	// Wait for stabilization window
	window := c.selectStabilizationWindow(ctx)
	c.logger.Info("waiting for stabilization", "window", window)

	select {
	case <-time.After(window):
		// Proceed with assignment
	case <-ctx.Done():
		return ctx.Err()
	}

	// Calculate and publish initial assignment
	if err := c.rebalance(ctx, "initial"); err != nil {
		return err
	}

	// Initialize lastWorkers with the workers from initial assignment
	// This prevents immediately re-entering scaling when monitorWorkers starts
	c.mu.Lock()
	c.lastWorkers = make(map[string]bool)
	for w := range c.currentWorkers {
		c.lastWorkers[w] = true
	}
	workerCount := len(c.lastWorkers)
	c.mu.Unlock()

	c.logger.Info("initial assignment complete", "workers", workerCount)

	return nil
}

// detectRebalanceType determines the type of rebalance needed based on worker topology changes.
//
// This method classifies the rebalance scenario:
//   - Emergency: Workers disappeared beyond grace period → No window, immediate rebalance
//   - Cold start: Starting from 0 workers → Use 30s stabilization window
//   - Planned scale: Gradual worker additions → Use 10s stabilization window
//
// Parameters:
//   - currentWorkers: Current set of active workers
//
// Returns:
//   - reason: Rebalance type ("emergency", "cold_start", "planned_scale", or "" if in grace period)
//   - window: Stabilization window duration (0 for emergency or during grace period)
func (c *Calculator) detectRebalanceType(currentWorkers map[string]bool) (reason string, window time.Duration) {
	prevCount := len(c.lastWorkers)
	currCount := len(currentWorkers)

	// Case 1: Worker(s) disappeared - Check for emergency with hysteresis
	if currCount < prevCount {
		emergency, disappearedWorkers := c.emergencyDetector.CheckEmergency(c.lastWorkers, currentWorkers)

		if emergency {
			c.logger.Warn("emergency: workers disappeared beyond grace period",
				"disappeared", disappearedWorkers,
				"prev_count", prevCount,
				"curr_count", currCount,
			)

			return "emergency", 0 // No stabilization - immediate action
		}

		// Still in grace period - no action yet
		c.logger.Info("workers disappeared but within grace period",
			"prev_count", prevCount,
			"curr_count", currCount,
		)

		return "", 0 // Wait for grace period to expire
	}

	// Case 2: Cold start - First workers joining
	if prevCount == 0 {
		c.logger.Info("cold start detected",
			"worker_count", currCount,
			"window", c.coldStartWindow,
		)

		return "cold_start", c.coldStartWindow
	}

	// Case 3: Planned scale - Worker(s) added
	c.logger.Info("planned scale detected",
		"prev_count", prevCount,
		"curr_count", currCount,
		"window", c.plannedScaleWin,
	)

	return "planned_scale", c.plannedScaleWin
}

// enterScalingState transitions the calculator into scaling state with stabilization window.
//
// Parameters:
//   - ctx: Context for cancellation
//   - reason: Reason for scaling ("cold_start", "planned_scale", "restart")
//   - window: Stabilization window duration to wait before rebalancing
func (c *Calculator) enterScalingState(ctx context.Context, reason string, window time.Duration) {
	// Only enter if currently idle
	oldState := types.CalculatorState(c.calcState.Swap(int32(types.CalcStateScaling)))
	if oldState != types.CalcStateIdle {
		c.logger.Warn("attempted to enter scaling state from non-idle state",
			"current_state", oldState.String(),
			"reason", reason)
		// Restore old state
		c.calcState.Store(int32(oldState)) //nolint:gosec // State values are small constants

		return
	}

	c.mu.Lock()
	c.scalingStart = time.Now()
	c.scalingReason = reason
	c.mu.Unlock()

	c.logger.Info("entering scaling state",
		"reason", reason,
		"window", window,
	)

	// Notify Manager of state change
	c.emitStateChange(types.CalcStateScaling)

	// Start timer for scaling window
	go func() {
		c.logger.Info("scaling timer goroutine started", "window", window)
		select {
		case <-time.After(window):
			c.logger.Info("scaling timer fired, entering rebalancing state")
			c.enterRebalancingState(ctx)
		case <-c.stopCh:
			c.logger.Info("scaling timer cancelled by stopCh")
			return
		case <-ctx.Done():
			c.logger.Info("scaling timer cancelled by context", "error", ctx.Err())
			return
		}
	}()
}

// enterRebalancingState transitions the calculator into rebalancing state.
//
// This triggers the actual assignment calculation and publishes new assignments.
//
// Parameters:
//   - ctx: Context for rebalance operation
func (c *Calculator) enterRebalancingState(ctx context.Context) {
	c.logger.Info("entering rebalancing state")

	// Notify Manager of state change
	c.emitStateChange(types.CalcStateRebalancing)

	// Perform rebalance
	if err := c.rebalance(ctx, c.scalingReason); err != nil {
		c.logger.Error("rebalancing failed", "error", err)
		// Return to idle even on error to allow retry
		c.returnToIdleState()

		return
	}

	// After successful rebalance, update lastWorkers to match currentWorkers
	// This prevents immediately re-entering scaling on the next poll
	c.mu.Lock()
	c.lastWorkers = make(map[string]bool)
	for w := range c.currentWorkers {
		c.lastWorkers[w] = true
	}
	c.mu.Unlock()

	// Successfully rebalanced, return to idle
	c.returnToIdleState()
}

// enterEmergencyState transitions the calculator into emergency state for immediate rebalancing.
//
// Emergency rebalancing has no stabilization window and happens immediately when a worker crashes.
//
// Parameters:
//   - ctx: Context for rebalance operation
func (c *Calculator) enterEmergencyState(ctx context.Context) {
	c.mu.Lock()
	c.scalingReason = "emergency"
	c.mu.Unlock()

	c.logger.Warn("entering emergency state - immediate rebalance")

	// Notify Manager of state change
	c.emitStateChange(types.CalcStateEmergency)

	// Perform immediate rebalance
	if err := c.rebalance(ctx, "emergency"); err != nil {
		c.logger.Error("emergency rebalancing failed", "error", err)
		// Return to idle to allow retry
		c.returnToIdleState()

		return
	}

	// After successful rebalance, update lastWorkers to match currentWorkers
	c.mu.Lock()
	c.lastWorkers = make(map[string]bool)
	for w := range c.currentWorkers {
		c.lastWorkers[w] = true
	}
	c.mu.Unlock()

	// Reset emergency detector after successful emergency rebalance
	c.emergencyDetector.Reset()

	// Successfully rebalanced, return to idle
	c.returnToIdleState()
}

// returnToIdleState transitions the calculator back to idle state after rebalancing completes.
func (c *Calculator) returnToIdleState() {
	c.mu.Lock()
	c.scalingReason = ""
	c.mu.Unlock()

	c.logger.Info("returned to idle state")

	// Notify Manager of state change
	c.emitStateChange(types.CalcStateIdle)
}

// selectStabilizationWindow chooses between cold start and planned scale window.

// selectStabilizationWindow chooses between cold start and planned scale window.
func (c *Calculator) selectStabilizationWindow(ctx context.Context) time.Duration {
	workers, _ := c.getActiveWorkers(ctx)
	if len(workers) == 0 {
		return c.coldStartWindow
	}

	// If many workers appear at once, it's likely a cold start
	// Use restart ratio to decide
	partitions, _ := c.source.ListPartitions(ctx)
	expectedWorkers := len(partitions) / 10 // Rough estimate
	if expectedWorkers == 0 {
		expectedWorkers = 1
	}

	ratio := float64(len(workers)) / float64(expectedWorkers)
	if ratio >= c.restartRatio {
		c.logger.Info("detected cold start", "workers", len(workers), "ratio", ratio)
		return c.coldStartWindow
	}

	c.logger.Info("detected planned scale", "workers", len(workers), "ratio", ratio)

	return c.plannedScaleWin
}

// monitorWorkers runs in the background and triggers rebalancing on worker changes.
//
// Current implementation: Polling-only
//   - Polls NATS KV heartbeats every HeartbeatTTL/2 (1.5s typical)
//   - Simple, reliable, and proven in tests
//
// Hybrid monitoring: watcher (primary, <100ms) + polling (fallback, ~1.5s)
func (c *Calculator) monitorWorkers(ctx context.Context) {
	defer close(c.doneCh)

	// Start watcher for fast detection
	if err := c.startWatcher(ctx); err != nil {
		c.logger.Warn("failed to start watcher, falling back to polling only", "error", err)
	}

	// Polling ticker for worker changes (fallback)
	ticker := time.NewTicker(c.hbTTL / 2) // Check twice per TTL
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Check for worker changes via polling (fallback)
			if err := c.pollForChanges(ctx); err != nil {
				c.logger.Error("polling error", "error", err)
			}

		case <-c.stopCh:
			c.stopWatcher()
			return

		case <-ctx.Done():
			c.stopWatcher()
			return
		}
	}
}

// startWatcher starts the NATS KV watcher for fast worker change detection.
func (c *Calculator) startWatcher(ctx context.Context) error {
	c.watcherMu.Lock()
	defer c.watcherMu.Unlock()

	if c.watcher != nil {
		return nil // Already started
	}

	// Watch all heartbeat keys with prefix pattern
	pattern := fmt.Sprintf("%s.*", c.hbPrefix)
	watcher, err := c.heartbeatKV.Watch(ctx, pattern)
	if err != nil {
		return fmt.Errorf("failed to start watcher: %w", err)
	}

	c.watcher = watcher
	c.logger.Info("watcher started for fast worker detection",
		"pattern", pattern,
		"hbPrefix", c.hbPrefix,
	)

	// Start goroutine to process watcher events
	go c.processWatcherEvents(ctx)

	return nil
}

// stopWatcher stops the NATS KV watcher.
func (c *Calculator) stopWatcher() {
	c.watcherMu.Lock()
	defer c.watcherMu.Unlock()

	if c.watcher != nil {
		if err := c.watcher.Stop(); err != nil {
			c.logger.Warn("failed to stop watcher", "error", err)
		}
		c.watcher = nil
		c.logger.Debug("watcher stopped")
	}
}

// processWatcherEvents processes events from the NATS KV watcher.
func (c *Calculator) processWatcherEvents(ctx context.Context) {
	c.logger.Debug("watcher event processor goroutine started")
	defer c.logger.Debug("watcher event processor goroutine stopped")

	c.watcherMu.Lock()
	watcher := c.watcher
	c.watcherMu.Unlock()

	if watcher == nil {
		c.logger.Warn("watcher is nil, cannot process events")
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
		case <-c.stopCh:
			return
		case entry := <-watcher.Updates():
			if entry == nil {
				// Watcher stopped or initial replay done
				c.logger.Debug("watcher: nil entry (replay done or stopped)")
				continue
			}

			// Heartbeat key changed (worker added or updated)
			c.logger.Debug("watcher: received entry", "key", entry.Key(), "operation", entry.Operation())

			// Schedule a debounced check
			if !pendingCheck {
				pendingCheck = true
				debounceTimer.Reset(100 * time.Millisecond)
			}

		case <-debounceTimer.C:
			if pendingCheck {
				pendingCheck = false
				c.logger.Debug("watcher detected change, triggering check")

				// Check for worker changes
				if err := c.pollForChanges(ctx); err != nil {
					c.logger.Error("watcher-triggered check failed", "error", err)
				}
			}
		}
	}
}

func (c *Calculator) pollForChanges(ctx context.Context) error {
	workers, err := c.getActiveWorkers(ctx)
	if err != nil {
		return err
	}

	// Convert workers slice to map for comparison
	currentWorkers := make(map[string]bool)
	for _, w := range workers {
		currentWorkers[w] = true
	}

	c.mu.RLock()
	changed := c.hasWorkersChangedMap(currentWorkers)
	c.mu.RUnlock()

	if !changed {
		return nil
	}

	c.logger.Info("polling detected worker change", "workers", len(workers))

	// Trigger rebalancing
	return c.checkForChanges(ctx, currentWorkers)
}

// Parameters:
//   - ctx: Context for cancellation
//   - currentWorkers: Optional map of current workers (if nil, fetches from KV)
//
// checkForChanges evaluates worker topology changes and triggers rebalancing if needed.
//
// Implements three-tier timing model:
//  1. Detection Speed (Tier 1) - Watcher/polling notices changes quickly
//  2. Rate Limiting (Tier 3) - Check MinRebalanceInterval FIRST to prevent thrashing
//  3. Stabilization (Tier 2) - Apply window AFTER rate limit passes
//
// Tier ordering is critical:
//   - Rate limit (Tier 3) takes precedence over stabilization (Tier 2)
//   - If rate limit is active, defer rebalance regardless of stabilization window
//   - Only after rate limit expires do we enter stabilization windows
//
// Parameters:
//   - ctx: Context for cancellation
//   - currentWorkers: Optional set of active worker IDs (if empty, fetched from KV)
//
// Returns:
//   - error: Processing error, nil on success
func (c *Calculator) checkForChanges(ctx context.Context, currentWorkers ...map[string]bool) error {
	var workers map[string]bool

	// Use provided workers or fetch from KV
	if len(currentWorkers) > 0 && currentWorkers[0] != nil {
		workers = currentWorkers[0]
	} else {
		// Fetch active workers from KV
		workerList, err := c.getActiveWorkers(ctx)
		if err != nil {
			return err
		}

		workers = make(map[string]bool)
		for _, w := range workerList {
			workers[w] = true
		}
	}

	c.mu.RLock()
	changed := c.hasWorkersChangedMap(workers)
	cooldownActive := time.Since(c.lastRebalance) < c.cooldown
	currentState := types.CalculatorState(c.calcState.Load())
	lastWorkerCount := len(c.lastWorkers)
	c.mu.RUnlock()

	c.logger.Debug("checkForChanges",
		"current_workers", len(workers),
		"last_workers", lastWorkerCount,
		"changed", changed,
		"cooldown_active", cooldownActive,
		"state", currentState.String())

	if !changed {
		return nil
	}

	// Don't trigger rebalance if already scaling/rebalancing
	if currentState != types.CalcStateIdle {
		c.logger.Info("worker change detected but calculator not idle",
			"state", currentState.String(),
		)

		return nil
	}

	// TIER 3: Rate limiting - Enforce MinRebalanceInterval FIRST
	// This prevents thrashing during rapid successive changes
	if cooldownActive {
		timeSinceLastRebalance := time.Since(c.lastRebalance)
		remaining := c.cooldown - timeSinceLastRebalance
		c.logger.Info("worker change detected but rate limit active",
			"min_rebalance_interval", c.cooldown,
			"time_since_last", timeSinceLastRebalance,
			"remaining", remaining,
			"next_allowed", c.lastRebalance.Add(c.cooldown),
		)

		return nil // Defer - will be checked again by next poll/watcher event
	}

	c.logger.Info("worker change detected", "workers", len(workers))

	// TIER 2: Determine rebalance type and stabilization window
	reason, window := c.detectRebalanceType(workers)

	// Handle grace period for worker disappearance
	if reason == "" {
		c.logger.Info("worker change in grace period - waiting for confirmation")
		// Don't update lastWorkers - keep tracking the disappeared workers
		return nil
	}

	// Update lastWorkers for next comparison
	c.mu.Lock()
	c.lastWorkers = make(map[string]bool)
	for w := range workers {
		c.lastWorkers[w] = true
	}
	c.mu.Unlock()

	// Trigger appropriate state transition based on reason
	if reason == "emergency" {
		// Emergency: immediate rebalance with no window
		c.enterEmergencyState(ctx)
	} else {
		// Cold start or planned scale: use stabilization window (Tier 2)
		c.enterScalingState(ctx, reason, window)
	}

	return nil
}

// hasWorkersChangedMap checks if the worker set has changed using map comparison.
func (c *Calculator) hasWorkersChangedMap(workers map[string]bool) bool {
	if len(workers) != len(c.lastWorkers) {
		return true
	}

	for w := range workers {
		if !c.lastWorkers[w] {
			return true
		}
	}

	return false
}

// getActiveWorkers retrieves the list of workers with active heartbeats.
func (c *Calculator) getActiveWorkers(ctx context.Context) ([]string, error) {
	// List all keys with heartbeat prefix
	keys, err := c.heartbeatKV.Keys(ctx)
	if err != nil {
		// Handle "no keys found" as empty list
		if err.Error() == "nats: no keys found" {
			c.logger.Debug("no heartbeat keys found")
			return []string{}, nil
		}

		return nil, fmt.Errorf("failed to list heartbeat keys: %w", err)
	}

	c.logger.Debug("scanning heartbeat keys", "total_keys", len(keys), "hb_prefix", c.hbPrefix)

	var workers []string
	for _, key := range keys {
		// Extract worker ID from key (format: "hbPrefix.workerID")
		if len(key) > len(c.hbPrefix)+1 && key[:len(c.hbPrefix)] == c.hbPrefix {
			workerID := key[len(c.hbPrefix)+1:]
			workers = append(workers, workerID)
			c.logger.Debug("found active worker heartbeat", "key", key, "worker_id", workerID)
		} else {
			c.logger.Debug("skipping non-heartbeat key", "key", key, "hb_prefix", c.hbPrefix)
		}
	}

	c.logger.Debug("active workers discovered", "count", len(workers), "workers", workers)

	return workers, nil
}

// rebalance calculates and publishes new assignments.
func (c *Calculator) rebalance(ctx context.Context, lifecycle string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Get current workers and partitions
	workers, err := c.getActiveWorkers(ctx)
	if err != nil {
		return err
	}

	c.logger.Debug("rebalance started", "lifecycle", lifecycle, "worker_count", len(workers), "workers", workers)

	partitions, err := c.source.ListPartitions(ctx)
	if err != nil {
		return err
	}

	c.logger.Debug("partitions retrieved", "partition_count", len(partitions))

	if len(workers) == 0 {
		c.logger.Info("no active workers for assignment")
		return nil
	}

	// Calculate new assignments using strategy
	assignments, err := c.strategy.Assign(workers, partitions)
	if err != nil {
		return fmt.Errorf("assignment calculation failed: %w", err)
	}

	c.logger.Debug("assignments calculated", "worker_count", len(assignments))

	// Increment version
	c.currentVersion++

	c.logger.Debug("publishing assignments to KV", "version", c.currentVersion, "worker_count", len(assignments))

	// Publish assignments to KV
	for workerID, parts := range assignments {
		assignment := types.Assignment{
			Version:    c.currentVersion,
			Lifecycle:  lifecycle,
			Partitions: parts,
		}

		data, err := json.Marshal(assignment)
		if err != nil {
			return fmt.Errorf("failed to marshal assignment: %w", err)
		}

		key := fmt.Sprintf("%s.%s", c.prefix, workerID)
		c.logger.Debug("publishing assignment", "key", key, "worker_id", workerID, "partitions", len(parts), "version", c.currentVersion)
		if _, err := c.assignmentKV.Put(ctx, key, data); err != nil {
			return fmt.Errorf("failed to publish assignment: %w", err)
		}
	}

	c.logger.Debug("all assignments published successfully", "version", c.currentVersion, "workers", len(assignments))

	// Update tracking state
	c.currentWorkers = make(map[string]bool)
	for _, w := range workers {
		c.currentWorkers[w] = true
	}
	c.currentAssignments = assignments
	c.lastRebalance = time.Now()

	// Record metrics
	for _, parts := range assignments {
		c.metrics.RecordAssignmentChange(len(parts), 0, c.currentVersion)
	}

	c.logger.Info("rebalance complete",
		"version", c.currentVersion,
		"workers", len(workers),
		"partitions", len(partitions),
		"lifecycle", lifecycle)

	return nil
}
