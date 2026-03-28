package metrics

import (
	"strconv"
	"sync"
	"time"

	"github.com/arloliu/parti/v2/internal/durable"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	promclient "github.com/prometheus/client_model/go"
)

// Collector collects and exposes simulation metrics.
type Collector struct {
	// Protects cached assignment metrics reads/writes
	assignMu sync.RWMutex
	// Message counters (per-partition)
	messagesSentTotal      *prometheus.CounterVec
	messagesReceivedTotal  *prometheus.CounterVec
	messageGapsTotal       prometheus.Counter
	messageDuplicatesTotal prometheus.Counter
	// Per-partition duplicates
	messageDuplicatesByPartition *prometheus.CounterVec

	// Aggregate message counters (label-free for rate calculations)
	messagesSentTotalAggregate     prometheus.Counter
	messagesReceivedTotalAggregate prometheus.Counter

	// Worker metrics
	workersActive           prometheus.Gauge
	partitionsPerWorker     prometheus.Histogram
	workerRebalanceDuration prometheus.Histogram

	// Processing metrics
	messageProcessingDuration prometheus.Histogram
	messageProcessingErrors   prometheus.Counter

	// Chaos metrics
	chaosEventsTotal *prometheus.CounterVec

	// Partition metrics
	partitionWeights *prometheus.GaugeVec

	// System metrics
	goroutinesActive prometheus.Gauge
	memoryUsageBytes prometheus.Gauge

	// Cached values for reporting
	cachedGoroutines    int
	cachedMemoryBytes   uint64
	cachedActiveWorkers int

	// Manual tracking for histogram statistics
	partitionsPerWorkerSum   float64
	partitionsPerWorkerCount int
	rebalanceDurationSum     float64
	rebalanceDurationCount   int
	processingDurationSum    float64
	processingDurationCount  int

	// Gate metrics
	gateNakTotal *prometheus.CounterVec
	gateNakDelay prometheus.Histogram

	// Resolver metrics
	resolverVisibilityLag prometheus.Histogram
	resolverCacheSize     prometheus.Gauge
	resolverUpdatesTotal  *prometheus.CounterVec
	resolverBatchDuration prometheus.Histogram
	resolverBatchSize     prometheus.Histogram
	resolverBatchFlushes  *prometheus.CounterVec

	// Coordinator gauges
	coordinatorInFlight     prometheus.Gauge
	coordinatorPendingHoles prometheus.Gauge

	// Assignment / locality metrics
	assignmentUnassignedPartitions prometheus.Gauge
	assignmentLocalityRatio        prometheus.Gauge
	assignmentStableLocalityRatio  prometheus.Gauge
	assignmentMovedPartitionsTotal prometheus.Counter

	// Delivery robustness metrics
	holeLifetimeSeconds   prometheus.Histogram
	holesHealedTotal      prometheus.Counter
	pendingHoleAgeSeconds prometheus.Histogram
	holesHealedRate       prometheus.Gauge
	disorderDepth         prometheus.Gauge

	// Latency & recovery metrics
	publishToConsumeLatency prometheus.Histogram
	// Per-partition latency histogram
	publishToConsumeLatencyByPartition *prometheus.HistogramVec
	recoveryDurationSeconds            prometheus.Histogram
	coldStartConvergenceSeconds        prometheus.Histogram

	// Cold start completion indicator
	coldStartComplete prometheus.Gauge

	// Catch-up SLO metrics
	workerCatchUpLatencySeconds prometheus.Histogram // latency from recovery start to completion
	workerCatchUpExceedTotal    prometheus.Counter   // number of recoveries exceeding deadline
	workerCatchUpBacklog        prometheus.Gauge     // latest observed initial backlog size (holes) for active recovery contexts
	holeAgeAtRecoverySeconds    prometheus.Histogram // distribution of oldest hole age at recovery start

	// Cached assignment & robustness values for report access
	cachedUnassignedPartitions int
	cachedLocalityRatio        float64
	cachedStableLocalityRatio  float64
	cachedMovedPartitionsTotal int64
	cachedDisorderDepth        int64

	// Tracker / stability audit metrics
	lateMessagesTotal    prometheus.Counter
	lostMessagesTotal    prometheus.Counter
	auditDurationSeconds prometheus.Histogram
	auditFailuresTotal   prometheus.Counter
	holesOpen            prometheus.Gauge

	// feature gates to control high-cardinality series
	enablePerPartitionLatency    bool
	enablePerPartitionDuplicates bool
	bucketCount                  int
	// Bucket-level (hashed) metrics when bucketCount>0
	publishToConsumeLatencyBuckets *prometheus.HistogramVec
	messageDuplicatesByBucket      *prometheus.CounterVec
}

// NewCollector creates a new metrics collector using the global Prometheus
// registry. For tests or multiple independent simulations running in the same
// process, prefer NewCollectorWithRegistry to avoid duplicate registration
// panics.
//
// Returns:
//   - *Collector: Initialized metrics collector registered on the global registry
func NewCollector() *Collector {
	// Backward-compatible default: enable per-partition metrics, no buckets.
	return NewCollectorWithRegistryAndOptions(prometheus.DefaultRegisterer, true, true, 0)
}

// NewCollectorWithRegistry creates a new metrics collector using the provided
// Prometheus registerer. This enables test isolation by supplying a fresh
// registry per test, preventing duplicate metric registration panics when
// constructing multiple collectors in the same process.
//
// Parameters:
//   - reg: Prometheus registerer (typically *prometheus.Registry from prometheus.NewRegistry())
//
// Returns:
//   - *Collector: Initialized metrics collector registered on reg
func NewCollectorWithRegistry(reg prometheus.Registerer) *Collector {
	factory := promauto.With(reg)
	return &Collector{
		messagesSentTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: "simulation_messages_sent_total",
				Help: "Total number of messages sent per partition",
			},
			[]string{"partition"},
		),
		messagesReceivedTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: "simulation_messages_received_total",
				Help: "Total number of messages received per partition",
			},
			[]string{"partition"},
		),
		messagesSentTotalAggregate: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_messages_sent_total_aggregate",
				Help: "Total number of messages sent (aggregate, no labels)",
			},
		),
		messagesReceivedTotalAggregate: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_messages_received_total_aggregate",
				Help: "Total number of messages received (aggregate, no labels)",
			},
		),
		messageGapsTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_message_gaps_total",
				Help: "Total number of message gaps detected",
			},
		),
		messageDuplicatesTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_message_duplicates_total",
				Help: "Total number of duplicate messages detected",
			},
		),
		messageDuplicatesByPartition: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: "simulation_message_duplicates_partition_total",
				Help: "Total number of duplicate messages detected per partition",
			},
			[]string{"partition"},
		),
		workersActive: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_workers_active",
				Help: "Number of active workers",
			},
		),
		partitionsPerWorker: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_partitions_per_worker",
				Help:    "Distribution of partitions per worker",
				Buckets: prometheus.LinearBuckets(0, 10, 20), // 0, 10, 20, ..., 190
			},
		),
		workerRebalanceDuration: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_rebalancing_duration_seconds",
				Help:    "Duration of rebalancing operations",
				Buckets: prometheus.ExponentialBuckets(0.1, 2, 10), // 0.1, 0.2, 0.4, ..., 51.2
			},
		),
		messageProcessingDuration: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_message_processing_duration_seconds",
				Help:    "Message processing duration",
				Buckets: prometheus.ExponentialBuckets(0.001, 2, 10), // 1ms, 2ms, 4ms, ..., 512ms
			},
		),
		messageProcessingErrors: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_message_processing_errors_total",
				Help: "Total number of message processing errors",
			},
		),
		chaosEventsTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: "simulation_chaos_events_total",
				Help: "Total number of chaos events by type",
			},
			[]string{"type"},
		),
		partitionWeights: factory.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "simulation_partition_weights",
				Help: "Weight assigned to each partition",
			},
			[]string{"partition"},
		),
		goroutinesActive: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_goroutines_active",
				Help: "Number of active goroutines",
			},
		),
		memoryUsageBytes: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_memory_usage_bytes",
				Help: "Current memory usage in bytes",
			},
		),

		// Gate metrics
		gateNakTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: "simulation_gate_nak_total",
				Help: "Total number of NAKs issued by processing gate by reason",
			},
			[]string{"reason"},
		),
		gateNakDelay: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_gate_nak_delay_seconds",
				Help:    "Observed NAK delay durations from the processing gate",
				Buckets: prometheus.ExponentialBuckets(0.01, 1.8, 12), // 10ms .. ~10s
			},
		),

		// Resolver metrics
		resolverVisibilityLag: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_resolver_visibility_lag_seconds",
				Help:    "Lag between claim LastUpdated and resolver observation",
				Buckets: prometheus.ExponentialBuckets(0.001, 2, 12), // 1ms .. ~2s
			},
		),
		resolverCacheSize: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_resolver_cache_size",
				Help: "Current number of partitions tracked in resolver cache",
			},
		),
		resolverUpdatesTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: "simulation_resolver_updates_total",
				Help: "Total resolver cache updates by operation",
			},
			[]string{"op"},
		),
		resolverBatchDuration: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_resolver_batch_apply_duration_seconds",
				Help:    "Time to apply a coalesced resolver batch to the cache",
				Buckets: prometheus.ExponentialBuckets(0.0005, 2, 12), // 0.5ms .. ~2s
			},
		),
		resolverBatchSize: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_resolver_batch_size",
				Help:    "Number of unique partition updates per applied batch",
				Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1,2,4,..,2048
			},
		),
		resolverBatchFlushes: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: "simulation_resolver_batch_flush_total",
				Help: "Number of resolver batch flushes by reason",
			},
			[]string{"reason"},
		),

		// Coordinator gauges
		coordinatorInFlight: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_coordinator_inflight_messages",
				Help: "Current count of messages sent but not yet received (in-flight)",
			},
		),
		coordinatorPendingHoles: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_coordinator_pending_holes",
				Help: "Current count of pending holes (missing sequences not yet aged out)",
			},
		),

		// Assignment / locality metrics
		assignmentUnassignedPartitions: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_assignment_unassigned_partitions",
				Help: "Number of partitions currently unassigned to any worker",
			},
		),
		assignmentLocalityRatio: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_assignment_locality_ratio",
				Help: "Ratio of partitions that remained on the same worker after the last rebalance (0-1)",
			},
		),
		assignmentStableLocalityRatio: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_assignment_stable_locality_ratio",
				Help: "Stable locality ratio computed across quiescent snapshots (0-1)",
			},
		),
		assignmentMovedPartitionsTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_assignment_moved_partitions_total",
				Help: "Total number of partitions that have moved between workers across rebalances",
			},
		),

		// Delivery robustness metrics
		holeLifetimeSeconds: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_hole_lifetime_seconds",
				Help:    "Time from first observation of a missing sequence (hole) to its eventual arrival",
				Buckets: prometheus.ExponentialBuckets(0.01, 2, 12), // 10ms .. ~40s
			},
		),
		holesHealedTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_message_holes_healed_total",
				Help: "Total number of previously missing sequences that were later received (healed holes)",
			},
		),
		pendingHoleAgeSeconds: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_pending_hole_age_seconds",
				Help:    "Age distribution of currently pending holes (missing sequences not yet healed or aged out)",
				Buckets: prometheus.ExponentialBuckets(0.01, 2, 14), // 10ms .. ~160s
			},
		),
		holesHealedRate: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_message_holes_healed_rate_per_second",
				Help: "Hole healing rate computed over last coordinator gauge interval (healed per second)",
			},
		),
		disorderDepth: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_disorder_depth",
				Help: "Maximum observed out-of-order depth (high watermark - contiguous received) across partitions",
			},
		),

		// Latency & recovery metrics
		publishToConsumeLatency: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_publish_to_consume_latency_seconds",
				Help:    "End-to-end latency from producer publish timestamp to worker consumption",
				Buckets: prometheus.ExponentialBuckets(0.001, 2, 14), // 1ms .. ~16s
			},
		),
		publishToConsumeLatencyByPartition: factory.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "simulation_publish_to_consume_latency_partition_seconds",
				Help:    "End-to-end latency from publish to consume per partition",
				Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
			},
			[]string{"partition"},
		),
		recoveryDurationSeconds: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_recovery_duration_seconds",
				Help:    "Duration from detected outage (leader/network) start to full assignment & delivery recovery",
				Buckets: prometheus.ExponentialBuckets(0.5, 1.6, 12), // 0.5s .. ~300s
			},
		),
		coldStartConvergenceSeconds: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_cold_start_convergence_seconds",
				Help:    "Time from first assignment snapshot to full coverage (all partitions assigned) during cold start",
				Buckets: prometheus.ExponentialBuckets(0.05, 1.6, 12), // 50ms .. ~160s
			},
		),

		// Cold start completion gauge (0/1)
		coldStartComplete: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_cold_start_complete",
				Help: "Indicator (0/1) that cold start baseline is locked (coverage=100% and baseline established)",
			},
		),

		// Catch-up SLO metrics
		workerCatchUpLatencySeconds: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_worker_catch_up_latency_seconds",
				Help:    "Latency from worker recovery start (post-absence activity) to backlog healing threshold",
				Buckets: prometheus.ExponentialBuckets(0.05, 1.8, 12), // 50ms .. ~150s
			},
		),
		workerCatchUpExceedTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_worker_catch_up_exceed_total",
				Help: "Total number of worker catch-up recoveries exceeding the configured deadline",
			},
		),
		workerCatchUpBacklog: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_worker_catch_up_backlog_holes",
				Help: "Initial hole backlog size for most recent/active worker recovery",
			},
		),
		holeAgeAtRecoverySeconds: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_hole_age_at_recovery_seconds",
				Help:    "Age distribution of oldest hole at worker recovery start",
				Buckets: prometheus.ExponentialBuckets(0.01, 2, 14), // 10ms .. ~160s
			},
		),

		// Tracker / stability audit metrics
		lateMessagesTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_tracker_late_messages_total",
				Help: "Total number of messages classified as late across audits",
			},
		),
		lostMessagesTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_tracker_lost_messages_total",
				Help: "Total number of messages classified as lost across audits (unresolved holes after timeout)",
			},
		),
		auditDurationSeconds: factory.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_tracker_audit_duration_seconds",
				Help:    "Duration of final drain audits per worker",
				Buckets: prometheus.ExponentialBuckets(0.01, 1.7, 10), // ~10ms .. ~30s
			},
		),
		auditFailuresTotal: factory.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_tracker_audit_failures_total",
				Help: "Total number of failed drain audits (timeout, loss, or late messages)",
			},
		),
		holesOpen: factory.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_tracker_holes_open",
				Help: "Current number of unresolved holes at audit time",
			},
		),
		// default gates true for backward compatibility
		enablePerPartitionLatency:    true,
		enablePerPartitionDuplicates: true,
		bucketCount:                  0,
	}
}

// NewCollectorWithOptions creates a collector using the global registry with
// configurable per-partition metric gates.
//
// Parameters:
//   - perPartitionLatency: enable per-partition latency histogram
//   - perPartitionDuplicates: enable per-partition duplicates counter
//
// Returns:
//   - *Collector: Initialized metrics collector
func NewCollectorWithOptions(perPartitionLatency, perPartitionDuplicates bool) *Collector {
	return NewCollectorWithRegistryAndOptions(prometheus.DefaultRegisterer, perPartitionLatency, perPartitionDuplicates, 0)
}

// NewCollectorWithRegistryAndOptions creates a collector with a custom registerer and metric gates.
//
// Parameters:
//   - reg: Prometheus registerer to use
//   - perPartitionLatency: enable per-partition latency histogram
//   - perPartitionDuplicates: enable per-partition duplicates counter
//
// Returns:
//   - *Collector: Initialized metrics collector registered on reg
func NewCollectorWithRegistryAndOptions(reg prometheus.Registerer, perPartitionLatency, perPartitionDuplicates bool, bucketCount int) *Collector {
	c := NewCollectorWithRegistry(reg)
	c.enablePerPartitionLatency = perPartitionLatency
	c.enablePerPartitionDuplicates = perPartitionDuplicates
	c.bucketCount = bucketCount
	// If bucketCount>0 create bucket metrics and suppress per-partition emission
	if bucketCount > 0 {
		factory := promauto.With(reg)
		c.publishToConsumeLatencyBuckets = factory.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "simulation_publish_to_consume_latency_bucket_seconds",
				Help:    "End-to-end latency from publish to consume aggregated by hashed bucket",
				Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
			},
			[]string{"bucket"},
		)
		c.messageDuplicatesByBucket = factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: "simulation_message_duplicates_bucket_total",
				Help: "Total duplicate messages aggregated by hashed bucket",
			},
			[]string{"bucket"},
		)
	}

	return c
}

// RecordMessageSent records a message sent event.
//
// Parameters:
//   - partitionID: Partition ID
func (c *Collector) RecordMessageSent(partitionID int) {
	c.messagesSentTotal.WithLabelValues(formatPartitionID(partitionID)).Inc()
	c.messagesSentTotalAggregate.Inc()
}

// RecordMessageReceived records a message received event.
//
// Parameters:
//   - partitionID: Partition ID
func (c *Collector) RecordMessageReceived(partitionID int) {
	c.messagesReceivedTotal.WithLabelValues(formatPartitionID(partitionID)).Inc()
	c.messagesReceivedTotalAggregate.Inc()
}

// RecordGap records a message gap detection.
func (c *Collector) RecordGap() {
	c.messageGapsTotal.Inc()
}

// RecordDuplicate records a duplicate message detection.
func (c *Collector) RecordDuplicate() {
	c.messageDuplicatesTotal.Inc()
}

// RecordDuplicatePartition records a duplicate event for a specific partition.
// Parameters:
//   - partitionID: Partition ID
func (c *Collector) RecordDuplicatePartition(partitionID int) {
	if c.bucketCount > 0 {
		b := partitionID % c.bucketCount
		c.messageDuplicatesByBucket.WithLabelValues(formatPartitionID(b)).Inc()
		return
	}
	if !c.enablePerPartitionDuplicates {
		return
	}
	c.messageDuplicatesByPartition.WithLabelValues(formatPartitionID(partitionID)).Inc()
}

// Gate metrics adapters
func (c *Collector) IncGateNAK(reason string) {
	c.gateNakTotal.WithLabelValues(reason).Inc()
}

func (c *Collector) ObserveGateNakDelay(d time.Duration) {
	c.gateNakDelay.Observe(d.Seconds())
}

// Resolver metrics adapters
func (c *Collector) ObserveResolverVisibilityLag(d time.Duration) {
	c.resolverVisibilityLag.Observe(d.Seconds())
}

func (c *Collector) SetResolverCacheSize(n int) {
	c.resolverCacheSize.Set(float64(n))
}

func (c *Collector) IncResolverUpdate(op string) {
	c.resolverUpdatesTotal.WithLabelValues(op).Inc()
}

// Coordinator gauge setters
func (c *Collector) SetCoordinatorInFlight(n int64) {
	c.coordinatorInFlight.Set(float64(n))
}

func (c *Collector) SetCoordinatorPendingHoles(n int64) {
	c.coordinatorPendingHoles.Set(float64(n))
}

// Assignment metric setters / increments
func (c *Collector) SetUnassignedPartitions(n int) {
	c.assignmentUnassignedPartitions.Set(float64(n))
	c.assignMu.Lock()
	c.cachedUnassignedPartitions = n
	c.assignMu.Unlock()
}

func (c *Collector) SetLocalityRatio(r float64) {
	// Clamp to [0,1] to avoid accidental >1 from math errors
	if r < 0 {
		r = 0
	} else if r > 1 {
		r = 1
	}
	c.assignmentLocalityRatio.Set(r)
	c.assignMu.Lock()
	c.cachedLocalityRatio = r
	c.assignMu.Unlock()
}

// SetStableLocalityRatio sets the stable locality ratio gauge and caches it for reporting.
// Parameters:
//   - r: Stable locality ratio in [0,1]
func (c *Collector) SetStableLocalityRatio(r float64) {
	if r < 0 {
		r = 0
	} else if r > 1 {
		r = 1
	}
	c.assignmentStableLocalityRatio.Set(r)
	c.assignMu.Lock()
	c.cachedStableLocalityRatio = r
	c.assignMu.Unlock()
}

func (c *Collector) IncMovedPartitions(n int) {
	if n <= 0 {
		return
	}
	for i := 0; i < n; i++ { // increment by n (counter has no Add in client_golang v1)
		c.assignmentMovedPartitionsTotal.Inc()
	}
	c.assignMu.Lock()
	c.cachedMovedPartitionsTotal += int64(n)
	c.assignMu.Unlock()
}

// Delivery robustness metric adapters
func (c *Collector) ObserveHoleLifetime(d time.Duration) {
	c.holeLifetimeSeconds.Observe(d.Seconds())
}

func (c *Collector) IncHolesHealed(n int) {
	for i := 0; i < n; i++ {
		c.holesHealedTotal.Inc()
	}
}

// ObservePendingHoleAge records the age of a single pending hole.
// Parameters:
//   - d: Age duration
func (c *Collector) ObservePendingHoleAge(d time.Duration) {
	c.pendingHoleAgeSeconds.Observe(d.Seconds())
}

// SetHolesHealedRate sets the healing rate gauge (healed holes per second in last interval).
// Parameters:
//   - rate: healing rate per second
func (c *Collector) SetHolesHealedRate(rate float64) {
	c.holesHealedRate.Set(rate)
}

// GetHolesHealedRate returns the current healed holes rate gauge value (per second).
// Returns:
//   - float64: current healed rate value
func (c *Collector) GetHolesHealedRate() float64 {
	m := &promclient.Metric{}
	if err := c.holesHealedRate.Write(m); err == nil && m.Gauge != nil {
		return m.Gauge.GetValue()
	}
	return 0.0
}

func (c *Collector) SetDisorderDepth(depth int64) {
	if depth < 0 {
		depth = 0
	}
	c.disorderDepth.Set(float64(depth))
	c.cachedDisorderDepth = depth
}

// Latency & recovery adapters
func (c *Collector) ObservePublishToConsumeLatency(d time.Duration) {
	c.publishToConsumeLatency.Observe(d.Seconds())
}

// ObservePublishToConsumeLatencyPartition records per-partition publish→consume latency.
// Parameters:
//   - partitionID: Partition ID
//   - d: Latency duration
func (c *Collector) ObservePublishToConsumeLatencyPartition(partitionID int, d time.Duration) {
	if c.bucketCount > 0 {
		b := partitionID % c.bucketCount
		c.publishToConsumeLatencyBuckets.WithLabelValues(formatPartitionID(b)).Observe(d.Seconds())
		return
	}
	if !c.enablePerPartitionLatency {
		return
	}
	c.publishToConsumeLatencyByPartition.WithLabelValues(formatPartitionID(partitionID)).Observe(d.Seconds())
}

func (c *Collector) ObserveRecoveryDuration(d time.Duration) {
	c.recoveryDurationSeconds.Observe(d.Seconds())
}

// Catch-up SLO metric adapters
// ObserveWorkerCatchUpLatency records the latency of a worker catch-up recovery.
// Parameters:
//   - d: Duration from recovery start to completion
func (c *Collector) ObserveWorkerCatchUpLatency(d time.Duration) {
	c.workerCatchUpLatencySeconds.Observe(d.Seconds())
}

// IncWorkerCatchUpExceed increments the exceed counter when a recovery exceeds the deadline.
func (c *Collector) IncWorkerCatchUpExceed() {
	c.workerCatchUpExceedTotal.Inc()
}

// SetWorkerCatchUpBacklog sets the gauge for initial backlog holes in a recovery.
// Parameters:
//   - n: Number of holes at recovery start
func (c *Collector) SetWorkerCatchUpBacklog(n int) {
	c.workerCatchUpBacklog.Set(float64(n))
}

// ObserveHoleAgeAtRecovery records the oldest hole age at recovery start.
// Parameters:
//   - d: Age duration
func (c *Collector) ObserveHoleAgeAtRecovery(d time.Duration) {
	c.holeAgeAtRecoverySeconds.Observe(d.Seconds())
}

// ObserveColdStartConvergence records cold start convergence duration.
// Parameters:
//   - d: Duration from first partial assignment to full coverage
func (c *Collector) ObserveColdStartConvergence(d time.Duration) {
	c.coldStartConvergenceSeconds.Observe(d.Seconds())
}

// SetColdStartComplete sets the cold start completion gauge.
// Parameters:
//   - complete: true when baseline locked, false otherwise
func (c *Collector) SetColdStartComplete(complete bool) {
	if complete {
		c.coldStartComplete.Set(1)
	} else {
		c.coldStartComplete.Set(0)
	}
}

// ColdStartComplete returns whether cold start baseline is locked.
// Returns:
//   - bool: true if cold start is complete
func (c *Collector) ColdStartComplete() bool {
	m := &promclient.Metric{}
	if err := c.coldStartComplete.Write(m); err == nil && m.Gauge != nil {
		return m.Gauge.GetValue() >= 0.5
	}
	return false
}

// Read-only accessors for reporting
func (c *Collector) GetAssignmentMetrics() (unassigned int, locality float64, moved int64) {
	c.assignMu.RLock()
	defer c.assignMu.RUnlock()
	return c.cachedUnassignedPartitions, c.cachedLocalityRatio, c.cachedMovedPartitionsTotal
}

// GetStableLocalityRatio returns the cached stable locality ratio.
// Returns:
//   - float64: Stable locality ratio in [0,1]
func (c *Collector) GetStableLocalityRatio() float64 {
	c.assignMu.RLock()
	defer c.assignMu.RUnlock()
	return c.cachedStableLocalityRatio
}

func (c *Collector) GetDisorderDepth() int64 {
	return c.cachedDisorderDepth
}

// PublishLatencySummary returns current histogram sample count and sum for publish→consume latency.
// Returns:
//   - uint64: Number of latency samples observed
//   - float64: Sum of all latency samples in seconds
func (c *Collector) PublishLatencySummary() (uint64, float64) {
	m := &promclient.Metric{}
	if err := c.publishToConsumeLatency.Write(m); err == nil && m.Histogram != nil {
		return m.Histogram.GetSampleCount(), m.Histogram.GetSampleSum()
	}
	return 0, 0
}

// RecoveryDurationSummary returns current histogram sample count and sum for recovery durations.
// Returns:
//   - uint64: Number of recovery duration samples observed
//   - float64: Sum of all recovery duration samples in seconds
func (c *Collector) RecoveryDurationSummary() (uint64, float64) {
	m := &promclient.Metric{}
	if err := c.recoveryDurationSeconds.Write(m); err == nil && m.Histogram != nil {
		return m.Histogram.GetSampleCount(), m.Histogram.GetSampleSum()
	}
	return 0, 0
}

// RecordDrainAudit records aggregated drain audit results from a worker's tracker.
// Parameters:
//   - results: Slice of audit results per partition
//   - workerID: ID of the worker performing the audit (for future labeling; currently unused)
func (c *Collector) RecordDrainAudit(results []durable.AuditResult, workerID string) {
	var totalLate, totalLost, totalHoles int
	var maxDuration time.Duration
	timedOut := false
	for _, r := range results {
		if r.LateMessages > 0 {
			totalLate += r.LateMessages
		}
		if r.LostMessages > 0 {
			totalLost += r.LostMessages
		}
		if r.UnresolvedHoles > 0 {
			totalHoles += r.UnresolvedHoles
		}
		if r.Duration > maxDuration {
			maxDuration = r.Duration
		}
		if r.TimedOut {
			timedOut = true
		}
	}
	if totalLate > 0 {
		for i := 0; i < totalLate; i++ { // counter has no Add pre v1.19
			c.lateMessagesTotal.Inc()
		}
	}
	if totalLost > 0 {
		for i := 0; i < totalLost; i++ {
			c.lostMessagesTotal.Inc()
		}
	}
	c.holesOpen.Set(float64(totalHoles))
	if maxDuration > 0 {
		c.auditDurationSeconds.Observe(maxDuration.Seconds())
	}
	if timedOut || totalLate > 0 || totalLost > 0 {
		c.auditFailuresTotal.Inc()
	}
}

// GetLateMessagesTotal returns the total late messages counter value.
func (c *Collector) GetLateMessagesTotal() uint64 {
	m := &promclient.Metric{}
	if err := c.lateMessagesTotal.Write(m); err == nil && m.Counter != nil {
		return uint64(m.Counter.GetValue())
	}
	return 0
}

// GetLostMessagesTotal returns the total lost messages counter value.
func (c *Collector) GetLostMessagesTotal() uint64 {
	m := &promclient.Metric{}
	if err := c.lostMessagesTotal.Write(m); err == nil && m.Counter != nil {
		return uint64(m.Counter.GetValue())
	}
	return 0
}

// GetAuditFailuresTotal returns the total audit failures counter value.
func (c *Collector) GetAuditFailuresTotal() uint64 {
	m := &promclient.Metric{}
	if err := c.auditFailuresTotal.Write(m); err == nil && m.Counter != nil {
		return uint64(m.Counter.GetValue())
	}
	return 0
}

// ColdStartConvergenceSummary returns sample count and sum for cold start convergence durations.
// Returns:
//   - uint64: Number of convergence samples observed
//   - float64: Sum of convergence durations in seconds
func (c *Collector) ColdStartConvergenceSummary() (uint64, float64) {
	m := &promclient.Metric{}
	if err := c.coldStartConvergenceSeconds.Write(m); err == nil && m.Histogram != nil {
		return m.Histogram.GetSampleCount(), m.Histogram.GetSampleSum()
	}
	return 0, 0
}

// GateMetricsAdapter returns an adapter that implements durable.GateMetrics.
func (c *Collector) GateMetricsAdapter() durable.GateMetrics {
	return gateMetricsAdapter{c: c}
}

// ResolverMetricsAdapter returns an adapter that implements durable.ResolverMetrics.
func (c *Collector) ResolverMetricsAdapter() durable.ResolverMetrics {
	return resolverMetricsAdapter{c: c}
}

type gateMetricsAdapter struct{ c *Collector }

func (a gateMetricsAdapter) IncGateNAK(reason string)            { a.c.IncGateNAK(reason) }
func (a gateMetricsAdapter) ObserveGateNakDelay(d time.Duration) { a.c.ObserveGateNakDelay(d) }

type resolverMetricsAdapter struct{ c *Collector }

func (a resolverMetricsAdapter) ObserveVisibilityLag(d time.Duration) {
	a.c.ObserveResolverVisibilityLag(d)
}
func (a resolverMetricsAdapter) SetCacheSize(n int)  { a.c.SetResolverCacheSize(n) }
func (a resolverMetricsAdapter) IncUpdate(op string) { a.c.IncResolverUpdate(op) }
func (a resolverMetricsAdapter) ObserveBatch(size int, applyDuration time.Duration) {
	a.c.resolverBatchSize.Observe(float64(size))
	a.c.resolverBatchDuration.Observe(applyDuration.Seconds())
}

func (a resolverMetricsAdapter) IncBatchFlush(reason string) {
	a.c.resolverBatchFlushes.WithLabelValues(reason).Inc()
}

// SetWorkersActive sets the number of active workers.
//
// Parameters:
//   - count: Number of active workers
func (c *Collector) SetWorkersActive(count int) {
	c.workersActive.Set(float64(count))
	c.cachedActiveWorkers = count
}

// RecordPartitionsPerWorker records the partition count for a worker.
//
// Parameters:
//   - count: Number of partitions assigned to the worker
func (c *Collector) RecordPartitionsPerWorker(count int) {
	c.partitionsPerWorker.Observe(float64(count))
	c.partitionsPerWorkerSum += float64(count)
	c.partitionsPerWorkerCount++
}

// RecordRebalanceDuration records a rebalancing operation duration.
//
// Parameters:
//   - duration: Duration of the rebalancing operation
func (c *Collector) RecordRebalanceDuration(duration time.Duration) {
	c.workerRebalanceDuration.Observe(duration.Seconds())
	c.rebalanceDurationSum += duration.Seconds()
	c.rebalanceDurationCount++
}

// RecordMessageProcessingDuration records message processing duration.
//
// Parameters:
//   - duration: Processing duration
func (c *Collector) RecordMessageProcessingDuration(duration time.Duration) {
	c.messageProcessingDuration.Observe(duration.Seconds())
	c.processingDurationSum += duration.Seconds()
	c.processingDurationCount++
}

// RecordProcessingError records a message processing error.
func (c *Collector) RecordProcessingError() {
	c.messageProcessingErrors.Inc()
}

// RecordChaosEvent records a chaos event.
//
// Parameters:
//   - eventType: Type of chaos event (e.g., "worker_crash", "scale_up")
func (c *Collector) RecordChaosEvent(eventType string) {
	c.chaosEventsTotal.WithLabelValues(eventType).Inc()
}

// SetPartitionWeight sets the weight for a partition.
//
// Parameters:
//   - partitionID: Partition ID
//   - weight: Partition weight
func (c *Collector) SetPartitionWeight(partitionID int, weight int64) {
	c.partitionWeights.WithLabelValues(formatPartitionID(partitionID)).Set(float64(weight))
}

// UpdateSystemMetrics updates system-level metrics.
//
// Parameters:
//   - goroutines: Number of active goroutines
//   - memoryBytes: Memory usage in bytes
func (c *Collector) UpdateSystemMetrics(goroutines int, memoryBytes uint64) {
	c.goroutinesActive.Set(float64(goroutines))
	c.memoryUsageBytes.Set(float64(memoryBytes))
	c.cachedGoroutines = goroutines
	c.cachedMemoryBytes = memoryBytes
}

// GetSystemMetrics returns current system metrics.
//
// Returns:
//   - int: Number of active goroutines
//   - float64: Memory usage in MiB
func (c *Collector) GetSystemMetrics() (int, float64) {
	memoryMiB := float64(c.cachedMemoryBytes) / (1024 * 1024)
	return c.cachedGoroutines, memoryMiB
}

// GetActiveWorkers returns the current number of active workers.
//
// Returns:
//   - int: Number of active workers
func (c *Collector) GetActiveWorkers() int {
	return c.cachedActiveWorkers
}

// GetAggregatedMetrics returns aggregated statistics from histograms.
//
// Returns:
//   - float64: Average partitions per worker
//   - float64: Average rebalance duration in seconds
//   - float64: Average processing latency in seconds
//
// Note: These are manually tracked averages, updated with each observation.
func (c *Collector) GetAggregatedMetrics() (avgPartitions float64, avgRebalance float64, avgLatency float64) {
	if c.partitionsPerWorkerCount > 0 {
		avgPartitions = c.partitionsPerWorkerSum / float64(c.partitionsPerWorkerCount)
	}

	if c.rebalanceDurationCount > 0 {
		avgRebalance = c.rebalanceDurationSum / float64(c.rebalanceDurationCount)
	}

	if c.processingDurationCount > 0 {
		avgLatency = c.processingDurationSum / float64(c.processingDurationCount)
	}

	return avgPartitions, avgRebalance, avgLatency
}

// formatPartitionID formats a partition ID as a string for labels.
func formatPartitionID(partitionID int) string {
	return strconv.Itoa(partitionID)
}
