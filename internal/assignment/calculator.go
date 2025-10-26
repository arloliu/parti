package assignment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/internal/logger"
	"github.com/arloliu/parti/internal/metrics"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
)

// calculatorState represents the internal state of the assignment calculator.
type calculatorState int32

const (
	// calcStateIdle indicates the calculator is idle (stable operation).
	calcStateIdle calculatorState = iota

	// calcStateScaling indicates the calculator is in a stabilization window.
	// Waiting for worker topology to stabilize before rebalancing.
	calcStateScaling

	// calcStateRebalancing indicates active rebalancing is in progress.
	// Assignments are being calculated and published.
	calcStateRebalancing

	// calcStateEmergency indicates emergency rebalancing (worker crash).
	// No stabilization window - immediate rebalancing required.
	calcStateEmergency
)

// String returns the string representation of calculator state.
func (s calculatorState) String() string {
	switch s {
	case calcStateIdle:
		return "Idle"
	case calcStateScaling:
		return "Scaling"
	case calcStateRebalancing:
		return "Rebalancing"
	case calcStateEmergency:
		return "Emergency"
	default:
		return "Unknown"
	}
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
	kv       jetstream.KeyValue
	prefix   string
	source   types.PartitionSource
	strategy types.AssignmentStrategy

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
	calcState     atomic.Int32    // calculatorState
	scalingStart  time.Time       // When scaling window started
	scalingReason string          // Reason: "cold_start", "planned_scale", "emergency", "restart"
	lastWorkers   map[string]bool // Previous worker set for comparison

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
//   - kv: NATS KV bucket for storing assignments
//   - prefix: Key prefix for assignment storage (e.g., "assignment")
//   - source: Partition source for discovering available partitions
//   - strategy: Assignment strategy for distributing partitions
//   - hbPrefix: Heartbeat key prefix for monitoring workers
//   - hbTTL: Heartbeat TTL duration for detecting dead workers
//
// Returns:
//   - *Calculator: New calculator instance ready to start
func NewCalculator(
	kv jetstream.KeyValue,
	prefix string,
	source types.PartitionSource,
	strategy types.AssignmentStrategy,
	hbPrefix string,
	hbTTL time.Duration,
) *Calculator {
	c := &Calculator{
		kv:                 kv,
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
		logger:             logger.NewNop(),
		currentWorkers:     make(map[string]bool),
		currentAssignments: make(map[string][]types.Partition),
		lastWorkers:        make(map[string]bool),
		stopCh:             make(chan struct{}),
		doneCh:             make(chan struct{}),
	}
	// Initialize calculator state to Idle
	c.calcState.Store(int32(calcStateIdle))
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
		c.logger = logger.NewNop()
	} else {
		c.logger = l
	}
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
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return errors.New("calculator already started")
	}
	c.started = true
	c.mu.Unlock()

	// Perform initial assignment with stabilization window
	if err := c.initialAssignment(ctx); err != nil {
		c.mu.Lock()
		c.started = false
		c.mu.Unlock()

		return fmt.Errorf("initial assignment failed: %w", err)
	}

	// Start background monitoring
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

	close(c.stopCh)
	<-c.doneCh

	c.mu.Lock()
	c.started = false
	c.mu.Unlock()

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
//   - string: Current state ("Idle", "Scaling", "Rebalancing", "Emergency")
func (c *Calculator) GetState() string {
	return calculatorState(c.calcState.Load()).String()
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
	return c.rebalance(ctx, "initial")
}

// detectRebalanceType determines the type of rebalance needed based on worker topology changes.
//
// This method analyzes worker count changes to classify the rebalance scenario:
//   - Cold start: Starting from 0 workers → Use 30s stabilization window
//   - Restart: >50% workers disappeared and rejoined → Use 30s window (like cold start)
//   - Emergency: One or more workers disappeared (crash) → No window, immediate rebalance
//   - Planned scale: Gradual worker additions → Use 10s stabilization window
//
// Parameters:
//   - currentWorkers: Current set of active workers
//
// Returns:
//   - reason: Rebalance type ("cold_start", "restart", "emergency", "planned_scale")
//   - window: Stabilization window duration
func (c *Calculator) detectRebalanceType(currentWorkers map[string]bool) (reason string, window time.Duration) {
	prevCount := len(c.lastWorkers)
	currCount := len(currentWorkers)

	// Emergency: Worker(s) disappeared (crash scenario)
	if currCount < prevCount {
		// Find which workers disappeared
		disappeared := make([]string, 0)
		for workerID := range c.lastWorkers {
			if !currentWorkers[workerID] {
				disappeared = append(disappeared, workerID)
			}
		}
		c.logger.Warn("workers disappeared - emergency rebalance",
			"disappeared", disappeared,
			"prev_count", prevCount,
			"curr_count", currCount,
		)

		return "emergency", 0 // No stabilization window
	}

	// Cold start: Starting from 0 workers
	if prevCount == 0 && currCount >= 1 {
		c.logger.Info("cold start detected",
			"workers", currCount,
		)

		return "cold_start", c.coldStartWindow
	}

	// Restart detection: Many workers disappeared then rejoined quickly
	// This happens when the entire system restarts (e.g., deployment)
	if prevCount > 0 {
		// Calculate how many workers from previous set are missing
		missingCount := 0
		for workerID := range c.lastWorkers {
			if !currentWorkers[workerID] {
				missingCount++
			}
		}

		// If more than restartRatio of workers changed, treat as restart
		missingRatio := float64(missingCount) / float64(prevCount)
		if missingRatio > c.restartRatio {
			c.logger.Info("restart detected",
				"prev_count", prevCount,
				"curr_count", currCount,
				"missing", missingCount,
				"missing_ratio", missingRatio,
			)

			return "restart", c.coldStartWindow // Use cold start window
		}
	}

	// Planned scale: Gradual worker additions
	c.logger.Info("planned scale detected",
		"prev_count", prevCount,
		"curr_count", currCount,
	)

	return "planned_scale", c.plannedScaleWin
}

// enterScalingState transitions the calculator into scaling state with stabilization window.
//
// Parameters:
//   - reason: Reason for scaling ("cold_start", "planned_scale", "restart")
//   - window: Stabilization window duration to wait before rebalancing
//   - ctx: Context for cancellation
func (c *Calculator) enterScalingState(reason string, window time.Duration, ctx context.Context) {
	c.calcState.Store(int32(calcStateScaling))

	c.mu.Lock()
	c.scalingStart = time.Now()
	c.scalingReason = reason
	c.mu.Unlock()

	c.logger.Info("entering scaling state",
		"reason", reason,
		"window", window,
	)

	// Start timer for scaling window
	go func() {
		select {
		case <-time.After(window):
			c.enterRebalancingState(ctx)
		case <-c.stopCh:
			return
		case <-ctx.Done():
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
	c.calcState.Store(int32(calcStateRebalancing))

	c.logger.Info("entering rebalancing state")

	// Perform rebalance
	if err := c.rebalance(ctx, c.scalingReason); err != nil {
		c.logger.Error("rebalancing failed", "error", err)
		// Return to idle even on error to allow retry
		c.returnToIdleState()

		return
	}

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
	c.calcState.Store(int32(calcStateEmergency))

	c.mu.Lock()
	c.scalingReason = "emergency"
	c.mu.Unlock()

	c.logger.Warn("entering emergency state - immediate rebalance")

	// Perform immediate rebalance
	if err := c.rebalance(ctx, "emergency"); err != nil {
		c.logger.Error("emergency rebalancing failed", "error", err)
		// Return to idle to allow retry
		c.returnToIdleState()

		return
	}

	// Successfully rebalanced, return to idle
	c.returnToIdleState()
}

// returnToIdleState transitions the calculator back to idle state after rebalancing completes.
func (c *Calculator) returnToIdleState() {
	c.calcState.Store(int32(calcStateIdle))

	c.mu.Lock()
	c.scalingReason = ""
	c.mu.Unlock()

	c.logger.Info("returned to idle state")
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
func (c *Calculator) monitorWorkers(ctx context.Context) {
	defer close(c.doneCh)

	ticker := time.NewTicker(c.hbTTL / 2) // Check twice per TTL
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := c.checkForChanges(ctx); err != nil {
				c.logger.Error("worker monitoring error", "error", err)
			}
		case <-c.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// checkForChanges detects worker changes and triggers rebalancing if needed.
func (c *Calculator) checkForChanges(ctx context.Context) error {
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
	cooldownActive := time.Since(c.lastRebalance) < c.cooldown
	currentState := calculatorState(c.calcState.Load())
	c.mu.RUnlock()

	if !changed {
		return nil
	}

	// Don't trigger rebalance if already scaling/rebalancing
	if currentState != calcStateIdle {
		c.logger.Info("worker change detected but calculator not idle",
			"state", currentState.String(),
		)

		return nil
	}

	if cooldownActive {
		c.logger.Info("rebalance needed but cooldown active", "cooldown", c.cooldown)

		return nil
	}

	c.logger.Info("worker change detected", "workers", len(workers))

	// Detect rebalance type and trigger appropriate state transition
	reason, window := c.detectRebalanceType(currentWorkers)

	// Update lastWorkers for next comparison
	c.mu.Lock()
	c.lastWorkers = make(map[string]bool)
	for w := range currentWorkers {
		c.lastWorkers[w] = true
	}
	c.mu.Unlock()

	// Trigger appropriate state transition based on reason
	if reason == "emergency" {
		// Emergency: immediate rebalance with no window
		c.enterEmergencyState(ctx)
	} else {
		// Cold start, planned scale, or restart: use stabilization window
		c.enterScalingState(reason, window, ctx)
	}

	return nil
}

// hasWorkersChanged checks if the worker set has changed.
func (c *Calculator) hasWorkersChanged(workers []string) bool {
	if len(workers) != len(c.currentWorkers) {
		return true
	}

	for _, w := range workers {
		if !c.currentWorkers[w] {
			return true
		}
	}

	return false
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
	keys, err := c.kv.Keys(ctx)
	if err != nil {
		// Handle "no keys found" as empty list
		if err.Error() == "nats: no keys found" {
			return []string{}, nil
		}

		return nil, fmt.Errorf("failed to list heartbeat keys: %w", err)
	}

	var workers []string
	for _, key := range keys {
		// Extract worker ID from key (format: "hbPrefix.workerID")
		if len(key) > len(c.hbPrefix)+1 && key[:len(c.hbPrefix)] == c.hbPrefix {
			workerID := key[len(c.hbPrefix)+1:]
			workers = append(workers, workerID)
		}
	}

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

	partitions, err := c.source.ListPartitions(ctx)
	if err != nil {
		return err
	}

	if len(workers) == 0 {
		c.logger.Info("no active workers for assignment")
		return nil
	}

	// Calculate new assignments using strategy
	assignments, err := c.strategy.Assign(workers, partitions)
	if err != nil {
		return fmt.Errorf("assignment calculation failed: %w", err)
	}

	// Increment version
	c.currentVersion++

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
		if _, err := c.kv.Put(ctx, key, data); err != nil {
			return fmt.Errorf("failed to publish assignment: %w", err)
		}
	}

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
