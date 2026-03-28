package election

import (
	"context"

	"github.com/arloliu/parti/v2/types"
)

// NopElection implements a no-op election agent.
type NopElection struct{}

// Compile-time assertion that NopElection implements ElectionAgent.
var _ types.ElectionAgent = (*NopElection)(nil)

// NewNopElection creates a new no-op election agent.
func NewNopElection() *NopElection {
	return &NopElection{}
}

// RequestLeadership implements the ElectionAgent interface.
func (n *NopElection) RequestLeadership(ctx context.Context, workerID string, leaseDuration int64) (bool, error) {
	return false, nil
}

// RenewLeadership implements the ElectionAgent interface.
func (n *NopElection) RenewLeadership(ctx context.Context) error {
	return nil
}

// ReleaseLeadership implements the ElectionAgent interface.
func (n *NopElection) ReleaseLeadership(ctx context.Context) error {
	return nil
}

// IsLeader implements the ElectionAgent interface.
func (n *NopElection) IsLeader(ctx context.Context) (bool, error) {
	return false, nil
}
