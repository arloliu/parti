package parti

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/hooks"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/types"
)

// recordingCoordinator is a test stub for handoff.Coordinator. Apply
// increments applyCount, invokes onApply if set, and returns a synthetic
// failure while applyCount <= failUntilCount (0 disables). All
// synchronization is via atomics.
type recordingCoordinator struct {
	applyCount     atomic.Int64
	failUntilCount atomic.Int64 // Apply returns synthetic failure while applyCount <= this; 0 disables
	onApply        func(ctx context.Context)
}

func (r *recordingCoordinator) Start(_ context.Context) {}

func (r *recordingCoordinator) Apply(
	ctx context.Context,
	_ string,
	_ types.Assignment,
	_ types.Assignment,
) error {
	n := r.applyCount.Add(1)
	if r.onApply != nil {
		r.onApply(ctx)
	}
	if u := r.failUntilCount.Load(); u > 0 && n <= u {
		return errors.New("synthetic apply failure")
	}

	return nil
}

var _ handoff.Coordinator = (*recordingCoordinator)(nil)

// newTestManagerWithJitter constructs a minimal Manager fixture with
// ApplyStartJitter set to jitter. NOT Stop-safe: election, source, and
// idClaimer are nil. Cancellation runs via t.Cleanup.
func newTestManagerWithJitter(t *testing.T, jitter time.Duration) *Manager {
	t.Helper()
	cfg := TestConfig()
	cfg.ApplyStartJitter = jitter
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	nopHooks := hooks.NewNop()
	m := &Manager{
		cfg:       cfg,
		hooks:     &nopHooks,
		metrics:   newRecordingMetrics(),
		logger:    logging.NewNop(),
		heartbeat: &recordingHeartbeat{},
		// handoffCoordinator is nil; callers must set it before invoking apply paths.
		handoffCoordinator: &recordingCoordinator{},
	}
	m.workerID.Store("worker-test")
	m.assignment.Store(Assignment{})
	m.ctx = ctx
	m.cancel = cancel

	return m
}
