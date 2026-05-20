package provision

// ReconcilePolicy controls how Apply handles drift. Validate rejects
// reserved future values by string match so callers receive a clear
// error today rather than a generic "unrecognized" message.
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
)

// Reserved string values not yet supported. Listed here so Validate can
// reject them with a clear "not supported yet" message without forcing
// callers to type the literal strings.
const (
	reservedPolicyForce = "force"
)
