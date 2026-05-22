package provision

import (
	"context"
	"errors"
	"testing"

	"github.com/arloliu/parti/v2/internal/partcodec"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// fakePartitionKV is a deterministic partitionKV for ApplyPartitions step
// tests — no NATS server, no timing.
type fakePartitionKV struct {
	entry     partitionEntry
	getErr    error
	createErr error
	updateErr error

	created   [][]byte
	updated   [][]byte
	updateRev []uint64
}

func (f *fakePartitionKV) Get(_ context.Context) (partitionEntry, error) {
	return f.entry, f.getErr
}

func (f *fakePartitionKV) Create(_ context.Context, data []byte) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, data)

	return nil
}

func (f *fakePartitionKV) Update(_ context.Context, data []byte, revision uint64) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updated = append(f.updated, data)
	f.updateRev = append(f.updateRev, revision)

	return nil
}

func writeRes(before, after []types.Partition) *WritePartitionsResource {
	return &WritePartitionsResource{
		Bucket: "parti-partitions",
		Key:    "partitions/v1",
		Before: before,
		After:  after,
	}
}

func decoded(t *testing.T, data []byte) []types.Partition {
	t.Helper()
	out, err := partcodec.Decode(data)
	require.NoError(t, err)

	return out
}

// --- applyWritePartitionsAction: steps 4-9 -----------------------------------

func TestApplyWritePartitions_CleanCreate(t *testing.T) {
	t.Parallel()

	after := []types.Partition{part(1, "a"), part(2, "b")}
	fake := &fakePartitionKV{} // key absent: zero-value entry, Found=false
	res := writeRes(nil, after)

	report, err := applyWritePartitionsAction(t.Context(), fake, res, after, false)
	require.NoError(t, err)
	require.Len(t, report.Executed, 1)
	require.Equal(t, ActionWritePartitions, report.Executed[0].Kind)
	require.False(t, report.Executed[0].Raced)
	require.Empty(t, report.Errors)

	require.Len(t, fake.created, 1)
	require.Empty(t, fake.updated)
	require.ElementsMatch(t, after, decoded(t, fake.created[0]))
}

func TestApplyWritePartitions_CleanCASUpdate(t *testing.T) {
	t.Parallel()

	before := []types.Partition{part(1, "a")}
	after := []types.Partition{part(1, "a"), part(1, "b")}
	fake := &fakePartitionKV{entry: partitionEntry{Parts: before, Revision: 7, Found: true}}
	res := writeRes(before, after)

	report, err := applyWritePartitionsAction(t.Context(), fake, res, after, false)
	require.NoError(t, err)
	require.Len(t, report.Executed, 1)
	require.False(t, report.Executed[0].Raced)

	require.Empty(t, fake.created)
	require.Len(t, fake.updated, 1)
	require.Equal(t, []uint64{7}, fake.updateRev)
	require.ElementsMatch(t, after, decoded(t, fake.updated[0]))
}

func TestApplyWritePartitions_NoOp_NotRaced(t *testing.T) {
	t.Parallel()

	set := []types.Partition{part(1, "a"), part(1, "b")}
	fake := &fakePartitionKV{entry: partitionEntry{Parts: set, Revision: 5, Found: true}}
	res := writeRes(set, set)

	report, err := applyWritePartitionsAction(t.Context(), fake, res, set, false)
	require.NoError(t, err)
	require.Len(t, report.Executed, 1)
	require.False(t, report.Executed[0].Raced)
	require.Empty(t, fake.created)
	require.Empty(t, fake.updated)
}

func TestApplyWritePartitions_NoOp_Raced(t *testing.T) {
	t.Parallel()

	before := []types.Partition{part(1, "a")}
	after := []types.Partition{part(1, "a"), part(1, "b")}
	// Live already converged to after — a concurrent writer got there first.
	fake := &fakePartitionKV{entry: partitionEntry{Parts: after, Revision: 9, Found: true}}
	res := writeRes(before, after)

	report, err := applyWritePartitionsAction(t.Context(), fake, res, after, false)
	require.NoError(t, err)
	require.Len(t, report.Executed, 1)
	require.True(t, report.Executed[0].Raced, "live != Before but == target is a raced no-op")
	require.Empty(t, fake.updated)
}

func TestApplyWritePartitions_StaleBefore_GenuineRace(t *testing.T) {
	t.Parallel()

	before := []types.Partition{part(1, "a")}
	after := []types.Partition{part(1, "a"), part(1, "b")}
	// Live is a third state — neither Before nor target.
	fake := &fakePartitionKV{entry: partitionEntry{Parts: []types.Partition{part(1, "x")}, Revision: 4, Found: true}}
	res := writeRes(before, after)

	report, err := applyWritePartitionsAction(t.Context(), fake, res, after, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "changed since plan")
	require.NotErrorIs(t, err, ErrInvalidConfig)
	require.NotErrorIs(t, err, context.Canceled)

	require.Len(t, report.Errors, 1)
	require.Equal(t, ActionWritePartitions, report.Errors[0].Kind)
	require.False(t, report.Aborted)
	require.Empty(t, report.Skipped)
	require.Empty(t, report.Executed)
	require.Empty(t, fake.updated)
}

func TestApplyWritePartitions_RemovalGate_RefusedWithoutPrune(t *testing.T) {
	t.Parallel()

	current := []types.Partition{part(1, "a"), part(1, "b")}
	after := []types.Partition{part(1, "a")}
	fake := &fakePartitionKV{entry: partitionEntry{Parts: current, Revision: 3, Found: true}}
	res := writeRes(current, after)

	report, err := applyWritePartitionsAction(t.Context(), fake, res, after, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--prune")
	require.Len(t, report.Errors, 1)
	require.Empty(t, fake.updated, "no write when the removal gate refuses")
}

func TestApplyWritePartitions_RemovalGate_WrittenWithPrune(t *testing.T) {
	t.Parallel()

	current := []types.Partition{part(1, "a"), part(1, "b")}
	after := []types.Partition{part(1, "a")}
	fake := &fakePartitionKV{entry: partitionEntry{Parts: current, Revision: 3, Found: true}}
	res := writeRes(current, after)

	report, err := applyWritePartitionsAction(t.Context(), fake, res, after, true)
	require.NoError(t, err)
	require.Len(t, report.Executed, 1)
	require.Len(t, fake.updated, 1)
	require.ElementsMatch(t, after, decoded(t, fake.updated[0]))
}

// TestApplyWritePartitions_RemovalGate_RacedConvergedBypassesGate is the key
// ordering test: a plan that removes records, applied without --prune, against
// a table a concurrent writer already converged to target, succeeds as a
// raced no-op — the removal gate (which runs after the no-op check) is never
// reached because no removal write happens.
func TestApplyWritePartitions_RemovalGate_RacedConvergedBypassesGate(t *testing.T) {
	t.Parallel()

	before := []types.Partition{part(1, "a"), part(1, "b")} // plan saw a+b
	after := []types.Partition{part(1, "a")}                // plan removes b
	// Live already converged to after.
	fake := &fakePartitionKV{entry: partitionEntry{Parts: after, Revision: 6, Found: true}}
	res := writeRes(before, after)

	report, err := applyWritePartitionsAction(t.Context(), fake, res, after, false)
	require.NoError(t, err, "raced-converged removal plan must not hit the prune gate")
	require.Len(t, report.Executed, 1)
	require.True(t, report.Executed[0].Raced)
	require.Empty(t, report.Errors)
	require.Empty(t, fake.updated)
}

func TestApplyWritePartitions_CASConflict_OnUpdate(t *testing.T) {
	t.Parallel()

	before := []types.Partition{part(1, "a")}
	after := []types.Partition{part(1, "a"), part(1, "b")}
	fake := &fakePartitionKV{
		entry:     partitionEntry{Parts: before, Revision: 7, Found: true},
		updateErr: jetstream.ErrKeyExists,
	}
	res := writeRes(before, after)

	report, err := applyWritePartitionsAction(t.Context(), fake, res, after, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "concurrently")
	require.NotErrorIs(t, err, context.Canceled)
	require.Len(t, report.Errors, 1)
	require.False(t, report.Aborted)
}

func TestApplyWritePartitions_CASConflict_OnCreate(t *testing.T) {
	t.Parallel()

	after := []types.Partition{part(1, "a")}
	fake := &fakePartitionKV{createErr: jetstream.ErrKeyExists} // key absent at re-read
	res := writeRes(nil, after)

	report, err := applyWritePartitionsAction(t.Context(), fake, res, after, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "concurrently")
	require.Len(t, report.Errors, 1)
}

func TestApplyWritePartitions_ReadError_NonCancellation(t *testing.T) {
	t.Parallel()

	fake := &fakePartitionKV{getErr: errors.New("corrupt KV data")}
	res := writeRes(nil, []types.Partition{part(1, "a")})

	report, err := applyWritePartitionsAction(t.Context(), fake, res, res.After, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "re-read")
	require.Len(t, report.Errors, 1)
	require.False(t, report.Aborted)
}

func TestApplyWritePartitions_CancelledAtReRead(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fake := &fakePartitionKV{getErr: context.Canceled}
	res := writeRes(nil, []types.Partition{part(1, "a")})

	report, err := applyWritePartitionsAction(ctx, fake, res, res.After, false)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, report.Aborted)
	require.Empty(t, report.Errors)
	require.Len(t, report.Skipped, 1)
	require.Equal(t, SkipReasonContextCancelled, report.Skipped[0].Reason)
	require.Empty(t, report.Executed)
}

func TestApplyWritePartitions_CancelledAtWrite(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	before := []types.Partition{part(1, "a")}
	after := []types.Partition{part(1, "a"), part(1, "b")}
	fake := &fakePartitionKV{
		entry:     partitionEntry{Parts: before, Revision: 2, Found: true},
		updateErr: context.Canceled,
	}
	res := writeRes(before, after)

	report, err := applyWritePartitionsAction(ctx, fake, res, after, false)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, report.Aborted)
	require.Empty(t, report.Errors)
	require.Len(t, report.Skipped, 1)
}

// --- extractWritePartitionsAction --------------------------------------------

func TestExtractWritePartitionsAction_None(t *testing.T) {
	t.Parallel()

	res, err := extractWritePartitionsAction(PlanResult{Actions: []PlannedAction{
		{Kind: ActionCreateKV, Name: "x"},
	}})
	require.NoError(t, err)
	require.Nil(t, res)
}

func TestExtractWritePartitionsAction_One(t *testing.T) {
	t.Parallel()

	want := writeRes(nil, []types.Partition{part(1, "a")})
	res, err := extractWritePartitionsAction(PlanResult{Actions: []PlannedAction{
		{Kind: ActionWritePartitions, Name: "parti-partitions", Resource: want},
	}})
	require.NoError(t, err)
	require.Same(t, want, res)
}

func TestExtractWritePartitionsAction_Multiple(t *testing.T) {
	t.Parallel()

	res := writeRes(nil, []types.Partition{part(1, "a")})
	_, err := extractWritePartitionsAction(PlanResult{Actions: []PlannedAction{
		{Kind: ActionWritePartitions, Name: "b1", Resource: res},
		{Kind: ActionWritePartitions, Name: "b2", Resource: res},
	}})
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestExtractWritePartitionsAction_WrongResourceType(t *testing.T) {
	t.Parallel()

	_, err := extractWritePartitionsAction(PlanResult{Actions: []PlannedAction{
		{Kind: ActionWritePartitions, Name: "b", Resource: "not a resource"},
	}})
	require.ErrorIs(t, err, ErrInvalidConfig)
}

// TestExtractWritePartitionsAction_TypedNilResource guards against a typed-nil
// *WritePartitionsResource passing the type assertion and collapsing into a
// silent no-op.
func TestExtractWritePartitionsAction_TypedNilResource(t *testing.T) {
	t.Parallel()

	_, err := extractWritePartitionsAction(PlanResult{Actions: []PlannedAction{
		{Kind: ActionWritePartitions, Name: "b", Resource: (*WritePartitionsResource)(nil)},
	}})
	require.ErrorIs(t, err, ErrInvalidConfig)
}

func TestApplyPartitions_TypedNilResource_Rejected(t *testing.T) {
	t.Parallel()

	plan := PlanResult{Actions: []PlannedAction{
		{Kind: ActionWritePartitions, Name: "b", Resource: (*WritePartitionsResource)(nil)},
	}}
	_, err := ApplyPartitions(t.Context(), nil, plan, false)
	require.ErrorIs(t, err, ErrInvalidConfig)
}

// --- ApplyPartitions: pre-I/O paths (nil JetStream proves no NATS is touched) -

func TestApplyPartitions_NoAction_EnvelopeReport(t *testing.T) {
	t.Parallel()

	report, err := ApplyPartitions(t.Context(), nil, PlanResult{Actions: []PlannedAction{}}, false)
	require.NoError(t, err)
	require.Equal(t, APIVersionProvisionV1, report.APIVersion)
	require.Equal(t, KindReport, report.Kind)
	require.NotNil(t, report.Executed)
	require.NotNil(t, report.Skipped)
	require.NotNil(t, report.Errors)
	require.Empty(t, report.Executed)
}

func TestApplyPartitions_BoundaryValidation_RejectsForgedAfter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		after []types.Partition
	}{
		{"duplicate canonical id", []types.Partition{part(1, "a"), part(2, "a")}},
		{"empty key record", []types.Partition{{Keys: []string{""}}}},
		{"empty after set", []types.Partition{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := PlanResult{Actions: []PlannedAction{{
				Kind:     ActionWritePartitions,
				Name:     "parti-partitions",
				Resource: writeRes(nil, tc.after),
			}}}
			// nil JetStream: a reached NATS call would panic, proving the
			// boundary validation runs before any I/O.
			_, err := ApplyPartitions(t.Context(), nil, plan, false)
			require.ErrorIs(t, err, ErrInvalidConfig)
		})
	}
}

func TestApplyPartitions_MultipleActions_Rejected(t *testing.T) {
	t.Parallel()

	res := writeRes(nil, []types.Partition{part(1, "a")})
	plan := PlanResult{Actions: []PlannedAction{
		{Kind: ActionWritePartitions, Name: "b1", Resource: res},
		{Kind: ActionWritePartitions, Name: "b2", Resource: res},
	}}
	_, err := ApplyPartitions(t.Context(), nil, plan, false)
	require.ErrorIs(t, err, ErrInvalidConfig)
}
