package parti

// FleetSizeObserver is an optional interface a [WorkerConsumerUpdater] MAY
// implement to receive the observed cluster worker-count (N) for fleet-size-
// aware (adaptive) rate limiting.
//
// When the registered updater (or any child of a composite updater) satisfies
// this interface, the Manager calls ObserveWorkerCount whenever the committed
// worker-set size changes — and once during startup, before the first apply —
// so the consumer can retune its consumer-create rate to
// min(perWorkerCeiling, clusterRate/N).
//
// Implementations MUST be:
//   - Non-blocking. ObserveWorkerCount runs on the Manager's commit-watch
//     goroutine while the Manager holds an internal lock; it must not perform
//     I/O or block.
//   - Safe for concurrent use.
//   - Non-reentrant: it MUST NOT call back into the Manager or any apply/update
//     path (mirrors the [CapabilityReporter] contract and the D5 lock-order
//     rule).
//
// n is always >= 1.
type FleetSizeObserver interface {
	// ObserveWorkerCount reports the current committed cluster worker-count.
	ObserveWorkerCount(n int)
}
