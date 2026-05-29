package provision

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/dynamicbuild"
	"github.com/arloliu/parti/v2/types"
)

// TestPlanDynamicConsumers_HappyPath asserts the v1 in-scope equality subset
// (Name, Durable, FilterSubject, AckPolicy, DeliverPolicy) for a small
// partition set.
func TestPlanDynamicConsumers_HappyPath(t *testing.T) {
	partitions := []types.Partition{
		{Keys: []string{"a", "1"}},
		{Keys: []string{"b", "2"}},
	}

	planned, err := PlanDynamicConsumers("orders", "ord-worker", "orders.{{.PartitionID}}.>", partitions)
	require.NoError(t, err)
	require.Len(t, planned, 2)

	// Subjects sorted lex.
	require.Equal(t, "orders.a.1.>", planned[0].Subject)
	require.Equal(t, "orders.b.2.>", planned[1].Subject)

	for _, pc := range planned {
		require.Equal(t, "orders", pc.StreamName)
		require.NotEmpty(t, pc.Durable)
		require.Equal(t, pc.Durable, pc.Config.Name)
		require.Equal(t, pc.Durable, pc.Config.Durable)
		require.Equal(t, pc.Subject, pc.Config.FilterSubject)
		require.Equal(t, jetstream.AckExplicitPolicy, pc.Config.AckPolicy)
		require.Equal(t, jetstream.DeliverAllPolicy, pc.Config.DeliverPolicy)
		require.True(t, strings.HasPrefix(pc.Durable, "ord-worker_"))
	}
}

// TestPlanDynamicConsumers_RuntimeRoundtripEquivalence is the load-bearing
// v1 byte-equivalence test. It constructs a runtime
// durable.WorkerConsumerConfig and calls SetDefaults() — exactly as
// consumer.Dynamic does via its Validate path — then re-derives the
// expected per-subject ConsumerConfig via dynamicbuild and asserts every
// field in the §2 in-scope equality subset matches PlanDynamicConsumers's
// output.
func TestPlanDynamicConsumers_RuntimeRoundtripEquivalence(t *testing.T) {
	const (
		streamName      = "orders"
		consumerPrefix  = "ord-worker"
		subjectTemplate = "orders.{{.PartitionID}}.>"
	)
	partitions := []types.Partition{
		{Keys: []string{"a", "1"}},
		{Keys: []string{"b", "2"}},
		{Keys: []string{"c", "3"}},
	}

	runtimeCfg := durable.WorkerConsumerConfig{
		StreamName:      streamName,
		ConsumerPrefix:  consumerPrefix,
		SubjectTemplate: subjectTemplate,
	}
	require.NoError(t, runtimeCfg.SetDefaults(),
		"the equivalence assertion is only meaningful after SetDefaults runs")

	// Pin: dynamicbuild.DefaultDynamicDefaults (what provision precreates
	// from) must match the runtime's post-SetDefaults config on the
	// NATS-immutable fields. If these drift, a provision-precreated consumer
	// can no longer be adopted by the runtime — CreateOrUpdateConsumer rejects
	// an immutable-field change.
	rd := dynamicbuild.DefaultDynamicDefaults()
	require.Equal(t, runtimeCfg.AckPolicy, rd.AckPolicy,
		"AckPolicy is NATS-immutable; provision defaults must match the runtime")
	require.Equal(t, runtimeCfg.MaxWaiting, rd.MaxWaiting,
		"MaxWaiting is NATS-immutable; provision defaults must match the runtime")
	require.Equal(t, runtimeCfg.ConsumerMemoryStorage, rd.ConsumerMemoryStorage,
		"MemoryStorage is NATS-immutable; provision defaults must match the runtime")

	pre, suf, _ := dynamicbuild.ParseSubjectTemplateParts(subjectTemplate)

	planned, err := PlanDynamicConsumers(streamName, consumerPrefix, subjectTemplate, partitions)
	require.NoError(t, err)
	require.Len(t, planned, len(partitions))

	for _, pc := range planned {
		expectedDurable := dynamicbuild.PerSubjectDurableName(consumerPrefix, pc.Subject, pre, suf)
		expectedCfg := dynamicbuild.ConsumerConfig(
			expectedDurable,
			pc.Subject,
			dynamicbuild.Defaults{
				AckPolicy:             runtimeCfg.AckPolicy,
				AckWait:               runtimeCfg.AckWait,
				MaxDeliver:            runtimeCfg.MaxDeliver,
				InactiveThreshold:     runtimeCfg.InactiveThreshold,
				MaxWaiting:            runtimeCfg.MaxWaiting,
				MaxAckPending:         runtimeCfg.MaxAckPending,
				ConsumerMemoryStorage: runtimeCfg.ConsumerMemoryStorage,
				ConsumerReplicas:      runtimeCfg.ConsumerReplicas,
			},
		)

		// Identity / immutable subset — these MUST match between runtime and
		// provision (NATS rejects an update that changes any of them, so a
		// precreated consumer must carry the runtime's values).
		require.Equal(t, expectedCfg.Name, pc.Config.Name)
		require.Equal(t, expectedCfg.Durable, pc.Config.Durable)
		require.Equal(t, expectedCfg.FilterSubject, pc.Config.FilterSubject)
		require.Equal(t, expectedCfg.AckPolicy, pc.Config.AckPolicy)
		require.Equal(t, expectedCfg.DeliverPolicy, pc.Config.DeliverPolicy)
		require.Equal(t, expectedCfg.MaxWaiting, pc.Config.MaxWaiting)
		require.Equal(t, expectedCfg.MemoryStorage, pc.Config.MemoryStorage)

		// The mutable runtime-owned tunables (AckWait, MaxDeliver,
		// InactiveThreshold, MaxAckPending, Replicas) are not asserted: the
		// runtime overwrites them freely via CreateOrUpdateConsumer, so they
		// need not match at precreation time.
	}
}

// TestPlanDynamicConsumers_RoundtripEquivalence_FailsOnIotaDrift documents
// the protection against future iota reordering in nats.go. If
// AckExplicitPolicy or DeliverAllPolicy stops being the zero value,
// PlanDynamicConsumers's explicit assignment keeps the output stable.
func TestPlanDynamicConsumers_HardCodesSemanticConstants(t *testing.T) {
	planned, err := PlanDynamicConsumers("s", "p", "x.{{.PartitionID}}", []types.Partition{{Keys: []string{"k"}}})
	require.NoError(t, err)
	require.Len(t, planned, 1)
	require.Equal(t, jetstream.AckExplicitPolicy, planned[0].Config.AckPolicy)
	require.Equal(t, jetstream.DeliverAllPolicy, planned[0].Config.DeliverPolicy)
}

// TestPlanDynamicConsumers_DeterministicOrderAcrossShuffles asserts that
// PlanDynamicConsumers produces identical output for any permutation of the
// input partition slice.
func TestPlanDynamicConsumers_DeterministicOrderAcrossShuffles(t *testing.T) {
	base := make([]types.Partition, 16)
	for i := range base {
		base[i] = types.Partition{Keys: []string{"part", string(rune('a' + i))}}
	}

	canonical, err := PlanDynamicConsumers("s", "p", "x.{{.PartitionID}}", base)
	require.NoError(t, err)

	r := rand.New(rand.NewSource(42)) //nolint:gosec // deterministic shuffle for test
	for trial := range 10 {
		shuffled := make([]types.Partition, len(base))
		copy(shuffled, base)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		got, err := PlanDynamicConsumers("s", "p", "x.{{.PartitionID}}", shuffled)
		require.NoError(t, err)
		require.Equal(t, canonical, got, "trial %d: output not deterministic", trial)
	}
}

// TestPlanDynamicConsumers_DeduplicatesEqualSubjects asserts that two
// partitions whose Keys resolve to the same subject collapse to one
// PlannedConsumer entry (matches runtime dedup behavior).
func TestPlanDynamicConsumers_DeduplicatesEqualSubjects(t *testing.T) {
	partitions := []types.Partition{
		{Keys: []string{"a", "1"}},
		{Keys: []string{"a", "1"}}, // dup
	}
	planned, err := PlanDynamicConsumers("s", "p", "x.{{.PartitionID}}", partitions)
	require.NoError(t, err)
	require.Len(t, planned, 1)
}

// --- Input validation tests ---

func TestPlanDynamicConsumers_RejectsEmptyStreamName(t *testing.T) {
	_, err := PlanDynamicConsumers("", "p", "x.{{.PartitionID}}", []types.Partition{{Keys: []string{"a"}}})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "streamName is required")
}

func TestPlanDynamicConsumers_RejectsEmptyConsumerPrefix(t *testing.T) {
	_, err := PlanDynamicConsumers("s", "", "x.{{.PartitionID}}", []types.Partition{{Keys: []string{"a"}}})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "consumerPrefix is required")
}

func TestPlanDynamicConsumers_RejectsBadConsumerPrefixChars(t *testing.T) {
	cases := []string{"foo bar", "foo/bar", "foo$bar"}
	for _, prefix := range cases {
		t.Run(prefix, func(t *testing.T) {
			_, err := PlanDynamicConsumers("s", prefix, "x.{{.PartitionID}}", []types.Partition{{Keys: []string{"a"}}})
			require.ErrorIs(t, err, ErrInvalidConfig)
			require.Contains(t, err.Error(), "invalid characters")
		})
	}
}

func TestPlanDynamicConsumers_RejectsEmptyTemplate(t *testing.T) {
	_, err := PlanDynamicConsumers("s", "p", "", []types.Partition{{Keys: []string{"a"}}})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "subjectTemplate is required")
}

func TestPlanDynamicConsumers_RejectsTemplateMissingPlaceholder(t *testing.T) {
	_, err := PlanDynamicConsumers("s", "p", "no.placeholder", []types.Partition{{Keys: []string{"a"}}})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "must contain {{.PartitionID}}")
}

func TestPlanDynamicConsumers_RejectsZeroPartitions(t *testing.T) {
	_, err := PlanDynamicConsumers("s", "p", "x.{{.PartitionID}}", nil)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "at least one partition is required")
}

func TestPlanDynamicConsumers_RejectsPartitionWithNoKeys(t *testing.T) {
	_, err := PlanDynamicConsumers("s", "p", "x.{{.PartitionID}}", []types.Partition{{Keys: nil}})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "partition has no keys")
}

// --- ValidateLiveDynamicConsumers cancellation tests ---

// cancelAfterNthErrCtx is a context.Context whose Err() returns
// context.Canceled only after the Nth call. This lets a test make the
// cancellation observable between the top-of-loop ctx.Err() check and the
// post-helper ctx.Err() check without requiring any NATS interaction.
//
// Why a context wrapper rather than a JS fake: v1 hardcodes RecoveryDisabled,
// so consumer.CheckWorkQueueRecoveryCompat short-circuits immediately (at the
// strategy switch) without touching JetStream at all. A JS fake intercepting
// Stream() would be a no-op. The only reliable way to simulate "context
// cancelled during the helper call" is to inject cancellation via Err().
type cancelAfterNthErrCtx struct {
	context.Context
	n     int64
	count atomic.Int64
}

func (c *cancelAfterNthErrCtx) Err() error {
	if c.count.Add(1) > c.n {
		return context.Canceled
	}
	return c.Context.Err()
}

// TestValidateLiveDynamicConsumers_DetectsCancellationAcrossHelperCall is the
// regression test for the P0 finding in the W4 post-impl review:
// ValidateLiveDynamicConsumers could return nil even after the caller's
// context was cancelled, because CheckWorkQueueRecoveryCompat treats JS errors
// as benign and returns nil — silently masking any mid-call cancellation.
//
// This test MUST FAIL on the pre-fix code (which only checks ctx.Err() before
// the helper call) and PASS after the fix (which also checks after).
//
// With n=2 and one cfg entry, the sequence of Err() calls is:
//   - call 1: top-of-function guard (line 133) → returns nil
//   - call 2: top-of-loop guard (line 137) → returns nil → loop proceeds
//   - helper call (RecoveryDisabled): returns nil immediately with no Err() calls
//   - call 3: post-helper guard added by the fix → returns Canceled
//
// Pre-fix code never reaches call 3, so it returns nil. Post-fix code hits
// call 3 and returns context.Canceled.
func TestValidateLiveDynamicConsumers_DetectsCancellationAcrossHelperCall(t *testing.T) {
	// cancelAfterNthErrCtx.Err() returns nil for the first n calls, then
	// context.Canceled. With n=2, the top-of-function guard (call 1) and
	// top-of-loop guard (call 2) both see nil; the post-helper check (call 3,
	// added by the fix) sees Canceled.
	ctx := &cancelAfterNthErrCtx{Context: context.Background(), n: 2}

	err := ValidateLiveDynamicConsumers(ctx, nil, []DynamicConsumerCfg{
		{StreamName: "s", ConsumerPrefix: "p", SubjectTemplate: "x.{{.PartitionID}}"},
	})
	require.Error(t, err, "expected context.Canceled; pre-fix code returns nil here")
	require.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got %v", err)
}
