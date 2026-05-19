package provision

// ReconcilePolicy controls how Apply handles drift. v1 only supports
// PolicyWarn; PolicySafeUpdate lands in Phase 2 and PolicyForce in Phase 6,
// each with explicit field/test coverage. Validate rejects the future values
// by string match so callers receive a clear error today.
type ReconcilePolicy string

const (
	// PolicyWarn creates missing resources and reports drift, never mutating
	// existing resources. This is the v1 default.
	PolicyWarn ReconcilePolicy = "warn"
)

// Reserved string values for future phases. They are documented here so
// Validate can reject them with a clear "not supported in v1" message,
// without forcing callers to type the literal strings themselves.
const (
	reservedPolicySafeUpdate = "safe-update" // Phase 2 (Safe Update + Adopt)
	reservedPolicyForce      = "force"       // Phase 6 (Force + Repair)
)
