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
}

// ProcessingDelayConfig configures message processing delay.
type ProcessingDelayConfig struct {
	Min time.Duration `yaml:"min"` // e.g., "10ms"
	Max time.Duration `yaml:"max"` // e.g., "100ms"
}

// CoordinatorConfig configures the coordinator.
type CoordinatorConfig struct {
	ValidationWindow time.Duration `yaml:"validation_window"` // e.g., "10m"
}

// ChaosConfig configures chaos engineering events.
type ChaosConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Events   []string `yaml:"events"`   // ["worker_crash", "worker_restart", ...]
	Interval string   `yaml:"interval"` // "10-30m" (random between 10-30 minutes)
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
}

// PrometheusConfig configures Prometheus metrics.
type PrometheusConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"` // 9090
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
