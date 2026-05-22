package provision_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/partcodec"
	"github.com/arloliu/parti/v2/provision"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

const (
	psBucket = "parti-partitions"
	psKey    = "partitions/v1"
)

func partitionsConfig(parts ...types.Partition) provision.Config {
	return provision.Config{
		APIVersion: provision.APIVersionV1,
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket:     psBucket,
			Key:        psKey,
			Partitions: parts,
		},
	}
}

// writePartitionKey seeds the partition-source key with the given table using
// the shared wire codec.
func writePartitionKey(t *testing.T, js jetstream.JetStream, parts []types.Partition) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data, err := partcodec.Encode(parts)
	require.NoError(t, err)
	kv, err := js.KeyValue(ctx, psBucket)
	require.NoError(t, err)
	_, err = kv.Put(ctx, psKey, data)
	require.NoError(t, err)
}

func writePartitionsAction(t *testing.T, plan provision.PlanResult) *provision.WritePartitionsResource {
	t.Helper()
	require.Len(t, plan.Actions, 1)
	require.Equal(t, provision.ActionWritePartitions, plan.Actions[0].Kind)
	require.Equal(t, psBucket, plan.Actions[0].Name)
	res, ok := plan.Actions[0].Resource.(*provision.WritePartitionsResource)
	require.True(t, ok, "resource must be *WritePartitionsResource")

	return res
}

func TestPlanPartitions_FirstPublish_AllAdded(t *testing.T) {
	js := newJS(t)
	createKV(t, js, jetstream.KeyValueConfig{Bucket: psBucket})

	cfg := partitionsConfig(
		types.Partition{Keys: []string{"a"}, Weight: 1},
		types.Partition{Keys: []string{"b"}},
	)
	plan, err := provision.PlanPartitions(t.Context(), js, cfg)
	require.NoError(t, err)

	res := writePartitionsAction(t, plan)
	require.Len(t, res.Added, 2)
	require.Empty(t, res.Removed)
	require.Empty(t, res.Changed)
	require.Empty(t, res.Before)
	require.Len(t, res.After, 2)
	require.Equal(t, psBucket, res.Bucket)
	require.Equal(t, psKey, res.Key)

	require.Len(t, plan.Drift, 1)
	require.Equal(t, provision.SeverityDriftMutable, plan.Drift[0].Severity)
	require.Equal(t, provision.KindPartitionRecords, plan.Drift[0].Kind)
}

func TestPlanPartitions_NoDiff_Informational(t *testing.T) {
	js := newJS(t)
	createKV(t, js, jetstream.KeyValueConfig{Bucket: psBucket})

	parts := []types.Partition{{Keys: []string{"a"}, Weight: 1}, {Keys: []string{"b"}}}
	writePartitionKey(t, js, parts)

	plan, err := provision.PlanPartitions(t.Context(), js, partitionsConfig(parts...))
	require.NoError(t, err)

	require.Empty(t, plan.Actions)
	require.Len(t, plan.Drift, 1)
	require.Equal(t, provision.SeverityInformational, plan.Drift[0].Severity)
	require.Equal(t, provision.KindPartitionRecords, plan.Drift[0].Kind)
}

func TestPlanPartitions_Mixed(t *testing.T) {
	js := newJS(t)
	createKV(t, js, jetstream.KeyValueConfig{Bucket: psBucket})

	writePartitionKey(t, js, []types.Partition{
		{Keys: []string{"keep"}, Weight: 1},
		{Keys: []string{"drop"}, Weight: 1},
		{Keys: []string{"reweight"}, Weight: 2},
	})

	cfg := partitionsConfig(
		types.Partition{Keys: []string{"keep"}, Weight: 1},
		types.Partition{Keys: []string{"reweight"}, Weight: 8},
		types.Partition{Keys: []string{"new"}, Weight: 1},
	)
	plan, err := provision.PlanPartitions(t.Context(), js, cfg)
	require.NoError(t, err)

	res := writePartitionsAction(t, plan)
	require.Len(t, res.Added, 1)
	require.Equal(t, []string{"new"}, res.Added[0].Keys)
	require.Len(t, res.Removed, 1)
	require.Equal(t, []string{"drop"}, res.Removed[0].Keys)
	require.Len(t, res.Changed, 1)
	require.Equal(t, []string{"reweight"}, res.Changed[0].Keys)
	require.Equal(t, int64(2), res.Changed[0].OldWeight)
	require.Equal(t, int64(8), res.Changed[0].NewWeight)
	require.Len(t, res.Before, 3)
	require.Len(t, res.After, 3)
}

// TestPlanPartitions_DeletedKey and _PurgedKey verify the W2 contract: a
// deleted or purged key is indistinguishable from never-written and is treated
// as an empty live table (config is the source of truth).
func TestPlanPartitions_DeletedKey(t *testing.T) {
	js := newJS(t)
	createKV(t, js, jetstream.KeyValueConfig{Bucket: psBucket})
	writePartitionKey(t, js, []types.Partition{{Keys: []string{"old"}}})

	kv, err := js.KeyValue(t.Context(), psBucket)
	require.NoError(t, err)
	require.NoError(t, kv.Delete(t.Context(), psKey))

	cfg := partitionsConfig(types.Partition{Keys: []string{"a"}}, types.Partition{Keys: []string{"b"}})
	plan, err := provision.PlanPartitions(t.Context(), js, cfg)
	require.NoError(t, err)

	res := writePartitionsAction(t, plan)
	require.Len(t, res.Added, 2)
	require.Empty(t, res.Removed)
	require.Empty(t, res.Before)
}

func TestPlanPartitions_PurgedKey(t *testing.T) {
	js := newJS(t)
	createKV(t, js, jetstream.KeyValueConfig{Bucket: psBucket})
	writePartitionKey(t, js, []types.Partition{{Keys: []string{"old"}}})

	kv, err := js.KeyValue(t.Context(), psBucket)
	require.NoError(t, err)
	require.NoError(t, kv.Purge(t.Context(), psKey))

	plan, err := provision.PlanPartitions(t.Context(), js, partitionsConfig(types.Partition{Keys: []string{"a"}}))
	require.NoError(t, err)

	res := writePartitionsAction(t, plan)
	require.Len(t, res.Added, 1)
	require.Empty(t, res.Before)
}

func TestPlanPartitions_BucketMissing(t *testing.T) {
	js := newJS(t) // no bucket created

	_, err := provision.PlanPartitions(t.Context(), js, partitionsConfig(types.Partition{Keys: []string{"a"}}))
	require.Error(t, err)
	require.ErrorIs(t, err, provision.ErrPartitionBucketMissing)
	require.ErrorIs(t, err, provision.ErrLiveValidation)
}

func TestPlanPartitions_CancelledContext(t *testing.T) {
	js := newJS(t)
	createKV(t, js, jetstream.KeyValueConfig{Bucket: psBucket})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := provision.PlanPartitions(ctx, js, partitionsConfig(types.Partition{Keys: []string{"a"}}))
	require.ErrorIs(t, err, context.Canceled)
}

// TestPlanPartitions_BadConfigBeatsCancelledContext verifies static validation
// runs before the cancellation check: a malformed config under a cancelled
// context still returns ErrInvalidConfig (exit 3), not context.Canceled.
func TestPlanPartitions_BadConfigBeatsCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := provision.PlanPartitions(ctx, nil, provision.Config{APIVersion: "parti.io/v9"})
	require.ErrorIs(t, err, provision.ErrInvalidConfig)
	require.NotErrorIs(t, err, context.Canceled)
}

// TestPlanPartitions_RejectsBadConfig proves the inherited static-validation
// boundary runs before any NATS I/O: every case returns ErrInvalidConfig with
// a nil JetStream handle, so reaching NATS would panic.
func TestPlanPartitions_RejectsBadConfig(t *testing.T) {
	t.Parallel()

	good := types.Partition{Keys: []string{"a"}}
	cases := []struct {
		name string
		cfg  provision.Config
	}{
		{
			name: "bad apiVersion",
			cfg: provision.Config{
				APIVersion:      "parti.io/v9",
				PartitionSource: &provision.PartitionSourceConfig{Bucket: "b", Key: "k", Partitions: []types.Partition{good}},
			},
		},
		{
			name: "empty bucket",
			cfg: provision.Config{
				APIVersion:      provision.APIVersionV1,
				PartitionSource: &provision.PartitionSourceConfig{Bucket: "", Key: "k", Partitions: []types.Partition{good}},
			},
		},
		{
			name: "empty key",
			cfg: provision.Config{
				APIVersion:      provision.APIVersionV1,
				PartitionSource: &provision.PartitionSourceConfig{Bucket: "b", Key: "", Partitions: []types.Partition{good}},
			},
		},
		{
			name: "nil partitionSource",
			cfg:  provision.Config{APIVersion: provision.APIVersionV1},
		},
		{
			name: "no partitions declared",
			cfg: provision.Config{
				APIVersion:      provision.APIVersionV1,
				PartitionSource: &provision.PartitionSourceConfig{Bucket: "b", Key: "k"},
			},
		},
		{
			name: "invalid record",
			cfg: provision.Config{
				APIVersion: provision.APIVersionV1,
				PartitionSource: &provision.PartitionSourceConfig{
					Bucket: "b", Key: "k",
					Partitions: []types.Partition{{Keys: []string{"a.b"}}},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := provision.PlanPartitions(t.Context(), nil, tc.cfg)
			require.Error(t, err)
			require.ErrorIs(t, err, provision.ErrInvalidConfig)
		})
	}
}
