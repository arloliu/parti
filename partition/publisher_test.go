package partition

import (
	"context"
	"testing"

	partitesting "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func TestNewPublisher_InvalidArgs(t *testing.T) {
	_, err := NewPublisher(nil, PartitionConfig{})
	require.Error(t, err)
}

func TestNewPublisher_WildcardPatternRejected(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)

	_, err := NewPublisher(nc, PartitionConfig{
		NumPartitions:  2,
		SubjectPattern: "events.*.{{partition}}",
	})
	require.Error(t, err)
}

func TestPublisher_GetSubjectForPartition_OutOfRange(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)

	pub, err := NewPublisher(nc, PartitionConfig{
		NumPartitions:  2,
		SubjectPattern: "events.{{partition}}",
	})
	require.NoError(t, err)

	_, err = pub.GetSubjectForPartition(3)
	require.ErrorIs(t, err, ErrPartitionOutOfRange)
}

func TestPublisher_PublishMsgNil(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)

	pub, err := NewPublisher(nc, PartitionConfig{
		NumPartitions:  1,
		SubjectPattern: "events.{{partition}}",
	})
	require.NoError(t, err)

	err = pub.PublishMsg(context.Background(), "key", (*nats.Msg)(nil))
	require.Error(t, err)
}

func TestPublisher_Publish_ContextCanceled(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)

	pub, err := NewPublisher(nc, PartitionConfig{
		NumPartitions:  2,
		SubjectPattern: "events.{{partition}}",
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err = pub.Publish(ctx, "key", []byte("data"))
	require.ErrorIs(t, err, context.Canceled)
}

func TestPublisher_PublishMsg_ContextCanceled(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)

	pub, err := NewPublisher(nc, PartitionConfig{
		NumPartitions:  2,
		SubjectPattern: "events.{{partition}}",
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	msg := &nats.Msg{Data: []byte("data")}
	err = pub.PublishMsg(ctx, "key", msg)
	require.ErrorIs(t, err, context.Canceled)
}
