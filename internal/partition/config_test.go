package ipartition

import (
	"errors"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/partutil"
	"github.com/arloliu/parti/v2/partition"
	"github.com/stretchr/testify/require"
)

func TestConsumerConfigValidate(t *testing.T) {
	cfg := ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
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
		PartitionConfig: partition.PartitionConfig{
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
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "events.{{partition}}",
		},
		StreamName:   "EVENTS",
		ConsumerName: "consumer-9",
		Partition:    9,
	}

	err := cfg.Validate()
	require.ErrorIs(t, err, partutil.ErrPartitionOutOfRange)
}

func TestConsumerConfig_SetDefaults(t *testing.T) {
	cfg := ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
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
	require.Equal(t, 1, cfg.BatchSize)
	require.Equal(t, 5*time.Second, cfg.FetchTimeout)
	require.False(t, cfg.ManualAck)
	require.Equal(t, -1, cfg.MaxDeliver)
}

func TestConsumerConfig_SetDefaults_PreservesExistingValues(t *testing.T) {
	cfg := ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
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
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  4,
			SubjectPattern: "events.{{partition}}.data", // No {{key}}
		},
		StreamName:    "EVENTS",
		ConsumerName:  "consumer-0",
		Partition:     0,
		DispatchByKey: &dispatchEnabled,
	}

	err := cfg.Validate()
	require.True(t, errors.Is(err, partutil.ErrDispatchByKeyRequiresKeyPlaceholder))
}

func TestConsumerConfig_DispatchByKeyWithKeyPlaceholder(t *testing.T) {
	dispatchEnabled := true
	cfg := ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
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
