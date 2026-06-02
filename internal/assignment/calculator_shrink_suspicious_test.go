package assignment

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestShrinkSuspicious pins the shared sharp-shrink predicate used by both the
// worker (workerObservationSuspicious) and partition
// (partitionInputCredibilityGuard) credibility guards. The small-count case is
// the load-bearing one: it discriminates the multiplied form
// (observed*100 < lastKnown*Pct) from the pre-divided form
// (observed < lastKnown*Pct/100), which integer-truncates at small counts and
// would silently miss a real shrink. Both guards delegate here so the two
// cannot drift back to the truncation-prone shape.
func TestShrinkSuspicious(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		observed     int
		lastKnown    int
		thresholdPct int
		want         bool
	}{
		{"drop to exactly the threshold is not suspicious", 5, 10, 50, false}, // 500 < 500 == false
		{"drop below the threshold is suspicious", 4, 10, 50, true},           // 400 < 500
		{"empty observation is suspicious", 0, 10, 50, true},                  // 0 < 500
		{"no drop is not suspicious", 10, 10, 50, false},                      // 1000 < 500 == false
		{"growth is not suspicious", 20, 10, 50, false},                       // 2000 < 500 == false
		// Truncation guard: with the multiplied form a 1-of-3 observation is a
		// >50% drop and suspicious (1*100=100 < 3*50=150). The pre-divided form
		// would compute 1 < (3*50)/100 = 1 < 1 = false and miss it.
		{"small-count truncation: multiplied form catches the drop", 1, 3, 50, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, shrinkSuspicious(tt.observed, tt.lastKnown, tt.thresholdPct))
		})
	}
}
