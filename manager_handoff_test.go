package parti

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/kvbuckets"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type mockClaimStore struct {
	mock.Mock
}

func (m *mockClaimStore) ListKeys(ctx context.Context) ([]string, error) {
	args := m.Called(ctx)
	val, _ := args.Get(0).([]string)
	return val, args.Error(1)
}

func (m *mockClaimStore) Get(ctx context.Context, key string) (handoff.Claim, uint64, error) {
	args := m.Called(ctx, key)
	claim, _ := args.Get(0).(handoff.Claim)
	ver, _ := args.Get(1).(uint64)
	return claim, ver, args.Error(2)
}

func (m *mockClaimStore) PutIfEpoch(ctx context.Context, key string, epoch int64, claim handoff.Claim) (uint64, error) {
	args := m.Called(ctx, key, epoch, claim)
	ver, _ := args.Get(0).(uint64)
	return ver, args.Error(1)
}

func (m *mockClaimStore) Delete(ctx context.Context, key string, revision uint64) error {
	args := m.Called(ctx, key, revision)
	return args.Error(0)
}

func TestManager_handoffStartupHygiene(t *testing.T) {
	logger := logging.NewNop()

	t.Run("nil store", func(t *testing.T) {
		m := &Manager{logger: logger}
		resumable := m.handoffStartupHygiene(context.Background(), nil)
		require.False(t, resumable)
	})

	t.Run("empty store", func(t *testing.T) {
		m := &Manager{logger: logger}
		store := new(mockClaimStore)
		store.On("ListKeys", mock.Anything).Return([]string{}, nil)

		resumable := m.handoffStartupHygiene(context.Background(), store)
		require.False(t, resumable)
		store.AssertExpectations(t)
	})

	t.Run("resumable claim", func(t *testing.T) {
		m := &Manager{logger: logger}
		store := new(mockClaimStore)

		now := time.Now().UTC()
		claim := handoff.Claim{
			State:       handoff.ClaimStatePrepare,
			LastUpdated: now, // Not expired
			TTLSeconds:  10,
		}

		store.On("ListKeys", mock.Anything).Return([]string{"p1"}, nil)
		store.On("Get", mock.Anything, "p1").Return(claim, uint64(1), nil)

		resumable := m.handoffStartupHygiene(context.Background(), store)
		require.True(t, resumable)
		store.AssertExpectations(t)
	})

	t.Run("expired claim reset", func(t *testing.T) {
		m := &Manager{logger: logger}
		store := new(mockClaimStore)

		now := time.Now().UTC()
		claim := handoff.Claim{
			State:       handoff.ClaimStatePrepare,
			LastUpdated: now.Add(-20 * time.Second), // Expired (assuming TTL 10)
			TTLSeconds:  10,
			Epoch:       5,
		}

		store.On("ListKeys", mock.Anything).Return([]string{"p1"}, nil)
		store.On("Get", mock.Anything, "p1").Return(claim, uint64(1), nil)
		store.On("PutIfEpoch", mock.Anything, "p1", int64(5), mock.MatchedBy(func(c handoff.Claim) bool {
			return c.State == handoff.ClaimStateStable && c.PendingOwner == ""
		})).Return(uint64(2), nil)

		resumable := m.handoffStartupHygiene(context.Background(), store)
		require.False(t, resumable)
		store.AssertExpectations(t)
	})
}

// handoffBucketStreamConfig reads the stream config backing a KV bucket.
func handoffBucketStreamConfig(t *testing.T, ctx context.Context, js jetstream.JetStream, bucket string) jetstream.StreamConfig {
	t.Helper()
	kv, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	status, err := kv.Status(ctx)
	require.NoError(t, err)
	bs, ok := status.(*jetstream.KeyValueBucketStatus)
	require.True(t, ok)
	require.NotNil(t, bs.StreamInfo())

	return bs.StreamInfo().Config
}

// handoffBucketMaxAge reads the MaxAge of the stream backing a KV bucket.
func handoffBucketMaxAge(t *testing.T, ctx context.Context, js jetstream.JetStream, bucket string) time.Duration {
	t.Helper()

	return handoffBucketStreamConfig(t, ctx, js, bucket).MaxAge
}

// twoPhaseHandoffConfig returns a fast test config with two-phase handoff
// enabled and a dedicated handoff bucket name.
func twoPhaseHandoffConfig(handoffBucket string) Config {
	cfg := TestConfig()
	cfg.EnableTwoPhaseHandoff = true
	cfg.KVBuckets.HandoffBucket = handoffBucket

	return cfg
}

// TestReconcileHandoffBucketMaxAge_ClearsExistingMaxAge verifies that a handoff
// bucket pre-created with a non-zero MaxAge (e.g. by an older parti version) is
// healed to MaxAge=0 when the Manager starts.
func TestReconcileHandoffBucketMaxAge_ClearsExistingMaxAge(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "reconcile-clears-handoff"

	// Pre-create the handoff bucket with a non-zero MaxAge.
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket, TTL: 30 * time.Second})
	require.NoError(t, err)
	require.Equal(t, 30*time.Second, handoffBucketMaxAge(t, ctx, js, bucket))

	cfg := twoPhaseHandoffConfig(bucket)
	mgr, err := NewManager(&cfg, js, source.NewStatic([]Partition{{Keys: []string{"p1"}}}), strategy.NewConsistentHash())
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	require.Equal(t, time.Duration(0), handoffBucketMaxAge(t, ctx, js, bucket),
		"Manager startup must clear the handoff bucket's MaxAge")
}

// TestReconcileHandoffBucketMaxAge_PreservesExistingStreamConfig verifies that
// the reconcile path relaxes only MaxAge and does not drift unrelated fields on
// a bucket that was pre-created outside this Manager.
func TestReconcileHandoffBucketMaxAge_PreservesExistingStreamConfig(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "reconcile-preserves-handoff"

	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      bucket,
		Description: "operator-created handoff bucket",
		TTL:         30 * time.Second,
		History:     4,
		MaxBytes:    4096,
		Storage:     jetstream.MemoryStorage,
	})
	require.NoError(t, err)

	before := handoffBucketStreamConfig(t, ctx, js, bucket)
	require.Equal(t, 30*time.Second, before.MaxAge)
	require.Equal(t, int64(4), before.MaxMsgsPerSubject)
	require.Equal(t, int64(4096), before.MaxBytes)
	require.Equal(t, jetstream.MemoryStorage, before.Storage)

	cfg := twoPhaseHandoffConfig(bucket)
	mgr, err := NewManager(&cfg, js, source.NewStatic([]Partition{{Keys: []string{"p1"}}}), strategy.NewConsistentHash())
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	after := handoffBucketStreamConfig(t, ctx, js, bucket)
	require.Equal(t, time.Duration(0), after.MaxAge,
		"Manager startup must clear only the handoff bucket's MaxAge")
	require.Equal(t, before.Name, after.Name)
	require.Equal(t, before.Subjects, after.Subjects)
	require.Equal(t, before.Description, after.Description)
	require.Equal(t, before.MaxMsgsPerSubject, after.MaxMsgsPerSubject)
	require.Equal(t, before.MaxBytes, after.MaxBytes)
	require.Equal(t, before.Storage, after.Storage)
	require.Equal(t, before.Replicas, after.Replicas)
}

// TestReconcileHandoffBucketMaxAge_FailLoudWhenUpdateDenied verifies that when
// the handoff bucket has a non-zero MaxAge and the stream update is rejected
// (e.g. a least-privilege NATS user), Manager.Start fails loudly with an
// actionable error rather than continuing into a delayed silent outage.
func TestReconcileHandoffBucketMaxAge_FailLoudWhenUpdateDenied(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "reconcile-faillloud-handoff"

	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket, TTL: 30 * time.Second})
	require.NoError(t, err)

	// Simulate a NATS user without stream-update permission.
	orig := kvStreamUpdate
	kvStreamUpdate = func(_ context.Context, _ jetstream.JetStream, _ jetstream.StreamConfig) error {
		return errors.New("nats: permissions violation for stream update")
	}
	defer func() { kvStreamUpdate = orig }()

	cfg := twoPhaseHandoffConfig(bucket)
	mgr, err := NewManager(&cfg, js, source.NewStatic([]Partition{{Keys: []string{"p1"}}}), strategy.NewConsistentHash())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	startErr := mgr.Start(ctx)
	require.Error(t, startErr, "Manager.Start must fail loudly when the handoff MaxAge cannot be cleared")
	require.Contains(t, startErr.Error(), "MaxAge")
	require.Contains(t, startErr.Error(), bucket)
}

// TestReconcileHandoffBucketMaxAge_LeastPrivilegeHappyPath verifies that a
// correctly-provisioned handoff bucket (no MaxAge) needs no stream update, so a
// least-privilege NATS user without stream-update permission starts cleanly.
func TestReconcileHandoffBucketMaxAge_LeastPrivilegeHappyPath(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "reconcile-happy-handoff"

	// Pre-create the handoff bucket correctly: no MaxAge.
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	// Any stream update would be a bug — fail the test if reconcile calls it.
	orig := kvStreamUpdate
	kvStreamUpdate = func(_ context.Context, _ jetstream.JetStream, _ jetstream.StreamConfig) error {
		t.Error("reconcile must not attempt a stream update when MaxAge is already 0")
		return errors.New("unexpected stream update")
	}
	defer func() { kvStreamUpdate = orig }()

	cfg := twoPhaseHandoffConfig(bucket)
	mgr, err := NewManager(&cfg, js, source.NewStatic([]Partition{{Keys: []string{"p1"}}}), strategy.NewConsistentHash())
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx),
		"Manager.Start must succeed without stream-update permission when the bucket is already correct")
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })
}

// TestReconcileHandoffBucketMaxAge_Concurrent verifies that concurrent
// reconcile attempts against the same bucket all succeed and converge on
// MaxAge=0 — mirroring multiple workers running setupHandoff at once.
func TestReconcileHandoffBucketMaxAge_Concurrent(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("embedded nats-server reports an internal race on concurrent stream updates")
	}

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "reconcile-concurrent-handoff"

	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket, TTL: 30 * time.Second})
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
			errs[i] = reconcileHandoffBucketMaxAge(ctx, js, kv, bucket, nil)
		})
	}
	wg.Wait()

	for i, e := range errs {
		require.NoError(t, e, "concurrent reconcile worker %d", i)
	}
	require.Equal(t, time.Duration(0), handoffBucketMaxAge(t, ctx, js, bucket))
}

// TestReconcileHandoffBucketMaxAge_GetFirstDoesNotRewiden verifies the
// rolling-upgrade safety property: once a handoff bucket has MaxAge=0, an
// opener that passes a non-zero TTL (mirroring an older parti binary's
// ensureKVBucket call) does NOT re-widen it — EnsureKVBucketWithRetry is
// get-first, so an old worker cannot undo the fix.
func TestReconcileHandoffBucketMaxAge_GetFirstDoesNotRewiden(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "reconcile-getfirst-handoff"

	// Bucket already in the fixed state: no MaxAge.
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)

	// An opener passing a non-zero TTL (an older parti binary's setupHandoff).
	_, err = kvutil.EnsureKVBucketWithRetry(ctx, js,
		kvbuckets.BuildKeyValueConfig(bucket, 2*time.Minute, jetstream.FileStorage), 5)
	require.NoError(t, err)

	require.Equal(t, time.Duration(0), handoffBucketMaxAge(t, ctx, js, bucket),
		"a get-first open must not re-widen an existing no-MaxAge handoff bucket")
}

// TestReconcileHandoffBucketMaxAge_FailsWhenStatusUnreadable verifies the
// fail-loud contract when the bucket's MaxAge cannot be verified at all (e.g. a
// transient error or an expired startup context): reconcile must return an
// error rather than silently succeed and leave a possibly-non-zero MaxAge in
// place.
func TestReconcileHandoffBucketMaxAge_FailsWhenStatusUnreadable(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "reconcile-unreadable-handoff"

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket, TTL: 30 * time.Second})
	require.NoError(t, err)

	// A cancelled context makes kv.Status fail — reconcile cannot verify the
	// bucket's MaxAge and must fail loud rather than silently succeed.
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()

	err = reconcileHandoffBucketMaxAge(cancelled, js, kv, bucket, nil)
	require.Error(t, err, "reconcile must fail loud when it cannot read the bucket's MaxAge")
	require.Contains(t, err.Error(), bucket)
}

// errListSource is a PartitionSource whose List always fails.
type errListSource struct{ err error }

func (e *errListSource) Start(_ context.Context) error                     { return nil }
func (e *errListSource) List(_ context.Context) ([]types.Partition, error) { return nil, e.err }
func (e *errListSource) Stop(_ context.Context) error                      { return nil }

// TestLivePartitionSet pins the orphan-reap supplier's vouching contract:
// only a leader with a healthy source AND a readable commit answers ok=true
// (the reap pass deletes claims based on this set, so an unverifiable view
// must never authorize deletion), and the vouched set is the union of the
// source view and the latest committed assignment's partitions.
func TestLivePartitionSet(t *testing.T) {
	t.Parallel()

	// newManager wires the commit-read seam to "no commit exists" by
	// default; subtests override the hooks to exercise the commit arm.
	newManager := func(src types.PartitionSource) *Manager {
		return &Manager{
			source: src,
			testHookCommitRead: func(_ context.Context) (*types.AssignmentCommit, uint64, error) {
				return nil, 0, nil
			},
		}
	}

	t.Run("non-leader never vouches", func(t *testing.T) {
		t.Parallel()
		m := newManager(source.NewStatic([]types.Partition{{Keys: []string{"p-0"}}}))
		// isLeader defaults to false (zero value)

		set, ok := m.livePartitionSet(t.Context())
		require.False(t, ok, "a follower's source view must never authorize reaping")
		require.Nil(t, set)
	})

	t.Run("leader vouches with SubjectKey-keyed set", func(t *testing.T) {
		t.Parallel()
		m := newManager(source.NewStatic([]types.Partition{
			{Keys: []string{"p-0"}},
			{Keys: []string{"region", "p-1"}}, // multi-key: claim key is dot-joined
		}))
		m.isLeader.Store(true)

		set, ok := m.livePartitionSet(t.Context())
		require.True(t, ok)
		require.Len(t, set, 2)
		require.Contains(t, set, "p-0")
		require.Contains(t, set, "region.p-1",
			"set must be keyed by SubjectKey — the identity claims are stored under")
	})

	t.Run("source error never vouches", func(t *testing.T) {
		t.Parallel()
		m := newManager(&errListSource{err: errors.New("boom")})
		m.isLeader.Store(true)

		set, ok := m.livePartitionSet(t.Context())
		require.False(t, ok, "an unreadable source must never authorize reaping")
		require.Nil(t, set)
	})

	t.Run("committed partitions stay live even when the source dropped them", func(t *testing.T) {
		t.Parallel()
		// Source no longer lists p-stalled, but the live commit still
		// references it (the stalled-rebalance window): its owner is still
		// consuming it through the gate, so it must not be reap-eligible.
		m := newManager(source.NewStatic([]types.Partition{{Keys: []string{"p-0"}}}))
		m.isLeader.Store(true)
		m.testHookCommitRead = func(_ context.Context) (*types.AssignmentCommit, uint64, error) {
			return &types.AssignmentCommit{Version: 7}, 42, nil
		}
		m.testHookCommitBatch = func(_ context.Context, c *types.AssignmentCommit) (map[string]struct{}, error) {
			require.EqualValues(t, 7, c.Version)
			return map[string]struct{}{"p-stalled": {}}, nil
		}

		set, ok := m.livePartitionSet(t.Context())
		require.True(t, ok)
		require.Contains(t, set, "p-0")
		require.Contains(t, set, "p-stalled",
			"a partition the live commit references is not an orphan, source view notwithstanding")
	})

	t.Run("commit read error never vouches", func(t *testing.T) {
		t.Parallel()
		m := newManager(source.NewStatic([]types.Partition{{Keys: []string{"p-0"}}}))
		m.isLeader.Store(true)
		m.testHookCommitRead = func(_ context.Context) (*types.AssignmentCommit, uint64, error) {
			return nil, 0, errors.New("kv down")
		}

		set, ok := m.livePartitionSet(t.Context())
		require.False(t, ok, "an unverifiable commit view must never authorize reaping")
		require.Nil(t, set)
	})
}

// TestManagerHandoff_HealthyLeaderHealsStaleClaimWithinSweepInterval pins
// design §4.11's manager-level healthy-leader companion to the
// handoff-package §4.10 no-leader proof
// (TestSweepAuthorityLive_NoLeaderHealingWithinI2Bound): with a real,
// single-worker Manager — which becomes leader synchronously by the time
// Start returns, a lone worker always winning election — a stale claim
// (an expired-TTL prepare, planted directly into the handoff bucket,
// bypassing the manager/coordinator entirely) heals to stable within
// ~SweepInterval via the ticker sweep's full (leader, unauthorized-gating
// never applies) cadence, exactly today's pre-gating behavior. This is
// the regression pin the leader-gated backstop cadence must not weaken.
func TestManagerHandoff_HealthyLeaderHealsStaleClaimWithinSweepInterval(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const bucket = "healthy-leader-heals-stale-claim"

	cfg := twoPhaseHandoffConfig(bucket)
	// Short, explicit sweep interval: twoPhaseHandoffConfig leaves
	// Handoff.SweepInterval at zero, which coordinator.New coerces to the
	// 30s production default (see TestHandoffConflictStress's identical
	// note) — too slow for a "heals within ~SweepInterval" test bound.
	cfg.Handoff.SweepInterval = 1 * time.Second

	mgr, err := NewManager(&cfg, js, source.NewStatic([]Partition{{Keys: []string{"p-0"}}}), strategy.NewConsistentHash())
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	require.True(t, mgr.IsLeader(), "a lone worker must become leader synchronously on Start")

	kv, err := js.KeyValue(ctx, bucket)
	require.NoError(t, err)
	store := handoff.NewNATSClaimStore(kv, "claims/")

	// A stuck, TTL-expired prepare for a partition the manager's own
	// source never lists — nothing but the coordinator's reconcile arm
	// (independent of LivePartitions/leadership) can ever touch it.
	stuck := handoff.Claim{
		PartitionID:  "p-stuck",
		Owner:        "worker-A",
		PendingOwner: "worker-B",
		State:        handoff.ClaimStatePrepare,
		Epoch:        1,
		LastUpdated:  time.Now().UTC().Add(-time.Hour),
		TTLSeconds:   1, // long since expired relative to LastUpdated
	}
	_, err = store.PutIfEpoch(ctx, "p-stuck", 0, stuck)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		got, rev, gerr := store.Get(ctx, "p-stuck")
		if gerr != nil || rev == 0 {
			return false
		}

		return got.State == handoff.ClaimStateStable && got.PendingOwner == ""
	}, 3*cfg.Handoff.SweepInterval, 50*time.Millisecond,
		"a stale claim must heal to stable within ~SweepInterval when a healthy leader exists")
}
