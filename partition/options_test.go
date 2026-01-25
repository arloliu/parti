package partition

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewConfigOptions(t *testing.T) {
	cfg := NewConfig(
		WithNumPartitions(8),
		WithSubjectPattern("events.{{partition}}"),
		WithHashSeed(42),
	)

	require.Equal(t, 8, cfg.NumPartitions)
	require.Equal(t, "events.{{partition}}", cfg.SubjectPattern)
	require.Equal(t, uint64(42), cfg.HashSeed)
}

func TestNewConsumerConfigOptions(t *testing.T) {
	cfg := NewConsumerConfig(
		WithStreamName("EVENTS"),
		WithConsumerName("consumer-1"),
		WithPartitionIndex(1),
		WithConsumerNumPartitions(4),
		WithConsumerSubjectPattern("events.{{partition}}"),
	)

	require.Equal(t, "EVENTS", cfg.StreamName)
	require.Equal(t, "consumer-1", cfg.ConsumerName)
	require.Equal(t, 1, cfg.Partition)
	require.Equal(t, 4, cfg.NumPartitions)
	require.Equal(t, "events.{{partition}}", cfg.SubjectPattern)
}
