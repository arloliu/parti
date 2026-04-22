package durable

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

type batchMetric struct {
	size int
	dur  time.Duration
}

// metricsSpy implements ResolverMetrics for unit tests.
type metricsSpy struct {
	visLagCount  int
	lastVisLag   time.Duration
	cacheSizes   []int
	updates      map[string]int
	batches      []batchMetric
	flushReasons map[string]int
}

func newMetricsSpy() *metricsSpy {
	return &metricsSpy{updates: make(map[string]int), flushReasons: make(map[string]int)}
}

func (m *metricsSpy) ObserveVisibilityLag(d time.Duration) { m.visLagCount++; m.lastVisLag = d }
func (m *metricsSpy) SetCacheSize(n int)                   { m.cacheSizes = append(m.cacheSizes, n) }
func (m *metricsSpy) IncUpdate(op string)                  { m.updates[op]++ }
func (m *metricsSpy) ObserveBatch(size int, dur time.Duration) {
	m.batches = append(m.batches, batchMetric{size, dur})
}
func (m *metricsSpy) IncBatchFlush(reason string) { m.flushReasons[reason]++ }

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
		wg.Add(2) //nolint:revive // sync.WaitGroup does not have Go method

		go func() {
			defer wg.Done()
			_ = r.ForceRefreshPartition(context.Background(), "p1")
		}()

		go func() {
			defer wg.Done()
			r.applyPendingBatch(batch, "test")
		}()

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
