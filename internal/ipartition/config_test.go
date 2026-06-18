package ipartition

import (
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/partutil"
	"github.com/arloliu/parti/v2/partition"
	"github.com/stretchr/testify/require"
)

func TestConsumerConfig_Validate(t *testing.T) {
	tests := []struct {
		name           string
		subjectPattern string
		numPartitions  int
		consumerName   string
		partition      int
		wantErr        error
	}{
		{
			name:           "valid",
			subjectPattern: "events.{{key}}.completed.{{partition}}",
			numPartitions:  4,
			consumerName:   "consumer-0",
			partition:      0,
			wantErr:        nil,
		},
		{
			name:           "allows wildcard pattern",
			subjectPattern: "events.{{key}}.{{partition}}.>",
			numPartitions:  4,
			consumerName:   "consumer-0",
			partition:      0,
			wantErr:        nil,
		},
		{
			name:           "out of range",
			subjectPattern: "events.{{partition}}",
			numPartitions:  2,
			consumerName:   "consumer-9",
			partition:      9,
			wantErr:        partutil.ErrPartitionOutOfRange,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ConsumerConfig{
				PartitionConfig: partition.PartitionConfig{
					NumPartitions:  tc.numPartitions,
					SubjectPattern: tc.subjectPattern,
				},
				StreamName:   "EVENTS",
				ConsumerName: tc.consumerName,
				Partition:    tc.partition,
			}

			err := cfg.Validate()
			if tc.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.wantErr)
			}
		})
	}
}

func TestConsumerConfig_SetDefaults(t *testing.T) {
	tests := []struct {
		name             string
		batchSize        int
		fetchTimeout     time.Duration
		maxDeliver       int
		wantBatchSize    int
		wantFetchTimeout time.Duration
		wantMaxDeliver   int
		wantManualAck    bool
		wantLogger       bool
	}{
		{
			name:             "applies defaults",
			wantBatchSize:    1,
			wantFetchTimeout: 5 * time.Second,
			wantMaxDeliver:   -1,
			wantManualAck:    false,
			wantLogger:       true,
		},
		{
			name:             "preserves existing values",
			batchSize:        50,
			fetchTimeout:     10 * time.Second,
			maxDeliver:       5,
			wantBatchSize:    50,
			wantFetchTimeout: 10 * time.Second,
			wantMaxDeliver:   5,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ConsumerConfig{
				PartitionConfig: partition.PartitionConfig{
					NumPartitions:  4,
					SubjectPattern: "events.{{partition}}",
				},
				StreamName:   "EVENTS",
				ConsumerName: "consumer-0",
				Partition:    0,
				BatchSize:    tc.batchSize,
				FetchTimeout: tc.fetchTimeout,
				MaxDeliver:   tc.maxDeliver,
			}
			require.NoError(t, cfg.Validate())

			require.Equal(t, tc.wantBatchSize, cfg.BatchSize)
			require.Equal(t, tc.wantFetchTimeout, cfg.FetchTimeout)
			require.Equal(t, tc.wantMaxDeliver, cfg.MaxDeliver)
			if tc.wantLogger {
				require.NotNil(t, cfg.Logger)
				require.Equal(t, tc.wantManualAck, cfg.ManualAck)
			}
		})
	}
}

func TestConsumerConfig_DispatchByKey(t *testing.T) {
	tests := []struct {
		name           string
		subjectPattern string
		wantErr        error
	}{
		{
			name:           "requires key placeholder",
			subjectPattern: "events.{{partition}}.data", // No {{key}}
			wantErr:        partutil.ErrDispatchByKeyRequiresKeyPlaceholder,
		},
		{
			name:           "with key placeholder",
			subjectPattern: "events.{{partition}}.{{key}}", // Has {{key}}
			wantErr:        nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dispatchEnabled := true
			cfg := ConsumerConfig{
				PartitionConfig: partition.PartitionConfig{
					NumPartitions:  4,
					SubjectPattern: tc.subjectPattern,
				},
				StreamName:    "EVENTS",
				ConsumerName:  "consumer-0",
				Partition:     0,
				DispatchByKey: &dispatchEnabled,
			}

			err := cfg.Validate()
			if tc.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.wantErr)
			}
		})
	}
}

func TestConsumerConfig_AcceptsLastProcessedWithManualAck(t *testing.T) {
	cfg := ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "events.{{partition}}",
		},
		StreamName:       "EVENTS",
		ConsumerName:     "consumer-0",
		Partition:        0,
		ManualAck:        true,
		RecoveryStrategy: durable.RecoverFromLastProcessed,
	}

	err := cfg.Validate()
	require.NoError(t, err, "ManualAck=true + RecoverFromLastProcessed must be accepted")
}

// --- merged from options_test.go ---

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
	require.Equal(t, "EVENTS", cfg.StreamName)
	require.Equal(t, "consumer-0", cfg.ConsumerName)
	require.Equal(t, 0, cfg.Partition)
}
