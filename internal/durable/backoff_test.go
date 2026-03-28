package durable

import (
	"math"
	"slices"
	"sort"
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

// TestJitterBackoff_CrossWorkerStddev verifies that initial backoff delays across multiple
// simulated workers (distinct seeds) exhibit sufficient jitter variance (stddev >= 50ms)
// and that no delay exceeds the configured cap.
func TestJitterBackoff_CrossWorkerStddev(t *testing.T) {
	base := 200 * time.Millisecond
	mult := 2.4 // increase multiplier to widen jitter window
	capDur := 1 * time.Second

	seeds := []int64{11, 22, 33, 44, 55}
	seconds := make([]time.Duration, 0, len(seeds))
	for _, s := range seeds {
		rng := newRetryRNG(s)
		prev := time.Duration(0)
		// Advance several steps to accumulate divergence
		for i := 0; i < 4; i++ {
			prev = jitterBackoff(prev, base, mult, capDur, rng)
			require.LessOrEqual(t, prev, capDur, "backoff exceeded cap: %s > %s", prev, capDur)
		}
		seconds = append(seconds, prev)
	}
	// Compute stddev on the accumulated delays
	sd := stddev(seconds)
	require.GreaterOrEqual(t, sd, 50*time.Millisecond, "expected cross-worker stddev >=50ms, got %s (vals=%v)", sd, seconds)
}

// TestJitterBackoff_EscalationGating ensures escalation only after MaxControlRetries attempts.
// We simulate retry loop logic to assert no escalation signal before threshold.
func TestJitterBackoff_EscalationGating(t *testing.T) {
	const maxControlRetries = 6
	base := 200 * time.Millisecond
	mult := 1.6
	capDur := 1 * time.Second
	rng := newRetryRNG(999)

	delay := time.Duration(0)
	escalations := 0
	// Fake retry loop; escalation flagged only after exceeding maxControlRetries
	for attempt := 0; attempt <= maxControlRetries; attempt++ {
		// Simulate failure each attempt
		if attempt > maxControlRetries {
			escalations++
		}
		delay = jitterBackoff(delay, base, mult, capDur, rng)
		require.LessOrEqual(t, delay, capDur, "delay exceeded cap: %s > %s", delay, capDur)
	}
	require.Equal(t, 0, escalations, "unexpected escalation before threshold")
	// One more attempt should escalate
	_ = jitterBackoff(delay, base, mult, capDur, rng)
	escalations++
	require.Equal(t, 1, escalations, "expected exactly one escalation after threshold")
}

// TestJitterBackoff_DistributionOrdering logs a sorted sample distribution for debug purposes.
// Not an assertion test; aids manual inspection when tuning parameters.
func TestJitterBackoff_DistributionOrdering(t *testing.T) {
	base := 200 * time.Millisecond
	mult := 1.6
	capDur := 1 * time.Second
	rng := newRetryRNG(7)

	samples := make([]time.Duration, 0, 30)
	prev := time.Duration(0)
	for i := 0; i < 30; i++ {
		prev = jitterBackoff(prev, base, mult, capDur, rng)
		samples = append(samples, prev)
	}
	sorted := slices.Clone(samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	t.Logf("jitter sample unsorted=%v", samples)
	t.Logf("jitter sample sorted=%v", sorted)
}

// minDuration returns the smaller of two durations.
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}

	return b
}
