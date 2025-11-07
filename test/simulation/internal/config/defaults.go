package config

import "time"

// applyDefaults applies default values to configuration fields that are not set.
//
//nolint:cyclop // Complexity acceptable for comprehensive config validation
func applyDefaults(cfg *Config) {
	// Simulation defaults
	if cfg.Simulation.Mode == "" {
		cfg.Simulation.Mode = "all-in-one"
	}
	if cfg.Simulation.Duration == 0 {
		cfg.Simulation.Duration = 1 * time.Hour
	}

	// Partitions defaults
	if cfg.Partitions.Count == 0 {
		cfg.Partitions.Count = 100
	}
	if cfg.Partitions.MessageRatePerPartition == 0 {
		cfg.Partitions.MessageRatePerPartition = 0.1 // 10 msg/sec for 100 partitions
	}
	if cfg.Partitions.Distribution == "" {
		cfg.Partitions.Distribution = "uniform"
	}

	// Exponential weights defaults
	if cfg.Partitions.Weights.Exponential.ExtremePercent == 0 {
		cfg.Partitions.Weights.Exponential.ExtremePercent = 0.05
	}
	if cfg.Partitions.Weights.Exponential.ExtremeWeight == 0 {
		cfg.Partitions.Weights.Exponential.ExtremeWeight = 100
	}
	if cfg.Partitions.Weights.Exponential.NormalWeight == 0 {
		cfg.Partitions.Weights.Exponential.NormalWeight = 1
	}

	// Producers defaults
	if cfg.Producers.Count == 0 {
		cfg.Producers.Count = 10
	}
	if cfg.Producers.RateVariation.Pattern == "" {
		cfg.Producers.RateVariation.Pattern = "global"
	}
	if cfg.Producers.RateVariation.MinMultiplier == 0 {
		cfg.Producers.RateVariation.MinMultiplier = 0.5
	}
	if cfg.Producers.RateVariation.MaxMultiplier == 0 {
		cfg.Producers.RateVariation.MaxMultiplier = 2.0
	}
	if cfg.Producers.RateVariation.RampInterval == 0 {
		cfg.Producers.RateVariation.RampInterval = 5 * time.Minute
	}

	// Workers defaults
	if cfg.Workers.Count == 0 {
		cfg.Workers.Count = 10
	}
	if cfg.Workers.AssignmentStrategy == "" {
		cfg.Workers.AssignmentStrategy = "ConsistentHash"
	}
	if cfg.Workers.ProcessingDelay.Min == 0 {
		cfg.Workers.ProcessingDelay.Min = 10 * time.Millisecond
	}
	if cfg.Workers.ProcessingDelay.Max == 0 {
		cfg.Workers.ProcessingDelay.Max = 50 * time.Millisecond
	}

	// Coordinator defaults
	if cfg.Coordinator.ValidationWindow == 0 {
		cfg.Coordinator.ValidationWindow = 5 * time.Minute
	}

	// Chaos defaults
	if cfg.Chaos.Interval == "" {
		cfg.Chaos.Interval = "10-30m"
	}

	// NATS defaults
	if cfg.NATS.Mode == "" {
		cfg.NATS.Mode = "embedded"
	}
	if cfg.NATS.URL == "" {
		cfg.NATS.URL = "nats://localhost:4222"
	}
	if cfg.NATS.JetStream.MaxMemory == "" {
		cfg.NATS.JetStream.MaxMemory = "10GB"
	}
	if cfg.NATS.JetStream.MaxFileStorage == "" {
		cfg.NATS.JetStream.MaxFileStorage = "50GB"
	}

	// Metrics defaults
	if cfg.Metrics.Prometheus.Port == 0 {
		cfg.Metrics.Prometheus.Port = 9090
	}

	// Checkpoint defaults
	if cfg.Checkpoint.Interval == 0 {
		cfg.Checkpoint.Interval = 30 * time.Minute
	}
	if cfg.Checkpoint.Path == "" {
		cfg.Checkpoint.Path = "./checkpoints"
	}
}
