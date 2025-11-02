package subscription

import (
	"context"
	"sync"
	"testing"
	"time"

	partitesting "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestDurableHelper_ConcurrentConsumerCreation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)

	ctx := context.Background()

	// Create stream
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "race-stream",
		Subjects: []string{"race.>"},
	})
	require.NoError(t, err)

	// Create multiple helpers simulating multiple workers
	numWorkers := 5
	helpers := make([]*DurableHelper, numWorkers)

	for i := range numWorkers {
		helper, err := NewDurableHelper(nc, DurableConfig{
			StreamName:        "race-stream",
			ConsumerPrefix:    "race",
			SubjectTemplate:   "race.{{.PartitionID}}",
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
		helpers[i] = helper
	}

	// Cleanup all helpers at the end
	t.Cleanup(func() {
		for _, helper := range helpers {
			if helper != nil {
				_ = helper.Close(ctx)
			}
		}
	})

	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		return nil
	})

	// Simulate race: all workers try to create consumer for same partition simultaneously
	partition := []types.Partition{
		{Keys: []string{"shared"}},
	}

	var wg sync.WaitGroup
	errors := make([]error, numWorkers)

	for i := range numWorkers {
		wg.Go(func() {
			errors[i] = helpers[i].UpdateSubscriptions(ctx, partition, nil, handler)
		})
	}

	wg.Wait()

	// All workers should succeed (no errors from race condition)
	for i, err := range errors {
		require.NoError(t, err, "worker %d failed", i)
	}

	// Verify all workers have the same consumer
	for i := range numWorkers {
		activePartitions := helpers[i].ActivePartitions()
		require.Len(t, activePartitions, 1)
		require.Contains(t, activePartitions, "shared")

		info, err := helpers[i].ConsumerInfo("shared")
		require.NoError(t, err)
		require.Equal(t, "race-shared", info.Name, "worker %d has wrong consumer name", i)
	}

	// Verify only one consumer was created in NATS
	stream, err := js.Stream(ctx, "race-stream")
	require.NoError(t, err)

	consumers := stream.ListConsumers(ctx)
	consumerCount := 0

	for range consumers.Info() {
		consumerCount++
	}

	require.Equal(t, 1, consumerCount, "should have exactly one consumer despite concurrent creation attempts")
}

func TestDurableHelper_ExistingConsumerReuse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)

	ctx := context.Background()

	// Create stream
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "reuse-stream",
		Subjects: []string{"reuse.>"},
	})
	require.NoError(t, err)

	// Create helper 1 and add partition
	helper1, err := NewDurableHelper(nc, DurableConfig{
		StreamName:        "reuse-stream",
		ConsumerPrefix:    "reuse",
		SubjectTemplate:   "reuse.{{.PartitionID}}",
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
	defer helper1.Close(ctx)

	handler := MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		return nil
	})

	partition := []types.Partition{
		{Keys: []string{"test"}},
	}

	// First helper creates consumer
	err = helper1.UpdateSubscriptions(ctx, partition, nil, handler)
	require.NoError(t, err)

	info1, err := helper1.ConsumerInfo("test")
	require.NoError(t, err)
	createdTime1 := info1.Created

	// Create helper 2 and add same partition
	helper2, err := NewDurableHelper(nc, DurableConfig{
		StreamName:        "reuse-stream",
		ConsumerPrefix:    "reuse",
		SubjectTemplate:   "reuse.{{.PartitionID}}",
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
	defer helper2.Close(ctx)

	// Second helper should reuse existing consumer
	err = helper2.UpdateSubscriptions(ctx, partition, nil, handler)
	require.NoError(t, err)

	info2, err := helper2.ConsumerInfo("test")
	require.NoError(t, err)
	createdTime2 := info2.Created

	// Both should use the same consumer (same creation time)
	require.Equal(t, createdTime1, createdTime2, "should reuse existing consumer, not create a new one")
	require.Equal(t, info1.Name, info2.Name, "consumer names should match")
}
