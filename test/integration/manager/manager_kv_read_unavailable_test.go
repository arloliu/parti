package manager_test

import (
	"context"
	"sync/atomic"
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

// --- connected-but-KV-unavailable fault seam ---------------------------------
//
// This seam reproduces the quorum-loss condition F-D1 targets: the NATS
// connection stays CONNECTED, but ops against specific buckets time out with
// context.DeadlineExceeded (their RAFT quorum is lost). It wraps the JetStream
// handle and, for an armed set of buckets, faults every KV op. Setup runs
// BEFORE arming, so bucket creation / warm succeed; arming after Stable mimics
// quorum loss striking a healthy cluster — exactly the bucket-wipe test's shape
// but with a read/op timeout instead of a delete, and the connection still up.

type kuFaultController struct {
	armed    atomic.Bool
	injected atomic.Int64
}

func (fc *kuFaultController) arm() { fc.injected.Store(0); fc.armed.Store(true) }
func (fc *kuFaultController) fault() bool {
	if !fc.armed.Load() {
		return false
	}
	fc.injected.Add(1)

	return true
}

type kuFaultJetStream struct {
	jetstream.JetStream
	buckets map[string]struct{}
	fc      *kuFaultController
}

func (f *kuFaultJetStream) wrap(kv jetstream.KeyValue, bucket string) jetstream.KeyValue {
	if _, ok := f.buckets[bucket]; ok {
		return &kuFaultKeyValue{KeyValue: kv, fc: f.fc}
	}

	return kv
}

func (f *kuFaultJetStream) KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.KeyValue(ctx, bucket)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, bucket), nil
}

func (f *kuFaultJetStream) CreateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.CreateKeyValue(ctx, cfg)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, cfg.Bucket), nil
}

func (f *kuFaultJetStream) CreateOrUpdateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.CreateOrUpdateKeyValue(ctx, cfg)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, cfg.Bucket), nil
}

// kuFaultKeyValue faults the active write/read ops the manager's periodic loops
// use (heartbeat Put, election renew Update, follower request Create, stableid
// renew Update) with context.DeadlineExceeded when armed. Watch/WatchAll/Keys
// pass through (embedded) — the assignment watcher is intentionally NOT in the
// fault set, so its envelope's OnPermanent cannot race a competing degraded
// reason against the one under test.
type kuFaultKeyValue struct {
	jetstream.KeyValue
	fc *kuFaultController
}

func (k *kuFaultKeyValue) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	if k.fc.fault() {
		return 0, context.DeadlineExceeded
	}

	return k.KeyValue.Put(ctx, key, value)
}

func (k *kuFaultKeyValue) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if k.fc.fault() {
		return 0, context.DeadlineExceeded
	}

	return k.KeyValue.Update(ctx, key, value, revision)
}

func (k *kuFaultKeyValue) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if k.fc.fault() {
		return 0, context.DeadlineExceeded
	}

	return k.KeyValue.Create(ctx, key, value, opts...)
}

func (k *kuFaultKeyValue) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if k.fc.fault() {
		return nil, context.DeadlineExceeded
	}

	return k.KeyValue.Get(ctx, key)
}

// TestManager_KVUnavailable_EntersDegraded is the F-D1 capstone: it drives
// the REAL production trigger end-to-end. With the connection up and the
// election / heartbeat / stableid buckets timing out (context.DeadlineExceeded),
// the manager must enter Degraded with the distinct reason "kv-unavailable"
// — the observability the operator relies on instead of a silent stall.
//
// Before F-D1 those timeouts were dropped by recordKVError (neither connectivity
// nor degrading-JetStream), so the manager never degraded.
func TestManager_KVUnavailable_EntersDegraded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	// Degrade quickly and deterministically under the fault.
	cfg.DegradedBehavior.KVErrorThreshold = 3
	cfg.DegradedBehavior.KVErrorWindow = 15 * time.Second

	fc := &kuFaultController{}
	faultJS := &kuFaultJetStream{
		JetStream: realJS,
		fc:        fc,
		buckets: map[string]struct{}{
			cfg.KVBuckets.ElectionBucket:  {},
			cfg.KVBuckets.HeartbeatBucket: {},
			cfg.KVBuckets.StableIDBucket:  {},
		},
	}

	src := source.NewStatic(testutil.CreateTestPartitions(4))

	degradedReasons := make(chan string, 8)
	hooks := &parti.Hooks{
		OnDegraded: func(_ context.Context, reason string) error {
			select {
			case degradedReasons <- reason:
			default:
			}

			return nil
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	mgr, err := parti.NewManager(&cfg, faultJS, src, strategy.NewConsistentHash(), parti.WithHooks(hooks))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	require.NoError(t, mgr.Start(ctx))
	require.NoError(t, <-mgr.WaitState(types.StateStable, 30*time.Second),
		"manager must stabilize before the fault is armed")

	// Arm the connected-but-KV-unavailable fault: every op against the election
	// / heartbeat / stableid buckets now times out, but the connection stays up.
	fc.arm()

	// The manager must enter Degraded with the distinct KV-unavailable reason.
	select {
	case reason := <-degradedReasons:
		require.Equal(t, "kv-unavailable", reason,
			"connected-but-KV-unavailable timeouts must degrade with the distinct reason")
	case <-time.After(30 * time.Second):
		t.Fatalf("manager did not enter Degraded within 30s of the KV-unavailable fault; state=%s", mgr.State())
	}

	require.Equal(t, types.StateDegraded, mgr.State())
	// Non-vacuous: the fault genuinely fired (the manager actually hit the
	// timing-out buckets, it did not degrade for some unrelated reason).
	require.Positive(t, fc.injected.Load(), "the KV-unavailable fault must have actually injected")
	require.True(t, nc.IsConnected(),
		"the NATS connection must remain CONNECTED throughout (this is the whole point)")
}
