package assignment

import (
	"errors"
	"fmt"
	"time"

	"github.com/arloliu/parti/internal/logging"
	"github.com/arloliu/parti/internal/metrics"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/puzpuzpuz/xsync/v4"
)

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
	MinThreshold         float64       // Minimum rebalance threshold (default: 0.2)
	RestartRatio         float64       // Cold start detection ratio (default: 0.5)
	ColdStartWindow      time.Duration // Stabilization window for cold start (default: 30s)
	PlannedScaleWindow   time.Duration // Stabilization window for planned scale (default: 10s)

	// Optional dependencies
	Metrics types.MetricsCollector // Metrics collector (default: no-op)
	Logger  types.Logger           // Logger (default: no-op)
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
	if c.MinThreshold == 0 {
		c.MinThreshold = 0.2
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

// NewCalculator creates a calculator with validated configuration.
//
// This constructor provides clear, self-documenting configuration and
// validation of required fields.
//
// Parameters:
//   - cfg: Calculator configuration (required fields must be set)
//
// Returns:
//   - *Calculator: New calculator instance ready to start
//   - error: Validation error if required fields are missing
//
// Example:
//
//	calc, err := assignment.NewCalculator(&assignment.Config{
//	    AssignmentKV:     assignKV,
//	    HeartbeatKV:      heartbeatKV,
//	    Source:           source,
//	    Strategy:         strategy,
//	    AssignmentPrefix: "assignment",
//	    HeartbeatPrefix:  "heartbeat",
//	    HeartbeatTTL:     3 * time.Second,
//	    // Optional fields use sensible defaults
//	    Logger:           logger,
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
func NewCalculator(cfg *Config) (*Calculator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	cfg.SetDefaults()

	c := &Calculator{
		assignmentKV:        cfg.AssignmentKV,
		heartbeatKV:         cfg.HeartbeatKV,
		prefix:              cfg.AssignmentPrefix,
		source:              cfg.Source,
		strategy:            cfg.Strategy,
		hbPrefix:            cfg.HeartbeatPrefix,
		hbTTL:               cfg.HeartbeatTTL,
		cooldown:            cfg.Cooldown,
		minThreshold:        cfg.MinThreshold,
		restartRatio:        cfg.RestartRatio,
		coldStartWindow:     cfg.ColdStartWindow,
		plannedScaleWin:     cfg.PlannedScaleWindow,
		metrics:             cfg.Metrics,
		logger:              cfg.Logger,
		hbWatchPattern:      fmt.Sprintf("%s.*", cfg.HeartbeatPrefix),
		assignmentKeyPrefix: fmt.Sprintf("%s.", cfg.AssignmentPrefix),
		currentWorkers:      make(map[string]bool),
		currentAssignments:  make(map[string][]types.Partition),
		lastWorkers:         make(map[string]bool),
		subscribers:         xsync.NewMap[uint64, *stateSubscriber](),
		stopCh:              make(chan struct{}),
		doneCh:              make(chan struct{}),
	}

	// Initialize calculator state to Idle
	c.calcState.Store(int32(types.CalcStateIdle))

	// Initialize emergency detector with configured grace period
	c.emergencyDetector = NewEmergencyDetector(cfg.EmergencyGracePeriod)

	return c, nil
}
