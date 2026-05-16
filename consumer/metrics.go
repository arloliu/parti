package consumer

import "time"

// GateMetrics captures optional Processing Gate metrics.
//
// Implementations should be non-blocking. When nil, metrics are not recorded.
type GateMetrics interface {
	// IncGateNAK increments the NAK counter by reason
	// (e.g., "unknown_ownership", "non_owner", "owner_disallowed_state").
	IncGateNAK(reason string)

	// ObserveGateNakDelay records the NAK delay applied by the gate.
	ObserveGateNakDelay(d time.Duration)
}

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

	// IncWatcherRestart increments a counter when the resolver's KV watcher
	// is re-established after closure or establishment failure.
	// Reasons: "channel_closed", "drift_detected", "establish_failed".
	IncWatcherRestart(reason string)

	// IncReconcileRescue increments when reconcileOnce applies any
	// change to the cache — i.e., the reconciler observed drift between
	// KV and the in-memory cache and rescued the cache. Persistent
	// non-zero values are strong evidence the KV watcher is silently
	// stalled (the nats.go KV watcher does not surface NATS server
	// restarts as Updates() channel close, so the supervisor's
	// channel_closed restart path will not fire for that trigger).
	// Operators should alert on this metric being non-zero across
	// multiple consecutive reconcile cycles.
	IncReconcileRescue()
}
