package hooks

import (
	"context"
	"testing"

	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

func TestNewNop(t *testing.T) {
	hooks := NewNop()

	require.NotNil(t, hooks.OnAssignmentChanged)
	require.NotNil(t, hooks.OnStateChanged)
	require.NotNil(t, hooks.OnError)
}

func TestNopHooks_OnAssignmentChanged(t *testing.T) {
	hooks := NewNop()
	ctx := context.Background()

	added := []types.Partition{
		{Keys: []string{"partition-1"}, Weight: 100},
		{Keys: []string{"partition-2"}, Weight: 100},
	}
	removed := []types.Partition{
		{Keys: []string{"partition-3"}, Weight: 100},
	}

	err := hooks.OnAssignmentChanged(ctx, added, removed)
	require.NoError(t, err)
}

func TestNopHooks_OnStateChanged(t *testing.T) {
	hooks := NewNop()
	ctx := context.Background()

	err := hooks.OnStateChanged(ctx, types.StateInit, types.StateStable)
	require.NoError(t, err)
}

func TestNopHooks_OnError(t *testing.T) {
	hooks := NewNop()
	ctx := context.Background()

	testErr := context.Canceled
	err := hooks.OnError(ctx, testErr)
	require.NoError(t, err)
}
