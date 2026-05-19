package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration structure.
type Config struct {
	Simulation  SimulationConfig  `yaml:"simulation"`
	Partitions  PartitionsConfig  `yaml:"partitions"`
	Producers   ProducersConfig   `yaml:"producers"`
	Workers     WorkersConfig     `yaml:"workers"`
	Coordinator CoordinatorConfig `yaml:"coordinator"`
	Chaos       ChaosConfig       `yaml:"chaos"`
	NATS        NATSConfig        `yaml:"nats"`
	Metrics     MetricsConfig     `yaml:"metrics"`
	Checkpoint  CheckpointConfig  `yaml:"checkpoint"`
}

// SimulationConfig configures the simulation runtime.
type SimulationConfig struct {
	Duration time.Duration `yaml:"duration"` // e.g., "12h"
	Mode     string        `yaml:"mode"`     // "all-in-one", "producer", "worker", "coordinator"
	// Cooldown stops chaos and producers this long before the end to allow healing.
	// When unset or 0, cooldown is disabled. CLI flag -cooldown overrides this value.
	Cooldown time.Duration `yaml:"cooldown"`
}

// PartitionsConfig configures partition count and distribution.
type PartitionsConfig struct {
	Count                   int           `yaml:"count"`                      // Total partitions (e.g., 1500)
	MessageRatePerPartition float64       `yaml:"message_rate_per_partition"` // Messages per second per partition
	Distribution            string        `yaml:"distribution"`               // "uniform", "exponential"
	Weights                 WeightsConfig `yaml:"weights"`
}

// WeightsConfig configures partition weight distributions.
type WeightsConfig struct {
	Exponential ExponentialWeightsConfig `yaml:"exponential"`
}

// ExponentialWeightsConfig configures exponential weight distribution.
type ExponentialWeightsConfig struct {
	ExtremePercent float64 `yaml:"extreme_percent"` // 0.05 = 5% of partitions
	ExtremeWeight  int64   `yaml:"extreme_weight"`  // Weight for extreme partitions (e.g., 100)
	NormalWeight   int64   `yaml:"normal_weight"`   // Weight for normal partitions (e.g., 1)
}

// ProducersConfig defines producer configuration.
type ProducersConfig struct {
	Count         int                 `yaml:"count"`
	RateVariation RateVariationConfig `yaml:"rate_variation"`
}

// RateVariationConfig configures rate variation over time.
type RateVariationConfig struct {
	Enabled       bool          `yaml:"enabled"`
	Pattern       string        `yaml:"pattern"`        // "global"
	MinMultiplier float64       `yaml:"min_multiplier"` // e.g., 0.5
	MaxMultiplier float64       `yaml:"max_multiplier"` // e.g., 2.0
	RampInterval  time.Duration `yaml:"ramp_interval"`  // e.g., "5m"
}

// WorkersConfig configures worker processes.
type WorkersConfig struct {
	Count              int                   `yaml:"count"`               // Number of worker processes
	AssignmentStrategy string                `yaml:"assignment_strategy"` // "WeightedConsistentHash", "ConsistentHash", "RoundRobin"
	StrategyConfig     map[string]any        `yaml:"strategy_config"`     // Strategy-specific config
	ProcessingDelay    ProcessingDelayConfig `yaml:"processing_delay"`
	// EnforceExclusiveConsumption enables the Processing Gate (owner-only ACKs)
	EnforceExclusiveConsumption bool `yaml:"enforce_exclusive_consumption"`
	// ProcessingGate contains optional tuning for the gate behavior
	ProcessingGate WorkersProcessingGateConfig `yaml:"processing_gate"`
	// ConsumerBatchSize sets the JetStream pull BatchSize for the worker durable helper.
	// Higher values increase throughput at the cost of burstiness per iterator.
	ConsumerBatchSize int `yaml:"consumer_batch_size"`
	// HandlerConcurrency sets the number of concurrent handler goroutines that process
	// messages (with ManualAck enabled). Use to scale per-worker processing parallelism.
	HandlerConcurrency int `yaml:"handler_concurrency"`
	// MaxSubjects optionally overrides the WorkerConsumer subject guardrail. If unset or 0,
	// the simulation will default this to the partition count to avoid cold-start cap errors.
	MaxSubjects int `yaml:"max_subjects"`
	// AckWait is the JetStream consumer AckWait for each worker. Must be strictly less than
	// Coordinator.GapAging — otherwise JetStream's redelivery window for a crashed worker
	// exceeds the oracle's hole-escalation window, producing false-positive gap escalations
	// under chaos. Defaults to 30s.
	AckWait time.Duration `yaml:"ack_wait"`
}

// ProcessingDelayConfig configures message processing delay.
type ProcessingDelayConfig struct {
	Min time.Duration `yaml:"min"` // e.g., "10ms"
	Max time.Duration `yaml:"max"` // e.g., "100ms"
}

// WorkersProcessingGateConfig holds simulation-level tuning for the subscription Processing Gate.
type WorkersProcessingGateConfig struct {
	// AllowedStates defines which states permit processing for the owner (e.g., ["commit", "stable"]).
	AllowedStates []string `yaml:"allowed_states"`
	// WarmupDuration enables a startup/rebalance warm-up phase (e.g., "10s").
	// During warm-up, only WarmupAllowedStates are permitted; after it elapses,
	// AllowedStates apply.
	WarmupDuration time.Duration `yaml:"warmup_duration"`
	// WarmupAllowedStates defines allowed states during the warm-up phase (e.g., ["stable"]).
	WarmupAllowedStates []string `yaml:"warmup_allowed_states"`
	// NakDelay is the base NAK delay for non-owners.
	NakDelay time.Duration `yaml:"nak_delay"`
	// NakJitter is a fractional jitter in [0.0, 1.0].
	NakJitter float64 `yaml:"nak_jitter"`
	// Debug enables verbose NAK logs in the Processing Gate
	Debug bool `yaml:"debug"`
}

// CoordinatorConfig configures the coordinator.
type CoordinatorConfig struct {
	ValidationWindow time.Duration  `yaml:"validation_window"` // e.g., "10m"
	DupTrace         DupTraceConfig `yaml:"dup_trace"`
	// GapAging defines how long a missing sequence can remain unfilled
	// before being escalated to a gap. Example: "60s".
	GapAging time.Duration `yaml:"gap_aging"`
	// SLO defines optional service-level objectives for reporting.
	SLO SLOConfig `yaml:"slo"`
	// StopOnFailure halts the simulation immediately when a gap is detected.
	StopOnFailure bool `yaml:"stop_on_failure"`
	// FailureReportPath defines where to write the JSON failure report.
	// Defaults to "failure_report.json" if empty.
	FailureReportPath string `yaml:"failure_report_path"`
	// WorkerCacheMaxPerPartition caps the per-partition seq→worker map
	// the tracker uses to classify ownership violations vs redeliveries.
	// Beyond this window, duplicates fall back to the legacy duplicate
	// counter. Default 4096 when unset; raise for very long stress runs
	// to extend the ownership-violation detection horizon.
	WorkerCacheMaxPerPartition int `yaml:"worker_cache_max_per_partition"`
}

// SLOConfig holds optional SLO thresholds for simulation reporting.
type SLOConfig struct {
	// HoleMaxAge caps the acceptable age for the oldest pending hole.
	// When > 0, the coordinator will count sampling intervals where the
	// oldest pending hole age exceeds this value and report exceedance stats
	// in the summary output.
	HoleMaxAge time.Duration `yaml:"hole_max_age"`

	// CatchUpDeadline defines the maximum acceptable duration for a returning worker
	// to heal its initial backlog (holes) after it becomes active again. When >0 and
	// EnableCatchUp is true, the coordinator will record the time from first post-absence
	// message (or assignment presence) to reaching the CatchUpPercent threshold (or 100%).
	CatchUpDeadline time.Duration `yaml:"catch_up_deadline"`

	// CatchUpPercent defines the healing target (1-100). When set (>0), recovery is
	// considered complete when this percentage of the initial hole backlog is healed.
	// When 0, all holes must be healed (100%).
	CatchUpPercent int `yaml:"catch_up_percent"`

	// AbsenceThreshold defines how long a worker must be silent (no received messages)
	// before its next activity is treated as a recovery event.
	AbsenceThreshold time.Duration `yaml:"absence_threshold"`

	// EnableCatchUp gates the catch-up SLO logic; when false, only legacy HoleMaxAge
	// sampling is performed.
	EnableCatchUp bool `yaml:"enable_catch_up"`
}

// DupTraceConfig configures duplicate tracing and snapshotting.
type DupTraceConfig struct {
	Enabled         bool          `yaml:"enabled"`
	Window          time.Duration `yaml:"window"`            // e.g., "60s"
	ThresholdPerMin float64       `yaml:"threshold_per_min"` // e.g., 5.0
	TopN            int           `yaml:"top_n"`             // e.g., 10
}

// ChaosConfig configures chaos engineering events.
type ChaosConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Events   []string `yaml:"events"`   // ["worker_crash", "worker_restart", ...]
	Interval string   `yaml:"interval"` // "10-30m" (random between 10-30 minutes)

	// MinWorkers and MaxWorkers bound worker count during chaos scale events in
	// all-in-one mode. Values <= 0 disable the bound.
	MinWorkers int `yaml:"min_workers"`
	MaxWorkers int `yaml:"max_workers"`

	// Burst mode: periodic rapid-fire events followed by quiet periods
	BurstEnabled     bool    `yaml:"burst_enabled"`     // Enable variable intensity
	BurstProbability float64 `yaml:"burst_probability"` // 0.0-1.0, default 0.2 (20%)
}

// NATSConfig configures NATS connection.
type NATSConfig struct {
	Mode      string          `yaml:"mode"` // "embedded", "external"
	URL       string          `yaml:"url"`  // "nats://localhost:4222"
	JetStream JetStreamConfig `yaml:"jetstream"`
}

// JetStreamConfig configures JetStream limits.
type JetStreamConfig struct {
	MaxMemory      string `yaml:"max_memory"`       // "10GB"
	MaxFileStorage string `yaml:"max_file_storage"` // "50GB"
}

// MetricsConfig configures metrics collection.
type MetricsConfig struct {
	Prometheus PrometheusConfig `yaml:"prometheus"`
	// PerPartition controls high-cardinality metrics to avoid excessive memory use.
	PerPartition PerPartitionMetricsConfig `yaml:"per_partition"`
}

// PrometheusConfig configures Prometheus metrics.
type PrometheusConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"` // 9090
}

// PerPartitionMetricsConfig gates high-cardinality series.
type PerPartitionMetricsConfig struct {
	// Latency enables per-partition publish→consume latency histograms.
	// When disabled, only aggregate latency is recorded.
	Latency bool `yaml:"latency"`

	// Duplicates enables per-partition duplicate counters.
	// When disabled, only aggregate duplicates are recorded.
	Duplicates bool `yaml:"duplicates"`

	// BucketCount enables hashed bucket aggregation instead of true per-partition
	// series when >0. Partitions are mapped to deterministic buckets in
	// [0, bucket_count-1] to cap cardinality. When set, per-partition series
	// are suppressed and bucket metrics are emitted instead.
	BucketCount int `yaml:"bucket_count"`
}

// CheckpointConfig configures checkpointing.
type CheckpointConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"` // e.g., "30m"
	Path     string        `yaml:"path"`     // "./checkpoints"
}

// LoadConfig loads configuration from a YAML file.
//
// Parameters:
//   - path: Path to the YAML configuration file
//
// Returns:
//   - *Config: Loaded configuration with defaults applied
//   - error: Error if file cannot be read or parsed
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	applyDefaults(&cfg)

	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &cfg, nil
}
