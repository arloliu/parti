package coordinator

import (
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Interfaces
// ---------------------------------------------------------------------------

// WorkerObserver is implemented by *worker.Worker in all-in-one mode.
// It exposes the subset of manager state the coordinator oracles need without
// creating an import cycle (coordinator ← worker ← coordinator).
//
// In process-mode the registry Obj is nil; callers type-assert and skip nil.
type WorkerObserver interface {
	// IsLeader returns true if the worker's manager is currently the leader.
	IsLeader() bool
	// WorkerStateInt returns the current manager state cast to int.
	// Mirrors parti.State (== types.State == int) without importing parti.
	WorkerStateInt() int
	// StableWorkerID returns the stable worker ID claimed by the manager, or ""
	// if not yet claimed.
	StableWorkerID() string
}

// workerStateStable is the int value of parti.StateStable / types.StateStable.
// types/state.go: StateInit=0, StateClaimingID=1, StateElection=2,
// StateWaitingAssignment=3, StateStable=4.
const workerStateStable = 4

// workerStateShutdown is the int value of parti.StateShutdown / types.StateShutdown.
// types/state.go: ..., StateScaling=5, StateRebalancing=6, StateEmergency=7,
// StateDegraded=8, StateShutdown=9.
const workerStateShutdown = 9

// ---------------------------------------------------------------------------
// Shared report types
// ---------------------------------------------------------------------------

// WorkerDegradedReport is sent on Config.DegradedReportCh when a worker enters
// degraded mode. WorkerID is the stable-ID string (may be "" before claim).
type WorkerDegradedReport struct {
	WorkerID string
	Reason   string
	At       time.Time
}

// ---------------------------------------------------------------------------
// Snapshot-overlap classifier (Gap 11)
// ---------------------------------------------------------------------------

// overlapEntry records when a (partition, workerPair) overlap was first observed.
type overlapEntry struct {
	firstSeen time.Time
}

// overlapPairKey is the normalized key for a (partition, two-worker-pair) overlap.
type overlapPairKey struct {
	partition        int
	workerA, workerB string // invariant: workerA <= workerB lexicographically
}

// SnapshotOverlapClassifier fires when two latest AssignmentReports for
// distinct workers share a partition for longer than graceWindow.
//
// Use NewSnapshotOverlapClassifier to construct; call IngestAssignment on each
// AssignmentReport ingest, then Check periodically (or after every ingest).
// workerSnapshot holds a worker's latest partition assignment and when it was last updated.
type workerSnapshot struct {
	partitions map[int]struct{}
	updatedAt  time.Time
}

type SnapshotOverlapClassifier struct {
	mu          sync.Mutex
	graceWindow time.Duration
	registry    *GoroutineRegistry         // may be nil; when set, only live workers are checked
	snapshots   map[string]*workerSnapshot // workerID → snapshot
	active      map[overlapPairKey]*overlapEntry
	violations  atomic.Int64
}

// NewSnapshotOverlapClassifier constructs a classifier with the given grace window.
// Pass registry (non-nil) so that Check() skips snapshots from workers that have
// already been unregistered (e.g. crashed/scaled-down workers). Pass nil only in
// unit tests that manage worker lifetimes explicitly.
// Default grace of 5 s is appropriate for the assignment-handoff window.
func NewSnapshotOverlapClassifier(graceWindow time.Duration, registry *GoroutineRegistry) *SnapshotOverlapClassifier {
	if graceWindow <= 0 {
		graceWindow = 5 * time.Second
	}
	return &SnapshotOverlapClassifier{
		graceWindow: graceWindow,
		registry:    registry,
		snapshots:   make(map[string]*workerSnapshot),
		active:      make(map[overlapPairKey]*overlapEntry),
	}
}

// IngestAssignment updates the snapshot for workerID and records the update timestamp.
// Safe to call from any goroutine.
func (c *SnapshotOverlapClassifier) IngestAssignment(workerID string, partitions []int) {
	set := make(map[int]struct{}, len(partitions))
	for _, p := range partitions {
		set[p] = struct{}{}
	}
	now := time.Now()
	c.mu.Lock()
	c.snapshots[workerID] = &workerSnapshot{partitions: set, updatedAt: now}
	c.mu.Unlock()
}

// ForgetWorker removes a worker's snapshot and any associated overlap state.
// Call when a worker is permanently stopped (e.g., scale-down) to prevent
// phantom overlaps with successor workers. Safe to call from any goroutine.
func (c *SnapshotOverlapClassifier) ForgetWorker(workerID string) {
	c.mu.Lock()
	c.pruneStaleWorker(workerID)
	c.mu.Unlock()
}

// staleSnapshotWindow is how long a snapshot can go without an update before it is
// considered stale and excluded from overlap checks. A network-disconnected worker
// stops sending AssignmentReports; after this window its snapshot is pruned so it
// does not generate phantom overlaps with its successor.
// Set to 2× the default grace window (10 s) to allow for message latency jitter.
const staleSnapshotWindow = 10 * time.Second

// pruneStaleWorker removes a worker's snapshot and any active overlap entries for it.
// Must be called with c.mu held.
func (c *SnapshotOverlapClassifier) pruneStaleWorker(wid string) {
	delete(c.snapshots, wid)
	for k := range c.active {
		if k.workerA == wid || k.workerB == wid {
			delete(c.active, k)
		}
	}
}

// Check evaluates the current snapshots at time now and increments the violation
// counter for overlaps that have persisted longer than graceWindow. Clears entries
// that are no longer overlapping.
//
// Snapshots are pruned when:
//  1. The worker is no longer active in the registry (crashed/scaled-down), OR
//  2. The snapshot has not been updated in staleSnapshotWindow (network-disconnected).
func (c *SnapshotOverlapClassifier) Check(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Build live-worker set from registry (if available) and prune dead/stale snapshots.
	var live map[string]struct{}
	if c.registry != nil {
		live = make(map[string]struct{})
		for _, info := range c.registry.GetByType(WorkerGoroutine) {
			live[info.ID] = struct{}{}
		}
	}
	for wid, snap := range c.snapshots {
		dead := live != nil && func() bool { _, ok := live[wid]; return !ok }()
		stale := now.Sub(snap.updatedAt) > staleSnapshotWindow
		if dead || stale {
			c.pruneStaleWorker(wid)
		}
	}

	// Collect worker IDs in deterministic order for stable pair enumeration.
	workers := make([]string, 0, len(c.snapshots))
	for w := range c.snapshots {
		workers = append(workers, w)
	}
	slices.Sort(workers)

	// Active overlap keys at this instant.
	currentKeys := make(map[overlapPairKey]struct{})

	for i := 0; i < len(workers); i++ {
		for j := i + 1; j < len(workers); j++ {
			wA, wB := workers[i], workers[j]
			// Normalize pair (lexicographic order guaranteed by sort).
			for pid := range c.snapshots[wA].partitions {
				if _, ok := c.snapshots[wB].partitions[pid]; !ok {
					continue
				}
				k := overlapPairKey{partition: pid, workerA: wA, workerB: wB}
				currentKeys[k] = struct{}{}

				entry, exists := c.active[k]
				if !exists {
					c.active[k] = &overlapEntry{firstSeen: now}
					continue
				}
				if now.Sub(entry.firstSeen) > c.graceWindow {
					log.Printf("[SnapshotOverlapClassifier] OVERLAP partition=%d workers=[%s,%s] age=%v > grace=%v",
						pid, wA, wB, now.Sub(entry.firstSeen).Truncate(time.Millisecond), c.graceWindow)
					c.violations.Add(1)
					// Reset first-seen so we count each grace-window span once.
					entry.firstSeen = now
				}
			}
		}
	}

	// Clear entries no longer overlapping.
	for k := range c.active {
		if _, active := currentKeys[k]; !active {
			delete(c.active, k)
		}
	}
}

// ViolationCount returns the total number of overlap violations detected.
func (c *SnapshotOverlapClassifier) ViolationCount() int64 {
	return c.violations.Load()
}

// ---------------------------------------------------------------------------
// Leader-uniqueness watcher (Gap 14)
// ---------------------------------------------------------------------------

// LeaderUniquenessWatcher polls the goroutine registry and counts polling instants
// where more than one active worker reports IsLeader()==true simultaneously.
//
// Pre-chaos double-leader observations go to doubleLeaderCount (fail-run counter).
// Post-chaos (after MarkChaosStarted) observations go to doubleLeaderPostChaos
// (informational only, not checked in the exit invariant). This mirrors the
// unobserved_post_chaos bucket used by the message tracker.
type LeaderUniquenessWatcher struct {
	registry              *GoroutineRegistry
	chaosStarted          atomic.Bool
	doubleLeaderCount     atomic.Int64
	doubleLeaderPostChaos atomic.Int64
}

// NewLeaderUniquenessWatcher creates a watcher backed by the given registry.
func NewLeaderUniquenessWatcher(registry *GoroutineRegistry) *LeaderUniquenessWatcher {
	return &LeaderUniquenessWatcher{registry: registry}
}

// MarkChaosStarted notifies the watcher that chaos events are underway. After this
// call, double-leader observations are counted in a separate post-chaos bucket and
// do not contribute to the fail-run counter. Idempotent.
func (w *LeaderUniquenessWatcher) MarkChaosStarted() {
	w.chaosStarted.Store(true)
}

// poll executes a single leader-uniqueness check. Called by the background
// goroutine started via Run, or directly in unit tests.
func (w *LeaderUniquenessWatcher) poll() {
	workers := w.registry.GetByType(WorkerGoroutine)
	leaders := 0
	for _, info := range workers {
		if obs, ok := info.Obj.(WorkerObserver); ok {
			if obs.IsLeader() {
				leaders++
			}
		}
	}
	if leaders > 1 {
		if w.chaosStarted.Load() {
			log.Printf("[LeaderUniquenessWatcher] DOUBLE_LEADER_POST_CHAOS leaders=%d (informational)", leaders)
			w.doubleLeaderPostChaos.Add(1)
		} else {
			log.Printf("[LeaderUniquenessWatcher] DOUBLE_LEADER leaders=%d", leaders)
			w.doubleLeaderCount.Add(1)
		}
	}
}

// Run starts a background polling goroutine at the given interval. Returns
// immediately; the goroutine stops when ctx is cancelled.
func (w *LeaderUniquenessWatcher) Run(ctx interface{ Done() <-chan struct{} }, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.poll()
			}
		}
	}()
}

// DoubleLeaderObservations returns the total number of polling instants where
// more than one worker was simultaneously the leader.
func (w *LeaderUniquenessWatcher) DoubleLeaderObservations() int64 {
	return w.doubleLeaderCount.Load()
}

// ---------------------------------------------------------------------------
// State-reconcile watcher (Gap 15)
// ---------------------------------------------------------------------------

// workerAssignmentSnap records a worker's latest assignment snapshot.
type workerAssignmentSnap struct {
	partitions []int
	at         time.Time
}

// StateReconcileWatcher polls each registered worker's StateInt() every K
// seconds and fires a violation when a worker reporting StateStable has a
// non-empty assignment older than K seconds AND no recent message.
//
// Pre-chaos violations go to the fail-run counter. Post-chaos (after
// MarkChaosStarted) violations go to a separate informational counter so
// that chaos configs with scale events do not produce false positives.
type StateReconcileWatcher struct {
	mu                  sync.Mutex
	registry            *GoroutineRegistry
	k                   time.Duration
	chaosStarted        atomic.Bool
	assignments         map[string]*workerAssignmentSnap // workerID → latest assignment
	lastMessage         map[string]time.Time             // workerID → last ReceivedMessage time
	violations          atomic.Int64
	violationsPostChaos atomic.Int64
}

// NewStateReconcileWatcher creates a watcher with the given K window.
func NewStateReconcileWatcher(registry *GoroutineRegistry, k time.Duration) *StateReconcileWatcher {
	return &StateReconcileWatcher{
		registry:    registry,
		k:           k,
		assignments: make(map[string]*workerAssignmentSnap),
		lastMessage: make(map[string]time.Time),
	}
}

// MarkChaosStarted notifies the watcher that chaos events are underway. After this
// call, state-reconcile violations go to the post-chaos counter and do not fail the
// run. Idempotent.
func (w *StateReconcileWatcher) MarkChaosStarted() {
	w.chaosStarted.Store(true)
}

// RecordAssignment records (or updates) a worker's assignment snapshot. Call
// this from the coordinator's assignment-ingest path (processAssignments) so
// the watcher's view stays in sync with the coordinator's view.
func (w *StateReconcileWatcher) RecordAssignment(workerID string, partitions []int, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cp := make([]int, len(partitions))
	copy(cp, partitions)
	w.assignments[workerID] = &workerAssignmentSnap{partitions: cp, at: at}
}

// RecordMessage records that a worker sent a message now. Call this from the
// coordinator's received-message path.
func (w *StateReconcileWatcher) RecordMessage(workerID string, _ int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastMessage[workerID] = time.Now()
}

// poll evaluates the state-reconcile invariant at time now.
func (w *StateReconcileWatcher) poll(now time.Time) {
	workers := w.registry.GetByType(WorkerGoroutine)

	w.mu.Lock()
	defer w.mu.Unlock()

	for _, info := range workers {
		obs, ok := info.Obj.(WorkerObserver)
		if !ok {
			continue
		}
		if obs.WorkerStateInt() != workerStateStable {
			continue
		}
		workerID := info.ID

		// Check message evidence.
		lastMsg, hasMsg := w.lastMessage[workerID]
		recentMsg := hasMsg && now.Sub(lastMsg) <= w.k

		// Check assignment evidence.
		snap := w.assignments[workerID]
		hasNonEmptyStaleAssignment := snap != nil && len(snap.partitions) > 0 && now.Sub(snap.at) > w.k

		// Violation: StateStable with a non-empty assignment older than k AND no
		// recent message. Workers with empty (or no) assignments are excluded because
		// legitimately scaled-down workers have zero partitions.
		if hasNonEmptyStaleAssignment && !recentMsg {
			if w.chaosStarted.Load() {
				log.Printf("[StateReconcileWatcher] VIOLATION_POST_CHAOS worker=%s state=stable recentMsg=%v staleSince=%v partitions=%v (informational)",
					workerID, recentMsg, now.Sub(snap.at).Truncate(time.Millisecond), snap.partitions)
				w.violationsPostChaos.Add(1)
			} else {
				log.Printf("[StateReconcileWatcher] VIOLATION worker=%s state=stable recentMsg=%v staleSince=%v partitions=%v",
					workerID, recentMsg, now.Sub(snap.at).Truncate(time.Millisecond), snap.partitions)
				w.violations.Add(1)
			}
		}
	}
}

// Run starts a background polling goroutine at the given interval.
func (w *StateReconcileWatcher) Run(ctx interface{ Done() <-chan struct{} }, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				w.poll(t)
			}
		}
	}()
}

// StateReconcileViolations returns the total number of state-reconcile
// violations detected.
func (w *StateReconcileWatcher) StateReconcileViolations() int64 {
	return w.violations.Load()
}

// ---------------------------------------------------------------------------
// Degraded-reason oracle (Phase 1)
// ---------------------------------------------------------------------------

// degradedExpectation records what a chaos-injected bucket primitive expects
// to observe in the WorkerDegradedReport stream.
//
//   - reasonSubstrings: any one of these must appear as a substring of the
//     reported Reason to count as "observed". Multiple substrings let the
//     oracle accept either of the two production paths (recordKVError vs
//     epoch-fence) without coupling to which fires first.
//   - deadline: time.Time after which a missing observation is final.
//   - targetWorker: peer-takeover only — the StableWorkerID expected to
//     reach Shutdown; "" means any (used by whole-bucket primitives).
//   - kind: human-readable chaos kind for log lines.
type degradedExpectation struct {
	kind             string
	reasonSubstrings []string
	deadline         time.Time
	targetWorker     string
	observedWorkers  map[string]struct{}
	// claimLostExpected is true when the chaos kind is supposed to drive
	// claimLostShutdown for exactly one worker (peer-takeover); false for
	// whole-bucket primitives where claimLostShutdown is the WRONG path.
	claimLostExpected bool
	// completed is set true once an observation matched. Whole-bucket
	// primitives still keep collecting observedWorkers until deadline so
	// the count can be checked separately.
	completed bool
}

// DegradedReasonOracle correlates active bucket-chaos expectations against the
// WorkerDegradedReport stream and bumps three counters:
//
//   - expected_degraded_observed: an active expectation matched at least one
//     report.
//   - expected_degraded_missing: an expectation's deadline elapsed without
//     a single matching report.
//   - unexpected_claim_lost_shutdown: a worker reached Shutdown when no active
//     expectation predicted it (i.e. the whole-bucket-loss path mis-routed
//     into claimLostShutdown). Caller calls ObserveClaimLostShutdown from the
//     worker-state observer.
//
// Concurrency: the oracle is safe under multi-producer ingest (one Ingest per
// worker degraded hook) and a single periodic Sweep tick. All state is
// mu-guarded except the atomic counters which are reads-only externally.
type DegradedReasonOracle struct {
	mu                          sync.Mutex
	active                      []*degradedExpectation
	expectedDegradedObserved    atomic.Int64
	expectedDegradedMissing     atomic.Int64
	unexpectedClaimLostShutdown atomic.Int64

	// suppressedClaimLost maps stable worker IDs to the deadline before
	// which a claimLostShutdown observation is silently tolerated (not
	// counted as unexpected). Used by long-disconnect chaos to suppress
	// the conditional claim-lost that can fire when the disconnect duration
	// approaches WorkerIDTTL.
	suppressedClaimLost map[string]time.Time
}

// NewDegradedReasonOracle constructs an oracle ready to accept expectations.
func NewDegradedReasonOracle() *DegradedReasonOracle {
	return &DegradedReasonOracle{}
}

// ExpectAfter registers an expectation for the given chaos kind.
//
//   - kind: human label, e.g. "bucket_delete:parti-stableid"
//   - reasonSubstrings: any one substring matches; e.g. for bucket_delete on
//     stableid this is ["bucket-unavailable:", "bucket-recreated:parti-stableid"].
//   - within: how long after now the expectation is valid; missing past
//     deadline increments expected_degraded_missing.
//   - targetWorker: stable-ID string for peer-takeover only; "" for whole-
//     bucket primitives.
//   - claimLostExpected: true for peer-takeover (exactly one worker should
//     reach Shutdown), false for whole-bucket primitives.
func (o *DegradedReasonOracle) ExpectAfter(kind string, reasonSubstrings []string, within time.Duration, targetWorker string, claimLostExpected bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	// Idempotency: if an active expectation already exists for the same
	// (kind, targetWorker) tuple, extend its deadline rather than
	// stacking a second expectation. Production OnDegraded hooks fire
	// only on TRANSITION to degraded, so multiple chaos events of the
	// same kind cannot satisfy multiple stacked expectations.
	for _, exp := range o.active {
		if exp.kind == kind && exp.targetWorker == targetWorker {
			newDeadline := time.Now().Add(within)
			if newDeadline.After(exp.deadline) {
				exp.deadline = newDeadline
			}
			return
		}
	}
	exp := &degradedExpectation{
		kind:              kind,
		reasonSubstrings:  append([]string(nil), reasonSubstrings...),
		deadline:          time.Now().Add(within),
		targetWorker:      targetWorker,
		observedWorkers:   make(map[string]struct{}),
		claimLostExpected: claimLostExpected,
	}
	o.active = append(o.active, exp)
}

// Ingest is the WorkerDegradedReport sink. Called from the coordinator's
// degraded-report processing goroutine.
func (o *DegradedReasonOracle) Ingest(r WorkerDegradedReport) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	for _, exp := range o.active {
		if exp.completed && exp.claimLostExpected {
			// peer-takeover only needs one match; skip
			continue
		}
		if now.After(exp.deadline) {
			continue
		}
		if !matchesAnySubstring(r.Reason, exp.reasonSubstrings) {
			continue
		}
		if exp.targetWorker != "" && r.WorkerID != exp.targetWorker {
			continue
		}
		if _, dup := exp.observedWorkers[r.WorkerID]; !dup {
			exp.observedWorkers[r.WorkerID] = struct{}{}
		}
		if !exp.completed {
			exp.completed = true
			o.expectedDegradedObserved.Add(1)
			log.Printf("[DegradedReasonOracle] OBSERVED kind=%s worker=%s reason=%s", exp.kind, r.WorkerID, r.Reason)
		}
	}
}

// ObserveClaimLostShutdown is called by the coordinator when it detects a
// worker transitioned to Shutdown via the claim-lost path. The oracle
// classifies the transition against active expectations:
//
//   - If an active peer-takeover expectation targets this worker, the
//     transition is expected; ignored.
//   - Otherwise, increments unexpected_claim_lost_shutdown.
//
// SuppressClaimLostForWorker registers a window during which a
// claimLostShutdown observation for stableWorkerID is silently tolerated.
// Used by long-disconnect chaos where claim-lost is a possible but not
// guaranteed outcome (the stable-ID key MAY expire if the disconnect
// duration approaches WorkerIDTTL - renewal_interval). Unlike ExpectAfter,
// this does NOT require the claim-lost to fire, and does NOT increment
// expectedDegradedMissing if it does not.
func (o *DegradedReasonOracle) SuppressClaimLostForWorker(stableWorkerID string, within time.Duration) {
	if stableWorkerID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.suppressedClaimLost == nil {
		o.suppressedClaimLost = make(map[string]time.Time)
	}
	deadline := time.Now().Add(within)
	// Extend if a later deadline already exists.
	if existing, ok := o.suppressedClaimLost[stableWorkerID]; !ok || deadline.After(existing) {
		o.suppressedClaimLost[stableWorkerID] = deadline
	}
	log.Printf("[DegradedReasonOracle] Suppressed claim-lost for worker %s until %v", stableWorkerID, deadline.Format(time.RFC3339))
}

func (o *DegradedReasonOracle) ObserveClaimLostShutdown(workerID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	for _, exp := range o.active {
		if !exp.claimLostExpected {
			continue
		}
		if now.After(exp.deadline.Add(5 * time.Second)) {
			// allow a small grace beyond the degraded-observe deadline
			continue
		}
		if exp.targetWorker == workerID {
			// Mark the expectation as observed so Sweep doesn't later
			// classify it as missing. This is the positive Phase 4b
			// signal: claim-lost FIRED for the target worker.
			if !exp.completed {
				exp.completed = true
				o.expectedDegradedObserved.Add(1)
			}
			log.Printf("[DegradedReasonOracle] CLAIM_LOST_OK worker=%s kind=%s", workerID, exp.kind)

			return
		}
	}
	// Check suppression window (long-disconnect conditional tolerance).
	if o.suppressedClaimLost != nil {
		if deadline, suppressed := o.suppressedClaimLost[workerID]; suppressed {
			if now.Before(deadline) {
				log.Printf("[DegradedReasonOracle] CLAIM_LOST_SUPPRESSED worker=%s (long-disconnect window)", workerID)
				return
			}
		}
	}
	log.Printf("[DegradedReasonOracle] UNEXPECTED_CLAIM_LOST worker=%s", workerID)
	o.unexpectedClaimLostShutdown.Add(1)
}

// Sweep is called periodically; expectations whose deadline has elapsed
// without observation are recorded as missing.
func (o *DegradedReasonOracle) Sweep(now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	kept := o.active[:0]
	for _, exp := range o.active {
		if now.Before(exp.deadline) {
			kept = append(kept, exp)
			continue
		}
		if !exp.completed {
			log.Printf("[DegradedReasonOracle] MISSING kind=%s reasons=%v target=%s deadline=%v",
				exp.kind, exp.reasonSubstrings, exp.targetWorker, exp.deadline.Format(time.RFC3339))
			o.expectedDegradedMissing.Add(1)
		}
		// Drop completed/expired entries.
	}
	o.active = kept
}

// Run starts a background goroutine that calls Sweep at the given interval.
// On ctx.Done it performs one final Sweep so deadlines that pass during the
// shutdown window are still classified.
func (o *DegradedReasonOracle) Run(ctx interface{ Done() <-chan struct{} }, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				// Final sweep to classify any expectations whose
				// deadlines have already elapsed at shutdown time.
				o.Sweep(time.Now())
				return
			case t := <-ticker.C:
				o.Sweep(t)
			}
		}
	}()
}

// ExpectedDegradedObserved returns the count of expectations that observed
// at least one matching WorkerDegradedReport.
func (o *DegradedReasonOracle) ExpectedDegradedObserved() int64 {
	return o.expectedDegradedObserved.Load()
}

// ExpectedDegradedMissing returns the count of expectations whose deadlines
// elapsed without a matching WorkerDegradedReport.
func (o *DegradedReasonOracle) ExpectedDegradedMissing() int64 {
	return o.expectedDegradedMissing.Load()
}

// UnexpectedClaimLostShutdown returns the count of claim-lost shutdowns that
// were not predicted by any active peer-takeover expectation.
func (o *DegradedReasonOracle) UnexpectedClaimLostShutdown() int64 {
	return o.unexpectedClaimLostShutdown.Load()
}

// matchesAnySubstring returns true if any of subs appears as a substring of s.
// Substring (not equality) so callers can match an entire "bucket-unavailable:"
// family by passing only the family prefix.
func matchesAnySubstring(s string, subs []string) bool {
	for _, sub := range subs {
		if sub == "" {
			continue
		}
		if strings.Contains(s, sub) {
			return true
		}
	}

	return false
}

// ---------------------------------------------------------------------------
// Claim-loss stop-ordering oracle (Phase 4)
// ---------------------------------------------------------------------------

// ClaimLossOrderingOracle asserts the stop-before-revoke ordering of the
// claimLostShutdown path. The production contract
// (manager_election.go:claimLostShutdown) is:
//
//  1. Manager.Stop tears down the apply loop (cancels m.ctx, waits for
//     in-flight applies to drain).
//  2. AFTER Stop returns, revokeWorkerConsumer drives a zero-partition
//     UpdateWorkerConsumer call to the application's consumer updater.
//
// A regression that revokes BEFORE Stop would race the apply loop and could
// re-apply a non-empty assignment onto the worker consumer immediately after
// the revoke, re-introducing a brief double-claim window. The oracle counts:
//
//   - claim_loss_stop_ordering_violations: A RevocationReport for worker W
//     arrived BEFORE the watcher observed W's StateShutdown transition; OR a
//     ReceivedMessage for one of W's last-known-assigned partitions arrived
//     STRICTLY AFTER the observed StateShutdown timestamp.
//
// The oracle uses the StableWorkerID as the join key because that's what the
// claim-lost ObserveClaimLostShutdown path reports. Assignment snapshots are
// indexed by both sim-side workerID and stableID so the partition lookup
// works even when a chaos event removes the registry entry before the
// ObserveClaimLostShutdown call.
// WorkerStateSampler returns the current manager state int for a sim worker
// (e.g. "worker-0"). The second return is false when the sampler has no entry
// for that ID (e.g. the registry entry was removed). Used by the claim-loss
// ordering oracle to distinguish a true Stop-before-revoke ordering bug from
// a poll-cadence race: when a revoke arrives without a prior shutdown
// observation, the sampler is consulted to ask "is the worker IN shutdown
// state right now?" — yes → backfill (the watcher was just late), no →
// violation.
type WorkerStateSampler func(simWorkerID string) (state int, known bool)

type ClaimLossOrderingOracle struct {
	mu sync.Mutex

	// stateSampler, if non-nil, is consulted by ObserveRevocation when a
	// revocation arrives without a prior shutdown observation. It MUST NOT
	// be called while holding o.mu (the sampler may take other locks /
	// reach into the registry; we keep the no-callout-under-lock invariant
	// uniform with the rest of the file). Set via SetStateSampler before
	// the oracle is wired into the coordinator's revocation drain.
	stateSampler WorkerStateSampler

	// shutdownBySim[simWorkerID] = time the watcher first observed
	// StateShutdown for the SIM-side worker (e.g. "worker-0"). The
	// watcher reports a stable-ID; we resolve that to a sim worker via
	// simToStable at observe time. Key by sim ID so a successor worker
	// that reclaims the same stable-ID via stale-takeover does NOT
	// inherit the predecessor's shutdown record.
	shutdownBySim map[string]time.Time

	// lastAssignmentBySim[simWorkerID] = partition set the worker was
	// assigned at the moment ObserveShutdown landed for its stable-ID.
	lastAssignmentBySim map[string]map[int]struct{}

	// simToStable[simWorkerID] = stableID. Idempotently updated; the
	// LATEST mapping is authoritative (a sim worker can only ever hold
	// one stable ID at a time).
	simToStable map[string]string

	// stableToSimAtShutdown[stableID] = simWorkerID that held that stable
	// ID at the moment of shutdown. Used by ObserveRevocation to find the
	// correct sim worker when only the stable ID is known.
	stableToSimAtShutdown map[string]string

	// assignmentBySim[simWorkerID] = partition set most recently reported.
	assignmentBySim map[string]map[int]struct{}

	violations atomic.Int64
}

// NewClaimLossOrderingOracle constructs an empty oracle.
func NewClaimLossOrderingOracle() *ClaimLossOrderingOracle {
	return &ClaimLossOrderingOracle{
		shutdownBySim:         make(map[string]time.Time),
		lastAssignmentBySim:   make(map[string]map[int]struct{}),
		simToStable:           make(map[string]string),
		stableToSimAtShutdown: make(map[string]string),
		assignmentBySim:       make(map[string]map[int]struct{}),
	}
}

// SetStateSampler installs a sampler used by ObserveRevocation to confirm a
// worker is actually in Shutdown state at revoke time. Without a sampler the
// oracle preserves the legacy "backfill on missing shutdown" behavior (kept
// for the no-coordinator unit tests). Production wiring in
// EnableShutdownOracles MUST install a sampler so revoke-before-shutdown
// is no longer silently absorbed.
func (o *ClaimLossOrderingOracle) SetStateSampler(s WorkerStateSampler) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stateSampler = s
}

// RegisterStableID records the mapping from the sim-side worker ID to the
// stable worker ID claimed by its manager. Idempotent.
func (o *ClaimLossOrderingOracle) RegisterStableID(simWorkerID, stableID string) {
	if simWorkerID == "" || stableID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.simToStable[simWorkerID] = stableID
}

// RecordAssignment records that the worker simWorkerID currently holds the
// given partitions. Called from the AssignmentReport ingest path.
func (o *ClaimLossOrderingOracle) RecordAssignment(simWorkerID string, partitions []int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	set := make(map[int]struct{}, len(partitions))
	for _, p := range partitions {
		set[p] = struct{}{}
	}
	o.assignmentBySim[simWorkerID] = set
}

// ObserveShutdown records the StateShutdown observation timestamp for the
// SIM worker identified by simWorkerID, which currently holds stableID.
// stableID may be empty — it's used to populate stableToSimAtShutdown for
// ObserveRevocation's lookup when only the stable-ID is reported.
//
// Keying the shutdown by SIM worker (not stable-ID) is required: when a
// successor sim worker reclaims the same stable-ID via stale-takeover,
// its post-takeover messages must not be flagged against the
// predecessor's shutdown record.
func (o *ClaimLossOrderingOracle) ObserveShutdown(simWorkerID, stableID string, at time.Time) {
	if simWorkerID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	simID := simWorkerID
	if _, seen := o.shutdownBySim[simID]; seen {
		return
	}
	o.shutdownBySim[simID] = at
	o.stableToSimAtShutdown[stableID] = simID
	if parts, ok := o.assignmentBySim[simID]; ok && len(parts) > 0 {
		snap := make(map[int]struct{}, len(parts))
		for p := range parts {
			snap[p] = struct{}{}
		}
		o.lastAssignmentBySim[simID] = snap
	}
}

// ObserveRevocation is called when the revocationObservingUpdater emits a
// RevocationReport (zero-partition UpdateWorkerConsumer call). The
// production contract is "Stop then revoke" — the watcher MUST see the
// StateShutdown transition before the revoke fires. However, two
// observation races can produce a false-positive revoke_before_shutdown:
//
//  1. Watcher poll cadence (250ms) lags the in-process transition: Stop()
//     returns, the revoke goroutine resumes and fires before the watcher's
//     next tick. The Manager state IS shutdown; only our observation is
//     late.
//  2. The Coordinator goroutines drain channels in distinct goroutines, so
//     the revocation channel can be drained before the watcher goroutine
//     gets its next tick.
//
// To distinguish a true ordering bug from a poll-cadence race, the oracle
// also samples the worker's CURRENT state via the registry at observe
// time. If WorkerStateInt() == workerStateShutdown when the revoke is
// observed, we backfill an "implicit shutdown at revoke time minus
// epsilon" entry rather than counting a violation. Only if the worker is
// NOT in shutdown state at observation time do we flag.
func (o *ClaimLossOrderingOracle) ObserveRevocation(simWorkerID, stableID string, at time.Time) {
	if simWorkerID == "" && stableID != "" {
		o.mu.Lock()
		for sim, sid := range o.simToStable {
			if sid == stableID {
				simWorkerID = sim
				break
			}
		}
		o.mu.Unlock()
	}
	if simWorkerID == "" {
		return
	}
	// Snapshot the sampler reference under lock, then call it OUTSIDE the
	// lock — the sampler walks the registry and reads observer atomics,
	// and we keep the file-wide convention of no callouts under o.mu.
	o.mu.Lock()
	sampler := o.stateSampler
	o.mu.Unlock()

	// If a sampler is installed, decide tolerate-vs-violate up front so the
	// branch below isn't tangled with the lookup.
	var (
		samplerKnown      bool
		samplerInShutdown bool
	)
	if sampler != nil {
		st, known := sampler(simWorkerID)
		samplerKnown = known
		samplerInShutdown = known && st == workerStateShutdown
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	tShutdown, seen := o.shutdownBySim[simWorkerID]
	if !seen {
		// Revoke arrived without a prior watcher-observed Shutdown.
		// The sim-observable revoke surface is DUAL-PURPOSE:
		//
		//   1. claimLostShutdown -> revokeWorkerConsumer
		//      (manager_election.go:79) — fires AFTER Stop transitions
		//      the state to Shutdown. This is the regression case the
		//      oracle was originally written to catch.
		//   2. The apply loop calling
		//      UpdateWorkerConsumer(workerID, nil) for any legitimate
		//      rebalance-to-empty (manager.go:653 cold-empty bootstrap;
		//      any leader-driven reassignment that drops a worker to
		//      zero partitions). State stays Stable; no Shutdown ever
		//      fires. Returning workers in long-disconnect transit
		//      through this path while the leader rebalances back.
		//
		// We cannot distinguish (1) from (2) from sim-observable state
		// alone — production routes both through the same
		// WorkerConsumerUpdater surface, and the apply path does not
		// emit a discriminator. Counting any revoke-without-Shutdown
		// as a violation produces false positives on every long
		// disconnect / scale-down test. The selective sampler check
		// (sample state RIGHT NOW; Shutdown ⇒ backfill) handles the
		// poll-cadence race for (1) but cannot rule (2) in or out.
		//
		// Therefore: revoke-before-Shutdown is logged as audit-only
		// here. The sharp negative signal for the Stop-before-revoke
		// regression is ObserveMessage below — any ReceivedMessage
		// frame for a previously-assigned partition arriving STRICTLY
		// AFTER an observed Shutdown unambiguously demonstrates the
		// invariant break, with no ambiguity from rebalance traffic.
		// post-impl-review v2 P1 #1 fix: only backfill shutdownBySim when
		// the sampler CONFIRMS the worker is currently in Shutdown (the
		// poll-cadence race the backfill was originally for) OR when no
		// sampler is installed (legacy behavior, preserved for unit-test
		// setups without a coordinator). When a sampler is installed but
		// reports a non-shutdown state (Stable / rebalance / lag) OR
		// reports unknown, we leave the revoke audit-only: NO write to
		// shutdownBySim, so later ObserveMessage frames for these
		// partitions DO NOT spuriously trip the post_shutdown_message
		// gate. The only post-shutdown violation that fires is one
		// preceded by a REAL ObserveShutdown.
		reason := "no_sampler"
		backfill := sampler == nil
		switch {
		case sampler != nil && samplerInShutdown:
			reason = "sampler_confirms_shutdown"
			backfill = true
		case sampler != nil && samplerKnown:
			reason = "sampler_reports_non_shutdown_rebalance_or_lag"
		case sampler != nil:
			reason = "sampler_reports_unknown"
		}
		if backfill {
			o.shutdownBySim[simWorkerID] = at
			if stableID != "" {
				o.stableToSimAtShutdown[stableID] = simWorkerID
			}
			if parts, ok := o.assignmentBySim[simWorkerID]; ok && len(parts) > 0 {
				snap := make(map[int]struct{}, len(parts))
				for p := range parts {
					snap[p] = struct{}{}
				}
				o.lastAssignmentBySim[simWorkerID] = snap
			}
		}
		log.Printf("[ClaimLossOrderingOracle] revoke_backfill worker=%s stable=%s at=%v reason=%s backfilled=%t",
			simWorkerID, stableID, at.Format(time.RFC3339Nano), reason, backfill)

		return
	}
	if at.Before(tShutdown) {
		log.Printf("[ClaimLossOrderingOracle] VIOLATION revoke_at=%v < shutdown_at=%v worker=%s stable=%s",
			at.Format(time.RFC3339Nano), tShutdown.Format(time.RFC3339Nano), simWorkerID, stableID)
		o.violations.Add(1)
	}
}

// ObserveMessage is called from the ReceivedMessage path. The contract is
// "no ReceivedMessage with worker=W for one of W's previously-assigned
// partitions arrived AFTER the watcher observed W's StateShutdown". Both
// the assignment snapshot AND the message attribution are keyed by
// simWorkerID — a different sim worker that takes over the same stable
// ID via stale-takeover is GOOD behavior, not a violation, so the lookup
// MUST NOT cross sim-worker boundaries.
func (o *ClaimLossOrderingOracle) ObserveMessage(simWorkerID string, partition int, at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	tShutdown, seen := o.simShutdownAt(simWorkerID)
	if !seen {
		return
	}
	if at.Before(tShutdown) || at.Equal(tShutdown) {
		return
	}
	parts, ok := o.simLastAssignment(simWorkerID)
	if !ok {
		return
	}
	if _, owned := parts[partition]; !owned {
		return
	}
	log.Printf("[ClaimLossOrderingOracle] VIOLATION post_shutdown_message worker=%s partition=%d msg_at=%v shutdown_at=%v",
		simWorkerID, partition, at.Format(time.RFC3339Nano), tShutdown.Format(time.RFC3339Nano))
	o.violations.Add(1)
}

// simShutdownAt returns the shutdown timestamp for the SIM worker.
// Caller must hold o.mu.
func (o *ClaimLossOrderingOracle) simShutdownAt(simWorkerID string) (time.Time, bool) {
	t, seen := o.shutdownBySim[simWorkerID]

	return t, seen
}

// simLastAssignment returns the partition set the SIM worker held at the
// instant of its shutdown observation. Caller must hold o.mu.
func (o *ClaimLossOrderingOracle) simLastAssignment(simWorkerID string) (map[int]struct{}, bool) {
	parts, ok := o.lastAssignmentBySim[simWorkerID]

	return parts, ok
}

// Violations returns the cumulative count of stop-ordering violations.
func (o *ClaimLossOrderingOracle) Violations() int64 {
	return o.violations.Load()
}
