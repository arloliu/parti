package worker

import (
	"testing"

	"github.com/arloliu/parti/v2/types"
)

func TestChaosProofMetrics_SatisfiesInterfaces(t *testing.T) {
	t.Parallel()
	var (
		_ types.MetricsCollector = (*ChaosProofMetrics)(nil)
		_ types.LabelMetrics     = (*ChaosProofMetrics)(nil)
	)
}

func TestChaosProofMetrics_CountersStartAtZero(t *testing.T) {
	t.Parallel()
	m := NewChaosProofMetrics()
	if m.LabelChangeTriggers() != 0 || m.EmergencyClaims() != 0 || m.LabelRechecksRun() != 0 || m.LabelSpills() != 0 {
		t.Fatalf("expected all counters to start at 0, got %d/%d/%d/%d",
			m.LabelChangeTriggers(), m.EmergencyClaims(), m.LabelRechecksRun(), m.LabelSpills())
	}
}

func TestChaosProofMetrics_IncrementLabelSpill(t *testing.T) {
	t.Parallel()
	m := NewChaosProofMetrics()
	m.IncrementLabelSpill("vip-a")
	m.IncrementLabelSpill("vip-a")
	m.IncrementLabelSpill("vip-b")
	if got := m.LabelSpills(); got != 3 {
		t.Errorf("LabelSpills() = %d, want 3 (total across all labels)", got)
	}
}

func TestChaosProofMetrics_IncrementLabelChangeTrigger(t *testing.T) {
	t.Parallel()
	m := NewChaosProofMetrics()
	m.IncrementLabelChangeTrigger()
	m.IncrementLabelChangeTrigger()
	if got := m.LabelChangeTriggers(); got != 2 {
		t.Errorf("LabelChangeTriggers() = %d, want 2", got)
	}
}

func TestChaosProofMetrics_RecordEmergencyRebalance(t *testing.T) {
	t.Parallel()
	m := NewChaosProofMetrics()
	m.RecordEmergencyRebalance(3)
	if got := m.EmergencyClaims(); got != 1 {
		t.Errorf("EmergencyClaims() = %d, want 1 (call count, not the disappeared-worker count)", got)
	}
}

func TestChaosProofMetrics_RecordRebalanceAttempt_FiltersToLabelRecheckSuccess(t *testing.T) {
	t.Parallel()
	m := NewChaosProofMetrics()
	m.RecordRebalanceAttempt("label_recheck", false) // failed attempt: must not count
	m.RecordRebalanceAttempt("cold_start", true)     // wrong reason: must not count
	m.RecordRebalanceAttempt("label_recheck", true)  // the one that counts
	if got := m.LabelRechecksRun(); got != 1 {
		t.Errorf("LabelRechecksRun() = %d, want 1", got)
	}
}
