package partition

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPartitionConfigValidate(t *testing.T) {
	cfg := PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.{{key}}.completed.{{partition}}",
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
