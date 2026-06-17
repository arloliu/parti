package parti

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/arloliu/parti/v2/internal/ratelimit"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

type recordingObserver struct{ ns []int }

func (r *recordingObserver) ObserveWorkerCount(n int) { r.ns = append(r.ns, n) }

// syncObserver is a concurrency-safe FleetSizeObserver for the commit-watch
// decode-path test, where ObserveWorkerCount runs on the watch-session
// goroutine while the test goroutine asserts.
type syncObserver struct {
	mu sync.Mutex
	ns []int
}

func (o *syncObserver) ObserveWorkerCount(n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ns = append(o.ns, n)
}

func (o *syncObserver) snapshot() []int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]int(nil), o.ns...)
}

func newFleetTestManager(lim ratelimit.Limiter, obs FleetSizeObserver, clusterRate float64) *Manager {
	m := &Manager{logger: logging.NewNop(), claimWriteLimiter: lim, fleetSizeObserver: obs}
	m.cfg.Handoff.ClaimWriteClusterRate = clusterRate
	m.cfg.Handoff.ClaimWritePerSec = 200
	return m
}

func commit(version int64, lr uint64, workers ...string) *types.AssignmentCommit {
	return &types.AssignmentCommit{Version: version, LeaderRevision: lr, Workers: workers}
}

func TestObserveFleetSize_RetunesClaimWriteAndPushes(t *testing.T) {
	lim := ratelimit.New(200, 50, nil)
	obs := &recordingObserver{}
	m := newFleetTestManager(lim, obs, 1000)

	m.observeFleetSize(commit(1, 1, "a", "b", "c", "d", "e")) // N=5
	require.InEpsilon(t, 200.0, lim.Limit(), 1e-9, "min(200, 1000/5=200) = 200")
	require.Equal(t, []int{5}, obs.ns)

	m.observeFleetSize(commit(2, 1, "a", "b", "c", "d", "e", "f", "g", "h", "i", "j")) // N=10
	require.InEpsilon(t, 100.0, lim.Limit(), 1e-9, "min(200, 1000/10=100) = 100")
	require.Equal(t, []int{5, 10}, obs.ns)
}

func TestObserveFleetSize_StaleCommit_NoRegression(t *testing.T) {
	lim := ratelimit.New(200, 50, nil)
	obs := &recordingObserver{}
	m := newFleetTestManager(lim, obs, 1000)

	m.observeFleetSize(commit(5, 2, "a", "b", "c", "d", "e", "f", "g", "h", "i", "j")) // v5, N=10 -> 100
	require.InEpsilon(t, 100.0, lim.Limit(), 1e-9)

	m.observeFleetSize(commit(3, 2, "a", "b")) // v3 < v5: stale
	require.InEpsilon(t, 100.0, lim.Limit(), 1e-9, "stale commit must not regress rate")
	require.Equal(t, []int{10}, obs.ns, "stale commit must not push")

	m.observeFleetSize(commit(5, 1, "a", "b")) // same v5, lower lr: stale
	require.Equal(t, []int{10}, obs.ns)
}

func TestObserveFleetSize_StaleLeaderCommit_NoRegression(t *testing.T) {
	lim := ratelimit.New(200, 50, nil)
	obs := &recordingObserver{}
	m := newFleetTestManager(lim, obs, 1000)
	m.lastSeenLeaderRevision.Store(100) // we have applied a commit from leader-rev 100

	// Authoritative commit from the current leader: 10 workers -> min(200,1000/10)=100.
	m.observeFleetSize(commit(7, 100, "a", "b", "c", "d", "e", "f", "g", "h", "i", "j"))
	require.InEpsilon(t, 100.0, lim.Limit(), 1e-9)
	require.Equal(t, []int{10}, obs.ns)

	// Split-brain: a stale former leader writes a HIGHER version with a LOWER
	// (stale) leader-revision and a shrunken worker-set. The apply path rejects
	// it (LeaderRevision 50 < lastSeen 100); observe must reject it too and NOT
	// regress N to 1.
	m.observeFleetSize(commit(8, 50, "a"))
	require.InEpsilon(t, 100.0, lim.Limit(), 1e-9, "stale-leader commit must not regress the rate")
	require.Equal(t, []int{10}, obs.ns, "stale-leader commit must not push N=1")
}

func TestObserveFleetSize_NoOpWhenNUnchanged(t *testing.T) {
	lim := ratelimit.New(200, 50, nil)
	obs := &recordingObserver{}
	m := newFleetTestManager(lim, obs, 1000)
	m.observeFleetSize(commit(1, 1, "a", "b"))
	m.observeFleetSize(commit(2, 1, "a", "b")) // version advanced, N still 2
	require.Equal(t, []int{2}, obs.ns, "unchanged N must not push again")
}

func TestObserveFleetSize_NoClusterRate_NoRetune(t *testing.T) {
	lim := ratelimit.New(200, 50, nil)
	m := newFleetTestManager(lim, nil, 0) // clusterRate=0
	m.observeFleetSize(commit(1, 1, "a", "b", "c"))
	require.InEpsilon(t, 200.0, lim.Limit(), 1e-9, "clusterRate=0 leaves limiter at perSec")
}

func TestObserveFleetSize_ClampsNToOne(t *testing.T) {
	lim := ratelimit.New(200, 50, nil)
	m := newFleetTestManager(lim, nil, 50)
	m.observeFleetSize(commit(1, 1)) // empty Workers -> clamp N=1
	require.InEpsilon(t, 50.0, lim.Limit(), 1e-9, "min(200, 50/1=50) = 50")
}

func TestObserveFleetSize_NilCommit(t *testing.T) {
	lim := ratelimit.New(200, 50, nil)
	obs := &recordingObserver{}
	m := newFleetTestManager(lim, obs, 1000)
	require.NotPanics(t, func() { m.observeFleetSize(nil) })
	require.Empty(t, obs.ns, "nil commit must not push")
}

// fleetCommitEntry builds a fake KV entry for an AssignmentCommit whose full
// Workers slice and this-worker payload hash are controllable, so the test can
// hold THIS worker's assignment fixed (stable PayloadHash) while the fleet
// size (len(Workers)) changes.
func fleetCommitEntry(version int64, lr uint64, selfID, selfHash string, workers ...string) jetstream.KeyValueEntry {
	payloads := map[string]types.AssignmentPayloadRef{
		selfID: {PayloadHash: selfHash},
	}
	b, _ := json.Marshal(types.AssignmentCommit{
		Version:        version,
		LeaderRevision: lr,
		Workers:        workers,
		Payloads:       payloads,
	})

	return fakeVersionEntry{value: b}
}

// TestObserveFleetSize_FiresPreDebounce_OnSliceUnchangedFleetGrowth proves the
// fleet observation is wired at the pre-debounce decode point: it fires for
// every decoded commit even when the commit debouncer suppresses the APPLY
// because THIS worker's own partition slice is unchanged.
//
// Two commits are fed through the real runCommitWatchSession decode path with
// a fake watcher. This worker (worker-test) holds the same PayloadHash in both
// (so workerAssignmentChanged is false and the debouncer collapses them into a
// single dispatch), but the fleet grows from N=1 to N=2 as a peer joins. The
// observer must record BOTH worker-counts while only one commit reaches the
// post-debounce dispatch surface.
func TestObserveFleetSize_FiresPreDebounce_OnSliceUnchangedFleetGrowth(t *testing.T) {
	const window = 100 * time.Millisecond
	m, _, _, _ := newTestManager(t)
	m.cfg.AssignmentWatcherDebounce = window
	workerID := m.WorkerID()

	obs := &syncObserver{}
	m.fleetSizeObserver = obs
	lim := ratelimit.New(200, 50, nil)
	m.claimWriteLimiter = lim
	m.cfg.Handoff.ClaimWritePerSec = 200
	// clusterRate=300 makes N=1 and N=2 produce DISTINCT effective rates
	// (N=1 -> min(200,300)=200; N=2 -> min(200,150)=150), so the final
	// limit proves the retune followed the grown (suppressed-apply) fleet.
	m.cfg.Handoff.ClaimWriteClusterRate = 300

	// Count post-debounce dispatches so we can assert the debouncer DID
	// suppress the second apply (slice unchanged for this worker).
	var (
		mu         sync.Mutex
		dispatches []*types.AssignmentCommit
	)
	m.testHookHandleCommitValue = func(c *types.AssignmentCommit) {
		mu.Lock()
		defer mu.Unlock()
		dispatches = append(dispatches, c)
	}

	watcher := newFakeKeyWatcher()
	go func() {
		_ = m.runCommitWatchSession(m.ctx, nil, watcher, nil, "assignment._commit")
	}()

	// V=10: this worker alone owns hash "aaaa" -> N=1.
	watcher.ch <- fleetCommitEntry(10, 10, workerID, "aaaa", workerID)
	time.Sleep(20 * time.Millisecond)

	// V=11: a peer joins. This worker still owns hash "aaaa" (slice
	// unchanged for us) -> workerAssignmentChanged is false, so the
	// debouncer collapses V=10 into V=11 and only ONE apply dispatches.
	// But N grows 1 -> 2, which the pre-debounce observer must still see.
	watcher.ch <- fleetCommitEntry(11, 11, workerID, "aaaa", workerID, "peer")

	// Wait past the idle window for the single timer-flush.
	time.Sleep(window + 100*time.Millisecond)

	// The observer must have seen BOTH fleet sizes, even though only one
	// commit reached the apply path.
	require.Equal(t, []int{1, 2}, obs.snapshot(),
		"pre-debounce observer must see both N=1 and N=2 even when the slice-unchanged apply is debounced")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, dispatches, 1,
		"slice-unchanged commits must collapse to a single apply (debouncer suppression)")
	require.Equal(t, int64(11), dispatches[0].Version,
		"the single dispatched apply is the highest-version commit")

	// The retune followed the latest observed N=2: min(200, 300/2=150)=150,
	// distinct from the N=1 rate (200), proving observation tracked the
	// grown fleet even though the second apply was debounced away.
	require.InEpsilon(t, 150.0, lim.Limit(), 1e-9)
}
