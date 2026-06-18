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

// stubInfo returns a stub InfoFunc that yields a ConsumerInfo with the given
// AckFloor.Stream. Used to drive SeedCheckpoint deterministically without a
// live NATS connection.
func stubInfo(ackFloor uint64) InfoFunc {
	return func(context.Context) (*jetstream.ConsumerInfo, error) {
		return &jetstream.ConsumerInfo{
			AckFloor: jetstream.SequenceInfo{Stream: ackFloor},
		}, nil
	}
}

// T2c — epoch fence. The tightest invariant in the stream-missing recovery
// design: a tracking msg captured under one stream incarnation must NOT
// re-raise the checkpoint when its Ack lands after HandleStreamRecreated
// has bumped the streamEpoch.
//
// Scenario:
//   - Strategy=FromLastProcessed. Checkpoint seeded to 100.
//   - WrapForTracking(seq=80) captures the current epoch (call it E0).
//   - HandleStreamRecreated() bumps the epoch (E0 → E1), resets the
//     checkpoint to 0, seeds from a stub returning AckFloor=0.
//   - The held wrapper's Ack() fires after the bump.
//
// Invariant: Checkpoint.Value() stays at 0. Without the fence,
// AdvanceCheckpoint would push it back to 80 (the wrapper's captured
// seq), which is exactly the fresh-stream skip hazard P2.3 must prevent.
func TestController_EpochFence_LateAckDroppedAfterStreamRecreated(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})
	require.NotNil(t, c)

	c.checkpoint.Seed(100)
	require.Equal(t, uint64(100), c.checkpoint.Value(), "precondition: checkpoint seeded to 100")

	// Wrap a message at seq 80. This captures the controller's current
	// streamEpoch at dispatch time (the load-bearing semantic).
	heldMsg := &mockMsg{seq: 80}
	heldWrapped := c.WrapForTracking(heldMsg)
	_, ok := heldWrapped.(*trackingMsg)
	require.True(t, ok, "FromLastProcessed must produce a *trackingMsg")

	// Stream is recreated. Epoch bumps; checkpoint resets to 0; seed
	// reads AckFloor=0 (fresh stream — no replay floor).
	c.HandleStreamRecreated(context.Background(), stubInfo(0))
	require.Equal(t, uint64(0), c.checkpoint.Value(),
		"HandleStreamRecreated must reset checkpoint to 0 for a fresh stream")

	// The held wrapper still references the OLD stream's epoch. Ack must
	// be fenced — Msg.Ack succeeds (no error from the mock), but the
	// checkpoint MUST NOT advance.
	err := heldWrapped.Ack()
	require.NoError(t, err)

	require.Equal(t, uint64(0), c.checkpoint.Value(),
		"epoch fence MUST drop the stale ack; checkpoint must stay at 0, not advance to 80")
}

// T2c — DoubleAck variant. Same invariant via the DoubleAck path. Both
// Ack and DoubleAck must consult the same fence; a regression in either
// would silently re-raise the checkpoint after stream recreation.
func TestController_EpochFence_LateDoubleAckDroppedAfterStreamRecreated(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})
	require.NotNil(t, c)

	c.checkpoint.Seed(100)

	heldMsg := &mockMsg{seq: 80}
	heldWrapped := c.WrapForTracking(heldMsg)

	c.HandleStreamRecreated(context.Background(), stubInfo(0))

	err := heldWrapped.DoubleAck(context.Background())
	require.NoError(t, err)

	require.Equal(t, uint64(0), c.checkpoint.Value(),
		"epoch fence must drop a stale DoubleAck as well as a stale Ack")
}

// T2c — current-epoch acks still advance. The fence is generation-based,
// not a blanket disable: a msg wrapped AFTER HandleStreamRecreated captures
// the new epoch and its Ack must advance the checkpoint normally.
func TestController_EpochFence_CurrentEpochAckStillAdvances(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})
	require.NotNil(t, c)

	// Recreate first, then wrap a "new" message after the bump.
	c.HandleStreamRecreated(context.Background(), stubInfo(0))
	require.Equal(t, uint64(0), c.checkpoint.Value())

	newMsg := &mockMsg{seq: 5}
	newWrapped := c.WrapForTracking(newMsg)

	require.NoError(t, newWrapped.Ack())
	require.Equal(t, uint64(5), c.checkpoint.Value(),
		"a msg wrapped after the epoch bump must advance the checkpoint normally")
}

// T2c-ordering — deterministic test that bump-before-reset is the only
// safe ordering. Replaces a v1 sleep/log-based fragile test. Uses the
// package-internal handleStreamRecreatedWithSteps seam to inject a held
// old-epoch Ack at the "after_reset" step.
//
// If the implementation does bump → reset (correct), the epoch was
// already bumped by the time the seam fires, so the old-epoch Ack is
// fenced and the checkpoint stays at 0.
//
// If the implementation did reset → bump (broken), at "after_reset" the
// epoch would still match the held wrapper's captured value, and the
// Ack would slip past the fence and advance the just-zeroed checkpoint
// past zero.
//
// The assertion (checkpoint stays at 0 across the seam) deterministically
// distinguishes correct vs broken ordering with no sleeps or scheduling
// assumptions.
func TestController_HandleStreamRecreated_BumpBeforeReset_DeterministicOrdering(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})
	require.NotNil(t, c)

	c.checkpoint.Seed(100)

	heldMsg := &mockMsg{seq: 80}
	heldWrapped := c.WrapForTracking(heldMsg)

	// onStep injects the held Ack at "after_reset". Under the correct
	// bump-before-reset ordering, the epoch is already bumped here, so
	// the fence drops the Ack. Under the broken reset-before-bump
	// ordering, the epoch is still the captured one and the Ack would
	// advance the checkpoint past zero.
	// Track every step the implementation reports so a typo in the
	// production step name desyncs the test rather than silently no-oping.
	var seenSteps []string
	c.handleStreamRecreatedWithSteps(context.Background(), stubInfo(0), func(step string) {
		seenSteps = append(seenSteps, step)
		if step == streamRecreatedStepAfterReset {
			_ = heldWrapped.Ack()
		}
	})
	require.Equal(t,
		[]string{
			streamRecreatedStepAfterBump,
			streamRecreatedStepAfterReset,
			streamRecreatedStepAfterSeed,
			streamRecreatedStepAfterFlag,
		},
		seenSteps,
		"the seam must report every step in the spec-locked order; a missing or renamed step would silently skip the ack-injection branch and mask a regression")

	require.Equal(t, uint64(0), c.checkpoint.Value(),
		"bump-before-reset ordering required: an old-epoch Ack injected at after_reset must be fenced; checkpoint must stay at 0")
}

// --- merged from controller_recreated_flag_test.go ---

// T6 — recreatedSinceLastBuild is one-shot. The first post-hook rebuild
// must consume the flag (emit DeliverAllPolicy when checkpoint==0). A
// SECOND rebuild without a fresh HandleStreamRecreated must NOT re-use
// the override; with checkpoint still 0 it must fall back to
// DeliverNewPolicy (the existing "no checkpoint" rule).
//
// The v1 spec masked this by advancing checkpoint > 0 before the second
// recovery (checkpoint > 0 wins inside BuildConfig before the flag is
// consulted, so a stuck flag would be invisible). v2 drives the second
// rebuild with checkpoint still at 0 to make the assertion load-bearing.
func TestController_RecreatedFlag_OneShot_NotReusedOnSecondRebuild(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})
	require.NotNil(t, c)

	c.checkpoint.Seed(100)
	c.HandleStreamRecreated(context.Background(), stubInfo(0))
	require.Equal(t, uint64(0), c.checkpoint.Value())

	// First rebuild: consumes the flag, emits DeliverAllPolicy.
	spy1 := &spyRecreate{}
	_, err := c.RebuildAfterStreamRecreated(context.Background(), jetstream.ConsumerConfig{}, spy1.fn())
	require.NoError(t, err)
	require.Len(t, spy1.calls, 1)
	require.Equal(t, jetstream.DeliverAllPolicy, spy1.calls[0].DeliverPolicy,
		"first post-hook rebuild must consume the recreated flag and emit DeliverAllPolicy")

	// Reset the checkpoint back to 0 to simulate "no progress since
	// the prior rebuild" — keeps the test load-bearing on the flag,
	// not on the checkpoint>0 branch.
	c.checkpoint.ResetForStreamRecreate()
	require.Equal(t, uint64(0), c.checkpoint.Value())

	// Second rebuild without a fresh HandleStreamRecreated. The flag
	// must already be cleared. With checkpoint==0 and the override
	// gone, BuildConfig must fall into the "no checkpoint" branch.
	spy2 := &spyRecreate{}
	_, err = c.RebuildAfterStreamRecreated(context.Background(), jetstream.ConsumerConfig{}, spy2.fn())
	require.NoError(t, err)
	require.Len(t, spy2.calls, 1)
	require.Equal(t, jetstream.DeliverNewPolicy, spy2.calls[0].DeliverPolicy,
		"second rebuild without a fresh HandleStreamRecreated must NOT re-use DeliverAllPolicy; flag must be cleared by the first rebuild")
}

// T6b — direct read-and-clear primitive. Pins the atomic.Bool.Swap
// semantic of recreatedSinceLastBuild independently of BuildConfig: a
// regression that mistakenly used Load instead of Swap would still
// emit the right policy on the first rebuild (the flag is true) but
// leak the flag forever, breaking T6's second-call invariant. T6b
// catches that primitive-level regression directly.
func TestController_RecreatedFlag_ReadAndClear_Primitive(t *testing.T) {
	c := NewController(ControllerConfig{Strategy: FromLastProcessed, Logger: logging.NewNop()})
	require.NotNil(t, c)

	c.HandleStreamRecreated(context.Background(), stubInfo(0))
	require.True(t, c.recreatedSinceLastBuild.Load(),
		"HandleStreamRecreated must set recreatedSinceLastBuild")

	// First Swap must read true and clear.
	require.True(t, c.recreatedSinceLastBuild.Swap(false))

	// Second Swap must read false (cleared by the first).
	require.False(t, c.recreatedSinceLastBuild.Swap(false),
		"the flag must be cleared by the first read-and-clear; no leak")
}

// --- merged from controller_recreated_replay_test.go ---

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
