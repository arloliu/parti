package provision_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/partcodec"
	"github.com/arloliu/parti/v2/provision"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// readPartitionTable reads and decodes the live partition-source key.
func readPartitionTable(t *testing.T, js jetstream.JetStream) []types.Partition {
	t.Helper()
	kv, err := js.KeyValue(t.Context(), psBucket)
	require.NoError(t, err)
	entry, err := kv.Get(t.Context(), psKey)
	require.NoError(t, err)
	out, err := partcodec.Decode(entry.Value())
	require.NoError(t, err)

	return out
}

func TestApplyPartitions_EndToEnd_FirstPublish(t *testing.T) {
	js := newJS(t)
	createKV(t, js, jetstream.KeyValueConfig{Bucket: psBucket})

	cfg := partitionsConfig(
		types.Partition{Keys: []string{"a"}, Weight: 1},
		types.Partition{Keys: []string{"b"}, Weight: 2},
	)
	plan, err := provision.PlanPartitions(t.Context(), js, cfg)
	require.NoError(t, err)

	report, err := provision.ApplyPartitions(t.Context(), js, plan, false)
	require.NoError(t, err)
	require.Len(t, report.Executed, 1)
	require.False(t, report.Executed[0].Raced)
	require.Empty(t, report.Errors)

	require.ElementsMatch(t, cfg.PartitionSource.Partitions, readPartitionTable(t, js))
}

func TestApplyPartitions_EndToEnd_UpdateAddAndReweight(t *testing.T) {
	js := newJS(t)
	createKV(t, js, jetstream.KeyValueConfig{Bucket: psBucket})
	writePartitionKey(t, js, []types.Partition{
		{Keys: []string{"a"}, Weight: 1},
		{Keys: []string{"b"}, Weight: 1},
	})

	cfg := partitionsConfig(
		types.Partition{Keys: []string{"a"}, Weight: 1},
		types.Partition{Keys: []string{"b"}, Weight: 5}, // reweighted
		types.Partition{Keys: []string{"c"}, Weight: 1}, // added
	)
	plan, err := provision.PlanPartitions(t.Context(), js, cfg)
	require.NoError(t, err)

	report, err := provision.ApplyPartitions(t.Context(), js, plan, false)
	require.NoError(t, err)
	require.Len(t, report.Executed, 1)

	require.ElementsMatch(t, cfg.PartitionSource.Partitions, readPartitionTable(t, js))
}

// TestApplyPartitions_EndToEnd_RemovalNeedsPrune verifies the removal gate
// end-to-end: the same plan is refused without --prune (no write) and applied
// with --prune.
func TestApplyPartitions_EndToEnd_RemovalNeedsPrune(t *testing.T) {
	js := newJS(t)
	createKV(t, js, jetstream.KeyValueConfig{Bucket: psBucket})
	initial := []types.Partition{
		{Keys: []string{"a"}},
		{Keys: []string{"b"}},
		{Keys: []string{"c"}},
	}
	writePartitionKey(t, js, initial)

	cfg := partitionsConfig(types.Partition{Keys: []string{"a"}})
	plan, err := provision.PlanPartitions(t.Context(), js, cfg)
	require.NoError(t, err)

	// Without --prune: refused, no write.
	report, err := provision.ApplyPartitions(t.Context(), js, plan, false)
	require.Error(t, err)
	require.Len(t, report.Errors, 1)
	require.ElementsMatch(t, initial, readPartitionTable(t, js), "refused apply must not write")

	// With --prune: applied.
	report, err = provision.ApplyPartitions(t.Context(), js, plan, true)
	require.NoError(t, err)
	require.Len(t, report.Executed, 1)
	require.ElementsMatch(t, cfg.PartitionSource.Partitions, readPartitionTable(t, js))
}

// TestApplyPartitions_EndToEnd_Idempotent verifies a second plan/apply cycle
// after convergence emits no action.
func TestApplyPartitions_EndToEnd_Idempotent(t *testing.T) {
	js := newJS(t)
	createKV(t, js, jetstream.KeyValueConfig{Bucket: psBucket})

	cfg := partitionsConfig(
		types.Partition{Keys: []string{"a"}},
		types.Partition{Keys: []string{"b"}},
	)
	plan, err := provision.PlanPartitions(t.Context(), js, cfg)
	require.NoError(t, err)
	_, err = provision.ApplyPartitions(t.Context(), js, plan, false)
	require.NoError(t, err)

	// Re-plan against the converged table: no diff, no action.
	plan2, err := provision.PlanPartitions(t.Context(), js, cfg)
	require.NoError(t, err)
	require.Empty(t, plan2.Actions)

	report, err := provision.ApplyPartitions(t.Context(), js, plan2, false)
	require.NoError(t, err)
	require.Empty(t, report.Executed)
	require.Equal(t, provision.APIVersionProvisionV1, report.APIVersion)
}

func TestApplyPartitions_EndToEnd_BucketMissing(t *testing.T) {
	js := newJS(t) // no bucket created

	// Hand-built plan: PlanPartitions cannot run without the bucket.
	plan := provision.PlanResult{
		APIVersion: provision.APIVersionProvisionV1,
		Kind:       "Plan",
		Actions: []provision.PlannedAction{{
			Kind: provision.ActionWritePartitions,
			Name: psBucket,
			Resource: &provision.WritePartitionsResource{
				Bucket: psBucket,
				Key:    psKey,
				After:  []types.Partition{{Keys: []string{"a"}}},
			},
		}},
	}
	_, err := provision.ApplyPartitions(t.Context(), js, plan, false)
	require.ErrorIs(t, err, provision.ErrPartitionBucketMissing)
	require.ErrorIs(t, err, provision.ErrLiveValidation)
}

// TestApplyPartitions_EndToEnd_CancelledContext verifies the cancellation
// return shape end-to-end: a cancelled context yields an aborted Report with
// the action skipped and ctx.Err(), regardless of which I/O step catches it.
func TestApplyPartitions_EndToEnd_CancelledContext(t *testing.T) {
	js := newJS(t)
	createKV(t, js, jetstream.KeyValueConfig{Bucket: psBucket})

	cfg := partitionsConfig(types.Partition{Keys: []string{"a"}})
	plan, err := provision.PlanPartitions(t.Context(), js, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report, err := provision.ApplyPartitions(ctx, js, plan, false)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, report.Aborted)
	require.Empty(t, report.Errors)
	require.Len(t, report.Skipped, 1)
	require.Equal(t, provision.SkipReasonContextCancelled, report.Skipped[0].Reason)
	require.Empty(t, report.Executed)
}

// TestApplyPartitions_EndToEnd_SuccessIsNotDurableConvergence encodes the
// honest-scope contract: a successful apply means the CAS landed, not that the
// table stays converged against a later writer.
func TestApplyPartitions_EndToEnd_SuccessIsNotDurableConvergence(t *testing.T) {
	js := newJS(t)
	createKV(t, js, jetstream.KeyValueConfig{Bucket: psBucket})

	cfg := partitionsConfig(types.Partition{Keys: []string{"a"}})
	plan, err := provision.PlanPartitions(t.Context(), js, cfg)
	require.NoError(t, err)

	report, err := provision.ApplyPartitions(t.Context(), js, plan, false)
	require.NoError(t, err)
	require.Len(t, report.Executed, 1, "apply reports success: its CAS landed")

	// A later writer overwrites the table; the earlier apply still succeeded.
	writePartitionKey(t, js, []types.Partition{{Keys: []string{"z"}}})
	require.ElementsMatch(t, []types.Partition{{Keys: []string{"z"}}}, readPartitionTable(t, js))
}

// TestApplyPartitions_EndToEnd_RuntimeReadsBack is the v3 capstone: a table
// published through ApplyPartitions is consumed unchanged by the runtime
// partition source (source.NatsKV) — the operator-write to runtime-read loop.
func TestApplyPartitions_EndToEnd_RuntimeReadsBack(t *testing.T) {
	js := newJS(t)
	createKV(t, js, jetstream.KeyValueConfig{Bucket: psBucket})

	cfg := partitionsConfig(
		types.Partition{Keys: []string{"orders", "0"}, Weight: 100},
		types.Partition{Keys: []string{"orders", "1"}, Weight: 100},
		types.Partition{Keys: []string{"audit"}},
	)
	plan, err := provision.PlanPartitions(t.Context(), js, cfg)
	require.NoError(t, err)
	_, err = provision.ApplyPartitions(t.Context(), js, plan, false)
	require.NoError(t, err)

	// The runtime partition source reads the same key.
	kv, err := js.KeyValue(t.Context(), psBucket)
	require.NoError(t, err)
	src := source.NewNatsKV(kv, psKey, nil, source.WithReconcileInterval(0))
	require.NoError(t, src.Start(t.Context()))
	defer func() { _ = src.Stop(context.Background()) }()

	require.Eventually(t, func() bool {
		got, listErr := src.List(t.Context())
		return listErr == nil && len(got) == len(cfg.PartitionSource.Partitions)
	}, 2*time.Second, 10*time.Millisecond)

	got, err := src.List(t.Context())
	require.NoError(t, err)
	require.ElementsMatch(t, cfg.PartitionSource.Partitions, got)
}
