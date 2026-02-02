package partition

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPartitionConfigValidate(t *testing.T) {
	cfg := PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.{{key}}.completed.{{partition}}",
	}

	require.NoError(t, cfg.Validate())
}

func TestPartitionConfigValidate_AllowsWildcardPattern(t *testing.T) {
	cfg := PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.*.{{key}}.{{partition}}",
	}

	require.NoError(t, cfg.Validate())
}

func TestPartitionConfigValidate_InvalidPattern(t *testing.T) {
	cfg := PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events..{{partition}}",
	}

	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPatternEmptyToken))
}

func TestConsumerConfigValidate(t *testing.T) {
	cfg := ConsumerConfig{
		PartitionConfig: PartitionConfig{
			NumPartitions:  4,
			SubjectPattern: "events.{{key}}.completed.{{partition}}",
		},
		StreamName:   "EVENTS",
		ConsumerName: "consumer-0",
		Partition:    0,
	}

	require.NoError(t, cfg.Validate())
}

func TestConsumerConfigValidate_AllowsWildcardPattern(t *testing.T) {
	cfg := ConsumerConfig{
		PartitionConfig: PartitionConfig{
			NumPartitions:  4,
			SubjectPattern: "events.{{key}}.{{partition}}.>",
		},
		StreamName:   "EVENTS",
		ConsumerName: "consumer-0",
		Partition:    0,
	}

	require.NoError(t, cfg.Validate())
}

func TestConsumerConfigValidate_OutOfRange(t *testing.T) {
	cfg := ConsumerConfig{
		PartitionConfig: PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "events.{{partition}}",
		},
		StreamName:   "EVENTS",
		ConsumerName: "consumer-9",
		Partition:    9,
	}

	err := cfg.Validate()
	require.ErrorIs(t, err, ErrPartitionOutOfRange)
}

func TestPartitionConfig_SetDefaults(t *testing.T) {
	cfg := PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.{{partition}}",
	}
	require.NoError(t, cfg.setDefaults())

	// Logger should be set to no-op
	require.NotNil(t, cfg.Logger)
}

func TestPartitionConfig_SetDefaults_PreservesExistingValues(t *testing.T) {
	cfg := PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.{{partition}}",
		HashSeed:       12345,
	}
	require.NoError(t, cfg.setDefaults())

	// Existing values preserved
	require.Equal(t, uint64(12345), cfg.HashSeed)
	require.NotNil(t, cfg.Logger)
}

func TestConsumerConfig_SetDefaults(t *testing.T) {
	cfg := ConsumerConfig{
		PartitionConfig: PartitionConfig{
			NumPartitions:  4,
			SubjectPattern: "events.{{partition}}",
		},
		StreamName:   "EVENTS",
		ConsumerName: "consumer-0",
		Partition:    0,
	}
	require.NoError(t, cfg.Validate())

	// Check defaults are applied (Validate calls setDefaults internally)
	require.NotNil(t, cfg.Logger)
	require.Equal(t, 100, cfg.BatchSize)
	require.Equal(t, 5*time.Second, cfg.FetchTimeout)
	require.False(t, cfg.ManualAck)
	require.Equal(t, 0, cfg.MaxDeliver)
}

func TestConsumerConfig_SetDefaults_PreservesExistingValues(t *testing.T) {
	cfg := ConsumerConfig{
		PartitionConfig: PartitionConfig{
			NumPartitions:  4,
			SubjectPattern: "events.{{partition}}",
		},
		StreamName:   "EVENTS",
		ConsumerName: "consumer-0",
		Partition:    0,
		BatchSize:    50,
		FetchTimeout: 10 * time.Second,
		MaxDeliver:   5,
	}
	require.NoError(t, cfg.Validate())

	// Existing values preserved
	require.Equal(t, 50, cfg.BatchSize)
	require.Equal(t, 10*time.Second, cfg.FetchTimeout)
	require.Equal(t, 5, cfg.MaxDeliver)
}

func TestConsumerConfig_DispatchByKeyRequiresKeyPlaceholder(t *testing.T) {
	dispatchEnabled := true
	cfg := ConsumerConfig{
		PartitionConfig: PartitionConfig{
			NumPartitions:  4,
			SubjectPattern: "events.{{partition}}.data", // No {{key}}
		},
		StreamName:    "EVENTS",
		ConsumerName:  "consumer-0",
		Partition:     0,
		DispatchByKey: &dispatchEnabled,
	}

	err := cfg.Validate()
	require.ErrorIs(t, err, ErrDispatchByKeyRequiresKeyPlaceholder)
}

func TestConsumerConfig_DispatchByKeyWithKeyPlaceholder(t *testing.T) {
	dispatchEnabled := true
	cfg := ConsumerConfig{
		PartitionConfig: PartitionConfig{
			NumPartitions:  4,
			SubjectPattern: "events.{{partition}}.{{key}}", // Has {{key}}
		},
		StreamName:    "EVENTS",
		ConsumerName:  "consumer-0",
		Partition:     0,
		DispatchByKey: &dispatchEnabled,
	}

	err := cfg.Validate()
	require.NoError(t, err)
}
