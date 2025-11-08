package integration_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/subscription"
	partitesting "github.com/arloliu/parti/testing"
	"github.com/arloliu/parti/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestWorkerConsumer_NewAndDefaults(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)

	// No-op handler
	noop := subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil })

	// Create a unique stream for this test to avoid collisions with other tests.
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateStream(context.Background(), jetstream.StreamConfig{Name: "defaults-stream", Subjects: []string{"work.>"}})
	require.NoError(t, err)

	// Valid config (explicit ack policy)
	helper, err := subscription.NewWorkerConsumer(nc, subscription.WorkerConsumerConfig{
		StreamName:      "defaults-stream",
		ConsumerPrefix:  "test",
		SubjectTemplate: "work.{{.PartitionID}}",
		AckPolicy:       jetstream.AckExplicitPolicy,
	}, noop)
	require.NoError(t, err)
	require.NotNil(t, helper)

	// Defaults applied when zero values provided (uses same stream)
	helper2, err := subscription.NewWorkerConsumer(nc, subscription.WorkerConsumerConfig{
		StreamName:      "defaults-stream",
		ConsumerPrefix:  "test",
		SubjectTemplate: "work.{{.PartitionID}}",
	}, noop)
	require.NoError(t, err)
	require.NotNil(t, helper2)
	// Access unexported config via observable behavior (AckPolicy default should be explicit).
	// We can't directly access helper2.config since it's unexported; verify by performing an update that relies on AckPolicy.
	ctx := context.Background()
	require.NoError(t, helper2.UpdateWorkerConsumer(ctx, "w1", []types.Partition{{Keys: []string{"p"}}}))
}

func TestWorkerConsumer_Basic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)
	ctx := context.Background()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "work-stream", Subjects: []string{"work.>"}})
	require.NoError(t, err)

	var msgCount atomic.Int32
	helper, err := subscription.NewWorkerConsumer(nc, subscription.WorkerConsumerConfig{
		StreamName:        "work-stream",
		ConsumerPrefix:    "worker",
		SubjectTemplate:   "work.{{.PartitionID}}",
		AckPolicy:         jetstream.AckExplicitPolicy,
		AckWait:           5 * time.Second,
		MaxDeliver:        3,
		InactiveThreshold: 1 * time.Hour,
		BatchSize:         5,
		FetchTimeout:      1 * time.Second,
		MaxRetries:        3,
		RetryBackoff:      100 * time.Millisecond,
	}, subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { msgCount.Add(1); return msg.Ack() }))
	require.NoError(t, err)
	defer helper.Close(ctx)

	initial := []types.Partition{{Keys: []string{"tool_001"}}, {Keys: []string{"tool_002"}}}
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "test-worker", initial))

	_, err = js.Publish(ctx, "work.tool_001", []byte("message 1"))
	require.NoError(t, err)
	_, err = js.Publish(ctx, "work.tool_002", []byte("message 2"))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return msgCount.Load() >= 2 }, 5*time.Second, 100*time.Millisecond)

	remaining := []types.Partition{{Keys: []string{"tool_002"}}}
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "test-worker", remaining))

	_, err = js.Publish(ctx, "work.tool_002", []byte("message 3"))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return msgCount.Load() >= 3 }, 5*time.Second, 100*time.Millisecond)
}

func TestWorkerConsumer_MessageHandling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)
	ctx := context.Background()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "test-stream", Subjects: []string{"test.>"}})
	require.NoError(t, err)

	var mu sync.Mutex
	processed := make(map[string]int)
	h := subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		data := string(msg.Data())
		mu.Lock()
		processed[data]++
		count := processed[data]
		mu.Unlock()
		if count == 1 {
			return errors.New("simulated error")
		}

		return nil
	})

	helper, err := subscription.NewWorkerConsumer(nc, subscription.WorkerConsumerConfig{
		StreamName:        "test-stream",
		ConsumerPrefix:    "test",
		SubjectTemplate:   "test.{{.PartitionID}}",
		AckPolicy:         jetstream.AckExplicitPolicy,
		AckWait:           2 * time.Second,
		MaxDeliver:        3,
		InactiveThreshold: 1 * time.Hour,
		BatchSize:         10,
		FetchTimeout:      1 * time.Second,
		MaxRetries:        3,
		RetryBackoff:      100 * time.Millisecond,
	}, h)
	require.NoError(t, err)
	defer helper.Close(ctx)

	initial := []types.Partition{{Keys: []string{"partition", "1"}}}
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-msg", initial))
	_, err = js.Publish(ctx, "test.partition.1", []byte("test-message"))
	require.NoError(t, err)

	require.Eventually(t, func() bool { mu.Lock(); v := processed["test-message"]; mu.Unlock(); return v == 2 }, 10*time.Second, 100*time.Millisecond)
}

func TestWorkerConsumer_Info(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)
	ctx := context.Background()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "info-stream", Subjects: []string{"info.>"}})
	require.NoError(t, err)

	helper, err := subscription.NewWorkerConsumer(nc, subscription.WorkerConsumerConfig{
		StreamName:        "info-stream",
		ConsumerPrefix:    "info",
		SubjectTemplate:   "info.{{.PartitionID}}",
		AckPolicy:         jetstream.AckExplicitPolicy,
		AckWait:           5 * time.Second,
		MaxDeliver:        3,
		InactiveThreshold: 1 * time.Hour,
		BatchSize:         5,
		FetchTimeout:      1 * time.Second,
		MaxRetries:        3,
		RetryBackoff:      100 * time.Millisecond,
	}, subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil }))
	require.NoError(t, err)
	defer helper.Close(ctx)

	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-info", []types.Partition{{Keys: []string{"test"}}}))

	cons, err := js.Consumer(ctx, "info-stream", "info-worker-info")
	require.NoError(t, err)
	ci, err := cons.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, "info-worker-info", ci.Name)
}

func TestWorkerConsumer_CloseAndLogger(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)
	ctx := context.Background()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: "close-stream", Subjects: []string{"close.>"}})
	require.NoError(t, err)

	testLogger := partitesting.NewTestLogger(t)
	helper, err := subscription.NewWorkerConsumer(nc, subscription.WorkerConsumerConfig{
		StreamName:        "close-stream",
		ConsumerPrefix:    "close",
		SubjectTemplate:   "close.{{.PartitionID}}",
		AckPolicy:         jetstream.AckExplicitPolicy,
		AckWait:           30 * time.Second,
		MaxDeliver:        3,
		InactiveThreshold: 1 * time.Hour,
		BatchSize:         10,
		FetchTimeout:      2 * time.Second,
		MaxRetries:        2,
		RetryBackoff:      100 * time.Millisecond,
		Logger:            testLogger,
	}, subscription.MessageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil }))
	require.NoError(t, err)

	defer func() { require.NoError(t, helper.Close(ctx)) }()

	initial := []types.Partition{{Keys: []string{"partition1"}}}
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-log", initial))
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-log", initial)) // no-op
	require.NoError(t, helper.UpdateWorkerConsumer(ctx, "worker-log", nil))     // drop subjects
}
