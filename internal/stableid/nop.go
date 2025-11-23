package stableid

import (
	"context"
)

// NopClaimer implements a no-op stable ID claimer.
type NopClaimer struct{}

// NewNop creates a new no-op stable ID claimer.
func NewNop() *NopClaimer {
	return &NopClaimer{}
}

// Claim implements the Claimer interface.
func (n *NopClaimer) Claim(ctx context.Context) (string, error) {
	return "", nil
}

// StartRenewal implements the Claimer interface.
func (n *NopClaimer) StartRenewal() error {
	return nil
}

// Release implements the Claimer interface.
func (n *NopClaimer) Release(ctx context.Context) error {
	return nil
}

// Close implements the Claimer interface.
func (n *NopClaimer) Close() {
}

// WorkerID implements the Claimer interface.
func (n *NopClaimer) WorkerID() string {
	return ""
}
