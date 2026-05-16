package parti

import (
	"context"
	"errors"
	"fmt"
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
	updaters []WorkerConsumerUpdater
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
	if len(c.updaters) == 0 {
		return nil
	}

	var errs []error
	for _, u := range c.updaters {
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
func (c *CompositeConsumerUpdater) Add(updaters ...WorkerConsumerUpdater) {
	for _, u := range updaters {
		if u != nil {
			c.updaters = append(c.updaters, u)
		}
	}
}

// Len returns the number of registered updaters.
func (c *CompositeConsumerUpdater) Len() int {
	return len(c.updaters)
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
