package parti

import (
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// runStartupBackground completes the parts of startup whose duration is
// not bounded by a single RPC: waiting for the leader-published initial
// assignment, and applying it via the unified pipeline. It is best-effort
// and single-attempt — on error it logs and falls through to monitor
// startup, letting the existing recovery mechanisms drive subsequent
// retries:
//
//   - Failed assignment fetch: monitorAssignmentChanges (started below)
//     delivers the assignment when the leader publishes it; handleAssignmentEntry
//     applies via the unified pipeline.
//   - Failed initial apply: applyAssignmentWithPrev's scheduleApplyRetry
//     (manager_assignment.go:944) schedules a retry on the same input.
//
// The runner does NOT call enterDegraded or exitDegraded. Soft probe
// signaling is handled by startStartupTimeoutWatchdog (separate goroutine
// decoupled from the runner so the watchdog fires even if the runner is
// blocked inside applyInitialAssignment).
//
// On a clean success the runner marks startup assignment readiness. If state
// is still WaitingAssignment, the readiness helper CAS-transitions
// WaitingAssignment → Stable. If the calculator-state monitor (started in
// startCalculator) has already moved state to Scaling/Rebalancing/Emergency,
// the helper only completes the deferred calculator Idle → Stable transition
// after the local assignment is applied and acked. The post-Stable monitor set
// always starts, regardless of apply success or state path; the monitors handle
// whatever state they find.
//
// Apply boundedness: applyInitialAssignment internally calls
// handoffCoordinator.Apply(m.ctx, ...) which is unbounded per attempt.
// This matches pre-refactor Start (same chain). A stuck updater can block
// the runner inside one apply attempt until m.ctx is cancelled. The
// watchdog still fires enterDegraded("startup-timeout") in that case so
// the readiness probe can rotate the pod.
func (m *Manager) runStartupBackground(assignmentKV jetstream.KeyValue) {
	defer func() {
		if r := recover(); r != nil {
			m.logError("startup background panicked", "panic", r)
			m.enterDegraded(DegradeReasonStartupBackgroundPanic)
			// Still attempt to start monitors so the manager can recover
			// once degraded is exited via attemptRecoveryFromDegraded.
			m.startPostStableMonitors(assignmentKV)
		}
	}()

	applyOK := false
	if err := m.waitForAssignment(m.ctx, assignmentKV, m.heartbeatKV); err != nil {
		m.logError("startup: initial assignment fetch failed; will recover via assignment watcher",
			"error", err)
	} else if err := m.applyInitialAssignment(m.ctx, assignmentKV); err != nil {
		m.logError("startup: initial apply failed; will recover via scheduleApplyRetry / commit watcher",
			"error", err)
	} else {
		applyOK = true
	}

	if applyOK {
		m.casToStableFromWaitingAssignment()
	}

	// Always start post-Stable monitors. If the runner failed to apply,
	// the monitors are the recovery path: monitorAssignmentChanges
	// redelivers; scheduleApplyRetry retries; monitorNATSConnection
	// drives attemptRecoveryFromDegraded if degraded fired.
	m.startPostStableMonitors(assignmentKV)
}

// casToStableFromWaitingAssignment performs a guarded WaitingAssignment →
// Stable transition. The calculator-state monitor (manager_assignment.go:157-168)
// can move state from WaitingAssignment to Scaling/Rebalancing/Emergency
// while this runner is mid-apply (see syncStateFromCalculator at
// manager_state.go:194-242). Generic transitionState(StateStable) would
// CAS-walk through those states — manager_state.go:165-168 lists StateStable
// as a valid next state from each. The direct CAS here only succeeds when
// state is still WaitingAssignment; otherwise calculator owns state.
//
// On a successful CAS it calls emitTransitionEffects — the same shared emitter
// transitionState uses for the OnStateChanged hook and RecordStateTransition
// metric — so observers see no difference from a normal transition.
//
// Idempotent: callable from any apply-success path (runner, watcher
// redelivery, scheduleApplyRetry). Second and later calls see state !=
// WaitingAssignment and no-op.
func (m *Manager) casToStableFromWaitingAssignment() {
	if !m.state.CompareAndSwap(int32(StateWaitingAssignment), int32(StateStable)) { //nolint:gosec // controlled enum
		return
	}
	m.emitTransitionEffects(StateWaitingAssignment, StateStable)
}

func isCalculatorOwnedActiveState(state State) bool {
	return state == StateScaling || state == StateRebalancing || state == StateEmergency
}

func (m *Manager) markStartupAssignmentApplied() {
	if !m.startupAssignmentApplied.CompareAndSwap(false, true) {
		return
	}

	m.casToStableFromWaitingAssignment()

	if !isCalculatorOwnedActiveState(m.State()) {
		return
	}
	if m.calculator == nil || m.calculator.GetState() != types.CalcStateIdle {
		return
	}

	m.transitionState(StateStable)
}

// startPostStableMonitors launches the four monitor goroutines that drive
// post-Stable lifecycle. Wrapped in postStableMonitorsOnce because the
// runner calls it whether or not the initial apply succeeded — and a future
// degraded→recovered cycle could re-enter and double-spawn without this
// guard. monitorNATSConnection itself is also independently idempotent via
// connMonitorOnce (manager_degraded.go:14-32).
func (m *Manager) startPostStableMonitors(assignmentKV jetstream.KeyValue) {
	m.postStableMonitorsOnce.Do(func() {
		m.wg.Go(func() { m.monitorCommitChanges(m.ctx, assignmentKV) })
		m.wg.Go(func() { m.monitorAssignmentChanges(m.ctx, assignmentKV) })
		m.monitorNATSConnection()
		m.wg.Go(func() { m.monitorBucketEpochs(m.ctx) })
	})
}

// startStartupTimeoutWatchdog spawns a goroutine that fires
// enterDegraded("startup-timeout") once if the manager is still in
// StateWaitingAssignment after StartupTimeout has elapsed. The deadline is
// absolute (computed from m.startedAt), so the synchronous sanity phase
// counts against the budget — preserving the documented contract that
// StartupTimeout covers full manager startup from Start invocation
// (config.go:406-410).
//
// Decoupled from runStartupBackground: the watchdog fires even if the
// runner is blocked inside applyInitialAssignment (which is unbounded —
// see runStartupBackground Godoc). enterDegraded is CAS-gated on
// degradedSince so concurrent degraded entries from other paths are
// harmless; OnDegraded fires exactly once per entry per the existing
// contract.
//
// startupWatchdogFired ensures the watchdog goroutine is scheduled at
// most once per Manager instance.
func (m *Manager) startStartupTimeoutWatchdog() {
	if m.cfg.StartupTimeout <= 0 {
		return
	}
	if !m.startupWatchdogFired.CompareAndSwap(false, true) {
		return
	}
	deadline := m.startedAt.Add(m.cfg.StartupTimeout)
	wait := time.Until(deadline)
	m.wg.Go(func() {
		if wait > 0 {
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		if m.State() != StateWaitingAssignment {
			return
		}
		m.logError("startup: exceeded StartupTimeout without reaching Stable; entering degraded for probe rotation",
			"startup_timeout", m.cfg.StartupTimeout,
			"elapsed", time.Since(m.startedAt),
		)
		m.enterDegraded(DegradeReasonStartupTimeout)
	})
}
