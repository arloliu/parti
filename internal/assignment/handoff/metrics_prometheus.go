package handoff

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// PrometheusRecorder implements MetricsRecorder using Prometheus.
// This is local to the handoff package to avoid coupling with the broader
// metrics collector interface. It can be provided via a future option.
//
// Note: This implementation registers its own metric family under the
// provided namespace and subsystem "handoff".
// If reg is nil, prometheus.DefaultRegisterer is used.
// If namespace is empty, "parti" is used by default.
//
// Safe for concurrent use.
//
// Initial metrics:
//   - handoff_total{result}
//   - handoff_duration_seconds
//   - handoff_phase_duration_seconds{phase}
//   - cas_conflicts_total
//   - claim_store_size
//   - claim_store_stale_total
//   - claim_stale_handoff_reset_total
//   - claim_write_throttled_total
//   - claim_write_throttle_wait_seconds
//
// These cover end-to-end outcomes, latency, and basic KV conflict/health.
type PrometheusRecorder struct {
	reg       prometheus.Registerer
	namespace string

	once sync.Once

	handoffTotal           *prometheus.CounterVec
	handoffDuration        prometheus.Histogram
	phaseDuration          *prometheus.HistogramVec
	casConflicts           prometheus.Counter
	claimStoreSize         prometheus.Gauge
	claimStoreStale        prometheus.Counter
	claimStaleHandoffReset prometheus.Counter
	claimWriteThrottled    prometheus.Counter
	claimWriteThrottleWait prometheus.Histogram
}

// NewPrometheusRecorder creates a new recorder.
func NewPrometheusRecorder(reg prometheus.Registerer, namespace string) *PrometheusRecorder {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	if namespace == "" {
		namespace = "parti"
	}
	return &PrometheusRecorder{reg: reg, namespace: namespace}
}

func (p *PrometheusRecorder) ensure() {
	p.once.Do(func() {
		p.handoffTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "handoff",
			Name:      "total",
			Help:      "Total handoff apply attempts by result.",
		}, []string{"result"})
		p.handoffDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: p.namespace,
			Subsystem: "handoff",
			Name:      "duration_seconds",
			Help:      "Total duration of handoff application in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.005, 1.6, 12),
		})
		p.phaseDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: p.namespace,
			Subsystem: "handoff",
			Name:      "phase_duration_seconds",
			Help:      "Duration of handoff phases in seconds.",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2},
		}, []string{"phase"})
		p.casConflicts = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "handoff",
			Name:      "cas_conflicts_total",
			Help:      "Total number of CAS conflict events in claim store updates.",
		})
		p.claimStoreSize = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: p.namespace,
			Subsystem: "handoff",
			Name:      "claim_store_size",
			Help:      "Current number of claim records in the store.",
		})
		p.claimStoreStale = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "handoff",
			Name:      "claim_store_stale_total",
			Help:      "Total number of stale claim records detected.",
		})
		p.claimStaleHandoffReset = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "handoff",
			Name:      "claim_stale_handoff_reset_total",
			Help:      "Total number of stuck-prepare claims reset to stable on re-acquire by the existing owner.",
		})
		p.claimWriteThrottled = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "handoff",
			Name:      "claim_write_throttled_total",
			Help:      "Total claim-write attempts that were delayed by the claim-write rate limiter.",
		})
		p.claimWriteThrottleWait = prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: p.namespace,
			Subsystem: "handoff",
			Name:      "claim_write_throttle_wait_seconds",
			Help:      "Duration in seconds that claim-write attempts waited due to rate limiting.",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
		})

		p.reg.MustRegister(
			p.handoffTotal,
			p.handoffDuration,
			p.phaseDuration,
			p.casConflicts,
			p.claimStoreSize,
			p.claimStoreStale,
			p.claimStaleHandoffReset,
			p.claimWriteThrottled,
			p.claimWriteThrottleWait,
		)
	})
}

func (p *PrometheusRecorder) IncHandoffTotal(result string) {
	p.ensure()
	p.handoffTotal.WithLabelValues(result).Inc()
}

func (p *PrometheusRecorder) ObserveHandoffDuration(d time.Duration) {
	p.ensure()
	p.handoffDuration.Observe(d.Seconds())
}

func (p *PrometheusRecorder) ObservePhaseDuration(phase string, d time.Duration) {
	p.ensure()
	p.phaseDuration.WithLabelValues(phase).Observe(d.Seconds())
}

func (p *PrometheusRecorder) IncCASConflicts() {
	p.ensure()
	p.casConflicts.Inc()
}

func (p *PrometheusRecorder) SetClaimStoreSize(n int) {
	p.ensure()
	p.claimStoreSize.Set(float64(n))
}

func (p *PrometheusRecorder) IncClaimStoreStale() {
	p.ensure()
	p.claimStoreStale.Inc()
}

func (p *PrometheusRecorder) IncClaimStaleHandoffReset() {
	p.ensure()
	p.claimStaleHandoffReset.Inc()
}

// IncClaimWriteThrottled increments the claim-write throttle counter. It
// satisfies the optional claim-write throttle sidecar the Manager type-asserts
// for when building the claim-write rate limiter; only positive-delay waits are
// recorded.
func (p *PrometheusRecorder) IncClaimWriteThrottled() {
	p.ensure()
	p.claimWriteThrottled.Inc()
}

// ObserveClaimWriteThrottleWait records a claim-write throttle wait duration in
// seconds. Companion to IncClaimWriteThrottled.
func (p *PrometheusRecorder) ObserveClaimWriteThrottleWait(seconds float64) {
	p.ensure()
	p.claimWriteThrottleWait.Observe(seconds)
}
