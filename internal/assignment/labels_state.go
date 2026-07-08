package assignment

import (
	"errors"
	"sync"
	"time"
)

// errLabelObservationDeferred is the benign-abort sentinel for the first
// observation of a disruptive label condition (previously non-empty pool
// reads empty; worker labels unreadable). Deliberately distinct from the
// suspicious-observation sentinels: those are swallowed without
// label-aware re-arm; this one arms the label re-check timer.
var errLabelObservationDeferred = errors.New("label observation deferred pending confirmation")

// errLabelReadBroadFailure aborts a rebalance whose heartbeat label reads
// failed broadly (bucket/connectivity-class trouble or more than
// max(1, 10%) of workers unreadable). Never converted into label
// decisions: a broad failure must not empty-assign the fleet.
var errLabelReadBroadFailure = errors.New("worker label read failed broadly")

// labelState tracks per-label grace clocks and defer-once confirmation
// streaks (spec §8.5). All methods are called from the rebalance path
// (serialized by rebalanceMu) but the mutex keeps the timer path (Task 9)
// safe when it inspects remaining grace.
type labelState struct {
	mu    sync.Mutex
	grace time.Duration
	now   func() time.Time

	emptySince    map[string]time.Time // label → first empty observation
	emptyStreak   map[string]int       // label → consecutive empty observations
	unknownStreak map[string]int       // workerID → consecutive unreadable-label observations
}

func newLabelState(grace time.Duration, now func() time.Time) *labelState {
	if now == nil {
		now = time.Now
	}
	return &labelState{
		grace:         grace,
		now:           now,
		emptySince:    map[string]time.Time{},
		emptyStreak:   map[string]int{},
		unknownStreak: map[string]int{},
	}
}

// observeEmptyPools records this rebalance's empty-pool set and returns
// the action per confirmed-empty label. deferred=true means at least one
// label is on its FIRST empty observation — the caller aborts the
// rebalance with errLabelObservationDeferred and arms the re-check timer.
// emptySince always starts at the first observation so the deferral does
// not extend the effective grace window.
func (s *labelState) observeEmptyPools(empty []string) (map[string]emptyPoolAction, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	deferred := false
	actions := make(map[string]emptyPoolAction, len(empty))
	for _, l := range empty {
		if _, ok := s.emptySince[l]; !ok {
			s.emptySince[l] = now
		}
		s.emptyStreak[l]++
		if s.emptyStreak[l] < 2 {
			deferred = true
			continue
		}
		if now.Sub(s.emptySince[l]) < s.grace {
			actions[l] = emptyPoolPark
		} else {
			actions[l] = emptyPoolSpill
		}
	}

	return actions, deferred
}

// observeNonEmpty resets streak and grace clock for recovered pools.
func (s *labelState) observeNonEmpty(labels []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range labels {
		delete(s.emptySince, l)
		delete(s.emptyStreak, l)
	}
}

// observeUnknownWorkers implements defer-once for unreadable labels.
// Returns true when at least one worker is on its first unreadable
// observation (caller defers). Passing the empty set resets everything
// (a fully successful read).
func (s *labelState) observeUnknownWorkers(unknown []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(unknown) == 0 {
		clear(s.unknownStreak)
		return false
	}
	current := make(map[string]bool, len(unknown))
	deferred := false
	for _, w := range unknown {
		current[w] = true
		s.unknownStreak[w]++
		if s.unknownStreak[w] < 2 {
			deferred = true
		}
	}
	// Workers that recovered reset their streak.
	for w := range s.unknownStreak {
		if !current[w] {
			delete(s.unknownStreak, w)
		}
	}

	return deferred
}

// prune drops state for labels absent from the current snapshot.
func (s *labelState) prune(currentLabels map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for l := range s.emptySince {
		if !currentLabels[l] {
			delete(s.emptySince, l)
			delete(s.emptyStreak, l)
		}
	}
}

// minRemainingGrace returns the shortest time until an emptySince clock
// crosses grace, and whether any clock is running. The re-check timer
// (armLabelRecheckAfterRebalance) arms with this value after a rebalance that
// parked anything.
func (s *labelState) minRemainingGrace() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	found := false
	var minLeft time.Duration
	now := s.now()
	for _, since := range s.emptySince {
		left := s.grace - now.Sub(since)
		if left < 0 {
			left = 0
		}
		if !found || left < minLeft {
			minLeft, found = left, true
		}
	}

	return minLeft, found
}

// requestLabelRecheck records that a label re-check is owed and, for the
// fast-confirmation reasons, wakes monitorLabelRecheck immediately. The sticky
// pendingLabelRecheck flag guarantees the re-check survives a busy state
// machine; the capacity-1 channel coalesces concurrent requests into one wake.
//
// Per-reason send behavior (liveness): reasonNonFreshObservation is fired from
// INSIDE a rebalance whose worker observation degraded to the cached list. If
// the observation is a persistent connectivity outage, waking the monitor
// synchronously would re-run the rebalance, re-observe non-fresh, re-fire, and
// spin at full speed for the outage's duration. For that reason we set the
// sticky flag only and let the drain tick (or any other wake) service it. Every
// other reason — deferral confirmation and grace expiry — keeps the immediate
// send so it re-fires well under the drain cadence (spec §16 item 7).
func (c *Calculator) requestLabelRecheck(reason string) {
	c.pendingLabelRecheck.Store(true)
	c.Logger.Debug("label recheck requested", "reason", reason)

	if reason == reasonNonFreshObservation {
		return
	}

	select {
	case c.labelRecheckCh <- struct{}{}:
	default:
	}
}

// armLabelRecheckAfterRebalance arms (or cancels) the grace-expiry re-check
// timer after a rebalance. When something was parked, it schedules a re-check
// just past the shortest remaining grace window so the parked pool spills
// without any external event; a fresh rebalance supersedes any previously-armed
// timer. When nothing is parked and no re-check is already pending, it cancels
// a stale timer. Guarded by labelRecheckTimerMu so Stop and re-arm race safely.
func (c *Calculator) armLabelRecheckAfterRebalance(parkedCount int) {
	c.labelRecheckTimerMu.Lock()
	defer c.labelRecheckTimerMu.Unlock()

	if parkedCount <= 0 {
		// Nothing parked: no grace clock to wait on. Cancel a stale timer
		// unless a re-check is already pending — the monitor's drain tick will
		// service that one.
		if c.labelRecheckTimer != nil && !c.pendingLabelRecheck.Load() {
			c.labelRecheckTimer.Stop()
			c.labelRecheckTimer = nil
		}

		return
	}

	// Something parked: (re)arm from the shortest remaining grace window. A
	// fresh rebalance supersedes any previously-armed timer.
	if c.labelRecheckTimer != nil {
		c.labelRecheckTimer.Stop()
		c.labelRecheckTimer = nil
	}

	left, running := c.labelState.minRemainingGrace()
	if !running {
		return
	}

	c.labelRecheckTimer = time.AfterFunc(left+50*time.Millisecond, func() {
		c.requestLabelRecheck("grace_expiry")
	})
}
