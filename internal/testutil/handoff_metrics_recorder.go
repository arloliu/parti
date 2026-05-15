package testutil

import (
	"sync"
	"time"
)

// HandoffMetricsRecorder is a test implementation of the internal handoff.MetricsRecorder
// interface that captures emitted metrics for assertions in integration tests.
//
// All methods are thread-safe and non-blocking. Zero values indicate the metric
// was never recorded. Durations are stored as raw time.Duration values for
// precise comparisons without float conversion loss.
type HandoffMetricsRecorder struct {
	mu                 sync.RWMutex
	totalSuccess       int
	totalFailure       int
	handoffDurations   []time.Duration
	phaseDurations     map[string][]time.Duration
	casConflicts       int
	claimStoreSizes    []int
	staleClaims        int
	staleHandoffResets int
}

// NewHandoffMetricsRecorder constructs a new recorder instance.
func NewHandoffMetricsRecorder() *HandoffMetricsRecorder {
	return &HandoffMetricsRecorder{phaseDurations: make(map[string][]time.Duration)}
}

// IncHandoffTotal increments counters keyed by result label ("success" or "failure").
func (r *HandoffMetricsRecorder) IncHandoffTotal(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch result {
	case "success":
		r.totalSuccess++
	case "failure":
		r.totalFailure++
	}
}

// ObserveHandoffDuration records the overall duration of a handoff attempt.
func (r *HandoffMetricsRecorder) ObserveHandoffDuration(d time.Duration) {
	r.mu.Lock()
	r.handoffDurations = append(r.handoffDurations, d)
	r.mu.Unlock()
}

// ObservePhaseDuration records the duration of a named phase.
func (r *HandoffMetricsRecorder) ObservePhaseDuration(phase string, d time.Duration) {
	r.mu.Lock()
	r.phaseDurations[phase] = append(r.phaseDurations[phase], d)
	r.mu.Unlock()
}

// IncCASConflicts increments the CAS conflict counter.
func (r *HandoffMetricsRecorder) IncCASConflicts() {
	r.mu.Lock()
	r.casConflicts++
	r.mu.Unlock()
}

// SetClaimStoreSize appends the observed claim store size (acts like a gauge timeline).
func (r *HandoffMetricsRecorder) SetClaimStoreSize(n int) {
	r.mu.Lock()
	r.claimStoreSizes = append(r.claimStoreSizes, n)
	r.mu.Unlock()
}

// IncClaimStoreStale increments the stale claim counter.
func (r *HandoffMetricsRecorder) IncClaimStoreStale() {
	r.mu.Lock()
	r.staleClaims++
	r.mu.Unlock()
}

// IncClaimStaleHandoffReset increments the counter for stuck-prepare claims
// that preparePhase reset to clean stable on re-acquire by the existing owner.
func (r *HandoffMetricsRecorder) IncClaimStaleHandoffReset() {
	r.mu.Lock()
	r.staleHandoffResets++
	r.mu.Unlock()
}

// Snapshot returns an immutable copy of the current metrics for assertions.
func (r *HandoffMetricsRecorder) Snapshot() HandoffMetricsSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snap := HandoffMetricsSnapshot{
		TotalSuccess:       r.totalSuccess,
		TotalFailure:       r.totalFailure,
		HandoffDurations:   append([]time.Duration(nil), r.handoffDurations...),
		PhaseDurations:     make(map[string][]time.Duration, len(r.phaseDurations)),
		CASConflicts:       r.casConflicts,
		ClaimStoreSizes:    append([]int(nil), r.claimStoreSizes...),
		StaleClaims:        r.staleClaims,
		StaleHandoffResets: r.staleHandoffResets,
	}
	for k, v := range r.phaseDurations {
		snap.PhaseDurations[k] = append([]time.Duration(nil), v...)
	}

	return snap
}

// HandoffMetricsSnapshot is an immutable view of recorded metrics for test assertions.
type HandoffMetricsSnapshot struct {
	TotalSuccess       int
	TotalFailure       int
	HandoffDurations   []time.Duration
	PhaseDurations     map[string][]time.Duration
	CASConflicts       int
	ClaimStoreSizes    []int
	StaleClaims        int
	StaleHandoffResets int
}
