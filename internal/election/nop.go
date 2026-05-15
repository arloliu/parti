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

// Revision returns 0; the no-op election never acquires leadership.
func (n *NopElection) Revision() uint64 {
	return 0
}

// CheckLeadership returns nil — the no-op election never holds leadership, but
// also never claims to lose it. Tests that need a strict no-leader semantic
// should use a real election or a mock that returns
// types.ErrLeadershipRevisionMismatch.
func (n *NopElection) CheckLeadership(ctx context.Context, claimed uint64) error {
	return nil
}
