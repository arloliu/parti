package assignment

import (
	"context"
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
		ctx := context.Background()
		
		// Create test partitions
		partitions := make([]types.Partition, 5)
		for i := 0; i < 5; i++ {
			partitions[i] = types.Partition{Keys: []string{string(rune('A' + i))}}
		}
		
		partitionSource := source.NewStatic(partitions)
		assignmentStrategy := strategy.NewRoundRobin()

		calc := NewCalculator(
			nc,
			"test-assignment",
			"test-heartbeat",
			"worker",
			partitionSource,
			assignmentStrategy,
		)

		// Default cooldown should be 10s
		require.Equal(t, 10*time.Second, calc.cooldown)

		t.Logf("✅ Default cooldown is 10 seconds")
	})

	t.Run("can set custom cooldown", func(t *testing.T) {
		ctx := context.Background()
		
		partitions := make([]types.Partition, 5)
		for i := 0; i < 5; i++ {
			partitions[i] = types.Partition{Keys: []string{string(rune('A' + i))}}
		}
		
		partitionSource := source.NewStatic(partitions)
		assignmentStrategy := strategy.NewRoundRobin()

		calc := NewCalculator(
			nc,
			"test-assignment",
			"test-heartbeat",
			"worker",
			partitionSource,
			assignmentStrategy,
		)

		// Set custom cooldown
		customCooldown := 2 * time.Second
		calc.SetCooldown(customCooldown)

		require.Equal(t, customCooldown, calc.cooldown)

		t.Logf("✅ Successfully set custom cooldown to %v", customCooldown)
	})

	t.Run("cooldown blocks rapid rebalances", func(t *testing.T) {
		// This test verifies that the cooldown actually prevents
		// rapid successive rebalances
		
		t.Skip("Integration test needed - requires actual rebalance scenarios")
	})
}
