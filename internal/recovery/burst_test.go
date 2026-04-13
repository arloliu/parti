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

// TestDefaultBurstWindow_UsesConfiguredThreshold verifies that defaultBurstWindow
// respects the supplied threshold rather than the package-level constant.
// Before the fix, the function hardcoded defaultBurstThreshold (3), so any
// custom threshold produced the wrong window.
func TestDefaultBurstWindow_UsesConfiguredThreshold(t *testing.T) {
	fetch := time.Second

	// threshold=3 (the default): window = 1s*(3+1)+3s = 7s
	require.Equal(t, 7*time.Second, defaultBurstWindow(fetch, 3))

	// threshold=5 (custom): window = 1s*(5+1)+3s = 9s
	// Old code would return 7s here (using hardcoded 3).
	require.Equal(t, 9*time.Second, defaultBurstWindow(fetch, 5))

	// threshold=1 (below default): window = 1s*(1+1)+3s = 5s
	// Old code would return 7s here too.
	require.Equal(t, 5*time.Second, defaultBurstWindow(fetch, 1))
}

// TestNewController_BurstWindow_DerivedFromThreshold verifies that NewController
// auto-derives BurstWindow using the resolved BurstThreshold, not the default
// constant. A caller that sets BurstThreshold=5 but omits BurstWindow must get
// FetchTimeout*(5+1)+3s, not FetchTimeout*(3+1)+3s.
func TestNewController_BurstWindow_DerivedFromThreshold(t *testing.T) {
	fetch := time.Second

	ctrl := NewController(ControllerConfig{
		Strategy:       FromNew,
		FetchTimeout:   fetch,
		BurstThreshold: 5,
		// BurstWindow intentionally zero — must be auto-derived.
	})
	require.NotNil(t, ctrl)

	// Expected: fetch*(5+1)+3s = 9s
	require.Equal(t, 9*time.Second, ctrl.burst.window,
		"auto-derived BurstWindow must use the configured BurstThreshold")
	require.Equal(t, 5, ctrl.burst.threshold)
}

func TestTrimTimes(t *testing.T) {
	now := time.Now()
	times := []time.Time{
		now.Add(-5 * time.Second),
		now.Add(-3 * time.Second),
		now.Add(-1 * time.Second),
		now,
	}

	result := TrimTimes(times, now.Add(-2*time.Second))
	require.Len(t, result, 2) // only the last 2 entries within window
}
