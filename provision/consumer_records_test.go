package provision

import (
	"testing"

	"github.com/arloliu/parti/v2/internal/dynamicbuild"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// resolveConsumerPartitions
// ---------------------------------------------------------------------------

func TestResolveConsumerPartitions_ReturnsDeclaredPartitions(t *testing.T) {
	t.Parallel()

	parts := []types.Partition{
		{Keys: []string{"a"}, Weight: 1},
		{Keys: []string{"b"}, Weight: 2},
	}
	cfg := Config{
		APIVersion: APIVersionV1,
		PartitionSource: &PartitionSourceConfig{
			Bucket:     "ps",
			Key:        "k",
			Partitions: parts,
		},
	}
	got := resolveConsumerPartitions(cfg)
	require.Equal(t, parts, got)
}

// ---------------------------------------------------------------------------
// ValidateConsumerSet — W1 validation tests
// ---------------------------------------------------------------------------

func TestValidateConsumerSet_NoOptedIn_Error(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		DynamicConsumers: []DynamicConsumerCfg{
			{StreamName: "s", ConsumerPrefix: "p", SubjectTemplate: "x.{{.PartitionID}}"},
		},
		PartitionSource: &PartitionSourceConfig{
			Bucket:     "ps",
			Key:        "k",
			Partitions: []types.Partition{{Keys: []string{"a"}}},
		},
	}
	err := ValidateConsumerSet(cfg)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "no dynamic consumers opted into precreation")
}

func TestValidateConsumerSet_EmptyPartitionsRefSkipped(t *testing.T) {
	t.Parallel()

	// An entry with empty PartitionsRef is silently skipped, so if that is the
	// only entry, we still get the "no opted-in" error — not a nil.
	cfg := Config{
		APIVersion: APIVersionV1,
		DynamicConsumers: []DynamicConsumerCfg{
			{StreamName: "s", ConsumerPrefix: "p", SubjectTemplate: "x.{{.PartitionID}}", PartitionsRef: ""},
		},
	}
	err := ValidateConsumerSet(cfg)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "no dynamic consumers opted into precreation")
}

func TestValidateConsumerSet_PartitionsRefMismatch_Error(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		DynamicConsumers: []DynamicConsumerCfg{
			{StreamName: "s", ConsumerPrefix: "p", SubjectTemplate: "x.{{.PartitionID}}", PartitionsRef: "other-bucket"},
		},
		PartitionSource: &PartitionSourceConfig{
			Bucket:     "ps",
			Key:        "k",
			Partitions: []types.Partition{{Keys: []string{"a"}}},
		},
	}
	err := ValidateConsumerSet(cfg)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "does not match partitionSource.bucket")
}

func TestValidateConsumerSet_NilPartitionSource_Error(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		DynamicConsumers: []DynamicConsumerCfg{
			{StreamName: "s", ConsumerPrefix: "p", SubjectTemplate: "x.{{.PartitionID}}", PartitionsRef: "ps"},
		},
		PartitionSource: nil,
	}
	err := ValidateConsumerSet(cfg)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "partitionSource is not declared")
}

func TestValidateConsumerSet_EmptyPartitions_Error(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		DynamicConsumers: []DynamicConsumerCfg{
			{StreamName: "s", ConsumerPrefix: "p", SubjectTemplate: "x.{{.PartitionID}}", PartitionsRef: "ps"},
		},
		PartitionSource: &PartitionSourceConfig{
			Bucket:     "ps",
			Key:        "k",
			Partitions: nil,
		},
	}
	err := ValidateConsumerSet(cfg)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "partitions is empty")
}

func TestValidateConsumerSet_InvalidConsumerPrefix_Error(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		DynamicConsumers: []DynamicConsumerCfg{
			{StreamName: "s", ConsumerPrefix: "bad prefix", SubjectTemplate: "x.{{.PartitionID}}", PartitionsRef: "ps"},
		},
		PartitionSource: &PartitionSourceConfig{
			Bucket:     "ps",
			Key:        "k",
			Partitions: []types.Partition{{Keys: []string{"a"}}},
		},
	}
	err := ValidateConsumerSet(cfg)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "invalid characters")
}

func TestValidateConsumerSet_InvalidSubjectTemplate_Error(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		DynamicConsumers: []DynamicConsumerCfg{
			{StreamName: "s", ConsumerPrefix: "p", SubjectTemplate: "no-placeholder", PartitionsRef: "ps"},
		},
		PartitionSource: &PartitionSourceConfig{
			Bucket:     "ps",
			Key:        "k",
			Partitions: []types.Partition{{Keys: []string{"a"}}},
		},
	}
	err := ValidateConsumerSet(cfg)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "{{.PartitionID}}")
}

func TestValidateConsumerSet_EmptyStreamName_Error(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		DynamicConsumers: []DynamicConsumerCfg{
			{StreamName: "", ConsumerPrefix: "p", SubjectTemplate: "x.{{.PartitionID}}", PartitionsRef: "ps"},
		},
		PartitionSource: &PartitionSourceConfig{
			Bucket:     "ps",
			Key:        "k",
			Partitions: []types.Partition{{Keys: []string{"a"}}},
		},
	}
	err := ValidateConsumerSet(cfg)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "streamName is required")
}

func TestValidateConsumerSet_EmptyConsumerPrefix_Error(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		DynamicConsumers: []DynamicConsumerCfg{
			{StreamName: "s", ConsumerPrefix: "", SubjectTemplate: "x.{{.PartitionID}}", PartitionsRef: "ps"},
		},
		PartitionSource: &PartitionSourceConfig{
			Bucket:     "ps",
			Key:        "k",
			Partitions: []types.Partition{{Keys: []string{"a"}}},
		},
	}
	err := ValidateConsumerSet(cfg)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "consumerPrefix is required")
}

func TestValidateConsumerSet_Valid(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		DynamicConsumers: []DynamicConsumerCfg{
			{StreamName: "s", ConsumerPrefix: "p", SubjectTemplate: "x.{{.PartitionID}}", PartitionsRef: "ps"},
		},
		PartitionSource: &PartitionSourceConfig{
			Bucket:     "ps",
			Key:        "k",
			Partitions: []types.Partition{{Keys: []string{"a"}}, {Keys: []string{"b"}}},
		},
	}
	require.NoError(t, ValidateConsumerSet(cfg))
}

// TestValidateConsumerSet_MixedOptedAndNonOpted verifies a mix of opted-in and
// non-opted-in entries — only the opted-in one is validated, and the overall
// result is valid.
func TestValidateConsumerSet_MixedOptedAndNonOpted(t *testing.T) {
	t.Parallel()

	cfg := Config{
		APIVersion: APIVersionV1,
		DynamicConsumers: []DynamicConsumerCfg{
			// Not opted in — skipped entirely.
			{StreamName: "s", ConsumerPrefix: "skip", SubjectTemplate: "y.{{.PartitionID}}"},
			// Opted in.
			{StreamName: "s", ConsumerPrefix: "opted", SubjectTemplate: "x.{{.PartitionID}}", PartitionsRef: "ps"},
		},
		PartitionSource: &PartitionSourceConfig{
			Bucket:     "ps",
			Key:        "k",
			Partitions: []types.Partition{{Keys: []string{"a"}}},
		},
	}
	require.NoError(t, ValidateConsumerSet(cfg))
}

// ---------------------------------------------------------------------------
// checkConsumerIdentityFields — unit tests
// ---------------------------------------------------------------------------

func TestCheckConsumerIdentityFields_AllMatch(t *testing.T) {
	t.Parallel()

	expected := PlannedConsumer{
		StreamName: "s",
		Durable:    "d",
		Subject:    "x.a",
	}
	live := jetstream.ConsumerConfig{
		FilterSubject: "x.a",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		MemoryStorage: false,
	}
	detail := checkConsumerIdentityFields(live, expected)
	require.Nil(t, detail)
}

func TestCheckConsumerIdentityFields_FilterSubjectMismatch(t *testing.T) {
	t.Parallel()

	expected := PlannedConsumer{Subject: "x.a"}
	live := jetstream.ConsumerConfig{
		FilterSubject: "x.WRONG",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	}
	detail := checkConsumerIdentityFields(live, expected)
	require.NotNil(t, detail)
	require.Contains(t, detail, "filterSubject")
	require.NotContains(t, detail, "ackPolicy")
}

func TestCheckConsumerIdentityFields_AckPolicyMismatch(t *testing.T) {
	t.Parallel()

	expected := PlannedConsumer{Subject: "x.a"}
	live := jetstream.ConsumerConfig{
		FilterSubject: "x.a",
		AckPolicy:     jetstream.AckNonePolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	}
	detail := checkConsumerIdentityFields(live, expected)
	require.NotNil(t, detail)
	require.Contains(t, detail, "ackPolicy")
	require.NotContains(t, detail, "filterSubject")
}

func TestCheckConsumerIdentityFields_MemoryStorageMismatch(t *testing.T) {
	t.Parallel()

	expected := PlannedConsumer{Subject: "x.a"}
	live := jetstream.ConsumerConfig{
		FilterSubject: "x.a",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		MemoryStorage: true,
	}
	detail := checkConsumerIdentityFields(live, expected)
	require.NotNil(t, detail)
	require.Contains(t, detail, "memoryStorage")
}

func TestCheckConsumerIdentityFields_MultipleFieldsMismatch(t *testing.T) {
	t.Parallel()

	expected := PlannedConsumer{Subject: "x.a"}
	live := jetstream.ConsumerConfig{
		FilterSubject: "x.WRONG",
		AckPolicy:     jetstream.AckNonePolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		MemoryStorage: true,
	}
	detail := checkConsumerIdentityFields(live, expected)
	require.NotNil(t, detail)
	require.Contains(t, detail, "filterSubject")
	require.Contains(t, detail, "ackPolicy")
	require.Contains(t, detail, "memoryStorage")
}

// ---------------------------------------------------------------------------
// W2 identity-equivalence test
// ---------------------------------------------------------------------------

// TestPlanConsumers_IdentityEquivalence_WithDynamicBuild is the regression
// guard for the identity-field byte-equivalence invariant. It verifies that a
// PlannedConsumer produced by PlanDynamicConsumers carries Config fields
// (Name, Durable, FilterSubject, AckPolicy, DeliverPolicy, MemoryStorage) that
// are identical to what dynamicbuild.ConsumerConfig produces directly.
func TestPlanConsumers_IdentityEquivalence_WithDynamicBuild(t *testing.T) {
	t.Parallel()

	const (
		stream   = "orders"
		prefix   = "ord-worker"
		template = "orders.{{.PartitionID}}"
	)
	partitions := []types.Partition{
		{Keys: []string{"a"}},
		{Keys: []string{"b"}},
	}

	planned, err := PlanDynamicConsumers(stream, prefix, template, partitions)
	require.NoError(t, err)
	require.Len(t, planned, len(partitions))

	pre, suf, _ := dynamicbuild.ParseSubjectTemplateParts(template)

	for _, pc := range planned {
		durable := dynamicbuild.PerSubjectDurableName(prefix, pc.Subject, pre, suf)
		direct := dynamicbuild.ConsumerConfig(durable, pc.Subject, dynamicbuild.Defaults{
			AckPolicy: jetstream.AckExplicitPolicy,
		})

		require.Equal(t, direct.Name, pc.Config.Name, "Name must match")
		require.Equal(t, direct.Durable, pc.Config.Durable, "Durable must match")
		require.Equal(t, direct.FilterSubject, pc.Config.FilterSubject, "FilterSubject must match")
		require.Equal(t, direct.AckPolicy, pc.Config.AckPolicy, "AckPolicy must match")
		require.Equal(t, direct.DeliverPolicy, pc.Config.DeliverPolicy, "DeliverPolicy must match")
		require.Equal(t, direct.MemoryStorage, pc.Config.MemoryStorage, "MemoryStorage must match")
	}
}
