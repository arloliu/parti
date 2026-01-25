package partition_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/parti/partition"
	partitesting "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestPartitionPublishConsume(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "events"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"events.*.completed.*"},
	})
	require.NoError(t, err)

	pub, err := partition.NewPublisher(nc, partition.PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.{{key}}.completed.{{partition}}",
	})
	require.NoError(t, err)

	key := findKeyForPartition(pub, 0)
	require.NotEmpty(t, key)

	msgCh := make(chan string, 1)
	consumer, err := partition.NewJSConsumer(js, partition.ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  4,
			SubjectPattern: "events.{{key}}.completed.{{partition}}",
		},
		StreamName:   streamName,
		ConsumerName: "consumer-0",
		Partition:    0,
	}, partition.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		msgCh <- string(msg.Data())
		return nil
	}))
	require.NoError(t, err)

	require.NoError(t, consumer.Start(ctx))

	err = pub.Publish(ctx, key, []byte("hello"))
	require.NoError(t, err)

	select {
	case got := <-msgCh:
		require.Equal(t, "hello", got)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPartitionPublishSubscribe(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)

	pub, err := partition.NewPublisher(nc, partition.PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.{{key}}.completed.{{partition}}",
	})
	require.NoError(t, err)

	key := findKeyForPartition(pub, 2)
	require.NotEmpty(t, key)

	msgCh := make(chan string, 1)
	sub, err := partition.NewSubscriber(
		nc,
		partition.PartitionConfig{
			NumPartitions:  4,
			SubjectPattern: "events.{{key}}.completed.{{partition}}",
		},
		2,
		partition.NATSMessageHandlerFunc(func(_ context.Context, msg *nats.Msg) error {
			msgCh <- string(msg.Data)
			return nil
		}),
	)
	require.NoError(t, err)

	require.NoError(t, sub.Start(ctx))

	err = pub.Publish(ctx, key, []byte("hello-nats"))
	require.NoError(t, err)

	select {
	case got := <-msgCh:
		require.Equal(t, "hello-nats", got)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestJSPublisherPublishConsume(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "events-js"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"events.*.completed.*"},
	})
	require.NoError(t, err)

	pub, err := partition.NewJSPublisher(js, partition.PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.{{key}}.completed.{{partition}}",
	})
	require.NoError(t, err)

	key := findKeyForPartitionPublisher(pub, 1)
	require.NotEmpty(t, key)

	msgCh := make(chan string, 1)
	consumer, err := partition.NewJSConsumer(js, partition.ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  4,
			SubjectPattern: "events.{{key}}.completed.{{partition}}",
		},
		StreamName:   streamName,
		ConsumerName: "consumer-1",
		Partition:    1,
	}, partition.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		msgCh <- string(msg.Data())
		return nil
	}))
	require.NoError(t, err)

	require.NoError(t, consumer.Start(ctx))

	_, err = pub.Publish(ctx, key, []byte("hello-js"))
	require.NoError(t, err)

	select {
	case got := <-msgCh:
		require.Equal(t, "hello-js", got)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPartitionConsistency(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)

	pub, err := partition.NewPublisher(nc, partition.PartitionConfig{
		NumPartitions:  8,
		SubjectPattern: "events.{{partition}}",
	})
	require.NoError(t, err)

	// Same key should always map to same partition
	keys := []string{"user-123", "order-456", "product-789", "session-abc"}
	for _, key := range keys {
		first := pub.GetPartition(key)
		for range 100 {
			require.Equal(t, first, pub.GetPartition(key), "partition should be consistent for key %s", key)
		}
	}
}

func TestHashSeedAffectsDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)

	pub1, err := partition.NewPublisher(nc, partition.PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.{{partition}}",
		HashSeed:       0,
	})
	require.NoError(t, err)

	pub2, err := partition.NewPublisher(nc, partition.PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.{{partition}}",
		HashSeed:       12345,
	})
	require.NoError(t, err)

	// Different seeds should produce different partitions for at least some keys
	different := false
	for i := range 100 {
		key := fmt.Sprintf("key-%d", i)
		if pub1.GetPartition(key) != pub2.GetPartition(key) {
			different = true
			break
		}
	}
	require.True(t, different, "different hash seeds should produce different distributions")
}

func TestMultiplePartitionConsumers(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "multi-partition"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"multi.*"},
	})
	require.NoError(t, err)

	pub, err := partition.NewJSPublisher(js, partition.PartitionConfig{
		NumPartitions:  3,
		SubjectPattern: "multi.{{partition}}",
	})
	require.NoError(t, err)

	// Create consumers for all 3 partitions
	results := make([]chan string, 3)
	consumers := make([]*partition.JSConsumer, 3)
	for i := range 3 {
		results[i] = make(chan string, 10)
		idx := i
		consumers[i], err = partition.NewJSConsumer(js, partition.ConsumerConfig{
			PartitionConfig: partition.PartitionConfig{
				NumPartitions:  3,
				SubjectPattern: "multi.{{partition}}",
			},
			StreamName:   streamName,
			ConsumerName: fmt.Sprintf("consumer-%d", i),
			Partition:    i,
		}, partition.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
			results[idx] <- string(msg.Data())
			return nil
		}))
		require.NoError(t, err)
		require.NoError(t, consumers[i].Start(ctx))
	}

	// Find keys that map to each partition
	keysForPartition := make([]string, 3)
	for i := range 3 {
		for j := range 1000 {
			key := fmt.Sprintf("key-%d", j)
			if pub.GetPartition(key) == i {
				keysForPartition[i] = key
				break
			}
		}
		require.NotEmpty(t, keysForPartition[i], "should find key for partition %d", i)
	}

	// Publish to each partition
	for i, key := range keysForPartition {
		_, err = pub.Publish(ctx, key, []byte(fmt.Sprintf("msg-%d", i)))
		require.NoError(t, err)
	}

	// Verify each consumer receives its message
	for i := range 3 {
		select {
		case got := <-results[i]:
			require.Equal(t, fmt.Sprintf("msg-%d", i), got)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for message on partition %d", i)
		}
	}
}

func TestJSPublisherAsync(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "async-test"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"async.*"},
	})
	require.NoError(t, err)

	pub, err := partition.NewJSPublisher(js, partition.PartitionConfig{
		NumPartitions:  2,
		SubjectPattern: "async.{{partition}}",
	})
	require.NoError(t, err)

	key := findKeyForPartitionPublisher(pub, 0)

	// Publish async
	future, err := pub.PublishAsync(key, []byte("async-msg"))
	require.NoError(t, err)
	require.NotNil(t, future)

	// Wait for ack
	select {
	case ack := <-future.Ok():
		require.NotNil(t, ack)
		require.Equal(t, streamName, ack.Stream)
	case err := <-future.Err():
		t.Fatalf("async publish failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for async ack")
	}
}

func TestConsumerManualAck(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "manual-ack"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"manual.*"},
	})
	require.NoError(t, err)

	pub, err := partition.NewJSPublisher(js, partition.PartitionConfig{
		NumPartitions:  1,
		SubjectPattern: "manual.{{partition}}",
	})
	require.NoError(t, err)

	acked := make(chan struct{}, 1)
	consumer, err := partition.NewJSConsumer(js, partition.ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  1,
			SubjectPattern: "manual.{{partition}}",
		},
		StreamName:   streamName,
		ConsumerName: "manual-consumer",
		Partition:    0,
		ManualAck:    true,
	}, partition.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		// Manually ack
		if err := msg.Ack(); err != nil {
			return err
		}
		acked <- struct{}{}
		return nil
	}))
	require.NoError(t, err)
	require.NoError(t, consumer.Start(ctx))

	_, err = pub.Publish(ctx, "any-key", []byte("manual-msg"))
	require.NoError(t, err)

	select {
	case <-acked:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for manual ack")
	}
}

func TestPatternWithoutKey(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)

	pub, err := partition.NewPublisher(nc, partition.PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "nokey.{{partition}}.events",
	})
	require.NoError(t, err)

	// Verify subject generation - GetSubjectForPartition returns subject for specific partition
	subj, err := pub.GetSubjectForPartition(2)
	require.NoError(t, err)
	require.Equal(t, "nokey.2.events", subj)

	// GetSubject returns subject based on key's partition
	key := findKeyForPartition(pub, 0)
	require.Equal(t, "nokey.0.events", pub.GetSubject(key))

	// Subscriber for partition 0
	msgCh := make(chan string, 1)

	sub, err := partition.NewSubscriber(nc, partition.PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "nokey.{{partition}}.events",
	}, 0, partition.NATSMessageHandlerFunc(func(_ context.Context, msg *nats.Msg) error {
		msgCh <- string(msg.Data)
		return nil
	}))
	require.NoError(t, err)
	require.NoError(t, sub.Start(ctx))

	require.NoError(t, pub.Publish(ctx, key, []byte("nokey-msg")))

	select {
	case got := <-msgCh:
		require.Equal(t, "nokey-msg", got)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPublisherGetMethods(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)

	pub, err := partition.NewPublisher(nc, partition.PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "test.{{key}}.{{partition}}",
	})
	require.NoError(t, err)

	require.Equal(t, 4, pub.NumPartitions())

	key := "user-123"
	partition := pub.GetPartition(key)
	require.GreaterOrEqual(t, partition, 0)
	require.Less(t, partition, 4)

	subject := pub.GetSubject(key)
	require.Contains(t, subject, key)
	require.Contains(t, subject, fmt.Sprintf("%d", partition))

	// GetSubjectForPartition
	for i := range 4 {
		subj, err := pub.GetSubjectForPartition(i)
		require.NoError(t, err)
		require.Contains(t, subj, fmt.Sprintf("%d", i))
		require.Contains(t, subj, "*") // key becomes wildcard
	}

	// Out of range
	_, err = pub.GetSubjectForPartition(10)
	require.Error(t, err)
}

func TestSubscriberSubjectAndPartition(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)

	sub, err := partition.NewSubscriber(nc, partition.PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "sub.{{key}}.{{partition}}",
	}, 2, partition.NATSMessageHandlerFunc(func(context.Context, *nats.Msg) error { return nil }))
	require.NoError(t, err)

	require.Equal(t, 2, sub.Partition())
	require.Equal(t, "sub.*.2", sub.Subject())
}

func findKeyForPartition(pub *partition.Publisher, target int) string {
	for i := range 1000 {
		key := fmt.Sprintf("tool-%d", i)
		if pub.GetPartition(key) == target {
			return key
		}
	}

	return ""
}

func findKeyForPartitionPublisher(pub *partition.JSPublisher, target int) string {
	for i := range 1000 {
		key := fmt.Sprintf("tool-%d", i)
		if pub.GetPartition(key) == target {
			return key
		}
	}

	return ""
}
