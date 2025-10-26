package parti

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/internal/assignment"
	"github.com/arloliu/parti/internal/election"
	"github.com/arloliu/parti/internal/heartbeat"
	"github.com/arloliu/parti/internal/hooks"
	"github.com/arloliu/parti/internal/logger"
	"github.com/arloliu/parti/internal/metrics"
	"github.com/arloliu/parti/internal/stableid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Manager coordinates workers in a distributed system for partition-based work distribution.
//
// Manager is the main entry point of the Parti library. It handles:
//   - Stable worker ID claiming using NATS KV
//   - Leader election for assignment coordination
//   - Partition assignment calculation and distribution
//   - Heartbeat publishing and failure detection
//   - Graceful rebalancing during scaling events
//
// Thread Safety:
//   - All public methods are safe for concurrent use
//   - State transitions are atomic and linearizable
//   - Assignment updates are copy-on-write
//
// Lifecycle:
//   - Create with NewManager()
//   - Call Start() to claim ID and begin coordination
//   - Use hooks to react to assignment changes
//   - Call Stop() for graceful shutdown
//
// Testing:
// Consumers can define minimal interfaces for mocking:
//
//	type WorkCoordinator interface {
//	    Start(ctx context.Context) error
//	    WorkerID() string
//	}
type Manager struct {
	cfg    Config
	conn   *nats.Conn
	source PartitionSource

	// Optional dependencies
	strategy      AssignmentStrategy
	electionAgent ElectionAgent
	hooks         *Hooks
	metrics       MetricsCollector
	logger        Logger

	// Internal components
	idClaimer  *stableid.Claimer
	election   *election.NATSElection
	heartbeat  *heartbeat.Publisher
	calculator *assignment.Calculator

	// KV buckets for coordination
	assignmentKV jetstream.KeyValue
	heartbeatKV  jetstream.KeyValue

	// State management
	state      atomic.Int32 // State
	workerID   atomic.Value // string
	isLeader   atomic.Bool
	assignment atomic.Value // Assignment

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.RWMutex
}

// NewManager creates a new Manager instance with the provided configuration.
//
// The Manager coordinates workers in a distributed system using NATS for:
//   - Stable worker ID claiming (via NATS KV)
//   - Leader election for assignment coordination
//   - Partition assignment distribution
//   - Heartbeat publication for health monitoring
//
// Returns a concrete *Manager struct following the "accept interfaces, return structs" principle.
// Consumers can define their own interfaces for testing if needed.
//
// Parameters:
//   - cfg: Runtime configuration with parsed durations
//   - conn: NATS connection for coordination
//   - source: Partition source for discovering partitions
//   - strategy: Assignment strategy for distributing partitions (recommended: strategy.NewConsistentHash())
//   - opts: Optional configuration (hooks, metrics, logger, election agent)
//
// Returns:
//   - *Manager: Initialized manager instance
//   - error: Validation error if configuration is invalid
//
// Example:
//
//	cfg := parti.Config{WorkerIDPrefix: "worker", WorkerIDMax: 63}
//	src := source.NewStatic(partitions)
//	curStrategy := strategy.NewConsistentHash()
//	mgr, err := parti.NewManager(&cfg, natsConn, src, curStrategy)
func NewManager(cfg *Config, conn *nats.Conn, source PartitionSource, strategy AssignmentStrategy, opts ...Option) (*Manager, error) {
	if cfg == nil {
		return nil, ErrInvalidConfig
	}
	if conn == nil {
		return nil, ErrNATSConnectionRequired
	}
	if source == nil {
		return nil, ErrPartitionSourceRequired
	}
	if strategy == nil {
		return nil, ErrAssignmentStrategyRequired
	}

	// Apply options
	options := &managerOptions{}
	for _, opt := range opts {
		opt(options)
	}

	// Provide safe defaults for optional dependencies to avoid nil checks everywhere
	metricsCollector := options.metrics
	if metricsCollector == nil {
		metricsCollector = metrics.NewNop()
	}

	loggerInstance := options.logger
	if loggerInstance == nil {
		loggerInstance = logger.NewNop()
	}

	hooksInstance := options.hooks
	if hooksInstance == nil {
		nopHooks := hooks.NewNop()
		hooksInstance = &nopHooks
	}

	m := &Manager{
		cfg:           *cfg,
		conn:          conn,
		source:        source,
		strategy:      strategy,
		electionAgent: options.electionAgent,
		hooks:         hooksInstance,
		metrics:       metricsCollector,
		logger:        loggerInstance,
	}

	// Initialize state
	m.state.Store(int32(StateInit))
	m.workerID.Store("")
	m.assignment.Store(Assignment{})

	return m, nil
}

// Start initializes and runs the manager.
//
// Blocks until worker ID claimed and initial assignment received.
//
// Parameters:
//   - ctx: Context for cancellation and timeout
//
// Returns:
//   - error: Startup error or context cancellation
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.ctx != nil {
		m.mu.Unlock()

		return ErrAlreadyStarted
	}

	// Create manager context with parent
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.mu.Unlock()

	// Apply startup timeout from the provided context
	startupCtx := ctx
	if m.cfg.StartupTimeout > 0 {
		var cancel context.CancelFunc
		startupCtx, cancel = context.WithTimeout(ctx, m.cfg.StartupTimeout)
		defer cancel()
	}

	// Initialize NATS JetStream
	js, err := jetstream.New(m.conn)
	if err != nil {
		return fmt.Errorf("failed to create jetstream context: %w", err)
	}

	// Create KV buckets for coordination
	stableIDKV, err := m.ensureKVBucket(startupCtx, js, "parti-stableid", m.cfg.WorkerIDTTL)
	if err != nil {
		return fmt.Errorf("failed to create stable ID KV: %w", err)
	}

	electionKV, err := m.ensureKVBucket(startupCtx, js, "parti-election", m.cfg.ElectionTimeout)
	if err != nil {
		return fmt.Errorf("failed to create election KV: %w", err)
	}

	heartbeatKV, err := m.ensureKVBucket(startupCtx, js, "parti-heartbeat", m.cfg.HeartbeatTTL)
	if err != nil {
		return fmt.Errorf("failed to create heartbeat KV: %w", err)
	}

	// Use heartbeat KV for both heartbeats and assignments (calculator expects them in same bucket)
	assignmentKV := heartbeatKV

	// Store KV buckets for later use
	m.assignmentKV = assignmentKV
	m.heartbeatKV = heartbeatKV

	// Step 1: Claim stable worker ID
	m.transitionState(m.State(), StateClaimingID)
	if err := m.claimWorkerID(startupCtx, stableIDKV); err != nil {
		return fmt.Errorf("failed to claim worker ID: %w", err)
	}

	// Step 2: Participate in leader election
	m.transitionState(m.State(), StateElection)
	if err := m.participateElection(startupCtx, electionKV); err != nil {
		return fmt.Errorf("failed to participate in election: %w", err)
	}

	// Step 3: Start heartbeat publisher
	if err := m.startHeartbeat(heartbeatKV); err != nil {
		return fmt.Errorf("failed to start heartbeat: %w", err)
	}

	// Step 4: If leader, start calculator
	if m.IsLeader() {
		if err := m.startCalculator(assignmentKV, heartbeatKV); err != nil {
			return fmt.Errorf("failed to start calculator: %w", err)
		}
	}

	// Step 5: Wait for assignment
	m.transitionState(m.State(), StateWaitingAssignment)
	// Use manager context for waiting (not the startup context which may be expiring)
	// Give plenty of time for calculator to stabilize and publish
	waitCtx, waitCancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer waitCancel()
	if err := m.waitForAssignment(waitCtx, assignmentKV, heartbeatKV); err != nil {
		return fmt.Errorf("failed to get assignment: %w", err)
	}

	// Step 6: Transition to stable state
	m.transitionState(m.State(), StateStable)

	// Start background workers
	m.wg.Add(1)
	go m.monitorAssignmentChanges(assignmentKV)

	return nil
}

// Stop gracefully shuts down the manager.
//
// Parameters:
//   - ctx: Context for shutdown timeout
//
// Returns:
//   - error: Shutdown error or timeout
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	if m.ctx == nil {
		m.mu.Unlock()

		return ErrNotStarted
	}

	// Transition to shutdown state
	m.transitionState(m.State(), StateShutdown)

	// Cancel manager context to stop all background goroutines
	m.cancel()
	m.mu.Unlock()

	// Shutdown sequence (reverse of startup)
	var shutdownErr error

	// Step 1: Stop calculator if running (leader only)
	if m.calculator != nil {
		m.stopCalculator()
	}

	// Step 2: Stop heartbeat publisher
	if m.heartbeat != nil {
		if err := m.heartbeat.Stop(); err != nil {
			m.logError("failed to stop heartbeat", "error", err)
			shutdownErr = fmt.Errorf("heartbeat stop failed: %w", err)
		}
	}

	// Step 3: Release election if we hold leadership
	if m.election != nil && m.IsLeader() {
		if err := m.election.ReleaseLeadership(ctx); err != nil {
			m.logError("failed to release leadership", "error", err)
			if shutdownErr == nil {
				shutdownErr = fmt.Errorf("leadership release failed: %w", err)
			}
		}
	}

	// Step 3: Release stable worker ID
	if m.idClaimer != nil {
		if err := m.idClaimer.Release(ctx); err != nil {
			m.logError("failed to release worker ID", "error", err)
			if shutdownErr == nil {
				shutdownErr = fmt.Errorf("worker ID release failed: %w", err)
			}
		}
	}

	// Step 4: Wait for all background goroutines with timeout
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		m.logger.Info("manager stopped gracefully")
		return shutdownErr
	case <-ctx.Done():
		m.logError("shutdown timeout exceeded, some goroutines may still be running")
		if shutdownErr == nil {
			return ctx.Err()
		}
		// Return both timeout error and any shutdown errors encountered
		return fmt.Errorf("shutdown timeout: %w; additional error: %w", ctx.Err(), shutdownErr)
	}
}

// WorkerID returns the claimed stable worker ID.
//
// Returns:
//   - string: Worker ID (empty if not claimed)
func (m *Manager) WorkerID() string {
	if id := m.workerID.Load(); id != nil {
		if str, ok := id.(string); ok {
			return str
		}
	}

	return ""
}

// IsLeader returns true if this worker is the current leader.
//
// Returns:
//   - bool: true if leader
func (m *Manager) IsLeader() bool {
	return m.isLeader.Load()
}

// CurrentAssignment returns a copy of the current assignment.
//
// Returns:
//   - Assignment: Current assignment (copy)
func (m *Manager) CurrentAssignment() Assignment {
	if a := m.assignment.Load(); a != nil {
		if asgn, ok := a.(Assignment); ok {
			return asgn
		}
	}

	return Assignment{}
}

// State returns the current worker state.
//
// Returns:
//   - State: Current state
func (m *Manager) State() State {
	return State(m.state.Load())
}

// RefreshPartitions triggers partition discovery refresh.
//
// This method forces the partition source to be re-queried and, if the worker is
// the leader, triggers an immediate rebalance with the updated partition list.
// Non-leader workers will receive the updated assignments automatically.
//
// Use this when:
//   - Partitions are added/removed dynamically (e.g., Kafka topics, Redis shards)
//   - You want to redistribute work after manual partition changes
//   - Your partition source has changed but workers haven't detected it yet
//
// Parameters:
//   - ctx: Context for operation timeout
//
// Returns:
//   - error: Refresh error, or ErrNotStarted if manager isn't running
//
// Example:
//
//	// After adding new partitions to your partition source
//	if err := manager.RefreshPartitions(ctx); err != nil {
//	    log.Printf("Failed to refresh partitions: %v", err)
//	}
func (m *Manager) RefreshPartitions(ctx context.Context) error {
	// Check if manager is started
	currentState := m.State()
	if currentState == StateInit || currentState == StateShutdown {
		return ErrNotStarted
	}

	// Only leaders can trigger rebalancing
	// Followers will receive updated assignments automatically
	if !m.IsLeader() {
		m.logger.Info("skipping partition refresh: not leader")
		return nil
	}

	// Check if calculator is available
	if m.calculator == nil {
		return errors.New("calculator not initialized")
	}

	m.logger.Info("refreshing partitions and triggering rebalance")

	// Trigger rebalance which will call source.ListPartitions() to get fresh partition list
	if err := m.calculator.TriggerRebalance(ctx); err != nil {
		return fmt.Errorf("failed to trigger rebalance: %w", err)
	}

	return nil
}

// transitionState transitions to a new state and triggers hooks.
func (m *Manager) transitionState(from, to State) {
	// Validate state transition
	if !m.isValidTransition(from, to) {
		m.logError("invalid state transition attempted",
			"from", from.String(),
			"to", to.String(),
		)

		return
	}

	m.state.Store(int32(to)) //nolint:gosec // State values are controlled enum

	m.logger.Info("state transition",
		"from", from.String(),
		"to", to.String(),
		"worker_id", m.WorkerID(),
	)

	// Trigger state change hook
	if m.hooks.OnStateChanged != nil {
		// Run hook in background to avoid blocking state machine
		go func() {
			if err := m.hooks.OnStateChanged(m.ctx, from, to); err != nil {
				m.logError("state change hook error", "from", from, "to", to, "error", err)
			}
		}()
	}

	// Record metrics (always non-nil, defaults to nopMetrics)
	m.metrics.RecordStateTransition(from, to, 0)
}

// isValidTransition validates that a state transition is allowed.
//
// Returns:
//   - bool: true if transition is valid, false otherwise
func (m *Manager) isValidTransition(from, to State) bool {
	// Define valid state transitions
	validTransitions := map[State][]State{
		StateInit:              {StateClaimingID, StateShutdown},
		StateClaimingID:        {StateElection, StateShutdown},
		StateElection:          {StateWaitingAssignment, StateShutdown},
		StateWaitingAssignment: {StateStable, StateShutdown},
		StateStable:            {StateScaling, StateRebalancing, StateEmergency, StateShutdown},
		StateScaling:           {StateRebalancing, StateStable, StateWaitingAssignment, StateShutdown}, // Added Stable and WaitingAssignment for leadership loss
		StateRebalancing:       {StateStable, StateWaitingAssignment, StateShutdown},                   // Added WaitingAssignment for leadership loss
		StateEmergency:         {StateStable, StateWaitingAssignment, StateShutdown},                   // Added WaitingAssignment for leadership loss
		StateShutdown:          {},                                                                     // Terminal state - no transitions allowed
	}

	allowedStates, exists := validTransitions[from]
	if !exists {
		return false
	}

	for _, allowed := range allowedStates {
		if allowed == to {
			return true
		}
	}

	return false
}

// logError logs an error message.
func (m *Manager) logError(msg string, keysAndValues ...any) {
	// Logger is always non-nil (defaults to nopLogger)
	m.logger.Error(msg, keysAndValues...)
}

// ensureKVBucket creates or opens a KV bucket with the specified TTL.
func (m *Manager) ensureKVBucket(ctx context.Context, js jetstream.JetStream, bucket string, ttl time.Duration) (jetstream.KeyValue, error) {
	cfg := jetstream.KeyValueConfig{
		Bucket:  bucket,
		History: 1, // Keep only latest value
	}

	if ttl > 0 {
		cfg.TTL = ttl
	}

	kv, err := js.CreateKeyValue(ctx, cfg)
	if err != nil {
		// If bucket exists, try to get it
		if errors.Is(err, jetstream.ErrBucketExists) {
			return js.KeyValue(ctx, bucket)
		}

		return nil, fmt.Errorf("failed to create KV bucket %s: %w", bucket, err)
	}

	return kv, nil
}

// claimWorkerID claims a stable worker ID.
func (m *Manager) claimWorkerID(ctx context.Context, kv jetstream.KeyValue) error {
	claimer := stableid.NewClaimer(
		kv,
		m.cfg.WorkerIDPrefix,
		m.cfg.WorkerIDMin,
		m.cfg.WorkerIDMax,
		m.cfg.WorkerIDTTL,
	)
	m.idClaimer = claimer

	workerID, err := claimer.Claim(ctx)
	if err != nil {
		return fmt.Errorf("failed to claim ID: %w", err)
	}

	m.workerID.Store(workerID)
	m.logger.Info("claimed stable worker ID", "worker_id", workerID)

	// Start renewal goroutine
	if err := claimer.StartRenewal(ctx); err != nil {
		return fmt.Errorf("failed to start renewal: %w", err)
	}

	return nil
}

// participateElection participates in leader election.
func (m *Manager) participateElection(ctx context.Context, kv jetstream.KeyValue) error {
	workerID := m.WorkerID()
	electionAgent := election.NewNATSElection(kv, "leader")
	m.election = electionAgent

	// Request leadership (TTL enforced by KV bucket)
	leaseDuration := int64(m.cfg.ElectionTimeout.Seconds())
	isLeader, err := electionAgent.RequestLeadership(ctx, workerID, leaseDuration)
	if err != nil {
		return fmt.Errorf("failed to request leadership: %w", err)
	}

	m.isLeader.Store(isLeader)

	if isLeader {
		m.logger.Info("elected as leader", "worker_id", workerID)
	} else {
		m.logger.Info("participating as follower", "worker_id", workerID)
	}

	// Start leadership monitoring
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.monitorLeadership()
	}()

	return nil
}

// monitorLeadership monitors leader changes.
func (m *Manager) monitorLeadership() {
	ticker := time.NewTicker(m.cfg.ElectionTimeout / 3)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			wasLeader := m.IsLeader()
			isLeader, err := m.election.IsLeader(m.ctx)
			if err != nil {
				m.logError("failed to check leadership", "error", err)

				continue
			}

			if wasLeader != isLeader {
				m.isLeader.Store(isLeader)

				if isLeader {
					m.logger.Info("became leader", "worker_id", m.WorkerID())
					// Start calculator
					if err := m.startCalculator(m.assignmentKV, m.heartbeatKV); err != nil {
						m.logError("failed to start calculator", "error", err)
					}
				} else {
					m.logger.Info("lost leadership", "worker_id", m.WorkerID())
					// Stop calculator
					m.stopCalculator()
				}
			}
		}
	}
}

// startHeartbeat starts publishing heartbeats.
func (m *Manager) startHeartbeat(kv jetstream.KeyValue) error {
	workerID := m.WorkerID()
	publisher := heartbeat.New(kv, "heartbeat", m.cfg.HeartbeatInterval)
	publisher.SetWorkerID(workerID)
	m.heartbeat = publisher

	// Start heartbeat in background
	if err := publisher.Start(m.ctx); err != nil {
		return fmt.Errorf("failed to start publisher: %w", err)
	}

	return nil
}

// waitForAssignment waits for initial assignment.
func (m *Manager) waitForAssignment(ctx context.Context, assignmentKV, _ jetstream.KeyValue) error {
	// If leader, calculate and publish initial assignment
	if m.IsLeader() {
		if err := m.calculateAndPublish(ctx); err != nil {
			return fmt.Errorf("failed to calculate initial assignment: %w", err)
		}
	}

	// Wait for assignment to appear in KV
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			asgn, err := m.fetchAssignment(ctx, assignmentKV)
			if err != nil {
				return fmt.Errorf("failed to fetch assignment: %w", err)
			}

			if asgn != nil {
				m.assignment.Store(*asgn)
				m.logger.Info("received initial assignment",
					"worker_id", m.WorkerID(),
					"partitions", len(asgn.Partitions),
					"version", asgn.Version,
				)

				return nil
			}
		}
	}
}

// startCalculator starts the assignment calculator (leader only).
func (m *Manager) startCalculator(assignmentKV, _ jetstream.KeyValue) error {
	if m.calculator != nil {
		return nil // Already started
	}

	calc := assignment.NewCalculator(
		assignmentKV, // Heartbeat KV bucket (contains both heartbeats and assignments)
		"assignment",
		m.source,
		m.strategy,
		"heartbeat", // Prefix for heartbeat keys in the same bucket
		m.cfg.HeartbeatTTL,
	)

	// Configure calculator with settings from config
	calc.SetCooldown(m.cfg.Assignment.RebalanceCooldown)
	calc.SetMinThreshold(m.cfg.Assignment.MinRebalanceThreshold)
	calc.SetRestartDetectionRatio(m.cfg.RestartDetectionRatio)
	calc.SetStabilizationWindows(m.cfg.ColdStartWindow, m.cfg.PlannedScaleWindow)

	// Set optional dependencies
	calc.SetMetrics(m.metrics)
	calc.SetLogger(m.logger)

	m.calculator = calc

	// Start calculator in background
	if err := calc.Start(m.ctx); err != nil {
		return fmt.Errorf("failed to start calculator: %w", err)
	}

	m.logger.Info("assignment calculator started", "worker_id", m.WorkerID())

	// Start monitoring calculator state (leader only)
	m.wg.Add(1)

	go m.monitorCalculatorState()

	return nil
}

// monitorCalculatorState monitors the calculator's internal state and syncs it to Manager state.
//
// This method runs only on the leader and translates calculator states to Manager states:
//   - calcStateScaling → StateScaling
//   - calcStateRebalancing → StateRebalancing
//   - calcStateEmergency → StateEmergency
//   - calcStateIdle (after rebalancing) → StateStable
func (m *Manager) monitorCalculatorState() {
	defer m.wg.Done()

	ticker := time.NewTicker(200 * time.Millisecond) // Check calculator state 5x per second
	defer ticker.Stop()

	var lastCalcState string

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if m.calculator == nil {
				// Calculator stopped (lost leadership)
				return
			}

			// Get current calculator state
			calcState := m.calculator.GetState()

			// Only transition if state changed
			if calcState == lastCalcState {
				continue
			}

			lastCalcState = calcState
			currentState := m.State()

			// Map calculator state to Manager state
			switch calcState {
			case "Scaling":
				// Calculator entered scaling window
				if currentState == StateStable {
					m.transitionState(currentState, StateScaling)
				}

			case "Rebalancing":
				// Calculator started rebalancing
				if currentState == StateScaling || currentState == StateEmergency {
					m.transitionState(currentState, StateRebalancing)
				} else if currentState == StateStable {
					// Direct rebalance (e.g., manual trigger)
					m.transitionState(currentState, StateRebalancing)
				}

			case "Emergency":
				// Calculator detected worker crash
				if currentState == StateStable {
					m.transitionState(currentState, StateEmergency)
				}

			case "Idle":
				// Calculator returned to idle after rebalancing
				if currentState == StateRebalancing {
					m.transitionState(currentState, StateStable)
				} else if currentState == StateEmergency {
					// Emergency rebalancing complete
					m.transitionState(currentState, StateStable)
				}
			}
		}
	}
}

// stopCalculator stops the assignment calculator.
func (m *Manager) stopCalculator() {
	if m.calculator == nil {
		return
	}

	// Before stopping, check if we need to transition state
	// If we're in a leader-only state (Scaling/Rebalancing/Emergency),
	// transition back to a follower state
	currentState := m.State()
	switch currentState {
	case StateScaling, StateRebalancing, StateEmergency:
		// Lost leadership while in leader-only state
		// Transition to Stable if we have an assignment, otherwise WaitingAssignment
		assignment := m.CurrentAssignment()
		if len(assignment.Partitions) > 0 {
			m.transitionState(currentState, StateStable)
			m.logger.Info("transitioned to Stable after losing leadership",
				"worker_id", m.WorkerID(),
				"from_state", currentState.String(),
			)
		} else {
			m.transitionState(currentState, StateWaitingAssignment)
			m.logger.Info("transitioned to WaitingAssignment after losing leadership",
				"worker_id", m.WorkerID(),
				"from_state", currentState.String(),
			)
		}
	}

	if err := m.calculator.Stop(); err != nil {
		m.logError("failed to stop calculator", "error", err)
	}

	m.calculator = nil
	m.logger.Info("assignment calculator stopped", "worker_id", m.WorkerID())
}

// calculateAndPublish calculates and publishes assignments.
func (m *Manager) calculateAndPublish(_ context.Context) error {
	if m.calculator == nil {
		return errors.New("calculator not started")
	}

	// Calculator runs in background and publishes automatically
	// Just wait a bit for initial calculation
	time.Sleep(500 * time.Millisecond)

	return nil
}

// fetchAssignment fetches the assignment for this worker from KV.
func (m *Manager) fetchAssignment(ctx context.Context, kv jetstream.KeyValue) (*Assignment, error) {
	workerID := m.WorkerID()
	key := fmt.Sprintf("assignment.%s", workerID) // Match calculator's key format
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil //nolint:nilnil // nil assignment with nil error indicates not yet assigned (valid state)
		}

		return nil, fmt.Errorf("failed to get assignment: %w", err)
	}

	var asgn Assignment
	if err := json.Unmarshal(entry.Value(), &asgn); err != nil {
		return nil, fmt.Errorf("failed to unmarshal assignment: %w", err)
	}

	return &asgn, nil
}

// monitorAssignmentChanges monitors for assignment changes.
func (m *Manager) monitorAssignmentChanges(kv jetstream.KeyValue) {
	defer m.wg.Done()

	workerID := m.WorkerID()
	key := fmt.Sprintf("assignment.%s", workerID) // Match calculator's key format
	watcher, err := kv.Watch(m.ctx, key)
	if err != nil {
		m.logError("failed to watch assignments", "error", err)

		return
	}
	defer func() {
		if err := watcher.Stop(); err != nil {
			m.logError("failed to stop watcher", "error", err)
		}
	}()

	for {
		select {
		case <-m.ctx.Done():
			return
		case entry := <-watcher.Updates():
			if entry == nil {
				continue
			}

			var newAssignment Assignment
			if err := json.Unmarshal(entry.Value(), &newAssignment); err != nil {
				m.logError("failed to unmarshal assignment", "error", err)

				continue
			}

			// Get old assignment
			oldAssignment := m.CurrentAssignment()

			// Check if assignment actually changed
			if oldAssignment.Version >= newAssignment.Version {
				continue // Ignore stale or duplicate assignments
			}

			// Update stored assignment
			m.assignment.Store(newAssignment)

			m.logger.Info("assignment updated",
				"worker_id", workerID,
				"old_version", oldAssignment.Version,
				"new_version", newAssignment.Version,
				"old_partitions", len(oldAssignment.Partitions),
				"new_partitions", len(newAssignment.Partitions),
			)

			// Trigger assignment change hook
			if m.hooks.OnAssignmentChanged != nil {
				// Run hook in background to avoid blocking
				go func() {
					if err := m.hooks.OnAssignmentChanged(m.ctx, oldAssignment.Partitions, newAssignment.Partitions); err != nil {
						m.logError("assignment change hook error", "error", err)
					}
				}()
			} // Record metrics
			added := len(newAssignment.Partitions) - len(oldAssignment.Partitions)
			if added < 0 {
				added = 0
			}
			removed := len(oldAssignment.Partitions) - len(newAssignment.Partitions)
			if removed < 0 {
				removed = 0
			}
			m.metrics.RecordAssignmentChange(added, removed, newAssignment.Version)
		}
	}
}
