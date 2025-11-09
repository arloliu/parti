package subscription

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func stddev(durs []time.Duration) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	// convert to float seconds for stable scale
	vals := make([]float64, len(durs))
	for i, d := range durs {
		vals[i] = float64(d) / float64(time.Second)
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(len(vals))
	var varSum float64
	for _, v := range vals {
		d := v - mean
		varSum += d * d
	}
	variance := varSum / float64(len(vals))
	std := math.Sqrt(variance)

	return time.Duration(std * float64(time.Second))
}

func TestJitterBackoff_BasicBoundsAndCapStickiness(t *testing.T) {
	base := 200 * time.Millisecond
	mult := 1.6
	capDur := 500 * time.Millisecond
	rng := newRetryRNG(42)

	prev := time.Duration(0)
	for i := 0; i < 10; i++ {
		next := jitterBackoff(prev, base, mult, capDur, rng)
		require.GreaterOrEqual(t, next, minDuration(base, capDur))
		require.LessOrEqual(t, next, capDur)
		prev = next
	}

	// When starting from cap, subsequent values must remain <= cap and >= base
	rng2 := newRetryRNG(99)
	prev = capDur
	for i := 0; i < 5; i++ {
		next := jitterBackoff(prev, base, mult, capDur, rng2)
		require.GreaterOrEqual(t, next, base)
		require.LessOrEqual(t, next, capDur)
		prev = next
	}
}

func TestJitterBackoff_CapLessThanBase(t *testing.T) {
	base := 200 * time.Millisecond
	capDur := 100 * time.Millisecond
	mult := 1.6
	rng := newRetryRNG(1)

	next0 := jitterBackoff(0, base, mult, capDur, rng)
	require.Equal(t, capDur, next0)

	next1 := jitterBackoff(base, base, mult, capDur, rng)
	require.Equal(t, capDur, next1)
}

func TestJitterBackoff_VarianceAcrossSeeds(t *testing.T) {
	base := 200 * time.Millisecond
	mult := 1.6
	capDur := 2 * time.Second

	const seeds = 5
	const steps = 12
	lasts := make([]time.Duration, 0, seeds)
	for s := int64(1); s <= seeds; s++ {
		prev := time.Duration(0)
		rng := newRetryRNG(s)
		for i := 0; i < steps; i++ {
			prev = jitterBackoff(prev, base, mult, capDur, rng)
		}
		lasts = append(lasts, prev)
	}

	// Ensure variance across seeds; expect stddev >= ~50ms
	sd := stddev(lasts)
	require.GreaterOrEqual(t, sd, 50*time.Millisecond, "expected stddev >= 50ms across seeds")
}

// minDuration returns the smaller of two durations.
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}

	return b
}
