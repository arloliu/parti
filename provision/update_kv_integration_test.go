package provision_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// safeUpdateCtx returns a context bounded for an embedded-NATS apply test.
func safeUpdateCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	return ctx
}

// markedElectionKV builds a KeyValueConfig for a Parti-marked
// control-plane election bucket. mutate lets a test introduce drift.
func markedElectionKV(mutate ...func(*jetstream.KeyValueConfig)) jetstream.KeyValueConfig {
	kv := jetstream.KeyValueConfig{
		Bucket:   "parti-election",
		Storage:  jetstream.MemoryStorage,
		History:  1,
		TTL:      10 * time.Second,
		Metadata: provision.BuildMarker(provision.ComponentControlPlaneElection, "prod"),
	}
	for _, m := range mutate {
		m(&kv)
	}

	return kv
}

// controlPlaneOnlyCfg returns a Config provisioning a single control-plane
// election bucket with the given policy. The other control-plane buckets
// are still emitted, so tests assert per-bucket.
func controlPlaneOnlyCfg(policy provision.ReconcilePolicy) provision.Config {
	return provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		Policy:     policy,
		ControlPlane: &provision.ControlPlaneConfig{
			WorkerIDTTL:     75 * time.Second,
			ElectionTimeout: 10 * time.Second,
			HeartbeatTTL:    15 * time.Second,
			AssignmentTTL:   0,
		},
	}
}

// liveStreamConfig fetches the current StreamConfig for a KV bucket.
func liveStreamConfig(t *testing.T, js jetstream.JetStream, bucket string) jetstream.StreamConfig {
	t.Helper()
	stream, err := js.Stream(context.Background(), "KV_"+bucket)
	require.NoError(t, err)
	info, err := stream.Info(context.Background())
	require.NoError(t, err)

	return info.Config
}

// driftSeverityFor returns the severities reported for a named bucket.
func driftSeverityFor(plan provision.PlanResult, name string) []string {
	var out []string
	for _, d := range plan.Drift {
		if d.Name == name {
			out = append(out, d.Severity)
		}
	}

	return out
}

func TestApply_UpdateKV_ControlPlaneTTL_Converges(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Pre-create a marked election bucket with a drifted TTL.
	createKV(t, js, markedElectionKV(func(kv *jetstream.KeyValueConfig) {
		kv.TTL = 99 * time.Second
	}))

	cfg := controlPlaneOnlyCfg(provision.PolicySafeUpdate)
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.False(t, rep.Aborted)
	require.Empty(t, rep.Errors)

	// election must have been reconciled in place (update-kv), not created.
	var electionExec provision.ExecutedAction
	for _, ex := range rep.Executed {
		if ex.Name == "parti-election" {
			electionExec = ex
		}
	}
	require.Equal(t, provision.ActionUpdateKV, electionExec.Kind)

	require.Equal(t, 10*time.Second, liveStreamConfig(t, js, "parti-election").MaxAge)

	// Re-plan: election is now informational.
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)
	require.Contains(t, driftSeverityFor(plan, "parti-election"), provision.SeverityInformational)
}

func TestApply_UpdateKV_ControlPlaneInstanceChange_Converges(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	createKV(t, js, markedElectionKV(func(kv *jetstream.KeyValueConfig) {
		kv.Metadata = provision.BuildMarker(provision.ComponentControlPlaneElection, "staging")
	}))

	cfg := controlPlaneOnlyCfg(provision.PolicySafeUpdate) // Instance "prod"
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)

	live := liveStreamConfig(t, js, "parti-election")
	require.Equal(t, "prod", provision.ParseMarker(live.Metadata).Instance)
}

func TestApply_UpdateKV_ControlPlaneReplicas_Converges(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Single-node embedded server: live Replicas is 1. Requesting an
	// explicit Replicas=1 is feasible and exercises the operator-
	// expressible Replicas path without server rejection.
	createKV(t, js, markedElectionKV(func(kv *jetstream.KeyValueConfig) {
		kv.TTL = 88 * time.Second // drift so an update-kv is emitted
	}))

	cfg := controlPlaneOnlyCfg(provision.PolicySafeUpdate)
	cfg.ControlPlane.Replicas = 1
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
	require.Equal(t, 1, liveStreamConfig(t, js, "parti-election").Replicas)
}

func TestApply_UpdateKV_ReplicasServerRejection_FailsFast(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	createKV(t, js, markedElectionKV(func(kv *jetstream.KeyValueConfig) {
		kv.TTL = 88 * time.Second
	}))

	cfg := controlPlaneOnlyCfg(provision.PolicySafeUpdate)
	cfg.ControlPlane.Replicas = 3 // infeasible on a single-node server

	rep, err := provision.Apply(ctx, js, cfg)
	require.Error(t, err)
	require.False(t, rep.Aborted, "server rejection is a resource error, not an abort")
	require.NotEmpty(t, rep.Errors)

	// Fail-fast contract: the failing action appears in Errors, not Skipped.
	for _, e := range rep.Errors {
		require.NotContains(t, []string{e.Name}, "",
			"resource error must name the failing bucket")
	}
	for _, s := range rep.Skipped {
		require.Equal(t, provision.SkipReasonPriorError, s.Reason,
			"actions after the failure are skipped with prior-error, not double-reported")
	}
}

func TestApply_UpdateKV_PartitionSourceTTLAndMaxValueSize_Converge(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:       "parti-partitions",
		Storage:      jetstream.FileStorage,
		History:      2,
		TTL:          time.Minute, // drifts
		MaxValueSize: 256,         // drifts
		Metadata:     provision.BuildMarker(provision.ComponentPartitionSource, "prod"),
	})

	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		Policy:     provision.PolicySafeUpdate,
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket:       "parti-partitions",
			Key:          "partitions/v1",
			Storage:      "file",
			History:      2,
			MaxValueSize: 4096,
			TTL:          5 * time.Minute,
		},
	}
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)

	live := liveStreamConfig(t, js, "parti-partitions")
	require.Equal(t, 5*time.Minute, live.MaxAge)
	require.Equal(t, int32(4096), live.MaxMsgSize)

	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)
	require.Contains(t, driftSeverityFor(plan, "parti-partitions"), provision.SeverityInformational)
}

func TestApply_UpdateKV_PartitionSourceReplicas_Converges(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Single-node embedded server: live Replicas is 1. An explicit
	// Replicas=1 exercises the partition-source operator-expressible
	// Replicas path without server rejection; the TTL drift triggers
	// the update-kv.
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "parti-partitions",
		Storage:  jetstream.FileStorage,
		History:  1,
		TTL:      time.Minute, // drifts
		Metadata: provision.BuildMarker(provision.ComponentPartitionSource, "prod"),
	})

	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		Policy:     provision.PolicySafeUpdate,
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket:   "parti-partitions",
			Key:      "partitions/v1",
			Storage:  "file",
			History:  1,
			Replicas: 1,
			TTL:      5 * time.Minute,
		},
	}
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)

	live := liveStreamConfig(t, js, "parti-partitions")
	require.Equal(t, 1, live.Replicas)
	require.Equal(t, 5*time.Minute, live.MaxAge,
		"TTL reconciled alongside the explicit Replicas=1")
}

func TestApply_UpdateKV_PreservesLiveDescriptionAndMaxBytes(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Pre-create a marked bucket with Description and MaxBytes set
	// out-of-band — neither has a YAML representation — plus a drifted TTL.
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:              "KV_parti-election",
		Description:       "owned by team X",
		Subjects:          []string{"$KV.parti-election.>"},
		Storage:           jetstream.MemoryStorage,
		MaxMsgsPerSubject: 1,
		MaxAge:            99 * time.Second,
		MaxBytes:          1 << 20,
		AllowDirect:       true,
		Metadata:          provision.BuildMarker(provision.ComponentControlPlaneElection, "prod"),
	})
	require.NoError(t, err)

	cfg := controlPlaneOnlyCfg(provision.PolicySafeUpdate)
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)

	live := liveStreamConfig(t, js, "parti-election")
	require.Equal(t, 10*time.Second, live.MaxAge, "TTL reconciled")
	require.Equal(t, "owned by team X", live.Description,
		"preserved-from-live Description survives a safe-update")
	require.Equal(t, int64(1<<20), live.MaxBytes,
		"preserved-from-live MaxBytes survives a safe-update")
}

func TestApply_UpdateKV_RerunIsNoOp(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	createKV(t, js, markedElectionKV(func(kv *jetstream.KeyValueConfig) {
		kv.TTL = 99 * time.Second
	}))

	cfg := controlPlaneOnlyCfg(provision.PolicySafeUpdate)

	first, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, first.Errors)

	// Second Apply: election already converged. Plan emits no update-kv
	// (canonical-equality suppression), so nothing is executed for it.
	second, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, second.Errors)
	require.Empty(t, second.Skipped)
	for _, ex := range second.Executed {
		require.NotEqual(t, "parti-election", ex.Name,
			"converged election needs no second update")
	}
}

func TestApply_UpdateKV_UnmarkedBucket_NotMutated(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	// Pre-create an UNMARKED election bucket with a drifted TTL.
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:  "parti-election",
		Storage: jetstream.MemoryStorage,
		History: 1,
		TTL:     99 * time.Second,
	})

	cfg := controlPlaneOnlyCfg(provision.PolicySafeUpdate)
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)

	// safe-update does not adopt: the unmarked bucket's TTL is untouched.
	require.Equal(t, 99*time.Second, liveStreamConfig(t, js, "parti-election").MaxAge)
	for _, ex := range rep.Executed {
		require.NotEqual(t, "parti-election", ex.Name)
	}
}

func TestApply_Warn_MarkedDriftedBucket_NotMutated(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx := safeUpdateCtx(t)

	createKV(t, js, markedElectionKV(func(kv *jetstream.KeyValueConfig) {
		kv.TTL = 99 * time.Second
	}))

	cfg := controlPlaneOnlyCfg(provision.PolicyWarn)
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)

	// warn never mutates an existing bucket.
	require.Equal(t, 99*time.Second, liveStreamConfig(t, js, "parti-election").MaxAge)
}
