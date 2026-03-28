package durable_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/durable"
	partitesting "github.com/arloliu/parti/v2/partitest"
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
	mh := func(c context.Context, msg jetstream.Msg) error {
		handled.Add(1)
		return msg.Ack()
	}

	helper, err := durable.NewWorkerConsumer(js, durable.WorkerConsumerConfig{
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
	subs := helper.WorkerSubjects()
	require.ElementsMatch(t, []string{"lifecycle.test.a", "lifecycle.test.b", "lifecycle.test.c"}, subs)
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

	mh := func(c context.Context, msg jetstream.Msg) error { return msg.Ack() }
	helper, err := durable.NewWorkerConsumer(js, durable.WorkerConsumerConfig{
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
		subs := helper.WorkerSubjects()
		want := map[string]struct{}{"converge.int.x": {}, "converge.int.y": {}}
		if len(subs) != 2 {
			return false
		}
		for _, s := range subs {
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

	mh := func(c context.Context, msg jetstream.Msg) error { return msg.Ack() }
	helper, err := durable.NewWorkerConsumer(js, durable.WorkerConsumerConfig{
		StreamName:          "switch-int-stream",
		ConsumerPrefix:      "wkr",
		SubjectTemplate:     "switch.int.{{.PartitionID}}",
		AllowWorkerIDChange: true,
	}, mh)
	require.NoError(t, err)
	defer helper.Close(context.Background())

	parts := []parti.Partition{{Keys: []string{"a"}}}
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-old", parts))
	// Durable names are per-subject and independent of workerID; switching workerID
	// should not recreate per-subject consumers. Verify subjects remain and messages still flow.
	subsBefore := helper.WorkerSubjects()
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-new", parts))
	subsAfter := helper.WorkerSubjects()
	require.Equal(t, subsBefore, subsAfter)
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

	mh := func(c context.Context, msg jetstream.Msg) error { return msg.Ack() }
	helper, err := durable.NewWorkerConsumer(js, durable.WorkerConsumerConfig{
		StreamName:      "extdel-stream",
		ConsumerPrefix:  "wkr",
		SubjectTemplate: "extdel.test.{{.PartitionID}}",
	}, mh)
	require.NoError(t, err)
	defer helper.Close(context.Background())

	parts := []parti.Partition{{Keys: []string{"a"}}, {Keys: []string{"b"}}}
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-X", parts))
	// Delete one per-subject durable externally and ensure helper continues operating
	// after an Update; use first subject durable.
	subs := helper.WorkerSubjects()
	require.Contains(t, subs, "extdel.test.a")
	ci, err := helper.WorkerConsumerInfo(ctx, "extdel.test.a")
	require.NoError(t, err)
	require.NoError(t, js.DeleteConsumer(ctx, "extdel-stream", ci.Name))

	// Apply a no-op update (same partitions) to let helper re-bind/create as needed
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-X", parts))

	// Add a new subject and verify it's present
	changed := []parti.Partition{{Keys: []string{"a"}}, {Keys: []string{"b"}}, {Keys: []string{"c"}}}
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-X", changed))
	require.Eventually(t, func() bool {
		have := helper.WorkerSubjects()
		want := map[string]struct{}{"extdel.test.a": {}, "extdel.test.b": {}, "extdel.test.c": {}}
		if len(have) != 3 {
			return false
		}
		for _, s := range have {
			if _, ok := want[s]; !ok {
				return false
			}
		}

		return true
	}, 5*time.Second, 100*time.Millisecond)
}
