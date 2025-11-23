package subscription

import "time"

// ResolverMetrics captures optional ownership resolver metrics.
// Implementations should be non-blocking. When nil, metrics are not recorded.
type ResolverMetrics interface {
	// ObserveVisibilityLag records the lag between claim LastUpdated and
	// when the resolver observes the update (watcher delivery time).
	ObserveVisibilityLag(d time.Duration)

	// SetCacheSize sets the current resolver cache size (number of partitions
	// with visible claims).
	SetCacheSize(n int)

	// IncUpdate increments update counters by operation type
	// (e.g., "upsert", "delete").
	IncUpdate(op string)

	// ObserveBatch records properties of an applied batch: the number of
	// coalesced items and how long it took to apply to the cache.
	ObserveBatch(size int, applyDuration time.Duration)

	// IncBatchFlush increments a counter for batch flush events by reason
	// (e.g., "timer", "maxitems").
	IncBatchFlush(reason string)
}
