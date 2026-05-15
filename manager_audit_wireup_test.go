package parti_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/assignment"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestStartCalculator_PropagatesEnableTwoPhaseHandoff is the regression
// guard for P0 #1 (post-impl review v1). The audit's escalation path
// (§4.2) refuses to escalate when its embedded Config.EnableTwoPhaseHandoff
// is false. Before the v1 fix, startCalculator built the assignment.Config
// literal without copying m.cfg.EnableTwoPhaseHandoff, so production
// calculators always saw the zero value and the audit emitted "direct_mode"
// even when two-phase mode was enabled.
//
// We verify by starting a real Manager with two-phase enabled and reading
// the embedded Config.EnableTwoPhaseHandoff field from the calculator that
// startCalculator builds.
func TestStartCalculator_PropagatesEnableTwoPhaseHandoff(t *testing.T) {
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := parti.DefaultConfig()
	cfg.StartupTimeout = 5 * time.Second
	cfg.WorkerIDTTL = 2 * time.Second
	cfg.HeartbeatTTL = 1 * time.Second
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.EmergencyGracePeriod = 750 * time.Millisecond
	cfg.EnableTwoPhaseHandoff = true

	src := source.NewStatic([]types.Partition{})
	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewRoundRobin())
	require.NoError(t, err)
	defer func() { _ = mgr.Stop(context.Background()) }()

	require.NoError(t, mgr.Start(context.Background()))

	// Only the leader runs a real Calculator; the single-worker startup
	// always wins election in <StartupTimeout.
	require.Eventually(t, mgr.IsLeader, 3*time.Second, 25*time.Millisecond)

	// Inspect the calculator's Config.EnableTwoPhaseHandoff via the
	// internal accessor; the Calculator embeds Config anonymously, so this
	// reads the exact field startCalculator populated.
	calc := parti.CalculatorForTest(mgr)
	require.NotNil(t, calc, "leader manager must have a real Calculator")
	require.True(t, calc.EnableTwoPhaseHandoff,
		"Calculator.EnableTwoPhaseHandoff MUST mirror Manager.cfg.EnableTwoPhaseHandoff")
}

// TestStartCalculator_DefaultsToDirectMode is the symmetric check: when
// the Manager-level flag is false, the calculator's flag is also false
// (so the audit's "direct_mode" skip path remains reachable).
func TestStartCalculator_DefaultsToDirectMode(t *testing.T) {
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := parti.DefaultConfig()
	cfg.StartupTimeout = 5 * time.Second
	cfg.WorkerIDTTL = 2 * time.Second
	cfg.HeartbeatTTL = 1 * time.Second
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.EmergencyGracePeriod = 750 * time.Millisecond
	// EnableTwoPhaseHandoff is false (default).

	src := source.NewStatic([]types.Partition{})
	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewRoundRobin())
	require.NoError(t, err)
	defer func() { _ = mgr.Stop(context.Background()) }()

	require.NoError(t, mgr.Start(context.Background()))
	require.Eventually(t, mgr.IsLeader, 3*time.Second, 25*time.Millisecond)

	calc := parti.CalculatorForTest(mgr)
	require.NotNil(t, calc)
	require.False(t, calc.EnableTwoPhaseHandoff,
		"direct mode default: calculator's flag must be false")
}

// Compile-time check that Calculator's EnableTwoPhaseHandoff is exported.
var _ = assignment.Calculator{}.EnableTwoPhaseHandoff
