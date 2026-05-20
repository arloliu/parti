package consumer_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestDynamic_NewDynamic_ExplicitPullGatingFalseDoesNotSuppressPulls is the
// public regression guard for the reported contract mismatch: a Dynamic
// consumer built with the processing gate enabled AND an explicit
// WithPullGating(false) must honor the explicit disable.
//
// The processing gate auto-enables pull gating by default, which is the safe
// behavior when the caller never configured pull gating. But an explicit
// WithPullGating(false) is a deliberate opt-in to the lower-prepull-gating
// mode where messages may be pulled and then NAKed by the processing gate.
//
// The test injects partitest.NopOwnershipResolver (its GetOwner reports
// "unknown owner", ok == false). When pull gating is active, that drives
// WorkerConsumer.shouldSuppressPull into its !ok branch and suppresses every
// pull before the partition loop ever asks the iterator factory for a pull
// iterator. With pull gating correctly disabled, shouldSuppressPull returns
// early without consulting the resolver, so the iterator factory IS called.
//
// On the buggy revision durable defaulting flips the explicit false back to
// true, the resolver suppresses, and the iterator factory counter stays at
// zero. On the fixed revision the explicit false is preserved and the factory
// is invoked.
func TestDynamic_NewDynamic_ExplicitPullGatingFalseDoesNotSuppressPulls(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	streamName := "DYN_PG_DISABLE"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"pgdisable.>"},
	})
	require.NoError(t, err)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	// iterAttempts counts how many times the partition loop reached past the
	// pull-suppression check and asked for a pull iterator.
	var iterAttempts atomic.Int64
	factory := func(cons jetstream.Consumer, batch int, expiry time.Duration) (jetstream.MessagesContext, error) {
		iterAttempts.Add(1)
		heartbeat := max(expiry/2, 100*time.Millisecond)

		return cons.Messages(
			jetstream.PullMaxMessages(batch),
			jetstream.PullExpiry(expiry),
			jetstream.PullHeartbeat(heartbeat),
		)
	}

	dyn, err := consumer.NewDynamic(
		js,
		streamName,
		"pgdisable_worker",
		"pgdisable.{{.PartitionID}}",
		handler,
		consumer.WithProcessingGate(&consumer.ProcessingGateConfig{Enabled: true}),
		consumer.WithResolver(consumer.ResolverConfig{OwnershipResolver: partitest.NopOwnershipResolver{}}),
		consumer.WithPullGating(false),
		consumer.WithIteratorFactory(factory),
	)
	require.NoError(t, err)
	defer func() { _ = dyn.Stop(ctx) }()

	err = dyn.Update(ctx, "worker-1", []types.Partition{{Keys: []string{"p1"}}})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return iterAttempts.Load() > 0
	}, 10*time.Second, 50*time.Millisecond,
		"explicit WithPullGating(false) must not suppress pulls: the partition loop "+
			"never attempted to create a pull iterator, which means durable defaulting "+
			"force-enabled pull gating and the unknown-owner resolver suppressed every pull")
}
