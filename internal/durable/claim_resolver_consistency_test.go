package durable

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

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
