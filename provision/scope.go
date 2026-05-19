package provision

// Scope selects which resource kinds View should consider, plus an optional
// instance filter.
type Scope struct {
	ControlPlane     bool
	PartitionSource  bool
	DynamicConsumers bool

	// Instance optionally restricts results to resources whose
	// parti.io/instance metadata equals this value. Empty means "all
	// instances, including resources marked without an instance value".
	Instance string
}

// ScopeAll returns a Scope that includes every resource kind v1 understands,
// with no instance filter (inventory mode).
func ScopeAll() Scope {
	return Scope{
		ControlPlane:     true,
		PartitionSource:  true,
		DynamicConsumers: true,
		Instance:         "",
	}
}

// ScopeFromConfig returns a Scope that enables every resource kind present in
// cfg, and sets Instance from cfg.Instance.
//
// "Present" means: ControlPlane != nil, PartitionSource != nil, and
// len(DynamicConsumers) > 0 respectively.
func ScopeFromConfig(cfg Config) Scope {
	return Scope{
		ControlPlane:     cfg.ControlPlane != nil,
		PartitionSource:  cfg.PartitionSource != nil,
		DynamicConsumers: len(cfg.DynamicConsumers) > 0,
		Instance:         cfg.Instance,
	}
}
