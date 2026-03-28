package handoff

import (
	"context"

	"github.com/arloliu/parti/v2/types"
)

// direct implements Coordinator by directly applying the new assignment
// via the ConsumerUpdater without additional orchestration steps.
type direct struct {
	cfg Config
}

// Apply executes the consumer update immediately.
//
// Parameters:
//   - ctx: Context for cancellation
//   - workerID: Stable worker ID
//   - old: Previous assignment (unused except future diff logic)
//   - new: New assignment whose partitions will be applied
//
// Returns:
//   - error: Update error, nil on success
func (d *direct) Apply(ctx context.Context, workerID string, previous, next types.Assignment) error {
	if d.cfg.ConsumerUpdater == nil {
		return nil // Nothing to do
	}
	inst := newInstrumenter(d.cfg.Metrics)

	// Apply is a single phase in direct mode
	err := inst.phase("apply", func() error {
		return d.cfg.ConsumerUpdater.UpdateWorkerConsumer(ctx, workerID, next.Partitions)
	})
	inst.finish(err)

	return err
}
