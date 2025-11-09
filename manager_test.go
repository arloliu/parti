package parti

import (
	"context"
	"testing"

	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// Mock implementations for testing
type mockSource struct{}

func (m *mockSource) ListPartitions(_ /* ctx */ context.Context) ([]Partition, error) {
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
			mgr.transitionState(StateInit, StateStable)
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
