package failure_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestFullNATSOutage_UnlimitedReconnects_RecoversFleet(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	nc, stopNATS, restartNATS := startEmbeddedNATSOutage(t, -1)
	realJS, err := jetstream.New(nc)
	require.NoError(t, err)

	rfBuildStream(t, ctx, realJS)
	src := source.NewStatic([]types.Partition{{Keys: []string{"p0"}}})

	var degradedCalls atomic.Int64
	stack := rfBuildWorkerStackCfg(t, ctx, realJS, src, 0, fastOutageConfig, func(h *parti.Hooks) {
		h.OnDegraded = func(_ context.Context, reason string) error {
			if reason == "NATS connection down" {
				degradedCalls.Add(1)
			}
			return nil
		}
	})
	require.NoError(t, <-stack.mgr.WaitState(parti.StateStable, 30*time.Second))

	rfPublish(t, ctx, realJS, "p0", "before-outage")
	require.Eventually(t, func() bool { return stack.consumed.Load() >= 1 },
		15*time.Second, 100*time.Millisecond, "baseline consume failed before outage")

	stopNATS(t)
	require.Eventually(t, func() bool { return nc.Status() != nats.CONNECTED },
		5*time.Second, 50*time.Millisecond, "client did not observe NATS outage")
	require.NoError(t, <-stack.mgr.WaitState(parti.StateDegraded, 10*time.Second))
	require.Eventually(t, func() bool { return degradedCalls.Load() > 0 },
		5*time.Second, 20*time.Millisecond, "OnDegraded must report NATS connection down")
	require.Len(t, stack.mgr.CurrentAssignment().Partitions, 1,
		"manager must retain cached assignment while NATS is down")

	restartNATS(t)
	require.Eventually(t, func() bool { return nc.Status() == nats.CONNECTED },
		5*time.Second, 50*time.Millisecond, "client did not reconnect")
	require.NoError(t, <-stack.mgr.WaitState(parti.StateStable, 20*time.Second))

	base := stack.consumed.Load()
	rfPublish(t, ctx, realJS, "p0", "after-outage")
	require.Eventually(t, func() bool { return stack.consumed.Load() > base },
		20*time.Second, 100*time.Millisecond, "consumer did not resume after NATS restart")
}

func TestFullNATSOutage_FiniteReconnects_DegradesClosedConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	nc, stopNATS, _ := startEmbeddedNATSOutage(t, 0)
	realJS, err := jetstream.New(nc)
	require.NoError(t, err)

	rfBuildStream(t, ctx, realJS)
	src := source.NewStatic([]types.Partition{{Keys: []string{"p0"}}})

	stack := rfBuildWorkerStackCfg(t, ctx, realJS, src, 0, fastOutageConfig, nil)
	require.NoError(t, <-stack.mgr.WaitState(parti.StateStable, 30*time.Second))

	stopNATS(t)
	require.Eventually(t, func() bool { return nc.Status() == nats.CLOSED },
		10*time.Second, 50*time.Millisecond, "finite reconnect client did not close")
	require.NoError(t, <-stack.mgr.WaitState(parti.StateDegraded, 10*time.Second))
	require.Never(t, func() bool { return stack.mgr.State() == parti.StateStable },
		3*time.Second, 100*time.Millisecond,
		"manager must not report Stable after the caller-owned NATS connection is closed")
}

func fastOutageConfig(cfg *parti.Config) {
	cfg.DegradedBehavior.EnterThreshold = 500 * time.Millisecond
	cfg.DegradedBehavior.ExitThreshold = 500 * time.Millisecond
}

func startEmbeddedNATSOutage(
	t *testing.T,
	maxReconnect int,
) (conn *nats.Conn, stop func(*testing.T), restart func(*testing.T)) {
	t.Helper()

	port, err := getFreePortForRestart()
	require.NoError(t, err)
	storeDir := t.TempDir()

	buildOpts := func() *server.Options {
		return &server.Options{
			Host:      "127.0.0.1",
			Port:      port,
			JetStream: true,
			StoreDir:  storeDir,
			NoLog:     true,
		}
	}

	startServer := func() *server.Server {
		ns, serr := server.NewServer(buildOpts())
		require.NoError(t, serr)
		go ns.Start()
		require.True(t, ns.ReadyForConnections(5*time.Second),
			"embedded NATS not ready for connections")
		return ns
	}

	nsPtr := &serverHolder{srv: startServer()}
	t.Cleanup(func() {
		if nsPtr.srv != nil {
			nsPtr.srv.Shutdown()
			nsPtr.srv.WaitForShutdown()
		}
	})

	nc, err := nats.Connect(nsPtr.srv.ClientURL(),
		nats.Timeout(2*time.Second),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(maxReconnect),
		nats.ReconnectWait(100*time.Millisecond),
	)
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	stopNATS := func(t *testing.T) {
		t.Helper()
		require.NotNil(t, nsPtr.srv)
		nsPtr.srv.Shutdown()
		nsPtr.srv.WaitForShutdown()
		nsPtr.srv = nil
	}

	restartNATS := func(t *testing.T) {
		t.Helper()
		require.Nil(t, nsPtr.srv)
		nsPtr.srv = startServer()
	}

	return nc, stopNATS, restartNATS
}
