package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/parti/v2/test/simulation/internal/metrics"
	"github.com/arloliu/parti/v2/test/simulation/internal/producer"
)

// Coordinator manages simulation tracking and validation.
type Coordinator struct {
	tracker          *MessageTracker
	metricsCollector *metrics.Collector
	sentCh           chan producer.ReportMessage
	receivedCh       chan ReceivedMessage
	stopCh           chan struct{}
	// duplicate tracing
	dup DupTracer

	// assignment tracking
	assignmentsCh       chan AssignmentReport
	stoppedWorkersCh    chan string // signals a worker goroutine has permanently stopped
	startLatenciesCh    chan StartLatencyReport
	workerAssignments   map[string]map[int]struct{}
	prevGlobalAssigned  map[int]struct{}
	prevPartitionOwners map[int]string // partitionID -> workerID for previous snapshot
	activeOwners        map[int]string // partitionID -> workerID based on actual processing reports
	ownersMu            sync.RWMutex   // guards activeOwners

	// ownership-violation evidence: bounded slice of cross-worker same-seq
	// observations. Surfaced verbatim in FailureReport.OwnershipViolations.
	// Bounded to ownershipViolationsCap entries to keep failure reports
	// from ballooning under chaos cascades.
	//
	// concurrentOwnerEvents indexes the subset of ownershipViolations
	// where the owner snapshot reported >1 current owner (
	// split-brain signal); same backing data, separately enumerable.
	//
	// inconclusiveOwnerEvents records cross-worker duplicates where the
	// owner snapshot was initialized but reported no current owner —
	// classifications that are non-fatal but still block Outcome A.
	ownershipViolationsMu   sync.Mutex
	ownershipViolations     []MessageOwnershipViolationError
	concurrentOwnerEvents   []MessageOwnershipViolationError
	inconclusiveOwnerEvents []ClassificationEvidence
	ownershipViolationsCap  int

	// start-latency tracking: time from Worker.Start() to first OnAssignmentChanged with
	// a non-empty partition set. Surfaces regressions of the leader-takeover hang fix
	// (commit 5345527) and stale-assignment-sweep fix (commit 7781b9b) — both cause
	// replacement workers to block in waitForAssignment longer than expected.
	startLatencyMu          sync.Mutex
	initialStartLatencies   map[string]time.Duration // first assignment arrived before coldStartComplete
	takeoverStartLatencies  map[string]time.Duration // first assignment arrived after coldStartComplete
	slowStartBudgetInitial  time.Duration            // threshold before cold-start completes
	slowStartBudgetTakeover time.Duration            // threshold after cold-start completes
	totalPartitions         int
	expectedWorkers         int // optional hint for expected worker count
	initialReporters        map[string]struct{}
	baselineLockTimeout     time.Duration
	baselineLocked          bool
	// finalCoverageAchieved is set once we've observed a union of assigned
	// partitions covering the full expected partition space. Before this point
	// (during cold start convergence) we suppress reporting partially assigned
	// partitions as "unassigned" to avoid transient false positives caused by
	// staggered worker startups. Once true, normal unassigned calculation is
	// used for subsequent scaling events.
	finalCoverageAchieved bool
	firstAssignmentAt     time.Time // timestamp of first assignment snapshot (used for convergence timing)

	// recovery tracking
	recoveryActive bool
	recoveryStart  time.Time

	// stable locality tracking
	lastStableOwners  map[int]string
	lastStableAt      time.Time
	stableQuietWindow time.Duration

	// SLO tracking (optional)
	sloMu            sync.RWMutex
	sloHoleMaxAge    time.Duration
	sloExceedTicks   int64
	sloTotalTicks    int64
	sloMaxOldestHole time.Duration

	// Catch-up SLO tracking
	catchMu                 sync.RWMutex
	catchUpEnabled          bool
	catchUpDeadline         time.Duration
	catchUpPercent          int
	catchUpAbsenceThreshold time.Duration
	workerLastSeen          map[string]time.Time
	workerRecovery          map[string]*workerRecoveryContext
	catchUpTotalRecoveries  int64
	catchUpTotalExceed      int64
	catchUpLatencySum       time.Duration
	catchUpLatencyMax       time.Duration
	catchUpLatencyMin       time.Duration
	catchUpMaxInitialHoles  int

	// failure handling
	stopOnFailure     bool
	failureReportPath string
	failureOnce       sync.Once
	stopOnce          sync.Once // guards close(stopCh) — both Start(ctx) and TriggerFailure can race to close on shutdown.

	// ownerSnap is an immutable per-partition owner snapshot published
	// by processAssignments. The receipt path reads it via
	// CurrentOwnersOf without locks. See docs/plans/sim-oracle-phase5/00-plan.md §1.
	ownerSnap atomic.Pointer[ownerSnapshot]

	// chaosOnce ensures firstChaosEventAt is captured exactly once when
	// MarkChaosStarted is called. The chaosStarted flag is on the
	// tracker (atomic.Bool) so the classifier can read it lock-free.
	//
	// firstChaosEventAt is published via atomic.Pointer because it is
	// read from goroutines other than the chaos-dispatch one (failure
	// report writer, external observers via FirstChaosEventAt).
	chaosOnce         sync.Once
	firstChaosEventAt atomic.Pointer[time.Time]

	// Shutdown oracles — nil until EnableShutdownOracles is called.
	snapshotOverlap *SnapshotOverlapClassifier
	leaderUniq      *LeaderUniquenessWatcher
	stateReconcile  *StateReconcileWatcher

	// Phase 1: bucket-lifecycle chaos oracle. Lazy-initialized in
	// EnableShutdownOracles so callers that already use Phase 0 oracles
	// pick this one up automatically.
	degradedReasonOracle *DegradedReasonOracle
	degradedReportsCh    chan WorkerDegradedReport
}

// ownerSnapshot is an immutable view of partition→workers at a moment
// in time. Built and atomically published by processAssignments;
// consumed by the tracker via the owner-lookup callback. Slices in
// perPartition are sorted (deterministic test output) and MUST NOT be
// mutated after the snapshot pointer is stored — subsequent updates
// build and publish a new snapshot.
type ownerSnapshot struct {
	perPartition map[int][]string
	initialized  bool
	asOf         time.Time
}

// workerRecoveryContext tracks a single worker's backlog healing progress.
type workerRecoveryContext struct {
	startedAt      time.Time
	deadline       time.Duration
	percentTarget  int
	initialBacklog int64
	processed      int64
	partitions     map[int]*partitionRecoveryState
	completed      bool
	exceed         bool
}

type partitionRecoveryState struct {
	startSeq  int64
	targetSeq int64
	processed int64
}

// StartRecovery marks the beginning of a recovery interval (e.g., leader/network failure).
// Safe to call multiple times; subsequent calls while active are ignored.
func (c *Coordinator) StartRecovery(reason string) {
	if c.recoveryActive {
		return
	}
	c.recoveryActive = true
	c.recoveryStart = time.Now()
	log.Printf("[Coordinator] Recovery started (reason=%s)", reason)
	// Reflect degraded state on dashboards by clearing cold start readiness.
	if c.metricsCollector != nil {
		c.metricsCollector.SetColdStartComplete(false)
	}
}

// ReceivedMessage reports a received message from workers.
type ReceivedMessage struct {
	PartitionID       int
	PartitionSequence int64
	WorkerID          string
}

// NewCoordinator creates a new coordinator.
//
// Parameters:
//   - metricsCollector: Optional metrics collector (can be nil)
//
// Returns:
//   - *Coordinator: Initialized coordinator
//
// AssignmentReport reports a worker's current partition set after a rebalance.
type AssignmentReport struct {
	WorkerID   string
	Partitions []int
}

// StartLatencyReport reports the latency from Worker.Start() to the first
// OnAssignmentChanged hook firing with a non-empty partition set. It is
// emitted exactly once per worker instance.
//
// IsInitialCohort is set by the caller at worker construction: true for
// workers spawned during simulation bootstrap, false for workers created
// via scale_up or restart. The coordinator uses this flag (not the
// receive-time view of baselineLocked) to decide which budget applies,
// closing a race where a chaos-delayed initial-cohort worker's report
// arrives after baselineLocked has already flipped and is misbucketed
// as takeover.
type StartLatencyReport struct {
	WorkerID        string
	Latency         time.Duration
	IsInitialCohort bool
}

// NewCoordinator creates a new coordinator.
//
// Parameters:
//   - totalPartitions: Total number of partitions expected (0 disables assignment completeness metrics)
//   - metricsCollector: Optional metrics collector (can be nil)
//   - dupCfg: Duplicate tracing configuration
//   - stopOnFailure: Halt simulation on gap detection
//   - failureReportPath: Path to write failure report JSON
//
// Returns:
//   - *Coordinator: Initialized coordinator
func NewCoordinator(totalPartitions int, metricsCollector *metrics.Collector, dupCfg DupTraceSettings, stopOnFailure bool, failureReportPath string) *Coordinator {
	c := &Coordinator{
		tracker:                 NewMessageTracker(),
		metricsCollector:        metricsCollector,
		sentCh:                  make(chan producer.ReportMessage, 10000),
		receivedCh:              make(chan ReceivedMessage, 10000),
		stopCh:                  make(chan struct{}),
		assignmentsCh:           make(chan AssignmentReport, 1000),
		stoppedWorkersCh:        make(chan string, 256),
		startLatenciesCh:        make(chan StartLatencyReport, 256),
		degradedReportsCh:       make(chan WorkerDegradedReport, 256),
		initialStartLatencies:   make(map[string]time.Duration),
		takeoverStartLatencies:  make(map[string]time.Duration),
		slowStartBudgetInitial:  25 * time.Second, // sim ColdStartWindow(10s) + election + chaos-burst headroom
		slowStartBudgetTakeover: 20 * time.Second, // PlannedScaleWindow(10s) + election + chaos-burst headroom; still well below pre-fix ColdStartWindow(30s)
		workerAssignments:       make(map[string]map[int]struct{}),
		prevGlobalAssigned:      make(map[int]struct{}),
		activeOwners:            make(map[int]string),
		totalPartitions:         totalPartitions,
		initialReporters:        make(map[string]struct{}),
		baselineLockTimeout:     5 * time.Second,
		recoveryActive:          false,
		stableQuietWindow:       10 * time.Second,
		workerLastSeen:          make(map[string]time.Time),
		workerRecovery:          make(map[string]*workerRecoveryContext),
		stopOnFailure:           stopOnFailure,
		failureReportPath:       failureReportPath,
		ownershipViolationsCap:  1000,
	}
	c.dup = NewDupTracer(dupCfg)
	// Suppress verbose out-of-order logs by default; metrics capture disorder depth.
	c.tracker.SetLogOutOfOrder(false)
	// Initialize the owner-snapshot to a non-nil zero so CurrentOwnersOf
	// never sees a nil pointer. processAssignments will publish updated
	// snapshots as AssignmentReports arrive.
	c.ownerSnap.Store(&ownerSnapshot{perPartition: map[int][]string{}, initialized: false})
	// Install the discriminator on the tracker.
	c.tracker.SetOwnerLookup(c.CurrentOwnersOf)

	return c
}

// CurrentOwnersOf returns the workerIDs currently reporting partitionID
// in their most-recent OnAssignmentChanged snapshot, plus a flag
// indicating whether the snapshot has been initialized.
//
// snapshotInitialized is false during cold start, before any
// AssignmentReport has been processed.
//
// The returned slice is part of an immutable snapshot. Callers MUST
// NOT mutate it. Concurrency-safe: backed by atomic.Pointer load.
func (c *Coordinator) CurrentOwnersOf(partitionID int) (owners []string, snapshotInitialized bool) {
	snap := c.ownerSnap.Load()
	if snap == nil {
		return nil, false
	}
	return snap.perPartition[partitionID], snap.initialized
}

// MarkChaosStarted notifies the coordinator that the first ChaosEvent
// has fired. Must be called by the chaos dispatch loop BEFORE invoking
// the chaos handler for the first event. Idempotent — subsequent calls
// are no-ops. Safe to call concurrently.
func (c *Coordinator) MarkChaosStarted() {
	c.chaosOnce.Do(func() {
		now := time.Now()
		c.firstChaosEventAt.Store(&now)
	})
	c.tracker.MarkChaosStarted()
	if c.leaderUniq != nil {
		c.leaderUniq.MarkChaosStarted()
	}
	if c.stateReconcile != nil {
		c.stateReconcile.MarkChaosStarted()
	}
}

// EnableShutdownOracles constructs the three foundation oracles (snapshot-overlap,
// leader-uniqueness, state-reconcile) and attaches them to the coordinator.
// Must be called BEFORE coord.Start(). Idempotent — subsequent calls are no-ops.
// registry is the GoroutineRegistry that maps worker IDs to *worker.Worker objects.
func (c *Coordinator) EnableShutdownOracles(registry *GoroutineRegistry) {
	if c.snapshotOverlap != nil {
		return
	}
	c.snapshotOverlap = NewSnapshotOverlapClassifier(5*time.Second, registry)
	c.leaderUniq = NewLeaderUniquenessWatcher(registry)
	c.stateReconcile = NewStateReconcileWatcher(registry, 30*time.Second)
	c.degradedReasonOracle = NewDegradedReasonOracle()
}

// DegradedReasonOracle returns the Phase 1 oracle. nil if EnableShutdownOracles
// was never called. Callers (chaos dispatch handlers) register expectations
// via this accessor.
func (c *Coordinator) DegradedReasonOracle() *DegradedReasonOracle {
	return c.degradedReasonOracle
}

// GetDegradedReportsChannel returns the channel workers use to publish
// WorkerDegradedReport events. The coordinator drains this channel and
// forwards each report to the DegradedReasonOracle.
func (c *Coordinator) GetDegradedReportsChannel() chan<- WorkerDegradedReport {
	return c.degradedReportsCh
}

// GetExpectedDegradedObserved returns the Phase 1 oracle's "observed" counter.
// Returns 0 if EnableShutdownOracles was never called.
func (c *Coordinator) GetExpectedDegradedObserved() int64 {
	if c.degradedReasonOracle == nil {
		return 0
	}
	return c.degradedReasonOracle.ExpectedDegradedObserved()
}

// GetExpectedDegradedMissing returns the Phase 1 oracle's "missing" counter.
func (c *Coordinator) GetExpectedDegradedMissing() int64 {
	if c.degradedReasonOracle == nil {
		return 0
	}
	return c.degradedReasonOracle.ExpectedDegradedMissing()
}

// GetUnexpectedClaimLostShutdown returns the Phase 1 oracle's claim-lost
// shutdown counter.
func (c *Coordinator) GetUnexpectedClaimLostShutdown() int64 {
	if c.degradedReasonOracle == nil {
		return 0
	}
	return c.degradedReasonOracle.UnexpectedClaimLostShutdown()
}

// GetSnapshotOverlapCount returns the total snapshot-overlap violations detected
// by the oracle. Returns 0 if EnableShutdownOracles was never called.
func (c *Coordinator) GetSnapshotOverlapCount() int64 {
	if c.snapshotOverlap == nil {
		return 0
	}
	return c.snapshotOverlap.ViolationCount()
}

// GetDoubleLeaderObservations returns the total (pre-chaos) double-leader
// observations detected by the oracle. Returns 0 if EnableShutdownOracles
// was never called.
func (c *Coordinator) GetDoubleLeaderObservations() int64 {
	if c.leaderUniq == nil {
		return 0
	}
	return c.leaderUniq.DoubleLeaderObservations()
}

// GetStateReconcileViolations returns the total (pre-chaos) state-reconcile
// violations detected by the oracle. Returns 0 if EnableShutdownOracles was
// never called.
func (c *Coordinator) GetStateReconcileViolations() int64 {
	if c.stateReconcile == nil {
		return 0
	}
	return c.stateReconcile.StateReconcileViolations()
}

// FirstChaosEventAt returns the timestamp captured when MarkChaosStarted
// was first invoked. Zero value if chaos has not yet started.
func (c *Coordinator) FirstChaosEventAt() time.Time {
	t := c.firstChaosEventAt.Load()
	if t == nil {
		return time.Time{}
	}
	return *t
}

// rebuildOwnerSnapshotLocked rebuilds and publishes an immutable owner
// snapshot from c.workerAssignments. Caller must be on the
// processAssignments goroutine (sole writer of workerAssignments).
//
// Always sets initialized=true: this function is only invoked from
// inside processAssignments after consuming an AssignmentReport or
// stopped-worker event, so reaching this point means at least one
// report has been processed. The empty-snapshot case (no workers
// currently own any partition) is a known state, not unknown — the
// spec's snapshotInitialized==false regime applies strictly to the
// cold-start window before any report has been ingested.
func (c *Coordinator) rebuildOwnerSnapshotLocked() {
	perPartition := make(map[int][]string)
	for wid, pset := range c.workerAssignments {
		for pid := range pset {
			perPartition[pid] = append(perPartition[pid], wid)
		}
	}
	for pid := range perPartition {
		slices.Sort(perPartition[pid])
	}
	c.ownerSnap.Store(&ownerSnapshot{
		perPartition: perPartition,
		initialized:  true,
		asOf:         time.Now(),
	})
}

// SetExpectedWorkers sets an optional expected worker count hint for baseline observations.
// This does not alter gating behavior by itself; it is used for informational logs
// and future aggregation refinements.
func (c *Coordinator) SetExpectedWorkers(n int) {
	if n <= 0 {
		return
	}
	c.expectedWorkers = n
	log.Printf("[Coordinator] Expected workers set to %d", n)
}

// GetSentChannel returns the channel for reporting sent messages.
//
// Returns:
//   - chan<- producer.ReportMessage: Channel for producer reports
func (c *Coordinator) GetSentChannel() chan<- producer.ReportMessage {
	return c.sentCh
}

// GetReceivedChannel returns the channel for reporting received messages.
//
// Returns:
//   - chan<- ReceivedMessage: Channel for worker reports
func (c *Coordinator) GetReceivedChannel() chan<- ReceivedMessage {
	return c.receivedCh
}

// GetAssignmentsChannel returns the channel workers use to report assignment snapshots.
//
// Returns:
//   - chan<- AssignmentReport: Channel for assignment reports
func (c *Coordinator) GetAssignmentsChannel() chan<- AssignmentReport {
	return c.assignmentsCh
}

// GetStartLatencyChannel returns the channel workers use to report their
// first-assignment latency. Workers send a single report per instance when
// their OnAssignmentChanged hook fires for the first time with a non-empty
// partition set.
//
// Returns:
//   - chan<- StartLatencyReport: Channel for per-worker start-latency reports
func (c *Coordinator) GetStartLatencyChannel() chan<- StartLatencyReport {
	return c.startLatenciesCh
}

// ConfigureSlowStartBudgets sets thresholds used to flag workers whose first
// assignment arrived too late. Separate budgets apply before vs. after cold
// start completes:
//   - initial: applied while the cluster is still converging for the first time
//     (expected to absorb ColdStartWindow + election + startup jitter).
//   - takeover: applied after cold-start convergence, when a new worker joining
//     an already-warm cluster should be picked up within PlannedScaleWindow
//     (this is the budget that regresses if fix 5345527 is reverted).
//
// Values <= 0 leave the existing default in place.
func (c *Coordinator) ConfigureSlowStartBudgets(initial, takeover time.Duration) {
	c.startLatencyMu.Lock()
	defer c.startLatencyMu.Unlock()
	if initial > 0 {
		c.slowStartBudgetInitial = initial
	}
	if takeover > 0 {
		c.slowStartBudgetTakeover = takeover
	}
}

// NotifyWorkerStopped signals that a worker goroutine has permanently stopped
// (e.g., scale-down). Its last reported assignment snapshot is pruned from the
// coordinator's tracking so ownership-audit telemetry does not attribute
// partitions to a dead worker whose entries would otherwise persist forever
// because a dead worker cannot send a final OnAssignmentChanged hook with an
// empty set.
//
// Best-effort: drops the notification if the internal buffer is full. A
// restarted worker reclaiming the same ID will re-populate its snapshot via
// its normal OnAssignmentChanged hook, so pruning here is always safe.
func (c *Coordinator) NotifyWorkerStopped(workerID string) {
	if workerID == "" {
		return
	}
	select {
	case c.stoppedWorkersCh <- workerID:
	default:
		log.Printf("[Coordinator] stoppedWorkersCh full, dropping stop notification for %s", workerID)
	}
}

// Start begins coordinator tracking.
//
// Parameters:
//   - ctx: Context for cancellation
func (c *Coordinator) Start(ctx context.Context) {
	log.Println("[Coordinator] Starting")

	// Drain sent and received channels concurrently to avoid starvation when one side is high-volume.
	go c.processSentMessages(ctx)

	go c.processReceivedMessages(ctx)

	// Periodically publish coordinator gauges (in-flight and pending holes)
	if c.metricsCollector != nil {
		go c.runMetricsTicker(ctx)
	}

	// Process assignment reports to derive completeness & locality metrics.
	// This goroutine also drains start-latency reports to classify them with
	// the same goroutine's view of baselineLocked (no cross-goroutine races).
	if c.metricsCollector != nil && c.totalPartitions > 0 {
		go c.processAssignments(ctx)
	}

	// Start shutdown oracle background pollers (no-ops when oracles are nil).
	if c.leaderUniq != nil {
		c.leaderUniq.Run(ctx, 500*time.Millisecond)
	}
	if c.stateReconcile != nil {
		c.stateReconcile.Run(ctx, 10*time.Second)
	}
	if c.degradedReasonOracle != nil {
		c.degradedReasonOracle.Run(ctx, 1*time.Second)
		go c.processDegradedReports(ctx)
		// Background watcher that classifies any worker transition to
		// Shutdown state (manager State()==StateShutdown) for the
		// claim-lost invariant.
		go c.watchClaimLostShutdowns(ctx)
	}

	<-ctx.Done()
	log.Println("[Coordinator] Stopping")
	c.closeStopCh()
}

// closeStopCh safely closes c.stopCh exactly once. Both the normal
// Start(ctx) shutdown path and TriggerFailure can fire under shutdown
// (the shutdown-invariant TriggerFailure runs alongside ctx.Done),
// and a Go channel double-close panics. The sync.Once guard makes the
// close idempotent across all paths.
func (c *Coordinator) closeStopCh() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
}

// GetStats returns current statistics.
//
// Returns:
//   - TrackerStats: Current statistics
func (c *Coordinator) GetStats() TrackerStats {
	return c.tracker.GetStats()
}

// GetTracker returns the message tracker for checkpointing.
//
// Returns:
//   - *MessageTracker: The message tracker
func (c *Coordinator) GetTracker() *MessageTracker {
	return c.tracker
}

// SetSLOHoleMaxAge sets the SLO threshold for the maximum acceptable age of the oldest pending hole.
// A value <= 0 disables SLO sampling.
//
// Parameters:
//   - d: Duration threshold; oldest pending hole age exceeding this value will be counted as an exceedance
func (c *Coordinator) SetSLOHoleMaxAge(d time.Duration) {
	c.sloMu.Lock()
	defer c.sloMu.Unlock()
	if d <= 0 {
		c.sloHoleMaxAge = 0
		c.sloExceedTicks = 0
		c.sloTotalTicks = 0
		c.sloMaxOldestHole = 0
		return
	}
	c.sloHoleMaxAge = d
}

// ConfigureCatchUpSLO configures catch-up SLO parameters.
// Parameters:
//   - enabled: enable catch-up tracking
//   - deadline: max acceptable latency (<=0 disables exceed classification)
//   - percent: healing percent target (0 => 100%)
//   - absence: inactivity threshold to treat next activity as recovery
func (c *Coordinator) ConfigureCatchUpSLO(enabled bool, deadline time.Duration, percent int, absence time.Duration) {
	c.catchMu.Lock()
	c.catchUpEnabled = enabled
	c.catchUpDeadline = deadline
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	c.catchUpPercent = percent
	c.catchUpAbsenceThreshold = absence
	// Reset aggregate metrics if disabled
	if !enabled {
		c.workerRecovery = make(map[string]*workerRecoveryContext)
	}
	c.catchMu.Unlock()
}

// startWorkerCatchUp initializes recovery context for a worker if absent and now active.
func (c *Coordinator) startWorkerCatchUp(workerID string) {
	c.catchMu.Lock()
	if !c.catchUpEnabled {
		c.catchMu.Unlock()
		return
	}
	if _, exists := c.workerRecovery[workerID]; exists { // already tracking
		c.catchMu.Unlock()
		return
	}
	// Build partition backlog snapshot
	assigned := c.workerAssignments[workerID]
	rec := &workerRecoveryContext{
		startedAt:     time.Now(),
		deadline:      c.catchUpDeadline,
		percentTarget: c.catchUpPercent,
		partitions:    make(map[int]*partitionRecoveryState),
	}
	var totalBacklog int64
	var oldestHoleAge time.Duration
	for pid := range assigned {
		sent, received := c.tracker.GetPartitionState(pid)
		lag := sent - received
		if lag > 0 {
			rec.partitions[pid] = &partitionRecoveryState{
				startSeq:  received,
				targetSeq: sent,
			}
			totalBacklog += lag
		}

		// Also check holes for age metrics
		count, oldest := c.tracker.GetPartitionHoleSnapshot(pid)
		if count > 0 {
			if oldest > oldestHoleAge {
				oldestHoleAge = oldest
			}
		}
	}
	rec.initialBacklog = totalBacklog
	if totalBacklog == 0 {
		// No backlog at start; skip creating a recovery context.
		log.Printf("[CatchUp] Worker %s absence ended but no backlog; skipping recovery start", workerID)
		c.catchMu.Unlock()
		return
	}
	// Record metrics
	if c.metricsCollector != nil {
		c.metricsCollector.SetWorkerCatchUpBacklog(int(totalBacklog))
		if oldestHoleAge > 0 {
			c.metricsCollector.ObserveHoleAgeAtRecovery(oldestHoleAge)
		}
	}
	c.workerRecovery[workerID] = rec
	log.Printf("[CatchUp] Recovery started for worker %s: backlog=%d parts=%d deadline=%v target=%d%%", workerID, totalBacklog, len(rec.partitions), rec.deadline, rec.percentTarget)
	c.catchMu.Unlock()
}

// processCatchUpActivity records activity timestamp and decides if a recovery should start.
func (c *Coordinator) processCatchUpActivity(workerID string) {
	if !c.catchUpEnabled {
		return
	}
	now := time.Now()
	c.catchMu.Lock()
	last, ok := c.workerLastSeen[workerID]
	absence := ok && c.catchUpAbsenceThreshold > 0 && now.Sub(last) > c.catchUpAbsenceThreshold
	c.workerLastSeen[workerID] = now
	c.catchMu.Unlock()
	if absence {
		log.Printf("[CatchUp] Absence detected for worker %s (gap=%v > threshold=%v)", workerID, now.Sub(last).Truncate(time.Millisecond), c.catchUpAbsenceThreshold)
		c.startWorkerCatchUp(workerID)
	}
}

// processCatchUpProgress updates recovery progress based on current received sequence.
func (c *Coordinator) processCatchUpProgress(workerID string, partitionID int, currentSeq int64) {
	c.catchMu.Lock()
	rec := c.workerRecovery[workerID]
	if rec == nil || rec.completed {
		c.catchMu.Unlock()
		return
	}
	prs := rec.partitions[partitionID]
	if prs == nil {
		c.catchMu.Unlock()
		return
	}

	// Calculate processed count for this partition
	if currentSeq > prs.startSeq {
		effectiveSeq := min(currentSeq, prs.targetSeq)
		newProcessed := effectiveSeq - prs.startSeq
		if newProcessed > prs.processed {
			diff := newProcessed - prs.processed
			prs.processed = newProcessed
			rec.processed += diff
		}
	}

	needed := rec.initialBacklog
	if rec.percentTarget > 0 && rec.initialBacklog > 0 {
		needed = int64((float64(rec.initialBacklog) * float64(rec.percentTarget) / 100.0) + 0.5)
	}

	if rec.processed >= needed {
		rec.completed = true
		latency := time.Since(rec.startedAt)
		c.catchUpTotalRecoveries++
		c.catchUpLatencySum += latency
		if latency > c.catchUpLatencyMax {
			c.catchUpLatencyMax = latency
		}
		if c.catchUpLatencyMin == 0 || latency < c.catchUpLatencyMin {
			c.catchUpLatencyMin = latency
		}
		if int(rec.initialBacklog) > c.catchUpMaxInitialHoles {
			c.catchUpMaxInitialHoles = int(rec.initialBacklog)
		}
		if latency > c.catchUpDeadline {
			c.catchUpTotalExceed++
			rec.exceed = true
		}

		// Record metrics
		if c.metricsCollector != nil {
			c.metricsCollector.ObserveWorkerCatchUpLatency(latency)
			if rec.exceed {
				c.metricsCollector.IncWorkerCatchUpExceed()
			}
		}

		log.Printf("[CatchUp] Recovery complete for worker %s: latency=%v initial=%d processed=%d exceed=%v", workerID, latency, rec.initialBacklog, rec.processed, rec.exceed)
		delete(c.workerRecovery, workerID)
	}
	c.catchMu.Unlock()
}

// printLegacySLO prints the original hole-age SLO block.
func (c *Coordinator) printLegacySLO(currentOldest time.Duration) {
	if c.sloHoleMaxAge <= 0 {
		return
	}
	c.sloMu.RLock()
	sloThresh := c.sloHoleMaxAge
	sloEx := c.sloExceedTicks
	sloTicks := c.sloTotalTicks
	sloMax := c.sloMaxOldestHole
	c.sloMu.RUnlock()
	exceedPct := 0.0
	if sloTicks > 0 {
		exceedPct = (float64(sloEx) / float64(sloTicks)) * 100.0
	}
	fmt.Println("-- Legacy Hole Age SLO --")
	fmt.Printf("Oldest Hole Max Age SLO: %v\n", sloThresh)
	fmt.Printf("Current Oldest Hole Age: %v\n", currentOldest)
	fmt.Printf("Max Oldest Hole Age:     %v\n", sloMax)
	fmt.Printf("Exceedances:             %d/%d (%.1f%% of samples)\n", sloEx, sloTicks, exceedPct)
}

// printCatchUpSLO prints aggregated catch-up recovery stats.
func (c *Coordinator) printCatchUpSLO() {
	if !c.catchUpEnabled {
		return
	}
	c.catchMu.RLock()
	total := c.catchUpTotalRecoveries
	exceed := c.catchUpTotalExceed
	sum := c.catchUpLatencySum
	maxLatency := c.catchUpLatencyMax
	minLatency := c.catchUpLatencyMin
	maxBacklog := c.catchUpMaxInitialHoles
	deadline := c.catchUpDeadline
	percent := c.catchUpPercent
	c.catchMu.RUnlock()
	if total == 0 {
		fmt.Println("-- Catch-Up SLO --")
		fmt.Println("No worker recoveries observed yet")
		return
	}
	avg := sum / time.Duration(total)
	pct := (float64(exceed) / float64(total)) * 100.0
	fmt.Println("-- Catch-Up SLO --")
	fmt.Printf("Recoveries:              %d (exceed=%d, %.1f%%)\n", total, exceed, pct)
	fmt.Printf("Latency (avg/min/max):   %v / %v / %v\n", avg, minLatency, maxLatency)
	fmt.Printf("Deadline:                %v (0 disables exceed)\n", deadline)
	if percent > 0 {
		fmt.Printf("Healing Target:          %d%% of initial backlog\n", percent)
	} else {
		fmt.Println("Healing Target:          100% of initial backlog")
	}
	fmt.Printf("Max Initial Backlog:     %d holes\n", maxBacklog)
}

// printDuplicateTrace prints duplicate trace snapshot if available.
func (c *Coordinator) printDuplicateTrace(now time.Time) {
	if snap, ok := c.dup.MaybeSnapshot(now); ok {
		fmt.Println("\n--- Duplicate Trace Snapshot ---")
		fmt.Printf("Window: %s, Rate: %.2f/min, Events: %d\n", snap.Window, snap.RatePerMin, snap.Total)
		fmt.Println("Top partitions by duplicates:")
		for _, p := range snap.TopPartitions {
			fmt.Printf("  partition=%d duplicates=%d\n", p.PartitionID, p.Count)
		}
		if len(snap.Recent) > 0 {
			fmt.Println("Recent duplicate events:")
			for _, e := range snap.Recent {
				fmt.Printf("  t=%s partition=%d seq=%d worker=%s\n", e.When.Format(time.RFC3339), e.PartitionID, e.Sequence, e.WorkerID)
			}
		}
		fmt.Println("--- End Duplicate Trace Snapshot ---")
	}
}

// printStartLatencyAudit summarizes how long workers took from Worker.Start()
// to their first non-empty OnAssignmentChanged hook. Separates the initial
// cold-start cohort from later joiners (chaos-spawned workers, post-takeover
// replacements) and flags any worker that breached the configured budget.
//
// Exists specifically to surface regressions of:
//   - commit 5345527 (takeover uses PlannedScaleWindow, not ColdStartWindow)
//   - commit 7781b9b (stale assignment.<id> keys are swept on takeover)
//
// Either regression would manifest as takeover-cohort latencies climbing toward
// the pre-fix ColdStartWindow default (30s).
func (c *Coordinator) printStartLatencyAudit() {
	c.startLatencyMu.Lock()
	initial := make(map[string]time.Duration, len(c.initialStartLatencies))
	maps.Copy(initial, c.initialStartLatencies)
	takeover := make(map[string]time.Duration, len(c.takeoverStartLatencies))
	maps.Copy(takeover, c.takeoverStartLatencies)
	initialBudget := c.slowStartBudgetInitial
	takeoverBudget := c.slowStartBudgetTakeover
	c.startLatencyMu.Unlock()

	if len(initial) == 0 && len(takeover) == 0 {
		return
	}

	fmt.Println("-- Start Latency (Start() -> first assignment) --")
	c.printStartLatencyCohort("Initial cohort", initial, initialBudget)
	c.printStartLatencyCohort("Takeover / late joiners", takeover, takeoverBudget)
}

func (c *Coordinator) printStartLatencyCohort(label string, samples map[string]time.Duration, budget time.Duration) {
	if len(samples) == 0 {
		fmt.Printf("%s: n=0\n", label)
		return
	}

	var sum, maxLat time.Duration
	minLat := time.Duration(1<<63 - 1)
	var maxWorker, minWorker string
	type exceed struct {
		id  string
		lat time.Duration
	}
	var exceeds []exceed
	for id, d := range samples {
		sum += d
		if d > maxLat {
			maxLat = d
			maxWorker = id
		}
		if d < minLat {
			minLat = d
			minWorker = id
		}
		if budget > 0 && d > budget {
			exceeds = append(exceeds, exceed{id: id, lat: d})
		}
	}
	avg := sum / time.Duration(len(samples))

	fmt.Printf("%s: n=%d avg=%s min=%s(%s) max=%s(%s) budget=%s exceeded=%d\n",
		label, len(samples), avg.Round(time.Millisecond),
		minLat.Round(time.Millisecond), minWorker,
		maxLat.Round(time.Millisecond), maxWorker,
		budget, len(exceeds))

	if len(exceeds) > 0 {
		const maxShown = 5
		fmt.Println("  Exceeded budget:")
		for i, e := range exceeds {
			if i >= maxShown {
				fmt.Printf("  ... and %d more\n", len(exceeds)-maxShown)
				break
			}
			fmt.Printf("  %s=%s\n", e.id, e.lat.Round(time.Millisecond))
		}
	}
}

// SlowStartExceedances returns the counts of workers that breached the
// start-latency budget, per cohort. Used by the stability invariant check
// at simulation shutdown.
func (c *Coordinator) SlowStartExceedances() (initial, takeover int) {
	c.startLatencyMu.Lock()
	defer c.startLatencyMu.Unlock()
	for _, d := range c.initialStartLatencies {
		if c.slowStartBudgetInitial > 0 && d > c.slowStartBudgetInitial {
			initial++
		}
	}
	for _, d := range c.takeoverStartLatencies {
		if c.slowStartBudgetTakeover > 0 && d > c.slowStartBudgetTakeover {
			takeover++
		}
	}

	return initial, takeover
}

// printOwnershipAudit compares active owners vs assigned owners and prints mismatch stats.
func (c *Coordinator) printOwnershipAudit(activeSnapshot map[int]string) {
	if len(c.prevPartitionOwners) == 0 || len(activeSnapshot) == 0 {
		return
	}
	mismatches := 0
	aligned := 0
	checked := 0
	type sample struct {
		pid              int
		assigned, active string
	}
	samples := make([]sample, 0, 10)
	for pid, active := range activeSnapshot {
		if assigned, ok := c.prevPartitionOwners[pid]; ok {
			checked++
			if assigned == active {
				aligned++
			} else {
				mismatches++
				if len(samples) < 10 {
					samples = append(samples, sample{pid: pid, assigned: assigned, active: active})
				}
			}
		}
	}
	alignment := 0.0
	if checked > 0 {
		alignment = float64(aligned) / float64(checked)
	}
	fmt.Printf("Ownership Mismatches:  %d (active vs assigned, checked=%d)\n", mismatches, checked)
	fmt.Printf("Active-Assigned Align: %.3f\n", alignment)
	if mismatches > 0 && len(samples) > 0 {
		fmt.Println("Sample mismatches (pid: assigned -> active):")
		for _, s := range samples {
			fmt.Printf("  %d: %s -> %s\n", s.pid, s.assigned, s.active)
		}
	}
}

// PrintReport prints a summary report.
func (c *Coordinator) PrintReport() {
	stats := c.GetStats()
	pendingHoles := c.tracker.GetPendingHoles()
	suppressedHoles := c.tracker.GetSuppressedHolesCount()
	physicalReceived := c.tracker.GetPhysicalReceivedCount()
	gapsHealed := c.tracker.GetGapsHealedCount()
	receivedEvents := c.tracker.GetEventReceivedCount()
	// Compute current oldest pending hole age (for summary visibility)
	var currentOldest time.Duration
	for _, age := range c.tracker.GetPendingHoleAges() {
		if age > currentOldest {
			currentOldest = age
		}
	}

	var goroutines int
	var memoryMiB float64
	var activeWorkers int
	var unassigned int
	var locality float64
	var stableLocality float64
	var moved int64
	var disorder int64
	var latencyCount, recoveryCount uint64
	var latencySum, recoverySum float64
	var coldStartComplete bool

	if c.metricsCollector != nil {
		goroutines, memoryMiB = c.metricsCollector.GetSystemMetrics()
		activeWorkers = c.metricsCollector.GetActiveWorkers()
		unassigned, locality, moved = c.metricsCollector.GetAssignmentMetrics()
		stableLocality = c.metricsCollector.GetStableLocalityRatio()
		disorder = c.metricsCollector.GetDisorderDepth()
		latencyCount, latencySum = c.metricsCollector.PublishLatencySummary()
		recoveryCount, recoverySum = c.metricsCollector.RecoveryDurationSummary()
		coldStartComplete = c.metricsCollector.ColdStartComplete()
	}

	fmt.Printf("\n=== Simulation Report [%s] ===\n", time.Now().Format(time.RFC3339))
	fmt.Println("-- Message Flow --")
	fmt.Printf("Total Partitions:     %d\n", stats.TotalPartitions)
	fmt.Printf("Total Sent:           %d\n", stats.TotalSent)
	fmt.Printf("Total Received:       %d\n", stats.TotalReceived)
	fmt.Printf("Physical Received:    %d\n", physicalReceived)
	fmt.Printf("Received Events:      %d\n", receivedEvents)
	fmt.Printf("In-Flight:            %d\n", stats.InFlight)
	fmt.Printf("Pending Holes:        %d\n", pendingHoles)
	fmt.Printf("Suppressed Holes:     %d\n", suppressedHoles)
	fmt.Printf("Gaps Detected:        %d\n", stats.GapCount)
	fmt.Printf("Duplicates:           %d\n", stats.DuplicateCount)

	if c.metricsCollector != nil {
		fmt.Println("-- System --")
		fmt.Printf("Active Workers:       %d\n", activeWorkers)
		fmt.Printf("Active Goroutines:    %d\n", goroutines)
		fmt.Printf("Memory Usage:         %.2f MiB\n", memoryMiB)

		avgPartitionsPerWorker, avgRebalanceDuration, avgProcessingLatency := c.metricsCollector.GetAggregatedMetrics()
		fmt.Printf("Avg Partitions/Worker: %.1f\n", avgPartitionsPerWorker)
		fmt.Printf("Avg Rebalance Duration: %.2fs\n", avgRebalanceDuration)
		fmt.Printf("Avg Processing Latency: %.2fms\n", avgProcessingLatency*1000)

		fmt.Println("-- Assignment --")
		fmt.Printf("Unassigned Partitions: %d\n", unassigned)
		fmt.Printf("Locality Ratio:        %.3f\n", locality)
		fmt.Printf("Stable Locality Ratio: %.3f\n", stableLocality)
		fmt.Printf("Moved Partitions Total: %d\n", moved)
		fmt.Printf("Cold Start Complete:   %v\n", coldStartComplete)

		// Ownership audit printing
		c.ownersMu.RLock()
		activeCount := len(c.activeOwners)
		activeSnapshot := make(map[int]string, activeCount)
		maps.Copy(activeSnapshot, c.activeOwners)
		c.ownersMu.RUnlock()
		c.printOwnershipAudit(activeSnapshot)

		fmt.Println("-- Robustness --")
		fmt.Printf("Disorder Depth:        %d\n", disorder)
		fmt.Printf("Holes Healed:          %d\n", c.tracker.GetHolesHealedCount())
		fmt.Printf("Gaps Healed:           %d\n", gapsHealed)
		// Healing rate gauge (recent interval)
		fmt.Printf("Healed Rate (last 5s): %.2f/sec\n", c.metricsCollector.GetHolesHealedRate())

		fmt.Println("-- Latency --")
		fmt.Printf("Publish→Consume Samples: %d (sum=%.3fs)\n", latencyCount, latencySum)

		fmt.Println("-- Recovery --")
		fmt.Printf("Recovery Samples: %d (sum=%.3fs) Active=%v\n", recoveryCount, recoverySum, c.recoveryActive)
	}

	// Start-latency audit: flags regressions of leader-takeover hang fixes.
	c.printStartLatencyAudit()

	// Legacy hole-age SLO and Catch-Up SLO blocks
	c.printLegacySLO(currentOldest)
	c.printCatchUpSLO()

	// if stats.GapCount == 0 && stats.DuplicateCount == 0 && stats.InFlight == 0 {
	// 	fmt.Println("SUCCESS: No message loss or duplication detected")
	// } else if stats.GapCount > 0 || stats.DuplicateCount > 0 {
	// 	fmt.Printf("FAILURE: Message loss or duplication detected, gaps=%d, duplicates=%d\n", stats.GapCount, stats.DuplicateCount)
	// } else {
	// 	fmt.Printf("PENDING: %d messages in-flight (not yet received)\n", stats.InFlight)
	// }

	if c.recoveryActive {
		fmt.Printf("Recovery in progress (started %v)\n", c.recoveryStart.Format(time.RFC3339))
	}

	c.printDuplicateTrace(time.Now())
}

func (c *Coordinator) processSentMessages(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.sentCh:
			c.tracker.RecordSent(msg.PartitionID, msg.ProducerID, msg.PartitionSequence, msg.ProducerSequence)
		}
	}
}

// ClassificationEvidence is a bounded record of a cross-worker
// known-duplicate classification . Used for InconclusiveOwner-
// Events (and would extend to other downgrade classes if added later).
// ConcurrentOwners and stale-receipt records are still captured via the
// extended MessageOwnershipViolationError; this struct exists for the
// non-violation classifications that still need post-hoc inspection.
type ClassificationEvidence struct {
	PartitionID     int       `json:"partition_id"`
	Sequence        int64     `json:"sequence"`
	OriginalWorker  string    `json:"original_worker"`
	ReceivingWorker string    `json:"receiving_worker"`
	Reason          string    `json:"reason"`
	ObservedAt      time.Time `json:"observed_at"`
}

// FailureReport represents the structured failure output.
type FailureReport struct {
	Timestamp                         time.Time                        `json:"timestamp"`
	Reason                            string                           `json:"reason"`
	Stats                             TrackerStats                     `json:"stats"`
	GapErrors                         []string                         `json:"gap_errors,omitempty"`
	DetailedGaps                      []MessageGapError                `json:"detailed_gaps,omitempty"`
	ActiveGaps                        int                              `json:"active_gaps"`
	PendingHoles                      int64                            `json:"pending_holes"`
	OwnershipViolations               []MessageOwnershipViolationError `json:"ownership_violations,omitempty"`
	ConcurrentOwnerEvents             []MessageOwnershipViolationError `json:"concurrent_owner_events,omitempty"`
	InconclusiveOwnerEvents           []ClassificationEvidence         `json:"inconclusive_owner_events,omitempty"`
	OwnershipUnobservedPreChaosCount  int64                            `json:"ownership_unobserved_pre_chaos_count"`
	OwnershipUnobservedPostChaosCount int64                            `json:"ownership_unobserved_post_chaos_count"`
	FirstChaosEventAt                 time.Time                        `json:"first_chaos_event_at,omitzero"`
}

// appendOwnershipViolation appends a violation to the bounded list. Caller
// must not hold any other coordinator locks (this takes its own).
// ConcurrentOwner-flagged violations are also indexed separately for
// failure-report visibility — same backing list, just an additional
// pointer-equivalent slice.
func (c *Coordinator) appendOwnershipViolation(ove MessageOwnershipViolationError) {
	c.ownershipViolationsMu.Lock()
	defer c.ownershipViolationsMu.Unlock()
	if len(c.ownershipViolations) >= c.ownershipViolationsCap {
		return
	}
	c.ownershipViolations = append(c.ownershipViolations, ove)
	if ove.ConcurrentOwners {
		if len(c.concurrentOwnerEvents) < c.ownershipViolationsCap {
			c.concurrentOwnerEvents = append(c.concurrentOwnerEvents, ove)
		}
	}
}

// appendInconclusiveOwnerEvent appends an inconclusive-owner
// classification record to its bounded list. Capped at
// ownershipViolationsCap entries to keep failure reports compact.
func (c *Coordinator) appendInconclusiveOwnerEvent(ev ClassificationEvidence) {
	c.ownershipViolationsMu.Lock()
	defer c.ownershipViolationsMu.Unlock()
	if len(c.inconclusiveOwnerEvents) >= c.ownershipViolationsCap {
		return
	}
	c.inconclusiveOwnerEvents = append(c.inconclusiveOwnerEvents, ev)
}

// snapshotOwnershipViolations returns a copy of the current violation list.
func (c *Coordinator) snapshotOwnershipViolations() []MessageOwnershipViolationError {
	c.ownershipViolationsMu.Lock()
	defer c.ownershipViolationsMu.Unlock()
	if len(c.ownershipViolations) == 0 {
		return nil
	}
	out := make([]MessageOwnershipViolationError, len(c.ownershipViolations))
	copy(out, c.ownershipViolations)

	return out
}

// snapshotConcurrentOwnerEvents returns a copy of the concurrent-owner
// subset of violations.
func (c *Coordinator) snapshotConcurrentOwnerEvents() []MessageOwnershipViolationError {
	c.ownershipViolationsMu.Lock()
	defer c.ownershipViolationsMu.Unlock()
	if len(c.concurrentOwnerEvents) == 0 {
		return nil
	}
	out := make([]MessageOwnershipViolationError, len(c.concurrentOwnerEvents))
	copy(out, c.concurrentOwnerEvents)

	return out
}

// snapshotInconclusiveOwnerEvents returns a copy of the inconclusive-owner
// classifications.
func (c *Coordinator) snapshotInconclusiveOwnerEvents() []ClassificationEvidence {
	c.ownershipViolationsMu.Lock()
	defer c.ownershipViolationsMu.Unlock()
	if len(c.inconclusiveOwnerEvents) == 0 {
		return nil
	}
	out := make([]ClassificationEvidence, len(c.inconclusiveOwnerEvents))
	copy(out, c.inconclusiveOwnerEvents)

	return out
}

// TriggerFailure triggers a failure report and stops the simulation.
// Idempotent via failureOnce. The stopCh close goes through closeStopCh
// (sync.Once) so it cannot panic on a race with Start(ctx)'s shutdown.
func (c *Coordinator) TriggerFailure(reason string, err error) {
	c.failureOnce.Do(func() {
		log.Printf("[Coordinator] CRITICAL FAILURE: %s (%v). Stopping simulation.", reason, err)
		c.writeFailureReport(reason, err)
		c.closeStopCh() // Signal global stop
	})
}

func (c *Coordinator) internalTriggerFailure(reason string, err error) {
	c.TriggerFailure(reason, err)
}

func (c *Coordinator) writeFailureReport(reason string, err error) {
	path := c.failureReportPath
	if path == "" {
		path = "failure_report.json"
	}

	stats := c.tracker.GetStats()
	report := FailureReport{
		Timestamp:                         time.Now(),
		Reason:                            fmt.Sprintf("%s: %v", reason, err),
		Stats:                             stats,
		ActiveGaps:                        stats.GapCount,
		PendingHoles:                      c.tracker.GetPendingHoles(),
		OwnershipViolations:               c.snapshotOwnershipViolations(),
		ConcurrentOwnerEvents:             c.snapshotConcurrentOwnerEvents(),
		InconclusiveOwnerEvents:           c.snapshotInconclusiveOwnerEvents(),
		OwnershipUnobservedPreChaosCount:  stats.OwnershipUnobservedPreChaosCount,
		OwnershipUnobservedPostChaosCount: stats.OwnershipUnobservedPostChaosCount,
		FirstChaosEventAt:                 c.FirstChaosEventAt(),
	}

	// Include specific error details if available
	if err != nil {
		report.GapErrors = append(report.GapErrors, err.Error())
		var gapErr *MessageGapError
		if errors.As(err, &gapErr) {
			report.DetailedGaps = append(report.DetailedGaps, *gapErr)
		}
	}

	data, marshalErr := json.MarshalIndent(report, "", "  ")
	if marshalErr != nil {
		log.Printf("[Coordinator] Failed to marshal failure report: %v", marshalErr)
		return
	}

	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		log.Printf("[Coordinator] Failed to write failure report to %s: %v", path, writeErr)
		return
	}
	log.Printf("[Coordinator] Failure report written to %s", path)
}

func (c *Coordinator) processReceivedMessages(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case msg := <-c.receivedCh:
			// Record received and handle potential catch-up lifecycle transitions.
			healed, err := c.tracker.RecordReceivedFromWorker(msg.PartitionID, msg.PartitionSequence, msg.WorkerID)
			if c.catchUpEnabled {
				c.processCatchUpActivity(msg.WorkerID)
			}
			if c.stateReconcile != nil {
				c.stateReconcile.RecordMessage(msg.WorkerID, msg.PartitionID)
			}
			// Track active (actual) owner by last processor seen per partition
			c.ownersMu.Lock()
			c.activeOwners[msg.PartitionID] = msg.WorkerID
			c.ownersMu.Unlock()
			if err != nil && c.dispatchReceiveError(msg, err) {
				return
			}

			// Record metrics
			if c.metricsCollector != nil {
				c.metricsCollector.RecordMessageReceived(msg.PartitionID)
				// Healed holes metrics
				if len(healed) > 0 {
					for _, d := range healed {
						c.metricsCollector.ObserveHoleLifetime(d)
					}
					c.metricsCollector.IncHolesHealed(len(healed))
				}
			}

			if c.catchUpEnabled {
				c.processCatchUpProgress(msg.WorkerID, msg.PartitionID, msg.PartitionSequence)
			}
		}
	}
}

// dispatchReceiveError routes a tracker error to its appropriate handler
// (metric, log, failure-report append, stop-on-failure trigger). Returns
// true if the caller should stop the receive loop (e.g., stop-on-failure
// triggered).
func (c *Coordinator) dispatchReceiveError(msg ReceivedMessage, err error) bool {
	// Redelivery is informational — record metric and continue. Must
	// dispatch BEFORE the generic error branches so it doesn't fall
	// through to DupTracer or stop-on-failure.
	if errors.Is(err, ErrMessageRedelivery) {
		if c.metricsCollector != nil {
			c.metricsCollector.RecordRedelivery()
		}

		return false
	}
	if errors.Is(err, ErrMessageOwnershipViolation) {
		return c.handleOwnershipViolation(err)
	}
	//  inconclusive/unobserved owner classifications are
	// non-fatal but still recorded so Outcome A can be audited.
	if errors.Is(err, ErrMessageOwnershipInconclusive) {
		var ie *MessageOwnershipInconclusiveError
		if errors.As(err, &ie) {
			c.appendInconclusiveOwnerEvent(ClassificationEvidence{
				PartitionID:     ie.PartitionID,
				Sequence:        ie.Sequence,
				OriginalWorker:  ie.OriginalWorker,
				ReceivingWorker: ie.CurrentWorker,
				Reason:          "inconclusive_owner_snapshot_empty",
				ObservedAt:      time.Now(),
			})
		}

		return false
	}
	if errors.Is(err, ErrMessageOwnershipUnobserved) {
		// Count tracking happens in the tracker (pre/post chaos buckets);
		// no failure-report record per occurrence — only aggregate counts.
		return false
	}

	// Generic gap/duplicate path.
	if c.metricsCollector != nil {
		if errors.Is(err, ErrMessageGap) {
			c.metricsCollector.RecordGap()
		}
		if errors.Is(err, ErrMessageDuplicate) {
			c.metricsCollector.RecordDuplicate()
			c.metricsCollector.RecordDuplicatePartition(msg.PartitionID)
		}
	}
	if errors.Is(err, ErrMessageDuplicate) {
		c.dup.RecordDuplicate(msg.PartitionID, msg.WorkerID, msg.PartitionSequence, time.Now())
	}
	if c.stopOnFailure && errors.Is(err, ErrMessageGap) {
		c.internalTriggerFailure("Gap detected", err)

		return true
	}

	return false
}

func (c *Coordinator) handleOwnershipViolation(err error) bool {
	var ove *MessageOwnershipViolationError
	if !errors.As(err, &ove) {
		return false
	}
	if c.metricsCollector != nil {
		c.metricsCollector.RecordOwnershipViolation()
	}
	c.appendOwnershipViolation(*ove)
	log.Printf("[Coordinator] OWNERSHIP VIOLATION: partition=%d seq=%d original=%s current=%s",
		ove.PartitionID, ove.Sequence, ove.OriginalWorker, ove.CurrentWorker)
	if c.stopOnFailure {
		c.internalTriggerFailure("Ownership violation detected", err)

		return true
	}

	return false
}

func (c *Coordinator) runMetricsTicker(ctx context.Context) {
	interval := 5 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var prevHealed int64
	prevHealed = c.tracker.GetHolesHealedCount()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats := c.tracker.GetStats()
			holes := c.tracker.GetPendingHoles()
			c.metricsCollector.SetCoordinatorInFlight(stats.InFlight)
			c.metricsCollector.SetCoordinatorPendingHoles(holes)
			// Disorder depth gauge
			c.metricsCollector.SetDisorderDepth(c.tracker.GetDisorderDepth())

			// Pending hole ages histogram observations
			ages := c.tracker.GetPendingHoleAges()
			var oldest time.Duration
			for _, age := range ages {
				c.metricsCollector.ObservePendingHoleAge(age)
				if age > oldest {
					oldest = age
				}
			}

			// SLO sampling (oldest pending hole vs threshold)
			if c.sloHoleMaxAge > 0 {
				c.sloMu.Lock()
				c.sloTotalTicks++
				if oldest > 0 && oldest > c.sloHoleMaxAge {
					c.sloExceedTicks++
				}
				if oldest > c.sloMaxOldestHole {
					c.sloMaxOldestHole = oldest
				}
				c.sloMu.Unlock()
			}

			// Healing rate (healed holes per second over interval)
			currentHealed := c.tracker.GetHolesHealedCount()
			delta := max(currentHealed-prevHealed,
				// defensive reset
				0)
			perSecond := float64(delta) / interval.Seconds()
			c.metricsCollector.SetHolesHealedRate(perSecond)
			prevHealed = currentHealed

			// Prune escalated gaps to prevent memory leaks
			c.tracker.PruneEscalatedGaps(10 * time.Minute)

			// Prune stale recoveries and worker tracking
			c.PruneStaleRecoveries(10 * time.Minute)
		}
	}
}

func (c *Coordinator) processAssignments(ctx context.Context) { //nolint:gocyclo,cyclop
	// Periodic evaluator to update Stable Locality even without new assignment events
	ticker := time.NewTicker(c.stableQuietWindow)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case wid := <-c.stoppedWorkersCh:
			// A worker goroutine has permanently stopped. Drop its snapshot and
			// rebuild prevPartitionOwners so the ownership audit no longer
			// attributes partitions to a dead worker. Running in the same
			// goroutine as assignment reports keeps these updates serialized.
			if _, had := c.workerAssignments[wid]; !had {
				continue
			}
			delete(c.workerAssignments, wid)
			c.rebuildOwnerSnapshotLocked()
			if c.snapshotOverlap != nil {
				c.snapshotOverlap.ForgetWorker(wid)
			}
			if c.baselineLocked && c.totalPartitions > 0 {
				fresh := make(map[int]string, c.totalPartitions)
				for id, wset := range c.workerAssignments {
					for pid := range wset {
						if _, exists := fresh[pid]; !exists {
							fresh[pid] = id
						}
					}
				}
				c.prevPartitionOwners = fresh
			}
		case sl := <-c.startLatenciesCh:
			// Classify by the caller-provided cohort flag. Earlier versions
			// used baselineLocked at receive time, but that races: when the
			// worker's assignment report and startLatency report sit in
			// separate channels, the select can drain the assignment first,
			// flip baselineLocked, then misbucket the trailing startLatency
			// of an initial-cohort worker as takeover. The flag is stamped
			// at worker construction, so the decision is timing-invariant.
			c.startLatencyMu.Lock()
			if sl.IsInitialCohort {
				c.initialStartLatencies[sl.WorkerID] = sl.Latency
			} else {
				c.takeoverStartLatencies[sl.WorkerID] = sl.Latency
			}
			c.startLatencyMu.Unlock()
		case ar := <-c.assignmentsCh:
			// Track reporter appearance and optionally log when all expected workers reported once
			if c.expectedWorkers > 0 {
				if _, seen := c.initialReporters[ar.WorkerID]; !seen {
					c.initialReporters[ar.WorkerID] = struct{}{}
					if len(c.initialReporters) == c.expectedWorkers {
						log.Printf("[Coordinator] Observed all expected workers (%d)", c.expectedWorkers)
					}
				}
			}
			// Update worker assignment snapshot.
			set := make(map[int]struct{}, len(ar.Partitions))
			for _, p := range ar.Partitions {
				if p >= 0 && p < c.totalPartitions { // ignore out of range silently
					set[p] = struct{}{}
				}
			}
			c.workerAssignments[ar.WorkerID] = set
			c.rebuildOwnerSnapshotLocked()

			// Feed oracle classifiers with the fresh assignment snapshot.
			if c.snapshotOverlap != nil {
				c.snapshotOverlap.IngestAssignment(ar.WorkerID, ar.Partitions)
				c.snapshotOverlap.Check(time.Now())
			}
			if c.stateReconcile != nil {
				c.stateReconcile.RecordAssignment(ar.WorkerID, ar.Partitions, time.Now())
			}

			// Build global set.
			global := make(map[int]struct{})
			for _, wset := range c.workerAssignments {
				for pid := range wset {
					global[pid] = struct{}{}
				}
			}

			// Compute unassigned with cold-start convergence gating plus soft expected-workers hint.
			// Until full coverage is first achieved AND either we have seen all expected workers
			// (when provided) or a small timeout elapses, we report unassigned=0 and defer
			// movement/locality to avoid transient skew.
			coverageAchieved := len(global) == c.totalPartitions
			if c.firstAssignmentAt.IsZero() {
				c.firstAssignmentAt = time.Now()
			}
			if !coverageAchieved {
				c.metricsCollector.SetUnassignedPartitions(0)
				if len(global) > 0 {
					c.metricsCollector.SetLocalityRatio(1.0)
				}
				c.prevGlobalAssigned = global
				c.prevPartitionOwners = make(map[int]string, len(global))
				continue
			}

			// Coverage reached: mark convergence and check expected-workers soft lock
			if !c.finalCoverageAchieved {
				c.finalCoverageAchieved = true
				if !c.firstAssignmentAt.IsZero() {
					c.metricsCollector.ObserveColdStartConvergence(time.Since(c.firstAssignmentAt))
				}
			}

			needWaitForWorkers := c.expectedWorkers > 0 && len(c.initialReporters) < c.expectedWorkers
			if needWaitForWorkers && time.Since(c.firstAssignmentAt) < c.baselineLockTimeout {
				// Keep gating a bit longer
				c.metricsCollector.SetUnassignedPartitions(0)
				c.metricsCollector.SetLocalityRatio(1.0)
				c.prevGlobalAssigned = global
				c.prevPartitionOwners = make(map[int]string, len(global))
				continue
			}
			if needWaitForWorkers && time.Since(c.firstAssignmentAt) >= c.baselineLockTimeout {
				log.Printf("[Coordinator] Baseline lock timeout after %v; proceeding with metrics (expected=%d, seen=%d)", time.Since(c.firstAssignmentAt).Truncate(time.Millisecond), c.expectedWorkers, len(c.initialReporters))
			}

			// Baseline is now locked (coverage achieved and either workers observed or timeout)
			if !c.baselineLocked {
				c.baselineLocked = true
				if c.metricsCollector != nil {
					c.metricsCollector.SetColdStartComplete(true)
				}
			}

			unassigned := max(c.totalPartitions-len(global),
				// sanity
				0)
			c.metricsCollector.SetUnassignedPartitions(unassigned)

			// Build current partition owner map (first seen worker wins if overlaps)
			currentOwners := make(map[int]string, len(global))
			for wid, wset := range c.workerAssignments {
				for pid := range wset {
					if _, exists := currentOwners[pid]; !exists {
						currentOwners[pid] = wid
					}
				}
			}

			// Locality & movement based on owner stability
			if len(c.prevPartitionOwners) > 0 {
				stable := 0
				moved := 0
				for pid, prevOwner := range c.prevPartitionOwners {
					currOwner, ok := currentOwners[pid]
					if !ok {
						continue // partition disappeared (shouldn't happen normally)
					}
					if currOwner == prevOwner {
						stable++
					} else {
						moved++
					}
				}
				prevCount := len(c.prevPartitionOwners)
				localityRatio := 0.0
				if prevCount > 0 {
					localityRatio = float64(stable) / float64(prevCount)
				}
				c.metricsCollector.SetLocalityRatio(localityRatio)
				if moved > 0 {
					c.metricsCollector.IncMovedPartitions(moved)
				}
			} else if len(currentOwners) > 0 {
				c.metricsCollector.SetLocalityRatio(1.0)
			}

			c.prevGlobalAssigned = global
			c.prevPartitionOwners = currentOwners

			// If recovering, check exit conditions: all partitions assigned & disorder depth zero.
			if c.recoveryActive && unassigned == 0 && c.tracker.GetDisorderDepth() == 0 {
				dur := time.Since(c.recoveryStart)
				c.recoveryActive = false
				if c.metricsCollector != nil {
					c.metricsCollector.ObserveRecoveryDuration(dur)
					// Restore readiness signal after recovery completes.
					c.metricsCollector.SetColdStartComplete(true)
				}
				log.Printf("[Coordinator] Recovery complete in %v", dur)
			}
		case <-ticker.C:
			// Periodically compute stable locality against last stable snapshot after quiet window
			if !c.baselineLocked {
				continue
			}
			// Build current owners from workerAssignments
			currentOwners := make(map[int]string)
			for wid, wset := range c.workerAssignments {
				for pid := range wset {
					if _, exists := currentOwners[pid]; !exists {
						currentOwners[pid] = wid
					}
				}
			}
			c.updateStableLocality(currentOwners)
		}
	}
}

func (c *Coordinator) updateStableLocality(currentOwners map[int]string) {
	if !c.baselineLocked {
		return
	}
	now := time.Now()
	if c.lastStableOwners != nil && now.Sub(c.lastStableAt) >= c.stableQuietWindow {
		// Compare current owners to last stable snapshot
		stable2 := 0
		count2 := len(c.lastStableOwners)
		for pid, prev := range c.lastStableOwners {
			if cur, ok := currentOwners[pid]; ok && cur == prev {
				stable2++
			}
		}
		ratio2 := 0.0
		if count2 > 0 {
			ratio2 = float64(stable2) / float64(count2)
		}
		c.metricsCollector.SetStableLocalityRatio(ratio2)
		// Refresh stable baseline snapshot
		c.lastStableOwners = currentOwners
		c.lastStableAt = now
	} else if c.lastStableOwners == nil {
		// Initialize stable baseline when first eligible
		c.lastStableOwners = currentOwners
		c.lastStableAt = now
	}
}

// PruneStaleRecoveries removes worker recovery contexts that have been active
// for too long, preventing memory leaks from stuck recoveries.
// It also prunes stale workerLastSeen entries.
func (c *Coordinator) PruneStaleRecoveries(maxAge time.Duration) {
	c.catchMu.Lock()
	defer c.catchMu.Unlock()

	now := time.Now()
	for id, rec := range c.workerRecovery {
		if now.Sub(rec.startedAt) > maxAge {
			log.Printf("[CatchUp] Pruning stale recovery for worker %s (age=%v)", id, now.Sub(rec.startedAt))
			delete(c.workerRecovery, id)
		}
	}

	// Prune stale last seen entries to prevent unbounded growth
	for id, last := range c.workerLastSeen {
		if now.Sub(last) > maxAge {
			// Only prune if not currently recovering (though recovery should be pruned above)
			if _, recovering := c.workerRecovery[id]; !recovering {
				delete(c.workerLastSeen, id)
			}
		}
	}
}

// processDegradedReports drains WorkerDegradedReport messages from
// degradedReportsCh and forwards each to the DegradedReasonOracle.
func (c *Coordinator) processDegradedReports(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-c.degradedReportsCh:
			if c.degradedReasonOracle != nil {
				c.degradedReasonOracle.Ingest(r)
			}
		}
	}
}

// watchClaimLostShutdowns polls each registered worker's state. When a worker
// first transitions to StateShutdown, the watcher informs the
// DegradedReasonOracle so it can classify the transition as expected (a
// peer-takeover-targeted worker) or unexpected (a whole-bucket-loss path
// that mis-routed into claimLostShutdown).
//
// Polling cadence is 250ms — short enough to observe a shutdown well within
// the WorkerIDTTL × 2 invariant window, long enough not to thrash.
func (c *Coordinator) watchClaimLostShutdowns(ctx context.Context) {
	if c.degradedReasonOracle == nil {
		return
	}
	// registry is held by snapshotOverlap; pull it directly via the
	// leaderUniq watcher which stores it explicitly.
	if c.leaderUniq == nil {
		return
	}
	registry := c.leaderUniq.registry
	if registry == nil {
		return
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	reported := make(map[string]struct{})
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, info := range registry.GetByType(WorkerGoroutine) {
				obs, ok := info.Obj.(WorkerObserver)
				if !ok {
					continue
				}
				if obs.WorkerStateInt() != workerStateShutdown {
					continue
				}
				wid := obs.StableWorkerID()
				key := info.ID
				if wid != "" {
					key = wid
				}
				if _, seen := reported[key]; seen {
					continue
				}
				reported[key] = struct{}{}
				c.degradedReasonOracle.ObserveClaimLostShutdown(key)
			}
		}
	}
}
