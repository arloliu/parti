package hooks

import (
	"context"

	"github.com/arloliu/parti/types"
)

// NopHooks implements Hooks with no-op callbacks.
//
// This is the default implementation used when no custom hooks are provided,
// eliminating the need for nil checks throughout the codebase.
type NopHooks struct{}

// Compile-time assertions that NopHooks implements hook callbacks.
var (
	_ func(context.Context, []types.Partition, []types.Partition) error = (*NopHooks)(nil).OnAssignmentChanged
	_ func(context.Context, types.State, types.State) error             = (*NopHooks)(nil).OnStateChanged
	_ func(context.Context, error) error                                = (*NopHooks)(nil).OnError
)

// NewNop creates a new no-op hooks implementation.
//
// Returns:
//   - types.Hooks: Hooks with no-op implementations
func NewNop() types.Hooks {
	h := &NopHooks{}
	return types.Hooks{
		OnAssignmentChanged: h.OnAssignmentChanged,
		OnStateChanged:      h.OnStateChanged,
		OnError:             h.OnError,
	}
}

// OnAssignmentChanged is a no-op implementation.
func (h *NopHooks) OnAssignmentChanged(ctx context.Context, added, removed []types.Partition) error {
	return nil
}

// OnStateChanged is a no-op implementation.
func (h *NopHooks) OnStateChanged(ctx context.Context, from, to types.State) error {
	return nil
}

// OnError is a no-op implementation.
func (h *NopHooks) OnError(ctx context.Context, err error) error {
	return nil
}
