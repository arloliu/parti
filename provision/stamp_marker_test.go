package provision

// Same-package unit tests for the stamp-marker plan-emission and apply
// helpers (the adopt reconcile policy). These use synthetic
// *jetstream.StreamInfo / StreamConfig values so they run without a
// live NATS server. Shared helpers (cpUpdateCfg, electionStream,
// planWith, fakeStreamReader, fakeKVUpdater, cpSpecsFor) live in
// update_kv_test.go.

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// stampMarkerActions returns just the stamp-marker actions in a plan.
func stampMarkerActions(p PlanResult) []PlannedAction {
	var out []PlannedAction
	for _, a := range p.Actions {
		if a.Kind == ActionStampMarker {
			out = append(out, a)
		}
	}

	return out
}

// stampMarkerRes type-asserts a PlannedAction's Resource to
// *StampMarkerResource, failing the test on a mismatch.
func stampMarkerRes(t *testing.T, a PlannedAction) *StampMarkerResource {
	t.Helper()
	res, ok := a.Resource.(*StampMarkerResource)
	require.True(t, ok, "Resource is *StampMarkerResource")

	return res
}

// unmarkedElectionStream returns a synthetic KV_parti-election stream
// with no Parti marker. mutate lets a test add stray keys.
func unmarkedElectionStream(mutate ...func(*jetstream.StreamConfig)) jetstream.StreamConfig {
	cfg := jetstream.StreamConfig{
		Name:              "KV_parti-election",
		Storage:           jetstream.MemoryStorage,
		MaxMsgsPerSubject: 1,
		MaxAge:            10 * time.Second,
	}
	for _, m := range mutate {
		m(&cfg)
	}

	return cfg
}

// --- type shape --------------------------------------------------------------

func TestStampMarkerResource_ShapeAndConstant(t *testing.T) {
	t.Parallel()

	require.Equal(t, "stamp-marker", ActionStampMarker)

	res := StampMarkerResource{
		Bucket:         "parti-election",
		MergedMetadata: map[string]string{MarkerManagedKey: MarkerManagedValue},
		PartiKeys:      []string{MarkerManagedKey},
	}
	require.Equal(t, "parti-election", res.Bucket)
	require.Equal(t, MarkerManagedValue, res.MergedMetadata[MarkerManagedKey])
	require.Equal(t, []string{MarkerManagedKey}, res.PartiKeys)
}

// --- keysAddedOrChanged ------------------------------------------------------

func TestKeysAddedOrChanged_Additions(t *testing.T) {
	t.Parallel()

	live := map[string]string{"team": "infra"}
	merged := map[string]string{
		"team":             "infra",
		MarkerManagedKey:   MarkerManagedValue,
		MarkerComponentKey: ComponentControlPlaneElection,
	}
	require.Equal(t, []string{MarkerComponentKey, MarkerManagedKey},
		keysAddedOrChanged(live, merged))
}

func TestKeysAddedOrChanged_Overwrite(t *testing.T) {
	t.Parallel()

	live := map[string]string{MarkerManagedKey: "v0"}
	merged := map[string]string{MarkerManagedKey: MarkerManagedValue}
	require.Equal(t, []string{MarkerManagedKey}, keysAddedOrChanged(live, merged))
}

func TestKeysAddedOrChanged_Removal(t *testing.T) {
	t.Parallel()

	// A stray parti.io/instance present live but deleted by the merge
	// (adopted under an empty instance) is a real change.
	live := map[string]string{MarkerInstanceKey: "old"}
	merged := map[string]string{}
	require.Equal(t, []string{MarkerInstanceKey}, keysAddedOrChanged(live, merged))
}

func TestKeysAddedOrChanged_NoChange(t *testing.T) {
	t.Parallel()

	m := map[string]string{MarkerManagedKey: MarkerManagedValue, "team": "infra"}
	require.Empty(t, keysAddedOrChanged(m, m))
}

// --- plan emission -----------------------------------------------------------

func TestPlanEmission_Adopt_UnmarkedControlPlaneBucket_EmitsStampMarker(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyAdopt)
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": unmarkedElectionStream(),
	}
	actions := stampMarkerActions(planWith(t, cfg, streams))
	require.Len(t, actions, 1)
	require.Equal(t, "parti-election", actions[0].Name)

	res := stampMarkerRes(t, actions[0])
	require.Equal(t, "parti-election", res.Bucket)
	require.Equal(t, MarkerManagedValue, res.MergedMetadata[MarkerManagedKey])
	require.Equal(t, ComponentControlPlaneElection, res.MergedMetadata[MarkerComponentKey])
	require.Equal(t, "prod", res.MergedMetadata[MarkerInstanceKey])
	require.ElementsMatch(t,
		[]string{MarkerManagedKey, MarkerComponentKey, MarkerInstanceKey}, res.PartiKeys)
}

func TestPlanEmission_Adopt_UnmarkedPartitionSourceBucket_EmitsStampMarker(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		Instance:   "prod",
		Policy:     PolicyAdopt,
		PartitionSource: &PartitionSourceConfig{
			Bucket:  "parti-partitions",
			Key:     "partitions/v1",
			Storage: "file",
			History: 1,
		},
	}
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-partitions": {
			Name:              "KV_parti-partitions",
			Storage:           jetstream.FileStorage,
			MaxMsgsPerSubject: 1,
		},
	}
	actions := stampMarkerActions(planWith(t, cfg, streams))
	require.Len(t, actions, 1)
	require.Equal(t, "parti-partitions", actions[0].Name)
	res := stampMarkerRes(t, actions[0])
	require.Equal(t, ComponentPartitionSource, res.MergedMetadata[MarkerComponentKey])
}

func TestPlanEmission_Adopt_MarkedBucket_NoStampMarker(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyAdopt)
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": electionStream(), // already marked
	}
	require.Empty(t, stampMarkerActions(planWith(t, cfg, streams)),
		"a marked bucket is already adopted; no stamp-marker")
}

func TestPlanEmission_Adopt_MissingBucket_NoActionInformationalFinding(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyAdopt)
	// No streams: every control-plane bucket is missing.
	plan := planWith(t, cfg, map[string]jetstream.StreamConfig{})
	require.Empty(t, plan.Actions, "adopt creates nothing for missing buckets")

	var found DriftFinding
	for _, d := range plan.Drift {
		if d.Name == "parti-election" {
			found = d
		}
	}
	require.Equal(t, SeverityInformational, found.Severity)
	require.Equal(t, KindControlPlaneKV, found.Kind)
	reason, _ := found.Detail["reason"].(string)
	require.Equal(t, "bucket missing; adopt does not create — run apply with warn or safe-update", reason)
}

func TestPlanEmission_Adopt_MissingPartitionSource_InformationalFinding(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		Instance:   "prod",
		Policy:     PolicyAdopt,
		PartitionSource: &PartitionSourceConfig{
			Bucket:  "parti-partitions",
			Key:     "partitions/v1",
			Storage: "file",
			History: 1,
		},
	}
	plan := planWith(t, cfg, map[string]jetstream.StreamConfig{})
	require.Empty(t, plan.Actions)

	var found DriftFinding
	for _, d := range plan.Drift {
		if d.Name == "parti-partitions" {
			found = d
		}
	}
	require.Equal(t, SeverityInformational, found.Severity)
	require.Equal(t, KindPartitionSource, found.Kind)
	reason, _ := found.Detail["reason"].(string)
	require.Equal(t, "bucket missing; adopt does not create — run apply with warn or safe-update", reason)
}

// Note: adopt-emits-no-update-kv is covered by TestPlanEmission_Adopt_NoUpdateKV
// in update_kv_test.go.

func TestPlanEmission_WarnAndSafeUpdate_NoStampMarker(t *testing.T) {
	t.Parallel()

	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": unmarkedElectionStream(),
	}
	require.Empty(t, stampMarkerActions(planWith(t, cpUpdateCfg(PolicyWarn), streams)),
		"warn never emits stamp-marker")
	require.Empty(t, stampMarkerActions(planWith(t, cpUpdateCfg(PolicySafeUpdate), streams)),
		"safe-update never emits stamp-marker")
}

func TestPlanEmission_Warn_MissingBucket_KeepsCreateKV(t *testing.T) {
	t.Parallel()

	// warn / safe-update keep their create-kv emission for a missing
	// bucket — only adopt substitutes the informational finding.
	for _, policy := range []ReconcilePolicy{PolicyWarn, PolicySafeUpdate} {
		plan := planWith(t, cpUpdateCfg(policy), map[string]jetstream.StreamConfig{})
		var sawCreate bool
		for _, a := range plan.Actions {
			if a.Kind == ActionCreateKV && a.Name == "parti-election" {
				sawCreate = true
			}
		}
		require.True(t, sawCreate, "%s still emits create-kv for a missing bucket", policy)
	}
}

func TestPlanEmission_Adopt_EmptyInstance_NoInstanceKey(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyAdopt)
	cfg.Instance = ""
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": unmarkedElectionStream(),
	}
	actions := stampMarkerActions(planWith(t, cfg, streams))
	require.Len(t, actions, 1)
	res := stampMarkerRes(t, actions[0])
	_, ok := res.MergedMetadata[MarkerInstanceKey]
	require.False(t, ok, "empty instance produces no parti.io/instance key")
	require.ElementsMatch(t, []string{MarkerManagedKey, MarkerComponentKey}, res.PartiKeys)
}

func TestPlanEmission_Adopt_NonPartiKeysPreserved(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyAdopt)
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": unmarkedElectionStream(func(c *jetstream.StreamConfig) {
			c.Metadata = map[string]string{"custom.io/team": "infra"}
		}),
	}
	actions := stampMarkerActions(planWith(t, cfg, streams))
	require.Len(t, actions, 1)
	res := stampMarkerRes(t, actions[0])
	require.Equal(t, "infra", res.MergedMetadata["custom.io/team"],
		"non-Parti keys are preserved in MergedMetadata")
	require.ElementsMatch(t,
		[]string{MarkerManagedKey, MarkerComponentKey, MarkerInstanceKey}, res.PartiKeys,
		"PartiKeys lists only the Parti keys added")
}

func TestPlanEmission_Adopt_StrayInstanceKey_RemovalInPartiKeys(t *testing.T) {
	t.Parallel()

	// An unmarked bucket carrying a stray parti.io/instance, adopted
	// under an empty cfg.Instance: the key is deleted by the merge and
	// the removal must surface in PartiKeys.
	cfg := cpUpdateCfg(PolicyAdopt)
	cfg.Instance = ""
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": unmarkedElectionStream(func(c *jetstream.StreamConfig) {
			c.Metadata = map[string]string{MarkerInstanceKey: "old"}
		}),
	}
	actions := stampMarkerActions(planWith(t, cfg, streams))
	require.Len(t, actions, 1)
	res := stampMarkerRes(t, actions[0])
	_, ok := res.MergedMetadata[MarkerInstanceKey]
	require.False(t, ok, "MergedMetadata omits the stray instance key")
	require.Contains(t, res.PartiKeys, MarkerInstanceKey,
		"PartiKeys includes the removed instance key")
}

func TestPlanEmission_Adopt_UnmarkedBucket_StillSurfacesAdoptedDrift(t *testing.T) {
	t.Parallel()

	// The classifier's adopted finding coexists with the stamp-marker
	// action: adoption is not approval.
	cfg := cpUpdateCfg(PolicyAdopt)
	streams := map[string]jetstream.StreamConfig{
		"KV_parti-election": unmarkedElectionStream(),
	}
	plan := planWith(t, cfg, streams)
	require.Len(t, stampMarkerActions(plan), 1)
	var adopted bool
	for _, d := range plan.Drift {
		if d.Name == "parti-election" && d.Severity == SeverityAdopted {
			adopted = true
		}
	}
	require.True(t, adopted, "unmarked bucket still surfaces adopted drift under adopt")
}

// --- apply, seam-based -------------------------------------------------------

func stampMarkerAction(bucket string) PlannedAction {
	return PlannedAction{
		Kind:     ActionStampMarker,
		Name:     bucket,
		Resource: &StampMarkerResource{Bucket: bucket},
	}
}

func TestApplyStampMarker_UnmarkedReread_WritesMergedMetadata(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyAdopt)
	live := unmarkedElectionStream(func(c *jetstream.StreamConfig) {
		c.MaxAge = 10 * time.Second
		c.Metadata = map[string]string{"custom.io/team": "infra"}
	})
	reader := fakeStreamReader{cfg: live}
	updater := &fakeKVUpdater{}

	executed, err := applyStampMarkerAction(context.Background(), reader, updater, cfg,
		cpSpecsFor(t, cfg), stampMarkerAction("parti-election"))
	require.NoError(t, err)
	require.False(t, executed.Raced)
	require.True(t, updater.called)

	// Metadata is the merged map.
	require.Equal(t, MarkerManagedValue, updater.got.Metadata[MarkerManagedKey])
	require.Equal(t, ComponentControlPlaneElection, updater.got.Metadata[MarkerComponentKey])
	require.Equal(t, "prod", updater.got.Metadata[MarkerInstanceKey])
	require.Equal(t, "infra", updater.got.Metadata["custom.io/team"])

	// Every non-metadata field equals the re-read snapshot.
	require.Equal(t, 10*time.Second, updater.got.TTL)
	require.Equal(t, jetstream.MemoryStorage, updater.got.Storage)
	require.Equal(t, "parti-election", updater.got.Bucket)
}

func TestApplyStampMarker_AlreadyMarkedReread_RacedNoWrite(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyAdopt)
	// The bucket already carries an equivalent marker: merge is a no-op.
	reader := fakeStreamReader{cfg: electionStream()}
	updater := &fakeKVUpdater{}

	executed, err := applyStampMarkerAction(context.Background(), reader, updater, cfg,
		cpSpecsFor(t, cfg), stampMarkerAction("parti-election"))
	require.NoError(t, err)
	require.True(t, executed.Raced, "an already-marked bucket is a raced no-op")
	require.False(t, updater.called, "no write when the marker is already present")
}

func TestApplyStampMarker_MissingOnReread_FailsFast(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyAdopt)
	reader := fakeStreamReader{err: jetstream.ErrStreamNotFound}
	updater := &fakeKVUpdater{}

	_, err := applyStampMarkerAction(context.Background(), reader, updater, cfg,
		cpSpecsFor(t, cfg), stampMarkerAction("parti-election"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "bucket-missing-before-stamp")
	require.False(t, updater.called)
}

func TestApplyStampMarker_MissingOnWrite_FailsFast(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyAdopt)
	reader := fakeStreamReader{cfg: unmarkedElectionStream()}
	updater := &fakeKVUpdater{err: jetstream.ErrBucketNotFound}

	_, err := applyStampMarkerAction(context.Background(), reader, updater, cfg,
		cpSpecsFor(t, cfg), stampMarkerAction("parti-election"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "bucket-missing-before-stamp")
	require.True(t, updater.called)
}

func TestApplyStampMarker_WrongResourceType_FailsFast(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyAdopt)
	action := PlannedAction{Kind: ActionStampMarker, Name: "parti-election", Resource: "nope"}
	_, err := applyStampMarkerAction(context.Background(), fakeStreamReader{cfg: unmarkedElectionStream()},
		&fakeKVUpdater{}, cfg, cpSpecsFor(t, cfg), action)
	require.Error(t, err)
	require.Contains(t, err.Error(), "wrong Resource type")
}

func TestApplyStampMarker_UnknownBucket_FailsFast(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyAdopt)
	// A bucket the resolved config does not describe: defensive guard.
	_, err := applyStampMarkerAction(context.Background(), fakeStreamReader{cfg: unmarkedElectionStream()},
		&fakeKVUpdater{}, cfg, cpSpecsFor(t, cfg), stampMarkerAction("not-in-config"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not described by the resolved config")
}

func TestApplyStampMarker_CancelledReread_PropagatesCancellation(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyAdopt)
	reader := fakeStreamReader{err: context.Canceled}
	_, err := applyStampMarkerAction(context.Background(), reader, &fakeKVUpdater{}, cfg,
		cpSpecsFor(t, cfg), stampMarkerAction("parti-election"))
	require.ErrorIs(t, err, context.Canceled)
}

// TestApplyStampMarker_CrossPolicyRaceWindow demonstrates the documented
// best-effort cross-policy contract. stamp-marker re-reads live state
// and writes that snapshot back with only Metadata merged — it has no
// "desired" non-metadata values. If a concurrent safe-update changed a
// field such as TTL between this re-read and the write, stamp-marker
// would carry the value it read (the stale one) and revert the
// concurrent change. NATS UpdateStream has no expected-revision token,
// so this window cannot be closed; operators serialize adopt and
// safe-update themselves.
func TestApplyStampMarker_CrossPolicyRaceWindow(t *testing.T) {
	t.Parallel()

	cfg := cpUpdateCfg(PolicyAdopt)
	live := unmarkedElectionStream(func(c *jetstream.StreamConfig) {
		c.MaxAge = 10 * time.Second // the value a concurrent writer might change
		c.Metadata = map[string]string{"custom.io/team": "infra"}
	})
	reader := fakeStreamReader{cfg: live}
	updater := &fakeKVUpdater{}

	_, err := applyStampMarkerAction(context.Background(), reader, updater, cfg,
		cpSpecsFor(t, cfg), stampMarkerAction("parti-election"))
	require.NoError(t, err)
	require.True(t, updater.called)

	// The captured target's TTL equals the re-read 10s — not any
	// "desired" value, because stamp-marker has none.
	require.Equal(t, 10*time.Second, updater.got.TTL,
		"the write carries the re-read TTL; a concurrent change would be reverted")
	// Metadata is the Parti marker plus the preserved non-Parti key.
	require.Equal(t, MarkerManagedValue, updater.got.Metadata[MarkerManagedKey])
	require.Equal(t, ComponentControlPlaneElection, updater.got.Metadata[MarkerComponentKey])
	require.Equal(t, "infra", updater.got.Metadata["custom.io/team"])
}
