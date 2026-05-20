package provision_test

// Embedded-NATS integration tests for the stamp-marker apply path (the
// adopt reconcile policy). Shared helpers (newJS, createKV,
// safeUpdateCtx, controlPlaneOnlyCfg, liveStreamConfig,
// driftSeverityFor) live in view_integration_test.go and
// update_kv_integration_test.go.

import (
	"testing"
	"time"

	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestApply_Adopt_UnmarkedBucket_StampsMarker(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Pre-create an unmarked election bucket.
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:  "parti-election",
		Storage: jetstream.MemoryStorage,
		History: 1,
		TTL:     10 * time.Second,
	})

	cfg := controlPlaneOnlyCfg(provision.PolicyAdopt)
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.False(t, rep.Aborted)
	require.Empty(t, rep.Errors)

	var electionExec provision.ExecutedAction
	for _, ex := range rep.Executed {
		if ex.Name == "parti-election" {
			electionExec = ex
		}
	}
	require.Equal(t, provision.ActionStampMarker, electionExec.Kind)

	live := liveStreamConfig(t, js, "parti-election")
	marker := provision.ParseMarker(live.Metadata)
	require.True(t, marker.IsManaged())
	require.Equal(t, provision.ComponentControlPlaneElection, marker.Component)
	require.Equal(t, "prod", marker.Instance)
}

func TestApply_Adopt_PreservesNonPartiMetadata(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "parti-election",
		Storage:  jetstream.MemoryStorage,
		History:  1,
		TTL:      10 * time.Second,
		Metadata: map[string]string{"custom.io/team": "x"},
	})

	cfg := controlPlaneOnlyCfg(provision.PolicyAdopt)
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)

	live := liveStreamConfig(t, js, "parti-election")
	require.Equal(t, "x", live.Metadata["custom.io/team"],
		"non-Parti metadata key survives adoption")
	require.Equal(t, provision.MarkerManagedValue, live.Metadata[provision.MarkerManagedKey])
	require.Equal(t, provision.ComponentControlPlaneElection,
		live.Metadata[provision.MarkerComponentKey])
}

func TestApply_Adopt_PreservesLiveFields(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Pre-create an unmarked bucket with Description, MaxBytes, TTL,
	// MaxValueSize, and an explicit Replicas set.
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:              "KV_parti-election",
		Description:       "owned by team X",
		Subjects:          []string{"$KV.parti-election.>"},
		Storage:           jetstream.MemoryStorage,
		MaxMsgsPerSubject: 1,
		MaxAge:            42 * time.Second,
		MaxBytes:          1 << 20,
		MaxMsgSize:        4096,
		Replicas:          1,
		AllowDirect:       true,
	})
	require.NoError(t, err)

	cfg := controlPlaneOnlyCfg(provision.PolicyAdopt)
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)

	live := liveStreamConfig(t, js, "parti-election")
	require.Equal(t, "owned by team X", live.Description, "Description preserved")
	require.Equal(t, int64(1<<20), live.MaxBytes, "MaxBytes preserved")
	require.Equal(t, 42*time.Second, live.MaxAge, "TTL preserved")
	require.Equal(t, int32(4096), live.MaxMsgSize, "MaxValueSize preserved")
	require.Equal(t, 1, live.Replicas, "Replicas preserved")
	// Only Metadata gained the Parti keys.
	require.True(t, provision.ParseMarker(live.Metadata).IsManaged())
}

func TestApply_Adopt_RerunIsNoOp(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:  "parti-election",
		Storage: jetstream.MemoryStorage,
		History: 1,
		TTL:     10 * time.Second,
	})

	cfg := controlPlaneOnlyCfg(provision.PolicyAdopt)

	first, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, first.Errors)

	// Second Apply: the bucket is now marked, so Plan emits no
	// stamp-marker for it.
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)
	for _, a := range plan.Actions {
		require.NotEqual(t, "parti-election", a.Name,
			"an adopted bucket needs no second stamp-marker")
	}

	second, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, second.Errors)
	for _, ex := range second.Executed {
		require.NotEqual(t, "parti-election", ex.Name,
			"converged election needs no second adoption")
	}

	// Metadata unchanged after the second run.
	live := liveStreamConfig(t, js, "parti-election")
	require.Equal(t, provision.ComponentControlPlaneElection,
		provision.ParseMarker(live.Metadata).Component)
}

func TestApply_Adopt_MissingBucket_CreatesNothing(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// No buckets exist. adopt must create nothing and surface one
	// informational finding per missing bucket.
	cfg := controlPlaneOnlyCfg(provision.PolicyAdopt)

	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, plan.Actions, "adopt creates nothing")
	require.Contains(t, driftSeverityFor(plan, "parti-election"),
		provision.SeverityInformational)

	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Executed, "adopt executes nothing for missing buckets")
	require.Empty(t, rep.Errors)

	// The stream was not created.
	_, err = js.Stream(ctx, "KV_parti-election")
	require.ErrorIs(t, err, jetstream.ErrStreamNotFound)
}

func TestApply_Adopt_ThenSafeUpdate_BucketBecomesVisible(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Pre-create an unmarked bucket whose TTL matches the desired
	// config (so after adoption a safe-update plan is in sync).
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:  "parti-election",
		Storage: jetstream.MemoryStorage,
		History: 1,
		TTL:     10 * time.Second,
	})

	adoptCfg := controlPlaneOnlyCfg(provision.PolicyAdopt)
	_, err := provision.Apply(ctx, js, adoptCfg)
	require.NoError(t, err)

	// After adoption the safe-update path classifies the bucket
	// normally — it is no longer "adopted" drift.
	safeCfg := controlPlaneOnlyCfg(provision.PolicySafeUpdate)
	plan, err := provision.Plan(ctx, js, safeCfg)
	require.NoError(t, err)
	sevs := driftSeverityFor(plan, "parti-election")
	require.Contains(t, sevs, provision.SeverityInformational,
		"adopted bucket is now in sync under safe-update")
	require.NotContains(t, sevs, provision.SeverityAdopted,
		"adoption made the bucket visible to the safe-update path")
}

func TestApply_Adopt_PartitionSource_StampsMarker(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:  "parti-partitions",
		Storage: jetstream.FileStorage,
		History: 1,
	})

	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		Policy:     provision.PolicyAdopt,
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket:  "parti-partitions",
			Key:     "partitions/v1",
			Storage: "file",
			History: 1,
		},
	}
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)

	live := liveStreamConfig(t, js, "parti-partitions")
	marker := provision.ParseMarker(live.Metadata)
	require.True(t, marker.IsManaged())
	require.Equal(t, provision.ComponentPartitionSource, marker.Component)
	require.Equal(t, "prod", marker.Instance)
}

// Note: the bucket-missing-before-stamp fail-fast path (a bucket
// deleted between the plan-time lookup and the apply-time re-read)
// cannot be triggered deterministically through the public Apply
// surface — Apply re-plans internally, so a bucket deleted before
// applyPlan simply produces no stamp-marker action. The fail-fast
// path is covered deterministically by the seam-based unit tests
// TestApplyStampMarker_MissingOnReread_FailsFast and
// TestApplyStampMarker_MissingOnWrite_FailsFast in stamp_marker_test.go.

// TestApply_Warn_Unchanged guards that warn behavior is byte-identical:
// an unmarked drifted bucket is not mutated and surfaces adopted drift.
func TestApply_Warn_Unchanged_ForUnmarkedBucket(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:  "parti-election",
		Storage: jetstream.MemoryStorage,
		History: 1,
		TTL:     99 * time.Second,
	})

	cfg := controlPlaneOnlyCfg(provision.PolicyWarn)
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)

	// warn does not adopt: the unmarked bucket is untouched.
	live := liveStreamConfig(t, js, "parti-election")
	require.Equal(t, 99*time.Second, live.MaxAge)
	require.False(t, provision.ParseMarker(live.Metadata).IsManaged())
}
