package integration_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti"
	"github.com/arloliu/parti/subscription"
	partitesting "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestWorkerConsumer_AutoRecreateOnExternalDeletion verifies that the helper automatically
// recreates the consumer when it is deleted externally, without requiring a changed assignment.
func TestWorkerConsumer_AutoRecreateOnExternalDeletion(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}
	ctx := context.Background()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Prepare stream
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "autorec-stream", Subjects: []string{"autorec.test.>"}})
	require.NoError(t, err)

	// Handler counts messages
	var handled atomic.Int64
	mh := subscription.MessageHandlerFunc(func(c context.Context, msg jetstream.Msg) error {
		handled.Add(1)
		return msg.Ack()
	})

	helper, err := subscription.NewWorkerConsumer(js, subscription.WorkerConsumerConfig{
		StreamName:      "autorec-stream",
		ConsumerPrefix:  "wkr",
		SubjectTemplate: "autorec.test.{{.PartitionID}}",
		BatchSize:       10,
	}, mh)
	require.NoError(t, err)
	defer helper.Close(context.Background())

	// Initial assignment
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-AR", []parti.Partition{{Keys: []string{"a"}}, {Keys: []string{"b"}}}))
	info, err := helper.WorkerConsumerInfo(ctx)
	require.NoError(t, err)
	durable := info.Config.Durable

	// Delete consumer externally
	require.NoError(t, js.DeleteConsumer(ctx, "autorec-stream", durable))

	// Publish a message after deletion; helper should auto-recreate and consume it
	_, err = js.Publish(ctx, "autorec.test.a", []byte("after-delete"))
	require.NoError(t, err)

	// Eventually, the consumer should be recreated automatically and info should succeed
	require.Eventually(t, func() bool {
		ci, e := helper.WorkerConsumerInfo(ctx)
		if e != nil {
			return false
		}
		return len(ci.Config.FilterSubjects) == 2
	}, 10*time.Second, 100*time.Millisecond)

	// And handler should have processed at least one message
	require.Eventually(t, func() bool { return handled.Load() > 0 }, 5*time.Second, 50*time.Millisecond)
}
