package provision_test

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/internal/dynamicbuild"
	"github.com/arloliu/parti/v2/jsutil"
	"github.com/arloliu/parti/v2/provision"
	"github.com/arloliu/parti/v2/types"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

const (
	crStream   = "orders"
	crPrefix   = "ord-worker"
	crTemplate = "orders.{{.PartitionID}}"
	crBucket   = "parti-partitions"
	crKey      = "partitions/v1"
)

func crPartitions(keys ...string) []types.Partition {
	parts := make([]types.Partition, len(keys))
	for i, k := range keys {
		parts[i] = types.Partition{Keys: []string{k}, Weight: 1}
	}
	return parts
}

func crConfig(parts ...types.Partition) provision.Config {
	return provision.Config{
		APIVersion: provision.APIVersionV1,
		DynamicConsumers: []provision.DynamicConsumerCfg{
			{
				StreamName:      crStream,
				ConsumerPrefix:  crPrefix,
				SubjectTemplate: crTemplate,
				PartitionsRef:   crBucket,
			},
		},
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket:     crBucket,
			Key:        crKey,
			Partitions: parts,
		},
	}
}

// createOrdersStream creates the test stream "orders" with subject "orders.>".
func createOrdersStream(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	createStream(t, js, jetstream.StreamConfig{
		Name:     crStream,
		Subjects: []string{"orders.>"},
		Storage:  jetstream.MemoryStorage,
	})
}

// durableName computes the expected durable for partition "a" — the single
// partition the drift-immutable tests use.
func durableName() string {
	pre, suf, _ := dynamicbuild.ParseSubjectTemplateParts(crTemplate)
	return dynamicbuild.PerSubjectDurableName(crPrefix, "orders.a", pre, suf)
}

// createConsumer creates a live consumer on the orders stream.
func createConsumer(t *testing.T, js jetstream.JetStream, cfg jetstream.ConsumerConfig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := js.CreateConsumer(ctx, crStream, cfg)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// PlanConsumers — static validation (no NATS needed)
// ---------------------------------------------------------------------------

func TestPlanConsumers_RejectsBadAPIVersion(t *testing.T) {
	t.Parallel()

	_, err := provision.PlanConsumers(t.Context(), nil, provision.Config{
		APIVersion: "parti.io/v99",
	})
	require.ErrorIs(t, err, provision.ErrInvalidConfig)
}

func TestPlanConsumers_RejectsNoOptedInTarget(t *testing.T) {
	t.Parallel()

	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		DynamicConsumers: []provision.DynamicConsumerCfg{
			{StreamName: "s", ConsumerPrefix: "p", SubjectTemplate: "x.{{.PartitionID}}"},
		},
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket:     "ps",
			Key:        "k",
			Partitions: []types.Partition{{Keys: []string{"a"}}},
		},
	}
	_, err := provision.PlanConsumers(t.Context(), nil, cfg)
	require.ErrorIs(t, err, provision.ErrInvalidConfig)
	require.Contains(t, err.Error(), "no dynamic consumers opted into precreation")
}

func TestPlanConsumers_BadConfigBeatsCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := provision.PlanConsumers(ctx, nil, provision.Config{APIVersion: "parti.io/v99"})
	require.ErrorIs(t, err, provision.ErrInvalidConfig)
	require.NotErrorIs(t, err, context.Canceled)
}

// ---------------------------------------------------------------------------
// PlanConsumers — all missing → create-consumer + drift-mutable per partition
// ---------------------------------------------------------------------------

func TestPlanConsumers_AllMissing_CreateActionsAndDriftMutable(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	parts := crPartitions("a", "b")
	plan, err := provision.PlanConsumers(t.Context(), js, crConfig(parts...))
	require.NoError(t, err)

	require.Len(t, plan.Actions, 2, "two create-consumer actions expected")
	require.Len(t, plan.Drift, 2, "two drift-mutable findings expected")

	for _, action := range plan.Actions {
		require.Equal(t, provision.ActionCreateConsumer, action.Kind)
		res, ok := action.Resource.(provision.PlannedConsumer)
		require.True(t, ok, "resource must be PlannedConsumer value")
		require.Equal(t, crStream, res.StreamName)
	}

	for _, df := range plan.Drift {
		require.Equal(t, provision.SeverityDriftMutable, df.Severity)
		require.Equal(t, provision.KindDynamicConsumer, df.Kind)
	}
}

// ---------------------------------------------------------------------------
// PlanConsumers — all present matching → all informational, no actions
// ---------------------------------------------------------------------------

func TestPlanConsumers_AllPresent_AllInformational_NoActions(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	parts := crPartitions("a", "b")

	// Pre-create both consumers via the shared builder with the runtime
	// defaults — exactly the config provision precreates.
	for _, p := range parts {
		pre, suf, _ := dynamicbuild.ParseSubjectTemplateParts(crTemplate)
		subject := "orders." + p.Keys[0]
		durable := dynamicbuild.PerSubjectDurableName(crPrefix, subject, pre, suf)
		createConsumer(t, js, dynamicbuild.ConsumerConfig(durable, subject, dynamicbuild.DefaultDynamicDefaults()))
	}

	plan, err := provision.PlanConsumers(t.Context(), js, crConfig(parts...))
	require.NoError(t, err)

	require.Empty(t, plan.Actions, "no actions when all consumers exist and match")
	require.Len(t, plan.Drift, 2, "one informational finding per consumer")
	for _, df := range plan.Drift {
		require.Equal(t, provision.SeverityInformational, df.Severity)
		require.Equal(t, provision.KindDynamicConsumer, df.Kind)
	}
}

// ---------------------------------------------------------------------------
// PlanConsumers — mixed: some missing, some present matching
// ---------------------------------------------------------------------------

func TestPlanConsumers_Mixed_SomeMissing_SomePresent(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	parts := crPartitions("a", "b", "c")

	// Pre-create only "a", via the shared builder with the runtime defaults.
	pre, suf, _ := dynamicbuild.ParseSubjectTemplateParts(crTemplate)
	subjectA := "orders.a"
	durableA := dynamicbuild.PerSubjectDurableName(crPrefix, subjectA, pre, suf)
	createConsumer(t, js, dynamicbuild.ConsumerConfig(durableA, subjectA, dynamicbuild.DefaultDynamicDefaults()))

	plan, err := provision.PlanConsumers(t.Context(), js, crConfig(parts...))
	require.NoError(t, err)

	require.Len(t, plan.Actions, 2, "two create-consumer for b and c")
	require.Len(t, plan.Drift, 3, "three drift findings total")

	var informational, driftMutable int
	for _, df := range plan.Drift {
		switch df.Severity {
		case provision.SeverityInformational:
			informational++
		case provision.SeverityDriftMutable:
			driftMutable++
		}
	}
	require.Equal(t, 1, informational)
	require.Equal(t, 2, driftMutable)
}

// ---------------------------------------------------------------------------
// PlanConsumers — drift-immutable cases
// ---------------------------------------------------------------------------

func TestPlanConsumers_FilterSubjectMismatch_DriftImmutable(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	parts := crPartitions("a")
	durable := durableName()

	// Create with a wrong FilterSubject; every other field is the runtime
	// default, so FilterSubject is the only divergence.
	createConsumer(t, js, dynamicbuild.ConsumerConfig(durable, "orders.WRONG", dynamicbuild.DefaultDynamicDefaults()))

	plan, err := provision.PlanConsumers(t.Context(), js, crConfig(parts...))
	require.NoError(t, err)

	require.Empty(t, plan.Actions, "no actions for drift-immutable")
	require.Len(t, plan.Drift, 1)
	require.Equal(t, provision.SeverityDriftImmutable, plan.Drift[0].Severity)
	require.Equal(t, provision.KindDynamicConsumer, plan.Drift[0].Kind)
	require.NotNil(t, plan.Drift[0].Detail)
	require.Contains(t, plan.Drift[0].Detail, "filterSubject")
}

func TestPlanConsumers_AckPolicyMismatch_DriftImmutable(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	parts := crPartitions("a")
	durable := durableName()
	subject := "orders.a"

	// Create with AckNone policy; every other field is the runtime default.
	ackCfg := dynamicbuild.ConsumerConfig(durable, subject, dynamicbuild.DefaultDynamicDefaults())
	ackCfg.AckPolicy = jetstream.AckNonePolicy
	createConsumer(t, js, ackCfg)

	plan, err := provision.PlanConsumers(t.Context(), js, crConfig(parts...))
	require.NoError(t, err)

	require.Empty(t, plan.Actions)
	require.Len(t, plan.Drift, 1)
	require.Equal(t, provision.SeverityDriftImmutable, plan.Drift[0].Severity)
	require.Contains(t, plan.Drift[0].Detail, "ackPolicy")
}

func TestPlanConsumers_MemoryStorageMismatch_DriftImmutable(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	parts := crPartitions("a")
	durable := durableName()
	subject := "orders.a"

	// Create with memory storage; every other field is the runtime default.
	memCfg := dynamicbuild.ConsumerConfig(durable, subject, dynamicbuild.DefaultDynamicDefaults())
	memCfg.MemoryStorage = true
	createConsumer(t, js, memCfg)

	plan, err := provision.PlanConsumers(t.Context(), js, crConfig(parts...))
	require.NoError(t, err)

	require.Empty(t, plan.Actions)
	require.Len(t, plan.Drift, 1)
	require.Equal(t, provision.SeverityDriftImmutable, plan.Drift[0].Severity)
	require.Contains(t, plan.Drift[0].Detail, "memoryStorage")
}

func TestPlanConsumers_MaxWaitingMismatch_DriftImmutable(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	parts := crPartitions("a")
	durable := durableName()
	subject := "orders.a"

	// Create with a non-default MaxWaiting; every other field is the runtime
	// default. MaxWaiting is NATS-immutable, so this is drift-immutable.
	mwCfg := dynamicbuild.ConsumerConfig(durable, subject, dynamicbuild.DefaultDynamicDefaults())
	mwCfg.MaxWaiting = 17
	createConsumer(t, js, mwCfg)

	plan, err := provision.PlanConsumers(t.Context(), js, crConfig(parts...))
	require.NoError(t, err)

	require.Empty(t, plan.Actions)
	require.Len(t, plan.Drift, 1)
	require.Equal(t, provision.SeverityDriftImmutable, plan.Drift[0].Severity)
	require.Contains(t, plan.Drift[0].Detail, "maxWaiting")
}

// ---------------------------------------------------------------------------
// PlanConsumers — ErrConsumerStreamMissing
// ---------------------------------------------------------------------------

func TestPlanConsumers_StreamMissing_ErrConsumerStreamMissing(t *testing.T) {
	js := newJS(t) // no stream created

	parts := crPartitions("a")
	_, err := provision.PlanConsumers(t.Context(), js, crConfig(parts...))
	require.Error(t, err)
	require.ErrorIs(t, err, provision.ErrConsumerStreamMissing)
	require.ErrorIs(t, err, provision.ErrLiveValidation)
}

// ---------------------------------------------------------------------------
// PlanConsumers — durable names match PerSubjectDurableName
// ---------------------------------------------------------------------------

func TestPlanConsumers_DurableNamesMatchPerSubjectDurableName(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	parts := crPartitions("x", "y", "z")
	plan, err := provision.PlanConsumers(t.Context(), js, crConfig(parts...))
	require.NoError(t, err)
	require.Len(t, plan.Actions, 3)

	pre, suf, _ := dynamicbuild.ParseSubjectTemplateParts(crTemplate)
	for _, action := range plan.Actions {
		res, ok := action.Resource.(provision.PlannedConsumer)
		require.True(t, ok)
		expected := dynamicbuild.PerSubjectDurableName(crPrefix, res.Subject, pre, suf)
		require.Equal(t, expected, action.Name, "action Name must be the durable name")
		require.Equal(t, expected, res.Durable, "PlannedConsumer.Durable must equal PerSubjectDurableName")
	}
}

// ---------------------------------------------------------------------------
// PlanConsumers — deterministic ordering
// ---------------------------------------------------------------------------

func TestPlanConsumers_DeterministicOrdering(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	parts := crPartitions("z", "a", "m")
	plan, err := provision.PlanConsumers(t.Context(), js, crConfig(parts...))
	require.NoError(t, err)
	require.Len(t, plan.Actions, 3)

	// Actions must be sorted by (Kind, Name).
	for i := 1; i < len(plan.Actions); i++ {
		prev, curr := plan.Actions[i-1], plan.Actions[i]
		if prev.Kind == curr.Kind {
			require.LessOrEqual(t, prev.Name, curr.Name, "actions must be sorted by Name within Kind")
		} else {
			require.LessOrEqual(t, prev.Kind, curr.Kind, "actions must be sorted by Kind")
		}
	}
}

// ---------------------------------------------------------------------------
// PlanConsumers — non-opted-in target produces no actions/findings
// ---------------------------------------------------------------------------

func TestPlanConsumers_EmptyPartitionsRefTarget_NoActionsOrFindings(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	// One opted-in target, one non-opted-in. The non-opted-in should be silent.
	cfg := provision.Config{
		APIVersion: provision.APIVersionV1,
		DynamicConsumers: []provision.DynamicConsumerCfg{
			// Non-opted-in target (alignment-check-only, unchanged from Phase 1).
			{StreamName: crStream, ConsumerPrefix: "skip", SubjectTemplate: "orders.{{.PartitionID}}"},
			// Opted-in target.
			{StreamName: crStream, ConsumerPrefix: crPrefix, SubjectTemplate: crTemplate, PartitionsRef: crBucket},
		},
		PartitionSource: &provision.PartitionSourceConfig{
			Bucket:     crBucket,
			Key:        crKey,
			Partitions: crPartitions("a"),
		},
	}

	plan, err := provision.PlanConsumers(t.Context(), js, cfg)
	require.NoError(t, err)

	// Only the opted-in target contributes actions/findings — one partition, all missing.
	require.Len(t, plan.Actions, 1, "only one opted-in target with one missing consumer")
	require.Len(t, plan.Drift, 1)
	require.Equal(t, provision.ActionCreateConsumer, plan.Actions[0].Kind)
	res, ok := plan.Actions[0].Resource.(provision.PlannedConsumer)
	require.True(t, ok)
	require.Equal(t, crPrefix, res.Config.Name[:len(crPrefix)], "durable starts with crPrefix")
}

// ---------------------------------------------------------------------------
// PlanConsumers — envelope fields
// ---------------------------------------------------------------------------

func TestPlanConsumers_EnvelopeFields(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	plan, err := provision.PlanConsumers(t.Context(), js, crConfig(crPartitions("a")...))
	require.NoError(t, err)

	require.Equal(t, provision.APIVersionProvisionV1, plan.APIVersion)
	require.Equal(t, provision.KindPlan, plan.Kind)
	require.NotNil(t, plan.Actions)
	require.NotNil(t, plan.Drift)
}

// ---------------------------------------------------------------------------
// No-oscillation invariant (the load-bearing runtime-owns test)
// ---------------------------------------------------------------------------

// TestPlanConsumers_NoOscillation verifies the central Phase-5 design decision:
// after PlanConsumers plans creation and consumers are created, simulating a
// runtime worker start via jsutil.EnsureConsumer with deliberately different
// mutable tunables (AckWait, MaxDeliver, etc.) must not cause PlanConsumers to
// report missing or non-informational drift.
func TestPlanConsumers_NoOscillation(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	parts := crPartitions("a", "b")
	cfg := crConfig(parts...)

	// Step 1: Plan — should see all consumers missing.
	plan1, err := provision.PlanConsumers(t.Context(), js, cfg)
	require.NoError(t, err)
	require.Len(t, plan1.Actions, 2)

	// Step 2: Create the consumers as PlanConsumers would (identity-only).
	for _, action := range plan1.Actions {
		res, ok := action.Resource.(provision.PlannedConsumer)
		require.True(t, ok)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := js.CreateConsumer(ctx, res.StreamName, res.Config)
		cancel()
		require.NoError(t, err)
	}

	// Step 3: Simulate a worker start — EnsureConsumer with the runtime's real
	// defaults (DefaultDynamicDefaults), but with the *mutable* tunables
	// overridden to deliberately different values. The immutable fields
	// (AckPolicy, MaxWaiting, MemoryStorage) stay at the runtime defaults — the
	// same values provision precreated — so the runtime's own
	// CreateOrUpdateConsumer does not fail on an immutable change. This proves
	// the no-oscillation invariant: a runtime that overwrites the mutable
	// tunables must not make PlanConsumers report drift.
	pre, suf, _ := dynamicbuild.ParseSubjectTemplateParts(crTemplate)
	for _, p := range parts {
		subject := "orders." + p.Keys[0]
		durable := dynamicbuild.PerSubjectDurableName(crPrefix, subject, pre, suf)

		runtimeDefaults := dynamicbuild.DefaultDynamicDefaults()
		runtimeDefaults.AckWait = 60 * time.Second           // deliberately different
		runtimeDefaults.MaxDeliver = 5                       // deliberately different
		runtimeDefaults.InactiveThreshold = 10 * time.Minute // deliberately different
		consumerCfg := dynamicbuild.ConsumerConfig(durable, subject, runtimeDefaults)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := jsutil.EnsureConsumer(ctx, js, crStream, consumerCfg)
		cancel()
		require.NoError(t, err, "EnsureConsumer with runtime tunables must succeed")
	}

	// Step 4: Re-run PlanConsumers. Must emit zero create-consumer actions and
	// only informational drift — hasDrift must be false.
	plan2, err := provision.PlanConsumers(t.Context(), js, cfg)
	require.NoError(t, err)

	require.Empty(t, plan2.Actions, "no create-consumer actions after runtime overwrites mutable tunables")
	require.Len(t, plan2.Drift, 2, "one informational finding per consumer")
	for _, df := range plan2.Drift {
		require.Equal(t, provision.SeverityInformational, df.Severity,
			"runtime-owned mutable tunables must not register as drift")
	}
}

// ---------------------------------------------------------------------------
// Precreate then read back
// ---------------------------------------------------------------------------

func TestPlanConsumers_PrecreateAndReadBack(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)

	parts := crPartitions("p1", "p2")
	plan, err := provision.PlanConsumers(t.Context(), js, crConfig(parts...))
	require.NoError(t, err)
	require.Len(t, plan.Actions, 2)

	// Create consumers.
	for _, action := range plan.Actions {
		res, ok := action.Resource.(provision.PlannedConsumer)
		require.True(t, ok)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := js.CreateConsumer(ctx, res.StreamName, res.Config)
		cancel()
		require.NoError(t, err)
	}

	// Read each consumer back and verify FilterSubject.
	pre, suf, _ := dynamicbuild.ParseSubjectTemplateParts(crTemplate)
	for _, p := range parts {
		subject := "orders." + p.Keys[0]
		durable := dynamicbuild.PerSubjectDurableName(crPrefix, subject, pre, suf)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		consumer, err := js.Consumer(ctx, crStream, durable)
		cancel()
		require.NoError(t, err)

		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		info, err := consumer.Info(ctx2)
		cancel2()
		require.NoError(t, err)
		require.Equal(t, subject, info.Config.FilterSubject)
	}
}

// ---------------------------------------------------------------------------
// Live partition mismatch (honest scope)
// ---------------------------------------------------------------------------

// TestPlanConsumers_HonestScope_DeclaredSetOnly verifies that PlanConsumers
// plans consumers only for the *declared* partition set, not the live NATS KV
// table. This encodes the documented honest-scope limit (Phase 5 Non-Goals).
func TestPlanConsumers_HonestScope_DeclaredSetOnly(t *testing.T) {
	js := newJS(t)
	createOrdersStream(t, js)
	createKV(t, js, jetstream.KeyValueConfig{Bucket: crBucket})

	// Declare partitions {A, B} in config.
	declaredParts := crPartitions("A", "B")

	// Write partitions {A, B, C} to the live KV table — simulating AddPartitions.
	writePartitionKey(t, js, crPartitions("A", "B", "C"))

	plan, err := provision.PlanConsumers(t.Context(), js, crConfig(declaredParts...))
	require.NoError(t, err)

	// Plan must only cover {A, B} — the declared set, not the live {A, B, C}.
	require.Len(t, plan.Actions, 2, "plan must cover declared set only: {A, B}")
	for _, action := range plan.Actions {
		res, ok := action.Resource.(provision.PlannedConsumer)
		require.True(t, ok)
		require.NotEqual(t, "orders.C", res.Subject,
			"consumer for undeclared partition C must not appear in plan")
	}
}
