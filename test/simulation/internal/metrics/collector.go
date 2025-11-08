package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Collector collects and exposes simulation metrics.
type Collector struct {
	// Message counters (per-partition)
	messagesSentTotal      *prometheus.CounterVec
	messagesReceivedTotal  *prometheus.CounterVec
	messageGapsTotal       prometheus.Counter
	messageDuplicatesTotal prometheus.Counter

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
}

// NewCollector creates a new metrics collector.
//
// Returns:
//   - *Collector: Initialized metrics collector
func NewCollector() *Collector {
	return &Collector{
		messagesSentTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "simulation_messages_sent_total",
				Help: "Total number of messages sent per partition",
			},
			[]string{"partition"},
		),
		messagesReceivedTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "simulation_messages_received_total",
				Help: "Total number of messages received per partition",
			},
			[]string{"partition"},
		),
		messagesSentTotalAggregate: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_messages_sent_total_aggregate",
				Help: "Total number of messages sent (aggregate, no labels)",
			},
		),
		messagesReceivedTotalAggregate: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_messages_received_total_aggregate",
				Help: "Total number of messages received (aggregate, no labels)",
			},
		),
		messageGapsTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_message_gaps_total",
				Help: "Total number of message gaps detected",
			},
		),
		messageDuplicatesTotal: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_message_duplicates_total",
				Help: "Total number of duplicate messages detected",
			},
		),
		workersActive: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_workers_active",
				Help: "Number of active workers",
			},
		),
		partitionsPerWorker: promauto.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_partitions_per_worker",
				Help:    "Distribution of partitions per worker",
				Buckets: prometheus.LinearBuckets(0, 10, 20), // 0, 10, 20, ..., 190
			},
		),
		workerRebalanceDuration: promauto.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_rebalancing_duration_seconds",
				Help:    "Duration of rebalancing operations",
				Buckets: prometheus.ExponentialBuckets(0.1, 2, 10), // 0.1, 0.2, 0.4, ..., 51.2
			},
		),
		messageProcessingDuration: promauto.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "simulation_message_processing_duration_seconds",
				Help:    "Message processing duration",
				Buckets: prometheus.ExponentialBuckets(0.001, 2, 10), // 1ms, 2ms, 4ms, ..., 512ms
			},
		),
		messageProcessingErrors: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "simulation_message_processing_errors_total",
				Help: "Total number of message processing errors",
			},
		),
		chaosEventsTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "simulation_chaos_events_total",
				Help: "Total number of chaos events by type",
			},
			[]string{"type"},
		),
		partitionWeights: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "simulation_partition_weights",
				Help: "Weight assigned to each partition",
			},
			[]string{"partition"},
		),
		goroutinesActive: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_goroutines_active",
				Help: "Number of active goroutines",
			},
		),
		memoryUsageBytes: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "simulation_memory_usage_bytes",
				Help: "Current memory usage in bytes",
			},
		),
	}
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
