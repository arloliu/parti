package parti

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/election"
	"github.com/arloliu/parti/v2/internal/heartbeat"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/stableid"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// logCaptureLogger is a minimal types.Logger that records every log line so a
// test can assert no warning/error fired during graceful shutdown.
type logCaptureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCaptureLogger) record(level, msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+msg+" "+fmt.Sprint(kv...))
}

func (l *logCaptureLogger) Debug(msg string, kv ...any) { l.record("DEBUG", msg, kv...) }
func (l *logCaptureLogger) Info(msg string, kv ...any)  { l.record("INFO", msg, kv...) }
func (l *logCaptureLogger) Warn(msg string, kv ...any)  { l.record("WARN", msg, kv...) }
func (l *logCaptureLogger) Error(msg string, kv ...any) { l.record("ERROR", msg, kv...) }
func (l *logCaptureLogger) Fatal(msg string, kv ...any) { l.record("FATAL", msg, kv...) }

func (l *logCaptureLogger) errorLinesContaining(substr string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, ln := range l.lines {
		if strings.HasPrefix(ln, "ERROR") && strings.Contains(ln, substr) {
			out = append(out, ln)
		}
	}

	return out
}

// followerCancelElection blocks the first RequestLeadership call until release
// is closed, then returns the in-flight ctx error. This deterministically
// reproduces the shutdown race: monitorLeadership's ticker fired before m.ctx
// was cancelled, the follower branch was entered, RequestLeadership was issued
// — then Stop ran m.cancel() while the request was in flight, so reqCtx (which
// inherits from m.ctx) propagates context.Canceled into the spy.
type followerCancelElection struct {
	entered chan struct{}
	release chan struct{}
}

func (b *followerCancelElection) RequestLeadership(ctx context.Context, _ string, _ int64) (bool, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("failed to create leader key: %w", err)
	}

	return false, nil
}

func (b *followerCancelElection) RenewLeadership(context.Context) error { return nil }

func (b *followerCancelElection) IsLeader(context.Context) (bool, error) { return false, nil }

func (b *followerCancelElection) ReleaseLeadership(context.Context) error { return nil }

// TestManager_MonitorLeadership_FollowerTickDuringStop_NoErrorLog is the
// regression test for the user-reported shutdown noise: a follower whose
// monitorLeadership tick races with Stop's m.cancel() must not log the
// "failed to request leadership" Error line. The inner KV error
// (context.Canceled) is benign — it only means Stop won the race against the
// next tick — and recordKVError already short-circuits context.Canceled (not
// classified as connectivity or degrading-JetStream), so the log was the only
// observable side effect.
func TestManager_MonitorLeadership_FollowerTickDuringStop_NoErrorLog(t *testing.T) {
	t.Parallel()

	spy := &followerCancelElection{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	logCap := &logCaptureLogger{}

	m := &Manager{
		cfg: Config{
			// Tight cadence so the first tick fires within the test budget.
			ElectionTimeout:  30 * time.Millisecond,
			OperationTimeout: 200 * time.Millisecond,
		},
		hooks:     &types.Hooks{},
		metrics:   metrics.NewNop(),
		logger:    logCap,
		idClaimer: stableid.NewNop(),
		election:  spy,
		heartbeat: heartbeat.NewNop(),
	}
	m.state.Store(int32(StateStable))
	m.workerID.Store("worker-0")
	m.isLeader.Store(false) // follower path
	m.assignment.Store(Assignment{})
	m.ctx, m.cancel = context.WithCancel(context.Background())

	// Run the goroutine the same way Stop's race would see it.
	done := make(chan struct{})
	go func() {
		m.monitorLeadership()
		close(done)
	}()

	// Wait until the follower tick has fired and the spy is mid-call.
	select {
	case <-spy.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorLeadership never reached the follower tick")
	}

	// Stop's race: cancel m.ctx WHILE the spy is mid-RequestLeadership, then
	// let the spy return. ctx.Err() inside the spy is context.Canceled at this
	// point, so RequestLeadership returns the wrapped error — exactly what
	// NATSElection.RequestLeadership would produce when kv.Create sees a
	// cancelled context mid-flight.
	m.cancel()
	close(spy.release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorLeadership did not return after m.cancel()")
	}

	noisy := logCap.errorLinesContaining("failed to request leadership")
	require.Empty(t, noisy,
		"graceful shutdown must not emit 'failed to request leadership' Error log; got: %v", noisy)
}

// leaderCancelElection blocks the first RenewLeadership call until release is
// closed, then returns ErrLeadershipLost wrapping the in-flight ctx error —
// mirroring exactly what NATSElection.RenewLeadership produces when kv.Update
// sees a cancelled context (see internal/election/nats_election.go:206).
type leaderCancelElection struct {
	entered chan struct{}
	release chan struct{}
}

func (b *leaderCancelElection) RequestLeadership(context.Context, string, int64) (bool, error) {
	return true, nil
}

func (b *leaderCancelElection) RenewLeadership(ctx context.Context) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", election.ErrLeadershipLost, err)
	}

	return nil
}

func (b *leaderCancelElection) IsLeader(context.Context) (bool, error) { return true, nil }

func (b *leaderCancelElection) ReleaseLeadership(context.Context) error { return nil }

// hookRecorder counts OnLeadershipChanged(false) invocations so the leader-side
// shutdown-race test can assert the hook does NOT fire spuriously when Stop
// cancels m.ctx mid-RenewLeadership. The user's app may wire this hook to
// trigger graceful resignation logic — firing it on every normal Stop would
// surprise them.
type hookRecorder struct {
	leadershipLost atomic.Int32
}

// TestManager_MonitorLeadership_LeaderTickDuringStop_NoErrorLogOrHook is the
// regression test for the symmetric leader-side shutdown race. When Stop's
// m.cancel() races with monitorLeadership's renew tick, RenewLeadership
// returns ErrLeadershipLost wrapping context.Canceled. Without the guard, the
// tick would log Error, log Info("lost leadership"), AND fire the
// OnLeadershipChanged(false) hook — the latter being an observable
// behaviour change for any app that reacts to leadership-lost.
func TestManager_MonitorLeadership_LeaderTickDuringStop_NoErrorLogOrHook(t *testing.T) {
	t.Parallel()

	spy := &leaderCancelElection{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	logCap := &logCaptureLogger{}
	rec := &hookRecorder{}

	m := &Manager{
		cfg: Config{
			ElectionTimeout:  30 * time.Millisecond,
			OperationTimeout: 200 * time.Millisecond,
		},
		hooks: &types.Hooks{
			OnLeadershipChanged: func(_ context.Context, isLeader bool) error {
				if !isLeader {
					rec.leadershipLost.Add(1)
				}

				return nil
			},
		},
		metrics:   metrics.NewNop(),
		logger:    logCap,
		idClaimer: stableid.NewNop(),
		election:  spy,
		heartbeat: heartbeat.NewNop(),
	}
	m.state.Store(int32(StateStable))
	m.workerID.Store("worker-0")
	m.isLeader.Store(true) // leader path
	m.assignment.Store(Assignment{})
	m.ctx, m.cancel = context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		m.monitorLeadership()
		close(done)
	}()

	select {
	case <-spy.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorLeadership never reached the leader renew tick")
	}

	m.cancel()
	close(spy.release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitorLeadership did not return after m.cancel()")
	}

	// Drain any wg.Go-spawned work (e.g. logError's OnError hook routing or
	// the leadership-lost hook) so the counter is observed.
	m.wg.Wait()

	noisy := logCap.errorLinesContaining("failed to renew leadership")
	require.Empty(t, noisy,
		"graceful shutdown must not emit 'failed to renew leadership' Error log; got: %v", noisy)
	require.Zero(t, rec.leadershipLost.Load(),
		"OnLeadershipChanged(false) must NOT fire on graceful Stop — Stop's ReleaseLeadership is the authoritative leadership-release path")
}
