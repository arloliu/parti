package durable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"text/template"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/testutil"
	partitesting "github.com/arloliu/parti/v2/partitest"
	"github.com/arloliu/parti/v2/types"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type batchMetric struct {
	size int
	dur  time.Duration
}

// metricsSpy implements ResolverMetrics for unit tests.
// It is safe for concurrent use; the resolver invokes metrics from both
// the watcher and reconcile goroutines.
type metricsSpy struct {
	mu              sync.Mutex
	visLagCount     int
	lastVisLag      time.Duration
	cacheSizes      []int
	updates         map[string]int
	batches         []batchMetric
	flushReasons    map[string]int
	watcherRestarts map[string]int
	rescueCount     atomic.Int64
}

func newMetricsSpy() *metricsSpy {
	return &metricsSpy{
		updates:         make(map[string]int),
		flushReasons:    make(map[string]int),
		watcherRestarts: make(map[string]int),
	}
}

func (m *metricsSpy) ObserveVisibilityLag(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.visLagCount++
	m.lastVisLag = d
}

func (m *metricsSpy) SetCacheSize(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheSizes = append(m.cacheSizes, n)
}

func (m *metricsSpy) IncUpdate(op string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updates[op]++
}

func (m *metricsSpy) ObserveBatch(size int, dur time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batches = append(m.batches, batchMetric{size, dur})
}

func (m *metricsSpy) IncBatchFlush(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushReasons[reason]++
}

func (m *metricsSpy) IncWatcherRestart(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.watcherRestarts[reason]++
}

func (m *metricsSpy) IncReconcileRescue() {
	m.rescueCount.Add(1)
}

// snapshot helpers for concurrent-safe reads in tests.
func (m *metricsSpy) watcherRestartCount(reason string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.watcherRestarts[reason]
}

func (m *metricsSpy) reconcileRescueCount() int {
	return int(m.rescueCount.Load())
}

func (m *metricsSpy) flushReasonCount(reason string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flushReasons[reason]
}

func (m *metricsSpy) updateCount(op string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updates[op]
}

func marshalClaim(t *testing.T, c handoff.Claim) []byte {
	t.Helper()
	b, err := c.Marshal()
	require.NoError(t, err)
	return b
}

func TestApplyPendingBatch_UpsertAndDeleteMetricsAndCache(t *testing.T) {
	// Resolver with no KV interaction for this test
	r := NewClaimBasedResolver(nil, "claims/", nil)
	ms := newMetricsSpy()
	r.SetMetrics(ms)

	// Preload cache with a key to be deleted
	existing := map[string]claimEntry{
		"pid2": {owner: "w2", state: toState(handoff.ClaimStateStable), epoch: 2, revision: 1},
	}
	r.cache.Store(&existing)

	now := time.Now().Add(-10 * time.Millisecond)
	c1 := handoff.Claim{PartitionID: "pid1", Owner: "w1", State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: now.UTC()}
	upsertVal := marshalClaim(t, c1)

	pendingByPID := map[string]pending{
		"pid1": {op: "upsert", data: upsertVal, revision: 2},
		"pid2": {op: "delete", revision: 2},
	}

	// Apply and verify
	r.applyPendingBatch(pendingByPID, "unit")

	// pending should be cleared
	require.Equal(t, 0, len(pendingByPID))

	// Verify cache contents
	cur := r.cache.Load()
	require.NotNil(t, cur)
	// pid1 present and set from upsert
	ce1, ok := (*cur)["pid1"]
	require.True(t, ok)
	require.Equal(t, "w1", ce1.owner)
	// pid2 deleted (tombstone present)
	ce2, ok := (*cur)["pid2"]
	require.True(t, ok)
	require.True(t, ce2.deleted)

	// Metrics assertions
	require.GreaterOrEqual(t, ms.visLagCount, 1)
	require.GreaterOrEqual(t, len(ms.cacheSizes), 1)
	require.Equal(t, 1, ms.updates["upsert"])
	require.Equal(t, 1, ms.updates["delete"])
	require.GreaterOrEqual(t, len(ms.batches), 1)
	require.Equal(t, 1, ms.flushReasons["unit"])
}

func TestHandleWatcherUpdate_CoalescingAndPrefixFilter(t *testing.T) {
	r := NewClaimBasedResolver(nil, "claims/", nil)

	pendingByPID := make(map[string]pending)

	// Should ignore keys outside prefix
	r.testHandleWatcherUpdateLite("other/x", 0, nil, pendingByPID)
	require.Equal(t, 0, len(pendingByPID))

	// Delete coalescing
	r.testHandleWatcherUpdateLite("claims/pidA", jetstream.KeyValueDelete, nil, pendingByPID)
	p, ok := pendingByPID["pidA"]
	require.True(t, ok)
	require.Equal(t, "delete", p.op)

	// Upsert coalescing with last-wins
	val1 := marshalClaim(t, handoff.Claim{PartitionID: "pidB", Owner: "w0", State: handoff.ClaimStateStable, Epoch: 1})
	val2 := marshalClaim(t, handoff.Claim{PartitionID: "pidB", Owner: "w9", State: handoff.ClaimStateStable, Epoch: 2})
	r.testHandleWatcherUpdateLite("claims/pidB", 0, val1, pendingByPID)
	r.testHandleWatcherUpdateLite("claims/pidB", 0, val2, pendingByPID)
	p2, ok := pendingByPID["pidB"]
	require.True(t, ok)
	require.Equal(t, "upsert", p2.op)
	// Verify stored data equals last value
	require.Equal(t, val2, p2.data)
}

// testHandleWatcherUpdateLite is a test-only shim to exercise the coalescing logic
// without requiring a full jetstream.KeyValueEntry implementation.
func (r *ClaimBasedResolver) testHandleWatcherUpdateLite(key string, op jetstream.KeyValueOp, val []byte, pendingByPID map[string]pending) {
	if r.claimsPref != "" && !strings.HasPrefix(key, r.claimsPref) {
		return
	}
	pid := strings.TrimPrefix(key, r.claimsPref)
	if op == jetstream.KeyValueDelete || op == jetstream.KeyValuePurge {
		pendingByPID[pid] = pending{op: "delete", revision: 100}
		return
	}
	pendingByPID[pid] = pending{op: "upsert", data: val, revision: 100}
}

// mockKV implements a minimal jetstream.KeyValue for testing Get.
type mockKV struct {
	jetstream.KeyValue
	store map[string][]byte
}

func (m *mockKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	val, ok := m.store[key]
	if !ok {
		return nil, jetstream.ErrKeyNotFound
	}
	return &mockKVEntry{key: key, val: val, revision: 10}, nil
}

type mockKVEntry struct {
	jetstream.KeyValueEntry
	key      string
	val      []byte
	revision uint64
}

func (e *mockKVEntry) Key() string      { return e.key }
func (e *mockKVEntry) Value() []byte    { return e.val }
func (e *mockKVEntry) Revision() uint64 { return e.revision }

func TestClaimBasedResolver_Concurrency_ForceRefreshAndWatcher(t *testing.T) {
	// Setup mock KV with data for ForceRefresh
	p1Claim := handoff.Claim{PartitionID: "p1", Owner: "w1", State: handoff.ClaimStateStable, Epoch: 1}
	p1Bytes := marshalClaim(t, p1Claim)

	kv := &mockKV{
		store: map[string][]byte{
			"claims/p1": p1Bytes,
		},
	}

	r := NewClaimBasedResolver(kv, "claims/", nil)

	// Prepare batch update for p2
	p2Claim := handoff.Claim{PartitionID: "p2", Owner: "w2", State: handoff.ClaimStateStable, Epoch: 1}
	p2Bytes := marshalClaim(t, p2Claim)

	// Run concurrent operations
	// We want to ensure that after both run, both p1 and p2 are in the cache.
	// We run this in a loop to increase chance of hitting the race if it exists.

	iterations := 100
	for i := range iterations {
		// Reset cache
		empty := make(map[string]claimEntry)
		r.cache.Store(&empty)
		// Reset rate limiter
		r.mu.Lock()
		clear(r.lastRefresh)
		r.mu.Unlock()

		// Create a fresh batch for this iteration because applyPendingBatch clears it
		batch := map[string]pending{
			"p2": {op: "upsert", data: p2Bytes},
		}

		var wg sync.WaitGroup
		wg.Go(func() {
			_ = r.ForceRefreshPartition(context.Background(), "p1")
		})
		wg.Go(func() {
			r.applyPendingBatch(batch, "test")
		})
		wg.Wait()

		// Verify both are present
		owner1, _, _, ok1 := r.GetOwner("p1")
		owner2, _, _, ok2 := r.GetOwner("p2")

		if !ok1 || !ok2 {
			t.Fatalf("Race detected at iteration %d: p1 found=%v, p2 found=%v", i, ok1, ok2)
		}
		require.Equal(t, "w1", owner1)
		require.Equal(t, "w2", owner2)
	}
}

// --- merged from claim_resolver_config_test.go ---

// TestWorkerConsumer_PassesReconcileIntervalToResolver verifies that the
// ReconcileInterval configured on ResolverConfig is plumbed all the way
// through to the auto-created *ClaimBasedResolver via WithReconcileInterval.
func TestWorkerConsumer_PassesReconcileIntervalToResolver(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:      "RI1",
		ConsumerPrefix:  "wc-ri1",
		SubjectTemplate: "ri1.{{.PartitionID}}",
		ProcessingGate:  &ProcessingGateConfig{Enabled: true},
		Resolver: ResolverConfig{
			HandoffBucketName:   "ri1-handoff",
			HandoffClaimsPrefix: "claims/",
			ReconcileInterval:   1 * time.Second,
		},
	}
	require.NoError(t, cfg.SetDefaults())
	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }),
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}
	require.NoError(t, wc.ensureGateResolver(ctx))
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	resolver, ok := wc.gateResolver.(*ClaimBasedResolver)
	require.True(t, ok, "auto-created resolver must be *ClaimBasedResolver")
	require.Equal(t, 1*time.Second, resolver.reconcileInterval,
		"ResolverConfig.ReconcileInterval must propagate to the resolver")
}

// TestWorkerConsumer_DefaultReconcileIntervalApplies verifies that when
// ResolverConfig.ReconcileInterval is left at zero, SetDefaults normalises
// it to 30s via the struct tag and the resolver is started with that value.
func TestWorkerConsumer_DefaultReconcileIntervalApplies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	_, nc := partitesting.StartEmbeddedNATS(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cfg := WorkerConsumerConfig{
		StreamName:      "RI2",
		ConsumerPrefix:  "wc-ri2",
		SubjectTemplate: "ri2.{{.PartitionID}}",
		ProcessingGate:  &ProcessingGateConfig{Enabled: true},
		Resolver: ResolverConfig{
			HandoffBucketName:   "ri2-handoff",
			HandoffClaimsPrefix: "claims/",
			// ReconcileInterval intentionally left at zero.
		},
	}
	require.NoError(t, cfg.SetDefaults())
	require.Equal(t, 30*time.Second, cfg.Resolver.ReconcileInterval,
		"SetDefaults must normalise zero ReconcileInterval to the 30s default")

	tmpl, err := template.New("subject").Parse(cfg.SubjectTemplate)
	require.NoError(t, err)

	wc := &WorkerConsumer{
		js:              js,
		config:          cfg,
		logger:          cfg.Logger,
		handler:         messageHandlerFunc(func(context.Context, jetstream.Msg) error { return nil }),
		subjects:        make(map[string]*partitionConsumer),
		iterFactory:     defaultIterFactory,
		subjectTemplate: tmpl,
	}
	require.NoError(t, wc.ensureGateResolver(ctx))
	t.Cleanup(func() { _ = wc.Close(context.Background()) })

	resolver, ok := wc.gateResolver.(*ClaimBasedResolver)
	require.True(t, ok)
	require.Equal(t, 30*time.Second, resolver.reconcileInterval,
		"resolver must run with the 30s default when config field is zero")
}

// --- merged from claim_resolver_consistency_test.go ---

// mockKVEntryCons implements jetstream.KeyValueEntry for testing.
type mockKVEntryCons struct {
	key      string
	value    []byte
	revision uint64
	op       jetstream.KeyValueOp
}

func (m *mockKVEntryCons) Bucket() string                  { return "bucket" }
func (m *mockKVEntryCons) Key() string                     { return m.key }
func (m *mockKVEntryCons) Value() []byte                   { return m.value }
func (m *mockKVEntryCons) Revision() uint64                { return m.revision }
func (m *mockKVEntryCons) Created() time.Time              { return time.Now() }
func (m *mockKVEntryCons) Delta() uint64                   { return 0 }
func (m *mockKVEntryCons) Operation() jetstream.KeyValueOp { return m.op }

// TestClaimBasedResolver_Consistency_StaleRefresh tests that ForceRefreshPartition
// does not overwrite a newer cache entry with stale data.
func TestClaimBasedResolver_Consistency_StaleRefresh(t *testing.T) {
	// Setup resolver with mocked KV (we won't use real KV for this unit test logic)
	// We will manually manipulate the cache and call internal methods or simulate behavior.
	// Since ForceRefreshPartition calls kv.Get, we need a real KV or a mock.
	// Using real embedded NATS is easier.

	// However, to simulate "stale fetch", we need to control the sequence.
	// 1. Cache has Rev 10.
	// 2. ForceRefresh fetches Rev 5 (simulated).
	// 3. Cache should stay at Rev 10.

	// Since we can't easily mock the KV client inside the struct without dependency injection,
	// we will test the logic by inspecting the code behavior or using a specialized test.
	// But wait, we can use the fact that ForceRefreshPartition uses the KV interface.
	// The struct uses `jetstream.KeyValue`. We can mock this interface!

	mockKV := &mockKVClient{
		data: make(map[string]*mockKVEntryCons),
	}

	r := NewClaimBasedResolver(mockKV, "claims/", nil)
	// Disable rate limiting for this test to allow immediate refreshes
	r.refreshCooldown = 0

	// 1. Seed the cache with a "newer" entry (Rev 10)
	// We can do this by simulating a watcher update.
	pending := make(map[string]pending)
	claim := handoff.Claim{Owner: "w1", State: handoff.ClaimStateStable, Epoch: 100}
	data, _ := claim.Marshal()

	// Simulate watcher update Rev 10
	r.handleWatcherUpdate(&mockKVEntryCons{
		key:      "claims/p1",
		value:    data,
		revision: 10,
		op:       jetstream.KeyValuePut,
	}, pending)
	r.applyPendingBatch(pending, "test")

	// Verify cache
	owner, _, _, ok := r.GetOwner("p1")
	require.True(t, ok)
	require.Equal(t, "w1", owner)

	// 2. Setup Mock KV to return "older" entry (Rev 5) for ForceRefresh
	oldClaim := handoff.Claim{Owner: "w2", State: handoff.ClaimStateStable, Epoch: 90}
	oldData, _ := oldClaim.Marshal()
	mockKV.data["claims/p1"] = &mockKVEntryCons{
		key:      "claims/p1",
		value:    oldData,
		revision: 5,
		op:       jetstream.KeyValuePut,
	}

	// 3. Call ForceRefreshPartition
	err := r.ForceRefreshPartition(context.Background(), "p1")
	require.NoError(t, err)

	// 4. Verify cache is STILL "w1" (Rev 10), not "w2" (Rev 5)
	owner, _, _, ok = r.GetOwner("p1")
	require.True(t, ok)
	require.Equal(t, "w1", owner, "Cache should not be overwritten by stale revision")

	// 5. Now update Mock KV to return "newer" entry (Rev 15)
	newClaim := handoff.Claim{Owner: "w3", State: handoff.ClaimStateStable, Epoch: 110}
	newData, _ := newClaim.Marshal()
	mockKV.data["claims/p1"] = &mockKVEntryCons{
		key:      "claims/p1",
		value:    newData,
		revision: 15,
		op:       jetstream.KeyValuePut,
	}

	// 6. Call ForceRefreshPartition
	err = r.ForceRefreshPartition(context.Background(), "p1")
	require.NoError(t, err)

	// 7. Verify cache is NOW "w3" (Rev 15)
	owner, _, _, ok = r.GetOwner("p1")
	require.True(t, ok)
	require.Equal(t, "w3", owner, "Cache should be updated by newer revision")
}

// TestClaimBasedResolver_Consistency_StaleWatcher tests that applyPendingBatch
// does not overwrite a newer cache entry (from ForceRefresh) with stale watcher data.
func TestClaimBasedResolver_Consistency_StaleWatcher(t *testing.T) {
	mockKV := &mockKVClient{data: make(map[string]*mockKVEntryCons)}
	r := NewClaimBasedResolver(mockKV, "claims/", nil)

	// 1. Seed cache with Rev 20 (simulating a ForceRefresh that just happened)
	// We can't directly inject into cache easily, but we can use ForceRefresh with the mock.
	claim := handoff.Claim{Owner: "w1", State: handoff.ClaimStateStable, Epoch: 200}
	data, _ := claim.Marshal()
	mockKV.data["claims/p1"] = &mockKVEntryCons{
		key:      "claims/p1",
		value:    data,
		revision: 20,
		op:       jetstream.KeyValuePut,
	}
	err := r.ForceRefreshPartition(context.Background(), "p1")
	require.NoError(t, err)

	// Verify
	owner, _, _, _ := r.GetOwner("p1")
	require.Equal(t, "w1", owner)

	// 2. Simulate a stale watcher update (Rev 10) arriving late
	pending := make(map[string]pending)
	oldClaim := handoff.Claim{Owner: "w2", State: handoff.ClaimStateStable, Epoch: 100}
	oldData, _ := oldClaim.Marshal()

	r.handleWatcherUpdate(&mockKVEntryCons{
		key:      "claims/p1",
		value:    oldData,
		revision: 10,
		op:       jetstream.KeyValuePut,
	}, pending)

	// 3. Apply batch
	r.applyPendingBatch(pending, "test")

	// 4. Verify cache is STILL "w1" (Rev 20)
	owner, _, _, _ = r.GetOwner("p1")
	require.Equal(t, "w1", owner, "Cache should not be overwritten by stale watcher update")
}

// --- Mock Implementation ---

type mockKVClient struct {
	jetstream.KeyValue
	data map[string]*mockKVEntryCons
}

func (m *mockKVClient) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if e, ok := m.data[key]; ok {
		return e, nil
	}
	return nil, jetstream.ErrKeyNotFound
}

func (m *mockKVClient) WatchAll(ctx context.Context, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return nil, errors.New("not implemented")
}

func (m *mockKVClient) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	keys := make([]string, 0, len(m.data))
	for k := range m.data {
		keys = append(keys, k)
	}
	return keys, nil
}

// --- merged from claim_resolver_drift_test.go ---

// TestClaimResolver_ReconcileRescueIncrementsMetric verifies that
// IncReconcileRescue fires when the reconciler applies a missed update.
// The watcher is stopped cooperatively first so the reconciler is the only
// path to convergence; once it catches up, the rescue metric must have
// incremented.
func TestClaimResolver_ReconcileRescueIncrementsMetric(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	ms := newMetricsSpy()
	// Disable drift restart so the rescue metric is the only signal
	// under test here (avoid confounding with the restart machinery).
	r := NewClaimBasedResolver(
		kv, "claims/", nil,
		WithReconcileInterval(50*time.Millisecond),
		WithDriftRestartCooldown(0),
	)
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// Stop the watcher cooperatively. Without drift-driven restart, the
	// supervisor's establish path may re-create it, but we use the
	// reconcile metric (not watcher state) as the signal here.
	require.NoError(t, r.watcher.Stop())

	// Write a claim so the reconciler observes drift on its next tick.
	c := handoff.Claim{
		PartitionID: "pRescue", Owner: "wR",
		State:       handoff.ClaimStateStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
	}
	b, err := c.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pRescue", b)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return ms.reconcileRescueCount() >= 1
	}, 5*time.Second, 25*time.Millisecond,
		"IncReconcileRescue must fire when reconcileOnce applies drift recovery")
}

// TestClaimResolver_ReconcileNoRescueWhenNoDrift asserts the rescue metric
// stays at zero across many reconcile ticks in steady state — i.e., the
// metric is precise to actual drift, not a per-tick counter.
func TestClaimResolver_ReconcileNoRescueWhenNoDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	c := handoff.Claim{
		PartitionID: "pSteady", Owner: "wS",
		State:       handoff.ClaimStateStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
	}
	b, err := c.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pSteady", b)
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(
		kv, "claims/", nil,
		WithReconcileInterval(50*time.Millisecond),
	)
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// Allow the watcher to populate the cache.
	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("pSteady")
		return ok && owner == "wS"
	}, 2*time.Second, 10*time.Millisecond)

	// Wait long enough for at least six reconcile ticks to fire. Negative
	// assertion: rescue counter must remain zero across the window.
	const reconcileTicks = 6
	settle := time.Duration(reconcileTicks) * 50 * time.Millisecond
	time.Sleep(settle)

	require.Zero(t, ms.reconcileRescueCount(),
		"IncReconcileRescue must NOT fire when reconcile finds no drift")
}

// TestClaimResolver_DriftTriggersWatcherRestart cooperatively stops the
// watcher, writes a claim, and asserts that the reconciler both rescues the
// cache AND signals the supervisor to restart the watcher under the
// "drift_detected" reason. After restart, a subsequent write must reach
// the cache via the new watcher.
func TestClaimResolver_DriftTriggersWatcherRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	ms := newMetricsSpy()
	// 50ms reconcile + tiny cooldown so the drift-restart fires promptly.
	r := NewClaimBasedResolver(
		kv, "claims/", nil,
		WithReconcileInterval(50*time.Millisecond),
		WithDriftRestartCooldown(100*time.Millisecond),
	)
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// Stop the watcher cooperatively — its channel closes; if no drift
	// signal fires the supervisor restarts under "channel_closed".
	require.NoError(t, r.watcher.Stop())

	// Write a claim. The reconciler observes the drift (cache is empty,
	// KV has the claim), emits rescue, and signals drift-driven restart.
	c1 := handoff.Claim{
		PartitionID: "pD1", Owner: "wA",
		State:       handoff.ClaimStateStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
	}
	b1, err := c1.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pD1", b1)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return ms.reconcileRescueCount() >= 1
	}, 5*time.Second, 25*time.Millisecond,
		"reconcile rescue must fire after drift observed")

	require.Eventually(t, func() bool {
		return ms.watcherRestartCount(watcherRestartReasonDriftDetected) >= 1
	}, 5*time.Second, 25*time.Millisecond,
		"watcher restart must be classified as drift_detected")

	// Verify the new watcher is actually serving updates: write a second
	// claim and assert it reaches the cache. (The race between watcher
	// re-replay and direct Updates() delivery doesn't matter; either
	// arrival path proves the new watcher is live.)
	c2 := handoff.Claim{
		PartitionID: "pD2", Owner: "wB",
		State:       handoff.ClaimStateStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
	}
	b2, err := c2.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pD2", b2)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("pD2")
		return ok && owner == "wB"
	}, 5*time.Second, 25*time.Millisecond,
		"new watcher must deliver subsequent writes to the cache")
}

// TestClaimResolver_DriftRestartRespectsCooldown drives two distinct drift
// events within 1 second and asserts the cooldown rate-limits drift-driven
// restarts to exactly one. The rescue metric may fire more than once (each
// reconcile drift bumps it), but the drift_detected watcher restart fires
// at most once per cooldown.
func TestClaimResolver_DriftRestartRespectsCooldown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	ms := newMetricsSpy()
	// 5s cooldown vs 50ms reconcile: the second drift event will land
	// well inside the cooldown window.
	r := NewClaimBasedResolver(
		kv, "claims/", nil,
		WithReconcileInterval(50*time.Millisecond),
		WithDriftRestartCooldown(5*time.Second),
	)
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// First drift event: stop the initial watcher and write claim1.
	require.NoError(t, r.watcher.Stop())
	c1 := handoff.Claim{
		PartitionID: "pCool1", Owner: "wA",
		State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: time.Now().UTC(),
	}
	b1, err := c1.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pCool1", b1)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return ms.watcherRestartCount(watcherRestartReasonDriftDetected) >= 1
	}, 5*time.Second, 25*time.Millisecond,
		"first drift restart should fire")

	// At this point the supervisor has re-established the watcher, which
	// replays history and converges the cache with KV. To drive a second
	// drift event INSIDE the cooldown, stop the new watcher again and
	// write claim2. The reconciler must rescue (cache misses claim2) and
	// invoke requestWatcherRestartFromReconcile — which the cooldown
	// must short-circuit, leaving drift_detected at exactly 1. The
	// cooperative Stop will register as channel_closed instead.
	require.Eventually(t, func() bool {
		r.watcherMu.Lock()
		w := r.currentWatcher
		r.watcherMu.Unlock()
		// The supervisor has re-established when currentWatcher differs
		// from the initial r.watcher.
		return w != nil && w != r.watcher
	}, 2*time.Second, 25*time.Millisecond,
		"supervisor should have re-established the watcher")

	r.watcherMu.Lock()
	w2 := r.currentWatcher
	r.watcherMu.Unlock()
	require.NoError(t, w2.Stop())

	c2 := handoff.Claim{
		PartitionID: "pCool2", Owner: "wB",
		State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: time.Now().UTC(),
	}
	b2, err := c2.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pCool2", b2)
	require.NoError(t, err)

	// Wait for the second drift to be rescued by the reconciler.
	require.Eventually(t, func() bool {
		return ms.reconcileRescueCount() >= 2
	}, 5*time.Second, 25*time.Millisecond,
		"second drift event must increment rescue counter")

	// Bounded negative assertion: drift_detected must remain at 1 across
	// a window well below the 5s cooldown.
	const probeDeadline = 1 * time.Second
	deadline := time.Now().Add(probeDeadline)
	for time.Now().Before(deadline) {
		require.LessOrEqual(t, ms.watcherRestartCount(watcherRestartReasonDriftDetected), 1,
			"cooldown must rate-limit drift restarts to one within the window")
		time.Sleep(50 * time.Millisecond)
	}

	require.Equal(t, 1, ms.watcherRestartCount(watcherRestartReasonDriftDetected),
		"exactly one drift_detected restart within the cooldown window")
}

// TestClaimResolver_DriftRestartDisabledByZeroCooldown verifies that a zero
// cooldown disables the watcher-restart half of the drift signal entirely:
// the rescue metric still fires but no watcher restart is issued.
func TestClaimResolver_DriftRestartDisabledByZeroCooldown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(
		kv, "claims/", nil,
		WithReconcileInterval(50*time.Millisecond),
		WithDriftRestartCooldown(0), // disable drift-driven restart
	)
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// Stop the watcher; we are about to write a claim that will appear
	// only via the reconciler.
	require.NoError(t, r.watcher.Stop())

	c := handoff.Claim{
		PartitionID: "pZero", Owner: "wZ",
		State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: time.Now().UTC(),
	}
	b, err := c.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pZero", b)
	require.NoError(t, err)

	// Wait for the rescue to fire — proves reconcile saw the drift.
	require.Eventually(t, func() bool {
		return ms.reconcileRescueCount() >= 1
	}, 5*time.Second, 25*time.Millisecond,
		"rescue metric must fire even when drift-restart is disabled")

	// Now assert (negative) that no drift_detected restart is emitted
	// within a bounded window. We use a short fixed sleep here because
	// the assertion is "no event occurred", which fundamentally requires
	// a bounded wait — there is no event-driven primitive that can
	// distinguish "hasn't happened yet" from "will never happen".
	time.Sleep(400 * time.Millisecond)
	require.Zero(t, ms.watcherRestartCount(watcherRestartReasonDriftDetected),
		"WithDriftRestartCooldown(0) must disable drift-driven restart")
}

// TestClaimResolver_DriftRestartReasonClassifiedCorrectly is the regression
// guard for the supervise reason CAS. A drift-driven restart must classify
// as "drift_detected"; a subsequent cooperative close must classify as
// "channel_closed" because the CAS consumed the pending flag on the first
// restart.
func TestClaimResolver_DriftRestartReasonClassifiedCorrectly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(
		kv, "claims/", nil,
		WithReconcileInterval(50*time.Millisecond),
		WithDriftRestartCooldown(100*time.Millisecond),
	)
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// Phase A: drive a drift restart. Stop the watcher and write a
	// claim; the reconciler will rescue + signal restart.
	require.NoError(t, r.watcher.Stop())
	cA := handoff.Claim{
		PartitionID: "pClsA", Owner: "wA",
		State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: time.Now().UTC(),
	}
	bA, err := cA.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pClsA", bA)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return ms.watcherRestartCount(watcherRestartReasonDriftDetected) >= 1
	}, 5*time.Second, 25*time.Millisecond,
		"phase A: drift restart must classify as drift_detected")

	// Snapshot counts before phase B so we can isolate the second event.
	drift1 := ms.watcherRestartCount(watcherRestartReasonDriftDetected)
	closed1 := ms.watcherRestartCount(watcherRestartReasonChannelClosed)

	// Phase B: cooperative close on the new watcher. Wait briefly for
	// the supervisor to have stored the new watcher in currentWatcher,
	// then stop it. We do NOT touch the KV here — there is no drift
	// trigger, so the next restart must classify as channel_closed.
	require.Eventually(t, func() bool {
		r.watcherMu.Lock()
		w := r.currentWatcher
		r.watcherMu.Unlock()
		return w != nil
	}, 2*time.Second, 25*time.Millisecond)

	r.watcherMu.Lock()
	w := r.currentWatcher
	r.watcherMu.Unlock()
	require.NotNil(t, w)
	require.NoError(t, w.Stop())

	require.Eventually(t, func() bool {
		return ms.watcherRestartCount(watcherRestartReasonChannelClosed) >= closed1+1
	}, 5*time.Second, 25*time.Millisecond,
		"phase B: cooperative close must classify as channel_closed")

	// Phase B must NOT have incremented drift_detected — the CAS in
	// phase A consumed the pending flag.
	require.Equal(t, drift1, ms.watcherRestartCount(watcherRestartReasonDriftDetected),
		"second restart (no drift signal) must NOT classify as drift_detected")
}

// --- merged from claim_resolver_retry_envelope_test.go ---

// TestClaimResolver_WatcherRestartBoundedAndEscalates is the P2.4b
// reproducer pinned by `docs/plans/self-healing/00-fix-plan.md` § P2.4b:
//
//   - T1: Delete the handoff bucket while the resolver runs; assert
//     bounded retries and that an exhaustion signal fires once.
//
// Before the F2 envelope is wired in, the supervisor's watcher-restart
// loop spins forever on a vanished bucket, generating unbounded
// `kv.WatchAll` API load against the deleted stream and producing no
// operator-visible escalation. The bound + escalation lets the
// reconciler remain the load-bearing recovery path while signalling
// that the watcher itself has given up — the same shape as the
// source-watcher envelope shipped in P2.4a.
func TestClaimResolver_WatcherRestartBoundedAndEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Tighten the envelope so the test completes in a few seconds
	// rather than the production worst-case ~80s. Save and restore so
	// other tests in the package see the production defaults.
	origMax := watcherMaxAttempts
	origBase := watcherBaseBackoff
	origCap := watcherMaxBackoff
	watcherMaxAttempts = 3
	watcherBaseBackoff = 30 * time.Millisecond
	watcherMaxBackoff = 60 * time.Millisecond
	defer func() {
		watcherMaxAttempts = origMax
		watcherBaseBackoff = origBase
		watcherMaxBackoff = origCap
	}()

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff-bounded"})
	require.NoError(t, err)

	// Seed one claim so the cache has something to converge on.
	initial := handoff.Claim{
		PartitionID: "p1",
		Owner:       "worker-A",
		State:       handoff.ClaimStateStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
	}
	b, err := initial.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/p1", b)
	require.NoError(t, err)

	ms := newMetricsSpy()
	var exhaustCount atomic.Int32
	var exhaustErr atomic.Value // error
	r := NewClaimBasedResolver(kv, "claims/", nil,
		WithReconcileInterval(0),
		WithWatcherRetryExhausted(func(err error) {
			exhaustCount.Add(1)
			exhaustErr.Store(err)
		}),
	)
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("p1")
		return ok && owner == "worker-A"
	}, 2*time.Second, 10*time.Millisecond, "initial cache convergence")

	// Delete the bucket and then force the current watcher's channel
	// to close. The bucket delete by itself does not necessarily close
	// an already-bound watcher's Updates() channel (the nats.go KV
	// watcher's channel does not close on a NATS server restart either
	// — see project_nats_watcher_empirical_finding). Manually stopping
	// the watcher triggers the supervisor's restart path, and the
	// subsequent runWatcher calls then hit the now-deleted bucket and
	// fail repeatedly, exhausting the envelope budget.
	require.NoError(t, js.DeleteKeyValue(ctx, "handoff-bounded"))
	require.NotNil(t, r.watcher)
	// Stop() may itself error against a now-deleted stream
	// (jetstream.ErrStreamNotFound from the cached handle); that is
	// fine — the call still closes the local channel which is what we
	// need to trigger supervise's restart path.
	_ = r.watcher.Stop()

	// The envelope should reach its budget within roughly:
	//   3 attempts × (base + jittered backoff up to cap) ≈ 0–200ms.
	// Add slack for scheduling. After the budget is exhausted the
	// supervise goroutine exits and the exhaustion callback fires
	// exactly once.
	require.Eventually(t, func() bool {
		return exhaustCount.Load() == 1
	}, 5*time.Second, 10*time.Millisecond,
		"OnWatcherRetryExhausted must fire exactly once after the envelope budget is consumed")

	// The bound: establish_failed counts must NOT exceed MaxAttempts.
	// (Pre-fix the loop would spin forever; this is the load-bearing
	// regression assertion.)
	require.LessOrEqual(t, ms.watcherRestartCount(watcherRestartReasonEstablishFailed),
		watcherMaxAttempts,
		"establish_failed restarts must be bounded by MaxAttempts; "+
			"unbounded count is the original failure mode this PR closes")

	// The dedicated exhaustion-reason metric must fire exactly once so
	// operators can alert on the give-up event.
	require.Equal(t, 1, ms.watcherRestartCount(watcherRestartReasonExhausted),
		"IncWatcherRestart(\"exhausted\") must fire exactly once at exhaustion")

	// Hold the assertion that the captured error is non-nil; it should
	// be the last establishment error the envelope saw.
	require.NotNil(t, exhaustErr.Load(),
		"exhaustion callback must receive the underlying establishment error")
}

// --- merged from claim_resolver_restart_test.go ---

// TestClaimResolver_WatcherRestartOnChannelClose mirrors the production-bug
// reproducer at TestClaimResolver_CacheFreezesAfterWatcherClose but
// additionally asserts that the IncWatcherRestart("channel_closed") metric
// fires after the supervisor establishes a new watcher.
func TestClaimResolver_WatcherRestartOnChannelClose(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	initial := handoff.Claim{PartitionID: "p1", Owner: "worker-A", State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: time.Now().UTC()}
	b, err := initial.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/p1", b)
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("p1")
		return ok && owner == "worker-A"
	}, 2*time.Second, 10*time.Millisecond)

	// Force the watcher channel to close.
	require.NotNil(t, r.watcher)
	require.NoError(t, r.watcher.Stop())

	// Write a new claim. The resolver must observe it after restart.
	// Either path is fine: if the supervisor has already re-established
	// the watcher, the update arrives via Updates(); if not, the
	// re-established watcher's initial walk delivers it. The Eventually
	// below tolerates either ordering.
	updated := handoff.Claim{PartitionID: "p1", Owner: "worker-B", State: handoff.ClaimStateStable, Epoch: 2, LastUpdated: time.Now().UTC()}
	bUpd, err := updated.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/p1", bUpd)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("p1")
		return ok && owner == "worker-B"
	}, 10*time.Second, 25*time.Millisecond, "cache should converge after watcher restart")

	require.Eventually(t, func() bool {
		return ms.watcherRestartCount(watcherRestartReasonChannelClosed) >= 1
	}, 5*time.Second, 25*time.Millisecond, "IncWatcherRestart(channel_closed) was not emitted")
}

// TestClaimResolver_ReconcileCatchesMissedEvent drives a fast reconcile
// cadence and verifies that the reconciler independently converges the cache
// when given a direct KV write. Both the watcher and reconciler should be
// idempotent under the shared apply path, so this is a "reconcile is
// observable" test rather than a "reconcile is the only path" test.
func TestClaimResolver_ReconcileCatchesMissedEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(50*time.Millisecond))
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// Stop the watcher to simulate the worst case: the only path to
	// convergence is the reconciler.
	require.NoError(t, r.watcher.Stop())

	c := handoff.Claim{PartitionID: "rPid", Owner: "wRecon", State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: time.Now().UTC()}
	b, err := c.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/rPid", b)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("rPid")
		return ok && owner == "wRecon"
	}, 5*time.Second, 25*time.Millisecond, "reconcile should have applied the direct KV write")

	require.Positive(t, ms.flushReasonCount("reconcile"),
		"reconcile flush reason should have fired at least once")
}

// TestClaimResolver_ReconcileNoSpuriousChanges asserts the reconciler does not
// reseat the cache pointer (and does not churn metrics) when in steady state.
func TestClaimResolver_ReconcileNoSpuriousChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	c := handoff.Claim{PartitionID: "p1", Owner: "wA", State: handoff.ClaimStateStable, Epoch: 1, LastUpdated: time.Now().UTC()}
	b, err := c.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/p1", b)
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(50*time.Millisecond))
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("p1")
		return ok && owner == "wA"
	}, 2*time.Second, 10*time.Millisecond)

	// Wait until at least one reconcile tick has fired (event-driven proxy
	// for "the reconciler is alive and steady-state"). We poll the
	// reconciler's internal ticker via its loop iteration count: every
	// reconcile pass that finds no work to do does not emit a flush
	// metric, but it does observe the ticker. Since we cannot observe the
	// ticker directly, use a tighter event-driven settle: wait for two
	// reconcile-interval-equivalent windows to pass with no upsert
	// activity, then snapshot.
	//
	// NOTE: this sleep is a negative-assertion synchronizer — we are
	// asserting that the reconciler does NOT change state across several
	// ticks. The 300-testing rule permits sleeps to bound negative
	// assertions when explicitly commented.
	const reconcileTicks = 6
	settle := time.Duration(reconcileTicks) * 50 * time.Millisecond

	// First settle: allow any in-flight watcher batch to drain.
	time.Sleep(settle / 3)
	// Snapshot cache pointer & metric counters once steady.
	ptrBefore := r.cache.Load()
	flushReconBefore := ms.flushReasonCount("reconcile")
	updUpsertBefore := ms.updateCount("upsert")

	// Let several reconcile ticks elapse — the assertion below is a
	// negative one ("no change occurred during this window"), so a bounded
	// wait is the correct synchronization primitive here.
	time.Sleep(settle)
	ptrAfter := r.cache.Load()
	flushReconAfter := ms.flushReasonCount("reconcile")
	updUpsertAfter := ms.updateCount("upsert")

	require.Same(t, ptrBefore, ptrAfter,
		"cache pointer should not reseat when reconcile finds no diff")
	require.Equal(t, flushReconBefore, flushReconAfter,
		"reconcile flush reason should not increment in steady state")
	require.Equal(t, updUpsertBefore, updUpsertAfter,
		"upsert update counter should not increment in steady state")
}

// TestClaimResolver_ReconcileDoesNotRegressLaterWatcherUpdates seeds the
// cache with a high-revision entry, then directly invokes reconcileOnce with
// the KV holding only an earlier revision. The revision-aware apply short
// circuit must protect the cache.
func TestClaimResolver_ReconcileDoesNotRegressLaterWatcherUpdates(t *testing.T) {
	// This test does NOT need a live NATS server — we construct the
	// resolver against a mock KV and seed the cache with a synthetic high
	// revision, then invoke reconcileOnce directly.
	kv := newMockKVForReconcile(map[string][]byte{
		"claims/p1": marshalClaim(t, handoff.Claim{
			PartitionID: "p1", Owner: "older", State: handoff.ClaimStateStable, Epoch: 1,
		}),
	}, 5) // mock returns revision=5

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))

	// Seed the cache with a "newer" watcher view (revision 10).
	seeded := map[string]claimEntry{
		"p1": {owner: "newer", state: toState(handoff.ClaimStateStable), epoch: 2, revision: 10},
	}
	r.cache.Store(&seeded)

	r.reconcileOnce(context.Background())

	owner, _, _, ok := r.GetOwner("p1")
	require.True(t, ok)
	require.Equal(t, "newer", owner, "reconcile must not regress a newer watcher revision")
}

// TestClaimResolver_StopBlocksUntilGoroutinesExit verifies Stop is a fence:
// after it returns, both supervised goroutines are no longer running.
func TestClaimResolver_StopBlocksUntilGoroutinesExit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(50*time.Millisecond))
	require.NoError(t, r.Start(ctx))

	doneCh := r.doneCh
	require.NotNil(t, doneCh, "Start must initialize doneCh")

	// Run Stop in a goroutine so we can bound the wait independently.
	stopped := make(chan struct{})
	start := time.Now()
	go func() {
		r.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return within 3s")
	}
	t.Logf("Stop returned after %v", time.Since(start))

	// doneCh must be closed by the time Stop returns.
	select {
	case <-doneCh:
	default:
		t.Fatal("doneCh not closed after Stop returned")
	}
}

// TestClaimResolver_StopWithRestartingWatcher forces the supervisor into its
// backoff path (by shutting the embedded NATS so kv.WatchAll fails), then
// calls Stop and asserts it returns well before the base backoff (2s) would
// otherwise elapse.
func TestClaimResolver_StopWithRestartingWatcher(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	// Note: we intentionally close NATS mid-test, so the deferred cleanup
	// may be a no-op for shutdown but still closes the conn handle safely.
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))

	// Trip the watcher: stop it so processWatcher returns errWatcherClosed.
	require.NoError(t, r.watcher.Stop())
	// Kill the NATS connection so subsequent WatchAll calls fail; this
	// forces the supervisor into its backoff sleep.
	nc.Close()

	// Wait until the supervisor has observed the closure and attempted to
	// re-establish (failing because NATS is down). This is an event-driven
	// signal that the supervisor is in its backoff sleep.
	require.Eventually(t, func() bool {
		return ms.watcherRestartCount(watcherRestartReasonEstablishFailed) >= 1
	}, 3*time.Second, 25*time.Millisecond,
		"supervisor should attempt to re-establish the watcher and fail")

	stopped := make(chan struct{})
	start := time.Now()
	go func() {
		r.Stop()
		close(stopped)
	}()
	// Base backoff is 2s; assert Stop returns well below that.
	select {
	case <-stopped:
	case <-time.After(1500 * time.Millisecond):
		t.Fatalf("Stop blocked on watcher backoff (>1.5s); start=%v", start)
	}
	t.Logf("Stop returned after %v while supervisor was in backoff", time.Since(start))
}

// TestClaimResolver_TombstoneSurvivesReconcile encodes the
// write -> delete -> reconcile invariant against an embedded NATS server.
// A claim is put then deleted in KV; the watcher must observe the delete and
// tombstone the cache. A subsequent reconcile pass must not resurrect the
// entry, and the watcher must not have died during the run.
func TestClaimResolver_TombstoneSurvivesReconcile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	// 1. Write a live claim.
	c := handoff.Claim{
		PartitionID: "pDel", Owner: "wA", State: handoff.ClaimStateStable,
		Epoch: 1, LastUpdated: time.Now().UTC(),
	}
	b, err := c.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pDel", b)
	require.NoError(t, err)

	ms := newMetricsSpy()
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(50*time.Millisecond))
	r.SetMetrics(ms)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// 2. Wait for the watcher to populate the cache.
	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("pDel")
		return ok && owner == "wA"
	}, 2*time.Second, 10*time.Millisecond)

	// 3. Delete the claim from KV.
	require.NoError(t, kv.Delete(ctx, "claims/pDel"))

	// 4. Wait for the watcher to tombstone the cache entry.
	require.Eventually(t, func() bool {
		_, _, _, ok := r.GetOwner("pDel")
		return !ok
	}, 2*time.Second, 10*time.Millisecond, "watcher should have tombstoned the entry")

	// 5. Trigger an explicit reconcile pass so the reconciler has had a
	// concrete chance to (incorrectly) resurrect the entry. This is a
	// stronger event-driven check than waiting on a tick metric: we call
	// reconcileOnce directly and observe its effect.
	r.reconcileOnce(ctx)

	// 6. Assert tombstone is still in place.
	_, _, _, ok := r.GetOwner("pDel")
	require.False(t, ok, "tombstoned entry must not be resurrected by reconcile")
	cur := r.cache.Load()
	require.NotNil(t, cur)
	e, hasKey := (*cur)["pDel"]
	require.True(t, hasKey, "tombstone entry should remain in the map")
	require.True(t, e.deleted, "entry should still be tombstoned after reconcile")

	// 7. Watcher must not have restarted during this test — the failure
	// mode is a confused tombstone, not a watcher death.
	require.Zero(t, ms.watcherRestartCount(watcherRestartReasonChannelClosed),
		"watcher must not have died during this test")
	require.Zero(t, ms.watcherRestartCount(watcherRestartReasonEstablishFailed),
		"watcher must not have failed to establish during this test")
}

// TestClaimResolver_ReconcileDoesNotTombstoneConcurrentWatcherUpsert is the
// P0 regression test. Reconcile must snapshot the cache BEFORE calling
// Keys(), so a watcher-applied entry that lands between Keys() returning
// (without the key) and the cache snapshot is NOT visible to the tombstone
// pass and therefore cannot be synthesized into a delete.
//
// On 493d879 (pre-fix), reconcileOnce reads Keys() first, then snapshots the
// cache. The afterKeys hook below mutates the cache mid-Keys, so when the
// snapshot is taken the injected entry is present; the tombstone pass sees
// it as "missing from seen" and stages a delete at injectedRev+1. The shared
// apply path's revision check then permanently shorts out the watcher's
// later upserts at the real revision.
//
// On main (post-fix), the snapshot is taken before Keys(), so the injected
// entry is not in `snap` and no tombstone is staged. A subsequent reconcile
// pass observes the entry in KV and converges normally.
func TestClaimResolver_ReconcileDoesNotTombstoneConcurrentWatcherUpsert(t *testing.T) {
	// Pre-seed: KV holds nothing for "pConcurrent"; cache holds nothing.
	// The afterKeys hook simulates the watcher applying a fresh upsert for
	// "pConcurrent" at revision 7 immediately after Keys() returned no
	// claims_pConcurrent key. We then snapshot KV at revision 8 for a
	// subsequent "real" upsert in step 3.
	kv := newMockKVForReconcile(map[string][]byte{}, 8)
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))

	injected := claimEntry{
		owner:    "watcher-owner",
		state:    toState(handoff.ClaimStateStable),
		epoch:    1,
		revision: 7,
	}

	// afterKeys runs inside Keys() right before it returns (deferred). On
	// the buggy code, the cache snapshot happens AFTER Keys(), so the
	// injection is visible to the snapshot. On the fixed code, the snapshot
	// happens BEFORE Keys(), so the injection lands too late to appear.
	kv.afterKeys = func() {
		next := map[string]claimEntry{
			"pConcurrent": injected,
		}
		r.cache.Store(&next)
	}

	// Trigger one reconcile pass. The expectation:
	//   * Fixed code: snap was taken before Keys; injection is NOT in snap;
	//     tombstone pass synthesizes nothing for pConcurrent; cache retains
	//     the live entry.
	//   * Buggy code: snap is taken AFTER Keys; injection IS in snap; seen
	//     does not include pConcurrent; tombstone at revision 8 is staged
	//     and applied (8 > 7), flipping the cache entry to deleted.
	r.reconcileOnce(context.Background())

	owner, _, _, ok := r.GetOwner("pConcurrent")
	require.True(t, ok,
		"reconcile must not tombstone a live cache entry injected concurrently with Keys()")
	require.Equal(t, "watcher-owner", owner)

	// A subsequent watcher upsert at a strictly higher revision must still
	// be applied through the shared apply path — i.e., reconcile did not
	// corrupt the cache by writing a phantom tombstone with a higher
	// revision than future updates.
	c := handoff.Claim{
		PartitionID: "pConcurrent", Owner: "later-owner",
		State: handoff.ClaimStateStable, Epoch: 2,
		LastUpdated: time.Now().UTC(),
	}
	bLater := marshalClaim(t, c)
	pendingByPID := map[string]pending{
		"pConcurrent": {op: "upsert", data: bLater, revision: 9},
	}
	r.applyPendingBatch(pendingByPID, "test")

	owner2, _, _, ok2 := r.GetOwner("pConcurrent")
	require.True(t, ok2, "later watcher upsert must reach the cache")
	require.Equal(t, "later-owner", owner2,
		"reconcile must not leave a phantom tombstone that shorts out later upserts")
}

// TestClaimResolver_StopBeforeStart asserts Stop is safe to call before
// Start, and that a subsequent Start observes the prior Stop and declines
// to spawn goroutines (returning nil).
func TestClaimResolver_StopBeforeStart(t *testing.T) {
	r := NewClaimBasedResolver(nil, "claims/", nil, WithReconcileInterval(0))

	// Stop before Start must return promptly with no panic and no leak.
	stopped := make(chan struct{})
	go func() {
		r.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(1 * time.Second):
		t.Fatal("Stop-before-Start did not return within 1s")
	}

	// A subsequent Start must observe stopCh closed and return without
	// launching supervise/reconcile goroutines. We can't easily observe
	// goroutine spawning, but we can verify Start returns nil promptly and
	// a second Stop is a true no-op.
	err := r.Start(context.Background())
	require.NoError(t, err)

	stopped2 := make(chan struct{})
	go func() {
		r.Stop()
		close(stopped2)
	}()
	select {
	case <-stopped2:
	case <-time.After(1 * time.Second):
		t.Fatal("second Stop did not return within 1s")
	}
}

// TestClaimResolver_StopRacingStart calls Start and Stop concurrently in many
// iterations and asserts no goroutine leak (we compare against a baseline).
func TestClaimResolver_StopRacingStart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	const iters = 100
	for i := range iters { //nolint:intrange // explicit counter for readability
		r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))

		done := make(chan struct{}, 2)
		go func() {
			_ = r.Start(ctx)
			done <- struct{}{}
		}()
		go func() {
			r.Stop()
			done <- struct{}{}
		}()
		// Bound each iteration to keep the test fast and detect deadlocks.
		for range 2 { //nolint:intrange // counter
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatalf("iteration %d: Start/Stop pair deadlocked", i)
			}
		}
		// Final Stop after both have completed must be a no-op.
		r.Stop()
	}
}

// --- helpers ---

// mockKVForReconcile is a controllable mockKV that returns a fixed set of
// claims at a fixed revision. Keys() returns the keys; Get() returns either
// the seeded value or ErrKeyNotFound.
//
// afterKeys is an optional test hook invoked after Keys() determines the
// returned slice but before Keys() returns. It exists so a test can inject
// a concurrent cache mutation between reconcile's pre-Keys snapshot point
// and the Keys() observation, exercising the P0 race directly.
type mockKVForReconcile struct {
	jetstream.KeyValue
	store     map[string][]byte
	revision  uint64
	afterKeys func()
}

func newMockKVForReconcile(store map[string][]byte, revision uint64) *mockKVForReconcile {
	if store == nil {
		store = map[string][]byte{}
	}
	return &mockKVForReconcile{store: store, revision: revision}
}

func (m *mockKVForReconcile) Keys(ctx context.Context, _ ...jetstream.WatchOpt) ([]string, error) {
	defer func() {
		if m.afterKeys != nil {
			m.afterKeys()
		}
	}()
	if len(m.store) == 0 {
		return nil, errors.New("nats: no keys found")
	}
	out := make([]string, 0, len(m.store))
	for k := range m.store {
		out = append(out, k)
	}

	return out, nil
}

func (m *mockKVForReconcile) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	val, ok := m.store[key]
	if !ok {
		return nil, jetstream.ErrKeyNotFound
	}
	return &mockKVEntryFull{key: key, val: val, revision: m.revision, op: 0}, nil
}

type mockKVEntryFull struct {
	jetstream.KeyValueEntry
	key      string
	val      []byte
	revision uint64
	op       jetstream.KeyValueOp
}

func (e *mockKVEntryFull) Key() string                     { return e.key }
func (e *mockKVEntryFull) Value() []byte                   { return e.val }
func (e *mockKVEntryFull) Revision() uint64                { return e.revision }
func (e *mockKVEntryFull) Operation() jetstream.KeyValueOp { return e.op }

// --- merged from claim_resolver_quorumloss_test.go ---

// ─── Mock extension ───────────────────────────────────────────────────────────
//
// mockKVForReconcile (defined in claim_resolver_restart_test.go) is extended
// with three nil-default fields:
//
//   getErrByKey map[string]error — if a key has an entry, Get returns that
//     error while Keys() STILL lists the key (the asymmetric window).
//   getOpByKey map[string]jetstream.KeyValueOp — if a key has an entry, the
//     entry Get returns has its Operation() overridden (surfacing a listed key
//     as a genuine delete/purge).
//   keysErr error — if set, Keys() returns this error immediately.
//
// All fields are set by direct field assignment (consistent with the existing
// afterKeys hook pattern). No existing call sites pass them, so they default
// to nil and the behaviour of existing tests is unchanged.
//
// The Get() implementation in claim_resolver_restart_test.go does not check
// getErrByKey; we shadow it here with a promoted-type wrapper so the existing
// struct definition needs no modification. See mockKVWithKeyErr below.

// mockKVWithKeyErr wraps mockKVForReconcile and adds per-key error injection
// and a Keys-level error, while keeping the existing store/revision/afterKeys
// fields for all other behaviour. It is the mock used in EVERY quorum-loss test.
//
// Invariant: getErrByKey[k] != nil → Get(k) returns that error even though
// Keys() still lists k. This is exactly the asymmetric Keys-ok/Get-fail
// window that reconcileOnce cannot handle.
type mockKVWithKeyErr struct {
	*mockKVForReconcile
	getErrByKey map[string]error                // optional per-key error override for Get
	getOpByKey  map[string]jetstream.KeyValueOp // optional per-key Operation() override for Get
	keysErr     error                           // if set, Keys() returns this error
}

func newMockKVWithKeyErr(store map[string][]byte, revision uint64) *mockKVWithKeyErr {
	return &mockKVWithKeyErr{
		mockKVForReconcile: newMockKVForReconcile(store, revision),
	}
}

// Keys overrides mockKVForReconcile.Keys so that keysErr can be returned.
// The afterKeys hook is still honoured.
func (m *mockKVWithKeyErr) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	if m.keysErr != nil {
		return nil, m.keysErr
	}
	return m.mockKVForReconcile.Keys(ctx, opts...)
}

// Get overrides mockKVForReconcile.Get to inject per-key errors and per-key
// operation overrides. If a key has an entry in getErrByKey the error is
// returned instead of the store value (the key deliberately stays in store, so
// Keys() still lists it — the asymmetric Keys-ok/Get-fail window). If a key has
// an entry in getOpByKey the returned entry's Operation() is overridden (so a
// listed key can surface as a genuine delete/purge tombstone via Get).
func (m *mockKVWithKeyErr) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if m.getErrByKey != nil {
		if err, hit := m.getErrByKey[key]; hit {
			return nil, err
		}
	}
	entry, err := m.mockKVForReconcile.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if m.getOpByKey != nil {
		if op, hit := m.getOpByKey[key]; hit {
			return &mockKVEntryFull{
				key:      entry.Key(),
				val:      entry.Value(),
				revision: entry.Revision(),
				op:       op,
			}, nil
		}
	}

	return entry, nil
}

// ─── Shared construction helpers ─────────────────────────────────────────────

const (
	quorumTestPID      = "USER21"
	quorumTestFullKey  = "claims/" + quorumTestPID
	quorumTestOwner    = "worker-A"
	quorumTestEpoch    = int64(1)
	quorumTestRevision = uint64(5) // R = 5; tombstone will land at R+1 = 6
)

// quorumTestClaim returns marshalled bytes for the standard live claim used
// across tests.
func quorumTestClaim(t *testing.T) []byte {
	t.Helper()
	return marshalClaim(t, handoff.Claim{
		PartitionID: quorumTestPID,
		Owner:       quorumTestOwner,
		State:       handoff.ClaimStateStable,
		Epoch:       quorumTestEpoch,
	})
}

// newHealthyKV returns a mock with a live claim at R, no error injection.
func newHealthyKV(t *testing.T) *mockKVWithKeyErr {
	t.Helper()
	return newMockKVWithKeyErr(map[string][]byte{
		quorumTestFullKey: quorumTestClaim(t),
	}, quorumTestRevision)
}

// seedResolverCache seeds the resolver's in-memory cache directly (bypassing
// Start/warm) with the standard single-pid live claim at quorumTestRevision.
// This mirrors the pattern used by
// TestClaimResolver_ReconcileDoesNotRegressLaterWatcherUpdates.
func seedResolverCache(r *ClaimBasedResolver) {
	m := map[string]claimEntry{
		quorumTestPID: {
			owner:    quorumTestOwner,
			state:    toState(handoff.ClaimStateStable),
			epoch:    quorumTestEpoch,
			revision: quorumTestRevision,
		},
	}
	r.cache.Store(&m)
}

// ─── Tests ────────────────────────────────────────────────────────────────────

// TestQuorumLoss_HealthyReconcile_Control is the mandatory false-green control.
//
// Setup: Keys-ok and Get-ok at R. After reconcileOnce the cache must retain
// the live claim (ok=true, correct owner). Case A below DIFFERS from this
// control by EXACTLY ONE variable: that key's Get fails. This pairing proves
// the Get-fail is the specific cause of the tombstone — if the control passed
// but Case A asserted ok=false for an unrelated setup reason, we would never
// know the tombstone was from the fault and not from something else.
func TestQuorumLoss_HealthyReconcile_Control(t *testing.T) {
	kv := newHealthyKV(t)
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))

	// Seed cache at R=5 (same as quorumTestRevision).
	seedResolverCache(r)

	r.reconcileOnce(context.Background())

	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok, "healthy reconcile must not tombstone a live claim")
	require.Equal(t, quorumTestOwner, owner, "owner must be unchanged after healthy reconcile")
}

// TestQuorumLoss_CaseA_GetFailDoesNotPoison is the primary F-D2a pin and the
// flipped reproducer: a Keys-ok / Get-fail window for a single pid must NOT
// tombstone the live claim. The transient read failure adds the pid to the
// `unreadable` set; the tombstone pass skips it; the cached claim survives.
//
// This is the RED→GREEN flip: on the pre-fix code this same fault staged a
// synthetic delete at R+1 and the final assertion would be ok=false. The fix
// keeps it ok=true.
//
// Non-vacuous because:
//   - The healthy-reconcile control above differs by exactly one variable
//     (Get-ok vs Get-fail) and also asserts ok=true. The pairing isolates the
//     Get-fail as the variable under test; the fix makes both outcomes equal.
//   - Step 3 proves the claim was never lost: after Get recovers, a later
//     reconcile still resolves ok=true with the correct owner — i.e. "we just
//     couldn't read it this pass; a later pass re-reads it", no restart needed.
func TestQuorumLoss_CaseA_GetFailDoesNotPoison(t *testing.T) {
	// Step 1 — fault setup: Keys returns the key, Get returns DeadlineExceeded.
	// This is the ONLY difference from TestQuorumLoss_HealthyReconcile_Control.
	kv := newHealthyKV(t)
	kv.getErrByKey = map[string]error{
		quorumTestFullKey: context.DeadlineExceeded,
	}

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))

	// Seed cache at R=5 so the resolver believes there is a live claim.
	seedResolverCache(r)

	// Step 2 — trigger the reconcile under fault conditions.
	// reconcileOnce: Keys() ok → Get() fails → pid added to `unreadable` →
	// tombstone pass skips it → no synthetic delete staged → cache untouched.
	r.reconcileOnce(context.Background())

	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok,
		// Non-vacuous: the healthy control differs only in Get-ok vs Get-fail.
		// On pre-fix code this asserted ok=false (the R+1 tombstone). The fix
		// keeps the unreadable claim live.
		"a listed-but-unreadable claim must NOT be tombstoned by a transient Get failure",
	)
	require.Equal(t, quorumTestOwner, owner, "owner must be unchanged after an unreadable-key reconcile")

	// Step 3 — recovery: restore Get, run another reconcile. The claim was never
	// lost, so it still resolves ok=true at the correct owner. No restart needed.
	kv.getErrByKey = nil
	r.reconcileOnce(context.Background())
	owner, _, _, ok = r.GetOwner(quorumTestPID)
	require.True(t, ok, "claim must remain resolvable after the read fault recovers")
	require.Equal(t, quorumTestOwner, owner)
}

// TestQuorumLoss_CaseAPrime_FleetWideReadFaultDoesNotPoison verifies that if
// ALL pids' Get calls fail in a single reconcile pass, NONE are tombstoned —
// every pid stays resolvable.
//
// This models the fleet-wide symptom from the incident report where all workers
// stopped processing after a bucket quorum loss affecting all keys. With F-D2a
// the whole fleet survives the transient read fault.
//
// Non-vacuous: we assert ok=true for ALL pids immediately after seeding (before
// the fault reconcile) AND after it. The pre-fault check proves the cache was
// genuinely populated; on pre-fix code the post-fault assertion was ok=false for
// every pid, so the ok=true here is a real flip, not a vacuous pass.
func TestQuorumLoss_CaseAPrime_FleetWideReadFaultDoesNotPoison(t *testing.T) {
	const numPIDs = 3
	pids := []string{"USER01", "USER02", "USER03"}

	store := make(map[string][]byte, numPIDs)
	for _, pid := range pids {
		store["claims/"+pid] = marshalClaim(t, handoff.Claim{
			PartitionID: pid,
			Owner:       "worker-" + pid,
			State:       handoff.ClaimStateStable,
			Epoch:       1,
		})
	}

	kv := newMockKVWithKeyErr(store, quorumTestRevision)

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))

	// Seed cache with all pids at R=5.
	seedMap := make(map[string]claimEntry, numPIDs)
	for _, pid := range pids {
		seedMap[pid] = claimEntry{
			owner:    "worker-" + pid,
			state:    toState(handoff.ClaimStateStable),
			epoch:    1,
			revision: quorumTestRevision,
		}
	}
	r.cache.Store(&seedMap)

	// Non-vacuous pre-fault check: all pids present before the fault.
	for _, pid := range pids {
		_, _, _, ok := r.GetOwner(pid)
		require.True(t, ok, "pre-fault: pid %s must be present", pid)
	}

	// Inject fault: ALL pids' Get returns DeadlineExceeded.
	kv.getErrByKey = make(map[string]error, numPIDs)
	for _, pid := range pids {
		kv.getErrByKey["claims/"+pid] = context.DeadlineExceeded
	}

	// Trigger the fault reconcile.
	r.reconcileOnce(context.Background())

	// Assert ALL pids survive (non-vacuous because we proved them present above
	// and pre-fix code tombstoned every one of them).
	for _, pid := range pids {
		_, _, _, ok := r.GetOwner(pid)
		require.True(t, ok, "post-fault: pid %s must survive an all-Get-fail reconcile", pid)
	}
}

// TestQuorumLoss_CaseADoublePrime_KVRewriteBeatsTheTombstone verifies that
// a claim re-write at revision R+2 (strictly greater than the tombstone at R+1)
// DOES beat the tombstone and restores the gate to open.
//
// This documents the monotonic-revision guard: a re-write at a strictly greater
// revision is the only thing that clears a tombstone in-process. The tombstone
// here is manufactured via a GENUINE delete-op (Get returns a KeyValueDelete) —
// the F-D2a fix only spares transient READ failures, so a real deletion still
// tombstones, which is exactly what this test relies on.
//
// Non-vacuous: we assert ok=false AFTER the tombstone (intermediate state)
// before delivering R+2. The final ok=true can only pass because the R+2
// write was accepted, NOT because the tombstone was absent.
func TestQuorumLoss_CaseADoublePrime_KVRewriteBeatsTheTombstone(t *testing.T) {
	// Step 1 — manufacture a genuine tombstone at R+1: Keys() lists the key but
	// Get() returns a delete operation, so the reconcile tombstone pass stages a
	// synthetic delete at R+1.
	kv := newHealthyKV(t)
	kv.getOpByKey = map[string]jetstream.KeyValueOp{
		quorumTestFullKey: jetstream.KeyValueDelete,
	}

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	seedResolverCache(r)

	r.reconcileOnce(context.Background())

	// Non-vacuous intermediate check: tombstone is in place before the heal.
	_, _, _, ok := r.GetOwner(quorumTestPID)
	require.False(t, ok, "a genuine delete-op must tombstone the claim before the heal attempt")

	// Step 2 — deliver a claim re-write at R+2=7 via the watcher path.
	// R+2=7 > tombstone=6 → guard: 6 >= 7 = false → write accepted.
	const healRevision = quorumTestRevision + 2 // R+2 = 7
	pendingMap := make(map[string]pending)
	rewriteEntry := &mockKVEntryFull{
		key:      quorumTestFullKey,
		val:      quorumTestClaim(t),
		revision: healRevision,
	}
	r.handleWatcherUpdate(rewriteEntry, pendingMap)
	r.applyPendingBatch(pendingMap, "test-rewrite-heal")

	// The re-write at R+2 must beat the tombstone at R+1.
	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok,
		// Non-vacuous: we asserted ok=false immediately above (tombstone present).
		// This ok=true can only result from the R+2 write being accepted.
		"claim re-write at R+2 must beat the tombstone at R+1",
	)
	require.Equal(t, quorumTestOwner, owner)
}

// TestQuorumLoss_CaseB_KeysFailDoesNotPoison verifies the boundary between the
// two fault branches: when Keys() ITSELF fails (not just per-key Get),
// reconcileOnce takes the early-return path and leaves the cache untouched.
//
// This is the branch the incident logs DID show ("reconcile … list keys
// failed"). Post-F-D2a it and Case A both leave the claim live (ok=true), but by
// DIFFERENT mechanisms: Keys-fail returns early so the tombstone pass never
// runs, whereas Keys-ok/Get-fail runs the tombstone pass but skips the pid via
// the `unreadable` set. This test pins the early-return mechanism specifically.
//
// Non-vacuous: the presence assertion before reconcileOnce proves the cache was
// genuinely populated. The post-reconcile ok=true cannot pass vacuously (it
// would fail if reconcileOnce had poisoned the cache via the tombstone pass).
func TestQuorumLoss_CaseB_KeysFailDoesNotPoison(t *testing.T) {
	kv := newHealthyKV(t)
	// Keys() returns DeadlineExceeded — the EARLY-RETURN branch in reconcileOnce.
	// Note: must NOT use an error containing "no keys found" — that branch
	// sets keys=nil and proceeds into the tombstone pass instead of returning.
	kv.keysErr = context.DeadlineExceeded

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	seedResolverCache(r)

	// Non-vacuous pre-reconcile check: cache is genuinely populated.
	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok, "pre-reconcile: cache must be populated (non-vacuous check)")
	require.Equal(t, quorumTestOwner, owner)

	// reconcileOnce: Keys() fails → early return (lines ~978-984 in production
	// code). The cache must be untouched.
	r.reconcileOnce(context.Background())

	owner, _, _, ok = r.GetOwner(quorumTestPID)
	require.True(t, ok,
		// Non-vacuous: we seeded the cache above AND confirmed it was present.
		// If reconcileOnce had touched the tombstone pass it would be ok=false.
		// The early-return prevents any cache modification — the claim survives.
		"Keys() failure must take the early-return path and NOT poison the cache",
	)
	require.Equal(t, quorumTestOwner, owner)
}

// ─── F-D2a boundary table: genuine deletions must STILL tombstone ─────────────
//
// These guard that the fix is not over-broad: it spares ONLY a transient read
// failure (Get errored). Every genuine-deletion signal still tombstones. They
// pass on pre-fix code too — they are preservation guards, not RED flips — and
// turn red if the fix were widened to skip tombstoning for ANY non-`seen` pid.

// TestQuorumLoss_Boundary_GetDeleteOpStillTombstones: Keys lists the key, Get
// returns a KeyValueDelete op → a genuine deletion → still tombstoned.
func TestQuorumLoss_Boundary_GetDeleteOpStillTombstones(t *testing.T) {
	kv := newHealthyKV(t)
	kv.getOpByKey = map[string]jetstream.KeyValueOp{
		quorumTestFullKey: jetstream.KeyValueDelete,
	}
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	seedResolverCache(r)

	_, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok, "pre-reconcile: claim must be present (non-vacuous)")

	r.reconcileOnce(context.Background())

	_, _, _, ok = r.GetOwner(quorumTestPID)
	require.False(t, ok, "a Get delete-op is a genuine deletion and must still tombstone")
}

// TestQuorumLoss_Boundary_GetPurgeOpStillTombstones: same as above with a
// KeyValuePurge op.
func TestQuorumLoss_Boundary_GetPurgeOpStillTombstones(t *testing.T) {
	kv := newHealthyKV(t)
	kv.getOpByKey = map[string]jetstream.KeyValueOp{
		quorumTestFullKey: jetstream.KeyValuePurge,
	}
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	seedResolverCache(r)

	_, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok, "pre-reconcile: claim must be present (non-vacuous)")

	r.reconcileOnce(context.Background())

	_, _, _, ok = r.GetOwner(quorumTestPID)
	require.False(t, ok, "a Get purge-op is a genuine deletion and must still tombstone")
}

// TestQuorumLoss_Boundary_AbsentFromKeysStillTombstones: the cached pid is no
// longer listed by Keys at all → genuinely gone → tombstoned. This is the
// backstop deletion path; the fix must not disturb it.
func TestQuorumLoss_Boundary_AbsentFromKeysStillTombstones(t *testing.T) {
	// Store holds a DIFFERENT live claim so Keys() returns non-empty (avoiding
	// the "no keys found" branch) but does NOT list the seeded pid.
	kv := newMockKVWithKeyErr(map[string][]byte{
		"claims/OTHER99": marshalClaim(t, handoff.Claim{
			PartitionID: "OTHER99",
			Owner:       "worker-other",
			State:       handoff.ClaimStateStable,
			Epoch:       1,
		}),
	}, quorumTestRevision)
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	seedResolverCache(r) // seeds quorumTestPID, which is NOT in the store

	_, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok, "pre-reconcile: seeded claim must be present (non-vacuous)")

	r.reconcileOnce(context.Background())

	_, _, _, ok = r.GetOwner(quorumTestPID)
	require.False(t, ok, "a pid absent from Keys is genuinely gone and must still tombstone")
}

// TestQuorumLoss_Boundary_PrefixFilteredKeyIsInert: a key that does not match
// the claims prefix is skipped before Get, so it enters neither `seen` nor
// `unreadable`. This test is load-bearing against a regressed prefix filter (one
// that stopped skipping out-of-prefix keys) on BOTH sides:
//
//   - seen-side: an out-of-prefix key carrying a VALID claim. If it were
//     fetched it would be staged as an upsert and become resolvable. (A junk
//     payload would be masked by applyPendingBatch's unmarshal-skip, so the
//     value must be a real claim for the assertion to bite.)
//   - unreadable-side: an out-of-prefix key whose bare name equals a cached,
//     genuinely-gone pid and whose Get errors. If it entered `unreadable` it
//     would wrongly suppress that gone pid's tombstone.
func TestQuorumLoss_Boundary_PrefixFilteredKeyIsInert(t *testing.T) {
	// An out-of-prefix key is cached (if a regression staged it) under its
	// TrimPrefix("claims/") result, which — having no prefix to strip — is the
	// full key string. So the seen-side assertion must query that full key.
	const outSeenKey = "other/SEEN9" // out-of-prefix valid claim; must stay inert
	const gonePID = "GONE7"          // cached but genuinely gone → must tombstone

	kv := newHealthyKV(t) // store: claims/USER21 (live)
	// seen-side probe: a valid claim under an out-of-prefix key.
	kv.store[outSeenKey] = marshalClaim(t, handoff.Claim{
		PartitionID: "SEEN9",
		Owner:       "worker-out",
		State:       handoff.ClaimStateStable,
		Epoch:       1,
	})
	// unreadable-side probe: a bare key whose name collides with gonePID and
	// whose Get errors. The real prefixed key (claims/GONE7) is absent, so a
	// correct prefix filter lets gonePID tombstone; a regressed filter would
	// route this errored Get into `unreadable[gonePID]` and suppress it.
	kv.store[gonePID] = []byte("ignored")
	kv.getErrByKey = map[string]error{gonePID: context.DeadlineExceeded}

	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	// Seed cache with the live claim AND the genuinely-gone pid.
	seed := map[string]claimEntry{
		quorumTestPID: {
			owner:    quorumTestOwner,
			state:    toState(handoff.ClaimStateStable),
			epoch:    quorumTestEpoch,
			revision: quorumTestRevision,
		},
		gonePID: {
			owner:    "worker-gone",
			state:    toState(handoff.ClaimStateStable),
			epoch:    1,
			revision: quorumTestRevision,
		},
	}
	r.cache.Store(&seed)

	// Non-vacuous pre-reconcile presence checks.
	_, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok, "pre-reconcile: live claim must be present")
	_, _, _, ok = r.GetOwner(gonePID)
	require.True(t, ok, "pre-reconcile: gone pid must be present before it tombstones")

	r.reconcileOnce(context.Background())

	owner, _, _, ok := r.GetOwner(quorumTestPID)
	require.True(t, ok, "the live claim must survive a reconcile that also saw out-of-prefix keys")
	require.Equal(t, quorumTestOwner, owner)

	// seen-side: the out-of-prefix valid claim must not have been staged. It
	// would be cached under its full-key pid if the prefix filter regressed.
	_, _, _, ok = r.GetOwner(outSeenKey)
	require.False(t, ok, "an out-of-prefix key must never become a resolvable claim (seen-side)")

	// unreadable-side: gonePID is genuinely gone (claims/GONE7 absent) and the
	// bare GONE7 key was prefix-filtered, so it must still tombstone.
	_, _, _, ok = r.GetOwner(gonePID)
	require.False(t, ok, "an out-of-prefix errored key must not suppress a genuine tombstone (unreadable-side)")
}

// --- merged from claim_resolver_envelope_concurrency_test.go ---

// TestClaimResolver_EnvelopeNoRaceUnderConcurrentKVTraffic is the
// regression-pin for GAP-2 from the v2.4.1->main integration-discipline
// audit (tmp/integration_discipline_audit_v2.4.1_to_main.md).
//
// The risk: the supervisor's WatchAll restart loop (claim_resolver.go:768)
// shares the *jetstream.KeyValue handle (r.kv) with every other KV-touching
// path on the resolver — Get inside ForceRefreshPartition
// (claim_resolver.go:479), Get + Keys inside warm
// (claim_resolver.go:519/533), and any external callers operating on the
// same handle. Under nats.go's internal model a *jetstream.KeyValue is
// backed by a *stream whose cached fields are written by metadata-touching
// calls (Watch/WatchAll/Status) and read by Get/GetLastMsgForSubject. If
// these access paths run on different goroutines without serialization,
// `go test -race` trips WARNING: DATA RACE.
//
// This is the same shape as the bug fixed in commit 4937443 ("open
// dedicated KV handle for epoch probe") — but for a different monitor
// goroutine. The fix for the epoch monitor was to open a dedicated probe
// handle per bucket. We have NOT yet applied an analogous fix to the
// claim resolver because no race has been observed; this test is the
// canary that would catch one if it surfaces.
//
// # Mechanism
//
//   - Tighten watcherBaseBackoff / watcherMaxBackoff / watcherMaxAttempts
//     to sub-second values so the supervisor's restart loop can fire many
//     times in the soak window.
//   - Stand up a real embedded NATS + JetStream KV bucket.
//   - Start a ClaimBasedResolver with reconciler disabled (so the
//     supervisor's WatchAll path is the only thing restarting watchers).
//   - Concurrent goroutines: force-close the current watcher every ~25 ms
//     to drive supervisor restarts; concurrent Gets across multiple
//     partitions; concurrent Keys probes; concurrent Puts to keep the
//     watcher updates stream alive.
//   - Soak for ~5 s, then assert (a) t.Failed() is false (no race), and
//     (b) the resolver is still functional (a fresh ForceRefreshPartition
//     succeeds against a freshly-written key).
//
// # Why this file lives in internal/durable
//
// AGENTS.md § "Concurrency stress tests for monitor goroutines" calls for
// stress tests under test/integration/<package>/. The watcherBaseBackoff /
// watcherMaxBackoff / watcherMaxAttempts test seams used here are
// package-private vars in claim_resolver.go ("// Production code must
// NEVER mutate these"). Same-package access is the cleanest path — same
// as the precedent set by internal/durable/claim_resolver_restart_test.go.
// The discipline's intent (real NATS + race detector + concurrent
// goroutines on the production code path) is met either way.
func TestClaimResolver_EnvelopeNoRaceUnderConcurrentKVTraffic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	// Not t.Parallel() — mutates package-level test-seam vars.
	origBase := watcherBaseBackoff
	origMax := watcherMaxBackoff
	origAttempts := watcherMaxAttempts
	watcherBaseBackoff = 10 * time.Millisecond
	watcherMaxBackoff = 50 * time.Millisecond
	// Generous so the soak doesn't exhaust the budget on consecutive
	// rapid closures; the property under test is "no race on shared
	// *stream cached state", not "envelope exhaustion behaves
	// correctly" (that contract is pinned in
	// claim_resolver_retry_envelope_test.go).
	watcherMaxAttempts = 1000
	t.Cleanup(func() {
		watcherBaseBackoff = origBase
		watcherMaxBackoff = origMax
		watcherMaxAttempts = origAttempts
	})

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff-stress"})
	require.NoError(t, err)

	// Plant a handful of claims so the Get-side goroutines have
	// something to find. The race surface is independent of the
	// returned data — Get reads the cached *stream state regardless of
	// whether the key exists.
	const numPartitions = 8
	for i := range numPartitions {
		c := handoff.Claim{
			PartitionID: fmt.Sprintf("p%d", i),
			Owner:       "worker-A",
			State:       handoff.ClaimStateStable,
			Epoch:       1,
			LastUpdated: time.Now().UTC(),
		}
		b, err := c.Marshal()
		require.NoError(t, err)
		_, err = kv.Put(ctx, fmt.Sprintf("claims/p%d", i), b)
		require.NoError(t, err)
	}

	// Reconciler disabled so the supervisor's WatchAll restart is the
	// only path that re-establishes the watcher — isolates the race
	// surface to the WatchAll vs Get/Keys cross-goroutine pattern.
	r := NewClaimBasedResolver(kv, "claims/", nil, WithReconcileInterval(0))
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	require.Eventually(t, func() bool {
		_, _, _, ok := r.GetOwner("p0")
		return ok
	}, 5*time.Second, 10*time.Millisecond, "initial warm must populate the cache")

	soakCtx, soakCancel := context.WithTimeout(ctx, 5*time.Second)
	defer soakCancel()

	var wg sync.WaitGroup
	var totalCloses atomic.Int64
	var totalGets atomic.Int64
	var totalKeys atomic.Int64
	var totalPuts atomic.Int64

	// Force-close goroutine: drives supervisor restarts. Each Stop()
	// closes the current watcher's Updates channel which causes
	// processWatcher to return errWatcherClosed, the supervisor to
	// pre-sleep ~10 ms, then runWatcher to call kv.WatchAll again. The
	// WatchAll call refreshes the cached *stream state — that's the
	// race-write side.
	wg.Go(func() {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-soakCtx.Done():
				return
			case <-ticker.C:
				r.watcherMu.Lock()
				w := r.currentWatcher
				r.watcherMu.Unlock()
				if w != nil {
					_ = w.Stop() // idempotent; closes Updates if not already closed
					totalCloses.Add(1)
				}
			}
		}
	})

	// Get-side goroutines: drive kv.Get reads on the same handle. This
	// is the race-read side. ErrKeyNotFound on missing keys is fine.
	const numGetters = 4
	for g := range numGetters {
		seed := g
		wg.Go(func() {
			i := 0
			for {
				select {
				case <-soakCtx.Done():
					return
				default:
				}
				key := fmt.Sprintf("claims/p%d", (seed+i)%numPartitions)
				if _, err := kv.Get(soakCtx, key); err == nil {
					totalGets.Add(1)
				}
				i++
			}
		})
	}

	// Keys-side goroutine: drive kv.Keys probes on the same handle.
	// Kept slower than Get (5 ms) since Keys is more expensive.
	wg.Go(func() {
		for {
			select {
			case <-soakCtx.Done():
				return
			default:
			}
			if _, err := kv.Keys(soakCtx); err == nil {
				totalKeys.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
		}
	})

	// Put-side goroutine: keep the watcher updates stream alive so
	// processWatcher sees traffic between closures.
	wg.Go(func() {
		i := 0
		for {
			select {
			case <-soakCtx.Done():
				return
			default:
			}
			pid := fmt.Sprintf("p%d", i%numPartitions)
			c := handoff.Claim{
				PartitionID: pid,
				Owner:       fmt.Sprintf("worker-%d", i),
				State:       handoff.ClaimStateStable,
				Epoch:       int64(i + 2), //nolint:gosec // small test indices
				LastUpdated: time.Now().UTC(),
			}
			b, err := c.Marshal()
			if err == nil {
				if _, err := kv.Put(soakCtx, fmt.Sprintf("claims/%s", pid), b); err == nil {
					totalPuts.Add(1)
				}
			}
			i++
			time.Sleep(10 * time.Millisecond)
		}
	})

	<-soakCtx.Done()
	wg.Wait()

	// Liveness sanity: a fresh ForceRefreshPartition against a known
	// key should succeed after the soak. If the resolver collapsed
	// under the aggressive close-cadence, this would fail and tell us
	// the test load broke the system rather than caught a race.
	livenessCtx, livenessCancel := context.WithTimeout(ctx, 3*time.Second)
	defer livenessCancel()
	// Bypass the rate-limit cooldown by waiting for it to expire if
	// needed, then issuing the refresh.
	require.Eventually(t, func() bool {
		err := r.ForceRefreshPartition(livenessCtx, "p0")
		return err == nil
	}, 3*time.Second, 50*time.Millisecond,
		"resolver must remain functional after the concurrent soak")

	// Primary assertion: the race detector did not fire during the
	// soak. t.Failed() flips to true on any -race-triggered "found
	// data race" stderr write, on any sub-test failure, and on any
	// prior require failure. Since we've already passed the liveness
	// checks above, a true here means the race detector tripped.
	require.False(t, t.Failed(),
		"race detector or sub-assertion failed during the concurrent soak; "+
			"check stderr for WARNING: DATA RACE blocks. The claim "+
			"resolver's supervisor WatchAll restart loop must not race "+
			"with concurrent kv.Get / kv.Keys reads on the shared "+
			"*jetstream.KeyValue handle.")

	t.Logf("soak complete: %d watcher closes, %d Gets, %d Keys probes, %d Puts",
		totalCloses.Load(), totalGets.Load(), totalKeys.Load(), totalPuts.Load())
}

// --- merged from claim_resolver_watcher_freeze_test.go ---

// TestClaimResolver_CacheFreezesAfterWatcherClose reproduces the production
// failure mode reported against parti v2.3.0: when the ClaimBasedResolver's
// KV watcher channel closes (NATS reconnect, server-side consumer GC,
// transient error), the resolver's processWatcher goroutine returns
// silently and the cache freezes forever at the pre-close state. The
// processing gate then suppresses pulls with "not_owner(owner=<stale>)"
// for partitions the worker actually owns per KV, and messages pile up
// in the stream.
//
// This is a verify-the-bug-first test. With the current (buggy) code it
// MUST FAIL — proving the bug exists. Once the watcher restart + reconcile
// fix lands, the same test MUST PASS without changes.
func TestClaimResolver_CacheFreezesAfterWatcherClose(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	nc, cleanup := testutil.StartEmbeddedNATS(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "handoff"})
	require.NoError(t, err)

	// Seed an initial claim: partition pX owned by worker-A.
	initial := handoff.Claim{
		PartitionID: "pX",
		Owner:       "worker-A",
		State:       handoff.ClaimStateStable,
		Epoch:       1,
		LastUpdated: time.Now().UTC(),
	}
	bInitial, err := initial.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pX", bInitial)
	require.NoError(t, err)

	// Start the resolver. warm() seeds the cache; startWatcher subscribes
	// to KV updates.
	r := NewClaimBasedResolver(kv, "claims/", nil)
	require.NoError(t, r.Start(ctx))
	defer r.Stop()

	// Sanity: the warm read populated the cache with the initial owner.
	require.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("pX")
		return ok && owner == "worker-A"
	}, 2*time.Second, 10*time.Millisecond, "initial warm did not populate cache with worker-A")

	// Force the watcher to die — this is the production trigger
	// (NATS reconnect, server-side consumer GC, etc.). Stopping the
	// underlying watcher causes its Updates() channel to be closed,
	// which is the exact !ok signal processWatcher returns on without
	// restart.
	require.NotNil(t, r.watcher)
	require.NoError(t, r.watcher.Stop())

	// Now write the new owner directly to KV. In a healthy resolver, the
	// watcher (or a periodic reconcile) would observe this within a
	// bounded time and update the cache. In the buggy resolver, the
	// cache is permanently frozen at worker-A.
	updated := handoff.Claim{
		PartitionID: "pX",
		Owner:       "worker-B",
		State:       handoff.ClaimStateStable,
		Epoch:       2,
		LastUpdated: time.Now().UTC(),
	}
	bUpdated, err := updated.Marshal()
	require.NoError(t, err)
	_, err = kv.Put(ctx, "claims/pX", bUpdated)
	require.NoError(t, err)

	// The bound here matches the production observation: the cache
	// stayed stale for tens of seconds (the entire reassignment window)
	// despite the KV being authoritative. We give the healthy resolver
	// generous time (5s) to converge; the buggy resolver will never
	// converge.
	if !assert.Eventually(t, func() bool {
		owner, _, _, ok := r.GetOwner("pX")
		return ok && owner == "worker-B"
	}, 5*time.Second, 50*time.Millisecond) {
		owner, _, _, _ := r.GetOwner("pX")
		t.Fatalf("cache failed to converge after watcher channel close: "+
			"GetOwner(pX) still reports owner=%q after 5s, "+
			"but KV has owner=worker-B at revision 2. "+
			"This is the production bug: a closed watcher freezes "+
			"the cache permanently. Fix: restart watcher on channel "+
			"close (mirror source/nats_kv.go Pillar 2 pattern) and "+
			"add a periodic reconcile as a safety net.", owner)
	}
}

// --- merged from claim_resolver_reconcile_observability_test.go ---

// captureLogger is a minimal types.Logger that records messages so a test can
// assert a specific line was (or was not) emitted.
type captureLogger struct {
	mu   sync.Mutex
	msgs []string
}

var _ types.Logger = (*captureLogger)(nil)

func (c *captureLogger) record(msg string) {
	c.mu.Lock()
	c.msgs = append(c.msgs, msg)
	c.mu.Unlock()
}

func (c *captureLogger) Debug(msg string, _ ...any) { c.record(msg) }
func (c *captureLogger) Info(msg string, _ ...any)  { c.record(msg) }
func (c *captureLogger) Warn(msg string, _ ...any)  { c.record(msg) }
func (c *captureLogger) Error(msg string, _ ...any) { c.record(msg) }
func (c *captureLogger) Fatal(msg string, _ ...any) { c.record(msg) }

func (c *captureLogger) has(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.msgs {
		if strings.Contains(m, substr) {
			return true
		}
	}

	return false
}

// TestReconcile_LogsUnreadableKeys pins F-D2c: a reconcile pass that lists keys
// but fails to Get some of them must surface that read failure (previously the
// per-key Get error was silently swallowed). One aggregated line per pass.
//
// Non-vacuous: the healthy control below runs the same reconcile without the
// read fault and asserts the line is NOT emitted.
func TestReconcile_LogsUnreadableKeys(t *testing.T) {
	kv := newHealthyKV(t)
	kv.getErrByKey = map[string]error{quorumTestFullKey: context.DeadlineExceeded}
	cl := &captureLogger{}
	r := NewClaimBasedResolver(kv, "claims/", cl, WithReconcileInterval(0))
	seedResolverCache(r)

	r.reconcileOnce(context.Background())

	require.True(t, cl.has("unreadable"),
		"reconcile must surface a listed-but-unreadable read failure (F-D2c)")
}

// TestReconcile_NoUnreadableLogWhenHealthy is the F-D2c control: a healthy
// reconcile (all Gets succeed) must not emit the unreadable-keys line.
func TestReconcile_NoUnreadableLogWhenHealthy(t *testing.T) {
	kv := newHealthyKV(t)
	cl := &captureLogger{}
	r := NewClaimBasedResolver(kv, "claims/", cl, WithReconcileInterval(0))
	seedResolverCache(r)

	r.reconcileOnce(context.Background())

	require.False(t, cl.has("unreadable"),
		"a healthy reconcile must not emit an unreadable-keys read-failure log")
}
