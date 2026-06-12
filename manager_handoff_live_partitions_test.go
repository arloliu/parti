package parti

import (
	"context"
	"errors"
	"testing"

	"github.com/arloliu/parti/v2/source"
	"github.com/arloliu/parti/v2/types"
	"github.com/stretchr/testify/require"
)

// errListSource is a PartitionSource whose List always fails.
type errListSource struct{ err error }

func (e *errListSource) Start(_ context.Context) error                     { return nil }
func (e *errListSource) List(_ context.Context) ([]types.Partition, error) { return nil, e.err }
func (e *errListSource) Stop(_ context.Context) error                      { return nil }

// TestLivePartitionSet pins the orphan-reap supplier's vouching contract:
// only a leader with a healthy source AND a readable commit answers ok=true
// (the reap pass deletes claims based on this set, so an unverifiable view
// must never authorize deletion), and the vouched set is the union of the
// source view and the latest committed assignment's partitions.
func TestLivePartitionSet(t *testing.T) {
	t.Parallel()

	// newManager wires the commit-read seam to "no commit exists" by
	// default; subtests override the hooks to exercise the commit arm.
	newManager := func(src types.PartitionSource) *Manager {
		return &Manager{
			source: src,
			testHookCommitRead: func(_ context.Context) (*types.AssignmentCommit, uint64, error) {
				return nil, 0, nil
			},
		}
	}

	t.Run("non-leader never vouches", func(t *testing.T) {
		t.Parallel()
		m := newManager(source.NewStatic([]types.Partition{{Keys: []string{"p-0"}}}))
		// isLeader defaults to false (zero value)

		set, ok := m.livePartitionSet(t.Context())
		require.False(t, ok, "a follower's source view must never authorize reaping")
		require.Nil(t, set)
	})

	t.Run("leader vouches with SubjectKey-keyed set", func(t *testing.T) {
		t.Parallel()
		m := newManager(source.NewStatic([]types.Partition{
			{Keys: []string{"p-0"}},
			{Keys: []string{"region", "p-1"}}, // multi-key: claim key is dot-joined
		}))
		m.isLeader.Store(true)

		set, ok := m.livePartitionSet(t.Context())
		require.True(t, ok)
		require.Len(t, set, 2)
		require.Contains(t, set, "p-0")
		require.Contains(t, set, "region.p-1",
			"set must be keyed by SubjectKey — the identity claims are stored under")
	})

	t.Run("source error never vouches", func(t *testing.T) {
		t.Parallel()
		m := newManager(&errListSource{err: errors.New("boom")})
		m.isLeader.Store(true)

		set, ok := m.livePartitionSet(t.Context())
		require.False(t, ok, "an unreadable source must never authorize reaping")
		require.Nil(t, set)
	})

	t.Run("committed partitions stay live even when the source dropped them", func(t *testing.T) {
		t.Parallel()
		// Source no longer lists p-stalled, but the live commit still
		// references it (the stalled-rebalance window): its owner is still
		// consuming it through the gate, so it must not be reap-eligible.
		m := newManager(source.NewStatic([]types.Partition{{Keys: []string{"p-0"}}}))
		m.isLeader.Store(true)
		m.testHookCommitRead = func(_ context.Context) (*types.AssignmentCommit, uint64, error) {
			return &types.AssignmentCommit{Version: 7}, 42, nil
		}
		m.testHookCommitBatch = func(_ context.Context, c *types.AssignmentCommit) (map[string]struct{}, error) {
			require.EqualValues(t, 7, c.Version)
			return map[string]struct{}{"p-stalled": {}}, nil
		}

		set, ok := m.livePartitionSet(t.Context())
		require.True(t, ok)
		require.Contains(t, set, "p-0")
		require.Contains(t, set, "p-stalled",
			"a partition the live commit references is not an orphan, source view notwithstanding")
	})

	t.Run("commit read error never vouches", func(t *testing.T) {
		t.Parallel()
		m := newManager(source.NewStatic([]types.Partition{{Keys: []string{"p-0"}}}))
		m.isLeader.Store(true)
		m.testHookCommitRead = func(_ context.Context) (*types.AssignmentCommit, uint64, error) {
			return nil, 0, errors.New("kv down")
		}

		set, ok := m.livePartitionSet(t.Context())
		require.False(t, ok, "an unverifiable commit view must never authorize reaping")
		require.Nil(t, set)
	})
}
