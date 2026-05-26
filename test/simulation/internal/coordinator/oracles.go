package coordinator

import (
	"log"
	"slices"
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
