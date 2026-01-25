package partition

import (
	"context"
	"testing"
	"time"

	partitesting "github.com/arloliu/parti/testing"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func TestSubscriber_StartStop(t *testing.T) {
	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)

	hit := make(chan struct{}, 1)
	sub, err := NewSubscriber(
		nc,
		PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "events.{{key}}.completed.{{partition}}",
		},
		1,
		NATSMessageHandlerFunc(func(_ context.Context, msg *nats.Msg) error {
			require.Equal(t, "events.tool-1.completed.1", msg.Subject)
			hit <- struct{}{}
			return nil
		}),
	)
	require.NoError(t, err)

	require.NoError(t, sub.Start(ctx))

	require.NoError(t, nc.Publish("events.tool-1.completed.1", []byte("payload")))
	require.NoError(t, nc.Flush())

	select {
	case <-hit:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for subscriber")
	}

	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	require.NoError(t, sub.Stop(stopCtx))
}

func TestSubscriber_InvalidPartition(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)

	sub, err := NewSubscriber(
		nc,
		PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "events.{{key}}.completed.{{partition}}",
		},
		3,
		NATSMessageHandlerFunc(func(context.Context, *nats.Msg) error { return nil }),
	)
	require.ErrorIs(t, err, ErrPartitionOutOfRange)
	require.Nil(t, sub)
}

func TestSubscriber_StartTwice(t *testing.T) {
	ctx := t.Context()
	_, nc := partitesting.StartEmbeddedNATS(t)

	sub, err := NewSubscriber(
		nc,
		PartitionConfig{
			NumPartitions:  2,
			SubjectPattern: "events.{{key}}.completed.{{partition}}",
		},
		1,
		NATSMessageHandlerFunc(func(context.Context, *nats.Msg) error { return nil }),
	)
	require.NoError(t, err)

	require.NoError(t, sub.Start(ctx))
	err = sub.Start(ctx)
	require.Error(t, err)

	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	require.NoError(t, sub.Stop(stopCtx))
}

func TestSubscriber_StopWithoutStart(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)

	sub, err := NewSubscriber(
		nc,
		PartitionConfig{
			NumPartitions:  1,
			SubjectPattern: "events.{{partition}}",
		},
		0,
		NATSMessageHandlerFunc(func(context.Context, *nats.Msg) error { return nil }),
	)
	require.NoError(t, err)

	stopCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, sub.Stop(stopCtx))
}

func TestNewSubscriber_InvalidArgs(t *testing.T) {
	_, err := NewSubscriber(nil, PartitionConfig{}, 0, NATSMessageHandlerFunc(func(context.Context, *nats.Msg) error { return nil }))
	require.Error(t, err)

	_, nc := partitesting.StartEmbeddedNATS(t)

	_, err = NewSubscriber(nc, PartitionConfig{}, 0, nil)
	require.Error(t, err)
}
