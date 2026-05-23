package parti

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/arloliu/parti/v2/partitest"
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

// replicaWarnSpy captures WARN lines for the mismatch test.
type replicaWarnSpy struct {
	mu    sync.Mutex
	warns []string
}

func (l *replicaWarnSpy) Debug(string, ...any) {}
func (l *replicaWarnSpy) Info(string, ...any)  {}
func (l *replicaWarnSpy) Warn(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}
func (l *replicaWarnSpy) Error(string, ...any) {}
func (l *replicaWarnSpy) Fatal(string, ...any) {}

func (l *replicaWarnSpy) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.warns))
	copy(out, l.warns)

	return out
}

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

	spy := &replicaWarnSpy{}
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

	spy := &replicaWarnSpy{}
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
