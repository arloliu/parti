package parti

import (
	"context"
	"errors"
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

// spyElection records whether ReleaseLeadership was called. releaseErr, when
// non-nil, is returned from ReleaseLeadership to exercise shutdown error paths.
type spyElection struct {
	releaseCalled atomic.Bool
	releaseErr    error
}

func (s *spyElection) RequestLeadership(context.Context, string, int64) (bool, error) {
	return false, nil
}
func (s *spyElection) RenewLeadership(context.Context) error  { return nil }
func (s *spyElection) IsLeader(context.Context) (bool, error) { return false, nil }
func (s *spyElection) ReleaseLeadership(_ context.Context) error {
	s.releaseCalled.Store(true)
	return s.releaseErr
}

// errStopSource is a fake PartitionSource whose Stop always fails with a fixed
// sentinel, used to prove Stop surfaces multiple component errors.
type errStopSource struct{ err error }

func (e *errStopSource) Start(_ context.Context) error                     { return nil }
func (e *errStopSource) List(_ context.Context) ([]types.Partition, error) { return nil, nil }
func (e *errStopSource) Stop(_ context.Context) error                      { return e.err }

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

// TestStop_JoinsAllComponentErrors pins that Stop surfaces EVERY failing
// component, not just the first. The regression it guards: source.Stop's error
// used to unconditionally overwrite an earlier leadership-release error (and the
// later steps were gated on shutdownErr==nil), so a multi-component failure hid
// all but one cause — exactly the kind of silent root-cause loss that misleads
// operators during shutdown. errors.Join makes every cause inspectable.
func TestStop_JoinsAllComponentErrors(t *testing.T) {
	errLeadership := errors.New("boom: leadership")
	errSource := errors.New("boom: source")

	spy := &spyElection{releaseErr: errLeadership}
	src := &errStopSource{err: errSource}

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

	err := m.Stop(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, errLeadership, "leadership-release error must survive in the joined error")
	require.ErrorIs(t, err, errSource, "source-stop error must survive in the joined error")
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
