package parti

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment"
	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/election"
	"github.com/arloliu/parti/v2/internal/heartbeat"
	"github.com/arloliu/parti/v2/internal/hooks"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/stableid"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/types"
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
	handoffMetrics     HandoffMetricsRecorder

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
	state        atomic.Int32  // State
	workerID     atomic.Value  // string
	isLeader     atomic.Bool   // leadership status
	assignment   atomic.Value  // Assignment
	capabilities atomic.Uint32 // capability bitmask; see types.CapXxx constants

	// Phase 4 commit-path state machine state. Read by both watchers and the
	// dual-read selector. All four are atomic-only; no mutex needed.

	// lastSeenLeaderRevision is the highest LeaderRevision this worker has
	// observed and accepted from either the commit watcher (case (c)/(d)
	// success) or the legacy alias path. Stale-leader fences read this;
	// successful state-machine actions update it via
	// updateLastSeenLeaderRevision (monotone, CAS-loop).
	lastSeenLeaderRevision atomic.Uint64

	// lastSeenAlias is the most-recent decoded legacy-alias Assignment
	// observed by handleAssignmentEntry. Consulted by handleCommitValue's
	// dual-read selector (§3.6 case 2 — legacy alias wins over commit when
	// legacy.LeaderRevision > commit.LeaderRevision).
	lastSeenAlias atomic.Pointer[Assignment]

	// lastObservedCommit is the most-recent decoded AssignmentCommit
	// observed by monitorCommitChanges. Consulted by handleAssignmentEntry's
	// dual-read selector and by the initial-bootstrap path so the manager
	// can re-route an initial assignment through the commit path when a
	// commit is already visible.
	lastObservedCommit atomic.Pointer[types.AssignmentCommit]

	// pendingApplyInFlight + stashedCommit implement §3.6 case (e)
	// coalescing. When a commit-path apply is in flight, additional commits
	// stash the highest-Version target. After the apply completes, the
	// stashed commit is re-routed through handleCommitValue.
	pendingApplyInFlight atomic.Bool
	stashedCommit        atomic.Pointer[types.AssignmentCommit]

	// stashedApplyRetry holds the most recent failed-apply target for the
	// scheduleApplyRetry coalescing path. Independent of stashedCommit so a
	// failed apply does not contend with an arriving commit.
	stashedApplyRetry atomic.Pointer[Assignment]
	applyRetryActive  atomic.Bool

	// Degraded mode tracking
	degradedSince      atomic.Int64  // UnixNano when degraded mode entered; 0 = not degraded
	lastAssignmentAt   atomic.Int64  // UnixNano of last successful assignment fetch; 0 = never
	lastAssignment     atomic.Value  // []Partition - cached assignment during degraded
	connMonitorOnce    sync.Once     // ensures single connection monitor goroutine
	connMonitorStop    chan struct{} // channel to stop connection monitor
	connDownSince      atomic.Int64  // UnixNano when connectivity lost; 0 = up
	connUpSince        atomic.Int64  // UnixNano when connectivity restored; 0 = none
	kvErrorCount       atomic.Int32  // consecutive KV error count
	kvErrorWindow      []time.Time   // timestamps of recent KV errors (protected by mu)
	recoveryGraceStart atomic.Int64  // UnixNano when recovery grace period started; 0 = not in grace
	inRecoveryGrace    atomic.Bool   // true during recovery grace period

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.RWMutex

	// testHookAfterApplyStore, when non-nil, is invoked synchronously after
	// applyAssignmentWithPrev's m.assignment.Store(newAssignment) returns.
	// Set ONLY by tests in this package to assert the LSR-before-Store
	// ordering invariant (v3 review P0). Production code MUST NOT set this
	// field; it is nil-default. See TestApplyAssignment_LSRAdvancesBeforeSnapshotStore.
	testHookAfterApplyStore func(Assignment)
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
//
// SetAppliedAssignment + PublishNow expose the Phase 2 / Phase 4 ack path:
// the manager calls them after a successful applyAssignment so the leader
// observes the new applied state within one heartbeat round-trip instead of
// waiting up to HeartbeatInterval.
//
// Implemented by internal/heartbeat.Publisher and heartbeat.NopPublisher.
type heartbeatPublisher interface {
	Start(ctx context.Context) error
	Stop() error
	SetAppliedAssignment(snap heartbeat.AppliedAssignment)
	PublishNow(ctx context.Context) error
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
	if err := SetDefaults(cfg); err != nil {
		return nil, fmt.Errorf("failed to apply config defaults: %w", err)
	}
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
		hm = types.NopHandoffMetricsRecorder{}
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
// If Start returns an error, all partially-acquired resources (KV leases,
// background goroutines, election state) are automatically cleaned up.
// The caller does NOT need to call Stop after a failed Start.
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
//	    return err // no need to call Stop
//	}
func (m *Manager) Start(ctx context.Context) (startErr error) {
	// Prepare context and startup deadline
	startupCtx, cancel, err := m.prepareStart(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	// Auto-cleanup on startup failure: release any partially-acquired resources
	// so callers do not need to call Stop after a failed Start.
	defer func() {
		if startErr != nil {
			shutdownTimeout := m.cfg.ShutdownTimeout
			if shutdownTimeout <= 0 {
				shutdownTimeout = 10 * time.Second
			}
			stopCtx, stopCancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer stopCancel()
			_ = m.Stop(stopCtx)
		}
	}()

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
	m.transitionState(StateClaimingID)
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
	// Start background maintenance (periodic claim sweep for two-phase; no-op for direct).
	m.handoffCoordinator.Start(m.ctx)
	// Report CapTwoPhaseHandoff only after the coordinator is successfully wired
	// and started. The manager is one-shot (Stop transitions to StateShutdown with
	// no restart path), so there is no need to clear this bit on Stop.
	if m.cfg.EnableTwoPhaseHandoff {
		m.SetCapability(types.CapTwoPhaseHandoff, true)
	}

	// Store KV buckets for later use
	m.assignmentKV = assignmentKV
	m.heartbeatKV = heartbeatKV
	m.logger.Info("startup: KV buckets ready")

	// Step 2: Participate in leader election
	m.transitionState(StateElection)
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

	// Step 5: Wait for assignment.
	//
	// waitForAssignment stores a candidate Assignment into m.assignment via
	// m.assignment.Store(*curAssignment). For Phase 4 we treat that store
	// as observational only — the real Apply→Store→Ack pipeline runs in
	// Step 5.5 below, gated on whether a commit is already visible.
	m.transitionState(StateWaitingAssignment)
	m.logger.Info("startup: waiting for assignment")
	if err := m.waitForAssignment(startupCtx, assignmentKV, heartbeatKV); err != nil {
		return fmt.Errorf("failed to get assignment: %w", err)
	}
	m.logger.Info("startup: initial assignment received")

	// Step 5.5: Apply the initial assignment via the unified pipeline (§4.4).
	// Must complete (Apply → Store → Ack) BEFORE we transition to StateStable
	// so the worker does not report AppliedVersion=0 while claiming stable.
	//
	// Prefer the commit-path route when a commit is already visible in KV:
	// re-routing through handleCommitValue populates SourceRevision /
	// SourceRevisionKnown correctly (the legacy alias envelope does not
	// carry those). When no commit exists yet (cold-start path against an
	// empty assignment bucket), apply what waitForAssignment stored —
	// SourceRevisionKnown remains false, which is the documented "unknown"
	// signal.
	if err := m.applyInitialAssignment(startupCtx, assignmentKV); err != nil {
		return fmt.Errorf("initial apply failed: %w", err)
	}

	// Step 6: Transition to stable state only after initial apply + ack.
	m.transitionState(StateStable)

	// Start background workers.
	// monitorCommitChanges is the primary path for CapAckV1 workers (§3.6
	// case 1). monitorAssignmentChanges runs concurrently for rolling-upgrade
	// compatibility (§3.6 case 2 — legacy alias path with leader fence).
	// The dual-read selector in handleCommitValue / handleAssignmentEntry
	// resolves which one's payload to apply on any given event.
	m.wg.Go(func() { m.monitorCommitChanges(m.ctx, assignmentKV) })
	m.wg.Go(func() { m.monitorAssignmentChanges(m.ctx, assignmentKV) })
	m.monitorNATSConnection()

	return nil
}

// applyInitialAssignment runs the apply-then-store-then-ack pipeline for the
// initial assignment surfaced by waitForAssignment. It prefers the
// commit-path route when "assignment._commit" already exists so
// SourceRevision flows correctly. Otherwise it applies the
// legacy-alias-derived assignment directly (which carries zero
// SourceRevision/SourceRevisionKnown by design — the legacy envelope does
// not encode them).
//
// Returns an error only on Apply failure; the caller's startup ctx
// governs how long we wait for retries.
func (m *Manager) applyInitialAssignment(ctx context.Context, assignmentKV jetstream.KeyValue) error {
	initial := m.CurrentAssignment()

	// Try to route through the commit path first. A successful
	// handleCommitValue applies, stores, and acks via applyAssignmentWithPrev
	// passing an explicit empty `previous` argument (set below via the
	// initialBootstrap branch of buildAssignmentFromCommit-driven apply).
	commit, _, gerr := kvutil.GetJSON[types.AssignmentCommit](ctx, assignmentKV, "assignment._commit")
	if gerr == nil && commit != nil && commit.Version >= initial.Version {
		// For the initial bootstrap, force previous=empty so the handoff
		// coordinator's prepare phase treats every partition as newly
		// acquired. We do this by re-using handleCommitValue's machinery
		// but routing via applyAssignmentWithPrev directly afterwards.
		newAsg, ok := m.buildAssignmentFromCommit(commit, m.WorkerID())
		if ok {
			// Commit-path success: run the apply with explicit empty previous
			// FIRST. Only advance lastObservedCommit after the apply returns
			// nil — on failure, the commit must not be surfaced to the
			// dual-read selector as an authoritative observation. LSR is
			// advanced by applyAssignmentWithPrev on success (single source
			// of truth — see applyAssignment Godoc). The watcher will
			// redeliver on the next tick if Apply failed.
			if err := m.applyAssignmentWithPrev(Assignment{}, newAsg); err != nil {
				return err
			}
			commitCopy := *commit
			m.lastObservedCommit.Store(&commitCopy)
			m.runInitialHandoffResumeIfPending()

			return nil
		}
		// Payload-verify failure fell through; fall back to applying
		// what waitForAssignment surfaced (legacy alias derived).
	}

	if initial.Version == 0 && len(initial.Partitions) == 0 {
		// Cold-start path: waitForAssignment surfaced an empty assignment
		// (no partitions yet and no commit). There is no Apply work to
		// perform — but the worker MUST still publish an explicit
		// applied-empty ack BEFORE transitioning to StateStable, otherwise
		// it would advertise AppliedVersion=0 with no leader-observable
		// receipt that the empty assignment was acknowledged (§4.4
		// invariant: every StateStable transition is preceded by an ack).
		//
		// Bypass applyAssignmentWithPrev / handoffCoordinator.Apply here:
		// the coordinator's Apply(empty, empty) would still invoke
		// UpdateWorkerConsumer with nil partitions (and, in two-phase
		// mode, emit phantom prepare/commit phase metrics for an
		// empty→empty transition). Direct SetAppliedAssignment +
		// PublishNow is faithful to the cold-empty intent.
		empty := Assignment{}
		m.assignment.Store(empty)
		m.heartbeat.SetAppliedAssignment(heartbeat.AppliedAssignment{
			LeaderRevision:        0,
			AppliedVersion:        0,
			AppliedDigest:         types.PartitionSetDigest(nil),
			AppliedSourceRevision: 0,
			AppliedSourceRevKnown: false,
			AppliedAt:             time.Now(),
		})
		if err := m.heartbeat.PublishNow(m.ctx); err != nil {
			m.logError("heartbeat publish-now after empty bootstrap failed", "error", err)
		}

		return nil
	}
	if err := m.applyAssignmentWithPrev(Assignment{}, initial); err != nil {
		return err
	}
	m.runInitialHandoffResumeIfPending()

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
	m.transitionState(StateShutdown)

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

	// Step 3: Release election leadership unconditionally.
	// Always attempt release to avoid TOCTOU race where leadership flag is cleared
	// by monitorLeadership between the check and the release call.
	// ErrNotLeader is benign — it simply means we weren't the leader.
	if err := m.election.ReleaseLeadership(ctx); err != nil &&
		!errors.Is(err, election.ErrNotLeader) {
		m.logError("failed to release leadership", "error", err)
		if shutdownErr == nil {
			shutdownErr = fmt.Errorf("leadership release failed: %w", err)
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
// The returned Assignment shares its Partitions backing array with the
// Manager's internal state. Callers MUST NOT modify the returned slice
// or its elements. If mutation is needed, make a copy first.
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

// SetCapability sets or clears a single capability bit in the manager's
// heartbeat capability bitmask.
//
// Called by the component that actually wires the corresponding safety
// mechanism — not by config-reading code. Examples:
//   - The two-phase handoff coordinator calls SetCapability(types.CapTwoPhaseHandoff, true)
//     after successfully starting, and (…, false) on Stop.
//   - The consumer/updater calls SetCapability(types.CapProcessingGate, true)
//     when it wraps handlers with the processing gate.
//   - The heartbeat publisher's CapAckV1 bit is set by startHeartbeat after
//     the publisher starts successfully.
//
// Parameters:
//   - capBit: Capability bit to set or clear (e.g., types.CapAckV1)
//   - active: true to set the bit, false to clear it
func (m *Manager) SetCapability(capBit uint32, active bool) {
	if active {
		m.capabilities.Or(capBit)
	} else {
		m.capabilities.And(^capBit)
	}
}

// Capabilities returns the current capability bitmask as an atomic snapshot.
//
// The heartbeat publisher calls this on every heartbeat composition to embed
// the current runtime wire-up state. Do not cache the result — always read
// via this method so the heartbeat reflects live state.
//
// Returns:
//   - uint32: Current capability bitmask (OR of active types.CapXxx constants)
func (m *Manager) Capabilities() uint32 {
	return m.capabilities.Load()
}

// invokeHook executes a hook function asynchronously with error logging.
// It handles nil checks, WaitGroup management, and error reporting.
func (m *Manager) invokeHook(name string, hook func() error) {
	if hook == nil {
		return
	}

	m.wg.Go(func() {
		if err := hook(); err != nil {
			m.logError(name+" hook error", "error", err)
		}
	})
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

			// Run hook asynchronously but tracked by WaitGroup so Stop waits for completion
			m.wg.Go(func() {
				if hookErr := m.hooks.OnError(ctx, err); hookErr != nil {
					m.logger.Warn("hook_error", "hook", "OnError", "error", hookErr)
				}
			})
		}
	}
}
