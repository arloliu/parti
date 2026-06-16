package parti

import "github.com/arloliu/parti/v2/internal/ratelimit"

// claimWriteThrottleObserver is an optional sidecar interface that a
// HandoffMetricsRecorder implementation MAY satisfy. When the configured
// handoff metrics value implements it, claim-write throttle events are emitted
// through it.
//
// It is intentionally kept off the public HandoffMetricsRecorder interface (the
// same D7 pattern the consumer-create limiter uses for WorkerConsumerMetrics)
// so external recorders that implement only the published interface are
// unaffected — they simply never receive throttle calls. The built-in
// handoff.PrometheusRecorder implements it.
//
// Only positive-delay waits are recorded; a claim-write that draws an
// immediately-available token does not trigger these methods.
type claimWriteThrottleObserver interface {
	// IncClaimWriteThrottled increments the count of claim-writes that were
	// actually delayed by the rate limiter.
	IncClaimWriteThrottled()
	// ObserveClaimWriteThrottleWait records the actual wait duration in seconds.
	ObserveClaimWriteThrottleWait(seconds float64)
}

// claimWriteThrottleAdapter bridges a claimWriteThrottleObserver to the
// ratelimit.ThrottleObserver the limiter calls on every positive-delay wait.
type claimWriteThrottleAdapter struct {
	obs claimWriteThrottleObserver
}

var _ ratelimit.ThrottleObserver = claimWriteThrottleAdapter{}

func (a claimWriteThrottleAdapter) IncrementThrottled() { a.obs.IncClaimWriteThrottled() }
func (a claimWriteThrottleAdapter) ObserveWait(seconds float64) {
	a.obs.ObserveClaimWriteThrottleWait(seconds)
}

// buildClaimWriteLimiter constructs the per-worker claim-write rate limiter from
// cfg.Handoff, or returns nil (unlimited) when ClaimWritePerSec is not positive.
//
// The same limiter is threaded into the two-phase coordinator and used directly
// by the startup hygiene/resume loops, so every claim-write this worker issues
// shares one rate budget. Validation (perSec >= 0, burst >= 1 when perSec > 0)
// is enforced at Config.Validate; this constructs from already-valid values.
//
// When the configured handoff metrics recorder implements
// claimWriteThrottleObserver, a throttle observer is wired so positive-delay
// waits surface as metrics. The type assertion happens once here, not per wait.
func (m *Manager) buildClaimWriteLimiter() ratelimit.Limiter {
	if m.cfg.Handoff.ClaimWritePerSec <= 0 {
		return nil
	}

	var obs ratelimit.ThrottleObserver
	if sidecar, ok := m.handoffMetrics.(claimWriteThrottleObserver); ok {
		obs = claimWriteThrottleAdapter{obs: sidecar}
	}

	return ratelimit.New(m.cfg.Handoff.ClaimWritePerSec, m.cfg.Handoff.ClaimWriteBurst, obs)
}
