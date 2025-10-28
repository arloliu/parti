package parti

import (
	"fmt"
	"time"
)

// AssignmentConfig controls rebalancing behavior.
type AssignmentConfig struct {
	// MinRebalanceThreshold is the minimum partition imbalance ratio (0.0-1.0) that triggers rebalancing.
	// For example, 0.15 means rebalancing occurs when the difference between max and min partition
	// counts exceeds 15% of the total partitions.
	MinRebalanceThreshold float64 `yaml:"minRebalanceThreshold"`

	// MinRebalanceInterval is the minimum time between rebalancing operations.
	//
	// Enforces rate limiting BEFORE stabilization windows to prevent thrashing
	// during rapid topology changes. If a rebalance was completed <MinRebalanceInterval
	// ago, new topology changes are deferred until the interval expires.
	//
	// Default: 10 seconds
	// Recommendation: Should be <= PlannedScaleWindow for proper coordination
	//
	// Note: This was renamed from MinRebalanceInterval in v0.x for semantic clarity.
	MinRebalanceInterval time.Duration `yaml:"minRebalanceInterval"`
}

// KVBucketConfig configures NATS JetStream KV bucket names and TTLs.
type KVBucketConfig struct {
	// StableIDBucket is the bucket name for stable worker ID claims.
	StableIDBucket string `yaml:"stableIdBucket"`

	// ElectionBucket is the bucket name for leader election.
	ElectionBucket string `yaml:"electionBucket"`

	// HeartbeatBucket is the bucket name for worker heartbeats.
	HeartbeatBucket string `yaml:"heartbeatBucket"`

	// AssignmentBucket is the bucket name for partition assignments.
	AssignmentBucket string `yaml:"assignmentBucket"`

	// AssignmentTTL is how long assignments remain in KV (0 = no expiration).
	// Assignments should persist across leader changes for version continuity.
	// Recommended: 0 (no TTL) or very long (e.g., 1 hour).
	AssignmentTTL time.Duration `yaml:"assignmentTtl"`
}

// ============================================================================
// Timing Configuration Model (Three-Tier System)
// ============================================================================
//
// Parti uses a three-tier timing model for predictable rebalancing behavior:
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │ TIER 1: Detection Speed - How fast we notice topology changes          │
// ├─────────────────────────────────────────────────────────────────────────┤
// │ • WatcherDebounce: 100ms (hardcoded)                                   │
// │   - Batches rapid heartbeat changes before triggering checks           │
// │ • PollingInterval: HeartbeatTTL/2 (calculated)                         │
// │   - Fallback detection if watcher fails                                │
// └─────────────────────────────────────────────────────────────────────────┘
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │ TIER 2: Stabilization - How long we wait before acting                 │
// ├─────────────────────────────────────────────────────────────────────────┤
// │ • ColdStartWindow: 30s (configurable)                                  │
// │   - Applied when all workers join from zero state                      │
// │   - Allows time for full fleet to come online                          │
// │ • PlannedScaleWindow: 10s (configurable)                               │
// │   - Applied for gradual worker additions                               │
// │   - Allows time for new workers to stabilize                           │
// │ • EmergencyWindow: 0s (immediate)                                      │
// │   - Applied when workers disappear unexpectedly                        │
// │   - No delay - immediate rebalance to restore capacity                 │
// │ • EmergencyGracePeriod: 1.5s (configurable)                            │
// │   - Minimum time worker must be missing before emergency               │
// │   - Prevents flapping from transient network issues                    │
// └─────────────────────────────────────────────────────────────────────────┘
//
// ┌─────────────────────────────────────────────────────────────────────────┐
// │ TIER 3: Rate Limiting - How often we can rebalance                     │
// ├─────────────────────────────────────────────────────────────────────────┤
// │ • MinRebalanceInterval: 10s (configurable)                             │
// │   - Enforced BEFORE stabilization windows begin                        │
// │   - Prevents thrashing during rapid successive changes                 │
// │   - If triggered <MinRebalanceInterval after last rebalance, defer     │
// └─────────────────────────────────────────────────────────────────────────┘
//
// Execution Flow Example:
//
//	T+0s:  Rebalance completes (lastRebalance = now)
//	T+5s:  Worker joins
//	       ├─ Check: 5s < 10s MinRebalanceInterval? YES
//	       └─ Action: Defer (no state change, check again later)
//	T+10s: MinRebalanceInterval expires
//	       ├─ Action: Enter Scaling state
//	       └─ Start: 10s PlannedScaleWindow (Tier 2)
//	T+20s: Stabilization complete
//	       ├─ Action: Transition to Rebalancing state
//	       └─ Action: Calculate and publish assignments
//	T+25s: Another worker joins
//	       ├─ Check: 5s < 10s MinRebalanceInterval? YES
//	       └─ Action: Defer to T+30s
//	T+30s: Rate limit expires, cycle repeats
//
// Configuration Constraints:
//   - MinRebalanceInterval <= PlannedScaleWindow (recommended)
//   - ColdStartWindow >= PlannedScaleWindow (cold start is slower)
//   - EmergencyGracePeriod <= HeartbeatTTL (detection window)
//
// ============================================================================

// Config is the configuration for the Manager.
//
// All duration fields accept standard Go duration strings like "30s", "5m", "1h".
type Config struct {
	// WorkerIDPrefix is the prefix for worker IDs (e.g., "worker" produces "worker-0", "worker-1").
	WorkerIDPrefix string `yaml:"workerIdPrefix"`

	// WorkerIDMin is the minimum stable ID number (inclusive).
	// Set to 0 for most use cases.
	WorkerIDMin int `yaml:"workerIdMin"`

	// WorkerIDMax is the maximum stable ID number (inclusive).
	// Determines the maximum number of concurrent workers: (WorkerIDMax - WorkerIDMin + 1).
	// For example, WorkerIDMin=0 and WorkerIDMax=99 allows up to 100 workers.
	WorkerIDMax int `yaml:"workerIdMax"`

	// WorkerIDTTL is how long a worker ID claim remains valid in the key-value store.
	// Must be greater than HeartbeatInterval to prevent premature expiration.
	// Recommended: 3-5x HeartbeatInterval.
	WorkerIDTTL time.Duration `yaml:"workerIdTtl"`

	// HeartbeatInterval is how often workers publish heartbeat messages.
	// Shorter intervals provide faster failure detection but increase network traffic.
	// Recommended: 2-5 seconds.
	HeartbeatInterval time.Duration `yaml:"heartbeatInterval"`

	// HeartbeatTTL is how long heartbeat messages remain valid before a worker is considered failed.
	// Must be greater than HeartbeatInterval.
	// Recommended: 3x HeartbeatInterval.
	HeartbeatTTL time.Duration `yaml:"heartbeatTtl"`

	// ColdStartWindow is the stabilization period when starting workers from zero.
	// During this window, partition assignment is delayed to allow all initial workers to join.
	// Recommended: 30 seconds.
	ColdStartWindow time.Duration `yaml:"coldStartWindow"`

	// PlannedScaleWindow is the stabilization period during rolling updates or planned scaling.
	// Shorter than ColdStartWindow to minimize disruption during controlled changes.
	// Recommended: 10 seconds.
	PlannedScaleWindow time.Duration `yaml:"plannedScaleWindow"`

	// EmergencyGracePeriod is the minimum time a worker must be missing before
	// triggering emergency rebalance. Prevents false positives from transient
	// network issues or brief connectivity loss.
	//
	// Default: 0 (auto-calculated as 1.5 * HeartbeatInterval)
	// Recommended: 1.5-2.0 * HeartbeatInterval
	// Constraint: Must be <= HeartbeatTTL
	EmergencyGracePeriod time.Duration `yaml:"emergencyGracePeriod"`

	// RestartDetectionRatio determines when a restart is classified as cold start vs planned.
	// If (failed workers / total workers) > ratio, it's treated as a cold start.
	// For example, 0.5 means if >50% of workers fail simultaneously, use ColdStartWindow.
	// Recommended: 0.5.
	RestartDetectionRatio float64 `yaml:"restartDetectionRatio"`

	// OperationTimeout is the timeout for KV operations (get, put, delete).
	// Recommended: 10 seconds.
	OperationTimeout time.Duration `yaml:"operationTimeout"`

	// ElectionTimeout is the maximum time to wait for leader election to complete.
	// Recommended: 5 seconds.
	ElectionTimeout time.Duration `yaml:"electionTimeout"`

	// StartupTimeout is the maximum time to wait for the manager to fully start.
	// Includes worker ID claiming, leader election, and initial partition assignment.
	// Recommended: 30 seconds.
	StartupTimeout time.Duration `yaml:"startupTimeout"`

	// ShutdownTimeout is the maximum time to wait for graceful shutdown.
	// Includes releasing worker ID, stopping heartbeats, and cleanup operations.
	// Recommended: 10 seconds.
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout"`

	// Assignment controls partition assignment and rebalancing behavior.
	Assignment AssignmentConfig `yaml:"assignment"`

	// KVBuckets controls NATS JetStream KV bucket configuration.
	KVBuckets KVBucketConfig `yaml:"kvBuckets"`
}

// DefaultConfig returns a Config with sensible defaults.
//
// Returns:
//   - Config: Configuration with default values
func DefaultConfig() Config {
	return Config{
		WorkerIDPrefix:        "worker",
		WorkerIDMin:           0,
		WorkerIDMax:           99,
		WorkerIDTTL:           30 * time.Second,
		HeartbeatInterval:     2 * time.Second,
		HeartbeatTTL:          6 * time.Second,
		ColdStartWindow:       30 * time.Second,
		PlannedScaleWindow:    10 * time.Second,
		RestartDetectionRatio: 0.5,
		OperationTimeout:      10 * time.Second,
		ElectionTimeout:       5 * time.Second,
		StartupTimeout:        30 * time.Second,
		ShutdownTimeout:       10 * time.Second,
		Assignment: AssignmentConfig{
			MinRebalanceThreshold: 0.15,
			MinRebalanceInterval:  10 * time.Second,
		},
		KVBuckets: KVBucketConfig{
			StableIDBucket:   "parti-stableid",
			ElectionBucket:   "parti-election",
			HeartbeatBucket:  "parti-heartbeat",
			AssignmentBucket: "parti-assignment",
			AssignmentTTL:    0, // No TTL - assignments persist for version continuity
		},
	}
}

// SetDefaults fills in missing configuration values with production defaults.
//
// Parameters:
//   - cfg: Config to apply defaults to (modified in place)
func SetDefaults(cfg *Config) {
	defaults := DefaultConfig()

	if cfg.WorkerIDPrefix == "" {
		cfg.WorkerIDPrefix = defaults.WorkerIDPrefix
	}
	if cfg.WorkerIDMax == 0 {
		cfg.WorkerIDMax = defaults.WorkerIDMax
	}
	if cfg.WorkerIDTTL == 0 {
		cfg.WorkerIDTTL = defaults.WorkerIDTTL
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = defaults.HeartbeatInterval
	}
	if cfg.HeartbeatTTL == 0 {
		cfg.HeartbeatTTL = defaults.HeartbeatTTL
	}
	if cfg.ColdStartWindow == 0 {
		cfg.ColdStartWindow = defaults.ColdStartWindow
	}
	if cfg.PlannedScaleWindow == 0 {
		cfg.PlannedScaleWindow = defaults.PlannedScaleWindow
	}
	if cfg.EmergencyGracePeriod == 0 {
		// Default: 1.5x HeartbeatInterval (allows one missed heartbeat)
		cfg.EmergencyGracePeriod = time.Duration(float64(cfg.HeartbeatInterval) * 1.5)
	}
	if cfg.RestartDetectionRatio == 0 {
		cfg.RestartDetectionRatio = defaults.RestartDetectionRatio
	}
	if cfg.OperationTimeout == 0 {
		cfg.OperationTimeout = defaults.OperationTimeout
	}
	if cfg.ElectionTimeout == 0 {
		cfg.ElectionTimeout = defaults.ElectionTimeout
	}
	if cfg.StartupTimeout == 0 {
		cfg.StartupTimeout = defaults.StartupTimeout
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = defaults.ShutdownTimeout
	}
	if cfg.Assignment.MinRebalanceThreshold == 0 {
		cfg.Assignment.MinRebalanceThreshold = defaults.Assignment.MinRebalanceThreshold
	}
	if cfg.Assignment.MinRebalanceInterval == 0 {
		cfg.Assignment.MinRebalanceInterval = defaults.Assignment.MinRebalanceInterval
	}
	if cfg.KVBuckets.StableIDBucket == "" {
		cfg.KVBuckets.StableIDBucket = defaults.KVBuckets.StableIDBucket
	}
	if cfg.KVBuckets.ElectionBucket == "" {
		cfg.KVBuckets.ElectionBucket = defaults.KVBuckets.ElectionBucket
	}
	if cfg.KVBuckets.HeartbeatBucket == "" {
		cfg.KVBuckets.HeartbeatBucket = defaults.KVBuckets.HeartbeatBucket
	}
	if cfg.KVBuckets.AssignmentBucket == "" {
		cfg.KVBuckets.AssignmentBucket = defaults.KVBuckets.AssignmentBucket
	}
	// Note: AssignmentTTL of 0 is valid (no expiration), so we don't apply default
}

// TTL Configuration Guide
// =======================
//
// This library uses three different TTLs with specific purposes and constraints:
//
// 1. WorkerIDTTL (Default: 30s)
//    Purpose: Stable worker identity lease duration in NATS KV
//    Renewal: Automatically renewed every WorkerIDTTL/3 (~10s)
//    Expiry Impact: Worker loses ID claim and must re-acquire (causes disruption)
//    Recommendation: Set to 3-5x HeartbeatInterval
//
// 2. HeartbeatTTL (Default: 6s)
//    Purpose: Worker liveness detection window
//    Renewal: Heartbeat published every HeartbeatInterval (2s)
//    Expiry Impact: Worker considered dead → Emergency rebalance triggered
//    Recommendation: Set to 3x HeartbeatInterval
//
// 3. AssignmentTTL (Default: 0 = infinite)
//    Purpose: Assignment persistence across leader changes
//    Renewal: Never (assignments persist indefinitely)
//    Expiry Impact: Lost assignment history → Version counter reset
//    Recommendation: 0 (infinite) or very long (1h+) for production
//
// Constraint Hierarchy:
//   WorkerIDTTL >= HeartbeatTTL >= 2 * HeartbeatInterval
//
// Example Valid Configurations:
//
//   // Production (default)
//   WorkerIDTTL: 30s, HeartbeatInterval: 2s, HeartbeatTTL: 6s
//
//   // Fast (testing)
//   WorkerIDTTL: 5s, HeartbeatInterval: 500ms, HeartbeatTTL: 1.5s
//
//   // Conservative (unstable network)
//   WorkerIDTTL: 60s, HeartbeatInterval: 5s, HeartbeatTTL: 15s

// Validate checks configuration constraints and returns error for invalid values.
//
// Hard Validation Rules:
//   - HeartbeatTTL >= 2 * HeartbeatInterval (allow 1 missed heartbeat)
//   - WorkerIDTTL >= 3 * HeartbeatInterval (stable ID renewal)
//   - WorkerIDTTL >= HeartbeatTTL (ID must outlive heartbeat)
//   - MinRebalanceInterval > 0 (prevent thrashing)
//   - ColdStartWindow >= PlannedScaleWindow (cold start is slower)
//   - MinRebalanceInterval <= PlannedScaleWindow (rate limit coordination)
//   - MinRebalanceInterval <= ColdStartWindow (rate limit coordination)
//   - EmergencyGracePeriod <= HeartbeatTTL (detection window)
//
// Returns:
//   - error: Validation error with clear explanation, nil if valid
func (cfg *Config) Validate() error {
	// Rule 1: HeartbeatTTL sanity
	if cfg.HeartbeatTTL < 2*cfg.HeartbeatInterval {
		return fmt.Errorf(
			"HeartbeatTTL (%v) must be >= 2*HeartbeatInterval (%v) to allow one missed heartbeat",
			cfg.HeartbeatTTL, cfg.HeartbeatInterval,
		)
	}

	// Rule 2: WorkerIDTTL vs HeartbeatInterval
	if cfg.WorkerIDTTL < 3*cfg.HeartbeatInterval {
		return fmt.Errorf(
			"WorkerIDTTL (%v) must be >= 3*HeartbeatInterval (%v) for stable ID renewal",
			cfg.WorkerIDTTL, cfg.HeartbeatInterval,
		)
	}

	// Rule 3: WorkerIDTTL vs HeartbeatTTL hierarchy
	if cfg.WorkerIDTTL < cfg.HeartbeatTTL {
		return fmt.Errorf(
			"WorkerIDTTL (%v) must be >= HeartbeatTTL (%v) to prevent ID expiry before heartbeat",
			cfg.WorkerIDTTL, cfg.HeartbeatTTL,
		)
	}

	// Rule 4: MinRebalanceInterval sanity
	if cfg.Assignment.MinRebalanceInterval <= 0 {
		return fmt.Errorf("MinRebalanceInterval must be > 0, got %v", cfg.Assignment.MinRebalanceInterval)
	}

	// Rule 5: Stabilization windows
	if cfg.ColdStartWindow < cfg.PlannedScaleWindow {
		return fmt.Errorf(
			"ColdStartWindow (%v) should be >= PlannedScaleWindow (%v)",
			cfg.ColdStartWindow, cfg.PlannedScaleWindow,
		)
	}

	// Rule 6: MinRebalanceInterval vs windows (recommended)
	if cfg.Assignment.MinRebalanceInterval > cfg.ColdStartWindow {
		return fmt.Errorf(
			"MinRebalanceInterval (%v) should not exceed ColdStartWindow (%v)",
			cfg.Assignment.MinRebalanceInterval, cfg.ColdStartWindow,
		)
	}

	if cfg.Assignment.MinRebalanceInterval > cfg.PlannedScaleWindow {
		return fmt.Errorf(
			"MinRebalanceInterval (%v) should not exceed PlannedScaleWindow (%v) for proper coordination",
			cfg.Assignment.MinRebalanceInterval, cfg.PlannedScaleWindow,
		)
	}

	// Rule 7: EmergencyGracePeriod sanity
	if cfg.EmergencyGracePeriod > cfg.HeartbeatTTL {
		return fmt.Errorf(
			"EmergencyGracePeriod (%v) must be <= HeartbeatTTL (%v)",
			cfg.EmergencyGracePeriod, cfg.HeartbeatTTL,
		)
	}

	return nil
}

// ValidateWithWarnings checks configuration and logs warnings for non-recommended values.
//
// This is called after Validate() in NewManager() to provide operator guidance.
//
// Parameters:
//   - logger: Logger instance for warning output
func (cfg *Config) ValidateWithWarnings(logger Logger) {
	// Warn if WorkerIDTTL is less than recommended 2x HeartbeatTTL
	if cfg.WorkerIDTTL < 2*cfg.HeartbeatTTL {
		logger.Warn(
			"WorkerIDTTL is below recommended minimum",
			"workerIDTTL", cfg.WorkerIDTTL,
			"heartbeatTTL", cfg.HeartbeatTTL,
			"recommended", 2*cfg.HeartbeatTTL,
		)
	}

	// Warn if MinRebalanceInterval is very short
	if cfg.Assignment.MinRebalanceInterval < 5*time.Second {
		logger.Warn(
			"MinRebalanceInterval is very short, may cause frequent rebalancing",
			"cooldown", cfg.Assignment.MinRebalanceInterval,
			"recommended", "10s or higher",
		)
	}
}

// TestConfig returns a configuration optimized for fast test execution.
//
// Test timings are 10-100x faster than production defaults to enable
// rapid iteration without sacrificing test coverage. Use DefaultConfig()
// for production deployments.
//
// Returns:
//   - Config: Configuration with fast timings for tests
//
// Example:
//
//	cfg := parti.TestConfig()
//	cfg.WorkerIDPrefix = "test-worker"
//	manager, err := parti.NewManager(nc, cfg)
func TestConfig() Config {
	cfg := DefaultConfig()

	// Fast timings for test execution (10-100x faster)
	cfg.Assignment.MinRebalanceInterval = 100 * time.Millisecond // 100x faster
	cfg.ColdStartWindow = 1 * time.Second                        // 30x faster
	cfg.PlannedScaleWindow = 500 * time.Millisecond              // 20x faster
	cfg.HeartbeatInterval = 500 * time.Millisecond               // 4x faster
	cfg.HeartbeatTTL = 1500 * time.Millisecond                   // 4x faster
	cfg.WorkerIDTTL = 5 * time.Second                            // 6x faster

	return cfg
}
