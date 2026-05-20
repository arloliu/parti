package provision

import "time"

// Config is the desired environment state for the provision SDK. APIVersion
// is required; v1 accepts only "parti.io/v1".
//
// Empty/omitted bucket-name fields are populated by Validate from runtime
// defaults (see ControlPlaneConfig). Validate also defaults Policy to
// PolicyWarn when empty.
type Config struct {
	APIVersion       string                 `yaml:"apiVersion"                  json:"apiVersion"`
	Instance         string                 `yaml:"instance,omitempty"          json:"instance,omitempty"`
	Policy           ReconcilePolicy        `yaml:"policy,omitempty"            json:"policy,omitempty"`
	ControlPlane     *ControlPlaneConfig    `yaml:"controlPlane,omitempty"      json:"controlPlane,omitempty"`
	PartitionSource  *PartitionSourceConfig `yaml:"partitionSource,omitempty"   json:"partitionSource,omitempty"`
	DynamicConsumers []DynamicConsumerCfg   `yaml:"dynamicConsumers,omitempty"  json:"dynamicConsumers,omitempty"`
	// Streams is intentionally absent in v1 (see Non-Goals in plan).
}

// ControlPlaneConfig mirrors the parti runtime fields that drive control-plane
// KV bucket creation. Field names and yaml tags intentionally match
// parti.Config / parti.KVBucketConfig so a single canonical config source can
// populate both the runtime manager and this provisioning input.
type ControlPlaneConfig struct {
	// Bucket names (runtime origin: parti.KVBucketConfig).
	// Empty/omitted fields default to the runtime defaults during Validate:
	// "parti-stableid", "parti-election", "parti-heartbeat",
	// "parti-assignment", "parti-handoff".
	StableIDBucket   string `yaml:"stableIdBucket"   json:"stableIdBucket"`
	ElectionBucket   string `yaml:"electionBucket"   json:"electionBucket"`
	HeartbeatBucket  string `yaml:"heartbeatBucket"  json:"heartbeatBucket"`
	AssignmentBucket string `yaml:"assignmentBucket" json:"assignmentBucket"`
	HandoffBucket    string `yaml:"handoffBucket"    json:"handoffBucket"`

	// TTLs. WorkerIDTTL/HeartbeatTTL/ElectionTimeout live on parti.Config;
	// AssignmentTTL and HandoffTTL live on parti.KVBucketConfig.
	WorkerIDTTL     time.Duration `yaml:"workerIdTtl"     json:"workerIdTtl"`
	ElectionTimeout time.Duration `yaml:"electionTimeout" json:"electionTimeout"`
	HeartbeatTTL    time.Duration `yaml:"heartbeatTtl"    json:"heartbeatTtl"`
	AssignmentTTL   time.Duration `yaml:"assignmentTtl"   json:"assignmentTtl"` // 0 = no expiration

	// HandoffTTL is the two-phase handoff coordinator's advisory sweep TTL for
	// recovering STUCK in-flight handoff claims. It does NOT set the handoff
	// bucket's MaxAge: the handoff bucket is provisioned with no TTL so stable
	// ownership claims never expire (a bucket-level TTL would age them out and
	// permanently suppress pull-gated consumers).
	HandoffTTL time.Duration `yaml:"handoffTtl" json:"handoffTtl"`

	// EnableTwoPhaseHandoff gates the optional handoff KV bucket. When true,
	// HandoffTTL must be > 0.
	EnableTwoPhaseHandoff bool `yaml:"enableTwoPhaseHandoff" json:"enableTwoPhaseHandoff"`

	// Replicas is the desired number of NATS stream replicas for every
	// control-plane KV bucket. 0 (the default) leaves the underlying
	// KeyValueConfig.Replicas field unset; nats.go normalizes that to 1
	// server-side. Non-zero values are drift-mutable under safe-update;
	// the NATS server enforces cluster-peer feasibility at apply time.
	//
	// Applies uniformly to every control-plane bucket; per-bucket
	// replica overrides are not supported.
	Replicas int `yaml:"replicas,omitempty" json:"replicas,omitempty"`
}

// PartitionSourceConfig declares the NATS KV bucket that holds the partition
// definition record. v1 provisions the bucket only; partition record contents
// are written by Phase 3 (Partition Records).
type PartitionSourceConfig struct {
	Bucket   string `yaml:"bucket"                 json:"bucket"`
	Key      string `yaml:"key"                    json:"key"`
	Replicas int    `yaml:"replicas,omitempty"     json:"replicas,omitempty"`
	Storage  string `yaml:"storage,omitempty"      json:"storage,omitempty"` // "file" | "memory"; default "file"
	History  uint8  `yaml:"history,omitempty"      json:"history,omitempty"` // default 1
	// MaxValueSize is the maximum per-value size in bytes. 0 means "no limit";
	// in live NATS state this is stored as MaxMsgSize=-1. provision.Plan
	// normalizes these as equivalent when classifying drift, so a config
	// MaxValueSize=0 and a live MaxMsgSize=-1 do not produce drift-mutable
	// findings.
	MaxValueSize int32         `yaml:"maxValueSize,omitempty" json:"maxValueSize,omitempty"`
	TTL          time.Duration `yaml:"ttl,omitempty"          json:"ttl,omitempty"`
}

// DynamicConsumerCfg describes one dynamic-consumer alignment target. v1
// performs alignment-check only; consumer pre-creation lands in Phase 5.
type DynamicConsumerCfg struct {
	StreamName      string `yaml:"streamName"              json:"streamName"`
	ConsumerPrefix  string `yaml:"consumerPrefix"          json:"consumerPrefix"`
	SubjectTemplate string `yaml:"subjectTemplate"         json:"subjectTemplate"`
	PartitionsRef   string `yaml:"partitionsRef,omitempty" json:"partitionsRef,omitempty"`
}

// Runtime bucket-name defaults. These mirror parti.KVBucketConfig's
// `default:"…"` struct tags exactly. Kept here so provision/ doesn't take
// a dependency on the parti runtime package's defaulting machinery.
const (
	defaultStableIDBucket   = "parti-stableid"
	defaultElectionBucket   = "parti-election"
	defaultHeartbeatBucket  = "parti-heartbeat"
	defaultAssignmentBucket = "parti-assignment"
	defaultHandoffBucket    = "parti-handoff"
)
