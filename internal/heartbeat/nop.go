package heartbeat

import (
	"context"
)

// NopPublisher implements a no-op heartbeat publisher.
type NopPublisher struct{}

// NewNop creates a new no-op heartbeat publisher.
func NewNop() *NopPublisher {
	return &NopPublisher{}
}

// Start implements the Publisher interface.
func (n *NopPublisher) Start(ctx context.Context) error {
	return nil
}

// Stop implements the Publisher interface.
func (n *NopPublisher) Stop() error {
	return nil
}
