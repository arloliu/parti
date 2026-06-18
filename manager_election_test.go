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
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/stableid"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestManager_LeadershipLoss_StateTransition tests that losing leadership
// while in Scaling/Rebalancing/Emergency state transitions correctly.
func TestManager_LeadershipLoss_StateTransition(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Setup embedded NATS
	_, nc := partitest.StartEmbeddedNATS(t)

	// Create test logger for debugging
	logger := logging.NewTest(t)

	cfg := Config{
		WorkerIDPrefix:        "worker",
		WorkerIDMin:           0,
		WorkerIDMax:           10,
		WorkerIDTTL:           10 * time.Second,
		HeartbeatInterval:     1 * time.Second,
		HeartbeatTTL:          2 * time.Second,
		ElectionTimeout:       2 * time.Second, // Short timeout to trigger leadership changes
		StartupTimeout:        15 * time.Second,
		ShutdownTimeout:       5 * time.Second,
		ColdStartWindow:       1 * time.Second,
		PlannedScaleWindow:    500 * time.Millisecond,
		RestartDetectionRatio: 0.5,
		RebalanceCooldown:     100 * time.Millisecond, // Short cooldown for testing fast detection
	}

	// Create partition source
	partitions := make([]types.Partition, 5)
	for i := range partitions {
		partitions[i] = types.Partition{
			Keys:   []string{"partition-" + string(rune('A'+i))},
			Weight: 100,
		}
	}
	src := source.NewStatic(partitions)
	strategy := strategy.NewConsistentHash()

	// Track state transitions (thread-safe)
	var stateTransitionsMu sync.RWMutex
	stateTransitions := make([]string, 0)
	hooks := &Hooks{
		OnStateChanged: func(_ context.Context, from, to types.State) error {
			transition := from.String() + " → " + to.String()
			stateTransitionsMu.Lock()
			stateTransitions = append(stateTransitions, transition)
			stateTransitionsMu.Unlock()
			t.Logf("State transition: %s", transition)

			return nil
		},
	}

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create first worker
	mgr1, err := NewManager(&cfg, js, src, strategy, WithHooks(hooks), WithLogger(logger))
	require.NoError(t, err)

	err = mgr1.Start(ctx)
	require.NoError(t, err)
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr1.Stop(stopCtx)
	}()

	// Wait for worker to become leader and reach Stable
	require.Eventually(t, func() bool {
		return mgr1.IsLeader() && mgr1.State() == StateStable
	}, 10*time.Second, 200*time.Millisecond, "worker 1 did not become leader")

	t.Log("Worker 1 is leader and stable")

	// Create second worker (will trigger scaling in leader)
	mgr2, err := NewManager(&cfg, js, src, strategy, WithLogger(logger))
	require.NoError(t, err)

	// Ensure mgr2 is stopped before test ends
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr2.Stop(stopCtx)
	}()

	// Track mgr2 start result
	mgr2Started := make(chan error, 1)
	go func() {
		startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mgr2Started <- mgr2.Start(startCtx)
	}()

	// Wait for mgr2 to reach a stable state (either WaitingAssignment or Stable)
	require.Eventually(t, func() bool {
		state := mgr2.State()
		t.Logf("Worker 2 state: %s", state.String())
		return state == StateWaitingAssignment || state == StateStable
	}, 3*time.Second, 100*time.Millisecond, "worker 2 did not start successfully")

	// Wait for leader to detect new worker and react to scaling event
	// The leader should transition through Scaling -> Rebalancing -> Stable
	// Due to the short scaling window (500ms), we might miss Scaling state when polling,
	// so we check the recorded state transitions for evidence of rebalancing activity
	require.Eventually(t, func() bool {
		stateTransitionsMu.RLock()
		defer stateTransitionsMu.RUnlock()
		for _, transition := range stateTransitions {
			if transition == "Stable → Scaling" ||
				transition == "Stable → Rebalancing" ||
				transition == "Scaling → Rebalancing" ||
				transition == "WaitingAssignment → Scaling" ||
				transition == "WaitingAssignment → Rebalancing" {
				t.Logf("Found rebalancing activity: %s", transition)
				return true
			}
		}

		return false
	}, 5*time.Second, 100*time.Millisecond, "leader did not enter Scaling or Rebalancing state")

	t.Log("Leader entered rebalancing state (Scaling or Rebalancing)")

	// Force leadership change by stopping first worker's election participation
	// This simulates losing leadership while in rebalancing state (Scaling or Rebalancing)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = mgr1.Stop(stopCtx)
	stopCancel()
	require.NoError(t, err)

	// Check that state transitions included proper cleanup
	// Should see either:
	// - Scaling → Rebalancing → Stable → Shutdown (if completed normally)
	// - Rebalancing → Stable → Shutdown (if we missed Scaling due to timing)
	// - Scaling → Stable → Shutdown (if lost leadership mid-scaling)
	// - Scaling → WaitingAssignment (if lost leadership with no assignment)

	t.Log("State transitions:")
	stateTransitionsMu.RLock()
	for i, transition := range stateTransitions {
		t.Logf("  %d. %s", i+1, transition)
	}
	stateTransitionsMu.RUnlock()

	// Verify we had a valid transition involving rebalancing activity
	// Accept either Scaling or Rebalancing as evidence of the rebalancing process
	foundValidExit := false
	stateTransitionsMu.RLock()
	for _, transition := range stateTransitions {
		if transition == "Scaling → Rebalancing" ||
			transition == "Rebalancing → Stable" ||
			transition == "Scaling → Stable" ||
			transition == "Scaling → WaitingAssignment" ||
			transition == "Scaling → Shutdown" ||
			transition == "Rebalancing → Shutdown" ||
			transition == "Stable → Rebalancing" {
			foundValidExit = true
			break
		}
	}
	stateTransitionsMu.RUnlock()

	require.True(t, foundValidExit, "did not find valid rebalancing transition")
	t.Log("Successfully transitioned through rebalancing states")
}

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

// TestWarnOnOperationTimeoutVsElection covers the F9-A companion
// warning that fires when OperationTimeout > ElectionTimeout/3 — at
// that ratio a single slow renew can consume the lease's three-
// attempt budget and produce false leadership flips.
func TestWarnOnOperationTimeoutVsElection(t *testing.T) {
	t.Parallel()
	const warnSubstr = "OperationTimeout exceeds ElectionTimeout/3"

	cases := []struct {
		name        string
		opTimeout   time.Duration
		electionTO  time.Duration
		expectWarns int
	}{
		{"safe ratio (OT=1/6 ET)", 5 * time.Second, 30 * time.Second, 0},
		{"boundary OT=ET/3 (silent)", 10 * time.Second, 30 * time.Second, 0},
		{"OT just over ET/3 warns", 11 * time.Second, 30 * time.Second, 1},
		{"OT equals ET warns", 30 * time.Second, 30 * time.Second, 1},
		{"default pair (10s/10s) warns", 10 * time.Second, 10 * time.Second, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spy := &warnSpy{}
			warnOnOperationTimeoutVsElection(Config{
				OperationTimeout: tc.opTimeout,
				ElectionTimeout:  tc.electionTO,
			}, spy)
			require.Equal(t, tc.expectWarns, spy.countContaining(warnSubstr),
				"warn count mismatch for OperationTimeout=%v ElectionTimeout=%v",
				tc.opTimeout, tc.electionTO)
		})
	}
}

// epochSpy captures the OnDegraded callback so the F1 reproducers can
// observe (a) whether degraded mode was entered and (b) the reason
// string. degraded-mode entry is what trips the readiness probe in
// production.
type epochSpy struct {
	mu      sync.Mutex
	reasons []string
}

func (s *epochSpy) record(_ context.Context, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reasons = append(s.reasons, reason)
	return nil
}

func (s *epochSpy) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.reasons))
	copy(out, s.reasons)
	return out
}

// makeBucket creates a freshly-named bucket and returns its handle.
// Bucket names are caller-provided so tests can probe a specific
// expected name (e.g. the heartbeat bucket the Manager would pick).
func makeBucket(t *testing.T, js jetstream.JetStream, name string) jetstream.KeyValue {
	t.Helper()
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{
		Bucket:  name,
		Storage: jetstream.MemoryStorage,
	})
	require.NoError(t, err)
	return kv
}

// TestManager_F1_BucketRecreate_TripsDegraded is the primary F1
// reproducer. For each Parti-owned bucket name shape the Manager's
// captureBucketEpoch flow would record, delete and recreate the
// backing stream and assert checkBucketEpochs trips degraded mode
// with the expected reason. The test drives checkBucketEpochs
// directly so the assertion is deterministic (no polling on the
// production OperationTimeout tick).
func TestManager_F1_BucketRecreate_TripsDegraded(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx := t.Context()
	cases := []string{
		"parti-epoch-test-stableid",
		"parti-epoch-test-election",
		"parti-epoch-test-heartbeat",
		"parti-epoch-test-assignment",
		"parti-epoch-test-handoff",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bucket := name + "-" + t.Name()[len("TestManager_F1_BucketRecreate_TripsDegraded/"):]
			// Bucket names are not allowed to contain '/'; strip just in case.
			bucket = strings.ReplaceAll(bucket, "/", "-")

			kv := makeBucket(t, js, bucket)

			spy := &epochSpy{}
			m := &Manager{
				cfg: Config{
					OperationTimeout: 500 * time.Millisecond,
					DegradedAlert:    DegradedAlertConfig{AlertInterval: time.Second},
				},
				logger: logging.NewNop(),
				hooks:  &Hooks{OnDegraded: spy.record},
				js:     js,
			}
			m.state.Store(int32(StateStable))
			m.degraded.Store(nil)
			m.metrics = nopManagerMetrics{}
			testCtx, cancel := context.WithCancel(ctx)
			m.ctx = testCtx
			m.cancel = cancel
			t.Cleanup(cancel)

			m.captureBucketEpoch(ctx, bucket, kv)
			require.Contains(t, m.bucketEpochs, bucket,
				"sanity: captureBucketEpoch must have recorded the bucket")

			// Wipe and recreate so the backing stream gets a fresh Created.
			require.NoError(t, js.DeleteKeyValue(ctx, bucket))
			// time.Now() advances; recreate gets a strictly later Created.
			time.Sleep(50 * time.Millisecond)
			kv2 := makeBucket(t, js, bucket)
			liveCreated, err := kvutil.BucketStreamCreated(ctx, kv2)
			require.NoError(t, err)
			require.True(t, liveCreated.After(m.bucketEpochs[bucket].created),
				"sanity: recreate must produce a later Created timestamp")

			// The cached kv handle points at the OLD stream and would
			// return an error on a status read; replace it with the new
			// handle to model the production path where the Manager's
			// cached handle silently re-binds after the recreate.
			ep := m.bucketEpochs[bucket]
			ep.kv = kv2
			m.bucketEpochs[bucket] = ep

			m.checkBucketEpochs(ctx)
			// OnDegraded fires asynchronously via invokeHook → wg.Go;
			// poll briefly for the hook to land.
			require.Eventually(t, func() bool {
				return len(spy.snapshot()) >= 1
			}, time.Second, 10*time.Millisecond,
				"epoch fence must fire OnDegraded after checkBucketEpochs returns")
			reasons := spy.snapshot()
			require.Len(t, reasons, 1,
				"epoch fence must fire OnDegraded exactly once")
			require.Equal(t, "bucket-recreated:"+bucket, reasons[0])
		})
	}
}

// TestManager_F1_HappyPath_NoDegraded confirms that a bucket whose
// Created timestamp is unchanged (no recreate) does NOT trip the
// fence. False-positive guard — the fence must be silent on
// healthy operation.
func TestManager_F1_HappyPath_NoDegraded(t *testing.T) {
	t.Parallel()
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := "parti-epoch-happy"
	kv := makeBucket(t, js, bucket)

	spy := &epochSpy{}
	m := &Manager{
		cfg: Config{
			OperationTimeout: 500 * time.Millisecond,
			DegradedAlert:    DegradedAlertConfig{AlertInterval: time.Second},
		},
		logger: logging.NewNop(),
		hooks:  &Hooks{OnDegraded: spy.record},
		js:     js,
	}
	m.state.Store(int32(StateStable))
	m.metrics = nopManagerMetrics{}
	ctx, cancel := context.WithCancel(t.Context())
	m.ctx = ctx
	m.cancel = cancel
	t.Cleanup(cancel)

	m.captureBucketEpoch(ctx, bucket, kv)

	for range 3 {
		m.checkBucketEpochs(ctx)
	}
	require.Empty(t, spy.snapshot(),
		"three consecutive ticks against an unchanged bucket must NOT trip degraded")
}

// nopManagerMetrics is the minimum metrics surface monitorBucketEpochs
// needs (enterDegraded calls SetDegradedMode). Implements just enough
// of MetricsCollector to satisfy the field type.
type nopManagerMetrics struct{ types.MetricsCollector }

func (nopManagerMetrics) SetDegradedMode(float64)                     {}
func (nopManagerMetrics) RecordStateTransition(_, _ State, _ float64) {}

// TestEpochMismatchOutstanding_BehaviorAndTiming is the focused unit + WI-3
// timing guard for the Family A recovery-exit re-probe. attemptRecoveryFromDegraded
// runs on the 1s connection-monitor goroutine, so epochMismatchOutstanding's
// per-tick blocking probe I/O is a timing concern (a real stall risk on health
// detection if a bucket is unreachable). This test pins three regimes:
//   - reachable + unchanged buckets: returns false, FAST (the realistic recovery
//     scenario — buckets are reachable because the exit only runs when connected).
//   - a recreated bucket: returns true (the wipe-and-recreate the gate must catch).
//   - an unreachable server: returns false (probe errors are skipped, not
//     actionable) and is BOUNDED by ~len(buckets) * OperationTimeout — it must not
//     hang the connection-monitor goroutine.
func TestEpochMismatchOutstanding_BehaviorAndTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-NATS test in short mode")
	}

	m, _, _, _ := newTestManager(t)
	srv, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	m.js = js
	m.cfg.OperationTimeout = 2 * time.Second

	ctx := m.ctx
	buckets := []string{"wi3-b0", "wi3-b1", "wi3-b2", "wi3-b3"}
	for _, b := range buckets {
		kv := partitest.CreateJetStreamKV(t, nc, b)
		m.captureBucketEpoch(ctx, b, kv)
	}
	require.Len(t, m.bucketEpochs, len(buckets), "all bucket epochs must be captured")

	// Regime 1: reachable + unchanged -> false, and fast (well under one
	// OperationTimeout for all four buckets combined).
	start := time.Now()
	require.False(t, m.epochMismatchOutstanding(ctx),
		"unchanged reachable buckets must not report a recreate")
	reachableLatency := time.Since(start)
	t.Logf("WI-3 timing: epochMismatchOutstanding over %d reachable buckets = %s", len(buckets), reachableLatency)
	require.Less(t, reachableLatency, m.cfg.OperationTimeout,
		"the reachable-bucket re-probe must be fast (a few round-trips), well under one OperationTimeout")

	// Regime 2: recreate one bucket -> the live Created differs -> true.
	require.NoError(t, js.DeleteKeyValue(ctx, buckets[1]))
	_ = partitest.CreateJetStreamKV(t, nc, buckets[1])
	require.True(t, m.epochMismatchOutstanding(ctx),
		"a recreated bucket's live Created must mismatch the captured epoch -> outstanding")

	// Regime 3 (the WI-3 worst case): the server is gone -> every probe errors and
	// is skipped (returns false), and the whole call is BOUNDED, not a hang. Use a
	// small OperationTimeout so the bound is tight and the test is quick.
	m.cfg.OperationTimeout = 300 * time.Millisecond
	srv.Shutdown()
	srv.WaitForShutdown()

	start = time.Now()
	require.False(t, m.epochMismatchOutstanding(ctx),
		"with the server down every probe errors and is skipped (not actionable) -> false")
	downLatency := time.Since(start)
	t.Logf("WI-3 timing: epochMismatchOutstanding with server DOWN over %d buckets = %s (OperationTimeout=%s each)",
		len(buckets), downLatency, m.cfg.OperationTimeout)
	// Hard upper bound: the per-bucket context.WithTimeout caps each probe, so the
	// total inline block on the connection-monitor goroutine cannot exceed
	// len(buckets)*OperationTimeout plus a small scheduling margin. This is the
	// guarantee that the re-probe degrades gracefully rather than wedging.
	maxBound := time.Duration(len(buckets))*m.cfg.OperationTimeout + 2*time.Second
	require.Less(t, downLatency, maxBound,
		"epochMismatchOutstanding must stay bounded by len(buckets)*OperationTimeout when buckets are unreachable, not hang")
}

func TestSelectAuthority_TableDriven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		commit   *types.AssignmentCommit
		legacy   *types.Assignment
		lastSeen uint64
		want     AuthorityChoice
	}{
		{
			name:     "Case1_CommitOnly",
			commit:   &types.AssignmentCommit{Version: 1, LeaderRevision: 10},
			legacy:   nil,
			lastSeen: 0,
			want:     AuthorityCommit,
		},
		{
			name:     "Case1_CommitFresherThanLegacy",
			commit:   &types.AssignmentCommit{LeaderRevision: 20},
			legacy:   &types.Assignment{LeaderRevision: 15},
			lastSeen: 10,
			want:     AuthorityCommit,
		},
		{
			name:     "Case1_CommitEqualLegacy_TieGoesToCommit",
			commit:   &types.AssignmentCommit{LeaderRevision: 20},
			legacy:   &types.Assignment{LeaderRevision: 20},
			lastSeen: 10,
			want:     AuthorityCommit,
		},
		{
			name:     "Case2_LegacyFresherThanCommit_HandoffWindow",
			commit:   &types.AssignmentCommit{LeaderRevision: 10},
			legacy:   &types.Assignment{LeaderRevision: 20},
			lastSeen: 10,
			want:     AuthorityLegacyAlias,
		},
		{
			name:     "Case2_LegacyOnly_NoCommit",
			commit:   nil,
			legacy:   &types.Assignment{LeaderRevision: 10},
			lastSeen: 10,
			want:     AuthorityLegacyAlias,
		},
		{
			name:     "Case3_LegacyBelowLastSeen",
			commit:   nil,
			legacy:   &types.Assignment{LeaderRevision: 5},
			lastSeen: 10,
			want:     AuthorityNone,
		},
		{
			name:     "Case3_NoCommitNoLegacy",
			commit:   nil,
			legacy:   nil,
			lastSeen: 0,
			want:     AuthorityNone,
		},
		{
			// RollingUpgrade_NewLeaderToOldLeaderHandoff_AliasOverridesStaleCommit:
			// the §3.6 "previously-fragile handoff" case — a new leader produces a
			// higher-LeaderRevision alias than the existing commit; the dual-read
			// selector picks the alias (legacy.LR=25 > commit.LR=20 ⇒ legacy wins).
			name:     "RollingUpgrade_AliasOverridesStaleCommit",
			commit:   &types.AssignmentCommit{Version: 10, LeaderRevision: 20},
			legacy:   &types.Assignment{Version: 11, LeaderRevision: 25},
			lastSeen: 0,
			want:     AuthorityLegacyAlias,
		},
		{
			// RollingUpgrade_AliasFresherThanCommit_NextCommitTakesOver: continues
			// the previous scenario — a new commit at an even higher LeaderRevision
			// arrives and once again wins the selector (commit.LR=30 > legacy.LR=25
			// ⇒ commit wins).
			name:     "RollingUpgrade_NextCommitTakesOver",
			commit:   &types.AssignmentCommit{Version: 12, LeaderRevision: 30},
			legacy:   &types.Assignment{Version: 11, LeaderRevision: 25},
			lastSeen: 25,
			want:     AuthorityCommit,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := selectAuthority(tc.commit, tc.legacy, tc.lastSeen)
			require.Equal(t, tc.want, got)
		})
	}
}
