package handoff

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// memStore is a simple in-memory ClaimStore for tests.
type memStore struct {
	mu   sync.Mutex
	data map[string]Claim
	rev  map[string]uint64
}

// compile-time assertion that memStore implements ClaimStore (test-only helper).
var _ ClaimStore = (*memStore)(nil)

func newMemStore() *memStore {
	return &memStore{data: make(map[string]Claim), rev: make(map[string]uint64)}
}

func (m *memStore) Get(ctx context.Context, partitionID string) (Claim, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.data[partitionID]
	if !ok {
		return Claim{}, 0, nil
	}
	return c, m.rev[partitionID], nil
}

func (m *memStore) PutIfEpoch(ctx context.Context, partitionID string, expectedEpoch int64, next Claim) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.data[partitionID]
	if !ok {
		if expectedEpoch != 0 {
			return 0, ErrEpochMismatch
		}
		m.data[partitionID] = next
		m.rev[partitionID] = 1
		return m.rev[partitionID], nil
	}
	if cur.Epoch != expectedEpoch {
		return 0, ErrEpochMismatch
	}
	m.data[partitionID] = next
	m.rev[partitionID]++

	return m.rev[partitionID], nil
}

func (m *memStore) ListKeys(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.data))
	for k := range m.data {
		out = append(out, k)
	}
	return out, nil
}

// ErrEpochMismatch is a sentinel for test store CAS failures.
var ErrEpochMismatch = errors.New("epoch mismatch")

func TestSweeper_ResetsExpiredNonStableToStable(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	now := time.Now().UTC()
	ttl := 1 * time.Second
	expired := now.Add(-2 * ttl)

	// Seed an expired prepare claim
	c := Claim{
		PartitionID:  "p1",
		Owner:        "w2",
		PendingOwner: "w1",
		State:        ClaimStatePrepare,
		Epoch:        2,
		LastUpdated:  expired,
		TTLSeconds:   int64(ttl.Seconds()),
	}
	store.data[c.PartitionID] = c
	store.rev[c.PartitionID] = 1

	cfg := Config{
		Store:         store,
		Now:           func() time.Time { return now },
		SweepInterval: 0, // force sweep on every Apply
		MaxRetries:    1,
		BaseBackoff:   1 * time.Millisecond,
		MaxBackoff:    2 * time.Millisecond,
	}
	coord := New(cfg, true)

	// Trigger Apply with no-op updater (nil updater short-circuits Apply path but sweeper is opportunistic)
	_ = coord.Apply(context.Background(), "worker-1", types.Assignment{}, types.Assignment{})

	got, _, err := store.Get(context.Background(), "p1")
	require.NoError(t, err)
	require.Equal(t, ClaimStateStable, got.State)
	require.Empty(t, got.PendingOwner)
	require.Equal(t, c.Owner, got.Owner)
	require.True(t, got.LastUpdated.Equal(now))
}

func TestSweeper_IgnoresStableOrFresh(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	now := time.Now().UTC()
	ttl := 5 * time.Second

	// Fresh stable claim
	fresh := Claim{
		PartitionID: "p2",
		Owner:       "w1",
		State:       ClaimStateStable,
		Epoch:       1,
		LastUpdated: now,
		TTLSeconds:  int64(ttl.Seconds()),
	}
	store.data[fresh.PartitionID] = fresh
	store.rev[fresh.PartitionID] = 1

	// Non-expired prepare claim (should remain unchanged)
	prep := Claim{
		PartitionID:  "p3",
		Owner:        "w2",
		PendingOwner: "w1",
		State:        ClaimStatePrepare,
		Epoch:        3,
		LastUpdated:  now.Add(-1 * time.Second),
		TTLSeconds:   int64(ttl.Seconds()),
	}
	store.data[prep.PartitionID] = prep
	store.rev[prep.PartitionID] = 1

	cfg := Config{
		Store:         store,
		Now:           func() time.Time { return now },
		SweepInterval: 0,
		MaxRetries:    1,
		BaseBackoff:   1 * time.Millisecond,
		MaxBackoff:    2 * time.Millisecond,
	}
	coord := New(cfg, true)

	_ = coord.Apply(context.Background(), "worker-1", types.Assignment{}, types.Assignment{})

	gotFresh, _, _ := store.Get(context.Background(), "p2")
	require.Equal(t, fresh, gotFresh)

	gotPrep, _, _ := store.Get(context.Background(), "p3")
	require.Equal(t, prep, gotPrep)
}
