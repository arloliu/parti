package partitest

import (
	"context"

	"github.com/arloliu/parti/v2/types"
)

// NopOwnershipResolver is a no-op [types.OwnershipResolver] for tests that
// only need to satisfy a non-nil precondition (e.g., the processing-gate
// wrap path in [internal/durable].WorkerConsumer). GetOwner always reports
// "no known owner" with HandoffStateStable.
type NopOwnershipResolver struct{}

//nolint:revive // function-result-limit: matches the OwnershipResolver interface signature.
func (NopOwnershipResolver) GetOwner(_ string) (string, types.HandoffState, int64, bool) {
	return "", types.HandoffStateStable, 0, false
}

func (NopOwnershipResolver) ForceRefreshPartition(_ context.Context, _ string) error { return nil }

var _ types.OwnershipResolver = NopOwnershipResolver{}
