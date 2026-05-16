package durable_test

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestWorkerConsumer_GateWiredReportsCapProcessingGate verifies that after
// a successful UpdateWorkerConsumer with the processing gate enabled, the
// WorkerConsumer reports types.CapProcessingGate via Capabilities(), and
// that with the gate disabled the bit stays clear.
func TestWorkerConsumer_GateWiredReportsCapProcessingGate(t *testing.T) {
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("gate enabled: bit set after successful wrap", func(t *testing.T) {
		streamName := "EVENTS_GATE_ON"
		_, err := js.CreateStream(ctx, jetstream.StreamConfig{
			Name:     streamName,
			Subjects: []string{"events_on.>"},
		})
		require.NoError(t, err)

		cfg := durable.WorkerConsumerConfig{
			StreamName:      streamName,
			SubjectTemplate: "events_on.{{.PartitionID}}",
			ConsumerPrefix:  "worker_on",
			AckWait:         time.Second,
			MaxDeliver:      3,
			ProcessingGate: &durable.ProcessingGateConfig{
				Enabled: true,
			},
			Resolver: durable.ResolverConfig{
				OwnershipResolver: partitest.NopOwnershipResolver{},
			},
		}

		handler := func(_ context.Context, _ jetstream.Msg) error { return nil }
		wc, err := durable.NewWorkerConsumer(js, cfg, handler)
		require.NoError(t, err)
		defer wc.Close(ctx)

		require.Zero(t, wc.Capabilities()&types.CapProcessingGate,
			"bit must be clear before any UpdateWorkerConsumer call")

		err = wc.UpdateWorkerConsumer(ctx, "worker-1",
			[]types.Partition{{Keys: []string{"p1"}}})
		require.NoError(t, err)

		require.NotZero(t, wc.Capabilities()&types.CapProcessingGate,
			"CapProcessingGate MUST be set after a successful gate wrap")
	})

	t.Run("gate disabled: bit stays clear", func(t *testing.T) {
		streamName := "EVENTS_GATE_OFF"
		_, err := js.CreateStream(ctx, jetstream.StreamConfig{
			Name:     streamName,
			Subjects: []string{"events_off.>"},
		})
		require.NoError(t, err)

		cfg := durable.WorkerConsumerConfig{
			StreamName:      streamName,
			SubjectTemplate: "events_off.{{.PartitionID}}",
			ConsumerPrefix:  "worker_off",
			AckWait:         time.Second,
			MaxDeliver:      3,
			// ProcessingGate intentionally nil.
		}

		handler := func(_ context.Context, _ jetstream.Msg) error { return nil }
		wc, err := durable.NewWorkerConsumer(js, cfg, handler)
		require.NoError(t, err)
		defer wc.Close(ctx)

		err = wc.UpdateWorkerConsumer(ctx, "worker-1",
			[]types.Partition{{Keys: []string{"p1"}}})
		require.NoError(t, err)

		require.Zero(t, wc.Capabilities(),
			"Capabilities() must be 0 when the processing gate is disabled")
	})
}

// TestWorkerConsumer_GateBitMonotonic_StaysSetAfterLaterSubjectError
// verifies the monotonic-set contract: within a SINGLE UpdateWorkerConsumer
// call that wraps p1 successfully and then fails on p2, the
// CapProcessingGate bit MUST already be set when the call returns its
// error. This catches a regression where someone moves the
// gateWired.Store(true) outside addSubjectLoop (e.g., to after the for
// loop in UpdateWorkerConsumer).
//
// Forcing strategy: the JetStream stream is created with MaxConsumers=1.
// The first per-subject consumer create (p1) succeeds and wraps the
// handler with the gate; the second (p2) fails with "max consumers
// reached", which addSubjectLoop propagates.
func TestWorkerConsumer_GateBitMonotonic_StaysSetAfterLaterSubjectError(t *testing.T) {
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streamName := "EVENTS_MONO"
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:         streamName,
		Subjects:     []string{"evmono.>"},
		MaxConsumers: 1, // forces the 2nd per-subject create in a single update to fail
	})
	require.NoError(t, err)

	cfg := durable.WorkerConsumerConfig{
		StreamName:      streamName,
		SubjectTemplate: "evmono.{{.PartitionID}}",
		ConsumerPrefix:  "worker_mono",
		AckWait:         time.Second,
		MaxDeliver:      3,
		ProcessingGate: &durable.ProcessingGateConfig{
			Enabled: true,
		},
		Resolver: durable.ResolverConfig{
			OwnershipResolver: partitest.NopOwnershipResolver{},
		},
	}

	handler := func(_ context.Context, _ jetstream.Msg) error { return nil }
	wc, err := durable.NewWorkerConsumer(js, cfg, handler)
	require.NoError(t, err)
	defer wc.Close(ctx)

	// Sanity baseline: bit is clear on a fresh WorkerConsumer that has
	// never run an update. A bug that sets the bit at construction time
	// (e.g., from config alone) would be caught here.
	require.Zero(t, wc.Capabilities()&types.CapProcessingGate,
		"baseline: bit must be clear before any update runs")

	// Single update with two partitions. p1's per-subject consumer
	// create succeeds and the gate wraps; p2's create fails on the
	// stream's MaxConsumers=1 cap. UpdateWorkerConsumer iterates
	// sequentially (worker_consumer.go:154-158), so by the time the
	// error from p2 propagates back, addSubjectLoop for p1 has already
	// run gateWired.Store(true).
	err = wc.UpdateWorkerConsumer(ctx, "worker-1", []types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p2"}},
	})
	require.Error(t, err, "expected per-subject create failure for p2 (max consumers reached)")
	require.NotZero(t, wc.Capabilities()&types.CapProcessingGate,
		"CapProcessingGate MUST be set after a single update that wrapped p1 even when p2 failed mid-loop")

	// A further failed apply must still leave the bit set (cross-update
	// monotonicity).
	cancelledCtx, cancelNow := context.WithCancel(ctx)
	cancelNow()
	err = wc.UpdateWorkerConsumer(cancelledCtx, "worker-1", []types.Partition{
		{Keys: []string{"p1"}},
		{Keys: []string{"p3"}},
	})
	require.Error(t, err)
	require.NotZero(t, wc.Capabilities()&types.CapProcessingGate,
		"CapProcessingGate MUST remain set across subsequent failed applies (monotonic)")
}
