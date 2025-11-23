package handoff

import "time"

// MetricsRecorder captures handoff metrics. Optional; a NOP is provided.
// Keeping this local avoids coupling to the global metrics interface while
// enabling a focused set of measurements for handoff.
type MetricsRecorder interface {
	// IncHandoffTotal increments the handoff total counter with the given result label.
	IncHandoffTotal(result string)
	// ObserveHandoffDuration records the overall handoff duration.
	ObserveHandoffDuration(d time.Duration)
	// ObservePhaseDuration records the duration of a handoff phase.
	ObservePhaseDuration(phase string, d time.Duration)
	// IncCASConflicts increments the CAS conflict counter.
	IncCASConflicts()
	// SetClaimStoreSize sets the current claim store size gauge.
	SetClaimStoreSize(n int)
	// IncClaimStoreStale increments the stale claim counter.
	IncClaimStoreStale()
}

// NopMetrics is a no-op implementation.
type NopMetrics struct{}

func (NopMetrics) IncHandoffTotal(string)                     {}
func (NopMetrics) ObserveHandoffDuration(time.Duration)       {}
func (NopMetrics) ObservePhaseDuration(string, time.Duration) {}
func (NopMetrics) IncCASConflicts()                           {}
func (NopMetrics) SetClaimStoreSize(int)                      {}
func (NopMetrics) IncClaimStoreStale()                        {}
