package provision

// Same-package unit tests for the update-kv plan-emission and apply
// helpers. These use synthetic *jetstream.StreamInfo / StreamConfig
// values so they run without a live NATS server.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --- streamConfigToKVConfig --------------------------------------------------

func TestStreamConfigToKVConfig_PopulatesEveryField(t *testing.T) {
	t.Parallel()

	sc := jetstream.StreamConfig{
		Name:                   "KV_my-bucket",
		Description:            "owned by team X",
		MaxMsgSize:             4096,
		MaxMsgsPerSubject:      3,
		MaxAge:                 7 * time.Minute,
		MaxBytes:               1 << 20,
		Storage:                jetstream.FileStorage,
		Replicas:               3,
		Placement:              &jetstream.Placement{Cluster: "c1", Tags: []string{"t1"}},
		RePublish:              &jetstream.RePublish{Source: "a", Destination: "b"},
		Mirror:                 &jetstream.StreamSource{Name: "src"},
		Sources:                []*jetstream.StreamSource{{Name: "s1"}},
		Compression:            jetstream.S2Compression,
		SubjectDeleteMarkerTTL: 9 * time.Second,
		Metadata:               map[string]string{"k": "v"},
	}

	kv := streamConfigToKVConfig(sc)

	require.Equal(t, "my-bucket", kv.Bucket)
	require.Equal(t, "owned by team X", kv.Description)
	require.Equal(t, int32(4096), kv.MaxValueSize)
	require.Equal(t, uint8(3), kv.History)
	require.Equal(t, 7*time.Minute, kv.TTL)
	require.Equal(t, int64(1<<20), kv.MaxBytes)
	require.Equal(t, jetstream.FileStorage, kv.Storage)
	require.Equal(t, 3, kv.Replicas)
	require.Equal(t, sc.Placement, kv.Placement)
	require.Equal(t, sc.RePublish, kv.RePublish)
	require.Equal(t, sc.Mirror, kv.Mirror)
	require.Equal(t, sc.Sources, kv.Sources)
	require.True(t, kv.Compression)
	require.Equal(t, 9*time.Second, kv.LimitMarkerTTL)
	require.Equal(t, map[string]string{"k": "v"}, kv.Metadata)
}

func TestStreamConfigToKVConfig_NoCompression_MapsToFalse(t *testing.T) {
	t.Parallel()

	kv := streamConfigToKVConfig(jetstream.StreamConfig{Name: "KV_b", Compression: jetstream.NoCompression})
	require.False(t, kv.Compression)
}

// TestExtractLiveKVConfig_DelegationIsBehaviorPreserving guards that
// extractLiveKVConfig, now delegating to streamConfigToKVConfig, still
// yields the same comparison-subset values kvConfigsEqual reads.
func TestExtractLiveKVConfig_DelegationIsBehaviorPreserving(t *testing.T) {
	t.Parallel()

	sc := jetstream.StreamConfig{
		Name:              "KV_b",
		MaxMsgSize:        2048,
		MaxMsgsPerSubject: 1,
		MaxAge:            time.Minute,
		Storage:           jetstream.MemoryStorage,
		Replicas:          1,
		Metadata:          map[string]string{"x": "y"},
	}
	live := extractLiveKVConfig(&sc)

	require.True(t, kvConfigsEqual(live, jetstream.KeyValueConfig{
		MaxValueSize: 2048,
		History:      1,
		TTL:          time.Minute,
		Storage:      jetstream.MemoryStorage,
		Replicas:     1,
		Metadata:     map[string]string{"x": "y"},
	}))
}

// --- mergeUpdateKVMetadata ---------------------------------------------------

func TestMergeUpdateKVMetadata_ForcesManagedAndSetsInstance(t *testing.T) {
	t.Parallel()

	merged := mergeUpdateKVMetadata(map[string]string{MarkerManagedKey: "v0"}, "prod")
	require.Equal(t, MarkerManagedValue, merged[MarkerManagedKey])
	require.Equal(t, "prod", merged[MarkerInstanceKey])
}

func TestMergeUpdateKVMetadata_EmptyInstanceDeletesKey(t *testing.T) {
	t.Parallel()

	merged := mergeUpdateKVMetadata(map[string]string{MarkerInstanceKey: "old"}, "")
	_, ok := merged[MarkerInstanceKey]
	require.False(t, ok)
}

func TestMergeUpdateKVMetadata_PreservesMismatchedComponentVerbatim(t *testing.T) {
	t.Parallel()

	live := map[string]string{
		MarkerManagedKey:   "v1",
		MarkerComponentKey: ComponentPartitionSource, // mismatched on purpose
		"team":             "infra",
	}
	merged := mergeUpdateKVMetadata(live, "prod")
	require.Equal(t, ComponentPartitionSource, merged[MarkerComponentKey],
		"update-kv must never re-label a bucket's component")
	require.Equal(t, "infra", merged["team"], "non-Parti keys must be preserved")
}

func TestMergeUpdateKVMetadata_NilLiveDoesNotPanic(t *testing.T) {
	t.Parallel()

	merged := mergeUpdateKVMetadata(nil, "prod")
	require.Equal(t, MarkerManagedValue, merged[MarkerManagedKey])
	require.Equal(t, "prod", merged[MarkerInstanceKey])
}

// --- buildUpdateKVTarget -----------------------------------------------------

func TestBuildControlPlaneUpdateTarget_PreservesNonOperatorFields(t *testing.T) {
	t.Parallel()

	before := jetstream.KeyValueConfig{
		Bucket:         "parti-election",
		Description:    "out-of-band note",
		MaxValueSize:   8192,
		History:        3,
		MaxBytes:       4096,
		Storage:        jetstream.MemoryStorage,
		Placement:      &jetstream.Placement{Cluster: "c"},
		Compression:    true,
		LimitMarkerTTL: 5 * time.Second,
		TTL:            time.Minute,
		Replicas:       1,
		Metadata:       map[string]string{MarkerManagedKey: "v1", MarkerComponentKey: ComponentControlPlaneElection},
	}
	spec := controlPlaneSpec{
		component: ComponentControlPlaneElection,
		bucket:    "parti-election",
		ttl:       2 * time.Minute,
		storage:   jetstream.MemoryStorage,
		replicas:  3,
	}
	after := buildControlPlaneUpdateTarget(spec, "prod", before)

	// Operator-expressible fields change.
	require.Equal(t, 2*time.Minute, after.TTL)
	require.Equal(t, 3, after.Replicas)
	require.Equal(t, "prod", after.Metadata[MarkerInstanceKey])

	// Every other field is inherited verbatim.
	require.Equal(t, before.Bucket, after.Bucket)
	require.True(t, reflect.DeepEqual(before.Description, after.Description))
	require.Equal(t, before.MaxValueSize, after.MaxValueSize) // CP: preserved-from-live
	require.Equal(t, before.History, after.History)
	require.Equal(t, before.MaxBytes, after.MaxBytes)
	require.Equal(t, before.Storage, after.Storage)
	require.True(t, reflect.DeepEqual(before.Placement, after.Placement))
	require.Equal(t, before.Compression, after.Compression)
	require.Equal(t, before.LimitMarkerTTL, after.LimitMarkerTTL)
}

func TestBuildPartitionSourceUpdateTarget_OverwritesMaxValueSize(t *testing.T) {
	t.Parallel()

	before := jetstream.KeyValueConfig{
		Bucket:       "parti-source",
		History:      2,
		Storage:      jetstream.FileStorage,
		MaxValueSize: 1024,
		TTL:          time.Minute,
		Replicas:     1,
		Metadata:     map[string]string{MarkerManagedKey: "v1", MarkerComponentKey: ComponentPartitionSource},
	}
	ps := &PartitionSourceConfig{
		Bucket:       "parti-source",
		History:      2,
		Storage:      "file",
		MaxValueSize: 65536,
		TTL:          3 * time.Minute,
		Replicas:     2,
	}
	after := buildPartitionSourceUpdateTarget(ps, "prod", before)

	require.Equal(t, int32(65536), after.MaxValueSize)
	require.Equal(t, 3*time.Minute, after.TTL)
	require.Equal(t, 2, after.Replicas)
	require.Equal(t, before.History, after.History)
	require.Equal(t, before.Storage, after.Storage)
}

// --- plan emission -----------------------------------------------------------

func cpUpdateCfg(policy ReconcilePolicy) Config {
	return Config{
		APIVersion: APIVersionV1,
		Instance:   "prod",
		Policy:     policy,
		ControlPlane: &ControlPlaneConfig{
			StableIDBucket:   "parti-stableid",
			ElectionBucket:   "parti-election",
			HeartbeatBucket:  "parti-heartbeat",
			AssignmentBucket: "parti-assignment",
			HandoffBucket:    "parti-handoff",
			ElectionTimeout:  10 * time.Second,
			HeartbeatTTL:     15 * time.Second,
			WorkerIDTTL:      75 * time.Second,
			AssignmentTTL:    0,
		},
	}
}

// fakeJS lets plan-emission unit tests inject one synthetic stream.
type fakeJS struct {
	jetstream.JetStream
	streams map[string]jetstream.StreamConfig // streamName -> config
}

func (f *fakeJS) Stream(_ context.Context, name string) (jetstream.Stream, error) {
	cfg, ok := f.streams[name]
	if !ok {
		return nil, jetstream.ErrStreamNotFound
	}

	return &fakeStream{cfg: cfg}, nil
}

type fakeStream struct {
	jetstream.Stream
	cfg jetstream.StreamConfig
}

func (s *fakeStream) Info(_ context.Context, _ ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error) {
	return &jetstream.StreamInfo{Config: s.cfg}, nil
}

// electionStream returns a synthetic KV_parti-election stream config.
func electionStream(mutate ...func(*jetstream.StreamConfig)) jetstream.StreamConfig {
	cfg := jetstream.StreamConfig{
		Name:              "KV_parti-election",
		Storage:           jetstream.MemoryStorage,
		MaxMsgsPerSubject: 1,
		MaxAge:            10 * time.Second,
		Metadata:          BuildMarker(ComponentControlPlaneElection, "prod"),
	}
	for _, m := range mutate {
		m(&cfg)
	}

	return cfg
}

func planWith(t *testing.T, cfg Config, streams map[string]jetstream.StreamConfig) PlanResult {
	t.Helper()
	js := &fakeJS{streams: streams}
	out, err := Plan(context.Background(), js, cfg)
	require.NoError(t, err)

	return out
}

func updateKVActions(p PlanResult) []PlannedAction {
	var out []PlannedAction
	for _, a := range p.Actions {
		if a.Kind == ActionUpdateKV {
			out = append(out, a)
		}
	}

	return out
}

func TestPlanEmission_SafeUpdate_TTLDrift_EmitsUpdateKV(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(func(c *jetstream.StreamConfig) {
			c.MaxAge = 99 * time.Second // drifts from cfg's 10s
		}),
	}
	actions := updateKVActions(planWith(t, cfg, streams))
	require.Len(t, actions, 1)
	require.Equal(t, "parti-election", actions[0].Name)
	res, ok := actions[0].Resource.(*UpdateKVResource)
	require.True(t, ok)
	require.Equal(t, 10*time.Second, res.After.TTL)
}

func TestPlanEmission_SafeUpdate_ReplicasDrift_EmitsUpdateKV(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	cfg.ControlPlane.Replicas = 3
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(func(c *jetstream.StreamConfig) { c.Replicas = 1 }),
	}
	actions := updateKVActions(planWith(t, cfg, streams))
	require.Len(t, actions, 1)
}

func TestPlanEmission_SafeUpdate_PartitionSourceReplicasDrift_EmitsUpdateKV(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		Instance:   "prod",
		Policy:     PolicySafeUpdate,
		PartitionSource: &PartitionSourceConfig{
			Bucket:   "parti-partitions",
			Key:      "partitions/v1",
			Storage:  "file",
			History:  1,
			Replicas: 3,
		},
	}
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-partitions": {
			Name:              "KV_parti-partitions",
			Storage:           jetstream.FileStorage,
			MaxMsgsPerSubject: 1,
			Replicas:          1, // drifts from cfg's 3
			Metadata:          BuildMarker(ComponentPartitionSource, "prod"),
		},
	}
	actions := updateKVActions(planWith(t, cfg, streams))
	require.Len(t, actions, 1)
	require.Equal(t, "parti-partitions", actions[0].Name)
	res, ok := actions[0].Resource.(*UpdateKVResource)
	require.True(t, ok)
	require.Equal(t, 3, res.After.Replicas)
}

func TestPlanEmission_SafeUpdate_InstanceChange_EmitsUpdateKV(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	cfg.Instance = "staging"
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(), // marked instance=prod
	}
	actions := updateKVActions(planWith(t, cfg, streams))
	require.Len(t, actions, 1)
}

func TestPlanEmission_SafeUpdate_InstanceRemoval_EmitsUpdateKV(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	cfg.Instance = ""
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(), // marked instance=prod
	}
	actions := updateKVActions(planWith(t, cfg, streams))
	require.Len(t, actions, 1)
	res, ok := actions[0].Resource.(*UpdateKVResource)
	require.True(t, ok)
	_, ok = res.After.Metadata[MarkerInstanceKey]
	require.False(t, ok)
}

func TestPlanEmission_SafeUpdate_ManagedVersionDrift_EmitsUpdateKV(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(func(c *jetstream.StreamConfig) {
			c.Metadata[MarkerManagedKey] = "v0"
		}),
	}
	actions := updateKVActions(planWith(t, cfg, streams))
	require.Len(t, actions, 1)
}

func TestPlanEmission_SafeUpdate_UnmarkedBucket_NoUpdateKV(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(func(c *jetstream.StreamConfig) {
			c.Metadata = nil // unmarked
			c.MaxAge = 99 * time.Second
		}),
	}
	plan := planWith(t, cfg, streams)
	require.Empty(t, updateKVActions(plan))
	var adopted bool
	for _, d := range plan.Drift {
		if d.Severity == SeverityAdopted && d.Name == "parti-election" {
			adopted = true
		}
	}
	require.True(t, adopted, "unmarked bucket still surfaces adopted drift")
}

func TestPlanEmission_SafeUpdate_ImmutableOnlyDrift_NoUpdateKV(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(func(c *jetstream.StreamConfig) {
			c.MaxMsgsPerSubject = 3 // History drift only — immutable
		}),
	}
	plan := planWith(t, cfg, streams)
	require.Empty(t, updateKVActions(plan), "History drift alone is immutable, no update-kv")
}

func TestPlanEmission_SafeUpdate_ComponentOnlyDrift_NoUpdateKV(t *testing.T) {
	t.Parallel()

	// A bucket whose ONLY drift is a mismatched parti.io/component.
	// Component is drift-immutable; update-kv never re-labels a bucket's
	// role. This guards the mergeUpdateKVMetadata asymmetry: it preserves
	// the live component, so before == after and no action is emitted.
	cfg := cpUpdateCfg(PolicySafeUpdate)
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(func(c *jetstream.StreamConfig) {
			c.Metadata[MarkerComponentKey] = ComponentPartitionSource // mismatched
		}),
	}
	plan := planWith(t, cfg, streams)
	require.Empty(t, updateKVActions(plan),
		"component drift alone is immutable; update-kv must not re-label the bucket")
	var immutable bool
	for _, d := range plan.Drift {
		if d.Name == "parti-election" && d.Severity == SeverityDriftImmutable {
			immutable = true
		}
	}
	require.True(t, immutable, "component drift still surfaces a drift-immutable finding")
}

func TestPlanEmission_SafeUpdate_InSyncBucket_NoUpdateKV(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(),
	}
	require.Empty(t, updateKVActions(planWith(t, cfg, streams)))
}

func TestPlanEmission_Warn_NoUpdateKV(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyWarn)
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(func(c *jetstream.StreamConfig) { c.MaxAge = 99 * time.Second }),
	}
	require.Empty(t, updateKVActions(planWith(t, cfg, streams)))
}

func TestPlanEmission_Adopt_NoUpdateKV(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyAdopt)
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(func(c *jetstream.StreamConfig) { c.MaxAge = 99 * time.Second }),
	}
	require.Empty(t, updateKVActions(planWith(t, cfg, streams)))
}

func TestPlanEmission_SafeUpdate_BeforePreservesLiveDescriptionAndComponent(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(func(c *jetstream.StreamConfig) {
			c.MaxAge = 99 * time.Second
			c.Description = "owned by team X"
			c.MaxBytes = 4096
			c.Metadata[MarkerComponentKey] = ComponentPartitionSource // mismatched
		}),
	}
	actions := updateKVActions(planWith(t, cfg, streams))
	require.Len(t, actions, 1)
	res, ok := actions[0].Resource.(*UpdateKVResource)
	require.True(t, ok)
	require.Equal(t, "owned by team X", res.After.Description)
	require.Equal(t, int64(4096), res.After.MaxBytes)
	require.Equal(t, ComponentPartitionSource, res.After.Metadata[MarkerComponentKey],
		"update-kv preserves a mismatched live component verbatim")
}

func TestPlanEmission_Determinism_CreateKVSortsBeforeUpdateKV(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	// election exists with drift (-> update-kv); the rest are missing
	// (-> create-kv). Sort is (Kind, Name); "create-kv" < "update-kv".
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(func(c *jetstream.StreamConfig) { c.MaxAge = 99 * time.Second }),
	}
	plan := planWith(t, cfg, streams)
	require.NotEmpty(t, plan.Actions)
	seenUpdate := false
	for _, a := range plan.Actions {
		if a.Kind == ActionUpdateKV {
			seenUpdate = true
		}
		if a.Kind == ActionCreateKV {
			require.False(t, seenUpdate, "all create-kv actions must sort before any update-kv")
		}
	}
}

// --- apply, seam-based -------------------------------------------------------

// fakeStreamReader / fakeKVUpdater drive applyUpdateKVAction deterministically.
type fakeStreamReader struct {
	cfg jetstream.StreamConfig
	err error
}

func (r fakeStreamReader) StreamInfo(_ context.Context, _ string) (*jetstream.StreamInfo, error) {
	if r.err != nil {
		return nil, r.err
	}

	return &jetstream.StreamInfo{Config: r.cfg}, nil
}

type fakeKVUpdater struct {
	err    error
	called bool
	got    jetstream.KeyValueConfig
}

func (u *fakeKVUpdater) UpdateKeyValue(_ context.Context, cfg jetstream.KeyValueConfig) error {
	u.called = true
	u.got = cfg

	return u.err
}

func electionUpdateAction(before jetstream.KeyValueConfig) PlannedAction {
	return PlannedAction{
		Kind:     ActionUpdateKV,
		Name:     "parti-election",
		Resource: &UpdateKVResource{Before: before, After: before},
	}
}

func cpSpecsFor(t *testing.T, cfg Config) map[string]controlPlaneSpec {
	t.Helper()
	m := map[string]controlPlaneSpec{}
	for _, s := range buildControlPlaneSpecs(*cfg.ControlPlane) {
		m[s.bucket] = s
	}

	return m
}

func TestApplyUpdateKV_StaleBefore_FailsFast(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	before := streamConfigToKVConfig(electionStream())
	// Re-read returns a TTL differing from Before -> stale.
	reader := fakeStreamReader{cfg: electionStream(func(c *jetstream.StreamConfig) {
		c.MaxAge = 999 * time.Second
	})}
	updater := &fakeKVUpdater{}

	_, err := applyUpdateKVAction(context.Background(), reader, updater, cfg,
		cpSpecsFor(t, cfg), electionUpdateAction(before))
	require.Error(t, err)
	require.Contains(t, err.Error(), "stale-before")
	require.False(t, updater.called, "no write on stale-before")
}

func TestApplyUpdateKV_NoOpConvergence_RacedTrueNoWrite(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	// A genuinely emitted action: Before carries the stale plan-time TTL,
	// After the desired one. Before != After is what the plan gate
	// guarantees for any emitted update-kv.
	before := streamConfigToKVConfig(electionStream(func(c *jetstream.StreamConfig) {
		c.MaxAge = 99 * time.Second
	}))
	after := streamConfigToKVConfig(electionStream())
	require.False(t, kvConfigsEqual(before, after), "test needs a genuine Before != After")

	// A concurrent operator converged the bucket to the desired state
	// between Plan and Apply: the re-read live equals the target but
	// differs from Before. This must be a raced success, not stale-before.
	reader := fakeStreamReader{cfg: electionStream()}
	updater := &fakeKVUpdater{}
	action := PlannedAction{
		Kind:     ActionUpdateKV,
		Name:     "parti-election",
		Resource: &UpdateKVResource{Before: before, After: after},
	}

	executed, err := applyUpdateKVAction(context.Background(), reader, updater, cfg,
		cpSpecsFor(t, cfg), action)
	require.NoError(t, err)
	require.True(t, executed.Raced, "bucket already at the target is a raced success")
	require.False(t, updater.called, "no write when already converged")
}

func TestApplyUpdateKV_MissingOnReread_FailsFast(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	before := streamConfigToKVConfig(electionStream())
	reader := fakeStreamReader{err: jetstream.ErrStreamNotFound}
	updater := &fakeKVUpdater{}

	_, err := applyUpdateKVAction(context.Background(), reader, updater, cfg,
		cpSpecsFor(t, cfg), electionUpdateAction(before))
	require.Error(t, err)
	require.Contains(t, err.Error(), "bucket-missing-before-update")
	require.False(t, updater.called)
}

func TestApplyUpdateKV_MissingOnWrite_FailsFast(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	// Live drifts on TTL (so a write is needed) but matches Before.
	live := electionStream(func(c *jetstream.StreamConfig) { c.MaxAge = 99 * time.Second })
	before := streamConfigToKVConfig(live)
	reader := fakeStreamReader{cfg: live}
	updater := &fakeKVUpdater{err: jetstream.ErrBucketNotFound}

	_, err := applyUpdateKVAction(context.Background(), reader, updater, cfg,
		cpSpecsFor(t, cfg), electionUpdateAction(before))
	require.Error(t, err)
	require.Contains(t, err.Error(), "bucket-missing-before-update")
	require.True(t, updater.called)
}

func TestApplyUpdateKV_HappyPath_WritesTarget(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	live := electionStream(func(c *jetstream.StreamConfig) { c.MaxAge = 99 * time.Second })
	before := streamConfigToKVConfig(live)
	reader := fakeStreamReader{cfg: live}
	updater := &fakeKVUpdater{}

	executed, err := applyUpdateKVAction(context.Background(), reader, updater, cfg,
		cpSpecsFor(t, cfg), electionUpdateAction(before))
	require.NoError(t, err)
	require.False(t, executed.Raced)
	require.True(t, updater.called)
	require.Equal(t, 10*time.Second, updater.got.TTL, "write target carries the desired TTL")
}

func TestApplyUpdateKV_WrongResourceType_FailsFast(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	action := PlannedAction{Kind: ActionUpdateKV, Name: "parti-election", Resource: "not-a-resource"}
	_, err := applyUpdateKVAction(context.Background(), fakeStreamReader{cfg: electionStream()},
		&fakeKVUpdater{}, cfg, cpSpecsFor(t, cfg), action)
	require.Error(t, err)
	require.Contains(t, err.Error(), "wrong Resource type")
}

func TestApplyUpdateKV_CancelledReread_PropagatesCancellation(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	before := streamConfigToKVConfig(electionStream())
	reader := fakeStreamReader{err: context.Canceled}

	_, err := applyUpdateKVAction(context.Background(), reader, &fakeKVUpdater{}, cfg,
		cpSpecsFor(t, cfg), electionUpdateAction(before))
	require.True(t, errors.Is(err, context.Canceled))
}

// --- recreate-kv plan emission (W2) ------------------------------------------

func recreateKVActions(p PlanResult) []PlannedAction {
	var out []PlannedAction
	for _, a := range p.Actions {
		if a.Kind == ActionRecreateKV {
			out = append(out, a)
		}
	}

	return out
}

// cpForceCfg returns a force-policy Config for the election bucket.
func cpForceCfg() Config {
	cfg := cpUpdateCfg(PolicyForce)
	cfg.ControlPlane.AllowDeleteRecreate = true

	return cfg
}

// electionStreamImmutable returns an election stream config with immutable
// drift (History=3, which is != 1 in config) and the correct marker.
func electionStreamImmutable() jetstream.StreamConfig {
	return electionStream(func(c *jetstream.StreamConfig) {
		c.MaxMsgsPerSubject = 3 // History drift — immutable
	})
}

// TestRecreateKV_ControlPlane_Force_Marked_ImmutableOnly_EmitsRecreate is the
// primary gate test: force + AllowDeleteRecreate:true + marked + immutable-only
// drift → exactly one recreate-kv, zero update-kv, drift-immutable finding in Drift.
func TestRecreateKV_ControlPlane_Force_Marked_ImmutableOnly_EmitsRecreate(t *testing.T) {
	t.Parallel()

	cfg := cpForceCfg()
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStreamImmutable(),
	}
	plan := planWith(t, cfg, streams)

	recreates := recreateKVActions(plan)
	require.Len(t, recreates, 1, "exactly one recreate-kv expected")
	require.Equal(t, "parti-election", recreates[0].Name)

	res, ok := recreates[0].Resource.(*RecreateKVResource)
	require.True(t, ok, "Resource must be *RecreateKVResource")
	require.NotNil(t, res.ImmutableDrift)
	require.Contains(t, res.ImmutableDrift, "history")

	// Drift finding must still surface.
	var hasImmutableDrift bool
	for _, d := range plan.Drift {
		if d.Name == "parti-election" && d.Severity == SeverityDriftImmutable {
			hasImmutableDrift = true
			require.Equal(t, res.ImmutableDrift, d.Detail,
				"RecreateKVResource.ImmutableDrift must equal the drift-immutable finding's Detail")
		}
	}
	require.True(t, hasImmutableDrift, "drift-immutable finding must coexist with recreate-kv action")

	// Mutual exclusion: no update-kv for the same bucket.
	require.Empty(t, updateKVActions(plan), "recreate-kv and update-kv must be mutually exclusive")
}

// TestRecreateKV_ControlPlane_Force_Marked_BothDrifts_RecreateExcludesUpdate
// verifies mutual exclusion when both immutable AND mutable drift are present:
// exactly one recreate-kv, zero update-kv. This is the load-bearing invariant.
func TestRecreateKV_ControlPlane_Force_Marked_BothDrifts_RecreateExcludesUpdate(t *testing.T) {
	t.Parallel()

	cfg := cpForceCfg()
	cfg.Instance = "staging" // mutable instance drift
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStreamImmutable(), // also has immutable drift
	}
	plan := planWith(t, cfg, streams)

	recreates := recreateKVActions(plan)
	require.Len(t, recreates, 1, "exactly one recreate-kv despite both drift types")
	require.Empty(t, updateKVActions(plan), "recreate-kv must suppress update-kv (mutual exclusion)")

	// Both findings must coexist in Drift.
	var hasImmutable, hasMutable bool
	for _, d := range plan.Drift {
		if d.Name == "parti-election" {
			switch d.Severity {
			case SeverityDriftImmutable:
				hasImmutable = true
			case SeverityDriftMutable:
				hasMutable = true
			}
		}
	}
	require.True(t, hasImmutable, "drift-immutable finding must coexist")
	require.True(t, hasMutable, "drift-mutable finding must coexist (both drift types)")
}

// TestRecreateKV_ControlPlane_Force_AllowDRFalse_NoRecreate verifies that
// AllowDeleteRecreate:false suppresses recreate-kv even under force + immutable drift.
func TestRecreateKV_ControlPlane_Force_AllowDRFalse_NoRecreate(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyForce) // AllowDeleteRecreate defaults to false
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStreamImmutable(),
	}
	plan := planWith(t, cfg, streams)

	require.Empty(t, recreateKVActions(plan), "AllowDeleteRecreate:false must suppress recreate-kv")

	// Drift finding must still surface.
	var hasImmutableDrift bool
	for _, d := range plan.Drift {
		if d.Name == "parti-election" && d.Severity == SeverityDriftImmutable {
			hasImmutableDrift = true
		}
	}
	require.True(t, hasImmutableDrift, "drift-immutable finding must surface even without recreate-kv")
}

// TestRecreateKV_ControlPlane_SafeUpdate_NoRecreate verifies that safe-update
// policy suppresses recreate-kv even with AllowDeleteRecreate:true + immutable drift.
func TestRecreateKV_ControlPlane_SafeUpdate_NoRecreate(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicySafeUpdate)
	cfg.ControlPlane.AllowDeleteRecreate = true
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStreamImmutable(),
	}
	plan := planWith(t, cfg, streams)

	require.Empty(t, recreateKVActions(plan), "safe-update must not emit recreate-kv")

	var hasImmutableDrift bool
	for _, d := range plan.Drift {
		if d.Name == "parti-election" && d.Severity == SeverityDriftImmutable {
			hasImmutableDrift = true
		}
	}
	require.True(t, hasImmutableDrift, "drift-immutable finding must surface under safe-update")
}

// TestRecreateKV_ControlPlane_Force_Unmarked_NoRecreate verifies that an
// unmarked bucket does not trigger recreate-kv (ownership not proven).
func TestRecreateKV_ControlPlane_Force_Unmarked_NoRecreate(t *testing.T) {
	t.Parallel()

	cfg := cpForceCfg()
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(func(c *jetstream.StreamConfig) {
			c.Metadata = nil        // unmarked
			c.MaxMsgsPerSubject = 3 // immutable drift (but ownership not proven)
		}),
	}
	plan := planWith(t, cfg, streams)

	require.Empty(t, recreateKVActions(plan), "unmarked bucket must not trigger recreate-kv")

	// Must have adopted drift instead.
	var hasAdopted bool
	for _, d := range plan.Drift {
		if d.Name == "parti-election" && d.Severity == SeverityAdopted {
			hasAdopted = true
		}
	}
	require.True(t, hasAdopted, "unmarked bucket must surface adopted drift")
}

// TestRecreateKV_ControlPlane_Force_MutableOnlyDrift_EmitsUpdateKV verifies
// that force + AllowDeleteRecreate:true + only mutable drift → update-kv, no recreate-kv.
// This is the "force ⊇ safe-update" invariant.
func TestRecreateKV_ControlPlane_Force_MutableOnlyDrift_EmitsUpdateKV(t *testing.T) {
	t.Parallel()

	cfg := cpForceCfg()
	cfg.ControlPlane.ElectionTimeout = 99 * time.Second // mutable TTL drift
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(), // marked, in-sync History — only TTL drifts
	}
	plan := planWith(t, cfg, streams)

	require.Empty(t, recreateKVActions(plan), "mutable-only drift must not emit recreate-kv")
	require.NotEmpty(t, updateKVActions(plan), "mutable drift under force must emit update-kv")
}

// TestRecreateKV_PartitionSource_Force_Marked_ImmutableOnly_EmitsRecreate verifies
// that partition-source immutable history drift under force + AllowDeleteRecreate
// emits exactly one recreate-kv and zero update-kv.
func TestRecreateKV_PartitionSource_Force_Marked_ImmutableOnly_EmitsRecreate(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		Instance:   "prod",
		Policy:     PolicyForce,
		PartitionSource: &PartitionSourceConfig{
			Bucket:              "parti-partitions",
			Key:                 "partitions/v1",
			Storage:             "file",
			History:             1,
			Replicas:            1,
			AllowDeleteRecreate: true,
		},
	}
	// Live bucket has History=3 (immutable drift).
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-partitions": {
			Name:              "KV_parti-partitions",
			Storage:           jetstream.FileStorage,
			MaxMsgsPerSubject: 3, // History=3, want 1
			Replicas:          1,
			Metadata:          BuildMarker(ComponentPartitionSource, "prod"),
		},
	}
	plan := planWith(t, cfg, streams)

	recreates := recreateKVActions(plan)
	require.Len(t, recreates, 1, "exactly one recreate-kv for partition-source")
	require.Equal(t, "parti-partitions", recreates[0].Name)

	res, ok := recreates[0].Resource.(*RecreateKVResource)
	require.True(t, ok, "Resource must be *RecreateKVResource")
	require.Contains(t, res.ImmutableDrift, "history")

	// Drift finding must coexist.
	var hasImmutableDrift bool
	for _, d := range plan.Drift {
		if d.Name == "parti-partitions" && d.Severity == SeverityDriftImmutable {
			hasImmutableDrift = true
			require.Equal(t, res.ImmutableDrift, d.Detail,
				"ImmutableDrift must equal drift-immutable finding's Detail")
		}
	}
	require.True(t, hasImmutableDrift, "drift-immutable finding must coexist with recreate-kv")
	require.Empty(t, updateKVActions(plan), "recreate-kv and update-kv must be mutually exclusive")
}

// TestRecreateKV_PartitionSource_Force_Marked_BothDrifts_RecreateExcludesUpdate
// verifies mutual exclusion for partition-source with both immutable AND mutable drift.
func TestRecreateKV_PartitionSource_Force_Marked_BothDrifts_RecreateExcludesUpdate(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		Instance:   "prod",
		Policy:     PolicyForce,
		PartitionSource: &PartitionSourceConfig{
			Bucket:              "parti-partitions",
			Key:                 "partitions/v1",
			Storage:             "file",
			History:             1,
			Replicas:            3, // desired 3 → mutable drift against live 1
			AllowDeleteRecreate: true,
		},
	}
	// Live has History=5 (immutable) and Replicas=1 (mutable drift against desired 3).
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-partitions": {
			Name:              "KV_parti-partitions",
			Storage:           jetstream.FileStorage,
			MaxMsgsPerSubject: 5, // History=5, want 1 → immutable
			Replicas:          1, // want 3 → mutable
			Metadata:          BuildMarker(ComponentPartitionSource, "prod"),
		},
	}
	plan := planWith(t, cfg, streams)

	require.Len(t, recreateKVActions(plan), 1, "exactly one recreate-kv despite both drift types")
	require.Empty(t, updateKVActions(plan), "recreate-kv must suppress update-kv (mutual exclusion)")
}

// TestRecreateKV_PartitionSource_Force_AllowDRFalse_NoRecreate verifies that
// AllowDeleteRecreate:false suppresses recreate-kv for partition-source.
func TestRecreateKV_PartitionSource_Force_AllowDRFalse_NoRecreate(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		Instance:   "prod",
		Policy:     PolicyForce,
		PartitionSource: &PartitionSourceConfig{
			Bucket:  "parti-partitions",
			Key:     "partitions/v1",
			Storage: "file",
			History: 1,
			// AllowDeleteRecreate defaults to false
		},
	}
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-partitions": {
			Name:              "KV_parti-partitions",
			Storage:           jetstream.FileStorage,
			MaxMsgsPerSubject: 3, // immutable drift
			Metadata:          BuildMarker(ComponentPartitionSource, "prod"),
		},
	}
	plan := planWith(t, cfg, streams)

	require.Empty(t, recreateKVActions(plan), "AllowDeleteRecreate:false must suppress recreate-kv")

	var hasImmutableDrift bool
	for _, d := range plan.Drift {
		if d.Name == "parti-partitions" && d.Severity == SeverityDriftImmutable {
			hasImmutableDrift = true
		}
	}
	require.True(t, hasImmutableDrift, "drift-immutable finding must surface even without recreate-kv")
}

// TestRecreateKV_PartitionSource_SafeUpdate_NoRecreate verifies that a
// non-force policy (safe-update) suppresses recreate-kv for partition-source
// even when AllowDeleteRecreate:true and immutable drift is present.
// The drift-immutable finding must still surface.
func TestRecreateKV_PartitionSource_SafeUpdate_NoRecreate(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		Instance:   "prod",
		Policy:     PolicySafeUpdate,
		PartitionSource: &PartitionSourceConfig{
			Bucket:              "parti-partitions",
			Key:                 "partitions/v1",
			Storage:             "file",
			History:             1,
			Replicas:            1,
			AllowDeleteRecreate: true,
		},
	}
	// Live bucket has History=3 (immutable drift).
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-partitions": {
			Name:              "KV_parti-partitions",
			Storage:           jetstream.FileStorage,
			MaxMsgsPerSubject: 3, // History=3, want 1 → immutable drift
			Replicas:          1,
			Metadata:          BuildMarker(ComponentPartitionSource, "prod"),
		},
	}
	plan := planWith(t, cfg, streams)

	require.Empty(t, recreateKVActions(plan), "safe-update must not emit recreate-kv for partition-source")

	var hasImmutableDrift bool
	for _, d := range plan.Drift {
		if d.Name == "parti-partitions" && d.Severity == SeverityDriftImmutable {
			hasImmutableDrift = true
		}
	}
	require.True(t, hasImmutableDrift, "drift-immutable finding must surface under safe-update")
}

// TestRecreateKV_PartitionSource_Force_Unmarked_NoRecreate verifies that an
// unmarked partition-source bucket does not trigger recreate-kv (ownership not
// proven). The marker gate applies even under force + AllowDeleteRecreate:true.
func TestRecreateKV_PartitionSource_Force_Unmarked_NoRecreate(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		Instance:   "prod",
		Policy:     PolicyForce,
		PartitionSource: &PartitionSourceConfig{
			Bucket:              "parti-partitions",
			Key:                 "partitions/v1",
			Storage:             "file",
			History:             1,
			Replicas:            1,
			AllowDeleteRecreate: true,
		},
	}
	// Unmarked bucket with immutable drift — ownership not proven.
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-partitions": {
			Name:              "KV_parti-partitions",
			Storage:           jetstream.FileStorage,
			MaxMsgsPerSubject: 3, // History=3 (immutable drift — but ownership not proven)
			Replicas:          1,
			// Metadata: nil → unmarked
		},
	}
	plan := planWith(t, cfg, streams)

	require.Empty(t, recreateKVActions(plan), "unmarked partition-source bucket must not trigger recreate-kv")

	// Must have adopted drift (not immutable drift) for unmarked bucket.
	var hasAdopted bool
	for _, d := range plan.Drift {
		if d.Name == "parti-partitions" && d.Severity == SeverityAdopted {
			hasAdopted = true
		}
	}
	require.True(t, hasAdopted, "unmarked partition-source bucket must surface adopted drift")
}

// TestRecreateKV_PartitionSource_Force_MutableOnlyDrift_EmitsUpdateKV verifies
// that force + AllowDeleteRecreate:true + only mutable drift → update-kv, no
// recreate-kv. This is the "force ⊇ safe-update" invariant for partition-source.
func TestRecreateKV_PartitionSource_Force_MutableOnlyDrift_EmitsUpdateKV(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		Instance:   "prod",
		Policy:     PolicyForce,
		PartitionSource: &PartitionSourceConfig{
			Bucket:              "parti-partitions",
			Key:                 "partitions/v1",
			Storage:             "file",
			History:             1,
			Replicas:            1,
			MaxValueSize:        65536, // desired — live has no limit (MaxValueSize=0)
			AllowDeleteRecreate: true,
		},
	}
	// Live bucket has matching History=1 and Storage=file (no immutable drift),
	// but MaxValueSize=0 when config wants 65536 (mutable drift only).
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-partitions": {
			Name:              "KV_parti-partitions",
			Storage:           jetstream.FileStorage,
			MaxMsgsPerSubject: 1, // History=1 matches config → no immutable drift
			Replicas:          1,
			// MaxMsgSize=0 → mutable drift against config's MaxValueSize=65536
			Metadata: BuildMarker(ComponentPartitionSource, "prod"),
		},
	}
	plan := planWith(t, cfg, streams)

	require.Empty(t, recreateKVActions(plan), "mutable-only drift must not emit recreate-kv for partition-source")
	require.NotEmpty(t, updateKVActions(plan), "mutable drift under force must emit update-kv for partition-source")
}

// TestRecreateKV_PartitionSource_Force_After_PreservesOperatorFields verifies
// that when a partition-source recreate-kv is emitted, its
// RecreateKVResource.After carries all operator-configurable fields
// (Storage / History / Replicas / MaxValueSize / TTL) from the
// PartitionSourceConfig. This is the regression guard that recreate-kv uses
// buildPartitionSourceKVConfig (not the control-plane builder which would lose
// those fields).
func TestRecreateKV_PartitionSource_Force_After_PreservesOperatorFields(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		Instance:   "prod",
		Policy:     PolicyForce,
		PartitionSource: &PartitionSourceConfig{
			Bucket:              "parti-partitions",
			Key:                 "partitions/v1",
			Storage:             "memory",        // distinctive: not the file default
			History:             5,               // distinctive: not 1
			Replicas:            3,               // distinctive: not 1
			MaxValueSize:        65536,           // distinctive: not 0
			TTL:                 2 * time.Minute, // distinctive: not 0
			AllowDeleteRecreate: true,
		},
	}
	// Live bucket has FileStorage (immutable drift vs config "memory") and
	// History=1 (immutable drift vs config History=5) → triggers recreate-kv.
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-partitions": {
			Name:              "KV_parti-partitions",
			Storage:           jetstream.FileStorage, // want memory → immutable drift
			MaxMsgsPerSubject: 1,                     // History=1, want 5 → immutable drift
			Replicas:          1,
			Metadata:          BuildMarker(ComponentPartitionSource, "prod"),
		},
	}
	plan := planWith(t, cfg, streams)

	recreates := recreateKVActions(plan)
	require.Len(t, recreates, 1, "exactly one recreate-kv expected")

	res, ok := recreates[0].Resource.(*RecreateKVResource)
	require.True(t, ok, "Resource must be *RecreateKVResource")

	// Assert each operator-configurable field appears in After.
	require.Equal(t, jetstream.MemoryStorage, res.After.Storage,
		"After.Storage must carry operator-configured value (memory)")
	require.Equal(t, uint8(5), res.After.History,
		"After.History must carry operator-configured value (5)")
	require.Equal(t, 3, res.After.Replicas,
		"After.Replicas must carry operator-configured value (3)")
	require.Equal(t, int32(65536), res.After.MaxValueSize,
		"After.MaxValueSize must carry operator-configured value (65536)")
	require.Equal(t, 2*time.Minute, res.After.TTL,
		"After.TTL must carry operator-configured value (2m)")
}

// TestRecreateKV_Determinism verifies that two identical planWith calls
// produce byte-equal PlanResults (no map iteration non-determinism in actions or drift).
func TestRecreateKV_Determinism(t *testing.T) {
	t.Parallel()

	cfg := cpForceCfg()
	cfg.Instance = "staging" // mutable instance drift too
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStreamImmutable(),
	}

	plan1 := planWith(t, cfg, streams)
	plan2 := planWith(t, cfg, streams)

	require.Equal(t, plan1.Actions, plan2.Actions, "action list must be deterministic")
	require.Equal(t, plan1.Drift, plan2.Drift, "drift list must be deterministic")
}
