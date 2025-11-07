package assignment

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/internal/natsutil"
	"github.com/arloliu/parti/types"
)

// cachedWorkerList bundles worker data with its timestamp for atomic operations.
//
// This ensures that the worker list and its freshness timestamp are always
// consistent when read together, preventing race conditions between updates
// and emergency detection checks.
type cachedWorkerList struct {
	workers   []string
	timestamp time.Time
}

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
	lastWorkers        map[string]bool // Previous worker set for comparison
	disappearedWorkers []string        // Workers that disappeared in emergency (cleared after rebalance)

	// Worker list cache for degraded mode with atomic freshness tracking
	cachedWorkers cachedWorkerList
	cacheMu       sync.RWMutex

	// Manager state provider for degraded mode checks
	stateProvider types.StateProvider

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
		stateProvider:       cfg.StateProvider, // Optional state provider for degraded mode checks
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

// SubscribeToStateChanges returns a channel for state updates and a function to unsubscribe.
func (c *Calculator) SubscribeToStateChanges() (<-chan types.CalculatorState, func()) {
	return c.stateMach.Subscribe()
}

// Start begins monitoring workers and calculating assignments.
//
// This method should only be called on the leader worker. It:
//  1. Discovers highest version from existing assignments
//  2. Starts background monitoring for worker changes
//  3. Performs initial assignment asynchronously (with stabilization window)
//  4. Triggers rebalancing when workers join/leave
//
// The initial assignment runs in a background goroutine, allowing Start() to return
// immediately without blocking on the stabilization window (10-30 seconds). This enables:
//   - Fast manager startup (milliseconds instead of seconds)
//   - Concurrent worker initialization across all instances
//   - Leader calculates assignment with all workers visible from the start
//
// Parameters:
//   - ctx: Context for cancellation and timeout
//
// Returns:
//   - error: Start error (e.g., already started, KV operation failed during setup)
//
// Note: Errors during background initial assignment are logged but not returned.
// Callers should wait for assignment via Manager.waitForAssignment() or similar mechanism.
func (c *Calculator) Start(ctx context.Context) error {
	c.Logger.Info("calculator Start() called")

	// Atomically check and set started flag
	if !c.started.CompareAndSwap(false, true) {
		return types.ErrCalculatorAlreadyStarted
	}

	// Discover highest version from existing assignments to ensure monotonicity across leader changes
	if err := c.discoverHighestVersion(ctx); err != nil {
		c.Logger.Warn("failed to discover existing versions, starting from 0", "error", err)
	}

	// Start worker monitoring component FIRST (before initial assignment)
	// This ensures the monitor is ready to detect worker changes during stabilization window
	if err := c.monitor.Start(ctx); err != nil {
		c.started.Store(false)
		return fmt.Errorf("failed to start worker monitor: %w", err)
	}

	// Perform initial assignment in background goroutine
	// This allows Start() to return immediately while stabilization window runs
	go func() {
		c.Logger.Info("performing initial assignment in background")

		if err := c.initialAssignment(ctx); err != nil {
			c.Logger.Error("initial assignment failed", "error", err)
			// Note: Don't set started=false here as monitor is already running
			// Manager will handle this via timeout in waitForAssignment()
		}
	}()

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
		return types.ErrCalculatorNotStarted
	}

	// 1. Signal stop to all components
	close(c.stopCh)

	// 2. Stop worker monitor (stops watcher and monitoring goroutines)
	if err := c.monitor.Stop(); err != nil {
		c.Logger.Error("failed to stop worker monitor", "error", err)
	}

	// 3. Wait for state machine shutdown
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
		return types.ErrCalculatorNotStarted
	}

	c.Logger.Info("manual rebalance triggered")

	return c.rebalance(ctx, "manual-refresh")
}

// initialAssignment performs the first assignment with two-phase approach:
//  1. Immediate assignment - Assign partitions to currently available workers immediately
//     to ensure zero coverage gap (no 30-second blackout period)
//  2. Stabilization wait - Wait for additional workers to join
//  3. Final assignment - Rebalance with complete worker set for optimal distribution
//
// This ensures partitions are always covered while still allowing for optimal load distribution.
func (c *Calculator) initialAssignment(ctx context.Context) error {
	// PHASE 1: Immediate assignment to whoever is available NOW
	// This ensures zero-downtime - partitions are covered from T=0
	c.Logger.Info("performing immediate initial assignment to current workers")

	if err := c.rebalance(ctx, "cold_start_immediate"); err != nil {
		return fmt.Errorf("immediate initial assignment failed: %w", err)
	}

	// Get worker count after immediate assignment
	c.mu.RLock()
	immediateWorkerCount := len(c.currentWorkers)
	c.mu.RUnlock()

	c.Logger.Info("immediate initial assignment complete - partitions now covered",
		"workers", immediateWorkerCount)

	// PHASE 2: Wait for stabilization window to let more workers join
	window := c.selectStabilizationWindow(ctx)
	c.Logger.Info("waiting for stabilization to detect additional workers", "window", window)

	select {
	case <-time.After(window):
		// Proceed to final assignment
	case <-ctx.Done():
		return ctx.Err()
	}

	// PHASE 3: Final rebalance with complete worker set
	c.Logger.Info("performing final initial assignment with all detected workers")

	if err := c.rebalance(ctx, "cold_start_final"); err != nil {
		return fmt.Errorf("final initial assignment failed: %w", err)
	}

	// Initialize lastWorkers with the workers from final assignment
	// This prevents immediately re-entering scaling when monitorWorkers starts
	c.mu.Lock()
	clear(c.lastWorkers)
	for w := range c.currentWorkers {
		c.lastWorkers[w] = true
	}
	finalWorkerCount := len(c.lastWorkers)
	c.mu.Unlock()

	c.Logger.Info("initial assignment complete",
		"immediate_workers", immediateWorkerCount,
		"final_workers", finalWorkerCount,
		"additional_workers_joined", finalWorkerCount-immediateWorkerCount)

	return nil
}

// discoverHighestVersion scans existing assignments in KV to find the highest version.
// This ensures version monotonicity across leader changes.
func (c *Calculator) discoverHighestVersion(ctx context.Context) error {
	return c.publisher.DiscoverHighestVersion(ctx)
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

			// Record emergency rebalance metric
			c.Metrics.RecordEmergencyRebalance(len(disappearedWorkers))

			// Store disappeared workers for emergency rebalancing
			c.mu.Lock()
			c.disappearedWorkers = disappearedWorkers
			c.mu.Unlock()

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

func (c *Calculator) pollForChanges(ctx context.Context) error {
	workers, err := c.getActiveWorkers(ctx)
	if err != nil {
		return fmt.Errorf("failed to get active workers: %w", err)
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
			return fmt.Errorf("failed to get active workers: %w", err)
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

	// Calculate worker changes for metrics
	added := 0
	removed := 0
	for w := range workers {
		if !lastWorkersCopy[w] {
			added++
		}
	}
	for w := range lastWorkersCopy {
		if !workers[w] {
			removed++
		}
	}

	// Record worker topology change
	c.Metrics.RecordWorkerChange(added, removed)
	c.Metrics.RecordActiveWorkers(len(workers))

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
//
// This method implements cache fallback for degraded mode:
//  1. Try to fetch workers from NATS KV (monitor.GetActiveWorkers)
//  2. On connectivity error, fall back to cached worker list
//  3. Update cache on successful fetches
//  4. Return ErrDegraded if no cache available during connectivity issues
//
// Parameters:
//   - ctx: Context for KV operations
//
// Returns:
//   - []string: List of active worker IDs
//   - error: Error if fetch fails and no cache available
func (c *Calculator) getActiveWorkers(ctx context.Context) ([]string, error) {
	// Try to fetch from NATS KV
	workers, err := c.monitor.GetActiveWorkers(ctx)
	if err != nil {
		// Check if this is a connectivity error
		if natsutil.IsConnectivityError(err) {
			// Try to use cached worker list
			if cached, age, ok := c.getCachedWorkers(); ok {
				c.Logger.Warn("using cached worker list due to connectivity error",
					"workers", len(cached),
					"cache_age", age,
					"error", err)
				// Record cache usage metrics
				c.Metrics.RecordCacheUsage("workers", age.Seconds())
				c.Metrics.IncrementCacheFallback("connectivity_error")

				return cached, nil
			}
			// No cache available - return degraded error
			c.Metrics.IncrementCacheFallback("no_cache")

			return nil, fmt.Errorf("%w: no cached workers available: %w", types.ErrDegraded, err)
		}
		// Non-connectivity error - return as-is
		return nil, err
	}

	// Success - update cache for future use
	c.updateCachedWorkers(workers)

	return workers, nil
}

// getActiveWorkersFiltered retrieves workers, excluding those confirmed disappeared in emergency.
//
// During emergency rebalancing, there's a timing gap where:
//   - EmergencyDetector confirms worker disappeared (grace period expired)
//   - Worker's heartbeat key still exists in KV (TTL not expired yet)
//
// This method acts as a circuit breaker to prevent assigning partitions to confirmed-dead workers.
//
// Parameters:
//   - ctx: Context for KV operations
//   - disappearedWorkers: Workers confirmed disappeared by EmergencyDetector (nil = no filtering)
//
// Returns:
//   - []string: Active workers excluding disappeared ones
//   - error: KV operation error
func (c *Calculator) getActiveWorkersFiltered(ctx context.Context, disappearedWorkers []string) ([]string, error) {
	workers, err := c.getActiveWorkers(ctx)
	if err != nil {
		return nil, err
	}

	// Fast path: no filtering needed
	if len(disappearedWorkers) == 0 {
		return workers, nil
	}

	// Build disappeared set for O(1) lookups
	disappearedSet := make(map[string]bool, len(disappearedWorkers))
	for _, w := range disappearedWorkers {
		disappearedSet[w] = true
	}

	// Filter out disappeared workers
	filtered := make([]string, 0, len(workers))
	for _, w := range workers {
		if !disappearedSet[w] {
			filtered = append(filtered, w)
		}
	}

	if len(filtered) < len(workers) {
		c.Logger.Info("filtered out disappeared workers during emergency",
			"total_workers_from_heartbeat", len(workers),
			"disappeared_workers", disappearedWorkers,
			"active_workers_after_filter", len(filtered),
		)
	}

	return filtered, nil
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
		return fmt.Errorf("rebalance failed for %s: %w", reason, err)
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
	start := time.Now()

	c.mu.Lock()
	disappearedWorkers := c.disappearedWorkers
	c.disappearedWorkers = nil // Clear after reading
	c.mu.Unlock()

	// Get active workers, filtering out disappeared ones during emergency
	workers, err := c.getActiveWorkersFiltered(ctx, disappearedWorkers)
	if err != nil {
		c.Metrics.RecordRebalanceAttempt(lifecycle, false)
		return fmt.Errorf("failed to get active workers: %w", err)
	}

	c.Logger.Debug("rebalance started", "lifecycle", lifecycle, "worker_count", len(workers), "workers", workers)

	partitions, err := c.Source.ListPartitions(ctx)
	if err != nil {
		c.Metrics.RecordRebalanceAttempt(lifecycle, false)
		return fmt.Errorf("failed to list partitions: %w", err)
	}

	c.Logger.Debug("partitions retrieved", "partition_count", len(partitions))

	// Record partition count
	c.Metrics.RecordPartitionCount(len(partitions))

	if len(workers) == 0 {
		c.Logger.Info("no active workers for assignment")
		c.Metrics.RecordRebalanceDuration(time.Since(start).Seconds(), lifecycle)
		c.Metrics.RecordRebalanceAttempt(lifecycle, true)

		return nil
	}

	// Calculate new assignments using strategy
	assignments, err := c.Strategy.Assign(workers, partitions)
	if err != nil {
		c.Metrics.RecordRebalanceAttempt(lifecycle, false)
		return fmt.Errorf("assignment calculation failed: %w", err)
	}

	c.Logger.Debug("assignments calculated", "worker_count", len(assignments))

	// Publish assignments via publisher component
	if err := c.publisher.Publish(ctx, workers, assignments, lifecycle); err != nil {
		c.Metrics.RecordRebalanceAttempt(lifecycle, false)
		return fmt.Errorf("failed to publish assignments: %w", err)
	}

	// Record successful rebalance
	c.Metrics.RecordRebalanceDuration(time.Since(start).Seconds(), lifecycle)
	c.Metrics.RecordRebalanceAttempt(lifecycle, true)

	// Update tracking state
	c.mu.Lock()
	clear(c.currentWorkers)
	for _, w := range workers {
		c.currentWorkers[w] = true
	}
	c.currentAssignments = assignments
	c.mu.Unlock()

	c.Logger.Info("rebalance complete",
		"version", c.publisher.CurrentVersion(),
		"workers", len(workers),
		"partitions", len(partitions),
		"lifecycle", lifecycle)

	return nil
}

// ============================================================================
// Degraded Mode - Worker Cache Management
// ============================================================================

// getCachedWorkers returns cached worker list with freshness timestamp.
//
// Returns defensive copy to prevent external mutations.
// Returns the timestamp atomically with the data to ensure consistency.
//
// Returns:
//   - []string: Copy of cached worker list
//   - time.Duration: Age of the cached data (time since last fresh read)
//   - bool: true if cache is available, false otherwise
func (c *Calculator) getCachedWorkers() ([]string, time.Duration, bool) {
	c.cacheMu.RLock()
	defer c.cacheMu.RUnlock()

	if c.cachedWorkers.workers == nil {
		return nil, 0, false
	}

	// Return a copy to prevent external modification
	cached := make([]string, len(c.cachedWorkers.workers))
	copy(cached, c.cachedWorkers.workers)

	// Calculate age based on timestamp
	age := time.Since(c.cachedWorkers.timestamp)

	return cached, age, true
}

// updateCachedWorkers updates the cached worker list atomically with timestamp.
//
// Creates defensive copy to prevent external mutations.
// Bundles worker list and timestamp together for atomic freshness tracking.
//
// Parameters:
//   - workers: Fresh worker list from KV
func (c *Calculator) updateCachedWorkers(workers []string) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	// Create defensive copy
	cached := make([]string, len(workers))
	copy(cached, workers)

	// Update atomically
	c.cachedWorkers = cachedWorkerList{
		workers:   cached,
		timestamp: time.Now(),
	}

	c.Logger.Debug("updated worker cache",
		"workers", len(workers),
	)
}
