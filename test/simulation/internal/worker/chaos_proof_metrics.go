// Package worker's ChaosProofMetrics gives label chaos scenarios a
// race-free way to positively prove four code paths actually fired,
// rather than inferring it from the absence of oracle violations (which
// can pass vacuously if the chaos primitive silently took a different,
// already-tested path — e.g. a relabel landing on an expired heartbeat
// key instead of a live one). All four signals are already public
// production API reachable through parti.WithMetrics; this file adds no
// new hooks to internal/assignment or the root parti package. Pattern
// copied from test/integration/assignment/label_policy_test.go's
// recordingLabelMetrics.
package worker

import (
	"sync/atomic"

	imetrics "github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
)

// ChaosProofMetrics embeds internal/metrics.NopMetrics (satisfying the full
// types.MetricsCollector surface with no-ops) and overrides exactly the
// four methods this plan's chaos scenarios need to positively prove:
//   - IncrementLabelChangeTrigger: the worker-monitor's tight-takeover
//     fingerprint-mismatch callback fired (internal/assignment/worker_monitor.go
//     checkLabelChange -> calculator.go's SetOnLabelChange registration).
//   - RecordEmergencyRebalance: the calculator's emergency carve-out claim
//     succeeded (internal/assignment/calculator.go claimEmergency).
//   - RecordRebalanceAttempt filtered to reason=="label_recheck", success:
//     monitorLabelRecheck actually won its CAS/claim and ran a rebalance
//     (as opposed to merely setting the internal, unobservable
//     pendingLabelRecheck flag).
//   - IncrementLabelSpill: a partition spilled from its labeled pool to an
//     unlabeled fallback worker (used to prove a whole-label-pool-outage
//     scenario actually triggered a spill, not just that no violation was
//     observed).
type ChaosProofMetrics struct {
	*imetrics.NopMetrics

	labelChangeTriggers atomic.Int64
	emergencyClaims     atomic.Int64
	labelRechecksRun    atomic.Int64
	labelSpills         atomic.Int64
}

var (
	_ types.MetricsCollector = (*ChaosProofMetrics)(nil)
	_ types.LabelMetrics     = (*ChaosProofMetrics)(nil)
)

// NewChaosProofMetrics constructs a zeroed collector.
func NewChaosProofMetrics() *ChaosProofMetrics {
	return &ChaosProofMetrics{NopMetrics: imetrics.NewNop()}
}

// IncrementLabelChangeTrigger implements types.LabelMetrics.
func (m *ChaosProofMetrics) IncrementLabelChangeTrigger() {
	m.labelChangeTriggers.Add(1)
}

// LabelChangeTriggers returns the count of tight-takeover fingerprint-change
// callbacks observed.
func (m *ChaosProofMetrics) LabelChangeTriggers() int64 {
	return m.labelChangeTriggers.Load()
}

// RecordEmergencyRebalance implements types.CalculatorMetrics. The
// disappearedWorkers argument is discarded — this collector only needs the
// call count, not the magnitude.
func (m *ChaosProofMetrics) RecordEmergencyRebalance(_ int) {
	m.emergencyClaims.Add(1)
}

// EmergencyClaims returns the count of successful emergency-carve-out
// claims observed.
func (m *ChaosProofMetrics) EmergencyClaims() int64 {
	return m.emergencyClaims.Load()
}

// RecordRebalanceAttempt implements types.CalculatorMetrics. Only
// successful attempts with reason "label_recheck" increment
// LabelRechecksRun; every other reason/outcome is discarded.
func (m *ChaosProofMetrics) RecordRebalanceAttempt(reason string, success bool) {
	if reason == "label_recheck" && success {
		m.labelRechecksRun.Add(1)
	}
}

// LabelRechecksRun returns the count of coalesced label-recheck rebalances
// that actually ran (as opposed to being requested/coalesced away).
func (m *ChaosProofMetrics) LabelRechecksRun() int64 {
	return m.labelRechecksRun.Load()
}

// IncrementLabelSpill implements types.LabelMetrics. The label argument is
// discarded — this collector only needs the total spill count, not a
// per-label breakdown.
func (m *ChaosProofMetrics) IncrementLabelSpill(_ string) {
	m.labelSpills.Add(1)
}

// LabelSpills returns the total count of partitions that spilled from a
// labeled pool to an unlabeled fallback worker, across all labels.
func (m *ChaosProofMetrics) LabelSpills() int64 {
	return m.labelSpills.Load()
}
