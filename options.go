package parti

// Option configures a Manager with optional dependencies.
type Option func(*managerOptions)

// managerOptions holds optional Manager configuration.
type managerOptions struct {
	electionAgent ElectionAgent
	hooks         *Hooks
	metrics       MetricsCollector
	logger        Logger
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
