package provision

// Same-package unit tests for classifyControlPlaneDrift and
// classifyPartitionSourceDrift. These tests use synthetic *jetstream.StreamInfo
// values so they run without a live NATS server and can exercise Replicas
// mismatches that a single-server cluster always normalizes to 1.

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// managedStreamConfig builds a jetstream.StreamConfig whose Metadata contains
// the expected managed marker for the given component. Instance is always
// "prod" across these unit tests. All unlisted fields default to zero.
func managedStreamConfig(component string, extra ...func(*jetstream.StreamConfig)) jetstream.StreamConfig {
	cfg := jetstream.StreamConfig{
		Metadata: BuildMarker(component, "prod"),
	}
	for _, fn := range extra {
		fn(&cfg)
	}

	return cfg
}

func TestClassifyControlPlaneDrift_ReplicasMismatch_DriftMutable(t *testing.T) {
	t.Parallel()

	spec := controlPlaneSpec{
		component: ComponentControlPlaneAssignment,
		bucket:    "parti-assignment",
		ttl:       0,
		storage:   jetstream.FileStorage,
		replicas:  0,
	}
	info := &jetstream.StreamInfo{
		Config: managedStreamConfig(ComponentControlPlaneAssignment, func(cfg *jetstream.StreamConfig) {
			cfg.Replicas = 3
			cfg.Storage = jetstream.FileStorage
			cfg.MaxMsgsPerSubject = 1
		}),
	}

	findings := classifyControlPlaneDrift(spec, info, "prod")

	var mutable []DriftFinding
	for _, f := range findings {
		if f.Severity == SeverityDriftMutable {
			mutable = append(mutable, f)
		}
	}
	require.Len(t, mutable, 1)
	require.Contains(t, mutable[0].Detail, "replicas")

	replicasDetail, ok := mutable[0].Detail["replicas"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 1, replicasDetail["want"])
	require.Equal(t, 3, replicasDetail["got"])

	for _, f := range findings {
		if f.Severity == SeverityDriftImmutable {
			require.NotContains(t, f.Detail, "replicas")
		}
	}
}

func TestClassifyControlPlaneDrift_ReplicasNormalization_NoMutable(t *testing.T) {
	t.Parallel()

	spec := controlPlaneSpec{
		component: ComponentControlPlaneAssignment,
		bucket:    "parti-assignment",
		ttl:       0,
		storage:   jetstream.FileStorage,
		replicas:  0,
	}
	info := &jetstream.StreamInfo{
		Config: managedStreamConfig(ComponentControlPlaneAssignment, func(cfg *jetstream.StreamConfig) {
			cfg.Replicas = 1
			cfg.Storage = jetstream.FileStorage
			cfg.MaxMsgsPerSubject = 1
		}),
	}

	findings := classifyControlPlaneDrift(spec, info, "prod")

	for _, f := range findings {
		require.NotContains(t, f.Detail, "replicas")
	}
}

func TestClassifyControlPlaneDrift_ReplicasExplicit_Match(t *testing.T) {
	t.Parallel()

	spec := controlPlaneSpec{
		component: ComponentControlPlaneHeartbeat,
		bucket:    "parti-heartbeat",
		ttl:       15 * time.Second,
		storage:   jetstream.MemoryStorage,
		replicas:  2,
	}
	info := &jetstream.StreamInfo{
		Config: managedStreamConfig(ComponentControlPlaneHeartbeat, func(cfg *jetstream.StreamConfig) {
			cfg.Replicas = 2
			cfg.Storage = jetstream.MemoryStorage
			cfg.MaxMsgsPerSubject = 1
			cfg.MaxAge = 15 * time.Second
		}),
	}

	findings := classifyControlPlaneDrift(spec, info, "prod")

	for _, f := range findings {
		require.NotContains(t, f.Detail, "replicas")
	}
}

func TestClassifyControlPlaneDrift_ReplicasExplicit_Mismatch(t *testing.T) {
	t.Parallel()

	spec := controlPlaneSpec{
		component: ComponentControlPlaneHeartbeat,
		bucket:    "parti-heartbeat",
		ttl:       15 * time.Second,
		storage:   jetstream.MemoryStorage,
		replicas:  2,
	}
	info := &jetstream.StreamInfo{
		Config: managedStreamConfig(ComponentControlPlaneHeartbeat, func(cfg *jetstream.StreamConfig) {
			cfg.Replicas = 1
			cfg.Storage = jetstream.MemoryStorage
			cfg.MaxMsgsPerSubject = 1
			cfg.MaxAge = 15 * time.Second
		}),
	}

	findings := classifyControlPlaneDrift(spec, info, "prod")

	var mutable []DriftFinding
	for _, f := range findings {
		if f.Severity == SeverityDriftMutable {
			mutable = append(mutable, f)
		}
	}
	require.Len(t, mutable, 1, "expected exactly one drift-mutable finding")
	require.Contains(t, mutable[0].Detail, "replicas")

	replicasDetail, ok := mutable[0].Detail["replicas"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 2, replicasDetail["want"])
	require.Equal(t, 1, replicasDetail["got"])
}

func TestClassifyPartitionSourceDrift_ReplicasMismatch_DriftMutable(t *testing.T) {
	t.Parallel()

	ps := &PartitionSourceConfig{
		Bucket:   "parti-partitions",
		Key:      "partitions/v1",
		Storage:  "file",
		History:  2,
		Replicas: 2,
	}
	info := &jetstream.StreamInfo{
		Config: managedStreamConfig(ComponentPartitionSource, func(cfg *jetstream.StreamConfig) {
			cfg.Replicas = 1
			cfg.Storage = jetstream.FileStorage
			cfg.MaxMsgsPerSubject = 2
		}),
	}

	findings := classifyPartitionSourceDrift(ps, info, "prod")

	var mutable []DriftFinding
	var immutable []DriftFinding
	for _, f := range findings {
		switch f.Severity {
		case SeverityDriftMutable:
			mutable = append(mutable, f)
		case SeverityDriftImmutable:
			immutable = append(immutable, f)
		}
	}

	hasMutableReplicas := false
	for _, f := range mutable {
		if _, ok := f.Detail["replicas"]; ok {
			hasMutableReplicas = true

			replicasDetail, ok := f.Detail["replicas"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, 2, replicasDetail["want"])
			require.Equal(t, 1, replicasDetail["got"])
		}
	}
	require.True(t, hasMutableReplicas, "findings: %+v", findings)

	for _, f := range immutable {
		require.NotContains(t, f.Detail, "replicas")
	}
}

func TestClassifyPartitionSourceDrift_ReplicasNormalization_NoMutable(t *testing.T) {
	t.Parallel()

	ps := &PartitionSourceConfig{
		Bucket:   "parti-partitions",
		Key:      "partitions/v1",
		Storage:  "file",
		History:  1,
		Replicas: 0,
	}
	info := &jetstream.StreamInfo{
		Config: managedStreamConfig(ComponentPartitionSource, func(cfg *jetstream.StreamConfig) {
			cfg.Replicas = 1
			cfg.Storage = jetstream.FileStorage
			cfg.MaxMsgsPerSubject = 1
		}),
	}

	findings := classifyPartitionSourceDrift(ps, info, "prod")

	for _, f := range findings {
		require.NotContains(t, f.Detail, "replicas")
	}
}

func TestClassifyPartitionSourceDrift_ReplicasMatch_NoDrift(t *testing.T) {
	t.Parallel()

	ps := &PartitionSourceConfig{
		Bucket:   "parti-partitions",
		Key:      "partitions/v1",
		Storage:  "file",
		History:  1,
		Replicas: 3,
	}
	info := &jetstream.StreamInfo{
		Config: managedStreamConfig(ComponentPartitionSource, func(cfg *jetstream.StreamConfig) {
			cfg.Replicas = 3
			cfg.Storage = jetstream.FileStorage
			cfg.MaxMsgsPerSubject = 1
		}),
	}

	findings := classifyPartitionSourceDrift(ps, info, "prod")

	for _, f := range findings {
		require.NotContains(t, f.Detail, "replicas")
	}
}

func TestClassifyControlPlaneDrift_ManagedVersionMismatch_DriftMutable(t *testing.T) {
	t.Parallel()

	spec := controlPlaneSpec{
		component: ComponentControlPlaneHeartbeat,
		bucket:    "parti-heartbeat",
		ttl:       15 * time.Second,
		storage:   jetstream.MemoryStorage,
		replicas:  0,
	}
	info := &jetstream.StreamInfo{
		Config: managedStreamConfig(ComponentControlPlaneHeartbeat, func(cfg *jetstream.StreamConfig) {
			cfg.Replicas = 1
			cfg.Storage = jetstream.MemoryStorage
			cfg.MaxMsgsPerSubject = 1
			cfg.MaxAge = 15 * time.Second
			cfg.Metadata[MarkerManagedKey] = "v2"
		}),
	}

	findings := classifyControlPlaneDrift(spec, info, "prod")

	var mutable []DriftFinding
	for _, f := range findings {
		if f.Severity == SeverityDriftMutable {
			mutable = append(mutable, f)
		}
	}
	require.Len(t, mutable, 1)
	require.Contains(t, mutable[0].Detail, "managed")
}

func TestClassifyControlPlaneDrift_InstanceRemoval_DriftMutable(t *testing.T) {
	t.Parallel()

	spec := controlPlaneSpec{
		component: ComponentControlPlaneHeartbeat,
		bucket:    "parti-heartbeat",
		ttl:       15 * time.Second,
		storage:   jetstream.MemoryStorage,
		replicas:  0,
	}
	info := &jetstream.StreamInfo{
		Config: managedStreamConfig(ComponentControlPlaneHeartbeat, func(cfg *jetstream.StreamConfig) {
			cfg.Replicas = 1
			cfg.Storage = jetstream.MemoryStorage
			cfg.MaxMsgsPerSubject = 1
			cfg.MaxAge = 15 * time.Second
		}),
	}

	findings := classifyControlPlaneDrift(spec, info, "")

	var mutable []DriftFinding
	for _, f := range findings {
		if f.Severity == SeverityDriftMutable {
			mutable = append(mutable, f)
		}
	}
	require.Len(t, mutable, 1)
	require.Contains(t, mutable[0].Detail, "instance")

	detail, ok := mutable[0].Detail["instance"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "", detail["want"])
	require.Equal(t, "prod", detail["got"])
}

func TestClassifyPartitionSourceDrift_ComponentMismatch_DriftImmutable(t *testing.T) {
	t.Parallel()

	ps := &PartitionSourceConfig{
		Bucket:   "parti-partitions",
		Key:      "partitions/v1",
		Storage:  "file",
		History:  1,
		Replicas: 0,
	}
	info := &jetstream.StreamInfo{
		Config: managedStreamConfig(ComponentControlPlaneAssignment, func(cfg *jetstream.StreamConfig) {
			cfg.Replicas = 1
			cfg.Storage = jetstream.FileStorage
			cfg.MaxMsgsPerSubject = 1
		}),
	}

	findings := classifyPartitionSourceDrift(ps, info, "prod")

	var immutable []DriftFinding
	for _, f := range findings {
		if f.Severity == SeverityDriftImmutable {
			immutable = append(immutable, f)
		}
	}
	require.Len(t, immutable, 1)
	require.Contains(t, immutable[0].Detail, "component")

	for _, f := range findings {
		if f.Severity == SeverityDriftMutable {
			require.NotContains(t, f.Detail, "component")
		}
	}
}
