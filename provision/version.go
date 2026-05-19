package provision

// APIVersionV1 is the only input-config schema accepted by v1 of the
// provision SDK. YAML/JSON callers must set `apiVersion: parti.io/v1` on
// the top-level Config.
const APIVersionV1 = "parti.io/v1"

// APIVersionProvisionV1 is the schema string stamped on every JSON output
// envelope (Snapshot, Plan, Report). The "/provision/" segment distinguishes
// output schema from input config schema.
const APIVersionProvisionV1 = "parti.io/provision/v1"

// Output envelope Kind values.
const (
	KindSnapshot = "Snapshot"
	KindPlan     = "Plan"
	KindReport   = "Report"
)
