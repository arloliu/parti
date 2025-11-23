package parti

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/parti/types"
	"github.com/stretchr/testify/require"
)

func TestManager_prepareStart(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		m := &Manager{
			cfg: Config{StartupTimeout: 5 * time.Second},
		}
		ctx, cancel, err := m.prepareStart(context.Background())
		require.NoError(t, err)
		require.NotNil(t, ctx)
		require.NotNil(t, cancel)
		defer cancel()

		// Verify context has timeout
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.WithinDuration(t, time.Now().Add(5*time.Second), deadline, 100*time.Millisecond)
	})

	t.Run("already started", func(t *testing.T) {
		m := &Manager{
			ctx: context.Background(), // Simulate started
		}
		ctx, cancel, err := m.prepareStart(context.Background())
		require.ErrorIs(t, err, types.ErrAlreadyStarted)
		require.Nil(t, ctx)
		require.NotNil(t, cancel) // Should return no-op cancel
		cancel()
	})

	t.Run("no timeout", func(t *testing.T) {
		m := &Manager{
			cfg: Config{StartupTimeout: 0},
		}
		ctx, cancel, err := m.prepareStart(context.Background())
		require.NoError(t, err)
		defer cancel()

		// Verify context has NO deadline (if parent doesn't)
		_, ok := ctx.Deadline()
		require.False(t, ok)
	})
}
