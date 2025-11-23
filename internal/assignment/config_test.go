package assignment

import (
	"testing"
	"time"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/internal/metrics"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// mockKV is a minimal mock for testing
type mockKV struct {
	jetstream.KeyValue
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *Config
		wantErr   bool
		errString string
	}{
		{
			name: "valid configuration",
			cfg: &Config{
				AssignmentKV:     &mockKV{},
				HeartbeatKV:      &mockKV{},
				Source:           source.NewStatic(nil),
				Strategy:         strategy.NewRoundRobin(),
				AssignmentPrefix: "assignment",
				HeartbeatPrefix:  "heartbeat",
				HeartbeatTTL:     3 * time.Second,
			},
			wantErr: false,
		},
		{
			name: "missing AssignmentKV",
			cfg: &Config{
				HeartbeatKV:      &mockKV{},
				Source:           source.NewStatic(nil),
				Strategy:         strategy.NewRoundRobin(),
				AssignmentPrefix: "assignment",
				HeartbeatPrefix:  "heartbeat",
				HeartbeatTTL:     3 * time.Second,
			},
			wantErr:   true,
			errString: "AssignmentKV is required",
		},
		{
			name: "missing HeartbeatKV",
			cfg: &Config{
				AssignmentKV:     &mockKV{},
				Source:           source.NewStatic(nil),
				Strategy:         strategy.NewRoundRobin(),
				AssignmentPrefix: "assignment",
				HeartbeatPrefix:  "heartbeat",
				HeartbeatTTL:     3 * time.Second,
			},
			wantErr:   true,
			errString: "HeartbeatKV is required",
		},
		{
			name: "missing Source",
			cfg: &Config{
				AssignmentKV:     &mockKV{},
				HeartbeatKV:      &mockKV{},
				Strategy:         strategy.NewRoundRobin(),
				AssignmentPrefix: "assignment",
				HeartbeatPrefix:  "heartbeat",
				HeartbeatTTL:     3 * time.Second,
			},
			wantErr:   true,
			errString: "Source is required",
		},
		{
			name: "missing Strategy",
			cfg: &Config{
				AssignmentKV:     &mockKV{},
				HeartbeatKV:      &mockKV{},
				Source:           source.NewStatic(nil),
				AssignmentPrefix: "assignment",
				HeartbeatPrefix:  "heartbeat",
				HeartbeatTTL:     3 * time.Second,
			},
			wantErr:   true,
			errString: "Strategy is required",
		},
		{
			name: "missing AssignmentPrefix",
			cfg: &Config{
				AssignmentKV:    &mockKV{},
				HeartbeatKV:     &mockKV{},
				Source:          source.NewStatic(nil),
				Strategy:        strategy.NewRoundRobin(),
				HeartbeatPrefix: "heartbeat",
				HeartbeatTTL:    3 * time.Second,
			},
			wantErr:   true,
			errString: "AssignmentPrefix is required",
		},
		{
			name: "missing HeartbeatPrefix",
			cfg: &Config{
				AssignmentKV:     &mockKV{},
				HeartbeatKV:      &mockKV{},
				Source:           source.NewStatic(nil),
				Strategy:         strategy.NewRoundRobin(),
				AssignmentPrefix: "assignment",
				HeartbeatTTL:     3 * time.Second,
			},
			wantErr:   true,
			errString: "HeartbeatPrefix is required",
		},
		{
			name: "missing HeartbeatTTL",
			cfg: &Config{
				AssignmentKV:     &mockKV{},
				HeartbeatKV:      &mockKV{},
				Source:           source.NewStatic(nil),
				Strategy:         strategy.NewRoundRobin(),
				AssignmentPrefix: "assignment",
				HeartbeatPrefix:  "heartbeat",
			},
			wantErr:   true,
			errString: "HeartbeatTTL is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errString)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestConfig_SetDefaults(t *testing.T) {
	cfg := &Config{
		AssignmentKV:     &mockKV{},
		HeartbeatKV:      &mockKV{},
		Source:           source.NewStatic(nil),
		Strategy:         strategy.NewRoundRobin(),
		AssignmentPrefix: "assignment",
		HeartbeatPrefix:  "heartbeat",
		HeartbeatTTL:     3 * time.Second,
		// Optional fields not set
	}

	cfg.SetDefaults()

	// Check all defaults were set
	require.Equal(t, 5*time.Second, cfg.EmergencyGracePeriod)
	require.Equal(t, 10*time.Second, cfg.Cooldown)
	require.Equal(t, 0.2, cfg.MinThreshold)
	require.Equal(t, 0.5, cfg.RestartRatio)
	require.Equal(t, 30*time.Second, cfg.ColdStartWindow)
	require.Equal(t, 10*time.Second, cfg.PlannedScaleWindow)
	require.NotNil(t, cfg.Metrics)
	require.NotNil(t, cfg.Logger)
}

func TestConfig_SetDefaults_PreservesExistingValues(t *testing.T) {
	customLogger := logging.NewNop()
	customMetrics := metrics.NewNop()

	cfg := &Config{
		AssignmentKV:         &mockKV{},
		HeartbeatKV:          &mockKV{},
		Source:               source.NewStatic(nil),
		Strategy:             strategy.NewRoundRobin(),
		AssignmentPrefix:     "assignment",
		HeartbeatPrefix:      "heartbeat",
		HeartbeatTTL:         3 * time.Second,
		EmergencyGracePeriod: 7 * time.Second,
		Cooldown:             15 * time.Second,
		MinThreshold:         0.3,
		RestartRatio:         0.6,
		ColdStartWindow:      45 * time.Second,
		PlannedScaleWindow:   12 * time.Second,
		Logger:               customLogger,
		Metrics:              customMetrics,
	}

	cfg.SetDefaults()

	// Check custom values were preserved
	require.Equal(t, 7*time.Second, cfg.EmergencyGracePeriod)
	require.Equal(t, 15*time.Second, cfg.Cooldown)
	require.Equal(t, 0.3, cfg.MinThreshold)
	require.Equal(t, 0.6, cfg.RestartRatio)
	require.Equal(t, 45*time.Second, cfg.ColdStartWindow)
	require.Equal(t, 12*time.Second, cfg.PlannedScaleWindow)
	require.Equal(t, customLogger, cfg.Logger)
	require.Equal(t, customMetrics, cfg.Metrics)
}

func TestNewCalculator(t *testing.T) {
	cfg := &Config{
		AssignmentKV:     &mockKV{},
		HeartbeatKV:      &mockKV{},
		Source:           source.NewStatic(nil),
		Strategy:         strategy.NewRoundRobin(),
		AssignmentPrefix: "assignment",
		HeartbeatPrefix:  "heartbeat",
		HeartbeatTTL:     3 * time.Second,
	}

	calc, err := NewCalculator(cfg)
	require.NoError(t, err)
	require.NotNil(t, calc)

	// Check required fields were set
	require.Equal(t, cfg.AssignmentKV, calc.AssignmentKV)
	require.Equal(t, cfg.HeartbeatKV, calc.HeartbeatKV)
	require.Equal(t, "assignment", calc.AssignmentPrefix)
	require.Equal(t, cfg.Source, calc.Source)
	require.Equal(t, cfg.Strategy, calc.Strategy)
	require.Equal(t, "heartbeat", calc.HeartbeatPrefix)
	require.Equal(t, 3*time.Second, calc.HeartbeatTTL)

	// Check defaults were applied
	require.Equal(t, 10*time.Second, calc.Cooldown)
	require.Equal(t, 0.2, calc.MinThreshold)
	require.Equal(t, 0.5, calc.RestartRatio)
	require.Equal(t, 30*time.Second, calc.ColdStartWindow)
	require.Equal(t, 10*time.Second, calc.PlannedScaleWindow)
	require.NotNil(t, calc.Metrics)
	require.NotNil(t, calc.Logger)

	// Check cached patterns were computed
	require.Equal(t, "heartbeat.*", calc.hbWatchPattern)
	require.Equal(t, "assignment.", calc.assignmentKeyPrefix)

	// Check emergency detector was initialized
	require.NotNil(t, calc.emergencyDetector)
}

func TestNewCalculator_ValidationError(t *testing.T) {
	cfg := &Config{
		// Missing required fields
		AssignmentPrefix: "assignment",
		HeartbeatPrefix:  "heartbeat",
		HeartbeatTTL:     3 * time.Second,
	}

	calc, err := NewCalculator(cfg)
	require.Error(t, err)
	require.Nil(t, calc)
	require.Contains(t, err.Error(), "invalid config")
}

func TestNewCalculator_CustomValues(t *testing.T) {
	customLogger := logging.NewNop()
	customMetrics := metrics.NewNop()

	cfg := &Config{
		AssignmentKV:         &mockKV{},
		HeartbeatKV:          &mockKV{},
		Source:               source.NewStatic(nil),
		Strategy:             strategy.NewRoundRobin(),
		AssignmentPrefix:     "assignment",
		HeartbeatPrefix:      "heartbeat",
		HeartbeatTTL:         3 * time.Second,
		EmergencyGracePeriod: 7 * time.Second,
		Cooldown:             15 * time.Second,
		MinThreshold:         0.3,
		RestartRatio:         0.6,
		ColdStartWindow:      45 * time.Second,
		PlannedScaleWindow:   12 * time.Second,
		Logger:               customLogger,
		Metrics:              customMetrics,
	}

	calc, err := NewCalculator(cfg)
	require.NoError(t, err)
	require.NotNil(t, calc)

	// Check custom values were used
	require.Equal(t, 15*time.Second, calc.Cooldown)
	require.Equal(t, 0.3, calc.MinThreshold)
	require.Equal(t, 0.6, calc.RestartRatio)
	require.Equal(t, 45*time.Second, calc.ColdStartWindow)
	require.Equal(t, 12*time.Second, calc.PlannedScaleWindow)
	require.Equal(t, customLogger, calc.Logger)
	require.Equal(t, customMetrics, calc.Metrics)
}
