package provision

// ReconcilePolicy controls how Apply handles drift. Validate rejects
// unrecognized values by string match so callers receive a clear error.
type ReconcilePolicy string

const (
	// PolicyWarn creates missing resources and reports drift, never
	// mutating existing resources. This is the default.
	PolicyWarn ReconcilePolicy = "warn"

	// PolicyAdopt stamps the Parti ownership marker on resources named
	// by config that exist live and are unmarked. Adopt creates no
	// missing resources and updates no non-marker fields.
	PolicyAdopt ReconcilePolicy = "adopt"

	// PolicySafeUpdate performs create-missing plus in-place
	// UpdateKeyValue for drift-mutable fields on Parti-marked resources.
	// Unmarked resources continue to surface as "adopted" drift and are
	// not mutated under safe-update; operators run partictl adopt first
	// to transition them.
	PolicySafeUpdate ReconcilePolicy = "safe-update"

	// PolicyForce is a strict superset of PolicySafeUpdate: it create-misses and
	// reconciles drift-mutable fields in place exactly as safe-update does, and
	// additionally repairs a drift-immutable resource by delete/recreate — but
	// only for an ownership-proven resource (a Parti-marked KV bucket or stream,
	// or a config-derived dynamic consumer) whose config sets
	// allowDeleteRecreate: true. Destructive and doubly gated.
	PolicyForce ReconcilePolicy = "force"
)

// policyReconcilesMutable reports whether p applies in-place mutation for
// drift-mutable fields (update-kv, update-stream). True for safe-update and
// force; false for warn and adopt.
func policyReconcilesMutable(p ReconcilePolicy) bool {
	return p == PolicySafeUpdate || p == PolicyForce
}

// policyRecreatesImmutable reports whether p permits destructive repair
// (delete/recreate) for drift-immutable resources. True only for force.
func policyRecreatesImmutable(p ReconcilePolicy) bool {
	return p == PolicyForce
}
