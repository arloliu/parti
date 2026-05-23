package recovery

import (
	"context"
	"errors"
	"testing"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// errConsumerConfigMismatch simulates a NATS consumer-config-mismatch
// returned by CreateOrUpdateConsumer when a restored same-named consumer
// has incompatible config. Not stream-not-found; the contract under test
// is that ANY post-hook recovery failure wraps types.ErrStreamMissing.
var errConsumerConfigMismatch = errors.New("nats: consumer config mismatch (10094)")

// spyRecreate captures the config passed to RecreateFunc on each call,
// optionally returning an error to drive failure paths.
type spyRecreate struct {
	calls   []jetstream.ConsumerConfig
	nextErr error
}

func (s *spyRecreate) fn() RecreateFunc {
	return func(_ context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {
		s.calls = append(s.calls, cfg)
		if s.nextErr != nil {
			return nil, s.nextErr
		}
		return nil, nil
	}
}

// T2 — fresh-stream replay. When the hook recreates the stream without
// a preserved same-named consumer, the new consumer's AckFloor is 0.
// HandleStreamRecreated must reset the checkpoint to 0; the next
// RebuildAfterStreamRecreated must build a config that replays from
// the start of the new stream (DeliverAllPolicy, OptStartSeq=0).
//
// This is the load-bearing fresh-stream invariant: without the
// recreatedSinceLastBuild override, BuildConfig with checkpoint=0 falls
// back to DeliverNewPolicy and skips messages published before the
// replacement consumer is bound.
func TestController_RebuildAfterStreamRecreated_FreshStream_ReplayFromStart(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})
	require.NotNil(t, c)

	c.checkpoint.Seed(100)
	require.Equal(t, uint64(100), c.checkpoint.Value())

	// Hook recreates the stream without preserving the consumer.
	c.HandleStreamRecreated(context.Background(), stubInfo(0))
	require.Equal(t, uint64(0), c.checkpoint.Value(),
		"fresh-stream HandleStreamRecreated must reset checkpoint to 0 and seed sees AckFloor=0")

	spy := &spyRecreate{}
	_, err := c.RebuildAfterStreamRecreated(context.Background(), jetstream.ConsumerConfig{}, spy.fn())
	require.NoError(t, err)
	require.Len(t, spy.calls, 1, "RebuildAfterStreamRecreated must call recreate exactly once")

	cfg := spy.calls[0]
	require.Equal(t, jetstream.DeliverAllPolicy, cfg.DeliverPolicy,
		"fresh-stream rebuild must use DeliverAllPolicy so the new consumer replays from seq 1, not skip pre-bind messages")
	require.Equal(t, uint64(0), cfg.OptStartSeq,
		"DeliverAllPolicy must not set OptStartSeq")
}

// T2b — restored-backup variant. When the hook recreates the stream
// with a restored same-named consumer whose AckFloor > 0 (e.g., a
// stream-snapshot restore preserved consumer state), HandleStreamRecreated
// resets the checkpoint to 0 first, then SeedCheckpoint reads the
// restored AckFloor. The next rebuild must resume from that floor +1.
func TestController_RebuildAfterStreamRecreated_RestoredAckFloor_ResumesFromFloorPlusOne(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})
	require.NotNil(t, c)

	c.checkpoint.Seed(100)

	// Restored backup: same-named consumer has AckFloor=50.
	c.HandleStreamRecreated(context.Background(), stubInfo(50))
	require.Equal(t, uint64(50), c.checkpoint.Value(),
		"restored-backup HandleStreamRecreated must reset to 0 then seed to AckFloor=50")

	spy := &spyRecreate{}
	_, err := c.RebuildAfterStreamRecreated(context.Background(), jetstream.ConsumerConfig{}, spy.fn())
	require.NoError(t, err)
	require.Len(t, spy.calls, 1)

	cfg := spy.calls[0]
	require.Equal(t, jetstream.DeliverByStartSequencePolicy, cfg.DeliverPolicy,
		"with checkpoint>0, rebuild must resume by start-sequence, not replay from beginning")
	require.Equal(t, uint64(51), cfg.OptStartSeq,
		"OptStartSeq must be checkpoint+1; resuming at the next unprocessed sequence")
}

// T2d — restored-consumer incompatible config (v4 — v3-P0.2 pin). When
// the restored same-named consumer has incompatible config (e.g.
// AckPolicy mismatch), recreate returns an error that is NOT
// stream-not-found. RebuildAfterStreamRecreated must still wrap it
// with types.ErrStreamMissing so the manager observer route fires.
// Pinning the typed-error class is the contract that keeps the
// post-hook recovery flow consistent regardless of underlying cause.
//
// Note: the types.ErrStreamMissing constant is added by the
// StreamMissingHook public-surface change (Task #28). This test
// references it via the public types package.
func TestController_RebuildAfterStreamRecreated_RecreateError_WrapsErrStreamMissing(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})
	require.NotNil(t, c)

	c.checkpoint.Seed(100)
	c.HandleStreamRecreated(context.Background(), stubInfo(0))

	spy := &spyRecreate{nextErr: errConsumerConfigMismatch}
	_, err := c.RebuildAfterStreamRecreated(context.Background(), jetstream.ConsumerConfig{}, spy.fn())
	require.Error(t, err)
	require.ErrorIs(t, err, errConsumerConfigMismatch,
		"the underlying cause must remain in the error chain for diagnostics")
	require.ErrorIs(t, err, types.ErrStreamMissing,
		"all post-hook recovery failures must wrap types.ErrStreamMissing so the observer route fires consistently")
}
