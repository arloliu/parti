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
	require.Equal(t, 10*time.Second, cfg.Assignment.MinRebalanceInterval)
}

func TestSetDefaults(t *testing.T) {
	t.Run("applies defaults to empty config", func(t *testing.T) {
		cfg := Config{}
		SetDefaults(&cfg)

		require.Equal(t, "worker", cfg.WorkerIDPrefix)
		require.Equal(t, 99, cfg.WorkerIDMax)
		require.Equal(t, 30*time.Second, cfg.WorkerIDTTL)
		require.Equal(t, 0.15, cfg.Assignment.MinRebalanceThreshold)
		require.Equal(t, 10*time.Second, cfg.Assignment.MinRebalanceInterval)
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
				MinRebalanceInterval:  15 * time.Second,
			},
		}
		SetDefaults(&cfg)

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
		require.Equal(t, 15*time.Second, cfg.Assignment.MinRebalanceInterval)
	})

	t.Run("applies partial defaults", func(t *testing.T) {
		cfg := Config{
			WorkerIDPrefix: "myworker",
			WorkerIDTTL:    45 * time.Second,
			// Leave other fields empty
		}
		SetDefaults(&cfg)

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
  minRebalanceInterval: 12s
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
	require.Equal(t, 12*time.Second, cfg.Assignment.MinRebalanceInterval)
}

// TestConfig_DefaultsWithPartialYAML demonstrates using SetDefaults with partial config
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
	SetDefaults(&cfg)

	// Custom values preserved
	require.Equal(t, "custom", cfg.WorkerIDPrefix)
	require.Equal(t, 5*time.Second, cfg.HeartbeatInterval)

	// Defaults applied
	require.Equal(t, 99, cfg.WorkerIDMax)
	require.Equal(t, 6*time.Second, cfg.HeartbeatTTL)
	require.Equal(t, 30*time.Second, cfg.WorkerIDTTL)
	require.Equal(t, 0.15, cfg.Assignment.MinRebalanceThreshold)
	require.Equal(t, 10*time.Second, cfg.Assignment.MinRebalanceInterval)
}

func TestConfigValidate(t *testing.T) {
	t.Run("valid default config passes validation", func(t *testing.T) {
		cfg := DefaultConfig()
		err := cfg.Validate()
		require.NoError(t, err)
	})

	t.Run("valid test config passes validation", func(t *testing.T) {
		cfg := TestConfig()
		err := cfg.Validate()
		require.NoError(t, err)
	})

	t.Run("HeartbeatTTL too short fails validation", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.HeartbeatTTL = 1 * time.Second
		cfg.HeartbeatInterval = 2 * time.Second

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "HeartbeatTTL")
		require.Contains(t, err.Error(), "2*HeartbeatInterval")
	})

	t.Run("WorkerIDTTL less than 3x HeartbeatInterval fails", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.HeartbeatInterval = 5 * time.Second
		cfg.WorkerIDTTL = 10 * time.Second // Less than 3x (15s)
		cfg.HeartbeatTTL = 15 * time.Second

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "WorkerIDTTL")
		require.Contains(t, err.Error(), "3*HeartbeatInterval")
	})

	t.Run("WorkerIDTTL less than HeartbeatTTL fails", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.HeartbeatInterval = 2 * time.Second
		cfg.HeartbeatTTL = 10 * time.Second
		cfg.WorkerIDTTL = 8 * time.Second // Less than HeartbeatTTL

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "WorkerIDTTL")
		require.Contains(t, err.Error(), "HeartbeatTTL")
	})

	t.Run("zero MinRebalanceInterval fails validation", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Assignment.MinRebalanceInterval = 0

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "MinRebalanceInterval")
		require.Contains(t, err.Error(), "> 0")
	})

	t.Run("negative MinRebalanceInterval fails validation", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Assignment.MinRebalanceInterval = -5 * time.Second

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "MinRebalanceInterval")
	})

	t.Run("ColdStartWindow less than PlannedScaleWindow fails", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.ColdStartWindow = 5 * time.Second
		cfg.PlannedScaleWindow = 10 * time.Second

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "ColdStartWindow")
		require.Contains(t, err.Error(), "PlannedScaleWindow")
	})

	t.Run("MinRebalanceInterval exceeds ColdStartWindow fails", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Assignment.MinRebalanceInterval = 40 * time.Second
		cfg.ColdStartWindow = 30 * time.Second

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "MinRebalanceInterval")
		require.Contains(t, err.Error(), "ColdStartWindow")
	})

	t.Run("valid custom config with tight timings", func(t *testing.T) {
		cfg := Config{
			WorkerIDPrefix:        "worker",
			WorkerIDMin:           0,
			WorkerIDMax:           99,
			HeartbeatInterval:     1 * time.Second,
			HeartbeatTTL:          2 * time.Second, // 2x interval (minimum)
			WorkerIDTTL:           3 * time.Second, // 3x interval (minimum)
			ColdStartWindow:       10 * time.Second,
			PlannedScaleWindow:    10 * time.Second, // Equal is valid
			OperationTimeout:      10 * time.Second,
			ElectionTimeout:       5 * time.Second,
			StartupTimeout:        30 * time.Second,
			ShutdownTimeout:       10 * time.Second,
			RestartDetectionRatio: 0.5,
			Assignment: AssignmentConfig{
				MinRebalanceThreshold: 0.15,
				MinRebalanceInterval:  5 * time.Second,
			},
			KVBuckets: KVBucketConfig{
				StableIDBucket:   "parti-stableid",
				ElectionBucket:   "parti-election",
				HeartbeatBucket:  "parti-heartbeat",
				AssignmentBucket: "parti-assignment",
				AssignmentTTL:    0,
			},
		}

		err := cfg.Validate()
		require.NoError(t, err)
	})

	t.Run("valid custom config with conservative timings", func(t *testing.T) {
		cfg := Config{
			WorkerIDPrefix:        "worker",
			WorkerIDMin:           0,
			WorkerIDMax:           99,
			HeartbeatInterval:     5 * time.Second,
			HeartbeatTTL:          15 * time.Second, // 3x interval
			WorkerIDTTL:           60 * time.Second, // 12x interval (very safe)
			ColdStartWindow:       60 * time.Second,
			PlannedScaleWindow:    20 * time.Second,
			OperationTimeout:      10 * time.Second,
			ElectionTimeout:       5 * time.Second,
			StartupTimeout:        30 * time.Second,
			ShutdownTimeout:       10 * time.Second,
			RestartDetectionRatio: 0.5,
			Assignment: AssignmentConfig{
				MinRebalanceThreshold: 0.15,
				MinRebalanceInterval:  15 * time.Second,
			},
			KVBuckets: KVBucketConfig{
				StableIDBucket:   "parti-stableid",
				ElectionBucket:   "parti-election",
				HeartbeatBucket:  "parti-heartbeat",
				AssignmentBucket: "parti-assignment",
				AssignmentTTL:    0,
			},
		}

		err := cfg.Validate()
		require.NoError(t, err)
	})
}

func TestTestConfig(t *testing.T) {
	cfg := TestConfig()

	// Verify fast timings
	require.Equal(t, 100*time.Millisecond, cfg.Assignment.MinRebalanceInterval)
	require.Equal(t, 1*time.Second, cfg.ColdStartWindow)
	require.Equal(t, 500*time.Millisecond, cfg.PlannedScaleWindow)
	require.Equal(t, 500*time.Millisecond, cfg.HeartbeatInterval)
	require.Equal(t, 1500*time.Millisecond, cfg.HeartbeatTTL)
	require.Equal(t, 5*time.Second, cfg.WorkerIDTTL)

	// Verify it passes validation
	err := cfg.Validate()
	require.NoError(t, err)

	// Verify other defaults are preserved
	require.Equal(t, "worker", cfg.WorkerIDPrefix)
	require.Equal(t, 0, cfg.WorkerIDMin)
	require.Equal(t, 99, cfg.WorkerIDMax)
}
