package parti_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// maxReconnectSpy captures WARN messages so the integration test can
// assert that warnOnFiniteMaxReconnects fires (or stays silent) when
// reached via Manager.Start's real call site — not just in isolation.
type maxReconnectSpy struct {
	mu    sync.Mutex
	warns []string
}

func (l *maxReconnectSpy) Debug(string, ...any) {}
func (l *maxReconnectSpy) Info(string, ...any)  {}
func (l *maxReconnectSpy) Warn(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}
func (l *maxReconnectSpy) Error(string, ...any) {}
func (l *maxReconnectSpy) Fatal(string, ...any) {}

func (l *maxReconnectSpy) countFiniteMaxReconnectWarns() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, w := range l.warns {
		if strings.Contains(w, "finite MaxReconnect") {
			n++
		}
	}

	return n
}

var _ types.Logger = (*maxReconnectSpy)(nil)

// startManagerWithMaxReconnects spins up an embedded NATS server, connects
// with the given MaxReconnect setting (caller-controlled — distinct from
// the partitest helper which now uses the recommended -1), and starts a
// Manager wired with a spy logger so the test can verify the warning
// reaches Manager.Start via the real call site.
func startManagerWithMaxReconnects(t *testing.T, maxReconnect int) *maxReconnectSpy {
	t.Helper()

	srv, defaultNC := partitest.StartEmbeddedNATS(t)
	defaultNC.Close() // unused; we reconnect with caller-controlled opts below

	nc, err := nats.Connect(srv.ClientURL(),
		nats.Timeout(2*time.Second),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(maxReconnect),
	)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := parti.DefaultConfig()
	cfg.StartupTimeout = 5 * time.Second

	// Unique bucket names so concurrent t.Parallel runs do not collide.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	cfg.KVBuckets.StableIDBucket = "parti-stable-" + suffix
	cfg.KVBuckets.ElectionBucket = "parti-election-" + suffix
	cfg.KVBuckets.HeartbeatBucket = "parti-heartbeat-" + suffix
	cfg.KVBuckets.AssignmentBucket = "parti-assignment-" + suffix
	cfg.KVBuckets.HandoffBucket = "parti-handoff-" + suffix

	spy := &maxReconnectSpy{}

	src := source.NewStatic([]types.Partition{})
	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewRoundRobin(), parti.WithLogger(spy))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	require.NoError(t, mgr.Start(context.Background()))

	return spy
}

// TestManager_MaxReconnects_WarnsOnFiniteCap verifies the warning reaches
// Manager.Start through the real call site (not just the unit-test helper)
// when the caller-owned nats.Conn was configured with a finite reconnect
// budget. Without this end-to-end test the helper's correctness does not
// prove the wiring is reachable.
func TestManager_MaxReconnects_WarnsOnFiniteCap(t *testing.T) {
	t.Parallel()
	spy := startManagerWithMaxReconnects(t, 5)
	require.Equal(t, 1, spy.countFiniteMaxReconnectWarns(),
		"Manager.Start must emit the finite-MaxReconnect WARN exactly once on a finite cap")
}

// TestManager_MaxReconnects_SilentOnUnlimited verifies the recommended
// posture (-1) leaves Manager.Start silent on this axis. Pairs with the
// finite-cap test to bracket the call-site behavior.
func TestManager_MaxReconnects_SilentOnUnlimited(t *testing.T) {
	t.Parallel()
	spy := startManagerWithMaxReconnects(t, -1)
	require.Equal(t, 0, spy.countFiniteMaxReconnectWarns(),
		"Manager.Start must NOT warn when MaxReconnects is unlimited (-1)")
}
