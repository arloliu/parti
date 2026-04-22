package assignment

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/types"
)

// errShuttingDown is returned by rebalance() when the manager is shutting down.
// handleRebalance catches this sentinel and logs at Info rather than Error so
// that normal manager shutdown does not produce spurious error-level log entries.
var errShuttingDown = errors.New("rebalance aborted: leader is shutting down")

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
	assignmentKeyPrefix string // "AssignmentPrefix." - cached for key construction

	// State management
	started            atomic.Bool
	mu                 sync.RWMutex
	rebalanceMu        sync.Mutex // Serializes rebalance operations
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
	wg     sync.WaitGroup // Tracks background goroutines (e.g., monitorPartitions)
}

// cachedWorkerList bundles worker data with its timestamp for atomic operations.
//
// This ensures that the worker list and its freshness timestamp are always
// consistent when read together, preventing race conditions between updates
// and emergency detection checks.
type cachedWorkerList struct {
	workers   []string
	timestamp time.Time
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

	// Initialize components
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
	existingWorkers, err := c.discoverHighestVersion(ctx)
	if err != nil {
		c.Logger.Warn("failed to discover existing versions, starting from 0", "error", err)
	}

	// Seed currentWorkers with IDs that currently have assignment keys in KV.
	// Without this seeding, a new leader's first rebalance has an empty
	// currentWorkers map, so its workersToRemove computation is empty and
	// stale assignment.<id> keys for already-dead workers are never swept.
	// A pod later reclaiming such an ID would then read stale partition data
	// from its own waitForAssignment poll.
	if len(existingWorkers) > 0 {
		c.mu.Lock()
		for _, wid := range existingWorkers {
			c.currentWorkers[wid] = true
		}
		c.mu.Unlock()
	}

	// A non-zero discovered version means prior leaders have already published
	// assignments in this cluster, so this Start() is a leadership takeover of
	// an already-warm cluster — not a true cold start from empty. On takeover
	// the full ColdStartWindow wait for "fleet to come online" is wasted: the
	// fleet is already online, and any worker joining during that wait gets
	// its assignment delayed by up to ColdStartWindow, which can exceed caller
	// startup deadlines (the user-reported restart hang).
	isTakeover := c.publisher.CurrentVersion() > 0

	// Step 1: Immediate assignment to whoever is available NOW
	// This ensures zero-downtime - partitions are covered from T=0
	// We do this synchronously to ensure Start() returns error if initial assignment fails.
	c.Logger.Info("performing immediate initial assignment to current workers")

	initialReason := "cold_start_immediate"
	if isTakeover {
		initialReason = "takeover_immediate"
	}
	if err := c.rebalance(ctx, initialReason); err != nil {
		c.started.Store(false)
		return fmt.Errorf("immediate initial assignment failed: %w", err)
	}

	// Get worker count after immediate assignment and update lastWorkers
	// to prevent monitor from triggering a duplicate "cold_start" rebalance.
	c.mu.Lock()
	immediateWorkerCount := c.setLastWorkersLocked(c.currentWorkers)
	c.mu.Unlock()

	c.Logger.Info("immediate initial assignment complete - partitions now covered",
		"workers", immediateWorkerCount, "takeover", isTakeover)

	// Start worker monitoring component AFTER establishing baseline
	// This ensures the monitor doesn't see the initial workers as "new" additions
	if err := c.monitor.Start(ctx); err != nil {
		c.started.Store(false)
		return fmt.Errorf("failed to start worker monitor: %w", err)
	}

	// Start partition monitoring if supported
	if watchable, ok := c.Source.(types.WatchablePartitionSource); ok {
		c.Logger.Info("starting partition monitor")
		c.wg.Go(func() {
			c.monitorPartitions(ctx, watchable)
		})
	}

	// Step 2: Enter stabilization window.
	//   - Cold start: use the long ColdStartWindow so the initial fleet has
	//     time to fully come online before the final rebalance fires.
	//   - Takeover: use the much shorter PlannedScaleWindow. The fleet is
	//     already warm; this window only needs to absorb any worker that was
	//     briefly invisible when the new leader ran its immediate rebalance
	//     (e.g. a pod restarting concurrently with leader takeover).
	window := c.ColdStartWindow
	reason := "cold_start"
	if isTakeover {
		window = c.PlannedScaleWindow
		reason = "takeover"
	}
	c.Logger.Info("entering stabilization phase", "window", window, "reason", reason)
	c.enterScalingState(ctx, reason, window)

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
	// Mark state machine as stopping before closing channels to avoid scheduling new timers
	c.stateMach.stopping.Store(true)
	close(c.stopCh)

	// 2. Stop worker monitor (stops watcher and monitoring goroutines)
	if err := c.monitor.Stop(); err != nil {
		c.Logger.Error("failed to stop worker monitor", "error", err)
	}

	// 3. Wait for state machine shutdown
	c.stateMach.WaitForShutdown()

	// 4. Wait for background goroutines (e.g., monitorPartitions)
	c.wg.Wait()

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

// discoverHighestVersion scans existing assignments in KV to find the highest
// version and the set of worker IDs that currently have assignment keys.
// This ensures version monotonicity across leader changes and lets the
// calculator seed its currentWorkers map so the first rebalance can sweep
// assignment keys for workers that no longer have active heartbeats.
func (c *Calculator) discoverHighestVersion(ctx context.Context) ([]string, error) {
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
	// Avoid scheduling scaling when stopping
	select {
	case <-c.stopCh:
		c.Logger.Info("skip EnterScaling: calculator stopping", "reason", reason)
		return
	default:
	}
	if c.stateMach.stopping.Load() {
		c.Logger.Info("skip EnterScaling: state machine stopping", "reason", reason)
		return
	}

	c.stateMach.EnterScaling(ctx, reason, window)
}

// enterEmergencyState transitions the calculator into emergency state for immediate rebalancing.
//
// Emergency rebalancing has no stabilization window and happens immediately when a worker crashes.
//
// Parameters:
//   - ctx: Context for rebalance operation
func (c *Calculator) enterEmergencyState(ctx context.Context) {
	// Avoid scheduling emergency when stopping
	select {
	case <-c.stopCh:
		c.Logger.Info("skip EnterEmergency: calculator stopping")
		return
	default:
	}
	if c.stateMach.stopping.Load() {
		c.Logger.Info("skip EnterEmergency: state machine stopping")
		return
	}

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
	partitions, _ := c.Source.List(ctx)
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
	// Optimization: Check for changes before proceeding to avoid log noise
	// and unnecessary processing.
	changed := c.hasWorkersChangedLocked(currentWorkers)
	c.mu.RUnlock()

	if !changed {
		return nil
	}

	c.Logger.Info("polling detected worker change", "workers", len(workers))

	// Trigger rebalancing
	return c.checkForChanges(ctx, currentWorkers)
}

func (c *Calculator) monitorPartitions(ctx context.Context, source types.WatchablePartitionSource) {
	ch := source.Watch(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case _, ok := <-ch:
			if !ok {
				return // Channel closed
			}
			c.Logger.Info("partition change detected")

			// Check if shutdown is in progress before triggering rebalance
			select {
			case <-c.stopCh:
				c.Logger.Info("skipping partition rebalance: shutdown in progress")
				return
			default:
			}

			// Trigger rebalance with timeout
			// We use a detached context with timeout because the rebalance
			// should complete even if the watcher loop is busy, but shouldn't hang forever.
			reqCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := c.rebalance(reqCtx, "partition_update"); err != nil {
				c.Logger.Error("failed to rebalance after partition update", "error", err)
			}
			cancel()
		}
	}
}

// checkForChanges evaluates worker topology changes and triggers rebalancing if needed.
//
// Implements "Emergency-First" priority model:
//  1. Detection (Tier 1) - Identify change type immediately
//  2. Emergency (Tier 0) - If emergency, BYPASS cooldown and stabilization
//  3. Rate Limiting (Tier 3) - If normal change, enforce RebalanceCooldown
//  4. Stabilization (Tier 2) - If passed cooldown, apply stabilization window
//
// This ensures worker crashes are handled immediately while normal scaling
// is smoothed out to prevent thrashing.
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
	// We must re-check for changes here because lastWorkers might have been updated
	// by a concurrent rebalance (e.g., from the scaling timer) since the caller's check.
	changed := c.hasWorkersChangedLocked(workers)
	lastWorkersCopy := c.cloneLastWorkersLocked()
	c.mu.RUnlock()

	if !changed {
		return nil
	}

	// TIER 1: Detect Rebalance Type immediately
	// We do this BEFORE rate limiting to identify emergencies that must bypass checks
	reason, window := c.detectRebalanceType(lastWorkersCopy, workers)

	// Handle Grace Period (wait for confirmation)
	if reason == "" {
		c.Logger.Debug("worker change in grace period - waiting for confirmation")
		return nil
	}

	// TIER 0: Emergency Handling - Bypass Cooldown & State Checks
	if reason == "emergency" {
		c.Logger.Warn("emergency detected - bypassing cooldown and stabilization",
			"reason", reason,
			"workers", len(workers))

		// Trigger immediate emergency rebalance
		// This will force-transition the state machine even if Scaling/Rebalancing
		c.enterEmergencyState(ctx)

		return nil
	}

	// Recovery Grace: defer non-emergency rebalancing while the leader is stabilizing
	// after exiting degraded mode. Emergencies (Tier 0) still proceed immediately.
	if c.stateProvider != nil && c.stateProvider.IsInRecoveryGrace() {
		c.Logger.Info("skipping rebalance: leader is in recovery grace period after degraded mode")
		return nil
	}

	// TIER 3: Rate limiting - Enforce RebalanceCooldown for NON-EMERGENCY changes
	// This prevents thrashing during rapid successive changes (flapping)
	if time.Since(c.publisher.LastRebalanceTime()) < c.Cooldown {
		lastRebalanceTime := c.publisher.LastRebalanceTime()
		timeSinceLastRebalance := time.Since(lastRebalanceTime)
		remaining := c.Cooldown - timeSinceLastRebalance

		c.Logger.Debug("worker change detected but rate limit active",
			"reason", reason,
			"min_rebalance_interval", c.Cooldown,
			"time_since_last", timeSinceLastRebalance,
			"remaining", remaining,
			"next_allowed", lastRebalanceTime.Add(c.Cooldown),
		)

		return nil // Defer - will be checked again by next poll/watcher event
	}

	// Check if we can enter scaling state (must be Idle)
	currentState := c.GetState()
	if currentState != types.CalcStateIdle {
		c.Logger.Debug("worker change detected but calculator not idle",
			"state", currentState.String(),
			"reason", reason,
		)

		return nil // Defer
	}

	c.Logger.Debug("worker change detected", "workers", len(workers), "reason", reason)

	// TIER 2: Stabilization - Enter Scaling State
	// Cold start or planned scale: use stabilization window
	c.enterScalingState(ctx, reason, window)

	return nil
}

// setLastWorkersLocked replaces lastWorkers with the provided worker set.
// Caller must hold c.mu.Lock(). Returns the total worker count after the update.
func (c *Calculator) setLastWorkersLocked(workers map[string]bool) int {
	clear(c.lastWorkers)
	for w := range workers {
		c.lastWorkers[w] = true
	}

	return len(c.lastWorkers)
}

// cloneLastWorkersLocked returns a copy of lastWorkers. Caller must hold c.mu.RLock().
func (c *Calculator) cloneLastWorkersLocked() map[string]bool {
	cloned := make(map[string]bool, len(c.lastWorkers))
	for w := range c.lastWorkers {
		cloned[w] = true
	}

	return cloned
}

// hasWorkersChangedLocked checks if the worker set has changed. Caller must hold c.mu.RLock().
func (c *Calculator) hasWorkersChangedLocked(workers map[string]bool) bool {
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
		// Shutdown aborts are expected during clean manager stop; log at Info so
		// the state machine does not escalate them to Error-level log entries.
		if errors.Is(err, errShuttingDown) {
			c.Logger.Info("rebalance skipped during shutdown", "reason", reason)
			return nil
		}
		return fmt.Errorf("rebalance failed for %s: %w", reason, err)
	}

	// After successful rebalance, update lastWorkers to match currentWorkers
	// This prevents immediately re-entering scaling on the next poll
	c.mu.Lock()
	c.setLastWorkersLocked(c.currentWorkers)
	c.mu.Unlock()

	// Reset emergency detector after successful emergency rebalance
	if reason == "emergency" {
		c.emergencyDetector.Reset()
	}

	return nil
}

// rebalance calculates and publishes new assignments.
func (c *Calculator) rebalance(ctx context.Context, lifecycle string) error {
	// Serialize rebalance operations to prevent race conditions
	c.rebalanceMu.Lock()
	defer c.rebalanceMu.Unlock()

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

	// Diagnostic: surface heartbeat keys when only a single worker is detected.
	// This helps investigate scenarios where multiple workers should be present
	// but only one heartbeat is visible to the leader, leading to skewed assignments.
	if len(workers) == 1 {
		if rawKeys, kerr := c.HeartbeatKV.Keys(ctx); kerr == nil {
			c.Logger.Info("diagnostic: single-worker rebalance", "lifecycle", lifecycle, "workers", workers, "heartbeat_keys", rawKeys)
		} else {
			c.Logger.Info("diagnostic: single-worker rebalance (heartbeat key list error)", "lifecycle", lifecycle, "workers", workers, "error", kerr)
		}
	}

	partitions, err := c.Source.List(ctx)
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

	// Check for orphaned partitions
	assignedCount := 0
	for _, parts := range assignments {
		assignedCount += len(parts)
	}

	if assignedCount != len(partitions) {
		orphaned := len(partitions) - assignedCount
		c.Logger.Error("orphaned partitions detected",
			"total", len(partitions),
			"assigned", assignedCount,
			"orphaned", orphaned)
		c.Metrics.RecordOrphanedPartitions(orphaned)
	} else {
		c.Metrics.RecordOrphanedPartitions(0)
	}

	c.Logger.Debug("assignments calculated", "worker_count", len(assignments))

	// Calculate removed workers for cleanup
	// We need to explicitly tell the publisher which workers to remove
	// because we're moving away from scanning all keys in the bucket.
	//
	// Optimization: Use map for O(1) lookups instead of O(N) slice scans
	activeWorkersMap := make(map[string]bool, len(workers))
	for _, w := range workers {
		activeWorkersMap[w] = true
	}

	var workersToRemove []string
	added := 0

	c.mu.RLock()
	// Calculate removed
	for w := range c.currentWorkers {
		if !activeWorkersMap[w] {
			workersToRemove = append(workersToRemove, w)
		}
	}
	// Calculate added
	for _, w := range workers {
		if !c.currentWorkers[w] {
			added++
		}
	}
	c.mu.RUnlock()

	// Record worker topology changes
	if added > 0 || len(workersToRemove) > 0 {
		c.Metrics.RecordWorkerChange(added, len(workersToRemove))
	}
	c.Metrics.RecordActiveWorkers(len(workers))

	// Pre-publish leadership check — abort if manager is shutting down to avoid
	// publishing stale assignments after leadership has been relinquished.
	if c.stateProvider != nil && c.stateProvider.State() == types.StateShutdown {
		c.Logger.Info("rebalance aborted: manager is shutting down")
		c.Metrics.RecordRebalanceAttempt(lifecycle, false)
		return errShuttingDown
	}

	// Embed the current leader revision so workers can detect assignments
	// from a former leader after a split-brain or leadership change.
	var leaderRevision uint64
	if c.LeaderRevision != nil {
		leaderRevision = c.LeaderRevision()
	}

	// Publish assignments via publisher component
	if err := c.publisher.Publish(ctx, workers, assignments, workersToRemove, lifecycle, leaderRevision); err != nil {
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
