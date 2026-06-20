package durable_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --- merged from worker_consumer_capabilities_test.go ---

// TestWorkerConsumer_GateWiredReportsCapProcessingGate verifies that after
// a successful UpdateWorkerConsumer with the processing gate enabled, the
// WorkerConsumer reports types.CapProcessingGate via Capabilities(), and
// that with the gate disabled the bit stays clear.
func TestWorkerConsumer_GateWiredReportsCapProcessingGate(t *testing.T) {
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("gate enabled: bit set after successful wrap", func(t *testing.T) {
		streamName := "EVENTS_GATE_ON"
		_, err := js.CreateStream(ctx, jetstream.StreamConfig{
			Name:     streamName,
			Subjects: []string{"events_on.>"},
		})
		require.NoError(t, err)

		cfg := durable.WorkerConsumerConfig{
			StreamName:      streamName,
			SubjectTemplate: "events_on.{{.PartitionID}}",
			ConsumerPrefix:  "worker_on",
			AckWait:         time.Second,
			MaxDeliver:      3,
			ProcessingGate: &durable.ProcessingGateConfig{
				Enabled: true,
			},
			Resolver: durable.ResolverConfig{
				OwnershipResolver: partitest.NopOwnershipResolver{},
			},
		}

		handler := func(_ context.Context, _ jetstream.Msg) error { return nil }
		wc, err := durable.NewWorkerConsumer(js, cfg, handler)
		require.NoError(t, err)
		defer wc.Close(ctx)

		require.Zero(t, wc.Capabilities()&types.CapProcessingGate,
			"bit must be clear before any UpdateWorkerConsumer call")

		err = wc.UpdateWorkerConsumer(ctx, "worker-1",
			[]types.Partition{{Keys: []string{"p1"}}})
		require.NoError(t, err)

		require.NotZero(t, wc.Capabilities()&types.CapProcessingGate,
			"CapProcessingGate MUST be set after a successful gate wrap")
	})

	t.Run("gate disabled: bit stays clear", func(t *testing.T) {
		streamName := "EVENTS_GATE_OFF"
		_, err := js.CreateStream(ctx, jetstream.StreamConfig{
			Name:     streamName,
			Subjects: []string{"events_off.>"},
		})
		require.NoError(t, err)

		cfg := durable.WorkerConsumerConfig{
			StreamName:      streamName,
			SubjectTemplate: "events_off.{{.PartitionID}}",
			ConsumerPrefix:  "worker_off",
			AckWait:         time.Second,
			MaxDeliver:      3,
			// ProcessingGate intentionally nil.
		}

		handler := func(_ context.Context, _ jetstream.Msg) error { return nil }
		wc, err := durable.NewWorkerConsumer(js, cfg, handler)
		require.NoError(t, err)
		defer wc.Close(ctx)

		err = wc.UpdateWorkerConsumer(ctx, "worker-1",
			[]types.Partition{{Keys: []string{"p1"}}})
		require.NoError(t, err)

		require.Zero(t, wc.Capabilities(),
			"Capabilities() must be 0 when the processing gate is disabled")
	})
}

// TestWorkerConsumer_GateBitMonotonic_StaysSetAfterLaterSubjectError
// verifies the monotonic-set contract: within a SINGLE UpdateWorkerConsumer
// call that wraps p1 successfully and then fails on p2, the
// CapProcessingGate bit MUST already be set when the call returns its
// error. This catches a regression where someone moves the
// gateWired.Store(true) outside addSubjectLoop (e.g., to after the for
// loop in UpdateWorkerConsumer).
//
// Forcing strategy: the JetStream stream is created with MaxConsumers=1.
// The first per-subject consumer create (p1) succeeds and wraps the
// handler with the gate; the second (p2) fails with "max consumers
// reached", which addSubjectLoop propagates.
func TestWorkerConsumer_GateBitMonotonic_StaysSetAfterLaterSubjectError(t *testing.T) {
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamName := "EVENTS_MONO"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:         streamName,
		Subjects:     []string{"evmono.>"},
		MaxConsumers: 1, // forces the 2nd per-subject create in a single update to fail
	})
	require.NoError(t, err)

	cfg := durable.WorkerConsumerConfig{
		StreamName:      streamName,
		SubjectTemplate: "evmono.{{.PartitionID}}",
		ConsumerPrefix:  "worker_mono",
		AckWait:         time.Second,
		MaxDeliver:      3,
		ProcessingGate: &durable.ProcessingGateConfig{
			Enabled: true,
		},
		Resolver: durable.ResolverConfig{
			OwnershipResolver: partitest.NopOwnershipResolver{},
		},
	}

	handler := func(_ context.Context, _ jetstream.Msg) error { return nil }
	wc, err := durable.NewWorkerConsumer(js, cfg, handler)
	require.NoError(t, err)
	defer wc.Close(ctx)

	// Sanity baseline: bit is clear on a fresh WorkerConsumer that has
	// never run an update. A bug that sets the bit at construction time
	// (e.g., from config alone) would be caught here.
	require.Zero(t, wc.Capabilities()&types.CapProcessingGate,
		"baseline: bit must be clear before any update runs")

	// Single update with two partitions. p1's per-subject consumer
	// create succeeds and the gate wraps; p2's create fails on the
	// stream's MaxConsumers=1 cap. UpdateWorkerConsumer iterates
	// sequentially (worker_consumer.go:154-158), so by the time the
	// error from p2 propagates back, addSubjectLoop for p1 has already
	// run gateWired.Store(true).
	err = wc.UpdateWorkerConsumer(ctx, "worker-1", []types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p2"}},
	})
	require.Error(t, err, "expected per-subject create failure for p2 (max consumers reached)")
	require.NotZero(t, wc.Capabilities()&types.CapProcessingGate,
		"CapProcessingGate MUST be set after a single update that wrapped p1 even when p2 failed mid-loop")

	// A further failed apply must still leave the bit set (cross-update
	// monotonicity).
	cancelledCtx, cancelNow := context.WithCancel(ctx)
	cancelNow()
	err = wc.UpdateWorkerConsumer(cancelledCtx, "worker-1", []types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p3"}},
	})
	require.Error(t, err)
	require.NotZero(t, wc.Capabilities()&types.CapProcessingGate,
		"CapProcessingGate MUST remain set across subsequent failed applies (monotonic)")
}

// --- merged from worker_consumer_integration_test.go ---

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
