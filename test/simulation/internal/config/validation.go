package config //nolint:cyclop

import (
	"errors"
	"fmt"
)

// validateConfig validates the configuration for logical consistency.
func validateConfig(cfg *Config) error { //nolint:cyclop,gocyclo
	// Validate simulation mode
	validModes := map[string]bool{
		"all-in-one":  true,
		"producer":    true,
		"worker":      true,
		"coordinator": true,
		// Phase 7a: process orchestrator — runs the coordinator and
		// spawns the configured worker count as child processes whose
		// stdout streams IPC NDJSON frames consumed by ProcessManager.
		"process": true,
	}
	if !validModes[cfg.Simulation.Mode] {
		return fmt.Errorf("invalid simulation mode: %s (must be one of: all-in-one, producer, worker, coordinator, process)", cfg.Simulation.Mode)
	}

	// Validate simulation duration
	if cfg.Simulation.Duration <= 0 {
		return errors.New("simulation duration must be positive")
	}

	// Validate partitions
	if cfg.Partitions.Count <= 0 {
		return errors.New("partition count must be positive")
	}
	if cfg.Partitions.MessageRatePerPartition < 0 {
		return errors.New("message rate per partition cannot be negative")
	}

	// Validate distribution
	validDistributions := map[string]bool{
		"uniform":     true,
		"exponential": true,
	}
	if !validDistributions[cfg.Partitions.Distribution] {
		return fmt.Errorf("invalid distribution: %s (must be one of: uniform, exponential)", cfg.Partitions.Distribution)
	}

	// Validate exponential weights if distribution is exponential
	if cfg.Partitions.Distribution == "exponential" {
		if cfg.Partitions.Weights.Exponential.ExtremePercent <= 0 || cfg.Partitions.Weights.Exponential.ExtremePercent >= 1 {
			return errors.New("extreme percent must be between 0 and 1")
		}
		if cfg.Partitions.Weights.Exponential.ExtremeWeight <= 0 {
			return errors.New("extreme weight must be positive")
		}
		if cfg.Partitions.Weights.Exponential.NormalWeight <= 0 {
			return errors.New("normal weight must be positive")
		}
	}

	// Validate producers
	if cfg.Producers.Count <= 0 {
		return errors.New("producer count must be positive")
	}

	// Validate rate variation
	if cfg.Producers.RateVariation.Enabled {
		if cfg.Producers.RateVariation.MinMultiplier <= 0 {
			return errors.New("min multiplier must be positive")
		}
		if cfg.Producers.RateVariation.MaxMultiplier <= 0 {
			return errors.New("max multiplier must be positive")
		}
		if cfg.Producers.RateVariation.MinMultiplier >= cfg.Producers.RateVariation.MaxMultiplier {
			return errors.New("min multiplier must be less than max multiplier")
		}
		if cfg.Producers.RateVariation.RampInterval <= 0 {
			return errors.New("ramp interval must be positive")
		}
	}

	// Validate workers
	if cfg.Workers.Count <= 0 {
		return errors.New("worker count must be positive")
	}
	if cfg.Workers.ConsumerBatchSize <= 0 {
		return errors.New("consumer_batch_size must be positive")
	}
	if cfg.Workers.HandlerConcurrency <= 0 {
		return errors.New("handler_concurrency must be positive")
	}
	if cfg.Workers.MaxSubjects < 0 {
		return errors.New("max_subjects cannot be negative")
	}
	validStrategies := map[string]bool{
		"ConsistentHash":         true,
		"WeightedConsistentHash": true,
		"RoundRobin":             true,
	}
	if !validStrategies[cfg.Workers.AssignmentStrategy] {
		return fmt.Errorf("invalid assignment strategy: %s (must be one of: ConsistentHash, WeightedConsistentHash, RoundRobin)",
			cfg.Workers.AssignmentStrategy)
	}
	if cfg.Workers.ProcessingDelay.Min < 0 {
		return errors.New("min processing delay cannot be negative")
	}
	if cfg.Workers.ProcessingDelay.Max < 0 {
		return errors.New("max processing delay cannot be negative")
	}
	if cfg.Workers.ProcessingDelay.Min > cfg.Workers.ProcessingDelay.Max {
		return errors.New("min processing delay must be less than or equal to max processing delay")
	}
	if cfg.Workers.AckWait <= 0 {
		return errors.New("workers.ack_wait must be positive")
	}

	// Validate Processing Gate jitter range when enabled
	if cfg.Workers.EnforceExclusiveConsumption {
		if cfg.Workers.ProcessingGate.NakJitter < 0 || cfg.Workers.ProcessingGate.NakJitter > 1 {
			return errors.New("processing_gate.nak_jitter must be between 0.0 and 1.0")
		}
	}

	// Validate coordinator
	if cfg.Coordinator.ValidationWindow <= 0 {
		return errors.New("validation window must be positive")
	}
	if cfg.Coordinator.GapAging <= 0 {
		return errors.New("gap_aging must be positive")
	}
	if cfg.Coordinator.WorkerCacheMaxPerPartition <= 0 {
		return errors.New("coordinator.worker_cache_max_per_partition must be positive")
	}
	// AckWait must be strictly less than GapAging so JetStream redelivers a
	// crashed worker's in-flight messages before the coordinator escalates
	// the hole to a confirmed gap. Equal values still race under scheduler
	// jitter.
	if cfg.Workers.AckWait >= cfg.Coordinator.GapAging {
		return fmt.Errorf("workers.ack_wait (%s) must be < coordinator.gap_aging (%s) "+
			"to avoid false-positive gap escalations during JetStream redelivery",
			cfg.Workers.AckWait, cfg.Coordinator.GapAging)
	}
	// Validate SLO (optional)
	if cfg.Coordinator.SLO.HoleMaxAge < 0 {
		return errors.New("coordinator.slo.hole_max_age cannot be negative")
	}
	if cfg.Coordinator.SLO.CatchUpDeadline < 0 {
		return errors.New("coordinator.slo.catch_up_deadline cannot be negative")
	}
	if cfg.Coordinator.SLO.CatchUpPercent < 0 || cfg.Coordinator.SLO.CatchUpPercent > 100 {
		return errors.New("coordinator.slo.catch_up_percent must be between 0 and 100")
	}
	if cfg.Coordinator.SLO.AbsenceThreshold < 0 {
		return errors.New("coordinator.slo.absence_threshold cannot be negative")
	}

	// Validate NATS mode
	validNATSModes := map[string]bool{
		"embedded": true,
		"external": true,
	}
	if !validNATSModes[cfg.NATS.Mode] {
		return fmt.Errorf("invalid NATS mode: %s (must be one of: embedded, external)", cfg.NATS.Mode)
	}

	// Validate optional chaos worker bounds
	if cfg.Chaos.MinWorkers < 0 {
		return errors.New("chaos.min_workers cannot be negative")
	}
	if cfg.Chaos.MaxWorkers < 0 {
		return errors.New("chaos.max_workers cannot be negative")
	}
	if cfg.Chaos.MinWorkers > 0 && cfg.Chaos.MaxWorkers > 0 && cfg.Chaos.MinWorkers > cfg.Chaos.MaxWorkers {
		return errors.New("chaos.min_workers must be less than or equal to chaos.max_workers")
	}

	// Validate metrics
	if cfg.Metrics.Prometheus.Enabled {
		if cfg.Metrics.Prometheus.Port <= 0 || cfg.Metrics.Prometheus.Port > 65535 {
			return fmt.Errorf("invalid Prometheus port: %d (must be 1-65535)", cfg.Metrics.Prometheus.Port)
		}
	}

	// Validate checkpoint
	if cfg.Checkpoint.Enabled {
		if cfg.Checkpoint.Interval <= 0 {
			return errors.New("checkpoint interval must be positive")
		}
		if cfg.Checkpoint.Path == "" {
			return errors.New("checkpoint path cannot be empty")
		}
	}

	return nil
}
