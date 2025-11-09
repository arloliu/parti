package metrics

import (
	"sync"

	"github.com/arloliu/parti/types"
	"github.com/prometheus/client_golang/prometheus"
)

// PrometheusCollector implements types.MetricsCollector backed by Prometheus.
//
// For now, it provides concrete instrumentation for WorkerConsumer metrics and
// defers other areas to embedded NopMetrics, ensuring full interface coverage
// without forcing immediate instrumentation of all domains.
type PrometheusCollector struct {
	*NopMetrics

	reg       prometheus.Registerer
	namespace string
	once      sync.Once

	// WorkerConsumer metrics
	wcRetryCounter             *prometheus.CounterVec
	wcBackoffHistogram         *prometheus.HistogramVec
	wcSubjectsGauge            prometheus.Gauge
	wcSubjectChanges           *prometheus.CounterVec
	wcGuardrailViolations      *prometheus.CounterVec
	wcSubjectThresholdWarnings prometheus.Counter
	wcUpdateResults            *prometheus.CounterVec
	wcUpdateLatency            prometheus.Histogram
	wcIteratorRestarts         *prometheus.CounterVec
	wcIteratorEscalations      prometheus.Counter
	wcConsecutiveFailures      prometheus.Gauge
	wcHealthStatus             prometheus.Gauge
	wcRecreationAttempts       *prometheus.CounterVec
	wcRecreations              *prometheus.CounterVec
	wcRecreationDuration       prometheus.Histogram
}

// Compile-time assertion that PrometheusCollector implements MetricsCollector.
var _ types.MetricsCollector = (*PrometheusCollector)(nil)

// NewPrometheus creates a new Prometheus-backed metrics collector.
//
// Parameters:
//   - reg: Prometheus registerer interface (uses prometheus.DefaultRegisterer if nil)
//   - namespace: Prometheus metrics namespace (defaults to "parti" if empty)
//
// Returns:
//   - *PrometheusCollector: A MetricsCollector implementation using Prometheus
func NewPrometheus(reg prometheus.Registerer, namespace string) *PrometheusCollector {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	if namespace == "" {
		namespace = "parti"
	}

	return &PrometheusCollector{NopMetrics: NewNop(), reg: reg, namespace: namespace}
}

func (p *PrometheusCollector) ensureRegistered() {
	p.once.Do(func() {
		p.wcRetryCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "control_retries_total",
			Help:      "Total control-plane retry attempts by operation.",
		}, []string{"op"})

		p.wcBackoffHistogram = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "retry_backoff_seconds",
			Help:      "Observed control-plane backoff durations in seconds by operation.",
			Buckets:   []float64{0.05, 0.1, 0.15, 0.25, 0.5, 1, 2, 5},
		}, []string{"op"})

		p.wcSubjectsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "subjects_current",
			Help:      "Current number of filter subjects on the worker consumer.",
		})
		p.wcSubjectChanges = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "subject_changes_total",
			Help:      "Total subject changes by kind (add/remove).",
		}, []string{"kind"})
		p.wcGuardrailViolations = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "guardrail_violations_total",
			Help:      "Total guardrail violations (max_subjects, workerid_mutation).",
		}, []string{"kind"})
		p.wcSubjectThresholdWarnings = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "subject_threshold_warnings_total",
			Help:      "Warnings emitted when subjects near the MaxSubjects threshold.",
		})

		p.wcUpdateResults = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "update_results_total",
			Help:      "Total worker consumer update outcomes (success,failure,noop).",
		}, []string{"result"})

		p.wcUpdateLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "update_latency_seconds",
			Help:      "Latency of worker consumer update operations in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.01, 1.6, 10), // 10ms .. ~1.6s
		})

		p.wcIteratorRestarts = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "iterator_restarts_total",
			Help:      "Total iterator restarts by reason (transient,heartbeat).",
		}, []string{"reason"})

		p.wcIteratorEscalations = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "iterator_escalations_total",
			Help:      "Total iterator escalation events that triggered a consumer refresh.",
		})

		p.wcConsecutiveFailures = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "consecutive_iterator_failures",
			Help:      "Current number of consecutive iterator failures without success.",
		})

		p.wcHealthStatus = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "health_status",
			Help:      "Worker consumer health status (1=healthy,0=unhealthy).",
		})

		p.wcRecreationAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "recreation_attempts_total",
			Help:      "Total attempts to recreate missing/invalid consumer by reason.",
		}, []string{"reason"})

		p.wcRecreations = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "recreations_total",
			Help:      "Recreation outcomes (success|failure) by reason.",
		}, []string{"result", "reason"})

		p.wcRecreationDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "recreation_duration_seconds",
			Help:      "Total duration in seconds of consumer recreation sequences.",
			Buckets:   []float64{0.1, 0.25, 0.5, 1, 2, 5, 10},
		})

		p.reg.MustRegister(p.wcRetryCounter)
		p.reg.MustRegister(p.wcBackoffHistogram)
		p.reg.MustRegister(p.wcSubjectsGauge)
		p.reg.MustRegister(p.wcSubjectChanges)
		p.reg.MustRegister(p.wcGuardrailViolations)
		p.reg.MustRegister(p.wcSubjectThresholdWarnings)
		p.reg.MustRegister(p.wcUpdateResults)
		p.reg.MustRegister(p.wcUpdateLatency)
		p.reg.MustRegister(p.wcIteratorRestarts)
		p.reg.MustRegister(p.wcIteratorEscalations)
		p.reg.MustRegister(p.wcConsecutiveFailures)
		p.reg.MustRegister(p.wcHealthStatus)
		p.reg.MustRegister(p.wcRecreationAttempts)
		p.reg.MustRegister(p.wcRecreations)
		p.reg.MustRegister(p.wcRecreationDuration)
	})
}

// WorkerConsumerMetrics implementation

// IncrementWorkerConsumerControlRetry increments retry attempts for the given op.
func (p *PrometheusCollector) IncrementWorkerConsumerControlRetry(op string) {
	p.ensureRegistered()
	p.wcRetryCounter.WithLabelValues(op).Inc()
}

// RecordWorkerConsumerRetryBackoff observes a backoff delay (seconds) for the given op.
func (p *PrometheusCollector) RecordWorkerConsumerRetryBackoff(op string, seconds float64) {
	p.ensureRegistered()
	p.wcBackoffHistogram.WithLabelValues(op).Observe(seconds)
}

// SetWorkerConsumerSubjectsCurrent sets current subject count.
func (p *PrometheusCollector) SetWorkerConsumerSubjectsCurrent(count int) {
	p.ensureRegistered()
	p.wcSubjectsGauge.Set(float64(count))
}

// IncrementWorkerConsumerSubjectChange increments subject add/remove counts.
func (p *PrometheusCollector) IncrementWorkerConsumerSubjectChange(kind string, count int) {
	p.ensureRegistered()
	p.wcSubjectChanges.WithLabelValues(kind).Add(float64(count))
}

// IncrementWorkerConsumerGuardrailViolation increments violations.
func (p *PrometheusCollector) IncrementWorkerConsumerGuardrailViolation(kind string) {
	p.ensureRegistered()
	p.wcGuardrailViolations.WithLabelValues(kind).Inc()
}

// IncrementWorkerConsumerSubjectThresholdWarning increments threshold warnings.
func (p *PrometheusCollector) IncrementWorkerConsumerSubjectThresholdWarning() {
	p.ensureRegistered()
	p.wcSubjectThresholdWarnings.Inc()
}

// RecordWorkerConsumerUpdate records the update result (success, failure, noop).
func (p *PrometheusCollector) RecordWorkerConsumerUpdate(result string) {
	p.ensureRegistered()
	p.wcUpdateResults.WithLabelValues(result).Inc()
}

// ObserveWorkerConsumerUpdateLatency observes update latency.
func (p *PrometheusCollector) ObserveWorkerConsumerUpdateLatency(seconds float64) {
	p.ensureRegistered()
	p.wcUpdateLatency.Observe(seconds)
}

// IncrementWorkerConsumerIteratorRestart increments iterator restart reason.
func (p *PrometheusCollector) IncrementWorkerConsumerIteratorRestart(reason string) {
	p.ensureRegistered()
	p.wcIteratorRestarts.WithLabelValues(reason).Inc()
}

// IncrementWorkerConsumerIteratorEscalation increments the iterator escalation counter.
func (p *PrometheusCollector) IncrementWorkerConsumerIteratorEscalation() {
	p.ensureRegistered()
	p.wcIteratorEscalations.Inc()
}

// SetWorkerConsumerConsecutiveIteratorFailures sets the consecutive failure gauge.
func (p *PrometheusCollector) SetWorkerConsumerConsecutiveIteratorFailures(count int) {
	p.ensureRegistered()
	p.wcConsecutiveFailures.Set(float64(count))
}

// SetWorkerConsumerHealthStatus sets health status gauge (1 healthy, 0 unhealthy).
func (p *PrometheusCollector) SetWorkerConsumerHealthStatus(healthy bool) {
	p.ensureRegistered()
	if healthy {
		p.wcHealthStatus.Set(1)
	} else {
		p.wcHealthStatus.Set(0)
	}
}

// IncrementWorkerConsumerRecreationAttempt increments recreation attempts.
func (p *PrometheusCollector) IncrementWorkerConsumerRecreationAttempt(reason string) {
	p.ensureRegistered()
	p.wcRecreationAttempts.WithLabelValues(reason).Inc()
}

// RecordWorkerConsumerRecreation records recreation outcome by result & reason.
func (p *PrometheusCollector) RecordWorkerConsumerRecreation(result string, reason string) {
	p.ensureRegistered()
	p.wcRecreations.WithLabelValues(result, reason).Inc()
}

// ObserveWorkerConsumerRecreationDuration observes recreation latency.
func (p *PrometheusCollector) ObserveWorkerConsumerRecreationDuration(seconds float64) {
	p.ensureRegistered()
	p.wcRecreationDuration.Observe(seconds)
}
