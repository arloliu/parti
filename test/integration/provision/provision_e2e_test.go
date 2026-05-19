package provision_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/kvbuckets"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/provision"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// makeE2EConfig returns a pair of configs that share bucket names and TTL
// values. parti.Config is built first (from IntegrationTestConfig defaults
// with per-test suffixed bucket names); provision.Config is derived from it
// so both configs are driven by a single source of truth.
//
// HandoffTTL is 30s to satisfy the provision validator's "HandoffTTL > 0 when
// EnableTwoPhaseHandoff" constraint while staying within the test budget.
func makeE2EConfig(t *testing.T, suffix string) (provision.Config, parti.Config) {
	t.Helper()

	// Build the runtime config first.
	partiCfg := testutil.IntegrationTestConfig()
	partiCfg.WorkerIDPrefix = "e2e-smoke-" + suffix
	partiCfg.EnableTwoPhaseHandoff = true
	partiCfg.KVBuckets.StableIDBucket = "parti-stableid-" + suffix
	partiCfg.KVBuckets.ElectionBucket = "parti-election-" + suffix
	partiCfg.KVBuckets.HeartbeatBucket = "parti-heartbeat-" + suffix
	partiCfg.KVBuckets.AssignmentBucket = "parti-assignment-" + suffix
	partiCfg.KVBuckets.HandoffBucket = "parti-handoff-" + suffix
	partiCfg.KVBuckets.HandoffTTL = 30 * time.Second

	// Derive the provision config from the runtime config so all TTLs and
	// bucket names stay in sync.
	provCfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "e2e-smoke-" + suffix,
		Policy:     provision.PolicyWarn,
		ControlPlane: &provision.ControlPlaneConfig{
			StableIDBucket:        partiCfg.KVBuckets.StableIDBucket,
			ElectionBucket:        partiCfg.KVBuckets.ElectionBucket,
			HeartbeatBucket:       partiCfg.KVBuckets.HeartbeatBucket,
			AssignmentBucket:      partiCfg.KVBuckets.AssignmentBucket,
			HandoffBucket:         partiCfg.KVBuckets.HandoffBucket,
			WorkerIDTTL:           partiCfg.WorkerIDTTL,
			ElectionTimeout:       partiCfg.ElectionTimeout,
			HeartbeatTTL:          partiCfg.HeartbeatTTL,
			AssignmentTTL:         partiCfg.KVBuckets.AssignmentTTL,
			HandoffTTL:            partiCfg.KVBuckets.HandoffTTL,
			EnableTwoPhaseHandoff: true,
		},
	}

	return provCfg, partiCfg
}

// expectedBuckets returns the sorted bucket names for the control-plane when
// EnableTwoPhaseHandoff is true (5 total).
func expectedBuckets(suffix string) []string {
	return []string{
		"parti-assignment-" + suffix,
		"parti-election-" + suffix,
		"parti-handoff-" + suffix,
		"parti-heartbeat-" + suffix,
		"parti-stableid-" + suffix,
	}
}

// TestProvisionApply_ManagerStartsCleanly is the golden path: after
// provision.Apply creates the control-plane KV buckets, a real parti.Manager
// must reach StateStable without errors. This proves that the byte-equivalence
// between provision's bucket configs and Manager's ensureKVBucket is
// operationally meaningful.
func TestProvisionApply_ManagerStartsCleanly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	const suffix = "clean"
	provCfg, partiCfg := makeE2EConfig(t, suffix)
	buckets := expectedBuckets(suffix)

	// Step 1: Apply provisions all 5 control-plane buckets.
	rep, err := provision.Apply(ctx, js, provCfg)
	require.NoError(t, err)
	require.False(t, rep.Aborted)
	require.Empty(t, rep.Errors)
	require.Empty(t, rep.Skipped)
	require.Len(t, rep.Executed, len(buckets),
		"expected exactly %d create-kv actions (one per control-plane bucket)", len(buckets))
	for _, ex := range rep.Executed {
		require.Equal(t, provision.ActionCreateKV, ex.Kind)
		require.False(t, ex.Raced, "no races on empty server")
	}

	// Step 2: Verify each bucket exists with the correct Parti marker.
	instance := "e2e-smoke-" + suffix
	for _, bucket := range buckets {
		stream, serr := js.Stream(ctx, "KV_"+bucket)
		require.NoError(t, serr, "bucket KV_%s must exist after Apply", bucket)
		info, ierr := stream.Info(ctx)
		require.NoError(t, ierr)

		marker := provision.ParseMarker(info.Config.Metadata)
		require.Equal(t, provision.MarkerManagedValue, marker.Managed,
			"bucket %s must carry parti.io/managed=v1", bucket)
		require.Equal(t, instance, marker.Instance,
			"bucket %s must carry parti.io/instance=%s", bucket, instance)
		require.NotEmpty(t, marker.Component,
			"bucket %s must carry parti.io/component", bucket)
	}

	// Step 3: Start Manager against the provisioned buckets.
	src := source.NewStatic(testutil.CreateTestPartitions(3))
	assignStrat := strategy.NewConsistentHash()

	mgr, err := parti.NewManager(&partiCfg, js, src, assignStrat)
	require.NoError(t, err)

	require.NoError(t, mgr.Start(ctx),
		"Manager must start cleanly against provision-created buckets (byte-equivalence check)")
	require.Equal(t, parti.StateStable, mgr.State())

	// Step 4: Clean shutdown.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	require.NoError(t, mgr.Stop(stopCtx))
	require.Equal(t, parti.StateShutdown, mgr.State())
}

// TestProvisionApply_IdempotentAfterManagerStart verifies that re-running
// provision.Plan (and Apply) after Manager has been running produces zero new
// mutations and no spurious drift findings. The Manager's presence must not
// modify bucket configs in any way detectable by the provision scanner.
func TestProvisionApply_IdempotentAfterManagerStart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	const suffix = "idempotent"
	provCfg, partiCfg := makeE2EConfig(t, suffix)

	// Step 1: Apply once on empty server.
	rep, err := provision.Apply(ctx, js, provCfg)
	require.NoError(t, err)
	require.Empty(t, rep.Errors)
	require.Len(t, rep.Executed, len(expectedBuckets(suffix)))

	// Step 2: Start Manager and wait for it to reach StateStable.
	src := source.NewStatic(testutil.CreateTestPartitions(3))
	assignStrat := strategy.NewConsistentHash()

	mgr, err := parti.NewManager(&partiCfg, js, src, assignStrat)
	require.NoError(t, err)

	require.NoError(t, mgr.Start(ctx))
	require.Equal(t, parti.StateStable, mgr.State())

	// Step 3: Re-run Plan with Manager running. All buckets exist and
	// match. Expect zero create-kv actions and no drift-mutable or
	// drift-immutable findings (only informational is allowed).
	planResult, err := provision.Plan(ctx, js, provCfg)
	require.NoError(t, err)
	require.Empty(t, planResult.Actions,
		"second Plan must produce zero create-kv actions (all buckets exist)")
	for _, finding := range planResult.Drift {
		require.NotEqual(t, provision.SeverityDriftMutable, finding.Severity,
			"unexpected drift-mutable finding for bucket %q after Manager start", finding.Name)
		require.NotEqual(t, provision.SeverityDriftImmutable, finding.Severity,
			"unexpected drift-immutable finding for bucket %q after Manager start", finding.Name)
	}

	// Step 4: Apply again with Manager running. Zero executed, no errors.
	rep2, err := provision.Apply(ctx, js, provCfg)
	require.NoError(t, err)
	require.Empty(t, rep2.Executed, "second Apply must execute nothing")
	require.Empty(t, rep2.Errors)
	require.Empty(t, rep2.Skipped)
	require.False(t, rep2.Aborted)

	// Cleanup.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	require.NoError(t, mgr.Stop(stopCtx))
}

// TestProvisionApply_AdoptsManuallyCreatedBuckets verifies the "operator's
// manual setup" path. A bucket pre-created without the Parti marker is
// reported as "adopted" drift and never mutated by Apply. Manager must still
// reach StateStable when using a mix of provision-created and manually-created
// buckets.
func TestProvisionApply_AdoptsManuallyCreatedBuckets(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	const suffix = "adopted"
	provCfg, partiCfg := makeE2EConfig(t, suffix)

	// Step 1: Manually pre-create the StableID bucket WITHOUT a Parti
	// marker. Use the same config the provision builder would produce
	// (same Storage, History=1, TTL) so there is no field drift — only
	// the marker is absent.
	stableIDBucket := partiCfg.KVBuckets.StableIDBucket
	manualCfg := kvbuckets.BuildKeyValueConfig(
		stableIDBucket,
		partiCfg.WorkerIDTTL,
		jetstream.FileStorage,
	)
	// Intentionally NO Metadata — this is what makes it "manually created".
	_, err = js.CreateKeyValue(ctx, manualCfg)
	require.NoError(t, err, "pre-create %s without marker", stableIDBucket)

	// Step 2: Plan — the pre-created bucket must appear as "adopted" drift
	// with NO create-kv action for it. Other buckets appear as create-kv.
	planResult, err := provision.Plan(ctx, js, provCfg)
	require.NoError(t, err)

	allBuckets := expectedBuckets(suffix)
	// Exactly (N-1) create-kv actions: all buckets except the pre-created one.
	require.Len(t, planResult.Actions, len(allBuckets)-1,
		"Plan must emit create-kv for all buckets except the pre-created one")
	for _, action := range planResult.Actions {
		require.NotEqual(t, stableIDBucket, action.Name,
			"Plan must NOT emit create-kv for the pre-created (adopted) bucket")
	}

	// The pre-created bucket must appear as SeverityAdopted drift.
	var adoptedFound bool
	for _, finding := range planResult.Drift {
		if finding.Name == stableIDBucket {
			require.Equal(t, provision.SeverityAdopted, finding.Severity,
				"pre-created bucket %q must be reported as adopted, not %s",
				stableIDBucket, finding.Severity)
			adoptedFound = true
		}
	}
	require.True(t, adoptedFound, "Plan must contain an adopted finding for %q", stableIDBucket)

	// Step 3: Apply — only the non-pre-created buckets are created; the
	// adopted bucket is NOT touched.
	rep, err := provision.Apply(ctx, js, provCfg)
	require.NoError(t, err)
	require.False(t, rep.Aborted)
	require.Empty(t, rep.Errors)
	require.Len(t, rep.Executed, len(allBuckets)-1,
		"Apply must create all buckets except the pre-created (adopted) one")
	for _, ex := range rep.Executed {
		require.NotEqual(t, stableIDBucket, ex.Name,
			"Apply must NOT execute a create-kv for the adopted bucket")
	}

	// Step 4: Load-bearing invariant — the adopted bucket's metadata must
	// still be empty (no marker added by Apply).
	adoptedStream, serr := js.Stream(ctx, "KV_"+stableIDBucket)
	require.NoError(t, serr)
	adoptedInfo, ierr := adoptedStream.Info(ctx)
	require.NoError(t, ierr)
	require.False(t, provision.ParseMarker(adoptedInfo.Config.Metadata).IsManaged(),
		"Apply must never add the Parti marker to an adopted (pre-existing) bucket")

	// Step 5: Start Manager — must reach StateStable using a mix of
	// provision-created and manually-created buckets.
	src := source.NewStatic(testutil.CreateTestPartitions(3))
	assignStrat := strategy.NewConsistentHash()

	mgr, err := parti.NewManager(&partiCfg, js, src, assignStrat)
	require.NoError(t, err)

	require.NoError(t, mgr.Start(ctx),
		"Manager must start cleanly with one manually-created bucket (no marker)")
	require.Equal(t, parti.StateStable, mgr.State())

	// Cleanup.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	require.NoError(t, mgr.Stop(stopCtx))
}
