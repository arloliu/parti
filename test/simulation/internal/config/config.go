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
	Process     ProcessConfig     `yaml:"process"`
}

// ProcessConfig configures runProcessOrchestrator (Phase 7a/7b).
type ProcessConfig struct {
	// SmokeWindow is the per-worker window for the IPC smoke oracle.
	// Within this window of each worker's first observed IPC frame the
	// orchestrator must have seen one of every lifecycle kind
	// (assignment_report, start_latency, state, leader). Defaults to 30s
	// when unset. On slow startups (cold embedded NATS, race-build
	// scenarios) bumping to 60s avoids false failures because state/leader
	// ticker frames can legitimately start the budget clock before
	// assignment_report arrives.
	SmokeWindow time.Duration `yaml:"smoke_window"`
}

// SimulationConfig configures the simulation runtime.
type SimulationConfig struct {
	Duration time.Duration `yaml:"duration"` // e.g., "12h"
	Mode     string        `yaml:"mode"`     // "all-in-one", "producer", "worker", "coordinator"
	// Cooldown stops chaos and producers this long before the end to allow healing.
	// When unset or 0, cooldown is disabled. CLI flag -cooldown overrides this value.
	Cooldown time.Duration `yaml:"cooldown"`

	// BurstAfterQuiet configures Phase 8 / Gap 13 burst-after-quiet
	// KV-op rate sampling. When QuietDuration > 0, the sampler is activated.
	BurstAfterQuiet BurstAfterQuietConfig `yaml:"burst_after_quiet"`
}

// BurstAfterQuietConfig configures the Phase 8 / Gap 13 burst-after-quiet
// KV-op rate sampling windows.
type BurstAfterQuietConfig struct {
	// QuietDuration is the length of the quiet baseline window. When > 0,
	// the burst-after-quiet sampler is activated. Typically 60s.
	QuietDuration time.Duration `yaml:"quiet_duration"`
	// BurstAt is the offset from simulation start at which the burst window
	// begins. This should coincide with (or shortly after) the scale_up
	// chaos event fires. Defaults to QuietDuration if unset.
	BurstAt time.Duration `yaml:"burst_at"`
	// BurstWindow is the duration of the burst sampling window. Defaults to 30s.
	BurstWindow time.Duration `yaml:"burst_window"`
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

	// PartitionSource selects the partition source backend used by each
	// worker. Allowed values: "static" (default) or "nats_kv". The
	// "nats_kv" path exercises the production source.NatsKV
	// watcher+reconciler machinery and is the only mode that supports
	// Phase 2's watcher_stall chaos primitive and source-convergence
	// oracle.
	PartitionSource string `yaml:"partition_source"`

	// SourceBucket overrides the source KV bucket name when
	// PartitionSource == "nats_kv". Defaults to "parti-sim-source".
	SourceBucket string `yaml:"source_bucket"`

	// SourceReconcileInterval overrides the NatsKV reconciler cadence.
	// Default 5s for Phase 2 scenarios (chosen so reconcileInterval <
	// watcher_stall duration < bucketUnavailableCooldown).
	SourceReconcileInterval time.Duration `yaml:"source_reconcile_interval"`

	// WorkerIDTTL overrides parti.WorkerIDTTL for ALL workers in this
	// simulation. Required for MaxAge-expiry tests (Phase 4b) because
	// every manager sharing the StableID bucket must agree on TTL —
	// each manager reconciles the bucket's MaxAge to its own WorkerIDTTL
	// at startup, so divergent values would either be overwritten or
	// fail Start. When 0, parti.DefaultConfig().WorkerIDTTL applies.
	WorkerIDTTL time.Duration `yaml:"worker_id_ttl"`

	// PerWorker provides per-worker StableID pool overrides for Phase 4
	// tiny-pool scenarios. Keyed by sim worker ID (e.g. "worker-0").
	// Workers not listed inherit the cluster-wide defaults.
	PerWorker map[string]PerWorkerConfig `yaml:"per_worker"`
}

// PerWorkerConfig configures per-worker StableID pool overrides. Each
// non-zero field overrides the corresponding sim default with
// override-if-set semantics (worker.go:NewWorker).
type PerWorkerConfig struct {
	WorkerIDPrefix string        `yaml:"worker_id_prefix"`
	WorkerIDMin    int           `yaml:"worker_id_min"`
	WorkerIDMax    int           `yaml:"worker_id_max"`
	WorkerIDTTL    time.Duration `yaml:"worker_id_ttl"`
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

	// BucketDeleteTargetOverride, when non-empty, replaces the default
	// "parti-stableid" target for bucket_delete chaos events. Used by
	// Phase 2 composed scenarios to delete the partition-source bucket
	// (e.g. "parti-sim-source") so INV3 — source-unavailable hook fires
	// on every worker — is exercised.
	BucketDeleteTargetOverride string `yaml:"bucket_delete_target_override"`

	// DisableSourceConvergenceDriver, when true, suppresses the Phase 2
	// source-convergence driver. Scenarios that delete the source bucket
	// (composed bucket_delete on parti-sim-source) must set this to true
	// because the driver's Update calls fail against a deleted bucket and
	// its expectations cannot converge — false convergence-missing
	// failures would mask the real INV3 signal.
	DisableSourceConvergenceDriver bool `yaml:"disable_source_convergence_driver"`

	// LongDisconnectDurationOverride, when > 0, sets a fixed disconnect
	// duration for network_disconnect_long events instead of the random
	// 60-180s range. Phase 3 scenarios use 60s to stay below WorkerIDTTL
	// (75s default) and avoid spurious claimLostShutdown.
	LongDisconnectDurationOverride time.Duration `yaml:"long_disconnect_duration_override"`

	// TinyPoolTarget names the sim-side worker ID (e.g. "worker-0") that
	// the tiny-pool / MaxAge-expiry chaos primitives target. Required by
	// Phase 4a (chaos_stableid_tiny_pool) and Phase 4b
	// (chaos_stableid_maxage_expiry). The corresponding entry in
	// workers.per_worker provides the pool overrides.
	TinyPoolTarget string `yaml:"tiny_pool_target"`

	// TinyPoolRespawnAfter is the offset from kill / disconnect at which
	// the chaos dispatcher schedules a fresh stableid_tiny_pool_respawn
	// event. Must be STRICTLY greater than staleThreshold =
	// 3 * max(WorkerIDTTL/3, 100ms) plus scheduler margin; for
	// WorkerIDTTL=6s the recommended value is 7s.
	TinyPoolRespawnAfter time.Duration `yaml:"tiny_pool_respawn_after"`

	// ScheduledEvents lists chaos events to fire at fixed offsets from
	// the simulation start. Each entry executes exactly once. Used by
	// Phase 4 scenarios that must coordinate a SIGKILL + respawn pair
	// (random chaos is insufficient because the respawn must land
	// strictly after staleThreshold and BEFORE the next chaos turn).
	ScheduledEvents []ScheduledChaosEvent `yaml:"scheduled_events"`

	// Handoff carries Phase 6 / Gap 5 handoff-bucket scenario knobs.
	Handoff HandoffChaosConfig `yaml:"handoff"`

	// Storage carries Phase 8 / Gap 9 storage-assertion and
	// burst-after-quiet (Gap 13) scenario knobs.
	Storage StorageChaosConfig `yaml:"storage"`

	// HeartbeatScan carries the heartbeat scan-flatness oracle knobs
	// (heartbeat_scan_flatness scenario). When enabled a sampler snapshots
	// the leader WorkerMonitor's parti-heartbeat Keys+ListKeys scan count at
	// phase boundaries and the final gate asserts the quiet-window scan rate
	// stays at the polling floor.
	HeartbeatScan HeartbeatScanConfig `yaml:"heartbeat_scan"`

	// Faults carries startup-armed fault injection knobs. Scheduled chaos
	// events are usually preferred; this is only for faults that must be
	// active before the initial worker Start path.
	Faults FaultsConfig `yaml:"faults"`
}

// FaultsConfig configures startup-armed fault injection.
type FaultsConfig struct {
	// HandoffClaimWriteOnStart arms the handoff claims/* write fault before
	// workers are constructed. This lets startup-apply scenarios fault the
	// first two-phase claim write.
	HandoffClaimWriteOnStart bool `yaml:"handoff_claim_write_on_start"`

	// HandoffClaimWriteDuration controls how long the startup claim-write
	// fault remains armed. Defaults to 20s when omitted.
	HandoffClaimWriteDuration time.Duration `yaml:"handoff_claim_write_duration"`
}

// HandoffChaosConfig configures Phase 6 / Gap 5 handoff-bucket chaos primitives.
type HandoffChaosConfig struct {
	// PrecreateMaxAge, when > 0, pre-creates the handoff KV bucket
	// (default name "parti-handoff") with this MaxAge BEFORE any worker
	// starts. This exercises the manager's reconcileHandoffBucketMaxAge
	// healing path (manager_setup.go:reconcileHandoffBucketMaxAge): the
	// manager must clear the inherited MaxAge to 0 before Start returns.
	// A non-zero live MaxAge after every worker reaches StateStable is an
	// invariant violation (handoff_bucket_maxage_violation counter).
	PrecreateMaxAge time.Duration `yaml:"precreate_maxage"`
}

// StorageChaosConfig configures Phase 8 / Gap 9 storage-assertion scenario knobs.
type StorageChaosConfig struct {
	// SkipBucketPrecreate, when true, suppresses the simulation's
	// pre-create loop for the four manager-owned coordination buckets
	// (election, heartbeat, assignment, handoff). The manager's own
	// Start path is then the only code path that creates these buckets,
	// exercising its storage-type and TTL choices end-to-end. After at
	// least one worker reaches StateStable the simulation asserts:
	//   - StorageType(parti-election) == FileStorage
	//   - StorageType(parti-assignment) == FileStorage
	//   - StorageType(parti-handoff) == FileStorage
	// A violation increments storage_type_violation and fails the run.
	//
	// NOTE: the StableID bucket is still pre-created regardless of this
	// flag because it is NOT created by the manager (it is created by the
	// stableid claimer which does not carry a storage-type choice, so
	// asserting on it here would be a different concern).
	SkipBucketPrecreate bool `yaml:"skip_bucket_precreate"`

	// ElectionReplicasOverride, when > 0, pre-creates the election KV
	// bucket (parti-election) with this replica count AND
	// Storage=FileStorage BEFORE any worker starts. This exercises the
	// "operator-precreated" path: the manager's get-first EnsureKV opens
	// the existing bucket (preserving the operator's Replicas setting).
	// After all workers reach StateStable the simulation asserts
	// Replicas == ElectionReplicasOverride on the live stream.
	// A violation increments election_replicas_violation.
	//
	// NOTE: Replicas > 1 requires a multi-server NATS cluster. Embedded
	// NATS (single server) rejects Replicas > 1 with an error; the
	// simulation falls back to Replicas=1 in that case and documents the
	// limitation. The assertion value is adjusted to match the actual
	// accepted value, so the test still catches a regression where
	// replicas were silently dropped from 1 to 0.
	ElectionReplicasOverride int `yaml:"election_replicas_override"`

	// KvOpRateCeilingMultiplier is the multiplier applied to the
	// per-worker quiet-window KV-op rate to derive the burst-window
	// ceiling. Default 0 means "use 5.0" (clearly-broken regression
	// signal). Tune this after the first run has established a baseline.
	// A value of 1.5 is the tight bound; 5.0 is the conservative first-
	// run default.
	KvOpRateCeilingMultiplier float64 `yaml:"kv_op_rate_ceiling_multiplier"`
}

// HeartbeatScanConfig configures the heartbeat scan-flatness oracle. It guards
// the benefit of the leader WorkerMonitor's heartbeat-refresh suppression
// (internal/assignment/worker_monitor.go): in a quiet window only the hbTTL/2
// polling ticker should scan parti-heartbeat; routine heartbeat refreshes must
// be suppressed (no Keys() scan). A stuck-open suppression holiday or a
// per-watcher-event forced check reverts to a scan-per-refresh storm — a pure
// performance regression that no ownership/gap oracle can see — which this
// oracle catches as a scan count above the polling floor.
type HeartbeatScanConfig struct {
	// Enabled turns on the scan-flatness sampler and the final-gate floor
	// assertion.
	Enabled bool `yaml:"enabled"`

	// PhaseATailStart / PhaseATailEnd bound the quiet baseline window (the
	// last 30s of phase A, before any chaos). The oracle asserts the
	// heartbeat scan count over this window stays at/below FloorBudget.
	PhaseATailStart time.Duration `yaml:"phase_a_tail_start"`
	PhaseATailEnd   time.Duration `yaml:"phase_a_tail_end"`

	// PhaseCTailStart / PhaseCTailEnd bound the post-chaos quiet window (the
	// final ~30s). PhaseCTailStart MUST begin at least one full suppression
	// holiday (3×HeartbeatTTL = 45s at the 15s default) after the last chaos
	// event so a correct holiday has already closed and scans returned to the
	// floor. PhaseCTailEnd MUST land a few seconds before simulation.duration
	// so the snapshot is captured before ordered shutdown — worker heartbeat
	// DELETEs during teardown would otherwise trigger a burst of leader scans
	// and contaminate the window.
	PhaseCTailStart time.Duration `yaml:"phase_c_tail_start"`
	PhaseCTailEnd   time.Duration `yaml:"phase_c_tail_end"`

	// FloorBudget is the maximum heartbeat Keys+ListKeys scan count allowed
	// in each quiet window, summed across all workers. Derive it from the
	// WorkerMonitor polling cadence (1 scan per hbTTL/2) with CI headroom;
	// see the scenario YAML for the arithmetic.
	FloorBudget int64 `yaml:"floor_budget"`
}

// ScheduledChaosEvent is a one-shot scenario-scheduled chaos event.
type ScheduledChaosEvent struct {
	// At is the offset from the simulation start at which to fire.
	At time.Duration `yaml:"at"`
	// Event is the chaos event kind (e.g. "worker_crash",
	// "stableid_tiny_pool_respawn", "network_disconnect_long").
	Event string `yaml:"event"`
	// Params are passed verbatim to the dispatcher. Common keys:
	// "target_worker" / "target_role" (sim worker ID),
	// "duration" (time.Duration string).
	Params map[string]any `yaml:"params"`
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
