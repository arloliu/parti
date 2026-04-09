package recovery

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBurstDetector_ThresholdReached(t *testing.T) {
	bd := NewBurstDetector(10*time.Second, 3)

	require.False(t, bd.Record(), "1st failure: below threshold")
	require.False(t, bd.Record(), "2nd failure: below threshold")
	require.True(t, bd.Record(), "3rd failure: threshold reached")
	require.True(t, bd.Record(), "4th failure: still above threshold")
}

func TestBurstDetector_Reset(t *testing.T) {
	bd := NewBurstDetector(10*time.Second, 2)

	bd.Record()
	bd.Record()
	bd.Reset()

	require.False(t, bd.Record(), "after reset, 1st failure: below threshold")
	require.True(t, bd.Record(), "2nd failure: threshold reached again")
}

func TestTrimTimes(t *testing.T) {
	now := time.Now()
	times := []time.Time{
		now.Add(-5 * time.Second),
		now.Add(-3 * time.Second),
		now.Add(-1 * time.Second),
		now,
	}

	result := trimTimes(times, now.Add(-2*time.Second))
	require.Len(t, result, 2) // only the last 2 entries within window
}
