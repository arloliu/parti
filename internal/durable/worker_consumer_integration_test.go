package durable_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestWorkerConsumer_Integration(t *testing.T) {
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create stream
	streamName := "EVENTS"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"events.>"},
	})
	require.NoError(t, err)

	// Message handler to track received messages
	var receivedCount int32
	receivedCh := make(chan string, 200)
	handler := func(ctx context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&receivedCount, 1)
		receivedCh <- string(msg.Data())
		return msg.Ack()
	}

	// Create WorkerConsumer
	cfg := durable.WorkerConsumerConfig{
		StreamName:      streamName,
		SubjectTemplate: "events.{{.PartitionID}}",
		ConsumerPrefix:  "worker",
		AckWait:         time.Second,
		MaxDeliver:      3,
	}

	wc, err := durable.NewWorkerConsumer(js, cfg, handler)
	require.NoError(t, err)
	defer wc.Close(ctx)

	// Assign partition "p1"
	p1 := types.Partition{Keys: []string{"p1"}}
	err = wc.UpdateWorkerConsumer(ctx, "worker-1", []types.Partition{p1})
	require.NoError(t, err)

	// Publish message to "events.p1"
	msgData := "hello-p1"
	_, err = js.Publish(ctx, "events.p1", []byte(msgData))
	require.NoError(t, err)

	// Verify message received
	select {
	case data := <-receivedCh:
		require.Equal(t, msgData, data)
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for message")
	}

	// Assign partition "p2", revoke "p1"
	p2 := types.Partition{Keys: []string{"p2"}}
	err = wc.UpdateWorkerConsumer(ctx, "worker-1", []types.Partition{p2})
	require.NoError(t, err)

	// Publish to "events.p2"
	msgData2 := "hello-p2"
	_, err = js.Publish(ctx, "events.p2", []byte(msgData2))
	require.NoError(t, err)

	// Verify message received from p2
	select {
	case data := <-receivedCh:
		require.Equal(t, msgData2, data)
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for message from p2")
	}

	// Publish to "events.p1" (should NOT be received)
	_, err = js.Publish(ctx, "events.p1", []byte("should-not-receive"))
	require.NoError(t, err)

	select {
	case data := <-receivedCh:
		t.Fatalf("Received unexpected message from revoked partition p1: %s", data)
	case <-time.After(500 * time.Millisecond):
		// Expected timeout
	}

	// Scale test: 100 partitions
	count := 100
	partitions := make([]types.Partition, count)
	for i := range count {
		partitions[i] = types.Partition{Keys: []string{fmt.Sprintf("scale-%d", i)}}
	}

	err = wc.UpdateWorkerConsumer(ctx, "worker-1", partitions)
	require.NoError(t, err)

	// Publish to all 100 partitions
	for i := range count {
		subject := fmt.Sprintf("events.scale-%d", i)
		msg := fmt.Sprintf("msg-%d", i)
		_, err = js.Publish(ctx, subject, []byte(msg))
		require.NoError(t, err)
	}

	// Verify all messages received
	receivedScale := make(map[string]bool)
	timeout := time.After(5 * time.Second)
	for range count {
		select {
		case data := <-receivedCh:
			receivedScale[data] = true
		case <-timeout:
			t.Fatalf("Timed out waiting for messages. Received %d/%d", len(receivedScale), count)
		}
	}

	for i := range count {
		msg := fmt.Sprintf("msg-%d", i)
		require.True(t, receivedScale[msg], "Missing message %s", msg)
	}

	// Assign partition with multi-part key
	pMulti := types.Partition{Keys: []string{"region", "us-east"}}
	err = wc.UpdateWorkerConsumer(ctx, "worker-1", []types.Partition{pMulti})
	require.NoError(t, err)

	// Publish to "events.region.us-east"
	_, err = js.Publish(ctx, "events.region.us-east", []byte("msg-multi"))
	require.NoError(t, err)

	// Verify message received
	select {
	case data := <-receivedCh:
		require.Equal(t, "msg-multi", data)
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for message from multi-part partition")
	}
}
