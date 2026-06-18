package parti

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/natsutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// replicaBucketSeq mints fresh bucket names per sub-test so the parallel
// runs do not collide on a shared embedded NATS server.
var replicaBucketSeq atomic.Uint64

// TestEnsureKVBucket_ReplicasZero_DefaultsToServer verifies the legacy
// behavior: Replicas == 0 leaves the field unset, nats.go normalizes
// to 1 server-side. This is the path every pre-existing deployment is
// on; the new field must not regress it.
func TestEnsureKVBucket_ReplicasZero_DefaultsToServer(t *testing.T) {
	t.Parallel()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := mintReplicaBucket("zero")
	m := &Manager{
		cfg: Config{
			KVBuckets: KVBucketConfig{Replicas: 0},
		},
		logger: nopLogger{},
	}
	kv, err := m.ensureKVBucket(t.Context(), js, bucket, 0, jetstream.FileStorage)
	require.NoError(t, err)
	require.NotNil(t, kv)

	cfg := streamConfigForBucket(t, kv)
	require.Equal(t, 1, cfg.Replicas,
		"server default normalizes an unset (0) replicas to 1 — legacy behavior")
}

// TestEnsureKVBucket_ReplicasOne_ExplicitSingleNode verifies that an
// explicit Replicas=1 (typical for single-node test deployments) is
// honored without ambiguity. This is the value the partitest helper
// would set if a test wanted to mirror production posture without HA.
func TestEnsureKVBucket_ReplicasOne_ExplicitSingleNode(t *testing.T) {
	t.Parallel()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := mintReplicaBucket("one")
	m := &Manager{
		cfg: Config{
			KVBuckets: KVBucketConfig{Replicas: 1},
		},
		logger: nopLogger{},
	}
	kv, err := m.ensureKVBucket(t.Context(), js, bucket, 0, jetstream.FileStorage)
	require.NoError(t, err)

	cfg := streamConfigForBucket(t, kv)
	require.Equal(t, 1, cfg.Replicas,
		"Replicas=1 must be honored explicitly on single-node clusters")
}

// TestEnsureKVBucket_ReplicasThree_FailsLoudlyOnSingleNode verifies the
// documented loud-failure path: setting Replicas=3 against a single-
// node NATS cluster must return a clear error at Manager.Start, not
// silently fall back. This is the documented "cluster topology must
// support the value" contract.
func TestEnsureKVBucket_ReplicasThree_FailsLoudlyOnSingleNode(t *testing.T) {
	t.Parallel()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := mintReplicaBucket("three")
	m := &Manager{
		cfg: Config{
			KVBuckets: KVBucketConfig{Replicas: 3},
		},
		logger: nopLogger{},
	}
	_, err = m.ensureKVBucket(t.Context(), js, bucket, 0, jetstream.FileStorage)
	require.Error(t, err,
		"Replicas=3 against a single-node cluster MUST fail loudly so operators see the topology mismatch")
	// The exact error string varies by nats.go version; the important
	// guarantee is that the call returns an error rather than silently
	// creating a single-replica bucket.
	require.Contains(t, strings.ToLower(err.Error()), "kv bucket",
		"error must reference KV bucket creation; got %v", err)
}

// TestEnsureKVBucket_PreCreatedBucket_KeepsExistingReplicas verifies
// the get-first contract called out in the Godoc: a pre-created bucket
// with explicit replicas is opened as-is even when the runtime
// Config.KVBuckets.Replicas would have created it differently.
func TestEnsureKVBucket_PreCreatedBucket_KeepsExistingReplicas(t *testing.T) {
	t.Parallel()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := mintReplicaBucket("pre")
	// Operator pre-creates the bucket with Replicas=1 explicitly. On a
	// single-node embedded NATS this is the only viable value, but the
	// point of the test is that the pre-created value is kept regardless
	// of what the runtime Config asks for.
	_, err = js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket:   bucket,
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	// Runtime config wants Replicas=3, but get-first opens the existing
	// bucket and leaves its config alone.
	m := &Manager{
		cfg: Config{
			KVBuckets: KVBucketConfig{Replicas: 3},
		},
		logger: nopLogger{},
	}
	kv, err := m.ensureKVBucket(t.Context(), js, bucket, 0, jetstream.FileStorage)
	require.NoError(t, err,
		"get-first must succeed even when Config.Replicas would otherwise fail to create")

	cfg := streamConfigForBucket(t, kv)
	require.Equal(t, 1, cfg.Replicas,
		"pre-created bucket's Replicas (1) wins over Config.Replicas (3); get-first does not upgrade")
}

// streamConfigForBucket reads the backing JetStream stream's Config so
// the test can assert on the replica count actually applied. Lives in
// the test file to avoid bloating the public API.
func streamConfigForBucket(t *testing.T, kv jetstream.KeyValue) jetstream.StreamConfig {
	t.Helper()
	status, err := kv.Status(t.Context())
	require.NoError(t, err)
	bs, ok := status.(*jetstream.KeyValueBucketStatus)
	require.True(t, ok, "bucket status type %T is not introspectable", status)
	si := bs.StreamInfo()
	require.NotNil(t, si)

	return si.Config
}

// mintReplicaBucket returns a bucket name unique to this test process.
// Combines the per-test label with an atomic counter so parallel
// sub-tests cannot collide on the shared embedded NATS instance.
func mintReplicaBucket(label string) string {
	const prefix = "parti-replicas-test-"
	n := replicaBucketSeq.Add(1)
	return prefix + label + "-" + timeSuffix(n)
}

// timeSuffix is a small base-10 stringifier so the bucket name stays
// human-readable in test failure logs.
func timeSuffix(n uint64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}

	return string(buf[i:])
}

// nopLogger satisfies the types.Logger interface for the replicas
// tests without taking on the integration-shaped spy from other test
// files. Silent by design — these tests assert on behavior, not logs.
type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}
func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}
func (nopLogger) Fatal(string, ...any) {}

// TestEnsureKVBucket_PreCreated_ReplicasMismatch_WARNs verifies the
// warnOnReplicasMismatch helper fires when an existing bucket's
// replica count differs from what Config.KVBuckets.Replicas requests.
// The library deliberately does NOT auto-reconcile (Replicas is HA
// quality, not correctness); the warn is the only operator signal that
// the requested value is not in effect.
func TestEnsureKVBucket_PreCreated_ReplicasMismatch_WARNs(t *testing.T) {
	t.Parallel()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := mintReplicaBucket("mismatch-warn")
	// Operator pre-creates with Replicas=1.
	_, err = js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket:   bucket,
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	spy := &warnSpy{}
	m := &Manager{
		cfg: Config{
			KVBuckets: KVBucketConfig{Replicas: 3}, // mismatched with pre-created
		},
		logger: spy,
	}
	_, err = m.ensureKVBucket(t.Context(), js, bucket, 0, jetstream.FileStorage)
	require.NoError(t, err)

	var saw bool
	for _, w := range spy.snapshot() {
		if strings.Contains(w, "replica count differs from Config.KVBuckets.Replicas") {
			saw = true
			break
		}
	}
	require.True(t, saw,
		"warnOnReplicasMismatch must emit the named WARN when existing Replicas differs from requested; got %v",
		spy.snapshot())
}

// TestEnsureKVBucket_ReplicasZero_NoMismatchWarning verifies the
// warning is silent when Config.KVBuckets.Replicas is 0 (no expectation
// expressed). Otherwise every existing deployment would log spurious
// "1 != 0" warnings.
func TestEnsureKVBucket_ReplicasZero_NoMismatchWarning(t *testing.T) {
	t.Parallel()

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	bucket := mintReplicaBucket("zero-no-warn")
	_, err = js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket:   bucket,
		Storage:  jetstream.FileStorage,
		Replicas: 1,
	})
	require.NoError(t, err)

	spy := &warnSpy{}
	m := &Manager{
		cfg:    Config{KVBuckets: KVBucketConfig{Replicas: 0}}, // legacy default
		logger: spy,
	}
	_, err = m.ensureKVBucket(t.Context(), js, bucket, 0, jetstream.FileStorage)
	require.NoError(t, err)

	for _, w := range spy.snapshot() {
		require.NotContains(t, w, "replica count differs",
			"Config.KVBuckets.Replicas=0 must NOT trigger the mismatch warning")
	}
}

// stableIDBucketMaxAge reads the MaxAge of the stream backing a KV bucket.
func stableIDBucketMaxAge(t *testing.T, ctx context.Context, js jetstream.JetStream, bucket string) time.Duration {
	t.Helper()
	kv, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	status, err := kv.Status(ctx)
	require.NoError(t, err)
	bs, ok := status.(*jetstream.KeyValueBucketStatus)
	require.True(t, ok)
	require.NotNil(t, bs.StreamInfo())

	return bs.StreamInfo().Config.MaxAge
}

// TestReconcileStableIDBucketMaxAge_FixesZeroMaxAge verifies that a stableID
// bucket pre-created with MaxAge=0 (unlimited — an operator misconfiguration
// that would leak worker IDs on every ungraceful restart) is reconciled to
// WorkerIDTTL when the Manager starts.
func TestReconcileStableIDBucketMaxAge_FixesZeroMaxAge(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "reconcile-stableid-zero"

	// Pre-create the stableID bucket with MaxAge=0 (TTL omitted => unlimited).
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)
	require.Equal(t, time.Duration(0), stableIDBucketMaxAge(t, ctx, js, bucket))

	cfg := TestConfig()
	cfg.KVBuckets.StableIDBucket = bucket
	mgr, err := NewManager(&cfg, js, source.NewStatic([]Partition{{Keys: []string{"p1"}}}), strategy.NewConsistentHash())
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	require.Equal(t, cfg.WorkerIDTTL, stableIDBucketMaxAge(t, ctx, js, bucket),
		"Manager startup must reconcile the stableID bucket MaxAge to WorkerIDTTL")
}

// TestReconcileStableIDBucketMaxAge_HappyPathNoUpdate verifies that a bucket
// already created with the correct MaxAge needs no stream update, so a
// least-privilege NATS user without stream-update permission starts cleanly.
func TestReconcileStableIDBucketMaxAge_HappyPathNoUpdate(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "reconcile-stableid-happy"

	cfg := TestConfig()
	cfg.KVBuckets.StableIDBucket = bucket

	// Pre-create the bucket correctly: MaxAge == WorkerIDTTL.
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket, TTL: cfg.WorkerIDTTL})
	require.NoError(t, err)

	// Any stream update would be a bug — fail the test if reconcile calls it.
	orig := kvStreamUpdate
	kvStreamUpdate = func(_ context.Context, _ jetstream.JetStream, _ jetstream.StreamConfig) error {
		t.Error("reconcile must not attempt a stream update when MaxAge already matches")
		return errors.New("unexpected stream update")
	}
	defer func() { kvStreamUpdate = orig }()

	mgr, err := NewManager(&cfg, js, source.NewStatic([]Partition{{Keys: []string{"p1"}}}), strategy.NewConsistentHash())
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx),
		"Manager.Start must succeed without stream-update permission when the bucket is already correct")
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })
}

// TestReconcileStableIDBucketMaxAge_FailLoudWhenUpdateDenied verifies that when
// the stableID bucket has a divergent MaxAge and the stream update is rejected
// (e.g. a least-privilege NATS user), Manager.Start fails loudly with an
// actionable error rather than continuing into a delayed worker-ID leak.
func TestReconcileStableIDBucketMaxAge_FailLoudWhenUpdateDenied(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "reconcile-stableid-faillloud"

	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	orig := kvStreamUpdate
	kvStreamUpdate = func(_ context.Context, _ jetstream.JetStream, _ jetstream.StreamConfig) error {
		return errors.New("nats: permissions violation for stream update")
	}
	defer func() { kvStreamUpdate = orig }()

	cfg := TestConfig()
	cfg.KVBuckets.StableIDBucket = bucket
	mgr, err := NewManager(&cfg, js, source.NewStatic([]Partition{{Keys: []string{"p1"}}}), strategy.NewConsistentHash())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	startErr := mgr.Start(ctx)
	require.Error(t, startErr, "Manager.Start must fail loudly when the stableID MaxAge cannot be corrected")
	require.Contains(t, startErr.Error(), "MaxAge")
	require.Contains(t, startErr.Error(), bucket)
}

// TestReconcileStableIDBucketMaxAge_FailsWhenStatusUnreadable verifies the
// fail-loud contract when the bucket's MaxAge cannot be verified at all: the
// reconciler must return an error rather than silently succeed.
func TestReconcileStableIDBucketMaxAge_FailsWhenStatusUnreadable(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "reconcile-stableid-unreadable"

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()

	err = reconcileStableIDBucketMaxAge(cancelled, js, kv, bucket, 5*time.Second, nil)
	require.Error(t, err, "reconcile must fail loud when it cannot read the bucket's MaxAge")
	require.Contains(t, err.Error(), bucket)
}

// TestReconcileStableIDBucketMaxAge_Concurrent verifies that concurrent
// reconcile attempts against the same bucket all succeed and converge.
func TestReconcileStableIDBucketMaxAge_Concurrent(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("embedded nats-server reports an internal race on concurrent stream updates")
	}

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "reconcile-stableid-concurrent"
	const want = 5 * time.Second

	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	const workers = 6
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Go(func() {
			kv, err := js.KeyValue(ctx, bucket)
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = reconcileStableIDBucketMaxAge(ctx, js, kv, bucket, want, nil)
		})
	}
	wg.Wait()

	for i, e := range errs {
		require.NoError(t, e, "concurrent reconcile worker %d", i)
	}
	require.Equal(t, want, stableIDBucketMaxAge(t, ctx, js, bucket))
}

// TestReconcileStableIDBucketMaxAge_ShrinksLongerMaxAge documents the operator
// policy that the stableID bucket MaxAge is reconciled to *exactly*
// WorkerIDTTL: a deliberately-longer operator-provisioned MaxAge is shortened
// on startup. WorkerIDTTL is the authoritative worker-ID lease window, so the
// bucket MaxAge must equal it — a longer MaxAge lets abandoned IDs linger past
// the lease.
func TestReconcileStableIDBucketMaxAge_ShrinksLongerMaxAge(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "reconcile-stableid-shrink"

	cfg := TestConfig()
	cfg.KVBuckets.StableIDBucket = bucket

	// Operator pre-created the bucket with a MaxAge far longer than WorkerIDTTL.
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket, TTL: 10 * time.Minute})
	require.NoError(t, err)

	mgr, err := NewManager(&cfg, js, source.NewStatic([]Partition{{Keys: []string{"p1"}}}), strategy.NewConsistentHash())
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	require.Equal(t, cfg.WorkerIDTTL, stableIDBucketMaxAge(t, ctx, js, bucket),
		"a longer operator MaxAge must be reconciled down to exactly WorkerIDTTL")
}

// TestReconcileStableIDBucketMaxAge_PreservesUnrelatedStreamConfig verifies the
// reconcile path changes only MaxAge (and the Duplicates window it must clamp
// alongside it) and does not drift other stream fields on an operator-created
// bucket.
func TestReconcileStableIDBucketMaxAge_PreservesUnrelatedStreamConfig(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "reconcile-stableid-preserve"

	cfg := TestConfig()
	cfg.KVBuckets.StableIDBucket = bucket

	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      bucket,
		Description: "operator-created stableID bucket",
		History:     4,
		MaxBytes:    8192,
	})
	require.NoError(t, err)

	kv, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	beforeStatus, err := kv.Status(ctx)
	require.NoError(t, err)
	beforeBS, ok := beforeStatus.(*jetstream.KeyValueBucketStatus)
	require.True(t, ok)
	before := beforeBS.StreamInfo().Config

	mgr, err := NewManager(&cfg, js, source.NewStatic([]Partition{{Keys: []string{"p1"}}}), strategy.NewConsistentHash())
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	afterStatus, err := kv.Status(ctx)
	require.NoError(t, err)
	afterBS, ok := afterStatus.(*jetstream.KeyValueBucketStatus)
	require.True(t, ok)
	after := afterBS.StreamInfo().Config

	require.Equal(t, cfg.WorkerIDTTL, after.MaxAge, "MaxAge must be reconciled to WorkerIDTTL")
	require.LessOrEqual(t, after.Duplicates, after.MaxAge,
		"the Duplicates window must be clamped to <= MaxAge (JetStream rejects otherwise)")
	require.Equal(t, before.Name, after.Name)
	require.Equal(t, before.Description, after.Description)
	require.Equal(t, before.MaxMsgsPerSubject, after.MaxMsgsPerSubject)
	require.Equal(t, before.MaxBytes, after.MaxBytes)
	require.Equal(t, before.Storage, after.Storage)
}

// kvUnavailManager builds a minimal Manager parked in StateStable with a
// degrade-reason capture hook. The returned pointer receives the reason string
// passed to enterDegraded (via the OnDegraded hook, the only surface that
// observes it).
//
// The threshold is fixed at 3: low enough that the degrade tests trip it with a
// short loop, high enough that the isolated-below-threshold negative-space test
// (which clears the window after each error) never reaches it.
func kvUnavailManager(t *testing.T) (*Manager, *string, context.CancelFunc) {
	t.Helper()
	const threshold = 3
	cfg := Config{
		DegradedBehavior: DegradedBehaviorConfig{
			KVErrorThreshold: threshold,
			KVErrorWindow:    10 * time.Second,
		},
		DegradedAlert: DegradedAlertConfig{
			AlertInterval: 1 * time.Minute,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	reason := new(string)
	m := &Manager{
		logger:  logging.NewNop(),
		cfg:     cfg,
		hooks:   &Hooks{},
		metrics: metrics.NewNop(),
		ctx:     ctx,
		cancel:  cancel,
	}
	m.hooks.OnDegraded = func(_ context.Context, r string) error {
		*reason = r
		return nil
	}
	m.state.Store(int32(StateStable))

	return m, reason, cancel
}

// TestMarkKVUnavailable is the F-D1 classifier/routing table. It pins the
// "existing classifiers win first" construction that makes it impossible for
// the new path to steal the whole-bucket-loss route (ErrNoStreamResponse stays
// connectivity) or the bucket-missing route (ErrBucketNotFound stays degrading
// JetStream). Only an otherwise-unclassified deadline / no-responders timeout
// is marked.
func TestMarkKVUnavailable(t *testing.T) {
	t.Run("nil passes through", func(t *testing.T) {
		require.NoError(t, markKVUnavailable(nil))
	})

	t.Run("bare deadline is marked", func(t *testing.T) {
		got := markKVUnavailable(context.DeadlineExceeded)
		require.ErrorIs(t, got, ErrKVUnavailable)
		require.ErrorIs(t, got, context.DeadlineExceeded,
			"the original cause must remain inspectable")
	})

	t.Run("no-responders is marked", func(t *testing.T) {
		got := markKVUnavailable(nats.ErrNoResponders)
		require.ErrorIs(t, got, ErrKVUnavailable)
		require.ErrorIs(t, got, nats.ErrNoResponders)
	})

	t.Run("wrapped deadline is marked", func(t *testing.T) {
		// The election renew path wraps the ctx deadline as
		// "leadership was lost: context deadline exceeded"; the wrapper must
		// still see the deadline through the wrap.
		got := markKVUnavailable(
			fmt.Errorf("leadership was lost: %w", context.DeadlineExceeded))
		require.ErrorIs(t, got, ErrKVUnavailable)
	})

	t.Run("ErrNoStreamResponse is NOT marked (stays whole-bucket route)", func(t *testing.T) {
		got := markKVUnavailable(jetstream.ErrNoStreamResponse)
		require.NotErrorIs(t, got, ErrKVUnavailable,
			"ErrNoStreamResponse must remain on the connectivity / whole-bucket route")
		require.True(t, natsutil.IsConnectivityError(got),
			"must still classify as connectivity after the wrapper")
	})

	t.Run("ErrBucketNotFound is NOT marked (stays degrading route)", func(t *testing.T) {
		got := markKVUnavailable(jetstream.ErrBucketNotFound)
		require.NotErrorIs(t, got, ErrKVUnavailable)
		require.True(t, natsutil.IsDegradingJetStreamError(got),
			"must still classify as degrading JetStream after the wrapper")
	})

	t.Run("unrelated error is NOT marked", func(t *testing.T) {
		got := markKVUnavailable(nats.ErrInvalidArg)
		require.NotErrorIs(t, got, ErrKVUnavailable)
		require.ErrorIs(t, got, nats.ErrInvalidArg, "passes through unchanged")
	})
}

// TestRecordKVError_ReadUnavailable_Degrades pins that a marked
// KV-unavailable error, sustained past the threshold, drives the manager into
// Degraded with the distinct reason kv-unavailable — the F-D1 observability
// value (the operator sees Degraded instead of a silent stall).
func TestRecordKVError_ReadUnavailable_Degrades(t *testing.T) {
	for _, tc := range []struct {
		name string
		base error
	}{
		{"deadline", context.DeadlineExceeded},
		{"no-responders", nats.ErrNoResponders},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, reason, cancel := kvUnavailManager(t)
			defer cancel()

			for range 3 {
				m.recordKVError(markKVUnavailable(tc.base))
			}
			cancel()
			m.wg.Wait()

			require.Equal(t, StateDegraded, m.State())
			require.Equal(t, DegradeReasonKVUnavailable, *reason,
				"a sustained KV-unavailable condition must use the distinct reason")
		})
	}
}

// TestRecordKVError_RawDeadline_StillDropped is the scoping guarantee: an
// UNWRAPPED deadline (one that did not pass through a manager KV-op call site)
// must still be dropped, exactly as before F-D1. Only call-site-marked errors
// enter the new path.
func TestRecordKVError_RawDeadline_StillDropped(t *testing.T) {
	m, _, cancel := kvUnavailManager(t)
	defer cancel()

	for range 5 {
		m.recordKVError(context.DeadlineExceeded) // raw, never marked
	}

	require.Equal(t, int32(0), m.kvErrorCount.Load(),
		"a raw deadline from outside the wrapped call sites must not count")
	require.NotEqual(t, StateDegraded, m.State())
}

// TestRecordKVError_WholeBucketLoss_KeepsThresholdReason pins cross-feature
// contract 1: whole-bucket loss must still reach
// enterDegraded("KV error threshold exceeded"), NOT the new reason — even when
// the bucket-missing error flows through a marked call site. The wrapper returns
// bucket-missing unchanged (classifiers win first), so the reason is unaffected.
func TestRecordKVError_WholeBucketLoss_KeepsThresholdReason(t *testing.T) {
	m, reason, cancel := kvUnavailManager(t)
	defer cancel()

	for range 3 {
		m.recordKVError(markKVUnavailable(jetstream.ErrBucketNotFound))
	}
	cancel()
	m.wg.Wait()

	require.Equal(t, StateDegraded, m.State())
	require.Equal(t, "KV error threshold exceeded", *reason,
		"whole-bucket loss must keep the threshold reason, not the KV-unavailable reason")
}

// TestRecordKVError_ReadUnavailable_IsolatedBelowThreshold_NoDegrade is the
// negative-space test for the threshold circuit (see the both-directions-of-a-
// boundary discipline): isolated marked failures, each fully recovered, must
// NEVER accumulate to a degrade. Positive-space "N consecutive failures degrade"
// is consistent with both correct and broken counter semantics; this proves the
// window resets on success.
func TestRecordKVError_ReadUnavailable_IsolatedBelowThreshold_NoDegrade(t *testing.T) {
	m, _, cancel := kvUnavailManager(t)
	defer cancel()

	// Ten isolated failures, each cleared by a success before the next.
	for range 10 {
		m.recordKVError(markKVUnavailable(context.DeadlineExceeded))
		m.recordKVSuccess()
	}

	require.NotEqual(t, StateDegraded, m.State(),
		"isolated KV-unavailable blips that each recover must not degrade")
	require.Zero(t, m.degradedSinceNano())
}

// TestOnClaimerError_ReadTimeout_Degrades pins the stableid-renew wrap site
// (onClaimerError's non-ErrClaimLost branch): a renewal read timeout — which
// arrives as a plain (non-ErrClaimLost) error — must be marked and drive the
// degraded circuit, NOT be silently dropped.
func TestOnClaimerError_ReadTimeout_Degrades(t *testing.T) {
	m, reason, cancel := kvUnavailManager(t)
	defer cancel()

	origShutdown := claimLostShutdown
	claimLostShutdown = func(*Manager) { t.Error("a read timeout must not stop the worker") }
	defer func() { claimLostShutdown = origShutdown }()

	for range 3 {
		m.onClaimerError(context.DeadlineExceeded) // renewal default branch: plain deadline
	}
	cancel()
	m.wg.Wait()

	require.Equal(t, StateDegraded, m.State())
	require.Equal(t, DegradeReasonKVUnavailable, *reason)
}
