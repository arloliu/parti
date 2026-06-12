package parti

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/v2/internal/assignment/handoff"
	"github.com/arloliu/parti/v2/internal/logging"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type mockClaimStore struct {
	mock.Mock
}

func (m *mockClaimStore) ListKeys(ctx context.Context) ([]string, error) {
	args := m.Called(ctx)
	val, _ := args.Get(0).([]string)
	return val, args.Error(1)
}

func (m *mockClaimStore) Get(ctx context.Context, key string) (handoff.Claim, uint64, error) {
	args := m.Called(ctx, key)
	claim, _ := args.Get(0).(handoff.Claim)
	ver, _ := args.Get(1).(uint64)
	return claim, ver, args.Error(2)
}

func (m *mockClaimStore) PutIfEpoch(ctx context.Context, key string, epoch int64, claim handoff.Claim) (uint64, error) {
	args := m.Called(ctx, key, epoch, claim)
	ver, _ := args.Get(0).(uint64)
	return ver, args.Error(1)
}

func (m *mockClaimStore) Delete(ctx context.Context, key string, revision uint64) error {
	args := m.Called(ctx, key, revision)
	return args.Error(0)
}

func TestManager_handoffStartupHygiene(t *testing.T) {
	logger := logging.NewNop()

	t.Run("nil store", func(t *testing.T) {
		m := &Manager{logger: logger}
		resumable := m.handoffStartupHygiene(context.Background(), nil)
		require.False(t, resumable)
	})

	t.Run("empty store", func(t *testing.T) {
		m := &Manager{logger: logger}
		store := new(mockClaimStore)
		store.On("ListKeys", mock.Anything).Return([]string{}, nil)

		resumable := m.handoffStartupHygiene(context.Background(), store)
		require.False(t, resumable)
		store.AssertExpectations(t)
	})

	t.Run("resumable claim", func(t *testing.T) {
		m := &Manager{logger: logger}
		store := new(mockClaimStore)

		now := time.Now().UTC()
		claim := handoff.Claim{
			State:       handoff.ClaimStatePrepare,
			LastUpdated: now, // Not expired
			TTLSeconds:  10,
		}

		store.On("ListKeys", mock.Anything).Return([]string{"p1"}, nil)
		store.On("Get", mock.Anything, "p1").Return(claim, uint64(1), nil)

		resumable := m.handoffStartupHygiene(context.Background(), store)
		require.True(t, resumable)
		store.AssertExpectations(t)
	})

	t.Run("expired claim reset", func(t *testing.T) {
		m := &Manager{logger: logger}
		store := new(mockClaimStore)

		now := time.Now().UTC()
		claim := handoff.Claim{
			State:       handoff.ClaimStatePrepare,
			LastUpdated: now.Add(-20 * time.Second), // Expired (assuming TTL 10)
			TTLSeconds:  10,
			Epoch:       5,
		}

		store.On("ListKeys", mock.Anything).Return([]string{"p1"}, nil)
		store.On("Get", mock.Anything, "p1").Return(claim, uint64(1), nil)
		store.On("PutIfEpoch", mock.Anything, "p1", int64(5), mock.MatchedBy(func(c handoff.Claim) bool {
			return c.State == handoff.ClaimStateStable && c.PendingOwner == ""
		})).Return(uint64(2), nil)

		resumable := m.handoffStartupHygiene(context.Background(), store)
		require.False(t, resumable)
		store.AssertExpectations(t)
	})
}
