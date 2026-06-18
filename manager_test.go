package parti

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// Mock implementations for testing
type mockSource struct{}

func (m *mockSource) Start(_ context.Context) error { return nil }
func (m *mockSource) Stop(_ context.Context) error  { return nil }

func (m *mockSource) List(_ /* ctx */ context.Context) ([]Partition, error) {
	return []Partition{{Keys: []string{"p0"}, Weight: 100}}, nil
}

type mockStrategy struct{}

func (m *mockStrategy) Assign(_ /* workers */ []string, partitions []Partition) (map[string][]Partition, error) {
	return map[string][]Partition{"worker-0": partitions}, nil
}

func TestNewManager_NilSafety(t *testing.T) {
	// Create minimal valid configuration
	cfg := &Config{
		WorkerIDPrefix: "worker",
		WorkerIDMax:    9,
	}

	// Mock NATS connection (would need real connection in integration tests)
	// Create a dummy NATS connection reference (nil JetStream will fail validation)
	conn := &nats.Conn{}
	js, _ := jetstream.New(conn) // js will be nil for placeholder conn; tests focus on constructor nil safety

	// Create mock source and strategy
	src := &mockSource{}
	strategy := &mockStrategy{}

	t.Run("without optional dependencies", func(t *testing.T) {
		// Create manager WITHOUT any optional dependencies
		mgr, err := NewManager(cfg, js, src, strategy)

		require.NoError(t, err)
		require.NotNil(t, mgr)

		// Verify optional fields get safe defaults (not nil)
		require.NotNil(t, mgr.hooks)      // defaults to NopHooks
		require.NotNil(t, mgr.metrics)    // defaults to nopMetrics
		require.NotNil(t, mgr.logger)     // defaults to nopLogger
		require.Nil(t, mgr.electionAgent) // electionAgent can still be nil (not used yet)

		// Verify internal methods don't panic even without custom implementations
		require.NotPanics(t, func() {
			mgr.logError("test error", "key", "value")
			// StateInit -> StateStable is invalid; transitionState must not panic
			mgr.transitionState(StateStable)
		})
	})

	t.Run("accepts optional hooks", func(t *testing.T) {
		hooks := &Hooks{}
		mgr, err := NewManager(cfg, js, src, strategy, WithHooks(hooks))

		require.NoError(t, err)
		require.NotNil(t, mgr)
	})
}

func TestNewManager_RequiredParameters(t *testing.T) {
	cfg := &Config{
		WorkerIDPrefix: "worker",
		WorkerIDMax:    9,
	}
	conn := &nats.Conn{}
	js, _ := jetstream.New(conn)
	src := &mockSource{}
	strategy := &mockStrategy{}

	t.Run("nil config", func(t *testing.T) {
		mgr, err := NewManager(nil, js, src, strategy)

		require.Error(t, err)
		require.ErrorIs(t, err, types.ErrInvalidConfig)
		require.Nil(t, mgr)
	})

	t.Run("nil connection", func(t *testing.T) {
		mgr, err := NewManager(cfg, nil, src, strategy)

		require.Error(t, err)
		require.ErrorIs(t, err, types.ErrNATSConnectionRequired)
		require.Nil(t, mgr)
	})

	t.Run("nil source", func(t *testing.T) {
		mgr, err := NewManager(cfg, js, nil, strategy)

		require.Error(t, err)
		require.ErrorIs(t, err, types.ErrPartitionSourceRequired)
		require.Nil(t, mgr)
	})

	t.Run("nil strategy", func(t *testing.T) {
		mgr, err := NewManager(cfg, js, src, nil)

		require.Error(t, err)
		require.ErrorIs(t, err, types.ErrAssignmentStrategyRequired)
		require.Nil(t, mgr)
	})
}

// TestManager_SetCapability_AtomicBitmask verifies that SetCapability correctly
// sets and clears individual bits in the capability bitmask, and that Capabilities
// reflects the live value after each mutation.
//
// SetCapability and Capabilities only touch the atomic.Uint32 capabilities field;
// no NATS connection or other infrastructure is needed.
func TestManager_SetCapability_AtomicBitmask(t *testing.T) {
	// Construct a minimal Manager directly — no NATS required for this test.
	mgr := &Manager{}

	// Initially all bits are clear.
	require.Equal(t, uint32(0), mgr.Capabilities())

	// Set CapAckV1.
	mgr.SetCapability(types.CapAckV1, true)
	require.NotZero(t, mgr.Capabilities()&types.CapAckV1, "CapAckV1 should be set")

	// Set CapTwoPhaseHandoff — must not disturb CapAckV1.
	mgr.SetCapability(types.CapTwoPhaseHandoff, true)
	require.NotZero(t, mgr.Capabilities()&types.CapAckV1)
	require.NotZero(t, mgr.Capabilities()&types.CapTwoPhaseHandoff)

	// Clear CapAckV1 — must not disturb CapTwoPhaseHandoff.
	mgr.SetCapability(types.CapAckV1, false)
	require.Zero(t, mgr.Capabilities()&types.CapAckV1, "CapAckV1 should be cleared")
	require.NotZero(t, mgr.Capabilities()&types.CapTwoPhaseHandoff, "CapTwoPhaseHandoff must remain set")

	// Set all three capability bits.
	mgr.SetCapability(types.CapAckV1, true)
	mgr.SetCapability(types.CapProcessingGate, true)
	all := types.CapAckV1 | types.CapTwoPhaseHandoff | types.CapProcessingGate
	require.Equal(t, all, mgr.Capabilities())

	// Clear all.
	mgr.SetCapability(types.CapAckV1, false)
	mgr.SetCapability(types.CapTwoPhaseHandoff, false)
	mgr.SetCapability(types.CapProcessingGate, false)
	require.Equal(t, uint32(0), mgr.Capabilities())
}

// TestManager_CapTwoPhaseHandoff_ReportsWhenWired verifies that CapTwoPhaseHandoff
// is set after a successful Start with EnableTwoPhaseHandoff=true, and remains
// clear when the feature is disabled.
//
// This is an integration test because CapTwoPhaseHandoff is set inside Start()
// after the coordinator is wired to its KV bucket — unit-stubbing that path would
// bypass the production wire-up sequence.
func TestManager_CapTwoPhaseHandoff_ReportsWhenWired(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	partitions := []types.Partition{
		{Keys: []string{"p0"}, Weight: 100},
	}
	src := source.NewStatic(partitions)
	assignStrat := strategy.NewConsistentHash()

	baseCfg := func() *Config {
		return &Config{
			WorkerIDPrefix:        "worker",
			WorkerIDMin:           0,
			WorkerIDMax:           9,
			WorkerIDTTL:           10 * time.Second,
			HeartbeatInterval:     500 * time.Millisecond,
			HeartbeatTTL:          2 * time.Second,
			ElectionTimeout:       2 * time.Second,
			StartupTimeout:        15 * time.Second,
			ShutdownTimeout:       5 * time.Second,
			ColdStartWindow:       1 * time.Second,
			PlannedScaleWindow:    500 * time.Millisecond,
			RestartDetectionRatio: 0.5,
			RebalanceCooldown:     100 * time.Millisecond,
			EmergencyGracePeriod:  750 * time.Millisecond,
			KVBuckets: KVBucketConfig{
				StableIDBucket:   "parti-stableid",
				ElectionBucket:   "parti-election",
				HeartbeatBucket:  "parti-heartbeat",
				AssignmentBucket: "parti-assignment",
				HandoffBucket:    "parti-handoff",
				HandoffTTL:       30 * time.Second,
			},
		}
	}

	t.Run("two-phase enabled: bit set after Start", func(t *testing.T) {
		cfg := baseCfg()
		cfg.EnableTwoPhaseHandoff = true

		mgr, err := NewManager(cfg, js, src, assignStrat)
		require.NoError(t, err)

		require.NoError(t, mgr.Start(ctx))
		defer func() {
			stopCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel2()
			_ = mgr.Stop(stopCtx)
		}()

		require.NotZero(t, mgr.Capabilities()&types.CapTwoPhaseHandoff,
			"CapTwoPhaseHandoff must be set when EnableTwoPhaseHandoff=true and Start succeeds")
	})

	t.Run("two-phase disabled: bit clear after Start", func(t *testing.T) {
		cfg := baseCfg()
		cfg.EnableTwoPhaseHandoff = false

		mgr, err := NewManager(cfg, js, src, assignStrat)
		require.NoError(t, err)

		require.NoError(t, mgr.Start(ctx))
		defer func() {
			stopCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel2()
			_ = mgr.Stop(stopCtx)
		}()

		require.Zero(t, mgr.Capabilities()&types.CapTwoPhaseHandoff,
			"CapTwoPhaseHandoff must be clear when EnableTwoPhaseHandoff=false")
	})
}

func TestManager_prepareStart(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		m := &Manager{
			cfg: Config{StartupTimeout: 5 * time.Second},
		}
		ctx, cancel, err := m.prepareStart(context.Background())
		require.NoError(t, err)
		require.NotNil(t, ctx)
		require.NotNil(t, cancel)
		defer cancel()

		// Verify context has timeout
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.WithinDuration(t, time.Now().Add(5*time.Second), deadline, 100*time.Millisecond)
	})

	t.Run("already started", func(t *testing.T) {
		m := &Manager{
			ctx: context.Background(), // Simulate started
		}
		ctx, cancel, err := m.prepareStart(context.Background())
		require.ErrorIs(t, err, types.ErrAlreadyStarted)
		require.Nil(t, ctx)
		require.NotNil(t, cancel) // Should return no-op cancel
		cancel()
	})

	t.Run("no timeout", func(t *testing.T) {
		m := &Manager{
			cfg: Config{StartupTimeout: 0},
		}
		ctx, cancel, err := m.prepareStart(context.Background())
		require.NoError(t, err)
		defer cancel()

		// Verify context has NO deadline (if parent doesn't)
		_, ok := ctx.Deadline()
		require.False(t, ok)
	})
}

// TestManager_warnOnFiniteMaxReconnects covers the read-only startup
// warning that fires when the caller-owned nats.Conn is configured
// with a finite MaxReconnects. -1 (unlimited) is the recommended
// posture and must be silent. Anything else (including 0 = disabled
// and any positive cap) must emit the warning exactly once.
//
// Defensive cases: a nil m.js or a nil m.js.Conn() must NOT panic and
// must NOT emit a warning (the helper is read-only and must not
// constrain test doubles that bypass the real JetStream surface).
func TestManager_warnOnFiniteMaxReconnects(t *testing.T) {
	// The helper accesses ONLY conn.Opts.MaxReconnects, which is a
	// value field on nats.Options. A zero-valued *nats.Conn with only
	// Opts populated is therefore safe to construct directly for this
	// unit test — no embedded NATS server required.
	// nats.Options field name is MaxReconnect (singular). The nats.MaxReconnects
	// setter (plural) is the Option-constructor; the underlying field is singular.
	mkConn := func(maxReconnect int) *nats.Conn {
		return &nats.Conn{Opts: nats.Options{MaxReconnect: maxReconnect}}
	}

	const warnSubstr = "finite MaxReconnect"

	// assertWarnedAbout fails the test unless exactly one WARN line
	// containing warnSubstr was emitted. Inlined assertion sidesteps
	// the unparam lint warning that fires on a single-call helper
	// whose substring argument never varies.
	assertWarnedOnce := func(t *testing.T, log *warnSpy) {
		t.Helper()
		var matches int
		for _, w := range log.snapshot() {
			if strings.Contains(w, warnSubstr) {
				matches++
			}
		}
		require.Equal(t, 1, matches, "expected exactly one warning matching %q; got warns=%v", warnSubstr, log.snapshot())
	}
	assertSilent := func(t *testing.T, log *warnSpy) {
		t.Helper()
		var matches int
		for _, w := range log.snapshot() {
			if strings.Contains(w, warnSubstr) {
				matches++
			}
		}
		require.Equal(t, 0, matches, "expected no warnings; got warns=%v", log.snapshot())
	}

	t.Run("unlimited -1 silent (recommended posture)", func(t *testing.T) {
		log := &warnSpy{}
		warnOnFiniteMaxReconnects(mkConn(-1), log)
		assertSilent(t, log)
	})

	t.Run("zero (disabled reconnect) warns", func(t *testing.T) {
		log := &warnSpy{}
		warnOnFiniteMaxReconnects(mkConn(0), log)
		assertWarnedOnce(t, log)
	})

	t.Run("finite positive warns", func(t *testing.T) {
		log := &warnSpy{}
		warnOnFiniteMaxReconnects(mkConn(5), log)
		assertWarnedOnce(t, log)
	})

	t.Run("nil conn silent (defensive)", func(t *testing.T) {
		log := &warnSpy{}
		require.NotPanics(t, func() {
			warnOnFiniteMaxReconnects(nil, log)
		})
		assertSilent(t, log)
	})
}

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

// setupTwoPhaseGateWarningTest wires a Manager with a spy logger and a
// caller-controlled capability stub. The stub's initial reported bit is
// `initialCaps`, which the test can mutate before driving apply.
//
// The spy is the shared warnSpy; each test owns its own instance, so the
// P0.3 test does not couple to P0.1's branch.
func setupTwoPhaseGateWarningTest(t *testing.T, twoPhase bool, initialCaps uint32) (*Manager, *warnSpy) {
	t.Helper()

	stub := &configurableGateUpdater{}
	stub.reported.Store(initialCaps)

	m, _, _, _ := newTestManager(t)
	m.cfg = Config{EnableTwoPhaseHandoff: twoPhase}
	spy := &warnSpy{}
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

func countTwoPhaseGateWarns(spy *warnSpy) int {
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
		spy := &warnSpy{}
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
