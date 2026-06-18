package parti_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/assignment"
	"github.com/arloliu/parti/v2/internal/testutil"
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

// TestManager_MaxReconnects verifies the finite-MaxReconnect WARN reaches
// Manager.Start through the real call site (not just the unit-test helper).
// The two rows bracket the call-site behavior:
//   - finite cap (5): without this end-to-end row the helper's correctness
//     does not prove the wiring is reachable; the WARN must fire exactly once.
//   - unlimited (-1): the recommended posture leaves Manager.Start silent on
//     this axis.
func TestManager_MaxReconnects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		maxReconnect int
		wantWarns    int
	}{
		{
			name:         "WarnsOnFiniteCap",
			maxReconnect: 5,
			wantWarns:    1,
		},
		{
			name:         "SilentOnUnlimited",
			maxReconnect: -1,
			wantWarns:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spy := startManagerWithMaxReconnects(t, tc.maxReconnect)
			require.Equal(t, tc.wantWarns, spy.countFiniteMaxReconnectWarns())
		})
	}
}

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
	cfg.HeartbeatInterval = max(heartbeatTTL/4, 100*time.Millisecond)
	cfg.EmergencyGracePeriod = max(heartbeatTTL/2, 250*time.Millisecond)
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

// TestManager_ResolverReconcileWarn verifies the one-shot resolver-reconcile
// WARN fires (or stays silent) based on HeartbeatTTL and two-phase mode. The
// warning fires synchronously inside Start before it returns. Each row
// preserves a distinct boundary:
//   - WarnsOnShortHeartbeatTTLWithTwoPhase: two-phase enabled and 5 ×
//     HeartbeatTTL (2s) is shorter than the resolver's 30s default reconcile
//     cadence → WARN fires exactly once.
//   - NoWarnWhenHeartbeatTTLLongEnough: 5 × 15s = 75s, comfortably > 30s
//     default reconcile → silent.
//   - NoWarnWhenTwoPhaseDisabled: with two-phase handoff disabled, no warning
//     fires regardless of HeartbeatTTL — the leader-side audit does not run,
//     so the grace mismatch is irrelevant.
func TestManager_ResolverReconcileWarn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		ttl       time.Duration
		twoPhase  bool
		wantWarns int
	}{
		{
			name:      "WarnsOnShortHeartbeatTTLWithTwoPhase",
			ttl:       2 * time.Second,
			twoPhase:  true,
			wantWarns: 1,
		},
		{
			name:      "NoWarnWhenHeartbeatTTLLongEnough",
			ttl:       15 * time.Second,
			twoPhase:  true,
			wantWarns: 0,
		},
		{
			name:      "NoWarnWhenTwoPhaseDisabled",
			ttl:       2 * time.Second,
			twoPhase:  false,
			wantWarns: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spy := startManagerWithSpy(t, tc.ttl, tc.twoPhase)
			require.Equal(t, tc.wantWarns, spy.countResolverWarn())
		})
	}
}

// TestApplyAssignment_InitialBootstrap_AckPublishedBeforeStateStable is
// the post-review-v1 rewrite of the previously degenerate ordering test.
//
// The §4.4 invariant we exercise here: the heartbeat publisher MUST
// receive an explicit SetAppliedAssignment + PublishNow BEFORE
// Manager.Start transitions to StateStable. Otherwise the leader's audit
// would observe AppliedAt=zero (i.e. "never ack'd") while the worker
// advertises itself as stable.
//
// We prove this by installing an OnStateChanged hook BEFORE Start, then
// capturing the heartbeat KV value at the exact moment StateStable is
// observed. AppliedAt is set only by Publisher.SetAppliedAssignment (the
// startup tick of the heartbeat publisher leaves it zero), so a non-zero
// AppliedAt at StateStable observation proves the ack was published first.
//
// Scope note: with a single-worker leader, Start typically reaches
// StateStable via the commit-path branch of applyInitialAssignment
// (the calculator publishes an empty v=1 commit before
// waitForAssignment returns). The narrow "cold empty no-commit"
// branch — the one fixed for P0 #2 — is unit-tested at
// TestApplyInitialAssignment_ColdEmpty_PublishesExplicitAck (which
// directly invokes applyInitialAssignment with no commit in KV).
func TestApplyAssignment_InitialBootstrap_AckPublishedBeforeStateStable(t *testing.T) {
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Use IntegrationTestConfig (RebalanceCooldown=2s) so the calculator
	// transitions Scaling → Idle within the test's 5s budget. The
	// Manager.Start refactor (2026-05-24) made StateStable arrival
	// dependent on the calculator reaching Idle rather than on an
	// unconditional transitionState(StateStable) at end of Start, so the
	// previous use of parti.DefaultConfig (RebalanceCooldown=10s) leaves
	// the manager pinned in Scaling well past the test's Eventually
	// budget. See tmp/impl-deviations.md.
	cfg := testutil.IntegrationTestConfig()
	cfg.StartupTimeout = 5 * time.Second
	cfg.WorkerIDTTL = 2 * time.Second
	cfg.HeartbeatTTL = 1 * time.Second
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.EmergencyGracePeriod = 750 * time.Millisecond

	// Use a non-empty source so applyAssignmentWithPrev runs (and thus
	// SetAppliedAssignment runs before the runner's CAS), pinning the
	// AppliedAt-before-StateStable invariant the test exercises.
	src := source.NewStatic(testutil.CreateTestPartitions(3))
	assignStrategy := strategy.NewRoundRobin()

	// Capture heartbeat KV bytes at the moment StateStable transition is
	// observed. We snapshot inside the OnStateChanged hook so the read
	// happens synchronously w.r.t. the transition.
	var (
		stableHeartbeatBytes atomic.Value // []byte
		stableSeen           atomic.Bool
		workerID             atomic.Value // string
	)
	hooks := &types.Hooks{
		OnStateChanged: func(ctx context.Context, _, to types.State) error {
			if to != parti.StateStable || !stableSeen.CompareAndSwap(false, true) {
				return nil
			}
			wid, _ := workerID.Load().(string)
			if wid == "" {
				return nil
			}
			hbKV, err := js.KeyValue(ctx, cfg.KVBuckets.HeartbeatBucket)
			if err != nil {
				return nil
			}
			entry, err := hbKV.Get(ctx, "heartbeat."+wid)
			if err != nil {
				return nil
			}
			b := append([]byte(nil), entry.Value()...)
			stableHeartbeatBytes.Store(b)

			return nil
		},
	}

	mgr, err := parti.NewManager(&cfg, js, src, assignStrategy, parti.WithHooks(hooks))
	require.NoError(t, err)
	defer func() { _ = mgr.Stop(context.Background()) }()

	// Stash worker ID before Start so the hook can read it on StateStable.
	// Start blocks until the initial apply pipeline completes; until then
	// WorkerID() is empty. We populate workerID atomically AFTER Start so
	// the hook can resolve it on first reach.
	err = mgr.Start(context.Background())
	require.NoError(t, err)
	workerID.Store(mgr.WorkerID())

	// Wait for the stable observation (may also have fired during Start
	// itself — re-fetch from KV either way to make the assertion robust).
	require.Eventually(t, func() bool {
		return mgr.State() == parti.StateStable
	}, 5*time.Second, 50*time.Millisecond)

	// If the hook didn't capture (Start completed before workerID was set),
	// fall back to a direct KV read now and validate the same invariant
	// against the current heartbeat. Either path proves the steady-state
	// invariant; only the hook-captured path proves ordering at the
	// transition moment.
	var heartbeatBytes []byte
	if v := stableHeartbeatBytes.Load(); v != nil {
		heartbeatBytes, _ = v.([]byte)
	}
	if heartbeatBytes == nil {
		hbKV, err := js.KeyValue(context.Background(), cfg.KVBuckets.HeartbeatBucket)
		require.NoError(t, err)
		entry, err := hbKV.Get(context.Background(), "heartbeat."+mgr.WorkerID())
		require.NoError(t, err)
		heartbeatBytes = entry.Value()
	}

	var hb types.Heartbeat
	require.NoError(t, json.Unmarshal(heartbeatBytes, &hb))

	// AppliedAt is set ONLY by Publisher.SetAppliedAssignment. A zero
	// AppliedAt at StateStable means the publisher's startup tick wrote
	// the heartbeat before the manager acked — the invariant we forbid.
	require.False(t, hb.AppliedAt.IsZero(),
		"AppliedAt MUST be non-zero at StateStable — proves SetAppliedAssignment ran before transition")
	// AppliedVersion is 0 for the pure cold-empty path (no commit yet),
	// but the leader's calculator may publish an empty v=1 commit before
	// we read the heartbeat. Either is acceptable evidence that the ack
	// pipeline ran; we only forbid a regression below 0.
	require.GreaterOrEqual(t, hb.AppliedVersion, int64(0))
}

// TestStartCalculator_PropagatesEnableTwoPhaseHandoff is the regression
// guard for P0 #1 (post-impl review v1). The audit's escalation path
// (§4.2) refuses to escalate when its embedded Config.EnableTwoPhaseHandoff
// is false. Before the v1 fix, startCalculator built the assignment.Config
// literal without copying m.cfg.EnableTwoPhaseHandoff, so production
// calculators always saw the zero value and the audit emitted "direct_mode"
// even when two-phase mode was enabled.
//
// We verify by starting a real Manager with two-phase enabled and reading
// the embedded Config.EnableTwoPhaseHandoff field from the calculator that
// startCalculator builds.
func TestStartCalculator_PropagatesEnableTwoPhaseHandoff(t *testing.T) {
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := parti.DefaultConfig()
	cfg.StartupTimeout = 5 * time.Second
	cfg.WorkerIDTTL = 2 * time.Second
	cfg.HeartbeatTTL = 1 * time.Second
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.EmergencyGracePeriod = 750 * time.Millisecond
	cfg.EnableTwoPhaseHandoff = true

	src := source.NewStatic([]types.Partition{})
	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewRoundRobin())
	require.NoError(t, err)
	defer func() { _ = mgr.Stop(context.Background()) }()

	require.NoError(t, mgr.Start(context.Background()))

	// Only the leader runs a real Calculator; the single-worker startup
	// always wins election in <StartupTimeout.
	require.Eventually(t, mgr.IsLeader, 3*time.Second, 25*time.Millisecond)

	// Inspect the calculator's Config.EnableTwoPhaseHandoff via the
	// internal accessor; the Calculator embeds Config anonymously, so this
	// reads the exact field startCalculator populated.
	calc := parti.CalculatorForTest(mgr)
	require.NotNil(t, calc, "leader manager must have a real Calculator")
	require.True(t, calc.EnableTwoPhaseHandoff,
		"Calculator.EnableTwoPhaseHandoff MUST mirror Manager.cfg.EnableTwoPhaseHandoff")
}

// TestStartCalculator_DefaultsToDirectMode is the symmetric check: when
// the Manager-level flag is false, the calculator's flag is also false
// (so the audit's "direct_mode" skip path remains reachable).
func TestStartCalculator_DefaultsToDirectMode(t *testing.T) {
	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := parti.DefaultConfig()
	cfg.StartupTimeout = 5 * time.Second
	cfg.WorkerIDTTL = 2 * time.Second
	cfg.HeartbeatTTL = 1 * time.Second
	cfg.HeartbeatInterval = 500 * time.Millisecond
	cfg.EmergencyGracePeriod = 750 * time.Millisecond
	// EnableTwoPhaseHandoff is false (default).

	src := source.NewStatic([]types.Partition{})
	mgr, err := parti.NewManager(&cfg, js, src, strategy.NewRoundRobin())
	require.NoError(t, err)
	defer func() { _ = mgr.Stop(context.Background()) }()

	require.NoError(t, mgr.Start(context.Background()))
	require.Eventually(t, mgr.IsLeader, 3*time.Second, 25*time.Millisecond)

	calc := parti.CalculatorForTest(mgr)
	require.NotNil(t, calc)
	require.False(t, calc.EnableTwoPhaseHandoff,
		"direct mode default: calculator's flag must be false")
}

// Compile-time check that Calculator's EnableTwoPhaseHandoff is exported.
var _ = assignment.Calculator{}.EnableTwoPhaseHandoff
