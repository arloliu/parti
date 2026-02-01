package consumer

import (
	rand "math/rand/v2"
	"time"
)

func jitterBackoff(prev, base time.Duration, mult float64, capDur time.Duration, rng *rand.Rand) time.Duration {
	if base <= 0 {
		base = 50 * time.Millisecond
	}
	if mult < 1.0 {
		mult = 1.0
	}
	if capDur > 0 && capDur < base {
		return capDur
	}

	if prev <= 0 {
		if capDur > 0 && base > capDur {
			return capDur
		}

		return base
	}

	maxDuration := time.Duration(float64(prev)*mult) - base
	if maxDuration <= 0 {
		maxDuration = base
	}

	var jitter int64
	if rng != nil {
		jitter = rng.Int64N(int64(maxDuration))
	} else {
		jitter = rand.Int64N(int64(maxDuration)) //nolint:gosec // non-crypto backoff jitter
	}

	next := base + time.Duration(jitter)
	if capDur > 0 && next > capDur {
		return capDur
	}

	return next
}

//nolint:gosec
func newRetryRNG(seed int64) *rand.Rand {
	if seed == 0 {
		return nil
	}
	s1 := uint64(seed)
	s2 := s1 ^ 0x9e3779b97f4a7c15

	return rand.New(rand.NewPCG(s1, s2))
}
