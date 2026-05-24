package parti

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// warnCapture is a minimal Logger that records Warn-level messages only.
// Other levels are dropped. Safe for concurrent use in case ValidateWithWarnings
// ever calls the logger from a goroutine.
type warnCapture struct {
	mu    sync.Mutex
	warns []string
}

func (w *warnCapture) Debug(string, ...any) {}
func (w *warnCapture) Info(string, ...any)  {}
func (w *warnCapture) Warn(msg string, _ ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.warns = append(w.warns, msg)
}
func (w *warnCapture) Error(string, ...any) {}
func (w *warnCapture) Fatal(string, ...any) {}

func (w *warnCapture) contains(substr string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, m := range w.warns {
		if len(m) >= len(substr) && containsSubstring(m, substr) {
			return true
		}
	}

	return false
}

func containsSubstring(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestValidateWithWarnings_StartupTimeout asserts that ValidateWithWarnings
// emits a warning when StartupTimeout is smaller than the safe minimum
// (ColdStartWindow + ElectionTimeout + 5s headroom) and stays silent otherwise.
// The guidance prevents the takeover-race hang fixed in an earlier commit from
// regressing due to misconfiguration.
func TestValidateWithWarnings_StartupTimeout(t *testing.T) {
	const warnPrefix = "StartupTimeout is shorter than"

	t.Run("warns when startup timeout is too small", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.StartupTimeout = cfg.ColdStartWindow // exactly equal — below safe minimum
		log := &warnCapture{}
		cfg.ValidateWithWarnings(log)
		require.True(t, log.contains(warnPrefix),
			"expected startup-timeout warning, got %v", log.warns)
	})

	t.Run("no warning at or above safe minimum", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.StartupTimeout = cfg.ColdStartWindow + cfg.ElectionTimeout + 5*time.Second
		log := &warnCapture{}
		cfg.ValidateWithWarnings(log)
		require.False(t, log.contains(warnPrefix),
			"did not expect startup-timeout warning, got %v", log.warns)
	})

	t.Run("no warning at defaults", func(t *testing.T) {
		// Defaults provide StartupTimeout >= ColdStartWindow + ElectionTimeout + 5s
		// headroom, so the warning must stay silent for an unmodified config.
		cfg := DefaultConfig()
		log := &warnCapture{}
		cfg.ValidateWithWarnings(log)
		require.False(t, log.contains(warnPrefix),
			"defaults should not emit the warning — StartupTimeout must cover cold-start stabilization")
	})
}

// TestValidateWithWarnings_EmergencyGracePeriodHysteresis asserts that
// ValidateWithWarnings emits a warning when EmergencyGracePeriod consumes more
// than half of HeartbeatTTL — leaving too little hysteresis budget for GC
// pauses or single stalled heartbeat publishes — and stays silent otherwise.
func TestValidateWithWarnings_EmergencyGracePeriodHysteresis(t *testing.T) {
	const warnPrefix = "EmergencyGracePeriod leaves little headroom"

	t.Run("warns when grace exceeds half of TTL", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.HeartbeatTTL = 10 * time.Second
		cfg.EmergencyGracePeriod = 7 * time.Second
		log := &warnCapture{}
		cfg.ValidateWithWarnings(log)
		require.True(t, log.contains(warnPrefix),
			"expected hysteresis warning, got %v", log.warns)
	})

	t.Run("no warning at exactly half of TTL", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.HeartbeatTTL = 10 * time.Second
		cfg.EmergencyGracePeriod = 5 * time.Second
		log := &warnCapture{}
		cfg.ValidateWithWarnings(log)
		require.False(t, log.contains(warnPrefix),
			"did not expect hysteresis warning at the boundary, got %v", log.warns)
	})

	t.Run("no warning at defaults", func(t *testing.T) {
		cfg := DefaultConfig()
		log := &warnCapture{}
		cfg.ValidateWithWarnings(log)
		require.False(t, log.contains(warnPrefix),
			"defaults must not emit the hysteresis warning — recommended ratio is roughly 1:5 (grace:TTL)")
	})
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	require.Equal(t, "worker", cfg.WorkerIDPrefix)
	require.Equal(t, 0, cfg.WorkerIDMin)
	require.Equal(t, 999, cfg.WorkerIDMax)
	require.Equal(t, 75*time.Second, cfg.WorkerIDTTL)
	require.Equal(t, 5*time.Second, cfg.HeartbeatInterval)
	require.Equal(t, 15*time.Second, cfg.HeartbeatTTL)
	require.Equal(t, 30*time.Second, cfg.ColdStartWindow)
	require.Equal(t, 10*time.Second, cfg.PlannedScaleWindow)
	require.Equal(t, 0.5, cfg.RestartDetectionRatio)
	require.Equal(t, 10*time.Second, cfg.OperationTimeout)
	require.Equal(t, 10*time.Second, cfg.ElectionTimeout)
	require.Equal(t, 60*time.Second, cfg.StartupTimeout)
	require.Equal(t, 10*time.Second, cfg.ShutdownTimeout)
	require.Equal(t, 10*time.Second, cfg.RebalanceCooldown)
}

func TestSetDefaults(t *testing.T) {
	t.Run("applies defaults to empty config", func(t *testing.T) {
		cfg := Config{}
		require.NoError(t, SetDefaults(&cfg))

		require.Equal(t, "worker", cfg.WorkerIDPrefix)
		require.Equal(t, 999, cfg.WorkerIDMax)
		require.Equal(t, 75*time.Second, cfg.WorkerIDTTL)
		require.Equal(t, 10*time.Second, cfg.RebalanceCooldown)
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
			RebalanceCooldown:     15 * time.Second,
		}
		require.NoError(t, SetDefaults(&cfg))

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
		require.Equal(t, 15*time.Second, cfg.RebalanceCooldown)
	})

	t.Run("applies partial defaults", func(t *testing.T) {
		cfg := Config{
			WorkerIDPrefix: "myworker",
			WorkerIDTTL:    45 * time.Second,
			// Leave other fields empty
		}
		require.NoError(t, SetDefaults(&cfg))

		// Custom values preserved
		require.Equal(t, "myworker", cfg.WorkerIDPrefix)
		require.Equal(t, 45*time.Second, cfg.WorkerIDTTL)
		// Defaults applied
		require.Equal(t, 999, cfg.WorkerIDMax)
		require.Equal(t, 5*time.Second, cfg.HeartbeatInterval)
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
	require.Equal(t, 12*time.Second, cfg.RebalanceCooldown)
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
	require.NoError(t, SetDefaults(&cfg))

	// Custom values preserved
	require.Equal(t, "custom", cfg.WorkerIDPrefix)
	require.Equal(t, 5*time.Second, cfg.HeartbeatInterval)

	// Defaults applied
	require.Equal(t, 999, cfg.WorkerIDMax)
	require.Equal(t, 15*time.Second, cfg.HeartbeatTTL)
	require.Equal(t, 75*time.Second, cfg.WorkerIDTTL)
	require.Equal(t, 10*time.Second, cfg.RebalanceCooldown)
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
		cfg.EmergencyGracePeriod = 500 * time.Millisecond // Ensure this passes validation

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "HeartbeatTTL")
		require.Contains(t, err.Error(), "2*HeartbeatInterval")
	})

	t.Run("WorkerIDTTL less than 3x HeartbeatInterval fails", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.HeartbeatInterval = 5 * time.Second
		cfg.WorkerIDTTL = 12 * time.Second  // Less than 3x (15s)
		cfg.HeartbeatTTL = 10 * time.Second // Ensure WorkerIDTTL >= HeartbeatTTL

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
		require.Contains(t, err.Error(), "gtefield")
	})

	t.Run("zero RebalanceCooldown fails validation", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.RebalanceCooldown = 0

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "RebalanceCooldown")
		require.Contains(t, err.Error(), "gt")
	})

	t.Run("negative RebalanceCooldown fails validation", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.RebalanceCooldown = -5 * time.Second

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "RebalanceCooldown")
		require.Contains(t, err.Error(), "gt")
	})

	t.Run("ColdStartWindow less than PlannedScaleWindow fails", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.ColdStartWindow = 5 * time.Second
		cfg.PlannedScaleWindow = 10 * time.Second

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "ColdStartWindow")
		require.Contains(t, err.Error(), "gtefield")
	})

	t.Run("RebalanceCooldown exceeds ColdStartWindow fails", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.RebalanceCooldown = 40 * time.Second
		cfg.ColdStartWindow = 30 * time.Second
		// Note: RebalanceCooldown also has ltefield=PlannedScaleWindow, which is 10s default.
		// So 40s > 10s will fail on PlannedScaleWindow first or as well.
		// But here we want to test ColdStartWindow constraint?
		// Actually, RebalanceCooldown has `ltefield=PlannedScaleWindow`.
		// It does NOT have `ltefield=ColdStartWindow` in the tags I added?
		// Let's check config.go.
		// RebalanceCooldown time.Duration `yaml:"rebalanceCooldown" default:"10s" validate:"gt=0,ltefield=PlannedScaleWindow"`
		// It does NOT have ltefield=ColdStartWindow.
		// So this test case might fail because I removed the manual check for ColdStartWindow?
		// Wait, Rule 6 in manual validation had:
		// if cfg.RebalanceCooldown > cfg.ColdStartWindow { ... }
		// I removed this manual check.
		// And I didn't add a tag for it.
		// I should add `ltefield=ColdStartWindow`?
		// But `validator` only supports one `ltefield`? No, it supports multiple tags.
		// But `ltefield` takes one field. Can I chain them? `ltefield=PlannedScaleWindow,ltefield=ColdStartWindow`?
		// I don't think so.
		// So I should probably restore the manual check for ColdStartWindow if it's important.
		// Or maybe `PlannedScaleWindow` is always <= `ColdStartWindow` (enforced by another rule), so `ltefield=PlannedScaleWindow` implies `ltefield=ColdStartWindow`.
		// Yes, `ColdStartWindow >= PlannedScaleWindow`.
		// So if `RebalanceCooldown <= PlannedScaleWindow`, then `RebalanceCooldown <= ColdStartWindow` is automatically true.
		// So the check `RebalanceCooldown <= ColdStartWindow` is redundant if `RebalanceCooldown <= PlannedScaleWindow` is enforced.
		// However, the test sets `RebalanceCooldown = 40s`, `ColdStartWindow = 30s`.
		// `PlannedScaleWindow` is default 10s.
		// So 40s > 10s. It fails on `PlannedScaleWindow`.
		// The test expects failure on `ColdStartWindow`.
		// But since the constraint is redundant, maybe I should just update the test to expect failure on `PlannedScaleWindow` or just failure.
		// Or I can remove this test case as redundant.
		// But wait, what if `PlannedScaleWindow` is large?
		// If `PlannedScaleWindow` = 50s, `ColdStartWindow` = 60s. `RebalanceCooldown` = 55s.
		// `RebalanceCooldown <= PlannedScaleWindow` (55 <= 50) -> False. Fails.
		// So `RebalanceCooldown` must be <= `PlannedScaleWindow`.
		// So it is always <= `ColdStartWindow`.
		// So the manual check was indeed redundant if the other check is present.
		// So I will update the test to expect failure, but maybe not specifically on "ColdStartWindow".
		// Actually, I'll just remove the specific error message check for "ColdStartWindow" and check for "RebalanceCooldown".

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "RebalanceCooldown")
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
			RebalanceCooldown:     5 * time.Second,
			KVBuckets: KVBucketConfig{
				StableIDBucket:   "parti-stableid",
				ElectionBucket:   "parti-election",
				HeartbeatBucket:  "parti-heartbeat",
				AssignmentBucket: "parti-assignment",
				AssignmentTTL:    0,
			},
		}

		// Apply defaults for degraded mode settings
		require.NoError(t, SetDefaults(&cfg))

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
			RebalanceCooldown:     15 * time.Second,
			KVBuckets: KVBucketConfig{
				StableIDBucket:   "parti-stableid",
				ElectionBucket:   "parti-election",
				HeartbeatBucket:  "parti-heartbeat",
				AssignmentBucket: "parti-assignment",
				AssignmentTTL:    0,
			},
		}

		// Apply defaults for degraded mode settings
		require.NoError(t, SetDefaults(&cfg))

		err := cfg.Validate()
		require.NoError(t, err)
	})
}

func TestTestConfig(t *testing.T) {
	cfg := TestConfig()

	// Verify fast timings
	require.Equal(t, 100*time.Millisecond, cfg.RebalanceCooldown)
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
	require.Equal(t, 999, cfg.WorkerIDMax)
}

func TestConfig_ValidateDegradedAlerts(t *testing.T) {
	t.Run("valid alert thresholds in ascending order", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DegradedAlert = DegradedAlertConfig{
			InfoThreshold:     30 * time.Second,
			WarnThreshold:     2 * time.Minute,
			ErrorThreshold:    5 * time.Minute,
			CriticalThreshold: 10 * time.Minute,
			AlertInterval:     1 * time.Minute,
		}

		err := cfg.Validate()
		require.NoError(t, err)
	})

	t.Run("error when warn < info", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DegradedAlert.InfoThreshold = 2 * time.Minute
		cfg.DegradedAlert.WarnThreshold = 1 * time.Minute // Less than info

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "WarnThreshold")
		require.Contains(t, err.Error(), "gtefield")
	})

	t.Run("error when error < warn", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DegradedAlert.WarnThreshold = 3 * time.Minute
		cfg.DegradedAlert.ErrorThreshold = 2 * time.Minute // Less than warn

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "ErrorThreshold")
		require.Contains(t, err.Error(), "gtefield")
	})

	t.Run("error when critical < error", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DegradedAlert.ErrorThreshold = 6 * time.Minute
		cfg.DegradedAlert.CriticalThreshold = 5 * time.Minute // Less than error

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "CriticalThreshold")
		require.Contains(t, err.Error(), "gtefield")
	})

	t.Run("error when alert interval is zero", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DegradedAlert.AlertInterval = 0

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "AlertInterval")
		require.Contains(t, err.Error(), "gt")
	})

	t.Run("error when alert interval is negative", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DegradedAlert.AlertInterval = -1 * time.Second

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "AlertInterval")
		require.Contains(t, err.Error(), "gt")
	})
}

func TestConfig_ValidateDegradedBehavior(t *testing.T) {
	t.Run("valid behavior configuration", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DegradedBehavior = DegradedBehaviorConfig{
			EnterThreshold:      10 * time.Second,
			ExitThreshold:       5 * time.Second,
			KVErrorThreshold:    5,
			KVErrorWindow:       30 * time.Second,
			RecoveryGracePeriod: 15 * time.Second,
		}

		err := cfg.Validate()
		require.NoError(t, err)
	})

	t.Run("error when enter threshold is negative", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DegradedBehavior.EnterThreshold = -1 * time.Second

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "EnterThreshold")
		require.Contains(t, err.Error(), "gte")
	})

	t.Run("error when exit threshold is negative", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DegradedBehavior.ExitThreshold = -1 * time.Second

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "ExitThreshold")
		require.Contains(t, err.Error(), "gte")
	})

	t.Run("error when recovery grace period is negative", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DegradedBehavior.RecoveryGracePeriod = -1 * time.Second

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "RecoveryGracePeriod")
		require.Contains(t, err.Error(), "gte")
	})

	t.Run("error when KV error threshold is negative", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DegradedBehavior.KVErrorThreshold = -1

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "KVErrorThreshold")
		require.Contains(t, err.Error(), "gte")
	})

	t.Run("error when KV error window is negative", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DegradedBehavior.KVErrorWindow = -1 * time.Second

		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "KVErrorWindow")
		require.Contains(t, err.Error(), "gte")
	})

	t.Run("zero values are valid", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.DegradedBehavior = DegradedBehaviorConfig{
			EnterThreshold:      0,
			ExitThreshold:       0,
			KVErrorThreshold:    0,
			KVErrorWindow:       0,
			RecoveryGracePeriod: 0,
		}

		err := cfg.Validate()
		require.NoError(t, err)
	})
}

func TestDegradedBehaviorPreset(t *testing.T) {
	t.Run("conservative preset", func(t *testing.T) {
		cfg, err := DegradedBehaviorPreset("conservative")
		require.NoError(t, err)
		require.Equal(t, 30*time.Second, cfg.EnterThreshold)
		require.Equal(t, 10*time.Second, cfg.ExitThreshold)
		require.Equal(t, 10, cfg.KVErrorThreshold)
		require.Equal(t, 20*time.Second, cfg.RecoveryGracePeriod)
	})

	t.Run("balanced preset", func(t *testing.T) {
		cfg, err := DegradedBehaviorPreset("balanced")
		require.NoError(t, err)
		require.Equal(t, 10*time.Second, cfg.EnterThreshold)
		require.Equal(t, 5*time.Second, cfg.ExitThreshold)
		require.Equal(t, 5, cfg.KVErrorThreshold)
		require.Equal(t, 15*time.Second, cfg.RecoveryGracePeriod)
	})

	t.Run("aggressive preset", func(t *testing.T) {
		cfg, err := DegradedBehaviorPreset("aggressive")
		require.NoError(t, err)
		require.Equal(t, 5*time.Second, cfg.EnterThreshold)
		require.Equal(t, 3*time.Second, cfg.ExitThreshold)
		require.Equal(t, 3, cfg.KVErrorThreshold)
		require.Equal(t, 10*time.Second, cfg.RecoveryGracePeriod)
	})

	t.Run("invalid preset returns error", func(t *testing.T) {
		_, err := DegradedBehaviorPreset("invalid")
		require.Error(t, err)
		require.ErrorIs(t, err, ErrInvalidPreset)
	})
}

func TestConfig_Validate_RejectsTinyWorkerIDTTL(t *testing.T) {
	cfg := TestConfig()
	// Scale heartbeat/emergency fields down together so the only failing rule
	// is the new WorkerIDTTL floor, not a cascading dependent constraint.
	cfg.HeartbeatInterval = 20 * time.Millisecond
	cfg.HeartbeatTTL = 100 * time.Millisecond
	cfg.EmergencyGracePeriod = 50 * time.Millisecond
	cfg.WorkerIDTTL = 200 * time.Millisecond // below 3×minRenewInterval (300ms)

	err := cfg.Validate()
	require.Error(t, err, "WorkerIDTTL below the renewal floor must be rejected")
	require.Contains(t, err.Error(), "renewal loop",
		"the error must be the new WorkerIDTTL floor rule, not a different field")
}

func TestConfig_Validate_AcceptsWorkerIDTTLAtFloor(t *testing.T) {
	cfg := TestConfig()
	cfg.HeartbeatInterval = 20 * time.Millisecond
	cfg.HeartbeatTTL = 100 * time.Millisecond
	cfg.EmergencyGracePeriod = 50 * time.Millisecond
	cfg.WorkerIDTTL = 300 * time.Millisecond // exactly the floor

	require.NoError(t, cfg.Validate(), "WorkerIDTTL at the floor must be accepted")
}

func TestConfig_ApplyStartJitter_Validation(t *testing.T) {
	cases := []struct {
		name    string
		jitter  time.Duration
		wantErr bool
	}{
		{"zero is allowed (disables jitter)", 0, false},
		{"positive within cap is allowed", 500 * time.Millisecond, false},
		{"negative is rejected", -1 * time.Millisecond, true},
		{"above 10s cap is rejected", 11 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := TestConfig()
			cfg.ApplyStartJitter = tc.jitter
			err := cfg.Validate()
			if tc.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "ApplyStartJitter")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
