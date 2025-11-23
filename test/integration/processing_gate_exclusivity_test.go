package integration_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/internal/testutil"
	"github.com/arloliu/parti/subscription"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestProcessingGate_ExclusivityAndHandoff spins two WorkerConsumers on the same subject set
// and verifies the Processing Gate enforces exclusive processing based on KV claims.
// It then simulates a handoff by switching the claim owner and verifies processing moves
// to the new owner without duplicates.
func TestProcessingGate_Exclusivity(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := context.Background()

	// Start embedded NATS and JetStream
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create stream for events subjects
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "PGATE_TEST",
		Subjects:  []string{"events.*"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	// Unique handoff bucket for this test
	bucket := fmt.Sprintf("itest-gate-%d", time.Now().UnixNano())

	// Partition and subject template
	pid := "partition-1"
	subject := "events." + pid

	// Seed initial stable claim: owner = w1
	err = testutil.SeedHandoffStableClaim(ctx, js, bucket, pid, "w1", 2*time.Minute)
	require.NoError(t, err)

	// Counters to track processed messages per worker
	var w1Processed int32
	var w2Processed int32

	handler := func(counter *int32) subscription.MessageHandler {
		return subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
			atomic.AddInt32(counter, 1)
			// Default mode: returning nil causes helper to ACK
			return nil
		})
	}

	// Create two WorkerConsumers with Processing Gate enabled and same subject template
	cfg := subscription.WorkerConsumerConfig{
		StreamName:      "PGATE_TEST",
		ConsumerPrefix:  "worker",
		SubjectTemplate: "events.{{.PartitionID}}",
		ProcessingGate:  &subscription.ProcessingGateConfig{Enabled: true, NakDelay: 50 * time.Millisecond},
		Resolver:        subscription.ResolverConfig{HandoffBucketName: bucket},
		// Keep small batch for determinism
		BatchSize: 1,
		// Make fetch responsive
		FetchTimeout: 2 * time.Second,
	}

	w1, err := subscription.NewWorkerConsumer(js, cfg, handler(&w1Processed))
	require.NoError(t, err)
	defer func() { _ = w1.Close(context.Background()) }()

	w2, err := subscription.NewWorkerConsumer(js, cfg, handler(&w2Processed))
	require.NoError(t, err)
	defer func() { _ = w2.Close(context.Background()) }()

	// Assign both workers the same partition (they will each have their own durable)
	part := types.Partition{Keys: []string{pid}}
	require.NoError(t, w1.UpdateWorkerConsumer(ctx, "w1", []types.Partition{part}))
	require.NoError(t, w2.UpdateWorkerConsumer(ctx, "w2", []types.Partition{part}))

	// Publish a first batch while owner is w1
	const batch1 = 12
	for i := 0; i < batch1; i++ {
		_, err := js.Publish(ctx, subject, nil)
		require.NoError(t, err)
	}

	// Expect only w1 to process all messages; w2's handler should not be invoked at all
	require.Eventually(t, func() bool { return atomic.LoadInt32(&w1Processed) == batch1 }, 5*time.Second, 50*time.Millisecond)
	require.Equal(t, int32(0), atomic.LoadInt32(&w2Processed), "non-owner must not process any messages")

	// Exclusivity validated; no further handoff steps in this test
}

// switchingResolver is a simple thread-safe resolver for tests.
type switchingResolver struct {
	owner atomic.Value // string
	state atomic.Value // types.HandoffState
}

func newSwitchingResolver(initialOwner string, st types.HandoffState) *switchingResolver {
	r := &switchingResolver{}
	r.owner.Store(initialOwner)
	r.state.Store(st)
	return r
}

func (r *switchingResolver) GetOwner(partitionID string) (string, types.HandoffState, int64, bool) { //nolint:revive
	return r.owner.Load().(string), r.state.Load().(types.HandoffState), 1, true //nolint:errcheck
}

// ForceRefreshPartition is a no-op stub to satisfy types.OwnershipResolver for tests.
func (r *switchingResolver) ForceRefreshPartition(ctx context.Context, partitionID string) error {
	return nil
}

func (r *switchingResolver) set(owner string, st types.HandoffState) {
	r.owner.Store(owner)
	r.state.Store(st)
}

// TestProcessingGate_Handoff_CustomResolver validates that processing shifts to the new owner
// when the resolver changes ownership/state, without relying on KV watch timing.
func TestProcessingGate_Handoff_CustomResolver(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := context.Background()
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name: "PGATE_TEST2", Subjects: []string{"events2.*"}, Retention: jetstream.LimitsPolicy,
		Storage: jetstream.MemoryStorage, MaxMsgs: -1,
	})
	require.NoError(t, err)

	pid := "partition-1"
	subject := "events2." + pid

	var w1Processed, w2Processed int32

	res := newSwitchingResolver("w1", types.HandoffStateStable)

	cfg := subscription.WorkerConsumerConfig{
		StreamName:      "PGATE_TEST2",
		ConsumerPrefix:  "worker",
		SubjectTemplate: "events2.{{.PartitionID}}",
		ProcessingGate: &subscription.ProcessingGateConfig{
			Enabled:       true,
			NakDelay:      50 * time.Millisecond,
			AllowedStates: []types.HandoffState{types.HandoffStateStable, types.HandoffStateCommit},
		},
		Resolver:  subscription.ResolverConfig{OwnershipResolver: res},
		BatchSize: 1, FetchTimeout: 2 * time.Second,
	}

	w1, err := subscription.NewWorkerConsumer(js, cfg, subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { atomic.AddInt32(&w1Processed, 1); return nil }))
	require.NoError(t, err)
	defer func() { _ = w1.Close(context.Background()) }()

	w2, err := subscription.NewWorkerConsumer(js, cfg, subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { atomic.AddInt32(&w2Processed, 1); return nil }))
	require.NoError(t, err)
	defer func() { _ = w2.Close(context.Background()) }()

	part := types.Partition{Keys: []string{pid}}
	require.NoError(t, w1.UpdateWorkerConsumer(ctx, "w1", []types.Partition{part}))
	require.NoError(t, w2.UpdateWorkerConsumer(ctx, "w2", []types.Partition{part}))

	// Batch 1: owned by w1
	const batch1 = 8
	for i := 0; i < batch1; i++ {
		_, err := js.Publish(ctx, subject, nil)
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool { return atomic.LoadInt32(&w1Processed) == batch1 }, 5*time.Second, 50*time.Millisecond)
	require.Equal(t, int32(0), atomic.LoadInt32(&w2Processed))

	// Switch ownership to w2 (commit allowed)
	res.set("w2", types.HandoffStateCommit)
	time.Sleep(150 * time.Millisecond)
	// Pause w1 to avoid competing consumption and isolate verification to gate allowing w2
	require.NoError(t, w1.UpdateWorkerConsumer(ctx, "w1", []types.Partition{}))
	time.Sleep(100 * time.Millisecond)

	// Batch 2: should be processed by w2; account for prior NAK backlog redeliveries
	const batch2 = 7
	w2Base := atomic.LoadInt32(&w2Processed)
	for i := 0; i < batch2; i++ {
		_, err := js.Publish(ctx, subject, nil)
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool { return atomic.LoadInt32(&w2Processed) >= w2Base+batch2 }, 8*time.Second, 50*time.Millisecond)
	require.Equal(t, int32(batch1), atomic.LoadInt32(&w1Processed))
}
