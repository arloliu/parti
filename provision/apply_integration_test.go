package provision_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/kvbuckets"
	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func applyCfg() provision.Config {
	return provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		ControlPlane: &provision.ControlPlaneConfig{
			WorkerIDTTL:           75 * time.Second,
			ElectionTimeout:       10 * time.Second,
			HeartbeatTTL:          15 * time.Second,
			AssignmentTTL:         0,
			HandoffTTL:            2 * time.Minute,
			EnableTwoPhaseHandoff: true,
		},
	}
}

// expectedControlPlaneNamesHandoffOn is the alphabetically-sorted set of
// bucket names Plan emits for the applyCfg() Config (handoff enabled).
var expectedControlPlaneNamesHandoffOn = []string{
	"parti-assignment",
	"parti-election",
	"parti-handoff",
	"parti-heartbeat",
	"parti-stableid",
}

func TestApply_EmptyServer_CreatesAllControlPlaneBuckets(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := applyCfg()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Equal(t, provision.APIVersionProvisionV1, rep.APIVersion)
	require.Equal(t, provision.KindReport, rep.Kind)
	require.False(t, rep.Aborted)
	require.Empty(t, rep.Errors)
	require.Empty(t, rep.Skipped)
	require.Len(t, rep.Executed, len(expectedControlPlaneNamesHandoffOn))

	gotNames := map[string]bool{}
	for _, ex := range rep.Executed {
		require.Equal(t, provision.ActionCreateKV, ex.Kind)
		require.False(t, ex.Raced, "no races on empty server")
		gotNames[ex.Name] = true
	}
	for _, want := range expectedControlPlaneNamesHandoffOn {
		require.True(t, gotNames[want], "expected create-kv for %q", want)
	}

	// Verify each bucket carries the Parti marker.
	for _, name := range expectedControlPlaneNamesHandoffOn {
		stream, err := js.Stream(ctx, "KV_"+name)
		require.NoError(t, err)
		info, err := stream.Info(ctx)
		require.NoError(t, err)
		marker := provision.ParseMarker(info.Config.Metadata)
		require.Equal(t, provision.MarkerManagedValue, marker.Managed,
			"bucket %s must be stamped as parti-managed", name)
		require.NotEmpty(t, marker.Component)
		require.Equal(t, "prod", marker.Instance)

		// The handoff bucket must be provisioned with no MaxAge: a bucket-level
		// TTL would age out stable ownership claims and suppress pull-gated
		// consumers. ControlPlane.HandoffTTL is the advisory sweep TTL only.
		if name == "parti-handoff" {
			require.Equal(t, time.Duration(0), info.Config.MaxAge,
				"handoff bucket must be provisioned with no MaxAge")
		}
	}
}

func TestApply_Idempotent_SecondRunIsNoOp(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := applyCfg()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Len(t, first.Executed, len(expectedControlPlaneNamesHandoffOn))

	second, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, second.Executed)
	require.Empty(t, second.Errors)
	require.Empty(t, second.Skipped)
	require.False(t, second.Aborted)
}

func TestApply_PreCreatedBucket_RaceTolerated(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := applyCfg()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Pre-create parti-election out-of-band BUT with a non-matching
	// metadata map so Plan still emits a create-kv action for the bucket.
	// We achieve this by creating the bucket WITHOUT the Parti marker
	// (Plan reports `adopted` drift but skips the create); to force a
	// race-on-create we instead pre-create with mismatching metadata so
	// Plan still emits the action — but Plan looks up by name, so any
	// existing stream skips the action.
	//
	// Instead, simulate the race by:
	//   1. Running Plan once to get the desired action list,
	//   2. Pre-creating one of those buckets out-of-band with the same
	//      KeyValueConfig,
	//   3. Calling applyPlan on the original plan.
	// Since applyPlan is unexported, we instead invoke Apply twice and
	// inject a race between them via a parallel goroutine. A simpler
	// equivalent: pre-create one bucket directly with the same NATS
	// name, then call Apply against a freshly-instantiated Plan whose
	// snapshot has not yet observed the bucket. We mimic that by
	// pre-creating the bucket with explicit, non-Parti metadata so the
	// Parti-managed marker is NOT present — Plan still considers the
	// bucket as `adopted` (skipping it) and Apply observes no race.
	//
	// To actually exercise the §2.E branch we instead use a thin
	// raceJetStream wrapper (declared below) that proxies all calls but
	// inserts a pre-create on the first CreateKeyValue. This guarantees
	// the bucket exists at Apply time even though Plan saw it absent.
	wrapped := newRaceOnceJS(js, "KV_parti-election")
	rep, err := provision.Apply(ctx, wrapped, cfg)
	require.NoError(t, err)
	require.False(t, rep.Aborted)
	require.Empty(t, rep.Errors)
	require.Empty(t, rep.Skipped)
	require.Len(t, rep.Executed, len(expectedControlPlaneNamesHandoffOn))

	var racedCount int
	for _, ex := range rep.Executed {
		if ex.Name == "parti-election" {
			require.True(t, ex.Raced, "parti-election should be marked raced")
			racedCount++
		} else {
			require.False(t, ex.Raced, "non-raced action %q should not be flagged", ex.Name)
		}
	}
	require.Equal(t, 1, racedCount)
}

func TestApply_PreMutationCancel_AllSkipped(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := applyCfg()

	// Apply runs ValidateLive + Plan against the same ctx; to model a
	// pre-mutation cancellation we inject the cancel at the moment the
	// FIRST CreateKeyValue would fire, so ValidateLive and Plan see a
	// live context and Apply hits ctx.Err() at the iteration head
	// (yielding zero Executed entries and every action in Skipped).
	ctx, cancel := context.WithCancel(context.Background())
	wrapped := newCancelOnFirstCreateJS(js, cancel)

	rep, err := provision.Apply(ctx, wrapped, cfg)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, rep.Aborted)
	require.Empty(t, rep.Errors)
	require.Empty(t, rep.Executed)
	require.Len(t, rep.Skipped, len(expectedControlPlaneNamesHandoffOn))
	for _, sk := range rep.Skipped {
		require.Equal(t, provision.SkipReasonContextCancelled, sk.Reason)
	}
}

func TestApply_MidMutationCancel_PartialExecuted(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := applyCfg()

	ctx, cancel := context.WithCancel(context.Background())
	wrapped := newCancelAfterNthCreateJS(js, 2, cancel)

	rep, err := provision.Apply(ctx, wrapped, cfg)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, rep.Aborted)
	require.Empty(t, rep.Errors)
	require.Equal(t, 2, len(rep.Executed),
		"first 2 actions should have completed before cancellation")
	require.Equal(t, len(expectedControlPlaneNamesHandoffOn)-2, len(rep.Skipped))
	for _, sk := range rep.Skipped {
		require.Equal(t, provision.SkipReasonContextCancelled, sk.Reason)
	}
}

func TestApply_NonCancellationFailure_FailFast(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := applyCfg()

	// Pre-create a NON-KV stream that conflicts with parti-election's
	// KV stream name (KV_parti-election). This makes the underlying
	// CreateKeyValue call return ErrStreamNameAlreadyInUse — but we
	// CANNOT use that path because §2.E classifies it as a race
	// success. To force a true server-side rejection, we pre-create a
	// stream that conflicts on the SUBJECT space ($KV.parti-election.>)
	// without using the KV-derived name; the KV create then fails with
	// a server error that is NOT ErrStreamNameAlreadyInUse / ErrBucketExists.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	subjectStream := jetstream.StreamConfig{
		Name:     "shadow-election",
		Subjects: []string{"$KV.parti-election.>"},
		Storage:  jetstream.MemoryStorage,
	}
	_, err := js.CreateStream(ctx, subjectStream)
	require.NoError(t, err)

	rep, err := provision.Apply(ctx, js, cfg)
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
	require.False(t, errors.Is(err, context.DeadlineExceeded))
	// Pin the test against a future nats.go change that might silently
	// move the subject-conflict error into the race-tolerated branch.
	// The shadow-stream technique must always produce a true failure, not
	// ErrBucketExists / ErrStreamNameAlreadyInUse.
	require.False(t, errors.Is(err, jetstream.ErrBucketExists),
		"fail-fast test must exercise a non-race error class")
	require.False(t, errors.Is(err, jetstream.ErrStreamNameAlreadyInUse),
		"fail-fast test must exercise a non-race error class")
	require.False(t, rep.Aborted)
	require.Len(t, rep.Errors, 1)
	require.Equal(t, "parti-election", rep.Errors[0].Name)
	require.Equal(t, provision.ActionCreateKV, rep.Errors[0].Kind)

	// Pre-error executions: parti-assignment (sorts before parti-election).
	for _, ex := range rep.Executed {
		require.NotEqual(t, "parti-election", ex.Name)
		require.False(t, ex.Raced)
	}
	// All actions after the failing one go to Skipped with prior-error.
	require.NotEmpty(t, rep.Skipped)
	for _, sk := range rep.Skipped {
		require.Equal(t, provision.SkipReasonPriorError, sk.Reason)
		require.NotEqual(t, "parti-election", sk.Name)
	}
	require.Equal(t,
		len(expectedControlPlaneNamesHandoffOn),
		len(rep.Executed)+len(rep.Errors)+len(rep.Skipped),
		"every planned action accounted for exactly once")
}

func TestApply_StaticValidateFailure_ReturnsImmediately(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := applyCfg()
	cfg.APIVersion = "" // unknown apiVersion → normalize fails

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep, err := provision.Apply(ctx, js, cfg)
	require.Error(t, err)
	require.ErrorIs(t, err, provision.ErrInvalidConfig)
	require.Zero(t, rep)
}

// TestApply_ValidateLiveFailure_ReturnsZeroReport verifies sub-spec §2.A:
// when ValidateLive fails (pre-mutation), Apply must return Report{} (zero
// value), not a populated mutation-shaped report. The pre-fix code returned
// liveReport which contained ResourceError entries.
func TestApply_ValidateLiveFailure_ReturnsZeroReport(t *testing.T) {
	t.Parallel()
	// Wrap JS to fail at AccountInfo (step 1.B reachability probe).
	js := newFailAccountInfoJS(newJS(t))
	cfg := applyCfg()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep, err := provision.Apply(ctx, js, cfg)
	require.Error(t, err)
	// No ErrInvalidConfig (static validation passed).
	require.False(t, errors.Is(err, provision.ErrInvalidConfig))
	// Sub-spec §2.A: pre-mutation failure → zero-value Report.
	require.Zero(t, rep,
		"Apply must return Report{} (zero value) when ValidateLive fails before mutation")
}

func TestApply_AlreadyExistingMarkedBucket_NotIncludedInExecuted(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := applyCfg()

	// Pre-create parti-election EXACTLY as Plan would, including marker.
	want := kvbuckets.BuildKeyValueConfig("parti-election", 10*time.Second, jetstream.MemoryStorage)
	want.Metadata = provision.BuildMarker(provision.ComponentControlPlaneElection, "prod")
	createKV(t, js, want)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.False(t, rep.Aborted)
	require.Empty(t, rep.Errors)
	require.Empty(t, rep.Skipped)
	// 4 of 5 actions execute (parti-election skipped by Plan with
	// informational drift).
	require.Len(t, rep.Executed, len(expectedControlPlaneNamesHandoffOn)-1)
	for _, ex := range rep.Executed {
		require.NotEqual(t, "parti-election", ex.Name)
	}
}
