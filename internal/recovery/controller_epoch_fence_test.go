package recovery

import (
	"context"
	"testing"

	"github.com/arloliu/parti/v2/internal/logging"
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
