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
	helper, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
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

	// Ensure per-subject consumers for work.a and work.b are created
	waitForSubjectInfos(t, helper, []string{"work.a", "work.b"})

	// Now modify the partition source to simulate an assignment change (remove b, add c)
	newPartitions := []parti.Partition{{Keys: []string{"a"}}, {Keys: []string{"c"}}}
	// Update static source to simulate change
	src.Update(newPartitions)

	// Trigger refresh which will lead to recalculation and assignment change
	refreshCtx, refreshCancel := context.WithTimeout(ctx, 5*time.Second)
	require.NoError(t, mgr.RefreshPartitions(refreshCtx))
	refreshCancel()

	// Expect per-subject consumers for work.a and work.c after update
	waitForSubjectInfos(t, helper, []string{"work.a", "work.c"})
}

// waitForSubjectInfos polls helper.SubjectConsumerInfos until all expected subjects have info or times out.
func waitForSubjectInfos(t *testing.T, helper *subscription.WorkerConsumer, expected []string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		infos, err := helper.SubjectConsumerInfos(context.Background(), expected)
		if err == nil {
			all := true
			for _, s := range expected {
				if infos[s] == nil {
					all = false
					break
				}
			}
			if all {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("subjects %v did not have consumers within timeout", expected)
}
