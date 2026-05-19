package provision_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/kvbuckets"
	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// planTestConfig builds a Config with all five control-plane buckets enabled
// (handoff on) and distinct fixed values so byte-equivalence checks are
// unambiguous.
func planTestConfig() provision.Config {
	return provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		ControlPlane: &provision.ControlPlaneConfig{
			StableIDBucket:        "parti-stableid",
			ElectionBucket:        "parti-election",
			HeartbeatBucket:       "parti-heartbeat",
			AssignmentBucket:      "parti-assignment",
			HandoffBucket:         "parti-handoff",
			WorkerIDTTL:           75 * time.Second,
			ElectionTimeout:       10 * time.Second,
			HeartbeatTTL:          15 * time.Second,
			AssignmentTTL:         0,
			HandoffTTL:            2 * time.Minute,
			EnableTwoPhaseHandoff: true,
		},
	}
}

func TestPlan_EmptyServer_EmitsCreateKVForAllControlPlaneBuckets(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := planTestConfig()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)
	require.Equal(t, provision.APIVersionProvisionV1, plan.APIVersion)
	require.Equal(t, provision.KindPlan, plan.Kind)
	require.Empty(t, plan.Drift)

	expected := []struct {
		bucket    string
		ttl       time.Duration
		storage   jetstream.StorageType
		component string
	}{
		{"parti-assignment", 0, jetstream.FileStorage, provision.ComponentControlPlaneAssignment},
		{"parti-election", 10 * time.Second, jetstream.MemoryStorage, provision.ComponentControlPlaneElection},
		{"parti-handoff", 2 * time.Minute, jetstream.FileStorage, provision.ComponentControlPlaneHandoff},
		{"parti-heartbeat", 15 * time.Second, jetstream.MemoryStorage, provision.ComponentControlPlaneHeartbeat},
		{"parti-stableid", 75 * time.Second, jetstream.FileStorage, provision.ComponentControlPlaneID},
	}
	require.Len(t, plan.Actions, len(expected))

	// Plan sorts actions by (Kind, Name). All Kinds are "create-kv" so the
	// resulting order is purely by Name lexicographic.
	for i, want := range expected {
		got := plan.Actions[i]
		require.Equal(t, provision.ActionCreateKV, got.Kind)
		require.Equal(t, want.bucket, got.Name)

		kv, ok := got.Resource.(jetstream.KeyValueConfig)
		require.True(t, ok, "action %d Resource is not jetstream.KeyValueConfig", i)

		// Byte-equivalence invariant: stripping Metadata yields exactly the
		// shared builder output for the same inputs.
		stripped := kv
		stripped.Metadata = nil
		shared := kvbuckets.BuildKeyValueConfig(want.bucket, want.ttl, want.storage)
		require.True(t, reflect.DeepEqual(stripped, shared),
			"create-kv resource (sans metadata) must equal kvbuckets.BuildKeyValueConfig output\nwant: %#v\ngot:  %#v",
			shared, stripped)

		// Marker contents.
		require.Equal(t, provision.MarkerManagedValue, kv.Metadata[provision.MarkerManagedKey])
		require.Equal(t, want.component, kv.Metadata[provision.MarkerComponentKey])
		require.Equal(t, "prod", kv.Metadata[provision.MarkerInstanceKey])
	}
}

func TestPlan_HandoffDisabled_OmitsHandoffAction(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := planTestConfig()
	cfg.ControlPlane.EnableTwoPhaseHandoff = false
	cfg.ControlPlane.HandoffTTL = 0

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)
	for _, a := range plan.Actions {
		require.NotEqual(t, "parti-handoff", a.Name,
			"handoff bucket must not appear when EnableTwoPhaseHandoff=false")
	}
	require.Len(t, plan.Actions, 4)
}

func TestPlan_UnmarkedExistingBucket_ReportsAdoptedNoAction(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	// Manually create an unmarked bucket with the same name as
	// parti-stableid. Plan must report this as `adopted` drift and emit
	// no `create-kv` action for that name.
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:  "parti-stableid",
		History: 1,
		Storage: jetstream.FileStorage,
		TTL:     75 * time.Second,
	})

	cfg := planTestConfig()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)

	for _, a := range plan.Actions {
		require.NotEqual(t, "parti-stableid", a.Name,
			"plan must not emit create-kv for an existing unmarked bucket")
	}

	// Other buckets still get create-kv.
	names := map[string]bool{}
	for _, a := range plan.Actions {
		names[a.Name] = true
	}
	require.True(t, names["parti-election"])
	require.True(t, names["parti-heartbeat"])
	require.True(t, names["parti-assignment"])
	require.True(t, names["parti-handoff"])

	// Drift: exactly one `adopted` entry for parti-stableid.
	var adopted []provision.DriftFinding
	for _, d := range plan.Drift {
		if d.Severity == provision.SeverityAdopted {
			adopted = append(adopted, d)
		}
	}
	require.Len(t, adopted, 1)
	require.Equal(t, "parti-stableid", adopted[0].Name)
}

func TestPlan_MarkedBucketMatchingConfig_ReportsInformational(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	// Build the parti-election bucket exactly as Plan would.
	want := kvbuckets.BuildKeyValueConfig("parti-election", 10*time.Second, jetstream.MemoryStorage)
	want.Metadata = provision.BuildMarker(provision.ComponentControlPlaneElection, "prod")
	createKV(t, js, want)

	cfg := planTestConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)

	// No create-kv for parti-election.
	for _, a := range plan.Actions {
		require.NotEqual(t, "parti-election", a.Name)
	}
	// Drift contains exactly one informational entry for parti-election.
	var info []provision.DriftFinding
	for _, d := range plan.Drift {
		if d.Name == "parti-election" {
			info = append(info, d)
		}
	}
	require.Len(t, info, 1)
	require.Equal(t, provision.SeverityInformational, info[0].Severity)
}

func TestPlan_MismatchedTTL_ReportsDriftMutableNoAction(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	// Create parti-election with a mismatching TTL (mutable drift in v2).
	live := kvbuckets.BuildKeyValueConfig("parti-election", 30*time.Second, jetstream.MemoryStorage)
	live.Metadata = provision.BuildMarker(provision.ComponentControlPlaneElection, "prod")
	createKV(t, js, live)

	cfg := planTestConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)

	for _, a := range plan.Actions {
		require.NotEqual(t, "parti-election", a.Name, "no update-* action should be emitted in v1")
	}
	// Drift carries drift-mutable for parti-election with a "ttl" detail.
	var found bool
	for _, d := range plan.Drift {
		if d.Name == "parti-election" && d.Severity == provision.SeverityDriftMutable {
			require.Contains(t, d.Detail, "ttl")
			found = true
		}
	}
	require.True(t, found, "expected drift-mutable for parti-election")
}

func TestPlan_MismatchedStorage_ReportsImmutableNoAction(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	// Election bucket should be MemoryStorage; create as FileStorage.
	live := kvbuckets.BuildKeyValueConfig("parti-election", 10*time.Second, jetstream.FileStorage)
	live.Metadata = provision.BuildMarker(provision.ComponentControlPlaneElection, "prod")
	createKV(t, js, live)

	cfg := planTestConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)

	for _, a := range plan.Actions {
		require.NotEqual(t, "parti-election", a.Name)
	}
	var found bool
	for _, d := range plan.Drift {
		if d.Name == "parti-election" && d.Severity == provision.SeverityDriftImmutable {
			require.Contains(t, d.Detail, "storage")
			found = true
		}
	}
	require.True(t, found, "expected drift-immutable for parti-election storage")
}

func TestPlan_Deterministic_AcrossRepeats(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := planTestConfig()
	// Add a partition-source so it is included in the determinism check
	// (W3 emits a create-kv for it when the bucket is absent).
	cfg.PartitionSource = &provision.PartitionSourceConfig{Bucket: "parti-partitions", Key: "partitions/v1"}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Pre-create some buckets in a specific order to introduce drift into the
	// plan result, which exercises the drift-sorting path.  Create in
	// non-alphabetical order so map-iteration variance in Plan is observable.
	//
	// Run 1: unmarked parti-heartbeat → adopted drift.
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:  "parti-heartbeat",
		History: 1,
		Storage: jetstream.MemoryStorage,
		TTL:     15 * time.Second,
	})
	// Run 1: marked parti-election with mismatched TTL → drift-mutable.
	liveElection := kvbuckets.BuildKeyValueConfig("parti-election", 30*time.Second, jetstream.MemoryStorage)
	liveElection.Metadata = provision.BuildMarker(provision.ComponentControlPlaneElection, "prod")
	createKV(t, js, liveElection)

	first, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)

	// Call Plan multiple more times (exploiting Go map-iteration randomness)
	// and assert full PlanResult equality, not just name/kind tuples.
	for i := 0; i < 9; i++ {
		again, err := provision.Plan(ctx, js, cfg)
		require.NoError(t, err)
		require.True(t, reflect.DeepEqual(first, again),
			"Plan must be fully deterministic across repeated calls (iteration %d):\nfirst=%#v\nagain=%#v",
			i+1, first, again)
	}
}

func TestPlan_CancelledContext(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan, err := provision.Plan(ctx, js, planTestConfig())
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, plan)
}

func TestPlan_DifferentNameBucketStillGetsCreateKV(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	// Pre-create an unrelated unmarked bucket (not named by config).
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:  "some-other-bucket",
		History: 1,
		Storage: jetstream.MemoryStorage,
	})

	cfg := planTestConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)

	// All five control-plane buckets get create-kv (the unrelated bucket
	// is invisible to a config-scoped Plan).
	require.Len(t, plan.Actions, 5)
	require.Empty(t, plan.Drift, "unrelated buckets must not appear in Plan drift")
}
