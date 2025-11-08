package integration_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/subscription"
	partitesting "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestWorkerConsumerLifecycleAndExpansion (integration) covers creation, no-op diff,
// expansion, and message handling continuity across updates.
func TestWorkerConsumerLifecycleAndExpansion(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}
	ctx := context.Background()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "lifecycle-stream", Subjects: []string{"lifecycle.test.>"}})
	require.NoError(t, err)

	var handled atomic.Int64
	mh := subscription.MessageHandlerFunc(func(c context.Context, msg jetstream.Msg) error {
		handled.Add(1)
		return msg.Ack()
	})

	helper, err := subscription.NewDurableHelper(nc, subscription.DurableConfig{
		StreamName:      "lifecycle-stream",
		ConsumerPrefix:  "wkr",
		SubjectTemplate: "lifecycle.test.{{.PartitionID}}",
		BatchSize:       10,
	}, mh)
	require.NoError(t, err)
	defer helper.Close(context.Background())

	initial := []parti.Partition{{Keys: []string{"a"}}, {Keys: []string{"b"}}}
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-L1", initial))

	// Publish messages
	_, err = js.Publish(ctx, "lifecycle.test.a", []byte("m1"))
	require.NoError(t, err)
	_, err = js.Publish(ctx, "lifecycle.test.b", []byte("m2"))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return handled.Load() >= 2 }, 5*time.Second, 50*time.Millisecond)

	// No-op reorder update
	noop := []parti.Partition{{Keys: []string{"b"}}, {Keys: []string{"a"}}}
	before := handled.Load()
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-L1", noop))
	_, err = js.Publish(ctx, "lifecycle.test.a", []byte("m3"))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return handled.Load() >= before+1 }, 5*time.Second, 50*time.Millisecond)

	// Expansion
	expanded := []parti.Partition{{Keys: []string{"a"}}, {Keys: []string{"b"}}, {Keys: []string{"c"}}}
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-L1", expanded))
	info, err := helper.WorkerConsumerInfo(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"lifecycle.test.a", "lifecycle.test.b", "lifecycle.test.c"}, info.Config.FilterSubjects)
	_, err = js.Publish(ctx, "lifecycle.test.c", []byte("m4"))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return handled.Load() >= before+2 }, 5*time.Second, 50*time.Millisecond)
}

// TestWorkerConsumerConcurrentUpdatesConverges ensures concurrent subject updates converge
// on the final assignment.
func TestWorkerConsumerConcurrentUpdatesConverges(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}
	ctx := context.Background()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "converge-int-stream", Subjects: []string{"converge.int.>"}})
	require.NoError(t, err)

	mh := subscription.MessageHandlerFunc(func(c context.Context, msg jetstream.Msg) error { return msg.Ack() })
	helper, err := subscription.NewDurableHelper(nc, subscription.DurableConfig{
		StreamName:      "converge-int-stream",
		ConsumerPrefix:  "wkr",
		SubjectTemplate: "converge.int.{{.PartitionID}}",
		BatchSize:       10,
	}, mh)
	require.NoError(t, err)
	defer helper.Close(context.Background())

	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-CI", []parti.Partition{{Keys: []string{"a"}}}))
	// Prepare assignments
	a1 := []parti.Partition{{Keys: []string{"a"}}, {Keys: []string{"b"}}}
	a2 := []parti.Partition{{Keys: []string{"c"}}}
	a3 := []parti.Partition{{Keys: []string{"a"}}, {Keys: []string{"c"}}, {Keys: []string{"d"}}}
	final := []parti.Partition{{Keys: []string{"x"}}, {Keys: []string{"y"}}}

	var wg sync.WaitGroup
	wg.Go(func() { _ = helper.UpdateWorkerConsumer(ctx, "worker-CI", a1) })
	wg.Go(func() { _ = helper.UpdateWorkerConsumer(ctx, "worker-CI", a2) })
	wg.Go(func() { _ = helper.UpdateWorkerConsumer(ctx, "worker-CI", a3) })
	wg.Wait()
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-CI", final))

	require.Eventually(t, func() bool {
		info, err := helper.WorkerConsumerInfo(ctx)
		if err != nil {
			return false
		}
		want := map[string]struct{}{"converge.int.x": {}, "converge.int.y": {}}
		if len(info.Config.FilterSubjects) != 2 {
			return false
		}
		for _, s := range info.Config.FilterSubjects {
			if _, ok := want[s]; !ok {
				return false
			}
		}

		return true
	}, 5*time.Second, 50*time.Millisecond)
}

// TestWorkerConsumerWorkerIDSwitch verifies that switching workerID creates a new durable
// and best-effort deletes the old durable.
func TestWorkerConsumerWorkerIDSwitch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}
	ctx := context.Background()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "switch-int-stream", Subjects: []string{"switch.int.>"}})
	require.NoError(t, err)

	mh := subscription.MessageHandlerFunc(func(c context.Context, msg jetstream.Msg) error { return msg.Ack() })
	helper, err := subscription.NewDurableHelper(nc, subscription.DurableConfig{
		StreamName:      "switch-int-stream",
		ConsumerPrefix:  "wkr",
		SubjectTemplate: "switch.int.{{.PartitionID}}",
	}, mh)
	require.NoError(t, err)
	defer helper.Close(context.Background())

	parts := []parti.Partition{{Keys: []string{"a"}}}
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-old", parts))
	info1, err := helper.WorkerConsumerInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, "wkr-worker-old", info1.Config.Durable)

	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-new", parts))
	info2, err := helper.WorkerConsumerInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, "wkr-worker-new", info2.Config.Durable)

	// Old durable should be deleted best-effort; allow some time for async cleanup goroutine.
	stream, err := js.Stream(ctx, "switch-int-stream")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := stream.Consumer(ctx, "wkr-worker-old")
		return errors.Is(err, jetstream.ErrConsumerNotFound)
	}, 5*time.Second, 100*time.Millisecond)
}

// TestWorkerConsumerExternalDeletion simulates external deletion of the durable consumer
// and verifies that a subsequent UpdateWorkerConsumer re-creates it. A no-op update should NOT
// recreate (current behavior), while a changed assignment does.
func TestWorkerConsumerExternalDeletion(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}
	ctx := context.Background()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "extdel-stream", Subjects: []string{"extdel.test.>"}})
	require.NoError(t, err)

	mh := subscription.MessageHandlerFunc(func(c context.Context, msg jetstream.Msg) error { return msg.Ack() })
	helper, err := subscription.NewDurableHelper(nc, subscription.DurableConfig{
		StreamName:      "extdel-stream",
		ConsumerPrefix:  "wkr",
		SubjectTemplate: "extdel.test.{{.PartitionID}}",
	}, mh)
	require.NoError(t, err)
	defer helper.Close(context.Background())

	parts := []parti.Partition{{Keys: []string{"a"}}, {Keys: []string{"b"}}}
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-X", parts))
	info, err := helper.WorkerConsumerInfo(ctx)
	require.NoError(t, err)
	durable := info.Config.Durable

	// Delete consumer externally
	require.NoError(t, js.DeleteConsumer(ctx, "extdel-stream", durable))

	// WorkerConsumerInfo should now fail (stale pointer calls Info -> ErrConsumerNotFound)
	_, err = helper.WorkerConsumerInfo(ctx)
	require.Error(t, err, "expected error after external deletion")

	// No-op update (same partitions) does NOT recreate (current behavior)
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-X", parts))
	_, err = helper.WorkerConsumerInfo(ctx)
	require.Error(t, err, "still error after no-op update")

	// Changed assignment (add partition c) should recreate via CreateOrUpdateConsumer
	changed := []parti.Partition{{Keys: []string{"a"}}, {Keys: []string{"b"}}, {Keys: []string{"c"}}}
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-X", changed))
	require.Eventually(t, func() bool {
		ci, err2 := helper.WorkerConsumerInfo(ctx)
		return err2 == nil && len(ci.Config.FilterSubjects) == 3
	}, 5*time.Second, 100*time.Millisecond)
}
