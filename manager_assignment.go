package parti

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	rand "math/rand/v2"
	"slices"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment"
	"github.com/arloliu/parti/v2/internal/heartbeat"
	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	watcherBaseBackoff = 2 * time.Second
	watcherMaxBackoff  = 30 * time.Second
	watcherJitter      = 0.3 // ±30% jitter

	// commitKeyName mirrors the publisher's "_commit" sub-component constant.
	commitKeyName = "_commit"
)

// commitReconcileInterval is the period between idempotent KV re-reads of
// the commit key. Recovers from missed watcher events (channel close
// gaps, NATS reconnects) without depending on the watcher's resync.
//
// Declared as a package-level var (not a const) so reconcile-timing tests
// can override it via export_test.go without depending on a 30s wall-clock
// timer. Production callers MUST NOT mutate this value.
var commitReconcileInterval = 30 * time.Second

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

	if _, ok := calc.(*assignment.NopCalculator); ok {
		return errors.New("calculator not initialized")
	}

	m.logger.Info("refreshing partitions and triggering rebalance")

	// Trigger rebalance which will call source.ListPartitions() to get fresh partition list
	if err := calc.TriggerRebalance(ctx); err != nil {
		return fmt.Errorf("failed to trigger rebalance: %w", err)
	}

	return nil
}

// startCalculator starts the assignment calculator (leader only).
func (m *Manager) startCalculator(assignmentKV, heartbeatKV jetstream.KeyValue) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.calculator.(*assignment.Calculator); ok {
		return nil // Already started
	}

	calc, err := assignment.NewCalculator(&assignment.Config{
		AssignmentKV:          assignmentKV,
		HeartbeatKV:           heartbeatKV,
		Source:                m.source,
		Strategy:              m.strategy,
		AssignmentPrefix:      "assignment",
		HeartbeatPrefix:       "heartbeat",
		HeartbeatTTL:          m.cfg.HeartbeatTTL,
		EmergencyGracePeriod:  m.cfg.EmergencyGracePeriod,
		Cooldown:              m.cfg.RebalanceCooldown,
		RestartRatio:          m.cfg.RestartDetectionRatio,
		ColdStartWindow:       m.cfg.ColdStartWindow,
		PlannedScaleWindow:    m.cfg.PlannedScaleWindow,
		EnableTwoPhaseHandoff: m.cfg.EnableTwoPhaseHandoff,
		Metrics:               m.metrics,
		Logger:                m.logger,
		StateProvider:         m, // Pass manager as state provider for degraded mode checks
		LeaderRevision:        m.electionRevision,
		LeaderCheck:           m.checkElectionLeadership,
	})
	if err != nil {
		return fmt.Errorf("failed to create calculator: %w", err)
	}

	m.calculator = calc

	// Start monitoring calculator state BEFORE starting the calculator.
	// Pass calc directly (not via m.mu) so the goroutine can subscribe without
	// waiting for the write lock that startCalculator holds.
	// The ready channel blocks until the subscription is established.
	readyCh := make(chan struct{})
	m.wg.Go(func() { m.monitorCalculatorState(calc, readyCh) })
	<-readyCh

	// Start calculator in background. On failure, restore the Nop default
	// (not nil) so any lifecycle method that reads m.calculator continues to
	// work without a nil-check.
	if err := calc.Start(m.ctx); err != nil {
		m.calculator = assignment.NewNopCalculator()
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
// readyCh is closed once the subscription is established, signalling the caller
// that no state changes can be missed from that point onward.
//
// This method runs only on the leader and translates calculator states to Manager states:
//   - types.CalcStateScaling → StateScaling
//   - types.CalcStateRebalancing → StateRebalancing
//   - types.CalcStateEmergency → StateEmergency
//   - types.CalcStateIdle (after rebalancing) → StateStable
func (m *Manager) monitorCalculatorState(calc assignmentCalculator, readyCh chan struct{}) {
	m.logger.Info("starting calculator state monitor")

	stateCh, unsubscribe := calc.SubscribeToStateChanges()
	close(readyCh) // Subscription established — caller may now start the calculator
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

// stopCalculator stops the assignment calculator, returning true if it was running.
func (m *Manager) stopCalculator() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	calc, ok := m.calculator.(*assignment.Calculator)
	if !ok {
		return false
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
			m.transitionState(StateStable)
			m.logger.Info("transitioned to Stable after losing leadership",
				"worker_id", m.WorkerID(),
				"from_state", currentState.String(),
			)
		} else {
			m.transitionState(StateWaitingAssignment)
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
	stopCtx, stopCancel := context.WithTimeout(context.Background(), m.cfg.OperationTimeout)
	defer stopCancel()

	if err := calc.Stop(stopCtx); err != nil {
		m.logError("failed to stop calculator", "error", err)
	}

	m.calculator = assignment.NewNopCalculator()

	return true
}

// calculateAndPublish calculates and publishes assignments.
func (m *Manager) calculateAndPublish(ctx context.Context) error {
	m.mu.RLock()
	calc := m.calculator
	m.mu.RUnlock()

	if _, ok := calc.(*assignment.NopCalculator); ok {
		return errors.New("calculator not started")
	}

	// Calculator runs in background and publishes automatically.
	// Wait briefly for the initial calculation, respecting the context.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(500 * time.Millisecond):
	}

	return nil
}

// fetchAssignment fetches the assignment for this worker from KV.
func (m *Manager) fetchAssignment(ctx context.Context, kv jetstream.KeyValue) (*Assignment, error) {
	workerID := m.WorkerID()
	key := fmt.Sprintf("assignment.%s", workerID) // Match calculator's key format

	asgn, _, err := kvutil.GetJSON[Assignment](ctx, kv, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get assignment: %w", err)
	}

	return asgn, nil
}

// monitorAssignmentChanges monitors for assignment changes with automatic retry.
//
// On watcher failure (e.g., transient NATS error), the monitor retries with
// exponential backoff and jitter, capped at watcherMaxBackoff. A clean exit
// (context cancelled or watcher channel closed) stops the loop immediately.
func (m *Manager) monitorAssignmentChanges(ctx context.Context, kv jetstream.KeyValue) {
	backoff := watcherBaseBackoff
	for {
		err := m.watchAssignment(ctx, kv)
		if err == nil || ctx.Err() != nil {
			return
		}
		m.logError("assignment watcher failed, retrying", "error", err, "backoff", backoff)
		// Feed into degraded circuit: a wiped assignment bucket repeatedly
		// fails here; connection-loss errors also land here.
		m.recordKVError(err)

		//nolint:gosec // jitter does not require crypto-secure random
		f := rand.Float64()
		low := 1 - watcherJitter
		high := 1 + watcherJitter
		delay := time.Duration(float64(backoff) * (low + f*(high-low)))

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		// Double for next attempt, capped at max.
		backoff = min(backoff*2, watcherMaxBackoff)
	}
}

// watchAssignment runs one watch session on this worker's assignment key.
// Returns nil on clean exit (context cancelled or channel closed normally),
// error if the watcher could not be established.
func (m *Manager) watchAssignment(ctx context.Context, kv jetstream.KeyValue) error {
	workerID := m.WorkerID()
	key := fmt.Sprintf("assignment.%s", workerID) // Match calculator's key format

	watcher, err := kv.Watch(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to watch assignments: %w", err)
	}

	defer func() {
		if err := watcher.Stop(); err != nil && !natsutil.IsConsumerNotFound(err) {
			m.logError("failed to stop watcher", "error", err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			m.logger.Debug("assignment monitor stopping (context cancelled)", "worker_id", workerID)
			return nil
		case entry, ok := <-watcher.Updates():
			if !ok {
				m.logger.Debug("assignment watcher closed", "worker_id", workerID)
				return nil
			}
			if entry == nil {
				// Nil entry indicates end of initial values replay
				// This is normal - continue watching for future updates
				continue
			}

			m.handleAssignmentEntry(workerID, entry)
		}
	}
}

func (m *Manager) handleAssignmentEntry(workerID string, entry jetstream.KeyValueEntry) {
	if entry.Operation() == jetstream.KeyValueDelete {
		m.logger.Debug("ignoring assignment deletion during leader transition")
		return
	}

	newAssignment, ok := m.decodeAssignmentEntry(entry)
	if !ok {
		return
	}

	// §4.5 stale-leader fence: reject any alias whose LeaderRevision is
	// older than what we have already observed and accepted.
	lastSeen := m.lastSeenLeaderRevision.Load()
	if newAssignment.LeaderRevision != 0 && newAssignment.LeaderRevision < lastSeen {
		m.metrics.RecordStaleLeaderRejected()
		return
	}

	oldAssignment := m.CurrentAssignment()
	if oldAssignment.Version >= newAssignment.Version {
		return
	}

	// Record this as the most-recent legacy alias observation so the
	// dual-read selector on the commit path can consult it.
	aliasCopy := newAssignment
	m.lastSeenAlias.Store(&aliasCopy)

	// Dual-read source-of-truth rule (§3.6). When the commit path has a
	// fresher view (case 1), drop this alias; the commit watcher will
	// drive the apply via handleCommitValue.
	commit := m.lastObservedCommit.Load()
	choice := selectAuthority(commit, &newAssignment, lastSeen)
	if choice != AuthorityLegacyAlias {
		return
	}

	// Legacy alias wins (case 2). Apply through the unified pipeline. The
	// legacy envelope carries zero SourceRevision/SourceRevisionKnown by
	// design — encoded as "unknown" downstream.
	//
	// LSR is advanced inside applyAssignmentWithPrev on success (single
	// source of truth — see comment at the top of applyAssignmentWithPrev).
	// On failure, the stale-leader fence intentionally does not accept this
	// leader revision (the worker has not committed to this leader's term).
	_ = m.applyAssignment(newAssignment)
	_ = workerID // workerID is retained for log/metric symmetry with the legacy signature
}

// monitorCommitChanges watches the singleton "assignment._commit" key. On
// channel close the watcher restarts with exponential backoff (closes A2 /
// §4.3); a periodic reconcile every commitReconcileInterval re-fetches the
// commit and routes idempotently through handleCommitValue so missed
// updates eventually converge.
func (m *Manager) monitorCommitChanges(ctx context.Context, kv jetstream.KeyValue) {
	backoff := watcherBaseBackoff
	reconcileTicker := time.NewTicker(commitReconcileInterval)
	defer reconcileTicker.Stop()
	for {
		err := m.watchCommit(ctx, kv, reconcileTicker.C)
		if err == nil || ctx.Err() != nil {
			return
		}
		m.logError("commit watcher failed, retrying", "error", err, "backoff", backoff)
		m.recordKVError(err)

		//nolint:gosec // jitter does not require crypto-secure random
		f := rand.Float64()
		low := 1 - watcherJitter
		high := 1 + watcherJitter
		delay := time.Duration(float64(backoff) * (low + f*(high-low)))

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		backoff = min(backoff*2, watcherMaxBackoff)
	}
}

// watchCommit runs one watch session on the commit key. Channel closure is
// returned as an error so monitorCommitChanges can restart with backoff;
// context cancellation returns nil for clean exit.
func (m *Manager) watchCommit(ctx context.Context, kv jetstream.KeyValue, reconcileTickC <-chan time.Time) error {
	key := "assignment." + commitKeyName
	watcher, err := kv.Watch(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to watch commit: %w", err)
	}
	defer func() {
		if serr := watcher.Stop(); serr != nil && !natsutil.IsConsumerNotFound(serr) {
			m.logError("failed to stop commit watcher", "error", serr)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case entry, ok := <-watcher.Updates():
			if !ok {
				return errors.New("commit watcher channel closed")
			}
			if entry == nil {
				// End of initial replay; keep watching.
				continue
			}
			m.handleCommitEntry(entry)
		case <-reconcileTickC:
			current, _, gerr := kvutil.GetJSON[types.AssignmentCommit](ctx, kv, key)
			if gerr != nil || current == nil {
				continue
			}
			m.handleCommitValue(current)
		}
	}
}

// handleCommitEntry decodes a watcher entry and routes through
// handleCommitValue. Deletion events are ignored; a deleted commit is
// treated as "no commit", which the dual-read selector handles correctly
// (the legacy alias may still drive).
func (m *Manager) handleCommitEntry(entry jetstream.KeyValueEntry) {
	if entry.Operation() == jetstream.KeyValueDelete {
		return
	}
	var commit types.AssignmentCommit
	if err := json.Unmarshal(entry.Value(), &commit); err != nil {
		m.logError("failed to unmarshal commit", "error", err)
		return
	}
	m.handleCommitValue(&commit)
}

// handleCommitValue implements §3.6 case 1 (commit-path state machine).
//
// State table (LSR = lastSeenLeaderRevision, cur = CurrentAssignment):
//
//	(a) commit.Version <= cur.Version          → no-op, LSR = max(LSR, commit.LR)
//	(b) commit.LR < LSR                        → drop, LSR unchanged, RecordStaleLeaderRejected
//	(c) worker in Workers, payload checks pass → applyAssignment, LSR = max(LSR, commit.LR)
//	(c) payload check fails                    → drop, LSR + pending unchanged, stage-specific metric
//	(d) worker NOT in Workers                  → applyAssignment(empty), LSR = max(LSR, commit.LR)
//	(e) pendingApplyInFlight                   → stash highest-version target, return
func (m *Manager) handleCommitValue(commit *types.AssignmentCommit) {
	// Iterate so case (e) drain runs without recursion while we still hold
	// pendingApplyInFlight. Recursing would re-enter the case (e) CAS check
	// and stash the drained commit back instead of applying it.
	for commit != nil {
		next := m.handleCommitValueOnce(commit)
		commit = next
	}
}

// handleCommitValueOnce processes one commit through the state machine and
// returns the next commit to process (drained from stashedCommit) or nil
// when no further work is pending. The caller (handleCommitValue) loops on
// the result so case (e) drain happens iteratively.
func (m *Manager) handleCommitValueOnce(commit *types.AssignmentCommit) *types.AssignmentCommit {
	if commit == nil {
		return nil
	}
	workerID := m.WorkerID()
	cur := m.CurrentAssignment()

	// Case (a): no-op, but still update lastSeen so the next handler
	// observes the latest leader revision. We also publish to
	// lastObservedCommit so the alias-side dual-read selector sees the
	// freshest commit — case (a) is a legitimate observation, not a reject.
	if commit.Version <= cur.Version {
		commitCopy := *commit
		m.lastObservedCommit.Store(&commitCopy)
		m.updateLastSeenLeaderRevision(commit.LeaderRevision)
		return nil
	}

	// Case (b): stale-leader fence. Do NOT publish to lastObservedCommit —
	// surfacing a rejected commit to the alias-side selector would let it
	// over-rule a legitimate alias arrival (advisor bug #3).
	lastSeen := m.lastSeenLeaderRevision.Load()
	if commit.LeaderRevision < lastSeen {
		m.metrics.RecordStaleLeaderRejected()
		return nil
	}

	// Publish the freshest commit observation now that we know it is not
	// rejected; the alias-side dual-read selector consults this.
	commitCopy := *commit
	m.lastObservedCommit.Store(&commitCopy)

	// Dual-read source-of-truth check (§3.6). If the legacy alias is
	// strictly fresher (case 2), drop this commit until the alias path
	// matches or a fresher commit arrives.
	legacy := m.lastSeenAlias.Load()
	if selectAuthority(commit, legacy, lastSeen) != AuthorityCommit {
		return nil
	}

	// Case (e): coalesce when an apply is already in flight.
	if !m.pendingApplyInFlight.CompareAndSwap(false, true) {
		for {
			prev := m.stashedCommit.Load()
			if prev != nil && prev.Version >= commit.Version {
				return nil
			}
			candidate := *commit
			if m.stashedCommit.CompareAndSwap(prev, &candidate) {
				return nil
			}
		}
	}

	newAssignment, ok := m.buildAssignmentFromCommit(commit, workerID)
	if !ok {
		// Payload verification failure: case (c) drop; lastSeen and stash
		// unchanged. Clear pending-flag explicitly (no defer here so the
		// drain check below sees a consistent flag state) and return.
		m.pendingApplyInFlight.Store(false)
		return nil
	}

	// LSR is advanced inside applyAssignmentWithPrev on success (single
	// source of truth — see comment at the top of applyAssignmentWithPrev).
	// On failure, the stale-leader fence intentionally does not accept this
	// leader revision; the scheduleApplyRetry coalescing path (invoked from
	// applyAssignmentWithPrev on failure) will re-attempt and advance LSR on
	// its own success.
	_ = m.applyAssignment(newAssignment)

	// Clear pendingApplyInFlight BEFORE returning the stashed drain target.
	// The outer loop in handleCommitValue then re-enters handleCommitValueOnce
	// with the drained commit; that call will CAS the flag back to true if
	// it needs to apply, so the case (e) interlock remains correct.
	//
	// Run the drain regardless of apply outcome — on apply failure the
	// stashedApplyRetry mechanism owns retry of newAssignment, but a strictly
	// higher-version commit that arrived during the failed apply still
	// deserves a fresh attempt rather than being orphaned.
	m.pendingApplyInFlight.Store(false)

	if pending := m.stashedCommit.Swap(nil); pending != nil && pending.Version > newAssignment.Version {
		return pending
	}

	return nil
}

// buildAssignmentFromCommit constructs the Assignment this worker must
// apply for the given commit. Returns ok=false on payload-verification
// failure (case (c) drop) — caller leaves lastSeen unchanged so the next
// tick retries.
//
// Case (c) ok: worker in Workers AND payload checks pass → return a fully
// populated Assignment.
// Case (c) malformed: worker in Workers but Payloads[w] is missing →
// RecordCommitPayloadMissing, return false.
// Case (d): worker NOT in Workers → return an empty Assignment carrying
// the commit's metadata (signals implicit revoke).
func (m *Manager) buildAssignmentFromCommit(commit *types.AssignmentCommit, workerID string) (Assignment, bool) {
	inWorkers := slices.Contains(commit.Workers, workerID)

	if !inWorkers {
		// Case (d): synthesize empty assignment.
		return Assignment{
			Version:             commit.Version,
			Lifecycle:           commit.Lifecycle,
			Partitions:          nil,
			LeaderRevision:      commit.LeaderRevision,
			SourceRevision:      commit.SourceRevision,
			SourceRevisionKnown: commit.SourceRevisionKnown,
			TotalWorkers:        len(commit.Workers),
		}, true
	}

	ref, hasRef := commit.Payloads[workerID]
	if !hasRef {
		// Case (c) malformed.
		m.metrics.RecordCommitPayloadMissing()
		return Assignment{}, false
	}

	payload, err := assignment.FetchAndVerifyCommitPayload(m.ctx, m.assignmentKV, ref)
	if err != nil {
		switch {
		case errors.Is(err, assignment.ErrCommitPayloadFetch):
			m.metrics.RecordPayloadFetchError()
		case errors.Is(err, assignment.ErrCommitPayloadDecompress):
			m.metrics.RecordPayloadDecompressError()
		case errors.Is(err, assignment.ErrCommitPayloadHashMismatch):
			m.metrics.RecordPayloadHashMismatch()
		case errors.Is(err, assignment.ErrCommitPayloadDecode):
			m.metrics.RecordPayloadDecodeError()
		case errors.Is(err, assignment.ErrCommitPayloadDigestMismatch):
			m.metrics.RecordSetDigestMismatch()
		}
		m.logger.Debug("commit payload verification failed", "error", err, "worker_id", workerID, "version", commit.Version)

		return Assignment{}, false
	}

	return Assignment{
		Version:             commit.Version,
		Lifecycle:           commit.Lifecycle,
		Partitions:          payload.Partitions,
		LeaderRevision:      commit.LeaderRevision,
		SourceRevision:      commit.SourceRevision,
		SourceRevisionKnown: commit.SourceRevisionKnown,
		TotalWorkers:        len(commit.Workers),
	}, true
}

func (m *Manager) decodeAssignmentEntry(entry jetstream.KeyValueEntry) (Assignment, bool) {
	var newAssignment Assignment
	if err := json.Unmarshal(entry.Value(), &newAssignment); err != nil {
		m.logError("failed to unmarshal assignment", "error", err)
		return Assignment{}, false
	}

	return newAssignment, true
}

// applyAssignment is the single apply-then-advance-LSR-then-store-then-ack
// pipeline (§4.4).
//
// Ordering invariant: Apply → LSR → Store → Ack → Hooks. On Apply failure,
// neither LSR nor Store nor Ack run, the manager enters degraded mode, and
// a bounded exponential-backoff retry is scheduled. The publisher's
// monotonicity guarantee (SetAppliedAssignment never regresses
// AppliedVersion) means a retry after a higher commit lands cannot ack a
// stale lower version.
//
// LSR invariant (single source of truth): on success this method advances
// lastSeenLeaderRevision = max(LSR, newAssignment.LeaderRevision) BEFORE
// storing the new snapshot. Commit, legacy-alias, initial-bootstrap, and
// scheduleApplyRetry callers therefore do NOT need to advance LSR
// themselves — the per-caller "advance LSR after successful apply" pattern
// is dangerous because the retry goroutine cannot easily reconstruct the
// failure-case bookkeeping. Centralizing LSR advancement here closes the
// v2-review P0 retry-success regression.
//
// LSR-before-Store ordering (v3 review P0): advancing LSR before
// m.assignment.Store eliminates the dangerous "new snapshot, old LSR"
// interleaving that a concurrent handleCommitValueOnce reader could
// otherwise observe — under which a stale higher-Version commit
// (commit.LR < newAssignment.LR) would bypass case (b)'s stale-leader
// fence after case (a)'s "Version <= cur.Version" no-op gate already
// advanced past it. The plan's invariant ("LSR is advanced only after a
// successful state-machine action") permits advancing LSR at this point
// because Apply has returned nil.
//
// Monotonicity gate: a retry that fires after a newer version has already
// applied (e.g. retry of V=10 after V=11 succeeded) is a no-op so the
// in-memory snapshot cannot regress. The carve-out for newAssignment.Version
// == 0 lets the initial-bootstrap path apply over a zero-value Assignment.
//
// Parameters:
//   - newAssignment: The assignment to apply.
//
// Returns:
//   - error: Non-nil only when Apply fails; Store/Ack failures are logged
//     but non-fatal (the next heartbeat tick picks up the snapshot).
func (m *Manager) applyAssignment(newAssignment Assignment) error {
	return m.applyAssignmentWithPrev(m.CurrentAssignment(), newAssignment)
}

// applyAssignmentWithPrev runs the apply-then-store-then-ack pipeline with an
// explicit previous-assignment argument. The default applyAssignment path
// reads m.CurrentAssignment() for prev; the initial-bootstrap path
// (applyInitialAssignment) passes an explicit Assignment{} so the handoff
// coordinator's prepare phase sees the full new partition set as "newly
// acquired" without touching the snapshot.
//
// See applyAssignment for the centralized LSR advancement contract.
func (m *Manager) applyAssignmentWithPrev(oldAssignment, newAssignment Assignment) error {
	workerID := m.WorkerID()
	curAssignment := m.CurrentAssignment()

	// Monotonicity gate: do not regress the snapshot when a stale retry
	// fires after a higher-version apply already succeeded. Compare against
	// the live in-memory snapshot, not the caller-supplied oldAssignment —
	// the initial-bootstrap path passes Assignment{} as oldAssignment to
	// force prepare phase to treat partitions as newly acquired, but the
	// snapshot is already at the to-be-applied version (waitForAssignment
	// stored it). Use strict less-than so the initial-bootstrap apply for
	// the SAME version proceeds (re-applying is idempotent for the
	// handoff coordinator).
	if newAssignment.Version != 0 && newAssignment.Version < curAssignment.Version {
		return nil
	}

	// 1) Apply via handoff coordinator. Must succeed before we touch the
	//    in-memory snapshot or publish the ack.
	if err := m.handoffCoordinator.Apply(m.ctx, workerID, oldAssignment, newAssignment); err != nil {
		m.logError("handoff apply failed", "error", err)
		m.scheduleApplyRetry(newAssignment)
		return err
	}

	// 2) Advance lastSeenLeaderRevision (stale-leader fence) — single
	//    source of truth for LSR. See applyAssignment Godoc. LSR MUST
	//    advance BEFORE the snapshot Store, otherwise a concurrent
	//    handleCommitValueOnce reader could observe (new snapshot, old
	//    LSR) — the v3-review P0 dangerous interleaving where a stale
	//    higher-Version commit (with commit.LR < newAssignment.LR)
	//    bypasses case (b)'s stale-leader fence on a snapshot that has
	//    already advanced past it via case (a)'s no-op gate. By
	//    advancing LSR first, the only interleavings a concurrent
	//    reader can observe are (old snap, old LSR), (old snap, new
	//    LSR), and (new snap, new LSR) — all safe. The fence may briefly
	//    reject commits with LR < newAssignment.LR against the old
	//    snapshot, but that is correct: LSR has actually advanced.
	m.updateLastSeenLeaderRevision(newAssignment.LeaderRevision)

	// 3) Store the now-applied assignment in the manager snapshot. After
	//    this point, (snapshot, LSR) pairs visible to concurrent readers
	//    are safely ordered.
	m.assignment.Store(newAssignment)
	if hook := m.testHookAfterApplyStore; hook != nil {
		hook(newAssignment)
	}

	m.logger.Info("assignment applied",
		"worker_id", workerID,
		"old_version", oldAssignment.Version,
		"new_version", newAssignment.Version,
		"old_partitions", len(oldAssignment.Partitions),
		"new_partitions", len(newAssignment.Partitions),
	)

	// 4) Ack via heartbeat publisher (Phase 2/4 receipt). Failures here are
	//    non-fatal — the next tick will publish a snapshot containing the
	//    same AppliedVersion (monotone).
	appliedDigest := types.PartitionSetDigest(newAssignment.Partitions)
	m.heartbeat.SetAppliedAssignment(heartbeat.AppliedAssignment{
		LeaderRevision:        newAssignment.LeaderRevision,
		AppliedVersion:        newAssignment.Version,
		AppliedDigest:         appliedDigest,
		AppliedSourceRevision: newAssignment.SourceRevision,
		AppliedSourceRevKnown: newAssignment.SourceRevisionKnown,
		AppliedAt:             time.Now(),
	})
	if err := m.heartbeat.PublishNow(m.ctx); err != nil {
		m.logError("heartbeat publish-now after apply failed", "error", err)
	}

	// 5) Metrics + hooks.
	m.recordAssignmentMetrics(oldAssignment, newAssignment)
	m.invokeAssignmentChangedHooks(workerID, oldAssignment, newAssignment)

	return nil
}

// invokeAssignmentChangedHooks dispatches OnAssignmentChanged and the
// convenience Assigned/Revoked hooks. Both run asynchronously off the
// invokeHook helper; the WaitGroup tracks them so Stop() waits for hook
// completion.
func (m *Manager) invokeAssignmentChangedHooks(_ /* workerID */ string, oldAssignment, newAssignment Assignment) {
	if m.hooks.OnAssignmentChanged != nil {
		m.invokeHook("assignment change", func() error {
			return m.hooks.OnAssignmentChanged(m.ctx, oldAssignment.Partitions, newAssignment.Partitions)
		})
	}

	if m.hooks.OnPartitionsAssigned != nil || m.hooks.OnPartitionsRevoked != nil {
		m.invokeHook("partition hooks", func() error {
			added, removed := diffPartitions(oldAssignment.Partitions, newAssignment.Partitions)

			if len(added) > 0 && m.hooks.OnPartitionsAssigned != nil {
				if err := m.hooks.OnPartitionsAssigned(m.ctx, added); err != nil {
					m.logError("partitions assigned hook error", "error", err)
				}
			}

			if len(removed) > 0 && m.hooks.OnPartitionsRevoked != nil {
				if err := m.hooks.OnPartitionsRevoked(m.ctx, removed); err != nil {
					m.logError("partitions revoked hook error", "error", err)
				}
			}

			return nil
		})
	}
}

// scheduleApplyRetry stages a failed assignment for a bounded
// exponential-backoff retry. Multiple failures coalesce to the
// highest-Version target via stashedApplyRetry. Only one retry goroutine
// is active at a time; subsequent calls update the stash and return.
//
// The retry initial backoff is 1s, doubling up to 30s with ±20% jitter.
// On retry success the goroutine self-terminates.
func (m *Manager) scheduleApplyRetry(newAssignment Assignment) {
	// Coalesce: keep the highest-Version pending.
	for {
		cur := m.stashedApplyRetry.Load()
		if cur != nil && cur.Version >= newAssignment.Version {
			break
		}
		candidate := newAssignment
		if m.stashedApplyRetry.CompareAndSwap(cur, &candidate) {
			break
		}
	}

	// Only one retry loop at a time.
	if !m.applyRetryActive.CompareAndSwap(false, true) {
		return
	}

	m.wg.Go(func() {
		defer m.applyRetryActive.Store(false)
		backoff := time.Second
		const maxBackoff = 30 * time.Second
		for {
			// Wait with jitter ±20%.
			//nolint:gosec // jitter does not require crypto-secure random
			jitter := time.Duration(float64(backoff) * 0.2 * (2*rand.Float64() - 1))
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(backoff + jitter):
			}

			pending := m.stashedApplyRetry.Swap(nil)
			if pending == nil {
				return
			}
			if err := m.applyAssignment(*pending); err != nil {
				// applyAssignment already re-stashed the failure; keep going.
				if backoff < maxBackoff {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
				}
				continue
			}
			// Success — drain any newer stash that arrived during the apply.
			if again := m.stashedApplyRetry.Load(); again == nil {
				return
			}
			backoff = time.Second // reset for the next attempt
		}
	})
}

func (m *Manager) recordAssignmentMetrics(oldAssignment, newAssignment Assignment) {
	added, removed := diffPartitions(oldAssignment.Partitions, newAssignment.Partitions)
	m.metrics.RecordAssignmentChange(len(added), len(removed), newAssignment.Version)
}

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

	m.assignment.Store(curAssignment)
	m.lastAssignmentAt.Store(time.Now().UnixNano())
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

// updateLastSeenLeaderRevision sets m.lastSeenLeaderRevision to
// max(current, rev) using a CAS loop so concurrent watchers cannot regress
// the value.
//
// Parameters:
//   - rev: Candidate revision; ignored if not greater than the current value.
func (m *Manager) updateLastSeenLeaderRevision(rev uint64) {
	for {
		cur := m.lastSeenLeaderRevision.Load()
		if rev <= cur {
			return
		}
		if m.lastSeenLeaderRevision.CompareAndSwap(cur, rev) {
			return
		}
	}
}

// diffPartitions calculates added and removed partitions between two sets.
func diffPartitions(oldPartitions, newPartitions []Partition) (added, removed []Partition) {
	oldMap := make(map[string]Partition, len(oldPartitions))
	for _, p := range oldPartitions {
		oldMap[p.ID()] = p
	}

	newMap := make(map[string]Partition, len(newPartitions))
	for _, p := range newPartitions {
		newMap[p.ID()] = p
	}

	for _, p := range newPartitions {
		if _, exists := oldMap[p.ID()]; !exists {
			added = append(added, p)
		}
	}

	for _, p := range oldPartitions {
		if _, exists := newMap[p.ID()]; !exists {
			removed = append(removed, p)
		}
	}

	return added, removed
}
