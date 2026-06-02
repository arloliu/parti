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
	"github.com/arloliu/parti/v2/internal/recovery"
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
	// Cached CapabilityReporter view of consumerUpdater. Set once at
	// construction; nil if consumerUpdater is nil or doesn't implement
	// the interface. Avoids a per-Apply type assertion.
	capReporter CapabilityReporter

	// Handoff coordinator (feature-flagged); abstracts assignment application.
	handoffCoordinator handoff.Coordinator
	handoffMetrics     HandoffMetricsRecorder
	// handoffStore is the claim store backing the two-phase coordinator. It is
	// wired during Start (setupHandoff) when the handoff KV bucket is created;
	// nil when two-phase handoff is disabled. The removal guard reads it to
	// decide whether a transfer has committed elsewhere.
	handoffStore handoff.ClaimStore

	// commitBatchCache memoizes the partition set of the current "_commit"
	// entry, keyed by (commit version, _commit KV revision), so the removal
	// guard does not re-fan-out per-payload KV reads on every apply-retry tick.
	// Guarded by commitBatchMu (independent of applyStoreMu; lock order is
	// applyStoreMu -> commitBatchMu, never the reverse).
	commitBatchMu    sync.Mutex
	commitBatchCache *commitBatchEntry

	// testHookCommitRead, when non-nil, replaces the live "_commit" KV read in
	// currentCommitPartitionSet. Test-only seam; nil in production.
	testHookCommitRead func(ctx context.Context) (*types.AssignmentCommit, uint64, error)
	// testHookCommitBatch, when non-nil, replaces the per-payload fan-out in
	// currentCommitPartitionSet (called only on a cache miss). Test-only seam
	// for asserting fetch-once cache behavior; nil in production.
	testHookCommitBatch func(ctx context.Context, commit *types.AssignmentCommit) (map[string]struct{}, error)

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

	// bucketEpochs caches each Parti-owned bucket's JetStream Created
	// timestamp at Start. monitorBucketEpochs periodically re-reads the
	// live timestamp and trips degraded mode on a mismatch (bucket
	// wipe-and-recreate detection). The map is populated only during
	// Start and read-only afterwards, so the monitor goroutine needs
	// no synchronization.
	bucketEpochs map[string]bucketEpoch

	// State management
	state        atomic.Int32  // State
	workerID     atomic.Value  // string
	isLeader     atomic.Bool   // leadership status
	assignment   atomic.Value  // Assignment
	capabilities atomic.Uint32 // capability bitmask; see types.CapXxx constants

	// capProcessingGateWarned guards the one-shot WARN that fires when
	// EnableTwoPhaseHandoff is true and the consumer has not reported
	// CapProcessingGate by the time reportConsumerCapabilities runs.
	// A subsequent apply that wires the gate (operator rolled the
	// consumer config and re-deployed) does not re-trigger the warning;
	// a single log line at the first misconfigured apply is enough.
	capProcessingGateWarned atomic.Bool

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

	// committedAssignment holds the last assignment this worker successfully
	// applied+acked (a successful handoffCoordinator.Apply through
	// applyAssignmentWithPrevCore). Two roles: (1) the prev source for every
	// apply's prepare diff; (2) the source of truth for "is the current snapshot
	// assignment applied?" — compared on the full applied-ack identity (Version,
	// LeaderRevision, PartitionSetDigest, source-rev-when-known) by
	// currentAssignmentApplied, used by the degraded-recovery guard. Starts as
	// the zero Assignment (empty@0): a never-applied worker diffs against empty →
	// writes the full set (subsuming the old F-D3 one-way bootstrap override),
	// and its identity matches a cold empty snapshot so a never-assigned worker
	// still exits recovery. Distinct from the snapshot (m.assignment): the
	// snapshot can be advanced past a failed apply by the recovery refresh's
	// monotonicStore; committedAssignment advances only on a successful apply.
	// Distinct from startupAssignmentApplied (readiness CAS).
	committedAssignment atomic.Pointer[Assignment]

	// startupAssignmentApplied latches true once the startup assignment path
	// has made this manager's local assignment visible and acked it. It is
	// separate from committedAssignment: empty-source startup has no claims
	// to commit but still must be allowed to reach StateStable after its
	// applied-empty ack.
	startupAssignmentApplied atomic.Bool

	// Degraded mode tracking
	degradedSince          atomic.Int64   // UnixNano when degraded mode entered; 0 = not degraded
	connMonitorOnce        sync.Once      // ensures single connection monitor goroutine
	postStableMonitorsOnce sync.Once      // ensures startPostStableMonitors fires exactly once
	startedAt              time.Time      // absolute wall-clock anchor for StartupTimeout budget; set in prepareStart
	startupWatchdogFired   atomic.Bool    // guards startStartupTimeoutWatchdog (one-shot)
	connDownSince          atomic.Int64   // UnixNano when connectivity lost; 0 = up
	connUpSince            atomic.Int64   // UnixNano when connectivity restored; 0 = none
	kvErrorCount           atomic.Int32   // count of KV errors in the current window (protected by mu)
	kvErrorWindow          []kvErrorEvent // recent KV errors, class-tagged (protected by mu)
	recoveryGraceStart     atomic.Int64   // UnixNano when recovery grace period started; 0 = not in grace
	inRecoveryGrace        atomic.Bool    // true during recovery grace period
	// lastDegradedReason holds the reason string of the active degrade. Written by
	// the WINNING enterDegraded immediately AFTER the degradedSince CAS (losers
	// never write it, so there is no loser-clobber) and cleared to "" by
	// exitDegraded BEFORE it clears degradedSince. The reason-scoped recovery gate
	// treats an empty reason as "this entry's reason is not yet observable" and
	// stays degraded that tick — closing the tiny post-CAS-pre-store window via the
	// degradedSince happens-before, without converting degradedSince to a record.
	// atomic.Value of string ("" when never degraded / between an exit and the next
	// entry's store).
	lastDegradedReason atomic.Value
	// lastHeartbeatSuccessAt is the UnixNano of the most recent successful
	// heartbeat Put (stamped unconditionally in recordKVHealthyOp, even while
	// degraded). The reason-scoped gate uses it to confirm the failing
	// connected-but-KV-unavailable op recovered AFTER we degraded. 0 = never.
	lastHeartbeatSuccessAt atomic.Int64
	// lastEnumerationSuccessAt is the UnixNano of the most recent healthy worker
	// enumeration (stamped via the calculator's OnEnumerationSuccess). The
	// reason-scoped recovery gate keys on it to confirm a heartbeat-enumeration
	// stall (NP-10) recovered before exiting Degraded. 0 = never.
	lastEnumerationSuccessAt atomic.Int64

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.RWMutex

	// applyStoreMu serializes the (stale-check, handoff Apply, snapshot
	// Store, LSR advance, heartbeat SetAppliedAssignment) critical section
	// across all callers — commit watcher (handleCommitValue → applyAssignment),
	// alias watcher (handleAssignmentEntry → applyAssignment), the
	// scheduleApplyRetry goroutine, and the operator recovery refresh path
	// (refreshAssignmentFromNATS → monotonicStore).
	//
	// Without serialization, three independent goroutines can each pass a
	// pre-Apply gate against a stale snapshot, run handoffCoordinator.Apply
	// concurrently, and Store in arbitrary order — losing the W15 cross-leader
	// race and the W16 apply-retry race. See PR-2 spec
	// docs/plans/worker-state-hardening/02-pr2-spec.md.
	//
	// Lock contract: applyStoreMu and m.mu are NEVER held together.
	// Callers must not hold m.mu when entering the apply pipeline (and vice
	// versa). The two locks protect disjoint state.
	applyStoreMu sync.Mutex

	// testHookAfterApplyStore, when non-nil, is invoked synchronously after
	// applyAssignmentWithPrev's m.assignment.Store(newAssignment) returns.
	// Set ONLY by tests in this package to assert the LSR-before-Store
	// ordering invariant (v3 review P0). Production code MUST NOT set this
	// field; it is nil-default. See TestApplyAssignment_LSRAdvancesBeforeSnapshotStore.
	//
	// Concurrency contract (PR-2): the hook runs INSIDE applyStoreMu. It
	// MUST NOT call applyAssignment, applyAssignmentWithPrev,
	// refreshAssignmentFromNATS, or any other method that acquires
	// applyStoreMu — doing so would self-deadlock. Lock-free reads of
	// m.assignment / m.lastSeenLeaderRevision are safe.
	testHookAfterApplyStore func(Assignment)

	// testHookApplyJittered, when non-nil, is invoked synchronously inside
	// applyAssignmentWithPrev before the jitter sleep (only when
	// ApplyStartJitter > 0). applyAssignmentWithPrevSkipJitter (the retry
	// path) does NOT invoke this hook. Set ONLY by same-package tests before
	// the relevant goroutine starts; production MUST leave this nil.
	// See TestApplyAssignmentRetry_DoesNotJitter.
	testHookApplyJittered func()

	// applyJitterSampler, when non-nil, overrides ApplyStartJitter's random
	// sampling. Set ONLY by same-package tests before the relevant goroutine
	// starts; production MUST leave this nil.
	applyJitterSampler func(maxJitter time.Duration) time.Duration

	// testHookHandleAssignment, when non-nil, is invoked instead of
	// handleAssignmentEntry from runAssignmentWatchSession's debounce-fire
	// and channel-close-flush branches. Set ONLY by tests in this package
	// to assert idle-window debounce semantics without booting NATS.
	// Production code MUST NOT set this field; it is nil-default. See
	// TestAssignmentWatcher_DebouncesMultiVersionBurst.
	//
	// Concurrency contract: tests must set this field BEFORE spawning the
	// runAssignmentWatchSession goroutine and MUST NOT mutate it
	// afterwards. The session reads the field from a single goroutine.
	testHookHandleAssignment func(workerID string, entry jetstream.KeyValueEntry)

	// testHookHandleCommitValue, when non-nil, is invoked instead of
	// handleCommitValue from runCommitWatchSession's stage/flush paths
	// (no-debounce immediate dispatch, force-flush on partition-set
	// change, timer flush, and watcher-close flush). Set ONLY by tests
	// in this package to assert commit debounce semantics without
	// scaffolding a real assignment payload KV. Production code MUST
	// NOT set this field. Same concurrency contract as
	// testHookHandleAssignment: set before spawning runCommitWatchSession.
	testHookHandleCommitValue func(commit *types.AssignmentCommit)
}

// assignmentCalculator defines the interface for partition assignment calculation.
// Implemented by internal/assignment.Calculator and NopCalculator.
type assignmentCalculator interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	SubscribeToStateChanges() (<-chan types.CalculatorState, func())
	TriggerRebalance(ctx context.Context) error
	// GetState returns the calculator's current state. Used by
	// monitorCalculatorState's periodic reconcile to recover from dropped
	// subscriber events (the buffer is fixed-size and trySend drops on full).
	GetState() types.CalculatorState
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

// releaseLeadershipTimeout bounds the ReleaseLeadership call in Stop so that
// a slow KV cannot delay shutdown past the election TTL. Must be less than the
// election bucket's TTL (default 10s). Test-overridable.
var releaseLeadershipTimeout = 2 * time.Second

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
		capReporter:     asCapabilityReporter(options.consumerUpdater),
		kvErrorWindow:   make([]kvErrorEvent, 0, cfg.DegradedBehavior.KVErrorThreshold),
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

// Start runs the manager's synchronous sanity-check phase: claim a stable
// worker ID, ensure required KV buckets exist, participate in election,
// start the heartbeat publisher, and start the calculator if elected
// leader. On success it transitions the manager to StateWaitingAssignment
// and spawns a background runner that attempts to fetch the initial
// assignment and apply it via the unified pipeline; on apply success the
// runner CAS-transitions to StateStable. A soft watchdog enters
// StateDegraded ("startup-timeout") if StartupTimeout (measured from
// Start invocation) elapses without reaching StateStable, signaling the
// readiness probe to rotate the pod.
//
// Start returns once the sanity-check phase succeeds. The state observed
// after Start may be WaitingAssignment, Stable, or any calculator-driven
// active state (Scaling, Rebalancing, Emergency) depending on race.
// Callers that need to know the manager is ready to process work should
// call mgr.WaitState(StateStable, timeout).
//
// The background runner is best-effort: on assignment-fetch or apply
// failure it logs and continues to monitor startup. Existing recovery
// mechanisms handle subsequent retries — the assignment watcher redelivers
// when the leader publishes; applyAssignmentWithPrev's scheduleApplyRetry
// re-attempts on apply failure; monitorNATSConnection drives
// attemptRecoveryFromDegraded on reconnect.
//
// Apply boundedness: applyInitialAssignment internally calls
// handoffCoordinator.Apply(m.ctx, ...) which is unbounded per attempt
// (identical to pre-refactor Start). A stuck consumer updater can block
// the runner inside one apply attempt until Stop. The soft watchdog still
// fires enterDegraded("startup-timeout") in that case for probe-driven
// rotation.
//
// Start returns an error only for synchronous-phase failures (bucket
// creation, ID claim, election RPC). Auto-cleanup invokes Stop on a
// non-nil error so callers do not need to call Stop after a failed Start.
//
// Parameters:
//   - ctx: Context for startup-phase timeout control (not manager lifetime)
//
// Returns:
//   - error: Synchronous-phase startup error or context cancellation
//
// Example usage:
//
//	mgr := parti.NewManager(cfg, js, source, strategy)
//	if err := mgr.Start(ctx); err != nil {
//	    return err // no need to call Stop
//	}
//	if err := <-mgr.WaitState(parti.StateStable, 30*time.Second); err != nil {
//	    return err
//	}
func (m *Manager) Start(ctx context.Context) (startErr error) {
	// Prepare context and startup deadline
	startupCtx, cancel, err := m.prepareStart(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	// Warn when the audit grace is shorter than the claim resolver's default
	// reconcile cadence. After a silent watcher stall (e.g., NATS server
	// restart, which the nats.go KV watcher does NOT surface as an Updates()
	// close), the worker's resolver cache only re-syncs on the next reconcile
	// tick — by default 30s. If the leader-side audit grace
	// (5 × HeartbeatTTL) is shorter, the audit can escalate audit_repair
	// while the worker's cache is still stale. The fix is to tune
	// consumer.ResolverConfig.ReconcileInterval at the consumer config layer.
	m.warnOnShortAuditGrace()
	// Warn when OperationTimeout could let a single slow renew consume
	// the lease's three-attempt budget.
	warnOnOperationTimeoutVsElection(m.cfg, m.logger)

	// Warn when the caller-owned nats.Conn is configured with a finite
	// MaxReconnect budget. A finite budget turns a sustained outage into
	// a permanently CLOSED zombie connection (the manager's connection
	// monitor then enters degraded mode and the readiness probe rotates
	// the pod). The recommended posture is nats.MaxReconnects(-1) — see
	// docs/OPERATIONS.md "NATS Client Connection".
	if m.js != nil {
		warnOnFiniteMaxReconnects(m.js.Conn(), m.logger)
	}

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
	coreKV, err := m.ensureCoreKVBuckets(startupCtx, js)
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
	m.assignmentKV = coreKV.assignment
	m.heartbeatKV = coreKV.heartbeat
	m.logger.Info("startup: KV buckets ready")

	// Step 2: Participate in leader election
	m.transitionState(StateElection)
	m.logger.Info("startup: participating in election")
	if err := m.participateElection(startupCtx, coreKV.election); err != nil {
		return fmt.Errorf("failed to participate in election: %w", err)
	}
	m.logger.Info("startup: election stage complete", "is_leader", m.IsLeader())

	// Step 3: Start heartbeat publisher
	if err := m.startHeartbeat(coreKV.heartbeat); err != nil {
		return fmt.Errorf("failed to start heartbeat: %w", err)
	}

	// Step 4: If leader, start calculator
	if m.IsLeader() {
		if err := m.startCalculator(coreKV.assignment, coreKV.heartbeat); err != nil {
			return fmt.Errorf("failed to start calculator: %w", err)
		}
	}

	// Step 5: Hand off the assignment-wait + initial apply to the
	// background runner. Start returns once the synchronous sanity
	// checks (claim, election, heartbeat, calculator) are wired. The
	// background runner attempts one initial wait + apply on a best-
	// effort basis; existing recovery mechanisms drive any retries
	// (monitorAssignmentChanges watcher redelivery, scheduleApplyRetry,
	// monitorNATSConnection → attemptRecoveryFromDegraded). The
	// startStartupTimeoutWatchdog goroutine fires
	// enterDegraded("startup-timeout") once if the manager is still in
	// WaitingAssignment after StartupTimeout from m.startedAt —
	// providing a probe-rotation signal independent of whether the
	// runner is blocked inside apply.
	//
	// Callers that need to know the manager is ready to process work
	// should call mgr.WaitState(StateStable, timeout).
	//
	// The runner preserves the pre-refactor invariant "Apply→Store→Ack
	// BEFORE StateStable" by ordering apply before CAS. Monitor goroutines
	// start unconditionally after the apply attempt so they are present
	// for subsequent recovery whether or not the initial apply succeeded.
	m.transitionState(StateWaitingAssignment)
	m.logger.Info("startup: sanity checks done; background runner taking over for initial apply")
	m.startStartupTimeoutWatchdog()
	m.wg.Go(func() { m.runStartupBackground(coreKV.assignment) })

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
		// Record the applied-empty assignment as committed too, so the
		// "committedAssignment = last applied+acked" invariant holds by
		// construction at this ack site (not by zero-value coincidence). The
		// degraded-recovery guard then exits for a cold-empty worker because the
		// snapshot and committed identities match.
		m.committedAssignment.Store(&empty)
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

		// Cold-start empty path also completes startup. Without this,
		// a worker that boots before the leader publishes any
		// partitions would receive an empty assignment, ack it, and
		// stay in WaitingAssignment until a later non-empty assignment
		// triggers applyAssignmentWithPrev's CAS — which may never
		// happen if the source is genuinely empty.
		m.markStartupAssignmentApplied()

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

	// Detach the stream-missing observer installed during prepareStart.
	// The observer closure routes to m.logError + m.enterDegraded; once
	// Stop is awaiting m.wg.Wait, a late stream-missing exhaustion from
	// a Dynamic that outlives this manager must not enqueue new work
	// into the wait group. The closure itself also short-circuits on
	// StateShutdown (see manager_setup.go:onStreamMissingError); this
	// clear is the primary remove, the closure guard the belt-and-braces.
	if obs, ok := m.consumerUpdater.(recovery.StreamMissingObserver); ok {
		obs.SetOnStreamMissingError(nil)
	}

	// Cancel manager context to stop all background goroutines
	// This will cause monitorAssignmentChanges watcher to close
	m.cancel()

	// Note: Keep m.ctx (even though cancelled) instead of setting to nil
	// so background goroutines can still use it in their select statements
	m.mu.Unlock()

	// Shutdown sequence (reverse of startup)
	var shutdownErr error

	// Release leadership before any slow work. m.ctx is already cancelled so
	// use the caller's ctx as parent. The 2s bound keeps Stop well under the
	// election TTL even when the KV is sluggish.
	// ErrNotLeader is benign — it means we were never the leader or already lost it.
	releaseCtx, releaseCancel := context.WithTimeout(ctx, releaseLeadershipTimeout)
	if err := m.election.ReleaseLeadership(releaseCtx); err != nil &&
		!errors.Is(err, election.ErrNotLeader) {
		m.logError("failed to release leadership on stop", "error", err)
		shutdownErr = fmt.Errorf("leadership release failed: %w", err)
	}
	releaseCancel()

	// Step 1: Stop calculator if running (leader only)
	if stopped := m.stopCalculator(); stopped {
		m.logger.Info("calculator stopped", "worker_id", m.WorkerID())
	} else {
		m.logger.Debug("calculator stop skipped: not running", "worker_id", m.WorkerID())
	}

	// Step 1.5: the degraded-mode connection monitor stops via m.cancel() above
	// (it selects on m.ctx.Done()); no separate stop signal is needed.

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

	// Step 3: Release stable worker ID (ignore ErrNotClaimed)
	if err := m.idClaimer.Release(ctx); err != nil && !errors.Is(err, stableid.ErrNotClaimed) {
		m.logError("failed to release worker ID", "error", err)
		if shutdownErr == nil {
			shutdownErr = fmt.Errorf("worker ID release failed: %w", err)
		}
	}

	// Step 4: Wait for all background goroutines with timeout
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
//   - The consumer/updater reports CapProcessingGate via the optional
//     [CapabilityReporter] interface; Manager samples it through
//     reportConsumerCapabilities after every handoffCoordinator.Apply
//     attempt and ORs the bit in here. The reporter pathway never
//     clears bits; only other components may call SetCapability with
//     active=false to clear.
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

// handoffClaimStore returns the manager's handoff claim store and whether it is
// wired. It is non-nil only when two-phase handoff is enabled and setupHandoff
// has run. The removal guard treats a nil store as "no proof available" and
// allows removals (no two-phase coordination in effect).
func (m *Manager) handoffClaimStore() (handoff.ClaimStore, bool) {
	if m.handoffStore == nil {
		return nil, false
	}
	return m.handoffStore, true
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

// reportableCapBits is the set of capability bits that may be sampled
// from a CapabilityReporter and ORed into Manager.capabilities. Add new
// runtime-wireup bits here explicitly.
const reportableCapBits = types.CapProcessingGate

// asCapabilityReporter returns u as a CapabilityReporter when it implements
// the interface; otherwise nil. Called once at construction so the apply
// hot path doesn't repeat the type assertion.
func asCapabilityReporter(u WorkerConsumerUpdater) CapabilityReporter {
	cr, _ := u.(CapabilityReporter)
	return cr
}

// reportConsumerCapabilities samples the cached CapabilityReporter (if any)
// and ORs reported runtime-wireup bits into the Manager's capability bitmask.
//
// Called after every handoffCoordinator.Apply attempt — even on error —
// because the updater (which actually wraps handlers with the gate) may
// have succeeded before the apply pipeline failed in a later phase.
// Short-circuits once all reportable bits are already set.
func (m *Manager) reportConsumerCapabilities() {
	if m.capReporter == nil {
		return
	}
	missing := reportableCapBits &^ m.capabilities.Load()
	if missing == 0 {
		return
	}
	newBits := m.capReporter.Capabilities() & missing
	if newBits != 0 {
		m.capabilities.Or(newBits)
	}
	// Surface a misconfigured two-phase handoff: if EnableTwoPhaseHandoff
	// is on but the consumer never wired a processing gate, claims are
	// written and never consulted — effectively a silent disable.
	// Fires at most once per Manager lifetime (guarded below).
	m.maybeWarnMissingProcessingGate()
}

// maybeWarnMissingProcessingGate emits a one-shot WARN when
// EnableTwoPhaseHandoff is on but the consumer's reported capabilities
// do not include CapProcessingGate.
//
// Limitation: if the consumer updater does NOT implement
// CapabilityReporter (m.capReporter == nil), reportConsumerCapabilities
// returns early and never calls this helper. The warning depends on
// capability reporting; consumers that bypass it are responsible for
// their own gate-wiring verification.
func (m *Manager) maybeWarnMissingProcessingGate() {
	if !m.cfg.EnableTwoPhaseHandoff {
		return
	}
	if m.capProcessingGateWarned.Load() {
		return
	}
	if m.capabilities.Load()&types.CapProcessingGate != 0 {
		return // gate present post-merge; silent
	}
	if !m.capProcessingGateWarned.CompareAndSwap(false, true) {
		return // raced another goroutine; that one fired the warning
	}
	m.logger.Warn(
		"two-phase handoff is enabled but the consumer reports no processing gate; "+
			"partition claims are written and never consulted",
		"remedy", "wire a processing gate on the consumer (e.g. consumer.Dynamic) so claims fence delivery",
	)
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
