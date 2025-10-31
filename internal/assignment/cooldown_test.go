package assignment

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	partitest "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
)

// TestCalculatorCooldown tests the rebalance cooldown behavior.
func TestCalculatorCooldown(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)

	t.Run("default cooldown is 10 seconds", func(t *testing.T) {
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-default-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-default-heartbeat")

		// Create test partitions
		partitions := make([]types.Partition, 5)
		for i := 0; i < 5; i++ {
			partitions[i] = types.Partition{Keys: []string{string(rune('A' + i))}}
		}

		partitionSource := source.NewStatic(partitions)
		assignmentStrategy := strategy.NewRoundRobin()

		calc, err := NewCalculator(&Config{
			AssignmentKV:         assignmentKV,
			HeartbeatKV:          heartbeatKV,
			AssignmentPrefix:     "test-assignment",
			Source:               partitionSource,
			Strategy:             assignmentStrategy,
			HeartbeatPrefix:      "worker",
			HeartbeatTTL:         5 * time.Second,
			EmergencyGracePeriod: 2 * time.Second, // Emergency grace period,
		})
		require.NoError(t, err)

		// Default cooldown should be 10s
		require.Equal(t, 10*time.Second, calc.Cooldown)

		t.Log("Default cooldown is 10 seconds")
	})

	t.Run("can set custom cooldown", func(t *testing.T) {
		assignmentKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-custom-assignment")
		heartbeatKV := partitest.CreateJetStreamKV(t, nc, "test-cooldown-custom-heartbeat")

		partitions := make([]types.Partition, 5)
		for i := 0; i < 5; i++ {
			partitions[i] = types.Partition{Keys: []string{string(rune('A' + i))}}
		}

		partitionSource := source.NewStatic(partitions)
		assignmentStrategy := strategy.NewRoundRobin()

		calc, err := NewCalculator(&Config{
			AssignmentKV:         assignmentKV,
			HeartbeatKV:          heartbeatKV,
			AssignmentPrefix:     "test-assignment",
			Source:               partitionSource,
			Strategy:             assignmentStrategy,
			HeartbeatPrefix:      "worker",
			HeartbeatTTL:         5 * time.Second,
			EmergencyGracePeriod: 2 * time.Second, // Emergency grace period,
		})
		require.NoError(t, err)

		// Set custom cooldown
		customCooldown := 2 * time.Second
		calc.SetCooldown(customCooldown)

		require.Equal(t, customCooldown, calc.Cooldown)

		t.Logf("Successfully set custom cooldown to %v", customCooldown)
	})

	// Note: Cooldown blocking behavior is tested in integration tests
	// (test/integration/) which simulate actual rebalance scenarios
}
