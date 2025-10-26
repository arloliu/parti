package parti

import (
	"time"
)

// AssignmentConfig controls rebalancing behavior.
type AssignmentConfig struct {
	// MinRebalanceThreshold is the minimum partition imbalance ratio (0.0-1.0) that triggers rebalancing.
	// For example, 0.15 means rebalancing occurs when the difference between max and min partition
	// counts exceeds 15% of the total partitions.
	MinRebalanceThreshold float64 `yaml:"minRebalanceThreshold"`

	// RebalanceCooldown is the minimum time to wait between rebalancing operations.
	// This prevents excessive rebalancing during worker churn.
	RebalanceCooldown time.Duration `yaml:"rebalanceCooldown"`
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
			RebalanceCooldown:     10 * time.Second,
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

// ApplyDefaults applies default values to zero-valued fields in the config.
//
// Parameters:
//   - cfg: Config to apply defaults to (modified in place)
func ApplyDefaults(cfg *Config) {
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
	if cfg.Assignment.RebalanceCooldown == 0 {
		cfg.Assignment.RebalanceCooldown = defaults.Assignment.RebalanceCooldown
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
