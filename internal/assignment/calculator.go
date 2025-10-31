package assignment

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/internal/metrics"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
)

// Calculator manages partition assignment calculation and distribution.
//
// The calculator runs on the leader worker and orchestrates three focused components:
//   - WorkerMonitor: Detects worker health changes via NATS KV heartbeats
//   - StateMachine: Manages state transitions (Idle, Scaling, Rebalancing, Emergency)
//   - AssignmentPublisher: Publishes partition assignments to NATS KV
//
// The calculator handles rebalancing logic and coordinates these components.
// It does NOT run on follower workers.
type Calculator struct {
	Config

	// Core components
	monitor           *WorkerMonitor
	stateMach         *StateMachine
	publisher         *AssignmentPublisher
	emergencyDetector *EmergencyDetector // Hysteresis-based emergency detection

	// Cached string patterns (for performance)
	hbWatchPattern      string // "HeartbeatPrefix.*" - cached for Watch() calls
	assignmentKeyPrefix string // "AssignmentPrefix." - cached for key construction

	// State management
	started            atomic.Bool
	mu                 sync.RWMutex
	currentWorkers     map[string]bool
	currentAssignments map[string][]types.Partition

	// Worker tracking for change detection
	lastWorkers map[string]bool // Previous worker set for comparison

	// Hybrid monitoring: watcher (primary) + polling (fallback)
	watcher   jetstream.KeyWatcher
	watcherMu sync.Mutex // Protects watcher lifecycle

	// Lifecycle
	stopCh chan struct{}
	doneCh chan struct{}
}

// NewCalculator creates a calculator with validated configuration.
//
// This constructor provides clear, self-documenting configuration and
// validation of required fields.
//
// Parameters:
//   - cfg: Calculator configuration (required fields must be set)
//
// Returns:
//   - *Calculator: New calculator instance ready to start
//   - error: Validation error if required fields are missing
//
// Example:
//
//	calc, err := assignment.NewCalculator(&assignment.Config{
//	    AssignmentKV:     assignKV,
//	    HeartbeatKV:      heartbeatKV,
//	    Source:           source,
//	    Strategy:         strategy,
//	    AssignmentPrefix: "assignment",
//	    HeartbeatPrefix:  "heartbeat",
//	    HeartbeatTTL:     3 * time.Second,
//	    // Optional fields use sensible defaults
//	    Logger:           logger,
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
func NewCalculator(cfg *Config) (*Calculator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	cfg.SetDefaults()

	stopCh := make(chan struct{})

	c := &Calculator{
		Config:              *cfg, // Anonymous embedding - copy config
		hbWatchPattern:      fmt.Sprintf("%s.*", cfg.HeartbeatPrefix),
		assignmentKeyPrefix: fmt.Sprintf("%s.", cfg.AssignmentPrefix),
		currentWorkers:      make(map[string]bool),
		currentAssignments:  make(map[string][]types.Partition),
		lastWorkers:         make(map[string]bool),
		stopCh:              stopCh,
		doneCh:              make(chan struct{}),
	}

	// Initialize emergency detector with configured grace period
	c.emergencyDetector = NewEmergencyDetector(cfg.EmergencyGracePeriod)

	// Initialize components (Phase 3.2)
	c.publisher = NewAssignmentPublisher(
		cfg.AssignmentKV,
		cfg.AssignmentPrefix,
		cfg.Logger,
		cfg.Metrics,
	)

	c.stateMach = NewStateMachine(
		cfg.Logger,
		cfg.Metrics,
		c.handleRebalance,
		stopCh,
	)

	c.monitor = NewWorkerMonitor(
		cfg.HeartbeatKV,
		cfg.HeartbeatPrefix,
		cfg.HeartbeatTTL,
		c.pollForChanges,
		cfg.Logger,
	)

	return c, nil
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
	c.Cooldown = cooldown
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
	c.MinThreshold = threshold
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
	c.RestartRatio = ratio
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
	c.ColdStartWindow = coldStart
	c.PlannedScaleWindow = plannedScale
}

// SetMetrics sets the metrics collector.
func (c *Calculator) SetMetrics(m types.MetricsCollector) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m == nil {
		c.Metrics = metrics.NewNop()
	} else {
		c.Metrics = m
	}
}

// SetLogger sets the logger.
func (c *Calculator) SetLogger(l types.Logger) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l == nil {
		c.Logger = logging.NewNop()
	} else {
		c.Logger = l
	}
}

// SubscribeToStateChanges returns a channel for state updates and a function to unsubscribe.
func (c *Calculator) SubscribeToStateChanges() (<-chan types.CalculatorState, func()) {
	return c.stateMach.Subscribe()
}

// discoverHighestVersion scans existing assignments in KV to find the highest version.
// This ensures version monotonicity across leader changes.
func (c *Calculator) discoverHighestVersion(ctx context.Context) error {
	return c.publisher.DiscoverHighestVersion(ctx)
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
	c.Logger.Info("calculator Start() called")

	// Atomically check and set started flag
	if !c.started.CompareAndSwap(false, true) {
		return errors.New("calculator already started")
	}

	// Discover highest version from existing assignments to ensure monotonicity across leader changes
	if err := c.discoverHighestVersion(ctx); err != nil {
		c.Logger.Warn("failed to discover existing versions, starting from 0", "error", err)
	}

	c.Logger.Info("performing initial assignment")

	// Perform initial assignment with stabilization window
	if err := c.initialAssignment(ctx); err != nil {
		c.started.Store(false)
		return fmt.Errorf("initial assignment failed: %w", err)
	}

	// Start worker monitoring component
	if err := c.monitor.Start(ctx); err != nil {
		c.started.Store(false)
		return fmt.Errorf("failed to start worker monitor: %w", err)
	}

	return nil
}

// Stop stops the calculator and waits for background goroutines to finish.
//
// This method performs a clean shutdown sequence:
//  1. Signals stop to all components
//  2. Cleans up assignments from KV (provides clean slate for new leader)
//  3. Stops worker monitor
//  4. Waits for state machine shutdown
//
// The assignment cleanup is best-effort and won't fail the Stop() operation.
// If cleanup fails, the new leader will discover existing versions and maintain
// version monotonicity via DiscoverHighestVersion().
//
// Parameters:
//   - ctx: Context for cleanup timeout control (typically 5s)
//
// Returns:
//   - error: Stop error (e.g., not started)
func (c *Calculator) Stop(ctx context.Context) error {
	// Atomically check and clear started flag
	if !c.started.CompareAndSwap(true, false) {
		return errors.New("calculator not started")
	}

	// 1. Signal stop to all components
	close(c.stopCh)

	// 2. Clean up assignments from KV (best-effort)
	// This provides a clean slate for the new leader and prevents confusion
	if err := c.publisher.CleanupAllAssignments(ctx); err != nil {
		c.Logger.Warn("assignment cleanup failed during stop, new leader will inherit state", "error", err)
		// Don't fail Stop() if cleanup fails - version monotonicity prevents issues
	}

	// 3. Stop worker monitor (stops watcher and monitoring goroutines)
	if err := c.monitor.Stop(); err != nil {
		c.Logger.Error("failed to stop worker monitor", "error", err)
	}

	// 4. Wait for state machine shutdown
	c.stateMach.WaitForShutdown()

	return nil
}

// IsStarted returns true if the calculator is currently running.
func (c *Calculator) IsStarted() bool {
	return c.started.Load()
}

// GetState returns the current calculator state.
//
// Returns:
//   - types.CalculatorState: Current calculator state (type-safe enum)
func (c *Calculator) GetState() types.CalculatorState {
	return c.stateMach.GetState()
}

// GetScalingReason returns the reason for the current or last scaling operation.
//
// Returns:
//   - string: Scaling reason ("cold_start", "planned_scale", "emergency", "restart") or empty string if idle
func (c *Calculator) GetScalingReason() string {
	return c.stateMach.GetScalingReason()
}

// CurrentVersion returns the current assignment version.
func (c *Calculator) CurrentVersion() int64 {
	return c.publisher.CurrentVersion()
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

	c.Logger.Info("manual rebalance triggered")

	return c.rebalance(ctx, "manual-refresh")
}

// initialAssignment performs the first assignment with stabilization window.
func (c *Calculator) initialAssignment(ctx context.Context) error {
	// Wait for stabilization window
	window := c.selectStabilizationWindow(ctx)
	c.Logger.Info("waiting for stabilization", "window", window)

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
	clear(c.lastWorkers)
	for w := range c.currentWorkers {
		c.lastWorkers[w] = true
	}
	workerCount := len(c.lastWorkers)
	c.mu.Unlock()

	c.Logger.Info("initial assignment complete", "workers", workerCount)

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
//   - lastWorkers: Previous set of active workers
//   - currentWorkers: Current set of active workers
//
// Returns:
//   - reason: Rebalance type ("emergency", "cold_start", "planned_scale", or "" if in grace period)
//   - window: Stabilization window duration (0 for emergency or during grace period)
func (c *Calculator) detectRebalanceType(lastWorkers, currentWorkers map[string]bool) (reason string, window time.Duration) {
	prevCount := len(lastWorkers)
	currCount := len(currentWorkers)

	// Case 1: Worker(s) disappeared - Check for emergency with hysteresis
	if currCount < prevCount {
		emergency, disappearedWorkers := c.emergencyDetector.CheckEmergency(lastWorkers, currentWorkers)

		if emergency {
			c.Logger.Warn("emergency: workers disappeared beyond grace period",
				"disappeared", disappearedWorkers,
				"prev_count", prevCount,
				"curr_count", currCount,
			)

			return "emergency", 0 // No stabilization - immediate action
		}

		// Still in grace period - no action yet
		c.Logger.Info("workers disappeared but within grace period",
			"prev_count", prevCount,
			"curr_count", currCount,
		)

		return "", 0 // Wait for grace period to expire
	}

	// Case 2: Cold start - First workers joining
	if prevCount == 0 {
		c.Logger.Info("cold start detected",
			"worker_count", currCount,
			"window", c.ColdStartWindow,
		)

		return "cold_start", c.ColdStartWindow
	}

	// Case 3: Planned scale - Worker(s) added
	c.Logger.Info("planned scale detected",
		"prev_count", prevCount,
		"curr_count", currCount,
		"window", c.PlannedScaleWindow,
	)

	return "planned_scale", c.PlannedScaleWindow
}

// enterScalingState transitions the calculator into scaling state with stabilization window.
//
// Parameters:
//   - ctx: Context for cancellation
//   - reason: Reason for scaling ("cold_start", "planned_scale", "restart")
//   - window: Stabilization window duration to wait before rebalancing
func (c *Calculator) enterScalingState(ctx context.Context, reason string, window time.Duration) {
	c.stateMach.EnterScaling(ctx, reason, window)
}

// enterEmergencyState transitions the calculator into emergency state for immediate rebalancing.
//
// Emergency rebalancing has no stabilization window and happens immediately when a worker crashes.
//
// Parameters:
//   - ctx: Context for rebalance operation
func (c *Calculator) enterEmergencyState(ctx context.Context) {
	c.stateMach.EnterEmergency(ctx)
}

// selectStabilizationWindow chooses between cold start and planned scale window.
func (c *Calculator) selectStabilizationWindow(ctx context.Context) time.Duration {
	workers, _ := c.getActiveWorkers(ctx)
	if len(workers) == 0 {
		return c.ColdStartWindow
	}

	// If many workers appear at once, it's likely a cold start
	// Use restart ratio to decide
	partitions, _ := c.Source.ListPartitions(ctx)
	expectedWorkers := len(partitions) / 10 // Rough estimate
	if expectedWorkers == 0 {
		expectedWorkers = 1
	}

	ratio := float64(len(workers)) / float64(expectedWorkers)
	if ratio >= c.RestartRatio {
		c.Logger.Info("detected cold start", "workers", len(workers), "ratio", ratio)
		return c.ColdStartWindow
	}

	c.Logger.Info("detected planned scale", "workers", len(workers), "ratio", ratio)

	return c.PlannedScaleWindow
}

// startWatcher starts the NATS KV watcher for fast worker change detection.
func (c *Calculator) startWatcher(ctx context.Context) error {
	c.watcherMu.Lock()
	defer c.watcherMu.Unlock()

	if c.watcher != nil {
		return nil // Already started
	}

	// Watch all heartbeat keys with prefix pattern
	watcher, err := c.HeartbeatKV.Watch(ctx, c.hbWatchPattern)
	if err != nil {
		return fmt.Errorf("failed to start watcher: %w", err)
	}

	c.watcher = watcher
	c.Logger.Info("watcher started for fast worker detection",
		"pattern", c.hbWatchPattern,
		"hbPrefix", c.HeartbeatPrefix,
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
			c.Logger.Warn("failed to stop watcher", "error", err)
		}
		c.watcher = nil
		c.Logger.Debug("watcher stopped")
	}
}

// processWatcherEvents processes events from the NATS KV watcher.
func (c *Calculator) processWatcherEvents(ctx context.Context) {
	c.Logger.Debug("watcher event processor goroutine started")
	defer c.Logger.Debug("watcher event processor goroutine stopped")

	c.watcherMu.Lock()
	watcher := c.watcher
	c.watcherMu.Unlock()

	if watcher == nil {
		c.Logger.Warn("watcher is nil, cannot process events")
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
				c.Logger.Debug("watcher: nil entry (replay done or stopped)")
				continue
			}

			// Heartbeat key changed (worker added or updated)
			c.Logger.Debug("watcher: received entry", "key", entry.Key(), "operation", entry.Operation())

			// Schedule a debounced check
			if !pendingCheck {
				pendingCheck = true
				debounceTimer.Reset(100 * time.Millisecond)
			}

		case <-debounceTimer.C:
			if pendingCheck {
				pendingCheck = false
				c.Logger.Debug("watcher detected change, triggering check")

				// Check for worker changes
				if err := c.pollForChanges(ctx); err != nil {
					c.Logger.Error("watcher-triggered check failed", "error", err)
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

	c.Logger.Info("polling detected worker change", "workers", len(workers))

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
	cooldownActive := time.Since(c.publisher.LastRebalanceTime()) < c.Cooldown
	currentState := c.GetState()
	lastWorkerCount := len(c.lastWorkers)
	// Make a copy of lastWorkers to avoid race in detectRebalanceType
	lastWorkersCopy := make(map[string]bool, len(c.lastWorkers))
	for w := range c.lastWorkers {
		lastWorkersCopy[w] = true
	}
	c.mu.RUnlock()

	c.Logger.Debug("checkForChanges",
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
		c.Logger.Info("worker change detected but calculator not idle",
			"state", currentState.String(),
		)

		return nil
	}

	// TIER 3: Rate limiting - Enforce MinRebalanceInterval FIRST
	// This prevents thrashing during rapid successive changes
	if cooldownActive {
		lastRebalanceTime := c.publisher.LastRebalanceTime()
		timeSinceLastRebalance := time.Since(lastRebalanceTime)
		remaining := c.Cooldown - timeSinceLastRebalance
		c.Logger.Info("worker change detected but rate limit active",
			"min_rebalance_interval", c.Cooldown,
			"time_since_last", timeSinceLastRebalance,
			"remaining", remaining,
			"next_allowed", lastRebalanceTime.Add(c.Cooldown),
		)

		return nil // Defer - will be checked again by next poll/watcher event
	}

	c.Logger.Info("worker change detected", "workers", len(workers))

	// TIER 2: Determine rebalance type and stabilization window
	reason, window := c.detectRebalanceType(lastWorkersCopy, workers)

	// Handle grace period for worker disappearance
	if reason == "" {
		c.Logger.Info("worker change in grace period - waiting for confirmation")
		// Don't update lastWorkers - keep tracking the disappeared workers
		return nil
	}

	// Update lastWorkers for next comparison
	c.mu.Lock()
	clear(c.lastWorkers)
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
	return c.monitor.GetActiveWorkers(ctx)
}

// handleRebalance is the callback invoked by StateMachine when rebalancing should occur.
//
// This method bridges the StateMachine component to the Calculator's rebalancing logic.
// It also handles post-rebalance state updates (updating lastWorkers, resetting emergency detector).
//
// Parameters:
//   - ctx: Context for the rebalance operation
//   - reason: Rebalance reason ("cold_start", "planned_scale", "emergency", "restart")
//
// Returns:
//   - error: Nil on success, error on rebalance failure
func (c *Calculator) handleRebalance(ctx context.Context, reason string) error {
	// Perform the rebalance
	if err := c.rebalance(ctx, reason); err != nil {
		return err
	}

	// After successful rebalance, update lastWorkers to match currentWorkers
	// This prevents immediately re-entering scaling on the next poll
	c.mu.Lock()
	clear(c.lastWorkers)
	for w := range c.currentWorkers {
		c.lastWorkers[w] = true
	}
	c.mu.Unlock()

	// Reset emergency detector after successful emergency rebalance
	if reason == "emergency" {
		c.emergencyDetector.Reset()
	}

	return nil
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

	c.Logger.Debug("rebalance started", "lifecycle", lifecycle, "worker_count", len(workers), "workers", workers)

	partitions, err := c.Source.ListPartitions(ctx)
	if err != nil {
		return err
	}

	c.Logger.Debug("partitions retrieved", "partition_count", len(partitions))

	if len(workers) == 0 {
		c.Logger.Info("no active workers for assignment")
		return nil
	}

	// Calculate new assignments using strategy
	assignments, err := c.Strategy.Assign(workers, partitions)
	if err != nil {
		return fmt.Errorf("assignment calculation failed: %w", err)
	}

	c.Logger.Debug("assignments calculated", "worker_count", len(assignments))

	// Publish assignments via publisher component
	if err := c.publisher.Publish(ctx, workers, assignments, lifecycle); err != nil {
		return err
	}

	// Update tracking state
	clear(c.currentWorkers)
	for _, w := range workers {
		c.currentWorkers[w] = true
	}
	c.currentAssignments = assignments

	c.Logger.Info("rebalance complete",
		"version", c.publisher.CurrentVersion(),
		"workers", len(workers),
		"partitions", len(partitions),
		"lifecycle", lifecycle)

	return nil
}
