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
	"github.com/arloliu/parti/internal/kvutil"
	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/internal/metrics"
	"github.com/arloliu/parti/internal/natsutil"
	"github.com/arloliu/parti/internal/stableid"
	"github.com/arloliu/parti/types"
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
	conn   *nats.Conn // underlying connection (extracted from js.Conn())
	js     jetstream.JetStream
	source PartitionSource

	// Optional dependencies
	strategy      AssignmentStrategy
	electionAgent ElectionAgent
	hooks         *Hooks
	metrics       MetricsCollector
	logger        Logger
	// Optional worker consumer updater
	consumerUpdater WorkerConsumerUpdater

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
	isLeader   atomic.Bool  // leadership status
	assignment atomic.Value // Assignment

	// Degraded mode tracking
	degradedSince      atomic.Value  // *time.Time - when degraded mode entered
	lastAssignmentAt   atomic.Value  // *time.Time - last successful assignment fetch
	lastAssignment     atomic.Value  // []Partition - cached assignment during degraded
	connMonitorOnce    sync.Once     // ensures single connection monitor goroutine
	connMonitorStop    chan struct{} // channel to stop connection monitor
	connDownSince      atomic.Value  // *time.Time - when connectivity lost
	connUpSince        atomic.Value  // *time.Time - when connectivity restored
	kvErrorCount       atomic.Int32  // consecutive KV error count
	kvErrorWindow      []time.Time   // timestamps of recent KV errors (protected by mu)
	recoveryGraceStart atomic.Value  // *time.Time - when recovery grace period started
	inRecoveryGrace    atomic.Bool   // true during recovery grace period

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.RWMutex
}

// Compile-time assertion that Manager implements StateProvider.
var _ types.StateProvider = (*Manager)(nil)

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
// Legacy Parameters (deprecated signature):
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
// Example (current JetStream-based constructor):
//
//	cfg := parti.Config{WorkerIDPrefix: "worker", WorkerIDMax: 63}
//	src := source.NewStatic(partitions)
//	curStrategy := strategy.NewConsistentHash()
//	js, _ := jetstream.New(natsConn)
//	mgr, err := parti.NewManager(&cfg, js, src, curStrategy)
//
// NewManager creates a new Manager using a pre-initialized JetStream context.
//
// Breaking change notice: This constructor replaces the previous signature that
// accepted *nats.Conn. Pass a jetstream.JetStream obtained via jetstream.New or
// custom domain/prefix constructors. The underlying *nats.Conn is captured for
// connectivity monitoring.
//
// Parameters:
//   - cfg: Runtime configuration (will be defaulted & validated)
//   - js: JetStream context (must be non-nil and backed by an active connection)
//   - source: Partition source implementation
//   - strategy: Assignment strategy implementation
//   - opts: Functional options (hooks, metrics, logger, election agent, consumer updater)
//
// Returns:
//   - *Manager: Initialized manager
//   - error: Configuration validation or nil dependency errors
func NewManager(cfg *Config, js jetstream.JetStream, source PartitionSource, strategy AssignmentStrategy, opts ...Option) (*Manager, error) {
	if cfg == nil {
		return nil, types.ErrInvalidConfig
	}
	if js == nil {
		return nil, types.ErrNATSConnectionRequired // reuse existing sentinel (represents missing transport)
	}
	if source == nil {
		return nil, types.ErrPartitionSourceRequired
	}
	if strategy == nil {
		return nil, types.ErrAssignmentStrategyRequired
	}

	// Fill defaults & validate
	SetDefaults(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	// Apply options
	options := &managerOptions{}
	for _, opt := range opts {
		opt(options)
	}

	metricsCollector := options.metrics
	if metricsCollector == nil {
		metricsCollector = metrics.NewNop()
	}
	loggerInstance := options.logger
	if loggerInstance == nil {
		loggerInstance = logging.NewNop()
	}
	cfg.ValidateWithWarnings(loggerInstance)
	hooksInstance := options.hooks
	if hooksInstance == nil {
		nopHooks := hooks.NewNop()
		hooksInstance = &nopHooks
	}

	m := &Manager{
		cfg:             *cfg,
		conn:            js.Conn(),
		js:              js,
		source:          source,
		strategy:        strategy,
		electionAgent:   options.electionAgent,
		hooks:           hooksInstance,
		metrics:         metricsCollector,
		logger:          loggerInstance,
		consumerUpdater: options.consumerUpdater,
		connMonitorStop: make(chan struct{}),
		kvErrorWindow:   make([]time.Time, 0, cfg.DegradedBehavior.KVErrorThreshold),
	}
	m.state.Store(int32(StateInit))
	m.workerID.Store("")
	m.assignment.Store(Assignment{})

	return m, nil
}

// Start initializes and runs the manager.
//
// Blocks until worker ID is claimed and the initial assignment is received.
// The manager lifecycle runs independently from the startup context - ctx is only
// used to control the startup timeout, not the manager's operational lifetime.
//
// If a WorkerConsumerUpdater was provided via WithWorkerConsumerUpdater, the
// initial assignment is applied (best-effort, asynchronously) to the worker's
// durable JetStream consumer immediately after it is fetched. Subsequent
// assignment changes will also trigger UpdateWorkerConsumer before Hooks.OnAssignmentChanged
// is invoked, enabling hot-reload of FilterSubjects without restarting pull loops.
//
// IMPORTANT: On error, caller MUST call Stop(ctx) to clean up resources:
//   - Stops ID renewal goroutine
//   - Releases claimed stable worker ID
//   - Cancels background operations
//   - Prevents goroutine and resource leaks
//
// Example usage:
//
//	mgr := parti.NewManager(cfg, conn)
//	if err := mgr.Start(ctx); err != nil {
//	    // Cleanup on startup failure
//	    _ = mgr.Stop(context.Background())
//	    return err
//	}
//	defer mgr.Stop(context.Background())
//
// Parameters:
//   - ctx: Context for startup timeout control (not manager lifetime)
//
// Returns:
//   - error: Startup error or context cancellation
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.ctx != nil {
		m.mu.Unlock()

		return types.ErrAlreadyStarted
	}

	// Create manager context with independent lifecycle (not tied to startup context)
	// The startup ctx parameter controls startup timeout, not manager lifetime
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.mu.Unlock()

	// Apply startup timeout from the provided context
	startupCtx := ctx
	if m.cfg.StartupTimeout > 0 {
		var cancel context.CancelFunc
		startupCtx, cancel = context.WithTimeout(ctx, m.cfg.StartupTimeout)
		defer cancel()
	}

	// Use injected JetStream context (already constructed by caller)
	js := m.js
	if js == nil {
		return errors.New("jetstream context not initialized")
	}

	// Create KV buckets for coordination
	stableIDKV, err := m.ensureKVBucket(startupCtx, js, m.cfg.KVBuckets.StableIDBucket, m.cfg.WorkerIDTTL)
	if err != nil {
		return fmt.Errorf("failed to create stable ID KV: %w", err)
	}

	electionKV, err := m.ensureKVBucket(startupCtx, js, m.cfg.KVBuckets.ElectionBucket, m.cfg.ElectionTimeout)
	if err != nil {
		return fmt.Errorf("failed to create election KV: %w", err)
	}

	heartbeatKV, err := m.ensureKVBucket(startupCtx, js, m.cfg.KVBuckets.HeartbeatBucket, m.cfg.HeartbeatTTL)
	if err != nil {
		return fmt.Errorf("failed to create heartbeat KV: %w", err)
	}

	// Create separate assignment bucket (no TTL - assignments persist for version continuity)
	assignmentKV, err := m.ensureKVBucket(startupCtx, js, m.cfg.KVBuckets.AssignmentBucket, m.cfg.KVBuckets.AssignmentTTL)
	if err != nil {
		return fmt.Errorf("failed to create assignment KV: %w", err)
	}

	// Store KV buckets for later use
	m.assignmentKV = assignmentKV
	m.heartbeatKV = heartbeatKV

	// Step 1: Claim stable worker ID
	m.logger.Debug("Claiming stable worker ID...")
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

	// Apply initial assignment to worker consumer (best-effort, async)
	if m.consumerUpdater != nil {
		initial := m.CurrentAssignment()
		if len(initial.Partitions) > 0 {
			wid := m.WorkerID()
			m.wg.Go(func() {
				if err := m.consumerUpdater.UpdateWorkerConsumer(m.ctx, wid, initial.Partitions); err != nil {
					m.logError("initial consumer update error", "error", err)
				}
			})
		}
	}

	// Step 6: Transition to stable state
	m.transitionState(m.State(), StateStable)

	// Start background workers
	m.wg.Go(func() {
		m.monitorAssignmentChanges(m.ctx, assignmentKV)
	})

	// Start degraded mode connection monitoring
	m.monitorNATSConnection()

	return nil
}

// Stop gracefully shuts down the manager.
//
// Safe to call multiple times - subsequent calls will return ErrNotStarted.
//
// Parameters:
//   - ctx: Context for shutdown timeout
//
// Returns:
//   - error: Shutdown error or timeout
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()

	// Check if already stopped or never started
	if m.ctx == nil {
		m.mu.Unlock()

		return types.ErrNotStarted
	}

	// Check if already in shutdown state (concurrent Stop() call)
	currentState := m.State()
	if currentState == StateShutdown {
		m.mu.Unlock()

		return types.ErrNotStarted
	}

	// Transition to shutdown state
	m.transitionState(currentState, StateShutdown)

	// Cancel manager context to stop all background goroutines
	// This will cause monitorAssignmentChanges watcher to close
	m.cancel()

	// Note: Keep m.ctx (even though cancelled) instead of setting to nil
	// so background goroutines can still use it in their select statements
	m.mu.Unlock()

	// Shutdown sequence (reverse of startup)
	var shutdownErr error

	// Step 1: Stop calculator if running (leader only)
	if m.calculator != nil {
		m.logger.Info("stopping calculator", "worker_id", m.WorkerID())
		m.stopCalculator()
		m.logger.Info("calculator stopped", "worker_id", m.WorkerID())
	}

	// Step 1.5: Stop degraded mode connection monitor
	select {
	case m.connMonitorStop <- struct{}{}:
	default:
		// Channel already closed or monitor not running
	}

	// Step 2: Stop heartbeat publisher (ignore ErrNotStarted)
	if m.heartbeat != nil {
		if err := m.heartbeat.Stop(); err != nil && !errors.Is(err, heartbeat.ErrNotStarted) {
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

	// Step 4: Release stable worker ID (ignore ErrNotClaimed)
	if m.idClaimer != nil {
		if err := m.idClaimer.Release(ctx); err != nil && !errors.Is(err, stableid.ErrNotClaimed) {
			m.logError("failed to release worker ID", "error", err)
			if shutdownErr == nil {
				shutdownErr = fmt.Errorf("worker ID release failed: %w", err)
			}
		}
	}

	// Step 5: Wait for all background goroutines with timeout
	m.logger.Debug("waiting for goroutines to exit...", "worker_id", m.WorkerID())
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

// WaitState waits for the manager to reach the expected state within the timeout period.
//
// This method is useful for testing and synchronization scenarios where you need to
// wait for the manager to reach a specific state before proceeding.
//
// The method returns a read-only channel that will receive exactly one value:
//   - nil if the expected state is reached within the timeout
//   - context.DeadlineExceeded if the timeout expires before reaching the state
//
// The channel is closed after sending the result, allowing safe use in select statements.
//
// Parameters:
//   - expectedState: The state to wait for
//   - timeout: Maximum duration to wait for the state
//
// Returns:
//   - <-chan error: A channel that receives the result (nil on success, error on timeout)
//
// Example:
//
//	// Wait for manager to reach Stable state
//	errCh := manager.WaitState(StateStable, 10*time.Second)
//	if err := <-errCh; err != nil {
//	    log.Printf("Failed to reach Stable state: %v", err)
//	}
//
//	// Using with select for multiple operations
//	select {
//	case err := <-manager.WaitState(StateStable, 5*time.Second):
//	    if err != nil {
//	        return fmt.Errorf("timeout waiting for stable state: %w", err)
//	    }
//	case <-ctx.Done():
//	    return ctx.Err()
//	}
//
//	// Waiting for multiple managers
//	for i, mgr := range managers {
//	    if err := <-mgr.WaitState(StateStable, 10*time.Second); err != nil {
//	        return fmt.Errorf("manager %d failed: %w", i, err)
//	    }
//	}
func (m *Manager) WaitState(expectedState State, timeout time.Duration) <-chan error {
	ch := make(chan error, 1) // Buffered to prevent goroutine leak

	go func() {
		defer close(ch)

		// Check if already in expected state
		if m.State() == expectedState {
			ch <- nil
			return
		}

		// Poll for state changes
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		timeoutTimer := time.NewTimer(timeout)
		defer timeoutTimer.Stop()

		for {
			select {
			case <-ticker.C:
				if m.State() == expectedState {
					ch <- nil
					return
				}
			case <-timeoutTimer.C:
				ch <- context.DeadlineExceeded
				return
			}
		}
	}()

	return ch
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
		return types.ErrNotStarted
	}

	// Only leaders can trigger rebalancing
	// Followers will receive updated assignments automatically
	if !m.IsLeader() {
		m.logger.Info("skipping partition refresh: not leader")
		return nil
	}

	// Check if calculator is available
	m.mu.RLock()
	calc := m.calculator
	m.mu.RUnlock()

	if calc == nil {
		return errors.New("calculator not initialized")
	}

	m.logger.Info("refreshing partitions and triggering rebalance")

	// Trigger rebalance which will call source.ListPartitions() to get fresh partition list
	if err := calc.TriggerRebalance(ctx); err != nil {
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
		StateWaitingAssignment: {StateStable, StateScaling, StateRebalancing, StateEmergency, StateShutdown},
		StateStable:            {StateScaling, StateRebalancing, StateEmergency, StateShutdown},
		StateScaling:           {StateRebalancing, StateWaitingAssignment, StateStable, StateShutdown},
		StateRebalancing:       {StateStable, StateWaitingAssignment, StateShutdown},
		StateEmergency:         {StateStable, StateWaitingAssignment, StateShutdown},
		StateShutdown:          {}, // Terminal state - no transitions allowed
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
//
// Uses retry logic to handle race conditions when multiple workers
// try to create the same bucket concurrently.
func (m *Manager) ensureKVBucket(ctx context.Context, js jetstream.JetStream, bucket string, ttl time.Duration) (jetstream.KeyValue, error) {
	cfg := jetstream.KeyValueConfig{
		Bucket:  bucket,
		History: 1, // Keep only latest value
	}

	if ttl > 0 {
		cfg.TTL = ttl
	}

	// Use retry logic to handle concurrent creation
	const maxRetries = 5
	kv, err := kvutil.EnsureKVBucketWithRetry(ctx, js, cfg, maxRetries)
	if err != nil {
		return nil, fmt.Errorf("failed to create/open KV bucket %s: %w", bucket, err)
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
		m.logger,
	)
	m.idClaimer = claimer

	workerID, err := claimer.Claim(ctx)
	if err != nil {
		return fmt.Errorf("failed to claim ID: %w", err)
	}

	m.workerID.Store(workerID)
	m.logger.Info("claimed stable worker ID", "worker_id", workerID)

	// Start renewal goroutine with manager's lifecycle context (not startup context)
	// CRITICAL: Must use m.ctx (manager lifecycle) not ctx (startup context)
	// The startup context gets cancelled after Start() returns, which would
	// stop renewal and allow the stable ID to expire and be reclaimed by other workers.
	if err := claimer.StartRenewal(m.ctx); err != nil {
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
	m.wg.Go(m.monitorLeadership)

	return nil
}

// monitorLeadership monitors leader changes and renews leadership lease.
//
// Leaders periodically renew their lease to maintain leadership.
// Followers periodically attempt to claim leadership if it becomes vacant.
func (m *Manager) monitorLeadership() {
	ticker := time.NewTicker(m.cfg.ElectionTimeout / 3)
	defer ticker.Stop()

	leaseDuration := int64(m.cfg.ElectionTimeout.Seconds())

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			wasLeader := m.IsLeader()

			// If we're the leader, renew the lease
			if wasLeader {
				if err := m.election.RenewLeadership(m.ctx); err != nil {
					m.logError("failed to renew leadership", "error", err)
					// Leadership lost
					m.isLeader.Store(false)
					m.logger.Info("lost leadership", "worker_id", m.WorkerID())
					m.stopCalculator()

					continue
				}
			} else {
				// Follower: Try to claim leadership if vacant
				isLeader, err := m.election.RequestLeadership(m.ctx, m.WorkerID(), leaseDuration)
				if err != nil {
					m.logError("failed to request leadership", "error", err)

					continue
				}

				// Check if we became leader
				if isLeader {
					m.isLeader.Store(true)
					m.logger.Info("became leader", "worker_id", m.WorkerID())

					// Start calculator
					if err := m.startCalculator(m.assignmentKV, m.heartbeatKV); err != nil {
						m.logError("failed to start calculator", "error", err)
					}
				}
			}
		}
	}
}

// startHeartbeat starts publishing heartbeats.
func (m *Manager) startHeartbeat(kv jetstream.KeyValue) error {
	workerID := m.WorkerID()
	publisher := heartbeat.New(kv, "heartbeat", workerID, m.cfg.HeartbeatInterval, m.metrics, m.logger)
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
			curAssignment, err := m.fetchAssignment(ctx, assignmentKV)
			if err != nil {
				return fmt.Errorf("failed to fetch assignment: %w", err)
			}

			if curAssignment != nil {
				m.assignment.Store(*curAssignment)
				m.logger.Info("received initial assignment",
					"worker_id", m.WorkerID(),
					"partitions", len(curAssignment.Partitions),
					"version", curAssignment.Version,
				)

				return nil
			}
		}
	}
}

// startCalculator starts the assignment calculator (leader only).
func (m *Manager) startCalculator(assignmentKV, heartbeatKV jetstream.KeyValue) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.calculator != nil {
		return nil // Already started
	}

	calc, err := assignment.NewCalculator(&assignment.Config{
		AssignmentKV:         assignmentKV,
		HeartbeatKV:          heartbeatKV,
		Source:               m.source,
		Strategy:             m.strategy,
		AssignmentPrefix:     "assignment",
		HeartbeatPrefix:      "heartbeat",
		HeartbeatTTL:         m.cfg.HeartbeatTTL,
		EmergencyGracePeriod: m.cfg.EmergencyGracePeriod,
		Cooldown:             m.cfg.Assignment.MinRebalanceInterval,
		MinThreshold:         m.cfg.Assignment.MinRebalanceThreshold,
		RestartRatio:         m.cfg.RestartDetectionRatio,
		ColdStartWindow:      m.cfg.ColdStartWindow,
		PlannedScaleWindow:   m.cfg.PlannedScaleWindow,
		Metrics:              m.metrics,
		Logger:               m.logger,
		StateProvider:        m, // Pass manager as state provider for degraded mode checks
	})
	if err != nil {
		return fmt.Errorf("failed to create calculator: %w", err)
	}

	m.calculator = calc

	// Start monitoring calculator state BEFORE starting the calculator
	// This ensures we don't miss any state transitions that happen during startup
	m.wg.Go(m.monitorCalculatorState)

	// Give the monitor goroutine a moment to set up its subscription
	// This prevents race conditions where calculator state changes before monitor is ready
	time.Sleep(10 * time.Millisecond)

	// Start calculator in background
	if err := calc.Start(m.ctx); err != nil {
		m.calculator = nil
		return fmt.Errorf("failed to start calculator: %w", err)
	}

	m.logger.Info("assignment calculator started", "worker_id", m.WorkerID())

	return nil
}

// monitorCalculatorState monitors the calculator's internal state and syncs it to Manager state.
//
// This goroutine listens to the Calculator's state change channel and updates
// the Manager's state machine accordingly. Replaces the previous polling-based
// approach (200ms ticker) with event-driven synchronization for zero-lag updates.
//
// This method runs only on the leader and translates calculator states to Manager states:
//   - types.CalcStateScaling → StateScaling
//   - types.CalcStateRebalancing → StateRebalancing
//   - types.CalcStateEmergency → StateEmergency
//   - types.CalcStateIdle (after rebalancing) → StateStable
func (m *Manager) monitorCalculatorState() {
	m.logger.Info("starting calculator state monitor")

	// Subscribe to calculator state changes with mutex protection
	m.mu.RLock()
	calc := m.calculator
	m.mu.RUnlock()

	if calc == nil {
		m.logger.Warn("calculator is nil, cannot monitor state")
		return
	}

	stateCh, unsubscribe := calc.SubscribeToStateChanges()
	defer unsubscribe()

	for {
		select {
		case <-m.ctx.Done():
			m.logger.Info("calculator state monitor stopped")
			return

		case calcState, ok := <-stateCh:
			if !ok {
				m.logger.Info("calculator state channel closed, stopping monitor")
				return
			}
			// Synchronize Manager state based on Calculator state
			if err := m.syncStateFromCalculator(calcState); err != nil {
				m.logError("failed to sync state from calculator",
					"calc_state", calcState,
					"error", err,
				)
			}
		}
	}
}

// syncStateFromCalculator updates Manager state based on Calculator state.
//
// State mapping:
//   - CalcStateIdle       → StateStable (if Manager is in active state)
//   - CalcStateScaling    → StateScaling
//   - CalcStateRebalancing → StateRebalancing
//   - CalcStateEmergency  → StateEmergency
//
// Parameters:
//   - calcState: Current calculator state to synchronize with
//
// Returns:
//   - error: State transition error if invalid transition attempted
func (m *Manager) syncStateFromCalculator(calcState types.CalculatorState) error {
	currentState := m.State()

	// Skip if Manager is in initialization or shutdown states
	// BUT allow Scaling/Rebalancing/Emergency states to be processed even from WaitingAssignment
	if currentState == StateInit || currentState == StateClaimingID ||
		currentState == StateElection || currentState == StateShutdown {
		return nil
	}

	// Special handling for WaitingAssignment: only process active calculator states
	if currentState == StateWaitingAssignment {
		if calcState == types.CalcStateIdle {
			return nil
		}
		// Allow Scaling/Rebalancing/Emergency to transition from WaitingAssignment
	}

	var targetState State

	switch calcState {
	case types.CalcStateIdle:
		// Only transition to Stable if we're in an intermediate state.
		// This prevents flapping back to Stable when we're already stable,
		// which can happen when subscribing to calculator state changes
		// (the calculator sends its current state immediately on subscription).
		if currentState != StateScaling && currentState != StateRebalancing && currentState != StateEmergency {
			// Already stable or in a non-active state, no transition needed.
			return nil
		}

		targetState = StateStable

	case types.CalcStateScaling:
		targetState = StateScaling

	case types.CalcStateRebalancing:
		targetState = StateRebalancing

	case types.CalcStateEmergency:
		targetState = StateEmergency

	default:
		return fmt.Errorf("unknown calculator state: %v", calcState)
	}

	// Only transition if state actually changed
	if currentState != targetState {
		m.transitionState(currentState, targetState)
	}

	return nil
}

// stopCalculator stops the assignment calculator.
func (m *Manager) stopCalculator() {
	m.mu.Lock()
	defer m.mu.Unlock()

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
		currentAssignment := m.CurrentAssignment()
		if len(currentAssignment.Partitions) > 0 {
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

	default:
		// No state transition needed for non-leader states
	}

	// Stop calculator with fresh context for cleanup
	// IMPORTANT: Cannot use m.ctx here because it's already cancelled during Stop()
	// Creating a timeout from cancelled context would result in immediate cancellation
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()

	if err := m.calculator.Stop(stopCtx); err != nil {
		m.logError("failed to stop calculator", "error", err)
	}

	m.calculator = nil

	m.logger.Info("assignment calculator stopped", "worker_id", m.WorkerID())
}

// calculateAndPublish calculates and publishes assignments.
func (m *Manager) calculateAndPublish(_ context.Context) error {
	m.mu.RLock()
	calc := m.calculator
	m.mu.RUnlock()

	if calc == nil {
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
func (m *Manager) monitorAssignmentChanges(ctx context.Context, kv jetstream.KeyValue) {
	workerID := m.WorkerID()
	key := fmt.Sprintf("assignment.%s", workerID) // Match calculator's key format

	// Watch for updates to this worker's assignment key
	// The watcher will deliver initial value, then a nil entry marker, then future updates
	watcher, err := kv.Watch(ctx, key)
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
		case <-ctx.Done():
			m.logger.Debug("assignment monitor stopping (context cancelled)", "worker_id", workerID)
			return
		case entry := <-watcher.Updates():
			if entry == nil {
				// Nil entry indicates end of initial values replay
				// This is normal - continue watching for future updates
				continue
			}

			if entry.Operation() == jetstream.KeyValueDelete {
				m.logger.Debug("ignoring assignment deletion during leader transition")
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
			if m.hooks.OnAssignmentChanged != nil || m.consumerUpdater != nil {
				// Run callbacks in background to avoid blocking
				m.wg.Go(func() {
					// 1) Invoke consumer updater if provided
					if m.consumerUpdater != nil {
						if err := m.consumerUpdater.UpdateWorkerConsumer(m.ctx, workerID, newAssignment.Partitions); err != nil {
							m.logError("consumer updater error", "error", err)
						}
					}

					// 2) Invoke OnAssignmentChanged hook if provided
					if m.hooks.OnAssignmentChanged != nil {
						if err := m.hooks.OnAssignmentChanged(m.ctx, oldAssignment.Partitions, newAssignment.Partitions); err != nil {
							m.logError("assignment change hook error", "error", err)
						}
					}
				})
			}

			// Record metrics
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

// ============================================================================
// Degraded Mode - Connection Monitoring
// ============================================================================

// monitorNATSConnection starts a goroutine that monitors NATS connectivity.
// Uses connMonitorOnce to ensure only one monitor runs per Manager instance.
func (m *Manager) monitorNATSConnection() {
	m.connMonitorOnce.Do(func() {
		m.wg.Go(func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-m.ctx.Done():
					return
				case <-m.connMonitorStop:
					return
				case <-ticker.C:
					m.checkConnectionHealth()
				}
			}
		})
	})
}

// checkConnectionHealth checks NATS connection status and updates degraded state.
func (m *Manager) checkConnectionHealth() {
	isConnected := m.conn.Status() == nats.CONNECTED

	now := time.Now()

	if !isConnected {
		// Connection is down
		if val := m.connDownSince.Load(); val == nil {
			// First detection of disconnection
			m.connDownSince.Store(&now)
			m.connUpSince.Store((*time.Time)(nil))
			m.logger.Warn("NATS connection lost", "time", now)
		} else {
			// Check if we should enter degraded mode
			downSince, _ := val.(*time.Time)
			if downSince != nil && time.Since(*downSince) >= m.cfg.DegradedBehavior.EnterThreshold {
				m.enterDegraded("NATS connection down")
			}
		}

		return
	}

	// Connection is up
	if val := m.connUpSince.Load(); val == nil {
		// First detection of reconnection
		m.connUpSince.Store(&now)
		m.connDownSince.Store((*time.Time)(nil))
		m.logger.Info("NATS connection restored", "time", now)
	} else {
		// Check if we should exit degraded mode
		upSince, _ := val.(*time.Time)
		if upSince != nil && time.Since(*upSince) >= m.cfg.DegradedBehavior.ExitThreshold {
			m.attemptRecoveryFromDegraded()
		}
	}
}

// recordKVError records a KV operation error and may trigger degraded mode.
func (m *Manager) recordKVError(err error) {
	if err == nil || !natsutil.IsConnectivityError(err) {
		return
	}

	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Add error timestamp
	m.kvErrorWindow = append(m.kvErrorWindow, now)

	// Remove errors outside the window
	windowStart := now.Add(-m.cfg.DegradedBehavior.KVErrorWindow)
	validIdx := 0
	for i, t := range m.kvErrorWindow {
		if t.After(windowStart) {
			validIdx = i
			break
		}
	}
	m.kvErrorWindow = m.kvErrorWindow[validIdx:]

	// Update error count (safe conversion with bounds check)
	// Extremely unlikely, but handle overflow case
	windowLen := min(len(m.kvErrorWindow), 0x7FFFFFFF)

	count := int32(windowLen) // #nosec G115 - bounded above
	m.kvErrorCount.Store(count)

	// Check threshold
	if int(count) >= m.cfg.DegradedBehavior.KVErrorThreshold {
		m.logger.Warn("KV error threshold exceeded",
			"count", count,
			"threshold", m.cfg.DegradedBehavior.KVErrorThreshold,
			"window", m.cfg.DegradedBehavior.KVErrorWindow,
		)
		m.enterDegraded("KV error threshold exceeded")
	}
}

// recordKVSuccess records a successful KV operation and resets error count.
func (m *Manager) recordKVSuccess() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.kvErrorWindow = m.kvErrorWindow[:0]
	m.kvErrorCount.Store(0)
}

// ============================================================================
// Degraded Mode - State Management
// ============================================================================

// enterDegraded transitions the manager to degraded mode.
func (m *Manager) enterDegraded(reason string) {
	// Check if already in degraded mode
	if val := m.degradedSince.Load(); val != nil {
		return
	}

	now := time.Now()
	m.degradedSince.Store(&now)

	// Update state
	oldState := State(m.state.Swap(int32(types.StateDegraded)))

	m.logger.Warn("entering degraded mode",
		"reason", reason,
		"previous_state", oldState,
		"time", now,
	)

	// Trigger state change hook
	if m.hooks.OnStateChanged != nil {
		go func() {
			if err := m.hooks.OnStateChanged(m.ctx, oldState, types.StateDegraded); err != nil {
				m.logError("state change hook error", "error", err)
			}
		}()
	}

	// Record metrics
	m.metrics.RecordStateTransition(oldState, types.StateDegraded, 0)
	m.metrics.SetDegradedMode(1.0)

	// Start alert monitoring
	m.wg.Go(m.monitorDegradedAlerts)
}

// exitDegraded transitions the manager out of degraded mode.
func (m *Manager) exitDegraded() {
	// Check if in degraded mode
	val := m.degradedSince.Load()
	if val == nil {
		return
	}

	tVal, _ := val.(*time.Time)
	duration := time.Since(*tVal)
	m.degradedSince.Store((*time.Time)(nil))

	// Restore previous state (typically Stable or WaitingAssignment)
	oldState := State(m.state.Swap(int32(StateStable)))

	m.logger.Info("exiting degraded mode",
		"duration", duration,
		"next_state", StateStable,
	)

	// Trigger state change hook
	if m.hooks.OnStateChanged != nil {
		go func() {
			if err := m.hooks.OnStateChanged(m.ctx, oldState, StateStable); err != nil {
				m.logError("state change hook error", "error", err)
			}
		}()
	}

	// Record metrics
	m.metrics.RecordStateTransition(oldState, StateStable, duration.Seconds())
	m.metrics.RecordDegradedDuration(duration.Seconds())
	m.metrics.SetDegradedMode(0.0)
	m.metrics.SetCacheAge(0.0)
	m.metrics.SetAlertLevel(0)

	// Start recovery grace period if leader
	if m.isLeader.Load() {
		m.enterRecoveryGracePeriod()
	}
}

// attemptRecoveryFromDegraded checks if recovery conditions are met and exits degraded mode.
func (m *Manager) attemptRecoveryFromDegraded() {
	// Check if in degraded mode
	if val := m.degradedSince.Load(); val == nil {
		return
	}

	// Try to refresh assignment from NATS
	if err := m.refreshAssignmentFromNATS(); err != nil {
		m.logger.Warn("failed to refresh assignment during recovery", "error", err)
		m.recordKVError(err)
		return
	}

	// Success - exit degraded mode
	m.recordKVSuccess()
	m.exitDegraded()
}

// ============================================================================
// Degraded Mode - Recovery Grace Period
// ============================================================================

// enterRecoveryGracePeriod starts the recovery grace period for the leader.
func (m *Manager) enterRecoveryGracePeriod() {
	now := time.Now()
	m.recoveryGraceStart.Store(&now)
	m.inRecoveryGrace.Store(true)

	m.logger.Info("entering recovery grace period",
		"duration", m.cfg.DegradedBehavior.RecoveryGracePeriod,
	)

	m.wg.Go(func() {
		timer := time.NewTimer(m.cfg.DegradedBehavior.RecoveryGracePeriod)
		defer timer.Stop()

		select {
		case <-m.ctx.Done():
			return
		case <-timer.C:
			m.exitRecoveryGracePeriod()
		}
	})
}

// exitRecoveryGracePeriod ends the recovery grace period.
func (m *Manager) exitRecoveryGracePeriod() {
	if !m.inRecoveryGrace.Load() {
		return
	}

	duration := time.Duration(0)
	if val := m.recoveryGraceStart.Load(); val != nil {
		if timePtr, ok := val.(*time.Time); ok {
			duration = time.Since(*timePtr)
		}
	}

	m.recoveryGraceStart.Store((*time.Time)(nil))
	m.inRecoveryGrace.Store(false)

	m.logger.Info("exiting recovery grace period", "duration", duration)
}

// IsInRecoveryGrace returns true if currently in recovery grace period.
//
// This is part of the StateProvider interface and allows components like
// Calculator to check recovery grace status without circular dependencies.
//
// Returns:
//   - bool: true if in recovery grace period
func (m *Manager) IsInRecoveryGrace() bool {
	return m.inRecoveryGrace.Load()
}

// ============================================================================
// Degraded Mode - Assignment Caching
// ============================================================================

// refreshAssignmentFromNATS attempts to fetch the current assignment from NATS KV.
func (m *Manager) refreshAssignmentFromNATS() error {
	workerID := m.WorkerID()
	if workerID == "" {
		return errors.New("worker ID not set")
	}

	key := fmt.Sprintf("assignment.%s", workerID)
	entry, err := m.assignmentKV.Get(m.ctx, key)
	if err != nil {
		return fmt.Errorf("failed to get assignment from KV: %w", err)
	}

	var curAssignment Assignment
	if err := json.Unmarshal(entry.Value(), &curAssignment); err != nil {
		return fmt.Errorf("failed to unmarshal assignment: %w", err)
	}

	now := time.Now()
	m.assignment.Store(curAssignment)
	m.lastAssignmentAt.Store(&now)
	m.lastAssignment.Store(m.clonePartitions(curAssignment.Partitions))

	m.logger.Info("assignment refreshed from NATS",
		"version", curAssignment.Version,
		"partitions", len(curAssignment.Partitions),
	)

	return nil
}

// clonePartitions creates a deep copy of partition slice.
func (m *Manager) clonePartitions(partitions []Partition) []Partition {
	if partitions == nil {
		return nil
	}

	cloned := make([]Partition, len(partitions))
	for i, p := range partitions {
		cloned[i] = Partition{
			Keys:   append([]string(nil), p.Keys...),
			Weight: p.Weight,
		}
	}

	return cloned
}

// ============================================================================
// Degraded Mode - Alert Monitoring
// ============================================================================

// monitorDegradedAlerts monitors degraded mode duration and emits alerts.
func (m *Manager) monitorDegradedAlerts() {
	ticker := time.NewTicker(m.cfg.DegradedAlert.AlertInterval)
	defer ticker.Stop()

	lastAlertLevel := AlertLevelInfo - 1 // Start below Info to trigger first alert

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			// Check if still in degraded mode
			val := m.degradedSince.Load()
			if val == nil {
				return // Exited degraded mode
			}

			degradedSince, _ := val.(*time.Time)
			duration := time.Since(*degradedSince)
			level := m.calculateAlertLevel(*degradedSince)

			// Update cache age metric
			m.metrics.SetCacheAge(duration.Seconds())

			// Only emit if level increased
			if level > lastAlertLevel {
				m.emitDegradedAlert(level, *degradedSince)
				lastAlertLevel = level
			}
		}
	}
}

// emitDegradedAlert emits a degraded mode alert at the specified level.
func (m *Manager) emitDegradedAlert(level AlertLevel, degradedSince time.Time) {
	duration := time.Since(degradedSince)

	var levelName string
	switch level {
	case AlertLevelInfo:
		levelName = "info"
	case AlertLevelWarn:
		levelName = "warn"
	case AlertLevelError:
		levelName = "error"
	case AlertLevelCritical:
		levelName = "critical"
	default:
		levelName = "unknown"
	}

	m.logger.Warn("degraded mode alert",
		"level", levelName,
		"duration", duration,
		"degraded_since", degradedSince,
	)

	// Record metrics
	m.metrics.SetAlertLevel(int(level))
	m.metrics.IncrementAlertEmitted(levelName)

	// TODO: Add hook for degraded alerts when implementing Phase 2.5
}

// calculateAlertLevel determines the alert level based on degraded duration.
func (m *Manager) calculateAlertLevel(degradedSince time.Time) AlertLevel {
	duration := time.Since(degradedSince)

	if duration >= m.cfg.DegradedAlert.CriticalThreshold {
		return AlertLevelCritical
	}
	if duration >= m.cfg.DegradedAlert.ErrorThreshold {
		return AlertLevelError
	}
	if duration >= m.cfg.DegradedAlert.WarnThreshold {
		return AlertLevelWarn
	}
	if duration >= m.cfg.DegradedAlert.InfoThreshold {
		return AlertLevelInfo
	}

	return AlertLevelInfo - 1 // Below Info level
}
