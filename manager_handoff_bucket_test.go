package parti

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/kvbuckets"
	"github.com/arloliu/parti/v2/kvutil"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

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
