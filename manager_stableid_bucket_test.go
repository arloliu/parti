package parti

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

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
