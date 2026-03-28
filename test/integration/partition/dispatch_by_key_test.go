package partition_test

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/ipartition"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/partition"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDispatchByKey_EndToEnd tests the full lifecycle of a JSConsumer with DispatchByKey enabled,
// from start to graceful stop, ensuring messages are processed correctly.
func TestDispatchByKey_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "dispatch-test"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"events.*.>"},
	})
	require.NoError(t, err)

	// Create publisher
	pub, err := partition.NewPublisher(nc, partition.PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "events.{{partition}}.{{key}}",
	})
	require.NoError(t, err)

	// Track processed messages
	var mu sync.Mutex
	processed := make(map[string][]string) // key -> list of data

	dispatchEnabled := true
	consumer, err := ipartition.NewJSConsumer(js, ipartition.ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  4,
			SubjectPattern: "events.{{partition}}.{{key}}",
		},
		StreamName:       streamName,
		ConsumerName:     "consumer-0",
		Partition:        0,
		DispatchByKey:    &dispatchEnabled,
		KeyChannelBuffer: 16,
		KeyIdleTimeout:   100 * time.Millisecond,
	}, func(_ context.Context, msg jetstream.Msg) error {
		// Extract key from subject (last token)
		subj := msg.Subject()
		var key string
		for i := len(subj) - 1; i >= 0; i-- {
			if subj[i] == '.' {
				key = subj[i+1:]
				break
			}
		}

		mu.Lock()
		processed[key] = append(processed[key], string(msg.Data()))
		mu.Unlock()

		return nil
	})
	require.NoError(t, err)

	// Start consumer
	require.NoError(t, consumer.Start(ctx))

	// Publish messages for different keys that hash to partition 0
	keys := findKeysForPartition(pub, 0, 3)
	require.Len(t, keys, 3, "need 3 keys for partition 0")

	for i, key := range keys {
		for j := range 5 {
			data := []byte(key + "-msg-" + string(rune('0'+j)))
			err := pub.Publish(ctx, key, data)
			require.NoError(t, err, "publish failed for key %d msg %d", i, j)
		}
	}

	// Wait for processing
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		total := 0
		for _, msgs := range processed {
			total += len(msgs)
		}
		return total >= 15
	}, 5*time.Second, 50*time.Millisecond, "should process all 15 messages")

	// Verify each key received its messages
	mu.Lock()
	for _, key := range keys {
		assert.Len(t, processed[key], 5, "key %s should have 5 messages", key)
	}
	mu.Unlock()

	// Stop consumer gracefully
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	require.NoError(t, consumer.Stop(stopCtx))
}

// TestDispatchByKey_GracefulStop ensures that stopping a consumer with DispatchByKey
// waits for all in-flight messages to be processed.
func TestDispatchByKey_GracefulStop(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "graceful-stop-test"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"graceful.*.>"},
	})
	require.NoError(t, err)

	pub, err := partition.NewPublisher(nc, partition.PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "graceful.{{partition}}.{{key}}",
	})
	require.NoError(t, err)

	var processed atomic.Int32
	handlerStarted := make(chan struct{}, 100)

	dispatchEnabled := true
	consumer, err := ipartition.NewJSConsumer(js, ipartition.ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  4,
			SubjectPattern: "graceful.{{partition}}.{{key}}",
		},
		StreamName:       streamName,
		ConsumerName:     "consumer-0",
		Partition:        0,
		DispatchByKey:    &dispatchEnabled,
		KeyChannelBuffer: 32,
		KeyIdleTimeout:   5 * time.Second,
	}, func(_ context.Context, msg jetstream.Msg) error {
		handlerStarted <- struct{}{}
		time.Sleep(50 * time.Millisecond) // Simulate processing time
		processed.Add(1)

		return nil
	})
	require.NoError(t, err)

	require.NoError(t, consumer.Start(ctx))

	// Publish messages
	keys := findKeysForPartition(pub, 0, 5)
	require.Len(t, keys, 5)

	for _, key := range keys {
		err := pub.Publish(ctx, key, []byte("data"))
		require.NoError(t, err)
	}

	// Wait for at least some handlers to start
	for range 3 {
		select {
		case <-handlerStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("handlers not starting")
		}
	}

	// Stop while handlers are still processing
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = consumer.Stop(stopCtx)
	require.NoError(t, err)

	// All started handlers should complete
	assert.GreaterOrEqual(t, processed.Load(), int32(3), "at least 3 messages should be processed")
}

// TestDispatchByKey_NoGoroutineLeak verifies that starting and stopping a consumer
// with DispatchByKey does not leak goroutines.
func TestDispatchByKey_NoGoroutineLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "leak-test"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"leak.*.>"},
	})
	require.NoError(t, err)

	pub, err := partition.NewPublisher(nc, partition.PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "leak.{{partition}}.{{key}}",
	})
	require.NoError(t, err)

	// Baseline goroutine count
	//nolint:revive // Explicit GC to flush finalizers for goroutine count check
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baselineGoroutines := runtime.NumGoroutine()

	// Run multiple consumer lifecycles
	for cycle := range 3 {
		dispatchEnabled := true
		consumer, err := ipartition.NewJSConsumer(js, ipartition.ConsumerConfig{
			PartitionConfig: partition.PartitionConfig{
				NumPartitions:  4,
				SubjectPattern: "leak.{{partition}}.{{key}}",
			},
			StreamName:       streamName,
			ConsumerName:     "consumer-0",
			Partition:        0,
			DispatchByKey:    &dispatchEnabled,
			KeyChannelBuffer: 8,
			KeyIdleTimeout:   50 * time.Millisecond,
		}, func(_ context.Context, msg jetstream.Msg) error {
			return nil
		})
		require.NoError(t, err)

		require.NoError(t, consumer.Start(ctx))

		// Publish messages to create key workers
		keys := findKeysForPartition(pub, 0, 10)
		for _, key := range keys {
			_ = pub.Publish(ctx, key, []byte("data"))
		}

		// Let messages process and idle timeout trigger
		time.Sleep(200 * time.Millisecond)

		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = consumer.Stop(stopCtx)
		cancel()
		require.NoError(t, err, "cycle %d: stop failed", cycle)
	}

	// Wait for cleanup
	runtime.GC() //nolint:revive // verify cleanup
	time.Sleep(500 * time.Millisecond)

	// Check goroutine count
	finalGoroutines := runtime.NumGoroutine()
	leakedGoroutines := finalGoroutines - baselineGoroutines

	// Allow small variance (+-5) for background goroutines
	assert.LessOrEqual(t, leakedGoroutines, 5,
		"goroutine leak detected: baseline=%d, final=%d, leaked=%d",
		baselineGoroutines, finalGoroutines, leakedGoroutines)
}

// TestDispatchByKey_PerKeyOrdering verifies that messages for the same key
// are processed in order even when using concurrent dispatch.
func TestDispatchByKey_PerKeyOrdering(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "ordering-test"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"order.*.>"},
	})
	require.NoError(t, err)

	pub, err := partition.NewPublisher(nc, partition.PartitionConfig{
		NumPartitions:  4,
		SubjectPattern: "order.{{partition}}.{{key}}",
	})
	require.NoError(t, err)

	var mu sync.Mutex
	keyOrder := make(map[string][]int) // key -> order of received sequence numbers

	dispatchEnabled := true
	consumer, err := ipartition.NewJSConsumer(js, ipartition.ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  4,
			SubjectPattern: "order.{{partition}}.{{key}}",
		},
		StreamName:       streamName,
		ConsumerName:     "consumer-0",
		Partition:        0,
		DispatchByKey:    &dispatchEnabled,
		KeyChannelBuffer: 64,
		KeyIdleTimeout:   1 * time.Second,
	}, func(_ context.Context, msg jetstream.Msg) error {
		// Extract key and sequence from data
		data := string(msg.Data())
		key, seq := parseKeySeq(data)

		// Simulate variable processing time
		time.Sleep(time.Duration(seq%3) * time.Millisecond)

		mu.Lock()
		keyOrder[key] = append(keyOrder[key], seq)
		mu.Unlock()

		return nil
	})
	require.NoError(t, err)

	require.NoError(t, consumer.Start(ctx))

	// Publish messages with sequence numbers
	keys := findKeysForPartition(pub, 0, 5)
	require.Len(t, keys, 5)

	messagesPerKey := 20
	for _, key := range keys {
		for seq := range messagesPerKey {
			data := []byte(key + ":" + string(rune('0'+seq/10)) + string(rune('0'+seq%10)))
			err := pub.Publish(ctx, key, data)
			require.NoError(t, err)
		}
	}

	// Wait for all messages
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		total := 0
		for _, seqs := range keyOrder {
			total += len(seqs)
		}
		return total >= len(keys)*messagesPerKey
	}, 10*time.Second, 100*time.Millisecond)

	// Verify ordering per key
	mu.Lock()
	defer mu.Unlock()
	for key, seqs := range keyOrder {
		for i := 1; i < len(seqs); i++ {
			assert.Less(t, seqs[i-1], seqs[i],
				"key %s: messages out of order at index %d: %v", key, i, seqs)
		}
	}

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	require.NoError(t, consumer.Stop(stopCtx))
}

// findKeysForPartition finds n keys that hash to the specified partition.
//
//nolint:unparam // targetPartition kept for API clarity and potential future use
func findKeysForPartition(pub *partition.Publisher, targetPartition, n int) []string {
	var keys []string
	for i := 0; len(keys) < n && i < 10000; i++ {
		key := "key-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		if pub.GetPartition(key) == targetPartition {
			keys = append(keys, key)
		}
	}

	return keys
}

// parseKeySeq parses "key:seq" format from data.
func parseKeySeq(data string) (string, int) {
	for i, c := range data {
		if c == ':' {
			key := data[:i]
			s := data[i+1:]
			seq := 0
			for _, d := range s {
				if d >= '0' && d <= '9' {
					seq = seq*10 + int(d-'0')
				}
			}

			return key, seq
		}
	}

	return data, 0
}
