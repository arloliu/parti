package coordinator_test

import (
	"context"
	"testing"
	"time"

	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/test/simulation/internal/coordinator"
	"github.com/arloliu/parti/v2/test/simulation/internal/producer"
	"github.com/arloliu/parti/v2/test/simulation/internal/worker"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestChaos_WorkerPause(t *testing.T) {
	// Enable logging
	// t.Setenv("PARTI_SIM_LOG", "1")

	// Start embedded NATS
	_, nc := partitesting.StartEmbeddedNATS(t)
	defer nc.Close()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create stream
	_, err = js.CreateStream(context.Background(), jetstream.StreamConfig{
		Name:     "SIMULATION",
		Subjects: []string{"simulation.partition.>"},
	})
	require.NoError(t, err)

	// Create channels
	reportCh := make(chan coordinator.ReceivedMessage, 100)
	assignCh := make(chan coordinator.AssignmentReport, 10)

	// Create worker
	cfg := worker.Config{
		ID:                  "pause-test-worker",
		NC:                  nc,
		JS:                  js,
		PartitionCount:      1,
		AssignmentStrategy:  "consistent-hash",
		ProcessingDelayMin:  1 * time.Millisecond,
		ProcessingDelayMax:  5 * time.Millisecond,
		CoordinatorReportCh: reportCh,
		AssignmentReportCh:  assignCh,
	}

	w, err := worker.NewWorker(cfg)
	require.NoError(t, err)

	ctx := t.Context()

	err = w.Start(ctx)
	require.NoError(t, err)
	defer w.Stop()

	// Wait for assignment
	select {
	case <-assignCh:
		t.Log("Worker assigned partitions")
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for assignment")
	}
	// Give a little more time for consumer creation after assignment
	time.Sleep(500 * time.Millisecond)

	// Helper to publish message
	publishMsg := func(seq int64) {
		msg := producer.SimulationMessage{
			PartitionID:       0,
			PartitionSequence: seq,
			Timestamp:         time.Now().UnixMicro(),
			// Payload:           []byte("test"), // SimulationMessage doesn't have Payload field
		}
		data, _ := msg.Marshal()
		_, err := js.Publish(ctx, "simulation.partition.0", data)
		require.NoError(t, err)
	}

	// 1. Verify normal processing
	publishMsg(1)
	select {
	case msg := <-reportCh:
		require.Equal(t, int64(1), msg.PartitionSequence)
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for message 1")
	}

	// 2. Pause worker
	pauseDuration := 2 * time.Second
	w.Pause(pauseDuration)
	t.Logf("Paused worker for %v", pauseDuration)

	// 3. Publish message during pause
	publishMsg(2)

	// 4. Verify NO report during pause
	select {
	case msg := <-reportCh:
		t.Fatalf("Received message %d during pause", msg.PartitionSequence)
	case <-time.After(1 * time.Second):
		// Expected timeout
	}

	// 5. Verify report resumes after pause
	select {
	case msg := <-reportCh:
		require.Equal(t, int64(2), msg.PartitionSequence)
	case <-time.After(2 * time.Second): // Wait remaining pause + buffer
		t.Fatal("Timeout waiting for message 2 after pause")
	}
}

func TestChaos_NetworkDisconnect(t *testing.T) {
	// t.Setenv("PARTI_SIM_LOG", "1")

	// Start embedded NATS
	srv, _ := partitesting.StartEmbeddedNATS(t)
	defer srv.Shutdown()

	// Create NetworkControl
	netCtrl := worker.NewNetworkControl()

	// Connect with custom dialer
	opts := []nats.Option{
		nats.SetCustomDialer(netCtrl),
		nats.ReconnectWait(100 * time.Millisecond),
		nats.MaxReconnects(-1),
	}
	nc, err := nats.Connect(srv.ClientURL(), opts...)
	require.NoError(t, err)
	defer nc.Close()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create stream
	_, err = js.CreateStream(context.Background(), jetstream.StreamConfig{
		Name:     "SIMULATION",
		Subjects: []string{"simulation.partition.>"},
	})
	require.NoError(t, err)

	reportCh := make(chan coordinator.ReceivedMessage, 100)
	assignCh := make(chan coordinator.AssignmentReport, 10)

	cfg := worker.Config{
		ID:                  "disconnect-test-worker",
		NC:                  nc,
		JS:                  js,
		PartitionCount:      1,
		AssignmentStrategy:  "consistent-hash",
		ProcessingDelayMin:  1 * time.Millisecond,
		ProcessingDelayMax:  5 * time.Millisecond,
		CoordinatorReportCh: reportCh,
		AssignmentReportCh:  assignCh,
	}

	w, err := worker.NewWorker(cfg)
	require.NoError(t, err)
	w.SetNetworkControl(netCtrl)

	ctx := t.Context()

	err = w.Start(ctx)
	require.NoError(t, err)
	defer w.Stop()

	// Wait for assignment
	select {
	case <-assignCh:
		t.Log("Worker assigned partitions")
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for assignment")
	}
	time.Sleep(500 * time.Millisecond)

	publishMsg := func(seq int64) {
		msg := producer.SimulationMessage{
			PartitionID:       0,
			PartitionSequence: seq,
			Timestamp:         time.Now().UnixMicro(),
			// Payload:           []byte("test"),
		}
		data, _ := msg.Marshal()
		// Use a separate connection for publishing to avoid being affected by worker disconnect
		pubNc, _ := nats.Connect(srv.ClientURL())
		defer pubNc.Close()
		pubJs, _ := jetstream.New(pubNc)
		_, err := pubJs.Publish(ctx, "simulation.partition.0", data)
		require.NoError(t, err)
	}

	// 1. Normal processing
	publishMsg(1)
	select {
	case msg := <-reportCh:
		require.Equal(t, int64(1), msg.PartitionSequence)
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for message 1")
	}

	// 2. Disconnect
	t.Log("Disconnecting worker...")
	w.Disconnect()

	// 3. Publish message
	publishMsg(2)

	// 4. Verify NO report (worker disconnected)
	select {
	case msg := <-reportCh:
		t.Fatalf("Received message %d during disconnect", msg.PartitionSequence)
	case <-time.After(1 * time.Second):
		// Expected
	}

	// 5. Reconnect
	t.Log("Reconnecting worker...")
	w.Reconnect()

	// 6. Verify recovery
	select {
	case msg := <-reportCh:
		require.Equal(t, int64(2), msg.PartitionSequence)
	case <-time.After(5 * time.Second): // Give plenty of time for reconnect
		t.Fatal("Timeout waiting for message 2 after reconnect")
	}
}
