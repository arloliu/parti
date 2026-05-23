package parti

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// configurableGateUpdater is a WorkerConsumerUpdater + CapabilityReporter
// whose reported capability bits are caller-controlled. Unlike
// gateReportingUpdater (which flips the gate ON in UpdateWorkerConsumer),
// this stub leaves the bits at whatever the test set them to, letting the
// F10-B warning test exercise the "consumer never wires the gate"
// misconfiguration scenario.
type configurableGateUpdater struct {
	reported atomic.Uint32
	updates  atomic.Int64
}

func (g *configurableGateUpdater) UpdateWorkerConsumer(_ context.Context, _ string, _ []Partition) error {
	g.updates.Add(1)
	return nil
}

func (g *configurableGateUpdater) Capabilities() uint32 { return g.reported.Load() }

// twoPhaseWarnSpy captures WARN messages for assertion. Mirrors the
// shape of warnCaptureLogger (manager_setup_test.go) but lives here so
// the P0.3 test does not couple to P0.1's branch.
type twoPhaseWarnSpy struct {
	mu    sync.Mutex
	warns []string
}

func (l *twoPhaseWarnSpy) Debug(string, ...any) {}
func (l *twoPhaseWarnSpy) Info(string, ...any)  {}
func (l *twoPhaseWarnSpy) Warn(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}
func (l *twoPhaseWarnSpy) Error(string, ...any) {}
func (l *twoPhaseWarnSpy) Fatal(string, ...any) {}

func (l *twoPhaseWarnSpy) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.warns))
	copy(out, l.warns)

	return out
}

// setupTwoPhaseGateWarningTest wires a Manager with a spy logger and a
// caller-controlled capability stub. The stub's initial reported bit is
// `initialCaps`, which the test can mutate before driving apply.
func setupTwoPhaseGateWarningTest(t *testing.T, twoPhase bool, initialCaps uint32) (*Manager, *twoPhaseWarnSpy) {
	t.Helper()

	stub := &configurableGateUpdater{}
	stub.reported.Store(initialCaps)

	m, _, _, _ := newTestManager(t)
	m.cfg = Config{EnableTwoPhaseHandoff: twoPhase}
	spy := &twoPhaseWarnSpy{}
	m.logger = spy
	m.consumerUpdater = stub
	m.capReporter = asCapabilityReporter(stub)
	m.handoffCoordinator = &forwardingHandoff{updater: stub}

	return m, spy
}

func driveOneApply(t *testing.T, m *Manager, version int64) {
	t.Helper()
	err := m.applyAssignmentWithPrev(Assignment{}, Assignment{
		Version:        version,
		LeaderRevision: uint64(version), //nolint:gosec // monotonic test sequence; non-negative
		Partitions:     []Partition{{Keys: []string{"p1"}}},
	})
	require.NoError(t, err, "applyAssignmentWithPrev must succeed for the warning-path tests")
}

func countTwoPhaseGateWarns(spy *twoPhaseWarnSpy) int {
	n := 0
	for _, w := range spy.snapshot() {
		if strings.Contains(w, "two-phase handoff is enabled but the consumer reports no processing gate") {
			n++
		}
	}

	return n
}

// TestManager_F10B_TwoPhaseHandoffWithoutGate_Warns covers the five
// configurations that distinguish the F10-B warning's intended
// behavior from any silent-failure or false-positive mode.
func TestManager_F10B_TwoPhaseHandoffWithoutGate_Warns(t *testing.T) {
	t.Parallel()
	t.Run("two-phase ON + no gate reported → warns once", func(t *testing.T) {
		t.Parallel()
		m, spy := setupTwoPhaseGateWarningTest(t, true, 0)
		driveOneApply(t, m, 1)
		require.Equal(t, 1, countTwoPhaseGateWarns(spy),
			"misconfigured two-phase handoff must surface a single WARN")
	})

	t.Run("two-phase ON + gate reported → silent", func(t *testing.T) {
		t.Parallel()
		m, spy := setupTwoPhaseGateWarningTest(t, true, types.CapProcessingGate)
		driveOneApply(t, m, 1)
		require.Equal(t, 0, countTwoPhaseGateWarns(spy),
			"the happy path (gate present) must be silent")
	})

	t.Run("two-phase ON + no gate, repeated applies → warns only once", func(t *testing.T) {
		t.Parallel()
		m, spy := setupTwoPhaseGateWarningTest(t, true, 0)
		driveOneApply(t, m, 1)
		driveOneApply(t, m, 2)
		driveOneApply(t, m, 3)
		require.Equal(t, 1, countTwoPhaseGateWarns(spy),
			"capProcessingGateWarned guard must suppress repeat applies")
	})

	t.Run("two-phase OFF → silent regardless of caps", func(t *testing.T) {
		t.Parallel()
		m, spy := setupTwoPhaseGateWarningTest(t, false, 0)
		driveOneApply(t, m, 1)
		require.Equal(t, 0, countTwoPhaseGateWarns(spy),
			"the flag gates the warning; nothing to warn about when off")
	})

	t.Run("nil capReporter → silent (limitation acknowledged)", func(t *testing.T) {
		t.Parallel()
		m, _, _, _ := newTestManager(t)
		m.cfg = Config{EnableTwoPhaseHandoff: true}
		spy := &twoPhaseWarnSpy{}
		m.logger = spy
		// Important: leave m.capReporter == nil (no CapabilityReporter
		// wired). The fwdHandoff still needs an updater; provide a
		// non-reporting stub for that.
		nonReportingStub := &configurableGateUpdater{}
		m.consumerUpdater = nonReportingStub
		m.capReporter = nil
		m.handoffCoordinator = &forwardingHandoff{updater: nonReportingStub}

		driveOneApply(t, m, 1)
		require.Equal(t, 0, countTwoPhaseGateWarns(spy),
			"without a CapabilityReporter the gate signal is undetectable; the warning correctly stays silent (documented limitation)")
	})
}
