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

	// IncClaimStaleHandoffReset increments the counter for stuck-prepare claims
	// that preparePhase reset back to clean stable on re-acquire by the existing
	// owner. Emitted when an A->B->A revert race leaves an in-flight handoff
	// recorded on a claim that the new (same as old) owner is re-acquiring.
	IncClaimStaleHandoffReset()
}

// HandoffSweepMetricsRecorder is an optional capability a
// [HandoffMetricsRecorder] implementation may additionally satisfy to
// receive claim-sweep pass observability. The handoff coordinator
// type-asserts its configured recorder for this interface once at
// construction; a recorder without it loses nothing else.
//
// [NopHandoffMetricsRecorder] implements it, so recorders embedding the
// no-op satisfy the capability automatically (as no-ops).
type HandoffSweepMetricsRecorder interface {
	// IncClaimSweepPass counts one admitted claim-sweep pass. Non-admitted
	// attempts (single-flight lock misses, interval throttling) and the
	// shutdown-only confirm-wait abort emit nothing.
	//
	// All three label sets are closed and low-cardinality:
	//   - origin: "apply" (opportunistic, from an assignment Apply) or
	//     "ticker" (the periodic sweep loop).
	//   - outcome: "full" (ListKeys + per-key reads) or "cached" (the
	//     scan-gated pass over the cached claim view; ticker-only).
	//   - reason: why a full pass ran; "" for cached passes. One of:
	//     "ungated" (apply-origin, or the store has no position probe),
	//     "unlatched" (no valid cache to skip against), "mismatch" (the
	//     bucket position moved since the cache was latched), "forced"
	//     (max consecutive cached passes reached — the backstop),
	//     "probe_error", "unsafe_config", "no_probe_handle".
	//
	// Parameters:
	//   - origin: Who initiated the pass.
	//   - outcome: Whether the pass ran full or against the cached view.
	//   - reason: Full-pass cause, "" for cached passes.
	IncClaimSweepPass(origin, outcome, reason string)
}

// NopHandoffMetricsRecorder is a no-op implementation of [HandoffMetricsRecorder]
// (and of the optional [HandoffSweepMetricsRecorder] capability).
type NopHandoffMetricsRecorder struct{}

func (NopHandoffMetricsRecorder) IncHandoffTotal(string)                     {}
func (NopHandoffMetricsRecorder) ObserveHandoffDuration(time.Duration)       {}
func (NopHandoffMetricsRecorder) ObservePhaseDuration(string, time.Duration) {}
func (NopHandoffMetricsRecorder) IncCASConflicts()                           {}
func (NopHandoffMetricsRecorder) SetClaimStoreSize(int)                      {}
func (NopHandoffMetricsRecorder) IncClaimStoreStale()                        {}
func (NopHandoffMetricsRecorder) IncClaimStaleHandoffReset()                 {}
func (NopHandoffMetricsRecorder) IncClaimSweepPass(string, string, string)   {}
