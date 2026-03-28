package consumer_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/consumer"
	"github.com/arloliu/parti/kvutil"
	"github.com/arloliu/parti/source"
	"github.com/arloliu/parti/strategy"
	"github.com/arloliu/parti/subscription"
	partitesting "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
)

// TestDynamic_UpdateWithPartitions verifies that a Dynamic consumer creates
// per-partition consumers when Update is called and processes messages.
func TestDynamic_UpdateWithPartitions(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "DYN_UPD"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dyn.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var received atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		received.Add(1)
		return nil
	})

	dc, err := consumer.NewDynamic(js, streamName, "dyn-worker", "dyn.{{.PartitionID}}", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	// Update with 2 partitions
	partitions := []types.Partition{
		{Keys: []string{"a"}},
		{Keys: []string{"b"}},
	}
	require.NoError(t, dc.Update(ctx, "worker-1", partitions))

	// Publish to both partition subjects
	_, err = js.Publish(ctx, "dyn.a", []byte("msg-a"))
	require.NoError(t, err)
	_, err = js.Publish(ctx, "dyn.b", []byte("msg-b"))
	require.NoError(t, err)

	// Wait for both messages
	require.Eventually(t, func() bool {
		return received.Load() >= 2
	}, 5*time.Second, 50*time.Millisecond, "both partition messages should be received")
}

// TestDynamic_PartitionExpansion verifies that calling Update with additional
// partitions starts consumers for the new partitions while keeping existing ones.
func TestDynamic_PartitionExpansion(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "DYN_EXP"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dynexp.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var received atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		received.Add(1)
		return nil
	})

	dc, err := consumer.NewDynamic(js, streamName, "dynexp-worker", "dynexp.{{.PartitionID}}", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	// Initial assignment: a, b
	require.NoError(t, dc.Update(ctx, "worker-1", []types.Partition{
		{Keys: []string{"a"}},
		{Keys: []string{"b"}},
	}))

	// Publish to a and b
	_, err = js.Publish(ctx, "dynexp.a", []byte("init-a"))
	require.NoError(t, err)
	_, err = js.Publish(ctx, "dynexp.b", []byte("init-b"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return received.Load() >= 2
	}, 5*time.Second, 50*time.Millisecond)

	// Expand to a, b, c
	require.NoError(t, dc.Update(ctx, "worker-1", []types.Partition{
		{Keys: []string{"a"}},
		{Keys: []string{"b"}},
		{Keys: []string{"c"}},
	}))

	received.Store(0)

	// Publish to all three
	_, err = js.Publish(ctx, "dynexp.a", []byte("exp-a"))
	require.NoError(t, err)
	_, err = js.Publish(ctx, "dynexp.b", []byte("exp-b"))
	require.NoError(t, err)
	_, err = js.Publish(ctx, "dynexp.c", []byte("exp-c"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return received.Load() >= 3
	}, 5*time.Second, 50*time.Millisecond, "all 3 partitions should receive messages")
}

// TestDynamic_PartitionContraction verifies that removing partitions from the
// assignment stops consumers for the removed partitions.
func TestDynamic_PartitionContraction(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "DYN_CON"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dyncon.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var receivedA, receivedB atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		if msg.Subject() == "dyncon.a" {
			receivedA.Add(1)
		} else if msg.Subject() == "dyncon.b" {
			receivedB.Add(1)
		}
		return nil
	})

	dc, err := consumer.NewDynamic(js, streamName, "dyncon-worker", "dyncon.{{.PartitionID}}", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	// Assign a, b
	require.NoError(t, dc.Update(ctx, "worker-1", []types.Partition{
		{Keys: []string{"a"}},
		{Keys: []string{"b"}},
	}))

	// Verify both work
	_, err = js.Publish(ctx, "dyncon.a", []byte("init"))
	require.NoError(t, err)
	_, err = js.Publish(ctx, "dyncon.b", []byte("init"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return receivedA.Load() >= 1 && receivedB.Load() >= 1
	}, 5*time.Second, 50*time.Millisecond)

	// Contract to only a (remove b)
	require.NoError(t, dc.Update(ctx, "worker-1", []types.Partition{
		{Keys: []string{"a"}},
	}))
	time.Sleep(1 * time.Second) // Wait for b to be torn down

	receivedA.Store(0)
	receivedB.Store(0)

	// Publish to both -- only a should be received
	_, err = js.Publish(ctx, "dyncon.a", []byte("after-contract"))
	require.NoError(t, err)
	_, err = js.Publish(ctx, "dyncon.b", []byte("should-not-receive"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return receivedA.Load() >= 1
	}, 5*time.Second, 50*time.Millisecond, "partition a should still receive messages")
	require.Never(t, func() bool {
		return receivedB.Load() > 0
	}, 1*time.Second, 50*time.Millisecond, "partition b should NOT receive messages after removal")
}

// TestDynamic_ManagerIntegration verifies the full Manager -> Dynamic consumer
// integration path using WithWorkerConsumerUpdater.
func TestDynamic_ManagerIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "DYN_MGR"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dynmgr.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var received atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		received.Add(1)
		return nil
	})

	// Create Dynamic consumer
	dc, err := consumer.NewDynamic(js, streamName, "dynmgr-worker", "dynmgr.{{.PartitionID}}", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(2*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	// Create Manager with Dynamic consumer as the updater
	partitions := []parti.Partition{
		{Keys: []string{"x"}},
		{Keys: []string{"y"}},
	}
	src := source.NewStatic(partitions)
	chStrat := strategy.NewConsistentHash()
	cfg := &parti.Config{WorkerIDPrefix: "w", WorkerIDMax: 10}
	parti.SetDefaults(cfg)

	mgr, err := parti.NewManager(cfg, js, src, chStrat, parti.WithWorkerConsumerUpdater(dc))
	require.NoError(t, err)

	startCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	require.NoError(t, mgr.Start(startCtx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	workerID := mgr.WorkerID()
	require.NotEmpty(t, workerID)

	// Wait for assignment to propagate
	time.Sleep(2 * time.Second)

	// Publish to assigned partitions
	_, err = js.Publish(ctx, "dynmgr.x", []byte("mgr-x"))
	require.NoError(t, err)
	_, err = js.Publish(ctx, "dynmgr.y", []byte("mgr-y"))
	require.NoError(t, err)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if received.Load() >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	require.GreaterOrEqual(t, received.Load(), int32(2),
		"dynamic consumer should receive messages via manager assignment")
}

// TestDynamic_StopAllPartitions verifies that Stop terminates all partition consumers.
func TestDynamic_StopAllPartitions(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "DYN_STOPALL"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dynstop.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var received atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		received.Add(1)
		return nil
	})

	dc, err := consumer.NewDynamic(js, streamName, "dynstop-worker", "dynstop.{{.PartitionID}}", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
	)
	require.NoError(t, err)

	// Start with partitions
	require.NoError(t, dc.Update(ctx, "worker-1", []types.Partition{
		{Keys: []string{"a"}},
		{Keys: []string{"b"}},
	}))

	// Verify they work
	_, err = js.Publish(ctx, "dynstop.a", []byte("msg"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return received.Load() >= 1
	}, 5*time.Second, 50*time.Millisecond)

	// Stop all
	require.NoError(t, dc.Stop(ctx))

	beforeStop := received.Load()
	_, err = js.Publish(ctx, "dynstop.a", []byte("after"))
	require.NoError(t, err)
	_, err = js.Publish(ctx, "dynstop.b", []byte("after"))
	require.NoError(t, err)

	time.Sleep(1 * time.Second)

	require.Equal(t, beforeStop, received.Load(), "no messages after stop")
}

// TestDynamic_ConcurrentUpdateConvergence verifies that multiple concurrent
// Update() calls converge to the final assignment without errors.
func TestDynamic_ConcurrentUpdateConvergence(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "DYN_CONC"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dynconc.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var received atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		received.Add(1)
		return nil
	})

	dc, err := consumer.NewDynamic(js, streamName, "dynconc-worker", "dynconc.{{.PartitionID}}", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
		consumer.WithAllowWorkerIDChange(true),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	// Fire 3 concurrent updates with different partition sets
	var wg sync.WaitGroup
	sets := [][]types.Partition{
		{{Keys: []string{"a"}}, {Keys: []string{"b"}}},
		{{Keys: []string{"c"}}, {Keys: []string{"d"}}},
		{{Keys: []string{"e"}}},
	}
	for _, ps := range sets {
		wg.Go(func() {
			_ = dc.Update(ctx, "worker-1", ps)
		})
	}
	wg.Wait()

	// Now apply the final assignment sequentially
	finalPartitions := []types.Partition{{Keys: []string{"x"}}, {Keys: []string{"y"}}}
	require.NoError(t, dc.Update(ctx, "worker-1", finalPartitions))

	// Publish to final partitions
	_, err = js.Publish(ctx, "dynconc.x", []byte("final-x"))
	require.NoError(t, err)
	_, err = js.Publish(ctx, "dynconc.y", []byte("final-y"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return received.Load() >= 2
	}, 5*time.Second, 50*time.Millisecond,
		"final assignment should receive messages after concurrent updates")
}

// TestDynamic_WithDrainOnRemove verifies that when DrainOnRemove is enabled,
// messages in-flight for a revoked partition are processed before shutdown.
func TestDynamic_WithDrainOnRemove(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "DYN_DRAIN"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dyndrain.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	var receivedA, receivedB atomic.Int32
	handler := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		if msg.Subject() == "dyndrain.a" {
			receivedA.Add(1)
		} else if msg.Subject() == "dyndrain.b" {
			receivedB.Add(1)
		}
		return nil
	})

	dc, err := consumer.NewDynamic(js, streamName, "dyndrain-worker", "dyndrain.{{.PartitionID}}", handler,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
		consumer.WithDrainOnRemove(true, 5*time.Second),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc.Stop(ctx) })

	// Assign a, b
	require.NoError(t, dc.Update(ctx, "worker-1", []types.Partition{
		{Keys: []string{"a"}},
		{Keys: []string{"b"}},
	}))

	// Publish to both
	for range 5 {
		_, err = js.Publish(ctx, "dyndrain.a", []byte("msg"))
		require.NoError(t, err)
		_, err = js.Publish(ctx, "dyndrain.b", []byte("msg"))
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return receivedA.Load() >= 5 && receivedB.Load() >= 5
	}, 5*time.Second, 50*time.Millisecond)

	// Contract: remove b (with drain enabled, b should finish pending work)
	require.NoError(t, dc.Update(ctx, "worker-1", []types.Partition{
		{Keys: []string{"a"}},
	}))

	// Wait for drain to complete
	time.Sleep(1 * time.Second)

	// Verify a still works, b is stopped
	receivedA.Store(0)
	receivedB.Store(0)

	_, err = js.Publish(ctx, "dyndrain.a", []byte("after"))
	require.NoError(t, err)
	_, err = js.Publish(ctx, "dyndrain.b", []byte("should-not"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return receivedA.Load() >= 1
	}, 5*time.Second, 50*time.Millisecond, "partition a should still be active")

	require.Never(t, func() bool {
		return receivedB.Load() > 0
	}, 1*time.Second, 50*time.Millisecond, "partition b should be stopped after drain")
}

// TestDynamic_WithProcessingGate verifies that when ProcessingGate is enabled,
// only the owning worker processes messages for a partition.
func TestDynamic_WithProcessingGate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	streamName := "DYN_GATE"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dyngate.*"},
		Storage:  jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// Seed a stable claim in the handoff KV bucket: owner = "w1"
	bucket := "parti-handoff"
	kv, err := kvutil.EnsureKVBucket(ctx, js, bucket, 2*time.Minute)
	require.NoError(t, err)

	claim := parti.HandoffClaim{
		PartitionID: "shared",
		Owner:       "w1",
		State:       parti.HandoffClaimStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
		TTLSeconds:  120,
	}
	claimBytes, err := json.Marshal(claim)
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/shared", claimBytes)
	require.NoError(t, err)

	var w1Count, w2Count atomic.Int32

	handler1 := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		w1Count.Add(1)
		return nil
	})
	handler2 := consumer.MessageHandlerFunc(func(_ context.Context, msg jetstream.Msg) error {
		w2Count.Add(1)
		return nil
	})

	gateConfig := &subscription.ProcessingGateConfig{
		Enabled:  true,
		NakDelay: 50 * time.Millisecond,
	}

	dc1, err := consumer.NewDynamic(js, streamName, "dyngate-worker", "dyngate.{{.PartitionID}}", handler1,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
		consumer.WithProcessingGate(gateConfig),
		consumer.WithResolver(subscription.ResolverConfig{HandoffBucketName: bucket}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc1.Stop(ctx) })

	dc2, err := consumer.NewDynamic(js, streamName, "dyngate-worker", "dyngate.{{.PartitionID}}", handler2,
		consumer.WithBatchSize(1),
		consumer.WithFetchTimeout(1*time.Second),
		consumer.WithProcessingGate(gateConfig),
		consumer.WithResolver(subscription.ResolverConfig{HandoffBucketName: bucket}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dc2.Stop(ctx) })

	// Both workers get the same partition, but KV claim says w1 is owner
	part := types.Partition{Keys: []string{"shared"}}
	require.NoError(t, dc1.Update(ctx, "w1", []types.Partition{part}))
	require.NoError(t, dc2.Update(ctx, "w2", []types.Partition{part}))

	// Publish messages to the shared partition
	const batch = 12
	for range batch {
		_, err = js.Publish(ctx, "dyngate.shared", []byte("msg"))
		require.NoError(t, err)
	}

	// Only w1 (the owner) should process all messages
	require.Eventually(t, func() bool {
		return w1Count.Load() >= batch
	}, 5*time.Second, 50*time.Millisecond, "owner w1 should process all messages")

	// w2 must NOT have processed any messages (gated out as non-owner)
	require.Equal(t, int32(0), w2Count.Load(), "non-owner w2 must not process any messages")
	t.Logf("w1=%d, w2=%d", w1Count.Load(), w2Count.Load())
}
