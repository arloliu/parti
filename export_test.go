package parti

import (
	"time"

	"github.com/arloliu/parti/v2/internal/assignment"
)

// LabelSpillGraceForTest returns the Manager's resolved (post-option) copy of
// Config.LabelSpillGrace — the value startCalculator passes to the calculator.
// Exposed via export_test.go so cross-package tests can assert the
// WithLabelSpillGrace override without making the config copy public.
func LabelSpillGraceForTest(m *Manager) time.Duration {
	return m.cfg.LabelSpillGrace
}

// CalculatorForTest returns the Manager's internal *assignment.Calculator
// when one is wired (i.e., the manager is the leader and startCalculator
// has run). Returns nil if the manager is a follower or before startup.
//
// This is exposed via export_test.go so cross-package tests in parti_test
// can read the Calculator's embedded Config — particularly the
// EnableTwoPhaseHandoff flag — without making the field public.
func CalculatorForTest(m *Manager) *assignment.Calculator {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if calc, ok := m.calculator.(*assignment.Calculator); ok {
		return calc
	}
	return nil
}
