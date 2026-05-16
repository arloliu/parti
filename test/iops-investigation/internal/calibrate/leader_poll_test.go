// Tests for PollLeader + LeaderMoved that simulate leadership transitions
// without an embedded NATS cluster. The LeaderLookup function-type seam is
// the entire reason we can cover the move-away-move-back case here: the
// real stream.Info path requires a multi-node cluster, which is exactly
// the environment our test sandbox cannot provide.
package calibrate

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLeaderMoved_DetectsMoveAwayMoveBack is the P1 regression: a leader
// that leaves and returns during the capture window must still be
// classified as moved. Pre/post comparison alone (baseline == final) can
// not see this; LeaderMoved looks at every sample.
func TestLeaderMoved_DetectsMoveAwayMoveBack(t *testing.T) {
	baseline := "nats-1"
	samples := []string{"nats-1", "nats-2", "nats-1"} // away and back
	require.True(t, LeaderMoved(baseline, samples),
		"any sample != baseline must count as a move, even if final sample matches baseline")
}

func TestLeaderMoved_StableLeader(t *testing.T) {
	require.False(t, LeaderMoved("nats-1", []string{"nats-1", "nats-1", "nats-1"}))
	require.False(t, LeaderMoved("nats-1", nil))
}

func TestLeaderMoved_ErrorSentinelCountsAsMove(t *testing.T) {
	// An indeterminate sample is treated as a move per Rule 10 (fail
	// loud): we'd rather discard a point than silently accept it.
	require.True(t, LeaderMoved("nats-1", []string{"nats-1", "<error>", "nats-1"}))
}

// TestPollLeader_CapturesAllTicks_Then_LeaderMovedFiresEvenWhenFinalMatches
// is the headline P1-2 unit test: simulate a leader that briefly moves
// during the window and verify the polling helper observes it.
func TestPollLeader_CapturesAllTicks_Then_LeaderMovedFiresEvenWhenFinalMatches(t *testing.T) {
	// Sequence per Lookup call: nats-1, nats-2, nats-1
	var calls atomic.Int64
	leaders := []string{"nats-1", "nats-2", "nats-1"}
	lookup := func(ctx context.Context) (string, error) {
		n := calls.Add(1)
		idx := int(n-1) % len(leaders)
		return leaders[idx], nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	samples := PollLeader(ctx, 30*time.Millisecond, lookup)
	require.GreaterOrEqual(t, len(samples), 3,
		"expected at least three polls in 200ms at 30ms cadence")
	require.True(t, LeaderMoved("nats-1", samples),
		"PollLeader must surface the transient nats-2 sample")
}

// TestPollLeader_PropagatesLookupErrorsAsSentinel ensures we don't lose
// indeterminate readings — they become the "<error>" string so the
// downstream LeaderMoved check fires.
func TestPollLeader_PropagatesLookupErrorsAsSentinel(t *testing.T) {
	lookup := func(ctx context.Context) (string, error) {
		return "", errors.New("simulated stream.Info failure")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	samples := PollLeader(ctx, 20*time.Millisecond, lookup)
	require.GreaterOrEqual(t, len(samples), 1)
	for _, s := range samples {
		require.Equal(t, "<error>", s)
	}
	require.True(t, LeaderMoved("nats-1", samples))
}

// TestPollLeader_NonPositiveIntervalIsNoop guards the documented contract
// in the function header — callers can disable polling by setting the
// interval to zero, leaving only pre/post detection in play.
func TestPollLeader_NonPositiveIntervalIsNoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.Nil(t, PollLeader(ctx, 0, func(context.Context) (string, error) { return "x", nil }))
}
