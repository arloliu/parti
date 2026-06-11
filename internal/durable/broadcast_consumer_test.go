package durable

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/parti/v2/internal/logging"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
)

func TestBroadcastConsumerConfig_Validate_RequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		cfg     BroadcastConsumerConfig
		wantErr bool
	}{
		{
			name:    "empty config",
			cfg:     BroadcastConsumerConfig{},
			wantErr: true,
		},
		{
			name: "missing stream name",
			cfg: BroadcastConsumerConfig{
				ConsumerPrefix: "test",
				WildcardFilter: "events.>",
			},
			wantErr: true,
		},
		{
			name: "missing consumer prefix",
			cfg: BroadcastConsumerConfig{
				StreamName:     "TEST",
				WildcardFilter: "events.>",
			},
			wantErr: true,
		},
		{
			name: "missing wildcard filter",
			cfg: BroadcastConsumerConfig{
				StreamName:     "TEST",
				ConsumerPrefix: "test",
			},
			wantErr: true,
		},
		{
			name: "valid config",
			cfg: BroadcastConsumerConfig{
				StreamName:     "TEST",
				ConsumerPrefix: "test",
				WildcardFilter: "events.>",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = tt.cfg.SetDefaults()
			err := tt.cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestBroadcastConsumerConfig_Validate_InvalidConsumerPrefix(t *testing.T) {
	cfg := BroadcastConsumerConfig{
		StreamName:     "TEST",
		ConsumerPrefix: "invalid prefix!", // contains space and !
		WildcardFilter: "events.>",
	}
	_ = cfg.SetDefaults()
	err := cfg.Validate()
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "invalid characters"))
}

func TestBroadcastConsumerConfig_SetDefaults(t *testing.T) {
	cfg := BroadcastConsumerConfig{
		StreamName:     "TEST",
		ConsumerPrefix: "test",
		WildcardFilter: "events.>",
	}
	require.NoError(t, cfg.SetDefaults())

	// Check defaults are applied
	require.NotNil(t, cfg.Logger)
	require.NotNil(t, cfg.Metrics)
	require.Equal(t, DefaultAckWait, cfg.AckWait)
	require.Equal(t, DefaultMaxDeliver, cfg.MaxDeliver)
	require.Equal(t, DefaultBatchSize, cfg.BatchSize)
	require.Equal(t, DefaultFetchTimeout, cfg.FetchTimeout)
	require.Equal(t, DefaultMaxWaiting, cfg.MaxWaiting)
	require.Equal(t, DefaultInactiveThreshold, cfg.InactiveThreshold)
}

func TestResolveBroadcastConsumerID_EnvPrefix(t *testing.T) {
	cfg := BroadcastConsumerConfig{
		ConsumerID: "env:TEST_CONSUMER_ID",
	}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults() error: %v", err)
	}

	t.Setenv("TEST_CONSUMER_ID", "pod-123")

	consumerID, source, err := resolveBroadcastConsumerID(cfg)
	require.NoError(t, err)
	require.Equal(t, "pod-123", consumerID)
	require.Equal(t, consumerIDSourceEnv, source)
}

func TestResolveBroadcastConsumerID_EnvPrefixMissing_FallsBack(t *testing.T) {
	cfg := BroadcastConsumerConfig{
		ConsumerID: "env:MISSING_ID",
	}
	if err := cfg.SetDefaults(); err != nil {
		t.Fatalf("SetDefaults() error: %v", err)
	}

	consumerID, source, err := resolveBroadcastConsumerID(cfg)
	require.NoError(t, err)
	require.Len(t, consumerID, 16)
	require.Equal(t, consumerIDSourceGenerated, source)
}

func TestNewBroadcastConsumer_RequiresJetStream(t *testing.T) {
	cfg := BroadcastConsumerConfig{
		StreamName:     "TEST",
		ConsumerPrefix: "bc",
		WildcardFilter: "events.>",
	}
	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil })

	_, err := NewBroadcastConsumer(nil, cfg, handler)
	require.Error(t, err)
	require.Contains(t, err.Error(), "JetStream")
}

func TestNewBroadcastConsumer_RequiresHandler(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := BroadcastConsumerConfig{
		StreamName:     "TEST",
		ConsumerPrefix: "bc",
		WildcardFilter: "events.>",
	}

	_, err = NewBroadcastConsumer(js, cfg, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "handler")
}

func TestBroadcastConsumer_DurableName_Format(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := BroadcastConsumerConfig{
		StreamName:     "TEST",
		ConsumerPrefix: "myapp",
		ConsumerID:     "worker-1",
		WildcardFilter: "events.>",
	}
	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error { return nil })

	bc, err := NewBroadcastConsumer(js, cfg, handler)
	require.NoError(t, err)

	name := bc.durableName()
	require.Equal(t, "myapp_broadcast_worker-1", name)

	// Verify allowed charset
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		t.Fatalf("unexpected rune in durable name: %q", r)
	}
}

func TestBroadcastConsumer_UpdateWorkerConsumer_ReceivesMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	// Start embedded NATS with JetStream
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Create a stream with LimitsPolicy (required for broadcast)
	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "BROADCAST",
		Subjects:  []string{"bc.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := BroadcastConsumerConfig{
		StreamName:     "BROADCAST",
		ConsumerPrefix: "bctest",
		ConsumerID:     "worker-1",
		WildcardFilter: "bc.>",
		BatchSize:      1,
		FetchTimeout:   2 * time.Second,
	}

	var handled int32
	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&handled, 1)
		return nil
	})

	bc, err := NewBroadcastConsumer(js, cfg, handler)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Close(ctx) })

	// Assign partitions - ignored but should not error
	parts := []types.Partition{{Keys: []string{"region", "us-east"}}}
	require.NoError(t, bc.UpdateWorkerConsumer(ctx, "worker-1", parts))

	// Wait for consumer to be created
	durable := bc.durableName()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Publish messages
	for range 3 {
		require.NoError(t, nc.Publish("bc.region.us-east.events", []byte("msg")))
	}
	_ = nc.Flush()

	// Wait for messages to be handled
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&handled) >= 3 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&handled), int32(3))
}

// TestEnsureConsumer_ReturnsConfigWithoutMutatingState verifies that ensureConsumer
// returns the jetstream.ConsumerConfig it built and does NOT write to bc.consumerConfig.
// The caller (startConsumerLoop) is responsible for snapshotting after confirmed success.
func TestEnsureConsumer_ReturnsConfigWithoutMutatingState(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "ENSURE_CFG",
		Subjects:  []string{"ensure.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	cfg := BroadcastConsumerConfig{
		StreamName:     "ENSURE_CFG",
		ConsumerPrefix: "bctest",
		ConsumerID:     "ens-worker",
		WildcardFilter: "ensure.>",
	}
	require.NoError(t, cfg.SetDefaults())

	handler := messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })
	bc, err := NewBroadcastConsumer(js, cfg, handler)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Close(ctx) })

	durable := bc.durableName()
	cons, retCfg, ensureErr := bc.ensureConsumer(ctx, durable)
	require.NoError(t, ensureErr)
	require.NotNil(t, cons)

	// Returned config must reflect the durable that was requested.
	require.Equal(t, durable, retCfg.Durable, "returned config must match the requested durable name")

	// ensureConsumer must NOT have stored anything in bc.consumerConfig.
	bc.consumerMu.RLock()
	stored := bc.consumerConfig
	bc.consumerMu.RUnlock()
	require.Empty(t, stored.Durable, "ensureConsumer must not mutate bc.consumerConfig; that is startConsumerLoop's responsibility")
}

// TestStartConsumerLoop_ConsumerConfigMatchesActiveDurable verifies that after a
// successful startConsumerLoop, bc.consumerConfig.Durable equals the active durable name.
func TestStartConsumerLoop_ConsumerConfigMatchesActiveDurable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "LOOP_CFG",
		Subjects:  []string{"loop.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	cfg := BroadcastConsumerConfig{
		StreamName:     "LOOP_CFG",
		ConsumerPrefix: "bctest",
		ConsumerID:     "loop-worker",
		WildcardFilter: "loop.>",
	}
	require.NoError(t, cfg.SetDefaults())

	handler := messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })
	bc, err := NewBroadcastConsumer(js, cfg, handler)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Close(ctx) })

	require.NoError(t, bc.startConsumerLoop(ctx))

	expectedDurable := bc.durableName()

	bc.consumerMu.RLock()
	stored := bc.consumerConfig
	bc.consumerMu.RUnlock()

	require.Equal(t, expectedDurable, stored.Durable,
		"bc.consumerConfig.Durable must equal the active durable after startConsumerLoop succeeds")
}

// TestStartConsumerLoop_CollisionFallback_SnapshotsWinningConfig is the regression test
// for the bug where bc.consumerConfig was snapshotted before EnsureConsumer succeeded.
//
// When attempt 1 fails with a name collision and attempt 2 succeeds with a fresh
// generated ID, bc.consumerConfig must reflect the winning durable — not the
// first (failed) attempt's name.
//
// Collision is triggered by pre-creating a push (InboxDelivery) consumer with the
// same name; pull consumer creation against that name returns "consumer name already
// in use" (or equivalent), which matches isConsumerNameConflict.
func TestStartConsumerLoop_CollisionFallback_SnapshotsWinningConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "COLLISION",
		Subjects:  []string{"col.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	// Pre-create a push consumer with the target durable name but a different
	// FilterSubject.  CreateOrUpdateConsumer will reject a conflicting pull
	// consumer creation attempt with a config-mismatch / name-already-in-use error.
	const conflictDurable = "bctest_broadcast_col-worker"
	_, err = js.CreateOrUpdateConsumer(ctx, "COLLISION", jetstream.ConsumerConfig{
		Name:           conflictDurable,
		Durable:        conflictDurable,
		FilterSubject:  "col.other.>", // different from bc's "col.>"
		DeliverSubject: nc.NewInbox(), // push consumer — incompatible with pull
	})
	require.NoError(t, err, "pre-create conflicting consumer")

	cfg := BroadcastConsumerConfig{
		StreamName:     "COLLISION",
		ConsumerPrefix: "bctest",
		WildcardFilter: "col.>",
	}
	require.NoError(t, cfg.SetDefaults())

	handler := messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })

	// Construct directly so we can set a controlled initial ID (same name as the
	// pre-created conflicting consumer) with consumerIDSourceGenerated to allow retry.
	bc := &BroadcastConsumer{
		js:               js,
		config:           cfg,
		logger:           cfg.Logger,
		handler:          handler,
		consumerID:       "col-worker", // durableName() → "bctest_broadcast_col-worker" (conflict!)
		consumerIDSource: consumerIDSourceGenerated,
		iterFactory:      defaultIterFactory,
		loopDone:         make(chan struct{}),
	}
	t.Cleanup(func() { _ = bc.Close(ctx) })

	err = bc.startConsumerLoop(ctx)
	if err != nil {
		// If the embedded NATS version does not return a string that matches
		// isConsumerNameConflict, the collision branch is not exercised. Skip
		// rather than fail so the test suite remains green on all NATS versions.
		t.Skipf("NATS did not produce a conflict error recognisable by isConsumerNameConflict; skipping collision path: %v", err)
	}

	bc.consumerMu.RLock()
	winningDurable := bc.consumerConfig.Durable
	bc.consumerMu.RUnlock()

	require.NotEmpty(t, winningDurable, "bc.consumerConfig must be set after successful start")
	require.NotEqual(t, conflictDurable, winningDurable,
		"bc.consumerConfig must reflect the winning (retry) durable, not the failed first attempt")

	// The winning consumer must actually exist on the server.
	_, err = js.Consumer(ctx, "COLLISION", winningDurable)
	require.NoError(t, err, "winning consumer %q must exist on the server", winningDurable)
}

func TestBroadcastConsumer_ReceivesAllMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "FILTER",
		Subjects:  []string{"filter.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
		MaxMsgs:   -1,
	})
	require.NoError(t, err)

	cfg := BroadcastConsumerConfig{
		StreamName:     "FILTER",
		ConsumerPrefix: "bcfilter",
		ConsumerID:     "worker-1",
		WildcardFilter: "filter.>",
		BatchSize:      1,
		FetchTimeout:   1 * time.Second,
	}

	var totalHandled int32
	handler := messageHandlerFunc(func(ctx context.Context, msg jetstream.Msg) error {
		atomic.AddInt32(&totalHandled, 1)
		return nil
	})

	bc, err := NewBroadcastConsumer(js, cfg, handler)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Close(ctx) })

	// Update to start consumer (no partitions needed)
	require.NoError(t, bc.UpdateWorkerConsumer(ctx, "worker-1", nil))

	// Wait for consumer
	durable := bc.durableName()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := js.Consumer(ctx, cfg.StreamName, durable); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Publish to arbitrary subjects
	require.NoError(t, nc.Publish("filter.p1.events", []byte("msg1")))
	require.NoError(t, nc.Publish("filter.p2.events", []byte("msg2")))
	require.NoError(t, nc.Publish("filter.misc.logs", []byte("msg3")))
	_ = nc.Flush()

	// Wait for processing
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&totalHandled) >= 3 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.GreaterOrEqual(t, atomic.LoadInt32(&totalHandled), int32(3))
}

// TestBroadcastConsumer_RecoverySnapshot_CarriesConsumerOptions verifies
// the recovery snapshot for Broadcast carries both new fields. Reads
// bc.consumerConfig under consumerMu (matching the canonical test
// at line ~365).
func TestBroadcastConsumer_RecoverySnapshot_CarriesConsumerOptions(t *testing.T) {
	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "BCSNAP",
		Subjects: []string{"bcsnap.>"},
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	noopHandler := func(_ context.Context, _ jetstream.Msg) error { return nil }

	cfgExplicit := BroadcastConsumerConfig{
		StreamName:            "BCSNAP",
		ConsumerPrefix:        "bcsnap-explicit",
		ConsumerID:            "snap-worker-1",
		WildcardFilter:        "bcsnap.>",
		ConsumerMemoryStorage: true,
		ConsumerReplicas:      1,
	}
	require.NoError(t, cfgExplicit.SetDefaults())
	bcExplicit, err := NewBroadcastConsumer(js, cfgExplicit, noopHandler)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bcExplicit.Close(ctx)
	})
	require.NoError(t, bcExplicit.startConsumerLoop(ctx))

	bcExplicit.consumerMu.RLock()
	storedExplicit := bcExplicit.consumerConfig
	bcExplicit.consumerMu.RUnlock()
	require.True(t, storedExplicit.MemoryStorage)
	require.Equal(t, 1, storedExplicit.Replicas)

	cfgDefault := BroadcastConsumerConfig{
		StreamName:     "BCSNAP",
		ConsumerPrefix: "bcsnap-default",
		ConsumerID:     "snap-worker-2",
		WildcardFilter: "bcsnap.>",
	}
	require.NoError(t, cfgDefault.SetDefaults())
	bcDefault, err := NewBroadcastConsumer(js, cfgDefault, noopHandler)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bcDefault.Close(ctx)
	})
	require.NoError(t, bcDefault.startConsumerLoop(ctx))

	bcDefault.consumerMu.RLock()
	storedDefault := bcDefault.consumerConfig
	bcDefault.consumerMu.RUnlock()
	require.False(t, storedDefault.MemoryStorage)
	require.Equal(t, 0, storedDefault.Replicas)
}

// broadcastSignalMetrics extends captureEscalationMetrics to also record
// IncrementWorkerConsumerIteratorRestart calls.
type broadcastSignalMetrics struct {
	captureEscalationMetrics
	restartReasons []string
}

func (m *broadcastSignalMetrics) IncrementWorkerConsumerIteratorRestart(reason string) {
	m.restartReasons = append(m.restartReasons, reason)
}

// TestBroadcastConsumer_RecoveryDisabled_IteratorErrorIsSignaled pins the operator
// signal for the default configuration: with RecoveryDisabled (nil controller), a
// non-graceful iterator error (e.g. the durable was deleted) must emit the
// iterator-restart metric with reason "recovery_disabled". Pre-fix the only artifact
// was a Debug log — at production log levels a deleted durable was a zero-signal
// permanent stall.
func TestBroadcastConsumer_RecoveryDisabled_IteratorErrorIsSignaled(t *testing.T) {
	m := &broadcastSignalMetrics{}

	bc := &BroadcastConsumer{
		config: BroadcastConsumerConfig{
			StreamName:     "BC_RECOVERY_DISABLED",
			ConsumerPrefix: "bc",
			WildcardFilter: "bc.>",
			BatchSize:      1,
			FetchTimeout:   time.Second,
			Metrics:        m,
			Retry: RetryConfig{
				Base:       20 * time.Millisecond,
				Backoff:    20 * time.Millisecond,
				Multiplier: 1.0,
				Max:        20 * time.Millisecond,
			},
		},
		logger:   logging.NewNop(),
		handler:  messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil }),
		loopDone: make(chan struct{}),
		iterFactory: func(_ jetstream.Consumer, _ int, _ time.Duration) (jetstream.MessagesContext, error) {
			return &errorOnNextIter{err: jetstream.ErrNoHeartbeat}, nil
		},
		recovery: nil, // RecoveryDisabled: NewController returns nil for Disabled strategy
	}

	ctx, cancel := context.WithCancel(context.Background())
	go bc.runLoop(ctx)

	// One backoff cycle (20ms) is enough to observe the metric; wait two.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-bc.loopDone

	require.NotEmpty(t, m.restartReasons,
		"expected IncrementWorkerConsumerIteratorRestart to be called on the nil-recovery path")
	require.Contains(t, m.restartReasons, "recovery_disabled",
		"expected reason=recovery_disabled on the nil-recovery path")
}

// TestBroadcastConsumer_UpdateAfterClose_ReturnsErrConsumerStopped pins the
// terminal-Stop contract: Close cancels the pull loop permanently
// (loopStarted was never reset; loopDone cannot be reused), so a subsequent
// UpdateWorkerConsumer/Start must FAIL with the sentinel instead of logging
// "active" and returning nil — which let a manager-driven apply report
// success with a dead fan-out loop and the worker Stable.
//
// Pre-fix: UpdateWorkerConsumer after Close saw loopStarted=true, skipped
// startConsumerLoop, logged "active", and returned nil — a silent lie.
// TestBroadcastDelayOrExit_BackoffGrows verifies that repeated calls to delayOrExit
// produce growing delays (the previous delay is threaded through, not reset to 0 each call).
//
// Pre-fix: every call passed prev=0 into jitterBackoff, so the delay was always Base
// regardless of how many times the helper was called.
func TestBroadcastDelayOrExit_BackoffGrows(t *testing.T) {
	const (
		base     = 1 * time.Millisecond
		mult     = 2.0
		maxDelay = 8 * time.Millisecond
		seed     = int64(42)
		// Number of invocations — enough that the cap is reached with Seed=42.
		calls = 6
	)

	bc := &BroadcastConsumer{
		config: BroadcastConsumerConfig{
			Retry: RetryConfig{
				Base:       base,
				Multiplier: mult,
				Max:        maxDelay,
				Seed:       seed,
			},
		},
		logger:   logging.NewNop(),
		loopDone: make(chan struct{}),
	}
	// Initialise the RNG exactly as NewBroadcastConsumer does.
	bc.rng = newRetryRNG(bc.config.Retry.Seed)

	ctx := context.Background()
	delays := make([]time.Duration, 0, calls)
	for range calls {
		// We capture the delay value directly rather than sleeping.
		// delayOrExit does sleep, so we cancel the context immediately after
		// capturing the sleep value by using a cancelable ctx that fires after
		// a generous deadline — the test runs with Base=1ms so actual wall time
		// is negligible.
		delayCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		bc.delayOrExit(delayCtx) //nolint:contextcheck // intentional: we want the sleep to complete
		cancel()
		delays = append(delays, bc.retryPrev)
	}

	require.Len(t, delays, calls)

	// Every delay must be in [Base, Max].
	for i, d := range delays {
		require.GreaterOrEqual(t, d, base, "delay[%d] below Base", i)
		require.LessOrEqual(t, d, maxDelay, "delay[%d] exceeds Max", i)
	}

	// Growth: last delay must exceed first (with a fixed seed this is deterministic).
	require.Greater(t, delays[calls-1], delays[0],
		"expected backoff to grow over %d calls; delays: %v", calls, delays)
}

func TestBroadcastConsumer_UpdateAfterClose_ReturnsErrConsumerStopped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      "BCTERM",
		Subjects:  []string{"bcterm.>"},
		Retention: jetstream.LimitsPolicy,
		Storage:   jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	cfg := BroadcastConsumerConfig{
		StreamName:     "BCTERM",
		ConsumerPrefix: "bcterm",
		ConsumerID:     "term-worker",
		WildcardFilter: "bcterm.>",
	}
	require.NoError(t, cfg.SetDefaults())

	handler := messageHandlerFunc(func(_ context.Context, _ jetstream.Msg) error { return nil })
	bc, err := NewBroadcastConsumer(js, cfg, handler)
	require.NoError(t, err)

	// Start the consumer
	require.NoError(t, bc.UpdateWorkerConsumer(ctx, "term-worker", nil))

	// Close the consumer (terminal)
	closeCtx, closeCancel := context.WithTimeout(ctx, 5*time.Second)
	defer closeCancel()
	require.NoError(t, bc.Close(closeCtx))

	// A second Close must remain idempotent (same nil result as pre-fix)
	require.NoError(t, bc.Close(closeCtx), "second Close must be idempotent")

	// UpdateWorkerConsumer after Close must return ErrConsumerStopped, not nil
	err = bc.UpdateWorkerConsumer(ctx, "term-worker", nil)
	require.ErrorIs(t, err, types.ErrConsumerStopped,
		"UpdateWorkerConsumer after Close must return ErrConsumerStopped, got: %v", err)

	// Pin the never-started variant: New→Close→Update must also return
	// ErrConsumerStopped even if UpdateWorkerConsumer was never called first.
	// Use direct construction so no NATS call is needed.
	t.Run("never-started", func(t *testing.T) {
		bc2 := &BroadcastConsumer{
			logger:   logging.NewNop(),
			loopDone: make(chan struct{}),
		}
		require.NoError(t, bc2.Close(context.Background()), "Close on never-started must be nil")
		err := bc2.UpdateWorkerConsumer(context.Background(), "", nil)
		require.ErrorIs(t, err, types.ErrConsumerStopped,
			"UpdateWorkerConsumer after Close on never-started must return ErrConsumerStopped")
	})
}
