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
			log.Printf("[DegradedReasonOracle] CLAIM_LOST_OK worker=%s kind=%s", workerID, exp.kind)
			return
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
