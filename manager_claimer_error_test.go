package parti

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/metrics"
	"github.com/arloliu/parti/v2/internal/stableid"
	"github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/strategy"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// claimerErrorTestManager builds a minimal Manager parked in StateStable, ready
// for an onClaimerError call.
func claimerErrorTestManager(t *testing.T) (*Manager, context.CancelFunc) {
	t.Helper()
	cfg := Config{
		DegradedBehavior: DegradedBehaviorConfig{
			KVErrorThreshold: 3,
			KVErrorWindow:    10 * time.Second,
		},
		DegradedAlert: DegradedAlertConfig{
			AlertInterval: 1 * time.Minute,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		logger:  logging.NewNop(),
		cfg:     cfg,
		hooks:   &Hooks{},
		metrics: metrics.NewNop(),
		ctx:     ctx,
		cancel:  cancel,
	}
	m.state.Store(int32(StateStable))

	return m, cancel
}

// TestOnClaimerError_ClaimLostStopsWorker verifies that onClaimerError responds
// to ErrClaimLost by firing the OnError hook with the typed error and invoking
// the self-stop action. recordKVError would silently drop ErrClaimLost (it is
// neither a connectivity nor a degrading-JetStream error), and Degraded mode
// does not halt processing — so the worker must be stopped outright.
func TestOnClaimerError_ClaimLostStopsWorker(t *testing.T) {
	m, cancel := claimerErrorTestManager(t)
	defer cancel()

	stopped := make(chan *Manager, 1)
	origShutdown := claimLostShutdown
	claimLostShutdown = func(mgr *Manager) { stopped <- mgr }
	defer func() { claimLostShutdown = origShutdown }()

	hookErr := make(chan error, 1)
	m.hooks.OnError = func(_ context.Context, e error) error {
		select {
		case hookErr <- e:
		default:
		}
		return nil
	}

	m.onClaimerError(fmt.Errorf("%w: ID worker-0", stableid.ErrClaimLost))

	select {
	case got := <-stopped:
		require.Same(t, m, got, "claim loss must stop this Manager")
	default:
		t.Fatal("ErrClaimLost must trigger the self-stop action")
	}

	select {
	case e := <-hookErr:
		require.ErrorIs(t, e, stableid.ErrClaimLost)
	case <-time.After(2 * time.Second):
		t.Fatal("OnError hook was not invoked with ErrClaimLost")
	}
}

// TestOnClaimerError_TransientErrorUsesKVCircuit verifies that a transient
// connectivity error flows through the windowed recordKVError circuit and does
// NOT trigger the self-stop action.
func TestOnClaimerError_TransientErrorUsesKVCircuit(t *testing.T) {
	m, cancel := claimerErrorTestManager(t)
	defer cancel()

	origShutdown := claimLostShutdown
	claimLostShutdown = func(*Manager) { t.Error("a transient error must not stop the worker") }
	defer func() { claimLostShutdown = origShutdown }()

	m.onClaimerError(nats.ErrTimeout) // a connectivity error

	require.Equal(t, int32(1), m.kvErrorCount.Load(),
		"a transient error must be recorded by the KV error circuit")
}

// recordingConsumerUpdater is a WorkerConsumerUpdater test double that records
// the most recent partition set applied to it.
type recordingConsumerUpdater struct {
	mu   sync.Mutex
	last []Partition
	set  bool
}

func (r *recordingConsumerUpdater) UpdateWorkerConsumer(_ context.Context, _ string, partitions []Partition) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = partitions
	r.set = true
	return nil
}

// revokedFinal reports whether the consumer has been updated at least once and
// its most recent partition set is empty — i.e. the worker consumer ended up
// revoked, not merely momentarily empty.
func (r *recordingConsumerUpdater) revokedFinal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.set && len(r.last) == 0
}

// TestManager_StopsItselfWhenClaimLost verifies the end-to-end claim-loss
// response: when the running worker's stable-ID key is taken over (its revision
// bumped out from under it), the renewal loop detects ErrClaimLost, and the
// Manager both stops itself and revokes its worker consumer — it does not keep
// processing partitions under the lost ID.
func TestManager_StopsItselfWhenClaimLost(t *testing.T) {
	_, nc := partitest.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	updater := &recordingConsumerUpdater{}
	cfg := TestConfig()
	mgr, err := NewManager(&cfg, js,
		source.NewStatic([]Partition{{Keys: []string{"p1"}}}),
		strategy.NewConsistentHash(),
		WithWorkerConsumerUpdater(updater),
	)
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	// Take the worker's stable-ID key over: bump its revision.
	stableKV, err := js.KeyValue(ctx, cfg.KVBuckets.StableIDBucket)
	require.NoError(t, err)
	_, err = stableKV.Put(ctx, mgr.WorkerID(), []byte("taken-over"))
	require.NoError(t, err)

	// Within a few renewal intervals (WorkerIDTTL/3) the worker must detect the
	// loss and stop itself.
	require.Eventually(t, func() bool {
		return mgr.State() == StateShutdown
	}, 30*time.Second, 200*time.Millisecond,
		"worker must stop itself after losing its stable-ID claim")

	// And its worker consumer must end up revoked (latest applied partition set
	// empty) — proving partition processing under the lost ID actually stopped,
	// not just the Manager state.
	require.Eventually(t, updater.revokedFinal, 5*time.Second, 100*time.Millisecond,
		"worker must revoke its consumer (empty partition set) after losing its claim")
}
