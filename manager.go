package parti

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/internal/assignment"
	"github.com/arloliu/parti/internal/assignment/handoff"
	"github.com/arloliu/parti/internal/election"
	"github.com/arloliu/parti/internal/heartbeat"
	"github.com/arloliu/parti/internal/hooks"
	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/internal/metrics"
	"github.com/arloliu/parti/internal/stableid"
	"github.com/arloliu/parti/types"
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

	// Handoff coordinator (feature-flagged); abstracts assignment application.
	handoffCoordinator handoff.Coordinator
	handoffMetrics     handoff.MetricsRecorder

	// Two-phase resume tracking
	// Populated at startup if we detect in-flight claims that require resumption.
	pendingHandoffResume atomic.Bool // indicates a resume scan should run after initial assignment

	// Internal components
	// These are initialized with Nop implementations in NewManager and are never nil.
	idClaimer  stableIDClaimer
	election   types.ElectionAgent
	heartbeat  heartbeatPublisher
	calculator assignmentCalculator

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

// assignmentCalculator defines the interface for partition assignment calculation.
// Implemented by internal/assignment.Calculator and NopCalculator.
type assignmentCalculator interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	SubscribeToStateChanges() (<-chan types.CalculatorState, func())
	TriggerRebalance(ctx context.Context) error
}

// heartbeatPublisher defines the interface for heartbeat publishing.
// Implemented by internal/heartbeat.Publisher and NopPublisher.
type heartbeatPublisher interface {
	Start(ctx context.Context) error
	Stop() error
}

// stableIDClaimer defines the interface for stable worker ID claiming.
// Implemented by internal/stableid.Claimer and NopClaimer.
type stableIDClaimer interface {
	Claim(ctx context.Context) (string, error)
	StartRenewal() error
	Release(ctx context.Context) error
	Close()
	WorkerID() string
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
// Internal components (calculator, heartbeat, election, claimer) are initialized
// with NoOp implementations, ensuring they are never nil.
//
// Parameters:
//   - cfg: Configuration for the manager
//   - js: JetStream context for NATS interaction
//   - source: Source of partitions to distribute
//   - strategy: Strategy for assigning partitions to workers
//   - opts: Optional configuration options
//
// Returns:
//   - *Manager: Initialized manager instance
//   - error: Validation error if configuration is invalid
//
// Example:
//
//	cfg := parti.Config{WorkerIDPrefix: "worker", WorkerIDMax: 999}
//	src := source.NewStatic(partitions)
//	curStrategy := strategy.NewConsistentHash()
//	js, _ := jetstream.New(natsConn)
//	mgr, err := parti.NewManager(&cfg, js, src, curStrategy)
//	if err != nil {
//	    log.Fatal(err)
//	}
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
	logger := options.logger
	if logger == nil {
		logger = logging.NewNop()
	}
	cfg.ValidateWithWarnings(logger)
	hooksInstance := options.hooks
	if hooksInstance == nil {
		nopHooks := hooks.NewNop()
		hooksInstance = &nopHooks
	}

	// Initialize internal components with Nop implementations
	// This ensures that these fields are never nil, simplifying lifecycle management
	// and avoiding nil pointer checks in operational methods.
	m := &Manager{
		cfg:             *cfg,
		js:              js,
		source:          source,
		strategy:        strategy,
		electionAgent:   options.electionAgent,
		hooks:           hooksInstance,
		metrics:         metricsCollector,
		logger:          logger,
		consumerUpdater: options.consumerUpdater,
		connMonitorStop: make(chan struct{}),
		kvErrorWindow:   make([]time.Time, 0, cfg.DegradedBehavior.KVErrorThreshold),
		// Initialize internal components with Nop implementations
		idClaimer:  stableid.NewNop(),
		election:   election.NewNopElection(),
		heartbeat:  heartbeat.NewNop(),
		calculator: assignment.NewNopCalculator(),
	}
	m.state.Store(int32(StateInit))
	m.workerID.Store("")
	m.assignment.Store(Assignment{})

	// Initialize handoff coordinator; claim store wired later during Start when KV buckets are created.
	hm := options.handoffMetrics
	if hm == nil {
		hm = handoff.NopMetrics{}
	}
	m.handoffMetrics = hm
	m.handoffCoordinator = handoff.New(
		handoff.Config{
			ConsumerUpdater: m.consumerUpdater,
			Metrics:         m.handoffMetrics,
			Logger:          m.logger,
		},
		cfg.EnableTwoPhaseHandoff,
	)

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
// Parameters:
//   - ctx: Context for startup timeout control (not manager lifetime)
//
// Returns:
//   - error: Startup error or context cancellation
//
// Example usage:
//
//	mgr := parti.NewManager(cfg, js, source, strategy)
//	if err := mgr.Start(ctx); err != nil {
//	    // Cleanup on startup failure
//	    _ = mgr.Stop(context.Background())
//	    return err
//	}
func (m *Manager) Start(ctx context.Context) error {
	// Prepare context and startup deadline
	startupCtx, cancel, err := m.prepareStart(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	// Use injected JetStream context (already constructed by caller)
	js := m.js
	if js == nil {
		return errors.New("jetstream context not initialized")
	}

	// Step 1: Claim stable worker ID early
	stableIDKV, err := m.ensureStableIDKV(startupCtx, js)
	if err != nil {
		return err
	}
	m.logger.Info("startup: claiming stable worker ID")
	m.transitionState(m.State(), StateClaimingID)
	if err := m.claimWorkerID(startupCtx, stableIDKV); err != nil {
		return fmt.Errorf("failed to claim worker ID: %w", err)
	}
	m.logger.Info("startup: claimed stable worker ID", "worker_id", m.WorkerID())

	// Step 1.2: Start partition source
	if err := m.source.Start(startupCtx); err != nil {
		return fmt.Errorf("failed to start partition source: %w", err)
	}

	// Step 1.5: Ensure coordination buckets (election/heartbeat/assignment)
	electionKV, heartbeatKV, assignmentKV, err := m.ensureCoreKVBuckets(startupCtx, js)
	if err != nil {
		return err
	}

	// Step 1.6: Optional handoff setup (two-phase)
	if m.cfg.EnableTwoPhaseHandoff {
		if err := m.setupHandoff(startupCtx, js); err != nil {
			return err
		}
	}

	// Store KV buckets for later use
	m.assignmentKV = assignmentKV
	m.heartbeatKV = heartbeatKV
	m.logger.Info("startup: KV buckets ready")

	// Step 2: Participate in leader election
	m.transitionState(m.State(), StateElection)
	m.logger.Info("startup: participating in election")
	if err := m.participateElection(startupCtx, electionKV); err != nil {
		return fmt.Errorf("failed to participate in election: %w", err)
	}
	m.logger.Info("startup: election stage complete", "is_leader", m.IsLeader())

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
	m.logger.Info("startup: waiting for assignment")
	waitCtx, waitCancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer waitCancel()
	if err := m.waitForAssignment(waitCtx, assignmentKV, heartbeatKV); err != nil {
		return fmt.Errorf("failed to get assignment: %w", err)
	}
	m.logger.Info("startup: initial assignment received")

	// Emit initial assignment events and apply handoff
	m.emitInitialAssignmentEvents()
	m.applyInitialHandoffAsync()

	// Step 6: Transition to stable state
	m.transitionState(m.State(), StateStable)

	// Start background workers
	m.wg.Go(func() { m.monitorAssignmentChanges(m.ctx, assignmentKV) })
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
	if stopped := m.stopCalculator(); stopped {
		m.logger.Info("calculator stopped", "worker_id", m.WorkerID())
	} else {
		m.logger.Debug("calculator stop skipped: not running", "worker_id", m.WorkerID())
	}

	// Step 1.5: Stop degraded mode connection monitor
	select {
	case m.connMonitorStop <- struct{}{}:
	default:
		// Channel already closed or monitor not running
	}

	// Step 1.6: Stop partition source
	if err := m.source.Stop(ctx); err != nil {
		m.logError("failed to stop partition source", "error", err)
		shutdownErr = fmt.Errorf("partition source stop failed: %w", err)
	}

	// Step 2: Stop heartbeat publisher (ignore ErrNotStarted)
	if err := m.heartbeat.Stop(); err != nil && !errors.Is(err, heartbeat.ErrNotStarted) {
		m.logError("failed to stop heartbeat", "error", err)
		if shutdownErr == nil {
			shutdownErr = fmt.Errorf("heartbeat stop failed: %w", err)
		}
	}

	// Step 3: Release election if we hold leadership
	if m.IsLeader() {
		if err := m.election.ReleaseLeadership(ctx); err != nil {
			m.logError("failed to release leadership", "error", err)
			if shutdownErr == nil {
				shutdownErr = fmt.Errorf("leadership release failed: %w", err)
			}
		}
	}

	// Step 4: Release stable worker ID (ignore ErrNotClaimed)
	if err := m.idClaimer.Release(ctx); err != nil && !errors.Is(err, stableid.ErrNotClaimed) {
		m.logError("failed to release worker ID", "error", err)
		if shutdownErr == nil {
			shutdownErr = fmt.Errorf("worker ID release failed: %w", err)
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
		m.logger.Warn("manager stop timed out")
		if shutdownErr == nil {
			shutdownErr = ctx.Err()
		}
		return shutdownErr
	}
}

// WorkerID returns the stable worker ID claimed by this manager.
//
// Returns:
//   - string: The worker ID (e.g., "worker-0"), or empty string if not yet claimed.
func (m *Manager) WorkerID() string {
	if v := m.workerID.Load(); v != nil {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}

// IsLeader returns whether this manager is currently the leader.
//
// Returns:
//   - bool: true if this manager is the leader, false otherwise.
func (m *Manager) IsLeader() bool {
	return m.isLeader.Load()
}

// CurrentAssignment returns the current partition assignment for this worker.
//
// Returns:
//   - Assignment: The current assignment. Returns empty assignment if none received.
func (m *Manager) CurrentAssignment() Assignment {
	if v := m.assignment.Load(); v != nil {
		if a, ok := v.(Assignment); ok {
			return a
		}
	}
	return Assignment{}
}

// State returns the current state of the manager.
//
// Returns:
//   - State: The current state (e.g., StateInit, StateStable).
func (m *Manager) State() State {
	return State(m.state.Load())
}

// logError logs an error with consistent formatting and invokes OnError hook.
func (m *Manager) logError(msg string, keysAndValues ...any) {
	m.logger.Error(msg, keysAndValues...)

	// Invoke OnError hook if configured
	if m.hooks != nil && m.hooks.OnError != nil {
		// Find the error in keysAndValues
		var err error
		for _, v := range keysAndValues {
			if e, ok := v.(error); ok {
				err = e
				break
			}
		}

		if err != nil {
			// Use manager context if available, otherwise background
			// Note: m.ctx is set in Start() and safe to read here as background
			// goroutines are only started after m.ctx is initialized.
			ctx := m.ctx
			if ctx == nil {
				ctx = context.Background()
			}

			// Run hook asynchronously to avoid blocking
			go func() {
				if hookErr := m.hooks.OnError(ctx, err); hookErr != nil {
					m.logger.Warn("hook_error", "hook", "OnError", "error", hookErr)
				}
			}()
		}
	}
}
