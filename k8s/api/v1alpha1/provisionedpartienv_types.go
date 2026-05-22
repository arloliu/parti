package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ppe
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Policy",type="string",JSONPath=`.spec.policy`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=`.metadata.creationTimestamp`

// ProvisionedPartiEnv is the Schema for the provisionedpartienvs API.
// It declares desired NATS infrastructure (KV buckets and streams) that the
// Parti operator reconciles by driving the provision SDK.
type ProvisionedPartiEnv struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProvisionedPartiEnvSpec   `json:"spec,omitempty"`
	Status ProvisionedPartiEnvStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProvisionedPartiEnvList contains a list of ProvisionedPartiEnv.
type ProvisionedPartiEnvList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProvisionedPartiEnv `json:"items"`
}

// ProvisionedPartiEnvSpec defines the desired state of ProvisionedPartiEnv.
type ProvisionedPartiEnvSpec struct {
	// NATS is the connection configuration for the NATS server.
	// +kubebuilder:validation:Required
	NATS NATSConnection `json:"nats"`

	// Instance is an optional identifier for this Parti instance. Maps to
	// provision.Config.Instance.
	// +optional
	Instance string `json:"instance,omitempty"`

	// Policy controls how the operator reconciles drift. Defaults to "warn"
	// when empty (the SDK default).
	// +kubebuilder:validation:Enum=warn;adopt;safe-update;force
	// +optional
	Policy string `json:"policy,omitempty"`

	// ControlPlane declares desired control-plane KV buckets. Omitting this
	// field leaves control-plane provisioning to SDK defaults.
	// +optional
	ControlPlane *ControlPlaneSpec `json:"controlPlane,omitempty"`

	// PartitionSource declares the desired partition-source KV bucket.
	// The partition table contents (Partitions) are deliberately omitted —
	// those are managed by the partictl CLI, not the operator.
	// +optional
	PartitionSource *PartitionSourceSpec `json:"partitionSource,omitempty"`

	// Streams declares desired application JetStream streams.
	// +optional
	Streams []StreamSpec `json:"streams,omitempty"`
}

// NATSConnection holds the NATS server address and optional authentication
// material for the operator to connect to NATS.
type NATSConnection struct {
	// Server is the NATS server URL (e.g. "nats://nats.example.com:4222").
	// +kubebuilder:validation:Required
	Server string `json:"server"`

	// CredentialsSecret is an optional reference to a Kubernetes Secret in
	// the same namespace as the CR. At most one of CredentialsKey, TokenKey,
	// or NKeyKey may be set; none means anonymous access.
	// +optional
	CredentialsSecret *NATSAuthSecret `json:"credentialsSecret,omitempty"`
}

// NATSAuthSecret references a Kubernetes Secret and the data key(s) within it
// that hold NATS authentication material. Exactly one of CredentialsKey,
// TokenKey, or NKeyKey may be set; omitting all three uses the secret for
// future extension only (no auth material read).
type NATSAuthSecret struct {
	// Name is the name of the Secret in the CR's namespace.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// CredentialsKey names the Secret data key whose value is a NATS .creds
	// file (NKey + JWT bundle). Mutually exclusive with TokenKey and NKeyKey.
	// +optional
	CredentialsKey string `json:"credentialsKey,omitempty"`

	// TokenKey names the Secret data key whose value is a NATS authentication
	// token. Mutually exclusive with CredentialsKey and NKeyKey.
	// +optional
	TokenKey string `json:"tokenKey,omitempty"`

	// NKeyKey names the Secret data key whose value is an NKey user seed.
	// Mutually exclusive with CredentialsKey and TokenKey.
	// +optional
	NKeyKey string `json:"nkeyKey,omitempty"`
}

// ControlPlaneSpec mirrors provision.ControlPlaneConfig for the CRD API.
// It declares the desired state of the control-plane KV buckets.
type ControlPlaneSpec struct {
	// StableIDBucket is the name of the stable-ID KV bucket.
	// +optional
	StableIDBucket string `json:"stableIdBucket,omitempty"`

	// ElectionBucket is the name of the election KV bucket.
	// +optional
	ElectionBucket string `json:"electionBucket,omitempty"`

	// HeartbeatBucket is the name of the heartbeat KV bucket.
	// +optional
	HeartbeatBucket string `json:"heartbeatBucket,omitempty"`

	// AssignmentBucket is the name of the assignment KV bucket.
	// +optional
	AssignmentBucket string `json:"assignmentBucket,omitempty"`

	// HandoffBucket is the name of the handoff KV bucket. Only used when
	// EnableTwoPhaseHandoff is true.
	// +optional
	HandoffBucket string `json:"handoffBucket,omitempty"`

	// WorkerIDTTL is the TTL for stable-ID entries.
	// +optional
	WorkerIDTTL metav1.Duration `json:"workerIdTtl,omitempty"`

	// ElectionTimeout is the TTL for election entries.
	// +optional
	ElectionTimeout metav1.Duration `json:"electionTimeout,omitempty"`

	// HeartbeatTTL is the TTL for heartbeat entries.
	// +optional
	HeartbeatTTL metav1.Duration `json:"heartbeatTtl,omitempty"`

	// AssignmentTTL is the TTL for assignment entries. 0 means no expiration.
	// +optional
	AssignmentTTL metav1.Duration `json:"assignmentTtl,omitempty"`

	// HandoffTTL is the advisory sweep TTL for recovering stuck in-flight
	// handoff claims. Only relevant when EnableTwoPhaseHandoff is true.
	// +optional
	HandoffTTL metav1.Duration `json:"handoffTtl,omitempty"`

	// EnableTwoPhaseHandoff gates the optional handoff KV bucket. When true,
	// HandoffTTL must be > 0.
	// +optional
	EnableTwoPhaseHandoff bool `json:"enableTwoPhaseHandoff,omitempty"`

	// Replicas is the desired NATS stream replica count for every
	// control-plane KV bucket. 0 leaves the server default (1).
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// AllowDeleteRecreate opts control-plane buckets into delete/recreate
	// under PolicyForce when they carry drift-immutable drift.
	// +optional
	AllowDeleteRecreate bool `json:"allowDeleteRecreate,omitempty"`
}

// PartitionSourceSpec mirrors provision.PartitionSourceConfig for the CRD API,
// omitting the Partitions field (partition-record contents are managed by
// partictl, not the operator).
type PartitionSourceSpec struct {
	// Bucket is the name of the partition-source KV bucket.
	// +optional
	Bucket string `json:"bucket,omitempty"`

	// Key is the KV key within Bucket that holds the partition table.
	// +optional
	Key string `json:"key,omitempty"`

	// Replicas is the desired NATS stream replica count for the
	// partition-source KV bucket. 0 leaves the server default (1).
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Storage is the NATS storage backend for the partition-source bucket.
	// +kubebuilder:validation:Enum=file;memory
	// +optional
	Storage string `json:"storage,omitempty"`

	// History is the KV history depth. The SDK uses uint8; the CRD uses int32
	// with a 0–255 bound so the API server rejects out-of-range values before
	// the mapper can silently wrap them.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=255
	// +optional
	History int32 `json:"history,omitempty"`

	// MaxValueSize is the maximum per-value size in bytes. 0 means no limit.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxValueSize int32 `json:"maxValueSize,omitempty"`

	// TTL is the per-entry TTL for the partition-source KV bucket.
	// +optional
	TTL metav1.Duration `json:"ttl,omitempty"`

	// AllowDeleteRecreate opts the partition-source bucket into delete/recreate
	// under PolicyForce when it carries drift-immutable drift.
	// +optional
	AllowDeleteRecreate bool `json:"allowDeleteRecreate,omitempty"`
}

// StreamSpec mirrors provision.StreamCfg for the CRD API.
type StreamSpec struct {
	// Name is the JetStream stream name.
	Name string `json:"name"`

	// Subjects is the list of NATS subjects the stream captures.
	Subjects []string `json:"subjects"`

	// Retention controls the stream's message retention policy.
	// +kubebuilder:validation:Enum=limits;workqueue;interest
	// +optional
	Retention string `json:"retention,omitempty"`

	// Storage is the NATS storage backend for this stream.
	// +kubebuilder:validation:Enum=file;memory
	// +optional
	Storage string `json:"storage,omitempty"`

	// Discard controls which messages are dropped when limits are reached.
	// +kubebuilder:validation:Enum=old;new
	// +optional
	Discard string `json:"discard,omitempty"`

	// Replicas is the desired NATS stream replica count. 0 leaves the server
	// default (1).
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// MaxAge is the maximum age of messages in the stream. 0 means unlimited.
	// +optional
	MaxAge metav1.Duration `json:"maxAge,omitempty"`

	// MaxBytes is the maximum total byte size of messages in the stream.
	// 0 means unlimited (NATS stores as -1 internally).
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxBytes int64 `json:"maxBytes,omitempty"`

	// MaxMsgs is the maximum number of messages in the stream.
	// 0 means unlimited (NATS stores as -1 internally).
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxMsgs int64 `json:"maxMsgs,omitempty"`

	// Description is an optional human-readable description of the stream.
	// +optional
	Description string `json:"description,omitempty"`

	// AllowDeleteRecreate opts this stream into delete/recreate under
	// PolicyForce when it carries drift-immutable drift.
	// +optional
	AllowDeleteRecreate bool `json:"allowDeleteRecreate,omitempty"`
}

// ProvisionedPartiEnvStatus defines the observed state of ProvisionedPartiEnv.
type ProvisionedPartiEnvStatus struct {
	// ObservedGeneration is the .metadata.generation the last reconcile acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the standard Kubernetes condition list. The only
	// condition type is "Ready"; its Reason field carries granular outcome
	// information (Reconciled, InvalidSpec, SecretMissing, NATSUnreachable,
	// ApplyError).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastReconcileTime is the timestamp of the most recent reconcile attempt
	// that produced a status write.
	// +optional
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`

	// LastPlan summarises the most recent provision.Plan result.
	// +optional
	LastPlan *PlanSummary `json:"lastPlan,omitempty"`

	// LastApply summarises the most recent provision.Apply result.
	// +optional
	LastApply *ApplySummary `json:"lastApply,omitempty"`
}

// PlanSummary carries counts from the most recent provision.Plan call.
type PlanSummary struct {
	// ActionCount is the total number of planned actions.
	ActionCount int `json:"actionCount"`

	// DriftInformational is the count of informational drift findings.
	DriftInformational int `json:"driftInformational"`

	// DriftMutable is the count of drift-mutable drift findings.
	DriftMutable int `json:"driftMutable"`

	// DriftImmutable is the count of drift-immutable drift findings.
	DriftImmutable int `json:"driftImmutable"`

	// DriftAdopted is the count of adopted drift findings.
	DriftAdopted int `json:"driftAdopted"`
}

// ApplySummary carries counts and error messages from the most recent
// provision.Apply call. Error messages are capped to keep the status object
// small (etcd size limits).
type ApplySummary struct {
	// ExecutedCount is the number of actions successfully executed.
	ExecutedCount int `json:"executedCount"`

	// SkippedCount is the number of actions skipped.
	SkippedCount int `json:"skippedCount"`

	// ErrorCount is the number of resource-level errors encountered.
	ErrorCount int `json:"errorCount"`

	// Aborted is true when the apply was cancelled mid-execution.
	Aborted bool `json:"aborted,omitempty"`

	// Errors holds the first N resource error messages from the apply.
	// +optional
	Errors []string `json:"errors,omitempty"`
}
