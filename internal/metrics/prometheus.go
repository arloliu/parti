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
	wcIteratorEscalations      *prometheus.CounterVec
	wcConsecutiveFailures      prometheus.Gauge
	wcHealthStatus             prometheus.Gauge
	wcRecreationAttempts       *prometheus.CounterVec
	wcRecreations              *prometheus.CounterVec
	wcRecreationDuration       prometheus.Histogram

	// Manager metrics
	mStateTransitions   *prometheus.CounterVec
	mStateTransitionDur *prometheus.HistogramVec
	mLeadershipChanges  *prometheus.CounterVec
	mDegradedDuration   prometheus.Histogram
	mDegradedMode       prometheus.Gauge
	mCacheAge           prometheus.Gauge
	mAlertLevel         prometheus.Gauge
	mAlertEmitted       *prometheus.CounterVec

	// Calculator metrics
	cRebalanceDuration    *prometheus.HistogramVec
	cRebalanceAttempts    *prometheus.CounterVec
	cPartitionCount       prometheus.Gauge
	cKVDuration           *prometheus.HistogramVec
	cStateChangeDropped   prometheus.Counter
	cEmergencyRebalance   prometheus.Counter
	cEmergencyLastMissing prometheus.Gauge
	cWorkerAdded          prometheus.Counter
	cWorkerRemoved        prometheus.Counter
	cActiveWorkers        prometheus.Gauge
	cCacheUsageAge        *prometheus.HistogramVec
	cCacheFallback        *prometheus.CounterVec
	cOrphanedPartitions   prometheus.Gauge

	// Worker metrics
	wHeartbeats *prometheus.CounterVec

	// Assignment metrics
	aChangesAdded   prometheus.Counter
	aChangesRemoved prometheus.Counter
	aVersion        prometheus.Gauge
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

		p.wcIteratorEscalations = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "worker_consumer",
			Name:      "iterator_escalations_total",
			Help:      "Total iterator escalation events that triggered a consumer refresh by reason.",
		}, []string{"reason"})

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

		// Manager metrics
		p.mStateTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "manager",
			Name:      "state_transitions_total",
			Help:      "Total manager state transitions by from/to.",
		}, []string{"from", "to"})
		p.mStateTransitionDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: p.namespace,
			Subsystem: "manager",
			Name:      "state_transition_duration_seconds",
			Help:      "Duration of manager state transitions in seconds.",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
		}, []string{"from", "to"})
		p.mLeadershipChanges = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "manager",
			Name:      "leadership_changes_total",
			Help:      "Total leadership change events.",
		}, []string{"leader"})
		p.mDegradedDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: p.namespace,
			Subsystem: "manager",
			Name:      "degraded_duration_seconds",
			Help:      "Duration spent in degraded mode in seconds.",
			Buckets:   []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
		})
		p.mDegradedMode = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: p.namespace,
			Subsystem: "manager",
			Name:      "degraded_mode",
			Help:      "Current degraded mode status (1=degraded,0=normal).",
		})
		p.mCacheAge = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: p.namespace,
			Subsystem: "manager",
			Name:      "cache_age_seconds",
			Help:      "Age of cached assignment data in seconds.",
		})
		p.mAlertLevel = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: p.namespace,
			Subsystem: "manager",
			Name:      "alert_level",
			Help:      "Current alert level (0-4).",
		})
		p.mAlertEmitted = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "manager",
			Name:      "alerts_emitted_total",
			Help:      "Total alerts emitted by level.",
		}, []string{"level"})

		// Calculator metrics
		p.cRebalanceDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: p.namespace,
			Subsystem: "calculator",
			Name:      "rebalance_duration_seconds",
			Help:      "Rebalance duration in seconds by reason.",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
		}, []string{"reason"})
		p.cRebalanceAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "calculator",
			Name:      "rebalance_attempts_total",
			Help:      "Rebalance attempts by reason/result.",
		}, []string{"reason", "result"})
		p.cPartitionCount = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: p.namespace,
			Subsystem: "calculator",
			Name:      "partition_count",
			Help:      "Current partition count managed by calculator.",
		})
		p.cKVDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: p.namespace,
			Subsystem: "calculator",
			Name:      "kv_operation_duration_seconds",
			Help:      "NATS KV operation latency in seconds by operation.",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
		}, []string{"operation"})
		p.cStateChangeDropped = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "calculator",
			Name:      "state_change_dropped_total",
			Help:      "Total state change notifications dropped due to slow subscribers.",
		})
		p.cEmergencyRebalance = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "calculator",
			Name:      "emergency_rebalance_total",
			Help:      "Total emergency rebalance triggers.",
		})
		p.cEmergencyLastMissing = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: p.namespace,
			Subsystem: "calculator",
			Name:      "emergency_last_missing_workers",
			Help:      "Last observed number of disappeared workers triggering emergency.",
		})
		p.cWorkerAdded = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "calculator",
			Name:      "workers_added_total",
			Help:      "Total workers added detected by calculator.",
		})
		p.cWorkerRemoved = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "calculator",
			Name:      "workers_removed_total",
			Help:      "Total workers removed detected by calculator.",
		})
		p.cActiveWorkers = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: p.namespace,
			Subsystem: "calculator",
			Name:      "active_workers",
			Help:      "Current active worker count.",
		})
		p.cCacheUsageAge = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: p.namespace,
			Subsystem: "calculator",
			Name:      "cache_usage_age_seconds",
			Help:      "Observed age of cached data used, by cache type.",
			Buckets:   []float64{0, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
		}, []string{"cache_type"})
		p.cCacheFallback = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "calculator",
			Name:      "cache_fallback_total",
			Help:      "Total cache fallback events by reason.",
		}, []string{"reason"})
		p.cOrphanedPartitions = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: p.namespace,
			Subsystem: "calculator",
			Name:      "orphaned_partitions",
			Help:      "Current number of orphaned partitions (unassigned).",
		})

		// Worker metrics
		p.wHeartbeats = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "worker",
			Name:      "heartbeats_total",
			Help:      "Total worker heartbeats by worker_id and result.",
		}, []string{"worker_id", "result"})

		// Assignment metrics
		p.aChangesAdded = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "assignment",
			Name:      "added_total",
			Help:      "Total partitions added in assignment changes.",
		})
		p.aChangesRemoved = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: p.namespace,
			Subsystem: "assignment",
			Name:      "removed_total",
			Help:      "Total partitions removed in assignment changes.",
		})
		p.aVersion = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: p.namespace,
			Subsystem: "assignment",
			Name:      "version",
			Help:      "Current assignment version.",
		})

		// Register new collectors
		p.reg.MustRegister(p.mStateTransitions)
		p.reg.MustRegister(p.mStateTransitionDur)
		p.reg.MustRegister(p.mLeadershipChanges)
		p.reg.MustRegister(p.mDegradedDuration)
		p.reg.MustRegister(p.mDegradedMode)
		p.reg.MustRegister(p.mCacheAge)
		p.reg.MustRegister(p.mAlertLevel)
		p.reg.MustRegister(p.mAlertEmitted)

		p.reg.MustRegister(p.cRebalanceDuration)
		p.reg.MustRegister(p.cRebalanceAttempts)
		p.reg.MustRegister(p.cPartitionCount)
		p.reg.MustRegister(p.cKVDuration)
		p.reg.MustRegister(p.cStateChangeDropped)
		p.reg.MustRegister(p.cEmergencyRebalance)
		p.reg.MustRegister(p.cEmergencyLastMissing)
		p.reg.MustRegister(p.cWorkerAdded)
		p.reg.MustRegister(p.cWorkerRemoved)
		p.reg.MustRegister(p.cActiveWorkers)
		p.reg.MustRegister(p.cCacheUsageAge)
		p.reg.MustRegister(p.cCacheFallback)
		p.reg.MustRegister(p.cOrphanedPartitions)

		p.reg.MustRegister(p.wHeartbeats)

		p.reg.MustRegister(p.aChangesAdded)
		p.reg.MustRegister(p.aChangesRemoved)
		p.reg.MustRegister(p.aVersion)
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
func (p *PrometheusCollector) IncrementWorkerConsumerIteratorEscalation(reason string) {
	p.ensureRegistered()
	p.wcIteratorEscalations.WithLabelValues(reason).Inc()
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

// ManagerMetrics implementation

func (p *PrometheusCollector) RecordStateTransition(from, to types.State, duration float64) {
	p.ensureRegistered()
	p.mStateTransitions.WithLabelValues(from.String(), to.String()).Inc()
	p.mStateTransitionDur.WithLabelValues(from.String(), to.String()).Observe(duration)
}

func (p *PrometheusCollector) RecordLeadershipChange(newLeader string) {
	p.ensureRegistered()
	p.mLeadershipChanges.WithLabelValues(newLeader).Inc()
}

func (p *PrometheusCollector) RecordDegradedDuration(duration float64) {
	p.ensureRegistered()
	p.mDegradedDuration.Observe(duration)
}

func (p *PrometheusCollector) SetDegradedMode(degraded float64) {
	p.ensureRegistered()
	p.mDegradedMode.Set(degraded)
}

func (p *PrometheusCollector) SetCacheAge(age float64) {
	p.ensureRegistered()
	p.mCacheAge.Set(age)
}

func (p *PrometheusCollector) SetAlertLevel(level int) {
	p.ensureRegistered()
	p.mAlertLevel.Set(float64(level))
}

func (p *PrometheusCollector) IncrementAlertEmitted(level string) {
	p.ensureRegistered()
	p.mAlertEmitted.WithLabelValues(level).Inc()
}

// CalculatorMetrics implementation

func (p *PrometheusCollector) RecordRebalanceDuration(duration float64, reason string) {
	p.ensureRegistered()
	p.cRebalanceDuration.WithLabelValues(reason).Observe(duration)
}

func (p *PrometheusCollector) RecordRebalanceAttempt(reason string, success bool) {
	p.ensureRegistered()
	result := "failure"
	if success {
		result = "success"
	}
	p.cRebalanceAttempts.WithLabelValues(reason, result).Inc()
}

func (p *PrometheusCollector) RecordPartitionCount(count int) {
	p.ensureRegistered()
	p.cPartitionCount.Set(float64(count))
}

func (p *PrometheusCollector) RecordKVOperationDuration(operation string, duration float64) {
	p.ensureRegistered()
	p.cKVDuration.WithLabelValues(operation).Observe(duration)
}

func (p *PrometheusCollector) RecordStateChangeDropped() {
	p.ensureRegistered()
	p.cStateChangeDropped.Inc()
}

func (p *PrometheusCollector) RecordEmergencyRebalance(disappearedWorkers int) {
	p.ensureRegistered()
	p.cEmergencyRebalance.Inc()
	p.cEmergencyLastMissing.Set(float64(disappearedWorkers))
}

func (p *PrometheusCollector) RecordWorkerChange(added, removed int) {
	p.ensureRegistered()
	if added > 0 {
		p.cWorkerAdded.Add(float64(added))
	}
	if removed > 0 {
		p.cWorkerRemoved.Add(float64(removed))
	}
}

// RecordOrphanedPartitions records the number of orphaned partitions.
func (p *PrometheusCollector) RecordOrphanedPartitions(count int) {
	p.ensureRegistered()
	p.cOrphanedPartitions.Set(float64(count))
}

func (p *PrometheusCollector) RecordActiveWorkers(count int) {
	p.ensureRegistered()
	p.cActiveWorkers.Set(float64(count))
}

func (p *PrometheusCollector) RecordCacheUsage(cacheType string, age float64) {
	p.ensureRegistered()
	p.cCacheUsageAge.WithLabelValues(cacheType).Observe(age)
}

func (p *PrometheusCollector) IncrementCacheFallback(reason string) {
	p.ensureRegistered()
	p.cCacheFallback.WithLabelValues(reason).Inc()
}

// WorkerMetrics implementation

func (p *PrometheusCollector) RecordHeartbeat(workerID string, success bool) {
	p.ensureRegistered()
	result := "failure"
	if success {
		result = "success"
	}
	p.wHeartbeats.WithLabelValues(workerID, result).Inc()
}

// AssignmentMetrics implementation

func (p *PrometheusCollector) RecordAssignmentChange(added, removed int, version int64) {
	p.ensureRegistered()
	if added > 0 {
		p.aChangesAdded.Add(float64(added))
	}
	if removed > 0 {
		p.aChangesRemoved.Add(float64(removed))
	}
	p.aVersion.Set(float64(version))
}
