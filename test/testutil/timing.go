package testutil

import (
	"time"

	"github.com/arloliu/parti"
)

// TimingProfile encapsulates a set of tuned timing parameters for integration tests.
// Profiles centralize commonly used shortened intervals so tests remain consistent
// and easy to adjust globally without touching every file.
//
// The profile follows internal invariants:
//   - HeartbeatTTL = ttlMultiplier * HeartbeatInterval
//   - EmergencyGracePeriod <= HeartbeatTTL and > HeartbeatInterval (when graceMultiplier > 1)
//   - MinRebalanceInterval <= PlannedScaleWindow <= ColdStartWindow (for batching semantics)
//
// Only fields that differ from parti defaults should be set; ApplyTo merges with defaults.
// Use MakeFast() for aggressively shortened but safe timings; MakeBaseline() mirrors
// IntegrationTestConfig; MakeEmergencyFast() focuses on emergency detection edge cases.
type TimingProfile struct {
	HeartbeatInterval    time.Duration
	TTLMultiplier        float64 // multiplier applied to HeartbeatInterval for HeartbeatTTL
	GraceMultiplier      float64 // multiplier applied to HeartbeatInterval for EmergencyGracePeriod
	ColdStartWindow      time.Duration
	PlannedScaleWindow   time.Duration
	MinRebalanceInterval time.Duration
	ElectionTimeout      time.Duration
	StartupTimeout       time.Duration
	ShutdownTimeout      time.Duration
	WorkerIDTTL          time.Duration
}

// MakeFast returns an aggressive profile for fast leader election & stabilization tests.
func MakeFast() TimingProfile {
	return TimingProfile{
		HeartbeatInterval:    300 * time.Millisecond,
		TTLMultiplier:        10.0 / 3.0, // ~3.33x -> ~1s TTL (rounded with integer math by caller)
		GraceMultiplier:      2.0,        // Grace > interval, < ttl
		ColdStartWindow:      500 * time.Millisecond,
		PlannedScaleWindow:   300 * time.Millisecond,
		MinRebalanceInterval: 300 * time.Millisecond,
		ElectionTimeout:      1 * time.Second,
		StartupTimeout:       5 * time.Second,
		ShutdownTimeout:      2 * time.Second,
		WorkerIDTTL:          30 * time.Second,
	}
}

// MakeBaseline returns a stable baseline similar to IntegrationTestConfig for general scenarios.
func MakeBaseline() TimingProfile {
	return TimingProfile{
		HeartbeatInterval:    500 * time.Millisecond,
		TTLMultiplier:        4.0, // 2s TTL
		GraceMultiplier:      0,   // disabled (use default unless emergency semantics needed)
		ColdStartWindow:      3 * time.Second,
		PlannedScaleWindow:   2 * time.Second,
		MinRebalanceInterval: 2 * time.Second,
		ElectionTimeout:      2 * time.Second,
		StartupTimeout:       10 * time.Second,
		ShutdownTimeout:      3 * time.Second,
		WorkerIDTTL:          5 * time.Second,
	}
}

// MakeEmergencyFast returns a profile tuned for emergency hysteresis edge tests.
func MakeEmergencyFast() TimingProfile {
	return TimingProfile{
		HeartbeatInterval:    300 * time.Millisecond,
		TTLMultiplier:        3.0, // 900ms TTL
		GraceMultiplier:      1.8, // grace between interval and TTL (~540ms)
		ColdStartWindow:      1200 * time.Millisecond,
		PlannedScaleWindow:   800 * time.Millisecond,
		MinRebalanceInterval: 400 * time.Millisecond,
		ElectionTimeout:      1 * time.Second,
		StartupTimeout:       5 * time.Second,
		ShutdownTimeout:      2 * time.Second,
		WorkerIDTTL:          20 * time.Second,
	}
}

// ApplyTo applies the timing profile to an existing parti.Config, respecting defaults
// and computing derived fields. Fields with zero values or multipliers <=0 are skipped.
// Returns the mutated config pointer for chaining.
func (tp TimingProfile) ApplyTo(cfg *parti.Config) *parti.Config {
	// Base defaults
	parti.SetDefaults(cfg)

	if tp.HeartbeatInterval > 0 {
		cfg.HeartbeatInterval = tp.HeartbeatInterval
	}
	if tp.TTLMultiplier > 0 && tp.HeartbeatInterval > 0 {
		cfg.HeartbeatTTL = time.Duration(float64(tp.HeartbeatInterval) * tp.TTLMultiplier)
	}
	if tp.GraceMultiplier > 0 && tp.HeartbeatInterval > 0 {
		cfg.EmergencyGracePeriod = time.Duration(float64(tp.HeartbeatInterval) * tp.GraceMultiplier)
	}
	if tp.ColdStartWindow > 0 {
		cfg.ColdStartWindow = tp.ColdStartWindow
	}
	if tp.PlannedScaleWindow > 0 {
		cfg.PlannedScaleWindow = tp.PlannedScaleWindow
	}
	if tp.MinRebalanceInterval > 0 {
		cfg.Assignment.MinRebalanceInterval = tp.MinRebalanceInterval
	}
	if tp.ElectionTimeout > 0 {
		cfg.ElectionTimeout = tp.ElectionTimeout
	}
	if tp.StartupTimeout > 0 {
		cfg.StartupTimeout = tp.StartupTimeout
	}
	if tp.ShutdownTimeout > 0 {
		cfg.ShutdownTimeout = tp.ShutdownTimeout
	}
	if tp.WorkerIDTTL > 0 {
		cfg.WorkerIDTTL = tp.WorkerIDTTL
	}

	// Invariant adjustments / sanity:
	// Ensure PlannedScaleWindow >= MinRebalanceInterval
	if cfg.PlannedScaleWindow < cfg.Assignment.MinRebalanceInterval {
		cfg.PlannedScaleWindow = cfg.Assignment.MinRebalanceInterval
	}
	// Ensure ColdStartWindow >= PlannedScaleWindow
	if cfg.ColdStartWindow < cfg.PlannedScaleWindow {
		cfg.ColdStartWindow = cfg.PlannedScaleWindow
	}
	// Ensure GracePeriod < HeartbeatTTL and > HeartbeatInterval if set
	if cfg.EmergencyGracePeriod > 0 {
		if cfg.EmergencyGracePeriod >= cfg.HeartbeatTTL {
			cfg.EmergencyGracePeriod = cfg.HeartbeatTTL - cfg.HeartbeatInterval/4 // leave margin
		}
		if cfg.EmergencyGracePeriod <= cfg.HeartbeatInterval {
			cfg.EmergencyGracePeriod = cfg.HeartbeatInterval + cfg.HeartbeatInterval/4
		}
	}

	return cfg
}

// NewConfigFromProfile creates a new parti.Config with defaults applied then profile overrides.
func NewConfigFromProfile(tp TimingProfile) parti.Config {
	cfg := parti.Config{}
	parti.SetDefaults(&cfg)
	return *tp.ApplyTo(&cfg)
}
