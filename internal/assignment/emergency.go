package assignment

import (
	"sync"
	"time"
)

// EmergencyDetector tracks worker disappearances with hysteresis to prevent
// false positives from transient network issues.
//
// Workers must remain disappeared for the grace period before triggering
// an emergency rebalance. This prevents flapping during brief connectivity loss.
type EmergencyDetector struct {
	// disappearedWorkers tracks when each worker was first seen as disappeared.
	disappearedWorkers map[string]time.Time

	// gracePeriod is the minimum time a worker must be missing before emergency.
	gracePeriod time.Duration

	// now returns the current time. Defaults to time.Now; tests inject a
	// deterministic clock by overriding this field.
	now func() time.Time

	mu sync.Mutex
}

// NewEmergencyDetector creates a new emergency detector with specified grace period.
//
// The grace period prevents false positives from transient network issues by requiring
// workers to remain disappeared for the full duration before triggering emergency
// rebalancing.
//
// Parameters:
//   - gracePeriod: Minimum time workers must be missing (recommended: 1.5 * HeartbeatInterval)
//
// Returns:
//   - *EmergencyDetector: Initialized detector ready for use
func NewEmergencyDetector(gracePeriod time.Duration) *EmergencyDetector {
	return &EmergencyDetector{
		disappearedWorkers: make(map[string]time.Time),
		gracePeriod:        gracePeriod,
		now:                time.Now,
	}
}

// ObserveAlive clears tracking for every worker observed alive in alive.
//
// Intended to be called by every code path that performs a fresh live-worker
// scan, not only CheckEmergency: rebalance(), audit-repair flows, partition-
// lifecycle rebalances, manual TriggerRebalance — all of them perform an
// independent KV scan and would otherwise update c.lastWorkers (eventually,
// via handleRebalance) without the detector observing the freshly-seen
// workers. This invariant must hold:
//
//	"firstSeen[A] is the moment since which A has been continuously absent
//	 from any leader observation of the live worker set."
//
// ObserveAlive is the side-channel that maintains it for non-poll observations.
//
// Parameters:
//   - alive: Worker IDs observed alive in the most recent fresh scan.
func (d *EmergencyDetector) ObserveAlive(alive []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, workerID := range alive {
		delete(d.disappearedWorkers, workerID)
	}
}

// CheckEmergency reconciles detector state with a fresh poll observation.
// Atomic under the detector mutex.
//
// Phases:
//  1. Clear by curr — any heartbeat-visible worker is alive.
//  2. Track newly missing workers in prev (firstSeen preserved if already tracked).
//  3. Safety valve — drop stranded (!prev) entries older than 10*gracePeriod
//     to bound map growth under pathological churn.
//  4. Confirm — entries in prev whose firstSeen exceeds gracePeriod.
//
// Parameters:
//   - prev: Previous set of active worker IDs.
//   - curr: Current set of active worker IDs.
//
// Returns:
//   - emergency: true if at least one worker's grace period has expired.
//   - confirmed: workers whose grace period has expired (empty if none).
//   - pending: true if at least one tracked entry is in prev (informational —
//     allows callers to suppress planned_scale while a disappearance is mid-grace).
func (d *EmergencyDetector) CheckEmergency(
	prev, curr map[string]bool,
) (emergency bool, confirmed []string, pending bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()

	// Phase 1: Clear by curr — any heartbeat-visible worker is alive.
	for workerID := range curr {
		delete(d.disappearedWorkers, workerID)
	}

	// Phase 2: Track newly missing workers in prev.
	for workerID := range prev {
		if curr[workerID] {
			continue
		}
		if _, exists := d.disappearedWorkers[workerID]; !exists {
			d.disappearedWorkers[workerID] = now
		}
	}

	// Phase 3: Safety valve — drop stranded entries older than 10*gracePeriod.
	expiry := 10 * d.gracePeriod
	for workerID, firstSeen := range d.disappearedWorkers {
		if !prev[workerID] && now.Sub(firstSeen) > expiry {
			delete(d.disappearedWorkers, workerID)
		}
	}

	// Phase 4: Confirm — entries in prev whose firstSeen exceeds gracePeriod.
	confirmed = make([]string, 0)
	for workerID, firstSeen := range d.disappearedWorkers {
		if !prev[workerID] {
			continue
		}
		pending = true
		if now.Sub(firstSeen) >= d.gracePeriod {
			confirmed = append(confirmed, workerID)
		}
	}

	return len(confirmed) > 0, confirmed, pending
}
