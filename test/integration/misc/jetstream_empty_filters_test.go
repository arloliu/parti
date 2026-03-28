package misc_test

import (
	"context"
	"testing"
	"time"

	partitesting "github.com/arloliu/parti/v2/testing"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestJetStream_EmptyFilterSubjects_DeliverNothing verifies that creating a consumer
// with an empty FilterSubjects list is accepted by JetStream (2.12+) and that
// no messages are delivered to that consumer.
func TestJetStream_EmptyFilterSubjects_MatchesAll(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx := context.Background()
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create a stream with a wildcard subject
	streamName := "empty-filters-stream"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"ef.>"},
	})
	require.NoError(t, err)

	// Create a consumer with an empty FilterSubjects list
	cons, err := js.CreateOrUpdateConsumer(ctx, streamName, jetstream.ConsumerConfig{
		Name:           "efc",
		Durable:        "efc",
		FilterSubjects: []string{},
		AckPolicy:      jetstream.AckExplicitPolicy,
	})
	require.NoError(t, err, "JetStream should accept empty FilterSubjects")

	// Confirm the server reports zero filter subjects
	info, err := cons.Info(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, len(info.Config.FilterSubjects))

	// Publish a message to the stream that would normally match if filters were present
	_, err = js.Publish(ctx, "ef.a", []byte("hello"))
	require.NoError(t, err)

	// Attempt to pull a message; expect none delivered
	iter, err := cons.Messages(
		jetstream.PullMaxMessages(1),
		jetstream.PullExpiry(1*time.Second),
	)
	require.NoError(t, err)

	defer iter.Stop()

	msg, nextErr := iter.Next()
	// On JetStream 2.12, an empty FilterSubjects list is treated as no filter
	// (matches all stream subjects). We expect a message to be delivered.
	require.NoError(t, nextErr)
	require.NotNil(t, msg, "expected a message to be delivered when FilterSubjects is empty (match-all semantics)")
}
