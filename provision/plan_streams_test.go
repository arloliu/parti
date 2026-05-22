package provision

// Internal unit tests for planStreams helpers and classifyStreamDrift.
// These tests use synthetic jetstream.StreamInfo values so they run without a
// live NATS server and can exercise field-level drift cases that a server would
// normalize away (e.g. storage-only divergence, retention-only divergence).

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --------------------------------------------------------------------------
// streamConfigsEqual normalization tests
// --------------------------------------------------------------------------

func TestStreamConfigsEqual_SubjectsAsSet(t *testing.T) {
	t.Parallel()

	a := jetstream.StreamConfig{
		Subjects: []string{"a.b", "c.d", "e.f"},
		Metadata: BuildMarker(ComponentStream, "prod"),
	}
	b := jetstream.StreamConfig{
		Subjects: []string{"e.f", "a.b", "c.d"},
		Metadata: BuildMarker(ComponentStream, "prod"),
	}
	require.True(t, streamConfigsEqual(a, b), "subject order must not matter")

	c := jetstream.StreamConfig{
		Subjects: []string{"a.b", "c.d"},
		Metadata: BuildMarker(ComponentStream, "prod"),
	}
	require.False(t, streamConfigsEqual(a, c), "different subject sets must not be equal")
}

func TestStreamConfigsEqual_StorageDefault(t *testing.T) {
	t.Parallel()

	// FileStorage is the zero value; a config with no Storage set should equal
	// one with FileStorage set explicitly.
	a := jetstream.StreamConfig{Subjects: []string{"x"}, Metadata: BuildMarker(ComponentStream, "prod")}
	b := jetstream.StreamConfig{Subjects: []string{"x"}, Storage: jetstream.FileStorage, Metadata: BuildMarker(ComponentStream, "prod")}
	require.True(t, streamConfigsEqual(a, b), "empty storage == FileStorage")

	c := jetstream.StreamConfig{Subjects: []string{"x"}, Storage: jetstream.MemoryStorage, Metadata: BuildMarker(ComponentStream, "prod")}
	require.False(t, streamConfigsEqual(a, c), "FileStorage != MemoryStorage")
}

func TestStreamConfigsEqual_RetentionDefault(t *testing.T) {
	t.Parallel()

	a := jetstream.StreamConfig{Subjects: []string{"x"}, Metadata: BuildMarker(ComponentStream, "prod")}
	b := jetstream.StreamConfig{Subjects: []string{"x"}, Retention: jetstream.LimitsPolicy, Metadata: BuildMarker(ComponentStream, "prod")}
	require.True(t, streamConfigsEqual(a, b), "empty retention == LimitsPolicy")

	c := jetstream.StreamConfig{Subjects: []string{"x"}, Retention: jetstream.WorkQueuePolicy, Metadata: BuildMarker(ComponentStream, "prod")}
	require.False(t, streamConfigsEqual(a, c), "LimitsPolicy != WorkQueuePolicy")
}

func TestStreamConfigsEqual_DiscardDefault(t *testing.T) {
	t.Parallel()

	a := jetstream.StreamConfig{Subjects: []string{"x"}, Metadata: BuildMarker(ComponentStream, "prod")}
	b := jetstream.StreamConfig{Subjects: []string{"x"}, Discard: jetstream.DiscardOld, Metadata: BuildMarker(ComponentStream, "prod")}
	require.True(t, streamConfigsEqual(a, b), "empty discard == DiscardOld")

	c := jetstream.StreamConfig{Subjects: []string{"x"}, Discard: jetstream.DiscardNew, Metadata: BuildMarker(ComponentStream, "prod")}
	require.False(t, streamConfigsEqual(a, c), "DiscardOld != DiscardNew")
}

func TestStreamConfigsEqual_ReplicasNormalization(t *testing.T) {
	t.Parallel()

	a := jetstream.StreamConfig{Subjects: []string{"x"}, Replicas: 0, Metadata: BuildMarker(ComponentStream, "prod")}
	b := jetstream.StreamConfig{Subjects: []string{"x"}, Replicas: 1, Metadata: BuildMarker(ComponentStream, "prod")}
	require.True(t, streamConfigsEqual(a, b), "replicas 0 == 1")

	c := jetstream.StreamConfig{Subjects: []string{"x"}, Replicas: 3, Metadata: BuildMarker(ComponentStream, "prod")}
	require.False(t, streamConfigsEqual(a, c), "replicas 0 != 3")
}

func TestStreamConfigsEqual_MaxBytesMaxMsgsNormalization(t *testing.T) {
	t.Parallel()

	// 0 and -1 must be equivalent for MaxBytes and MaxMsgs.
	a := jetstream.StreamConfig{Subjects: []string{"x"}, MaxBytes: 0, MaxMsgs: 0, Metadata: BuildMarker(ComponentStream, "prod")}
	b := jetstream.StreamConfig{Subjects: []string{"x"}, MaxBytes: -1, MaxMsgs: -1, Metadata: BuildMarker(ComponentStream, "prod")}
	require.True(t, streamConfigsEqual(a, b), "MaxBytes/MaxMsgs 0 == -1")

	c := jetstream.StreamConfig{Subjects: []string{"x"}, MaxBytes: 1024, Metadata: BuildMarker(ComponentStream, "prod")}
	require.False(t, streamConfigsEqual(a, c), "MaxBytes 0 != 1024")
}

func TestStreamConfigsEqual_MaxAgeDirectComparison(t *testing.T) {
	t.Parallel()

	// MaxAge does NOT use the 0↔-1 normalization. A live value of -1 must NOT
	// equal a config value of 0, and 0 must equal only 0.
	a := jetstream.StreamConfig{Subjects: []string{"x"}, MaxAge: 0, Metadata: BuildMarker(ComponentStream, "prod")}
	b := jetstream.StreamConfig{Subjects: []string{"x"}, MaxAge: 0, Metadata: BuildMarker(ComponentStream, "prod")}
	require.True(t, streamConfigsEqual(a, b), "MaxAge 0 == 0")

	// Simulate a live -1 (should it ever appear) not equaling config 0.
	c := jetstream.StreamConfig{Subjects: []string{"x"}, MaxAge: -1, Metadata: BuildMarker(ComponentStream, "prod")}
	require.False(t, streamConfigsEqual(a, c), "MaxAge 0 != -1 (direct comparison, no normalization)")

	d := jetstream.StreamConfig{Subjects: []string{"x"}, MaxAge: 5 * time.Second, Metadata: BuildMarker(ComponentStream, "prod")}
	require.False(t, streamConfigsEqual(a, d), "MaxAge 0 != 5s")
}

func TestStreamConfigsEqual_MetadataMap(t *testing.T) {
	t.Parallel()

	a := jetstream.StreamConfig{
		Subjects: []string{"x"},
		Metadata: map[string]string{"parti.io/managed": "v1", "parti.io/component": "stream"},
	}
	b := jetstream.StreamConfig{
		Subjects: []string{"x"},
		Metadata: map[string]string{"parti.io/managed": "v1", "parti.io/component": "stream"},
	}
	require.True(t, streamConfigsEqual(a, b))

	c := jetstream.StreamConfig{
		Subjects: []string{"x"},
		Metadata: map[string]string{"parti.io/managed": "v1", "parti.io/component": "stream", "owner": "payments"},
	}
	require.False(t, streamConfigsEqual(a, c), "extra operator key must cause inequality")
}

// --------------------------------------------------------------------------
// classifyStreamDrift tests
// --------------------------------------------------------------------------

// buildLiveStreamInfo builds a *jetstream.StreamInfo representing a Parti-managed
// application stream. The base has all server defaults (LimitsPolicy, FileStorage,
// DiscardOld) and is marked with ComponentStream/"prod".
func buildLiveStreamInfo(name string, extra ...func(*jetstream.StreamConfig)) *jetstream.StreamInfo {
	cfg := jetstream.StreamConfig{
		Name:     name,
		Subjects: []string{"work." + name + ".>"},
		Metadata: BuildMarker(ComponentStream, "prod"),
	}
	for _, fn := range extra {
		fn(&cfg)
	}

	return &jetstream.StreamInfo{Config: cfg}
}

func TestClassifyStreamDrift_UnmarkedStream_AdoptedFinding(t *testing.T) {
	t.Parallel()

	streamCfg := StreamCfg{
		Name:     "my-stream",
		Subjects: []string{"work.>"},
		Storage:  "file",
	}
	// Unmarked stream: no Metadata.
	info := &jetstream.StreamInfo{
		Config: jetstream.StreamConfig{
			Name:     "my-stream",
			Subjects: []string{"work.>"},
		},
	}

	findings := classifyStreamDrift(streamCfg, info, "prod")

	require.Len(t, findings, 1)
	require.Equal(t, SeverityAdopted, findings[0].Severity)
	require.Equal(t, KindApplicationStream, findings[0].Kind)
	require.Equal(t, "my-stream", findings[0].Name)
}

func TestClassifyStreamDrift_ConvergedStream_Informational(t *testing.T) {
	t.Parallel()

	const streamName = "orders-stream"
	streamCfg := StreamCfg{
		Name:      streamName,
		Subjects:  []string{"work." + streamName + ".>"},
		Retention: "limits",
		Storage:   "file",
		Discard:   "old",
		Replicas:  0,
	}
	info := buildLiveStreamInfo(streamName)

	findings := classifyStreamDrift(streamCfg, info, "prod")

	require.Len(t, findings, 1)
	require.Equal(t, SeverityInformational, findings[0].Severity)
	require.Equal(t, KindApplicationStream, findings[0].Kind)
}

func TestClassifyStreamDrift_MutableOnlyDrift_DriftMutable(t *testing.T) {
	t.Parallel()

	streamCfg := StreamCfg{
		Name:        "my-stream",
		Subjects:    []string{"work.new.>"}, // different from live
		Retention:   "limits",
		Storage:     "file",
		Discard:     "old",
		Replicas:    2,
		MaxAge:      10 * time.Second,
		MaxBytes:    1024,
		MaxMsgs:     100,
		Description: "updated",
	}
	info := buildLiveStreamInfo("my-stream", func(cfg *jetstream.StreamConfig) {
		cfg.Subjects = []string{"work.old.>"}
		cfg.Replicas = 1
	})

	findings := classifyStreamDrift(streamCfg, info, "prod")

	require.Len(t, findings, 1)
	require.Equal(t, SeverityDriftMutable, findings[0].Severity)
	require.Equal(t, KindApplicationStream, findings[0].Kind)
}

func TestClassifyStreamDrift_StorageOnlyDivergence_DriftImmutable(t *testing.T) {
	t.Parallel()

	// The regression guard: wantedStreamConfig overlays Storage, so a
	// storage-only divergence must NOT be masked as informational.
	streamCfg := StreamCfg{
		Name:      "my-stream",
		Subjects:  []string{"work.my-stream.>"},
		Retention: "limits",
		Storage:   "memory", // desired: memory
		Discard:   "old",
	}
	info := buildLiveStreamInfo("my-stream", func(cfg *jetstream.StreamConfig) {
		cfg.Subjects = []string{"work.my-stream.>"}
		cfg.Storage = jetstream.FileStorage // live: file
	})

	findings := classifyStreamDrift(streamCfg, info, "prod")

	var immutable []DriftFinding
	for _, f := range findings {
		if f.Severity == SeverityDriftImmutable {
			immutable = append(immutable, f)
		}
	}
	require.Len(t, immutable, 1, "storage-only divergence must produce exactly one drift-immutable finding; got: %+v", findings)
	require.Contains(t, immutable[0].Detail, "storage")
	// Must NOT be masked as informational.
	for _, f := range findings {
		require.NotEqual(t, SeverityInformational, f.Severity, "storage divergence must not be informational")
	}
}

func TestClassifyStreamDrift_RetentionOnlyDivergence_DriftImmutable(t *testing.T) {
	t.Parallel()

	// The regression guard: wantedStreamConfig overlays Retention.
	streamCfg := StreamCfg{
		Name:      "my-stream",
		Subjects:  []string{"work.my-stream.>"},
		Retention: "workqueue", // desired: workqueue
		Storage:   "file",
		Discard:   "old",
	}
	info := buildLiveStreamInfo("my-stream", func(cfg *jetstream.StreamConfig) {
		cfg.Subjects = []string{"work.my-stream.>"}
		cfg.Retention = jetstream.LimitsPolicy // live: limits
	})

	findings := classifyStreamDrift(streamCfg, info, "prod")

	var immutable []DriftFinding
	for _, f := range findings {
		if f.Severity == SeverityDriftImmutable {
			immutable = append(immutable, f)
		}
	}
	require.Len(t, immutable, 1, "retention-only divergence must produce exactly one drift-immutable finding; got: %+v", findings)
	require.Contains(t, immutable[0].Detail, "retention")
	for _, f := range findings {
		require.NotEqual(t, SeverityInformational, f.Severity, "retention divergence must not be informational")
	}
}

func TestClassifyStreamDrift_LimitsToInterestRetention_DriftImmutable(t *testing.T) {
	t.Parallel()

	// Conservative-retention policy: limits↔interest must also classify
	// drift-immutable, even though NATS technically accepts this transition.
	streamCfg := StreamCfg{
		Name:      "my-stream",
		Subjects:  []string{"work.my-stream.>"},
		Retention: "interest",
		Storage:   "file",
		Discard:   "old",
	}
	info := buildLiveStreamInfo("my-stream", func(cfg *jetstream.StreamConfig) {
		cfg.Subjects = []string{"work.my-stream.>"}
		cfg.Retention = jetstream.LimitsPolicy
	})

	findings := classifyStreamDrift(streamCfg, info, "prod")

	hasImmutableRetention := false
	for _, f := range findings {
		if f.Severity == SeverityDriftImmutable {
			if _, ok := f.Detail["retention"]; ok {
				hasImmutableRetention = true
			}
		}
	}
	require.True(t, hasImmutableRetention, "limits↔interest retention change must classify drift-immutable; got: %+v", findings)
}

func TestClassifyStreamDrift_ComponentOnlyDivergence_DriftImmutable(t *testing.T) {
	t.Parallel()

	// A stream marked with a different component (e.g. control-plane:id) under
	// the application-stream config path must classify drift-immutable.
	streamCfg := StreamCfg{
		Name:      "my-stream",
		Subjects:  []string{"work.my-stream.>"},
		Retention: "limits",
		Storage:   "file",
		Discard:   "old",
	}
	// Marked managed=v1, instance=prod, but component=control-plane:id (wrong
	// component). Instance matches the classify arg so the divergence is
	// genuinely component-only — no instance drift to mask a routing regression.
	info := &jetstream.StreamInfo{
		Config: jetstream.StreamConfig{
			Name:     "my-stream",
			Subjects: []string{"work.my-stream.>"},
			Metadata: map[string]string{
				MarkerManagedKey:   MarkerManagedValue,
				MarkerComponentKey: ComponentControlPlaneID,
				MarkerInstanceKey:  "prod",
			},
		},
	}

	findings := classifyStreamDrift(streamCfg, info, "prod")

	var immutable []DriftFinding
	for _, f := range findings {
		if f.Severity == SeverityDriftImmutable {
			immutable = append(immutable, f)
		}
	}
	require.NotEmpty(t, immutable, "component-only divergence must produce drift-immutable; got: %+v", findings)
	hasComponent := false
	for _, f := range immutable {
		if _, ok := f.Detail["component"]; ok {
			hasComponent = true
		}
	}
	require.True(t, hasComponent, "immutable finding must contain 'component' field; got: %+v", immutable)
}

func TestClassifyStreamDrift_MixedMutableAndImmutable_BothFindings(t *testing.T) {
	t.Parallel()

	streamCfg := StreamCfg{
		Name:      "my-stream",
		Subjects:  []string{"work.new.>"}, // mutable drift
		Retention: "workqueue",            // immutable drift
		Storage:   "file",
		Discard:   "old",
	}
	info := buildLiveStreamInfo("my-stream", func(cfg *jetstream.StreamConfig) {
		cfg.Subjects = []string{"work.old.>"}
		cfg.Retention = jetstream.LimitsPolicy
	})

	findings := classifyStreamDrift(streamCfg, info, "prod")

	severities := make(map[string]bool)
	for _, f := range findings {
		severities[f.Severity] = true
	}
	require.True(t, severities[SeverityDriftMutable], "must have drift-mutable")
	require.True(t, severities[SeverityDriftImmutable], "must have drift-immutable")
}

func TestClassifyStreamDrift_NonPartiMetadataPreserved_NoSpuriousDrift(t *testing.T) {
	t.Parallel()

	// An operator-added key (owner=payments) in the live metadata must NOT
	// cause spurious drift — wantedStreamConfig preserves non-Parti keys.
	streamCfg := StreamCfg{
		Name:      "my-stream",
		Subjects:  []string{"work.my-stream.>"},
		Retention: "limits",
		Storage:   "file",
		Discard:   "old",
	}
	info := buildLiveStreamInfo("my-stream", func(cfg *jetstream.StreamConfig) {
		cfg.Subjects = []string{"work.my-stream.>"}
		cfg.Metadata = map[string]string{
			MarkerManagedKey:   MarkerManagedValue,
			MarkerComponentKey: ComponentStream,
			MarkerInstanceKey:  "prod",
			"owner":            "payments", // operator-added key
		}
	})

	findings := classifyStreamDrift(streamCfg, info, "prod")

	require.Len(t, findings, 1)
	require.Equal(t, SeverityInformational, findings[0].Severity, "operator-added metadata key must not cause drift; got: %+v", findings)
}

func TestClassifyStreamDrift_ComponentMismatchIsImmutableNotMasked(t *testing.T) {
	t.Parallel()

	// Verify the component mismatch is NOT filtered out before classify.
	streamCfg := StreamCfg{
		Name:      "my-stream",
		Subjects:  []string{"work.my-stream.>"},
		Retention: "limits",
		Storage:   "file",
		Discard:   "old",
	}
	// Fully managed stream, instance matches, but with wrong component — so
	// the only divergence is the component, isolating the routing under test.
	info := &jetstream.StreamInfo{
		Config: jetstream.StreamConfig{
			Name:     "my-stream",
			Subjects: []string{"work.my-stream.>"},
			Metadata: map[string]string{
				MarkerManagedKey:   MarkerManagedValue,
				MarkerComponentKey: ComponentPartitionSource,
				MarkerInstanceKey:  "prod",
			},
		},
	}

	findings := classifyStreamDrift(streamCfg, info, "prod")

	hasImmutableComponent := false
	for _, f := range findings {
		if f.Severity == SeverityDriftImmutable {
			if _, ok := f.Detail["component"]; ok {
				hasImmutableComponent = true
			}
		}
	}
	require.True(t, hasImmutableComponent, "component mismatch must be drift-immutable; got: %+v", findings)
	for _, f := range findings {
		require.NotEqual(t, SeverityInformational, f.Severity)
	}
}

// --------------------------------------------------------------------------
// buildStreamConfig / wantedStreamConfig / buildStreamUpdateTarget tests
// --------------------------------------------------------------------------

func TestBuildStreamConfig_UnlimitedConvention(t *testing.T) {
	t.Parallel()

	// config 0 → -1 for MaxBytes and MaxMsgs; MaxAge passes through (0 stays 0).
	streamCfg := StreamCfg{
		Name:      "my-stream",
		Subjects:  []string{"work.>"},
		Retention: "limits",
		Storage:   "file",
		Discard:   "old",
		MaxBytes:  0,
		MaxMsgs:   0,
		MaxAge:    0,
	}

	got := buildStreamConfig(streamCfg, "prod")

	require.Equal(t, int64(-1), got.MaxBytes, "MaxBytes 0 must translate to -1")
	require.Equal(t, int64(-1), got.MaxMsgs, "MaxMsgs 0 must translate to -1")
	require.Equal(t, time.Duration(0), got.MaxAge, "MaxAge 0 must stay 0")
}

func TestBuildStreamConfig_MarkerStamped(t *testing.T) {
	t.Parallel()

	streamCfg := StreamCfg{
		Name:      "my-stream",
		Subjects:  []string{"work.>"},
		Retention: "limits",
		Storage:   "file",
		Discard:   "old",
	}

	got := buildStreamConfig(streamCfg, "prod")

	marker := ParseMarker(got.Metadata)
	require.True(t, marker.IsManaged())
	require.Equal(t, ComponentStream, marker.Component)
	require.Equal(t, "prod", marker.Instance)
}

func TestWantedStreamConfig_OverlaysImmutableFields(t *testing.T) {
	t.Parallel()

	// wantedStreamConfig must overlay Storage and Retention (not inherit from live).
	streamCfg := StreamCfg{
		Name:      "my-stream",
		Subjects:  []string{"work.>"},
		Retention: "workqueue",
		Storage:   "memory",
		Discard:   "old",
	}
	live := jetstream.StreamConfig{
		Name:      "my-stream",
		Subjects:  []string{"work.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.FileStorage,
		Metadata:  BuildMarker(ComponentStream, "prod"),
	}

	wanted := wantedStreamConfig(streamCfg, "prod", live)

	require.Equal(t, jetstream.WorkQueuePolicy, wanted.Retention, "wantedStreamConfig must overlay Retention")
	require.Equal(t, jetstream.MemoryStorage, wanted.Storage, "wantedStreamConfig must overlay Storage")
}

func TestBuildStreamUpdateTarget_InheritsImmutableFields(t *testing.T) {
	t.Parallel()

	// buildStreamUpdateTarget must inherit Storage and Retention from live.
	streamCfg := StreamCfg{
		Name:      "my-stream",
		Subjects:  []string{"work.new.>"},
		Retention: "workqueue", // desired: workqueue — but must be ignored
		Storage:   "memory",    // desired: memory — but must be ignored
		Discard:   "old",
	}
	live := jetstream.StreamConfig{
		Name:      "my-stream",
		Subjects:  []string{"work.old.>"},
		Retention: jetstream.LimitsPolicy, // live: limits
		Storage:   jetstream.FileStorage,  // live: file
		Metadata:  BuildMarker(ComponentStream, "prod"),
	}

	target := buildStreamUpdateTarget(streamCfg, "prod", live)

	require.Equal(t, jetstream.LimitsPolicy, target.Retention, "buildStreamUpdateTarget must inherit Retention from live")
	require.Equal(t, jetstream.FileStorage, target.Storage, "buildStreamUpdateTarget must inherit Storage from live")
	require.Equal(t, []string{"work.new.>"}, target.Subjects, "buildStreamUpdateTarget must overwrite Subjects")
}

func TestBuildStreamUpdateTarget_ComponentPreserved(t *testing.T) {
	t.Parallel()

	// mergeUpdateKVMetadata preserves parti.io/component from live.
	streamCfg := StreamCfg{
		Name:     "my-stream",
		Subjects: []string{"work.>"},
		Storage:  "file",
	}
	live := jetstream.StreamConfig{
		Name:     "my-stream",
		Subjects: []string{"work.>"},
		Metadata: map[string]string{
			MarkerManagedKey:   MarkerManagedValue,
			MarkerComponentKey: ComponentStream,
			MarkerInstanceKey:  "prod",
		},
	}

	target := buildStreamUpdateTarget(streamCfg, "prod", live)

	require.Equal(t, ComponentStream, target.Metadata[MarkerComponentKey])
}

// --------------------------------------------------------------------------
// Marker-related classifier tests
// --------------------------------------------------------------------------

func TestClassifyStreamDrift_MutableMarkerDrift_InstanceChange(t *testing.T) {
	t.Parallel()

	streamCfg := StreamCfg{
		Name:      "my-stream",
		Subjects:  []string{"work.my-stream.>"},
		Retention: "limits",
		Storage:   "file",
		Discard:   "old",
	}
	// Stream marked for "staging" but config expects "prod".
	info := buildLiveStreamInfo("my-stream", func(cfg *jetstream.StreamConfig) {
		cfg.Subjects = []string{"work.my-stream.>"}
		cfg.Metadata = map[string]string{
			MarkerManagedKey:   MarkerManagedValue,
			MarkerComponentKey: ComponentStream,
			MarkerInstanceKey:  "staging", // wrong instance
		}
	})

	findings := classifyStreamDrift(streamCfg, info, "prod")

	hasMutableInstance := false
	for _, f := range findings {
		if f.Severity == SeverityDriftMutable {
			if _, ok := f.Detail["instance"]; ok {
				hasMutableInstance = true
			}
		}
	}
	require.True(t, hasMutableInstance, "instance mismatch must be drift-mutable; got: %+v", findings)
}

func TestClassifyStreamDrift_SafeUpdate_MutableDrift_UpdateStreamEmittedExternally(t *testing.T) {
	t.Parallel()

	// Verify that buildStreamUpdateTarget + streamConfigsEqual agree on
	// whether an update-stream action should be emitted (the logic planStreams
	// uses). A mutable-only drift should yield a non-equal update target.
	streamCfg := StreamCfg{
		Name:      "my-stream",
		Subjects:  []string{"work.new.>"},
		Retention: "limits",
		Storage:   "file",
		Discard:   "old",
		MaxAge:    30 * time.Second,
	}
	live := jetstream.StreamConfig{
		Name:     "my-stream",
		Subjects: []string{"work.old.>"},
		MaxAge:   0,
		Metadata: BuildMarker(ComponentStream, "prod"),
	}

	target := buildStreamUpdateTarget(streamCfg, "prod", live)
	equal := streamConfigsEqual(live, target)
	require.False(t, equal, "mutable drift must yield unequal update target")
}

// --------------------------------------------------------------------------
// Deterministic (Kind, Name) ordering test
// --------------------------------------------------------------------------

func TestSortActions_StreamKindsOrderedCorrectly(t *testing.T) {
	t.Parallel()

	// Verify that all action kinds — including the recreate-* kinds added in
	// W2 — sort correctly by (Kind, Name). Expected ASCII order:
	// create-kv < create-stream < recreate-consumer < recreate-kv < recreate-stream
	//   < stamp-marker < stamp-stream-marker < update-kv < update-stream
	actions := []PlannedAction{
		{Kind: ActionUpdateStream, Name: "b"},
		{Kind: ActionCreateKV, Name: "a"},
		{Kind: ActionStampStreamMarker, Name: "a"},
		{Kind: ActionCreateStream, Name: "a"},
		{Kind: ActionUpdateKV, Name: "a"},
		{Kind: ActionStampMarker, Name: "a"},
		{Kind: ActionRecreateKV, Name: "a"},
		{Kind: ActionRecreateStream, Name: "a"},
		{Kind: ActionRecreateConsumer, Name: "a"},
	}

	sortActions(actions)

	kinds := make([]string, len(actions))
	for i, a := range actions {
		kinds[i] = a.Kind
	}

	require.Equal(t, []string{
		ActionCreateKV,
		ActionCreateStream,
		ActionRecreateConsumer,
		ActionRecreateKV,
		ActionRecreateStream,
		ActionStampMarker,
		ActionStampStreamMarker,
		ActionUpdateKV,
		ActionUpdateStream,
	}, kinds)
}

func TestSortDrift_ApplicationStreamKindOrdering(t *testing.T) {
	t.Parallel()

	// application-stream < control-plane-kv < partition-records < partition-source-kv
	drift := []DriftFinding{
		{Kind: KindPartitionSource, Name: "a"},
		{Kind: KindControlPlaneKV, Name: "a"},
		{Kind: KindApplicationStream, Name: "b"},
		{Kind: KindPartitionRecords, Name: "a"},
		{Kind: KindApplicationStream, Name: "a"},
	}

	sortDrift(drift)

	kinds := make([]string, len(drift))
	for i, d := range drift {
		kinds[i] = d.Kind
	}

	require.Equal(t, []string{
		KindApplicationStream,
		KindApplicationStream,
		KindControlPlaneKV,
		KindPartitionRecords,
		KindPartitionSource,
	}, kinds)
}

func TestSortDrift_ApplicationStreamByName(t *testing.T) {
	t.Parallel()

	drift := []DriftFinding{
		{Kind: KindApplicationStream, Name: "z-stream"},
		{Kind: KindApplicationStream, Name: "a-stream"},
		{Kind: KindApplicationStream, Name: "m-stream"},
	}

	sortDrift(drift)

	require.Equal(t, "a-stream", drift[0].Name)
	require.Equal(t, "m-stream", drift[1].Name)
	require.Equal(t, "z-stream", drift[2].Name)
}

// --------------------------------------------------------------------------
// newStampStreamMarkerAction test
// --------------------------------------------------------------------------

func TestNewStampStreamMarkerAction_PartiKeysCorrect(t *testing.T) {
	t.Parallel()

	live := jetstream.StreamConfig{
		Name:     "my-stream",
		Metadata: map[string]string{"owner": "payments"}, // unmarked, but has an operator key
	}

	action := newStampStreamMarkerAction("my-stream", "prod", live)

	require.Equal(t, ActionStampStreamMarker, action.Kind)
	require.Equal(t, "my-stream", action.Name)

	res, ok := action.Resource.(*StreamStampMarkerResource)
	require.True(t, ok)
	require.Equal(t, "my-stream", res.Stream)
	require.Contains(t, res.MergedMetadata, MarkerManagedKey)
	require.Contains(t, res.MergedMetadata, MarkerComponentKey)
	require.Contains(t, res.MergedMetadata, MarkerInstanceKey)
	require.Equal(t, "payments", res.MergedMetadata["owner"], "non-Parti key must be preserved")
	// PartiKeys must include the three marker keys added/changed.
	require.Contains(t, res.PartiKeys, MarkerManagedKey)
	require.Contains(t, res.PartiKeys, MarkerComponentKey)
	require.Contains(t, res.PartiKeys, MarkerInstanceKey)
	// Must not include the non-Parti key.
	for _, k := range res.PartiKeys {
		require.NotEqual(t, "owner", k)
	}
}

// --------------------------------------------------------------------------
// normalizeStreamLimit test
// --------------------------------------------------------------------------

func TestNormalizeStreamLimit(t *testing.T) {
	t.Parallel()

	require.Equal(t, int64(-1), normalizeStreamLimit(0))
	require.Equal(t, int64(-1), normalizeStreamLimit(-1))
	require.Equal(t, int64(1024), normalizeStreamLimit(1024))
	require.Equal(t, int64(512), normalizeStreamLimit(512))
}

// --------------------------------------------------------------------------
// recreate-stream plan emission (W2)
// --------------------------------------------------------------------------

func recreateStreamActions(p PlanResult) []PlannedAction {
	var out []PlannedAction
	for _, a := range p.Actions {
		if a.Kind == ActionRecreateStream {
			out = append(out, a)
		}
	}

	return out
}

func updateStreamActions(p PlanResult) []PlannedAction {
	var out []PlannedAction
	for _, a := range p.Actions {
		if a.Kind == ActionUpdateStream {
			out = append(out, a)
		}
	}

	return out
}

// streamForceCfg returns a force-policy Config with one stream that has
// AllowDeleteRecreate enabled.
func streamForceCfg(allowDR bool) Config {
	return Config{
		APIVersion: APIVersionV1,
		Instance:   "prod",
		Policy:     PolicyForce,
		Streams: []StreamCfg{{
			Name:                "my-stream",
			Subjects:            []string{"work.my-stream.>"},
			Retention:           "limits",
			Storage:             "file",
			Discard:             "old",
			AllowDeleteRecreate: allowDR,
		}},
	}
}

// buildLiveStreamWithImmutableDrift returns a fakeJS streams map with a
// marked my-stream that has storage drift (immutable).
func buildLiveStreamWithImmutableDrift() map[string]jetstream.StreamConfig {
	return map[string]jetstream.StreamConfig{
		"my-stream": {
			Name:     "my-stream",
			Subjects: []string{"work.my-stream.>"},
			Storage:  jetstream.MemoryStorage, // want "file" → immutable drift
			Metadata: BuildMarker(ComponentStream, "prod"),
		},
	}
}

// TestRecreateStream_Force_Marked_ImmutableOnly_EmitsRecreate verifies the
// primary gate: force + AllowDeleteRecreate:true + marked + immutable-only drift
// → exactly one recreate-stream, zero update-stream, drift-immutable in Drift.
func TestRecreateStream_Force_Marked_ImmutableOnly_EmitsRecreate(t *testing.T) {
	t.Parallel()

	cfg := streamForceCfg(true)
	plan := planWith(t, cfg, buildLiveStreamWithImmutableDrift())

	recreates := recreateStreamActions(plan)
	require.Len(t, recreates, 1, "exactly one recreate-stream expected")
	require.Equal(t, "my-stream", recreates[0].Name)

	res, ok := recreates[0].Resource.(*RecreateStreamResource)
	require.True(t, ok, "Resource must be *RecreateStreamResource")
	require.NotNil(t, res.ImmutableDrift)
	require.Contains(t, res.ImmutableDrift, "storage")

	// Drift finding must still coexist.
	var hasImmutableDrift bool
	for _, d := range plan.Drift {
		if d.Name == "my-stream" && d.Severity == SeverityDriftImmutable {
			hasImmutableDrift = true
			require.Equal(t, res.ImmutableDrift, d.Detail,
				"RecreateStreamResource.ImmutableDrift must equal drift-immutable finding's Detail")
		}
	}
	require.True(t, hasImmutableDrift, "drift-immutable finding must coexist with recreate-stream")
	require.Empty(t, updateStreamActions(plan), "recreate-stream and update-stream must be mutually exclusive")
}

// TestRecreateStream_Force_Marked_BothDrifts_RecreateExcludesUpdate verifies
// mutual exclusion when both immutable AND mutable drift are present.
func TestRecreateStream_Force_Marked_BothDrifts_RecreateExcludesUpdate(t *testing.T) {
	t.Parallel()

	cfg := streamForceCfg(true)
	// Live has storage drift (immutable) and different subjects (mutable).
	streams := map[string]jetstream.StreamConfig{
		"my-stream": {
			Name:     "my-stream",
			Subjects: []string{"work.old.>"},  // mutable drift
			Storage:  jetstream.MemoryStorage, // immutable drift
			Metadata: BuildMarker(ComponentStream, "prod"),
		},
	}
	plan := planWith(t, cfg, streams)

	require.Len(t, recreateStreamActions(plan), 1, "exactly one recreate-stream despite both drift types")
	require.Empty(t, updateStreamActions(plan), "recreate-stream must suppress update-stream (mutual exclusion)")

	var hasImmutable, hasMutable bool
	for _, d := range plan.Drift {
		if d.Name == "my-stream" {
			switch d.Severity {
			case SeverityDriftImmutable:
				hasImmutable = true
			case SeverityDriftMutable:
				hasMutable = true
			}
		}
	}
	require.True(t, hasImmutable, "drift-immutable finding must coexist")
	require.True(t, hasMutable, "drift-mutable finding must coexist")
}

// TestRecreateStream_Force_AllowDRFalse_NoRecreate verifies AllowDeleteRecreate:false
// suppresses recreate-stream even under force + immutable drift.
func TestRecreateStream_Force_AllowDRFalse_NoRecreate(t *testing.T) {
	t.Parallel()

	cfg := streamForceCfg(false) // AllowDeleteRecreate:false
	plan := planWith(t, cfg, buildLiveStreamWithImmutableDrift())

	require.Empty(t, recreateStreamActions(plan), "AllowDeleteRecreate:false must suppress recreate-stream")

	var hasImmutableDrift bool
	for _, d := range plan.Drift {
		if d.Name == "my-stream" && d.Severity == SeverityDriftImmutable {
			hasImmutableDrift = true
		}
	}
	require.True(t, hasImmutableDrift, "drift-immutable finding must surface even without recreate-stream")
}

// TestRecreateStream_SafeUpdate_NoRecreate verifies that safe-update policy
// suppresses recreate-stream even with AllowDeleteRecreate:true + immutable drift.
func TestRecreateStream_SafeUpdate_NoRecreate(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		Instance:   "prod",
		Policy:     PolicySafeUpdate,
		Streams: []StreamCfg{{
			Name:                "my-stream",
			Subjects:            []string{"work.my-stream.>"},
			Storage:             "file",
			AllowDeleteRecreate: true,
		}},
	}
	plan := planWith(t, cfg, buildLiveStreamWithImmutableDrift())

	require.Empty(t, recreateStreamActions(plan), "safe-update must not emit recreate-stream")

	var hasImmutableDrift bool
	for _, d := range plan.Drift {
		if d.Name == "my-stream" && d.Severity == SeverityDriftImmutable {
			hasImmutableDrift = true
		}
	}
	require.True(t, hasImmutableDrift, "drift-immutable finding must surface under safe-update")
}

// TestRecreateStream_Force_Unmarked_NoRecreate verifies that an unmarked stream
// does not trigger recreate-stream (ownership not proven).
func TestRecreateStream_Force_Unmarked_NoRecreate(t *testing.T) {
	t.Parallel()

	cfg := streamForceCfg(true)
	streams := map[string]jetstream.StreamConfig{
		"my-stream": {
			Name:     "my-stream",
			Subjects: []string{"work.my-stream.>"},
			Storage:  jetstream.MemoryStorage, // immutable drift but ownership not proven
			// Metadata: nil → unmarked
		},
	}
	plan := planWith(t, cfg, streams)

	require.Empty(t, recreateStreamActions(plan), "unmarked stream must not trigger recreate-stream")

	var hasAdopted bool
	for _, d := range plan.Drift {
		if d.Name == "my-stream" && d.Severity == SeverityAdopted {
			hasAdopted = true
		}
	}
	require.True(t, hasAdopted, "unmarked stream must surface adopted drift")
}

// TestRecreateStream_Force_MutableOnlyDrift_EmitsUpdateStream verifies that
// force + AllowDeleteRecreate:true + only mutable drift → update-stream, no recreate-stream.
// This is the "force ⊇ safe-update" invariant.
func TestRecreateStream_Force_MutableOnlyDrift_EmitsUpdateStream(t *testing.T) {
	t.Parallel()

	cfg := streamForceCfg(true)
	// Live stream has different subjects (mutable) but same storage (no immutable drift).
	streams := map[string]jetstream.StreamConfig{
		"my-stream": {
			Name:     "my-stream",
			Subjects: []string{"work.old.>"}, // mutable drift
			Storage:  jetstream.FileStorage,  // matches config → no immutable drift
			Metadata: BuildMarker(ComponentStream, "prod"),
		},
	}
	plan := planWith(t, cfg, streams)

	require.Empty(t, recreateStreamActions(plan), "mutable-only drift must not emit recreate-stream")
	require.NotEmpty(t, updateStreamActions(plan), "mutable drift under force must emit update-stream")
}

// TestRecreateStream_Determinism verifies that two identical planWith calls
// produce byte-equal PlanResults.
func TestRecreateStream_Determinism(t *testing.T) {
	t.Parallel()

	cfg := streamForceCfg(true)
	cfg.Instance = "staging" // mutable drift too
	streams := map[string]jetstream.StreamConfig{
		"my-stream": {
			Name:     "my-stream",
			Subjects: []string{"work.my-stream.>"},
			Storage:  jetstream.MemoryStorage, // immutable drift
			Metadata: BuildMarker(ComponentStream, "staging"),
		},
	}

	plan1 := planWith(t, cfg, streams)
	plan2 := planWith(t, cfg, streams)

	require.Equal(t, plan1.Actions, plan2.Actions, "action list must be deterministic")
	require.Equal(t, plan1.Drift, plan2.Drift, "drift list must be deterministic")
}
