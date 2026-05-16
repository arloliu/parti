package consumer_test

import (
	"context"
	"testing"
	"time"

	parti "github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/consumer"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// Compile-time assertion: *consumer.Dynamic satisfies parti.CapabilityReporter
// so the Manager's type-assertion picks it up at runtime.
var _ parti.CapabilityReporter = (*consumer.Dynamic)(nil)

// TestDynamic_ImplementsCapabilityReporter verifies both the compile-time
// interface assertion above and the runtime forwarding: a Dynamic constructed
// with the processing gate enabled MUST report CapProcessingGate via
// Capabilities() after a successful first Update.
func TestDynamic_ImplementsCapabilityReporter(t *testing.T) {
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamName := "DYN_CAP"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     streamName,
		Subjects: []string{"dyncap.>"},
	})
	require.NoError(t, err)

	handler := consumer.MessageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	gateCfg := &consumer.ProcessingGateConfig{Enabled: true}

	d, err := consumer.NewDynamic(
		js,
		streamName,
		"dyncap_worker",
		"dyncap.{{.PartitionID}}",
		handler,
		consumer.WithProcessingGate(gateCfg),
		consumer.WithResolver(consumer.ResolverConfig{OwnershipResolver: partitest.NopOwnershipResolver{}}),
	)
	require.NoError(t, err)
	defer func() { _ = d.Stop(ctx) }()

	require.Zero(t, d.Capabilities()&types.CapProcessingGate,
		"bit must be clear before the first Update wires the gate")

	err = d.Update(ctx, "worker-1", []types.Partition{{Keys: []string{"p1"}}})
	require.NoError(t, err)

	require.NotZero(t, d.Capabilities()&types.CapProcessingGate,
		"Dynamic.Capabilities() MUST forward CapProcessingGate from inner WorkerConsumer")
}
