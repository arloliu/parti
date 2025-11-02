package subscription

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	partitesting "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestNewDurableHelper(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)

	tests := []struct {
		name        string
		conn        any
		config      DurableConfig
		expectError bool
	}{
		{
			name: "valid configuration",
			conn: nc,
			config: DurableConfig{
				StreamName:      "test-stream",
				ConsumerPrefix:  "test",
				SubjectTemplate: "work.{{.PartitionID}}",
				AckPolicy:       jetstream.AckExplicitPolicy,
			},
			expectError: false,
		},
		{
			name:        "nil connection",
			conn:        nil,
			config:      DurableConfig{StreamName: "test"},
			expectError: true,
		},
		{
			name: "missing stream name",
			conn: nc,
			config: DurableConfig{
				ConsumerPrefix:  "test",
				SubjectTemplate: "work.{{.PartitionID}}",
			},
			expectError: true,
		},
		{
			name: "missing consumer prefix",
			conn: nc,
			config: DurableConfig{
				StreamName:      "test-stream",
				SubjectTemplate: "work.{{.PartitionID}}",
			},
			expectError: true,
		},
		{
			name: "missing subject template",
			conn: nc,
			config: DurableConfig{
				StreamName:     "test-stream",
				ConsumerPrefix: "test",
			},
			expectError: true,
		},
		{
			name: "invalid template syntax",
			conn: nc,
			config: DurableConfig{
				StreamName:      "test-stream",
				ConsumerPrefix:  "test",
				SubjectTemplate: "work.{{.PartitionID",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var helper *DurableHelper
			var err error

			if tt.conn == nil {
				helper, err = NewDurableHelper(nil, tt.config)
			} else {
				helper, err = NewDurableHelper(nc, tt.config)
			}

			if tt.expectError {
				require.Error(t, err)
				require.Nil(t, helper)
			} else {
				require.NoError(t, err)
				require.NotNil(t, helper)
			}
		})
	}
}

func TestDurableHelper_DefaultValues(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)

	tests := []struct {
		name   string
		config DurableConfig
		assert func(t *testing.T, helper *DurableHelper)
	}{
		{
			name: "all defaults applied",
			config: DurableConfig{
				StreamName:      "test-stream",
				ConsumerPrefix:  "test",
				SubjectTemplate: "work.{{.PartitionID}}",
				// Leave all optional fields zero-valued
			},
			assert: func(t *testing.T, helper *DurableHelper) {
				require.Equal(t, jetstream.AckExplicitPolicy, helper.config.AckPolicy)
				require.Equal(t, DefaultAckWait, helper.config.AckWait)
				require.Equal(t, DefaultMaxDeliver, helper.config.MaxDeliver)
				require.Equal(t, DefaultInactiveThreshold, helper.config.InactiveThreshold)
				require.Equal(t, DefaultBatchSize, helper.config.BatchSize)
				require.Equal(t, DefaultMaxWaiting, helper.config.MaxWaiting)
				require.Equal(t, DefaultFetchTimeout, helper.config.FetchTimeout)
				require.Equal(t, DefaultMaxRetries, helper.config.MaxRetries)
				require.Equal(t, DefaultRetryBackoff, helper.config.RetryBackoff)
				require.NotNil(t, helper.config.Logger) // Should get NopLogger
			},
		},
		{
			name: "partial configuration",
			config: DurableConfig{
				StreamName:      "test-stream",
				ConsumerPrefix:  "test",
				SubjectTemplate: "work.{{.PartitionID}}",
				BatchSize:       50,
				MaxRetries:      5,
				AckWait:         1 * time.Minute,
			},
			assert: func(t *testing.T, helper *DurableHelper) {
				// Custom values preserved
				require.Equal(t, 50, helper.config.BatchSize)
				require.Equal(t, 5, helper.config.MaxRetries)
				require.Equal(t, 1*time.Minute, helper.config.AckWait)
				// Defaults applied to unset fields
				require.Equal(t, jetstream.AckExplicitPolicy, helper.config.AckPolicy)
				require.Equal(t, DefaultMaxDeliver, helper.config.MaxDeliver)
				require.Equal(t, DefaultInactiveThreshold, helper.config.InactiveThreshold)
				require.Equal(t, DefaultMaxWaiting, helper.config.MaxWaiting)
				require.Equal(t, DefaultFetchTimeout, helper.config.FetchTimeout)
				require.Equal(t, DefaultRetryBackoff, helper.config.RetryBackoff)
			},
		},
		{
			name: "custom logger preserved",
			config: DurableConfig{
				StreamName:      "test-stream",
				ConsumerPrefix:  "test",
				SubjectTemplate: "work.{{.PartitionID}}",
				Logger:          partitesting.NewTestLogger(t),
			},
			assert: func(t *testing.T, helper *DurableHelper) {
				// Verify logger is set (can't check type since testLogger is unexported)
				require.NotNil(t, helper.config.Logger)
				require.NotNil(t, helper.logger)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helper, err := NewDurableHelper(nc, tt.config)
			require.NoError(t, err)
			require.NotNil(t, helper)
			tt.assert(t, helper)
		})
	}
}

func TestDurableHelper_GeneratePartitionID(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)

	helper, err := NewDurableHelper(nc, DurableConfig{
		StreamName:      "test-stream",
		ConsumerPrefix:  "test",
		SubjectTemplate: "work.{{.PartitionID}}",
	})
	require.NoError(t, err)

	tests := []struct {
		name      string
		partition types.Partition
		expected  string
	}{
		{
			name:      "single key",
			partition: types.Partition{Keys: []string{"tool_001"}},
			expected:  "tool_001",
		},
		{
			name:      "multiple keys",
			partition: types.Partition{Keys: []string{"tool_001", "region", "us"}},
			expected:  "tool_001-region-us",
		},
		{
			name:      "empty keys",
			partition: types.Partition{Keys: []string{}},
			expected:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := helper.generatePartitionID(tt.partition)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestDurableHelper_SanitizeConsumerName(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)

	helper, err := NewDurableHelper(nc, DurableConfig{
		StreamName:      "test-stream",
		ConsumerPrefix:  "test",
		SubjectTemplate: "work.{{.PartitionID}}",
	})
	require.NoError(t, err)

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "valid name",
			input:    "worker-tool_001-region-us",
			expected: "worker-tool_001-region-us",
		},
		{
			name:     "whitespace",
			input:    "worker tool 001",
			expected: "worker_tool_001",
		},
		{
			name:     "dots",
			input:    "worker.tool.001",
			expected: "worker_tool_001",
		},
		{
			name:     "asterisk",
			input:    "worker*001",
			expected: "worker_001",
		},
		{
			name:     "greater than",
			input:    "worker>001",
			expected: "worker_001",
		},
		{
			name:     "forward slash",
			input:    "worker/001",
			expected: "worker_001",
		},
		{
			name:     "backward slash",
			input:    "worker\\001",
			expected: "worker_001",
		},
		{
			name:     "tabs and newlines",
			input:    "worker\t001\n002",
			expected: "worker_001_002",
		},
		{
			name:     "mixed invalid chars",
			input:    "worker.tool*001>region/us",
			expected: "worker_tool_001_region_us",
		},
		{
			name:     "non-printable characters",
			input:    "worker\x00\x01\x1f\x7f001",
			expected: "worker____001",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := helper.sanitizeConsumerName(tt.input)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestDurableHelper_GenerateSubject(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)

	tests := []struct {
		name        string
		template    string
		partition   types.Partition
		expected    string
		expectError bool
	}{
		{
			name:        "simple template",
			template:    "work.{{.PartitionID}}",
			partition:   types.Partition{Keys: []string{"tool_001"}},
			expected:    "work.tool_001",
			expectError: false,
		},
		{
			name:        "multiple keys",
			template:    "dc.{{.PartitionID}}.complete",
			partition:   types.Partition{Keys: []string{"tool_001", "region", "us"}},
			expected:    "dc.tool_001.region.us.complete",
			expectError: false,
		},
		{
			name:        "complex template",
			template:    "stream.{{.PartitionID}}.v1.data",
			partition:   types.Partition{Keys: []string{"partition", "123"}},
			expected:    "stream.partition.123.v1.data",
			expectError: false,
		},
		{
			name:        "empty keys",
			template:    "work.{{.PartitionID}}",
			partition:   types.Partition{Keys: []string{}},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helper, err := NewDurableHelper(nc, DurableConfig{
				StreamName:      "test-stream",
				ConsumerPrefix:  "test",
				SubjectTemplate: tt.template,
			})
			require.NoError(t, err)

			result, err := helper.generateSubject(tt.partition)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestDurableHelper_UpdateSubscriptions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)

	ctx := context.Background()

	// Create stream
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "work-stream",
		Subjects: []string{"work.>"},
	})
	require.NoError(t, err)

	// Create helper
	helper, err := NewDurableHelper(nc, DurableConfig{
		StreamName:        "work-stream",
		ConsumerPrefix:    "worker",
		SubjectTemplate:   "work.{{.PartitionID}}",
		AckPolicy:         jetstream.AckExplicitPolicy,
		AckWait:           5 * time.Second,
		MaxDeliver:        3,
		InactiveThreshold: 1 * time.Hour,
		BatchSize:         5,
		FetchTimeout:      1 * time.Second,
		MaxRetries:        3,
		RetryBackoff:      100 * time.Millisecond,
	})
	require.NoError(t, err)
	defer helper.Close(ctx)

	// Message counter
	var msgCount atomic.Int32

	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		msgCount.Add(1)
		return nil
	})

	// Add partitions
	added := []types.Partition{
		{Keys: []string{"tool_001"}},
		{Keys: []string{"tool_002"}},
	}

	err = helper.UpdateSubscriptions(ctx, added, nil, handler)
	require.NoError(t, err)

	// Verify consumers created
	activePartitions := helper.ActivePartitions()
	require.Len(t, activePartitions, 2)
	require.Contains(t, activePartitions, "tool_001")
	require.Contains(t, activePartitions, "tool_002")

	// Publish messages
	_, err = js.Publish(ctx, "work.tool_001", []byte("message 1"))
	require.NoError(t, err)

	_, err = js.Publish(ctx, "work.tool_002", []byte("message 2"))
	require.NoError(t, err)

	// Wait for messages to be processed
	require.Eventually(t, func() bool {
		return msgCount.Load() == 2
	}, 5*time.Second, 100*time.Millisecond)

	// Remove one partition
	removed := []types.Partition{
		{Keys: []string{"tool_001"}},
	}

	err = helper.UpdateSubscriptions(ctx, nil, removed, handler)
	require.NoError(t, err)

	// Verify consumer removed from tracking
	activePartitions = helper.ActivePartitions()
	require.Len(t, activePartitions, 1)
	require.Contains(t, activePartitions, "tool_002")
	require.NotContains(t, activePartitions, "tool_001")

	// Publish more messages
	_, err = js.Publish(ctx, "work.tool_002", []byte("message 3"))
	require.NoError(t, err)

	// Wait for message to be processed
	require.Eventually(t, func() bool {
		return msgCount.Load() == 3
	}, 5*time.Second, 100*time.Millisecond)
}

func TestDurableHelper_MessageHandling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)

	ctx := context.Background()

	// Create stream
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "test-stream",
		Subjects: []string{"test.>"},
	})
	require.NoError(t, err)

	// Create helper
	helper, err := NewDurableHelper(nc, DurableConfig{
		StreamName:        "test-stream",
		ConsumerPrefix:    "test",
		SubjectTemplate:   "test.{{.PartitionID}}",
		AckPolicy:         jetstream.AckExplicitPolicy,
		AckWait:           2 * time.Second,
		MaxDeliver:        3,
		InactiveThreshold: 1 * time.Hour,
		BatchSize:         10,
		FetchTimeout:      1 * time.Second,
		MaxRetries:        3,
		RetryBackoff:      100 * time.Millisecond,
	})
	require.NoError(t, err)
	defer helper.Close(ctx)

	// Track processed messages
	var mu sync.Mutex
	processed := make(map[string]int)

	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		data := string(msg.Data())

		mu.Lock()
		processed[data]++
		count := processed[data]
		mu.Unlock()

		// Fail first attempt, succeed on retry
		if count == 1 {
			return errors.New("simulated error")
		}

		return nil
	})

	// Add partition
	added := []types.Partition{
		{Keys: []string{"partition", "1"}},
	}

	err = helper.UpdateSubscriptions(ctx, added, nil, handler)
	require.NoError(t, err)

	// Publish message
	_, err = js.Publish(ctx, "test.partition.1", []byte("test-message"))
	require.NoError(t, err)

	// Wait for message to be processed (should be retried once)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return processed["test-message"] == 2
	}, 10*time.Second, 100*time.Millisecond)
}

func TestDurableHelper_ConsumerInfo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)

	ctx := context.Background()

	// Create stream
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "info-stream",
		Subjects: []string{"info.>"},
	})
	require.NoError(t, err)

	// Create helper
	helper, err := NewDurableHelper(nc, DurableConfig{
		StreamName:        "info-stream",
		ConsumerPrefix:    "info",
		SubjectTemplate:   "info.{{.PartitionID}}",
		AckPolicy:         jetstream.AckExplicitPolicy,
		AckWait:           5 * time.Second,
		MaxDeliver:        3,
		InactiveThreshold: 1 * time.Hour,
		BatchSize:         5,
		FetchTimeout:      1 * time.Second,
		MaxRetries:        3,
		RetryBackoff:      100 * time.Millisecond,
	})
	require.NoError(t, err)
	defer helper.Close(ctx)

	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		return nil
	})

	// Add partition
	added := []types.Partition{
		{Keys: []string{"test"}},
	}

	err = helper.UpdateSubscriptions(ctx, added, nil, handler)
	require.NoError(t, err)

	// Get consumer info
	info, err := helper.ConsumerInfo("test")
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, "info-test", info.Name)
	require.Equal(t, "info.test", info.Config.FilterSubject)

	// Try to get info for non-existent partition
	_, err = helper.ConsumerInfo("nonexistent")
	require.Error(t, err)
}

func TestDurableHelper_Close(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)

	ctx := context.Background()

	// Create stream
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "close-stream",
		Subjects: []string{"close.>"},
	})
	require.NoError(t, err)

	// Create helper
	helper, err := NewDurableHelper(nc, DurableConfig{
		StreamName:        "close-stream",
		ConsumerPrefix:    "close",
		SubjectTemplate:   "close.{{.PartitionID}}",
		AckPolicy:         jetstream.AckExplicitPolicy,
		AckWait:           5 * time.Second,
		MaxDeliver:        3,
		InactiveThreshold: 1 * time.Hour,
		BatchSize:         5,
		FetchTimeout:      1 * time.Second,
		MaxRetries:        3,
		RetryBackoff:      100 * time.Millisecond,
	})
	require.NoError(t, err)

	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		return nil
	})

	// Add partitions
	added := []types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p2"}},
	}

	err = helper.UpdateSubscriptions(ctx, added, nil, handler)
	require.NoError(t, err)

	// Verify active
	require.Len(t, helper.ActivePartitions(), 2)

	// Close helper
	err = helper.Close(ctx)
	require.NoError(t, err)

	// Verify all cleaned up
	require.Empty(t, helper.ActivePartitions())
}

func TestDurableHelper_LoggerIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)
	ctx := context.Background()

	// Create test stream
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "test-logger-stream"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"test.logger.>"},
	})
	require.NoError(t, err)

	// Create helper with test logger
	testLogger := partitesting.NewTestLogger(t)
	helper, err := NewDurableHelper(nc, DurableConfig{
		StreamName:        streamName,
		ConsumerPrefix:    "logger-test",
		SubjectTemplate:   "test.logger.{{.PartitionID}}",
		AckPolicy:         jetstream.AckExplicitPolicy,
		AckWait:           30 * time.Second,
		MaxDeliver:        3,
		InactiveThreshold: 1 * time.Hour,
		BatchSize:         10,
		FetchTimeout:      2 * time.Second,
		MaxRetries:        2,
		RetryBackoff:      100 * time.Millisecond,
		Logger:            testLogger, // Use test logger to see log output
	})
	require.NoError(t, err)
	require.NotNil(t, helper)

	defer func() {
		err := helper.Close(ctx)
		require.NoError(t, err)
	}()

	// Message handler
	handler := MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		return msg.Ack()
	})

	// Add a partition - this should trigger logging
	added := []types.Partition{
		{Keys: []string{"partition1"}},
	}

	t.Log("Adding partition - should see INFO logs")
	err = helper.UpdateSubscriptions(ctx, added, nil, handler)
	require.NoError(t, err)
	require.Len(t, helper.ActivePartitions(), 1)

	// Add same partition again - should see DEBUG log about skipping
	t.Log("Adding same partition again - should see DEBUG log about skipping")
	err = helper.UpdateSubscriptions(ctx, added, nil, handler)
	require.NoError(t, err)

	// Remove partition - should see DEBUG log
	t.Log("Removing partition - should see DEBUG logs")
	err = helper.UpdateSubscriptions(ctx, nil, added, handler)
	require.NoError(t, err)
	require.Empty(t, helper.ActivePartitions())

	t.Log("Closing helper - should see INFO logs")
}
