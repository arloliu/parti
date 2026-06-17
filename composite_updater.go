package parti

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/arloliu/parti/v2/internal/recovery"
)

// CompositeConsumerUpdater combines multiple WorkerConsumerUpdater instances.
// When UpdateWorkerConsumer is called, it fans out to all registered updaters.
//
// Use cases:
//   - Register both WorkerConsumer (per-partition) and BroadcastConsumer (fan-out)
//   - Multiple BroadcastConsumer instances for different stream/subject patterns
//
// Error handling:
//   - Calls all updaters even if some fail
//   - Returns a combined error with all failures
type CompositeConsumerUpdater struct {
	// mu guards updaters, currentObserver, and lastFleetN. Held only
	// long enough to snapshot the slice (or update the observer/count);
	// fan-out callbacks to children run unlocked so a slow child cannot
	// serialize the rest and so re-entrant Add/SetOnStreamMissingError
	// calls from a child never self-deadlock.
	mu              sync.Mutex
	updaters        []WorkerConsumerUpdater
	currentObserver func(streamName string, err error)
	// lastFleetN is the most recent worker-count observed via ObserveWorkerCount,
	// replayed to children added later (mirrors the stream-missing replay). 0 =
	// none observed yet.
	lastFleetN int
}

// NewCompositeConsumerUpdater creates a composite from multiple updaters.
// All provided updaters will receive partition updates when UpdateWorkerConsumer is called.
//
// Example:
//
//	wc, _ := durable.NewWorkerConsumer(js, wcConfig, handler1)
//	bc, _ := durable.NewBroadcastConsumer(js, bcConfig, handler2)
//	composite := parti.NewCompositeConsumerUpdater(wc, bc)
//	mgr, _ := parti.NewManager(cfg, js, src, strategy,
//	    parti.WithWorkerConsumerUpdater(composite),
//	)
func NewCompositeConsumerUpdater(updaters ...WorkerConsumerUpdater) *CompositeConsumerUpdater {
	// Filter out nil updaters
	filtered := make([]WorkerConsumerUpdater, 0, len(updaters))
	for _, u := range updaters {
		if u != nil {
			filtered = append(filtered, u)
		}
	}

	return &CompositeConsumerUpdater{
		updaters: filtered,
	}
}

// UpdateWorkerConsumer implements WorkerConsumerUpdater.
// Calls UpdateWorkerConsumer on all registered updaters, collecting any errors.
// All updaters are called even if some fail.
func (c *CompositeConsumerUpdater) UpdateWorkerConsumer(ctx context.Context, workerID string, partitions []Partition) error {
	c.mu.Lock()
	if len(c.updaters) == 0 {
		c.mu.Unlock()
		return nil
	}
	// Snapshot under the mutex so a concurrent Add does not race the
	// fan-out loop. Each child runs unlocked — its UpdateWorkerConsumer
	// may block on JetStream and must not serialize peers or deadlock
	// against a callback that calls back into the composite.
	snapshot := make([]WorkerConsumerUpdater, len(c.updaters))
	copy(snapshot, c.updaters)
	c.mu.Unlock()

	var errs []error
	for _, u := range snapshot {
		if err := u.UpdateWorkerConsumer(ctx, workerID, partitions); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) == 0 {
		return nil
	}

	if len(errs) == 1 {
		return errs[0]
	}

	return fmt.Errorf("composite consumer updater errors: %w", errors.Join(errs...))
}

// Add appends additional updaters to the composite.
// This is useful for dynamically registering consumers after creation.
//
// Newly-added children inherit any state the composite has already received:
// an installed stream-missing observer (via SetOnStreamMissingError) and the
// last observed fleet worker-count (via ObserveWorkerCount), so a late
// registration does not silently miss either. Inherited-state forwarding runs
// outside c.mu so a slow child cannot serialize peers or deadlock a re-entrant
// Add.
func (c *CompositeConsumerUpdater) Add(updaters ...WorkerConsumerUpdater) {
	c.mu.Lock()
	var added []WorkerConsumerUpdater
	for _, u := range updaters {
		if u == nil {
			continue
		}
		c.updaters = append(c.updaters, u)
		added = append(added, u)
	}
	observer := c.currentObserver
	fleetN := c.lastFleetN
	c.mu.Unlock()

	for _, u := range added {
		if observer != nil {
			if obs, ok := u.(recovery.StreamMissingObserver); ok {
				obs.SetOnStreamMissingError(observer)
			}
		}
		if fleetN > 0 {
			if fo, ok := u.(FleetSizeObserver); ok {
				fo.ObserveWorkerCount(fleetN)
			}
		}
	}
}

// ObserveWorkerCount implements [FleetSizeObserver] by caching the worker-count
// and forwarding it to every child that implements FleetSizeObserver. The cache
// is replayed to children added later via Add.
func (c *CompositeConsumerUpdater) ObserveWorkerCount(n int) {
	c.mu.Lock()
	c.lastFleetN = n
	snapshot := make([]WorkerConsumerUpdater, len(c.updaters))
	copy(snapshot, c.updaters)
	c.mu.Unlock()

	for _, u := range snapshot {
		if fo, ok := u.(FleetSizeObserver); ok {
			fo.ObserveWorkerCount(n)
		}
	}
}

// Len returns the number of registered updaters.
func (c *CompositeConsumerUpdater) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.updaters)
}

// SetOnStreamMissingError records the callback and forwards it to all
// current children that implement [recovery.StreamMissingObserver].
// Children that do not implement the interface are silently skipped.
// Subsequent [Add] calls forward the recorded observer to newly-added
// observer-capable children.
//
// Passing fn == nil clears the recorded observer and forwards the
// nil to current observer-capable children so they stop dispatching
// to the previous closure.
//
// Implements [recovery.StreamMissingObserver]; the Parti manager
// type-asserts the composite during Manager.Start and bridges the
// stream-missing recovery exhaustion event to enterDegraded(
// "stream-missing-recovery-exhausted").
func (c *CompositeConsumerUpdater) SetOnStreamMissingError(fn func(streamName string, err error)) {
	c.mu.Lock()
	c.currentObserver = fn
	// Snapshot the observer-capable children under the lock; release
	// before calling SetOnStreamMissingError on each so a slow
	// installation does not block peers and re-entrant Add calls
	// remain deadlock-free.
	observers := make([]recovery.StreamMissingObserver, 0, len(c.updaters))
	for _, u := range c.updaters {
		if obs, ok := u.(recovery.StreamMissingObserver); ok {
			observers = append(observers, obs)
		}
	}
	c.mu.Unlock()

	for _, obs := range observers {
		obs.SetOnStreamMissingError(fn)
	}
}

// Capabilities implements [CapabilityReporter] by ORing the runtime
// capability bits reported by each child updater that implements
// [CapabilityReporter]. Children that do not implement the interface
// contribute zero bits.
//
// Composition rule: OR. A capability is considered wired for the
// composite if any child updater has wired it.
//
// Returns:
//   - uint32: OR of all child-reported capability bits, or 0 if no
//     child implements CapabilityReporter or none has wired any bits.
func (c *CompositeConsumerUpdater) Capabilities() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var bits uint32
	for _, u := range c.updaters {
		if cr, ok := u.(CapabilityReporter); ok {
			bits |= cr.Capabilities()
		}
	}

	return bits
}

// Compile-time assertion: CompositeConsumerUpdater satisfies
// CapabilityReporter so the Manager's type-assertion picks it up.
var _ CapabilityReporter = (*CompositeConsumerUpdater)(nil)

// Compile-time assertion: CompositeConsumerUpdater satisfies
// recovery.StreamMissingObserver. The Manager's type-assertion during
// prepareStart forwards through this seam to each observer-capable
// child.
var _ recovery.StreamMissingObserver = (*CompositeConsumerUpdater)(nil)

// Compile-time assertion: CompositeConsumerUpdater satisfies FleetSizeObserver so
// the Manager's type-assertion picks it up.
var _ FleetSizeObserver = (*CompositeConsumerUpdater)(nil)
