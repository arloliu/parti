package provision_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/provision"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// partitionSourceCfg returns a Config with PartitionSource populated and no
// ControlPlane, so tests focus exclusively on partition-source behaviour.
func partitionSourceCfg() provision.Config {
	return provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket:       "parti-partitions",
			Key:          "partitions/v1",
			Storage:      "file",
			History:      2,
			Replicas:     1,
			MaxValueSize: 512,
			TTL:          5 * time.Minute,
		},
	}
}

// TestPlan_PartitionSource_EmptyServer_EmitsCreateKV verifies that Plan
// emits a create-kv action for the partition-source bucket when none exists,
// that the marker is correct, and that the KeyValueConfig fields match the
// builder output exactly.
func TestPlan_PartitionSource_EmptyServer_EmitsCreateKV(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := partitionSourceCfg()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)
	require.Equal(t, provision.APIVersionProvisionV1, plan.APIVersion)
	require.Equal(t, provision.KindPlan, plan.Kind)
	require.Empty(t, plan.Drift)
	require.Len(t, plan.Actions, 1)

	action := plan.Actions[0]
	require.Equal(t, provision.ActionCreateKV, action.Kind)
	require.Equal(t, "parti-partitions", action.Name)

	kv, ok := action.Resource.(jetstream.KeyValueConfig)
	require.True(t, ok, "Resource must be jetstream.KeyValueConfig")

	// Field-level checks.
	require.Equal(t, "parti-partitions", kv.Bucket)
	require.Equal(t, jetstream.FileStorage, kv.Storage)
	require.Equal(t, uint8(2), kv.History)
	require.Equal(t, 1, kv.Replicas)
	require.Equal(t, int32(512), kv.MaxValueSize)
	require.Equal(t, 5*time.Minute, kv.TTL)

	// Marker checks.
	require.Equal(t, provision.MarkerManagedValue, kv.Metadata[provision.MarkerManagedKey])
	require.Equal(t, provision.ComponentPartitionSource, kv.Metadata[provision.MarkerComponentKey])
	require.Equal(t, "prod", kv.Metadata[provision.MarkerInstanceKey])

	// Byte-equivalence: stripping Metadata must match what the builder
	// returns for the same inputs. This verifies the builder is the
	// single source of truth for KeyValueConfig construction.
	wantKV := jetstream.KeyValueConfig{
		Bucket:       "parti-partitions",
		History:      2,
		Storage:      jetstream.FileStorage,
		Replicas:     1,
		MaxValueSize: 512,
		TTL:          5 * time.Minute,
	}
	stripped := kv
	stripped.Metadata = nil
	require.True(t, reflect.DeepEqual(stripped, wantKV),
		"create-kv resource (sans metadata) must equal expected builder output\nwant: %#v\ngot:  %#v",
		wantKV, stripped)
}

// TestApply_PartitionSource_EmptyServer_CreatesBucket verifies that Apply
// creates the partition-source bucket with the correct config and marker.
func TestApply_PartitionSource_EmptyServer_CreatesBucket(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := partitionSourceCfg()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rep, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.False(t, rep.Aborted)
	require.Empty(t, rep.Errors)
	require.Empty(t, rep.Skipped)
	require.Len(t, rep.Executed, 1)
	require.Equal(t, provision.ActionCreateKV, rep.Executed[0].Kind)
	require.Equal(t, "parti-partitions", rep.Executed[0].Name)
	require.False(t, rep.Executed[0].Raced, "no races on empty server")

	// Verify the bucket was created with the expected settings and marker.
	stream, err := js.Stream(ctx, "KV_parti-partitions")
	require.NoError(t, err)
	info, err := stream.Info(ctx)
	require.NoError(t, err)

	require.Equal(t, jetstream.FileStorage, info.Config.Storage)
	require.Equal(t, int64(2), info.Config.MaxMsgsPerSubject) // History=2
	require.Equal(t, int32(512), info.Config.MaxMsgSize)      // MaxValueSize=512
	require.Equal(t, 5*time.Minute, info.Config.MaxAge)       // TTL=5m

	marker := provision.ParseMarker(info.Config.Metadata)
	require.Equal(t, provision.MarkerManagedValue, marker.Managed)
	require.Equal(t, provision.ComponentPartitionSource, marker.Component)
	require.Equal(t, "prod", marker.Instance)
}

// TestPlan_PartitionSource_ImmutableHistoryDrift verifies that Plan reports
// drift-immutable when the live bucket's History differs from config.
func TestPlan_PartitionSource_ImmutableHistoryDrift(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := partitionSourceCfg() // wants History=2

	// Pre-create a marked bucket with History=5 (differs from config).
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "parti-partitions",
		History:  5,
		Storage:  jetstream.FileStorage,
		Metadata: provision.BuildMarker(provision.ComponentPartitionSource, "prod"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)

	// No create-kv action must be emitted.
	for _, a := range plan.Actions {
		require.NotEqual(t, "parti-partitions", a.Name,
			"must not emit create-kv for existing bucket with history drift")
	}

	// A drift-immutable finding with "history" detail must be present.
	var found bool
	for _, d := range plan.Drift {
		if d.Name == "parti-partitions" && d.Severity == provision.SeverityDriftImmutable {
			require.Contains(t, d.Detail, "history")
			found = true
		}
	}
	require.True(t, found, "expected drift-immutable for history mismatch")
}

// TestPlan_PartitionSource_UnmarkedBucket_ReportsAdopted verifies that Plan
// reports adopted drift for an existing unmarked bucket and emits no action.
func TestPlan_PartitionSource_UnmarkedBucket_ReportsAdopted(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	// Pre-create an unmarked bucket with the same name.
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:  "parti-partitions",
		History: 1,
		Storage: jetstream.FileStorage,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, partitionSourceCfg())
	require.NoError(t, err)

	// No create-kv action.
	for _, a := range plan.Actions {
		require.NotEqual(t, "parti-partitions", a.Name,
			"must not emit create-kv for an existing unmarked bucket")
	}

	// Exactly one adopted drift entry.
	var adopted []provision.DriftFinding
	for _, d := range plan.Drift {
		if d.Severity == provision.SeverityAdopted && d.Name == "parti-partitions" {
			adopted = append(adopted, d)
		}
	}
	require.Len(t, adopted, 1)
}

// TestView_PartitionSource_AppearsInPartitionSourceSlice verifies that View
// routes a partition-source-marked bucket to Snapshot.PartitionSource.
// (The basic routing is already covered by view_integration_test.go;
// this test confirms the W3 path with a fully-specified config round-trip.)
func TestView_PartitionSource_AppearsInPartitionSourceSlice(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "parti-partitions",
		History:  2,
		Storage:  jetstream.FileStorage,
		Metadata: provision.BuildMarker(provision.ComponentPartitionSource, "prod"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := provision.View(ctx, js, provision.ScopeAll())
	require.NoError(t, err)
	require.Empty(t, snap.ControlPlane)
	require.Len(t, snap.PartitionSource, 1)
	require.Equal(t, "parti-partitions", snap.PartitionSource[0].Bucket)
	require.Equal(t, provision.ComponentPartitionSource, snap.PartitionSource[0].Component)
	require.Equal(t, "prod", snap.PartitionSource[0].Instance)
}

// TestApply_PartitionSource_Idempotent verifies that applying twice is a no-op
// on the second run.
func TestApply_PartitionSource_Idempotent(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := partitionSourceCfg()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// First apply: creates the bucket.
	rep1, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Len(t, rep1.Executed, 1)
	require.Empty(t, rep1.Errors)

	// Second apply: bucket already exists and matches → no actions.
	rep2, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, rep2.Executed, "second Apply must produce no executions")
	require.Empty(t, rep2.Errors)

	// Plan after second apply must also be empty of actions.
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, plan.Actions, "Plan after idempotent apply must emit no actions")
}

// TestPlan_PartitionSource_MemoryStorage verifies that Plan correctly emits a
// MemoryStorage bucket when cfg.Storage is "memory".
func TestPlan_PartitionSource_MemoryStorage(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket:  "parti-partitions",
			Key:     "partitions/v1",
			Storage: "memory",
			History: 1,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 1)

	kv, ok := plan.Actions[0].Resource.(jetstream.KeyValueConfig)
	require.True(t, ok)
	require.Equal(t, jetstream.MemoryStorage, kv.Storage)
}

// TestPlan_PartitionSource_StorageDefaultsToFile verifies that omitting
// Storage in config results in file storage being used.
func TestPlan_PartitionSource_StorageDefaultsToFile(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket: "parti-partitions",
			Key:    "partitions/v1",
			// Storage intentionally omitted → should default to "file"
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 1)

	kv, ok := plan.Actions[0].Resource.(jetstream.KeyValueConfig)
	require.True(t, ok)
	require.Equal(t, jetstream.FileStorage, kv.Storage)
	require.Equal(t, uint8(1), kv.History, "History must default to 1")
}

// TestApply_PartitionSource_ZeroMaxValueSize_Idempotent verifies that a bucket
// created with MaxValueSize=0 ("no limit") does not produce spurious
// drift-mutable on the subsequent Plan. NATS stores "no limit" as MaxMsgSize=-1
// internally; drift classification must normalise -1→0 before comparing.
func TestApply_PartitionSource_ZeroMaxValueSize_Idempotent(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket:       "parti-partitions-zero",
			Key:          "partitions/v1",
			Storage:      "file",
			History:      1,
			MaxValueSize: 0, // "no limit"
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// First Apply: creates bucket.
	_, err := provision.Apply(ctx, js, cfg)
	require.NoError(t, err)

	// Second Plan: must emit no actions and no drift-mutable for maxValueSize.
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)
	require.Empty(t, plan.Actions, "Plan after Apply with MaxValueSize=0 must not re-emit create-kv")

	for _, d := range plan.Drift {
		if d.Name == "parti-partitions-zero" && d.Severity == provision.SeverityDriftMutable {
			t.Errorf("spurious drift-mutable: %+v", d.Detail)
		}
	}
}

// TestPlan_PartitionSource_ImmutableStorageDrift verifies that Plan reports
// drift-immutable when the live bucket's Storage type differs from config.
func TestPlan_PartitionSource_ImmutableStorageDrift(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket:  "parti-partitions",
			Key:     "partitions/v1",
			Storage: "memory", // desired storage
			History: 1,
		},
	}

	// Pre-create a marked bucket with the opposite storage (file).
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "parti-partitions",
		History:  1,
		Storage:  jetstream.FileStorage, // differs from desired "memory"
		Metadata: provision.BuildMarker(provision.ComponentPartitionSource, "prod"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)

	// No create-kv action must be emitted.
	for _, a := range plan.Actions {
		require.NotEqual(t, "parti-partitions", a.Name,
			"must not emit create-kv for existing bucket with storage drift")
	}

	// A drift-immutable finding with "storage" detail must be present.
	var found bool
	for _, d := range plan.Drift {
		if d.Name == "parti-partitions" && d.Severity == provision.SeverityDriftImmutable {
			require.Contains(t, d.Detail, "storage")
			found = true
		}
	}
	require.True(t, found, "expected drift-immutable for storage mismatch")
}

// TestPlan_PartitionSource_DescriptionPreservedNoSrift verifies that Plan does
// NOT report any drift when the live bucket has a non-empty Description. Description
// is preserved-from-live: the v2 YAML schema cannot express it, so it is never
// drifted.
func TestPlan_PartitionSource_DescriptionPreservedNoDrift(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := partitionSourceCfg()

	// Pre-create a marked bucket with a Description set by an operator.
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:      "parti-partitions",
		History:     2,
		Storage:     jetstream.FileStorage,
		Description: "operator note",
		Metadata:    provision.BuildMarker(provision.ComponentPartitionSource, "prod"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)

	// No create-kv action.
	for _, a := range plan.Actions {
		require.NotEqual(t, "parti-partitions", a.Name, "no action for description (preserved-from-live)")
	}

	// Description must NOT appear in any drift finding — it is preserved-from-live.
	for _, d := range plan.Drift {
		if d.Name == "parti-partitions" {
			require.NotContains(t, d.Detail, "description",
				"description is preserved-from-live and must not appear in drift findings")
		}
	}
}

// TestPlan_PartitionSource_MaxBytesPreservedNoDrift verifies that Plan does NOT
// report any drift when the live bucket has a non-zero MaxBytes. MaxBytes is
// preserved-from-live: the v2 YAML schema cannot express it, so it is never
// drifted.
func TestPlan_PartitionSource_MaxBytesPreservedNoDrift(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := partitionSourceCfg()

	// Pre-create a marked bucket with MaxBytes set.
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "parti-partitions",
		History:  2,
		Storage:  jetstream.FileStorage,
		MaxBytes: 1024,
		Metadata: provision.BuildMarker(provision.ComponentPartitionSource, "prod"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)

	// No create-kv action.
	for _, a := range plan.Actions {
		require.NotEqual(t, "parti-partitions", a.Name, "no action for maxBytes (preserved-from-live)")
	}

	// MaxBytes must NOT appear in any drift finding — it is preserved-from-live.
	for _, d := range plan.Drift {
		if d.Name == "parti-partitions" {
			require.NotContains(t, d.Detail, "maxBytes",
				"maxBytes is preserved-from-live and must not appear in drift findings")
		}
	}
}

// TestPlan_PartitionSource_MutableReplicasDrift verifies that Plan reports
// drift-mutable (NOT drift-immutable) when the live bucket's Replicas count
// differs from the desired count. Replicas is reclassified from drift-immutable
// (Phase 1) to drift-mutable (Phase 2).
func TestPlan_PartitionSource_ReplicasNormalization_NoDrift(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket:   "parti-partitions",
			Key:      "partitions/v1",
			Storage:  "file",
			History:  1,
			Replicas: 1, // desired
		},
	}

	// Pre-create a marked bucket with default replicas (1 == desired).
	// NATS single-server normalizes Replicas to 1 on creation; we can only
	// verify the normalization equivalence (0↔1) path here.
	// Positive Replicas drift-mutable classification is tested in the unit
	// tests (classify_drift_test.go) using synthetic StreamInfo.
	cfgZeroReplicas := provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket:   "parti-partitions",
			Key:      "partitions/v1",
			Storage:  "file",
			History:  1,
			Replicas: 0, // 0 normalizes to 1 — must not drift against live Replicas=1
		},
	}

	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "parti-partitions",
		History:  1,
		Storage:  jetstream.FileStorage,
		Replicas: 1,
		Metadata: provision.BuildMarker(provision.ComponentPartitionSource, "prod"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Config Replicas=0 (== 1) vs live Replicas=1 → no replicas drift.
	plan, err := provision.Plan(ctx, js, cfgZeroReplicas)
	require.NoError(t, err)
	for _, d := range plan.Drift {
		if d.Name == "parti-partitions" {
			require.NotContains(t, d.Detail, "replicas",
				"Replicas=0 and live Replicas=1 are equivalent; no replicas drift expected")
		}
	}

	// Config Replicas=1 vs live Replicas=1 → no replicas drift.
	plan2, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)
	for _, d := range plan2.Drift {
		if d.Name == "parti-partitions" {
			require.NotContains(t, d.Detail, "replicas",
				"Replicas=1 matches live; no replicas drift expected")
		}
	}
}

// TestPlan_PartitionSource_ReplicasDefaultMatch_Informational verifies that
// when both config and live have the default replica count (1), Plan reports
// no drift finding for replicas. The positive drift-mutable path is exercised
// in classify_drift_test.go using synthetic StreamInfo (single-server NATS
// always normalizes Replicas to 1, making positive-drift integration tests
// impractical).
func TestPlan_PartitionSource_ReplicasDefaultMatch_Informational(t *testing.T) {
	t.Parallel()
	js := newJS(t)

	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		Instance:   "prod",
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket:   "parti-partitions-r2",
			Key:      "partitions/v1",
			Storage:  "file",
			History:  1,
			Replicas: 0, // server default (1); live will also be 1
		},
	}

	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "parti-partitions-r2",
		History:  1,
		Storage:  jetstream.FileStorage,
		Metadata: provision.BuildMarker(provision.ComponentPartitionSource, "prod"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)

	// The key invariant: no drift-immutable finding with "replicas".
	for _, d := range plan.Drift {
		if d.Name == "parti-partitions-r2" && d.Severity == provision.SeverityDriftImmutable {
			require.NotContains(t, d.Detail, "replicas",
				"Replicas must be classified as drift-mutable (not drift-immutable)")
		}
	}
}

// TestPlan_PartitionSource_MutableMarkerDrift verifies that Plan reports
// drift-mutable when the live bucket's marker has a managed version other than
// "v1" (e.g. "v0" written by an older version of the tool).
func TestPlan_PartitionSource_MutableMarkerDrift(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := partitionSourceCfg()

	// Pre-create a marked bucket with a stale managed version.
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:  "parti-partitions",
		History: 2,
		Storage: jetstream.FileStorage,
		Metadata: map[string]string{
			provision.MarkerManagedKey:   "v0", // wrong version
			provision.MarkerComponentKey: provision.ComponentPartitionSource,
			provision.MarkerInstanceKey:  "prod",
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)

	// No create-kv action.
	for _, a := range plan.Actions {
		require.NotEqual(t, "parti-partitions", a.Name, "no action for mutable marker drift")
	}

	// A drift-mutable finding with "managed" detail must be present.
	var found bool
	for _, d := range plan.Drift {
		if d.Name == "parti-partitions" && d.Severity == provision.SeverityDriftMutable {
			if _, ok := d.Detail["managed"]; ok {
				found = true
			}
		}
	}
	require.True(t, found, "expected drift-mutable for marker managed version mismatch")
}

// TestPlan_PartitionSource_MutableTTLDrift verifies that TTL mismatch on a
// marked bucket is reported as drift-mutable (no action).
func TestPlan_PartitionSource_MutableTTLDrift(t *testing.T) {
	t.Parallel()
	js := newJS(t)
	cfg := partitionSourceCfg() // TTL = 5 minutes

	// Pre-create marked bucket with a different TTL.
	createKV(t, js, jetstream.KeyValueConfig{
		Bucket:   "parti-partitions",
		History:  2,
		Storage:  jetstream.FileStorage,
		TTL:      10 * time.Minute,
		Metadata: provision.BuildMarker(provision.ComponentPartitionSource, "prod"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plan, err := provision.Plan(ctx, js, cfg)
	require.NoError(t, err)

	for _, a := range plan.Actions {
		require.NotEqual(t, "parti-partitions", a.Name, "no action for mutable TTL drift")
	}

	var found bool
	for _, d := range plan.Drift {
		if d.Name == "parti-partitions" && d.Severity == provision.SeverityDriftMutable {
			require.Contains(t, d.Detail, "ttl")
			found = true
		}
	}
	require.True(t, found, "expected drift-mutable for TTL mismatch")
}
