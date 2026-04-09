package assignment

import (
	"errors"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
)

// CalculatorAndAssignmentMetrics combines the metrics interfaces needed by the calculator
// and its sub-components (e.g., AssignmentPublisher).
//
// The calculator itself records CalculatorMetrics (rebalance durations, worker changes, etc.)
// and delegates AssignmentMetrics to the AssignmentPublisher.
type CalculatorAndAssignmentMetrics interface {
	types.CalculatorMetrics
	types.AssignmentMetrics
}

// Config holds calculator configuration.
//
// Use NewCalculatorWithConfig(cfg) to create a calculator with validated
// configuration and sensible defaults for optional fields.
//
// Required fields must be set before calling NewCalculatorWithConfig.
// Optional fields will be set to sensible defaults if zero-valued.
type Config struct {
	// Required dependencies
	AssignmentKV jetstream.KeyValue // NATS KV bucket for assignments
	HeartbeatKV  jetstream.KeyValue // NATS KV bucket for heartbeats
	Source       types.PartitionSource
	Strategy     types.AssignmentStrategy

	// Required configuration
	AssignmentPrefix string        // Key prefix for assignments (e.g., "assignment")
	HeartbeatPrefix  string        // Key prefix for heartbeats (e.g., "heartbeat")
	HeartbeatTTL     time.Duration // Heartbeat TTL for worker health detection

	// Optional configuration (with defaults)
	EmergencyGracePeriod time.Duration // Minimum time before emergency rebalance (default: 5s)
	Cooldown             time.Duration // Minimum time between rebalances (default: 10s)
	RestartRatio         float64       // Cold start detection ratio (default: 0.5)
	ColdStartWindow      time.Duration // Stabilization window for cold start (default: 30s)
	PlannedScaleWindow   time.Duration // Stabilization window for planned scale (default: 10s)

	// Optional dependencies
	Metrics       CalculatorAndAssignmentMetrics // Metrics collector (default: no-op)
	Logger        types.Logger                   // Logger (default: no-op)
	StateProvider types.StateProvider            // Manager state provider for degraded mode checks (default: nil)
	// LeaderEpoch, if set, is called before each rebalance to obtain the current
	// leader epoch (NATS KV revision of the leader key). The value is embedded in
	// every published assignment so workers can detect stale assignments from a
	// former leader. When nil, LeaderEpoch defaults to 0 in published assignments.
	LeaderEpoch func() uint64
}

// Validate checks configuration validity.
//
// Returns an error if any required field is missing or invalid.
func (c *Config) Validate() error {
	if c.AssignmentKV == nil {
		return errors.New("the AssignmentKV is required")
	}
	if c.HeartbeatKV == nil {
		return errors.New("the HeartbeatKV is required")
	}
	if c.Source == nil {
		return errors.New("the Source is required")
	}
	if c.Strategy == nil {
		return errors.New("the Strategy is required")
	}
	if c.AssignmentPrefix == "" {
		return errors.New("the AssignmentPrefix is required")
	}
	if c.HeartbeatPrefix == "" {
		return errors.New("the HeartbeatPrefix is required")
	}
	if c.HeartbeatTTL == 0 {
		return errors.New("the HeartbeatTTL is required")
	}

	return nil
}

// SetDefaults applies default values for optional fields.
//
// This method is called automatically by NewCalculatorWithConfig.
// Fields that are already set (non-zero) are not overwritten.
func (c *Config) SetDefaults() {
	if c.EmergencyGracePeriod == 0 {
		c.EmergencyGracePeriod = 5 * time.Second
	}
	if c.Cooldown == 0 {
		c.Cooldown = 10 * time.Second
	}
	if c.RestartRatio == 0 {
		c.RestartRatio = 0.5
	}
	if c.ColdStartWindow == 0 {
		c.ColdStartWindow = 30 * time.Second
	}
	if c.PlannedScaleWindow == 0 {
		c.PlannedScaleWindow = 10 * time.Second
	}
	if c.Metrics == nil {
		c.Metrics = metrics.NewNop()
	}
	if c.Logger == nil {
		c.Logger = logging.NewNop()
	}
}
