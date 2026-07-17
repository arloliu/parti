package assignment

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
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

// errSuspiciousPartitionObservation is returned by rebalance() when the
// partition-source snapshot is empty or has sharply shrunk relative to the
// last known credible count, and the confirmation window
// (PartitionShrinkConfirmationCount) has not yet been satisfied. The
// candidate rebalance is skipped — the cached assignment remains in
// effect. handleRebalance treats this sentinel like a no-op (no error)
// because it is an explicit "keep cached assignment" decision, not a
// failure.
//
// This implements the "minimum credible partition input" doctrine: a
// single observation of an empty / sharply-shrunk source is more likely
// a transient blip (KV miss, watcher race) than legitimate operator
// intent; the confirmation window prevents one bad poll from triggering
// a fleet-wide reassign-to-zero thundering herd.
var errSuspiciousPartitionObservation = errors.New("rebalance skipped: partition observation pending confirmation")

// errSuspiciousWorkerObservation is returned by rebalance() when the
// heartbeat-bucket scan returns a sharply-shrunk worker set with no
// emergency-confirmed deaths in c.disappearedWorkers, and the
// confirmation window (WorkerShrinkConfirmationCount) has not yet been
// satisfied. The candidate rebalance is skipped; the cached assignment
// remains in effect. handleRebalance treats this sentinel like a no-op
// (no error) because it is an explicit "keep cached assignment"
// decision, not a failure.
//
// This implements the "minimum credible worker input" doctrine: a
// single observation of a sharply-shrunk heartbeat scan is more likely
// a transient blip (KV truncated-read race, watcher hiccup) than a
// legitimate fleet-wide loss; the confirmation window plus the
// emergency-detector composition prevents one bad scan from
// reassigning every partition off live workers.
//
// The companion to the in-getActiveWorkers defense: that path degrades
// the observation to cached+fresh=false; this sentinel covers the
// rebalance-floor branch when a fresh observation arrives but
// EmergencyDetector has not yet confirmed deaths.
var errSuspiciousWorkerObservation = errors.New("rebalance skipped: worker observation pending confirmation")

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
	gc                *CommitGC          // best-effort payload reaper (§3.5 step 12)
	emergencyDetector *EmergencyDetector // Hysteresis-based emergency detection

	// Cached string patterns (for performance)
	assignmentKeyPrefix string // "AssignmentPrefix." - cached for key construction

	// State management
	started            atomic.Bool
	mu                 sync.RWMutex
	rebalanceMu        sync.Mutex // Serializes rebalance operations
	pollMu             sync.Mutex // Serializes the observe→decide→claim critical section
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

	// pendingPartitionUpdate records that a partition-source change was
	// observed by monitorPartitions but skipped because the leader was in
	// recovery grace. The drain ticker in monitorPartitions retries when
	// grace lifts (see §3.3 of docs/plans/assignment-correctness-fixes/01-pr1-spec.md).
	pendingPartitionUpdate atomic.Bool

	// auditDoneCh is closed by auditLoop on return. Tests can poll it to
	// confirm the audit goroutine has exited. wg.Wait() already covers the
	// shutdown sequence; this channel is purely an observation hook.
	auditDoneCh chan struct{}

	// partitionRebalanceEntries counts successful entries into
	// handlePartitionRebalance (incremented as the first line of the callback,
	// before c.rebalance runs). Test-only signal exposed via export_test.go;
	// the §2.2.3 / §10.3 "at most one extra cycle" invariant is asserted as a
	// counter delta equal to 1 per dropped partition update.
	partitionRebalanceEntries atomic.Int64

	// Minimum-credible-partition-input doctrine (see
	// errSuspiciousPartitionObservation Godoc):
	//
	// lastKnownPartitionCount is the most recent partition count the
	// calculator acted on. Empty / sharply-shrunk observations do NOT
	// advance it — that is the load-bearing safety. Only confirmed
	// observations (the rebalance proceeds) update it.
	//
	// partitionShrunkObservations is the running count of consecutive
	// suspicious observations. Once it reaches
	// PartitionShrinkConfirmationCount the calculator trusts the
	// shrink and rebalances; any healthy observation resets it to 0.
	//
	// Both fields are read+written only on the rebalance path, which is
	// already serialized by rebalanceMu/pollMu — no extra synchronization
	// is needed.
	lastKnownPartitionCount     int
	partitionShrunkObservations int

	// Minimum-credible-worker-input doctrine (see
	// errSuspiciousWorkerObservation Godoc):
	//
	// lastKnownWorkerCount is the most recent worker count the
	// calculator committed to. Only a rebalance that passes the F10-A
	// floor advances it — that is the load-bearing safety.
	//
	// workerShrunkObservations is the running count of consecutive
	// suspicious observations. Once it reaches
	// WorkerShrinkConfirmationCount the calculator trusts the shrink;
	// any healthy observation resets it to 0. Named distinctly from
	// partitionShrunkObservations so the two cross-feature counters
	// do not share a confirmation window.
	//
	// Both fields are touched from the getActiveWorkers path (called
	// from rebalance under rebalanceMu and from the worker-monitor
	// poll under pollMu — independent locks) and from the rebalance
	// floor itself. c.mu fences those read-modify-writes without
	// widening either of the path-specific locks.
	lastKnownWorkerCount     int
	workerShrunkObservations int

	// enumFailures is the running count of consecutive non-connectivity
	// enumeration (heartbeat Keys scan) failures. Crossed against
	// EnumerationFailureThreshold to fire OnEnumerationError; reset to zero on any
	// successful enumeration (nil-error GetActiveWorkers, before F10-A handling).
	// Guarded by c.mu (same as workerShrunkObservations).
	enumFailures int

	// labelState carries per-label grace clocks and defer-once streaks
	// (spec §8.5). Touched only on the rebalance path (serialized by
	// rebalanceMu) and, once Task 9 lands, the label re-check timer; its
	// own mutex fences the two.
	labelState *labelState

	// lastPublishedPartitions retains (a reference to) the source snapshot
	// behind THIS calculator's most recent successful publish. The
	// cached-observation replay proof compares the current snapshot against
	// it by label-aware content (Keys + Weight + Label): the revision
	// fast-path alone cannot prove "unchanged" for revision-blind sources,
	// and PartitionSetDigest/BatchDigest are deliberately label-blind (a
	// label-only edit slips through them). Holding a reference is safe:
	// snapshot providers return a fresh slice per call (sources replace
	// their backing slice rather than mutating it in place). Guarded by
	// c.mu. Nil until the first successful publish by this instance.
	lastPublishedPartitions []types.Partition

	// Label re-check timer state (spec §8.3). The guaranteed second
	// observation — grace expiry and deferral confirmation — must re-fire
	// without any external event and survive busy states.
	//
	// pendingLabelRecheck is the sticky "a label re-check is owed" flag. Set
	// by requestLabelRecheck (deferral / grace-expiry / non-fresh) and drained
	// by monitorLabelRecheck under a CompareAndSwap; it survives a busy state
	// machine because the monitor re-stores it when it cannot claim.
	//
	// labelRecheckCh (capacity 1) coalesces timer + external signals into a
	// single monitor wake. A non-blocking send never blocks the rebalance path.
	pendingLabelRecheck atomic.Bool
	labelRecheckCh      chan struct{}

	// labelRecheckTimer is the grace-expiry AfterFunc armed after a rebalance
	// that parked anything (armLabelRecheckAfterRebalance). Guarded by
	// labelRecheckTimerMu so Stop and re-arm can cancel the previous one.
	labelRecheckTimer   *time.Timer
	labelRecheckTimerMu sync.Mutex

	// prevMetricLabels is the label set recorded by the last successful
	// rebalance's LabelMetrics pass (spec §13 lifecycle: per-label gauges
	// recomputed every rebalance; a label absent from the current snapshot
	// is explicitly zeroed in the same pass rather than left at its last
	// non-zero reading; a stopping leader zeroes and clears everything —
	// the gauges are leader-scoped). Touched only inside recordLabelMetrics
	// (called from rebalance under rebalanceMu) and zeroLabelGaugesOnStop
	// (takes rebalanceMu itself) — no separate mutex needed.
	prevMetricLabels map[string]bool

	// labelReadModel is the retained pull-side counterpart of the
	// recordLabelMetrics push pass: the per-label pool sizes and parked
	// counts from the last successfully published rebalance, kept for
	// Manager.LabelState. Written only post-publish (retainLabelReadModel)
	// and cleared in Stop — leader-scoped, like the label gauges. nil until
	// the first publish.
	labelReadModel atomic.Pointer[LabelReadModel]
}

// LabelReadModel is the retained label snapshot behind Manager.LabelState:
// per-label live pool sizes and parked partition counts from the last
// successfully published rebalance. Keys carry exact parity with the
// recordLabelMetrics gauge pass (topo.SortedLabels — labeled pools only).
type LabelReadModel struct {
	PoolSizes map[string]int
	Parked    map[string]int
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
		labelRecheckCh:      make(chan struct{}, 1), // capacity 1: timer + external signals coalesce
	}

	// Initialize emergency detector with configured grace period
	c.emergencyDetector = NewEmergencyDetector(cfg.EmergencyGracePeriod)

	// Label grace/confirmation state. cfg.Now is defaulted by SetDefaults
	// above, so the injected clock (tests) or time.Now (production) is
	// already resolved. UnlabeledPartitionPolicy is defaulted to
	// "dedicated" by SetDefaults; the topology builder treats "" the same,
	// so the default is belt-and-suspenders.
	c.labelState = newLabelState(cfg.LabelSpillGrace, cfg.Now)

	// Initialize components.
	//
	// The GC is constructed first (without a publisher) only conceptually:
	// in practice we construct the publisher first, then the GC, then wire
	// the publisher's GCTriggerFn to the GC's Trigger method. This avoids a
	// chicken-and-egg dependency.
	c.publisher = NewAssignmentPublisher(PublisherConfig{
		AssignmentKV:    cfg.AssignmentKV,
		HeartbeatKV:     cfg.HeartbeatKV,
		Prefix:          cfg.AssignmentPrefix,
		HeartbeatPrefix: cfg.HeartbeatPrefix,
		LeaderCheckFn:   cfg.LeaderCheck,
		Logger:          cfg.Logger,
		Metrics:         cfg.Metrics,
		IsShuttingDown:  c.isStopping,
		Now:             cfg.Now,
	})

	c.gc = NewCommitGC(CommitGCConfig{
		Publisher: c.publisher,
		Logger:    cfg.Logger,
		Metrics:   cfg.Metrics,
	})
	if c.gc != nil {
		c.publisher.gcTriggerFn = c.gc.Trigger
	}

	c.stateMach = NewStateMachine(
		cfg.Logger,
		cfg.Metrics,
		c.handleRebalance,
		stopCh,
	)
	// Partition-lifecycle callback is wired here rather than via the
	// constructor signature to keep NewStateMachine's surface stable (tests
	// construct StateMachine directly without the partition path).
	c.stateMach.onPartitionRebalanceCb = c.handlePartitionRebalance

	c.monitor = NewWorkerMonitor(
		cfg.HeartbeatKV,
		cfg.HeartbeatPrefix,
		cfg.HeartbeatTTL,
		c.pollForChanges,
		cfg.Logger,
	)

	// Route a detected worker-label change (a heartbeat PUT whose labels differ
	// from the monitor's retained fingerprint — e.g. a stale-ID takeover behind
	// a live worker ID) through the label re-check path. The worker-set change
	// path no-ops on an unchanged set, so this is the only path that recomputes
	// label-correct assignments when the fleet's labels shift without the set
	// changing. The callback is coalesced by requestLabelRecheck.
	c.monitor.SetOnLabelChange(func() {
		if lm, ok := c.Metrics.(types.LabelMetrics); ok {
			lm.IncrementLabelChangeTrigger()
		}
		c.requestLabelRecheck(lifecycleLabelChange)
	})

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
		// A deferred label observation is benign HERE TOO (spec §8.5): a new
		// leader's fresh per-term labelState (spec §8.4) defers its first
		// adverse observation by design, and requestLabelRecheck (fired
		// inside the rebalance) has already armed the confirming second
		// observation. The incumbent commit stays valid meanwhile, so
		// success-without-publish is safe. Failing Start instead would
		// release leadership (releaseLeadershipAfterCalculatorFailure), hand
		// the next leader another fresh labelState that defers again, and
		// livelock the fleet in a leadership flap where the defer-once
		// streak never reaches 2 and parked partitions are never committed
		// (the Task 14 pool-outage reproducer, spec I2 violation).
		if !errors.Is(err, errLabelObservationDeferred) {
			c.started.Store(false)
			return fmt.Errorf("immediate initial assignment failed: %w", err)
		}
		c.Logger.Info("initial assignment deferred pending label observation confirmation",
			"reason", initialReason)
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

	// Bootstrap the publisher's LastCommit cache so the leader-side audit can
	// run from t=0 against the incumbent commit (rolling-upgrade / takeover
	// path). Best-effort — failures leave LastCommit nil and the audit
	// becomes a no-op until the first locally-written commit lands.
	if err := c.publisher.BootstrapLastCommit(ctx); err != nil {
		c.Logger.Debug("bootstrap last commit failed (non-fatal)", "error", err)
	}

	// Start the leader-side apply audit loop (§4.2). Runs at AuditInterval
	// ticks, gated by the publisher's LastCommit cache. wg.Wait in Stop()
	// covers shutdown.
	c.auditDoneCh = make(chan struct{})
	c.wg.Go(func() { c.auditLoop(ctx) })

	// Start the best-effort payload GC loop (§3.5 step 12 / §3.9). Failures
	// here are non-fatal: GC is conservative and never participates in
	// correctness, so a missing background sweep only delays orphan reaping.
	if c.gc != nil {
		if err := c.gc.Start(ctx); err != nil {
			c.Logger.Warn("commit GC start failed (non-fatal)", "error", err)
		}
	}

	// Start partition monitoring if supported
	if watchable, ok := c.Source.(types.WatchablePartitionSource); ok {
		c.Logger.Info("starting partition monitor")
		c.wg.Go(func() {
			c.monitorPartitions(ctx, watchable)
		})
	}

	// Start the label re-check monitor unconditionally (spec §8.3): grace
	// expiry and deferral confirmation must re-fire without any external
	// event, independent of whether the source is watchable. Joined on Stop
	// via wg.Wait.
	c.wg.Go(func() {
		c.monitorLabelRecheck(ctx)
	})

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

	// Cancel any armed label re-check grace-expiry timer so a late AfterFunc
	// cannot fire requestLabelRecheck after the monitor goroutine has joined.
	c.labelRecheckTimerMu.Lock()
	if c.labelRecheckTimer != nil {
		c.labelRecheckTimer.Stop()
		c.labelRecheckTimer = nil
	}
	c.labelRecheckTimerMu.Unlock()

	// 2. Stop worker monitor (stops watcher and monitoring goroutines)
	if err := c.monitor.Stop(); err != nil {
		c.Logger.Error("failed to stop worker monitor", "error", err)
	}

	// Stop the GC loop (drains any in-flight sweep). Best-effort.
	if c.gc != nil {
		c.gc.Stop()
	}

	// 3. Wait for state machine shutdown
	c.stateMach.WaitForShutdown()

	// 4. Wait for background goroutines (e.g., monitorPartitions)
	c.wg.Wait()

	// 5. Close out the leader-scoped label gauges (spec §13): zero every
	// per-label gauge this leader last recorded so a deposed leader's
	// metrics export doesn't freeze at stale non-zero readings. Placed
	// after the joins above so no rebalance can record concurrently or
	// after the zeroing pass.
	c.zeroLabelGaugesOnStop()

	// Same leader-scoped lifecycle for the pull-side snapshot: a deposed or
	// stopping leader must not keep serving stale label state.
	c.labelReadModel.Store(nil)

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

	if err := c.rebalance(ctx, "manual-refresh"); err != nil {
		// A deferred label observation is benign everywhere (spec §8.5):
		// success-without-publish. The re-check armed inside the rebalance
		// owns the confirming retry; surfacing the internal sentinel to the
		// manual-refresh API caller would report a designed no-op as a
		// failure. lastWorkers is deliberately NOT updated on this path —
		// mirroring handleRebalance's early sentinel return — so the next
		// poll still observes any pending worker change.
		if errors.Is(err, errLabelObservationDeferred) {
			return nil
		}

		return err
	}

	c.mu.Lock()
	c.setLastWorkersLocked(c.currentWorkers)
	c.mu.Unlock()

	return nil
}

// discoverHighestVersion scans existing assignments in KV to find the highest
// version and the set of worker IDs that currently have assignment keys.
// This ensures version monotonicity across leader changes and lets the
// calculator seed its currentWorkers map so the first rebalance can sweep
// assignment keys for workers that no longer have active heartbeats.
func (c *Calculator) discoverHighestVersion(ctx context.Context) ([]string, error) {
	return c.publisher.DiscoverHighestVersion(ctx)
}

// detectRebalanceType classifies a non-emergency rebalance.
//
// Emergency detection and claim are handled in observeAndDecide, BEFORE
// this method is called; detectRebalanceType is pure: it returns only
// "cold_start", "planned_scale", or "" (suppressed).
//
// When pending is true (the detector is tracking at least one mid-grace
// disappearance for a worker that is still in `prev`), the scheduler must
// not start a planned_scale rebalance — letting the grace expire and the
// emergency path fire instead avoids the same-count-replacement race where
// a planned_scale would pick up the replacement worker and the detector
// would lose the tracking for the disappeared one.
//
// Parameters:
//   - prev: Previous set of active workers
//   - curr: Current set of active workers
//   - pending: True if the detector is currently tracking a disappearance for a worker in prev
//
// Returns:
//   - reason: "cold_start" | "planned_scale" | "" (suppressed)
//   - window: Stabilization window duration (0 if suppressed)
func (c *Calculator) detectRebalanceType(prev, curr map[string]bool, pending bool) (reason string, window time.Duration) {
	if pending {
		c.Logger.Info("worker change in grace period - waiting for confirmation",
			"prev_count", len(prev),
			"curr_count", len(curr),
		)

		return "", 0
	}

	if len(prev) == 0 {
		c.Logger.Info("cold start detected",
			"worker_count", len(curr),
			"window", c.ColdStartWindow,
		)

		return "cold_start", c.ColdStartWindow
	}

	c.Logger.Info("planned scale detected",
		"prev_count", len(prev),
		"curr_count", len(curr),
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

// pollForChanges is the WorkerMonitor callback. It runs the observe→decide→
// claim critical section under pollMu and, on an emergency claim, releases
// pollMu before invoking the long-running rebalance.
func (c *Calculator) pollForChanges(ctx context.Context) error {
	return c.observeAndDecide(ctx, nil)
}

// Partition-lifecycle constants. monitorPartitions threads these through
// rebalance so the in-rebalance recovery-grace guard can short-circuit
// only partition-triggered calls (worker-change, audit, emergency paths
// retain their own grace handling).
const (
	lifecyclePartitionUpdate         = "partition_update"
	lifecyclePartitionUpdateDeferred = "partition_update_deferred"
)

// isPartitionLifecycle reports whether the lifecycle is one of the
// monitor-claimed rebalances (monitorPartitions or monitorLabelRecheck) that
// honor the in-rebalance recovery-grace re-check in shouldDeferForRecoveryGrace.
// lifecycleLabelRecheck belongs here for the same reason as the partition
// lifecycles: its monitor checks inRecoveryGrace before claiming, so a grace
// flip between that pre-claim check and rebalanceMu acquisition must bail
// (errShuttingDown) and be retried by the drain tick —
// restorePendingLabelOnGraceBail restores the sticky flag on exactly that bail.
func isPartitionLifecycle(lc string) bool {
	return lc == lifecyclePartitionUpdate || lc == lifecyclePartitionUpdateDeferred || lc == lifecycleLabelRecheck
}

// Label re-check lifecycle constants (spec §8.3). monitorLabelRecheck drives
// its rebalances through the state-machine claim path under these reasons.
const (
	// lifecycleLabelRecheck drives a rebalance re-run owed to the label
	// re-check timer / requestLabelRecheck (deferral confirmation, grace
	// expiry, persistent-non-fresh convergence).
	lifecycleLabelRecheck = "label_recheck"
	// lifecycleLabelChange is the requestLabelRecheck reason fired when the
	// worker monitor detects a heartbeat label change behind a live worker ID
	// (SetOnLabelChange wiring in NewCalculator). It is distinct from
	// reasonNonFreshObservation, so requestLabelRecheck wakes the monitor
	// immediately for fast convergence.
	lifecycleLabelChange = "label_change"
)

// reasonNonFreshObservation is the requestLabelRecheck reason fired from INSIDE
// a rebalance when the worker observation degraded to the cached list (the
// replay-skip / defer path). Defined as a constant so the call site and the
// spin-guard comparison in requestLabelRecheck cannot drift: a non-fresh
// re-check must NOT wake the monitor synchronously (see requestLabelRecheck).
const reasonNonFreshObservation = "non_fresh_observation"

// isStopping reports whether Stop has been called. Used as the
// Publisher's IsShuttingDown gate.
func (c *Calculator) isStopping() bool {
	select {
	case <-c.stopCh:
		return true
	default:
		return false
	}
}

// inRecoveryGrace is a nil-safe accessor for the state provider's grace flag.
func (c *Calculator) inRecoveryGrace() bool {
	return c.stateProvider != nil && c.stateProvider.IsInRecoveryGrace()
}

// shouldDeferForRecoveryGrace reports whether a rebalance with the given
// lifecycle should bail because the leader entered recovery grace between
// the caller's check and rebalanceMu acquisition. Only monitor-claimed
// rebalances (partition-triggered and label re-check) honor this re-check;
// worker-change / audit / emergency paths have their own grace handling
// (checkForChanges, emergency bypass).
func (c *Calculator) shouldDeferForRecoveryGrace(lifecycle string) bool {
	if !isPartitionLifecycle(lifecycle) {
		return false
	}

	return c.inRecoveryGrace()
}

// ctxFromStopCh returns a derived context that is cancelled when EITHER
// the parent context is cancelled OR stopCh is closed. The caller MUST
// invoke the returned CancelFunc to release the watcher goroutine.
// timeout == 0 means no timeout — only parent + stopCh cancellation.
func ctxFromStopCh(parent context.Context, stopCh <-chan struct{}, timeout time.Duration) (context.Context, context.CancelFunc) {
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	return ctx, cancel
}

// wrapPublishErr lets errShuttingDown pass through unchanged so handleRebalance
// can suppress it, while reclassifying other publish errors through wrapStopErr.
func (c *Calculator) wrapPublishErr(err error) error {
	if errors.Is(err, errShuttingDown) {
		return err
	}

	return c.wrapStopErr(fmt.Errorf("failed to publish assignments: %w", err))
}

// wrapStopErr reclassifies an error as errShuttingDown when stopCh is
// closed. This preserves the existing handleRebalance contract: state-machine
// callers and monitorPartitions log non-errShuttingDown errors at error
// level, but a normal shutdown should not surface as a hard failure.
func (c *Calculator) wrapStopErr(err error) error {
	if err == nil {
		return nil
	}
	select {
	case <-c.stopCh:
		return errShuttingDown
	default:
		return err
	}
}

// partitionRebalanceRequestTimeout bounds the request context allocated for
// the partition-lifecycle rebalance itself. Production default matches the
// pre-PR-3 hard-coded 30s used by the legacy triggerPartitionRebalance.
// Declared as a package-level var so tests can shorten it via export_test.go
// to force deterministic context-exhaustion regressions. Production callers
// MUST NOT mutate this value.
var partitionRebalanceRequestTimeout = 30 * time.Second

// partitionTailCheckTimeout bounds the in-line observeAndDecide tail-check
// that runs after a partition-lifecycle rebalance returns to Idle. It MUST
// be allocated on a fresh ctxFromStopCh-derived context — NOT on the
// rebalance's reqCtx — because reqCtx may already be cancelled or near its
// partitionRebalanceRequestTimeout deadline by the time RunClaimedRebalanceErr
// returns.
//
// 15s gives roughly 3x headroom over the expected p99 of one worker-set KV
// read (the dominant cost; TryClaimEmergency is sub-microsecond), stays well
// below partitionRebalanceRequestTimeout, and shutdown still cancels via
// stopCh regardless of the value. Operational policy bound, not an
// empirically proven threshold.
var partitionTailCheckTimeout = 15 * time.Second

// partitionRebalanceBlocker is a test-only synchronization hook consumed by
// handlePartitionRebalance after the entries counter is incremented and
// before c.rebalance runs. Nil in production; tests install a non-nil
// channel via export_test.go to hold the callback open until they have
// observed callback entry and confirmed reqCtx has expired. The hook is
// scoped to the partition callback only; scaling/emergency callbacks do not
// consult it, so production rebalance latency is unaffected.
var partitionRebalanceBlocker chan struct{}

// partitionTailCheckEntryHook is a test-only observation hook fired
// immediately before the tail-check observeAndDecide call in
// triggerPartitionRebalance. Tests install it via export_test.go to capture
// tailCtx state and prove the §3.5 fresh-context invariant directly. Nil in
// production.
var partitionTailCheckEntryHook func(tailCtx context.Context, reqCtxErr error)

// handlePartitionRebalance is the partition-lifecycle rebalance callback
// invoked by StateMachine.RunClaimedRebalanceErr. Unlike handleRebalance, it
// does NOT swallow errShuttingDown — the caller needs that error so
// restorePendingOnGraceBail can restore pendingPartitionUpdate when grace
// flipped between the pre-check and rebalanceMu acquisition.
func (c *Calculator) handlePartitionRebalance(ctx context.Context, lifecycle string) error {
	c.partitionRebalanceEntries.Add(1)
	if h := partitionRebalanceBlocker; h != nil {
		<-h
	}
	if err := c.rebalance(ctx, lifecycle); err != nil {
		if errors.Is(err, errShuttingDown) {
			c.Logger.Info("partition rebalance skipped during shutdown / grace flip",
				"lifecycle", lifecycle)
			return err
		}
		// A suspicious partition observation is an explicit skip, not a
		// failure. Surface the sentinel to the lifecycle caller
		// (triggerPartitionRebalance) so it can re-arm
		// pendingPartitionUpdate for the next drainTick. The lifecycle
		// caller translates it to a benign nil; nothing else escalates
		// because handlePartitionRebalance is wired only as the
		// state-machine's onPartitionRebalanceCb callback.
		if errors.Is(err, errSuspiciousPartitionObservation) {
			return err
		}
		// A suspicious worker observation does not need re-arming here:
		// the worker-monitor poll loop will fire its own rebalance
		// once the heartbeat scan heals or EmergencyDetector confirms
		// deaths, and that rebalance will pick up any pending
		// partition change. Swallowing avoids the spurious "partition
		// rebalance failed" Error log a non-failure would otherwise
		// produce.
		if errors.Is(err, errSuspiciousWorkerObservation) {
			return nil
		}

		return fmt.Errorf("partition rebalance failed for %s: %w", lifecycle, err)
	}
	// IMPORTANT: do NOT update lastWorkers here. The immediate tail-check in
	// triggerPartitionRebalance must see lastWorkers as it was BEFORE this
	// partition rebalance so EmergencyDetector.CheckEmergency Phase 4 can
	// still recognise any worker that disappeared during the rebalance
	// window (it needs the disappeared worker in `prev`). If the tail-check
	// observes a worker-set change it claims emergency / scaling and
	// handleRebalance refreshes lastWorkers on that path; if it does not,
	// the next observeAndDecide cycle refreshes lastWorkers naturally on
	// its own write path.
	return nil
}

// triggerPartitionRebalance runs a partition-lifecycle rebalance via the
// state-machine claim path. On claim success it drives the rebalance via
// RunClaimedRebalanceErr, then runs a single tail-check observeAndDecide so
// any emergency that arrived during the rebalance window gets an immediate
// claim opportunity. On claim failure it restores pendingPartitionUpdate so
// the drain ticker retries.
//
// Returns the rebalance callback's error so the caller can run
// restorePendingOnGraceBail.
func (c *Calculator) triggerPartitionRebalance(lifecycle string) error {
	if !c.stateMach.TryClaimRebalancing(context.Background(), lifecycle) {
		c.pendingPartitionUpdate.Store(true)
		return nil
	}

	reqCtx, cancel := ctxFromStopCh(context.Background(), c.stopCh, partitionRebalanceRequestTimeout)
	defer cancel()

	err := c.stateMach.RunClaimedRebalanceErr(reqCtx, lifecycle)

	// A suspicious-observation suppression is a deferred no-op, not a
	// failure. The watcher in monitorPartitions fires only when the
	// partition list changes, so a single N→0 (or N→tiny) transition
	// produces one signal; without a re-arm the confirmation window
	// stalls forever because the drainTick has nothing pending to drain.
	// Re-arm here so the next drainTick re-attempts (each re-attempt
	// observes the same shrunk source and advances the counter toward
	// PartitionShrinkConfirmationCount). The lifecycle caller sees a
	// benign nil; the rebalance was suppressed, not failed.
	if errors.Is(err, errSuspiciousPartitionObservation) {
		c.pendingPartitionUpdate.Store(true)
		err = nil
	}

	// A deferred label observation is a benign "confirm before acting"
	// decision. Unlike the suspicious-partition case above, do NOT re-arm
	// pendingPartitionUpdate: the label re-check timer (Task 9) owns the
	// retry, so re-arming here would double-drive the same rebalance.
	if errors.Is(err, errLabelObservationDeferred) {
		err = nil
	}

	// Tail-check on a FRESH stop-aware context: reqCtx may already be near or
	// past its deadline; observeAndDecide exits fast on a cancelled context
	// and would strand any emergency loser until the next poll tick.
	tailCtx, tailCancel := ctxFromStopCh(context.Background(), c.stopCh, partitionTailCheckTimeout)
	defer tailCancel()
	if h := partitionTailCheckEntryHook; h != nil {
		h(tailCtx, reqCtx.Err())
	}
	if tailErr := c.observeAndDecide(tailCtx, nil); tailErr != nil &&
		!errors.Is(tailErr, errShuttingDown) {
		c.Logger.Debug("post-partition-rebalance tail-check observeAndDecide returned error",
			"lifecycle", lifecycle, "error", tailErr)
	}

	return err
}

// restorePendingOnGraceBail restores pendingPartitionUpdate=true when
// the rebalance returned errShuttingDown but stopCh is still open. That
// indicates the bail came from the in-rebalance recovery-grace re-check
// (grace flapped to true between drain decision and rebalance entry),
// NOT from a real shutdown — so the deferred update should be retried.
func (c *Calculator) restorePendingOnGraceBail(err error) {
	if !errors.Is(err, errShuttingDown) {
		return
	}
	select {
	case <-c.stopCh:
		// Real shutdown; nothing to restore.
	default:
		c.pendingPartitionUpdate.Store(true)
	}
}

func (c *Calculator) monitorPartitions(ctx context.Context, source types.WatchablePartitionSource) {
	ch := source.Watch(ctx)
	drainTick := time.NewTicker(c.RebalanceGraceDrainInterval)
	defer drainTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-drainTick.C:
			if !c.pendingPartitionUpdate.CompareAndSwap(true, false) {
				continue
			}
			if c.inRecoveryGrace() {
				c.pendingPartitionUpdate.Store(true) // restore; retry next tick
				continue
			}
			err := c.triggerPartitionRebalance(lifecyclePartitionUpdateDeferred)
			c.restorePendingOnGraceBail(err)
		case _, ok := <-ch:
			if !ok {
				return // Channel closed
			}
			// Partition change is the rebalance-eligible signal for
			// "currentSourceRevision > commit.SourceRevision" (§4.2 final
			// paragraph). The leader-side apply audit deliberately does
			// NOT compare source revisions against a fresh snapshot —
			// that drift is detected here and triggers a rebalance via
			// the publisher's normal path (the next commit will carry
			// the fresh SourceRevision).
			c.Logger.Info("partition change detected")

			select {
			case <-c.stopCh:
				c.Logger.Info("skipping partition rebalance: shutdown in progress")
				return
			default:
			}

			if c.inRecoveryGrace() {
				c.pendingPartitionUpdate.Store(true)
				c.Logger.Info("deferring partition rebalance: leader is in recovery grace period")
				continue
			}

			// Immediate trigger supersedes any pending deferred update; both
			// would publish the same fresh source snapshot.
			c.pendingPartitionUpdate.Store(false)
			err := c.triggerPartitionRebalance(lifecyclePartitionUpdate)
			c.restorePendingOnGraceBail(err)
		}
	}
}

// restorePendingLabelOnGraceBail restores pendingLabelRecheck=true when the
// rebalance returned errShuttingDown but stopCh is still open — the bail came
// from an in-rebalance recovery-grace re-check (grace flipped between the
// monitor's pre-claim check and rebalance entry), NOT from a real shutdown, so
// the owed re-check should be retried on the next tick. Mirrors
// restorePendingOnGraceBail for the label flag (a straight copy is clearer than
// generalizing).
func (c *Calculator) restorePendingLabelOnGraceBail(err error) {
	if !errors.Is(err, errShuttingDown) {
		return
	}
	select {
	case <-c.stopCh:
		// Real shutdown; nothing to restore.
	default:
		c.pendingLabelRecheck.Store(true)
	}
}

// monitorLabelRecheck owns the guaranteed second observation (spec §8.3): it
// re-runs a rebalance when a label re-check is owed (deferral confirmation,
// grace expiry, persistent-non-fresh convergence) even if no external event
// ever arrives. It mirrors monitorPartitions: a capacity-1 channel coalesces
// signals, a drain ticker guarantees eventual service across busy/grace
// states, and the pendingLabelRecheck flag is sticky so nothing is lost when
// the state machine is busy. Started unconditionally by Start; joined on Stop
// via wg.Wait (returns on stopCh close).
func (c *Calculator) monitorLabelRecheck(ctx context.Context) {
	drainTick := time.NewTicker(c.RebalanceGraceDrainInterval)
	defer drainTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-c.labelRecheckCh:
		case <-drainTick.C:
		}

		if !c.pendingLabelRecheck.CompareAndSwap(true, false) {
			continue
		}
		if c.inRecoveryGrace() {
			c.pendingLabelRecheck.Store(true) // retry next tick
			continue
		}
		if !c.stateMach.TryClaimRebalancing(context.Background(), lifecycleLabelRecheck) {
			c.pendingLabelRecheck.Store(true) // busy; sticky flag survives
			// Short retry so a deferral raised INSIDE a running rebalance
			// (claim still held when the wake arrives) confirms in ~2s rather
			// than waiting a full drain tick (spec §16 item 7: re-check well
			// under the drain cadence). Coalesced: the channel has capacity 1.
			time.AfterFunc(2*time.Second, func() {
				select {
				case c.labelRecheckCh <- struct{}{}:
				default:
				}
			})

			continue
		}

		reqCtx, cancel := ctxFromStopCh(context.Background(), c.stopCh, partitionRebalanceRequestTimeout)
		err := c.stateMach.RunClaimedRebalanceErr(reqCtx, lifecycleLabelRecheck)
		cancel()
		if errors.Is(err, errLabelObservationDeferred) {
			// The deferral re-arms itself via requestLabelRecheck inside the
			// rebalance; nothing extra to do (the flag is already set again).
			continue
		}
		c.restorePendingLabelOnGraceBail(err)
	}
}

// checkForChanges is a thin wrapper around observeAndDecide retained for
// tests and any caller that wants to inject a pre-computed worker set.
//
// Parameters:
//   - ctx: Context for cancellation
//   - currentWorkers: Optional set of active worker IDs (if empty, fetched from KV)
//
// Returns:
//   - error: Processing error, nil on success
func (c *Calculator) checkForChanges(ctx context.Context, currentWorkers ...map[string]bool) error {
	var provided map[string]bool
	if len(currentWorkers) > 0 && currentWorkers[0] != nil {
		provided = currentWorkers[0]
	}

	return c.observeAndDecide(ctx, provided)
}

// pollAction is the action observeAndDecideLocked hands back to observeAndDecide
// to perform AFTER pollMu is released. Both the emergency rebalance and the
// degraded-cache skip log must run with the lock released — the prior code
// unlocked before each — so the locked core signals them rather than running
// them under the lock (a slow injected Logger or the long rebalance must not
// extend the poll critical section).
type pollAction int

const (
	pollActionNone            pollAction = iota // locked core fully handled it
	pollActionRunEmergency                      // caller runs RunClaimedRebalance
	pollActionLogDegradedSkip                   // caller logs the degraded-cache skip
)

// observeAndDecide performs a fresh observation of the worker set, runs
// CheckEmergency (if the observation is fresh), and either claims the emergency
// lifecycle or enters the planned scaling state. The observe→decide→claim
// critical section runs in observeAndDecideLocked under pollMu; this wrapper then
// performs the actions that must happen with pollMu RELEASED.
//
// The pollMu is released BEFORE the emergency rebalance is invoked: the state
// machine has already committed to Emergency under the lock, so a concurrent poll
// cannot start a competing decision, and the rebalance itself is independently
// serialized by rebalanceMu. Holding pollMu across the rebalance would stall
// every other poller for its duration. The degraded-cache skip is likewise logged
// after release, matching the prior unlock-before-log on that path.
//
// Parameters:
//   - ctx: Context for cancellation
//   - provided: Pre-computed worker set (nil → fetch fresh from KV)
//
// Returns:
//   - error: Processing error, nil on success
func (c *Calculator) observeAndDecide(ctx context.Context, provided map[string]bool) error {
	action, err := c.observeAndDecideLocked(ctx, provided)
	if err != nil {
		return err
	}
	switch action {
	case pollActionRunEmergency:
		c.stateMach.RunClaimedRebalance(ctx, "emergency")
	case pollActionLogDegradedSkip:
		c.Logger.Info("skipping rebalance decision: observation is from degraded cache")
	case pollActionNone:
		// Fully handled inside the locked core; nothing to do post-release.
	}

	return nil
}

// observeAndDecideLocked is the observe→decide→claim critical section. pollMu
// serializes the whole sequence to prevent races between concurrent pollers /
// watchers / lifecycle callers, and is released by a SINGLE defer on every exit
// path (the structured replacement for the prior hand-placed unlocks). It returns
// the pollAction observeAndDecide must perform AFTER the lock is released:
// pollActionRunEmergency (an emergency was detected AND claimed) or
// pollActionLogDegradedSkip (a changed-but-non-fresh observation). The planned
// scaling-state entry and every other skip path complete inside the locked
// section and return pollActionNone.
//
// When the observation is not fresh (degraded mode), the detector is NOT mutated
// and no rebalance decision is taken: stale data must not drive topology changes.
func (c *Calculator) observeAndDecideLocked(ctx context.Context, provided map[string]bool) (pollAction, error) {
	c.pollMu.Lock()
	defer c.pollMu.Unlock()

	workers, fresh, err := c.collectWorkerObservation(ctx, provided)
	if err != nil {
		return pollActionNone, err
	}

	c.mu.RLock()
	changed := c.hasWorkersChangedLocked(workers)
	lastWorkersCopy := c.cloneLastWorkersLocked()
	c.mu.RUnlock()

	// Reconcile the detector against every fresh observation, BEFORE the
	// unchanged short-circuit. The detector invariant — firstSeen[A] is the
	// moment A was last observed alive in any leader scan — requires that a
	// fresh observation in which A is alive clears stale tracking for A even
	// when the topology is otherwise unchanged. Skipping this on !changed
	// would let a "pure recovery" poll keep an old firstSeen, so the next
	// disappearance of A would inherit the stale timestamp and bypass the
	// grace period.
	//
	// In degraded mode the worker list comes from cache; we must not mutate
	// detector state from stale data, so CheckEmergency is gated on fresh.
	var (
		emergency   bool
		disappeared []string
		pending     bool
	)
	if fresh {
		emergency, disappeared, pending = c.emergencyDetector.CheckEmergency(lastWorkersCopy, workers)
	}

	if !changed {
		return pollActionNone, nil
	}

	c.Logger.Info("polling detected worker change", "workers", len(workers), "fresh", fresh)

	// Degraded observation cannot drive a topology decision: the worker list
	// was returned from cache and may not reflect current truth. The skip is
	// logged by observeAndDecide AFTER pollMu is released, matching the prior
	// unlock-before-log on this path.
	if !fresh {
		return pollActionLogDegradedSkip, nil
	}

	if emergency {
		// Claim under pollMu; observeAndDecide runs the rebalance after the
		// deferred Unlock fires (release-before-rebalance).
		if c.claimEmergency(ctx, disappeared) {
			return pollActionRunEmergency, nil
		}

		return pollActionNone, nil
	}

	reason, window := c.detectRebalanceType(lastWorkersCopy, workers, pending)
	if reason == "" {
		return pollActionNone, nil
	}

	if c.inRecoveryGrace() {
		c.Logger.Info("skipping rebalance: leader is in recovery grace period after degraded mode")
		return pollActionNone, nil
	}

	// c.Now is the injectable clock (embedded Config field, defaulted to
	// time.Now in SetDefaults); the publisher stamps lastRebalance with the
	// same clock so this comparison is deterministic under test.
	if c.Now().Sub(c.publisher.LastRebalanceTime()) < c.Cooldown {
		c.Logger.Debug("worker change detected but rate limit active",
			"reason", reason,
			"min_rebalance_interval", c.Cooldown,
		)
		return pollActionNone, nil
	}

	if c.GetState() != types.CalcStateIdle {
		c.Logger.Debug("worker change detected but calculator not idle",
			"state", c.GetState().String(),
			"reason", reason,
		)
		return pollActionNone, nil
	}

	c.enterScalingState(ctx, reason, window)

	return pollActionNone, nil
}

// collectWorkerObservation builds the current-worker set, either from a
// caller-supplied map (treated as fresh) or by fetching from KV.
func (c *Calculator) collectWorkerObservation(ctx context.Context, provided map[string]bool) (map[string]bool, bool, error) {
	if provided != nil {
		return provided, true, nil
	}

	workerList, fresh, err := c.getActiveWorkers(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("failed to get active workers: %w", err)
	}

	workers := make(map[string]bool, len(workerList))
	for _, w := range workerList {
		workers[w] = true
	}

	return workers, fresh, nil
}

// claimEmergency atomically claims the emergency lifecycle on the state
// machine. Returns true if the claim succeeded; the caller is then
// responsible for releasing pollMu and invoking RunClaimedRebalance.
//
// Lock ownership stays with observeAndDecide: this function never touches
// pollMu, so the critical-section boundaries are visible in one place.
func (c *Calculator) claimEmergency(ctx context.Context, disappeared []string) bool {
	if !c.stateMach.TryClaimEmergency(ctx) {
		c.Logger.Warn("emergency detected but state machine could not claim",
			"disappeared", disappeared)
		return false
	}

	c.mu.Lock()
	c.disappearedWorkers = disappeared
	c.mu.Unlock()

	c.Metrics.RecordEmergencyRebalance(len(disappeared))
	c.Logger.Warn("emergency claim succeeded - running rebalance",
		"disappeared", disappeared)

	return true
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
// This method implements cache fallback for two failure shapes:
//  1. Connectivity error from NATS KV: returns the last cached worker
//     list with fresh=false; if no cache exists, returns ErrDegraded.
//  2. Sharply-shrunk fresh observation (the F10-A truncated-Keys()
//     defense, see errSuspiciousWorkerObservation Godoc): degrades to
//     the cached set with fresh=false until the confirmation window
//     (WorkerShrinkConfirmationCount consecutive suspicious reads) is
//     met.
//
// A healthy (non-shrunk) fresh observation refreshes the cache, resets
// workerShrunkObservations to zero, and returns fresh=true.
//
// lastKnownWorkerCount is intentionally NOT advanced here — the
// rebalance-side floor advances it only after the floor passes, so the
// floor always compares against a baseline established by a prior
// committed rebalance.
//
// Callers that mutate detector state on the observation MUST gate that
// mutation on fresh==true (see collectRebalanceWorkers's ObserveAlive
// gating).
func (c *Calculator) getActiveWorkers(ctx context.Context) ([]string, bool, error) {
	workers, err := c.monitor.GetActiveWorkers(ctx)
	if err != nil {
		if natsutil.IsConnectivityError(err) {
			return c.cacheFallbackOrDegraded("connectivity_error",
				fmt.Errorf("%w: no cached workers available: %w", types.ErrDegraded, err),
				func(cached []string, age time.Duration) {
					c.Logger.Warn("using cached worker list due to connectivity error",
						"workers", len(cached),
						"cache_age", age,
						"error", err)
				})
		}

		// Sustained non-connectivity enumeration failure (NP-10): the poll loop
		// only logs this and no caller routes it to the degraded circuit. Threshold
		// locally and, once sustained, surface it via OnEnumerationError so the
		// manager can degrade directly (the transient KV-error window would be
		// cleared by the still-succeeding heartbeat Put — the F-D1 constraint).
		c.recordEnumerationFailure(err)

		return nil, false, err
	}

	// Enumeration succeeded: the Keys-scan stall (if any) has recovered. Reset the
	// NP-10 counter and signal recovery HERE — before the F10-A suspicious-shrink
	// handling below — so a recovered-but-suspicious scan still clears the stall.
	c.resetEnumerationFailures()

	// F10-A defense: a sharply-shrunk fresh observation is treated as
	// suspicious until the confirmation window is satisfied. The first
	// such reads degrade to cached+fresh=false; once the confirmation
	// window is satisfied the read is surfaced fresh so the
	// rebalance-side floor can apply the emergency-confirmation gate.
	//
	// getActiveWorkers is called from the rebalance path
	// (collectRebalanceWorkers, under rebalanceMu) AND from the
	// worker-monitor poll path (collectWorkerObservation, under
	// pollMu). Those locks are independent, so c.mu fences the F10-A
	// counter read-modify-write here.
	suspicious, consec, accepted, lastKnown := c.recordWorkerObservation(len(workers))
	if suspicious && !accepted {
		c.Logger.Warn("ignoring sharply-shrunk worker observation pending confirmation",
			"last_known", lastKnown,
			"observed", len(workers),
			"threshold_pct", c.WorkerShrinkConfirmationThresholdPct,
			"consecutive_observations", consec,
			"required", c.WorkerShrinkConfirmationCount)

		// The Warn above already explained the fallback, so onHit is nil here
		// (unlike the connectivity path, which logs on the cache hit).
		return c.cacheFallbackOrDegraded("suspicious_worker_shrink",
			fmt.Errorf("%w: %w", types.ErrDegraded, errSuspiciousWorkerObservation), nil)
	}

	// Healthy observation: refresh the cache so future connectivity
	// errors fall back to a trusted shape. An accepted-shrunk read is
	// surfaced fresh but NOT cached: until the rebalance floor commits,
	// the shrunk view is not yet trusted.
	if !suspicious {
		c.updateCachedWorkers(workers)
	}

	return workers, true, nil
}

// readWorkerLabels fetches labels-of-record for the rebalance worker set
// (spec §6). One bounded retry for missing workers; then the taxonomy:
// more than max(1, 10%) unreadable ⇒ errLabelReadBroadFailure (broad
// failures must never become label decisions). Legacy heartbeats decode
// with nil labels — that is a SUCCESSFUL read of an empty set.
//
// Unreadable means unreadable for ANY reason: an absent heartbeat key
// (worker departed between the active-set scan and this read) and a
// present-but-undecodeable payload both leave the worker UNKNOWN and both
// count toward the cap. Spec §6 is explicit: unreadable labels are
// unknown, never guessed — "unlabeled" would hand general work to what
// may be a dedicated worker under the dedicated policy. A decoded legacy
// timestamp heartbeat proves a well-formed pre-label worker; an
// undecodeable payload proves nothing.
//
// Connectivity / degrading-JetStream errors on ANY worker are broad by
// class regardless of count (the 1-worker-fleet trap) and short-circuit
// before the count rule, which only governs UNCLASSIFIED failures (e.g.
// key-not-found on a live worker, malformed payloads).
func (c *Calculator) readWorkerLabels(ctx context.Context, workers []string) (map[string][]string, map[string]bool, error) {
	hbs, fails, err := c.monitor.GetHeartbeatsFor(ctx, workers)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errLabelReadBroadFailure, err)
	}
	missing := missingWorkers(workers, hbs)
	if len(missing) > 0 { // one inline retry for the stragglers
		retry, retryFails, rerr := c.monitor.GetHeartbeatsFor(ctx, missing)
		if rerr == nil {
			maps.Copy(hbs, retry)
			fails = retryFails // post-retry classification uses the fresh errors
		}
		missing = missingWorkers(workers, hbs)
	}

	// Error-CLASS taxonomy first (spec §6): a connectivity or
	// degrading-JetStream failure is broad by nature, regardless of how
	// many workers it hit — in a 1-worker fleet a count-based rule alone
	// would misclassify it as an isolated unknown and eventually
	// empty-assign the whole fleet.
	for _, w := range missing {
		ferr := fails[w]
		if natsutil.IsConnectivityError(ferr) || natsutil.IsDegradingJetStreamError(ferr) {
			return nil, nil, fmt.Errorf("%w: worker %s: %w", errLabelReadBroadFailure, w, ferr)
		}
	}

	broadCap := max(1, len(workers)/10)
	if len(missing) > broadCap {
		return nil, nil, fmt.Errorf("%w: %d of %d workers unreadable", errLabelReadBroadFailure, len(missing), len(workers))
	}

	labels := make(map[string][]string, len(hbs))
	for w, hb := range hbs {
		labels[w] = hb.Labels
	}
	unknown := make(map[string]bool, len(missing))
	for _, w := range missing {
		unknown[w] = true
	}

	return labels, unknown, nil
}

// missingWorkers returns workers absent from the heartbeat map, sorted.
func missingWorkers(workers []string, hbs map[string]types.Heartbeat) []string {
	var out []string
	for _, w := range workers {
		if _, ok := hbs[w]; !ok {
			out = append(out, w)
		}
	}
	slices.Sort(out)

	return out
}

// partitionsContentEqual reports label-aware content equality of two
// partition slices: pairwise-equal Keys, Weight, AND Label, in order. The
// replay proof must compare content directly because
// PartitionSetDigest/BatchDigest are deliberately label-blind — a label-only
// edit slips through them. Order-sensitive by design: a source that reorders
// its listing without changing content merely degrades replay to the benign
// defer, the conservative outcome.
func partitionsContentEqual(a, b []types.Partition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Weight != b[i].Weight || a[i].Label != b[i].Label || !slices.Equal(a[i].Keys, b[i].Keys) {
			return false
		}
	}

	return true
}

// cachedObservationReplayable reports whether the last published commit
// already covers the current (cached) observation exactly, so the rebalance
// can skip the publish. Replay is provable iff:
//
//	(i)  the cached worker set equals the last commit's payload worker set, AND
//	(ii) the source snapshot is unchanged since that publish — via the cheap
//	     revision fast-path when BOTH revisions are known, else via label-aware
//	     content equality against the snapshot retained at publish time (the
//	     path that makes revision-blind sources provable).
//
// A bootstrapped commit (inherited from a previous leader, no local publish)
// has no retained snapshot: only the revision fast-path can prove it; content
// equality conservatively fails and the caller defers.
func (c *Calculator) cachedObservationReplayable(workers []string, partitions []types.Partition, srcRev uint64, srcKnown bool) bool {
	commit := c.publisher.LastCommit()
	if commit == nil {
		return false
	}

	// (i) worker-set equality against the commit's payload keys.
	if len(commit.Payloads) != len(workers) {
		return false
	}
	for _, w := range workers {
		if _, ok := commit.Payloads[w]; !ok {
			return false
		}
	}

	// (ii) source unchanged: revision fast-path, then content equality.
	if srcKnown && commit.SourceRevisionKnown && srcRev == commit.SourceRevision {
		return true
	}
	c.mu.RLock()
	last := c.lastPublishedPartitions
	c.mu.RUnlock()

	return last != nil && partitionsContentEqual(partitions, last)
}

// rebalanceOnCachedObservation handles a rebalance whose worker observation
// degraded to the cached list (connectivity error or suspicious shrink)
// WITHOUT emergency confirmation. The adjudicated contract: a non-fresh
// observation may REPLAY the previously committed shape but must never
// compute new label routing or mint new label provenance.
//
//   - Replay provable (cachedObservationReplayable): skip the publish
//     entirely — the previous commit already covers this exact state.
//     Success (nil).
//   - Otherwise: benign defer (errLabelObservationDeferred; converted to a
//     no-op by handleRebalance / triggerPartitionRebalance). Nothing is
//     published; a pending source change is not lost — any later rebalance
//     re-snapshots the source.
//
// Both exits request a label re-check (belt-and-braces; Task 9 gives the
// stub teeth) so the system converges to a fresh label-aware rebalance once
// conditions heal, even if no external event arrives. Neither exit advances
// labelState: grace/confirmation clocks only move on trusted observations.
func (c *Calculator) rebalanceOnCachedObservation( //nolint:revive // argument-limit: the rebalance-context bundle for the replay decision
	lifecycle string, workers []string, partitions []types.Partition, srcRev uint64, srcKnown bool, start time.Time,
) error {
	replayable := c.cachedObservationReplayable(workers, partitions, srcRev, srcKnown)
	c.requestLabelRecheck(reasonNonFreshObservation)
	c.Metrics.RecordRebalanceDuration(time.Since(start).Seconds(), lifecycle)
	c.Metrics.RecordRebalanceAttempt(lifecycle, true)

	if replayable {
		c.Logger.Info("rebalance skipped: cached worker observation already covered by the last published commit",
			"lifecycle", lifecycle, "workers", len(workers), "partitions", len(partitions))
		return nil
	}

	c.Logger.Info("rebalance deferred: cached worker observation not provably covered by the last published commit",
		"lifecycle", lifecycle, "workers", len(workers), "partitions", len(partitions))

	return errLabelObservationDeferred
}

// deriveRebalanceAssignments produces the assignment shape for a TRUSTED
// worker observation by running the label pipeline (routing + parked set +
// labels-of-record). Callers gate trust: a fresh enumeration, or the
// emergency carve-out (emergency-confirmed deaths filtered from the cached
// list).
//
// It owns the label-path side effects and their metrics: firing
// OnLabelReadBroadFailure on a broad read failure, arming the re-check timer on
// a deferral, and the attempt/duration metrics for the deferral and error
// exits. A non-nil error is terminal for the rebalance — either the benign
// errLabelObservationDeferred sentinel the caller maps to a no-op, or a wrapped
// hard failure — and its metrics are already recorded here.
//
// The result is only meaningful on a nil error. Its topo/actions fields let
// the caller's recordLabelMetrics pass report the label observability
// metrics (spec §13) from the exact decision that will be published, without
// recomputing (and re-mutating labelState's grace clocks) a second time.
func (c *Calculator) deriveRebalanceAssignments(
	ctx context.Context, lifecycle string, workers []string, partitions []types.Partition, start time.Time,
) (labelPipelineResult, error) {
	res, deferred, err := c.labelRoutedAssignments(ctx, workers, partitions)
	if err != nil {
		// Broad label-read failures (connectivity/degrading-JetStream class,
		// or above the isolated-failure cap) route to the manager's KV-error/
		// degraded machinery via the OnLabelReadBroadFailure seam (mirrors the
		// OnEnumerationError precedent). Nil-safe: unit-test default leaves it
		// unwired. Any label error aborts: a broad failure must never be
		// converted into a label decision that empty-assigns the fleet.
		//
		// Suppressed while stopping: Stop's stop-channel close cancels the
		// rebalance context, which makes the heartbeat reads abort "broadly"
		// — that is shutdown, not a KV failure, and must not pollute the
		// manager's degraded window. It also re-enters the manager through a
		// lock-taking callback (recordLabelReadFailure → recordKVError →
		// m.mu) exactly while Manager.Stop is tearing the calculator down —
		// the Task 15 shutdown-deadlock ingredient this guard removes
		// (defense-in-depth alongside stopCalculator's lock restructure,
		// which is the load-bearing fix: a GENUINE broad failure can still
		// race Stop and must not deadlock either).
		if errors.Is(err, errLabelReadBroadFailure) && c.OnLabelReadBroadFailure != nil && !c.isStopping() {
			c.OnLabelReadBroadFailure(err)
		}
		c.Metrics.RecordRebalanceAttempt(lifecycle, false)

		return labelPipelineResult{}, c.wrapStopErr(err)
	}
	if deferred {
		// First observation of a disruptive label condition: confirm before
		// acting. The label re-check timer (Task 9) re-fires the rebalance.
		c.requestLabelRecheck("observation_deferred")
		c.Metrics.RecordRebalanceDuration(time.Since(start).Seconds(), lifecycle)
		c.Metrics.RecordRebalanceAttempt(lifecycle, true)

		return labelPipelineResult{}, errLabelObservationDeferred
	}

	return res, nil
}

// labelPipelineResult bundles the outputs of one label-pipeline run
// (labelRoutedAssignments) for a trusted worker observation: everything the
// rebalance needs to publish (assignments, parked set, labels-of-record) plus
// the topology/actions the decision was made from, which the post-publish
// LabelMetrics pass (recordLabelMetrics) reports without recomputing.
type labelPipelineResult struct {
	assignments map[string][]types.Partition // worker → merged partitions
	parked      []types.Partition            // partitions parked awaiting an eligible worker
	labels      map[string][]string          // workerID → labels-of-record for payload stamping
	topo        labelTopology                // the topology the assignments were computed from
	actions     map[string]emptyPoolAction   // empty-pool decisions applied (park/spill per label)
}

// labelRoutedAssignments runs the label pipeline (spec §7) for a TRUSTED
// worker observation: reads labels-of-record, builds the pool topology,
// advances the grace/confirmation state, and computes per-pool assignments
// plus the parked set.
//
// deferred=true means a first disruptive label observation (an unreadable
// worker or a newly-empty pool) needs confirmation before acting; the caller
// aborts the rebalance with errLabelObservationDeferred and arms the re-check
// timer. A broad label-read failure returns an error wrapping
// errLabelReadBroadFailure; a strategy failure returns an error wrapped as
// "assignment calculation failed". On success it returns the pipeline's
// bundled outputs: the merged assignments, the parked set, the
// labels-of-record for payload stamping, and the topology/actions the
// decision was made from — the caller's LabelMetrics pass
// (recordLabelMetrics) reads these AFTER a successful publish rather than
// recomputing them, so it reports exactly what was applied.
func (c *Calculator) labelRoutedAssignments(ctx context.Context, workers []string, partitions []types.Partition) (labelPipelineResult, bool, error) {
	labels, unknown, err := c.readWorkerLabels(ctx, workers)
	if err != nil {
		return labelPipelineResult{}, false, err
	}

	unknownList := slices.Sorted(maps.Keys(unknown))
	unknownDeferred := c.labelState.observeUnknownWorkers(unknownList)

	topo := buildLabelTopology(topologyInput{
		Workers:    workers,
		Labels:     labels,
		Unknown:    unknown,
		Partitions: partitions,
		Policy:     c.UnlabeledPartitionPolicy,
	})

	nonEmpty := make([]string, 0, len(topo.SortedLabels))
	for _, l := range topo.SortedLabels {
		if len(topo.Pools[l]) > 0 {
			nonEmpty = append(nonEmpty, l)
		}
	}
	c.labelState.observeNonEmpty(nonEmpty)
	actions, emptyDeferred := c.labelState.observeEmptyPools(topo.EmptyLabels)

	currentLabels := make(map[string]bool, len(topo.SortedLabels))
	for _, l := range topo.SortedLabels {
		currentLabels[l] = true
	}
	c.labelState.prune(currentLabels)

	if unknownDeferred || emptyDeferred {
		return labelPipelineResult{}, true, nil
	}

	assignments, parked, err := computeLabelAssignments(c.Strategy, topo, actions)
	if err != nil {
		return labelPipelineResult{}, false, fmt.Errorf("assignment calculation failed: %w", err)
	}

	return labelPipelineResult{
		assignments: assignments,
		parked:      parked,
		labels:      labels,
		topo:        topo,
		actions:     actions,
	}, false, nil
}

// recordLabelMetrics emits the label-observability metrics (spec §13) for a
// rebalance that just published successfully. Per-label gauges are
// recomputed from the topology/parked snapshot the publish used; a label
// that left the snapshot since the last recorded pass is explicitly zeroed
// in this SAME pass rather than left at its last non-zero reading. The
// spill/fallback counters are derived from the same topo/actions — never
// recomputed — so they report exactly what was applied, and both count
// PARTITIONS (matching their interface Godoc), not rebalances:
//
//   - IncrementLabelSpill fires once per partition of every label whose
//     empty-pool decision was emptyPoolSpill in the published rebalance.
//   - IncrementUnlabeledFallback fires once per partition of the unlabeled
//     group when its general/fallback pool was empty and the group itself
//     was non-empty, forcing computeLabelAssignments' ladder down to
//     topo.AllWorkers.
//
// No-ops entirely when the configured collector doesn't implement
// types.LabelMetrics. Callers must only invoke this after a successful
// publish: the cached-observation replay/defer paths and the broad-failure/
// error aborts earlier in rebalance never reach here, so a rebalance that
// didn't publish records nothing.
func (c *Calculator) recordLabelMetrics(res labelPipelineResult) {
	lm, ok := c.Metrics.(types.LabelMetrics)
	if !ok {
		return
	}

	topo := res.topo
	parkedCountByLabel := make(map[string]int, len(topo.SortedLabels))
	for _, p := range res.parked {
		parkedCountByLabel[p.Label]++
	}

	current := make(map[string]bool, len(topo.SortedLabels))
	for _, l := range topo.SortedLabels {
		current[l] = true
		lm.RecordLabelPoolSize(l, len(topo.Pools[l]))
		lm.RecordParkedPartitions(l, parkedCountByLabel[l])
		if res.actions[l] == emptyPoolSpill {
			for range topo.Groups[l] {
				lm.IncrementLabelSpill(l)
			}
		}
	}
	// Zero gauges for labels that left the snapshot since the last recorded
	// pass — a drained label pool must not leave a stale non-zero reading.
	for l := range c.prevMetricLabels {
		if !current[l] {
			lm.RecordLabelPoolSize(l, 0)
			lm.RecordParkedPartitions(l, 0)
		}
	}
	c.prevMetricLabels = current

	if len(topo.GeneralPool) == 0 {
		for range topo.Groups[""] {
			lm.IncrementUnlabeledFallback()
		}
	}
}

// retainLabelReadModel stores the pull-side label snapshot for
// Manager.LabelState from the pipeline result a rebalance just published.
// Derivation matches recordLabelMetrics exactly (pool sizes from topo.Pools,
// parked counts from res.parked by label, keys = topo.SortedLabels), but it
// runs UNCONDITIONALLY — the pull accessor must work precisely when no
// LabelMetrics collector is wired. Callers must only invoke this after a
// successful publish, alongside recordLabelMetrics.
func (c *Calculator) retainLabelReadModel(res labelPipelineResult) {
	topo := res.topo
	model := &LabelReadModel{
		PoolSizes: make(map[string]int, len(topo.SortedLabels)),
		Parked:    make(map[string]int, len(topo.SortedLabels)),
	}
	parkedCountByLabel := make(map[string]int, len(topo.SortedLabels))
	for _, p := range res.parked {
		parkedCountByLabel[p.Label]++
	}
	for _, l := range topo.SortedLabels {
		model.PoolSizes[l] = len(topo.Pools[l])
		model.Parked[l] = parkedCountByLabel[l]
	}
	c.labelReadModel.Store(model)
}

// LabelSnapshot returns copies of the retained label read model, or ok=false
// when no rebalance has published since this calculator started. Lock-free;
// safe concurrently with Stop (the caller may observe either the snapshot or
// the cleared state, never a partial one).
func (c *Calculator) LabelSnapshot() (pools, parked map[string]int, ok bool) {
	model := c.labelReadModel.Load()
	if model == nil {
		return nil, nil, false
	}

	return maps.Clone(model.PoolSizes), maps.Clone(model.Parked), true
}

// zeroLabelGaugesOnStop closes out the leader-scoped gauge lifecycle (spec
// §13): a deposed/stopping leader zeroes every per-label gauge it last
// recorded so its own metrics export doesn't freeze at stale non-zero
// readings — the next leader's collector, not this one, owns the live
// values from here on. Called from Stop after all rebalance-driving
// goroutines have joined; rebalanceMu preserves prevMetricLabels' documented
// single-lock discipline against any straggling external TriggerRebalance.
func (c *Calculator) zeroLabelGaugesOnStop() {
	c.rebalanceMu.Lock()
	defer c.rebalanceMu.Unlock()

	lm, ok := c.Metrics.(types.LabelMetrics)
	if !ok {
		c.prevMetricLabels = nil

		return
	}
	for l := range c.prevMetricLabels {
		lm.RecordLabelPoolSize(l, 0)
		lm.RecordParkedPartitions(l, 0)
	}
	c.prevMetricLabels = nil
}

// cacheFallbackOrDegraded is the shared exit for getActiveWorkers' two degraded
// fallbacks (a connectivity enumeration error and a suspicious worker-shrink). On
// a cache hit it returns the last cached worker list as a NON-fresh observation
// (fresh=false, so the rebalance path knows not to trust it as current truth) and
// records cache usage + the reason-tagged fallback metric; on a miss it records
// the no_cache metric and surfaces degradedErr (a types.ErrDegraded wrap). onHit,
// when non-nil, runs on the hit path before the metrics — the connectivity path
// logs its Warn there, while the suspicious-shrink path already logged earlier and
// passes nil. Centralizing this keeps the fresh=false contract and the metric
// pair identical across both fallbacks.
func (c *Calculator) cacheFallbackOrDegraded(reason string, degradedErr error, onHit func(cached []string, age time.Duration)) ([]string, bool, error) {
	if cached, age, ok := c.getCachedWorkers(); ok {
		if onHit != nil {
			onHit(cached, age)
		}
		c.Metrics.RecordCacheUsage("workers", age.Seconds())
		c.Metrics.IncrementCacheFallback(reason)

		return cached, false, nil
	}
	c.Metrics.IncrementCacheFallback("no_cache")

	return nil, false, degradedErr
}

// recordEnumerationFailure increments the consecutive enumeration-failure counter
// and invokes OnEnumerationError once the threshold is reached/exceeded. Firing
// while already over the threshold is intentional and harmless: the manager's
// enterDegraded is idempotent (CAS short-circuits while degraded). Only the
// swallowed non-connectivity branch of getActiveWorkers calls this; connectivity
// errors stay owned by the existing circuit (heartbeat Put SetOnError).
func (c *Calculator) recordEnumerationFailure(err error) {
	if c.OnEnumerationError == nil {
		return
	}
	c.mu.Lock()
	c.enumFailures++
	fire := c.enumFailures >= c.EnumerationFailureThreshold
	c.mu.Unlock()
	if fire {
		c.OnEnumerationError(err)
	}
}

// resetEnumerationFailures clears the consecutive-failure counter and signals
// enumeration recovery. Called on every SUCCESSFUL enumeration — right after a
// nil-error GetActiveWorkers, before F10-A suspicious-shrink handling — NOT only
// on the fresh==true path (a recovered-but-suspicious scan must still clear).
func (c *Calculator) resetEnumerationFailures() {
	c.mu.Lock()
	c.enumFailures = 0
	c.mu.Unlock()
	if c.OnEnumerationSuccess != nil {
		c.OnEnumerationSuccess()
	}
}

// recordWorkerObservation runs the F10-A counter under c.mu. It
// returns whether the raw observation was suspicious, the resulting
// consecutive count, whether the observation should be surfaced as
// accepted (suspicious but past the confirmation window), and the
// last-known baseline (for the caller's warning log). A healthy
// non-shrunk observation resets the counter; a suspicious observation
// increments it. Splitting the locked read-modify-write from the
// caller's cache-fallback / metrics path keeps c.mu's critical section
// tight and lets the caller log without holding the lock.
func (c *Calculator) recordWorkerObservation(observed int) (suspicious bool, consec int, accepted bool, lastKnown int) { //nolint:revive // function-result-limit: caller needs all four signals atomically under c.mu
	c.mu.Lock()
	defer c.mu.Unlock()

	lastKnown = c.lastKnownWorkerCount
	suspicious = c.workerObservationSuspicious(observed)
	if !suspicious {
		c.workerShrunkObservations = 0
		return suspicious, 0, false, lastKnown
	}

	c.workerShrunkObservations++
	consec = c.workerShrunkObservations
	accepted = consec >= c.WorkerShrinkConfirmationCount

	return suspicious, consec, accepted, lastKnown
}

// shrinkSuspicious reports whether observed is sharply shrunk relative to a
// positive lastKnown baseline, i.e.
//
//	observed*100 < lastKnown*thresholdPct
//
// The multiplied form (rather than the pre-divided lastKnown*thresholdPct/100)
// avoids the intermediate-truncation surprise at small counts. Both shrink
// guards — worker heartbeat scans (workerObservationSuspicious) and
// partition-source observations (partitionInputCredibilityGuard) — share this
// one definition so the two cannot drift. Callers own the lastKnown == 0
// "baseline not yet established" case; each guard treats it differently.
func shrinkSuspicious(observed, lastKnown, thresholdPct int) bool {
	return observed*100 < lastKnown*thresholdPct
}

// workerObservationSuspicious returns true when a fresh worker-count
// observation is sharply shrunk relative to the last known baseline
// (see shrinkSuspicious). The guard is silent until lastKnownWorkerCount > 0 —
// the first ever scan must be trusted to establish the baseline.
func (c *Calculator) workerObservationSuspicious(observed int) bool {
	if c.lastKnownWorkerCount == 0 {
		return false
	}

	return shrinkSuspicious(observed, c.lastKnownWorkerCount, c.WorkerShrinkConfirmationThresholdPct)
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
//   - bool: true if the underlying observation was fresh from KV; false if cached.
//   - error: KV operation error
func (c *Calculator) getActiveWorkersFiltered(ctx context.Context, disappearedWorkers []string) ([]string, bool, error) {
	workers, fresh, err := c.getActiveWorkers(ctx)
	if err != nil {
		return nil, false, err
	}

	// Fast path: no filtering needed
	if len(disappearedWorkers) == 0 {
		return workers, fresh, nil
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

	return filtered, fresh, nil
}

// partitionInputCredibilityGuard implements the confirmation window for
// the minimum-credible-partition-input doctrine (see
// errSuspiciousPartitionObservation Godoc).
// Returns true when the observation is suspicious and the caller must skip
// the rebalance (the caller surfaces errSuspiciousPartitionObservation).
// Returns false when the observation is credible (the caller advances the
// baseline and proceeds). A healthy observation also resets the running
// suspicious-observations counter.
//
// The guard is silent until lastKnownPartitionCount > 0 — the first ever
// rebalance must run so the baseline can be established. Subsequent
// empty / sharply-shrunk observations require
// PartitionShrinkConfirmationCount consecutive sightings before being
// trusted.
func (c *Calculator) partitionInputCredibilityGuard(lifecycle string, observed int) bool {
	if c.lastKnownPartitionCount == 0 {
		return false // baseline not yet established; let the first rebalance run
	}
	switch {
	case observed == 0:
		c.partitionShrunkObservations++
		if c.partitionShrunkObservations < c.PartitionShrinkConfirmationCount {
			c.Logger.Warn("ignoring empty partition observation pending confirmation",
				"lifecycle", lifecycle,
				"last_known", c.lastKnownPartitionCount,
				"consecutive_observations", c.partitionShrunkObservations,
				"required", c.PartitionShrinkConfirmationCount)

			return true
		}
	case shrinkSuspicious(observed, c.lastKnownPartitionCount, c.PartitionShrinkConfirmationThresholdPct):
		c.partitionShrunkObservations++
		if c.partitionShrunkObservations < c.PartitionShrinkConfirmationCount {
			c.Logger.Warn("ignoring sharply-shrunk partition observation pending confirmation",
				"lifecycle", lifecycle,
				"last_known", c.lastKnownPartitionCount,
				"observed", observed,
				"threshold_pct", c.PartitionShrinkConfirmationThresholdPct,
				"consecutive_observations", c.partitionShrunkObservations,
				"required", c.PartitionShrinkConfirmationCount)

			return true
		}
	default:
		c.partitionShrunkObservations = 0 // healthy observation; reset counter
	}

	return false
}

// handleRebalance is the callback invoked by StateMachine when rebalancing should occur.
//
// This method bridges the StateMachine component to the Calculator's rebalancing logic
// and refreshes lastWorkers after a successful rebalance so the next poll does not
// re-enter scaling for the same topology.
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
		// A suspicious partition observation is an explicit "keep
		// cached assignment for now" decision, not a failure. Treat as
		// no-op so the state machine does not escalate. The rebalance
		// path already logged a WARN with the confirmation progress.
		if errors.Is(err, errSuspiciousPartitionObservation) {
			return nil
		}
		// A suspicious worker observation is the same shape: the
		// floor explicitly chose to keep the cached assignment until
		// EmergencyDetector confirms deaths or the heartbeat scan
		// heals. The next poll naturally re-observes; no escalation.
		if errors.Is(err, errSuspiciousWorkerObservation) {
			return nil
		}
		// A deferred label observation is an explicit "confirm before
		// acting" decision; the label re-check timer re-fires it. Benign.
		if errors.Is(err, errLabelObservationDeferred) {
			return nil
		}

		return fmt.Errorf("rebalance failed for %s: %w", reason, err)
	}

	// After successful rebalance, update lastWorkers to match currentWorkers.
	// This prevents immediately re-entering scaling on the next poll.
	c.mu.Lock()
	c.setLastWorkersLocked(c.currentWorkers)
	c.mu.Unlock()

	return nil
}

// collectRebalanceWorkers fetches the active worker set for a rebalance and
// maintains the emergency-detector invariant by calling ObserveAlive on the
// fresh scan result (gated on fresh==true).
//
// Only the emergency lifecycle consumes c.disappearedWorkers — other
// lifecycles must leave the buffer intact so a later emergency rebalance
// still sees the confirmed-disappeared workers.
//
// ObserveAlive keeps the detector consistent across out-of-poll rebalance
// triggers (audit_repair, scaling-timer, partition-lifecycle, manual): any
// worker visible to this fresh KV scan has its stale tracking cleared, so a
// subsequent disappearance starts a fresh grace period.
//
// Returns the worker set, plus a "deaths confirmed" flag for the F10-A
// rebalance-side floor and a "fresh" flag. The deaths-confirmed flag is
// true iff EmergencyDetector has captured at least one death — either
// consumed locally on the emergency lifecycle, or still resident in
// c.disappearedWorkers on the non-emergency lifecycle. The floor's check
// must see the pre-clear state so an emergency rebalance is never wrongly
// suppressed by its own consumption. The fresh flag is false when the
// observation was degraded to the cached worker list (connectivity error
// or suspicious shrink); the label pipeline gates on it because a cached
// set may contain departed workers whose heartbeats no longer decode.
func (c *Calculator) collectRebalanceWorkers(ctx context.Context, lifecycle string) ([]string, bool, bool, error) { //nolint:revive // function-result-limit: four coordinated rebalance-input signals
	var (
		disappearedWorkers []string
		deathsConfirmed    bool
	)
	if lifecycle == "emergency" {
		c.mu.Lock()
		disappearedWorkers = c.disappearedWorkers
		c.disappearedWorkers = nil
		c.mu.Unlock()
		deathsConfirmed = len(disappearedWorkers) > 0
	} else {
		c.mu.RLock()
		deathsConfirmed = len(c.disappearedWorkers) > 0
		c.mu.RUnlock()
	}

	workers, fresh, err := c.getActiveWorkersFiltered(ctx, disappearedWorkers)
	if err != nil {
		return nil, false, false, c.wrapStopErr(fmt.Errorf("failed to get active workers: %w", err))
	}

	if fresh {
		c.emergencyDetector.ObserveAlive(workers)
	}

	return workers, deathsConfirmed, fresh, nil
}

// snapshotSource picks the best partition snapshot path for the source. If
// the source implements RevisionedPartitionSource, it carries the source
// revision into the commit so the audit (Phase 4) can apply strict
// source-revision checks. Otherwise it falls back to List with srcRev=0
// and srcKnown=false.
func snapshotSource(ctx context.Context, src types.PartitionSource) ([]types.Partition, uint64, bool, error) { //nolint:revive // function-result-limit: mirrors RevisionedPartitionSource.Snapshot signature
	if rs, ok := src.(types.RevisionedPartitionSource); ok {
		parts, rev, known, err := rs.Snapshot(ctx)
		return parts, rev, known, err
	}
	parts, err := src.List(ctx)

	return parts, 0, false, err
}

// rebalance calculates and publishes new assignments.
//
//nolint:cyclop // already-large function; the credibility-guard branch adds one more — refactoring is a separate effort
func (c *Calculator) rebalance(ctx context.Context, lifecycle string) error {
	// Serialize rebalance operations to prevent race conditions
	c.rebalanceMu.Lock()
	defer c.rebalanceMu.Unlock()

	// Partition-lifecycle grace re-check (PR-1 / ISSUE-005 P0-A): the
	// drain ticker / immediate watch arm already check IsInRecoveryGrace
	// before calling here, but grace can flip to true between that check
	// and rebalanceMu acquisition. Atomic under rebalanceMu.
	if c.shouldDeferForRecoveryGrace(lifecycle) {
		return errShuttingDown
	}

	// Stop-aware ctx (PR-1 / ISSUE-002 + ISSUE-003 Layer 1): downstream
	// ctx-aware operations (getActiveWorkersFiltered, snapshotSource,
	// publisher.Publish) short-circuit on Stop instead of running to
	// completion against the detached background timeout.
	ctx, cancel := ctxFromStopCh(ctx, c.stopCh, 0)
	defer cancel()

	start := time.Now()

	workers, deathsConfirmed, workersFresh, err := c.collectRebalanceWorkers(ctx, lifecycle)
	if err != nil {
		c.Metrics.RecordRebalanceAttempt(lifecycle, false)
		return err
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

	partitions, srcRev, srcKnown, snapErr := snapshotSource(ctx, c.Source)
	if snapErr != nil {
		c.Metrics.RecordRebalanceAttempt(lifecycle, false)
		return c.wrapStopErr(fmt.Errorf("failed to snapshot partitions: %w", snapErr))
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

	// F10-A worker-set floor: refuse to commit a rebalance on a
	// sharply-shrunk worker set when EmergencyDetector has not
	// confirmed any deaths. Composes with — does not duplicate — the
	// per-worker grace window in EmergencyDetector: once any death is
	// confirmed (c.disappearedWorkers populated) the floor releases
	// and the rebalance proceeds. The in-getActiveWorkers defense
	// covers the per-poll noise filtering; the floor here is the
	// emergency-confirmation gate that must clear before the
	// calculator commits to a shrunk worker set. lastKnownWorkerCount
	// is advanced ONLY when the floor passes, so it always reflects a
	// baseline confirmed by a prior successful rebalance.
	c.mu.Lock()
	floorBlocks := !deathsConfirmed && c.workerObservationSuspicious(len(workers))
	lastKnownSnapshot := c.lastKnownWorkerCount
	if !floorBlocks {
		c.lastKnownWorkerCount = len(workers)
	}
	c.mu.Unlock()

	if floorBlocks {
		c.Logger.Warn("ignoring rebalance on suspiciously-shrunk worker set; no emergency-confirmed deaths",
			"lifecycle", lifecycle,
			"current_count", len(workers),
			"last_known_count", lastKnownSnapshot,
			"threshold_pct", c.WorkerShrinkConfirmationThresholdPct)
		c.Metrics.RecordRebalanceDuration(time.Since(start).Seconds(), lifecycle)
		c.Metrics.RecordRebalanceAttempt(lifecycle, true)

		return errSuspiciousWorkerObservation
	}

	// Minimum-credible-partition-input guard (see
	// errSuspiciousPartitionObservation Godoc).
	if suspicious := c.partitionInputCredibilityGuard(lifecycle, len(partitions)); suspicious {
		c.Metrics.RecordRebalanceDuration(time.Since(start).Seconds(), lifecycle)
		c.Metrics.RecordRebalanceAttempt(lifecycle, true)

		return errSuspiciousPartitionObservation
	}
	c.lastKnownPartitionCount = len(partitions)

	// Trusted-observation gate. The label pipeline computes NEW routing only
	// from a trustworthy worker set: a fresh enumeration, or the emergency
	// carve-out — emergency-CONFIRMED deaths filtered out of the cached list
	// make the survivor set trusted (EmergencyDetector independently verified
	// the removals; the per-worker label reads below are fresh reads, so
	// nothing is fabricated, and a broad read failure still aborts — never
	// launder). Anything else (connectivity fallback, suspicious-shrink
	// degrade) must not mint new routing or provenance: replay the previous
	// commit if provable, else defer benignly.
	trusted := workersFresh || (lifecycle == "emergency" && deathsConfirmed)
	if !trusted {
		return c.rebalanceOnCachedObservation(lifecycle, workers, partitions, srcRev, srcKnown, start)
	}

	// Assignment shape for this rebalance: routing, the parked set, and the
	// labels-of-record for payload stamping, from the label pipeline. A
	// non-nil error is terminal (a benign deferral sentinel or a wrapped
	// hard failure) and its metrics are already recorded.
	pipeline, derr := c.deriveRebalanceAssignments(ctx, lifecycle, workers, partitions, start)
	if derr != nil {
		return derr
	}
	assignments, parked, labels := pipeline.assignments, pipeline.parked, pipeline.labels

	// Coverage is enforced strictly inside the publisher via set-equality of
	// partition CanonicalIDs (§3.8 of the assignment robustness plan). We
	// keep the orphaned-partition gauge for ops visibility but rely on the
	// publisher to abort the batch on mismatch with ErrCoverageMismatch.
	// Parked partitions are deliberately unassigned, so the orphan gauge
	// compares assigned against the ELIGIBLE count (source minus parked).
	assignedCount := 0
	for _, parts := range assignments {
		assignedCount += len(parts)
	}
	eligible := len(partitions) - len(parked)
	if assignedCount != eligible {
		c.Metrics.RecordOrphanedPartitions(eligible - assignedCount)
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

	// Publish assignments via the refs-always commit flow (§3.5).
	if err := c.publisher.Publish(ctx, PublishInput{
		Workers:             workers,
		Assignments:         assignments,
		SourcePartitions:    partitions,
		SourceRevision:      srcRev,
		SourceRevisionKnown: srcKnown,
		LeaderRevision:      leaderRevision,
		Lifecycle:           lifecycle,
		WorkersToRemove:     workersToRemove,
		ParkedPartitions:    parked,
		WorkerLabels:        labels,
	}); err != nil {
		c.Metrics.RecordRebalanceAttempt(lifecycle, false)
		return c.wrapPublishErr(err)
	}

	// Record successful rebalance
	c.Metrics.RecordRebalanceDuration(time.Since(start).Seconds(), lifecycle)
	c.Metrics.RecordRebalanceAttempt(lifecycle, true)

	// Label observability (spec §13). Placed strictly after the publish
	// succeeded above: the cached-observation replay/defer paths and the
	// broad-failure/error aborts earlier in this function return before
	// reaching here, so a rebalance that didn't publish records nothing.
	c.recordLabelMetrics(pipeline)
	c.retainLabelReadModel(pipeline)

	// Arm/disarm the label re-check timer for any parked grace (Task 9
	// provides the body; the stub is a no-op).
	c.armLabelRecheckAfterRebalance(len(parked))

	// Update tracking state. lastPublishedPartitions backs the
	// cached-observation replay proof (see the field Godoc).
	c.mu.Lock()
	clear(c.currentWorkers)
	for _, w := range workers {
		c.currentWorkers[w] = true
	}
	c.currentAssignments = assignments
	c.lastPublishedPartitions = partitions
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
