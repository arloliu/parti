package provision

import (
	"time"

	"github.com/arloliu/parti/v2/types"
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
	// DynamicConsumers is always empty in the current release: alignment-check
	// is the only dynamic-consumer surface, and it does not produce
	// ConsumerState entries because the SDK does not yet stamp the ownership
	// marker on dynamic consumers. Dynamic consumer precreation is not yet
	// supported.
	DynamicConsumers []ConsumerState `json:"dynamicConsumers"`
	// Streams holds the live view of every Parti-marked application JetStream
	// stream visible to the account, filtered by Scope. Always non-nil (empty
	// slice when none are present) so the JSON envelope shape is stable.
	Streams []StreamState `json:"streams"`
}

// StreamState is the live view of a Parti-marked application JetStream stream
// returned by View. Fields mirror the operator-expressible subset of
// jetstream.StreamConfig; unlabelled or non-stream-component resources are
// excluded by View.
type StreamState struct {
	// Stream is the NATS stream name (the resource identity; no prefix).
	Stream string `json:"stream"`
	// Component is the parti.io/component value; always "stream" for a
	// StreamState entry.
	Component string `json:"component,omitempty"`
	// Instance is the parti.io/instance value; empty if absent.
	Instance string `json:"instance,omitempty"`
	// Managed is the raw parti.io/managed value.
	Managed string `json:"managed,omitempty"`
	// Subjects is the list of subject patterns the stream captures.
	Subjects []string `json:"subjects,omitempty"`
	// Retention is "limits", "workqueue", or "interest".
	Retention string `json:"retention,omitempty"`
	// Storage is "file" or "memory".
	Storage string `json:"storage,omitempty"`
	// Discard is "old" or "new".
	Discard string `json:"discard,omitempty"`
	// Replicas is the number of stream replicas reported by NATS.
	Replicas int `json:"replicas,omitempty"`
	// MaxAge is the maximum message age; zero means unlimited.
	MaxAge time.Duration `json:"maxAge,omitempty"`
	// MaxBytes is the maximum stream size in bytes; zero means unlimited
	// (live -1 is normalised to 0).
	MaxBytes int64 `json:"maxBytes,omitempty"`
	// MaxMsgs is the maximum number of messages; zero means unlimited
	// (live -1 is normalised to 0).
	MaxMsgs int64 `json:"maxMsgs,omitempty"`
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

// ConsumerState is a placeholder. Dynamic-consumer alignment is not yet
// populated in the current release; the struct exists so the Snapshot shape
// is stable for future use.
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

// PlannedAction is one operation Apply would perform. Kind is one of the
// ActionCreateKV, ActionUpdateKV, ActionStampMarker, ActionWritePartitions,
// ActionCreateStream, ActionUpdateStream, or ActionStampStreamMarker constants.
// The Resource field carries the would-be NATS config:
//   - "create-kv": jetstream.KeyValueConfig value
//   - "update-kv": *UpdateKVResource
//   - "stamp-marker": *StampMarkerResource
//   - "write-partitions": *WritePartitionsResource
//   - "create-stream": jetstream.StreamConfig value
//   - "update-stream": *UpdateStreamResource
//   - "stamp-stream-marker": *StreamStampMarkerResource
//
// Consumer precreation and destructive repair (delete/recreate) are not yet
// supported.
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

	// ActionWritePartitions is emitted by PlanPartitions when the declared
	// partition set differs from the live contents of the partition-source
	// KV key. There is at most one per plan: the key holds the whole
	// partition table, so apply is a single atomic CAS write of the full
	// array. ApplyPartitions re-reads the key, re-validates the target,
	// short-circuits a no-op, and CAS-writes. Resource is
	// *WritePartitionsResource.
	ActionWritePartitions = "write-partitions"

	// ActionCreateStream is emitted by Plan under PolicyWarn or PolicySafeUpdate
	// when a stream named by config does not exist live. Apply calls
	// js.CreateStream. Resource is a jetstream.StreamConfig value (the
	// desired config, including the Parti ownership marker).
	ActionCreateStream = "create-stream"

	// ActionUpdateStream is emitted by Plan under PolicySafeUpdate when a
	// Parti-marked stream has at least one operator-expressible mutable field
	// (Subjects, Discard, Replicas, MaxAge, MaxBytes, MaxMsgs, Description,
	// Metadata) that differs from the desired config. Apply re-reads live state,
	// verifies it still matches the plan-time Before, rebuilds the target from
	// the re-read snapshot, and calls js.UpdateStream. Resource is
	// *UpdateStreamResource.
	ActionUpdateStream = "update-stream"

	// ActionStampStreamMarker is emitted by Plan under PolicyAdopt for an
	// application stream named by config that exists live and carries no Parti
	// ownership marker. Apply re-reads live state, recomputes the merged
	// metadata (live keys plus the Parti marker keys), short-circuits when the
	// merge is already a no-op, otherwise writes the re-read snapshot back with
	// only Metadata changed. Resource is *StreamStampMarkerResource.
	ActionStampStreamMarker = "stamp-stream-marker"
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

// WritePartitionsResource is the Resource carried by an ActionWritePartitions
// PlannedAction. Before is the live partition table observed at plan time;
// After is the full declared table that Apply writes. Added, Removed, and
// Changed are the record-level diff (each sorted by CanonicalID) — the audit
// surface JSON consumers render to show exactly which records change.
//
// Apply does not blindly write After: it re-reads the live key, re-validates
// the target, and CAS-writes. Before is what the stale-before check compares
// the re-read against. All slices are deep clones, so the Plan output is
// immutable regardless of later Apply mutation.
type WritePartitionsResource struct {
	Bucket  string                  `json:"bucket"`
	Key     string                  `json:"key"`
	Added   []types.Partition       `json:"added"`
	Removed []types.Partition       `json:"removed"`
	Changed []PartitionWeightChange `json:"changed"`
	Before  []types.Partition       `json:"before"`
	After   []types.Partition       `json:"after"`
}

// UpdateStreamResource is the Resource carried by an ActionUpdateStream
// PlannedAction. Before is the live StreamConfig observed at plan time; After
// is the desired target. Both are deep clones so the Plan output is immutable
// regardless of later Apply or nats.go mutation.
//
// Apply does not write Resource.After verbatim — it re-reads live state and
// rebuilds the target from the re-read snapshot (see the Apply algorithm).
// Resource.Before / Resource.After are the audit surface: JSON consumers diff
// them to render exactly which fields change.
type UpdateStreamResource struct {
	Before jetstream.StreamConfig `json:"before"`
	After  jetstream.StreamConfig `json:"after"`
}

// StreamStampMarkerResource is the Resource carried by an
// ActionStampStreamMarker PlannedAction. Stream is the NATS stream name,
// MergedMetadata is the full Metadata map the action will write, and
// PartiKeys lists exactly the metadata keys the action adds or changes
// relative to the live stream, for operator review.
//
// Apply does not write MergedMetadata verbatim — it re-reads live state and
// recomputes the merge against the re-read metadata. MergedMetadata /
// PartiKeys are the audit surface for plan / apply -dry-run output.
type StreamStampMarkerResource struct {
	Stream         string            `json:"stream"`
	MergedMetadata map[string]string `json:"mergedMetadata"`
	PartiKeys      []string          `json:"partiKeys"`
}

// PartitionWeightChange records a partition present in both the live and
// declared tables whose Weight differs. Keys identifies the partition (it is
// unchanged — a different key set is a different partition, i.e. an add plus
// a remove, not a change).
type PartitionWeightChange struct {
	Keys      []string `json:"keys"`
	OldWeight int64    `json:"oldWeight"`
	NewWeight int64    `json:"newWeight"`
}

// DriftFinding describes how a live resource differs from the desired state,
// or that an existing resource is unmarked ("adopted").
//
// Severity values:
//   - "informational": resource exists and matches desired state; no action needed.
//   - "drift-mutable": fields differ but can be reconciled in place (see
//     PolicySafeUpdate and the ActionUpdateKV action kind).
//   - "drift-immutable": fields differ and require delete/recreate to repair;
//     destructive repair is not yet supported.
//   - "adopted": resource exists without the Parti marker; see PolicyAdopt.
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

	// KindPartitionRecords tags drift between the declared partition set and
	// the live contents of the partition-source key (see PlanPartitions).
	// It is distinct from KindPartitionSource, which tags drift in the
	// partition-source bucket *config*.
	KindPartitionRecords = "partition-records"

	// KindApplicationStream tags drift findings and informational findings
	// for application JetStream streams managed by provision (see planStreams).
	KindApplicationStream = "application-stream"
)

// Report is the result of Apply: what executed, what was skipped, what failed.
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
// Reason values:
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

// PlannedConsumer is the dynamic-consumer alignment output type. The struct
// exists so the Plan surface is stable; dynamic-consumer entries are not
// yet populated through Plan (use PlanDynamicConsumers directly).
type PlannedConsumer struct {
	StreamName string                   `json:"streamName"`
	Subject    string                   `json:"subject"`
	Durable    string                   `json:"durable"`
	Config     jetstream.ConsumerConfig `json:"config,omitempty"`
}
