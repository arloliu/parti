package parti

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	require.Equal(t, "worker", cfg.WorkerIDPrefix)
	require.Equal(t, 0, cfg.WorkerIDMin)
	require.Equal(t, 99, cfg.WorkerIDMax)
	require.Equal(t, 30*time.Second, cfg.WorkerIDTTL)
	require.Equal(t, 2*time.Second, cfg.HeartbeatInterval)
	require.Equal(t, 6*time.Second, cfg.HeartbeatTTL)
	require.Equal(t, 30*time.Second, cfg.ColdStartWindow)
	require.Equal(t, 10*time.Second, cfg.PlannedScaleWindow)
	require.Equal(t, 0.5, cfg.RestartDetectionRatio)
	require.Equal(t, 10*time.Second, cfg.OperationTimeout)
	require.Equal(t, 5*time.Second, cfg.ElectionTimeout)
	require.Equal(t, 30*time.Second, cfg.StartupTimeout)
	require.Equal(t, 10*time.Second, cfg.ShutdownTimeout)
	require.Equal(t, 0.15, cfg.Assignment.MinRebalanceThreshold)
	require.Equal(t, 10*time.Second, cfg.Assignment.RebalanceCooldown)
}

func TestApplyDefaults(t *testing.T) {
	t.Run("applies defaults to empty config", func(t *testing.T) {
		cfg := Config{}
		ApplyDefaults(&cfg)

		require.Equal(t, "worker", cfg.WorkerIDPrefix)
		require.Equal(t, 99, cfg.WorkerIDMax)
		require.Equal(t, 30*time.Second, cfg.WorkerIDTTL)
		require.Equal(t, 0.15, cfg.Assignment.MinRebalanceThreshold)
		require.Equal(t, 10*time.Second, cfg.Assignment.RebalanceCooldown)
	})

	t.Run("preserves custom values", func(t *testing.T) {
		cfg := Config{
			WorkerIDPrefix:        "custom",
			WorkerIDMin:           10,
			WorkerIDMax:           200,
			WorkerIDTTL:           60 * time.Second,
			HeartbeatInterval:     5 * time.Second,
			HeartbeatTTL:          15 * time.Second,
			ColdStartWindow:       45 * time.Second,
			PlannedScaleWindow:    20 * time.Second,
			RestartDetectionRatio: 0.7,
			OperationTimeout:      20 * time.Second,
			ElectionTimeout:       10 * time.Second,
			StartupTimeout:        60 * time.Second,
			ShutdownTimeout:       20 * time.Second,
			Assignment: AssignmentConfig{
				MinRebalanceThreshold: 0.25,
				RebalanceCooldown:     15 * time.Second,
			},
		}
		ApplyDefaults(&cfg)

		// All custom values should be preserved
		require.Equal(t, "custom", cfg.WorkerIDPrefix)
		require.Equal(t, 10, cfg.WorkerIDMin)
		require.Equal(t, 200, cfg.WorkerIDMax)
		require.Equal(t, 60*time.Second, cfg.WorkerIDTTL)
		require.Equal(t, 5*time.Second, cfg.HeartbeatInterval)
		require.Equal(t, 15*time.Second, cfg.HeartbeatTTL)
		require.Equal(t, 45*time.Second, cfg.ColdStartWindow)
		require.Equal(t, 20*time.Second, cfg.PlannedScaleWindow)
		require.Equal(t, 0.7, cfg.RestartDetectionRatio)
		require.Equal(t, 20*time.Second, cfg.OperationTimeout)
		require.Equal(t, 10*time.Second, cfg.ElectionTimeout)
		require.Equal(t, 60*time.Second, cfg.StartupTimeout)
		require.Equal(t, 20*time.Second, cfg.ShutdownTimeout)
		require.Equal(t, 0.25, cfg.Assignment.MinRebalanceThreshold)
		require.Equal(t, 15*time.Second, cfg.Assignment.RebalanceCooldown)
	})

	t.Run("applies partial defaults", func(t *testing.T) {
		cfg := Config{
			WorkerIDPrefix: "myworker",
			WorkerIDTTL:    45 * time.Second,
			// Leave other fields empty
		}
		ApplyDefaults(&cfg)

		// Custom values preserved
		require.Equal(t, "myworker", cfg.WorkerIDPrefix)
		require.Equal(t, 45*time.Second, cfg.WorkerIDTTL)
		// Defaults applied
		require.Equal(t, 99, cfg.WorkerIDMax)
		require.Equal(t, 2*time.Second, cfg.HeartbeatInterval)
		require.Equal(t, 0.15, cfg.Assignment.MinRebalanceThreshold)
	})
}

// TestConfig_YAML demonstrates that time.Duration works directly with YAML unmarshaling
func TestConfig_YAML(t *testing.T) {
	yamlConfig := `
workerIdPrefix: "my-worker"
workerIdMin: 0
workerIdMax: 50
workerIdTtl: 45s
heartbeatInterval: 3s
heartbeatTtl: 9s
coldStartWindow: 1m
plannedScaleWindow: 15s
restartDetectionRatio: 0.6
operationTimeout: 15s
electionTimeout: 8s
startupTimeout: 45s
shutdownTimeout: 15s
assignment:
  minRebalanceThreshold: 0.2
  rebalanceCooldown: 12s
`

	var cfg Config
	err := yaml.Unmarshal([]byte(yamlConfig), &cfg)
	require.NoError(t, err)

	// Verify durations were parsed correctly
	require.Equal(t, "my-worker", cfg.WorkerIDPrefix)
	require.Equal(t, 50, cfg.WorkerIDMax)
	require.Equal(t, 45*time.Second, cfg.WorkerIDTTL)
	require.Equal(t, 3*time.Second, cfg.HeartbeatInterval)
	require.Equal(t, 9*time.Second, cfg.HeartbeatTTL)
	require.Equal(t, 1*time.Minute, cfg.ColdStartWindow)
	require.Equal(t, 15*time.Second, cfg.PlannedScaleWindow)
	require.Equal(t, 0.6, cfg.RestartDetectionRatio)
	require.Equal(t, 15*time.Second, cfg.OperationTimeout)
	require.Equal(t, 8*time.Second, cfg.ElectionTimeout)
	require.Equal(t, 45*time.Second, cfg.StartupTimeout)
	require.Equal(t, 15*time.Second, cfg.ShutdownTimeout)
	require.Equal(t, 0.2, cfg.Assignment.MinRebalanceThreshold)
	require.Equal(t, 12*time.Second, cfg.Assignment.RebalanceCooldown)
}

// TestConfig_DefaultsWithPartialYAML demonstrates using ApplyDefaults with partial config
func TestConfig_DefaultsWithPartialYAML(t *testing.T) {
	// Only specify a few fields, rest will use defaults
	yamlConfig := `
workerIdPrefix: "custom"
heartbeatInterval: 5s
`

	var cfg Config
	err := yaml.Unmarshal([]byte(yamlConfig), &cfg)
	require.NoError(t, err)

	// Apply defaults for unset fields
	ApplyDefaults(&cfg)

	// Custom values preserved
	require.Equal(t, "custom", cfg.WorkerIDPrefix)
	require.Equal(t, 5*time.Second, cfg.HeartbeatInterval)

	// Defaults applied
	require.Equal(t, 99, cfg.WorkerIDMax)
	require.Equal(t, 6*time.Second, cfg.HeartbeatTTL)
	require.Equal(t, 30*time.Second, cfg.WorkerIDTTL)
	require.Equal(t, 0.15, cfg.Assignment.MinRebalanceThreshold)
	require.Equal(t, 10*time.Second, cfg.Assignment.RebalanceCooldown)
}
