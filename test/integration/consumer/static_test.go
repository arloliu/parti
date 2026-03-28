package consumer_test

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/consumer"
	"github.com/arloliu/parti/partition"
	partitesting "github.com/arloliu/parti/testing"
)

// TestStatic_BasicPublishConsume verifies that a Static consumer receives messages
// published to its specific partition subject.
func TestStatic_BasicPublishConsume(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "ST_BASIC"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"st.*.completed.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	subjectPattern := "st.{{key}}.completed.{{partition}}"
	numPartitions := 4

	// Create publisher to find a key that maps to partition 0
	pub, err := partition.NewPublisher(nc, partition.PartitionConfig{
		NumPartitions:  numPartitions,
		SubjectPattern: subjectPattern,
	})
	require.NoError(t, err)

	key := findKeyForPartition(pub, 0)
	require.NotEmpty(t, key, "should find a key mapping to partition 0")

	msgCh := make(chan string, 1)
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		msgCh <- string(msg.Data())
		return nil
	})

	// Create Static consumer for partition 0
	sc, err := consumer.NewStatic(js, streamName, "st-consumer-0", subjectPattern,
		numPartitions, 0, handler,
	)
	require.NoError(t, err)

	require.NoError(t, sc.Start(ctx))
	t.Cleanup(func() { _ = sc.Stop(ctx) })

	// Verify partition and subject
	require.Equal(t, 0, sc.Partition())
	require.NotEmpty(t, sc.Subject())

	// Publish via the partition publisher
	err = pub.Publish(ctx, key, []byte("hello-static"))
	require.NoError(t, err)

	select {
	case got := <-msgCh:
		require.Equal(t, "hello-static", got)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

// TestStatic_StopPreventsDelivery verifies that after Stop, no more messages are
// delivered to the handler.
func TestStatic_StopPreventsDelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "ST_STOP"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"sts.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var handled atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		handled.Add(1)
		return nil
	})

	sc, err := consumer.NewStatic(js, streamName, "sts-consumer-0",
		"sts.{{partition}}", 2, 0, handler,
	)
	require.NoError(t, err)

	require.NoError(t, sc.Start(ctx))

	// Publish messages
	for range 3 {
		_, err = js.Publish(ctx, "sts.0", []byte("msg"))
		require.NoError(t, err)
	}

	// Wait for messages
	require.Eventually(t, func() bool {
		return handled.Load() >= 3
	}, 5*time.Second, 50*time.Millisecond)

	// Stop
	require.NoError(t, sc.Stop(ctx))

	beforeStop := handled.Load()
	for range 3 {
		_, err = js.Publish(ctx, "sts.0", []byte("after-stop"))
		require.NoError(t, err)
	}
	time.Sleep(500 * time.Millisecond)

	require.Equal(t, beforeStop, handled.Load(), "no messages after stop")
}

// TestStatic_MultiplePartitions verifies that Static consumers for different
// partitions each receive only their own partition's messages.
func TestStatic_MultiplePartitions(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "ST_MULTI"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"stm.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	numPartitions := 3
	counts := make([]atomic.Int32, numPartitions)
	consumers := make([]*consumer.Static, numPartitions)

	for i := range numPartitions {
		idx := i
		handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
			counts[idx].Add(1)
			return nil
		})

		sc, err := consumer.NewStatic(js, streamName,
			fmt.Sprintf("stm-consumer-%d", idx),
			"stm.{{partition}}", numPartitions, idx, handler,
		)
		require.NoError(t, err)

		require.NoError(t, sc.Start(ctx))
		t.Cleanup(func() { _ = sc.Stop(ctx) })
		consumers[idx] = sc
	}

	// Publish 3 messages to each partition
	for i := range numPartitions {
		for range 3 {
			_, err = js.Publish(ctx, fmt.Sprintf("stm.%d", i), []byte("msg"))
			require.NoError(t, err)
		}
	}

	// Wait for all messages
	require.Eventually(t, func() bool {
		for i := range numPartitions {
			if counts[i].Load() < 3 {
				return false
			}
		}
		return true
	}, 10*time.Second, 50*time.Millisecond, "all partitions should receive 3 messages")

	for i := range numPartitions {
		require.Equal(t, int32(3), counts[i].Load(),
			"partition %d should receive exactly 3 messages", i)
	}
}

// TestStatic_PublishBeforeStart verifies that messages published before consumer
// start are delivered once the consumer starts.
func TestStatic_PublishBeforeStart(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "ST_BEFORE"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"stb.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// Publish BEFORE consumer starts
	for i := range 3 {
		_, err = js.Publish(ctx, "stb.0", []byte(fmt.Sprintf("before-%d", i)))
		require.NoError(t, err)
	}

	msgCh := make(chan string, 10)
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		msgCh <- string(msg.Data())
		return nil
	})

	sc, err := consumer.NewStatic(js, streamName, "stb-consumer-0",
		"stb.{{partition}}", 2, 0, handler,
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)

	require.NoError(t, sc.Start(ctx))
	t.Cleanup(func() { _ = sc.Stop(ctx) })

	// Should receive the pre-published messages
	for i := range 3 {
		select {
		case got := <-msgCh:
			require.Equal(t, fmt.Sprintf("before-%d", i), got)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for pre-published message %d", i)
		}
	}
}

// findKeyForPartition finds a key that hashes to the given partition index.
func findKeyForPartition(pub *partition.Publisher, targetPartition int) string {
	for i := range 10000 {
		key := fmt.Sprintf("key-%d", i)
		if pub.GetPartition(key) == targetPartition {
			return key
		}
	}
	return ""
}

// findKeysForPartition finds n distinct keys that hash to the given partition.
func findKeysForPartition(pub *partition.Publisher, targetPartition, n int) []string {
	keys := make([]string, 0, n)
	for i := range 100000 {
		key := fmt.Sprintf("key-%d", i)
		if pub.GetPartition(key) == targetPartition {
			keys = append(keys, key)
			if len(keys) == n {
				return keys
			}
		}
	}

	return keys
}

// TestStatic_WithDispatchByKey verifies that enabling WithDispatchByKey routes
// messages to per-key goroutines, preserving per-key ordering while allowing
// concurrent processing across different keys.
func TestStatic_WithDispatchByKey(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "ST_DBK"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"stdbk.*.>"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	subjectPattern := "stdbk.{{partition}}.{{key}}"
	numPartitions := 4

	pub, err := partition.NewPublisher(nc, partition.PartitionConfig{
		NumPartitions:  numPartitions,
		SubjectPattern: subjectPattern,
	})
	require.NoError(t, err)

	// Find 3 distinct keys that hash to partition 0
	keys := findKeysForPartition(pub, 0, 3)
	require.Len(t, keys, 3, "need 3 keys hashing to partition 0")

	// Track per-key message sequences
	var mu sync.Mutex
	processed := make(map[string][]int) // key -> ordered list of sequence numbers

	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		// Extract key from subject (last token)
		subj := msg.Subject()
		var key string
		for i := len(subj) - 1; i >= 0; i-- {
			if subj[i] == '.' {
				key = subj[i+1:]
				break
			}
		}

		// Parse sequence number from data
		seq, parseErr := strconv.Atoi(string(msg.Data()))
		if parseErr != nil {
			return fmt.Errorf("parse sequence from %q: %w", string(msg.Data()), parseErr)
		}

		mu.Lock()
		processed[key] = append(processed[key], seq)
		mu.Unlock()

		return nil
	})

	sc, err := consumer.NewStatic(js, streamName, "stdbk-consumer-0", subjectPattern,
		numPartitions, 0, handler,
		consumer.WithDispatchByKey(),
		consumer.WithKeyChannelBuffer(16),
		consumer.WithKeyIdleTimeout(200*time.Millisecond),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)

	require.NoError(t, sc.Start(ctx))
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = sc.Stop(stopCtx)
	})

	// Publish 5 messages per key with sequence numbers
	msgsPerKey := 5
	for _, key := range keys {
		for seq := range msgsPerKey {
			err := pub.Publish(ctx, key, []byte(fmt.Sprintf("%d", seq)))
			require.NoError(t, err)
		}
	}

	totalExpected := len(keys) * msgsPerKey
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		total := 0
		for _, msgs := range processed {
			total += len(msgs)
		}
		return total >= totalExpected
	}, 10*time.Second, 50*time.Millisecond, "all messages should be processed")

	// Verify per-key ordering is preserved
	mu.Lock()
	defer mu.Unlock()
	for _, key := range keys {
		seqs := processed[key]
		require.Len(t, seqs, msgsPerKey, "key %s should have %d messages", key, msgsPerKey)
		for i := 1; i < len(seqs); i++ {
			require.Greater(t, seqs[i], seqs[i-1],
				"key %s: sequence should be monotonically increasing, got %v", key, seqs)
		}
	}
}
