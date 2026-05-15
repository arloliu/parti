package parti

import (
	"context"
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
