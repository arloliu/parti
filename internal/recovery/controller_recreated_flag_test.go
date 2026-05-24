package recovery

import (
	"context"
	"testing"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

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
