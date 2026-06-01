package failure_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/parti/v2"
	"github.com/arloliu/parti/v2/internal/testutil"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// --- NP-9 full quorum-loss fault seam ----------------------------------------
//
// NP-9 reproduces a TOTAL coordination-plane quorum loss while the NATS
// connection stays UP: ops against ALL FOUR coordination buckets
// (Election/Heartbeat/StableID/Assignment) AND their watchers time out with
// context.DeadlineExceeded, but the client never disconnects. This is the
// genuine "RAFT quorum lost, TCP healthy" condition.
//
// The seam is modeled on the manager package's kuFault* seam. Because the
// failure_test package does not have those symbols, NP-9 defines its own copy
// (np9FaultController/np9FaultJetStream/np9FaultKeyValue) with two additions
// over the original: a disarm() method (to drive watcher-independent recovery)
// and a faulting Watch() override (the original lets Watch pass through; for a
// FULL quorum loss the watcher must fault too).
//
// The point of the test is ARBITRATION: with both the fast
// heartbeat/election/stableid threshold path AND the assignment-watcher
// exhaustion path racing to win the enterDegraded CAS, the fast Put/Update/Get
// threshold path wins under production watcher cadence — so the first
// OnDegraded reason is "kv-unavailable", NOT "assignment-watcher-exhausted".

type np9FaultController struct {
	armed    atomic.Bool
	injected atomic.Int64
}

func (fc *np9FaultController) arm()    { fc.injected.Store(0); fc.armed.Store(true) }
func (fc *np9FaultController) disarm() { fc.armed.Store(false) }
func (fc *np9FaultController) fault() bool {
	if !fc.armed.Load() {
		return false
	}
	fc.injected.Add(1)

	return true
}

type np9FaultJetStream struct {
	jetstream.JetStream
	buckets map[string]struct{}
	fc      *np9FaultController
}

func (f *np9FaultJetStream) wrap(kv jetstream.KeyValue, bucket string) jetstream.KeyValue {
	if _, ok := f.buckets[bucket]; ok {
		return &np9FaultKeyValue{KeyValue: kv, fc: f.fc}
	}

	return kv
}

func (f *np9FaultJetStream) KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.KeyValue(ctx, bucket)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, bucket), nil
}

func (f *np9FaultJetStream) CreateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.CreateKeyValue(ctx, cfg)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, cfg.Bucket), nil
}

func (f *np9FaultJetStream) CreateOrUpdateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	kv, err := f.JetStream.CreateOrUpdateKeyValue(ctx, cfg)
	if err != nil {
		return kv, err
	}

	return f.wrap(kv, cfg.Bucket), nil
}

// np9FaultKeyValue faults every active op the manager's periodic loops use
// (heartbeat Put, election renew Update, follower request Create, stableid renew
// Update, assignment Get refresh) AND the assignment Watch when armed. This is a
// FULL quorum loss across the coordination plane — both the fast threshold path
// and the watcher exhaustion path are driven simultaneously, letting the test
// observe which one wins the enterDegraded CAS.
type np9FaultKeyValue struct {
	jetstream.KeyValue
	fc *np9FaultController
}

func (k *np9FaultKeyValue) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	if k.fc.fault() {
		return 0, context.DeadlineExceeded
	}

	return k.KeyValue.Put(ctx, key, value)
}

func (k *np9FaultKeyValue) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if k.fc.fault() {
		return 0, context.DeadlineExceeded
	}

	return k.KeyValue.Update(ctx, key, value, revision)
}

func (k *np9FaultKeyValue) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if k.fc.fault() {
		return 0, context.DeadlineExceeded
	}

	return k.KeyValue.Create(ctx, key, value, opts...)
}

func (k *np9FaultKeyValue) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if k.fc.fault() {
		return nil, context.DeadlineExceeded
	}

	return k.KeyValue.Get(ctx, key)
}

// Watch faults too: for a FULL quorum loss the assignment watcher cannot make
// progress. When armed it returns context.DeadlineExceeded so the watcher's
// reconnect/retry envelope drives its exhaustion path. When disarmed, Watch
// passes through so the watcher can re-arm during recovery.
func (k *np9FaultKeyValue) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	if k.fc.fault() {
		return nil, context.DeadlineExceeded
	}

	return k.KeyValue.Watch(ctx, keys, opts...)
}

// TestNP9_FullQuorumLoss_KVUnavailableWins_RecoversToStable is the NP-9 proof.
//
// It faults all four coordination buckets plus their watchers (connection stays
// UP) and asserts (a) ARBITRATION: the first OnDegraded reason is the fast
// threshold path's "kv-unavailable", not the watcher's
// "assignment-watcher-exhausted"; (b) RECOVERY: after disarm the manager returns
// to Stable via the watcher-independent Get-based refresh; (c) NO FLAP: exactly
// one Degraded entry and one Stable exit across the whole test.
func TestNP9_FullQuorumLoss_KVUnavailableWins_RecoversToStable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Parallel()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	t.Cleanup(cleanup)

	realJS, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := testutil.IntegrationTestConfig()
	// Degrade quickly and deterministically under the fault. Leave the
	// assignment-watcher cadence at production defaults (do NOT shrink
	// watcherMaxAttempts) so the realistic timing has the fast threshold path
	// win the enterDegraded CAS.
	cfg.DegradedBehavior.KVErrorThreshold = 3
	cfg.DegradedBehavior.KVErrorWindow = 15 * time.Second
	cfg.DegradedBehavior.ExitThreshold = 2 * time.Second

	fc := &np9FaultController{}
	faultJS := &np9FaultJetStream{
		JetStream: realJS,
		fc:        fc,
		buckets: map[string]struct{}{
			cfg.KVBuckets.ElectionBucket:   {},
			cfg.KVBuckets.HeartbeatBucket:  {},
			cfg.KVBuckets.StableIDBucket:   {},
			cfg.KVBuckets.AssignmentBucket: {},
		},
	}

	src := source.NewStatic(testutil.CreateTestPartitions(4))

	// Buffered so OnDegraded never blocks; the first reason is the arbitration
	// outcome under test.
	degradedReasons := make(chan string, 8)
	var np9DegradedEntries atomic.Int64
	var np9StableExits atomic.Int64
	hooks := &parti.Hooks{
		OnDegraded: func(_ context.Context, reason string) error {
			select {
			case degradedReasons <- reason:
			default:
			}

			return nil
		},
		OnStateChanged: func(_ context.Context, from, to parti.State) error {
			if to == parti.StateDegraded {
				np9DegradedEntries.Add(1)
			}
			if from == parti.StateStable && to != parti.StateStable {
				np9StableExits.Add(1)
			}

			return nil
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	mgr, err := parti.NewManager(&cfg, faultJS, src, strategy.NewConsistentHash(), parti.WithHooks(hooks))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	require.NoError(t, mgr.Start(ctx))
	require.NoError(t, <-mgr.WaitState(parti.StateStable, 30*time.Second),
		"manager must stabilize before the fault is armed")

	// Arm the full quorum loss: every op against all four coordination buckets,
	// and their watchers, now times out — but the connection stays up.
	fc.arm()

	// ARBITRATION: the FIRST OnDegraded reason must be the fast threshold path's
	// "kv-unavailable". "assignment-watcher-exhausted" (or any other) would be an
	// arbitration finding worth recording.
	select {
	case reason := <-degradedReasons:
		require.Equal(t, "kv-unavailable", reason,
			"full quorum loss must degrade with the fast threshold path's reason, not the watcher's")
	case <-time.After(30 * time.Second):
		t.Fatalf("manager did not enter Degraded within 30s of the full quorum-loss fault; state=%s", mgr.State())
	}

	require.Equal(t, parti.StateDegraded, mgr.State())
	// CONNECTION UP: this is a genuine quorum loss, not a connection-down event.
	require.True(t, nc.IsConnected(),
		"the NATS connection must remain CONNECTED throughout (this is the whole point)")

	// RECOVERY: disarm the fault and confirm the manager returns to Stable via
	// the watcher-independent Get-based refresh.
	fc.disarm()
	require.NoError(t, <-mgr.WaitState(parti.StateStable, 20*time.Second),
		"manager must recover to Stable after the quorum loss clears")

	// NON-VACUITY: the fault genuinely fired (the manager actually hit the
	// timing-out buckets, it did not degrade for an unrelated reason).
	require.Positive(t, fc.injected.Load(), "the full quorum-loss fault must have actually injected")

	// NO FLAP: across the whole test there must be exactly ONE Degraded entry and
	// ONE Stable exit — no oscillation between Stable and Degraded.
	require.Eventually(t, func() bool {
		return np9DegradedEntries.Load() == 1 && np9StableExits.Load() == 1
	}, 5*time.Second, 100*time.Millisecond,
		"expected exactly one Degraded entry and one Stable exit (no flap)")
	require.Equal(t, int64(1), np9DegradedEntries.Load(),
		"manager must enter Degraded exactly once (no flap)")
	require.Equal(t, int64(1), np9StableExits.Load(),
		"manager must exit Stable exactly once (no flap)")
}
