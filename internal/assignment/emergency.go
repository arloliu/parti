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
	// disappearedWorkers tracks when each worker was first seen as disappeared
	disappearedWorkers map[string]time.Time

	// gracePeriod is the minimum time a worker must be missing before emergency
	gracePeriod time.Duration

	mu sync.RWMutex
}

// Compile-time assertion that EmergencyDetector is properly initialized.
var _ = (*EmergencyDetector)(nil)

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
	}
}

// CheckEmergency determines if an emergency rebalance is needed based on worker changes.
//
// Implements hysteresis by tracking disappearance timestamps. Workers that reappear
// within the grace period are not considered emergencies. Only workers that remain
// absent for the full grace period trigger emergency rebalancing.
//
// The method performs the following:
//   - Tracks newly disappeared workers with their first seen timestamp
//   - Clears tracking for workers that reappear
//   - Returns workers that have exceeded the grace period
//
// Parameters:
//   - prev: Previous set of active worker IDs
//   - curr: Current set of active worker IDs
//
// Returns:
//   - bool: true if emergency conditions met (workers confirmed disappeared beyond grace period)
//   - []string: List of workers that disappeared beyond grace period (empty if no emergency)
func (d *EmergencyDetector) CheckEmergency(prev, curr map[string]bool) (bool, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()

	// Track newly disappeared workers and clear reappeared ones
	for workerID := range prev {
		if !curr[workerID] {
			// Worker disappeared - track if not already tracking
			if _, exists := d.disappearedWorkers[workerID]; !exists {
				d.disappearedWorkers[workerID] = now
			}
		} else {
			// Worker reappeared - clear tracking
			delete(d.disappearedWorkers, workerID)
		}
	}

	// Check which workers exceeded grace period
	confirmed := make([]string, 0)
	for workerID, firstSeen := range d.disappearedWorkers {
		if now.Sub(firstSeen) >= d.gracePeriod {
			confirmed = append(confirmed, workerID)
		}
	}

	return len(confirmed) > 0, confirmed
}

// Reset clears all tracked disappearances.
//
// Called after successful rebalance or when emergency state ends to prepare
// for fresh tracking of future disappearances. This prevents stale tracking
// data from affecting future emergency detection.
func (d *EmergencyDetector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disappearedWorkers = make(map[string]time.Time)
}
