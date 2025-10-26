package parti

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go"
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
	conn := &nats.Conn{} // Placeholder - would be real connection in actual use

	// Create mock source and strategy
	src := &mockSource{}
	strategy := &mockStrategy{}

	t.Run("without optional dependencies", func(t *testing.T) {
		// Create manager WITHOUT any optional dependencies
		mgr, err := NewManager(cfg, conn, src, strategy)

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
		mgr, err := NewManager(cfg, conn, src, strategy, WithHooks(hooks))

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
	src := &mockSource{}
	strategy := &mockStrategy{}

	t.Run("nil config", func(t *testing.T) {
		mgr, err := NewManager(nil, conn, src, strategy)

		require.Error(t, err)
		require.ErrorIs(t, err, ErrInvalidConfig)
		require.Nil(t, mgr)
	})

	t.Run("nil connection", func(t *testing.T) {
		mgr, err := NewManager(cfg, nil, src, strategy)

		require.Error(t, err)
		require.ErrorIs(t, err, ErrNATSConnectionRequired)
		require.Nil(t, mgr)
	})

	t.Run("nil source", func(t *testing.T) {
		mgr, err := NewManager(cfg, conn, nil, strategy)

		require.Error(t, err)
		require.ErrorIs(t, err, ErrPartitionSourceRequired)
		require.Nil(t, mgr)
	})

	t.Run("nil strategy", func(t *testing.T) {
		mgr, err := NewManager(cfg, conn, src, nil)

		require.Error(t, err)
		require.ErrorIs(t, err, ErrAssignmentStrategyRequired)
		require.Nil(t, mgr)
	})
}
