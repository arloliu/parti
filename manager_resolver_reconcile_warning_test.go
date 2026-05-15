package parti_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// logEntry captures a single structured log line for assertion.
type logEntry struct {
	Level string
	Msg   string
	KV    []any
}

// spyLogger records every log call so tests can assert on level + substring.
// Safe for concurrent use; the Manager's background goroutines log freely.
type spyLogger struct {
	mu      sync.Mutex
	entries []logEntry
	t       *testing.T
}

func newSpyLogger(t *testing.T) *spyLogger { return &spyLogger{t: t} }

func (l *spyLogger) record(level, msg string, kv []any) {
	l.mu.Lock()
	l.entries = append(l.entries, logEntry{Level: level, Msg: msg, KV: kv})
	l.mu.Unlock()
	// Echo into test output for debuggability without failing the test.
	l.t.Logf("%s: %s %v", level, msg, kv)
}

func (l *spyLogger) Debug(msg string, kv ...any) { l.record("DEBUG", msg, kv) }
func (l *spyLogger) Info(msg string, kv ...any)  { l.record("INFO", msg, kv) }
func (l *spyLogger) Warn(msg string, kv ...any)  { l.record("WARN", msg, kv) }
func (l *spyLogger) Error(msg string, kv ...any) { l.record("ERROR", msg, kv) }
func (l *spyLogger) Fatal(msg string, kv ...any) {
	l.record("FATAL", msg, kv)
	l.t.Fatalf("FATAL: %s %v", msg, kv)
}

// countResolverWarn counts how many WARN entries contain the
// resolver-reconcile-gap substring.
func (l *spyLogger) countResolverWarn() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.entries {
		if e.Level != "WARN" {
			continue
		}
		if strings.Contains(e.Msg, "claim resolver reconcile interval") {
			n++
		}
	}

	return n
}

// Compile-time assertion: spyLogger satisfies parti.Logger.
var _ types.Logger = (*spyLogger)(nil)

// startManagerWithSpy boots a Manager against an embedded NATS with the given
// HeartbeatTTL / EnableTwoPhaseHandoff and returns the spy for assertion.
func startManagerWithSpy(t *testing.T, heartbeatTTL time.Duration, twoPhase bool) *spyLogger {
	t.Helper()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := parti.DefaultConfig()
	cfg.StartupTimeout = 5 * time.Second
	// WorkerIDTTL must be >= HeartbeatTTL per Config.Validate's gtefield tag.
	cfg.WorkerIDTTL = 2 * heartbeatTTL
	cfg.HeartbeatTTL = heartbeatTTL
	// HeartbeatInterval must be < HeartbeatTTL per Validate().
	cfg.HeartbeatInterval = heartbeatTTL / 4
	if cfg.HeartbeatInterval < 100*time.Millisecond {
		cfg.HeartbeatInterval = 100 * time.Millisecond
	}
	cfg.EmergencyGracePeriod = heartbeatTTL / 2
	if cfg.EmergencyGracePeriod < 250*time.Millisecond {
		cfg.EmergencyGracePeriod = 250 * time.Millisecond
	}
	cfg.EnableTwoPhaseHandoff = twoPhase

	// Use unique bucket names per test to avoid cross-pollution on a
	// shared embedded NATS instance.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	cfg.KVBuckets.StableIDBucket = "parti-stable-" + suffix
	cfg.KVBuckets.ElectionBucket = "parti-election-" + suffix
	cfg.KVBuckets.HeartbeatBucket = "parti-heartbeat-" + suffix
	cfg.KVBuckets.AssignmentBucket = "parti-assignment-" + suffix
	cfg.KVBuckets.HandoffBucket = "parti-handoff-" + suffix

	spy := newSpyLogger(t)

	src := source.NewStatic([]types.Partition{})
	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewRoundRobin(), parti.WithLogger(spy))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	require.NoError(t, mgr.Start(context.Background()))

	return spy
}

// TestManager_WarnsOnShortHeartbeatTTLWithTwoPhase verifies the one-shot
// WARN fires when EnableTwoPhaseHandoff is true and 5 × HeartbeatTTL is
// shorter than the resolver's 30s default reconcile cadence.
func TestManager_WarnsOnShortHeartbeatTTLWithTwoPhase(t *testing.T) {
	t.Parallel()

	spy := startManagerWithSpy(t, 2*time.Second, true)

	// The warning fires synchronously inside Start before it returns.
	require.Equal(t, 1, spy.countResolverWarn(),
		"resolver-reconcile WARN must fire exactly once at startup")
}

// TestManager_NoWarnWhenHeartbeatTTLLongEnough verifies the warning is
// silent when 5 × HeartbeatTTL >= the resolver reconcile default.
func TestManager_NoWarnWhenHeartbeatTTLLongEnough(t *testing.T) {
	t.Parallel()

	// 5 × 15s = 75s, comfortably > 30s default reconcile.
	spy := startManagerWithSpy(t, 15*time.Second, true)
	require.Equal(t, 0, spy.countResolverWarn(),
		"resolver-reconcile WARN must NOT fire when HeartbeatTTL is long enough")
}

// TestManager_NoWarnWhenTwoPhaseDisabled verifies that with two-phase
// handoff disabled, no warning fires regardless of HeartbeatTTL — the
// leader-side audit does not run, so the grace mismatch is irrelevant.
func TestManager_NoWarnWhenTwoPhaseDisabled(t *testing.T) {
	t.Parallel()

	spy := startManagerWithSpy(t, 2*time.Second, false)
	require.Equal(t, 0, spy.countResolverWarn(),
		"resolver-reconcile WARN must NOT fire when EnableTwoPhaseHandoff is false")
}
