package provision

// Marker metadata keys stamped on every Parti-managed NATS resource. They
// live in the resource's Metadata map (jetstream.KeyValueConfig.Metadata for
// KV buckets), not Description. See docs/plans/provision-sdk-cli for the
// authoritative contract.
const (
	// MarkerManagedKey holds the marker schema version, e.g. "v1". The
	// default View filter treats a non-empty value as proof that the
	// resource is Parti-managed.
	MarkerManagedKey = "parti.io/managed"

	// MarkerComponentKey categorizes the resource: one of the Component*
	// constants below.
	MarkerComponentKey = "parti.io/component"

	// MarkerInstanceKey is an optional logical environment identifier.
	// When set, View can filter inventory by this value.
	MarkerInstanceKey = "parti.io/instance"

	// MarkerManagedValue is the schema-version value v1 always writes.
	MarkerManagedValue = "v1"
)

// Component values. The provision SDK never writes any other value.
const (
	ComponentControlPlaneID         = "control-plane:id"
	ComponentControlPlaneElection   = "control-plane:election"
	ComponentControlPlaneHeartbeat  = "control-plane:heartbeat"
	ComponentControlPlaneAssignment = "control-plane:assignment"
	ComponentControlPlaneHandoff    = "control-plane:handoff"
	ComponentPartitionSource        = "partition-source"
	ComponentDynamicConsumer        = "dynamic-consumer"
	// ComponentStream marks an application JetStream stream provision manages.
	ComponentStream = "stream"
	// ComponentUnknown is reported by View/Plan when a resource carries the
	// Parti managed marker but a component value outside the enumerated set
	// above (forward-compat).
	ComponentUnknown = "unknown"
)

// MarkerInfo captures the parsed Parti marker fields on a resource.
type MarkerInfo struct {
	// Managed is the value of parti.io/managed; empty means "unmarked".
	Managed string
	// Component is the value of parti.io/component; empty if absent.
	Component string
	// Instance is the value of parti.io/instance; empty if absent.
	Instance string
}

// IsManaged reports whether the resource is marked as Parti-managed (i.e.
// parti.io/managed is set to a non-empty value).
func (m MarkerInfo) IsManaged() bool {
	return m.Managed != ""
}

// ClassifyComponent returns the component as-stamped on the resource, or
// ComponentUnknown if the marker exists but the value is not one of the
// enumerated v1 components.
func (m MarkerInfo) ClassifyComponent() string {
	if !m.IsManaged() {
		return ""
	}
	switch m.Component {
	case ComponentControlPlaneID,
		ComponentControlPlaneElection,
		ComponentControlPlaneHeartbeat,
		ComponentControlPlaneAssignment,
		ComponentControlPlaneHandoff,
		ComponentPartitionSource,
		ComponentDynamicConsumer,
		ComponentStream:
		return m.Component
	default:
		return ComponentUnknown
	}
}

// BuildMarker constructs the Metadata map that should be set on a freshly
// created Parti-managed resource. The instance arg is optional ("" means no
// instance label).
//
// The returned map always contains MarkerManagedKey and MarkerComponentKey;
// MarkerInstanceKey is added only when instance != "".
func BuildMarker(component, instance string) map[string]string {
	m := map[string]string{
		MarkerManagedKey:   MarkerManagedValue,
		MarkerComponentKey: component,
	}
	if instance != "" {
		m[MarkerInstanceKey] = instance
	}

	return m
}

// ParseMarker reads the Parti marker keys out of a resource's Metadata.
// Unknown parti.io/* keys are ignored (forward-compat). A nil or empty map
// returns the zero MarkerInfo (which IsManaged()==false).
//
// The Component field is the classified component value: for managed resources
// it is one of the Component* constants or ComponentUnknown for unrecognised
// values (forward-compat). For unmanaged resources Component is always "".
func ParseMarker(metadata map[string]string) MarkerInfo {
	if len(metadata) == 0 {
		return MarkerInfo{}
	}
	managed := metadata[MarkerManagedKey]
	var component string
	if managed != "" {
		switch metadata[MarkerComponentKey] {
		case ComponentControlPlaneID,
			ComponentControlPlaneElection,
			ComponentControlPlaneHeartbeat,
			ComponentControlPlaneAssignment,
			ComponentControlPlaneHandoff,
			ComponentPartitionSource,
			ComponentDynamicConsumer,
			ComponentStream:
			component = metadata[MarkerComponentKey]
		default:
			component = ComponentUnknown
		}
	}

	return MarkerInfo{
		Managed:   managed,
		Component: component,
		Instance:  metadata[MarkerInstanceKey],
	}
}
