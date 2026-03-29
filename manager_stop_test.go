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

func TestStop_AlwaysReleasesLeadership(t *testing.T) {
	t.Run("releases leadership even when IsLeader is false", func(t *testing.T) {
		spy := &spyElection{}

		m := &Manager{
			cfg:             Config{ShutdownTimeout: 5 * time.Second},
			hooks:           &types.Hooks{},
			metrics:         metrics.NewNop(),
			logger:          logging.NewNop(),
			connMonitorStop: make(chan struct{}),
			idClaimer:       stableid.NewNop(),
			election:        spy,
			heartbeat:       heartbeat.NewNop(),
			source:          source.NewStatic(nil),
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
