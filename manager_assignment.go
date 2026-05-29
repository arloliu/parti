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
	"github.com/arloliu/parti/v2/internal/retry"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// Watcher restart backoff knobs. Declared as vars (not consts) so
// integration tests can drive the F2 envelope at a sub-second cadence
// without waiting the production worst-case of ~80s. Production code
// must NEVER mutate these.
var (
	watcherBaseBackoff = 2 * time.Second
	watcherMaxBackoff  = 30 * time.Second

	// watcherMaxAttempts caps the bounded-retry envelope on the
	// assignment watcher's restart loop in monitorAssignmentChanges.
	// With watcherBaseBackoff=2s and watcherMaxBackoff=30s, six
	// attempts span roughly one reconcile cycle. Past this point the
	// assignment bucket is treated as unrecoverable from this
	// connection and the manager enters degraded mode with the named
	// reason "assignment-watcher-exhausted".
	watcherMaxAttempts = 6
)

const (
	watcherJitter = 0.3 // ±30% jitter

	// commitKeyName mirrors the publisher's "_commit" sub-component constant.
	commitKeyName = "_commit"

	// assignmentWatcherDegradedReason is the operator-visible
	// degraded-mode reason emitted by the F2 envelope's permanent-
	// failure callback when the assignment watcher exhausts its
	// attempt budget. Distinct from the generic "KV error threshold
	// exceeded" reason from recordKVError so operators can
	// distinguish "assignment bucket is unrecoverable" from
	// "accumulated transient errors".
	assignmentWatcherDegradedReason = "assignment-watcher-exhausted"
)

// watcherReconcileInterval is the period between idempotent KV re-reads
// of the commit key and the legacy per-worker alias key. Recovers from
// missed watcher events (channel close gaps, NATS reconnects) without
// depending on the watcher's resync. Shared by monitorCommitChanges and
// monitorAssignmentChanges.
//
// Declared as a package-level var (not a const) so reconcile-timing
// tests can override it without depending on a 30s wall-clock timer.
// Production callers MUST NOT mutate this value.
var watcherReconcileInterval = 30 * time.Second

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

// calculatorStateReconcileInterval is the period between idempotent reads of
// calc.GetState() in monitorCalculatorState. Recovers from dropped subscriber
// events (state_subscriber.go trySend drops on a 4-slot full buffer).
//
// Reconcile guarantees eventual projection of the CURRENT calculator state.
// If a transient state (e.g. Rebalancing) is dropped AND completes between
// two reconcile ticks, the intermediate transition is NOT replayed — only
// the current state is projected.
//
// Declared as a package-level var (not a const) so reconcile-timing tests can
// override it. Production callers MUST NOT mutate this value.
var calculatorStateReconcileInterval = 1 * time.Second

// monitorCalculatorState monitors the calculator's internal state and syncs it to Manager state.
//
// This goroutine listens to the Calculator's state change channel and updates
// the Manager's state machine accordingly. A periodic reconcile arm re-reads
// calc.GetState() and re-drives syncStateFromCalculator when the current state
// differs from the last applied value, recovering from any subscriber drop.
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

	reconcileTicker := time.NewTicker(calculatorStateReconcileInterval)
	defer reconcileTicker.Stop()

	var (
		lastApplied    types.CalculatorState
		lastAppliedSet bool
	)

	apply := func(calcState types.CalculatorState) {
		if err := m.syncStateFromCalculator(calcState); err != nil {
			m.logError("failed to sync state from calculator",
				"calc_state", calcState,
				"error", err,
			)
			return
		}
		lastApplied = calcState
		lastAppliedSet = true
	}

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
			apply(calcState)

		case <-reconcileTicker.C:
			current := calc.GetState()
			if lastAppliedSet && current == lastApplied {
				continue
			}
			apply(current)
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

// monitorAssignmentChanges monitors for assignment changes with automatic
// retry. The reconcile ticker is created once and shared across every
// runAssignmentWatchSession invocation so a missed event during a backoff
// window is still recovered on the next tick.
//
// The F2 bounded-retry envelope governs ONLY the kv.Watch establish step:
// each successful establish ends the envelope's Run cleanly (the inner
// session runs outside the envelope) so the next channel close starts a
// fresh attempt budget. This preserves the "per failure episode, reset on
// success" contract — a watcher that re-establishes after each close does
// not accumulate attempts across the manager's lifetime.
//
// Both establish errors and session-end errors feed the degraded-mode
// circuit via recordKVError so the whole-bucket-loss contract still
// trips through the existing threshold path on recordable errors. The
// envelope's exhaustion is the orthogonal escalation for non-recordable
// errors and for sustained establish-failure rates: after the budget is
// exhausted on consecutive establish failures, the manager enters
// degraded mode with the named reason "assignment-watcher-exhausted" —
// the assignment bucket is a hard correctness dependency, so silent
// infinite retries are not acceptable. Context cancellation stops the
// loop cleanly.
func (m *Manager) monitorAssignmentChanges(ctx context.Context, kv jetstream.KeyValue) {
	reconcileTicker := time.NewTicker(watcherReconcileInterval)
	defer reconcileTicker.Stop()

	workerID := m.WorkerID()
	key := fmt.Sprintf("assignment.%s", workerID) // Match calculator's key format

	// One outer iteration == one failure episode. Each iteration spins
	// up a fresh envelope so the attempt budget resets when the prior
	// session terminated cleanly (channel close after some healthy
	// operation). This preserves the F2 contract "per failure episode,
	// reset on success" — see internal/retry/envelope.go for the
	// single-shot semantics of Run that this loop adapts.
	for {
		if ctx.Err() != nil {
			return
		}

		var watcher jetstream.KeyWatcher
		env := retry.New(retry.Config{
			Work: func(_ context.Context) error {
				w, err := kv.Watch(ctx, key)
				if err != nil {
					// Establish-time failures feed the existing
					// degraded circuit (recordKVError) so the
					// whole-bucket-loss contract still trips through
					// the threshold path on recordable errors. The
					// envelope's exhaustion is the orthogonal
					// escalation for non-recordable error classes
					// and for sustained establish failure rates that
					// outrun the threshold window.
					m.recordKVError(err)
					m.logError("assignment watcher establish failed, will retry", "error", err)

					return fmt.Errorf("failed to watch assignments: %w", err)
				}
				watcher = w

				return nil
			},
			OnPermanent: func(err error) {
				m.logError("assignment watcher restart attempt budget exhausted; entering degraded mode",
					"error", err,
					"max_attempts", watcherMaxAttempts,
					"reason", assignmentWatcherDegradedReason)
				m.enterDegraded(assignmentWatcherDegradedReason)
			},
			BaseBackoff: watcherBaseBackoff,
			MaxBackoff:  watcherMaxBackoff,
			MaxAttempts: watcherMaxAttempts,
			Jitter:      watcherJitter,
		})
		if runErr := env.Run(ctx); runErr != nil {
			// ctx.Err() (clean exit) or ErrExhausted (OnPermanent
			// already entered degraded). Either way we exit.
			return
		}

		// Watcher established. Run one session; the deferred Stop
		// fires on every return path of runAssignmentWatchSession.
		sessionErr := m.runAssignmentWatchSession(ctx, kv, watcher, reconcileTicker.C, workerID, key)
		if sessionErr == nil {
			// ctx-cancel inside the session: clean shutdown.
			return
		}
		// Session ended on channel close. Feed the threshold circuit
		// (preserves whole-bucket-loss → Degraded contract when the
		// loss manifests mid-session) and loop with a fresh envelope.
		m.recordKVError(sessionErr)
		m.logError("assignment watcher session ended, will reconnect", "error", sessionErr)
	}
}

// runAssignmentWatchSession drives one watch session against the
// already-established watcher. Returns nil for ctx-cancel (clean exit)
// and a non-nil error when the Updates() channel closes (the session
// must be restarted with a fresh watcher by the caller).
//
// The reconcileTickC arm idempotently re-reads the alias key so missed
// watcher events (channel close gaps, NATS reconnects, silent stalls) are
// eventually recovered. The arm is safe because this select loop is
// single-goroutine: the reconcile arm cannot run while the watcher arm is
// mid-apply (and vice versa), and once an apply completes the existing
// stale-leader/version fences in handleAssignmentEntry reject a re-read of
// the same alias. See PR-1 spec §4 (idempotency contract).
func (m *Manager) runAssignmentWatchSession(
	ctx context.Context,
	kv jetstream.KeyValue,
	watcher jetstream.KeyWatcher,
	reconcileTickC <-chan time.Time,
	workerID, key string,
) error {
	defer func() {
		if serr := watcher.Stop(); serr != nil && !natsutil.IsBenignWatcherStopErr(serr) {
			m.logError("failed to stop assignment watcher", "error", serr)
		}
	}()

	window := m.cfg.AssignmentWatcherDebounce
	debounce := window > 0

	var (
		pending jetstream.KeyValueEntry
		timer   *time.Timer
		timerC  <-chan time.Time
	)
	if debounce {
		timer = time.NewTimer(time.Hour) // any duration; we Stop before first use
		timer.Stop()
		timerC = timer.C
	}
	// flush dispatches the pending entry to the hook (tests) or production handler.
	flush := func() {
		if pending == nil {
			return
		}
		entry := pending
		pending = nil
		if hook := m.testHookHandleAssignment; hook != nil {
			hook(workerID, entry)
		} else {
			m.handleAssignmentEntry(workerID, entry)
		}
	}

	for {
		select {
		case <-ctx.Done():
			// Intentionally do NOT flush. Stop has cancelled m.ctx; running
			// apply work now would invoke handoffCoordinator.Apply with an
			// already-dead context, then scheduleApplyRetry would spawn a
			// wait-group goroutine while Stop is draining the wait group.
			// Dropping the pending entry is the correct shutdown behavior.
			m.logger.Debug("assignment monitor stopping (context cancelled)", "worker_id", workerID)
			return nil

		case entry, ok := <-watcher.Updates():
			if !ok {
				// Session-restart path. Flush the pending entry so a
				// re-election whose final delivery arrived just before
				// the channel closed is not lost — the caller restarts
				// the watcher and the version gate makes a repeat safe.
				//
				// EXCEPT if Stop has already cancelled m.ctx: a
				// connection-side close racing Stop cancellation can
				// surface here, and flushing would apply work during
				// shutdown (the round-2 P1 hazard reapplied to the
				// !ok branch).
				if ctx.Err() != nil {
					return nil
				}
				flush()
				return errors.New("assignment watcher channel closed")
			}
			if entry == nil {
				// Nil entry indicates end of initial values replay.
				// This is normal — continue watching for future updates.
				continue
			}
			if !debounce {
				m.handleAssignmentEntry(workerID, entry)
				continue
			}
			pending = entry
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(window)

		case <-timerC:
			flush()

		case <-reconcileTickC:
			// Idempotent re-read. Single-goroutine serialization with the
			// watcher arm above guarantees no concurrent application of
			// the same alias; the version fence at handleAssignmentEntry
			// rejects re-reads of an already-applied alias.
			current, gerr := kv.Get(ctx, key)
			if gerr != nil {
				if errors.Is(gerr, jetstream.ErrKeyNotFound) {
					// Alias deleted (or never existed). No-op — matches
					// handleAssignmentEntry's KeyValueDelete branch and
					// W11's "delete is not a reassignment primitive".
					continue
				}
				// Transient/connectivity error: silently continue,
				// matching the commit watcher's symmetric reconcile arm.
				// recordKVError is intentionally NOT called here: feeding
				// a 30s-ticker into the degraded-mode circuit would
				// amplify entry under transient KV stress.
				continue
			}
			m.handleAssignmentEntry(workerID, current)
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
// §4.3); a periodic reconcile every watcherReconcileInterval re-fetches
// the commit and routes idempotently through handleCommitValue so missed
// updates eventually converge.
func (m *Manager) monitorCommitChanges(ctx context.Context, kv jetstream.KeyValue) {
	backoff := watcherBaseBackoff
	reconcileTicker := time.NewTicker(watcherReconcileInterval)
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
	return m.runCommitWatchSession(ctx, kv, watcher, reconcileTickC, key)
}

// commitDebouncer holds the mutable state for one runCommitWatchSession
// invocation. Extracting stage/flush into methods keeps the main select loop
// below the cyclop complexity limit while preserving the load-bearing guard
// ordering documented on stage().
type commitDebouncer struct {
	debounce bool
	window   time.Duration
	timer    *time.Timer
	workerID string
	pending  *types.AssignmentCommit
	dispatch func(*types.AssignmentCommit)
}

func newCommitDebouncer(window time.Duration, workerID string, dispatch func(*types.AssignmentCommit)) *commitDebouncer {
	d := &commitDebouncer{
		debounce: window > 0,
		window:   window,
		workerID: workerID,
		dispatch: dispatch,
	}
	if d.debounce {
		d.timer = time.NewTimer(time.Hour)
		d.timer.Stop()
	}

	return d
}

func (d *commitDebouncer) timerC() <-chan time.Time {
	if d.timer == nil {
		return nil
	}

	return d.timer.C
}

// stage routes a freshly decoded commit through the debounce window.
//
// Correctness invariant (two-phase handoff): every commit that changes THIS
// worker's effective partition set must reach handleCommitValue as a discrete
// apply. Otherwise a skipped intermediate that moved a partition away from us
// (worker not in commit.Workers, or a payload-set change that drops a
// partition we own) would leave our local snapshot still containing the
// partition — and the next debounced apply's preparePhase, which only diffs
// next-vs-previous on the local snapshot, would elide the reclaim/release and
// let the claim store and local snapshot diverge
// (see internal/assignment/handoff/twophase.go:208-232, 334-386).
// The "flush before staging on payload-hash change" rule below enforces this:
// bursts of identical-assignment-for-us commits collapse to the highest
// version (the common case during leader churn), but any intermediate that
// reshuffles us is flushed before the new pending is staged.
func (d *commitDebouncer) stage(commit *types.AssignmentCommit) {
	if commit == nil {
		return
	}
	if !d.debounce {
		d.dispatch(commit)
		return
	}
	// Out-of-order guard FIRST: drop any commit whose version is strictly
	// lower than what is already staged. This must run BEFORE the
	// workerAssignmentChanged check — otherwise a stale lower-version commit
	// with a different worker hash would force-flush the newer pending and
	// then re-stage itself, regressing both the version-order guarantee and
	// the "highest pending" architecture promise. The watcher stream is
	// monotone in practice, but the reconcile arm can surface a snapshot that
	// races with a more recent watcher event still in flight, so the guard is
	// load-bearing.
	if d.pending != nil && commit.Version < d.pending.Version {
		return
	}
	// If the new commit changes THIS worker's assignment vs. pending, flush
	// pending first so the handoff diff for the intermediate change is
	// observed. workerAssignmentChanged compares Workers-membership and
	// Payloads[workerID].PayloadHash — identical hashes mean identical
	// partition slices by AssignmentPayloadRef's content-addressable contract
	// (types/assignment_commit.go:51-66).
	if d.pending != nil && workerAssignmentChanged(d.pending, commit, d.workerID) {
		flushed := d.pending
		d.pending = nil
		d.dispatch(flushed)
	}
	// pending is now nil OR commit.Version >= pending.Version. The replacement
	// is unconditional in the nil case; the `>= pending.Version` arm in the
	// non-nil case simply refreshes the timer with the freshest view
	// (typically identical hash; we already flushed otherwise).
	commitCopy := *commit
	d.pending = &commitCopy
	if !d.timer.Stop() {
		select {
		case <-d.timer.C:
		default:
		}
	}
	d.timer.Reset(d.window)
}

func (d *commitDebouncer) flush() {
	if d.pending == nil {
		return
	}
	commit := d.pending
	d.pending = nil
	d.dispatch(commit)
}

// runCommitWatchSession drives one watch session against the already-established
// commit watcher. Returns nil for ctx-cancel (clean exit) and a non-nil error
// when the Updates() channel closes (the session must be restarted by the
// caller via monitorCommitChanges's backoff loop).
//
// When Config.AssignmentWatcherDebounce > 0, rapid bursts of commit events are
// coalesced: only the highest-version commit within each idle window reaches
// handleCommitValue. When an intermediate commit changes this worker's
// effective partition set, the pending commit is force-flushed before staging
// the new one so the two-phase handoff diff sees the ownership change.
// When debounce == 0 (default), every commit is dispatched immediately,
// preserving the current behavior.
//
//nolint:unparam // key is always "assignment._commit" today but the param documents the reconcile contract and allows future test overrides
func (m *Manager) runCommitWatchSession(
	ctx context.Context,
	kv jetstream.KeyValue,
	watcher jetstream.KeyWatcher,
	reconcileTickC <-chan time.Time,
	key string,
) error {
	defer func() {
		if serr := watcher.Stop(); serr != nil && !natsutil.IsBenignWatcherStopErr(serr) {
			m.logError("failed to stop commit watcher", "error", serr)
		}
	}()

	// dispatch is the single seam through which staged commits exit the
	// debounce window. It routes to handleCommitValue in production and to
	// testHookHandleCommitValue when set, mirroring the alias-side
	// testHookHandleAssignment pattern. Read the hook field once per dispatch
	// so tests that mutate it after spawn (forbidden by the hook's contract)
	// cannot create a torn read.
	dispatch := func(commit *types.AssignmentCommit) {
		if hook := m.testHookHandleCommitValue; hook != nil {
			hook(commit)
			return
		}
		m.handleCommitValue(commit)
	}

	db := newCommitDebouncer(m.cfg.AssignmentWatcherDebounce, m.WorkerID(), dispatch)

	for {
		select {
		case <-ctx.Done():
			return nil
		case entry, ok := <-watcher.Updates():
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				db.flush()

				return errors.New("commit watcher channel closed")
			}
			if entry == nil {
				continue
			}
			commit, ok := m.decodeCommitEntry(entry)
			if ok {
				db.stage(commit)
			}
		case <-db.timerC():
			db.flush()
		case <-reconcileTickC:
			if kv == nil {
				continue
			}
			current, _, gerr := kvutil.GetJSON[types.AssignmentCommit](ctx, kv, key)
			if gerr != nil || current == nil {
				continue
			}
			db.stage(current)
		}
	}
}

// handleCommitEntry decodes a watcher entry and routes through
// handleCommitValue. Deletion events are ignored; a deleted commit is
// treated as "no commit", which the dual-read selector handles correctly
// (the legacy alias may still drive).
func (m *Manager) handleCommitEntry(entry jetstream.KeyValueEntry) {
	commit, ok := m.decodeCommitEntry(entry)
	if !ok {
		return
	}
	m.handleCommitValue(commit)
}

// decodeCommitEntry decodes a KV entry into an AssignmentCommit. Deletion
// events return (nil, false). JSON decode failures are logged and return
// (nil, false). On success returns the decoded commit and true.
func (m *Manager) decodeCommitEntry(entry jetstream.KeyValueEntry) (*types.AssignmentCommit, bool) {
	if entry.Operation() == jetstream.KeyValueDelete {
		return nil, false
	}
	var commit types.AssignmentCommit
	if err := json.Unmarshal(entry.Value(), &commit); err != nil {
		m.logError("failed to unmarshal commit", "error", err)

		return nil, false
	}

	return &commit, true
}

// workerAssignmentChanged reports whether `next` assigns a different
// partition slice to `workerID` than `prev`. Used by the commit watcher
// debounce path to force-flush pending before staging a new commit that
// would change this worker's local snapshot.
//
// "Different" means EITHER:
//   - Workers-membership flips (this worker present in one, absent in
//     the other) — case (d) revoke or case (c) acquire.
//   - Both have this worker but Payloads[workerID].PayloadHash differs
//     — case (c) acquire of a different partition set.
//
// PayloadHash is the authoritative content identity for
// AssignmentPayloadRef (types/assignment_commit.go:55-57): identical
// hashes mean identical canonical partition bytes. Equality on hash is
// therefore sound for "no partition-set change for this worker".
func workerAssignmentChanged(prev, next *types.AssignmentCommit, workerID string) bool {
	prevHas := commitContainsWorker(prev, workerID)
	nextHas := commitContainsWorker(next, workerID)
	if prevHas != nextHas {
		return true
	}
	if !prevHas {
		// worker absent in both — no diff for us
		return false
	}

	return prev.Payloads[workerID].PayloadHash != next.Payloads[workerID].PayloadHash
}

// commitContainsWorker reports whether the commit's Workers slice contains
// workerID. Returns false for a nil commit.
func commitContainsWorker(c *types.AssignmentCommit, workerID string) bool {
	if c == nil {
		return false
	}
	return slices.Contains(c.Workers, workerID)
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

// isApplyResultStale returns true iff the candidate is older than the live
// snapshot in (Version, LeaderRevision) lex order. Used at the head of the
// applyStoreMu critical section to drop loser candidates BEFORE any
// coordinator-side side effect (W15+W16, PR-2).
//
// V=0 semantics:
//   - candidate.V=0 over cur.V=0: not stale. Permits the cold-bootstrap
//     path's idempotent re-apply over the just-initialized Assignment{}.
//   - candidate.V=0 over cur.V>0: STALE. A V=0 retry or legacy candidate
//     would otherwise regress a real snapshot.
//
// Same V same LR: not stale (idempotent reapply).
// Same V lower LR: STALE (the W15 cross-leader case).
// Lower V: STALE (the W16 stale-retry case).
func isApplyResultStale(candidate, cur Assignment) bool {
	if candidate.Version == 0 {
		return cur.Version != 0
	}
	if candidate.Version != cur.Version {
		return candidate.Version < cur.Version
	}
	return candidate.LeaderRevision < cur.LeaderRevision
}

// applyAssignmentWithPrev is the fresh-version apply entry (watcher,
// commit, alias, initial-bootstrap). It jitters once before calling core,
// so a fleet of workers observing the same fresh version spreads its
// JetStream consumer create/destroy load.
//
// Retries (scheduleApplyRetry) must NOT jitter — see
// applyAssignmentWithPrevSkipJitter below. Retries already pay their own
// exponential backoff; adding jitter on top compounds latency for no
// fleet-spread benefit (the retry is one worker, not a fleet).
func (m *Manager) applyAssignmentWithPrev(oldAssignment, newAssignment Assignment) error {
	if jitter := m.cfg.ApplyStartJitter; jitter > 0 {
		if hook := m.testHookApplyJittered; hook != nil {
			hook()
		}
		d := m.sampleApplyJitter(jitter)
		select {
		case <-time.After(d):
		case <-m.ctx.Done():
			return m.ctx.Err()
		}
	}

	return m.applyAssignmentWithPrevCore(oldAssignment, newAssignment)
}

// applyAssignmentWithPrevSkipJitter is the retry entry. It calls core
// directly, skipping the apply-start jitter sleep. Used by
// scheduleApplyRetry so a retry's compound delay is bounded by the
// retry-backoff envelope alone.
func (m *Manager) applyAssignmentWithPrevSkipJitter(oldAssignment, newAssignment Assignment) error {
	return m.applyAssignmentWithPrevCore(oldAssignment, newAssignment)
}

// sampleApplyJitter returns the jitter sleep duration for one apply
// invocation. Defaults to a uniform sample in [0, maxJitter) via
// math/rand/v2's top-level (concurrency-safe) Int64N. Tests override
// applyJitterSampler to force specific durations.
func (m *Manager) sampleApplyJitter(maxJitter time.Duration) time.Duration {
	if s := m.applyJitterSampler; s != nil {
		return s(maxJitter)
	}
	//nolint:gosec // jitter does not require crypto-secure random
	return time.Duration(rand.Int64N(int64(maxJitter)))
}

// applyAssignmentWithPrevCore runs the apply-then-store-then-ack pipeline
// with an explicit previous-assignment argument. The default applyAssignment
// path reads m.CurrentAssignment() for prev; the initial-bootstrap path
// (applyInitialAssignment) passes an explicit Assignment{} so the handoff
// coordinator's prepare phase sees the full new partition set as "newly
// acquired" without touching the snapshot.
//
// Holds applyStoreMu across (stale-check, Apply, LSR advance, Store,
// heartbeat SetAppliedAssignment) so concurrent apply paths (commit, alias,
// retry, refresh) cannot interleave their Stores or heartbeat acks. See
// applyAssignment Godoc + applyStoreMu Godoc.
func (m *Manager) applyAssignmentWithPrevCore(oldAssignment, newAssignment Assignment) error {
	workerID := m.WorkerID()

	m.applyStoreMu.Lock()
	// Before the stale gate: retries and fresh applies both call core, so
	// recording here counts all pipeline entries. Recording after the gate
	// would miss multi-version bursts (isApplyResultStale returns false for
	// distinct versions, so each would pass the gate anyway).
	m.metrics.RecordApplyAttempt(workerID, newAssignment.Version)

	// Stale gate (W15+W16, PR-2). Drops the candidate BEFORE Apply, so no
	// coordinator-side prepare/commit/stabilize runs for a loser. Strict
	// (V, LR) lex comparison via isApplyResultStale.
	curAssignment := m.CurrentAssignment()
	if isApplyResultStale(newAssignment, curAssignment) {
		m.applyStoreMu.Unlock()
		m.metrics.RecordStaleSnapshotStoreDropped()
		m.logger.Info("apply dropped by (V, LR) stale gate",
			"worker_id", workerID,
			"candidate_version", newAssignment.Version,
			"candidate_leader_revision", newAssignment.LeaderRevision,
			"current_version", curAssignment.Version,
			"current_leader_revision", curAssignment.LeaderRevision,
		)

		return nil
	}

	// 1) Apply via handoff coordinator. Must succeed before we touch the
	//    in-memory snapshot or publish the ack.
	applyErr := m.handoffCoordinator.Apply(m.ctx, workerID, oldAssignment, newAssignment)

	// Sample consumer-updater runtime capabilities unconditionally: the
	// updater may have wired a capability (e.g. CapProcessingGate via
	// handler wrap) even if a later phase of Apply failed. Runs BEFORE
	// the error return and BEFORE the success-path SetAppliedAssignment +
	// PublishNow, so the immediate post-apply heartbeat carries any
	// newly-wired bit on the first successful apply.
	m.reportConsumerCapabilities()

	if applyErr != nil {
		m.applyStoreMu.Unlock()
		m.logError("handoff apply failed", "error", applyErr)
		m.scheduleApplyRetry(newAssignment)
		return applyErr
	}

	// 2) Advance lastSeenLeaderRevision (stale-leader fence) — single
	//    source of truth for LSR. LSR MUST advance BEFORE the snapshot
	//    Store, otherwise a concurrent handleCommitValueOnce reader could
	//    observe (new snapshot, old LSR) — see commit at applyAssignment
	//    Godoc.
	m.updateLastSeenLeaderRevision(newAssignment.LeaderRevision)

	// 3) Store the now-applied assignment in the manager snapshot.
	m.assignment.Store(newAssignment)
	if hook := m.testHookAfterApplyStore; hook != nil {
		hook(newAssignment)
	}

	// 4) Update heartbeat in-memory snapshot INSIDE the lock (W15+W16
	//    plan-review v2 P0-B fix). The heartbeat publisher's monotonicity
	//    is V-only (internal/heartbeat/publisher.go:170-195); equal-V Acks
	//    overwrite LeaderRevision unconditionally. If we released the lock
	//    before SetAppliedAssignment, a slow loser path could post its
	//    same-V/lower-LR Ack after a winner has stored, regressing the
	//    heartbeat. PublishNow stays outside the lock (network IO).
	appliedDigest := types.PartitionSetDigest(newAssignment.Partitions)
	m.heartbeat.SetAppliedAssignment(heartbeat.AppliedAssignment{
		LeaderRevision:        newAssignment.LeaderRevision,
		AppliedVersion:        newAssignment.Version,
		AppliedDigest:         appliedDigest,
		AppliedSourceRevision: newAssignment.SourceRevision,
		AppliedSourceRevKnown: newAssignment.SourceRevisionKnown,
		AppliedAt:             time.Now(),
	})

	m.applyStoreMu.Unlock()

	m.logger.Info("assignment applied",
		"worker_id", workerID,
		"old_version", oldAssignment.Version,
		"new_version", newAssignment.Version,
		"old_partitions", len(oldAssignment.Partitions),
		"new_partitions", len(newAssignment.Partitions),
	)

	// 5) Publish heartbeat (network IO, OUTSIDE lock). Failures here are
	//    non-fatal — the next tick will publish a snapshot containing the
	//    same AppliedVersion (monotone).
	if err := m.heartbeat.PublishNow(m.ctx); err != nil {
		m.logError("heartbeat publish-now after apply failed", "error", err)
	}

	// 6) Metrics + hooks.
	m.recordAssignmentMetrics(oldAssignment, newAssignment)
	m.invokeAssignmentChangedHooks(workerID, oldAssignment, newAssignment)

	// 7) Complete startup if we are still in WaitingAssignment. Idempotent CAS
	// (guarded to only fire from WaitingAssignment) so calling it from
	// every apply-success path (initial, watcher-delivered,
	// scheduleApplyRetry-driven) is safe. Without this, a worker whose
	// runner's first apply failed but whose retry-or-watcher-driven apply
	// later succeeded would stay in WaitingAssignment forever even though
	// the assignment was applied and acked. See manager_startup_async.go
	// runStartupBackground Godoc.
	m.casToStableFromWaitingAssignment()

	return nil
}

// monotonicStore performs the (gate-check, LSR-advance, Store) sequence
// under applyStoreMu. Returns true if the Store landed; false if dropped
// as stale.
//
// REFRESH-PATH ONLY. Used by refreshAssignmentFromNATS to bring an
// authoritative KV snapshot under the same (V, LR) gate as the apply
// pipeline. NOT usable from applyAssignmentWithPrev because that function
// must hold applyStoreMu ACROSS handoffCoordinator.Apply; inlining the
// same primitive in both call sites is intentional.
//
// Callers MUST NOT hold applyStoreMu — the helper acquires and releases
// it itself.
func (m *Manager) monotonicStore(newAssignment Assignment) bool {
	m.applyStoreMu.Lock()
	defer m.applyStoreMu.Unlock()

	cur := m.CurrentAssignment()
	if isApplyResultStale(newAssignment, cur) {
		m.metrics.RecordStaleSnapshotStoreDropped()
		return false
	}

	m.updateLastSeenLeaderRevision(newAssignment.LeaderRevision)
	m.assignment.Store(newAssignment)

	return true
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
			prev := m.CurrentAssignment()
			if err := m.applyAssignmentWithPrevSkipJitter(prev, *pending); err != nil {
				// applyAssignmentWithPrevSkipJitter already re-stashed via core's
				// failure path; keep going.
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

	// Route the Store through monotonicStore so a refresh racing the apply
	// pipeline cannot regress a fresher snapshot (W15+W16, PR-2 §3.4). The
	// helper handles the (V, LR) gate, LSR advance, and Store under
	// applyStoreMu.
	if !m.monotonicStore(curAssignment) {
		m.logger.Debug("refresh skipped: snapshot already at-or-newer than KV",
			"version", curAssignment.Version,
			"leader_revision", curAssignment.LeaderRevision,
		)
		return nil
	}

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
