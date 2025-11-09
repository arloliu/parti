package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	"github.com/arloliu/parti/subscription"
	partitesting "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestWorkerConsumerUpdate verifies that providing WithWorkerConsumerUpdater causes the Manager
// to invoke UpdateWorkerConsumer after initial assignment and on subsequent changes.
//
// This is an integration test (short) that spins up an embedded NATS server. It avoids
// timing flakiness by polling JetStream consumer info with a bounded timeout after forcing
// an assignment change. We keep the partition count small for speed.
func TestWorkerConsumerUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := context.Background()

	// Start embedded NATS
	_, nc := partitesting.StartEmbeddedNATS(t)

	// Create stream used for subjects
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "WORKER_TEST",
		Subjects:  []string{"work.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	// Initial partitions (two)
	partitions := []parti.Partition{{Keys: []string{"a"}}, {Keys: []string{"b"}}}
	src := source.NewStatic(partitions)

	cfg := &parti.Config{WorkerIDPrefix: "w", WorkerIDMax: 10}
	parti.SetDefaults(cfg)

	// Use consistent hash strategy for assignment
	chStrat := strategy.NewConsistentHash()

	// Durable helper for single consumer updates
	helper, err := subscription.NewWorkerConsumerJS(js, subscription.WorkerConsumerConfig{
		StreamName:      "WORKER_TEST",
		ConsumerPrefix:  "worker",
		SubjectTemplate: "work.{{.PartitionID}}",
		BatchSize:       5,
	}, subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		// Simply ACK
		return msg.Ack()
	}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = helper.Close(context.Background()) })

	mgr, err := parti.NewManager(cfg, js, src, chStrat, parti.WithWorkerConsumerUpdater(helper))
	require.NoError(t, err)

	startCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	require.NoError(t, mgr.Start(startCtx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	workerID := mgr.WorkerID()
	require.NotEmpty(t, workerID)

	// Helper should eventually reflect the two subjects: work.a and work.b
	durableName := "worker-" + workerID
	waitForSubjects(t, js, "WORKER_TEST", durableName, []string{"work.a", "work.b"})

	// Now modify the partition source to simulate an assignment change (remove b, add c)
	newPartitions := []parti.Partition{{Keys: []string{"a"}}, {Keys: []string{"c"}}}
	// Update static source to simulate change
	src.Update(newPartitions)

	// Trigger refresh which will lead to recalculation and assignment change
	refreshCtx, refreshCancel := context.WithTimeout(ctx, 5*time.Second)
	require.NoError(t, mgr.RefreshPartitions(refreshCtx))
	refreshCancel()

	// Expect subjects work.a and work.c after update
	waitForSubjects(t, js, "WORKER_TEST", durableName, []string{"work.a", "work.c"})
}

// waitForSubjects polls the consumer info until it matches the expected subjects or times out.
func waitForSubjects(t *testing.T, js jetstream.JetStream, stream, durable string, expected []string) {
	deadline := time.Now().Add(10 * time.Second)
	expectedSet := make(map[string]struct{}, len(expected))
	for _, s := range expected {
		expectedSet[s] = struct{}{}
	}

	for time.Now().Before(deadline) {
		cons, err := js.Consumer(context.Background(), stream, durable)
		if err == nil {
			info, err2 := cons.Info(context.Background())
			if err2 == nil {
				current := make(map[string]struct{}, len(info.Config.FilterSubjects))
				for _, s := range info.Config.FilterSubjects {
					current[s] = struct{}{}
				}
				if len(current) == len(expectedSet) {
					match := true
					for s := range expectedSet {
						if _, ok := current[s]; !ok {
							match = false
							break
						}
					}
					if match {
						return
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("consumer %s did not reach expected subjects %v within timeout", durable, expected)
}
