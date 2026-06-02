package parti

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/heartbeat"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/stableid"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// blockingSource is a fake PartitionSource whose Stop blocks until stopGate is
// closed. Used to prove that ReleaseLeadership fires before slow source cleanup.
type blockingSource struct {
	stopGate chan struct{}
}

func (b *blockingSource) Start(_ context.Context) error                     { return nil }
func (b *blockingSource) List(_ context.Context) ([]types.Partition, error) { return nil, nil }
func (b *blockingSource) Stop(_ context.Context) error                      { <-b.stopGate; return nil }

// spyElection records whether ReleaseLeadership was called.
type spyElection struct {
	releaseCalled atomic.Bool
}

func (s *spyElection) RequestLeadership(context.Context, string, int64) (bool, error) {
	return false, nil
}
func (s *spyElection) RenewLeadership(context.Context) error  { return nil }
func (s *spyElection) IsLeader(context.Context) (bool, error) { return false, nil }
func (s *spyElection) ReleaseLeadership(_ context.Context) error {
	s.releaseCalled.Store(true)
	return nil
}

func TestManager_Stop_ReleasesLeadershipBeforeSlowSourceStop(t *testing.T) {
	// Short timeout so the test override doesn't bleed into other tests.
	orig := releaseLeadershipTimeout
	releaseLeadershipTimeout = 200 * time.Millisecond
	t.Cleanup(func() { releaseLeadershipTimeout = orig })

	gate := make(chan struct{})
	spy := &spyElection{}
	src := &blockingSource{stopGate: gate}

	m := &Manager{
		cfg:       Config{ShutdownTimeout: 5 * time.Second},
		hooks:     &types.Hooks{},
		metrics:   metrics.NewNop(),
		logger:    logging.NewNop(),
		idClaimer: stableid.NewNop(),
		election:  spy,
		heartbeat: heartbeat.NewNop(),
		source:    src,
	}
	m.state.Store(int32(StateStable))
	m.workerID.Store("worker-0")
	m.assignment.Store(Assignment{})
	m.ctx, m.cancel = context.WithCancel(context.Background())

	stopDone := make(chan error, 1)
	go func() { stopDone <- m.Stop(t.Context()) }()

	// Leadership must be released while the source is still blocking.
	require.Eventually(t, spy.releaseCalled.Load, 500*time.Millisecond, 5*time.Millisecond,
		"ReleaseLeadership must be called before source Stop unblocks")

	// Unblock the source so Stop can return.
	close(gate)
	require.NoError(t, <-stopDone)
}

func TestStop_AlwaysReleasesLeadership(t *testing.T) {
	t.Run("releases leadership even when IsLeader is false", func(t *testing.T) {
		spy := &spyElection{}

		m := &Manager{
			cfg:       Config{ShutdownTimeout: 5 * time.Second},
			hooks:     &types.Hooks{},
			metrics:   metrics.NewNop(),
			logger:    logging.NewNop(),
			idClaimer: stableid.NewNop(),
			election:  spy,
			heartbeat: heartbeat.NewNop(),
			source:    source.NewStatic(nil),
		}
		m.state.Store(int32(StateStable))
		m.workerID.Store("worker-0")
		m.assignment.Store(Assignment{})
		m.ctx, m.cancel = context.WithCancel(context.Background())
		// isLeader defaults to false (zero value)

		err := m.Stop(t.Context())
		require.NoError(t, err)

		require.True(t, spy.releaseCalled.Load(),
			"ReleaseLeadership must be called even when m.IsLeader() is false")
	})
}
