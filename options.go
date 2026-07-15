package parti

import (
	"context"
	"time"
)

// Option configures a Manager with optional dependencies.
type Option func(*managerOptions)

// managerOptions holds optional Manager configuration.
type managerOptions struct {
	electionAgent   ElectionAgent
	hooks           *Hooks
	metrics         MetricsCollector
	logger          Logger
	consumerUpdater WorkerConsumerUpdater
	handoffMetrics  HandoffMetricsRecorder

	// workerLabels carries the raw label set supplied via WithWorkerLabels;
	// workerLabelsSet distinguishes "option provided" from "unset" so the
	// option can override Config.WorkerLabels. Normalized (and validated) in
	// NewManager, because Option cannot return an error.
	workerLabels    []string
	workerLabelsSet bool

	// labelSpillGrace carries the value supplied via WithLabelSpillGrace. A nil
	// pointer means "unset" (use Config.LabelSpillGrace / the 60s default); a
	// non-nil pointer overrides it, including an explicit 0 that the config
	// field cannot express. Validated in NewManager, because Option cannot
	// return an error.
	labelSpillGrace *time.Duration

	// bucketEpochProbeInterval carries the value supplied via
	// WithBucketEpochProbeInterval; bucketEpochProbeIntervalSet distinguishes
	// "option provided" from "unset" so the option can override
	// Config.BucketEpochProbeInterval (already defaulted by SetDefaults).
	// Unlike labelSpillGrace, 0 has no distinct meaning for this field
	// (validated gt=0, matching OperationTimeout), so a bool flag is
	// sufficient — no pointer indirection is needed to preserve an
	// "explicit 0" case. Validated in NewManager, because Option cannot
	// return an error.
	bucketEpochProbeInterval    time.Duration
	bucketEpochProbeIntervalSet bool
}

// WithElectionAgent sets a custom election agent.
//
// Parameters:
//   - agent: ElectionAgent implementation
//
// Returns:
//   - Option: Functional option for NewManager
//
// Example:
//
//	agent := myElectionAgent
//	js, _ := jetstream.New(conn)
//	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(), parti.WithElectionAgent(agent))
//	if err != nil { /* handle */ }
func WithElectionAgent(agent ElectionAgent) Option {
	return func(o *managerOptions) {
		o.electionAgent = agent
	}
}

// WithHooks sets lifecycle event hooks.
//
// Parameters:
//   - hooks: Hooks structure with callback functions
//
// Returns:
//   - Option: Functional option for NewManager
//
// Example:
//
//	hooks := &parti.Hooks{
//	    OnAssignmentChanged: func(ctx context.Context, old, new []parti.Partition) error {
//	        // derive added/removed by diffing old vs new if needed
//	        return nil
//	    },
//	}
//	js, _ := jetstream.New(conn)
//	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(), parti.WithHooks(hooks))
//	if err != nil { /* handle */ }
func WithHooks(hooks *Hooks) Option {
	return func(o *managerOptions) {
		o.hooks = hooks
	}
}

// WithMetrics sets a metrics collector.
//
// Parameters:
//   - metrics: MetricsCollector implementation
//
// Returns:
//   - Option: Functional option for NewManager
//
// Example:
//
//	metrics := myPrometheusCollector
//	js, _ := jetstream.New(conn)
//	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(), parti.WithMetrics(metrics))
//	if err != nil { /* handle */ }
func WithMetrics(metrics MetricsCollector) Option {
	return func(o *managerOptions) {
		o.metrics = metrics
	}
}

// WithLogger sets a logger.
//
// Parameters:
//   - logger: Logger implementation (compatible with zap.SugaredLogger)
//
// Returns:
//   - Option: Functional option for NewManager
//
// Example:
//
//	logger := zap.NewExample().Sugar()
//	js, _ := jetstream.New(conn)
//	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewConsistentHash(), parti.WithLogger(logger))
//	if err != nil { /* handle */ }
func WithLogger(logger Logger) Option {
	return func(o *managerOptions) {
		o.logger = logger
	}
}

// WithWorkerLabels sets this worker's label set, overriding
// Config.WorkerLabels. Labels are fixed for the manager's lifetime,
// published in every heartbeat, and drive label-based partition
// assignment. Invalid labels cause NewManager to return an error.
//
// Use this instead of Config.WorkerLabels when several workers share one
// Config value (e.g. test clusters) but need distinct labels.
//
// Parameters:
//   - labels: Worker label set (validated, sorted, and deduplicated by NewManager)
//
// Returns:
//   - Option: Functional option for NewManager
func WithWorkerLabels(labels ...string) Option {
	return func(o *managerOptions) {
		o.workerLabels = labels
		o.workerLabelsSet = true
	}
}

// WithLabelSpillGrace sets how long a label's worker pool must be continuously
// empty before its partitions spill to unlabeled fallback workers, overriding
// Config.LabelSpillGrace.
//
// Use this to reach an immediate spill (d == 0): Config.LabelSpillGrace is a
// non-pointer duration with a 60s default, so an explicit 0 in the config is
// silently re-defaulted to 60s and cannot be configured. This option preserves
// the difference between "unset" and "0", so WithLabelSpillGrace(0) yields a
// grace of 0 (spill on the first rebalance that finds the pool empty).
//
// The option wins over Config.LabelSpillGrace, mirroring the WithWorkerLabels
// vs Config.WorkerLabels precedence. A negative duration causes NewManager to
// return an error wrapping types.ErrInvalidConfig, matching the config field's
// gte=0 rule.
//
// Parameters:
//   - d: Spill grace duration (>= 0; 0 means immediate spill)
//
// Returns:
//   - Option: Functional option for NewManager
func WithLabelSpillGrace(d time.Duration) Option {
	return func(o *managerOptions) {
		o.labelSpillGrace = &d
	}
}

// WithBucketEpochProbeInterval sets how often the bucket-epoch fence probes
// each Parti-owned KV bucket for a wipe-and-recreate event, overriding
// Config.BucketEpochProbeInterval — see that field's doc for the full
// cadence-vs-deadline rationale (OperationTimeout still bounds each
// individual probe).
//
// A non-positive duration causes NewManager to return an error wrapping
// types.ErrInvalidConfig, matching the config field's gt=0 rule.
//
// Parameters:
//   - d: Probe interval (> 0)
//
// Returns:
//   - Option: Functional option for NewManager
func WithBucketEpochProbeInterval(d time.Duration) Option {
	return func(o *managerOptions) {
		o.bucketEpochProbeInterval = d
		o.bucketEpochProbeIntervalSet = true
	}
}

// WithHandoffMetricsRecorder sets a specialized metrics recorder for the internal
// two-phase handoff coordinator.
//
// Intended primarily for tests to assert CAS conflicts, phase timings, and
// sweeper behavior. In production, leave unset to use the default no-op or a
// future global wiring.
//
// Parameters:
//   - mr: HandoffMetricsRecorder implementation
//
// Returns:
//   - Option: Functional option for NewManager
func WithHandoffMetricsRecorder(mr HandoffMetricsRecorder) Option {
	return func(o *managerOptions) {
		o.handoffMetrics = mr
	}
}

// WorkerConsumerUpdater applies partition assignments to a worker-level durable JetStream consumer.
//
// Semantics:
//   - Single durable consumer per worker (named <ConsumerPrefix>-<workerID>)
//   - Complete partition set provided each call (NOT a delta)
//   - Must be idempotent: identical subject set re-applied => no change
//   - SHOULD implement internal retries/backoff for transient JetStream errors
//   - MUST return error only for unrecoverable misconfiguration (e.g., invalid stream)
//
// Concurrency: Implementations SHOULD be safe for concurrent calls.
type WorkerConsumerUpdater interface {
	// UpdateWorkerConsumer applies the given partition assignment to the worker's durable consumer.
	//
	// See interface documentation for semantics and concurrency guarantees.
	//
	// Parameters:
	//   - ctx: Context for cancellation and deadline
	//   - workerID: Stable worker ID claimed by Manager
	//   - partitions: Complete assignment slice (may be empty for zero subjects)
	//
	// Returns:
	//   - error: Non-nil only on unrecoverable configuration or API failure after retries
	UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []Partition) error
}

// WithWorkerConsumerUpdater injects a WorkerConsumerUpdater used by Manager to apply the
// worker's current assignment to a single durable JetStream consumer.
//
// Invocation Points:
//   - Immediately after initial assignment (async, best-effort)
//   - After each subsequent assignment change
//
// This option enables fully manager-driven consumer reconciliation; hooks.OnAssignmentChanged
// can then be reserved for metrics or side effects instead of subscription wiring.
//
// Parameters:
//   - updater: Implementation that maps assignments to consumer FilterSubjects
//
// Returns:
//   - Option: Functional option for NewManager
//
// Example:
//
//	js, _ := jetstream.New(nc)
//	helper, _ := durable.NewWorkerConsumer(js, durable.WorkerConsumerConfig{ /* ... */ }, handler)
//	mgr, err := parti.NewManager(cfg, js, src, strategy, parti.WithWorkerConsumerUpdater(helper))
//	if err != nil { /* handle */ }
func WithWorkerConsumerUpdater(updater WorkerConsumerUpdater) Option {
	return func(o *managerOptions) {
		o.consumerUpdater = updater
	}
}
