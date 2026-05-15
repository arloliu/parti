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

// SetAppliedAssignment discards the applied-assignment ack. Mirrors
// Publisher.SetAppliedAssignment but does no I/O.
func (n *NopPublisher) SetAppliedAssignment(_ AppliedAssignment) {
	// No-op
}

// PublishNow discards the immediate-publish request. Returns nil so the
// manager's apply pipeline succeeds in test contexts that wire a nop
// heartbeat publisher.
func (n *NopPublisher) PublishNow(_ context.Context) error {
	return nil
}
