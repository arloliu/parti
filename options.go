package parti

import "context"

// Option configures a Manager with optional dependencies.
type Option func(*managerOptions)

// managerOptions holds optional Manager configuration.
type managerOptions struct {
	electionAgent   ElectionAgent
	hooks           *Hooks
	metrics         MetricsCollector
	logger          Logger
	consumerUpdater WorkerConsumerUpdater
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
//	mgr := parti.NewManager(&cfg, conn, src, parti.WithElectionAgent(agent))
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
//	    OnAssignmentChanged: func(ctx context.Context, added, removed []parti.Partition) error {
//	        return handleChanges(added, removed)
//	    },
//	}
//	mgr := parti.NewManager(&cfg, conn, src, parti.WithHooks(hooks))
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
//	mgr := parti.NewManager(&cfg, conn, src, parti.WithMetrics(metrics))
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
//	mgr := parti.NewManager(&cfg, conn, src, parti.WithLogger(logger))
func WithLogger(logger Logger) Option {
	return func(o *managerOptions) {
		o.logger = logger
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
//	helper, _ := subscription.NewDurableHelper(nc, subscription.DurableConfig{ /* ... */ }, handler)
//	mgr, _ := parti.NewManager(cfg, nc, src, strategy, parti.WithWorkerConsumerUpdater(helper))
func WithWorkerConsumerUpdater(updater WorkerConsumerUpdater) Option {
	return func(o *managerOptions) {
		o.consumerUpdater = updater
	}
}
