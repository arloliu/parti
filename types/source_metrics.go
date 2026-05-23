package types

// SourceMetrics captures source-layer health metrics.
//
// Implementations must be non-blocking and thread-safe. A no-op
// implementation ([NopSourceMetrics]) is provided for callers that do
// not need source metrics.
//
// This interface is intentionally separate from [MetricsCollector] because
// the source layer is independent of the Manager runtime — a
// [WatchablePartitionSource] can run standalone, without a Manager, and the
// source-side observability surface is small enough that bundling it into
// the larger collector would add coupling without value.
type SourceMetrics interface {
	// SetSourceBucketMissing reports whether the partition-source bucket is
	// currently observed to be missing on the live connection. true =
	// missing (the gauge reads 1); false = available (the gauge reads 0).
	//
	// Called from the source's reconcile / restart goroutines at the first
	// bucket-loss observation and again when a subsequent successful
	// operation confirms recovery. The error surface that classifies as
	// "missing" is broader than a single sentinel — implementations should
	// not assume a specific [jetstream] / [nats] error type. Implementations
	// should treat repeated identical values as idempotent.
	SetSourceBucketMissing(missing bool)
}

// NopSourceMetrics is a no-op implementation of [SourceMetrics] suitable as
// the default when callers do not wire a real collector.
type NopSourceMetrics struct{}

func (NopSourceMetrics) SetSourceBucketMissing(bool) {}
