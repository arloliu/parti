package ipartition

import (
	"testing"

	"github.com/arloliu/parti/v2/partition"
	"github.com/stretchr/testify/require"
)

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

// TestImplementsPartitionJSConsumer ensures JSConsumer implements PartitionJSConsumer.
func TestImplementsPartitionJSConsumer(t *testing.T) {
	var _ PartitionJSConsumer = (*JSConsumer)(nil)
}

// TestConsumerConfigWithPartitionConfig verifies that partition.PartitionConfig embeds correctly.
func TestConsumerConfigWithPartitionConfig(t *testing.T) {
	cfg := ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  4,
			SubjectPattern: "events.{{partition}}",
		},
		StreamName:   "EVENTS",
		ConsumerName: "consumer-0",
		Partition:    0,
	}
	require.Equal(t, 4, cfg.NumPartitions)
	require.Equal(t, "events.{{partition}}", cfg.SubjectPattern)
}
