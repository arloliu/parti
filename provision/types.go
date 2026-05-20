package provision

import (
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Snapshot is the read-only live state returned by View. Resource slices are
// plural so a single NATS account hosting multiple Parti environments (each
// distinguished by parti.io/instance) appears as one Snapshot.
//
// ObservedAt is emitted in JSON output only; text renderers (CLI) should
// omit it.
type Snapshot struct {
	APIVersion      string          `json:"apiVersion"`
	Kind            string          `json:"kind"`
	ObservedAt      time.Time       `json:"observedAt"`
	ControlPlane    []KVBucketState `json:"controlPlane"`
	PartitionSource []KVBucketState `json:"partitionSource"`
	// DynamicConsumers is reserved for Phase 5 (dynamic-consumer
	// precreation). In v1 it is always empty: alignment-check is the
	// only dynamic-consumer surface and it does not produce ConsumerState
	// entries because v1 does not stamp the ownership marker on
	// dynamic consumers.
	DynamicConsumers []ConsumerState `json:"dynamicConsumers"`
}

// KVBucketState is the live view of a NATS KV bucket relevant to provision.
type KVBucketState struct {
	// Bucket is the NATS KV bucket name (without the "KV_" stream prefix).
	Bucket string `json:"bucket"`
	// Component is the parti.io/component value the bucket is stamped with,
	// or "unknown" if marked but with an unrecognized component, or "" if
	// the bucket is unmarked (only possible when the caller explicitly
	// asked for unmarked inventory in a future phase — v1 View filters
	// unmarked out).
	Component string `json:"component,omitempty"`
	// Instance is the parti.io/instance value the bucket is stamped with,
	// or "" if absent.
	Instance string `json:"instance,omitempty"`
	// Managed is the raw parti.io/managed value ("" if unmarked).
	Managed string `json:"managed,omitempty"`
	// History from the live stream config.
	History uint8 `json:"history,omitempty"`
	// Storage is "file" or "memory".
	Storage string `json:"storage,omitempty"`
	// TTL is the bucket TTL; zero means "no expiration".
	TTL time.Duration `json:"ttl,omitempty"`
	// Replicas reported by NATS for the stream (informational).
	Replicas int `json:"replicas,omitempty"`
	// MaxBytes from the live config (informational).
	MaxBytes int64 `json:"maxBytes,omitempty"`
	// MaxValueSize from the live config (informational).
	MaxValueSize int32 `json:"maxValueSize,omitempty"`
}

// ConsumerState is a placeholder; populated in W4 (dynamic-consumer alignment).
type ConsumerState struct {
	StreamName string `json:"streamName,omitempty"`
	Durable    string `json:"durable,omitempty"`
	Component  string `json:"component,omitempty"`
	Instance   string `json:"instance,omitempty"`
	Managed    string `json:"managed,omitempty"`
}

// PlanResult is the deterministic list of actions Apply would take, plus any
// drift findings discovered during planning. Actions and drift are
// independently sorted by (Kind, Name).
//
// The Go type is named PlanResult (not Plan) because the package exposes a
// top-level Plan(ctx, js, cfg) constructor function; in Go the function and
// type cannot share an identifier. The JSON envelope's "kind" field is still
// the string "Plan" — operator tooling that keys on kind sees "Plan", not
// "PlanResult".
type PlanResult struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Actions    []PlannedAction `json:"actions"`
	Drift      []DriftFinding  `json:"drift"`
}

// PlannedAction is one operation Apply would perform. In v1, Kind is always
// "create-kv" (update-* lands in Phase 2; consumer-create in Phase 5;
// delete-* in Phase 6). The Resource field carries the would-be NATS config
// — for "create-kv" this is a jetstream.KeyValueConfig value.
type PlannedAction struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Resource any    `json:"resource"`
}

// Action kind constants.
const (
	ActionCreateKV = "create-kv"

	// ActionUpdateKV is emitted by Plan under PolicySafeUpdate when a
	// Parti-marked KV bucket has at least one operator-expressible field
	// (Metadata, TTL, MaxValueSize, Replicas) that differs from the
	// desired config. Apply re-reads live state, verifies it still
	// matches the plan-time Before, rebuilds the target from the re-read
	// snapshot, and calls js.UpdateKeyValue. Resource is *UpdateKVResource.
	ActionUpdateKV = "update-kv"

	// ActionStampMarker is emitted by Plan under PolicyAdopt for a KV
	// bucket named by config that exists live and carries no Parti
	// ownership marker. Apply re-reads live state, recomputes the
	// merged metadata (live keys plus the Parti marker keys),
	// short-circuits when the merge is already a no-op, otherwise
	// writes the re-read snapshot back with only Metadata changed.
	// Resource is *StampMarkerResource.
	ActionStampMarker = "stamp-marker"
)

// UpdateKVResource is the Resource carried by an ActionUpdateKV
// PlannedAction. Before is the live KeyValueConfig observed at plan
// time; After is the desired target. Both are deep clones (see
// cloneKVConfig) so the Plan output is immutable regardless of later
// Apply or nats.go mutation.
//
// Apply does not write Resource.After verbatim — it re-reads live
// state and rebuilds the target from the re-read snapshot (see the
// Apply algorithm). Resource.Before / Resource.After are the audit
// surface: JSON consumers diff them to render exactly which fields
// change.
type UpdateKVResource struct {
	Before jetstream.KeyValueConfig `json:"before"`
	After  jetstream.KeyValueConfig `json:"after"`
}

// StampMarkerResource is the Resource carried by an ActionStampMarker
// PlannedAction. MergedMetadata is the full Metadata map the action
// will write: the union of the live bucket's existing keys and the
// Parti marker keys (parti.io/managed, parti.io/component, and
// parti.io/instance when the instance is non-empty).
//
// PartiKeys lists exactly the metadata keys the action adds or
// changes relative to the live bucket, so operator review can verify
// that no non-Parti key is being modified.
//
// Apply does not write MergedMetadata verbatim — it re-reads live
// state and recomputes the merge against the re-read metadata (see
// the Apply algorithm). MergedMetadata / PartiKeys are the audit
// surface for plan / apply -dry-run output.
type StampMarkerResource struct {
	Bucket         string            `json:"bucket"`
	MergedMetadata map[string]string `json:"mergedMetadata"`
	PartiKeys      []string          `json:"partiKeys"`
}

// DriftFinding describes how a live resource differs from the desired state,
// or that an existing resource is unmarked ("adopted"). v1 emits drift
// findings but never emits update-* / delete-* actions.
//
// Severity values:
//   - "informational": resource exists and matches; no action needed.
//   - "drift-mutable": fields differ but would be safe to live-edit (v2).
//   - "drift-immutable": fields differ and require delete/recreate (v6).
//   - "adopted": resource exists without the Parti marker; reported only.
type DriftFinding struct {
	Severity string         `json:"severity"`
	Kind     string         `json:"kind"`
	Name     string         `json:"name"`
	Detail   map[string]any `json:"detail,omitempty"`
}

// Drift severity constants.
const (
	SeverityInformational  = "informational"
	SeverityDriftMutable   = "drift-mutable"
	SeverityDriftImmutable = "drift-immutable"
	SeverityAdopted        = "adopted"
)

// Drift kind constants.
const (
	KindControlPlaneKV  = "control-plane-kv"
	KindPartitionSource = "partition-source-kv"
	KindDynamicConsumer = "dynamic-consumer"
)

// Report is the result of Apply: what executed, what was skipped, what failed.
// v1 never emits Apply, but the type is part of the load-bearing W1 surface
// so later phases inherit it verbatim.
type Report struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	Executed   []ExecutedAction `json:"executed"`
	Skipped    []SkippedAction  `json:"skipped"`
	Errors     []ResourceError  `json:"errors"`
	// Aborted is true if the caller's ctx was cancelled mid-apply. It is
	// false for ordinary resource-level errors (those go in Errors).
	Aborted bool `json:"aborted"`
}

// ExecutedAction is a PlannedAction that Apply completed successfully.
//
// Raced is true when the desired resource already existed at the moment
// Apply called js.CreateKeyValue (a Plan→Apply race). The outcome is still
// "the desired resource exists now," so Apply records the action as
// Executed rather than as a resource-level error. Operator-visible drift
// for racing creators that did not stamp the Parti marker surfaces on the
// next `plan` invocation (as `adopted` drift).
type ExecutedAction struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Raced bool   `json:"raced,omitempty"`
}

// SkippedAction is a PlannedAction Apply did not run.
//
// Reason values (W2+):
//   - "context-cancelled": ctx was cancelled before this action started.
//   - "prior-error": an earlier action errored; Apply is fail-fast.
type SkippedAction struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Skip reason constants.
const (
	SkipReasonContextCancelled = "context-cancelled"
	SkipReasonPriorError       = "prior-error"
)

// ResourceError records a single resource-level failure during Apply. The
// underlying NATS error is wrapped in the returned error of Apply itself.
type ResourceError struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Error string `json:"error"`
}

// PlannedConsumer is the W4 dynamic-consumer alignment output type. The
// struct lives here so the Plan surface is stable across phases; v1 never
// populates dynamic-consumer entries (alignment-check ships in W4).
type PlannedConsumer struct {
	StreamName string                   `json:"streamName"`
	Subject    string                   `json:"subject"`
	Durable    string                   `json:"durable"`
	Config     jetstream.ConsumerConfig `json:"config,omitempty"`
}
