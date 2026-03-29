package types

import "time"

// HandoffMetricsRecorder captures two-phase handoff metrics.
//
// Implementations should be non-blocking, thread-safe, and handle failures gracefully.
// A no-op implementation ([NopHandoffMetricsRecorder]) is provided for production use
// when handoff metrics are not needed.
//
// This interface is intentionally separate from [MetricsCollector] because handoff
// metrics are only relevant when EnableTwoPhaseHandoff is true and represent a
// focused, opt-in set of measurements.
type HandoffMetricsRecorder interface {
	// IncHandoffTotal increments the handoff total counter with the given result label.
	//
	// Parameters:
	//   - result: Outcome label (e.g., "success", "failure")
	IncHandoffTotal(result string)

	// ObserveHandoffDuration records the overall handoff duration.
	//
	// Parameters:
	//   - d: Duration of the entire handoff operation
	ObserveHandoffDuration(d time.Duration)

	// ObservePhaseDuration records the duration of a handoff phase.
	//
	// Parameters:
	//   - phase: Phase name (e.g., "prepare", "commit", "stable")
	//   - d: Duration of the phase
	ObservePhaseDuration(phase string, d time.Duration)

	// IncCASConflicts increments the CAS conflict counter.
	IncCASConflicts()

	// SetClaimStoreSize sets the current claim store size gauge.
	//
	// Parameters:
	//   - n: Current number of claim records in the store
	SetClaimStoreSize(n int)

	// IncClaimStoreStale increments the stale claim counter.
	IncClaimStoreStale()
}

// NopHandoffMetricsRecorder is a no-op implementation of [HandoffMetricsRecorder].
type NopHandoffMetricsRecorder struct{}

func (NopHandoffMetricsRecorder) IncHandoffTotal(string)                     {}
func (NopHandoffMetricsRecorder) ObserveHandoffDuration(time.Duration)       {}
func (NopHandoffMetricsRecorder) ObservePhaseDuration(string, time.Duration) {}
func (NopHandoffMetricsRecorder) IncCASConflicts()                           {}
func (NopHandoffMetricsRecorder) SetClaimStoreSize(int)                      {}
func (NopHandoffMetricsRecorder) IncClaimStoreStale()                        {}
