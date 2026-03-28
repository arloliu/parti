package partition_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partition"
	ipartition "github.com/arloliu/parti/v2/internal/partition"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestJSConsumer_PublishBeforeAndAfterStart reproduces the reported issue:
// 1. Publish messages BEFORE consumer starts
// 2. Start consumer - should receive initial messages
// 3. Publish more messages AFTER initial batch consumed
// 4. Consumer should receive them too (reported as stuck here)
func TestJSConsumer_PublishBeforeAndAfterStart(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "repro-stuck"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"repro.*"},
	})
	require.NoError(t, err)

	// Step 1: Publish 3 messages BEFORE consumer starts
	for i := 1; i <= 3; i++ {
		_, err = js.Publish(ctx, "repro.0", []byte(fmt.Sprintf("before-%d", i)))
		require.NoError(t, err)
	}
	t.Log("Step 1: Published 3 messages before consumer start")

	// Step 2: Start consumer
	var received atomic.Int32
	msgCh := make(chan string, 20)
	consumer, err := ipartition.NewJSConsumer(js, ipartition.ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "repro.{{partition}}",
		},
		StreamName:   streamName,
		ConsumerName: "test-repro-0",
		Partition:    0,
		FetchTimeout: 2 * time.Second,
	}, func(_ context.Context, msg jetstream.Msg) error {
		n := received.Add(1)
		data := string(msg.Data())
		t.Logf("  Received message #%d: %s", n, data)
		msgCh <- data
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, consumer.Start(ctx))

	// Wait for initial 3 messages
	for i := 1; i <= 3; i++ {
		select {
		case got := <-msgCh:
			require.Equal(t, fmt.Sprintf("before-%d", i), got)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for initial message %d", i)
		}
	}
	t.Log("Step 2: Consumer received all 3 initial messages")

	// Small gap to let the iterator settle
	time.Sleep(500 * time.Millisecond)

	// Step 3: Publish 3 MORE messages after initial batch consumed
	for i := 1; i <= 3; i++ {
		_, err = js.Publish(ctx, "repro.0", []byte(fmt.Sprintf("after-%d", i)))
		require.NoError(t, err)
	}
	t.Log("Step 3: Published 3 more messages after initial batch consumed")

	// Step 4: Consumer should receive them (this is where the reported bug is)
	for i := 1; i <= 3; i++ {
		select {
		case got := <-msgCh:
			require.Equal(t, fmt.Sprintf("after-%d", i), got)
		case <-time.After(10 * time.Second):
			t.Fatalf("STUCK: timed out waiting for post-start message %d (received %d total)", i, received.Load())
		}
	}
	t.Logf("Step 4: Consumer received all 3 post-start messages (total: %d)", received.Load())

	// Cleanup
	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	require.NoError(t, consumer.Stop(stopCtx))
}

// TestJSConsumer_PublishAfterMultipleIteratorCycles tests that messages published
// after multiple iterator timeout/restart cycles are still received.
// This simulates the user's real-world scenario where there's a longer gap between
// initial message consumption and new message publication.
func TestJSConsumer_PublishAfterMultipleIteratorCycles(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "repro-cycles"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"cycles.*"},
	})
	require.NoError(t, err)

	// Publish initial messages
	for i := 1; i <= 3; i++ {
		_, err = js.Publish(ctx, "cycles.0", []byte(fmt.Sprintf("initial-%d", i)))
		require.NoError(t, err)
	}
	t.Log("Step 1: Published 3 initial messages")

	// Start consumer with a SHORT FetchTimeout so iterator cycles quickly
	var received atomic.Int32
	msgCh := make(chan string, 20)
	consumer, err := ipartition.NewJSConsumer(js, ipartition.ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "cycles.{{partition}}",
		},
		StreamName:   streamName,
		ConsumerName: "test-cycles-0",
		Partition:    0,
		FetchTimeout: 1 * time.Second, // Short timeout to force iterator restarts
	}, func(_ context.Context, msg jetstream.Msg) error {
		n := received.Add(1)
		data := string(msg.Data())
		t.Logf("  Received message #%d: %s", n, data)
		msgCh <- data
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, consumer.Start(ctx))

	// Wait for initial messages
	for i := 1; i <= 3; i++ {
		select {
		case <-msgCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for initial message %d", i)
		}
	}
	t.Log("Step 2: Consumer received all 3 initial messages")

	// Wait long enough for multiple iterator cycles to occur (each cycle ~500ms + 200ms sleep)
	// This ensures the iterator has restarted several times before new messages arrive
	t.Log("Step 3: Waiting 5 seconds (multiple iterator cycles)...")
	time.Sleep(5 * time.Second)

	// Publish more messages after the long gap
	for i := 1; i <= 3; i++ {
		_, err = js.Publish(ctx, "cycles.0", []byte(fmt.Sprintf("later-%d", i)))
		require.NoError(t, err)
	}
	t.Log("Step 4: Published 3 more messages after long gap")

	// Consumer should receive them
	for i := 1; i <= 3; i++ {
		select {
		case <-msgCh:
		case <-time.After(10 * time.Second):
			t.Fatalf("STUCK: timed out waiting for post-gap message %d (total received: %d)", i, received.Load())
		}
	}
	t.Logf("Step 5: Consumer received all 3 post-gap messages (total: %d)", received.Load())

	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	require.NoError(t, consumer.Stop(stopCtx))
}

// TestJSConsumer_SeparatePublisherConnections simulates nats CLI behavior:
// each publish uses a SEPARATE connection that is closed after publishing.
// This is the real-world scenario: connect -> publish -> disconnect.
func TestJSConsumer_SeparatePublisherConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	ns, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "repro-separate"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"separate.*"},
	})
	require.NoError(t, err)

	// Helper: publish using a separate, short-lived connection (like nats CLI)
	publishWithNewConn := func(subject, data string) {
		t.Helper()
		pubNC, err := nats.Connect(ns.ClientURL())
		require.NoError(t, err)
		pubJS, err := jetstream.New(pubNC)
		require.NoError(t, err)
		_, err = pubJS.Publish(ctx, subject, []byte(data))
		require.NoError(t, err)
		pubNC.Close() // close immediately after publish, like nats CLI
		t.Logf("  Published %q via separate connection (now closed)", data)
	}

	// Step 1: Publish with separate connections BEFORE consumer starts
	for i := 1; i <= 3; i++ {
		publishWithNewConn("separate.0", fmt.Sprintf("before-%d", i))
	}
	t.Log("Step 1: Published 3 messages via separate connections")

	// Step 2: Start consumer
	var received atomic.Int32
	msgCh := make(chan string, 20)
	consumer, err := ipartition.NewJSConsumer(js, ipartition.ConsumerConfig{
		PartitionConfig: partition.PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "separate.{{partition}}",
		},
		StreamName:   streamName,
		ConsumerName: "test-separate-0",
		Partition:    0,
		FetchTimeout: 2 * time.Second,
	}, func(_ context.Context, msg jetstream.Msg) error {
		n := received.Add(1)
		data := string(msg.Data())
		t.Logf("  Received message #%d: %s (subject=%s)", n, data, msg.Subject())
		msgCh <- data
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, consumer.Start(ctx))

	// Wait for initial 3 messages
	for i := 1; i <= 3; i++ {
		select {
		case got := <-msgCh:
			require.Equal(t, fmt.Sprintf("before-%d", i), got)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for initial message %d", i)
		}
	}
	t.Log("Step 2: Consumer received all 3 initial messages")

	// Small gap
	time.Sleep(1 * time.Second)

	// Step 3: Publish MORE messages via SEPARATE connections (nats CLI style)
	for i := 1; i <= 3; i++ {
		publishWithNewConn("separate.0", fmt.Sprintf("after-%d", i))
	}
	t.Log("Step 3: Published 3 more messages via separate connections")

	// Step 4: Consumer should receive them
	for i := 1; i <= 3; i++ {
		select {
		case got := <-msgCh:
			require.Equal(t, fmt.Sprintf("after-%d", i), got)
		case <-time.After(15 * time.Second):
			t.Fatalf("STUCK: timed out waiting for post-start message %d (total received: %d)", i, received.Load())
		}
	}
	t.Logf("Step 4: Consumer received all 3 post-start messages (total: %d)", received.Load())

	// Cleanup
	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	require.NoError(t, consumer.Stop(stopCtx))
}
